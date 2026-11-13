// Package admin is the archive's administration application: the callsign
// exception list, database maintenance, and a SQL console.
//
// It is a separate package and a separate binary from the public archive, not a
// role of it. The two serve different hostnames, hold different database
// handles, and must not share a route table: when they did, an admin route and
// a public route lived in the same mux and the admin hostname answered the
// public pages. A package boundary makes that mistake a compile error rather
// than a 404 somebody has to notice.
package admin

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strings"
	"time"

	"aprs-archive/internal/prefixes"
)

//go:embed templates/*.html templates/style.css
var assets embed.FS

// Config is what the admin deployment is told by its environment.
type Config struct {
	Addr     string
	DBPath   string
	AppName  string
	Version  string
	Location string
	// Password protects every page. Empty disables the application: a write
	// surface with no lock is worse than no surface.
	Password string
	// PrefixSource is the dataset the exception page explains.
	PrefixSource string
	// BackupKeep is how many snapshots to retain.
	BackupKeep int
	// RetentionDays is the collector's retention, shown for context.
	RetentionDays int
	// Now is injectable for tests.
	Now func() time.Time
}

// Server is the admin application.
type Server struct {
	cfg    Config
	db     *sql.DB
	tpl    *template.Template
	prefix *prefixes.Fetcher
	// sessionKey signs the session and confirmation tokens. It is the password,
	// so changing the password ends every existing session.
	sessionKey []byte
}

// New builds the application against a read-write archive handle.
func New(cfg Config, db *sql.DB) (*Server, error) {
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC() }
	}
	if cfg.BackupKeep <= 0 {
		cfg.BackupKeep = 7
	}
	if cfg.Location == "" {
		cfg.Location = "https://aprs-admin.ea7klk.es"
	}
	s := &Server{
		cfg:        cfg,
		db:         db,
		prefix:     prefixes.NewFetcher(),
		sessionKey: []byte(cfg.Password),
	}
	if cfg.PrefixSource != "" {
		s.prefix.URL = cfg.PrefixSource
	}
	tpl, err := parseTemplates()
	if err != nil {
		return nil, err
	}
	s.tpl = tpl
	return s, nil
}

// Enabled reports whether a password was configured.
func (s *Server) Enabled() bool { return s.cfg.Password != "" }

// Handler returns the admin application's routes.
//
// Everything here is admin: there is no public page in this binary to conflict
// with, which is the point of splitting them.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /{$}", s.root)

	mux.HandleFunc("GET /admin", s.page)
	mux.HandleFunc("GET /admin/login", s.loginForm)
	mux.HandleFunc("POST /admin/login", s.loginPost)
	mux.HandleFunc("POST /admin/logout", s.logout)
	mux.HandleFunc("POST /admin/exceptions", s.addException)
	mux.HandleFunc("POST /admin/exceptions/delete/{callsign}", s.deleteException)
	mux.HandleFunc("POST /admin/prefixes/refresh", s.refreshPrefixes)

	mux.HandleFunc("GET /admin/database", s.databasePage)
	mux.HandleFunc("POST /admin/database/checkpoint", s.checkpoint)
	mux.HandleFunc("POST /admin/database/vacuum", s.vacuum)
	mux.HandleFunc("POST /admin/database/reindex", s.reindex)
	mux.HandleFunc("POST /admin/database/backup", s.backup)
	mux.HandleFunc("GET /admin/database/backup/{name}", s.downloadBackup)
	mux.HandleFunc("POST /admin/database/backup/{name}/delete", s.deleteBackup)
	mux.HandleFunc("POST /admin/database/delete-before", s.deleteBefore)

	mux.HandleFunc("GET /admin/sql", s.sqlPage)
	mux.HandleFunc("POST /admin/sql/query", s.sqlRun)
	mux.HandleFunc("POST /admin/sql/schema", s.sqlSchema)

	mux.HandleFunc("GET /admin/{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
	})
	mux.HandleFunc("/", s.notFound)

	return securityHeaders(mux)
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if err := s.db.PingContext(r.Context()); err != nil {
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// root is the bare hostname: the application's front page, not a 404.
func (s *Server) root(w http.ResponseWriter, r *http.Request) {
	if !s.Enabled() {
		s.renderStatus(w, r, http.StatusServiceUnavailable, "admin_login", nil, disabledMessage)
		return
	}
	if !s.authorized(r) {
		http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// notFound explains what this host is.
func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(`<!doctype html><html lang="en"><head><meta charset="utf-8">` +
		`<meta name="robots" content="noindex"><title>Not here</title>` +
		`<style>body{font-family:ui-monospace,monospace;background:#080a18;color:#f2f0ff;padding:40px;line-height:1.6}` +
		`a{color:#4fd8ff}h1{font-size:20px;letter-spacing:.1em;text-transform:uppercase}</style></head><body>` +
		`<h1>Not part of the admin application</h1>` +
		`<p>This host serves administration only: exceptions, maintenance and SQL.</p>` +
		`<p><a href="/admin">Admin home</a> · the public archive is at <a href="https://aprs.ea7klk.es/">aprs.ea7klk.es</a></p>` +
		`</body></html>`))
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy",
			"default-src 'none'; style-src 'unsafe-inline'; img-src 'self' data:; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
		w.Header().Set("X-Robots-Tag", "noindex, nofollow")
		next.ServeHTTP(w, r)
	})
}

