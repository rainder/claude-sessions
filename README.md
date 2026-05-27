# claude-sessions

A live, multi-host viewer and manager for running Claude Code CLI sessions.

This is the Go rewrite of the original bash+python script. Single static binary, no runtime deps, copy across machines.

## Install

```sh
go install github.com/rainder/claude-sessions@latest
# or, from a clone:
make build          # cross-compiles all archs into ./bin
make install        # copies the current-host binary to ~/.local/bin
```

`make build` always produces all three target binaries in `./bin/`:

```
bin/claude-sessions-darwin-arm64
bin/claude-sessions-linux-amd64
bin/claude-sessions-linux-arm64
```

For remote deploys (macOS dev box → Linux server / Pi):

```sh
make deploy         # scp's the matching Linux binary to BELUGA_SSH and RPI1_SSH
# or one at a time:
make deploy-beluga
make deploy-rpi1
```

Override the hosts on the command line if needed:

```sh
make deploy BELUGA_SSH=beluga.tail-net.ts.net RPI1_SSH=pi@rpi1.local
```

## Usage

```sh
claude-sessions                            # live TUI (default)
claude-sessions --once                     # one-shot print
claude-sessions -s [--port 8765]           # run HTTP server (Tailscale-bound)

claude-sessions kill PID [-y]              # kill a session (tmux-aware)
claude-sessions migrate PID [-y]           # kill + resume in a new tmux session
claude-sessions new --cwd PATH [--name N]  # spawn a tmux+claude session
claude-sessions attach PID                 # tmux attach (or switch-client)
claude-sessions preview PID                # tmux capture or transcript tail
claude-sessions tmux-info PID              # tmux session name for a pid
```

### Live-view keys

| Key  | Action                                         |
| ---- | ---------------------------------------------- |
| ↑/↓  | navigate                                       |
| n    | new tmux+claude session (cwd picker)           |
| k    | kill (tmux-aware)                              |
| a    | attach (or migrate to tmux first)              |
| p    | preview (tmux pane snapshot or transcript)     |
| m    | toggle view mode (full ↔ minimal, persisted)   |
| r    | refresh now                                    |
| ?    | help modal                                     |
| q    | quit (Ctrl-C / Ctrl-D also work)               |

### Multi-host

Add servers to `~/.config/claude-sessions/servers.yaml`:

```yaml
servers:
  - name: beluga
    host: 100.64.0.1            # Tailscale IPv4 of the server
    port: 8765
    token: <copy from server>
    ssh_host: beluga            # optional, defaults to host
    ssh_user: beluga            # optional, defaults to your local $USER
                                # tmux sessions are per-user — set this if the
                                # server runs as a different user than you log
                                # in as locally, or `ssh attach` shows "no sessions"
  - name: rpi1
    host: 100.64.0.2
    port: 8765
    token: <copy from server>
    ssh_user: pi
```

Start the server on each remote host with `claude-sessions -s`. The bind IP and token are printed; copy them into the client's `servers.yaml`. Token is auto-generated on first start and persisted at `~/.config/claude-sessions/server-token` (mode 0600).

Remote rows appear in their own section under the local one. Selection works across all rows; actions on a remote row use the HTTP API + `ssh -t <ssh_host>` for attach.

## Files

- `~/.claude/sessions/<pid>.json` — session metadata (written by Claude Code)
- `~/.claude/projects/<encoded-cwd>/<sid>.jsonl` — conversation transcripts
- `~/.config/claude-sessions/view-mode` — persisted view mode (1 or 2)
- `~/.config/claude-sessions/server-token` — bearer token (server side, 0600)
- `~/.config/claude-sessions/servers.yaml` — client server list

## Layout

```
main.go              CLI dispatch
session.go           Session struct + CollectLocal
tmux.go              pane mapping + ppid walk
render.go            full/minimal views with multi-section layout
config.go            view-mode load/save
yaml.go              tiny YAML parser for servers.yaml
remote.go            HTTP client + RemoteResult
server.go            HTTP server (Tailscale bind, bearer auth, all endpoints)
tui.go               alt-screen + raw mode + key reader + main loop
actions.go           local action handlers (kill/attach/preview/new)
remote_actions.go    remote action handlers
commands.go          scriptable subcommands (used by server shell-out)
migrate.go           shared migrate/spawn logic
preview.go           tmux capture / JSONL transcript renderer
picker.go            cwd suggestions for `new` (live + history)
helpers.go           terminal mode helpers, prompts
termios_*.go         platform ioctl constants (BSD vs Linux)
```
