package main

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// fastSpawnGroupPolling shrinks the wait to something a test can spend, and
// swaps in a collector. The tmux liveness check is stubbed to "still there",
// so a test that says nothing about it exercises the ordinary wait rather than
// the dead-session bail (and never shells out to a real tmux). Every test here
// goes through it, so nothing ever reaches the real CollectLocal or the real
// 15s timeout.
func fastSpawnGroupPolling(t *testing.T, collect func() ([]Session, error)) {
	t.Helper()
	origCollect, origInterval, origTimeout, origAlive :=
		spawnGroupCollect, spawnGroupPollInterval, spawnGroupTimeout, spawnGroupTmuxAlive
	spawnGroupCollect = collect
	spawnGroupPollInterval = time.Millisecond
	spawnGroupTimeout = 50 * time.Millisecond
	spawnGroupTmuxAlive = func(string) bool { return true }
	t.Cleanup(func() {
		spawnGroupCollect, spawnGroupPollInterval, spawnGroupTimeout, spawnGroupTmuxAlive =
			origCollect, origInterval, origTimeout, origAlive
	})
}

// tempFlagsStore is a real store over a temp file, with pruning off so a
// fabricated session id survives the write.
func tempFlagsStore(t *testing.T) *FlagsStore {
	t.Helper()
	return newFlagsStore(filepath.Join(t.TempDir(), flagsFileName), time.Now, noResolver)
}

func TestSetGroupAfterSpawnAssignsTheResolvedSession(t *testing.T) {
	calls := 0
	fastSpawnGroupPolling(t, func() ([]Session, error) {
		calls++
		if calls < 3 {
			// The first passes see the world before the new session wrote its
			// session file: another host's row, and one in a different tmux
			// session, neither of which may be adopted.
			return []Session{{SessionID: "elsewhere", Tmux: "other-abc123:0.0"}}, nil
		}
		return []Session{
			{SessionID: "elsewhere", Tmux: "other-abc123:0.0"},
			{SessionID: "fresh-one", Tmux: "proj-def456:0.0"},
		}, nil
	})
	store := tempFlagsStore(t)

	if warn := setGroupAfterSpawn(store, "proj-def456", 4); warn != "" {
		t.Fatalf("setGroupAfterSpawn warned %q, want none", warn)
	}
	if got := store.Group("fresh-one"); got != 4 {
		t.Errorf("group of fresh-one = %d, want 4", got)
	}
	if got := store.Group("elsewhere"); got != 0 {
		t.Errorf("group of elsewhere = %d, want 0 — only the spawned session may be touched", got)
	}
}

func TestSetGroupAfterSpawnWarnsWhenTheSessionNeverAppears(t *testing.T) {
	fastSpawnGroupPolling(t, func() ([]Session, error) {
		return []Session{{SessionID: "elsewhere", Tmux: "other-abc123:0.0"}}, nil
	})
	store := tempFlagsStore(t)

	warn := setGroupAfterSpawn(store, "proj-def456", 4)
	if warn != spawnGroupNotSeenWarning {
		t.Fatalf("setGroupAfterSpawn = %q, want %q", warn, spawnGroupNotSeenWarning)
	}
	if got := store.Group("elsewhere"); got != 0 {
		t.Errorf("group of elsewhere = %d, want 0 — a timed-out resolve must set nothing", got)
	}
}

// TestSetGroupAfterSpawnSurvivesACollectorFailure: ps or tmux hiccupping once
// is "not there yet", not a lost group.
func TestSetGroupAfterSpawnSurvivesACollectorFailure(t *testing.T) {
	calls := 0
	fastSpawnGroupPolling(t, func() ([]Session, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("read process tree: boom")
		}
		return []Session{{SessionID: "fresh-one", Tmux: "proj-def456:0.0"}}, nil
	})
	store := tempFlagsStore(t)

	if warn := setGroupAfterSpawn(store, "proj-def456", 7); warn != "" {
		t.Fatalf("setGroupAfterSpawn warned %q, want none", warn)
	}
	if got := store.Group("fresh-one"); got != 7 {
		t.Errorf("group of fresh-one = %d, want 7", got)
	}
}

