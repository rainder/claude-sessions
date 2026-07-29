# Loopback-Server-Only Local Mutations Design

**Date:** 2026-07-29
**Status:** REJECTED — see `2026-07-29-thread-mutation-preconditions-design.md`

**Why rejected:** this design's whole premise was that local kill/migrate/
spawn are racier than remote, because remote gets `sessionIDPrecondition`/
`reattest`/`spawnDedupe` and local doesn't. An `ask-codex` review (independently
verified directly against server.go/paste.go before accepting it) found that
premise false: those guards are opt-in per request field
(`session_id`/`request_id`), and the *existing* remote call sites
(`actKillRemote`, `actAttachRemote`'s migrate call, `actNewRemote`,
`cmdNewRemote`) never send them either — `{}` bodies throughout. Remote is
exactly as unguarded as local today; there was no gap for routing local
through HTTP to close. It also would have introduced a real regression: a
`--bind tailscale` daemon serves a second, paste-only loopback listener
(`paste.go:446-461`) that would make every local mutation misreport "server
not reachable." Kept in the repo for the reasoning trail; superseded in full
by the smaller, correct fix in the doc named above.

## Goal

Remove the client's direct, in-process **mutation** of local sessions
(`KillSession`, `MigrateLocal`, `SpawnNew`, `DisabledStore.SetDisabled`
called straight from `actions.go`/`commands.go`). Route local kill/migrate/
spawn/disable through the local `-s` daemon's HTTP API instead — the same
path remote hosts already use — so local mutations get the same
`sessionIDPrecondition` + `reattest` (CLAUDE.md's TOCTOU guard) and
`spawnDedupe` protection remote mutations already have. Local mutations
today have none of that; remote mutations of the identical feature do. If
the daemon isn't reachable, the client refuses loudly rather than silently
falling back to the racy direct call — "always depend on a running server"
means mutations actually depend on one.

## History and correction

The original draft of this spec proposed an explicit `self: true`
servers.yaml entry and a hard error when servers.yaml has zero entries.
That was written before reading `server_client.go`, which already
implements almost exactly this pattern for **reads**:
`collectClientLocal()` (server_client.go:119) tries the loopback daemon
first via `fetchLocalServerSessions()` → `sessionServerConfig("")` (a
hardcoded `127.0.0.1:8765` config with an auto-created token from
`loadOrCreateToken()`, no servers.yaml involvement at all) and silently
falls back to direct `CollectLocal()` on any failure. `dropSelfServer`
(remote.go:72) actively filters a servers.yaml entry that points at
`localAddrs()` (includes `127.0.0.1`/`localhost`/`::1`) out of
`FetchAllRemote` — i.e. the codebase already rejects the "local as a
servers.yaml entry" shape the original draft proposed. Local access is not,
and under this design still isn't, config-driven. This revision builds on
`sessionServerConfig("")` / `localServerRequestWithTimeout` instead of
inventing config surface to re-derive what that function already returns.

## Non-goals

- No change to `render.go`, `service install`/`uninstall`/`status`
  internals, or servers.yaml's shape (`servers:`/`commands:` unchanged,
  `ServerConfig` unchanged).
- No change to `collectClientLocal`'s existing silent-fallback behavior for
  **reads** — the TUI and `cmdList` still show sessions when the daemon is
  down. Only mutations (kill/migrate/spawn/disable) switch to refuse-loudly.
- No auto-spawn of the daemon on demand. A local mutation with the daemon
  unreachable errors with instructions (`claude-sessions service install`);
  it does not launch one behind the user's back.
- No change to remote (non-loopback) behavior at all — `remote_actions.go`'s
  SSH-attach, HTTP-mutate path against `servers.yaml` entries is untouched.
- No change to `worktree.go:137` or `resume.go:353`'s direct `CollectLocal()`
  calls — the former's fresh, uncached collect right before a destructive
  decision is a documented invariant; the latter is out of scope (resume
  picker, not a mutation).

## Architecture

