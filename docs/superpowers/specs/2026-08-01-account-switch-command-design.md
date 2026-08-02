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

**Correction from the first draft of this spec:** it originally claimed the
Go port would mirror "the same ordering both existing scripts already use"
for syncing the outgoing credential back to its snapshot before switching.
An independent review (see repo history / session notes around this spec's
approval) checked the actual installed macOS script and found it did
*not* back up the outgoing Keychain credential blob before overwriting it —
only the identity JSON — meaning a switch away from an account whose
snapshot had gone stale (token rotated since last capture) could discard the
only live copy with nothing to fall back to. That macOS script has since
been fixed directly (it now captures the outgoing credential the same way
the Linux script's `cp -p "$dst" "$HOME/.claude/.${current_name}.credentials.json"`
step already did) — so the shell scripts and this spec's Go design are now
in genuine agreement, but that agreement was verified, not assumed.

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
- Sync the outgoing account's credential + identity back to its own snapshot
  before switching, patch the `~/.claude.json` identity cache
  (`oauthAccount`/`userID`) so no manual `/login` is needed when a snapshot
  exists, plus an unconditional rescue backup of whatever's live (see Core
  logic) so a switch can never discard a credential with zero copy left
  anywhere.

## Non-goals

- Refreshing an expired snapshot's token (no OAuth refresh flow) — same
  non-goal as the usage-visibility spec.
- Retiring the shell scripts.
- A confirm-before-switch dialog (explicitly decided against — picker +
  Enter is the whole interaction).
- Adding a `jq` dependency. The identity-cache patch is done with
  `encoding/json` only, same "stdlib + golang.org/x/term + golang.org/x/sys
  only" constraint the rest of the codebase holds to.

**Second correction, post-implementation:** this spec's first draft (Core
logic, old step 6) let `switchAccount` proceed against a credential-only
snapshot (no `.account.json`), skipping the identity patch and leaving
`~/.claude.json` pointing at the OUTGOING account — matching what the
standalone shell scripts already tolerate. Independent review during
implementation found this genuinely corruptible: `currentAccountName` has no
way to detect that split state after the fact, so a LATER switch would read
the stale identity, misidentify which snapshot is outgoing, and silently back
the wrong live credential up under the wrong account's name. A "last switch"
marker file was built to patch over it and rejected on the same review pass:
it made the wrong snapshot look current whenever anything else rewrote
`~/.claude.json`, which Claude Code does constantly for reasons unrelated to
identity. The fix that shipped instead is a precondition, not a patch:
`switchAccount` now refuses a credential-only target outright (step 2, naming
`account save <name>` as the remedy) before touching anything. Both real
machines this ships to already have an identity snapshot for every credential
snapshot (verified during review), so the precondition costs nothing in
practice — Core logic and the CLI/TUI/HTTP sections below reflect this
corrected behavior throughout.

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
`knownAccounts` + `activeSnapshotName` fields (all already carry
name/email/active-ness per the usage-visibility spec) — **no second new GET
endpoint**, this reuses transport that spec already adds.

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
`KnownAccountsHub` snapshot + `localAU` + local `activeSnapshotName`; a
remote row's `RemoteResult.Usage`/`.KnownAccounts`/`.ActiveSnapshotName`) —
no fetch triggered by opening the picker. Visual style matches the existing
small overlays (bordered box, arrow-key nav) rather than the full searchable
resume picker — account counts are small (2–3 typically), no search needed.

- Current account is marked using `activeSnapshotName` (e.g. a leading `●`)
  — this is why spec 1 reports that field explicitly rather than making
  every consumer re-derive it from an email match.
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
func withAccountLock(fn func() error) error               // advisory flock, see Concurrency below
func switchAccount(name string) (email string, err error)
func saveAccountSnapshot(name string) error
func patchIdentityCache(accountJSON map[string]json.RawMessage) error // merges oauthAccount+userID into
                                                                        // ~/.claude.json, plain encoding/json
