package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
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
	// gap = 20 - 12 - 10 = -2: too tight to share the row, so the source
	// must be dropped entirely.
	//
	// A broken implementation that appends the source anyway (clamping the
	// negative gap up instead of dropping) leaks a truncated fragment of
	// "transcript" once clipLine cuts the row to width — e.g.
	// "tttttttttttt transcr" for a 1-space clamp. clipLine always cuts
	// *before* the full 10-char word can appear here (title(12) + any
	// padding + source(10) inherently overflows a 20-col row whenever the
	// drop condition holds), so a plain Contains(got[0], "transcript") check
	// can never fire — verified by hand by applying exactly that mutation
	// and confirming Contains stayed false. Comparing against the exact
	// expected drop-only row catches any such leak, full word or fragment.
	title := strings.Repeat("t", 12)
	prev := &overlayPreview{Loaded: true, Title: title, Source: "transcript"}
	got := previewBlock(prev, 20, 2)
	want := clipLine(title, 20) + ansiReset
	if got[0] != want {
		t.Fatalf("title row = %q, want %q (source must be dropped when it cannot fit)", got[0], want)
	}
	if visibleWidth(got[0]) > 20 {
		t.Fatalf("title row is %d cols, want <=20: %q", visibleWidth(got[0]), got[0])
	}
}

func TestPreviewBlockTitleRowFlushRightSource(t *testing.T) {
	prev := &overlayPreview{
		Loaded: true,
		Title:  "abcde",
		Source: "tmux",
		Lines:  []string{"x"},
	}
	got := previewBlock(prev, 20, 2)
	if visibleWidth(got[0]) != 20 {
		t.Fatalf("title row is %d cols, want exactly 20: %q", visibleWidth(got[0]), got[0])
	}
	// Strip the ANSI codes previewTitleRow/previewBlock can emit around the
	// source (dim + the trailing reset) and confirm the text itself ends
	// with the source marker, i.e. it sits flush against the right edge.
	cleaned := strings.NewReplacer(ansiDim, "", ansiReset, "").Replace(got[0])
	if !strings.HasSuffix(cleaned, "tmux") {
		t.Fatalf("source should sit flush right: %q", got[0])
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

func TestPreviewPaneSnapshotBeforeFetchIsUnloaded(t *testing.T) {
	release := make(chan struct{})
	p := startPreviewPane("t", func() (PreviewResult, error) {
		<-release
		return PreviewResult{Source: "tmux", Content: "done"}, nil
	})
	defer func() { close(release); p.close() }()

	snap := p.snapshot()
	if snap.Loaded {
		t.Fatal("snapshot reported Loaded before the fetch returned")
	}
	if snap.Title != "t" {
		t.Fatalf("Title = %q, want %q", snap.Title, "t")
	}
}

// TestPreviewPaneWakeDrainsThroughPollEvents exercises wake()'s fd against its
// only real consumer, pollEvents, rather than unix.Select alone: pollEvents'
// drain loop (tui_events.go:414-421) issues a SECOND unix.Read on the fd after
// consuming the single wake byte, expecting EAGAIN. A blocking read end (e.g.
// an *os.File pipe whose fd was obtained via Fd(), which flips it back to
// blocking) hangs forever on that second read instead of returning it — this
// test is what catches that, where TestPreviewPaneWakeFiresOnCompletion does
// not, because unix.Select alone never performs the second read.
func TestPreviewPaneWakeDrainsThroughPollEvents(t *testing.T) {
	p := startPreviewPane("t", func() (PreviewResult, error) {
		return PreviewResult{Content: "x"}, nil
	})
	defer p.close()

	waitLoaded(t, p) // the wake byte is written under the same lock, before Loaded is visible

	_, woke := pollEvents(newInputDecoder(), 50*time.Millisecond, []wakeFD{p.wake()})
	if woke&wakePreview == 0 {
		t.Fatalf("woke = %b, want wakePreview", woke)
	}
}

func TestPreviewPaneSnapshotAfterFetchCarriesContent(t *testing.T) {
	p := startPreviewPane("t", func() (PreviewResult, error) {
		return PreviewResult{Source: "tmux", Content: "alpha\nbravo"}, nil
	})
	defer p.close()

	snap := waitLoaded(t, p)
	if snap.Source != "tmux" {
		t.Fatalf("Source = %q, want tmux", snap.Source)
	}
	if len(snap.Lines) != 2 || snap.Lines[0] != "alpha" || snap.Lines[1] != "bravo" {
		t.Fatalf("Lines = %q", snap.Lines)
	}
}

func TestPreviewPaneFetchErrorIsCaptured(t *testing.T) {
	want := errors.New("boom")
	p := startPreviewPane("t", func() (PreviewResult, error) { return PreviewResult{}, want })
	defer p.close()

	snap := waitLoaded(t, p)
	if !errors.Is(snap.Err, want) {
		t.Fatalf("Err = %v, want %v", snap.Err, want)
	}
}

func TestPreviewPaneWakeFiresOnCompletion(t *testing.T) {
	p := startPreviewPane("t", func() (PreviewResult, error) {
		return PreviewResult{Content: "x"}, nil
	})
	defer p.close()

	w := p.wake()
	if w.fd < 0 || w.kind != wakePreview {
		t.Fatalf("wake() = %+v, want a live fd with wakePreview", w)
	}
	// The pipe must become readable once the fetch lands.
	deadline := time.Now().Add(2 * time.Second)
	for {
		var set unix.FdSet
		set.Zero()
		set.Set(w.fd)
		tv := unix.Timeval{Usec: 50000}
		n, err := unix.Select(w.fd+1, &set, nil, nil, &tv)
		if err == nil && n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("wake pipe never became readable after the fetch completed")
		}
	}
}

func TestPreviewPaneCloseBeforeFetchDoesNotPanic(t *testing.T) {
	release := make(chan struct{})
	finished := make(chan struct{})
	p := startPreviewPane("t", func() (PreviewResult, error) {
		<-release
		defer close(finished)
		return PreviewResult{Content: "late"}, nil
	})
	p.close()
	close(release)

	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("fetch goroutine leaked after close")
	}
}

