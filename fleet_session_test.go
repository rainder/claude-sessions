package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Invented sessions and cards. The worker's card writes its session as
// "name:0.0"; only the part before ':' counts.
const fleetSessionState = `{"version":1,"writtenAt":"2026-10-13T09:40:00Z","cards":[
 {"id":"a","kind":"working","status":"working","title":"Forecast fix","ticket":"DR-1","session":"spawn-dr-1-fix:0.0"},
 {"id":"b","kind":"waiting","title":"Grok check","ticket":7,"session":"grok-dr-2"},
 {"id":"c","kind":"working","session":"fleet"},
 {"id":"d","kind":"working","session":"named-fleet"},
 {"id":"e","kind":"working","session":"ended-dr-3"},
 {"id":"f","kind":"working","session":"dup"},
 {"id":"g","kind":"working","session":"spawn-dr-4"},
 {"id":"h","kind":"decision","title":"no session"}
]}`

func fleetSessionRows(workerSID string) []Session {
	return []Session{
		{PID: 7, Name: "FLEET", NameSource: "user", SessionID: "fleet-sid", Status: "shell", Tmux: "fleet:0.0"},
		{PID: 11, Name: "DR-1 fix", SessionID: workerSID, Status: "busy", Model: "claude-opus-x", Tmux: "spawn-dr-1-fix:0.0"},
		{PID: 12, Tool: toolGrok, SessionID: "grok-sid", Status: "idle", Tmux: "grok-dr-2:0.0"},
		// A grok row titled FLEET is not FLEET for findFleet, but its tmux
		// session still must not be typed into from here.
		{PID: 13, Tool: toolGrok, Name: "FLEET", SessionID: "g2", Tmux: "named-fleet:0.0"},
		{PID: 14, SessionID: "dup-a", Tmux: "dup:0.0"},
		{PID: 15, SessionID: "dup-b", Tmux: "dup:1.0"},
		{PID: 16, SessionID: fleetUUID(199), Tmux: "spawn-dr-4:0.0"}, // no transcript on disk
		{PID: 17, SessionID: "not-on-a-card", Tmux: "other:0.0"},
	}
}

func fleetSessionServer(t *testing.T, workerSID string) *server {
	t.Helper()
	s := fleetTestServer(t, fleetSessionRows(workerSID))
	writeFleetFile(t, s, "state.json", fleetSessionState)
	return s
}

func getFleetSession(t *testing.T, s *server, query string) (int, map[string]any) {
	t.Helper()
	r := httptest.NewRequest("GET", "/fleet/session?"+query, nil)
	r.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	fleetMux(s).ServeHTTP(w, r)
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s: %d %q", query, w.Code, w.Body.String())
	}
	return w.Code, out
}

func TestFleetSessionAllowlist(t *testing.T) {
	s := fleetSessionServer(t, fleetUUID(101))
	cases := []struct {
		t    string
		code int
		err  string
	}{
		{"spawn-dr-1-fix", 200, ""},
		{"grok-dr-2", 200, ""},
		{"other", 403, "not_allowed"},             // live, but no card names it
		{"nobody", 403, "not_allowed"},            // neither
		{"fleet", 403, "fleet_self"},              // FLEET's own tmux session
		{"named-fleet", 403, "fleet_self"},        // a row there is named FLEET
		{"ended-dr-3", 404, "gone"},               // on a card, no live row
		{"dup", 409, "ambiguous"},                 // two rows in one tmux session
		{"spawn-dr-1-fix:0.0", 400, "bad_target"}, // a pane address is not a name
		{"", 400, "bad_target"},
		{"a%20b", 400, "bad_target"},
		{"x;rm", 400, "bad_target"},
		{strings.Repeat("a", 129), 400, "bad_target"},
	}
	for _, c := range cases {
		code, out := getFleetSession(t, s, "t="+c.t)
		if code != c.code || (c.err != "" && (out["code"] != c.err || out["ok"] != false)) {
			t.Errorf("t=%q: %d %v, want %d %s", c.t, code, out, c.code, c.err)
		}
	}
	// No state.json at all: nothing is allowed.
	s2 := fleetTestServer(t, fleetSessionRows(fleetUUID(102)))
	if code, out := getFleetSession(t, s2, "t=spawn-dr-1-fix"); code != 403 || out["code"] != "not_allowed" {
		t.Fatalf("no state: %d %v", code, out)
	}
}

