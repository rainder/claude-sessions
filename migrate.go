package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// MakeTmuxName generates a tmux-safe session name with a 6-char suffix to
// guarantee uniqueness against any existing session.
//
//	name   — user-set display name (preferred when present)
//	cwd    — falls back to basename of cwd
//	suffix — 6 chars of session id or random
func MakeTmuxName(cwd, suffix, name string) string {
	var base string
	switch {
	case name != "":
		base = sanitizeForTmux(name)
	case cwd != "" && cwd != "/":
		base = sanitizeForTmux(filepath.Base(strings.TrimRight(cwd, "/")))
	default:
		base = "claude"
	}
	if len(suffix) > 6 {
		suffix = suffix[:6]
	}
	if suffix == "" {
		suffix = randomSlug()
	}
	return base + "-" + suffix
}

// randomSlug returns 6 hex chars. Used as a tmux-name suffix for `cmd new`
// where there's no session id yet.
func randomSlug() string {
	b := make([]byte, 3)
	if _, err := rand.Read(b); err != nil {
		// Fallback to time-based — never panic on a non-essential helper.
		return fmt.Sprintf("%06x", time.Now().UnixNano()&0xffffff)
	}
	return hex.EncodeToString(b)
}

// MigrateLocal stops the Claude process at pid and spawns a new tmux session
// running `claude --resume <sessionId>` in the same cwd. Returns the tmux
// session name on success.
func MigrateLocal(pid int) (string, error) {
	sess, ok := readSessionByPID(pid)
	if !ok {
		return "", fmt.Errorf("no session file for PID %d", pid)
	}
	if sess.SessionID == "" || sess.CWD == "" {
		return "", fmt.Errorf("session file missing sessionId or cwd")
	}

	tname := MakeTmuxName(sess.CWD, sess.SessionID, sess.Name)

	// SIGTERM, wait up to 5s, then SIGKILL fallback.
	_ = syscall.Kill(pid, syscall.SIGTERM)
	for i := 0; i < 5; i++ {
		time.Sleep(time.Second)
		if !pidAlive(pid) {
			break
		}
	}
	if pidAlive(pid) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		time.Sleep(time.Second)
	}
	time.Sleep(time.Second) // let state flush to disk

	if err := exec.Command("tmux", "new-session", "-d", "-s", tname, "-c", sess.CWD).Run(); err != nil {
		return "", fmt.Errorf("tmux new-session: %w", err)
	}
	if err := exec.Command("tmux", "send-keys", "-t", tname,
		"claude --resume "+sess.SessionID, "Enter").Run(); err != nil {
		return "", fmt.Errorf("tmux send-keys: %w", err)
	}
	return tname, nil
}

// SpawnNew creates a fresh tmux session at cwd and sends command to it inside
// the user's shell. Returns the tmux session name.
func SpawnNew(cwd, displayName, command string) (string, error) {
	tname := MakeTmuxName(cwd, randomSlug(), displayName)
	if err := exec.Command("tmux", "new-session", "-d", "-s", tname, "-c", cwd).Run(); err != nil {
		return "", fmt.Errorf("tmux new-session: %w", err)
	}
	if err := exec.Command("tmux", "send-keys", "-t", tname, command, "Enter").Run(); err != nil {
		return "", fmt.Errorf("tmux send-keys: %w", err)
	}
	return tname, nil
}

// trustPromptMarker is unique text from Claude Code's first-run workspace
// trust dialog ("Is this a project you created or one you trust?"). A
// directory that has never been opened with Claude before shows this dialog
// and waits for a keypress before doing anything else — including consuming
// a seeded initial prompt passed as a CLI argument.
const trustPromptMarker = "Yes, I trust this folder"

// trustPromptPollInterval/trustPromptTimeout bound dismissTrustPrompt's poll
// loop. Vars rather than consts so tests can shrink them instead of eating
// the full real-time timeout on every run.
var (
	trustPromptPollInterval = 250 * time.Millisecond
	trustPromptTimeout      = 3 * time.Second
)

// dismissTrustPrompt polls the tmux pane for up to trustPromptTimeout and, if the workspace
// trust dialog appears, accepts its default "trust this folder" option so the
// session can proceed to the prompt it was launched with. It's a no-op
// (settling quickly) when the dialog never shows, e.g. because the directory
// was already trusted by an earlier session.
//
// Only the background/prompt spawn path calls this: an attached launch has
// the user right there to see and decide on the dialog themselves within a
// second or two, same as before this existed. An unattended background
// launch has nobody watching, so without this it hangs at the dialog forever
// and the seeded prompt never reaches Claude — call it in a goroutine right
// after a successful SpawnNew so the caller doesn't block waiting on it.
func dismissTrustPrompt(tname string) {
	deadline := time.Now().Add(trustPromptTimeout)
	for time.Now().Before(deadline) {
		time.Sleep(trustPromptPollInterval)
		out, err := exec.Command("tmux", "capture-pane", "-t", tname, "-p").Output()
		if err != nil {
			return // session already gone; nothing to dismiss
		}
		if strings.Contains(string(out), trustPromptMarker) {
			_ = exec.Command("tmux", "send-keys", "-t", tname, "Enter").Run()
			return
		}
	}
}

// killDeps are the side-effecting operations KillSession performs, injected so
// the kill routing can be tested without signalling a real PID or sleeping.
type killDeps struct {
	killTmux func(string) error
	signal   func(int, syscall.Signal) error
	alive    func(int) bool
	sleep    func(time.Duration)
}

// defaultKillDeps wires the production side effects.
var defaultKillDeps = killDeps{
	killTmux: func(name string) error {
		return exec.Command("tmux", "kill-session", "-t", name).Run()
	},
	signal: syscall.Kill,
	alive:  pidAlive,
	sleep:  time.Sleep,
}

// tmuxSessionName extracts the tmux session name from a "session:win.pane"
// location string. Malformed metadata (no colon, or an empty session name) is a
// hard error so callers never guess at a target.
func tmuxSessionName(tmux string) (string, error) {
	i := strings.IndexByte(tmux, ':')
	if i <= 0 {
		return "", fmt.Errorf("malformed tmux metadata %q", tmux)
	}
	return tmux[:i], nil
}

// KillSession kills the Claude session using the session's own trusted metadata
// (no live re-discovery): if s.Tmux is set, kill the whole tmux session (which
// SIGHUPs the pane process); otherwise SIGTERM the pid, escalating to SIGKILL
// after a few seconds.
func KillSession(s Session) error {
	return killSessionWith(s, defaultKillDeps)
}

func killSessionWith(s Session, deps killDeps) error {
	if s.Tmux != "" {
		name, err := tmuxSessionName(s.Tmux)
		if err != nil {
			return err
		}
		if err := deps.killTmux(name); err != nil {
			return fmt.Errorf("tmux kill-session %s: %w", name, err)
		}
		return nil
	}
	if err := deps.signal(s.PID, syscall.SIGTERM); err != nil {
		return fmt.Errorf("SIGTERM %d: %w", s.PID, err)
	}
	for i := 0; i < 5; i++ {
		deps.sleep(time.Second)
		if !deps.alive(s.PID) {
			return nil
		}
	}
	_ = deps.signal(s.PID, syscall.SIGKILL)
	deps.sleep(time.Second)
	return nil
}

// readSessionByPID reads a single ~/.claude/sessions/<pid>.json file.
// Returns ok=false if missing or malformed.
func readSessionByPID(pid int) (Session, bool) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Session{}, false
	}
	return readSessionFile(filepath.Join(home, ".claude", "sessions", strconv.Itoa(pid)+".json"))
}
