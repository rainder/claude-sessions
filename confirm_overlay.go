package main

import (
	"os"
	"strings"

	"golang.org/x/term"
)

// confirmHint is the fixed hint row drawn below the question inside the box.
const confirmHint = "[y] yes    [n] no"

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
// terminal: the question (one line per '\n' in question), a blank separator,
// then the dimmed "[y] yes   [n] no" hint. On a narrow terminal the box
// shrinks to fit and each line is clipped rather than wrapped. When cols or
// rows is unknown (<=0) the box is emitted unpositioned at the top-left,
// mirroring renderNewPicker's fallback for an unknown terminal size.
func renderConfirmOverlay(question string, cols, rows int) string {
	qLines := strings.Split(question, "\n")
	innerWidth := visualLen(confirmHint)
	for _, l := range qLines {
		if w := visualLen(l); w > innerWidth {
			innerWidth = w
		}
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

	box := make([]string, 0, len(qLines)+4)
	box = append(box, confirmBoxTL+strings.Repeat(confirmBoxH, innerWidth+2)+confirmBoxTR)
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

// confirmOverlay drives a blocking y/n dialog rendered as a centered overlay
// box, mirroring pickNewSession's read/handle loop shape. Must be called in
// raw mode; it never leaves raw or the alt-screen, so the caller's next
// render() paints over it. wakes lets the caller pass modal wake sources
// (e.g. resize) so the box stays correctly positioned across a live resize.
func confirmOverlay(question string, wakes []wakeFD) bool {
	state := confirmState{}
	renderer := newScreenRenderer(os.Stdout)
	decoder := newInputDecoder()
	fd := int(os.Stdin.Fd())

	for {
		cols, rows, err := term.GetSize(fd)
		if err != nil {
			cols, rows = 0, 0
		}
		_ = renderer.Draw(renderConfirmOverlay(question, cols, rows), cols, rows)
		keys, _ := readModalEvents(decoder, wakes)
		for _, key := range keys {
			confirmed, done := state.handle(key)
			if done {
				return confirmed
			}
		}
	}
}
