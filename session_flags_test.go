package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fixedClock is defined in state_test.go (same package) — reused here.

// noResolver stands in for a host whose liveness answer is unavailable, which
// is what most of these tests want: pruning off, so a fabricated session id
// survives a write.
func noResolver() (map[string]bool, bool) { return nil, false }

// resolverFor returns a resolver reporting exactly ids as live-or-resumable.
func resolverFor(ids ...string) func() (map[string]bool, bool) {
	set := map[string]bool{}
	for _, id := range ids {
		set[id] = true
	}
	return func() (map[string]bool, bool) { return set, true }
}

func readFlagsFile(t *testing.T, path string) map[string]sessionFlags {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var entries map[string]sessionFlags
	if err := json.Unmarshal(data, &entries); err != nil {
		t.Fatalf("%s is not valid JSON: %v", path, err)
	}
	return entries
}

func TestFlagsStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, flagsFileName)
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)

	s := newFlagsStore(path, fixedClock(now), noResolver)
	s.SetDisabled("alpha", true)
	s.SetGroup("alpha", 3)
	s.SetGroup("beta", 7)

	reloaded := newFlagsStore(path, fixedClock(now), noResolver)
	if !reloaded.Disabled("alpha") {
		t.Fatal("alpha not disabled after reload")
	}
	if got := reloaded.Group("alpha"); got != 3 {
		t.Fatalf("alpha group = %d, want 3", got)
	}
	if got := reloaded.Group("beta"); got != 7 {
		t.Fatalf("beta group = %d, want 7", got)
	}
	if reloaded.Disabled("beta") {
		t.Fatal("beta reported disabled with no disabled bit")
	}
	if got := reloaded.Group("gamma"); got != 0 {
		t.Fatalf("gamma group = %d, want 0 with no entry", got)
	}
}

// TestFlagsStoreSetFlagsAbsentMeansUnchanged pins the distinction the wire
// contract rests on: a nil field leaves that flag exactly as it was, while an
// explicit zero clears the group.
func TestFlagsStoreSetFlagsAbsentMeansUnchanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, flagsFileName)
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)

	s := newFlagsStore(path, fixedClock(now), noResolver)
	_, _ = s.SetFlags("alpha", intPtr(4), boolPtr(true))

	// Only disabled named: the group must survive.
	if got, _ := s.SetFlags("alpha", nil, boolPtr(false)); got.Group != 4 || got.Disabled {
		t.Fatalf("after disabled=false: %#v, want group 4 and enabled", got)
	}
	// Only group named: the disabled bit must survive.
	_, _ = s.SetFlags("alpha", nil, boolPtr(true))
	if got, _ := s.SetFlags("alpha", intPtr(9), nil); got.Group != 9 || !got.Disabled {
		t.Fatalf("after group=9: %#v, want group 9 and still disabled", got)
	}
	// Explicit zero clears the group and leaves disabled alone.
	if got, _ := s.SetFlags("alpha", intPtr(0), nil); got.Group != 0 || !got.Disabled {
		t.Fatalf("after group=0: %#v, want ungrouped and still disabled", got)
	}
}

func TestFlagsStoreEntryWithNoStateIsDeleted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, flagsFileName)
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)

	s := newFlagsStore(path, fixedClock(now), noResolver)
	_, _ = s.SetFlags("alpha", intPtr(2), boolPtr(true))
	_, _ = s.SetFlags("alpha", intPtr(0), boolPtr(false))

	if entries := readFlagsFile(t, path); len(entries) != 0 {
		t.Fatalf("entry survived losing both flags: %#v", entries)
	}
}

