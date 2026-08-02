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

`usage.go` polls Anthropic's OAuth usage endpoint (token from the macOS Keychain
/ `~/.claude/.credentials.json`) every 2 minutes in a background goroutine
(`UsageHub`, following `RemoteHub`'s pattern minus the wake pipe); the TUI header
shows it as two progress bars (5-hour and weekly limits). The token is read-only
— refresh/rotation is Claude Code's job.

`known_accounts.go` polls the *other* accounts each host knows about:
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
never an error: it becomes that entry's `Expired` flag, so
`fetchAllKnownAccounts` always returns a full slice and the poller's
whole-batch backoff only ever fires on a host-level problem, never because one
account is flaky. And only non-`Expired` entries are cached, so a restart can
seed a recently-good account but never resurrects an expired one.

`snapshotAccountNames` lists snapshots with `os.ReadDir` + a name filter, never
`filepath.Glob`: the home directory is data, not a pattern, so a home path
containing `[` would make the *only* error path fire (permanent backoff, every
account gone) and one containing `*` would match into sibling directories and
read their credential snapshots. Nothing below an unresolvable `$HOME` errors —
a missing or unreadable `~/.claude` is an empty list, which is what keeps the
never-fail-the-batch property intact.

Transport is additive: `GET /sessions` gains `knownAccounts` and
`activeSnapshotName` (both `omitempty`, both nil/"" from an older server).
`activeSnapshotName` is a side output of the poll, not a per-request lookup:
`allKnownAccounts` reports the name it skipped as the live account, the poller
parks it beside the slice in `knownAccountsResult`, and the handler reads both
from one `Snapshot()` — so `/sessions` touches no files and the two fields
always describe the same pass. It is never cached to disk (which account is
logged in can change while the process is down).
`dedupeAccounts` merges them in a second pass after the live-account pass — a
live line always wins over any host's snapshot copy of the same email — and an
`Expired` entry keeps its line as a dim `auth expired` placeholder instead of
being dropped. Snapshot-derived lines are never marked `mine`, so one can never
render bare and pass as this machine's live account. Host headings show each
host's active account email, dimmed, after `LOAD` (`section.account`, fed by
that host's existing usage snapshot — no extra fetch, no protocol change).

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
