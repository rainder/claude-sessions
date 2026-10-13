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

// paneInputText finds Claude Code's input box in a capture-pane -e dump and
// returns the text typed into it. The box is anchored on its prompt line —
// the LAST line starting with the ❯ glyph — and closed by the nearest rule
// line above and below it. Anchoring on the prompt, not on "the last two rule
// lines", matters: Claude Code draws other rows around the box (a background
// task panel with its own rule under the status lines, a notice or queued
// message between the top rule and the prompt, a rule wrapped into two lines
// in a narrow pane), and each of those made the old rule read a normal idle
// box as "not visible".
//
// Box text is the rest of the prompt line plus every line down to the bottom
// rule. Lines between the top rule and the prompt are notices, not input, and
// anything below the bottom rule is status or menus. Dim runs (SGR 2) are the
// empty box's placeholder and are dropped. found is false when there is no
// prompt line or no rule on either side of it. A dialog that draws its own
// "❯ 1. Yes" cursor between two rules reads as non-empty text, so it is
// refused as a busy box, and the landed check after typing still requires
// our own text.
func paneInputText(capture string) (text string, found bool) {
	lines := strings.Split(strings.TrimRight(capture, "\n"), "\n")
	plain := make([]string, len(lines))
	for i, l := range lines {
		plain[i] = visibleText(l)
	}
	p := -1
	for i := len(plain) - 1; i >= 0; i-- {
		if strings.HasPrefix(strings.TrimLeft(plain[i], " "), "❯") {
			p = i
			break
		}
	}
	if p < 0 {
		return "", false
	}
	top := -1
	for i := p - 1; i >= 0; i-- {
		if isRuleLine(plain[i]) {
			top = i
			break
		}
	}
	bottom := -1
	for i := p + 1; i < len(plain); i++ {
		if isRuleLine(plain[i]) {
			bottom = i
			break
		}
	}
	if top < 0 || bottom < 0 {
		return "", false
	}
	var parts []string
	for i := p; i < bottom; i++ {
		l := plain[i]
		if i == p {
			l = strings.TrimPrefix(strings.TrimLeft(l, " "), "❯")
		}
		l = strings.TrimSpace(strings.ReplaceAll(l, "\u00a0", " "))
		if l != "" {
			parts = append(parts, l)
		}
	}
	return strings.Join(parts, " "), true
}

// isRuleLine reports the border Claude Code draws above and below its input
// box: at least 10 ─ in all, starting and ending with ─. A named session puts
// its name into the rule ("──────── FLEET ─"), so one contiguous label of at
// most 80 runes between ─ runs is allowed too; a line made only of ─ still
// qualifies. Two labels, or text at either end, is not a rule.
func isRuleLine(s string) bool {
	s = strings.TrimSpace(s)
	if strings.Count(s, "─") < 10 || !strings.HasPrefix(s, "─") || !strings.HasSuffix(s, "─") {
		return false
	}
	label := strings.Trim(s, "─")
	if label == "" {
		return true
	}
	return !strings.Contains(label, "─") && utf8.RuneCountInString(label) <= 80
}

// visibleText strips CSI escape sequences from one captured line and drops
// every rune printed while the dim attribute (SGR 2) is on.
func visibleText(line string) string { return paneLineText(line, true) }

// stripPaneEscapes strips CSI escape sequences and other control bytes from
// one captured line but keeps dim text: for a log a person reads, where the
// placeholder is part of what the screen showed. C1 controls (U+0080..U+009F)
// go too: U+009B is a one-byte CSI that some terminals act on when the
// server runs in the foreground.
func stripPaneEscapes(line string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			return -1
		}
		return r
	}, paneLineText(line, false))
}

func paneLineText(line string, dropDim bool) string {
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
		if !dim || !dropDim {
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