func TestFlagsStoreOverlaySetsFlagsAndTouchesStaleEntries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, flagsFileName)
	t0 := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)

	s := newFlagsStore(path, fixedClock(t0), noResolver)
	_, _ = s.SetFlags("alpha", intPtr(5), boolPtr(true))

	sessions := []Session{{SessionID: "alpha"}, {SessionID: "beta"}, {SessionID: ""}}
	// A later store instance stands in for a different process (or a later
	// render tick) observing the same file after the touch interval elapses.
	later := t0.Add(2 * time.Hour)
	s2 := newFlagsStore(path, fixedClock(later), noResolver)
	s2.Overlay(sessions)

	if !sessions[0].Disabled || sessions[0].Group != 5 {
		t.Fatalf("alpha overlaid as %#v, want disabled and group 5", sessions[0])
	}
	if sessions[1].Disabled || sessions[1].Group != 0 {
		t.Fatalf("beta (no entry) overlaid as %#v, want zero flags", sessions[1])
	}
	if sessions[2].Disabled || sessions[2].Group != 0 {
		t.Fatalf("blank SessionID overlaid as %#v, want zero flags", sessions[2])
	}

	want := later.UTC().Format(time.RFC3339)
	if got := readFlagsFile(t, path)["alpha"].LastSeen; got != want {
		t.Fatalf("alpha last_seen = %q, want touched to %q", got, want)
	}
}

func TestFlagsStoreGCDropsStaleEntries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, flagsFileName)
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	old := now.Add(-40 * 24 * time.Hour).UTC().Format(time.RFC3339)

	seed := map[string]sessionFlags{"stale": {Disabled: true, LastSeen: old}}
	data, _ := json.MarshalIndent(seed, "", "  ")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	s := newFlagsStore(path, fixedClock(now), noResolver)
	// Any mutation runs the read-modify-write cycle that GCs the file.
	s.SetDisabled("fresh", true)

	if s.Disabled("stale") {
		t.Fatal("stale entry survived GC")
	}
	if !s.Disabled("fresh") {
		t.Fatal("fresh entry missing")
	}
}

// TestFlagsStorePrunesUnresolvableSessions covers the store's other GC rule:
// an entry whose session id no longer resolves to anything live or resumable
// on this host goes on the next write, however recently it was touched.
func TestFlagsStorePrunesUnresolvableSessions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, flagsFileName)
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)

	s := newFlagsStore(path, fixedClock(now), noResolver)
	s.SetGroup("gone", 2)
	s.SetGroup("alive", 3)

	pruning := newFlagsStore(path, fixedClock(now), resolverFor("alive", "other"))
	pruning.SetGroup("other", 4)

	entries := readFlagsFile(t, path)
	if _, ok := entries["gone"]; ok {
		t.Fatalf("unresolvable entry survived a write: %#v", entries)
	}
	if entries["alive"].Group != 3 || entries["other"].Group != 4 {
		t.Fatalf("resolvable entries did not survive: %#v", entries)
	}
}

// TestFlagsStoreDoesNotPruneWhenLivenessIsUnknown is the safety half of the
// rule above: a resolver that cannot answer (no home directory, unreadable
// transcript dir), or that answers with nothing at all, must not be read as
// "no session exists" — that would wipe every badge on the host.
func TestFlagsStoreDoesNotPruneWhenLivenessIsUnknown(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		resolver func() (map[string]bool, bool)
	}{
		{"resolver failed", noResolver},
		{"resolver returned nothing", func() (map[string]bool, bool) { return map[string]bool{}, true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, flagsFileName)
			seed := newFlagsStore(path, fixedClock(now), noResolver)
			seed.SetGroup("kept", 1)

			s := newFlagsStore(path, fixedClock(now), tc.resolver)
			s.SetGroup("added", 2)

			if got := readFlagsFile(t, path)["kept"].Group; got != 1 {
				t.Fatalf("kept group = %d, want 1 (nothing should have been pruned)", got)
			}
		})
	}
}

func TestFlagsStoreConcurrentGoroutinesSameInstance(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, flagsFileName)
	s := newFlagsStore(path, fixedClock(time.Now()), noResolver)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("session-%d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.SetDisabled(id, true)
		}()
	}
	wg.Wait()

	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("session-%d", i)
		if !s.Disabled(id) {
			t.Fatalf("%s not disabled after concurrent writes", id)
		}
	}
}

