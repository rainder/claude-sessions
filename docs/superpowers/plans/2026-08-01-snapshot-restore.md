# Session Snapshot Save/Restore Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `claude-sessions snapshot save|restore|list` — capture the current set of local sessions to a named file, and later recreate them (one tmux session per entry, each resuming its original transcript), including sessions whose original worktree checkout no longer exists.

**Architecture:** New `snapshot.go` owns the data model, save, restore, and list logic, built entirely on primitives that already exist (`CollectLocal`, `ResumeSession`, `worktreeName`/`worktreeRepoRoot`). `resume.go` gains one bug fix (orphan tmux cleanup) and one new function (`ResumeSessionInWorktree`) that the restore path needs. `commands.go`/`main.go` wire up the CLI surface. `server.go` gains a ticker that keeps a `latest` snapshot fresh while the server runs.

**Tech Stack:** Go stdlib only (`encoding/json`, `os/exec` for tmux, `regexp` for input validation) — matches the repo's zero-dependency-beyond-`x/term`-and-`x/sys` policy.

## Global Constraints

- Local sessions only — no remote-host capture/restore in this plan (see spec's Scope section).
- Snapshot files live at `~/.config/claude-sessions/snapshots/<name>.json`, never `/tmp` (spec: must survive a reboot).
- Restore is manual only — nothing auto-restores on server startup (spec: surprise-avoidance).
- Restore is best-effort — one entry failing must not abort the rest (spec: Restore flow).
- `snapshot restore <name>` requires an explicit name; no silent default to `latest` (spec: Restore flow).
- Auto-`latest` save runs only while `claude-sessions -s` is running, on a fixed 10-minute interval, no new config surface (spec: Save flow / Scope).

---

### Task 1: Fix `ResumeSession`'s orphaned-tmux-on-failure bug

**Files:**
- Modify: `resume.go:392-401` (`ResumeSession`)
- Test: `resume_test.go`

**Interfaces:**
- Consumes: `killTmuxSession(tname string)` (already exists, migrate.go:227).
- Produces: no signature change to `ResumeSession(sessionID, cwd string) (string, error)` — behavior only.

Today, if `tmux new-session` succeeds but the following `send-keys` fails, `ResumeSession` returns an error and leaves the empty tmux session running forever. `SpawnNew` (migrate.go:188-196) and `MigrateLocalAttested` (migrate.go:163-169) already guard against exactly this by calling `killTmuxSession` before returning. `ResumeSession` is the one caller missing it — and Task 4's batch restore loop is about to call it N times, so a failure mode that's rare today would start compounding orphaned sessions.

- [ ] **Step 1: Write the failing test**

Add to `resume_test.go`:

```go
// TestResumeSessionCleansUpOrphanOnSendKeysFailure: a send-keys failure after
// a successful tmux new-session must not leave the empty session running —
// batch restore (RestoreSnapshot) calls ResumeSession in a loop, and a leaked
// session per failure would compound silently.
func TestResumeSessionCleansUpOrphanOnSendKeysFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeResumableTranscript(t, home, "proj", "aaaa-1111", time.Now(),
		`{"cwd":"/srv/app","type":"user","message":{"role":"user","content":"hi"}}`)

	dir := t.TempDir()
	script := filepath.Join(dir, "tmux")
	// new-session succeeds; send-keys and the killTmuxSession cleanup's
	// kill-session both get logged so the test can assert cleanup ran.
	body := "#!/bin/sh\n" +
		"echo \"$@\" >> \"$TMUX_LOG\"\n" +
		"case \"$1\" in\n" +
		"  send-keys) exit 1;;\n" +
		"esac\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "tmux.log")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TMUX_LOG", logPath)

	if _, err := ResumeSession("aaaa-1111", "/srv/app"); err == nil {
		t.Fatal("expected error from send-keys failure")
	}

	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), "kill-session") {
		t.Errorf("tmux log = %q, want a kill-session cleanup call after send-keys failed", log)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./... -run TestResumeSessionCleansUpOrphanOnSendKeysFailure -v`
Expected: FAIL — tmux log contains no `kill-session` call.

- [ ] **Step 3: Fix `ResumeSession`**

In `resume.go`, replace:

```go
	if err := exec.Command("tmux", "send-keys", "-t", tname,
		"claude --resume "+sessionID, "Enter").Run(); err != nil {
		return "", fmt.Errorf("tmux send-keys: %w", err)
	}
	return tname, nil
}
```

with:

```go
	if err := exec.Command("tmux", "send-keys", "-t", tname,
		"claude --resume "+sessionID, "Enter").Run(); err != nil {
		// Same partial commit SpawnNew/MigrateLocalAttested guard against: the
		// session exists but was never told what to run.
		killTmuxSession(tname)
		return "", fmt.Errorf("tmux send-keys: %w", err)
	}
	return tname, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./... -run TestResumeSessionCleansUpOrphanOnSendKeysFailure -v`
Expected: PASS

- [ ] **Step 5: Run the full existing resume test suite to confirm no regression**

Run: `go test ./... -run TestResume -v`
Expected: PASS (all existing `TestResume*` tests still green)

- [ ] **Step 6: Commit**

```bash
git add resume.go resume_test.go
git commit -m "fix: clean up orphaned tmux session on ResumeSession send-keys failure"
```

---

### Task 2: Add worktree-aware resume (`ResumeSessionInWorktree`)

**Files:**
- Modify: `resume.go` (refactor `ResumeSession` to share a core with the new function)
- Test: `resume_test.go`

**Interfaces:**
- Consumes: `resumeSessionIDRe` (resume.go:369, existing), `liveSessionIDs()` (resume.go:351, existing), `findTranscript(home, sessionID string) string` (existing), `MakeTmuxName`, `tmuxNewDetachedSession`, `resolveSpawnSize`, `killTmuxSession` (all existing, migrate.go/spawn_size.go).
- Produces: `ResumeSessionInWorktree(sessionID, repoRoot, worktreeName string) (string, error)` — Task 4's `RestoreSnapshot` calls this for worktree entries.

Restoring a session whose original cwd was `<repo>/.claude/worktrees/<name>/...` needs a different command (`claude --worktree <name> --resume <id>`) run from the main checkout (`repoRoot`), not the possibly-gone worktree path — see the design spec's Restore flow section. `worktreeName` flows into a shell command via `tmux send-keys`, so it needs the same charset guard `resumeSessionIDRe` already gives `sessionID`.

Extract the shared logic in `ResumeSession` into a private `resumeCommon` so both functions stay in sync instead of duplicating the validate → check-live → spawn → send-keys sequence.

- [ ] **Step 1: Write the failing tests**

Add to `resume_test.go`:

```go
// TestResumeSessionInWorktreeSendsWorktreeFlag: a worktree-aware resume spawns
// tmux at the repo root (not the worktree path) and sends the --worktree
// incantation claude itself suggests on exit from a worktree session.
func TestResumeSessionInWorktreeSendsWorktreeFlag(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	logPath := installFakeTmux(t)
	writeResumableTranscript(t, home, "proj", "bbbb-2222", time.Now(),
		`{"cwd":"/repo/.claude/worktrees/DR-860","type":"user","message":{"role":"user","content":"hi"}}`)

	if _, err := ResumeSessionInWorktree("bbbb-2222", "/repo", "DR-860"); err != nil {
		t.Fatal(err)
	}

	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), "<new-session>") || !strings.Contains(string(log), "<-c></repo>") {
		t.Errorf("tmux log = %q, want new-session -c /repo", log)
	}
	if !strings.Contains(string(log), "claude --worktree DR-860 --resume bbbb-2222") {
		t.Errorf("tmux log = %q, want the --worktree --resume command", log)
	}
}

// TestResumeSessionInWorktreeRejectsUnsafeName: worktreeName reaches a shell
// command via tmux send-keys, so it needs the same charset guard sessionID
// already gets from resumeSessionIDRe — a name with shell metacharacters must
// be rejected before anything is spawned.
func TestResumeSessionInWorktreeRejectsUnsafeName(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	logPath := installFakeTmux(t)
	writeResumableTranscript(t, home, "proj", "cccc-3333", time.Now(),
		`{"cwd":"/repo/.claude/worktrees/evil","type":"user","message":{"role":"user","content":"hi"}}`)

	_, err := ResumeSessionInWorktree("cccc-3333", "/repo", "evil; rm -rf /")
	if err == nil {
		t.Fatal("expected error for unsafe worktree name")
	}

	log, _ := os.ReadFile(logPath)
	if len(log) != 0 {
		t.Errorf("tmux log = %q, want no tmux calls for a rejected name", log)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./... -run TestResumeSessionInWorktree -v`
Expected: FAIL — `ResumeSessionInWorktree` does not exist yet (compile error).

- [ ] **Step 3: Refactor `ResumeSession` into a shared core and add `ResumeSessionInWorktree`**

In `resume.go`, replace the whole `ResumeSession` function body with:

```go
// worktreeNameRe constrains worktree names to a safe charset before they reach
// a shell command via tmux send-keys — the same discipline resumeSessionIDRe
// applies to session ids.
var worktreeNameRe = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,100}$`)

