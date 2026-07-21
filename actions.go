package main

import (
	"fmt"
	"strings"
	"time"

	"golang.org/x/term"
)

// actCtx is the runtime state passed to action handlers.
type actCtx struct {
	fd       int
	oldState *term.State       // for switching back to cooked mode
	targets  []selectionTarget // current snapshot
	sel      string            // selected target ID

	// pause/resume suspend the background pollers (remote + usage hubs)
	// while an external program owns the terminal — nothing renders, so
	// fetching would be wasted traffic. Either may be nil.
	pause  func()
	resume func()
}

// runInteractive hands the terminal to prog with the pollers suspended,
// resuming them (with an immediate refetch) when the program exits.
func (c *actCtx) runInteractive(prog string, args ...string) error {
	if c.pause != nil {
		c.pause()
	}
	if c.resume != nil {
		defer c.resume()
	}
	return runInteractive(c.fd, c.oldState, prog, args...)
}

// selectedTarget returns the currently-selected target, or nil if sel doesn't
// resolve to anything in the current snapshot.
func (c *actCtx) selectedTarget() *selectionTarget {
	for i := range c.targets {
		if c.targets[i].id == c.sel {
			return &c.targets[i]
		}
	}
	return nil
}

// selected returns the currently-selected session, or nil if sel doesn't
// resolve to a session-backed target (e.g. an empty remote-host row).
func (c *actCtx) selected() *Session {
	target := c.selectedTarget()
	if target == nil {
		return nil
	}
	return target.session
}

// selectedRemoteNewTarget reports the host and default cwd for spawning a new
// remote session on the selected row. A populated remote row supplies its cwd;
// an empty remote-host row has none. Returns ok=false for no selection or a
// local row.
func (c *actCtx) selectedRemoteNewTarget() (host, defaultCWD string, ok bool) {
	target := c.selectedTarget()
	if target == nil || target.host == "" {
		return "", "", false
	}
	if target.session != nil {
		defaultCWD = target.session.CWD
	}
	return target.host, defaultCWD, true
}

// actKill confirms then kills the selected session. Tmux-aware: kills the
// whole tmux session when the pid is in a pane.
func actKill(c *actCtx) {
	s := c.selected()
	if s == nil {
		return
	}
	if s.Host != "" {
		actKillRemote(c)
		return
	}
	enterCooked(c.fd, c.oldState)
	defer enterRaw(c.fd)

	var prompt string
	if s.Tmux != "" {
		sessName := strings.SplitN(s.Tmux, ":", 2)[0]
		prompt = fmt.Sprintf("\nkill tmux session %q (PID %d)? [y/N] ", sessName, s.PID)
	} else {
		prompt = fmt.Sprintf("\nkill PID %d? [y/N] ", s.PID)
	}
	if !confirm(prompt) {
		return
	}
	if err := KillSession(s.PID); err != nil {
		fmt.Printf("\nkill failed: %v\n", err)
		pauseForKey(c.fd, c.oldState)
	}
}

// actAttach attaches to the tmux session containing the selected pid. If the
// session isn't in tmux, offers to migrate first.
func actAttach(c *actCtx) {
	s := c.selected()
	if s == nil {
		return
	}
	if s.Host != "" {
		actAttachRemote(c)
		return
	}
	if s.Tmux != "" {
		sessName := strings.SplitN(s.Tmux, ":", 2)[0]
		runTmuxAttach(c, sessName)
		return
	}
	// Not in tmux — offer migration.
	enterCooked(c.fd, c.oldState)
	prompt := fmt.Sprintf("\nPID %d is not in tmux. Migrate (kill + resume in tmux) first? [y/N] ", s.PID)
	if !confirm(prompt) {
		enterRaw(c.fd)
		return
	}
	fmt.Printf("\nmigrating PID %d... ", s.PID)
	tname, err := MigrateLocal(s.PID)
	if err != nil {
		fmt.Printf("\nmigrate failed: %v\n", err)
		pauseForKey(c.fd, c.oldState)
		enterRaw(c.fd)
		return
	}
	fmt.Printf("ok → %s\n", tname)
	enterRaw(c.fd)
	runTmuxAttach(c, tname)
}

