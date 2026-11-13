package web

import (
	"embed"
	"fmt"
	"html/template"
	"math"
	"strconv"
	"time"

	"aprs-archive/internal/store"
)

//go:embed templates/*.html templates/style.css
var templateFS embed.FS

// parseTemplates loads every page template plus the stylesheet, which is
// embedded and inlined into the page so the archive ships as one binary.
func parseTemplates() (*template.Template, error) {
	css, err := templateFS.ReadFile("templates/style.css")
	if err != nil {
		return nil, fmt.Errorf("read stylesheet: %w", err)
	}
	funcs := template.FuncMap{
		"style": func() template.CSS { return template.CSS(css) },
		// date renders a timestamp with the archive's one format.
		"date": func(t time.Time) string {
			if t.IsZero() {
				return "—"
			}
			return t.UTC().Format("2006-01-02")
		},
		"clock": func(t time.Time) string {
			if t.IsZero() {
				return "—"
			}
			return t.UTC().Format("15:04:05")
		},
		// lastDay renders an exclusive upper bound as the last day it includes.
		// Report and search windows are half-open in SQL and inclusive in the
		// form; every page must speak the second language.
		"lastDay": func(t time.Time) string {
			if t.IsZero() {
				return "—"
			}
			return t.AddDate(0, 0, -1).UTC().Format("2006-01-02")
		},
		// oldestDay is the earliest start a report may use: the reporting limit
		// counted back from the chosen end, so the date picker cannot offer a
		// window the server would trim.
		"oldestDay": func(until time.Time, maxDays int) string {
			if until.IsZero() || maxDays <= 0 {
				return ""
			}
			return until.AddDate(0, 0, -maxDays).UTC().Format("2006-01-02")
		},
		"lastStamp": func(t time.Time) string {
			if t.IsZero() {
				return "—"
			}
			return t.AddDate(0, 0, -1).UTC().Format("2006-01-02") + " 24:00"
		},
		"stamp": func(t time.Time) string {
			if t.IsZero() {
				return "—"
			}
			return t.UTC().Format("2006-01-02 15:04:05")
		},
		// ago renders a relative time for "last written" style fields.
		"ago": func(t time.Time, now time.Time) string {
			if t.IsZero() {
				return "never"
			}
			d := now.Sub(t).Round(time.Second)
			if d < 0 {
				return "just now"
			}
			switch {
			case d < time.Minute:
				return fmt.Sprintf("%ds ago", int(d.Seconds()))
			case d < time.Hour:
				return fmt.Sprintf("%dm ago", int(d.Minutes()))
			case d < 48*time.Hour:
				return fmt.Sprintf("%dh ago", int(d.Hours()))
			default:
				return fmt.Sprintf("%dd ago", int(d.Hours()/24))
			}
		},
		"num": func(n int64) string { return groupDigits(n) },
		"pct": func(part, total int64) string {
			if total == 0 {
				return "0%"
			}
			return strconv.FormatFloat(float64(part)/float64(total)*100, 'f', 1, 64) + "%"
		},
		// barWidth turns a count into a percentage of the largest count, for the
		// CSS bar chart. A floor keeps a non-zero bar visible.
		"barWidth": func(count, max int64) string {
			if max <= 0 || count <= 0 {
				return "0%"
			}
			w := float64(count) / float64(max) * 100
			if w < 1.5 {
				w = 1.5
			}
			return strconv.FormatFloat(w, 'f', 1, 64) + "%"
		},
		"maxCount": func(buckets any) int64 {
			var m int64
			switch v := buckets.(type) {
			case []store.Bucket:
				for _, b := range v {
					if b.Count > m {
						m = b.Count
					}
				}
			case []categoryRow:
				for _, b := range v {
					if b.Count > m {
						m = b.Count
					}
				}
			}
			return m
		},
		// sparkHeight turns a count into a pixel height for the sparkline.
		"sparkHeight": func(count, max int64) string {
			if max <= 0 {
				return "1px"
			}
			h := math.Max(1, float64(count)/float64(max)*52)
			return strconv.FormatFloat(h, 'f', 0, 64) + "px"
		},
		"add": func(a, b int) int { return a + b },
		"sub": func(a, b int) int { return a - b },
		"humanBytes": func(n int64) string {
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
		},
		"navActive": navActive,
		// dict builds a map from alternating keys and values, so a partial can
		// be called with more than one argument.
		"dict": func(values ...any) map[string]any {
			m := map[string]any{}
			for i := 0; i+1 < len(values); i += 2 {
				m[fmt.Sprint(values[i])] = values[i+1]
			}
			return m
		},
	}

	// ParseFS parses every page in one namespace, so a page can call another's
	// definitions. A parse error is fatal at startup rather than on first use:
	// a web pod that starts and then 500s every page is worse than one that
	// refuses to start and says which template is broken.
	tpl, err := template.New("app").Funcs(funcs).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	return tpl, nil
}

// bucket is a label/count pair as the templates see it. The store's Bucket is
// the real type; this alias exists so a template helper can name it without
// importing the store package in the FuncMap signature.
type bucket = store.Bucket
