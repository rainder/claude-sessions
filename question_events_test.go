package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDecisionQuestionPatterns(t *testing.T) {
	yes := []string{
		"Working on it.\n\nWhich direction do you want: A, B, or C?",
		"Which option should I take",
		"Your decision is required before I continue",
		"This needs your yes",
		"Pick one: A, B, or C",
		"Earlier paragraph ends here?\n\nStill a question?",
		"One line?",
	}
	for _, text := range yes {
		line, ok := decisionQuestion(text)
		if !ok || line == "" {
			t.Fatalf("missed %q", text)
		}
	}
	if line, ok := decisionQuestion("Working on it.\n\nWhich direction do you want: A, B, or C?"); !ok || line != "Which direction do you want: A, B, or C?" {
		t.Fatalf("line %q ok %v", line, ok)
	}
	// A "?" in an earlier paragraph is not the decision. The last paragraph
	// has no question and no phrase.
	if _, ok := decisionQuestion("Should I stop?\n\nI started the background shell."); ok {
		t.Fatal("counted a question that is not in the last paragraph")
	}
	if _, ok := decisionQuestion("The build is still running."); ok {
		t.Fatal("plain status was a question")
	}
	pane := questionPaneText("notes\n\nWhich option do you want?\n❯ \n────────────\n")
	if line, ok := decisionQuestion(pane); !ok || line != "Which option do you want?" {
		t.Fatalf("pane line %q ok %v text %q", line, ok, pane)
	}
}

