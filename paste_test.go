package main

import (
	"context"
	"testing"
	"time"
)

func TestValidPaneID(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"%0", true},
		{"%12", true},
		{"%123456", true},
		{"", false},
		{"%", false},    // no digits
		{"12", false},   // no leading %
		{"%1a", false},  // non-digit
		{"%-1", false},  // sign
		{"% 1", false},  // space
		{"%12 ", false}, // trailing space
		{"$(rm -rf)", false},
		{"%12;reboot", false},
	}
	for _, tt := range tests {
		if got := validPaneID(tt.in); got != tt.want {
			t.Errorf("validPaneID(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

// TestPasteBrokerZeroWaiterPassthrough: with no waiter connected, a request
// returns passthrough and enqueues nothing.
func TestPasteBrokerZeroWaiterPassthrough(t *testing.T) {
	b := newPasteBroker()
	if got := b.request("%1"); got != pasteActionPassthrough {
		t.Fatalf("request with no waiter = %v, want passthrough", got)
	}
	b.mu.Lock()
	n := len(b.queue)
	b.mu.Unlock()
	if n != 0 {
		t.Fatalf("passthrough should enqueue nothing, got %d queued", n)
	}
}

// TestPasteBrokerQueueOverflowDropOldest: with a waiter present, requests queue
// FIFO and overflow past pasteQueueCap drops the oldest.
func TestPasteBrokerQueueOverflowDropOldest(t *testing.T) {
	b := newPasteBroker()
	// Register a waiter directly (no HTTP) so request() enqueues.
	b.waiters[&pasteWaiter{wake: make(chan struct{}, 1)}] = struct{}{}

	total := pasteQueueCap + 2
	for i := 1; i <= total; i++ {
		if got := b.request(paneName(i)); got != pasteActionQueued {
			t.Fatalf("request %d = %v, want queued", i, got)
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.queue) != pasteQueueCap {
		t.Fatalf("queue length = %d, want cap %d", len(b.queue), pasteQueueCap)
	}
	// Oldest (total-cap) entries dropped; head is the first survivor.
	wantHead := paneName(total - pasteQueueCap + 1)
	if b.queue[0].paneID != wantHead {
		t.Fatalf("queue head = %s, want %s", b.queue[0].paneID, wantHead)
	}
	wantTail := paneName(total)
	if b.queue[len(b.queue)-1].paneID != wantTail {
		t.Fatalf("queue tail = %s, want %s", b.queue[len(b.queue)-1].paneID, wantTail)
	}
}

// TestPasteBrokerClaimSkipsExpired: claimLocked drops requests older than the
// TTL and returns the oldest fresh one.
func TestPasteBrokerClaimSkipsExpired(t *testing.T) {
	base := time.Unix(1_000_000, 0)
	now := base.Add(4 * time.Second)
	b := newPasteBroker()
	b.ttl = 3 * time.Second
	b.now = func() time.Time { return now }
	b.queue = []*pasteRequest{
		{paneID: "%1", ts: base},                              // age 4s: expired
		{paneID: "%2", ts: base.Add(2 * time.Second)},         // age 2s: fresh
		{paneID: "%3", ts: base.Add(3500 * time.Millisecond)}, // age 0.5s: fresh
	}
	b.mu.Lock()
	claimed := b.claimLocked("")
	remaining := append([]*pasteRequest(nil), b.queue...)
	b.mu.Unlock()

	if claimed == nil || claimed.paneID != "%2" {
		t.Fatalf("claim = %+v, want %%2 (oldest fresh)", claimed)
	}
	if len(remaining) != 1 || remaining[0].paneID != "%3" {
		t.Fatalf("remaining queue = %+v, want just %%3 (expired dropped, claimed removed)", remaining)
	}
}

// TestPasteBrokerSessionScopedClaim: a waiter only claims requests whose pane
// resolves to its own tmux session; non-matching requests stay queued.
func TestPasteBrokerSessionScopedClaim(t *testing.T) {
	b := newPasteBroker()
	b.resolve = func(pane string) string {
		return map[string]string{"%1": "foo", "%2": "bar"}[pane]
	}
	b.queue = []*pasteRequest{
		{paneID: "%1", ts: b.timeNow()},
		{paneID: "%2", ts: b.timeNow()},
	}
	b.mu.Lock()
	bar := b.claimLocked("bar")
	afterBar := append([]*pasteRequest(nil), b.queue...)
	b.mu.Unlock()
	if bar == nil || bar.paneID != "%2" {
		t.Fatalf("claim(bar) = %+v, want %%2", bar)
	}
	if len(afterBar) != 1 || afterBar[0].paneID != "%1" {
		t.Fatalf("after claim(bar) queue = %+v, want %%1 still queued", afterBar)
	}
	b.mu.Lock()
	foo := b.claimLocked("foo")
	b.mu.Unlock()
	if foo == nil || foo.paneID != "%1" {
		t.Fatalf("claim(foo) = %+v, want %%1", foo)
	}
}

// TestPasteBrokerStaleUploadClaimGuard: a /paste upload only proceeds when a
// fresh claim from /paste-wait exists for the pane, and the claim is single-use.
func TestPasteBrokerStaleUploadClaimGuard(t *testing.T) {
	base := time.Unix(2_000_000, 0)
	now := base
	b := newPasteBroker()
	b.now = func() time.Time { return now }

	// Unknown pane: no claim.
	if b.consumeClaim("%9") {
		t.Fatal("consumeClaim on unknown pane = true, want false")
	}

	b.mu.Lock()
	b.recordClaimLocked("%5")
	b.mu.Unlock()

	// Fresh claim consumes once.
	if !b.consumeClaim("%5") {
		t.Fatal("first consumeClaim(%5) = false, want true")
	}
	// Single-use: second consume fails.
	if b.consumeClaim("%5") {
		t.Fatal("second consumeClaim(%5) = true, want false (already consumed)")
	}

	// Stale claim (older than TTL) is rejected.
	b.mu.Lock()
	b.recordClaimLocked("%6")
	b.mu.Unlock()
	now = base.Add(pasteClaimTTL + time.Second)
	if b.consumeClaim("%6") {
		t.Fatal("consumeClaim on stale claim = true, want false")
	}
}

// TestPasteBrokerWakeAllDelivery: a request wakes every waiter; the one whose
// session matches claims it while the rest keep waiting (and time out here).
func TestPasteBrokerWakeAllDelivery(t *testing.T) {
	b := newPasteBroker()
	b.resolve = func(pane string) string {
		if pane == "%1" {
			return "S1"
		}
		return ""
	}
	got1 := make(chan *pasteRequest, 1)
	got2 := make(chan *pasteRequest, 1)
	go func() { got1 <- b.wait(context.Background(), 500*time.Millisecond, "S1") }()
	go func() { got2 <- b.wait(context.Background(), 500*time.Millisecond, "S2") }()
	waitForCond(t, func() bool { return b.activeWaiters() == 2 }, time.Second)

	if got := b.request("%1"); got != pasteActionQueued {
		t.Fatalf("request with waiters = %v, want queued", got)
	}
	select {
	case req := <-got1:
		if req == nil || req.paneID != "%1" {
			t.Fatalf("S1 waiter got %+v, want %%1", req)
		}
	case <-time.After(time.Second):
		t.Fatal("matching (S1) waiter was not delivered the request")
	}
	select {
	case req := <-got2:
		if req != nil {
			t.Fatalf("S2 waiter got %+v, want nil (no matching session)", req)
		}
	case <-time.After(time.Second):
		t.Fatal("non-matching (S2) waiter did not time out")
	}
}

// TestPasteBrokerWaiterWakeRecordsClaim: a woken waiter returns the request and
// records a claim authorizing the follow-up upload for that pane.
func TestPasteBrokerWaiterWakeRecordsClaim(t *testing.T) {
	b := newPasteBroker()
	done := make(chan *pasteRequest, 1)
	go func() {
		done <- b.wait(context.Background(), 2*time.Second, "") // accept any
	}()
	waitForCond(t, func() bool { return b.activeWaiters() == 1 }, time.Second)

	if got := b.request("%7"); got != pasteActionQueued {
		t.Fatalf("request with blocked waiter = %v, want queued", got)
	}
	select {
	case req := <-done:
		if req == nil || req.paneID != "%7" {
			t.Fatalf("woken waiter got %+v, want %%7", req)
		}
	case <-time.After(time.Second):
		t.Fatal("waiter was not woken")
	}
	if !b.consumeClaim("%7") {
		t.Fatal("a successful claim should authorize the /paste upload for %7")
	}
	if n := b.activeWaiters(); n != 0 {
		t.Fatalf("waiter count = %d after wake, want 0", n)
	}
}

// TestPasteBrokerWaitTimeout: wait() returns nil when nothing arrives.
func TestPasteBrokerWaitTimeout(t *testing.T) {
	b := newPasteBroker()
	if req := b.wait(context.Background(), 10*time.Millisecond, ""); req != nil {
		t.Fatalf("wait timeout = %+v, want nil", req)
	}
	if n := b.activeWaiters(); n != 0 {
		t.Fatalf("waiter count = %d after timeout, want 0", n)
	}
}

// TestPasteBrokerWaitContextCancel: wait() returns nil when the context is
// cancelled (e.g. the attach ended and the relay goroutine was stopped).
func TestPasteBrokerWaitContextCancel(t *testing.T) {
	b := newPasteBroker()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan *pasteRequest, 1)
	go func() {
		done <- b.wait(ctx, 5*time.Second, "")
	}()
	waitForCond(t, func() bool { return b.activeWaiters() == 1 }, time.Second)
	cancel()
	select {
	case req := <-done:
		if req != nil {
			t.Fatalf("cancelled wait = %+v, want nil", req)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled wait did not return")
	}
}

// paneName builds a tmux-style pane id "%<n>" for tests.
func paneName(n int) string {
	return "%" + itoaForTest(n)
}

func itoaForTest(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// waitForCond polls cond until it's true or the deadline passes.
func waitForCond(t *testing.T, cond func() bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}
