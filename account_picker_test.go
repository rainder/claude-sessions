package main

import (
	"strings"
	"testing"
)

// TestAccountPickerStateHandle is the picker's key table: navigation wraps,
// Enter confirms, Esc/q/Ctrl-C cancel, and anything else is ignored rather than
// treated as a dismissal.
func TestAccountPickerStateHandle(t *testing.T) {
	tests := []struct {
		name        string
		state       accountPickerState
		key         string
		wantSel     int
		wantConfirm bool
		wantCancel  bool
	}{
		{name: "down moves", state: accountPickerState{sel: 0, rows: 3}, key: KeyDown, wantSel: 1},
		{name: "down wraps", state: accountPickerState{sel: 2, rows: 3}, key: KeyDown, wantSel: 0},
		{name: "up wraps", state: accountPickerState{sel: 0, rows: 3}, key: KeyUp, wantSel: 2},
		{name: "enter confirms", state: accountPickerState{sel: 1, rows: 3}, key: KeyEnter, wantSel: 1, wantConfirm: true},
		{name: "esc cancels", state: accountPickerState{rows: 3}, key: KeyEsc, wantCancel: true},
		{name: "q cancels", state: accountPickerState{rows: 3}, key: "q", wantCancel: true},
		{name: "ctrl-c cancels", state: accountPickerState{rows: 3}, key: "\x03", wantCancel: true},
		{name: "unmapped key is ignored", state: accountPickerState{sel: 1, rows: 3}, key: "x", wantSel: 1},
		// An empty host has nothing to apply: only Esc is live, and Enter must
		// not index into an empty slice.
		{name: "enter on an empty list does nothing", state: accountPickerState{rows: 0}, key: KeyEnter},
		{name: "nav on an empty list does nothing", state: accountPickerState{rows: 0}, key: KeyDown},
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

// TestRenderAccountPicker proves the overlay marks the active account (from
// activeSnapshotName, carried in the row) and shows every account's email.
func TestRenderAccountPicker(t *testing.T) {
	rows := []accountRow{
		{Name: "avisoma", Email: "andy@avisoma.com"},
		{Name: "trecs", Email: "andy@trecs.aero", Active: true},
	}
	out := renderAccountPicker("agent-workstation", rows, 1, 80, 24)
	if !strings.Contains(out, "agent-workstation") {
		t.Fatalf("title missing the host:\n%s", out)
	}
	for _, want := range []string{"avisoma", "andy@avisoma.com", "trecs", "andy@trecs.aero"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "avisoma") && strings.Contains(line, accountActiveGlyph) {
			t.Fatalf("inactive account is marked active: %q", line)
		}
		if strings.Contains(line, "trecs") && !strings.Contains(line, accountActiveGlyph) {
			t.Fatalf("active account is not marked: %q", line)
		}
	}
}

// TestRenderAccountPickerEmpty proves a host with no snapshots explains itself
// (and names the command that fixes it) instead of showing an empty box.
func TestRenderAccountPickerEmpty(t *testing.T) {
	out := renderAccountPicker("", nil, 0, 80, 24)
	if !strings.Contains(out, accountPickerEmpty) {
		t.Fatalf("empty state missing:\n%s", out)
	}
	if !strings.Contains(out, accountPickerEmptyHint) {
		t.Fatalf("empty-state hint missing:\n%s", out)
	}
	if strings.Contains(out, accountPickerHint) {
		t.Fatalf("empty state offers ⏎ switch with nothing to switch to:\n%s", out)
	}
}

// TestRenderAccountPickerUnknownSize mirrors renderConfirmOverlay's fallback: an
// unknown terminal size emits the box unpositioned rather than panicking on
// negative padding.
func TestRenderAccountPickerUnknownSize(t *testing.T) {
	out := renderAccountPicker("box", []accountRow{{Name: "avisoma"}}, 0, 0, 0)
	if !strings.HasPrefix(out, confirmBoxTL) {
		t.Fatalf("want an unpositioned box, got:\n%s", out)
	}
}

func TestAccountSwitchToast(t *testing.T) {
	got := accountSwitchToast("box", "trecs", "andy@trecs.aero")
	if !strings.Contains(got, "box") || !strings.Contains(got, "trecs") || !strings.Contains(got, "andy@trecs.aero") {
		t.Fatalf("toast = %q", got)
	}
}
