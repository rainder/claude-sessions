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

**A killed selection lands on its neighbor, not the top of the list.**
`validateTargetSel` (selection.go) takes both the previous and the current
target list: when the selected row is gone, it looks up where that id sat in
the *previous* list and selects whatever now sits at that same index in the
current one (clamped to the new, possibly shorter, length) — the row that
shifted up into the killed row's slot, or the new last row if it was the
last one. This relies on an invariant `settleSelection` maintains by
construction: every settle assigns `s.sel` from the very target list it was
just given (the pending-spawn branch, the neighbor branch, and the
`targets[0]` fallback all return ids sourced from that call's `targets`), so
`s.sel` is always a member of the most recently settled list. That's what
guarantees `prevTargets` — the previous settle's `targets` — still contains
`sel` at the moment a row disappears, even though `settleRows` runs on every
2s tick and both kill paths refresh asynchronously once their background job
lands (see "Worktree cleanup on kill"). Passing `nil` as `prevTargets` skips
the neighbor lookup and falls back to `targets[0]`, the original behavior —
`tui.go`'s `settleRows(neighborFallback bool)` uses that deliberately for the
three view-filter call sites (text filter typing, group digit filter,
hide-disabled toggle): those narrow what's shown by the user's own explicit
action, not by a row actually disappearing, and a "neighbor" computed against
last keystroke's filtered view has no relation to this keystroke's. Only
`refresh()` (the data-changed path every action — kill, disable/group toggle,
wall-clock tick, remote poll — funnels through) passes the real list.

### Grok sessions

`grok.go` lists the xAI Grok CLI's live sessions as first-class rows beside
Claude Code's. `Session.Tool` names the owner: `""` is claude — every session
file Claude Code writes, and every row an older server sends over the wire,
decodes as claude with no migration step — and `toolGrok` marks a grok row.

The two stores look nothing alike. Claude writes one file per pid; grok keeps a
single `~/.grok/active_sessions.json` array (`session_id`, `pid`, `cwd`,
`opened_at`) that it rewrites under its own `active_sessions.lock`. This
collector never takes that lock, so a torn mid-write read is possible and is
treated as **no grok sessions**, never an error — the caller is really trying to
list claude sessions, and a file this tool does not own must not be able to
blank that list. Missing, unreadable and unparseable all collapse to the same
answer for the same reason.

Per-session metadata lives at
`~/.grok/sessions/<encoded-cwd>/<session-id>/summary.json`. The directory name
is `url.PathEscape` of the absolute cwd, verified byte for byte against the
live directories (`/` → `%2F`; `.` and `-` are left alone). That is the fast
path; a miss falls back to scanning the per-cwd directories for one holding the
session id, so the exact encoding is **not** load-bearing.

**`grokSummaryPath`'s step order is a cost decision, not a style one.**
`CollectLocal` runs on a 2s tick and a host can hold thousands of per-cwd
directories, so a scan on every routine miss is thousands of syscalls per tick
— the same failure shape the resume collector's laziness exists to avoid, and
the common miss is entirely ordinary: grok creates the per-cwd directory before
it writes `summary.json` on the first turn. So a miss on the summary checks the
**directory** next: if that exists, the encoding is right and there is nothing
a scan could find, so it answers not-found without listing anything. Only a
per-cwd directory missing *entirely* can mean the encoding changed, and only
then does the scan run. A resolved scan is memoized per cwd (`grokScanned`,
mutex-guarded, keyed by sessions-root plus cwd so two homes never share an
entry), so an encoding change costs one scan per cwd **per process** rather
than one per pass. A scan that finds nothing is deliberately not memoized — the
directory may appear the moment grok's first turn lands, and a negative memo
would hide it for the life of the process; the residual cost is one scan per
pass for a brand-new session under an unknown encoding, until its directory
exists. `grokSessionsReadDir` is the seam that makes "did not scan" testable,
which is the property that matters here rather than the answer being right. The
scan uses `os.ReadDir` and never `filepath.Glob`, for the same reason
`snapshotAccountNames` does: home is data, not a pattern.

NAME prefers `generated_title`, then `session_summary`, and is marked
`nameSource: "derived"` — grok generated it, the user did not type it — so it
renders dimmed like claude's own derived names. A session whose summary has not
been written yet still renders as a row. A row with no parseable `opened_at`
**and** no summary has nothing on disk to date it by, so `grokSessionFrom`
stamps collection time rather than leave `StartedAt` at zero — `Session.Updated`
would otherwise answer the epoch and the AGE column would read ~20679d.

`collectGrokLocal` fills what the registry and summary know, plus a derived
`Status` / `WaitingFor` from the session's `events.jsonl`. `CollectLocal`
still runs its CPU/tmux/Home/GitRoot enrichment over claude and grok rows in
one shared pass. That is what makes attach, preview, send-keys, resize and
kill need no per-tool path at all: they all key off `Session.Tmux`, and
`walkTmuxPane` already handles grok's process shape (the pane pid is a shell
whose child is `grok`) because it walks pid → ppid. Context comes from sibling
`signals.json` (`contextTokensUsed`); missing, unreadable or unparseable is 0
and CTX stays `-`. Color uses `contextWindowTokens` on the same file
(`Session.ContextWindow`); 0 falls back to the Claude 300k constant. Cost is
the incremental sum of `turn_completed` `costUsdTicks` on sibling
`updates.jsonl`, divided by 1e10 (Grok's documented unit). A missing file or
no ticks is 0 / "—". Do not scan the whole file every tick: only new bytes,
same contract as `scanCostIncremental`. Turns without `costUsdTicks` are
skipped (may understate, accepted). `CostSubagentsUSD` stays 0 — parent
`turn_completed.usage` already includes finished subagents. Do not scan
`updates.jsonl` for CTX; CTX stays on `signals.json`.

Grok writes no `status` field. The live signal is `events.jsonl` beside
`summary.json` (`phase_changed`, `turn_started` / `turn_ended`,
`permission_requested` / `permission_resolved`). `grokStatusFromEvents` maps
that log onto Claude's vocabulary so render, sort and `StatusDisplay` need no
grok branch: `waiting` is only an open `ask_user_question` (WaitingFor is
`"input"` — `Waiting()` keys off WaitingFor, and `sessionStatusRank` would
bury a waiting row that only set Status). A tool permission prompt
(`permission_requested` / `permission_prompt`, including
`run_terminal_command`) is `busy`, not waiting — that is a running turn, not
a question to the user. A last event of `turn_ended` is `idle` (grok never
writes an idle phase, so the last `phase_changed` after a finished turn is
still `streaming_text`); an open turn or a busy phase (`waiting_for_model`,
`streaming_reasoning`, `streaming_text`, `tool_execution`,
`permission_prompt`) is `busy`. A missing, unreadable or unparseable log
leaves both fields empty, and STATUS renders as the same `-` placeholder
MODEL/TMUX/SID already use — `statusCellText`, and `statusGlyphFor` for the
minimal view's one-character version, which must agree with it: `?` there
means "this session reports a status this tool does not recognise", which is
the wrong claim about a session that reported none. The file is tailed
(`grokEventsTailSize`, 8KB), not scanned: `phase_changed` fires per token
chunk, and CollectLocal runs on a 2s tick. An open `ask_user_question` older
than the window with later non-question events would read as busy —
accepted, because a session still blocked on the user writes nothing after
the `tool_started`. There is no `shell` status: a long bash tool is still
`busy`. The row carries a dim `grok`
badge inside the NAME cell (`toolBadge`/`nameCell`), which takes part in that
column's **sizing** (`nameCellTextWidth`) — a badge left out of the width math
pushes every column right of it out of alignment.

**A pid claude still holds a session file for wins outright.** Claude Code does
not delete that file when its process exits, so a pid it once used and grok now
owns would otherwise produce two rows whose `Session.ID()` is the same
`"<pid>"` — and selection, kill routing and the TUI's row bookkeeping all
assume that id is unique. `CollectLocal` drops the grok row in that case:
claude's carries a user-set name and a real status, where the grok row for a
recycled pid is the one more likely to be describing something that has moved.

**`CollectLocalLite` is deliberately not extended, but the flag store's prune
resolver is** — and the split is the point, because the two answer different
questions for different consumers. Lite feeds the account-switch warning about
processes still holding the outgoing Anthropic token, and a grok session holds
no Anthropic credential, so listing one there would warn about a process that
cannot possibly clobber the credential being installed.
`resolvableSessionIDs` (session_flags.go) decides what the flag store may
**prune**, and a grok row can be grouped or disabled from the TUI exactly like
a claude one. Built from Lite alone, a grok session id never resolves, so
`mutateLocked`'s prune deleted the group or disable bit the user had just set
on the very next flag write. It now unions in `grokLiveSessionIDs`, which is
deliberately cheaper than `collectGrokLocal` — one registry read plus the
liveness check, no `summary.json` join — because it runs on every flag write
and needs identity only.

**Two liveness seams, and tests must stub both.** `sessionPIDAlive`
(session.go) is the claude collectors' check and `grokPIDAlive` (grok.go) is
grok's; production leaves both as `pidAlive`. A test that stubs only one leaves
the other's rows filtered by the real check, so its outcome silently depends on
whether a made-up pid happens to exist on the machine running `go test` — green
locally, and green for the wrong reason. `allPIDsAlive`/`livePIDs` in
`grok_test.go` stub the pair together.

**Reattestation routes off the row's own tool**, never off the pid: the pid
alone cannot tell the two stores apart, and asking claude's store about a grok
pid reports a healthy session as `not_live`. `localReattestSession`
(migrate.go) picks `localReattest` or `localReattestGrok`; the server's
`reattest(pid, wantSession, tool)` picks the `s.attest` seam or
`grokSessionLookup`, with `tool` taken from the server's own freshly collected
row so a client cannot steer which file is consulted. Both grok legs go through
`grokSessionByPID`, which — unlike `readSessionByPID` — also checks liveness.
That check **narrows** the stale-entry window; it does not close it. It proves
some process holds the pid, never that the process is this grok session, so an
entry left behind by a crash whose pid has since been recycled still attests —
the same accepted residual window a claude kill against a session with no tmux
name already has (see "Known residual windows, accepted"). It is worth having
because claude's session file lingering after exit never claimed anything about
the pid at all, while grok's entry is supposed to vanish with the session, so a
surviving one is already evidence something went wrong. The refusal envelope
and its `session_mismatch`/`not_live` codes are unchanged.
`resolveLivePID`'s own not-live message is deliberately tool-neutral ("is not a
live session"): nothing resolved for that pid, so nothing there knows which
store would have owned it.

**Migrate and snapshot restore stay claude-only.** Migrate means "kill
it and respawn as `claude --resume <id>`", which no grok session can be, so
`MigrateLocalAttested` refuses a pid the registry claims — one place covering
every entry point at once, and costing nothing on the normal path since the
registry is consulted only after the claude session file has already missed.
Four call sites also refuse up front, so the refusal arrives before a
confirmation or a round trip rather than after one: `actAttach`'s migrate
branch, `actAttachRemote`'s (the server can only refuse it too), `cmdMigrate`,
and `cmdAttach`'s not-in-tmux branch — which used to point the user at `run:
claude-sessions migrate <pid>`, a command that can only refuse. Two subtler
paths matter for the same reason: `finishKillJob`'s resurrect offer is skipped
for a grok row (it calls `ResumeSessionInWorktree`, i.e. `claude --resume`),
and `saveSnapshotFrom` skips grok rows outright, since a snapshot is restored
by resuming each entry's claude transcript and a grok session has none.

`lookupLiveSessionByPID` is how the scriptable subcommands cope with a bare pid
that names no tool: claude's file first, grok's registry second. It is what
lets `kill PID` work on a grok session — with the same reattestation — and what
lets `migrate PID` and `attach PID` name the right refusal.

**Claude wins that race only while its own claim is still plausible.**
`readSessionByPID` checks nothing about liveness — Claude Code leaves the file
behind when the process exits — so a pid it once used and grok now owns would
resolve to the dead claude session, and `kill PID` would act on the wrong row
entirely. So when the claude file is present but the pid is **not** alive and
grok's registry holds a live session there, grok's row is the truthful one and
wins. `MigrateLocalAttested` carries the same guard in its own shape: it
refuses rather than adopting the stale file, because migrating it would SIGTERM
the live grok process and then resume a stranger's transcript in its place.
Both guards are deliberately narrow — a dead claude pid with **no** grok
session at that pid still resolves and still migrates, which is the legitimate
"resume a session whose process died" path.

Messages: `resolveLivePID`, `resolveLivePIDLocal` and the bare-pid subcommands
all report "is not a live session" rather than "is not a live Claude session",
since at that point nothing resolved and nothing knows which store would have
owned the pid. `localReattest` keeps the word "Claude" — it re-read claude's
own session file, so it knows exactly what it did not find.

### Resume picker

`resume.go` owns the whole `r`-key feature: collect past Claude transcripts
(`~/.claude/projects/*/*.jsonl`; session id = filename stem) plus finished
Grok sessions (`~/.grok/sessions/**/summary.json`), a searchable picker
(reuses `filterNewPickerLines`), and the spawn primitives both the local TUI
path and `POST /sessions/resume` call (409 when the session is already live).
Claude rows go through `ResumeSession` (`claude --resume <id>`). Grok rows
go through `ResumeGrokSession` (`grok --resume <id>` in the session's cwd —
no `--cwd`, no `--worktree`, no `--restore-code`). Empty `tool` on the wire
is Claude, so old clients and servers stay compatible. A non-empty unknown
`tool` is `400`. Remote lists come from `GET /resumable`, fetched concurrently
per host and merged newest-first. An old client talking to a new server can
still see Grok rows as Claude — the reverse mismatch this repo's deploy
order (client first) does not handle.

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
(`resumeSessionIDRe`) before touching the filesystem or tmux
(`validateResumeSessionID` in `ResumeSession` / `ResumeGrokSession`, not
only later in `resumeCommon`).

Grok rows come from walking `~/.grok/sessions/<encoded-cwd>/<session-id>/summary.json`
— never `grok sessions list`, never `session_search.sqlite`. The same 30-day
window, 100-row cap, scratch-cwd filter and live-id exclusion apply after a
merge sorted by recency across both tools. Live exclusion unions
`grokLiveSessionIDs` into `liveSessionIDs`: `CollectLocal` drops a grok row
when a Claude file still claims that pid, and without the union the picker
would offer a still-running grok session. A torn or unreadable registry is
unknown live-set, so that pass lists no grok rows and `ResumeGrokSession`
refuses — unlike `collectGrokLocal`, which treats a torn read as no live grok
rows and must not blank the Claude list. `session_kind=subagent` is skipped;
a missing or empty kind is kept, including forks that carry a
`parent_session_id`. NAME is `grokSummaryName` (generated_title, then
session_summary). A dim `grok` badge rides inside NAME and takes part in that
column's width math (`toolBadge` / `nameCellTextWidth` / `fitNameForBadge` /
`nameCell`). Prompts stay empty — the → overlay is claude-only. A missing
`.grok` tree is not an error: the Claude list is exactly what it was before.
`collectResumableLimited` stays Claude-only (the lazy two-pass + cache
below). `collectResumableFrom` asks it and `collectGrokResumable` separately,
then merges.

**The collector is lazy, and its two passes are split by cost.**
`collectResumableLimited` (which `collectResumableFromLimited` still calls with
the same cap) first stats every match and applies only what a name and a
mtime can decide — dirs, zero-byte files, the `resumableMaxAge` cutoff, live
session ids — producing candidates sorted mtime-desc, ties broken by path
string comparison (not `Glob`'s own order — a worktree's project dir is a
suffixed variant of its parent's, e.g. `-repo` vs `-repo--claude-worktrees-x`,
and `Glob` sorts those per path component while a raw string compare orders
them by byte value, so the two orderings can disagree on which of two
identical-mtime candidates comes first; harmless here, since both candidates
carry the same session id and mtime either way). Only then does it walk that
list newest-first calling `readResumableHeadFn` + `countFileLines`, and it
**stops the moment `limit` entries are collected**. It used to scan everything
and sort at the end, which cost one head scan and one full-file line count per
transcript inside the window regardless of the cap — measured on a
3348-transcript corpus, 3348 head scans and 608 line counts (2.58s); the lazy
version cut that to 686 and 100 (0.39s), a ratio that tracks how many recent
transcripts get rejected by content filters, not a fixed "~100 files". On the
5363-transcript host that motivated this fix it took 5.8–6s, past
`fetchRemoteResumable`'s 5s client timeout, so the picker rendered a healthy
host as unreachable. Because pass 2 emits in the order it walks, its output is
already mtime-desc and already capped; the sort-then-truncate this replaced was
redundant, not load-bearing.

Two details are load-bearing. Dedupe is deliberately **not** resolved in the
cheap pass, even though mtime is all it needs: doing so would drop a session id
whose newest transcript fails a *content* filter, where the loop this replaced
fell back to an older valid copy. The rule is "newest **content-valid**
transcript per id", so a rejected candidate leaves its id unmarked in `emitted`
and the next-newest copy still gets its turn
(`TestCollectResumableFallsBackToOlderValidTranscript` pins it). And
`readResumableHeadFn` is a package-var seam whose default is the **real**
function, not a `TestMain` panic like `keychainRead`/`usageInfoFetch` — it only
reads a fixture file, so nothing needs to be stopped from reaching it; the
seam exists so `TestCollectResumableStopsScanningAtTheLimit` can count *how
often* it is reached, which is the only way the laziness is observable.
`collectResumableLimited` takes the cap as a parameter for the same reason —
that test asserts against a small limit without weakening the production
constant.

**The two expensive reads are memoized on disk** (`resume_cache.go`,
`~/.claude/.resumable-cache.json`, path derived from the same `home` parameter
the collector globs under — which is what keeps every collector test cold and
hermetic in its own temp home). Pass 2 still stops at the cap; a warm open just
re-reads no transcript contents at all. Measured on a 3356-transcript corpus:
cold 692 head scans / 361ms, warm 0 / 17ms, 398KB cache file. The key is
**(path, mtime, size)**, never the session id — ids are not unique per file (a
worktree move leaves two transcripts under one id) and the rule is "newest
content-valid transcript per id", so an id-keyed entry would let one file's
cached answer stand in for another's; a path key carrying the file's own mtime
and size makes every edit, truncation or replacement a miss by construction, so
there is no invalidation logic to get wrong. What is cached is
`readResumableHead`'s answer, which is also what every *content* rejection is
derived from (no cwd, scratch cwd, agent transcript), so a rejected file
short-circuits exactly like an accepted one — and that is where most of the win
is, since of the 692 files pass 2 opened only 100 became rows. A candidate
skipped by `emitted[sid]` is never read and so never cached, which is what keeps
dedupe purely per-pass state and the fall-back-to-an-older-valid-copy rule
untouched. `Lines: -1` means "count not computed": a file whose head was read
but which was then rejected never had `countFileLines` run, and a later pass
that does accept it (its newer duplicate having gone) fills the number in
without invalidating the head beside it.

The cache sits **in front of** `readResumableHeadFn`, not behind it, so that
seam keeps counting transcript reads that genuinely happened — the same
quantity before and after the cache existed.
`TestCollectResumableStopsScanningAtTheLimit` is therefore unchanged and still
proves laziness against a cold cache, and the cache's own tests prove their
half with the identical counter reading zero warm. Behind the seam it would
have counted cache *lookups*, a number that says nothing about work avoided and
that would let a cache bug hide inside a still-green laziness test. There is
deliberately no second seam for `countFileLines`: the round-trip test replaces a
transcript's contents while restoring its mtime and size, so a re-read would
change the answer — which proves the memo harder than a counter would, and for
both reads at once.

The size bound is the prune on every write: an entry goes if its path was not
in this pass's glob (deleted or moved — `seen` is recorded *before* the cheap
pass's filters, since a zero-byte or currently-live transcript is still a file
whose memo must survive) or if its own mtime has fallen past `resumableMaxAge`.
So the file can only ever hold transcripts inside the window that some pass
actually read. The prune runs even on a pass that collects nothing, so a host
whose whole corpus ages out does not keep its entries for as long as the picker
goes unused. Nothing is written at all when a pass neither read a file nor
pruned one. Writes are temp-file-then-rename in the same directory
(`patchIdentityCache`'s pattern) with **no lock**: the TUI and the `/resumable`
handler each run their own pass, and the loser of a rename race simply loses its
new entries — one more scan of those files on some later pass, which is what
"best-effort memo, not a source of truth" is worth.

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
survives and git's refusal is what the user sees — in `actKill` that refusal
now offers a recovery step, below. The branch is never deleted.

**Both TUI kill paths run in the background, not inline in `actKill`/
`actKillRemote`** (`kill_async.go`). Locally, `KillSession` can block up to 5s
waiting for a tmux pane's process to actually die after `tmux kill-session`
returns (`killSessionWith`, migrate.go); remotely, `killRemote` and
`removeRemoteWorktree` are each a multi-second HTTP round trip. Blocking the
whole TUI in cooked mode on either — as both paths used to — was the "killing
dialog" hang this replaced. Once the overlay confirms, `actKill` resolves the
worktree target (still synchronous — the survivors check needs the pre-kill
session list) then hands `localReattest` + `KillSession` + any
`RemoveWorktree` to `startLocalKillJob`; `actKillRemote` hands `killRemote` +
any `removeRemoteWorktree` to `startRemoteKillJob`. Both are thin wrappers
around the shared `startKillJob(s, run)`, which runs `run` in a goroutine and
returns a `*killJob` immediately — the TUI stays fully interactive with a
"killing …" toast in place of the old blocking prompt. `startKillJob` follows
`previewPane`'s wake-pipe pattern so the main loop's `select` wakes the
instant the job lands, but `finishDoneKillJobs` (tui.go) also checks every
poll tick unconditionally — via the pure `partitionKillJobs` (kill_async.go)
— so a job whose pipe failed to create still gets picked up rather than
hanging forever silently. A second Ctrl+X on the same row while its job is
still in flight is refused (`actCtx.killInFlight`, backed by `killJobs` in
tui.go, keyed on host+PID so it covers both paths): the row stays in the list
until the job lands, so without the guard a duplicate confirm would race a
still-live `localReattest`/`killRemote` and re-attempt worktree removal the
first job already handled. `killInFlight` only covers a job that is still
*running*, not the gap between one landing and the session-list refresh
catching up — a row can still show a session whose kill already succeeded.
`startRemoteKillJob` treats that gap as a no-op rather than a failure: a
second remote kill against an already-gone session answers `not_live` or
`session_mismatch` (`codeNotLive`/`codeSessionMismatch`, server.go — "your row
is stale, refresh", never "retry"), and the job reports success instead of
surfacing a `kill failed` toast over a kill that actually worked. Local kill
has no equivalent path: `localReattest`'s own refusal on an already-gone PID
is a genuine failure to report, since there is no server-side second kill to
have already succeeded.

`finishKillJob`'s worktree-remove failure offers to resurrect **only for a
local kill**: `git worktree remove` refuses on a dirty/untracked tree, and the
alternative to leaving the user stranded is resuming the session that was
just killed, right back into the same still-on-disk worktree checkout. This
step — unlike the kill itself — runs synchronously on the main loop once the
background job's result lands, because it owns the raw/cooked terminal
handoff a background goroutine can't safely perform (see "Single stdin
consumer" above). A `y` answer calls `ResumeSessionInWorktree(s.SessionID,
repoRoot, name)` — the same primitive the `r` resume picker uses for a
worktree session, which works whether or not the checkout survived, since
`--worktree <name>` recreates it if needed. On success the new tmux name is
stashed on `c.spawnedTmux` (the same field `actResume` and `actNew` use) and
`runTmuxAttach` takes over the terminal immediately, mirroring `actResume`'s
own resume→attach sequence rather than leaving the user back at the session
list. A `N` answer, or a failed resume, falls through to the existing
`pauseForKey` pause. A **remote** kill's worktree-remove failure has no such
prompt — resurrecting needs local git state `finishKillJob` has no access to
over HTTP — so it just reports the failure straight to the toast, matching
what `actKillRemote` always did on this path, just non-blocking now.
`cmdKill` still blocks synchronously and reports the failure inline — a CLI
invocation was never going to be instant anyway, so there is no dialog to
unblock.

Three entry points: `actKill`/`finishKillJob` (local TUI — the kill itself is
confirmed up front, but a worktree left idle by it is removed outright once
the background job lands, no second confirm), `actKillRemote`/`finishKillJob`
(remote TUI, same no-second-confirm rule and now also backgrounded, but no
resurrect prompt — see above), `cmdKill` (`--remove-worktree`, or a prompt
when interactive; a bare `-y` run keeps the worktree), and the server, which
puts a `worktree` object in the kill response and removes via authed
`POST /worktree/remove`. That endpoint takes a client-supplied path, so
`validateWorktreePath` gates it (absolute, clean, worktree root, real
worktree) and the handler re-checks the live session list.

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

`POST /sessions/new` and `new --group N` also accept a **group** (1..9) for the
session about to exist, so a spawned child can inherit its parent's badge (the
`/spawn` skill walks its own ppid chain against `list-sessions --local --json`
to find it). The range is the store's own; **0 is refused** rather than read as
"ungrouped" — there is nothing to clear on a session that did not exist a
moment ago — which is why the server decodes it as a `*int` (absent is an older
client, present is checked) and answers `400 bad group`. The whole thing is
best effort *after* the spawn, never a precondition of it: `spawn_group.go`
polls `CollectLocal` for the session whose tmux pane sits in the name
`SpawnNew` returned, then calls `FlagsStore.SetGroup`. The wait exists because
the flags store is keyed by session id, which does not exist until the new
claude process writes its own session file — seconds later, longer behind a
first-run trust dialog. It ends early rather than spending
`spawnGroupTimeout` (15s) when `tmux has-session` says the session is gone: a
command that died on its first line is never going to produce an id.

**The server does that in the background; the CLI waits.** `groupSpawned`
starts the assignment and returns immediately, because it runs inside the
`request_id` single-flight: waiting would push the response toward
`remoteRequest`'s 30s ceiling and hold the dedupe slot long past the point the
spawn's outcome is known — and a client that gives up retries with a *fresh*
request_id, so a response held too long is not a slow spawn, it is a second
one. The cost is that a background failure has no response left to ride: it
goes to the host's own stderr, and a remote caller simply never learns the
badge did not land. Only a failure knowable with nothing to wait for — this
host has no flags store at all — still comes back in
`actionResult.Warnings` (the shape `accountSwitchResult.Warnings` already
established). `cmdNewLocal` keeps waiting: stdout already carries the tmux
name, the user is right there, and a warning on stderr is worth the seconds.
Nothing on either path may turn a session that was created into a failure the
caller might retry. `s.setGroup` (the assignment) and `s.groupGo` (what runs
it — a goroutine in production, inline in tests) are the seams beside
`s.spawn`; `spawnGroupCollect`/`spawnGroupTmuxAlive`/`spawnGroupPollInterval`/
`spawnGroupTimeout` are the package-level ones, so no test spends the real
wait or shells out to tmux. A replayed `request_id` replays the stored
envelope and never re-enters `spawnSession`, so one spawn is one assignment.

`POST /sessions/{pid}/send-keys` (`{"session_id", "text"}`, send_keys.go) is
the inspector's `i`-to-compose feature: it types `text` into `pid`'s tmux pane
as literal keystrokes plus Enter. Unlike kill/migrate's legacy optional
precondition, `session_id` is **required** here — there is no pre-existing
caller to stay compatible with, so there's no reason to allow an unguarded
send. It resolves its target with a single fresh `resolveLivePID` check
immediately before sending, not kill's extra late `reattest`: the design
reasoning (refined during final review) is that a send's worst case is one
line of text landing in a recycled pane, not a destroyed session, which
doesn't justify a second `CollectLocal()` walk. The local TUI path mirrors
this with `resolveLivePIDLocal` (send_keys.go), a fresh `CollectLocal()` plus
the same `SessionID` check, since `Session.Tmux` is derived at collection
time and never stored on disk — a plain `session_id`-only reattestation like
`localReattest` would revalidate identity but still hand back a stale pane
address.

`POST /sessions/{pid}/resize` (`{"session_id", "cols", "rows", "revert"}`,
resize.go/server.go) is the inspector's preview-resize feature: entering
preview resizes the previewed session's tmux **window** to the inspector's
inner viewport, and leaving preview un-pins it again. `session_id` is
**required** for the same reason send-keys requires it — a new endpoint with
no legacy caller to stay compatible with, so there is no reason to allow an
unguarded one — and `resizeHandler` resolves its target with the same single
fresh `resolveLivePID` immediately before acting, with the same reasoning: a
resize's worst case is one recycled pane briefly changing size, not a
destroyed session, which doesn't justify kill's second `CollectLocal()` walk.
The revert is `tmux set-window-option -u window-size`, **not** `resize-window
-A`. That distinction is the whole reason this endpoint has a `revert` flag
rather than just resizing back: `-A` recalculates the size once but leaves
`window-size` explicitly set to `manual` (verified directly against a live
tmux session — `show-window-options` still reports `manual` afterwards), so a
window "reverted" that way never auto-adjusts to an attaching client again
and stays silently frozen at the last preview's dimensions. `-u` (unset) is
the only thing that actually clears the override. Every call site is
best-effort: errors are discarded, never logged, since preview renders via
`capture-pane` at any pane size — nothing about a failed resize should block
entering or leaving preview.

Reverting is the direction that matters, because a missed revert is silent
and permanent, so `RunTUI` reverts from a top-level `defer` too (quitting
outright never reaches `closeInspector`). One gap survives that, and it is
**local-only closed**: both paths resolve the pane fresh, so a session that
*exits during preview* no longer resolves and the revert finds nothing to act
on. Locally `resizeInspected` (tui.go) then falls back to `revertTmuxTarget`
against the inspector snapshot's own `sess.Tmux` — a last-known-good address
whose worst case is un-pinning a window that has already gone or been reused,
which is why the same fallback is right here and wrong for send-keys (that
one types text) and wrong for the **entry** resize (pinning a stranger's
window, with no stuck state to recover in the first place). The remote path
cannot do this: `resizeHandler` resolves the pane itself and takes no
client-supplied pane address, so a remote session ending mid-preview can
still leave its window pinned to `manual` — accepted, since closing it means
trusting a client-supplied pane address on the wire, and it only bites a
hand-managed window that outlives its claude process (a window this tool
spawned dies with the session).

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
limits with that snapshot's own token, **one account at a time** (inline in
`newKnownAccountsFetcher`'s loop) rather than fanned out — these are
typically several different Anthropic accounts, each spending its own
budget, but they are all requests from the same process to the same endpoint
in the same moment, and going one at a time spreads that out instead of
bursting it. Strictly read-only — nothing is ever swapped into the Keychain
or the live credential file. Three rules hold. The **live** account is never
read from its own snapshot (Claude Code rotates the live token in place
while the snapshot keeps whatever was stashed at switch time, so the
snapshot can look expired while the session is healthy) — it is skipped by
email equality against `loadAccountEmail`. A per-account failure is never an
error: it becomes that entry's own classification, so the walk always
produces a full slice and the poller's whole-batch backoff only ever fires
on a host-level problem, never because one account is flaky. And only
entries with numbers are cached, so a restart can seed a recently-good
account but never resurrects an expired one.

**Identity is bound to the token, not to a file.** Every usage fetch is paired
with one profile fetch on the *same* token (`fetchProfileEmail`,
`/api/oauth/profile` → `account.email`; the usage response carries no identity
of its own), and the answer rides back on `UsageInfo.VerifiedAccount` —
`json:"-"`, since each host verifies where its own tokens live and a second
writer on the wire would just make the winner depend on poll ordering.
`usageAccountLabel` is the one attribution rule (verified beats the file read)
and `verifiedIdentityMismatch` the one disagreement test; the live path and the
known-accounts poller both go through both, so they cannot drift from each
other. This closed a read-order race — the token was read at one instant and
`~/.claude.json` at another, so a switch in between labelled one account's
numbers with another's — and it is what makes a *clobber* visible: a header
showing an unexpected address means the credential store and the identity cache
have drifted apart. A verified email that disagrees with the file **wins**;
smoothing it over is what hid the problem.
`/usage` no longer takes part: it never fetches, so it has no token to verify
against and always reports the file's email (`loadAccountEmail`). A clobber on
a *remote* host is therefore invisible through `/usage` — visible only from
that host's own client, the same one place that can still afford to spend the
token checking it.

`usageInfoFetch`'s production value is therefore `fetchVerifiedUsageInfo`, not
the bare `fetchUsageInfo` — one seam covers both legs, so a test fake with a
zero `VerifiedAccount` reproduces the pre-verification behaviour exactly.
`profileEmailFetch` is the probe's own seam (`TestMain` panics on it, like
`usageInfoFetch`). Three rules keep the cost down and the failure modes benign.
The probe runs **only after a successful usage fetch** — a throttled or dead
account already has its answer, and asking twice would double the cost of the
very failure the backoff exists to stop paying for. A probe failure is **never**
an error: identity verification is an upgrade over a file read, so every caller
falls back to the file. And the answer is **cached per token** (`profileEmails`,
keyed by sha256 of the access token, ~32 entries, successes only), because it is
a property of the credential — a rotated token is a different key and re-probes
naturally, so no TTL is needed to stay correct, and steady-state request volume
is unchanged.

**The two legs are sequential**: 5s for usage + `profileFetchTimeout` (2s) ≈ 7s
worst case. This budget belongs to the client-side fetchers only
(`newUsageFetcher`, `newKnownAccountsFetcher`) — the server's `/usage` handler
no longer calls either leg (see below), so there is no longer a server-side
timeout wrapper in this chain to check it against.

**A failed fetch has six outcomes, not two.** `Expired` used to mean "the last
fetch failed", which is how a 429 came to render as `auth expired` and send the
user off to re-login over a throttle — and the endpoint 429s constantly, since
every Claude Code session shares the account's per-token budget. So
`classifyUsageErr` (usage.go) splits the failure on the typed `usageHTTPError`
that `fetchUsageInfo` now returns: **401/403 alone** is `Expired` (the only
actionable state — that credential really is dead), while 429 → `rate limited`,
5xx → `server error`, a client-side deadline (`net.Error.Timeout()`) → `timed
out`, and anything else (network, parse) → `unreachable`. A credential
snapshot that cannot be read or
parsed at all never reaches `classifyUsageErr` — there was no answer to
classify — and carries its own `bad snapshot` tag (`usageBadSnapshotReason`,
known_accounts.go) rather than falling in with `unreachable`: nothing about it
is a network problem, and unlike every tag above it does not heal on its own.
The fifth is `wrong identity` (`usageWrongIdentityReason`): the token answered
fine, for a *different* account than the snapshot's `.<name>.account.json`
claims. It is deliberately not an error at all and has no `classifyUsageErr`
branch — the request succeeded; the two call sites that compare emails set the
tag on the entry directly. Never `Expired` (the credential works — it is the
name on it that is wrong), and its own tag rather than `bad snapshot` (the file
is perfectly readable); the fix is the same `account save <name>`. `Reason`
is a short fixed tag and never the underlying error's text: a `*url.Error`
stringifies with the whole request URL, which would land in a one-line header
placeholder and in the `/usage` JSON.

The sixth is `needs refresh` (`usageNeedsRefreshReason`, known_accounts.go): a
parked snapshot's *access* token has aged out while its *refresh* token is
still good. `knownAccountUsage` catches this before spending a request —
`snapshotAccessTokenExpiresAt` reads the same credential's `expiresAt` field
`validateSnapshotCredential` (account.go) also reads — there for a different
reason (an expired access token there just skips the profile-identity probe;
it is never itself a reason to refuse a switch, only an expired *refresh*
token is) — applied here as an actual gate on the polling path instead. It
shares `bad snapshot`'s shape (set
directly, no `classifyUsageErr` branch, since no request was made) and is
deliberately neither of the two tags it would otherwise resemble: not `Expired`
(the refresh token is fine, no re-login needed) and not `rate limited` (which
is what this state rendered as before this tag existed — confirmed live
against the real endpoint, an expired access token gets **429**, not 401, from
`/api/oauth/usage` specifically, even though the same token gets a correct 401
from `/api/oauth/profile`; the usage endpoint's own quirk, not a real
throttle). The fix is the same `account save <name>` while logged into that
account. Like every other non-`Expired` reason, a carryable `prev` still gets
served with a `Stale` marker ahead of this tag taking effect, so a
recently-healthy account that goes into this state keeps showing its last
numbers with a growing staleness age rather than the `needs refresh` text
itself — consistent with how `bad snapshot` already behaves, and with this
section's own "old numbers beside a visible Stale marker still beat no numbers
at all" reasoning above; the age is the discoverability signal, not the tag.

A transient failure then **carries the last good numbers forward** rather than
blanking the account — in `KnownAccountsHub`, the client-side poller, which is
the only thing anywhere with memory of a previous pass:
`knownAccountUsage` takes a `prev` and re-serves its
`Info` with `Stale: true`, the failure's `Reason`, and `prev`'s **original**
`FetchedAt` — restamping it would let an account that fails forever look
forever fresh, when that timestamp is the whole reason the header can tell a
carried reading from a live one (`carryable`, which is deliberately not
age-bounded — see below). An `Expired` entry carries nothing forward:
bars beside a dead credential imply it still works. The four states are
`Info` fresh / `Info` + `Stale` / `Expired` / neither-with-a-`Reason`, and
`KnownAccountUsage` documents them.

**The `wrong identity` path carries only a `Verified` reading**, and that extra
condition is load-bearing rather than belt-and-braces. `carryable` compares the
*claimed* email on both sides, which is exactly what is in doubt here: a pass
whose probe was unavailable stores the stranger's numbers under the claimed
email (nothing contradicted the claim, so the entry looks ordinary), and the
next pass — the one that does reach the probe and does detect the mismatch —
would carry precisely those numbers forward, smuggling in what dropping the
fresh reading exists to prevent. So `KnownAccountUsage.Verified` records that
`Info`'s identity was *confirmed*, travels with the numbers through a carry
(one throttled pass in between must not re-open the hole), is serialized so a
warm start keeps the distinction, and gates this one path only. Every other
failure keeps today's carry semantics regardless: a 429 or a network blip says
nothing about whose numbers `prev` holds.

**Fetch order is the one thing the closure still carries in memory**, and it
exists because going one account at a time means WHICH one goes first
matters — fanned out, every account got fetched every pass regardless of
order, but a chronically struggling account fetched sequentially would
otherwise always land wherever `snapshotAccountNames`' directory listing puts
it, typically last. Any account this pass could not get fresh numbers for —
`Expired`, or any `Reason` at all, which covers both a genuine failure and a
synthesized backoff answer — moves to the front for the next pass
(`reorderFailedFirst`), ahead of the accounts that were fine.
`reconcileFetchOrder` runs first each pass to fold in additions and removals:
a name the order slice still remembers but that `snapshotAccountNames` no
longer lists (renamed or deleted) drops out, and a name newly listed but not
yet in the order (a snapshot saved since the last pass) is appended at the
end. Order is read and written only inside the serial closure
`usagePoller.run` calls one tick at a time.

Numbers and backoff state are **not** otherwise held in memory across passes
any more — see "One cache entry per account" below. `newKnownAccountsFetcher`
takes no seed parameter at all: each pass reads every account's own file
fresh (`loadAccountCache`), so a renamed or deleted snapshot simply drops out
of `snapshotAccountNames()` and is never consulted again, with nothing to
separately rebuild or prune. `usageInfoFetch` is the package-var seam that
lets tests drive the poller without a network (same pattern as
`keychainRead`/`keychainWrite`).

There used to be a gap here worth documenting: an account only a **remote**
host held a snapshot for had nothing anywhere remembering its last good
numbers, so a 429 there rendered as a bare `rate limited` placeholder with no
bars, and closing it would have meant growing a second carry-forward layer on
a machine other than the one whose header the numbers land in. That gap is
moot now — see "The `/usage` endpoint" below: a remote host no longer fetches
numbers for anyone, so there is nothing left to carry forward on its behalf.

**Both hubs run in the TUI client only, and only against Anthropic directly.**
A `claude-sessions -s` server used to start them too, so every host polled
Anthropic every 2 minutes for the life of the process whether or not a client
had ever connected — and hosts sharing an account each paid separately for
numbers the client's own `dedupeAccounts` would then throw away, deepening
exactly the 429s that surface as "auth expired" flicker in the header. The
server's `GET /usage` used to fetch on demand instead of polling forever, which
fixed the always-on cost but not the multiplication: one client machine polling
every configured remote still meant every host holding a snapshot of a shared
account spent that account's budget on the same tick. `/usage` now never calls
Anthropic at all (see below) — the only thing that ever spends an account's
budget is the one machine actually running a client against it, whether that's
this host's own TUI/CLI (`UsageHub`/`KnownAccountsHub`) or a client SSHed
into it and run there directly. The Codex side (`codex_usage.go`) still polls
on the server and still rides `/sessions`; only the Anthropic half changed.

`snapshotAccountNames` lists snapshots with `os.ReadDir` + a name filter, never
`filepath.Glob`: the home directory is data, not a pattern, so a home path
containing `[` would make the *only* error path fire (permanent backoff, every
account gone) and one containing `*` would match into sibling directories and
read their credential snapshots. Nothing below an unresolvable `$HOME` errors —
a missing or unreadable `~/.claude` is an empty list, which is what keeps the
never-fail-the-batch property intact.

**The `/usage` endpoint.** `GET /usage` (server.go, beside `sessions`) answers
`usageResponse`: `usage` (the live account), `knownAccounts`, and
`activeSnapshotName`. It reports **identity only and never calls Anthropic** —
no live-account fetch, no per-known-account fetch, nothing. The whole account
universe — names from `snapshotAccountNames`, emails from
`snapshotAccountEmail`, and which snapshot matches `loadAccountEmail` — is
resolved from disk on **every** call, mirroring `newKnownAccountsFetcher`'s own
skip rule (every snapshot standing for the live account is left out of `knownAccounts`;
the first one names `activeSnapshotName`). Being pure disk reads is what keeps
it always current: a picker opened right after a remote account switch is never
stale, with no cache and no kick-on-switch machinery needed to keep it that way.
Every `Info` field in the response is always `nil`.

This is deliberately narrower than the endpoint used to be. It originally also
fetched each account's percentages on the caller's behalf — single-flighted and
briefly cached per email (`usage_cache.go`, since removed) — which solved the
always-on-poller problem above but not the multiplication one: the same
Anthropic account is routinely held as a credential snapshot on two or more
machines, and every one of them asking a remote host to check that account's
numbers spent that account's shared per-token budget again, on top of whatever
the machine actually using it was already spending locally. A client machine
polling several configured remotes could rack up several times the request
volume of the account it was actually trying to read a number for. The fix is
to stop a remote host from ever spending Anthropic budget on another machine's
behalf: only a client running *against* an account — this host's own
`UsageHub`/`KnownAccountsHub`, or a client SSHed onto the host holding that
account's live login or snapshot and run there directly — ever fetches its
numbers. Every other machine sees that account only as a name and an email, for
the Ctrl+W switch picker and `account list` (`accountRowsFrom`,
account_list.go, builds both straight out of `knownAccounts` and needs nothing
else). The consequence in the header: `dedupeAccounts`'s `add`/`addKnown` drop
any line with `Info == nil` and nothing else to show, so a remote host's
account bars simply don't appear — only its heading label does
(`section.account`, fed by `Usage.Account`, which is still populated for free).
That is the intended shape, not a regression: the header now shows bars only
for accounts an actual poller somewhere is spending against.

**A mixed-version fleet has a transient window.** `RemoteUsageHub` still polls
every configured remote's `/usage` regardless of that host's version. A remote
still running the old handler answers with real numbers, spending its
account's budget on this client's behalf exactly as before — the multiplying
case this change exists to close. That window closes itself once the remote is
upgraded, the same "the reverse mismatch needs no handling, since this repo's
deploy order always upgrades the local machine first" precedent used elsewhere
in this file; there is deliberately no version negotiation added to close it
sooner.

**Consecutive-429 backoff.** One Anthropic account is routinely held as a
credential snapshot on *two* hosts, and each fetches it on its own
`usageRefreshInterval` tick — `localFreshAccountEmails`' `ignore` list only
suppresses the remote's copy once the local fetch is already **fresh**, which is
exactly what a chronically throttled account never becomes. So both sides kept
paying a failed round trip every 2 minutes, forever, against the endpoint doing
the throttling. Every fetch path now counts consecutive `rate limited` outcomes
per account and skips the fetch outright while a wait is armed: the first 429
imposes no wait of its own (most heal within one tick — only an explicit
`Retry-After` can delay it), the second waits `usageBackoffSecond` (4min), the
third and beyond `usageBackoffMax` (8min, the cap). A 429's `Retry-After`
(`parseRetryAfter`, either RFC 9110 form) may only *lengthen* that wait, bounded
by `usageBackoffCeiling` (1h — raised from 15min after a live 429 was observed
asking for ~31.6min, Retry-After: 1895, which the old ceiling silently
truncated) so a bogus header still can't park an account for the life of the
process. On a pass that actually attempts the account, any outcome
other than another rate-limited failure — numbers, `Expired`, a different
failure, or the snapshot turning out to be the live account — clears the streak
(`usageBackoffUntil` / `usageBackoff` in usage.go hold the shared arithmetic;
each scheduler holds its own state). A *skipped* pass never touches the
streak either way, even if the synthesized path's own local reads (e.g.
`snapshotToken`) would have reported something other than a throttle, like a
now-unreadable credential — the already-armed wait simply stands, and
self-corrects at the next real attempt once it elapses.

**One cache entry per account, not one file per role.** `account_cache.go`'s
`accountCacheEntry` holds numbers AND backoff state together — `Account`,
`FetchedAt`, `Info`, `Stale`, `Verified`, `BackoffStreak`,
`BackoffNextAttempt` — one JSON file per account
(`claude-sessions-account-<uid>-<name>.json`, `accountCachePath`), read and
written by `loadAccountCache`/`saveAccountCache`. Before this type existed,
the live account and every known account were cached across four separate
files split by ROLE (a live-numbers file, a live-backoff file, a
known-accounts-numbers file holding every non-live snapshot in one array, and
a known-accounts-backoff file mirroring it) — which meant switching which
account was live lost continuity in **both** directions: the account you
switched TO started its numbers over on the live side (its known-account
history lived in a different file the live path never read), and the account
you switched AWAY FROM lost its backoff streak the instant it reappeared on
the known side (same problem, other direction). One file per account, keyed
by identity rather than by which role is currently asking, makes a switch a
non-event for both: whichever side asks next just finds what was already
there.

The key is the claude-switch **snapshot name**, never the email — an account
whose `.<name>.account.json` is unreadable (email `""`) still needs a stable
slot to persist real backoff state into, or it gets fetched (and likely
re-throttled) on every single pass; a name survives that where an email
can't. `resolveActiveSnapshotName(liveEmail)` (account_cache.go) is the one
place the live account's cache resolves "which per-account slot am I" —
`snapshotAccountNames()` plus an `emailMatchesLive` scan, first match wins,
`""` when the live email is unknown or matches no snapshot. A live account
with no snapshot at all has nowhere to persist anything and degrades to a
plain in-memory-only fetch every pass, which is what every account got
before this file existed — no regression for that case, just no cross-restart
or cross-switch memory either, since there is no slot to hold it.

**Both fetchers are now fully stateless between passes** — no more in-memory
`last`/`backoff`/`armedFor` maps carried inside the closure, the single
biggest simplification this type enabled. `newUsageFetcher` (usage.go) takes
no seed of any kind; every call resolves the live account's name fresh
(`resolveActiveSnapshotName`) and loads that slot fresh (`loadAccountCache`)
before deciding whether to carry, placeholder, or fetch. `newKnownAccountsFetcher`
(known_accounts.go) already walked `snapshotAccountNames()` fresh every pass;
now it also loads each account's own file fresh inside that same walk instead
of consulting an in-memory map rebuilt from a construction-time seed. This is
what makes switching a non-event: a name that stops being skipped (its email
no longer matches who's live) simply resolves to whatever was already
persisted for it, including a wait armed while it was still live a moment
ago — no special-casing needed anywhere to detect the switch happened, because
there is no separate identity-tracking state (the old `armedFor`) left to get
out of sync with reality.

**Unconfirmable identity (`loadAccountEmail()` returns `""`) refuses
unconditionally**, before anything else runs — there is no email to resolve a
name from, so there is no way to know whether a wait is even armed for
whoever was live a moment ago, and showing an account's bars means knowing
whose they are. The version of `newUsageFetcher` that predates
`account_cache.go` only refused when a wait already blocked the alternative
(its backoff state lived in memory, decoupled from any per-account lookup, so
an unconfirmable identity with nothing currently armed fell through to a real
fetch); the per-account design has no lookup to fall back to once identity is
unreadable, so the safe answer is to refuse always, not only when convenient.
Slightly more conservative than before, and consistent with the
identity-unconfirmable rule everywhere else in this section.

**A loaded backoff deadline that has already elapsed keeps its streak.** An
earlier version of `loadAccountCache` dropped the streak along with an
elapsed deadline (reasoning: `due()` reports the account fetchable either
way, so a streak whose deadline ended who-knows-how-long ago says nothing
trustworthy about whether the next attempt is part of the same run) — this
was fine when backoff state was loaded once at process startup, but became a
live bug once loading happens on **every pass**: a streak of 1 always has
`BackoffNextAttempt` equal to the instant it was recorded (the first throttle
imposes no wait, by design — see `usageBackoffUntil`), which means it reads
as "elapsed" on the very next load, microseconds later, as an entirely
ordinary part of back-to-back polling. Dropping the streak there silently
prevented it from EVER reaching 2 and arming a real wait —
`TestUsageFetcherBacksOffRepeatedThrottles` and
`TestKnownAccountsFetcherBacksOffRepeatedThrottles` are what caught it (both
reported the pass meant to be held back by an armed wait fetching anyway).
`loadAccountCache` now leaves the streak alone regardless of how elapsed the
deadline is; `due()` alone decides whether a fetch happens, exactly as it
always has.

A deadline that has NOT yet elapsed is still clamped to
`now+usageBackoffCeiling`, for the reason armed waits are capped in the first
place: a corrupt file, a backwards clock adjustment, or a future bug that
writes a garbage timestamp must not be able to wedge an account out of
rotation for longer than a live-armed wait ever could. Clamping alone only
bounds one process's read of a bad value, though; without writing the
correction back, a crash loop that never lets the real (bogus) deadline
elapse would re-clamp it fresh from the same bad raw value on every restart
and never actually fix it — so a clamp is followed by a best-effort save of
the corrected value, the same self-healing shape every other cache write here
already has.

This closes a restart gap, not a concurrency one: every account's cache file
is keyed only by `os.Getuid()` and its own snapshot name (like every cache in
this codebase), so two processes run by the same user on the same host — two
TUIs, or a TUI and a one-shot CLI invocation — share one file per account, not
one per process. `saveAccountCache` writes via `writeFileAtomic` (account.go,
temp-file-then-rename) rather than a truncating `os.WriteFile` specifically
because of this: one account's file can now legitimately have two writers at
once (the live hub and the known-accounts hub both touch the SAME file across
a switch, where before they wrote entirely separate files by role), so a
reader must never be able to observe a half-written entry. Nothing here
coordinates writers beyond that atomicity, though: whoever writes last simply
overwrites the other, the same best-effort "no lock" behavior every other
cache in this file already accepts — atomic rename only rules out a *torn*
read, not a *lost* write. A second process's own successful fetch clearing an
account's wait
mid-throttle is not a bug either — it means that process's view of the
account has genuinely recovered, which is real information the first
process's next own pass will independently confirm. What this still does
nothing for is the same account backed off independently on two *different*
hosts — each host's cache files are local, with no cross-host memory at all,
and the in-memory design this replaced never covered that case either.

A backed-off known account is **not** filtered out of `newKnownAccountsFetcher`'s
walk: it still goes through `knownAccountUsage` with a synthesized 429 in
place of the round trip, so one place keeps deciding whether that name stands
for the live account (which is what resolves `ActiveName`) and whether
`prev`'s numbers may be carried forward — the entry renders as the same
stale-with-`rate limited` line a real throttle produces. The **live** account
gets the same treatment from `newUsageFetcher`, the one-account shape of that
logic — it is the account every session on the host is actually spending, so
it is the likeliest to be throttled. A skipped pass there re-serves the
loaded reading as an ordinary **success**, never an error: an error would
engage `usagePoller.run`'s generic 5s-doubling retry — shared with
`CodexUsageHub`, and the right answer for a genuine transient, which is why
it stays untouched — and that retry is the very burst this prevents. An armed
wait holds **whether or not** there is anything to show: when nothing is safe
to re-serve — no cached entry at all (a cold start, or an account that has
never once succeeded), or a loaded entry whose `Account` doesn't match who is
live (a stale or corrupted file, or a reused snapshot name — the same
`carryable`/`liveCarryable` identity gate as before, now checked against a
value freshly loaded from disk rather than one carried in memory) — the pass
answers with a bare identity placeholder
(`AccountUsage{Account: loadAccountEmail()}`, nil `Info`) and still makes no
request.

`AccountUsage` carries its own `FetchedAt` (stamped in `fetchUsage` and
nowhere else — the server's on-demand `/usage` handler has no memory to carry
anything forward, so it never produces a `Stale` reading, exactly as with
`KnownAccountUsage`). `liveCarryable` is `carryable` in one-account shape —
`fresh`'s numbers-and-info test, including the explicit zero check, plus the
identity test — and a re-serve returns a **copy** marked `Stale` keeping the
original timestamp; the loaded value itself is a fresh struct reconstructed
from JSON on every pass; nothing stores or ages a live pointer across calls
any more. There is deliberately no age bound on either: an outage running
long still leaves numbers that are more informative than a bare "rate
limited" placeholder, and `Stale` (never cleared by a carry) is what tells
the header, and `localFreshAccountEmails`, not to trust them as current. Any
disk seed with a non-nil `Info` is force-marked `Stale` by `loadAccountCache`
itself — this is a disk read, not a completed poll, so nothing has confirmed
the numbers as current — and an unstamped reading (a pre-timestamp disk
write) is simply not carryable, since its vintage is unknown rather than
merely old.

There is no more `saveOnceUsage`-style wrapper needed on the live side:
`saveAccountCache` is called by `newUsageFetcher` itself, directly, only from
the two branches that actually changed something (a real fetch's success or
failure) — the carry and placeholder branches return early without touching
disk at all, so there is nothing analogous to "skip re-persisting an
unchanged re-serve" left to get wrong.

**A first version of this whole unification shipped with real bugs an
independent cross-model review caught before merge** — the kind of gap that
only shows up once you stop trusting a design and start trying to break it.
Worth naming so the next person doesn't reintroduce them:

- *The identity gate protected numbers but not backoff.* `liveCarryable`/
  `carryable` always gated whether a loaded entry's `Info` could be shown, but
  the loaded `BackoffStreak`/`BackoffNextAttempt` were extracted and trusted
  unconditionally in both fetchers. A claude-switch snapshot name reassigned
  to a different account (`account save --force`) would inherit the outgoing
  account's armed wait even though the new account was never itself
  throttled. Fixed with `entryIdentityMatches` (account_cache.go), applied to
  both halves together in `newUsageFetcher` and `newKnownAccountsFetcher`: a
  mismatched entry has neither its numbers nor its backoff trusted, which
  forces a real fetch instead of waiting out someone else's throttle.
- *A live account with no matching snapshot lost backoff protection
  entirely, not just cross-restart memory.* The first version had genuinely
  zero in-memory state; `resolveActiveSnapshotName` returning `""` meant
  every single pass loaded nothing and treated the account as never
  throttled — not just after a restart, but on the very next 2-minute tick,
  forever. Verified with an A/B fetch-count comparison against `main` before
  the fix (`TestUsageFetcherBacksOffEvenWithNoMatchingSnapshot` pins the
  fixed count). The fix reintroduces a small amount of in-memory state
  (`fbLast`/`fbBackoff`/`fbArmedFor` in `newUsageFetcher`) used ONLY when
  `name == ""` — an unconfirmable identity or a never-saved snapshot — mirroring
  the design this whole file replaced, but scoped narrowly enough that it
  never interferes with the disk-based continuity a resolved name gets.
- *An unconfirmable identity refused unconditionally, even with nothing
  armed.* The first version treated "can't read `~/.claude.json`" as an
  automatic refusal, reasoning that showing bars means knowing whose they
  are. But the file read isn't the only route to identity —
  `fetchVerifiedUsageInfo`'s profile probe can attribute numbers to a
  verified account the file alone can't confirm (`usageAccountLabel`) — so an
  unconditional refusal would permanently blank the header the instant the
  file becomes unreadable, even though a real fetch might still recover
  identity. Fixed: the fallback path now refuses only while a wait is
  actually armed, exactly like the disk path does.
- *The fallback and disk paths didn't share memory, so a wait armed while
  identity was readable stopped being enforced the moment identity broke.*
  Fixed by having the disk path mirror its outcome into the same fallback
  vars at every return point, not just use them — so a wait armed on disk
  survives identity going unreadable mid-run instead of a still-armed wait
  silently going unenforced the instant `resolveActiveSnapshotName` stops
  resolving. (`TestUsageFetcherPlaceholdsWhenIdentityIsUnconfirmable` is the
  regression pin: two throttles while identity is readable, then the file is
  removed — the pass must still refuse, not fetch.)
- *`Verified` silently downgraded to `false` on every carried or failed
  pass.* `persist` originally derived it from `u.Info.VerifiedAccount`, but
  that field is `json:"-"` — always empty on anything reconstructed from a
  JSON-round-tripped `loadAccountCache` read, which is exactly what `last` is
  on a carry or a failure. Fixed by threading `matches && cached.Verified`
  through explicitly on those paths, and only deriving from
  `u.Info.VerifiedAccount` on an actual fresh fetch.
  (`TestUsageFetcherPreservesVerifiedAcrossACarry` pins it.)
- *`saveAccountCache` used a truncating `os.WriteFile`, not atomic
  temp-file-then-rename.* Every other cache in this codebase gets away with
  that because each file had exactly one writer (the live hub owned its
  files, the known-accounts hub owned its own separate ones). One file per
  account now genuinely has two possible writers — the live hub and the
  known-accounts hub both touch the SAME account's file across a switch — so
  a reader could observe a torn write. Fixed: `saveAccountCache` now goes
  through `writeFileAtomic` (account.go), the same temp-file-then-rename
  helper `account.go` already uses for its own credential writes.
- *`loadAccountCache`'s clamp self-heal wrote to disk directly*, bypassing
  the injected-`save` discipline the rest of this design follows specifically
  so tests don't need to isolate TMPDIR for every code path that might write.
  Fixed: `loadAccountCache` now only *reports* whether it clamped (a second
  return value), and each fetcher writes the correction back through its own
  already-injected `save`.
- *`allKnownAccounts` (the old per-account-walk helper) became dead code*
  once `newKnownAccountsFetcher` started inlining that walk directly to reach
  each account's own disk entry — nothing outside its own tests called it any
  more. Removed; the tests that exercised it now exercise the same behavior
  through the real `newKnownAccountsFetcher` path instead
  (`TestKnownAccountsFetcherNeverFailsOnOneBadAccount`,
  `TestKnownAccountsFetcherSkipsLiveAccountAndReportsItActive`,
  `TestKnownAccountsFetcherThreadsEachAccountsOwnEntry`).
- *`seedKnownAccounts` dropped the vintage gate `fresh()` applies everywhere
  else* — it checked only `Info != nil`, not `!FetchedAt.IsZero()`, so an
  unstamped entry (never produced by a current writer, but latent) could seed
  the header with numbers the fetcher's own `carryable` would then refuse to
  carry forward. Fixed to match `fresh()`'s own gate.

**A second review pass against the fixed diff, before merge, caught two more
— proof that fixing reasoning-derived bugs with more reasoning is exactly how
this class of bug survives a review.** Both were verified with a failing
reproduction *before* the fix landed, not just argued through:

- *`entryIdentityMatches`'s effect on an unverified (`Account == ""`) entry
  looked, on paper, like it could combine "backoff held" with "numbers
  withheld" into a permanent bare-placeholder state.* Checked empirically
  with a throwaway reproduction rather than taken on the reviewer's word:
  the combination is real but is *not* a regression — `liveCarryable` already
  refused to carry numbers for an empty `Account` before this file existed
  (its own explicit zero check), and switch detection here is structurally
  independent of `Account` entirely (it goes through
  `resolveActiveSnapshotName` reading a *different file*, not through
  comparing `Account` strings), so both the wait and the eventual recovery
  behave exactly as they did pre-unification. No fix needed; recorded here so
  it isn't re-litigated.
- *The fallback vars (`fbLast`/`fbBackoff`/`fbArmedFor`) are shared across
  every account `newUsageFetcher` is ever asked about, and every disk-path
  return point mirrored into them unconditionally.* So a snapshotted
  account's disk pass would clobber a *different*, still-unsnapshotted
  account's own armed wait: switch from an unsnapshotted account (with a
  wait armed in the fallback) to a snapshotted one and back, and the first
  account's wait is gone, even though nothing about that account itself
  changed. Reproduced empirically first (a debug test showing the fetch
  count incrementing on the pass that should have been held back), then
  fixed by gating the mirror: it now only writes when the fallback is
  untracked or already tracking the *current* live email
  (`mirrorFallback` in `newUsageFetcher`).
  `TestUsageFetcherFallbackSurvivesAnUnrelatedDiskPass` pins it, and was
  confirmed to fail against the unguarded mirror before the guard was added.
  A second, independent-review-caught round on this same fix: the guard
  compares `fbArmedFor` against `live`, but a wait armed while identity was
  itself unconfirmable (`live == ""`) was written into `fbArmedFor` as `""`
  too — indistinguishable from "untracked" to the guard, so a later resolved
  account's disk pass could still clobber an ownerless wait even with the
  guard in place. Fixed with the `fbUnconfirmedOwner` sentinel
  (`coalesceOwner` in usage.go): an unconfirmable identity's armed wait is
  now recorded under a value that can never equal a real email, so `""`
  reliably means "nothing armed" everywhere it's checked.
  `TestUsageFetcherUnconfirmedIdentityWaitSurvivesALaterResolvedAccount` pins
  it, confirmed to fail without the sentinel. (One thing this fix does *not*
  touch: the "different account" clearing check in the `name == ""` branch
  still requires `live != ""` before clearing — an unreadable identity is
  never itself evidence of a *different* account, which is the whole reason
  a disk-mirrored wait survives identity going unreadable mid-run at all;
  removing that guard to "simplify" alongside the sentinel change was tried
  and immediately broke `TestUsageFetcherPlaceholdsWhenIdentityIsUnconfirmable`.)

Also added in this pass: `TestAccountSwitchRoundTripsThroughRealDisk`, which
`TestAccountSwitchPreservesContinuityBothDirections` was missing — that test
pre-seeds both accounts' files by hand and passes a no-op `save` to both
fetchers, proving only that a *pre-existing* file loads correctly across a
switch, never that what one fetcher actually *writes* is what the other then
*reads*. The new test wires both fetchers to the real `saveAccountCache` and
drives one real write (a live fetch as trecs) followed by a real read (the
known-accounts fetcher, after switching away) — the literal round trip the
whole per-account-file design exists for.

**Accepted, currently-undocumented-elsewhere gaps, left open rather than
fixed in this pass:**

- *A resolvable account whose disk write fails loses backoff accumulation
  entirely, not just warm-start memory.* The disk path in both fetchers
  keeps no in-memory copy of what it last wrote — every pass reloads from
  disk — so if `saveAccountCache` silently fails (e.g. a read-only TMPDIR),
  the next pass never sees the streak the previous pass tried to record, and
  the account gets fetched (and likely re-throttled) every single pass
  instead of backing off. The `fbXXX` fallback does not cover this case: it
  only engages when the name fails to *resolve*, not when it resolves but
  the *write* fails. Documented directly on `saveAccountCache`
  (account_cache.go) rather than fixed here, since covering it means
  threading a write-success signal out of `save func(name, e)` into the
  fallback's engagement condition — a real change, not a comment fix.
- *Orphaned per-account files are never pruned.* Removing a claude-switch
  snapshot (`account remove`) does not delete its `claude-sessions-account-*`
  cache file — it just stops being read, since `snapshotAccountNames()` no
  longer lists it. Harmless (an unread file costs nothing but disk), but
  worth knowing before assuming the file list on disk matches the account
  list `account list` shows.
- *`Stale` is write-only on disk.* `loadAccountCache` always sets it to
  `true` unconditionally on any successful decode (a loaded entry is, by
  definition, not this pass's own fresh fetch), so nothing ever persists
  `Stale: false` — it only exists as an in-memory signal on the value each
  fetcher hands back for the current pass. Not a bug: nothing reads the
  on-disk field expecting anything else.

`localFreshAccountEmails` excludes a `Stale` live account for the identical
reason it already excluded a `Stale` known one, and `dedupeAccounts` threads
`Stale` into its pass-1 `add` so a carried live or remote line takes the same dim
`stale` marker a snapshot line does. That marker is display only: precedence and
`mine` are untouched, and a live line still wins its email's slot over any
snapshot copy of the same account however old it is — showing your *own*
account's last-known reading is the point of that rule.
There is no server-side counterpart to any of this any more: `/usage` never
fetches, so it has no streak to back off and nothing to classify.

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
`FetchRemoteUsage`'s own timeout is still 8s, not `FetchRemote`'s 5s — left
generous even though the handler it calls is now pure disk reads with no
per-account fan-out to wait on.

`FetchRemoteUsage` still sends `?ignore=` (built by `localFreshAccountEmails`
from this machine's own hub snapshots), but the handler no longer reads
`r.URL.Query()` at all — an unrecognized query parameter a plain HTTP server
just ignores. `/usage` no longer fetches for anyone, so there is nothing left
for `ignore` to skip; sending it today changes nothing about the response. It
is left in place on the client rather than ripped out: it costs one query
string per poll, and a future server that ever re-adds a scoped fetch (a
client explicitly asking one host to check one account, say) would want
exactly this "accounts I already have numbers for" list without re-deriving
it.

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
still dropped is `Info == nil && !Expired && Reason == ""` — every remote
account now, since `/usage` reports identity only; giving one a header line
would show a bar-less row for every account any remote host merely knows
about. Snapshot-derived lines are never marked `mine`, so one
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
overlay and its action. Entry points: the `account switch|save|list|remove`
subcommands, `POST /account/switch` (bearer auth like every mutating endpoint,
`400 unknown_account` / `500 switch_failed`, no `session_id`-style precondition
because it is host identity, not a session), and Ctrl+W in the TUI (picker →
Enter → done, no confirm dialog). `remove` is CLI-only on purpose — no
endpoint, no TUI binding: deleting a switch target is rare, irreversible
without a relogin, and has no business being one keystroke from the picker.

The two override flags are deliberately *not* interchangeable
(`accountArgs`, commands.go). `remove` asks a yes/no question, so `-y` answers
it and `--force` is a synonym. `save` asks nothing — it *refuses*, and
`--force` means "reassign this snapshot to another account", so `-y` is
rejected there: the reflexive don't-ask-me flag must not reassign a
credential. `switch` and `list` reject both.

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
refreshed with a stale one) → **step 2.4**, validate the credential about to
be installed (`validateSnapshotCredential`) → **step 2.5**, collect the
advisory session warning (`switchSessionWarnings`) → unconditional rescue copy
of the live credential to the single rolling `.last-switch-rescue.<ext>` slot → when the
outgoing account resolves to a name, its own credential + identity snapshot →
arm the pending-switch marker → only then overwrite the live credential →
patch `~/.claude.json` → disarm the marker → return the email re-read from
disk. The rescue and named-sync-back steps exist so a failure can never
strand you: the outgoing credential always has at least one copy on disk
before anything overwrites it, including when its account can't be named
(first switch, renamed account), which is exactly the case the *rescue* copy
covers and the named sync-back cannot.

**Steps 2.4 and 2.5 both sit after the no-op return, and that placement is the
point.** 2.4 refuses a snapshot that is no longer a login: no `refreshToken`,
or a `refreshTokenExpiresAt` already past. Installing one logs the host out —
Claude Code's next refresh is answered `invalid_grant` and it *zeroes* the
stored credential — which is the failure that reads as "the switch worked and
then everything broke". An expired *access* token is fine and is not probed;
refreshing it is what the refresh token is for. When the access token is still
valid the profile endpoint is asked whose it is, and a disagreement with the
identity file is refused too (the same misattribution `wrong identity` reports
after the fact, caught before it becomes the live credential). That probe is
advisory — validation works offline, and any failure to reach it leaves the
two file-only checks as the whole decision. The validated bytes are carried to
the write rather than re-read, so what was checked is what gets installed.
Running this *before* the no-op return would let a stale parked snapshot turn a
guaranteed no-op into a refusal, and spend a network probe on it.

2.5 warns — never refuses, never prompts — about Claude Code sessions still
running under the outgoing account. Those processes hold its token and write a
refreshed one back to the live store whenever it ages out, so one of them can
overwrite the credential this switch just installed, or (on a superseded
refresh token) make Claude Code zero it. Nothing here can prevent that: this
tool does not own those processes and there is no way to make one re-read its
credential. Refusing or prompting would block the very case it warns about —
the command is quite likely being typed inside one of those sessions. The
collector is `CollectLocalLite` behind the `collectSessionsForSwitch` seam,
because this runs under the flock where every millisecond blocks another
switch, and all the warning needs is pids. A collection failure yields no
warning rather than an error. `switchAccount` returns
`(email, warnings, err)`; the CLI prints the full text to stderr,
`POST /account/switch` carries it in `warnings` on the *success* response, and
the Ctrl+W picker folds a short clause into `accountSwitchToast` — the picker's
cooked window is repainted the instant the TUI loop resumes, so the toast is
the only surface that survives.

`account save NAME` refuses to file the live credential under a name whose
`.<name>.account.json` already names a *different* account — that is the
misattribution every other guard here exists to prevent, reached by hand.
Refreshing a snapshot of the same account after a relogin is untouched, and a
first save of an unclaimed name is never refused; `--force` reassigns
deliberately and still clears the pending-switch marker. `account remove NAME`
deletes the credential blob under **both** platforms' names plus the identity
file, under the lock, validated against `snapshotAccountNames()` — which is
what makes the rescue slot unremovable, since the listing filters it out. It
never touches the live login. Whether the name stood for the live account is
re-derived **under the lock** and returned, not taken from the pre-prompt
`planAccountRemoval`: a human can sit on the confirmation while a Ctrl+W switch
changes the answer, and what is reported has to describe the removal that
actually happened.

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
