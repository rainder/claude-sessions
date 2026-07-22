# Host Resource Usage Design

**Date:** 2026-07-21
**Status:** Approved

## Summary

Expose whole-host CPU and physical-memory consumption for the current machine and every configured remote server. Each host section in the TUI and one-shot list output will show a compact heading such as:

```text
  devbox  CPU 23%  MEM 61%
```

CPU uses a 0–100 whole-host scale averaged across all logical cores. A machine with every core fully busy reports 100%, not `N * 100%`.

The implementation must preserve the existing single-binary deployment model and add no dependencies beyond the current standard library and `golang.org/x/sys` / `golang.org/x/term` modules.

## Goals

- Show current aggregate CPU usage for the local host and each remote host.
- Show current physical-memory usage for the local host and each remote host.
- Use the same heading format in all three TUI view modes and in one-shot output.
- Keep session collection and actions working when host metrics cannot be collected.
- Keep new clients compatible with old servers and old clients compatible with new servers.
- Sample once per process, not once per remote HTTP client.
- Support the current release targets: `darwin/arm64`, `linux/amd64`, and `linux/arm64`.

## Non-goals

- Per-session memory usage.
- Per-core CPU breakdowns.
- Historical charts, trends, load averages, or alert thresholds.
- Persisting host metrics.
- Adding a general-purpose system-monitoring dependency.
- Changing selection, sorting, session actions, or remote configuration semantics.

## User-visible behavior

Every host section gets a heading with host identity and resource usage:

```text
  workstation  CPU 17%  MEM 54%
  ...local session rows...

  beluga  CPU 42%  MEM 71%
  ...remote session rows...
```

- Local identity is `shortHostname()`.
- Remote identity remains the configured server alias (`RemoteResult.Name`), preserving the stable name already used for rendering, selection IDs, and actions.
- Values render as rounded whole percentages.
- Missing values render independently as `CPU --` or `MEM --`.
- The shared column header remains global; host headings appear immediately before each section's rows.
- Loading, unreachable, and `(no sessions)` states remain below their host heading.
- No color thresholds are added. This feature exposes values without introducing alert policy.

## Data model

Add a transport-safe value with optional fields:

```go
type HostUsage struct {
    CPUPercent    *float64 `json:"cpuPercent,omitempty"`
    MemoryPercent *float64 `json:"memoryPercent,omitempty"`
}
```

Pointers distinguish a valid `0%` from an unavailable metric.

Add host usage to remote poll results:

```go
type RemoteResult struct {
    Name      string
    Sessions  []Session
    HostUsage HostUsage
    Error     string
    Loading   bool
}
```

Group local render data explicitly instead of adding more positional arguments to `RenderAll`:

```go
type LocalHost struct {
    Name      string
    Sessions  []Session
    HostUsage HostUsage
}
```

`CollectLocal()` continues returning `[]Session`; TUI and one-shot callers construct `LocalHost` from `shortHostname()`, the session slice, and the host-usage snapshot. Selection and sorting continue operating on `LocalHost.Sessions`.

Rendering's internal `section` gains `hostUsage HostUsage`. `buildSections` accepts `LocalHost` plus `[]RemoteResult` and creates one uniform section model for all hosts.

## HTTP protocol

The authenticated `GET /sessions` response adds `hostUsage`:

```json
{
  "hostname": "beluga",
  "ts": 1784678400,
  "hostUsage": {
    "cpuPercent": 42.3,
    "memoryPercent": 71.0
  },
  "sessions": []
}
```

Compatibility behavior:

- Old clients ignore the new JSON field.
- New clients decoding an old server response receive zero-value `HostUsage`, whose pointer fields are nil and render as unavailable.
- A new server includes `hostUsage` even when one or both nested fields are omitted.
- Metric collection failure does not change HTTP status. `/sessions` fails only under its existing session-collection failure conditions.

`FetchRemote` decodes `hostUsage` alongside `sessions`, retains the configured alias as `RemoteResult.Name`, and keeps existing host tagging for each returned session.

## Sampling architecture

### HostUsageHub

Add one `HostUsageHub` per long-running process role. It owns one platform collector and a mutex-protected cached `HostUsage` snapshot.

Responsibilities:

- Perform one bounded initial sample before the caller uses the first snapshot.
- Refresh every two seconds, matching the session/remote refresh cadence.
- Serialize collector calls.
- Serve lock-protected snapshots to any number of readers.
- Cancel an in-flight platform command during shutdown.
- Treat collector errors as metric unavailability, not process errors.

Usage by role:

- **TUI:** create one hub at startup; render reads `Snapshot()`. Existing wall-clock refresh is sufficient, so no extra wake pipe is needed.
- **Server:** create one hub before serving requests; every concurrent `/sessions` handler reads the same cached snapshot. Sampling cost remains constant regardless of connected clients.
- **One-shot list:** call a synchronous helper using one collector instance, including Linux's initial two-sample bootstrap, then exit without starting a ticker.

Each sample has a bounded context. Initial or periodic failure produces nil for the failed metric and is retried at the next interval. Successful CPU and memory values are clamped to `[0, 100]` before publication.

## Linux collector

Use procfs directly; do not spawn a command.

### CPU

Read the aggregate `cpu` line from `/proc/stat`.

- Include `user`, `nice`, `system`, `idle`, `iowait`, `irq`, `softirq`, and `steal` in total time.
- Exclude `guest` and `guest_nice` because Linux already includes them in `user` and `nice`.
- Treat `idle + iowait` as idle time.
- Compute utilization from counter deltas:

