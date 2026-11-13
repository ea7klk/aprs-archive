package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"strings"
	"time"

	"aprs-archive/internal/aprs"
	"aprs-archive/internal/sanitize"
)

// DefaultFlushInterval is how often the collector commits a batch. Short enough
// that a dashboard does not look stalled, long enough that a busy feed commits
// a handful of times per minute rather than thousands.
const DefaultFlushInterval = 2 * time.Second

// DefaultBatchSize bounds a single transaction, so a burst cannot hold the
// write lock for long or build an unbounded slice in memory.
const DefaultBatchSize = 2000

// Writer is the collector's handle on the archive. It is not safe for
// concurrent use: the collector's single write loop owns it.
type Writer struct {
	db  *sql.DB
	tx  *sql.Tx
	ins *sql.Stmt
	bul *sql.Stmt
	// dirty counts rows written into the open transaction.
	dirty int
}

// NewWriter prepares the writer against an already-migrated database.
func NewWriter(ctx context.Context, db *sql.DB) (*Writer, error) {
	w := &Writer{db: db}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO collector_state (id, started_at, last_write_at)
		VALUES (1, ?, 0)
		ON CONFLICT(id) DO UPDATE SET started_at = excluded.started_at`,
		time.Now().Unix()); err != nil {
		return nil, err
	}
	return w, nil
}

// Record stores one parsed packet.
//
// The caller has already classified it; this function only normalises and
// writes. Bulletins go to both tables: `messages` keeps the transmission
// history the archive promises, `bulletins` keeps the deduplicated copy a
// reader browses.
func (w *Writer) Record(ctx context.Context, p aprs.Packet, at time.Time) error {
	if err := w.open(ctx); err != nil {
		return err
	}
	from := sanitize.SingleLine(p.From)
	to := sanitize.SingleLine(p.To)
	text := sanitize.SingleLine(p.Text)
	if text == "" {
		return nil
	}
	category := ""
	if p.Kind == aprs.KindBulletin {
		category = aprs.CategoryFor(p.Bulletin.Group, text)
	}
	if _, err := w.ins.ExecContext(ctx,
		at.Unix(), from, to, text, string(p.Kind), p.MessageID, category); err != nil {
		return err
	}
	w.dirty++
	if p.Kind == aprs.KindBulletin {
		key := bulletinKey(from, p.Bulletin.Group, text)
		if _, err := w.bul.ExecContext(ctx,
			key, at.Unix(), at.Unix(), from, p.Bulletin.Slot,
			sanitize.SingleLine(p.Bulletin.Group), category, text); err != nil {
			return err
		}
	}
	if w.dirty >= DefaultBatchSize {
		return w.commit(ctx)
	}
	return nil
}

// Flush commits the open transaction, if any.
func (w *Writer) Flush(ctx context.Context) error {
	return w.commit(ctx)
}

// Close flushes and releases statements. The database handle is closed by the
// caller, which also owns it for retention and state updates.
func (w *Writer) Close(ctx context.Context) error {
	// commit closes the statement handles, which belong to the transaction.
	return w.commit(ctx)
}

// Dirty reports whether a transaction is open with uncommitted rows.
func (w *Writer) Dirty() bool { return w.dirty > 0 }

func (w *Writer) open(ctx context.Context) error {
	if w.tx != nil {
		return nil
	}
	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	w.tx = tx
	if w.ins, err = tx.PrepareContext(ctx,
		`INSERT INTO messages (received_at, from_callsign, to_callsign, msg_text, kind, message_id, category)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`); err != nil {
		w.abort()
		return err
	}
	if w.bul, err = tx.PrepareContext(ctx,
		`INSERT INTO bulletins (dedupe_key, first_seen, last_seen, seen_count, from_callsign, slot, grp, category, text)
		 VALUES (?, ?, ?, 1, ?, ?, ?, ?, ?)
		 ON CONFLICT(dedupe_key) DO UPDATE SET
		   last_seen  = excluded.last_seen,
		   seen_count = seen_count + 1`); err != nil {
		w.abort()
		return err
	}
	return nil
}

func (w *Writer) commit(ctx context.Context) error {
	if w.tx == nil {
		return nil
	}
	// Statement handles belong to the transaction; close them before either
	// path so a rollback cannot leave a prepared statement pointing at a dead
	// transaction.
	if w.ins != nil {
		_ = w.ins.Close()
	}
	if w.bul != nil {
		_ = w.bul.Close()
	}
	w.ins, w.bul = nil, nil
	n := w.dirty
	err := w.tx.Commit()
	w.tx = nil
	w.dirty = 0
	if err != nil {
		return err
	}
	if n > 0 {
		_, _ = w.db.ExecContext(ctx,
			`UPDATE collector_state SET last_write_at = ?, packets_stored = packets_stored + ? WHERE id = 1`,
			time.Now().Unix(), n)
	}
	return nil
}

func (w *Writer) abort() {
	if w.tx != nil {
		_ = w.tx.Rollback()
	}
	w.tx = nil
	w.dirty = 0
}

// SetConnected records a successful APRS-IS login.
func SetConnected(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `UPDATE collector_state SET last_connect_at = ?, connects = connects + 1 WHERE id = 1`, time.Now().Unix())
	return err
}

// SetError records the reason the collector last lost its connection, so the
// web page can show it without reading pod logs.
func SetError(ctx context.Context, db *sql.DB, msg string) error {
	if len(msg) > 500 {
		msg = msg[:500]
	}
	_, err := db.ExecContext(ctx, `UPDATE collector_state SET last_error = ? WHERE id = 1`, sanitize.SingleLine(msg))
	return err
}

// AddSeen counts packets heard, including the ones not stored.
func AddSeen(ctx context.Context, db *sql.DB, n int64) error {
	if n <= 0 {
		return nil
	}
	_, err := db.ExecContext(ctx, `UPDATE collector_state SET packets_seen = packets_seen + ? WHERE id = 1`, n)
	return err
}

// Retention is how long a message or a bulletin stays readable.
type Retention struct {
	Messages  time.Duration
	Bulletins time.Duration
}

// Sweep deletes rows older than the retention window in bounded chunks.
//
// It runs in chunks because one delete of a year's backlog inside a single
// transaction would hold the write lock, grow the WAL to the size of the data
// it is removing, and on restart-after-an-outage that is exactly the state it
// finds. Each chunk commits separately so the collector and the web interface
// keep working throughout.
func Sweep(ctx context.Context, db *sql.DB, r Retention, now time.Time) (messages, bulletins int64, err error) {
	const chunk = 2000
	for {
		res, err := db.ExecContext(ctx,
			`DELETE FROM messages WHERE id IN (SELECT id FROM messages WHERE received_at < ? LIMIT ?)`,
			now.Add(-r.Messages).Unix(), chunk)
		if err != nil {
			return messages, bulletins, err
		}
		n, _ := res.RowsAffected()
		messages += n
		if n < chunk || ctx.Err() != nil {
			break
		}
	}
	res, err := db.ExecContext(ctx,
		`DELETE FROM bulletins WHERE last_seen < ?`, now.Add(-r.Bulletins).Unix())
	if err != nil {
		return messages, bulletins, err
	}
	bulletins, _ = res.RowsAffected()
	return messages, bulletins, ctx.Err()
}

// Optimize runs a bounded incremental vacuum and refreshes the query planner's
// statistics after a sweep, so the file shrinks without rewriting the whole
// database every night.
func Optimize(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `PRAGMA incremental_vacuum(4000)`); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx, `PRAGMA optimize`)
	return err
}

// bulletinKey identifies a bulletin by sender, group and normalised text, so a
// repeat updates the existing row instead of adding another.
func bulletinKey(from, group, text string) string {
	normalised := strings.Join(strings.Fields(sanitize.SingleLine(text)), " ")
	sum := sha256.Sum256([]byte(from + "|" + strings.ToUpper(group) + "|" + normalised))
	return hex.EncodeToString(sum[:])
}
