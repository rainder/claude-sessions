package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// clarifyForkID is the fake fork's session id. Test FLEET ids are
// fleetUUID(7001...), unique per test: findTranscript caches by id.
var clarifyForkID = fleetUUID(7000)

type clarifyCall struct {
	cwd  string
	argv []string
}

type clarifyFake struct {
	res clarifyRunResult
	err error
}

// clarifyRig is a fleet test server with a scripted fork runner. results are
// consumed in order and the last one repeats; block makes the runner wait for
// its context instead.
type clarifyRig struct {
	s        *server
	sessions []Session
	calls    []clarifyCall
	results  []clarifyFake
	block    bool
	queued   []func() // clarifyGo's funcs when collect is set
	collect  bool
	writes   int
	now      time.Time
}

func newClarifyRig(t *testing.T) *clarifyRig {
	t.Helper()
	rig := &clarifyRig{now: time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)}
	rig.s = fleetTestServer(t, nil)
	rig.s.collect = func() ([]Session, error) { return rig.sessions, nil }
	rig.s.fleet.now = func() time.Time { return rig.now }
	rig.s.fleet.clarifyRetryDelay = time.Millisecond
	rig.s.fleet.clarifyCuPath = func() string { return "/usr/bin/cu-test" }
	rig.s.fleet.clarifyGo = func(f func()) {
		if rig.collect {
			rig.queued = append(rig.queued, f)
			return
		}
		f()
	}
	rig.s.fleet.clarifyRun = func(ctx context.Context, cwd string, argv []string) (clarifyRunResult, error) {
		rig.calls = append(rig.calls, clarifyCall{cwd: cwd, argv: append([]string(nil), argv...)})
		if rig.block {
			<-ctx.Done()
			return clarifyRunResult{}, ctx.Err()
		}
		if len(rig.results) == 0 {
			return clarifyRunResult{SessionID: clarifyForkID, Text: "because"}, nil
		}
		r := rig.results[0]
		if len(rig.results) > 1 {
			rig.results = rig.results[1:]
		}
		return r.res, r.err
	}
	return rig
}

// clarifyFleet is a live claude FLEET row whose transcript head says /w.
func (rig *clarifyRig) clarifyFleet(t *testing.T, sid string) {
	t.Helper()
	rig.sessions = []Session{{PID: 7, Name: "FLEET", SessionID: sid, Model: "M", CWD: "/fallback", Status: "shell", Tmux: "fleet:0.0"}}
	putFleetTranscript(t, rig.s.fleet.home, sid, `{"type":"user","cwd":"/w"}`)
}

