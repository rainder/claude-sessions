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

const (
	emptyBoxCapture = "out\n────────────────────\n❯ \n────────────────────\n status\n"
	dialogCapture   = "────────────────────\n Do you want to proceed?\n ❯ 1. Yes\n   2. No\n"
)

func boxCapture(text string) string {
	return "out\n────────────────────\n❯ " + text + "\n────────────────────\n status\n"
}

type sayRig struct {
	s        *server
	typed    []string
	enters   int
	now      time.Time
	captures []string // consumed in order; the last one repeats
}

func newSayRig(t *testing.T, sessions []Session) *sayRig {
	rig := &sayRig{now: time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)}
	rig.s = fleetTestServer(t, sessions)
	rig.s.fleet.now = func() time.Time { return rig.now }
	rig.s.fleet.sleep = func(time.Duration) {}
	rig.s.fleet.capture = func(string) (string, error) {
		// Before typing: empty box. After typing: the box holds text.
		if len(rig.captures) > 0 {
			c := rig.captures[0]
			if len(rig.captures) > 1 {
				rig.captures = rig.captures[1:]
			}
			return c, nil
		}
		if len(rig.typed) > rig.enters {
			return boxCapture(rig.typed[len(rig.typed)-1]), nil
		}
		return emptyBoxCapture, nil
	}
	rig.s.fleet.typeText = func(_ string, text string) error { rig.typed = append(rig.typed, text); return nil }
	rig.s.fleet.pressEnter = func(string) error { rig.enters++; return nil }
	rig.s.fleet.paneInMode = func(string) (bool, error) { return false, nil }
	return rig
}

func fleetRow() Session {
	return Session{PID: 7, Name: "FLEET", SessionID: "sid", Status: "shell", Tmux: "fleet:0.0"}
}

func (rig *sayRig) post(t *testing.T, body string, mod func(*http.Request)) (int, fleetSayResult) {
	t.Helper()
	r := httptest.NewRequest("POST", "http://ws.ts.net:8765/fleet/say", strings.NewReader(body))
	r.AddCookie(&http.Cookie{Name: "cs_fleet", Value: "sec"})
	r.Header.Set("Origin", "http://ws.ts.net:8765")
	if mod != nil {
		mod(r)
	}
	w := httptest.NewRecorder()
	fleetMux(rig.s).ServeHTTP(w, r)
	var res fleetSayResult
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	return w.Code, res
}

func TestFleetSaySendsExactTextAndLogs(t *testing.T) {
	rig := newSayRig(t, []Session{fleetRow()})
	code, res := rig.post(t, `{"text":"Andy decides: DR-1 — yes (explicit).","cardId":"dr-1"}`, nil)
	if code != 200 || !res.OK || res.Queued {
		t.Fatalf("%d %+v", code, res)
	}
	if len(rig.typed) != 1 || rig.typed[0] != "Andy decides: DR-1 — yes (explicit)." || rig.enters != 1 {
		t.Fatalf("typed %q enters %d", rig.typed, rig.enters)
	}
	data, _ := os.ReadFile(filepath.Join(rig.s.fleet.dir, "log.jsonl"))
	if !strings.Contains(string(data), `"type":"web-say"`) || !strings.Contains(string(data), `"cardId":"dr-1"`) {
		t.Fatalf("log %q", data)
	}
}

func TestFleetSayQueuedWhenBusy(t *testing.T) {
	row := fleetRow()
	row.Status = "busy"
	rig := newSayRig(t, []Session{row})
	if _, res := rig.post(t, `{"text":"hi"}`, nil); !res.OK || !res.Queued {
		t.Fatalf("%+v", res)
	}
}

func TestFleetSayFlattensNewlinesAndRefusesControl(t *testing.T) {
	rig := newSayRig(t, []Session{fleetRow()})
	if _, res := rig.post(t, `{"text":"line one\r\nline two\nthree"}`, nil); !res.OK {
		t.Fatalf("%+v", res)
	}
	if rig.typed[0] != "line one line two three" {
		t.Fatalf("typed %q", rig.typed[0])
	}
	rig.now = rig.now.Add(time.Minute)
	if code, _ := rig.post(t, `{"text":"esc \u001b[2J"}`, nil); code != http.StatusBadRequest {
		t.Fatalf("ESC must be 400, got %d", code)
	}
	if code, _ := rig.post(t, `{"text":"   "}`, nil); code != http.StatusBadRequest {
		t.Fatalf("blank must be 400, got %d", code)
	}
	if code, _ := rig.post(t, `{"text":"`+strings.Repeat("a", 4097)+`"}`, nil); code != http.StatusBadRequest {
		t.Fatalf("4097 bytes must be 400, got %d", code)
	}
	if len(rig.typed) != 1 {
		t.Fatalf("nothing else may be typed: %q", rig.typed)
	}
}