### Local mutations go through the loopback daemon

`actKill`/`actAttach`/`actNew`/`actToggleDisabled` (actions.go) currently
branch on `s.Host != ""` and, for the local (`Host == ""`) case, call
`KillSession`/`MigrateLocal`/`SpawnNew`/`c.disabled.SetDisabled` directly.
That branch is replaced with an HTTP call to the loopback daemon, built from
the same primitives `collectClientLocal` already uses:

- `sessionServerConfig("")` (server_client.go:24) resolves the loopback
  `ServerConfig` (host, port, auto-created token) — no servers.yaml lookup.
- `localServerRequestWithTimeout` (server_client.go:46) sends the request,
  with its existing loopback→Tailscale-IPv4 fallback (used for detecting
  the box's own Tailscale address, unrelated to and unaffected by this
  change).
- The endpoints are the ones `remote_actions.go` already calls by name
  (`/sessions/{pid}/kill`, `/sessions/{pid}/migrate`, `/sessions/new`,
  `/sessions/{pid}/disable`, `/sessions/{pid}/tmux-info`,
  `/worktree/remove`) — all already registered in `cmdServer`'s mux
  (server.go:1557-1576). No new server-side endpoints.

This is a **new local mutation path**, not a reuse of `remote_actions.go`'s
functions as-is — those resolve their target by servers.yaml *name* via
`LookupServer`/`remoteRequest`, and local has no name to look up. The shared
piece is the HTTP request/response shape (`actionResult` envelope,
`postDisableRemote`-style JSON marshaling), refactored into a helper
parameterized by `ServerConfig` so both the loopback call and the
named-remote call can use it.

**Attach stays direct — no HTTP, no SSH.** Attaching to a local tmux session
is `tmux attach -t <name>` (or `switch-client`) run directly by the client,
exactly as `runTmuxAttach` (actions.go:276) does today. There is no
"self-flagged server" concept and no SSH involved for the local case; only
the *migrate-before-attach* step (when the session isn't in tmux yet) goes
through the loopback HTTP path described above, since that's a mutation.

**Worktree cleanup on local kill changes source.** Today `actKill`'s local
branch computes the worktree-removal offer client-side via
`localWorktreeCleanupTarget` (actions.go:203) before calling `KillSession`.
Once the kill itself goes over HTTP, the server's response already carries
this decision (`actionResult.Worktree`, the same field
`actKillRemote`/server.go's kill handler already populate) — the local path
adopts that instead of its own client-side `localWorktreeCleanupTarget`
call. This is a **behavior change worth calling out**, not a no-op: the
server re-checks the live session list at kill time rather than the
client's slightly-earlier snapshot, which is strictly more correct (matches
the same freshness argument CLAUDE.md makes for `reattest`), but it means
`localWorktreeCleanupTarget` (actions.go, worktree.go) becomes dead code for
this call site — remove it. Confirm no other caller depends on it first.

### Refuse-loudly on daemon-unreachable

`localServerRequestWithTimeout` returning an error (timeout, connection
refused, non-2xx) for a **mutation** call is a terminal failure for that
action: surface `"local server not reachable — run 'claude-sessions
service install'"` via the same `showActionError`/stderr path each action
already uses for its other failure modes, and stop — no fallback to
`KillSession`/`MigrateLocal`/`SpawnNew`/`DisabledStore` direct calls. This is
deliberately asymmetric with `collectClientLocal`'s read-path fallback (see
Non-goals): a stale read is cheap and self-heals next poll; a mutation
that silently downgrades to the unguarded path is exactly the bug this
design exists to close.

### Scriptable subcommands

`cmdKill`, `cmdMigrate`, `cmdNewLocal` (commands.go:226 — `cmdNew`'s local
branch), and `cmdAttach`'s migrate-first sub-case (commands.go:362) switch
from direct calls to the same loopback-HTTP helper the TUI uses. Same
refuse-loudly behavior on daemon-unreachable. `cmdAttach`'s already-in-tmux
case stays a direct `tmux attach` — same reasoning as the TUI's attach
above.

