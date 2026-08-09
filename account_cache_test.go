package main

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestAccountCacheRoundTrip(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	if got, _ := loadAccountCache("trecs"); !got.empty() {
		t.Fatalf("loadAccountCache with no file = %+v, want empty", got)
	}
	fetchedAt := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
	want := accountCacheEntry{
		Account:   "andy@trecs.aero",
		FetchedAt: fetchedAt,
		Info:      &UsageInfo{FiveHour: usageBucket{Pct: 42}},
		Verified:  true,
	}
	saveAccountCache("trecs", want)
	got, _ := loadAccountCache("trecs")
	if got.Account != "andy@trecs.aero" || got.Info == nil || got.Info.FiveHour.Pct != 42 || !got.Verified {
		t.Fatalf("round-trip = %+v, want %+v", got, want)
	}
	if !got.FetchedAt.Equal(fetchedAt) {
		t.Errorf("round-trip FetchedAt = %v, want %v", got.FetchedAt, fetchedAt)
	}
	// A disk seed is a read, not a completed poll: it must never render as
	// indistinguishable from a live fetch.
	if !got.Stale {
		t.Error("loaded entry with Info = not Stale, want every disk seed marked Stale")
	}
}

// The numbers half and the backoff half persist and load independently within
// the same entry — an account can have real numbers with no active wait, an
// active wait with no numbers yet, or both.
func TestAccountCacheHoldsNumbersAndBackoffTogether(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	next := time.Now().Add(6 * time.Minute).UTC().Truncate(time.Second)
	saveAccountCache("avisoma", accountCacheEntry{
		Account:            "andy@avisoma.com",
		Info:               &UsageInfo{FiveHour: usageBucket{Pct: 5}},
		FetchedAt:          time.Now().Add(-time.Hour),
		BackoffStreak:      2,
		BackoffNextAttempt: next,
	})
	got, _ := loadAccountCache("avisoma")
	if got.Info == nil || got.Info.FiveHour.Pct != 5 {
		t.Fatalf("numbers half lost: %+v", got)
	}
	if got.BackoffStreak != 2 || !got.BackoffNextAttempt.Equal(next) {
		t.Fatalf("backoff half lost or wrong: %+v, want streak 2 next %v", got, next)
	}
}

// An entry with neither numbers nor an active wait removes the file rather
// than writing an empty one — a later load must see "nothing persisted", not
// a technically valid but meaningless empty entry.
func TestAccountCacheEmptyEntryRemovesFile(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	saveAccountCache("trecs", accountCacheEntry{Account: "andy@trecs.aero", Info: &UsageInfo{}})
	if got, _ := loadAccountCache("trecs"); got.empty() {
		t.Fatal("setup: expected a cached entry before clearing it")
	}
	saveAccountCache("trecs", accountCacheEntry{})
	if got, _ := loadAccountCache("trecs"); !got.empty() {
		t.Fatalf("after clearing, entry = %+v, want empty", got)
	}
	if _, err := os.Stat(accountCachePath("trecs")); !os.IsNotExist(err) {
		t.Errorf("cache file still exists after clearing, err = %v", err)
	}
}

// An Account with nothing else (no numbers, no wait) is not worth a file
// either — the same "nothing to keep" rule as the fully empty case.
func TestAccountCacheBareIdentityIsEmpty(t *testing.T) {
	e := accountCacheEntry{Account: "andy@trecs.aero"}
	if !e.empty() {
		t.Errorf("entry with only Account set = not empty, want empty (nothing worth a file)")
	}
}

// An elapsed backoff deadline keeps its streak: due() (usageBackoff.due)
// already reports the account fetchable once nextAttempt has passed, and
// loadAccountCache runs on every single pass now (not once at process
// startup), so a streak of 1 — whose nextAttempt is the instant it was
// recorded, wait zero by design — would read as "elapsed" on the very next
// load if this dropped the streak, silently preventing it from ever reaching
// 2 and arming a real wait. An earlier version of this function did exactly
// that; TestUsageFetcherBacksOffRepeatedThrottles and its known-accounts
// equivalent are what caught it.
func TestAccountCacheKeepsAnElapsedWaitsStreak(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	saveAccountCache("avisoma", accountCacheEntry{
		Account:            "andy@avisoma.com",
		BackoffStreak:      3,
		BackoffNextAttempt: time.Now().Add(-time.Minute),
	})
	got, _ := loadAccountCache("avisoma")
	if got.BackoffStreak != 3 {
		t.Fatalf("elapsed wait loaded as streak=%d, want the streak preserved (due() alone decides fetchability)", got.BackoffStreak)
	}
	if !got.backoff().due(time.Now()) {
		t.Fatal("elapsed wait still reports not due — the account would never be retried")
	}
}

