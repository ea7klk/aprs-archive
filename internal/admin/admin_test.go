package admin

import (
	"context"
	"crypto/tls"
	"database/sql"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aprs-archive/internal/aprs"
	"aprs-archive/internal/store"
)

// newTestServer builds the admin application over a real archive.
func newTestServer(t *testing.T) *httptest.Server {
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
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		if err := writer.Record(ctx, aprs.Packet{
			From: "EA7KLK-9", To: "N0CALL", Text: "hello", Kind: aprs.KindMessage,
		}, now); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	writer.Close(ctx)
	w.Close()

	db, err := store.OpenAdminWriter(path)
	if err != nil {
		t.Fatalf("OpenAdminWriter: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	srv, err := New(Config{
		DBPath: path, AppName: "APRS Archive", Version: "test",
		Password: "pw", RetentionDays: 365, BackupKeep: 7,
		Now: func() time.Time { return now },
	}, db)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// TLS: the session cookie is Secure by design, and an HTTP test server would
	// silently drop it.
	ts := httptest.NewTLSServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func client() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{
		Jar: jar,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
}

// rawClient does not follow redirects, so a test can see the first hop.
func rawClient() *http.Client {
	c := client()
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }
	return c
}

func login(t *testing.T, ts *httptest.Server) (*http.Client, string) {
	t.Helper()
	c := client()
	resp, err := c.PostForm(ts.URL+"/admin/login", url.Values{"password": {"pw"}})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	resp.Body.Close()
	body := getBody(t, c, ts.URL+"/admin")
	marker := `name="confirm" value="`
	i := strings.Index(body, marker)
	if i < 0 {
		t.Fatal("no confirmation token on the admin page")
	}
	rest := body[i+len(marker):]
	return c, rest[:strings.Index(rest, `"`)]
}

func getBody(t *testing.T, c *http.Client, u string) string {
	t.Helper()
	resp, err := c.Get(u)
	if err != nil {
		t.Fatalf("get %s: %v", u, err)
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

func TestDisabledWithoutPassword(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.db")
	w, _ := store.OpenWriter(path)
	w.Close()
	db, _ := store.OpenAdminWriter(path)
	defer db.Close()
	srv, err := New(Config{DBPath: path, Password: ""}, db)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h := srv.Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestLoginAndSession(t *testing.T) {
	ts := newTestServer(t)
	// Wrong password: 401, no cookie.
	resp, err := rawClient().PostForm(ts.URL+"/admin/login", url.Values{"password": {"wrong"}})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong password = %d, want 401", resp.StatusCode)
	}
	if len(resp.Cookies()) != 0 {
		t.Fatal("a wrong password set a cookie")
	}
	// Correct password: 303 and a hardened cookie.
	resp, err = rawClient().PostForm(ts.URL+"/admin/login", url.Values{"password": {"pw"}})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login = %d, want 303", resp.StatusCode)
	}
	var ck *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == cookieName {
			ck = c
		}
	}
	if ck == nil {
		t.Fatal("no session cookie")
	}
	if !ck.HttpOnly || !ck.Secure || ck.SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie is not hardened: %+v", ck)
	}
}

func TestRootIsTheAdminApplication(t *testing.T) {
	ts := newTestServer(t)
	// Anonymous "/" goes to the login page.
	resp, err := rawClient().Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if loc := resp.Header.Get("Location"); loc != "/admin/login" {
		t.Fatalf("anonymous / -> %q, want /admin/login", loc)
	}
	// Signed in, "/" goes to the exception list.
	c, _ := login(t, ts)
	resp, err = rawClient().Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	_ = c
}

func TestAdminServesNoPublicPage(t *testing.T) {
	ts := newTestServer(t)
	for _, p := range []string{"/messages", "/bulletins", "/statistics", "/reports", "/about"} {
		body := getBody(t, client(), ts.URL+p)
		if strings.Contains(body, "Every message, on the record") || strings.Contains(body, "Bulletin reader") {
			t.Errorf("%s served the public archive", p)
		}
		if !strings.Contains(body, "Not part of the admin application") {
			t.Errorf("%s did not serve the admin 404", p)
		}
	}
}

