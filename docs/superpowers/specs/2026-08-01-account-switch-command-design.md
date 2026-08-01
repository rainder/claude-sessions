# Account Switch Command Design

**Date:** 2026-08-01
**Status:** Approved

## Summary

Port both standalone `claude-switch` scripts (macOS Keychain-based, Linux
file-based — see `~/.local/bin/claude-switch` on each machine) into the
`claude-sessions` binary as a cross-platform `account` subcommand family,
plus a TUI hotkey to switch accounts (local or remote) without leaving the
table, plus one new mutating HTTP endpoint so a remote host's account can be
switched from any machine's TUI/CLI.

This depends on `2026-08-01-account-usage-visibility-design.md`'s snapshot
discovery and `KnownAccounts`/`Usage` transport — that spec's data already
tells a client every known account name + email + which one is currently
active per host, which is exactly what the switch picker needs. Implement
that spec first.

The Go port **coexists** with the existing shell scripts: identical file
formats and paths (`.{name}.keychain-cred` / `.{name}.credentials.json` /
`.{name}.account.json`), so either tool works interchangeably on any
machine, no migration step.

## Goals

- `claude-sessions account switch <name> [--server S]` — switch this
  machine's (or a configured remote's) active Claude Code account.
- `claude-sessions account save <name>` — capture the currently active
  credential + identity into a named snapshot, local-only. Automates the
  manual two-command setup step described in `claude-switch`'s own header
  comment.
- `claude-sessions account list [--server S]` — show every known account
  snapshot, local and every configured remote, which one is active where.
- TUI: `Ctrl+W` on a selected row opens a small picker of known accounts for
  that row's host (local row → local accounts; remote row → that host's
  accounts), current one marked. `Enter` applies immediately (matches your
  described flow — no extra confirm screen), `Esc` cancels.
- One new endpoint, `POST /account/switch`, auth-gated the same way as every
  other mutating endpoint (`Authorization: Bearer <token>`), so the TUI and
  CLI can switch a remote host's account.
- Full parity with both existing scripts' behavior: sync the outgoing
  account back to its own snapshot before switching, patch the
  `~/.claude.json` identity cache (`oauthAccount`/`userID`) so no manual
  `/login` is needed when a snapshot exists.

## Non-goals

- Refreshing an expired snapshot's token (no OAuth refresh flow) — same
  non-goal as the usage-visibility spec.
- Retiring the shell scripts.
- A confirm-before-switch dialog (explicitly decided against — picker +
  Enter is the whole interaction).
- Adding a `jq` dependency. The identity-cache patch is done with
  `encoding/json` only, same "stdlib + golang.org/x/term + golang.org/x/sys
  only" constraint the rest of the codebase holds to.

## CLI

### `account switch <name> [--server S]`

Local (no `--server`): calls `switchAccount(name)` directly (see Core logic
below). Remote (`--server S`, resolved via `LookupServer`, same as `new
--server`): `POST /account/switch` on that host via the existing
`remoteRequest` helper, body `{"name": "<name>"}`.

Errors: unknown name → lists available names (from `snapshotAccountNames()`)
and exits 1, same UX as `claude-switch`'s own "no snapshot for '$target'"
message.

### `account save <name>`

Local-only, no `--server` flag — saving snapshots a *specific* machine's
currently active credential, so it only ever makes sense to run it on that
machine (matches your "(cmd only)" framing). Captures:

1. The live credential blob (macOS: `security find-generic-password -w` for
   "Claude Code-credentials"; Linux: read `~/.claude/.credentials.json`) →
   written to `.{name}.keychain-cred` / `.{name}.credentials.json`,
   `chmod 600`.
2. `{oauthAccount, userID}` sliced out of `~/.claude.json` → written to
   `.{name}.account.json`, `chmod 600`.

Overwrites an existing snapshot of the same name (re-running it after a
relogin refreshes the snapshot) — this is the automation of the setup step,
not a one-time-only command.

### `account list [--server S]`

No `--server`: local snapshot names (via `snapshotAccountNames()`), each
paired with its `.{name}.account.json` email and whether it matches the
live `loadAccountEmail()` (marked `active`).

With `--server S`: same, but for one configured remote, fetched by hitting
that host's existing `GET /sessions` and reading its `usage` +
`knownAccounts` fields (both already carry name/email/active-ness per the
usage-visibility spec) — **no second new GET endpoint**, this reuses
transport that spec already adds.

