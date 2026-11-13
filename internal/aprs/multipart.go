package aprs

import (
	"strconv"
	"strings"
	"time"
)

// Multi-part messages.
//
// APRS has no multipart message type, so senders invent numbering, and the
// archive has to recognise each convention it sees:
//
//	[1/2] body            prefix, the form simple-ham-bbs uses
//	[n/m] body            prefix, any numbers
//	body (1/3)            suffix, as EA3JCH-13 and EB3CLX-3 transmit
//	body (2/3){4Y}        suffix followed by the part's own message id
//
// The suffix form matters because it is invisible if the numbering is not
// looked for at the end: a real three-part message from EA3JCH-13 to EB3CLX-3
// arrived, was stored, and was never assembled because the prefix parser only
// looked at the start of the body.
//
// Two rules keep the suffix form from matching prose. The numbering is a token
// of its own — `les (2/3)` — so a body like `ALERTA ... (13/11)` is a date, not
// a part, and single-digit numbering inside a word is not numbering at all. And
// assembly needs a *set*: a second part with the same total from the same
// sender to the same recipient, arriving in the window. A stray "(1/1)" in a
// sentence therefore stays a message on its own, because the second rule cannot
// be satisfied by chance.
//
// `[1/1]` and `(1/1)` are complete single-part messages and never open a set.
// One station sent twenty-one `[1/1]` messages to twenty-one recipients in the
// first day of traffic; treating them as parts of one set would have merged
// twenty-one unrelated messages.

// PartPrefix is a parsed `[n/m]` prefix.
type PartPrefix struct {
	Part  int
	Total int
	Body  string
}

// PartSuffixRE matches a trailing "(n/m)" numbering token.
//
// The token must be at the end of the body or followed only by a message id
// ("{4Y}"), and either may be preceded by a space or by the end of the previous
// word — but not by a digit, which is what separates "(2/3)" from the "13/11" in
// a date written "(13/11)". Only single-digit numbering is accepted: a real
// two-digit part is not a thing, and the restriction is what keeps years,
// dates and fractions out.
// ParsePartPrefix reports whether a body carries an `[n/m]` prefix or an
// `(n/m)` suffix, and returns the part number, the total and the body without
// the numbering.
//
// The prefix form is checked first because it is unambiguous; the suffix form
// is then anchored to the end of the body.
func ParsePartPrefix(text string) (PartPrefix, bool) {
	text = strings.TrimLeft(text, " \t")

	if strings.HasPrefix(text, "[") {
		close := strings.IndexByte(text, ']')
		if close >= 4 && close <= 6 {
			if p, ok := parseNumbering(text[1:close]); ok {
				return PartPrefix{Part: p.Part, Total: p.Total, Body: strings.TrimPrefix(text[close+1:], " ")}, true
			}
		}
	}

	if p, ok := parsePartSuffix(text); ok {
		return p, true
	}

	return PartPrefix{}, false
}

// parsePartSuffix recognises a trailing "(n/m)" numbering token.
//
// It is parsed by hand rather than by a regular expression, because that is the
// clearer statement of a small grammatical rule and because it is easy to test
// exhaustively: the token must end the body (optionally followed by a message id
// in braces), must be one digit over one digit, and must be a token of its own.
//
// The "token of its own" test is what rejects a date. `(13/11)` is two digits
// and is not numbering; `les (2/3)` is one digit and follows a space, so it is.
// Inside a word — `x2/3)` — it is not.
func parsePartSuffix(text string) (PartPrefix, bool) {
	body := strings.TrimRight(text, " \t")

	// Drop a trailing message id first: "(2/3){4Y}" ends with the id, not with
	// the numbering. The braces must be a well-formed id, so prose that happens
	// to end in "}" is not silently accepted.
	if strings.HasSuffix(body, "}") {
		k := strings.LastIndexByte(body, '{')
		if k < 0 {
			return PartPrefix{}, false
		}
		id := body[k+1 : len(body)-1]
		if id == "" || len(id) > 5 || !isIDString(id) {
			return PartPrefix{}, false
		}
		body = strings.TrimRight(body[:k], " \t")
	}
	if !strings.HasSuffix(body, ")") {
		return PartPrefix{}, false
	}
	open := strings.LastIndexByte(body, '(')
	if open < 0 {
		return PartPrefix{}, false
	}
	inner := body[open+1 : len(body)-1]
	if len(inner) != 3 || inner[1] != '/' {
		// One digit, a slash, one digit. "13/11" is a date, not numbering.
		return PartPrefix{}, false
	}
	part, total := inner[0], inner[2]
	if part < '1' || part > '9' || total < '1' || total > '9' {
		return PartPrefix{}, false
	}
	if part > total {
		return PartPrefix{}, false
	}
	// The token must be separated from the text before it: a space, punctuation
	// or the start of the body. "x2/3)" has no separator and is not a part.
	before := body[:open]
	if before != "" {
		switch last := before[len(before)-1]; last {
		case ' ', '\t', '.', ',', '!', ';', ':':
		default:
			return PartPrefix{}, false
		}
	}
	body = strings.TrimSpace(before)
	if body == "" {
		return PartPrefix{}, false
	}
	return PartPrefix{Part: int(part - '0'), Total: int(total - '0'), Body: body}, true
}

