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

	"golang.org/x/term"
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
	// Disabled state and group assignments are host-owned (session_flags.go).
	// Local sessions are this host, so overlay directly; remote sessions
	// already carry authoritative flags from the wire (each remote host's own
	// FlagsStore, applied server-side in GET /sessions). Disabled orders
	// disabled rows last in both outputs; the group rides along too, and
	// reaches `list-sessions --json` as each row's "group" key.
	LoadFlagsStore().Overlay(local)
	sortMode := LoadSortMode()
	SortSessions(local, sortMode, false)
	remotes = sortRemotes(remotes, sortMode, false)
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

	// Only the rendered form shows account identity/bars, and they come from
	// each host's /usage endpoint rather than /sessions (a remote host's own
	// numbers never come back — GET /usage answers identity only, see
	// server.go). Fetching after the --json branch has returned keeps a shell
	// pipeline off a round of requests whose result it would never print.
	remotes = mergeRemoteUsage(remotes)
	RenderAll(os.Stdout, "1", LocalHost{
		Name:      shortHostname(),
		Sessions:  local,
		HostUsage: hostUsage,
	}, remotes, "", nil, 0, 0, sortMode)
	return 0
}

// accountUsageMsg is the `account` subcommand family's usage line. `save` takes
// no --server on purpose: it captures the credential that is live on *this*
// machine, which is only meaningful where that credential lives.
const accountUsageMsg = `usage: claude-sessions account switch NAME [--server SERVER]
       claude-sessions account save NAME [--force]
       claude-sessions account list [--server SERVER]
       claude-sessions account remove NAME [-y|--force]`

// cmdAccount dispatches the switch/save/list/remove subcommands of `account`.
func cmdAccount(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, accountUsageMsg)
		return 2
	}
	switch args[0] {
	case "switch":
		return cmdAccountSwitch(args[1:])
	case "save":
		return cmdAccountSave(args[1:])
	case "list":
		return cmdAccountList(args[1:])
	case "remove":
		return cmdAccountRemove(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "account: unknown subcommand %q\n%s\n", args[0], accountUsageMsg)
		return 2
	}
}

// accountArgs is what the `account` subcommands parse out of their arguments.
//
// force and assumeYes are tracked separately rather than collapsed into one
// bool, because the two subcommands that take an override mean different things
// by it. `remove` asks a yes/no question, so `-y` (the repo-wide "don't ask me"
// spelling, as in `kill -y`) answers it and `--force` is accepted as a synonym.
// `save` asks nothing — it refuses, and `--force` means "reassign this snapshot
// to another account", a deliberate act with no prompt to skip. Letting `-y`
// stand for that would make a reflexive flag reassign a credential.
type accountArgs struct {
	name      string
	server    string
	force     bool // --force
	assumeYes bool // -y
}

// parseAccountArgs reads an optional positional NAME plus an optional --server
// value and the two override flags. An unknown flag or a second positional is an
// error rather than a silent no-op, matching parseKillFlags' rule that a typo
// never reads as "do it anyway"; each subcommand then rejects the flags it has
// no mode for, which is what keeps `account list --force` from being silently
// ignored.
func parseAccountArgs(args []string) (accountArgs, error) {
	var out accountArgs
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--server":
			if i+1 >= len(args) {
				return accountArgs{}, fmt.Errorf("--server needs a value")
			}
			out.server = args[i+1]
			i++
		case a == "--force":
			out.force = true
		case a == "-y":
			out.assumeYes = true
		case strings.HasPrefix(a, "--"):
			return accountArgs{}, fmt.Errorf("unknown flag: %s", a)
		case out.name == "":
			out.name = a
		default:
			return accountArgs{}, fmt.Errorf("unexpected argument: %s", a)
		}
	}
	return out, nil
}

// accountSwitchedLine is the confirmation printed after a switch. switchAccount
// only ever succeeds with a real, non-empty email — it refuses upstream
// (before touching anything) when the target has no usable identity snapshot
// to patch — so there is no "switched but email unknown" case to format here.
func accountSwitchedLine(name, email string) string {
	return fmt.Sprintf("switched to %s (%s)", name, email)
}

