// Package aprs parses APRS-IS lines far enough to archive message traffic.
//
// The archive stores APRS *messages*, not the whole feed: position reports,
// weather beacons, objects and telemetry are filtered out at the source
// (`filter t/m`) and are not modelled here. A message-format packet is the only
// thing this package understands, and it understands it as raw bytes, not as a
// decoded station: the archive never needs to know whether a sender is a
// tracker or a human.
//
// The parsing rules follow APRS101 chapter 14 and match what simple-ham-bbs
// already does for the same traffic, so the two applications agree about what a
// bulletin is, what a message ID is, and when a packet is an acknowledgement.
package aprs

import (
	"strconv"
	"strings"
)

// Kind classifies a message packet. The archive stores it so the web interface
// can separate human conversation from protocol noise and broadcast bulletins.
type Kind string

const (
	// KindMessage is an ordinary message addressed to a station.
	KindMessage Kind = "message"
	// KindBulletin is a broadcast to `BLNn` plus a group.
	KindBulletin Kind = "bulletin"
	// KindAck acknowledges a message ID it previously received.
	KindAck Kind = "ack"
	// KindRej rejects a message ID it previously received.
	KindRej Kind = "rej"
)

// Address is a parsed bulletin addressee: `BLNn` plus a group.
//
// The field is nine characters, space padded:
//
//	:BLN1NWS  :Tornado warning until 21:00Z for the metro area
//	 ^^^ ^^^^
//	 slot group
type Address struct {
	Slot  int
	Group string
}

// Packet is one archived message-format packet.
type Packet struct {
	// From is the source callsign including any SSID, upper case.
	From string
	// To is the addressee: a callsign, a bulletin address, or a software group.
	To string
	// Text is the message body with the message ID and trailing padding
	// removed. For an ack or rej this is the original id token.
	Text string
	// Kind classifies the packet.
	Kind Kind
	// MessageID is the sender's message ID, if the packet carried one.
	MessageID string
	// AckID is the message ID being acknowledged (KindAck) or rejected (KindRej).
	AckID string
	// Bulletin is set when Kind is KindBulletin.
	Bulletin Address
}

// MaxTextLength is the APRS101 limit for a message body in characters. The
// archive sizes its columns from this.
const MaxTextLength = 67

// addressLen is the fixed length of the APRS message addressee field.
const addressLen = 9

// ParseRaw parses one APRS-IS line and reports whether it is a message packet.
//
// The line format is `SOURCE>DEST,PATH:INFORMATION`, where a message packet
// begins with a colon, a nine-character addressee, and a second colon. Lines
// that are not messages — position reports, status, objects, third-party
// wrappers — are rejected here rather than stored and filtered later.
func ParseRaw(line string) (Packet, bool) {
	line = strings.TrimRight(line, "\r\n")
	header, info, ok := strings.Cut(line, ":")
	if !ok || !strings.HasPrefix(info, ":") || len(info) < addressLen+2 {
		return Packet{}, false
	}
	if info[addressLen+1] != ':' {
		return Packet{}, false
	}
	source, _, ok := strings.Cut(header, ">")
	if !ok {
		return Packet{}, false
	}
	from := NormalizeCallsign(source)
	to := NormalizeCallsign(info[1 : addressLen+1])
	body := strings.TrimSpace(info[addressLen+2:])
	if from == "" || to == "" || body == "" {
		return Packet{}, false
	}

	p := Packet{From: from, To: to}

	// Acknowledgements are checked before the message ID is split, because an
	// ack body itself ends with `{<id>` and would otherwise look like a message
	// whose text is just "ack".
	if id, ok := responseID(body, "ack"); ok {
		p.Kind = KindAck
		p.AckID = id
		p.Text = NormalizeMessageID(id)
		return p, true
	}
	if id, ok := responseID(body, "rej"); ok {
		p.Kind = KindRej
		p.AckID = id
		p.Text = NormalizeMessageID(id)
		return p, true
	}

	text, messageID := SplitMessageID(body)
	if text == "" {
		return Packet{}, false
	}
	p.Text = TruncateRunes(text, MaxTextLength)
	p.MessageID = messageID

	if addr, isBulletin := ParseBulletinAddress(to); isBulletin {
		p.Kind = KindBulletin
		p.Bulletin = addr
		return p, true
	}
	p.Kind = KindMessage
	return p, true
}

