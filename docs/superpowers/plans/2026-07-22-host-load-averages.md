# Host Load Averages Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Show raw htop-style 1/5/15-minute load averages in every host heading
(local and remote, all view modes and one-shot output), after the existing CPU
and MEM percentages, reusing the current `HostUsage` transport and the single
cached `HostUsageHub` per process.

**Architecture:** Carry load inside the existing `HostUsage` value as a nested
optional `LoadAverage` object. A validated constructor (`hostLoadAverage`)
enforces atomic, un-clamped, finite-only values. Linux populates it from
`/proc/loadavg`; macOS populates it from the `Load Avg:` line already present in
the `top` output the collector runs today. A dedicated load formatter renders
`LOAD <1m> <5m> <15m>` (two decimals) or `LOAD -- -- --` in the shared host
heading.

**Tech Stack:** Go, standard library (`context`, `math`, `os`, `strconv`,
`strings`), existing `golang.org/x/sys` and `golang.org/x/term` dependencies
only. No new dependency.

## Global Constraints

- Load averages are raw kernel values: two decimals, never a percentage, never
  divided by core count, never clamped.
- Load availability is atomic: render all three numbers or `LOAD -- -- --`,
  never a partial mix. `hostLoadAverage` returns nil atomically on any negative,
  `NaN`, or infinite input.
- Load rides inside `HostUsage` as nested `loadAverage`; `RemoteResult` and the
  `/sessions` envelope need no new field — they already carry `HostUsage`.
- Load failure must never fail session collection, CPU, MEM, `/sessions`, TUI
  refresh, or one-shot output; load parsing is independent of CPU/MEM parsing.
- No extra sampling and no extra macOS process: one cached `HostUsageHub` per
  process, one `top` invocation reused for CPU, MEM, and load.
- The load formatter must NOT reuse `formatHostPercent` (which clamps to
  `[0,100]` and appends `%`).
- `hostPercent` behavior is unchanged.
- Preserve selection, sorting, session actions, account-usage bars, and the
  per-session `CPU%` column.
- Supported release targets remain exactly `darwin/arm64`, `linux/amd64`, and
  `linux/arm64`.
- Add no module dependency; `go.mod` and `go.sum` must remain unchanged.
- Keep parser/collector filenames OS-neutral (`host_usage_proc.go`,
  `host_usage_top.go`) so cross-platform parser tests keep compiling.

---

## File Structure

- Modify `host_usage.go`: add `LoadAverage`, `HostUsage.Load`, and
  `hostLoadAverage` (done in Task 1).
- Modify `host_usage_proc.go`: read/parse `/proc/loadavg` in the Linux
  collector's `Sample`.
- Modify `host_usage_top.go`: parse the `Load Avg:` line inside `parseDarwinTop`.
- Modify `host_usage_test.go` / `host_usage_proc_test.go` / `host_usage_top_test.go`:
  validation, Linux parser, and macOS parser tests.
- Modify `render.go`: add a load formatter and extend `renderHostHeading` with
  the `LOAD` field.
- Modify `render_test.go`: heading tests for valid, zero, >100, and unavailable
  load in all view modes.
- Modify `server_test.go` / `remote_test.go`: confirm nested `loadAverage`
  round-trips and old-server compatibility.
- Create `docs/superpowers/specs/2026-07-22-host-load-averages-design.md` and
  `docs/superpowers/plans/2026-07-22-host-load-averages.md` (done in Task 1).
- Modify `README.md`: note the `LOAD` field in the host heading.

---

### Task 1: Load-average model, validation, and approved docs (done)

**Files:**
- Modify: `host_usage.go`
- Create: `host_usage_test.go`
- Create: `docs/superpowers/specs/2026-07-22-host-load-averages-design.md`
- Create: `docs/superpowers/plans/2026-07-22-host-load-averages.md`

**Interfaces:**
- Produces:
  - `type LoadAverage struct { OneMinute, FiveMinutes, FifteenMinutes *float64 }`
  - `HostUsage.Load *LoadAverage` (json `loadAverage,omitempty`)
  - `func hostLoadAverage(one, five, fifteen float64) *LoadAverage`

- [x] **Step 1: Write failing validation tests** — table-driven `hostLoadAverage`
  cases covering normal, all-zero, above-100, and each invalid class (negative,
  `NaN`, `+Inf`, `-Inf`), reusing `floatPtr` / `assertFloatPtr`.
- [x] **Step 2: Add model and constructor** — `LoadAverage`, `HostUsage.Load`,
  and `hostLoadAverage` (loop over the three inputs; return nil on any
  `v < 0 || IsNaN(v) || IsInf(v, 0)`; else return all three by pointer; no
  clamp). Run `gofmt -w`.
- [x] **Step 3: Record approved behavior in the design and plan docs.**
- [x] **Step 4: Verify** — focused test, `go test ./...`, `go vet ./...`;
  `go.mod`/`go.sum` unchanged.

---

### Task 2: Collect Linux load averages from `/proc/loadavg`

**Files:**
- Modify: `host_usage_proc.go`
- Modify: `host_usage_proc_test.go`

**Interfaces:**
- Consumes: `hostLoadAverage`, the collector's `readFile` seam.
- Produces: `func parseLinuxLoadAverage(string) *LoadAverage`; `Load` populated
  in `linuxHostUsageCollector.Sample`.

