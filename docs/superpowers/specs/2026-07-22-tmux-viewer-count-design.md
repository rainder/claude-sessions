# Tmux Viewer Count Design

**Status:** Approved

## Problem

The session list currently distinguishes sessions with and without a tmux pane, but it cannot show whether a tmux session is detached or how many tmux clients are attached. A detached pane still runs and produces output, so tmux membership alone does not answer whether anyone is currently viewing it.

The TUI should collect tmux's attached-client count and expose it in every list mode without increasing rendered line width.

## Goals

- Collect exact `session_attached` counts for local and remote sessions.
- Distinguish no tmux, unknown count, detached tmux, and attached tmux.
- Show count in full, intermediate, and minimal list modes.
- Preserve total row width and existing column widths.
- Keep mixed-version remote deployments truthful.
- Keep tmux collection best-effort and within the existing single tmux subprocess per refresh.

## Non-goals

- Detect whether a human is actively looking at a terminal.
- Count only clients displaying one exact pane.
- Change session sorting.
- Change picker, inspector, or modal selection markers.
- Add a dedicated viewer-count column.

## Viewer semantics

Count means number of tmux clients attached to the tmux **session**, matching tmux's `#{session_attached}` value. If multiple Claude panes share one tmux session, each row reports the same count.

`capture-pane` inspection does not create a tmux client and therefore does not increment this count.

## Collection

Extend the existing `tmux list-panes -a` format rather than spawning a second `tmux list-sessions` process:

```sh
tmux list-panes -a -F '#{pane_pid}\t#{session_name}:#{window_index}.#{pane_index}\t#{session_attached}'
```

Use tab-delimited parsing so spaces in tmux session names do not conflict with field boundaries.

Introduce pane metadata:

```go
type tmuxPaneInfo struct {
    Location string
    Attached *int
}
```

`Attached` is a pointer because count availability has three states:

- `nil`: count unavailable or malformed.
- pointer to `0`: tmux session is detached.
- pointer to a positive integer: attached-client count.

Extract parsing into a pure helper so tmux output can be tested without invoking tmux. A valid PID and location remain usable when count parsing fails; only `Attached` stays `nil`.

Change pane ancestry lookup to return the complete `tmuxPaneInfo` and a found flag. Preserve the current invariant that the session PID itself is checked before walking parent PIDs.

`CollectLocal` assigns both the existing `Session.Tmux` locator and new count from the matched pane metadata. tmux command failure remains best-effort and produces an empty map, matching current behavior.

## Session model and remote compatibility

Add:

```go
TmuxAttached *int `json:"tmuxAttached,omitempty"`
```

The pointer preserves mixed-version behavior:

- New collector, detached tmux: JSON contains `"tmuxAttached": 0`.
- New collector, attached tmux: JSON contains positive count.
- Old remote collector: field is absent and decodes as `nil`.
- No tmux pane: `Tmux` is empty and count is ignored.

Existing server and remote-client paths serialize and decode `Session` directly, so no separate protocol or handler change is required.

## List prefix

Replace the session list's current arrow/dot marker with a shared two-cell prefix: one state character plus one separator.

| State | Prefix | Styling |
| --- | --- | --- |
| No tmux | `  ` | none |
| Tmux, count unavailable | `· ` | dim |
| Tmux detached | `0 ` | dim |
| 1–9 attached clients | `1 ` through `9 ` | green |
| 10 or more attached clients | `+ ` | green |

The `Session` field retains the exact count even though the list compresses counts above nine to `+`.

All three list modes use the same prefix. Headers already reserve two leading cells, so no row, header, or column width changes.

## Selection rendering

Session-list selection changes from a leading `▶` to continuous reverse-video highlighting of the selected row. Viewer count therefore remains visible while selected without consuming another cell.

Selected rows must not contain nested ANSI resets that cancel reverse video midway through the row. Each renderer should:

1. Determine whether the row is selected.
2. Use a plain cell-rendering path for selected rows, suppressing every cell-level ANSI wrapper: status, model, context, name, tmux placeholder, viewer prefix, and headless dimming.
3. Build the complete plain row.
4. Wrap the complete row once with reverse video and one trailing reset.

For unselected rows, existing cell colors remain. Unselected headless rows remain dimmed. Selection overrides headless dimming while selected; dimming returns after selection moves away.

Selectable empty-host rows use the same whole-row reverse-video selection and omit their normal dim wrapper while selected. Other screens retain their current arrow markers. Row identity, mouse hit regions, keyboard movement, and viewport behavior do not change.

## Rendering structure

Add shared helpers for:

- Converting `Tmux` plus `TmuxAttached` into the one-character viewer symbol.
- Rendering the fixed two-cell prefix with normal, dim, green, or plain-selected styling.
- Wrapping a complete row in reverse video.

Full, intermediate, and minimal renderers should call the shared prefix helper rather than duplicate state logic.

The full view keeps its existing `TMUX` locator column unchanged. Viewer count appears in the prefix so behavior remains consistent across all widths.

## Error handling

- tmux unavailable or not running: retain current empty-map behavior.
- Malformed pane PID or location: skip that output line.
- Malformed attached count with valid pane data: retain locator and expose unknown `·`.
- Older remote without count field: expose unknown `·`, never false detached `0`.
- Counts below zero, if returned or injected, are treated as unavailable.

## Testing

### Collection and parsing

- Parse detached, one-client, multi-client, and double-digit counts.
- Preserve session names containing spaces.
- Preserve valid location with malformed or negative attached count.
- Skip malformed PID/location records.
- Verify PID-self and ancestor pane lookup return complete metadata.

### Model and transport

- JSON round-trip pointer-to-zero distinctly from `nil`.
- Missing legacy JSON field remains `nil`.
- Positive exact counts survive JSON transport.

### Rendering

For full, intermediate, and minimal modes:

- Blank prefix for no tmux.
- Dim `· ` for unknown count.
- Dim `0 ` for detached tmux.
- Green `1 ` through `9 ` for exact displayed counts.
- Green `+ ` for counts of ten or more.
- Header and line widths remain unchanged.
- Selected rows contain no arrow, preserve count, and use one continuous reverse-video span.
- Selected rows contain no nested reset before the final reset.
- Selected headless and empty-host rows remain visibly selected.
- Unselected headless rows retain current dimming.

### Verification

```sh
go test ./...
go vet ./...
go build .
```
