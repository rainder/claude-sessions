package main

import (
	"strings"
	"testing"
)

func TestNewPickerStateAxesAndWrap(t *testing.T) {
	state := newPickerState{RowCount: 3, PresetCount: 2}
	state.handle(KeyLeft)
	if state.Row != 0 || state.Preset != 1 {
		t.Fatalf("left = %#v", state)
	}
	state.handle(KeyRight)
	if state.Preset != 0 {
		t.Fatalf("right = %#v", state)
	}
	state.handle(KeyUp)
	if state.Row != 2 || state.Preset != 0 {
		t.Fatalf("up = %#v", state)
	}
	state.handle(KeyDown)
	if state.Row != 0 {
		t.Fatalf("down = %#v", state)
	}
}

func TestNewPickerStateConfirmAndCancel(t *testing.T) {
	state := newPickerState{RowCount: 3, PresetCount: 2}
	if confirm, cancel := state.handle("2"); !confirm || cancel || state.Row != 1 {
		t.Fatalf("digit = confirm %v cancel %v state %#v", confirm, cancel, state)
	}
	if confirm, cancel := state.handle("q"); confirm || !cancel {
		t.Fatalf("q = confirm %v cancel %v", confirm, cancel)
	}
}

func TestRenderNewPicker(t *testing.T) {
	presets := []CommandPreset{{Name: "Claude", Command: "claude"}, {Name: "Fable", Command: "claude --model fable"}}
	out := renderNewPicker("New session on beluga", []string{"/repo", "enter path manually…"}, presets,
		newPickerState{Row: 1, Preset: 1, RowCount: 2, PresetCount: 2}, "remote suggestions unavailable")
	for _, want := range []string{"New session on beluga", "Fable", "claude --model fable", "←/→ command", "remote suggestions unavailable"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b[H") || strings.Contains(out, "\x1b[J") || strings.Contains(out, "\x1b[2J") {
		t.Fatalf("picker contains terminal positioning or clear: %q", out)
	}
}
