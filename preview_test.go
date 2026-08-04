package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSanitizeTerminalTextPreservesSGRAndStripsControls(t *testing.T) {
	in := "ok\x1b[31mred\x1b[0m" +
		"\x1b]0;owned\x07" +
		"\x1b]8;;https://example.com\x1b\\link\x1b]8;;\x1b\\" +
		"\x1b[2J\x1b[?1000hEND\r\n"
	want := "ok\x1b[31mred\x1b[0mlinkEND\n"
	if got := sanitizeTerminalText(in); got != want {
		t.Fatalf("sanitize = %q, want %q", got, want)
	}
}

func TestSanitizeTerminalTextExpandsTabsAndKeepsUTF8(t *testing.T) {
	in := "a\tb\x00\x07└─ café\n"
	want := "a    b└─ café\n"
	if got := sanitizeTerminalText(in); got != want {
		t.Fatalf("sanitize = %q, want %q", got, want)
	}
}

func TestSanitizeTerminalTextStripsC1Controls(t *testing.T) {
	// Raw single-byte C1 controls (0x9b = CSI, 0x9d = OSC) must be dropped.
	if got := sanitizeTerminalText("a\x9bb\x9dc"); got != "abc" {
		t.Fatalf("raw C1 = %q, want %q", got, "abc")
	}

	// C1 encoded as UTF-8 (U+009B = 0xc2 0x9b, a CSI introducer on
	// C1-honoring terminals) must leave no C1 byte or rune behind; the trailing
	// literal "[31mx" is inert text.
	got := sanitizeTerminalText("\xc2\x9b[31mx")
	if got != "[31mx" {
		t.Fatalf("utf8 C1 = %q, want %q", got, "[31mx")
	}
	for _, r := range got {
		if r >= 0x80 && r <= 0x9f {
			t.Fatalf("C1 rune U+%04X survived in %q", r, got)
		}
	}

	// Legitimate UTF-8 at or above U+00A0 must be preserved intact — including
	// characters whose continuation bytes fall inside 0x80–0x9f (→ = e2 86 92,
	// whose middle byte 0x86 lies in the C1 range).
	in := "café →   └─"
	if got := sanitizeTerminalText(in); got != in {
		t.Fatalf("utf8 preserved = %q, want %q", got, in)
	}
}

func TestSanitizeTerminalTextStripsPrivateAndIntermediateCSI(t *testing.T) {
	// Private-parameter CSI sequences that happen to end in 'm' (XTMODKEYS
	// "\x1b[>4;2m", DECRQM-style "\x1b[?4m") must be stripped, not replayed —
	// their body carries private markers ('>' 0x3e, '?' 0x3f) outside the
	// pure-numeric SGR body range.
	if got := sanitizeTerminalText("\x1b[>4;2mx\x1b[?4m"); got != "x" {
		t.Fatalf("private CSI = %q, want %q", got, "x")
	}
	// A genuine multi-parameter SGR (256-colour foreground) with a numeric-only
	// body must still be preserved intact.
	in := "\x1b[38;5;196mred\x1b[0m"
	if got := sanitizeTerminalText(in); got != in {
		t.Fatalf("SGR = %q, want %q", got, in)
	}
}

func TestLimitPreviewKeepsNewestLinesWithinBytes(t *testing.T) {
	in := strings.Repeat("old\n", 20) + "new-a\nnew-b\n"
	got := limitPreview(in, PreviewLimits{MaxLines: 2, MaxBytes: 64})
	if got != "new-a\nnew-b\n" {
		t.Fatalf("limit = %q", got)
	}
}

func TestLimitPreviewTrimsOldestBytesOnLineBoundary(t *testing.T) {
	in := "aaaa\nbbbb\ncccc\n" // 15 bytes, three 4-char lines
	got := limitPreview(in, PreviewLimits{MaxLines: 100, MaxBytes: 10})
	if got != "bbbb\ncccc\n" {
		t.Fatalf("limit = %q, want %q", got, "bbbb\ncccc\n")
	}
}

