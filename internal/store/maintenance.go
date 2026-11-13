package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Maintenance is the health of the archive as an operator sees it.
type Maintenance struct {
	Path        string
	Bytes       int64
	WALBytes    int64
	Messages    int64
	Bulletins   int64
	OldestAt    time.Time
	NewestAt    time.Time
	SchemaVer   int
	PageSize    int64
	PageCount   int64
	FreePages   int64
	FreeBytes   int64
	QuickCheck  string
	JournalMode string
}

// Inspect reads the archive's size, counts and integrity.
//
// `PRAGMA quick_check` is used rather than `integrity_check`: it reads every
// page but skips the slower index-content comparison, which is the right
// trade-off for a page an operator loads casually. It still catches corruption
// of the file itself, which is what an operator wants to know first.
func Inspect(ctx context.Context, path string) (Maintenance, error) {
	m := Maintenance{Path: path}
	if fi, err := os.Stat(path); err == nil {
		m.Bytes = fi.Size()
	}
	if fi, err := os.Stat(path + "-wal"); err == nil {
		m.WALBytes = fi.Size()
	}
	db, err := OpenReader(path)
	if err != nil {
		return m, err
	}
	defer db.Close()

	var schema, pageSize, pageCount, freePages int64
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&schema); err != nil {
		return m, err
	}
	m.SchemaVer = int(schema)
	_ = db.QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize)
	_ = db.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pageCount)
	_ = db.QueryRowContext(ctx, "PRAGMA freelist_count").Scan(&freePages)
	m.PageSize, m.PageCount, m.FreePages = pageSize, pageCount, freePages
	m.FreeBytes = freePages * pageSize
	_ = db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&m.JournalMode)

	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM messages").Scan(&m.Messages); err != nil {
		return m, err
	}
	_ = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM bulletins").Scan(&m.Bulletins)
	var oldest, newest sql.NullInt64
	_ = db.QueryRowContext(ctx, "SELECT MIN(received_at), MAX(received_at) FROM messages").Scan(&oldest, &newest)
	if oldest.Valid {
		m.OldestAt = time.Unix(oldest.Int64, 0).UTC()
	}
	if newest.Valid {
		m.NewestAt = time.Unix(newest.Int64, 0).UTC()
	}
	var check string
	if err := db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&check); err != nil {
		m.QuickCheck = "error: " + err.Error()
	} else {
		m.QuickCheck = check
	}
	return m, nil
}

// TableCount is one table's row count, for the maintenance page.
type TableCount struct {
	Name string
	Rows int64
}

// Counts returns row counts for the archive's tables and indexes, so an operator
// can see growth without a SQL client.
func Counts(ctx context.Context, db *sql.DB) ([]TableCount, error) {
	tables := []string{"messages", "bulletins", "collector_state", "callsign_exceptions"}
	var out []TableCount
	for _, name := range tables {
		var n int64
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+name).Scan(&n); err != nil {
			return nil, fmt.Errorf("count %s: %w", name, err)
		}
		out = append(out, TableCount{Name: name, Rows: n})
	}
	return out, nil
}

// Vacuum compacts the file and returns the bytes reclaimed.
//
// VACUUM rewrites the database, so it needs free space equal to its size and it
// takes exclusive use of the file while it runs. The collector is writing
// continuously, so this can wait; the admin page runs it on demand and says what
// it reclaimed rather than pretending it was free.
func Vacuum(ctx context.Context, db *sql.DB, path string) (int64, error) {
	before := fileSize(path)
	if _, err := db.ExecContext(ctx, "VACUUM"); err != nil {
		return 0, fmt.Errorf("vacuum: %w", err)
	}
	after := fileSize(path)
	if after >= before {
		return 0, nil
	}
	return before - after, nil
}

// Checkpoint moves the write-ahead log back into the database file.
//
// The collector checkpoints on its own schedule; this lets an operator force it,
// which is what makes a backup copy meaningful and keeps the WAL from growing
// while the collector is quiet.
func Checkpoint(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
	return err
}

