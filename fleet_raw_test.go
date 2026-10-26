package main

import (
	"strconv"
	"strings"
	"testing"
)

func TestFleetRawLines(t *testing.T) {
	c := fleetTestCard("a", "needs-you")
	c.Summary = "Run this:\n```\ngit pull --ff-only && make install && claude-sessions service restart\n```\npage [4388c878](https://brain.dev.trecs.dev/document-pages/4388c878-1111)"
	c.Options[0].Line = "merges #3301"
	got := fleetRawLines(fleetItem{Card: c, Section: fleetSecApprove})
	want := []string{
		"card a",
		"DR-1 · P" + strconv.Itoa(c.priority()),
		"",
		"Run this:",
		"```",
		"git pull --ff-only && make install && claude-sessions service restart",
		"```",
		"page [4388c878](https://brain.dev.trecs.dev/document-pages/4388c878-1111)",
		"",
		"Options:",
		"1  yes — Merge now",
		"merges #3301",
		"2  not-yet — Hold",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("raw lines:\n got %q\nwant %q", got, want)
	}
}

func TestFleetRawRows(t *testing.T) {
	for _, c := range []struct {
		line       string
		cols, want int
	}{
		{"", 5, 1},
		{"abc", 3, 1},
		{"abcd", 3, 2},
		{"abcdefg", 3, 3},
		{"⛵⛵", 3, 2},
		{"abc", 0, 1},
	} {
		if got := fleetRawRows(c.line, c.cols); got != c.want {
			t.Errorf("fleetRawRows(%q, %d) = %d, want %d", c.line, c.cols, got, c.want)
		}
	}
}

func TestFleetRawFrame(t *testing.T) {
	long := "git pull --ff-only && make install && PATH=\"$HOME/.local/bin:$PATH\" claude-sessions service restart"
	lines := []string{"title", long, "after", "more", "end"}
	frame, top, shown := fleetRawFrame(lines, 0, 20, 8)
	if top != 0 {
		t.Fatalf("top = %d", top)
	}
	// The long line is written whole, for the terminal to soft-wrap.
	if !strings.Contains(frame, "\n"+long+"\n") {
		t.Fatalf("long line broken in %q", frame)
	}
	if !strings.Contains(frame, "\x1b[?7h") {
		t.Fatal("autowrap not on")
	}
	// 7 rows above the help row: title (1), the long line (5 at 20 columns)
	// and "after" (1) fill them.
	if shown != 3 || !strings.Contains(frame, "after") || strings.Contains(frame, "more") {
		t.Fatalf("shown = %d, frame %q", shown, frame)
	}
	if !strings.Contains(frame, "\x1b[8;1H") || !strings.Contains(frame, "raw card text") {
		t.Fatalf("no help row: %q", frame)
	}
	// The help row is cut one column short of the edge so it never wraps.
	i := strings.Index(frame, "\x1b[8;1H")
	if w := visualLen(strings.TrimSuffix(frame[i+len("\x1b[8;1H"):], screenSyncEnd)); w > 19 {
		t.Fatalf("help row is %d wide", w)
	}
	// A first line taller than the screen still shows.
	if _, _, shown := fleetRawFrame(lines, 1, 5, 3); shown != 1 {
		t.Fatalf("tall line: shown = %d", shown)
	}
	// top is clamped to the lines.
	if _, top, _ := fleetRawFrame(lines, 99, 80, 24); top != len(lines)-1 {
		t.Fatalf("clamped top = %d", top)
	}
	if _, top, shown := fleetRawFrame(nil, 3, 80, 24); top != 0 || shown != 0 {
		t.Fatalf("no lines: top %d shown %d", top, shown)
	}
}

func TestFleetRawKey(t *testing.T) {
	for _, c := range []struct {
		k             string
		top, shown, n int
		wantTop       int
		wantOpen      bool
	}{
		{KeyDown, 0, 5, 10, 1, true},
		{KeyDown, 9, 5, 10, 9, true},
		{KeyUp, 0, 5, 10, 0, true},
		{KeyPageDown, 2, 5, 10, 7, true},
		{KeyPageDown, 7, 5, 10, 9, true},
		{KeyPageUp, 3, 5, 10, 0, true},
		{KeyHome, 6, 5, 10, 0, true},
		{KeyEnd, 0, 5, 10, 5, true},
		{KeyEsc, 4, 5, 10, 4, false},
		{"t", 4, 5, 10, 4, false},
		{"q", 4, 5, 10, 4, false},
	} {
		top, open := fleetRawKey(c.k, c.top, c.shown, c.n)
		if top != c.wantTop || open != c.wantOpen {
			t.Errorf("fleetRawKey(%q, %d) = %d,%v want %d,%v", c.k, c.top, top, open, c.wantTop, c.wantOpen)
		}
	}
}
