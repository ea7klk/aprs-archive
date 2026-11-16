package admin

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"aprs-archive/internal/store"
	"aprs-archive/internal/telemetry"
)

// The telemetry filter page.
//
// Editing a pattern is a decision about what the archive keeps, and a bad
// pattern deletes real messages silently. So the page does three things a plain
// table would not: it validates a pattern before saving it, it shows how many
// stored rows it currently matches, and it offers a tester so the operator can
// paste a real body and see the verdict.

type filterData struct {
	Filters     []store.MessageFilter
	Flash       string
	ErrorDetail string
	// TestText and TestResult are the tester's last run.
	TestText  string
	TestRule  string
	TestMatch bool
	TestRan   bool
	TestError string
	// Edit is the row being edited, if any.
	Edit *store.MessageFilter
	// Apply is the "delete matching a pattern now" panel: the pattern as typed,
	// and up to twelve examples of what it would delete.
	Apply         string
	ApplyCount    int64
	ApplyCounted  bool
	ApplyError    string
	ApplyExamples []applyExample
	ApplyRemoved  int64
	ApplyDeleted  bool
	// ApplyTokens are the patterns the operator has previewed and may now
	// apply. Applying requires a token that only a preview can produce, so a
	// delete can never be triggered by a form submitted without first seeing
	// what it would remove.
	ApplyToken string
}

// applyExample is one stored message a pattern would delete.
type applyExample struct {
	ID   int64
	From string
	At   string
	Text string
}

func (s *Server) filtersPage(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	ctx := r.Context()
	filters, err := store.ListMessageFilters(ctx, s.db)
	if err != nil {
		log.Printf("admin: list filters: %v", err)
		s.render(w, r, "admin_filters", &filterData{Flash: r.URL.Query().Get("ok")},
			"The filter list could not be read.")
		return
	}
	data := &filterData{
		Filters:     filters,
		Flash:       r.URL.Query().Get("ok"),
		ErrorDetail: r.URL.Query().Get("err"),
	}
	// Live match counts, computed once per page load over the archive. A
	// pattern's usefulness is not obvious from its text, so it is worth the
	// scan to show it.
	for i := range data.Filters {
		data.Filters[i].Matches = s.countMatches(ctx, data.Filters[i].Pattern)
	}
	if id := r.URL.Query().Get("edit"); id != "" {
		if n, err := strconv.ParseInt(id, 10, 64); err == nil {
			for i := range data.Filters {
				if data.Filters[i].ID == n {
					f := data.Filters[i]
					data.Edit = &f
				}
			}
		}
	}
	s.render(w, r, "admin_filters", data, "")
}

