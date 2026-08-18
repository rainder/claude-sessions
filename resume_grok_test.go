package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// grokResumeSummary is a finished-session summary: kind omitted, title set,
// real cwd, branch and message count present. Timestamps are filled per test.
func grokResumeSummary(title, cwd, branch string, msgs int, lastActive, updated string) string {
	b, _ := json.Marshal(map[string]any{
		"info":            map[string]string{"cwd": cwd},
		"generated_title": title,
		"head_branch":     branch,
		"num_messages":    msgs,
		"last_active_at":  lastActive,
		"updated_at":      updated,
	})
	return string(b)
}

func writeGrokResumable(t *testing.T, home, cwd, sid string, mtime time.Time, body string) {
	t.Helper()
	grokSummaryFixture(t, home, cwd, sid, body)
	if mtime.IsZero() {
		return
	}
	if err := os.Chtimes(grokSummaryFile(t, home, cwd, sid), mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func grokSummaryFile(t *testing.T, home, cwd, sid string) string {
	t.Helper()
	p, ok := grokSummaryPath(home, cwd, sid)
	if !ok {
		t.Fatalf("summary for %s in %s not found", sid, cwd)
	}
	return p
}

func TestCollectResumableIncludesFinishedGrokSession(t *testing.T) {
	home := t.TempDir()
	now := time.Date(2026, 8, 14, 18, 0, 0, 0, time.UTC)
	const (
		sid    = "grok-sess-1"
		cwd    = "/work/trecs-brain"
		title  = "Ticket review"
		branch = "develop"
		msgs   = 138
	)
	active := "2026-08-14T17:00:00.000000000Z"
	writeGrokResumable(t, home, cwd, sid, now.Add(-time.Hour),
		grokResumeSummary(title, cwd, branch, msgs, active, "2026-08-14T16:00:00.000000000Z"))

	got := collectResumableFrom(home, nil, now)
	if len(got) != 1 {
		t.Fatalf("got %d sessions %v, want 1 grok row", len(got), ids(got))
	}
	s := got[0]
	if s.Tool != toolGrok {
		t.Errorf("Tool = %q, want %q", s.Tool, toolGrok)
	}
	if s.SessionID != sid {
		t.Errorf("SessionID = %q, want %q", s.SessionID, sid)
	}
	if s.Name != title {
		t.Errorf("Name = %q, want %q", s.Name, title)
	}
	if s.CWD != cwd {
		t.Errorf("CWD = %q, want %q", s.CWD, cwd)
	}
	if s.GitBranch != branch {
		t.Errorf("GitBranch = %q, want %q", s.GitBranch, branch)
	}
	if s.MessageCount != msgs {
		t.Errorf("MessageCount = %d, want %d", s.MessageCount, msgs)
	}
	if s.FirstPrompt != "" || len(s.Prompts) != 0 {
		t.Errorf("grok prompts = %q %v, want empty", s.FirstPrompt, s.Prompts)
	}
	wantMod := time.Date(2026, 8, 14, 17, 0, 0, 0, time.UTC)
	if !s.ModifiedAt.Equal(wantMod) {
		t.Errorf("ModifiedAt = %v, want last_active_at %v", s.ModifiedAt, wantMod)
	}
}

func TestCollectResumableSkipsGrokSubagent(t *testing.T) {
	home := t.TempDir()
	now := time.Date(2026, 8, 14, 18, 0, 0, 0, time.UTC)
	body := `{"info":{"cwd":"/work/a"},"generated_title":"child","session_kind":"subagent","num_messages":3}`
	writeGrokResumable(t, home, "/work/a", "grok-sub-1", now.Add(-time.Hour), body)

	got := collectResumableFrom(home, nil, now)
	if len(got) != 0 {
		t.Fatalf("got %d sessions %v, want none (subagent)", len(got), ids(got))
	}
}

func TestCollectResumableSkipsLiveGrokIDs(t *testing.T) {
	home := t.TempDir()
	now := time.Date(2026, 8, 14, 18, 0, 0, 0, time.UTC)
	const sid = "grok-live-1"
	writeGrokResumable(t, home, "/work/a", sid, now.Add(-time.Hour),
		grokResumeSummary("live one", "/work/a", "main", 2, "", ""))

	got := collectResumableFrom(home, map[string]bool{sid: true}, now)
	if len(got) != 0 {
		t.Fatalf("got %d sessions %v, want none (live)", len(got), ids(got))
	}
}

func TestCollectResumableSkipsOldGrokSessions(t *testing.T) {
	home := t.TempDir()
	now := time.Date(2026, 8, 14, 18, 0, 0, 0, time.UTC)
	old := now.Add(-40 * 24 * time.Hour)
	recent := now.Add(-time.Hour)

	// Cheap pass: mtime older than resumableMaxAge.
	writeGrokResumable(t, home, "/work/old-mtime", "grok-old-m", old,
		grokResumeSummary("old mtime", "/work/old-mtime", "main", 1, now.Format(time.RFC3339Nano), ""))

	// Content pass: recent mtime, but last_active_at / updated_at are stale.
	writeGrokResumable(t, home, "/work/old-ts", "grok-old-t", recent,
		grokResumeSummary("old stamps", "/work/old-ts", "main", 1,
			old.Format(time.RFC3339Nano), old.Format(time.RFC3339Nano)))

	got := collectResumableFrom(home, nil, now)
	if len(got) != 0 {
		t.Fatalf("got %d sessions %v, want none (older than resumableMaxAge)", len(got), ids(got))
	}
}

func TestCollectResumableSkipsGrokScratchCWD(t *testing.T) {
	home := t.TempDir()
	now := time.Date(2026, 8, 14, 18, 0, 0, 0, time.UTC)
	writeGrokResumable(t, home, "/tmp/scratch", "grok-tmp-1", now.Add(-time.Hour),
		grokResumeSummary("temp work", "/tmp/scratch", "main", 1, "", ""))

	got := collectResumableFrom(home, nil, now)
	if len(got) != 0 {
		t.Fatalf("got %d sessions %v, want none (scratch cwd)", len(got), ids(got))
	}
}

func TestCollectResumableSkipsBadGrokSummariesWithoutDroppingClaude(t *testing.T) {
	home := t.TempDir()
	now := time.Date(2026, 8, 14, 18, 0, 0, 0, time.UTC)

	writeResumableTranscript(t, home, "proj", "claude-ok", now.Add(-time.Hour),
		`{"type":"attachment","cwd":"/home/u/proj"}`,
		`{"type":"user","message":{"role":"user","content":"hi"}}`,
	)

	// Missing summary.json: session dir only.
	miss := filepath.Join(home, grokDir, "sessions", "%2Fwork%2Fmiss", "grok-miss")
	if err := os.MkdirAll(miss, 0o755); err != nil {
		t.Fatal(err)
	}

	// Unparseable summary.
	writeGrokResumable(t, home, "/work/bad", "grok-bad", now.Add(-time.Hour), `{this is not json`)

	// Unreadable summary.
	writeGrokResumable(t, home, "/work/deny", "grok-deny", now.Add(-time.Hour),
		grokResumeSummary("hidden", "/work/deny", "main", 1, "", ""))
	deny := grokSummaryFile(t, home, "/work/deny", "grok-deny")
	if err := os.Chmod(deny, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(deny, 0o644) })

	got := collectResumableFrom(home, nil, now)
	if len(got) != 1 || got[0].SessionID != "claude-ok" || got[0].Tool != "" {
		t.Fatalf("got %v, want the one Claude row (bad grok summaries skipped)", ids(got))
	}
}

func TestCollectResumableMergeCapIsRecencyAcrossTools(t *testing.T) {
	now := time.Date(2026, 8, 14, 18, 0, 0, 0, time.UTC)

	t.Run("newer grok wins", func(t *testing.T) {
		home := t.TempDir()
		writeResumableTranscript(t, home, "proj", "claude-old", now.Add(-2*time.Hour),
			`{"type":"attachment","cwd":"/home/u/proj"}`,
			`{"type":"user","message":{"role":"user","content":"old claude"}}`,
		)
		writeGrokResumable(t, home, "/work/g", "grok-new", now.Add(-time.Hour),
			grokResumeSummary("new grok", "/work/g", "main", 4, now.Add(-time.Hour).Format(time.RFC3339Nano), ""))

		got := collectResumableFromLimited(home, nil, now, 1)
		if len(got) != 1 || got[0].SessionID != "grok-new" || got[0].Tool != toolGrok {
			t.Fatalf("got %v tool=%q, want grok-new", ids(got), toolOf(got))
		}
	})

	t.Run("newer claude wins", func(t *testing.T) {
		home := t.TempDir()
		writeResumableTranscript(t, home, "proj", "claude-new", now.Add(-time.Hour),
			`{"type":"attachment","cwd":"/home/u/proj"}`,
			`{"type":"user","message":{"role":"user","content":"new claude"}}`,
		)
		writeGrokResumable(t, home, "/work/g", "grok-old", now.Add(-2*time.Hour),
			grokResumeSummary("old grok", "/work/g", "main", 4, now.Add(-2*time.Hour).Format(time.RFC3339Nano), ""))

		got := collectResumableFromLimited(home, nil, now, 1)
		if len(got) != 1 || got[0].SessionID != "claude-new" || got[0].Tool != "" {
			t.Fatalf("got %v tool=%q, want claude-new", ids(got), toolOf(got))
		}
	})
}

func toolOf(ss []ResumableSession) string {
	if len(ss) == 0 {
		return ""
	}
	return ss[0].Tool
}

func TestResumeGrokSessionArgv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	logPath := installFakeTmux(t)
	const (
		sid = "aaaa-g111"
		cwd = "/work/proj"
	)
	grokSummaryFixture(t, home, cwd, sid, grokResumeSummary("n", cwd, "main", 1, "", ""))

	if _, err := ResumeGrokSession(sid, cwd); err != nil {
		t.Fatal(err)
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(log)
	if !strings.Contains(got, "grok --resume "+sid) {
		t.Errorf("tmux log = %q, want grok --resume %s", got, sid)
	}
	if strings.Contains(got, "claude --resume") {
		t.Errorf("tmux log = %q, must not contain claude --resume", got)
	}
	if strings.Contains(got, "--cwd") || strings.Contains(got, "--worktree") || strings.Contains(got, "--restore-code") {
		t.Errorf("tmux log = %q, grok resume must not pass --cwd/--worktree/--restore-code", got)
	}
	if !strings.Contains(got, "<-c><"+cwd+">") {
		t.Errorf("tmux log = %q, want new-session -c %s", got, cwd)
	}
}

func TestResumeGrokSessionRefusesMissingSummary(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	logPath := installFakeTmux(t)

	_, err := ResumeGrokSession("aaaa-g222", "/work/proj")
	if err == nil {
		t.Fatal("expected error when summary is missing")
	}
	log, _ := os.ReadFile(logPath)
	if len(log) != 0 {
		t.Errorf("tmux log = %q, want no tmux calls for a missing summary", log)
	}
}

func TestResumeSessionStillRequiresClaudeTranscript(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	logPath := installFakeTmux(t)
	const (
		sid = "aaaa-g333"
		cwd = "/work/proj"
	)
	grokSummaryFixture(t, home, cwd, sid, grokResumeSummary("n", cwd, "main", 1, "", ""))

	_, err := ResumeSession(sid, cwd)
	if err == nil {
		t.Fatal("expected ResumeSession to refuse a grok-only id")
	}
	log, _ := os.ReadFile(logPath)
	if strings.Contains(string(log), "claude --resume") {
		t.Errorf("tmux log = %q, grok-only id must not start claude --resume", log)
	}
}

func TestLiveSessionIDsIncludesGrokWhenClaudeFileWinsThePID(t *testing.T) {
	allPIDsAlive(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	now := time.Date(2026, 8, 14, 18, 0, 0, 0, time.UTC)

	writeClaudeSessionFile(t, home, Session{
		PID: 137851, SessionID: "claude-sess", CWD: "/work/a",
		Status: "idle", Entrypoint: "cli", Name: "mine", NameSource: "user",
	})
	grokFixture(t, home, grokActiveOne)
	writeGrokResumable(t, home, "/work/trecs-brain", grokActiveOneID, now.Add(-time.Hour),
		grokResumeSummary("live grok", "/work/trecs-brain", "main", 4, "", ""))

	live := liveSessionIDs()
	if !live[grokActiveOneID] {
		t.Fatalf("liveSessionIDs missing %s after CollectLocal dropped the grok row", grokActiveOneID)
	}

	got := collectResumableFrom(home, live, now)
	for _, s := range got {
		if s.SessionID == grokActiveOneID {
			t.Fatalf("picker listed live grok %s; resume would start a second process", grokActiveOneID)
		}
	}
}

func TestResumeGrokSessionRejectsUnsafeIDBeforeLookup(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	logPath := installFakeTmux(t)

	_, err := ResumeGrokSession("../evil", "/work/proj")
	if err == nil || !strings.Contains(err.Error(), "invalid session id") {
		t.Fatalf("err = %v, want invalid session id", err)
	}
	log, _ := os.ReadFile(logPath)
	if len(log) != 0 {
		t.Errorf("tmux log = %q, want no tmux calls", log)
	}
}

func TestResumeSessionRejectsUnsafeIDBeforeGlob(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	logPath := installFakeTmux(t)

	_, err := ResumeSession("*", "/work/proj")
	if err == nil || !strings.Contains(err.Error(), "invalid session id") {
		t.Fatalf("err = %v, want invalid session id before findTranscript", err)
	}
	log, _ := os.ReadFile(logPath)
	if len(log) != 0 {
		t.Errorf("tmux log = %q, want no tmux calls", log)
	}
}

func TestResumeHandlerEmptyToolStaysClaude(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	logPath := installFakeTmux(t)
	writeResumableTranscript(t, home, "proj", "aaaa-1111", time.Now(),
		`{"cwd":"/srv/app","type":"user","message":{"role":"user","content":"hi"}}`)

	body, _ := json.Marshal(map[string]string{
		"session_id": "aaaa-1111",
		"cwd":        "/srv/app",
	})
	req := httptest.NewRequest(http.MethodPost, "/sessions/resume", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	(&server{token: "test-token"}).resume(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), "claude --resume aaaa-1111") {
		t.Errorf("tmux log = %q, empty tool must spawn claude --resume", log)
	}
	if strings.Contains(string(log), "grok --resume") {
		t.Errorf("tmux log = %q, empty tool must not spawn grok --resume", log)
	}
}

func TestResumeGrokSessionRejectsRelativeCWD(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	logPath := installFakeTmux(t)
	const sid = "aaaa-g555"
	grokSummaryFixture(t, home, "..", sid, grokResumeSummary("n", "..", "main", 1, "", ""))

	_, err := ResumeGrokSession(sid, "..")
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("err = %v, want cwd must be absolute", err)
	}
	if _, ok := grokSummaryPath(home, "..", sid); ok {
		t.Fatal("grokSummaryPath accepted cwd=.. and walked out of sessions")
	}
	log, _ := os.ReadFile(logPath)
	if len(log) != 0 {
		t.Errorf("tmux log = %q, want no tmux calls", log)
	}
}

func TestCollectResumableSkipsGrokWhenRegistryUnreadable(t *testing.T) {
	home := t.TempDir()
	now := time.Date(2026, 8, 14, 18, 0, 0, 0, time.UTC)
	writeGrokResumable(t, home, "/work/a", "grok-ok-1", now.Add(-time.Hour),
		grokResumeSummary("ok", "/work/a", "main", 2, "", ""))
	grokFixture(t, home, `{this is not a registry`)

	got := collectResumableFrom(home, nil, now)
	if len(got) != 0 {
		t.Fatalf("got %v, want no grok rows when the registry is torn", ids(got))
	}
}

func TestResumeGrokSessionRefusesUnreadableRegistry(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	logPath := installFakeTmux(t)
	const (
		sid = "aaaa-g666"
		cwd = "/work/proj"
	)
	grokSummaryFixture(t, home, cwd, sid, grokResumeSummary("n", cwd, "main", 1, "", ""))
	grokFixture(t, home, `{nope`)

	_, err := ResumeGrokSession(sid, cwd)
	if err == nil || !strings.Contains(err.Error(), "registry") {
		t.Fatalf("err = %v, want registry unreadable", err)
	}
	log, _ := os.ReadFile(logPath)
	if len(log) != 0 {
		t.Errorf("tmux log = %q, want no tmux calls", log)
	}
}

func TestResumeHandlerRejectsUnknownTool(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/sessions/resume", strings.NewReader(
		`{"session_id":"aaaa-1111","cwd":"/srv/app","tool":"codex"}`))
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	(&server{token: "test-token"}).resume(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want 400", rec.Code, rec.Body.String())
	}
}

func TestResumeHandlerDispatchesGrokTool(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	logPath := installFakeTmux(t)
	const (
		sid = "aaaa-g444"
		cwd = "/work/proj"
	)
	grokSummaryFixture(t, home, cwd, sid, grokResumeSummary("n", cwd, "main", 1, "", ""))

	body, _ := json.Marshal(map[string]string{
		"session_id": sid,
		"cwd":        cwd,
		"tool":       toolGrok,
	})
	req := httptest.NewRequest(http.MethodPost, "/sessions/resume", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	(&server{token: "test-token"}).resume(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var got actionResult
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if !got.OK || got.Tmux == "" {
		t.Fatalf("result = %#v", got)
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), "grok --resume "+sid) {
		t.Errorf("tmux log = %q, want grok --resume (tool=grok dispatch)", log)
	}
	if strings.Contains(string(log), "claude --resume") {
		t.Errorf("tmux log = %q, tool=grok must not spawn claude --resume", log)
	}
}
