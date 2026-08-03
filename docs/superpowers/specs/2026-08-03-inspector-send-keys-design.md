# Inspector send-keys: freeform text into a live session's tmux pane

Date: 2026-08-03

## Problem

The fullscreen inspector (`inspector.go`, opened with `h`/`p`, aka "preview
mode") is read-only: it polls a session's tmux pane and renders it, but there
is no way to act on what you see there short of leaving the inspector,
attaching, and typing. The user wants a fast in-place way to send a line of
text — a follow-up prompt, a `y`, a slash command — into the pane they're
already looking at, without leaving the inspector, for both local sessions
and sessions on a remote host.

## Scope

- Inspector screen only (not the session list).
- Single-line compose (Enter submits; no multiline/paste-buffer editing).
- Local and remote sessions both supported, symmetric dispatch.
- Sessions with no tmux pane (`Session.Tmux == ""`) cannot compose — nothing
  to send keys into.

Out of scope: multiline input, an undo/history of sent lines, sending to
multiple sessions at once.

## Design

### Compose key: `i`

Space was the first candidate but is already bound to page-down in the
inspector (`tui_state.go:398`, `case " ", KeyPageDown: s.inspector.page(1)`).
`i` (unused, vim convention for entering text-input mode) opens the compose
box instead. Disabled (no-op, dim hint shown) when `session.Tmux == ""`.

### State

`inspectorViewState` (inspector.go) gains:

```go
composing           bool
composeText         string
composeStatus       string    // transient feedback, e.g. "sent" or an error
composeStatusUntil  time.Time
```

### Key routing

`handleInspectorEvent`/`handleInspectorKey` (tui.go, tui_state.go) branch on
`composing`. While composing, keystrokes feed the compose buffer instead of
dispatching to the inspector's existing scroll/kill/refresh/follow bindings —
same byte-level edit rule as `newPickerState.handlePrompt` (new_picker.go:101):
single-byte printable ASCII (0x20–0x7e) appends, `\x7f`/`\x08` (DEL/BS)
backspaces, Enter submits, Esc cancels-and-discards.

Ctrl-C while composing cancels the compose (does not quit the app), mirroring
`handlePrompt`'s own Ctrl-C-cancels-picker behavior. Ctrl-D still quits.

**Batch safety**: one read of stdin can decode into multiple queued
`inputEvent`s (fast typing, a paste). The compose handler must drain every
event already in the current batch while `composing` is true, so that a
paste containing `\n` submits on the embedded newline and does *not* let
trailing bytes (e.g. `kq` after the newline) fall through to the inspector's
normal key dispatch and trigger kill/back.

### Sending

`sendKeys(s Session, text string) error` issues **two** `tmux send-keys`
calls, matching the existing `tmuxSendLiteral` helper (paste.go:403-405):

```go
tmux send-keys -t <pane> -l <text>
tmux send-keys -t <pane> Enter
```

The `-l` flag is required: without it tmux parses `text` as key names, so a
user typing `Enter` or `C-c` as message content would trigger that tmux
action instead of sending the literal string. Both calls run via
`exec.Command` with argument slices (no shell), so no shell-injection
surface regardless of message content.

### Attestation

Both local and remote paths must resolve a **fresh** pane target, not the
inspector's stale snapshot. `session_id`-only reattestation
(`localReattest`/`reattest`) is insufficient here: it re-checks
`Session.SessionID` against the on-disk session file, but never recomputes
`Session.Tmux` — a value derived at collection time, not stored on disk. A
session recycled onto a different pane between the inspector snapshot and
the send would still validate under the old (wrong) `Tmux`.

Both paths therefore go through a fresh **collect-then-attest**, the same
`resolveLivePID` shape kill already uses (server.go:1148):

- Local: re-collect this host's sessions, find `pid`, compare `SessionID`,
  use *that* row's `Tmux`.
- Remote: `resolveLivePID(pid, wantSession)` server-side, same as kill/migrate.

### Remote endpoint

New authed endpoint, same shape as kill/migrate:

```
POST /sessions/{pid}/send-keys
{"session_id": "<uuid>", "text": "<line>"}
```

`session_id` is **required** here (400 if empty/missing) — unlike kill/
migrate's legacy fail-open (empty session_id = "no precondition", kept for
callers that predate the guard), there is no legacy caller for this new
endpoint to stay compatible with, so there's no reason to allow an
unguarded send. The body decoder cannot reuse `sessionIDPrecondition`
unmodified (`DisallowUnknownFields` would reject the new `text` field) — a
sibling decoder is needed for `{session_id, text}` with `session_id`
required and `text` bounded (reject empty, embedded CR/LF/NUL; cap length).

