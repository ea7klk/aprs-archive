package admin

import (
	"context"
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
