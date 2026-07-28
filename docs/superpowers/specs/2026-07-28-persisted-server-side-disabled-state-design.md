# Persisted Server-Side Disabled State Design

**Date:** 2026-07-28
**Status:** Approved

## Goal

Make the disable/enable flag (`-`/`+`) authoritative on the host that owns
the session, persisted to disk, so it survives both a client restart and a
server (`-s` daemon) restart, and is visible to every client polling that
host — not just the client machine that toggled it.

## History

This flag was server-authoritative once already (`07f6b28`,
`2026-07-22-session-disable-toggle-design.md`): server-memory-only, reset on
restart, deleted on `8f7045f` when session groups were added, in favor of a
simpler client-side-only store (`state.json`, `SessionStore`, see
`2026-07-24-session-groups-design.md`). That trade-off was explicit, not a
bug fix — but it means a session disabled by client A is invisible to client
B viewing the same remote host, and nothing survives a server restart because
nothing was ever written to that host at all. This design restores
server-authoritative state, this time **persisted**, without regressing the
groups feature that shares `state.go` today.

## Non-goals

- `Group` stays client/viewer-side, unchanged (`state.go`, `SessionStore`).
  It's a view-organization concept that can span multiple hosts in one TUI;
  `Disabled` is data about one specific session owned by one specific host.
- No migration of existing `state.json` disabled flags into the new store.
  One-time reset; users re-disable once.
- No central cross-host coordinator. Each host stays independently
  authoritative for its own sessions, matching the existing per-host
  ownership model used by kill/migrate.
- No change to TUI rendering (badge/rail/sort partitioning) — `render.go`
  already treats `Session.Disabled` as authoritative from whatever source
  sets it; only the source changes.

## Architecture

A new dedicated store, **not** a reuse of `SessionStore`/`state.json`.
Reusing `state.json` was considered and rejected: the local TUI process and a
local `-s` daemon on the same host would each hold their own in-memory copy
and do whole-file last-writer-wins saves, so a remote `/disable` write could
be silently reverted by the next local keypress (or vice versa) — a
cross-*process* race a single Go-level mutex cannot fix.

### `DisabledStore` (new `disabled_store.go`)

- On-disk file: `~/.config/claude-sessions/disabled.json` (`ConfigDir()`,
  sibling to `state.json`/`servers.yaml`/`server-token`).
- Shape: `{"sessions": {"<sessionID>": {"disabled": true, "last_seen":
  "RFC3339"}}}` — omit zero fields, same convention as `state.json`.
- Keyed by `SessionID`, never PID — survives PID reuse, resume, migration
  (same reasoning as the original `07f6b28` design).
- **Every mutation takes an OS file lock** (`unix.Flock`, `golang.org/x/sys`
  already a dependency) around a full read-modify-write cycle: open, `LOCK_EX`,
  decode current on-disk contents fresh (never trust in-memory state), apply
  the one change, encode, write, unlock. This is what makes it safe across
  processes on the same host (local TUI + local `-s` daemon, or two `-s`
  processes, however unlikely) — not just across goroutines within one
  process. Linux `flock` locks are per open-file-description, so this also
  correctly serializes concurrent goroutines within the same process; no
  separate in-memory mutex is needed.
- Reads (`Disabled(id)`, `Overlay(sessions)`) `stat` the file's mtime and
  reload only when it changed, avoiding a decode on every render tick /
  every poll while staying correct.
- GC: 30-day retention (`stateMaxAge`, matching `state.go`), pruned at load
  and opportunistically during mutation. Retention is judged against the
  **caller's own currently-observed live-session set**, never a
  client-supplied list: whichever code path calls `Overlay`/`Touch` passes the
  IDs it just collected (local `CollectLocal` result, or the server's own
  `cachedSessions()` result), and any disabled entry present in that set gets
  its `last_seen` touched — throttled to at most once per hour per entry (skip
  the write if `last_seen` is already within the last hour) to bound disk
  churn regardless of poll/render frequency.

## HTTP API

New endpoint, registered alongside kill/migrate in `cmdServer`'s mux:

```
POST /sessions/{pid}/disable
{"session_id": "...", "disabled": true}
```

Both fields **required** (no optional/omittable precondition, unlike kill's
optional `session_id` — a disable write is meaningless without a target
identity). Strict decode: reject unknown fields, explicit `null`, and
trailing content, matching `sessionIDPrecondition`'s discipline (see
CLAUDE.md's "Action preconditions and idempotency"). Malformed body → `400`.

Resolution reuses `s.resolveLivePID` (server.go:558, same helper kill and
migrate use) to map the PID to this host's own current row and produce the
same refusal envelope on a stale precondition:

