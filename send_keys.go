// send_keys.go
package main

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"unicode/utf8"
)

// sendKeysMaxLen bounds a single compose-box message. Kept here so both the
// TUI (which can reject before paying a round trip) and the server's own
// validator (server.go's sendKeysBody) enforce the identical limit.
const sendKeysMaxLen = 4096

// validateSendKeysText is the text rule for one send-keys message, shared by
// the server (sendKeysBody) and the send-keys CLI, which checks before it
// resolves or sends anything so a local and a remote send refuse the same
// text. Empty, oversized and invalid UTF-8 text is refused, and so is every
// C0 control byte except the newline. A newline is a line break inside the
// message (sendKeys types it as one, never as Enter); every other control
// byte is refused, not just CR/NUL: sendKeys's -l flag only keeps tmux
// itself from parsing text as key names. It says nothing about the
// receiving pty — an embedded ESC (0x1b) reaches the pane as a literal byte
// and can still be interpreted there as the start of a terminal escape
// sequence. DEL (0x7f) is rejected alongside it for the same reason: neither
// belongs in message content.
func validateSendKeysText(text string) error {
	if text == "" {
		return fmt.Errorf("text must not be empty")
	}
	if len(text) > sendKeysMaxLen {
		return fmt.Errorf("text exceeds %d bytes", sendKeysMaxLen)
	}
	if !utf8.ValidString(text) {
		return fmt.Errorf("text must be valid UTF-8")
	}
	for i := 0; i < len(text); i++ {
		if b := text[i]; (b < 0x20 && b != '\n') || b == 0x7f {
			return fmt.Errorf("text must not contain control characters")
		}
	}
	return nil
}

// tmuxNewlineKey is the key typed between the lines of a multi-line message
// to a Claude Code session: a line break in its input box, not a submit.
//
// Probed 2026-11-24 against Claude Code 2.1.331 in a throwaway tmux pane
// (capture-pane after each try, nothing submitted):
//   - send-keys -l 'line1\' then send-keys Enter: a 2-line box. But a long
//     line typed in one send-keys shows as a "[Pasted text #1]" placeholder
//     (seen), so its backslash sits inside the paste and that Enter could
//     submit half the message (not tried: it would submit).
//   - send-keys S-Enter: a 2-line box, with this tmux's extended-keys on;
//     without it tmux has no Shift-Enter encoding to send.
//   - set-buffer + paste-buffer -p of "a\nb": a 2-line box, but tmux adds
//     the brackets only while the app asks for bracketed paste; without them
//     each newline is Enter.
//   - send-keys M-Enter: a 2-line box, also right after a long line that
//     became a "[Pasted text #1]" placeholder, and with an empty line (two
//     M-Enter in a row). Chosen: it needs no tmux option and no app mode.
const tmuxNewlineKey = "M-Enter"

// typeLines types text line by line: typeLine for each non-empty line,
// newline between lines. Neither ever gets a "\n".
func typeLines(text string, typeLine func(string) error, newline func() error) error {
	for i, line := range strings.Split(text, "\n") {
		if i > 0 {
			if err := newline(); err != nil {
				return err
			}
		}
		if line == "" {
			continue
		}
		if err := typeLine(line); err != nil {
			return err
		}
	}
	return nil
}

// sendKeysFlatten is a multi-line text as one line, for a session whose
// line-break key is unknown (grok): each newline becomes a space.
func sendKeysFlatten(text string) string { return strings.ReplaceAll(text, "\n", " ") }

// sendKeysPaneInMode and sendKeysTmux are sendKeys' seams, so a test never
// types into a real pane.
var (
	sendKeysPaneInMode = tmuxPaneInMode
	sendKeysTmux       = func(args ...string) error { return exec.Command("tmux", args...).Run() }
)

