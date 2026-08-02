package main

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// usageStub is a counting, gateable fetch. When gate is non-nil the fetch blocks
// on it, which is what lets the concurrency tests assert on ordering instead of
// on wall-clock timing.
type usageStub struct {
	mu     sync.Mutex
	calls  int
	gate   chan struct{}
	seen   chan struct{} // one send per entry, buffered by the test
	info   *UsageInfo
	err    error
	perPct bool // when set, each call returns a distinct FiveHour.Pct (= call number)
}

func (s *usageStub) fetch() (*UsageInfo, error) {
	s.mu.Lock()
	s.calls++
	n := s.calls
	s.mu.Unlock()
	if s.seen != nil {
		s.seen <- struct{}{}
	}
	if s.gate != nil {
		<-s.gate
	}
	if s.err != nil {
		return nil, s.err
	}
	if s.perPct {
		return &UsageInfo{FiveHour: usageBucket{Pct: float64(n)}}, nil
	}
	return s.info, nil
}

func (s *usageStub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func TestUsageCacheSingleFlightsConcurrentCallers(t *testing.T) {
	const callers = 6
	stub := &usageStub{
		gate: make(chan struct{}),
		seen: make(chan struct{}, callers),
		info: &UsageInfo{FiveHour: usageBucket{Pct: 42}},
	}
	var c usageCache

	infos := make([]*UsageInfo, callers)
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			infos[i], errs[i] = c.GetOrFetch("andy@avisoma.com", stub.fetch)
		}()
	}
	// Deterministic instead of timing-based: hold the one fetch that started
	// until every caller has had the chance to pile in behind it, then release.
	select {
	case <-stub.seen:
	case <-time.After(5 * time.Second):
		t.Fatal("no fetch started")
	}
	close(stub.gate)
	wg.Wait()

	if got := stub.count(); got != 1 {
		t.Fatalf("fetches = %d, want exactly 1 for %d concurrent callers", got, callers)
	}
	for i := range infos {
		if errs[i] != nil {
			t.Fatalf("caller %d: err = %v", i, errs[i])
		}
		if infos[i] == nil || infos[i].FiveHour.Pct != 42 {
			t.Fatalf("caller %d got %#v, want the single fetch's result", i, infos[i])
		}
	}
}

func TestUsageCacheRepliesFromCacheUntilTTLExpires(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	c := usageCache{now: func() time.Time { return now }}
	stub := &usageStub{perPct: true}

	first, err := c.GetOrFetch("andy@avisoma.com", stub.fetch)
	if err != nil || first.FiveHour.Pct != 1 {
		t.Fatalf("first = %#v, err = %v", first, err)
	}
	now = now.Add(usageCacheTTL - time.Second)
	again, _ := c.GetOrFetch("andy@avisoma.com", stub.fetch)
	if stub.count() != 1 || again.FiveHour.Pct != 1 {
		t.Fatalf("a call inside the TTL refetched: calls = %d, pct = %v", stub.count(), again.FiveHour.Pct)
	}
	now = now.Add(2 * time.Second)
	fresh, _ := c.GetOrFetch("andy@avisoma.com", stub.fetch)
	if stub.count() != 2 || fresh.FiveHour.Pct != 2 {
		t.Fatalf("a call past the TTL replayed: calls = %d, pct = %v", stub.count(), fresh.FiveHour.Pct)
	}
}

func TestUsageCacheRemembersAFailureOnlyBriefly(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	c := usageCache{now: func() time.Time { return now }}
	stub := &usageStub{err: errors.New("usage endpoint: HTTP 429")}

	if _, err := c.GetOrFetch("andy@avisoma.com", stub.fetch); err == nil {
		t.Fatal("err = nil, want the fetch's failure")
	}
	// Inside the negative-cache window a burst of callers must not each re-hit a
	// throttling endpoint.
	now = now.Add(usageCacheFailTTL - time.Second)
	if _, err := c.GetOrFetch("andy@avisoma.com", stub.fetch); err == nil {
		t.Fatal("cached failure replayed as success")
	}
	if stub.count() != 1 {
		t.Fatalf("fetches = %d, want the failure to be remembered inside its TTL", stub.count())
	}
	// But a failure must not be held for the success TTL — the point is a short
	// pause, not two minutes of a blank bar.
	now = now.Add(2 * time.Second)
	stub.err = nil
	stub.perPct = true
	recovered, err := c.GetOrFetch("andy@avisoma.com", stub.fetch)
	if err != nil {
		t.Fatalf("err = %v, want the retry to run", err)
	}
	if stub.count() != 2 || recovered.FiveHour.Pct != 2 {
		t.Fatalf("calls = %d, pct = %v, want a genuine refetch past the failure TTL", stub.count(), recovered.FiveHour.Pct)
	}
}

