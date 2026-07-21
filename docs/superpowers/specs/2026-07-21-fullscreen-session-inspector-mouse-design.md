# Full-Screen Session Inspector with Mouse Support

**Date:** 2026-07-21
**Status:** Approved design

## Summary

Add a read-only, full-screen session inspector to the existing TUI. The inspector shows a live tmux pane snapshot when the session has a tmux pane and falls back to transcript content otherwise. It supports local and remote sessions, keyboard and mouse navigation, bounded scrollback, follow-bottom behavior, and a compact metadata strip.

Mouse support also extends to the session list: single-click selects a row, double-click opens the inspector, and the wheel scrolls the table viewport. The implementation preserves the current dependency policy and terminal-ownership invariants.

## Goals

- Provide an in-app full-screen view of selected session output without attaching interactively.
- Preserve the appearance of Claude Code output by using tmux pane capture with SGR styling.
- Keep headless and stopped sessions inspectable through transcript fallback.
- Support local and remote sessions with equivalent behavior.
- Support complete keyboard operation plus mouse row selection, double-click opening, wheel scrolling, and clickable inspector controls.
- Keep terminal input, refreshes, remote requests, and tmux subprocesses responsive.
- Preserve the repository's current `golang.org/x/term` and `golang.org/x/sys` dependency set.

## Non-goals

- Sending keyboard input to the inspected Claude session.
- Embedding an interactive tmux client.
- Replacing the renderer with Bubble Tea, tcell, termbox, or another TUI framework.
- Pixel-level terminal emulation or reconstructing arbitrary cursor movement from raw session output.
- Adding mouse drag selection or a draggable scrollbar in the first release.
- Changing the server's authentication model or YAML configuration schema.

## User experience

### Session list

- `Up` and `Down` move selection.
- The table owns a viewport and scrolls automatically to keep keyboard selection visible.
- The mouse wheel scrolls the table viewport.
- Single-clicking a session row selects it.
- Double-clicking the same row within approximately 350 milliseconds opens the inspector.
- `Enter` and the existing `p` key open the inspector for the selected session.
- Existing action keys continue to work.
- Empty remote-host rows remain selectable for navigation consistency but do not open an inspector.

### Inspector layout

The inspector uses the approved metadata-strip layout:

1. Title row: session display name, PID, and host.
2. Compact metadata row: model, status, context usage, and cost, collapsed progressively on narrow terminals.
3. Output viewport occupying remaining rows.
4. Footer with Back, Refresh, and Follow controls; source and scroll status; and live/stale state.

The output viewport shows a sanitized, bounded tmux capture when possible. If no tmux pane is available, it shows bounded transcript content.

### Inspector controls

- `Up` and `Down`: scroll one line and leave follow mode.
- Mouse wheel: scroll three lines and leave follow mode.
- `Page Up` and `Page Down`: scroll one viewport height.
- `Home`: move to oldest retained content.
- `End`: move to the bottom and resume follow mode.
- `r`: request an immediate refresh.
- `Esc`, `q`, or `p`: return to the session list.
- `Ctrl-C`: quit the application from either screen.
- Footer Back, Refresh, and Follow regions are clickable.

### Follow behavior

The inspector starts in follow mode, anchored to the bottom. New snapshots keep the viewport at the bottom while follow mode is active.

Scrolling upward disables follow mode and preserves the current top line as new output arrives. The footer changes from `LIVE` to `PAUSED` and shows the number of newly appended lines when that count can be determined. `End` or the Follow control returns to the live bottom.

If retained content rotates or shrinks, the viewport clamps to a valid position without panicking.

## Architecture

### Terminal ownership

`RunTUI` remains the only terminal owner. No new goroutine reads stdin. Raw-mode setup, alternate-screen ownership, output processing, prompt handoff, and interactive attach behavior remain centralized.

The main loop continues to use `unix.Select`. Generalize event polling to accept a set of wake descriptors: the existing remote-hub pipe, the inspector-hub pipe while open, and the resize pipe. Each ready descriptor is drained and reported as a typed wake reason. This preserves source-specific handling without adding a competing stdin reader or merging unrelated ownership into one shared pipe.

### Typed event decoder

Replace the current stateless key parser with a stateful `inputDecoder`. It retains incomplete bytes across reads and emits typed events:

