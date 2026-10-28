package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"
)

// The Fleet view's raw text screen (t on a card). The details panel indents
// every summary line and hard-wraps it at the panel width, so a command that
// wraps copies as "cmd a\n   b": two broken commands. This screen prints the
// card text as FLEET wrote it, from column 0, with the terminal's own
// autowrap on (?7h) and no clipping, so a long line soft-wraps and a drag
// copies it back as one line. Markdown is not rendered: a [label](url) link
// shows its full URL, which iTerm2 opens on Cmd+click by itself.

const fleetRawHelp = "raw card text · drag selects, Cmd+C copies · ↑↓ PgUp/PgDn scroll · any other key: back"

// fleetRawLines is the card's text for the raw screen: title, ticket and
// priority, the summary unchanged, what it waits on (times in now's
// location), and the options. The
// strings are the view's sanitized copies (fleetDisplayText), so no escape
// reaches the terminal.
func fleetRawLines(it fleetItem, now time.Time) []string {
	c := it.Card
	var out []string
	if strings.TrimSpace(c.Title) != "" {
		out = append(out, c.Title)
	}
	var meta []string
	if c.Ticket != "" {
		meta = append(meta, c.Ticket)
	}
	if it.Section != fleetSecSent {
		meta = append(meta, "P"+strconv.Itoa(c.priority()))
	}
	if c.Session != "" {
		meta = append(meta, "session "+c.Session)
	}
	if len(meta) > 0 {
		out = append(out, strings.Join(meta, " · "))
	}
	if strings.TrimSpace(c.Summary) != "" {
		out = append(out, "")
		out = append(out, strings.Split(c.Summary, "\n")...)
	}
	if c.Waiting != nil {
		out = append(out, "", fleetWaitingText(c.Waiting, now))
	} else if c.WaitingOn != "" {
		out = append(out, "", "Waiting on: "+c.WaitingOn)
	}
	if len(c.Options) > 0 {
		out = append(out, "", "Options:")
		for i, o := range c.Options {
			out = append(out, fmt.Sprintf("%d  %s — %s", i+1, o.Key, o.Label))
			if o.Line != "" {
				out = append(out, o.Line)
			}
		}
	}
	return out
}

// fleetRawRows is how many terminal rows line takes once the terminal wraps
// it at cols: at least one.
func fleetRawRows(line string, cols int) int {
	w := cellWidth(line)
	if cols <= 0 || w <= cols {
		return 1
	}
	return (w + cols - 1) / cols
}

// fleetRawFrame is what the raw screen writes for a cols×rows terminal with
// lines[top] at the top: the lines that fit above the help row, unclipped and
// with autowrap on, then the help row on the last row. It returns the bytes
// to write, top clamped to the lines, and how many lines it showed. The first
// line always shows, even when it is taller than the screen; rows <= 0
// (unknown size) shows every line and no help row.
func fleetRawFrame(lines []string, top, cols, rows int) (frame string, clamped, shown int) {
	top = min(max(top, 0), max(len(lines)-1, 0))
	room := rows - 1
	if rows <= 0 {
		room = int(^uint(0) >> 1) // unknown size: show every line
	}
	used := 0
	var b strings.Builder
	b.WriteString(screenSyncBegin + "\x1b[?7h" + screenHome + "\x1b[2J")
	for i := top; i < len(lines); i++ {
		n := fleetRawRows(lines[i], cols)
		if shown > 0 && used+n > room {
			break
		}
		if shown > 0 {
			b.WriteString("\n")
		}
		b.WriteString(lines[i])
		used += n
		shown++
	}
	if rows > 0 {
		fmt.Fprintf(&b, "\x1b[%d;1H", rows)
		help := fleetRawHelp
		if cols > 1 {
			// One column short of the edge, so the help row never wraps.
			help = truncateCells(help, cols-1)
		}
		b.WriteString(dim(help))
	}
	b.WriteString(screenSyncEnd)
	return b.String(), top, shown
}

// fleetRawKey moves top for a scroll key and reports whether the screen stays
// open: ↑ ↓ move a line, PgUp PgDn a page (shown lines), Home End to the ends;
// any other key closes it.
func fleetRawKey(k string, top, shown, n int) (next int, open bool) {
	page := max(shown, 1)
	switch k {
	case KeyUp:
		return max(top-1, 0), true
	case KeyDown:
		return min(top+1, max(n-1, 0)), true
	case KeyPageUp:
		return max(top-page, 0), true
	case KeyPageDown, " ":
		return min(top+page, max(n-1, 0)), true
	case KeyHome:
		return 0, true
	case KeyEnd:
		return max(n-page, 0), true
	}
	return top, false
}

// runFleetRawView shows lines on the raw screen until a key that is not a
// scroll key. It leaves autowrap off again (the TUI's ?7l) and the screen
// cleared; the caller invalidates its renderer so the next draw repaints all.
func runFleetRawView(fd int, lines []string, wakes []wakeFD) {
	dec := newInputDecoder()
	top := 0
	defer fmt.Fprint(os.Stdout, "\x1b[?7l"+screenHome+"\x1b[2J")
	// This screen exists so a drag selects the text: mouse reporting goes
	// off here and comes back on (the Fleet view captures it) on the way out.
	defer mouseMode.suspend(terminalOutput)()
	for {
		cols, rows, err := term.GetSize(fd)
		if err != nil {
			cols, rows = 0, 0
		}
		frame, t, shown := fleetRawFrame(lines, top, cols, rows)
		top = t
		fmt.Fprint(os.Stdout, frame)
		keys, _ := readModalEvents(dec, wakes)
		for _, k := range keys {
			next, open := fleetRawKey(k, top, shown, len(lines))
			if !open {
				return
			}
			top = next
		}
	}
}