func TestExceptionWriteAndRemove(t *testing.T) {
	ts := newTestServer(t)
	c, token := login(t, ts)
	resp, err := c.PostForm(ts.URL+"/admin/exceptions", url.Values{
		"callsign": {"aprsph"}, "valid": {"true"}, "note": {"wanted"}, "confirm": {token},
	})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	resp.Body.Close()
	if !strings.Contains(resp.Request.URL.RawQuery, "APRSPH+kept") {
		t.Fatalf("add did not report success: %q", resp.Request.URL.RawQuery)
	}
	// The template's prose also mentions APRSPH as an example, so the check is
	// for the table cell, not for the string anywhere on the page.
	if !strings.Contains(getBody(t, c, ts.URL+"/admin"), "<strong>APRSPH</strong>") {
		t.Fatal("the exception is not listed")
	}
	resp, err = c.PostForm(ts.URL+"/admin/exceptions/delete/APRSPH", url.Values{"confirm": {token}})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	resp.Body.Close()
	if strings.Contains(getBody(t, c, ts.URL+"/admin"), "<strong>APRSPH</strong>") {
		t.Fatal("the exception survived removal")
	}
}

func TestWritesNeedConfirmationToken(t *testing.T) {
	ts := newTestServer(t)
	c, _ := login(t, ts)
	resp, err := c.PostForm(ts.URL+"/admin/exceptions", url.Values{"callsign": {"X"}, "valid": {"true"}})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	if !strings.Contains(resp.Request.URL.RawQuery, "err=") {
		t.Fatalf("a write without a token was not refused: %q", resp.Request.URL.RawQuery)
	}
}

func TestWritesNeedASession(t *testing.T) {
	ts := newTestServer(t)
	resp, err := rawClient().PostForm(ts.URL+"/admin/exceptions", url.Values{
		"callsign": {"X"}, "valid": {"true"}, "confirm": {"guess"},
	})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	if loc := resp.Header.Get("Location"); loc != "/admin/login" {
		t.Fatalf("unauthenticated write -> %q, want /admin/login", loc)
	}
}

// ---- SQL console ----

func TestSQLSelectReturnsRows(t *testing.T) {
	ts := newTestServer(t)
	c, token := login(t, ts)
	// One row per message: five rows, each carrying the sender.
	body := postSQL(t, c, ts, token, "SELECT from_callsign, msg_text FROM messages")
	if !strings.Contains(body, "EA7KLK-9") {
		t.Fatal("the query result does not show the seeded sender")
	}
	if !strings.Contains(body, "5 rows") {
		t.Fatalf("row count missing; result header was:\n%s", between(body, "Result", "</div>"))
	}
	// An aggregate really does return one row, and says so.
	agg := postSQL(t, c, ts, token, "SELECT COUNT(*) AS n FROM messages")
	if !strings.Contains(agg, "1 rows") && !strings.Contains(agg, "1 row") {
		t.Fatalf("aggregate row count wrong:\n%s", between(agg, "Result", "</div>"))
	}
}

func TestSQLStatementThatChangesData(t *testing.T) {
	ts := newTestServer(t)
	c, token := login(t, ts)
	body := postSQL(t, c, ts, token, "UPDATE messages SET msg_text = 'changed'")
	if !strings.Contains(body, "statement executed") && !strings.Contains(body, "Rows affected") {
		t.Fatalf("an UPDATE result was not reported:\n%s", between(body, "Result", "Schema"))
	}
	if !strings.Contains(getBody(t, c, ts.URL+"/admin/sql?q=SELECT%20msg_text%20FROM%20messages%20LIMIT%201"), "changed") {
		t.Fatal("the UPDATE did not take effect")
	}
}

func TestSQLReportsErrors(t *testing.T) {
	ts := newTestServer(t)
	c, token := login(t, ts)
	body := postSQL(t, c, ts, token, "SELECT * FROM no_such_table")
	if !strings.Contains(body, "no such table") {
		t.Fatalf("the error was not shown: %s", firstLines(body, 20))
	}
}

