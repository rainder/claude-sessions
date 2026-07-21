# Selectable empty local host

Date: 2026-07-21
Status: approved design, pre-implementation

## Goal

Allow the TUI cursor to select the local machine when it currently has no
sessions. Pressing `n` on that row creates the first local session through the
existing local-new flow.

When the local host is empty, the top of the table shows a `(no sessions)`
placeholder that behaves like a normal row: arrow keys can highlight it, and `n`
opens the local `new tmux+claude session` picker.

This is the local counterpart to
`2026-07-21-empty-remote-host-selection-design.md`, which already made empty
*remote* hosts selectable. That change built the whole `selectionTarget` /
empty-host machinery; this one reuses it for the local section, which was left
out because the local section is rendered differently from remote sections.

## Root cause

Empty remote hosts became selectable, but the local section did not, for two
reasons:

- `buildSelectionTargets` (selection.go) emits a target per local session and,
  for each remote, either its sessions or one `emptyHostSelectionTarget(host)`.
  When the local list is empty it emits **no** local target at all — there is
  nothing to select.
- The renderer treats the local section (section 0) specially. Remote sections
  print a `bold(host)` heading followed by a switch that renders
  `renderEmptyHostRow` when the section is empty. The local section is rendered
  unconditionally via `rowFn(sectionRows[0])` with no heading, so zero local
  sessions prints nothing — no placeholder, no selectable row.

The existing empty-host target model already supports an empty host name: its ID
namespace is `"\x00host:" + host`, and for the local host that host name is the
empty string `""`, yielding ID `"\x00host:"`. Navigation, selection validation,
and the local-vs-remote action routing already key on host and are host-agnostic,
so `host == ""` needs no special-casing beyond emitting the target and rendering
it.

## Selection model

In `buildSelectionTargets`, when `len(local) == 0`, prepend a single
`emptyHostSelectionTarget("")` before appending remote targets. Its ID is
`emptyHostSelectionID("")` = `"\x00host:"`.

No loading/error gating is needed: unlike a remote fetch, the local session
collection has no loading or unreachable state, so an empty local host is always
a confirmed, selectable destination.

Ordering is unchanged otherwise: empty-local target (when present) → remote
targets in rendered order. The empty-local host does not become a fake
`Session`, does not enter sorting, and does not affect session or agent counts.

Per the approved decision, the empty-local row shows **whenever local is empty**,
independent of whether remote hosts have sessions — symmetric with remote empty
rows, and so `n` can create a local session at any time.

## Navigation and selection reconciliation

`navTargets` and `validateTargetSel` already operate on `[]selectionTarget` and
are host-agnostic, so they require no change:

- Navigation wraps through the empty-local target like any other target.
- After `n` creates the first local session, the empty-local target disappears.
  `validateTargetSel`'s existing NUL-prefix reconciliation trims `"\x00host:"`
  to host `""` and moves selection to the first target whose
  `session.Host == ""` — i.e. the first local session. This is the same
  empty-host-to-populated transition already specified for remotes, and it works
  for local for free because local sessions carry `Host == ""`.

## Rendering

Give the local section (index 0) the same empty/populated switch the remote
sections use, in all three view modes (full, intermediate, minimal):

- `len(sectionRows[0]) == 0` → `renderEmptyHostRow(w, "", sel)`.
- otherwise → `rowFn(sectionRows[0])` as today.

`renderEmptyHostRow(w, "", sel)` prints `(no sessions)` with the cursor marker
when `sel == emptyHostSelectionID("")`, reusing the exact code that already
renders remote empty rows.

No heading is added for the empty-local row. Local sessions render with no
section heading today; adding a `local:` heading only in the empty case would
make a label flicker in and out as the last local session comes and goes. The
placeholder sits at the top where local sessions normally appear, and reads as
the local machine by position — consistent with how populated local rows are
already presented.

Section headings elsewhere, placeholder wording, and header counts are unchanged.

## Actions

No new action code is required; the empty-local target must route to the
existing local creation flow:

- `actNew` routes on the selected target's host. `selectedRemoteNewTarget()`
  returns `ok == false` when the target host is `""` (both for a real local
  session and for the empty-local target), so `actNew` falls through to the
  local prompt + `SpawnNew(cwd, name, command)` path.
- The empty-local target has `session == nil`. The local `new` path prompts
  fresh for cwd/name/command and must not dereference the selected session. This
  is verified by test; a guard is added only if a deref is found.

`kill`, `attach`, and `preview` already no-op on a `session == nil` target (they
resolve through the session-only accessor), so selecting the empty-local row and
pressing those keys does nothing — same as an empty remote row.

Remote behavior, the remote-new flow, server behavior, and SSH attachment are
unchanged.

## Failure and refresh behavior

- Local host with zero sessions: selectable; `n` creates a local session.
- Local host gains its first session (via `n` or an external `claude` launch):
  refresh drops the empty-local target and selection follows to the first local
  session.
- Local host loses its last session: refresh re-emits the empty-local target;
  selection reconciles to it (or to the first remaining target).
- Local create failure/cancel: existing local `SpawnNew` / picker behavior.

## Testing

### Selection tests

- `buildSelectionTargets` with empty/`nil` local and no remotes emits exactly one
  target: the empty-local target, ID `"\x00host:"`.
- With empty local and one populated remote, the empty-local target is first,
  followed by the remote session targets.
- With one or more local sessions present, no empty-local target is emitted
  (existing behavior preserved).
- Navigation enters the empty-local target and wraps in both directions.
- `validateTargetSel` reconciles `"\x00host:"` to the first local session after
  the local host becomes populated.

### Action tests

- `actNew` with the empty-local target selected routes to the local creation
  path (not remote), and does not dereference the nil session.
- Existing local (session selected) and remote routing remain unchanged.

### Rendering tests

- The selected cursor marker appears beside the local `(no sessions)` row in
  full, intermediate, and minimal views.
- Unselected empty-local placeholder output is unchanged from the remote empty
  row wording.
- Header session/agent counts ignore the empty-local target.

### Verification

```sh
go test ./...
go vet ./...
```

## Out of scope

- A global local/remote host chooser when pressing `n`.
- Any change to remote empty-host behavior.
- A `local:` section heading, or restyling populated local rows.
- Config schema or server API changes.
- Treating the empty local host as a process-like session.
- Unrelated navigation, sorting, rendering, or action refactors.
