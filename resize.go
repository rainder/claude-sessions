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

// inspectorContentCapMargin is how many blank rows of a shrunk gap are kept
// (rather than cut down to zero) when capInspectorPaneRows shrinks an
// oversized inspector pane — natural breathing room, and a hedge against a
// session sitting right at a wrapped-line boundary.
const inspectorContentCapMargin = 2

// inspectorGapShrinkThreshold is the minimum length of a single contiguous
// blank run before it's treated as unwanted padding — from oversizing the
// pane past what the live app actually rendered — rather than a normal
// one- or two-line spacer the app itself puts between messages. Verified
// against a real Claude Code session preview: a 4x-oversized pane showed a
// single 28-line blank run between the end of the conversation and the
// status-line footer pinned at the pane's bottom row (see below).
const inspectorGapShrinkThreshold = 6

// ansiSGR matches a complete SGR ("...m") escape sequence — the only kind of
// escape a preview's Content can still contain, since sanitizeTerminalText
// already strips everything else (preview.go) but deliberately keeps color.
// Blank-line detection needs plain text, so it strips these first; leaving
// them in would misread a color-only line (e.g. a bare reset code) as
// content.
var ansiSGR = regexp.MustCompile("\x1b\\[[0-9;]*m")

// largestBlankRun returns the length of the longest contiguous run of blank
// (ANSI-SGR-stripped, whitespace-trimmed) lines in content, and the total
// line count. Both are needed by capInspectorPaneRows: shrinking a pane by
// "how much is blank" only works against the total it was captured at.
func largestBlankRun(content string) (run, total int) {
	lines := strings.Split(strings.TrimSuffix(content, "\n"), "\n")
	total = len(lines)
	best, cur := 0, 0
	for _, l := range lines {
		if strings.TrimSpace(ansiSGR.ReplaceAllString(l, "")) == "" {
			cur++
			if cur > best {
				best = cur
			}
			continue
		}
		cur = 0
	}
	return best, total
}

// capInspectorPaneRows decides whether an oversized inspector pane
// (currently `requested` rows tall) should be shrunk back down, based on
// content — a capture of that pane taken right after the oversize resize.
//
// It measures the single largest contiguous blank run, not "content height
// from the top" or "content height minus trailing blanks": Claude Code's own
// TUI (and most full-redraw terminal apps) pins its status/input footer to
// the pane's last row regardless of how tall the pane is, so on a short
// conversation the wasted space is a gap *between* the conversation and the
// footer, not trailing blank rows after everything real — the footer itself
// is real, non-blank content sitting at the very bottom. A trailing-blanks-only
// measure sees that footer and concludes the pane is nearly full, so it never
// shrinks anything; this is why that approach was replaced (verified empirically
// against a live remote session's preview capture before shipping).
//
// ok is false when no shrink is warranted: the capture read as entirely blank
// (a failed or too-early probe, before the live app has redrawn at the new
// size — shrinking on that would cut a session that simply hadn't caught up
// yet) or no single gap is wide enough to look like oversize padding rather
// than the app's own normal spacing (inspectorGapShrinkThreshold). Otherwise
// rows is `requested` minus the gap, trimmed down to inspectorContentCapMargin
// rather than removed outright, floored at `floor` (the inspector's own
// physical viewport — never shrink below what the user's terminal shows).
func capInspectorPaneRows(content string, requested, floor int) (rows int, ok bool) {
	if content == "" {
		return 0, false
	}
	run, total := largestBlankRun(content)
	if run >= total {
		// Entirely (or all-but-entirely) blank: not real content to size
		// against, most likely a probe taken before the app redrew.
		return 0, false
	}
	if run <= inspectorGapShrinkThreshold {
		return 0, false
	}
	capped := total - (run - inspectorContentCapMargin)
	if capped < floor {
		capped = floor
	}
	if capped >= requested {
		return 0, false
	}
	return capped, true
}
