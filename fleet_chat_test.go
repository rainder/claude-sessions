package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
