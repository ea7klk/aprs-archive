package web

import (
	"context"
	"encoding/csv"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"aprs-archive/internal/aprs"
	"aprs-archive/internal/store"
)

// ---- bulletins ----

type bulletinsData struct {
	Page       store.Page[store.Bulletin]
	Categories []categoryRow
	Current    string
	Group      string
	Query      string
}

func (s *Server) bulletins(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	category := strings.TrimSpace(q.Get("category"))
	if category != "" && !aprs.IsBulletinCategory(category) {
		category = ""
	}
	perPage, _ := strconv.Atoi(q.Get("per_page"))
	if perPage <= 0 {
		perPage = 50
	}
	pageNo, _ := strconv.Atoi(q.Get("page"))
	if pageNo < 1 {
		pageNo = 1
	}
	filter := store.BulletinFilter{
		Category: category,
		Group:    strings.TrimSpace(q.Get("group")),
		From:     strings.TrimSpace(q.Get("from")),
		Text:     strings.TrimSpace(q.Get("text")),
		Limit:    perPage,
		Offset:   (pageNo - 1) * perPage,
	}
	page, err := store.Bulletins(r.Context(), s.db, filter)
	if err != nil {
		s.render(w, r, http.StatusInternalServerError, "error", nil, "The bulletins could not be read.")
		return
	}
	s.render(w, r, http.StatusOK, "bulletins", &bulletinsData{
		Page:       page,
		Categories: s.bulletinCategoryRows(r.Context()),
		Current:    category,
		Group:      filter.Group,
		Query:      r.URL.RawQuery,
	}, "")
}

func (s *Server) bulletinDetail(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		s.notFound(w, r)
		return
	}
	b, err := store.GetBulletin(r.Context(), s.db, id)
	if err == store.ErrNotFound {
		s.notFound(w, r)
		return
	}
	if err != nil {
		s.render(w, r, http.StatusInternalServerError, "error", nil, "The bulletin could not be read.")
		return
	}
	// Transmissions of this bulletin, so a reader can see how often it repeats
	// and from where, which is what "is this still current" means for a bulletin.
	transmissions, _ := store.SearchMessages(r.Context(), s.db, store.SearchFilter{
		Kind:  "bulletin",
		Text:  b.Text,
		From:  b.From,
		Limit: 50,
		Since: time.Time{},
	})
	s.render(w, r, http.StatusOK, "bulletin_detail", &bulletinData{Bulletin: b, Transmissions: transmissions}, "")
}

type bulletinData struct {
	Bulletin      store.Bulletin
	Transmissions store.Page[store.Message]
}

// ---- statistics ----

type statisticsData struct {
	Totals        store.Totals
	Collector     store.CollectorState
	Fresh         bool
	StaleFor      time.Duration
	RetentionDays int
	WindowDays    int
	Series        []store.Bucket
	Granularity   string
	Kinds         []store.Bucket
	Senders       []store.Bucket
	Recipients    []store.Bucket
	Pairs         []store.Pair
	PairsMax      int64
	CategoryRows  []categoryRow
	Groups        []store.Bucket
	DBBytes       int64
}

func (s *Server) statistics(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := s.cfg.Now()

	windowDays, _ := strconv.Atoi(r.URL.Query().Get("days"))
	if windowDays <= 0 || windowDays > s.cfg.RetentionDays {
		windowDays = 30
	}
	granularity := store.Daily
	granularityName := "day"
	if windowDays <= 3 {
		granularity = store.Hourly
		granularityName = "hour"
	}
	since := store.StartOfDay(now).AddDate(0, 0, -(windowDays - 1))

	totals, err := store.GetTotals(ctx, s.db, now)
	if err != nil {
		s.render(w, r, http.StatusInternalServerError, "error", nil, "The archive could not be read.")
		return
	}
	series, _ := store.Series(ctx, s.db, since, now, granularity, "")
	kinds, _ := store.KindCounts(ctx, s.db, since, now)
	senders, _ := store.TopSenders(ctx, s.db, since, now, 15)
	recipients, _ := store.TopRecipients(ctx, s.db, since, now, 15)
	// Pairs are the headline statistic and the most expensive query, so they get
	// a wider limit: a ranking of conversations is more useful than a ranking of
	// stations, and the aggregation already visits every row in the window.
	pairs, err := store.TopPairs(ctx, s.db, since, now, 20)
	if err != nil {
		log.Printf("statistics pairs: %v", err)
	}
	groups, _ := store.BulletinGroups(ctx, s.db, 20)
	collector, _ := store.GetCollectorState(ctx, s.db)
	size, _ := store.DatabaseSize(s.cfg.DBPath)

	fresh := false
	if collector.Found && !collector.LastWriteAt.IsZero() {
		fresh = now.Sub(collector.LastWriteAt) <= s.cfg.CollectorStaleFor
	}

	s.render(w, r, http.StatusOK, "statistics", &statisticsData{
		Totals:        totals,
		Collector:     collector,
		Fresh:         fresh,
		StaleFor:      s.cfg.CollectorStaleFor,
		RetentionDays: s.cfg.RetentionDays,
		WindowDays:    windowDays,
		Series:        series,
		Granularity:   granularityName,
		Kinds:         kinds,
		Senders:       senders,
		Recipients:    recipients,
		Pairs:         pairs,
		PairsMax:      maxPairCount(pairs),
		CategoryRows:  s.bulletinCategoryRows(ctx),
		Groups:        groups,
		DBBytes:       size,
	}, "")
}

