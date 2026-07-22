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

// feedKeys mimics pickNewSession's per-key loop: before each key it recomputes
// the filtered row count from the full lines so RowCount / Row stay consistent
// with the current filter, then dispatches to handle. It returns the final
// (confirm, cancel) and the original-index map for the last computed view.
func feedKeys(state *newPickerState, lines []string, keys ...string) (confirm, cancel bool, indices []int) {
	for _, key := range keys {
		_, indices = filterNewPickerLines(lines, state.Filter)
		state.RowCount = len(indices)
		if state.Row >= state.RowCount {
			state.Row = 0
		}
		confirm, cancel = state.handle(key)
		if confirm || cancel {
			return confirm, cancel, indices
		}
	}
	// Recompute the view once more so callers see the post-edit index map.
	_, indices = filterNewPickerLines(lines, state.Filter)
	state.RowCount = len(indices)
	if state.Row >= state.RowCount {
		state.Row = 0
	}
	return confirm, cancel, indices
}

func TestNewPickerFilterTypingNarrows(t *testing.T) {
	lines := []string{"/alpha", "/beta", "/gamma", "enter path manually…"}
	state := newPickerState{PresetCount: 1}
	if _, _, indices := feedKeys(&state, lines, "g", "a", "m"); state.Filter != "gam" || len(indices) != 1 || indices[0] != 2 {
		t.Fatalf("filter %q indices %v", state.Filter, indices)
	}
	// Row was reset to the top match on each keystroke.
	if state.Row != 0 {
		t.Fatalf("row = %d, want 0", state.Row)
	}
}

func TestNewPickerFilterBackspace(t *testing.T) {
	lines := []string{"/alpha", "/beta", "/gamma", "enter path manually…"}
	state := newPickerState{PresetCount: 1}
	feedKeys(&state, lines, "a", "l")
	if state.Filter != "al" {
		t.Fatalf("filter = %q, want %q", state.Filter, "al")
	}
	feedKeys(&state, lines, "\x7f") // DEL backspace
	if state.Filter != "a" {
		t.Fatalf("after backspace filter = %q, want %q", state.Filter, "a")
	}
	feedKeys(&state, lines, "\x08") // BS backspace clears to empty
	if state.Filter != "" {
		t.Fatalf("after second backspace filter = %q, want empty", state.Filter)
	}
	// Backspace on an empty filter is a harmless no-op.
	feedKeys(&state, lines, "\x7f")
	if state.Filter != "" {
		t.Fatalf("backspace on empty filter changed it to %q", state.Filter)
	}
}

func TestNewPickerFilterEnterMapsOriginalIndex(t *testing.T) {
	lines := []string{"/alpha", "/beta", "/gamma", "enter path manually…"}
	state := newPickerState{PresetCount: 1}
	// Type "bet" → only "/beta" (original index 1) matches; Enter confirms it.
	confirm, cancel, indices := feedKeys(&state, lines, "b", "e", "t", KeyEnter)
	if !confirm || cancel {
		t.Fatalf("confirm %v cancel %v", confirm, cancel)
	}
	if len(indices) != 1 || indices[state.Row] != 1 {
		t.Fatalf("indices %v row %d, want original index 1", indices, state.Row)
	}
}

func TestNewPickerFilterQIsLiteralText(t *testing.T) {
	lines := []string{"/queue", "/beta", "enter path manually…"}
	state := newPickerState{PresetCount: 1}
	// Start a filter, then 'q' must extend it rather than cancel.
	confirm, cancel, _ := feedKeys(&state, lines, "u", "q")
	if confirm || cancel {
		t.Fatalf("filtering 'q' should not confirm/cancel: confirm %v cancel %v", confirm, cancel)
	}
	if state.Filter != "uq" {
		t.Fatalf("filter = %q, want %q", state.Filter, "uq")
	}
}

func TestNewPickerEmptyFilterQCancels(t *testing.T) {
	lines := []string{"/alpha", "enter path manually…"}
	state := newPickerState{PresetCount: 1}
	confirm, cancel, _ := feedKeys(&state, lines, "q")
	if confirm || !cancel {
		t.Fatalf("empty-filter q: confirm %v cancel %v, want cancel", confirm, cancel)
	}
}

func TestNewPickerEscCancelsWhileFiltering(t *testing.T) {
	lines := []string{"/alpha", "/beta", "enter path manually…"}
	state := newPickerState{PresetCount: 1}
	confirm, cancel, _ := feedKeys(&state, lines, "a", "l", KeyEsc)
	if confirm || !cancel {
		t.Fatalf("Esc while filtering: confirm %v cancel %v, want cancel", confirm, cancel)
	}
}

func TestNewPickerDigitShortcutWhenEmptyFilter(t *testing.T) {
	lines := []string{"/alpha", "/beta", "/gamma", "enter path manually…"}
	state := newPickerState{PresetCount: 1}
	confirm, cancel, indices := feedKeys(&state, lines, "3")
	if !confirm || cancel {
		t.Fatalf("digit 3: confirm %v cancel %v", confirm, cancel)
	}
	if state.Filter != "" || indices[state.Row] != 2 {
		t.Fatalf("digit selected row %d (filter %q), want original index 2", state.Row, state.Filter)
	}
}

