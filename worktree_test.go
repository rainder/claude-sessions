package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// makeWorktree creates <repo>/.claude/worktrees/<name> with the ".git" FILE a
// linked git worktree carries, and returns the checkout root.
func makeWorktree(t *testing.T, repo, name string) string {
	t.Helper()
	root := filepath.Join(repo, ".claude", "worktrees", name)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: "+repo+"/.git/worktrees/"+name+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestWorktreeRoot(t *testing.T) {
	cases := []struct{ cwd, root, repo string }{
		{"/repo/.claude/worktrees/DR-860", "/repo/.claude/worktrees/DR-860", "/repo"},
		{"/repo/.claude/worktrees/DR-860/sub/dir", "/repo/.claude/worktrees/DR-860", "/repo"},
		{"/Users/andy/Developer/x/.claude/worktrees/feat", "/Users/andy/Developer/x/.claude/worktrees/feat", "/Users/andy/Developer/x"},
		{"/repo/.claude/worktrees/", "", ""},
		{"/repo/not-a-worktree", "", ""},
		{"", "", ""},
	}
	for _, c := range cases {
		if got := worktreeRoot(c.cwd); got != c.root {
			t.Errorf("worktreeRoot(%q) = %q, want %q", c.cwd, got, c.root)
		}
		if got := worktreeRepoRoot(c.cwd); got != c.repo {
			t.Errorf("worktreeRepoRoot(%q) = %q, want %q", c.cwd, got, c.repo)
		}
	}
}

