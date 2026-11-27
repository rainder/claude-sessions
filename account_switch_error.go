package main

import (
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// The Ctrl+W failure panel. A failed switch used to be a 4 s toast, gone
// before it was read; this modal stays until dismissed. It follows
// showInfoDialog's shape: its own screenRenderer and decoder, keys from
// readModalEvents (the single stdin consumer), raw mode throughout.

const (
	accountSwitchErrorInner = 64 // default box width, clamped down on a narrow terminal
	accountSwitchErrorTitle = "Account switch failed"
	accountSwitchErrorHint  = "Enter/Esc close"
	// accountSwitchErrorChrome is the two border rows plus headroom.
	accountSwitchErrorChrome = 4
)

// accountSwitchErrorClose reports whether key dismisses the panel: Enter, Esc,
// q, Space, and Ctrl-C (raw mode delivers it as a byte; here it closes the
// panel and never quits the app). Every other key is ignored.
func accountSwitchErrorClose(key string) bool {
	switch key {
	case KeyEnter, "\r", "\n", KeyEsc, "q", "Q", " ", "\x03":
		return true
	}
	return false
}

// accountSwitchErrorText makes text safe and flat enough to draw: escape and
// control sequences are dropped (a remote host's error text is untrusted),
// tabs expanded; newlines stay as line breaks.
func accountSwitchErrorText(s string) string {
	return fleetDisplayText(s)
}

// accountSwitchErrorBody is the panel's content lines at width: the title,
// host, tool and account, then the error wrapped, then the hint.
func accountSwitchErrorBody(host, tool, name, errText string, width int) []string {
	clean := func(s string) string {
		return strings.Join(strings.Fields(accountSwitchErrorText(s)), " ")
	}
	body := []string{
		colorize("1;31", accountSwitchErrorTitle),
		"",
		"host:    " + clean(host),
		"tool:    " + clean(tool),
		"account: " + clean(name),
		"",
	}
	for _, line := range strings.Split(accountSwitchErrorText(errText), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		body = append(body, wrapRunes(line, width)...)
	}
	return append(body, "", dim(accountSwitchErrorHint))
}

// renderAccountSwitchError draws the bordered panel centered in a cols x rows
// terminal, with renderInfoDialog's geometry: clamp to the width, clip each
// line, cut an over-tall body with "…" above the hint, and fall back to the
// top-left when the size is unknown (<=0).
func renderAccountSwitchError(host, tool, name, errText string, cols, rows int) string {
	innerWidth := accountSwitchErrorInner
	if cols > 0 {
		if max := cols - 4; innerWidth > max {
			innerWidth = max
		}
		if innerWidth < 1 {
			innerWidth = 1
		}
	}
	body := accountSwitchErrorBody(host, tool, name, errText, innerWidth)
	if rows > 0 {
		capacity := rows - accountSwitchErrorChrome
		if capacity < 2 {
			capacity = 2
		}
		if capacity < len(body) {
			hint := body[len(body)-1]
			body = append(body[:capacity-2:capacity-2], dim("…"), hint)
		}
	}

	pad := func(s string) string {
		s = clipLine(s, innerWidth)
		return confirmBoxV + " " + s + strings.Repeat(" ", innerWidth-visualLen(s)) + " " + confirmBoxV
	}
	box := make([]string, 0, len(body)+2)
	box = append(box, confirmBoxTL+strings.Repeat(confirmBoxH, innerWidth+2)+confirmBoxTR)
	for _, line := range body {
		box = append(box, pad(line))
	}
	box = append(box, confirmBoxBL+strings.Repeat(confirmBoxH, innerWidth+2)+confirmBoxBR)
	if cols <= 0 || rows <= 0 {
		return strings.Join(box, "\n")
	}

	left := (cols - (innerWidth + 4)) / 2
	if left < 0 {
		left = 0
	}
	leftPad := strings.Repeat(" ", left)
	for i, l := range box {
		box[i] = leftPad + l
	}
	top := (rows - len(box)) / 2
	if top < 0 {
		top = 0
	}
	lines := make([]string, 0, top+len(box))
	for i := 0; i < top; i++ {
		lines = append(lines, "")
	}
	return strings.Join(append(lines, box...), "\n")
}

// accountSwitchBell rings the terminal bell once when the panel opens. A
// seam so tests can capture it.
var accountSwitchBell = func(w io.Writer) { _, _ = io.WriteString(w, "\a") }

// showAccountSwitchError opens the failure panel and blocks until a close
// key. Must be called in raw mode; it never leaves raw or the alt-screen, so
// the caller's next render() paints over it. wakes keeps it centered across
// a resize.
func showAccountSwitchError(host, tool, name, errText string, wakes []wakeFD) {
	accountSwitchBell(terminalOutput)
	renderer := newScreenRenderer(os.Stdout)
	decoder := newInputDecoder()
	fd := int(os.Stdin.Fd())
	for {
		cols, rows, err := term.GetSize(fd)
		if err != nil {
			cols, rows = 0, 0
		}
		_ = renderer.Draw(renderAccountSwitchError(host, tool, name, errText, cols, rows), cols, rows)
		keys, _ := readModalEvents(decoder, wakes)
		for _, key := range keys {
			if accountSwitchErrorClose(key) {
				return
			}
		}
	}
}
