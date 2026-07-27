package main

import (
	"errors"
	"strings"
	"testing"
)

func TestTrimTrailingBlankDropsPaddingRows(t *testing.T) {
	got := trimTrailingBlank([]string{"a", "", "b", "", "   ", ""})
	want := []string{"a", "", "b"}
	if len(got) != len(want) {
		t.Fatalf("got %d lines %q, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestTrimTrailingBlankAllBlankReturnsEmpty(t *testing.T) {
	if got := trimTrailingBlank([]string{"", "  ", ""}); len(got) != 0 {
		t.Fatalf("got %q, want empty", got)
	}
}

func TestPreviewStatusLineStates(t *testing.T) {
	cases := []struct {
		name string
		prev overlayPreview
		want string
	}{
		{"loading", overlayPreview{Loaded: false}, "loading preview…"},
		{"ended", overlayPreview{Loaded: true, Err: errSessionEnded}, "session already gone"},
		{"failed", overlayPreview{Loaded: true, Err: errors.New("timeout")}, "preview unavailable: timeout"},
		{"empty", overlayPreview{Loaded: true}, "(pane empty)"},
		{"content", overlayPreview{Loaded: true, Lines: []string{"x"}}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := previewStatusLine(tc.prev)
			if !strings.Contains(got, tc.want) {
				t.Fatalf("previewStatusLine = %q, want to contain %q", got, tc.want)
			}
			if tc.want == "" && got != "" {
				t.Fatalf("previewStatusLine = %q, want empty", got)
			}
		})
	}
}

func TestPreviewBlockNilOrNoRoomReturnsNil(t *testing.T) {
	prev := &overlayPreview{Loaded: true, Lines: []string{"x"}}
	if got := previewBlock(nil, 40, 4); got != nil {
		t.Fatalf("nil preview returned %q", got)
	}
	if got := previewBlock(prev, 40, 0); got != nil {
		t.Fatalf("contentRows=0 returned %q", got)
	}
	if got := previewBlock(prev, 0, 4); got != nil {
		t.Fatalf("innerWidth=0 returned %q", got)
	}
}

func TestPreviewBlockShapeAndTailSelection(t *testing.T) {
	prev := &overlayPreview{
		Title:  "repo:branch · pid 42",
		Source: "tmux",
		Loaded: true,
		Lines:  []string{"one", "two", "three", "four", "five", "", ""},
	}
	got := previewBlock(prev, 40, 3)
	// title + divider + 3 content + divider
	if len(got) != 6 {
		t.Fatalf("got %d lines, want 6:\n%s", len(got), strings.Join(got, "\n"))
	}
	if !strings.Contains(got[0], "repo:branch · pid 42") || !strings.Contains(got[0], "tmux") {
		t.Fatalf("title row = %q", got[0])
	}
	// Trailing blanks trimmed, then the LAST 3 real lines kept.
	for i, want := range []string{"three", "four", "five"} {
		if !strings.Contains(got[2+i], want) {
			t.Fatalf("content row %d = %q, want %q", i, got[2+i], want)
		}
	}
	if !strings.HasPrefix(got[1], "─") || !strings.HasPrefix(got[5], "─") {
		t.Fatalf("dividers missing: %q / %q", got[1], got[5])
	}
}

func TestPreviewBlockFewerLinesThanRoomDoesNotPad(t *testing.T) {
	prev := &overlayPreview{Loaded: true, Title: "t", Lines: []string{"only"}}
	got := previewBlock(prev, 40, 8)
	if len(got) != 4 { // title + divider + 1 content + divider
		t.Fatalf("got %d lines, want 4:\n%s", len(got), strings.Join(got, "\n"))
	}
}

func TestPreviewBlockEveryLineEndsReset(t *testing.T) {
	prev := &overlayPreview{
		Loaded: true,
		Title:  "t",
		Source: "tmux",
		Lines:  []string{"\033[31mred and never reset"},
	}
	for i, l := range previewBlock(prev, 40, 2) {
		if !strings.HasSuffix(l, ansiReset) {
			t.Fatalf("line %d does not end with reset: %q", i, l)
		}
	}
}

func TestPreviewBlockClipsWideLinesToInnerWidth(t *testing.T) {
	prev := &overlayPreview{Loaded: true, Title: "t", Lines: []string{strings.Repeat("x", 200)}}
	for i, l := range previewBlock(prev, 20, 2) {
		if visibleWidth(l) > 20 {
			t.Fatalf("line %d is %d cols, want <=20: %q", i, visibleWidth(l), l)
		}
	}
}

func TestPreviewBlockDropsSourceWhenTitleFillsWidth(t *testing.T) {
	prev := &overlayPreview{Loaded: true, Title: strings.Repeat("t", 30), Source: "transcript"}
	got := previewBlock(prev, 20, 2)
	if strings.Contains(got[0], "transcript") {
		t.Fatalf("source should be dropped when it cannot fit: %q", got[0])
	}
}

func TestPreviewBlockStatusRendersInsteadOfContent(t *testing.T) {
	prev := &overlayPreview{Title: "t"} // not loaded
	got := previewBlock(prev, 40, 5)
	if len(got) != 4 { // title + divider + 1 status row + divider
		t.Fatalf("got %d lines, want 4:\n%s", len(got), strings.Join(got, "\n"))
	}
	if !strings.Contains(got[2], "loading preview…") {
		t.Fatalf("status row = %q", got[2])
	}
}
