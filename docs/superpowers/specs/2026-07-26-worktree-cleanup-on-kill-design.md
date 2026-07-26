# Worktree cleanup on kill — design

Date: 2026-07-26

## Problem

Sessions frequently run inside a git worktree at `<repo>/.claude/worktrees/<name>`.
When the last session working in such a worktree is killed, the checkout is left
behind: a stale directory plus a stale `git worktree list` entry that has to be
cleaned up by hand later. Nothing in the tool notices the moment when cleanup
became safe — the instant the worktree went idle.

## Goal

When a kill leaves a worktree with no live sessions in it, offer to remove that
worktree. Offer, never assume: the removal is a separate confirmation, and it
never destroys uncommitted work.

## Scope

All three kill entry points:

- local TUI (`k` on a local row)
- remote TUI (`k` on a remote row) — via the HTTP server
- `claude-sessions kill PID` subcommand

Out of scope (explicit decisions, not oversights):

- **Branch deletion.** The worktree's branch survives. Deleting it is a separate
  decision with its own merged/unmerged risk.
- **`git worktree remove --force`.** When git refuses because the tree is dirty
  or has untracked files, its message is surfaced and the worktree is kept. No
  path through this feature can discard uncommitted work.

## Primitives (`worktree.go`)

```go
// "/repo/.claude/worktrees/DR-860/sub/dir" -> "/repo/.claude/worktrees/DR-860"
func worktreeRoot(cwd string) string

// "/repo/.claude/worktrees/DR-860/sub/dir" -> "/repo"
func worktreeRepoRoot(cwd string) string

// A linked worktree's ".git" is a FILE containing "gitdir: ...", not a dir.
func isGitWorktree(root string) bool

// Sessions that keep the target's worktree occupied after the kill.
func worktreeSurvivors(target Session, all []Session) []Session

// git -C <repoRoot> worktree remove <path>
func RemoveWorktree(path string) error
```

`worktreeRoot` reuses the `/.claude/worktrees/` marker already used by
`worktreeName` (session.go) and `collapseWorktreePath` (render.go).

`worktreeSurvivors` returns sessions that share the target's worktree root,
minus:

- the target itself (matched on `Host` + `PID`);
- sessions on a different `Host` — a remote host's path namespace is its own;
- **tmux siblings**: sessions whose tmux session name equals the target's. A
  tmux-backed kill runs `tmux kill-session`, which takes down every pane in that
  session, so those sessions die alongside the target and must not count as
  survivors.

Removal is offered only when `worktreeSurvivors` is empty **and**
`isGitWorktree(root)` — a directory that merely matches the path shape is not a
registered worktree and is left alone.

`RemoveWorktree` runs git from the *repo root*, not from inside the worktree
being removed, and goes through an injected command runner so tests never shell
out.

## Flow per entry point

### Local TUI (`actKill`, actions.go)

1. Compute survivors from `c.targets` **before** killing (post-kill the session
    is gone from every snapshot).
2. Existing kill confirmation → `KillSession`.
3. On success, if the pre-kill survivor list was empty and the root is a real
    worktree: a second `confirmOverlay` —
    `last session in worktree "DR-860" — remove it?`
4. `RemoveWorktree`; on error, print git's message and `pauseForKey`.

### Subcommand (`cmdKill`, commands.go)

Flags become an order-independent scan instead of `args[1] == "-y"`:

- `-y` — assume yes for the kill itself.
- `--remove-worktree` — assume yes for the worktree removal.

Session list comes from `CollectLocal`. Interactive runs (no `-y`) prompt
`remove worktree "DR-860" at <path>? [y/N]` after a successful kill. With `-y`
and no `--remove-worktree` the worktree is kept — a non-interactive kill never
removes anything the caller didn't ask for. Usage string updated.

### Server + remote TUI

`POST /sessions/{pid}/kill` gains a response field, computed server-side from
the server's own `CollectLocal` before the kill (the server is authoritative
about its own host; the client does not guess):

```json
{"ok": true, "worktree": {"path": "/repo/.claude/worktrees/DR-860",
                          "name": "DR-860", "last": true}}
```

The field is omitted when the session wasn't in a worktree. Older clients ignore
the extra key; a newer client against an older server simply never sees
`last: true`, so both directions degrade to today's behaviour.

New endpoint `POST /worktree/remove`, body `{"path": "..."}`, authed like every
other route. The path arrives from a client, so it is validated before anything
touches disk:

- absolute, and unchanged by `filepath.Clean` (rejects `..` and dirty forms);
- contains the `/.claude/worktrees/` marker and resolves to a worktree root;
- exists and satisfies `isGitWorktree`;
- has no live session under it (re-checked at removal time, so a session that
  started between the kill and the removal blocks it).

`actKillRemote` prompts on `last: true` and POSTs the path; failures render like
any other remote action error.

## Testing

- `worktreeRoot` / `worktreeRepoRoot` / `worktreeName` agreement on a shared
  table, including non-worktree and trailing-slash inputs.
- `worktreeSurvivors`: occupied worktree, last-session case, cross-host
  same-path case, tmux-sibling exclusion, non-worktree cwd.
- `RemoveWorktree` through the injected runner: argument shape (`-C <repoRoot>`)
  and error propagation.
- Server: response shape with and without a worktree; `/worktree/remove`
  rejecting each invalid path class and the live-session case.
- `cmdKill` flag scan: `-y`, `--remove-worktree`, both, either order.