// ParseBulletinAddress reports whether an addressee is a bulletin address and
// returns its slot and group.
//
// The field is `BLNn` followed by up to five group characters. A bare `BLNn` is
// valid — it simply has no group — and so is a group shorter than five
// characters. Anything longer than five is a different addressee.
func ParseBulletinAddress(to string) (Address, bool) {
	to = strings.ToUpper(strings.TrimSpace(to))
	if len(to) < 4 || !strings.HasPrefix(to, "BLN") {
		return Address{}, false
	}
	if to[3] < '0' || to[3] > '9' {
		return Address{}, false
	}
	slot, err := strconv.Atoi(string(to[3]))
	if err != nil {
		return Address{}, false
	}
	group := strings.ToUpper(strings.TrimSpace(to[4:]))
	if len(group) > 5 {
		return Address{}, false
	}
	return Address{Slot: slot, Group: group}, true
}

// SplitMessageID separates a sender's message ID from the body.
//
// APRS appends `{XXXXX` where the id is one to five alphanumeric characters.
// A `{` that does not carry such an id is part of the text, not an id.
func SplitMessageID(text string) (string, string) {
	text = strings.TrimSpace(text)
	idx := strings.LastIndex(text, "{")
	if idx < 0 {
		return text, ""
	}
	id := text[idx+1:]
	// APRS101 writes the id as `{4X` with no closing brace, but clients send
	// `{4X}` as well. Accepting the brace is what lets a part be recognised as
	// a part: the archive saw `... les (2/3){4Y}` and stored the id as text.
	braced := strings.HasSuffix(id, "}")
	if braced {
		id = strings.TrimSuffix(id, "}")
	}
	if id == "" || len([]rune(id)) > 5 {
		return text, ""
	}
	for _, r := range id {
		if !isIDRune(r) {
			return text, ""
		}
	}
	return strings.TrimSpace(text[:idx]), NormalizeMessageID(id)
}

// StripMessageID returns the body without a trailing message ID.
func StripMessageID(text string) string {
	body, _ := SplitMessageID(text)
	return body
}

// IsResponse reports whether text is a bare ack or rej with no id, which some
// clients send for messages they received without one.
func IsResponse(text string) bool {
	if _, ok := responseID(text, "ack"); ok {
		return true
	}
	_, ok := responseID(text, "rej")
	return ok
}

// NormalizeMessageID upper-cases an id and keeps at most five characters.
func NormalizeMessageID(id string) string {
	id = strings.TrimSpace(id)
	if len(id) > 5 {
		id = id[:5]
	}
	var b strings.Builder
	for _, r := range id {
		if isIDRune(r) {
			b.WriteRune(toUpper(r))
		}
	}
	return b.String()
}

// TruncateRunes cuts s to at most n runes, without splitting a multi-byte
// character. APRS bodies are ASCII in practice, but a misbehaving client can
// send UTF-8 and the column must not be cut mid-rune.
func TruncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}

