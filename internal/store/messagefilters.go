package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"aprs-archive/internal/sanitize"
)

// Message filters: patterns that mark a body as machine output.
//
// They are rows, not constants, because they encode a judgement about traffic
// that changes as devices appear on the network. The collector reads them, an
// operator edits them on the admin page, and neither needs the other to restart.

// MessageFilter is one pattern.
type MessageFilter struct {
	ID      int64
	Name    string
	Pattern string
	// Kind is "regex" or "encoded-blob".
	Kind      string
	Note      string
	Enabled   bool
	Builtin   bool
	UpdatedAt time.Time
	// Matches is filled by the admin page from a recent sample, not stored.
	Matches int64
}

// DefaultMessageFilters are the patterns seeded into a new archive.
//
// Each one was derived from observed traffic on this network, not guessed: the
// sample that motivated it is in the note. They are deliberately narrow — they
// match a *shape* that no person types — because a filter that catches a real
// message deletes something a human wrote, and that cannot be undone.
var DefaultMessageFilters = []struct {
	Name    string
	Pattern string
	// Kind defaults to regex.
	Kind string
	Note string
	// EnabledByDefault is false for a rule broad enough that the operator should
	// decide. It is seeded switched off, present and documented, one click away.
	EnabledByDefault bool
}{
	{
		Name:             "tracker status block",
		Pattern:          `^\[S[\d.]+\s+[A-Z]+[\d.]`,
		Note:             "Tracker health dump: [S88.5 L125 V2 B84 A0 R1 G0 T9 M537 D0 U4.20 C1 W1]~ed (DL6TJ-9).",
		EnabledByDefault: true,
	},
	{
		Name:             "uptime phrase",
		Pattern:          `(?i)^uptime\s*:\s*\d`,
		Note:             "Daemon uptime: 'uptime: 3d 3h 30m 37s' (SV9TNF-3).",
		EnabledByDefault: true,
	},
	{
		Name:             "uptime assignment",
		Pattern:          `(?i)^UPTIME\s*=\s*(up|down|\d)`,
		Note:             "Daemon uptime with a version: 'UPTIME=up 1 week, 5 days v20250629' (AE5TC-13).",
		EnabledByDefault: true,
	},
	{
		Name:             "uptime compact",
		Pattern:          `(?i)^UPTM\.`,
		Note:             "Compact uptime beacon: 'UPTM.13T06:36:40' (YO3JAZ-EY, FW0690, YO3JAZ-NC).",
		EnabledByDefault: true,
	},
	{
		Name:             "weather summary",
		Pattern:          `(?i)^WX:\s*T=`,
		Note:             "Weather gateway summary: 'WX: T=26C H=78% W=0.5m/s P=1014hPa' (HS3LIQ-13).",
		EnabledByDefault: true,
	},
	{
		Name:             "instrument key=value dump",
		Pattern:          `(?i)^[A-Z]{1,5}=[\d.:-]+\s+[A-Z]{1,5}=[\d.:-]+\s+[A-Z]{1,5}=[\d.:-]+`,
		Note:             "Instrument readout: 'T=11:47:59 A=29.6 B=0.96 C=0.92 RF=1 RP=0 ...' (IZ6RND). Three key=value pairs at the start of the body.",
		EnabledByDefault: true,
	},
	{
		// Two or more comma-separated numbers and nothing else. The end anchor
		// is what separates a reading from a sentence: "9.3,99.9" is a sensor
		// output, "1,2,3 let's go" is somebody counting.
		Name:             "numeric series",
		Pattern:          `^\s*-?[\d.]+\s*,\s*-?[\d.]+(\s*,\s*-?[\d.]+)*\s*$`,
		Note:             "Comma-separated readings and nothing else: '7.8,99.9' (PE1RXF-6), '270.0,1.8,3.5,...' (PE1RXF-13).",
		EnabledByDefault: true,
	},
	{
		// Broad on purpose: a frame checksum ending a body is not something a
		// person types, and every match in the survey was tracker output. It
		// also catches short machine replies ("HA 8~c0"), which is why it ships
		// switched off — the operator decides.
		Name:    "frame checksum suffix",
		Pattern: `~[0-9A-Fa-f]{2}$`,
		Note:    "Body ending in a two-character frame checksum: 'ok ST [S92.6 ...]~89', 'HA 8~c0', 'err busy~ad' (DL6TJ, DL6TJ-9). Review the matches before enabling.",
	},
	{
		// Opaque encoded payloads: encrypted messages, packed frames, file
		// fragments. Detected in code because a regex cannot say "looks random";
		// see internal/telemetry/blob.go. Ships off: it removes content that has
		// no readable words, which is the operator's call to make.
		Name:    "encoded blob",
		Kind:    "encoded-blob",
		Pattern: "base64-like opaque run",
		Note:    "A long base64-ish body with no words: '2NbMAEKDSEp5R7up8X84D83W5KDr9EPJ4SycmVdwuQ' (LA4HQ <-> LA3HQ). Review the matches before enabling.",
	},
	{
		Name:    "memory dump",
		Pattern: `^[A-Z]\d+/\d+\s+\d+:[0-9a-fuws]{16,}`,
		Note:    "Position-indexed dump: 'M46/79 135:0u0000042s00s000u...' (DL6TJ-9).",
	},
}