```

`switchAccount(name)`, run entirely inside `withAccountLock` (see
Concurrency):

1. Validate `name` against `snapshotAccountNames()`; error listing available
   names if unknown. **Nothing is read or written past this point if
   invalid.**
2. Require `.{name}.account.json` to exist, parse, AND carry a real
   (non-null, non-empty, exact-key) email; refuse (naming `account save
   <name>` as the fix) if any of that fails. **Nothing is read or written
   past this point if it fails.** (Revised from this spec's first draft,
   which allowed a credential-only target and silently skipped the identity
   patch — see the correction note below the Non-goals section. The non-null
   requirement closes a follow-on gap: `identitySlice` writes an explicit
   JSON `null` for an absent key, jq parity with the shell scripts, so a
   snapshot saved while not actually logged in is syntactically valid but
   practically empty. The "exact-key" requirement closes a second follow-on
   gap: the check must read `oauthAccount` by the identical exact-key lookup
   `patchIdentityCache` itself uses on the same parsed map — a looser,
   case-insensitive check (as a struct-based unmarshal would give) could pass
   a snapshot whose casing doesn't match what the patch step actually looks
   up, silently applying nothing while looking like it succeeded.)
3. Refuse if the pending-switch marker (step 6.5 below) is armed — a
   previous switch was interrupted between installing the credential and
   patching identity, and retrying blind risks corrupting whichever snapshot
   the (now stale) identity cache misidentifies as outgoing. `account save
   <name>` also clears this marker (capturing what's live right now under a
   name is exactly the confirmation it's waiting for), so recovery is one
   command, not a resync-then-remember-to-clear two-parter.
4. `current := currentAccountName()`. **If `current == name`, return
   immediately** — a true no-op, touching zero files. (First draft of this
   spec called re-writing the same bytes on every call "harmless"; it isn't:
   the live credential can have refreshed since the target's snapshot was
   last captured, so blindly re-applying the snapshot would silently
   overwrite a fresher live token with a stale one. Skipping entirely is the
   only version of "no-op" that's actually safe.)
5. **Unconditional rescue backup**, regardless of whether `current` resolved
   to a known name: `readActiveCredential()` → write to
   `.last-switch-rescue.<ext>` (a single rolling slot, not per-name). This
   is the fix for the case `current == ""` (live email doesn't match any
   known snapshot — first-ever switch, a renamed/unrecognized account, or a
   prior partial failure): without an unconditional copy, that live
   credential would be discarded in step 7 with no backup anywhere, because
   step 6's named sync-back only fires when `current` resolved to a name.
6. If `current != ""`: sync the outgoing account back to its own named
   snapshot — `readActiveCredential()` → write to `.{current}.<ext>`; slice
   `~/.claude.json`'s `oauthAccount`/`userID` → write to
   `.{current}.account.json`. **Steps 5–6 must complete before step 7
   touches the live credential** — the live credential is never overwritten
   until it's been captured at least once (step 5) and, when its owning
   account is known, captured by name too (step 6).
6.5. Arm the pending-switch marker, naming `name`. This is the mitigation for
   the one gap that can't be closed by ordering alone: steps 7 and 8 are two
   separate writes to two separate stores (Keychain/credential file, then
   `~/.claude.json`), so nothing can make them atomic together — a crash
   between them leaves exactly the split state step 2 exists to prevent, just
   reached via interruption instead of a missing file. Best-effort (a failed
   arm is no worse than not having this mitigation, so it doesn't block the
   switch), but always attempted.
7. Read `.{name}.<ext>`, write it via `writeActiveCredential` (the actual
   account switch).
8. `patchIdentityCache` the identity snapshot already read and parsed at
   step 2 into `~/.claude.json` — no `/login` needed. Guaranteed to exist,
   parse, and carry a real email by step 2, so this cannot fail for a reason
   step 2 didn't already catch before anything was written.
8.5. Disarm the pending-switch marker — both halves of the pair it guards
   completed, so there is nothing left to detect.
9. Return the new active email (re-read via `loadAccountEmail()` after the
   patch, so the return value reflects what's actually on disk, not what was
   assumed).

`patchIdentityCache` decodes `~/.claude.json` into a
`map[string]json.RawMessage` (preserving every field this codebase doesn't
otherwise touch), overwrites exactly the `oauthAccount` and `userID` keys
from the snapshot's own `map[string]json.RawMessage`, re-encodes, writes via
a temp-file-then-rename in the same directory (same crash-safety pattern
`SessionStore.save` in `state.go` already uses — `os.CreateTemp` +
`os.Rename`, same-directory so the rename is same-filesystem and atomic) —
never a partial write visible to a concurrently-running Claude Code process.

### Concurrency

`withAccountLock` wraps the whole `switchAccount`/`saveAccountSnapshot` body
in an advisory lock via `golang.org/x/sys/unix.Flock` on
`~/.claude/.account-switch.lock` (`LOCK_EX`, blocking) — no new dependency,
`x/sys/unix` is already imported elsewhere in this codebase (`remote.go`).
This serializes concurrent `claude-sessions account switch`/`save`
invocations (including a concurrent standalone `claude-switch` shell
invocation, if it's updated to take the same flock — tracked as a follow-up,
not blocking this spec, since today's shell scripts have never had a lock
either and this doesn't make that any worse).

**What this does not cover, and why that's acceptable:** a live Claude Code
process silently refreshing/rewriting the active credential *during* the
brief window between this tool's step 5 rescue-backup read and step 7's
overwrite. This tool cannot lock a process it doesn't control. The residual
exposure is small and non-destructive to *this* switch: worst case, the
rescue/named backup captures a token that was about to be superseded by a
fresher refresh — a slightly-stale-but-still-valid backup, not data loss —
because the backup read always happens strictly before this tool's own
overwrite. This is the same category of accepted, documented residual
window this codebase already carries for kill/migrate preconditions (see
CLAUDE.md's "Known residual windows, accepted").

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
  idempotent: switching to the currently-active name returns immediately at
  step 4 without touching any file (see Core logic — a real no-op, not a
  harmless re-write).
- A request naming an unknown snapshot never touches any file — validation
  is the first thing `switchAccount` does.

## Error handling

- `switchAccount` failing partway (steps 7/8) is reported as an error, not
  silently swallowed — the CLI exits 1 with the underlying error, the
  endpoint returns 500, the TUI toasts it.
- A missing, unparseable, or empty-of-email identity snapshot (step 2) fails
  before anything is read or written — the same "nothing touched" guarantee
  as the unknown-name check in step 1. A pending-switch marker from a prior
  interrupted switch (step 3) fails the same way, for every target, not just
  a retry of the interrupted one.
- The sequencing guarantee: a failure can never leave you unable to get back
  to the account you were just on, because the live credential is captured
  at least once (step 5's unconditional rescue backup) and, when its account
  is known, captured by name too (step 6) — both before step 7 touches the
  live credential. A failure in step 7 or 8 leaves you exactly where the
  previous account left off, in either its own named snapshot or, failing
  that, `.last-switch-rescue.<ext>` — worst case is "re-run `account switch
  <current>`" (or manually restore from the rescue file), never "no copy
  exists anywhere of where you just were." A failure specifically *between*
  steps 7 and 8 (the one pair that can't be made atomic — see step 6.5) is
  the pending-switch marker's job, not the rescue backup's: it stops the next
  switch, of any target, from acting on a stale identity cache rather than
  trying to reconstruct which account is actually live.
- `saveAccountSnapshot` failing partway (credential written, identity write
  fails, or vice versa) is reported as an error; re-running the command is
  always safe (it overwrites both files unconditionally on success).
- Snapshot reads/writes never touch `security`/Keychain on Linux and never
  touch a plain file on macOS — the platform dispatch in
  `readActiveCredential`/`writeActiveCredential` mirrors `loadOAuthToken`'s
  existing `runtime.GOOS` check exactly.

## Testing

- `switchAccount`: unknown name rejected before any file touched; a
  credential-only target (missing `.{name}.account.json`) is refused before
  any file is touched, naming `account save <name>` in the error; a
  syntactically-valid-but-null identity snapshot (`{"oauthAccount":null,...}`)
  is refused the same way; a pending-switch marker armed by a prior
  interrupted switch refuses EVERY target, not just a retry of the
  interrupted one, and leaves the tree untouched; switching to the
  already-active name touches zero files (verify via mtime/no-write
  assertion, not just a successful return); a clean switch disarms the
  pending-switch marker; the unconditional rescue backup is written even when
  `current == ""`; named sync-back writes the outgoing account's snapshot
  correctly when `current` resolves; identity patch preserves unrelated
  `~/.claude.json` keys; a live-credential read failure that is genuinely
  "not found" (exit 44 + matching stderr on macOS, `os.ErrNotExist` on Linux)
  proceeds without a rescue copy, while any other read failure refuses the
  switch outright, including when `current == ""`.
- Concurrency: two overlapping `switchAccount` calls (goroutines/subprocesses
  racing on the same lock file) serialize rather than interleave — assert
  the second observes the first's fully-applied state, not a torn write.
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
  triggered by opening it), using `activeSnapshotName` to mark current;
  `Enter` on the active entry is a true no-op (no request sent, or a request
  that itself no-ops server-side) that still closes cleanly; `Esc` makes no
  change; empty-snapshot host shows the empty state.

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
  account, including when the outgoing account can't be identified by name
  (unconditional rescue backup covers that case).
- Switching to the already-active account is a true no-op: zero files
  touched, never a fresh live token overwritten by a stale snapshot.
- A credential-only or null-identity snapshot is refused with an actionable
  error, never silently applied — `currentAccountName`'s email matching stays
  correct for every account this tool has ever switched to.
- A switch interrupted between installing the credential and patching
  identity leaves the pending-switch marker armed, which refuses every
  subsequent switch (any target) until a human confirms which account is
  actually live — never a silent misattributed backup on the natural retry.
- A "not found" classification for the outgoing live credential (safe to
  proceed without a rescue copy) requires corroborating evidence beyond a
  single ambiguous exit code, on both platforms.
- Concurrent switch/save invocations on the same host serialize via
  `withAccountLock` rather than racing.
- No new dependency (the concurrency lock uses `golang.org/x/sys/unix`,
  already a dependency of this codebase); `go vet`, full tests, and all
  three release-platform cross-builds pass.
