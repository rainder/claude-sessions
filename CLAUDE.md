# CLAUDE.md

Guidance for Claude Code working in this repo. Full design rationale for
each rule lives in the code comments and in git history
(`git log -p CLAUDE.md`).

## What this is

A single Go binary that views and manages Claude Code and Grok CLI sessions
locally and on remote hosts. One binary, roles by subcommand:

- **client** (no args / `list`): live TUI with local + remote sessions
- **server** (`-s`): HTTP+JSON daemon for this host's sessions, plus the
  FLEET web page and the FLEET event loops
- **scriptable subcommands** (`kill`, `attach`, `migrate`, `new`, `preview`,
  `tmux-info`, `usage`, `account`, `fleet-url`, `*-events status`, ...)

It is a Go rewrite of a bash+python script, for single-binary distribution
(macOS dev box, Linux server, Pi).

## Build / test

```sh
make                                       # cross-compile every arch into ./bin/
make install                               # build, copy host binary to ~/.local/bin
make deploy-linux-amd64 HOST=user@server   # build + scp Linux/amd64
make deploy-linux-arm64 HOST=pi@somepi     # build + scp Linux/arm64
make run                                   # build + run host binary
```

`HOST` goes straight to ssh/scp. Per-developer targets go in
`Makefile.local` (gitignored, auto-included). Tests sit next to the code:
run `go build ./...`, `go vet ./...`, `go test ./...`.

## Workflow

Always work in a git worktree (`.claude/worktrees/<name>`), never on `main`:
the main checkout is the live dev environment. When `go build`, `go vet` and
`go test` are green, do all of these without asking:

1. Merge the worktree branch into `main` locally (no PR for solo work).
2. `make install`.
3. `git push origin main`.
4. Deploy to `agent-workstation` over ssh:
   `git pull --ff-only && make install &&
   PATH="$HOME/.local/bin:$PATH" claude-sessions service restart`
   (a login ssh session does not put `~/.local/bin` on `PATH`).

Then `git worktree remove` and `git branch -d` the branch.

## Architecture

One package (`main`), split into files by concern. Server handlers and
`cmd*` subcommands call the same primitives in-process (`session.go`,
`migrate.go`, `preview.go`, ...). The server never shells out to its own
subcommands.

Data flow: `CollectLocal` (session.go) reads `~/.claude/sessions/<pid>.json`
plus grok's registry (grok.go), enriches CPU/tmux/git (tmux.go), and returns
`[]Session`. `render.go` draws it; `remote.go` merges remote hosts.

`Session.ID()` is `"<pid>"` for local rows and `"<host>:<pid>"` for remote
rows. Actions route on `s.Host != ""` to `remote_actions.go`.

### Terminal invariants

- **Single stdin consumer.** No background reader goroutine. A prompt
  switches to cooked mode and uses `bufio.Scanner`.
- **`term.MakeRaw` zeros OPOST.** Call `enableOutputProcessing(fd)` every
  time you re-enter raw mode. `enterRaw` and `runInteractive` already do.
- **Tick uses `unix.Select`, not `SetReadDeadline`.** The deadline silently
  no-ops on stdin inherited at process start.
- **Mouse reporting has one owner, `mouseMode`** (mouse_mode.go). Priority:
  suspend > capture > select mode. Every raw re-entry writes
  `mouseMode.wanted()`, never a bare enable or disable.
- **Draw through `screenRenderer`**: it rewrites changed rows only and never
  clears. Wide runes are two cells (`runeCells`); `visualLen` / `clipLine`
  count OSC sequences as zero width and never cut one.

### Session rules

- `walkTmuxPane` checks `panes[cur]` before moving to the ppid: claude can be
  the pane pid itself.
- ssh attach needs the right user (`ssh_user` in servers.yaml or ssh config;
  `EffectiveSSHTarget()`): tmux sessions are per user.
- Every `-t` built from a row's `Tmux` goes through `tmuxExactTarget` (`=`
  prefix). A plain name prefix-matches another session when it is gone.
- A killed selection lands on its neighbor: `validateTargetSel` takes the
  previous and current target lists. Only `refresh()` passes the real
  previous list; view-filter call sites pass `nil` on purpose.

### Grok sessions (grok.go)

- `Session.Tool`: `""` is claude (old files and old servers decode as
  claude), `toolGrok` is grok. Render, sort and actions need no grok branch;
  attach/preview/send-keys/kill key off `Session.Tmux`.