// TestFlagsStoreCrossInstanceSerialization proves the file lock — not just the
// in-process mutex — is what prevents lost writes: two separate FlagsStore
// instances sharing one path (standing in for the local TUI process and the
// local server process on the same host) write different session ids
// concurrently, and both survive. A whole-file-in-memory-then-save approach
// would have lost whichever instance saved second.
func TestFlagsStoreCrossInstanceSerialization(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, flagsFileName)
	a := newFlagsStore(path, fixedClock(time.Now()), noResolver)
	b := newFlagsStore(path, fixedClock(time.Now()), noResolver)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); a.SetGroup("from-a", 1) }()
	go func() { defer wg.Done(); b.SetDisabled("from-b", true) }()
	wg.Wait()

	verify := newFlagsStore(path, fixedClock(time.Now()), noResolver)
	if verify.Group("from-a") != 1 {
		t.Fatal("from-a lost to concurrent cross-instance write")
	}
	if !verify.Disabled("from-b") {
		t.Fatal("from-b lost to concurrent cross-instance write")
	}
}

// TestFlagsStoreWarmCacheReloadsAfterExternalWrite proves the mtime-based
// reload path, not just the cold-load path every other cross-instance test
// exercises: instance A reads once (warming its cache with a real, non-zero
// loadedModTime), instance B writes, and A's NEXT call must see the change —
// this is the core promise ("visible to every client polling that host") under
// repeated polling, not just on first load.
//
// A's first read has to observe an *existing* file for this to warm a real
// mtime: reloadIfStaleLocked early-returns on os.Stat's ENOENT for a
// not-yet-created file, which would leave loadedModTime at its zero value and
// silently collapse this into the same cold-load path every other
// cross-instance test already covers. So B seeds the file first, and the
// warm.IsZero() assertion below is load-bearing — it's what stops this test
// from regressing back to the cold path unnoticed.
func TestFlagsStoreWarmCacheReloadsAfterExternalWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, flagsFileName)
	b := newFlagsStore(path, fixedClock(time.Now()), noResolver)

	// Seed the file so A's first read warms its cache against a real mtime.
	b.SetDisabled("seed", true)

	a := newFlagsStore(path, fixedClock(time.Now()), noResolver)
	if a.Group("alpha") != 0 {
		t.Fatal("alpha unexpectedly grouped before any write")
	}
	a.mu.Lock()
	warm := a.loadedModTime
	a.mu.Unlock()
	if warm.IsZero() {
		t.Fatal("A cached a zero mtime — this test would only exercise the cold-load path")
	}

	b.SetGroup("alpha", 6)

	if a.Group("alpha") != 6 {
		t.Fatal("A's warm cache did not reload after B's external write")
	}
}

func TestFlagsStoreEmptyPathDisablesPersistence(t *testing.T) {
	s := newFlagsStore("", fixedClock(time.Now()), noResolver)
	s.SetDisabled("x", true) // must not panic
	s.SetGroup("x", 4)
	if s.Disabled("x") || s.Group("x") != 0 {
		t.Fatal("empty-path store reported flags without persistence")
	}
}

// TestFlagsStoreCorruptFileIsLeftUntouched is the discipline the two stores
// this replaces did NOT have: a file that fails to parse is never overwritten.
// Both halves matter — the bytes survive for a human to repair, and the
// mutation that would have wiped them is dropped rather than silently
// "succeeding".
func TestFlagsStoreCorruptFileIsLeftUntouched(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, flagsFileName)
	corrupt := []byte(`{"alpha": {"group": 3`)
	if err := os.WriteFile(path, corrupt, 0o644); err != nil {
		t.Fatal(err)
	}

	s := newFlagsStore(path, fixedClock(time.Now()), noResolver)
	if s.Group("alpha") != 0 || s.Disabled("alpha") {
		t.Fatal("corrupt file reported flags")
	}
	s.SetGroup("alpha", 1)

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(corrupt) {
		t.Fatalf("corrupt file was rewritten:\n%s", got)
	}
}

