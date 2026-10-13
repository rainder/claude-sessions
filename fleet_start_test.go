package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fleetSpawnCall struct{ cwd, display, command string }

// fleetStartServer is fleetTestServer plus start seams: spawns are recorded,
// never run, and the presets and default cwd are fixed.
func fleetStartServer(t *testing.T, sessions []Session) (*server, *[]fleetSpawnCall, *time.Time) {
	t.Helper()
	s := fleetTestServer(t, sessions)
	now := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	s.fleet.now = func() time.Time { return now }
	s.fleet.defaultCwd = t.TempDir()
	s.fleet.presets = func() ([]CommandPreset, error) {
		return []CommandPreset{{Name: "Claude", Command: "claude"}, {Name: "Opus", Command: "claude --model=opus --x"}}, nil
	}
	var calls []fleetSpawnCall
	s.fleet.spawn = func(cwd, display, command string) (string, error) {
		calls = append(calls, fleetSpawnCall{cwd, display, command})
		return "FLEET-abc", nil
	}
	return s, &calls, &now
}

func postFleetStart(s *server, setup func(*http.Request)) (*httptest.ResponseRecorder, fleetStartResult) {
	r := httptest.NewRequest("POST", "/fleet/start", strings.NewReader("{}"))
	if setup == nil {
		r.Header.Set("Authorization", "Bearer tok")
	} else {
		setup(r)
	}
	w := httptest.NewRecorder()
	fleetMux(s).ServeHTTP(w, r)
	var out fleetStartResult
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w, out
}

func TestFleetStartRefusesWhenFleetRunsOrIsAmbiguous(t *testing.T) {
	cases := []struct {
		name     string
		sessions []Session
		code     string
	}{
		{"alive", []Session{{PID: 1, Name: "FLEET", SessionID: "st-alive"}}, "already_running"},
		{"ambiguous", []Session{{PID: 1, Name: "FLEET"}, {PID: 2, Name: "FLEET"}}, "ambiguous"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, calls, _ := fleetStartServer(t, c.sessions)
			w, out := postFleetStart(s, nil)
			if w.Code != http.StatusConflict || out.OK || out.Code != c.code || out.Error == "" {
				t.Fatalf("%d %+v", w.Code, out)
			}
			if len(*calls) != 0 {
				t.Fatal("must not spawn")
			}
			// A refused start does not count against the rate limit.
			s.collect = func() ([]Session, error) { return nil, nil }
			if w, _ := postFleetStart(s, nil); w.Code != http.StatusOK {
				t.Fatalf("start after a 409: %d", w.Code)
			}
		})
	}
}

func TestFleetStartUsesFreshDiscovery(t *testing.T) {
	s, calls, _ := fleetStartServer(t, nil)
	getFleetJSON(t, s, "/fleet/state") // caches "not running"
	s.collect = func() ([]Session, error) { return []Session{{PID: 1, Name: "FLEET", SessionID: "st-fresh"}}, nil }
	if w, out := postFleetStart(s, nil); w.Code != http.StatusConflict || out.Code != "already_running" || len(*calls) != 0 {
		t.Fatalf("a FLEET started since the cached list must be seen: %d %+v", w.Code, out)
	}
}

func TestFleetStartListFailure(t *testing.T) {
	s, calls, _ := fleetStartServer(t, nil)
	s.collect = func() ([]Session, error) { return nil, errors.New("boom") }
	if w, out := postFleetStart(s, nil); w.Code != http.StatusInternalServerError || out.Code != "start_failed" || len(*calls) != 0 {
		t.Fatalf("%d %+v", w.Code, out)
	}
}

func TestFleetStartResumesLast(t *testing.T) {
	s, calls, _ := fleetStartServer(t, nil)
	cwd := t.TempDir()
	writeFleetLast(t, s, fleetLast{SessionID: "st-resume-1", CWD: cwd, Tmux: "old"})
	putFleetTranscript(t, s.fleet.home, "st-resume-1", `{"type":"user"}`)
	w, out := postFleetStart(s, nil)
	if w.Code != http.StatusOK || !out.OK || out.Mode != "resume" || out.Tmux != "FLEET-abc" {
		t.Fatalf("%d %+v", w.Code, out)
	}
	if len(*calls) != 1 {
		t.Fatalf("calls %v", *calls)
	}
	c := (*calls)[0]
	if c.cwd != cwd || c.display != "FLEET" || c.command != "claude --model=opus --x --resume st-resume-1 -n FLEET" {
		t.Fatalf("spawn = %+v", c)
	}
}