// EnsureMessageFilters seeds the default patterns on a new archive.
//
// It is idempotent and never overwrites an operator's edit: a builtin row is
// inserted only when no row with that name exists at all.
func EnsureMessageFilters(ctx context.Context, db *sql.DB) error {
	for _, f := range DefaultMessageFilters {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM message_filters WHERE name = ?`, f.Name).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			continue
		}
		enabled := 0
		if f.EnabledByDefault {
			enabled = 1
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO message_filters (name, pattern, kind, note, enabled, builtin, updated_at)
			VALUES (?, ?, ?, ?, ?, 1, ?)`, f.Name, f.Pattern, kindOr(f.Kind), sanitize.SingleLine(f.Note), enabled, time.Now().Unix()); err != nil {
			return fmt.Errorf("seed filter %q: %w", f.Name, err)
		}
	}
	_, err := db.ExecContext(ctx, `INSERT INTO message_filters_state (id, generation) VALUES (1, 1) ON CONFLICT(id) DO NOTHING`)
	return err
}

// ListMessageFilters returns every pattern, enabled or not.
func ListMessageFilters(ctx context.Context, db *sql.DB) ([]MessageFilter, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, name, pattern, COALESCE(kind,'regex'), note, enabled, builtin, updated_at
		FROM message_filters ORDER BY enabled DESC, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MessageFilter
	for rows.Next() {
		var f MessageFilter
		var enabled, builtin int
		var at int64
		if err := rows.Scan(&f.ID, &f.Name, &f.Pattern, &f.Kind, &f.Note, &enabled, &builtin, &at); err != nil {
			return nil, err
		}
		f.Enabled = enabled == 1
		f.Builtin = builtin == 1
		f.UpdatedAt = time.Unix(at, 0).UTC()
		out = append(out, f)
	}
	return out, rows.Err()
}

// EnabledMessageFilters returns the patterns the collector should apply.
func EnabledMessageFilters(ctx context.Context, db *sql.DB) ([]MessageFilter, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, name, pattern, COALESCE(kind,'regex'), note, enabled, builtin, updated_at
		FROM message_filters WHERE enabled = 1 ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MessageFilter
	for rows.Next() {
		var f MessageFilter
		var enabled, builtin int
		var at int64
		if err := rows.Scan(&f.ID, &f.Name, &f.Pattern, &f.Kind, &f.Note, &enabled, &builtin, &at); err != nil {
			return nil, err
		}
		f.Enabled = enabled == 1
		f.Builtin = builtin == 1
		f.UpdatedAt = time.Unix(at, 0).UTC()
		out = append(out, f)
	}
	return out, rows.Err()
}

// MessageFiltersGeneration is the cheap check the collector's tick makes.
func MessageFiltersGeneration(ctx context.Context, db *sql.DB) (int64, error) {
	var gen int64
	err := db.QueryRowContext(ctx, `SELECT generation FROM message_filters_state WHERE id = 1`).Scan(&gen)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return gen, err
}

