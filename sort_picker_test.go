package main

import (
	"strings"
	"testing"
)

// TestSortPickerStateHandle is the picker's key table: navigation wraps,
// Enter confirms, Esc/q/Ctrl-C cancel, and anything else is ignored rather
// than treated as a dismissal. Mirrors TestAccountPickerStateHandle's key
// contract.
func TestSortPickerStateHandle(t *testing.T) {
	tests := []struct {
		name        string
		state       sortPickerState
		key         string
		wantSel     int
		wantConfirm bool
		wantCancel  bool
	}{
		{name: "down moves", state: sortPickerState{sel: 0, rows: 6}, key: KeyDown, wantSel: 1},
		{name: "down wraps", state: sortPickerState{sel: 5, rows: 6}, key: KeyDown, wantSel: 0},
		{name: "up wraps", state: sortPickerState{sel: 0, rows: 6}, key: KeyUp, wantSel: 5},
		{name: "enter confirms", state: sortPickerState{sel: 2, rows: 6}, key: KeyEnter, wantSel: 2, wantConfirm: true},
		{name: "esc cancels", state: sortPickerState{rows: 6}, key: KeyEsc, wantCancel: true},
		{name: "q cancels", state: sortPickerState{rows: 6}, key: "q", wantCancel: true},
		{name: "ctrl-c cancels", state: sortPickerState{rows: 6}, key: "\x03", wantCancel: true},
		{name: "unmapped key is ignored", state: sortPickerState{sel: 1, rows: 6}, key: "x", wantSel: 1},
		{name: "enter on an empty list does nothing", state: sortPickerState{rows: 0}, key: KeyEnter},
		{name: "nav on an empty list does nothing", state: sortPickerState{rows: 0}, key: KeyDown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := tt.state
			confirm, cancel := state.handle(tt.key)
			if confirm != tt.wantConfirm || cancel != tt.wantCancel {
				t.Fatalf("handle(%q) = (%v, %v), want (%v, %v)", tt.key, confirm, cancel, tt.wantConfirm, tt.wantCancel)
			}
			if state.sel != tt.wantSel {
				t.Fatalf("sel = %d, want %d", state.sel, tt.wantSel)
			}
		})
	}
}

// TestRenderSortPicker proves every mode's label appears and the marker sits
// on the row matching `current`, not necessarily the row matching `sel` —
// the cursor can move away from the live mode before Enter is pressed.
func TestRenderSortPicker(t *testing.T) {
	out := renderSortPicker("created", 0, 80, 24)
	if !strings.Contains(out, "sort by") {
		t.Fatalf("title missing:\n%s", out)
	}
	for _, mode := range sortModeOrder {
		if !strings.Contains(out, sortDesc(mode)) {
			t.Fatalf("output missing label for %q:\n%s", mode, out)
		}
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, sortDesc("created")) && !strings.Contains(line, sortPickerActiveGlyph) {
			t.Fatalf("current mode row is not marked: %q", line)
		}
		if strings.Contains(line, sortDesc("status")) && strings.Contains(line, sortPickerActiveGlyph) {
			t.Fatalf("non-current mode row is marked: %q", line)
		}
	}
}

// TestRenderSortPickerHighlightsSelection proves sel, not current, drives
// which row highlightSelectedRow marks — a renderSortPicker that ignored sel
// entirely would still pass TestRenderSortPicker above.
func TestRenderSortPickerHighlightsSelection(t *testing.T) {
	highlightedIndex := func(out string) int {
		for i, mode := range sortModeOrder {
			for _, line := range strings.Split(out, "\n") {
				if strings.Contains(line, sortDesc(mode)) && strings.Contains(line, ansiSelectedBG) {
					return i
				}
			}
		}
		return -1
	}

	out0 := renderSortPicker("created", 0, 80, 24)
	if got := highlightedIndex(out0); got != 0 {
		t.Fatalf("sel=0: highlighted row index = %d, want 0\n%s", got, out0)
	}

	out2 := renderSortPicker("created", 2, 80, 24)
	if got := highlightedIndex(out2); got != 2 {
		t.Fatalf("sel=2: highlighted row index = %d, want 2\n%s", got, out2)
	}
}

// TestRenderSortPickerUnknownSize mirrors renderConfirmOverlay's fallback: an
// unknown terminal size emits the box unpositioned rather than panicking on
// negative padding.
func TestRenderSortPickerUnknownSize(t *testing.T) {
	out := renderSortPicker("dir", 0, 0, 0)
	if !strings.HasPrefix(out, confirmBoxTL) {
		t.Fatalf("want an unpositioned box, got:\n%s", out)
	}
}
