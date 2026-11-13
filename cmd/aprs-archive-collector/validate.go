package main

import (
	"context"
	"database/sql"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"aprs-archive/internal/prefixes"
	"aprs-archive/internal/store"
)

// validator decides whether a message belongs to the archive.
//
// APRS-IS carries a large amount of machine traffic that is not operator
// conversation: weather gateways, message relays, registration bots, net
// announcers. They address themselves with names rather than callsigns, and a
// message to or from one of them is not what this archive is for.
//
// Validation is bidirectional. A message from a bot to a real station is bot
// output; a message from a real station to a bot is a command. Neither is
// conversation, so both are dropped.
//
// Two sources decide, in order:
//
//  1. The operator's exceptions, which always win. APRSPH is the standing
//     example: it is a bot, but the operator wants its traffic.
//  2. The prefix list, which says whether a callsign could belong to a real
//     amateur station at all.
//
// The prefix list is read from the archive rather than held only in this
// process. That is what lets a new list be adopted without a restart: a restart
// drops the APRS-IS session, and every packet in the gap is never heard, so the
// collector must never need one to pick up a list somebody published.
type validator struct {
	fetcher *prefixes.Fetcher
	db      *sql.DB

	// mu guards exceptions, set and generation. It is taken once per message,
	// so the list is a prebuilt Set rather than something parsed on demand.
	mu         sync.RWMutex
	exceptions map[string]bool
	set        *prefixes.Set
	generation int64

	// Counters, reported by the status endpoint.
	droppedNoList atomic.Int64
	droppedBot    atomic.Int64
	reloads       atomic.Int64
}

func newValidator(ctx context.Context, db *sql.DB, f *prefixes.Fetcher) *validator {
	v := &validator{fetcher: f, db: db, exceptions: map[string]bool{}}
	if err := v.reloadExceptions(ctx); err != nil {
		log.Printf("could not load callsign exceptions: %v", err)
	}
	v.reloadPrefixes(ctx)
	return v
}

func (v *validator) reloadExceptions(ctx context.Context) error {
	m, err := store.LoadExceptions(ctx, v.db)
	if err != nil {
		return err
	}
	v.mu.Lock()
	v.exceptions = m
	v.mu.Unlock()
	return nil
}

// reloadPrefixes reads the cached dataset and adopts it when it changed.
//
// Adoption is all-or-nothing: the body is parsed into a new Set, and only a set
// that parses is swapped in. A corrupt or truncated cache leaves the current
// list in place, so it can never turn validation off.
func (v *validator) reloadPrefixes(ctx context.Context) {
	snap, err := store.ReadPrefixSnapshot(ctx, v.db)
	if err != nil {
		if err != store.ErrNoPrefixSnapshot {
			log.Printf("could not read the prefix snapshot: %v", err)
		}
		return
	}
	v.mu.RLock()
	current := v.generation
	v.mu.RUnlock()
	if current == snap.Generation {
		return
	}
	set, err := prefixes.ParseCSV(snap.Body)
	if err != nil {
		// Keep the list already in use. A bad snapshot is a reason to keep
		// validating against the last good one, not to stop validating.
		log.Printf("cached prefix snapshot %d is unusable, keeping the loaded list: %v", snap.Generation, err)
		return
	}
	set.FetchedAt = snap.FetchedAt
	set.Source = snap.Source
	v.mu.Lock()
	v.set = set
	v.generation = snap.Generation
	v.mu.Unlock()
	v.reloads.Add(1)
	log.Printf("prefix list active: generation %d, %d prefixes, fetched %s",
		snap.Generation, set.Count, snap.FetchedAt.Format(time.RFC3339))
}

// Watch is the minute tick. It picks up a new prefix list, a new exception list,
// or both, without touching the APRS-IS session.
func (v *validator) Watch(ctx context.Context) {
	if err := v.reloadExceptions(ctx); err != nil {
		log.Printf("could not load callsign exceptions: %v", err)
	}
	v.reloadPrefixes(ctx)
}

// Keep reports whether a message between these two stations should be stored.
func (v *validator) Keep(from, to string) bool {
	return v.allow(from) && v.allow(to)
}

// allow decides one side of a message.
//
// Fail closed: with no prefix list at all, nothing is stored. That is a
// deliberate reversal of the earlier behaviour, which accepted everything when
// the list was missing — a network failure should not quietly re-admit the bot
// traffic this validation exists to remove. The archive logs it and reports it
// on the status endpoint rather than filling with traffic nobody wanted.
func (v *validator) allow(callsign string) bool {
	v.mu.RLock()
	override, has := v.exceptions[upper(callsign)]
	set := v.set
	v.mu.RUnlock()

	if has {
		return override
	}
	if set == nil {
		v.droppedNoList.Add(1)
		return false
	}
	if set.Valid(callsign) {
		return true
	}
	v.droppedBot.Add(1)
	return false
}

func upper(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'a' && b[i] <= 'z' {
			b[i] -= 'a' - 'A'
		}
	}
	return string(b)
}

// prefixState reports what the validator is using, for the status endpoint.
func (v *validator) prefixState() (generation int64, count int, fetched time.Time) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.set == nil {
		return v.generation, 0, time.Time{}
	}
	return v.generation, v.set.Count, v.set.FetchedAt
}

// seedDefaultExceptions records the exceptions the archive starts with.
//
// APRSPH is a bot that a great many operators talk to deliberately, and its
// replies are the answer to their question. The operator can remove this entry
// from the admin page like any other; this only ensures it exists on a fresh
// archive.
func seedDefaultExceptions(ctx context.Context, db *sql.DB) error {
	existing, err := store.ListExceptions(ctx, db)
	if err != nil {
		return err
	}
	for _, e := range existing {
		if e.Callsign == "APRSPH" {
			return nil
		}
	}
	return store.SetException(ctx, db, "APRSPH", true, "standing exception: APRS bot whose replies operators ask for")
}
