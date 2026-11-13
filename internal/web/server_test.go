package web

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aprs-archive/internal/aprs"
	"aprs-archive/internal/store"
)

// newTestServer builds a server over a real archive in a temp directory, seeded
// through the store's own writer so the tests exercise the same path the
// collector uses.
func newTestServer(t *testing.T) (*Server, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "archive.db")
	db, err := store.OpenWriter(path)
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}
	ctx := context.Background()
	w, err := store.NewWriter(ctx, db)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	packets := []aprs.Packet{
		{From: "EA7KLK-9", To: "N0CALL", Text: "hello from Spain", Kind: aprs.KindMessage, MessageID: "A1"},
		{From: "N0CALL", To: "EA7KLK-9", Text: "hello back", Kind: aprs.KindMessage},
		{From: "N0CALL", To: "EA7KLK-9", Text: "ackA1", Kind: aprs.KindAck, AckID: "A1"},
		{From: "DB0KUH-10", To: "BLN1WXZL", Text: "DWD WARNUNG vor FROST", Kind: aprs.KindBulletin,
			Bulletin: aprs.Address{Slot: 1, Group: "WXZL"}},
		{From: "DL6TJ-9", To: "BLN1NWS", Text: "Tornado watch", Kind: aprs.KindBulletin,
			Bulletin: aprs.Address{Slot: 1, Group: "NWS"}},
	}
	for i, p := range packets {
		// The ack is parsed as an ack; whether it is stored is the collector's
		// decision, and the web layer must render correctly either way.
		if err := w.Record(ctx, p, now.Add(-time.Duration(i)*time.Minute)); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	if err := w.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reader, err := store.OpenReader(path)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	t.Cleanup(func() { reader.Close(); db.Close() })

	srv, err := New(Config{
		DBPath:            path,
		AppName:           "APRS Archive",
		Location:          "https://aprs.ea7klk.es",
		Topic:             "test",
		Version:           "test",
		Collector:         "EA7KLK-15",
		RetentionDays:     365,
		CollectorStaleFor: 15 * time.Minute,
		Now:               func() time.Time { return now },
	}, reader)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv, reader
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestEveryPageRenders(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.Handler()
	pages := []string{
		"/", "/messages", "/bulletins", "/statistics", "/reports",
		"/reports?run=1&group_by=day", "/reports?run=1&group_by=station",
		"/reports?run=1&group_by=kind", "/reports?run=1&group_by=category",
		"/stations", "/stations/EA7KLK-9", "/about", "/healthz",
	}
	for _, p := range pages {
		rec := get(t, h, p)
		if rec.Code != http.StatusOK {
			t.Errorf("%s -> %d: %s", p, rec.Code, rec.Body.String()[:min(200, rec.Body.Len())])
			continue
		}
		if !strings.Contains(rec.Body.String(), "</html>") && p != "/healthz" {
			t.Errorf("%s did not render a complete page", p)
		}
	}
}

func TestDetailPagesAndNotFound(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.Handler()
	if rec := get(t, h, "/messages/1"); rec.Code != http.StatusOK {
		t.Fatalf("/messages/1 -> %d", rec.Code)
	}
	if rec := get(t, h, "/messages/999999"); rec.Code != http.StatusNotFound {
		t.Fatalf("missing message -> %d", rec.Code)
	}
	if rec := get(t, h, "/messages/not-a-number"); rec.Code != http.StatusNotFound {
		t.Fatalf("bad id -> %d", rec.Code)
	}
	if rec := get(t, h, "/bulletins/1"); rec.Code != http.StatusOK {
		t.Fatalf("/bulletins/1 -> %d", rec.Code)
	}
	if rec := get(t, h, "/stations/NOBODY"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown station -> %d", rec.Code)
	}
	if rec := get(t, h, "/"); rec.Code != http.StatusOK {
		t.Fatalf("home -> %d", rec.Code)
	}
}

func TestUnstoredKindIsNotAcceptedAsAFilter(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := get(t, srv.Handler(), "/messages?kind=ack")
	if rec.Code != http.StatusOK {
		t.Fatalf("ack filter -> %d", rec.Code)
	}
	// The query is normalised away, so the page is the unfiltered one.
	if strings.Contains(rec.Body.String(), `value="ack" selected`) {
		t.Fatal("the ack option is still selectable")
	}
}

func TestSearchFormsHaveNoAckOrRejOption(t *testing.T) {
	srv, _ := newTestServer(t)
	for _, p := range []string{"/messages", "/reports"} {
		body := get(t, srv.Handler(), p).Body.String()
		if strings.Contains(body, `<option value="ack"`) || strings.Contains(body, `<option value="rej"`) {
			t.Errorf("%s still offers an ack or rej filter", p)
		}
	}
}

func TestThirdPartyScriptIsEscaped(t *testing.T) {
	// The web handle is read-only by design, so the hostile row is seeded
	// through the writer and then reopened, exactly as the collector would.
	dir := t.TempDir()
	path := filepath.Join(dir, "archive.db")
	w := openWriter(t, path)
	wdb := w.db
	_, err := wdb.Exec(`INSERT INTO messages (received_at, from_callsign, to_callsign, msg_text, kind)
		VALUES (1763000000, 'EA7KLK-15', 'N0CALL', 'x <script>alert(1)</script> y', 'message')`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	var id int64
	if err := wdb.QueryRow("SELECT MAX(id) FROM messages").Scan(&id); err != nil {
		t.Fatalf("id: %v", err)
	}
	reader, err := store.OpenReader(path)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer reader.Close()
	srv, err := New(Config{DBPath: path, AppName: "APRS Archive", Version: "test",
		RetentionDays: 365, CollectorStaleFor: 15 * time.Minute,
		Now: func() time.Time { return time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC) }}, reader)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	body := get(t, srv.Handler(), "/messages/"+itoa(id)).Body.String()
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Fatal("third-party script reached the page unescaped")
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Fatal("the message body is missing from the page")
	}
}

