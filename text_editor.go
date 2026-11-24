package main

import (
	"strings"
	"unicode/utf8"
)

// The multi-line text box shared by the inspector's compose box (key i in a
// preview) and the Fleet view's chat with FLEET (key i there): a rune buffer
// with a cursor, edited one decoded key at a time (textEditor.key). It draws
// itself wrapped to a width, with the cursor as a reverse-video cell; there
// is no real terminal cursor in this TUI.

// textEditorMaxRows is the most rows a box grows to before it scrolls.
const textEditorMaxRows = 8

// textEditor holds the text and the cursor. width is the wrap width of the
// last draw (Up and Down move across those wrapped rows); top is the first
// row shown, kept by view so the cursor stays visible.
type textEditor struct {
	buf      []rune
	cur      int  // cursor index in buf, 0..len(buf)
	bytes    int  // UTF-8 length of buf
	maxBytes int  // 0: no cap
	goal     int  // column Up/Down aim for, while hasGoal
	hasGoal  bool // the last move was Up or Down
	width    int
	top      int
}

// newTextEditor returns an empty box that holds at most maxBytes bytes.
func newTextEditor(maxBytes int) textEditor {
	return textEditor{maxBytes: maxBytes}
}

// Text is the box's text; line breaks are "\n".
func (e *textEditor) Text() string { return string(e.buf) }

// Empty reports whether the box holds nothing but whitespace.
func (e *textEditor) Empty() bool { return strings.TrimSpace(string(e.buf)) == "" }

// SetText replaces the text and puts the cursor at its end. Text that does
// not fit the cap or holds a rune the box would refuse is cut there.
func (e *textEditor) SetText(s string) {
	e.buf, e.cur, e.bytes, e.top, e.hasGoal = nil, 0, 0, 0, false
	for _, r := range s {
		if r == '\n' {
			if !e.Newline() {
				return
			}
			continue
		}
		if !e.insertRune(r) {
			return
		}
	}
}

// Clear empties the box.
func (e *textEditor) Clear() { e.SetText("") }

// fits reports whether n more bytes may go in.
func (e *textEditor) fits(n int) bool { return e.maxBytes <= 0 || e.bytes+n <= e.maxBytes }

// textEditorPrintable is a rune the box takes as typed text: no C0 or C1
// control, no DEL, no invalid rune. A line break goes in through Newline.
func textEditorPrintable(r rune) bool {
	return r >= 0x20 && r != 0x7f && !(r >= 0x80 && r < 0xa0) && r != utf8.RuneError
}

func (e *textEditor) insertRune(r rune) bool {
	if !textEditorPrintable(r) && r != '\n' {
		return false
	}
	n := utf8.RuneLen(r)
	if !e.fits(n) {
		return false
	}
	e.buf = append(e.buf, 0)
	copy(e.buf[e.cur+1:], e.buf[e.cur:])
	e.buf[e.cur] = r
	e.cur++
	e.bytes += n
	e.hasGoal = false
	return true
}

// Insert types text at the cursor, rune by rune, until a rune does not fit
// or is not printable. It reports whether all of it went in.
func (e *textEditor) Insert(s string) bool {
	for _, r := range s {
		if r == '\n' || !e.insertRune(r) {
			return false
		}
	}
	return true
}

// Newline puts a line break at the cursor.
func (e *textEditor) Newline() bool { return e.insertRune('\n') }

// Backspace deletes the rune before the cursor.
func (e *textEditor) Backspace() bool {
	if e.cur == 0 {
		return false
	}
	e.cur--
	e.deleteAt(e.cur)
	return true
}

// Delete deletes the rune under the cursor.
func (e *textEditor) Delete() bool {
	if e.cur >= len(e.buf) {
		return false
	}
	e.deleteAt(e.cur)
	return true
}

func (e *textEditor) deleteAt(i int) {
	e.bytes -= utf8.RuneLen(e.buf[i])
	e.buf = append(e.buf[:i], e.buf[i+1:]...)
	e.hasGoal = false
}

func (e *textEditor) Left() {
	if e.cur > 0 {
		e.cur--
	}
	e.hasGoal = false
}

func (e *textEditor) Right() {
	if e.cur < len(e.buf) {
		e.cur++
	}
	e.hasGoal = false
}

// Home moves to the start of the cursor's line (up to the line break
// before it, not to the start of a wrapped row).
func (e *textEditor) Home() {
	for e.cur > 0 && e.buf[e.cur-1] != '\n' {
		e.cur--
	}
	e.hasGoal = false
}

// End moves to the end of the cursor's line.
func (e *textEditor) End() {
	for e.cur < len(e.buf) && e.buf[e.cur] != '\n' {
		e.cur++
	}
	e.hasGoal = false
}

// Up moves one wrapped row up, to the column it was in (or the row's end
// when that row is shorter). On the first row it goes to the start.
func (e *textEditor) Up() { e.vertical(-1) }

// Down moves one wrapped row down; on the last row it goes to the end.
func (e *textEditor) Down() { e.vertical(1) }

func (e *textEditor) vertical(d int) {
	rows := e.layout(e.wrapWidth())
	row, col := e.cursorAt(rows)
	if !e.hasGoal {
		e.goal = col
	}
	goal := e.goal
	t := row + d
	switch {
	case t < 0:
		e.cur = 0
	case t >= len(rows):
		e.cur = len(e.buf)
	default:
		e.cur = e.indexAtCol(rows, t, goal)
	}
	e.goal, e.hasGoal = goal, true
}