func TestFleetSayStrictBody(t *testing.T) {
	rig := newSayRig(t, []Session{fleetRow()})
	for _, b := range []string{`{"text":"a","extra":1}`, `{"text":"a"} x`, `null`, `[]`} {
		if code, _ := rig.post(t, b, nil); code != http.StatusBadRequest {
			t.Errorf("%s: %d", b, code)
		}
	}
}

func TestFleetSayNotAliveAmbiguousAndNoSessionID(t *testing.T) {
	rig := newSayRig(t, nil)
	if _, res := rig.post(t, `{"text":"hi"}`, nil); res.OK || res.Code != "not_live" {
		t.Fatalf("%+v", res)
	}
	rig = newSayRig(t, []Session{fleetRow(), {PID: 8, Name: "FLEET", SessionID: "x", Tmux: "b:0.0"}})
	if _, res := rig.post(t, `{"text":"hi"}`, nil); res.OK || res.Code != "ambiguous" {
		t.Fatalf("%+v", res)
	}
	row := fleetRow()
	row.SessionID = ""
	rig = newSayRig(t, []Session{row})
	if _, res := rig.post(t, `{"text":"hi"}`, nil); res.OK || res.Code != "not_live" {
		t.Fatalf("%+v", res)
	}
	if len(rig.typed) != 0 {
		t.Fatal("nothing may be typed")
	}
}

// collectPhases returns the cached-discovery list on the first collect and
// the fresh list on every later one (resolveLivePID collects directly).
func collectPhases(first, fresh []Session) func() ([]Session, error) {
	calls := 0
	return func() ([]Session, error) {
		calls++
		if calls == 1 {
			return first, nil
		}
		return fresh, nil
	}
}

func TestFleetSayRefusesRecycledPid(t *testing.T) {
	rig := newSayRig(t, nil)
	rig.s.collect = collectPhases([]Session{fleetRow()},
		[]Session{{PID: 7, Name: "other", SessionID: "new", Tmux: "x:0.0"}})
	if _, res := rig.post(t, `{"text":"hi"}`, nil); res.OK || res.Code != codeSessionMismatch {
		t.Fatalf("%+v", res)
	}
	if len(rig.typed) != 0 {
		t.Fatal("nothing may be typed into a recycled pid")
	}
}

func TestFleetSayRefusesWhenFreshListDisagrees(t *testing.T) {
	second := Session{PID: 9, Name: "FLEET", SessionID: "other", Tmux: "y:0.0"}
	renamed := fleetRow()
	renamed.Name = "not fleet any more"
	for name, fresh := range map[string][]Session{
		"second FLEET appeared": {fleetRow(), second},
		"renamed":               {renamed},
	} {
		rig := newSayRig(t, nil)
		rig.s.collect = collectPhases([]Session{fleetRow()}, fresh)
		if _, res := rig.post(t, `{"text":"hi"}`, nil); res.OK || res.Code != "fleet_changed" {
			t.Errorf("%s: %+v", name, res)
		}
		if len(rig.typed) != 0 {
			t.Errorf("%s: typed %q", name, rig.typed)
		}
	}
}

func TestFleetSayRefusesDialog(t *testing.T) {
	row := fleetRow()
	row.Status, row.WaitingFor = "busy", "permission prompt"
	rig := newSayRig(t, []Session{row})
	if _, res := rig.post(t, `{"text":"hi"}`, nil); res.OK || res.Code != "dialog" {
		t.Fatalf("%+v", res)
	}
	rig = newSayRig(t, []Session{fleetRow()})
	rig.captures = []string{dialogCapture}
	if _, res := rig.post(t, `{"text":"hi"}`, nil); res.OK || res.Code != "dialog" {
		t.Fatalf("%+v", res)
	}
	if len(rig.typed) != 0 {
		t.Fatal("nothing may be typed into a dialog")
	}
}

