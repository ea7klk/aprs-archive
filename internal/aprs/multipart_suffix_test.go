package aprs

import "testing"

// TestTrailingNumberingFromRealTraffic is the failure reported on 2026-11-13:
// three parts of one message from EA3JCH-13 to EB3CLX-3, numbered at the *end*
// of the body, stored as three separate messages because the parser only looked
// for numbering at the start.
func TestTrailingNumberingFromRealTraffic(t *testing.T) {
	parts := []struct {
		body  string
		part  int
		total int
		text  string
	}{
		{
			"A la xarxa quedarà enllaçat perquè els nostres nodes (1/3){4X}",
			1, 3, "A la xarxa quedarà enllaçat perquè els nostres nodes",
		},
		{
			"són igate; amb posar el digirepeater a Gardeny, les (2/3){4Y}",
			2, 3, "són igate; amb posar el digirepeater a Gardeny, les",
		},
		{
			"trames entraran a internet sense problemes (3/3){4Z}",
			3, 3, "trames entraran a internet sense problemes",
		},
	}
	for _, want := range parts {
		body, id := SplitMessageID(want.body)
		if id == "" {
			t.Fatalf("%q: the trailing id was not removed", want.body)
		}
		got, ok := ParsePartPrefix(body)
		if !ok {
			t.Errorf("%q: not recognised as a part", body)
			continue
		}
		if got.Part != want.part || got.Total != want.total {
			t.Errorf("%q: got %d/%d, want %d/%d", body, got.Part, got.Total, want.part, want.total)
		}
		if got.Body != want.text {
			t.Errorf("%q: body = %q, want %q", body, got.Body, want.text)
		}
	}
}

func TestTrailingNumberingAcceptance(t *testing.T) {
	cases := []struct {
		body  string
		part  int
		total int
		ok    bool
	}{
		// trailing numbering, with and without a message id
		{"les (2/3){4Y}", 2, 3, true},
		{"les (2/3)", 2, 3, true},
		{"les (2/3)  ", 2, 3, true},
		{"les (1/2){92}", 1, 2, true},
		{"word (9/9)", 9, 9, true},
		// a date, a ratio inside prose, an impossible part
		{"noche (13/11)", 0, 0, false},
		{"scored 3/4 (1/2) of the way", 0, 0, false},
		{"meeting at 18:30 (2/3) done", 0, 0, false},
		{"x2/3) glued to a word", 0, 0, false},
		{"backwards (3/2)", 0, 0, false},
		{"zero (0/2)", 0, 0, false},
		{"malformed (2/3){4Y", 0, 0, false},
		{"only numbering (2/3)", 2, 3, true},
		{"(2/3)", 0, 0, false},
		{"nothing here", 0, 0, false},
	}
	for _, c := range cases {
		got, ok := ParsePartPrefix(c.body)
		if ok != c.ok {
			t.Errorf("%q: ok = %v, want %v", c.body, ok, c.ok)
			continue
		}
		if ok && (got.Part != c.part || got.Total != c.total) {
			t.Errorf("%q: got %d/%d, want %d/%d", c.body, got.Part, got.Total, c.part, c.total)
		}
	}
}

// TestBracedMessageIDIsRemoved covers the third defect: the sender writes the id
// as `{4Y}` with a closing brace, while APRS101 defines `{4Y` without one.
func TestBracedMessageIDIsRemoved(t *testing.T) {
	body, id := SplitMessageID("A la xarxa ... nodes (1/3){4X}")
	if id != "4X" {
		t.Fatalf("id = %q, want 4X", id)
	}
	if body != "A la xarxa ... nodes (1/3)" {
		t.Fatalf("body = %q", body)
	}
	// A brace that is not an id stays in the text.
	if body, id := SplitMessageID("body {x} thing"); id != "" || body != "body {x} thing" {
		t.Fatalf("prose with braces was altered: %q %q", body, id)
	}
}

// TestThreePartSetAssembles is the whole reported case end to end at the parser
// level: the three bodies, in arrival order, complete one message.
func TestThreePartSetAssembles(t *testing.T) {
	bodies := []string{
		"A la xarxa quedarà enllaçat perquè els nostres nodes (1/3){4X}",
		"són igate; amb posar el digirepeater a Gardeny, les (2/3){4Y}",
		"trames entraran a internet sense problemes (3/3){4Z}",
	}
	a := NewAssembler(nil)
	var assembled Packet
	var complete bool
	for _, body := range bodies {
		p, ok := ParseRaw("EA3JCH-13>APRS::EB3CLX-3 :" + body)
		if !ok {
			t.Fatalf("%q did not parse as a packet", body)
		}
		var done bool
		assembled, _, done = a.Add(p, 0)
		if done {
			complete = true
		}
	}
	if !complete {
		t.Fatal("the three parts did not assemble")
	}
	want := "A la xarxa quedarà enllaçat perquè els nostres nodes són igate; amb posar el digirepeater a Gardeny, les trames entraran a internet sense problemes"
	if assembled.Text != want {
		t.Fatalf("assembled text =\n%q\nwant\n%q", assembled.Text, want)
	}
	if a.Pending() != 0 {
		t.Fatalf("pending = %d after completion", a.Pending())
	}
}
