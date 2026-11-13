package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"aprs-archive/internal/aprs"
	"aprs-archive/internal/store"
)

// TestLiveTrailingPartsAreStoredOnce drives the collector's own handle logic:
// three parts numbered at the end of the body must end as one stored row.
//
// It mirrors main.go rather than calling it, because the handle is a closure
// over live network state; the logic under test — store each part, then replace
// the set — is the same sequence.
func TestLiveTrailingPartsAreStoredOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.db")
	db, err := store.OpenWriter(path)
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	w, err := store.NewWriter(ctx, db)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	defer w.Close(ctx)

	assembler := aprs.NewAssembler(nil)
	bodies := []string{
		"A la xarxa quedarà enllaçat perquè els nostres nodes (1/3){4X}",
		"són igate; amb posar el digirepeater a Gardeny, les (2/3){4Y}",
		"trames entraran a internet sense problemes (3/3){4Z}",
	}
	now := time.Now().UTC()
	for i, body := range bodies {
		p, ok := aprs.ParseRaw("EA3JCH-13>APRS::EB3CLX-3 :" + body)
		if !ok {
			t.Fatalf("part %d did not parse", i+1)
		}
		partID, err := w.RecordPacket(ctx, p, now.Add(time.Duration(i)*15*time.Second))
		if err != nil {
			t.Fatalf("record part %d: %v", i+1, err)
		}
		assembled, replaced, complete := assembler.Add(p, partID)
		if !complete {
			continue
		}
		if partID > 0 {
			replaced = append(replaced, partID)
		}
		if err := w.ReplaceParts(ctx, replaced, assembled, now); err != nil {
			t.Fatalf("replace: %v", err)
		}
	}

	var rows, parts int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE msg_text LIKE '%(2/3)%'`).Scan(&parts); err != nil {
		t.Fatalf("count parts: %v", err)
	}
	if rows != 1 {
		t.Fatalf("%d rows stored, want 1", rows)
	}
	if parts != 0 {
		t.Fatalf("%d part rows survive", parts)
	}
}
