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
// tmuxExactTarget makes a "session:win.pane" address match its session name
// exactly. A plain -t falls back to a prefix match when the name is gone
// (tmux 3.4: with only "spawn-x-retry" alive, "-t spawn-x:0.0" resolves to
// it), so keys meant for an ended session would land in a different one; the
// "=" form errors instead. An address that already starts with "=" or "%"
// (a pane id) is left alone; "" stays "". Idempotent.
func tmuxExactTarget(addr string) string {
	if addr == "" || strings.HasPrefix(addr, "=") || strings.HasPrefix(addr, "%") {
		return addr
	}
	return "=" + addr
}

func capturePane(target string) (string, error) {
	if target == "" {
		return "", fmt.Errorf("no tmux pane")
	}
	out, err := exec.Command("tmux", "capture-pane", "-p", "-e", "-t", tmuxExactTarget(target)).Output()
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

// grokBoxMaxLines bounds how far grokPaneInputText walks up from the bottom
// border looking for the top one. A composer taller than this is not one
// FLEET would type into.
const grokBoxMaxLines = 40

// isGrokScrollbar reports a block element (U+2580–U+259F), which grok's
// composer scrollbar draws at the right edge of an interior line once a long
// text scrolls (█ and the partial ▇ seen live).
func isGrokScrollbar(r rune) bool { return r >= 0x2580 && r <= 0x259f }

// paneInputTextFor reads the input box of row's tool: grok draws a rounded
// box, claude two rule lines around a ❯ prompt.
func paneInputTextFor(row Session, capture string) (text string, found bool) {
	if row.IsGrok() {
		return grokPaneInputText(capture)
	}
	return paneInputText(capture)
}

// grokPaneInputText finds the Grok CLI's composer in a capture-pane -e dump
// and returns the text typed into it. The composer is a rounded box:
//
//	╭──────────────────────╮
//	│ ❯ typed text         │
//	│   wrapped line       │
//	╰──── Grok 4.7 (low) ─╯
//
// It is anchored on the LAST bottom border (╰…╯, which carries a model label),
// then walked up: every line must be a │…│ side line until the ╭…╮ top
// border. Any other line, or no top within grokBoxMaxLines, is not a box.
// Notices grok draws above the box ("Queued · Enter to send now") sit outside
// it and are ignored. The first interior line must start with ❯; an empty box
// is that glyph and spaces. Dim runs are dropped as for claude. Grok's
// empty-box suggestion is not dim: it is italic (SGR 3) in a faint grey
// (38;5;240). composerText drops that too, so a ghost does not read as
// typed text. An
// ask_user_question dialog hides the composer entirely, so it reads as not
// found and a send refuses. Grok also shows an empty box during a busy turn,
// so the screen alone cannot tell busy. A page send still types: Enter
// queues. An idle event refuses a busy grok row before it looks.
func grokPaneInputText(capture string) (text string, found bool) {
	lines := strings.Split(strings.TrimRight(capture, "\n"), "\n")
	bottom := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if isGrokBorder(paneBoxLine(lines[i]), "╰", "╯") {
			bottom = i
			break
		}
	}
	if bottom < 0 {
		return "", false
	}
	top := -1
	for i := bottom - 1; i >= 0 && bottom-i <= grokBoxMaxLines; i-- {
		l := paneBoxLine(lines[i])
		if isGrokBorder(l, "╭", "╮") {
			top = i
			break
		}
		if !strings.HasPrefix(l, "│") || !strings.HasSuffix(l, "│") || utf8.RuneCountInString(l) < 2 {
			return "", false
		}
	}
	if top < 0 || top+1 == bottom {
		return "", false
	}
	var parts []string
	for i := top + 1; i < bottom; i++ {
		// A text taller than the composer scrolls, and grok draws the
		// scrollbar thumb inside the right border. composerText also drops
		// the italic grey suggestion an empty box shows.
		l := composerText(lines[i])
		l = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(l), "│"), "│")
		l = strings.TrimSpace(strings.TrimRightFunc(strings.TrimSpace(l), isGrokScrollbar))
		if i == top+1 {
			if !strings.HasPrefix(l, "❯") {
				return "", false
			}
			l = strings.TrimSpace(strings.TrimPrefix(l, "❯"))
		}
		if l != "" {
			parts = append(parts, l)
		}
	}
	return strings.Join(parts, " "), true
}