func TestLastAssistantTextIgnoresALaterUserTurn(t *testing.T) {
	home := t.TempDir()
	sid := "assistant-then-user"
	path := writeQuestionTranscript(t, home, sid, "msg_1", "Which direction do you want?")
	if _, _, ok := lastAssistantText(path); !ok {
		t.Fatal("assistant text was not read")
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	user := `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"take A"}]}}` + "\n"
	if _, err := f.WriteString(user); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, _, ok := lastAssistantText(path); ok {
		t.Fatal("a later user turn still counted as a question")
	}
}

type questionRig struct {
	s        *server
	dir      string
	home     string
	now      time.Time
	rows     []Session
	captures map[string]string
	asked    []string
}

func newQuestionRig(t *testing.T) *questionRig {
	t.Helper()
	dir := t.TempDir()
	rig := &questionRig{
		s:        &server{},
		dir:      dir,
		home:     t.TempDir(),
		now:      time.Date(2026, 10, 14, 17, 0, 0, 0, time.UTC),
		captures: map[string]string{},
	}
	if err := os.WriteFile(filepath.Join(dir, "question-events.json"), []byte(`{"enabled":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "idle-events.json"), []byte(`{"enabled":true,"ignore":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	rig.s.question.configPath = filepath.Join(dir, "question-events.json")
	rig.s.question.statePath = filepath.Join(dir, "state", "question-events-state.json")
	rig.s.question.logPath = filepath.Join(dir, "state", "question-events.log")
	rig.s.question.now = func() time.Time { return rig.now }
	rig.s.idle.configPath = filepath.Join(dir, "idle-events.json")
	rig.s.idle.statePath = filepath.Join(dir, "state", "idle-events-state.json")
	rig.s.idle.logPath = filepath.Join(dir, "state", "idle-events.log")
	rig.s.idle.now = func() time.Time { return rig.now }
	rig.s.collect = func() ([]Session, error) {
		return append([]Session(nil), rig.rows...), nil
	}
	rig.s.question.capture = func(target string) (string, error) {
		text, ok := rig.captures[target]
		if !ok {
			return "", fmt.Errorf("no capture")
		}
		return text, nil
	}
	rig.s.question.deliver = func(_, text string) (string, string) {
		rig.asked = append(rig.asked, text)
		return "sent", "fleet"
	}
	return rig
}

func (rig *questionRig) tick(d time.Duration) {
	rig.now = rig.now.Add(d)
	rig.s.questionTick()
}

func writeQuestionTranscript(t *testing.T, home, sid, id, text string) string {
	t.Helper()
	dir := filepath.Join(home, ".claude", "projects", "repo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	msg := map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"id":   id,
			"role": "assistant",
			"content": []map[string]string{{
				"type": "text",
				"text": text,
			}},
		},
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, sid+".jsonl")
	if err := os.WriteFile(path, append(raw, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func questionRow(pid int, home, sid, status, tmux string) Session {
	return Session{PID: pid, SessionID: sid, Status: status, Tmux: tmux, Home: home}
}

func TestQuestionEventsShellQuestionFiresAtOneMinute(t *testing.T) {
	rig := newQuestionRig(t)
	sid := "shell-question"
	writeQuestionTranscript(t, rig.home, sid, "msg_shell", "The shell is still running.\n\nWhich direction do you want: A, B, or C?")
	rig.rows = []Session{
		fleetRow(),
		questionRow(4242, rig.home, sid, "shell", "work:0.0"),
	}
	rig.tick(0)
	rig.tick(59 * time.Second)
	if len(rig.asked) != 0 {
		t.Fatalf("sent before one minute: %q", rig.asked)
	}
	rig.tick(time.Second)
	want := fleetEventTag + ` question 4242 work:0.0 (1 min) "Which direction do you want: A, B, or C?"`
	if len(rig.asked) != 1 || rig.asked[0] != want {
		t.Fatalf("asked %#v, want %q", rig.asked, want)
	}
}

func TestQuestionEventsNoQuestionStaysQuiet(t *testing.T) {
	rig := newQuestionRig(t)
	sid := "shell-quiet"
	writeQuestionTranscript(t, rig.home, sid, "msg_quiet", "The build is still running.")
	rig.rows = []Session{
		fleetRow(),
		questionRow(77, rig.home, sid, "shell", "w2:0.0"),
	}
	rig.tick(0)
	for range 4 {
		rig.tick(30 * time.Second)
	}
	if len(rig.asked) != 0 {
		t.Fatalf("sent a question for a plain shell: %q", rig.asked)
	}
}

func TestQuestionEventsNoQuestionKeepsTheShellIdleWait(t *testing.T) {
	rig := newIdleRig(t, idleCfgOn)
	home := t.TempDir()
	sid := "shell-idle-wait"
	writeQuestionTranscript(t, home, sid, "msg_wait", "The build is still running.")
	row := worker(77, sid, "shell", "w2:0.0")
	row.Home = home
	rig.rows = []Session{fleetRow(), row}
	rig.s.question.configPath = filepath.Join(rig.dir, "question-events.json")
	rig.s.question.statePath = filepath.Join(rig.dir, "state", "question-events-state.json")
	rig.s.question.logPath = filepath.Join(rig.dir, "state", "question-events.log")
	rig.s.question.now = func() time.Time { return rig.now }
	if err := os.WriteFile(rig.s.question.configPath, []byte(`{"enabled":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	both := func(d time.Duration) {
		rig.now = rig.now.Add(d)
		rig.s.questionTick()
		rig.s.idleTick()
	}
	both(0)
	for el := time.Duration(0); el < 1770*time.Second; el += 30 * time.Second {
		both(30 * time.Second)
	}
	if len(rig.s.idle.batch) != 0 || len(rig.typed) != 0 {
		t.Fatalf("shell reported before 1800 s: batch %+v typed %q", rig.s.idle.batch, rig.typed)
	}
	both(30 * time.Second)
	if len(rig.s.idle.batch) != 1 || rig.s.idle.batch[0].Status != "shell" {
		t.Fatalf("batch %+v", rig.s.idle.batch)
	}
}

func TestQuestionEventsSameQuestionDoesNotRepeat(t *testing.T) {
	rig := newQuestionRig(t)
	sid := "same-question"
	writeQuestionTranscript(t, rig.home, sid, "msg_same", "Which option should I take?")
	rig.rows = []Session{questionRow(12, rig.home, sid, "shell", "work:0.0")}
	rig.tick(0)
	rig.tick(60 * time.Second)
	if len(rig.asked) != 1 {
		t.Fatalf("first send = %d (%q)", len(rig.asked), rig.asked)
	}
	rig.tick(30 * time.Second)
	rig.tick(60 * time.Second)
	rig.tick(5 * time.Minute)
	if len(rig.asked) != 1 {
		t.Fatalf("repeated the same question: %q", rig.asked)
	}
	// A new assistant message is a new question and waits its own minute.
	writeQuestionTranscript(t, rig.home, sid, "msg_next", "Your decision: A, B, or C")
	rig.tick(time.Second)
	if len(rig.asked) != 1 {
		t.Fatalf("sent the new question before it had waited: %q", rig.asked)
	}
	rig.tick(60 * time.Second)
	if len(rig.asked) != 2 || !strings.Contains(rig.asked[1], "Your decision: A, B, or C") {
		t.Fatalf("new question: %q", rig.asked)
	}
}

func TestQuestionEventsGrokUsesThePane(t *testing.T) {
	rig := newQuestionRig(t)
	row := questionRow(9, rig.home, "grok-1", "shell", "grok-work:0.0")
	row.Tool = toolGrok
	rig.rows = []Session{row}
	rig.captures["grok-work:0.0"] = "still running\n\nWhich direction do you want: A, B, or C?\n❯ \n"
	rig.tick(0)
	rig.tick(60 * time.Second)
	want := fleetEventTag + ` question 9 grok-work:0.0 (1 min) "Which direction do you want: A, B, or C?"`
	if len(rig.asked) != 1 || rig.asked[0] != want {
		t.Fatalf("asked %#v, want %q", rig.asked, want)
	}
	rig.tick(2 * time.Minute)
	if len(rig.asked) != 1 {
		t.Fatalf("repeated the pane question: %q", rig.asked)
	}
}

func TestQuestionEventsPrefersQuestionOverIdle(t *testing.T) {
	rig := newIdleRig(t, idleCfgOn)
	home := t.TempDir()
	sid := "idle-and-question"
	writeQuestionTranscript(t, home, sid, "msg_both", "Which direction do you want: A, B, or C?")
	row := worker(42, sid, "idle", "work:0.0")
	row.Home = home
	rig.rows = []Session{fleetRow(), row}
	rig.s.question.configPath = filepath.Join(rig.dir, "question-events.json")
	rig.s.question.statePath = filepath.Join(rig.dir, "state", "question-events-state.json")
	rig.s.question.logPath = filepath.Join(rig.dir, "state", "question-events.log")
	rig.s.question.now = func() time.Time { return rig.now }
	if err := os.WriteFile(rig.s.question.configPath, []byte(`{"enabled":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var asked []string
	rig.s.question.deliver = func(_, text string) (string, string) {
		asked = append(asked, text)
		return "sent", "fleet"
	}
	both := func(d time.Duration) {
		rig.now = rig.now.Add(d)
		rig.s.questionTick()
		rig.s.idleTick()
	}
	both(0)
	both(60 * time.Second)
	if len(asked) != 1 || len(rig.typed) != 0 {
		t.Fatalf("at 1 min asked %q typed %q", asked, rig.typed)
	}
	if !strings.HasPrefix(asked[0], fleetEventTag+" question ") {
		t.Fatalf("not a question line: %q", asked[0])
	}
	// The idle line would leave at 120 s. It must not.
	both(60 * time.Second)
	both(60 * time.Second)
	if len(asked) != 1 || len(rig.typed) != 0 {
		t.Fatalf("double report asked %q typed %q batch %+v", asked, rig.typed, rig.s.idle.batch)
	}
	// A new final message is a new idle report. It waits its own minute.
	writeQuestionTranscript(t, home, sid, "msg_next", "The checks passed.")
	both(time.Second) // clock starts
	both(59 * time.Second)
	if len(rig.typed) != 0 {
		t.Fatalf("idle before the new message waited: %q", rig.typed)
	}
	both(60 * time.Second) // queued, then the batch window sends on the next minute
	both(60 * time.Second)
	if len(asked) != 1 || len(rig.typed) != 1 || !strings.Contains(rig.typed[0], "idle: 42 idle work:0.0") {
		t.Fatalf("new message asked %q typed %q", asked, rig.typed)
	}
}

func TestQuestionEventsSkipsFleetAndBatchesOneTick(t *testing.T) {
	rig := newQuestionRig(t)
	writeQuestionTranscript(t, rig.home, "fleet-sid", "msg_f", "Which direction do you want?")
	writeQuestionTranscript(t, rig.home, "a-sid", "msg_a", "Which option?")
	writeQuestionTranscript(t, rig.home, "b-sid", "msg_b", "Your decision?")
	fleet := fleetRow()
	fleet.Home = rig.home
	fleet.SessionID = "fleet-sid"
	rig.rows = []Session{
		fleet,
		questionRow(20, rig.home, "b-sid", "shell", "b:0.0"),
		questionRow(10, rig.home, "a-sid", "shell", "a:0.0"),
	}
	rig.tick(0)
	rig.tick(60 * time.Second)
	if len(rig.asked) != 1 {
		t.Fatalf("asked %q", rig.asked)
	}
	if strings.Contains(rig.asked[0], "fleet") || strings.Count(rig.asked[0], fleetEventTag) != 1 {
		t.Fatalf("line %q", rig.asked[0])
	}
	want := fleetEventTag + ` question 10 a:0.0 (1 min) "Which option?" ; 20 b:0.0 (1 min) "Your decision?"`
	if rig.asked[0] != want {
		t.Fatalf("asked %q, want %q", rig.asked[0], want)
	}
}

func TestQuestionEventsBusyIsNotAQuestion(t *testing.T) {
	rig := newQuestionRig(t)
	sid := "busy-then-shell"
	writeQuestionTranscript(t, rig.home, sid, "msg_b", "Which direction do you want?")
	row := questionRow(3, rig.home, sid, "busy", "work:0.0")
	rig.rows = []Session{row}
	rig.tick(0)
	rig.tick(60 * time.Second)
	if len(rig.asked) != 0 {
		t.Fatalf("sent while busy: %q", rig.asked)
	}
	row.Status = "shell"
	rig.rows = []Session{row}
	rig.tick(0)
	if len(rig.asked) != 0 {
		t.Fatalf("sent at the start of the shell state: %q", rig.asked)
	}
	rig.tick(60 * time.Second)
	if len(rig.asked) != 1 {
		t.Fatalf("asked %#v", rig.asked)
	}
}