// ---- reports ----

// ReportSpec is what a report is asked for, and what its CSV exports.
type ReportSpec struct {
	// Truncated is set when the requested window was longer than the reporting
	// limit and the start was moved forward. The page says so rather than
	// presenting a shorter range as the one that was asked for.
	Truncated bool
	Kind      string
	Station   string
	Since     time.Time
	Until     time.Time
	GroupBy   string
	Category  string
}

// ReportRowLimit bounds the rows any report returns.
//
// A report is a summary, not an export: the CSV exists for the full set, and a
// grouping with a million distinct values (stations over a year) would either
// time out or produce a page nobody can read. The limit is applied in SQL so
// the cost is bounded too, not just the output.
const ReportRowLimit = 100

// ValidGroupings are the report groupings offered.
var ValidGroupings = []struct {
	ID    string
	Label string
}{
	{"hour", "Hour"},
	{"day", "Day"},
	{"station", "Station"},
	{"kind", "Packet kind"},
	{"category", "Bulletin category"},
}

func parseReportSpec(r *http.Request) ReportSpec {
	q := r.URL.Query()
	spec := ReportSpec{
		Kind:     filterArchivedKind(strings.TrimSpace(q.Get("kind"))),
		Station:  strings.ToUpper(strings.TrimSpace(q.Get("station"))),
		GroupBy:  strings.TrimSpace(q.Get("group_by")),
		Category: strings.TrimSpace(q.Get("category")),
	}
	spec.Since = parseDate(q.Get("since"))
	// The date picker marks days, not instants: "until 13 November" must include
	// 13 November, so the bound is the start of the following day. Without this
	// the report asked for rows before midnight on the day the archive began and
	// returned nothing, while the same range on the search page returned data —
	// the two used different rules for the same field.
	if d := parseDate(q.Get("until")); !d.IsZero() {
		spec.Until = d.AddDate(0, 0, 1)
	}
	return spec
}

// window normalises an empty or inverted range to something answerable.
func (spec *ReportSpec) window(now time.Time, maxDays int) {
	if spec.Until.IsZero() {
		spec.Until = now
	}
	if spec.Since.IsZero() {
		defaultDays := 30
		if maxDays > 0 && defaultDays > maxDays {
			defaultDays = maxDays
		}
		spec.Since = spec.Until.AddDate(0, 0, -defaultDays)
	}
	if !spec.Until.After(spec.Since) {
		spec.Until = spec.Since.AddDate(0, 0, 1)
	}
	if maxDays > 0 {
		// Until is exclusive, so a window of exactly maxDays spans maxDays days
		// and ends the instant it begins. Anything longer is trimmed from the
		// start: the end of the range is the part the visitor asked for last
		// and is the part the report must keep.
		limit := time.Duration(maxDays) * 24 * time.Hour
		if spec.Until.Sub(spec.Since) > limit {
			spec.Since = spec.Until.Add(-limit)
			spec.Truncated = true
		}
	}
	if !spec.ValidGrouping() {
		spec.GroupBy = "day"
	}
}

