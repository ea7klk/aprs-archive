// Package web serves the archive: search, statistics, bulletins, reports and
// message details, over an anonymous read-only connection.
package web

import (
	"context"
	"database/sql"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"aprs-archive/internal/aprs"
	"aprs-archive/internal/prefixes"
	"aprs-archive/internal/store"
)

// Config is everything the server needs from the environment.
type Config struct {
	Addr      string
	DBPath    string
	AppName   string
	Location  string
	Topic     string
	Version   string
	Collector string
	// RetentionDays is how long the archive keeps data; it is a property of
	// the collector and is shown to visitors.
	RetentionDays int
	// ReportMaxDays bounds a single report's window. It is deliberately
	// independent of RetentionDays: retention is how much history exists,
	// this is how much of it one query may scan. A 30-day window over a
	// year of traffic is a cheap index range on (kind, received_at); unbound,
	// the same query walks ten million rows.
	ReportMaxDays int
	// AdminPassword protects the callsign exception page. Empty disables it:
	// a write surface with no lock is worse than no surface.
	AdminPassword string
	// AdminDBPath is set only by the admin deployment, which opens the archive
	// read-write to maintain the exception list. The public deployment leaves
	// it empty and therefore cannot write at all.
	AdminDBPath string
	// PrefixSource is the callsign prefix dataset the admin page refreshes.
	PrefixSource      string
	CollectorStaleFor time.Duration
	// Now is injectable so tests can pin the clock.
	Now func() time.Time
}

// Server holds the database handle and parsed templates.
type Server struct {
	cfg Config
	db  *sql.DB
	tpl *template.Template
	// adminDB is the read-write handle held only by the admin deployment.
	adminDB *sql.DB
	// readOnly records which deployment this is. The public server is
	// read-only by construction and registers no admin route at all.
	readOnly bool
	// prefixFetcher keeps the callsign prefix list current for the admin page.
	prefixFetcher *prefixes.Fetcher
	admin         *admin
}

// New builds the server.
func New(cfg Config, db *sql.DB) (*Server, error) {
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC() }
	}
	if cfg.RetentionDays <= 0 {
		cfg.RetentionDays = 365
	}
	if cfg.CollectorStaleFor <= 0 {
		cfg.CollectorStaleFor = 15 * time.Minute
	}
	if cfg.ReportMaxDays <= 0 {
		cfg.ReportMaxDays = 92
	}
	// A report limit above the retention period would promise history the
	// archive no longer holds; the smaller of the two wins.
	if cfg.ReportMaxDays > cfg.RetentionDays {
		cfg.ReportMaxDays = cfg.RetentionDays
	}
	s := &Server{cfg: cfg, db: db, prefixFetcher: prefixes.NewFetcher(), readOnly: true}
	// Only the admin deployment supplies a write handle. Everything else about
	// this binary is identical, so the public server has no admin routes at all.
	if cfg.AdminDBPath != "" {
		adminDB, err := store.OpenAdminWriter(cfg.AdminDBPath)
		if err != nil {
			return nil, fmt.Errorf("open admin database: %w", err)
		}
		s.adminDB = adminDB
		s.readOnly = false
	}
	if cfg.PrefixSource != "" {
		s.prefixFetcher.URL = cfg.PrefixSource
	}
	s.admin = newAdmin(cfg, s.adminDB, s.prefixFetcher)
	tpl, err := parseTemplates()
	if err != nil {
		return nil, err
	}
	s.tpl = tpl
	return s, nil
}

