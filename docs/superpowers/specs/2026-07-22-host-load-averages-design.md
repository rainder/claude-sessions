# Host Load Averages Design

**Date:** 2026-07-22
**Status:** Approved

## Summary

Extend the whole-host resource heading with htop-style raw load averages. This
is a follow-on to the CPU/MEM feature (see
`2026-07-21-host-resource-usage-design.md`); it reuses the same `HostUsage`
transport, the same single cached `HostUsageHub` per process, and the same
platform collectors. No new process, no new dependency, no selection or action
change.

Each host section heading gains a `LOAD` field showing the raw 1-, 5-, and
15-minute load averages:

```text
  agent-workstation  CPU 27%  MEM 30%  LOAD 1.24 0.96 0.72
```

Load averages are reported exactly as the kernel exposes them: two decimals, not
a percentage, not divided by core count. A machine can legitimately show a load
above its core count (backlog) or above 100, so the values are never clamped.

## Goals

- Show the current raw 1/5/15-minute load averages for the local host and each
  remote host, alongside the existing CPU and MEM percentages.
- Reuse the existing per-process `HostUsageHub` snapshot; add no extra sampling
  and no extra macOS subprocess.
- Keep new clients compatible with old servers and old clients compatible with
  new servers.
- Keep session collection, CPU, MEM, and the HTTP endpoint working when load
  averages cannot be collected.
- Support the current release targets: `darwin/arm64`, `linux/amd64`, and
  `linux/arm64`.

## Non-goals

- Per-core or per-process load.
- Core-normalized load (dividing by CPU count) or a load percentage.
- Historical charts, trends, or alert thresholds on load.
- Colorizing load by saturation.
- Adding a system-monitoring dependency or a second macOS command.
- Changing selection, sorting, session actions, or remote configuration.

## User-visible behavior

The host heading extends the existing `CPU %  MEM %` form with `LOAD`:

```text
  workstation  CPU 17%  MEM 54%  LOAD 0.42 0.55 0.61
  ...local session rows...

  beluga  CPU 42%  MEM 71%  LOAD 3.10 2.80 2.55
  ...remote session rows...
```

- The three load numbers are the raw 1-, 5-, and 15-minute averages, formatted
  with two decimals, space-separated, in that fixed order.
- Values are never clamped or normalized; a valid `0.00` renders as `0.00`, and
  a value above 100 renders unchanged (e.g. `128.50`).
- Load availability is **atomic**: the three numbers are collected and validated
  as one unit, so the heading shows either all three or none. Unavailable renders
  as `LOAD -- -- --`, never a partial mix like `LOAD 1.24 -- 0.72`. This mirrors
  the transport rule below — `hostLoadAverage` returns nil atomically — so
  validation-atomicity and render-atomicity are the same invariant.
- `LOAD` is independent of `CPU` and `MEM`: a host can show `CPU 17%  MEM 54%
  LOAD -- -- --` (or the reverse) when one source parses and the other does not.
- No color thresholds are added.

## Data model

Load averages ride inside the existing `HostUsage` value as a nested optional
object, preserving the "nil means unavailable, zero is a real value" contract:

```go
type LoadAverage struct {
	OneMinute      *float64 `json:"oneMinute,omitempty"`
	FiveMinutes    *float64 `json:"fiveMinutes,omitempty"`
	FifteenMinutes *float64 `json:"fifteenMinutes,omitempty"`
}

type HostUsage struct {
	CPUPercent    *float64     `json:"cpuPercent,omitempty"`
	MemoryPercent *float64     `json:"memoryPercent,omitempty"`
	Load          *LoadAverage `json:"loadAverage,omitempty"`
}
```

A nil `Load` pointer means load is unavailable and is omitted from JSON. When
present, all three inner pointers are populated together.

The validated constructor centralizes the atomic rule:

```go
func hostLoadAverage(one, five, fifteen float64) *LoadAverage
```

- Returns a `*LoadAverage` with all three fields set when every input is a
  non-negative finite number.
- A valid zero is preserved.
- Values above 100 are valid and passed through unchanged (no clamp — load
  averages are not percentages).
- If any input is negative, `NaN`, `+Inf`, or `-Inf`, it returns `nil`
  atomically rather than a partially valid `LoadAverage`.

Unlike `hostPercent`, `hostLoadAverage` does not clamp to `[0, 100]`;
`hostPercent`'s existing behavior is unchanged.

## HTTP protocol

`GET /sessions` embeds `loadAverage` inside the existing `hostUsage` object:

```json
{
  "hostname": "agent-workstation",
  "ts": 1784851200,
  "hostUsage": {
    "cpuPercent": 27.0,
    "memoryPercent": 30.0,
    "loadAverage": {
      "oneMinute": 1.24,
      "fiveMinutes": 0.96,
      "fifteenMinutes": 0.72
    }
  },
  "sessions": []
}
```

Compatibility behavior:

- Old clients ignore the new nested `loadAverage` field.
- New clients decoding an old server response (with `hostUsage` but no
  `loadAverage`) receive a nil `HostUsage.Load` and render `LOAD -- -- --`.
- A new server omits `loadAverage` entirely when load is unavailable; CPU and
  MEM continue to serialize independently.
