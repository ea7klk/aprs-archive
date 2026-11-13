# APRS Archive

An archive of APRS message traffic from APRS-IS, with a public read-only web
interface for searching it, summarising it, reading bulletins from it, and
building reports over it.

Two binaries, one repository, one database:

- **`aprs-archive-collector`** subscribes to APRS-IS (callsign `EA7KLK-15`,
  filter `t/m`, receive-only) and writes every message-format packet to SQLite.
  It is the only writer.
- **`aprs-archive-web`** serves https://aprs.ea7klk.es from the same file,
  opened read-only. It never writes.

They are separate Deployments so that changing the web interface cannot
interrupt collection, and so that a slow report cannot hold the collector's
write lock.

## What is stored

| Stored | Not stored |
| --- | --- |
| Direct messages between stations | Positions, weather beacons, telemetry, objects |
| Bulletin transmissions (`BLNn` + group) | The raw packet line |
| Sender, addressee, body (≤ 67 chars), message ID, kind | Acknowledgements and rejections |
| Bulletins deduplicated by content, with a transmission count | Rollups or aggregates |

Retention is **365 days** for both messages and bulletins, swept nightly.

A **report** covers at most **92 days** (three months). Retention is how much
history exists; the reporting limit is how much of it one query may scan. A
longer range is trimmed from the start and the page says so — the end of the
range is kept, because that is the part a visitor asked for last.

Acknowledgements and rejections are message-format packets, so the parser
classifies them — but they are delivery receipts, not traffic, and the archive
keeps what was said, not the bookkeeping around it. They are dropped in the
collector (`cmd/aprs-archive-collector/main.go`) and pinned by
`TestArchivedKindsExcludeAcks`.

### Size

Measured rate on 2026-11-13 was 0.32 message packets/s, about 27,600 a day.
At worst-case 67-character bodies and three indexes that is about **1.7 GiB per
year**; with realistic bodies about 1.1 GiB. The claim is provisioned at 5 GiB.

## Build and test

```sh
make build      # both binaries into ./build
make test       # go test ./...
make lint       # gofmt check plus go vet
```

Local run, two terminals:

```sh
make run-collector   # writes ./data/archive.db
make run-web         # serves http://localhost:8080
```

## Configuration

Collector:

| Variable | Default | Meaning |
| --- | --- | --- |
| `APRS_ARCHIVE_DB` | `/var/lib/aprs-archive/archive.db` | SQLite file |
| `APRS_IS_CALLSIGN` | `EA7KLK-15` | APRS-IS login |
| `APRS_IS_PASSCODE` | derived from base callsign | Override for a foreign callsign |
| `APRS_IS_SERVER` / `APRS_IS_PORT` | `rotate.aprs2.net` / `14580` | APRS-IS hub |
| `APRS_IS_FILTER` | `t/m` | Message traffic only |
| `APRS_ARCHIVE_RETENTION_DAYS` | `365` | Retention |
| `APRS_ARCHIVE_SWEEP_HOURS` | `24` | Retention sweep interval |
| `APRS_ARCHIVE_FLUSH_SECONDS` | `2` | Commit interval |
| `APRS_ARCHIVE_HEALTH_ADDR` | `:8081` | `/livez`, `/readyz`, `/status` |

Web:

| Variable | Default | Meaning |
| --- | --- | --- |
| `APRS_ARCHIVE_DB` | `/var/lib/aprs-archive/archive.db` | SQLite file (read-only) |
| `APRS_ARCHIVE_ADDR` | `:8080` | Listen address |
| `APRS_ARCHIVE_LOCATION` | `https://aprs.ea7klk.es` | Shown in the header |
| `APRS_ARCHIVE_RETENTION_DAYS` | `365` | Shown to visitors; caps report ranges |
| `APRS_ARCHIVE_COLLECTOR_STALE_MINUTES` | `15` | When the header says "idle" |
| `APRS_ARCHIVE_REPORT_MAX_DAYS` | `92` | Longest window one report may scan (three months) |
| `APRS_ARCHIVE_ROLE` | `public` | `admin` gives the server a write handle and the admin routes |
| `APRS_ARCHIVE_ADMIN_PASSWORD` | — | Admin page password; empty disables the page |

## Callsign validation

The collector stores a message only if **both** the sender and the recipient
look like real amateur stations. A callsign qualifies when its base contains a
digit and begins with a prefix from the world-wide prefix list published at
<https://www.country-files.com/cty/cty.csv>, refreshed daily. That rejects
`WXBOT`, `ANSRVR`, `MYANET` and the rest of the machine traffic, and accepts
`EA7KLK-9`, `IZ6RND` and `KH6ABC`.

