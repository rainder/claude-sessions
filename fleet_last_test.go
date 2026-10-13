package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fleetUUID is a session id of the shape fleet-last.json accepts. Tests use
// a distinct n each: findTranscript caches resolutions process-wide.
func fleetUUID(n int) string { return fmt.Sprintf("%08x-0000-4000-8000-%012x", n, n) }

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
	sessions := []Session{{PID: 7, Name: "FLEET", NameSource: "user", SessionID: fleetUUID(1), CWD: "/w/a", Tmux: "t-a"}}
	s := fleetTestServer(t, nil)
	s.collect = func() ([]Session, error) { return sessions, nil }
	now := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	s.fleet.now = func() time.Time { return now }

	getFleetJSON(t, s, "/fleet/state")
	got, ok := readFleetLastFile(t, s)
	if !ok || got != (fleetLast{SessionID: fleetUUID(1), CWD: "/w/a", Tmux: "t-a", SeenAt: "2026-10-13T10:00:00Z"}) {
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
	sessions = []Session{{PID: 8, Name: "FLEET", NameSource: "user", SessionID: fleetUUID(2), CWD: "/w/a", Tmux: "t-a"}}
	s.invalidateSessions()
	getFleetJSON(t, s, "/fleet/state")
	if got, _ := readFleetLastFile(t, s); got.SessionID != fleetUUID(2) || got.SeenAt != "2026-10-13T10:01:00Z" {
		t.Fatalf("after id change = %+v", got)
	}
}

func TestFleetLastNotRewrittenAfterRestart(t *testing.T) {
	s := fleetTestServer(t, []Session{{PID: 7, Name: "FLEET", SessionID: fleetUUID(3), CWD: "/w", Tmux: "t"}})
	writeFleetLast(t, s, fleetLast{SessionID: fleetUUID(3), CWD: "/w", Tmux: "t", SeenAt: "2026-01-01T00:00:00Z"})
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

func TestFleetStateCodeHasLastCanStart(t *testing.T) {
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
		{name: "not live, last", last: &fleetLast{SessionID: fleetUUID(4), CWD: cwd}, code: "not_live", hasLast: true},
		{name: "not live, bad id", last: &fleetLast{SessionID: "../x", CWD: cwd}, code: "not_live"},
		{name: "not live, cwd gone", last: &fleetLast{SessionID: fleetUUID(5), CWD: filepath.Join(cwd, "gone")}, code: "not_live"},
		{name: "not live, no transcript", last: &fleetLast{SessionID: fleetUUID(6), CWD: cwd}, noScript: true, code: "not_live"},
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
			// Nothing is starting here, so canStart is exactly "not_live".
			if fl["alive"] != c.alive || code != c.code || fl["hasLast"] != c.hasLast ||
				fl["canStart"] != (c.code == "not_live") {
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

func TestFleetLastIgnoresTmuxOnlyChange(t *testing.T) {
	sessions := []Session{{PID: 7, Name: "FLEET", SessionID: fleetUUID(20), CWD: "/w", Tmux: "t1"}}
	s := fleetTestServer(t, nil)
	s.collect = func() ([]Session, error) { return sessions, nil }
	getFleetJSON(t, s, "/fleet/state")
	if err := os.Remove(s.fleet.lastPath); err != nil {
		t.Fatal(err)
	}
	sessions = []Session{{PID: 7, Name: "FLEET", SessionID: fleetUUID(20), CWD: "/w", Tmux: "t2"}}
	s.invalidateSessions()
	getFleetJSON(t, s, "/fleet/state")
	if _, ok := readFleetLastFile(t, s); ok {
		t.Fatal("a new tmux name alone must not rewrite fleet-last.json")
	}
	sessions = []Session{{PID: 7, Name: "FLEET", SessionID: fleetUUID(20), CWD: "/w/wt", Tmux: "t2"}}
	s.invalidateSessions()
	getFleetJSON(t, s, "/fleet/state")
	if got, _ := readFleetLastFile(t, s); got.CWD != "/w/wt" || got.Tmux != "t2" {
		t.Fatalf("cwd change: %+v", got)
	}
}

func TestFleetSessionIDMustBeUUID(t *testing.T) {
	for id, want := range map[string]bool{
		"0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d":  true,
		"0A1B2C3D-4E5F-4A6B-8C7D-9E0F1A2B3C4D":  true,
		"--dangerously-skip-permissions":        false,
		"last-a":                                false,
		"0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4":   false,
		"0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4dd": false,
		"":                                      false,
	} {
		if got := validFleetSessionID(id); got != want {
			t.Errorf("%q: %v, want %v", id, got, want)
		}
	}
	s := fleetTestServer(t, []Session{{PID: 7, Name: "FLEET", SessionID: "--dangerously-skip-permissions"}})
	getFleetJSON(t, s, "/fleet/state")
	if _, ok := readFleetLastFile(t, s); ok {
		t.Fatal("a non-UUID live id must not be recorded")
	}
	writeFleetLast(t, s, fleetLast{SessionID: "--dangerously-skip-permissions", CWD: t.TempDir()})
	if _, ok := readFleetLast(s.fleet.lastPath); ok {
		t.Fatal("a non-UUID id in the file must be ignored")
	}
}

func TestFleetTranscriptMemo(t *testing.T) {
	s := fleetTestServer(t, nil)
	now := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	s.fleet.now = func() time.Time { return now }
	sid := fleetUUID(21)
	reads := 0
	real := readResumableHeadFn
	readResumableHeadFn = func(p string) (resumableHead, bool) { reads++; return real(p) }
	t.Cleanup(func() { readResumableHeadFn = real })

	if _, _, ok := s.fleetTranscript(sid); ok {
		t.Fatal("no transcript yet")
	}
	// A miss is reused for 30 s, even once the file exists.
	path := putFleetTranscript(t, s.fleet.home, sid, `{"type":"user","cwd":"/started/here"}`)
	now = now.Add(29 * time.Second)
	if _, _, ok := s.fleetTranscript(sid); ok {
		t.Fatal("a miss must be reused for 30 s")
	}
	now = now.Add(time.Second)
	got, cwd, ok := s.fleetTranscript(sid)
	if !ok || got != path || cwd != "/started/here" || reads != 1 {
		t.Fatalf("after 30 s: %q %q %v reads %d", got, cwd, ok, reads)
	}
	// A hit is reused (no new head read) for as long as the file exists.
	now = now.Add(time.Hour)
	if _, _, ok := s.fleetTranscript(sid); !ok || reads != 1 {
		t.Fatalf("hit: %v reads %d", ok, reads)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := s.fleetTranscript(sid); ok {
		t.Fatal("a removed transcript must stop resolving")
	}
}
