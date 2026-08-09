// resize.go
package main

import (
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// resizeTmuxTarget resizes s's tmux window to cols x rows via `tmux
// resize-window`. This pins the window to manual-size mode (tmux stops
// auto-sizing it to the largest attached client) until revertTmuxTarget
// undoes it — see docs/superpowers/specs/2026-08-03-preview-resize-design.md
// for the accepted tradeoffs (scrollback rewrap, a real attach seeing a
// pinned size until revert fires).
//
// s.Tmux is the pane-qualified "session:window.pane" location captureTmuxPreview
// already resolves and uses directly for capture-pane (preview.go); tmux's
// resize-window accepts a pane-qualified target and resolves it to the
// containing window, so no separate window-only lookup is needed.
//
// s.Tmux must be non-empty, matching every other call site against
// Session.Tmux in this codebase (sendKeys, send_keys.go:32).
func resizeTmuxTarget(s Session, cols, rows int) error {
	if s.Tmux == "" {
		return fmt.Errorf("PID %d has no tmux pane", s.PID)
	}
	if cols <= 0 || rows <= 0 {
		return fmt.Errorf("invalid size %dx%d", cols, rows)
	}
	if err := exec.Command("tmux", "resize-window", "-t", s.Tmux,
		"-x", strconv.Itoa(cols), "-y", strconv.Itoa(rows)).Run(); err != nil {
		return fmt.Errorf("resize-window: %w", err)
	}
	return nil
}

// revertTmuxTarget un-pins s's tmux window from manual-size mode via `tmux
// set-window-option -u window-size`, restoring the window's normal
// (typically auto-resize-to-latest-active-client) behavior.
//
// This is NOT `tmux resize-window -A`: that command recalculates the size
// once but leaves the window-size option explicitly set to "manual" (verified
// against a real tmux session — `show-window-options` reports "manual" even
// after `-A`), so a window "reverted" that way never again auto-adjusts to an
// attaching client and stays silently frozen at the last preview's
// dimensions. `-u` (unset) is the only way to actually clear the override.
func revertTmuxTarget(s Session) error {
	if s.Tmux == "" {
		return fmt.Errorf("PID %d has no tmux pane", s.PID)
	}
	if err := exec.Command("tmux", "set-window-option", "-t", s.Tmux, "-u", "window-size").Run(); err != nil {
		return fmt.Errorf("set-window-option -u window-size: %w", err)
	}
	return nil
}

// resizeSession dispatches to resizeTmuxTarget or revertTmuxTarget. This is
// the single function both the server handler's injectable seam and the
// local TUI call site point at, so entry (revert=false) and exit
// (revert=true) share one call shape.
func resizeSession(s Session, cols, rows int, revert bool) error {
	if revert {
		return revertTmuxTarget(s)
	}
	return resizeTmuxTarget(s, cols, rows)
}

// inspectorContentCapMargin is kept below the measured content height when
// shrinking an oversized inspector pane back down, so a session sitting
// right at a wrapped-line boundary doesn't get re-cut immediately below the
// fold.
const inspectorContentCapMargin = 2

// ansiSGR matches a complete SGR ("...m") escape sequence — the only kind of
// escape a preview's Content can still contain, since sanitizeTerminalText
// already strips everything else (preview.go) but deliberately keeps color.
// measuredContentRows needs plain text to judge a line blank, so it strips
// these first; leaving them in would misread a color-only line (e.g. a bare
// reset code) as content.
var ansiSGR = regexp.MustCompile("\x1b\\[[0-9;]*m")

// measuredContentRows returns the 1-based index of the last non-blank line in
// content — how many rows of a capture actually hold rendered output, not
// counting unused rows below it. A blank line embedded between real content
// still counts (it's inside the span), only trailing blank rows are excluded,
// which is what oversizing a pane past what the live app rendered leaves
// behind. Returns 0 for an entirely blank (or empty) capture.
func measuredContentRows(content string) int {
	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
	last := 0
	for i, l := range lines {
		if strings.TrimSpace(ansiSGR.ReplaceAllString(l, "")) != "" {
			last = i + 1
		}
	}
	return last
}

// capInspectorPaneRows decides whether an oversized inspector pane
// (currently `requested` rows tall) should be shrunk back down, based on
// content — a capture of that pane taken right after the oversize resize.
// ok is false when no shrink is warranted: the capture read as blank (a
// failed or too-early probe — shrinking on that would cut a session that
// simply hadn't redrawn yet) or the live app's content already fills
// `requested` rows, meaning the oversize was worth it. Otherwise rows is
// the measured content height plus inspectorContentCapMargin, floored at
// `floor` (the inspector's own physical viewport — never shrink below what
// the user's terminal shows regardless of how little content there is).
func capInspectorPaneRows(content string, requested, floor int) (rows int, ok bool) {
	used := measuredContentRows(content)
	if used <= 0 {
		return 0, false
	}
	capped := used + inspectorContentCapMargin
	if capped < floor {
		capped = floor
	}
	if capped >= requested {
		return 0, false
	}
	return capped, true
}
