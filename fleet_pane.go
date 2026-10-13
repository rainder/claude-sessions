package main

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"unicode/utf8"
)

// capturePane returns the visible content of a tmux pane with SGR escapes
// kept (-e): the dim attribute is the only thing that tells Claude Code's
// placeholder text ("Try …") apart from text a person has typed.
func capturePane(target string) (string, error) {
	if target == "" {
		return "", fmt.Errorf("no tmux pane")
	}
	out, err := exec.Command("tmux", "capture-pane", "-p", "-e", "-t", target).Output()
	if err != nil {
		return "", fmt.Errorf("capture pane: %w", err)
	}
	return string(out), nil
}

// paneInputText finds Claude Code's input box in a capture-pane -e dump —
// the region between the last two rule lines, whose first line starts with
// the ❯ prompt glyph — and returns the text typed into it. Dim runs (SGR 2)
// are the empty box's placeholder and are dropped. found is false when no
// such box is visible: a permission prompt or question dialog has replaced
// it, and typed text plus Enter would answer that dialog instead.
func paneInputText(capture string) (text string, found bool) {
	lines := strings.Split(strings.TrimRight(capture, "\n"), "\n")
	plain := make([]string, len(lines))
	for i, l := range lines {
		plain[i] = visibleText(l)
	}
	bottom := -1
	for i := len(plain) - 1; i >= 0; i-- {
		if isRuleLine(plain[i]) {
			bottom = i
			break
		}
	}
	if bottom < 0 {
		return "", false
	}
	top := -1
	for i := bottom - 1; i >= 0; i-- {
		if isRuleLine(plain[i]) {
			top = i
			break
		}
	}
	if top < 0 || bottom-top < 2 {
		return "", false
	}
	first := strings.TrimLeft(plain[top+1], " ")
	if !strings.HasPrefix(first, "❯") {
		return "", false
	}
	var parts []string
	for i := top + 1; i < bottom; i++ {
		l := plain[i]
		if i == top+1 {
			l = strings.TrimPrefix(strings.TrimLeft(l, " "), "❯")
		}
		l = strings.TrimSpace(strings.ReplaceAll(l, "\u00a0", " "))
		if l != "" {
			parts = append(parts, l)
		}
	}
	return strings.Join(parts, " "), true
}

// isRuleLine reports a line made only of ─ (at least 10), the border Claude
// Code draws above and below its input box.
func isRuleLine(s string) bool {
	s = strings.TrimSpace(s)
	if strings.Count(s, "─") < 10 {
		return false
	}
	return strings.Trim(s, "─") == ""
}

// visibleText strips CSI escape sequences from one captured line and drops
// every rune printed while the dim attribute (SGR 2) is on.
func visibleText(line string) string {
	var b strings.Builder
	dim := false
	for i := 0; i < len(line); {
		if line[i] == 0x1b && i+1 < len(line) && line[i+1] == '[' {
			j := i + 2
			for j < len(line) && (line[j] < 0x40 || line[j] > 0x7e) {
				j++
			}
			if j < len(line) && line[j] == 'm' {
				dim = applySGRDim(line[i+2:j], dim)
			}
			i = j + 1
			continue
		}
		r, size := rune(line[i]), 1
		if line[i] >= 0x80 {
			r, size = utf8.DecodeRuneInString(line[i:])
		}
		if !dim {
			b.WriteRune(r)
		}
		i += size
	}
	return b.String()
}

// applySGRDim folds one SGR parameter list into the dim state: 2 sets it,
// 0 / empty (reset) and 22 (normal intensity) clear it. 38;5;N and 38;2;R;G;B
// arguments are skipped so a colour index of 2 is not read as dim.
func applySGRDim(params string, dim bool) bool {
	if params == "" {
		return false
	}
	fields := strings.Split(params, ";")
	for i := 0; i < len(fields); i++ {
		n, err := strconv.Atoi(fields[i])
		if err != nil {
			continue
		}
		switch {
		case n == 38 || n == 48 || n == 58:
			if i+1 < len(fields) && fields[i+1] == "5" {
				i += 2
			} else if i+1 < len(fields) && fields[i+1] == "2" {
				i += 4
			}
		case n == 0 || n == 22:
			dim = false
		case n == 2:
			dim = true
		}
	}
	return dim
}
