package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestAccountSwitchErrorClose(t *testing.T) {
	for _, k := range []string{KeyEnter, "\r", "\n", KeyEsc, "q", "Q", " ", "\x03"} {
		if !accountSwitchErrorClose(k) {
			t.Errorf("%q must close the panel", k)
		}
	}
	// Ctrl-C is consumed by the panel, never passed on to the list's quit.
	for _, k := range []string{"x", "y", "n", KeyUp, KeyDown, KeyLeft, KeyRight, "\t", "\x17", KeyPageDown} {
		if accountSwitchErrorClose(k) {
			t.Errorf("%q must be ignored", k)
		}
	}
}

func TestRenderAccountSwitchErrorSanitizes(t *testing.T) {
	errText := "remote said \x1b]52;c;QUFBQQ==\x07boom\x1b[2J\x9b\r\nsecond\tline \x1b[31mred"
	got := renderAccountSwitchError("ws\x1b[H", "claude", "work\x07", errText, 80, 24)
	for _, bad := range []string{"]52", "\x1b[2J", "\x1b[H", "\x07", "\r", "\x9b", "\x1b[31m"} {
		if strings.Contains(got, bad) {
			t.Errorf("panel carries %q:\n%q", bad, got)
		}
	}
	for _, want := range []string{"\x1b[1;31m" + accountSwitchErrorTitle, "host:    ws", "tool:    claude", "account: work",
		"remote said boom", "second line red", accountSwitchErrorHint} {
		if !strings.Contains(got, want) {
			t.Errorf("panel lacks %q:\n%s", want, got)
		}
	}
}

func TestRenderAccountSwitchErrorWrapsAndFits(t *testing.T) {
	long := strings.Repeat("word ", 60)
	got := renderAccountSwitchError("ws", "grok", "gx", long, 40, 40)
	lines := strings.Split(got, "\n")
	words := 0
	for _, l := range lines {
		if w := visualLen(l); w > 40 {
			t.Fatalf("line wider than the terminal (%d): %q", w, l)
		}
		words += strings.Count(l, "word")
	}
	if words != 60 {
		t.Fatalf("wrapped text lost words: %d of 60", words)
	}
	// Too short for the whole body: cut with … and the hint kept.
	short := renderAccountSwitchError("ws", "grok", "gx", long, 40, 12)
	if n := len(strings.Split(short, "\n")); n > 12 {
		t.Fatalf("%d rows for a 12-row terminal", n)
	}
	if !strings.Contains(short, "…") || !strings.Contains(short, accountSwitchErrorHint) {
		t.Fatalf("cut panel:\n%s", short)
	}
}

func TestAccountSwitchBellOnce(t *testing.T) {
	var buf bytes.Buffer
	accountSwitchBell(&buf)
	if buf.String() != "\a" {
		t.Fatalf("bell %q", buf.String())
	}
}
