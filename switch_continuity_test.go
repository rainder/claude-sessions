package main

import (
	"testing"
	"time"
)

// TestAccountSwitchPreservesContinuityBothDirections is the actual feature
// account_cache.go exists for: switching which account is live must not lose
// either account's numbers or backoff wait. Before this file, the live
// account and every known account were cached in separate files split by
// ROLE (live vs known), so switching lost continuity in both directions —
// the incoming account started its numbers over on the live side, and the
// outgoing account lost its backoff streak the moment it reappeared on the
// known side. Now both newUsageFetcher and newKnownAccountsFetcher resolve
// straight to the same per-account file regardless of which side is asking,
// so this proves the round trip: trecs (live) and avisoma (known) both carry
// their own state across a switch, in both directions, at once.
//
// Both accounts start with an armed (not-due) backoff wait specifically so
// every check in this test — before and after the switch, on both sides —
// exercises the carry-forward path deterministically, with no real or stubbed
// network fetch anywhere: a cached entry with no active wait would legitimately
// attempt a fresh fetch (only an armed wait holds it back), which would make
// this test's outcome depend on a fetch stub instead of on the continuity
// this file exists to prove.
func TestAccountSwitchPreservesContinuityBothDirections(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TMPDIR", t.TempDir())
	writeSnapshotFixture(t, home, "trecs", "tok-trecs", "andy@trecs.aero")
	writeSnapshotFixture(t, home, "avisoma", "tok-avisoma", "andy@avisoma.com")
	writeLiveAccount(t, home, "andy@trecs.aero")

	// trecs (live) has stale carried numbers and an armed wait.
	trecsFetchedAt := time.Now().Add(-10 * time.Minute)
	saveAccountCache("trecs", accountCacheEntry{
		Account:            "andy@trecs.aero",
		Info:               &UsageInfo{FiveHour: usageBucket{Pct: 63}},
		FetchedAt:          trecsFetchedAt,
		BackoffStreak:      1,
		BackoffNextAttempt: time.Now().Add(5 * time.Minute),
	})
	// avisoma (known) is mid-backoff with stale carried numbers — exactly the
	// state a chronically throttled known account sits in.
	avisomaFetchedAt := time.Now().Add(-20 * time.Minute)
	saveAccountCache("avisoma", accountCacheEntry{
		Account:            "andy@avisoma.com",
		Info:               &UsageInfo{FiveHour: usageBucket{Pct: 88}},
		FetchedAt:          avisomaFetchedAt,
		BackoffStreak:      2,
		BackoffNextAttempt: time.Now().Add(3 * time.Minute),
	})

	usageFetcher := newUsageFetcher(func() (*AccountUsage, error) {
		t.Fatal("live fetcher must not hit the network — both accounts have an armed wait throughout this test")
		return nil, nil
	}, noSaveAccountCache)
	knownFetcher := newKnownAccountsFetcher(noSaveAccountCache)

	// Before the switch: the live fetcher carries trecs, the known fetcher
	// carries avisoma (and only avisoma — trecs is live, excluded from the
	// known list).
	before, err := usageFetcher()
	if err != nil {
		t.Fatalf("live fetch before switch: %v", err)
	}
	if before.Info == nil || before.Info.FiveHour.Pct != 63 {
		t.Fatalf("live before switch = %#v, want trecs's carried 63%%", before)
	}
	knownBefore, err := knownFetcher()
	if err != nil {
		t.Fatalf("known fetch before switch: %v", err)
	}
	if len(knownBefore.Accounts) != 1 || knownBefore.Accounts[0].Name != "avisoma" || knownBefore.Accounts[0].Info == nil || knownBefore.Accounts[0].Info.FiveHour.Pct != 88 {
		t.Fatalf("known before switch = %#v, want just avisoma's carried 88%%", knownBefore.Accounts)
	}

	// The switch itself: nothing but ~/.claude.json changes. Neither fetcher's
	// own account_cache.go files are touched by a switch (see account.go —
	// switchAccount never touches them), which is exactly what makes this a
	// non-event for both.
	writeLiveAccount(t, home, "andy@avisoma.com")

	// avisoma is now live: the live fetcher must resolve straight to its
	// existing slot and honor the wait already armed there — no fetch, no
	// starting over, and the fixture's t.Fatal above proves no network call
	// was attempted.
	after, err := usageFetcher()
	if err != nil {
		t.Fatalf("live fetch after switch: %v", err)
	}
	if after.Info == nil || after.Info.FiveHour.Pct != 88 {
		t.Fatalf("live after switch = %#v, want avisoma's carried 88%%", after)
	}
	if !after.Stale {
		t.Error("avisoma's carried numbers must read Stale once live — they were never re-fetched")
	}
	if !after.FetchedAt.Equal(avisomaFetchedAt) {
		t.Errorf("live after switch FetchedAt = %v, want avisoma's original %v preserved", after.FetchedAt, avisomaFetchedAt)
	}

	// trecs is now demoted to known: it must show up there with its own prior
	// numbers AND its own armed wait intact, not starting over as a brand new
	// account.
	knownAfter, err := knownFetcher()
	if err != nil {
		t.Fatalf("known fetch after switch: %v", err)
	}
	if len(knownAfter.Accounts) != 1 || knownAfter.Accounts[0].Name != "trecs" {
		t.Fatalf("known after switch = %#v, want just trecs", knownAfter.Accounts)
	}
	trecs := knownAfter.Accounts[0]
	if trecs.Info == nil || trecs.Info.FiveHour.Pct != 63 {
		t.Fatalf("demoted trecs = %#v, want its prior 63%% preserved, not started over", trecs)
	}
	if !trecs.FetchedAt.Equal(trecsFetchedAt) {
		t.Errorf("demoted trecs FetchedAt = %v, want its original %v preserved", trecs.FetchedAt, trecsFetchedAt)
	}
}