func TestSecurityHeaders(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := get(t, srv.Handler(), "/")
	for header, want := range map[string]string{
		"Content-Security-Policy": "default-src 'none'",
		"X-Content-Type-Options":  "nosniff",
		"X-Frame-Options":         "DENY",
		"Referrer-Policy":         "same-origin",
	} {
		if got := rec.Header().Get(header); !strings.Contains(got, want) {
			t.Errorf("%s = %q, want to contain %q", header, got, want)
		}
	}
}

func TestCSVIsAnAttachment(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := get(t, srv.Handler(), "/reports/export.csv?group_by=day")
	if rec.Code != http.StatusOK {
		t.Fatalf("csv -> %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Fatalf("Content-Type = %q", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") {
		t.Fatalf("Content-Disposition = %q", cd)
	}
	lines := strings.Split(strings.TrimSpace(rec.Body.String()), "\n")
	if len(lines) < 2 || lines[0] != "label,packets,share" {
		t.Fatalf("unexpected csv: %q", lines)
	}
	if !strings.Contains(lines[1], "2026-11-13") {
		t.Fatalf("day grouping key wrong: %q", lines[1])
	}
}

func TestBulletinCategoryFilter(t *testing.T) {
	srv, _ := newTestServer(t)
	// Both seeded bulletins resolve to weather: NWS is a declared weather
	// group and WXZL matches the WX prefix family. The filter must therefore
	// show both and exclude nothing weather-shaped.
	body := get(t, srv.Handler(), "/bulletins?category=weather").Body.String()
	if !strings.Contains(body, "DWD WARNUNG") {
		t.Fatal("weather bulletin missing from the weather category")
	}
	if !strings.Contains(body, "Tornado watch") {
		t.Fatal("NWS bulletin missing from the weather category")
	}
	// A category with nothing in it must show none of them.
	body = get(t, srv.Handler(), "/bulletins?category=satellite").Body.String()
	if strings.Contains(body, "DWD WARNUNG") || strings.Contains(body, "Tornado watch") {
		t.Fatal("weather bulletins leak into another category")
	}
	// An invalid category falls back to "all" rather than an empty page or an
	// error.
	if rec := get(t, srv.Handler(), "/bulletins?category=nonsense"); rec.Code != http.StatusOK {
		t.Fatalf("invalid category -> %d", rec.Code)
	}
}

func TestReportRejectsBadWindowPolitely(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := get(t, srv.Handler(), "/reports?run=1&group_by=day&since=2026-11-15&until=2026-11-01")
	if rec.Code != http.StatusOK {
		t.Fatalf("inverted window -> %d", rec.Code)
	}
}

// openWriter is the seam the escape test needs: the web handle is read-only, so
// a test that has to insert hostile text opens the writer side instead.
type writerDB struct {
	db  *sql.DB
	raw *store.Writer
}

func openWriter(t *testing.T, path string) *writerDB {
	t.Helper()
	db, err := store.OpenWriter(path)
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return &writerDB{db: db}
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func TestStatisticsShowsBusiestPairs(t *testing.T) {
	// The seeded archive already holds EA7KLK-9 <-> N0CALL. Pairs are rendered
	// from it, and the bulletin addressees must never appear as stations.
	srv, _ := newTestServer(t)
	body := get(t, srv.Handler(), "/statistics?days=7").Body.String()
	if !strings.Contains(body, "Busiest pairs") {
		t.Fatal("the busiest-pairs section is missing")
	}
	if !strings.Contains(body, "N0CALL") {
		t.Fatal("the seeded pair is missing from the ranking")
	}
	if strings.Contains(body, "BLN1WXZL</a>") {
		t.Fatal("a bulletin address appears as a station in the pair ranking")
	}
}

// TestReportIncludesTheDayTheUserPicked is the regression for the empty
// reports reported on 2026-11-13.
//
// The form offers days, not instants. A report whose "to date" was today used
// to stop at midnight today, so any row after midnight was excluded — and on a
// young archive, whose first row is mid-day, that made the only useful range an
// empty one. The seeded row is deliberately at 00:30 on the chosen day: the
// old code returned zero for it, the fixed code does not.
func TestReportIncludesTheDayTheUserPicked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.db")
	db, err := store.OpenWriter(path)
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}
	ctx := context.Background()
	w, err := store.NewWriter(ctx, db)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	day := time.Date(2026, 11, 13, 0, 30, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		if err := w.Record(ctx, aprs.Packet{From: "EA7KLK-9", To: "N0CALL", Text: "x", Kind: aprs.KindMessage}, day.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	if err := w.Close(ctx); err != nil {
		t.Fatalf("close: %v", err)
	}
	reader, err := store.OpenReader(path)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer reader.Close()
	db.Close()
	srv, err := New(Config{DBPath: path, AppName: "APRS Archive", Version: "test",
		RetentionDays: 365, CollectorStaleFor: 15 * time.Minute,
		Now: func() time.Time { return day.Add(6 * time.Hour) }}, reader)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h := srv.Handler()

	// The range the user actually sent: a window that ends on the chosen day and
	// begins before it, so window() has nothing to repair and the upper bound is
	// the only thing deciding whether the day is included.
	for _, p := range []string{
		"/reports?run=1&group_by=day&since=2026-11-10&until=2026-11-13",
		"/reports?run=1&group_by=hour&since=2026-11-10&until=2026-11-13",
		"/reports?run=1&group_by=station&since=2026-11-10&until=2026-11-13",
		"/reports?run=1&group_by=kind&since=2026-11-10&until=2026-11-13",
	} {
		body := get(t, h, p).Body.String()
		if strings.Contains(body, "The report is empty") {
			t.Errorf("%s returned an empty report", p)
		}
		if strings.Contains(body, ">0 packets") {
			t.Errorf("%s counted zero packets", p)
		}
	}
}

// TestReportCSVMatchesTheDayRange checks the export speaks the same inclusive
// days as the form, so the file is not the empty one the page no longer shows.
func TestReportCSVMatchesTheDayRange(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := get(t, srv.Handler(), "/reports/export.csv?group_by=day&since=2026-11-13&until=2026-11-13")
	lines := strings.Split(strings.TrimSpace(rec.Body.String()), "\n")
	if len(lines) < 2 {
		t.Fatalf("csv has no data rows: %q", lines)
	}
	if !strings.Contains(lines[1], "2026-11-13") {
		t.Fatalf("csv row is not the chosen day: %q", lines[1])
	}
	if strings.Contains(lines[1], ",0,") {
		t.Fatalf("csv day has zero packets: %q", lines[1])
	}
}

// TestReportFormEchoesTheInclusiveDay guards the other half of the confusion:
// the form must not show the exclusive bound, or reopening a report changes it.
func TestReportFormEchoesTheInclusiveDay(t *testing.T) {
	srv, _ := newTestServer(t)
	body := get(t, srv.Handler(), "/reports?since=2026-11-01&until=2026-11-13").Body.String()
	if !strings.Contains(body, `name="until" type="date" value="2026-11-13"`) {
		t.Fatal("the form does not echo the day the user picked")
	}
}

// TestSearchFormEchoesTheInclusiveDay is the same defect as the report form's:
// Until is exclusive in the query but a day in the form. Echoing the exclusive
// bound moved the date forward by one every time a search was reopened.
func TestSearchFormEchoesTheInclusiveDay(t *testing.T) {
	srv, _ := newTestServer(t)
	body := get(t, srv.Handler(), "/messages?since=2026-11-01&until=2026-11-13").Body.String()
	if !strings.Contains(body, `name="until" type="date" value="2026-11-13"`) {
		t.Fatal("the search form does not echo the day the user picked")
	}
}

// ---- reporting limit ----

// TestReportWindowIsCappedAtThreeMonths pins the operational limit: a report
// scans a window, and an unbounded one is a full table scan.
func TestReportWindowIsCappedAtThreeMonths(t *testing.T) {
	srv, _ := newTestServer(t)
	if srv.cfg.ReportMaxDays != 92 {
		t.Fatalf("default report limit = %d days, want 92", srv.cfg.ReportMaxDays)
	}
	body := get(t, srv.Handler(), "/reports?run=1&group_by=day&since=2025-01-01&until=2026-11-13").Body.String()
	if !strings.Contains(body, "reporting limit") {
		t.Fatal("a trimmed window is not announced")
	}
	// The end of the range is kept; the start moves forward.
	if strings.Contains(body, "2025-01-01") && strings.Contains(body, "→ 2025-01-01") {
		t.Fatal("the start of the range was not trimmed")
	}
}

func TestReportWindowLimitIsExactlyTheLimit(t *testing.T) {
	spec := ReportSpec{GroupBy: "day", Since: time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC), Until: time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)}
	spec.window(time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC), 92)
	if spec.Truncated {
		t.Fatal("a 92-day window was trimmed despite the 92-day limit")
	}
}