// ResumeSession validates the transcript for sessionID exists, refuses if the
// session is already live (errResumeSessionLive), then spawns a fresh tmux
// session running `claude --resume <id>` in cwd. Returns the tmux session name.
// Shared by the server handler and the local TUI path.
func ResumeSession(sessionID, cwd string) (string, error) {
	if cwd == "" {
		return "", fmt.Errorf("resume: session id and cwd required")
	}
	return resumeCommon(sessionID, cwd, "claude --resume "+sessionID)
}

// ResumeSessionInWorktree resumes sessionID the same way ResumeSession does,
// but for a session whose original cwd was a `.claude/worktrees/<name>`
// checkout. It spawns tmux at repoRoot (the main checkout, which always still
// exists) rather than the worktree path, and sends `claude --worktree <name>
// --resume <id>` — the same incantation claude itself suggests on exit from a
// worktree session. This works whether or not the worktree checkout is still
// on disk: `--worktree <name>` recreates it if needed, or reuses it if
// present, since `.claude/worktrees/<name>` is Claude Code's own convention
// for that name.
func ResumeSessionInWorktree(sessionID, repoRoot, worktreeName string) (string, error) {
	if repoRoot == "" || worktreeName == "" {
		return "", fmt.Errorf("resume: repo root and worktree name required")
	}
	if !worktreeNameRe.MatchString(worktreeName) {
		return "", fmt.Errorf("resume: invalid worktree name")
	}
	return resumeCommon(sessionID, repoRoot, "claude --worktree "+worktreeName+" --resume "+sessionID)
}

