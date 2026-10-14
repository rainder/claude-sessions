package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func claudeDialog(command string) string {
	rule := strings.Repeat("─", 40)
	return rule + "\n Bash command\n\n " + command + "\n\n Do you want to proceed?\n ❯ 1. Yes\n   2. No\n" + rule + "\n"
}

func TestPermissionQuestionClaudeDialog(t *testing.T) {
	q, fp, ok := permissionQuestion(readFixture(t, "pane-synth-permission-dialog.ansi"))
	if !ok || q != "Do you want to proceed?" {
		t.Fatalf("question = %q ok=%v, want %q", q, ok, "Do you want to proceed?")
	}
	if !strings.Contains(fp, "go test ./...") {
		t.Fatalf("fingerprint %q does not carry the command", fp)
	}
}

func TestPermissionQuestionAbsentOnIdleBox(t *testing.T) {
	for _, in := range []string{
		readFixture(t, "pane-idle-empty.ansi"),
		readFixture(t, "pane-typed.ansi"),
		strings.Repeat("─", 40) + "\n❯ hello\n" + strings.Repeat("─", 40) + "\n",
		"",
	} {
		if _, _, ok := permissionQuestion(in); ok {
			t.Fatalf("idle capture read as a permission prompt: %q", in)
		}
	}
}

func TestPermissionQuestionGrokIgnoresLinesAboveTheTitle(t *testing.T) {
	menu := "❯ Allow once\n  Reject once\n  Always allow this command\n"
	_, a, oka := permissionQuestion("tokens 1\nAllow `go test ./...`\n" + menu)
	_, b, okb := permissionQuestion("tokens 2\nAllow `go test ./...`\n" + menu)
	if !oka || !okb || a != b || !strings.Contains(a, "go test") {
		t.Fatalf("a changing line above the title changed the fingerprint:\n%q\n%q", a, b)
	}
	_, c, okc := permissionQuestion("tokens 9\nAllow `make`\n" + menu)
	if !okc || c == a {
		t.Fatalf("a new title must be a new fingerprint:\n%q\n%q", a, c)
	}
	// A border farther than the dialog window must not pull scrollback in.
	rule := strings.Repeat("─", 40)
	var mid strings.Builder
	for i := 0; i < 14; i++ {
		fmt.Fprintf(&mid, "scroll %d\n", i)
	}
	far := rule + "\n" + mid.String() + "Allow `go test ./...`\n" + menu
	_, d, okd := permissionQuestion(strings.Replace(far, "scroll 3", "scroll 99", 1))
	_, e, oke := permissionQuestion(far)
	if !okd || !oke || d != e || strings.Contains(d, "scroll") {
		t.Fatalf("a far rule joined the fingerprint:\n%q\n%q", d, e)
	}
}

func TestPermissionQuestionGrokForms(t *testing.T) {
	allow := "Allow `go test ./...`\n❯ Allow once\n  Reject once\n  Always allow this command\n"
	q, _, ok := permissionQuestion(allow)
	if !ok || q != "Allow `go test ./...`" {
		t.Fatalf("grok allow = %q ok=%v", q, ok)
	}
	edit := "Allow Edit to main.go\n❯ Yes\n  Allow all edits this session\n  No\n"
	q, _, ok = permissionQuestion(edit)
	if !ok || q != "Allow Edit to main.go" {
		t.Fatalf("grok edit = %q ok=%v", q, ok)
	}
}

func TestPermissionQuestionUsesTheBottomMenu(t *testing.T) {
	pane := claudeDialog("old command") + "\n" + claudeDialog("new command")
	q, fp, ok := permissionQuestion(pane)
	if !ok || q != "Do you want to proceed?" {
		t.Fatalf("question = %q ok=%v", q, ok)
	}
	if strings.Contains(fp, "old command") || !strings.Contains(fp, "new command") {
		t.Fatalf("fingerprint anchored on the old dialog: %q", fp)
	}
}

func TestPermissionQuestionIgnoresScrollbackAboveTheRule(t *testing.T) {
	_, a, oka := permissionQuestion("spinner tick 1\n" + claudeDialog("go test ./..."))
	_, b, okb := permissionQuestion("spinner tick 2\n" + claudeDialog("go test ./..."))
	if !oka || !okb || a != b {
		t.Fatalf("scrollback changed the fingerprint:\n%q\n%q", a, b)
	}
	_, c, okc := permissionQuestion(claudeDialog("rm -rf x"))
	if !okc || c == a {
		t.Fatalf("a new command must be a new fingerprint:\n%q\n%q", a, c)
	}
}