// query renders the spec as a URL query, so a link to the CSV export carries
// exactly the parameters the visible report used.
func (spec ReportSpec) query() string {
	v := url.Values{}
	v.Set("since", spec.Since.Format(store.DateLayout))
	// Until is exclusive internally; the form and the export both speak in days,
	// so it is written back as the last included day.
	v.Set("until", spec.Until.AddDate(0, 0, -1).Format(store.DateLayout))
	v.Set("group_by", spec.GroupBy)
	if spec.Kind != "" {
		v.Set("kind", spec.Kind)
	}
	if spec.Station != "" {
		v.Set("station", spec.Station)
	}
	if spec.Category != "" {
		v.Set("category", spec.Category)
	}
	return v.Encode()
}

func (spec ReportSpec) ValidGrouping() bool {
	for _, g := range ValidGroupings {
		if g.ID == spec.GroupBy {
			return true
		}
	}
	return false
}

// ReportResult is a computed report.
type ReportResult struct {
	Spec    ReportSpec
	Rows    []store.Bucket
	Total   int64
	Columns []string
	// Truncated is set when the grouping had more rows than ReportRowLimit.
	Truncated bool
}

// Run computes the report. The grouping is chosen from a fixed list and turned
// into SQL here, never taken from the request as raw SQL fragments.
func (s *Server) Run(ctx context.Context, spec ReportSpec) (ReportResult, error) {
	var where []string
	var args []any
	if spec.Kind != "" {
		where = append(where, "kind = ?")
		args = append(args, spec.Kind)
	}
	if spec.Station != "" {
		where = append(where, "(from_callsign = ? OR to_callsign = ?)")
		args = append(args, spec.Station, spec.Station)
	}
	if spec.Category != "" && aprs.IsBulletinCategory(spec.Category) {
		where = append(where, "category = ?")
		args = append(args, spec.Category)
	}
	where = append(where, "received_at >= ?", "received_at < ?")
	args = append(args, spec.Since.Unix(), spec.Until.Unix())

	clause := strings.Join(where, " AND ")
	var query string
	switch spec.GroupBy {
	case "hour", "day":
		// Group in SQL by integer division so SQLite can use the
		// (received_at, id) index for the range instead of scanning the table.
		step := int64(3600)
		if spec.GroupBy == "day" {
			step = 86400
		}
		query = fmt.Sprintf(`SELECT received_at / %d AS bucket, COUNT(*) AS n, MIN(received_at) AS first_at
			FROM messages WHERE %s GROUP BY bucket ORDER BY bucket LIMIT %d`, step, clause, ReportRowLimit)
	case "station":
		query = fmt.Sprintf(`SELECT from_callsign, COUNT(*) AS n, MIN(received_at) AS first_at
			FROM messages WHERE %s GROUP BY from_callsign ORDER BY n DESC, from_callsign ASC LIMIT %d`, clause, ReportRowLimit)
	case "kind":
		query = fmt.Sprintf(`SELECT kind, COUNT(*) AS n, MIN(received_at) AS first_at
			FROM messages WHERE %s GROUP BY kind ORDER BY n DESC LIMIT %d`, clause, ReportRowLimit)
	case "category":
		query = fmt.Sprintf(`SELECT CASE WHEN category = '' THEN '(none)' ELSE category END, COUNT(*) AS n, MIN(received_at) AS first_at
			FROM messages WHERE %s GROUP BY category ORDER BY n DESC LIMIT %d`, clause, ReportRowLimit)
	default:
		return ReportResult{}, fmt.Errorf("unknown grouping %q", spec.GroupBy)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return ReportResult{}, err
	}
	defer rows.Close()

	result := ReportResult{Spec: spec}
	step := int64(0)
	if spec.GroupBy == "hour" {
		step = 3600
	}
	if spec.GroupBy == "day" {
		step = 86400
	}
	for rows.Next() {
		var label any
		var n int64
		var firstAt int64
		if err := rows.Scan(&label, &n, &firstAt); err != nil {
			return result, err
		}
		text := ""
		switch v := label.(type) {
		case int64:
			text = time.Unix(v*step, 0).UTC().Format(dayOrHour(spec.GroupBy))
		case string:
			text = v
		case []byte:
			text = string(v)
		}
		result.Rows = append(result.Rows, store.Bucket{Label: text, Count: n})
		result.Total += n
	}
	if err := rows.Err(); err != nil {
		return result, err
	}
	result.Columns = []string{groupingLabel(spec.GroupBy), "Packets", "Share"}
	result.Truncated = len(result.Rows) >= ReportRowLimit
	return result, nil
}

