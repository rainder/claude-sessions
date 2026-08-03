// resize_test.go
package main

import (
	"strings"
	"testing"
)

func TestResizeTmuxTargetEmptyTmuxRefuses(t *testing.T) {
	err := resizeTmuxTarget(Session{PID: 4242}, 120, 40)
	if err == nil || !strings.Contains(err.Error(), "no tmux pane") {
		t.Fatalf("resizeTmuxTarget = %v, want no-tmux-pane error", err)
	}
}

func TestResizeTmuxTargetInvalidSizeRefuses(t *testing.T) {
	err := resizeTmuxTarget(Session{PID: 4242, Tmux: "work:0.0"}, 0, 40)
	if err == nil || !strings.Contains(err.Error(), "invalid size") {
		t.Fatalf("resizeTmuxTarget = %v, want invalid-size error", err)
	}
	err = resizeTmuxTarget(Session{PID: 4242, Tmux: "work:0.0"}, 120, 0)
	if err == nil || !strings.Contains(err.Error(), "invalid size") {
		t.Fatalf("resizeTmuxTarget = %v, want invalid-size error", err)
	}
}

func TestRevertTmuxTargetEmptyTmuxRefuses(t *testing.T) {
	err := revertTmuxTarget(Session{PID: 4242})
	if err == nil || !strings.Contains(err.Error(), "no tmux pane") {
		t.Fatalf("revertTmuxTarget = %v, want no-tmux-pane error", err)
	}
}

func TestResizeSessionDispatchesToRevertTmuxTarget(t *testing.T) {
	// revert=true with an empty Tmux refuses via the empty-Tmux guard that
	// both resizeTmuxTarget and revertTmuxTarget share. This only proves the
	// empty-Tmux guard fires on the revert path — it does NOT distinguish
	// dispatch to revertTmuxTarget from resizeTmuxTarget, since both produce
	// an identical "no tmux pane" message when Tmux is empty. Telling the two
	// apart would need an exec-command seam (out of scope here).
	err := resizeSession(Session{PID: 1}, 0, 0, true)
	if err == nil || !strings.Contains(err.Error(), "no tmux pane") {
		t.Fatalf("resizeSession(revert) = %v, want no-tmux-pane error", err)
	}
}

func TestResizeSessionDispatchesToResizeTmuxTarget(t *testing.T) {
	// revert=false with a non-empty Tmux and an invalid size (cols=0) passes
	// resizeTmuxTarget's empty-Tmux guard and hits its cols/rows validation,
	// which only resizeTmuxTarget performs — revertTmuxTarget has no such
	// check. The "invalid size" error therefore proves resizeSession(revert=
	// false) actually dispatches to resizeTmuxTarget (not just that some
	// shared empty-Tmux guard fired), and it returns before exec.Command runs,
	// so the test stays tmux-free like the others.
	err := resizeSession(Session{PID: 1, Tmux: "work:0.0"}, 0, 40, false)
	if err == nil || !strings.Contains(err.Error(), "invalid size") {
		t.Fatalf("resizeSession = %v, want invalid-size error", err)
	}
}
