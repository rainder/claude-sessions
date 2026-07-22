# Session Disable Toggle Design

**Date:** 2026-07-22  
**Status:** Approved

## Goal

Add a `d` hotkey that marks a live session disabled or enabled. Disabled state is authoritative on the server that owns the session, visible to every client polling that server, and held only in server memory.

Disabled sessions remain fully actionable. The flag changes row styling and ordering only.

## Non-goals

- Persist disabled state across server restarts.
- Write disabled state into Claude Code session files.
- Block attach, preview, migrate, or kill actions.
- Add a central coordinator across hosts.
- Automatically launch or supervise the local server.
- Add custom local-server address configuration in the first version.

## Architecture

Each host server is authoritative for disabled state of sessions running on that host. This follows the existing per-host session ownership model and avoids a new global service.

Add a server-derived field to `Session`:

```go
Disabled bool `json:"disabled,omitempty"`
```

`Disabled` is never authoritative in `~/.claude/sessions/*.json`. Session-file decoding explicitly resets this field to `false`, even if persisted JSON contains a `disabled` key. `CollectLocal` therefore returns `false` unless a server annotates the collected sessions.

The server owns:

```go
disabledMu         sync.RWMutex
disabledSessionIDs map[string]struct{}
disabledGeneration uint64
```

State is keyed by a non-empty `SessionID`, not PID, to prevent PID reuse from transferring disabled state to another session. API paths continue to use PID because that matches existing session action endpoints. The server resolves PID to a current live session before reading or changing state. A live row without a stable `SessionID` cannot be disabled: the server returns `409 Conflict`, and clients leave the row unchanged.

After each successful server-side session collection:

1. Capture `disabledGeneration` before collection starts.
2. Collect current live sessions.
3. Build the set of current `SessionID` values.
4. Remove disabled entries absent from the live set only when `disabledGeneration` still matches the captured value.
5. Set `Session.Disabled` from the current registry state.
6. Return annotated sessions.

Each successful write increments `disabledGeneration`. Collection errors do not prune state, and a collection started before a newer write cannot prune that write. Successful current-generation collections bound memory and clear state when a session ends. Restarting the server clears the entire map by design.

## HTTP API

Add an authenticated endpoint:

```text
PUT /sessions/{pid}/disabled
Content-Type: application/json

{"disabled": true, "sessionId": "current-session-id"}
```

Success response:

```json
{"disabled": true, "sessionId": "current-session-id"}
```

Behavior:

- Require both explicit desired state and the selected row's non-empty `sessionId` in the request.
- Resolve PID through a fresh raw session collection. Validation-only `404`/`409` paths do not annotate or prune registry state.
- Return `404` when PID is unknown or session ended.
- Return `409 Conflict` without mutation when PID now resolves to a different `SessionID`; this closes the selection-to-write PID-reuse race.
- Return `409 Conflict` when the live row has no non-empty `SessionID` and therefore cannot be keyed safely.
- Decode `disabled` and `sessionId` as pointer fields so omission is distinguishable from zero values; reject trailing JSON and return `400` for malformed JSON, missing boolean state, or missing/empty identity.
- Reuse existing bearer-token authentication and error conventions.
- Add or remove resolved `SessionID` under the server mutex.
- Return authoritative resulting state and resolved `sessionId`; clients reject a response whose identity differs from the request.

Explicit state makes retries idempotent. Concurrent clients use last successful write. All clients reconcile through normal polling.

`GET /sessions` keeps its existing response shape plus optional `disabled`. Older clients ignore the field; newer clients treat an absent field as `false`.

## Client Data Flow

### Remote sessions

Remote polling already gets sessions from each host server. `FetchRemote` decodes `Disabled` with no separate state request. Pressing `d` sends the update to the selected row's configured remote server.

A successful remote write patches the current hub state immediately. `RemoteHub` snapshots deep-copy both result rows and nested session slices so later patches cannot mutate a previously returned snapshot or race a TUI reader. `RemoteHub` records the poll generation active when the write completed and overlays the desired value only onto results from that generation or older, preventing a pre-write in-flight poll from visually undoing the write. The first successful result from a later poll generation is authoritative and clears the pending overlay even when it contains the opposite value, because another client may have completed a newer write. Error results do not clear the pending overlay; a later successful result does.

### Local sessions

The TUI first requests sessions from the local server at:

```text
http://127.0.0.1:8765
```

It uses the same locally stored bearer token as `claude-sessions --server`. A server bound to `127.0.0.1` or `0.0.0.0` accepts the loopback request. A server bound only to its Tailscale address does not; combined local and remote use should run `claude-sessions --server --bind 0.0.0.0`.