```text
busy_delta / total_delta * 100
```

The first call reads a baseline, waits approximately 100 ms with context cancellation, then reads again. Later calls compare against the previous sample without an extra delay.

Counter regression, malformed input, missing fields, or a zero total delta makes CPU unavailable for that sample.

### Memory

Read `/proc/meminfo` and compute:

```text
(MemTotal - MemAvailable) / MemTotal * 100
```

`MemAvailable` is used instead of `MemFree` so readily reclaimable cache does not appear as consumed memory. Missing, malformed, zero, or inconsistent values make memory unavailable for that sample.

## macOS collector

`golang.org/x/sys/unix` does not expose the Mach `host_statistics` APIs needed for accurate aggregate CPU and memory metrics in a CGO-disabled binary. Adding cgo, `purego`, or `gopsutil` would expand the dependency/build model, so macOS uses the native `top` command.

Run a bounded, non-interactive snapshot with a deterministic locale:

```text
LC_ALL=C top -l 2 -n 0 -s 0
```

macOS `top` needs two samples to calculate interval CPU usage; the first sample has no prior baseline. Parse the last sample block:

- Aggregate CPU idle percentage from the final `CPU usage:` line; utilization is `100 - idle`.
- Physical used and unused memory from the final `PhysMem:` line; utilization is `used / (used + unused) * 100`.

The parser supports the size suffixes emitted by supported macOS versions and rejects malformed or incomplete lines without panicking. CPU and memory parsing are independent, so one valid value can still be published when the other is unavailable.

Long-running TUI and server roles invoke `top` only through their hub, once every two seconds per process and never once per HTTP request. One-shot mode invokes it once through the synchronous collector. `exec.CommandContext` provides timeout and shutdown cancellation.

## Rendering

Add a shared host-heading helper used by all three view modes:

```text
  <bold host>  CPU <value>  MEM <value>
```

The helper owns formatting for rounded percentages and unavailable values. Each view mode calls the same helper rather than duplicating label construction.

Rendering order for each mode:

1. Global title and account-usage line.
2. Shared table column header and separator.
3. Local host heading, then local rows or `(no sessions)`.
4. For each remote: blank separator line, remote host heading, then loading/error/empty/session rows.

Existing clipping handles narrow terminals. Host headings do not participate in session selection and do not change section counts.

RemoteHub behavior remains unchanged:

- While a new request is in flight, its previous `RemoteResult`, including host usage, stays visible.
- A failed request replaces the result with its existing unreachable state and no metrics, so the heading shows `CPU --  MEM --`.
- A successful response with a collector failure still shows sessions and only marks the failed metric unavailable.

## Error handling

- Host metric errors never abort session collection, TUI refresh, one-shot rendering, or the HTTP endpoint.
- No raw collector error is exposed in the table or protocol.
- Parsers return unavailable values for malformed data and never panic.
- Linux sleeps and macOS subprocesses respect cancellation.
- Hub shutdown stops its ticker, cancels active work, and waits for its goroutine.
- Percentage calculations guard against division by zero, counter regression, NaN, and infinity.

## Testing

### Parser and calculation tests

Use pure parsing helpers and fixture strings so Linux and macOS formats can be tested on the development host.

Linux cases:

- Normal aggregate CPU delta.
- Guest fields excluded from total.
- `idle + iowait` treated as idle.
- Zero delta.
- Counter regression.
- Truncated or malformed `/proc/stat`.
- Normal `MemTotal` / `MemAvailable` calculation.
- Missing, malformed, zero, and inconsistent memory values.

macOS cases:

- Two sample blocks use the final `CPU usage:` and `PhysMem:` lines.
- `0%` and `100%` boundaries.
- Supported binary size suffixes and decimals.
- Missing one metric while the other remains valid.
- Malformed and localized/unexpected output rejected safely.

### Hub tests

Inject a fake collector to verify:

- Initial snapshot publication.
- Periodic cache updates.
- Concurrent snapshot readers.
- Serialized collector calls.
- Independent CPU/memory unavailability.
- Retry after a failed sample.
- Shutdown cancellation and goroutine exit.

### Protocol tests

- `/sessions` includes `hostUsage` without changing existing fields.
- `FetchRemote` decodes both metrics.
- Missing `hostUsage` from an old server becomes unavailable.
- A partially populated `hostUsage` remains partially available.
- Session `Host` tagging remains unchanged.

### Rendering tests

For all three modes:

- Local and remote headings appear in the correct order.
- Valid zero, normal, and 100-percent values render correctly.
- Unavailable values render as `--`.
- Loading, unreachable, empty, and populated sections retain existing body behavior.
- Header counts and selection behavior remain unchanged.

### Verification commands

```sh
go test ./...
go vet ./...
make
```

`make` cross-builds `darwin/arm64`, `linux/amd64`, and `linux/arm64`. Also run a Linux one-shot smoke test and inspect its local host heading. `go.mod` and `go.sum` must show no new dependencies.

## Acceptance criteria

- Current/local host shows `CPU` and `MEM` percentages in every view mode and one-shot output.
- Every configured remote section shows the same metrics when supplied by its server.
- CPU is a 0–100 whole-host percentage across all cores.
- Memory is a 0–100 physical-memory consumption percentage using the platform definitions above.
- `0%` is distinguishable from unavailable.
- Metric failures never hide sessions or fail `/sessions`.
- Concurrent HTTP clients do not multiply sampling work.
- New and old client/server combinations remain compatible.
- Full tests, vet, and release-platform cross-builds pass.
- No new dependency is added.
