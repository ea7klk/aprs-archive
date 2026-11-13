package web

import (
	"strconv"
	"strings"

	"aprs-archive/internal/store"
)

// groupDigits formats an integer with thin, ASCII-only thousands separators.
//
// Commas are correct in English and an unambiguous grouping in a monospace
// column; a locale-aware formatter would add a dependency for one character in
// a statistics number.
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

// maxPairCount is the largest pair count, for bar widths in the template.
func maxPairCount(pairs []store.Pair) int64 {
	var m int64
	for _, p := range pairs {
		if p.Count > m {
			m = p.Count
		}
	}
	return m
}
