// Command aprs-archive-collector keeps the APRS message archive current.
//
// It is the only writer: it subscribes to APRS-IS with a message-only filter,
// parses message-format packets, and commits them in batches. It never
// transmits — authentication is receive-only and the socket's write side is
// closed after login — because an archive has no business putting traffic on the
// air in anyone's name.
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"aprs-archive/internal/aprs"
	"aprs-archive/internal/store"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

type config struct {
	dbPath           string
	server           string
	port             int
	callsign         string
	passcode         int
	filter           string
	sweepInterval    time.Duration
	flushInterval    time.Duration
	messageRetention time.Duration
	bulletRetention  time.Duration
	healthAddr       string
}

func main() {
	if err := run(); err != nil {
		log.Printf("fatal: %v", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	log.SetFlags(log.LstdFlags | log.LUTC)
	log.Printf("aprs-archive-collector %s: db=%s aprs-is=%s:%d login=%s filter=%q",
		version, cfg.dbPath, cfg.server, cfg.port, cfg.callsign, cfg.filter)

	db, err := store.OpenWriter(cfg.dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Collapse numbered parts that were archived as separate rows. The live
	// assembler only sees packets arriving now, so anything already on disk —
	// including a set split by a collector restart — is repaired here.
	if count, err := store.AssembleStoredParts(ctx, db, time.Now().UTC()); err != nil {
		log.Printf("assemble stored parts: %v", err)
	} else if count > 0 {
		log.Printf("assembled %d multi-part messages from previously stored rows", count)
	}

	// One-off repair of rows an earlier build stored wrongly: acknowledgements
	// and rejections that the recogniser did not catch and that were archived
	// as ordinary messages. Idempotent — on a repaired archive it deletes
	// nothing and logs nothing.
	if removed, err := store.PurgeMisclassifiedResponses(ctx, db); err != nil {
		return fmt.Errorf("purge misclassified acknowledgements: %w", err)
	} else if removed > 0 {
		log.Printf("removed %d acknowledgements that an earlier build stored as messages", removed)
	}

	writer, err := store.NewWriter(ctx, db)
	if err != nil {
		return fmt.Errorf("prepare writer: %w", err)
	}

	client := &client{
		server:   cfg.server,
		port:     cfg.port,
		login:    cfg.callsign,
		passcode: cfg.passcode,
		filter:   cfg.filter,
	}

	// The flush loop is separate from the read loop on purpose: a slow disk or
	// a busy database must apply backpressure through the batch size, not stall
	// the socket long enough for the server to time us out.
	// The assembler holds the parts of multi-part messages until the set is
	// complete. Each part is also stored immediately, so a set whose last part
	// never arrives still leaves its content in the archive.
	assembler := aprs.NewAssembler(nil)

	var pending atomic.Bool
	handle := func(p aprs.Packet, at time.Time) {
		// Acks and rejects are message-format packets, so they arrive under the
		// message filter, but they are delivery receipts rather than traffic:
		// the archive keeps what was said, not the bookkeeping around it. They
		// are classified by the parser and dropped here, which keeps the parser
		// an honest description of the protocol.
		if p.Kind == aprs.KindAck || p.Kind == aprs.KindRej {
			return
		}
		partID, err := writer.RecordPacket(ctx, p, at)
		if err != nil {
			log.Printf("store packet from=%s to=%s: %v", p.From, p.To, err)
			return
		}
		pending.Store(true)

		assembled, replacedIDs, complete := assembler.Add(p, partID)
		if !complete {
			return
		}
		if partID > 0 {
			replacedIDs = append(replacedIDs, partID)
		}
		if err := writer.ReplaceParts(ctx, replacedIDs, assembled, at); err != nil {
			// The parts stay stored, so nothing is lost; the message shows as
			// its parts until the next attempt.
			log.Printf("assemble multi-part from=%s to=%s: %v", p.From, p.To, err)
			return
		}
		log.Printf("assembled a %d-part message from=%s to=%s", len(replacedIDs), p.From, p.To)
	}

	healthSrv := startHealth(cfg.healthAddr, client, db)
	defer shutdown(healthSrv)

	go flushLoop(ctx, writer, &pending, cfg.flushInterval)
	go sweepLoop(ctx, db, cfg, &pending)

	for {
		if err := client.run(ctx, handle); err != nil {
			return err
		}
		if ctx.Err() != nil {
			break
		}
	}
	log.Printf("shutting down")
	if err := writer.Close(ctx); err != nil {
		log.Printf("final flush: %v", err)
	}
	return nil
}

// flushLoop commits whatever the handler buffered.
//
// It also commits on a bare timer whether or not anything is pending, because a
// quiet feed still needs collector_state.last_write_at to move so the web page
// can distinguish "nothing is being said" from "the collector is stuck".
func flushLoop(ctx context.Context, w *store.Writer, pending *atomic.Bool, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = w.Flush(context.Background())
			return
		case <-ticker.C:
			if err := w.Flush(ctx); err != nil {
				log.Printf("flush: %v", err)
			}
			pending.Store(false)
		}
	}
}

// sweepLoop applies retention. It runs once at startup — after an outage the
// first thing an operator wants is the database back to its steady state — and
// then on the configured interval.
func sweepLoop(ctx context.Context, db *sql.DB, cfg config, _ *atomic.Bool) {
	retention := store.Retention{Messages: cfg.messageRetention, Bulletins: cfg.bulletRetention}
	sweep := func() {
		start := time.Now()
		messages, bulletins, err := store.Sweep(ctx, db, retention, time.Now().UTC())
		if err != nil {
			log.Printf("retention sweep: %v", err)
			return
		}
		if messages > 0 || bulletins > 0 {
			log.Printf("retention sweep removed %d messages and %d bulletins in %s", messages, bulletins, time.Since(start).Round(time.Second))
		}
		if err := store.Optimize(ctx, db); err != nil {
			log.Printf("retention optimize: %v", err)
		}
	}
	select {
	case <-time.After(2 * time.Minute):
		sweep()
	case <-ctx.Done():
		return
	}
	ticker := time.NewTicker(cfg.sweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweep()
		}
	}
}

