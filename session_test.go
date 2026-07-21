package main

import (
	"testing"
	"time"
)

func TestDisplayName(t *testing.T) {
	const wtCWD = "/Users/andy/Developer/trecs-brain/.claude/worktrees/extraction-tables-unify"
	cases := []struct {
		name      string
		s         Session
		wantLabel string
		wantDim   bool
	}{
		{"user-set is bright", Session{Name: "my-task", NameSource: "user"}, "my-task", false},
		{"missing source treated as user-set", Session{Name: "my-task"}, "my-task", false},
		{"derived is dimmed", Session{Name: "trecs-brain-84", NameSource: "derived"}, "trecs-brain-84", true},
		{"worktree beats derived", Session{Name: "trecs-brain-84", NameSource: "derived", CWD: wtCWD}, "extraction-tables-unify", true},
		{"tmux never used as fallback", Session{Tmux: "trecs-brain-84:0.1"}, "-", true},
		{"derived beats tmux", Session{Name: "trecs-brain-84", NameSource: "derived", Tmux: "sess:0.0"}, "trecs-brain-84", true},
		{"worktree fallback dimmed", Session{CWD: wtCWD}, "extraction-tables-unify", true},
		{"worktree beats tmux", Session{Tmux: "sess:0.0", CWD: wtCWD}, "extraction-tables-unify", true},
		{"last resort dash", Session{CWD: "/tmp/plain"}, "-", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			label, dimmed := c.s.DisplayName()
			if label != c.wantLabel || dimmed != c.wantDim {
				t.Errorf("DisplayName() = (%q, %v), want (%q, %v)", label, dimmed, c.wantLabel, c.wantDim)
			}
		})
	}
}

func TestWorktreeName(t *testing.T) {
	cases := []struct {
		cwd, want string
	}{
		{"/Users/andy/Developer/trecs-brain/.claude/worktrees/extraction-tables-unify", "extraction-tables-unify"},
		{"/repo/.claude/worktrees/DR-860/sub/dir", "DR-860"},
		{"/repo/.claude/worktrees/", ""},
		{"/repo/not-a-worktree", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := worktreeName(c.cwd); got != c.want {
			t.Errorf("worktreeName(%q) = %q, want %q", c.cwd, got, c.want)
		}
	}
}

func TestUpdatedPrefersUpdatedAt(t *testing.T) {
	s := Session{StartedAt: 1781093160434, UpdatedAt: 1781093170000}
	if got, want := s.Updated(), time.UnixMilli(1781093170000); !got.Equal(want) {
		t.Errorf("Updated() = %v, want %v", got, want)
	}
}

func TestUpdatedFallsBackToStartedAt(t *testing.T) {
	// Headless sessions (entrypoint "sdk-cli") never write updatedAt; their
	// age must come from startedAt, not the epoch.
	s := Session{StartedAt: 1781093160434}
	if got, want := s.Updated(), time.UnixMilli(1781093160434); !got.Equal(want) {
		t.Errorf("Updated() = %v, want %v", got, want)
	}
}

func TestHeadless(t *testing.T) {
	cases := []struct {
		entrypoint string
		want       bool
	}{
		{"cli", false},
		{"", false},
		{"sdk-cli", true},
		{"sdk-ts", true},
	}
	for _, c := range cases {
		s := Session{Entrypoint: c.entrypoint}
		if got := s.Headless(); got != c.want {
			t.Errorf("Headless() with entrypoint %q = %v, want %v", c.entrypoint, got, c.want)
		}
	}
}

func TestSortSessions(t *testing.T) {
	// "/alpha" and "/Beta" differ only by case, so dir order is case-insensitive.
	// StartedAt / UpdatedAt are deliberately out of step so each mode reorders
	// the trio differently.
	fixture := []Session{
		{SessionID: "s1", CWD: "/Beta", StartedAt: 100, UpdatedAt: 150},
		{SessionID: "s2", CWD: "/alpha", StartedAt: 300, UpdatedAt: 250},
		{SessionID: "s3", CWD: "/alpha", StartedAt: 200, UpdatedAt: 350},
	}
	cases := []struct {
		mode string
		want []string
	}{
		{"dir", []string{"s2", "s3", "s1"}},         // cwd asc, StartedAt desc tiebreak
		{"created", []string{"s2", "s3", "s1"}},     // StartedAt desc
		{"created-asc", []string{"s1", "s3", "s2"}}, // StartedAt asc
		{"updated", []string{"s3", "s2", "s1"}},     // Updated() desc
		{"updated-asc", []string{"s1", "s2", "s3"}}, // Updated() asc
		{"bogus", []string{"s2", "s3", "s1"}},       // unknown => dir
	}
	for _, c := range cases {
		rows := append([]Session(nil), fixture...)
		SortSessions(rows, c.mode)
		got := make([]string, len(rows))
		for i, s := range rows {
			got[i] = s.SessionID
		}
		if !equalStrings(got, c.want) {
			t.Errorf("SortSessions(%q) = %v, want %v", c.mode, got, c.want)
		}
	}
}

func TestSortSessionsStable(t *testing.T) {
	// Rows tied on the sort key must keep their input order.
	rows := []Session{
		{SessionID: "a", StartedAt: 100},
		{SessionID: "b", StartedAt: 100},
		{SessionID: "c", StartedAt: 100},
	}
	SortSessions(rows, "created")
	got := make([]string, len(rows))
	for i, s := range rows {
		got[i] = s.SessionID
	}
	if want := []string{"a", "b", "c"}; !equalStrings(got, want) {
		t.Errorf("SortSessions stability = %v, want %v", got, want)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
