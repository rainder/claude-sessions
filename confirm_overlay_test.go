package main

import (
	"strings"
	"testing"
)

func TestConfirmStateHandleConfirms(t *testing.T) {
	state := confirmState{}
	for _, key := range []string{"y", "Y", "\r", "\n", KeyEnter} {
		confirmed, done := state.handle(key)
		if !confirmed || !done {
			t.Fatalf("handle(%q) = confirmed %v done %v, want true true", key, confirmed, done)
		}
	}
}

func TestConfirmStateHandleCancels(t *testing.T) {
	state := confirmState{}
	for _, key := range []string{"n", "N", "q", "Q", KeyEsc, "\x03"} {
		confirmed, done := state.handle(key)
		if confirmed || !done {
			t.Fatalf("handle(%q) = confirmed %v done %v, want false true", key, confirmed, done)
		}
	}
}

func TestConfirmStateHandleIgnoresOtherKeys(t *testing.T) {
	state := confirmState{}
	for _, key := range []string{KeyUp, KeyDown, KeyLeft, KeyRight, "a", "1", " "} {
		confirmed, done := state.handle(key)
		if confirmed || done {
			t.Fatalf("handle(%q) = confirmed %v done %v, want false false", key, confirmed, done)
		}
	}
}

func TestRenderConfirmOverlayShowsQuestionAndHint(t *testing.T) {
	out := renderConfirmOverlay("kill PID 1234?", 80, 24)
	for _, want := range []string{"kill PID 1234?", "[y] yes", "[n] no", confirmBoxTL, confirmBoxTR, confirmBoxBL, confirmBoxBR} {
		if !strings.Contains(out, want) {
			t.Fatalf("renderConfirmOverlay missing %q:\n%s", want, out)
		}
	}
}

func TestRenderConfirmOverlayMultilineQuestion(t *testing.T) {
	out := renderConfirmOverlay("line one\nline two", 80, 24)
	for _, want := range []string{"line one", "line two"} {
		if !strings.Contains(out, want) {
			t.Fatalf("renderConfirmOverlay missing %q:\n%s", want, out)
		}
	}
}

func TestRenderConfirmOverlayUnknownSizeUnpositioned(t *testing.T) {
	out := renderConfirmOverlay("kill it?", 0, 0)
	if !strings.Contains(out, "kill it?") {
		t.Fatalf("renderConfirmOverlay missing question:\n%s", out)
	}
	// No terminal positioning/clear escapes leak into the content itself —
	// that's the renderer's job, mirroring TestRenderNewPicker.
	if strings.Contains(out, "\x1b[H") || strings.Contains(out, "\x1b[J") || strings.Contains(out, "\x1b[2J") {
		t.Fatalf("overlay contains terminal positioning or clear: %q", out)
	}
}

func TestRenderConfirmOverlayNarrowTerminalNoPanic(t *testing.T) {
	// A tiny terminal must clip rather than panic (negative repeat/width).
	for _, size := range []struct{ cols, rows int }{
		{cols: 1, rows: 1},
		{cols: 3, rows: 3},
		{cols: 0, rows: 5},
		{cols: 5, rows: 0},
	} {
		out := renderConfirmOverlay("a very long question that will not fit", size.cols, size.rows)
		if out == "" {
			t.Fatalf("renderConfirmOverlay(%d,%d) returned empty output", size.cols, size.rows)
		}
	}
}

func TestRenderConfirmOverlayCentered(t *testing.T) {
	out := renderConfirmOverlay("hi", 40, 10)
	lines := strings.Split(out, "\n")
	// The top border row should be indented (centered), not flush left.
	found := false
	for _, l := range lines {
		if strings.Contains(l, confirmBoxTL) {
			if strings.HasPrefix(l, " ") {
				found = true
			}
			break
		}
	}
	if !found {
		t.Fatalf("expected the box's top border to be horizontally centered (indented):\n%s", out)
	}
	// At least one leading blank line for vertical centering with plenty of
	// vertical room.
	if lines[0] != "" {
		t.Fatalf("expected vertical centering to leave a leading blank line, got first line %q:\n%s", lines[0], out)
	}
}
