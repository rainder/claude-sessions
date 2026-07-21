package main

import (
	"testing"
	"time"

	"golang.org/x/sys/unix"
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

func TestInputDecoderModifiedCSIDoesNotLeakBytes(t *testing.T) {
	// "\x1b[1;5A" is Ctrl+Up: a CSI sequence with two parameters (";5")
	// before the final byte. It isn't a mapped key, but it must be
	// consumed as a whole rather than leaking "5" and "A" as literal keys.
	d := newInputDecoder()
	got := d.Feed([]byte("\x1b[1;5A"), time.Unix(0, 0))
	if len(got) != 0 {
		t.Fatalf("Feed(%q) = %#v, want no events", "\x1b[1;5A", got)
	}
}

func TestInputDecoderModifiedCSISplitFeed(t *testing.T) {
	// Same sequence as above, fed one byte at a time: the decoder must
	// keep reporting "incomplete" until the real final byte ('A') arrives,
	// and must not leak any of the intermediate parameter bytes.
	seq := []byte("\x1b[1;5A")
	d := newInputDecoder()
	now := time.Unix(0, 0)
	for i := 0; i < len(seq)-1; i++ {
		if got := d.Feed(seq[i:i+1], now); len(got) != 0 {
			t.Fatalf("byte %d feed = %#v, want none pending", i, got)
		}
	}
	got := d.Feed(seq[len(seq)-1:], now)
	if len(got) != 0 {
		t.Fatalf("final byte feed = %#v, want no events", got)
	}
}

func TestInputDecoderUnrecognizedCSIAndSS3DoNotLeakBytes(t *testing.T) {
	cases := []string{
		"\x1b[Z", // shift-tab
		"\x1bOP", // F1
	}
	for _, seq := range cases {
		d := newInputDecoder()
		got := d.Feed([]byte(seq), time.Unix(0, 0))
		if len(got) != 0 {
			t.Fatalf("Feed(%q) = %#v, want no events", seq, got)
		}
	}
}

func TestInputDecoderCSIAbortsOnControlByte(t *testing.T) {
	// "\x1b[" followed by a control byte (here CR) is not a valid CSI body
	// byte (valid body bytes are 0x20-0x7e). Rather than swallowing the
	// control key as part of a discarded sequence, the decoder must treat
	// the lone ESC as KeyEsc and reprocess "[\r" independently, so the
	// Enter keystroke is never lost.
	d := newInputDecoder()
	got := d.Feed([]byte("\x1b[\r"), time.Unix(0, 0))
	if len(got) != 3 {
		t.Fatalf("Feed(%q) = %#v, want 3 events (KeyEsc, literal '[', KeyEnter)", "\x1b[\r", got)
	}
	if got[0].kind != eventKey || got[0].key != KeyEsc {
		t.Errorf("event 0 = %#v, want KeyEsc", got[0])
	}
	if got[1].kind != eventKey || got[1].key != "[" {
		t.Errorf("event 1 = %#v, want literal '['", got[1])
	}
	if got[2].kind != eventKey || got[2].key != KeyEnter {
		t.Errorf("event 2 = %#v, want KeyEnter", got[2])
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

// testPipe returns a non-blocking pipe closed automatically at test end.
func testPipe(t *testing.T) (r, w int) {
	t.Helper()
	var p [2]int
	if err := unix.Pipe(p[:]); err != nil {
		t.Fatalf("pipe: %v", err)
	}
	_ = unix.SetNonblock(p[0], true)
	_ = unix.SetNonblock(p[1], true)
	t.Cleanup(func() { _ = unix.Close(p[0]); _ = unix.Close(p[1]) })
	return p[0], p[1]
}

func TestPollEventsReportsEachWakeSource(t *testing.T) {
	remoteR, remoteW := testPipe(t)
	inspectorR, inspectorW := testPipe(t)
	_, _ = unix.Write(remoteW, []byte{1})
	_, _ = unix.Write(inspectorW, []byte{1})

	_, woke := pollEvents(newInputDecoder(), 50*time.Millisecond, []wakeFD{
		{fd: remoteR, kind: wakeRemote},
		{fd: inspectorR, kind: wakeInspector},
	})
	if woke != wakeRemote|wakeInspector {
		t.Fatalf("woke = %b, want remote|inspector", woke)
	}
}

func TestResizeWakeSignals(t *testing.T) {
	r, err := newResizeWake()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	r.Signal()
	_, woke := pollEvents(newInputDecoder(), 50*time.Millisecond,
		[]wakeFD{{fd: r.FD(), kind: wakeResize}})
	if woke&wakeResize == 0 {
		t.Fatalf("woke = %b, want resize", woke)
	}
}