- Registry `~/.grok/active_sessions.json`. Never take grok's lock. Missing,
  torn or bad = no grok rows, never an error: it must not blank the claude
  list. Exception: the resume picker and `ResumeGrokSession` read a torn
  registry as "unknown live set" and list no grok rows / refuse.
- `grokSummaryPath` order is a cost rule (CollectLocal runs every 2s):
  `url.PathEscape(cwd)` fast path → on a miss, check the directory exists
  → scan only when the directory is missing; memoize positive scans only.
  Use `os.ReadDir`, never `filepath.Glob` (home is data, not a pattern).
- Status comes from a tail of `events.jsonl` (8KB) mapped to claude's
  vocabulary; `WaitingFor` alone drives `Waiting()` and `StatusDisplay()`.
  Cost and background tasks come from an incremental scan of
  `updates.jsonl` (new bytes only, last-wins per `prompt_id`, subagent
  children summed separately). CTX comes from `signals.json` only.
- Pid collisions: a claude session file for the same pid wins in
  `CollectLocal` (IDs must be unique). `lookupLiveSessionByPID` tries claude
  first, but grok wins when the leftover claude file would not be listed
  (dead pid, scratch cwd, `sdk-*` entrypoint).
- Two liveness seams: `sessionPIDAlive` and `grokPIDAlive`. Tests must stub
  both (`allPIDsAlive` / `livePIDs` in grok_test.go).
- Reattestation routes on the row's tool, never on the pid.
- `CollectLocalLite` stays claude-only (account-switch warning).
  `resolvableSessionIDs` unions `grokLiveSessionIDs`, or the flag store
  prunes grok groups/disables on the next write.
- Snapshot restore, migrate and resume dispatch on tool. Grok has no
  `--worktree`. The kill-resurrect offer is claude-only.

### Resume picker (resume.go, resume_cache.go)

- Collector is lazy: a cheap stat pass (age, size, live ids), then content
  reads newest-first that stop at the limit. Do not dedupe in the cheap pass:
  the rule is "newest content-valid transcript per id".
- `readResumableHeadFn` is a seam that counts real reads. The disk cache sits
  in front of it, keyed by (path, mtime, size), never by session id. No lock;
  temp-file-then-rename.
- Filters: older than 30 days, live, empty/corrupt, scratch cwd
  (`/tmp`, `/private`), sidechain or non-`cli` entrypoint.
- Session ids from HTTP are validated in `ResumeSession` /
  `ResumeGrokSession` before any filesystem or tmux use.
- `resumeRows` re-runs every frame at the live width.

### Kill and worktree cleanup (worktree.go, kill_async.go)

- The cleanup check runs **before** the kill, on a **freshly collected**
  list (TUI rows are filtered). Survivors exclude the target, other hosts
  and tmux siblings. `git worktree remove` never uses `--force`; the branch
  is never deleted.
- TUI kills run in the background (`startKillJob`, wake pipe plus a per-tick
  check). `killInFlight` refuses a duplicate. A remote second kill that
  answers `not_live` / `session_mismatch` counts as success.
- The resurrect prompt after a failed worktree remove is local-only and runs
  on the main loop (it owns the terminal).
- `POST /worktree/remove` is gated by `validateWorktreePath` plus a live
  session re-check.

### Action preconditions and idempotency

- kill/migrate take an optional `{"session_id"}`. An empty body is OK. An
  unknown field, `null` or trailing content is `400`, because each would
  fail open. Decode through a pointer and require `dec.Token() == io.EOF`.
- The id is checked against the collected row, then again by `reattest`
  from the session file right before the destructive call. Local paths use
  `localReattest` and `MigrateLocalAttested`.
- Refusals are HTTP 200 envelopes with `Code` `session_mismatch` or
  `not_live`: "your row is stale, refresh", never "retry".
- `POST /sessions/new` `request_id` → `spawnDedupe` single-flight. Failures
  are not remembered; a spawn tears down a tmux session it failed to send to;
  all slots in flight = `503`. `group` (1..9, 0 = `400`) is best effort after
  the spawn: the server assigns in the background, the CLI waits.
- send-keys and resize require `session_id` and do one fresh
  `resolveLivePID`. send-keys types through `tmuxTypeArgs` and refuses a
  pane in copy-mode. Text may hold `\n` (no other control byte, not only
  whitespace): a claude row gets `M-Enter` between lines (`tmuxNewlineKey`,
  probe notes there), a grok row the lines joined. An old remote's 400 on a
  newline reads "too old for multi-line text". Resize revert is `set-window-option -u window-size`, never
  `resize-window -A` (that leaves `window-size manual` for good). `RunTUI`
  defers a revert; the local revert falls back to the snapshot's tmux name.
