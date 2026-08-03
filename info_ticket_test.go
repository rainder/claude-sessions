package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestDetectTicketID(t *testing.T) {
	cases := []struct {
		name string
		cwd  string
		sess string
		want string
	}{
		{"worktree basename match", "/Users/x/repo/.claude/worktrees/DR-860-fix-thing", "irrelevant", "DR-860"},
		{"session name fallback", "/Users/x/repo", "Fix DR-2213 login bug", "DR-2213"},
		{"worktree takes precedence over name", "/Users/x/repo/.claude/worktrees/DR-1", "mentions DR-999 too", "DR-1"},
		{"no match", "/Users/x/repo", "just a normal session", ""},
		{"rejects ADR prefix", "/Users/x/repo/.claude/worktrees/ADR-123", "ADR-123 design doc", ""},
		{"rejects trailing alnum suffix", "/Users/x/repo", "DR-123abc not a ticket", ""},
		{"accepts short and long digit runs", "/Users/x/repo", "DR-12 and DR-123456 both match, first wins", "DR-12"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := detectTicketID(c.cwd, c.sess); got != c.want {
				t.Errorf("detectTicketID(%q, %q) = %q, want %q", c.cwd, c.sess, got, c.want)
			}
		})
	}
}

func TestFetchTicketSummarySuccess(t *testing.T) {
	prevCu, prevClaude := cuFetchFunc, claudeSummarizeFunc
	t.Cleanup(func() { cuFetchFunc, claudeSummarizeFunc = prevCu, prevClaude })

	cuFetchFunc = func(ctx context.Context, id string) ([]byte, error) {
		return []byte("raw ticket text"), nil
	}
	claudeSummarizeFunc = func(ctx context.Context, instruction string, input []byte) ([]byte, error) {
		if instruction != ticketSummaryInstruction {
			t.Errorf("instruction = %q", instruction)
		}
		if string(input) != "raw ticket text" {
			t.Errorf("input = %q", input)
		}
		return []byte("  short summary  \n"), nil
	}

	got, err := fetchTicketSummary(context.Background(), "DR-1")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got.Content != "short summary" {
		t.Errorf("Content = %q, want trimmed summary", got.Content)
	}
	if got.Label != "DR-1" {
		t.Errorf("Label = %q", got.Label)
	}
}

func TestFetchTicketSummaryCuFails(t *testing.T) {
	prevCu := cuFetchFunc
	t.Cleanup(func() { cuFetchFunc = prevCu })
	cuFetchFunc = func(ctx context.Context, id string) ([]byte, error) {
		return nil, errors.New("network down")
	}
	_, err := fetchTicketSummary(context.Background(), "DR-1")
	if err == nil {
		t.Fatal("want non-nil error when cu fetch fails")
	}
}

func TestFetchTicketSummaryClaudeFailsFallsBackToRaw(t *testing.T) {
	prevCu, prevClaude := cuFetchFunc, claudeSummarizeFunc
	t.Cleanup(func() { cuFetchFunc, claudeSummarizeFunc = prevCu, prevClaude })
	cuFetchFunc = func(ctx context.Context, id string) ([]byte, error) {
		return []byte("raw ticket text"), nil
	}
	claudeSummarizeFunc = func(ctx context.Context, instruction string, input []byte) ([]byte, error) {
		return nil, errors.New("rate limited")
	}
	got, err := fetchTicketSummary(context.Background(), "DR-1")
	if err != nil {
		t.Fatalf("err = %v, want nil (raw fallback is not an error)", err)
	}
	if !strings.Contains(got.Content, "raw ticket text") {
		t.Errorf("Content = %q, want it to contain the raw text", got.Content)
	}
	if !strings.Contains(got.Content, "summary unavailable") {
		t.Errorf("Content = %q, want a summary-unavailable note", got.Content)
	}
}

func TestFetchTicketSummaryRawFallbackTruncatesMultilineWithoutGutter(t *testing.T) {
	prevCu, prevClaude := cuFetchFunc, claudeSummarizeFunc
	t.Cleanup(func() { cuFetchFunc, claudeSummarizeFunc = prevCu, prevClaude })

	// Build multi-line raw text longer than ticketRawCap so the cap actually
	// bites, and so trunc's old "\n" -> "\n  │ " gutter rewrite (if it were
	// still in play) would be caught.
	var b strings.Builder
	for b.Len() <= ticketRawCap {
		b.WriteString("line of ticket text\n")
	}
	raw := b.String()

	cuFetchFunc = func(ctx context.Context, id string) ([]byte, error) {
		return []byte(raw), nil
	}
	claudeSummarizeFunc = func(ctx context.Context, instruction string, input []byte) ([]byte, error) {
		return nil, errors.New("rate limited")
	}

	got, err := fetchTicketSummary(context.Background(), "DR-1")
	if err != nil {
		t.Fatalf("err = %v, want nil (raw fallback is not an error)", err)
	}
	if strings.Contains(got.Content, "│") {
		t.Errorf("Content contains gutter marker │, want plain truncation: %q", got.Content)
	}
	// The kept raw prefix (before the dim "…(+N bytes)" suffix and the
	// "[summary unavailable...]" header) must not exceed ticketRawCap bytes.
	idx := strings.Index(got.Content, "\n\n")
	if idx < 0 {
		t.Fatalf("Content = %q, want a header/body separator", got.Content)
	}
	body := got.Content[idx+2:]
	cutIdx := strings.Index(body, "…(+")
	if cutIdx < 0 {
		t.Fatalf("Content body = %q, want a truncation marker", body)
	}
	// cutIdx includes the dim-marker's ANSI SGR escape (kept by
	// sanitizeTerminalText) plus the "  " before the ellipsis, so allow a
	// small fixed slack instead of an exact match.
	if cutIdx < ticketRawCap || cutIdx > ticketRawCap+16 {
		t.Errorf("truncated body length = %d, want ~%d (+small ANSI/space slack)", cutIdx, ticketRawCap)
	}
	if !strings.HasPrefix(body, raw[:ticketRawCap]) {
		t.Errorf("truncated body does not start with the expected raw prefix")
	}
}

func TestFetchTicketSummaryCachedAvoidsRefetch(t *testing.T) {
	prevCu, prevClaude := cuFetchFunc, claudeSummarizeFunc
	t.Cleanup(func() { cuFetchFunc, claudeSummarizeFunc = prevCu, prevClaude })
	var calls int
	cuFetchFunc = func(ctx context.Context, id string) ([]byte, error) {
		calls++
		return []byte("raw"), nil
	}
	claudeSummarizeFunc = func(ctx context.Context, instruction string, input []byte) ([]byte, error) {
		return []byte("summary"), nil
	}
	ticketCache = newSummaryCache(time.Hour, 15*time.Second, 20*time.Second, 64) // fresh cache, isolated from other tests
	fetchTicketSummaryCached(context.Background(), "DR-99")
	fetchTicketSummaryCached(context.Background(), "DR-99")
	if calls != 1 {
		t.Errorf("cu fetch called %d times, want 1", calls)
	}
}
