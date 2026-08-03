// resize.go
package main

import (
	"fmt"
	"os/exec"
	"strconv"
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
