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
// name and delegates the actual write to saveSnapshotFrom. Kept as a thin
// wrapper so callers that already have a fresh []Session (the auto-save
// ticker in server.go) can reuse that same slice instead of paying for a
// second CollectLocal — see saveSnapshotFrom's doc comment.
func SaveSnapshot(name string) (string, int, error) {
	sessions, err := CollectLocal()
	if err != nil {
		return "", 0, err
	}
	return saveSnapshotFrom(name, sessions)
}

// saveSnapshotFrom writes a snapshot named name from an already-collected
// []Session, atomically (unique temp file + rename) so a crash mid-write —
// expected only for the unattended auto-"latest" save — never leaves a
// truncated snapshot behind. The temp name must be unique per call, not a
// fixed name+".tmp": Task 6's auto-save ticker and a manual save can both
// target "latest" concurrently, and a shared temp path would let one
// writer's Rename publish a file the other is still mid-write into.
//
// Taking sessions as a parameter (rather than calling CollectLocal itself)
// matters for the auto-save ticker: it has to inspect the session list for a
// live session *before* deciding whether to save at all, and if that check
// and the save each called CollectLocal separately, every session could exit
// in the gap between the two calls — most likely exactly during the
// shutdown this feature protects against — leaving the ticker's "yes, save"
// decision stale by the time the second collection runs, so it saves an
// empty snapshot anyway. Passing the one already-collected slice through
// closes that window and skips the second CollectLocal price (ps -A, tmux
// pane mapping, transcript scans) on every tick.
func saveSnapshotFrom(name string, sessions []Session) (string, int, error) {
	path, err := snapshotPath(name)
	if err != nil {
		return "", 0, err
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
		return "", 0, err
	}
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, name+"-*.json.tmp")
	if err != nil {
		return "", 0, err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // best-effort; no-op once the rename below succeeds
	if _, err := f.Write(data); err != nil {
		f.Close()
		return "", 0, err
	}
	if err := f.Chmod(0o644); err != nil {
		f.Close()
		return "", 0, err
	}
	if err := f.Close(); err != nil {
		return "", 0, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", 0, err
	}
	return path, len(snap.Entries), nil
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

// RestoreEntryResult is the outcome of restoring one Snapshot entry.
type RestoreEntryResult struct {
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
	Restored  bool   `json:"restored"`
	Reason    string `json:"reason,omitempty"` // empty when Restored is true
}

// RestoreReport is the full outcome of a RestoreSnapshot call.
type RestoreReport struct {
	Results []RestoreEntryResult `json:"results"`
}

// RestoreSnapshot recreates every session in the named snapshot: one tmux
// session per entry, each resuming its original transcript. Best-effort — one
// entry failing (already live, transcript gone, cwd/repo root gone) does not
// stop the rest; every entry gets a Restored/Reason outcome in the report.
//
// A worktree entry (cwd under .claude/worktrees/<name>) resumes via
// ResumeSessionInWorktree, which spawns at the main checkout and passes
// --worktree <name> — the same command claude itself suggests on exit from a
// worktree session, and it works whether or not the worktree checkout is
// still on disk. Every other entry resumes via the plain ResumeSession path.
func RestoreSnapshot(name string) (RestoreReport, error) {
	snap, err := loadSnapshot(name)
	if err != nil {
		return RestoreReport{}, err
	}
	var report RestoreReport
	for _, e := range snap.Entries {
		result := RestoreEntryResult{SessionID: e.SessionID, Cwd: e.Cwd}
		var rerr error
		if wt := worktreeName(e.Cwd); wt != "" {
			_, rerr = ResumeSessionInWorktree(e.SessionID, worktreeRepoRoot(e.Cwd), wt)
		} else {
			_, rerr = ResumeSession(e.SessionID, e.Cwd)
		}
		if rerr != nil {
			result.Reason = rerr.Error()
		} else {
			result.Restored = true
		}
		report.Results = append(report.Results, result)
	}
	return report, nil
}
