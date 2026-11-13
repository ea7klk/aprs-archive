package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Time formats used by the archive.
//
// SQLite has no date type: a column called "date" is whatever the caller passed.
// Every timestamp the archive stores is whole seconds since the Unix epoch,
// UTC, and every timestamp it renders is formatted here so the two processes and
// the templates cannot disagree.
const (
	DateLayout = "2006-01-02"
	TimeLayout = "15:04:05"
)

// ErrNotFound is returned when a requested row does not exist.
var ErrNotFound = errors.New("not found")

// Message is one archived message-format packet.
type Message struct {
	ID        int64
	At        time.Time
	From      string
	To        string
	Text      string
	Kind      string
	MessageID string
	Category  string
}

// Bulletin is a deduplicated bulletin.
type Bulletin struct {
	ID        int64
	FirstSeen time.Time
	LastSeen  time.Time
	SeenCount int64
	From      string
	Slot      int
	Group     string
	Category  string
	Text      string
}

// SearchFilter is the query form for the message search.
type SearchFilter struct {
	// From and To are substring matches, case-insensitive, on the callsign.
	From string
	To   string
	// Text is a substring match on the body.
	Text string
	// Kind restricts to message, bulletin, ack or rej. Empty means any.
	Kind string
	// Category restricts bulletins. Empty means any.
	Category string
	// Since and Until bound received_at, inclusive-exclusive. Zero means open.
	Since time.Time
	Until time.Time
	// Limit and Offset page the result.
	Limit  int
	Offset int
}

// PerPageSizes are the page sizes the search form offers.
var PerPageSizes = []int{25, 50, 100, 200}

// Page is one page of results plus what it takes to page through them.
type Page[T any] struct {
	Rows    []T
	Total   int64
	Page    int
	PerPage int
	Pages   int
}

// Normalize caps the search filter at values the archive can answer.
func (f *SearchFilter) Normalize() error {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 50
	}
	if f.Offset < 0 {
		f.Offset = 0
	}
	if f.Since.IsZero() && f.Until.IsZero() && f.From == "" && f.To == "" && f.Text == "" {
		// An unbounded, unfiltered search on a ten-million-row table is a full
		// scan for no reason: default the window to the last day so the first
		// page a visitor lands on is cheap.
		f.Since = time.Now().UTC().Add(-24 * time.Hour)
	}
	if !f.Since.IsZero() && !f.Until.IsZero() && f.Until.Before(f.Since) {
		return fmt.Errorf("the end of the window is before its start")
	}
	return nil
}

// where builds the shared predicate and its arguments. It is one function so
// the count query and the page query cannot drift apart.
func (f SearchFilter) where() (string, []any) {
	var clauses []string
	var args []any
	if f.From != "" {
		clauses = append(clauses, `from_callsign LIKE ? ESCAPE '\'`)
		args = append(args, likeContains(f.From))
	}
	if f.To != "" {
		clauses = append(clauses, `to_callsign LIKE ? ESCAPE '\'`)
		args = append(args, likeContains(f.To))
	}
	if f.Text != "" {
		clauses = append(clauses, `msg_text LIKE ? ESCAPE '\'`)
		args = append(args, likeContains(f.Text))
	}
	if f.Kind != "" {
		clauses = append(clauses, `kind = ?`)
		args = append(args, f.Kind)
	}
	if f.Category != "" {
		clauses = append(clauses, `category = ?`)
		args = append(args, f.Category)
	}
	if !f.Since.IsZero() {
		clauses = append(clauses, `received_at >= ?`)
		args = append(args, f.Since.Unix())
	}
	if !f.Until.IsZero() {
		clauses = append(clauses, `received_at < ?`)
		args = append(args, f.Until.Unix())
	}
	if len(clauses) == 0 {
		return "1=1", nil
	}
	return strings.Join(clauses, " AND "), args
}

