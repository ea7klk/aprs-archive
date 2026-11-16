package telemetry

import (
	"testing"

	"aprs-archive/internal/store"
)

// TestEncodedBlobCatchesTheObservedPayloads uses the real bodies from the
// LA4HQ <-> LA3HQ exchange that motivated the detector.
func TestEncodedBlobCatchesTheObservedPayloads(t *testing.T) {
	blobs := []string{
		"2NbMAEKDSEp5R7up8X84D83W5KDr9EPJ4SycmVdwuQ",
		"yegE8YcwFwagwmCBe/IIyUrludmU5GOt/UtEkvwyE+sHExSB",
		"BZwHZOw3wMUEGzUoOkQ8BQzAtFGJLBszzFndG4vE6eyD",
		";2j0hOp63Yd8L7CJoyE1OimaLBmC9xiN2",
		"y7366eVDt2HHZohTHqBENuRWLaV/515BO;",  // trailing BBS terminator
		";Ee+KwCYpzGBM0rhCP/J/YJcNJDGxyLSJA;", // both ends
		"9Sh8q8eZJHaS2EF/yN1AxdPUaDYrCaOrWsQpfginLw",
	}
	for _, b := range blobs {
		if !isEncodedBlob(b) {
			t.Errorf("not detected as a blob: %q", b)
		}
	}
}

// TestEncodedBlobKeepsWordLikeStrings is the false-positive guard. Each of these
// is long enough and uses only base64 characters, but is a readable string a
// person sent — including a repeater announcement that a naive length rule
// would have deleted.
func TestEncodedBlobKeepsWordLikeStrings(t *testing.T) {
	keep := []string{
		"HelloBeiJingFengTai438500",          // a person announcing a repeater
		"Paralelepipidoouuaabcdefghijlmnopq", // a test string
		"L+12283+10282+9181+8380+7879+7479",  // a channel scan list: repetitive
		"S+9487+6986+5886+5386+4986+4785",    // the same, other direction
		"ANCDEFGHIJKLMNOPQRSTUVWXYZ",         // an alphabet test
		"73 de EA7KLK, thanks for the QSO",
		"MerryChristmasAndHappyNewYear2026",
		"qwertyuiopasdfghjklzxcvbnm123456",
	}
	for _, k := range keep {
		if isEncodedBlob(k) {
			t.Errorf("a readable string was detected as a blob: %q", k)
		}
	}
}

// TestEncodedBlobNeedsEveryCondition proves the three conditions each matter.
func TestEncodedBlobNeedsEveryCondition(t *testing.T) {
	cases := map[string]bool{
		"short":                             false,
		"abcABC123abcABC123abcABC":          false, // repetitive: mixed case but low entropy
		"aB3kL9mQ2xZ7wE4rT8yU1iO6pS5dF0gH":  true,  // 32 chars, mixed, high entropy
		"abcabcabcabcabcabcabcabcabc12":     false, // all lower + digits, no upper
		"ABCABCABCABCABCABCABCABCABC12":     false, // all upper, no lower
		"abcdefghijklmnopqrstuvwxABCDEF":    false, // letters only, no digit/symbol
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa":  false, // zero entropy
		"2NbMAEKDSEp5R7up8X84D83W5KDr9EPJ4": true,
	}
	for body, want := range cases {
		if got := isEncodedBlob(body); got != want {
			t.Errorf("isEncodedBlob(%q) = %v, want %v", body, got, want)
		}
	}
}

// TestEncodedBlobRuleCompilesAndMatches checks the seeded rule end to end.
func TestEncodedBlobRuleCompilesAndMatches(t *testing.T) {
	var src []Source
	for i, f := range store.DefaultMessageFilters {
		src = append(src, Source{ID: int64(i + 1), Name: f.Name, Pattern: f.Pattern, Kind: f.Kind})
	}
	m := Compile(src, 1)
	if broken := m.Broken(); len(broken) > 0 {
		t.Fatalf("a seeded rule failed to compile: %v", broken)
	}
	if _, ok := m.Match("2NbMAEKDSEp5R7up8X84D83W5KDr9EPJ4SycmVdwuQ"); !ok {
		t.Fatal("the seeded blob rule did not match a real payload")
	}
	if _, ok := m.Match("HelloBeiJingFengTai438500"); ok {
		t.Fatal("the seeded rules match a readable string")
	}
	if _, ok := m.Match("73 de EA7KLK"); ok {
		t.Fatal("the seeded rules match a sign-off")
	}
}

func TestValidateKind(t *testing.T) {
	if err := ValidateKind("regex", `^uptime`); err != nil {
		t.Fatalf("a valid regex was refused: %v", err)
	}
	if err := ValidateKind("regex", "([bad"); err == nil {
		t.Fatal("an invalid regex was accepted")
	}
	if err := ValidateKind("encoded-blob", "label"); err != nil {
		t.Fatalf("an encoded-blob rule was refused: %v", err)
	}
	if err := ValidateKind("encoded-blob", ""); err == nil {
		t.Fatal("an encoded-blob rule with no label was accepted")
	}
	if err := ValidateKind("nonsense", "x"); err == nil {
		t.Fatal("an unknown kind was accepted")
	}
}