// TestFlagsStoreCorruptedUnderneathRefusesWrite covers the case load-time
// detection cannot: the file parsed fine when this instance loaded it, and
// another process corrupted it afterwards. The write must still be dropped,
// because mutateLocked re-reads under the lock.
func TestFlagsStoreCorruptedUnderneathRefusesWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, flagsFileName)
	s := newFlagsStore(path, fixedClock(time.Now()), noResolver)
	s.SetGroup("alpha", 2)

	corrupt := []byte("not json at all")
	if err := os.WriteFile(path, corrupt, 0o644); err != nil {
		t.Fatal(err)
	}

	s.SetGroup("beta", 3)

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(corrupt) {
		t.Fatalf("file corrupted underneath was rewritten:\n%s", got)
	}
}

func TestFlagsStoreEmptyFileIsNotCorrupt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, flagsFileName)
	if err := os.WriteFile(path, []byte("\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := newFlagsStore(path, fixedClock(time.Now()), noResolver)
	s.SetGroup("alpha", 8)

	if got := readFlagsFile(t, path)["alpha"].Group; got != 8 {
		t.Fatalf("alpha group = %d after writing to an empty file, want 8", got)
	}
}

// failingFlagsWriter stands in for the locked file on a disk that refuses the
// write (ENOSPC, EIO): no bytes land, the error comes straight back. Truncate
// still goes to the real file, so a truncate-then-write order really does
// empty it — which is exactly what these tests are here to catch.
type failingFlagsWriter struct{ f *os.File }

func (w *failingFlagsWriter) WriteAt(b []byte, off int64) (int, error) {
	return 0, fmt.Errorf("no space left on device")
}

func (w *failingFlagsWriter) Truncate(size int64) error { return w.f.Truncate(size) }

// truncateFailingFlagsWriter lets the rewrite land but fails the shortening
// step afterwards, leaving the tail of the older, longer file past the new
// data — the state a write-then-truncate order can leave behind.
type truncateFailingFlagsWriter struct{ f *os.File }

func (w *truncateFailingFlagsWriter) WriteAt(b []byte, off int64) (int, error) {
	return w.f.WriteAt(b, off)
}

func (w *truncateFailingFlagsWriter) Truncate(size int64) error {
	return fmt.Errorf("input/output error")
}

// TestWriteFlagsFileFailedWriteIsNotAnEmptyStore pins the write order. A write
// that fails outright (ENOSPC, EIO) must not leave a 0-byte file behind:
// decodeFlagsFile reads 0 bytes as a legitimately empty store, so the
// read-only latch would never engage and the next write would cement the loss
// of every badge and disabled mark on the host. The old bytes must still be
// there, and still be what the store reports.
func TestWriteFlagsFileFailedWriteIsNotAnEmptyStore(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), flagsFileName)
	seed := newFlagsStore(path, fixedClock(now), noResolver)
	seed.SetGroup("alpha", 3)
	seed.SetDisabled("alpha", true)

	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	ok := writeFlagsFile(&failingFlagsWriter{f: f}, map[string]sessionFlags{"beta": {Group: 5}})
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("writeFlagsFile reported success on a write that failed")
	}

	check, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	entries, decoded := decodeFlagsFile(check)
	if decoded && len(entries) == 0 {
		t.Fatal("a failed write left a file that reads as an empty-but-valid store — every badge on the host is silently gone")
	}
	if !decoded {
		t.Fatal("a failed write left an unreadable file; the untouched old entries should still parse")
	}
	if got := entries["alpha"]; got.Group != 3 || !got.Disabled {
		t.Fatalf("alpha = %#v, want group 3 and disabled — the old entries must survive a failed write", got)
	}
	if got := newFlagsStore(path, fixedClock(now), noResolver).Group("alpha"); got != 3 {
		t.Fatalf("reloaded alpha group = %d, want 3", got)
	}
}