No flag at all: local, then every configured remote in `servers.yaml`
order, one small table:

```text
HOST                NAME      EMAIL              ACTIVE
local               avisoma   andy@avisoma.com   yes
local               trecs     andy@trecs.aero    no
agent-workstation   avisoma   andy@avisoma.com   no
agent-workstation   trecs     andy@trecs.aero    yes
```

A remote that's unreachable prints its `Error` in place of rows (matching
`cmdListSessions`'s existing per-host error handling) rather than aborting
the whole command.

## TUI

`Ctrl+W` (unused today — verified against the full key-dispatch table in
`tui.go`) on a selected row opens an overlay listing that row's host's known
accounts, built entirely from data already in memory (local's own
`KnownAccountsHub` snapshot + `localAU`; a remote row's
`RemoteResult.Usage`/`RemoteResult.KnownAccounts`) — no fetch triggered by
opening the picker. Visual style matches the existing small overlays
(bordered box, arrow-key nav) rather than the full searchable resume
picker — account counts are small (2–3 typically), no search needed.

- Current account is marked (e.g. a leading `●`).
- `Enter` on a different entry applies immediately: local row → calls
  `switchAccount(name)` in-process; remote row → `POST /account/switch` via
  `remoteRequest`, same pattern `actKillRemote`/`actAttachRemote` already
  follow.
- `Esc` closes with no change.
- A host with zero known snapshots shows an empty-state line ("no known
  account snapshots — `claude-sessions account save <name>`") and only
  `Esc` is live.
- After a successful switch, the same `refresh(true)` + toast pattern other
  mutating actions use (`actNew`, `actToggleDisabled`) confirms the new
  active account without a separate dialog.
- A failed switch (network error, host rejected the name) shows a toast
  error; the overlay closes either way — it does not retry or leave a
  half-applied state (see Core logic ordering below for why a failure can't
  leave the host worse off than before).

## Core logic (`account.go`, new file, both platforms)

```go
func snapshotAccountNames() ([]string, error)          // shared with the usage-visibility spec
func snapshotAccountEmail(name string) string           // shared with the usage-visibility spec
func readActiveCredential() ([]byte, error)              // darwin: security find-generic-password -w
                                                          // else: read ~/.claude/.credentials.json
func writeActiveCredential(data []byte) error             // darwin: security add-generic-password -U
                                                          // else: write ~/.claude/.credentials.json
func currentAccountName() string                         // matches live email against every known
                                                          // snapshot's account.json email; "" if none match
func switchAccount(name string) (email string, err error)
func saveAccountSnapshot(name string) error
func patchIdentityCache(accountJSON map[string]json.RawMessage) error // merges oauthAccount+userID into
                                                                        // ~/.claude.json, plain encoding/json
```

`switchAccount(name)`:

1. Validate `name` against `snapshotAccountNames()`; error listing available
   names if unknown.
2. `current := currentAccountName()`. If `current != "" && current != name`:
   sync the outgoing account back to its own snapshot first —
   `readActiveCredential()` → write to `.{current}.<ext>`; slice
   `~/.claude.json`'s `oauthAccount`/`userID` → write to
   `.{current}.account.json`. **This step must succeed (or be a no-op, e.g.
   `current == ""`) before step 3 touches the live credential** — the same
   ordering both existing shell scripts already use, and the reason a
   partial failure here can't strand you: the live credential isn't
   overwritten until the outgoing one is safely captured.
3. Read `.{name}.<ext>`, write it via `writeActiveCredential` (the actual
   account switch).
4. If `.{name}.account.json` exists, `patchIdentityCache` it into
   `~/.claude.json` — no `/login` needed. If it doesn't exist (a snapshot
   created by the old `claude-switch` before this feature, or a
   never-fully-synced one), skip silently — same fallback both scripts
   already document ("`/login` once, cached for next time").
5. Return the new active email (re-read via `loadAccountEmail()` after the
   patch, so the return value reflects what's actually on disk, not what was
   assumed).

