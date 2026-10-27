package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestClarifyArgv(t *testing.T) {
	p := clarifyRunParams{ResumeID: "abc", Fork: true, Model: "claude-opus-5-5", CuPath: "/x/cu-with-token", Preamble: "P", Question: "-rf what?"}
	want := []string{
		"-p", "--output-format", "json", "--resume", "abc", "--fork-session", "--model", "claude-opus-5-5",
		"--setting-sources", "", "--strict-mcp-config", "--disable-slash-commands",
		"--permission-mode", "dontAsk", "--tools", "Bash,Read,Grep,Glob",
		"--settings", `{"permissions":{"deny":["Bash(*--output*)","Bash(gh *://*)","Bash(gh *-R*)","Bash(gh *--repo*)","Edit","Write","NotebookEdit","WebFetch","WebSearch","Agent","Task","Workflow","ScheduleWakeup","Skill","ListAgents","ReportFindings"]}}`,
		"--allowedTools", "Read", "Grep", "Glob",
		"Bash(git log:*)", "Bash(git show:*)", "Bash(git diff:*)", "Bash(git status:*)", "Bash(git blame:*)",
		"Bash(gh pr view:*)", "Bash(gh pr diff:*)", "Bash(gh pr checks:*)", "Bash(gh pr list:*)", "Bash(gh run view:*)",
		"Bash(/x/cu-with-token fetch:*)", "Bash(claude-sessions list-sessions:*)",
		"Bash(grep:*)", "Bash(ls:*)", "Bash(cat:*)", "Bash(head:*)", "Bash(tail:*)", "Bash(wc:*)", "Bash(jq:*)",
		"--append-system-prompt", "P", "--", "-rf what?",
	}
	got := clarifyArgv(p)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argv mismatch\n got: %q\nwant: %q", got, want)
	}

	p.Fork = false
	for _, a := range clarifyArgv(p) {
		if a == "--fork-session" {
			t.Fatal("--fork-session present with Fork false")
		}
	}

	p.Model = ""
	argv := clarifyArgv(p)
	for i, a := range argv {
		if a == "--model" && argv[i+1] != "opus" {
			t.Fatalf("empty model gave %q", argv[i+1])
		}
	}
	// A model that is not a plain model name (Claude Code writes
	// "<synthetic>" on an API-error line, and a store file from before the
	// check may hold one) falls back to opus too.
	for _, bad := range []string{"<synthetic>", "--dangerously-skip-permissions", "Opus", strings.Repeat("a", 101)} {
		p.Model = bad
		if !hasRun(clarifyArgv(p), "--model", "opus") {
			t.Errorf("model %q not replaced by opus: %q", bad, clarifyArgv(p))
		}
	}
	p.Model = "claude-opus-4-1[1m]"
	if !hasRun(clarifyArgv(p), "--model", "claude-opus-4-1[1m]") {
		t.Errorf("a plain model name was dropped")
	}
	if n := len(argv); argv[n-2] != "--" || argv[n-1] != "-rf what?" {
		t.Fatalf("tail = %q", argv[n-2:])
	}
}

func TestClarifyPreamble(t *testing.T) {
	c := fleetCardView{ID: "DR-5302", Title: "T", Summary: "S", Lean: "B",
		Options: []fleetOptionView{{Key: "A", Label: "x"}, {Key: "B", Label: "y"}}}
	got := clarifyPreamble(c)
	for _, w := range []string{
		"read-only side chat", "forked from FLEET",
		"Never resume, re-run or finish any step FLEET left unfinished",
		"say so plainly in your answer", "Nothing you write reaches FLEET",
		"DR-5302", "T", "S", "A: x", "B: y", "FLEET leans B",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("preamble lacks %q:\n%s", w, got)
		}
	}
}

