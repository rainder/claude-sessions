# Design: cross-process usage-fetch coalescing + stale-age display

## Problem

Two independent gaps in the usage-polling subsystem (see CLAUDE.md's "Usage
polling" section for full background):

**1. Duplicate fetches across processes.** `newUsagePoller[T]` (usage_hub.go)
fires an immediate fetch on start (`h.Kick()`) and then re-fetches every
`usageRefreshInterval` (2 minutes) on its own ticker. This poller is
instantiated once per running `claude-sessions` client process — both
`UsageHub` (live account) and `KnownAccountsHub` (other known accounts). If a
user runs two `claude-sessions` processes on the same host under the same OS
user at the same time, each runs its own uncoordinated poller: no lock, no
phase offset, no shared in-memory state. Result: up to Nx request volume
against the same Anthropic account's shared per-token usage-endpoint budget,
worst right at a second process's launch (both fire an immediate `Kick()`
with no jitter). This endpoint already 429s readily under single-process
load — the whole subsystem exists to avoid amplifying that.

**2. No staleness age shown.** When a fetch fails and prior numbers are
carried forward, the header renders a fixed dim label `"stale"`
(`usageStaleText`, render.go) next to the bars — no indication of how stale:
30 seconds old reads identically to 3 hours old. `AccountUsage.FetchedAt` and
`KnownAccountUsage.FetchedAt` already carry this timestamp end-to-end
(persisted in `accountCacheEntry`, correctly preserved — not restamped — on
carry-forward) but `dedupeAccounts`'s `add`/`addKnown` closures only pass
`info *UsageInfo` into `accountUsageLine` — `FetchedAt` is dropped before it
reaches the renderer. usage.go already flags this as a known gap.

## Existing mechanism this builds on

`newUsageFetcher` (usage.go) and `newKnownAccountsFetcher`'s per-account loop
(known_accounts.go) already reload each account's on-disk cache file fresh on
every single pass — no in-memory state is kept across passes for a resolvable
account (a deliberate prior redesign; see account_cache.go's doc comments).
The cache is `accountCacheEntry`, one JSON file per account
(`accountCachePath`, keyed by claude-switch snapshot name + uid), written via
`writeFileAtomic` (temp-file-then-rename, no lock — this codebase's existing,
documented, accepted stance: "best-effort, whoever writes last wins, atomic
rename only rules out a torn read not a lost write"). `loadAccountCache`
unconditionally marks a loaded `Info` as `Stale = true` — a disk read is never
itself proof the numbers are current.

## First design + independent review

An initial version of Fix 1 proposed: right after loading the cache, if
`matches && cached.Info != nil && FetchedAt` is within a coalescing window,
short-circuit and serve it as fresh — inserted *before* the existing
backoff-due check. This was reviewed via `ask-codex` (Codex, high effort) and
returned **DISAGREE** with five holes, all verified directly against the code
before accepting:

1. **Self-throttle bug.** The ticker starts right after `Kick()` fires
   (usage_hub.go), so even a *lone* process's own next natural tick lands
   with `now - FetchedAt` just under the proposed window — it would coalesce
   against its own prior fetch, silently halving its own poll rate.
2. **Bypasses an armed backoff.** Placed before the due() check, a freshly
   armed wait (from a real failure) could be silently ignored if `FetchedAt`
   from an earlier success still fell inside the window.
3. **Breaks the wrong-identity safety model.** Didn't consult `Verified` — a
   deliberately-stale wrong-identity carry (non-nil info, matching claimed
   email, recent timestamp) could be promoted to "fresh," defeating the
   `Verified`-gating CLAUDE.md documents at length.
4. **Misses unsnapshotted accounts.** A live account with no claude-switch
   snapshot (`name == ""`) never reaches the per-name disk path at all — a
   documented, common setup.
5. **No lower bound.** A future-dated or corrupted `FetchedAt` could suppress
   fetching indefinitely; nothing clamps it the way backoff deadlines are.

Fix 2 was accepted with caveats (below).

## Revised design (this spec)

### Fix 1 — read-before-fetch coalescing, corrected

- New constant `usageCoalesceWindow = usageRefreshInterval / 2` (60s) in
  usage.go — deliberately *shorter* than the poll interval so a lone
  process's own next tick (~120s later) always falls outside it and is
  never affected. This only has a chance to fire when two processes' fetches
  land close together in time — the launch-burst case, and any steady state
  where two long-running processes' ticker phases happen to stay close.
- The check is inserted **inside** the existing `if backoff.due(now)`
  (i.e. the branch that was already about to make a real network call), not
  before it — an armed backoff is untouched; this only ever replaces "I'm
  about to fetch" with "someone already has, skip the call."
- It reuses the **existing** `liveCarryable`/`carryable` eligibility check
  (already identity-gated) rather than a new ad-hoc
  condition — this closes hole 3 for free: anything not safe to carry
  forward today is not safe to coalesce onto either. To be precise about
  what that gate is and is not: neither `carryable` nor `liveCarryable`
  consults `Verified` — they test numbers-present plus claimed-identity
  equality only. `Verified` gates one narrower path, the wrong-identity
  carry inside `knownAccountUsage`'s `failed` helper, which coalescing does
  not go through. So coalescing inherits exactly the ordinary carry-forward
  path's trust, no more; the consequence (a wrong-identity mismatch can be
  masked for up to one window) is bounded and self-healing.
- Additional guard: `!cached.FetchedAt.After(now)` — reject a future-dated
  entry (closes hole 5).
- Net condition, evaluated only when `backoff.due(now)` is true: eligible
  via `liveCarryable(last, live)` (or `carryable` for known-accounts) AND
  `!cached.FetchedAt.After(now)` AND `now.Sub(cached.FetchedAt) <
  usageCoalesceWindow`. On match: return a copy of `last` with `Stale`
  forced `false` (this process's confidence is as good as a fetch it did
  itself — the numbers are seconds old) and the same `FetchedAt`. No network
  call, no re-persist (disk already reflects it). Still call
  `mirrorFallback` so the in-memory fallback vars stay consistent.
- Mirrored into `newKnownAccountsFetcher`'s per-account loop
  (known_accounts.go) with the same gating.
- **Accepted gap, not fixed here (hole 4):** unsnapshotted live accounts
  (`name == ""`) have no shared disk slot at all today — the `fbXXX`
  in-memory fallback is per-process by construction. Giving that case
  cross-process coalescing means giving it a shared disk slot, which is a
  larger change than this fix's scope. Documented, not solved.

### Fix 1b — hard floor on backoff (new, requested directly by user)

`usageBackoff.due(now)` (usage.go) currently returns `!now.Before(nextAttempt)`.
Change to `!now.Before(nextAttempt.Add(usageBackoffSafetyMargin))`, with
`usageBackoffSafetyMargin = time.Minute`. `due()` is the single shared gate
already used by every real fetch call site (live-fetcher's snapshot path,
its unresolvable-identity fallback, and the known-accounts loop), so this
one change enforces "never call the endpoint earlier than
`backoff_next_attempt + 1 minute`" everywhere, including for any fetch that
the coalescing check above did not intercept. A zero `nextAttempt` (no
backoff armed) is unaffected — the margin only pushes out a wait that is
already armed.

### Fix 2 — stale age in header

- Add `FetchedAt time.Time` to `accountUsageLine` (render.go). Thread it
  through from `dedupeAccounts`'s `add` (local + remote call sites, from
  `AccountUsage.FetchedAt`) and `addKnown` (from `KnownAccountUsage.FetchedAt`).
