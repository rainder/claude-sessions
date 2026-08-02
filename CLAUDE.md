# CLAUDE.md

Guidance for Claude Code (claude.ai/code) working in this repo.

## What this is

A single Go binary that views and manages Claude Code CLI sessions locally and
across remote machines. One binary, three roles by subcommand:

- **client** (no args / `list`): live TUI showing local + remote sessions
- **server** (`-s`): HTTP+JSON daemon exposing this host's sessions
- **scriptable subcommands** (`kill`/`attach`/`migrate`/`new`/`preview`/`tmux-info`):
  non-interactive entry points for automation and shell pipelines

It began as a ~1500-line bash+python script; the Go rewrite exists for
single-binary distribution across machines (macOS dev box, Linux server, Pi) —
no wrangling Python versions or shell portability.

## Build / install / deploy

```sh
make                                       # cross-compile every arch into ./bin/
make install                               # build, then copy host-arch binary to ~/.local/bin
make deploy-linux-amd64 HOST=user@server   # build + scp Linux/amd64
make deploy-linux-arm64 HOST=pi@somepi     # build + scp Linux/arm64
make run                                   # build + run host binary
```

`HOST` goes straight to ssh/scp — anything `~/.ssh/config` resolves works.
Per-developer shortcuts (e.g. a named `deploy-myserver` target) belong in
`Makefile.local` (gitignored, auto-included). `go build .` / `go run .` work for
single-arch iteration. Tests sit next to the code (`usage_test.go`,
`render_test.go`); run `go test ./...` plus `go vet ./...`.

## Workflow

Always do implementation work in a git worktree (`.claude/worktrees/<name>`),
never directly on `main` — this repo's main checkout is also the live dev
environment, so isolating work avoids a half-done change blocking something
else. Once the work is done and verified (`go build ./...`, `go vet ./...`,
`go test ./...` all green):