// responseID recognises an acknowledgement or a rejection and returns the id it
// refers to (possibly empty, for the bare "ack" and "rej" some clients send).
//
// APRS101 defines the format as `ack<id>` / `rej<id>`, and recommends lower
// case. Real traffic is looser, and the archive has to recognise all of it:
//
//	ack4A}   ack49}   ackJU}JT   ack   ACK   rej99}   ack{12}   ack 12
//
// The rule is: the body starts with ack or rej, a whole keyword, and what
// follows is an id-like token — up to five alphanumeric characters, optionally
// introduced by `{` and followed by anything. That excludes prose, because a
// word that merely begins with the keyword continues with letters rather than
// ending the keyword: "acknowledge" and "rejoined" have a longer alphanumeric
// run, and "rejoin now" has a run that ends in the middle of the word rather
// than at a separator.
//
// One case stays ambiguous and is decided in favour of the sender: a bare
// keyword followed by a space and a word ("ack the signal is loud") is a
// message, not an acknowledgement of nothing.
func responseID(text, prefix string) (string, bool) {
	text = strings.TrimSpace(text)
	if len(text) < len(prefix) {
		return "", false
	}
	if !strings.EqualFold(text[:len(prefix)], prefix) {
		return "", false
	}
	rest := text[len(prefix):]
	if rest == "" {
		// A bare acknowledgement: valid, with no id.
		return "", true
	}

	// Optional `{` introducing the id, as in "ack{12}" and "ACKQP{12". The
	// brace delimits the id, so its case does not matter: "ack{ab1}" is an id
	// even though a bare lower-case run would be read as a word.
	rest = strings.TrimSpace(rest)
	braced := false
	if strings.HasPrefix(rest, "{") {
		braced = true
		rest = rest[1:]
		end := 0
		for end < len(rest) && isIDByte(rest[end]) {
			end++
		}
		if end == 0 || end > 5 {
			// "ack{" with nothing, or a brace around a word.
			return "", end == 0 && braced
		}
		return NormalizeMessageID(rest[:end]), true
	}
	switch {
	case rest == "":
		return "", braced // "ack{" with nothing after it is still an ack
	case rest[0] == ' ' || rest[0] == '\t':
		// "ack 12" is an id; "ack now" is prose. A single token of at most five
		// alphanumeric characters is an id.
		token := strings.Fields(rest)
		if len(token) == 0 {
			return "", false
		}
		candidate := token[0]
		if len(candidate) > 5 || !allIDBytes(candidate) {
			return "", false
		}
		return NormalizeMessageID(candidate), true
	}

	// "ack4A}", "ack49}", "ackJU}JT", "rej99}".
	run := 0
	lowerSeen := false
	for run < len(rest) && isIDByte(rest[run]) {
		if rest[run] >= 'a' && rest[run] <= 'z' {
			lowerSeen = true
		}
		run++
	}
	if run > 5 {
		return "", false
	}
	if run > 0 && lowerSeen {
		// A run with lower-case letters in it. An id is conventionally upper
		// case; a lower-case run is the middle of a word ("rejoin", "ackerly").
		// The only lower-case ids that exist in practice are quoted in braces
		// ("ack{ab1"), and those took the braced path above.
		return "", false
	}
	if run == 0 {
		// Punctuation straight after the keyword: "ack}".
		return "", isKeywordAlone(rest)
	}
	return NormalizeMessageID(rest[:run]), true
}

// allIDBytes reports whether every byte of s is alphanumeric.
func allIDBytes(s string) bool {
	for i := 0; i < len(s); i++ {
		if !isIDByte(s[i]) {
			return false
		}
	}
	return true
}

// isKeywordAlone reports whether the text after "ack" or "rej" is punctuation
// rather than prose. "ack}" and "ack >" are acknowledgements; "acknowledge the
// message" never reaches here (its prefix match consumes the word), but "ack
// now" must stay a message.
func isKeywordAlone(rest string) bool {
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return true
	}
	for _, r := range rest {
		if r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return false
		}
	}
	return true
}