// TestWriteFlagsFileLeftoverTailReadsAsCorrupt is the other half of the same
// order: when the write lands but the file cannot be shortened to fit it, the
// tail of the older, longer content stays past the new data. That cannot
// parse — so the store latches read-only and refuses to write over it, rather
// than treating the wreckage as state.
func TestWriteFlagsFileLeftoverTailReadsAsCorrupt(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), flagsFileName)
	seed := newFlagsStore(path, fixedClock(now), noResolver)
	for _, id := range []string{"alpha", "beta", "gamma", "delta"} {
		seed.SetGroup(id, 4)
	}

	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	ok := writeFlagsFile(&truncateFailingFlagsWriter{f: f}, map[string]sessionFlags{"epsilon": {Group: 5}})
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("writeFlagsFile reported success when the file could not be shortened")
	}

	check, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	if _, decoded := decodeFlagsFile(check); decoded {
		t.Fatal("new data plus a leftover tail still parses — the corruption latch never engages")
	}

	s := newFlagsStore(path, fixedClock(now), noResolver)
	if s.SetGroup("alpha", 1) {
		t.Fatal("the store wrote over a file left half-rewritten by a failed write")
	}
}

func TestToggleGroup(t *testing.T) {
	if got := toggleGroup(0, 3); got != 3 {
		t.Fatalf("ungrouped + 3 = %d, want 3", got)
	}
	if got := toggleGroup(3, 3); got != 0 {
		t.Fatalf("group 3 + 3 = %d, want 0 (same digit ungroups)", got)
	}
	if got := toggleGroup(3, 5); got != 5 {
		t.Fatalf("group 3 + 5 = %d, want 5 (single membership)", got)
	}
}

// --- migration ------------------------------------------------------------

