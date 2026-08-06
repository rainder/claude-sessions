# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/);
this project follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- `account remove NAME` deletes one parked account snapshot — the credential
  blob (both platforms' file names, so a machine that has held either loses
  both) and the identity file beside it. It never touches the live login, and
  the rescue slot is unremovable by construction, since the only listing it
  validates against filters that name out. Removing the snapshot that stands
  for the account you are logged into is allowed but never quiet: it costs you
  the ability to switch back until `account save` recaptures it, so a terminal
  is asked and a pipeline has to pass `-y`. Local only — no endpoint, no TUI
  binding.
- `account switch` warns when Claude Code sessions are still running. Those
  processes hold the outgoing account's token and write a refreshed one back to
  the live credential store whenever it ages out, so one of them can overwrite
  the credential the switch just installed — or, when its own refresh token has
  been superseded and the server answers `invalid_grant`, make Claude Code zero
  the stored credential outright and log the host out of both accounts. Nothing
  can prevent that from here, so the switch says so and proceeds: it never
  refuses and never prompts, because the command is quite likely being typed
  inside one of the sessions it is warning about. `POST /account/switch`
  carries the same text in a new `warnings` field on the success response, and
  the Ctrl+W picker prints it.
- Inspector: press `i` to compose a line of text and send it as literal
  keystrokes (`tmux send-keys -l`) into the selected session's tmux pane,
  local or remote. Disabled with a "no tmux pane" hint when the session has
  none; a failed send keeps the buffer so it can be corrected and resent.
- Inspector: opening preview on a session resizes that session's tmux window
  to match the inspector's viewport, so its output wraps to your terminal's
  width instead of the width whoever last attached happened to have. Local or
  remote. The window is un-pinned again when you leave preview — including
  when you quit the TUI outright with the inspector still open, and before an
  Enter-to-attach hands you the real session.
- `GET /sessions` now carries an `api` object — `{"schema": 2, "capabilities":
  […]}` — so a client can tell an old host from a misconfigured one without
  probing endpoints. Until now nothing in any answer distinguished a server
  that enforced the `session_id` and `request_id` preconditions from one that
  silently ignored them, and every new endpoint would otherwise need its own
  "was that 404 an old server or a bad request?" dance. `schema` is bumped only
  when the payload changes in a way an older client cannot read past; adding an
  optional field is not that. `capabilities` is a flat list of action names a
  client gates its UI on. The object is emitted unconditionally and never
  omitted when empty: its **absence** is the signal that the host predates the
  handshake, which a client reads as schema 1 with the phase-B action set — so
  an old server stays healthy and simply shows an "update this host" hint where
  a name it needs is missing. The list lives in one function (`capabilities()`)
  which hands out a fresh slice per call, and clients must treat the names as
  permanent: append, never rename or reorder. A name is added by the same change
  that lands the route serving it — never ahead of one — so a name present here
  is a promise this build can keep.
- `GET /sessions/{pid}/preview` accepts `?offset=N` — the "preview-range"
  capability — so a client can page back through pane scrollback or transcript
  history instead of only ever reading the tail. `offset` counts rendered lines
  to skip from the newest end, the same unit whichever source answers, and it
  composes with `lines`/`bytes` rather than replacing them: those still size and
  cap the page. Absent or zero is byte for byte the request every client sent
  before paging existed — an unpaged capture sends the argv it always sent and
  never asks tmux for the pane geometry a page-back needs — so nothing that does
  not page pays for paging.

  Every answer is a 200, the one past the start of history included: an
  exhausted page is an empty body with the source header unchanged, never a 404
  and never a 500, so a client learns history has run out without having to read
  it out of an error. Paging is exact for a paused or finished session; a pane
  that is still printing shifts its own history under a plain offset, so a page
  taken mid-flow can repeat or skip lines.

  Each response carries `X-Claude-Sessions-Preview-Lines`, the number of lines
  in the body as the server itself counts them. A client pages by adding the
  number of lines it **received**, not the number it asked for — a page comes
  back short when `bytes` trims it or history runs out — so this is the number
  the next `offset` is built from, and it saves every client reimplementing "a
  trailing newline is not a line" and keeping that in step with the server
  forever. It is always sent, `0` included: its absence means the host predates
  the header, not that the page was empty.
