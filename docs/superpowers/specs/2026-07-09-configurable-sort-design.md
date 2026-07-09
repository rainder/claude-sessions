# Configurable session sort

Date: 2026-07-09
Status: approved

## Problem

Row order is hard-coded in `CollectLocal`: cwd asc (case-insensitive),
newest-started first as tiebreaker. Users want other orderings.

## Decision

A persisted sort mode, cycled with the `s` key in the TUI, applied
client-side per section.

Modes (cycle order): `dir` (default, today's order) → `created` →
`created-asc` → `updated` → `updated-asc` → back to `dir`.

## Design

1. **`SortSessions(rows []Session, mode string)`** (session.go) — stable
   in-place sort:
   - `dir`: cwd asc case-insensitive, `StartedAt` desc tiebreak
   - `created`: `StartedAt` desc; `created-asc`: asc
   - `updated`: `Updated()` desc; `updated-asc`: asc
   - unknown mode: treated as `dir`
2. **Persistence** (config.go): `LoadSortMode()` / `SaveSortMode(mode)` on
   the `view-mode` pattern — file `~/.config/claude-sessions/sort-mode`,
   default `dir` on error or unknown value, best-effort save.
3. **Client-side application.** `CollectLocal` keeps its built-in dir sort
   (server-side default for older clients). The TUI's `refresh()` closure
   applies `SortSessions` after collection: `local` sorted in place; each
   remote section's `Sessions` sorted on a **copy** (`hub.Snapshot()` returns
   the hub's shared slice — mutating it in place races the hub goroutine).
   Render and `AllSessions` (nav) both consume the sorted vars, so cursor
   order always matches visual order. `RenderAll` signature unchanged.
   `cmdList` (main.go) applies the same sorting after `CollectLocal` +
   `FetchAllRemote`.
4. **Key binding**: `s`/`S` cycles modes, persists via `SaveSortMode`,
   re-sorts and re-renders immediately. Added to the `?` help screen along
   with the current mode.

## Testing

- `SortSessions`: each mode's order on a fixture slice; unknown mode = dir;
  stability.
- `LoadSortMode`: missing file and garbage value both give `dir`.

## Out of scope

- Sorting by cost/status/name.
- Visible sort indicator in the table header.
- Server-side sort parameter.