func TestLoadPreviewUsesBoundedTmuxCapture(t *testing.T) {
	old := previewTmuxCapture
	t.Cleanup(func() { previewTmuxCapture = old })
	previewTmuxCapture = func(pid int, limits PreviewLimits) (string, string, error) {
		if limits.MaxLines != 2000 || limits.MaxBytes != 512<<10 {
			t.Fatalf("limits = %#v", limits)
		}
		return "tmux pane dev:0.0", "hello\n", nil
	}
	got, err := LoadPreview(42, DefaultPreviewLimits())
	if err != nil || got.Source != "tmux" || got.Content != "hello\n" {
		t.Fatalf("result=%#v err=%v", got, err)
	}
	if got.Label != "tmux pane dev:0.0" {
		t.Fatalf("label = %q", got.Label)
	}
}

func TestLoadPreviewPropagatesTmuxCaptureError(t *testing.T) {
	old := previewTmuxCapture
	t.Cleanup(func() { previewTmuxCapture = old })
	previewTmuxCapture = func(pid int, limits PreviewLimits) (string, string, error) {
		return "", "", os.ErrPermission
	}
	if _, err := LoadPreview(42, DefaultPreviewLimits()); err == nil {
		t.Fatal("want error from tmux capture failure")
	}
}

func TestLoadPreviewFallsBackToTranscript(t *testing.T) {
	old := previewTmuxCapture
	t.Cleanup(func() { previewTmuxCapture = old })
	previewTmuxCapture = func(pid int, limits PreviewLimits) (string, string, error) {
		return "", "", errNoTmuxPane
	}

	home := t.TempDir()
	t.Setenv("HOME", home)

	sessDir := filepath.Join(home, ".claude", "sessions")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessDir, "77.json"),
		[]byte(`{"pid":77,"sessionId":"sid-preview-fallback"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	projDir := filepath.Join(home, ".claude", "projects", "-tmp-proj")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	transcript := `{"type":"user","message":{"role":"user","content":"hi there"}}` + "\n"
	if err := os.WriteFile(filepath.Join(projDir, "sid-preview-fallback.jsonl"),
		[]byte(transcript), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := LoadPreview(77, DefaultPreviewLimits())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got.Source != "transcript" {
		t.Fatalf("source = %q, want transcript", got.Source)
	}
	if !strings.Contains(got.Content, "hi there") {
		t.Fatalf("content = %q", got.Content)
	}
}

func TestLoadPreviewReturnsSessionEndedWhenMissing(t *testing.T) {
	old := previewTmuxCapture
	t.Cleanup(func() { previewTmuxCapture = old })
	previewTmuxCapture = func(pid int, limits PreviewLimits) (string, string, error) {
		return "", "", errNoTmuxPane
	}
	home := t.TempDir()
	t.Setenv("HOME", home)

	_, err := LoadPreview(999999, DefaultPreviewLimits())
	if err != errSessionEnded {
		t.Fatalf("err = %v, want errSessionEnded", err)
	}
}

// stubPaneGeometry makes the pane look `height` rows tall with `history` lines
// of scrollback, and fails the test if the paged path never asks.
func stubPaneGeometry(t *testing.T, height, history int) {
	t.Helper()
	old := previewPaneGeometry
	t.Cleanup(func() { previewPaneGeometry = old })
	previewPaneGeometry = func(string) (int, int, error) { return height, history, nil }
}

// stubTmuxRun records the argv of every capture-pane call and answers with out.
func stubTmuxRun(t *testing.T, out string) *[][]string {
	t.Helper()
	old := previewTmuxRun
	t.Cleanup(func() { previewTmuxRun = old })
	var calls [][]string
	previewTmuxRun = func(args ...string) ([]byte, error) {
		calls = append(calls, args)
		return []byte(out), nil
	}
	return &calls
}

// TestCapturePaneContentUnpagedArgvIsUnchanged pins the argv every caller that
// predates paging still sends. The whole point of Offset being additive is that
// an absent offset produces byte-for-byte the old request: no -E, no geometry
// lookup, one exec.
func TestCapturePaneContentUnpagedArgvIsUnchanged(t *testing.T) {
	old := previewPaneGeometry
	t.Cleanup(func() { previewPaneGeometry = old })
	previewPaneGeometry = func(string) (int, int, error) {
		t.Fatal("unpaged capture must not ask tmux for pane geometry")
		return 0, 0, nil
	}
	calls := stubTmuxRun(t, "hello\n")

	got, err := capturePaneContent("dev:0.0", DefaultPreviewLimits())
	if err != nil || got != "hello\n" {
		t.Fatalf("content = %q, err = %v", got, err)
	}
	want := []string{"capture-pane", "-p", "-e", "-S", "-2000", "-t", "dev:0.0"}
	if len(*calls) != 1 || !slices.Equal((*calls)[0], want) {
		t.Fatalf("argv = %v, want one call %v", *calls, want)
	}
}

// TestCapturePaneContentPagesWithRange checks the -S/-E window for a page back.
// tmux numbers the first visible line 0, so the newest line is height-1 and the
// scrollback runs -1..-history.
func TestCapturePaneContentPagesWithRange(t *testing.T) {
	stubPaneGeometry(t, 40, 5000)
	calls := stubTmuxRun(t, "older\n")

	limits := PreviewLimits{MaxLines: 100, MaxBytes: 512 << 10, Offset: 250}
	if _, err := capturePaneContent("dev:0.0", limits); err != nil {
		t.Fatalf("err = %v", err)
	}
	// end = 40-1-250 = -211; start = -211-100+1 = -310 (100 lines inclusive).
	want := []string{"capture-pane", "-p", "-e", "-S", "-310", "-E", "-211", "-t", "dev:0.0"}
	if len(*calls) != 1 || !slices.Equal((*calls)[0], want) {
		t.Fatalf("argv = %v, want one call %v", *calls, want)
	}
}

// TestCapturePaneRangeIsContiguousAcrossPages is the property the iOS client
// relies on: the newest line of page n+1 is the line directly above the oldest
// line of page n — no gap the size of the pane, no repeated block.
func TestCapturePaneRangeIsContiguousAcrossPages(t *testing.T) {
	const height, history, lines = 40, 5000, 100
	// Page 0 is the unpaged capture: -S -lines returns lines+height rows and
	// limitPreview keeps the newest `lines` of them, so it ends at height-1.
	page0Start := height - lines
	start, end, ok := capturePaneRange(PreviewLimits{MaxLines: lines, Offset: lines}, height, history)
	if !ok {
		t.Fatal("page 1 reported empty for a pane with history")
	}
	if end != page0Start-1 {
		t.Fatalf("page 1 ends at %d, want %d (directly above page 0's oldest line)", end, page0Start-1)
	}
	if end-start+1 != lines {
		t.Fatalf("page 1 spans %d lines, want %d", end-start+1, lines)
	}
}

// TestCapturePaneContentPastHistoryStartIsEmptyNotAnError: paging further back
// than the pane ever kept is a normal end-of-history, not a failure. It must
// come back as an empty tmux page (HTTP 200, source still "tmux"), never as a
// capture error the handler would turn into a 500 or a source switch.
func TestCapturePaneContentPastHistoryStartIsEmptyNotAnError(t *testing.T) {
	stubPaneGeometry(t, 40, 500)
	calls := stubTmuxRun(t, "must not be captured\n")

	limits := PreviewLimits{MaxLines: 100, MaxBytes: 512 << 10, Offset: 9000}
	got, err := capturePaneContent("dev:0.0", limits)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if got != "" {
		t.Fatalf("content = %q, want empty", got)
	}
	if len(*calls) != 0 {
		t.Fatalf("exhausted range still ran tmux: %v", *calls)
	}
}

// TestCapturePaneContentClampsStartToHistory: a window that reaches past the
// oldest line is trimmed to the oldest line rather than asking tmux for lines
// that never existed.
func TestCapturePaneContentClampsStartToHistory(t *testing.T) {
	stubPaneGeometry(t, 40, 5000)
	calls := stubTmuxRun(t, "oldest\n")

	limits := PreviewLimits{MaxLines: 100, MaxBytes: 512 << 10, Offset: 4990}
	if _, err := capturePaneContent("dev:0.0", limits); err != nil {
		t.Fatalf("err = %v", err)
	}
	// end = 40-1-4990 = -4951; start would be -5050, clamped to -5000.
	want := []string{"capture-pane", "-p", "-e", "-S", "-5000", "-E", "-4951", "-t", "dev:0.0"}
	if len(*calls) != 1 || !slices.Equal((*calls)[0], want) {
		t.Fatalf("argv = %v, want one call %v", *calls, want)
	}
}

func TestCapturePaneContentPropagatesGeometryError(t *testing.T) {
	old := previewPaneGeometry
	t.Cleanup(func() { previewPaneGeometry = old })
	previewPaneGeometry = func(string) (int, int, error) { return 0, 0, os.ErrPermission }

	calls := stubTmuxRun(t, "")

	_, err := capturePaneContent("dev:0.0", PreviewLimits{MaxLines: 100, Offset: 5})
	if err == nil {
		t.Fatal("want the geometry failure surfaced, not a silent empty page")
	}
	if len(*calls) != 0 {
		t.Fatalf("captured with unknown geometry: %v", *calls)
	}
}

// TestTmuxPaneGeometryFormatIsLocaleSafe pins the second tmux format string this
// tool parses. launchd starts the server with no locale, and a tmux client
// without a UTF-8 locale sanitizes everything outside printable ASCII in its
// output — tabs arrive as "_" and a tab-split parser sees nothing (commit
// b8c835d). Two numeric fields separated by a space carry no byte that can be
// rewritten, so this format parses wherever the server is started from.
func TestTmuxPaneGeometryFormatIsLocaleSafe(t *testing.T) {
	calls := stubTmuxRun(t, "40 5000\n")

	h, hist, err := tmuxPaneGeometry("dev:0.0")
	if err != nil || h != 40 || hist != 5000 {
		t.Fatalf("height=%d history=%d err=%v", h, hist, err)
	}
	want := []string{"-u", "display-message", "-p", "-t", "dev:0.0",
		"-F", "#{pane_height} #{history_size}"}
	if len(*calls) != 1 || !slices.Equal((*calls)[0], want) {
		t.Fatalf("argv = %v, want one call %v", *calls, want)
	}
	if strings.ContainsAny((*calls)[0][6], "\t") {
		t.Fatalf("format uses a tab: %q", (*calls)[0][6])
	}
}

func TestParseTmuxPaneGeometry(t *testing.T) {
	h, hist, err := parseTmuxPaneGeometry("40 5000\n")
	if err != nil || h != 40 || hist != 5000 {
		t.Fatalf("height=%d history=%d err=%v", h, hist, err)
	}
	if _, _, err := parseTmuxPaneGeometry("nonsense\n"); err == nil {
		t.Fatal("want an error for output that is not two numbers")
	}
}

func TestDropTrailingLines(t *testing.T) {
	in := "a\nb\nc\n"
	if got := dropTrailingLines(in, 0); got != in {
		t.Fatalf("drop 0 = %q", got)
	}
	if got := dropTrailingLines(in, 1); got != "a\nb\n" {
		t.Fatalf("drop 1 = %q", got)
	}
	if got := dropTrailingLines(in, 3); got != "" {
		t.Fatalf("drop all = %q, want empty", got)
	}
	if got := dropTrailingLines(in, 99); got != "" {
		t.Fatalf("drop past start = %q, want empty", got)
	}
}

// writeNumberedTranscript writes n user entries, "entry-1" … "entry-n", each of
// which renders as three lines (header, body, blank).
func writeNumberedTranscript(t *testing.T, n int) string {
	t.Helper()
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, `{"type":"user","message":{"role":"user","content":"entry-%d"}}`+"\n", i)
	}
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestFormatTranscriptTailUnpagedKeepsTheLastEightEntries pins the fallback a
// caller without an offset gets: the newest transcriptTailEntries entries and
// nothing older, whatever MaxLines allows.
func TestFormatTranscriptTailUnpagedKeepsTheLastEightEntries(t *testing.T) {
	path := writeNumberedTranscript(t, 20)
	got, err := formatTranscriptTail(path, transcriptTailEntries, DefaultPreviewLimits())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(got, "entry-13") || !strings.Contains(got, "entry-20") {
		t.Fatalf("tail is missing the newest eight entries:\n%s", got)
	}
	if strings.Contains(got, "entry-12") {
		t.Fatalf("tail reached past the eight-entry window:\n%s", got)
	}
}

// TestFormatTranscriptTailPagesFurtherBack: an offset is a number of rendered
// lines to skip from the newest end, so a client that received R lines asks for
// offset R and gets the block directly above them — the same rule as the pane.
func TestFormatTranscriptTailPagesFurtherBack(t *testing.T) {
	path := writeNumberedTranscript(t, 20)
	limits := PreviewLimits{MaxLines: 6, MaxBytes: 512 << 10}

	page0, err := formatTranscriptTail(path, transcriptTailEntries, limits)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	page0 = limitPreview(page0, limits) // what the handler returns
	if !strings.Contains(page0, "entry-19") || !strings.Contains(page0, "entry-20") {
		t.Fatalf("page 0 = %q", page0)
	}

	limits.Offset = strings.Count(page0, "\n") // six lines: two entries
	page1, err := formatTranscriptTail(path, transcriptTailEntries, limits)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	page1 = limitPreview(page1, limits)
	if !strings.Contains(page1, "entry-17") || !strings.Contains(page1, "entry-18") {
		t.Fatalf("page 1 = %q, want the two entries above page 0", page1)
	}
	if strings.Contains(page1, "entry-19") || strings.Contains(page1, "entry-16") {
		t.Fatalf("page 1 overlaps or skips: %q", page1)
	}
}

// TestFormatTranscriptTailPagesPastTheEightEntryWindow proves the fixed tail is
// only the *default* depth: with an offset the fallback reaches older entries
// the unpaged call can never show.
func TestFormatTranscriptTailPagesPastTheEightEntryWindow(t *testing.T) {
	path := writeNumberedTranscript(t, 20)
	limits := PreviewLimits{MaxLines: 6, MaxBytes: 512 << 10, Offset: 45} // 15 entries back
	got, err := formatTranscriptTail(path, transcriptTailEntries, limits)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(got, "entry-5") {
		t.Fatalf("deep page = %q, want entry-5", got)
	}
}

// TestFormatTranscriptTailPastStartIsEmpty: paging past the first entry is an
// empty page, not an error and not a repeat of the oldest entry.
func TestFormatTranscriptTailPastStartIsEmpty(t *testing.T) {
	path := writeNumberedTranscript(t, 20)
	limits := PreviewLimits{MaxLines: 100, MaxBytes: 512 << 10, Offset: 10000}
	got, err := formatTranscriptTail(path, transcriptTailEntries, limits)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got != "" {
		t.Fatalf("page past start = %q, want empty", got)
	}
}

// TestFormatTranscriptTailPagedEmptyTranscriptIsEmpty: the "(no user/assistant
// entries)" sentinel belongs to the live tail; a page-back must not serve it as
// if it were history.
func TestFormatTranscriptTailPagedEmptyTranscriptIsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"summary"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	limits := PreviewLimits{MaxLines: 100, MaxBytes: 512 << 10, Offset: 3}
	got, err := formatTranscriptTail(path, transcriptTailEntries, limits)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got != "" {
		t.Fatalf("paged empty transcript = %q, want empty", got)
	}
}

// TestLoadPreviewPagesTheTranscriptFallback runs the offset through the real
// entry point for a session with no pane.
func TestLoadPreviewPagesTheTranscriptFallback(t *testing.T) {
	old := previewTmuxCapture
	t.Cleanup(func() { previewTmuxCapture = old })
	previewTmuxCapture = func(pid int, limits PreviewLimits) (string, string, error) {
		return "", "", errNoTmuxPane
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	sessDir := filepath.Join(home, ".claude", "sessions")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessDir, "78.json"),
		[]byte(`{"pid":78,"sessionId":"sid-preview-paging"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	projDir := filepath.Join(home, ".claude", "projects", "-tmp-proj")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for i := 1; i <= 20; i++ {
		fmt.Fprintf(&b, `{"type":"user","message":{"role":"user","content":"entry-%d"}}`+"\n", i)
	}
	if err := os.WriteFile(filepath.Join(projDir, "sid-preview-paging.jsonl"),
		[]byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	limits := PreviewLimits{MaxLines: 6, MaxBytes: 512 << 10, Offset: 6}
	got, err := LoadPreview(78, limits)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got.Source != "transcript" {
		t.Fatalf("source = %q", got.Source)
	}
	if !strings.Contains(got.Content, "entry-17") || strings.Contains(got.Content, "entry-19") {
		t.Fatalf("paged content = %q", got.Content)
	}
}

// TestLoadPreviewKeepsTmuxSourceForAnEmptyPage: an exhausted pane page stays a
// tmux result. Falling through to the transcript would change source mid-scroll
// and splice two different histories together.
func TestLoadPreviewKeepsTmuxSourceForAnEmptyPage(t *testing.T) {
	old := previewTmuxCapture
	t.Cleanup(func() { previewTmuxCapture = old })
	previewTmuxCapture = func(pid int, limits PreviewLimits) (string, string, error) {
		return "tmux pane dev:0.0", "", nil
	}
	got, err := LoadPreview(42, PreviewLimits{MaxLines: 100, MaxBytes: 4096, Offset: 9000})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got.Source != "tmux" || got.Content != "" {
		t.Fatalf("result = %#v", got)
	}
}
