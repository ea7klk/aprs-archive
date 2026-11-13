package web

import (
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"aprs-archive/internal/store"
)

// Database maintenance handlers.
//
// Every one of these writes, needs an authenticated admin session, and requires
// a confirmation token that the page renders: a CSRF guard, and also a guard
// against the browser replaying a form submission on a refresh. The destructive
// ones (vacuum, reindex, delete-before) state plainly what they did, including
// when they did nothing.
//
// None of them touch the public server. The routes are registered only on the
// deployment that holds a write handle.

// adminDB is the page that shows health and offers the operations.
func (s *Server) adminDatabase(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	ctx := r.Context()
	path := s.cfg.DBPath
	info, err := store.Inspect(ctx, path)
	if err != nil {
		log.Printf("admin: inspect: %v", err)
		s.renderAdmin(w, r, "admin_database", &databaseData{Flash: r.URL.Query().Get("ok")},
			"The archive could not be inspected: "+err.Error())
		return
	}
	counts, _ := store.Counts(ctx, s.adminDB)
	backups, _ := store.ListBackups(path)
	s.renderAdmin(w, r, "admin_database", &databaseData{
		Info:        info,
		Counts:      counts,
		Backups:     backups,
		Flash:       r.URL.Query().Get("ok"),
		ErrorDetail: r.URL.Query().Get("err"),
		BackupDir:   store.BackupDir(path),
		Keep:        s.cfg.BackupKeep,
		Now:         s.cfg.Now(),
	}, "")
}

type databaseData struct {
	Info    store.Maintenance
	Counts  []store.TableCount
	Backups []store.BackupFile
	Flash   string
	// ErrorDetail is the outcome of a failed action, carried in the redirect.
	ErrorDetail string
	BackupDir   string
	Keep        int
	Now         time.Time
}

// adminCheckpoint forces the WAL back into the database file.
func (s *Server) adminCheckpoint(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPost(w, r) {
		return
	}
	if err := store.Checkpoint(r.Context(), s.adminDB); err != nil {
		s.adminResult(w, r, "Checkpoint failed: "+err.Error(), false)
		return
	}
	s.adminResult(w, r, "Checkpoint complete: the write-ahead log is folded into the archive.", true)
}

// adminVacuum compacts the file.
func (s *Server) adminVacuum(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPost(w, r) {
		return
	}
	reclaimed, err := store.Vacuum(r.Context(), s.adminDB, s.cfg.DBPath)
	if err != nil {
		s.adminResult(w, r, "Vacuum failed: "+err.Error(), false)
		return
	}
	if reclaimed == 0 {
		s.adminResult(w, r, "Vacuum complete: nothing to reclaim.", true)
		return
	}
	s.adminResult(w, r, "Vacuum complete: reclaimed "+humanBytes(reclaimed)+".", true)
}

// adminReindex rebuilds every index.
func (s *Server) adminReindex(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPost(w, r) {
		return
	}
	if err := store.RebuildIndexes(r.Context(), s.adminDB); err != nil {
		s.adminResult(w, r, "Reindex failed: "+err.Error(), false)
		return
	}
	s.adminResult(w, r, "Indexes rebuilt.", true)
}

// adminBackup writes a consistent snapshot.
func (s *Server) adminBackup(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPost(w, r) {
		return
	}
	name, err := store.CreateBackup(r.Context(), s.adminDB, s.cfg.DBPath, s.cfg.BackupKeep)
	if err != nil {
		s.adminResult(w, r, "Backup failed: "+err.Error(), false)
		return
	}
	s.adminResult(w, r, "Backup written: "+filepath.Base(name)+" (keeping the newest "+strconv.Itoa(s.cfg.BackupKeep)+").", true)
}