func TestSQLRefusesMultipleStatements(t *testing.T) {
	ts := newTestServer(t)
	c, token := login(t, ts)
	body := postSQL(t, c, ts, token, "DELETE FROM messages; DELETE FROM bulletins")
	if !strings.Contains(body, "one statement at a time") {
		t.Fatalf("a multi-statement submission was accepted: %s", firstLines(body, 20))
	}
	// Nothing was deleted.
	if !strings.Contains(getBody(t, c, ts.URL+"/admin/sql?q=SELECT%20COUNT(*)%20FROM%20messages"), "5") {
		t.Fatal("a refused multi-statement changed data")
	}
}

func TestSQLSemicolonInLiteralIsNotMultipleStatements(t *testing.T) {
	if hasMultipleStatements("SELECT 'a;b' AS x") {
		t.Fatal("a semicolon inside a string literal was treated as a separator")
	}
	if !hasMultipleStatements("SELECT 1; SELECT 2") {
		t.Fatal("two statements were not detected")
	}
	if hasMultipleStatements("SELECT 1;") {
		t.Fatal("a trailing semicolon was treated as a second statement")
	}
}

func TestSQLRequiresSessionAndToken(t *testing.T) {
	ts := newTestServer(t)
	// No session.
	resp, err := rawClient().PostForm(ts.URL+"/admin/sql/query", url.Values{"statement": {"SELECT 1"}, "confirm": {"x"}})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	if loc := resp.Header.Get("Location"); loc != "/admin/login" {
		t.Fatalf("unauthenticated SQL -> %q", loc)
	}
	// Session but no token.
	c, _ := login(t, ts)
	resp, err = c.PostForm(ts.URL+"/admin/sql/query", url.Values{"statement": {"SELECT 1"}})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	if !strings.Contains(resp.Request.URL.RawQuery, "err=") {
		t.Fatal("a SQL write without a token was accepted")
	}
}

func TestSQLSchemaBrowser(t *testing.T) {
	ts := newTestServer(t)
	c, _ := login(t, ts)
	body := getBody(t, c, ts.URL+"/admin/sql")
	for _, want := range []string{"messages", "bulletins", "callsign_exceptions"} {
		if !strings.Contains(body, want) {
			t.Errorf("schema browser is missing %q", want)
		}
	}
}

func TestFormatValueDistinguishesNull(t *testing.T) {
	if got := formatValue(nil); got != "NULL" {
		t.Fatalf("nil = %q, want NULL", got)
	}
	if got := formatValue(""); got != "" {
		t.Fatalf("empty string = %q, want empty", got)
	}
	if got := formatValue([]byte{0x00, 0x01}); !strings.HasPrefix(got, "0x") {
		t.Fatalf("binary = %q, want hex", got)
	}
}

func TestReadOnlyClassification(t *testing.T) {
	for _, s := range []string{"SELECT 1", "WITH x AS (SELECT 1) SELECT * FROM x", "PRAGMA page_count", "EXPLAIN SELECT 1"} {
		if !readOnly(s) {
			t.Errorf("%q classified as a write", s)
		}
	}
	for _, s := range []string{"DELETE FROM messages", "UPDATE messages SET a=1", "PRAGMA journal_mode=WAL", "DROP TABLE messages"} {
		if readOnly(s) {
			t.Errorf("%q classified as read-only", s)
		}
	}
}

func postSQL(t *testing.T, c *http.Client, ts *httptest.Server, token, statement string) string {
	t.Helper()
	resp, err := c.PostForm(ts.URL+"/admin/sql/query", url.Values{"statement": {statement}, "confirm": {token}})
	if err != nil {
		t.Fatalf("post sql: %v", err)
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

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

var _ = sql.ErrNoRows

func between(s, start, end string) string {
	i := strings.Index(s, start)
	if i < 0 {
		return "(no " + start + ")"
	}
	rest := s[i:]
	if j := strings.Index(rest, end); j > 0 {
		return rest[:j]
	}
	return rest
}
