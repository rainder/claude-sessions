# Flicker-Free TUI Rendering Design

**Date:** 2026-07-22
**Status:** Approved

## Goal

Remove visible blank flashes and unnecessary repainting from every alternate-screen view:

- session list
- inspector
- help
- new-session picker

Rendering must remain stable during keyboard navigation, mouse scrolling, automatic refreshes, remote updates, terminal resize, prompts, and interactive subprocess handoffs. It must work directly in common terminals and through tmux without adding dependencies.

## Current Problem

The TUI currently prefixes full frames with `\x1b[H\x1b[J`. Clearing the display and repainting are separate terminal operations even when sent in one string. A terminal or tmux can display the cleared screen before processing the remaining bytes, producing visible flicker. Every redraw also rewrites unchanged rows.

The same clear-before-paint pattern appears in the session list, inspector, help screen, and new-session picker.

## Non-Goals

- Changing table, inspector, picker, or help content and layout
- Adding marquee animation
- Adding a terminal UI dependency
- Detecting terminal brands or maintaining a capability database
- Changing input, selection, scrolling, mouse, refresh, or terminal ownership semantics

## Chosen Approach

Use both rendering layers:

1. A line-diff renderer that works without terminal-specific atomic-update support
2. DEC synchronized-output markers around each non-empty patch for terminals and tmux versions that support them

Unsupported terminals ignore the private synchronized-output mode and still receive the line diff. The renderer never clears the display before drawing a frame.

## Architecture

Add a small `screenRenderer` component, isolated from content generation. It owns:

- output `io.Writer`
- previously emitted logical rows
- previous terminal width and height
- cache-valid state

Its public operations are:

- `Draw(content string, cols, rows int) error`: normalize, diff, and emit a frame
- `Invalidate()`: discard confidence in terminal contents so the next valid-size draw repaints every terminal row

`RunTUI` remains the only owner of terminal lifecycle, stdin, raw mode, alternate-screen mode, mouse mode, and top-level event dispatch.

Existing render functions continue building content and hit metadata. They do not emit cursor movement or clear-screen commands.

## Frame Model

For valid dimensions (`cols > 0 && rows > 0`), `Draw` creates a normalized frame:

1. Split content on newlines.
2. Clip each logical row to `cols` using existing ANSI-aware `clipLine` behavior.
3. Truncate content beyond `rows`.
4. Pad with empty rows until the frame contains exactly `rows` rows.

Padding makes removed content explicit. A shorter frame changes old rows to empty rows, so stale terminal text is cleared without a full-display erase.

The cache stores the normalized rows and geometry only after the whole patch is written successfully.

## Patch Algorithm

For each normalized row:

- If cache is valid, geometry is unchanged, and row bytes equal cached row bytes, emit nothing.
- Otherwise emit absolute cursor positioning for that row, row content, an SGR reset, and erase-to-end-of-line.

A non-empty patch has this order:

1. `CSI ? 2026 h` — begin synchronized output
2. changed row patches, each using `CSI <row>;1 H`
3. `CSI ? 2026 l` — end synchronized output

Patch bytes are assembled in memory and passed to the writer through one `Write` call. A short write counts as an error.

An identical frame emits no bytes and performs no writer call.

The renderer does not use `CSI J`, `CSI 2J`, or alternate-screen toggles during ordinary frame updates.

## Style Safety

Every changed row ends with the global ANSI reset before erase-to-end-of-line. This prevents style state from leaking into later rows or terminal contents. Existing styling inside each row remains unchanged.

Raw styled row bytes are compared. A style-only change therefore repaints the row even when visible text is identical.

## Unknown-Size Fallback

When terminal dimensions are unavailable or non-positive, safe row addressing and padding are impossible. The renderer:

- invalidates its cache
- emits synchronized-output begin, home cursor, complete content, ANSI reset, and synchronized-output end in one write
- does not clear the display