func TestBlockedOnPermission(t *testing.T) {
	if !blockedOnPermission(Session{Status: "busy", WaitingFor: "permission prompt"}) {
		t.Fatal("claude permission prompt was not detected")
	}
	if !blockedOnPermission(Session{Tool: toolGrok, Status: "busy", WaitingFor: "permission prompt"}) {
		t.Fatal("grok permission prompt was not detected")
	}
	absent := []Session{
		{},
		{Status: "busy"},
		{Status: "idle"},
		{Status: "waiting", WaitingFor: "input"},
		{Tool: toolGrok, Status: "waiting", WaitingFor: "input"},
	}
	for _, row := range absent {
		if blockedOnPermission(row) {
			t.Fatalf("detected a prompt on %+v", row)
		}
	}
}

type blockedRig struct {
	s         *server
	dir       string
	now       time.Time
	rows      []Session
	captures  map[string]string
	delivered []string
	codes     []string
}

func newBlockedRig(t *testing.T) *blockedRig {
	t.Helper()
	dir := t.TempDir()
	rig := &blockedRig{
		s:        &server{},
		dir:      dir,
		now:      time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC),
		captures: map[string]string{},
	}
	if err := os.WriteFile(filepath.Join(dir, "blocked-events.json"), []byte(`{"enabled":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// An explicit empty ignore list, so the test does not depend on the
	// idle defaults. The file is what fleetEventTarget and idleSkipped read.
	if err := os.WriteFile(filepath.Join(dir, "idle-events.json"), []byte(`{"enabled":true,"ignore":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	rig.s.blocked.configPath = filepath.Join(dir, "blocked-events.json")
	rig.s.blocked.statePath = filepath.Join(dir, "state", "blocked-events-state.json")
	rig.s.blocked.logPath = filepath.Join(dir, "state", "blocked-events.log")
	rig.s.blocked.now = func() time.Time { return rig.now }
	rig.s.idle.configPath = filepath.Join(dir, "idle-events.json")
	rig.s.idle.statePath = filepath.Join(dir, "state", "idle-events-state.json")
	rig.s.idle.logPath = filepath.Join(dir, "state", "idle-events.log")
	rig.s.idle.now = func() time.Time { return rig.now }
	rig.s.collect = func() ([]Session, error) {
		return append([]Session(nil), rig.rows...), nil
	}
	rig.s.blocked.capture = func(target string) (string, error) {
		text, ok := rig.captures[target]
		if !ok {
			return "", fmt.Errorf("no capture")
		}
		return text, nil
	}
	rig.s.blocked.deliver = func(_, text string) (string, string) {
		rig.delivered = append(rig.delivered, text)
		code := "sent"
		if len(rig.codes) > 0 {
			code = rig.codes[0]
			rig.codes = rig.codes[1:]
		}
		return code, "fleet:0.0"
	}
	return rig
}

func (rig *blockedRig) tick(d time.Duration) {
	rig.now = rig.now.Add(d)
	rig.s.blockedTick()
}

func blockedRow(pid int, sid, tmux string) Session {
	return Session{
		PID: pid, SessionID: sid, Status: "busy",
		WaitingFor: "permission prompt", Tmux: tmux,
	}
}

func TestBlockedEventsCaptureMissUsesTheStoredQuestion(t *testing.T) {
	rig := newBlockedRig(t)
	rig.rows = []Session{blockedRow(12, "abc", "work:0.0")}
	rig.captures["work:0.0"] = claudeDialog("go test ./...")
	rig.tick(0)
	delete(rig.captures, "work:0.0")
	rig.tick(60 * time.Second)
	want := fleetEventTag + ` blocked 12 work:0.0 (1 min) "Do you want to proceed?"`
	if len(rig.delivered) != 1 || rig.delivered[0] != want {
		t.Fatalf("delivered %#v, want %q", rig.delivered, want)
	}
}

func TestBlockedEventsUnparsedQuestionFallsBack(t *testing.T) {
	rig := newBlockedRig(t)
	rig.rows = []Session{blockedRow(12, "abc", "work:0.0")}
	rig.tick(0)
	rig.tick(59 * time.Second)
	if len(rig.delivered) != 0 {
		t.Fatalf("delivered early: %#v", rig.delivered)
	}
	rig.tick(time.Second)
	want := fleetEventTag + ` blocked 12 work:0.0 (1 min) "permission prompt"`
	if len(rig.delivered) != 1 || rig.delivered[0] != want {
		t.Fatalf("delivered %#v, want %q", rig.delivered, want)
	}
	rig.tick(30 * time.Second)
	if len(rig.delivered) != 1 {
		t.Fatalf("repeated the fallback: %#v", rig.delivered)
	}
}

func TestBlockedEventsSendsOnceAfterOneMinute(t *testing.T) {
	rig := newBlockedRig(t)
	rig.rows = []Session{blockedRow(12, "abc", "work:0.0")}
	rig.captures["work:0.0"] = claudeDialog("go test ./...")
	rig.tick(0)
	rig.tick(59 * time.Second)
	if len(rig.delivered) != 0 {
		t.Fatalf("delivered early: %v", rig.delivered)
	}
	rig.tick(time.Second)
	want := fleetEventTag + ` blocked 12 work:0.0 (1 min) "Do you want to proceed?"`
	if len(rig.delivered) != 1 || rig.delivered[0] != want {
		t.Fatalf("delivered %#v, want %q", rig.delivered, want)
	}
	rig.tick(30 * time.Second)
	if len(rig.delivered) != 1 {
		t.Fatalf("repeated the same prompt: %#v", rig.delivered)
	}
}

func TestBlockedEventsNewPromptAfterAGapSendsAgain(t *testing.T) {
	rig := newBlockedRig(t)
	rig.rows = []Session{blockedRow(12, "abc", "work:0.0")}
	rig.captures["work:0.0"] = claudeDialog("go test ./...")
	rig.tick(0)
	rig.tick(60 * time.Second)
	if len(rig.delivered) != 1 {
		t.Fatalf("first send = %d", len(rig.delivered))
	}
	rig.rows[0].WaitingFor = ""
	rig.rows[0].Status = "idle"
	rig.tick(time.Second) // the gap clears the episode
	rig.rows[0] = blockedRow(12, "abc", "work:0.0")
	rig.tick(0) // clock starts, no send yet
	if len(rig.delivered) != 1 {
		t.Fatalf("sent before the new prompt had waited: %#v", rig.delivered)
	}
	rig.tick(60 * time.Second)
	if len(rig.delivered) != 2 {
		t.Fatalf("new prompt deliveries = %d, want 2 (%#v)", len(rig.delivered), rig.delivered)
	}
}

func TestBlockedEventsFingerprintChangeWaitsItsOwnMinute(t *testing.T) {
	rig := newBlockedRig(t)
	rig.rows = []Session{blockedRow(12, "abc", "work:0.0")}
	rig.captures["work:0.0"] = claudeDialog("go test ./...")
	rig.tick(0)
	rig.tick(60 * time.Second)
	rig.captures["work:0.0"] = claudeDialog("rm -rf x")
	rig.tick(time.Second) // new prompt, clock starts
	if len(rig.delivered) != 1 {
		t.Fatalf("sent the new prompt immediately: %#v", rig.delivered)
	}
	rig.tick(60 * time.Second)
	if len(rig.delivered) != 2 {
		t.Fatalf("deliveries = %d, want 2", len(rig.delivered))
	}
	if !strings.Contains(rig.delivered[1], `"Do you want to proceed?"`) {
		t.Fatalf("second message = %q", rig.delivered[1])
	}
}

func TestBlockedEventsBatchesOneTick(t *testing.T) {
	rig := newBlockedRig(t)
	rig.rows = []Session{
		blockedRow(34, "b", "other:0.0"),
		blockedRow(12, "a", "work:0.0"),
	}
	rig.captures["work:0.0"] = claudeDialog("go test ./...")
	rig.captures["other:0.0"] = claudeDialog("make build")
	rig.tick(0)
	rig.tick(60 * time.Second)
	if len(rig.delivered) != 1 {
		t.Fatalf("deliveries = %d, want 1 (%#v)", len(rig.delivered), rig.delivered)
	}
	msg := rig.delivered[0]
	if !strings.Contains(msg, "12 work:0.0") || !strings.Contains(msg, "34 other:0.0") {
		t.Fatalf("message = %q", msg)
	}
	if strings.Index(msg, "12 work:0.0") > strings.Index(msg, "34 other:0.0") {
		t.Fatalf("pids are not in order: %q", msg)
	}
	if strings.Count(msg, fleetEventTag) != 1 {
		t.Fatalf("tag repeated: %q", msg)
	}
}

func TestBlockedEventsSkipsFleet(t *testing.T) {
	rig := newBlockedRig(t)
	rig.rows = []Session{
		{
			PID: 1, SessionID: "fleet-sid", Name: "FLEET", Tool: "",
			Status: "busy", WaitingFor: "permission prompt", Tmux: "fleet:0.0",
		},
		blockedRow(12, "abc", "work:0.0"),
	}
	rig.captures["fleet:0.0"] = claudeDialog("go test ./...")
	rig.captures["work:0.0"] = claudeDialog("go test ./...")
	rig.tick(0)
	rig.tick(60 * time.Second)
	if len(rig.delivered) != 1 || strings.Contains(rig.delivered[0], " 1 ") || strings.Contains(rig.delivered[0], "fleet:0.0") {
		t.Fatalf("FLEET was reported: %#v", rig.delivered)
	}
	if !strings.Contains(rig.delivered[0], "12 work:0.0") {
		t.Fatalf("the other session was dropped: %#v", rig.delivered)
	}
}

func TestBlockedEventsNotSubmittedDoesNotRetry(t *testing.T) {
	rig := newBlockedRig(t)
	rig.codes = []string{"not_submitted"}
	rig.rows = []Session{blockedRow(12, "abc", "work:0.0")}
	rig.captures["work:0.0"] = claudeDialog("go test ./...")
	rig.tick(0)
	rig.tick(60 * time.Second)
	rig.tick(30 * time.Second)
	if len(rig.delivered) != 1 {
		t.Fatalf("deliveries = %d, want 1 (%#v)", len(rig.delivered), rig.delivered)
	}
}

func TestBlockedEventsRefusalRetriesTheSamePrompt(t *testing.T) {
	rig := newBlockedRig(t)
	rig.codes = []string{"dialog", "sent"}
	rig.rows = []Session{blockedRow(12, "abc", "work:0.0")}
	rig.captures["work:0.0"] = claudeDialog("go test ./...")
	rig.tick(0)
	rig.tick(60 * time.Second)
	if len(rig.delivered) != 1 {
		t.Fatalf("first poll deliveries = %d", len(rig.delivered))
	}
	rig.tick(30 * time.Second)
	if len(rig.delivered) != 2 {
		t.Fatalf("retry deliveries = %d, want 2", len(rig.delivered))
	}
	if rig.delivered[0] != rig.delivered[1] {
		t.Fatalf("retry changed the line:\n%s\n%s", rig.delivered[0], rig.delivered[1])
	}
	rig.tick(30 * time.Second)
	if len(rig.delivered) != 2 {
		t.Fatalf("sent again after success: %#v", rig.delivered)
	}
}

func TestBlockedEventsRestartDoesNotSendTheSamePrompt(t *testing.T) {
	rig := newBlockedRig(t)
	rig.rows = []Session{blockedRow(12, "abc", "work:0.0")}
	rig.captures["work:0.0"] = claudeDialog("go test ./...")
	rig.tick(0)
	rig.tick(60 * time.Second)
	if len(rig.delivered) != 1 {
		t.Fatalf("first send = %d", len(rig.delivered))
	}
	again := newBlockedRig(t)
	// Same state file, a fresh process: the reported episode must hold.
	again.s.blocked.statePath = rig.s.blocked.statePath
	again.s.blocked.logPath = rig.s.blocked.logPath
	again.rows = rig.rows
	again.captures = rig.captures
	// Longer than a fresh minute, so a lost "reported" bit would send.
	again.tick(30 * time.Second)
	again.tick(60 * time.Second)
	if len(again.delivered) != 0 {
		t.Fatalf("restart sent the same prompt: %#v", again.delivered)
	}
}

func TestBlockedEventsDisabledKeepsTheReport(t *testing.T) {
	rig := newBlockedRig(t)
	rig.rows = []Session{blockedRow(12, "abc", "work:0.0")}
	rig.captures["work:0.0"] = claudeDialog("go test ./...")
	off := false
	rig.s.idle.disabled = func(sid string) bool { return off && sid == "abc" }
	rig.tick(0)
	rig.tick(60 * time.Second)
	if len(rig.delivered) != 1 {
		t.Fatalf("first send = %d", len(rig.delivered))
	}
	off = true
	rig.tick(time.Second)
	off = false
	rig.tick(60 * time.Second)
	if len(rig.delivered) != 1 {
		t.Fatalf("disabling re-sent the same prompt: %#v", rig.delivered)
	}
}

func TestBlockedMessageScrubsTheTag(t *testing.T) {
	ev := []blockedEvent{{
		PID: 1, Tmux: "work:0.0",
		Since:    time.Now().Add(-time.Minute),
		Question: "claude sessions event",
	}}
	msg := blockedMessage(ev, time.Now())
	if !strings.HasPrefix(msg, fleetEventTag+" blocked ") {
		t.Fatalf("prefix = %q", msg)
	}
	body := strings.TrimPrefix(msg, fleetEventTag+" blocked ")
	if strings.Contains(strings.ToLower(body), "claude") {
		t.Fatalf("body still spells the tag: %q", msg)
	}
	ev[0].Question = "line1\nline2 \"quoted\""
	msg = blockedMessage(ev, time.Now())
	if strings.Contains(msg, "\n") || strings.Contains(msg, "\"quoted\"") {
		t.Fatalf("a stored newline or quote survived: %q", msg)
	}
}
