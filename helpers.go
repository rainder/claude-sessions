package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"golang.org/x/term"
)

// enterCooked restores the terminal to its original (cooked) mode and shows
// the cursor. Used around prompts and subprocesses that need normal input.
func enterCooked(fd int, oldState *term.State) {
	_ = term.Restore(fd, oldState)
	fmt.Print("\033[?25h")
}

// enterRaw re-enables raw mode and hides the cursor. Discards the new "old
// state" — the caller should keep the original cooked state for final restore.
// Re-enables OPOST after MakeRaw so '\n' still translates to '\r\n'.
func enterRaw(fd int) {
	_, _ = term.MakeRaw(fd)
	enableOutputProcessing(fd)
	fmt.Print("\033[?25l")
}

// readLine prompts and reads a line in cooked mode. Empty string on EOF.
// Clears any pending read deadline first so the scanner doesn't inherit a
// stale timeout from the TUI loop's previous readEvents call.
func readLine(prompt string) string {
	fmt.Print(prompt)
	_ = os.Stdin.SetReadDeadline(time.Time{})
	s := bufio.NewScanner(os.Stdin)
	if s.Scan() {
		return s.Text()
	}
	return ""
}

// confirm asks a yes/no question (default no). Anything starting with Y/y is yes.
func confirm(prompt string) bool {
	yn := readLine(prompt)
	return len(yn) > 0 && (yn[0] == 'y' || yn[0] == 'Y')
}

// pauseForKey prints a dim prompt and waits for one byte from stdin. Works in
// raw or cooked mode (we explicitly enable raw for the read so a single key
// suffices instead of needing Enter).
func pauseForKey(fd int, oldState *term.State) {
	fmt.Print(ansiDim + "[any key to continue]" + ansiReset)
	enterRaw(fd)
	_ = os.Stdin.SetReadDeadline(time.Time{}) // block indefinitely
	buf := make([]byte, 1)
	_, _ = os.Stdin.Read(buf)
	enterCooked(fd, oldState)
}

// pickMenu shows an arrow-key menu on a cleared screen and returns the index
// of the chosen line, or -1 on cancel. Must be called in raw mode (OPOST on).
// ↑/↓ move (wrapping), Enter confirms, digits 1-9 select directly,
// q/ESC/Ctrl-C cancel. The caller repaints the screen afterwards.
func pickMenu(title, hint string, lines []string, start int) int {
	if len(lines) == 0 {
		return -1
	}
	sel := start
	if sel < 0 || sel >= len(lines) {
		sel = 0
	}
	for {
		var b strings.Builder
		b.WriteString("\033[H\033[J\n " + bold(title) + "\n\n")
		for i, l := range lines {
			marker := "   "
			if i == sel {
				marker = " ▶ "
			}
			fmt.Fprintf(&b, "%s%s%2d)%s  %s\n", marker, ansiBold, i+1, ansiReset, l)
		}
		b.WriteString("\n " + dim(hint) + "\n")
		fmt.Print(b.String())
		for _, k := range readEventBlocking() {
			switch k {
			case KeyUp:
				sel = (sel + len(lines) - 1) % len(lines)
			case KeyDown:
				sel = (sel + 1) % len(lines)
			case "\r", "\n":
				return sel
			case "q", "Q", KeyEsc, "\x03":
				return -1
			default:
				if len(k) == 1 && k[0] >= '1' && k[0] <= '9' && int(k[0]-'1') < len(lines) {
					return int(k[0] - '1')
				}
			}
		}
	}
}

// runInteractive leaves the alt-screen + raw mode so the named program owns
// the terminal (e.g. tmux attach, ssh -t), runs it, then re-enters our UI.
func runInteractive(fd int, oldState *term.State, prog string, args ...string) error {
	// Exit alt-screen, restore wrap, show cursor, cooked mode.
	fmt.Print("\033[?7h\033[?1049l\033[?25h")
	_ = term.Restore(fd, oldState)
	cmd := exec.Command(prog, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	// Re-enter our UI mode (raw with OPOST preserved).
	_, _ = term.MakeRaw(fd)
	enableOutputProcessing(fd)
	fmt.Print("\033[?1049h\033[?25l\033[?7l")
	return err
}

// sanitizeForTmux replaces characters tmux doesn't allow in session names
// with '_', collapses runs, trims edges. Falls back to "claude" if empty.
func sanitizeForTmux(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	for strings.Contains(out, "__") {
		out = strings.ReplaceAll(out, "__", "_")
	}
	out = strings.Trim(out, "_")
	if out == "" {
		return "claude"
	}
	return out
}
