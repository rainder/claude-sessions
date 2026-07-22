package main

import (
	"strings"
	"time"
)

// screenMode is which top-level screen the TUI is showing.
type screenMode uint8

const (
	screenSessions screenMode = iota
	screenInspector
)

// hitAction identifies what a clickable region does when activated. The
// inspector actions are declared here but only wired up by later tasks.
type hitAction uint8

const (
	hitSelectSession hitAction = iota
	hitInspectorBack
	hitInspectorRefresh
	hitInspectorFollow
)

// hitRegion is a rectangular clickable area on the current screen, addressed in
// zero-based terminal cells with inclusive bounds.
type hitRegion struct {
	x0, y0, x1, y1 int
	action         hitAction
	targetID       string
	openable       bool
}

// tableRow records where a selectable row landed in a rendered table frame and
// what it points at. targetID mirrors the row's selectionTarget.id, so a click
// resolves to the same target keyboard navigation would.
type tableRow struct {
	line     int
	targetID string
	openable bool
}

// tableFrame is a fully-rendered session table plus the metadata needed to map
// clicks and scrolling back onto rows. lines are the frame text split on
// newline; overflowing mirrors RenderAll's marquee-overflow signal.
type tableFrame struct {
	lines       []string
	rows        []tableRow
	overflowing bool
}

// targetLine returns the frame line index for a target ID, or -1 if absent.
func (f tableFrame) targetLine(id string) int {
	for _, r := range f.rows {
		if r.targetID == id {
			return r.line
		}
	}
	return -1
}

// visibleFrame is a viewport-cropped slice of a tableFrame: the visible text
// and the hit regions for the rows it contains, addressed in viewport-local
// (zero-based) coordinates.
type visibleFrame struct {
	text string
	hits []hitRegion
}

// cropTableFrame selects at most rows lines of frame starting at offset, clips
// each to cols visible columns, and emits a full-width hitRegion for every
// table row that falls inside the window (its y set to line-offset). offset and
// the window are clamped to the frame so out-of-range callers get an empty crop
// rather than a panic.
func cropTableFrame(frame tableFrame, offset, rows, cols int) visibleFrame {
	total := len(frame.lines)
	if offset < 0 {
		offset = 0
	}
	if offset > total {
		offset = total
	}
	end := offset + rows
	if rows < 0 {
		end = offset
	}
	if end > total {
		end = total
	}

	visible := frame.lines[offset:end]
	clipped := make([]string, len(visible))
	for i, line := range visible {
		clipped[i] = clipLine(line, cols)
	}

	var hits []hitRegion
	for _, r := range frame.rows {
		if r.line < offset || r.line >= end {
			continue
		}
		y := r.line - offset
		hits = append(hits, hitRegion{
			x0: 0, y0: y, x1: cols - 1, y1: y,
			action:   hitSelectSession,
			targetID: r.targetID,
			openable: r.openable,
		})
	}
	return visibleFrame{text: strings.Join(clipped, "\n"), hits: hits}
}

// withBottomRow pads or truncates content so bottom occupies the final terminal
// row. The screen renderer performs width clipping and final frame padding.
func withBottomRow(content string, rows int, bottom string) string {
	if rows <= 0 {
		return content
	}
	lines := strings.Split(content, "\n")
	bodyRows := rows - 1
	if len(lines) > bodyRows {
		lines = lines[:bodyRows]
	}
	for len(lines) < bodyRows {
		lines = append(lines, "")
	}
	lines = append(lines, bottom)
	return strings.Join(lines, "\n")
}

// doubleClickWindow is the maximum gap between two clicks on the same row that
// still counts as a double-click (which opens the inspector).
const doubleClickWindow = 350 * time.Millisecond

// tuiCommand is the side effect a state handler asks the render loop to
// perform. The inspector commands are declared here but only acted on by later
// tasks.
type tuiCommand uint8

const (
	commandNone tuiCommand = iota
	commandRender
	commandOpenInspector
	commandBack
	commandRefreshInspector
	commandFollowInspector
	commandQuit
)

// sessionKeyCommand maps a keystroke on the session-list screen to the
// screen-transition command it triggers, or commandNone when the key is not a
// screen transition (navigation, actions, sort, etc. are handled inline by the
// render loop because they need runtime dependencies). Right/p/P open the
// fullscreen inspector (preview) for the selected row; Enter attaches (handled
// inline by the render loop, not here).
func sessionKeyCommand(key string) tuiCommand {
	switch key {
	case KeyRight, "p", "P":
		return commandOpenInspector
	default:
		return commandNone
	}
}

