package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/term"
)

// TestFinishKillJobReportsFailure: a reattest/KillSession error surfaces as
// the toast text verbatim, with no cooked-mode prompt.
func TestFinishKillJobReportsFailure(t *testing.T) {
	res := killJobResult{session: Session{PID: 7, Name: "myapp"}, err: errors.New("boom")}
	c := &actCtx{}
	got := finishKillJob(c, res)
	want := "kill failed: boom"
	if got != want {
		t.Fatalf("finishKillJob() = %q, want %q", got, want)
	}
}

// TestFinishKillJobReportsSuccess covers the two silent-success shapes: no
// worktree involved, and a worktree that was removed cleanly. Both must
// report a "killed <name>" toast with no cooked-mode prompt.
func TestFinishKillJobReportsSuccess(t *testing.T) {
	cases := []struct {
		name string
		res  killJobResult
	}{
		{"no worktree", killJobResult{session: Session{PID: 7, Name: "myapp"}}},
		{"worktree removed cleanly", killJobResult{session: Session{PID: 7, Name: "myapp"}, worktree: "/tmp/wt"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &actCtx{}
			got := finishKillJob(c, tc.res)
			want := "killed myapp"
			if got != want {
				t.Fatalf("finishKillJob() = %q, want %q", got, want)
			}
		})
	}
}

// TestFinishKillJobDirtyWorktreePromptsAndDeclines drives the one branch that
// does own the raw/cooked handoff: a worktree removal failure. Declining the
// resurrect prompt must still return "" (already reported via the cooked
// output) and must not touch c.spawnedTmux.
func TestFinishKillJobDirtyWorktreePromptsAndDeclines(t *testing.T) {
	var sink strings.Builder
	terminalOutput = &sink
	t.Cleanup(func() { terminalOutput = os.Stdout })

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	origStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = origStdin })
	if _, err := w.WriteString("n\n"); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()

	c := &actCtx{oldState: &term.State{}}
	res := killJobResult{
		session:   Session{PID: 7, Name: "myapp", SessionID: "s1"},
		worktree:  "/tmp/does-not-exist",
		removeErr: errors.New("dirty tree"),
	}
	var got string
	captureStdout(t, func() { got = finishKillJob(c, res) })

	if got != "" {
		t.Fatalf("finishKillJob() = %q, want \"\" (already reported via cooked output)", got)
	}
	if c.spawnedTmux != "" {
		t.Fatalf("spawnedTmux = %q, want empty on a declined resurrect", c.spawnedTmux)
	}
}

// TestActKillRefusesDuplicateInFlight proves the killInFlight guard stops a
// second Ctrl+X on a row whose background kill hasn't landed yet — without
// it, a duplicate confirm would race a still-live localReattest and
// re-attempt worktree cleanup the first job already handled.
func TestActKillRefusesDuplicateInFlight(t *testing.T) {
	target := sessionSelectionTarget(Session{PID: 42, SessionID: "dup"})
	c := &actCtx{
		targets: []selectionTarget{target},
		sel:     target.id,
		killInFlight: func(host string, pid int) bool {
			return host == "" && pid == 42
		},
	}
	actKill(c)
	if c.killJob != nil {
		t.Fatal("actKill started a duplicate kill job while one was already in flight")
	}
}

// TestKillJobDrainTracksCompletion exercises the wake-pipe mechanics a
// manually-completed job needs the main loop to observe: snapshot() reports
// done only after the goroutine's write lands, wake() reports its fd until
// close(), and a nil job is safe throughout.
func TestKillJobDrainTracksCompletion(t *testing.T) {
	var nilJob *killJob
	if _, done := nilJob.snapshot(); done {
		t.Fatal("nil job reported done")
	}
	if got := nilJob.wake(); got.fd != -1 {
		t.Fatalf("nil job wake fd = %d, want -1", got.fd)
	}
	nilJob.close() // must not panic

	done := make(chan struct{})
	job := startLocalKillJob(Session{PID: 999999999}, "")
	// PID 999999999 can't be reattested (no such session file), so the
	// goroutine finishes almost immediately with res.err set — exactly the
	// completion signal this test needs, not a real kill.
	go func() {
		for {
			if _, ok := job.snapshot(); ok {
				close(done)
				return
			}
		}
	}()
	<-done

	res, ok := job.snapshot()
	if !ok {
		t.Fatal("snapshot() done=false after goroutine completed")
	}
	if res.err == nil {
		t.Fatal("expected an error for a nonexistent PID")
	}
	job.close()
	if got := job.wake().fd; got != -1 {
		t.Fatalf("wake fd after close = %d, want -1", got)
	}
}

// TestFinishKillJobRemoteWorktreeFailureNoPrompt: a remote kill whose
// worktree cleanup failed has no resurrect flow (that needs local git
// state), so it must report the failure straight to the toast — no
// prepareLineOutput, no confirm, no pauseForKey.
func TestFinishKillJobRemoteWorktreeFailureNoPrompt(t *testing.T) {
	res := killJobResult{
		session:   Session{PID: 7, Name: "myapp", Host: "box"},
		worktree:  "/home/bob/.claude/worktrees/myapp",
		removeErr: errors.New("dirty tree"),
	}
	// A zero actCtx: if finishKillJob tried to switch terminal modes here it
	// would need c.oldState/c.fd, which are unset — the point is that this
	// branch never touches them.
	c := &actCtx{}
	got := finishKillJob(c, res)
	want := "killed myapp, worktree removal failed: dirty tree"
	if got != want {
		t.Fatalf("finishKillJob() = %q, want %q", got, want)
	}
}

