package main

import (
	"strings"
	"testing"
)

func TestSessionScreenOpenKeys(t *testing.T) {
	for _, key := range []string{KeyEnter, "p", "P"} {
		if got := sessionKeyCommand(key); got != commandOpenInspector {
			t.Errorf("key %q command = %v", key, got)
		}
	}
}

func TestInspectorBackAndQuitKeys(t *testing.T) {
	for _, key := range []string{KeyEsc, "q", "Q", "p", "P"} {
		if got := inspectorKeyCommand(key); got != commandBack {
			t.Errorf("key %q command = %v", key, got)
		}
	}
	if got := inspectorKeyCommand("\x03"); got != commandQuit {
		t.Fatalf("Ctrl-C command = %v", got)
	}
}

func TestCycleSortMode(t *testing.T) {
	cases := []struct {
		mode  string
		delta int
		want  string
	}{
		{"dir", 1, "status"},
		{"status", 1, "created"},
		{"created", -1, "status"},
		{"updated-asc", 1, "dir"},
		{"dir", -1, "updated-asc"},
		{"created-asc", -1, "created"},
		{"bogus", 1, "status"},
		{"bogus", -1, "updated-asc"},
	}
	for _, c := range cases {
		if got := cycleSortMode(c.mode, c.delta); got != c.want {
			t.Errorf("cycleSortMode(%q, %d) = %q, want %q", c.mode, c.delta, got, c.want)
		}
	}
}

func TestSortDescStatus(t *testing.T) {
	if got := sortDesc("status"); got != "status (waiting → idle → busy)" {
		t.Fatalf("sortDesc(status) = %q", got)
	}
}

func TestRenderHelpIsPureContent(t *testing.T) {
	out := renderHelp("status")
	for _, want := range []string{"claude-sessions", "NAVIGATION", "current sort: status", "press any key to return"} {
		if !strings.Contains(out, want) {
			t.Fatalf("help missing %q: %q", want, out)
		}
	}
	if strings.Contains(out, "\x1b[H") || strings.Contains(out, "\x1b[J") || strings.Contains(out, "\x1b[2J") {
		t.Fatalf("help contains terminal positioning or clear: %q", out)
	}
}