- The `send-keys` CLI (cmd_send_keys.go) takes the TUI compose box's path
  (`sendKeysToInspected`) and checks text with the server's own
  `validateSendKeysText` before resolving, so local and remote refuse alike.
- Accepted residual windows: a bare-pid kill with no tmux name, the gap
  between `reattest` and SIGTERM, and the gap before SIGKILL.

### FLEET (fleet*.go, fleet.html, fleet_assets/)

A phone page (`/fleet`) and a TUI view (`F`) for the one session named
`FLEET`. The server makes no decisions: it serves
`~/.claude/fleet/state.json` and `log.jsonl`, mirrors the transcript, and
types into panes. Spec and plan: `docs/superpowers/`.

- **Discovery:** exactly one local row, claude or grok, with a user-set
  `Name == "FLEET"`. 0 = `not_live`, 2+ = `ambiguous`; both refuse sends.
- **Auth:** bearer header, or the `cs_fleet` cookie holding a fleet-only
  secret (never the server token). Login is a one-time code from
  `claude-sessions fleet-url`; only a POST consumes it (link previews GET
  links). Cookie POSTs need `sameOrigin`. Use `Referrer-Policy: same-origin`,
  not `no-referrer` (that sends `Origin: null` and breaks the CSRF check).
  Static app files need no auth. `fleet-url --revoke` logs every device out.
- **Typing into a pane** (`fleetTypeInto`, shared by every sender): refuse
  when `WaitingFor` is set, the input box is not visible or not empty, the
  pane is in copy-mode, or a grok row is busy and the sender sets
  `refuseBusyGrok`. Type the whole text in one tmux call (`tmuxTypeArgs`:
  `send-keys -l --` per line, chained with `;` argv). tmux eats a trailing
  `;` of any argv, so no literal ends in one; those go as `-H 3b`. Press
  Enter only when the box holds a contiguous piece of our text (a claude
  `[Pasted text #N]` placeholder splits it into pieces that each must be
  ours), then verify Enter took. Never
  press Enter on text the server did not type. The box parsers
  (`paneInputText`, `grokPaneInputText`) fail closed; when a CLI changes how
  it draws, refresh `testdata/fleet/*.ansi` from a real pane.
- **Say text:** newlines flatten to spaces, except a no-card chat to a
  claude FLEET and every `/fleet/session/say` to a claude row
  (`fleetCleanChatText`), which keep line breaks (`M-Enter` between lines,
  one tmux call); a grok target gets them joined. Control
  bytes and a leading `!`/`#` on the first line are refused, and
  `[claude-sessions event]` is refused on folded text (`fleetFoldText`).
- **Answer store** is server-side. Its guard runs in `fleetSay` under `sayMu`
  on a fresh state.json, right before typing. `cardHash` never includes
  `updatedAt` or `priority`. Lock order: `sayMu`, then the store's mutex.
- **Waiting line.** Optional card object `waiting`: `who` (required, ≤ 40
  runes), `since` (required, RFC 3339), `answeredAt`, `note` (≤ 120 runes).
  A bad `who`/`since` drops it; a bad `answeredAt`/`note` drops alone; text
  folds to one line and is cut rune-safe. Renders `waiting on <who> since
  <T>` (` · <note>`) or `<who> answered <T>` (` (<note>)`); T is `HH:MM`
  today, else `MM-DD HH:MM` (Vilnius on the page, `time.Local` in the TUI).
  A valid one hides `waitingOn`. Not in `cardHash`. Page (`waitingLine`,
  node-tested against `fleetWaitingText`), TUI details and raw screen.
