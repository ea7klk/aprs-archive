package main

import (
	"context"
	"database/sql"
	"log"
	"time"

	"aprs-archive/internal/prefixes"
	"aprs-archive/internal/store"
)

// Prefix list handling.
//
// Two separate duties, and conflating them is what made the earlier design
// fragile:
//
//   - **Who writes the cache.** One process, on a slow schedule, from upstream.
//     It validates the body before writing, so a broken download never replaces
//     a working list.
//   - **Who reads it.** Every process that needs to validate, on a fast
//     schedule, with no network involved. That is what makes a new list take
//     effect everywhere without a restart.
//
// The collector does both here. The admin deployment can also write the cache,
// which is the point: publishing a list must not require the collector to
// restart, and the collector must notice within its watch interval.

// ensurePrefixCache makes sure the archive has a usable list at startup.
//
// Order of preference:
//
//  1. A cached snapshot that parses: use it and revalidate against upstream in
//     the background, so a restart costs no network time and no packets.
//  2. No usable cache: fetch now and write it. This is the only blocking
//     fetch, and it happens on a fresh archive or after a corrupt cache.
func ensurePrefixCache(ctx context.Context, db *sql.DB, fetcher *prefixes.Fetcher) error {
	if snap, err := store.ReadPrefixSnapshot(ctx, db); err == nil {
		if _, perr := prefixes.ParseCSV(snap.Body); perr == nil {
			log.Printf("prefix cache: generation %d, %d prefixes, fetched %s",
				snap.Generation, snap.PrefixCount, snap.FetchedAt.Format(time.RFC3339))
			return nil
		} else {
			log.Printf("prefix cache: generation %d does not parse (%v); fetching an authoritative copy", snap.Generation, perr)
		}
	} else if err != store.ErrNoPrefixSnapshot {
		return err
	}
	body, etag, lastMod, notModified, err := fetcher.Fetch(ctx, "", "")
	if err != nil {
		return err
	}
	_ = notModified
	if err := writePrefixSnapshot(ctx, db, body, etag, lastMod, fetcher.URL); err != nil {
		return err
	}
	log.Printf("prefix cache: seeded from %s", fetcher.URL)
	return nil
}

// writePrefixSnapshot parses, checks and stores a body.
//
// Parsing first is the whole safety property: a body that does not yield a
// plausible prefix list is never written, so a captive portal's HTML or a
// truncated download cannot replace a working cache.
func writePrefixSnapshot(ctx context.Context, db *sql.DB, body []byte, etag, lastModified, source string) error {
	set, err := prefixes.ParseCSV(body)
	if err != nil {
		return err
	}
	gen, err := store.WritePrefixSnapshot(ctx, db, store.PrefixSnapshot{
		Body:         body,
		Source:       source,
		PrefixCount:  int64(set.Count),
		ETag:         etag,
		LastModified: lastModified,
	})
	if err != nil {
		return err
	}
	log.Printf("prefix cache: wrote generation %d from %s (%d prefixes)", gen, source, set.Count)
	return nil
}

// refreshPrefixes fetches upstream and writes the cache only if it changed.
func refreshPrefixes(ctx context.Context, db *sql.DB, fetcher *prefixes.Fetcher) {
	snap, err := store.ReadPrefixSnapshot(ctx, db)
	etag, lastMod := "", ""
	if err == nil {
		etag, lastMod = snap.ETag, snap.LastModified
	}
	body, respETag, respLastMod, notModified, err := fetcher.Fetch(ctx, etag, lastMod)
	if err != nil {
		// Keep the cache exactly as it is: the collector keeps validating
		// against it and the admin page shows the failure.
		log.Printf("prefix refresh failed, keeping the cached list: %v", err)
		return
	}
	if notModified {
		if terr := store.TouchPrefixSnapshot(ctx, db, respETag, respLastMod); terr != nil {
			log.Printf("prefix refresh: could not record the check: %v", terr)
		}
		log.Printf("prefix refresh: upstream unchanged")
		return
	}
	if werr := writePrefixSnapshot(ctx, db, body, respETag, respLastMod, fetcher.URL); werr != nil {
		log.Printf("prefix refresh: new list rejected, keeping the cached one: %v", werr)
	}
}

// watchLoop is the fast tick: it adopts a new prefix list or a new exception
// list within a minute, without touching the APRS-IS session.
//
// This is the mechanism that makes "publish a list, do not restart" work. It
// reads one row from the archive; if the generation is unchanged it does
// nothing at all, so the cost of the tick is a single indexed read.
func watchLoop(ctx context.Context, v *validator) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			v.Watch(ctx)
		}
	}
}

// prefixRefreshLoop fetches the upstream list on a slow schedule and writes it
// only when it changed.
//
// It runs once shortly after startup so a long-running process picks up changes
// made upstream while it was not running, then on its interval.
func prefixRefreshLoop(ctx context.Context, db *sql.DB, fetcher *prefixes.Fetcher, every time.Duration) {
	if every <= 0 {
		every = 24 * time.Hour
	}
	// A short delay keeps startup traffic off the critical path.
	select {
	case <-time.After(2 * time.Minute):
		refreshPrefixes(ctx, db, fetcher)
	case <-ctx.Done():
		return
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			refreshPrefixes(ctx, db, fetcher)
		}
	}
}
