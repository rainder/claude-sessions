package main

import (
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// visibleWidth counts the display columns of s the way clipLine bounds them:
// ANSI escape sequences are skipped and every remaining rune is one column. This
// differs from visualLen (which returns byte length), so it stays accurate for
// the inspector's multibyte glyphs like "·" and "↓".
func visibleWidth(s string) int {
	cols := 0
	for i := 0; i < len(s); {
		if s[i] == '\033' && i+1 < len(s) && s[i+1] == '[' {
			j := strings.IndexByte(s[i:], 'm')
			if j < 0 {
				break
			}
			i += j + 1
			continue
		}
		_, sz := utf8.DecodeRuneInString(s[i:])
		cols++
		i += sz
	}
	return cols
}

// populatedInspectorView returns a view with full metadata for layout tests.
func populatedInspectorView() inspectorViewState {
	v := newInspectorViewState("dev:42")
	v.snapshot = InspectorSnapshot{
		TargetID: "dev:42",
		Session: Session{PID: 42, Host: "dev", Name: "api-refactor",
			Model: "claude-opus-4-8", Status: "busy",
			ContextTokens: 42000, CostUSD: 1.28},
		Source: "tmux", Label: "dev:0.0",
		Lines: []string{"one", "two"},
	}
	v.viewportRows = 6
	return v
}

// hasHit reports whether any region carries the given action.
func hasHit(hits []hitRegion, action hitAction) bool {
	for _, h := range hits {
		if h.action == action {
			return true
		}
	}
	return false
}

func TestRenderInspectorMetadataAndLiveFooter(t *testing.T) {
	v := newInspectorViewState("dev:42")
	v.snapshot = InspectorSnapshot{
		TargetID: "dev:42", Session: Session{PID: 42, Host: "dev", Name: "api-refactor", Model: "claude-opus-4-8", Status: "busy", ContextTokens: 42000, CostUSD: 1.28},
		Source: "tmux", Label: "dev:0.0", Lines: []string{"one", "two"},
	}
	v.viewportRows = 10
	var b strings.Builder
	hits := RenderInspector(&b, v, nil, 100, 20)
	out := b.String()
	for _, want := range []string{"api-refactor", "PID 42", "dev", "opus", "busy", "LIVE", "Back", "Refresh", "Follow", "Compose"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if !hasHit(hits, hitInspectorBack) || !hasHit(hits, hitInspectorRefresh) || !hasHit(hits, hitInspectorFollow) || !hasHit(hits, hitInspectorCompose) {
		t.Fatalf("footer hits = %#v", hits)
	}
}

func TestRenderInspectorNarrowDropsMetadataBeforeControls(t *testing.T) {
	v := populatedInspectorView()
	var b strings.Builder
	hits := RenderInspector(&b, v, nil, 38, 10)
	if strings.Contains(b.String(), "$1.28") {
		t.Fatalf("cost not collapsed:\n%s", b.String())
	}
	if !strings.Contains(b.String(), "Back") || !hasHit(hits, hitInspectorBack) {
		t.Fatalf("Back missing")
	}
}

// TestRenderInspectorNarrowDropsContext confirms the middle breakpoint: below 64
// columns the context figure collapses but the model still renders.
func TestRenderInspectorNarrowDropsContext(t *testing.T) {
	v := populatedInspectorView()
	var b strings.Builder
	RenderInspector(&b, v, nil, 60, 10)
	out := b.String()
	if strings.Contains(out, "42k") {
		t.Fatalf("context not collapsed at cols=60:\n%s", out)
	}
	if !strings.Contains(out, "opus") {
		t.Fatalf("model dropped too early at cols=60:\n%s", out)
	}
}

func TestRenderInspectorStatusPriority(t *testing.T) {
	base := func() inspectorViewState {
		v := newInspectorViewState("dev:42")
		v.snapshot = InspectorSnapshot{
			TargetID: "dev:42",
			Session:  Session{PID: 42, Host: "dev", Name: "api-refactor", Model: "claude-opus-4-8", Status: "busy"},
			Source:   "tmux",
			Lines:    []string{"one", "two", "three"},
		}
		v.viewportRows = 6
		return v
	}

	cases := []struct {
		name   string
		mutate func(*inspectorViewState)
		want   string
		absent string
	}{
		{
			name:   "loading",
			mutate: func(v *inspectorViewState) { v.snapshot.Lines = nil; v.snapshot.Loading = true },
			want:   "LOADING",
		},
		{
			name:   "stale",
			mutate: func(v *inspectorViewState) { v.snapshot.Stale = true; v.snapshot.Error = "timeout" },
			want:   "STALE",
		},
		{
			name:   "ended",
			mutate: func(v *inspectorViewState) { v.snapshot.Ended = true },
			want:   "SESSION ENDED",
		},
		{
			name: "paused with new lines",
			mutate: func(v *inspectorViewState) {
				v.follow = false
				v.newLines = 2
				v.top = 0
			},
			want:   "PAUSED · 2 new",
			absent: "LIVE",
		},
		{
			name: "ended outranks stale",
			mutate: func(v *inspectorViewState) {
				v.snapshot.Ended = true
				v.snapshot.Stale = true
			},
			want:   "SESSION ENDED",
			absent: "STALE",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := base()
			tc.mutate(&v)
			var b strings.Builder
			RenderInspector(&b, v, nil, 100, 20)
			out := b.String()
			if !strings.Contains(out, tc.want) {
				t.Errorf("want status %q in:\n%s", tc.want, out)
			}
			if tc.absent != "" && strings.Contains(out, tc.absent) {
				t.Errorf("did not want %q in:\n%s", tc.absent, out)
			}
		})
	}
}

// TestRenderInspectorErrorWithoutContent shows the error text in the body when
// no lines have ever loaded.
func TestRenderInspectorErrorWithoutContent(t *testing.T) {
	v := newInspectorViewState("dev:42")
	v.snapshot = InspectorSnapshot{
		TargetID: "dev:42",
		Session:  Session{PID: 42, Host: "dev", Name: "api-refactor"},
		Source:   "tmux",
		Error:    "connection refused",
	}
	v.viewportRows = 6
	var b strings.Builder
	RenderInspector(&b, v, nil, 80, 12)
	if !strings.Contains(b.String(), "connection refused") {
		t.Fatalf("error text missing from body:\n%s", b.String())
	}
}

// TestRenderInspectorFollowClickableWhileFollowing verifies the Follow control
// stays clickable even when the view is already pinned to the tail.
func TestRenderInspectorFollowClickableWhileFollowing(t *testing.T) {
	v := populatedInspectorView() // follow defaults to true
	if !v.follow {
		t.Fatal("precondition: expected follow=true")
	}
	var b strings.Builder
	hits := RenderInspector(&b, v, nil, 100, 20)
	if !hasHit(hits, hitInspectorFollow) {
		t.Fatalf("Follow not clickable while following: %#v", hits)
	}
}

// TestInspectorTitleDimsAutoDerivedName mirrors render.go's TestDerivedNameDimmed:
// an auto-derived name renders dim, a user-set one renders bold and undimmed.
func TestInspectorTitleDimsAutoDerivedName(t *testing.T) {
	derived := InspectorSnapshot{Session: Session{PID: 42, Name: "der-name", NameSource: "derived"}}
	if title := inspectorTitle(derived); !strings.Contains(title, ansiDim+"der-name") {
		t.Errorf("derived name not dimmed: %q", title)
	}

	userSet := InspectorSnapshot{Session: Session{PID: 42, Name: "usr-name", NameSource: "user"}}
	if title := inspectorTitle(userSet); !strings.Contains(title, ansiBold+"usr-name") {
		t.Errorf("user-set name not bold: %q", title)
	} else if strings.Contains(title, ansiDim+"usr-name") {
		t.Errorf("user-set name unexpectedly dimmed: %q", title)
	}
}

func TestRenderInspectorTerminalTooSmall(t *testing.T) {
	v := populatedInspectorView()
	var b strings.Builder
	hits := RenderInspector(&b, v, nil, 20, 4)
	out := b.String()
	if !strings.Contains(out, "terminal too small") {
		t.Fatalf("missing too-small message:\n%s", out)
	}
	if !hasHit(hits, hitInspectorBack) {
		t.Fatalf("too-small screen missing Back hit: %#v", hits)
	}
	// Every emitted line must fit the requested width.
	for _, ln := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if visibleWidth(ln) > 20 {
			t.Fatalf("line exceeds width: %q (%d cols)", ln, visibleWidth(ln))
		}
	}
}