// resumeCommon is the shared validate → refuse-if-live → spawn → send-keys
// sequence behind ResumeSession and ResumeSessionInWorktree. tmuxCwd is where
// the detached tmux session is created; command is what gets typed into it.
func resumeCommon(sessionID, tmuxCwd, command string) (string, error) {
	if sessionID == "" {
		return "", fmt.Errorf("resume: session id and cwd required")
	}
	if !resumeSessionIDRe.MatchString(sessionID) {
		return "", fmt.Errorf("resume: invalid session id")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if findTranscript(home, sessionID) == "" {
		return "", fmt.Errorf("no transcript for session %s", sessionID)
	}
	if liveSessionIDs()[sessionID] {
		return "", errResumeSessionLive
	}
	tname := MakeTmuxName(tmuxCwd, sessionID, "")
	cols, rows := resolveSpawnSize()
	if err := tmuxNewDetachedSession(tname, tmuxCwd, cols, rows); err != nil {
		return "", fmt.Errorf("tmux new-session: %w", err)
	}
	if err := exec.Command("tmux", "send-keys", "-t", tname, command, "Enter").Run(); err != nil {
		// Same partial commit SpawnNew/MigrateLocalAttested guard against: the
		// session exists but was never told what to run.
		killTmuxSession(tname)
		return "", fmt.Errorf("tmux send-keys: %w", err)
	}
	return tname, nil
}
```

Delete the old `ResumeSession` body (the one Task 1 just patched) since `resumeCommon` now contains that logic — Task 1's `killTmuxSession` call lives on inside `resumeCommon`.

Add `"regexp"` to `resume.go`'s import block if not already present (it is — `resumeSessionIDRe` already uses it).

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./... -run TestResume -v`
Expected: PASS, including Task 1's test and both new tests.

- [ ] **Step 5: Commit**

```bash
git add resume.go resume_test.go
git commit -m "feat: add ResumeSessionInWorktree for restoring worktree sessions"
```

---

### Task 3: Snapshot data model, name validation, save, and list

**Files:**
- Create: `snapshot.go`
- Test: `snapshot_test.go`

**Interfaces:**
- Consumes: `CollectLocal() ([]Session, error)` (session.go:161, existing) — uses `Session.SessionID`, `Session.CWD`.
- Produces:
  - `type SnapshotEntry struct { SessionID string; Cwd string }`
  - `type Snapshot struct { Name string; TakenAt time.Time; Entries []SnapshotEntry }`
  - `SaveSnapshot(name string) (path string, err error)`
  - `ListSnapshots() ([]Snapshot, error)`
  - `loadSnapshot(name string) (Snapshot, error)` — unexported, Task 4 also uses it.
  - `snapshotPath(name string) (string, error)` — unexported, Task 4 also uses it.
  - `snapshotDir() (string, error)` — unexported.

- [ ] **Step 1: Write the failing tests**

Create `snapshot_test.go`:

```go
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
	data, _ := json.Marshal(Session{PID: pid, SessionID: "", CWD: "/x", StartedAt: time.Now().UnixMilli()})
	if err := os.WriteFile(filepath.Join(sessDir, strconv.Itoa(pid)+".json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	path, err := SaveSnapshot("nosession")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	var snap Snapshot
	json.Unmarshal(raw, &snap)
	if len(snap.Entries) != 0 {
		t.Errorf("entries = %+v, want none (session has no SessionID)", snap.Entries)
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./... -run TestSnapshot -run TestListSnapshots -v`
Expected: FAIL — `snapshot.go` symbols don't exist yet (compile error).

- [ ] **Step 3: Write the implementation**

Create `snapshot.go`:

```go
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
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./... -run TestSnapshot -run TestListSnapshots -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add snapshot.go snapshot_test.go
git commit -m "feat: add snapshot save/list (snapshot.go)"
```

---

### Task 4: Restore flow (`RestoreSnapshot`)

**Files:**
- Modify: `snapshot.go` (append)
- Test: `snapshot_test.go` (append)

**Interfaces:**
- Consumes: `ResumeSession(sessionID, cwd string) (string, error)` (resume.go, Task 1), `ResumeSessionInWorktree(sessionID, repoRoot, worktreeName string) (string, error)` (resume.go, Task 2), `worktreeName(cwd string) string` (session.go:128, existing), `worktreeRepoRoot(cwd string) string` (worktree.go:33, existing), `loadSnapshot(name string) (Snapshot, error)` (Task 3).
- Produces:
  - `type RestoreEntryResult struct { SessionID string; Cwd string; Restored bool; Reason string }`
  - `type RestoreReport struct { Results []RestoreEntryResult }`
  - `RestoreSnapshot(name string) (RestoreReport, error)`

- [ ] **Step 1: Write the failing tests**

Append to `snapshot_test.go`:

```go
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./... -run TestRestoreSnapshot -v`
Expected: FAIL — `RestoreSnapshot` doesn't exist yet (compile error).

- [ ] **Step 3: Write the implementation**

Append to `snapshot.go`:

```go
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
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./... -run TestRestoreSnapshot -v`
Expected: PASS

- [ ] **Step 5: Run the full snapshot + resume suites together**

Run: `go test ./... -run 'TestSnapshot|TestListSnapshots|TestRestoreSnapshot|TestResume' -v`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add snapshot.go snapshot_test.go
git commit -m "feat: add RestoreSnapshot with best-effort worktree-aware restore"
```

---

### Task 5: CLI surface (`snapshot save|restore|list`)

**Files:**
- Modify: `commands.go` (append)
- Modify: `main.go` (dispatch + usage string)
- Modify: `README.md` (usage block)

**Interfaces:**
- Consumes: `SaveSnapshot(name string) (string, error)`, `RestoreSnapshot(name string) (RestoreReport, error)`, `ListSnapshots() ([]Snapshot, error)` (all Tasks 3–4).
- Produces: `cmdSnapshot(args []string) int` — dispatched from `main.go` the same way every other subcommand is.

This task is CLI plumbing over already-tested primitives — matches how `cmdKill`/`cmdMigrate`/`cmdNew` are themselves untested at the wrapper layer in this codebase (their argument-parsing helpers are tested; the `cmd*` I/O wrappers are verified by build + manual smoke test, per existing convention). No new test file.

- [ ] **Step 1: Add `cmdSnapshot` to `commands.go`**

Append to `commands.go`:

```go
func cmdSnapshot(args []string) int {
	const usage = "usage: claude-sessions snapshot save [name] | restore NAME | list"
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	switch args[0] {
	case "save":
		name := time.Now().Format("2006-01-02-1504")
		if len(args) > 1 {
			name = args[1]
		}
		path, err := SaveSnapshot(name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "snapshot save: %v\n", err)
			return 1
		}
		fmt.Printf("saved snapshot %q to %s\n", name, path)
		return 0
	case "restore":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: claude-sessions snapshot restore NAME")
			return 2
		}
		report, err := RestoreSnapshot(args[1])
		if err != nil {
			fmt.Fprintf(os.Stderr, "snapshot restore: %v\n", err)
			return 1
		}
		restored := 0
		for _, r := range report.Results {
			mark := "✗"
			status := r.Reason
			if r.Restored {
				mark = "✓"
				status = r.Cwd
				restored++
			}
			fmt.Printf("  %s %s %s\n", mark, r.SessionID, status)
		}
		fmt.Printf("restored %d/%d\n", restored, len(report.Results))
		if restored < len(report.Results) {
			return 1
		}
		return 0
	case "list":
		snaps, err := ListSnapshots()
		if err != nil {
			fmt.Fprintf(os.Stderr, "snapshot list: %v\n", err)
			return 1
		}
		if len(snaps) == 0 {
			fmt.Println("no snapshots saved")
			return 0
		}
		for _, s := range snaps {
			fmt.Printf("%-30s %s  %d session(s)\n", s.Name, s.TakenAt.Format(time.RFC3339), len(s.Entries))
		}
		return 0
	default:
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
}
```

- [ ] **Step 2: Wire the dispatch into `main.go`**

In `main.go`, add a case alongside `"kill"`/`"migrate"`/`"new"`:

```go
	case "snapshot":
		os.Exit(cmdSnapshot(args[1:]))