The next draw with valid dimensions performs a full normalized repaint.

## Integration

### Session list

`RunTUI` builds the existing `tableFrame`, crops it, and converts the toast into a normal logical final row instead of embedding an absolute cursor-position sequence. The resulting content goes through the shared `screenRenderer`.

Hit regions remain derived from the same cropped frame and keep current coordinates.

### Inspector

`RenderInspector` continues writing content into a buffer and returning hit regions. `RunTUI` passes that content to the shared renderer instead of prefixing it with home-and-clear commands.

### Help

Help rendering becomes a pure content builder. `RunTUI` draws help through the same renderer, waits for one input event, then draws the session list again. No direct clear-screen print remains.

### New-session picker

Picker content no longer includes home-and-clear commands. `pickNewSession` owns a short-lived local `screenRenderer`, reads current terminal dimensions for each draw, and uses row diffs while arrow keys move selection.

The main renderer is invalidated before entering the picker and again when returning because another renderer owned the screen temporarily.

### Prompts and interactive subprocesses

Actions that print prompts, switch cooked/raw mode, or hand the terminal to tmux/ssh can mutate screen contents outside the renderer. `RunTUI` invalidates the main renderer before those actions. The existing post-action render then repaints every terminal row.

Interactive primary-screen handoff behavior remains unchanged. Re-entering the alternate screen always leads to an invalidated full repaint.

### Resize and screen transitions

A geometry change automatically forces a full normalized repaint. `RunTUI` also invalidates before entering or leaving each top-level view (session list, inspector, and help), making every view boundary an explicit full repaint without a display clear.

## Error Handling

`Draw` returns writer errors and `io.ErrShortWrite` for incomplete writes. On any failure it sets cache-invalid state; it never records a frame that may not have reached the terminal.

The TUI treats drawing as best-effort, matching current stdout behavior. A transient write failure does not terminate session management. A later draw retries as a full repaint.

## Compatibility

- No new dependency
- Uses existing ANSI/CSI terminal assumptions already required by alternate-screen, cursor, color, and mouse support
- Line diff remains useful when synchronized output is unsupported
- Synchronized-output markers improve atomicity where supported
- tmux and direct-terminal workflows remain supported
- Existing single-stdin-consumer and output-processing invariants remain unchanged

## Tests

Add focused renderer tests covering:

- first valid-size draw emits every terminal row without a display-clear sequence
- identical second frame emits zero bytes and performs no write
- one changed row emits only its cursor-targeted patch
- shorter replacement erases stale suffix
- removed rows become blank row patches
- geometry change forces full repaint
- `Invalidate` forces full repaint
- every non-empty patch has balanced synchronized-output markers
- each patch uses one writer call
- short writes and writer errors invalidate cache
- each changed row ends with style reset and erase-to-end-of-line
- ANSI-styled rows compare and clip correctly
- unknown dimensions use full-content fallback and invalidate cache

Update integration tests to verify:

- session-list and inspector paint paths no longer emit home-and-clear prefixes
- toast occupies the final logical frame row
- help content contains no cursor or display-clear commands
- picker content contains no cursor or display-clear commands
- picker redraws use the renderer
- hit-region behavior and existing rendered content remain unchanged

Run:

```sh
go test ./...
go vet ./...
go build .
```

## Manual Acceptance

Verify directly and inside tmux:

1. Hold navigation keys on the session list. No blank flashes appear.
2. Leave automatic and remote refreshes running. Unchanged rows stay stable.
3. Scroll the inspector continuously. No full-screen clearing appears.
4. Cycle view and sort modes.
5. Open and close help.
6. Navigate and cancel the new-session picker.
7. Resize the terminal repeatedly.
8. Enter and return from confirmation prompts.
9. Attach to an interactive session and return to the TUI.

Success means no visible clear-screen flash during any alternate-screen redraw, stale text never survives shorter frames, and existing interaction behavior remains unchanged.
