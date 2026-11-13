package aprs

import (
	"testing"
	"time"
)

func TestParsePartPrefix(t *testing.T) {
	cases := []struct {
		text  string
		part  int
		total int
		body  string
		ok    bool
	}{
		{"[1/2]: Use previous info? YES / NO", 1, 2, ": Use previous info? YES / NO", true},
		{"[2/2]: Prev: P Endramawan", 2, 2, ": Prev: P Endramawan", true},
		{"[1/1] PR > NP4PC-2", 1, 1, "PR > NP4PC-2", true},
		{"[1/3]no space", 1, 3, "no space", true},
		{"[1/10] too many", 0, 0, "", false},
		{"[3/2] impossible", 0, 0, "", false},
		{"[0/2] zero", 0, 0, "", false},
		{"[a/b] not numeric", 0, 0, "", false},
		{"(1/2) wrong bracket", 0, 0, "", false},
		{"no prefix at all", 0, 0, "", false},
		{"[1/2]", 1, 2, "", true},
	}
	for _, c := range cases {
		got, ok := ParsePartPrefix(c.text)
		if ok != c.ok {
			t.Errorf("%q: ok = %v, want %v", c.text, ok, c.ok)
			continue
		}
		if !ok {
			continue
		}
		if got.Part != c.part || got.Total != c.total || got.Body != c.body {
			t.Errorf("%q: got %+v, want part %d/%d body %q", c.text, got, c.part, c.total, c.body)
		}
	}
}

// TestSinglePartIsNotMultiPart is the [1/1] trap, from real traffic: one station
// sent twenty-one different messages to twenty-one recipients, all marked
// [1/1]. They are complete messages, not parts of a set.
func TestSinglePartIsNotMultiPart(t *testing.T) {
	for _, text := range []string{"[1/1] PR > NP4PC-2 > Buenos dias PR 73", "[1/1] only"} {
		if IsMultiPart(text) {
			t.Errorf("%q treated as multi-part", text)
		}
		p := Packet{From: "GRP", To: "WP4QNQ-9", Text: text, Kind: KindMessage}
		a := NewAssembler(nil)
		if _, _, complete := a.Add(p, 0); complete {
			t.Errorf("%q assembled into a set", text)
		}
		if a.Pending() != 0 {
			t.Errorf("%q opened a pending set", text)
		}
	}
}

func TestAssemblerJoinsPartsInOrder(t *testing.T) {
	tm := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	a := NewAssembler(func() time.Time { return tm })

	// Parts arrive out of order, with separate message ids, as on the air.
	first := Packet{From: "PKTNET", To: "YD2UNL-9", Text: "[2/2]: Prev: P Endramawan", Kind: KindMessage, MessageID: "1672"}
	second := Packet{From: "PKTNET", To: "YD2UNL-9", Text: "[1/2]: Use previous info? YES / NO", Kind: KindMessage, MessageID: "1671"}

	if _, _, complete := a.Add(first, 0); complete {
		t.Fatal("the second half alone completed a set")
	}
	assembled, replaced, complete := a.Add(second, 0)
	if !complete {
		t.Fatal("the set did not complete when the last part arrived")
	}
	want := "Use previous info? YES / NO Prev: P Endramawan"
	if assembled.Text != want {
		t.Fatalf("assembled text = %q, want %q", assembled.Text, want)
	}
	// The assembled message keeps part one's id, so it can be found by id.
	if assembled.MessageID != "1671" {
		t.Fatalf("assembled id = %q, want the id of part one", assembled.MessageID)
	}
	if assembled.From != "PKTNET" || assembled.To != "YD2UNL-9" {
		t.Fatalf("assembled addresses changed: %+v", assembled)
	}
	// No ids were supplied, so nothing is reported for deletion.
	if len(replaced) != 0 {
		t.Fatalf("replaced = %v, want none when no rows were stored", replaced)
	}
	if a.Pending() != 0 {
		t.Fatal("the completed set is still pending")
	}
}

func TestAssemblerDoesNotMergeDifferentRecipients(t *testing.T) {
	// The real [1/1] station addressed twenty-one recipients; the same trap
	// applies to a genuine set sent to two people at once.
	tm := time.Now()
	a := NewAssembler(func() time.Time { return tm })
	a.Add(Packet{From: "S", To: "A", Text: "[1/2] first", Kind: KindMessage}, 0)
	if _, _, complete := a.Add(Packet{From: "S", To: "B", Text: "[2/2] second", Kind: KindMessage}, 0); complete {
		t.Fatal("parts addressed to different stations were merged")
	}
	if a.Pending() != 2 {
		t.Fatalf("pending = %d, want 2 independent sets", a.Pending())
	}
}

func TestAssemblerDropsStaleSets(t *testing.T) {
	clock := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	a := NewAssembler(func() time.Time { return clock })
	a.Add(Packet{From: "S", To: "R", Text: "[1/3] one", Kind: KindMessage}, 0)
	if a.Pending() != 1 {
		t.Fatal("set not opened")
	}
	// Longer than the assembly window: a late second part must not join it.
	clock = clock.Add(assemblyTimeout + time.Minute)
	a.Add(Packet{From: "S", To: "R", Text: "[2/3] two", Kind: KindMessage}, 0)
	if a.Pending() != 1 {
		t.Fatalf("pending = %d, want 1 (the stale set replaced, not merged)", a.Pending())
	}
}

func TestAssemblerIsBounded(t *testing.T) {
	tm := time.Now()
	a := NewAssembler(func() time.Time { return tm })
	for i := 0; i < assemblyCap+50; i++ {
		a.Add(Packet{From: "S", To: "R" + string(rune('A'+i%26)) + string(rune('a'+i/26)), Text: "[1/3] x", Kind: KindMessage}, 0)
	}
	if a.Pending() > assemblyCap {
		t.Fatalf("pending = %d, want at most %d", a.Pending(), assemblyCap)
	}
}

func TestAssemblerPassesThroughOrdinaryMessages(t *testing.T) {
	a := NewAssembler(nil)
	p := Packet{From: "A", To: "B", Text: "hello", Kind: KindMessage}
	if _, _, complete := a.Add(p, 0); complete {
		t.Fatal("an ordinary message reported as assembled")
	}
}

// TestAssemblerReportsTheStoredRowsToDelete pins that replacement is by row id:
// two stations send the same greeting, and a text match would delete the wrong
// message.
func TestAssemblerReportsTheStoredRowsToDelete(t *testing.T) {
	tm := time.Now()
	a := NewAssembler(func() time.Time { return tm })
	a.Add(Packet{From: "S", To: "R", Text: "[1/2] same text", Kind: KindMessage}, 101)
	_, replaced, complete := a.Add(Packet{From: "S", To: "R", Text: "[2/2] same text", Kind: KindMessage}, 102)
	if !complete {
		t.Fatal("set did not complete")
	}
	if len(replaced) != 2 || replaced[0] != 101 || replaced[1] != 102 {
		t.Fatalf("replaced ids = %v, want [101 102]", replaced)
	}
}
