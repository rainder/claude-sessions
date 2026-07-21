package main

import (
	"strings"
	"testing"
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
	hits := RenderInspector(&b, v, 100, 20)
	out := b.String()
	for _, want := range []string{"api-refactor", "PID 42", "dev", "opus", "busy", "LIVE", "Back", "Refresh", "Follow"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if !hasHit(hits, hitInspectorBack) || !hasHit(hits, hitInspectorRefresh) || !hasHit(hits, hitInspectorFollow) {
		t.Fatalf("footer hits = %#v", hits)
	}
}

func TestRenderInspectorNarrowDropsMetadataBeforeControls(t *testing.T) {
	v := populatedInspectorView()
	var b strings.Builder
	hits := RenderInspector(&b, v, 38, 10)
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
	RenderInspector(&b, v, 60, 10)
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
			RenderInspector(&b, v, 100, 20)
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
	RenderInspector(&b, v, 80, 12)
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
	hits := RenderInspector(&b, v, 100, 20)
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
	hits := RenderInspector(&b, v, 20, 4)
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
	RenderInspector(&b, v, 40, 12)
	for _, ln := range strings.Split(strings.TrimRight(b.String(), "\n"), "\n") {
		if visibleWidth(ln) > 40 {
			t.Fatalf("line exceeds width 40: %d cols", visibleWidth(ln))
		}
	}
}

// TestRenderInspectorFooterHitColumns checks the footer hit regions land on the
// visible label columns of "Back  Refresh  Follow".
func TestRenderInspectorFooterHitColumns(t *testing.T) {
	v := populatedInspectorView()
	var b strings.Builder
	hits := RenderInspector(&b, v, 100, 20)

	want := map[hitAction][2]int{
		hitInspectorBack:    {0, 3},   // "Back"
		hitInspectorRefresh: {6, 12},  // "Refresh"
		hitInspectorFollow:  {15, 20}, // "Follow"
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