// inspectorKeyCommand maps a keystroke on the inspector screen to a fixed
// loop-level command independent of scroll state: Back on Esc/q/Q/p/P (p toggles
// the inspector, mirroring its open key), Quit on Ctrl-C/Ctrl-D. Scrolling and
// refresh/follow keys return commandNone here and are dispatched through
// handleInspectorKey, which mutates the viewport.
func inspectorKeyCommand(key string) tuiCommand {
	switch key {
	case KeyEsc, "q", "Q", "p", "P":
		return commandBack
	case "\x03", "\x04":
		return commandQuit
	default:
		return commandNone
	}
}

// pendingSpawn is a just-spawned session's landing target, held across refreshes
// until a snapshot surfaces its tmux pane so settleSelection can move onto it.
// New local metadata can lag and the first remote snapshot after a spawn is
// necessarily stale, so a one-shot post-spawn lookup would miss; retaining the
// intent lets every later refresh retry until the row appears. host is the
// session's host label ("" for local); tmux is the spawned tmux session name.
type pendingSpawn struct {
	host string
	tmux string
}

// tuiState is the mutable screen state owned by the render loop: which screen
// is showing, the current selection, the session-list scroll offset, the hit
// regions for the frame last drawn, the double-click tracking pair, and the
// fullscreen inspector's scroll/follow state.
type tuiState struct {
	mode       screenMode
	sel        string
	listOffset int
	// pending holds a just-spawned session's landing target until a refresh
	// finds its tmux pane (settleSelection); nil when there is no spawn to chase.
	// Explicit navigation (keyboard nav, click-select) clears it.
	pending *pendingSpawn
	// anchorSelection requests that the next session-list render scroll the
	// selected row into view. It is set when the selection changes (keyboard
	// nav, click-select, or a validateTargetSel fallback) and cleared once
	// consumed; plain re-renders and wheel scrolling leave it unset, so the
	// viewport is free to move off the selection.
	anchorSelection bool
	hits            []hitRegion
	lastClickID     string
	lastClickAt     time.Time
	inspector       inspectorViewState
	// inspectorTargetGone is set when the inspected session has left the
	// session list; render overlays a terminal "ended" verdict so the view
	// stops reading as live while preserving the last content.
	inspectorTargetGone bool
}

// newTUIState starts on the session list with no selection.
func newTUIState() *tuiState {
	return &tuiState{mode: screenSessions}
}

// settleSelection reconciles the selection against a freshly-built set of
// targets, called on every refresh after buildSelectionTargets. When a spawn is
// pending it first tries to land the selection on that session's tmux pane: on a
// hit it selects the row (anchoring if the selection moved) and clears the
// intent; while the pane is still absent it keeps both the pending intent and
// the current selection. Otherwise (or once settled) it falls back to
// validateTargetSel so a vanished selected row drops to a valid target,
// anchoring whenever the effective selection changes.
func (s *tuiState) requestSelectionAnchor() {
	s.anchorSelection = true
}

func (s *tuiState) settleSelection(targets []selectionTarget) {
	if s.pending != nil {
		if id := selectionForTmux(targets, s.pending.host, s.pending.tmux); id != "" {
			if s.sel != id {
				s.sel = id
				s.anchorSelection = true
			}
			s.pending = nil
			return
		}
	}
	prevSel := s.sel
	s.sel = validateTargetSel(targets, s.sel)
	if s.sel != prevSel {
		s.anchorSelection = true
	}
}

// navigate moves the selection by delta over targets and requests a re-anchor.
// It is explicit user navigation, so it cancels any pending post-spawn intent:
// once the user has moved the cursor themselves, a later spawn snapshot must not
// yank the selection away.
func (s *tuiState) navigate(targets []selectionTarget, delta int) {
	s.pending = nil
	s.sel = navTargets(targets, s.sel, delta)
	s.anchorSelection = true
}

// hitAt returns the first hit region containing the cell (x, y), or nil.
func (s *tuiState) hitAt(x, y int) *hitRegion {
	for i := range s.hits {
		h := &s.hits[i]
		if x >= h.x0 && x <= h.x1 && y >= h.y0 && y <= h.y1 {
			return h
		}
	}
	return nil
}