// likeContains escapes LIKE metacharacters so a callsign containing `%` or `_`
// is searched literally instead of matching everything.
func likeContains(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(strings.TrimSpace(s)) + "%"
}

// SearchMessages returns one page of messages, newest first.
func SearchMessages(ctx context.Context, db *sql.DB, f SearchFilter) (Page[Message], error) {
	var page Page[Message]
	if err := f.Normalize(); err != nil {
		return page, err
	}
	where, args := f.where()

	if err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM messages WHERE "+where, args...).Scan(&page.Total); err != nil {
		return page, fmt.Errorf("count messages: %w", err)
	}

	query := "SELECT id, received_at, from_callsign, to_callsign, msg_text, kind, message_id, category FROM messages WHERE " +
		where + " ORDER BY received_at DESC, id DESC LIMIT ? OFFSET ?"
	rows, err := db.QueryContext(ctx, query, append(args, f.Limit, f.Offset)...)
	if err != nil {
		return page, fmt.Errorf("search messages: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var m Message
		var at int64
		if err := rows.Scan(&m.ID, &at, &m.From, &m.To, &m.Text, &m.Kind, &m.MessageID, &m.Category); err != nil {
			return page, err
		}
		m.At = time.Unix(at, 0).UTC()
		page.Rows = append(page.Rows, m)
	}
	page.PerPage = f.Limit
	page.Page = f.Offset/f.Limit + 1
	if page.Total > 0 {
		page.Pages = int((page.Total + int64(f.Limit) - 1) / int64(f.Limit))
	} else {
		page.Pages = 1
	}
	return page, rows.Err()
}

// GetMessage returns one message by id.
func GetMessage(ctx context.Context, db *sql.DB, id int64) (Message, error) {
	var m Message
	var at int64
	err := db.QueryRowContext(ctx,
		`SELECT id, received_at, from_callsign, to_callsign, msg_text, kind, message_id, category FROM messages WHERE id = ?`, id).
		Scan(&m.ID, &at, &m.From, &m.To, &m.Text, &m.Kind, &m.MessageID, &m.Category)
	if errors.Is(err, sql.ErrNoRows) {
		return m, ErrNotFound
	}
	m.At = time.Unix(at, 0).UTC()
	return m, err
}

// Conversation returns the messages exchanged between two stations around a
// reference message, so a detail page can show the exchange in context.
func Conversation(ctx context.Context, db *sql.DB, m Message, window time.Duration, limit int) ([]Message, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := db.QueryContext(ctx, `
		SELECT id, received_at, from_callsign, to_callsign, msg_text, kind, message_id, category
		FROM messages
		WHERE kind IN ('message')
		  AND ((from_callsign = ? AND to_callsign = ?) OR (from_callsign = ? AND to_callsign = ?))
		  AND received_at BETWEEN ? AND ?
		ORDER BY received_at ASC, id ASC
		LIMIT ?`,
		m.From, m.To, m.To, m.From,
		m.At.Add(-window).Unix(), m.At.Add(window).Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var row Message
		var at int64
		if err := rows.Scan(&row.ID, &at, &row.From, &row.To, &row.Text, &row.Kind, &row.MessageID, &row.Category); err != nil {
			return nil, err
		}
		row.At = time.Unix(at, 0).UTC()
		out = append(out, row)
	}
	return out, rows.Err()
}

// ---- statistics ----

// Totals is the headline count board.
type Totals struct {
	Messages   int64
	Bulletins  int64
	Acks       int64
	Stations   int64
	Recipients int64
	FirstAt    time.Time
	LastAt     time.Time
	Today      int64
	LastHour   int64
}

