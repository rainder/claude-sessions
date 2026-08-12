package main

import (
	"errors"
	"os"
	"strings"
	"testing"

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
	job := startKillJob(Session{PID: 999999999}, "")
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