Handler: decode → `resolveLivePID(pid, sessionID)` → refuse on mismatch/
not-live (`codeSessionMismatch`/`codeNotLive`, same envelope kill/migrate
use) → `sendKeys(target, text)` → `actionResult{OK: true}`.

Client: `sendKeysRemote(host string, pid int, sessionID, text string)
(actionResult, error)` in remote_actions.go, same shape as `killRemote`
(remote_actions.go:426) — marshal body, POST via `remoteRequest`, unmarshal
`actionResult`.

### Dispatch

Inspector picks local vs remote the same way every other action already
does: `target.Host != ""` routes to `sendKeysRemote`; empty routes to the
local `resolveLivePID` + `sendKeys` path.

### Feedback

On submit, the inspector shows a "sending…" indicator immediately — a
remote POST can take up to `remoteRequest`'s existing ~30s timeout, and the
fullscreen inspector must not look frozen for that window.

On success: `composeStatus = "sent"`, `composeStatusUntil = now + 4s`,
rendered in the inspector's own footer (it has its own render path,
`RenderInspector`/render_inspector.go, separate from the session list's
existing bottom-row toast — so this is a new, inspector-scoped status line,
not a reuse of `tui.go`'s `toast`/`toastUntil`). `composeStatusUntil` must
feed the event loop's wait/timeout calculation the same way the session
list's toast deadline already does (tui.go:470-476), so the message clears
on schedule without waiting for unrelated input. No forced immediate hub
refresh — `InspectorHub`'s existing poll interval picks up the pane's
updated content on its normal cadence.

On failure: `composeStatus = <error text>`, and **`composeText` is restored
(not discarded)** so the user can correct and resend without retyping.

### No-tmux sessions

`i` no-ops when `session.Tmux == ""`; a dim hint (e.g. "no tmux pane")
explains why, so the user isn't left typing into a compose box that can
never send.

## Testing

- `tui_state_test.go`: compose key routing — arm on `i` (only when
  `Tmux != ""`), backspace/append edit rules, Enter submits, Esc/Ctrl-C
  cancel-and-discard, and the batch-drain rule (multiple queued events in
  one decode, embedded newline submits without leaking trailing keys to
  kill/back dispatch).
- A `sendKeys` test (co-located with `paste_test.go`'s `tmuxSendLiteral`
  coverage or new): asserts the two-call `-l` + `Enter` shape.
- `server_test.go`: new endpoint — missing/empty `session_id` rejected
  (400), mismatch/not-live refusals return the existing envelope codes,
  success path returns OK.
- `remote_actions_test.go`: `sendKeysRemote` request marshaling and
  response unmarshaling, mirroring `killRemote`'s existing test.

## Design review

Reviewed via `/ask-codex` before this document was written. First pass
returned DISAGREE (verified against the actual code, all citations
confirmed correct) on an earlier draft that: bound the compose key to
Space (already taken by page-down), sent text via unflagged
`tmux send-keys` (unsafe for text matching tmux key names — no `-l`),
reattested identity without refreshing the stale `Tmux` pane target, and
would have reused `sessionIDPrecondition`'s `DisallowUnknownFields` decoder
verbatim for a body it doesn't fit. All four are incorporated into this
revision as described above.
