package web

import (
	"crypto/tls"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aprs-archive/internal/store"
)

// newAdminServer builds a server with the admin page enabled over a real
// archive, returning a client that keeps cookies.
func newAdminServer(t *testing.T, password string) *httptest.Server {
	t.Helper()
	path := filepath.Join(t.TempDir(), "archive.db")
	db, err := store.OpenWriter(path)
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}
	lastAdminDBPath = path
	t.Cleanup(func() { db.Close() })
	reader, err := store.OpenReader(path)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	t.Cleanup(func() { reader.Close() })
	srv, err := New(Config{
		DBPath: path, AdminDBPath: path, AdminPassword: password,
		AppName: "APRS Archive", Version: "test", RetentionDays: 365,
		ReportMaxDays: 92, CollectorStaleFor: time.Minute,
		Now: func() time.Time { return time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC) },
	}, reader)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// TLS, because the session cookie is Secure by design and an HTTP test
	// server would silently drop it — the test would then pass for the wrong
	// reason or fail for one that does not exist in production.
	ts := httptest.NewTLSServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// rawClient does not follow redirects, so a test can assert on the login
// response itself rather than on the page the redirect lands on.
func rawClient() *http.Client {
	c := client()
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return c
}

// client follows redirects the way a browser does, keeping cookies.
func client() *http.Client {
	// A cookie jar is what makes this a browser: without one the client keeps
	// no cookies between requests and every "authenticated" step re-logs in.
	jar, _ := cookiejar.New(nil)
	return &http.Client{
		Jar:           jar,
		CheckRedirect: func(req *http.Request, via []*http.Request) error { return nil },
		Transport: &http.Transport{
			// The test server's certificate is self-signed; this is the only
			// reason to skip verification, and it is confined to the tests.
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
}

func TestAdminIsDisabledWithoutAPassword(t *testing.T) {
	ts := newAdminServer(t, "")
	resp, err := client().Get(ts.URL + "/admin")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	// Fail closed: no password means no page, not an open page.
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 when no password is configured", resp.StatusCode)
	}
}

func TestAdminRequiresThePassword(t *testing.T) {
	ts := newAdminServer(t, "correct horse battery staple")
	c := client()

	resp, err := c.Get(ts.URL + "/admin")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Request.URL.Path, "/admin/login") {
		// A redirect to the login page is the expected outcome; httptest
		// exposes the final path.
		if resp.Request.URL.Path != "/admin/login" {
			t.Fatalf("reached %s, want the login page", resp.Request.URL.Path)
		}
	}

	resp, err = c.PostForm(ts.URL+"/admin/login", url.Values{"password": {"wrong"}})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong password status = %d, want 401", resp.StatusCode)
	}

	resp, err = c.PostForm(ts.URL+"/admin/login", url.Values{"password": {"correct horse battery staple"}})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	resp, err = c.Get(ts.URL + "/admin")
	if err != nil {
		t.Fatalf("get after login: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authenticated /admin = %d, want 200", resp.StatusCode)
	}
	if got := resp.Request.URL.Path; got != "/admin" {
		t.Fatalf("landed on %s, want /admin", got)
	}
}

func TestAdminSessionCookieIsClosedByDefault(t *testing.T) {
	ts := newAdminServer(t, "pw")
	// Do not follow: the cookie and the status belong to the login response.
	resp, err := rawClient().PostForm(ts.URL+"/admin/login", url.Values{"password": {"pw"}})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login status = %d, want 303", resp.StatusCode)
	}
	var found *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == adminCookie {
			found = c
		}
	}
	if found == nil {
		t.Fatal("no session cookie was set")
	}
	if !found.HttpOnly || !found.Secure || found.SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie is not closed: %+v", found)
	}
	// The cookie must cover "/" as well: the admin host's front page is the bare
	// hostname, and a cookie scoped to /admin is not sent there.
	if found.Path != "/" {
		t.Fatalf("cookie path = %q, want / (the admin host's front page)", found.Path)
	}
}

func TestPasswordChangeInvalidatesSessions(t *testing.T) {
	a := &admin{password: "old", sessionKey: []byte("old")}
	oldToken := a.sessionToken()
	b := &admin{password: "new", sessionKey: []byte("new")}
	if oldToken == b.sessionToken() {
		t.Fatal("a password change left existing sessions valid")
	}
}

