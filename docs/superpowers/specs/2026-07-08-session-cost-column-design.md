# Session cost column — design

Date: 2026-07-08
Status: approved (views + hardcoded pricing confirmed by user)

## Goal

Show each session's cumulative dollar cost (like Claude Code `/usage` "Total
cost") as a `COST` column in the session table, for local and remote rows.

## Data source

No precomputed cost exists on disk. Cost is derived from the session's
transcript JSONL (`~/.claude/projects/<slug>/<session-uuid>.jsonl`) plus its
Task-tool subagent transcripts (`<slug>/<session-uuid>/subagents/agent-*.jsonl`
— Claude Code writes subagent turns to separate files, so subagent usage never
appears in the parent transcript):

- Each assistant line carries `message.model`, `message.id`, and
  `message.usage` with `input_tokens`, `output_tokens`,
  `cache_read_input_tokens`, `cache_creation_input_tokens`, and (newer
  format) `cache_creation.ephemeral_5m_input_tokens` /
  `ephemeral_1h_input_tokens`.
- Dedupe repeated usage lines by `message.id` + `requestId` (streaming can
  re-emit the same usage on multiple lines). Each file has its own dedup set;
  parent and subagent files are disjoint, so no cross-file dedup is needed.
- Sum tokens per model, multiply by the pricing table.
- Cost is split by transcript source: the parent transcript's cost (the main
  loop) versus the summed cost of all subagent files.

## Pricing table (hardcoded, $/MTok)

| family prefix match | input | output | cache read | cache write 5m | cache write 1h |
|---|---|---|---|---|---|
| `claude-fable-`, `claude-mythos-` | 10 | 50 | 1.00 | 12.50 | 20.00 |
| `claude-opus-` | 5 | 25 | 0.50 | 6.25 | 10.00 |
| `claude-sonnet-` | 3 | 15 | 0.30 | 3.75 | 6.00 |
| `claude-haiku-` | 1 | 5 | 0.10 | 1.25 | 2.00 |

Longest-prefix / family match on the model id. Unknown model → that
message's tokens contribute nothing and the row's cost renders `—`? No:
render the cost of known models and ignore unknown (matches /usage
behavior closely enough); if *no* tokens priced, show `—`.

Cache-write pricing: when `cache_creation.ephemeral_5m_input_tokens` /
`ephemeral_1h_input_tokens` are present, price them at the 5m/1h rates.
Otherwise fall back to pricing all of `cache_creation_input_tokens` at the
5m rate. (Verified against a real /usage sample: opus/sonnet rows reproduce
exactly; fable rows require the 5m/1h split.)

## Architecture

- **`cost.go` (new)**: pricing table + `scanCostIncremental` (one transcript
  file) + `scanSessionCost` (parent + subagents). `scanCostIncremental`
  maintains an in-memory cache keyed by file path: `{offset int64, seen map,
  costUSD}`. On each call: stat file; if size < cached offset
  (rotation/truncation) reset; read from offset with bufio, parse only needed
  fields per line, update the running cost; store new offset. First scan reads
  the whole file once; subsequent ticks parse only appended bytes.
  `scanSessionCost` globs `<uuid>/subagents/*.jsonl` each call (new files
  appear mid-session) and runs each through the same per-path cache, returning
  `(parent, subagentsSum)`.
- **`Session.CostUSD` / `Session.CostSubagentsUSD` (float64)** new fields,
  `json:"costUsd,omitempty"` / `json:"costSubagentsUsd,omitempty"` — server
  responses carry both to remote clients automatically (server.go marshals
  `[]Session` as-is; no server change). The split is computed at collection
  time so render stays dumb and remote rows render identically client-side.
- **Enrichment**: in the same place MODEL/CTX are attached (model.go
  transcript-scan path from the in-flight uncommitted diff), also attach the
  cost pair via `scanSessionCost`. Each file's dedup `seen` map persists in its
  cache entry.
- **Render**: `COST` column in full + intermediate views only (render.go),
  right-aligned, dynamic width. Rendered as `$<parent> (+$<subagents>)` — the
  ` (+$x.xx)` suffix is omitted when the subagent part rounds under a cent.
  Each dollar figure is `$%.2f` below $100 and `$%.0f` (no cents) at $100+.
  Both parts zero → `—`.

## Error handling

- Missing transcript / unreadable file → CostUSD 0 → `—`.
- Malformed JSONL lines skipped silently.
- File shrunk → full rescan.

## Testing

- Pricing math per family incl. 5m/1h cache split (table-driven).
- Dedup by message.id+requestId.
- Incremental scan: write temp jsonl, scan, append lines, rescan — verify
  only delta parsed (offset advanced) and totals correct.
- Truncation reset.
- Subagent aggregation: parent jsonl + `<uuid>/subagents/agent-*.jsonl` layout,
  verify `(parent, subagentsSum)` split.
- Render: cost column formatting (incl. ` (+$x.xx)` suffix and its omission)
  and width in full + intermediate views.

## Out of scope

- Config-file pricing overrides.
- Minimal view column.
- Historical/aggregate cost.
