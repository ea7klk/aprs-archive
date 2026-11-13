// Package store owns the archive database: one SQLite file, written only by the
// collector and read by the web interface.
//
// Two processes open the same file, so the settings here are a contract between
// them, not a local preference:
//
//   - WAL, so a reader never blocks the writer and a dashboard query cannot
//     stall collection.
//   - One writer. SQLite allows a single write transaction at a time; the
//     collector batches into one transaction per flush instead of one per
//     packet, so contention is measured in flushes per minute.
//   - One reader connection in the web process, so a long report cannot open a
//     dozen connections and starve the writer of WAL checkpoints.
//
// The collector owns the schema. The web process asserts the schema exists and
// refuses to start if it does not: a web pod that silently served an empty
// archive would look like data loss.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// SchemaVersion is bumped whenever schema.sql changes in a way that needs code.
const SchemaVersion = 1

// DSN for the writing process.
//
// `_txlock=immediate` takes the write lock at BEGIN, which turns a busy
// database into a wait rather than a lost update; `_busy_timeout` bounds the
// wait; `synchronous=NORMAL` is safe under WAL and avoids an fsync per commit.
func writeDSN(path string) string {
	return "file:" + path +
		"?_pragma=busy_timeout(10000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=wal_autocheckpoint(4000)" +
		"&_pragma=temp_store(MEMORY)" +
		"&_txlock=immediate"
}

// DSN for the read-only process.
//
// `mode=ro` makes an accidental write a hard error at the SQLite layer rather
// than a bug in a handler; the fail-closed posture is deliberate.
func readDSN(path string) string {
	return "file:" + path +
		"?mode=ro" +
		"&_pragma=busy_timeout(10000)" +
		"&_pragma=query_only(true)" +
		"&_pragma=temp_store(MEMORY)" +
		"&_pragma=cache_size(-8000)"
}

// OpenWriter opens (or creates) the archive for the collector and applies the
// schema. It is safe to call on every start.
func OpenWriter(path string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	db, err := sql.Open("sqlite", writeDSN(path))
	if err != nil {
		return nil, fmt.Errorf("open archive: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := migrate(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// OpenReader opens an existing archive read-only. It refuses to create the
// file: the collector is the only thing allowed to.
func OpenReader(path string) (*sql.DB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("open archive read-only: %w", err)
	}
	db, err := sql.Open("sqlite", readDSN(path))
	if err != nil {
		return nil, fmt.Errorf("open archive read-only: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		db.Close()
		return nil, fmt.Errorf("read schema version: %w", err)
	}
	if version != SchemaVersion {
		db.Close()
		return nil, fmt.Errorf("archive schema is version %d, this build expects %d: let the collector migrate it first", version, SchemaVersion)
	}
	return db, nil
}

// SchemaError is returned when the archive file exists but is not an archive.
type SchemaError struct{ Path string }

func (e SchemaError) Error() string {
	return "no archive schema at " + e.Path + " (the collector has not migrated it yet)"
}

func migrate(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration: %w", err)
	}
	defer tx.Rollback()

	for _, statement := range splitStatements(schemaSQL) {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("apply schema: %w (statement: %.80s)", err, statement)
		}
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", SchemaVersion)); err != nil {
		return fmt.Errorf("record schema version: %w", err)
	}
	return tx.Commit()
}

// splitStatements splits the embedded schema on semicolons, dropping comment
// lines. The schema deliberately contains no triggers or string literals with
// semicolons, so a naive split is exact here; keeping it naive keeps the
// migration readable. A semicolon inside a comment would break this, which is
// why comment lines are removed before splitting.
func splitStatements(schema string) []string {
	var out []string
	var b strings.Builder
	for _, line := range strings.Split(schema, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	for _, statement := range strings.Split(b.String(), ";") {
		if statement = strings.TrimSpace(statement); statement != "" {
			out = append(out, statement)
		}
	}
	return out
}

// Backup copies the live archive to dest using SQLite's online backup API.
//
// A database file copied with `cp` while the collector is writing is torn: WAL
// writes land in a sidecar, and a checkpoint can move pages mid-copy. VACUUM
// INTO produces a consistent snapshot without stopping the collector, which is
// what an operator taking a backup before a migration needs.
func Backup(ctx context.Context, db *sql.DB, dest string) error {
	if err := os.Remove(dest); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove stale backup: %w", err)
	}
	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", dest); err != nil {
		return fmt.Errorf("vacuum into %s: %w", dest, err)
	}
	return nil
}
