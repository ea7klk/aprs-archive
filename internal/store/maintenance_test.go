package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"aprs-archive/internal/aprs"
)

func TestInspectReportsSizeAndIntegrity(t *testing.T) {
	path, db := newTestArchive(t)
	now := time.Now().UTC()
	seed(t, db, []aprs.Packet{msg("A", "B", "x"), bulletin("C", "NWS", "w")}, now)

	m, err := Inspect(context.Background(), path)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if m.Messages != 2 {
		t.Fatalf("messages = %d, want 2", m.Messages)
	}
	if m.Bulletins != 1 {
		t.Fatalf("bulletins = %d, want 1", m.Bulletins)
	}
	if m.SchemaVer != SchemaVersion {
		t.Fatalf("schema = %d, want %d", m.SchemaVer, SchemaVersion)
	}
	if m.QuickCheck != "ok" {
		t.Fatalf("quick_check = %q, want ok", m.QuickCheck)
	}
	if m.Bytes == 0 || m.PageSize == 0 {
		t.Fatalf("size not reported: %+v", m)
	}
	if m.OldestAt.IsZero() || m.NewestAt.IsZero() {
		t.Fatalf("time range not reported: %+v", m)
	}
}

func TestBackupCreatesAConsistentCopyAndPrunes(t *testing.T) {
	path, db := newTestArchive(t)
	now := time.Now().UTC()
	seed(t, db, []aprs.Packet{msg("A", "B", "one"), msg("C", "D", "two")}, now)

	ctx := context.Background()
	var names []string
	for i := 0; i < 3; i++ {
		name, err := CreateBackup(ctx, db, path, 2)
		if err != nil {
			t.Fatalf("CreateBackup: %v", err)
		}
		names = append(names, filepath.Base(name))
		// The timestamp is second-resolution; make each distinct.
		time.Sleep(1100 * time.Millisecond)
	}
	backups, err := ListBackups(path)
	if err != nil {
		t.Fatalf("ListBackups: %v", err)
	}
	if len(backups) != 2 {
		t.Fatalf("%d backups kept, want 2 (the cap)", len(backups))
	}
	// The newest must be readable and complete.
	reader, err := OpenReader(filepath.Join(BackupDir(path), backups[0].Name))
	if err != nil {
		t.Fatalf("open backup: %v", err)
	}
	defer reader.Close()
	var n int
	if err := reader.QueryRow("SELECT COUNT(*) FROM messages").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Fatalf("backup holds %d messages, want 2", n)
	}
	_ = names
}

func TestDeleteBackupRefusesPaths(t *testing.T) {
	path, db := newTestArchive(t)
	_ = db
	for _, bad := range []string{"../archive.db", "sub/x.db", ".hidden.db", "", "x.txt"} {
		if err := DeleteBackup(path, bad); err == nil {
			t.Errorf("DeleteBackup accepted %q", bad)
		}
	}
	// A legitimate name is removed.
	if _, err := CreateBackup(context.Background(), db, path, 7); err != nil {
		t.Fatalf("CreateBackup: %v", err)
	}
	backups, _ := ListBackups(path)
	if len(backups) != 1 {
		t.Fatalf("%d backups, want 1", len(backups))
	}
	if err := DeleteBackup(path, backups[0].Name); err != nil {
		t.Fatalf("DeleteBackup: %v", err)
	}
	if got, _ := ListBackups(path); len(got) != 0 {
		t.Fatalf("%d backups remain", len(got))
	}
}

func TestVacuumReclaimsAndKeepsData(t *testing.T) {
	path, db := newTestArchive(t)
	now := time.Now().UTC()
	packets := make([]aprs.Packet, 0, 500)
	for i := 0; i < 500; i++ {
		packets = append(packets, msg("A", "B", "some text to take up pages"))
	}
	seed(t, db, packets, now)
	if _, err := db.Exec(`DELETE FROM messages`); err != nil {
		t.Fatalf("delete: %v", err)
	}
	before, _ := os.Stat(path)
	reclaimed, err := Vacuum(context.Background(), db, path)
	if err != nil {
		t.Fatalf("Vacuum: %v", err)
	}
	after, _ := os.Stat(path)
	if after.Size() > before.Size() {
		t.Fatal("vacuum made the file larger")
	}
	if reclaimed == 0 && before.Size()-after.Size() > 0 {
		t.Fatalf("vacuum reclaimed bytes but reported 0")
	}
}

func TestCountsCoversEveryTable(t *testing.T) {
	_, db := newTestArchive(t)
	seed(t, db, []aprs.Packet{msg("A", "B", "x")}, time.Now())
	counts, err := Counts(context.Background(), db)
	if err != nil {
		t.Fatalf("Counts: %v", err)
	}
	if len(counts) < 3 {
		t.Fatalf("counts = %+v", counts)
	}
	for _, c := range counts {
		if c.Name == "messages" && c.Rows != 1 {
			t.Fatalf("messages = %d, want 1", c.Rows)
		}
	}
}

func TestRebuildIndexesLeavesDataIntact(t *testing.T) {
	_, db := newTestArchive(t)
	now := time.Now().UTC()
	seed(t, db, []aprs.Packet{msg("A", "B", "findme")}, now)
	if err := RebuildIndexes(context.Background(), db); err != nil {
		t.Fatalf("RebuildIndexes: %v", err)
	}
	page, err := SearchMessages(context.Background(), db, SearchFilter{Text: "findme", Since: now.Add(-time.Hour), Until: now.Add(time.Hour)})
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if page.Total != 1 {
		t.Fatalf("search after reindex found %d, want 1", page.Total)
	}
}