// TestRenderInspectorClipsEveryLine confirms no emitted line exceeds cols, so a
// long content line cannot corrupt the frame.
func TestRenderInspectorClipsEveryLine(t *testing.T) {
	v := populatedInspectorView()
	v.snapshot.Lines = []string{strings.Repeat("x", 500)}
	v.follow = true
	var b strings.Builder
	RenderInspector(&b, v, nil, 40, 12)
	for _, ln := range strings.Split(strings.TrimRight(b.String(), "\n"), "\n") {
		if visibleWidth(ln) > 40 {
			t.Fatalf("line exceeds width 40: %d cols", visibleWidth(ln))
		}
	}
}

func TestRenderInspectorComposingShowsInputBar(t *testing.T) {
	var buf strings.Builder
	view := inspectorViewState{
		viewportRows: 10,
		composing:    true,
		composeText:  "hello",
		snapshot:     InspectorSnapshot{Session: Session{PID: 1}},
	}
	RenderInspector(&buf, view, nil, 80, 14)
	if !strings.Contains(buf.String(), "> hello") {
		t.Fatalf("output = %q, want it to contain the compose prompt", buf.String())
	}
}

func TestRenderInspectorComposingShowsSendingStatus(t *testing.T) {
	var buf strings.Builder
	view := inspectorViewState{
		viewportRows:  10,
		composing:     true,
		composeText:   "hello",
		composeStatus: "sending…",
		snapshot:      InspectorSnapshot{Session: Session{PID: 1}},
	}
	RenderInspector(&buf, view, nil, 80, 14)
	out := buf.String()
	if !strings.Contains(out, "> hello") {
		t.Fatalf("output = %q, want it to contain the compose prompt", out)
	}
	if !strings.Contains(out, "sending…") {
		t.Fatalf("output = %q, want it to contain the in-flight send status", out)
	}
}

