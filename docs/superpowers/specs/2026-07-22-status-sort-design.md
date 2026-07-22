# Status Sort Design

Date: 2026-07-22

## Goal

Add a persisted TUI sort mode that groups sessions by actionable status: waiting first, then idle, then busy, with unknown statuses last. Within each group, show recently active sessions first.

## Current Behavior

The TUI supports directory, creation-time, and update-time sort modes. `s` and `S` cycle forward and backward through those modes, the selected mode is persisted, and active directory or age columns show an arrow. Session status is already present in the shared local/remote `Session` JSON shape, but sorting does not use it.

## Status Classification

Status sort uses a semantic rank rather than alphabetical display text:

1. Waiting: `Session.WaitingFor` is non-empty. This takes precedence over `Session.Status` because a waiting session may retain another base status while blocked on input.
2. Idle: `Session.Status` equals `idle`, case-insensitively.
3. Busy: `Session.Status` equals `busy`, case-insensitively.
4. Unknown: blank or any unrecognized status.

Unknown values remain visible and sort after known groups. They do not cause errors.

## Ordering

`SortSessions(sessions, "status")` compares:

1. Semantic status rank ascending: waiting, idle, busy, unknown.
2. `Session.Updated()` descending within equal ranks, so recently active sessions appear first.
3. Equal rank and equal update time preserve input order through the existing stable sort.

The same comparator applies independently to the local section and each remote host section, matching current sort behavior.

## TUI Integration

Add `status` to the persisted sort-mode cycle immediately after `dir`:

`dir → status → created → created-asc → updated → updated-asc`

- `s` moves forward; `S` moves backward and both continue wrapping.
- Toast/current-sort description: `status (waiting → idle → busy)`.
- Help text includes status in the cycle summary.
- When status mode is active, the `STATUS` column header displays the active-sort arrow. Directory and age headers remain unmarked.
- Existing modes and labels remain unchanged.
- Existing sort-mode persistence accepts and saves `status` through the current config path and best-effort I/O behavior.

Status mode has one direction only because its order is semantic, not ascending or descending text.

## Compatibility and Scope

- No session schema or remote API changes.
- No new dependencies.
- Unknown persisted modes continue degrading to existing directory behavior.
- No changes to status rendering, colors, waiting labels, session collection, or remote transport.
- Current pending-spawn selection and interactive handoff fixes remain unchanged.

## Testing

Add focused tests covering:

1. Waiting sessions sort before idle, busy, and unknown sessions.
2. `WaitingFor` takes precedence over base status.
3. Status matching is case-insensitive.
4. Sessions within one group sort by `Updated()` descending.
5. Equal rank/update values retain stable input order.
6. `status` appears in forward and backward sort cycles at the designed position.
7. Status sort description and active `STATUS` header label are correct.
8. Existing sort modes remain green.
9. Full `go test ./...`, `go vet ./...`, `go build ./...`, and `git diff --check` pass.