```

Add a usage line to the `usage` const, after the `service` line:

```go
  snapshot save [name] | restore NAME | list
                                  save/restore the local session set; name
                                  defaults to a timestamp; restore recreates
                                  each session (best-effort) via tmux + resume
```

- [ ] **Step 3: Build and smoke-test manually**

```bash
go build ./... && go vet ./...
./claude-sessions snapshot list                 # expect "no snapshots saved"
./claude-sessions snapshot save smoketest
./claude-sessions snapshot list                 # expect one row: smoketest
```

Expected: builds clean, all three commands run without error, `list` shows `smoketest` after `save`. (Skip `restore` in this manual check — it spawns a real tmux session against whatever's actually running; Task 4's tests already cover restore logic in isolation.)

- [ ] **Step 4: Update `README.md`'s usage block**

In `README.md`, after the `service install ...` block (currently ending around line 91), add:

```
claude-sessions snapshot save [name]       # save the local session set (name defaults to a timestamp)
claude-sessions snapshot restore NAME      # recreate a saved set (best-effort; requires an explicit name)
claude-sessions snapshot list              # list saved snapshots
```

- [ ] **Step 5: Commit**

```bash
git add commands.go main.go README.md
git commit -m "feat: wire up snapshot save|restore|list CLI subcommand"
```

---

### Task 6: Auto-maintain the `latest` snapshot in the server

**Files:**
- Modify: `server.go`
- Test: none new — the ticker itself follows the existing untested paste-binding-ticker precedent (server.go:1587-1594); the function it calls each tick (`SaveSnapshot`) is already fully tested by Task 3.

**Interfaces:**
- Consumes: `SaveSnapshot(name string) (string, error)` (Task 3), `snapshotAutoSaveInterval` (Task 3, snapshot.go).
- Produces: none new — this task only adds a background goroutine to `cmdServer`.

- [ ] **Step 1: Add the ticker to `cmdServer`**

In `server.go`, find the hub-startup block (around line 1518-1519, right after `codexUsageHub := NewCodexUsageHub()` / `defer codexUsageHub.Shutdown()`), and add immediately after it:

```go
	// Auto-maintain a "latest" snapshot so a reboot doesn't require having
	// remembered to save beforehand. Best-effort: a failed save is logged, never
	// fatal to the server. No Shutdown/stop — matches the existing paste-binding
	// ticker below, which also runs for the process's lifetime.
	go func() {
		t := time.NewTicker(snapshotAutoSaveInterval)
		defer t.Stop()
		for range t.C {
			if _, err := SaveSnapshot("latest"); err != nil {
				fmt.Fprintf(os.Stderr, "claude-sessions: auto-snapshot failed: %v\n", err)
			}
		}
	}()
