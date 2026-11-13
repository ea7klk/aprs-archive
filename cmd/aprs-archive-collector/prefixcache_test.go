package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aprs-archive/internal/prefixes"
	"aprs-archive/internal/store"
)

// fixture builds a prefix CSV body with the given extra prefixes.
func fixture(prefixesList ...string) []byte {
	var b strings.Builder
	b.WriteString("EA,Spain,281,EU,14,33,40,-4,0,EA;\n")
	b.WriteString("DL,Germany,230,EU,14,28,51,10,0,DL;\n")
	for _, p := range prefixesList {
		fmt.Fprintf(&b, "%s,Generated,999,EU,15,28,0,0,0,%s;\n", p, p)
	}
	// Pad past ParseCSV's plausibility floor.
	for i := 0; i < 150; i++ {
		fmt.Fprintf(&b, "X%d,Pad %d,999,EU,15,28,0,0,0,X%d;\n", i, i, i)
	}
	return []byte(b.String())
}

func newTestDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "archive.db")
	db, err := store.OpenWriter(path)
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db, path
}

// TestNewListIsAdoptedWithoutRestart is the behaviour the operator asked for: a
// list published by someone else reaches the running collector within its watch
// tick, with no restart and no new APRS-IS session.
func TestNewListIsAdoptedWithoutRestart(t *testing.T) {
	db, _ := newTestDB(t)
	ctx := context.Background()
	fetcher := &prefixes.Fetcher{URL: "http://example.invalid/cty.csv"}

	// Seed generation 1 with a list that does not know "ZZ".
	if _, err := store.WritePrefixSnapshot(ctx, db, store.PrefixSnapshot{
		Body: fixture("AA"), Source: "test", PrefixCount: 150,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	v := newValidator(ctx, db, fetcher)
	if v.set == nil {
		t.Fatal("validator did not load the seeded list")
	}
	if v.Keep("ZZ9ABC", "EA7KLK") {
		t.Fatal("test setup: ZZ should be unknown to generation 1")
	}

	// Somebody publishes a new list, generation 2, that knows "ZZ".
	if _, err := store.WritePrefixSnapshot(ctx, db, store.PrefixSnapshot{
		Body: fixture("AA", "ZZ"), Source: "test", PrefixCount: 151,
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// No restart. The watch tick is all that runs.
	v.Watch(ctx)

	if !v.Keep("ZZ9ABC", "EA7KLK") {
		t.Fatal("the new list was not adopted: the collector still rejects ZZ")
	}
}

// TestWatchDoesNothingWhenUnchanged keeps the tick cheap: the generation is
// read and compared, and an unchanged list is not re-parsed.
func TestWatchDoesNothingWhenUnchanged(t *testing.T) {
	db, _ := newTestDB(t)
	ctx := context.Background()
	if _, err := store.WritePrefixSnapshot(ctx, db, store.PrefixSnapshot{Body: fixture("AA")}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	v := newValidator(ctx, db, &prefixes.Fetcher{})
	before := v.reloads.Load()
	for i := 0; i < 5; i++ {
		v.Watch(ctx)
	}
	if got := v.reloads.Load(); got != before {
		t.Fatalf("reloaded %d times for an unchanged list", got-before)
	}
}

// TestBrokenSnapshotKeepsTheGoodList is the safety property: a corrupt cache
// must never replace a list that works.
func TestBrokenSnapshotKeepsTheGoodList(t *testing.T) {
	db, _ := newTestDB(t)
	ctx := context.Background()
	if _, err := store.WritePrefixSnapshot(ctx, db, store.PrefixSnapshot{Body: fixture("AA")}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	v := newValidator(ctx, db, &prefixes.Fetcher{})
	if !v.Keep("EA7KLK", "DL6TJ") {
		t.Fatal("test setup: the good list should accept EA and DL")
	}

	// Write an unusable body directly, bypassing the parse check a writer does.
	if _, err := db.Exec(`UPDATE prefix_cache SET body = ?, generation = generation + 1 WHERE id = 1`,
		[]byte("<html>Service Unavailable</html>")); err != nil {
		t.Fatalf("corrupt: %v", err)
	}
	v.Watch(ctx)

	if !v.Keep("EA7KLK", "DL6TJ") {
		t.Fatal("a corrupt snapshot replaced the working list")
	}
	if v.set == nil {
		t.Fatal("the validator lost its list")
	}
}

// TestNoListFailsClosed is the deliberate reversal of the earlier behaviour.
func TestNoListFailsClosed(t *testing.T) {
	db, _ := newTestDB(t)
	v := newValidator(context.Background(), db, &prefixes.Fetcher{})
	if v.set != nil {
		t.Fatal("test setup: expected no list")
	}
	if v.Keep("EA7KLK", "DL6TJ") {
		t.Fatal("with no prefix list, messages were stored; validation must fail closed")
	}
	if v.droppedNoList.Load() == 0 {
		t.Fatal("the no-list drop was not counted")
	}
}

// TestExceptionsStillWin checks that fail-closed does not override an operator
// decision: APRSPH must survive even with no list at all.
func TestExceptionsStillWin(t *testing.T) {
	db, _ := newTestDB(t)
	ctx := context.Background()
	if err := store.SetException(ctx, db, "EA7KLK", true, "operator"); err != nil {
		t.Fatalf("exception: %v", err)
	}
	v := newValidator(ctx, db, &prefixes.Fetcher{})
	if v.allow("EA7KLK") != true {
		t.Fatal("an explicit keep was overridden by fail-closed")
	}
}

// TestRefreshRejectsABadDownload proves the upstream path refuses garbage.
func TestRefreshRejectsABadDownload(t *testing.T) {
	db, _ := newTestDB(t)
	ctx := context.Background()
	// A working cache exists.
	if _, err := store.WritePrefixSnapshot(ctx, db, store.PrefixSnapshot{Body: fixture("AA")}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "<html>404 not found</html>")
	}))
	defer srv.Close()
	refreshPrefixes(ctx, db, &prefixes.Fetcher{URL: srv.URL, Client: srv.Client()})

	snap, err := store.ReadPrefixSnapshot(ctx, db)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if _, perr := prefixes.ParseCSV(snap.Body); perr != nil {
		t.Fatal("a bad download replaced the working cache")
	}
}

// TestRefreshUsesConditionalRequest proves the daily refresh is cheap when
// upstream has not changed.
func TestRefreshUsesConditionalRequest(t *testing.T) {
	db, _ := newTestDB(t)
	ctx := context.Background()
	body := fixture("AA")
	if _, err := store.WritePrefixSnapshot(ctx, db, store.PrefixSnapshot{Body: body, ETag: `"v1"`}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	asked := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.Header.Get("If-None-Match")
		w.WriteHeader(http.StatusNotModified)
	}))
	defer srv.Close()
	refreshPrefixes(ctx, db, &prefixes.Fetcher{URL: srv.URL, Client: srv.Client()})

	if asked != `"v1"` {
		t.Fatalf("If-None-Match = %q, want the stored ETag", asked)
	}
	snap, err := store.ReadPrefixSnapshot(ctx, db)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(snap.Body) != string(body) {
		t.Fatal("a 304 changed the cached body")
	}
	if !snap.FetchedAt.After(time.Now().Add(-time.Minute)) {
		t.Fatal("a 304 did not record that the check happened")
	}
}

// TestEnsureCacheSeedsFromUpstream covers a fresh archive.
func TestEnsureCacheSeedsFromUpstream(t *testing.T) {
	db, _ := newTestDB(t)
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(fixture("AA"))
	}))
	defer srv.Close()
	if err := ensurePrefixCache(ctx, db, &prefixes.Fetcher{URL: srv.URL, Client: srv.Client()}); err != nil {
		t.Fatalf("ensurePrefixCache: %v", err)
	}
	snap, err := store.ReadPrefixSnapshot(ctx, db)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if snap.Generation != 1 {
		t.Fatalf("generation = %d, want 1", snap.Generation)
	}
}

// TestEnsureCachePrefersTheCache proves a restart with no network still
// validates: the cached list is used and the failed fetch is not fatal.
func TestEnsureCachePrefersTheCache(t *testing.T) {
	db, _ := newTestDB(t)
	ctx := context.Background()
	if _, err := store.WritePrefixSnapshot(ctx, db, store.PrefixSnapshot{Body: fixture("AA")}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// A server that always fails: this is the "no network at restart" case.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	if err := ensurePrefixCache(ctx, db, &prefixes.Fetcher{URL: srv.URL, Client: srv.Client()}); err != nil {
		t.Fatalf("ensurePrefixCache should accept the cache: %v", err)
	}
	v := newValidator(ctx, db, &prefixes.Fetcher{})
	if !v.Keep("EA7KLK", "DL6TJ") {
		t.Fatal("restart with no network did not validate from the cache")
	}
}