func TestRenderInspectorComposingShowsFailureStatus(t *testing.T) {
	var buf strings.Builder
	view := inspectorViewState{
		viewportRows:  10,
		composing:     true,
		composeText:   "hello",
		composeStatus: "send failed: broken pipe",
		snapshot:      InspectorSnapshot{Session: Session{PID: 1}},
	}
	RenderInspector(&buf, view, nil, 80, 14)
	out := buf.String()
	if !strings.Contains(out, "> hello") {
		t.Fatalf("output = %q, want it to still contain the compose prompt so the user can retry", out)
	}
	if !strings.Contains(out, "send failed: broken pipe") {
		t.Fatalf("output = %q, want it to contain the failure message", out)
	}
}

func TestInspectorFooterRightShowsComposeStatusBeforeExpiry(t *testing.T) {
	view := inspectorViewState{
		composeStatus:      "sent",
		composeStatusUntil: time.Now().Add(1 * time.Minute),
	}
	if got := inspectorFooterRight(view); got != "sent" {
		t.Fatalf("inspectorFooterRight = %q, want sent", got)
	}
}

func TestInspectorFooterRightFallsBackAfterExpiry(t *testing.T) {
	view := inspectorViewState{
		composeStatus:      "sent",
		composeStatusUntil: time.Now().Add(-1 * time.Minute),
		follow:             true,
	}
	if got := inspectorFooterRight(view); got != "LIVE ↓" {
		t.Fatalf("inspectorFooterRight = %q, want LIVE ↓ (status expired, fall back to freshness text)", got)
	}
}

