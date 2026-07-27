# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/);
this project follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- `claude-sessions service install|uninstall|status` runs the server as a
  supervised background service — a launchd LaunchAgent on macOS, a systemd
  `--user` unit on Linux. `install` writes the unit and loads it, so the server
  is running when the command returns; re-run it to change `--port`/`--bind` or
  after upgrading the binary, since neither supervisor restarts a service whose
  binary was replaced underneath it. On Linux it also enables linger so the
  service survives logout. `status` is scriptable: exit 0 running, 1 loaded but
  not running (or unanswerable — no console session on a Mac, no user D-Bus
  session on Linux), 3 not loaded. The unit carries an explicit `PATH`, because
  supervisors start services with a near-empty one and this binary shells out to
  `tmux`, `tailscale`, and `claude` by bare name — without it tmux detection
  silently finds nothing and `--bind tailscale` crash-loops under `KeepAlive`.
  Neither unit keeps stdout, since the server prints its bearer token there at
  startup; stderr, which carries the "listening on" and notification lines, is
  what gets logged.

### Changed

- `-s` prints the bearer token only when stdout is a terminal. Redirected or
  piped output gets the path to the token file instead, so `claude-sessions -s
  > server.log` no longer copies a secret that lives `0600` on disk into
  whatever mode the shell creates. The rest of the banner is unchanged.
- Push notifications to a paired iPhone when a session becomes blocked on you.
  The server polls a new cheap collection path (identity and status only, no
  transcript scanning) and runs an explicit wait-generation state machine, so an
  alert fires only after a session has been waiting for two consecutive polls —
  a prompt answered at the keyboard within a couple of seconds never reaches the
  phone. Each host signs its own ES256 provider tokens and talks to Apple
  directly; there is no relay and no third-party service. Configure via
  `~/.config/claude-sessions/apns.yaml`; without it the server behaves exactly as
  before and logs one line saying notifications are off.
- `claude-sessions pair` prints a terminal QR for the iOS client. It encodes the
  host, port, and a five-minute single-use code — never the bearer token, which
  the app fetches once by exchanging the code. The code is cleared when `pair`
  exits, so a photographed QR stops working almost immediately.
- `claude-sessions notify-test` sends a test push to every registered device and
  prints Apple's reason string per failure, so a silently broken watchdog is
  visible rather than indistinguishable from a quiet week.
- New authed routes `POST /devices`, `DELETE /devices/{token}`, `POST /pair/arm`,
  and `POST /pair/disarm`, plus `POST /pair/exchange` — the only unauthenticated
  route, and inert unless `pair` is running with a live code.
- Killing the last session running in a git worktree now offers to remove that
  worktree. Available from the TUI (local and remote rows), and from
  `claude-sessions kill` via a second prompt or `--remove-worktree`. Removal
  runs plain `git worktree remove` from the main checkout — no `--force`, so a
  dirty or untracked-file worktree is kept and git's refusal is shown. The
  branch is never touched. Remote kills carry the decision in the kill response
  and remove through the new authed `POST /worktree/remove`, which validates the
  path and refuses a worktree that has since gained a live session.
- Remote image paste: pressing Ctrl+V while attached to a remote session pastes
  the image from the local clipboard into the remote Claude prompt as a file
  path. A tmux binding on the server host relays the request through the server
  (`/paste-request`, `/paste-wait`, `/paste`); the attached client reads its own
  clipboard (`pngpaste`/`osascript` on macOS, `wl-paste`/`xclip` on Linux) and
  pushes the PNG. With no client attached or an empty clipboard, the keystroke
  passes through as a native Ctrl+V. The binding is installed only on Linux
  servers (macOS keeps native local paste).
- Local and remote host headings now show the raw 1-, 5-, and 15-minute load
  averages (`LOAD 1.24 0.96 0.72`) alongside the aggregate CPU/MEM figures.
  Unavailable load renders atomically as dashes (`LOAD -- -- --`).
- Account rate-limit usage bars (5-hour + weekly, like `/usage`) in the TUI
  header, refreshed every 2 minutes in the background. Hidden when
  credentials or the endpoint are unavailable.

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