- **Affects line.** Optional card array `envs` of `prod`, `sandbox`, `dev`,
  `predev`, `local`, `none`. Unknown values and repeats drop, order is that
  list, `none` drops next to another value (`fleetDecodeEnvs`, node-tested
  against the page's `envList`). The page draws `Affects:` chips on every
  decision card, sent row and done row, `env: not set` when empty; chip text
  is the fixed label, never the card's string. Not in `cardHash`. The TUI
  details panel draws it under the meta line (`fleetEnvLines`, same colours,
  wrapped) and the raw screen plain; no line when empty.
- **Start FLEET:** a mutex across the handler, a 60s gap, a fresh collect,
  and a pending start blocks a second spawn (two taps must not make two
  FLEETs).
- **Outbox:** memory only; card answers are never held.
- **Auto switch** (auto_switch.go): `auto-switch.json` (`enabled` false,
  `threshold` 95, `pollSeconds` 120; parsed like the event configs). Spend
  gate: disk-only numbers first; `fleetClaudeUsage` (which may run a pass)
  only when the live reading is missing, 30 min old, past its reset, or
  within 10 points of the threshold. Decides on those fresh numbers only.
  Triggers only on a saved live account's fresh 7-day reading at the
  threshold whose reset has not passed; stale, expired or unsaved never
  triggers. Target: lowest fresh 7-day below it, 5-hour (when reported)
  and scoped weekly below too, in the saved list. Switches through
  `fleetDoSwitch`, the page chip's path (busy = skip, no cooldown; the live
  account must still be the one read). 30 min cooldown from the later of
  the last attempt and the last manual claude switch on the page; a torn
  state file starts one. A
  failure (not `already_live`) is one FLEET line. No target = one FLEET line
  per episode; with saved accounts lacking a fresh reading the line says so
  and does not end the episode; a list error sends nothing. Kill switch:
  `claude-sessions auto-switch off`, which also pins the live account.
- **Terminal switches:** after a TUI Ctrl+W switch lands (local or remote;
  `POST /account/switch` itself tells no one), the TUI runs `fleetLocal`
  discovery and posts `POST /fleet/account-event` `{host, tool, from, to}`
  (strict body like kill/migrate; host `[A-Za-z0-9._-]{1,64}`, else 400).
  The server switches nothing: it logs one `tui-switch` line and types
  `fleetSwitchEventTextOn`'s line (the page's, plus `on <host> (terminal)`)
  through `fleetSwitchNotify`, answering the delivery code. The toast says
  `FLEET told` or `FLEET not told (<reason>)`; not told or a warning is
  amber for 8 s. A failed switch opens a modal panel instead (bell once,
  sanitized error text, closes on Enter/Esc/q/Space/Ctrl-C, never quits).
- **Session view:** `fleetResolveSession` is the allowlist (a card's
  session: its `tmux` field, else its `session`, as the page's
  `cardSession` reads them; a working-list row, a local row disabled for FLEET that passes
  the list's other rules, or a usable desk; never FLEET's tmux); the pane
  address always comes from the resolved row, never the request.
- **Desks (CHAT, SLACK):** the chat panel's other targets are the sessions
  named in `fleetDeskSessionNames`, in order, one each (`fleetDeskRow`,
  findFleet's rules). `/fleet/state` serves them as `desks` (and CHAT alone
  as the old `desk`, for one release); the page reads and sends through
  `/fleet/session*`, whose allowlist also takes a usable desk's tmux session
  (even disabled), never FLEET's. A desk view pins every request to its own
  desk (`desk=<NAME>`, `"desk": "<NAME>"`; the old `desk=1` / `true` mean
  CHAT for one release): `fleetDeskPin` refuses with 409 `not_desk` unless
  that tmux session still holds that desk on the list the handler resolved
  from. Each target keeps its own DOM log and composer (one `#desk-tpl`
  clone per desk): a draft never crosses.
  After the desks, one picked-session slot (the `⋯` picker over
  `state.sessions` plus `state.disabledSessions`, one list in the working
  list's order, disabled rows tagged "off FLEET"; rows carry `sessionId`
  and `pid`; usable desks' tmux left out; `fleetEligibleSessions` is the
  shared filter, so `sessions` itself never holds a disabled row):
  a desk object with `pick` set, rebound by a new pick, a draft kept per
  picked session. Its requests pin the session id (`sid=<id>`, `"sid"`):
  `fleetSessionPin` refuses 409 `not_session` when the resolved row runs
  another session; a malformed id is 400 `bad_target`. Both pins may apply.
- **Clarify:** claude FLEET only. The fork is exec'd (no shell) with
  `--setting-sources ""`, a fixed tool list and a deny list, and never
  resumes FLEET's own session id.
- **Untrusted text:** state.json is LLM text built from ticket titles that
  strangers control. The TUI runs `fleetDisplayText` on every drawn string;
  the page builds DOM with `createElement` only (`fleet_page_test.go`
  forbids HTML sinks); links go through `safeLink` / `safeLinkURL`, and
  labels fail closed.
- **TUI client:** `fleetLocal` asks the local service first, then every
  servers.yaml host. Views carry `Source`; send, dismiss and attach refuse
  when the chosen host moved. Network calls run only in `fleetJob`s or the
  badge hub, never on the render path.
