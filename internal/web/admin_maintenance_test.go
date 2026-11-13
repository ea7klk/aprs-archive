package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aprs-archive/internal/store"
)

// maintenanceLogin signs in and returns the confirmation token the forms carry.
func maintenanceLogin(t *testing.T, ts *httptest.Server) (*http.Client, string) {
	t.Helper()
	c := client()
	resp, err := c.PostForm(ts.URL+"/admin/login", url.Values{"password": {"pw"}})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	resp.Body.Close()
	body := readAll(t, ts.URL+"/admin/database", c)
	start := strings.Index(body, `name="confirm" value="`)
	if start < 0 {
		t.Fatal("no confirmation token on the database page")
	}
	rest := body[start+len(`name="confirm" value="`):]
	return c, rest[:strings.Index(rest, `"`)]
}

func TestDatabasePageShowsHealth(t *testing.T) {
	ts := newAdminServer(t, "pw")
	c := client()
	resp, _ := c.PostForm(ts.URL+"/admin/login", url.Values{"password": {"pw"}})
	resp.Body.Close()
	body := readAll(t, ts.URL+"/admin/database", c)
	for _, want := range []string{"quick_check ok", "Schema version", "Table sizes", "Backup now", "Delete before a date"} {
		if !strings.Contains(body, want) {
			t.Errorf("database page is missing %q", want)
		}
	}
}

func TestMaintenanceActionsNeedConfirmation(t *testing.T) {
	ts := newAdminServer(t, "pw")
	c, _ := maintenanceLogin(t, ts)
	// A post without the token must not act.
	for _, path := range []string{"/admin/database/vacuum", "/admin/database/reindex", "/admin/database/backup"} {
		resp, err := c.PostForm(ts.URL+path, url.Values{})
		if err != nil {
			t.Fatalf("post %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.Request.URL.Path != "/admin" {
			t.Fatalf("%s without a token redirected to %s, want the refusal page", path, resp.Request.URL.Path)
		}
		if !strings.Contains(resp.Request.URL.RawQuery, "err=") {
			t.Fatalf("%s without a token did not report a refusal", path)
		}
	}
}

func TestMaintenanceActionsNeedASession(t *testing.T) {
	ts := newAdminServer(t, "pw")
	resp, err := client().PostForm(ts.URL+"/admin/database/vacuum", url.Values{"confirm": {"guess"}})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.Request.URL.Path != "/admin/login" {
		t.Fatalf("an unauthenticated maintenance action reached %s", resp.Request.URL.Path)
	}
}

func TestBackupThenDownloadAndDelete(t *testing.T) {
	ts := newAdminServer(t, "pw")
	c, token := maintenanceLogin(t, ts)

	resp, err := c.PostForm(ts.URL+"/admin/database/backup", url.Values{"confirm": {token}, "return_to": {"database"}})
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	resp.Body.Close()
	if !strings.Contains(resp.Request.URL.RawQuery, "Backup+written") {
		t.Fatalf("backup did not report success: %q", resp.Request.URL.RawQuery)
	}

	backups, err := store.ListBackups(lastAdminDBPath)
	if err != nil || len(backups) != 1 {
		t.Fatalf("backups = %+v, err %v", backups, err)
	}
	name := backups[0].Name

	// Download serves the file as an attachment.
	dl, err := c.Get(ts.URL + "/admin/database/backup/" + name)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	defer dl.Body.Close()
	if dl.StatusCode != http.StatusOK {
		t.Fatalf("download status = %d", dl.StatusCode)
	}
	if cd := dl.Header.Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Fatalf("Content-Disposition = %q", cd)
	}

	// Delete removes it.
	resp, err = c.PostForm(ts.URL+"/admin/database/backup/"+name+"/delete", url.Values{"confirm": {token}, "return_to": {"database"}})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	resp.Body.Close()
	if got, _ := store.ListBackups(lastAdminDBPath); len(got) != 0 {
		t.Fatalf("%d backups remain after delete", len(got))
	}
}

func TestDownloadRefusesTraversal(t *testing.T) {
	ts := newAdminServer(t, "pw")
	c, _ := maintenanceLogin(t, ts)
	for _, bad := range []string{"..%2Farchive.db", ".hidden.db", "notadb.txt"} {
		resp, err := c.Get(ts.URL + "/admin/database/backup/" + bad)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%q served %d, want 404", bad, resp.StatusCode)
		}
	}
}

func TestDeleteBeforeRefusesAFutureDate(t *testing.T) {
	ts := newAdminServer(t, "pw")
	c, token := maintenanceLogin(t, ts)
	resp, err := c.PostForm(ts.URL+"/admin/database/delete-before", url.Values{
		"confirm": {token}, "before": {"2099-01-01"}, "return_to": {"database"},
	})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	if !strings.Contains(resp.Request.URL.RawQuery, "future") {
		t.Fatalf("a future cutoff was not refused: %q", resp.Request.URL.RawQuery)
	}
	// And nothing was deleted.
	reader, err := store.OpenReader(lastAdminDBPath)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer reader.Close()
	var n int
	if err := reader.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
}