// waitForKillJob polls a job until it finishes or the test times out —
// startRemoteKillJob's goroutine talks to an httptest server, so there is no
// synchronous point to assert against.
func waitForKillJob(t *testing.T, job *killJob) killJobResult {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if res, ok := job.snapshot(); ok {
			return res
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("kill job did not complete in time")
	return killJobResult{}
}

// TestStartRemoteKillJobAppliesServerWorktreeCleanup drives startRemoteKillJob
// against a fake server end to end: the kill response names a worktree, and
// the follow-up /worktree/remove call fails — the async job must surface
// both the worktree path and the removal error in its result, exactly what
// actKillRemote used to do synchronously in cooked mode.
func TestStartRemoteKillJobAppliesServerWorktreeCleanup(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/kill"):
			_, _ = w.Write([]byte(`{"ok":true,"worktree":{"path":"/home/bob/.claude/worktrees/myapp"}}`))
		case r.URL.Path == "/worktree/remove":
			_, _ = w.Write([]byte(`{"ok":false,"error":"dirty tree"}`))
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
		}
	}))
	defer backend.Close()

	u, _ := url.Parse(backend.URL)
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeServerYAML(t, home, "box", u.Hostname(), u.Port(), "secret")

	job := startRemoteKillJob(Session{PID: 42, Host: "box", Name: "myapp"})
	res := waitForKillJob(t, job)

	if res.err != nil {
		t.Fatalf("res.err = %v, want nil (the kill itself succeeded)", res.err)
	}
	if res.worktree != "/home/bob/.claude/worktrees/myapp" {
		t.Fatalf("res.worktree = %q, want the server-reported path", res.worktree)
	}
	if res.removeErr == nil || !strings.Contains(res.removeErr.Error(), "dirty tree") {
		t.Fatalf("res.removeErr = %v, want the server's removal failure", res.removeErr)
	}
}

// TestStartRemoteKillJobTreatsStaleRowAsSuccess: killInFlight only guards a
// job still running, not the gap between one landing and the session-list
// refresh catching up. A second Ctrl+X on a row whose kill already succeeded
// hits the server after the session is gone, which answers not_live/
// session_mismatch — that must read as a no-op, not a failure toast over a
// kill that actually worked.
func TestStartRemoteKillJobTreatsStaleRowAsSuccess(t *testing.T) {
	for _, code := range []string{codeNotLive, codeSessionMismatch} {
		t.Run(code, func(t *testing.T) {
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"ok":false,"error":"stale","code":"` + code + `"}`))
			}))
			defer backend.Close()

			u, _ := url.Parse(backend.URL)
			home := t.TempDir()
			t.Setenv("HOME", home)
			writeServerYAML(t, home, "box", u.Hostname(), u.Port(), "secret")

			job := startRemoteKillJob(Session{PID: 42, Host: "box", Name: "myapp"})
			res := waitForKillJob(t, job)

			if res.err != nil {
				t.Fatalf("res.err = %v, want nil for a stale-row %s response", res.err, code)
			}
			if got := finishKillJob(&actCtx{}, res); got != "killed myapp" {
				t.Fatalf("finishKillJob() = %q, want a plain success toast", got)
			}
		})
	}
}

// TestPartitionKillJobsKeepsRunningAndClosesFinished pins the drain logic
// finishDoneKillJobs relies on: unfinished jobs stay in the returned slice
// (same order), finished ones are reported in order and closed so their pipe
// fd is released.
func TestPartitionKillJobsKeepsRunningAndClosesFinished(t *testing.T) {
	running := &killJob{session: Session{PID: 1}, wakeR: -1, wakeW: -1}
	finishedA := &killJob{session: Session{PID: 2}, done: true, result: killJobResult{session: Session{PID: 2}}}
	finishedB := &killJob{session: Session{PID: 3}, done: true, result: killJobResult{session: Session{PID: 3}, err: errors.New("boom")}, wakeR: -1, wakeW: -1}

	remaining, done := partitionKillJobs([]*killJob{running, finishedA, finishedB})

	if len(remaining) != 1 || remaining[0] != running {
		t.Fatalf("remaining = %#v, want only the still-running job", remaining)
	}
	if len(done) != 2 || done[0].session.PID != 2 || done[1].session.PID != 3 {
		t.Fatalf("done = %#v, want finishedA then finishedB in order", done)
	}
	if done[1].err == nil {
		t.Fatal("finishedB's error was lost in partitioning")
	}
	if !finishedA.closed || !finishedB.closed {
		t.Fatal("a finished job was not closed")
	}
	if running.closed {
		t.Fatal("the still-running job was closed")
	}
}

// TestPartitionKillJobsAllRunningReturnsNoDone: nothing finished yet, so
// nothing should be reported or closed, and the slice comes back unchanged.
func TestPartitionKillJobsAllRunningReturnsNoDone(t *testing.T) {
	a := &killJob{session: Session{PID: 1}, wakeR: -1, wakeW: -1}
	b := &killJob{session: Session{PID: 2}, wakeR: -1, wakeW: -1}

	remaining, done := partitionKillJobs([]*killJob{a, b})

	if len(done) != 0 {
		t.Fatalf("done = %#v, want none", done)
	}
	if len(remaining) != 2 || remaining[0] != a || remaining[1] != b {
		t.Fatalf("remaining = %#v, want both jobs unchanged", remaining)
	}
}
