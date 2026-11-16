package main

import (
	"context"
	"database/sql"
	"log"
	"sync"
	"time"

	"aprs-archive/internal/store"
	"aprs-archive/internal/telemetry"
)

// filterState holds the compiled telemetry rules.
//
// Replaced wholesale on a generation change, so a message is matched against
// either the old rule set or the new one and never a half-built one.
type filterState struct {
	mu         sync.RWMutex
	matcher    *telemetry.Matcher
	generation int64
	// dropped counts per-rule hits since start, for the status endpoint.
	dropped map[int64]int64
	total   int64
}

func newFilterState(ctx context.Context, db *sql.DB) *filterState {
	f := &filterState{dropped: map[int64]int64{}}
	f.reload(ctx, db)
	return f
}

// reload recompiles the rules when their generation changed.
func (f *filterState) reload(ctx context.Context, db *sql.DB) {
	gen, err := store.MessageFiltersGeneration(ctx, db)
	if err != nil {
		log.Printf("could not read the message-filter generation: %v", err)
		return
	}
	f.mu.RLock()
	same := f.matcher != nil && f.generation == gen
	f.mu.RUnlock()
	if same {
		return
	}
	items, err := store.EnabledMessageFilters(ctx, db)
	if err != nil {
		log.Printf("could not load message filters: %v", err)
		return
	}
	src := make([]telemetry.Source, 0, len(items))
	for _, it := range items {
		src = append(src, telemetry.Source{ID: it.ID, Name: it.Name, Pattern: it.Pattern, Kind: it.Kind})
	}
	m := telemetry.Compile(src, gen)
	if broken := m.Broken(); len(broken) > 0 {
		for _, b := range broken {
			log.Printf("message filter will not compile and is ignored: %s", b)
		}
	}
	f.mu.Lock()
	f.matcher = m
	f.generation = gen
	f.mu.Unlock()
	log.Printf("message filters active: generation %d, %d patterns", gen, m.Count())
}

// Drop reports whether a body is machine telemetry.
func (f *filterState) Drop(text string) bool {
	f.mu.RLock()
	m := f.matcher
	f.mu.RUnlock()
	rule, ok := m.Match(text)
	if !ok {
		return false
	}
	f.mu.Lock()
	f.dropped[rule.ID]++
	f.total++
	f.mu.Unlock()
	return true
}

// Refresh is the minute tick.
func (f *filterState) Refresh(ctx context.Context, db *sql.DB) { f.reload(ctx, db) }

func (f *filterState) totals() (int64, map[int64]int64, int) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	cp := make(map[int64]int64, len(f.dropped))
	for k, v := range f.dropped {
		cp[k] = v
	}
	n := 0
	if f.matcher != nil {
		n = f.matcher.Count()
	}
	return f.total, cp, n
}

// telemetryWatchLoop keeps the rules current without a restart, on the same
// minute cadence as the prefix list.
func telemetryWatchLoop(ctx context.Context, db *sql.DB, f *filterState) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			f.Refresh(ctx, db)
		}
	}
}
