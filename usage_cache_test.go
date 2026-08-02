package main

import (
	"errors"
	"net/http"
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

// The negative cache absorbs a burst of clients; this absorbs a client that
// keeps coming back every usageRefreshInterval. Without it, an account the
// endpoint chronically throttles costs one failed round trip per poll, forever,
// against the very endpoint doing the throttling.
func TestUsageCacheBacksOffConsecutiveThrottles(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	c := usageCache{now: func() time.Time { return now }}
	stub := &usageStub{err: &usageHTTPError{Status: http.StatusTooManyRequests}}
	const email = "andy@avisoma.com"
	// Past both the negative-cache window and any wait the schedule can arm, so
	// each call below is decided by the streak alone.
	pastFailTTL := func() { now = now.Add(usageCacheFailTTL + time.Second) }

	// The first throttle arms nothing: the next poll fetches normally.
	if _, err := c.GetOrFetch(email, stub.fetch); err == nil {
		t.Fatal("err = nil, want the throttle")
	}
	pastFailTTL()
	if _, err := c.GetOrFetch(email, stub.fetch); errors.Is(err, errUsageBackoffActive) {
		t.Fatal("backed off after a single throttle, want the next poll to try")
	}
	if stub.count() != 2 {
		t.Fatalf("fetches = %d, want both polls to have run", stub.count())
	}

	// The second consecutive one does. A caller arriving now is turned away
	// without an Anthropic round trip, and classifies as what it stands in for.
	pastFailTTL()
	_, err := c.GetOrFetch(email, stub.fetch)
	if !errors.Is(err, errUsageBackoffActive) {
		t.Fatalf("err = %v, want the backoff sentinel", err)
	}
	if stub.count() != 2 {
		t.Fatalf("fetches = %d, want the backed-off call to skip the endpoint", stub.count())
	}
	if expired, reason := classifyUsageErr(err); expired || reason != usageRateLimitedReason {
		t.Fatalf("classify = %v/%q, want a plain rate-limited row", expired, reason)
	}

	// Past the wait it tries again — a throttle that heals must be picked up.
	now = now.Add(usageBackoffSecond)
	if _, err := c.GetOrFetch(email, stub.fetch); errors.Is(err, errUsageBackoffActive) {
		t.Fatal("still backed off past the deadline")
	}
	if stub.count() != 3 {
		t.Fatalf("fetches = %d, want the retry past the deadline to run", stub.count())
	}

	// A success ends the streak, so the next throttle is a first one again.
	now = now.Add(usageBackoffMax)
	stub.err = nil
	stub.perPct = true
	if _, err := c.GetOrFetch(email, stub.fetch); err != nil {
		t.Fatalf("err = %v, want the recovery to land", err)
	}
	now = now.Add(usageCacheTTL + time.Second)
	stub.err = &usageHTTPError{Status: http.StatusTooManyRequests}
	stub.perPct = false
	if _, err := c.GetOrFetch(email, stub.fetch); errors.Is(err, errUsageBackoffActive) {
		t.Fatal("a success did not clear the streak")
	}
	pastFailTTL()
	if _, err := c.GetOrFetch(email, stub.fetch); errors.Is(err, errUsageBackoffActive) {
		t.Fatal("the streak resumed from where it left off, want it restarted")
	}
	if stub.count() != 6 {
		t.Fatalf("fetches = %d, want every non-backed-off call to have run", stub.count())
	}
}

// Only a failure the endpoint actually answered with counts. A timeout or an
// unreachable host says nothing about the account's budget, and thinning those
// retries would hide a host that is merely slow.
func TestUsageCacheBacksOffOnlyOnThrottles(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	c := usageCache{now: func() time.Time { return now }}
	stub := &usageStub{err: &usageHTTPError{Status: http.StatusInternalServerError}}
	const email = "andy@avisoma.com"

	for i := 0; i < 4; i++ {
		if _, err := c.GetOrFetch(email, stub.fetch); errors.Is(err, errUsageBackoffActive) {
			t.Fatalf("call %d backed off on a 5xx", i+1)
		}
		now = now.Add(usageCacheFailTTL + time.Second)
	}
	if stub.count() != 4 {
		t.Fatalf("fetches = %d, want every call to have run", stub.count())
	}
}

// A burst of concurrent callers is one attempt, not one per caller: joiners
// return before the streak is touched. Otherwise five clients arriving together
// would jump straight to the longest wait off a single 429.
func TestUsageCacheThrottleStreakCountsAttemptsNotCallers(t *testing.T) {
	const callers = 5
	now := time.Unix(1_700_000_000, 0)
	c := usageCache{now: func() time.Time { return now }}
	stub := &usageStub{
		gate: make(chan struct{}),
		seen: make(chan struct{}, callers),
		err:  &usageHTTPError{Status: http.StatusTooManyRequests},
	}
	const email = "andy@avisoma.com"

	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.GetOrFetch(email, stub.fetch)
		}()
	}
	select {
	case <-stub.seen:
	case <-time.After(5 * time.Second):
		t.Fatal("no fetch started")
	}
	close(stub.gate)
	wg.Wait()

	c.mu.Lock()
	streak := c.failures[email].streak
	c.mu.Unlock()
	if streak != 1 {
		t.Fatalf("streak = %d after %d concurrent callers, want 1 — one attempt was made", streak, callers)
	}
}

// The map must not grow for accounts nobody asks about any more (a renamed
// snapshot, a host dropped from servers.yaml).
func TestUsageCacheForgetsIdleFailureRecords(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	c := usageCache{now: func() time.Time { return now }}
	stub := &usageStub{err: &usageHTTPError{Status: http.StatusTooManyRequests}}

	c.GetOrFetch("gone@avisoma.com", stub.fetch)
	c.mu.Lock()
	held := len(c.failures)
	c.mu.Unlock()
	if held != 1 {
		t.Fatalf("failure records = %d, want the throttled account remembered", held)
	}
	// Long enough that its deadline is ancient, then any later insert sweeps it.
	now = now.Add(usageBackoffCeiling + usageBackoffForget + time.Minute)
	c.GetOrFetch("other@avisoma.com", stub.fetch)
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.failures["gone@avisoma.com"]; ok {
		t.Fatalf("failures = %#v, want the idle record swept", c.failures)
	}
}
