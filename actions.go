package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// terminalOutput is where actCtx.enterRaw writes the mouse-enable sequence when
// returning to the main TUI. A package var (defaulting to os.Stdout) so tests
// can capture the write; restore it with t.Cleanup.
var terminalOutput io.Writer = os.Stdout

// actCtx is the runtime state passed to action handlers.
type actCtx struct {
	fd       int
	oldState *term.State       // for switching back to cooked mode
	targets  []selectionTarget // current snapshot
	sel      string            // selected target ID

	// pause/resume suspend the background pollers (remote, account-usage,
	// and host-usage hubs) while an external program owns the terminal —
	// nothing renders, so fetching would be wasted traffic. Either may be nil.
	pause  func()
	resume func()

	// modalWakes are the foreground-only wake sources used while an action owns
	// a fullscreen modal. RunTUI supplies its resize pipe but excludes data hubs.
	modalWakes []wakeFD

	// spawnedHost/spawnedTmux record a tmux session just created by this
	// action (actNew / actNewRemote), so the caller can re-target the
	// selection onto it once a post-action refresh picks it up. Empty when
	// no new session was spawned (cancelled, or spawn failed).
	spawnedHost string
	spawnedTmux string

	// killJob is set by actKill once a kill is confirmed: the kill itself
	// (and any worktree cleanup) runs in the background rather than
	// blocking the TUI, so the caller (tui.go) tracks the job and applies
	// its result via finishKillJob once it completes. Nil when actKill
	// didn't start one (no selection, remote row, cancelled, or a
	// pre-confirm failure).
	killJob *killJob

	// killInFlight reports whether a background kill is already running for
	// (host, pid) — the killed row stays in the list until its job lands, so
	// actKill uses this to refuse a duplicate confirm on the same session
	// rather than racing a second kill against the first. Nil in tests (and
	// any caller) that don't exercise background kills, in which case actKill
	// never refuses on this basis.
	killInFlight func(host string, pid int) bool
	// spawnedBackground is set when the just-spawned session (above) was
	// launched with a prompt via the 'p' overlay: it's already running
	// unattended, so the caller shows a toast instead of attaching.
	spawnedBackground bool

	// accounts reports one host's known-account picture ("" = this machine) from
	// what the pollers already hold, so opening the Ctrl+W picker never triggers
	// a fetch. Nil in tests (and any caller) that don't exercise the picker, in
	// which case Ctrl+W does nothing.
	accounts func(host string) accountSnapshot

	// flags is this host's FlagsStore, written directly for local rows
	// (Host == ""). Remote rows never write it — they go through
	// actToggleDisabledRemote / actSetGroupRemote instead. May be nil in tests
	// that don't exercise it.
	flags *FlagsStore
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

// enterRaw returns to the main raw-mode TUI and re-enables mouse reporting.
// Bare enterRaw(fd) is deliberately mouse-neutral (see helpers.go), so every
// action handler that finishes a cooked prompt or subprocess must re-enable
// it; centralizing that here keeps the calls paired.
func (c *actCtx) enterRaw() {
	enterRaw(c.fd)
	writeMouseMode(terminalOutput, true)
}

// prepareLineOutput parks the cursor in a cleared bottom row before restoring
// cooked mode for an action prompt or status message. screenRenderer leaves the
// cursor on its last patch, which may be inside the session list or a picker.
func (c *actCtx) prepareLineOutput() {
	_, rows, err := term.GetSize(c.fd)
	if err != nil {
		rows = 0
	}
	writeActionOutputPosition(terminalOutput, rows)
	enterCooked(c.fd, c.oldState)
}

// writeActionOutputPosition clears the row where a leading newline from an
// action prompt will land, then parks the cursor immediately above it. A
// one-row terminal cannot avoid scrolling; unknown size uses a bottom-clamped
// fallback instead of moving to the home position.
func writeActionOutputPosition(w io.Writer, rows int) {
	switch {
	case rows > 1:
		_, _ = fmt.Fprintf(w, "\x1b[%d;1H\x1b[K\x1b[%d;1H", rows, rows-1)
	case rows == 1:
		_, _ = io.WriteString(w, "\x1b[1;1H\x1b[K")
	default:
		_, _ = io.WriteString(w, "\x1b[9999;1H\x1b[K\x1b[1A\r")
	}
}

// selectedTarget returns the currently-selected target, or nil if sel doesn't
// resolve to anything in the current snapshot.
func (c *actCtx) selectedTarget() *selectionTarget {
	return findSelectionTarget(c.targets, c.sel)
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

// actToggleDisabled flips the disabled flag for the selected session. Local
// rows (Host == "") write directly to this host's FlagsStore — an
// instant local write with no HTTP, mirroring the local branch every other
// action (actKill, actAttach) takes. Remote rows delegate to
// actToggleDisabledRemote, which makes the same write over HTTP against the
// row's own host. Either way the store write is authoritative: nothing
// in-memory is patched here (c.selected() points into a throwaway targets
// copy), so the caller MUST settleRows()/refresh() afterwards to re-overlay
// the new value onto the live rows. A row with no selection or no stable
// SessionID is ignored (it can't be keyed). A store that cannot persist the
// change (no config dir, a file that failed to parse) says so in the same
// one-line action error the remote path uses — silence would leave the key
// looking dead for the rest of the process's life. Reports whether anything
// changed.
func actToggleDisabled(c *actCtx) bool {
	session := c.selected()
	if session == nil || session.SessionID == "" {
		return false
	}
	if session.Host != "" {
		return actToggleDisabledRemote(c)
	}
	if c.flags != nil && !c.flags.SetDisabled(session.SessionID, !session.Disabled) {
		showActionError(c, "disable", c.flags.saveError())
		return false
	}
	return true
}

// actSetGroup assigns the selected session to group (1..9), or ungroups it
// when it already carries that group — the single-membership toggle Shift+1..9
// has always had, now resolved against the row's own flag rather than a
// client-local store. Same split as actToggleDisabled, for the same reason:
// the group lives on the host that owns the session, so a local row writes
// this host's FlagsStore directly and a remote row goes over HTTP to its own
// host. The write is authoritative; the caller must refresh to see it. A store
// that cannot persist the change reports it the same way the remote path
// reports a refusal from its host — one line, then back to the list.
func actSetGroup(c *actCtx, group int) bool {
	session := c.selected()
	if session == nil || session.SessionID == "" {
		return false
	}
	if session.Host != "" {
		return actSetGroupRemote(c, group)
	}
	if c.flags != nil && !c.flags.SetGroup(session.SessionID, toggleGroup(session.Group, group)) {
		showActionError(c, "group", c.flags.saveError())
		return false
	}
	return true
}

func showActionError(c *actCtx, label string, err error) {
	c.prepareLineOutput()
	fmt.Printf("\n%s: %v\n", label, err)
	pauseForKey(c.fd, c.oldState)
	c.enterRaw()
}

// actKill confirms then kills the selected session. Tmux-aware: kills the
// whole tmux session when the pid is in a pane.
func actKill(c *actCtx) {
	s := c.selected()
	if s == nil {
		return
	}
	if c.killInFlight != nil && c.killInFlight(s.Host, s.PID) {
		// A background kill for this row is already running — the row stays
		// in the list until it lands, so a second Ctrl+X here would race a
		// still-live localReattest and re-attempt worktree cleanup the first
		// job already handled.
		return
	}
	if s.Host != "" {
		actKillRemote(c)
		return
	}

	var question string
	if s.Tmux != "" {
		sessName, err := tmuxSessionName(s.Tmux)
		if err != nil {
			c.prepareLineOutput()
			fmt.Printf("\nkill failed: %v\n", err)
			pauseForKey(c.fd, c.oldState)
			c.enterRaw()
			return
		}
		question = fmt.Sprintf("kill tmux session %q (PID %d)?", sessName, s.PID)
	} else {
		question = fmt.Sprintf("kill PID %d?", s.PID)
	}
	if s.NotIdle() {
		question = colorize(statusColor[s.Status], fmt.Sprintf("⚠ session is %s, not idle — killing will interrupt it", s.StatusDisplay())) + "\n" + question
	}
	pane := startLocalPreview(*s)
	confirmed := confirmOverlayPreview(question, pane, c.modalWakes, s.NotIdle())
	pane.close()
	if !confirmed {
		return
	}
	// Resolve the worktree question before the kill — afterwards the session is
	// in no list to reason about. A failed collect simply means no offer.
	worktree, _ := localWorktreeCleanupTarget(*s)
	// The kill itself (and any worktree cleanup) runs in the background:
	// KillSession can block up to 5s waiting for the tmux pane's process to
	// actually exit (killSessionWith, migrate.go), and the confirm overlay
	// already ran without leaving raw mode, so there is nothing left to tear
	// down here — the caller picks up c.killJob and reports progress via the
	// toast instead of a blocking cooked-mode dialog.
	c.killJob = startLocalKillJob(*s, worktree)
}

// finishKillJob applies a completed background kill's result and returns the
// toast text to show, or "" when the result was already reported some other
// way (the interactive resurrect prompt below, or a successful attach into
// the resumed worktree). A worktree left dirty by a LOCAL kill still needs a
// human decision, which is why this — unlike the kill itself — runs
// synchronously on the main loop: it owns the raw/cooked terminal handoff
// that a background goroutine can't safely perform (see CLAUDE.md's "single
// stdin consumer" invariant). A remote kill has no such prompt — resurrecting
// into a worktree needs local git state, so a dirty remote worktree just
// reports the failure in the toast, same as actKillRemote always has.
func finishKillJob(c *actCtx, res killJobResult) string {
	if res.err != nil {
		return fmt.Sprintf("kill failed: %v", res.err)
	}
	label, _ := res.session.DisplayName()
	if res.worktree == "" || res.removeErr == nil {
		return fmt.Sprintf("killed %s", label)
	}
	// A remote kill cannot be resurrected (that needs local git state over
	// HTTP), and neither can a grok one: the resurrect step is
	// ResumeSessionInWorktree, i.e. `claude --resume <id>`, which would spawn
	// a Claude Code session on a grok transcript id. Both just report.
	if res.session.Host != "" || res.session.IsGrok() {
		return fmt.Sprintf("killed %s, worktree removal failed: %v", label, res.removeErr)
	}
	c.prepareLineOutput()
	defer c.enterRaw()
	fmt.Printf("\nremoving worktree... failed: %v\n", res.removeErr)
	if confirm("resurrect the killed session in this worktree? [y/N] ") {
		repoRoot := worktreeRepoRoot(res.worktree)
		name := worktreeName(res.worktree)
		fmt.Print("resuming... ")
		tname, rerr := ResumeSessionInWorktree(res.session.SessionID, repoRoot, name)
		if rerr != nil {
			fmt.Printf("failed: %v\n", rerr)
		} else {
			fmt.Printf("ok → %s\n", tname)
			c.spawnedTmux = tname
			c.enterRaw()
			runTmuxAttach(c, tname)
			return ""
		}
	}
	pauseForKey(c.fd, c.oldState)
	return ""
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
	// Not in tmux — offer migration, unless migration is not a thing this
	// session can do. Migrating means respawning as `claude --resume <id>`,
	// which has no grok equivalent, so say so rather than offering a
	// confirmation that can only end in a refusal.
	if s.IsGrok() {
		showActionError(c, "attach", errMigrateUnsupportedTool)
		return
	}
	question := fmt.Sprintf("PID %d is not in tmux. Migrate (kill + resume in tmux) first?", s.PID)
	if s.NotIdle() {
		question = colorize(statusColor[s.Status], fmt.Sprintf("⚠ session is %s, not idle — migrating will interrupt it", s.StatusDisplay())) + "\n" + question
	}
	pane := startLocalPreview(*s)
	confirmed := confirmOverlayPreview(question, pane, c.modalWakes, s.NotIdle())
	pane.close()
	if !confirmed {
		return
	}
	c.prepareLineOutput()
	fmt.Printf("\nmigrating PID %d... ", s.PID)
	tname, err := MigrateLocalAttested(s.PID, s.SessionID)
	if err != nil {
		fmt.Printf("\nmigrate failed: %v\n", err)
		pauseForKey(c.fd, c.oldState)
		c.enterRaw()
		return
	}
	fmt.Printf("ok → %s\n", tname)
	c.enterRaw()
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

// actNew prompts for a cwd (with picker of recent + history) and a command
// preset, then spawns a new tmux session there and attaches to it. If the
// selected row is remote, asks the remote server to spawn it via /sessions/new.
func actNew(c *actCtx) {
	if host, defaultCWD, ok := c.selectedRemoteNewTarget(); ok {
		actNewRemote(c, host, defaultCWD)
		return
	}
	presets, err := LoadCommandPresets()
	if err != nil {
		c.prepareLineOutput()
		fmt.Printf("\nload commands: %v\n", err)
		pauseForKey(c.fd, c.oldState)
		c.enterRaw()
		return
	}
	presetStart := LoadCommandPresetIndex(presets)

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
	row, presetIndex, prompt, ok := pickNewSession("New tmux session", lines, start, presets, presetStart, "", c.modalWakes)
	if !ok {
		return
	}
	preset := presets[presetIndex]
	SaveCommandPresetName(preset.Name)

	c.prepareLineOutput()
	defer c.enterRaw()

	var cwd string
	if row < len(picker.entries) {
		cwd = picker.entries[row].cwd
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
	command := preset.Command
	if prompt != "" {
		command = command + " " + shellQuote(prompt)
	}
	fmt.Printf("\nspawning in %s... ", cwd)
	tname, err := SpawnNew(cwd, "", command)
	if err != nil {
		fmt.Printf("failed: %v\n", err)
		pauseForKey(c.fd, c.oldState)
		return
	}
	c.spawnedTmux = tname
	if prompt != "" {
		fmt.Printf("ok → %s (running in background)\n", tname)
		c.spawnedBackground = true
		go dismissTrustPrompt(tname)
		return
	}
	fmt.Printf("ok → %s\n", tname)
	c.enterRaw()
	runTmuxAttach(c, tname)
}