// runTmuxAttach exits the UI, runs `tmux attach -t <sess>` (or switch-client
// if we're inside tmux), then re-enters the UI when the user detaches.
func runTmuxAttach(c *actCtx, sessName string) {
	if os, _ := isInsideTmux(); os {
		_ = c.runInteractive("tmux", "switch-client", "-t", sessName)
		return
	}
	_ = c.runInteractive("tmux", "attach", "-t", sessName)
}

// actPreview opens a full-screen preview that auto-refreshes alongside the
// caller's interval. Returns on q/p/ESC/Ctrl-C.
func actPreview(c *actCtx, interval time.Duration) {
	s := c.selected()
	if s == nil {
		return
	}
	if s.Host != "" {
		actPreviewRemote(c, interval)
		return
	}
	render := func() {
		fmt.Print("\033[H\033[J")
		fmt.Printf("%sPreview: PID %d%s  %s(q/p=back · r=refresh · auto-refresh %s)%s\n\n",
			ansiBold, s.PID, ansiReset, ansiDim, interval, ansiReset)
		fmt.Print(PreviewContent(s.PID))
	}
	render()
	nextTick := time.Now().Add(interval)
	for {
		timeout := time.Until(nextTick)
		if timeout <= 0 {
			render()
			nextTick = time.Now().Add(interval)
			continue
		}
		events, _ := readEvents(timeout, -1)
		if len(events) == 0 {
			render()
			nextTick = time.Now().Add(interval)
			continue
		}
		for _, k := range events {
			switch k {
			case "q", "Q", "p", "P", KeyEsc, "\x03":
				return
			case "r", "R":
				render()
			}
		}
	}
}

// actNew prompts for a cwd (with picker of recent + history), then spawns a
// new tmux+claude session there and attaches to it. If the selected row is
// remote, asks the remote server to spawn it via /sessions/new.
func actNew(c *actCtx) {
	if host, defaultCWD, ok := c.selectedRemoteNewTarget(); ok {
		actNewRemote(c, host, defaultCWD)
		return
	}
	picker := buildCwdPicker(c.selected())
	start := 0
	lines := make([]string, 0, len(picker.entries)+1)
	for i, p := range picker.entries {
		if p.isDefault {
			start = i
		}
		freq := ""
		if p.count > 0 {
			freq = "  " + dim(fmt.Sprintf("(%d)", p.count))
		}
		lines = append(lines, fmt.Sprintf("%-50s%s", picker.shortName(p.cwd), freq))
	}
	lines = append(lines, "enter path manually…")
	idx := pickMenu("New tmux+claude session",
		"↑/↓ move · Enter select · q cancel", lines, start)
	if idx < 0 {
		return
	}

	enterCooked(c.fd, c.oldState)
	defer enterRaw(c.fd)

	var cwd string
	if idx < len(picker.entries) {
		cwd = picker.entries[idx].cwd
	} else {
		input := readLine("\ncwd path (q=cancel) > ")
		if input == "" || input == "q" || input == "Q" {
			return
		}
		cwd = expandTilde(input)
	}
	if !isDir(cwd) {
		fmt.Printf("\nnot a directory: %s\n", cwd)
		pauseForKey(c.fd, c.oldState)
		return
	}
	fmt.Printf("\nspawning in %s... ", cwd)
	tname, err := SpawnNew(cwd, "")
	if err != nil {
		fmt.Printf("failed: %v\n", err)
		pauseForKey(c.fd, c.oldState)
		return
	}
	fmt.Printf("ok → %s\n", tname)
	enterRaw(c.fd)
	runTmuxAttach(c, tname)
}
