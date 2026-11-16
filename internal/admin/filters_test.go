package admin

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aprs-archive/internal/aprs"
	"aprs-archive/internal/store"
)

// newFilterServer builds the admin app over an archive seeded with a mix of
// conversation and telemetry.
func newFilterServer(t *testing.T) *httptest.Server {
	t.Helper()
	path := filepath.Join(t.TempDir(), "archive.db")
	w, err := store.OpenWriter(path)
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}
	ctx := context.Background()
	writer, err := store.NewWriter(ctx, w)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	bodies := []string{
		"uptime: 3d 3h 30m 37s",
		"uptime: 4d 1h 2m 3s",
		"CQ APRSMX LA GUAJIRA",
		"73",
		"hello from Spain",
		"UPTM.13T06:36:40",
	}
	for i, b := range bodies {
		if err := writer.Record(ctx, aprs.Packet{
			From: "EA7KLK-9", To: "N0CALL", Text: b, Kind: aprs.KindMessage,
		}, now.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	writer.Close(ctx)
	// Seed the filters while the writer is still open.
	if err := store.EnsureMessageFilters(ctx, w); err != nil {
		t.Fatalf("EnsureMessageFilters: %v", err)
	}
	w.Close()

	lastAdminDBPath = path
	db, err := store.OpenAdminWriter(path)
	if err != nil {
		t.Fatalf("OpenAdminWriter: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	srv, err := New(Config{DBPath: path, AppName: "APRS Archive", Version: "t",
		Password: "pw", RetentionDays: 365, Now: func() time.Time { return now }}, db)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewTLSServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func messageCount(t *testing.T, path string) int64 {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	var n int64
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func TestAddingAnEnabledFilterPurgesStoredMatches(t *testing.T) {
	ts := newFilterServer(t)
	path := lastAdminDBPath
	before := messageCount(t, path)
	if before != 6 {
		t.Fatalf("seeded %d messages, want 6", before)
	}

	c, token := login(t, ts)
	resp, err := c.PostForm(ts.URL+"/admin/filters", url.Values{
		"confirm": {token},
		"name":    {"uptime phrase test"},
		"pattern": {`(?i)^uptime\s*:\s*\d`},
		"note":    {"test"},
		"enabled": {"true"},
	})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	if !strings.Contains(resp.Request.URL.RawQuery, "Removed+2") {
		t.Fatalf("the save did not report removing 2 rows: %q", resp.Request.URL.RawQuery)
	}

	after := messageCount(t, path)
	if after != before-2 {
		t.Fatalf("%d messages remain, want %d (two uptime rows removed)", after, before-2)
	}
	// The real messages must survive.
	body := getBody(t, c, ts.URL+"/admin/filters")
	if body == "" {
		t.Fatal("empty page")
	}
}

func TestAddingADisabledFilterDeletesNothing(t *testing.T) {
	ts := newFilterServer(t)
	path := lastAdminDBPath
	before := messageCount(t, path)

	c, token := login(t, ts)
	resp, err := c.PostForm(ts.URL+"/admin/filters", url.Values{
		"confirm": {token},
		"name":    {"uptime but off"},
		"pattern": {`(?i)^uptime\s*:\s*\d`},
		"enabled": {"false"},
	})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	if !strings.Contains(resp.Request.URL.RawQuery, "no+stored+messages+were+removed") &&
		!strings.Contains(resp.Request.URL.RawQuery, "disabled") {
		t.Fatalf("the save did not explain that nothing was removed: %q", resp.Request.URL.RawQuery)
	}
	if after := messageCount(t, path); after != before {
		t.Fatalf("%d messages remain, want %d: a disabled filter deleted rows", after, before)
	}
}

func TestEnablingAFilterPurgesAndDisablingDoesNot(t *testing.T) {
	ts := newFilterServer(t)
	path := lastAdminDBPath
	c, token := login(t, ts)

	// Add the rule disabled.
	if _, err := c.PostForm(ts.URL+"/admin/filters", url.Values{
		"confirm": {token}, "name": {"uptime off"}, "pattern": {`(?i)^uptime\s*:\s*\d`}, "enabled": {"false"},
	}); err != nil {
		t.Fatalf("post: %v", err)
	}
	before := messageCount(t, path)

	// Find its id.
	var id int64
	db, _ := sql.Open("sqlite", "file:"+path+"?mode=ro")
	db.QueryRow(`SELECT id FROM message_filters WHERE name = 'uptime off'`).Scan(&id)
	db.Close()
	if id == 0 {
		t.Fatal("filter was not created")
	}

	resp, err := c.PostForm(ts.URL+"/admin/filters/"+strconvItoa(id)+"/toggle", url.Values{
		"confirm": {token}, "enabled": {"1"},
	})
	if err != nil {
		t.Fatalf("toggle: %v", err)
	}
	resp.Body.Close()
	if !strings.Contains(resp.Request.URL.RawQuery, "Removed+2") {
		t.Fatalf("enabling did not report a purge: %q", resp.Request.URL.RawQuery)
	}
	if after := messageCount(t, path); after != before-2 {
		t.Fatalf("%d remain, want %d", after, before-2)
	}

	// Disabling must not delete anything further.
	before = messageCount(t, path)
	resp, err = c.PostForm(ts.URL+"/admin/filters/"+strconvItoa(id)+"/toggle", url.Values{
		"confirm": {token}, "enabled": {"0"},
	})
	if err != nil {
		t.Fatalf("toggle: %v", err)
	}
	resp.Body.Close()
	if after := messageCount(t, path); after != before {
		t.Fatalf("disabling deleted %d rows", before-after)
	}
}

func TestPurgeRefusesAnInvalidPattern(t *testing.T) {
	ts := newFilterServer(t)
	path := lastAdminDBPath
	before := messageCount(t, path)
	c, token := login(t, ts)
	resp, err := c.PostForm(ts.URL+"/admin/filters", url.Values{
		"confirm": {token}, "name": {"broken"}, "pattern": {"([unclosed"}, "enabled": {"true"},
	})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	if !strings.Contains(resp.Request.URL.RawQuery, "err=") {
		t.Fatal("a broken pattern was accepted")
	}
	if after := messageCount(t, path); after != before {
		t.Fatalf("%d remain, want %d", after, before)
	}
}

func TestPurgeAllRemovesEveryEnabledMatch(t *testing.T) {
	ts := newFilterServer(t)
	path := lastAdminDBPath
	c, token := login(t, ts)
	resp, err := c.PostForm(ts.URL+"/admin/filters/purge", url.Values{"confirm": {token}})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	// The seeded uptime and UPTM rules are enabled by default.
	if after := messageCount(t, path); after != 3 {
		t.Fatalf("%d remain, want 3 (three uptime rows removed, three real messages kept)", after)
	}
	_ = http.StatusOK
}

func strconvItoa(n int64) string {
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

// ---- delete matching a pattern now ----

func TestApplyPreviewCountsWithoutDeleting(t *testing.T) {
	ts := newFilterServer(t)
	path := lastAdminDBPath
	before := messageCount(t, path)
	c, token := login(t, ts)

	body := postForm(t, c, ts.URL+"/admin/filters/apply/preview", url.Values{
		"confirm": {token}, "pattern": {`(?i)^uptime\s*:\s*\d`},
	})
	if !strings.Contains(body, "stored messages match") && !strings.Contains(body, "stored message match") {
		t.Fatalf("the preview did not report a count:\n%s", firstLines(body, 40))
	}
	if !strings.Contains(body, "APPLY NOW") {
		t.Fatal("the preview did not offer the apply button")
	}
	if !strings.Contains(body, "uptime: 3d 3h 30m 37s") {
		t.Fatal("the preview did not show an example of what it would delete")
	}
	// Nothing removed by a preview.
	if after := messageCount(t, path); after != before {
		t.Fatalf("the preview deleted %d rows", before-after)
	}
}

func TestApplyNow(t *testing.T) {
	ts := newFilterServer(t)
	path := lastAdminDBPath
	before := messageCount(t, path)
	c, token := login(t, ts)

	body := postForm(t, c, ts.URL+"/admin/filters/apply/preview", url.Values{
		"confirm": {token}, "pattern": {`(?i)^uptime\s*:\s*\d`},
	})
	applyToken := extractApplyToken(t, body)
	if applyToken == "" {
		t.Fatal("no apply token in the preview")
	}

	resp, err := c.PostForm(ts.URL+"/admin/filters/apply/delete", url.Values{
		"confirm":     {token},
		"pattern":     {`(?i)^uptime\s*:\s*\d`},
		"apply_token": {applyToken},
	})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	if !strings.Contains(resp.Request.URL.RawQuery, "Deleted+2") {
		t.Fatalf("the delete did not report removing 2: %q", resp.Request.URL.RawQuery)
	}
	if after := messageCount(t, path); after != before-2 {
		t.Fatalf("%d remain, want %d", after, before-2)
	}
	// The real messages are still there.
	if after := messageCount(t, path); after != 4 {
		t.Fatalf("%d remain, want the four real messages", after)
	}
}

// TestApplyRequiresAPreview is the safety property: a delete cannot be triggered
// without first seeing the count. The token encodes the pattern, so it also
// cannot be reused for a different one.
func TestApplyRequiresAPreview(t *testing.T) {
	ts := newFilterServer(t)
	path := lastAdminDBPath
	before := messageCount(t, path)
	c, token := login(t, ts)

	// No token at all.
	resp, err := c.PostForm(ts.URL+"/admin/filters/apply/delete", url.Values{
		"confirm": {token}, "pattern": {`(?i)^uptime`},
	})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	if !strings.Contains(resp.Request.URL.RawQuery, "Preview+this+pattern+first") {
		t.Fatalf("a delete without a preview was not refused: %q", resp.Request.URL.RawQuery)
	}
	if after := messageCount(t, path); after != before {
		t.Fatal("a delete without a preview removed rows")
	}

	// A token for a different pattern must not work.
	body := postForm(t, c, ts.URL+"/admin/filters/apply/preview", url.Values{
		"confirm": {token}, "pattern": {`^NOTHING_MATCHES_THIS$`},
	})
	stolen := extractApplyToken(t, body)
	resp, err = c.PostForm(ts.URL+"/admin/filters/apply/delete", url.Values{
		"confirm": {token}, "pattern": {`(?i)^uptime`}, "apply_token": {stolen},
	})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	if after := messageCount(t, path); after != before {
		t.Fatal("a token from a different pattern was accepted")
	}
}

func TestApplyRefusesAnInvalidPattern(t *testing.T) {
	ts := newFilterServer(t)
	path := lastAdminDBPath
	before := messageCount(t, path)
	c, token := login(t, ts)
	body := postForm(t, c, ts.URL+"/admin/filters/apply/preview", url.Values{
		"confirm": {token}, "pattern": {"([unclosed"},
	})
	if !strings.Contains(body, "will not compile") {
		t.Fatalf("a broken pattern was not reported:\n%s", firstLines(body, 30))
	}
	if after := messageCount(t, path); after != before {
		t.Fatal("a broken pattern deleted rows")
	}
}

// ---- helpers ----

func postForm(t *testing.T, c *http.Client, u string, v url.Values) string {
	t.Helper()
	resp, err := c.PostForm(u, v)
	if err != nil {
		t.Fatalf("post %s: %v", u, err)
	}
	defer resp.Body.Close()
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return b.String()
}

func extractApplyToken(t *testing.T, body string) string {
	t.Helper()
	const marker = `name="apply_token" value="`
	i := strings.Index(body, marker)
	if i < 0 {
		return ""
	}
	rest := body[i+len(marker):]
	return rest[:strings.Index(rest, `"`)]
}