- [ ] **Step 1: Write failing parser tests.** Cases: normal
  (`"1.24 0.96 0.72 2/1234 5678\n"` returns 1.24/0.96/0.72); trailing fields
  ignored; truncated (fewer than three fields) returns nil; malformed numeric
  field returns nil; negative field returns nil (atomic via `hostLoadAverage`).
- [ ] **Step 2: Implement `parseLinuxLoadAverage`.** Split on whitespace, require
  at least three fields, `strconv.ParseFloat` each of the first three, and pass
  them through `hostLoadAverage`. Any parse error returns nil.
- [ ] **Step 3: Wire into `Sample`.** Read `/proc/loadavg` via `c.readFile`
  alongside the existing `/proc/meminfo` and `/proc/stat` reads; set
  `usage.Load`. A read error leaves `Load` nil and does not touch CPU/MEM.
- [ ] **Step 4: Verify.** `gofmt -w`; focused tests; `go build ./...`;
  `go test ./...`.

---

### Task 3: Parse macOS load averages from existing `top` output

**Files:**
- Modify: `host_usage_top.go`
- Modify: `host_usage_top_test.go`

**Interfaces:**
- Consumes: `hostLoadAverage`, the `Load Avg:` line already present in
  `top -l 2 -n 0 -s 0` output.
- Produces: `func parseDarwinLoadAverage(string) *LoadAverage`; `Load` set in
  `parseDarwinTop`.

- [ ] **Step 1: Write failing parser tests.** Extend the multi-block fixture with
  `Load Avg: 1.24, 0.96, 0.72` and assert the three values; assert `Load` nil
  when the line is missing or malformed while CPU/MEM stay valid; assert load and
  CPU/MEM remain independent.
- [ ] **Step 2: Implement `parseDarwinLoadAverage`.** Match the `Load Avg:` line
  (last one wins, mirroring the CPU/PhysMem final-block rule), split the trailing
  numbers on commas, `ParseFloat` the three, and pass through `hostLoadAverage`.
- [ ] **Step 3: Wire into `parseDarwinTop`.** Populate `HostUsage.Load` in the
  same pass that fills `CPUPercent` and `MemoryPercent` — no second command.
- [ ] **Step 4: Verify.** `gofmt -w`; focused tests; `go build ./...`;
  `go test ./...`.

---

### Task 4: Render `LOAD` in every host heading and confirm transport

**Files:**
- Modify: `render.go`
- Modify: `render_test.go`
- Modify: `server_test.go`
- Modify: `remote_test.go`

**Interfaces:**
- Consumes: `HostUsage.Load`, `LoadAverage`.
- Produces: `func formatHostLoad(*LoadAverage) string`; `LOAD` field appended in
  `renderHostHeading`.

- [ ] **Step 1: Write failing heading tests.** Extend the all-views heading test
  so a populated `Load` renders `LOAD 1.24 0.96 0.72` (two decimals), a nil
  `Load` renders `LOAD -- -- --`, a zero triple renders `LOAD 0.00 0.00 0.00`,
  and a value above 100 renders un-clamped (e.g. `128.50`). Add a
  `formatHostLoad` unit test for nil and a `>100` value.
- [ ] **Step 2: Add `formatHostLoad`.** Return `-- -- --` when the pointer or any
  inner field is nil; otherwise `fmt.Sprintf("%.2f %.2f %.2f", ...)`. Do not
  clamp; do not reuse `formatHostPercent`.
- [ ] **Step 3: Extend `renderHostHeading`.** Append `  LOAD %s` using
  `formatHostLoad(sec.hostUsage.Load)`. One shared line for all three view modes.
- [ ] **Step 4: Confirm transport round-trips.** Add/extend a `server_test.go`
  case asserting `/sessions` emits nested `loadAverage`, and a `remote_test.go`
  case asserting `FetchRemote` decodes it and that an old-server response without
  `loadAverage` yields a nil `Load`. No struct change to `RemoteResult` or the
  server envelope is needed — both already carry `HostUsage`.
- [ ] **Step 5: Verify.** `gofmt -w`; focused render/protocol tests;
  `go build ./...`; `go test ./...`.

---

### Task 5: Document and verify

**Files:**
- Modify: `README.md`
- Modify: `docs/superpowers/specs/2026-07-22-host-load-averages-design.md` (if any
  correction is needed during implementation)

- [ ] **Step 1: Document the visible heading.** Extend the README host-usage
  paragraph to note the `LOAD 1.24 0.96 0.72` field: raw 1/5/15-minute averages,
  two decimals, un-clamped, `LOAD -- -- --` when unavailable.
- [ ] **Step 2: Full correctness gates.**

  ```sh
  go test ./...
  go test -race ./... -run 'TestHostUsageHub'
  go vet ./...
  GOOS=darwin GOARCH=arm64 go build -o /tmp/cs-darwin-arm64 .
  GOOS=linux GOARCH=amd64 go build -o /tmp/cs-linux-amd64 .
  GOOS=linux GOARCH=arm64 go build -o /tmp/cs-linux-arm64 .
  make
  ```

- [ ] **Step 3: Linux one-shot smoke test.** `go run . --once | sed -n '1,10p'`
  shows a local heading of the form `<host>  CPU <n>%  MEM <n>%  LOAD <n> <n> <n>`.
- [ ] **Step 4: macOS runtime check (on a real Mac).** Confirm the `Load Avg:`
  line parses and the three numbers render; cross-compilation alone cannot
  validate macOS output.
- [ ] **Step 5: Scope/dependency check.** `git diff -- go.mod go.sum` is empty;
  `git status --short` contains only intended files.