func (rig *clarifyRig) cards(t *testing.T, cards string) {
	t.Helper()
	path := filepath.Join(rig.s.fleet.dir, "state.json")
	body := `{"version":1,"writtenAt":"` + rig.now.UTC().Format(time.RFC3339) + `","cards":` + cards + `}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	rig.writes++
	mt := rig.now.Add(time.Duration(rig.writes) * time.Second)
	if err := os.Chtimes(path, mt, mt); err != nil {
		t.Fatal(err)
	}
}

func (rig *clarifyRig) do(t *testing.T, method, path, body string) (int, map[string]json.RawMessage) {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	r.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	fleetMux(rig.s).ServeHTTP(w, r)
	var out map[string]json.RawMessage
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func (rig *clarifyRig) ask(t *testing.T, card, text string) (int, string) {
	t.Helper()
	code, out := rig.do(t, "POST", "/fleet/clarify/ask", sayBody(t, map[string]any{"cardId": card, "text": text}))
	return code, rawCode(out)
}

func rawCode(out map[string]json.RawMessage) string {
	var c string
	_ = json.Unmarshal(out["code"], &c)
	return c
}

type clarifyChat struct {
	Card    string          `json:"card"`
	Tool    string          `json:"tool"`
	Turns   []clarifyTurn   `json:"turns"`
	Pending *clarifyPending `json:"pending"`
	Draft   *clarifyDraft   `json:"draft"`
}

func (rig *clarifyRig) chat(t *testing.T, card string) (clarifyChat, map[string]json.RawMessage) {
	t.Helper()
	code, out := rig.do(t, "GET", "/fleet/clarify?card="+card, "")
	if code != http.StatusOK {
		t.Fatalf("GET clarify: %d", code)
	}
	b, _ := json.Marshal(out)
	var c clarifyChat
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	return c, out
}

func lastCall(t *testing.T, rig *clarifyRig) clarifyCall {
	t.Helper()
	if len(rig.calls) == 0 {
		t.Fatal("the fork never ran")
	}
	return rig.calls[len(rig.calls)-1]
}

func hasRun(argv []string, run ...string) bool {
	for i := 0; i+len(run) <= len(argv); i++ {
		if slices.Equal(argv[i:i+len(run)], run) {
			return true
		}
	}
	return false
}

func TestClarifyAuth(t *testing.T) {
	rig := newClarifyRig(t)
	rig.cards(t, dr1Card)
	for _, rt := range []struct{ method, path, body string }{
		{"GET", "/fleet/clarify?card=dr-1", ""},
		{"POST", "/fleet/clarify/ask", `{"cardId":"dr-1","text":"why?"}`},
		{"POST", "/fleet/clarify/draft", `{"cardId":"dr-1"}`},
	} {
		w := httptest.NewRecorder()
		fleetMux(rig.s).ServeHTTP(w, httptest.NewRequest(rt.method, rt.path, strings.NewReader(rt.body)))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s with no auth: %d", rt.method, rt.path, w.Code)
		}
		if rt.method != "POST" {
			continue
		}
		r := httptest.NewRequest(rt.method, "http://ws.ts.net:8765"+rt.path, strings.NewReader(rt.body))
		r.AddCookie(&http.Cookie{Name: "cs_fleet", Value: "sec"})
		r.Header.Set("Origin", "http://evil.example")
		w = httptest.NewRecorder()
		fleetMux(rig.s).ServeHTTP(w, r)
		var out map[string]json.RawMessage
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		if w.Code != http.StatusForbidden || rawCode(out) != "origin" {
			t.Fatalf("%s foreign origin: %d %s", rt.path, w.Code, w.Body.String())
		}
	}
	if len(rig.calls) != 0 {
		t.Fatalf("a refused request ran a fork: %+v", rig.calls)
	}
	if code, out := rig.do(t, "GET", "/fleet/clarify", ""); code != http.StatusBadRequest || rawCode(out) != "bad_request" {
		t.Fatalf("GET with no card: %d %v", code, out)
	}
}

func TestClarifyAskFirstForksFleet(t *testing.T) {
	rig := newClarifyRig(t)
	rig.cards(t, dr1Card)
	rig.clarifyFleet(t, fleetUUID(7001))
	code, out := rig.do(t, "POST", "/fleet/clarify/ask", `{"cardId":"dr-1","text":"  why now?  "}`)
	if code != http.StatusAccepted {
		t.Fatalf("ask: %d %v", code, out)
	}
	var p struct {
		Pending *clarifyPending `json:"pending"`
	}
	b, _ := json.Marshal(out)
	_ = json.Unmarshal(b, &p)
	if p.Pending == nil || p.Pending.Kind != "ask" {
		t.Fatalf("pending = %s", b)
	}
	c := lastCall(t, rig)
	if c.cwd != "/w" {
		t.Fatalf("cwd %q, want the transcript head's /w", c.cwd)
	}
	if !hasRun(c.argv, "--resume", fleetUUID(7001), "--fork-session", "--model", "M") {
		t.Fatalf("argv %q", c.argv)
	}
	if n := len(c.argv); n < 2 || c.argv[n-2] != "--" || c.argv[n-1] != "why now?" {
		t.Fatalf("argv tail %q", c.argv)
	}
	if !hasRun(c.argv, "--append-system-prompt", clarifyPreamble(cardFromJSON(t, dr1Card[1:len(dr1Card)-1]))) {
		t.Fatalf("preamble missing from %q", c.argv)
	}
	chat, raw := rig.chat(t, "dr-1")
	if chat.Card != "dr-1" || chat.Tool != "" || len(chat.Turns) != 2 ||
		chat.Turns[0].Role != "you" || chat.Turns[0].Text != "why now?" ||
		chat.Turns[1].Role != "fleet" || chat.Turns[1].Text != "because" {
		t.Fatalf("chat %+v", chat)
	}
	if string(raw["pending"]) != "null" || string(raw["draft"]) != "null" {
		t.Fatalf("pending %s draft %s, want null", raw["pending"], raw["draft"])
	}
}

func TestClarifyFallsBackToSessionCwd(t *testing.T) {
	rig := newClarifyRig(t)
	rig.cards(t, dr1Card)
	rig.sessions = []Session{{PID: 7, Name: "FLEET", SessionID: fleetUUID(7002), CWD: "/fallback", Status: "shell"}}
	if code, c := rig.ask(t, "dr-1", "why?"); code != http.StatusAccepted {
		t.Fatalf("ask: %d %s", code, c)
	}
	c := lastCall(t, rig)
	if c.cwd != "/fallback" || !hasRun(c.argv, "--model", "opus") {
		t.Fatalf("cwd %q argv %q", c.cwd, c.argv)
	}
}

func TestClarifyLaterAskNeedsNoLiveFleet(t *testing.T) {
	rig := newClarifyRig(t)
	rig.cards(t, dr1Card)
	rig.clarifyFleet(t, fleetUUID(7003))
	if code, c := rig.ask(t, "dr-1", "why?"); code != http.StatusAccepted {
		t.Fatalf("first ask: %d %s", code, c)
	}
	rig.sessions = nil
	rig.results = []clarifyFake{{res: clarifyRunResult{SessionID: clarifyForkID, Text: "still because"}}}
	if code, c := rig.ask(t, "dr-1", "and then?"); code != http.StatusAccepted {
		t.Fatalf("second ask: %d %s", code, c)
	}
	c := lastCall(t, rig)
	if !hasRun(c.argv, "--resume", clarifyForkID) || slices.Contains(c.argv, "--fork-session") || c.cwd != "/w" {
		t.Fatalf("cwd %q argv %q", c.cwd, c.argv)
	}
	if !hasRun(c.argv, "--model", "M") {
		t.Fatalf("a resumed fork must keep the stored model: %q", c.argv)
	}
	if chat, _ := rig.chat(t, "dr-1"); len(chat.Turns) != 4 {
		t.Fatalf("turns %+v", chat.Turns)
	}
}

func TestClarifyAskRefusals(t *testing.T) {
	rig := newClarifyRig(t)
	if code, c := rig.ask(t, "dr-1", "why?"); code != http.StatusConflict || c != "card_gone" {
		t.Fatalf("no state.json: %d %s", code, c)
	}
	rig.cards(t, dr1Card)
	if code, c := rig.ask(t, "nope", "why?"); code != http.StatusConflict || c != "card_gone" {
		t.Fatalf("unknown card: %d %s", code, c)
	}
	rig.sessions = []Session{{PID: 9, Name: "FLEET", SessionID: "g", Tool: toolGrok, NameSource: "user"}}
	if code, c := rig.ask(t, "dr-1", "why?"); code != http.StatusConflict || c != "unsupported_tool" {
		t.Fatalf("grok FLEET: %d %s", code, c)
	}
	rig.sessions = nil
	if code, c := rig.ask(t, "dr-1", "why?"); code != http.StatusConflict || c != "not_live" {
		t.Fatalf("no FLEET: %d %s", code, c)
	}
	rig.sessions = []Session{{PID: 7, Name: "FLEET", SessionID: "a"}, {PID: 8, Name: "FLEET", SessionID: "b"}}
	if code, c := rig.ask(t, "dr-1", "why?"); code != http.StatusConflict || c != "ambiguous" {
		t.Fatalf("two FLEETs: %d %s", code, c)
	}
	rig.clarifyFleet(t, fleetUUID(7004))
	if code, c := rig.ask(t, "dr-1", strings.Repeat("é", 4001)); code != http.StatusBadRequest || c != "too_long" {
		t.Fatalf("4001 runes: %d %s", code, c)
	}
	if code, c := rig.ask(t, "dr-1", "  "); code != http.StatusBadRequest || c != "empty" {
		t.Fatalf("blank: %d %s", code, c)
	}
	for _, body := range []string{`{"cardId":"dr-1","text":"x","extra":1}`, `{"text":"x"}`, `null`, `{"cardId":"dr-1","text":"x"} {}`} {
		if code, out := rig.do(t, "POST", "/fleet/clarify/ask", body); code != http.StatusBadRequest || rawCode(out) != "bad_request" {
			t.Fatalf("body %s: %d %v", body, code, out)
		}
	}
	for _, id := range []string{"dr 1", "dr/1", strings.Repeat("a", 101)} {
		if code, c := rig.ask(t, id, "why?"); code != http.StatusBadRequest || c != "bad_request" {
			t.Fatalf("card id %q: %d %s", id, code, c)
		}
	}
	rig.sessions = []Session{{PID: 7, Name: "FLEET", SessionID: "--dangerously-skip-permissions"}}
	if code, c := rig.ask(t, "dr-1", "why?"); code != http.StatusConflict || c != "not_live" {
		t.Fatalf("FLEET with a bad session id: %d %s", code, c)
	}
	rig.clarifyFleet(t, fleetUUID(7004))
	if len(rig.calls) != 0 {
		t.Fatalf("a refused ask ran a fork: %+v", rig.calls)
	}
	rig.collect = true
	if code, c := rig.ask(t, "dr-1", strings.Repeat("é", 4000)); code != http.StatusAccepted {
		t.Fatalf("4000 runes: %d %s", code, c)
	}
	if code, c := rig.ask(t, "dr-1", "again?"); code != http.StatusConflict || c != "busy" {
		t.Fatalf("second ask while pending: %d %s", code, c)
	}
}

func TestClarifyAskErrorTurn(t *testing.T) {
	for i, tc := range []struct {
		name  string
		fakes []clarifyFake
		calls int
		want  string
	}{
		{"is_error", []clarifyFake{{res: clarifyRunResult{SessionID: clarifyForkID, Text: "boom /secret/path", IsError: true}}}, 1, "the fork answered with an error"},
		{"timeout", []clarifyFake{{err: errClarifyTimeout}}, 1, "no answer within 5 minutes"},
		{"mid-step", []clarifyFake{{err: errClarifyExit}, {err: errClarifyExit}}, 2, "FLEET is mid-step; ask again in a moment"},
		{"bad output then other", []clarifyFake{{err: errClarifyBadOutput}, {err: context.Canceled}}, 2, "the fork failed; see the server log"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newClarifyRig(t)
			rig.cards(t, dr1Card)
			rig.clarifyFleet(t, fleetUUID(7010+i))
			rig.results = tc.fakes
			if code, c := rig.ask(t, "dr-1", "why?"); code != http.StatusAccepted {
				t.Fatalf("ask: %d %s", code, c)
			}
			if len(rig.calls) != tc.calls {
				t.Fatalf("%d fork calls, want %d", len(rig.calls), tc.calls)
			}
			chat, _ := rig.chat(t, "dr-1")
			if len(chat.Turns) != 2 || chat.Turns[1].Role != "error" || chat.Turns[1].Text != tc.want || chat.Pending != nil {
				t.Fatalf("chat %+v", chat)
			}
		})
	}
}

func TestClarifyNoRetryOnLaterAsk(t *testing.T) {
	rig := newClarifyRig(t)
	rig.cards(t, dr1Card)
	rig.clarifyFleet(t, fleetUUID(7005))
	if code, c := rig.ask(t, "dr-1", "why?"); code != http.StatusAccepted {
		t.Fatalf("ask: %d %s", code, c)
	}
	rig.results = []clarifyFake{{err: errClarifyExit}}
	if code, c := rig.ask(t, "dr-1", "more?"); code != http.StatusAccepted {
		t.Fatalf("ask: %d %s", code, c)
	}
	chat, _ := rig.chat(t, "dr-1")
	if len(rig.calls) != 2 || chat.Turns[3].Text != "the fork failed; see the server log" {
		t.Fatalf("calls %d turns %+v", len(rig.calls), chat.Turns)
	}
}

func TestClarifyAnswerCutAt32KiB(t *testing.T) {
	rig := newClarifyRig(t)
	rig.cards(t, dr1Card)
	rig.clarifyFleet(t, fleetUUID(7006))
	rig.results = []clarifyFake{{res: clarifyRunResult{SessionID: clarifyForkID, Text: "x" + strings.Repeat("é", 20000)}}}
	if code, c := rig.ask(t, "dr-1", "why?"); code != http.StatusAccepted {
		t.Fatalf("ask: %d %s", code, c)
	}
	chat, _ := rig.chat(t, "dr-1")
	got := chat.Turns[1].Text
	if len(got) > 32<<10 || len(got) < 32<<10-1 || !utf8.ValidString(got) {
		t.Fatalf("cut to %d bytes, valid %v", len(got), utf8.ValidString(got))
	}
}

func TestClarifyDraftAndDiscard(t *testing.T) {
	rig := newClarifyRig(t)
	rig.cards(t, dr1Card)
	rig.clarifyFleet(t, fleetUUID(7007))
	if code, out := rig.do(t, "POST", "/fleet/clarify/draft", `{"cardId":"dr-1"}`); code != http.StatusBadRequest || rawCode(out) != "empty" {
		t.Fatalf("draft before a turn: %d %v", code, out)
	}
	if code, c := rig.ask(t, "dr-1", "why?"); code != http.StatusAccepted {
		t.Fatalf("ask: %d %s", code, c)
	}
	rig.results = []clarifyFake{{res: clarifyRunResult{SessionID: clarifyForkID, Text: "  Go with yes.\n"}}}
	code, out := rig.do(t, "POST", "/fleet/clarify/draft", `{"cardId":"dr-1"}`)
	if code != http.StatusAccepted || !strings.Contains(string(out["pending"]), `"draft"`) {
		t.Fatalf("draft: %d %v", code, out)
	}
	c := lastCall(t, rig)
	if c.argv[len(c.argv)-1] != clarifyDraftPrompt("dr-1") || !hasRun(c.argv, "--resume", clarifyForkID) {
		t.Fatalf("argv %q", c.argv)
	}
	if clarifyDraftPrompt("dr-1") != "Write the decision I reached in this chat about card dr-1 as one short paragraph addressed to FLEET. No greeting, no preamble: output only the message." {
		t.Fatalf("draft prompt %q", clarifyDraftPrompt("dr-1"))
	}
	chat, _ := rig.chat(t, "dr-1")
	if chat.Draft == nil || chat.Draft.Text != "Go with yes." || len(chat.Turns) != 2 {
		t.Fatalf("chat %+v", chat)
	}
	if code, out := rig.do(t, "POST", "/fleet/clarify/draft", `{"cardId":"dr-1","discard":true}`); code != http.StatusOK || string(out["ok"]) != "true" {
		t.Fatalf("discard: %d %v", code, out)
	}
	if code, out := rig.do(t, "POST", "/fleet/clarify/draft", `{"cardId":"dr-1","discard":true}`); code != http.StatusNotFound || rawCode(out) != "not_found" {
		t.Fatalf("second discard: %d %v", code, out)
	}
	if chat, _ := rig.chat(t, "dr-1"); chat.Draft != nil {
		t.Fatalf("draft after discard %+v", chat.Draft)
	}
}

func (rig *clarifyRig) stateClarify(t *testing.T) map[string]clarifySummary {
	t.Helper()
	code, out := rig.do(t, "GET", "/fleet/state", "")
	if code != http.StatusOK {
		t.Fatalf("state: %d", code)
	}
	var m map[string]clarifySummary
	if err := json.Unmarshal(out["clarify"], &m); err != nil || m == nil {
		t.Fatalf("clarify = %s (%v), want an object", out["clarify"], err)
	}
	return m
}

func TestClarifyStateSummaryAndPrune(t *testing.T) {
	rig := newClarifyRig(t)
	rig.cards(t, dr1Card)
	rig.clarifyFleet(t, fleetUUID(7008))
	if got := rig.stateClarify(t); len(got) != 0 {
		t.Fatalf("before any chat: %+v", got)
	}
	if code, c := rig.ask(t, "dr-1", "why?"); code != http.StatusAccepted {
		t.Fatalf("ask: %d %s", code, c)
	}
	if got := rig.stateClarify(t); got["dr-1"] != (clarifySummary{Turns: 2}) || len(got) != 1 {
		t.Fatalf("summary %+v", got)
	}
	rig.cards(t, `[]`)
	if got := rig.stateClarify(t); len(got) != 0 {
		t.Fatalf("after the card left: %+v", got)
	}
	chat, raw := rig.chat(t, "dr-1")
	if len(chat.Turns) != 0 || string(raw["turns"]) != "[]" {
		t.Fatalf("chat after prune: %s", raw["turns"])
	}
}

func TestClarifyPruneCancelsRunningFork(t *testing.T) {
	rig := newClarifyRig(t)
	rig.cards(t, dr1Card)
	rig.clarifyFleet(t, fleetUUID(7009))
	rig.collect = true
	rig.block = true
	if code, c := rig.ask(t, "dr-1", "why?"); code != http.StatusAccepted || len(rig.queued) != 1 {
		t.Fatalf("ask: %d %s, %d queued", code, c, len(rig.queued))
	}
	rig.cards(t, `[]`)
	rig.stateClarify(t)
	done := make(chan struct{})
	go func() { rig.queued[0](); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the fork did not stop when its card was pruned")
	}
	if chat, raw := rig.chat(t, "dr-1"); len(chat.Turns) != 0 || chat.Pending != nil {
		t.Fatalf("chat after the cancelled fork: %v", raw)
	}
}

func TestClarifyDraftNeedsAFork(t *testing.T) {
	rig := newClarifyRig(t)
	rig.cards(t, dr1Card)
	rig.clarifyFleet(t, fleetUUID(7020))
	rig.results = []clarifyFake{{err: errClarifyExit}, {err: errClarifyExit}}
	if code, c := rig.ask(t, "dr-1", "why?"); code != http.StatusAccepted {
		t.Fatalf("ask: %d %s", code, c)
	}
	calls := len(rig.calls)
	if code, out := rig.do(t, "POST", "/fleet/clarify/draft", `{"cardId":"dr-1"}`); code != http.StatusBadRequest || rawCode(out) != "empty" {
		t.Fatalf("draft with no fork: %d %v", code, out)
	}
	if len(rig.calls) != calls {
		t.Fatalf("the draft ran a fork: %+v", rig.calls[calls:])
	}
}

func TestClarifyForkIDMustBeFresh(t *testing.T) {
	rig := newClarifyRig(t)
	rig.cards(t, dr1Card)
	fleetID := fleetUUID(7021)
	rig.clarifyFleet(t, fleetID)
	for _, bad := range []string{fleetID, "--help"} {
		rig.results = []clarifyFake{{res: clarifyRunResult{SessionID: bad, Text: "because"}}}
		if code, c := rig.ask(t, "dr-1", "why?"); code != http.StatusAccepted {
			t.Fatalf("ask: %d %s", code, c)
		}
		rig.results = []clarifyFake{{res: clarifyRunResult{SessionID: clarifyForkID, Text: "ok"}}}
		if code, c := rig.ask(t, "dr-1", "and?"); code != http.StatusAccepted {
			t.Fatalf("next ask: %d %s", code, c)
		}
		c := lastCall(t, rig)
		if !hasRun(c.argv, "--resume", fleetID, "--fork-session") {
			t.Fatalf("after fork id %q the next ask resumed %q", bad, c.argv)
		}
		rig.cards(t, `[]`)
		rig.stateClarify(t)
		rig.cards(t, dr1Card)
	}
}

func TestClarifyWorkerPanicFinishes(t *testing.T) {
	rig := newClarifyRig(t)
	rig.cards(t, dr1Card)
	rig.clarifyFleet(t, fleetUUID(7022))
	rig.s.fleet.clarifyRun = func(context.Context, string, []string) (clarifyRunResult, error) { panic("boom") }
	if code, c := rig.ask(t, "dr-1", "why?"); code != http.StatusAccepted {
		t.Fatalf("ask: %d %s", code, c)
	}
	chat, _ := rig.chat(t, "dr-1")
	if chat.Pending != nil || len(chat.Turns) != 2 || chat.Turns[1].Role != "error" || chat.Turns[1].Text != "the fork failed; see the server log" {
		t.Fatalf("chat after a panic: %+v", chat)
	}
	if code, c := rig.ask(t, "dr-1", "again?"); code != http.StatusAccepted {
		t.Fatalf("ask after a panic: %d %s (the running count leaked?)", code, c)
	}
}
