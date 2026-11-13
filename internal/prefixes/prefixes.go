// Package prefixes validates amateur callsigns against the world-wide prefix
// list published by country-files.com (the cty.dat / cty.csv dataset used by
// every logging program and contest calendar).
//
// Why validation at all: APRS-IS is an open network carrying a large amount of
// machine traffic — weather gateways, message relays, registration bots, net
// announcers — that addresses itself with names rather than callsigns
// (`WXBOT`, `ANSRVR`, `MYANET`, `OTA`). Those are not stations and their messages
// are not operator traffic. A real amateur callsign starts with a prefix
// assigned to a country and contains a digit; a bot's name does neither.
//
// Why a downloadable list rather than an embedded rule: prefixes change. New
// entities appear, callsign series are reallocated, and a hard-coded table goes
// stale silently. The dataset is fetched, parsed and cached, and the collector
// refreshes it on a timer.
package prefixes

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// DefaultSource is the published CSV form of the cty prefix list.
//
// The CSV is preferred over cty.dat because each entity is one line with the
// prefix list as its last field; cty.dat wraps that list across continuation
// lines, which is one more way to be wrong. Both are regenerated together by
// the same maintainer, so choosing one does not risk staleness.
const DefaultSource = "https://www.country-files.com/cty/cty.csv"

// Set is an immutable snapshot of the valid prefixes.
type Set struct {
	// prefixes are upper case, longest first, so the longest match wins.
	prefixes []string
	// maxLen bounds the prefix scan.
	maxLen int
	// FetchedAt is when this snapshot was read from the source.
	FetchedAt time.Time
	// Source records where it came from, for the admin page.
	Source string
	// Count is how many prefixes the snapshot holds.
	Count int
}

// Contains reports whether callsign begins with a known prefix.
//
// It does not decide validity on its own: a callsign is valid when a prefix
// matches AND it contains a digit. See Valid.
func (s *Set) Contains(callsign string) bool {
	if s == nil {
		return false
	}
	c := strings.ToUpper(strings.TrimSpace(callsign))
	for _, p := range s.prefixes {
		if strings.HasPrefix(c, p) {
			return true
		}
	}
	return false
}

// Valid reports whether callsign looks like a real amateur callsign.
//
// The rule is deliberately permissive, because the cost of the two mistakes is
// not symmetric. Rejecting a real station silently deletes a person's message
// from the archive. Accepting a bot leaves one junk row, which is visible and
// fixable. So a callsign is accepted when:
//
//   - its base (before any ssid or operating suffix) is 3 to 12 characters;
//   - it contains at least one digit — every real callsign does, and almost no
//     software name does;
//   - and it starts with a prefix from the dataset.
//
// That accepts `EA7KLK-9`, `IZ6RND`, `KH6ABC`, `EA7KLK/P` and `EA7KLK-15`, and
// rejects `MYANET`, `OTA`, `WXBOT`, `ANSRVR`, `EMAIL-2` and `MTG-RRRA`. It also
// accepts a hypothetical bot called `N0BOT`, which is why the operator's
// exception list exists: no prefix rule can tell a cleverly named robot from a
// station.
func (s *Set) Valid(callsign string) bool {
	if s == nil {
		return false
	}
	base := baseCallsign(callsign)
	if len(base) < 3 || len(base) > 12 {
		return false
	}
	if !hasDigit(base) {
		return false
	}
	return s.Contains(base)
}