// paneBoxLine is one captured line's visible text, NBSP as space, trimmed.
func paneBoxLine(line string) string {
	return strings.TrimSpace(strings.ReplaceAll(visibleText(line), "\u00a0", " "))
}

// isGrokBorder reports a top (╭…╮) or bottom (╰…╯) border of grok's
// composer: the corners around at least 10 ─, with at most one label of at
// most 80 runes between ─ runs, in isRuleLine's spirit. The bottom one carries
// the model ("Grok 4.7 (high) · always-approve"); the top one carries the
// session title once it was set with /rename ("FLEET"), which is exactly the
// session FLEET is.
func isGrokBorder(s, left, right string) bool {
	if !strings.HasPrefix(s, left) || !strings.HasSuffix(s, right) {
		return false
	}
	mid := strings.TrimSuffix(strings.TrimPrefix(s, left), right)
	if strings.Count(mid, "─") < 10 || !strings.HasPrefix(mid, "─") || !strings.HasSuffix(mid, "─") {
		return false
	}
	inner := strings.Trim(mid, "─")
	if inner == "" {
		return true
	}
	return !strings.Contains(inner, "─") && utf8.RuneCountInString(inner) <= 80
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

// composerText is visibleText for Grok's composer. It also drops the empty-box
// suggestion: italic (SGR 3) and faint grey (SGR 90, or 38;5;232–249). Typed
// text is a bright colour (254) and stays. The box border is faint too, so
// call this only on an interior line, after the box shape is already known.
func composerText(line string) string { return paneLineStyled(line, true, true) }

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
	return paneLineStyled(line, dropDim, false)
}

// paneLineStyled strips CSI sequences. dropGhost also drops Grok's empty-box
// suggestion: italic and faint grey. Borders use paneLineText, which keeps
// faint grey so a grey rule still parses.
func paneLineStyled(line string, dropDim, dropGhost bool) string {
	var b strings.Builder
	var st sgrStyle
	for i := 0; i < len(line); {
		if line[i] == 0x1b && i+1 < len(line) && line[i+1] == '[' {
			j := i + 2
			for j < len(line) && (line[j] < 0x40 || line[j] > 0x7e) {
				j++
			}
			if j < len(line) && line[j] == 'm' {
				st.apply(line[i+2 : j])
			}
			i = j + 1
			continue
		}
		r, size := rune(line[i]), 1
		if line[i] >= 0x80 {
			r, size = utf8.DecodeRuneInString(line[i:])
		}
		ghost := (dropDim && st.dim) || (dropGhost && (st.italic || st.faint))
		if !ghost {
			b.WriteRune(r)
		}
		i += size
	}
	return b.String()
}

// sgrStyle is the part of an SGR state this parser needs.
type sgrStyle struct {
	dim, italic, faint bool
}

func (s *sgrStyle) apply(params string) {
	if params == "" {
		*s = sgrStyle{}
		return
	}
	fields := strings.Split(params, ";")
	for i := 0; i < len(fields); i++ {
		n, err := strconv.Atoi(fields[i])
		if err != nil {
			continue
		}
		switch {
		case n == 38 || n == 48 || n == 58:
			if i+1 < len(fields) && fields[i+1] == "5" && i+2 < len(fields) {
				c, err := strconv.Atoi(fields[i+2])
				if err == nil && n == 38 {
					s.faint = c >= 232 && c <= 249
				}
				i += 2
			} else if i+1 < len(fields) && fields[i+1] == "2" {
				i += 4
			}
		case n == 0:
			*s = sgrStyle{}
		case n == 22:
			s.dim = false
		case n == 23:
			s.italic = false
		case n == 39:
			s.faint = false
		case n == 2:
			s.dim = true
		case n == 3:
			s.italic = true
		case n == 90:
			s.faint = true
		}
	}
}