// sendKeys injects text into s's tmux pane as literal keystrokes followed by
// Enter — two calls, matching the existing tmuxSendLiteral (paste.go:403-405).
// A multi-line text is typed a line at a time with tmuxNewlineKey between
// the lines (typeLines), so only the final Enter submits; a grok session
// gets it flattened to one line (sendKeysFlatten) instead.
// The -l flag on the first call is required: without it tmux parses text as
// key names, so a message that happens to read "Enter" or "C-c" would trigger
// that tmux action instead of being typed literally. The "--" after it ends
// tmux's flag parsing: without it a text that starts with "-" is read as
// flags ("-R hello" fails with "invalid flag"). Neither call goes through a
// shell (exec.Command argument slices), so there is no shell-injection surface
// regardless of message content.
//
// A pane in copy-mode is refused before anything is typed: the keys would go
// to the copy-mode key table, a "q" in the text leaves copy-mode, and only the
// rest would reach the session — a mangled message, then submitted by Enter.
//
// s.Tmux must be non-empty. tmux does not fail safely on an empty target: `tmux
// send-keys -t "" ...` exits 0 and silently delivers to tmux's default/current
// session instead of erroring, so an unguarded call here would misdeliver
// keystrokes into the wrong pane with no error to catch it. Every other call
// site against Session.Tmux in this codebase checks it first (actions.go:182,256;
// commands.go:68; migrate.go:327; worktree.go:87,100; render.go:80) — this
// matches that idiom. resolveLivePIDLocal can legitimately return a Session
// with Tmux == "" (a claude session not yet mapped to a pane), so callers
// cannot assume the field is populated.
func sendKeys(s Session, text string) error {
	if s.Tmux == "" {
		return fmt.Errorf("PID %d has no tmux pane", s.PID)
	}
	pane := tmuxExactTarget(s.Tmux) // never a prefix match on a gone session
	inMode, err := sendKeysPaneInMode(pane)
	if err != nil {
		return fmt.Errorf("read pane mode: %w", err)
	}
	if inMode {
		return errors.New("pane is in scroll mode; press q in the terminal, then send again")
	}
	if s.IsGrok() {
		text = sendKeysFlatten(text)
	}
	err = typeLines(text,
		func(line string) error { return sendKeysTmux("send-keys", "-t", pane, "-l", "--", line) },
		func() error { return sendKeysTmux("send-keys", "-t", pane, tmuxNewlineKey) })
	if err != nil {
		return fmt.Errorf("send text: %w", err)
	}
	if err := sendKeysTmux("send-keys", "-t", pane, "Enter"); err != nil {
		return fmt.Errorf("send enter: %w", err)
	}
	return nil
}

// tmuxPaneInMode reports whether target is in a tmux mode such as copy-mode.
// Keys sent then go to the mode's key table, not to Claude: a "q" in the
// text leaves copy-mode and only the rest reaches the input box. Shared by
// sendKeys and the FLEET page's say.
func tmuxPaneInMode(target string) (bool, error) {
	if target == "" {
		return false, errors.New("no tmux pane")
	}
	out, err := exec.Command("tmux", "display", "-p", "-t", tmuxExactTarget(target), "#{pane_in_mode}").Output()
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) == "1", nil
}

// resolveLivePIDLocal is the local-TUI-process counterpart of the server's
// resolveLivePID (server.go:739): a fresh CollectLocal() so the returned
// Session.Tmux is current, not the possibly-stale pane address an inspector
// snapshot may still be holding. wantSessionID is mandatory — the TUI always
// has a real one from the live inspector snapshot, so unlike localReattest
// (migrate.go:360, which kill/migrate's legacy optional-precondition callers
// still need), there is no "no precondition" mode to support here.
func resolveLivePIDLocal(pid int, wantSessionID string) (Session, error) {
	if wantSessionID == "" {
		return Session{}, fmt.Errorf("session_id is required")
	}
	sessions, err := CollectLocal()
	if err != nil {
		return Session{}, err
	}
	for _, s := range sessions {
		if s.PID != pid {
			continue
		}
		if s.SessionID != wantSessionID {
			return Session{}, fmt.Errorf("PID %d is a different session now", pid)
		}
		return s, nil
	}
	// Tool-neutral, like resolveLivePID's own not-live message: nothing
	// resolved for this pid, so nothing here knows which store owned it.
	return Session{}, fmt.Errorf("PID %d is not a live session", pid)
}
