package main

import (
	"os"
	"strings"

	"golang.org/x/term"
)

// confirmHint is the fixed hint row drawn below the question inside the box.
const confirmHint = "[y] yes    [n] no"

// previewBoxMinInner is the inner width the box widens to when it carries a
// preview block, so pane output is legible rather than shredded by clipping.
const previewBoxMinInner = 72

// Box-drawing characters for the confirm overlay, matching the square-corner
// style preview.go already uses for its "┌─" / "│" transcript framing.
const (
	confirmBoxTL = "┌"
	confirmBoxTR = "┐"
	confirmBoxBL = "└"
	confirmBoxBR = "┘"
	confirmBoxH  = "─"
	confirmBoxV  = "│"
)

// confirmState is the pure key-handling core of the confirm overlay. It has
// no fields — every key maps deterministically to confirm/cancel/ignore — but
// stays a struct (mirroring newPickerState) so handle's signature never has
// to change if it grows state later.
type confirmState struct{}

// handle applies one key event, reporting whether the dialog is done and, if
// so, whether the user confirmed. y/Y/Enter confirm; n/N/q/Q/Esc/Ctrl-C
// cancel; everything else (arrows, stray printable keys, …) is ignored so the
// loop keeps waiting.
func (confirmState) handle(key string) (confirmed, done bool) {
	switch key {
	case "y", "Y", "\r", "\n", KeyEnter:
		return true, true
	case "n", "N", "q", "Q", KeyEsc, "\x03":
		return false, true
	default:
		return false, false
	}
}

// renderConfirmOverlay draws a bordered box centered in a cols x rows
// terminal: an optional preview block (title, divider, up to 12 content rows,
// divider), the question (one line per '\n' in question), a blank separator,
// then the dimmed "[y] yes   [n] no" hint. On a narrow terminal the box
// shrinks to fit and each line is clipped rather than wrapped. When cols or
// rows is unknown (<=0) the box is emitted unpositioned at the top-left,
// mirroring renderNewPicker's fallback for an unknown terminal size. prev may
// be nil, in which case no preview block is drawn and the box matches its
// pre-preview appearance byte-for-byte.
func renderConfirmOverlay(question string, prev *overlayPreview, cols, rows int) string {
	contentRows := 0
	if prev != nil && rows > 0 {
		contentRows = rows - 12 // box chrome + question + blank + hint
		if contentRows > 12 {
			contentRows = 12
		}
		if contentRows < 0 {
			contentRows = 0
		}
	}
	hasPreview := prev != nil && contentRows > 0

	qLines := strings.Split(question, "\n")
	innerWidth := visualLen(confirmHint)
	for _, l := range qLines {
		if w := visualLen(l); w > innerWidth {
			innerWidth = w
		}
	}
	// A preview block needs room to be legible; the floor applies only when one
	// actually renders, so the callers that pass nil keep today's narrow box.
	if hasPreview && innerWidth < previewBoxMinInner {
		innerWidth = previewBoxMinInner
	}
	if cols > 0 {
		max := cols - 4 // border + 1 space of padding on each side
		if max < 1 {
			max = 1
		}
		if innerWidth > max {
			innerWidth = max
		}
	}

	pad := func(s string) string {
		s = clipLine(s, innerWidth)
		return confirmBoxV + " " + s + strings.Repeat(" ", innerWidth-visualLen(s)) + " " + confirmBoxV
	}

	block := previewBlock(prev, innerWidth, contentRows)

	box := make([]string, 0, len(qLines)+len(block)+4)
	box = append(box, confirmBoxTL+strings.Repeat(confirmBoxH, innerWidth+2)+confirmBoxTR)
	for _, l := range block {
		box = append(box, pad(l))
	}
	for _, l := range qLines {
		box = append(box, pad(l))
	}
	box = append(box, pad(""))
	box = append(box, pad(dim(confirmHint)))
	box = append(box, confirmBoxBL+strings.Repeat(confirmBoxH, innerWidth+2)+confirmBoxBR)

	if cols <= 0 || rows <= 0 {
		return strings.Join(box, "\n")
	}

	boxWidth := innerWidth + 4
	left := (cols - boxWidth) / 2
	if left < 0 {
		left = 0
	}
	leftPad := strings.Repeat(" ", left)
	for i, l := range box {
		box[i] = leftPad + l
	}

	top := (rows - len(box)) / 2
	if top < 0 {
		top = 0
	}
	lines := make([]string, 0, top+len(box))
	for i := 0; i < top; i++ {
		lines = append(lines, "")
	}
	lines = append(lines, box...)
	return strings.Join(lines, "\n")
}

// modalWakesWith returns wakes plus the pane's wake source, copying rather
// than appending in place: modalWakes is built once in RunTUI (tui.go:333) and
// shared by every modal, so an in-place append would corrupt it for the next
// dialog.
func modalWakesWith(wakes []wakeFD, p *previewPane) []wakeFD {
	w := p.wake()
	if w.fd < 0 {
		return wakes
	}
	out := make([]wakeFD, len(wakes), len(wakes)+1)
	copy(out, wakes)
	return append(out, w)
}

// confirmOverlay drives a blocking y/n dialog rendered as a centered overlay
// box, mirroring pickNewSession's read/handle loop shape. Must be called in
// raw mode; it never leaves raw or the alt-screen, so the caller's next
// render() paints over it. wakes lets the caller pass modal wake sources
// (e.g. resize) so the box stays correctly positioned across a live resize.
func confirmOverlay(question string, wakes []wakeFD) bool {
	return confirmOverlayPreview(question, nil, wakes)
}

// confirmOverlayPreview is confirmOverlay with an optional preview block. The
// pane fetches in the background and wakes the loop when it lands, so a slow
// or unreachable remote host never delays the dialog appearing. A nil pane
// renders exactly what confirmOverlay renders.
//
// Invariant: wake() is called exactly once, before the loop, and this
// function never closes p. The single-threaded contract that makes wake()'s
// bare-int fd safe (see previewPane.wake) only holds if the same goroutine
// that runs this loop is the only one that can close the pane, and only
// after the loop returns — so close() is the caller's responsibility, not
// this function's.
func confirmOverlayPreview(question string, p *previewPane, wakes []wakeFD) bool {
	state := confirmState{}
	renderer := newScreenRenderer(os.Stdout)
	decoder := newInputDecoder()
	fd := int(os.Stdin.Fd())
	modalWakes := modalWakesWith(wakes, p)

	for {
		cols, rows, err := term.GetSize(fd)
		if err != nil {
			cols, rows = 0, 0
		}
		var prev *overlayPreview
		if p != nil {
			snap := p.snapshot()
			prev = &snap
		}
		_ = renderer.Draw(renderConfirmOverlay(question, prev, cols, rows), cols, rows)
		keys, _ := readModalEvents(decoder, modalWakes)
		for _, key := range keys {
			confirmed, done := state.handle(key)
			if done {
				return confirmed
			}
		}
	}
}
