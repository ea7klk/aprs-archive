package prefixes

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// realSample is a slice of the published cty.csv, including the annotations and
// the one-letter prefixes that matter most for the US and Italy.
const realSample = `1A,Sov Mil Order of Malta,246,EU,15,28,41.90,-12.43,-1.0,1A;
3A,Monaco,260,EU,14,27,43.73,-7.40,-1.0,3A =VERSION;
I,Italy,248,EU,15,28,42.83,12.83,-1.0,I;
K,United States,291,NA,5,8,37.53,-91.87,-1.0,K W N;
EA,Spain,281,EU,14,33,40.00,-4.00,-1.0,EA EB EC ED EE EF EG EH;
DL,Germany,230,EU,14,28,51.00,10.00,-1.0,DA DB DC DD DF DG DH DI DJ DK DL DM DN DO DP DQ DR;
JA,Japan,339,AS,25,45,36.00,138.00,-9.0,JA JB JC JD JE JF JG JH JI JJ JK JL JM JN JO JP JQ JR;
KH6,Hawaii,110,OC,31,61,21.00,-157.00,-10.0,KH6;
B2A,China,318,AS,24,44,35.00,105.00,-8.0,B2A[33] B2B[33];
G,England,223,EU,14,27,52.00,-1.00,0.0,G GX M;
VK,Australia,150,OC,29,55,-25.00,135.00,-10.0,VK;
PY,Brazil,108,SA,11,15,-10.00,-55.00,-3.0,PP PQ PR PS PT PU PV PW PX PY PZ;
ZS,South Africa,462,AF,38,57,-29.00,24.00,-2.0,ZR ZS ZT ZU ZV ZW ZX ZY ZZ;
9M,West Malaysia,299,AS,28,54,4.00,102.00,-8.0,9M 9M2 9M4 9M6 9M8;
ON,Belgium,209,EU,14,27,50.50,4.50,0.0,ON ON4 ON5 ON6 ON7 ON8;
HB,Switzerland,287,EU,14,28,47.00,8.00,0.0,HB HB3 HB9 HBL;
SP,Poland,269,EU,15,28,52.00,20.00,0.0,SN SO SP SQ SR SU 3Z;
`

func sampleSet(t *testing.T) *Set {
	t.Helper()
	// ParseCSV requires a plausible size, so the fixture is padded with
	// generated prefixes to reach the floor.
	var b strings.Builder
	b.WriteString(realSample)
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&b, "X%d,Generated %d,999,EU,15,28,0,0,0,X%d;\n", i, i, i)
	}
	set, err := ParseCSV([]byte(b.String()))
	if err != nil {
		t.Fatalf("ParseCSV: %v", err)
	}
	return set
}

func TestValidAcceptsRealCallsigns(t *testing.T) {
	set := sampleSet(t)
	real := []string{
		"EA7KLK", "EA7KLK-9", "EA7KLK-15", "DL6TJ-9", "IZ6RND", "NB9D-9",
		"KH6ABC", "JA1ABC", "G4ABC", "VK2ABC", "PY2ABC", "ZS6ABC", "9M2ABC",
		"ON1MS-1", "HB9ABC", "SP0ABC", "K1ABC", "W3ABC", "N0CALL",
		"EA7KLK/QRP", "EA7KLK/P", "ea7klk-9",
	}
	for _, c := range real {
		if !set.Valid(c) {
			t.Errorf("%q rejected, but it is a real station", c)
		}
	}
}

func TestValidRejectsBotNames(t *testing.T) {
	set := sampleSet(t)
	bots := []string{
		"MYANET", "OTA", "WXBOT", "ANSRVR", "CQSRVR", "RGSTRY", "MLBFLS",
		"QRX", "CRPFLS", "DAGA", "PKTQSO", "BBSMY", "GRP", "BORIKE",
		"PKTNET", "WXNOW", "WHO-IS", "SHUB", "FIND", "JOKE", "EMAIL-2",
		"MTG-RRRA", "POLY-13", "SMS", "MAIL", "TWC", "APRSD", "NWS", "APRS",
	}
	for _, c := range bots {
		if set.Valid(c) {
			t.Errorf("%q accepted, but it is a bot or a software group", c)
		}
	}
}

// TestBotWithACallsignShapeIsAccepted documents the limit of prefix validation:
// a robot that names itself like a station passes. That is what the operator's
// exception list is for.
func TestBotWithACallsignShapeIsAccepted(t *testing.T) {
	set := sampleSet(t)
	if !set.Valid("EA7BOT") {
		t.Fatal("a callsign-shaped name was rejected; the exception list would never be needed")
	}
}

func TestParseCSVRemovesAnnotations(t *testing.T) {
	set := sampleSet(t)
	if !set.Valid("B2ABC") {
		t.Fatal("an annotated prefix (B2A[33]) was not reduced to B2A")
	}
}

func TestParseCSVRejectsAnErrorPage(t *testing.T) {
	// A short body is what a captive portal or an error page looks like.
	if _, err := ParseCSV([]byte("<html>Service unavailable</html>")); err == nil {
		t.Fatal("garbage was accepted as a prefix list")
	}
}

func TestFetcherKeepsTheOldSnapshotWhenRefreshFails(t *testing.T) {
	var fail bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		var b strings.Builder
		b.WriteString(realSample)
		for i := 0; i < 200; i++ {
			fmt.Fprintf(&b, "X%d,Generated %d,999,EU,15,28,0,0,0,X%d;\n", i, i, i)
		}
		fmt.Fprint(w, b.String())
	}))
	defer srv.Close()

	f := &Fetcher{URL: srv.URL, Client: srv.Client(), MaxAge: 0}
	if err := f.Refresh(context.Background()); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	if !f.Ready() {
		t.Fatal("not ready after a successful refresh")
	}
	fail = true
	if err := f.Refresh(context.Background()); err == nil {
		t.Fatal("a failing refresh reported success")
	}
	// Current still answers, with the last good snapshot.
	set, err := f.Current(context.Background())
	if err != nil || set == nil {
		t.Fatalf("lost the snapshot after a failed refresh: %v", err)
	}
	if !set.Valid("EA7KLK-9") {
		t.Fatal("the surviving snapshot does not validate")
	}
	if _, refreshErr := f.Snapshot(); refreshErr == nil {
		t.Fatal("the refresh failure is not recorded for the admin page")
	}
}

func TestSetIsNilSafe(t *testing.T) {
	var s *Set
	if s.Valid("EA7KLK") || s.Contains("EA7KLK") {
		t.Fatal("a nil set accepted a callsign")
	}
}