// countMatches runs a pattern over stored bodies.
//
// It is bounded and reads only the text column. A pattern that does not compile
// counts as -1 so the page can say so rather than showing zero, which would
// look like a harmless rule.
func (s *Server) countMatches(ctx context.Context, pattern string) int64 {
	if err := telemetry.Validate(pattern); err != nil {
		return -1
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT msg_text FROM messages`)
	if err != nil {
		return 0
	}
	defer rows.Close()
	m := telemetry.Compile([]telemetry.Source{{ID: 0, Name: "test", Pattern: pattern}}, 0)
	var n int64
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return n
		}
		if _, ok := m.Match(t); ok {
			n++
		}
	}
	return n
}

func (s *Server) saveFilter(w http.ResponseWriter, r *http.Request) {
	if !s.requirePost(w, r) {
		return
	}
	id, _ := strconv.ParseInt(r.PostFormValue("id"), 10, 64)
	f := store.MessageFilter{
		ID:      id,
		Name:    r.PostFormValue("name"),
		Pattern: r.PostFormValue("pattern"),
		Note:    r.PostFormValue("note"),
		Enabled: r.PostFormValue("enabled") == "on" || r.PostFormValue("enabled") == "true",
	}
	if err := telemetry.Validate(f.Pattern); err != nil {
		s.resultTo(w, r, "/admin/filters", "That pattern will not compile: "+err.Error(), false)
		return
	}
	if _, err := store.SaveMessageFilter(r.Context(), s.db, f); err != nil {
		s.resultTo(w, r, "/admin/filters", "Could not save: "+err.Error(), false)
		return
	}
	// A saved rule that is not enabled stops nothing, so there is nothing to
	// clean up. Say so rather than reporting a purge that did not happen.
	if !f.Enabled {
		s.resultTo(w, r, "/admin/filters",
			"Saved, but disabled: no stored messages were removed and nothing new will be filtered until you enable it.", true)
		return
	}
	removed, err := s.purgeForPattern(r.Context(), f.Pattern)
	if err != nil {
		s.resultTo(w, r, "/admin/filters",
			"Saved and active, but removing the stored matches failed: "+err.Error()+". Use the purge action to retry.", false)
		return
	}
	s.resultTo(w, r, "/admin/filters", fmt.Sprintf(
		"Saved and active. Removed %d stored %s matching it; the collector applies the rule to new traffic within a minute.",
		removed, plural(removed, "message", "messages")), true)
}

func (s *Server) toggleFilter(w http.ResponseWriter, r *http.Request) {
	if !s.requirePost(w, r) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.resultTo(w, r, "/admin/filters", "Bad filter id.", false)
		return
	}
	enabled := r.PostFormValue("enabled") == "1"
	filters, err := store.ListMessageFilters(r.Context(), s.db)
	if err != nil {
		s.resultTo(w, r, "/admin/filters", "Could not read the filters: "+err.Error(), false)
		return
	}
	var target *store.MessageFilter
	for i := range filters {
		if filters[i].ID == id {
			target = &filters[i]
		}
	}
	if target == nil {
		s.resultTo(w, r, "/admin/filters", "No such filter.", false)
		return
	}
	if err := store.SetMessageFilterEnabled(r.Context(), s.db, id, enabled); err != nil {
		s.resultTo(w, r, "/admin/filters", "Could not change the filter: "+err.Error(), false)
		return
	}
	if !enabled {
		s.resultTo(w, r, "/admin/filters",
			"Filter disabled. Stored messages are untouched; only new traffic is affected.", true)
		return
	}
	// Enabling is the moment the rule takes effect, so the archive is brought
	// into line with it in the same action.
	removed, err := s.purgeForPattern(r.Context(), target.Pattern)
	if err != nil {
		s.resultTo(w, r, "/admin/filters",
			"Filter enabled, but removing the stored matches failed: "+err.Error()+". Use the purge action to retry.", false)
		return
	}
	s.resultTo(w, r, "/admin/filters", fmt.Sprintf(
		"Filter enabled. Removed %d stored %s matching it.", removed, plural(removed, "message", "messages")), true)
}

// purgeForPattern deletes the stored messages one pattern matches.
//
// The bounds are deliberate: a page load must not start an unbounded delete, so
// the scan has its own timeout and the count is reported back to the operator
// rather than performed silently.
func (s *Server) purgeForPattern(parent context.Context, pattern string) (int64, error) {
	if err := telemetry.Validate(pattern); err != nil {
		return 0, err
	}
	m := telemetry.Compile([]telemetry.Source{{Name: "purge", Pattern: pattern}}, 0)
	ctx, cancel := context.WithTimeout(parent, 5*time.Minute)
	defer cancel()
	return store.PurgeFilterMatches(ctx, s.db, func(text string) bool {
		_, ok := m.Match(text)
		return ok
	})
}

// purgeAll re-runs the purge for every enabled rule, for the archive that was
// written before a rule existed or when a purge failed part-way.
func (s *Server) purgeAll(w http.ResponseWriter, r *http.Request) {
	if !s.requirePost(w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	filters, err := store.EnabledMessageFilters(r.Context(), s.db)
	if err != nil {
		s.resultTo(w, r, "/admin/filters", "Could not read the filters: "+err.Error(), false)
		return
	}
	var total int64
	for _, f := range filters {
		if err := telemetry.Validate(f.Pattern); err != nil {
			continue
		}
		m := telemetry.Compile([]telemetry.Source{{Name: f.Name, Pattern: f.Pattern}}, 0)
		n, err := store.PurgeFilterMatches(ctx, s.db, func(text string) bool {
			_, ok := m.Match(text)
			return ok
		})
		if err != nil {
			s.resultTo(w, r, "/admin/filters",
				fmt.Sprintf("Purge stopped at %q after removing %d: %v", f.Name, total, err), false)
			return
		}
		total += n
	}
	s.resultTo(w, r, "/admin/filters", fmt.Sprintf(
		"Removed %d stored %s matching the enabled filters.", total, plural(total, "message", "messages")), true)
}

func plural(n int64, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func (s *Server) deleteFilter(w http.ResponseWriter, r *http.Request) {
	if !s.requirePost(w, r) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.resultTo(w, r, "/admin/filters", "Bad filter id.", false)
		return
	}
	if err := store.DeleteMessageFilter(r.Context(), s.db, id); err != nil {
		s.resultTo(w, r, "/admin/filters", "Could not delete: "+err.Error(), false)
		return
	}
	s.resultTo(w, r, "/admin/filters", "Filter deleted. Stored messages are not affected; run a purge to remove them.", true)
}

// testFilter checks one body against the saved rules, or against a draft
// pattern when one is supplied.
func (s *Server) testFilter(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.resultTo(w, r, "/admin/filters", "Malformed form.", false)
		return
	}
	text := r.PostFormValue("text")
	draft := strings.TrimSpace(r.PostFormValue("pattern"))
	data := &filterData{TestText: text, TestRan: true}
	filters, _ := store.ListMessageFilters(r.Context(), s.db)
	data.Filters = filters
	for i := range data.Filters {
		data.Filters[i].Matches = s.countMatches(r.Context(), data.Filters[i].Pattern)
	}
	if draft != "" {
		if err := telemetry.Validate(draft); err != nil {
			data.TestError = err.Error()
			s.render(w, r, "admin_filters", data, "")
			return
		}
		m := telemetry.Compile([]telemetry.Source{{Name: "draft", Pattern: draft}}, 0)
		if rule, ok := m.Match(text); ok {
			data.TestMatch = true
			data.TestRule = rule.Name
		}
		s.render(w, r, "admin_filters", data, "")
		return
	}
	// No draft: test the saved, enabled rules.
	items, _ := store.EnabledMessageFilters(r.Context(), s.db)
	var src []telemetry.Source
	for _, it := range items {
		src = append(src, telemetry.Source{ID: it.ID, Name: it.Name, Pattern: it.Pattern})
	}
	if rule, ok := telemetry.Compile(src, 0).Match(text); ok {
		data.TestMatch = true
		data.TestRule = rule.Name
	}
	s.render(w, r, "admin_filters", data, "")
}

// ---- delete by pattern: preview, then apply ----

// applyPreview counts what a pattern would delete and shows examples.
//
// The operator sees the number and a sample of the actual messages before
// anything is removed. This is the read-only half of the operation, and nothing
// is deleted here.
func (s *Server) applyPreview(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.resultTo(w, r, "/admin/filters", "Malformed form.", false)
		return
	}
	pattern := strings.TrimSpace(r.PostFormValue("pattern"))
	data, err := s.filterPageData(r)
	if err != nil {
		s.render(w, r, "admin_filters", nil, "The filter list could not be read.")
		return
	}
	data.Apply = pattern
	data.ApplyCounted = true
	if pattern == "" {
		data.ApplyError = "Give a pattern to match."
		s.render(w, r, "admin_filters", data, "")
		return
	}
	if err := telemetry.Validate(pattern); err != nil {
		data.ApplyError = "That pattern will not compile: " + err.Error()
		s.render(w, r, "admin_filters", data, "")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()

	n, examples, err := countAndSample(ctx, s.db, pattern, 12)
	if err != nil {
		data.ApplyError = "Could not scan the archive: " + err.Error()
		s.render(w, r, "admin_filters", data, "")
		return
	}
	data.ApplyCount = n
	data.ApplyExamples = examples
	data.ApplyToken = s.applyToken(pattern)
	s.render(w, r, "admin_filters", data, "")
}

// applyDelete removes every stored message matching the pattern.
//
// It requires the token produced by a preview, and the token encodes the
// pattern, so the pattern actually applied is the one whose count the operator
// saw. A hand-crafted POST cannot skip the preview.
func (s *Server) applyDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requirePost(w, r) {
		return
	}
	pattern := strings.TrimSpace(r.PostFormValue("pattern"))
	supplied := r.PostFormValue("apply_token")
	if pattern == "" {
		s.resultTo(w, r, "/admin/filters", "Give a pattern to match.", false)
		return
	}
	if !s.validApplyToken(pattern, supplied) {
		s.resultTo(w, r, "/admin/filters",
			"Preview this pattern first: the delete step needs the confirmation from the preview, so you can see what it will remove.", false)
		return
	}
	removed, err := s.purgeForPattern(r.Context(), pattern)
	if err != nil {
		s.resultTo(w, r, "/admin/filters",
			fmt.Sprintf("Deleted %d messages, then stopped: %v", removed, err), false)
		return
	}
	// The pattern itself is not echoed back into the redirect: a regular
	// expression is mostly punctuation, and URL-escaping it produces a message
	// nobody can read. The operator has just typed it; the count is the news.
	s.resultTo(w, r, "/admin/filters", fmt.Sprintf(
		"Deleted %d stored %s matching that pattern.", removed, plural(removed, "message", "messages")), true)
}

// countAndSample scans the archive once for both the count and the examples.
func countAndSample(ctx context.Context, db *sql.DB, pattern string, limit int) (int64, []applyExample, error) {
	m := telemetry.Compile([]telemetry.Source{{Name: "apply", Pattern: pattern}}, 0)
	rows, err := db.QueryContext(ctx, `
		SELECT id, from_callsign, received_at, msg_text FROM messages`)
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()
	var n int64
	var out []applyExample
	for rows.Next() {
		var id, at int64
		var from, text string
		if err := rows.Scan(&id, &from, &at, &text); err != nil {
			return n, out, err
		}
		if _, ok := m.Match(text); !ok {
			continue
		}
		n++
		if len(out) < limit {
			out = append(out, applyExample{
				ID: id, From: from, Text: text,
				At: time.Unix(at, 0).UTC().Format("2006-01-02 15:04"),
			})
		}
	}
	return n, out, rows.Err()
}

// applyToken derives the confirmation for one pattern from the admin password.
//
// Encoding the pattern ties the token to exactly what was previewed, and
// deriving it from the password means it dies with the session and cannot be
// fabricated without the credential.
func (s *Server) applyToken(pattern string) string {
	mac := hmac.New(sha256.New, s.sessionKey)
	mac.Write([]byte("aprs-archive-apply-v1"))
	mac.Write([]byte(pattern))
	return hex.EncodeToString(mac.Sum(nil))
}

func (s *Server) validApplyToken(pattern, supplied string) bool {
	if supplied == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(supplied), []byte(s.applyToken(pattern))) == 1
}

// filterPageData loads the rule table shared by the page and the apply panel.
func (s *Server) filterPageData(r *http.Request) (*filterData, error) {
	filters, err := store.ListMessageFilters(r.Context(), s.db)
	if err != nil {
		return nil, err
	}
	data := &filterData{
		Filters:     filters,
		Flash:       r.URL.Query().Get("ok"),
		ErrorDetail: r.URL.Query().Get("err"),
	}
	for i := range data.Filters {
		data.Filters[i].Matches = s.countMatches(r.Context(), data.Filters[i].Pattern)
	}
	return data, nil
}
