package main

import (
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"
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
	b.WriteString("\n " + bold(title) + "\n\n")
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

const pickerHelp = "↑/↓ cwd · ←/→ command · Enter select · q cancel"

func pickerCommandRows(preset CommandPreset) []string {
	return []string{
		fmt.Sprintf(" Command:  ◀ %s ▶", bold(preset.Name)),
		"           " + dim(preset.Command),
	}
}

func pickerRow(i int, line string, selected int) string {
	marker := "   "
	if i == selected {
		marker = " ▶ "
	}
	return fmt.Sprintf("%s%s%2d)%s  %s", marker, ansiBold, i+1, ansiReset, line)
}

// renderNewPickerViewport returns the full picker when its size is unknown or
// it fits. On a short known terminal it reserves the command and footer chrome,
// then centers a window of rows around the selected cwd where possible.
func renderNewPickerViewport(title string, lines []string, presets []CommandPreset, state newPickerState, note string, rows int) string {
	full := renderNewPicker(title, lines, presets, state, note)
	if rows <= 0 || len(strings.Split(full, "\n")) <= rows {
		return full
	}

	preset := presets[state.Preset]
	command := pickerCommandRows(preset)
	prefix := []string{" " + bold(title), "", command[0], command[1], ""}
	suffix := []string{"", " " + dim(pickerHelp)}
	if note != "" {
		suffix = []string{"", " " + dim(note), "", " " + dim(pickerHelp)}
	}
	if rows < len(prefix)+len(suffix)+1 {
		return renderNewPickerCompact(title, lines, preset, state, rows)
	}

	capacity := rows - len(prefix) - len(suffix)
	start := state.Row - capacity/2
	if start < 0 {
		start = 0
	}
	if maxStart := len(lines) - capacity; start > maxStart {
		start = maxStart
	}
	visible := make([]string, 0, rows)
	visible = append(visible, prefix...)
	for i := start; i < start+capacity; i++ {
		visible = append(visible, pickerRow(i, lines[i], state.Row))
	}
	visible = append(visible, suffix...)
	return strings.Join(visible, "\n")
}

// renderNewPickerCompact keeps the selected row visible even when there is not
// enough height for the picker chrome and one cwd row. It adds context in order
// of usefulness as rows become available.
func renderNewPickerCompact(title string, lines []string, preset CommandPreset, state newPickerState, rows int) string {
	selected := pickerRow(state.Row, lines[state.Row], state.Row)
	if rows <= 1 {
		return selected
	}
	command := pickerCommandRows(preset)
	titleRow := " " + bold(title)
	helpRow := " " + dim(pickerHelp)
	switch rows {
	case 2:
		return strings.Join([]string{titleRow, selected}, "\n")
	case 3:
		return strings.Join([]string{titleRow, command[0], selected}, "\n")
	case 4:
		return strings.Join([]string{titleRow, command[0], command[1], selected}, "\n")
	default:
		return strings.Join([]string{titleRow, command[0], command[1], selected, helpRow}, "\n")
	}
}

// pickNewSession drives the picker in a read/handle loop until the user
// confirms a row+preset selection or cancels. Must be called in raw mode.
func pickNewSession(title string, lines []string, rowStart int, presets []CommandPreset, presetStart int, note string, wakes []wakeFD) (row, preset int, ok bool) {
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
	renderer := newScreenRenderer(os.Stdout)
	decoder := newInputDecoder()
	fd := int(os.Stdin.Fd())
	for {
		cols, rows, err := term.GetSize(fd)
		if err != nil {
			cols, rows = 0, 0
		}
		_ = renderer.Draw(renderNewPickerViewport(title, lines, presets, state, note, rows), cols, rows)
		keys, _ := readModalEvents(decoder, wakes)
		for _, key := range keys {
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