func TestFleetSessionNeedsAuth(t *testing.T) {
	s := fleetSessionServer(t, fleetUUID(103))
	w := httptest.NewRecorder()
	fleetMux(s).ServeHTTP(w, httptest.NewRequest("GET", "/fleet/session?t=spawn-dr-1-fix", nil))
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), `"unauthorized"`) {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestFleetSessionTranscriptFilterLeaksNothing(t *testing.T) {
	sid := fleetUUID(104)
	s := fleetSessionServer(t, sid)
	putFleetTranscript(t, s.fleet.home, sid,
		chatLine(t, "user", "u1", "2026-10-13T09:00:00Z", "hello worker <system-reminder>secret-reminder</system-reminder>", nil),
		chatLine(t, "assistant", "a1", "2026-10-13T09:00:01Z", []map[string]any{
			{"type": "thinking", "thinking": "secret-thinking"},
			{"type": "text", "text": "visible answer"},
			{"type": "tool_use", "name": "Bash", "input": map[string]any{"command": "secret-tool-input"}},
		}, map[string]any{"message.id": "m1", "requestId": "r1"}),
		chatLine(t, "user", "u2", "2026-10-13T09:00:02Z", []map[string]any{
			{"type": "tool_result", "content": "secret-tool-result"},
			{"type": "text", "text": "secret-text-beside-result"},
		}, nil),
		chatLine(t, "user", "u3", "2026-10-13T09:00:03Z", "secret-meta", map[string]any{"isMeta": true}),
		chatLine(t, "user", "u4", "2026-10-13T09:00:04Z",
			"<command-name>/secret-cmd</command-name><command-message>secret-msg</command-message><command-args>secret-args</command-args>", nil),
		chatLine(t, "user", "u5", "2026-10-13T09:00:05Z",
			"<local-command-stdout>secret-stdout</local-command-stdout><local-command-stderr>secret-stderr</local-command-stderr>", nil),
		chatLine(t, "user", "u6", "2026-10-13T09:00:06Z", chatText("before <system-reminder>secret-unclosed"), nil),
		chatLine(t, "assistant", "a2", "2026-10-13T09:00:07Z",
			chatText("<system-reminder>\nsecret-multiline\n</system-reminder>\nfinal words"), map[string]any{"message.id": "m2", "requestId": "r2"}),
		chatLine(t, "assistant", "a3", "2026-10-13T09:00:08Z", []map[string]any{
			{"type": "thinking", "thinking": "secret-thinking-only"},
		}, map[string]any{"message.id": "m3", "requestId": "r3"}),
	)
	code, out := getFleetSession(t, s, "t=spawn-dr-1-fix")
	if code != 200 || out["source"] != "transcript" {
		t.Fatalf("%d %v", code, out)
	}
	b, _ := json.Marshal(out)
	if strings.Contains(string(b), "secret") {
		t.Fatalf("leak: %s", b)
	}
	var got []string
	for _, tr := range out["turns"].([]any) {
		m := tr.(map[string]any)
		got = append(got, m["role"].(string)+":"+m["text"].(string))
	}
	want := []string{"user:hello worker", "assistant:visible answer", "user:before", "assistant:final words"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("turns %q, want %q", got, want)
	}
	tg := out["target"].(map[string]any)
	if tg["tmux"] != "spawn-dr-1-fix" || tg["ticket"] != "DR-1" || tg["title"] != "Forecast fix" || tg["kind"] != "working" ||
		tg["model"] != "claude-opus-x" || tg["status"] != "busy" || tg["tool"] != "claude" || tg["sessionId"] != sid ||
		tg["canSend"] != true || tg["sendNote"] != nil {
		t.Fatalf("target %v", tg)
	}
	if _, ok := out["pane"]; ok {
		t.Fatal("a transcript answer has no pane")
	}
}

func TestFleetSessionPagingAndCursor(t *testing.T) {
	sid := fleetUUID(105)
	s := fleetSessionServer(t, sid)
	var lines []string
	for i := 0; i < 10; i++ {
		lines = append(lines, chatLine(t, "user", fmt.Sprintf("u%d", i), "2026-10-13T09:00:00Z", fmt.Sprintf("turn %d", i), nil))
	}
	path := putFleetTranscript(t, s.fleet.home, sid, lines...)
	texts := func(out map[string]any) string {
		var got []string
		for _, tr := range out["turns"].([]any) {
			got = append(got, tr.(map[string]any)["text"].(string))
		}
		return strings.Join(got, ",")
	}
	_, out := getFleetSession(t, s, "t=spawn-dr-1-fix&n=3")
	if texts(out) != "turn 7,turn 8,turn 9" || out["reset"] != true || out["older"].(float64) <= 0 {
		t.Fatalf("first page: %v", out)
	}
	pos := int64(out["pos"].(float64))
	older := int64(out["older"].(float64))
	for _, want := range []string{"turn 4,turn 5,turn 6", "turn 1,turn 2,turn 3"} {
		_, out = getFleetSession(t, s, "t=spawn-dr-1-fix&n=3&before="+strconv.FormatInt(older, 10))
		if texts(out) != want || out["reset"] != false {
			t.Fatalf("before=%d: %v, want %s", older, out, want)
		}
		older = int64(out["older"].(float64))
	}
	_, out = getFleetSession(t, s, "t=spawn-dr-1-fix&n=3&before="+strconv.FormatInt(older, 10))
	if texts(out) != "turn 0" || out["older"].(float64) != 0 {
		t.Fatalf("last page: %v", out)
	}
	// since: nothing new, then one appended turn.
	_, out = getFleetSession(t, s, "t=spawn-dr-1-fix&since="+strconv.FormatInt(pos, 10))
	if len(out["turns"].([]any)) != 0 || out["reset"] != false {
		t.Fatalf("no change: %v", out)
	}
	appendChat(t, path, chatLine(t, "user", "u10", "2026-10-13T09:01:00Z", "turn 10", nil))
	_, out = getFleetSession(t, s, "t=spawn-dr-1-fix&since="+strconv.FormatInt(pos, 10))
	if texts(out) != "turn 10" || out["reset"] != false {
		t.Fatalf("appended: %v", out)
	}
	for _, q := range []string{"n=0", "n=201", "since=-1", "before=0", "before=x"} {
		if code, out := getFleetSession(t, s, "t=spawn-dr-1-fix&"+q); code != 400 || out["code"] != "bad_request" {
			t.Errorf("%s: %d %v", q, code, out)
		}
	}
}

func TestFleetSessionPaneSource(t *testing.T) {
	s := fleetSessionServer(t, fleetUUID(106))
	var asked []string
	s.fleet.paneHistory = func(target string) (string, error) {
		asked = append(asked, target)
		return "grok says hi \x1b[31mred\x1b[0m\x07\nsecond line   \n\n", nil
	}
	code, out := getFleetSession(t, s, "t=grok-dr-2")
	if code != 200 || out["source"] != "pane" || out["pane"] != "grok says hi red\nsecond line" || len(out["turns"].([]any)) != 0 {
		t.Fatalf("%d %v", code, out)
	}
	if len(asked) != 1 || asked[0] != "=grok-dr-2:0.0" {
		t.Fatalf("captured %v, want the resolved row's pane", asked)
	}
	tg := out["target"].(map[string]any)
	if tg["tool"] != "grok" || tg["canSend"] != false || tg["sendNote"] != "Send is not available for Grok sessions" || tg["ticket"] != "" {
		t.Fatalf("target %v", tg)
	}
	// A claude row whose transcript is not on disk falls back to the pane.
	if _, out := getFleetSession(t, s, "t=spawn-dr-4"); out["source"] != "pane" || out["target"].(map[string]any)["canSend"] != true {
		t.Fatalf("no transcript: %v", out)
	}
}

func TestFleetSessionPaneCap(t *testing.T) {
	s := fleetSessionServer(t, fleetUUID(107))
	big := strings.Repeat("ä", fleetSessionPaneMax) // 2 bytes each, one line
	s.fleet.paneHistory = func(string) (string, error) { return "first\n" + big, nil }
	_, out := getFleetSession(t, s, "t=grok-dr-2")
	p := out["pane"].(string)
	if len(p) > fleetSessionPaneMax || strings.Contains(p, "first") || !strings.HasPrefix(p, "ä") {
		t.Fatalf("pane len %d prefix %q", len(p), p[:4])
	}
}

// sessionSayRig is a say rig on the session fixtures that also records which
// pane each keystroke went to.
func sessionSayRig(t *testing.T) (*sayRig, *[]string) {
	rig := newSayRig(t, fleetSessionRows(fleetUUID(108)))
	writeFleetFile(t, rig.s, "state.json", fleetSessionState)
	var targets []string
	rig.s.fleet.typeText = func(target, text string) error {
		targets = append(targets, target)
		rig.typed = append(rig.typed, text)
		return nil
	}
	rig.s.fleet.pressEnter = func(target string) error { targets = append(targets, target); rig.enters++; return nil }
	return rig, &targets
}

func (rig *sayRig) postSession(t *testing.T, body string, mod func(*http.Request)) (int, map[string]any) {
	t.Helper()
	r := httptest.NewRequest("POST", "http://ws.ts.net:8765/fleet/session/say", strings.NewReader(body))
	r.AddCookie(&http.Cookie{Name: "cs_fleet", Value: "sec"})
	r.Header.Set("Origin", "http://ws.ts.net:8765")
	if mod != nil {
		mod(r)
	}
	w := httptest.NewRecorder()
	fleetMux(rig.s).ServeHTTP(w, r)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func TestFleetSessionSayTypesIntoTheResolvedPane(t *testing.T) {
	rig, targets := sessionSayRig(t)
	code, out := rig.postSession(t, `{"target":"spawn-dr-1-fix","text":"please rebase\non main"}`, nil)
	if code != 200 || out["ok"] != true || out["queued"] != true || out["target"] != "spawn-dr-1-fix" {
		t.Fatalf("%d %v", code, out)
	}
	if len(rig.typed) != 1 || rig.typed[0] != "please rebase on main" || rig.enters != 1 {
		t.Fatalf("typed %q enters %d", rig.typed, rig.enters)
	}
	if strings.Join(*targets, ",") != "=spawn-dr-1-fix:0.0,=spawn-dr-1-fix:0.0" {
		t.Fatalf("keys went to %v", *targets)
	}
	data, _ := os.ReadFile(rig.s.fleet.sayLogPath)
	if !strings.HasSuffix(string(data), " sent target=spawn-dr-1-fix len=21\n") || strings.Contains(string(data), "rebase") {
		t.Fatalf("say log %q", data)
	}
	if _, err := os.Stat(filepath.Join(rig.s.fleet.dir, "log.jsonl")); !os.IsNotExist(err) {
		t.Fatal("a session send must not touch log.jsonl")
	}
}

func TestFleetSessionSayRefusals(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		mod    func(*http.Request)
		status int
		code   string
	}{
		{"cookie without origin", `{"target":"spawn-dr-1-fix","text":"hi"}`, func(r *http.Request) { r.Header.Del("Origin") }, 403, "origin"},
		{"cross origin", `{"target":"spawn-dr-1-fix","text":"hi"}`, func(r *http.Request) { r.Header.Set("Origin", "http://evil.test") }, 403, "origin"},
		{"no auth", `{"target":"spawn-dr-1-fix","text":"hi"}`, func(r *http.Request) { r.Header.Del("Cookie") }, 401, "unauthorized"},
		{"grok", `{"target":"grok-dr-2","text":"hi"}`, nil, 409, "send_unsupported"},
		{"not on a card", `{"target":"other","text":"hi"}`, nil, 403, "not_allowed"},
		{"fleet itself", `{"target":"fleet","text":"hi"}`, nil, 403, "fleet_self"},
		{"gone", `{"target":"ended-dr-3","text":"hi"}`, nil, 404, "gone"},
		{"ambiguous", `{"target":"dup","text":"hi"}`, nil, 409, "ambiguous"},
		{"pane address", `{"target":"spawn-dr-1-fix:0.0","text":"hi"}`, nil, 400, "bad_target"},
		{"bang", `{"target":"spawn-dr-1-fix","text":"!rm"}`, nil, 400, "bad_text"},
		{"control", `{"target":"spawn-dr-1-fix","text":"a\u001bb"}`, nil, 400, "bad_text"},
		{"unknown field", `{"target":"spawn-dr-1-fix","text":"hi","pane":"x:0.0"}`, nil, 400, "bad_json"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rig, targets := sessionSayRig(t)
			code, out := rig.postSession(t, c.body, c.mod)
			if code != c.status || out["code"] != c.code || out["ok"] != false {
				t.Fatalf("%d %v", code, out)
			}
			if len(*targets) != 0 {
				t.Fatalf("typed into %v", *targets)
			}
		})
	}
}

