package telemetry

import (
	"math"
	"strings"
)

// Encoded-blob detection.
//
// Some machine traffic is not a beacon or a status string but an opaque run of
// encoded bytes — an encrypted message, a packed telemetry frame, a file
// fragment. It has no words to match and no fixed prefix, so a regular
// expression cannot describe it: what distinguishes it is that it looks like
// random base64.
//
// A regex cannot express that. Go's RE2 has no lookahead, so "contains an
// uppercase letter and a digit somewhere" is not directly writable, and
// "entropy above 4.3 bits per character" is not a regular property in any
// practical form. This file therefore provides a detector in code, and the
// filter table carries a `kind` column so a rule can name it.

// encodedKind is the pattern value that selects this detector.
const encodedKind = "encoded-blob"

// blobChars are the base64 and base64url alphabets plus the line prefix a
// packet BBS adds. A body made only of these, and long enough, is a candidate.
var blobChars = func() [256]bool {
	var t [256]bool
	for _, r := range "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/=-_" {
		t[r] = true
	}
	return t
}()

// minBlobLen is the shortest body considered. Words and callsigns are shorter;
// a base64 run of 24 characters carries about 18 bytes of payload.
const minBlobLen = 24

// minBlobEntropy is in bits per character.
//
// English prose sits near 4.0 including spaces; the blobs observed on this
// network measured 4.5 and above. The threshold is placed between them, and the
// case and symbol requirements below remove the remaining word-like strings that
// happen to clear it.
const minBlobEntropy = 4.3

// isEncodedBlob reports whether a body is an opaque encoded run.
//
// Three conditions, all needed:
//
//   - every character is in the base64 or base64url alphabet (the ';' a packet
//     BBS wraps a line in is stripped from both ends first);
//   - it is at least minBlobLen characters;
//   - it mixes upper and lower case, contains a digit or '+/=', and has entropy
//     of at least minBlobEntropy bits per character.
//
// The case and symbol tests are what keep a run-together callsign or a
// human-readable name ("HelloBeiJingFengTai438500", "Paralelepipidoouua...")
// out, while the entropy test is what keeps a repetitive scan list
// ("L+12283+10282+...") out.
func isEncodedBlob(text string) bool {
	s := strings.TrimSpace(text)
	// A packet BBS wraps a line in ';' — sometimes leading, sometimes trailing,
	// sometimes both. Strip both ends before the alphabet test, or a single
	// terminator hides an otherwise obvious payload.
	s = strings.Trim(s, ";")
	if len(s) < minBlobLen {
		return false
	}
	var hasUpper, hasLower, hasDigitOrSymbol bool
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !blobChars[c] {
			return false
		}
		switch {
		case c >= 'A' && c <= 'Z':
			hasUpper = true
		case c >= 'a' && c <= 'z':
			hasLower = true
		case c >= '0' && c <= '9' || c == '+' || c == '/' || c == '=' || c == '-' || c == '_':
			hasDigitOrSymbol = true
		}
	}
	if !hasUpper || !hasLower || !hasDigitOrSymbol {
		return false
	}
	return entropy(s) >= minBlobEntropy
}

// entropy is Shannon entropy in bits per character.
func entropy(s string) float64 {
	if len(s) == 0 {
		return 0
	}
	var counts [256]int
	for i := 0; i < len(s); i++ {
		counts[s[i]]++
	}
	n := float64(len(s))
	var h float64
	for _, c := range counts {
		if c == 0 {
			continue
		}
		p := float64(c) / n
		h -= p * math.Log2(p)
	}
	return h
}
