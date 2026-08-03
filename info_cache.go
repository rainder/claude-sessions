package main

import (
	"context"
	"sort"
	"sync"
	"time"
)

// summaryCacheEntry holds one in-flight-or-completed fetch. done is closed
// once fetch returns; a caller that arrives before that selects on it,
// joining the same flight rather than starting a second one.
type summaryCacheEntry struct {
	result  PreviewResult
	err     error
	expires time.Time
	done    chan struct{}
}

// summaryCache is a small single-flight + TTL + bounded-size cache, modeled
// on usage_cache.go's GetOrFetch *shape* (single-flight + TTL), not reused
// literally — usage_cache.go's key space ("accounts this host holds a
// snapshot for") is small and explicitly documented as needing no size
// bound; this cache's key spaces (ticket ids, and (host,session,mtime,size)
// tuples) are not, so eviction-over-capacity is new logic here.
//
// The underlying fetch always runs to its own bounded completion,
// independent of any individual caller's context — ctx here only bounds how
// long THIS caller waits to join, not the flight's lifetime. This means a
// caller that cancels while it happens to be the one that started the
// fetch does not abort the underlying subprocess; the fetch keeps running,
// bounded by whatever timeout its own closure applies (e.g.
// infoDialogTimeout), to finish populating the cache for the next lookup.
// This is an intentional "narrow the race, don't guess" tradeoff, the same
// shape CLAUDE.md documents for this repo's kill/migrate preconditions —
// not a leak, since it always terminates and its result is useful.
type summaryCache struct {
	mu      sync.Mutex
	entries map[string]*summaryCacheEntry
	ttl     time.Duration
	max     int
}

func newSummaryCache(ttl time.Duration, max int) *summaryCache {
	return &summaryCache{entries: make(map[string]*summaryCacheEntry), ttl: ttl, max: max}
}

func (c *summaryCache) getOrFetch(ctx context.Context, key string, fetch func(context.Context) (PreviewResult, error)) (PreviewResult, error) {
	c.mu.Lock()
	if e, ok := c.entries[key]; ok {
		// An in-flight entry (done not yet closed) has a zero-value expires
		// — it hasn't finished, so there's nothing to compare against a TTL
		// yet — and must always be joined rather than mistaken for expired.
		inFlight := false
		select {
		case <-e.done:
		default:
			inFlight = true
		}
		if inFlight || time.Now().Before(e.expires) {
			c.mu.Unlock()
			select {
			case <-e.done:
				return e.result, e.err
			case <-ctx.Done():
				return PreviewResult{}, ctx.Err()
			}
		}
		delete(c.entries, key)
	}
	e := &summaryCacheEntry{done: make(chan struct{})}
	c.entries[key] = e
	c.prune()
	c.mu.Unlock()

	e.result, e.err = fetch(ctx)
	e.expires = time.Now().Add(c.ttl)
	close(e.done)
	return e.result, e.err
}

// prune drops expired entries, then — if still over capacity — evicts
// completed entries with the soonest expiry until back at c.max. An
// in-flight entry (done not yet closed) is never evicted: its key is the
// only thing joiners have to find it by. Caller holds c.mu.
func (c *summaryCache) prune() {
	now := time.Now()
	for k, e := range c.entries {
		select {
		case <-e.done:
			if now.After(e.expires) {
				delete(c.entries, k)
			}
		default:
		}
	}
	if len(c.entries) <= c.max {
		return
	}
	type kv struct {
		key     string
		expires time.Time
	}
	var evictable []kv
	for k, e := range c.entries {
		select {
		case <-e.done:
			evictable = append(evictable, kv{k, e.expires})
		default:
		}
	}
	sort.Slice(evictable, func(i, j int) bool { return evictable[i].expires.Before(evictable[j].expires) })
	for _, item := range evictable {
		if len(c.entries) <= c.max {
			break
		}
		delete(c.entries, item.key)
	}
}
