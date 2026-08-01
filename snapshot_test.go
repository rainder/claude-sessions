package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestSnapshotPathRejectsUnsafeNames(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cases := []struct {
		name    string
		wantErr bool
	}{
		{"latest", false},
		{"my-snapshot_2026.08.01", false},
		{"", true},
		{"../etc/passwd", true},
		{"with/slash", true},
		{"with spaces", true},
	}
	for _, tc := range cases {
		_, err := snapshotPath(tc.name)
		if (err != nil) != tc.wantErr {
			t.Errorf("snapshotPath(%q) err = %v, wantErr %v", tc.name, err, tc.wantErr)
		}
	}
}

func TestSaveSnapshotCapturesLocalSessions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	sessDir := filepath.Join(home, ".claude", "sessions")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pid := os.Getpid()
	data, err := json.Marshal(Session{
		PID: pid, SessionID: "snap-test-1", CWD: "/home/testuser/project",
		StartedAt: time.Now().UnixMilli(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessDir, strconv.Itoa(pid)+".json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	path, err := SaveSnapshot("mysave")
	if err != nil {
		t.Fatal(err)
	}
	wantPath := filepath.Join(home, ".config", "claude-sessions", "snapshots", "mysave.json")
	if path != wantPath {
		t.Errorf("path = %q, want %q", path, wantPath)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var snap Snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatal(err)
	}
	if snap.Name != "mysave" {
		t.Errorf("Name = %q, want mysave", snap.Name)
	}
	var found bool
	for _, e := range snap.Entries {
		if e.SessionID == "snap-test-1" && e.Cwd == "/home/testuser/project" {
			found = true
		}
	}
	if !found {
		t.Errorf("entries = %+v, want an entry for snap-test-1", snap.Entries)
	}
}

func TestSaveSnapshotSkipsSessionsWithoutSessionID(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	sessDir := filepath.Join(home, ".claude", "sessions")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pid := os.Getpid()
	noSession, err := json.Marshal(Session{PID: pid, SessionID: "", CWD: "/x", StartedAt: time.Now().UnixMilli()})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessDir, strconv.Itoa(pid)+".json"), noSession, 0o644); err != nil {
		t.Fatal(err)
	}
	withSession, err := json.Marshal(Session{
		PID: pid, SessionID: "snap-test-2", CWD: "/home/testuser/other",
		StartedAt: time.Now().UnixMilli(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessDir, strconv.Itoa(pid)+"-with-session.json"), withSession, 0o644); err != nil {
		t.Fatal(err)
	}

	path, err := SaveSnapshot("nosession")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var snap Snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.Entries) != 1 {
		t.Fatalf("entries = %+v, want exactly 1 (the session with a SessionID)", snap.Entries)
	}
	if got := snap.Entries[0]; got.SessionID != "snap-test-2" || got.Cwd != "/home/testuser/other" {
		t.Errorf("entries[0] = %+v, want {SessionID: snap-test-2, Cwd: /home/testuser/other}", got)
	}
}

func TestListSnapshotsReturnsAllSavedNewestFirst(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if _, err := SaveSnapshot("first"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if _, err := SaveSnapshot("second"); err != nil {
		t.Fatal(err)
	}

	snaps, err := ListSnapshots()
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 2 {
		t.Fatalf("len(snaps) = %d, want 2", len(snaps))
	}
	if snaps[0].Name != "second" || snaps[1].Name != "first" {
		t.Errorf("order = [%s, %s], want [second, first] (newest first)", snaps[0].Name, snaps[1].Name)
	}
}
