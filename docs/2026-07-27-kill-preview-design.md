# Preview snapshot in the kill confirmation dialog

Date: 2026-07-27
Status: design, approved for planning

## Problem

Pressing `k` in the TUI opens a centered confirm box asking "Kill session
48221 (tmux: DR-860)?". The box says nothing about what that session is
*doing*. Deciding whether to kill it means cancelling the dialog, opening the
inspector, reading, and starting over — or killing blind and hoping it wasn't
mid-compaction.

The data is already available and cheap: `LoadPreview` (preview.go:62) grabs a
live `tmux capture-pane` snapshot, falling back to a transcript tail.

## Goal

Show the last handful of lines of the session's pane inside the kill
confirmation box, so the decision is made with the session's current output in
view.

Non-goals: scrolling, live refresh, previews on unrelated confirms.

## Scope

Preview appears on:

- `actKill` — local `k` (actions.go:184)
- `actKillRemote` — remote `k` (remote_actions.go:266)

Preview does **not** appear on:

- the second, worktree-removal confirm inside `actKill` (actions.go:202) — by
  then the session is dead and there is nothing to preview
- the attach "migrate to tmux?" prompt (actions.go:237)
- any other `confirmOverlay` caller

## Design

### Overview

```
┌──────────────────────────────────────────────────┐
│ trecs-brain:DR-860 · pid 48221            tmux   │
│ ──────────────────────────────────────────────── │
│  > running tests...                              │
│    PASS app/src/lib/foo.test.ts                  │
│    PASS app/src/lib/bar.test.ts                  │
│                                                  │
│  ● Compacting conversation… (esc to interrupt)   │
│ ──────────────────────────────────────────────── │
│ Kill session 48221 (tmux: DR-860)?               │
│                                                  │
│ [y] yes   [n] no                                 │
└──────────────────────────────────────────────────┘
```

One box. Title row (session identity, dim source marker right-aligned),
divider, preview rows, divider, then today's existing question + blank +
`confirmHint` footer unchanged.

### Component boundaries

New file `kill_preview.go` owns the preview lifecycle. `confirm_overlay.go`
gains rendering of an optional preview block. Neither file learns anything
about tmux, HTTP, or sessions — `kill_preview.go` calls the existing
`LoadPreview` / `fetchRemotePreview` primitives and hands `confirm_overlay.go`
a plain snapshot struct.

```go
// kill_preview.go

// overlayPreview is an immutable snapshot handed to the renderer.
type overlayPreview struct {
    Title  string   // "trecs-brain:DR-860 · pid 48221" (host-prefixed when remote)
    Source string   // "tmux" | "transcript" | "" while loading
    Lines  []string // sanitized pane lines, oldest first, not yet clipped
    Err    error    // fetch failure; Lines is nil
    Loaded bool     // false => still in flight
}

// previewPane owns the in-flight fetch and the wake pipe the modal selects on.
type previewPane struct { /* mu, snap overlayPreview, wakeR/wakeW *os.File, done chan struct{} */ }

func startLocalKillPreview(s Session) *previewPane
func startRemoteKillPreview(s Session) *previewPane
func (p *previewPane) snapshot() overlayPreview
func (p *previewPane) wake() wakeFD
func (p *previewPane) close()
```

`confirmOverlay`'s existing signature is **unchanged** — three other call sites
(actions.go:237, remote_actions.go:291, remote_actions.go:357) keep compiling
untouched. A sibling entry point takes the pane:

```go
// confirm_overlay.go
func confirmOverlayPreview(question string, p *previewPane, wakes []wakeFD) bool
func renderConfirmOverlay(question string, prev *overlayPreview, cols, rows int) string
```

`confirmOverlay` becomes a one-line wrapper: `confirmOverlayPreview(question,
nil, wakes)`. A nil pane renders byte-for-byte what renders today.

### Data flow

```
actKill
  │
  ├─ p := startLocalKillPreview(s)      ──►  goroutine: LoadPreview(pid, killPreviewLimits)
  │  defer p.close()                              │      (remote: fetchRemotePreview(host, pid, …))
  │                                               ▼
  │                                    p.mu: store snapshot; write 1 byte to wakeW
  │                                               │
  └─ confirmOverlayPreview(q, p, wakes)           │
        loop:                                     │
          snap := p.snapshot()                    │
          renderer.Draw(renderConfirmOverlay(q, &snap, cols, rows))
          readModalEvents(dec, append(wakes, p.wake()))  ◄── wake fires, re-render
```

The fetch never blocks the dialog. The box appears immediately with a
`loading preview…` placeholder and grows when the snapshot lands.

### Sizing

Box width is **target-driven, not content-driven**. Today `innerWidth` is the
max `visualLen` across question and hint lines (confirm_overlay.go:53-57). That
stays exactly as-is when no preview block renders. When one does:

```
innerWidth = clamp(max(existingInnerWidth, 72), 1, cols-4)
```

The 72-column floor applies **only** when a preview block is actually rendered
— a nil pane, or a terminal too short for `previewRows > 0`, keeps today's
content-sized width. Otherwise the three untouched callers and the short-
terminal fallback would silently get wider boxes.

Preview lines never participate in the width calculation. A 200-column pane
capture gets clipped to the box, not the reverse.

Height:

```
previewRows = clamp(rows-12, 0, 12)
```

