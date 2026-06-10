# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

A single Go binary that views and manages Claude Code CLI sessions both locally
and across remote machines. The same binary serves three roles depending on the
subcommand:

- **client** (no args / `list`): live TUI showing local + remote sessions
- **server** (`-s`): HTTP+JSON daemon exposing this host's sessions
- **scriptable subcommands** (`kill`/`attach`/`migrate`/`new`/`preview`/`tmux-info`):
  non-interactive entry points for automation and shell pipelines

History: this was originally a ~1500-line bash+python script. The Go rewrite
exists because the project was being deployed across machines (macOS dev box,
Linux home server, Raspberry Pi) and single-binary distribution beats wrangling
Python versions and shell portability.

## Build / install / deploy

```sh
make                                       # cross-compile every arch into ./bin/
make install                               # build, then copy host-arch binary to ~/.local/bin
make deploy-linux-amd64 HOST=user@server   # build + scp Linux/amd64
make deploy-linux-arm64 HOST=pi@somepi     # build + scp Linux/arm64
make run                                   # build + run host binary
```

`HOST` goes straight to ssh/scp — anything `~/.ssh/config` resolves works.
Per-developer convenience shortcuts (e.g. a named `deploy-myserver` target)
belong in `Makefile.local` (gitignored, auto-included).

`go build .` / `go run .` work for quick single-arch iteration.

Tests live next to the code (`usage_test.go`, `render_test.go`); run
`go test ./...` plus `go vet ./...`.

## Architecture

The codebase is one Go package (`main`) split into files by concern. The three
runtime roles share a common foundation; nothing is duplicated between them.

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

Important: the Go server handlers call the underlying primitives directly. The
bash version shelled out from server → subcommands; in Go that's an
anti-pattern, so the cmd_* subcommands and server handlers both wrap the same
`migrate.go` / `preview.go` / `session.go` primitives.

### Live TUI architecture

`tui.go::RunTUI` is the only place that owns the terminal. Flow:

1. `term.MakeRaw` (saving the cooked state) + `enableOutputProcessing` (see gotcha below)
2. Print alt-screen / hide-cursor / disable-wrap escape sequences
3. Render → `readEvents(interval)` → handle keys / handle tick → repeat

Actions (act_kill, act_attach, act_preview, act_new) take an `actCtx` and may:
- prompt for input (switch to cooked, `bufio.Scanner`, switch back to raw)
- shell out interactively (`runInteractive` in helpers.go: exit alt-screen,
  restore cooked, exec, re-enter alt-screen + raw)
- recurse into remote-action helpers (`remote_actions.go`) when the selected
  row's `Host` is non-empty

### Subtle invariants

**Single stdin consumer.** Only one thing reads stdin at a time. The TUI loop
uses `os.Stdin.SetReadDeadline` to time out (no goroutine). When an action
prompts, it switches to cooked mode and uses `bufio.Scanner`. The bash version
had a background goroutine that raced with the prompt scanner and silently
dropped "y" keystrokes — don't reintroduce that pattern.

**`term.MakeRaw` zeros OPOST.** That makes `\n` move the cursor down but
*not* to column 0, which visually destroys every multi-line render. The fix
is `enableOutputProcessing(fd)` (defined in tui.go, ioctl constants in
`termios_{bsd,linux}.go`). Call it **every time** you re-enter raw mode —
initial entry, after each prompt, after every `runInteractive`. Both
`enterRaw` (helpers.go) and `runInteractive` already do this; new call sites
must too.

**Tmux pane detection must check the pid itself first.** `walkTmuxPane`
(tmux.go) walks pid → ppid up to 32 hops. It checks `panes[cur]` **before**
moving to `ppid[cur]`, because `tmux new-session "claude --resume ..."`
spawns claude as the pane's foreground process — claude's pid *is* the pane
pid, with no shell parent.

**ssh attach needs an explicit user.** Tmux sessions are per-user. If the
local username differs from the user the server runs as, `ssh host tmux
attach` will see "no sessions" because it's looking in the wrong namespace.
Either set `ssh_user` in `servers.yaml` or `User <name>` for that host in
`~/.ssh/config`. `ServerConfig.EffectiveSSHTarget()` builds the `user@host`
string.

**Wall-clock tick uses `unix.Select`, not `os.Stdin.SetReadDeadline`.**
SetReadDeadline silently no-ops on stdin inherited at process start (the Go
runtime's netpoller doesn't auto-register it), so `Read` would block until
input arrives and the auto-refresh tick would never fire under continuous
typing. `readEvents` in tui.go uses `unix.Select(fd+1, &fdSet, nil, nil, tv)`
to poll the raw fd with a real timeout. Don't "simplify" back to the
Deadline API — it'll silently re-break ticking on PTY-inherited stdin.

**Remote PIDs in `Session.ID()`.** Local rows have `Host == ""` and `ID()`
returns `"<pid>"`. Remote rows have `Host == "<name>"` and `ID()` returns
`"<name>:<pid>"`. Action dispatch uses `s.Host != ""` to route between local
and remote handlers.

### Usage polling

`usage.go` polls Anthropic's OAuth usage endpoint (token from the macOS
Keychain / `~/.claude/.credentials.json`) every 2 minutes in a background
goroutine (`UsageHub`, following `RemoteHub`'s pattern minus the wake pipe)
and the TUI header displays it as two progress bars (5-hour and weekly limits).
The token is read-only — token refresh/rotation is Claude Code's job.

### YAML config

`yaml.go` is a hand-rolled parser for exactly one shape: a top-level
`servers:` key whose value is a list of flat mappings of scalars (`name`,
`host`, `port`, `token`, `ssh_host`, `ssh_user`). No flow style, no anchors,
no nested structures, no multiline scalars. Don't extend the schema without
extending the parser.

### Cross-platform termios

The only OS-conditional code is `termios_bsd.go` (darwin/freebsd/openbsd/
netbsd) and `termios_linux.go`, which provide `ioctlGetTermios` /
`ioctlSetTermios` constants. Everything else uses `golang.org/x/sys/unix` and
`golang.org/x/term` cross-platform.

## Releases

Tags matching `v*` trigger `.github/workflows/release.yml`, which
cross-compiles all three platform binaries, generates `SHA256SUMS`, and
creates a GitHub release with the binaries attached. Release notes are
extracted from the matching `## [vX.Y.Z]` section in `CHANGELOG.md` — add an
entry there before tagging or the release will have empty notes.

```sh
# example for a new release
edit CHANGELOG.md           # add ## [v1.1.0] section at top
git commit -am "v1.1.0"
git tag -a v1.1.0 -m "v1.1.0"
git push origin main v1.1.0
```

## Dependencies

Only `golang.org/x/term` (raw mode helpers) and `golang.org/x/sys` (for the
termios ioctls). Stdlib for everything else: `net/http` server + client, JSON,
threading, file I/O. Keep it that way — single-binary deployment is the whole
point of the rewrite.

## Files at a glance

(See README.md for the full layout table. The biggest files are
`render.go` ~390, `server.go` ~280, `tui.go` ~270, `remote_actions.go` ~250,
`actions.go` ~210.)
