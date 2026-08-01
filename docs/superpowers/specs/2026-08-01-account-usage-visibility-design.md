# Account Usage Visibility Design

**Date:** 2026-08-01
**Status:** Approved

## Summary

Two related, independently shippable features that both surface *which
Anthropic account* is in play, per host:

1. **Active-account label on the host heading.** Append the logged-in
   account's email, dimmed, after the existing `LOAD` field.
2. **All-known-accounts usage.** Every host already holds credential
   snapshots for every account `claude-switch` knows about (created when
   switching away from an account — see `claude-switch`'s
   `.{name}.credentials.json` / `.{name}.keychain-cred` +
   `.{name}.account.json` files). Read those snapshots directly (read-only,
   no keychain swap, no disruption to whatever is actively logged in) and
   fetch usage for every account they represent, not just whichever one
   happens to be live on a given host. The header then shows one usage line
   per distinct account across the whole fleet, regardless of where it's
   currently logged in.

Both features build on existing machinery (`HostUsage`/`renderHostHeading`
for #1, `AccountUsage`/`UsageHub`/`dedupeAccounts` for #2) and follow the
same old-server/new-client compatibility pattern used throughout this
codebase (see `2026-07-22-host-load-averages-design.md`).

## Goals

- Show, per host, which account is actively logged in — inline with the
  existing CPU/MEM/LOAD heading.
- Show rate-limit usage for every account any host holds a credential
  snapshot for, deduped by email across the whole fleet, without requiring
  that account to be the one currently logged in anywhere.
- Reuse `claude-switch`'s existing snapshot file convention as the source of
  truth for "known accounts" — no new config.
- Keep old clients/servers and new clients/servers interoperable.
- Never disrupt the live, active credential on any host — snapshot reads are
  strictly read-only.

## Non-goals

- Refreshing an expired snapshot token (no OAuth refresh-token flow). A
  snapshot that fails to fetch is shown as expired, not silently repaired.
- Changing `claude-switch` itself.
- A UI to manage or add snapshot accounts.
- Historical usage trends or alerting.

## Feature 1: active-account label on the host heading

### User-visible behavior

```text
  workstation  CPU 17%  MEM 54%  LOAD 0.42 0.55 0.61  andy@avisoma.com
```

- The email renders dimmed, after the LOAD triple, separated by two spaces
  (matching the existing field separator style).
- When the account is unknown (email unreadable), nothing is appended — no
  placeholder, no dash. This matches how `dedupeAccounts` already treats an
  unknown local-part today.

### Data flow

- `section` gains an `account string` field (full email, `""` if unknown).
- `buildSections` gains a `localAccount string` parameter; the local section
  gets it directly, each remote section pulls `RemoteResult.Usage.Account`
  (already fetched by the existing usage poller — no new network calls, no
  protocol change for this half of the work).
- In `BuildTableFrame`, the existing `localAU` computation (today located
  after the `buildSections` call) moves above it so the value is available
  in time.
- `renderHostHeading` appends `"  " + dim(sec.account)` when non-empty.

## Feature 2: all-known-accounts usage

### User-visible behavior

The header's account usage section (today: one bar per distinct
*currently-logged-in* account) grows to include every known account, even
ones logged in nowhere at the moment:

```text
avisoma  5h ▓▓▓░░ 41%   7d ▓▓░░░ 22%
trecs    5h ░░░░░ auth expired
```

- An account whose latest fetch failed (expired/invalid snapshot token)
  renders as a dim placeholder line — `auth expired` — instead of numbers,
  rather than being hidden or showing stale cached numbers. It stays visible
  so its existence is a reminder to `claude-switch` into it again.
- Ordering and labeling (email local-part, promoted to full email on
  collision) follow the existing `dedupeAccounts` rules.

### Snapshot discovery

Reuses `claude-switch`'s own naming convention — no new config:

- macOS: `~/.claude/.<name>.keychain-cred` (plain file holding the same JSON
  blob `security find-generic-password -w` would print).
- Linux: `~/.claude/.<name>.credentials.json`.
- Email per snapshot: `~/.claude/.<name>.account.json`'s
  `.oauthAccount.emailAddress` (same shape `loadAccountEmail` already reads
  from the live `~/.claude.json`).

