# Sort picker dialog (`s` key)

## Problem

`s`/`S` currently cycle the sort mode one step at a time through
`sortModeOrder` (tui.go:809, case at tui.go:703-713). The user wants `s` to
open a dialog box instead: pick a sort mode from a list with up/down, confirm
with Enter, cancel with Esc.

## Decision

`s` is repurposed. It no longer cycles; it opens the picker. `S` (shift-s) is
dropped along with the cycle behavior — the picker replaces both.

## Design

New file `sort_picker.go`, mirroring `account_picker.go`'s shape exactly
(state struct + handle + render + blocking loop), since that file already
solves "small bordered up/down/enter/esc list overlay" and the two dialogs
should look like the same kind of thing.

**State**: `sortPickerState{sel, rows int}` — `handle(key)` returns
`(confirm, cancel bool)`. Up/Down wrap through `rows`. Enter confirms
(rows is always 6, never zero, so no empty-list case like the account
picker's). Esc/q/Q/^C/^D cancels.

**Rows**: the existing `sortModeOrder` (dir, status, created, created-asc,
updated, updated-asc), one row per mode, label text from the existing
`sortDesc(mode)` — no new label table. Active marker (`●`) on the row
matching the current live `sortMode`, selection cursor starts on that same
row (so Enter without moving is a no-op, same rule as the account picker).

**Render**: `renderSortPicker(mode string, sel, cols, rows int) string` —
same bordered-box layout as `renderAccountPicker` (title, blank line, one row
per mode with the active row's marker + label, blank line, dimmed hint,
centered in the terminal). Title: `"sort by"`. Hint:
`"↑/↓ select    ⏎ confirm    esc cancel"`.

**Blocking loop**: `pickSortMode(current string, wakes []wakeFD) (string, bool)`
— same shape as `pickAccount`: new `screenRenderer`/`inputDecoder`, loop
drawing `renderSortPicker`, `readModalEvents`, dispatch through
`state.handle`, return `(mode, true)` on confirm or `("", false)` on cancel.

**Wiring** (tui.go, replacing the `case "s", "S":` block at 703-713): call
`screen.Invalidate()`, then `pickSortMode(sortMode, modalWakes)` inline —
same pattern the `"?"` help case already uses (tui.go:725-740), not routed
through `actCtx`/`actions.go`, since this needs no session-row target and no
host split, just the loop-local `sortMode`/`fd`/`screen`/`modalWakes` the
`"?"` case already has in scope. On confirm: `sortMode = <picked>`,
`SaveSortMode(sortMode)`, toast = `"sort: " + sortDesc(sortMode)` (same toast
text the old cycle used), `refresh(false)`. On cancel: nothing changes. Both
paths end with `screen.Invalidate(); render()`.

## Out of scope

- No new persistence — `SaveSortMode`/`LoadSortMode` (config.go:43-59) are
  reused unchanged.
- No change to `SortSessions`/`sortRemotes` or the sort modes themselves.
- `renderHelp`'s sort-key hint text (uses `sortMode`) gets a one-line update
  to describe the new dialog instead of cycling, since it's user-facing help
  text that would otherwise go stale.

## Testing

- Unit test `sortPickerState.handle` (up/down wrap, enter/esc), mirroring
  `account_picker_test.go`.
- Manual TUI check: `s` opens dialog pre-selected on current mode, arrow keys
  move selection, Enter applies + persists + toasts, Esc leaves sort
  unchanged, resize while open keeps the box centered.
