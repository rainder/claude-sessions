# DIR column marquee on narrow terminals

Date: 2026-07-09
Status: approved

## Problem

The DIR column is sized to the longest `squashPath(cwd)` across rows. On a
narrow terminal the table exceeds the width and `clipLines` chops every row at
the right edge — the columns after DIR (MODEL, COST, STATUS, TMUX) disappear
entirely, and long paths are still unreadable.

## Decision

When the table doesn't fit, shrink the DIR column (never below **16** cells)
and marquee-scroll any path that overflows its shrunken cell: classic loop
with a 3-space gap, all overflowing rows animate on a shared clock.

Chosen over: bounce/ping-pong scrolling, static middle-ellipsis, and
animating only the selected row.

## Design

### 1. Width budget (render.go)

Each table view already computes per-column widths (`dirW = max(len(...))`).
New step: sum the fixed columns + separators; if the total exceeds the
terminal width (`cols > 0`), reduce `dirW` by the deficit, clamped to
`minDirW = 16`. If the table still doesn't fit at 16, behavior is unchanged
from today (line clipped at the right edge by `clipLines`).

Applies to all three views that print a DIR column: wide (local+remote
table), narrow, and headless.

### 2. marqueeCell(s string, width, offset int) string

- `runeLen(s) <= width`: return `s` padded with spaces to `width` (static).
- Otherwise: treat `s + "   "` (3-space gap) as a ring of period
  `p = runeLen(s) + 3`; starting at `offset % p`, take `width` runes,
  wrapping around the ring. Rune-safe (UTF-8), no ANSI inside `s` — color
  is applied by the caller after slicing, so selection highlight survives.

### 3. Shared clock

One package-level marquee step counter (or a value threaded through
`RenderAll`). Every marquee tick increments it by 1; each overflowing row
computes its own wrap via `offset % ownPeriod`. Rows that fit render static.

### 4. Animation tick (tui.go)

`RenderAll` (or the render closure) reports `overflowing bool` — true when at
least one visible row's DIR was marquee'd. The main loop keeps its existing
structure; the only change is the Select timeout:

```go
timeout := time.Until(nextTick)
if overflowing {
    timeout = min(timeout, marqueeInterval) // ~300ms
}
```

On a marquee-tick expiry (fired before `nextTick`): advance the step counter
and call `render()` only — **no** `refresh()`, so ps/tmux shell-outs stay on
the 2s wall-clock cadence. `nextTick` is not reset by marquee ticks.

No overflow → no fast ticks → zero idle cost. No new goroutines; the
single-renderer / single-stdin-consumer invariants are untouched.

## Testing

- `marqueeCell`: fits (pad), exact fit, overflow at offset 0, mid-ring,
  wrap-around across the gap, multi-byte runes, width 0/negative.
- Width budget: no shrink when fits; shrink to exact deficit; clamp at 16.
- Existing render tests unaffected (wide terminal ⇒ no shrink, offset 0).

## Out of scope

- Marquee for other columns (NAME, MODEL).
- Configurable speed/gap/min width.
- Pausing animation while a prompt or interactive action is active (actions
  already own the terminal; marquee only runs inside the main loop).
