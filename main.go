// claude-sessions — list, monitor, and manage running Claude Code CLI sessions
// across machines. See README for the full feature set; this is the Go rewrite
// of the original bash+python script.
package main

import (
	"fmt"
	"os"
	"time"
)

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		args = []string{"list"}
	}
	switch args[0] {
	case "-h", "--help":
		fmt.Fprintln(os.Stderr, usage)
	case "-s", "--server":
		os.Exit(cmdServer(args[1:]))
	case "-1", "--once":
		if err := cmdList(); err != nil {
			fmt.Fprintln(os.Stderr, "claude-sessions:", err)
			os.Exit(1)
		}
	case "list":
		if len(args) > 1 && (args[1] == "--once" || args[1] == "-1") {
			if err := cmdList(); err != nil {
				fmt.Fprintln(os.Stderr, "claude-sessions:", err)
				os.Exit(1)
			}
			return
		}
		if err := RunTUI(2 * time.Second); err != nil {
			fmt.Fprintln(os.Stderr, "claude-sessions:", err)
			os.Exit(1)
		}
	case "list-sessions":
		os.Exit(cmdListSessions(args[1:]))
	case "kill":
		os.Exit(cmdKill(args[1:]))
	case "migrate":
		os.Exit(cmdMigrate(args[1:]))
	case "new":
		os.Exit(cmdNew(args[1:]))
	case "preview":
		os.Exit(cmdPreview(args[1:]))
	case "tmux-info":
		os.Exit(cmdTmuxInfo(args[1:]))
	case "attach":
		os.Exit(cmdAttach(args[1:]))
	case "clip-request":
		os.Exit(cmdClipRequest(args[1:]))
	case "notify-test":
		os.Exit(cmdNotifyTest(args[1:]))
	case "pair":
		os.Exit(cmdPair(args[1:]))
	case "service":
		os.Exit(cmdService(args[1:]))
	case "snapshot":
		os.Exit(cmdSnapshot(args[1:]))
	case "account":
		os.Exit(cmdAccount(args[1:]))
	case "summary":
		os.Exit(cmdSummary(args[1:]))
	default:
		fmt.Fprintln(os.Stderr, "unknown subcommand:", args[0])
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
}

const usage = `usage: claude-sessions [SUBCOMMAND] [args]

subcommands:
  (no args), list                 live auto-refreshing view (TUI)
  list --once, -1                 print local sessions and exit
  list-sessions [--json] [--local]
                                  print local + remote sessions in full-mode
                                  layout (or as JSON) and exit; --local skips
                                  the remote hosts entirely
  -s, --server [--port N] [--bind ADDR]
                                  run HTTP server (default 127.0.0.1:8765;
                                  --bind tailscale auto-detects Tailscale IPv4)
  kill PID [-y] [--remove-worktree]
                                  kill a session (tmux-aware); offers to remove
                                  the git worktree it was the last session in
  migrate PID [-y]                kill + resume in a new tmux session
  new --dir PATH [--name NAME] [--command PRESET | --cmd BINARY [ARG...] --] [--group 1-9] [--server SERVER] [PROMPT...]
                                  spawn a new tmux+claude session, locally or
                                  on a configured server (--cwd is a synonym
                                  for --dir)
  attach PID                      tmux attach (or switch-client) to a session
  preview PID                     print tmux capture or transcript tail
  tmux-info PID                   print tmux session name for a pid
  pair [--port N]                 print a pairing QR for the iOS app
  service install [--port N] [--bind ADDR] | uninstall | restart | status
                                  run the server as a supervised background
                                  service (launchd on macOS, systemd --user on
                                  Linux); install also starts it
  snapshot save [name] | restore NAME | list
                                  save/restore the local session set; name
                                  defaults to a timestamp; restore recreates
                                  each session (best-effort) via tmux + resume
  account switch NAME [--server SERVER] | save NAME | list [--server SERVER]
                                  switch the active Claude Code account from a
                                  claude-switch credential snapshot, capture the
                                  live one as a snapshot, or list what each host
                                  knows about
  summary [claude|codex]          print or set the ticket/conversation
                                  summary backend (default claude)
  notify-test                     send a test push to every registered device
  -h, --help                      this help

live-view keys:
  ↑/↓  navigate     n  new
  k    kill         a  attach (or migrate)
  p    preview      m  cycle view mode
  s    sort menu    r  refresh
  ^W   account      ?  help
  q    quit`

func cmdList() error {
	local, err := CollectLocal()
	if err != nil {
		return err
	}
	remotes := FetchAllRemote()
	// Disabled state and group assignments are host-owned (session_flags.go).
	// Local sessions are this host, so overlay directly; remote sessions
	// already carry authoritative flags from the wire (each remote host's own
	// FlagsStore, applied server-side in GET /sessions). Disabled is what shows
	// here — it orders disabled rows last — but the overlay sets the group too.
	LoadFlagsStore().Overlay(local)
	sortMode := LoadSortMode()
	groupSortOn := LoadGroupSort()
	SortSessions(local, sortMode, groupSortOn)
	remotes = sortRemotes(remotes, sortMode, groupSortOn)
	// Each host's account identity (and, for the local host only, its
	// rate-limit numbers) come from its /usage endpoint, not /sessions; without
	// this even a remote host's account-email heading label would be blank.
	// GET /usage never calls Anthropic on a remote host's behalf (server.go), so
	// a remote's own numbers never appear here — only its identity does.
	remotes = mergeRemoteUsage(remotes)
	RenderAll(os.Stdout, LoadViewMode(), LocalHost{
		Name:      shortHostname(),
		Sessions:  local,
		HostUsage: CollectHostUsage(),
	}, remotes, "", nil, 0, 0, sortMode)
	return nil
}
