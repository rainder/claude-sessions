package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// worktreeMarker is the path segment that identifies a Claude Code worktree
// checkout: <repo>/.claude/worktrees/<name>. session.go's worktreeName and
// render.go's collapseWorktreePath key off the same convention.
const worktreeMarker = "/.claude/worktrees/"

// worktreeRoot returns the worktree checkout root for a cwd inside one —
// "/repo/.claude/worktrees/DR-860/sub/dir" → "/repo/.claude/worktrees/DR-860".
// Returns "" when cwd isn't under a worktree, or names no worktree (a bare
// ".../worktrees/" path).
func worktreeRoot(cwd string) string {
	name := worktreeName(cwd)
	if name == "" {
		return ""
	}
	i := strings.Index(cwd, worktreeMarker)
	return cwd[:i+len(worktreeMarker)] + name
}

// worktreeRepoRoot returns the main checkout a worktree cwd belongs to —
// "/repo/.claude/worktrees/DR-860/sub" → "/repo". Empty when cwd isn't under a
// worktree. Git commands aimed at a worktree run from here, never from inside
// the worktree being removed.
func worktreeRepoRoot(cwd string) string {
	if worktreeName(cwd) == "" {
		return ""
	}
	return cwd[:strings.Index(cwd, worktreeMarker)]
}

// isGitWorktree reports whether root is a linked git worktree. A linked
// worktree's ".git" is a FILE holding "gitdir: ..."; the main checkout has a
// directory. A path that merely matches the .claude/worktrees/ shape (a leftover
// directory, a plain folder someone made by hand) is not one, and we never offer
// to git-remove it.
func isGitWorktree(root string) bool {
	if root == "" {
		return false
	}
	info, err := os.Stat(filepath.Join(root, ".git"))
	return err == nil && info.Mode().IsRegular()
}

// gitRootFor walks up from cwd looking for a ".git" entry (directory for a
// main checkout, file for a linked worktree) and returns the directory it's
// in, or "" if cwd isn't inside a git repo. Used at collection time so
// render.go can show a bare repo name instead of a squashed home path.
func gitRootFor(cwd string) string {
	dir := cwd
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// worktreeSurvivors returns the sessions that would still be running in
// target's worktree after target is killed. Empty means the kill empties the
// worktree.
//
// Excluded from the count: target itself; sessions on another host (each host
// has its own path namespace, so an identical cwd elsewhere is unrelated); and
// target's tmux siblings — a tmux-backed kill runs `tmux kill-session`, which
// takes down every pane in that session, so those sessions die with it.
//
// Returns nil when target isn't in a worktree at all.
func worktreeSurvivors(target Session, all []Session) []Session {
	root := worktreeRoot(target.CWD)
	if root == "" {
		return nil
	}
	tmuxName := ""
	if target.Tmux != "" {
		if n, err := tmuxSessionName(target.Tmux); err == nil {
			tmuxName = n
		}
	}
	var survivors []Session
	for _, s := range all {
		if s.Host != target.Host || s.PID == target.PID {
			continue
		}
		if worktreeRoot(s.CWD) != root {
			continue
		}
		if tmuxName != "" && s.Tmux != "" {
			if n, err := tmuxSessionName(s.Tmux); err == nil && n == tmuxName {
				continue
			}
		}
		survivors = append(survivors, s)
	}
	return survivors
}

// worktreeCleanupTarget reports the worktree that a kill of target would leave
// empty, or "" when there's nothing to offer: target isn't in a worktree, the
// path isn't a real git worktree, or other sessions keep it occupied.
func worktreeCleanupTarget(target Session, all []Session) string {
	root := worktreeRoot(target.CWD)
	if root == "" || !isGitWorktree(root) {
		return ""
	}
	if len(worktreeSurvivors(target, all)) > 0 {
		return ""
	}
	return root
}

// localWorktreeCleanupTarget answers the cleanup question for a local kill by
// re-collecting this host's sessions. It deliberately does NOT trust a caller's
// snapshot: the TUI's rows are filtered by the active group and text query, and
// a session hidden by a filter would read as "no survivors" — `git worktree
// remove` doesn't care that a live process is sitting in the checkout, so that
// mistake pulls the ground out from under a running session.
//
// Sessions outside a worktree skip the collect entirely. A collection error
// yields no offer plus the error, so the caller can say why it stayed quiet.
func localWorktreeCleanupTarget(target Session) (string, error) {
	if worktreeRoot(target.CWD) == "" {
		return "", nil
	}
	all, err := CollectLocal()
	if err != nil {
		return "", err
	}
	return worktreeCleanupTarget(target, all), nil
}

// worktreeRunner runs the git command RemoveWorktree needs, injected so tests
// never shell out. It returns combined output alongside the error so git's own
// refusal ("contains modified or untracked files") reaches the user verbatim.
type worktreeRunner func(repoRoot string, args ...string) ([]byte, error)

var defaultWorktreeRunner worktreeRunner = func(repoRoot string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"-C", repoRoot}, args...)...)
	return cmd.CombinedOutput()
}

// RemoveWorktree removes the worktree checkout at path via git, running from
// the main checkout rather than from inside the worktree. No --force: when the
// tree is dirty or holds untracked files, git refuses and that refusal is what
// the caller shows. Nothing in this path can discard uncommitted work.
func RemoveWorktree(path string) error {
	return removeWorktreeWith(path, defaultWorktreeRunner)
}

func removeWorktreeWith(path string, run worktreeRunner) error {
	repoRoot := worktreeRepoRoot(path)
	if repoRoot == "" {
		return fmt.Errorf("not a worktree path: %s", path)
	}
	out, err := run(repoRoot, "worktree", "remove", path)
	if err == nil {
		return nil
	}
	msg := strings.TrimSpace(string(out))
	if !strings.Contains(msg, "locked working tree") {
		if msg == "" {
			return fmt.Errorf("git worktree remove: %w", err)
		}
		return fmt.Errorf("git worktree remove: %s", msg)
	}
	// Session left the tree administratively locked (e.g. a prior kill's
	// worktree-remove raced with tmux teardown). Unlocking doesn't touch tree
	// contents, so the retry below still refuses on dirty/untracked files —
	// the --force-free guarantee above holds either way.
	if _, unlockErr := run(repoRoot, "worktree", "unlock", path); unlockErr != nil {
		return fmt.Errorf("git worktree remove: %s", msg)
	}
	out, err = run(repoRoot, "worktree", "remove", path)
	if err != nil {
		msg = strings.TrimSpace(string(out))
		if msg == "" {
			return fmt.Errorf("git worktree remove: %w", err)
		}
		return fmt.Errorf("git worktree remove: %s", msg)
	}
	return nil
}

// validateWorktreePath vets a worktree path that arrived over HTTP before
// anything touches disk: absolute, already clean (so no "..", no doubled
// separators), shaped like a worktree root, and actually a registered git
// worktree on this host.
func validateWorktreePath(path string) error {
	if path == "" {
		return fmt.Errorf("missing worktree path")
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("worktree path must be absolute and clean")
	}
	if worktreeRoot(path) != path {
		return fmt.Errorf("not a worktree root: %s", path)
	}
	if !isGitWorktree(path) {
		return fmt.Errorf("not a git worktree: %s", path)
	}
	return nil
}