func writeLegacyState(t *testing.T, dir string, entries map[string]sessionState) {
	t.Helper()
	data, err := json.MarshalIndent(clientState{Sessions: entries}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeLegacyDisabled(t *testing.T, dir string, entries map[string]legacyDisabledEntry) {
	t.Helper()
	data, err := json.MarshalIndent(legacyDisabledFile{Sessions: entries}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "disabled.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestMigrateLegacyFlagsMergesBothStores(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	seen := now.Add(-time.Hour).UTC().Format(time.RFC3339)

	writeLegacyState(t, dir, map[string]sessionState{
		"grouped":     {Group: 4, LastSeen: seen},
		"both":        {Group: 7, LastSeen: seen},
		"no-group":    {Group: 0, LastSeen: seen},
		"no-lastseen": {Group: 2},
	})
	writeLegacyDisabled(t, dir, map[string]legacyDisabledEntry{
		"both":          {LastSeen: seen},
		"disabled-only": {LastSeen: seen},
	})

	migrateLegacyFlags(dir, fixedClock(now))

	entries := readFlagsFile(t, filepath.Join(dir, flagsFileName))
	if got := entries["grouped"]; got.Group != 4 || got.Disabled || got.LastSeen != seen {
		t.Fatalf("grouped = %#v, want group 4, enabled, last_seen carried over", got)
	}
	if got := entries["both"]; got.Group != 7 || !got.Disabled {
		t.Fatalf("both = %#v, want group 7 and disabled", got)
	}
	if got := entries["disabled-only"]; got.Group != 0 || !got.Disabled {
		t.Fatalf("disabled-only = %#v, want ungrouped and disabled", got)
	}
	if _, ok := entries["no-group"]; ok {
		t.Fatalf("an ungrouped, un-disabled legacy entry was migrated: %#v", entries)
	}
	if got := entries["no-lastseen"].LastSeen; got != now.UTC().Format(time.RFC3339) {
		t.Fatalf("no-lastseen last_seen = %q, want stamped with now", got)
	}
}

// TestMigrateLegacyFlagsKeepsOldFilesAsBak pins the rule that makes this
// migration recoverable: the two legacy files are renamed, never deleted, and
// their bytes are unchanged.
func TestMigrateLegacyFlagsKeepsOldFilesAsBak(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	writeLegacyState(t, dir, map[string]sessionState{"alpha": {Group: 1, LastSeen: now.UTC().Format(time.RFC3339)}})
	writeLegacyDisabled(t, dir, map[string]legacyDisabledEntry{"beta": {LastSeen: now.UTC().Format(time.RFC3339)}})
	stateBefore, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	disabledBefore, err := os.ReadFile(filepath.Join(dir, "disabled.json"))
	if err != nil {
		t.Fatal(err)
	}

	migrateLegacyFlags(dir, fixedClock(now))

	for _, p := range []string{"state.json", "disabled.json"} {
		if _, err := os.Stat(filepath.Join(dir, p)); !os.IsNotExist(err) {
			t.Fatalf("%s still in place after migration (err=%v)", p, err)
		}
	}
	if got, err := os.ReadFile(filepath.Join(dir, "state.json.bak")); err != nil || string(got) != string(stateBefore) {
		t.Fatalf("state.json.bak missing or altered (err=%v)", err)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "disabled.json.bak")); err != nil || string(got) != string(disabledBefore) {
		t.Fatalf("disabled.json.bak missing or altered (err=%v)", err)
	}
}

func TestMigrateLegacyFlagsSkipsWhenNewFileExists(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	existing := []byte(`{"already":{"group":9}}`)
	if err := os.WriteFile(filepath.Join(dir, flagsFileName), existing, 0o644); err != nil {
		t.Fatal(err)
	}
	writeLegacyState(t, dir, map[string]sessionState{"alpha": {Group: 1}})

	migrateLegacyFlags(dir, fixedClock(now))

	got, err := os.ReadFile(filepath.Join(dir, flagsFileName))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(existing) {
		t.Fatalf("existing store was overwritten by a second migration:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "state.json")); err != nil {
		t.Fatalf("state.json was renamed by a migration that should not have run: %v", err)
	}
}

func TestMigrateLegacyFlagsWithNoLegacyFilesWritesNothing(t *testing.T) {
	dir := t.TempDir()
	migrateLegacyFlags(dir, fixedClock(time.Now()))
	if _, err := os.Stat(filepath.Join(dir, flagsFileName)); !os.IsNotExist(err) {
		t.Fatalf("migration created a store with nothing to migrate (err=%v)", err)
	}
}

// TestMigrateLegacyFlagsCorruptLegacyFileIsKept: a legacy file that cannot be
// parsed contributes nothing, but the migration still completes for the other
// file and the corrupt bytes survive as .bak.
func TestMigrateLegacyFlagsCorruptLegacyFileIsKept(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	corrupt := []byte("{oops")
	if err := os.WriteFile(filepath.Join(dir, "disabled.json"), corrupt, 0o644); err != nil {
		t.Fatal(err)
	}
	writeLegacyState(t, dir, map[string]sessionState{"alpha": {Group: 5, LastSeen: now.UTC().Format(time.RFC3339)}})

	migrateLegacyFlags(dir, fixedClock(now))

	entries := readFlagsFile(t, filepath.Join(dir, flagsFileName))
	if got := entries["alpha"].Group; got != 5 {
		t.Fatalf("alpha group = %d, want 5 — the readable legacy file must still migrate", got)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "disabled.json.bak")); err != nil || string(got) != string(corrupt) {
		t.Fatalf("corrupt legacy file not preserved as .bak (err=%v)", err)
	}
}

// TestMigrateLegacyFlagsIsWhatTheStoreReads ties the two halves together: what
// the migration writes is exactly what a FlagsStore opened on that directory
// reports, which is what keeps the desktop's badges identical across the
// upgrade.
func TestMigrateLegacyFlagsIsWhatTheStoreReads(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	stamp := now.UTC().Format(time.RFC3339)
	writeLegacyState(t, dir, map[string]sessionState{"alpha": {Group: 6, LastSeen: stamp}})
	writeLegacyDisabled(t, dir, map[string]legacyDisabledEntry{"alpha": {LastSeen: stamp}})

	migrateLegacyFlags(dir, fixedClock(now))

	s := newFlagsStore(filepath.Join(dir, flagsFileName), fixedClock(now), noResolver)
	sessions := []Session{{SessionID: "alpha"}}
	s.Overlay(sessions)
	if sessions[0].Group != 6 || !sessions[0].Disabled {
		t.Fatalf("post-migration overlay = %#v, want group 6 and disabled", sessions[0])
	}
}

// intPtr/boolPtr build the optional-field pointers SetFlags takes, where nil
// means "leave this flag alone".
func intPtr(n int) *int    { return &n }
func boolPtr(b bool) *bool { return &b }

// TestMigratedFlagsRenderIdenticalFrames is the migration's real acceptance
// test: the desktop must look exactly as it did before the two stores merged.
//
// It renders the session list twice at the same width and view mode — once
// from the inputs the old code path produced (groups straight out of
// state.json, disabled from presence in disabled.json) and once from the
// migrated store (Overlay + groupsOfRows) — and compares the frames byte for
// byte, colour codes and badge slots included. Local rows only: a remote row's
// group now comes from its own host over the wire, which is the one deliberate
// behaviour change here and cannot be asserted against a client-local file.
func TestMigratedFlagsRenderIdenticalFrames(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	stamp := now.UTC().Format(time.RFC3339)

	legacyGroups := map[string]sessionState{
		"sess-a": {Group: 1, LastSeen: stamp},
		"sess-c": {Group: 9, LastSeen: stamp},
	}
	legacyDisabled := map[string]legacyDisabledEntry{
		"sess-b": {LastSeen: stamp},
		"sess-c": {LastSeen: stamp},
	}
	writeLegacyState(t, dir, legacyGroups)
	writeLegacyDisabled(t, dir, legacyDisabled)

	rows := func() []Session {
		return []Session{
			{PID: 1, SessionID: "sess-a", CWD: "/repo/one", Status: "idle", CPU: "1.0", StartedAt: 1},
			{PID: 2, SessionID: "sess-b", CWD: "/repo/two", Status: "busy", CPU: "2.0", StartedAt: 2},
			{PID: 3, SessionID: "sess-c", CWD: "/repo/three", Status: "idle", CPU: "3.0", StartedAt: 3},
			{PID: 4, SessionID: "sess-d", CWD: "/repo/four", Status: "idle", CPU: "4.0", StartedAt: 4},
		}
	}

	// The old path: a client-local group map, and disabled overlaid by
	// presence in disabled.json.
	before := rows()
	beforeGroups := map[string]int{}
	for id, e := range legacyGroups {
		beforeGroups[id] = e.Group
	}
	for i := range before {
		_, disabled := legacyDisabled[before[i].SessionID]
		before[i].Disabled = disabled
	}

	// The new path: one shared file, migrated, overlaid onto the rows.
	migrateLegacyFlags(dir, fixedClock(now))
	store := newFlagsStore(filepath.Join(dir, flagsFileName), fixedClock(now), noResolver)
	after := rows()
	store.Overlay(after)
	afterGroups := groupsOfRows(after, nil)

	for _, mode := range []string{"1", "2", "3"} {
		for _, filter := range []groupFilter{{}, {mode: filterOnly, mask: 1 << uint(1)}, {mode: filterHide, mask: 1 << uint(9)}} {
			for _, hideDisabled := range []bool{false, true} {
				SortSessions(before, "dir")
				SortSessions(after, "dir")
				want := BuildTableFrame(mode, testLocalHost(before...), nil, "1", nil, 120, 0, "dir",
					groupView{groups: beforeGroups, filter: filter, hideDisabled: hideDisabled})
				got := BuildTableFrame(mode, testLocalHost(after...), nil, "1", nil, 120, 0, "dir",
					groupView{groups: afterGroups, filter: filter, hideDisabled: hideDisabled})
				if strings.Join(got.lines, "\n") != strings.Join(want.lines, "\n") {
					t.Fatalf("view %s, filter %#v, hideDisabled %v: frame changed after migration\nbefore:\n%s\nafter:\n%s",
						mode, filter, hideDisabled, strings.Join(want.lines, "\n"), strings.Join(got.lines, "\n"))
				}
			}
		}
	}
}

// TestFlagsStoreReportsWhetherAWriteLanded is what lets an HTTP handler answer
// honestly: a store with nowhere to write, and one refusing to touch a corrupt
// file, both say so instead of returning silently.
func TestFlagsStoreReportsWhetherAWriteLanded(t *testing.T) {
	if ok := newFlagsStore("", fixedClock(time.Now()), noResolver).SetGroup("a", 1); ok {
		t.Fatal("a store with no path reported a successful write")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, flagsFileName)
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ok := newFlagsStore(path, fixedClock(time.Now()), noResolver).SetDisabled("a", true); ok {
		t.Fatal("a store holding a corrupt file reported a successful write")
	}

	good := newFlagsStore(filepath.Join(t.TempDir(), flagsFileName), fixedClock(time.Now()), noResolver)
	if ok := good.SetGroup("a", 1); !ok {
		t.Fatal("a healthy store reported a failed write")
	}
}

// TestMigrateLegacyFlagsReturnsAOneLineNotice: the notice is handed back
// instead of printed, because the TUI shows it as its opening toast — one
// bottom row on the alternate screen, where a stderr line would just be
// painted over by the first repaint. So it must be a single line, and the
// thing the user has to do has to survive an 80-column clip.
func TestMigrateLegacyFlagsReturnsAOneLineNotice(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	writeLegacyState(t, dir, map[string]sessionState{"alpha": {Group: 3}})

	notice := migrateLegacyFlags(dir, fixedClock(now))
	if notice == "" {
		t.Fatal("a migration that ran said nothing")
	}
	if strings.Contains(notice, "\n") {
		t.Fatalf("notice is not a single line: %q", notice)
	}
	head := []rune(notice)
	if len(head) > 80 {
		head = head[:80]
	}
	if !strings.Contains(string(head), "set them again") {
		t.Fatalf("the ask does not survive an 80-column clip: %q", string(head))
	}

	if again := migrateLegacyFlags(dir, fixedClock(now)); again != "" {
		t.Fatalf("a second run said %q, want silence — nothing was migrated", again)
	}
}

// TestMigrateLegacyFlagsRetriesAfterAZeroByteStore: a zero-byte
// session-flags.json is what a migration whose write failed leaves behind. It
// must not be mistaken for "already migrated", or state.json would sit unread
// and un-renamed forever.
func TestMigrateLegacyFlagsRetriesAfterAZeroByteStore(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	if err := os.WriteFile(filepath.Join(dir, flagsFileName), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	writeLegacyState(t, dir, map[string]sessionState{"alpha": {Group: 3, LastSeen: now.UTC().Format(time.RFC3339)}})

	migrateLegacyFlags(dir, fixedClock(now))

	if got := readFlagsFile(t, filepath.Join(dir, flagsFileName))["alpha"].Group; got != 3 {
		t.Fatalf("alpha group = %d, want 3 — the retry must have migrated", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "state.json.bak")); err != nil {
		t.Fatalf("state.json not renamed on the retry: %v", err)
	}
}
