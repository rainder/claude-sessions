package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Scriptable subcommands. Used by the HTTP server (shell-out) and available
// from the shell for ad-hoc automation. All non-interactive.

// killFlags are the order-independent flags cmdKill accepts after the PID.
type killFlags struct {
	assumeYes      bool // -y: skip the kill confirmation
	removeWorktree bool // --remove-worktree: also remove a worktree left empty
}

// parseKillFlags reads the flags following the PID. An unrecognized flag is an
// error rather than a silent no-op, so a typo never reads as "kill anyway".
func parseKillFlags(args []string) (killFlags, error) {
	var f killFlags
	for _, a := range args {
		switch a {
		case "-y":
			f.assumeYes = true
		case "--remove-worktree":
			f.removeWorktree = true
		default:
			return f, fmt.Errorf("unknown flag: %s", a)
		}
	}
	return f, nil
}

func cmdKill(args []string) int {
	const usage = "usage: claude-sessions kill PID [-y] [--remove-worktree]"
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	pid, err := strconv.Atoi(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "kill: not a pid: %s\n", args[0])
		return 2
	}
	flags, err := parseKillFlags(args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "kill: %v\n%s\n", err, usage)
		return 2
	}
	assumeYes := flags.assumeYes
	sess, ok := readSessionByPID(pid)
	if !ok {
		fmt.Fprintf(os.Stderr, "PID %d is not a live Claude session\n", pid)
		return 1
	}
	// Resolve the live tmux location once and let it drive the kill, so the
	// confirmation, execution, and result text all agree on the same target.
	sess.Tmux = tmuxLocForPID(pid)
	tmuxName := ""
	if sess.Tmux != "" {
		if n, err := tmuxSessionName(sess.Tmux); err == nil {
			tmuxName = n
		}
	}
	if !assumeYes {
		prompt := fmt.Sprintf("kill PID %d? [y/N] ", pid)
		if tmuxName != "" {
			prompt = fmt.Sprintf("kill tmux session %q (PID %d)? [y/N] ", tmuxName, pid)
		}
		if !confirm(prompt) {
			fmt.Println("aborted")
			return 0
		}
	}
	// Decide the worktree question before the kill, while the session is still
	// in the list. Only worktree-resident sessions pay for the collect.
	worktree, err := localWorktreeCleanupTarget(sess)
	if err != nil {
		fmt.Fprintf(os.Stderr, "worktree check skipped: %v\n", err)
	}
	if err := localReattest(pid, sess.SessionID); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := KillSession(sess); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if tmuxName != "" {
		fmt.Printf("killed tmux session %s (PID %d)\n", tmuxName, pid)
	} else {
		fmt.Printf("killed PID %d\n", pid)
	}
	if worktree != "" {
		cleanupWorktreeCLI(worktree, flags)
	}
	return 0
}

// cleanupWorktreeCLI handles the "that was the last session in this worktree"
// case for the kill subcommand. --remove-worktree removes it outright;
// otherwise an interactive run asks. A -y run without --remove-worktree keeps
// the worktree: a non-interactive kill never removes what wasn't asked for.
// Never fatal — the kill already succeeded, so this only reports.
func cleanupWorktreeCLI(worktree string, flags killFlags) {
	if !flags.removeWorktree {
		if flags.assumeYes {
			fmt.Printf("worktree %s is now idle (pass --remove-worktree to remove it)\n", worktree)
			return
		}
		if !confirm(fmt.Sprintf("last session in worktree %q — remove %s? [y/N] ", filepath.Base(worktree), worktree)) {
			return
		}
	}
	if err := RemoveWorktree(worktree); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	fmt.Printf("removed worktree %s\n", worktree)
}

func cmdMigrate(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: claude-sessions migrate PID [-y]")
		return 2
	}
	pid, err := strconv.Atoi(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "migrate: not a pid: %s\n", args[0])
		return 2
	}
	assumeYes := len(args) > 1 && args[1] == "-y"
	sess, ok := readSessionByPID(pid)
	if !ok {
		fmt.Fprintf(os.Stderr, "PID %d is not a live Claude session\n", pid)
		return 1
	}
	tname := MakeTmuxName(sess.CWD, sess.SessionID, sess.Name)
	if !assumeYes {
		if !confirm(fmt.Sprintf("migrate PID %d to tmux %q? [y/N] ", pid, tname)) {
			fmt.Println("aborted")
			return 0
		}
	}
	out, err := MigrateLocalAttested(pid, sess.SessionID)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println(out)
	return 0
}