func TestFleetSayNoEnterWhenDialogOpensAfterTyping(t *testing.T) {
	rig := newSayRig(t, []Session{fleetRow()})
	rig.captures = []string{emptyBoxCapture, dialogCapture}
	_, res := rig.post(t, `{"text":"hi"}`, nil)
	if res.OK || res.Code != "not_submitted" {
		t.Fatalf("%+v", res)
	}
	if len(rig.typed) != 1 || rig.enters != 0 {
		t.Fatalf("typed %q enters %d: Enter must not be pressed into a dialog", rig.typed, rig.enters)
	}
	data, _ := os.ReadFile(filepath.Join(rig.s.fleet.dir, "log.jsonl"))
	if !strings.Contains(string(data), "not submitted") || strings.Contains(string(data), `"text":"hi"`) {
		t.Fatalf("log %q", data)
	}
}

func TestFleetSayNoEnterWhenTextNeverLands(t *testing.T) {
	rig := newSayRig(t, []Session{fleetRow()})
	rig.captures = []string{emptyBoxCapture} // repeats: the box stays empty
	if _, res := rig.post(t, `{"text":"hi"}`, nil); res.Code != "not_submitted" || rig.enters != 0 {
		t.Fatalf("%+v enters %d", res, rig.enters)
	}
}

func TestFleetSayRefusesNonEmptyInput(t *testing.T) {
	rig := newSayRig(t, []Session{fleetRow()})
	rig.captures = []string{"────────────────────\n❯ half typed\n────────────────────\n"}
	if _, res := rig.post(t, `{"text":"hi"}`, nil); res.OK || res.Code != "input_busy" || res.Error != "input box not empty" {
		t.Fatalf("%+v", res)
	}
}

func TestFleetSaySendFailure(t *testing.T) {
	rig := newSayRig(t, []Session{fleetRow()})
	rig.s.fleet.typeText = func(string, string) error { return errors.New("tmux gone") }
	if _, res := rig.post(t, `{"text":"hi"}`, nil); res.OK || res.Code != "send_failed" {
		t.Fatalf("%+v", res)
	}
}

func TestFleetSayRateLimitAndDoubleTap(t *testing.T) {
	rig := newSayRig(t, []Session{fleetRow()})
	rig.post(t, `{"text":"A"}`, nil)
	rig.now = rig.now.Add(500 * time.Millisecond)
	if _, res := rig.post(t, `{"text":"B"}`, nil); res.Code != "rate_limited" {
		t.Fatalf("%+v", res)
	}
	rig.now = rig.now.Add(1500 * time.Millisecond) // t=2s
	if _, res := rig.post(t, `{"text":"B"}`, nil); !res.OK {
		t.Fatalf("B at 2s: %+v", res)
	}
	rig.now = rig.now.Add(2 * time.Second) // t=4s: A, B, A
	if _, res := rig.post(t, `{"text":"A"}`, nil); res.Code != "double_tap" {
		t.Fatalf("A again at 4s must be a double tap: %+v", res)
	}
	rig.now = rig.now.Add(7 * time.Second) // t=11s
	if _, res := rig.post(t, `{"text":"A"}`, nil); !res.OK {
		t.Fatalf("after 10 s the same text is allowed: %+v", res)
	}
}

func TestFleetSayStampsAtCompletion(t *testing.T) {
	rig := newSayRig(t, []Session{fleetRow()})
	rig.s.fleet.pressEnter = func(string) error {
		rig.enters++
		rig.now = rig.now.Add(3 * time.Second) // slow send
		return nil
	}
	rig.post(t, `{"text":"one"}`, nil)
	rig.now = rig.now.Add(500 * time.Millisecond)
	if _, res := rig.post(t, `{"text":"two"}`, nil); res.Code != "rate_limited" {
		t.Fatalf("0.5 s after a slow send finished must still be rate limited: %+v", res)
	}
}