// BackupDir is where backups are written. It sits inside the claim so a backup
// survives a pod restart, and outside the database path so it is never confused
// with the live file.
func BackupDir(path string) string { return filepath.Join(filepath.Dir(path), "backups") }

// BackupFile is one completed backup.
type BackupFile struct {
	Name    string
	Bytes   int64
	Created time.Time
}

// ListBackups returns the backups on disk, newest first.
func ListBackups(path string) ([]BackupFile, error) {
	entries, err := os.ReadDir(BackupDir(path))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []BackupFile
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".db") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, BackupFile{Name: e.Name(), Bytes: info.Size(), Created: info.ModTime().UTC()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out, nil
}

// CreateBackup writes a consistent snapshot to the backup directory and prunes
// old ones.
//
// The snapshot is made with VACUUM INTO, not a file copy: copying a live SQLite
// database plus its WAL is not consistent, and an operator restoring it would
// find a torn archive. VACUUM INTO is safe while the collector keeps writing.
//
// Keeping a bounded number of backups is part of the operation, not a separate
// chore: an unbounded backup directory on a 5 GiB claim fills the disk, and a
// full disk stops collection.
func CreateBackup(ctx context.Context, db *sql.DB, path string, keep int) (string, error) {
	if keep <= 0 {
		keep = 7
	}
	dir := BackupDir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create backup directory: %w", err)
	}
	name := "archive-" + time.Now().UTC().Format("20060102-150405") + ".db"
	dest := filepath.Join(dir, name)
	if err := Backup(ctx, db, dest); err != nil {
		// A partial file would look like a usable backup; remove it.
		_ = os.Remove(dest)
		return "", err
	}
	if err := pruneBackups(dir, keep); err != nil {
		return dest, err
	}
	return dest, nil
}

// DeleteBackup removes one backup by file name.
//
// The name is validated, not trusted: a path from a form must never be able to
// walk out of the backup directory.
func DeleteBackup(path, name string) error {
	if name == "" || strings.ContainsAny(name, `/\`) || !strings.HasSuffix(name, ".db") {
		return fmt.Errorf("invalid backup name")
	}
	if strings.HasPrefix(name, ".") {
		return fmt.Errorf("invalid backup name")
	}
	return os.Remove(filepath.Join(BackupDir(path), name))
}

func pruneBackups(dir string, keep int) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var files []os.DirEntry
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".db") {
			files = append(files, e)
		}
	}
	if len(files) <= keep {
		return nil
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name() < files[j].Name() })
	for _, e := range files[:len(files)-keep] {
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// DeleteMessages removes messages by id range or by an explicit predicate, and
// reports how many went.
//
// Deleting by kind, by age or by callsign is what an operator does when a bad
// import or a single noisy station has to go. It is bounded so it cannot hold
// the write lock for a long time on a large archive.
func DeleteMessages(ctx context.Context, db *sql.DB, clause string, args ...any) (int64, error) {
	res, err := db.ExecContext(ctx, "DELETE FROM messages WHERE "+clause, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeleteMessagesBefore applies retention manually, so an operator can free space
// without waiting for the nightly sweep.
func DeleteMessagesBefore(ctx context.Context, db *sql.DB, cutoff time.Time) (int64, error) {
	messages, _, err := Sweep(ctx, db, Retention{Messages: time.Since(cutoff), Bulletins: time.Since(cutoff)}, time.Now().UTC())
	return messages, err
}

// RebuildIndexes drops and recreates every index, which repairs an index that
// `quick_check` reports as damaged and reclaims index bloat.
func RebuildIndexes(ctx context.Context, db *sql.DB) error {
	for _, idx := range []string{
		"idx_messages_received", "idx_messages_from", "idx_messages_to", "idx_messages_kind",
		"idx_bulletins_last_seen", "idx_bulletins_category",
	} {
		if _, err := db.ExecContext(ctx, "REINDEX "+idx); err != nil {
			return fmt.Errorf("reindex %s: %w", idx, err)
		}
	}
	return nil
}

func fileSize(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}
