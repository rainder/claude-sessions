package main

import (
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

// killJobResult is what a background kill (started by actKill) finishes
// with. The main loop (tui.go) consumes it to update the toast and, when a
// worktree cleanup failed, drive the interactive resurrect prompt.
type killJobResult struct {
	session   Session
	err       error  // localReattest/KillSession failure; worktree fields are zero
	worktree  string // non-empty when a worktree cleanup was attempted
	removeErr error  // RemoveWorktree failure, only set when worktree != ""
}

// killJob runs a session kill (and any worktree cleanup) in the background,
// following previewPane's wake-pipe pattern (kill_preview.go). It exists
// because killSessionWith (migrate.go) waits up to 5s for a tmux pane's
// process to actually die after `tmux kill-session` returns — blocking the
// TUI on that wait after every confirmed kill is the "killing dialog" hang
// this replaces. Every method is nil-safe so callers never have to branch on
// a nil job.
type killJob struct {
	mu sync.Mutex
	// session is the kill target, set once at construction and never
	// mutated — safe to read without the lock, unlike result/done below.
	// It lets the caller show a "killing X…" toast before the job finishes.
	session Session
	done    bool
	result  killJobResult
	wakeR   int
	wakeW   int
	closed  bool
}

// startKillJob kicks off the kill in the background and returns immediately.
// If the pipe cannot be created the job still runs — it simply never wakes
// the main loop, so its result surfaces on the next tick/keypress instead.
func startKillJob(s Session, worktree string) *killJob {
	j := &killJob{session: s, wakeR: -1, wakeW: -1}
	var fds [2]int
	if err := unix.Pipe(fds[:]); err == nil {
		syscall.CloseOnExec(fds[0])
		syscall.CloseOnExec(fds[1])
		_ = unix.SetNonblock(fds[0], true)
		_ = unix.SetNonblock(fds[1], true)
		j.wakeR, j.wakeW = fds[0], fds[1]
	}
	go func() {
		res := killJobResult{session: s}
		if err := localReattest(s.PID, s.SessionID); err != nil {
			res.err = err
		} else if err := KillSession(s); err != nil {
			res.err = err
		} else if worktree != "" {
			res.worktree = worktree
			if err := RemoveWorktree(worktree); err != nil {
				res.removeErr = err
			}
		}
		j.mu.Lock()
		j.done = true
		j.result = res
		// The write happens under the same lock close() takes, so the fd can
		// never be closed and reused between the check and the write.
		if !j.closed && j.wakeW >= 0 {
			_, _ = unix.Write(j.wakeW, []byte{1})
		}
		j.mu.Unlock()
	}()
	return j
}

// snapshot reports the job's result and whether it has finished yet.
func (j *killJob) snapshot() (killJobResult, bool) {
	if j == nil {
		return killJobResult{}, false
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.result, j.done
}

// wake exposes the read end for unix.Select. A negative fd means "no
// source" and is skipped by pollEvents.
func (j *killJob) wake() wakeFD {
	if j == nil {
		return wakeFD{fd: -1, kind: wakeKill}
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed || j.wakeR < 0 {
		return wakeFD{fd: -1, kind: wakeKill}
	}
	return wakeFD{fd: j.wakeR, kind: wakeKill}
}

// close releases the pipe. Idempotent, and safe to call while the kill is
// still running — the goroutine's write is guarded by the same mutex.
func (j *killJob) close() {
	if j == nil {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return
	}
	j.closed = true
	if j.wakeR >= 0 {
		_ = unix.Close(j.wakeR)
	}
	if j.wakeW >= 0 {
		_ = unix.Close(j.wakeW)
	}
}

// partitionKillJobs splits jobs into those still running (kept, in their
// original order) and the results of those that finished this call — each
// finished job is closed here so callers never have to remember to. A pure
// function (no terminal/toast side effects) so the drain logic itself —
// which jobs are kept vs. reported — is testable without RunTUI's plumbing;
// tui.go's finishDoneKillJobs wraps this with the toast/refresh behavior.
func partitionKillJobs(jobs []*killJob) (remaining []*killJob, done []killJobResult) {
	remaining = jobs[:0]
	for _, job := range jobs {
		res, ok := job.snapshot()
		if !ok {
			remaining = append(remaining, job)
			continue
		}
		job.close()
		done = append(done, res)
	}
	return remaining, done
}