// TestRenderInspectorFooterRightWinsOverComposeWhenNarrow is a regression test
// for the width-threshold bug: adding the "Compose" footer-left label grew the
// left side from 21 to 30 visible columns, which pushed every footer-right
// threshold up by 9 and re-hid text — including the "no tmux pane" hint — at
// widths (like 35) where it used to fit. cols=35 fits the pre-Compose 3-label
// footer ("Back  Refresh  Follow" = 21 cols) plus "no tmux pane" (12 runes)
// with room to spare (21+12+2=35), but does not fit the 4-label footer
// (30+12+2=44). Compose must be the one dropped so the hint stays visible.
func TestRenderInspectorFooterRightWinsOverComposeWhenNarrow(t *testing.T) {
	v := populatedInspectorView()
	v.composeStatus = "no tmux pane"
	v.composeStatusUntil = time.Now().Add(4 * time.Second)
	var b strings.Builder
	hits := RenderInspector(&b, v, nil, 35, 20)
	out := b.String()

	if !strings.Contains(out, "no tmux pane") {
		t.Fatalf("footer-right hint dropped at cols=35:\n%s", out)
	}
	if strings.Contains(out, "Compose") {
		t.Fatalf("Compose label should have been dropped to make room at cols=35:\n%s", out)
	}
	for _, want := range []string{"Back", "Refresh", "Follow"} {
		if !strings.Contains(out, want) {
			t.Errorf("footer-left label %q missing at cols=35:\n%s", want, out)
		}
	}
	if !hasHit(hits, hitInspectorBack) || !hasHit(hits, hitInspectorRefresh) || !hasHit(hits, hitInspectorFollow) {
		t.Fatalf("footer hits missing a floor control at cols=35: %#v", hits)
	}
	if hasHit(hits, hitInspectorCompose) {
		t.Fatalf("Compose hit region should not exist when its label is dropped: %#v", hits)
	}
	for _, ln := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if visibleWidth(ln) > 35 {
			t.Fatalf("line exceeds width: %q (%d cols)", ln, visibleWidth(ln))
		}
	}
}

// TestRenderInspectorFooterAtMinColsNeverOverflows exercises the floor of the
// drop logic: cols=24 is minInspectorCols, and the longest realistic
// footer-right text ("tmux · SESSION ENDED", 20 runes) can't fit alongside
// even the 3-label floor (21+20+2=43 > 24). Compose must still be dropped
// (its label would otherwise clip mid-word, since the full 4-label footer is
// 30 cols on its own), the floor controls must still render and stay
// clickable, and — the actual regression class this guards against — no
// emitted line may exceed cols regardless of how the drop math falls out.
func TestRenderInspectorFooterAtMinColsNeverOverflows(t *testing.T) {
	v := populatedInspectorView()
	v.snapshot.Ended = true // longest footer-right text: "tmux · SESSION ENDED"
	var b strings.Builder
	hits := RenderInspector(&b, v, nil, minInspectorCols, 20)
	out := b.String()

	if strings.Contains(out, "Compose") {
		t.Fatalf("Compose label should have been dropped at cols=%d:\n%s", minInspectorCols, out)
	}
	if hasHit(hits, hitInspectorCompose) {
		t.Fatalf("Compose hit region should not exist when its label is dropped at cols=%d: %#v", minInspectorCols, hits)
	}
	if !hasHit(hits, hitInspectorBack) || !hasHit(hits, hitInspectorRefresh) || !hasHit(hits, hitInspectorFollow) {
		t.Fatalf("footer hits missing a floor control at cols=%d: %#v", minInspectorCols, hits)
	}
	for _, ln := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if visibleWidth(ln) > minInspectorCols {
			t.Fatalf("line exceeds width: %q (%d cols, want <= %d)", ln, visibleWidth(ln), minInspectorCols)
		}
	}
}

