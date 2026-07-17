# Running agents metric: AGENTS column + header grand total

Date: 2026-07-17
Status: approved design, pre-implementation

## Goal

Show how many subagents each Claude Code session is currently running (including
nested subagents-of-subagents), and a grand total of concurrent agent loops
across all sessions (local + remote) in the TUI header.

## Background: available signals

Verified on-disk signals (macOS, current Claude Code build):

- `~/.claude/projects/<slug>/<sessionUuid>/subagents/agent-*.meta.json` — one
  per spawned subagent. Fields: `agentType`, `description`, `toolUseId`,
  `spawnDepth`, `model`. Proves *spawned*, not *running*. Cheap dir listing.
- Transcript JSONL (parent session and each subagent's own `agent-*.jsonl`) —
  a subagent spawn is an `Agent` tool_use block (this build names the tool
  `Agent`, not `Task`); completion produces a `tool_result` with the matching
  `tool_use_id`. Unmatched tool_use = not finished. Verified 14/14 matched on a
  completed session.
- Non-signals: `~/.claude/sessions/<pid>.json` has no agent fields; subagents
  run in-process (no child PIDs); `~/.claude/tasks/` holds only lock files.

## Detection: hybrid rule

A subagent counts as **running** iff both:

1. Its spawning `Agent` tool_use has **no matching `tool_result`** in the
   transcript that spawned it (authoritative "not finished"), and
2. Its own transcript (`subagents/agent-*.jsonl`) mtime is **within 5
   minutes** (guards against phantom counts from crashed/killed sessions where
   the tool_result never lands; a live agent on a long tool call stays fresher
   than that, and crash-stale files are hours old).

Nesting: recurse — each running subagent's own transcript is scanned for its
unmatched `Agent` tool_use blocks the same way; nested running agents count
into the session total. `spawnDepth` from meta.json is available but not
displayed (total only).

## Data flow

- `Session` gains `AgentsRunning int` (JSON-tagged), placed beside
  `CostSubagentsUSD` (session.go:38).
- Computed during local enrichment at the same call site as cost
  (session.go:146 — `findTranscript` → `cachedMeta` → `scanSessionCost`).
- Scanning piggybacks on the existing incremental machinery:
  `scanSessionCost` (cost.go:184) already walks `subagents/*.jsonl`, and
  `scanCostIncremental` (cost.go:160) already caches per-file offsets. Extend
  that scan to also track the set of `Agent` tool_use ids seen and which have
  a matching tool_result, per file, in the same pass — near-zero cost over
  what the COST column already pays. mtime comes free from the same walk.
- Remote: the server enriches sessions in-process via `CollectLocal`, so the
  exported field rides the existing JSON response automatically; the client's
  decode picks it up because remote rows share the `Session` type. No API
  shape change beyond the new field.

## Display

### AGENTS column

Right-aligned column between COST and CTX. Shows the running count for the
session; **blank when zero** (keeps the table quiet). Hidden in minimal-width
adaptive mode like other secondary columns.

```
NAME          MODEL   COST    AGENTS  CTX
my-feature    fable   $1.20   3       45%
other-work    opus    $0.15           12%
```

### Header grand total

Header line shows total concurrent agent loops: every *alive* session counts
as 1, plus all running subagents (incl. nested), summed across local and all
reachable remote hosts:

```
claude-sessions   9 agents (4 sessions + 5 sub)
```

Alive = sessions currently listed as running (same liveness the table already
uses); dead/stale sessions contribute nothing. When zero subagents anywhere,
degrade to just the session count (`4 agents (4 sessions)`). Unreachable
remotes are simply excluded (same as their rows).

## Testing

- Unit tests for the matching logic: unmatched tool_use → running; matched →
  not running; nested (running child inside running parent) → both counted;
  stale mtime (> 5 min) → excluded despite unmatched tool_use.
- Fixture-based, alongside existing cost tests (same fixture style).
- Render test: AGENTS column blank-at-zero, header total formatting including
  the zero-subagents degraded form.

## Out of scope

- Per-agent detail view (names/models of running subagents).
- Depth breakdown in the column (e.g. `3+2`) — total only.
- Historical counts; this is a live metric only.
