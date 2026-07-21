package main

import "testing"

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
		{"dir", 1, "created"},
		{"updated-asc", 1, "dir"},  // wraps forward
		{"dir", -1, "updated-asc"}, // wraps backward
		{"created-asc", -1, "created"},
		{"bogus", 1, "created"}, // unknown = dir
		{"bogus", -1, "updated-asc"},
	}
	for _, c := range cases {
		if got := cycleSortMode(c.mode, c.delta); got != c.want {
			t.Errorf("cycleSortMode(%q, %d) = %q, want %q", c.mode, c.delta, got, c.want)
		}
	}
}
