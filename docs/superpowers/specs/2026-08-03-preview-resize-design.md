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
  revertTmuxTarget(location string) error                   // tmux set-window-option -u window-size

Local caller:  tui.go inspector-open/close hooks → resolveLivePIDLocal → resizeTmuxTarget / revertTmuxTarget directly
Remote caller: remote_actions.go → POST /sessions/{pid}/resize → server.go handler → same primitives, server-side
```

**Correction from an independent review (Codex), verified empirically against a
real tmux session:** `resize-window -A` does **not** actually revert anything —
it recalculates the window's size once, but leaves the `window-size` **window
option** explicitly set to `manual`. A window in `manual` mode never again
auto-adjusts to an attaching client's terminal size, so a session "reverted"
this way stays silently frozen at whatever the last preview requested, forever
(verified: `tmux show-window-options -t <target> window-size` reports `manual`
even after `-A`). The actual fix is `tmux set-window-option -t <target> -u
window-size` — verified this correctly *unsets* the window-level override, so
the window falls back to the global default (typically `latest`, i.e. tracks
whichever client last interacted with it) and a subsequent real attach behaves
exactly as if this feature had never touched it.

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
- **Attach-before-revert ordering (Codex-caught bug, fixed here):** the
  Enter-to-attach flow (`tui.go:900-903`, `attach(); closeInspector()`) used to
  call the blocking interactive `attach()` **before** `closeInspector()`'s
  revert — meaning the entire real, interactive attach session ran while the
  window was still pinned at the previewer's dimensions, not the attacher's.
  The revert must fire **before** the interactive attach begins, not after, so
  the real attach sees a normally-behaving (unpinned) window from the start.
- **Quit-path safety net (Codex-caught gap, fixed here):** quitting the TUI
  outright while the inspector is open (Ctrl+D, `q`/`commandQuit`) previously
  returned straight out of the event loop without ever calling
  `closeInspector`, so the revert never fired — not a rare crash edge case, a
  normal and common path. `RunTUI` now reverts any still-open inspector target
  via a top-level `defer` alongside its other hub-shutdown defers, so every
  return path — quit, error, or otherwise — reverts before the process exits.
  A hard kill (SIGKILL) or genuine crash is still unrecoverable and stays an
  accepted limitation (see below) — a `defer` cannot run past that.
- Both the entry resize and the exit revert are **one-shot** per open/close.
  No repeat on inspector poll ticks, and no live re-resize if the viewer's
  terminal itself resizes while preview stays open.
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
  behavior) until `set-window-option -u window-size` reverts it (see the
  Architecture section above for why `resize-window -A` alone does not). A
  hard process kill (SIGKILL, `kill -9`, power loss) between entry and exit
  bypasses even the `defer`-based safety net described under Lifecycle hooks,
  so that session stays pinned until someone manually reverts or resizes
  again. No extra mitigation beyond that `defer` — same "narrow the race,
  don't guess" approach already used for kill/migrate preconditions elsewhere
  in this codebase.
- **Whole-window blast radius**: `resize-window` resizes the entire tmux
  window, not a single pane — every pane in that window is relaid out, and if
  the window happens to be linked into more than one session (or has more
  than one pane), other viewers of it are affected too, not just the pane
  `captureTmuxPreview` is reading. In this codebase's normal usage every
  Claude session occupies its own single-pane window (`tmux new-session`
  spawns claude as the pane's sole foreground process — see this repo's
  CLAUDE.md), so window and pane are practically the same thing for the
  sessions this feature targets. A manually split or multi-linked window is
  not guarded against.
- **Concurrent real attach**: if a real user is already attached to the
  target session when preview opens, resizing will visibly disrupt their
  session. This design does not skip resizing in that case; revert-on-exit is
  the mitigation, not prevention. (`Session.TmuxAttached`, tmux.go, could
  detect this and skip the resize entirely — considered out of scope for this
  iteration; worth revisiting if the disruption proves noticeable in
  practice.)

## Testing plan

- Unit tests for `resizeTmuxTarget`/`revertTmuxTarget` against a fake tmux
  command runner (injectable seam, not the real `tmux` binary — same style as
  `keychainRead`/`keychainWrite`).
- Server handler test: `s.resizeFn` seam, body decode/validation,
  `resolveLivePID` failure paths (400/409-style codes).
- Manual check: open preview on a remote session from a narrow and a wide
  terminal, confirm pane width changes; exit and confirm
  `set-window-option -u window-size` fired (via `tmux show-window-options -t
  <target> window-size` reporting nothing/inherited, not `manual`) and that a
  real attach afterward sees a normally-behaving (unpinned) window.
- Manual check: quit the TUI outright (Ctrl+D) while the inspector is open on
  a session, then confirm that session's window was still reverted (the
  `defer`-based safety net fired).
- Manual check: attach to the previewed session via Enter and confirm the
  attach sees an unpinned window from the very start of the interactive
  session, not just after detaching.
