package admin

import (
	"log"
	"net/http"
	"strings"
	"time"

	"aprs-archive/internal/store"
)

// ---- exception list ----

type exceptionData struct {
	Exceptions    []store.Exception
	PrefixCount   int
	PrefixFetched time.Time
	Source        string
	PrefixRefresh string
	Flash         string
	ErrorDetail   string
}

func (s *Server) page(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	ctx := r.Context()
	exceptions, err := store.ListExceptions(ctx, s.db)
	if err != nil {
		log.Printf("admin: list exceptions: %v", err)
		s.render(w, r, "admin", nil, "The exception list could not be read.")
		return
	}
	if _, err := s.prefix.Current(ctx); err != nil {
		log.Printf("admin: prefix list unavailable: %v", err)
	}
	set, refreshErr := s.prefix.Snapshot()
	data := &exceptionData{
		Exceptions: exceptions,
		Source:     s.prefix.URL,
	}
	if set != nil {
		data.PrefixCount = set.Count
		data.PrefixFetched = set.FetchedAt
	}
	if refreshErr != nil {
		data.PrefixRefresh = refreshErr.Error()
	}
	data.Flash = r.URL.Query().Get("ok")
	data.ErrorDetail = r.URL.Query().Get("err")
	s.render(w, r, "admin", data, "")
}

func (s *Server) addException(w http.ResponseWriter, r *http.Request) {
	if !s.requirePost(w, r) {
		return
	}
	callsign := strings.ToUpper(strings.TrimSpace(r.PostFormValue("callsign")))
	valid := r.PostFormValue("valid") == "true"
	if callsign == "" || len(callsign) > 24 {
		s.result(w, r, "A callsign of up to 24 characters is required.", false)
		return
	}
	if err := store.SetException(r.Context(), s.db, callsign, valid, r.PostFormValue("note")); err != nil {
		s.result(w, r, "Could not save: "+err.Error(), false)
		return
	}
	word := "kept"
	if !valid {
		word = "excluded"
	}
	s.result(w, r, "Callsign "+callsign+" "+word+". The collector reloads the list within a minute.", true)
}

func (s *Server) deleteException(w http.ResponseWriter, r *http.Request) {
	if !s.requirePost(w, r) {
		return
	}
	callsign := strings.ToUpper(strings.TrimSpace(r.PathValue("callsign")))
	if err := store.DeleteException(r.Context(), s.db, callsign); err != nil {
		s.result(w, r, "Could not remove: "+err.Error(), false)
		return
	}
	s.result(w, r, "Exception for "+callsign+" removed; prefix validation applies again.", true)
}

func (s *Server) refreshPrefixes(w http.ResponseWriter, r *http.Request) {
	if !s.requirePost(w, r) {
		return
	}
	if err := s.prefix.Refresh(r.Context()); err != nil {
		s.result(w, r, "Prefix list refresh failed: "+err.Error(), false)
		return
	}
	set, _ := s.prefix.Snapshot()
	count := 0
	if set != nil {
		count = set.Count
	}
	s.result(w, r, "Prefix list refreshed here: "+itoa(count)+" prefixes. The collector refreshes on its own schedule.", true)
}

// ---- database ----

type databaseData struct {
	Info        store.Maintenance
	Counts      []store.TableCount
	Backups     []store.BackupFile
	Flash       string
	ErrorDetail string
	BackupDir   string
	Keep        int
	HumanBytes  func(int64) string
}

func (s *Server) databasePage(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	ctx := r.Context()
	info, err := store.Inspect(ctx, s.cfg.DBPath)
	if err != nil {
		log.Printf("admin: inspect: %v", err)
		s.render(w, r, "admin_database", &databaseData{Flash: r.URL.Query().Get("ok")},
			"The archive could not be inspected: "+err.Error())
		return
	}
	counts, _ := store.Counts(ctx, s.db)
	backups, _ := store.ListBackups(s.cfg.DBPath)
	s.render(w, r, "admin_database", &databaseData{
		Info:        info,
		Counts:      counts,
		Backups:     backups,
		Flash:       r.URL.Query().Get("ok"),
		ErrorDetail: r.URL.Query().Get("err"),
		BackupDir:   store.BackupDir(s.cfg.DBPath),
		Keep:        s.cfg.BackupKeep,
		HumanBytes:  humanBytes,
	}, "")
}

func (s *Server) checkpoint(w http.ResponseWriter, r *http.Request) {
	if !s.requirePost(w, r) {
		return
	}
	if err := store.Checkpoint(r.Context(), s.db); err != nil {
		s.result(w, r, "Checkpoint failed: "+err.Error(), false)
		return
	}
	s.result(w, r, "Checkpoint complete: the write-ahead log is folded into the archive.", true)
}

