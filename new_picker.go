package main

import (
	"fmt"
	"strings"
)

// newPickerState holds the cursor position for the two-axis new-session
// picker: Row selects the cwd/path line, Preset selects the command preset.
type newPickerState struct {
	Row, Preset           int
	RowCount, PresetCount int
}

// handle applies one key event to the state, returning whether the picker
// should confirm (return the current selection) or cancel.
func (s *newPickerState) handle(key string) (confirm, cancel bool) {
	switch key {
	case KeyUp:
		s.Row = (s.Row + s.RowCount - 1) % s.RowCount
	case KeyDown:
		s.Row = (s.Row + 1) % s.RowCount
	case KeyLeft:
		s.Preset = (s.Preset + s.PresetCount - 1) % s.PresetCount
	case KeyRight:
		s.Preset = (s.Preset + 1) % s.PresetCount
	case "\r", "\n", KeyEnter:
		return true, false
	case "q", "Q", KeyEsc, "\x03":
		return false, true
	default:
		if len(key) == 1 && key[0] >= '1' && key[0] <= '9' {
			row := int(key[0] - '1')
			if row < s.RowCount {
				s.Row = row
				return true, false
			}
		}
	}
	return false, false
}

// renderNewPicker draws the two-axis new-session modal: a preset selector
// on top and a scrollable list of cwd/path lines below.
func renderNewPicker(title string, lines []string, presets []CommandPreset, state newPickerState, note string) string {
	preset := presets[state.Preset]
	var b strings.Builder
	b.WriteString("\033[H\033[J\n " + bold(title) + "\n\n")
	fmt.Fprintf(&b, " Command:  ◀ %s ▶\n           %s\n\n", bold(preset.Name), dim(preset.Command))
	for i, line := range lines {
		marker := "   "
		if i == state.Row {
			marker = " ▶ "
		}
		fmt.Fprintf(&b, "%s%s%2d)%s  %s\n", marker, ansiBold, i+1, ansiReset, line)
	}
	if note != "" {
		b.WriteString("\n " + dim(note) + "\n")
	}
	b.WriteString("\n " + dim("↑/↓ cwd · ←/→ command · Enter select · q cancel") + "\n")
	return b.String()
}

// pickNewSession drives renderNewPicker in a read/handle loop until the user
// confirms a row+preset selection or cancels. Must be called in raw mode.
func pickNewSession(title string, lines []string, rowStart int, presets []CommandPreset, presetStart int, note string) (row, preset int, ok bool) {
	if len(lines) == 0 || len(presets) == 0 {
		return 0, 0, false
	}
	state := newPickerState{Row: rowStart, Preset: presetStart, RowCount: len(lines), PresetCount: len(presets)}
	if state.Row < 0 || state.Row >= state.RowCount {
		state.Row = 0
	}
	if state.Preset < 0 || state.Preset >= state.PresetCount {
		state.Preset = 0
	}
	for {
		fmt.Print(renderNewPicker(title, lines, presets, state, note))
		for _, key := range readEventBlocking() {
			confirm, cancel := state.handle(key)
			if cancel {
				return 0, 0, false
			}
			if confirm {
				return state.Row, state.Preset, true
			}
		}
	}
}
