package main

import (
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"
)

// newPickerState holds the cursor position for the two-axis new-session
// picker: Row selects the cwd/path line, Preset selects the command preset.
// Filter, when non-empty, narrows the visible rows to a case-insensitive
// substring match; RowCount always reflects the currently-filtered row count,
// which the caller recomputes each iteration.
//
// PromptMode/Prompt hold a third, independent axis: pressing 'p' with no
// active Filter opens a free-text prompt buffer (Prompt) layered on top of the
// current Row/Preset selection. Confirming it (Enter) launches the session in
// the background with that prompt instead of attaching; Esc discards the
// buffer and returns to normal picker navigation. Prompt text is not subject
// to the Filter special-casing ('q' cancels, digits jump-select) — every
// printable byte is literal prompt content.
type newPickerState struct {
	Row, Preset           int
	RowCount, PresetCount int
	Filter                string
	PromptMode            bool
	Prompt                string
}

// handle applies one key event to the state, returning whether the picker
// should confirm (return the current selection) or cancel.
//
// While a filter is active (Filter != ""), 'q'/'Q' and the 1-9 digits are
// treated as literal filter text rather than cancel / quick-select, so Esc is
// the only way to cancel and digits extend the filter. With no filter the
// original shortcuts stand: 'q' cancels, 1-9 jump-select. Any printable ASCII
// byte extends the filter; Backspace (DEL 0x7f or BS 0x08) trims its last
// character. Filter edits reset Row to 0 so the top match is selected.
func (s *newPickerState) handle(key string) (confirm, cancel bool) {
	switch key {
	case KeyUp:
		if s.RowCount > 0 {
			s.Row = (s.Row + s.RowCount - 1) % s.RowCount
		}
	case KeyDown:
		if s.RowCount > 0 {
			s.Row = (s.Row + 1) % s.RowCount
		}
	case KeyLeft:
		s.Preset = (s.Preset + s.PresetCount - 1) % s.PresetCount
	case KeyRight:
		s.Preset = (s.Preset + 1) % s.PresetCount
	case "\r", "\n", KeyEnter:
		return true, false
	case KeyEsc, "\x03":
		return false, true
	case "\x7f", "\x08":
		if s.Filter != "" {
			s.Filter = s.Filter[:len(s.Filter)-1]
			s.Row = 0
		}
	default:
		// 'q' cancels only when not filtering (it's a valid path character).
		if s.Filter == "" && (key == "q" || key == "Q") {
			return false, true
		}
		// 'p' opens the background-prompt overlay, only when not filtering
		// ('p' is a valid path character while a filter is active).
		if s.Filter == "" && (key == "p" || key == "P") {
			s.PromptMode = true
			return false, false
		}
		// Digit quick-select only when not filtering; otherwise digits are
		// ordinary filter text.
		if s.Filter == "" && len(key) == 1 && key[0] >= '1' && key[0] <= '9' {
			row := int(key[0] - '1')
			if row < s.RowCount {
				s.Row = row
				return true, false
			}
			return false, false
		}
		// Any other printable ASCII byte extends the filter. This excludes the
		// multi-byte "\x00…" key sentinels (arrows, Enter, …) and control bytes.
		if len(key) == 1 && key[0] >= 0x20 && key[0] <= 0x7e {
			s.Filter += key
			s.Row = 0
		}
	}
	return false, false
}

// handlePrompt applies one key event while PromptMode is active, editing the
// free-text Prompt buffer. Enter confirms the whole picker (the caller reads
// row + preset + the composed Prompt) as long as the buffer is non-empty;
// Esc discards the buffer and drops back to normal picker navigation without
// cancelling the picker; Ctrl+C cancels the picker entirely, mirroring
// handle's Ctrl+C behavior.
func (s *newPickerState) handlePrompt(key string) (confirm, cancel bool) {
	switch key {
	case "\r", "\n", KeyEnter:
		if s.Prompt == "" {
			return false, false
		}
		return true, false
	case "\x03":
		return false, true
	case KeyEsc:
		s.PromptMode = false
		s.Prompt = ""
	case "\x7f", "\x08":
		if s.Prompt != "" {
			s.Prompt = s.Prompt[:len(s.Prompt)-1]
		}
	default:
		// Every printable ASCII byte is literal prompt content — unlike the
		// cwd Filter, there is no jump-select or cancel shortcut to carve out.
		if len(key) == 1 && key[0] >= 0x20 && key[0] <= 0x7e {
			s.Prompt += key
		}
	}
	return false, false
}