// newArgs is cmdNew's parsed flags plus the joined trailing prompt.
type newArgs struct {
	dir, name, command, server, prompt string
}

// parseNewArgs parses `new`'s flags. --dir and --cwd are synonyms (--dir is
// preferred, --cwd kept for backward compatibility). Any non-flag args are
// joined with spaces to form the optional initial prompt, so callers can
// write it unquoted: `new --dir X some initial prompt`.
func parseNewArgs(args []string) (newArgs, error) {
	var a newArgs
	var promptParts []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--dir", "--cwd":
			if i+1 >= len(args) {
				return newArgs{}, fmt.Errorf("%s needs a value", args[i])
			}
			a.dir = args[i+1]
			i++
		case "--name":
			if i+1 >= len(args) {
				return newArgs{}, fmt.Errorf("--name needs a value")
			}
			a.name = args[i+1]
			i++
		case "--command":
			if i+1 >= len(args) {
				return newArgs{}, fmt.Errorf("--command needs a value")
			}
			a.command = args[i+1]
			i++
		case "--server":
			if i+1 >= len(args) {
				return newArgs{}, fmt.Errorf("--server needs a value")
			}
			a.server = args[i+1]
			i++
		default:
			if strings.HasPrefix(args[i], "--") {
				return newArgs{}, fmt.Errorf("unknown arg %q", args[i])
			}
			promptParts = append(promptParts, args[i])
		}
	}
	a.prompt = strings.Join(promptParts, " ")
	return a, nil
}

const newUsage = "usage: claude-sessions new --dir PATH [--name NAME] [--command PRESET] [--server SERVER] [PROMPT...]"

func cmdNew(args []string) int {
	a, err := parseNewArgs(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "new:", err)
		return 2
	}
	if a.dir == "" {
		fmt.Fprintln(os.Stderr, newUsage)
		return 2
	}
	if a.server != "" {
		return cmdNewRemote(a)
	}
	return cmdNewLocal(a)
}

// cmdNewLocal spawns a new tmux+claude session on this host.
func cmdNewLocal(a newArgs) int {
	dir := expandTilde(a.dir)
	if !isDir(dir) {
		fmt.Fprintf(os.Stderr, "not a directory: %s\n", dir)
		return 1
	}
	presets, err := LoadCommandPresets()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	preset := presets[0]
	if a.command != "" {
		var ok bool
		preset, ok = findCommandPreset(presets, a.command)
		if !ok {
			names := make([]string, len(presets))
			for i, p := range presets {
				names[i] = p.Name
			}
			fmt.Fprintf(os.Stderr, "new: command preset not found: %s (available: %s)\n", a.command, strings.Join(names, ", "))
			return 2
		}
	}
	command := preset.Command
	if a.prompt != "" {
		command = command + " " + shellQuote(a.prompt)
	}
	tname, err := SpawnNew(dir, a.name, command)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if a.prompt != "" {
		// Run synchronously, not backgrounded: unlike the TUI (a long-running
		// process where a goroutine can outlive the triggering keypress), this
		// CLI process exits the moment cmdNew returns, which would kill a
		// goroutine before it ever polled. dismissTrustPrompt bounds itself to
		// trustPromptTimeout, so this adds at most a few seconds.
		dismissTrustPrompt(tname)
	}
	fmt.Println(tname)
	return 0
}

