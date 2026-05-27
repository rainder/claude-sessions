package main

import (
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// enableOutputProcessing re-enables OPOST | ONLCR after term.MakeRaw, which
// turns them off. Without this, '\n' moves the cursor down but not back to
// column 0, breaking every multi-line render.
func enableOutputProcessing(fd int) {
	t, err := unix.IoctlGetTermios(fd, ioctlGetTermios)
	if err != nil {
		return
	}
	t.Oflag |= unix.OPOST | unix.ONLCR
	_ = unix.IoctlSetTermios(fd, ioctlSetTermios, t)
}

// Key constants returned by parseEvents.
const (
	KeyUp    = "\x00up"
	KeyDown  = "\x00down"
	KeyLeft  = "\x00left"
	KeyRight = "\x00right"
	KeyEsc   = "\x00esc"
)

// parseEvents extracts keystrokes from a raw input chunk read from a terminal
// in raw mode. ESC sequences for arrow keys are recognized as KeyUp / KeyDown /
// KeyLeft / KeyRight; bare ESC becomes KeyEsc; everything else is the literal
// byte as a 1-rune string.
func parseEvents(buf []byte) []string {
	var out []string
	for len(buf) > 0 {
		if buf[0] == 0x1b {
			if len(buf) >= 3 && (buf[1] == '[' || buf[1] == 'O') {
				switch buf[2] {
				case 'A':
					out = append(out, KeyUp)
				case 'B':
					out = append(out, KeyDown)
				case 'C':
					out = append(out, KeyRight)
				case 'D':
					out = append(out, KeyLeft)
				default:
					out = append(out, KeyEsc)
				}
				buf = buf[3:]
				continue
			}
			out = append(out, KeyEsc)
			buf = buf[1:]
			continue
		}
		out = append(out, string(buf[0]))
		buf = buf[1:]
	}
	return out
}

// readEvents reads up to timeout for a stdin chunk and parses keystrokes.
// Returns nil on timeout or read error. Single-threaded — no goroutine — so
// cooked-mode prompts (bufio.Scanner) and raw-mode polling never race on
// the same fd.
func readEvents(timeout time.Duration) []string {
	if timeout > 0 {
		_ = os.Stdin.SetReadDeadline(time.Now().Add(timeout))
	} else {
		_ = os.Stdin.SetReadDeadline(time.Time{})
	}
	buf := make([]byte, 16)
	n, err := os.Stdin.Read(buf)
	if err != nil || n == 0 {
		return nil
	}
	return parseEvents(buf[:n])
}

// readEventBlocking waits indefinitely for the next event(s) from stdin.
// Useful inside action handlers that pause for input ("any key to continue").
func readEventBlocking() []string {
	return readEvents(0)
}

// isTimeoutErr returns true for the deadline-exceeded errors os.Stdin.Read
// returns when the SetReadDeadline timer fires.
func isTimeoutErr(err error) bool {
	return errors.Is(err, os.ErrDeadlineExceeded)
}

// nav returns the next selection ID after moving by delta (+1 down, -1 up).
// Wraps at the ends. Defaults to the first/last row when sel is empty.
func nav(sessions []Session, sel string, delta int) string {
	n := len(sessions)
	if n == 0 {
		return ""
	}
	if sel == "" {
		if delta > 0 {
			return sessions[0].ID()
		}
		return sessions[n-1].ID()
	}
	for i, s := range sessions {
		if s.ID() == sel {
			j := ((i+delta)%n + n) % n
			return sessions[j].ID()
		}
	}
	return sessions[0].ID()
}

// validateSel ensures sel still exists in the session list, defaulting to the
// first row when not (covers the case where the selected session died).
func validateSel(sessions []Session, sel string) string {
	for _, s := range sessions {
		if s.ID() == sel {
			return sel
		}
	}
	if len(sessions) > 0 {
		return sessions[0].ID()
	}
	return ""
}

// RunTUI is the live view: alt-screen, raw mode, render-loop, key handler.
// Returns nil on clean quit (q / Ctrl-C / Ctrl-D), or an error if setup failed.
func RunTUI(interval time.Duration) error {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return fmt.Errorf("stdin is not a terminal")
	}
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return fmt.Errorf("set raw mode: %w", err)
	}
	defer term.Restore(fd, oldState)
	// Re-enable output processing so '\n' still translates to '\r\n'.
	enableOutputProcessing(fd)

	// Alt-screen, hide cursor, disable line-wrap. Restored on return.
	fmt.Print("\033[?1049h\033[?25l\033[?7l")
	defer fmt.Print("\033[?7h\033[?25h\033[?1049l")

	viewMode := LoadViewMode()
	var local []Session
	var remotes []RemoteResult
	sel := ""

	// refresh refetches local sessions (always) and remote sessions (when
	// pollRemote is true). Arrow nav does NOT trigger remote re-poll —
	// otherwise every keypress would hit the network.
	refresh := func(pollRemote bool) {
		if s, err := CollectLocal(); err == nil {
			local = s
		}
		if pollRemote {
			remotes = FetchAllRemote()
		}
		sel = validateSel(AllSessions(local, remotes), sel)
	}
	render := func() {
		fmt.Print("\033[H\033[J")
		RenderAll(os.Stdout, viewMode, local, remotes, sel)
	}

	makeCtx := func() *actCtx {
		return &actCtx{
			fd:       fd,
			oldState: oldState,
			sessions: AllSessions(local, remotes),
			sel:      sel,
		}
	}

	refresh(true)
	render()

	for {
		events := readEvents(interval)
		if len(events) == 0 {
			refresh(true) // tick: full refresh including remote
			render()
			continue
		}
		for _, k := range events {
			switch k {
			case "q", "Q", "\x03", "\x04":
				return nil
			case KeyUp:
				sel = nav(AllSessions(local, remotes), sel, -1)
				render()
			case KeyDown:
				sel = nav(AllSessions(local, remotes), sel, 1)
				render()
			case "k", "K":
				actKill(makeCtx())
				refresh(true)
				render()
			case "a", "A":
				actAttach(makeCtx())
				refresh(true)
				render()
			case "p", "P":
				actPreview(makeCtx(), interval)
				refresh(true)
				render()
			case "n", "N":
				actNew(makeCtx())
				refresh(true)
				render()
			case "m", "M":
				if viewMode == "1" {
					viewMode = "2"
				} else {
					viewMode = "1"
				}
				SaveViewMode(viewMode)
				render()
			case "r", "R":
				refresh(true)
				render()
			case "?":
				renderHelp()
				readEventBlocking()
				render()
			}
		}
	}
}