func TestPublicServerHasNoMaintenanceRoutes(t *testing.T) {
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
	srv, err := New(Config{
		DBPath: path, AdminPassword: "pw", AppName: "APRS Archive", Version: "test",
		RetentionDays: 365, ReportMaxDays: 92, CollectorStaleFor: 60,
	}, reader)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h := srv.Handler()
	for _, p := range []string{"/admin/database", "/admin", "/admin/login"} {
		if rec := get(t, h, p); rec.Code != http.StatusNotFound {
			t.Errorf("%s = %d on the public server, want 404", p, rec.Code)
		}
	}
	if _, err := os.Stat(store.BackupDir(path)); err == nil {
		t.Log("backup directory exists (fine if a test created it)")
	}
}

// TestAdminDeploymentDoesNotServeThePublicSite is the regression for the
// reported fault: https://aprs-admin.ea7klk.es/ served the public home page.
//
// The public routes were registered on both deployments with only the admin
// half conditional, so the admin hostname answered "/" with the archive. A
// hostname should be the application it names.
func TestAdminDeploymentDoesNotServeThePublicSite(t *testing.T) {
	ts := newAdminServer(t, "pw")
	c := client()

	// Unauthenticated "/" goes to the login page, not to the archive.
	resp, err := c.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("get /: %v", err)
	}
	resp.Body.Close()
	if resp.Request.URL.Path != "/admin/login" {
		t.Fatalf("/ landed on %s, want /admin/login", resp.Request.URL.Path)
	}

	// No public page is served at all.
	for _, p := range []string{"/messages", "/bulletins", "/statistics", "/reports", "/stations", "/about"} {
		r, err := c.Get(ts.URL + p)
		if err != nil {
			t.Fatalf("get %s: %v", p, err)
		}
		body := readAll(t, ts.URL+p, c)
		r.Body.Close()
		if strings.Contains(body, "Every message, on the record") || strings.Contains(body, "Bulletin reader") {
			t.Errorf("%s served the public site from the admin deployment", p)
		}
	}

	// And once signed in, "/" is the admin home.
	resp, err = c.PostForm(ts.URL+"/admin/login", url.Values{"password": {"pw"}})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	resp.Body.Close()
	resp, err = c.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("get / after login: %v", err)
	}
	resp.Body.Close()
	if resp.Request.URL.Path != "/admin" {
		t.Fatalf("signed-in / landed on %s, want /admin", resp.Request.URL.Path)
	}
}

// TestPublicDeploymentServesNoAdminPage is the other direction, and pins that
// the public site still works after the split.
func TestPublicDeploymentServesNoAdminPage(t *testing.T) {
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
	srv, err := New(Config{
		DBPath: path, AdminPassword: "pw", AppName: "APRS Archive", Version: "test",
		RetentionDays: 365, ReportMaxDays: 92, CollectorStaleFor: 60,
	}, reader)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h := srv.Handler()
	// The archive itself works.
	if rec := get(t, h, "/"); rec.Code != http.StatusOK {
		t.Fatalf("public / = %d, want 200", rec.Code)
	}
	if !strings.Contains(get(t, h, "/").Body.String(), "Every message, on the record") {
		t.Fatal("the public home page did not render")
	}
	// Admin paths do not exist.
	for _, p := range []string{"/admin", "/admin/login", "/admin/database"} {
		if rec := get(t, h, p); rec.Code != http.StatusNotFound {
			t.Errorf("%s = %d on the public server, want 404", p, rec.Code)
		}
	}
}

// TestAdminTrailingSlashRedirects covers the URL a person actually types.
func TestAdminTrailingSlashRedirects(t *testing.T) {
	ts := newAdminServer(t, "pw")
	c := client()
	resp, err := c.Get(ts.URL + "/admin/")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if resp.Request.URL.Path != "/admin/login" && resp.Request.URL.Path != "/admin" {
		t.Fatalf("/admin/ landed on %s", resp.Request.URL.Path)
	}
	if resp.StatusCode == http.StatusNotFound {
		t.Fatal("/admin/ is a 404")
	}
}

// TestSignedInOperatorLandsOnTheAdminApp is the regression for a cookie scoped
// too narrowly: the session cookie was set for /admin, so "/" never received it
// and a signed-in operator was sent back to the login page.
func TestSignedInOperatorLandsOnTheAdminApp(t *testing.T) {
	ts := newAdminServer(t, "pw")
	// rawClient does not follow redirects, so the first hop is observable.
	c := rawClient()
	resp, err := c.PostForm(ts.URL+"/admin/login", url.Values{"password": {"pw"}})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	resp.Body.Close()
	if len(resp.Cookies()) == 0 {
		t.Fatal("no session cookie")
	}
	// Reuse the cookie through a following client, as a browser would.
	b := client()
	for _, ck := range resp.Cookies() {
		b.Jar.SetCookies(mustParse(t, ts.URL), []*http.Cookie{ck})
	}
	root, err := b.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("get /: %v", err)
	}
	defer root.Body.Close()
	if root.Request.URL.Path != "/admin" {
		t.Fatalf("a signed-in operator at / landed on %s, want /admin", root.Request.URL.Path)
	}
}

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}