The `-12` reserves the box chrome plus the existing question/blank/hint rows.
On a terminal under roughly 14 rows `previewRows` is 0 and the dialog renders
exactly as it does today — no title, no dividers, no preview. The dialog never
scrolls and the question is never pushed off screen.

### Line preparation

`PreviewResult.Content` is a single newline-joined string already run through
`sanitizeTerminalText` (preview.go:120), which preserves SGR color and strips
everything else. Preparation, in order:

1. Split on `\n`.
2. **Trim trailing blank lines.** `tmux capture-pane` pads its output to the
   full pane height; without this the box renders mostly empty rows.
3. Take the last `previewRows` lines.
4. Per line: `clipLine(line, innerWidth)` then append `"\033[0m"`.

Step 4's reset matters: `clipLine` (render.go:1883) passes SGR sequences
through without counting them and adds no reset of its own, so an unterminated
color sequence in the capture would bleed into the box border and every row
below it.

The block's own rows are built with the existing `pad` closure
(confirm_overlay.go:66), which left-aligns and space-fills to `innerWidth`:

- **title row** — `pad` receives a pre-composed string: `title`, then padding
  to push `dim(source)` flush right, computed as
  `innerWidth - visualLen(title) - visualLen(source)`. If that is under 1 the
  source marker is dropped and the title is clipped instead.
- **divider rows** — `pad(strings.Repeat("─", innerWidth))`.
- **preview rows** — one `pad` per prepared line.

### Fetch limits

```go
var killPreviewLimits = PreviewLimits{Lines: 60, Bytes: 32 << 10}
```

At most 12 lines are rendered; 60 gives headroom for the trailing-blank trim
and keeps the remote response body small versus `DefaultPreviewLimits`'s
2000 lines / 512 KiB (preview.go:38).

### Wake plumbing

`readModalEvents(dec, wakes)` (tui.go:47) already selects over stdin plus a
`[]wakeFD` via `unix.Select` (tui_events.go:376). `previewPane` allocates an
`os.Pipe` and exposes the read end as `wakeFD{fd: …, kind: wakePreview}` — the
same self-pipe pattern as `RemoteHub.WakeFD()` (remote.go:152) and the
inspector (inspector.go:169). A new `wakePreview` `wakeKind` is added; the
modal loop treats it as "re-render, do not exit".

The call site passes `append(slices.Clone(c.modalWakes), p.wake())` so the
shared `modalWakes` slice (actions.go:31, built once at tui.go:333) is never
mutated.

### Lifetime and cancellation

The overlay `defer p.close()`s. The fetch goroutine delivers into a buffered
channel and selects against `p.done`, so answering `y` before the fetch lands
neither writes to a closed pipe nor leaks: `LoadPreview` shells out to tmux
(fast) and `fetchRemotePreview` is capped by a 5s `http.Client` timeout
(remote_actions.go:148).

`close()` is idempotent, closes `done`, then both pipe ends.

### Failure and edge states

| State | Preview slot renders |
| --- | --- |
| fetch in flight | dim `loading preview…` |
| tmux and transcript both fail | dim `no preview available` |
| content empty after trimming | dim `(pane empty)` |
| remote timeout / unreachable | dim `preview unavailable: timeout` |
| remote 404 (`errSessionEnded`) | dim `session already gone` |
| terminal under ~14 rows | nothing — today's plain box |

Every state is non-blocking; the kill decision is always reachable. Errors are
rendered as one short dim line, never a stack of wrapped text.

The box changes height when the snapshot replaces the placeholder.
`screenRenderer.Draw` (screen_renderer.go:32) forces a full redraw whenever the
frame's row count differs from the previous one, so there is no ghosting from
the taller box.

## Testing

Renderer tests are pure functions over strings — no terminal, matching
`render_test.go` / `render_inspector_test.go` style. `visibleWidth` already
exists in `render_inspector_test.go:13` for width assertions.

`renderConfirmOverlay` cases:

- nil preview produces output identical to the current implementation
  (regression guard for the three untouched callers)
- loading placeholder, loaded snapshot, error, and empty-after-trim states
- trailing blank lines are trimmed before the last-N slice
- every rendered line ends with a reset when the source contains an
  unterminated SGR sequence
- a preview line wider than the box is clipped, and the box width is unchanged
  by it
- `rows` below the threshold renders the plain box **at today's width** — the
  72-column floor must not leak into the fallback
- box width floor of 72 applies when the question is short and a preview block
  renders, and does not apply when the pane is nil

`previewPane` cases:

- `snapshot()` before the fetch completes reports `Loaded: false`
- `close()` before the fetch completes does not panic and does not leak the
  goroutine (verified by the fetch func signalling through a test channel)
- `close()` twice is safe

## Files touched

| File | Change |
| --- | --- |
| `kill_preview.go` | new — `overlayPreview`, `previewPane`, start/snapshot/wake/close |
| `confirm_overlay.go` | `renderConfirmOverlay` takes `*overlayPreview`; add `confirmOverlayPreview`; `confirmOverlay` becomes a wrapper |
| `tui_events.go` | add `wakePreview` wake kind |
| `actions.go` | `actKill` starts a local pane, calls `confirmOverlayPreview` |
| `remote_actions.go` | `actKillRemote` starts a remote pane, calls `confirmOverlayPreview` |
| `kill_preview_test.go` | new — renderer + pane lifecycle tests |
