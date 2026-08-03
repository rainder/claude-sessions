package main

import "testing"

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
