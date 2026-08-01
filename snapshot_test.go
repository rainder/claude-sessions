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

func TestRestoreSnapshotResumesPlainAndWorktreeEntries(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	installFakeTmux(t)
	writeResumableTranscript(t, home, "proj1", "plain-1111", time.Now(),
		`{"cwd":"/srv/app","type":"user","message":{"role":"user","content":"hi"}}`)
	writeResumableTranscript(t, home, "proj2", "wt-2222", time.Now(),
		`{"cwd":"/repo/.claude/worktrees/DR-1","type":"user","message":{"role":"user","content":"hi"}}`)

	snap := Snapshot{
		Name:    "restoreme",
		TakenAt: time.Now(),
		Entries: []SnapshotEntry{
			{SessionID: "plain-1111", Cwd: "/srv/app"},
			{SessionID: "wt-2222", Cwd: "/repo/.claude/worktrees/DR-1"},
		},
	}
	data, _ := json.MarshalIndent(snap, "", "  ")
	path, err := snapshotPath("restoreme")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := RestoreSnapshot("restoreme")
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Results) != 2 {
		t.Fatalf("len(Results) = %d, want 2", len(report.Results))
	}
	for _, r := range report.Results {
		if !r.Restored {
			t.Errorf("entry %s: Restored = false, Reason = %q, want true", r.SessionID, r.Reason)
		}
	}
}

func TestRestoreSnapshotSkipsAlreadyLiveEntry(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	installFakeTmux(t)
	writeResumableTranscript(t, home, "proj", "live-3333", time.Now(),
		`{"cwd":"/srv/app","type":"user","message":{"role":"user","content":"hi"}}`)

	// A session file makes CollectLocal (and thus liveSessionIDs) report this
	// session id as already running.
	sessDir := filepath.Join(home, ".claude", "sessions")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pid := os.Getpid()
	data, _ := json.Marshal(Session{PID: pid, SessionID: "live-3333", CWD: "/srv/app", StartedAt: time.Now().UnixMilli()})
	if err := os.WriteFile(filepath.Join(sessDir, strconv.Itoa(pid)+".json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	snap := Snapshot{Name: "livecheck", TakenAt: time.Now(), Entries: []SnapshotEntry{
		{SessionID: "live-3333", Cwd: "/srv/app"},
	}}
	snapData, _ := json.MarshalIndent(snap, "", "  ")
	path, _ := snapshotPath("livecheck")
	if err := os.WriteFile(path, snapData, 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := RestoreSnapshot("livecheck")
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Results) != 1 || report.Results[0].Restored {
		t.Fatalf("Results = %+v, want one skipped entry", report.Results)
	}
	if report.Results[0].Reason == "" {
		t.Error("Reason is empty, want an explanation for the skip")
	}
}

func TestRestoreSnapshotContinuesAfterOneFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	installFakeTmux(t)
	// Only the second session has a transcript; the first will fail to resume.
	writeResumableTranscript(t, home, "proj2", "good-5555", time.Now(),
		`{"cwd":"/srv/good","type":"user","message":{"role":"user","content":"hi"}}`)

	snap := Snapshot{Name: "mixed", TakenAt: time.Now(), Entries: []SnapshotEntry{
		{SessionID: "missing-4444", Cwd: "/srv/missing"},
		{SessionID: "good-5555", Cwd: "/srv/good"},
	}}
	data, _ := json.MarshalIndent(snap, "", "  ")
	path, _ := snapshotPath("mixed")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := RestoreSnapshot("mixed")
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Results) != 2 {
		t.Fatalf("len(Results) = %d, want 2 (best-effort: both entries attempted)", len(report.Results))
	}
	if report.Results[0].Restored {
		t.Error("Results[0].Restored = true, want false (no transcript)")
	}
	if !report.Results[1].Restored {
		t.Errorf("Results[1].Restored = false, Reason = %q, want true", report.Results[1].Reason)
	}
}
