package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Session mirrors the schema Claude Code writes to ~/.claude/sessions/<pid>.json,
// plus the derived fields we compute at collection time (CPU, Tmux, Host).
type Session struct {
	PID        int    `json:"pid"`
	SessionID  string `json:"sessionId"`
	CWD        string `json:"cwd"`
	Status     string `json:"status"`
	WaitingFor string `json:"waitingFor"`
	Version    string `json:"version"`
	Name       string `json:"name"`
	UpdatedAt  int64  `json:"updatedAt"` // millis since epoch

	CPU  string `json:"cpu"`
	Tmux string `json:"tmux"` // "session:win.pane" or "" if not in tmux
	Host string `json:"-"`    // set by client when row came from a remote server

}

// StatusDisplay returns the status label including the waitingFor suffix
// when relevant (e.g. "waiting:permission prompt").
func (s Session) StatusDisplay() string {
	if s.WaitingFor != "" {
		return s.Status + ":" + s.WaitingFor
	}
	return s.Status
}

// ID is the stable identifier used for selection: <pid> for local rows,
// <host>:<pid> for remote.
func (s Session) ID() string {
	if s.Host == "" {
		return strconv.Itoa(s.PID)
	}
	return s.Host + ":" + strconv.Itoa(s.PID)
}

// Updated returns the parsed updated-at timestamp.
func (s Session) Updated() time.Time {
	return time.UnixMilli(s.UpdatedAt)
}

// CollectLocal reads every *.json under ~/.claude/sessions, filters out dead
// pids, and enriches each session with CPU% and tmux pane info.
func CollectLocal() ([]Session, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(home, ".claude", "sessions")
	matches, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}

	panes, _ := tmuxPaneMap() // best-effort: empty map if tmux not running
	ppid, cpu, err := processInfo()
	if err != nil {
		return nil, fmt.Errorf("read process tree: %w", err)
	}

	out := make([]Session, 0, len(matches))
	for _, p := range matches {
		s, ok := readSessionFile(p)
		if !ok {
			continue
		}
		if !pidAlive(s.PID) {
			continue
		}
		if c, ok := cpu[s.PID]; ok {
			s.CPU = c
		} else {
			s.CPU = "-"
		}
		s.Tmux = walkTmuxPane(s.PID, panes, ppid)
		out = append(out, s)
	}
	// Sort by cwd (case-insensitive), recency desc as tiebreaker.
	sort.SliceStable(out, func(i, j int) bool {
		ci, cj := strings.ToLower(out[i].CWD), strings.ToLower(out[j].CWD)
		if ci != cj {
			return ci < cj
		}
		return out[i].UpdatedAt > out[j].UpdatedAt
	})
	return out, nil
}

func readSessionFile(path string) (Session, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Session{}, false
	}
	var s Session
	if err := json.Unmarshal(data, &s); err != nil {
		return Session{}, false
	}
	if s.PID == 0 {
		return Session{}, false
	}
	return s, true
}

// pidAlive returns true if signal 0 to the pid succeeds (process exists and
// we have permission to signal it).
func pidAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

