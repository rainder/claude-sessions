# Thread Mutation Preconditions Design

**Date:** 2026-07-29
**Status:** Approved

## Goal

Close the actual TOCTOU/duplicate-spawn gap in kill, migrate, and spawn — for
both local and remote sessions — by having every call site supply the
precondition fields the server (and, for local, a new equivalent in-process
check) already knows how to use, but that no caller currently sends.

## History

This replaces `2026-07-29-servers-only-client-design.md` (status: REJECTED,
see that file for the full account). That design proposed routing local
mutations through the loopback HTTP daemon so they'd inherit
`sessionIDPrecondition`/`reattest`/`spawnDedupe`. An `ask-codex` review — verified
directly against the code before being accepted — found the premise false:
those guards fire only when a caller supplies `session_id`/`request_id`, and
the *existing* remote call sites never do (`actKillRemote`, `actAttachRemote`'s
migrate call, `actNewRemote`, `cmdNewRemote` all send bare `{}` bodies today).
Remote is exactly as unguarded as local — there was no local-vs-remote gap to
close by rerouting. `disableSession`/`postDisableRemote` is the control case
proving the guard works when used: `decodeDisableRequest` makes `session_id`
mandatory, so disable is already race-safe on both paths, unchanged by this
design.

## Non-goals

- No change to how local sessions are collected or mutated architecturally —
  `KillSession`, `MigrateLocal`, `SpawnNew` stay direct, in-process calls.
  Nothing routes through HTTP for local.
- No change to `disableSession`/`postDisableRemote`/`actToggleDisabled` — already
  correct.
- No change to servers.yaml, `ServerConfig`, or any server-side handler
  (`kill`, `migrate`, `newSession` already support the fields this design
  starts sending — see server.go:894 `sessionIDPrecondition`, server.go:1042
  `RequestID`).
- No local equivalent of `spawnDedupe` — see Architecture below for why
  local spawn has no analogous race to close.
- The loopback-rerouting idea for local/remote code de-duplication is not
  part of this design. It may be revisited later, on its own architectural
  merits, but only after resolving the `--bind tailscale` dual-listener
  collision this session's review surfaced (`paste.go:446-461`) — it is not
  a race fix and must not be justified as one again.

## Architecture

### Remote: send the fields the server already accepts

