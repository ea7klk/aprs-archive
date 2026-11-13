package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aprs-archive/internal/aprs"
)

// newTestArchive opens a fresh writer-side archive in a temp directory and
// returns its path. Tests use the real schema, never a hand-written imitation.
func newTestArchive(t *testing.T) (string, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "archive.db")
	db, err := OpenWriter(path)
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return path, db
}

// seed writes packets through the real Writer so the tests exercise batching,
// the bulletin upsert and the sanitising path, not a direct INSERT.
func seed(t *testing.T, db *sql.DB, packets []aprs.Packet, at time.Time) {
	t.Helper()
	ctx := context.Background()
	w, err := NewWriter(ctx, db)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	for i, p := range packets {
		if err := w.Record(ctx, p, at.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	if err := w.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func msg(from, to, text string) aprs.Packet {
	return aprs.Packet{From: from, To: to, Text: text, Kind: aprs.KindMessage}
}

func bulletin(from, group, text string) aprs.Packet {
	return aprs.Packet{
		From: from, To: "BLN1" + group, Text: text, Kind: aprs.KindBulletin,
		Bulletin: aprs.Address{Slot: 1, Group: group},
	}
}

func TestSchemaAppliesAndReaderSeesIt(t *testing.T) {
	path, db := newTestArchive(t)
	seed(t, db, []aprs.Packet{msg("A-1", "B", "hi")}, time.Now())

	reader, err := OpenReader(path)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer reader.Close()
	var n int
	if err := reader.QueryRow("SELECT COUNT(*) FROM messages").Scan(&n); err != nil {
		t.Fatalf("read: %v", err)
	}
	if n != 1 {
		t.Fatalf("count = %d, want 1", n)
	}
}

func TestReaderIsReadOnly(t *testing.T) {
	path, db := newTestArchive(t)
	seed(t, db, []aprs.Packet{msg("A-1", "B", "hi")}, time.Now())
	reader, err := OpenReader(path)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer reader.Close()
	if _, err := reader.Exec("INSERT INTO messages (received_at, from_callsign, to_callsign, msg_text, kind) VALUES (1,'X','Y','z','message')"); err == nil {
		t.Fatal("a read-only archive accepted a write")
	}
}

func TestReaderRefusesUnmigratedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.db")
	other, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	other.Close()
	if _, err := OpenReader(path); err == nil {
		t.Fatal("a file without the schema was opened as an archive")
	}
}

func TestReaderRefusesMissingFile(t *testing.T) {
	if _, err := OpenReader(filepath.Join(t.TempDir(), "nope.db")); err == nil {
		t.Fatal("missing archive did not fail")
	}
}

func TestSearchFiltersAndPages(t *testing.T) {
	_, db := newTestArchive(t)
	base := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	var packets []aprs.Packet
	for i := 0; i < 30; i++ {
		packets = append(packets, msg("EA7KLK-9", "N0CALL", "ping"))
	}
	packets = append(packets, msg("DL6TJ-9", "N0CALL", "hello world"))
	seed(t, db, packets, base)

	ctx := context.Background()
	page, err := SearchMessages(ctx, db, SearchFilter{From: "ea7klk", Limit: 10})
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if page.Total != 30 {
		t.Fatalf("total = %d, want 30", page.Total)
	}
	if len(page.Rows) != 10 || page.Pages != 3 {
		t.Fatalf("rows=%d pages=%d, want 10 and 3", len(page.Rows), page.Pages)
	}
	if page.Rows[0].At.Before(page.Rows[1].At) {
		t.Fatal("results are not newest first")
	}

	page, err = SearchMessages(ctx, db, SearchFilter{Text: "WORLD"})
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if page.Total != 1 || page.Rows[0].From != "DL6TJ-9" {
		t.Fatalf("text search: %+v", page.Rows)
	}
}

func TestSearchTreatsLikeWildcardsLiterally(t *testing.T) {
	_, db := newTestArchive(t)
	seed(t, db, []aprs.Packet{msg("A_1", "B", "x"), msg("AB1", "B", "y")}, time.Now())
	page, err := SearchMessages(context.Background(), db, SearchFilter{From: "A_1"})
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if page.Total != 1 {
		t.Fatalf("underscore matched %d rows, want 1", page.Total)
	}
}

func TestEmptySearchDefaultsToLastDay(t *testing.T) {
	f := SearchFilter{}
	if err := f.Normalize(); err != nil {
		t.Fatal(err)
	}
	if f.Since.IsZero() {
		t.Fatal("an unbounded search was not given a default window")
	}
}

func TestSearchRejectsInvertedWindow(t *testing.T) {
	f := SearchFilter{Since: time.Now(), Until: time.Now().Add(-time.Hour)}
	if err := f.Normalize(); err == nil {
		t.Fatal("inverted window accepted")
	}
}

func TestBulletinUpsertCountsRepeats(t *testing.T) {
	_, db := newTestArchive(t)
	base := time.Now().UTC()
	seed(t, db, []aprs.Packet{
		bulletin("N0CALL", "NWS", "Tornado warning"),
		bulletin("N0CALL", "NWS", "Tornado warning"),
	}, base)

	page, err := Bulletins(context.Background(), db, BulletinFilter{})
	if err != nil {
		t.Fatalf("Bulletins: %v", err)
	}
	if page.Total != 1 || page.Rows[0].SeenCount != 2 {
		t.Fatalf("bulletin not deduplicated: total=%d seen=%d", page.Total, page.Rows[0].SeenCount)
	}
	if page.Rows[0].Category != aprs.CategoryWeather {
		t.Fatalf("category = %q", page.Rows[0].Category)
	}
	// The message table keeps both transmissions: the archive records the air.
	messages, err := SearchMessages(context.Background(), db, SearchFilter{Kind: "bulletin"})
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if messages.Total != 2 {
		t.Fatalf("bulletin transmissions = %d, want 2", messages.Total)
	}
}

func TestRetentionSweepRemovesOldRowsOnly(t *testing.T) {
	_, db := newTestArchive(t)
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	seed(t, db, []aprs.Packet{
		msg("OLD", "B", "ancient"),
		msg("NEW", "B", "recent"),
	}, now.Add(-400*24*time.Hour))
	seed(t, db, []aprs.Packet{msg("FRESH", "B", "today")}, now)

	deleted, _, err := Sweep(context.Background(), db, Retention{Messages: 365 * 24 * time.Hour, Bulletins: 365 * 24 * time.Hour}, now)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("deleted = %d, want 2", deleted)
	}
	page, err := SearchMessages(context.Background(), db, SearchFilter{Since: now.Add(-1000 * 24 * time.Hour), Until: now.Add(24 * time.Hour)})
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if page.Total != 1 || page.Rows[0].From != "FRESH" {
		t.Fatalf("sweep removed the wrong rows: %+v", page.Rows)
	}
}

func TestGetStationSummarises(t *testing.T) {
	_, db := newTestArchive(t)
	now := time.Now().UTC()
	seed(t, db, []aprs.Packet{
		msg("EA7KLK-9", "N0CALL", "one"),
		msg("N0CALL", "EA7KLK-9", "two"),
		msg("DL6TJ", "EA7KLK-9", "three"),
	}, now)

	ctx := context.Background()
	s, err := GetStation(ctx, db, "ea7klk-9", now)
	if err != nil {
		t.Fatalf("GetStation: %v", err)
	}
	if s.Callsign != "EA7KLK-9" || s.Sent != 1 || s.Received != 2 {
		t.Fatalf("summary = %+v", s)
	}
	if _, err := GetStation(ctx, db, "NOBODY", now); err != ErrNotFound {
		t.Fatalf("unknown station err = %v, want ErrNotFound", err)
	}
}

func TestGetMessageNotFound(t *testing.T) {
	_, db := newTestArchive(t)
	if _, err := GetMessage(context.Background(), db, 999); err != ErrNotFound {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestConversationIsSymmetric(t *testing.T) {
	_, db := newTestArchive(t)
	now := time.Now().UTC()
	seed(t, db, []aprs.Packet{
		msg("A", "B", "ping"),
		msg("B", "A", "pong"),
		msg("A", "C", "unrelated"),
	}, now)

	m, err := GetMessage(context.Background(), db, 1)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	rows, err := Conversation(context.Background(), db, m, time.Hour, 10)
	if err != nil {
		t.Fatalf("Conversation: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("conversation = %d rows, want 2", len(rows))
	}
}

func TestCollectorStateIsWritten(t *testing.T) {
	_, db := newTestArchive(t)
	seed(t, db, []aprs.Packet{msg("A", "B", "x")}, time.Now())
	state, err := GetCollectorState(context.Background(), db)
	if err != nil {
		t.Fatalf("GetCollectorState: %v", err)
	}
	if !state.Found || state.PacketsStored != 1 || state.LastWriteAt.IsZero() {
		t.Fatalf("state = %+v", state)
	}
}

func TestSeriesBucketsByHour(t *testing.T) {
	_, db := newTestArchive(t)
	base := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	seed(t, db, []aprs.Packet{
		msg("A", "B", "1"), msg("A", "B", "2"),
	}, base)
	seed(t, db, []aprs.Packet{msg("A", "B", "3")}, base.Add(2*time.Hour))

	series, err := Series(context.Background(), db, base.Add(-time.Minute), base.Add(3*time.Hour), Hourly, "")
	if err != nil {
		t.Fatalf("Series: %v", err)
	}
	if len(series) != 2 || series[0].Count != 2 || series[1].Count != 1 {
		t.Fatalf("series = %+v", series)
	}
	if series[0].Label != "2026-11-13 10:00" {
		t.Fatalf("label = %q", series[0].Label)
	}
}

func TestBackupProducesReadableCopy(t *testing.T) {
	path, db := newTestArchive(t)
	seed(t, db, []aprs.Packet{msg("A", "B", "x")}, time.Now())
	dest := filepath.Join(t.TempDir(), "backup.db")
	if err := Backup(context.Background(), db, dest); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	copyDB, err := OpenReader(dest)
	if err != nil {
		t.Fatalf("open backup: %v", err)
	}
	defer copyDB.Close()
	var n int
	if err := copyDB.QueryRow("SELECT COUNT(*) FROM messages").Scan(&n); err != nil || n != 1 {
		t.Fatalf("backup count=%d err=%v", n, err)
	}
	_ = path
}

// ---- busiest pairs ----

func TestTopPairsCountsBothDirectionsAsOnePair(t *testing.T) {
	_, db := newTestArchive(t)
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	seed(t, db, []aprs.Packet{
		msg("EA7KLK-9", "N0CALL", "one"),
		msg("N0CALL", "EA7KLK-9", "two"),
		msg("n0call", "ea7klk-9", "three"), // case differs on the wire
		msg("DL6TJ", "EA7KLK-9", "other partner"),
	}, now)

	pairs, err := TopPairs(context.Background(), db, now.Add(-time.Hour), now.Add(time.Hour), 10)
	if err != nil {
		t.Fatalf("TopPairs: %v", err)
	}
	if len(pairs) != 2 {
		t.Fatalf("pairs = %d (%+v), want 2", len(pairs), pairs)
	}
	top := pairs[0]
	if top.A != "EA7KLK-9" || top.B != "N0CALL" || top.Count != 3 {
		t.Fatalf("top pair = %+v, want EA7KLK-9 <-> N0CALL with 3", top)
	}
	if top.A > top.B {
		t.Fatalf("pair is not in lexical order: %q > %q", top.A, top.B)
	}
}

func TestTopPairsExcludesBulletins(t *testing.T) {
	_, db := newTestArchive(t)
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	seed(t, db, []aprs.Packet{
		msg("A-1", "B", "hello"),
		bulletin("A-1", "NWS", "warning"),
		bulletin("A-1", "NWS", "warning"),
	}, now)

	pairs, err := TopPairs(context.Background(), db, now.Add(-time.Hour), now.Add(time.Hour), 10)
	if err != nil {
		t.Fatalf("TopPairs: %v", err)
	}
	if len(pairs) != 1 {
		t.Fatalf("pairs = %+v, want only the direct message", pairs)
	}
	if pairs[0].Count != 1 {
		t.Fatalf("count = %d, want 1 (bulletins excluded)", pairs[0].Count)
	}
}

func TestTopPairsSelfAddressIsItsOwnPair(t *testing.T) {
	_, db := newTestArchive(t)
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	seed(t, db, []aprs.Packet{
		msg("WX1-1", "WX1-1", "telemetry to self"),
		msg("WX1-1", "WX1-1", "again"),
	}, now)
	pairs, err := TopPairs(context.Background(), db, now.Add(-time.Hour), now.Add(time.Hour), 10)
	if err != nil {
		t.Fatalf("TopPairs: %v", err)
	}
	if len(pairs) != 1 || pairs[0].A != "WX1-1" || pairs[0].B != "WX1-1" || pairs[0].Count != 2 {
		t.Fatalf("pairs = %+v", pairs)
	}
}

func TestTopPairsRespectsTheWindow(t *testing.T) {
	_, db := newTestArchive(t)
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	seed(t, db, []aprs.Packet{msg("A", "B", "old")}, now.Add(-48*time.Hour))
	seed(t, db, []aprs.Packet{msg("C", "D", "new")}, now)

	pairs, err := TopPairs(context.Background(), db, now.Add(-time.Hour), now.Add(time.Hour), 10)
	if err != nil {
		t.Fatalf("TopPairs: %v", err)
	}
	if len(pairs) != 1 || pairs[0].A != "C" {
		t.Fatalf("window not applied: %+v", pairs)
	}
}

func TestTopPairsRanksByCount(t *testing.T) {
	_, db := newTestArchive(t)
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	var packets []aprs.Packet
	for i := 0; i < 5; i++ {
		packets = append(packets, msg("BUSY", "PARTNER", "x"))
	}
	packets = append(packets, msg("QUIET", "OTHER", "x"))
	packets = append(packets, msg("QUIET", "OTHER", "x"))
	seed(t, db, packets, now)

	pairs, err := TopPairs(context.Background(), db, now.Add(-time.Hour), now.Add(time.Hour), 10)
	if err != nil {
		t.Fatalf("TopPairs: %v", err)
	}
	if pairs[0].A != "BUSY" || pairs[0].Count != 5 {
		t.Fatalf("ranking wrong: %+v", pairs)
	}
	if len(pairs) < 2 || pairs[1].Count != 2 {
		t.Fatalf("second pair wrong: %+v", pairs)
	}
}

// TestTopPairsQueryUsesTheKindIndex pins the plan the measurement chose.
//
// The alternative — an index on the canonical pair — was measured on two
// million rows and rejected: it grows the file by about 11%, the planner never
// picks it, and forcing it is twice as slow because it cannot apply the time
// filter. If this test starts failing, the query changed shape: re-measure
// before adding an index.
func TestTopPairsQueryUsesTheKindIndex(t *testing.T) {
	_, db := newTestArchive(t)
	rows, err := db.Query("EXPLAIN QUERY PLAN "+topPairsQuery,
		int64(0), int64(1<<40), 10)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var plans []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		plans = append(plans, detail)
	}
	joined := strings.Join(plans, "\n")
	if !strings.Contains(joined, "idx_messages_kind") {
		t.Fatalf("pair query no longer uses the kind index:\n%s", joined)
	}
	if strings.Contains(joined, "SCAN messages") {
		t.Fatalf("pair query scans the whole table:\n%s", joined)
	}
}

// ---- opening while the collector has not created the file yet ----

func TestWaitForReaderWaitsForTheCollector(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.db")
	go func() {
		time.Sleep(1200 * time.Millisecond)
		db, err := OpenWriter(path)
		if err == nil {
			db.Close()
		}
	}()
	start := time.Now()
	db, err := WaitForReader(path, 10*time.Second)
	if err != nil {
		t.Fatalf("WaitForReader: %v", err)
	}
	db.Close()
	if elapsed := time.Since(start); elapsed < time.Second {
		t.Fatalf("returned before the archive existed (%s)", elapsed)
	}
}

func TestWaitForReaderGivesUp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "never.db")
	if _, err := WaitForReader(path, 300*time.Millisecond); err == nil {
		t.Fatal("a missing archive did not time out")
	}
}

func TestWaitForReaderRefusesAVersionMismatchImmediately(t *testing.T) {
	// A file that is not an archive is a deployment error, not a race: waiting
	// cannot fix a wrong image, so the failure must be immediate.
	path := filepath.Join(t.TempDir(), "other.db")
	other, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := other.Exec("CREATE TABLE x (id INTEGER)"); err != nil {
		t.Fatalf("create: %v", err)
	}
	other.Close()

	start := time.Now()
	if _, err := WaitForReader(path, 10*time.Second); err == nil {
		t.Fatal("a non-archive file was accepted")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("a version mismatch was retried for %s", elapsed)
	}
}

// ---- purging rows an earlier build stored wrongly ----

func TestPurgeMisclassifiedResponsesUsesTheParser(t *testing.T) {
	_, db := newTestArchive(t)
	now := time.Now().UTC()
	seed(t, db, []aprs.Packet{
		msg("A", "B", "keep me"),
		msg("A", "B", "ack please, the signal is loud"), // prose: keep
		msg("EA3JCH-13", "EB3CLX-3", "ack49}"),          // a receipt: remove
		msg("ANSRVR", "KB3KBR-1", "ackQP}"),             // a receipt: remove
		msg("X", "Y", "rej99}"),                         // a receipt: remove
		bulletin("C", "NWS", "warning"),                 // not a message: keep
	}, now)
	// The old recogniser would have left these as kind=message.
	removed, err := PurgeMisclassifiedResponses(context.Background(), db)
	if err != nil {
		t.Fatalf("PurgeMisclassifiedResponses: %v", err)
	}
	if removed != 3 {
		t.Fatalf("removed %d rows, want 3", removed)
	}
	var remaining int
	if err := db.QueryRow("SELECT COUNT(*) FROM messages").Scan(&remaining); err != nil {
		t.Fatalf("count: %v", err)
	}
	if remaining != 3 {
		t.Fatalf("%d rows remain, want 3 (message, prose, bulletin)", remaining)
	}
	var bodies []string
	rows, err := db.Query("SELECT msg_text FROM messages ORDER BY id")
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var b string
		if err := rows.Scan(&b); err != nil {
			t.Fatalf("scan: %v", err)
		}
		bodies = append(bodies, b)
	}
	for _, b := range bodies {
		if b == "ack49}" || b == "ackQP}" || b == "rej99}" {
			t.Fatalf("receipt %q survived the purge", b)
		}
	}
	// Idempotent: a second pass leaves the prose and the message alone.
	if again, err := PurgeMisclassifiedResponses(context.Background(), db); err != nil || again != 0 {
		t.Fatalf("second purge removed %d, err %v", again, err)
	}
}

func TestPurgeMisclassifiedResponsesHandlesAChunkedBacklog(t *testing.T) {
	_, db := newTestArchive(t)
	now := time.Now().UTC()
	packets := make([]aprs.Packet, 0, 2600)
	for i := 0; i < 2600; i++ {
		packets = append(packets, msg("A", "B", "ack49}"))
	}
	packets = append(packets, msg("A", "B", "a real message"))
	seed(t, db, packets, now)

	removed, err := PurgeMisclassifiedResponses(context.Background(), db)
	if err != nil {
		t.Fatalf("PurgeMisclassifiedResponses: %v", err)
	}
	if removed != 2600 {
		t.Fatalf("removed %d, want 2600", removed)
	}
	var remaining int
	if err := db.QueryRow("SELECT COUNT(*) FROM messages").Scan(&remaining); err != nil {
		t.Fatalf("count: %v", err)
	}
	if remaining != 1 {
		t.Fatalf("%d rows remain, want 1", remaining)
	}
}
