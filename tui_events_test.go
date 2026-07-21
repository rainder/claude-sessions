package main

import (
	"testing"
	"time"
)

func TestInputDecoderArrowSplitAtEveryBoundary(t *testing.T) {
	seq := []byte("\x1b[A")
	for split := 1; split < len(seq); split++ {
		d := newInputDecoder()
		if got := d.Feed(seq[:split], time.Unix(0, 0)); len(got) != 0 {
			t.Fatalf("split %d first feed = %#v, want none", split, got)
		}
		got := d.Feed(seq[split:], time.Unix(0, int64(time.Millisecond)))
		if len(got) != 1 || got[0].kind != eventKey || got[0].key != KeyUp {
			t.Fatalf("split %d result = %#v, want KeyUp", split, got)
		}
	}
}

func TestInputDecoderExtendedKeys(t *testing.T) {
	cases := map[string]string{
		"\r": KeyEnter, "\n": KeyEnter,
		"\x1b[H": KeyHome, "\x1b[F": KeyEnd,
		"\x1b[1~": KeyHome, "\x1b[4~": KeyEnd,
		"\x1b[5~": KeyPageUp, "\x1b[6~": KeyPageDown,
	}
	for seq, want := range cases {
		d := newInputDecoder()
		got := d.Feed([]byte(seq), time.Unix(0, 0))
		if len(got) != 1 || got[0].key != want {
			t.Errorf("%q = %#v, want %q", seq, got, want)
		}
	}
}

func TestInputDecoderSGRMouse(t *testing.T) {
	cases := []struct {
		seq     string
		button  mouseButton
		release bool
		x, y    int
	}{
		{"\x1b[<0;12;7M", mouseLeft, false, 11, 6},
		{"\x1b[<0;12;7m", mouseLeft, true, 11, 6},
		{"\x1b[<64;2;3M", mouseWheelUp, false, 1, 2},
		{"\x1b[<65;2;3M", mouseWheelDown, false, 1, 2},
	}
	for _, tc := range cases {
		d := newInputDecoder()
		got := d.Feed([]byte(tc.seq), time.Unix(0, 0))
		if len(got) != 1 || got[0].kind != eventMouse {
			t.Fatalf("%q = %#v", tc.seq, got)
		}
		m := got[0].mouse
		if m.button != tc.button || m.release != tc.release || m.x != tc.x || m.y != tc.y {
			t.Errorf("%q mouse = %#v", tc.seq, m)
		}
	}
}

func TestInputDecoderPendingTimeout(t *testing.T) {
	d := newInputDecoder()
	now := time.Unix(0, 0)
	if _, ok := d.PendingTimeout(now); ok {
		t.Fatalf("PendingTimeout with empty buffer should report nothing pending")
	}
	d.Feed([]byte{'\x1b'}, now)
	if remaining, ok := d.PendingTimeout(now); !ok || remaining != escapeSequenceDelay {
		t.Fatalf("PendingTimeout right after ESC = (%v, %v), want (%v, true)", remaining, ok, escapeSequenceDelay)
	}
	if remaining, ok := d.PendingTimeout(now.Add(escapeSequenceDelay)); !ok || remaining != 0 {
		t.Fatalf("PendingTimeout after delay = (%v, %v), want (0, true)", remaining, ok)
	}
}

func TestInputDecoderBareEscapeFlushes(t *testing.T) {
	d := newInputDecoder()
	now := time.Unix(0, 0)
	if got := d.Feed([]byte{'\x1b'}, now); len(got) != 0 {
		t.Fatalf("Feed(ESC) = %#v, want pending", got)
	}
	if got := d.Flush(now.Add(escapeSequenceDelay / 2)); len(got) != 0 {
		t.Fatalf("early Flush = %#v, want pending", got)
	}
	got := d.Flush(now.Add(escapeSequenceDelay))
	if len(got) != 1 || got[0].key != KeyEsc {
		t.Fatalf("Flush = %#v, want KeyEsc", got)
	}
}