func bumpFilterGeneration(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO message_filters_state (id, generation) VALUES (1, 1)
		ON CONFLICT(id) DO UPDATE SET generation = message_filters_state.generation + 1`)
	return err
}

// SaveMessageFilter inserts or updates a pattern and bumps the generation.
func SaveMessageFilter(ctx context.Context, db *sql.DB, f MessageFilter) (int64, error) {
	name := sanitize.SingleLine(f.Name)
	if name == "" {
		return 0, errors.New("a name is required")
	}
	if len(name) > 80 {
		return 0, errors.New("the name is too long")
	}
	if f.Pattern == "" {
		return 0, errors.New("a pattern is required")
	}
	enabled := 0
	if f.Enabled {
		enabled = 1
	}
	var id int64
	if f.ID == 0 {
		res, err := db.ExecContext(ctx, `
			INSERT INTO message_filters (name, pattern, kind, note, enabled, builtin, updated_at)
			VALUES (?, ?, ?, ?, ?, 0, ?)`,
			name, f.Pattern, kindOr(f.Kind), sanitize.SingleLine(f.Note), enabled, time.Now().Unix())
		if err != nil {
			return 0, err
		}
		id, _ = res.LastInsertId()
	} else {
		if _, err := db.ExecContext(ctx, `
			UPDATE message_filters SET name = ?, pattern = ?, kind = ?, note = ?, enabled = ?, updated_at = ?
			WHERE id = ?`,
			name, f.Pattern, kindOr(f.Kind), sanitize.SingleLine(f.Note), enabled, time.Now().Unix(), f.ID); err != nil {
			return 0, err
		}
		id = f.ID
	}
	return id, bumpFilterGeneration(ctx, db)
}

// DeleteMessageFilter removes a pattern and bumps the generation.
func DeleteMessageFilter(ctx context.Context, db *sql.DB, id int64) error {
	if _, err := db.ExecContext(ctx, `DELETE FROM message_filters WHERE id = ?`, id); err != nil {
		return err
	}
	return bumpFilterGeneration(ctx, db)
}

// SetMessageFilterEnabled toggles one pattern.
func SetMessageFilterEnabled(ctx context.Context, db *sql.DB, id int64, enabled bool) error {
	v := 0
	if enabled {
		v = 1
	}
	if _, err := db.ExecContext(ctx, `
		UPDATE message_filters SET enabled = ?, updated_at = ? WHERE id = ?`,
		v, time.Now().Unix(), id); err != nil {
		return err
	}
	return bumpFilterGeneration(ctx, db)
}

// PurgeTelemetry deletes stored messages whose body matches a filter rule.
//
// It asks the caller's predicate rather than a LIKE pattern, so the cleanup and
// the collector cannot disagree about what telemetry is. The walk is by
// ascending id — a keyset cursor, not OFFSET — because the collector keeps
// writing while this runs.
func PurgeTelemetry(ctx context.Context, db *sql.DB, isTelemetry func(string) bool) (int64, error) {
	const batch = 2000
	var total int64
	var cursor int64
	for {
		rows, err := db.QueryContext(ctx,
			`SELECT id, msg_text FROM messages WHERE id > ? ORDER BY id LIMIT ?`, cursor, batch)
		if err != nil {
			return total, err
		}
		var ids []any
		var scanned int
		for rows.Next() {
			var id int64
			var text string
			if err := rows.Scan(&id, &text); err != nil {
				rows.Close()
				return total, err
			}
			scanned++
			cursor = id
			if isTelemetry(text) {
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

// PurgeFilterMatches deletes every stored message that one pattern matches.
//
// It runs when a filter becomes active — on insert of a new rule, on edit, and
// on enable — so that "add a filter" means the archive reflects the rule
// immediately rather than only for traffic from now on. Without it the operator
// enables a rule that visibly changes nothing, which is the confusing case.
//
// It asks the caller's predicate, so the cleanup and the collector use the same
// compiled pattern. The walk is by ascending id — a keyset cursor, not OFFSET —
// because the collector keeps writing while this runs, and each chunk commits on
// its own so the write lock is released between them.
//
// It returns how many rows it removed.
func PurgeFilterMatches(ctx context.Context, db *sql.DB, matches func(string) bool) (int64, error) {
	const batch = 2000
	var total int64
	var cursor int64
	for {
		rows, err := db.QueryContext(ctx,
			`SELECT id, msg_text FROM messages WHERE id > ? ORDER BY id LIMIT ?`, cursor, batch)
		if err != nil {
			return total, err
		}
		var ids []any
		var scanned int
		for rows.Next() {
			var id int64
			var text string
			if err := rows.Scan(&id, &text); err != nil {
				rows.Close()
				return total, err
			}
			scanned++
			cursor = id
			if matches(text) {
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

// CountFilterMatches reports how many stored messages one pattern matches,
// without deleting anything. The admin page uses it to tell the operator what a
// save is about to remove before they confirm.
func CountFilterMatches(ctx context.Context, db *sql.DB, matches func(string) bool) (int64, error) {
	rows, err := db.QueryContext(ctx, `SELECT msg_text FROM messages`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var n int64
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			return n, err
		}
		if matches(text) {
			n++
		}
	}
	return n, rows.Err()
}

// kindOr defaults an empty kind to regex, so a caller that predates the column
// keeps working.
func kindOr(k string) string {
	if k == "" {
		return "regex"
	}
	return k
}