// isIDString reports whether every character of s is alphanumeric.
func isIDString(s string) bool {
	for i := 0; i < len(s); i++ {
		if !isIDByte(s[i]) {
			return false
		}
	}
	return true
}

// parseNumbering parses "n/m" from the inside of a prefix.
func parseNumbering(inner string) (PartPrefix, bool) {
	partText, totalText, ok := strings.Cut(inner, "/")
	if !ok || partText == "" || totalText == "" {
		return PartPrefix{}, false
	}
	part, err := strconv.Atoi(partText)
	if err != nil {
		return PartPrefix{}, false
	}
	total, err := strconv.Atoi(totalText)
	if err != nil {
		return PartPrefix{}, false
	}
	if part < 1 || total < 1 || part > total || total > 9 {
		return PartPrefix{}, false
	}
	return PartPrefix{Part: part, Total: total}, true
}

// IsMultiPart reports whether a body is one part of a set of two or more.
func IsMultiPart(text string) bool {
	p, ok := ParsePartPrefix(text)
	return ok && p.Total > 1
}

// AssemblyKey groups the parts of one message.
//
// The parts of a set arrive with *separate* message ids — the sender numbers
// each part as its own packet — so the id cannot be the key. Sender, recipient
// and total are what is shared, and it is the same key simple-ham-bbs uses for
// the same traffic, so the two applications agree about which packets belong
// together. The sampling window is what keeps two similar messages from
// merging: see assemblyTimeout.
func AssemblyKey(from, to string, total int) string {
	return strings.ToUpper(from) + "|" + strings.ToUpper(to) + "|" + strconv.Itoa(total)
}

const (
	// assemblyTimeout is how long a part waits for its siblings. A sender
	// transmits parts back to back; five minutes covers a slow network and
	// retransmissions without merging two messages a station sent an hour
	// apart with the same total.
	assemblyTimeout = 5 * time.Minute
	// assemblyCap bounds how many sets may be incomplete at once. A flood of
	// unmatched first parts must not become unbounded memory in the collector.
	assemblyCap = 512
)

// Assembler collects the parts of multi-part messages.
//
// It is not safe for concurrent use: the collector's single read loop owns it.
type Assembler struct {
	open map[string]*pendingMessage
	// now is injectable so tests can advance the clock without sleeping.
	now func() time.Time
}

type pendingMessage struct {
	parts map[int]Packet
	// ids are the archive row ids of the parts already stored, so the caller
	// can delete exactly those rows when the set completes. Matching by text
	// would delete somebody else's message: the same greeting is sent by many
	// stations.
	ids   map[int]int64
	first time.Time
	// updated is the last time any part arrived, which is what the timeout is
	// measured from: a sender retransmitting a lost part is still in progress.
	updated time.Time
}

// NewAssembler returns an empty assembler.
func NewAssembler(now func() time.Time) *Assembler {
	if now == nil {
		now = time.Now
	}
	return &Assembler{open: map[string]*pendingMessage{}, now: now}
}

