package main

import (
	"strings"
	"testing"
)

// editorWith is a box capped at sendKeysMaxLen holding s, cursor at the end.
func editorWith(s string) textEditor {
	e := newTextEditor(sendKeysMaxLen)
	e.SetText(s)
	return e
}

// typeKeys feeds keys one at a time, none in a paste.
func typeKeys(e *textEditor, keys ...string) {
	for _, k := range keys {
		e.key(k, false)
	}
}

// cursorView is the text with "|" at the cursor.
func cursorView(e *textEditor) string {
	return string(e.buf[:e.cur]) + "|" + string(e.buf[e.cur:])
}

func TestTextEditorInsertAndCursorMoves(t *testing.T) {
	e := newTextEditor(0)
	typeKeys(&e, "a", "b", "c", KeyLeft, KeyLeft, "X")
	if got := cursorView(&e); got != "aX|bc" {
		t.Fatalf("after insert mid-text = %q", got)
	}
	typeKeys(&e, KeyRight, KeyRight, KeyRight, "\x7f")
	if got := cursorView(&e); got != "aXb|" {
		t.Fatalf("after Right past end and Backspace = %q", got)
	}
	typeKeys(&e, KeyHome, KeyDelete)
	if got := cursorView(&e); got != "|Xb" {
		t.Fatalf("after Home and Delete = %q", got)
	}
	typeKeys(&e, "\x05", KeyLeft, "\x01")
	if got := cursorView(&e); got != "|Xb" {
		t.Fatalf("ctrl-e, Left, ctrl-a = %q", got)
	}
	typeKeys(&e, KeyLeft, "\x7f", KeyEnd, KeyDelete)
	if got := cursorView(&e); got != "Xb|" {
		t.Fatalf("Left/Backspace at start and Delete at end must do nothing: %q", got)
	}
}

func TestTextEditorNonASCIIAndWideRunes(t *testing.T) {
	e := newTextEditor(0)
	typeKeys(&e, "ą", "ž", "漢", "字", KeyLeft, "\x7f")
	if got := cursorView(&e); got != "ąž|字" {
		t.Fatalf("got %q", got)
	}
	if e.bytes != len("ąž字") {
		t.Fatalf("bytes = %d, want %d", e.bytes, len("ąž字"))
	}
	// Two wide runes fill 4 cells: at width 4 (wrap at 3) the second goes
	// to the next row.
	e.SetText("漢字")
	rows := e.layout(3)
	if len(rows) != 2 || rows[0].end != 1 {
		t.Fatalf("rows = %+v, want 漢 | 字", rows)
	}
}

func TestTextEditorRefusesControlRunes(t *testing.T) {
	e := newTextEditor(0)
	for _, k := range []string{"\x1b", "\x00", "\x07", "\u0085", "\u009b", "\x7f", "\t", KeyEsc, KeyPageUp} {
		e.key(k, false)
	}
	if e.Text() != "" {
		t.Fatalf("text = %q, want empty", e.Text())
	}
	if e.Insert("a\x1bb") || e.Text() != "a" {
		t.Fatalf("Insert stops at a control rune: %q", e.Text())
	}
	if got := e.key(KeyPageUp, false); got != editorIgnored {
		t.Fatalf("PgUp = %v, want ignored", got)
	}
}

func TestTextEditorEnterKeys(t *testing.T) {
	e := newTextEditor(0)
	if got := e.key(KeyEnter, false); got != editorEdited || e.Text() != "" {
		t.Fatalf("Enter on empty = %v %q", got, e.Text())
	}
	typeKeys(&e, "a", KeyAltEnter, "b", KeyShiftEnter, "c", "\\")
	if got := e.key(KeyEnter, false); got != editorEdited {
		t.Fatalf("backslash-Enter = %v, want a line break", got)
	}
	typeKeys(&e, "d")
	if e.Text() != "a\nb\nc\nd" {
		t.Fatalf("text = %q", e.Text())
	}
	// In a paste Enter is a line break, even after a backslash.
	e.key("\\", true)
	if got := e.key(KeyEnter, true); got != editorEdited {
		t.Fatalf("Enter in a paste = %v", got)
	}
	if got := e.key("\t", true); got != editorEdited {
		t.Fatalf("Tab in a paste = %v", got)
	}
	if e.Text() != "a\nb\nc\nd\\\n " {
		t.Fatalf("text after paste = %q", e.Text())
	}
	if got := e.key(KeyEnter, false); got != editorSubmit {
		t.Fatalf("Enter = %v, want submit", got)
	}
	e.SetText("  \n ")
	if got := e.key(KeyEnter, false); got != editorEdited {
		t.Fatalf("Enter on whitespace = %v, want no submit", got)
	}
}