// TestRenderInspectorFooterShowsComposeWhenRoomAllows confirms Compose is not
// dropped unconditionally — once cols is wide enough for both the full
// 4-label footer-left and the footer-right text (44 cols for a 12-rune hint:
// 30+12+2), both render together.
func TestRenderInspectorFooterShowsComposeWhenRoomAllows(t *testing.T) {
	v := populatedInspectorView()
	v.composeStatus = "no tmux pane"
	v.composeStatusUntil = time.Now().Add(4 * time.Second)
	var b strings.Builder
	hits := RenderInspector(&b, v, nil, 44, 20)
	out := b.String()

	if !strings.Contains(out, "no tmux pane") {
		t.Fatalf("footer-right hint missing at cols=44:\n%s", out)
	}
	if !strings.Contains(out, "Compose") {
		t.Fatalf("Compose label unnecessarily dropped at cols=44:\n%s", out)
	}
	if !hasHit(hits, hitInspectorCompose) {
		t.Fatalf("Compose hit region missing at cols=44: %#v", hits)
	}
}

// TestRenderInspectorFooterHitColumns checks the footer hit regions land on the
// visible label columns of "Back  Refresh  Follow  Compose".
func TestRenderInspectorFooterHitColumns(t *testing.T) {
	v := populatedInspectorView()
	var b strings.Builder
	hits := RenderInspector(&b, v, nil, 100, 20)

	want := map[hitAction][2]int{
		hitInspectorBack:    {0, 3},   // "Back"
		hitInspectorRefresh: {6, 12},  // "Refresh"
		hitInspectorFollow:  {15, 20}, // "Follow"
		hitInspectorCompose: {23, 29}, // "Compose"
	}
	footerY := 20 - 1
	for _, h := range hits {
		exp, ok := want[h.action]
		if !ok {
			continue
		}
		if h.x0 != exp[0] || h.x1 != exp[1] || h.y0 != footerY || h.y1 != footerY {
			t.Errorf("%v hit = (x0=%d,x1=%d,y=%d), want (x0=%d,x1=%d,y=%d)",
				h.action, h.x0, h.x1, h.y0, exp[0], exp[1], footerY)
		}
	}
}

// inspectorFrameLines splits a rendered frame into its emitted rows, dropping
// the trailing empty element Fprintln's final newline leaves behind.
func inspectorFrameLines(out string) []string {
	return strings.Split(strings.TrimSuffix(out, "\n"), "\n")
}

// TestInspectorTicketSummaryLinesNothingToShow covers every state that must
// render no summary at all: still fetching, failed, and loaded-but-empty. The
// inspector reserves no space for a summary it doesn't have.
func TestInspectorTicketSummaryLinesNothingToShow(t *testing.T) {
	cases := []struct {
		name string
		prev overlayPreview
	}{
		{"not loaded", overlayPreview{Lines: []string{"summary"}}},
		{"error", overlayPreview{Loaded: true, Err: errors.New("boom"), Lines: []string{"summary"}}},
		{"no lines", overlayPreview{Loaded: true}},
		{"blank lines only", overlayPreview{Loaded: true, Lines: []string{"", "   ", ""}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := inspectorTicketSummaryLines(tc.prev, 80); got != nil {
				t.Fatalf("inspectorTicketSummaryLines = %q, want nil", got)
			}
			if n := inspectorSummaryExtraRows(nil); n != 0 {
				t.Fatalf("inspectorSummaryExtraRows(nil) = %d, want 0", n)
			}
		})
	}
}