// Totals returns archive-wide counts.
func GetTotals(ctx context.Context, db *sql.DB, now time.Time) (Totals, error) {
	var t Totals
	var first, last sql.NullInt64
	err := db.QueryRowContext(ctx, `
		SELECT
		  COUNT(*),
		  COUNT(CASE WHEN kind = 'bulletin' THEN 1 END),
		  COUNT(DISTINCT from_callsign),
		  COUNT(DISTINCT to_callsign),
		  MIN(received_at),
		  MAX(received_at),
		  COUNT(CASE WHEN received_at >= ? THEN 1 END),
		  COUNT(CASE WHEN received_at >= ? THEN 1 END)
		FROM messages`,
		startOfDay(now).Unix(), now.Add(-time.Hour).Unix()).
		Scan(&t.Messages, &t.Bulletins, &t.Stations, &t.Recipients, &first, &last, &t.Today, &t.LastHour)
	if err != nil {
		return t, fmt.Errorf("totals: %w", err)
	}
	if first.Valid {
		t.FirstAt = time.Unix(first.Int64, 0).UTC()
	}
	if last.Valid {
		t.LastAt = time.Unix(last.Int64, 0).UTC()
	}
	return t, nil
}

// Bucket is one row of a time series or a ranking.
type Bucket struct {
	Label string
	Count int64
}

// Granularity names the time bucket for a series.
type Granularity string

const (
	Hourly Granularity = "hour"
	Daily  Granularity = "day"
)

// SeriesByHour returns one bucket per hour in [since, until).
//
// The bucket arithmetic happens in Go, not in SQLite: `strftime` cannot use the
// index and would force a full scan of the window, while an integer division
// rule is one pass over an index range and the same cost whatever the window.
func Series(ctx context.Context, db *sql.DB, since, until time.Time, g Granularity, kind string) ([]Bucket, error) {
	args := []any{since.Unix(), until.Unix()}
	kindClause := ""
	if kind != "" {
		kindClause = " AND kind = ?"
		args = append(args, kind)
	}
	rows, err := db.QueryContext(ctx,
		`SELECT received_at FROM messages WHERE received_at >= ? AND received_at < ?`+kindClause+` ORDER BY received_at`,
		args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	step := int64(3600)
	layout := "2006-01-02 15:00"
	if g == Daily {
		step = 86400
		layout = "2006-01-02"
	}
	counts := map[int64]int64{}
	order := []int64{}
	for rows.Next() {
		var at int64
		if err := rows.Scan(&at); err != nil {
			return nil, err
		}
		key := at / step
		if _, seen := counts[key]; !seen {
			order = append(order, key)
		}
		counts[key]++
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]Bucket, 0, len(order))
	for _, key := range order {
		out = append(out, Bucket{
			Label: time.Unix(key*step, 0).UTC().Format(layout),
			Count: counts[key],
		})
	}
	return out, nil
}

