package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"strings"
	"sync"
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

// Writer is the collector's handle on the archive.
//
// It is safe for concurrent use through one mutex. That is not belt and braces:
// the collector records packets on the read goroutine while a separate flush
// goroutine commits the open batch on a timer, and those two touched the same
// statement handles. A flush that ran between a lookup and a write cleared
// w.ins, and the packet path then dereferenced nil — the panic that crashed the
// collector on 2026-11-13. The mutex makes the commit and the write mutually
// exclusive, which is also what the single SQLite write lock requires anyway.
type Writer struct {
	mu  sync.Mutex
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

// RecordPacket stores one packet and returns the row id the archive assigned to
// it, so the caller can replace the row later.
//
// Multi-part messages need this: each part is stored as it arrives, because a
// set whose last part never comes must not lose what was said, and when the
// set does complete the stored parts are replaced by one row.
func (w *Writer) RecordPacket(ctx context.Context, p aprs.Packet, at time.Time) (int64, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.open(ctx); err != nil {
		return 0, err
	}
	from := sanitize.SingleLine(p.From)
	to := sanitize.SingleLine(p.To)
	text := sanitize.SingleLine(p.Text)
	if text == "" {
		return 0, nil
	}
	category := ""
	if p.Kind == aprs.KindBulletin {
		category = aprs.CategoryFor(p.Bulletin.Group, text)
	}
	res, err := w.ins.ExecContext(ctx,
		at.Unix(), from, to, text, string(p.Kind), p.MessageID, category)
	if err != nil {
		return 0, err
	}
	w.dirty++
	if p.Kind == aprs.KindBulletin {
		key := bulletinKey(from, p.Bulletin.Group, text)
		if _, err := w.bul.ExecContext(ctx,
			key, at.Unix(), at.Unix(), from, p.Bulletin.Slot,
			sanitize.SingleLine(p.Bulletin.Group), category, text); err != nil {
			return 0, err
		}
	}
	if w.dirty >= DefaultBatchSize {
		if err := w.commit(ctx); err != nil {
			return 0, err
		}
	}
	id, err := res.LastInsertId()
	return id, err
}

// ReplaceParts turns the stored parts of one multi-part message into a single
// assembled row, in one transaction.
//
// Order matters. The assembled row is inserted first and the part rows deleted
// after, so a crash between the two leaves two copies — visible, and fixed by
// the next run — rather than none. The reverse order would delete what has been
// said and then fail to write its replacement.
//
// The parts are identified by id, not by text: two different stations send the
// same greeting, and a text match would delete somebody else's message.
func (w *Writer) ReplaceParts(ctx context.Context, partIDs []int64, assembled aprs.Packet, at time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	// The open batch is committed first: the archive has one connection, and
	// the transaction below needs it. Committing first also means the parts are
	// definitely on disk before anything deletes them.
	if err := w.commit(ctx); err != nil {
		return err
	}
	return replaceParts(ctx, w.db, partIDs, assembled, at)
}

// replaceParts is the one implementation, shared by the live path and the
// startup replay.
func replaceParts(ctx context.Context, db *sql.DB, partIDs []int64, assembled aprs.Packet, at time.Time) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	category := ""
	if assembled.Kind == aprs.KindBulletin {
		category = aprs.CategoryFor(assembled.Bulletin.Group, assembled.Text)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO messages (received_at, from_callsign, to_callsign, msg_text, kind, message_id, category)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		at.Unix(),
		sanitize.SingleLine(assembled.From),
		sanitize.SingleLine(assembled.To),
		sanitize.SingleLine(assembled.Text),
		string(assembled.Kind),
		assembled.MessageID,
		category); err != nil {
		return err
	}
	for _, id := range partIDs {
		if _, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE id = ?`, id); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	_, _ = db.ExecContext(ctx,
		`UPDATE collector_state SET last_write_at = ? WHERE id = 1`, time.Now().Unix())
	return nil
}

// Record stores one parsed packet.
//
// The caller has already classified it; this function only normalises and
// writes. Bulletins go to both tables: `messages` keeps the transmission
// history the archive promises, `bulletins` keeps the deduplicated copy a
// reader browses.
func (w *Writer) Record(ctx context.Context, p aprs.Packet, at time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()
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
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.commit(ctx)
}

// Close flushes and releases statements. The database handle is closed by the
// caller, which also owns it for retention and state updates.
func (w *Writer) Close(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	// commit closes the statement handles, which belong to the transaction.
	return w.commit(ctx)
}

// Dirty reports whether a transaction is open with uncommitted rows.
func (w *Writer) Dirty() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.dirty > 0
}

func (w *Writer) open(ctx context.Context) error {
	// The guard is on the statements, not only on the transaction. ReplaceParts
	// commits through commit(), which closes the statement handles, so a writer
	// whose transaction is nil *and* whose statements are nil must rebuild
	// both. Treating a non-nil transaction as "ready" is what let a nil
	// statement reach ExecContext and panic the collector.
	if w.tx != nil && w.ins != nil && w.bul != nil {
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

// abort rolls back an open transaction and forgets everything bound to it.
//
// The statements belong to the transaction. Leaving them set after a rollback
// left the writer holding handles bound to a dead transaction, which is exactly
// the state that produced a nil-statement panic in production: open() saw a
// transaction-less writer, tried to prepare again, and the stale handle was
// either reused or nil.
func (w *Writer) abort() {
	if w.ins != nil {
		_ = w.ins.Close()
	}
	if w.bul != nil {
		_ = w.bul.Close()
	}
	w.ins, w.bul = nil, nil
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

// PurgeMisclassifiedResponses deletes acknowledgements and rejections that an
// earlier version stored as ordinary messages.
//
// This exists because the first recogniser only accepted `ack<id>` with nothing
// after it, so real traffic — `ack49}`, `ackJU}JT` — was archived as a message
// and has sat in the counts ever since. Those rows are not traffic, and leaving
// them would keep them in the statistics and in every report long after the
// collector stopped producing new ones.
//
// The decision comes from the parser, not from a LIKE pattern here. One
// recogniser means the archive cannot disagree with itself about what an
// acknowledgement is, and a fix to the parser repairs old rows too.
//
// It is chunked for the same reason the retention sweep is. It returns how many
// rows it removed, so the caller can log the one-off transition and stay quiet
// afterwards.
func PurgeMisclassifiedResponses(ctx context.Context, db *sql.DB) (int64, error) {
	const chunk = 2000
	var total int64
	// The batch is taken newest-first: the page never needs to be complete for
	// the DELETE to be correct, and the loop stops as soon as a batch contains
	// no misclassification.
	var cursor int64 = 1 << 62
	for {
		rows, err := db.QueryContext(ctx, `
			SELECT id, msg_text FROM messages
			WHERE kind = 'message' AND id < ?
			ORDER BY id DESC LIMIT ?`, cursor, chunk)
		if err != nil {
			return total, err
		}
		var ids []any
		var lowest int64
		for rows.Next() {
			var id int64
			var text string
			if err := rows.Scan(&id, &text); err != nil {
				rows.Close()
				return total, err
			}
			lowest = id
			if aprs.IsResponse(text) {
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
		if lowest <= 1 {
			break
		}
		cursor = lowest
		if ctx.Err() != nil {
			return total, ctx.Err()
		}
	}
	return total, ctx.Err()
}

// AssembleStoredParts replays archived numbered parts through the assembler and
// collapses each complete set into one message.
//
// The live path cannot cover this on its own: the assembly window is minutes
// long, so a collector restart between part one and part two — a rollout, a
// network drop, a crash — would leave the pair split forever. The rows are
// already on disk, so replaying them closes that gap.
//
// Three row shapes are read, because the stored text depends on which build
// wrote it:
//
//   - prefixed parts, whose body begins with `[`;
//   - trailing-numbered parts, ending in `)`;
//   - the same, as older builds stored them, ending in the braced message id
//     (`... (2/3){4Y}`) — the id used to stay in the text.
//
// All three are narrow enough to skip the archive's bulk, and anything that
// survives them still has to be accepted by the parser: a body that merely ends
// in a bracket — a telemetry frame, a sentence in parentheses — is not a part.
//
// Rows are fed in arrival order and the assembler's clock is set to each row's
// timestamp, so its timeout applies to the archive's timeline — a set stored
// days apart is still rejected — while the scan itself does not depend on when
// it runs.
//
// It returns how many messages it assembled.
func AssembleStoredParts(ctx context.Context, db *sql.DB, now time.Time) (int, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, received_at, from_callsign, to_callsign, msg_text, message_id
		FROM messages
		WHERE kind = 'message'
		  AND (substr(msg_text, 1, 1) = '['
		       OR substr(rtrim(msg_text), -1, 1) = ')'
		       OR substr(rtrim(msg_text), -1, 1) = '}')
		ORDER BY received_at ASC, id ASC`)
	if err != nil {
		return 0, err
	}
	type storedPart struct {
		id     int64
		at     time.Time
		packet aprs.Packet
	}
	var parts []storedPart
	for rows.Next() {
		var id, at int64
		var from, to, text, messageID string
		if err := rows.Scan(&id, &at, &from, &to, &text, &messageID); err != nil {
			rows.Close()
			return 0, err
		}
		parts = append(parts, storedPart{
			id: id, at: time.Unix(at, 0).UTC(),
			packet: aprs.Packet{From: from, To: to, Text: text, Kind: aprs.KindMessage, MessageID: messageID},
		})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	assembler := aprs.NewAssembler(nil)
	assembled := 0
	for _, part := range parts {
		clock := part.at
		assembler.SetClock(func() time.Time { return clock })
		message, replaced, complete := assembler.Add(part.packet, part.id)
		if !complete || len(replaced) < 2 {
			continue
		}
		clock = now
		assembler.SetClock(func() time.Time { return clock })
		if err := replaceParts(ctx, db, replaced, message, part.at); err != nil {
			return assembled, err
		}
		assembled++
	}
	return assembled, ctx.Err()
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
