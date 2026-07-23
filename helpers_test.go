package main

import (
	"strings"
	"testing"
)

func TestWriteMouseMode(t *testing.T) {
	var b strings.Builder
	writeMouseMode(&b, true)
	writeMouseMode(&b, false)
	want := "\x1b[?1000h\x1b[?1006h\x1b[?1006l\x1b[?1000l"
	if b.String() != want {
		t.Fatalf("mouse sequences = %q, want %q", b.String(), want)
	}
}

func TestShellQuote(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", "''"},
		{"hello", "'hello'"},
		{"hello world", "'hello world'"},
		{"it's", `'it'\''s'`},
		{"$(rm -rf /)", "'$(rm -rf /)'"},
		{"`echo hi`", "'`echo hi`'"},
		{"a; b && c | d", "'a; b && c | d'"},
	}
	for _, c := range cases {
		if got := shellQuote(c.in); got != c.want {
			t.Errorf("shellQuote(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestWriteInteractiveHandoff(t *testing.T) {
	var b strings.Builder
	writeInteractiveHandoff(&b)
	// Disable mouse, restore wrap, exit alt-screen, clear the revealed primary
	// screen, home the cursor, show the cursor — in that exact order. The 2J+H
	// pair is what suppresses the flicker of stale shell output before the
	// subprocess (tmux/ssh) paints.
	want := mouseDisableSequence + "\033[?7h\033[?1049l\033[2J\033[H\033[?25h"
	if b.String() != want {
		t.Fatalf("interactive handoff = %q, want %q", b.String(), want)
	}
}
