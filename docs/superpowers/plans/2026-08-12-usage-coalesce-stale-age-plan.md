# Usage-fetch coalescing + stale-age display Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop concurrent `claude-sessions` processes on the same host from duplicating Anthropic usage-endpoint fetches, add a hard `+1min` floor on every backoff-gated fetch, and show carried-forward "stale" numbers with an actual age instead of a bare word.

**Architecture:** Three independent changes to the usage-polling subsystem (see `docs/superpowers/specs/2026-08-12-usage-coalesce-stale-age-design.md` for the full design and its `ask-codex` review history): (1) a package-level clock seam so tests can simulate elapsed time without real sleeps, (2) a blanket safety margin added to the shared `usageBackoff.due()` gate, (3) a read-before-fetch coalescing check reusing the existing `liveCarryable`/`carryable` eligibility functions, and (4) threading `FetchedAt` into the header renderer to show a compact age next to the "stale" marker.

**Tech Stack:** Go stdlib only (`time`), existing test helpers (`loginAsWithSnapshot`, `liveUsageFixture`, `writeSnapshotFixture`, `noSaveAccountCache`).

## Global Constraints

- No new dependencies. No lock files, no flock — this repo's usage-cache design is deliberately best-effort/no-lock (see account_cache.go's doc comments); this plan does not change that.
- The `+1min` backoff floor applies **unconditionally**, including to a streak-1 "free" wait (user's explicit choice — this deliberately changes today's "first 429 costs nothing, retry immediately" behavior; see the design doc's "First design + independent review" section for the rejected narrower alternative).
- Coalescing is scoped to disk-backed accounts only (a resolved claude-switch snapshot name, or a known account). The in-memory `fbXXX` fallback path for an unsnapshotted live account (`name == ""` in `newUsageFetcher`) gets the `+1min` margin (via the shared `due()`) but **not** coalescing — cross-process coalescing needs a shared disk slot that path doesn't have, and building one is out of scope (documented gap in the design doc).
- `usageCoalesceWindow = usageRefreshInterval / 2` (60s) — shorter than the poll interval so a lone process never coalesces against its own next natural tick.
- Every test fixed or added in this plan must pass with `go test ./... -run <name> -v` before moving to the next task, and the whole suite (`go test ./...`) must be green before the final commit.

---

### Task 1: Clock seam for `newUsageFetcher` / `newKnownAccountsFetcher`

Both functions call `time.Now()` directly inside their returned closures, which makes the `+1min` margin and the coalescing window impossible to test without real `time.Sleep` calls. This task adds a package-level clock var (the same seam pattern this codebase already uses for `usageInfoFetch`, `profileEmailFetch`, `keychainRead`) with zero production behavior change.

**Files:**
- Modify: `usage.go:949-951` (top of `newUsageFetcher`'s returned closure), plus a new package var
- Modify: `known_accounts.go:479` (top of `newKnownAccountsFetcher`'s returned closure)
- Test: `usage_test.go` (new helper, used by later tasks' tests)

**Interfaces:**
- Produces: `var usageClockNow = time.Now` (usage.go, package `main`) — the shared clock both fetcher closures call instead of `time.Now()` directly.
- Produces: `func fakeClock(t *testing.T, start time.Time) *time.Time` (usage_test.go) — installs a controllable `usageClockNow`, restored via `t.Cleanup`. Returns a pointer whose value IS what `usageClockNow()` returns; advance it with `*clock = clock.Add(delta)`.

- [ ] **Step 1: Add the package var**

In usage.go, add near the top of the `usageBackoff`/`usageBackoffUntil` block (right before `type usageBackoff struct {` at line 693):

```go
// usageClockNow is newUsageFetcher's and newKnownAccountsFetcher's shared
// clock — a package var, not a parameter, so tests can advance it without
// changing either function's signature (the same reason usageInfoFetch and
// keychainRead are package vars: TestMain never needs to know it exists).
// Production code never touches it; it is always time.Now.
var usageClockNow = time.Now
```

- [ ] **Step 2: Wire it into `newUsageFetcher`**

In usage.go, inside `newUsageFetcher`'s returned closure (currently line 950):

```go
		now := time.Now()
```

Change to:

```go
		now := usageClockNow()
```

- [ ] **Step 3: Wire it into `newKnownAccountsFetcher`**

In known_accounts.go, inside `newKnownAccountsFetcher`'s returned closure (currently line 479):

```go
		now := time.Now()
```

Change to:

```go
		now := usageClockNow()
```

- [ ] **Step 4: Add the test helper**

In usage_test.go, add near `loginAsWithSnapshot` (after line 361):

```go
// fakeClock points usageClockNow — shared by newUsageFetcher and
// newKnownAccountsFetcher — at a controllable instant, so a test can
// simulate elapsed wall-clock time (past the backoff safety margin, or past
// the coalesce window) without a real sleep. The returned pointer IS the
// fetcher's "now" for every call until advanced with clock.Add; t.Cleanup
// restores the real clock so later tests in the same run are unaffected.
func fakeClock(t *testing.T, start time.Time) *time.Time {
	t.Helper()
	now := start
	orig := usageClockNow
	usageClockNow = func() time.Time { return now }
	t.Cleanup(func() { usageClockNow = orig })
	return &now
}
```

- [ ] **Step 5: Prove the seam works**

Add this test right after `fakeClock`:

```go
func TestFakeClockControlsUsageClockNow(t *testing.T) {
	start := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	clock := fakeClock(t, start)
	if got := usageClockNow(); !got.Equal(start) {
		t.Fatalf("usageClockNow() = %v, want %v", got, start)
	}
	*clock = clock.Add(90 * time.Second)
	if got := usageClockNow(); !got.Equal(start.Add(90 * time.Second)) {
		t.Fatalf("usageClockNow() after advance = %v, want %v", got, start.Add(90*time.Second))
	}
}
```

- [ ] **Step 6: Run it**

Run: `go test . -run TestFakeClockControlsUsageClockNow -v`
Expected: PASS

- [ ] **Step 7: Run the full existing suite to confirm zero behavior change**

Run: `go test ./... `
Expected: PASS, identical to before this task (the seam is a no-op in production — `usageClockNow` still equals `time.Now`).

- [ ] **Step 8: Commit**

```bash
git add usage.go known_accounts.go usage_test.go
git commit -m "test: add clock seam for usage-fetcher backoff/coalescing tests"
```

---

### Task 2: Backoff safety margin (`+1min` floor)

**Files:**
- Modify: `usage.go:648-663` (backoff constants block), `usage.go:702-703` (`due` method)
- Test: `usage_test.go` (new test near `TestUsageBackoffUntil`)

**Interfaces:**
- Consumes: nothing new (uses stdlib `time`).
- Produces: `const usageBackoffSafetyMargin = time.Minute` — consumed by `due()`, and indirectly by every fetch call site in Tasks 3 and 5 (they call `backoff.due(now)`/`fbBackoff.due(now)` unchanged; the floor is enforced inside `due` itself).

- [ ] **Step 1: Write the failing test**

Add to usage_test.go, right after `TestUsageBackoffUntil` (after line 261):

```go
func TestUsageBackoffDueSafetyMargin(t *testing.T) {
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name        string
		nextAttempt time.Time
		want        bool
	}{
		{"nextAttempt exactly now is not due — the margin must still be met", now, false},
		{"nextAttempt just under the margin is not due", now.Add(-59 * time.Second), false},
		{"nextAttempt exactly one margin in the past is due", now.Add(-time.Minute), true},
		{"nextAttempt well past the margin is due", now.Add(-time.Hour), true},
		{"a streak-1 'free' wait (nextAttempt == the arming instant) is not due before the margin elapses", now, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := usageBackoff{streak: 1, nextAttempt: c.nextAttempt}
			if got := b.due(now); got != c.want {
				t.Fatalf("due() = %v, want %v (nextAttempt %v, now %v)", got, c.want, c.nextAttempt, now)
			}
		})
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test . -run TestUsageBackoffDueSafetyMargin -v`
Expected: FAIL (`nextAttempt exactly now is not due` and the "59 seconds" case report `true`, want `false` — today's `due()` has no margin)

- [ ] **Step 3: Add the constant**

In usage.go, inside the `const (...)` block at lines 647-663 (right after `usageBackoffCeiling`'s closing, before the `)`):

```go
	// usageBackoffSafetyMargin is a hard floor added on top of nextAttempt:
	// due() never allows a real fetch earlier than nextAttempt+this, even for
	// a streak-1 "free" wait (nextAttempt == the recording instant, wait=0
	// above). Applied unconditionally, not just once a real multi-minute wait
	// is armed — insurance against two processes racing to reload the same
	// disk entry (see account_cache.go: writes are best-effort, no lock), and
	// a deliberate, requested tightening of the "first 429 costs nothing"
	// rule: the very next attempt after ANY throttle now waits at least this
	// long, not zero.
	usageBackoffSafetyMargin = time.Minute
```

- [ ] **Step 4: Change `due()`**

In usage.go, line 703:

```go
func (b usageBackoff) due(now time.Time) bool { return !now.Before(b.nextAttempt) }
```

Change to:

```go
// due reports whether an account may be fetched again now. nextAttempt+
// usageBackoffSafetyMargin, not nextAttempt alone — see that constant's doc
// comment for why the margin applies even to a streak-1 "free" wait.
func (b usageBackoff) due(now time.Time) bool {
	return !now.Before(b.nextAttempt.Add(usageBackoffSafetyMargin))
}
```

- [ ] **Step 5: Run the new test to verify it passes**

Run: `go test . -run TestUsageBackoffDueSafetyMargin -v`
Expected: PASS

- [ ] **Step 6: Do NOT run the full suite yet** — many existing tests are expected to fail until Task 4 fixes them. Confirm the scope of failure matches expectations:

Run: `go test ./... 2>&1 | grep -c FAIL`
Expected: a nonzero count (failures in `TestUsageFetcherBacksOffRepeatedThrottles`, `TestUsageFetcherColdStartStopsFetchingOnceBackedOff`, `TestUsageFetcherPlaceholdsWhenIdentityIsUnconfirmable`, `TestUsageFetcherCarriesNumbersRegardlessOfAge`, `TestUsageFetcherPersistsBackoffTransitions`, `TestUsageFetcherBacksOffEvenWithNoMatchingSnapshot`, `TestUsageFetcherFallbackSurvivesAnUnrelatedDiskPass`, `TestUsageFetcherUnconfirmedIdentityWaitSurvivesALaterResolvedAccount`, `TestKnownAccountsFetcherRetriesAFailedAccountFirst`, `TestKnownAccountsFetcherPersistsBackoffAfterEachPass`, `TestKnownAccountsFetcherBacksOffRepeatedThrottles`, `TestKnownAccountsFetcherResolvesActiveNameWhileBackedOff` — this is expected; Task 4 fixes these).

- [ ] **Step 7: Commit**

```bash
git add usage.go usage_test.go
git commit -m "feat: add a hard +1min floor to usageBackoff.due()"
```

---

### Task 3: Live-account read-before-fetch coalescing

**Files:**
- Modify: `usage.go` (new const near `usageRefreshInterval`; new branch inside `newUsageFetcher`'s disk-path, lines 1012-1053)
- Test: `usage_test.go` (new tests near `TestUsageFetcherReservesAPersistedEntry`)

**Interfaces:**
- Consumes: `usageClockNow` (Task 1), `usageCoalesceWindow` (this task), `liveCarryable` (existing, usage.go:814-817).
- Produces: `const usageCoalesceWindow = usageRefreshInterval / 2` — also consumed by Task 5 (known_accounts.go).

- [ ] **Step 1: Write the failing tests**

Add to usage_test.go, right after `TestUsageFetcherReservesAPersistedEntry` (after line 608):

```go
// The whole point: a fresh disk entry (written moments ago, by this process
// or another) means a pass that would otherwise fetch skips the network
// call entirely and re-serves those numbers as fresh, not stale.
func TestUsageFetcherCoalescesARecentReading(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	loginAsWithSnapshot(t, "trecs", "andy@trecs.aero")
	clock := fakeClock(t, time.Now())
	good := liveUsageFixture(37)
	saveAccountCache("trecs", accountCacheEntry{Account: good.Account, Info: good.Info, FetchedAt: good.FetchedAt})
	calls := 0
	fetcher := newUsageFetcher(func() (*AccountUsage, error) {
		calls++
		return nil, &usageHTTPError{Status: 429}
	}, noSaveAccountCache)

	u, err := fetcher()
	if calls != 0 {
		t.Fatalf("fetches = %d, want a fresh disk entry to be coalesced instead of fetched", calls)
	}
	if err != nil {
		t.Fatalf("coalesced pass = %v, want a success", err)
	}
	if u == nil || u.Stale {
		t.Fatalf("coalesced result = %#v, want fresh (non-stale) numbers", u)
	}
	if u.Account != good.Account || !reflect.DeepEqual(u.Info, good.Info) {
		t.Fatalf("coalesced numbers = %#v, want %#v", u, good)
	}
	if !u.FetchedAt.Equal(good.FetchedAt) {
		t.Errorf("coalesced pass restamped FetchedAt (%v, want %v)", u.FetchedAt, good.FetchedAt)
	}
	_ = clock
}

// Past the coalesce window, a disk entry is old enough that this pass must
// fetch for real — coalescing must not suppress ordinary polling forever.
func TestUsageFetcherFetchesPastTheCoalesceWindow(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	loginAsWithSnapshot(t, "trecs", "andy@trecs.aero")
	good := liveUsageFixture(37)
	saveAccountCache("trecs", accountCacheEntry{Account: good.Account, Info: good.Info, FetchedAt: good.FetchedAt})
	clock := fakeClock(t, time.Now())
	*clock = clock.Add(usageCoalesceWindow + time.Second)
	fresh := liveUsageFixture(55)
	calls := 0
	fetcher := newUsageFetcher(func() (*AccountUsage, error) {
		calls++
		return fresh, nil
	}, noSaveAccountCache)

	u, err := fetcher()
	if calls != 1 {
		t.Fatalf("fetches = %d, want an entry past the coalesce window to trigger a real fetch", calls)
	}
	if err != nil || u != fresh {
		t.Fatalf("pass = (%#v, %v), want the fresh fetch's own numbers", u, err)
	}
}

// A lone process must never coalesce against its own prior fetch at its next
// natural tick — usageCoalesceWindow is half of usageRefreshInterval
// specifically so this can't happen. Regression pin for the self-throttle
// bug an independent review caught in the first version of this design.
func TestUsageFetcherDoesNotSelfCoalesceAtItsOwnNextTick(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	loginAsWithSnapshot(t, "trecs", "andy@trecs.aero")
	clock := fakeClock(t, time.Now())
	first := liveUsageFixture(21)
	calls := 0
	fetcher := newUsageFetcher(func() (*AccountUsage, error) {
		calls++
		if calls == 1 {
			return first, nil
		}
		return liveUsageFixture(99), nil
	}, saveAccountCache)

	if _, err := fetcher(); err != nil {
		t.Fatalf("pass 1 = %v, want a success", err)
	}
	if calls != 1 {
		t.Fatalf("fetches = %d, want the first pass to fetch", calls)
	}
	*clock = clock.Add(usageRefreshInterval)
	if _, err := fetcher(); err != nil {
		t.Fatalf("pass 2 (one full interval later) = %v, want a success", err)
	}
	if calls != 2 {
		t.Fatalf("fetches = %d, want a lone process's own next natural tick to fetch for real, not coalesce against itself", calls)
	}
}

// An identity mismatch must never be coalesced onto — the same rule that
// already governs the carry-forward path. Reuses liveCarryable, so this is
// really pinning that reuse rather than new logic.
func TestUsageFetcherDoesNotCoalesceAMismatchedEntry(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	loginAsWithSnapshot(t, "trecs", "andy@trecs.aero")
	saveAccountCache("trecs", accountCacheEntry{
		Account:   "someone@else.example",
		Info:      &UsageInfo{FiveHour: usageBucket{Pct: 99}},
		FetchedAt: time.Now(),
	})
	fresh := liveUsageFixture(21)
	calls := 0
	fetcher := newUsageFetcher(func() (*AccountUsage, error) {
		calls++
		return fresh, nil
	}, noSaveAccountCache)
	u, err := fetcher()
	if calls != 1 {
		t.Fatalf("fetches = %d, want a mismatched entry to be ignored, forcing a real fetch", calls)
	}
	if err != nil || u != fresh {
		t.Fatalf("pass = (%#v, %v), want the fresh fetch's own numbers", u, err)
	}
}
```

Note: `TestUsageFetcherCoalescesARecentReading` references `clock` only to keep the import/variable used intentionally at the default (no advance) — this documents that "no advance" is the coalescing case, matching the pattern the other three tests use for contrast. `reflect` is already imported in usage_test.go (used by `carriedFrom`).

- [ ] **Step 2: Run them to verify they fail**

Run: `go test . -run 'TestUsageFetcherCoalescesARecentReading|TestUsageFetcherFetchesPastTheCoalesceWindow|TestUsageFetcherDoesNotSelfCoalesceAtItsOwnNextTick|TestUsageFetcherDoesNotCoalesceAMismatchedEntry' -v`
Expected: `TestUsageFetcherCoalescesARecentReading` FAILs (calls=1, not 0 — no coalescing exists yet); the other three PASS already (they describe today's real-fetch behavior, which coalescing must not break).

- [ ] **Step 3: Add the constant**

In usage.go, right after `usageRefreshInterval`'s declaration (after line 761):

```go
// usageCoalesceWindow bounds how recently another process (or this same
// process's own prior pass) must have fetched an account for a pass that
// would otherwise make a real request to skip it and re-serve that reading
// as fresh instead. Deliberately half of usageRefreshInterval: a lone
// process's own next natural tick lands one full usageRefreshInterval after
// its own fetch, comfortably outside this window, so it never coalesces
// against itself (see TestUsageFetcherDoesNotSelfCoalesceAtItsOwnNextTick) —
// only two fetches landing close together (a launch burst, or two processes
// whose tickers happen to be closely phased) ever trigger it. An earlier
// version of this design used the full interval and was rejected on
// independent review for exactly this self-throttle failure mode.
const usageCoalesceWindow = usageRefreshInterval / 2
```

- [ ] **Step 4: Insert the coalescing branch**

In usage.go, inside `newUsageFetcher`'s disk-resolved branch, the code currently reads (lines 1037-1053):

```go
		if !backoff.due(now) {
			mirrorFallback(backoff, last)
			switch {
			case liveCarryable(last, live):
				// A copy, never the loaded value: last must keep its original
				// FetchedAt so the carry stays bounded, and must not itself become
				// stale — a later real fetch replaces it wholesale anyway.
				carried := *last
				carried.Stale = true
				return &carried, nil
			default:
				// Nothing safe to re-serve. Identity only, no numbers, and no
				// request — see the doc comment.
				return &AccountUsage{Account: live}, nil
			}
		}
		u, err := fetch()
```

Insert a new branch between the `if !backoff.due(now) { ... }` block and `u, err := fetch()`:

```go
		if !backoff.due(now) {
			mirrorFallback(backoff, last)
			switch {
			case liveCarryable(last, live):
				// A copy, never the loaded value: last must keep its original
				// FetchedAt so the carry stays bounded, and must not itself become
				// stale — a later real fetch replaces it wholesale anyway.
				carried := *last
				carried.Stale = true
				return &carried, nil
			default:
				// Nothing safe to re-serve. Identity only, no numbers, and no
				// request — see the doc comment.
				return &AccountUsage{Account: live}, nil
			}
		}
		// Read-before-fetch coalescing: another process (or this one, a
		// moment ago) already has a reading recent enough that a fetch here
		// would be redundant. liveCarryable is the same identity gate the
		// carry-forward branch above already trusts, so nothing is served
		// here that wasn't already safe to re-serve — this only changes HOW
		// recently trusted, not WHETHER. No persist: disk already reflects
		// this reading. See usageCoalesceWindow's doc comment for why a lone
		// process's own next tick can never trigger this.
		if liveCarryable(last, live) && !last.FetchedAt.After(now) && now.Sub(last.FetchedAt) < usageCoalesceWindow {
			coalesced := *last
			coalesced.Stale = false
			mirrorFallback(backoff, &coalesced)
			return &coalesced, nil
		}
		u, err := fetch()
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test . -run 'TestUsageFetcherCoalescesARecentReading|TestUsageFetcherFetchesPastTheCoalesceWindow|TestUsageFetcherDoesNotSelfCoalesceAtItsOwnNextTick|TestUsageFetcherDoesNotCoalesceAMismatchedEntry' -v`
Expected: all four PASS

- [ ] **Step 6: Commit**

```bash
git add usage.go usage_test.go
git commit -m "feat: coalesce a recent disk reading instead of re-fetching the live account"
```

---

### Task 4: Fix existing usage_test.go tests broken by Tasks 2 and 3

Both changes gate on real elapsed wall-clock time between calls. Every test below currently calls the fetcher multiple times back-to-back with zero elapsed time, relying on either "a streak-1 wait is free" (broken by Task 2) or "an unaged fixture is immediately re-fetchable" (broken by Task 3). The fix is uniform: install `fakeClock` and advance it by `2 * time.Minute` before every subsequent call meant to represent a later, independent attempt (comfortably past both the 1-minute margin and the 60-second coalesce window; leave it unadvanced between calls a test deliberately keeps close together, e.g. verifying a carry is re-served identically twice).

**Files:**
- Modify: `usage_test.go` (8 existing tests)

**Interfaces:**
- Consumes: `fakeClock` (Task 1), `usageClockNow` (Task 1).

- [ ] **Step 1: `TestUsageFetcherBacksOffRepeatedThrottles` (line 370)**

Current body (370-426, reproduced in full for context — see Task's earlier read):

```go
func TestUsageFetcherBacksOffRepeatedThrottles(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	loginAsWithSnapshot(t, "trecs", "andy@trecs.aero")
	calls := 0
	throttled := true
	good := liveUsageFixture(21)
	fetcher := newUsageFetcher(func() (*AccountUsage, error) {
		calls++
		if throttled {
			return nil, &usageHTTPError{Status: 429}
		}
		return good, nil
	}, saveAccountCache)

	// A first throttle imposes no wait at all — most heal within one tick.
	if _, err := fetcher(); err == nil {
		t.Fatal("pass 1 = nil error, want the throttle reported")
	}
	if calls != 1 {
		t.Fatalf("fetches = %d, want the first pass to fetch", calls)
	}
	// A success ends the streak before it can build.
	throttled = false
	u, err := fetcher()
	if err != nil || u != good {
		t.Fatalf("pass 2 = (%#v, %v), want the fresh numbers", u, err)
	}
	if calls != 2 {
		t.Fatalf("fetches = %d, want the pass after one throttle to fetch", calls)
	}

	// Now two consecutive throttles. The second is what arms the wait — which
	// also proves the success above cleared the streak, since otherwise this
	// second pass would already have been skipped.
	throttled = true
	for i := 3; i <= 4; i++ {
		if _, err := fetcher(); err == nil {
			t.Fatalf("pass %d = nil error, want the throttle reported", i)
		}
	}
	if calls != 4 {
		t.Fatalf("fetches = %d, want both throttled passes to have fetched", calls)
	}

	// Inside the armed wait the endpoint is not touched, and the skip reads as an
	// ordinary success carrying the last good numbers: an error here would engage
	// usagePoller.run's generic 5s-doubling retry, which is the burst this whole
	// mechanism exists to prevent.
	u, err = fetcher()
	if calls != 4 {
		t.Fatalf("fetches = %d, want the pass inside the backoff to skip the endpoint", calls)
	}
	if err != nil {
		t.Fatalf("backed-off pass = %v, want the last good reading re-served as a success", err)
	}
	carriedFrom(t, u, good)
}
```

Replace with:

```go
func TestUsageFetcherBacksOffRepeatedThrottles(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	loginAsWithSnapshot(t, "trecs", "andy@trecs.aero")
	clock := fakeClock(t, time.Now())
	calls := 0
	throttled := true
	good := liveUsageFixture(21)
	fetcher := newUsageFetcher(func() (*AccountUsage, error) {
		calls++
		if throttled {
			return nil, &usageHTTPError{Status: 429}
		}
		return good, nil
	}, saveAccountCache)

	// A first throttle imposes no wait of its own, but the safety margin
	// still requires real elapsed time before the next attempt.
	if _, err := fetcher(); err == nil {
		t.Fatal("pass 1 = nil error, want the throttle reported")
	}
	if calls != 1 {
		t.Fatalf("fetches = %d, want the first pass to fetch", calls)
	}
	// A success ends the streak before it can build.
	throttled = false
	*clock = clock.Add(2 * time.Minute)
	u, err := fetcher()
	if err != nil || u != good {
		t.Fatalf("pass 2 = (%#v, %v), want the fresh numbers", u, err)
	}
	if calls != 2 {
		t.Fatalf("fetches = %d, want the pass after one throttle to fetch", calls)
	}

	// Now two consecutive throttles, each separated by enough simulated time
	// to clear both the margin and the coalesce window. The second throttle
	// is what arms the wait — which also proves the success above cleared
	// the streak, since otherwise this second pass would already have been
	// skipped.
	throttled = true
	*clock = clock.Add(2 * time.Minute)
	if _, err := fetcher(); err == nil {
		t.Fatal("pass 3 = nil error, want the throttle reported")
	}
	*clock = clock.Add(2 * time.Minute)
	if _, err := fetcher(); err == nil {
		t.Fatal("pass 4 = nil error, want the throttle reported")
	}
	if calls != 4 {
		t.Fatalf("fetches = %d, want both throttled passes to have fetched", calls)
	}

	// Inside the armed wait (4 minutes) the endpoint is not touched, and the
	// skip reads as an ordinary success carrying the last good numbers: an
	// error here would engage usagePoller.run's generic 5s-doubling retry,
	// which is the burst this whole mechanism exists to prevent. No further
	// clock advance — this call happens immediately after pass 4, well
	// inside the 4-minute wait plus its own 1-minute margin.
	u, err = fetcher()
	if calls != 4 {
		t.Fatalf("fetches = %d, want the pass inside the backoff to skip the endpoint", calls)
	}
	if err != nil {
		t.Fatalf("backed-off pass = %v, want the last good reading re-served as a success", err)
	}
	carriedFrom(t, u, good)
}
```

- [ ] **Step 2: `TestUsageFetcherColdStartStopsFetchingOnceBackedOff` (line 459)**

Current (459-482):

```go
func TestUsageFetcherColdStartStopsFetchingOnceBackedOff(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	loginAsWithSnapshot(t, "trecs", "andy@trecs.aero")
	calls := 0
	fetcher := newUsageFetcher(func() (*AccountUsage, error) {
		calls++
		return nil, &usageHTTPError{Status: 429}
	}, saveAccountCache)
	// The first throttle imposes no wait; the second is what arms it. Both are
	// real attempts, and both report the error they actually got.
	for i := 1; i <= 2; i++ {
		if _, err := fetcher(); err == nil {
			t.Fatalf("pass %d = nil error, want the throttle reported", i)
		}
	}
	if calls != 2 {
		t.Fatalf("fetches = %d, want both throttled passes to have fetched", calls)
	}
	u, err := fetcher()
	if calls != 2 {
		t.Fatalf("fetches = %d, want the pass inside the backoff to skip the endpoint even with nothing to re-serve", calls)
	}
	placeholded(t, u, err, "andy@trecs.aero")
}
```

Replace the `for` loop with two explicit calls separated by a clock advance, and add the `fakeClock` line:

```go
func TestUsageFetcherColdStartStopsFetchingOnceBackedOff(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	loginAsWithSnapshot(t, "trecs", "andy@trecs.aero")
	clock := fakeClock(t, time.Now())
	calls := 0
	fetcher := newUsageFetcher(func() (*AccountUsage, error) {
		calls++
		return nil, &usageHTTPError{Status: 429}
	}, saveAccountCache)
	// The first throttle imposes no wait; the second is what arms it. Both are
	// real attempts, and both report the error they actually got.
	if _, err := fetcher(); err == nil {
		t.Fatal("pass 1 = nil error, want the throttle reported")
	}
	*clock = clock.Add(2 * time.Minute)
	if _, err := fetcher(); err == nil {
		t.Fatal("pass 2 = nil error, want the throttle reported")
	}
	if calls != 2 {
		t.Fatalf("fetches = %d, want both throttled passes to have fetched", calls)
	}
	u, err := fetcher()
	if calls != 2 {
		t.Fatalf("fetches = %d, want the pass inside the backoff to skip the endpoint even with nothing to re-serve", calls)
	}
	placeholded(t, u, err, "andy@trecs.aero")
}
```

- [ ] **Step 3: `TestUsageFetcherPlaceholdsWhenIdentityIsUnconfirmable` (line 489)**

Current (489-515): same `for i := 1; i <= 2` shape as Step 2, immediately followed by `os.Remove(...claude.json)` then a final call. Apply the identical fix: add `clock := fakeClock(t, time.Now())` after the `writeSnapshotFixture` call, replace the loop with two explicit calls separated by `*clock = clock.Add(2 * time.Minute)`:

```go
func TestUsageFetcherPlaceholdsWhenIdentityIsUnconfirmable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("HOME", home)
	writeLiveAccount(t, home, "andy@trecs.aero")
	writeSnapshotFixture(t, home, "trecs", "tok-trecs", "andy@trecs.aero")
	clock := fakeClock(t, time.Now())

	calls := 0
	fetcher := newUsageFetcher(func() (*AccountUsage, error) {
		calls++
		return nil, &usageHTTPError{Status: 429}
	}, saveAccountCache)
	if _, err := fetcher(); err == nil {
		t.Fatal("pass 1 = nil error, want the throttle reported")
	}
	*clock = clock.Add(2 * time.Minute)
	if _, err := fetcher(); err == nil {
		t.Fatal("pass 2 = nil error, want the throttle reported")
	}
	// Whose numbers these are can no longer be established.
	if err := os.Remove(filepath.Join(home, ".claude.json")); err != nil {
		t.Fatal(err)
	}
	u, err := fetcher()
	if calls != 2 {
		t.Fatalf("fetches = %d, want an unconfirmable identity to hold the wait, not spend a request", calls)
	}
	placeholded(t, u, err, "")
}
```

- [ ] **Step 4: `TestUsageFetcherCarriesNumbersRegardlessOfAge` (line 520)**

Current (520-543): the seed is already aged 6 hours (safe from Task 3's coalescing), but the two throttled passes are back-to-back (broken by Task 2's margin). Add `fakeClock` and one advance:

```go
func TestUsageFetcherCarriesNumbersRegardlessOfAge(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	loginAsWithSnapshot(t, "trecs", "andy@trecs.aero")
	old := liveUsageFixtureAged(37, 6*time.Hour)
	saveAccountCache("trecs", accountCacheEntry{Account: old.Account, Info: old.Info, FetchedAt: old.FetchedAt})
	clock := fakeClock(t, time.Now())
	calls := 0
	fetcher := newUsageFetcher(func() (*AccountUsage, error) {
		calls++
		return nil, &usageHTTPError{Status: 429}
	}, saveAccountCache)
	if _, err := fetcher(); err == nil {
		t.Fatal("pass 1 = nil error, want the throttle reported")
	}
	*clock = clock.Add(2 * time.Minute)
	if _, err := fetcher(); err == nil {
		t.Fatal("pass 2 = nil error, want the throttle reported")
	}
	u, err := fetcher()
	if calls != 2 {
		t.Fatalf("fetches = %d, want a still-carryable reading to hold the wait rather than force a fetch", calls)
	}
	if err != nil {
		t.Fatalf("backed-off pass = %v, want a success", err)
	}
	carriedFrom(t, u, old)
}
```

- [ ] **Step 5: `TestUsageFetcherPersistsBackoffTransitions` (line 761)**

Current (761-794): pass 1 throttles (streak→1), pass 2 (immediately after, `throttled=false`) expects a real fetch. Add `fakeClock` right after `loginAsWithSnapshot`, advance before pass 2:

```go
func TestUsageFetcherPersistsBackoffTransitions(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	loginAsWithSnapshot(t, "trecs", "andy@trecs.aero")
	clock := fakeClock(t, time.Now())
	type saveCall struct {
		name   string
		streak int
	}
	var saved []saveCall
	save := func(name string, e accountCacheEntry) {
		saved = append(saved, saveCall{name, e.BackoffStreak})
	}
	throttled := true
	good := liveUsageFixture(21)
	fetcher := newUsageFetcher(func() (*AccountUsage, error) {
		if throttled {
			return nil, &usageHTTPError{Status: 429}
		}
		return good, nil
	}, save)

	if _, err := fetcher(); err == nil {
		t.Fatal("pass 1 = nil error, want the throttle reported")
	}
	if len(saved) != 1 || saved[0].name != "trecs" || saved[0].streak != 1 {
		t.Fatalf("saved after first throttle = %+v, want [{trecs 1}]", saved)
	}
	throttled = false
	*clock = clock.Add(2 * time.Minute)
	if _, err := fetcher(); err != nil {
		t.Fatalf("pass 2 = %v, want a success", err)
	}
	if len(saved) != 2 || saved[1].streak != 0 {
		t.Fatalf("saved after success = %+v, want the wait cleared", saved)
	}
}
```

- [ ] **Step 6: `TestUsageFetcherBacksOffEvenWithNoMatchingSnapshot` (line 804)**

Current (804-827): fallback path (no snapshot), same `for i := 1; i <= 2` double-throttle shape. Add `fakeClock` after `loginAs`, replace the loop with two explicit calls plus an advance:

```go
func TestUsageFetcherBacksOffEvenWithNoMatchingSnapshot(t *testing.T) {
	loginAs(t, "andy@trecs.aero") // no snapshot for trecs — deliberately
	clock := fakeClock(t, time.Now())
	calls := 0
	fetcher := newUsageFetcher(func() (*AccountUsage, error) {
		calls++
		return nil, &usageHTTPError{Status: 429}
	}, noSaveAccountCache)

	// First throttle imposes no wait of its own; the second arms it. Both
	// still need real elapsed time between them to clear the safety margin.
	if _, err := fetcher(); err == nil {
		t.Fatal("pass 1 = nil error, want the throttle reported")
	}
	*clock = clock.Add(2 * time.Minute)
	if _, err := fetcher(); err == nil {
		t.Fatal("pass 2 = nil error, want the throttle reported")
	}
	if calls != 2 {
		t.Fatalf("fetches = %d, want both throttled passes to have fetched", calls)
	}
	if _, err := fetcher(); err != nil {
		t.Fatalf("backed-off pass = %v, want a success (in-memory fallback holds the wait)", err)
	}
	if calls != 2 {
		t.Fatalf("fetches = %d, want the pass inside the armed wait to skip the endpoint even with no snapshot to persist into", calls)
	}
}
```

- [ ] **Step 7: `TestUsageFetcherFallbackSurvivesAnUnrelatedDiskPass` (line 837)**

Current (837-885): avisoma (fallback) is armed to streak 2 via two immediate throttles, then a trecs (disk) pass, then back to avisoma. Add `fakeClock` after the `writeSnapshotFixture` line, advance once between the two avisoma-arming calls:

```go
func TestUsageFetcherFallbackSurvivesAnUnrelatedDiskPass(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TMPDIR", t.TempDir())
	// avisoma has no snapshot (fallback path); trecs has one (disk path).
	writeLiveAccount(t, home, "andy@avisoma.com")
	writeSnapshotFixture(t, home, "trecs", "tok-trecs", "andy@trecs.aero")
	clock := fakeClock(t, time.Now())

	calls := 0
	fetcher := newUsageFetcher(func() (*AccountUsage, error) {
		calls++
		if calls <= 2 {
			return nil, &usageHTTPError{Status: 429}
		}
		return &AccountUsage{Account: "andy@trecs.aero", Info: &UsageInfo{FiveHour: usageBucket{Pct: 55}}, FetchedAt: time.Now()}, nil
	}, noSaveAccountCache)

	// Arm avisoma's fallback streak to 2 (first throttle imposes no wait, the
	// second does — each still needs real elapsed time to clear the margin).
	if _, err := fetcher(); err == nil {
		t.Fatal("avisoma pass 1 = nil error, want the throttle reported")
	}
	*clock = clock.Add(2 * time.Minute)
	if _, err := fetcher(); err == nil {
		t.Fatal("avisoma pass 2 = nil error, want the throttle reported")
	}
	if calls != 2 {
		t.Fatalf("fetches = %d, want both avisoma passes to have fetched", calls)
	}

	// Switch to trecs, which resolves to the disk path and has nothing
	// cached — this used to unconditionally overwrite the shared fallback
	// vars with trecs's own (empty) state.
	writeLiveAccount(t, home, "andy@trecs.aero")
	if _, err := fetcher(); err != nil {
		t.Fatalf("trecs pass = %v, want a fresh fetch to succeed", err)
	}
	if calls != 3 {
		t.Fatalf("fetches = %d, want the trecs pass to have fetched", calls)
	}

	// Switch back to avisoma: its wait (armed two passes ago) must still be
	// honored, not reset by the intervening trecs pass. No clock advance —
	// avisoma's 4-minute armed wait must still be holding.
	writeLiveAccount(t, home, "andy@avisoma.com")
	if _, err := fetcher(); err != nil {
		t.Fatalf("avisoma pass after unrelated trecs pass = %v, want a success (fallback wait still armed)", err)
	}
	if calls != 3 {
		t.Fatalf("fetches = %d, want avisoma's armed wait to still hold after an unrelated account's disk pass", calls)
	}
}
```

- [ ] **Step 8: `TestUsageFetcherUnconfirmedIdentityWaitSurvivesALaterResolvedAccount` (line 896)**

Same shape as Step 7 (ownerless fallback armed to streak 2, then a trecs pass, then back). Add `fakeClock` after `writeSnapshotFixture`, advance once between the two arming calls:

```go
func TestUsageFetcherUnconfirmedIdentityWaitSurvivesALaterResolvedAccount(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TMPDIR", t.TempDir())
	writeSnapshotFixture(t, home, "trecs", "tok-trecs", "andy@trecs.aero")
	// No ~/.claude.json yet: identity starts unconfirmable.
	clock := fakeClock(t, time.Now())

	calls := 0
	fetcher := newUsageFetcher(func() (*AccountUsage, error) {
		calls++
		if calls <= 2 {
			return nil, &usageHTTPError{Status: 429}
		}
		return &AccountUsage{Account: "andy@trecs.aero", Info: &UsageInfo{FiveHour: usageBucket{Pct: 55}}, FetchedAt: time.Now()}, nil
	}, noSaveAccountCache)

	// Arm the ownerless wait to streak 2.
	if _, err := fetcher(); err == nil {
		t.Fatal("unconfirmed pass 1 = nil error, want the throttle reported")
	}
	*clock = clock.Add(2 * time.Minute)
	if _, err := fetcher(); err == nil {
		t.Fatal("unconfirmed pass 2 = nil error, want the throttle reported")
	}
	if calls != 2 {
		t.Fatalf("fetches = %d, want both unconfirmed passes to have fetched", calls)
	}

	// Identity becomes readable and resolves to a snapshotted account: the
	// disk path engages and, on the old unguarded write, would have mirrored
	// its own state over the ownerless wait (both looked like "" before the
	// sentinel).
	writeLiveAccount(t, home, "andy@trecs.aero")
	if _, err := fetcher(); err != nil {
		t.Fatalf("trecs pass = %v, want a fresh fetch to succeed", err)
	}
	if calls != 3 {
		t.Fatalf("fetches = %d, want the trecs pass to have fetched", calls)
	}

	// Identity becomes unconfirmable again: the ownerless wait armed at the
	// start must still be honored. No clock advance — still inside the
	// 4-minute armed wait.
	if err := os.Remove(filepath.Join(home, ".claude.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := fetcher(); err != nil {
		t.Fatalf("unconfirmed pass after trecs = %v, want a success (ownerless wait still armed)", err)
	}
	if calls != 3 {
		t.Fatalf("fetches = %d, want the ownerless wait to still hold after an unrelated resolved account's disk pass", calls)
	}
}
```

- [ ] **Step 9: `TestUsageFetcherReservesRepeatedlyWithoutMarkingItsOwnCopy` (line 548)**

Current (548-579): seeds a fresh (`liveUsageFixture(37)`, unaged) entry directly via `saveAccountCache`, then expects the first two calls to be real throttled fetches. This one is broken by Task 3 (coalescing on pass 1), not Task 2 — but the same `fakeClock` mechanism fixes it: install the clock, and advance past the coalesce window (not just the margin) before the first call, since the seed's `FetchedAt` is effectively "now" at seed time:

```go
func TestUsageFetcherReservesRepeatedlyWithoutMarkingItsOwnCopy(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	loginAsWithSnapshot(t, "trecs", "andy@trecs.aero")
	good := liveUsageFixture(37)
	saveAccountCache("trecs", accountCacheEntry{Account: good.Account, Info: good.Info, FetchedAt: good.FetchedAt})
	clock := fakeClock(t, time.Now())
	*clock = clock.Add(usageCoalesceWindow + time.Second)
	calls := 0
	fetcher := newUsageFetcher(func() (*AccountUsage, error) {
		calls++
		return nil, &usageHTTPError{Status: 429}
	}, saveAccountCache)
	if _, err := fetcher(); err == nil {
		t.Fatal("pass 1 = nil error, want the throttle reported")
	}
	*clock = clock.Add(2 * time.Minute)
	if _, err := fetcher(); err == nil {
		t.Fatal("pass 2 = nil error, want the throttle reported")
	}
	first, err := fetcher()
	if err != nil {
		t.Fatalf("first re-serve = %v, want a success", err)
	}
	carriedFrom(t, first, good)
	second, err := fetcher()
	if err != nil {
		t.Fatalf("second re-serve = %v, want a success", err)
	}
	carriedFrom(t, second, good)
	if calls != 2 {
		t.Fatalf("fetches = %d, want every re-serve to skip the endpoint", calls)
	}
	if first == second {
		t.Error("both re-serves handed out one shared copy; each pass must own its own")
	}
}
```

- [ ] **Step 10: `TestUsageFetcherReservesAPersistedEntry` (line 585)**

Same shape as Step 9 — fresh unaged seed, then two immediate throttles. Apply the identical fix:

```go
func TestUsageFetcherReservesAPersistedEntry(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	loginAsWithSnapshot(t, "trecs", "andy@trecs.aero")
	seed := liveUsageFixture(37)
	saveAccountCache("trecs", accountCacheEntry{Account: seed.Account, Info: seed.Info, FetchedAt: seed.FetchedAt})
	clock := fakeClock(t, time.Now())
	*clock = clock.Add(usageCoalesceWindow + time.Second)
	calls := 0
	fetcher := newUsageFetcher(func() (*AccountUsage, error) {
		calls++
		return nil, &usageHTTPError{Status: 429}
	}, saveAccountCache)
	if _, err := fetcher(); err == nil {
		t.Fatal("pass 1 = nil error, want the throttle reported")
	}
	*clock = clock.Add(2 * time.Minute)
	if _, err := fetcher(); err == nil {
		t.Fatal("pass 2 = nil error, want the throttle reported")
	}
	u, err := fetcher()
	if calls != 2 {
		t.Fatalf("fetches = %d, want the pass inside the backoff to skip the endpoint", calls)
	}
	if err != nil {
		t.Fatalf("backed-off pass = %v, want the persisted entry re-served as a success", err)
	}
	carriedFrom(t, u, seed)
}
```

- [ ] **Step 11: `TestUsageFetcherDropsAnotherAccountsNumbers` (line 615)**

Combines a fresh-seed coalescing issue (trecs pass 1) with a margin issue (avisoma's two passes after the switch). Add `fakeClock`, advance past the coalesce window before the first trecs call, and advance again between the avisoma pair:

```go
func TestUsageFetcherDropsAnotherAccountsNumbers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TMPDIR", t.TempDir())
	writeLiveAccount(t, home, "andy@trecs.aero")
	writeSnapshotFixture(t, home, "trecs", "tok-trecs", "andy@trecs.aero")
	writeSnapshotFixture(t, home, "avisoma", "tok-avisoma", "andy@avisoma.com")

	calls := 0
	seed := liveUsageFixture(37)
	saveAccountCache("trecs", accountCacheEntry{Account: seed.Account, Info: seed.Info, FetchedAt: seed.FetchedAt})
	clock := fakeClock(t, time.Now())
	*clock = clock.Add(usageCoalesceWindow + time.Second)
	fetcher := newUsageFetcher(func() (*AccountUsage, error) {
		calls++
		return nil, &usageHTTPError{Status: 429}
	}, saveAccountCache)
	if _, err := fetcher(); err == nil {
		t.Fatal("pass 1 = nil error, want the throttle reported")
	}
	*clock = clock.Add(2 * time.Minute)
	if _, err := fetcher(); err == nil {
		t.Fatal("pass 2 = nil error, want the throttle reported")
	}
	u, err := fetcher()
	if calls != 2 || err != nil {
		t.Fatalf("backed-off pass = %v after %d fetches, want the persisted entry re-served", err, calls)
	}
	carriedFrom(t, u, seed)

	// Ctrl+W, or a relogin: this pass now resolves to avisoma's own slot, which
	// has nothing armed against it yet.
	writeLiveAccount(t, home, "andy@avisoma.com")
	if _, err := fetcher(); err == nil {
		t.Fatal("pass after the switch = nil error, want a real fetch's throttle")
	}
	if calls != 3 {
		t.Fatalf("fetches = %d, want the switch to have forced a fetch", calls)
	}
	// avisoma's first throttle imposes no wait of its own, but the safety
	// margin still requires real elapsed time before the very next pass.
	*clock = clock.Add(2 * time.Minute)
	if _, err := fetcher(); err == nil {
		t.Fatal("pass after the switch's throttle = nil error, want a real fetch")
	}
	if calls != 4 {
		t.Fatalf("fetches = %d, want the new account's second throttle to also have fetched", calls)
	}
}
```

- [ ] **Step 12: `TestUsageFetcherPreservesVerifiedAcrossACarry` (line 976)**

Current (976-...): seeds a fresh (`liveUsageFixture(21)`, unaged) `Verified: true` entry, then a single call against an always-503 mock, expecting a real fetch (and the carry to preserve `Verified`). This is a coalescing conflict (Task 3) — 503 is not a throttle, but the coalescing check runs regardless of what the mock WOULD return, since it intercepts before `fetch()` is ever called. Fix: advance past the coalesce window before the call.

Read the full current body first (usage_test.go:976-999) to confirm exact structure, then add `clock := fakeClock(t, time.Now())` after the `saveAccountCache` call and `*clock = clock.Add(usageCoalesceWindow + time.Second)` immediately before the `fetcher()` call that currently expects the 503 to be reached.

- [ ] **Step 13: Run every fixed test**

Run: `go test . -run 'TestUsageFetcherBacksOffRepeatedThrottles|TestUsageFetcherColdStartStopsFetchingOnceBackedOff|TestUsageFetcherPlaceholdsWhenIdentityIsUnconfirmable|TestUsageFetcherCarriesNumbersRegardlessOfAge|TestUsageFetcherPersistsBackoffTransitions|TestUsageFetcherBacksOffEvenWithNoMatchingSnapshot|TestUsageFetcherFallbackSurvivesAnUnrelatedDiskPass|TestUsageFetcherUnconfirmedIdentityWaitSurvivesALaterResolvedAccount|TestUsageFetcherReservesRepeatedlyWithoutMarkingItsOwnCopy|TestUsageFetcherReservesAPersistedEntry|TestUsageFetcherDropsAnotherAccountsNumbers|TestUsageFetcherPreservesVerifiedAcrossACarry' -v`
Expected: all PASS

- [ ] **Step 14: Run the whole file plus the rest of the package**

Run: `go test ./...`
Expected: PASS (known_accounts_test.go failures are expected and addressed in Task 5)

- [ ] **Step 15: Commit**

```bash
git add usage_test.go
git commit -m "test: adapt usage_test.go to the new backoff margin and coalescing"
```

---

### Task 5: Known-account coalescing + fix known_accounts_test.go

**Files:**
- Modify: `known_accounts.go` (per-account loop, lines 488-561)
- Modify: `known_accounts_test.go` (6 existing tests, per the audit below)

**Interfaces:**
- Consumes: `usageCoalesceWindow` (Task 3), `carryable` (existing, known_accounts.go:368-370), `fakeClock`/`usageClockNow` (Task 1).

- [ ] **Step 1: Write the failing test**

Add to known_accounts_test.go, near `TestKnownAccountsFetcherCarriesNumbersAcrossPolls` (the file already has an equivalent seeding pattern to copy from — read that test first for exact helper names, e.g. how `newKnownAccountsFetcher`'s save is constructed and how a per-account fetch stub is wired, since known accounts use `snapshotToken`/HTTP roundtrip mocking rather than a bare `fetch func() (*AccountUsage, error)`). Add:

```go
// The known-accounts sibling of TestUsageFetcherCoalescesARecentReading: a
// fresh disk entry for a non-live snapshot account means the next pass
// skips the network round trip and re-serves it as fresh, not stale.
func TestKnownAccountsFetcherCoalescesARecentReading(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TMPDIR", t.TempDir())
	writeLiveAccount(t, home, "andy@avisoma.com")
	writeSnapshotFixture(t, home, "trecs", "tok-trecs", "andy@trecs.aero")
	clock := fakeClock(t, time.Now())

	saveAccountCache("trecs", accountCacheEntry{
		Account:   "andy@trecs.aero",
		Info:      &UsageInfo{FiveHour: usageBucket{Pct: 63}},
		FetchedAt: time.Now(),
		Verified:  true,
	})
	calls := 0
	swapFetch(t, func(token string) (*UsageInfo, error) {
		calls++
		return nil, &usageHTTPError{Status: 429}
	})
	fetcher := newKnownAccountsFetcher(noSaveAccountCache)
	res, err := fetcher()
	if err != nil {
		t.Fatalf("pass = %v, want a success", err)
	}
	if calls != 0 {
		t.Fatalf("fetches = %d, want a fresh disk entry to be coalesced instead of fetched", calls)
	}
	if len(res.Accounts) != 1 {
		t.Fatalf("accounts = %#v, want exactly trecs", res.Accounts)
	}
	got := res.Accounts[0]
	if got.Stale || got.Reason != "" {
		t.Fatalf("coalesced entry = %#v, want fresh (non-stale, no reason)", got)
	}
	if got.Info == nil || got.Info.FiveHour.Pct != 63 {
		t.Fatalf("coalesced entry numbers = %#v, want the seeded 63%%", got.Info)
	}
	_ = clock
}
```

Note: `swapFetch(t, fetch)` is a real, already-existing helper (known_accounts_test.go:193-198) that points the package var `usageInfoFetch` (known_accounts.go:239, `func(token string) (*UsageInfo, error)`) at a stub for the test's duration, restoring it via `t.Cleanup`. `noSaveAccountCache` (known_accounts_test.go:21) is the no-op `save` arg. Both are used exactly as written above.

- [ ] **Step 2: Run it to verify it fails**

Run: `go test . -run TestKnownAccountsFetcherCoalescesARecentReading -v`
Expected: FAIL (calls=1, not 0)

- [ ] **Step 3: Insert the coalescing branch**

In known_accounts.go, the per-account loop currently reads (lines 511-534):

```go
			backoff := cached.backoff()
			if !entryIdentityMatches(cached, email) && cached.Account != "" {
				// Unlike numbers (guarded by knownAccountUsage's own carryable
				// check above), nothing gated the backoff half until an
				// independent review caught it: a claude-switch snapshot name
				// reassigned to a different account (`account save --force`)
				// would otherwise inherit the outgoing account's armed wait
				// even though the new account was never itself throttled.
				backoff = usageBackoff{}
			}
			attempted := backoff.due(now)
			var r *KnownAccountUsage
			if attempted {
				r, _ = fetchKnownAccountUsage(name, liveEmail, prev)
			} else {
				// A backed-off account still goes through knownAccountUsage, with the
				// answer a throttled endpoint would have given substituted for the
				// round trip. That keeps one place deciding whether this name stands
				// for the live account, whether prev's numbers may be carried
				// forward, and how the entry reads.
				r, _ = knownAccountUsage(name, liveEmail, prev, func(string) (*UsageInfo, error) {
					return nil, &usageHTTPError{Status: http.StatusTooManyRequests}
				})
			}
```

Replace with:

```go
			backoff := cached.backoff()
			if !entryIdentityMatches(cached, email) && cached.Account != "" {
				// Unlike numbers (guarded by knownAccountUsage's own carryable
				// check above), nothing gated the backoff half until an
				// independent review caught it: a claude-switch snapshot name
				// reassigned to a different account (`account save --force`)
				// would otherwise inherit the outgoing account's armed wait
				// even though the new account was never itself throttled.
				backoff = usageBackoff{}
			}
			attempted := backoff.due(now)
			// Read-before-fetch coalescing: another process (or this one, a
			// moment ago) already has a reading recent enough that a round
			// trip here would be redundant. carryable is the same identity
			// gate the ordinary carry-forward path (inside knownAccountUsage's
			// failed helper) already trusts, so nothing is served here that
			// wasn't already safe to re-serve. Reason is cleared: this is
			// being treated as equivalent to a fresh success, not a failure.
			coalesced := attempted && carryable(prev, email) && !prev.FetchedAt.After(now) && now.Sub(prev.FetchedAt) < usageCoalesceWindow
			var r *KnownAccountUsage
			switch {
			case coalesced:
				fresh := *prev
				fresh.Stale, fresh.Reason = false, ""
				r = &fresh
				attempted = false
			case attempted:
				r, _ = fetchKnownAccountUsage(name, liveEmail, prev)
			default:
				// A backed-off account still goes through knownAccountUsage, with the
				// answer a throttled endpoint would have given substituted for the
				// round trip. That keeps one place deciding whether this name stands
				// for the live account, whether prev's numbers may be carried
				// forward, and how the entry reads.
				r, _ = knownAccountUsage(name, liveEmail, prev, func(string) (*UsageInfo, error) {
					return nil, &usageHTTPError{Status: http.StatusTooManyRequests}
				})
			}
```

Note: `carryable(prev, email)` safely handles `prev == nil` — `fresh(nil)` (which `carryable` calls first) returns false, and Go's `&&` short-circuits before `prev.FetchedAt` is ever dereferenced.

Then, further down, the unconditional `save(name, e)` (currently line 560):

```go
			e := accountCacheEntry{BackoffStreak: backoff.streak, BackoffNextAttempt: backoff.nextAttempt}
			if r.Info != nil {
				e.Account, e.FetchedAt, e.Info, e.Verified = r.Account, r.FetchedAt, r.Info, r.Verified
			}
			save(name, e)
```

Change to:

```go
			e := accountCacheEntry{BackoffStreak: backoff.streak, BackoffNextAttempt: backoff.nextAttempt}
			if r.Info != nil {
				e.Account, e.FetchedAt, e.Info, e.Verified = r.Account, r.FetchedAt, r.Info, r.Verified
			}
			if !coalesced {
				save(name, e)
			}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test . -run TestKnownAccountsFetcherCoalescesARecentReading -v`
Expected: PASS

- [ ] **Step 5: Fix the 6 existing broken tests**

Apply the identical `fakeClock` + `2 * time.Minute` advance pattern from Task 4 to each of the following (read each test's current full body first — known_accounts_test.go's helper names for injecting the HTTP fetch stub differ slightly from usage_test.go's, so match whatever `TestKnownAccountsFetcherCarriesNumbersAcrossPolls` already uses):

- `TestKnownAccountsFetcherRetriesAFailedAccountFirst` (line 646): install `fakeClock` after setup; advance `2 * time.Minute` before the second `fetcher()` call so bravo's armed streak-1 wait clears the margin AND alpha/charlie's fresh persisted entries from pass 1 clear the coalesce window.
- `TestKnownAccountsFetcherPersistsBackoffAfterEachPass` (line 750): install `fakeClock`; advance `2 * time.Minute` between pass 1 (throttle) and pass 2 (success).
- `TestKnownAccountsFetcherCarriesNumbersAcrossPolls` (line 792): install `fakeClock`; advance `2 * time.Minute` between pass 1 (success, persists fresh entries) and pass 2 (expects the 429 stub to actually fire and produce `Stale`/`"rate limited"` entries).
- `TestKnownAccountsFetcherBacksOffRepeatedThrottles` (line 911): install `fakeClock`; advance `2 * time.Minute` before each of pass 2 and pass 3 (mirrors Task 4 Step 1's usage.go sibling).
- `TestKnownAccountsFetcherResolvesActiveNameWhileBackedOff` (line 999): install `fakeClock`; advance `2 * time.Minute` between the two throttle passes.
- `TestKnownAccountsFetcherThreadsEachAccountsOwnEntry` (line 541): not a hard failure (no `calls` assertion), but the seeded entry (`FetchedAt: time.Now()`) is now coalesced instead of exercising the 429 stub it was written to test. Install `fakeClock` and advance past `usageCoalesceWindow` before the call, to keep the test exercising what its own comment says it exercises.
- `TestKnownAccountsFetcherSeedsFromCache` (line 863): currently borderline-safe (seed is `time.Now().Add(-time.Minute)`, right at the coalesce boundary). Harden it: change the seed's `FetchedAt` to `time.Now().Add(-90 * time.Second)`, comfortably past `usageCoalesceWindow`, removing the timing race.

For each, use the exact same mechanism as Task 4: `clock := fakeClock(t, time.Now())` near the top (after HOME/TMPDIR/snapshot setup, before the first fetcher call), then `*clock = clock.Add(2 * time.Minute)` immediately before whichever call needs to be treated as later/independent — read the current body first, since the exact insertion point depends on that test's specific call sequence.

- [ ] **Step 6: Run the fixed tests**

Run: `go test . -run 'TestKnownAccountsFetcherRetriesAFailedAccountFirst|TestKnownAccountsFetcherPersistsBackoffAfterEachPass|TestKnownAccountsFetcherCarriesNumbersAcrossPolls|TestKnownAccountsFetcherBacksOffRepeatedThrottles|TestKnownAccountsFetcherResolvesActiveNameWhileBackedOff|TestKnownAccountsFetcherThreadsEachAccountsOwnEntry|TestKnownAccountsFetcherSeedsFromCache|TestKnownAccountsFetcherCoalescesARecentReading' -v`
Expected: all PASS

- [ ] **Step 7: Run the whole package**

Run: `go test ./...`
Expected: PASS

- [ ] **Step 8: Commit**

```bash
git add known_accounts.go known_accounts_test.go
git commit -m "feat: coalesce a recent disk reading instead of re-fetching known accounts"
```

---

### Task 6: Stale-age display in the header

**Files:**
- Modify: `render.go` (`accountUsageLine` struct at 595-622, `dedupeAccounts` at 697-760ish, `writeUsageHeader`'s `addClaude` closure at 830-845)
- Test: `render_test.go` (new tests near `TestWriteUsageHeaderMarksStaleNumbers`)

**Interfaces:**
- Consumes: `formatAge(seconds float64) string` (existing, render.go:1149).
- Produces: `accountUsageLine.fetchedAt time.Time` field, consumed by `writeUsageHeader`.

- [ ] **Step 1: Write the failing tests**

Add to render_test.go, right after `TestWriteUsageHeaderMarksStaleNumbers`:

```go
func TestWriteUsageHeaderShowsStaleAge(t *testing.T) {
	now := time.Now()
	accounts := []accountUsageLine{
		{label: "avisoma", email: "andy@avisoma.com", info: &UsageInfo{FiveHour: usageBucket{Pct: 41}}},
		{
			label: "trecs", email: "andy@trecs.aero",
			info: &UsageInfo{FiveHour: usageBucket{Pct: 63}}, stale: true,
			fetchedAt: now.Add(-12 * time.Minute),
		},
	}
	var b strings.Builder
	writeUsageHeader(&b, accounts, nil, 0)
	lines := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %#v, want one per account", lines)
	}
	want := usageStaleText + " 12m"
	if !strings.HasSuffix(lines[1], " "+dim(want)) {
		t.Fatalf("stale line = %q, want it suffixed with %q", lines[1], want)
	}
	if strings.Contains(lines[0], usageStaleText) {
		t.Fatalf("fresh line picked up the marker: %q", lines[0])
	}
}

func TestWriteUsageHeaderStaleWithoutAgeStaysBare(t *testing.T) {
	accounts := []accountUsageLine{
		{label: "trecs", email: "andy@trecs.aero", info: &UsageInfo{FiveHour: usageBucket{Pct: 63}}, stale: true},
	}
	var b strings.Builder
	writeUsageHeader(&b, accounts, nil, 0)
	line := strings.TrimRight(b.String(), "\n")
	if !strings.HasSuffix(line, " "+dim(usageStaleText)) {
		t.Fatalf("stale line with zero FetchedAt = %q, want the bare word with no age", line)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test . -run 'TestWriteUsageHeaderShowsStaleAge|TestWriteUsageHeaderStaleWithoutAgeStaysBare' -v`
Expected: `TestWriteUsageHeaderShowsStaleAge` FAILs (the suffix is still the bare `usageStaleText`, no age); `TestWriteUsageHeaderStaleWithoutAgeStaysBare` already PASSes (documents the no-regression case).

- [ ] **Step 3: Add the field to `accountUsageLine`**

In render.go, the struct (lines 595-622) currently ends:

```go
	// placeholder is the dim text a known account with no numbers renders
	// instead of bars: "auth expired" for a dead credential (the actionable
	// one — claude-switch into it and log in again), or whatever reason the
	// failed attempt classified as otherwise ("rate limited", "bad snapshot",
	// …). Empty means a normal segment line. Such
	// a line is deliberately never dropped, so the account's existence stays
	// visible either way.
	placeholder string
}
```

Insert a new field before `placeholder`:

```go
	// fetchedAt is when info was actually fetched — used to render the stale
	// marker's age ("stale 12m") instead of a bare fixed word. Zero when the
	// source never stamped one (a pre-timestamp disk entry, or a line with
	// nothing to carry): the bare "stale" word is rendered in that case, not
	// a nonsensical multi-decade duration.
	fetchedAt time.Time
	// placeholder is the dim text a known account with no numbers renders
	// instead of bars: "auth expired" for a dead credential (the actionable
	// one — claude-switch into it and log in again), or whatever reason the
	// failed attempt classified as otherwise ("rate limited", "bad snapshot",
	// …). Empty means a normal segment line. Such
	// a line is deliberately never dropped, so the account's existence stays
	// visible either way.
	placeholder string
}
```

- [ ] **Step 4: Thread `FetchedAt` through `dedupeAccounts`**

In render.go, the `add` closure and its two call sites (lines 700-721) currently read:

```go
	add := func(account, host string, info *UsageInfo, stale, isLocal bool) {
		if info == nil {
			return
		}
		mine := isLocal || (account != "" && strings.EqualFold(account, local.Account))
		if account == "" {
			lines = append(lines, accountUsageLine{label: host, info: info, mine: mine, stale: stale})
			return
		}
		key := strings.ToLower(account)
		if seen[key] {
			return
		}
		seen[key] = true
		lines = append(lines, accountUsageLine{label: accountLocalPart(account), email: account, info: info, mine: mine, stale: stale})
	}
	add(local.Account, "local", local.Info, local.Stale, true)
	for _, r := range remotes {
		if r.Usage != nil {
			add(r.Usage.Account, r.Name, r.Usage.Info, r.Usage.Stale, false)
		}
	}
```

Replace with:

```go
	add := func(account, host string, info *UsageInfo, stale bool, fetchedAt time.Time, isLocal bool) {
		if info == nil {
			return
		}
		mine := isLocal || (account != "" && strings.EqualFold(account, local.Account))
		if account == "" {
			lines = append(lines, accountUsageLine{label: host, info: info, mine: mine, stale: stale, fetchedAt: fetchedAt})
			return
		}
		key := strings.ToLower(account)
		if seen[key] {
			return
		}
		seen[key] = true
		lines = append(lines, accountUsageLine{label: accountLocalPart(account), email: account, info: info, mine: mine, stale: stale, fetchedAt: fetchedAt})
	}
	add(local.Account, "local", local.Info, local.Stale, local.FetchedAt, true)
	for _, r := range remotes {
		if r.Usage != nil {
			add(r.Usage.Account, r.Name, r.Usage.Info, r.Usage.Stale, r.Usage.FetchedAt, false)
		}
	}
```

Then the `addKnown` closure's `candidate` construction (around line 749-755):

```go
		candidate := accountUsageLine{
			label:       label,
			email:       k.Account,
			info:        k.Info,
			stale:       k.Stale,
			placeholder: knownAccountPlaceholder(k),
		}
```

Change to:

```go
		candidate := accountUsageLine{
			label:       label,
			email:       k.Account,
			info:        k.Info,
			stale:       k.Stale,
			fetchedAt:   k.FetchedAt,
			placeholder: knownAccountPlaceholder(k),
		}
```

- [ ] **Step 5: Render the age in `writeUsageHeader`**

In render.go, `writeUsageHeader`'s signature and `addClaude` closure (lines 809-845) currently read:

```go
func writeUsageHeader(w io.Writer, accounts []accountUsageLine, codexAccounts []codexAccountLine, cols int) {
	type entry struct {
		label string
		segs  []usageSeg
		placeholder string
		suffix  string
		suffixW int
	}
	var entries []entry
	codexPresent := len(codexAccounts) > 0

	addClaude := func(label string, a accountUsageLine) {
		if a.placeholder != "" {
			entries = append(entries, entry{label: label, placeholder: a.placeholder})
			return
		}
		if a.info == nil {
			return
		}
		if segs := claudeSegs(a.info); len(segs) > 0 {
			suffix, suffixW := "", 0
			if a.stale {
				suffix, suffixW = " "+dim(usageStaleText), 1+len(usageStaleText)
			}
			entries = append(entries, entry{label: label, segs: segs, suffix: suffix, suffixW: suffixW})
		}
	}
```

(comments in the real file are preserved as-is; only the two bodies below change). Add `now := time.Now()` right after `var entries []entry`, and change the `if a.stale` block:

```go
func writeUsageHeader(w io.Writer, accounts []accountUsageLine, codexAccounts []codexAccountLine, cols int) {
	type entry struct {
		label string
		segs  []usageSeg
		placeholder string
		suffix  string
		suffixW int
	}
	var entries []entry
	// now is captured once so every stale line in this render agrees on its
	// age relative to a single instant, rather than drifting line to line.
	now := time.Now()
	codexPresent := len(codexAccounts) > 0

	addClaude := func(label string, a accountUsageLine) {
		if a.placeholder != "" {
			entries = append(entries, entry{label: label, placeholder: a.placeholder})
			return
		}
		if a.info == nil {
			return
		}
		if segs := claudeSegs(a.info); len(segs) > 0 {
			suffix, suffixW := "", 0
			if a.stale {
				text := usageStaleText
				if !a.fetchedAt.IsZero() {
					text += " " + formatAge(now.Sub(a.fetchedAt).Seconds())
				}
				suffix, suffixW = " "+dim(text), 1+len(text)
			}
			entries = append(entries, entry{label: label, segs: segs, suffix: suffix, suffixW: suffixW})
		}
	}
```

(Keep every other line of the function, including the doc comments above `type entry struct` and `suffix`/`suffixW`, exactly as they are today — only the two shown bodies change.)

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test . -run 'TestWriteUsageHeaderShowsStaleAge|TestWriteUsageHeaderStaleWithoutAgeStaysBare' -v`
Expected: both PASS

- [ ] **Step 7: Run the full existing render test suite to confirm no regressions**

Run: `go test . -run 'TestWriteUsageHeader|TestDedupeAccounts' -v`
Expected: all PASS, including `TestWriteUsageHeaderStaleMarkerShiftsNoOtherColumn` and the narrow-terminal tests — none of them set `fetchedAt`, so they render the bare word exactly as before.

- [ ] **Step 8: Add a narrow-width test for the longer suffix**

Add to render_test.go, after `TestWriteUsageHeaderShowsStaleAge`:

```go
// A longer suffix ("stale 12m" vs "stale") must still be accounted for in
// bar-width sizing at a narrow terminal — render.go's own history documents
// a real prior bug from getting stale-marker sizing wrong (a marker sized
// out of the shared bar width, then clipped away by cropTableFrame).
func TestWriteUsageHeaderStaleAgeSurvivesNarrowWidth(t *testing.T) {
	now := time.Now()
	accounts := []accountUsageLine{
		{
			label: "trecs", email: "andy@trecs.aero", mine: true,
			info: &UsageInfo{FiveHour: usageBucket{Pct: 63}}, stale: true,
			fetchedAt: now.Add(-3 * time.Hour),
		},
	}
	var b strings.Builder
	writeUsageHeader(&b, accounts, nil, 40)
	line := strings.TrimRight(b.String(), "\n")
	want := usageStaleText + " 3h"
	if !strings.Contains(line, want) {
		t.Fatalf("narrow-width stale line = %q, want it to still contain %q", line, want)
	}
}
```

Run: `go test . -run TestWriteUsageHeaderStaleAgeSurvivesNarrowWidth -v`
Expected: PASS. If it fails, inspect `cropTableFrame`'s clip behavior and `lineBarW`/`usageLineFixedWidth`'s sizing (render.go) — `suffixW` should already be accounted for per their existing doc comments (lines 818-825); this test is verification, not expected to require a further code change, but if it does, the fix belongs in `lineBarW`/`usageLineFixedWidth`, not in this task's other steps.

- [ ] **Step 9: Run the whole suite**

Run: `go test ./...`
Expected: PASS

- [ ] **Step 10: Commit**

```bash
git add render.go render_test.go
git commit -m "feat: show carried-forward numbers' age instead of a bare 'stale' word"
```

---

### Task 7: Final verification

**Files:** none (verification only)

- [ ] **Step 1: Full build, vet, test**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: all clean, zero failures

- [ ] **Step 2: Confirm no stray debug code**

Run: `git diff main --stat` (from the worktree, against the branch point) and read through the full diff once — check for leftover `fmt.Println`/`t.Skip`/commented-out code.

- [ ] **Step 3: Update the design doc's status** (optional but recommended — matches this repo's pattern of specs staying accurate after implementation)

No code change; if anything in the design doc's "Out of scope" section turned out different during implementation (e.g. an exact test count), note it, but the design's substance should already match what was built.
