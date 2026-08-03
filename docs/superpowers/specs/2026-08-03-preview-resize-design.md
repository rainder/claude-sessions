# Preview-mode tmux resize — design

## Purpose

When the user opens inspector/preview mode ('p' / arrow-select → inspector) on a
session, the previewed content is captured via `tmux capture-pane` at whatever
size the target tmux window happens to be — which is often not the size of the
viewer's own terminal. This causes wrapped or truncated lines that don't match
what the viewer can actually see.

This feature resizes the previewed session's tmux window to match the
inspector's available viewport before capturing, for both local and remote
targets, and reverts the resize when the user leaves preview mode.

## Scope

Applies to **both local and remote** preview targets — one shared code path,
not a remote-only special case.

## Architecture

One shared primitive, two callers (local direct, remote over HTTP) — mirrors
the existing `send_keys.go` pattern (primitive + local caller + server
endpoint + remote client call).

```
resize.go (new)
  resizeTmuxTarget(location string, cols, rows int) error   // tmux resize-window -x <cols> -y <rows>
  revertTmuxTarget(location string) error                   // tmux resize-window -A

Local caller:  tui.go inspector-open/close hooks → resolveLivePIDLocal → resizeTmuxTarget / revertTmuxTarget directly
Remote caller: remote_actions.go → POST /sessions/{pid}/resize → server.go handler → same primitives, server-side
```

`location` reuses the pane target string `preview.go`'s `walkTmuxPane` already
resolves (e.g. `session:window.pane`) — no new pane-resolution logic is
needed; it's the same lookup preview capture already performs.

## Remote endpoint

`POST /sessions/{pid}/resize`

Body: `{"session_id": "...", "cols": N, "rows": N, "revert": bool}`

Mirrors `sendKeysBody`'s dedicated-reader pattern. `session_id` is
**required** (no legacy bare-`{}` compatibility — this is a new endpoint,
same reasoning as send-keys: there is no pre-existing caller to stay
compatible with).

- `revert: false` (preview entry) → resolve live PID (`resolveLivePID`,
  single fresh check — same low-stakes reasoning as send-keys, no extra
  `reattest`) → `resizeTmuxTarget(loc, cols, rows)`.
- `revert: true` (preview exit) → resolve live PID → `revertTmuxTarget(loc)`,
  `cols`/`rows` ignored.
- Injectable `s.resizeFn` seam for tests, same shape as `s.sendKeysFn`.
- Errors: `400` bad body / bad pid; `409`-style `not_live` / `session_mismatch`
  codes reused from the existing `resolveLivePID` failure shape.

## Local path

Same lifecycle, no HTTP hop: `tui.go`'s inspector-open/close resolves the
target via `resolveLivePIDLocal` (fresh `CollectLocal()` + `SessionID` check,
same pattern already used by send-keys' local path) and calls
`resizeTmuxTarget`/`revertTmuxTarget` directly.

## Lifecycle hooks

- **Entry** (opening inspector/preview for a target, local or remote):
  compute inner-viewport cols/rows — terminal size minus inspector chrome
  (reuse the arithmetic `inspectorViewState` already computes for its own
  layout; local terminal size comes from `term.GetSize`, same source already
  used for local rendering elsewhere in the TUI) — then fire resize once.
- **Exit** (leaving inspector back to the main list): fire revert once.
- Both are **one-shot** per open/close. No repeat on inspector poll ticks, and
  no live re-resize if the viewer's terminal itself resizes while preview
  stays open.
- A resize/revert failure is logged and non-fatal: preview content still
  renders via `capture-pane` regardless of pane size, so this is a
  best-effort enhancement, never a blocker to entering or exiting preview.

## Sizing

Target size is the inspector's **inner viewport** (terminal size minus
borders/header/footer chrome), not the raw full-terminal size — so wrapped
lines line up exactly with the space actually visible inside the inspector,
with no wasted or cut-off width.

## Known limitations (accepted, not solved by this design)

- **Scrollback rewrap**: tmux does not retroactively rewrap already-buffered
  scrollback when a pane is resized — only content written *after* the resize
  reflows to the new width. Immediately after entering preview, older lines
  in the pane's history may still show wrapped at whatever width they were
  originally written; only new output benefits from the resize.
- **Manual-size-mode pinning**: `tmux resize-window` pins the window to manual
  sizing (blocking tmux's normal auto-resize-to-largest-attached-client
  behavior) until `resize-window -A` reverts it. If the process crashes
  between entry and exit (no revert fires), that session stays pinned until
  someone manually reverts or resizes again. No extra mitigation beyond the
  one-shot revert-on-exit hook — same "narrow the race, don't guess" approach
  already used for kill/migrate preconditions elsewhere in this codebase.
- **Concurrent real attach**: if a real user is already attached to the
  target session when preview opens, resizing will visibly disrupt their
  session. This design does not skip resizing in that case; revert-on-exit is
  the mitigation, not prevention.

## Testing plan

- Unit tests for `resizeTmuxTarget`/`revertTmuxTarget` against a fake tmux
  command runner (injectable seam, not the real `tmux` binary — same style as
  `keychainRead`/`keychainWrite`).
- Server handler test: `s.resizeFn` seam, body decode/validation,
  `resolveLivePID` failure paths (400/409-style codes).
- Manual check: open preview on a remote session from a narrow and a wide
  terminal, confirm pane width changes; exit and confirm `resize-window -A`
  (or a real attach) shows normal auto-resize behavior restored.
