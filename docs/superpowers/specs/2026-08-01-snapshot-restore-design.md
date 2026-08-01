# Session snapshot save/restore — design

Date: 2026-08-01

## Problem

A VM reboot (or a crashed `claude-sessions` server) kills every tmux session
on that host. Nothing captures what was running beforehand, so getting back
to where you were means manually remembering which sessions existed, finding
their transcripts, and resuming each one by hand.

## Goal

`claude-sessions snapshot save [name]` / `claude-sessions snapshot restore
<name>` — capture the current set of local sessions to a named file, and
later recreate that set: one tmux session per entry, each resuming its
original transcript.

An auto-maintained `latest` snapshot, refreshed periodically by the server,
means recovery doesn't depend on remembering to save before the VM goes down.

## Scope

- **Local only.** No remote-host capture or restore in v1 — remote resume
  already exists (`POST /sessions/resume`, `GET /resumable`) but snapshot
  save/restore doesn't drive it yet. Revisit once local is proven out.
- **Auto-`latest` requires the server running.** The periodic save lives in
  `cmdServer`; if `claude-sessions -s` isn't running as a persistent service on
  a host, nothing keeps `latest` fresh there. Manual `snapshot save <name>`
  still works standalone via the CLI regardless.
- **Manual restore only.** The server never auto-restores on its own startup.
  Spawning tmux sessions and `claude` processes unattended is a correctness
  and surprise risk out of proportion to the convenience — the user runs
  `snapshot restore` deliberately, when ready.
- **Best-effort restore.** One entry failing (already live, transcript
  vanished, cwd/repo root gone) does not abort the rest. Every entry gets a
  restored/skipped outcome in the final report.
- **No worktree-metadata capture.** Restoring a session whose worktree
  checkout got cleaned up between save and restore is handled by an existing
  `claude` CLI capability (`--worktree <name> --resume <id>`, see below), not
  by storing repo-root/branch info and reconstructing the worktree ourselves.

## Data model (`snapshot.go`, new file)

```go
type SnapshotEntry struct {
	SessionID string
	Cwd       string
}

type Snapshot struct {
	Name    string
	TakenAt time.Time
	Entries []SnapshotEntry
}
```

Stored as JSON, one file per name, at
`~/.config/claude-sessions/snapshots/<name>.json` — not `/tmp`, which can be
tmpfs and is wiped on reboot; this needs to survive exactly the event it
protects against. `latest` is the reserved name for the auto-maintained slot;
anything else is a user-chosen manual save. Names are validated against a
safe charset (same style as `resumeSessionIDRe`) before touching the
filesystem, since a name becomes a path component.

## Save flow

`SaveSnapshot(name string) (path string, err error)`:

1. `CollectLocal()` — the same primitive the TUI and server already use for
   `/sessions`.
2. Map each session to `SnapshotEntry{SessionID, Cwd}`. Skip sessions with no
   resolvable session id — nothing to `--resume` later.
3. Write JSON atomically: write to `<name>.json.tmp`, then `os.Rename`. Matters
   most for `latest`, which writes unattended on a timer — a crash mid-write
   must never leave a truncated snapshot behind.

CLI: `claude-sessions snapshot save [name]`. `name` is optional; omitted, it
defaults to a timestamp (`2026-08-01-1830`) so a bare `snapshot save` always
works.

**Auto-`latest`:** `cmdServer` starts a ticker goroutine (same shape as the
existing 60s paste-binding-reassert ticker, server.go:1587) that calls
`SaveSnapshot("latest")` every 10 minutes. Fixed interval, no new config
surface in v1. Failures are logged, never fatal to the server.

## Restore flow

`RestoreSnapshot(name string) (RestoreReport, error)`:

1. Read `<name>.json`.
2. For each entry, attempt restore; record the outcome; continue regardless
   of failure (best-effort, per Scope).
3. Per entry, branch on whether the original cwd was a worktree checkout:
   - `worktreeName(cwd) != ""` (worktree.go) → spawn tmux at
     `worktreeRepoRoot(cwd)` (the main checkout, which always still exists)
     and send `claude --worktree <worktreeName(cwd)> --resume <sessionID>`.
     This is the same incantation `claude` itself suggests on exit from a
     worktree session, and it works whether or not the worktree checkout is
     still on disk: `.claude/worktrees/<name>` is Claude Code's own worktree
     convention (worktree.go's marker comment), so `--worktree <name>`
     recreates it if needed or reuses it if present — no branch/repo-root
     metadata needs to be captured at save time, since both are derivable
     from the stored `cwd` alone.
   - otherwise → today's `ResumeSession(sessionID, cwd)` path, unchanged.
4. `RestoreReport` is `[]entry{SessionID, Cwd, Status, Reason}` — printed as a
   table:
   ```
   restored 3/4
     ✓ a1b2c3 /Users/andy/Developer/trecs-brain
     ✓ d4e5f6 /Users/andy/Developer/trecs-brain/.claude/worktrees/DR-2500
     ✗ 7a8b9c already live (skipped)
     ✗ 2d3e4f repo root gone: /Users/andy/old-project
   ```

CLI: `claude-sessions snapshot restore <name>`. `name` is required — no
silent default to `latest`; restoring the wrong set unattended is exactly the
surprise that "manual restore only" (Scope) exists to avoid.

CLI: `claude-sessions snapshot list` — lists
`~/.config/claude-sessions/snapshots/*.json` with taken-at time and entry
count, so `restore`'s required `<name>` has something to look up.

### Prerequisite fix

`ResumeSession` (resume.go:375) doesn't clean up its tmux session if
`send-keys` fails after tmux creation succeeds. Today that's a rare
single-shot path; a batch restore calling it N times in a loop would compound
orphaned tmux sessions on every partial failure. Fix first: on `send-keys`
failure, kill the tmux session just created, matching the cleanup
`SpawnNew`/`MigrateLocalAttested` already do (migrate.go:163, migrate.go:188).

### Explicitly not done

Not reusing `collectResumableFrom`'s staleness filters (age, corrupt,
zero-byte transcripts) in restore. Those guard the *general* resume picker's
sweep across months of history. A snapshot is narrower — its entries were
live sessions (per `CollectLocal`) minutes-to-hours before restore, not
archaeology. If that assumption is ever wrong (restoring a very old manual
snapshot), a bad entry just becomes a `skipped` row, not a crash.

## Testing

- `SaveSnapshot`/`RestoreSnapshot` against a temp `HOME`, mirroring existing
  test patterns (`usage_test.go`, `render_test.go`) — fakes/temp dirs, no real
  tmux.
- Worktree-branch command construction: table test over `worktreeName(cwd)`
  empty vs. non-empty, asserting which command variant is built (`--resume`
  vs. `--worktree ... --resume`) and which cwd tmux is spawned at — this is
  pure/testable without touching tmux.
- `ResumeSession` orphan-cleanup: a forced `send-keys` failure results in the
  tmux session being torn down.
- Best-effort loop: table test mixing live/dead/missing-cwd entries, asserting
  one failure doesn't abort the rest and the report reflects every outcome.
- Snapshot name validation: rejects path-traversal / unsafe characters before
  any file write.