- Character and control keys.
- Arrow, Enter, Home, End, Page Up, and Page Down keys.
- SGR mouse press, release, and wheel events with one-based terminal coordinates normalized into one internal convention.
- Resize events generated through a wake pipe.

Malformed or unsupported escape sequences are ignored or degrade safely to Escape behavior. Parsing never waits indefinitely for an incomplete sequence; the event loop retains control of timing.

### Application state

Introduce explicit TUI state with two screen modes:

- Session list.
- Session inspector.

State includes:

- Selected target ID.
- Session-list viewport offset.
- Current terminal dimensions.
- Last rendered hit regions.
- Last click target and timestamp for double-click detection.
- Inspector state when open.
- Existing sort, view mode, and toast state.

Event handlers mutate state and request actions. They do not print directly.

### Rendering and hit regions

Rendering continues to build a complete frame in memory and clear/redraw the screen. A diff renderer is unnecessary for this scope.

Each render returns hit regions alongside text. A hit region maps terminal coordinates to an action such as:

- Select session target.
- Open session target.
- Inspector Back.
- Inspector Refresh.
- Inspector Follow.

Hit regions are recalculated after every render and resize. Mouse events never infer row identity from stale list indexes.

### Inspector state

Inspector state contains:

- Stable session target ID, including host for remote sessions.
- Current sanitized content lines.
- Source kind: tmux or transcript.
- Top-line viewport offset.
- Follow-bottom flag.
- New-line count while paused.
- Loading, stale, ended, and error status.
- Snapshot generation or timestamp.

The stable target ID prevents a PID on one host from being confused with the same PID on another host.

### Inspector hub

Add an `InspectorHub` following the existing `RemoteHub` ownership pattern. It polls only while the inspector is open and owns preview fetching for the selected target.

Responsibilities:

- Capture local tmux output or load local transcript fallback off the input loop.
- Fetch remote preview output off the input loop.
- Enforce payload and history limits.
- Publish immutable snapshots.
- Wake the main loop when a snapshot changes or a refresh fails.
- Support immediate refresh requests.
- Stop cleanly when the inspector closes or target changes.

No network request or `tmux` subprocess blocks keyboard or mouse processing.

## Preview providers

### Local tmux provider

Resolve the tmux pane using the existing PID-to-pane logic. Capture a bounded amount of scrollback, initially approximately 2,000 lines, while preserving SGR style information.

The provider caps output at approximately 512 KiB. If both limits apply, the smaller retained result wins.

### Transcript fallback

When no tmux pane is available, resolve the transcript using existing session and transcript lookup logic. Render recent meaningful user, assistant, and tool entries up to the same payload bound. System and hook noise remains excluded.

Transcript fallback is read-only and refreshes through the same hub contract.

### Remote provider

Extend the existing preview endpoint with backward-compatible bounded-preview parameters. Existing clients that omit parameters retain valid behavior.

The TUI requests the same bounded tmux capture or transcript fallback used locally. Remote responses are subject to the client-side payload cap even if the server misbehaves.

HTTP timeout or host failure does not clear a previously successful snapshot.

## Terminal and mouse lifecycle

Enable basic button/wheel tracking and SGR coordinates while the application owns the terminal:

- `CSI ?1000h`
- `CSI ?1006h`

Disable both modes before:

- Cooked-mode prompts.
- Interactive tmux or SSH attach.
- Any external process taking terminal ownership.
- Normal shutdown.
- Error cleanup.

Re-enable them each time the application re-enters raw mode, alongside output processing. Cleanup must be idempotent.

Terminals without mouse support remain fully usable through keyboard controls.

## Resize handling

Register `SIGWINCH` and convert it into a wake-pipe notification. The main `unix.Select` loop observes that descriptor, reads the latest terminal size, clamps both viewports, and redraws.

This avoids a second stdin reader and avoids waiting for the periodic refresh tick before correcting hit regions.

## Content safety

Captured tmux and remote content is untrusted terminal data. Before rendering:

- Preserve supported SGR color and style sequences.
- Strip all OSC sequences, including OSC 8 hyperlinks, plus title changes, cursor movement, erase commands, mode changes, and unsupported controls.
- Normalize tabs and line endings.
- Reject or truncate oversized payloads.
- Apply visual-width clipping only during rendering.

