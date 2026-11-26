package main

import (
	"strings"
	"time"
)

// The Fleet view's key help: ? on the card list or the ship screen opens a
// scrollable panel listing every key of the view, its panels and chats.
// It replaces the whole view like a chat does; every key it does not use is
// swallowed, so a digit or r typed while reading it answers nothing. The
// text is fixed, so it needs no sanitizing.

// fleetHelpKeys is the panel's key line.
const fleetHelpKeys = "↑↓ PgUp/PgDn Home/End scroll · Esc / q / ? close"

// fleetHelpText is the panel's body: a heading per screen, then one key per
// line. A heading is a line with no leading space.
var fleetHelpText = []string{
	"CARD LIST",
	"  ↑ / ↓          select a card",
	"  PgUp / PgDn    scroll the card's details",
	"  1-9            pick an option (a confirm panel opens)",
	"  r              send FLEET's pick on an unanswered card, no panel",
	"  R              send every FLEET pick (a confirm panel lists them)",
	"  o              Other: send your own answer",
	"  c              correct an answered card (then a digit)",
	"  x              dismiss an answer FLEET has not confirmed",
	"  k              Clarify: ask a fork of FLEET about the card",
	"  a              attach to the card's session",
	"  i / →          chat with the card's session",
	"  f              chat with FLEET",
	"  t              the card's raw text (drag to select)",
	"  ?              this help",
	"  Esc / q        back, or close the Fleet view",
	"",
	"SHIP SCREEN (no card needs you)",
	"  Enter          show the sent cards",
	"  f              chat with FLEET",
	"  ?              this help",
	"  Esc / q        close the Fleet view",
	"",
	"OPTION AND OTHER PANELS",
	"  type           an optional comment (the Other answer)",
	"  Enter          send",
	"  Esc            cancel",
	"",
	"R PANEL (all FLEET picks)",
	"  Enter          send them all",
	"  PgUp / PgDn    scroll the list",
	"  Esc / q        cancel",
	"",
	"CHAT (with FLEET, or a card's session)",
	"  Enter          send",
	"  Alt-Enter / \\⏎ new line",
	"  PgUp / PgDn    scroll the log",
	"  Esc            close, keep the draft",
	"  Ctrl-C         close, clear the draft",
	"",
	"CLARIFY (k)",
	"  Enter          ask",
	"  Ctrl-D         draft an answer for FLEET",
	"  s / x          use the draft as the answer / discard it (empty input)",
	"  PgUp / PgDn    scroll the chat",
	"  Esc            back, keep the typed question",
	"  Ctrl-C         back, clear the typed question",
	"",
	"RAW TEXT (t)",
	"  ↑↓ PgUp/PgDn   scroll",
	"  any other key  back",
}

// fleetHelpMode is the open help panel. scroll is the first body line shown;
// fleetHelpLines clamps it. page is the body height it last drew (0 before
// the first frame), the PgUp/PgDn step.
type fleetHelpMode struct {
	scroll int
	page   int
}

// pageStep is one PgUp/PgDn of the help body: its drawn height less one line
// of overlap, or fleetPageStep before the first frame.
func (h *fleetHelpMode) pageStep() int {
	if h.page <= 0 {
		return fleetPageStep
	}
	return max(h.page-1, 1)
}

// openHelp is ? on the card list or the ship screen. handleKey routes every
// key to an open chat, Clarify, panel or R panel first, so ? there is text
// or nothing. A paste never opens it.
func (st *fleetViewState) openHelp(inBatch bool) fleetCmd {
	if inBatch {
		return fleetCmdNone
	}
	st.help = &fleetHelpMode{}
	return fleetCmdRender
}

// handleHelpKey is handleKey while the help is open: Esc, q, ? and Ctrl-C
// close it, the arrows and page keys scroll it, and every other key does
// nothing.
func (st *fleetViewState) handleHelpKey(k string) fleetCmd {
	h := st.help
	switch k {
	case KeyEsc, "q", "Q", "?", "\x03":
		st.help = nil
		return fleetCmdRender
	case KeyUp:
		h.scroll = max(h.scroll-1, 0)
	case KeyDown:
		h.scroll++
	case KeyPageUp:
		h.scroll = max(h.scroll-h.pageStep(), 0)
	case KeyPageDown, " ":
		h.scroll += h.pageStep()
	case KeyHome:
		h.scroll = 0
	case KeyEnd:
		h.scroll = len(fleetHelpText)
	default:
		return fleetCmdNone
	}
	return fleetCmdRender
}

// fleetHelpLines draws the help as the whole view: a head line, the body
// window from h.scroll (clamped here so a key past the end moves at once on
// the way back), and the key line, or a toast in its place while one shows
// (a send, R or dismiss result that lands while the help is open), as the
// chat does. With rows > 0 the frame is exactly rows lines.
func fleetHelpLines(st *fleetViewState, cols, rows int, now time.Time) []string {
	h := st.help
	sep := dim(strings.Repeat("─", cols))
	head := bold("FLEET VIEW · KEYS")
	body := make([]string, len(fleetHelpText))
	for i, l := range fleetHelpText {
		if l != "" && !strings.HasPrefix(l, " ") {
			l = bold(l)
		}
		body[i] = l
	}
	n := len(body)
	if rows > 0 {
		n = max(rows-4, 0)
	}
	h.page = n
	h.scroll = max(min(h.scroll, len(body)-n), 0)
	end := min(h.scroll+n, len(body))
	keys := fleetHelpKeys
	if h.scroll > 0 || end < len(body) {
		keys = "more · " + keys
	}
	out := append([]string{head, sep}, body[h.scroll:end]...)
	for rows > 0 && len(out) < rows-2 {
		out = append(out, "")
	}
	last := fleetToastLine(st, now)
	if last == "" {
		last = dim(truncateCells(keys, cols))
	}
	out = append(out, sep, last)
	if rows > 0 && len(out) > rows {
		// A screen shorter than the head and key lines: keep the key line.
		out = append(out[:max(rows-1, 0)], out[len(out)-1])
	}
	return out
}