// ReplaceBackslash turns a "\" right before the cursor into a line break,
// the backslash-Enter way to start a new line. false when there is none.
func (e *textEditor) ReplaceBackslash() bool {
	if e.cur == 0 || e.buf[e.cur-1] != '\\' {
		return false
	}
	e.buf[e.cur-1] = '\n'
	e.hasGoal = false
	return true // same byte length: "\" and "\n" are one byte each
}

// textEditorKey is what one key did in the box.
type textEditorKey int

const (
	editorIgnored textEditorKey = iota // not a box key; the caller may use it
	editorEdited                       // the box changed or the cursor moved
	editorSubmit                       // Enter on a non-empty box
)

// key applies one decoded key. Enter submits a non-empty box, except in a
// paste (inBatch), where it is a line break and never sends, and right after
// a "\" (backslash-Enter), which becomes a line break. Alt-Enter and
// Shift-Enter are line breaks. A Tab in a paste is a space. Every key the
// box does not use is editorIgnored; Enter on an empty box is editorEdited
// (it does nothing, but it is the box's key).
func (e *textEditor) key(k string, inBatch bool) textEditorKey {
	switch k {
	case KeyEnter:
		switch {
		case inBatch:
			e.Newline()
		case e.ReplaceBackslash():
		case !e.Empty():
			return editorSubmit
		}
		return editorEdited
	case KeyAltEnter, KeyShiftEnter:
		e.Newline()
	case "\x7f", "\x08":
		e.Backspace()
	case KeyDelete:
		e.Delete()
	case KeyLeft:
		e.Left()
	case KeyRight:
		e.Right()
	case KeyUp:
		e.Up()
	case KeyDown:
		e.Down()
	case KeyHome, "\x01": // ctrl-a
		e.Home()
	case KeyEnd, "\x05": // ctrl-e
		e.End()
	case "\t":
		if !inBatch {
			return editorIgnored
		}
		e.Insert(" ")
	default:
		r, n := utf8.DecodeRuneInString(k)
		if n == 0 || n != len(k) || !textEditorPrintable(r) {
			return editorIgnored
		}
		e.insertRune(r)
	}
	return editorEdited
}

// editorRow is one wrapped row: buf[start:end], and hard when a line break
// (at buf[end]) ends it rather than the width.
type editorRow struct {
	start, end int
	hard       bool
}

// wrapWidth is the width rows wrap at: one column short of the last drawn
// width, so a cursor at the end of a full row still has its cell.
func (e *textEditor) wrapWidth() int {
	w := e.width
	if w <= 0 {
		w = 80
	}
	return max(w-1, 1)
}

// layout wraps buf to width columns (runeCells), breaking at every line
// break. There is always at least one row.
func (e *textEditor) layout(width int) []editorRow {
	var rows []editorRow
	start, w := 0, 0
	for i, r := range e.buf {
		if r == '\n' {
			rows = append(rows, editorRow{start, i, true})
			start, w = i+1, 0
			continue
		}
		c := runeCells(r)
		if w+c > width && i > start {
			rows = append(rows, editorRow{start, i, false})
			start, w = i, 0
		}
		w += c
	}
	return append(rows, editorRow{start, len(e.buf), false})
}

// cursorAt is the cursor's row and column (in cells) in rows. A cursor at
// the end of a wrapped row belongs to the start of the next one.
func (e *textEditor) cursorAt(rows []editorRow) (row, col int) {
	for i, r := range rows {
		last := i == len(rows)-1
		if e.cur >= r.start && (e.cur < r.end || (e.cur == r.end && (r.hard || last))) {
			return i, cellWidth(string(e.buf[r.start:e.cur]))
		}
	}
	return len(rows) - 1, 0
}

// indexAtCol is the index in row t closest to column col without passing
// it. On a wrapped row the cursor stops before its last rune, since the
// row's end index draws at the start of the next row.
func (e *textEditor) indexAtCol(rows []editorRow, t, col int) int {
	r := rows[t]
	end := r.end
	if !r.hard && t < len(rows)-1 && end > r.start {
		end--
	}
	i, w := r.start, 0
	for i < end {
		c := runeCells(e.buf[i])
		if w+c > col {
			break
		}
		w += c
		i++
	}
	return i
}

// view draws the box at width columns, at most maxRows rows tall (at least
// one), scrolled so the cursor's row shows. Each row is plain text (the box
// only holds printable runes) with the cursor cell in reverse video, so
// visualLen counts it as one cell. The width is kept for Up and Down.
func (e *textEditor) view(width, maxRows int) []string {
	if width < 2 {
		width = 2
	}
	e.width = width
	rows := e.layout(e.wrapWidth())
	crow, _ := e.cursorAt(rows)
	h := min(max(maxRows, 1), len(rows))
	if crow < e.top {
		e.top = crow
	}
	if crow >= e.top+h {
		e.top = crow - h + 1
	}
	e.top = min(max(e.top, 0), len(rows)-h)
	out := make([]string, 0, h)
	for i := e.top; i < e.top+h; i++ {
		r := rows[i]
		text := string(e.buf[r.start:r.end])
		if i == crow {
			at := e.cur - r.start
			under := " "
			rest := ""
			if e.cur < r.end {
				under = string(e.buf[e.cur])
				rest = string(e.buf[e.cur+1 : r.end])
			}
			text = string(e.buf[r.start:r.start+at]) + "\x1b[7m" + under + "\x1b[27m" + rest
		}
		out = append(out, text)
	}
	return out
}

// rowsFor is how many rows view would draw at width, before the maxRows cap.
func (e *textEditor) rowsFor(width int) int {
	if width < 2 {
		width = 2
	}
	saved := e.width
	e.width = width
	n := len(e.layout(e.wrapWidth()))
	e.width = saved
	return n
}
