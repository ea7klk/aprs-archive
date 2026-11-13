package aprs

import "strings"

// Bulletin categories.
//
// The group a sender declares is authoritative, but it is often empty or a
// local abbreviation, so resolution is staged: declared group, then keywords in
// the body, then a default. This is the same taxonomy simple-ham-bbs uses for
// `/aprs/bulletins`, kept identical so a reader moving between the BBS and the
// archive does not have to learn two sets of names.
const (
	CategoryWeather      = "weather"
	CategoryEmergency    = "emergency"
	CategoryTraffic      = "traffic"
	CategoryNets         = "nets"
	CategoryEvents       = "events"
	CategoryDX           = "dx"
	CategorySatellite    = "satellite"
	CategoryNetwork      = "network"
	CategoryService      = "service"
	CategoryOrganisation = "organisation"
	CategoryTest         = "test"
	CategoryGeneral      = "general"
	CategoryOther        = "other"
)

// BulletinCategory is one category, in display order.
type BulletinCategory struct {
	ID   string
	Desc string
}

// BulletinCategories lists every category a bulletin can land in, in the order
// a reader most needs them: what is dangerous first, then coordination, then
// interest, with the catch-alls last.
var BulletinCategories = []BulletinCategory{
	{CategoryWeather, "Watches, warnings, advisories and spotter activation"},
	{CategoryEmergency, "Declared emergencies, evacuations and served-agency coordination"},
	{CategoryTraffic, "National Traffic System and formal radiogram traffic"},
	{CategoryNets, "Net times, frequencies and control stations"},
	{CategoryEvents, "Field days, contests and special-event stations"},
	{CategoryDX, "DXpeditions, band openings and propagation forecasts"},
	{CategorySatellite, "Satellite passes, transponder status and ISS operations"},
	{CategoryNetwork, "APRS network announcements and igate or digipeater service notes"},
	{CategoryService, "Repeater, gateway and infrastructure disruption"},
	{CategoryOrganisation, "Society bulletins, rule changes and membership notices"},
	{CategoryTest, "Test bulletins"},
	{CategoryGeneral, "Uncategorised bulletins"},
	{CategoryOther, "A group the archive does not recognise"},
}

// BulletinCategoryIDs returns the category ids in display order.
func BulletinCategoryIDs() []string {
	ids := make([]string, 0, len(BulletinCategories))
	for _, c := range BulletinCategories {
		ids = append(ids, c.ID)
	}
	return ids
}

// IsBulletinCategory reports whether id is one of BulletinCategories.
func IsBulletinCategory(id string) bool {
	for _, c := range BulletinCategories {
		if c.ID == id {
			return true
		}
	}
	return false
}

// bulletinGroups maps an observed group to a category. An unknown group falls
// through to the keyword heuristic and then to `other`.
var bulletinGroups = map[string]string{
	"NWS":   CategoryWeather,
	"WX":    CategoryWeather,
	"SKY":   CategoryWeather,
	"SKYW":  CategoryWeather,
	"EMERG": CategoryEmergency,
	"EMER":  CategoryEmergency,
	"ARES":  CategoryEmergency,
	"RACES": CategoryEmergency,
	"NTS":   CategoryTraffic,
	"TRAFF": CategoryTraffic,
	"RADIO": CategoryTraffic,
	"NET":   CategoryNets,
	"SCHED": CategoryNets,
	"EVENT": CategoryEvents,
	"CONTE": CategoryEvents,
	"FIELD": CategoryEvents,
	"FD":    CategoryEvents,
	"DX":    CategoryDX,
	"PROP":  CategoryDX,
	"SPOT":  CategoryDX,
	"SAT":   CategorySatellite,
	"AMSAT": CategorySatellite,
	"ISS":   CategorySatellite,
	"APRS":  CategoryNetwork,
	"IGATE": CategoryNetwork,
	"DIGI":  CategoryNetwork,
	"OUTAG": CategoryService,
	"MAINT": CategoryService,
	"SERV":  CategoryService,
	"ARRL":  CategoryOrganisation,
	"IARU":  CategoryOrganisation,
	"TEST":  CategoryTest,
	// Local and regional bulletin areas. Observed in live traffic, usually
	// weather products from national services that broadcast one bulletin per
	// warning area (DWD's WX<area> codes, for example).
	"LOCAL": CategoryGeneral,
}

// bulletinGroupPrefixes maps a group *prefix* to a category, for services whose
// group is a family rather than one name.
//
// A group that would not otherwise resolve is checked against these before the
// keyword fallback: a national weather service encodes its warning areas in the
// group, so `WXZL` and `WXERZ` are weather even though neither is a known group.
var bulletinGroupPrefixes = []struct {
	Prefix   string
	Category string
}{
	{"WX", CategoryWeather},
	{"WARN", CategoryWeather},
	{"MOWAS", CategoryWeather},
	{"EMER", CategoryEmergency},
}

// bulletinKeywords is the content fallback, used only when the group is empty
// or unknown. Order matters: an emergency word outranks a weather word, because
// a bulletin mentioning both is an emergency bulletin.
var bulletinKeywords = []struct {
	Category string
	Words    []string
}{
	{CategoryEmergency, []string{"EMERGENCY", "EVACUAT", "MAYDAY", "LIFE THREAT", "IMMEDIATE EVACUATION"}},
	{CategoryWeather, []string{"WARNING", "WATCH", "ADVISORY", "TORNADO", "THUNDERSTORM", "FLOOD", "HURRICANE", "TYPHOON", "BLIZZARD", "WIND CHILL", "HEAT ADVISORY", "SPOTTER"}},
	{CategorySatellite, []string{"SATELLITE", "TRANSPONDER", "ISS ", "AO-", "SO-", "FO-", "PASS "}},
	{CategoryTraffic, []string{"RADIOGRAM", "TRAFFIC", "NTS "}},
	{CategoryNets, []string{"NET ", "NETS ", "CHECK-IN", "CHECKIN", "CONTROL STATION"}},
	{CategoryEvents, []string{"CONTEST", "FIELD DAY", "SPECIAL EVENT", "EXPEDITION"}},
	{CategoryService, []string{"OUTAGE", "MAINTENANCE", "OFF AIR", "DOWN FOR"}},
}

// CategoryFor resolves a bulletin category from its declared group and its
// body text.
func CategoryFor(group, text string) string {
	normalisedGroup := strings.ToUpper(strings.TrimSpace(group))
	if category, ok := bulletinGroups[normalisedGroup]; ok {
		return category
	}
	for _, rule := range bulletinGroupPrefixes {
		if strings.HasPrefix(normalisedGroup, rule.Prefix) {
			return rule.Category
		}
	}
	upper := strings.ToUpper(text)
	for _, rule := range bulletinKeywords {
		for _, word := range rule.Words {
			if strings.Contains(upper, word) {
				return rule.Category
			}
		}
	}
	if strings.TrimSpace(group) == "" {
		// No group and no recognisable content: an ordinary uncategorised
		// bulletin, not an unknown one.
		return CategoryGeneral
	}
	// A group was declared but the archive does not know it. Keep the bulletin
	// visible with the raw group rather than pretending it is general.
	return CategoryOther
}