func TestFleetStartFresh(t *testing.T) {
	cases := []struct {
		name string
		last func(t *testing.T, s *server)
	}{
		{"no last", func(*testing.T, *server) {}},
		{"last cwd missing", func(t *testing.T, s *server) {
			writeFleetLast(t, s, fleetLast{SessionID: "st-gone-cwd", CWD: filepath.Join(t.TempDir(), "gone")})
			putFleetTranscript(t, s.fleet.home, "st-gone-cwd", `{"type":"user"}`)
		}},
		{"last transcript missing", func(t *testing.T, s *server) {
			writeFleetLast(t, s, fleetLast{SessionID: "st-no-script", CWD: t.TempDir()})
		}},
		{"last id invalid", func(t *testing.T, s *server) {
			writeFleetLast(t, s, fleetLast{SessionID: "x;touch /tmp/p", CWD: t.TempDir()})
		}},
		{"last unparseable", func(t *testing.T, s *server) {
			if err := os.MkdirAll(filepath.Dir(s.fleet.lastPath), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(s.fleet.lastPath, []byte("{"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, calls, _ := fleetStartServer(t, nil)
			c.last(t, s)
			w, out := postFleetStart(s, nil)
			if w.Code != http.StatusOK || out.Mode != "fresh" || len(*calls) != 1 {
				t.Fatalf("%d %+v %v", w.Code, out, *calls)
			}
			if got := (*calls)[0]; got.cwd != s.fleet.defaultCwd || got.command != "claude --model=opus --x -n FLEET '/fleet'" {
				t.Fatalf("spawn = %+v", got)
			}
		})
	}
}

func TestFleetStartFreshNeedsDefaultCwd(t *testing.T) {
	s, calls, _ := fleetStartServer(t, nil)
	s.fleet.defaultCwd = filepath.Join(t.TempDir(), "missing")
	if w, out := postFleetStart(s, nil); w.Code != http.StatusInternalServerError || out.Code != "start_failed" || len(*calls) != 0 {
		t.Fatalf("%d %+v", w.Code, out)
	}
}

func TestFleetStartRefusesWhenLastIDIsLiveUnderAnotherName(t *testing.T) {
	cwd := t.TempDir()
	s, calls, _ := fleetStartServer(t, []Session{{PID: 3, Name: "renamed", SessionID: "st-live-other"}})
	writeFleetLast(t, s, fleetLast{SessionID: "st-live-other", CWD: cwd})
	putFleetTranscript(t, s.fleet.home, "st-live-other", `{"type":"user"}`)
	if w, out := postFleetStart(s, nil); w.Code != http.StatusConflict || out.Code != "already_running" || len(*calls) != 0 {
		t.Fatalf("%d %+v", w.Code, out)
	}
}

func TestFleetStartPresetFallback(t *testing.T) {
	for name, presets := range map[string]func() ([]CommandPreset, error){
		"no opus preset": func() ([]CommandPreset, error) { return []CommandPreset{{Name: "Claude", Command: "claude"}}, nil },
		"load error":     func() ([]CommandPreset, error) { return nil, errors.New("bad yaml") },
	} {
		t.Run(name, func(t *testing.T) {
			s, calls, _ := fleetStartServer(t, nil)
			s.fleet.presets = presets
			postFleetStart(s, nil)
			if len(*calls) != 1 || (*calls)[0].command != "claude --model=opus -n FLEET '/fleet'" {
				t.Fatalf("calls %+v", *calls)
			}
		})
	}
}

func TestFleetStartRateLimited(t *testing.T) {
	s, calls, now := fleetStartServer(t, nil)
	if w, _ := postFleetStart(s, nil); w.Code != http.StatusOK {
		t.Fatalf("first: %d", w.Code)
	}
	*now = now.Add(10*time.Second + 300*time.Millisecond)
	w, out := postFleetStart(s, nil)
	if w.Code != http.StatusTooManyRequests || out.Code != "rate_limited" || out.RetryAfter != 50 ||
		w.Header().Get("Retry-After") != "50" || len(*calls) != 1 {
		t.Fatalf("second: %d %+v %q", w.Code, out, w.Header().Get("Retry-After"))
	}
	*now = now.Add(50 * time.Second)
	if w, _ := postFleetStart(s, nil); w.Code != http.StatusOK || len(*calls) != 2 {
		t.Fatalf("after 60 s: %d", w.Code)
	}
}

func TestFleetStartFailedSpawnStillRateLimited(t *testing.T) {
	s, _, now := fleetStartServer(t, nil)
	s.fleet.spawn = func(string, string, string) (string, error) { return "", errors.New("tmux: no server at /home/x") }
	w, out := postFleetStart(s, nil)
	if w.Code != http.StatusInternalServerError || out.Code != "start_failed" || strings.Contains(out.Error, "/home") {
		t.Fatalf("%d %+v", w.Code, out)
	}
	*now = now.Add(time.Second)
	if w, out := postFleetStart(s, nil); w.Code != http.StatusTooManyRequests || out.RetryAfter != 59 {
		t.Fatalf("after a failed spawn: %d %+v", w.Code, out)
	}
	if _, ok := getFleetJSON(t, s, "/fleet/state")["fleet"].(map[string]any)["starting"]; ok {
		t.Fatal("a failed spawn must not report a pending start")
	}
}

func TestFleetStartAuth(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*http.Request)
		code  int
	}{
		{"none", func(*http.Request) {}, http.StatusUnauthorized},
		{"cookie, no origin", func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: "cs_fleet", Value: "sec"})
		}, http.StatusForbidden},
		{"cookie, cross origin", func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: "cs_fleet", Value: "sec"})
			r.Header.Set("Origin", "http://evil.test")
		}, http.StatusForbidden},
		{"cookie, same origin", func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: "cs_fleet", Value: "sec"})
			r.Header.Set("Origin", "http://example.com")
		}, http.StatusOK},
		{"bearer, no origin", func(r *http.Request) { r.Header.Set("Authorization", "Bearer tok") }, http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, calls, _ := fleetStartServer(t, nil)
			w, _ := postFleetStart(s, c.setup)
			if w.Code != c.code || (c.code == http.StatusOK) != (len(*calls) == 1) {
				t.Fatalf("%d calls %d, want %d", w.Code, len(*calls), c.code)
			}
		})
	}
}