// TestInspectorTicketSummaryLinesHeadingAndWrap checks the loaded shape: a bold
// heading carrying the label, content word-wrapped to cols, blank lines passed
// through untouched, and trailing blanks dropped.
func TestInspectorTicketSummaryLinesHeadingAndWrap(t *testing.T) {
	got := inspectorTicketSummaryLines(overlayPreview{
		Loaded: true,
		Label:  "DR-1234",
		Lines:  []string{"alpha beta gamma delta epsilon", "", ""},
	}, 20)

	want := []string{
		bold("ticket: DR-1234:"),
		"alpha beta gamma",
		"delta epsilon",
	}
	if len(got) != len(want) {
		t.Fatalf("lines = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
	if n := inspectorSummaryExtraRows(got); n != len(got)+1 {
		t.Errorf("inspectorSummaryExtraRows = %d, want %d", n, len(got)+1)
	}

	// No label: the heading is the bare word.
	unlabelled := inspectorTicketSummaryLines(overlayPreview{Loaded: true, Lines: []string{"body"}}, 20)
	if len(unlabelled) == 0 || unlabelled[0] != bold("ticket:") {
		t.Errorf("unlabelled heading = %q, want %q", unlabelled, bold("ticket:"))
	}

	// A blank line between content lines survives as its own row.
	spaced := inspectorTicketSummaryLines(overlayPreview{Loaded: true, Lines: []string{"a", "", "b"}}, 20)
	if len(spaced) != 4 || spaced[2] != "" {
		t.Errorf("spaced = %q, want a blank third row", spaced)
	}
}

// TestInspectorTicketSummaryLinesTruncates confirms the cap: at most
// inspectorTicketSummaryMaxLines rows, the last one replaced by a dim ellipsis
// so the cut is visible.
func TestInspectorTicketSummaryLinesTruncates(t *testing.T) {
	got := inspectorTicketSummaryLines(overlayPreview{
		Loaded: true,
		Lines:  []string{"one", "two", "three", "four", "five"},
	}, 80)
	if len(got) != inspectorTicketSummaryMaxLines {
		t.Fatalf("len = %d, want %d: %q", len(got), inspectorTicketSummaryMaxLines, got)
	}
	if last := got[len(got)-1]; last != dim("…") {
		t.Errorf("last line = %q, want %q", last, dim("…"))
	}
}

// TestInspectorSummaryFit checks the row clamp: full-height terminals keep the
// whole block, short ones keep a prefix leaving at least one body row, and one
// too short for a heading plus a content line drops it entirely. Idempotence is
// what lets RunTUI and RenderInspector both apply it.
func TestInspectorSummaryFit(t *testing.T) {
	block := []string{"head", "a", "b", "c"}
	cases := []struct {
		rows int
		want int
	}{
		{rows: 20, want: 4}, // room for all of it
		{rows: 10, want: 4},
		{rows: 9, want: 3}, // 9-4-(3+1) = 1 body row
		{rows: 8, want: 2},
		{rows: 7, want: 0}, // a bare heading is worse than nothing
		{rows: 5, want: 0},
	}
	for _, tc := range cases {
		got := inspectorSummaryFit(block, tc.rows)
		if len(got) != tc.want {
			t.Errorf("fit(rows=%d) = %q, want %d lines", tc.rows, got, tc.want)
		}
		if again := inspectorSummaryFit(got, tc.rows); len(again) != len(got) {
			t.Errorf("fit not idempotent at rows=%d: %q then %q", tc.rows, got, again)
		}
		// Whatever survives must leave a body row and fit the frame.
		if body := tc.rows - inspectorChromeRows - inspectorSummaryExtraRows(got); body < 1 {
			t.Errorf("fit(rows=%d) leaves %d body rows", tc.rows, body)
		}
	}
}

// TestRenderInspectorSummaryAboveBody places the summary block between the
// header separator and the body, with a separator of its own, and confirms the
// frame still ends on the footer at row rows-1.
func TestRenderInspectorSummaryAboveBody(t *testing.T) {
	v := populatedInspectorView()
	summary := []string{bold("ticket: DR-1234:"), "fix the thing", "and the other"}
	var b strings.Builder
	hits := RenderInspector(&b, v, summary, 100, 20)

	lines := inspectorFrameLines(b.String())
	if len(lines) != 20 {
		t.Fatalf("frame has %d lines, want 20:\n%s", len(lines), b.String())
	}
	sep := clipLine(dim(strings.Repeat("-", 100)), 100)
	if lines[2] != sep {
		t.Fatalf("row 2 = %q, want the header separator", lines[2])
	}
	for i, want := range summary {
		if lines[3+i] != clipLine(want, 100) {
			t.Errorf("row %d = %q, want %q", 3+i, lines[3+i], want)
		}
	}
	if lines[3+len(summary)] != sep {
		t.Errorf("row %d = %q, want the summary separator", 3+len(summary), lines[3+len(summary)])
	}
	// Body starts right after that separator and the footer is still last.
	if !strings.Contains(lines[4+len(summary)], "one") {
		t.Errorf("body did not start at row %d: %q", 4+len(summary), lines[4+len(summary)])
	}
	if !strings.Contains(lines[19], "Back") {
		t.Errorf("footer not on the last row: %q", lines[19])
	}
	for _, h := range hits {
		if h.y0 != 19 || h.y1 != 19 {
			t.Errorf("hit %v at y=%d, want 19", h.action, h.y0)
		}
	}
}

// TestRenderInspectorSummaryClampedOnShortTerminal is the reason RenderInspector
// re-fits: a block that cannot fit is shortened (or dropped) rather than pushing
// the footer off the bottom, where the screen renderer's truncation would eat it
// while the footer hit regions still claimed the last row.
func TestRenderInspectorSummaryClampedOnShortTerminal(t *testing.T) {
	v := populatedInspectorView()
	summary := []string{bold("ticket: DR-1234:"), "one", "two", dim("…")}
	for _, rows := range []int{5, 6, 7, 8, 9, 12} {
		var b strings.Builder
		RenderInspector(&b, v, summary, 100, rows)
		lines := inspectorFrameLines(b.String())
		if len(lines) != rows {
			t.Fatalf("rows=%d: frame has %d lines, want %d:\n%s", rows, len(lines), rows, b.String())
		}
		if !strings.Contains(lines[rows-1], "Back") {
			t.Errorf("rows=%d: footer not on the last row: %q", rows, lines[rows-1])
		}
	}
}

// TestRenderInspectorNilSummaryUnchanged pins the no-ticket case — the common
// one — to the layout it has always had: fixed 4-row chrome, body from row 3,
// footer last.
func TestRenderInspectorNilSummaryUnchanged(t *testing.T) {
	v := populatedInspectorView()
	var b strings.Builder
	RenderInspector(&b, v, nil, 100, 20)
	lines := inspectorFrameLines(b.String())

	if len(lines) != 20 {
		t.Fatalf("frame has %d lines, want 20", len(lines))
	}
	if lines[2] != clipLine(dim(strings.Repeat("-", 100)), 100) {
		t.Errorf("row 2 = %q, want the separator", lines[2])
	}
	if !strings.Contains(lines[3], "one") {
		t.Errorf("body did not start at row 3: %q", lines[3])
	}
	if !strings.Contains(lines[19], "Back") {
		t.Errorf("footer not on the last row: %q", lines[19])
	}

	// An empty (non-nil) slice must render identically to nil.
	var empty strings.Builder
	RenderInspector(&empty, v, []string{}, 100, 20)
	if empty.String() != b.String() {
		t.Errorf("empty slice frame differs from nil frame")
	}
}
