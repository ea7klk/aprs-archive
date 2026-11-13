package aprs

import "testing"

func TestCategoryForDeclaredGroup(t *testing.T) {
	cases := map[string]string{
		"NWS":  CategoryWeather,
		"ARES": CategoryEmergency,
		"ARRL": CategoryOrganisation,
		"UNKN": CategoryOther,
	}
	for group, want := range cases {
		if got := CategoryFor(group, "neutral text"); got != want {
			t.Errorf("CategoryFor(%q) = %q, want %q", group, got, want)
		}
	}
}

func TestCategoryForFallsBackToKeywords(t *testing.T) {
	if got := CategoryFor("", "Tornado warning for the metro"); got != CategoryWeather {
		t.Fatalf("got %q", got)
	}
	if got := CategoryFor("", "EMERGENCY evacuation of sector 4"); got != CategoryEmergency {
		t.Fatalf("emergency should outrank weather, got %q", got)
	}
	if got := CategoryFor("", "plain text with no keywords"); got != CategoryGeneral {
		t.Fatalf("got %q, want general", got)
	}
}

func TestCategoryForUnknownGroupStaysVisible(t *testing.T) {
	if got := CategoryFor("ZZZ", "plain text"); got != CategoryOther {
		t.Fatalf("got %q, want other", got)
	}
}

func TestBulletinCategoryIDsStable(t *testing.T) {
	ids := BulletinCategoryIDs()
	if len(ids) != len(BulletinCategories) {
		t.Fatalf("id count mismatch")
	}
	for _, id := range ids {
		if !IsBulletinCategory(id) {
			t.Fatalf("IsBulletinCategory(%q) false for a listed id", id)
		}
	}
	if IsBulletinCategory("nonsense") {
		t.Fatal("unknown category accepted")
	}
}

func TestCategoryForWeatherGroupFamilies(t *testing.T) {
	// A national weather service encodes warning areas in the group suffix.
	for _, group := range []string{"WXZL", "WXERZ", "WX", "MOWAS", "WXWARN"} {
		if got := CategoryFor(group, "no keywords here"); got != CategoryWeather {
			t.Errorf("CategoryFor(%q) = %q, want weather", group, got)
		}
	}
	// A prefix rule must not swallow an exact group that says something else.
	if got := CategoryFor("FDR", "no keywords"); got != CategoryOther {
		t.Errorf("unknown group classified as %q", got)
	}
}