func TestUsageCacheKeepsAccountsIndependent(t *testing.T) {
	var c usageCache
	stub := &usageStub{perPct: true}

	a, _ := c.GetOrFetch("andy@avisoma.com", stub.fetch)
	b, _ := c.GetOrFetch("andy@trecs.aero", stub.fetch)
	if stub.count() != 2 {
		t.Fatalf("fetches = %d, want one per account", stub.count())
	}
	if a.FiveHour.Pct == b.FiveHour.Pct {
		t.Fatalf("two accounts shared one result: %v", a.FiveHour.Pct)
	}
	// The key is case-insensitive, the way every other email comparison here is.
	same, _ := c.GetOrFetch("ANDY@Avisoma.COM", stub.fetch)
	if stub.count() != 2 || same.FiveHour.Pct != a.FiveHour.Pct {
		t.Fatalf("case-different email missed the cache: calls = %d", stub.count())
	}
}

func TestUsageCacheBypassesTheEmptyEmail(t *testing.T) {
	var c usageCache
	stub := &usageStub{perPct: true}

	// Two *different* accounts whose .account.json is missing both resolve to "".
	// They must not be served each other's limits, fetched with the wrong token.
	first, _ := c.GetOrFetch("", stub.fetch)
	second, _ := c.GetOrFetch("", stub.fetch)
	if stub.count() != 2 {
		t.Fatalf("fetches = %d, want the empty email to bypass the cache", stub.count())
	}
	if first.FiveHour.Pct == second.FiveHour.Pct {
		t.Fatalf("two unknown-email accounts shared a result: %v", first.FiveHour.Pct)
	}
	if c.len() != 0 {
		t.Fatalf("entries = %d, want the bypass to store nothing", c.len())
	}
}

func TestUsageCachePrunesExpiredEntriesOnInsert(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	c := usageCache{now: func() time.Time { return now }}
	stub := &usageStub{perPct: true}

	if _, err := c.GetOrFetch("gone@avisoma.com", stub.fetch); err != nil {
		t.Fatal(err)
	}
	now = now.Add(usageCacheTTL + time.Second)
	if _, err := c.GetOrFetch("andy@trecs.aero", stub.fetch); err != nil {
		t.Fatal(err)
	}
	if c.len() != 1 {
		t.Fatalf("entries = %d, want the expired account pruned by the insert", c.len())
	}
}

// A fetch that never returns must not wedge the account forever. Without a
// bound, expired/prune both treat an unfinished flight as never-expired (by
// design, so a genuinely in-flight fetch survives a concurrent prune) — so a
// hung fetch would leave every later caller for that email parked on the same
// flight's done channel for the life of the process. The fix bounds the fetch
// itself so the flight always finishes, publishing a timeout error that the
// short usageCacheFailTTL then lets a later caller retry past.
func TestUsageCacheDoesNotWedgeOnAFetchThatNeverReturns(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	c := usageCache{now: func() time.Time { return now }, fetchTimeout: 10 * time.Millisecond}
	hang := make(chan struct{}) // never closed — the fetch blocks on it forever
	t.Cleanup(func() { close(hang) })

	_, err := c.GetOrFetch("andy@avisoma.com", func() (*UsageInfo, error) {
		<-hang
		return &UsageInfo{}, nil
	})
	if err == nil {
		t.Fatal("err = nil for a fetch that never returned, want a timeout")
	}
	if c.len() != 1 {
		t.Fatalf("entries = %d, want the timed-out flight still cached as a failure", c.len())
	}

	// Inside the negative-cache window, a second caller must not start a second
	// goroutine racing the still-hung first one.
	calls := 0
	now = now.Add(usageCacheFailTTL - time.Second)
	if _, err := c.GetOrFetch("andy@avisoma.com", func() (*UsageInfo, error) {
		calls++
		return &UsageInfo{}, nil
	}); err == nil {
		t.Fatal("cached timeout replayed as success")
	}
	if calls != 0 {
		t.Fatalf("fetches = %d inside the fail TTL, want the cached timeout served instead", calls)
	}

	// Past the fail TTL, a fresh caller gets a genuine retry rather than staying
	// wedged forever.
	now = now.Add(2 * time.Second)
	info, err := c.GetOrFetch("andy@avisoma.com", func() (*UsageInfo, error) {
		return &UsageInfo{FiveHour: usageBucket{Pct: 7}}, nil
	})
	if err != nil || info == nil || info.FiveHour.Pct != 7 {
		t.Fatalf("info = %#v, err = %v, want a successful retry past the fail TTL", info, err)
	}
}

func TestUsageCacheTurnsAResultlessFetchIntoAnError(t *testing.T) {
	var c usageCache
	info, err := c.GetOrFetch("andy@avisoma.com", func() (*UsageInfo, error) { return nil, nil })
	if err == nil {
		t.Fatal("err = nil for a fetch that produced nothing — callers may deref info")
	}
	if info != nil {
		t.Fatalf("info = %#v, want nil", info)
	}
}