func TestAdminAddAndRemoveException(t *testing.T) {
	ts := newAdminServer(t, "pw")
	c := client()
	if resp, err := c.PostForm(ts.URL+"/admin/login", url.Values{"password": {"pw"}}); err != nil {
		t.Fatalf("login: %v", err)
	} else {
		resp.Body.Close()
	}
	resp, err := c.PostForm(ts.URL+"/admin/exceptions", url.Values{
		"callsign": {"aprsph"}, "valid": {"true"}, "note": {"wanted bot"},
	})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	resp.Body.Close()
	if resp.Request.URL.Path != "/admin" {
		t.Fatalf("after adding, landed on %s", resp.Request.URL.Path)
	}
	if got := exceptionCount(t); got != 1 {
		t.Fatalf("%d exceptions after adding, want 1", got)
	}
	body := readAll(t, ts.URL+"/admin", c)
	if !strings.Contains(strings.ToUpper(body), "APRSPH") {
		t.Fatal("the exception is not listed")
	}

	resp, err = c.PostForm(ts.URL+"/admin/exceptions/delete/APRSPH", url.Values{})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	resp.Body.Close()
	if got := exceptionCount(t); got != 0 {
		t.Fatalf("%d exceptions after removing, want 0", got)
	}
}

// exceptionCount reads the exception table through a fresh reader on the
// archive the last newAdminServer built. It is a package-level hook so the
// helper can stay small.
func exceptionCount(t *testing.T) int {
	t.Helper()
	reader, err := store.OpenReader(lastAdminDBPath)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer reader.Close()
	var n int
	if err := reader.QueryRow(`SELECT COUNT(*) FROM callsign_exceptions`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func TestAdminWritesAreRejectedWithoutASession(t *testing.T) {
	ts := newAdminServer(t, "pw")
	resp, err := client().PostForm(ts.URL+"/admin/exceptions", url.Values{
		"callsign": {"WXBOT"}, "valid": {"true"},
	})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.Request.URL.Path != "/admin/login" {
		t.Fatalf("an unauthenticated write reached %s", resp.Request.URL.Path)
	}
}

func readAll(t *testing.T, u string, c *http.Client) string {
	t.Helper()
	resp, err := c.Get(u)
	if err != nil {
		t.Fatalf("get: %v", err)
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

// lastAdminDBPath is where the most recent newAdminServer put its archive.
var lastAdminDBPath string

// TestPublicServerHasNoAdminRoutes is the invariant the split exists to keep.
//
// The public archive serves third-party traffic to anonymous visitors and opens
// the database read-only. The admin surface lives in a separate deployment, so
// the public server must not even register the routes — a handler that checks a
// flag is a surface; an unregistered path is not.
func TestPublicServerHasNoAdminRoutes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.db")
	db, err := store.OpenWriter(path)
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}
	defer db.Close()
	reader, err := store.OpenReader(path)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer reader.Close()
	// No AdminDBPath and a password set: exactly the public deployment's config.
	srv, err := New(Config{
		DBPath: path, AdminPassword: "pw", AppName: "APRS Archive", Version: "test",
		RetentionDays: 365, ReportMaxDays: 92, CollectorStaleFor: time.Minute,
	}, reader)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !srv.readOnly {
		t.Fatal("a server without an admin database was not marked read-only")
	}
	for _, p := range []string{"/admin", "/admin/login"} {
		if rec := get(t, srv.Handler(), p); rec.Code != http.StatusNotFound {
			t.Errorf("%s = %d on the public server, want 404 (no admin surface)", p, rec.Code)
		}
	}
}

// TestPublicServerCannotWrite pins the read-only guarantee at the SQLite layer,
// which is what actually prevents a future handler from writing.
func TestPublicServerCannotWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.db")
	db, err := store.OpenWriter(path)
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}
	defer db.Close()
	reader, err := store.OpenReader(path)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer reader.Close()
	if _, err := reader.Exec(
		`INSERT INTO callsign_exceptions (callsign, valid, note, updated_at) VALUES ('X', 1, '', 0)`,
	); err == nil {
		t.Fatal("the public server's database handle accepted a write")
	}
}

// TestAdminServerRegistersAdminRoutes is the other half.
func TestAdminServerRegistersAdminRoutes(t *testing.T) {
	ts := newAdminServer(t, "pw")
	c := client()
	resp, err := c.PostForm(ts.URL+"/admin/login", url.Values{"password": {"pw"}})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	resp.Body.Close()
	if resp.Request.URL.Path != "/admin" {
		t.Fatalf("after login landed on %s, want /admin", resp.Request.URL.Path)
	}
}