// adminDownloadBackup serves a backup file to the operator.
//
// Backups live inside the claim, so the only way to get one off the cluster is
// through this handler. The name is validated by the store, not trusted from the
// path.
func (s *Server) adminDownloadBackup(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	name := r.PathValue("name")
	if name == "" || strings.ContainsAny(name, `/\`) || !strings.HasSuffix(name, ".db") || strings.HasPrefix(name, ".") {
		http.NotFound(w, r)
		return
	}
	full := filepath.Join(store.BackupDir(s.cfg.DBPath), name)
	f, err := os.Open(full)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	http.ServeContent(w, r, name, time.Time{}, f)
}

func (s *Server) adminDeleteBackup(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPost(w, r) {
		return
	}
	name := r.PathValue("name")
	if err := store.DeleteBackup(s.cfg.DBPath, name); err != nil {
		s.adminResult(w, r, "Could not delete backup: "+err.Error(), false)
		return
	}
	s.adminResult(w, r, "Backup deleted: "+name, true)
}

// adminDeleteBefore applies a retention cut immediately.
func (s *Server) adminDeleteBefore(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPost(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.adminResult(w, r, "Malformed form.", false)
		return
	}
	cutoff := parseDate(r.PostFormValue("before"))
	if cutoff.IsZero() {
		s.adminResult(w, r, "Give a date to delete before.", false)
		return
	}
	// Refuse a cutoff in the future: that would delete the whole archive, and
	// it is never what an operator means.
	if cutoff.After(s.cfg.Now()) {
		s.adminResult(w, r, "That date is in the future; nothing was deleted.", false)
		return
	}
	messages, err := store.DeleteMessagesBefore(r.Context(), s.adminDB, cutoff)
	if err != nil {
		s.adminResult(w, r, "Delete failed: "+err.Error(), false)
		return
	}
	word := "messages"
	if messages == 1 {
		word = "message"
	}
	s.adminResult(w, r, "Deleted "+strconv.FormatInt(messages, 10)+" "+word+" older than "+cutoff.Format("2006-01-02")+".", true)
}

// ---- shared guards ----

// requireAdmin sends the caller to the login page unless the session is valid.
func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if !s.admin.enabled() {
		s.render(w, r, http.StatusServiceUnavailable, "admin_login", nil, adminDisabled)
		return false
	}
	if !s.admin.authorized(r) {
		http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
		return false
	}
	return true
}

// requireAdminPost additionally demands the confirmation token every write
// form carries. A cross-site POST cannot know it, and a browser cannot replay a
// stale one because it is derived from the session.
func (s *Server) requireAdminPost(w http.ResponseWriter, r *http.Request) bool {
	if !s.requireAdmin(w, r) {
		return false
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	if err := r.ParseForm(); err != nil {
		s.adminResult(w, r, "Malformed form.", false)
		return false
	}
	if !s.admin.validConfirm(r.PostFormValue("confirm")) {
		s.adminResult(w, r, "Confirmation expired. Run the action again from the page.", false)
		return false
	}
	return true
}

// adminResult redirects back to whichever admin page the action came from, with
// the outcome in the query string.
func (s *Server) adminResult(w http.ResponseWriter, r *http.Request, msg string, ok bool) {
	page := r.PostFormValue("return_to")
	switch page {
	case "database":
		page = "/admin/database"
	default:
		page = "/admin"
	}
	prefix := "ok="
	if !ok {
		prefix = "err="
	}
	http.Redirect(w, r, page+"?"+prefix+url.QueryEscape(msg), http.StatusSeeOther)
}

// renderAdmin renders an admin page and fills in the confirmation token the
// forms need.
func (s *Server) renderAdmin(w http.ResponseWriter, r *http.Request, name string, data any, errText string) {
	s.renderAdminStatus(w, r, http.StatusOK, name, data, errText)
}

// renderAdminStatus is renderAdmin with an explicit status, which the login
// page needs: a wrong password is a 401 and a disabled page is a 503, not a
// successful page that happens to mention the problem.
func (s *Server) renderAdminStatus(w http.ResponseWriter, r *http.Request, status int, name string, data any, errText string) {
	// The admin pages use their own view model, not the public one: they have
	// no visitor-facing navigation and must not link back into the public site
	// as if it were the same application.
	vd := adminView{
		AppName:       s.cfg.AppName,
		Path:          r.URL.Path,
		AdminNav:      strings.HasPrefix(r.URL.Path, "/admin/database"),
		Error:         errText,
		Data:          data,
		Now:           s.cfg.Now(),
		Confirm:       s.admin.confirmToken(),
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

// adminView is what the admin templates receive.
type adminView struct {
	AppName       string
	Path          string
	AdminNav      bool
	Error         string
	Data          any
	Now           time.Time
	Confirm       string
	RetentionDays int
}

// humanBytes formats a byte count for a maintenance message.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return strconv.FormatFloat(float64(n)/float64(div), 'f', 1, 64) + " " + string("KMGTPE"[exp]) + "iB"
}