// A clamp that only lived in memory would leave a bogus far-future value on
// disk to be re-clamped fresh (and never actually fixed) on every future
// load — a crash loop that never lets the real deadline elapse would never
// heal. loadAccountCache itself never writes (its own doc comment: the
// caller holds the injected save seam it doesn't), so it reports the clamp
// via its second return value, and the caller — mirroring what
// newUsageFetcher/newKnownAccountsFetcher actually do — writes the
// correction back through its own save.
func TestAccountCacheClampSelfHeals(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	saveAccountCache("avisoma", accountCacheEntry{
		Account:            "andy@avisoma.com",
		BackoffStreak:      5,
		BackoffNextAttempt: time.Now().Add(6 * time.Hour),
	})
	got, clamped := loadAccountCache("avisoma")
	if got.BackoffStreak != 5 {
		t.Fatalf("setup: loaded = %+v, want the clamped wait with streak preserved", got)
	}
	if !clamped {
		t.Fatal("loadAccountCache did not report the excessive deadline as clamped")
	}
	if max := time.Now().Add(usageBackoffCeiling + 5*time.Second); got.BackoffNextAttempt.After(max) {
		t.Fatalf("returned next_attempt = %v, want it clamped to within the ceiling", got.BackoffNextAttempt)
	}

	saveAccountCache("avisoma", got)
	data, err := os.ReadFile(accountCachePath("avisoma"))
	if err != nil {
		t.Fatal(err)
	}
	var raw accountCacheEntry
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if max := time.Now().Add(usageBackoffCeiling + 5*time.Second); raw.BackoffNextAttempt.After(max) {
		t.Fatalf("raw file next_attempt = %v, want the clamped value persisted back, not the original 6h one", raw.BackoffNextAttempt)
	}
}

// A non-excessive deadline is not reported as clamped — only the actual
// correction case should trigger a caller's self-heal write.
func TestAccountCacheDoesNotReportClampWhenDeadlineIsFine(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	saveAccountCache("avisoma", accountCacheEntry{
		Account:            "andy@avisoma.com",
		BackoffStreak:      2,
		BackoffNextAttempt: time.Now().Add(4 * time.Minute),
	})
	if _, clamped := loadAccountCache("avisoma"); clamped {
		t.Fatal("an ordinary deadline within the ceiling was reported as clamped")
	}
}

func TestEntryIdentityMatches(t *testing.T) {
	cases := []struct {
		name  string
		entry accountCacheEntry
		email string
		want  bool
	}{
		{"matching email, case-insensitive", accountCacheEntry{Account: "Andy@Trecs.Aero"}, "andy@trecs.aero", true},
		{"mismatched account", accountCacheEntry{Account: "andy@trecs.aero"}, "andy@avisoma.com", false},
		{"entry has no claimed account", accountCacheEntry{}, "andy@trecs.aero", false},
		{"nothing to compare against", accountCacheEntry{Account: "andy@trecs.aero"}, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := entryIdentityMatches(c.entry, c.email); got != c.want {
				t.Errorf("entryIdentityMatches(%+v, %q) = %v, want %v", c.entry, c.email, got, c.want)
			}
		})
	}
}

func TestAccountCacheRejectsCorruptFile(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	if err := os.WriteFile(accountCachePath("trecs"), []byte("not json"), 0600); err != nil {
		t.Fatal(err)
	}
	if got, _ := loadAccountCache("trecs"); !got.empty() {
		t.Fatalf("corrupt cache = %+v, want empty", got)
	}
}

func TestResolveActiveSnapshotName(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeSnapshotFixture(t, home, "avisoma", "tok-a", "andy@avisoma.com")
	writeSnapshotFixture(t, home, "trecs", "tok-t", "andy@trecs.aero")

	name, err := resolveActiveSnapshotName("andy@trecs.aero")
	if err != nil || name != "trecs" {
		t.Fatalf("resolveActiveSnapshotName(trecs email) = (%q, %v), want trecs", name, err)
	}
	name, err = resolveActiveSnapshotName("andy@avisoma.com")
	if err != nil || name != "avisoma" {
		t.Fatalf("resolveActiveSnapshotName(avisoma email) = (%q, %v), want avisoma", name, err)
	}
	name, err = resolveActiveSnapshotName("andy@nobody.dev")
	if err != nil || name != "" {
		t.Fatalf("resolveActiveSnapshotName(unknown email) = (%q, %v), want empty/no error", name, err)
	}
	name, err = resolveActiveSnapshotName("")
	if err != nil || name != "" {
		t.Fatalf("resolveActiveSnapshotName(empty email) = (%q, %v), want empty/no error", name, err)
	}
}
