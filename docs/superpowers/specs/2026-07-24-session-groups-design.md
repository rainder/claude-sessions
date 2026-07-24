# Session groups + client-side persisted state

Approved design, 2026-07-24.

## Goal

Assign sessions to numbered groups (1–9) from the TUI, filter the view to one
group, and persist both group assignments and the disabled flag on the client
machine so restarts don't clear them. Disabled-session state moves fully
client-side; the server's disabled endpoint and annotation machinery are
deleted.

## Keys (top-level TUI, all view modes)

- `1`–`9`: filter view to group N. Same digit again clears the filter; `0`
  clears too.
- `!@#$%^&*(` (Shift+1–9, US layout): set the selected session's group to
  1–9. Pressing the session's current group ungroups it. Single membership —
  assigning replaces any previous group.
- Sessions with an empty `SessionID` cannot be assigned (ignored).
- `-`/`+` disabled toggle keeps its binding, now backed by the client store
  (no HTTP).

## Client state store (new `state.go`)

- Path: `~/.config/claude-sessions/state.json` (same dir as `servers.yaml`).
- Shape: `{"sessions": {"<sessionID>": {"group": 3, "disabled": true,
  "last_seen": "RFC3339"}}}` — omit zero fields.
- Loaded once at TUI start; saved atomically (temp file + rename) on every
  mutation. `last_seen` refreshed for currently-visible sessions when saving.
- GC on load: drop entries with neither group nor disabled set, and entries
  whose `last_seen` is older than 30 days.
- Keyed by SessionID: survives pid churn, resume, and cross-host migration.
- Only the TUI goroutine touches the store (single-threaded event loop).
  Two concurrent TUIs: last-write-wins, accepted.

## Disabled moves client-side

- TUI overlays `Disabled` from the store onto locally collected and
  remotely fetched sessions each frame; any server-reported value is
  overwritten by the overlay.
- Delete: the server disabled PUT endpoint, `disabledSessionIDs` /
  `annotateDisabled` / `disabledGeneration` in server.go, the disabled PUT
  plumbing in server_client.go, and the `pendingDisabled` override machinery
  in remote.go.
- `actToggleDisabled` becomes a store write plus in-memory patch — instant.
- Semantics are per-client-machine (accepted trade-off).
- The scriptable `list` subcommand loads the store read-only and overlays
  disabled the same way; groups don't change its output.

## Badge rendering

- A 2-char badge slot (`①` + space) sits between the tmux-viewer symbol and
  the disabled rail, colored by group with a fixed palette:
  1→36 2→35 3→33 4→32 5→34 6→31 7→96 8→95 9→97 (SGR codes).
- Circled digits U+2460–U+2468. Ambiguous-width caveat accepted.
- The slot is reserved only when ≥1 visible session has a group, so the
  ungrouped layout stays as narrow as today; layout shifts 2 cols when the
  first badge appears.
- Dim/selected rows follow the existing decorateSessionRow paths (pad and
  color interplay consistent with highlightSelectedRow's reset replacement).

## Filter semantics

- Filter applies across all hosts/sections; sections render normally with
  rows filtered, using the existing empty-host row when nothing matches.
- Header shows a colored `group ③` indicator while a filter is active.
- Active filter is runtime-only (not persisted); restart shows all.
- When the selected row is filtered out, selection falls back via existing
  machinery.

## Tests

- Store: round-trip, atomic save, GC (empty entries, 30-day expiry).
- Render: badge presence/absence and colors, layout shift only when a group
  is visible, filter rendering incl. empty-host rows, header indicator.
- Actions: assign/replace/ungroup toggle, disabled overlay replacing
  server-reported values.
- All existing tests keep passing; `go test -race ./...`, `go vet`, gofmt.
