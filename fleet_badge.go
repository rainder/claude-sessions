package main

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// fleetBadgeInterval is how often the badge hub asks for /fleet/state (the
// local service, and the remote hosts when FLEET is not live here). The page polls faster; a header count can lag a few seconds.
const fleetBadgeInterval = 10 * time.Second

// FleetBadgeHub polls FLEET's state in one background
// goroutine so the session-list header can show how many decisions wait,
// without the render loop ever touching the network. It has RemoteUsageHub's
// shape: a ticker, a kick channel, a pause flag, and no wake pipe, because
// the TUI's own 2s tick repaints a changed count soon enough.
//
// fetch (fleetLocal.State) holds a lock across the local call and, when FLEET
// is not live here, the parallel remote fan-out: up to 10s. So it must only
// ever run on the hub's goroutine. The render path reads
// Snapshot and nothing else.
type FleetBadgeHub struct {
	mu       sync.Mutex
	snap     fleetBadgeSnap
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

// fleetBadgeSnap is what the badge shows. OK is true only for a good answer
// from a live FLEET; Host names the remote host it runs on ("" local);
// Ambiguous is true when more than one host runs a live FLEET.
type fleetBadgeSnap struct {
	Counts    fleetCardCounts
	OK        bool
	Host      string
	Ambiguous bool
}

// fleetBadgeSnapFor maps one fetch to the badge. An error, a service that is
// down or a FLEET that is not running all hide the badge, so a stale count
// never outlives the session it described.
func fleetBadgeSnapFor(view fleetStateView, err error) fleetBadgeSnap {
	switch {
	case err != nil:
		return fleetBadgeSnap{}
	case len(view.Ambiguous) > 0:
		return fleetBadgeSnap{Ambiguous: true}
	case view.Fleet.Alive:
		return fleetBadgeSnap{Counts: countFleetCards(view), OK: true, Host: view.Host}
	}
	return fleetBadgeSnap{}
}

// poll fetches once.
func (h *FleetBadgeHub) poll() {
	snap := fleetBadgeSnapFor(h.fetch())
	h.mu.Lock()
	h.snap = snap
	h.mu.Unlock()
}

// Snapshot returns the last badge state.
func (h *FleetBadgeHub) Snapshot() fleetBadgeSnap {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.snap
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
// count in color when it is non-zero and dim when it is zero, and
// "FLEET@<host>" when FLEET runs on a remote host. It is empty when FLEET is
// not reachable, and when nothing waits for Andy (Sent cards are already
// answered). Two live FLEETs show a fixed "FLEET ?". Every word is a fixed
// string, every number a count, and the host a fleetHostName, so no service
// text reaches the terminal.
func fleetBadgeText(s fleetBadgeSnap) string {
	if s.Ambiguous {
		return colorize("1;33", "FLEET ?")
	}
	c := s.Counts
	if !s.OK || c.Approve+c.Decide == 0 {
		return ""
	}
	name := "FLEET"
	if s.Host != "" {
		name += "@" + s.Host
	}
	seg := func(sgr string, n int, word string) string {
		s := fmt.Sprintf("%d %s", n, word)
		if n == 0 {
			return dim(s)
		}
		return colorize(sgr, s)
	}
	return name + " " + seg("1;31", c.Approve, "approve") + " · " + seg("1;33", c.Decide, "decide")
}
