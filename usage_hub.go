package main

import (
	"sync"
	"sync/atomic"
	"time"
)

// usagePoller is the shared background poller behind UsageHub (Anthropic) and
// CodexUsageHub (OpenAI Codex): the two differ only in what they fetch, cache,
// and hold, so the loop, pause/resume, kick, snapshot, and shutdown live here
// once and each provider supplies fetch + save. It refetches on a fixed cadence
// (usageRefreshInterval) into a single *T slot the render loop reads via
// Snapshot, backing off from usageRetryMin on failure (capped at the refresh
// interval) and persisting each success via save so a restart during a throttle
// still shows a stale bar. Following RemoteHub's pattern minus the wake pipe —
// the TUI repaints on its own tick, and a slightly stale percentage is fine.
// Snapshot is nil until the first success (or a warm-start seed), so the bar
// lazily appears on a later repaint; a failed refresh keeps the previous value
// visible instead of blinking the bar away.
type usagePoller[T any] struct {
	mu     sync.Mutex
	info   *T
	paused atomic.Bool
	kick   chan struct{}
	stop   chan struct{}
	fetch  func() (*T, error)
	save   func(*T)
}

// newUsagePoller starts the poller and returns immediately; the first fetch is
// kicked off asynchronously. seed is a recent disk-cached snapshot (or nil) so a
// restart while the endpoint is throttling still shows a stale bar.
//
// fetch is expected to return a non-nil *T on success (err == nil); the loop
// tolerates a nil-on-success without panicking (it just skips the save) but a
// provider should never do it.
func newUsagePoller[T any](seed *T, fetch func() (*T, error), save func(*T)) *usagePoller[T] {
	h := &usagePoller[T]{
		info:  seed,
		kick:  make(chan struct{}, 1),
		stop:  make(chan struct{}),
		fetch: fetch,
		save:  save,
	}
	go h.run()
	h.Kick()
	return h
}

func (h *usagePoller[T]) run() {
	t := time.NewTicker(usageRefreshInterval)
	defer t.Stop()
	backoff := usageRetryMin
	var retry <-chan time.Time
	for {
		select {
		case <-h.stop:
			return
		case <-t.C:
		case <-h.kick:
		case <-retry:
		}
		retry = nil
		if h.paused.Load() {
			continue
		}
		if info, err := h.fetch(); err == nil {
			h.mu.Lock()
			h.info = info
			h.mu.Unlock()
			// save dereferences info; guard against a provider that violates the
			// non-nil-on-success contract rather than panicking the goroutine.
			if info != nil {
				h.save(info)
			}
			backoff = usageRetryMin
		} else {
			retry = time.After(backoff)
			backoff *= 2
			if backoff > usageRefreshInterval {
				backoff = usageRefreshInterval
			}
		}
	}
}

// Snapshot returns the last successful fetch, or nil if none yet.
func (h *usagePoller[T]) Snapshot() *T {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.info
}

// Pause makes the poller ignore ticks and kicks — used while an external
// program owns the terminal and nothing renders.
func (h *usagePoller[T]) Pause() { h.paused.Store(true) }

// Resume re-enables polling and kicks an immediate refetch.
func (h *usagePoller[T]) Resume() {
	h.paused.Store(false)
	h.Kick()
}

// Kick requests an immediate refetch. Non-blocking; coalesces when one is
// already pending.
func (h *usagePoller[T]) Kick() {
	select {
	case h.kick <- struct{}{}:
	default:
	}
}

// Shutdown stops the background goroutine.
func (h *usagePoller[T]) Shutdown() {
	close(h.stop)
}