// TopSenders returns the busiest sources in a window.
func TopSenders(ctx context.Context, db *sql.DB, since, until time.Time, limit int) ([]Bucket, error) {
	if limit <= 0 || limit > 100 {
		limit = 10
	}
	rows, err := db.QueryContext(ctx, `
		SELECT from_callsign, COUNT(*) AS n FROM messages
		WHERE received_at >= ? AND received_at < ?
		GROUP BY from_callsign ORDER BY n DESC, from_callsign ASC LIMIT ?`,
		since.Unix(), until.Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBuckets(rows)
}

// TopRecipients returns the busiest addressees in a window, excluding protocol
// groups so the list is about stations.
func TopRecipients(ctx context.Context, db *sql.DB, since, until time.Time, limit int) ([]Bucket, error) {
	if limit <= 0 || limit > 100 {
		limit = 10
	}
	rows, err := db.QueryContext(ctx, `
		SELECT to_callsign, COUNT(*) AS n FROM messages
		WHERE received_at >= ? AND received_at < ? AND kind = 'message'
		GROUP BY to_callsign ORDER BY n DESC, to_callsign ASC LIMIT ?`,
		since.Unix(), until.Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBuckets(rows)
}

// Pair is two stations that exchanged direct messages, with the count of
// messages in both directions.
//
// The pair is unordered: A writing to B and B writing to A are the same
// conversation, so a ranking of "busiest pairs" must not report them as two
// rows. A and B are returned in lexical order, which makes the identity stable
// across queries and is what the primary key in the UI is built from.
type Pair struct {
	A     string
	B     string
	Count int64
}

// TopPairs returns the busiest station pairs in a window.
//
// Bulletins are excluded: a bulletin has a group as its addressee, not a
// station, so it is not an exchange between two operators. Acks and rejects are
// not stored at all.
//
// Comparison is upper-case: APRS is case-insensitive in the callsign field, and
// a client that transmits `ea7klk-9` is the same station as `EA7KLK-9`. Doing
// the folding in SQL (rather than relying on a column COLLATE) keeps the
// existing indexes usable and keeps the rule visible at the point it matters.
// The canonical form is `min(upper(from),upper(to))` and `max(...)`, so a pair
// has one identity however it was transmitted. A self-addressed packet produces
// a pair whose two halves are equal, which is correct and not filtered: a
// station talking to itself is still one station talking.
//
// No index supports this. The window is served by idx_messages_kind
// (kind, received_at), and the grouping happens in a temporary B-tree over the
// rows in the window. An index on the canonical pair was measured and rejected:
// it grows the file by ~11% and the planner never picks it, because a scan of
// that index cannot apply the time filter and walks every row in the table
// (3.4 s against 1.7 s on two million rows).
func TopPairs(ctx context.Context, db *sql.DB, since, until time.Time, limit int) ([]Pair, error) {
	if limit <= 0 || limit > 100 {
		limit = 10
	}
	rows, err := db.QueryContext(ctx, topPairsQuery,
		since.Unix(), until.Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Pair
	for rows.Next() {
		var a, b string
		var n int64
		if err := rows.Scan(&a, &b, &n); err != nil {
			return nil, err
		}
		out = append(out, Pair{A: a, B: b, Count: n})
	}
	return out, rows.Err()
}

// topPairsQuery is a constant so the test can EXPLAIN exactly what runs, rather
// than a copy of it that could drift.
const topPairsQuery = `
	WITH pairs AS (
	  SELECT CASE WHEN upper(from_callsign) < upper(to_callsign) THEN upper(from_callsign) ELSE upper(to_callsign) END AS a,
	         CASE WHEN upper(from_callsign) < upper(to_callsign) THEN upper(to_callsign)   ELSE upper(from_callsign) END AS b
	  FROM messages
	  WHERE kind = 'message' AND received_at >= ? AND received_at < ?
	)
	SELECT a, b, COUNT(*) AS n
	FROM pairs
	GROUP BY a, b
	ORDER BY n DESC, a ASC, b ASC
	LIMIT ?`

// KindCounts returns the number of packets of each kind in a window.
func KindCounts(ctx context.Context, db *sql.DB, since, until time.Time) ([]Bucket, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT kind, COUNT(*) AS n FROM messages
		WHERE received_at >= ? AND received_at < ?
		GROUP BY kind ORDER BY n DESC, kind ASC`,
		since.Unix(), until.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBuckets(rows)
}

// StationSummary is a per-station profile for the station page.
type StationSummary struct {
	Callsign    string
	Sent        int64
	Received    int64
	SentToday   int64
	FirstAt     time.Time
	LastAt      time.Time
	TopPartners []Bucket
}

// GetStation summarises one callsign.
func GetStation(ctx context.Context, db *sql.DB, callsign string, now time.Time) (StationSummary, error) {
	var s StationSummary
	s.Callsign = strings.ToUpper(strings.TrimSpace(callsign))
	var first, last sql.NullInt64
	// COUNT instead of SUM: SUM over no rows is NULL, and a station that has
	// never been heard must become ErrNotFound, not a scan error.
	err := db.QueryRowContext(ctx, `
		SELECT
		  COUNT(CASE WHEN from_callsign = ? THEN 1 END),
		  COUNT(CASE WHEN to_callsign = ? THEN 1 END),
		  COUNT(CASE WHEN from_callsign = ? AND received_at >= ? THEN 1 END),
		  MIN(received_at), MAX(received_at)
		FROM messages WHERE from_callsign = ? OR to_callsign = ?`,
		s.Callsign, s.Callsign, s.Callsign, startOfDay(now).Unix(),
		s.Callsign, s.Callsign).Scan(&s.Sent, &s.Received, &s.SentToday, &first, &last)
	if err != nil {
		return s, err
	}
	if first.Valid {
		s.FirstAt = time.Unix(first.Int64, 0).UTC()
	}
	if last.Valid {
		s.LastAt = time.Unix(last.Int64, 0).UTC()
	}
	if s.Sent == 0 && s.Received == 0 {
		return s, ErrNotFound
	}

	rows, err := db.QueryContext(ctx, `
		SELECT partner, COUNT(*) AS n FROM (
		  SELECT CASE WHEN from_callsign = ? THEN to_callsign ELSE from_callsign END AS partner
		  FROM messages
		  WHERE (from_callsign = ? OR to_callsign = ?) AND kind = 'message'
		) WHERE partner <> ''
		GROUP BY partner ORDER BY n DESC, partner ASC LIMIT 10`,
		s.Callsign, s.Callsign, s.Callsign)
	if err != nil {
		return s, err
	}
	defer rows.Close()
	s.TopPartners, err = scanBuckets(rows)
	return s, err
}

func scanBuckets(rows *sql.Rows) ([]Bucket, error) {
	var out []Bucket
	for rows.Next() {
		var b Bucket
		if err := rows.Scan(&b.Label, &b.Count); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ---- bulletins ----

// BulletinFilter selects bulletins for the reader.
type BulletinFilter struct {
	Category string
	Group    string
	From     string
	Text     string
	Since    time.Time
	Until    time.Time
	Limit    int
	Offset   int
}

// Bulletins returns one page of bulletins, newest first.
func Bulletins(ctx context.Context, db *sql.DB, f BulletinFilter) (Page[Bulletin], error) {
	var page Page[Bulletin]
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 50
	}
	var clauses []string
	var args []any
	if f.Category != "" {
		clauses = append(clauses, "category = ?")
		args = append(args, f.Category)
	}
	if f.Group != "" {
		clauses = append(clauses, "grp = ?")
		args = append(args, strings.ToUpper(strings.TrimSpace(f.Group)))
	}
	if f.From != "" {
		clauses = append(clauses, `from_callsign LIKE ? ESCAPE '\'`)
		args = append(args, likeContains(f.From))
	}
	if f.Text != "" {
		clauses = append(clauses, `text LIKE ? ESCAPE '\'`)
		args = append(args, likeContains(f.Text))
	}
	if !f.Since.IsZero() {
		clauses = append(clauses, "last_seen >= ?")
		args = append(args, f.Since.Unix())
	}
	if !f.Until.IsZero() {
		clauses = append(clauses, "last_seen < ?")
		args = append(args, f.Until.Unix())
	}
	where := "1=1"
	if len(clauses) > 0 {
		where = strings.Join(clauses, " AND ")
	}
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM bulletins WHERE "+where, args...).Scan(&page.Total); err != nil {
		return page, err
	}
	rows, err := db.QueryContext(ctx,
		`SELECT id, first_seen, last_seen, seen_count, from_callsign, slot, grp, category, text
		 FROM bulletins WHERE `+where+` ORDER BY last_seen DESC, id DESC LIMIT ? OFFSET ?`,
		append(args, f.Limit, f.Offset)...)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		var b Bulletin
		var first, last int64
		if err := rows.Scan(&b.ID, &first, &last, &b.SeenCount, &b.From, &b.Slot, &b.Group, &b.Category, &b.Text); err != nil {
			return page, err
		}
		b.FirstSeen = time.Unix(first, 0).UTC()
		b.LastSeen = time.Unix(last, 0).UTC()
		page.Rows = append(page.Rows, b)
	}
	page.PerPage = f.Limit
	page.Page = f.Offset/f.Limit + 1
	if page.Total > 0 {
		page.Pages = int((page.Total + int64(f.Limit) - 1) / int64(f.Limit))
	} else {
		page.Pages = 1
	}
	return page, rows.Err()
}

