package admin

import (
	"fmt"
	"html/template"
	"strconv"
	"strings"
	"time"

	"aprs-archive/internal/prefixes"
)

// parseTemplates loads the admin templates and inlines the shared stylesheet.
//
// The admin application carries its own copy of the stylesheet rather than
// reaching into the public package's assets: the two binaries embed their own
// templates, so an admin deployment cannot fail to start because a public
// template moved, and a change to one cannot silently reshape the other's pages.
func parseTemplates() (*template.Template, error) {
	css, err := assets.ReadFile("templates/style.css")
	if err != nil {
		return nil, fmt.Errorf("read stylesheet: %w", err)
	}
	funcs := template.FuncMap{
		"style": func() template.CSS { return template.CSS(css) },
		"stamp": func(t time.Time) string {
			if t.IsZero() {
				return "—"
			}
			return t.UTC().Format("2006-01-02 15:04:05")
		},
		"date": func(t time.Time) string {
			if t.IsZero() {
				return "—"
			}
			return t.UTC().Format("2006-01-02")
		},
		"ago": func(t time.Time, now time.Time) string {
			if t.IsZero() {
				return "never"
			}
			d := now.Sub(t).Round(time.Second)
			switch {
			case d < 0:
				return "just now"
			case d < time.Minute:
				return fmt.Sprintf("%ds ago", int(d.Seconds()))
			case d < time.Hour:
				return fmt.Sprintf("%dm ago", int(d.Minutes()))
			default:
				return fmt.Sprintf("%dh ago", int(d.Hours()))
			}
		},
		"humanBytes": humanBytes,
		// int64 widens a template's int field for num, which takes int64 so a
		// row count cannot overflow on a 32-bit build.
		"int64": func(n int) int64 { return int64(n) },
		"num":   func(n int64) string { return groupDigits(n) },
		"itoa":  strconv.Itoa,
		// prefixDataset is the source the exception page explains itself
		// against.
		"prefixDataset": func() string { return prefixes.DefaultSource },
	}
	return template.New("admin").Funcs(funcs).ParseFS(assets, "templates/*.html")
}

// groupDigits formats an integer with thousands separators.
func groupDigits(n int64) string {
	negative := n < 0
	if negative {
		n = -n
	}
	digits := strconv.FormatInt(n, 10)
	var b strings.Builder
	for i, r := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if negative {
		return "-" + b.String()
	}
	return b.String()
}