### Cheap consistency win, same scope

`cmdList` (main.go:105) and the scriptable list output (commands.go:464)
currently call `CollectLocal()` directly, while the TUI already calls
`collectClientLocal()` (tui.go:237) for the equivalent read. That's a real,
pre-existing local/local inconsistency (scriptable output can show a
slightly different snapshot — no server-side `Disabled` overlay applied at
the same layer — than the live TUI on the same box). Both call sites switch
to `collectClientLocal()`. This has no interaction with the refuse-loudly
behavior above since it's a read, not a mutation — it keeps the same silent
fallback `collectClientLocal` already has.

## Data Flow

```
Reads (unchanged):
┌─────────┐  collectClientLocal(): loopback HTTP, silent fallback to direct
│ TUI/CLI │ ───────────────────────────────────────────────────────────────►
└─────────┘

Mutations (this design):
Before:                                   After:
┌─────────┐  direct call                  ┌─────────┐  loopback HTTP only
│ TUI/CLI │ ──────────────────────────►   │ TUI/CLI │ ──────────────────►
│ (local) │  KillSession/MigrateLocal/    │ (local) │  127.0.0.1:8765     │
└─────────┘  SpawnNew/SetDisabled         └─────────┘  (refuse if down)   │
                                                              │           ▼
                                                              ▼    ┌──────────────┐
                                                        ┌──────────────┐  KillSession
                                                        │ -s (this box)│  MigrateLocal
                                                        │ same handlers│  SpawnNew
                                                        │ remote uses  │  DisabledStore
                                                        └──────────────┘
```

## Error and Concurrency Behavior

- Local kill/migrate/spawn/disable now get `sessionIDPrecondition` +
  `reattest` + `spawnDedupe` for free — the exact protection remote already
  has, closing the gap this design exists to close.
- Loopback daemon unreachable during a mutation → refuse loudly, no
  fallback (see above). Reads are unaffected — `collectClientLocal` keeps
  falling back silently, so the TUI still shows sessions with the daemon
  down; only acting on them requires it to be up.
- Local kill's worktree-removal offer now comes from the server's
  `actionResult.Worktree` response, not `localWorktreeCleanupTarget` —
  behavior change noted above, not a no-op.
- Attach for a local session is unaffected except for the migrate-first sub
  case, which now goes through the loopback HTTP path like any other local
  mutation.

## Test Plan

- New helper `localMutateRequest` (exact name chosen in the plan): given a
  fake `ServerConfig`/injectable transport, confirms it builds the same
  request shape `remoteRequest` builds for a named server, just against the
  loopback `ServerConfig`.
- `actKill`/`actAttach`/`actNew`/`actToggleDisabled` local branches: daemon
  reachable → mutation succeeds via HTTP, same `session_mismatch`/`not_live`
  refusals as the existing remote-path tests exercise (reuse or mirror
  `server_test.go` coverage against a real loopback listener in tests).
- Daemon unreachable (test double/closed port): local mutation refuses with
  the "run `service install`" message, does **not** call
  `KillSession`/`MigrateLocal`/`SpawnNew`/`DisabledStore.SetDisabled` — a
  test seam similar to `localServerRequestAttempt`
  (server_client.go:20) that fails the request and asserts the direct
  functions were never invoked (spy/mock on those seams).
- Worktree offer on local kill: comes from the mocked server response's
  `Worktree` field, not from a client-side `localWorktreeCleanupTarget`
  call — assert the latter is not called (or is removed and this is
  moot if dead-code removal lands in the same change).
- Regression: `collectClientLocal`'s existing read-path fallback tests are
  untouched and still pass (this design doesn't touch that function).
- `cmdList`/commands.go:464 switched to `collectClientLocal()`: existing
  tests for scriptable list output still pass, now sourced through the
  server-first path when reachable.

### Verification

```sh
go build ./...
go vet ./...
go test ./...
go test -race ./...
```
