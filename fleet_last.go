package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// fleetLast is fleet-last.json: the last FLEET session discovery saw alive.
// POST /fleet/start resumes it, and /fleet/chat mirrors its transcript
// read-only while FLEET is down.
type fleetLast struct {
	SessionID string `json:"sessionId"`
	CWD       string `json:"cwd"`
	Tmux      string `json:"tmux"`
	SeenAt    string `json:"seenAt"` // refreshed only on a write
}

func (s *server) fleetLastPath() string {
	if s.fleet.lastPath != "" {
		return s.fleet.lastPath
	}
	dir := ConfigDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "fleet-last.json")
}

func (s *server) fleetHome() (string, error) {
	if s.fleet.home != "" {
		return s.fleet.home, nil
	}
	return os.UserHomeDir()
}

// readFleetLast reads fleet-last.json. Any read or parse error, or a session
// id that fails the resume id format check, is "no last session": the id goes
// into a shell command and a transcript glob, so it is checked before any use.
func readFleetLast(path string) (fleetLast, bool) {
	if path == "" {
		return fleetLast{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fleetLast{}, false
	}
	var l fleetLast
	if json.Unmarshal(data, &l) != nil || validateResumeSessionID(l.SessionID) != nil {
		return fleetLast{}, false
	}
	return l, true
}

// rememberFleet writes fleet-last.json when the alive FLEET's id, cwd or tmux
// name differ from the copy in memory. State polls run every few seconds, so
// an unchanged FLEET must cost nothing. The first call loads the file, so a
// server restart does not rewrite it either. A failed write is logged and not
// retried until FLEET changes again.
func (s *server) rememberFleet(sess Session) {
	if validateResumeSessionID(sess.SessionID) != nil {
		return
	}
	f := &s.fleet
	f.lastMu.Lock()
	defer f.lastMu.Unlock()
	path := s.fleetLastPath()
	if !f.lastLoaded {
		f.lastMem, _ = readFleetLast(path)
		f.lastLoaded = true
	}
	m := f.lastMem
	if m.SessionID == sess.SessionID && m.CWD == sess.CWD && m.Tmux == sess.Tmux {
		return
	}
	l := fleetLast{SessionID: sess.SessionID, CWD: sess.CWD, Tmux: sess.Tmux,
		SeenAt: s.fleetNow().UTC().Format(time.RFC3339)}
	f.lastMem = l
	if path == "" {
		return
	}
	data, err := json.Marshal(l)
	if err == nil {
		err = writeFileAtomic(path, append(data, '\n'), 0o600)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "fleet: cannot write fleet-last.json: %v\n", err)
	}
}

// fleetResumable returns the last session when POST /fleet/start would resume
// it: a valid id, a cwd that is still a directory, and a transcript on disk.
func (s *server) fleetResumable() (fleetLast, bool) {
	l, ok := readFleetLast(s.fleetLastPath())
	if !ok {
		return fleetLast{}, false
	}
	if st, err := os.Stat(l.CWD); l.CWD == "" || err != nil || !st.IsDir() {
		return fleetLast{}, false
	}
	home, err := s.fleetHome()
	if err != nil || findTranscript(home, l.SessionID) == "" {
		return fleetLast{}, false
	}
	return l, true
}
