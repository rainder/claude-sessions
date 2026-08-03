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
	// revert=true with an empty Tmux still refuses via revertTmuxTarget's own
	// guard — proves resizeSession(revert=true) reaches revertTmuxTarget, not
	// resizeTmuxTarget (which would additionally complain about cols/rows).
	err := resizeSession(Session{PID: 1}, 0, 0, true)
	if err == nil || !strings.Contains(err.Error(), "no tmux pane") {
		t.Fatalf("resizeSession(revert) = %v, want no-tmux-pane error", err)
	}
}

func TestResizeSessionDispatchesToResizeTmuxTarget(t *testing.T) {
	// revert=false with an empty Tmux refuses via resizeTmuxTarget's own guard.
	err := resizeSession(Session{PID: 1}, 120, 40, false)
	if err == nil || !strings.Contains(err.Error(), "no tmux pane") {
		t.Fatalf("resizeSession = %v, want no-tmux-pane error", err)
	}
}