func isIDByte(b byte) bool {
	return (b >= '0' && b <= '9') || (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z')
}

// cutPrefixLower matches an ack or rej keyword case-insensitively and returns
// what follows it.
//
// Two things make the keyword a token rather than the start of a word:
//
//   - **Boundary after the keyword.** An ack or rej is written `ack<id>`,
//     `ack<id>}`, `ack{<id>}`, `ack <id>` or `ack` alone. A letter directly
//     after the keyword means a word: "acknowledge" and "rejoined" are not
//     acknowledgements.
//   - **Boundary after the id.** The id is at most five alphanumeric
//     characters and must be followed by punctuation, whitespace or the end of
//     the packet. That is what separates `ack4A}` (an id) from `acknowledge`
//     (an eleven-character word): the run of alphanumeric characters after the
//     keyword is checked for its length and its terminator together.
//
// One consequence, accepted deliberately: `ackJU}` — a mixed-case id with no
// separator before the closing brace is fine, but `ackJU` with nothing after it
// is ambiguous with a word and is treated as a message. A false negative costs
// one stored packet; a false positive deletes somebody's message.
func cutPrefixLower(text, prefix string) (string, bool) {
	if len(text) < len(prefix) {
		return "", false
	}
	if !strings.EqualFold(text[:len(prefix)], prefix) {
		return "", false
	}
	rest := text[len(prefix):]
	if rest == "" {
		return rest, true
	}
	// Take the alphanumeric run that follows the keyword. An id is a token,
	// so its case is mixed freely ("4A", "JU", "QP"); a word is not.
	run := 0
	upperSeen := false
	for run < len(rest) && isIDByte(rest[run]) {
		if rest[run] >= 'A' && rest[run] <= 'Z' {
			upperSeen = true
		}
		run++
	}
	terminated := run < len(rest) // something other than an id character follows
	if run > 5 {
		return "", false
	}
	if run > 0 && !upperSeen {
		// All lower case: "rejoined" would otherwise read as an id. An id
		// written entirely in lower case with no separator is indistinguishable
		// from a word, and the archive does not delete words.
		return "", false
	}
	if run > 0 && !terminated {
		return "", false
	}
	if run == 0 {
		// No id: the keyword must stand alone or be followed by punctuation.
		switch rest[0] {
		case ' ', '\t', '{', '}', '!', '>', '.', ':', '-', ',':
			return rest, true
		}
		return "", false
	}
	return rest, true
}

func isIDRune(r rune) bool {
	return (r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z')
}

func toUpper(r rune) rune {
	if r >= 'a' && r <= 'z' {
		return r - ('a' - 'A')
	}
	return r
}

// NormalizeCallsign upper-cases and trims a callsign as it appears on the wire.
//
// It does not validate: the archive stores what it hears, including malformed
// source addresses from bad clients, because an archive that silently drops
// packets is worse than one that shows an odd callsign.
func NormalizeCallsign(callsign string) string {
	return strings.ToUpper(strings.TrimSpace(callsign))
}

// Passcode computes the APRS-IS verification code for a base callsign.
//
// It is the documented algorithm: start from 0x73E2 and alternate each
// character of the base callsign into the high and low byte, then mask to 15
// bits. The archive connects receive-only, but APRS-IS still expects a passcode
// for a validated login.
func Passcode(callsign string) int {
	base := BaseCallsign(callsign)
	code := 0x73e2
	for i := 0; i < len(base); i++ {
		if i%2 == 0 {
			code ^= int(base[i]) << 8
		} else {
			code ^= int(base[i])
		}
	}
	return code & 0x7fff
}

// BaseCallsign strips the SSID from a callsign.
func BaseCallsign(callsign string) string {
	base, _, _ := strings.Cut(NormalizeCallsign(callsign), "-")
	return base
}

// ParentCallsign returns `BASE-N` for an SSID, or the callsign unchanged when it
// has no SSID or the SSID is not a negative number.
func ParentCallsign(callsign string) string {
	for i := len(callsign) - 1; i >= 0; i-- {
		if callsign[i] == '-' {
			if i == 0 || i < len(callsign)-1 {
				return callsign[:i] + "-0"
			}
			return callsign
		}
	}
	return callsign
}
