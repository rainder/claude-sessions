package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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

// fleetSessionIDRe is the only id shape fleet-last.json may hold: a Claude
// Code session UUID. The resume id check alone (resumeSessionIDRe) admits
// "--dangerously-skip-permissions", which would turn into a flag in the
// command a start types.
var fleetSessionIDRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func validFleetSessionID(id string) bool {
	return fleetSessionIDRe.MatchString(id) && validateResumeSessionID(id) == nil
}

// readFleetLast reads fleet-last.json. Any read or parse error, or a session
// id that is not a UUID, is "no last session": the id goes into a shell
// command and a transcript glob, so it is checked before any use.
func readFleetLast(path string) (fleetLast, bool) {
	if path == "" {
		return fleetLast{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fleetLast{}, false
	}
	var l fleetLast
	if json.Unmarshal(data, &l) != nil || !validFleetSessionID(l.SessionID) {
		return fleetLast{}, false
	}
	return l, true
}

// rememberFleet writes fleet-last.json when the alive FLEET's id or cwd
// differ from the copy in memory (a new tmux name alone is not worth a
// write; it is stored with the next one). State polls run every few seconds,
// so an unchanged FLEET must cost nothing. The first call loads the file, so
// a server restart does not rewrite it either. A failed write is logged and
// not retried until FLEET changes again.
func (s *server) rememberFleet(sess Session) {
	if !validFleetSessionID(sess.SessionID) {
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
	if m.SessionID == sess.SessionID && m.CWD == sess.CWD {
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

// fleetScriptMissTTL: how long a "no transcript" answer is reused. While
// FLEET is down every state and chat poll asks, and a miss is a glob over
// every project directory.
const fleetScriptMissTTL = 30 * time.Second

// fleetScript is one memoized transcript lookup: the path and the cwd its
// head records (the cwd the session started in), or a miss and when.
type fleetScript struct {
	path, cwd string
	missAt    time.Time
}

// fleetTranscript finds sid's transcript and the cwd its head records. A hit
// is reused until the file disappears; a miss for fleetScriptMissTTL.
func (s *server) fleetTranscript(sid string) (path, cwd string, ok bool) {
	f := &s.fleet
	now := s.fleetNow()
	f.scriptMu.Lock()
	c, hit := f.scripts[sid]
	f.scriptMu.Unlock()
	if hit {
		if c.path != "" {
			if _, err := os.Stat(c.path); err == nil {
				return c.path, c.cwd, true
			}
		} else if now.Sub(c.missAt) < fleetScriptMissTTL {
			return "", "", false
		}
	}
	c = fleetScript{missAt: now}
	if home, err := s.fleetHome(); err == nil {
		if p := findTranscript(home, sid); p != "" {
			head, _ := readResumableHeadFn(p)
			c = fleetScript{path: p, cwd: head.cwd}
		}
	}
	f.scriptMu.Lock()
	if f.scripts == nil {
		f.scripts = map[string]fleetScript{}
	}
	f.scripts[sid] = c
	f.scriptMu.Unlock()
	return c.path, c.cwd, c.path != ""
}

// fleetLastPlan is what POST /fleet/start would do with fleet-last.json.
// valid: the file names a usable id. ok: it can be resumed — a transcript
// exists and its folder is still a directory. cwd is the folder the session
// started in, from the transcript head: claude keys a transcript to that
// folder, and the session's own cwd moves when it enters a worktree.
// fleet-last.json's cwd is only the fallback for a head that has none.
func (s *server) fleetLastPlan() (l fleetLast, cwd string, valid, ok bool) {
	l, valid = readFleetLast(s.fleetLastPath())
	if !valid {
		return fleetLast{}, "", false, false
	}
	_, cwd, found := s.fleetTranscript(l.SessionID)
	if !found {
		return l, "", true, false
	}
	if cwd == "" {
		cwd = l.CWD
	}
	if st, err := os.Stat(cwd); cwd == "" || err != nil || !st.IsDir() {
		return l, "", true, false
	}
	return l, cwd, true, true
}
