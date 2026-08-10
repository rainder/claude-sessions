# Group-first sort

## Problem

Sessions can be tagged with a group (Shift-1..9, `Session.Group` 1-9, 0 =
ungrouped). Today grouping only *filters* the view (`1..9` show-only,
`h1..9` hide); it has no effect on row order. Rows within a group scatter
across the list according to the normal sort mode (dir/status/created/
updated), so a group's sessions aren't visually clustered.

## Goal

Add an optional secondary ordering: when enabled, rows are ordered by group
number first (ascending, 1..9), then by the existing sort mode within each
group. Ungrouped rows (0) sort after all grouped rows. This is a toggle on
top of the existing sort mode, not a new mode — every existing sort mode
gains a "grouped" variant.

## UI: folded into the existing sort dialog

No new global keybinding. The `s`/`S` sort-by dialog (`sort_picker.go`)
gains a second control:

- `g`/`G` while the dialog is open toggles "group first" on/off (redraw
  only, does not confirm or close the dialog).
- The dialog box shows the toggle's current state (a line under the title,
  e.g. `group first: on`/`group first: off`).
- The hint row becomes:
  `↑/↓ select    g group first    ⏎ confirm    esc cancel`
- `⏎` confirms both the selected sort mode and the group-first state
  together, as one atomic apply.

`renderHelp`'s existing `s / S` entry (tui.go:1378-1379) is extended to
document `g` and to reflect the current group-first state next to
`current sort: <mode>`.

## State & persistence

- New config file `~/.config/claude-sessions/group-sort` holding `"on"` or
  `"off"`, following the exact pattern of `sort-mode`/`view-mode`
  (config.go). `LoadGroupSort() bool` / `SaveGroupSort(bool)`.
- `groupSortOn := LoadGroupSort()` loaded once in `RunTUI`, alongside
  `sortMode`.
- Runtime-only state during the dialog itself lives on `sortPickerState`
  (new `groupSort bool` field), seeded from `groupSortOn` when the dialog
  opens, discarded on cancel.

## Comparator

New `sessionLessGrouped(a, b Session, mode string, groupSort bool) bool` in
session.go:

```go
func sessionLessGrouped(a, b Session, mode string, groupSort bool) bool {
    if a.Disabled != b.Disabled {
        return !a.Disabled // unchanged: disabled always sorts last
    }
    if groupSort {
        ra, rb := groupSortRank(a.Group), groupSortRank(b.Group)
        if ra != rb {
            return ra < rb
        }
    }
    return sessionLess(a, b, mode)
}

// groupSortRank maps group 1..9 to itself; 0 (ungrouped) ranks last (10).
func groupSortRank(group int) int {
    if group < 1 || group > 9 {
        return 10
    }
    return group
}
```

`sessionLess` itself is unchanged — `sessionLessGrouped` wraps it, and when
`groupSort` is false or group ranks are equal, the tie-break is exactly
today's per-mode comparison (redundant disabled check on the delegated call
is harmless: it re-confirms equality already established above).

`SortSessions(rows []Session, mode string, groupSort bool)` gains the new
parameter and uses `sessionLessGrouped` instead of `sessionLess`. Both call
sites update:
- `tui.go:357` (local rows) passes `groupSortOn`.
- `sortRemotes` (tui.go:1111) gains a `groupSort bool` parameter, passed
  through from its caller at tui.go:361.

Each section (local, and each remote host independently) still sorts on its
own rows — group-first sort does not merge rows across hosts, matching how
sort mode already behaves.

## Header indicator

A small persistent badge is added next to the existing group-filter badge
(`groupFilterIndicator`, render.go:1540) whenever `groupSortOn` is true, so
the state is visible without opening the dialog: the dim, uncolored text
`"group↑"` (no per-group color, since it's not tied to any single group
number — plain `dim(...)`, matching the weight of the `hide` label in
`groupFilterIndicator` but without its red). Rendered in `renderHeader`
next to the group-filter badge; empty string (nothing shown) when
`groupSortOn` is false, mirroring `groupFilterIndicator`'s own
empty-string-means-inactive convention.

## Toast

On confirming the dialog, if either the sort mode or the group-first state
changed from what was live, a single toast covers both, e.g.:
`"sort: dir (cwd a→z) · group first"` when group-first is on, or plain
`"sort: dir (cwd a→z)"` when off — reusing `sortDesc(mode)` and appending
the suffix conditionally.

## Testing

Table-driven tests for `sessionLessGrouped` covering:
- group ordering (1 < 2 < ... < 9 < ungrouped) when `groupSort` is true.
- ungrouped-last placement.
- disabled-last precedence preserved regardless of `groupSort`.
- `groupSort=false` behaves identically to calling `sessionLess` directly
  (delegation is a pure pass-through).
- `sortPickerState.handle` gains a case for `g`/`G` toggling `groupSort`
  without confirming/cancelling (mirrors the existing up/down/enter/esc
  table tests in sort_picker_test.go).

## Out of scope

- No change to the group *filter* (`1..9` / `h1..9`) — orthogonal feature,
  unaffected.
- No new global keybinding.
- No change to `groupsOfRows` or how `Session.Group` is set/synced.