func TestFleetStartBodyCap(t *testing.T) {
	s, calls, _ := fleetStartServer(t, nil)
	r := httptest.NewRequest("POST", "/fleet/start", strings.NewReader(strings.Repeat("x", 5<<10)))
	r.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	fleetMux(s).ServeHTTP(w, r)
	if w.Code != http.StatusRequestEntityTooLarge || len(*calls) != 0 {
		t.Fatalf("%d", w.Code)
	}
}

func TestFleetStartWritesLogLine(t *testing.T) {
	s, _, _ := fleetStartServer(t, nil)
	cwd := t.TempDir()
	writeFleetLast(t, s, fleetLast{SessionID: "st-log-1", CWD: cwd})
	putFleetTranscript(t, s.fleet.home, "st-log-1", `{"type":"user"}`)
	postFleetStart(s, nil)
	data, err := os.ReadFile(filepath.Join(s.fleet.dir, "log.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var e map[string]any
	if err := json.Unmarshal(data, &e); err != nil {
		t.Fatal(err)
	}
	if e["type"] != "web-start" || e["at"] != "2026-10-13T10:00:00Z" || e["mode"] != "resume" ||
		e["sessionId"] != "st-log-1" || e["tmux"] != "FLEET-abc" || e["text"] == "" {
		t.Fatalf("log line = %v", e)
	}
}

func TestFleetStateStartingAndCanStart(t *testing.T) {
	s, _, now := fleetStartServer(t, nil)
	fleetBlock := func() map[string]any {
		s.invalidateSessions()
		return getFleetJSON(t, s, "/fleet/state")["fleet"].(map[string]any)
	}
	if fl := fleetBlock(); fl["canStart"] != true || fl["starting"] != nil {
		t.Fatalf("before: %v", fl)
	}
	postFleetStart(s, nil)
	fl := fleetBlock()
	st, _ := fl["starting"].(map[string]any)
	if fl["canStart"] != false || st["mode"] != "fresh" || st["at"] != "2026-10-13T10:00:00Z" {
		t.Fatalf("pending: %v", fl)
	}
	// Past the rate limit but still pending: no start button yet.
	*now = now.Add(70 * time.Second)
	if fl := fleetBlock(); fl["canStart"] != false || fl["starting"] == nil {
		t.Fatalf("at 70 s: %v", fl)
	}
	*now = now.Add(20 * time.Second)
	if fl := fleetBlock(); fl["canStart"] != true || fl["starting"] != nil {
		t.Fatalf("at 90 s: %v", fl)
	}
}

func TestFleetStateStartingClearsWhenAlive(t *testing.T) {
	s, _, _ := fleetStartServer(t, nil)
	postFleetStart(s, nil)
	s.collect = func() ([]Session, error) { return []Session{{PID: 9, Name: "FLEET", SessionID: "st-came-up"}}, nil }
	s.invalidateSessions()
	if fl := getFleetJSON(t, s, "/fleet/state")["fleet"].(map[string]any); fl["starting"] != nil || fl["canStart"] != false {
		t.Fatalf("alive: %v", fl)
	}
	// Cleared for good: FLEET going down again does not bring it back.
	s.collect = func() ([]Session, error) { return nil, nil }
	s.invalidateSessions()
	if fl := getFleetJSON(t, s, "/fleet/state")["fleet"].(map[string]any); fl["starting"] != nil {
		t.Fatalf("down again: %v", fl)
	}
}