Captured content must not be able to move the cursor outside the viewport, alter mouse mode, switch screens, or change terminal title.

## Errors and edge cases

- Initial local or remote load: show a loading state without blocking navigation.
- Refresh failure with prior content: retain content and show `STALE` with a short error.
- Refresh failure without prior content: show an error state and Refresh control.
- Session exits while inspector is open: retain final content and show `SESSION ENDED`.
- Selected session disappears from refreshed list: inspector remains open against its stable target until provider confirms it ended or user returns.
- Snapshot shrinks or rotates: clamp top-line offset and clear invalid new-line counts.
- Tiny terminal: progressively remove low-priority metadata; below a defined minimum, show a concise resize message while retaining Back and quit controls.
- Mouse event outside any current hit region: ignore it.
- Unsupported or malformed mouse sequence: ignore it without affecting keyboard input.

## Expected file changes

Likely new files:

- `tui_events.go` — stateful keyboard and SGR mouse decoding.
- `tui_state.go` — screen state, table viewport, hit regions, and state transitions.
- `inspector.go` — inspector state, viewport behavior, and `InspectorHub`.
- `render_inspector.go` — metadata-strip inspector rendering.

Likely focused changes:

- `tui.go` — orchestration around typed events and screen rendering.
- `render.go` — session-list viewport and hit-region support.
- `preview.go` — bounded providers and transcript fallback contract.
- `actions.go` and `helpers.go` — mouse lifecycle during prompts and interactive handoff.
- `remote_actions.go` — remove blocking preview loop in favor of shared inspector behavior.
- `server.go` — backward-compatible bounded preview parameters.

Exact file boundaries may be adjusted during implementation planning to match existing test seams.

## Testing strategy

### Event decoder tests

- Escape sequences split at every possible byte boundary.
- SGR press, release, and wheel events.
- Coordinate parsing and bounds.
- Arrow, Enter, Home, End, Page Up, and Page Down.
- Malformed, oversized, and unknown sequences.
- Standalone Escape handling.

### State and viewport tests

- Keyboard selection remains visible.
- Table wheel scrolling clamps correctly.
- Single-click selection and same-row double-click opening.
- Different-row or expired second clicks do not open.
- Follow mode anchors to bottom.
- Manual scrolling pauses follow mode.
- New output increments paused count.
- End/Follow resumes live mode.
- Resize, content truncation, and session-ended transitions clamp safely.

### Rendering and hit-region tests

- Local and remote rows map to correct coordinates.
- Empty remote-host rows are not openable.
- Footer regions match rendered labels.
- Metadata collapses in defined order.
- ANSI-safe clipping preserves terminal column calculations.
- Loading, live, paused, stale, ended, and minimum-size states.

### Provider and sanitizer tests

- Bounded tmux capture invocation.
- Transcript fallback when tmux resolution fails.
- Remote success, timeout, non-2xx response, and oversized response.
- Backward-compatible server query behavior.
- Allowed SGR sequences survive.
- OSC, cursor movement, screen switching, mouse-mode changes, and malformed controls are removed.

### Lifecycle tests

- Mouse mode enable and disable output.
- Prompt handoff disables mouse and restores it afterward.
- Interactive attach disables mouse before terminal transfer.
- Cleanup restores wrap, cursor, alternate screen, raw mode, and mouse mode after normal and error exits.

### Manual verification

Run focused manual checks on:

- macOS Terminal and iTerm2.
- Linux terminal.
- Application running inside tmux.
- Local and remote sessions.
- Narrow and resized terminals.
- Terminals where mouse support is disabled or unavailable.

## Delivery plan and estimate

Recommended implementation sequence:

1. Extract typed events and state transitions while preserving current behavior.
2. Add session-list viewport, hit regions, click selection, and wheel navigation.
3. Add inspector rendering and local preview provider.
4. Add scrolling, follow mode, refresh, resize, and footer controls.
5. Add remote parity and stale/error states.
6. Harden sanitizer and terminal lifecycle.
7. Complete automated and cross-platform manual verification.

Expected scope is approximately 6–9 touched files and 700–1,100 lines including tests. A focused implementation should take approximately 5–8 engineering days. The highest-risk work is terminal event decoding and terminal lifecycle cleanup; inspector rendering itself is straightforward.