// filterNewPickerLines narrows lines to those whose visible text contains
// filter (case-insensitive substring). It returns the matching lines plus a
// parallel slice mapping each visible row back to its index in the original
// lines — so the caller can translate a confirmed filtered row into the
// original entry index. An empty filter is the identity: all lines, indices
// 0..n-1. ANSI color codes embedded in a row are stripped before matching so
// the filter tests what the user sees, not escape sequences.
func filterNewPickerLines(lines []string, filter string) (filtered []string, indices []int) {
	if filter == "" {
		indices = make([]int, len(lines))
		for i := range lines {
			indices[i] = i
		}
		return lines, indices
	}
	needle := strings.ToLower(filter)
	for i, line := range lines {
		if strings.Contains(strings.ToLower(stripSGR(line)), needle) {
			filtered = append(filtered, line)
			indices = append(indices, i)
		}
	}
	return filtered, indices
}

// stripSGR removes ANSI SGR ("\033[…m") escape sequences from s, leaving the
// visible text. Mirrors visualLen's scan (render.go) but returns the stripped
// string rather than its width.
func stripSGR(s string) string {
	if !strings.Contains(s, "\033[") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == '\033' && i+1 < len(s) && s[i+1] == '[' {
			if j := strings.IndexByte(s[i:], 'm'); j >= 0 {
				i += j + 1
				continue
			}
			b.WriteString(s[i:])
			break
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// renderNewPicker draws the two-axis new-session modal: a preset selector
// on top and a scrollable list of cwd/path lines below. When a filter is
// active it shows the filter string under the title and, if nothing matches, a
// "(no matches)" placeholder in place of the rows.
func renderNewPicker(title string, lines []string, presets []CommandPreset, state newPickerState, note string) string {
	preset := presets[state.Preset]
	var b strings.Builder
	b.WriteString("\n " + bold(title) + "\n\n")
	if state.Filter != "" {
		fmt.Fprintf(&b, " %s %s\n\n", dim("Filter:"), state.Filter)
	}
	fmt.Fprintf(&b, " Command:  ◀ %s ▶\n           %s\n\n", bold(preset.Name), dim(preset.Command))
	if len(lines) == 0 {
		b.WriteString("   " + dim("(no matches)") + "\n")
	}
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
	b.WriteString("\n " + dim(pickerFooter(state.Filter)) + "\n")
	return b.String()
}

// pickerFooter returns the footer hint, which differs while filtering (Esc
// cancels and Backspace edits, since 'q' is filter text). The no-filter hint
// keeps "Enter select · q cancel" contiguous so callers/tests that match that
// substring stay valid.
func pickerFooter(filter string) string {
	if filter != "" {
		return "↑/↓ cwd · ←/→ command · Enter select · ⌫ edit · Esc cancel"
	}
	return pickerHelp
}

const pickerHelp = "↑/↓ cwd · ←/→ command · type to filter · p prompt · Enter select · q cancel"

// renderPromptInput draws the background-prompt overlay: the chosen command
// preset and cwd for context, then the free-text buffer being composed.
// Confirming launches the session detached (no attach) with this prompt as
// its initial input.
func renderPromptInput(title string, preset CommandPreset, cwdLabel, prompt string) string {
	var b strings.Builder
	b.WriteString("\n " + bold(title) + "\n\n")
	fmt.Fprintf(&b, " Command:  %s\n", bold(preset.Name))
	if cwdLabel != "" {
		fmt.Fprintf(&b, " Cwd:      %s\n", cwdLabel)
	}
	b.WriteString("\n " + dim("Prompt (runs in background, no attach):") + "\n")
	fmt.Fprintf(&b, " > %s%s\n", prompt, dim("_"))
	b.WriteString("\n " + dim("Enter run in background · ⌫ edit · Esc back") + "\n")
	return b.String()
}

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
	// No rows to window (e.g. a filter with no matches): renderNewPicker draws
	// the placeholder without indexing into lines, so skip the compaction path
	// that would index lines[state.Row].
	if len(lines) == 0 {
		return renderNewPicker(title, lines, presets, state, note)
	}
	full := renderNewPicker(title, lines, presets, state, note)
	fullRows := strings.Split(full, "\n")
	if fullRows[len(fullRows)-1] == "" {
		fullRows = fullRows[:len(fullRows)-1]
	}
	if rows <= 0 || len(fullRows) <= rows {
		return full
	}

	preset := presets[state.Preset]
	command := pickerCommandRows(preset)
	prefix := []string{" " + bold(title), ""}
	if state.Filter != "" {
		prefix = append(prefix, " "+dim("Filter:")+" "+state.Filter, "")
	}
	prefix = append(prefix, command[0], command[1], "")
	footer := " " + dim(pickerFooter(state.Filter))
	suffix := []string{"", footer}
	if note != "" {
		suffix = []string{"", " " + dim(note), "", footer}
	}
	if rows < len(prefix)+len(suffix)+1 {
		return renderNewPickerCompact(title, lines, preset, state, rows)
	}

	capacity := rows - len(prefix) - len(suffix)
	if capacity > len(lines) {
		capacity = len(lines)
	}
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
	if state.Filter != "" {
		// Fold the filter into the title row so the compact layout keeps its
		// fixed per-height row budgets.
		titleRow += "  " + dim("/"+state.Filter)
	}
	helpRow := " " + dim(pickerFooter(state.Filter))
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
//
// prompt is empty for a normal confirm (caller attaches as before). A
// non-empty prompt means the user composed one via the 'p' overlay and
// confirmed it: the caller should spawn and launch in the background —
// append the prompt to the command and skip attaching — instead of
// attaching interactively.
func pickNewSession(title string, lines []string, rowStart int, presets []CommandPreset, presetStart int, note string, wakes []wakeFD) (row, preset int, prompt string, ok bool) {
	if len(lines) == 0 || len(presets) == 0 {
		return 0, 0, "", false
	}
	state := newPickerState{Row: rowStart, Preset: presetStart, PresetCount: len(presets)}
	if state.Row < 0 || state.Row >= len(lines) {
		state.Row = 0
	}
	if state.Preset < 0 || state.Preset >= state.PresetCount {
		state.Preset = 0
	}
	renderer := newScreenRenderer(os.Stdout)
	decoder := newInputDecoder()
	fd := int(os.Stdin.Fd())

	// sync recomputes the filtered view from the full lines + current Filter and
	// keeps RowCount / Row consistent with it. It returns the index map so a
	// confirmed filtered row can be translated back to the original lines index.
	sync := func() (filtered []string, indices []int) {
		filtered, indices = filterNewPickerLines(lines, state.Filter)
		state.RowCount = len(filtered)
		if state.Row >= state.RowCount {
			state.Row = 0
		}
		return filtered, indices
	}

	for {
		filtered, indices := sync()
		cols, rows, err := term.GetSize(fd)
		if err != nil {
			cols, rows = 0, 0
		}
		var content string
		if state.PromptMode {
			cwdLabel := ""
			if state.Row < len(filtered) {
				cwdLabel = stripSGR(filtered[state.Row])
			}
			content = renderPromptInput(title, presets[state.Preset], cwdLabel, state.Prompt)
		} else {
			content = renderNewPickerViewport(title, filtered, presets, state, note, rows)
		}
		_ = renderer.Draw(content, cols, rows)
		keys, _ := readModalEvents(decoder, wakes)
		for _, key := range keys {
			// Recompute before each key so RowCount and the index map reflect
			// any filter edit earlier in the same batch (e.g. a pasted
			// "foo\r": Enter must map through the post-"foo" indices).
			_, indices = sync()
			var confirm, cancel bool
			if state.PromptMode {
				confirm, cancel = state.handlePrompt(key)
			} else {
				confirm, cancel = state.handle(key)
			}
			if cancel {
				return 0, 0, "", false
			}
			if confirm {
				if state.RowCount == 0 {
					// Nothing matches the current filter; ignore the confirm.
					continue
				}
				return indices[state.Row], state.Preset, state.Prompt, true
			}
		}
	}
}