func TestFleetSessionSayUsesAFreshList(t *testing.T) {
	rig, targets := sessionSayRig(t)
	getFleetSession(t, rig.s, "t=spawn-dr-1-fix") // caches the list with the worker
	rows := fleetSessionRows(fleetUUID(108))
	rig.s.collect = func() ([]Session, error) { return rows[:1], nil } // the worker ended
	if code, out := rig.postSession(t, `{"target":"spawn-dr-1-fix","text":"hi"}`, nil); code != 404 || out["code"] != "gone" || len(*targets) != 0 {
		t.Fatalf("%d %v", code, out)
	}
}

func TestFleetSessionSayRefusesADialog(t *testing.T) {
	rig, targets := sessionSayRig(t)
	rig.captures = []string{dialogCapture}
	code, out := rig.postSession(t, `{"target":"spawn-dr-1-fix","text":"hi"}`, nil)
	if code != 200 || out["ok"] != false || out["code"] != "dialog" || len(*targets) != 0 {
		t.Fatalf("%d %v", code, out)
	}
	data, _ := os.ReadFile(rig.s.fleet.sayLogPath)
	if string(data) != "2026-10-13T10:00:00Z refused code=dialog target=spawn-dr-1-fix len=2\n" {
		t.Fatalf("say log %q, want the header line only", data)
	}
}

