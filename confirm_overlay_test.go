package main

import (
	"fmt"
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
	out := renderConfirmOverlay("kill PID 1234?", nil, 80, 24)
	for _, want := range []string{"kill PID 1234?", "[y] yes", "[n] no", confirmBoxTL, confirmBoxTR, confirmBoxBL, confirmBoxBR} {
		if !strings.Contains(out, want) {
			t.Fatalf("renderConfirmOverlay missing %q:\n%s", want, out)
		}
	}
}

func TestRenderConfirmOverlayMultilineQuestion(t *testing.T) {
	out := renderConfirmOverlay("line one\nline two", nil, 80, 24)
	for _, want := range []string{"line one", "line two"} {
		if !strings.Contains(out, want) {
			t.Fatalf("renderConfirmOverlay missing %q:\n%s", want, out)
		}
	}
}

func TestRenderConfirmOverlayUnknownSizeUnpositioned(t *testing.T) {
	out := renderConfirmOverlay("kill it?", nil, 0, 0)
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
		out := renderConfirmOverlay("a very long question that will not fit", nil, size.cols, size.rows)
		if out == "" {
			t.Fatalf("renderConfirmOverlay(%d,%d) returned empty output", size.cols, size.rows)
		}
	}
}

func TestRenderConfirmOverlayCentered(t *testing.T) {
	out := renderConfirmOverlay("hi", nil, 40, 10)
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

func TestRenderConfirmOverlayNilPreviewKeepsNarrowBox(t *testing.T) {
	out := renderConfirmOverlay("hi", nil, 120, 40)
	for _, ln := range strings.Split(out, "\n") {
		if strings.Contains(ln, confirmBoxTL) && visibleWidth(strings.TrimLeft(ln, " ")) >= 72 {
			t.Fatalf("nil preview widened the box to %d cols: %q", visibleWidth(ln), ln)
		}
	}
}

func TestRenderConfirmOverlayShowsPreviewRows(t *testing.T) {
	prev := &overlayPreview{
		Title:  "repo · pid 42",
		Source: "tmux",
		Loaded: true,
		Lines:  []string{"alpha", "bravo", "charlie"},
	}
	out := renderConfirmOverlay("kill PID 42?", prev, 120, 40)
	for _, want := range []string{"repo · pid 42", "alpha", "bravo", "charlie", "kill PID 42?", "[y] yes"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

func TestRenderConfirmOverlayPreviewAppliesWidthFloor(t *testing.T) {
	prev := &overlayPreview{Title: "t", Loaded: true, Lines: []string{"x"}}
	out := renderConfirmOverlay("hi", prev, 120, 40)
	var top string
	for _, ln := range strings.Split(out, "\n") {
		if strings.Contains(ln, confirmBoxTL) {
			top = strings.TrimLeft(ln, " ")
			break
		}
	}
	if got := visibleWidth(top); got != 76 { // 72 inner + 4 border/padding
		t.Fatalf("box width = %d, want 76: %q", got, top)
	}
}

func TestRenderConfirmOverlayShortTerminalDropsPreview(t *testing.T) {
	prev := &overlayPreview{Title: "should-not-appear", Loaded: true, Lines: []string{"nope"}}
	out := renderConfirmOverlay("hi", prev, 120, 10)
	if strings.Contains(out, "should-not-appear") || strings.Contains(out, "nope") {
		t.Fatalf("preview rendered on a 10-row terminal:\n%s", out)
	}
	if plain := renderConfirmOverlay("hi", nil, 120, 10); out != plain {
		t.Fatalf("short-terminal output differs from the nil-preview output:\n%s\n---\n%s", out, plain)
	}
}

func TestRenderConfirmOverlayUnknownSizeDropsPreview(t *testing.T) {
	prev := &overlayPreview{Title: "should-not-appear", Loaded: true, Lines: []string{"nope"}}
	out := renderConfirmOverlay("hi", prev, 0, 0)
	if strings.Contains(out, "should-not-appear") {
		t.Fatalf("preview rendered at unknown terminal size:\n%s", out)
	}
}

func TestRenderConfirmOverlayPreviewNeverWidensBox(t *testing.T) {
	prev := &overlayPreview{Title: "t", Loaded: true, Lines: []string{strings.Repeat("x", 400)}}
	out := renderConfirmOverlay("hi", prev, 100, 40)
	for _, ln := range strings.Split(out, "\n") {
		if visibleWidth(ln) > 100 {
			t.Fatalf("line exceeds terminal width: %d cols", visibleWidth(ln))
		}
	}
}

func TestRenderConfirmOverlayPreviewCapsAtTwelveRows(t *testing.T) {
	lines := make([]string, 40)
	for i := range lines {
		lines[i] = fmt.Sprintf("line-%02d", i)
	}
	prev := &overlayPreview{Title: "t", Loaded: true, Lines: lines}
	out := renderConfirmOverlay("hi", prev, 120, 200)
	if strings.Contains(out, "line-27") {
		t.Fatalf("more than 12 content rows rendered:\n%s", out)
	}
	if !strings.Contains(out, "line-39") || !strings.Contains(out, "line-28") {
		t.Fatalf("last 12 rows not rendered:\n%s", out)
	}
}
