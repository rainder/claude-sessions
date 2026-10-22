package main

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// fleetBadgeInterval is how often the badge hub asks the local service for
// /fleet/state. The page polls faster; a header count can lag a few seconds.
const fleetBadgeInterval = 10 * time.Second

// FleetBadgeHub polls the local service's FLEET state in one background
// goroutine so the session-list header can show how many decisions wait,
// without the render loop ever touching the network. It has RemoteUsageHub's
// shape: a ticker, a kick channel, a pause flag, and no wake pipe, because
// the TUI's own 2s tick repaints a changed count soon enough.
//
// fetch (fleetLocal.State) holds a lock across a network call of up to 5s, so
// it must only ever run on the hub's goroutine. The render path reads
// Snapshot and nothing else.
type FleetBadgeHub struct {
	mu       sync.Mutex
	counts   fleetCardCounts
	ok       bool
	paused   atomic.Bool
	kick     chan struct{}
	stop     chan struct{}
	fetch    func() (fleetStateView, error)
	interval time.Duration
}

// NewFleetBadgeHub starts the poller and returns at once; the first fetch is
// kicked off asynchronously, so the badge is simply absent until it lands.
func NewFleetBadgeHub(fetch func() (fleetStateView, error), interval time.Duration) *FleetBadgeHub {
	h := &FleetBadgeHub{
		kick:     make(chan struct{}, 1),
		stop:     make(chan struct{}),
		fetch:    fetch,
		interval: interval,
	}
	go h.run()
	h.Refresh()
	return h
}

func (h *FleetBadgeHub) run() {
	t := time.NewTicker(h.interval)
	defer t.Stop()
	for {
		select {
		case <-h.stop:
			return
		case <-t.C:
		case <-h.kick:
		}
		if h.paused.Load() {
			continue
		}
		h.poll()
	}
}

// poll fetches once. ok is true only for a good answer from a live FLEET: an
// error, a service that is down or a FLEET that is not running all hide the
// badge, so a stale count never outlives the session it described.
func (h *FleetBadgeHub) poll() {
	view, err := h.fetch()
	var counts fleetCardCounts
	ok := err == nil && view.Fleet.Alive
	if ok {
		counts = countFleetCards(view)
	}
	h.mu.Lock()
	h.counts, h.ok = counts, ok
	h.mu.Unlock()
}

// Snapshot returns the last counts and whether they are good to show.
func (h *FleetBadgeHub) Snapshot() (fleetCardCounts, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.counts, h.ok
}

// Pause makes the poller ignore ticks and kicks while another program owns
// the terminal.
func (h *FleetBadgeHub) Pause() { h.paused.Store(true) }

// Resume re-enables polling and kicks a fetch so the first repaint is fresh.
func (h *FleetBadgeHub) Resume() {
	h.paused.Store(false)
	h.Refresh()
}

// Refresh requests an immediate fetch. Non-blocking; coalesces with a kick
// already pending.
func (h *FleetBadgeHub) Refresh() {
	select {
	case h.kick <- struct{}{}:
	default:
	}
}

// Shutdown stops the background goroutine.
func (h *FleetBadgeHub) Shutdown() { close(h.stop) }

// fleetBadgeText is the header badge: "FLEET 1 approve · 2 decide", with a
// count in color when it is non-zero and dim when it is zero. It is empty when
// FLEET is not reachable, and when nothing waits for Andy (Sent cards are
// already answered). Every word is a fixed string and every number is a count,
// so no service text reaches the terminal.
func fleetBadgeText(c fleetCardCounts, ok bool) string {
	if !ok || c.Approve+c.Decide == 0 {
		return ""
	}
	seg := func(sgr string, n int, word string) string {
		s := fmt.Sprintf("%d %s", n, word)
		if n == 0 {
			return dim(s)
		}
		return colorize(sgr, s)
	}
	return "FLEET " + seg("1;31", c.Approve, "approve") + " · " + seg("1;33", c.Decide, "decide")
}