- `GET /sessions/{pid}/attach` upgrades to a WebSocket carrying a live
  terminal, advertised as the `attach` capability. Every session already lives
  in tmux, so a faithful remote terminal is cheap in concept: the server
  allocates a PTY, runs `tmux attach-session` in it, and pumps bytes. Binary
  frames are raw PTY bytes in both directions; client→server text frames are
  JSON control, today only `{"resize":{"cols":C,"rows":R}}`, which becomes a
  `TIOCSWINSZ` on the master and so resizes the tmux client itself. An optional
  `?cols=&rows=` opens the PTY at the phone's own size, so the first frame it
  ever sees is not a standard 80x24 that resizes a moment later — which, with
  tmux sizing a window to its most recent client, the desktop user would watch
  happen too.

  `?readonly=1` runs the tmux client with `-r`, **and** drops every input frame
  server-side. The flag alone would put the enforcement inside the process a
  compromised or buggy client asked for; the drop is what actually holds, and
  it is proven by a test that starts the tmux client read-write while the
  connection stays readonly, so nothing but the server stands between a
  readonly socket and a live shell.

  Refusals happen before the upgrade, so nothing is started and no socket is
  accepted unless the PID still holds the `session_id` the client named
  (`session_mismatch` / `not_live`, the same precondition kill and migrate use)
  and that session has a pane (`no_pane`). They answer with the action-contract
  `{ok, error, code}` envelope but not with its HTTP 200 — a 200 in reply to an
  upgrade request is a protocol lie, and a WebSocket client is told the status
  and not the body — so a refused precondition is `409`, an over-cap session is
  `429` with `attach_busy`, and the body carries the precise code for anything
  that can read it. At most two terminals may be attached to one session at a
  time.

  A client hanging up closes the PTY and reaps the attach process, and nothing
  else: tmux detaches, and the session, its pane process and its scrollback all
  outlive the connection — the whole reason for attaching to tmux rather than
  to the process. A connection nobody has typed into for 30 minutes is closed
  with `1001 idle timeout`. Idleness counts client→server frames only, on
  purpose: tmux redraws its status line every `status-interval` seconds whether
  anybody is there or not, so a timer that terminal output could reset would
  never fire.

  The attach client's environment is built rather than inherited: `TERM` is
  forced (launchd starts this server with none, and tmux refuses to attach
  without one — and the PTY is read by the phone's emulator, never by the
  server's own terminal), `LANG` is forced to UTF-8 unless what was inherited
  already is, with a non-UTF-8 `LC_ALL`/`LC_CTYPE` dropped so it cannot undo
  that, and `TMUX`/`TMUX_PANE` are dropped so a server started from inside tmux
  does not produce a client that refuses to nest.
- Group assignments and the disabled bit are now **per-host shared state**,
  advertised as the `flags` capability. They live together in one file,
  `~/.config/claude-sessions/session-flags.json`, keyed by session id:
  `{"<session_id>": {"group": 3, "disabled": true}}`. `GET /sessions` carries
  both per row, and `POST /sessions/{pid}/flags {session_id, group?,
  disabled?}` sets them — action-contract envelope, `session_id` mandatory
  (there is no legacy caller to keep working, and a flag write with no
  precondition would act on whoever holds that PID now).

  The point is that both screens see the same badges. A group set on the phone
  shows up in the desktop's next poll, and one set in the TUI is visible to
  every client of that host — where before, groups were a client-machine-local
  file nobody else could read, so two desktops watching the same host disagreed
  and a phone saw nothing at all. Which host owns a flag follows the session,
  not the viewer: the TUI writes its own rows directly and sends a remote row's
  change to that row's host, exactly as `-`/`+` already did for disabled.

  `group: 0` clears an assignment; a field absent from the request body means
  "leave that flag alone", so a client changing one flag never overwrites the
  other from a stale view. An explicit `null` is rejected rather than read as
  either, per the tolerant-decoding rule: an absent field is an older client,
  a null one is version skew and is reported.

  Both writers on a host — the TUI and the server — take an OS file lock around
  a fresh read of the file, so neither ever writes back a copy made before the
  other's change. Unlike the two stores this replaces, a file that fails to
  parse is never overwritten: the store goes read-only, says so once on stderr,
  and leaves the bytes for a human, because a silent wipe would now lose every
  badge and disabled mark on the host at once. Entries are dropped when their
  session no longer resolves to anything live or resumable, and after 30 days
  with no sighting.
- `POST /devices/{token}/test` sends one test push to one registered device —
  the per-device form of `notify-test`, so a phone can test the push pipe from
  its own settings screen instead of needing a shell on the host. It answers
  `{"ok":true}` on delivery to Apple, `{"ok":false,"error":"..."}` carrying
  Apple's own reason string **verbatim** on failure, and `404` for a token this
  host has not registered. No request body is read.

  Two deliberate differences from the notification fan-out. A failed send is
  `200` with the `{ok,error}` envelope, not `5xx`: the send happened and its
  answer is the result, and the reason string — `BadDeviceToken`,
  `TopicDisallowed`, `ExpiredProviderToken` — is the whole point, so nothing
  rewords it. And a device Apple calls dead is *not* pruned here, unlike a real
  push: pruning would make the next test answer `404 unknown token` when the
  truth was "Apple rejected a token this host has registered", which is exactly
  how a sandbox/production mismatch reads. A diagnostic must not destroy the
  evidence it exists to produce.

  Advertised as the `test-push` capability, because that 404 already means
  "this host has no such token" — without the handshake a client could not tell
  it from an old host with no such route, and hiding the button on an old host
  is precisely what it needs the answer for.
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

- The resume picker no longer reports a busy host as unreachable.
  `GET /resumable` head-scanned and line-counted *every* transcript inside the
  30-day window and then discarded all but the newest 100 — on a host with
  5363 transcripts (157MB) that is a >50x waste and took 5.8–6s, past the
  picker's 5s per-host timeout, so a perfectly healthy host whose `/sessions`
  answered in 0.3s read as down. The collector now splits into a cheap
  stat-only pass (glob, mtime cutoff, live ids) and an expensive pass that
  walks the survivors newest-first and stops the moment the cap is full, so it
  parses ~100 files instead of thousands. Every filter, the per-session-id
  dedupe, and the mtime-desc ordering are unchanged — including the subtle
  case where a session id's newest transcript fails a content filter and an
  older, valid copy of it is what gets listed.
- Account rate limits are now attributed to the account the *token* belongs to,
  not to whatever `~/.claude.json` happened to say. Every usage fetch is paired
  with a profile lookup on the same token, and the verified email wins over the
  file read. This closes a read-order race — the token was read at one instant
  and the email at another, so a switch in between labelled one account's
  numbers with another's — and it makes the clobber above *visible*: a header
  suddenly showing an unexpected address is the credential store and the
  identity cache having drifted apart. The probe is a strict upgrade, never a
  precondition: it only runs after the numbers arrive, and any failure to reach
  it falls back to exactly the behaviour before this change.
- A credential snapshot whose token provably belongs to a different account
  than its `.<name>.account.json` claims now reads as `wrong identity` instead
  of putting that other account's bars on screen under this one's email. It is
  its own tag rather than `bad snapshot` (the file is perfectly readable) and
  never `auth expired` (the credential works — it is the name on it that is
  wrong); the fix is `account save <name>` while logged into that account.
  Carry-forward is unchanged, so this account's own last verified reading still
  shows, marked stale.
- `account switch` validates the snapshot before installing it. A credential
  with no refresh token, or one whose refresh token has already expired, is
  refused outright — installing it is a switch that logs the host out as soon
  as the access token ages out, which is exactly the failure that reads as "the
  switch worked and then everything broke". An expired *access* token is still
  fine and is not probed. When the access token is still valid, the profile
  endpoint is asked whose it is and a disagreement with the identity file is
  refused too — the same misattribution reported above, caught before it
  becomes the live credential. That probe is advisory: validation works offline,
  and an unreachable endpoint leaves the two file-only checks as the decision.
- `account save NAME` refuses to file the live credential under a name that
  already stands for a different account, which would misattribute the
  snapshot and make the next switch to that name install the wrong login.
  Refreshing a snapshot of the same account after a relogin — the documented
  use — is untouched, and a first save of an unclaimed name is never refused.
  `--force` reassigns deliberately, and still clears the pending-switch marker,
  so `save` remains the one complete recovery step.
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
