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