```

- [ ] **Step 2: Build**

Run: `go build ./... && go vet ./...`
Expected: clean build, no vet warnings.

- [ ] **Step 3: Manual smoke test**

```bash
./claude-sessions -s --port 18765 &
SERVER_PID=$!
sleep 1
kill $SERVER_PID
```

This only confirms the server still starts cleanly with the new goroutine present — the 10-minute interval is too long to observe a tick in a smoke test, and `SaveSnapshot`'s behavior is already covered by Task 3's tests.

- [ ] **Step 4: Run the full test suite**

Run: `go test ./... && go vet ./...`
Expected: all packages PASS, vet clean.

- [ ] **Step 5: Commit**

```bash
git add server.go
git commit -m "feat: auto-maintain a latest snapshot every 10 minutes while the server runs"
```

---

## Self-Review Notes

**Spec coverage:**
- Data model → Task 3.
- Save flow (`SaveSnapshot`, atomic write, CLI `save`) → Tasks 3, 5.
- Auto-`latest` ticker → Task 6.
- Restore flow (best-effort, worktree branch, CLI `restore`) → Tasks 2, 4, 5.
- Prerequisite fix (`ResumeSession` orphan cleanup) → Task 1.
- `snapshot list` → Tasks 3, 5.
- Snapshot name validation → Task 3.
- README/usage docs → Task 5.

**Type consistency:** `SnapshotEntry{SessionID, Cwd}` (Task 3) is what `Snapshot.Entries` holds throughout; `RestoreEntryResult{SessionID, Cwd, Restored, Reason}` (Task 4) is used identically in `RestoreReport.Results` and in Task 5's CLI printing. `ResumeSessionInWorktree(sessionID, repoRoot, worktreeName string)` (Task 2) is called with that exact argument order in Task 4's `RestoreSnapshot`.

**No placeholders:** every step has real code; none defers to "add appropriate error handling" or "similar to Task N".
