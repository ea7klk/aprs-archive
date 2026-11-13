// Package sanitize holds the one rule for what third-party text may keep.
//
// Every string that reaches the archive comes from the APRS-IS stream, which is
// an open network: anybody with a transmitter can put anything in a message
// body. The web interface renders that text, so it is cleaned once on write and
// again on read, exactly as simple-ham-bbs and ham-bbs-web do for their own
// copy of the same traffic. Keeping the rule in one package means the collector
// and the web process cannot disagree about what a "clean" row looks like.
package sanitize

import (
	"strings"
	"unicode"
)

// maxConsecutiveLineBreaks bounds blank-line runs so a stored body cannot
// scroll a page with newlines alone.
const maxConsecutiveLineBreaks = 2

// hostileRunes are printable-but-invisible characters that can reorder, hide,
// or split text: bidi overrides and isolates, directional marks, zero-width
// space, word joiner, soft hyphen, the byte-order mark, and Unicode line and
// paragraph separators.
//
// ZWJ (U+200D) and ZWNJ (U+200C) are deliberately kept: they are required for
// correct shaping and emoji sequences in scripts this archive displays.
var hostileRunes = map[rune]struct{}{
	'\u00ad': {}, // soft hyphen
	'\u061c': {}, // arabic letter mark
	'\u200b': {}, // zero width space
	'\u200e': {}, // left-to-right mark
	'\u200f': {}, // right-to-left mark
	'\u2028': {}, // line separator
	'\u2029': {}, // paragraph separator
	'\u202a': {}, // left-to-right embedding
	'\u202b': {}, // right-to-left embedding
	'\u202c': {}, // pop directional formatting
	'\u202d': {}, // left-to-right override
	'\u202e': {}, // right-to-left override
	'\u2060': {}, // word joiner
	'\u2066': {}, // left-to-right isolate
	'\u2067': {}, // right-to-left isolate
	'\u2068': {}, // first strong isolate
	'\u2069': {}, // pop directional isolate
	'\ufeff': {}, // zero width no-break space / byte-order mark
}

// Text strips everything that could drive a browser or a terminal (ESC, C0/C1
// controls, DEL), drops the invisible characters above, and normalises line
// breaks: CRLF and lone CR become LF, tabs become spaces, and blank-line runs
// are capped. Multi-line text keeps its line breaks; single-line text turns
// them into spaces.
//
// The function is idempotent, so applying it at more than one boundary cannot
// corrupt content.
func Text(text string, multiline bool) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ToValidUTF8(text, string(unicode.ReplacementChar))
	var b strings.Builder
	b.Grow(len(text))
	breaks := 0
	for _, r := range text {
		switch {
		case r == '\n' || r == '\r':
			if !multiline {
				b.WriteByte(' ')
				breaks = 0
				continue
			}
			if breaks < maxConsecutiveLineBreaks {
				b.WriteByte('\n')
			}
			breaks++
			continue
		case r == '\t':
			b.WriteByte(' ')
			continue
		}
		if unicode.IsControl(r) {
			continue
		}
		if _, hostile := hostileRunes[r]; hostile {
			continue
		}
		breaks = 0
		b.WriteRune(r)
	}
	return b.String()
}

// SingleLine cleans text that renders on one line (callsigns, groups) and
// collapses internal whitespace runs.
func SingleLine(text string) string {
	return strings.Join(strings.Fields(Text(text, false)), " ")
}

// MultiLine cleans text that may keep line breaks and trims surrounding space.
func MultiLine(text string) string {
	return strings.TrimSpace(Text(text, true))
}