func loadConfig() (config, error) {
	cfg := config{}
	flag.StringVar(&cfg.dbPath, "db", env("APRS_ARCHIVE_DB", "/var/lib/aprs-archive/archive.db"), "path to the SQLite archive file")
	flag.StringVar(&cfg.server, "server", env("APRS_IS_SERVER", "rotate.aprs2.net"), "APRS-IS server")
	port, _ := strconv.Atoi(env("APRS_IS_PORT", "14580"))
	cfg.port = port
	flag.StringVar(&cfg.callsign, "callsign", env("APRS_IS_CALLSIGN", "EA7KLK-15"), "APRS-IS login callsign (SSID included)")
	passcode, err := strconv.Atoi(env("APRS_IS_PASSCODE", "-1"))
	if err != nil {
		return cfg, fmt.Errorf("APRS_IS_PASSCODE must be a number: %w", err)
	}
	cfg.passcode = passcode
	flag.StringVar(&cfg.filter, "filter", env("APRS_IS_FILTER", "t/m"), "APRS-IS filter; message traffic only by default")
	sweepHours, _ := strconv.Atoi(env("APRS_ARCHIVE_SWEEP_HOURS", "24"))
	cfg.sweepInterval = time.Duration(sweepHours) * time.Hour
	flushSecs, _ := strconv.Atoi(env("APRS_ARCHIVE_FLUSH_SECONDS", "2"))
	cfg.flushInterval = time.Duration(flushSecs) * time.Second
	retentionDays, _ := strconv.Atoi(env("APRS_ARCHIVE_RETENTION_DAYS", "365"))
	cfg.messageRetention = time.Duration(retentionDays) * 24 * time.Hour
	cfg.bulletRetention = cfg.messageRetention
	flag.StringVar(&cfg.healthAddr, "health-addr", env("APRS_ARCHIVE_HEALTH_ADDR", ":8081"), "address for the liveness endpoint")
	flag.Parse()

	if cfg.flushInterval <= 0 {
		cfg.flushInterval = store.DefaultFlushInterval
	}
	if cfg.sweepInterval < time.Hour {
		cfg.sweepInterval = time.Hour
	}
	if cfg.callsign == "" {
		return cfg, errors.New("an APRS-IS login callsign is required")
	}
	if cfg.passcode < 0 {
		// Derive from the base callsign, which is what APRS-IS expects. An
		// explicit value wins so an operator can override a callsign the
		// archive does not know the passcode for.
		cfg.passcode = aprs.Passcode(cfg.callsign)
		log.Printf("derived APRS-IS passcode %d for %s", cfg.passcode, aprs.BaseCallsign(cfg.callsign))
	}
	return cfg, nil
}

func env(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

// startHealth serves /livez, /readyz and /status for Kubernetes probes and for
// an operator who wants to know what the collector is doing without reading logs.
func startHealth(addr string, c *client, db *sql.DB) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/livez", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		// Ready means the database answers. Being disconnected from APRS-IS is
		// not unreadiness: the collector will reconnect, and restarting it
		// would not make the network come back.
		if err := db.PingContext(r.Context()); err != nil {
			http.Error(w, "database unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		connected := c.connected.Load()
		fmt.Fprintf(w, "version=%s\nconnected=%t\nconnects=%d\nlines=%d\npackets=%d\nlast_error=%q\n",
			version, connected, c.connects.Load(), c.lines.Load(), c.packets.Load(), c.lastErrorString())
	})
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("health server: %v", err)
		}
	}()
	return srv
}

func shutdown(srv *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}
