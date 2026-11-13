package aprs

import "testing"

func TestParseRawMessage(t *testing.T) {
	got, ok := ParseRaw("EA7KLK-9>APRS,TCPIP*,qAC,T2SPAIN::EA7KLK-15:Hello there{ABC12")
	if !ok {
		t.Fatal("packet not parsed")
	}
	want := Packet{From: "EA7KLK-9", To: "EA7KLK-15", Text: "Hello there", Kind: KindMessage, MessageID: "ABC12"}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestParseRawStripsPaddingFromAddressee(t *testing.T) {
	got, ok := ParseRaw("N0CALL>APRS::EA7KLK   :hi")
	if !ok {
		t.Fatal("packet not parsed")
	}
	if got.To != "EA7KLK" {
		t.Fatalf("To = %q, want EA7KLK", got.To)
	}
}

func TestParseRawBulletin(t *testing.T) {
	got, ok := ParseRaw("N0CALL>APRS::BLN1NWS  :Tornado warning until 21:00Z")
	if !ok {
		t.Fatal("bulletin not parsed")
	}
	if got.Kind != KindBulletin {
		t.Fatalf("Kind = %q, want bulletin", got.Kind)
	}
	if got.Bulletin.Slot != 1 || got.Bulletin.Group != "NWS" {
		t.Fatalf("Bulletin = %+v, want slot 1 group NWS", got.Bulletin)
	}
	if got.To != "BLN1NWS" {
		t.Fatalf("To = %q", got.To)
	}
}

func TestParseRawBareBulletinAddress(t *testing.T) {
	got, ok := ParseRaw("N0CALL>APRS::BLN3     :test")
	if !ok || got.Kind != KindBulletin {
		t.Fatalf("bare BLN address not treated as bulletin: %+v %v", got, ok)
	}
	if got.Bulletin.Group != "" || got.Bulletin.Slot != 3 {
		t.Fatalf("Bulletin = %+v", got.Bulletin)
	}
}

func TestParseRawAck(t *testing.T) {
	got, ok := ParseRaw("EA7KLK-9>APRS::EA7KLK-15:ack12345")
	if !ok {
		t.Fatal("ack not parsed")
	}
	if got.Kind != KindAck || got.AckID != "12345" {
		t.Fatalf("got %+v", got)
	}
}

func TestParseRawAckCaseInsensitive(t *testing.T) {
	got, ok := ParseRaw("A>APRS::B        :ACKABC{77")
	if !ok || got.Kind != KindAck || got.AckID != "ABC" {
		t.Fatalf("got %+v ok=%v", got, ok)
	}
}

func TestParseRawRej(t *testing.T) {
	got, ok := ParseRaw("EA7KLK-9>APRS::EA7KLK-15:rej99")
	if !ok || got.Kind != KindRej || got.AckID != "99" {
		t.Fatalf("got %+v ok=%v", got, ok)
	}
}

func TestParseRawRejectsNonMessage(t *testing.T) {
	cases := []string{
		"EA7KLK>APRS:!4020.00N/00340.00W>hello",          // position report
		"EA7KLK>APRS:>status text",                       // status
		"EA7KLK>APRS:}N0CALL>APRS,TCPIP::X       :inner", // third-party wrapper
		"",
		"garbage",
		"EA7KLK>APRS::SHORT:x", // addressee too short
	}
	for _, c := range cases {
		if _, ok := ParseRaw(c); ok {
			t.Errorf("ParseRaw(%q) accepted a non-message", c)
		}
	}
}

func TestParseRawRejectsEmptyBody(t *testing.T) {
	if _, ok := ParseRaw("A>APRS::B        :"); ok {
		t.Fatal("empty body accepted")
	}
}

func TestParseRawTruncatesOverlongBody(t *testing.T) {
	long := ""
	for i := 0; i < 100; i++ {
		long += "x"
	}
	got, ok := ParseRaw("A>APRS::B        :" + long)
	if !ok {
		t.Fatal("not parsed")
	}
	if n := len([]rune(got.Text)); n != MaxTextLength {
		t.Fatalf("body length = %d, want %d", n, MaxTextLength)
	}
}

func TestSplitMessageIDLeavesBracesInText(t *testing.T) {
	cases := map[string]string{
		"hello world":       "hello world",
		"has {braces} here": "has {braces} here",
		"body{12":           "body",
		"body{":             "body{",
		"body{TOOLONGID":    "body{TOOLONGID",
	}
	for in, want := range cases {
		body, _ := SplitMessageID(in)
		if body != want {
			t.Errorf("SplitMessageID(%q) = %q, want %q", in, body, want)
		}
	}
}

func TestParseBulletinAddressRejectsLongGroup(t *testing.T) {
	if _, ok := ParseBulletinAddress("BLN1NWSXXX"); ok {
		t.Fatal("six-character group accepted")
	}
	if _, ok := ParseBulletinAddress("BLNX"); ok {
		t.Fatal("non-digit slot accepted")
	}
	if _, ok := ParseBulletinAddress("N0CALL"); ok {
		t.Fatal("callsign treated as bulletin")
	}
}

func TestPasscodeKnownValue(t *testing.T) {
	// The APRS-IS documentation gives 13023 for N0CALL.
	if got := Passcode("N0CALL"); got != 13023 {
		t.Fatalf("Passcode(N0CALL) = %d, want 13023", got)
	}
	if got := Passcode("EA7KLK-15"); got != Passcode("EA7KLK") {
		t.Fatalf("SSID changed the passcode: %d", got)
	}
}

func TestParentCallsign(t *testing.T) {
	cases := map[string]string{
		"EA7KLK-15": "EA7KLK-0",
		"EA7KLK":    "EA7KLK",
		"N0CALL-1":  "N0CALL-0",
	}
	for in, want := range cases {
		if got := ParentCallsign(in); got != want {
			t.Errorf("ParentCallsign(%q) = %q, want %q", in, got, want)
		}
	}
}
