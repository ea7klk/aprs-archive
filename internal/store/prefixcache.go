package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrNoPrefixSnapshot means the archive has never cached a prefix list.
//
// It is a distinct condition from "the list is empty" and from "the fetch
// failed": on a fresh archive nothing has been fetched yet, and the collector
// must treat that as "do not store anything" rather than "store everything".
var ErrNoPrefixSnapshot = errors.New("no prefix snapshot cached")

// PrefixSnapshot is the cached callsign prefix dataset.
type PrefixSnapshot struct {
	Body         []byte
	Source       string
	Generation   int64
	FetchedAt    time.Time
	PrefixCount  int64
	ETag         string
	LastModified string
}

// ReadPrefixSnapshot returns the cached dataset.
//
// It is one row read on a path the collector hits once a minute, so it takes the
// body too: a hundred kilobytes read once a minute is nothing next to a database
// query, and comparing a hash would need the body anyway.
func ReadPrefixSnapshot(ctx context.Context, db *sql.DB) (PrefixSnapshot, error) {
	var s PrefixSnapshot
	var at int64
	err := db.QueryRowContext(ctx, `
		SELECT body, source, generation, fetched_at, prefix_count, etag, last_modified
		FROM prefix_cache WHERE id = 1`).
		Scan(&s.Body, &s.Source, &s.Generation, &at, &s.PrefixCount, &s.ETag, &s.LastModified)
	if errors.Is(err, sql.ErrNoRows) {
		return s, ErrNoPrefixSnapshot
	}
	if err != nil {
		return s, err
	}
	s.FetchedAt = time.Unix(at, 0).UTC()
	return s, nil
}

// PrefixGeneration returns only the generation counter, for the cheap poll the
// collector runs every minute. It avoids reading the body when nothing changed.
func PrefixGeneration(ctx context.Context, db *sql.DB) (int64, error) {
	var gen int64
	err := db.QueryRowContext(ctx, `SELECT generation FROM prefix_cache WHERE id = 1`).Scan(&gen)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNoPrefixSnapshot
	}
	return gen, err
}

// WritePrefixSnapshot replaces the cached dataset.
//
// It is called only with a body that has already been parsed and accepted, so a
// broken download cannot reach here. The generation is taken from the current
// row inside the statement, so two writers cannot produce the same generation
// and a reader cannot miss a change between reading the counter and the body.
func WritePrefixSnapshot(ctx context.Context, db *sql.DB, s PrefixSnapshot) (int64, error) {
	var gen int64
	err := db.QueryRowContext(ctx, `
		INSERT INTO prefix_cache (id, body, source, generation, fetched_at, prefix_count, etag, last_modified)
		VALUES (1, ?, ?, 1, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
		  body          = excluded.body,
		  source        = excluded.source,
		  generation    = prefix_cache.generation + 1,
		  fetched_at    = excluded.fetched_at,
		  prefix_count  = excluded.prefix_count,
		  etag          = excluded.etag,
		  last_modified = excluded.last_modified
		RETURNING generation`,
		s.Body, s.Source, time.Now().Unix(), s.PrefixCount, s.ETag, s.LastModified).Scan(&gen)
	if err != nil {
		return 0, fmt.Errorf("write prefix snapshot: %w", err)
	}
	return gen, nil
}

// TouchPrefixSnapshot records a conditional-GET hit: the upstream list is
// unchanged, but we asked and it answered. It does not bump the generation,
// so the already-loaded list is not reloaded for nothing.
func TouchPrefixSnapshot(ctx context.Context, db *sql.DB, etag, lastModified string) error {
	_, err := db.ExecContext(ctx, `
		UPDATE prefix_cache SET fetched_at = ?, etag = ?, last_modified = ? WHERE id = 1`,
		time.Now().Unix(), etag, lastModified)
	return err
}