const disabledMessage = "The admin application is disabled: no password is configured."

// view is what the admin templates receive.
type view struct {
	AppName       string
	Location      string
	Version       string
	Path          string
	AdminNav      bool
	SQLNav        bool
	Error         string
	Data          any
	Now           time.Time
	Confirm       string
	RetentionDays int
}

// render draws a page with the layout the admin templates share.
func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, data any, errText string) {
	s.renderStatus(w, r, http.StatusOK, name, data, errText)
}

func (s *Server) renderStatus(w http.ResponseWriter, r *http.Request, status int, name string, data any, errText string) {
	vd := view{
		AppName:       s.cfg.AppName,
		Location:      s.cfg.Location,
		Version:       s.cfg.Version,
		Path:          r.URL.Path,
		AdminNav:      r.URL.Path == "/admin/database",
		SQLNav:        r.URL.Path == "/admin/sql",
		Error:         errText,
		Data:          data,
		Now:           s.cfg.Now(),
		Confirm:       s.confirmToken(),
		RetentionDays: s.cfg.RetentionDays,
	}
	var buf strings.Builder
	if err := s.tpl.ExecuteTemplate(&buf, name, vd); err != nil {
		log.Printf("render %s: %v", name, err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(buf.String()))
}

// ---- authentication ----

// sessionToken derives the session cookie from the password.
func (s *Server) sessionToken() string { return derive(s.sessionKey, "session") }

// confirmToken derives the CSRF token from the same key, so it expires with
// the session and changes when the password does.
func (s *Server) confirmToken() string { return derive(s.sessionKey, "confirm") }

func derive(key []byte, purpose string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("aprs-archive-admin-" + purpose + "-v2"))
	return hex.EncodeToString(mac.Sum(nil))
}

// authorized reports whether the request carries a valid session.
func (s *Server) authorized(r *http.Request) bool {
	if !s.Enabled() {
		return false
	}
	c, err := r.Cookie(cookieName)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(c.Value), []byte(s.sessionToken())) == 1
}

// checkPassword compares in constant time.
func (s *Server) checkPassword(got string) bool {
	if !s.Enabled() || got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.Password)) == 1
}

// validConfirm compares the CSRF token.
func (s *Server) validConfirm(got string) bool {
	if !s.Enabled() || got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.confirmToken())) == 1
}

const cookieName = "aprs_archive_admin"

func (s *Server) loginForm(w http.ResponseWriter, r *http.Request) {
	if !s.Enabled() {
		s.renderStatus(w, r, http.StatusServiceUnavailable, "admin_login", nil, disabledMessage)
		return
	}
	if s.authorized(r) {
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	s.render(w, r, "admin_login", nil, "")
}

func (s *Server) loginPost(w http.ResponseWriter, r *http.Request) {
	if !s.Enabled() {
		s.renderStatus(w, r, http.StatusServiceUnavailable, "admin_login", nil, disabledMessage)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderStatus(w, r, http.StatusBadRequest, "admin_login", nil, "Malformed form.")
		return
	}
	if !s.checkPassword(r.PostFormValue("password")) {
		s.renderStatus(w, r, http.StatusUnauthorized, "admin_login", nil, "Wrong password.")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    s.sessionToken(),
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   12 * 60 * 60,
	})
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: true})
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// requireAdmin sends an unauthenticated caller to the login page.
func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if !s.Enabled() {
		s.renderStatus(w, r, http.StatusServiceUnavailable, "admin_login", nil, disabledMessage)
		return false
	}
	if !s.authorized(r) {
		http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
		return false
	}
	return true
}

// requirePost is requireAdmin plus the CSRF token every write form carries.
func (s *Server) requirePost(w http.ResponseWriter, r *http.Request) bool {
	if !s.requireAdmin(w, r) {
		return false
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	if err := r.ParseForm(); err != nil {
		s.result(w, r, "Malformed form.", false)
		return false
	}
	if !s.validConfirm(r.PostFormValue("confirm")) {
		s.result(w, r, "Confirmation expired. Run the action again from the page.", false)
		return false
	}
	return true
}

// result redirects back to the page an action came from, with the outcome.
func (s *Server) result(w http.ResponseWriter, r *http.Request, msg string, ok bool) {
	page := "/admin"
	switch r.PostFormValue("return_to") {
	case "database":
		page = "/admin/database"
	case "sql":
		page = "/admin/sql"
	}
	prefix := "ok="
	if !ok {
		prefix = "err="
	}
	http.Redirect(w, r, page+"?"+prefix+template.URLQueryEscaper(msg), http.StatusSeeOther)
}

// humanBytes formats a byte count for a maintenance message.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
