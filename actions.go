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
	oldState *term.State // for switching back to cooked mode
	sessions []Session   // current snapshot
	sel      string      // selected session ID
}

// selected returns the currently-selected session, or nil if sel doesn't
// resolve to anything in the current snapshot.
func (c *actCtx) selected() *Session {
	for i := range c.sessions {
		if c.sessions[i].ID() == c.sel {
			return &c.sessions[i]
		}
	}
	return nil
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
		fmt.Println("\naborted")
		pauseForKey(c.fd, c.oldState)
		return
	}
	if err := KillSession(s.PID); err != nil {
		fmt.Printf("\nkill failed: %v\n", err)
	} else {
		fmt.Println("\nkilled")
	}
	pauseForKey(c.fd, c.oldState)
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
		fmt.Println("\naborted")
		pauseForKey(c.fd, c.oldState)
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
		_ = runInteractive(c.fd, c.oldState, "tmux", "switch-client", "-t", sessName)
		return
	}
	_ = runInteractive(c.fd, c.oldState, "tmux", "attach", "-t", sessName)
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
	for {
		events := readEvents(interval)
		if len(events) == 0 {
			render() // tick
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
	if s := c.selected(); s != nil && s.Host != "" {
		actNewRemote(c)
		return
	}
	enterCooked(c.fd, c.oldState)
	defer enterRaw(c.fd)

	picker := buildCwdPicker(c.selected())
	fmt.Print("\n" + bold("New tmux+claude session") + "\n\n")
	for i, p := range picker.entries {
		marker := ""
		if p.isDefault {
			marker = "  " + dim("← selected")
		}
		freq := ""
		if p.count > 0 {
			freq = dim(fmt.Sprintf("(%d)", p.count))
		}
		fmt.Printf("  %s%2d)%s  %-50s  %s%s\n",
			ansiBold, i+1, ansiReset, picker.shortName(p.cwd), freq, marker)
	}
	fmt.Println()
	input := readLine("cwd (#, path, Enter=#1, q=cancel) > ")

	var cwd string
	switch {
	case input == "q" || input == "Q":
		return
	case input == "":
		if len(picker.entries) == 0 {
			fmt.Println("no default")
			pauseForKey(c.fd, c.oldState)
			return
		}
		cwd = picker.entries[0].cwd
	default:
		if n, ok := parseIndex(input); ok && n >= 1 && n <= len(picker.entries) {
			cwd = picker.entries[n-1].cwd
		} else {
			cwd = expandTilde(input)
		}
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