func TestIsGitWorktree(t *testing.T) {
	repo := t.TempDir()
	linked := makeWorktree(t, repo, "DR-860")

	// A directory that matches the path shape but was never registered with
	// git: no ".git" at all.
	bare := filepath.Join(repo, ".claude", "worktrees", "leftover")
	if err := os.MkdirAll(bare, 0o755); err != nil {
		t.Fatal(err)
	}
	// A main checkout: ".git" is a directory, not a file.
	mainCheckout := filepath.Join(repo, ".claude", "worktrees", "mainlike")
	if err := os.MkdirAll(filepath.Join(mainCheckout, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		path string
		want bool
	}{
		{linked, true},
		{bare, false},
		{mainCheckout, false},
		{filepath.Join(repo, "missing"), false},
		{"", false},
	}
	for _, c := range cases {
		if got := isGitWorktree(c.path); got != c.want {
			t.Errorf("isGitWorktree(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

func TestWorktreeSurvivors(t *testing.T) {
	const wt = "/repo/.claude/worktrees/DR-860"
	target := Session{PID: 1, CWD: wt + "/app"}

	cases := []struct {
		name   string
		target Session
		all    []Session
		want   []int // surviving PIDs
	}{
		{
			name:   "other session in same worktree survives",
			target: target,
			all:    []Session{target, {PID: 2, CWD: wt + "/docs"}},
			want:   []int{2},
		},
		{
			name:   "last session leaves nobody",
			target: target,
			all:    []Session{target, {PID: 2, CWD: "/repo"}, {PID: 3, CWD: "/repo/.claude/worktrees/other"}},
			want:   nil,
		},
		{
			name:   "same path on another host is a different machine",
			target: target,
			all:    []Session{target, {PID: 2, CWD: wt, Host: "pi"}},
			want:   nil,
		},
		{
			name:   "tmux siblings die with the kill",
			target: Session{PID: 1, CWD: wt, Tmux: "work:0.0"},
			all: []Session{
				{PID: 1, CWD: wt, Tmux: "work:0.0"},
				{PID: 2, CWD: wt, Tmux: "work:1.0"}, // same tmux session
				{PID: 3, CWD: wt, Tmux: "other:0.0"},
			},
			want: []int{3},
		},
		{
			name:   "target not in a worktree",
			target: Session{PID: 1, CWD: "/repo"},
			all:    []Session{{PID: 1, CWD: "/repo"}, {PID: 2, CWD: "/repo"}},
			want:   nil,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := worktreeSurvivors(c.target, c.all)
			if len(got) != len(c.want) {
				t.Fatalf("survivors = %v, want PIDs %v", got, c.want)
			}
			for i, s := range got {
				if s.PID != c.want[i] {
					t.Fatalf("survivor[%d].PID = %d, want %d", i, s.PID, c.want[i])
				}
			}
		})
	}
}

func TestWorktreeCleanupTarget(t *testing.T) {
	repo := t.TempDir()
	root := makeWorktree(t, repo, "DR-860")
	target := Session{PID: 1, CWD: root}

	if got := worktreeCleanupTarget(target, []Session{target}); got != root {
		t.Errorf("last session: got %q, want %q", got, root)
	}
	occupied := []Session{target, {PID: 2, CWD: root + "/sub"}}
	if got := worktreeCleanupTarget(target, occupied); got != "" {
		t.Errorf("occupied worktree: got %q, want \"\"", got)
	}

	// Path shape without git registration must never be offered.
	bare := filepath.Join(repo, ".claude", "worktrees", "leftover")
	if err := os.MkdirAll(bare, 0o755); err != nil {
		t.Fatal(err)
	}
	unregistered := Session{PID: 3, CWD: bare}
	if got := worktreeCleanupTarget(unregistered, []Session{unregistered}); got != "" {
		t.Errorf("unregistered dir: got %q, want \"\"", got)
	}

	outside := Session{PID: 4, CWD: repo}
	if got := worktreeCleanupTarget(outside, []Session{outside}); got != "" {
		t.Errorf("non-worktree cwd: got %q, want \"\"", got)
	}
}

func TestRemoveWorktreeRunsFromRepoRoot(t *testing.T) {
	var gotRepo string
	var gotArgs []string
	err := removeWorktreeWith("/repo/.claude/worktrees/DR-860",
		func(repoRoot string, args ...string) ([]byte, error) {
			gotRepo, gotArgs = repoRoot, args
			return nil, nil
		})
	if err != nil {
		t.Fatalf("removeWorktreeWith: %v", err)
	}
	if gotRepo != "/repo" {
		t.Errorf("repoRoot = %q, want /repo", gotRepo)
	}
	want := []string{"worktree", "remove", "/repo/.claude/worktrees/DR-860"}
	if strings.Join(gotArgs, " ") != strings.Join(want, " ") {
		t.Errorf("args = %v, want %v", gotArgs, want)
	}
}

func TestRemoveWorktreeSurfacesGitRefusal(t *testing.T) {
	refusal := "fatal: '.claude/worktrees/DR-860' contains modified or untracked files, use --force to delete it"
	err := removeWorktreeWith("/repo/.claude/worktrees/DR-860",
		func(string, ...string) ([]byte, error) {
			return []byte(refusal + "\n"), errors.New("exit status 128")
		})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "contains modified or untracked files") {
		t.Fatalf("error = %q, want git's own message", err)
	}
}

func TestRemoveWorktreeRejectsNonWorktreePath(t *testing.T) {
	called := false
	err := removeWorktreeWith("/repo", func(string, ...string) ([]byte, error) {
		called = true
		return nil, nil
	})
	if err == nil {
		t.Fatal("expected an error for a non-worktree path")
	}
	if called {
		t.Fatal("git ran for a non-worktree path")
	}
}

func TestValidateWorktreePath(t *testing.T) {
	repo := t.TempDir()
	root := makeWorktree(t, repo, "DR-860")

	if err := validateWorktreePath(root); err != nil {
		t.Fatalf("valid worktree rejected: %v", err)
	}

	bad := []struct{ name, path string }{
		{"empty", ""},
		{"relative", ".claude/worktrees/DR-860"},
		{"dot-dot escape", root + "/../../../../etc"},
		{"unclean", root + "/"},
		{"subdirectory not root", root + "/sub"},
		{"outside worktrees", repo},
		{"missing", filepath.Join(repo, ".claude", "worktrees", "ghost")},
	}
	for _, c := range bad {
		if err := validateWorktreePath(c.path); err == nil {
			t.Errorf("%s: %q accepted, want rejection", c.name, c.path)
		}
	}
}

// TestRemoveWorktreeAgainstRealGit exercises the production path (real git, no
// injected runner) so the argument shape and the dirty-tree refusal are checked
// against git itself, not against our idea of it.
func TestRemoveWorktreeAgainstRealGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := filepath.Join(t.TempDir(), "repo")
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	git(repo, "init", "-q", ".")
	git(repo, "commit", "-q", "--allow-empty", "-m", "init")

	// A clean worktree removes.
	clean := filepath.Join(repo, ".claude", "worktrees", "clean")
	git(repo, "worktree", "add", "-q", "-b", "clean", clean)
	if !isGitWorktree(clean) {
		t.Fatalf("%s not recognized as a git worktree", clean)
	}
	if err := RemoveWorktree(clean); err != nil {
		t.Fatalf("RemoveWorktree(clean): %v", err)
	}
	if _, err := os.Stat(clean); !os.IsNotExist(err) {
		t.Fatalf("worktree still on disk after removal (stat err = %v)", err)
	}

	// A worktree with untracked work is refused, and the work survives.
	dirty := filepath.Join(repo, ".claude", "worktrees", "dirty")
	git(repo, "worktree", "add", "-q", "-b", "dirty", dirty)
	scratch := filepath.Join(dirty, "uncommitted.txt")
	if err := os.WriteFile(scratch, []byte("work in progress\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := RemoveWorktree(dirty)
	if err == nil {
		t.Fatal("dirty worktree removed; it must be refused")
	}
	if !strings.Contains(err.Error(), "git worktree remove") {
		t.Fatalf("error = %q, want git's refusal", err)
	}
	if _, statErr := os.Stat(scratch); statErr != nil {
		t.Fatalf("uncommitted file lost: %v", statErr)
	}
}

// TestLocalWorktreeCleanupTargetSkipsNonWorktree: a session outside a worktree
// is answered without collecting anything, so an unreadable HOME can't turn it
// into an error path.
func TestLocalWorktreeCleanupTargetSkipsNonWorktree(t *testing.T) {
	t.Setenv("HOME", filepath.Join(t.TempDir(), "does-not-exist"))
	got, err := localWorktreeCleanupTarget(Session{PID: 1, CWD: "/repo"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Fatalf("got %q, want \"\"", got)
	}
}

// TestLocalWorktreeCleanupTargetIgnoresCallerSnapshot: the answer comes from a
// fresh collect, not from any list the caller holds. With no live sessions on
// this host, an idle worktree is offered for removal.
func TestLocalWorktreeCleanupTargetIgnoresCallerSnapshot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := makeWorktree(t, filepath.Join(home, "repo"), "DR-860")

	got, err := localWorktreeCleanupTarget(Session{PID: 1, CWD: root})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != root {
		t.Fatalf("got %q, want %q", got, root)
	}
}
