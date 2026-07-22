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
  -s, --server [--port N] [--bind ADDR]
                                  run HTTP server (default 127.0.0.1:8765;
                                  --bind tailscale auto-detects Tailscale IPv4)
  kill PID [-y]                   kill a session (tmux-aware)
  migrate PID [-y]                kill + resume in a new tmux session
  new --cwd PATH [--name NAME]    spawn a new tmux+claude session
  attach PID                      tmux attach (or switch-client) to a session
  preview PID                     print tmux capture or transcript tail
  tmux-info PID                   print tmux session name for a pid
  -h, --help                      this help

live-view keys:
  ↑/↓  navigate     n  new
  k    kill         a  attach (or migrate)
  p    preview      m  cycle view mode
  s    cycle sort   r  refresh
  ?    help         q  quit`

func cmdList() error {
	local, err := CollectLocal()
	if err != nil {
		return err
	}
	remotes := FetchAllRemote()
	sortMode := LoadSortMode()
	SortSessions(local, sortMode)
	remotes = sortRemotes(remotes, sortMode)
	RenderAll(os.Stdout, LoadViewMode(), LocalHost{
		Name:     shortHostname(),
		Sessions: local,
	}, remotes, "", nil, 0, 0, sortMode)
	return nil
}