A glob (`filepath.Glob(".claude/.*.credentials.json")` /
`.claude/.*.keychain-cred`) cannot collide with the live credential file on
either platform — verified: the live file has no `<name>` segment
(`.credentials.json` on Linux, and on macOS the live credential isn't a file
at all, it's a Keychain item).

### Why the active account is never read from its own snapshot

A snapshot file is only refreshed by `claude-switch` at switch time. An
account can stay logged in on a host for days while Claude Code silently
rotates its access token in place (Keychain item or live
`.credentials.json`) — the snapshot copy doesn't track that rotation and can
go stale/expired even while the live session is completely healthy. So:

- The currently-live account on a host is **always** fetched via the
  existing live path (`loadOAuthToken` / `loadAccountEmail` — unchanged,
  still feeds the existing singular `Usage` field).
- Every *other* known snapshot name is fetched using that snapshot's own
  stored token — read-only, no keychain/file mutation.
- A snapshot is skipped (not double-fetched) when its stored email matches
  that host's own live email — it's already covered by the live fetch.
  Matching is by email equality, not by hardcoding domain-to-name mappings
  in Go (unlike `claude-switch`'s shell `domain_for`) — the email is already
  available in `.<name>.account.json`, so no extra mapping is needed.

### Data model

```go
// KnownAccountUsage is one non-live known account's rate-limit snapshot,
// read from its claude-switch credential snapshot rather than the host's
// active login. Expired distinguishes "known account, last fetch failed"
// from "never attempted" so the header can show a placeholder instead of
// silently dropping the account.
type KnownAccountUsage struct {
	Name    string     `json:"name"`    // snapshot name, e.g. "avisoma"
	Account string     `json:"account"` // email, "" if account.json missing/unreadable
	Info    *UsageInfo `json:"info"`    // nil when Expired
	Expired bool       `json:"expired"`
}
```

`RemoteResult` and the local snapshot equivalent gain an additive field:

```go
// KnownAccounts lists usage for every other account this host holds a
// claude-switch credential snapshot for (excludes whichever account is
// currently live — that's still reported via Usage). Nil from older
// servers or hosts with no snapshots.
KnownAccounts []KnownAccountUsage `json:"knownAccounts,omitempty"`
```

The existing `Usage *AccountUsage` field is untouched — feature 1 and all
existing behavior keep working byte-identical when `KnownAccounts` is absent.

### Token/email loading

`loadOAuthToken`'s JSON-parsing body is extracted into a shared helper so
both the live path and the snapshot path use identical parsing:

```go
func parseOAuthCredentials(data []byte) (string, error) // shared parse, was inline in loadOAuthToken
func snapshotToken(name string) (string, error)         // reads the snapshot file, parses via parseOAuthCredentials
func snapshotAccountEmail(name string) string            // reads .<name>.account.json, mirrors loadAccountEmail
func snapshotAccountNames() ([]string, error)             // globs snapshot files, strips the platform-specific fixed parts
```

`fetchKnownAccountUsage(name, liveEmail string) (*KnownAccountUsage, bool)`
— the bool reports whether this name was skipped as covering the live
account (so the caller doesn't emit a redundant entry). On any read/parse/
HTTP error, returns `{Name: name, Account: snapEmail, Expired: true}` rather
than an error — this is best-effort background enrichment, never a reason to
fail `/sessions` or the local usage poller.

### Polling

A new poller alongside `UsageHub`, same `usagePoller[T]` generic mechanism,
`T = []KnownAccountUsage`:

```go
type KnownAccountsHub = usagePoller[[]KnownAccountUsage]
func NewKnownAccountsHub() *KnownAccountsHub
```

- Fetches all non-live snapshot accounts in parallel (small, fixed fanout —
  bounded by however many accounts `claude-switch -l` would list).
- Same 2-minute refresh interval and failed-fetch backoff as `UsageHub`.
- Disk cache follows the same envelope pattern as `usageCachePath`, separate
  file (`claude-sessions-known-accounts-<uid>.json`), same
  `usageCacheMaxAge` bound. Only successful (non-`Expired`) entries are
  persisted, so a restart during a throttle can still seed a recently-good
  account from cache, but never resurrects a previously-expired account as
  if it were fresh — an `Expired` account always starts as "no data yet"
  after a restart and waits for the next live fetch to decide its state.
- Runs on the server (for remote hosts) and locally (client's own snapshots),
  mirroring how `UsageHub` already runs in both roles.

### HTTP protocol

`GET /sessions` gains `knownAccounts` alongside the existing `usage` field:

```json
{
  "usage": { "account": "andy@avisoma.com", "info": { "...": "..." } },
  "knownAccounts": [
    { "name": "trecs", "account": "andy@trecs.aero", "info": null, "expired": true }
  ]
}
```

- Old servers omit `knownAccounts`; `FetchRemote` decodes it as nil — client
  falls back to today's live-only display for that host, same as an old
  server omitting `loadAverage` falls back to `LOAD -- -- --`.
- Absence never changes HTTP status or affects `Sessions`/`HostUsage`/`Usage`
  decoding.

### Client-side merge (`dedupeAccounts`)

Two-pass, preserving today's first-occurrence-wins behavior for live
accounts entirely unchanged:

1. **Pass 1 (unchanged).** Build the live-account line list exactly as
   `dedupeAccounts` does today — local first, then remotes in config order,
   deduped by lowercased email.
2. **Pass 2 (new).** Flatten every host's `KnownAccounts` (local's own
   snapshot poll first, then each remote's, in config order). For each
   entry, skip it if its email already appears in the pass-1 line list
   (a live copy elsewhere is always preferred over any host's snapshot copy
   of the same account — this is what correctly handles account X being
   live on host B while host A's stale snapshot also mentions X). Dedupe
   remaining entries among themselves by lowercased email, first occurrence
   wins. Unknown-email entries (`account.json` missing/unreadable) key by
   `name` instead, same spirit as today's per-host keying for unknown live
   accounts.
3. Append pass-2 survivors to the pass-1 line list as additional
   `accountUsageLine`s. An `Expired` entry carries `info: nil` — the
   existing render path already treats nil `Info` as "no bar data"; it's
   extended to print `auth expired` (dim) instead of just omitting the line,
   since these lines are intentionally never dropped (unlike live-account
   pass-1 entries, which still drop on nil `Info` — no behavior change
   there).

## Error handling

- A snapshot read/parse/HTTP failure never fails `/sessions`, the TUI
  render, or any other feature — it only marks that one `KnownAccountUsage`
  as `Expired`.
- No raw error text is shown in the UI; `Expired` is a boolean, not an error
  string (mirrors `HostUsage`'s nil-means-unavailable convention).
- A host with zero snapshot files (fresh machine, no `claude-switch` setup)
  reports an empty `KnownAccounts` list — same as today's behavior, not an
  error.

## Testing

- `parseOAuthCredentials` extraction: existing `loadOAuthToken` tests keep
  passing unchanged; add direct tests for the shared parse function (valid,
  missing token, malformed JSON).
- `snapshotAccountNames`: glob matches `.avisoma.credentials.json` /
  `.avisoma.keychain-cred`, does not match the bare live file, handles zero
  snapshots.
- `fetchKnownAccountUsage`: skips when snapshot email matches live email;
  returns `Expired: true` on HTTP/parse failure; returns populated `Info` on
  success.
- `dedupeAccounts` pass 2: known-account entry deduped away when a pass-1
  live entry already has that email; survives when no live entry matches;
  two hosts' snapshots for the same email collapse to one; expired entries
  render as a placeholder rather than being dropped.
- Protocol: `knownAccounts` round-trips through `/sessions`; an old-server
  response without it decodes to nil and renders exactly as today.
- Feature 1: `renderHostHeading` includes the dimmed email when
  `sec.account != ""`, omits it when `""`, across all three view modes.

### Verification commands

```sh
go test ./...
go vet ./...
make
```

## Acceptance criteria

- Every host heading shows its active account's email, dimmed, after LOAD
  (or nothing, if unknown) — no protocol change for this half.
- The header's account usage section shows one line per distinct known
  account across the whole fleet, whether or not that account is currently
  logged in anywhere.
- An account with a currently-unusable snapshot token shows as a dim "auth
  expired" placeholder, never silently disappears and never shows stale
  numbers as if they were current.
- No feature here ever mutates a live credential (Keychain item or
  `.credentials.json`) or triggers a Keychain permission prompt beyond the
  one the existing live-usage fetch already causes.
- Old and new client/server combinations remain interoperable.
- Full tests, vet, and release-platform cross-builds pass; no new
  dependency.