If the local server is unavailable, the TUI falls back to direct `CollectLocal` so listing and existing actions still work. Loopback listing uses a dedicated `750ms` timeout instead of the remote server's `5s` timeout, preventing an unavailable local server from freezing normal TUI refresh. In fallback mode disabled metadata is unavailable, all directly collected rows appear enabled, and pressing `d` reports that the local server is unavailable.

Custom local ports and automatic daemon startup remain out of scope for this version.

### Toggle flow

1. User presses `d` on a session row.
2. Client sends the inverse of displayed `Disabled` plus selected row's `SessionID` to authoritative host.
3. On success, client patches the current row immediately and re-sorts.
4. Existing polling distributes and reconciles the new value across clients.
5. On failure, client leaves the row unchanged and reports the server error through the existing action-error path.

Empty-host placeholder rows ignore `d`.

## TUI Presentation

The selected visual treatment is an accent rail plus muted row.

All session layouts gain a fixed one-character marker column so enabled and disabled rows remain aligned:

- Enabled: blank marker.
- Disabled: amber `│` marker and dimmed row text.

Apply this treatment in full, intermediate, and minimal layouts. Selected-row styling remains dominant so keyboard focus stays clear; the amber rail remains visible. Headless rows already use dim text, so the rail is the distinguishing disabled marker.

Add `d disable/enable` to the footer and help modal.

## Sorting and Selection

Disabled state is a fixed primary partition:

1. Enabled sessions.
2. Disabled sessions.

The active sort field, tie-breakers, and direction apply independently inside each partition. Reversing sort direction never moves disabled rows above enabled rows.

After toggling and re-sorting, selection follows the same session using existing stable selection identity. The selected row may move, but focus must remain on it.

Disabled state does not affect action availability or status ranking inside its partition.

## Error and Concurrency Behavior

- Server map reads and writes are mutex-protected.
- State updates are idempotent explicit writes, not server-side toggles.
- Unknown or ended sessions return `404` without changing state.
- PID reuse that changes `SessionID` between selection and write returns `409` without changing either session.
- Sessions without a stable non-empty `SessionID` return `409` without changing state.
- Failed client updates do not optimistically alter the row.
- Local server failure degrades to direct local collection.
- Remote server failure keeps existing remote snapshot behavior.
- Server restart intentionally resets disabled state.
- A stale entry may exist between collections, but it cannot attach to a reused PID because state is keyed by `SessionID` and updates resolve a live session first.

## Test Plan

### Session and JSON

- Missing `disabled` decodes as `false`.
- `disabled: true` round-trips through server JSON.
- Direct session-file collection ignores persisted `disabled:true` and returns rows enabled.

### Server

- Set and clear disabled state.
- `GET /sessions` annotates matching sessions.
- Ended sessions prune map entries.
- State follows `SessionID`, not reused PID.
- Stale expected `sessionId` after PID reuse returns `409` and does not mutate replacement session.
- Unknown PID returns `404` without changing registry generation or invalidating the existing `/sessions` cache.
- Live session without a stable `SessionID` returns `409` without changing registry generation, creating an empty-key entry, or invalidating the existing `/sessions` cache.
- Malformed or trailing body data returns `400`.
- Unauthorized requests preserve existing behavior.
- Concurrent reads and writes remain race-free.

### Client and actions

- `d` routes local rows to loopback server.
- `d` routes remote rows to configured host server.
- Empty-host rows are no-ops.
- Failed updates leave displayed state unchanged.
- Local server outage falls back to direct collection after bounded `750ms` loopback timeout.
- Remote decoding preserves disabled state.
- Returned `RemoteHub` snapshots do not alias hub-owned nested session slices and remain race-free during patch/store activity.
- A pre-write in-flight poll cannot visually undo a successful remote toggle.
- First successful post-write poll is authoritative, clears pending state, and allows a newer write from another client to win.
- Remote poll errors retain pending state; a successful response that omits the session clears it.

### Sorting and rendering

- Enabled rows precede disabled rows for every sort field.
- Both sort directions preserve disabled-last partitioning.
- Existing tie-breakers remain stable inside partitions.
- Selection follows toggled row after it moves.
- Full, intermediate, and minimal layouts render aligned marker columns.
- Disabled rows show amber rail and dim text.
- Selected disabled rows keep clear focus styling.
- Footer and help include `d`.

### Verification

```sh
go test ./...
go test -race ./...
go vet ./...
go build .
```