// renderHelp paints the help modal. Caller waits for a keypress to dismiss.
func renderHelp() {
	fmt.Print("\033[H\033[J")
	fmt.Println(bold("claude-sessions  ·  help"))
	fmt.Println()
	fmt.Println("  " + bold("NAVIGATION"))
	fmt.Println("    ↑ / ↓        move selection")
	fmt.Println()
	fmt.Println("  " + bold("ACTIONS") + "  (on selected row)")
	fmt.Println("    n            new tmux+claude session (cwd picker)")
	fmt.Println("    k            kill the session (tmux-aware)")
	fmt.Println("    a            attach (or migrate to tmux first)")
	fmt.Println("    p            preview (tmux pane snapshot or transcript tail)")
	fmt.Println()
	fmt.Println("  " + bold("VIEW"))
	fmt.Println("    m            toggle mode (full ↔ minimal)  ·  persisted")
	fmt.Println("    r            refresh now")
	fmt.Println("    q / Ctrl-C   quit")
	fmt.Println("    ?            this help")
	fmt.Println()
	fmt.Println("  " + bold("SUBCOMMANDS") + "  (from the shell)")
	fmt.Println("    claude-sessions kill PID [-y]")
	fmt.Println("    claude-sessions migrate PID [-y]")
	fmt.Println("    claude-sessions new --cwd PATH [--name NAME]")
	fmt.Println("    claude-sessions preview PID")
	fmt.Println("    claude-sessions tmux-info PID")
	fmt.Println("    claude-sessions attach PID")
	fmt.Println()
	fmt.Println(dim("press any key to return"))
}