// handleListMouse applies a mouse event to the session-list screen. Release
// events are ignored; wheel events scroll three lines and leave the selection
// where it is (free scroll — the render path does not re-anchor to it); a left
// press selects the row under the cursor and requests a re-anchor; a second
// left press on the same openable row within doubleClickWindow opens the
// inspector.
func (s *tuiState) handleListMouse(m mouseEvent, now time.Time) tuiCommand {
	if m.release {
		return commandNone
	}
	switch m.button {
	case mouseWheelUp:
		s.listOffset -= 3
		if s.listOffset < 0 {
			s.listOffset = 0
		}
		return commandRender
	case mouseWheelDown:
		s.listOffset += 3
		return commandRender
	case mouseLeft:
		hit := s.hitAt(m.x, m.y)
		if hit == nil {
			return commandNone
		}
		doubleClick := hit.openable &&
			s.lastClickID == hit.targetID &&
			!s.lastClickAt.IsZero() &&
			now.Sub(s.lastClickAt) <= doubleClickWindow
		// Clicking a row is explicit navigation: drop any pending post-spawn
		// intent so a later spawn snapshot can't override the user's choice.
		s.pending = nil
		s.sel = hit.targetID
		if doubleClick {
			// Reset so a third quick click starts a fresh single-click.
			s.lastClickID = ""
			s.lastClickAt = time.Time{}
			return commandOpenInspector
		}
		s.lastClickID = hit.targetID
		s.lastClickAt = now
		s.anchorSelection = true
		return commandRender
	default:
		return commandNone
	}
}

// resolveListOffset settles the session-list scroll offset for a freshly-built
// frame of viewRows visible rows. When a selection change requested a re-anchor
// it scrolls the selected row into view exactly once (via ensureLineVisible)
// and clears the request; otherwise it preserves the current wheel-driven
// offset. Either way the result is clamped to the frame's valid range. The
// frame's phantom trailing "" line (from the terminating newline) is excluded
// from the effective line count.
func (s *tuiState) resolveListOffset(frame tableFrame, viewRows int) {
	effLines := len(frame.lines)
	if effLines > 0 {
		effLines--
	}
	if s.anchorSelection {
		if line := frame.targetLine(s.sel); line >= 0 {
			s.listOffset = ensureLineVisible(s.listOffset, line, viewRows, effLines)
		}
		s.anchorSelection = false
	}
	maxOff := effLines - viewRows
	if maxOff < 0 {
		maxOff = 0
	}
	if s.listOffset > maxOff {
		s.listOffset = maxOff
	}
	if s.listOffset < 0 {
		s.listOffset = 0
	}
}

// handleInspectorKey applies a keystroke to the inspector viewport and returns
// the command the render loop should run. Pure scrolling (arrows, page keys,
// Home) mutates the view state directly and asks for a repaint; Back, Refresh,
// and Follow defer to the render loop via their commands because they touch the
// hub (leave the screen, refetch, or jump to the live tail and resume polling).
func (s *tuiState) handleInspectorKey(key string) tuiCommand {
	switch key {
	case "q", "Q", KeyEsc, KeyLeft:
		return commandBack
	case "r", "R":
		return commandRefreshInspector
	case "G", KeyEnd:
		return commandFollowInspector
	case "g", KeyHome:
		s.inspector.home()
		return commandRender
	case "k", KeyUp:
		s.inspector.scroll(-1)
		return commandRender
	case "j", KeyDown:
		s.inspector.scroll(1)
		return commandRender
	case KeyPageUp:
		s.inspector.page(-1)
		return commandRender
	case " ", KeyPageDown:
		s.inspector.page(1)
		return commandRender
	default:
		return commandNone
	}
}

// handleInspectorMouse applies a mouse event to the inspector viewport. Release
// events are ignored; the wheel scrolls three lines; a left click on a control
// region returns that control's command (Back, Refresh, or Follow).
func (s *tuiState) handleInspectorMouse(m mouseEvent) tuiCommand {
	if m.release {
		return commandNone
	}
	switch m.button {
	case mouseWheelUp:
		s.inspector.scroll(-3)
		return commandRender
	case mouseWheelDown:
		s.inspector.scroll(3)
		return commandRender
	case mouseLeft:
		hit := s.hitAt(m.x, m.y)
		if hit == nil {
			return commandNone
		}
		switch hit.action {
		case hitInspectorBack:
			return commandBack
		case hitInspectorRefresh:
			return commandRefreshInspector
		case hitInspectorFollow:
			return commandFollowInspector
		}
		return commandNone
	default:
		return commandNone
	}
}

// ensureLineVisible adjusts a viewport's scroll offset so that line sits inside
// the viewportRows-tall window, scrolling the minimum distance, then clamps the
// result to the valid range for a frame of totalLines.
func ensureLineVisible(offset, line, viewportRows, totalLines int) int {
	if viewportRows <= 0 {
		return offset
	}
	if line < offset {
		offset = line
	} else if line >= offset+viewportRows {
		offset = line - viewportRows + 1
	}
	maxOffset := totalLines - viewportRows
	if maxOffset < 0 {
		maxOffset = 0
	}
	if offset > maxOffset {
		offset = maxOffset
	}
	if offset < 0 {
		offset = 0
	}
	return offset
}
