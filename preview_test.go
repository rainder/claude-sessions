package main

import (
	"os"
	"path/filepath"
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
