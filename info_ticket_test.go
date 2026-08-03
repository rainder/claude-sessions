package main

import (
	"context"
	"errors"
	"strings"
	"testing"
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