func TestFleetSayOriginGuard(t *testing.T) {
	rig := newSayRig(t, []Session{fleetRow()})
	if code, _ := rig.post(t, `{"text":"hi"}`, func(r *http.Request) { r.Header.Set("Origin", "http://evil.example") }); code != http.StatusForbidden {
		t.Fatalf("wrong origin: %d", code)
	}
	if code, _ := rig.post(t, `{"text":"hi"}`, func(r *http.Request) { r.Header.Set("Origin", "null") }); code != http.StatusForbidden {
		t.Fatalf("null origin: %d", code)
	}
	if code, _ := rig.post(t, `{"text":"hi"}`, func(r *http.Request) { r.Header.Del("Origin") }); code != http.StatusForbidden {
		t.Fatalf("no origin, no referer: %d", code)
	}
	code, res := rig.post(t, `{"text":"hi"}`, func(r *http.Request) {
		r.Header.Del("Origin")
		r.Header.Set("Referer", "http://ws.ts.net:8765/fleet")
	})
	if code != 200 || !res.OK {
		t.Fatalf("matching referer: %d %+v", code, res)
	}
	if len(rig.typed) != 1 {
		t.Fatalf("typed %q", rig.typed)
	}
}

func TestFleetSayBearerSkipsOrigin(t *testing.T) {
	rig := newSayRig(t, []Session{fleetRow()})
	r := httptest.NewRequest("POST", "http://ws.ts.net:8765/fleet/say", strings.NewReader(`{"text":"hi"}`))
	r.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	fleetMux(rig.s).ServeHTTP(w, r)
	if w.Code != 200 || len(rig.typed) != 1 {
		t.Fatalf("%d %q", w.Code, rig.typed)
	}
}

func TestFleetSayUnauthorized(t *testing.T) {
	rig := newSayRig(t, []Session{fleetRow()})
	r := httptest.NewRequest("POST", "/fleet/say", strings.NewReader(`{"text":"hi"}`))
	w := httptest.NewRecorder()
	fleetMux(rig.s).ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized || len(rig.typed) != 0 {
		t.Fatalf("%d", w.Code)
	}
}

func TestFleetSayRefusesCopyMode(t *testing.T) {
	rig := newSayRig(t, []Session{fleetRow()})
	rig.s.fleet.paneInMode = func(string) (bool, error) { return true, nil }
	if _, res := rig.post(t, `{"text":"hi"}`, nil); res.OK || res.Code != "pane_mode" ||
		res.Error != "The FLEET pane is in scroll mode. Press q in the terminal, then try again." {
		t.Fatalf("%+v", res)
	}
	rig.s.fleet.paneInMode = func(string) (bool, error) { return false, errors.New("tmux gone") }
	rig.now = rig.now.Add(time.Minute)
	if _, res := rig.post(t, `{"text":"hi"}`, nil); res.OK || res.Code != "no_pane" {
		t.Fatalf("%+v", res)
	}
	if len(rig.typed) != 0 {
		t.Fatalf("nothing may be typed into copy mode: %q", rig.typed)
	}
}

func TestFleetSayLandedCheck(t *testing.T) {
	long := "Andy decides: DR-1 — yes (explicit). " + strings.Repeat("word ", 20)
	for name, tc := range map[string]struct {
		text, box string
		want      bool
	}{
		"exact text":     {"hi there", "hi there", true},
		"wrapped prefix": {strings.TrimSpace(long), strings.TrimSpace(long[:60]), true},
		"pasted":         {"hi", "[Pasted text #1 +2 lines]", true},
		"other text":     {"hi", "zzz", false},
		"longer than":    {"hi", "hi there", false},
	} {
		rig := newSayRig(t, []Session{fleetRow()})
		rig.captures = []string{emptyBoxCapture, boxCapture(tc.box)}
		_, res := rig.post(t, `{"text":"`+tc.text+`"}`, nil)
		if tc.want {
			if !res.OK || rig.enters != 1 {
				t.Errorf("%s: %+v enters %d", name, res, rig.enters)
			}
		} else if res.Code != "not_submitted" || rig.enters != 0 {
			t.Errorf("%s: %+v enters %d", name, res, rig.enters)
		}
	}
}

func TestFleetSayRefusesLeadingBangOrHash(t *testing.T) {
	rig := newSayRig(t, []Session{fleetRow()})
	for _, text := range []string{"!ls", "#remember this", "  !ls"} {
		r := httptest.NewRequest("POST", "http://ws.ts.net:8765/fleet/say", strings.NewReader(`{"text":"`+text+`"}`))
		r.Header.Set("Authorization", "Bearer tok")
		w := httptest.NewRecorder()
		fleetMux(rig.s).ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest || strings.TrimSpace(w.Body.String()) != "text must not start with ! or #" {
			t.Errorf("%q: %d %q", text, w.Code, w.Body.String())
		}
	}
	if len(rig.typed) != 0 {
		t.Fatalf("typed %q", rig.typed)
	}
}
