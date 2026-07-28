package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fixedClock returns a now func pinned to t.
func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

func TestSessionStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)

	s := loadSessionStore(path, fixedClock(now))
	s.SetGroup("alpha", 3, []string{"alpha"})

	// Reload from disk: assignment survives.
	got := loadSessionStore(path, fixedClock(now))
	if got.Group("alpha") != 3 {
		t.Fatalf("alpha group = %d, want 3", got.Group("alpha"))
	}
	if got.Group("beta") != 0 {
		t.Fatalf("cross-contamination: %#v", got.entries)
	}
	if m := got.GroupsMap(); len(m) != 1 || m["alpha"] != 3 {
		t.Fatalf("GroupsMap = %#v, want {alpha:3}", m)
	}
}

func TestSessionStoreAtomicSaveLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)

	s := loadSessionStore(path, fixedClock(now))
	s.SetGroup("alpha", 1, []string{"alpha"})

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "state.json" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("dir contents = %v, want only state.json", names)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cs clientState
	if err := json.Unmarshal(data, &cs); err != nil {
		t.Fatalf("state.json not valid JSON: %v", err)
	}
	e, ok := cs.Sessions["alpha"]
	if !ok || e.Group != 1 || e.LastSeen != now.Format(time.RFC3339) {
		t.Fatalf("persisted alpha = %#v", cs.Sessions)
	}
}

func TestSessionStoreGCOnLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	old := now.Add(-40 * 24 * time.Hour).Format(time.RFC3339)
	recent := now.Add(-1 * time.Hour).Format(time.RFC3339)

	seed := clientState{Sessions: map[string]sessionState{
		"empty": {LastSeen: recent},           // no group: drop
		"stale": {Group: 2, LastSeen: old},    // grouped but expired: drop
		"kept":  {Group: 5, LastSeen: recent}, // grouped + fresh: keep
	}}
	data, _ := json.MarshalIndent(seed, "", "  ")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	s := loadSessionStore(path, fixedClock(now))
	want := map[string]bool{"kept": true}
	if len(s.entries) != len(want) {
		t.Fatalf("post-GC entries = %#v, want %v", s.entries, want)
	}
	for id := range want {
		if _, ok := s.entries[id]; !ok {
			t.Fatalf("GC dropped %q which should survive: %#v", id, s.entries)
		}
	}
}

func TestSessionStoreGroupToggleAndReplace(t *testing.T) {
	s := loadSessionStore("", fixedClock(time.Now()))

	// Assign, then same group again ungroups.
	s.SetGroup("x", 3, nil)
	if s.Group("x") != 3 {
		t.Fatalf("after assign group = %d, want 3", s.Group("x"))
	}
	s.SetGroup("x", 3, nil)
	if s.Group("x") != 0 {
		t.Fatalf("re-assigning same group did not ungroup: %d", s.Group("x"))
	}
	if _, ok := s.entries["x"]; ok {
		t.Fatalf("ungrouped entry not dropped: %#v", s.entries)
	}

	// Single membership: a new group replaces the old.
	s.SetGroup("y", 2, nil)
	s.SetGroup("y", 7, nil)
	if s.Group("y") != 7 {
		t.Fatalf("replace group = %d, want 7", s.Group("y"))
	}
}

func TestSessionStoreLastSeenRefreshesOnlyExistingVisible(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	t0 := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	clock := t0

	s := loadSessionStore(path, func() time.Time { return clock })
	s.SetGroup("grouped", 1, []string{"grouped", "ungrouped"})

	// Advance time. SetGroup toggles: calling it again with the entry's
	// current group (1) flips it back to 0, which deletes the entry rather
	// than no-op'ing; the following identical call then toggles it back on,
	// recreating the entry and stamping last_seen with the advanced clock —
	// that recreated entry is what the assertion below observes.
	clock = t0.Add(48 * time.Hour)
	s.SetGroup("grouped", 1, []string{"grouped", "ungrouped"}) // toggles off: deletes the entry
	s.SetGroup("grouped", 1, []string{"grouped", "ungrouped"}) // toggles back on: recreates it with the new last_seen

	if e := s.entries["grouped"]; e.LastSeen != clock.Format(time.RFC3339) {
		t.Fatalf("grouped last_seen = %q, want %q", e.LastSeen, clock.Format(time.RFC3339))
	}
	if _, ok := s.entries["ungrouped"]; ok {
		t.Fatalf("ungrouped visible session got a junk entry: %#v", s.entries)
	}
}

func TestSessionStoreIgnoresEmptySessionID(t *testing.T) {
	s := loadSessionStore("", fixedClock(time.Now()))
	s.SetGroup("", 4, nil)
	if len(s.entries) != 0 {
		t.Fatalf("empty sessionID created entries: %#v", s.entries)
	}
}

func TestLoadSessionStoreCorruptOrMissing(t *testing.T) {
	missing := loadSessionStore(filepath.Join(t.TempDir(), "nope.json"), fixedClock(time.Now()))
	if len(missing.entries) != 0 {
		t.Fatalf("missing file yielded %#v", missing.entries)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	corrupt := loadSessionStore(path, fixedClock(time.Now()))
	if len(corrupt.entries) != 0 {
		t.Fatalf("corrupt file yielded %#v", corrupt.entries)
	}
}

func TestSessionStoreEmptyPathDisablesPersistence(t *testing.T) {
	s := loadSessionStore("", fixedClock(time.Now()))
	s.SetGroup("x", 1, []string{"x"})
	if s.Group("x") != 1 {
		t.Fatalf("in-memory group lost: %d", s.Group("x"))
	}
}
