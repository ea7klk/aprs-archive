package telemetry

import (
	"testing"

	"aprs-archive/internal/store"
)

func seeded(t *testing.T) *Matcher {
	t.Helper()
	var items []Source
	for i, f := range store.DefaultMessageFilters {
		items = append(items, Source{ID: int64(i + 1), Name: f.Name, Pattern: f.Pattern})
	}
	return Compile(items, 1)
}

// TestCatchesObservedTelemetry uses the exact bodies the operator reported plus
// the other families found by scanning the archive.
func TestCatchesObservedTelemetry(t *testing.T) {
	m := seeded(t)
	junk := []string{
		"[S74.6 L8 V0 B0 A0 R1 G0 T14 M42 Q19.2 K14 D0 U4.20 C1 W1]~7d", // IZ6RND
		"[S72.2 L9 V0 B0 A0 R1 G0 T14 M47 Q19.2 K14 D0 U4.20 C1 W1]~05", // DL6TJ-9
		"uptime: 1d 19h 37m 52s", // SV9TNF-3
		"9.3,99.9",               // PE1RXF-6
		"7.8,99.9",               // PE1RXF-6
		"270.0,1.8,3.5,0.0,25.1,7.7,99.9,1009.5,7.8,0.0,107.0", // PE1RXF-13
		"99,13.86,0.0,6.58", // PE1RXF-9
		"T=11:47:59 A=29.6 B=0.96 C=0.92 RF=1 RP=0 RS=0 F=14.065000 TP=25 TD", // IZ6RND
		"UPTM.13T06:36:40",  // YO3JAZ-EY
		"UPTM.174d05:30:35", // YO3JAZ-NC
		"UPTIME=up 1 week, 4 days, 15 hours, 56 minutes v20250629", // AE5TC-13
		"WX: T=26C H=78% W=0.5m/s P=1014hPa",                       // HS3LIQ-13
	}
	for _, body := range junk {
		if _, ok := m.Match(body); !ok {
			t.Errorf("not filtered: %q", body)
		}
	}
}

// TestKeepsRealConversation is the constraint that matters most: a filter that
// eats a person's message deletes something that cannot be recovered.
func TestKeepsRealConversation(t *testing.T) {
	m := seeded(t)
	real := []string{
		"73",
		"73!",
		"CQ APRSMX LA GUAJIRA",
		"LA GUAJIRA recibido OK 13/11/2026 11:47Z PKT 2m 14",
		"Test 1",
		"hello from Spain",
		"A la xarxa quedarà enllaçat perquè els nostres nodes són igate",
		"Merry Christmas and a Happy New Year from all of us",
		"QRV on 144.390, see you there",
		"Thanks for the QSO, 73 de EA7KLK",
		"Meeting tonight at 8pm in the club room",
		"Clave incorrecta. Use: CQ KILNOY (o INFO/WEB)",
		"ALERTA AMARILLA POR TORMENTAS (Viernes por la tarde y noche (13/11)",
		"WXBOT: tornado warning for the metro area",
		"T=26 is the temperature outside today",
		"1,2,3 let's go",
		"L+12283+10282+9181+8380",
	}
	for _, body := range real {
		if r, ok := m.Match(body); ok {
			t.Errorf("filtered a real message %q by rule %q", body, r.Name)
		}
	}
}

// TestNumericSeriesEndsAtTheNumbers is the line between a reading and a
// sentence: a body that is only numbers is machine output, a body that starts
// with numbers and continues as prose is a person.
func TestNumericSeriesEndsAtTheNumbers(t *testing.T) {
	m := seeded(t)
	for _, body := range []string{"9.3,99.9", "7.8,99.9", "9,9", "270.0,1.8,3.5"} {
		if _, ok := m.Match(body); !ok {
			t.Errorf("a numeric-only body was kept: %q", body)
		}
	}
	for _, body := range []string{"1,2,3 let's go", "73, see you", "9,9 is my locator"} {
		if r, ok := m.Match(body); ok {
			t.Errorf("%q was filtered by %q but it is prose", body, r.Name)
		}
	}
}

func TestEmptyMatcherKeepsEverything(t *testing.T) {
	var m *Matcher
	if _, ok := m.Match("[S74.6 L8 V0]"); ok {
		t.Fatal("a nil matcher filtered a message")
	}
	if _, ok := Compile(nil, 1).Match("[S74.6 L8 V0]"); ok {
		t.Fatal("a matcher with no rules filtered a message")
	}
}

func TestBrokenPatternIsReportedNotFatal(t *testing.T) {
	m := Compile([]Source{
		{ID: 1, Name: "good", Pattern: `^[A-Z]{3}=[0-9]+`},
		{ID: 2, Name: "bad", Pattern: `([unclosed`},
	}, 1)
	if m.Count() != 1 {
		t.Fatalf("%d rules active, want 1", m.Count())
	}
	if len(m.Broken()) != 1 {
		t.Fatalf("broken = %v, want the one bad pattern", m.Broken())
	}
	if _, ok := m.Match("ABC=123"); !ok {
		t.Fatal("the good rule stopped working because of a bad neighbour")
	}
}

func TestValidateRejectsBadPattern(t *testing.T) {
	if err := Validate(`([unclosed`); err == nil {
		t.Fatal("an invalid pattern was accepted")
	}
	if err := Validate(""); err == nil {
		t.Fatal("an empty pattern was accepted")
	}
	if err := Validate(`^UPTIME=`); err != nil {
		t.Fatalf("a valid pattern was rejected: %v", err)
	}
}

// TestBroadRulesMatchWhenEnabled covers the two rules that ship switched off.
// They are in the seeded set so an operator can turn them on, but they must not
// affect the default matcher.
func TestBroadRulesMatchWhenEnabled(t *testing.T) {
	var all []Source
	var enabledOnly []Source
	for i, f := range store.DefaultMessageFilters {
		src := Source{ID: int64(i + 1), Name: f.Name, Pattern: f.Pattern}
		all = append(all, src)
		if f.EnabledByDefault {
			enabledOnly = append(enabledOnly, src)
		}
	}
	def := Compile(enabledOnly, 1)
	full := Compile(all, 1)

	checksum := "ok ST [S92.6 L123 V4 B85 A0 R1 G0 T9 M527 D0 U4.20 C1 W1]~89"
	if _, ok := def.Match(checksum); ok {
		t.Fatal("the checksum rule is enabled by default; it should be opt-in")
	}
	if _, ok := full.Match(checksum); !ok {
		t.Fatal("the checksum rule does not match when enabled")
	}
	dump := "M46/79 135:0u0000042s00s000u0000042s00s0"
	if _, ok := def.Match(dump); ok {
		t.Fatal("the memory-dump rule is enabled by default")
	}
	if _, ok := full.Match(dump); !ok {
		t.Fatal("the memory-dump rule does not match when enabled")
	}
	// And the broad rules still leave prose alone.
	if _, ok := full.Match("73 de EA7KLK"); ok {
		t.Fatal("a sign-off was filtered")
	}
}