func (s *Server) vacuum(w http.ResponseWriter, r *http.Request) {
	if !s.requirePost(w, r) {
		return
	}
	reclaimed, err := store.Vacuum(r.Context(), s.db, s.cfg.DBPath)
	if err != nil {
		s.result(w, r, "Vacuum failed: "+err.Error(), false)
		return
	}
	if reclaimed == 0 {
		s.result(w, r, "Vacuum complete: nothing to reclaim.", true)
		return
	}
	s.result(w, r, "Vacuum complete: reclaimed "+humanBytes(reclaimed)+".", true)
}

func (s *Server) reindex(w http.ResponseWriter, r *http.Request) {
	if !s.requirePost(w, r) {
		return
	}
	if err := store.RebuildIndexes(r.Context(), s.db); err != nil {
		s.result(w, r, "Reindex failed: "+err.Error(), false)
		return
	}
	s.result(w, r, "Indexes rebuilt.", true)
}

func (s *Server) backup(w http.ResponseWriter, r *http.Request) {
	if !s.requirePost(w, r) {
		return
	}
	name, err := store.CreateBackup(r.Context(), s.db, s.cfg.DBPath, s.cfg.BackupKeep)
	if err != nil {
		s.result(w, r, "Backup failed: "+err.Error(), false)
		return
	}
	s.result(w, r, "Backup written: "+baseName(name)+".", true)
}

func (s *Server) downloadBackup(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	name := r.PathValue("name")
	if !validBackupName(name) {
		http.NotFound(w, r)
		return
	}
	f, err := openBackup(s.cfg.DBPath, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.WriteHeader(http.StatusOK)
	_, _ = f.WriteTo(w)
}

func (s *Server) deleteBackup(w http.ResponseWriter, r *http.Request) {
	if !s.requirePost(w, r) {
		return
	}
	name := r.PathValue("name")
	if err := store.DeleteBackup(s.cfg.DBPath, name); err != nil {
		s.result(w, r, "Could not delete backup: "+err.Error(), false)
		return
	}
	s.result(w, r, "Backup deleted: "+name, true)
}

func (s *Server) deleteBefore(w http.ResponseWriter, r *http.Request) {
	if !s.requirePost(w, r) {
		return
	}
	cutoff, ok := parseDay(r.PostFormValue("before"))
	if !ok {
		s.result(w, r, "Give a date to delete before.", false)
		return
	}
	if cutoff.After(s.cfg.Now()) {
		s.result(w, r, "That date is in the future; nothing was deleted.", false)
		return
	}
	messages, err := store.DeleteMessagesBefore(r.Context(), s.db, cutoff)
	if err != nil {
		s.result(w, r, "Delete failed: "+err.Error(), false)
		return
	}
	s.result(w, r, "Deleted "+itoa64(messages)+" messages older than "+cutoff.Format("2006-01-02")+".", true)
}

// ---- SQL console ----

type sqlData struct {
	Flash       string
	ErrorDetail string
	Result      *SQLResult
	Schema      []SchemaItem
	ReadOnly    bool
	Limit       int
	PreFill     string
}

func (s *Server) sqlPage(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	schema, err := s.Schema(r.Context())
	if err != nil {
		log.Printf("admin: schema: %v", err)
	}
	s.render(w, r, "admin_sql", &sqlData{
		Flash:       r.URL.Query().Get("ok"),
		ErrorDetail: r.URL.Query().Get("err"),
		Schema:      schema,
		Limit:       sqlRows,
		PreFill:     r.URL.Query().Get("q"),
	}, "")
}

func (s *Server) sqlRun(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.result(w, r, "Malformed form.", false)
		return
	}
	if !s.validConfirm(r.PostFormValue("confirm")) {
		s.result(w, r, "Confirmation expired. Run the statement again from the page.", false)
		return
	}
	statement := r.PostFormValue("statement")
	res := s.Exec(r.Context(), statement)
	schema, _ := s.Schema(r.Context())
	// The result is rendered directly rather than through a redirect: it is
	// the answer to the submission, and a redirect would throw it away.
	s.render(w, r, "admin_sql", &sqlData{
		Result:   &res,
		Schema:   schema,
		ReadOnly: readOnly(statement),
		Limit:    sqlRows,
		PreFill:  statement,
	}, "")
}

// sqlSchema renders one object's definition, for the schema browser's links.
func (s *Server) sqlSchema(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.result(w, r, "Malformed form.", false)
		return
	}
	name := r.PostFormValue("name")
	statement := "SELECT sql FROM sqlite_master WHERE name = '" + strings.ReplaceAll(name, "'", "''") + "'"
	res := s.Exec(r.Context(), statement)
	schema, _ := s.Schema(r.Context())
	s.render(w, r, "admin_sql", &sqlData{
		Result:   &res,
		Schema:   schema,
		ReadOnly: true,
		Limit:    sqlRows,
	}, "")
}