func dayOrHour(groupBy string) string {
	if groupBy == "hour" {
		return "2006-01-02 15:00"
	}
	return "2006-01-02"
}

func groupingLabel(groupBy string) string {
	for _, g := range ValidGroupings {
		if g.ID == groupBy {
			return g.Label
		}
	}
	return groupBy
}

func (s *Server) reportsForm(w http.ResponseWriter, r *http.Request) {
	spec := parseReportSpec(r)
	spec.window(s.cfg.Now(), s.cfg.ReportMaxDays)
	exportQuery := spec.query()

	if r.URL.Query().Get("run") == "" {
		s.render(w, r, http.StatusOK, "reports", &reportsData{
			Spec: spec, Groupings: ValidGroupings, Categories: aprs.BulletinCategories,
			RetentionDays: s.cfg.RetentionDays, ReportMaxDays: s.cfg.ReportMaxDays, Ran: false, ExportQuery: exportQuery,
		}, "")
		return
	}
	result, err := s.Run(r.Context(), spec)
	if err != nil {
		log.Printf("report: %v", err)
		s.render(w, r, http.StatusOK, "reports", &reportsData{
			Spec: spec, Groupings: ValidGroupings, Categories: aprs.BulletinCategories,
			RetentionDays: s.cfg.RetentionDays, ReportMaxDays: s.cfg.ReportMaxDays, Ran: true, ExportQuery: exportQuery,
			Error: "The report could not be computed.",
		}, "")
		return
	}
	s.render(w, r, http.StatusOK, "reports", &reportsData{
		Spec: spec, Groupings: ValidGroupings, Categories: aprs.BulletinCategories,
		RetentionDays: s.cfg.RetentionDays, ReportMaxDays: s.cfg.ReportMaxDays, Ran: true, Result: &result, ExportQuery: exportQuery,
	}, "")
}

type reportsData struct {
	Spec          ReportSpec
	Groupings     []struct{ ID, Label string }
	Categories    []aprs.BulletinCategory
	RetentionDays int
	ReportMaxDays int
	Ran           bool
	Result        *ReportResult
	Error         string
	// ExportQuery is the query string for the CSV link, without the `run`
	// flag: the export endpoint computes the report from the same parameters.
	ExportQuery string
}

// reportCSV streams the report as CSV.
//
// A CSV is an attachment: a browser must download it rather than render it, and
// a spreadsheet must open it as UTF-8 so accented callsigns survive. Both are
// headers, not hopes.
// filterArchivedKind drops a kind the archive does not store.
//
// Acks and rejects are valid APRS packets but not archived traffic. A visitor
// can still ask for one — by typing the query string, or from a link saved
// before the change — and the honest answer is "that kind is not archived",
// not an empty report that looks like a search failure.
func filterArchivedKind(kind string) string {
	switch kind {
	case "ack", "rej":
		return ""
	}
	return kind
}

func (s *Server) reportCSV(w http.ResponseWriter, r *http.Request) {
	spec := parseReportSpec(r)
	spec.window(s.cfg.Now(), s.cfg.ReportMaxDays)
	result, err := s.Run(r.Context(), spec)
	if err != nil {
		http.Error(w, "report failed", http.StatusInternalServerError)
		return
	}
	filename := fmt.Sprintf("aprs-archive-%s-%s.csv", spec.GroupBy, spec.Since.Format("2006-01-02"))
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	cw := csv.NewWriter(w)
	defer cw.Flush()
	_ = cw.Write([]string{"label", "packets", "share"})
	for _, row := range result.Rows {
		share := 0.0
		if result.Total > 0 {
			share = float64(row.Count) / float64(result.Total) * 100
		}
		_ = cw.Write([]string{row.Label, strconv.FormatInt(row.Count, 10), fmt.Sprintf("%.2f%%", share)})
	}
}

func (s *Server) reportData(w http.ResponseWriter, r *http.Request) { s.reportsForm(w, r) }

// ---- about ----

func (s *Server) about(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, "about_page", &aboutData{
		RetentionDays: s.cfg.RetentionDays,
		Collector:     s.cfg.Collector,
	}, "")
}

type aboutData struct {
	RetentionDays int
	Collector     string
}
