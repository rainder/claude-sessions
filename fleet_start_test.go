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
	writeFleetLast(t, s, fleetLast{SessionID: fleetUUID(7), CWD: cwd, Tmux: "old"})
	putFleetTranscript(t, s.fleet.home, fleetUUID(7), `{"type":"user"}`)
	w, out := postFleetStart(s, nil)
	if w.Code != http.StatusOK || !out.OK || out.Mode != "resume" || out.Tmux != "FLEET-abc" {
		t.Fatalf("%d %+v", w.Code, out)
	}
	if len(*calls) != 1 {
		t.Fatalf("calls %v", *calls)
	}
	c := (*calls)[0]
	if c.cwd != cwd || c.display != "FLEET" || c.command != "claude --model=opus --x --resume "+fleetUUID(7)+" -n FLEET" {
		t.Fatalf("spawn = %+v", c)
	}
}

func TestFleetStartFresh(t *testing.T) {
	cases := []struct {
		name        string
		lastMissing bool
		last        func(t *testing.T, s *server)
	}{
		{"no last", false, func(*testing.T, *server) {}},
		{"last cwd missing", true, func(t *testing.T, s *server) {
			writeFleetLast(t, s, fleetLast{SessionID: fleetUUID(8), CWD: filepath.Join(t.TempDir(), "gone")})
			putFleetTranscript(t, s.fleet.home, fleetUUID(8), `{"type":"user"}`)
		}},
		{"last transcript missing", true, func(t *testing.T, s *server) {
			writeFleetLast(t, s, fleetLast{SessionID: fleetUUID(9), CWD: t.TempDir()})
		}},
		{"head cwd missing", true, func(t *testing.T, s *server) {
			// The transcript's own cwd wins over fleet-last.json's, even
			// when only the latter still exists.
			writeFleetLast(t, s, fleetLast{SessionID: fleetUUID(15), CWD: t.TempDir()})
			putFleetTranscript(t, s.fleet.home, fleetUUID(15), `{"type":"user","cwd":"`+filepath.Join(t.TempDir(), "gone")+`"}`)
		}},
		{"last id invalid", false, func(t *testing.T, s *server) {
			writeFleetLast(t, s, fleetLast{SessionID: "x;touch /tmp/p", CWD: t.TempDir()})
		}},
		{"last id is a flag", false, func(t *testing.T, s *server) {
			writeFleetLast(t, s, fleetLast{SessionID: "--dangerously-skip-permissions", CWD: t.TempDir()})
			putFleetTranscript(t, s.fleet.home, "--dangerously-skip-permissions", `{"type":"user"}`)
		}},
		{"last unparseable", false, func(t *testing.T, s *server) {
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
			if w.Code != http.StatusOK || out.Mode != "fresh" || out.LastMissing != c.lastMissing || len(*calls) != 1 {
				t.Fatalf("%d %+v %v", w.Code, out, *calls)
			}
			wantText := "Started a new FLEET session"
			if c.lastMissing {
				wantText += " (the last one could not be resumed)"
			}
			if e := lastFleetLogLine(t, s); e["text"] != wantText {
				t.Fatalf("log text = %v", e["text"])
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
	s, calls, _ := fleetStartServer(t, []Session{{PID: 3, Name: "renamed", SessionID: fleetUUID(10)}})
	writeFleetLast(t, s, fleetLast{SessionID: fleetUUID(10), CWD: cwd})
	putFleetTranscript(t, s.fleet.home, fleetUUID(10), `{"type":"user"}`)
	if w, out := postFleetStart(s, nil); w.Code != http.StatusConflict || out.Code != "last_live" ||
		out.Error != "the last FLEET session is running without the FLEET name" || len(*calls) != 0 {
		t.Fatalf("%d %+v", w.Code, out)
	}
	fl := getFleetJSON(t, s, "/fleet/state")["fleet"].(map[string]any)
	if fl["code"] != "not_live" || fl["lastLive"] != true || fl["canStart"] != false {
		t.Fatalf("state: %v", fl)
	}
}

func TestFleetStartResumesInTheTranscriptCwd(t *testing.T) {
	s, calls, _ := fleetStartServer(t, nil)
	started, moved := t.TempDir(), t.TempDir()
	// Session.CWD went into a worktree; the transcript is keyed to where the
	// session started.
	writeFleetLast(t, s, fleetLast{SessionID: fleetUUID(16), CWD: moved})
	putFleetTranscript(t, s.fleet.home, fleetUUID(16), `{"type":"summary","summary":"x"}`, `{"type":"user","cwd":"`+started+`"}`)
	if w, out := postFleetStart(s, nil); w.Code != http.StatusOK || out.Mode != "resume" || out.LastMissing {
		t.Fatalf("%d %+v", w.Code, out)
	}
	if got := (*calls)[0].cwd; got != started {
		t.Fatalf("cwd = %q, want the transcript's %q", got, started)
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
	*now = now.Add(80 * time.Second) // past the 60 s and the 90 s pending window
	if w, _ := postFleetStart(s, nil); w.Code != http.StatusOK || len(*calls) != 2 {
		t.Fatalf("after 90 s: %d", w.Code)
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
		{"none", func(*http.Request) {}, http.StatusUnauthorized}, // JSON, code unauthorized
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
	writeFleetLast(t, s, fleetLast{SessionID: fleetUUID(11), CWD: cwd})
	putFleetTranscript(t, s.fleet.home, fleetUUID(11), `{"type":"user"}`)
	postFleetStart(s, nil)
	e := lastFleetLogLine(t, s)
	if e["type"] != "web-start" || e["at"] != "2026-10-13T10:00:00Z" || e["mode"] != "resume" ||
		e["sessionId"] != fleetUUID(11) || e["tmux"] != "FLEET-abc" || e["text"] == "" {
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

// lastFleetLogLine is the newest line of the test server's log.jsonl.
func lastFleetLogLine(t *testing.T, s *server) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(s.fleet.dir, "log.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	var e map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &e); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestFleetStartUnauthorizedIsJSON(t *testing.T) {
	s, _, _ := fleetStartServer(t, nil)
	w, out := postFleetStart(s, func(*http.Request) {})
	if w.Code != http.StatusUnauthorized || out.OK || out.Code != "unauthorized" || out.Error == "" ||
		!strings.Contains(w.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("%d %q %+v", w.Code, w.Header().Get("Content-Type"), out)
	}
}

func TestFleetStartRefusesWhileAStartIsPending(t *testing.T) {
	cases := []struct {
		name   string
		cmd    string // pane command once FLEET has not appeared; "" = session gone
		ending bool   // that state ends the pending start
	}{
		{"claude still running", "claude", false},
		{"node (claude) still running", "node", false},
		{"claude exited to zsh", "zsh", true},
		{"claude exited to bash", "bash", true},
		{"login shell", "-fish", true},
		{"sh", "sh", true},
		{"dash", "dash", true},
		{"session gone", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, calls, now := fleetStartServer(t, nil)
			var asked []string
			s.fleet.paneCommand = func(name string) (string, bool) {
				asked = append(asked, name)
				return c.cmd, c.cmd != ""
			}
			fleetBlock := func() map[string]any {
				s.invalidateSessions()
				return getFleetJSON(t, s, "/fleet/state")["fleet"].(map[string]any)
			}
			if w, _ := postFleetStart(s, nil); w.Code != http.StatusOK {
				t.Fatalf("first: %d", w.Code)
			}
			// Inside the 60 s gap and the 90 s window: pending, with the tmux
			// name, and no retryAfter (the gap is not the only block).
			*now = now.Add(10 * time.Second)
			fl := fleetBlock()
			if fl["pending"] != true || fl["pendingTmux"] != "FLEET-abc" || fl["canStart"] != false ||
				fl["starting"] == nil || fl["retryAfter"] != nil {
				t.Fatalf("at 10 s: %v", fl)
			}
			if len(asked) != 0 {
				t.Fatalf("tmux asked inside the window: %v", asked)
			}
			// Past the 60 s gap, inside the 90 s window: always pending.
			*now = now.Add(60 * time.Second)
			if w, out := postFleetStart(s, nil); w.Code != http.StatusConflict || out.Code != "starting" || len(*calls) != 1 {
				t.Fatalf("in the window: %d %+v", w.Code, out)
			}
			// Past the window: the pane decides.
			*now = now.Add(30 * time.Second)
			fl = fleetBlock()
			if len(asked) == 0 || asked[len(asked)-1] != "FLEET-abc" {
				t.Fatalf("tmux asked %v, want the started tmux name", asked)
			}
			if c.ending {
				if fl["pending"] != false || fl["pendingTmux"] != nil || fl["canStart"] != true {
					t.Fatalf("after the window: %v", fl)
				}
				if w, _ := postFleetStart(s, nil); w.Code != http.StatusOK || len(*calls) != 2 {
					t.Fatalf("start after it ended: %d", w.Code)
				}
				return
			}
			if fl["pending"] != true || fl["pendingTmux"] != "FLEET-abc" || fl["canStart"] != false || fl["starting"] != nil {
				t.Fatalf("after the window: %v", fl)
			}
			if w, out := postFleetStart(s, nil); w.Code != http.StatusConflict || out.Code != "starting" || len(*calls) != 1 {
				t.Fatalf("pane still runs claude: %d %+v", w.Code, out)
			}
		})
	}
}

func TestFleetStateRetryAfterOnlyForTheGap(t *testing.T) {
	s, _, now := fleetStartServer(t, nil)
	s.fleet.spawn = func(string, string, string) (string, error) { return "", errors.New("no tmux") }
	postFleetStart(s, nil) // failed: no pending start, but the gap runs
	*now = now.Add(20*time.Second + 200*time.Millisecond)
	fl := getFleetJSON(t, s, "/fleet/state")["fleet"].(map[string]any)
	if fl["canStart"] != false || fl["pending"] != false || fl["retryAfter"] != float64(40) {
		t.Fatalf("in the gap: %v", fl)
	}
	*now = now.Add(40 * time.Second)
	s.invalidateSessions()
	fl = getFleetJSON(t, s, "/fleet/state")["fleet"].(map[string]any)
	if fl["canStart"] != true || fl["retryAfter"] != nil {
		t.Fatalf("after the gap: %v", fl)
	}
	// Not offered at all when FLEET is ambiguous, gap or not.
	*now = now.Add(-30 * time.Second)
	s.collect = func() ([]Session, error) { return []Session{{PID: 1, Name: "FLEET"}, {PID: 2, Name: "FLEET"}}, nil }
	s.invalidateSessions()
	fl = getFleetJSON(t, s, "/fleet/state")["fleet"].(map[string]any)
	if fl["canStart"] != false || fl["retryAfter"] != nil {
		t.Fatalf("ambiguous: %v", fl)
	}
}

func TestFleetStateLastLiveUsesDiscoveryList(t *testing.T) {
	s, _, _ := fleetStartServer(t, nil)
	writeFleetLast(t, s, fleetLast{SessionID: fleetUUID(22), CWD: t.TempDir()})
	n := 0
	s.collect = func() ([]Session, error) {
		n++
		if n == 1 { // discovery's list holds the last id under another name
			return []Session{{PID: 3, Name: "other", SessionID: fleetUUID(22)}}, nil
		}
		return nil, nil
	}
	fl := getFleetJSON(t, s, "/fleet/state")["fleet"].(map[string]any)
	if fl["lastLive"] != true || n != 1 {
		t.Fatalf("lastLive %v after %d collections", fl["lastLive"], n)
	}
}

func TestFleetStartPendingEndsWhenFleetAppears(t *testing.T) {
	s, _, now := fleetStartServer(t, nil)
	s.fleet.paneCommand = func(string) (string, bool) { return "claude", true }
	postFleetStart(s, nil)
	s.collect = func() ([]Session, error) { return []Session{{PID: 9, Name: "FLEET", SessionID: fleetUUID(17)}}, nil }
	s.invalidateSessions()
	getFleetJSON(t, s, "/fleet/state") // sees FLEET: the start is done
	s.collect = func() ([]Session, error) { return nil, nil }
	*now = now.Add(61 * time.Second)
	if w, out := postFleetStart(s, nil); w.Code != http.StatusOK {
		t.Fatalf("FLEET came up and went down again: %d %+v", w.Code, out)
	}
}