func cmdAccountSwitch(args []string) int {
	a, err := parseAccountArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "account switch: %v\n%s\n", err, accountUsageMsg)
		return 2
	}
	if a.name == "" {
		fmt.Fprintln(os.Stderr, accountUsageMsg)
		return 2
	}
	if a.force || a.assumeYes {
		fmt.Fprintf(os.Stderr, "account switch: --force/-y are not supported (a switch never prompts)\n")
		return 2
	}
	name := a.name
	if a.server != "" {
		return cmdAccountSwitchRemote(name, a.server)
	}
	email, warnings, err := switchAccount(name)
	if err != nil {
		fmt.Fprintln(os.Stderr, "account switch:", err)
		return 1
	}
	printAccountWarnings(warnings)
	fmt.Println(accountSwitchedLine(name, email))
	return 0
}

// printAccountWarnings writes a switch's advisory warnings to stderr, so the
// success line on stdout stays the one thing a shell pipeline reads. They are
// printed BEFORE that line: the warning is about what may happen next, and a
// reader who stops at "switched to …" has not missed anything they needed.
func printAccountWarnings(warnings []string) {
	for _, w := range warnings {
		fmt.Fprintln(os.Stderr, "account switch: warning:", w)
	}
}

// cmdAccountSwitchRemote switches a configured remote's active account over
// HTTP. The server's own refusal message is preferred over the bare transport
// error, so an unknown name still prints the list of names that host holds.
func cmdAccountSwitchRemote(name, server string) int {
	if _, ok := LookupServer(server); !ok {
		cfgs, _ := LoadServerConfigs()
		names := make([]string, len(cfgs))
		for i, c := range cfgs {
			names[i] = c.Name
		}
		fmt.Fprintf(os.Stderr, "account switch: unknown server %q (configured: %s)\n", server, strings.Join(names, ", "))
		return 2
	}
	result, err := switchAccountRemote(server, name)
	if err != nil {
		fmt.Fprintln(os.Stderr, "account switch:", err)
		return 1
	}
	if !result.OK {
		fmt.Fprintf(os.Stderr, "account switch: %s: %s\n", server, result.Message)
		return 1
	}
	// The remote host's own warnings, about processes running there — same
	// hazard, reported by the machine that can see it.
	printAccountWarnings(result.Warnings)
	fmt.Println(accountSwitchedLine(name, result.Account))
	return 0
}

func cmdAccountSave(args []string) int {
	a, err := parseAccountArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "account save: %v\n%s\n", err, accountUsageMsg)
		return 2
	}
	name := a.name
	if name == "" {
		fmt.Fprintln(os.Stderr, accountUsageMsg)
		return 2
	}
	if a.server != "" {
		fmt.Fprintf(os.Stderr, "account save: --server is not supported (a snapshot captures the credential live on this machine)\n")
		return 2
	}
	if a.assumeYes {
		// Save never prompts, so there is no "yes" to assume. Its override
		// reassigns a snapshot to a different account, which is a deliberate act
		// and has to be spelled out.
		fmt.Fprintf(os.Stderr, "account save: -y is not supported; use --force to reassign a snapshot to another account\n")
		return 2
	}
	if err := saveAccountSnapshot(name, a.force); err != nil {
		fmt.Fprintln(os.Stderr, "account save:", err)
		return 1
	}
	email := snapshotAccountEmail(name)
	if email == "" {
		fmt.Printf("saved snapshot %s\n", name)
		return 0
	}
	fmt.Printf("saved snapshot %s (%s)\n", name, email)
	return 0
}

func cmdAccountList(args []string) int {
	a, err := parseAccountArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "account list: %v\n%s\n", err, accountUsageMsg)
		return 2
	}
	if a.name != "" {
		fmt.Fprintf(os.Stderr, "account list: unexpected argument: %s\n%s\n", a.name, accountUsageMsg)
		return 2
	}
	if a.force || a.assumeYes {
		fmt.Fprintf(os.Stderr, "account list: --force/-y are not supported (listing changes nothing)\n")
		return 2
	}
	if server := a.server; server != "" {
		srv, ok := LookupServer(server)
		if !ok {
			cfgs, _ := LoadServerConfigs()
			names := make([]string, len(cfgs))
			for i, c := range cfgs {
				names[i] = c.Name
			}
			fmt.Fprintf(os.Stderr, "account list: unknown server %q (configured: %s)\n", server, strings.Join(names, ", "))
			return 2
		}
		// /usage alone, no /sessions poll: the table is built entirely from the
		// three account fields (see accountRowsFrom), and the session list behind
		// them would be collected and thrown away.
		fmt.Print(renderAccountTable([]accountListing{remoteAccountListing(oneRemoteUsage(srv))}))
		return 0
	}
	listings := []accountListing{localAccountListing()}
	for _, r := range FetchAllRemoteUsage() {
		listings = append(listings, remoteAccountListing(r))
	}
	fmt.Print(renderAccountTable(listings))
	return 0
}