func TestReportWindowTrimsFromTheStart(t *testing.T) {
	end := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	spec := ReportSpec{GroupBy: "day", Since: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Until: end}
	spec.window(time.Now(), 92)
	if !spec.Truncated {
		t.Fatal("an over-long window was not reported as trimmed")
	}
	if !spec.Until.Equal(end) {
		t.Fatalf("the end moved: %v, want %v", spec.Until, end)
	}
	if got := spec.Until.Sub(spec.Since); got != 92*24*time.Hour {
		t.Fatalf("window = %v, want 92 days", got)
	}
}

func TestReportLimitNeverExceedsRetention(t *testing.T) {
	// A report limit above retention would promise history the archive does
	// not hold.
	db, err := store.OpenWriter(filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}
	defer db.Close()
	srv, err := New(Config{DBPath: "x", RetentionDays: 30, ReportMaxDays: 365, Now: time.Now}, db)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if srv.cfg.ReportMaxDays != 30 {
		t.Fatalf("report limit = %d, want 30 (retention)", srv.cfg.ReportMaxDays)
	}
}

func TestReportDatePickerCannotOfferATooEarlyStart(t *testing.T) {
	srv, _ := newTestServer(t)
	body := get(t, srv.Handler(), "/reports?since=2026-01-01&until=2026-11-13").Body.String()
	// "Until 13 November" is exclusive in the query (14 November 00:00), so the
	// earliest permitted start is 92 days before that: 14 August.
	if !strings.Contains(body, `min="2026-08-14"`) {
		t.Fatal("the from-date field does not bound itself to the reporting limit")
	}
	if !strings.Contains(body, "at most 92 days") {
		t.Fatal("the page does not state the reporting limit")
	}
}
