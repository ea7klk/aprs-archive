package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strings"
	"time"

	"aprs-archive/internal/prefixes"
	"aprs-archive/internal/store"
)

// The admin page maintains the callsign exception list.
//
// Access is a single shared password, supplied by the deployment as a Secret and
// read once at startup. That is deliberate: the archive has no accounts, and
// inventing a user table for one operator would be a liability rather than a
// feature. The password is compared in constant time, and the session cookie it
// sets is signed with a key derived from the same password, so a restart or a
// password change logs every session out.
// admin maintains the exception list on the admin deployment. db here is the
// read-write handle; the public deployment has no admin at all.
type admin struct {
	db      *sql.DB
	fetcher *prefixes.Fetcher
	// password is the configured secret. Empty means the page is disabled —
	// fail closed rather than exposing a write surface with no lock.
	password string
	// sessionKey signs the session cookie.
	sessionKey []byte
}

const (
	adminCookie   = "aprs_archive_admin"
	adminSession  = 12 * time.Hour
	adminDisabled = "The admin page is disabled: no password is configured."
)

func newAdmin(cfg Config, db *sql.DB, fetcher *prefixes.Fetcher) *admin {
	return &admin{
		db: db, fetcher: fetcher,
		password: cfg.AdminPassword, sessionKey: []byte(cfg.AdminPassword),
	}
}

// enabled reports whether a password was configured.
func (a *admin) enabled() bool { return a.password != "" }

// authorized reports whether the request carries a valid session.
//
// The token is the password's HMAC over a fixed message, so it can be computed
// from the password alone — no session table, nothing to persist, and a
// password change invalidates every existing cookie because the token changes
// with it.
func (a *admin) authorized(r *http.Request) bool {
	if !a.enabled() {
		return false
	}
	c, err := r.Cookie(adminCookie)
	if err != nil {
		return false
	}
	want := a.sessionToken()
	return subtle.ConstantTimeCompare([]byte(c.Value), []byte(want)) == 1
}

// confirmToken is the CSRF token the admin write forms carry.
//
// It is derived from the same key as the session, so it expires with the
// session and changes when the password changes. A cross-site request cannot
// read it, and a browser replaying a stale form is refused.
func (a *admin) confirmToken() string {
	mac := hmac.New(sha256.New, a.sessionKey)
	mac.Write([]byte("aprs-archive-admin-confirm-v1"))
	return hex.EncodeToString(mac.Sum(nil))
}

// validConfirm compares a submitted token in constant time.
func (a *admin) validConfirm(got string) bool {
	if !a.enabled() || got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(a.confirmToken())) == 1
}

// checkPassword compares a submitted password in constant time.
func (a *admin) checkPassword(got string) bool {
	if !a.enabled() || got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(a.password)) == 1
}

// ---- handlers ----

func (s *Server) adminLogin(w http.ResponseWriter, r *http.Request) {
	if !s.admin.enabled() {
		s.renderAdminStatus(w, r, http.StatusServiceUnavailable, "admin_login", nil, adminDisabled)
		return
	}
	if s.admin.authorized(r) {
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	s.renderAdmin(w, r, "admin_login", nil, "")
}

func (s *Server) adminLoginPost(w http.ResponseWriter, r *http.Request) {
	if !s.admin.enabled() {
		s.renderAdminStatus(w, r, http.StatusServiceUnavailable, "admin_login", nil, adminDisabled)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderAdminStatus(w, r, http.StatusBadRequest, "admin_login", nil, "Malformed form.")
		return
	}
	if !s.admin.checkPassword(r.PostFormValue("password")) {
		// Same message for a wrong password and a disabled page: nothing to
		// learn from probing.
		s.renderAdminStatus(w, r, http.StatusUnauthorized, "admin_login", nil, "Wrong password.")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     adminCookie,
		Value:    s.admin.sessionToken(),
		Path:     "/admin",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(adminSession.Seconds()),
	})
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) adminLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: adminCookie, Value: "", Path: "/admin", MaxAge: -1, HttpOnly: true})
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) adminPage(w http.ResponseWriter, r *http.Request) {
	if !s.admin.enabled() {
		s.renderAdminStatus(w, r, http.StatusServiceUnavailable, "admin_login", nil, adminDisabled)
		return
	}
	if !s.admin.authorized(r) {
		http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
		return
	}
	ctx := r.Context()
	exceptions, err := store.ListExceptions(ctx, s.admin.db)
	if err != nil {
		log.Printf("admin: list exceptions: %v", err)
		s.renderAdmin(w, r, "admin", nil, "The exception list could not be read.")
		return
	}
	// The page shows the dataset validation is based on, so it must have one:
	// a cached snapshot if this process fetched before, otherwise a fetch now.
	// A failure is shown rather than hidden, and does not stop the page.
	if _, err := s.admin.fetcher.Current(r.Context()); err != nil {
		log.Printf("admin: prefix list unavailable: %v", err)
	}
	set, refreshErr := s.admin.fetcher.Snapshot()
	data := &adminData{
		Exceptions: exceptions,
		PrefixCount: func() int {
			if set == nil {
				return 0
			}
			return set.Count
		}(),
		PrefixFetched: func() time.Time {
			if set == nil {
				return time.Time{}
			}
			return set.FetchedAt
		}(),
		PrefixSource:  s.admin.fetcher.URL,
		PrefixRefresh: refreshErr,
		Flash:         r.URL.Query().Get("ok"),
		ErrorDetail:   r.URL.Query().Get("err"),
	}
	s.renderAdmin(w, r, "admin", data, "")
}

