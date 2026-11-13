package store

import (
	"context"
	"testing"
	"time"

	"aprs-archive/internal/aprs"
)

func TestExceptionsRoundTrip(t *testing.T) {
	_, db := newTestArchive(t)
	ctx := context.Background()
	if err := SetException(ctx, db, "aprsph", true, "APRSPH bot is wanted"); err != nil {
		t.Fatalf("SetException: %v", err)
	}
	if err := SetException(ctx, db, "WXBOT", false, "weather bot"); err != nil {
		t.Fatalf("SetException: %v", err)
	}
	list, err := ListExceptions(ctx, db)
	if err != nil {
		t.Fatalf("ListExceptions: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("%d exceptions, want 2", len(list))
	}
	// Stored upper case regardless of how it was typed.
	for _, e := range list {
		if e.Callsign != "APRSPH" && e.Callsign != "WXBOT" {
			t.Fatalf("unexpected callsign %q", e.Callsign)
		}
	}
	m, err := LoadExceptions(ctx, db)
	if err != nil {
		t.Fatalf("LoadExceptions: %v", err)
	}
	if !m["APRSPH"] || m["WXBOT"] {
		t.Fatalf("lookup map wrong: %v", m)
	}
	// Upsert replaces the decision rather than duplicating.
	if err := SetException(ctx, db, "APRSPH", false, "changed my mind"); err != nil {
		t.Fatalf("update: %v", err)
	}
	m, _ = LoadExceptions(ctx, db)
	if m["APRSPH"] {
		t.Fatal("the decision was not replaced")
	}
	if err := DeleteException(ctx, db, "APRSPH"); err != nil {
		t.Fatalf("DeleteException: %v", err)
	}
	m, _ = LoadExceptions(ctx, db)
	if _, ok := m["APRSPH"]; ok {
		t.Fatal("the exception survived deletion")
	}
}

func TestPurgeInvalidCallsignsRemovesBothDirections(t *testing.T) {
	_, db := newTestArchive(t)
	ctx := context.Background()
	now := time.Now().UTC()
	seed(t, db, []aprs.Packet{
		msg("EA7KLK-9", "N0CALL", "real to real"),
		msg("MYANET", "EA7KLK-9", "bot to real"),
		msg("EA7KLK-9", "WXBOT", "real to bot"),
		msg("OTA", "RGSTRY", "bot to bot"),
		bulletin("EA7KLK-9", "NWS", "real bulletin"),
	}, now)

	keep := func(c string) bool {
		switch c {
		case "MYANET", "WXBOT", "OTA", "RGSTRY":
			return false
		}
		return true
	}
	removed, err := PurgeInvalidCallsigns(ctx, db, keep)
	if err != nil {
		t.Fatalf("PurgeInvalidCallsigns: %v", err)
	}
	if removed != 3 {
		t.Fatalf("removed %d, want 3", removed)
	}
	page, err := SearchMessages(ctx, db, SearchFilter{Since: now.Add(-time.Hour), Until: now.Add(time.Hour)})
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if page.Total != 2 {
		t.Fatalf("%d rows remain, want 2", page.Total)
	}
	for _, r := range page.Rows {
		if r.From == "MYANET" || r.To == "WXBOT" || r.From == "OTA" {
			t.Fatalf("bot row survived: %+v", r)
		}
	}
}

func TestPurgeInvalidCallsignsWalksPastACleanChunk(t *testing.T) {
	_, db := newTestArchive(t)
	ctx := context.Background()
	now := time.Now().UTC()
	// More than one batch of clean rows, then a bot beyond the first chunk.
	packets := make([]aprs.Packet, 0, 2100)
	for i := 0; i < 2100; i++ {
		packets = append(packets, msg("EA7KLK-9", "N0CALL", "clean"))
	}
	packets = append(packets, msg("MYANET", "EA7KLK-9", "bot"))
	seed(t, db, packets, now)

	removed, err := PurgeInvalidCallsigns(ctx, db, func(c string) bool { return c != "MYANET" })
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed %d, want 1 (a bot past the first batch)", removed)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2100 {
		t.Fatalf("%d rows remain, want 2100", n)
	}
}