func TestPreviewPaneCloseIsIdempotent(t *testing.T) {
	p := startPreviewPane("t", func() (PreviewResult, error) { return PreviewResult{}, nil })
	p.close()
	p.close()
	if got := p.wake(); got.fd >= 0 {
		t.Fatalf("wake() after close = %+v, want a negative fd", got)
	}
}

func TestPreviewPaneNilIsSafe(t *testing.T) {
	var p *previewPane
	if snap := p.snapshot(); snap.Loaded {
		t.Fatal("nil pane snapshot should be zero-valued")
	}
	if got := p.wake(); got.fd >= 0 {
		t.Fatalf("nil pane wake = %+v, want a negative fd", got)
	}
	p.close() // must not panic
}

func TestKillPreviewTitle(t *testing.T) {
	local := Session{PID: 4242, Name: "my-session", NameSource: "user"}
	if got := previewTitle(local); !strings.Contains(got, "my-session") || !strings.Contains(got, "pid 4242") {
		t.Fatalf("local title = %q", got)
	}
	remote := Session{PID: 99, Host: "pi", Name: "my-session", NameSource: "user"}
	got := previewTitle(remote)
	if !strings.Contains(got, "pi:99") {
		t.Fatalf("remote title = %q, want host-qualified", got)
	}
	if strings.Contains(got, "pid 99") {
		t.Fatalf("remote title should not use the local pid form: %q", got)
	}
}

// waitLoaded polls until the pane's fetch has landed, failing the test on timeout.
func waitLoaded(t *testing.T, p *previewPane) overlayPreview {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if snap := p.snapshot(); snap.Loaded {
			return snap
		}
		if time.Now().After(deadline) {
			t.Fatal("fetch never completed")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
