# Sort picker dialog (`s` key)

## Problem

`s`/`S` currently cycle the sort mode one step at a time through
`sortModeOrder` (tui.go:809, case at tui.go:703-713). The user wants `s` to
open a dialog box instead: pick a sort mode from a list with up/down, confirm
with Enter, cancel with Esc.

## Decision

`s` is repurposed. It no longer cycles; it opens the picker. `S` (shift-s)
also opens the same picker (not dropped) — reviewed by Codex, which flagged a
silent drop as an unneeded muscle-memory regression with no offsetting
benefit.

## Design

New file `sort_picker.go`, mirroring `account_picker.go`'s shape exactly
(state struct + handle + render + blocking loop), since that file already
solves "small bordered up/down/enter/esc list overlay" and the two dialogs
should look like the same kind of thing.

**State**: `sortPickerState{sel, rows int}` — `handle(key)` returns
`(confirm, cancel bool)`. Up/Down wrap through `rows`. Enter confirms.
Esc/q/Q/^C/^D cancels. Keep the `rows == 0` guard from `accountPickerState`
even though `rows` is always constructed as `len(sortModeOrder)` (6) today —
`sortModeOrder` is a mutable package `var`, not a const, and the guard is
cheap insurance against a future zero-value construction panicking on
`sortModeOrder[state.sel]`.

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
through `actCtx`/`actions.go` (Codex-reviewed and agreed: sorting owns none
of `actCtx`'s concerns — no selected target, no host split — so forcing it
through `actCtx` buys nothing), just the loop-local
`sortMode`/`fd`/`screen`/`modalWakes` the `"?"` case already has in scope.

```go
case "s", "S":
    screen.Invalidate()
    if picked, ok := pickSortMode(sortMode, modalWakes); ok && picked != sortMode {
        sortMode = picked
        SaveSortMode(sortMode)
        toast = "sort: " + sortDesc(sortMode)
        toastUntil = time.Now().Add(4 * time.Second)
        refresh(false)
    }
    screen.Invalidate()
    render()
```

On confirm with a changed mode: update `sortMode`, `SaveSortMode`, toast +
**`toastUntil`** (toast display is gated on `time.Now().Before(toastUntil)`
at tui.go:335 — a toast set without bumping `toastUntil` never renders; this
was caught in Codex review as a real bug in an earlier draft of this
snippet), `refresh(false)`. Confirm on the already-current mode (`picked ==
sortMode`) is a true no-op — no save, no toast, no refresh — mirroring the
account picker's `choice.Active` suppression. Cancel: nothing changes. Both
paths end with `screen.Invalidate(); render()`.

## Out of scope

- No new persistence — `SaveSortMode`/`LoadSortMode` (config.go:43-59) are
  reused unchanged.
- No change to `SortSessions`/`sortRemotes` or the sort modes themselves.
- `renderHelp`'s sort-key hint text (uses `sortMode`) gets a one-line update
  to describe the new dialog instead of cycling, since it's user-facing help
  text that would otherwise go stale. Same update for `main.go:114`'s CLI
  help text ("s cycle sort") — a second stale-docs site Codex review found.
- Pre-existing shared-input hazard, not introduced by this design: `pollEvents`
  can decode multiple keys in one batch, so `s`/`S` + a trailing Enter arriving
  together can leak that Enter into a session-row action once the picker
  closes. The `?` and Ctrl+W overlays already have this same exposure — not a
  regression, just worth a test case rather than silent inheritance.
- No selection-viewport re-anchor after a sort-mode change (`refresh(false)`
  preserves row identity but not scroll position) — not a regression vs. the
  old cycle behavior, left as a future improvement.

## Testing

- Unit test `sortPickerState.handle` (up/down wrap, enter/esc, `rows == 0`
  guard), mirroring `account_picker_test.go`.
- Manual TUI check: `s`/`S` open dialog pre-selected on current mode
  (marker on that row even after moving `sel` away from it), arrow keys move
  selection, Enter on a *changed* mode applies + persists + toasts (verify
  the toast actually renders, not just gets set — the `toastUntil` bug
  above), Enter on the *unchanged* mode is a true no-op, Esc leaves sort
  unchanged, resize while open keeps the box centered, and a terminal
  shorter than the box's ~12 lines (title + 6 rows + 2 blank + hint +
  border) still shows the selected row rather than truncating it out of
  view.
