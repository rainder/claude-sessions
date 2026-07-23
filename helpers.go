package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"golang.org/x/term"
)

// Mouse reporting escape sequences (SGR extended mode, x10 button tracking).
// Split into enable/disable halves so callers can sequence them explicitly
// around terminal-mode transitions instead of leaving mouse reporting on
// across a handoff to a subprocess that doesn't expect it.
const (
	mouseEnableSequence  = "\x1b[?1000h\x1b[?1006h"
	mouseDisableSequence = "\x1b[?1006l\x1b[?1000l"
)

// writeMouseMode writes the enable or disable mouse-reporting sequence to w.
func writeMouseMode(w io.Writer, enabled bool) {
	if enabled {
		_, _ = io.WriteString(w, mouseEnableSequence)
		return
	}
	_, _ = io.WriteString(w, mouseDisableSequence)
}

// enterCooked restores the terminal to its original (cooked) mode and shows
// the cursor. Used around prompts and subprocesses that need normal input.
// Disables mouse reporting first so a subprocess or prompt reading stdin
// doesn't see stray SGR mouse byte sequences.
func enterCooked(fd int, oldState *term.State) {
	writeMouseMode(os.Stdout, false)
	_ = term.Restore(fd, oldState)
	fmt.Print("\033[?25h")
}

// enterRaw re-enables raw mode and hides the cursor. Discards the new "old
// state" — the caller should keep the original cooked state for final restore.
// Re-enables OPOST after MakeRaw so '\n' still translates to '\r\n'.
// Deliberately mouse-neutral: it does not enable mouse reporting. pauseForKey
// calls this to read a single key, and if mouse reporting were left on, a
// stray click during that window could leave a partial SGR mouse sequence in
// stdin for the next reader to choke on. Callers that want mouse reporting in
// the main TUI re-enable it explicitly after returning to raw mode.
func enterRaw(fd int) {
	_, _ = term.MakeRaw(fd)
	enableOutputProcessing(fd)
	fmt.Print("\033[?25l")
}

// readLine prompts and reads a line in cooked mode. Empty string on EOF.
// Clears any pending read deadline first so the scanner doesn't inherit a
// stale timeout left on stdin by an earlier reader (e.g. pauseForKey).
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

// writeInteractiveHandoff writes the terminal-mode transition that hands the
// terminal off to an interactive subprocess: disable mouse reporting, restore
// wrap, exit the alternate screen, clear the revealed primary screen, home the
// cursor, show the cursor — in that exact order. Clearing (2J+H) after leaving
// the alt-screen suppresses the flicker of stale primary-buffer shell contents
// before the subprocess (tmux attach / ssh -t) paints.
func writeInteractiveHandoff(w io.Writer) {
	_, _ = io.WriteString(w, mouseDisableSequence+"\033[?7h\033[?1049l\033[2J\033[H\033[?25h")
}

// runInteractive leaves the alt-screen + raw mode so the named program owns
// the terminal (e.g. tmux attach, ssh -t), runs it, then re-enters our UI.
func runInteractive(fd int, oldState *term.State, prog string, args ...string) error {
	// Disable mouse reporting, exit alt-screen, restore wrap, clear the
	// revealed primary screen, show cursor, cooked mode. Mouse reporting must
	// go first: the subprocess doesn't expect SGR mouse sequences on its
	// stdin. Clearing the primary screen avoids flashing stale shell output
	// before the subprocess paints.
	writeInteractiveHandoff(os.Stdout)
	_ = term.Restore(fd, oldState)
	cmd := exec.Command(prog, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	// Re-enter our UI mode (raw with OPOST preserved), then re-enable mouse
	// reporting last so it takes effect only once we're back in the TUI.
	_, _ = term.MakeRaw(fd)
	enableOutputProcessing(fd)
	fmt.Print("\033[?1049h\033[?25l\033[?7l" + mouseEnableSequence)
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

// shellQuote wraps s in single quotes for safe interpolation into a shell
// command line, escaping embedded single quotes with the standard POSIX
// close-escape-reopen break-out sequence. SpawnNew's tmux send-keys types the
// assembled command into a live shell, so an unquoted prompt containing shell
// metacharacters (backticks, $(), ;, quotes) would execute as arbitrary
// commands rather than reach claude as text.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