// cmdNewRemote spawns a new tmux+claude session on a configured remote server.
func cmdNewRemote(a newArgs) int {
	if _, ok := LookupServer(a.server); !ok {
		cfgs, _ := LoadServerConfigs()
		names := make([]string, len(cfgs))
		for i, c := range cfgs {
			names[i] = c.Name
		}
		fmt.Fprintf(os.Stderr, "new: unknown server %q (configured: %s)\n", a.server, strings.Join(names, ", "))
		return 2
	}
	if a.command != "" {
		// Validate against the remote's own preset names before spawning, so a
		// typo fails fast locally with the list of what that host actually
		// offers. An old server without the /presets route can't be asked
		// ahead of time; fall through and let /sessions/new validate as before.
		if presets, err := fetchRemotePresets(a.server); err == nil {
			found := false
			names := make([]string, len(presets))
			for i, p := range presets {
				names[i] = p.Name
				if p.Name == a.command {
					found = true
				}
			}
			if !found {
				fmt.Fprintf(os.Stderr, "new: command preset not found on %s: %s (available: %s)\n", a.server, a.command, strings.Join(names, ", "))
				return 2
			}
		}
	}
	// No local ~ expansion or directory check: dir lives on the remote host,
	// whose home and filesystem differ from ours. The server resolves and
	// validates it.
	body, _ := json.Marshal(map[string]string{
		"cwd":        a.dir,
		"name":       a.name,
		"command":    a.command,
		"prompt":     a.prompt,
		"request_id": newSpawnRequestID(),
	})
	resp, err := remoteRequest(a.server, "/sessions/new", "POST", body)
	if err != nil {
		fmt.Fprintln(os.Stderr, "new:", err)
		return 1
	}
	var r actionResult
	if err := json.Unmarshal(resp, &r); err != nil {
		fmt.Fprintln(os.Stderr, "new: bad response from server:", err)
		return 1
	}
	if !r.OK || r.Tmux == "" {
		fmt.Fprintln(os.Stderr, "new:", r.Error)
		return 1
	}
	fmt.Println(r.Tmux)
	return 0
}

func cmdPreview(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: claude-sessions preview PID")
		return 2
	}
	pid, err := strconv.Atoi(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "preview: not a pid: %s\n", args[0])
		return 2
	}
	fmt.Print(PreviewContent(pid))
	return 0
}

func cmdTmuxInfo(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: claude-sessions tmux-info PID")
		return 2
	}
	pid, err := strconv.Atoi(args[0])
	if err != nil {
		return 2
	}
	loc := tmuxSessionForPID(pid)
	if loc != "" {
		fmt.Println(loc)
	}
	return 0
}

// cmdAttach mirrors the bash behavior: attach to tmux if pid is in a pane,
// else offer migration. Non-interactive callers (e.g. the server) should use
// migrate + tmux-info directly.
func cmdAttach(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: claude-sessions attach PID")
		return 2
	}
	pid, err := strconv.Atoi(args[0])
	if err != nil {
		return 2
	}
	if _, ok := readSessionByPID(pid); !ok {
		fmt.Fprintf(os.Stderr, "PID %d is not a live Claude session\n", pid)
		return 1
	}
	sessName := tmuxSessionForPID(pid)
	if sessName == "" {
		fmt.Fprintf(os.Stderr, "PID %d is not in tmux\n", pid)
		fmt.Fprintln(os.Stderr, "run: claude-sessions migrate", pid)
		return 1
	}
	subcommand := "attach"
	if os.Getenv("TMUX") != "" {
		subcommand = "switch-client"
	}
	cmd := exec.Command("tmux", subcommand, "-t", sessName)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "tmux %s: %v\n", subcommand, err)
		return 1
	}
	return 0
}

// trimColon returns the part before the first colon. Useful when tmux-info
// includes the window/pane suffix (session:1.0) but we only want "session".
func trimColon(s string) string { return strings.SplitN(s, ":", 2)[0] }

// cmdClipRequest is invoked by the tmux Ctrl+V binding on a server host:
// `claude-sessions clip-request <pane_id> [port]`. It asks this host's own
// server to handle a remote-image paste for that pane. On any failure it falls
// back to passing the raw Ctrl+V through itself, so Ctrl+V is never a dead key.
// Always exits 0 — tmux run-shell surfaces non-zero exits and stderr obnoxiously.
func cmdClipRequest(args []string) int {
	if len(args) == 0 || !validPaneID(args[0]) {
		return 0
	}
	paneID := args[0]
	port := defaultServerPort
	if len(args) > 1 {
		if p, err := strconv.Atoi(args[1]); err == nil && p > 0 {
			port = p
		}
	}
	if !clipRequestRelay(paneID, port) {
		_ = tmuxSendPassthrough(paneID)
	}
	return 0
}

