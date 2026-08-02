package main

import "testing"

// TestSortPickerStateHandle is the picker's key table: navigation wraps,
// Enter confirms, Esc/q/Ctrl-C cancel, and anything else is ignored rather
// than treated as a dismissal. Mirrors TestAccountPickerStateHandle, minus
// the empty-list cases account_picker_test.go needs — sortModeOrder is never
// empty — while still keeping the rows==0 guard itself (defensive, in case a
// caller ever zero-constructs the state).
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
