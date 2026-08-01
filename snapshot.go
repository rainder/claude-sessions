package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

// snapshotAutoSaveInterval is how often the server refreshes the "latest"
// snapshot while it runs. Fixed for v1 — no config surface yet.
const snapshotAutoSaveInterval = 10 * time.Minute

// snapshotNameRe constrains snapshot names to a safe charset — a name becomes
// a filename, so this rejects path traversal and anything shell-unsafe before
// it ever touches the filesystem.
var snapshotNameRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)

// SnapshotEntry is one session captured in a Snapshot.
type SnapshotEntry struct {
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
}

// Snapshot is a named, timestamped capture of the local sessions that were
// running when it was taken.
type Snapshot struct {
	Name    string          `json:"name"`
	TakenAt time.Time       `json:"takenAt"`
	Entries []SnapshotEntry `json:"entries"`
}

// snapshotDir returns ~/.config/claude-sessions/snapshots, creating it if
// missing. Not /tmp: a snapshot's entire purpose is surviving the reboot that
// would wipe a tmpfs /tmp.
func snapshotDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".config", "claude-sessions", "snapshots")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// snapshotPath validates name and returns its on-disk path.
func snapshotPath(name string) (string, error) {
	if !snapshotNameRe.MatchString(name) {
		return "", fmt.Errorf("invalid snapshot name: %q", name)
	}
	dir, err := snapshotDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name+".json"), nil
}

// SaveSnapshot captures the current local sessions (via CollectLocal) under
// name, writing atomically (temp file + rename) so a crash mid-write —
// expected only for the unattended auto-"latest" save — never leaves a
// truncated snapshot behind.
func SaveSnapshot(name string) (string, error) {
	path, err := snapshotPath(name)
	if err != nil {
		return "", err
	}
	sessions, err := CollectLocal()
	if err != nil {
		return "", err
	}
	snap := Snapshot{Name: name, TakenAt: time.Now()}
	for _, s := range sessions {
		if s.SessionID == "" {
			continue
		}
		snap.Entries = append(snap.Entries, SnapshotEntry{SessionID: s.SessionID, Cwd: s.CWD})
	}
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return "", err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	return path, nil
}

// loadSnapshot reads and decodes the snapshot file for name.
func loadSnapshot(name string) (Snapshot, error) {
	path, err := snapshotPath(name)
	if err != nil {
		return Snapshot{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Snapshot{}, err
	}
	var snap Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return Snapshot{}, fmt.Errorf("snapshot %q is corrupt: %w", name, err)
	}
	return snap, nil
}

// ListSnapshots returns every saved snapshot, newest first.
func ListSnapshots() ([]Snapshot, error) {
	dir, err := snapshotDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var snaps []Snapshot
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		name := e.Name()[:len(e.Name())-len(".json")]
		snap, err := loadSnapshot(name)
		if err != nil {
			continue // corrupt/partial file: skip it, don't fail the whole list
		}
		snaps = append(snaps, snap)
	}
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].TakenAt.After(snaps[j].TakenAt) })
	return snaps, nil
}
