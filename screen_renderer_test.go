package main

import (
	"errors"
	"io"
	"strings"
	"testing"
)

type recordingScreenWriter struct {
	writes [][]byte
	err    error
	limit  int
}

func (w *recordingScreenWriter) Write(p []byte) (int, error) {
	w.writes = append(w.writes, append([]byte(nil), p...))
	if w.err != nil {
		return 0, w.err
	}
	if w.limit > 0 && w.limit < len(p) {
		return w.limit, nil
	}
	return len(p), nil
}

func (w *recordingScreenWriter) last() string {
	if len(w.writes) == 0 {
		return ""
	}
	return string(w.writes[len(w.writes)-1])
}

func TestScreenRendererFirstDrawAndNoOp(t *testing.T) {
	w := &recordingScreenWriter{}
	r := newScreenRenderer(w)
	if err := r.Draw("one\ntwo", 10, 3); err != nil {
		t.Fatal(err)
	}
	if len(w.writes) != 1 {
		t.Fatalf("writes = %d, want 1", len(w.writes))
	}
	out := w.last()
	for _, want := range []string{
		screenSyncBegin,
		"\x1b[1;1Hone" + ansiReset + screenEraseLine,
		"\x1b[2;1Htwo" + ansiReset + screenEraseLine,
		"\x1b[3;1H" + ansiReset + screenEraseLine,
		screenSyncEnd,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("first draw missing %q: %q", want, out)
		}
	}
	if strings.Contains(out, "\x1b[J") || strings.Contains(out, "\x1b[2J") {
		t.Fatalf("first draw clears display: %q", out)
	}
	if strings.Count(out, screenSyncBegin) != 1 || strings.Count(out, screenSyncEnd) != 1 {
		t.Fatalf("unbalanced sync markers: %q", out)
	}

	if err := r.Draw("one\ntwo", 10, 3); err != nil {
		t.Fatal(err)
	}
	if len(w.writes) != 1 {
		t.Fatalf("unchanged draw wrote again: %d writes", len(w.writes))
	}
}

func TestScreenRendererWritesOnlyChangedRows(t *testing.T) {
	w := &recordingScreenWriter{}
	r := newScreenRenderer(w)
	if err := r.Draw("one\ntwo\nthree", 10, 3); err != nil {
		t.Fatal(err)
	}
	w.writes = nil

	if err := r.Draw("one\nTWO\nthree", 10, 3); err != nil {
		t.Fatal(err)
	}
	if len(w.writes) != 1 {
		t.Fatalf("writes = %d, want 1", len(w.writes))
	}
	out := w.last()
	if !strings.Contains(out, "\x1b[2;1HTWO"+ansiReset+screenEraseLine) {
		t.Fatalf("changed row missing: %q", out)
	}
	if strings.Contains(out, "\x1b[1;1H") || strings.Contains(out, "\x1b[3;1H") {
		t.Fatalf("unchanged rows repainted: %q", out)
	}
}

func TestScreenRendererErasesShortenedAndRemovedRows(t *testing.T) {
	w := &recordingScreenWriter{}
	r := newScreenRenderer(w)
	if err := r.Draw("long value\nremove me", 20, 3); err != nil {
		t.Fatal(err)
	}
	w.writes = nil

	if err := r.Draw("x", 20, 3); err != nil {
		t.Fatal(err)
	}
	out := w.last()
	if !strings.Contains(out, "\x1b[1;1Hx"+ansiReset+screenEraseLine) {
		t.Fatalf("shortened row does not erase suffix: %q", out)
	}
	if !strings.Contains(out, "\x1b[2;1H"+ansiReset+screenEraseLine) {
		t.Fatalf("removed row not cleared: %q", out)
	}
	if strings.Contains(out, "\x1b[3;1H") {
		t.Fatalf("unchanged blank row repainted: %q", out)
	}
}

func TestScreenRendererResizeAndInvalidateForceFullPaint(t *testing.T) {
	w := &recordingScreenWriter{}
	r := newScreenRenderer(w)
	if err := r.Draw("one\ntwo", 10, 2); err != nil {
		t.Fatal(err)
	}
	w.writes = nil

	if err := r.Draw("one\ntwo", 12, 3); err != nil {
		t.Fatal(err)
	}
	out := w.last()
	for _, want := range []string{"\x1b[1;1H", "\x1b[2;1H", "\x1b[3;1H"} {
		if !strings.Contains(out, want) {
			t.Fatalf("resize omitted %q: %q", want, out)
		}
	}

	w.writes = nil
	r.Invalidate()
	if err := r.Draw("one\ntwo", 12, 3); err != nil {
		t.Fatal(err)
	}
	out = w.last()
	for _, want := range []string{"\x1b[1;1H", "\x1b[2;1H", "\x1b[3;1H"} {
		if !strings.Contains(out, want) {
			t.Fatalf("invalidated draw missing %q: %q", want, out)
		}
	}
}

func TestScreenRendererClipsStyledRowsAndResetsStyle(t *testing.T) {
	w := &recordingScreenWriter{}
	r := newScreenRenderer(w)
	if err := r.Draw("\x1b[31mabcdef", 3, 1); err != nil {
		t.Fatal(err)
	}
	out := w.last()
	if !strings.Contains(out, "\x1b[31mabc"+ansiReset+screenEraseLine) {
		t.Fatalf("styled clipped row = %q", out)
	}
}

func TestScreenRendererUnknownSizeFallback(t *testing.T) {
	w := &recordingScreenWriter{}
	r := newScreenRenderer(w)
	if err := r.Draw("one\ntwo", 0, 0); err != nil {
		t.Fatal(err)
	}
	want := screenSyncBegin + screenHome + "one\ntwo" + ansiReset + screenSyncEnd
	if got := w.last(); got != want {
		t.Fatalf("fallback = %q, want %q", got, want)
	}
	if r.valid {
		t.Fatal("unknown-size draw left cache valid")
	}
	if strings.Contains(w.last(), "\x1b[J") || strings.Contains(w.last(), "\x1b[2J") {
		t.Fatalf("fallback clears display: %q", w.last())
	}
}

func TestScreenRendererWriteFailuresInvalidateCache(t *testing.T) {
	t.Run("writer error", func(t *testing.T) {
		good := &recordingScreenWriter{}
		r := newScreenRenderer(good)
		if err := r.Draw("one", 10, 1); err != nil {
			t.Fatal(err)
		}
		boom := errors.New("boom")
		r.w = &recordingScreenWriter{err: boom}
		if err := r.Draw("two", 10, 1); !errors.Is(err, boom) {
			t.Fatalf("error = %v, want %v", err, boom)
		}
		if r.valid {
			t.Fatal("writer error left cache valid")
		}
	})

	t.Run("short write", func(t *testing.T) {
		good := &recordingScreenWriter{}
		r := newScreenRenderer(good)
		if err := r.Draw("one", 10, 1); err != nil {
			t.Fatal(err)
		}
		r.w = &recordingScreenWriter{limit: 1}
		if err := r.Draw("two", 10, 1); !errors.Is(err, io.ErrShortWrite) {
			t.Fatalf("short write error = %v, want %v", err, io.ErrShortWrite)
		}
		if r.valid {
			t.Fatal("short write left cache valid")
		}
	})
}