// clipRequestRelay POSTs /paste-request to the local server on port. It returns
// true when the server handled the keystroke (queued it for a waiter, or passed
// it through itself), and false on any transport/auth error so the caller can do
// its own passthrough. The token is read from the shared token file the server
// itself uses.
func clipRequestRelay(paneID string, port int) bool {
	tok, err := readServerToken()
	if err != nil {
		return false
	}
	u := fmt.Sprintf("http://127.0.0.1:%d/paste-request?pane_id=%s", port, url.QueryEscape(paneID))
	req, err := http.NewRequest(http.MethodPost, u, nil)
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// cmdListSessions prints the equivalent of the TUI's full-mode table for
// local + configured remote sessions and exits. --json emits the same data
// as JSON instead, one object per host (local first, then remotes in the
// same sorted order), shaped like the server's GET /sessions response.
func cmdListSessions(args []string) int {
	const usageMsg = "usage: claude-sessions list-sessions [--json]"
	jsonOut := false
	for _, a := range args {
		switch a {
		case "--json":
			jsonOut = true
		default:
			fmt.Fprintf(os.Stderr, "list-sessions: unknown flag: %s\n%s\n", a, usageMsg)
			return 2
		}
	}

	local, err := CollectLocal()
	if err != nil {
		fmt.Fprintln(os.Stderr, "claude-sessions:", err)
		return 1
	}
	remotes := FetchAllRemote()
	// Disabled state is host-owned (disabled_store.go). Local sessions are
	// this host, so overlay directly; remote sessions already carry
	// authoritative Disabled from the wire (each remote host's own
	// DisabledStore, applied server-side in GET /sessions). Groups don't
	// affect this output.
	LoadDisabledStore().Overlay(local)
	sortMode := LoadSortMode()
	SortSessions(local, sortMode)
	remotes = sortRemotes(remotes, sortMode)
	hostUsage := CollectHostUsage()

	if jsonOut {
		hosts := make([]map[string]any, 0, 1+len(remotes))
		hosts = append(hosts, map[string]any{
			"hostname":  shortHostname(),
			"hostUsage": hostUsage,
			"sessions":  local,
		})
		for _, r := range remotes {
			host := map[string]any{
				"hostname":  r.Name,
				"hostUsage": r.HostUsage,
				"sessions":  r.Sessions,
			}
			if r.Error != "" {
				host["error"] = r.Error
			}
			hosts = append(hosts, host)
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(hosts); err != nil {
			fmt.Fprintln(os.Stderr, "claude-sessions:", err)
			return 1
		}
		return 0
	}

	RenderAll(os.Stdout, "1", LocalHost{
		Name:      shortHostname(),
		Sessions:  local,
		HostUsage: hostUsage,
	}, remotes, "", nil, 0, 0, sortMode)
	return 0
}

// cmdSnapshot dispatches the save/restore/list subcommands of `snapshot`.
func cmdSnapshot(args []string) int {
	const usage = "usage: claude-sessions snapshot save [name] | restore NAME | list"
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	switch args[0] {
	case "save":
		name := time.Now().Format("2006-01-02-150405")
		if len(args) > 1 {
			name = args[1]
		}
		path, err := SaveSnapshot(name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "snapshot save: %v\n", err)
			return 1
		}
		fmt.Printf("saved snapshot %q to %s\n", name, path)
		return 0
	case "restore":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: claude-sessions snapshot restore NAME")
			return 2
		}
		report, err := RestoreSnapshot(args[1])
		if err != nil {
			fmt.Fprintf(os.Stderr, "snapshot restore: %v\n", err)
			return 1
		}
		restored := 0
		for _, r := range report.Results {
			mark := "✗"
			status := r.Reason
			if r.Restored {
				mark = "✓"
				status = r.Cwd
				restored++
			}
			fmt.Printf("  %s %s %s\n", mark, r.SessionID, status)
		}
		fmt.Printf("restored %d/%d\n", restored, len(report.Results))
		if restored < len(report.Results) {
			return 1
		}
		return 0
	case "list":
		snaps, err := ListSnapshots()
		if err != nil {
			fmt.Fprintf(os.Stderr, "snapshot list: %v\n", err)
			return 1
		}
		if len(snaps) == 0 {
			fmt.Println("no snapshots saved")
			return 0
		}
		for _, s := range snaps {
			fmt.Printf("%-30s %s  %d session(s)\n", s.Name, s.TakenAt.Format(time.RFC3339), len(s.Entries))
		}
		return 0
	default:
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
}
