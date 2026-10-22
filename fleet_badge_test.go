package main

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestFleetBadgeText(t *testing.T) {
	if got := fleetBadgeText(fleetCardCounts{1, 2, 3}, false); got != "" {
		t.Fatalf("not ok: got %q, want empty", got)
	}
	if got := fleetBadgeText(fleetCardCounts{0, 0, 3}, true); got != "" {
		t.Fatalf("only sent: got %q, want empty", got)
	}
	got := fleetBadgeText(fleetCardCounts{1, 2, 0}, true)
	if s := stripANSI(got); s != "FLEET 1 approve · 2 decide" {
		t.Fatalf("plain text = %q", s)
	}
	for _, want := range []string{colorize("1;31", "1 approve"), colorize("1;33", "2 decide")} {
		if !strings.Contains(got, want) {
			t.Fatalf("badge %q missing %q", got, want)
		}
	}
	got = fleetBadgeText(fleetCardCounts{0, 2, 0}, true)
	if !strings.Contains(got, dim("0 approve")) {
		t.Fatalf("zero approve not dim: %q", got)
	}
}

func fleetBadgeAliveView(cards ...fleetCardView) fleetStateView {
	return fleetStateView{Fleet: fleetSession{Alive: true}, Cards: cards}
}

// waitFleetBadge polls Snapshot for up to 1 s until want holds.
func waitFleetBadge(t *testing.T, h *FleetBadgeHub, want func(fleetCardCounts, bool) bool) (fleetCardCounts, bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		c, ok := h.Snapshot()
		if want(c, ok) || time.Now().After(deadline) {
			return c, ok
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestFleetBadgeHubSnapshot(t *testing.T) {
	var view atomic.Value
	var errNow atomic.Bool
	var calls atomic.Int32
	view.Store(fleetBadgeAliveView(fleetCardView{ID: "a", Kind: "explicit-yes"}))
	h := NewFleetBadgeHub(func() (fleetStateView, error) {
		calls.Add(1)
		if errNow.Load() {
			return fleetStateView{}, errors.New("down")
		}
		return view.Load().(fleetStateView), nil
	}, time.Hour)
	defer h.Shutdown()

	c, ok := waitFleetBadge(t, h, func(_ fleetCardCounts, ok bool) bool { return ok })
	if !ok || c != (fleetCardCounts{Approve: 1}) {
		t.Fatalf("alive: got %+v ok=%v", c, ok)
	}

	errNow.Store(true)
	h.Refresh()
	if _, ok := waitFleetBadge(t, h, func(_ fleetCardCounts, ok bool) bool { return !ok }); ok {
		t.Fatal("fetch error: ok stayed true")
	}

	errNow.Store(false)
	h.Refresh()
	if _, ok := waitFleetBadge(t, h, func(_ fleetCardCounts, ok bool) bool { return ok }); !ok {
		t.Fatal("recovery: ok stayed false")
	}

	view.Store(fleetStateView{Fleet: fleetSession{Alive: false}, Cards: []fleetCardView{{ID: "a", Kind: "explicit-yes"}}})
	h.Refresh()
	if _, ok := waitFleetBadge(t, h, func(_ fleetCardCounts, ok bool) bool { return !ok }); ok {
		t.Fatal("not alive: ok stayed true")
	}
}

func TestFleetBadgeHubPausedDoesNotFetch(t *testing.T) {
	var calls atomic.Int32
	h := NewFleetBadgeHub(func() (fleetStateView, error) {
		calls.Add(1)
		return fleetBadgeAliveView(), nil
	}, time.Hour)
	defer h.Shutdown()
	deadline := time.Now().Add(time.Second)
	for calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if calls.Load() != 1 {
		t.Fatalf("initial fetch count = %d, want 1", calls.Load())
	}
	h.Pause()
	h.Refresh()
	time.Sleep(100 * time.Millisecond)
	if n := calls.Load(); n != 1 {
		t.Fatalf("paused Refresh fetched: %d calls", n)
	}
	h.Resume()
	deadline = time.Now().Add(time.Second)
	for calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("Resume did not fetch: %d calls", n)
	}
}