// baseCallsign strips an SSID and any non-alphanumeric operating suffix.
//
// `EA7KLK-15` and `EA7KLK` are one station; `EA7KLK/P` and `EA7KLK/QRP` are the
// same station operating portable or low power. The archive judges the station,
// not the operating style.
func baseCallsign(callsign string) string {
	c := strings.ToUpper(strings.TrimSpace(callsign))
	if c == "" {
		return ""
	}
	c = strings.SplitN(c, "/", 2)[0]
	if i := strings.LastIndexByte(c, '-'); i > 0 {
		// A trailing -N is an SSID, but only if N is numeric: `MTG-RRRA` must
		// keep its whole name so that it is judged as the word it is.
		if isNumeric(c[i+1:]) {
			c = c[:i]
		}
	}
	var b strings.Builder
	for _, r := range c {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func hasDigit(s string) bool {
	for _, r := range s {
		if r >= '0' && r <= '9' {
			return true
		}
	}
	return false
}

// bracketRE removes the optional `[n]` annotations cty.csv puts after some
// prefixes, such as `B2A[33]`.
var bracketRE = regexp.MustCompile(`\[[^\]]*\]`)

// ParseCSV builds a Set from the cty.csv body.
//
// Each line is:
//
//	1A,Sov Mil Order of Malta,246,EU,15,28,41.90,-12.43,-1.0,1A;
//
// The last comma-separated field is the prefix list, space separated and ended
// by a semicolon. A line that does not have that shape is skipped rather than
// failing the whole file: a partially understood update is better than no
// update, and the count is reported so a sudden drop is visible.
func ParseCSV(data []byte) (*Set, error) {
	seen := map[string]struct{}{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	lines, skipped := 0, 0
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		lines++
		idx := strings.LastIndexByte(line, ',')
		if idx < 0 || idx == len(line)-1 {
			skipped++
			continue
		}
		fields := strings.Fields(strings.TrimRight(line[idx+1:], ";"))
		if len(fields) == 0 {
			skipped++
			continue
		}
		for _, f := range fields {
			f = strings.ToUpper(bracketRE.ReplaceAllString(f, ""))
			f = strings.TrimSpace(f)
			// `=VERSION` and friends are directives, not prefixes.
			if f == "" || strings.HasPrefix(f, "=") {
				continue
			}
			seen[f] = struct{}{}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read prefix list: %w", err)
	}
	if len(seen) < 100 {
		// A real list has thousands. Fewer means the source changed shape or
		// returned an error page, and accepting it would reject every real
		// callsign — the worst failure this package can have.
		return nil, fmt.Errorf("prefix list looks wrong: %d prefixes from %d lines (%d skipped)", len(seen), lines, skipped)
	}
	list := make([]string, 0, len(seen))
	maxLen := 0
	for p := range seen {
		list = append(list, p)
		if len(p) > maxLen {
			maxLen = len(p)
		}
	}
	// Longest first: the first match is then the most specific, so a two-letter
	// entity prefix is not shadowed by a one-letter one.
	sort.Slice(list, func(i, j int) bool {
		if len(list[i]) != len(list[j]) {
			return len(list[i]) > len(list[j])
		}
		return list[i] < list[j]
	})
	return &Set{prefixes: list, maxLen: maxLen, Count: len(list)}, nil
}

// Fetcher downloads and caches the prefix list.
type Fetcher struct {
	URL    string
	Client *http.Client
	// MaxAge is how long a cached snapshot is trusted before a refresh.
	MaxAge time.Duration

	mu  sync.RWMutex
	set *Set
	// lastErr is the most recent refresh failure, reported by the admin page.
	lastErr error
}

// NewFetcher returns a fetcher for the default source.
func NewFetcher() *Fetcher {
	return &Fetcher{
		URL:    DefaultSource,
		Client: &http.Client{Timeout: 30 * time.Second},
		MaxAge: 24 * time.Hour,
	}
}

// Current returns the cached snapshot, fetching one if the cache is empty or
// stale. It returns an error only when no snapshot has ever been obtained.
func (f *Fetcher) Current(ctx context.Context) (*Set, error) {
	f.mu.RLock()
	set := f.set
	f.mu.RUnlock()
	if set != nil && time.Since(set.FetchedAt) < f.MaxAge {
		return set, nil
	}
	if err := f.Refresh(ctx); err != nil {
		f.mu.RLock()
		have := f.set
		f.mu.RUnlock()
		if have != nil {
			// Stale but usable beats nothing: a day-old prefix list is still a
			// correct one for every station that existed yesterday.
			return have, nil
		}
		return nil, err
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.set, nil
}

// Fetch downloads the list, using a conditional request when the caller has a
// previous validator.
//
// It returns notModified=true when the upstream answered 304, which means the
// bytes on disk are already current: the caller should record that it checked
// and reload nothing. This is what makes a daily refresh cost a few hundred
// bytes instead of a hundred kilobytes on the days nothing changed.
func (f *Fetcher) Fetch(ctx context.Context, etag, lastModified string) (body []byte, respETag, respLastModified string, notModified bool, err error) {
	req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, f.URL, nil)
	if reqErr != nil {
		return nil, "", "", false, reqErr
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	if lastModified != "" {
		req.Header.Set("If-Modified-Since", lastModified)
	}
	resp, err := f.Client.Do(req)
	if err != nil {
		f.recordErr(err)
		return nil, "", "", false, err
	}
	defer resp.Body.Close()

	respETag = resp.Header.Get("ETag")
	respLastModified = resp.Header.Get("Last-Modified")
	if resp.StatusCode == http.StatusNotModified {
		return nil, respETag, respLastModified, true, nil
	}
	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("prefix source returned %s", resp.Status)
		f.recordErr(err)
		return nil, "", "", false, err
	}
	body, err = io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		f.recordErr(err)
		return nil, "", "", false, err
	}
	return body, respETag, respLastModified, false, nil
}

// Refresh downloads and replaces the snapshot.
//
// A failed download leaves the previous snapshot in place. Validation is the
// only thing standing between the archive and a flood of bot messages, so a
// network blip must not turn it off or turn it loose.
func (f *Fetcher) Refresh(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.URL, nil)
	if err != nil {
		return err
	}
	resp, err := f.Client.Do(req)
	if err != nil {
		f.recordErr(err)
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("prefix source returned %s", resp.Status)
		f.recordErr(err)
		return err
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		f.recordErr(err)
		return err
	}
	set, err := ParseCSV(body)
	if err != nil {
		f.recordErr(err)
		return err
	}
	set.FetchedAt = time.Now().UTC()
	set.Source = f.URL

	f.mu.Lock()
	f.set = set
	f.lastErr = nil
	f.mu.Unlock()
	return nil
}

func (f *Fetcher) recordErr(err error) {
	f.mu.Lock()
	f.lastErr = err
	f.mu.Unlock()
}

// Snapshot returns the cached set and the last refresh error, without
// triggering a download. The admin page uses it.
func (f *Fetcher) Snapshot() (*Set, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.set, f.lastErr
}

// Ready reports whether any snapshot has been loaded.
func (f *Fetcher) Ready() bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.set != nil
}

// ErrNoSnapshot is returned when validation is asked for before any list was
// loaded.
var ErrNoSnapshot = errors.New("prefix list not loaded")
