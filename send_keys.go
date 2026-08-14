// send_keys.go
package main

import (
	"fmt"
	"os/exec"
)

// sendKeysMaxLen bounds a single compose-box message. Kept here so both the
// TUI (which can reject before paying a round trip) and the server's own
// validator (server.go's sendKeysBody) enforce the identical limit.
const sendKeysMaxLen = 4096

// sendKeys injects text into s's tmux pane as literal keystrokes followed by
// Enter — two calls, matching the existing tmuxSendLiteral (paste.go:403-405).
// The -l flag on the first call is required: without it tmux parses text as
// key names, so a message that happens to read "Enter" or "C-c" would trigger
// that tmux action instead of being typed literally. Neither call goes
// through a shell (exec.Command argument slices), so there is no
// shell-injection surface regardless of message content.
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
	if err := exec.Command("tmux", "send-keys", "-t", s.Tmux, "-l", text).Run(); err != nil {
		return fmt.Errorf("send text: %w", err)
	}
	if err := exec.Command("tmux", "send-keys", "-t", s.Tmux, "Enter").Run(); err != nil {
		return fmt.Errorf("send enter: %w", err)
	}
	return nil
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
