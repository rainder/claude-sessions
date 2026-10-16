package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func chatLine(t *testing.T, typ, uuid, ts string, content any, extra map[string]any) string {
	t.Helper()
	m := map[string]any{"type": typ, "uuid": uuid, "timestamp": ts,
		"message": map[string]any{"role": typ, "content": content}}
	for k, v := range extra {
		if k == "message.id" {
			m["message"].(map[string]any)["id"] = v
			continue
		}
		m[k] = v
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func chatText(s string) []map[string]any { return []map[string]any{{"type": "text", "text": s}} }

func writeChatTranscript(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func appendChat(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		t.Fatal(err)
	}
}

func sampleTranscript(t *testing.T) string {
	return writeChatTranscript(t,
		chatLine(t, "user", "u1", "2026-10-13T09:00:00Z", "hello fleet", nil),
		chatLine(t, "assistant", "a1", "2026-10-13T09:00:01Z", chatText("draft"), map[string]any{"message.id": "m1", "requestId": "r1"}),
		chatLine(t, "user", "u2", "2026-10-13T09:00:02Z", []map[string]any{{"type": "tool_result", "content": "x"}}, nil),
		chatLine(t, "assistant", "a1b", "2026-10-13T09:00:03Z", chatText("final answer"), map[string]any{"message.id": "m1", "requestId": "r1"}),
		chatLine(t, "user", "u3", "2026-10-13T09:00:04Z", "<task-notification>done</task-notification>", nil),
		chatLine(t, "user", "u4", "2026-10-13T09:00:05Z", "meta", map[string]any{"isMeta": true}),
		chatLine(t, "user", "u5", "2026-10-13T09:00:06Z", "Andy decides: yes", nil),
	)
}

func TestExtractChatTurnsFiltersAndDedupes(t *testing.T) {
	path := sampleTranscript(t)
	turns, pos, reset, err := extractChatTurns(path, 0, 50, 1<<20)
	if err != nil || !reset {
		t.Fatalf("err %v reset %v", err, reset)
	}
	if st, _ := os.Stat(path); pos != st.Size() {
		t.Fatalf("pos %d, size %d", pos, st.Size())
	}
	want := []fleetChatTurn{
		{UUID: "u1", Role: "user", At: "2026-10-13T09:00:00Z", Text: "hello fleet"},
		{UUID: "a1", Role: "assistant", At: "2026-10-13T09:00:01Z", Text: "final answer"},
		{UUID: "u5", Role: "user", At: "2026-10-13T09:00:06Z", Text: "Andy decides: yes"},
	}
	if len(turns) != len(want) {
		t.Fatalf("turns = %+v", turns)
	}
	for i := range want {
		if turns[i] != want[i] {
			t.Errorf("turn %d = %+v want %+v", i, turns[i], want[i])
		}
	}
}

func TestExtractChatTurnsNoChangeSkipsScan(t *testing.T) {
	path := sampleTranscript(t)
	_, pos, _, _ := extractChatTurns(path, 0, 50, 1<<20)
	turns, pos2, reset, err := extractChatTurns(path, pos, 50, 1<<20)
	if err != nil || reset || len(turns) != 0 || pos2 != pos {
		t.Fatalf("turns %+v pos %d reset %v err %v", turns, pos2, reset, err)
	}
}

func TestExtractChatTurnsReturnsNewAndUpdatedTurns(t *testing.T) {
	// Assistant A (draft), then a queued user message B, then a later line
	// of A's same message. A uuid cursor at B would never resend A.
	path := writeChatTranscript(t,
		chatLine(t, "assistant", "a1", "2026-10-13T09:00:01Z", chatText("draft"), map[string]any{"message.id": "m1", "requestId": "r1"}),
		chatLine(t, "user", "b1", "2026-10-13T09:00:02Z", "queued question", nil),
	)
	_, pos, _, _ := extractChatTurns(path, 0, 50, 1<<20)
	appendChat(t, path,
		chatLine(t, "assistant", "a1x", "2026-10-13T09:00:03Z", chatText("final"), map[string]any{"message.id": "m1", "requestId": "r1"}),
		chatLine(t, "user", "c1", "2026-10-13T09:00:04Z", "next", nil),
	)
	turns, _, reset, err := extractChatTurns(path, pos, 50, 1<<20)
	if err != nil || reset {
		t.Fatalf("err %v reset %v", err, reset)
	}
	if len(turns) != 2 || turns[0].UUID != "a1" || turns[0].Text != "final" || turns[1].UUID != "c1" {
		t.Fatalf("turns = %+v (want updated a1, then c1; not b1)", turns)
	}
}

func TestExtractChatTurnsIgnoresPartialLastLine(t *testing.T) {
	path := sampleTranscript(t)
	st, _ := os.Stat(path)
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	_, _ = f.WriteString(`{"type":"user","uuid":"half"`)
	f.Close()
	turns, pos, _, _ := extractChatTurns(path, 0, 50, 1<<20)
	if pos != st.Size() || turns[len(turns)-1].UUID != "u5" {
		t.Fatalf("pos %d want %d, last %+v", pos, st.Size(), turns[len(turns)-1])
	}
}

func TestExtractChatTurnsTooManyKeepsNewestAndResets(t *testing.T) {
	turns, _, reset, _ := extractChatTurns(sampleTranscript(t), 0, 2, 1<<20)
	if !reset || len(turns) != 2 || turns[0].UUID != "a1" || turns[1].UUID != "u5" {
		t.Fatalf("reset %v turns %+v", reset, turns)
	}
}

func TestExtractChatTurnsSinceBeyondFileResets(t *testing.T) {
	turns, _, reset, _ := extractChatTurns(sampleTranscript(t), 1<<40, 50, 1<<20)
	if !reset || len(turns) != 3 {
		t.Fatalf("reset %v turns %+v", reset, turns)
	}
}

func TestFleetChatNoFleet(t *testing.T) {
	s := fleetTestServer(t, nil)
	out := getFleetJSON(t, s, "/fleet/chat")
	if len(out["turns"].([]any)) != 0 || out["reason"] != "not running" || out["reset"] != true {
		t.Fatalf("got %v", out)
	}
}

func TestFleetChatBadCursor(t *testing.T) {
	s := fleetTestServer(t, nil)
	for _, q := range []string{"?since=x", "?since=-1", "?n=0", "?n=201"} {
		r := httptest.NewRequest("GET", "/fleet/chat"+q, nil)
		r.Header.Set("Authorization", "Bearer tok")
		w := httptest.NewRecorder()
		fleetMux(s).ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", q, w.Code)
		}
	}
}

func fleetChatFixture(t *testing.T) []string {
	return []string{
		chatLine(t, "user", "u1", "2026-10-13T09:00:00Z", "hello fleet", nil),
		chatLine(t, "assistant", "a1", "2026-10-13T09:00:01Z", chatText("all quiet"), map[string]any{"message.id": "m1", "requestId": "r1"}),
	}
}

func TestFleetChatAliveHasNoReadOnly(t *testing.T) {
	s := fleetTestServer(t, []Session{{PID: 1, Name: "FLEET", SessionID: "chat-alive-1"}})
	putFleetTranscript(t, s.fleet.home, "chat-alive-1", fleetChatFixture(t)...)
	out := getFleetJSON(t, s, "/fleet/chat")
	if len(out["turns"].([]any)) != 2 || out["sessionId"] != "chat-alive-1" {
		t.Fatalf("got %v", out)
	}
	if _, ok := out["readOnly"]; ok {
		t.Fatalf("alive FLEET must not be read-only: %v", out)
	}
}

func TestFleetChatReadOnlyFallback(t *testing.T) {
	s := fleetTestServer(t, nil)
	writeFleetLast(t, s, fleetLast{SessionID: fleetUUID(12), CWD: "/no/longer/there"})
	path := putFleetTranscript(t, s.fleet.home, fleetUUID(12), fleetChatFixture(t)...)
	out := getFleetJSON(t, s, "/fleet/chat")
	if len(out["turns"].([]any)) != 2 || out["sessionId"] != fleetUUID(12) || out["readOnly"] != true ||
		out["reason"] != "not running" || out["reset"] != true {
		t.Fatalf("got %v", out)
	}
	// Same cursor semantics as a live FLEET.
	pos := int64(out["pos"].(float64))
	appendChat(t, path, chatLine(t, "user", "u2", "2026-10-13T09:01:00Z", "one more", nil))
	out = getFleetJSON(t, s, "/fleet/chat?since="+strconv.FormatInt(pos, 10))
	if turns := out["turns"].([]any); len(turns) != 1 || out["reset"] != false || out["readOnly"] != true {
		t.Fatalf("since: %v", out)
	}
}

func TestFleetChatNoFallback(t *testing.T) {
	cases := []struct {
		name     string
		sessions []Session
		last     *fleetLast
		script   bool
		reason   string
	}{
		{"ambiguous", []Session{{PID: 1, Name: "FLEET"}, {PID: 2, Name: "FLEET"}}, &fleetLast{SessionID: fleetUUID(13)}, true, "ambiguous"},
		{"no transcript", nil, &fleetLast{SessionID: fleetUUID(14)}, false, "not running"},
		{"bad id", nil, &fleetLast{SessionID: "../../x"}, false, "not running"},
		{"flag id", nil, &fleetLast{SessionID: "--dangerously-skip-permissions"}, true, "not running"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := fleetTestServer(t, c.sessions)
			writeFleetLast(t, s, *c.last)
			if c.script {
				putFleetTranscript(t, s.fleet.home, c.last.SessionID, fleetChatFixture(t)...)
			}
			out := getFleetJSON(t, s, "/fleet/chat")
			if len(out["turns"].([]any)) != 0 || out["reason"] != c.reason || out["sessionId"] != "" {
				t.Fatalf("got %v", out)
			}
			if _, ok := out["readOnly"]; ok {
				t.Fatalf("no fallback must not be read-only: %v", out)
			}
		})
	}
}

