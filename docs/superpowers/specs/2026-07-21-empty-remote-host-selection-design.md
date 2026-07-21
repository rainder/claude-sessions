# Selectable empty remote hosts

Date: 2026-07-21
Status: approved design, pre-implementation

## Goal

Allow the TUI cursor to select a reachable remote host that currently has no
sessions. Pressing `n` on that row creates the first session on that host through
the existing remote-new flow.

For the example in the reported screenshot, arrow-key navigation can highlight
beluga's `(no sessions)` row and `n` opens `New tmux+claude session on beluga`.

## Root cause

The renderer and navigation use different row models:

- `buildSections` retains every configured remote host, including a successful
  result whose session list is empty.
- Each renderer prints `(no sessions)` for that empty section.
- Navigation, selection validation, and actions use `AllSessions`, which
  contains concrete `Session` values only.
- An empty remote contributes no `Session`, so its visible placeholder has no
  selectable ID. `actCtx.selected()` cannot resolve it, and `actNew` falls back
  to the local creation flow.

The placeholder must become a first-class selection target without pretending
to be a process session.

## Selection model

Add an internal `selectionTarget` type in a focused selection unit. A target is
one of:

1. **Session target** — references a real `Session`. Its ID is the existing
   `Session.ID()` value.
2. **Empty-host target** — carries a remote host name and no process identity.
   Its internal ID uses a NUL-prefixed namespace such as `"\x00host:" + name`.
   Session IDs cannot contain that prefix, so the target cannot collide with a
   local PID ID or remote `<host>:<pid>` ID.

The target builder emits items in rendered order:

1. all local sessions;
2. for each remote section:
   - all concrete remote sessions, or
   - one empty-host target when the fetch succeeded and the session list is
     empty.

Loading and errored remote sections do not emit targets. Their status is not a
confirmed usable destination for a create request.

The target model stays separate from `RemoteResult.Sessions`. Empty hosts do not
become fake `Session{PID: 0}` values, do not enter sorting, and do not inflate
session or agent counts.

## Navigation and selection reconciliation

`nav` and selection validation operate on `[]selectionTarget` instead of
`[]Session`. Navigation order continues to match screen order and wraps at both
ends.

Normal validation keeps the current selection when its target still exists and
falls back to the first target when it does not.

One explicit transition preserves host context after creation:

- If selection is the NUL-prefixed empty-host target for `beluga`, that target
  disappears, and the refreshed target list contains one or more beluga session
  targets, selection moves to the first beluga session.

This mapping applies only to the empty-host-to-populated transition. Other
missing-session cases retain the existing first-target fallback.

## Rendering

All three view modes already render an empty-section placeholder. Each
`(no sessions)` branch compares the selected ID with that section's empty-host
target ID and prints the same cursor marker used by normal session rows.

Section headings and placeholder wording remain unchanged. Header counts remain
based on concrete sessions only.

## Actions

`actCtx` receives the current selection targets and provides two resolvers:

- `selectedTarget()` returns either target kind.
- `selectedSession()` returns a session only for a real session target.

Action behavior:

- `kill`, `attach`, and `preview` use `selectedSession()`. On an empty-host
  target they return immediately, with no prompt, process operation, or network
  request.
- `new` uses `selectedTarget()`:
  - a local session target keeps the local picker and creation flow;
  - a remote session target uses its host and CWD as today;
  - an empty-host target uses its host with no default CWD;
  - no resolved target keeps the current local-picker fallback.

Refactor `actNewRemote` to receive explicit `host` and `defaultCWD` values rather
than requiring a selected concrete session. With no default CWD, the existing
prompt requires the user to enter a remote path; pressing Enter with an empty
value produces the existing `no cwd` message. The client must not validate a
remote path against the local filesystem.

Remote API calls, server behavior, SSH attachment, and error display remain
unchanged.

## Failure and refresh behavior

- Reachable empty host: selectable.
- Loading host: unselectable.
- Errored/unreachable host: unselectable.
- Host removed from config or changed to loading/error before an action:
  refresh validation drops the stale target and safely selects the first
  remaining target, or clears selection when none remain.
- Remote create failure: existing `actNewRemote` failure message and pause
  behavior.
- Successful create: existing SSH/tmux attach behavior; on return and refresh,
  selection follows the first session on that host.

## Testing

### Selection tests

- Target builder includes local and remote session targets in rendered order.
- Successful empty remote emits one empty-host target.
- Loading and errored empty remotes emit no target.
- Multiple consecutive empty hosts remain independently selectable.
- Navigation enters empty-host targets and wraps in both directions.
- Validation keeps existing targets and preserves current fallback behavior.
- `host:beluga` reconciles to the first beluga session after population.

### Action tests

- `selectedSession()` returns nil for an empty-host target.
- Session-only actions perform no work for an empty-host target.
- `new` routes an empty-host target to remote creation with host `beluga` and an
  empty default CWD.
- Existing local and populated-remote routing remains unchanged.

### Rendering tests

- Selected marker appears beside `(no sessions)` in full, intermediate, and
  minimal views.
- Unselected placeholder output remains unchanged.
- Header session/agent counts ignore empty-host targets.

### Verification

Run:

```sh
go test ./...
go vet ./...
```

## Out of scope

- A global local/remote host chooser when pressing `n`.
- Selecting loading or unreachable hosts.
- Retrying remote connections from the placeholder row.
- Config schema or server API changes.
- Treating an empty host as a process-like session.
- Unrelated navigation, sorting, rendering, or action refactors.