// GetBulletin returns one bulletin by id.
func GetBulletin(ctx context.Context, db *sql.DB, id int64) (Bulletin, error) {
	var b Bulletin
	var first, last int64
	err := db.QueryRowContext(ctx,
		`SELECT id, first_seen, last_seen, seen_count, from_callsign, slot, grp, category, text FROM bulletins WHERE id = ?`, id).
		Scan(&b.ID, &first, &last, &b.SeenCount, &b.From, &b.Slot, &b.Group, &b.Category, &b.Text)
	if errors.Is(err, sql.ErrNoRows) {
		return b, ErrNotFound
	}
	b.FirstSeen = time.Unix(first, 0).UTC()
	b.LastSeen = time.Unix(last, 0).UTC()
	return b, err
}

// BulletinCategoryCounts returns the number of bulletins per category.
func BulletinCategoryCounts(ctx context.Context, db *sql.DB) (map[string]int64, error) {
	rows, err := db.QueryContext(ctx, `SELECT category, COUNT(*) FROM bulletins GROUP BY category`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var category string
		var n int64
		if err := rows.Scan(&category, &n); err != nil {
			return nil, err
		}
		out[category] = n
	}
	return out, rows.Err()
}

// BulletinGroups returns the busiest declared groups.
func BulletinGroups(ctx context.Context, db *sql.DB, limit int) ([]Bucket, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := db.QueryContext(ctx, `
		SELECT CASE WHEN grp = '' THEN '(none)' ELSE grp END, COUNT(*) AS n
		FROM bulletins GROUP BY grp ORDER BY n DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBuckets(rows)
}

// ---- collector health ----

// CollectorState is what the collector last reported about itself.
type CollectorState struct {
	StartedAt     time.Time
	LastWriteAt   time.Time
	LastConnectAt time.Time
	PacketsSeen   int64
	PacketsStored int64
	Connects      int64
	LastError     string
	Found         bool
}

// GetCollectorState reads the single health row.
func GetCollectorState(ctx context.Context, db *sql.DB) (CollectorState, error) {
	var s CollectorState
	var started, lastWrite, lastConnect int64
	err := db.QueryRowContext(ctx,
		`SELECT started_at, last_write_at, last_connect_at, packets_seen, packets_stored, connects, last_error FROM collector_state WHERE id = 1`).
		Scan(&started, &lastWrite, &lastConnect, &s.PacketsSeen, &s.PacketsStored, &s.Connects, &s.LastError)
	if errors.Is(err, sql.ErrNoRows) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	s.Found = true
	s.StartedAt = time.Unix(started, 0).UTC()
	if lastWrite > 0 {
		s.LastWriteAt = time.Unix(lastWrite, 0).UTC()
	}
	if lastConnect > 0 {
		s.LastConnectAt = time.Unix(lastConnect, 0).UTC()
	}
	return s, nil
}

// DatabaseSize returns the archive file size in bytes, plus the WAL, so the
// statistics page can show growth against the claim's capacity.
func DatabaseSize(path string) (int64, error) {
	var total int64
	for _, suffix := range []string{"", "-wal"} {
		if info, err := statFile(path + suffix); err == nil {
			total += info
		}
	}
	return total, nil
}

func startOfDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// StartOfDay is the exported form, used by the web layer for its date pickers.
func StartOfDay(t time.Time) time.Time { return startOfDay(t) }