// Add offers one part to an open set, given the archive row id the caller
// stored it under (0 when it was not stored).
//
// It returns the assembled message and the row ids of the parts it replaces, on
// the single call that completes the set; the second return is nil otherwise.
// The caller has already stored every part, so those rows must be removed: the
// archive counts a message once. The id of the part that completed the set is
// included only if it was stored before this call, which is not the normal
// case — the caller adds its own id.
func (a *Assembler) Add(p Packet, storedID int64) (assembled Packet, replacedIDs []int64, complete bool) {
	parsed, ok := ParsePartPrefix(p.Text)
	if !ok || parsed.Total <= 1 {
		return Packet{}, nil, false
	}

	key := AssemblyKey(p.From, p.To, parsed.Total)
	now := a.now()
	a.prune(now)

	set := a.open[key]
	if set == nil {
		if len(a.open) >= assemblyCap {
			// Refuse a new set rather than grow without bound. The part stays
			// stored on its own, which is the pre-existing behaviour and never
			// loses the content.
			return Packet{}, nil, false
		}
		set = &pendingMessage{parts: map[int]Packet{}, ids: map[int]int64{}, first: now}
		a.open[key] = set
	}
	set.parts[parsed.Part] = p
	if storedID > 0 {
		set.ids[parsed.Part] = storedID
	}
	set.updated = now

	// Complete when every part from 1..total has been seen in this window.
	bodies := make([]string, 0, parsed.Total)
	for i := 1; i <= parsed.Total; i++ {
		part, ok := set.parts[i]
		if !ok {
			return Packet{}, nil, false
		}
		bodies = append(bodies, trimPartBody(ParsePartBody(part.Text)))
	}
	delete(a.open, key)

	// Part one supplies the identity: its message id is the id of the whole
	// message, so a reader can find the set by the id the sender published.
	assembled = Packet{
		From:      p.From,
		To:        p.To,
		Text:      TruncateRunes(strings.Join(bodies, " "), assembledLimit),
		Kind:      p.Kind,
		MessageID: set.parts[1].MessageID,
	}
	for i := 1; i <= parsed.Total; i++ {
		if id, ok := set.ids[i]; ok {
			replacedIDs = append(replacedIDs, id)
		}
	}
	return assembled, replacedIDs, true
}

// trimPartBody removes the separator a sender puts between the numbering and the
// text.
//
// `[1/2]: Use previous info` and `[2/2]: Prev: ...` both carry a leading colon,
// which is part of the message format rather than of the text. Left in place,
// joining the parts produces ": ... : ..." — two stray separators where the
// reader expects one sentence continuing into the next.
func trimPartBody(body string) string {
	body = strings.TrimLeft(body, " ")
	for strings.HasPrefix(body, ":") {
		body = strings.TrimLeft(strings.TrimPrefix(body, ":"), " ")
	}
	return body
}

// ParsePartBody returns the body of a numbered part without its `[n/m]` prefix.
func ParsePartBody(text string) string {
	if p, ok := ParsePartPrefix(text); ok {
		return p.Body
	}
	return text
}

// assembledLimit bounds an assembled body. 67 is the single-packet limit; a set
// of parts is by definition longer, and the web column and the report exports
// size themselves from MaxTextLength. Eight parts of 67 characters is a
// realistic ceiling for the convention.
const assembledLimit = MaxTextLength * 8

// Pending reports how many incomplete sets are open, for the status endpoint.
func (a *Assembler) Pending() int { return len(a.open) }

// SetClock replaces the time source.
//
// It exists for replaying archived rows: those are fed in their stored order,
// and the assembler's timeout must be measured on that timeline rather than on
// the wall clock, or a pair stored days ago would be rejected as stale. It is
// only safe before the next Add.
func (a *Assembler) SetClock(now func() time.Time) {
	if now != nil {
		a.now = now
	}
}

// prune drops sets that have waited too long for the parts that never came.
func (a *Assembler) prune(now time.Time) {
	for key, set := range a.open {
		if now.Sub(set.updated) > assemblyTimeout {
			delete(a.open, key)
		}
	}
}