- `session_mismatch` — that PID is live but is a different session now.
- `not_live` — nothing live there.

Both refusals stay in the existing `actionResult` envelope at HTTP 200, per
the established convention — never a bare 404/409.

On success: `store.SetDisabled(resolvedSessionID, disabled)`, respond with
the authoritative `{"session_id": ..., "disabled": ...}`.

### `GET /sessions`

Overlay `store.Disabled(id)` per session after `cachedSessions()`, on a
**copy** of the cached slice — not in place, since the cache is shared across
concurrent requests and mutating it while another handler encodes it is its
own race. Update the stale comment at server.go:391-393 (it currently claims
this is "entirely client-side" and hardcodes `false`).

## Client Data Flow

### Local sessions

No network hop: the TUI calls `DisabledStore` directly in-process, same file
a local `-s` daemon (if one happens to be running on the same host) also
touches — this is exactly the case the flock exists for.

### Remote sessions

`actToggleDisabled` (actions.go:140-149) gains the same internal branch
`actKill` already uses (actions.go:165-168: `if s.Host != "" { actKillRemote(c); return }`)
— the tui.go dispatch call site (tui.go:561) stays a single, unbranched
`actToggleDisabled(makeCtx())`. New `actToggleDisabledRemote` (remote_actions.go,
alongside `actKillRemote`) POSTs to the row's host, parses the `actionResult`
envelope, reports errors via `showActionError` on failure. No confirmation
dialog — unlike kill, disabling isn't destructive.

After a truthy return, tui.go's `"-"/"+"` case calls `refresh(true)` (kicking
`hub.Refresh()`) instead of only `settleRows()` — matching how kill/attach
already make remote changes appear promptly, accepting the same
eventual-consistency window the codebase already tolerates there (a
poll already in flight can still show stale data briefly).

### Removed

- `OverlayDisabled` calls onto **remote** rows: tui.go's remote-snapshot
  overlay, `main.go:104-115`, `commands.go:470-476` — remote rows now carry
  authoritative `Disabled` over the wire, no client-side overlay needed.
- `Disabled` field and `Disabled()`/`SetDisabled()`/the disabled half of
  `OverlayDisabled()` from `SessionStore`/`state.go` — `Group` is all that
  remains there.

## Error and Concurrency Behavior

- File-lock protected read-modify-write closes the cross-process
  last-writer-wins hole that ruled out reusing `state.json`.
- Unknown/ended PID → `not_live` refusal, no mutation.
- PID reuse changing `SessionID` between selection and write → `session_mismatch`
  refusal, no mutation (same protection as kill/migrate).
- Malformed/trailing body → `400`.
- Failed remote writes leave the row unchanged; error surfaces via
  `showActionError`.
- Local `DisabledStore` failure (unwritable config dir) is best-effort,
  matching `SessionStore.save`'s existing silent-failure convention. A failed
  save must still update the in-memory cache (not just attempt the write and
  give up), and the mtime-based reload check compares against this instance's
  own last-*successfully-observed* mtime — since a failed save leaves the file
  (and thus its mtime) unchanged, the next `Disabled()`/`Overlay()` call sees
  no mtime change and keeps serving the in-memory value rather than reloading
  and silently reverting the toggle within the same process's lifetime. Only
  a config directory that's unwritable for the process's entire lifetime loses
  the flag on restart — an accepted trade-off, not new error-surfacing
  plumbing.

## Test Plan

### `DisabledStore`

- Round-trip set/get, atomic save, GC (30-day expiry, throttled touch).
- Concurrent mutation from multiple goroutines (in-process) is race-free
  (`go test -race`).
- Cross-process: two separate `DisabledStore` instances (simulating TUI +
  server) mutating the same file serialize correctly — neither's write is
  lost (`go test -race`, spawn via `os/exec` or simulate via separate file
  handles in-process).
- mtime-based reload: a write from one instance is visible to another after
  its next `Disabled()`/`Overlay()` call.

### Server

- `GET /sessions` overlays disabled state without mutating the shared cache.
- `POST /sessions/{pid}/disable`: success path, `session_mismatch`, `not_live`,
  malformed/trailing body → `400`, unauthorized preserved.

### Client/actions

- `actToggleDisabled` routes local rows locally, remote rows to
  `actToggleDisabledRemote`.
- Remote failure leaves the row unchanged and surfaces the error.
- `refresh(true)` is invoked after a successful toggle (local or remote).

### Verification

```sh
go build ./...
go vet ./...
go test ./...
go test -race ./...
```