// Handler returns the routed, middleware-wrapped handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", s.home)
	mux.HandleFunc("GET /messages", s.messages)
	mux.HandleFunc("GET /messages/{id}", s.messageDetail)
	mux.HandleFunc("GET /stations", s.stations)
	mux.HandleFunc("GET /stations/{callsign}", s.stationDetail)
	mux.HandleFunc("GET /bulletins", s.bulletins)
	mux.HandleFunc("GET /bulletins/{id}", s.bulletinDetail)
	mux.HandleFunc("GET /statistics", s.statistics)
	mux.HandleFunc("GET /reports", s.reportsForm)
	mux.HandleFunc("GET /reports/data", s.reportData)
	mux.HandleFunc("GET /reports/export.csv", s.reportCSV)
	mux.HandleFunc("GET /about", s.about)
	mux.HandleFunc("GET /healthz", s.health)

	// Admin routes exist only on the deployment that holds a write handle.
	// Registering them on the public server and refusing inside the handler
	// would leave a surface whose safety depended on every future edit
	// remembering the check; not registering them means the public server has
	// no admin code path at all.
	if !s.readOnly {
		mux.HandleFunc("GET /admin", s.adminPage)
		mux.HandleFunc("GET /admin/login", s.adminLogin)
		mux.HandleFunc("POST /admin/login", s.adminLoginPost)
		mux.HandleFunc("POST /admin/logout", s.adminLogout)
		mux.HandleFunc("POST /admin/exceptions", s.adminAddException)
		mux.HandleFunc("POST /admin/exceptions/delete/{callsign}", s.adminDeleteException)
		mux.HandleFunc("POST /admin/prefixes/refresh", s.adminRefreshPrefixes)
	}

	return securityHeaders(mux)
}

// viewData is what every template receives.
type viewData struct {
	AppName  string
	Location string
	Topic    string
	Version  string
	Path     string
	Nav      string
	Error    string
	Flash    string
	Data     any
	Now      time.Time
	// RetentionLabel and CollectorLabel are set by the page handler before
	// rendering; the layout shows them in the header meta block.
	RetentionLabel string
	CollectorLabel string
}

// navActive reports whether the given section is the current one, so the tab
// bar can mark it without each handler remembering the key it used.
func navActive(current, section string) bool {
	if section == "" {
		return current == ""
	}
	return current == section || strings.HasPrefix(current, section)
}

