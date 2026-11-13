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
type validator struct {
	fetcher *prefixes.Fetcher
	db      *sql.DB

	// exceptions are reloaded periodically rather than read per packet: this
	// runs on the read loop for every message on the network.
	mu         sync.RWMutex
	exceptions map[string]bool

	droppedBots     atomic.Int64
	droppedExemptNo atomic.Int64
}

func newValidator(ctx context.Context, db *sql.DB, f *prefixes.Fetcher) *validator {
	v := &validator{fetcher: f, db: db, exceptions: map[string]bool{}}
	v.reloadExceptions(ctx)
	return v
}

func (v *validator) reloadExceptions(ctx context.Context) {
	m, err := store.LoadExceptions(ctx, v.db)
	if err != nil {
		log.Printf("could not load callsign exceptions: %v", err)
		return
	}
	v.mu.Lock()
	v.exceptions = m
	v.mu.Unlock()
}

// Keep reports whether a message between these two stations should be stored.
func (v *validator) Keep(from, to string) bool {
	if v.allow(from) && v.allow(to) {
		return true
	}
	return false
}

// allow decides one side of a message.
func (v *validator) allow(callsign string) bool {
	// An operator decision beats everything.
	v.mu.RLock()
	override, has := v.exceptions[upper(callsign)]
	v.mu.RUnlock()
	if has {
		return override
	}

	set, err := v.fetcher.Snapshot()
	if err != nil || set == nil {
		// No prefix list yet. Refusing everything would silently empty the
		// archive; accepting everything is the pre-validation behaviour, and
		// the listener only starts once a list is loaded, so this is a
		// transient state at worst.
		return true
	}
	return set.Valid(callsign)
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

// refreshLoop keeps the prefix list current and the exceptions fresh.
func (v *validator) refreshLoop(ctx context.Context, listEvery, exceptionsEvery time.Duration) {
	list := time.NewTicker(listEvery)
	defer list.Stop()
	exc := time.NewTicker(exceptionsEvery)
	defer exc.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-list.C:
			if err := v.fetcher.Refresh(ctx); err != nil {
				log.Printf("prefix list refresh failed, keeping the previous list: %v", err)
			} else if set, _ := v.fetcher.Snapshot(); set != nil {
				log.Printf("prefix list refreshed: %d prefixes", set.Count)
			}
		case <-exc.C:
			// The admin deployment writes the table directly; reloading here is
			// what makes its changes take effect within a minute.
			v.reloadExceptions(ctx)
		}
	}
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
