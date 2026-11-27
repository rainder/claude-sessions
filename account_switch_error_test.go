package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
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

// TestAccountSwitchErrorGateIgnoresTypeAhead: inside the grace window no key
// closes the panel, so an Enter typed during the cooked wait cannot dismiss
// it unseen; after it, the close keys work.
func TestAccountSwitchErrorGateIgnoresTypeAhead(t *testing.T) {
	now := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	orig := accountSwitchErrorNow
	accountSwitchErrorNow = func() time.Time { return now }
	t.Cleanup(func() { accountSwitchErrorNow = orig })
	gate := newAccountSwitchErrorGate()
	for _, k := range []string{KeyEnter, "\r", " ", KeyEsc, "q", "\x03"} {
		if gate.closes(k) {
			t.Fatalf("%q closed the panel at open", k)
		}
	}
	now = now.Add(accountSwitchErrorGrace - time.Millisecond)
	if gate.closes(KeyEnter) {
		t.Fatal("Enter closed the panel inside the grace window")
	}
	now = now.Add(time.Millisecond)
	if !gate.closes(KeyEnter) || !gate.closes("\x03") || gate.closes("x") {
		t.Fatal("after the grace window the close keys must work, others not")
	}
}

func TestDrainModalInputDropsBufferedBytes(t *testing.T) {
	r, w := testPipe(t)
	orig := stdinFD
	stdinFD = r
	t.Cleanup(func() { stdinFD = orig })
	if _, err := unix.Write(w, []byte("\r \rq")); err != nil {
		t.Fatal(err)
	}
	dec := newInputDecoder()
	drainModalInput(dec)
	if events, _ := pollEvents(dec, 20*time.Millisecond, nil); len(events) != 0 {
		t.Fatalf("buffered input survived the drain: %+v", events)
	}
}
