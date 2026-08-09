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

func TestLargestBlankRun(t *testing.T) {
	tests := []struct {
		name      string
		content   string
		wantRun   int
		wantTotal int
	}{
		{"empty", "", 1, 1},
		{"all blank", "\n\n\n", 3, 3},
		{"trailing blank rows", "hello\nworld\n\n\n\n", 3, 5},
		{"interior gap larger than trailing", "hello\n\n\n\n\nworld\n\n", 4, 7},
		{"no blanks at all", "hello\nworld\n", 0, 2},
		{"color-only line reads as blank", "hello\n\x1b[0m\n\n", 2, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			run, total := largestBlankRun(tt.content)
			if run != tt.wantRun || total != tt.wantTotal {
				t.Errorf("largestBlankRun(%q) = (%d, %d), want (%d, %d)", tt.content, run, total, tt.wantRun, tt.wantTotal)
			}
		})
	}
}

func TestCapInspectorPaneRows(t *testing.T) {
	t.Run("blank capture refuses to shrink", func(t *testing.T) {
		_, ok := capInspectorPaneRows("", 80, 20)
		if ok {
			t.Fatal("capInspectorPaneRows(blank) should refuse to shrink")
		}
	})

	t.Run("entirely blank capture (too-early probe) refuses to shrink", func(t *testing.T) {
		content := strings.Repeat("\n", 80)
		_, ok := capInspectorPaneRows(content, 80, 20)
		if ok {
			t.Fatal("capInspectorPaneRows(all-blank) should refuse to shrink")
		}
	})

	t.Run("content fills the oversized pane, no shrink", func(t *testing.T) {
		content := strings.Repeat("x\n", 80)
		_, ok := capInspectorPaneRows(content, 80, 20)
		if ok {
			t.Fatal("capInspectorPaneRows(full) should refuse to shrink")
		}
	})

	t.Run("only small natural spacing between messages, no shrink", func(t *testing.T) {
		// Every message followed by exactly one blank separator line — normal
		// formatting, not oversize padding. No run exceeds the threshold.
		content := strings.Repeat("x\n\n", 20)
		_, ok := capInspectorPaneRows(content, 40, 10)
		if ok {
			t.Fatal("capInspectorPaneRows(small gaps) should refuse to shrink")
		}
	})

	t.Run("trailing blank run shrinks to content height plus margin", func(t *testing.T) {
		content := strings.Repeat("x\n", 10) + strings.Repeat("\n", 70)
		rows, ok := capInspectorPaneRows(content, 80, 5)
		if !ok {
			t.Fatal("capInspectorPaneRows(short) should shrink")
		}
		want := 10 + inspectorContentCapMargin
		if rows != want {
			t.Errorf("rows = %d, want %d", rows, want)
		}
	})

	t.Run("interior gap before an anchored footer still shrinks", func(t *testing.T) {
		// Mirrors a real Claude Code preview capture: header + history, a
		// wide gap left by oversizing the pane, then a footer pinned to the
		// pane's last rows — content on both sides of the gap, so a
		// trailing-blanks-only measure would see this as "full" and refuse.
		content := strings.Repeat("h\n", 13) + strings.Repeat("\n", 10) + strings.Repeat("f\n", 3)
		// total = 13 + 10 + 3 = 26, largest run = 10
		rows, ok := capInspectorPaneRows(content, 26, 5)
		if !ok {
			t.Fatal("capInspectorPaneRows(anchored footer) should shrink")
		}
		want := 26 - (10 - inspectorContentCapMargin)
		if rows != want {
			t.Errorf("rows = %d, want %d", rows, want)
		}
	})

	t.Run("shrink never goes below floor", func(t *testing.T) {
		content := strings.Repeat("x\n", 3) + strings.Repeat("\n", 77)
		rows, ok := capInspectorPaneRows(content, 80, 20)
		if !ok {
			t.Fatal("capInspectorPaneRows(very short) should shrink")
		}
		if rows != 20 {
			t.Errorf("rows = %d, want floor 20", rows)
		}
	})
}
