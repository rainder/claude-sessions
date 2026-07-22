package main

import (
	"fmt"
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

func TestRenderNewPickerViewportKeepsFullOutputWhenItFits(t *testing.T) {
	presets := []CommandPreset{{Name: "Claude", Command: "claude"}}
	lines := []string{"/repo", "enter path manually…"}
	state := newPickerState{Row: 1, RowCount: len(lines), PresetCount: len(presets)}
	full := renderNewPicker("New session", lines, presets, state, "")

	if got := renderNewPickerViewport("New session", lines, presets, state, "", 40); got != full {
		t.Fatalf("viewport changed fitting output:\n got: %q\nwant: %q", got, full)
	}
}

func TestRenderNewPickerViewportKeepsSelectionVisible(t *testing.T) {
	presets := []CommandPreset{{Name: "Claude", Command: "claude"}}
	lines := make([]string, 12)
	for i := range lines {
		lines[i] = "entry-" + string(rune('a'+i))
	}
	lines[len(lines)-1] = "enter path manually…"

	for _, tc := range []struct {
		name string
		row  int
	}{
		{name: "top", row: 0},
		{name: "middle", row: 6},
		{name: "tail", row: 10},
		{name: "manual", row: len(lines) - 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := newPickerState{Row: tc.row, RowCount: len(lines), PresetCount: len(presets)}
			out := renderNewPickerViewport("New session", lines, presets, state, "", 8)
			want := " ▶ " + ansiBold + fmt.Sprintf("%2d)", tc.row+1) + ansiReset + "  " + lines[tc.row]
			if !strings.Contains(out, want) {
				t.Fatalf("selected row %d not visible with original marker and ordinal:\n%s", tc.row, out)
			}
			for _, chrome := range []string{"Command:", "Enter select · q cancel"} {
				if !strings.Contains(out, chrome) {
					t.Fatalf("viewport missing %q:\n%s", chrome, out)
				}
			}
			if got := len(strings.Split(out, "\n")); got > 8 {
				t.Fatalf("viewport has %d rows, want at most 8:\n%s", got, out)
			}
		})
	}
}

func TestRenderNewPickerViewportNoteReducesRowCapacity(t *testing.T) {
	presets := []CommandPreset{{Name: "Claude", Command: "claude"}}
	lines := []string{"one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten"}
	state := newPickerState{Row: 4, RowCount: len(lines), PresetCount: len(presets)}

	withoutNote := renderNewPickerViewport("New session", lines, presets, state, "", 10)
	withNote := renderNewPickerViewport("New session", lines, presets, state, "remote suggestions unavailable", 10)
	if !strings.Contains(withNote, "remote suggestions unavailable") {
		t.Fatalf("viewport omitted note:\n%s", withNote)
	}
	countRows := func(out string) int {
		count := 0
		for _, line := range lines {
			if strings.Contains(out, line) {
				count++
			}
		}
		return count
	}
	if got, wantLessThan := countRows(withNote), countRows(withoutNote); got >= wantLessThan {
		t.Fatalf("note rows = %d, want fewer than no-note rows %d", got, wantLessThan)
	}
	if !strings.Contains(withNote, lines[state.Row]) {
		t.Fatalf("note viewport hid selected row:\n%s", withNote)
	}
}

func TestRenderNewPickerViewportCompactHeightKeepsSelection(t *testing.T) {
	presets := []CommandPreset{{Name: "Claude", Command: "claude"}}
	lines := []string{"/repo", "/other", "enter path manually…"}
	state := newPickerState{Row: 2, RowCount: len(lines), PresetCount: len(presets)}

	out := renderNewPickerViewport("New session", lines, presets, state, "", 2)
	if !strings.Contains(out, " ▶ "+ansiBold+" 3)"+ansiReset+"  "+lines[state.Row]) {
		t.Fatalf("compact viewport hid selected manual row:\n%s", out)
	}
	if got := len(strings.Split(out, "\n")); got > 2 {
		t.Fatalf("compact viewport has %d rows, want at most 2:\n%s", got, out)
	}
}