func TestNewPickerDigitExtendsFilterWhileFiltering(t *testing.T) {
	lines := []string{"/r1", "/r2", "enter path manually…"}
	state := newPickerState{PresetCount: 1}
	// With a filter active, a digit is literal text, not a jump-select: typing
	// "r" then "1" narrows to the "/r1" row (original index 0).
	confirm, cancel, indices := feedKeys(&state, lines, "r", "1")
	if confirm || cancel {
		t.Fatalf("digit while filtering: confirm %v cancel %v", confirm, cancel)
	}
	if state.Filter != "r1" {
		t.Fatalf("filter = %q, want %q", state.Filter, "r1")
	}
	if len(indices) != 1 || indices[0] != 0 {
		t.Fatalf("indices %v, want single match at original index 0", indices)
	}
}

func TestNewPickerZeroMatchesNoCrash(t *testing.T) {
	lines := []string{"/alpha", "/beta", "enter path manually…"}
	state := newPickerState{PresetCount: 1}
	feedKeys(&state, lines, "z", "z", "z") // matches nothing
	if state.RowCount != 0 {
		t.Fatalf("RowCount = %d, want 0", state.RowCount)
	}
	// Arrow keys and confirm must not panic (divide-by-zero) on an empty view.
	if _, cancel := state.handle(KeyDown); cancel {
		t.Fatalf("KeyDown on empty view cancelled")
	}
	if _, cancel := state.handle(KeyUp); cancel {
		t.Fatalf("KeyUp on empty view cancelled")
	}
	if confirm, _ := state.handle(KeyEnter); !confirm {
		t.Fatalf("KeyEnter still reports confirm=true; caller must guard RowCount==0")
	}
	if state.Row != 0 {
		t.Fatalf("row drifted to %d on empty view", state.Row)
	}
}

func TestFilterNewPickerLines(t *testing.T) {
	lines := []string{"/alpha", "/BETA", "/gamma  " + dim("(2)")}
	// Empty filter is the identity with an identity index map.
	got, idx := filterNewPickerLines(lines, "")
	if len(got) != 3 || idx[0] != 0 || idx[2] != 2 {
		t.Fatalf("empty filter got %v idx %v", got, idx)
	}
	// Case-insensitive substring.
	got, idx = filterNewPickerLines(lines, "beta")
	if len(got) != 1 || idx[0] != 1 {
		t.Fatalf("beta got %v idx %v", got, idx)
	}
	// ANSI codes in the freq suffix must not spuriously match; "2" only lives
	// inside the stripped visible text, not the escape bytes.
	got, idx = filterNewPickerLines(lines, "gamma")
	if len(got) != 1 || idx[0] != 2 {
		t.Fatalf("gamma got %v idx %v", got, idx)
	}
}

func TestStripSGR(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"plain", "plain"},
		{dim("(3)"), "(3)"},
		{"a" + bold("b") + "c", "abc"},
		{"", ""},
	}
	for _, c := range cases {
		if got := stripSGR(c.in); got != c.want {
			t.Errorf("stripSGR(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRenderNewPickerShowsFilterAndNoMatches(t *testing.T) {
	presets := []CommandPreset{{Name: "Claude", Command: "claude"}}
	// Non-empty filter with matches shows the filter line and filtering footer.
	out := renderNewPicker("New session", []string{"/repo"}, presets,
		newPickerState{Row: 0, RowCount: 1, PresetCount: 1, Filter: "re"}, "")
	if !strings.Contains(out, "Filter:") || !strings.Contains(out, "re") {
		t.Fatalf("filter line missing:\n%s", out)
	}
	if !strings.Contains(out, "Esc cancel") {
		t.Fatalf("filtering footer missing:\n%s", out)
	}
	// Empty filtered list renders the placeholder without indexing rows.
	empty := renderNewPicker("New session", nil, presets,
		newPickerState{Row: 0, RowCount: 0, PresetCount: 1, Filter: "zzz"}, "")
	if !strings.Contains(empty, "(no matches)") {
		t.Fatalf("no-matches placeholder missing:\n%s", empty)
	}
}

func TestRenderNewPickerViewportEmptyFilteredNoPanic(t *testing.T) {
	presets := []CommandPreset{{Name: "Claude", Command: "claude"}}
	// A tiny terminal height with zero rows must not panic indexing lines[Row].
	out := renderNewPickerViewport("New session", nil, presets,
		newPickerState{Row: 0, RowCount: 0, PresetCount: 1, Filter: "zzz"}, "", 2)
	if !strings.Contains(out, "(no matches)") {
		t.Fatalf("viewport did not render placeholder for empty filtered list:\n%s", out)
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

func TestRenderNewPickerViewportNearFitKeepsFullOutput(t *testing.T) {
	presets := []CommandPreset{{Name: "Claude", Command: "claude"}}
	lines := []string{"/repo", "enter path manually…"}
	state := newPickerState{Row: 1, RowCount: len(lines), PresetCount: len(presets)}
	full := renderNewPicker("New session", lines, presets, state, "")
	rows := len(strings.Split(full, "\n")) - 1 // trailing newline is not a visible content row

	if got := renderNewPickerViewport("New session", lines, presets, state, "", rows); got != full {
		t.Fatalf("viewport changed near-fitting output:\n got: %q\nwant: %q", got, full)
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