// TestAccountSwitchRoundTripsThroughRealDisk is
// TestAccountSwitchPreservesContinuityBothDirections' sibling: that test
// pre-seeds both accounts' files by hand and passes noSaveAccountCache to
// both fetchers, so it proves the LOAD side of continuity but never proves
// that what one fetcher actually WRITES is what the other fetcher then
// READS. This test wires both fetchers to the real saveAccountCache and
// drives an actual write: the live fetcher fetches successfully as trecs
// (writing trecs's own slot), the account switches, and the known-accounts
// fetcher must read that just-written slot back — the literal round trip
// the whole per-account-file design exists for.
func TestAccountSwitchRoundTripsThroughRealDisk(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TMPDIR", t.TempDir())
	writeSnapshotFixture(t, home, "trecs", "tok-trecs", "andy@trecs.aero")
	writeSnapshotFixture(t, home, "avisoma", "tok-avisoma", "andy@avisoma.com")
	writeLiveAccount(t, home, "andy@trecs.aero")

	usageFetcher := newUsageFetcher(func() (*AccountUsage, error) {
		return &AccountUsage{
			Account:   "andy@trecs.aero",
			Info:      &UsageInfo{FiveHour: usageBucket{Pct: 77}, VerifiedAccount: "andy@trecs.aero"},
			FetchedAt: time.Now(),
		}, nil
	}, saveAccountCache)

	if _, err := usageFetcher(); err != nil {
		t.Fatalf("live fetch as trecs: %v", err)
	}

	// trecs's slot must now exist on disk with what the fetch produced —
	// not via a hand-authored saveAccountCache call, but via the fetcher's
	// own persist path.
	onDisk, _ := loadAccountCache("trecs")
	if onDisk.Info == nil || onDisk.Info.FiveHour.Pct != 77 {
		t.Fatalf("trecs on disk after live fetch = %#v, want the fetch's own 77%% written by the fetcher itself", onDisk)
	}

	// Arm a wait on top of the fetcher's own write so the known-side pass
	// below reads it back rather than attempting a real network fetch
	// (newKnownAccountsFetcher has no injectable fetch function — it calls
	// the real endpoint unless a cached wait holds it back).
	onDisk.BackoffStreak, onDisk.BackoffNextAttempt = 2, time.Now().Add(5*time.Minute)
	saveAccountCache("trecs", onDisk)

	writeLiveAccount(t, home, "andy@avisoma.com")

	knownFetcher := newKnownAccountsFetcher(saveAccountCache)
	known, err := knownFetcher()
	if err != nil {
		t.Fatalf("known fetch after switch: %v", err)
	}
	if len(known.Accounts) != 1 || known.Accounts[0].Name != "trecs" {
		t.Fatalf("known after switch = %#v, want just trecs", known.Accounts)
	}
	trecs := known.Accounts[0]
	if trecs.Info == nil || trecs.Info.FiveHour.Pct != 77 {
		t.Fatalf("known-side trecs = %#v, want the 77%% the live fetcher itself wrote to disk", trecs)
	}
	if !trecs.Verified {
		t.Errorf("known-side trecs.Verified = false, want the live fetch's verified numbers to round-trip through disk")
	}
}