func TestClarifyToolsNote(t *testing.T) {
	got := clarifyToolsNote("/x/cu-with-token")
	for _, w := range []string{"git log", "gh pr view", "/x/cu-with-token fetch <TICKET>", "jq",
		"`/x/cu-with-token fetch DR-1234`", "no shell variables", "does not mean Bash is off"} {
		if !strings.Contains(got, w) {
			t.Errorf("tools note lacks %q:\n%s", w, got)
		}
	}
	if strings.Contains(got, "claude-sessions usage") || strings.Contains(got, "rg,") {
		t.Errorf("tools note names a command the fork may not run:\n%s", got)
	}
}

func TestClarifyAskPrompt(t *testing.T) {
	c := fleetCardView{ID: "dr5791-count", Title: "run the count", Summary: "only on your Mac",
		Options: []fleetOptionView{{Key: "done", Label: "Ran it"}}}
	first := clarifyAskPrompt(c, true, "why can't you run it?")
	for _, w := range []string{"this one FLEET card", "id: dr5791-count", "title: run the count",
		"summary: only on your Mac", "- done: Ran it"} {
		if !strings.Contains(first, w) {
			t.Errorf("first ask lacks %q:\n%s", w, first)
		}
	}
	if !strings.HasSuffix(first, "\nMy question: why can't you run it?") {
		t.Errorf("first ask must end with the question:\n%s", first)
	}
	if got := clarifyAskPrompt(c, false, "and then?"); got != "Still about card dr5791-count (run the count): and then?" {
		t.Errorf("later ask = %q", got)
	}
}

func writeClarifyScript(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fake-claude")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestClarifyRunParsesJSON(t *testing.T) {
	dir := t.TempDir()
	rec := filepath.Join(t.TempDir(), "rec")
	bin := writeClarifyScript(t, `pwd > `+rec+`.pwd
cat > `+rec+`.stdin
echo '{"session_id":"f1","result":"hi","is_error":false,"permission_denials":[{},{}]}'`)
	got, err := runClarifyFork(context.Background(), bin, dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := clarifyRunResult{SessionID: "f1", Text: "hi", IsError: false, Denials: 2}
	if got != want {
		t.Fatalf("got %+v want %+v", got, want)
	}
	pwd, _ := os.ReadFile(rec + ".pwd")
	wantDir, _ := filepath.EvalSymlinks(dir)
	if gotDir, _ := filepath.EvalSymlinks(strings.TrimSpace(string(pwd))); gotDir != wantDir {
		t.Fatalf("cwd = %q want %q", gotDir, wantDir)
	}
	if in, _ := os.ReadFile(rec + ".stdin"); len(in) != 0 {
		t.Fatalf("stdin = %q, want empty", in)
	}
}

func TestClarifyRunFailureModes(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	if _, err := runClarifyFork(ctx, writeClarifyScript(t, "exit 3"), dir, nil); !errors.Is(err, errClarifyExit) {
		t.Fatalf("exit 3: err = %v", err)
	}
	if _, err := runClarifyFork(ctx, writeClarifyScript(t, "echo not json"), dir, nil); !errors.Is(err, errClarifyBadOutput) {
		t.Fatalf("not json: err = %v", err)
	}
	r, err := runClarifyFork(ctx, writeClarifyScript(t, `echo '{"is_error":true,"result":"x"}'`), dir, nil)
	if err != nil || !r.IsError {
		t.Fatalf("is_error: r=%+v err=%v", r, err)
	}

	pidFile := filepath.Join(t.TempDir(), "child.pid")
	bin := writeClarifyScript(t, "sleep 30 &\necho $! > "+pidFile+"\nwait")
	tctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = runClarifyFork(tctx, bin, dir, nil)
	if !errors.Is(err, errClarifyTimeout) {
		t.Fatalf("timeout: err = %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("timeout took %v", d)
	}
	data, rerr := os.ReadFile(pidFile)
	if rerr != nil {
		t.Fatal(rerr)
	}
	pid, perr := strconv.Atoi(strings.TrimSpace(string(data)))
	if perr != nil {
		t.Fatal(perr)
	}
	// The kill is asynchronous to our return only by a reap; give it a moment.
	deadline := time.Now().Add(time.Second)
	for syscall.Kill(pid, 0) == nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if syscall.Kill(pid, 0) == nil {
		t.Fatalf("child %d still alive", pid)
	}
}