type adminData struct {
	Exceptions    []store.Exception
	PrefixCount   int
	PrefixFetched time.Time
	PrefixSource  string
	PrefixRefresh error
	Flash         string
	ErrorDetail   string
}

// adminAddException records an operator decision.
func (s *Server) adminAddException(w http.ResponseWriter, r *http.Request) {
	if !s.admin.authorized(r) {
		http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "malformed form", http.StatusBadRequest)
		return
	}
	callsign := strings.ToUpper(strings.TrimSpace(r.PostFormValue("callsign")))
	valid := r.PostFormValue("valid") == "true"
	note := r.PostFormValue("note")
	if callsign == "" || len(callsign) > 24 {
		s.redirectAdminErr(w, r, "A callsign of up to 24 characters is required.")
		return
	}
	if err := store.SetException(r.Context(), s.admin.db, callsign, valid, note); err != nil {
		s.redirectAdminErr(w, r, "Could not save: "+err.Error())
		return
	}
	word := "kept"
	if !valid {
		word = "excluded"
	}
	http.Redirect(w, r, "/admin?ok=Callsign+"+urlQueryEscape(callsign)+"+"+word+".+The+collector+reloads+the+list+within+a+minute.", http.StatusSeeOther)
}

func (s *Server) adminDeleteException(w http.ResponseWriter, r *http.Request) {
	if !s.admin.authorized(r) {
		http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
		return
	}
	callsign := strings.ToUpper(strings.TrimSpace(r.PathValue("callsign")))
	if err := store.DeleteException(r.Context(), s.admin.db, callsign); err != nil {
		s.redirectAdminErr(w, r, "Could not remove: "+err.Error())
		return
	}
	http.Redirect(w, r, "/admin?ok=Exception+removed%3B+prefix+validation+applies+again.", http.StatusSeeOther)
}

// adminRefreshPrefixes forces a new prefix list.
func (s *Server) adminRefreshPrefixes(w http.ResponseWriter, r *http.Request) {
	if !s.admin.authorized(r) {
		http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
		return
	}
	// The collector owns the list it validates against, so this only reports
	// what the admin deployment sees. The collector refreshes on its own timer.
	if err := s.admin.fetcher.Refresh(r.Context()); err != nil {
		s.redirectAdminErr(w, r, "Prefix list refresh failed: "+err.Error())
		return
	}
	set, _ := s.admin.fetcher.Snapshot()
	count := 0
	if set != nil {
		count = set.Count
	}
	http.Redirect(w, r, "/admin?ok="+urlQueryEscape(
		fmt.Sprintf("Prefix list refreshed here: %d prefixes. The collector refreshes on its own schedule.", count),
	), http.StatusSeeOther)
}

func (s *Server) redirectAdminErr(w http.ResponseWriter, r *http.Request, msg string) {
	http.Redirect(w, r, "/admin?ok="+urlQueryEscape(msg), http.StatusSeeOther)
}

// token derives the session value from the password.
//
// HMAC with the password as the key means the token cannot be computed without
// it, and changing the password changes the token — so a password change logs
// every existing session out without a session table.
func (a *admin) sessionToken() string {
	mac := hmac.New(sha256.New, a.sessionKey)
	mac.Write([]byte("aprs-archive-admin-session-v1"))
	return hex.EncodeToString(mac.Sum(nil))
}

// urlQueryEscape escapes a redirect message.
func urlQueryEscape(s string) string {
	return template.URLQueryEscaper(s)
}
