package aprs

import "testing"

// TestResponseRecognition uses the exact bodies seen on APRS-IS on 2026-11-13
// plus the prose they could be confused with.
//
// The first group is what the archive was storing as ordinary messages: the old
// recogniser only accepted `ack<id>` with nothing after it, so a trailing brace
// or a `{id` suffix turned a receipt into a message. The second group is why the
// rule cannot simply be "starts with ack or rej": APRS is a chat network, and
// people write sentences that begin with those letters.
func TestResponseRecognition(t *testing.T) {
	cases := []struct {
		body string
		want Kind
	}{
		// Acknowledgements and rejections, as they actually arrive.
		{"ack4A}", KindAck},
		{"ack49}", KindAck},
		{"ackJU}JT", KindAck},
		{"ack", KindAck},
		{"ACK", KindAck},
		{"rej99}", KindRej},
		{"rej", KindRej},
		{"ack{12", KindAck},
		{"ack{ab1}", KindAck},
		{"ACKQP{12", KindAck},
		{"ack 12", KindAck},
		{"ack}", KindAck},
		{"ackJU", KindAck},

		// Prose that merely begins with those letters.
		{"acknowledge the message please", KindMessage},
		{"acknowledged", KindMessage},
		{"rejoin now", KindMessage},
		{"reject that", KindMessage},
		{"rejoined", KindMessage},
		{"acknowledge", KindMessage},
		{"ack999999", KindMessage},
		{"ack please", KindMessage},
		{"rejoin the net at 8pm", KindMessage},
		{"ackerly on 40m", KindMessage},
		{"ACK, see you there", KindMessage},
	}
	for _, c := range cases {
		line := "EA3JCH-13>APRS,TCPIP*::EB3CLX-3 :" + c.body
		p, ok := ParseRaw(line)
		if !ok {
			t.Errorf("%q was not parsed at all", c.body)
			continue
		}
		if p.Kind != c.want {
			t.Errorf("%q classified as %s, want %s", c.body, p.Kind, c.want)
		}
	}
}

// TestResponseIDReturnsTheIdentifier checks that a recognised receipt carries
// the id it refers to, since that is the only part worth keeping.
func TestResponseIDReturnsTheIdentifier(t *testing.T) {
	cases := map[string]string{
		"ack4A}":   "4A",
		"ackJU}JT": "JU",
		"rej99}":   "99",
		"ack{12}":  "12",
		"ack 12":   "12",
		"ACKQP{12": "QP",
	}
	for body, wantID := range cases {
		p, ok := ParseRaw("A>APRS::B        :" + body)
		if !ok {
			t.Errorf("%q not parsed", body)
			continue
		}
		if p.AckID != wantID {
			t.Errorf("%q: AckID = %q, want %q", body, p.AckID, wantID)
		}
	}
}
