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
	// A wide terminal (cols=120, cap 116) so the cols-4 clamp can't mask a
	// contaminated width: if preview lines ever leaked into the innerWidth
	// max, the 400-char line would push the box to 116+4=120 cols, well past
	// the 76 the 72-column floor dictates. Asserting the exact top-border
	// width (not just "under the terminal width") is what actually catches
	// preview content driving the box wider than its floor.
	prev := &overlayPreview{Title: "t", Loaded: true, Lines: []string{strings.Repeat("x", 400)}}
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

func TestModalWakesAppendDoesNotMutateCallerSlice(t *testing.T) {
	// modalWakes is built once in RunTUI and shared by every modal. A naive
	// append(wakes, w) that reuses spare capacity would write into the
	// caller's backing array beyond its length — invisible to a check that
	// only inspects base[:len(base)], since that view still looks pristine.
	// So this seeds a sentinel in the spare slot (index 1, within cap 4 but
	// outside the length-1 slice passed to modalWakesWith) and asserts that
	// slot is untouched after the call — that's the only way to actually
	// catch a naive append reusing the caller's backing array.
	base := make([]wakeFD, 2, 4)
	base[0] = wakeFD{fd: 7, kind: wakeResize}
	base[1] = wakeFD{fd: 99, kind: wakeResize} // sentinel in the spare region

	p := startPreviewPane("t", func() (PreviewResult, error) { return PreviewResult{}, nil })
	defer p.close()

	got := modalWakesWith(base[:1], p)
	if len(base) != 2 || base[0].kind != wakeResize {
		t.Fatalf("caller slice mutated: %+v", base)
	}
	if base[1].fd != 99 {
		t.Fatalf("modalWakesWith wrote into the caller's spare capacity: base[1] = %+v, want sentinel fd 99", base[1])
	}
	if len(got) != 2 || got[1].kind != wakePreview {
		t.Fatalf("modalWakesWith = %+v, want base plus the preview wake", got)
	}
	if nilCase := modalWakesWith(base[:1], nil); len(nilCase) != 1 {
		t.Fatalf("nil pane should add no wake, got %+v", nilCase)
	}

	closed := startPreviewPane("t", func() (PreviewResult, error) { return PreviewResult{}, nil })
	closed.close()
	if closedCase := modalWakesWith(base[:1], closed); len(closedCase) != 1 {
		t.Fatalf("closed pane should add no wake, got %+v", closedCase)
	}
}

// numberedPreview builds a preview whose lines are individually identifiable,
// so a test can count exactly how many of them the box rendered.
func numberedPreview(n int) *overlayPreview {
	lines := make([]string, n)
	for i := range lines {
		lines[i] = fmt.Sprintf("line-%03d", i)
	}
	return &overlayPreview{Title: "t", Loaded: true, Lines: lines}
}

// renderedPreviewRows counts how many of numberedPreview's lines survived into
// the rendered box.
func renderedPreviewRows(out string, n int) int {
	count := 0
	for i := 0; i < n; i++ {
		if strings.Contains(out, fmt.Sprintf("line-%03d", i)) {
			count++
		}
	}
	return count
}

// The preview has no fixed ceiling — it grows with the terminal. These pin the
// exact arithmetic across the band, which was previously unguarded: mutating
// the chrome or margin constant left the whole suite green.
func TestRenderConfirmOverlayPreviewGrowsWithTerminal(t *testing.T) {
	cases := []struct{ rows, want int }{
		{12, 0},    // too short: plain box
		{13, 1},    // first row that fits
		{25, 13},   // mid-band
		{50, 38},   // tall terminal keeps growing
		{200, 188}, // no cap at any height
	}
	for _, tc := range cases {
		prev := numberedPreview(250)
		out := renderConfirmOverlay("hi", prev, 120, tc.rows)
		if got := renderedPreviewRows(out, 250); got != tc.want {
			t.Fatalf("rows=%d rendered %d preview lines, want %d", tc.rows, got, tc.want)
		}
	}
}

// The tail is what matters — the newest pane output, not the oldest.
func TestRenderConfirmOverlayPreviewKeepsNewestLines(t *testing.T) {
	prev := numberedPreview(100)
	out := renderConfirmOverlay("hi", prev, 120, 25) // 13 content rows
	if !strings.Contains(out, "line-099") || !strings.Contains(out, "line-087") {
		t.Fatalf("newest 13 lines not rendered:\n%s", out)
	}
	if strings.Contains(out, "line-086") {
		t.Fatalf("rendered more than the newest 13 lines:\n%s", out)
	}
}

// Adding a preview must never push the box past the viewport. The reserve
// accounts for len(qLines) rather than assuming 1, so a multi-line question
// eats into the preview instead of overflowing the bottom of the screen — a
// 6-line question at rows=24 used to produce a 25-row box.
//
// A question taller than the terminal overflows on its own, with or without a
// preview (the plain box has always done this), so the bar is: no worse than
// the nil-preview box, and within the viewport whenever the box fits at all.
func TestRenderConfirmOverlayPreviewBoxFitsViewport(t *testing.T) {
	for _, rows := range []int{13, 14, 20, 24, 40, 80} {
		for _, qLines := range []int{1, 2, 6, 15} {
			q := strings.TrimSuffix(strings.Repeat("q\n", qLines), "\n")
			plain := len(strings.Split(renderConfirmOverlay(q, nil, 120, rows), "\n"))
			got := len(strings.Split(renderConfirmOverlay(q, numberedPreview(250), 120, rows), "\n"))

			limit := rows
			if plain > limit {
				limit = plain // question alone already overflows; not the preview's doing
			}
			if got > limit {
				t.Fatalf("rows=%d qLines=%d: preview box is %d lines, want <= %d (plain box is %d)",
					rows, qLines, got, limit, plain)
			}
		}
	}
}