// sectionOf maps a request path to the top-level navigation key.
func sectionOf(path string) string {
	switch {
	case path == "/":
		return ""
	case strings.HasPrefix(path, "/messages"), strings.HasPrefix(path, "/stations"):
		return "explore"
	case strings.HasPrefix(path, "/bulletins"):
		return "bulletins"
	case strings.HasPrefix(path, "/statistics"):
		return "statistics"
	case strings.HasPrefix(path, "/reports"):
		return "reports"
	case strings.HasPrefix(path, "/about"):
		return "about"
	}
	return ""
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, name string, data any, errText string) {
	vd := viewData{
		AppName:  s.cfg.AppName,
		Location: s.cfg.Location,
		Topic:    s.cfg.Topic,
		Version:  s.cfg.Version,
		Path:     r.URL.Path,
		Nav:      sectionOf(r.URL.Path),
		Error:    errText,
		Data:     data,
		Now:      s.cfg.Now(),

		RetentionLabel: fmt.Sprintf("%d days", s.cfg.RetentionDays),
		CollectorLabel: s.collectorLabel(r.Context()),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The page is rendered into a buffer first: html/template reports an error
	// only at the end, so streaming straight to the response would emit half a
	// page and then be unable to change the status code.
	var buf strings.Builder
	if err := s.tpl.ExecuteTemplate(&buf, name, vd); err != nil {
		log.Printf("render %s: %v", name, err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(status)
	_, _ = w.Write([]byte(buf.String()))
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("X-Frame-Options", "DENY")
		// The archive renders third-party text. A restrictive CSP is the
		// second line of defence behind the sanitiser: no inline script runs
		// unless a nonce is present, and nothing loads from another origin.
		w.Header().Set("Content-Security-Policy",
			"default-src 'none'; style-src 'unsafe-inline'; img-src 'self' data:; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

// ---- handlers ----

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if err := s.db.PingContext(r.Context()); err != nil {
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusNotFound, "not_found", nil, "")
}

// homeData is the dashboard.
type homeData struct {
	Totals          store.Totals
	Collector       store.CollectorState
	Fresh           bool
	Series          []store.Bucket
	Kinds           []store.Bucket
	Senders         []store.Bucket
	Latest          []store.Message
	BulletinCats    []categoryRow
	RecentBulletins []store.Bulletin
	StaleFor        time.Duration
}

func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := s.cfg.Now()

	totals, err := store.GetTotals(ctx, s.db, now)
	if err != nil {
		log.Printf("home totals: %v", err)
		s.render(w, r, http.StatusInternalServerError, "error", nil, "The archive could not be read.")
		return
	}
	series, _ := store.Series(ctx, s.db, now.Add(-24*time.Hour), now, store.Hourly, "")
	kinds, _ := store.KindCounts(ctx, s.db, now.Add(-24*time.Hour), now)
	senders, _ := store.TopSenders(ctx, s.db, now.Add(-24*time.Hour), now, 8)
	latest, _ := store.SearchMessages(ctx, s.db, store.SearchFilter{Limit: 12, Since: now.Add(-7 * 24 * time.Hour)})
	bulletinPage, _ := store.Bulletins(ctx, s.db, store.BulletinFilter{Limit: 5})
	collector, _ := store.GetCollectorState(ctx, s.db)

	fresh := false
	if collector.Found && !collector.LastWriteAt.IsZero() {
		fresh = now.Sub(collector.LastWriteAt) <= s.cfg.CollectorStaleFor
	}

	s.render(w, r, http.StatusOK, "home", homeData{
		Totals:          totals,
		Collector:       collector,
		Fresh:           fresh,
		Series:          series,
		Kinds:           kinds,
		Senders:         senders,
		Latest:          latest.Rows,
		BulletinCats:    s.bulletinCategoryRows(ctx),
		RecentBulletins: bulletinPage.Rows,
		StaleFor:        s.cfg.CollectorStaleFor,
	}, "")
}

// categoryRow is a bulletin category with its count, for the section bar.
type categoryRow struct {
	ID    string
	Desc  string
	Count int64
}

func (s *Server) bulletinCategoryRows(ctx context.Context) []categoryRow {
	counts, err := store.BulletinCategoryCounts(ctx, s.db)
	if err != nil {
		log.Printf("bulletin counts: %v", err)
	}
	return buildCategoryRows(counts)
}

func buildCategoryRows(counts map[string]int64) []categoryRow {
	var rows []categoryRow
	var total int64
	for _, c := range aprs.BulletinCategories {
		rows = append(rows, categoryRow{ID: c.ID, Desc: c.Desc, Count: counts[c.ID]})
		total += counts[c.ID]
	}
	// A "all" row first, so the reader sees the whole set as one number.
	rows = append([]categoryRow{{ID: "", Desc: "Every bulletin in the archive", Count: total}}, rows...)
	return rows
}

func (s *Server) messages(w http.ResponseWriter, r *http.Request) {
	filter := parseSearchFilter(r)
	page, err := store.SearchMessages(r.Context(), s.db, filter)
	if err != nil {
		s.render(w, r, http.StatusBadRequest, "messages", &messagesData{Filter: filter}, err.Error())
		return
	}
	s.render(w, r, http.StatusOK, "messages", &messagesData{Filter: filter, Page: page, Query: r.URL.RawQuery}, "")
}

type messagesData struct {
	Filter store.SearchFilter
	Page   store.Page[store.Message]
	Query  string
}

func parseSearchFilter(r *http.Request) store.SearchFilter {
	q := r.URL.Query()
	f := store.SearchFilter{
		From:     strings.TrimSpace(q.Get("from")),
		To:       strings.TrimSpace(q.Get("to")),
		Text:     strings.TrimSpace(q.Get("text")),
		Kind:     filterArchivedKind(strings.TrimSpace(q.Get("kind"))),
		Category: strings.TrimSpace(q.Get("category")),
	}
	if v := q.Get("since"); v != "" {
		f.Since = parseDate(v)
	}
	if v := q.Get("until"); v != "" {
		// A date in the "until" field means "through the end of that day",
		// which is what a visitor picking 13 November expects to see.
		if d := parseDate(v); !d.IsZero() {
			f.Until = d.Add(24 * time.Hour)
		}
	}
	perPage, _ := strconv.Atoi(q.Get("per_page"))
	f.Limit = perPage
	pageNo, _ := strconv.Atoi(q.Get("page"))
	if pageNo < 1 {
		pageNo = 1
	}
	if f.Limit > 0 {
		f.Offset = (pageNo - 1) * f.Limit
	}
	return f
}

func (s *Server) messageDetail(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		s.notFound(w, r)
		return
	}
	m, err := store.GetMessage(r.Context(), s.db, id)
	if err == store.ErrNotFound {
		s.notFound(w, r)
		return
	}
	if err != nil {
		s.render(w, r, http.StatusInternalServerError, "error", nil, "The message could not be read.")
		return
	}
	conversation, _ := store.Conversation(r.Context(), s.db, m, 48*time.Hour, 50)
	s.render(w, r, http.StatusOK, "message_detail", &messageData{Message: m, Conversation: conversation, RetentionDays: s.cfg.RetentionDays}, "")
}

type messageData struct {
	Message       store.Message
	Conversation  []store.Message
	RetentionDays int
}

type stationData struct {
	Station       store.StationSummary
	Page          store.Page[store.Message]
	RetentionDays int
}

func (s *Server) stations(w http.ResponseWriter, r *http.Request) {
	now := s.cfg.Now()
	senders, _ := store.TopSenders(r.Context(), s.db, now.Add(-7*24*time.Hour), now, 100)
	s.render(w, r, http.StatusOK, "stations", &stationsData{Senders: senders}, "")
}

type stationsData struct {
	Senders []store.Bucket
}

func (s *Server) stationDetail(w http.ResponseWriter, r *http.Request) {
	callsign := strings.ToUpper(strings.TrimSpace(r.PathValue("callsign")))
	if callsign == "" || len(callsign) > 16 {
		s.notFound(w, r)
		return
	}
	now := s.cfg.Now()
	summary, err := store.GetStation(r.Context(), s.db, callsign, now)
	if err == store.ErrNotFound {
		s.notFound(w, r)
		return
	}
	if err != nil {
		s.render(w, r, http.StatusInternalServerError, "error", nil, "The station could not be read.")
		return
	}
	page, _ := store.SearchMessages(r.Context(), s.db, store.SearchFilter{
		From: callsign, Limit: 50, Since: now.Add(-365 * 24 * time.Hour),
	})
	s.render(w, r, http.StatusOK, "station_detail", &stationData{
		Station: summary, Page: page, RetentionDays: s.cfg.RetentionDays,
	}, "")
}

// ---- small helpers ----

// parseDate accepts the two shapes a date input and a URL can produce.
func parseDate(v string) time.Time {
	v = strings.TrimSpace(v)
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.UTC()
	}
	if t, err := time.Parse(store.DateLayout, v); err == nil {
		return t.UTC()
	}
	return time.Time{}
}

// parseID parses a positive row id from a path segment.
func parseID(v string) (int64, bool) {
	var id int64
	for _, r := range v {
		if r < '0' || r > '9' {
			return 0, false
		}
		id = id*10 + int64(r-'0')
		if id > 1<<62 {
			return 0, false
		}
	}
	return id, id > 0
}

// collectorLabel describes the collector's liveness for the header meta block.
// It reads the one health row rather than a cached value so a restarted or
// stuck collector is visible on the next page load.
func (s *Server) collectorLabel(ctx context.Context) string {
	state, err := store.GetCollectorState(ctx, s.db)
	if err != nil || !state.Found {
		return "unknown"
	}
	if state.LastWriteAt.IsZero() {
		return "waiting for first packet"
	}
	if s.cfg.Now().Sub(state.LastWriteAt) <= s.cfg.CollectorStaleFor {
		return "live · " + state.LastWriteAt.Format("15:04:05") + " UTC"
	}
	return "idle since " + state.LastWriteAt.Format("2006-01-02 15:04")
}
