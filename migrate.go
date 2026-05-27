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

// SpawnNew creates a fresh tmux session with `claude` running inside the
// user's shell at cwd. Returns the tmux session name.
func SpawnNew(cwd, displayName string) (string, error) {
	tname := MakeTmuxName(cwd, randomSlug(), displayName)
	if err := exec.Command("tmux", "new-session", "-d", "-s", tname, "-c", cwd).Run(); err != nil {
		return "", fmt.Errorf("tmux new-session: %w", err)
	}
	if err := exec.Command("tmux", "send-keys", "-t", tname, "claude", "Enter").Run(); err != nil {
		return "", fmt.Errorf("tmux send-keys: %w", err)
	}
	return tname, nil
}

// KillSession kills the Claude session, tmux-aware: if pid is in a tmux pane,
// kill the whole tmux session (which SIGHUPs the pane process). Otherwise
// SIGTERM the pid directly, escalating to SIGKILL after a few seconds.
func KillSession(pid int) error {
	panes, _ := tmuxPaneMap()
	ppid, _ := ppidMap()
	loc := walkTmuxPane(pid, panes, ppid)
	if loc != "" {
		sessName := strings.SplitN(loc, ":", 2)[0]
		if err := exec.Command("tmux", "kill-session", "-t", sessName).Run(); err != nil {
			return fmt.Errorf("tmux kill-session %s: %w", sessName, err)
		}
		return nil
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return fmt.Errorf("SIGTERM %d: %w", pid, err)
	}
	for i := 0; i < 5; i++ {
		time.Sleep(time.Second)
		if !pidAlive(pid) {
			return nil
		}
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	time.Sleep(time.Second)
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
