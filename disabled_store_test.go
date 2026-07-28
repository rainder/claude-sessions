package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fixedClock is defined in state_test.go (same package) — reused here.

func TestDisabledStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "disabled.json")
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)

	d := newDisabledStore(path, fixedClock(now))
	d.SetDisabled("alpha", true)

	reloaded := newDisabledStore(path, fixedClock(now))
	if !reloaded.Disabled("alpha") {
		t.Fatal("alpha not disabled after reload")
	}
	if reloaded.Disabled("beta") {
		t.Fatal("beta reported disabled with no entry")
	}
}

func TestDisabledStoreSetFalseRemovesEntry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "disabled.json")
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)

	d := newDisabledStore(path, fixedClock(now))
	d.SetDisabled("alpha", true)
	d.SetDisabled("alpha", false)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var df disabledFile
	if err := json.Unmarshal(data, &df); err != nil {
		t.Fatalf("disabled.json not valid JSON: %v", err)
	}
	if _, ok := df.Sessions["alpha"]; ok {
		t.Fatalf("alpha entry survived re-enable: %#v", df.Sessions)
	}
}

func TestDisabledStoreOverlaySetsFlagAndTouchesStaleEntries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "disabled.json")
	t0 := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)

	d := newDisabledStore(path, fixedClock(t0))
	d.SetDisabled("alpha", true)

	sessions := []Session{{SessionID: "alpha"}, {SessionID: "beta"}, {SessionID: ""}}
	// A later store instance stands in for a different process (or a later
	// render tick) observing the same file after the touch interval elapses.
	later := t0.Add(2 * time.Hour)
	d2 := newDisabledStore(path, fixedClock(later))
	d2.Overlay(sessions)

	if !sessions[0].Disabled {
		t.Fatal("alpha not overlaid as disabled")
	}
	if sessions[1].Disabled {
		t.Fatal("beta (no entry) overlaid as disabled")
	}
	if sessions[2].Disabled {
		t.Fatal("blank SessionID overlaid as disabled")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var df disabledFile
	if err := json.Unmarshal(data, &df); err != nil {
		t.Fatal(err)
	}
	want := later.UTC().Format(time.RFC3339)
	if got := df.Sessions["alpha"].LastSeen; got != want {
		t.Fatalf("alpha last_seen = %q, want touched to %q", got, want)
	}
}

func TestDisabledStoreGCDropsStaleEntries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "disabled.json")
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	old := now.Add(-40 * 24 * time.Hour).UTC().Format(time.RFC3339)

	seed := disabledFile{Sessions: map[string]disabledEntry{"stale": {LastSeen: old}}}
	data, _ := json.MarshalIndent(seed, "", "  ")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	d := newDisabledStore(path, fixedClock(now))
	// Any mutation runs the read-modify-write cycle that GCs the file.
	d.SetDisabled("fresh", true)

	if d.Disabled("stale") {
		t.Fatal("stale entry survived GC")
	}
	if !d.Disabled("fresh") {
		t.Fatal("fresh entry missing")
	}
}

func TestDisabledStoreConcurrentGoroutinesSameInstance(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "disabled.json")
	d := newDisabledStore(path, fixedClock(time.Now()))

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("session-%d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.SetDisabled(id, true)
		}()
	}
	wg.Wait()

	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("session-%d", i)
		if !d.Disabled(id) {
			t.Fatalf("%s not disabled after concurrent writes", id)
		}
	}
}

// TestDisabledStoreCrossInstanceSerialization proves the file lock — not
// just the in-process mutex — is what prevents lost writes: two separate
// DisabledStore instances sharing one path (standing in for the local TUI
// process and the local server process on the same host) write different
// session ids concurrently, and both survive. Reusing SessionStore's
// whole-file-in-memory-then-save approach would have lost whichever instance
// saved second.
func TestDisabledStoreCrossInstanceSerialization(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "disabled.json")
	a := newDisabledStore(path, fixedClock(time.Now()))
	b := newDisabledStore(path, fixedClock(time.Now()))

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); a.SetDisabled("from-a", true) }()
	go func() { defer wg.Done(); b.SetDisabled("from-b", true) }()
	wg.Wait()

	verify := newDisabledStore(path, fixedClock(time.Now()))
	if !verify.Disabled("from-a") {
		t.Fatal("from-a lost to concurrent cross-instance write")
	}
	if !verify.Disabled("from-b") {
		t.Fatal("from-b lost to concurrent cross-instance write")
	}
}

// TestDisabledStoreWarmCacheReloadsAfterExternalWrite proves the mtime-based
// reload path, not just the cold-load path every other cross-instance test
// exercises: instance A reads once (warming its cache with a real, non-zero
// loadedModTime), instance B writes, and A's NEXT call must see the change —
// this is the core promise ("visible to every client polling that host")
// under repeated polling, not just on first load.
//
// A's first read has to observe an *existing* file for this to warm a real
// mtime: reloadIfStaleLocked early-returns on os.Stat's ENOENT for a
// not-yet-created file, which would leave loadedModTime at its zero value
// and silently collapse this into the same cold-load path every other
// cross-instance test already covers. So B seeds the file first, and the
// warm.IsZero() assertion below is load-bearing — it's what stops this test
// from regressing back to the cold path unnoticed.
func TestDisabledStoreWarmCacheReloadsAfterExternalWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "disabled.json")
	a := newDisabledStore(path, fixedClock(time.Now()))
	b := newDisabledStore(path, fixedClock(time.Now()))

	// Seed the file so A's first read warms its cache against a real mtime.
	b.SetDisabled("seed", true)

	if a.Disabled("alpha") {
		t.Fatal("alpha unexpectedly disabled before any write")
	}
	a.mu.Lock()
	warm := a.loadedModTime
	a.mu.Unlock()
	if warm.IsZero() {
		t.Fatal("A cached a zero mtime — this test would only exercise the cold-load path")
	}

	b.SetDisabled("alpha", true)

	if !a.Disabled("alpha") {
		t.Fatal("A's warm cache did not reload after B's external write")
	}
}

func TestDisabledStoreEmptyPathDisablesPersistence(t *testing.T) {
	d := newDisabledStore("", fixedClock(time.Now()))
	d.SetDisabled("x", true) // must not panic
	if d.Disabled("x") {
		t.Fatal("empty-path store reported disabled without persistence")
	}
}

func TestDisabledStoreCorruptFileStartsFresh(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "disabled.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	d := newDisabledStore(path, fixedClock(time.Now()))
	if d.Disabled("x") {
		t.Fatal("corrupt file reported a disabled session")
	}
	d.SetDisabled("x", true)
	if !d.Disabled("x") {
		t.Fatal("write after corrupt-file recovery did not stick")
	}
}
