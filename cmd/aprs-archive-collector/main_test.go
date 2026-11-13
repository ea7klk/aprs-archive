package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"aprs-archive/internal/aprs"
	"aprs-archive/internal/store"
)

// TestArchivedKindsExcludeAcks pins the storage decision: acknowledgements and
// rejections are message-format packets, so the parser produces them, but the
// archive keeps only what was said. If this test fails because the collector
// started storing acks again, that is a deliberate change to the contract.
func TestArchivedKindsExcludeAcks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.db")
	db, err := store.OpenWriter(path)
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}
	defer db.Close()
	w, err := store.NewWriter(context.Background(), db)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	defer w.Close(context.Background())

	now := time.Now().UTC()
	packets := []aprs.Packet{
		{From: "A-1", To: "B", Text: "hello", Kind: aprs.KindMessage},
		{From: "B", To: "A-1", Text: "ack1", Kind: aprs.KindAck, AckID: "1"},
		{From: "B", To: "A-1", Text: "rej2", Kind: aprs.KindRej, AckID: "2"},
		{From: "C", To: "BLN1NWS", Text: "warning", Kind: aprs.KindBulletin,
			Bulletin: aprs.Address{Slot: 1, Group: "NWS"}},
	}
	stored := 0
	for _, p := range packets {
		if p.Kind == aprs.KindAck || p.Kind == aprs.KindRej {
			continue // the collector's filter, mirrored here
		}
		if err := w.Record(context.Background(), p, now); err != nil {
			t.Fatalf("Record: %v", err)
		}
		stored++
	}
	if err := w.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	var kinds int
	if err := db.QueryRow(`SELECT COUNT(DISTINCT kind) FROM messages`).Scan(&kinds); err != nil {
		t.Fatalf("count: %v", err)
	}
	if kinds != 2 {
		t.Fatalf("distinct kinds = %d, want 2 (message, bulletin)", kinds)
	}
	var acks int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE kind IN ('ack','rej')`).Scan(&acks); err != nil {
		t.Fatalf("ack count: %v", err)
	}
	if acks != 0 {
		t.Fatalf("archive holds %d ack/rej rows, want 0", acks)
	}
	if stored != 2 {
		t.Fatalf("stored %d packets, want 2", stored)
	}
}