- **TUI keys:** a digit opens a confirm panel; `r` sends the option whose
  `Key == card.Lean` (exactly one match) with no panel, and only on an
  unanswered card (a correction is `c`, a digit and the panel). Both share
  `pickRefusal` and `sendOption` (same request, hash, `Source` and send job).
  `r` does nothing in a paste batch, or unless the list screen has drawn
  that exact card (id and hash, `st.drawn`, recorded by `renderFleetView`)
  for `fleetLeanPickDelay` (1s): any other frame or a changed pair restarts
  the wait, so a double-tap, a mouse move or a new state cannot answer an
  unseen card. `R` builds the set of cards `r` would answer (`leanBatch`),
  lists it with the skipped ones on a confirm panel (option panel's arming;
  Enter also waits until a frame drew the last card line, `seenAll`; a new
  state that moves FLEET or any listed card closes it), and sends it in one
  `fleetBatchJob` through the same send path, 1.1s apart; a host move,
  failed re-check, unclear result, 401 or a FLEET-wide refusal
  (`fleetBatchStopCodes`) stops it. Sent cards lock in `batchSent` as
  `justSent` does. `f` (list or ship screen, no panel/batch/Clarify open)
  opens the chat with FLEET (fleet_tui_chat.go): `/fleet/chat` read in a
  `fleetJob` every 2s while open, pinned to `Source`; the composer is the
  shared `textEditor` (Enter sends, alt-Enter / `\`-Enter a new line, a
  paste's Enter never sends); Esc keeps the draft, Ctrl-C clears it (the
  inspector's compose box and Clarify's input follow the same rule). `a`
  (attach) and `i` or `→` (chat) on a card share `fleetCardSession`, the
  page's `cardSession` on the raw card fields: `tmux` decides when set (not
  live = ended, no fallback), else `session` by display or tmux name, and
  the ticket only when the card has neither; no one session toasts why.
  `i`/`→` (list screen, not in a paste) opens the same chat panel on that session
  (`/fleet/session`, `/fleet/session/say`, pinned to the row's `sid` and
  `Source`; `gone`/`not_session` end it), one draft per tmux+sid beside
  FLEET's. Opening tells FLEET nothing; the first send to that session
  that goes through tells FLEET "Andy opened chat on card <id> (<tmux>)",
  once per card id per run, after Andy's send (the server's 1 s say gap
  is shared, and a no-card line that hits it is held, never refused), and
  marks the card told only once sent (no card id; id and tmux must pass
  `fleetSessionNameRe`; a refusal is a toast). `?` (list or ship screen)
  opens the key help (fleet_tui_help.go), which swallows every other key.
  The key help row offers `c`/`x` only through `canCorrect`/`canDismiss`,
  the keys' own checks, and `fleetFitHelp` drops its generic parts first.
- **Uploads:** type sniffed from the bytes, server-chosen file names, pruned
  after 7 days.
- **Web Push:** stdlib RFC 8291/8292. The endpoint host allowlist is the SSRF
  guard. Never log the endpoint path or keys.
- **Logs** hold counts and codes only: never typed text, titles, stderr or
  emails. The service unit has `StandardOutput=null`, so diagnostics go to
  files under `$XDG_STATE_HOME/claude-sessions/` (default
  `~/.local/state/claude-sessions/`; `service install` bakes only `PATH` and
  `TMPDIR` into the unit).

### FLEET event loops (idle/blocked/question/clickup_events.go)

Each loop is a goroutine started in `cmdServer`. Config is
`~/.config/claude-sessions/<name>-events.json` (missing = off, re-read on an
mtime/size change, a bad file keeps the last good one); state and log sit
under `$XDG_STATE_HOME/claude-sessions/`; `claude-sessions <name>-events
status` prints them.

- Delivery is `fleetEventTarget` + `idleDeliver`: a fresh collect, fail
  closed on none or two targets, then `fleetTypeInto`.
- `not_submitted` counts as delivered. A retry would duplicate the line.
- Every field a session or a stranger controls is scrubbed (`idleSanitize`,
  `scrubEventField`, `idleScrubTag`). No pane or transcript text except the
  scrubbed question line.
- The idle and question loops share `idle.mu`.
- ClickUp: a bad `cu` answer never reads as "tickets gone"; state is saved
  before anything is sent. On agent-workstation `cu` points at
  `~/.local/bin/cu-with-token`.

### Usage polling (usage*.go, known_accounts.go, account_cache.go)

- Identity (which accounts, emails, which is live) is free disk reads and is
  never cached. Numbers cost an Anthropic call that 429s readily (the budget
  is shared per token).
- Only the client hubs (`UsageHub`, `KnownAccountsHub`) fetch numbers.
  Exception: the enabled auto-switch loop may ask `s.numbers` (its spend
  gate allows one pass per 30 min while far below the threshold, the
  per-interval rule when near it).
  `GET /usage` is identity-only and never calls Anthropic.
  `GET /usage/numbers` may run one pass per interval, only when no hub wrote
  a cache file recently; `?cached=1` is disk only.
- One cache file per account, keyed by snapshot name, holds numbers and
  backoff; written with `writeFileAtomic`. Both fetchers reload it every
  pass. `entryIdentityMatches` gates numbers **and** backoff. The `fb*`
  in-memory fallback is used only when the name does not resolve.
- The 5-hour window is optional (`UsageInfo.FiveHour` nil): absent is no
  bar and no check, never 0%. A null `seven_day` falls back to the `limits`
  entry `weekly_all`; neither is a parse error.
- Only 401/403 is `Expired`. Other reasons: `rate limited`, `server error`,
  `timed out`, `unreachable`, `bad snapshot`, `wrong identity`,
  `needs refresh`. An expired access token answers **429**, not 401.
  `Reason` is a fixed tag, never error text (it can hold URLs).
- A transient failure carries the last good numbers with `Stale` and the
  original `FetchedAt`. `Expired` carries nothing; `wrong identity` carries
  only a `Verified` reading.
- Identity is bound to the token: a profile probe runs after a successful
  usage fetch, cached per token hash. A verified email beats the file.
- Backoff: second 429 in a row waits 4 min, third and later 8 min;
  `Retry-After` only lengthens it, capped at 1h. A loaded elapsed deadline
  keeps its streak.
- Parked snapshots rotate (`oauth_refresh.go`) under `withAccountLock`, and
  skip when the snapshot now matches the live account. Rotating a grant that
  is live on another host can kill that host's refresh token.
- `snapshotAccountNames` uses `os.ReadDir`, never `Glob`.
- The service and TUI must share one `TMPDIR` (hub-alive check). `service
  install` bakes it; re-run `install`, not only `restart`, after changes.
- `writeUsageHeader`'s fit search never clips a label or marker while some
  tier fits.

### Account switching (account.go)

- File names and formats match the standalone `claude-switch` scripts.
- `switchAccount`'s step order is the contract, all under
  `withAccountLock` (opened per call; flock is per fd): validate name →
  require an identity snapshot with a real email (`identitySnapshotEmail`,
  exact key) → refuse while the pending-switch marker is armed → no-op if
  already current (touch nothing) → validate the credential → warn about
  live sessions (never refuse) → rotate the parked snapshot → rescue copy →
  named sync-back → arm marker → write credential → patch `~/.claude.json`
  → disarm marker.
- `account save` refuses to reassign a name to another account without
  `--force`; `-y` is rejected there. `save` clears the pending marker.
  `remove` is CLI-only.
- `patchIdentityCache` decodes to `map[string]json.RawMessage`, touches only
  `oauthAccount`/`userID`, skips `null`, and writes temp-then-rename keeping
  the mode.
- macOS keychain "not found" needs exit code 44 **and** stderr
  "could not be found".

### Test seams

`keychainRead`, `keychainWrite`, `usageInfoFetch` and `profileEmailFetch`
panic in `TestMain`; every test installs a fake. Never run the real
`security` command in a test. Never type into real tmux in a test:
send-keys tests stub `sendKeysTmux` and `sendKeysPaneInMode`; FLEET say
rigs stub `fleetDeps.typeText` (it gets the whole text, all lines),
`pressEnter`, `capture` and `paneInMode`.

### Misc

- `yaml.go` is a hand-rolled parser for the servers.yaml shape only (a
  `servers:` list of flat scalar maps). Extend the parser before the schema.
- `termios_{bsd,linux}.go` are the only OS-specific files.

## Releases

Tags `v*` trigger `.github/workflows/release.yml` (cross-compile, SHA256SUMS,
GitHub release). Notes come from the matching `## [vX.Y.Z]` section in
`CHANGELOG.md`, so add it before tagging.

## Dependencies

Only `golang.org/x/term` and `golang.org/x/sys`. Stdlib for everything else.
Keep it that way: single-binary deployment is the point.