func TestTextEditorUpDownAcrossHardAndWrappedLines(t *testing.T) {
	e := newTextEditor(0)
	e.width = 6 // wraps at 5 cells
	e.SetText("abcdefgh\nxy\nlonger")
	// Rows: "abcde" "fgh" | "xy" | "longe" "r".
	if rows := e.layout(e.wrapWidth()); len(rows) != 5 {
		t.Fatalf("rows = %+v", rows)
	}
	e.cur = 3 // abc|de
	e.Down()
	if got := cursorView(&e); got != "abcdefgh|\nxy\nlonger" {
		t.Fatalf("Down into the shorter wrapped row = %q", got)
	}
	e.Down()
	if got := cursorView(&e); got != "abcdefgh\nxy|\nlonger" {
		t.Fatalf("Down into the hard line = %q", got)
	}
	e.Down()
	if got := cursorView(&e); got != "abcdefgh\nxy\nlon|ger" {
		t.Fatalf("Down keeps the goal column = %q", got)
	}
	e.Down()
	if got := cursorView(&e); got != "abcdefgh\nxy\nlonger|" {
		t.Fatalf("Down into the last short row = %q", got)
	}
	e.Down()
	if got := cursorView(&e); got != "abcdefgh\nxy\nlonger|" {
		t.Fatalf("Down on the last row stays at the end = %q", got)
	}
	e.cur = 2 // ab|cde
	e.Up()
	if e.cur != 0 {
		t.Fatalf("Up on the first row goes to the start, cur = %d", e.cur)
	}
	// A wrapped row's end draws at the next row's start, so Down to a
	// column past a full wrapped row stops before its last rune.
	e.SetText("ab\nabcdefgh")
	e.cur = 2
	e.Down()
	if got := cursorView(&e); got != "ab\nab|cdefgh" {
		t.Fatalf("got %q", got)
	}
}

func TestTextEditorHomeEndAreLogicalLines(t *testing.T) {
	e := newTextEditor(0)
	e.width = 4
	e.SetText("one\ntwo three")
	e.cur = len([]rune("one\ntwo th"))
	e.Home()
	if got := cursorView(&e); got != "one\n|two three" {
		t.Fatalf("Home = %q", got)
	}
	e.End()
	if got := cursorView(&e); got != "one\ntwo three|" {
		t.Fatalf("End = %q", got)
	}
}

func TestTextEditorByteCap(t *testing.T) {
	e := newTextEditor(5)
	typeKeys(&e, "a", "b", "c", "ą", "d")
	if e.Text() != "abcą" {
		t.Fatalf("text = %q, want the rune that does not fit refused", e.Text())
	}
	if e.bytes != 5 {
		t.Fatalf("bytes = %d, want 5", e.bytes)
	}
	if e.Newline() {
		t.Fatal("a line break past the cap went in")
	}
	e.SetText(strings.Repeat("x", 10))
	if e.Text() != "xxxxx" {
		t.Fatalf("SetText cut = %q", e.Text())
	}
	e.cur = 4
	e.buf[3] = '\\'
	if !e.ReplaceBackslash() || e.bytes != 5 {
		t.Fatalf("backslash to line break keeps the byte count: %d", e.bytes)
	}
}

func TestTextEditorViewCursorAndScroll(t *testing.T) {
	e := newTextEditor(0)
	e.SetText("ab")
	got := e.view(10, 8)
	if len(got) != 1 || got[0] != "ab\x1b[7m \x1b[27m" {
		t.Fatalf("view = %q", got)
	}
	if visualLen(got[0]) != 3 {
		t.Fatalf("visualLen = %d, want 3 (cursor cell counts once)", visualLen(got[0]))
	}
	e.cur = 0
	if got := e.view(10, 8); got[0] != "\x1b[7ma\x1b[27mb" {
		t.Fatalf("cursor on a rune = %q", got[0])
	}
	e.SetText("1\n2\n3\n4\n5")
	got = e.view(10, 3)
	if len(got) != 3 || !strings.HasPrefix(got[2], "5") {
		t.Fatalf("view follows the cursor at the bottom: %q", got)
	}
	e.cur = 0
	got = e.view(10, 3)
	if !strings.Contains(got[0], "1") || e.top != 0 {
		t.Fatalf("view follows the cursor to the top: %q top=%d", got, e.top)
	}
	if e.rowsFor(10) != 5 {
		t.Fatalf("rowsFor = %d", e.rowsFor(10))
	}
	// A full row leaves the cursor a cell: width 4 wraps at 3.
	e.SetText("abc")
	if got := e.view(4, 8); len(got) != 1 || visualLen(got[0]) != 4 {
		t.Fatalf("full row = %q", got)
	}
}