// cmdAccountRemove deletes one parked account snapshot from this machine.
//
// Local-only, like `save`, and for the same reason: it acts on files that exist
// only on the machine holding them. There is deliberately no HTTP endpoint and
// no TUI binding — removing a switch target is rare, irreversible without a
// relogin, and has no business being one keystroke away from the picker.
func cmdAccountRemove(args []string) int {
	a, err := parseAccountArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "account remove: %v\n%s\n", err, accountUsageMsg)
		return 2
	}
	name := a.name
	// Remove asks a yes/no question, so -y answers it and --force is accepted
	// as a synonym; see accountArgs for why save does not take both.
	force := a.force || a.assumeYes
	if name == "" {
		fmt.Fprintln(os.Stderr, accountUsageMsg)
		return 2
	}
	if a.server != "" {
		fmt.Fprintf(os.Stderr, "account remove: --server is not supported (a snapshot only exists on the machine holding it)\n")
		return 2
	}
	plan, err := planAccountRemoval(name)
	if err != nil {
		fmt.Fprintln(os.Stderr, "account remove:", err)
		return 1
	}
	if plan.Live && !force {
		// Removing the live account's snapshot is allowed — it deletes a parked
		// copy, not the login — but it is the one removal that quietly costs
		// something later, so it is never done unasked. A pipeline has to say -y;
		// a human is asked.
		fmt.Println("this snapshot stands for the account logged in right now.")
		fmt.Println("removing it does NOT log you out — it deletes the parked copy, so nothing")
		fmt.Println("will be able to switch back to this account until 'account save' recaptures it.")
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			fmt.Fprintf(os.Stderr, "account remove: refusing without -y (not a terminal, so nothing can confirm)\n")
			return 1
		}
		if !confirm(fmt.Sprintf("remove snapshot %q anyway? [y/N] ", name)) {
			fmt.Println("aborted")
			return 0
		}
	}
	removed, wasLive, err := removeAccountSnapshot(name)
	if err != nil {
		fmt.Fprintln(os.Stderr, "account remove:", err)
		return 1
	}
	fmt.Printf("removed snapshot %s (%s)\n", name, strings.Join(removed, ", "))
	// Re-derived under the lock, so this describes the removal that happened
	// rather than what the plan predicted before the prompt. A -y run reaches
	// here without ever having seen the warning above, and a switch during the
	// prompt can make a removal live that was not when it was planned.
	if wasLive {
		fmt.Printf("note: %s was the account logged in here; nothing can switch back to it until 'account save %s' recaptures it\n", name, name)
	}
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
		path, count, err := SaveSnapshot(name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "snapshot save: %v\n", err)
			return 1
		}
		fmt.Printf("saved snapshot %q to %s (%d session(s))\n", name, path, count)
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

// cmdSummary manages the backend for the ticket/conversation summary
// feature (info_ticket.go, info_transcript.go): `claude-sessions summary`
// prints the current backend, `claude-sessions summary claude|codex` sets it.
func cmdSummary(args []string) int {
	const usage = "usage: claude-sessions summary [claude|codex]"
	if len(args) == 0 {
		fmt.Println(LoadSummaryBackend())
		return 0
	}
	if len(args) > 1 {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	switch args[0] {
	case "claude", "codex":
		SaveSummaryBackend(args[0])
		fmt.Printf("summary backend set to %s\n", args[0])
		return 0
	default:
		fmt.Fprintf(os.Stderr, "summary: unknown backend %q\n%s\n", args[0], usage)
		return 2
	}
}
