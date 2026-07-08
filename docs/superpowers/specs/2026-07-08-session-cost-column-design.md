# Session cost column — design

Date: 2026-07-08
Status: approved (views + hardcoded pricing confirmed by user)

## Goal

Show each session's cumulative dollar cost (like Claude Code `/usage` "Total
cost") as a `COST` column in the session table, for local and remote rows.

## Data source

No precomputed cost exists on disk. Cost is derived from the session's
transcript JSONL (`~/.claude/projects/<slug>/<session-uuid>.jsonl`):

- Each assistant line carries `message.model`, `message.id`, and
  `message.usage` with `input_tokens`, `output_tokens`,
  `cache_read_input_tokens`, `cache_creation_input_tokens`, and (newer
  format) `cache_creation.ephemeral_5m_input_tokens` /
  `ephemeral_1h_input_tokens`.
- Dedupe repeated usage lines by `message.id` + `requestId` (streaming can
  re-emit the same usage on multiple lines).
- Sum tokens per model, multiply by the pricing table.

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

- **`cost.go` (new)**: pricing table + `scanCostIncremental`. Maintains an
  in-memory cache keyed by transcript path: `{offset int64, seen map, per-model
  token sums, costUSD}`. On each call: stat file; if size < cached offset
  (rotation/truncation) reset; read from offset with bufio, parse only needed
  fields per line, update sums; store new offset. First scan reads the whole
  file once; subsequent ticks parse only appended bytes.
- **`Session.CostUSD float64`** new field, `json:"costUsd,omitempty"` —
  server responses carry it to remote clients automatically (server.go
  marshals `[]Session` as-is; no server change).
- **Enrichment**: in the same place MODEL/CTX are attached (model.go
  transcript-scan path from the in-flight uncommitted diff), also attach
  CostUSD via the cost scanner. Dedup `seen` map bounded to the session
  (persists in cache entry).
- **Render**: `COST` column in full + intermediate views only (render.go),
  right-aligned, formatted `$%.2f` (`$1,234` style not needed; ≥$100 →
  `$123` no cents to keep width ≤7). Empty/zero → `—`.

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
- Render: cost column formatting and width in full + intermediate views.

## Out of scope

- Config-file pricing overrides.
- Minimal view column.
- Historical/aggregate cost.
