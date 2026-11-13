package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"aprs-archive/internal/sanitize"
)

// Exception is one operator decision about a callsign.
type Exception struct {
	Callsign  string
	Valid     bool
	Note      string
	UpdatedAt time.Time
}

// ListExceptions returns every exception, newest decision first.
func ListExceptions(ctx context.Context, db *sql.DB) ([]Exception, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT callsign, valid, note, updated_at
		FROM callsign_exceptions ORDER BY callsign COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Exception
	for rows.Next() {
		var e Exception
		var valid int
		var at int64
		if err := rows.Scan(&e.Callsign, &valid, &e.Note, &at); err != nil {
			return nil, err
		}
		e.Valid = valid == 1
		e.UpdatedAt = time.Unix(at, 0).UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

// LoadExceptions returns the decisions as a lookup map keyed by upper-case
// callsign. The collector loads it periodically, so validation costs one map
// read per packet rather than a query.
func LoadExceptions(ctx context.Context, db *sql.DB) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT callsign, valid FROM callsign_exceptions`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var c string
		var valid int
		if err := rows.Scan(&c, &valid); err != nil {
			return nil, err
		}
		out[strings.ToUpper(c)] = valid == 1
	}
	return out, rows.Err()
}

// SetException records a decision, replacing any previous one for the callsign.
func SetException(ctx context.Context, db *sql.DB, callsign string, valid bool, note string) error {
	c := strings.ToUpper(strings.TrimSpace(callsign))
	if c == "" {
		return fmt.Errorf("callsign is required")
	}
	if len(c) > 24 {
		return fmt.Errorf("callsign is too long")
	}
	v := 0
	if valid {
		v = 1
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO callsign_exceptions (callsign, valid, note, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(callsign) DO UPDATE SET
		  valid = excluded.valid,
		  note = excluded.note,
		  updated_at = excluded.updated_at`,
		c, v, sanitize.SingleLine(note), time.Now().Unix())
	return err
}

// DeleteException removes a decision, returning to prefix validation.
func DeleteException(ctx context.Context, db *sql.DB, callsign string) error {
	_, err := db.ExecContext(ctx, `DELETE FROM callsign_exceptions WHERE callsign = ?`, strings.ToUpper(strings.TrimSpace(callsign)))
	return err
}

// PurgeInvalidCallsigns deletes every stored message whose sender or addressee
// the given predicate rejects.
//
// This is how the archive is cleaned after validation is introduced or a new
// bot is identified. It asks the caller's predicate rather than matching a
// pattern, so the cleanup and the collector cannot disagree about what a bot is.
//
// It walks the table by ascending id — a keyset cursor, not OFFSET — because the
// collector keeps writing while this runs and OFFSET would skip rows. Each chunk
// commits on its own so the write lock is released between them.
func PurgeInvalidCallsigns(ctx context.Context, db *sql.DB, keep func(string) bool) (int64, error) {
	const batch = 2000
	var total int64
	var cursor int64
	for {
		rows, err := db.QueryContext(ctx,
			`SELECT id, from_callsign, to_callsign FROM messages WHERE id > ? ORDER BY id LIMIT ?`,
			cursor, batch)
		if err != nil {
			return total, err
		}
		var ids []any
		var scanned int
		for rows.Next() {
			var id int64
			var from, to string
			if err := rows.Scan(&id, &from, &to); err != nil {
				rows.Close()
				return total, err
			}
			scanned++
			cursor = id
			if !keep(from) || !keep(to) {
				ids = append(ids, id)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return total, err
		}
		if len(ids) > 0 {
			placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
			res, err := db.ExecContext(ctx, "DELETE FROM messages WHERE id IN ("+placeholders+")", ids...)
			if err != nil {
				return total, err
			}
			n, _ := res.RowsAffected()
			total += n
		}
		if scanned < batch {
			break
		}
		if ctx.Err() != nil {
			return total, ctx.Err()
		}
	}
	return total, ctx.Err()
}