// putGrokChat writes a grok session's summary.json and chat_history.jsonl
// under home and returns the chat_history path.
func putGrokChat(t *testing.T, home, cwd, sid string, lines ...string) string {
	t.Helper()
	grokSummaryFixture(t, home, cwd, sid, `{"generated_title": "FLEET", "title_is_manual": true}`)
	p, ok := grokChatHistoryPath(home, cwd, sid)
	if !ok {
		t.Fatal("no grok summary")
	}
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// grokChatFixture is the shape of a real chat_history.jsonl: a system line,
// a synthetic reminder, a real prompt, reasoning, an assistant line with a
// tool call and text, a tool result, and a final answer.
var grokChatFixture = []string{
	`{"type":"system","content":"You are Grok"}`,
	`{"type":"user","content":[{"type":"text","text":"<system-reminder>ctx</system-reminder>"}],"synthetic_reason":"project_instructions"}`,
	`{"type":"user","content":[{"type":"text","text":"<user_query>\nhello   fleet\n</user_query>"}],"prompt_index":0}`,
	`{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"thinking"}]}`,
	`{"type":"assistant","content":"I will check.","tool_calls":[{"id":"c1","name":"run"}]}`,
	`{"type":"tool_result","tool_call_id":"c1","content":"exit: 0"}`,
	`{"type":"assistant","content":"","tool_calls":[{"id":"c2","name":"run"}]}`,
	`{"type":"user","content":"no prompt index: not a real prompt"}`,
	`{"type":"assistant","content":"all quiet"}`,
}

func TestGrokChatTurns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chat_history.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(grokChatFixture, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	turns, pos, reset, older, err := extractTurns(path, 0, 50, 1<<20, grokChatTurn)
	if err != nil || !reset || older != 0 {
		t.Fatalf("err %v reset %v older %d", err, reset, older)
	}
	want := []struct{ role, text string }{{"user", "hello fleet"}, {"assistant", "I will check."}, {"assistant", "all quiet"}}
	if len(turns) != len(want) {
		t.Fatalf("turns %+v", turns)
	}
	for i, w := range want {
		if turns[i].Role != w.role || turns[i].Text != w.text || turns[i].At != "" || !strings.HasPrefix(turns[i].UUID, "g") {
			t.Errorf("turn %d = %+v, want %s %q", i, turns[i], w.role, w.text)
		}
	}
	// The uuid is the line's byte offset plus a hash of the line: stable,
	// so the page can upsert, and a rewritten line gets a new one.
	off := int64(len(strings.Join(grokChatFixture[:2], "\n")) + 1)
	if !strings.HasPrefix(turns[0].UUID, "g"+strconv.FormatInt(off, 10)+"-") {
		t.Fatalf("uuid %q, want g%d-<hash>", turns[0].UUID, off)
	}
	// Same since cursor: an unchanged file returns nothing, an append
	// returns only the new turn.
	if again, p2, r2, _, _ := extractTurns(path, pos, 50, 1<<20, grokChatTurn); len(again) != 0 || p2 != pos || r2 {
		t.Fatalf("unchanged: %+v %d %v", again, p2, r2)
	}
	appendChat(t, path, `{"type":"user","content":"next","prompt_index":1}`)
	if more, _, r3, _, _ := extractTurns(path, pos, 50, 1<<20, grokChatTurn); len(more) != 1 || more[0].Text != "next" || r3 {
		t.Fatalf("append: %+v %v", more, r3)
	}
	// Paging backward uses the same decoder and offsets.
	back, _, err := extractTurnsBefore(path, pos, 2, grokChatTurn)
	if err != nil || len(back) != 2 || back[1].UUID != turns[2].UUID {
		t.Fatalf("before: %+v %v", back, err)
	}
}

func TestFleetChatGrokAlive(t *testing.T) {
	sid := fleetUUID(40)
	row := Session{PID: 1, Tool: toolGrok, Name: "FLEET", NameSource: "user", SessionID: sid, CWD: "/w/grok"}
	s := fleetTestServer(t, []Session{row})
	putGrokChat(t, s.fleet.home, "/w/grok", sid, grokChatFixture...)
	out := getFleetJSON(t, s, "/fleet/chat")
	turns := out["turns"].([]any)
	if len(turns) != 3 || out["sessionId"] != sid || turns[2].(map[string]any)["text"] != "all quiet" {
		t.Fatalf("got %v", out)
	}
	if _, ok := out["readOnly"]; ok {
		t.Fatalf("alive grok FLEET must not be read-only: %v", out)
	}
	if fl := getFleetJSON(t, s, "/fleet/state")["fleet"].(map[string]any); fl["tool"] != "grok" {
		t.Fatalf("state fleet = %v", fl)
	}
}

func TestFleetChatGrokReadOnlyFallback(t *testing.T) {
	sid := fleetUUID(41)
	s := fleetTestServer(t, nil)
	writeFleetLast(t, s, fleetLast{SessionID: sid, CWD: "/w/grok", Tool: toolGrok})
	path := putGrokChat(t, s.fleet.home, "/w/grok", sid, grokChatFixture...)
	out := getFleetJSON(t, s, "/fleet/chat")
	if len(out["turns"].([]any)) != 3 || out["sessionId"] != sid || out["readOnly"] != true {
		t.Fatalf("got %v", out)
	}
	pos := int64(out["pos"].(float64))
	appendChat(t, path, `{"type":"assistant","content":"one more"}`)
	out = getFleetJSON(t, s, "/fleet/chat?since="+strconv.FormatInt(pos, 10))
	if turns := out["turns"].([]any); len(turns) != 1 || out["reset"] != false {
		t.Fatalf("since: %v", out)
	}
}

// Grok can rewrite chat_history.jsonl whole. A cursor that then lands inside
// a line was not handed out by this file: the answer resets.
func TestGrokChatRewriteResetsCursor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chat_history.jsonl")
	write := func(lines []string) {
		if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(grokChatFixture)
	_, pos, _, _, err := extractTurns(path, 0, 50, 1<<20, grokChatTurn)
	if err != nil {
		t.Fatal(err)
	}
	// Rewritten with a longer first line: the old pos now sits mid-line.
	rewritten := append([]string{`{"type":"system","content":"` + strings.Repeat("x", 7) + `"}`}, grokChatFixture[1:]...)
	rewritten = append(rewritten, `{"type":"assistant","content":"after rewrite"}`)
	write(rewritten)
	turns, _, reset, _, err := extractTurns(path, pos, 50, 1<<20, grokChatTurn)
	if err != nil || !reset || len(turns) != 4 {
		t.Fatalf("err %v reset %v turns %+v", err, reset, turns)
	}
}