func TestFleetSessionSaySharesTheGapWithFleetSay(t *testing.T) {
	rows := fleetSessionRows(fleetUUID(109))
	rig := newSayRig(t, rows)
	writeFleetFile(t, rig.s, "state.json", fleetSessionState)
	if _, res := rig.post(t, `{"text":"to fleet"}`, nil); !res.OK {
		t.Fatalf("fleet say: %+v", res)
	}
	rig.now = rig.now.Add(500 * time.Millisecond)
	if _, out := rig.postSession(t, `{"target":"spawn-dr-1-fix","text":"to worker"}`, nil); out["code"] != "rate_limited" {
		t.Fatalf("within 1 s of a FLEET send: %v", out)
	}
	rig.now = rig.now.Add(time.Second)
	if _, out := rig.postSession(t, `{"target":"spawn-dr-1-fix","text":"to worker"}`, nil); out["ok"] != true {
		t.Fatalf("after the gap: %v", out)
	}
	rig.now = rig.now.Add(500 * time.Millisecond)
	if _, res := rig.post(t, `{"text":"to fleet again"}`, nil); res.Code != "rate_limited" {
		t.Fatalf("FLEET within 1 s of a session send: %+v", res)
	}
	// The double-tap key is per target: the same text is fine for FLEET.
	rig.now = rig.now.Add(2 * time.Second)
	if _, res := rig.post(t, `{"text":"to worker"}`, nil); !res.OK {
		t.Fatalf("same text to FLEET: %+v", res)
	}
	rig.now = rig.now.Add(2 * time.Second)
	if _, out := rig.postSession(t, `{"target":"spawn-dr-1-fix","text":"to worker"}`, nil); out["code"] != "double_tap" {
		t.Fatalf("same text to the same session within 10 s: %v", out)
	}
}