- In `writeUsageHeader`'s `addClaude` closure, where `a.stale` currently
  renders the bare `usageStaleText`, compute
  `age := formatAge(now.Sub(a.fetchedAt).Seconds())` (reusing the existing
  `formatAge` helper, which already produces compact `"12m"` / `"2h"` /
  `"3d"` strings) and render `usageStaleText+" "+age` — keeping the word
  `"stale"` as a component rather than replacing it outright, per review
  feedback. Recompute `suffixW` from the resulting string's length.
- `now` is captured once per `writeUsageHeader` call (not per line), so
  multiple stale lines in the same render agree on their age relative to a
  single instant.
- Guard a zero `FetchedAt` (a seed path can produce a `Stale` entry without
  one — `seedKnownAccounts`'s zero-guard is the precedent): a zero
  `FetchedAt` renders the bare `"stale"` word with no age, same as today,
  rather than a nonsensical multi-decade duration.
- Verify at narrow terminal widths that the longer suffix still gets
  accounted for in `lineBarW`/`usageLineFixedWidth` sizing and is not
  clipped by `cropTableFrame` — render.go's own history documents a real
  prior bug from getting stale-marker sizing wrong.

## Testing

- `usage_test.go` / `known_accounts_test.go`: coalescing fires when due,
  eligible, and within window (no `fetch()` call observed via the seam);
  does not fire when backoff is armed (due()==false); does not fire when not
  eligible (carryable/verified check fails); does not fire when
  `FetchedAt` is outside the window or future-dated; a lone process's own
  next natural tick still fetches (regression pin for hole 1).
- `usage_test.go`: `due()` respects the `+1min` floor — a `nextAttempt`
  exactly `now` is not due; `now - nextAttempt >= 1min` is due.
- `render_test.go`: a stale line renders `"stale <age>"` with the correct
  compact duration; a zero `FetchedAt` renders bare `"stale"`; width/sizing
  test at a narrow terminal width confirms the marker is not clipped.

## Out of scope

- Cross-process coalescing for unsnapshotted live accounts (hole 4, accepted
  gap).
- A true cross-process lock (flock) — considered and rejected by the user in
  favor of this disk-read-based, no-lock approach, consistent with the
  codebase's existing philosophy for this file.