Prefix validation cannot tell a station from a robot that named itself like one,
so the operator maintains an **exception list** on the admin page. It is checked
before the prefix list and wins in both directions. `APRSPH` is seeded as a
standing exception.

## Admin

Administration is a **separate binary** in the same image:

| Binary | Host | Handle | Serves |
| --- | --- | --- | --- |
| `aprs-archive-web` | aprs.ea7klk.es | read-only | the archive, anonymously |
| `aprs-archive-admin` | aprs-admin.ea7klk.es | read-write | exceptions, maintenance, SQL |

They share no route table and no templates. The public binary does not link the
admin package at all, so an admin route cannot appear on the public host — that
is a property of the build, not a conditional to remember.

The password comes from Secret **`aprs-archive-admin`**, key `password`. To
change it:

```sh
kubectl -n aprs create secret generic aprs-archive-admin \
  --from-literal=password='new-password' \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl -n aprs rollout restart deploy/aprs-archive-admin
```

The session token is derived from the password, so changing it ends every
existing admin session.

## Admin: database maintenance

`/admin/database` on the admin host shows the archive's health and offers the
operations an operator actually needs:

- **Checkpoint** — fold the WAL into the database file.
- **Vacuum** — reclaim free pages (reports bytes reclaimed, or "nothing").
- **Rebuild indexes** — repair or defragment the indexes.
- **Back up now** — a consistent `VACUUM INTO` snapshot, safe while the
  collector writes; keeps the newest 7 and downloads as a file.
- **Delete before a date** — apply retention immediately. A future date is
  refused, because it would delete the archive.
- **SQL console** — full SQLite: arbitrary statements, one at a time, 500 rows
  rendered, 30-second timeout, plus a schema browser with per-object definitions
  and previews. There is no undo; take a backup first.

Every write form carries a confirmation token derived from the admin password,
so a cross-site POST cannot trigger a maintenance action and a browser replaying
a stale form is refused. All of it exists only on the admin deployment.

## Removing bot traffic already stored

Validation applies to what arrives, not to what is already in the archive. To
purge existing rows in one pass, run the collector once with the flag set:

```sh
kubectl -n aprs set env deploy/aprs-archive-collector APRS_ARCHIVE_PURGE_BOTS=1
# wait for the "purge removed N messages" log line, then:
kubectl -n aprs set env deploy/aprs-archive-collector APRS_ARCHIVE_PURGE_BOTS-
```

It uses the same predicate as collection, so the cleanup and the collector
cannot disagree.

## Deployment

Fleet reconciles `fleet/aprs-archive/` in the `ea7klk` workspace onto the
`spainip-k3s` cluster, in namespace **`aprs`**.

- `aprs-archive-collector` — 1 replica, the only writer, 5 GiB claim on
  `retained-local-path`.
- `aprs-archive-web` — 1 replica, mounts the claim read-only, served by
  IngressRoute `Host(\`aprs.ea7klk.es\`)` with the cluster's `acmeresolver`.
- The namespace itself is declared once in `spainip/infra`
  (`fleet/namespaces.yaml`) with `helm.sh/resource-policy: keep`.

The image is pinned by digest in `fleet/aprs-archive/resources.yaml`. To ship a
release: publish a GitHub release, let CI build and push the image, and pin the
digest it prints for both Deployments in one commit.

### Backups

The collector owns the file, and a plain `cp` of a live SQLite database plus its
WAL is not a consistent copy. Take a backup through SQLite instead:

```sh
kubectl -n aprs exec deploy/aprs-archive-collector -- sh -c \
  'sqlite3 /var/lib/aprs-archive/archive.db "VACUUM INTO /tmp/archive-backup.db"'
```

The collector's `/status` endpoint reports what it is doing; the web page
reports the same state in its header and on `/statistics`.

## Layout

```
cmd/aprs-archive-collector/   the writer: APRS-IS client, batching, retention
cmd/aprs-archive-web/         the reader: HTTP server and configuration
internal/aprs/                message-format parsing and bulletin taxonomy
internal/sanitize/            the one rule for third-party text
internal/store/               schema, batched writer, every read query
internal/web/                 handlers, templates, embedded stylesheet
fleet/aprs-archive/           the Fleet bundle (claims, workloads, route)
```

The visual language is `ham-bbs-web`'s, carried over unchanged: the same
stylesheet, the same "Packet Manual structure, Spectrum palette" — monospace,
hard rules, uppercase letter-spaced labels, ruled tables.
