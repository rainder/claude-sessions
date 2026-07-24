# Free-form text filter ('/')

Approved design, 2026-07-24. Builds on the group-filter machinery
(2026-07-24-session-groups-design.md, incl. the hide-set addition).

## Goal

`/` enters a live incremental text filter (k9s-style): rows narrow as the
query is typed. Composes with the group filter (AND). Runtime-only, never
persisted.

## Matching

- Query is split on spaces into tokens; every token must match (AND).
- A token matches if it is a case-insensitive substring of any of: display
  name, cwd, host name (remote section host), or tmux session name.
- Empty query = no text filter.
- Pure helper `matchesTextFilter(s Session, host, query string) bool` (or
  equivalent) so matching is table-testable.

## Input mode

- `/` enters input mode; existing committed query is preloaded for editing.
- While editing, every keystroke goes to the query — hotkeys (digits, k, q,
  h, …) are suspended:
  - printable chars append (ASCII printable; multi-byte input appended as-is)
  - Backspace (0x7f/0x08) deletes last rune; Ctrl+U clears the line
  - Enter commits: leaves input mode, query stays active (empty query
    clears the filter)
  - Esc cancels input mode AND clears the query entirely
- Rows re-filter and re-render on every keystroke (settleRows + selection
  anchor, same as group-filter changes).
- Esc's exit-TUI binding is shadowed while editing; input mode always wins.

## Rendering

- Footer while editing: `/query▌` (cursor block) replaces the hint line.
- Header indicator when a query is active (editing or committed): dim
  `/query` appended after the group-filter indicator, e.g.
  `only ③  /api`. Empty when no query.
- Footer hint gains `/ search` entry; help screen documents the mode keys.

## Composition & state

- `groupView` gains a `query string` field; `filterSessionRows` /
  `filterRemoteResults` apply text match AND group filter. Doc comment
  updated (it now carries the full view filter, not just groups).
- TUI state: `textQuery string` (committed), `textEditing bool`,
  `editBuffer` — runtime-only. Factor the editing key handling into a pure
  helper (key → next buffer/mode/committed) for table tests.
- Selection fallback via existing machinery when the selected row is
  filtered out.

## Tests

- Matching: tokens AND, case-insensitivity, each field, empty query.
- Editing state machine: append, backspace on empty, Ctrl+U, Enter commit,
  Enter with empty buffer clears, Esc clears, '/' preloads committed query.
- Indicator: none / query-only / combined with group indicator.
- Filter composition: group AND text in filterSessionRows.
- Existing tests keep passing; gofmt, go vet, go test -race.
