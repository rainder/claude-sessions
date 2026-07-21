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
)

// tuiState is the mutable screen state owned by the render loop: which screen
// is showing, the current selection, the session-list scroll offset, the hit
// regions for the frame last drawn, and the double-click tracking pair.
//
// Task 6 adds an `inspector inspectorViewState` field for the fullscreen
// inspector screen; it is intentionally omitted until then.
type tuiState struct {
	mode        screenMode
	sel         string
	listOffset  int
	hits        []hitRegion
	lastClickID string
	lastClickAt time.Time
}

// newTUIState starts on the session list with no selection.
func newTUIState() *tuiState {
	return &tuiState{mode: screenSessions}
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
// events are ignored; wheel events scroll three lines; a left press selects the
// row under the cursor; a second left press on the same openable row within
// doubleClickWindow opens the inspector.
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
		s.sel = hit.targetID
		if doubleClick {
			// Reset so a third quick click starts a fresh single-click.
			s.lastClickID = ""
			s.lastClickAt = time.Time{}
			return commandOpenInspector
		}
		s.lastClickID = hit.targetID
		s.lastClickAt = now
		return commandRender
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