`patchIdentityCache` decodes `~/.claude.json` into a
`map[string]json.RawMessage` (preserving every field this codebase doesn't
otherwise touch), overwrites exactly the `oauthAccount` and `userID` keys
from the snapshot's own `map[string]json.RawMessage`, re-encodes, writes via
a temp-file-then-rename (same crash-safety pattern `MigrateLocalAttested`
and friends already use elsewhere in this codebase) — never a partial write
visible to a concurrently-running Claude Code process.

`saveAccountSnapshot(name)`: reads `readActiveCredential()` and
`~/.claude.json`'s `oauthAccount`/`userID`, writes both snapshot files,
`chmod 600` on each. No interaction with the sync-back logic above — this is
the explicit, user-invoked "capture what's live right now" operation.

## HTTP protocol

```
POST /account/switch
Authorization: Bearer <token>
{"name": "avisoma"}

200 {"ok": true, "account": "andy@avisoma.com"}
400 {"ok": false, "code": "unknown_account", "message": "..."}
500 {"ok": false, "code": "switch_failed", "message": "..."}
```

- Registered alongside every other mutating handler:
  `mux.HandleFunc("POST /account/switch", s.switchAccount)`.
- Same bearer-token check every other handler uses (`server.go`'s existing
  `s.auth(r)`-equivalent, `subtle.ConstantTimeCompare`).
- No `session_id`-style precondition — this endpoint isn't scoped to a
  session, it's host identity level, and `switchAccount` is already
  idempotent (switching to the currently-active name is a harmless no-op:
  step 2 skips sync-back since `current == name`, step 3 re-writes the same
  bytes, step 4 re-patches the same identity).
- A request naming an unknown snapshot never touches any file — validation
  is the first thing `switchAccount` does.

## Error handling

- `switchAccount` failing partway (steps 3/4) is reported as an error, not
  silently swallowed — the CLI exits 1 with the underlying error, the
  endpoint returns 500, the TUI toasts it.
- The one sequencing guarantee: a failure can never leave you unable to get
  back to the account you were just on, because the outgoing account's
  sync-back (step 2) completes before the live credential is touched (step
  3). A failure in step 3 or 4 leaves you exactly where the previous account
  left off in its own snapshot — worst case is "re-run `account switch
  <current>` to get back," never "no snapshot exists for where you just
  were."
- `saveAccountSnapshot` failing partway (credential written, identity write
  fails, or vice versa) is reported as an error; re-running the command is
  always safe (it overwrites both files unconditionally on success).
- Snapshot reads/writes never touch `security`/Keychain on Linux and never
  touch a plain file on macOS — the platform dispatch in
  `readActiveCredential`/`writeActiveCredential` mirrors `loadOAuthToken`'s
  existing `runtime.GOOS` check exactly.

## Testing

- `switchAccount`: unknown name rejected before any file touched; sync-back
  writes the outgoing account's snapshot correctly; identity patch preserves
  unrelated `~/.claude.json` keys; missing `.{name}.account.json` skips the
  patch without erroring; switching to the already-active name is a no-op
  that still succeeds.
- `saveAccountSnapshot`: writes both files with `0600`; overwrites an
  existing snapshot; captures exactly `oauthAccount`+`userID`, nothing else.
- `currentAccountName`: matches by email against every known snapshot;
  returns `""` when no snapshot's email matches the live one.
- Endpoint: unknown name → 400 `unknown_account`; valid name → 200 with the
  new email; auth rejection matches every other endpoint's existing test
  pattern.
- CLI: `account list` renders local + remote table; a broken remote shows
  its error inline without aborting the other rows; `account switch` with
  `--server` hits the endpoint via `remoteRequest`.
- TUI: `Ctrl+W` builds the picker from already-fetched data (no fetch
  triggered by opening it); `Enter` on the active entry is a no-op that
  still closes cleanly; `Esc` makes no change; empty-snapshot host shows the
  empty state.

### Verification commands

```sh
go test ./...
go vet ./...
make
```

## Acceptance criteria

- `account switch`, `account save`, `account list` all work as specified,
  local and (switch/list) remote.
- `Ctrl+W` in the TUI switches a local or remote row's account with exactly
  picker → Enter → done, no extra confirmation step.
- The Go implementation and both existing shell scripts remain fully
  interchangeable — same file formats, same paths, no coexistence bugs.
- A switch failure never leaves the host unable to return to its previous
  account.
- No new dependency; `go vet`, full tests, and all three release-platform
  cross-builds pass.
