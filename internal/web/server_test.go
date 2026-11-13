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