1. Merge the worktree branch into `main` locally (worktree branches are local;
   there's no PR step for solo work here).
2. `make install` — refresh the local binary at `~/.local/bin`.
3. `git push origin main`.
4. Deploy to `agent-workstation` (see the deploy memory / project notes for the
   exact remote command — it's `git pull --ff-only && make install &&
   systemctl restart claude-sessions` run over ssh).

Then remove the worktree and its branch (`git worktree remove` /
`git branch -d`) once merged — don't leave stale worktrees lying around.

## Architecture

One Go package (`main`) split into files by concern. The three roles share a
common foundation; nothing is duplicated between them.

### Data flow

```
┌─────────────┐  CollectLocal              ┌──────────────────────┐
│ session.go  │ ────────────────────────►  │ ~/.claude/sessions/  │
└─────────────┘                            │   <pid>.json         │
       │                                   └──────────────────────┘
       │ enrich with CPU + Tmux
       ▼
┌─────────────┐     tmuxPaneMap + ppidMap + walkTmuxPane
│  tmux.go    │ ────────────────────────►  tmux list-panes -a, ps -A
└─────────────┘
       │
       ▼
   []Session  ──────────────►  render.go (RenderAll)
                                  + remote.go FetchAllRemote → []RemoteResult
                                  → multi-section table
```

### Three roles, one substrate

| Role | Entry | Calls |
| --- | --- | --- |
| TUI client | `RunTUI` in tui.go | `act_*` (local) / `act_*_remote` |
| HTTP server | `cmdServer` in server.go | `KillSession`, `MigrateLocal`, `SpawnNew`, `PreviewContent`, `tmuxSessionForPID`, `CollectLocal` (in-process — does **not** shell out) |
| Subcommands | `cmd*` in commands.go | Same underlying functions as the server |

The Go server handlers call the underlying primitives directly. The bash version
shelled out from server → subcommands; in Go that's an anti-pattern, so the
cmd_* subcommands and server handlers both wrap the same `migrate.go` /
`preview.go` / `session.go` primitives.

### Live TUI architecture

`tui.go::RunTUI` is the only place that owns the terminal:

1. `term.MakeRaw` (saving the cooked state) + `enableOutputProcessing` (gotcha below)
2. Print alt-screen / hide-cursor / disable-wrap escapes
3. Render → `readEvents(interval)` → handle keys / tick → repeat

Actions (act_kill, act_attach, act_preview, act_new) take an `actCtx` and may:
prompt for input (switch to cooked, `bufio.Scanner`, back to raw); shell out
interactively (`runInteractive` in helpers.go: exit alt-screen, restore cooked,
exec, re-enter alt-screen + raw); or recurse into `remote_actions.go` when the
selected row's `Host` is non-empty.

### Subtle invariants

**Single stdin consumer.** Only one thing reads stdin at a time. The TUI loop
uses `os.Stdin.SetReadDeadline` to time out (no goroutine). When an action
prompts, it switches to cooked mode and uses `bufio.Scanner`. The bash version
had a background goroutine that raced with the prompt scanner and silently
dropped "y" keystrokes — don't reintroduce that pattern.

**`term.MakeRaw` zeros OPOST.** That makes `\n` move the cursor down but *not* to
column 0, which visually destroys every multi-line render. The fix is
`enableOutputProcessing(fd)` (defined in tui.go, ioctl constants in
`termios_{bsd,linux}.go`). Call it **every time** you re-enter raw mode — initial
entry, after each prompt, after every `runInteractive`. Both `enterRaw`
(helpers.go) and `runInteractive` already do this; new call sites must too.

**Tmux pane detection must check the pid itself first.** `walkTmuxPane` (tmux.go)
walks pid → ppid up to 32 hops. It checks `panes[cur]` **before** moving to
`ppid[cur]`, because `tmux new-session "claude --resume ..."` spawns claude as
the pane's foreground process — claude's pid *is* the pane pid, with no shell
parent.

**ssh attach needs an explicit user.** Tmux sessions are per-user. If the local
username differs from the user the server runs as, `ssh host tmux attach` sees
"no sessions" — wrong namespace. Set `ssh_user` in `servers.yaml` or `User
<name>` for that host in `~/.ssh/config`. `ServerConfig.EffectiveSSHTarget()`
builds the `user@host` string.

**Wall-clock tick uses `unix.Select`, not `os.Stdin.SetReadDeadline`.**
SetReadDeadline silently no-ops on stdin inherited at process start (the Go
runtime's netpoller doesn't auto-register it), so `Read` blocks until input
arrives and the auto-refresh tick never fires under continuous typing.
`readEvents` in tui.go uses `unix.Select(fd+1, &fdSet, nil, nil, tv)` to poll the
raw fd with a real timeout. Don't "simplify" back to the Deadline API — it
silently re-breaks ticking on PTY-inherited stdin.

**Remote PIDs in `Session.ID()`.** Local rows have `Host == ""` and `ID()`
returns `"<pid>"`. Remote rows have `Host == "<name>"` and `ID()` returns
`"<name>:<pid>"`. Action dispatch uses `s.Host != ""` to route between local and
remote handlers.

### Resume picker

`resume.go` owns the whole `r`-key feature: collect past transcripts
(`~/.claude/projects/*/*.jsonl`; session id = filename stem), a searchable
picker (reuses `filterNewPickerLines`), and `ResumeSession(sessionID, cwd)` —
the shared primitive both the local TUI path and the server's
`POST /sessions/resume` handler call (409 when the session is already live).
Remote lists come from `GET /resumable`, fetched concurrently per host and
merged newest-first.

Collector filters (all server-side, in `collectResumableFrom`): >30 days old,
currently-live session ids, zero-byte/corrupt files, scratch cwds (`/tmp`,
`/private` — narrower than picker.go's `hiddenCwd`; worktrees stay resumable),
and agent transcripts — any entry with `isSidechain:true` or an `entrypoint`
other than `"cli"` (headless `claude -p` / SDK runs); a missing entrypoint
field passes (old format). Metadata comes from a single scan: cwd/branch/name
resolve within the first ~30 lines, but the scan runs up to 400 lines to
collect up to 3 user prompts, since later prompts routinely land past line 30
once tool_use/tool_result entries are interleaved in. NAME is best-effort:
user-set name from a lingering `~/.claude/sessions`
file (`nameSource != "derived"`), else the transcript's summary line, else
`-`. Session ids arriving over HTTP are format-validated
(`resumeSessionIDRe`) before touching the filesystem or tmux.

`resumeRows` autocompacts to terminal width: HOST only when a remote row
exists; then shed order is shrink PROMPT → drop #MSG → drop BRANCH → shrink
DIR → shrink NAME. AGE/NAME/DIR always survive; header mirrors the layout.
`pickResumeSession` re-runs it **every frame** at the live width (minus
`resumeRowIndent`) — a width baked in at open time leaves a resize to
`screenRenderer`'s blind right-edge clip, which chops a column mid-value
instead of shedding it. Selection is a background highlight
(`highlightSelectedRow`), not a marker column, so rows align with the header.
`→` opens a bordered overlay with the row's first three user prompts
(`ResumableSession.Prompts`, collected by the same head scan and serialized
over `/resumable`); Esc returns to the picker with the selection intact.

### Worktree cleanup on kill

`worktree.go` decides whether a kill empties a `<repo>/.claude/worktrees/<name>`
checkout and removes it on request. `worktreeCleanupTarget(target, all)` returns
the root only when the path is a *registered* worktree (`.git` is a FILE, not a
dir) and `worktreeSurvivors` is empty — survivors exclude the target, other
hosts, and the target's **tmux siblings** (a tmux-backed kill runs `tmux
kill-session`, so every pane in that session dies with it).

Three rules hold everywhere. The check runs **before** the kill (afterwards the
session is in no list). It runs against a **freshly collected** list, never the
TUI's rows — `localWorktreeCleanupTarget` re-collects because `c.targets` is
filtered by the active group and text query, and a filtered-out session in the
same worktree would read as "no survivors"; `git worktree remove` doesn't care
that a live process is sitting in the checkout. And removal is plain `git
worktree remove` from the main checkout — never `--force`, so a dirty worktree
survives and git's refusal is what the user sees. The branch is never deleted.

Three entry points: `actKill` (local TUI, second `confirmOverlay` — note it
needs raw mode back, since `prepareLineOutput` left cooked mode for the kill),
`cmdKill` (`--remove-worktree`, or a prompt when interactive; a bare `-y` run
keeps the worktree), and the server, which puts a `worktree` object in the kill
response and removes via authed `POST /worktree/remove`. That endpoint takes a
client-supplied path, so `validateWorktreePath` gates it (absolute, clean,
worktree root, real worktree) and the handler re-checks the live session list.

The worktree decision is made *after* the kill's optional `session_id`
precondition (below), so a refused kill never offers a worktree that is still
in use.

### Action preconditions and idempotency

`POST /sessions/{pid}/kill` and `POST /sessions/{pid}/migrate` accept an
optional `{"session_id": "…"}` body. Absent or empty it means what it always
meant — act on whatever is at that PID — and an empty request body is *not* an
error (`sessionIDPrecondition` tolerates `io.EOF`, since the desktop posts `{}`
and scripted callers post nothing). Everything *else* is strict, and
deliberately so: an unknown field, an explicit `null`, or trailing content all
decode to an empty id, which this endpoint reads as "no precondition", so each
one fails open. `sessionId` (the iOS model's spelling) is one typo away from
disarming the guard entirely, so all three are `400`. Duplicate keys stay
undetected — Go's decoder applies last-one-wins silently.

Supplied, the id is compared against the row `s.collectLocal()` resolves for
that PID, and then **again** by `reattest` from that PID's own session file
immediately before the destructive call. The second check is not redundant:
`collectLocal` maps tmux panes, resolves git roots and scans transcripts for
cost and agent counts, so its answer describes the world as it was when the
walk began. Re-reading the one file that establishes identity narrows the
window to the syscall itself. `kill` still hands `terminateSession` the
*enriched* row — its tmux metadata is what lets `KillSession` kill the pane
group by name instead of signalling a bare PID — and the attestation is what
confirms that row still describes what lives there. Migrate only collects when
an id was supplied, so the desktop path costs exactly what it did before, and
threads the id into `MigrateLocalAttested` so the re-read that finds the
transcript verifies rather than adopts.

The body itself is parsed through a *pointer* to the struct, because a
top-level `null` decoded into a struct value is a silent no-op that leaves
every field zero — another fail-open form. Exactly one JSON value is accepted:
`dec.Token()` must then report `io.EOF`, which `dec.More()` does not catch,
since More() only reports a well-formed next element and misses trailing
garbage that is not itself valid JSON.

Local kill/migrate get the same guard without a network hop: `localReattest`
(migrate.go) re-reads the PID's session file immediately before the
destructive call, the same two refusal cases as the server's `reattest`
(server.go:618), just in-process — `actKill`/`cmdKill` call it directly before
`KillSession`, and the local migrate sites (`actAttach`'s migrate branch,
`cmdMigrate`) get it for free by calling `MigrateLocalAttested` instead of
`MigrateLocal`. And every client now actually supplies the fields this section
describes: before this branch the desktop and remote paths sent bare `{}` (or
nothing) even though the server had carried `reattest` and `spawnDedupe` since
they were built, so the guards existed but nothing armed them. Remote
kill/migrate (`actKillRemote`, `actAttachRemote`, via `killRemote`/
`migrateRemote` in remote_actions.go) now send `session_id`, and remote spawn
(`actNewRemote`, `cmdNewRemote`) now sends a `request_id`
(`newSpawnRequestID()`), so `spawnDedupe` finally has a caller.

**Known residual windows, accepted.** The precondition narrows the race; it
does not eliminate it. Three remain and none is closable here: a session with
no tmux name is still killed by bare PID, since attesting a PID atomically with
signalling it needs `pidfd` and macOS has no equivalent; the gap between
`reattest` and the SIGTERM is still a gap; and so is the one between SIGTERM
and the SIGKILL escalation. The guarantee is "no action on a snapshot that has
visibly moved", not "atomic".

Refusals stay in the `actionResult` envelope at HTTP 200 and carry a
machine-readable `Code`: `session_mismatch` (that PID is live but is a
different session now) or `not_live` (nothing live there). `omitempty` keeps
the field invisible to clients that don't know it. Both mean "your row is
stale, refresh" — neither means "retry".

`POST /sessions/new` accepts an optional `request_id` (8–128 chars of
`[A-Za-z0-9_-]`, else `400 bad request_id`). It is the one mutating endpoint
that is not naturally safe to repeat — a second kill finds nothing live, a
second resume is 409, a second worktree remove fails validation, but a second
spawn happily creates a second tmux session. `spawnDedupe` single-flights by id
the way `sessionCache` does for `/sessions`: a repeat joins a running spawn,
and replays a *successful* one for `spawnDedupeTTL` (10 min, `spawnDedupeMax`
32 entries, pruned on insert, in memory only). The id is the whole key: reusing
one with a different body replays the first result. Failures are deliberately
not remembered so a retry genuinely re-runs — which is only safe because
`SpawnNew` and `MigrateLocalAttested` now tear down a tmux session they created
but failed to send a command to, instead of leaving an orphan for the retry to
duplicate. Publication goes through a `defer`, so a panic cannot leave joiners
parked forever, and joiners `select` on their own request context so a hung
tmux does not hold their connections. Completion is published the moment the
outcome is known rather than after the response is written, so a client that is
slow to read cannot hold a finished spawn's slot and make a later request look
like saturation. Cleanup is bounded (`tmuxCleanupTimeout`) and logs loudly with
the session name, since nothing else will ever mention an orphan again; and the
tmux name is derived from the request_id (`spawnSuffix`), so if cleanup does
fail, the retry collides with the survivor and errors instead of quietly
building a second session next to it. Eviction never touches a running entry;
when all `spawnDedupeMax` are in flight the handler answers **503** rather than
growing past the bound. `s.spawn` and `s.attest` are the injectable seams
alongside `collect`/`terminate`/`removeTree`.

### Usage polling

Two different things live here and they cost wildly different amounts, which is
the whole shape of this subsystem. **Which** accounts a host has, their emails,
and which one is logged in are local file reads — free. **How much** of each
account's rate limit is used costs an Anthropic round trip, and that endpoint
429s readily, since every Claude Code session shares the account's per-token
budget (`usageRetryMin`). So the free half is recomputed from disk every single
time it is asked for and never cached anywhere, and only the numbers go through
a cache.

`usage.go` polls Anthropic's OAuth usage endpoint (token from the macOS Keychain
/ `~/.claude/.credentials.json`) every 2 minutes in a background goroutine
(`UsageHub`, following `RemoteHub`'s pattern minus the wake pipe); the TUI header
shows it as two progress bars (5-hour and weekly limits). The token is read-only
— refresh/rotation is Claude Code's job.

`known_accounts.go` covers the *other* accounts each host knows about:
`claude-switch` parks a credential snapshot per account
(`~/.claude/.<name>.keychain-cred` on macOS, `.<name>.credentials.json`
elsewhere, plus `.<name>.account.json` for the email), and `KnownAccountsHub`
(same `usagePoller` generic, `T = knownAccountsResult`) fetches each one's
limits with that snapshot's own token. Strictly read-only — nothing is ever
swapped into the Keychain or the live credential file. Three rules hold. The
**live** account is never read from its own snapshot (Claude Code rotates the
live token in place while the snapshot keeps whatever was stashed at switch
time, so the snapshot can look expired while the session is healthy) — it is
skipped by email equality against `loadAccountEmail`. A per-account failure is
never an error: it becomes that entry's own classification, so
`allKnownAccounts` always returns a full slice and the poller's
whole-batch backoff only ever fires on a host-level problem, never because one
account is flaky. And only entries with numbers are cached, so a restart can
seed a recently-good account but never resurrects an expired one.

**A failed fetch has four outcomes, not two.** `Expired` used to mean "the last
fetch failed", which is how a 429 came to render as `auth expired` and send the
user off to re-login over a throttle — and the endpoint 429s constantly, since
every Claude Code session shares the account's per-token budget. So
`classifyUsageErr` (usage.go) splits the failure on the typed `usageHTTPError`
that `fetchUsageInfo` now returns: **401/403 alone** is `Expired` (the only
actionable state — that credential really is dead), while 429 → `rate limited`,
5xx → `server error`, `errUsageFetchTimedOut` → `timed out`, and anything else
(network, parse) → `unreachable`. A credential snapshot that cannot be read or
parsed at all never reaches `classifyUsageErr` — there was no answer to
classify — and carries its own `bad snapshot` tag (`usageBadSnapshotReason`,
known_accounts.go) rather than falling in with `unreachable`: nothing about it
is a network problem, and unlike every tag above it does not heal on its own.
The fix is `account save <name>` while logged into that account, the same
recovery `switchAccount`'s identity precondition names. `Reason`
is a short fixed tag and never the underlying error's text: a `*url.Error`
stringifies with the whole request URL, which would land in a one-line header
placeholder and in the `/usage` JSON.

A transient failure then **carries the last good numbers forward** rather than
blanking the account — in `KnownAccountsHub`, the client-side poller, which is
the only thing anywhere with memory of a previous pass (the server classifies
identically but has no `prev`, see `/usage` below):
`knownAccountUsage` takes a `prev` and re-serves its
`Info` with `Stale: true`, the failure's `Reason`, and `prev`'s **original**
`FetchedAt` — restamping it would let an account that fails forever look
forever fresh, when that timestamp is the only thing bounding the carry
(`carryable`, `usageCacheMaxAge`). An `Expired` entry carries nothing forward:
bars beside a dead credential imply it still works. The four states are
`Info` fresh / `Info` + `Stale` / `Expired` / neither-with-a-`Reason`, and
`KnownAccountUsage` documents them.

Memory across polls lives in `newKnownAccountsFetcher`'s closure, because
`usagePoller`'s `fetch` takes no arguments. Its `last` map is **rebuilt** from
each pass (intersected with the current snapshot names, entries with `Info`
only), so a renamed or deleted snapshot stops being re-served instead of
lingering for the life of the process. `NewKnownAccountsHub` reads the disk
cache **once** and hands it to both the poller (what the header shows before
the first fetch) and the fetcher (what a first pass landing mid-throttle
carries forward). There is deliberately no memoryless one-pass wrapper beside
it: nothing outside the poller calls one, and keeping one alive for a test to
exercise is how dead code survives — the test that used it now drives
`newKnownAccountsFetcher(nil)()`, the shape production actually runs.
`usageInfoFetch` is the package-var seam that lets tests drive the poller
without a network (same pattern as `keychainRead`/`keychainWrite`).

The disk cache ages **per account**, not per file. Entries in one file no
longer share a vintage — a carried-forward entry's numbers can be
`usageCacheMaxAge` older than the write that persisted them — so
`loadKnownAccountsCache` gates each entry through the same `carryable` a live
carry-forward uses and ignores the envelope timestamp entirely. A cache file
written before per-account timestamps existed decodes to the zero time and is
dropped whole on the first load; that is intended, and there is deliberately no
fallback to the envelope's clock.

`usageCache` (usage_cache.go) was left alone on purpose — it has no last-good
layer. For any account **this machine** holds a snapshot for that is enough:
`dedupeAccounts` already prefers the local known-account line over any remote's
copy of the same email, so the local hub's carry-forward is what the header
shows and the server's answer never gets a chance to matter. The gap is an
account only a **remote** holds a snapshot for — nothing anywhere remembers its
last good numbers, so a 429 there still renders as a bare `rate limited`
placeholder with no bars. That is no worse than the `auth expired` placeholder
it replaced, and it is left open rather than papered over: closing it means
`usageCache` growing a last-good layer of its own — a second carry-forward with
its own age bound, running on a different machine from the one whose header the
numbers land in — and one place deciding how old is too old is worth more than
a remote-only account's bars.

**Both hubs run in the TUI client only.** A `claude-sessions -s` server used to
start them too, so every host polled Anthropic every 2 minutes for the life of
the process whether or not a client had ever connected — and hosts sharing an
account each paid separately for numbers the client's own `dedupeAccounts`
would then throw away, deepening exactly the 429s that surface as "auth
expired" flicker in the header. The server now answers `GET /usage` on demand
instead, and makes no request until a client asks. The Codex side
(`codex_usage.go`) still polls on the server and still rides `/sessions`; only
the Anthropic half moved.

`snapshotAccountNames` lists snapshots with `os.ReadDir` + a name filter, never
`filepath.Glob`: the home directory is data, not a pattern, so a home path
containing `[` would make the *only* error path fire (permanent backoff, every
account gone) and one containing `*` would match into sibling directories and
read their credential snapshots. Nothing below an unresolvable `$HOME` errors —
a missing or unreadable `~/.claude` is an empty list, which is what keeps the
never-fail-the-batch property intact.

**The `/usage` endpoint.** `GET /usage[?ignore=a@x.com&ignore=b@y.com]`
(server.go, beside `sessions`) answers `usageResponse`: `usage` (the live
account), `knownAccounts`, and `activeSnapshotName`. The whole account universe
— names from `snapshotAccountNames`, emails from `snapshotAccountEmail`, and
which snapshot matches `loadAccountEmail` — is resolved from disk on **every**
call, mirroring `allKnownAccounts`' skip rule (every snapshot standing for the
live account is left out of `knownAccounts`; the first one names
`activeSnapshotName`) but split apart from fetching, since fetching is now
conditional. That is what removed a whole class of staleness: an earlier
revision of this code had `kickUsage`/`kickKnownAccounts` seams on the server
and matching `Kick()` calls after a remote account switch, purely so
`activeSnapshotName` would stop describing the pre-switch account before the
next 2-minute tick. Nothing caches it now, so the next call is already right and
those seams are gone.

`ignore` names accounts the caller already holds good numbers for, so the host
skips their fetch. It is a **repeated** parameter, never comma-joined — an
email's local-part may legally contain a comma, and `r.URL.Query()["ignore"]`
parses repeats natively. An ignored account is still reported, with a nil
`Info`: `accountRowsFrom` (account_list.go) builds the Ctrl+W picker's and
`account list`'s rows straight out of `knownAccounts`, so omitting it would
silently remove it as a *switch target*, and the header is unaffected either way
because `dedupeAccounts`' `addKnown` drops an entry with no `Info`, no `Expired`
and no `Reason` — which an ignored account, never fetched, is exactly. The live
account's `Account` (its email) is populated even when its
numbers were ignored or failed — reading it is free, and it is what labels that
host's heading. Per-account failures go through `classifyUsageErr` into that
entry's `Expired`/`Reason`, never an error for the request, and a success
stamps `FetchedAt`; the server keeps no cross-request memory, so it never
produces a `Stale` entry — carrying numbers forward is the client hub's job.
Accounts are fetched **concurrently**, one goroutine each, because a
cold cache with a handful of accounts would otherwise serialize into N × the
endpoint's 5s timeout and outlive the client's own.

`usage_cache.go` is the only thing that remembers anything: a per-email
single-flight + TTL cache modeled on `spawnDedupe` (same map-of-flights,
claim/join/publish shape), wrapped in a single `GetOrFetch` so `begin`/`finish`
can't be misused. It differs from `spawnDedupe` in two ways. Failures are
*kept*, for a short `usageCacheFailTTL` (15s) rather than forgotten — nothing
was created, so there is nothing making a retry unsafe, only an endpoint to stop
hammering when a burst of clients arrives during a throttle (that window absorbs
*concurrency*; the consecutive-429 backoff below absorbs *time*, and neither
replaces the other); success keeps
`usageCacheTTL` (100s, deliberately a little under `usageRefreshInterval` — a
cache entry's clock starts when its fetch *completes*, strictly after the poll
tick that triggered it, so a TTL exactly equal to the poll interval would mean
the next tick almost always lands just before expiry and reuses a stale answer
instead of refreshing, roughly doubling steady-state staleness). And an
**empty email bypasses the cache entirely**: an account whose `.account.json`
is missing or unreadable resolves to `""`, and two different such accounts
would otherwise collide on one key and be served each other's limits, fetched
with the wrong token. Entries are pruned on insert (no size cap — the key space
is "accounts this host holds snapshots for"; pruning exists so a renamed
snapshot stops occupying an entry, not to enforce a bound).

Every fetch inside `GetOrFetch` is itself bounded (`runBounded`,
`usageFetchTimeout` = 10s), independent of whatever `fetch` does. Without this,
a fetch that never returns — a locked macOS Keychain prompting a SecurityAgent
dialog nothing will ever answer in a headless launchd/systemd session
(`loadOAuthToken`, usage.go), or a wedged filesystem read (`snapshotToken`) —
would leave that email's flight permanently unfinished: `expired`/`prune` both
treat an unfinished flight as never-expired, the same rule that correctly
protects a genuinely in-flight fetch from eviction, so every later `GET /usage`
call for that account would park a new goroutine on the same flight forever and
the account would never be fetchable again for the life of the process — a
strictly worse failure mode than the always-on poller this replaced, which just
left one snapshot stale while every other request kept working. `runBounded`
guarantees the flight always finishes, publishing `errUsageFetchTimedOut` that
the short `usageCacheFailTTL` then lets a later caller retry past — self-healing
if the hang was transient, and at worst one leaked goroutine every
`usageCacheFailTTL` for a persistently wedged account, not one per client per
`usageRefreshInterval` forever. Keying by email rather than by snapshot name
also means two differently-named snapshots sharing one email share one fetch
and outcome — accepted, since two snapshots for the identical account is an
unusual setup and the alternative would refetch the same account once per name.

**Consecutive-429 backoff.** One Anthropic account is routinely held as a
credential snapshot on *two* hosts, and each fetches it on its own
`usageRefreshInterval` tick — `localFreshAccountEmails`' `ignore` list only
suppresses the remote's copy once the local fetch is already **fresh**, which is
exactly what a chronically throttled account never becomes. So both sides kept
paying a failed round trip every 2 minutes, forever, against the endpoint doing
the throttling. Both fetch paths now count consecutive `rate limited` outcomes
per account and skip the fetch outright while a wait is armed: the first 429
imposes no wait of its own (most heal within one tick — only an explicit
`Retry-After` can delay it), the second waits `usageBackoffSecond` (4min), the
third and beyond `usageBackoffMax` (8min, the cap). A 429's `Retry-After`
(`parseRetryAfter`, either RFC 9110 form) may only *lengthen* that wait, bounded
by `usageBackoffCeiling` (15min) so a bogus header can't park an account for the
life of the process. On a pass that actually attempts the account, any outcome
other than another rate-limited failure — numbers, `Expired`, a different
failure, or the snapshot turning out to be the live account — clears the streak
(`usageBackoffUntil` / `usageBackoff` in usage.go hold the shared arithmetic;
the two schedulers hold their own state). A *skipped* pass never touches the
streak either way, even if the synthesized path's own local reads (e.g.
`snapshotToken`) would have reported something other than a throttle, like a
now-unreadable credential — the already-armed wait simply stands, and
self-corrects at the next real attempt once it elapses.

Client-side it lives in `newKnownAccountsFetcher`'s closure beside `last`, keyed
by snapshot name and rebuilt from `snapshotAccountNames()` each pass for the same
reason `last` is. A backed-off account is **not** filtered out of the batch: it
still goes through `knownAccountUsage` with a synthesized 429 in place of the
round trip, so one place keeps deciding whether that name stands for the live
account (which is what resolves `ActiveName`) and whether `prev`'s numbers may be
carried forward — the entry renders as the same stale-with-`rate limited` line a
real throttle produces. Both maps are read and written only in the serial
closure, never inside the goroutines `allKnownAccounts` spawns. Server-side it is
`usageCache.failures`, keyed by email and deliberately separate from `entries`
(a flight is one attempt; a streak outlives many), swept by `prune` after
`usageBackoffForget`. Only the goroutine that owned the flight records an
outcome, so a burst of joiners is one attempt rather than one per caller; a
turned-away caller gets `errUsageBackoffActive`, which `classifyUsageErr` maps to
the same `rate limited` tag the skipped round trip would have produced.

On the client, `FetchRemote` no longer decodes the three account fields at all,
even from an older server that still sends them — two writers for one field
would make the winner depend on poll ordering. `RemoteUsageHub`
(`remote_usage.go`) polls `/usage` per host on `usageRefreshInterval`, and
`tui.go` overlays its `Snapshot()` onto `remotes` in `settleRows` via
`overlayRemoteUsage` (remote_usage.go), which is the single place those fields
are written client-side. `render.go` needed no changes — the field shape
didn't move, only its source. The hub is `RemoteHub`'s smaller sibling minus
the wake pipe (a percentage doesn't need an instant repaint; the 2s render
tick suffices). Its recurring ticker is phase-shifted by
`remoteUsagePhaseOffset` (30s): a remote host has no ticker of its own, so
without the shift this machine's own fetch of a shared account and every
remote's fetch of that same account land in one tight window every 2 minutes —
the shape that turns a shared per-token budget into mutual 429s. The offset
applies to the ticker only; `NewRemoteUsageHub`'s initial kick still fetches
immediately, and `stop`/`kick` stay live through the offset window, so
Pause/Resume/Shutdown are never held up by it. The hub replaces its whole
result set per pass rather than
streaming per host, so a failed host's numbers *clear* instead of freezing —
the same reasoning `mergeRemoteResult` documents. The `stale` marker does not
change that: it belongs to `KnownAccountsHub`'s per-account carry-forward, and
`RemoteUsageHub` has no equivalent — it keeps no memory across passes, so a
frozen remote reading would carry nothing marking it and would silently pass
as live.
`FetchRemoteUsage`'s own timeout is 8s, not `FetchRemote`'s 5s: a cold-cache
`/usage` handler fans out one fetch per account concurrently against that same
5s-per-account budget (server.go), so the handler's own wall-clock time can
approach 5s before a 5s client would give up on it.

The `ignore` list is recomputed each tick by `localFreshAccountEmails` from this
machine's *own* hub snapshots, and only from entries it actually has numbers for
(`Info != nil`, and neither `Expired` nor `Stale` for known accounts). A
locally-**expired** email is deliberately excluded: telling every remote to skip
the one account this machine has no numbers for would turn one transient 429
here into a blank bar everywhere, which is the opposite of the point. **`Stale`
is excluded for exactly the same reason** — carried-forward numbers are this
machine's memory of an account it currently cannot reach, and a host that *can*
reach it must not be told to skip it. (Local's own bars can be up to
`usageCacheMaxAge` stale from a disk-cache warm start — accepted, since the
remote's bar then just mirrors what local already shows.)

One-shot CLI paths have nowhere to put a poller, so they pay for one extra
parallel round instead: `mergeRemoteUsage` (remote.go) overlays `/usage` onto
`FetchAllRemote`'s results for `cmdList` and `cmdListSessions`' rendered branch
— placed *after* the `--json` early return, which never prints account fields —
and `FetchAllRemoteUsage` / `oneRemoteUsage` serve `account list`, which drops
its `/sessions` poll entirely since `accountRowsFrom` never reads `Sessions`.
The two differ in one deliberate way: a `/usage` failure is silent in
`mergeRemoteUsage` (the row's session list is still good, and overwriting
`Error` would print a usage failure beside it) but becomes the row's `Error` in
`FetchAllRemoteUsage`, where a silent skip would read as "this host has no
snapshots" rather than "this host is unreachable". A `404` from a server
predating the route is just another per-host failure; the reverse mismatch needs
no handling, since this repo's deploy order always upgrades the local machine
first.

`dedupeAccounts` merges known accounts in a second pass after the live-account
pass — a live line always wins over any host's snapshot copy of the same email —
and a failed entry keeps its line instead of being dropped: an `Expired` one as
the dim `auth expired` placeholder, a transient one as a dim placeholder naming
its `Reason` (`rate limited`, `bad snapshot`, …), and a `Stale` one as its bars
plus a dim `stale` marker. That marker is appended after the **last** segment,
which is the one place `segTrailerText` never pads or aligns, so it shifts no
column on any other line (`usageSegsLine` exists to make that append possible).
It does still take part in **sizing**: `writeUsageHeader` carries its display
width as that entry's `suffixW` into `lineBarW`/`usageLineFixedWidth`, so the
shared bar width shrinks to leave room for it. Alignment and sizing pulling
apart like that is deliberate, and leaving the marker out of *both* was a real
bug — with two collision-promoted full-email labels at ~64 columns the bars
sized to fill the entire budget, `cropTableFrame`'s clip then ate the marker,
and the stale line rendered byte-identical to a live one, which is the single
thing the marker exists to prevent. The one entry shape
still dropped is `Info == nil && !Expired && Reason == ""` — that is precisely
the **ignored** account `/usage` reports for identity only, and giving it a
header line would undo what `ignore` is for. Snapshot-derived lines are never
marked `mine`, so one
can never render bare and pass as this machine's live account. Host headings
show each host's active account email, dimmed, after `LOAD` (`section.account`,
fed by that host's `Usage.Account` — which is why `/usage` reports the live
email even when it reported no numbers for it).

### Account switching

`account.go` is the Go port of the standalone `claude-switch` scripts, and the
two stay interchangeable: identical file names, formats and paths
(`.<name>.keychain-cred` / `.<name>.credentials.json` / `.<name>.account.json`),
so either tool can switch an account on any machine with no migration step.
`account_list.go` renders `account list`; `account_picker.go` is the Ctrl+W
overlay and its action. Entry points: the `account switch|save|list`
subcommands, `POST /account/switch` (bearer auth like every mutating endpoint,
`400 unknown_account` / `500 switch_failed`, no `session_id`-style precondition
because it is host identity, not a session), and Ctrl+W in the TUI (picker →
Enter → done, no confirm dialog).

`switchAccount`'s step order **is** the contract, all of it inside
`withAccountLock`: validate the name against `snapshotAccountNames()` (nothing
is read or written past a failure here) → require the target to have a
readable, parseable `.<name>.account.json` identity snapshot with a real,
non-null email (a file that merely exists and parses isn't enough — see
below), refusing with a message naming `account save <name>` as the fix if it
doesn't (nothing is read or written past this failure either) → refuse if a
previous switch left the pending-switch marker armed (see below) → if it is
already current, return immediately, a *true* no-op touching zero files
(re-applying the snapshot would overwrite a live token that may have
refreshed with a stale one) → unconditional rescue copy of the live
credential to the single rolling `.last-switch-rescue.<ext>` slot → when the
outgoing account resolves to a name, its own credential + identity snapshot →
arm the pending-switch marker → only then overwrite the live credential →
patch `~/.claude.json` → disarm the marker → return the email re-read from
disk. The rescue and named-sync-back steps exist so a failure can never
strand you: the outgoing credential always has at least one copy on disk
before anything overwrites it, including when its account can't be named
(first switch, renamed account), which is exactly the case the *rescue* copy
covers and the named sync-back cannot.

**Why a credential-only snapshot is refused, not silently applied with a
"`/login` once" fallback.** An earlier version of this code allowed it: the
identity patch was simply skipped, matching what the standalone shell scripts
already tolerate. That leaves `~/.claude.json` naming the OUTGOING account
while the credential installed is the incoming one, and there is no reliable
way to detect that split state after the fact — `currentAccountName` only has
the (now-stale) identity cache to go on, and a later switch reading it would
back the wrong live credential up under the wrong outgoing account's name,
corrupting it. A "last switch" marker was tried to paper over this and
rejected: it made the wrong snapshot look current every time something else
rewrote `~/.claude.json`, which Claude Code does constantly for reasons that
have nothing to do with identity (project history, tips state, onboarding
flags). Refusing upstream — before touching anything, including the rescue
backup — is what keeps `currentAccountName`'s plain email matching always
correct: every switch this tool performs now leaves identity in sync with the
credential it installs, with no exception to reason about. On both machines
this ships to, every snapshot already has its identity file (verified, not
assumed), so this precondition costs nothing in practice; a legacy
credential-only snapshot self-heals with one `account save <name>` run while
logged into it. The check is on the DATA, not the file's mere existence:
`identitySlice` (the counterpart that builds a snapshot in `saveAccountSnapshot`)
writes an explicit JSON `null` for any identity key `~/.claude.json` didn't
have at capture time (jq parity with the shell scripts), so a snapshot saved
while not actually logged in is syntactically valid but practically empty.
This is checked via `identitySnapshotEmail`, not `snapshotAccountEmail` (the
latter is for display elsewhere and unmarshals into a Go struct, which falls
back to case-insensitive key matching) — `identitySnapshotEmail` looks the
already-parsed map up by the exact key `"oauthAccount"`, the same lookup
`patchIdentityCache` itself performs, so validating "is this usable" and
determining "what will actually get copied" ask the identical question
against the identical bytes. A struct-based check and a map-based patch
disagreeing on key casing was a real, independent-review-caught bug: a
snapshot with any casing but exactly `"oauthAccount"` would have passed a
looser check while the patch silently copied nothing.

**The pending-switch marker (`~/.claude/.account-switch-pending`) closes the
one gap the identity precondition above cannot: two separate writes to two
separate stores (the live credential, then `~/.claude.json`) can't be made
atomic together, so a process killed between them leaves exactly the split
state the precondition exists to prevent — just reached by a crash instead of
a missing file.** The natural response to "that switch didn't seem to take"
is to run it again, and doing so with a stale identity cache would
misattribute the outgoing backup to the wrong snapshot and corrupt it — for
ANY subsequent switch target, not just a retry of the same one. The marker is
armed right before the credential write and disarmed right after the identity
patch succeeds; while armed, every switch (any target) refuses. `account save
<name>` ALSO clears the marker (not just a completed `switchAccountLocked`) —
capturing what's live right now under a name is exactly the human
confirmation the marker is waiting for, and making `save` the one complete
recovery step (rather than a two-part "resync, then separately remember to
clear the warning") is what the refusal message points at. This converts a
rare, silent, corrupting failure mode into a rare, loud, safe one — the same
"narrow the race, never guess" philosophy the session kill/migrate
preconditions elsewhere in this file already use.

Three details are load-bearing. `rescueSnapshotName` is filtered out by
`snapshotAccountNames` — the rescue file has a snapshot's file-name shape but
nobody is ever logged into it, and without the filter it would surface as an
account in the poller, `account list`, and the picker alike. That also makes it
deliberately un-switchable-to (`switchAccount` only accepts a name the listing
produced): the slot is a manual last resort, restored by hand, not a fourth
account. `withAccountLock`
opens `~/.claude/.account-switch.lock` **per call**: flock locks are per open
file description, so a cached handle would let two goroutines in the same
process both "acquire" it. And `patchIdentityCache` decodes `~/.claude.json`
into a `map[string]json.RawMessage`, overwrites only `oauthAccount`/`userID`
(skipping an explicit `null`, which would strip a good value), and writes via
temp-file-then-rename preserving the original mode — that file belongs to Claude
Code and a live process may be reading it. And `securityFindPassword`
(macOS) only treats a `security find-generic-password` failure as "no such
item" (`os.ErrNotExist`, which `backupOutgoing` reads as "nothing to lose,
proceed without a rescue copy") when BOTH the exit code is 44 AND stderr
contains "could not be found" — exit 44 alone is ambiguous, since it's the
low byte of a Security framework `OSStatus` and other statuses collide with
it mod 256. Any other failure is refused rather than assumed safe: proceeding
without a backup when a credential might actually be sitting there unreadable
is the wrong side of that tradeoff to guess on.

The macOS Keychain legs sit behind the `keychainRead`/`keychainWrite` package
vars purely so tests can never reach the real Keychain: `TestMain` defaults them
to a panic and each test installs a tempdir-backed fake, so a forgotten override
fails closed instead of overwriting the developer's own live credential. Never
add a test that runs the real `security` invocations.

### YAML config

`yaml.go` is a hand-rolled parser for exactly one shape: a top-level `servers:`
key whose value is a list of flat mappings of scalars (`name`, `host`, `port`,
`token`, `ssh_host`, `ssh_user`). No flow style, anchors, nested structures, or
multiline scalars. Don't extend the schema without extending the parser.

### Cross-platform termios

The only OS-conditional code is `termios_bsd.go`
(darwin/freebsd/openbsd/netbsd) and `termios_linux.go`, providing
`ioctlGetTermios` / `ioctlSetTermios` constants. Everything else uses
`golang.org/x/sys/unix` and `golang.org/x/term` cross-platform.

## Releases

Tags matching `v*` trigger `.github/workflows/release.yml`: cross-compiles all
three platform binaries, generates `SHA256SUMS`, and creates a GitHub release
with the binaries attached. Release notes come from the matching `## [vX.Y.Z]`
section in `CHANGELOG.md` — add an entry before tagging or notes will be empty.

```sh
# example for a new release
edit CHANGELOG.md           # add ## [v1.1.0] section at top
git commit -am "v1.1.0"
git tag -a v1.1.0 -m "v1.1.0"
git push origin main v1.1.0
```

## Dependencies

Only `golang.org/x/term` (raw mode) and `golang.org/x/sys` (termios ioctls).
Stdlib for everything else: `net/http` server + client, JSON, threading, file
I/O. Keep it that way — single-binary deployment is the whole point of the rewrite.

## Files at a glance

(See README.md for the full layout table. The biggest files are `render.go`
~1900, `tui.go` ~1000, `resume.go` ~900, `server.go` ~740, `remote_actions.go`
~490.)
