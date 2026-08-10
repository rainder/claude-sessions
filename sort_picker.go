package main

import (
	"os"
	"strings"

	"golang.org/x/term"
)

// The 's'/'S' sort-mode dialog: a small bordered overlay listing every entry
// in sortModeOrder, current mode marked, Enter applies immediately.
//
// Style follows the existing small overlays (account_picker.go /
// confirm_overlay.go) rather than the full searchable resume picker: there
// are exactly 6 fixed modes, so there is nothing to search.

// sortPickerState is the picker's pure key-handling core: which row is
// selected, and what a keystroke does to it. Structurally identical to
// accountPickerState (account_picker.go) — same four-key contract.
type sortPickerState struct {
	sel       int
	rows      int
	groupSort bool
}

// handle applies one key event. confirm means "apply the selected row";
// cancel means "close, change nothing". Any other key (including nav on an
// empty list) just redraws, so an unmapped keystroke never dismisses the
// overlay. rows is always len(sortModeOrder) (6) in production, never 0 —
// the rows==0 guard is kept anyway since sortModeOrder is a mutable package
// var, not a const, so a future zero-value construction should redraw
// instead of panicking on an out-of-range index.
func (s *sortPickerState) handle(key string) (confirm, cancel bool) {
	switch key {
	case KeyEsc, "q", "Q", "\x03", "\x04":
		return false, true
	case KeyUp:
		if s.rows > 0 {
			s.sel = (s.sel - 1 + s.rows) % s.rows
		}
		return false, false
	case KeyDown:
		if s.rows > 0 {
			s.sel = (s.sel + 1) % s.rows
		}
		return false, false
	case "g", "G":
		s.groupSort = !s.groupSort
		return false, false
	case KeyEnter, "\r", "\n":
		if s.rows == 0 {
			return false, false
		}
		return true, false
	default:
		return false, false
	}
}

// sortPickerHint is the fixed hint row drawn below the mode rows.
const sortPickerHint = "↑/↓ select    g group first    ⏎ confirm    esc cancel"

// sortPickerActiveGlyph marks the row matching the currently live sort mode.
const sortPickerActiveGlyph = "●"

// sortPickerTitle is the dialog's fixed title.
const sortPickerTitle = "sort by"

// renderSortPicker draws the bordered box centered in a cols x rows
// terminal: the title, one line per sortModeOrder entry, a blank separator,
// then the dimmed hint. Positioning, clipping and the unknown-size fallback
// match renderAccountPicker / renderConfirmOverlay, so every small overlay in
// this app looks like the same kind of dialog.
func renderSortPicker(current string, sel int, groupSort bool, cols, rows int) string {
	body := make([]string, 0, len(sortModeOrder))
	for _, mode := range sortModeOrder {
		marker := " "
		if mode == current {
			marker = sortPickerActiveGlyph
		}
		body = append(body, marker+" "+sortDesc(mode))
	}

	toggleLine := "group first: off"
	if groupSort {
		toggleLine = "group first: on"
	}

	innerWidth := visualLen(sortPickerTitle)
	for _, l := range append(append([]string{toggleLine}, body...), sortPickerHint) {
		if w := visualLen(l); w > innerWidth {
			innerWidth = w
		}
	}
	if cols > 0 {
		max := cols - 4 // border + one space of padding each side
		if max < 1 {
			max = 1
		}
		if innerWidth > max {
			innerWidth = max
		}
	}

	pad := func(s string, selected bool) string {
		s = clipLine(s, innerWidth)
		line := s + strings.Repeat(" ", innerWidth-visualLen(s))
		if selected {
			line = highlightSelectedRow(line, true)
		}
		return confirmBoxV + " " + line + " " + confirmBoxV
	}

	box := make([]string, 0, len(body)+6)
	box = append(box, confirmBoxTL+strings.Repeat(confirmBoxH, innerWidth+2)+confirmBoxTR)
	box = append(box, pad(bold(sortPickerTitle), false))
	box = append(box, pad(dim(toggleLine), false))
	box = append(box, pad("", false))
	for i, l := range body {
		box = append(box, pad(l, i == sel))
	}
	box = append(box, pad("", false))
	box = append(box, pad(dim(sortPickerHint), false))
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

// pickSortMode drives the blocking picker, mirroring pickAccount's read/handle
// loop (account_picker.go). Must be called in raw mode; it never leaves raw
// or the alt-screen, so the caller's next render() paints over it. wakes
// carries the modal wake sources (resize) so the box stays centered across a
// live resize.
//
// The selection starts on the mode matching `current`, seeded with
// currentGroupSort as the toggle's starting state. Enter without changing
// anything reports the same mode and toggle state back — the caller is
// responsible for treating "confirmed but unchanged" as a no-op (see tui.go's
// wiring).
func pickSortMode(current string, currentGroupSort bool, wakes []wakeFD) (picked string, groupSort bool, ok bool) {
	state := sortPickerState{rows: len(sortModeOrder), groupSort: currentGroupSort}
	for i, mode := range sortModeOrder {
		if mode == current {
			state.sel = i
			break
		}
	}
	renderer := newScreenRenderer(os.Stdout)
	decoder := newInputDecoder()
	fd := int(os.Stdin.Fd())

	for {
		cols, rows, err := term.GetSize(fd)
		if err != nil {
			cols, rows = 0, 0
		}
		_ = renderer.Draw(renderSortPicker(current, state.sel, state.groupSort, cols, rows), cols, rows)
		keys, _ := readModalEvents(decoder, wakes)
		for _, key := range keys {
			confirm, cancel := state.handle(key)
			if cancel {
				return "", false, false
			}
			if confirm {
				return sortModeOrder[state.sel], state.groupSort, true
			}
		}
	}
}