`actKillRemote` (remote_actions.go:259) and `actAttachRemote`'s migrate call
(remote_actions.go:404, the `/sessions/%d/migrate` request) currently send
literal `[]byte("{}")`. Both change to send `{"session_id": s.SessionID}`
when `s.SessionID != ""` (falling back to `{}` — today's exact behavior — when
it's empty, matching `sessionIDPrecondition`'s own "absent means no
precondition" contract; not introducing a new empty-ID policy). This makes
`reattest` (server.go:618) actually fire for the TUI's remote kill/migrate,
closing the real window: a confirmation dialog can sit open for an arbitrary
time, and the row's tmux pane can be recycled to a different session in that
window.

`actNewRemote` (remote_actions.go:483) and `cmdNewRemote` (commands.go:272)
gain a `request_id`: a fresh random ID generated once per spawn attempt,
included in the JSON body's `request_id` field (already accepted, currently
optional and unused by these two callers — server.go:1042). New helper
`newSpawnRequestID()` (crypto/rand, hex-encoded, sized to satisfy
`validSpawnRequestID`'s 8-128-char `[A-Za-z0-9_-]` requirement — the
existing `randomSlug()` in migrate.go is only 6 hex chars, too short, and is
for tmux-name suffixes, a different purpose; this gets its own helper rather
than widening that one's contract).

### Local: an in-process equivalent of `reattest`

Local kill/migrate never touch the network, so there is no server-side
precondition to thread a field into — `reattest` itself needs an in-process
analog. New helper (`local_reattest.go` or added to `migrate.go` — decided at
plan time by whichever keeps the file focused):

```go
// localReattest re-reads the session file for pid immediately before a
// destructive local action and compares it against the session identity the
// caller last observed. Mirrors reattest (server.go:618) — same rationale,
// same two refusal cases, just in-process instead of over HTTP: the
// snapshot the caller is acting on can be however old the confirmation
// dialog took to resolve, and the PID can have been recycled in that
// window. An empty wantSessionID skips the check — matches
// sessionIDPrecondition's own "absent means no precondition" contract,
// not a new policy invented for this path.
func localReattest(pid int, wantSessionID string) error {
	if wantSessionID == "" {
		return nil
	}
	sess, ok := readSessionByPID(pid)
	if !ok {
		return fmt.Errorf("PID %d is not a live Claude session", pid)
	}
	if sess.SessionID != wantSessionID {
		return fmt.Errorf("PID %d is a different session now", pid)
	}
	return nil
}
```

Called immediately before `KillSession`/`MigrateLocal` in all four local call
sites: `actKill` (actions.go:165), `actAttach`'s migrate-first branch
(actions.go:235), `cmdKill` (commands.go:42), `cmdMigrate` (commands.go:126).
Each already holds the session snapshot it selected/read earlier
(`s`/`sess`), so this is a one-line insertion per site, not a signature
change to the four functions themselves.

### Why local spawn needs no equivalent

`spawnDedupe`/`request_id` protects against a **retried** spawn request
racing the original — the scenario is a client that times out waiting for a
slow spawn and retries, while the first attempt is still in flight
server-side, risking two tmux sessions for one user intent. Local `SpawnNew`
is a single synchronous in-process call with no network hop and no retry
loop calling it twice — there is no analogous "two requests for one intent"
race to protect against locally. `cmdNewLocal`/`actNew`'s local branch stay
unchanged.

## Error and Concurrency Behavior

- Local kill/migrate: `localReattest` runs immediately before the
  destructive call, using the freshest available read
  (`readSessionByPID`, a single file read — not the poll-interval-stale
  in-memory `local []Session` slice `s`/`sess` originally came from).
  Mismatch or gone → the action reports the failure via its existing
  error-display path (`showActionError`/stderr) instead of calling
  `KillSession`/`MigrateLocal`. Message wording matches `reattest`'s own,
  for consistency across local and remote refusals.
- Remote kill/migrate: unchanged wire behavior for a server that ignores an
  unrecognized-but-optional field is impossible here — `session_id` is
  already a supported, decoded field; sending it merely activates an
  existing code path (server.go:894-899, `resolveLivePID`/`reattest`) that a
  request without it already skips today. No server-side change.
- Remote spawn: a request without `request_id` keeps today's behavior
  exactly (server.go:1042's `if body.RequestID == "" { ... spawn
  unconditionally ... }`); adding one activates the existing dedup/replay
  ledger (`s.spawns`, already implemented and tested — server_test.go), not
  new server logic.
- No change to the empty-`SessionID` edge case's behavior on either path —
  it already means "no precondition," before and after this design.

## Test Plan

- `localReattest`: matching PID+SessionID → nil error. PID no longer live →
  `not_live`-style error. PID live but different SessionID (simulating pane
  reuse) → `session_mismatch`-style error. Empty `wantSessionID` → nil
  (skip), even for a PID that doesn't exist — matches the "no precondition"
  contract exactly, not a new behavior to special-case.
- `actKill`/`cmdKill`/`actAttach`'s migrate branch/`cmdMigrate`: each calls
  `localReattest` before its destructive call; a stubbed/injected mismatch
  prevents `KillSession`/`MigrateLocal` from being called at all (test seam:
  package-level `KillSession`/`MigrateLocal` are plain functions already, so
  this may need a small test seam — e.g. a swappable var — decided at plan
  time; do not skip this assertion by only checking the error message, the
  point is the destructive call must not fire).
- `actKillRemote`/`actAttachRemote`'s migrate call: request body includes
  `session_id` equal to the selected session's `SessionID` when non-empty,
  and omits/empties it when the snapshot's `SessionID` is empty (existing
  behavior preserved for that edge case).
- `newSpawnRequestID`: returns a string satisfying `validSpawnRequestID`
  (8-128 chars, `[A-Za-z0-9_-]`) on every call; two calls return different
  values.
- `actNewRemote`/`cmdNewRemote`: request body includes a `request_id` field
  satisfying `validSpawnRequestID`.
- Regression: `postDisableRemote`/`actToggleDisabled`/`decodeDisableRequest`
  are untouched by this design — no test changes there.

### Verification

```sh
go build ./...
go vet ./...
go test ./...
go test -race ./...
```
