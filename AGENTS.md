# Project Agent Instructions

Guidance for AI agents working in this repository.

## What this is

An APRS message archive. `aprs-archive-collector` subscribes to APRS-IS and
writes message traffic to SQLite; `aprs-archive-web` serves
https://aprs.ea7klk.es from that file, read-only. They are separate Deployments
on purpose (see `README.md`).

## Build, test, lint

```sh
go build ./...
go test ./...
go vet ./...
gofmt -l .        # must print nothing; gofmt -w . to fix
```

Tests never touch a real archive: `internal/store` builds one in `t.TempDir()`
with the real schema, and `internal/web` renders against a seeded one. Run the
full suite before every commit.

## Invariants

0. **The collector is the only writer.** The web process opens the database
   `mode=ro` and `query_only`, and its pod mounts the volume read-only. Two
   independent mechanisms, both deliberate: keep them. A write in a web handler
   is a bug, not a feature request.
1. **The archiver stores messages, not packets.** No raw-line column. Positions,
   weather, telemetry and objects are filtered at the source (`t/m`); do not
   store them "just in case".
2. **Acks and rejects are not stored.** They parse as message-format packets so
   the parser models them, but `main.go` drops them before the writer, and
   `TestArchivedKindsExcludeAcks` pins it.
   Multi-part numbering comes in **two** shapes and both are real:
   `[1/2] body` and `body (2/3)`. A trailing `(n/m)` must be a token of its own
   and one digit over one digit — `(13/11)` is a date and `x2/3)` is not
   numbering. Message ids arrive as `{4X` (APRS101) and `{4X}` (some clients);
   both must be stripped, and the braced id must be removed **before** the
   suffix is examined, not after.
   Recognition is `internal/aprs.responseID`, and it must accept the forms that
   really arrive: `ack4A}`, `ack49}`, `ackJU}JT`, `ack`, `ACK`, `rej99}`,
   `ack{12}`, `ack 12`. It must not classify prose that begins with those
   letters (`acknowledge`, `rejoin now`) — `TestResponseRecognition` holds both
   directions. Rows an earlier build stored wrongly are removed at collector
   start by `store.PurgeMisclassifiedResponses`, which uses the same parser.
3. **Retention is enforced by the collector**, in chunks, on a nightly timer and
   once at startup. A single unbounded `DELETE` after a long outage would hold
   the write lock and grow the WAL to the size of the data removed.
4. **Third-party text is hostile.** Every string from APRS-IS passes
   `internal/sanitize` on write and is escaped by `html/template` on render;
   the CSP (`default-src 'none'`) is the second line. Never build HTML by
   concatenating a message body.
5. **Schema changes bump `store.SchemaVersion`** and must keep the web process
   from opening an older file: `OpenReader` fails closed on a version mismatch.
6. **The taxonomy is shared in spirit with `simple-ham-bbs`.** Bulletin
   categories and group meanings come from there; extend them (prefix families
   such as `WX…` are this archive's addition) rather than inventing synonyms.
7. **A multi-part message is stored once, as one row.** Parts arrive as separate
   packets with separate ids. Each part is stored as it arrives — a set whose
   last part never comes must not lose content — and when the set completes,
   `ReplaceParts` inserts the assembled row and deletes the parts by **row id**
   in one transaction, insert first. Matching parts by text would delete another
   station's identical greeting. The live path is `aprs.Assembler`; the startup
   replay `store.AssembleStoredParts` repairs sets split by a restart. Never
   assemble `[1/1]`: it is a complete single-part message, and treating it as a
   set would merge unrelated messages.
8. **Never transmit to APRS-IS.** The collector writes one login line and
   nothing else. Half-closing the socket is not an option: APRS-IS treats it as
   a dead client and drops the session immediately (observed 2026-11-13).
9. **The public server is read-only; the admin server is a separate deployment.**
   Same image, `APRS_ARCHIVE_ROLE=admin` gives a write handle. The public server
   must keep: a read-only volume mount, a `mode=ro` handle, and **no admin
   routes registered at all**. Do not register an admin route and refuse inside
   the handler — that is a surface, and its safety then depends on every future
   edit. `TestPublicServerHasNoAdminRoutes` and `TestPublicServerCannotWrite`
   pin both halves.
10. **Validation is bidirectional and exceptions beat it.** A message is stored
   only if both endpoints pass. The exception list is consulted first and wins
   in both directions, so a "drop" entry works for a station that prefixes would
   accept. Exceptions are reloaded every minute, so the admin page takes effect
   without a restart.
11. **Fleet pins digests.** `fleet/aprs-archive/resources.yaml` must reference the
   image by `@sha256:`; a tag would be reverted by drift correction.

## Visual language

`internal/web/templates/style.css` is `ham-bbs-web`'s stylesheet, carried over
so the two surfaces read as one estate. Add archive-specific rules at the bottom
(the `---- archive additions ----` block) and keep new markup on the existing
grid: `.card`/`.panel` for a box, `.strip` for a section header,
`.rows`/`.rowlink` for a list, `.status`/`.tiles` for metrics, `.pagehead` for a
page heading. No inline `style=` attributes.
