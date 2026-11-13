-- Archive schema. The collector owns this file; the web process only reads it.
--
-- Two tables. `messages` holds every message-format packet heard, one row per
-- packet, because the archive's job is to keep what was on the air. `bulletins`
-- is deduplicated by content because senders repeat the same bulletin every few
-- minutes and a reader wants the bulletin, not each transmission.
--
-- No raw packet column: the raw line is available from APRS-IS live, and the
-- archive's contract says it stores messages, not packets.

CREATE TABLE IF NOT EXISTS messages (
    -- AUTOINCREMENT: ids must never be reused, so a deleted row's id cannot
    -- reappear as a different packet in a report a user has already saved.
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    -- Whole seconds since the Unix epoch, UTC. Integer time keeps the index
    -- small and comparisons exact; sub-second ordering comes from id.
    received_at   INTEGER NOT NULL,
    -- Source callsign with SSID, upper case, as heard.
    from_callsign TEXT    NOT NULL,
    -- Addressee: callsign, `BLNn`+group, or a software group.
    to_callsign   TEXT    NOT NULL,
    -- Body with the message ID removed, at most 67 characters.
    msg_text      TEXT    NOT NULL,
    -- message | bulletin | ack | rej
    kind          TEXT    NOT NULL,
    -- Sender's message ID, empty when absent.
    message_id    TEXT    NOT NULL DEFAULT '',
    -- For kind=bulletin: the category resolved by internal/aprs.
    category      TEXT    NOT NULL DEFAULT ''
);

-- Time-ordered scans for search, stats and reports. id breaks ties so paging is
-- stable when many packets share a second.
CREATE INDEX IF NOT EXISTS idx_messages_received ON messages (received_at, id);
-- Per-station history, newest first.
CREATE INDEX IF NOT EXISTS idx_messages_from ON messages (from_callsign, received_at DESC);
-- Per-recipient history, newest first.
CREATE INDEX IF NOT EXISTS idx_messages_to ON messages (to_callsign, received_at DESC);
-- Statistics by kind inside a window, without touching the table body.
CREATE INDEX IF NOT EXISTS idx_messages_kind ON messages (kind, received_at);

CREATE TABLE IF NOT EXISTS bulletins (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    -- SHA-256 of sender, group and normalised text. A repeat updates the row.
    dedupe_key    TEXT    NOT NULL UNIQUE,
    first_seen    INTEGER NOT NULL,
    last_seen     INTEGER NOT NULL,
    seen_count    INTEGER NOT NULL DEFAULT 1,
    from_callsign TEXT    NOT NULL,
    slot          INTEGER NOT NULL DEFAULT 0,
    grp           TEXT    NOT NULL DEFAULT '',
    category      TEXT    NOT NULL DEFAULT '',
    text          TEXT    NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_bulletins_last_seen ON bulletins (last_seen DESC);
CREATE INDEX IF NOT EXISTS idx_bulletins_category ON bulletins (category, last_seen DESC);

-- Collector liveness, so the web page can say "the collector last wrote at ..."
-- without a Prometheus dependency. One row.
CREATE TABLE IF NOT EXISTS collector_state (
    id             INTEGER PRIMARY KEY CHECK (id = 1),
    started_at     INTEGER NOT NULL,
    last_write_at  INTEGER NOT NULL,
    last_connect_at INTEGER NOT NULL DEFAULT 0,
    packets_seen   INTEGER NOT NULL DEFAULT 0,
    packets_stored INTEGER NOT NULL DEFAULT 0,
    connects       INTEGER NOT NULL DEFAULT 0,
    last_error     TEXT    NOT NULL DEFAULT ''
);

-- Callsigns an operator has forced valid or invalid.
--
-- Prefix validation cannot tell a station from a robot that named itself like
-- one, and it cannot know that a particular gateway is wanted. `valid = 1`
-- keeps a callsign the prefix list would reject (APRSPH is the standing
-- example); `valid = 0` rejects one it would accept. The collector consults
-- this before the prefix list, so an override always wins.
CREATE TABLE IF NOT EXISTS callsign_exceptions (
    callsign  TEXT PRIMARY KEY COLLATE NOCASE,
    valid     INTEGER NOT NULL CHECK (valid IN (0, 1)),
    note      TEXT NOT NULL DEFAULT '',
    updated_at INTEGER NOT NULL
);