- Load collection failure does not change HTTP status. `/sessions` still fails
  only under its existing session-collection failure conditions.

`FetchRemote` decodes `loadAverage` as part of the `hostUsage` object it already
decodes; no new field is added to `RemoteResult` and host tagging is unchanged.

## Sampling architecture

No new sampling path. The single `HostUsageHub` per process role (TUI, server,
one-shot) already samples one `HostUsage` every two seconds through one platform
collector; load averages are populated inside that same snapshot. Concurrent
`/sessions` handlers keep reading the one cached snapshot, so load costs nothing
beyond the sample already taken.

## Linux collector

Read `/proc/loadavg` and parse the first three space-separated fields (the 1-,
5-, and 15-minute averages). The remaining fields (running/total tasks, last
PID) are ignored. Feed the three parsed values through `hostLoadAverage`, which
returns nil atomically on any malformed, missing, negative, or non-finite field.

This is a new, cheap file read added to the existing Linux collector's `Sample`
alongside the current `/proc/stat` and `/proc/meminfo` reads; it does not spawn a
process. A `/proc/loadavg` read failure or parse failure leaves `Load` nil and
does not affect CPU or memory.

## macOS collector

macOS load averages are already present in the output the collector runs today:

```text
LC_ALL=C top -l 2 -n 0 -s 0
```

`top` prints a `Load Avg:` line (e.g. `Load Avg: 1.24, 0.96, 0.72`) in each
sample block. The load line is parsed in the **same** `parseDarwinTop` pass that
already extracts `CPU usage:` and `PhysMem:` from the final sample block — this
is why the feature adds no extra process on macOS and needs no second command.
The three comma-separated numbers are passed through `hostLoadAverage`. A
missing or malformed `Load Avg:` line leaves `Load` nil and is independent of CPU
and memory parsing.

## Rendering

The shared host-heading helper (`renderHostHeading`) appends a `LOAD` field after
`MEM`:

```text
  <bold host>  CPU <pct>  MEM <pct>  LOAD <1m> <5m> <15m>
```

Load formatting is distinct from `formatHostPercent` and must **not** reuse it:

- `formatHostPercent` renders a rounded, clamped `[0,100]` percentage with a `%`
  suffix — wrong for load.
- Load uses a separate formatter: two decimals (`%.2f`), no `%` suffix, no
  clamp, no core-normalization.
- When `HostUsage.Load` is nil, the whole field renders `LOAD -- -- --` (three
  dashes), matching the atomic-availability invariant.

The heading remains a single line shared by the full, intermediate, and minimal
view modes; it does not participate in selection and does not change section
counts.

## Error handling

- Load-average errors never abort session collection, TUI refresh, one-shot
  rendering, or the HTTP endpoint.
- No raw collector error is exposed in the table or protocol.
- Parsers return nil (via `hostLoadAverage`) for malformed data and never panic.
- Load parsing is independent of CPU and memory parsing on both platforms.
- Load values are never clamped; validation only rejects negative and
  non-finite inputs.

## Testing

### Validation tests (this task)

Table-driven `hostLoadAverage` cases, reusing the existing `floatPtr` /
`assertFloatPtr` helpers:

- Normal 1/5/15 triple returns all three fields unchanged.
- All-zero triple preserved (not treated as unavailable).
- Values above 100 pass through un-clamped.
- Each invalid class returns nil atomically: negative, `NaN`, `+Inf`, `-Inf`,
  exercised across different field positions to prove atomicity.
- `hostPercent` behavior remains unchanged (existing indirect coverage).

### Later-task tests

- Linux: `/proc/loadavg` parser — normal, extra trailing fields ignored,
  truncated (fewer than three fields), and malformed numeric fields.
- macOS: `parseDarwinTop` extracts `Load Avg:` from the final block; missing or
  malformed load line leaves `Load` nil while CPU/MEM stay valid.
- Protocol: `/sessions` includes nested `loadAverage`; an old server response
  without it decodes to a nil `Load`; `FetchRemote` decodes it.
- Rendering: `LOAD 1.24 0.96 0.72` for valid data and `LOAD -- -- --` for nil,
  in all three view modes; two-decimal formatting; values above 100 unchanged.

### Verification commands

```sh
go test ./...
go vet ./...
make
```

`make` cross-builds `darwin/arm64`, `linux/amd64`, and `linux/arm64`. `go.mod`
and `go.sum` must show no new dependencies.

## Acceptance criteria

- Every host heading (local and remote, all view modes and one-shot output)
  shows raw 1/5/15-minute load averages after CPU and MEM.
- Load values render with two decimals, un-clamped and not core-normalized.
- Load availability is atomic: all three numbers or `LOAD -- -- --`, never a
  partial mix.
- `0.00` load is distinguishable from unavailable.
- Load failures never hide sessions, CPU, MEM, or fail `/sessions`.
- Load rides inside the existing `HostUsage`/`hostUsage` object as a nested
  `loadAverage`; new and old client/server combinations remain compatible.
- No extra macOS process and no additional sampling: one cached hub per process.
- Full tests, vet, and release-platform cross-builds pass; no new dependency.
