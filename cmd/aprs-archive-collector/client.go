package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"aprs-archive/internal/aprs"
)

// APRS-IS connection behaviour.
//
// The stream is read-only: this collector never transmits, so it never opens a
// second connection to acknowledge or reply, and a hostile or misconfigured
// sender cannot turn inbound traffic into outbound dials.
const (
	// handshakeTimeout bounds the TCP connect and the login response.
	handshakeTimeout = 30 * time.Second
	// readDeadline is how long silence is tolerated. APRS-IS sends a keepalive
	// comment every 20 seconds; two minutes of nothing means the link is dead
	// even if the TCP socket has not noticed.
	readDeadline = 2 * time.Minute
	// loginGrace is how long after login to wait for the server's `# logresp`.
	loginGrace = 30 * time.Second
	// minBackoff and maxBackoff bound the reconnect delay.
	minBackoff = 5 * time.Second
	maxBackoff = 5 * time.Minute
	// maxLoginRetries is how many consecutive rejected logins are tolerated
	// before giving up for a long sleep. A bad passcode will not fix itself on
	// the next attempt, so hammering the server is wrong.
	maxLoginRetries = 5
)

// client is one APRS-IS subscription.
type client struct {
	server   string
	port     int
	login    string
	passcode int
	filter   string

	// stats are read by the status endpoint and the log line.
	lines       atomic.Int64
	packets     atomic.Int64
	connects    atomic.Int64
	lastError   atomic.Value // string
	connected   atomic.Bool
	connectedAt atomic.Int64
}

// loginLine builds the APRS-IS login.
//
// `vers` names the software; APRS-IS uses it to identify buggy clients. The
// filter is sent in the login so the server sends messages only — positions,
// weather and telemetry never cross the wire, which is what keeps this archive
// to the traffic it promises to store.
func (c *client) loginLine() string {
	return fmt.Sprintf("user %s pass %d vers aprs-archive 1.0 filter %s\r\n",
		c.login, c.passcode, c.filter)
}

// run keeps a subscription alive until ctx is cancelled.
//
// A disconnect is normal — the APRS-IS hub rotates servers, networks flap, the
// server enforces a login duration — so the loop reconnects with exponential
// backoff and resets the backoff only after a session that actually delivered
// data. Resetting on connect instead would turn a server that accepts and
// immediately drops into a hot reconnect loop.
func (c *client) run(ctx context.Context, handle func(aprs.Packet, time.Time)) error {
	backoff := minBackoff
	failures := 0
	for {
		if ctx.Err() != nil {
			return nil
		}
		start := time.Now()
		delivered, err := c.session(ctx, handle)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			c.lastError.Store(err.Error())
			log.Printf("aprs-is: session ended: %v", err)
			if isAuthError(err) {
				failures++
				if failures >= maxLoginRetries {
					log.Printf("aprs-is: %d consecutive login failures; the callsign or passcode is probably wrong", failures)
					backoff = maxBackoff
				}
			}
		} else {
			log.Printf("aprs-is: session closed by server after %s", time.Since(start).Round(time.Second))
		}
		if delivered > 0 {
			backoff = minBackoff
			failures = 0
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		if backoff < maxBackoff {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}

func isAuthError(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "logresp") && strings.Contains(msg, "unverified")
}

// session runs one TCP connection and returns how many packets it delivered.
func (c *client) session(ctx context.Context, handle func(aprs.Packet, time.Time)) (int64, error) {
	addr := net.JoinHostPort(c.server, strconv.Itoa(c.port))
	dialer := net.Dialer{Timeout: handshakeTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return 0, fmt.Errorf("dial %s: %w", addr, err)
	}
	defer conn.Close()
	defer c.connected.Store(false)

	log.Printf("aprs-is: connected to %s as %s", addr, c.login)
	if _, err := conn.Write([]byte(c.loginLine())); err != nil {
		return 0, fmt.Errorf("send login: %w", err)
	}
	// The login is the only write this process ever makes, and it is enforced
	// by there being no other write in this file. The socket is NOT half-closed
	// after login: APRS-IS treats a write-shutdown from the client as a dead
	// connection and closes the session within a second (observed against
	// rotate.aprs2.net as EA7KLK-15). Receive-only is a property of the code,
	// not of the socket's write side.

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 4096), 64*1024)

	loginDeadline := time.Now().Add(loginGrace)
	verified := false
	var delivered int64
	for {
		if ctx.Err() != nil {
			return delivered, nil
		}
		_ = conn.SetReadDeadline(readDeadlineDeadline(loginDeadline, verified))
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return delivered, err
			}
			return delivered, nil
		}
		line := strings.TrimRight(scanner.Text(), "\r\n")
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			// Server comment: keepalive, a login response, or a filter notice.
			if resp, ok := strings.CutPrefix(line, "# logresp "); ok {
				if strings.Contains(resp, "unverified") {
					return delivered, fmt.Errorf("login rejected: # logresp %s", strings.TrimSpace(resp))
				}
				verified = true
				c.connected.Store(true)
				c.connectedAt.Store(time.Now().Unix())
				c.connects.Add(1)
				log.Printf("aprs-is: login verified (%s)", strings.TrimSpace(resp))
			}
			continue
		}
		c.lines.Add(1)
		packet, ok := aprs.ParseRaw(line)
		if !ok {
			continue
		}
		c.packets.Add(1)
		delivered++
		handle(packet, time.Now().UTC())
	}
}

// readDeadlineDeadline returns the deadline for the next read: the login grace
// while the server has not answered yet, then the keepalive window.
// Past the grace period without a login response the normal window applies, so
// a quiet but working stream is not dropped for the server's silence alone.
func readDeadlineDeadline(loginDeadline time.Time, verified bool) time.Time {
	if verified || time.Now().After(loginDeadline) {
		return time.Now().Add(readDeadline)
	}
	return loginDeadline
}

// lastErrorString returns the last connection error, for the status endpoint.
func (c *client) lastErrorString() string {
	if v, ok := c.lastError.Load().(string); ok {
		return v
	}
	return ""
}
