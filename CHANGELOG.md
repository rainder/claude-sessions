# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/);
this project follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Inspector: press `i` to compose a line of text and send it as literal
  keystrokes (`tmux send-keys -l`) into the selected session's tmux pane,
  local or remote. Disabled with a "no tmux pane" hint when the session has
  none; a failed send keeps the buffer so it can be corrected and resent.
- `POST /sessions/{pid}/kill` and `POST /sessions/{pid}/migrate` accept an
  optional `{"session_id": "…"}` precondition. A PID on its own proves nothing
  once a tmux pane has been recycled and handed that number to a different
  process, and a phone acting on a list it polled minutes ago is far more
  exposed to that than a desktop sitting on the same machine. When the id is
  supplied it is checked against the session the server itself resolves for
  that PID, then checked again from that PID's own session file immediately
  before the signal — the first list costs real I/O to build, so a pane
  recycled while it was being built would otherwise slip through. Either check
  failing refuses: nothing is terminated and nothing is spawned. Omitting the
  id, or sending an empty body (which is what `curl -X POST` with no `-d`
  does), keeps the previous behaviour, so the desktop and every scripted
  caller keep working.

  Two deliberate differences from the old responses, both on paths that
  previously said less than they knew: a kill against a PID with no live
  session now carries `code: "not_live"` alongside the same `error` text it
  always had, and a **malformed** request body is now rejected with `400`
  before anything is collected or signalled instead of being parsed as an
  empty one. The second is the point rather than a side effect — an unknown
  field, an explicit `null`, a bare `null` body, or trailing content all used to
  decode to an empty id, which this endpoint reads as "no precondition", so a client typo
  (`sessionId`, the spelling the iOS model uses) would silently turn a guarded
  kill into an unconditional one. An absent body and `{}` are still accepted
  and still mean "no precondition". Duplicate keys are not detected: Go's
  decoder applies last-one-wins without reporting it.
- `actionResult` gained an optional `code` field so a client can tell the two
  refusals apart without matching on prose: `session_mismatch` (that PID is
  live, but it is a different session now) and `not_live` (no live session
  there). Both mean "refresh, the row was stale"; neither means "retry".
  Logical failures still answer HTTP 200 in the same `{ok, error}` envelope,
  and `omitempty` keeps the new field invisible to clients that don't read it.
- `POST /sessions/new` accepts an optional `request_id` (8–128 characters of
  `[A-Za-z0-9_-]`; anything else is `400 bad request_id`) that makes spawning
  idempotent. It is the only mutating endpoint that is not naturally safe to
  repeat — a second kill finds nothing live, a second resume is refused with
  409, a second worktree remove fails validation, but a second spawn creates a
  second tmux session and a second Claude process in the same directory. A
  repeat of the same id joins a spawn that is still running and replays one
  that already succeeded, so the phone-timed-out-and-the-user-tapped-again case
  costs nothing. The id is the whole key: reusing one with a different body
  replays the first result rather than honouring the new body. Failed spawns
  are deliberately not remembered, so a retry genuinely re-runs — and to make
  that true, a spawn whose `send-keys` fails now tears down the tmux session it
  had already created, which would otherwise survive as an orphan and be
  duplicated by the retry. Bounded to 32 remembered ids for 10 minutes, in
  memory; when all 32 are still running the host answers `503` rather than
  growing past the bound, since evicting a running entry would strand its
  joiners into a second spawn. Retry that with the same id. The tmux session
  name is derived from the `request_id`, so if the post-failure cleanup itself
  fails, a retry collides with the surviving session and errors rather than
  quietly creating a second one beside it.
- `POST /sessions/{pid}/migrate` now verifies the session file it re-reads
  rather than adopting it. It always re-read the PID's file to find the
  transcript to resume, so a pane recycled between the caller's check and that
  read would have had the new occupant killed *and* its transcript resumed.
  It also confirms the old process actually died before creating the new tmux
  session — counting a `kill(pid, 0)` of `EPERM` as *alive*, since that means
  the process exists and merely cannot be signalled — both signals discarded their errors, so a failed kill read exactly
  like a successful one, and two live sessions on one transcript corrupt it.
  A migrate that cannot confirm the process is gone now fails instead.
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
- `account switch NAME [--server S]`, `account save NAME` and
  `account list [--server S]`, plus `Ctrl+W` in the live view and a new
  `POST /account/switch`, port the standalone `claude-switch` scripts into the
  binary. Same files, same formats, same paths, so either tool still works on
  any machine. `Ctrl+W` lists the accounts the selected row's host holds —
  built from data the pollers already have, so it opens without fetching —
  marks the current one, and `Enter` applies it immediately (local rows switch
  in-process, remote rows post to that host); there is no confirmation step by
  design. `account list` reuses `GET /sessions`' existing `usage` /
  `knownAccounts` / `activeSnapshotName` fields rather than adding a second
  read endpoint, and an unreachable host prints its error in place of its rows
  instead of aborting the table.
  Two properties are worth knowing. Switching to the account that is already
  active is a **true** no-op — zero files touched — because re-applying a
  snapshot would overwrite a live token that may have refreshed since capture
  with an older one. And before the live credential is overwritten it is copied
  unconditionally to a single rolling `.last-switch-rescue.<ext>` slot, and
  again under its own name when the outgoing account can be identified, so no
  switch can leave you with no copy of the account you were just on — including
  the case where that account matches no snapshot at all. Concurrent switches
  and saves on one host serialize through an advisory lock on
  `~/.claude/.account-switch.lock`; a live Claude Code process rewriting the
  credential mid-switch remains an accepted, documented residual window.

### Fixed

- `--bind tailscale` now finds the CLI when it is not on `PATH`. The macOS GUI
  builds ship it inside the app bundle and install no symlink, so a Mac fully
  authenticated to a tailnet had nothing named `tailscale` to run and reported
  "no Tailscale IPv4 found — is tailscaled running and authenticated?", which
  is the wrong thing to go and check. `/Applications/Tailscale.app` and the two
  common install prefixes are tried after `PATH`, and the two causes now get
  different messages: one names the missing command and how to fix it, the
  other names the binary it actually ran and quotes what it printed. Under
  `service install` this was a permanent restart loop rather than a single
  confusing line.
- `--bind tailscale` validates that the CLI actually returned an address. The
  macOS bundled CLI exits 0 and prints "The Tailscale GUI failed to start" on
  stdout when it has no GUI session to talk to — which is what a launchd agent
  gets — and that sentence was being used as the bind address, failing with a
  DNS lookup of it. Note this means `--bind tailscale` cannot work from a
  LaunchAgent on a Mac whose only CLI is the app bundle; pass the address.

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