func TestTmuxExactTarget(t *testing.T) {
	for in, want := range map[string]string{
		"":             "",
		"spawn-x:0.0":  "=spawn-x:0.0",
		"=spawn-x:0.0": "=spawn-x:0.0",
		"%12":          "%12",
		"fleet":        "=fleet",
	} {
		if got := tmuxExactTarget(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

func TestFleetSaySeamsGetTheExactPane(t *testing.T) {
	rig := newSayRig(t, []Session{fleetRow()})
	var seen []string
	capture := rig.s.fleet.capture
	rig.s.fleet.capture = func(p string) (string, error) { seen = append(seen, "capture "+p); return capture(p) }
	rig.s.fleet.paneInMode = func(p string) (bool, error) { seen = append(seen, "mode "+p); return false, nil }
	rig.s.fleet.typeText = func(p, text string) error {
		seen = append(seen, "type "+p)
		rig.typed = append(rig.typed, text)
		return nil
	}
	rig.s.fleet.pressEnter = func(p string) error { seen = append(seen, "enter "+p); rig.enters++; return nil }
	if _, res := rig.post(t, `{"text":"hi"}`, nil); !res.OK {
		t.Fatalf("%+v", res)
	}
	for _, c := range seen {
		if !strings.HasSuffix(c, " =fleet:0.0") {
			t.Fatalf("seam calls %v, want every one on =fleet:0.0", seen)
		}
	}
	if len(seen) < 4 {
		t.Fatalf("seam calls %v", seen)
	}
}

func TestFleetSessionRefusalLogNeverHoldsTheText(t *testing.T) {
	rig, _ := sessionSayRig(t)
	// The box shows other text after typing: not submitted. The pane holds
	// the typed words, so none of it may reach the log.
	rig.captures = []string{emptyBoxCapture, boxCapture("secret words typed here, then more")}
	_, out := rig.postSession(t, `{"target":"spawn-dr-1-fix","text":"secret words typed here"}`, nil)
	if out["code"] != "not_submitted" {
		t.Fatalf("%v", out)
	}
	data, _ := os.ReadFile(rig.s.fleet.sayLogPath)
	if string(data) != "2026-10-13T10:00:00Z refused code=not_submitted target=spawn-dr-1-fix len=23\n" {
		t.Fatalf("say log %q", data)
	}
}

func TestFleetSayRefusalLogKeepsFleetPaneLines(t *testing.T) {
	rig := newSayRig(t, []Session{fleetRow()})
	rig.captures = []string{dialogCapture}
	if _, res := rig.post(t, `{"text":"hi"}`, nil); res.Code != "dialog" {
		t.Fatalf("%+v", res)
	}
	data, _ := os.ReadFile(rig.s.fleet.sayLogPath)
	if !strings.Contains(string(data), "refused code=dialog target=fleet:0.0\n") || !strings.Contains(string(data), "Do you want to proceed?") {
		t.Fatalf("say log %q", data)
	}
}

func TestFleetSessionFilterPlumbing(t *testing.T) {
	sid := fleetUUID(110)
	s := fleetSessionServer(t, sid)
	putFleetTranscript(t, s.fleet.home, sid,
		chatLine(t, "user", "u1", "2026-10-13T09:00:00Z", "<bash-input>secret-cmd</bash-input>", nil),
		chatLine(t, "user", "u2", "2026-10-13T09:00:01Z", "<bash-stdout>secret-stdout</bash-stdout><bash-stderr>secret-stderr</bash-stderr>", nil),
		chatLine(t, "user", "u3", "2026-10-13T09:00:02Z", "<task-notification>secret-task</task-notification>", nil),
		chatLine(t, "user", "u4", "2026-10-13T09:00:03Z", "<user-prompt-submit-hook>secret-hook</user-prompt-submit-hook>real ask", nil),
		chatLine(t, "user", "u5", "2026-10-13T09:00:04Z", "This session is being continued. secret-compact", map[string]any{"isCompactSummary": true}),
		chatLine(t, "user", "u6", "2026-10-13T09:00:05Z", "<unknown-wrapper>secret-unknown</unknown-wrapper>", nil),
		chatLine(t, "assistant", "a1", "2026-10-13T09:00:06Z", chatText("Use the <command-name> tag to name it"),
			map[string]any{"message.id": "m1", "requestId": "r1"}),
	)
	_, out := getFleetSession(t, s, "t=spawn-dr-1-fix")
	b, _ := json.Marshal(out["turns"])
	if strings.Contains(string(b), "secret") {
		t.Fatalf("leak: %s", b)
	}
	var got []string
	for _, tr := range out["turns"].([]any) {
		m := tr.(map[string]any)
		got = append(got, m["role"].(string)+":"+m["text"].(string))
	}
	want := "user:real ask|assistant:Use the <command-name> tag to name it"
	if strings.Join(got, "|") != want {
		t.Fatalf("turns %q, want %q", got, want)
	}
}