func TestSetGroupAfterSpawnWarnsWhenTheStoreCannotSave(t *testing.T) {
	fastSpawnGroupPolling(t, func() ([]Session, error) {
		return []Session{{SessionID: "fresh-one", Tmux: "proj-def456:0.0"}}, nil
	})
	// Empty path is what a host with no config directory gets: reads report
	// zero flags and writes are no-ops.
	store := newFlagsStore("", time.Now, noResolver)

	warn := setGroupAfterSpawn(store, "proj-def456", 4)
	if warn != spawnGroupNotSavedWarning {
		t.Fatalf("setGroupAfterSpawn = %q, want %q", warn, spawnGroupNotSavedWarning)
	}
}

// TestSetGroupAfterSpawnWithNoStoreNeverPolls: there is nowhere to write the
// answer to, so waiting for the session would only delay the same warning.
func TestSetGroupAfterSpawnWithNoStoreNeverPolls(t *testing.T) {
	polled := false
	fastSpawnGroupPolling(t, func() ([]Session, error) {
		polled = true
		return nil, nil
	})

	if warn := setGroupAfterSpawn(nil, "proj-def456", 4); warn != spawnGroupNotSavedWarning {
		t.Fatalf("setGroupAfterSpawn = %q, want %q", warn, spawnGroupNotSavedWarning)
	}
	if polled {
		t.Error("collector was polled with no store to write to")
	}
}

// TestSetGroupAfterSpawnIgnoresGroupZero: 0 is how a caller says it asked for
// nothing, so nothing is collected and nothing is written.
func TestSetGroupAfterSpawnIgnoresGroupZero(t *testing.T) {
	polled := false
	fastSpawnGroupPolling(t, func() ([]Session, error) {
		polled = true
		return nil, nil
	})
	store := tempFlagsStore(t)

	if warn := setGroupAfterSpawn(store, "proj-def456", 0); warn != "" {
		t.Fatalf("setGroupAfterSpawn warned %q for group 0, want none", warn)
	}
	if polled {
		t.Error("collector was polled for group 0")
	}
}

// TestSetGroupAfterSpawnBailsWhenTheTmuxSessionIsGone: a command that dies on
// its first line leaves no session to wait for, so the wait ends at once
// instead of spending the whole timeout watching nothing.
func TestSetGroupAfterSpawnBailsWhenTheTmuxSessionIsGone(t *testing.T) {
	polls := 0
	fastSpawnGroupPolling(t, func() ([]Session, error) {
		polls++
		return nil, nil
	})
	spawnGroupTmuxAlive = func(tname string) bool {
		if tname != "proj-def456" {
			t.Errorf("liveness checked %q, want the spawned session", tname)
		}
		return false
	}
	// Long enough that spending it would be obvious; the bail is what keeps
	// this test fast.
	spawnGroupTimeout = time.Minute
	store := tempFlagsStore(t)

	start := time.Now()
	warn := setGroupAfterSpawn(store, "proj-def456", 4)
	if warn != spawnGroupNotSeenWarning {
		t.Fatalf("setGroupAfterSpawn = %q, want %q", warn, spawnGroupNotSeenWarning)
	}
	if polls != 1 {
		t.Errorf("collector polled %d times, want 1 — the bail is after the first look", polls)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("waited %v for a dead session", elapsed)
	}
}

// TestSetGroupAfterSpawnWithNoTmuxNameWarns: a spawn that reported success
// without naming its session cannot be resolved, and silence is the one answer
// this must never give once a group was asked for.
func TestSetGroupAfterSpawnWithNoTmuxNameWarns(t *testing.T) {
	polled := false
	fastSpawnGroupPolling(t, func() ([]Session, error) {
		polled = true
		return nil, nil
	})

	if warn := setGroupAfterSpawn(tempFlagsStore(t), "", 4); warn != spawnGroupNotSeenWarning {
		t.Fatalf("setGroupAfterSpawn = %q, want %q", warn, spawnGroupNotSeenWarning)
	}
	if polled {
		t.Error("collector was polled with no tmux name to match")
	}
}

func TestValidSpawnGroup(t *testing.T) {
	for _, n := range []int{1, 5, 9} {
		if !validSpawnGroup(n) {
			t.Errorf("validSpawnGroup(%d) = false, want true", n)
		}
	}
	for _, n := range []int{-1, 0, 10, 99} {
		if validSpawnGroup(n) {
			t.Errorf("validSpawnGroup(%d) = true, want false", n)
		}
	}
}
