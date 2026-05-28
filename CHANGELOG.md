# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/);
this project follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [v1.1.0] - 2026-05-28

### Added

- `enable: false` field in `servers.yaml` — hides an entry from the TUI,
  remote polling, and lookups without removing it from the config. Defaults
  to `true`, so existing configs are unaffected.
- One-liner installer: `curl -fsSL https://raw.githubusercontent.com/rainder/claude-sessions/main/install.sh | bash`.
  Auto-detects OS/arch, downloads the release binary, verifies SHA256.
  Honors `VERSION=` and `INSTALL_DIR=` env vars.
- `(loading...)` placeholder for a remote section before its first fetch
  completes, so users can see which servers are still pending.

### Changed

- Remote fetches now stream asynchronously. The render loop reads from a
  background `RemoteHub` snapshot instead of calling `FetchAllRemote()`
  synchronously every tick, so a slow/unreachable host can no longer freeze
  keystrokes or auto-refresh. Each server's row populates as soon as that
  host replies; prior data is preserved across cycles so a flaky host's row
  doesn't blink to blank.
- Per-host HTTP timeout bumped from 2s to 5s. Made tolerable by the async
  fetcher above.
- `CollectLocal` now issues a single `ps -A -o pid=,ppid=,%cpu=` call
  instead of one `ps -p` per session per tick. Drops N+1 process spawns to
  1 per refresh, regardless of session count.

## [v1.0.0] - 2026-05-27

Initial release. Single static binary; cross-compiled for darwin/arm64,
linux/amd64, linux/arm64.

### Live TUI client

- Auto-refreshing table (2s wall-clock tick via `unix.Select`; works even under
  continuous input)
- Arrow-key navigation with selection persistence across refreshes
- Full vs. minimal view modes (`m` to toggle, persisted at
  `~/.config/claude-sessions/view-mode`)
- Help modal (`?`), redraw (`r`), quit (`q` / Ctrl-C / Ctrl-D)
- Status glyphs in minimal view: ● busy (red), $ shell (cyan), ! waiting
  (yellow), · idle (dim)
- Path squashing in full view:
  `~/Developer/trecs-brain/src/dir` → `~/D/tb/s/dir`

### Actions on the selected session

- `k` — kill (tmux-aware: kills the tmux session when the pid is in a pane)
- `a` — attach (or offer to migrate to tmux first)
- `p` — preview: pixel-perfect tmux pane capture, or filtered JSONL transcript
  tail for non-tmux sessions
- `n` — spawn new tmux+claude session with a cwd picker built from live
  sessions plus project history

### Server mode (`-s`)

- HTTP+JSON server with bearer-token auth (token auto-generated on first
  start, persisted at `~/.config/claude-sessions/server-token` mode 0600)
- Configurable bind interface — default `127.0.0.1`, with `--bind tailscale`
  to auto-detect the host's Tailscale IPv4 and `--bind <addr>` for any
  explicit address
- Endpoints: `GET /sessions`, `GET /sessions/<pid>/preview`,
  `GET /sessions/<pid>/tmux-info`, `POST /sessions/<pid>/kill`,
  `POST /sessions/<pid>/migrate`, `POST /sessions/new`

### Multi-host client

- YAML server list at `~/.config/claude-sessions/servers.yaml`
- Parallel polling (2s per-host timeout) with goroutines
- Per-host sections in the table; unreachable hosts shown inline
- Remote actions (kill / attach via ssh / preview / migrate / new) dispatch
  transparently when a remote row is selected
- `ssh_user` / `ssh_host` config fields for per-host SSH overrides

### Scriptable subcommands

`list`, `kill`, `migrate`, `new`, `attach`, `preview`, `tmux-info` — all
non-interactive, usable from shell pipelines.

### Build / install / deploy

- `make build` cross-compiles all three target binaries into `./bin/`
- `make install` copies the host-arch binary to `~/.local/bin/claude-sessions`
- `make deploy-linux-{amd64,arm64} HOST=...` for remote deploys via ssh/scp
- Personal shortcuts go in `Makefile.local` (gitignored, auto-included)

### Notable implementation details (for future contributors)

- Single stdin consumer: cooked-mode prompts (`bufio.Scanner`) and raw-mode
  polling never race
- `term.MakeRaw` zeros OPOST; we restore it after every transition so `\n`
  still translates to `\r\n` in alt-screen
- Tmux pane detection checks the pid itself before walking parents (covers
  `tmux new-session "claude ..."` where claude *is* the pane process)
- `unix.Select` instead of `os.Stdin.SetReadDeadline` because stdin
  inherited at process start isn't registered with Go's netpoller
