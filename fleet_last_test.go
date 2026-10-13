package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// putFleetTranscript makes home/.claude/projects/<dir>/<sid>.jsonl hold lines
// and returns its path. Session ids must be unique per test: findTranscript
// caches resolutions process-wide.
func putFleetTranscript(t *testing.T, home, sid string, lines ...string) string {
	t.Helper()
	dir := filepath.Join(home, ".claude", "projects", "-proj")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, sid+".jsonl")
	body := ""
	for _, l := range lines {
		body += l + "\n"
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func writeFleetLast(t *testing.T, s *server, l fleetLast) {
	t.Helper()
	b, err := json.Marshal(l)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(s.fleet.lastPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.fleet.lastPath, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFleetLastFile(t *testing.T, s *server) (fleetLast, bool) {
	t.Helper()
	data, err := os.ReadFile(s.fleet.lastPath)
	if os.IsNotExist(err) {
		return fleetLast{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	var l fleetLast
	if err := json.Unmarshal(data, &l); err != nil {
		t.Fatal(err)
	}
	return l, true
}

func TestFleetLastWrittenOnlyWhenFleetChanges(t *testing.T) {
	sessions := []Session{{PID: 7, Name: "FLEET", NameSource: "user", SessionID: "last-a", CWD: "/w/a", Tmux: "t-a"}}
	s := fleetTestServer(t, nil)
	s.collect = func() ([]Session, error) { return sessions, nil }
	now := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	s.fleet.now = func() time.Time { return now }

	getFleetJSON(t, s, "/fleet/state")
	got, ok := readFleetLastFile(t, s)
	if !ok || got != (fleetLast{SessionID: "last-a", CWD: "/w/a", Tmux: "t-a", SeenAt: "2026-10-13T10:00:00Z"}) {
		t.Fatalf("first write = %+v %v", got, ok)
	}
	st, err := os.Stat(s.fleet.lastPath)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v %v, want 0600", st.Mode(), err)
	}

	// Unchanged FLEET: no write, so a removed file stays removed.
	if err := os.Remove(s.fleet.lastPath); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	s.invalidateSessions()
	getFleetJSON(t, s, "/fleet/state")
	if _, ok := readFleetLastFile(t, s); ok {
		t.Fatal("unchanged FLEET must not rewrite fleet-last.json")
	}

	// A new id (FLEET restarted) is written, with a new seenAt.
	sessions = []Session{{PID: 8, Name: "FLEET", NameSource: "user", SessionID: "last-b", CWD: "/w/a", Tmux: "t-a"}}
	s.invalidateSessions()
	getFleetJSON(t, s, "/fleet/state")
	if got, _ := readFleetLastFile(t, s); got.SessionID != "last-b" || got.SeenAt != "2026-10-13T10:01:00Z" {
		t.Fatalf("after id change = %+v", got)
	}
}

func TestFleetLastNotRewrittenAfterRestart(t *testing.T) {
	s := fleetTestServer(t, []Session{{PID: 7, Name: "FLEET", SessionID: "last-c", CWD: "/w", Tmux: "t"}})
	writeFleetLast(t, s, fleetLast{SessionID: "last-c", CWD: "/w", Tmux: "t", SeenAt: "2026-01-01T00:00:00Z"})
	getFleetJSON(t, s, "/fleet/state")
	if got, _ := readFleetLastFile(t, s); got.SeenAt != "2026-01-01T00:00:00Z" {
		t.Fatalf("a fresh process must not rewrite an unchanged file: %+v", got)
	}
}

func TestFleetLastSkipsInvalidLiveID(t *testing.T) {
	s := fleetTestServer(t, []Session{{PID: 7, Name: "FLEET", SessionID: "bad id;rm", CWD: "/w"}})
	getFleetJSON(t, s, "/fleet/state")
	if _, ok := readFleetLastFile(t, s); ok {
		t.Fatal("an id that fails the resume format must not be recorded")
	}
}

func TestFleetStateCodeAndHasLast(t *testing.T) {
	cwd := t.TempDir()
	cases := []struct {
		name     string
		sessions []Session
		last     *fleetLast
		noScript bool // transcript missing
		code     string
		alive    bool
		hasLast  bool
	}{
		{name: "alive", sessions: []Session{{PID: 1, Name: "FLEET", SessionID: "hl-alive"}}, alive: true},
		{name: "not live, no last", code: "not_live"},
		{name: "not live, last", last: &fleetLast{SessionID: "hl-ok", CWD: cwd}, code: "not_live", hasLast: true},
		{name: "not live, bad id", last: &fleetLast{SessionID: "../x", CWD: cwd}, code: "not_live"},
		{name: "not live, cwd gone", last: &fleetLast{SessionID: "hl-nocwd", CWD: filepath.Join(cwd, "gone")}, code: "not_live"},
		{name: "not live, no transcript", last: &fleetLast{SessionID: "hl-noscript", CWD: cwd}, noScript: true, code: "not_live"},
		{name: "ambiguous", sessions: []Session{{PID: 1, Name: "FLEET"}, {PID: 2, Name: "FLEET"}}, code: "ambiguous"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := fleetTestServer(t, c.sessions)
			if c.last != nil {
				writeFleetLast(t, s, *c.last)
				if !c.noScript {
					putFleetTranscript(t, s.fleet.home, c.last.SessionID, `{"type":"user"}`)
				}
			}
			fl := getFleetJSON(t, s, "/fleet/state")["fleet"].(map[string]any)
			code, _ := fl["code"].(string)
			if fl["alive"] != c.alive || code != c.code || fl["hasLast"] != c.hasLast {
				t.Fatalf("fleet = %v", fl)
			}
		})
	}
}

func TestFleetStateListFailedCode(t *testing.T) {
	s := fleetTestServer(t, nil)
	s.collect = func() ([]Session, error) { return nil, os.ErrPermission }
	fl := getFleetJSON(t, s, "/fleet/state")["fleet"].(map[string]any)
	if fl["code"] != "list_failed" || fl["reason"] != "session list failed" {
		t.Fatalf("fleet = %v", fl)
	}
}
