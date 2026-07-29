# servers.yaml-Only Client Design

**Date:** 2026-07-29
**Status:** Approved

## Goal

Remove the TUI's and scriptable subcommands' direct, in-process access to
local sessions (`CollectLocal`, `KillSession`, `MigrateLocal` called straight
from the client). Route every session view and mutation — local or remote —
through a running `-s` server's HTTP API, the same path remote hosts already
use. This closes a real correctness gap, not just a style preference: the
server's HTTP handlers carry `sessionIDPrecondition` + `reattest` (the
TOCTOU guard described in CLAUDE.md's "Action preconditions and idempotency")
and `spawnDedupe`; the client's local-direct calls (`actKill`/`actAttach`/
`actNew` in actions.go, `cmdKill`/`cmdMigrate`/`cmdNew` in commands.go) have
none of that. Local usage is racier today than remote usage of the identical
feature.

## History

Local and remote have been two parallel implementations since the Go rewrite
(see CLAUDE.md's "Three roles, one substrate" — client, server, subcommands
share primitives, but the *client's* local path calls those primitives
directly rather than through the server). That duplication is the likely
source of recent drift bugs (DisabledStore wired separately for local vs.
remote in `86641ff`/`160edb0`, the self-referencing remote-fetch fix in
`851a61e`). This design collapses the duplication by making local sessions
just another `servers.yaml` entry.

## Non-goals

- No change to `render.go`'s section layout, `service install`/`uninstall`/
  `status` internals, or the servers.yaml top-level shape (`servers:` /
  `commands:`).
- No auto-spawn of a server process on demand. If nothing is configured, the
  client errors and offers to configure — it does not silently launch a
  daemon behind the user's back.
- No migration tooling for existing servers.yaml files — adding a `self`
  entry is additive; existing remote entries are untouched.
- No change to how remote (non-self) hosts already work — `remote_actions.go`
  keeps its existing SSH-attach, HTTP-mutate behavior; this design only
  extends that path to also cover the local machine.

## Architecture

### `self` server entries

`ServerConfig` gains one new field:

```
self: true    (optional; default false)
```

Exactly one entry in a given servers.yaml is expected to carry `self: true`
— the local machine's own `-s`/service instance, normally `127.0.0.1:<port>`
with its own generated token. `service install` seeds this entry (and starts
the daemon) as part of its existing setup flow; it is not a new standalone
concept for the user to hand-write, though they still can.

`self` is what `actAttach`/`actAttachRemote` use to decide whether to SSH: a
`self`-flagged host runs `tmux attach` directly on this machine instead of
`ssh <target> tmux attach` — SSH-to-yourself is unnecessary overhead and
would require SSH-to-localhost to be configured for no reason. Everything
else (data fetch, kill, migrate, spawn, disable) is indistinguishable from
any other host: plain HTTP against `EffectiveSSHTarget`'s host equivalent
(the HTTP `host:port`, not the SSH target).

### Client becomes HTTP-only

- **TUI**: the local-collect branch in the render/refresh path is removed.
  `FetchAllRemote` (already fetching every configured server concurrently)
  becomes the only data source, `self` entries included. `actKill`/
  `actAttach`/`actNew`/`actToggleDisabled`'s local branches in actions.go
  are deleted; `actKillRemote`/`actAttachRemote`/`actNewRemote`/
  `actToggleDisabledRemote` (remote_actions.go) become the only
  implementations, parameterized by whether the target `ServerConfig.Self`
  is true (for the SSH-skip in attach only).
- **Scriptable subcommands** (`kill`/`attach`/`migrate`/`new`/`preview`):
  given a bare PID with no host qualifier, resolve against the `self` entry
  and call its HTTP API instead of touching the PID in-process. Same
  attestation guarantee the server already provides to remote callers.
- **Server** (`cmdServer`, `CollectLocal`, `KillSession`, `MigrateLocal`,
  `SpawnNew`): unchanged. These remain exactly what a `-s` process — local
  or remote — uses to inventory and act on its own host. Only who *calls*
  them changes (the local server process, never the client directly).

### Empty-config behavior

`len(servers) == 0` (no `self`, no remote entries) is a hard error at
startup, in both TUI and every scriptable subcommand:

- TUI: prints the error, offers interactively (y/N prompt) to seed a `self`
  entry into servers.yaml and run the equivalent of `service install`.
- Scriptable subcommands: print the same error and the equivalent
  instructions (`claude-sessions service install`), exit non-zero. No
  interactive prompt, no auto-mutation of config from a script context.

An unreachable-but-configured server (self or remote) is **not** this error
— that's the existing per-host offline/error row behavior `FetchAllRemote`
already has, unchanged.

## Data Flow

```
Before:                              After:
┌─────────┐  direct call             ┌─────────┐  HTTP (self entry)
│ TUI/CLI │ ───────────────────────► │ TUI/CLI │ ───────────────────────►
│ (local) │  CollectLocal/           │         │  http://127.0.0.1:<port>
└─────────┘  KillSession/            └─────────┘  (same code path as any
             MigrateLocal                          remote host)
                                                          │
┌─────────┐  HTTP (servers.yaml)                          ▼
│ TUI/CLI │ ───────────────────────►               ┌──────────────┐
│(remote) │  http://<remote-host>:<port>            │ -s (this box)│
└─────────┘                                         │ CollectLocal │
                                                     │ KillSession  │
                                                     │ MigrateLocal │
                                                     └──────────────┘
```

## Error and Concurrency Behavior

- Local kill/migrate/spawn/disable now get `sessionIDPrecondition` +
  `reattest` + `spawnDedupe` for free — the exact TOCTOU/duplicate-spawn
  protection remote already has, closing the gap this design exists to
  close.
- Attach for a `self` entry is a plain local `tmux attach`/`switch-client`
  (unchanged behavior, just re-routed through the `Self` flag instead of
  `Host == ""`).
- `servers.yaml` with zero entries: hard error, offer to configure (see
  above) — never a silent no-op or an empty TUI.
- A configured-but-unreachable `self` entry (daemon crashed, not started)
  behaves exactly like a configured-but-unreachable remote entry today: an
  error/offline row, not the "nothing configured" error path.

## Test Plan

- `yaml.go`: parses/round-trips `self: true`; defaults to `false` when
  absent; rejects a second `self: true` entry (config error) — exactly one
  self entry is a documented invariant, not merely a convention.
- Attach: `self`-flagged target skips SSH and runs local `tmux attach`;
  non-`self` remote target still SSHes, unchanged.
- Empty servers.yaml: TUI prompts to configure; scriptable subcommands print
  the error + instructions and exit non-zero — both paths covered.
- Local kill/migrate/spawn/disable via the `self` HTTP path reproduce the
  same `session_mismatch`/`not_live` refusals and dedupe behavior already
  tested for remote hosts (existing server_test.go coverage extends to
  cover the loopback case, not new logic).
- Regression: existing remote-only servers.yaml configs (no `self` entry)
  continue to work unchanged — remote-only viewing (the "machine A views
  only machine B" case) stays valid with zero local entry.

### Verification

```sh
go build ./...
go vet ./...
go test ./...
go test -race ./...
```
