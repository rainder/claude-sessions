# Host Resource Usage Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Show current 0–100 whole-host CPU and physical-memory consumption for the local machine and every configured remote server in all table views and one-shot output.

**Architecture:** Add pure Linux/macOS parsers, platform collectors selected at runtime, and one cancellable `HostUsageHub` per long-running process. Extend the existing `/sessions` JSON envelope and `RemoteResult`, then feed one uniform local/remote section model into shared host-heading rendering.

**Tech Stack:** Go 1.26.3, standard library (`context`, `encoding/json`, `math`, `net/http`, `os`, `os/exec`, `runtime`, `strconv`, `strings`, `sync`, `time`), existing `golang.org/x/sys` and `golang.org/x/term` dependencies only.

## Global Constraints

- CPU is a 0–100 whole-host percentage averaged across all logical cores; fully busy means 100%, not `N * 100%`.
- Linux CPU comes from aggregate `/proc/stat` counter deltas; guest fields are excluded and `idle + iowait` count as idle.
- Linux memory is `(MemTotal - MemAvailable) / MemTotal * 100`.
- macOS uses `LC_ALL=C top -l 2 -n 0 -s 0` and parses the final sample block; the first block has no prior CPU baseline.
- Missing metrics use nil pointers and render as `--`; valid `0%` must remain distinguishable.
- Host metric failure must never fail session collection, `/sessions`, TUI refresh, or one-shot output.
- Long-running server and TUI roles sample once per process every two seconds, not once per HTTP request.
- Preserve compatibility: old clients ignore `hostUsage`; new clients treat an absent field from old servers as unavailable.
- Preserve selection, sorting, session actions, inspector behavior, account-usage bars, and per-session `CPU%` column.
- Supported release targets remain exactly `darwin/arm64`, `linux/amd64`, and `linux/arm64`.
- Add no module dependency; `go.mod` and `go.sum` must remain unchanged.
- Keep parser/collector filenames OS-neutral (`host_usage_proc.go`, `host_usage_top.go`); Go treats `_linux.go` and `_darwin.go` suffixes as implicit build constraints, which would prevent cross-platform parser tests and break runtime collector selection.
- Every commit message must end with `Co-Authored-By: Claude <noreply@anthropic.com>`.

---

## File Structure

- Create `host_usage.go`: shared model, percentage guard, collector interface, runtime collector selection, synchronous collection, cached hub, pause/resume/kick/shutdown lifecycle.
- Create `host_usage_proc.go`: Linux procfs parser and stateful two-sample collector.
- Create `host_usage_top.go`: macOS `top` parser and command-backed collector.
- Create `host_usage_proc_test.go`: Linux parser/math and cancellable bootstrap tests.
- Create `host_usage_top_test.go`: multi-block macOS parser and size-unit tests.
- Create `host_usage_hub_test.go`: deterministic fake-collector tests for initial sample, refresh, pause/resume, concurrency, retry, and cancellation.
- Create `remote_test.go`: `/sessions` client compatibility and session-host tagging tests.
- Modify `remote.go`: add `HostUsage` to `RemoteResult` and decode it.
- Modify `server.go`: inject cached host snapshot into `/sessions`; create one server-side hub.
- Modify `server_test.go`: verify `hostUsage` response shape and partial/unavailable values.
- Modify `render.go`: accept `LocalHost`, carry usage in sections, and render one shared host heading in all views.
- Modify `render_test.go`, `model_test.go`, `tui_state_test.go`: migrate signatures and verify headings without changing selection/count semantics.
- Modify `tui.go`: create one local host hub, render its snapshot, and coordinate pause/resume/manual refresh.
- Modify `main.go`: collect one synchronous local sample for `--once`.
- Modify `README.md`: document local/remote host resource headings.
- Modify `docs/superpowers/specs/2026-07-21-host-resource-usage-design.md`: already corrected to the two-sample macOS command; include this correction in the next documentation commit.

---

### Task 1: Add Host Usage Models and Pure Parsers

**Files:**
- Create: `host_usage.go`
- Create: `host_usage_proc.go`
- Create: `host_usage_top.go`
- Create: `host_usage_proc_test.go`
- Create: `host_usage_top_test.go`

**Interfaces:**
- Consumes: standard-library text parsing only.
- Produces:
  - `type HostUsage struct { CPUPercent, MemoryPercent *float64 }`
  - `type LocalHost struct { Name string; Sessions []Session; HostUsage HostUsage }`
  - `func hostPercent(float64) *float64`
  - `func parseLinuxCPUTimes(string) (linuxCPUTimes, bool)`
  - `func linuxCPUPercent(linuxCPUTimes, linuxCPUTimes) *float64`
  - `func parseLinuxMemory(string) *float64`
  - `func parseDarwinTop(string) HostUsage`

- [ ] **Step 1: Write failing Linux parser tests**

Create `host_usage_proc_test.go`:

```go
package main

import "testing"

func TestParseLinuxCPUTimesExcludesGuestAndCountsIOWaitIdle(t *testing.T) {
	got, ok := parseLinuxCPUTimes("cpu  100 10 20 50 5 3 2 10 40 7\ncpu0 1 2 3 4 5 6 7 8\n")
	if !ok {
		t.Fatal("parseLinuxCPUTimes returned !ok")
	}
	// total includes the first eight counters only. guest/guest_nice are already
	// included in user/nice and must not be added again.
	if got.total != 200 {
		t.Fatalf("total = %d, want 200", got.total)
	}
	if got.idle != 55 {
		t.Fatalf("idle = %d, want 55", got.idle)
	}
}

func TestLinuxCPUPercent(t *testing.T) {
	cases := []struct {
		name       string
		prev, next linuxCPUTimes
		want       *float64
	}{
		{"normal", linuxCPUTimes{total: 100, idle: 40}, linuxCPUTimes{total: 200, idle: 65}, floatPtr(75)},
		{"zero delta", linuxCPUTimes{total: 100, idle: 40}, linuxCPUTimes{total: 100, idle: 40}, nil},
		{"total regression", linuxCPUTimes{total: 100, idle: 40}, linuxCPUTimes{total: 99, idle: 40}, nil},
		{"idle regression", linuxCPUTimes{total: 100, idle: 40}, linuxCPUTimes{total: 200, idle: 39}, nil},
		{"idle exceeds total delta", linuxCPUTimes{total: 100, idle: 40}, linuxCPUTimes{total: 110, idle: 60}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := linuxCPUPercent(tc.prev, tc.next)
			assertFloatPtr(t, got, tc.want)
		})
	}
}

func TestParseLinuxCPUTimesRejectsBadInput(t *testing.T) {
	for _, input := range []string{
		"",
		"cpu0 1 2 3 4 5 6 7 8\n",
		"cpu 1 2 3\n",
		"cpu 1 2 bad 4 5 6 7 8\n",
	} {
		if _, ok := parseLinuxCPUTimes(input); ok {
			t.Fatalf("parseLinuxCPUTimes(%q) returned ok", input)
		}
	}
}

func TestParseLinuxMemory(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  *float64
	}{
		{"normal", "MemTotal: 1000 kB\nMemAvailable: 250 kB\n", floatPtr(75)},
		{"zero used", "MemTotal: 1000 kB\nMemAvailable: 1000 kB\n", floatPtr(0)},
		{"missing total", "MemAvailable: 250 kB\n", nil},
		{"missing available", "MemTotal: 1000 kB\n", nil},
		{"zero total", "MemTotal: 0 kB\nMemAvailable: 0 kB\n", nil},
		{"inconsistent", "MemTotal: 1000 kB\nMemAvailable: 1001 kB\n", nil},
		{"malformed", "MemTotal: nope kB\nMemAvailable: 250 kB\n", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertFloatPtr(t, parseLinuxMemory(tc.input), tc.want)
		})
	}
}
```

Use shared test helpers at the bottom of the same file:

```go
func floatPtr(v float64) *float64 { return &v }

func assertFloatPtr(t *testing.T, got, want *float64) {
	t.Helper()
	if got == nil || want == nil {
		if got != nil || want != nil {
			t.Fatalf("got %v, want %v", got, want)
		}
		return
	}
	if *got != *want {
		t.Fatalf("got %.4f, want %.4f", *got, *want)
	}
}
```

- [ ] **Step 2: Write failing macOS parser tests**

Create `host_usage_top_test.go`:

```go
package main

import "testing"

func TestParseDarwinTopUsesFinalSample(t *testing.T) {
	out := `Processes: 500 total
CPU usage: 99.0% user, 0.0% sys, 1.0% idle
PhysMem: 15G used (2G wired), 1G unused.
Processes: 501 total
CPU usage: 12.5% user, 7.5% sys, 80.0% idle
PhysMem: 12G used (2G wired), 4G unused.
`
	got := parseDarwinTop(out)
	assertFloatPtr(t, got.CPUPercent, floatPtr(20))
	assertFloatPtr(t, got.MemoryPercent, floatPtr(75))
}

func TestParseDarwinTopMetricsAreIndependent(t *testing.T) {
	cpuOnly := parseDarwinTop("CPU usage: 0.0% user, 0.0% sys, 100.0% idle\n")
	assertFloatPtr(t, cpuOnly.CPUPercent, floatPtr(0))
	assertFloatPtr(t, cpuOnly.MemoryPercent, nil)

	memOnly := parseDarwinTop("PhysMem: 1.5G used, 512M unused.\n")
	assertFloatPtr(t, memOnly.CPUPercent, nil)
	assertFloatPtr(t, memOnly.MemoryPercent, floatPtr(75))
}

func TestParseDarwinTopBoundariesAndUnits(t *testing.T) {
	cases := []struct {
		name string
		out  string
		cpu  *float64
		mem  *float64
	}{
		{"all idle", "CPU usage: 0% user, 0% sys, 100% idle\nPhysMem: 0B used, 1T unused.\n", floatPtr(0), floatPtr(0)},
		{"all busy", "CPU usage: 100% user, 0% sys, 0% idle\nPhysMem: 1T used, 0B unused.\n", floatPtr(100), floatPtr(100)},
		{"kilobytes", "PhysMem: 768K used, 256K unused.\n", nil, floatPtr(75)},
		{"decimal gigabytes", "PhysMem: 1.5G used, 0.5G unused.\n", nil, floatPtr(75)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseDarwinTop(tc.out)
			assertFloatPtr(t, got.CPUPercent, tc.cpu)
			assertFloatPtr(t, got.MemoryPercent, tc.mem)
		})
	}
}

func TestParseDarwinTopRejectsMalformedOutput(t *testing.T) {
	for _, out := range []string{
		"CPU usage: nope idle\n",
		"CPU usage: 10,0% user, 90,0% idle\n",
		"PhysMem: nope used, 1G unused.\n",
		"PhysMem: 1G used\n",
	} {
		got := parseDarwinTop(out)
		if got.CPUPercent != nil || got.MemoryPercent != nil {
			t.Fatalf("parseDarwinTop(%q) = %#v, want unavailable", out, got)
		}
	}
}
```

- [ ] **Step 3: Run parser tests and verify expected compile failure**

Run:

```sh
go test ./... -run 'Test(ParseLinux|LinuxCPUPercent|ParseDarwin)'
```

Expected: FAIL because `HostUsage`, `linuxCPUTimes`, `parseLinuxCPUTimes`, `linuxCPUPercent`, `parseLinuxMemory`, and `parseDarwinTop` do not exist.

- [ ] **Step 4: Add shared models and percentage guard**

Create `host_usage.go`:

```go
package main

import "math"

// HostUsage is one whole-host resource snapshot. Nil fields mean unavailable;
// pointers preserve a valid zero percentage through JSON omitempty.
type HostUsage struct {
	CPUPercent    *float64 `json:"cpuPercent,omitempty"`
	MemoryPercent *float64 `json:"memoryPercent,omitempty"`
}

// LocalHost groups the current machine's identity, sessions, and resource
// snapshot for rendering. RemoteResult is the corresponding remote shape.
type LocalHost struct {
	Name      string
	Sessions  []Session
	HostUsage HostUsage
}

func hostPercent(v float64) *float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	v = max(0, min(100, v))
	return &v
}
```

- [ ] **Step 5: Add Linux pure parsers**

Create `host_usage_proc.go`:

```go
package main

import (
	"strconv"
	"strings"
)

type linuxCPUTimes struct {
	total uint64
	idle  uint64
}

func parseLinuxCPUTimes(data string) (linuxCPUTimes, bool) {
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "cpu" {
			continue
		}
		if len(fields) < 9 {
			return linuxCPUTimes{}, false
		}
		values := make([]uint64, 8)
		for i := range values {
			v, err := strconv.ParseUint(fields[i+1], 10, 64)
			if err != nil {
				return linuxCPUTimes{}, false
			}
			values[i] = v
		}
		var total uint64
		for _, v := range values {
			total += v
		}
		return linuxCPUTimes{total: total, idle: values[3] + values[4]}, true
	}
	return linuxCPUTimes{}, false
}

func linuxCPUPercent(prev, next linuxCPUTimes) *float64 {
	if next.total <= prev.total || next.idle < prev.idle {
		return nil
	}
	totalDelta := next.total - prev.total
	idleDelta := next.idle - prev.idle
	if idleDelta > totalDelta {
		return nil
	}
	return hostPercent(float64(totalDelta-idleDelta) / float64(totalDelta) * 100)
}

func parseLinuxMemory(data string) *float64 {
	var total, available uint64
	var haveTotal, haveAvailable bool
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		key := strings.TrimSuffix(fields[0], ":")
		if key != "MemTotal" && key != "MemAvailable" {
			continue
		}
		v, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return nil
		}
		switch key {
		case "MemTotal":
			total, haveTotal = v, true
		case "MemAvailable":
			available, haveAvailable = v, true
		}
	}
	if !haveTotal || !haveAvailable || total == 0 || available > total {
		return nil
	}
	return hostPercent(float64(total-available) / float64(total) * 100)
}
```

- [ ] **Step 6: Add macOS pure parser**

Create `host_usage_top.go`:

```go
package main

import (
	"strconv"
	"strings"
)

func parseDarwinTop(out string) HostUsage {
	var cpuLine, memoryLine string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "CPU usage:") {
			cpuLine = line
		}
		if strings.HasPrefix(line, "PhysMem:") {
			memoryLine = line
		}
	}
	return HostUsage{
		CPUPercent:    parseDarwinCPU(cpuLine),
		MemoryPercent: parseDarwinMemory(memoryLine),
	}
}

func parseDarwinCPU(line string) *float64 {
	parts := strings.Split(line, ",")
	if len(parts) != 3 {
		return nil
	}
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if !strings.HasSuffix(part, "% idle") {
			continue
		}
		number := strings.TrimSuffix(part, "% idle")
		idle, err := strconv.ParseFloat(strings.TrimSpace(number), 64)
		if err != nil {
			return nil
		}
		return hostPercent(100 - idle)
	}
	return nil
}

func parseDarwinMemory(line string) *float64 {
	usedToken, ok := tokenBeforeMarker(line, " used")
	if !ok {
		return nil
	}
	unusedToken, ok := tokenBeforeMarker(line, " unused")
	if !ok {
		return nil
	}
	used, ok := parseDarwinSize(usedToken)
	if !ok {
		return nil
	}
	unused, ok := parseDarwinSize(unusedToken)
	if !ok || used+unused == 0 {
		return nil
	}
	return hostPercent(used / (used + unused) * 100)
}

func tokenBeforeMarker(s, marker string) (string, bool) {
	i := strings.LastIndex(s, marker)
	if i < 0 {
		return "", false
	}
	fields := strings.Fields(s[:i])
	if len(fields) == 0 {
		return "", false
	}
	return strings.Trim(fields[len(fields)-1], "(),."), true
}

func parseDarwinSize(s string) (float64, bool) {
	s = strings.TrimSpace(strings.TrimSuffix(s, "+"))
	if s == "" {
		return 0, false
	}
	multipliers := map[byte]float64{
		'B': 1,
		'K': 1 << 10,
		'M': 1 << 20,
		'G': 1 << 30,
		'T': 1 << 40,
	}
	unit := s[len(s)-1]
	multiplier, ok := multipliers[unit]
	if ok {
		s = s[:len(s)-1]
	} else {
		multiplier = 1
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 {
		return 0, false
	}
	return v * multiplier, true
}
```

- [ ] **Step 7: Run parser tests**

Run:

```sh
gofmt -w host_usage.go host_usage_proc.go host_usage_top.go host_usage_proc_test.go host_usage_top_test.go
go test ./... -run 'Test(ParseLinux|LinuxCPUPercent|ParseDarwin)'
```

Expected: PASS.

- [ ] **Step 8: Run package gate and commit**

Run:

```sh
go build ./...
go test ./...
git add host_usage.go host_usage_proc.go host_usage_top.go host_usage_proc_test.go host_usage_top_test.go
git commit -m "feat: parse host resource usage" -m "Co-Authored-By: Claude <noreply@anthropic.com>"
```

Expected: build/tests PASS; commit contains models and pure parsers only.

---

### Task 2: Add Platform Collectors and Cached Host Usage Hub

**Files:**
- Modify: `host_usage.go`
- Modify: `host_usage_proc.go`
- Modify: `host_usage_top.go`
- Modify: `host_usage_proc_test.go`
- Create: `host_usage_hub_test.go`

**Interfaces:**
- Consumes: `HostUsage`, parser functions, and `hostPercent` from Task 1.
- Produces:
  - `type hostUsageCollector interface { Sample(context.Context) HostUsage }`
  - `func CollectHostUsage() HostUsage`
  - `func NewHostUsageHub(time.Duration) *HostUsageHub`
  - `func (h *HostUsageHub) Snapshot() HostUsage`
  - `Pause`, `Resume`, `Kick`, and idempotent `Shutdown` methods.

- [ ] **Step 1: Write failing hub lifecycle tests**

Create `host_usage_hub_test.go`:

```go
package main

import (
	"context"
	"sync"
	"testing"
	"time"
)

type fakeHostUsageCollector struct {
	mu      sync.Mutex
	samples   []HostUsage
	calls     int
	active    int
	maxActive int
	started   chan struct{}
	block     bool
}

func (f *fakeHostUsageCollector) Sample(ctx context.Context) HostUsage {
	f.mu.Lock()
	index := f.calls
	f.calls++
	f.active++
	f.maxActive = max(f.maxActive, f.active)
	block := f.block && index > 0
	started := f.started
	var sample HostUsage
	if len(f.samples) > 0 {
		sample = f.samples[min(index, len(f.samples)-1)]
	}
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.active--
		f.mu.Unlock()
	}()

	if block {
		select {
		case started <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return HostUsage{}
	}
	return sample
}

func (f *fakeHostUsageCollector) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeHostUsageCollector) maxConcurrent() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxActive
}

func TestHostUsageHubPublishesInitialAndKickedSamples(t *testing.T) {
	collector := &fakeHostUsageCollector{samples: []HostUsage{
		{CPUPercent: floatPtr(10), MemoryPercent: floatPtr(20)},
		{CPUPercent: floatPtr(30), MemoryPercent: floatPtr(40)},
	}}
	hub := newHostUsageHubWithCollector(collector, time.Hour)
	defer hub.Shutdown()
	assertFloatPtr(t, hub.Snapshot().CPUPercent, floatPtr(10))

	hub.Kick()
	waitFor(t, time.Second, func() bool {
		return hub.Snapshot().CPUPercent != nil && *hub.Snapshot().CPUPercent == 30
	})
	assertFloatPtr(t, hub.Snapshot().MemoryPercent, floatPtr(40))
}

func TestHostUsageHubPauseResume(t *testing.T) {
	collector := &fakeHostUsageCollector{samples: []HostUsage{{CPUPercent: floatPtr(1)}}}
	hub := newHostUsageHubWithCollector(collector, time.Hour)
	defer hub.Shutdown()

	hub.Pause()
	hub.Kick()
	time.Sleep(20 * time.Millisecond)
	if got := collector.callCount(); got != 1 {
		t.Fatalf("calls while paused = %d, want 1", got)
	}
	hub.Resume()
	waitFor(t, time.Second, func() bool { return collector.callCount() >= 2 })
}

func TestHostUsageHubConcurrentSnapshots(t *testing.T) {
	collector := &fakeHostUsageCollector{samples: []HostUsage{{CPUPercent: floatPtr(50), MemoryPercent: floatPtr(60)}}}
	hub := newHostUsageHubWithCollector(collector, time.Hour)
	defer hub.Shutdown()

	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				_ = hub.Snapshot()
			}
		}()
	}
	wg.Wait()
}

func TestHostUsageHubSerializesCollectorCalls(t *testing.T) {
	collector := &fakeHostUsageCollector{samples: []HostUsage{{CPUPercent: floatPtr(10)}}}
	hub := newHostUsageHubWithCollector(collector, time.Millisecond)
	defer hub.Shutdown()
	waitFor(t, time.Second, func() bool { return collector.callCount() >= 3 })
	if got := collector.maxConcurrent(); got != 1 {
		t.Fatalf("max concurrent samples = %d, want 1", got)
	}
}

func TestHostUsageHubRetriesUnavailableSample(t *testing.T) {
	collector := &fakeHostUsageCollector{samples: []HostUsage{
		{},
		{CPUPercent: floatPtr(25)},
	}}
	hub := newHostUsageHubWithCollector(collector, time.Hour)
	defer hub.Shutdown()
	if hub.Snapshot().CPUPercent != nil {
		t.Fatal("initial CPU should be unavailable")
	}
	hub.Kick()
	waitFor(t, time.Second, func() bool { return hub.Snapshot().CPUPercent != nil })
	assertFloatPtr(t, hub.Snapshot().CPUPercent, floatPtr(25))
}

func TestHostUsageHubShutdownCancelsSample(t *testing.T) {
	collector := &fakeHostUsageCollector{
		samples: []HostUsage{{CPUPercent: floatPtr(1)}},
		started: make(chan struct{}, 1),
		block:   true,
	}
	hub := newHostUsageHubWithCollector(collector, time.Hour)
	hub.Kick()
	select {
	case <-collector.started:
	case <-time.After(time.Second):
		t.Fatal("blocked sample did not start")
	}
	done := make(chan struct{})
	go func() {
		hub.Shutdown()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Shutdown did not cancel blocked sample")
	}
	// Idempotency must not panic or block.
	hub.Shutdown()
}

func waitFor(t *testing.T, timeout time.Duration, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition did not become true before timeout")
}
```

- [ ] **Step 2: Write failing Linux bootstrap-cancellation test**

Append to `host_usage_proc_test.go`:

```go
func TestLinuxCollectorBootstrapRespectsCancellation(t *testing.T) {
	reads := 0
	collector := &linuxHostUsageCollector{
		readFile: func(path string) ([]byte, error) {
			if path == "/proc/meminfo" {
				return []byte("MemTotal: 1000 kB\nMemAvailable: 500 kB\n"), nil
			}
			reads++
			return []byte("cpu 1 1 1 1 1 1 1 1\n"), nil
		},
		primingDelay: time.Hour,
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got := collector.Sample(ctx)
	if reads != 1 {
		t.Fatalf("stat reads = %d, want 1 after cancellation", reads)
	}
	if got.CPUPercent != nil {
		t.Fatal("CPU should be unavailable after canceled bootstrap")
	}
	assertFloatPtr(t, got.MemoryPercent, floatPtr(50))
}
```

Add `context` and `time` imports to that test file.

- [ ] **Step 3: Run focused tests and verify expected compile failure**

Run:

```sh
go test ./... -run 'Test(HostUsageHub|LinuxCollector)'
```

Expected: FAIL because collector and hub types/functions do not exist.

- [ ] **Step 4: Add collector interface, runtime selection, one-shot collection, and hub**

Expand `host_usage.go` to:

```go
package main

import (
	"context"
	"math"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

const hostUsageSampleTimeout = 2 * time.Second

// HostUsage is one whole-host resource snapshot. Nil fields mean unavailable;
// pointers preserve a valid zero percentage through JSON omitempty.
type HostUsage struct {
	CPUPercent    *float64 `json:"cpuPercent,omitempty"`
	MemoryPercent *float64 `json:"memoryPercent,omitempty"`
}

// LocalHost groups the current machine's identity, sessions, and resource
// snapshot for rendering. RemoteResult is the corresponding remote shape.
type LocalHost struct {
	Name      string
	Sessions  []Session
	HostUsage HostUsage
}

func hostPercent(v float64) *float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	v = max(0, min(100, v))
	return &v
}

type hostUsageCollector interface {
	Sample(context.Context) HostUsage
}

type unavailableHostUsageCollector struct{}

func (unavailableHostUsageCollector) Sample(context.Context) HostUsage { return HostUsage{} }

func newHostUsageCollector() hostUsageCollector {
	switch runtime.GOOS {
	case "linux":
		return newLinuxHostUsageCollector()
	case "darwin":
		return newDarwinHostUsageCollector()
	default:
		return unavailableHostUsageCollector{}
	}
}

func CollectHostUsage() HostUsage {
	ctx, cancel := context.WithTimeout(context.Background(), hostUsageSampleTimeout)
	defer cancel()
	return newHostUsageCollector().Sample(ctx)
}

// HostUsageHub serializes one platform collector and caches its latest result.
type HostUsageHub struct {
	mu        sync.RWMutex
	current   HostUsage
	collector hostUsageCollector
	interval  time.Duration
	paused    atomic.Bool
	kick      chan struct{}
	ctx       context.Context
	cancel    context.CancelFunc
	done      chan struct{}
	shutdown  sync.Once
}

func NewHostUsageHub(interval time.Duration) *HostUsageHub {
	return newHostUsageHubWithCollector(newHostUsageCollector(), interval)
}

func newHostUsageHubWithCollector(collector hostUsageCollector, interval time.Duration) *HostUsageHub {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &HostUsageHub{
		collector: collector,
		interval:  interval,
		kick:      make(chan struct{}, 1),
		ctx:       ctx,
		cancel:    cancel,
		done:      make(chan struct{}),
	}
	h.sample()
	go h.run()
	return h
}

func (h *HostUsageHub) sample() {
	ctx, cancel := context.WithTimeout(h.ctx, hostUsageSampleTimeout)
	usage := h.collector.Sample(ctx)
	cancel()
	h.mu.Lock()
	h.current = usage
	h.mu.Unlock()
}

func (h *HostUsageHub) run() {
	defer close(h.done)
	ticker := time.NewTicker(h.interval)
	defer ticker.Stop()
	for {
		select {
		case <-h.ctx.Done():
			return
		case <-ticker.C:
		case <-h.kick:
		}
		if h.paused.Load() {
			continue
		}
		h.sample()
	}
}

func (h *HostUsageHub) Snapshot() HostUsage {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.current
}

func (h *HostUsageHub) Pause() { h.paused.Store(true) }

func (h *HostUsageHub) Resume() {
	h.paused.Store(false)
	h.Kick()
}

func (h *HostUsageHub) Kick() {
	select {
	case h.kick <- struct{}{}:
	default:
	}
}

func (h *HostUsageHub) Shutdown() {
	h.shutdown.Do(func() {
		h.cancel()
		<-h.done
	})
}
```

- [ ] **Step 5: Add stateful Linux collector**

Add imports `context`, `os`, and `time` to `host_usage_proc.go`, then append:

```go
const linuxCPUPrimingDelay = 100 * time.Millisecond

type linuxHostUsageCollector struct {
	readFile     func(string) ([]byte, error)
	primingDelay time.Duration
	previous     linuxCPUTimes
	primed       bool
}

func newLinuxHostUsageCollector() hostUsageCollector {
	return &linuxHostUsageCollector{
		readFile:     os.ReadFile,
		primingDelay: linuxCPUPrimingDelay,
	}
}

func (c *linuxHostUsageCollector) Sample(ctx context.Context) HostUsage {
	usage := HostUsage{}
	if data, err := c.readFile("/proc/meminfo"); err == nil {
		usage.MemoryPercent = parseLinuxMemory(string(data))
	}
	usage.CPUPercent = c.sampleCPU(ctx)
	return usage
}

func (c *linuxHostUsageCollector) sampleCPU(ctx context.Context) *float64 {
	data, err := c.readFile("/proc/stat")
	if err != nil {
		return nil
	}
	current, ok := parseLinuxCPUTimes(string(data))
	if !ok {
		return nil
	}
	if !c.primed {
		c.previous = current
		c.primed = true
		timer := time.NewTimer(c.primingDelay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}
		data, err = c.readFile("/proc/stat")
		if err != nil {
			return nil
		}
		current, ok = parseLinuxCPUTimes(string(data))
		if !ok {
			return nil
		}
	}
	pct := linuxCPUPercent(c.previous, current)
	c.previous = current
	return pct
}
```

- [ ] **Step 6: Add macOS command-backed collector**

Add imports `context`, `os`, and `os/exec` to `host_usage_top.go`, then append:

```go
type darwinHostUsageCollector struct {
	runTop func(context.Context) ([]byte, error)
}

func newDarwinHostUsageCollector() hostUsageCollector {
	return &darwinHostUsageCollector{runTop: runDarwinTop}
}

func runDarwinTop(ctx context.Context) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "top", "-l", "2", "-n", "0", "-s", "0")
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	return cmd.Output()
}

func (c *darwinHostUsageCollector) Sample(ctx context.Context) HostUsage {
	out, err := c.runTop(ctx)
	if err != nil {
		return HostUsage{}
	}
	return parseDarwinTop(string(out))
}
```

- [ ] **Step 7: Run hub tests, race detector, and package gate**

Run:

```sh
gofmt -w host_usage.go host_usage_proc.go host_usage_top.go host_usage_proc_test.go host_usage_hub_test.go
go test ./... -run 'Test(HostUsageHub|LinuxCollector)'
go test -race ./... -run 'TestHostUsageHub'
go build ./...
go test ./...
```

Expected: all commands PASS.

- [ ] **Step 8: Commit collector and hub**

Run:

```sh
git add host_usage.go host_usage_proc.go host_usage_top.go host_usage_proc_test.go host_usage_hub_test.go
git commit -m "feat: sample host resource usage" -m "Co-Authored-By: Claude <noreply@anthropic.com>"
```

Expected: commit contains collector/hub lifecycle only.

---

### Task 3: Extend Server and Remote Protocol

**Files:**
- Modify: `remote.go:16-53`
- Modify: `server.go:29-62,343-376`
- Modify: `server_test.go`
- Create: `remote_test.go`

**Interfaces:**
- Consumes: `HostUsage`, `NewHostUsageHub`, `HostUsageHub.Snapshot` from Tasks 1–2.
- Produces:
  - `/sessions.hostUsage` JSON object.
  - `RemoteResult.HostUsage HostUsage`.
  - `server.hostSnapshot func() HostUsage` injection seam.

- [ ] **Step 1: Write failing `/sessions` response tests**

Append to `server_test.go`:

```go
func TestSessionsIncludesHostUsage(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cpu, memory := 12.5, 67.25
	s := &server{
		token: "secret",
		host:  "devbox",
		hostSnapshot: func() HostUsage {
			return HostUsage{CPUPercent: &cpu, MemoryPercent: &memory}
		},
	}
	req := httptest.NewRequest(http.MethodGet, "/sessions", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.sessions(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
	}
	var got struct {
		Hostname  string    `json:"hostname"`
		HostUsage HostUsage `json:"hostUsage"`
		Sessions  []Session `json:"sessions"`
		TS        int64     `json:"ts"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Hostname != "devbox" || got.TS == 0 {
		t.Fatalf("response metadata = %#v", got)
	}
	assertFloatPtr(t, got.HostUsage.CPUPercent, &cpu)
	assertFloatPtr(t, got.HostUsage.MemoryPercent, &memory)
}

func TestSessionsIncludesEmptyHostUsageWhenUnavailable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := &server{token: "secret", host: "devbox"}
	req := httptest.NewRequest(http.MethodGet, "/sessions", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.sessions(rec, req)
	if !strings.Contains(rec.Body.String(), `"hostUsage":{}`) {
		t.Fatalf("response missing empty hostUsage object: %s", rec.Body.String())
	}
}
```

`server_test.go` already imports `encoding/json`, `net/http`, `httptest`, and `strings`; no new imports are needed.

- [ ] **Step 2: Write failing remote compatibility tests**

Create `remote_test.go`:

```go
package main

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
)

func TestFetchRemoteDecodesHostUsageAndTagsSessions(t *testing.T) {
	result := fetchRemoteFixture(t, `{
		"hostUsage":{"cpuPercent":25.5,"memoryPercent":75},
		"sessions":[{"pid":42,"cwd":"/srv/app"}]
	}`)
	assertFloatPtr(t, result.HostUsage.CPUPercent, floatPtr(25.5))
	assertFloatPtr(t, result.HostUsage.MemoryPercent, floatPtr(75))
	if len(result.Sessions) != 1 || result.Sessions[0].Host != "alias" {
		t.Fatalf("sessions = %#v", result.Sessions)
	}
}

func TestFetchRemoteCompatibilityWithMissingAndPartialHostUsage(t *testing.T) {
	missing := fetchRemoteFixture(t, `{"sessions":[]}`)
	if missing.HostUsage.CPUPercent != nil || missing.HostUsage.MemoryPercent != nil {
		t.Fatalf("missing hostUsage decoded as %#v", missing.HostUsage)
	}

	partial := fetchRemoteFixture(t, `{"hostUsage":{"cpuPercent":0},"sessions":[]}`)
	assertFloatPtr(t, partial.HostUsage.CPUPercent, floatPtr(0))
	if partial.HostUsage.MemoryPercent != nil {
		t.Fatalf("partial memory = %v, want nil", partial.HostUsage.MemoryPercent)
	}
}

func fetchRemoteFixture(t *testing.T, body string) RemoteResult {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer token" {
			http.Error(w, "bad auth", http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, body)
	}))
	defer server.Close()
	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	host, portText, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	result := FetchRemote(ServerConfig{Name: "alias", Host: host, Port: port, Token: "token"})
	if result.Error != "" {
		t.Fatalf("FetchRemote error = %q", result.Error)
	}
	return result
}
```

- [ ] **Step 3: Run protocol tests and verify expected failure**

Run:

```sh
go test ./... -run 'Test(SessionsIncludes|FetchRemote)'
```

Expected: FAIL because server and remote response types do not carry `HostUsage`.

- [ ] **Step 4: Extend `RemoteResult` and `FetchRemote`**

Replace `RemoteResult` in `remote.go` with:

```go
type RemoteResult struct {
	Name      string    // server name from config
	Sessions  []Session // empty when Error != ""
	HostUsage HostUsage
	Error     string // "" on success, short reason otherwise
	Loading   bool   // true for a placeholder slot whose first fetch hasn't returned yet
}
```

Replace the local decode struct and success return in `FetchRemote` with:

```go
	var body struct {
		Sessions  []Session `json:"sessions"`
		HostUsage HostUsage `json:"hostUsage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return RemoteResult{Name: srv.Name, Error: "bad response: " + shortErr(err)}
	}
	// Tag every session with the configured host alias so ID(), selection, and
	// remote action routing remain stable even when the server hostname differs.
	for i := range body.Sessions {
		body.Sessions[i].Host = srv.Name
	}
	return RemoteResult{Name: srv.Name, Sessions: body.Sessions, HostUsage: body.HostUsage}
```

- [ ] **Step 5: Add server snapshot injection and response field**

Add this field to `server` in `server.go`:

```go
	hostSnapshot func() HostUsage
```

Replace the success response in `sessions` with:

```go
	hostUsage := HostUsage{}
	if s.hostSnapshot != nil {
		hostUsage = s.hostSnapshot()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"hostname":  s.host,
		"ts":        time.Now().Unix(),
		"hostUsage": hostUsage,
		"sessions":  sessions,
	})
```

Before constructing `server` in `cmdServer`, create one process-wide hub:

```go
	hostUsageHub := NewHostUsageHub(2 * time.Second)
	defer hostUsageHub.Shutdown()

	s := &server{
		token:        tok,
		host:         host,
		hostSnapshot: hostUsageHub.Snapshot,
	}
```

Replace the existing one-line `s := &server{token: tok, host: host}` with that block.

- [ ] **Step 6: Run protocol tests and package gate**

Run:

```sh
gofmt -w remote.go remote_test.go server.go server_test.go
go test ./... -run 'Test(SessionsIncludes|FetchRemote)'
go build ./...
go test ./...
```

Expected: all commands PASS.

- [ ] **Step 7: Commit protocol changes**

Run:

```sh
git add remote.go remote_test.go server.go server_test.go
git commit -m "feat: expose host usage over sessions API" -m "Co-Authored-By: Claude <noreply@anthropic.com>"
```

Expected: one protocol-focused commit; no rendering changes.

---

### Task 4: Migrate Rendering to `LocalHost`

**Files:**
- Modify: `render.go:381-547,695-715,801-819,921-939`
- Modify: `main.go:81-91`
- Modify: `tui.go:102-106,242`
- Modify: `render_test.go`
- Modify: `model_test.go:159`
- Modify: `tui_state_test.go:9-18`

**Interfaces:**
- Consumes: `LocalHost` and `RemoteResult.HostUsage`.
- Produces:
  - `func BuildTableFrame(viewMode string, local LocalHost, remotes []RemoteResult, ...) tableFrame`
  - `func RenderAll(w io.Writer, viewMode string, local LocalHost, remotes []RemoteResult, ...) bool`
  - `section.name`, `section.host`, and `section.hostUsage` fields.
- This task changes types only; output stays byte-equivalent except tests now supply an explicit local name that Task 5 will render.

- [ ] **Step 1: Change render tests to compile only with `LocalHost`**

Add this helper after `findRow` in `render_test.go`:

```go
func testLocalHost(rows ...Session) LocalHost {
	return LocalHost{Name: "local", Sessions: rows}
}
```

Update every `RenderAll`/`BuildTableFrame` local argument in `render_test.go`:

```go
// Slice literals:
RenderAll(&b, mode, testLocalHost(normal, ghost), nil, "", nil, 0, 0, "dir")

// Existing []Session variables:
RenderAll(&buf, "1", testLocalHost(local...), nil, "", nil, 0, 0, "dir")

// Empty local host:
RenderAll(&b, "1", testLocalHost(), nil, "", nil, 0, 0, "dir")
```

Apply the same exact rule to every call listed by:

```sh
rg -n 'RenderAll\(|BuildTableFrame\(' render_test.go
```

Update `model_test.go:159`:

```go
RenderAll(&b, view, LocalHost{Name: "local", Sessions: []Session{s}}, nil, "", nil, 0, 0, "dir")
```

Update `tui_state_test.go:10-12`:

```go
	frame := BuildTableFrame("2",
		LocalHost{Name: "local", Sessions: []Session{{PID: 11, CWD: "/tmp/local"}}},
		[]RemoteResult{{Name: "dev"}}, "11", nil, 100, 0, "dir")
```

- [ ] **Step 2: Run tests and verify compile failure at production signatures**

Run:

```sh
go test ./...
```

Expected: FAIL because `RenderAll` and `BuildTableFrame` still accept `[]Session`.

- [ ] **Step 3: Replace section model and `buildSections`**

Replace `section` and `buildSections` in `render.go` with:

```go
// section is one rendering block. host is the stable selection/action key
// ("" for local, configured alias for remote); name is the visible heading.
type section struct {
	name      string
	host      string
	hostUsage HostUsage
	rows      []Session
	error     string
	loading   bool
}

func buildSections(local LocalHost, remotes []RemoteResult) []section {
	out := make([]section, 0, 1+len(remotes))
	out = append(out, section{
		name:      local.Name,
		host:      "",
		hostUsage: local.HostUsage,
		rows:      local.Sessions,
	})
	for _, r := range remotes {
		out = append(out, section{
			name:      r.Name,
			host:      r.Name,
			hostUsage: r.HostUsage,
			rows:      r.Sessions,
			error:     r.Error,
			loading:   r.Loading,
		})
	}
	return out
}
```

- [ ] **Step 4: Change public rendering signatures**

Replace the signatures and local uses with:

```go
func BuildTableFrame(viewMode string, local LocalHost, remotes []RemoteResult, sel string, usage *UsageInfo, cols, step int, sortMode string) tableFrame {
	sections := buildSections(local, remotes)
	// Existing switch and tableFrame construction remain unchanged.
}

func RenderAll(w io.Writer, viewMode string, local LocalHost, remotes []RemoteResult, sel string, usage *UsageInfo, cols, step int, sortMode string) (overflowing bool) {
	frame := BuildTableFrame(viewMode, local, remotes, sel, usage, cols, step, sortMode)
	io.WriteString(w, strings.Join(frame.lines, "\n"))
	return frame.overflowing
}
```

Update wrappers:

```go
func RenderFull(w io.Writer, sessions []Session, sel string) {
	RenderAll(w, "1", LocalHost{Name: shortHostname(), Sessions: sessions}, nil, sel, nil, 0, 0, "dir")
}

func RenderMinimal(w io.Writer, sessions []Session, sel string) {
	RenderAll(w, "2", LocalHost{Name: shortHostname(), Sessions: sessions}, nil, sel, nil, 0, 0, "dir")
}
```

- [ ] **Step 5: Preserve empty-host selection keys in all render modes**

Change local empty-row calls at `render.go:696`, `render.go:801`, and `render.go:921` to:

```go
renderEmptyHostRow(w, sections[0].host, sel)
```

Change remote empty-row calls to:

```go
renderEmptyHostRow(w, sections[i].host, sel)
```

Change the existing remote visible-name references from `sections[i].label` to `sections[i].name`. Task 5 will replace those label prints with the shared usage heading.

- [ ] **Step 6: Migrate production call sites with zero-value usage**

Change `main.go:90`:

```go
	RenderAll(os.Stdout, LoadViewMode(), LocalHost{
		Name:     shortHostname(),
		Sessions: local,
	}, remotes, "", nil, 0, 0, sortMode)
```

Change `tui.go:242`:

```go
			frame := BuildTableFrame(viewMode, LocalHost{
				Name:     shortHostname(),
				Sessions: local,
			}, remotes, state.sel, usageHub.Snapshot(), cols, 0, sortMode)
```

- [ ] **Step 7: Verify no stale signatures remain**

Run:

```sh
rg -n 'RenderAll\(|BuildTableFrame\(' --glob '*.go'
gofmt -w render.go render_test.go model_test.go tui_state_test.go main.go tui.go
go build ./...
go test ./...
```

Expected: all calls pass `LocalHost`; build/tests PASS. Existing output/selection tests remain green.

- [ ] **Step 8: Commit type migration**

Run:

```sh
git add render.go render_test.go model_test.go tui_state_test.go main.go tui.go
git commit -m "refactor: model local host rendering" -m "Co-Authored-By: Claude <noreply@anthropic.com>"
```

Expected: no host heading output yet; only render model/signature migration.

---

### Task 5: Render CPU and Memory in Every Host Heading

**Files:**
- Modify: `render.go:3-11,399-409,635-715,749-819,873-939`
- Modify: `render_test.go`

**Interfaces:**
- Consumes: `section.name`, `section.hostUsage`, and `HostUsage` pointers.
- Produces:
  - `func formatHostPercent(*float64) string`
  - `func renderHostHeading(io.Writer, section)`
  - local and remote headings in full, intermediate, and minimal modes.

- [ ] **Step 1: Write failing host-heading tests**

Append to `render_test.go`:

```go
func TestFormatHostPercent(t *testing.T) {
	cases := []struct {
		name string
		in   *float64
		want string
	}{
		{"unavailable", nil, "--"},
		{"zero", floatPtr(0), "0%"},
		{"round down", floatPtr(42.4), "42%"},
		{"round half up", floatPtr(42.5), "43%"},
		{"hundred", floatPtr(100), "100%"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatHostPercent(tc.in); got != tc.want {
				t.Fatalf("formatHostPercent() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestHostUsageHeadingsAllViews(t *testing.T) {
	local := LocalHost{
		Name:      "workstation",
		Sessions:  []Session{{PID: 1, CWD: "/local-dir"}},
		HostUsage: HostUsage{CPUPercent: floatPtr(12.5), MemoryPercent: floatPtr(50)},
	}
	remotes := []RemoteResult{{
		Name:      "beluga",
		Sessions:  []Session{{PID: 2, Host: "beluga", CWD: "/remote-dir"}},
		HostUsage: HostUsage{CPUPercent: floatPtr(0)},
	}}
	for _, mode := range []string{"1", "2", "3"} {
		t.Run(mode, func(t *testing.T) {
			var b strings.Builder
			RenderAll(&b, mode, local, remotes, "", nil, 0, 0, "dir")
			out := b.String()
			localHeading := findRow(t, out, "workstation")
			if !strings.Contains(localHeading, "CPU 13%  MEM 50%") {
				t.Fatalf("local heading = %q", localHeading)
			}
			remoteHeading := findRow(t, out, "beluga")
			if !strings.Contains(remoteHeading, "CPU 0%  MEM --") {
				t.Fatalf("remote heading = %q", remoteHeading)
			}
			if strings.Index(out, "workstation") > strings.Index(out, "local-dir") {
				t.Fatal("local heading rendered after local row")
			}
			if strings.Index(out, "beluga") > strings.Index(out, "remote-dir") {
				t.Fatal("remote heading rendered after remote row")
			}
		})
	}
}

func TestHostHeadingPrecedesRemoteStates(t *testing.T) {
	// Keep local populated so the only "(no sessions)" body belongs to the
	// empty remote section under test.
	local := LocalHost{Name: "local", Sessions: []Session{{PID: 1, CWD: "/local-session"}}}
	remotes := []RemoteResult{
		{Name: "loading", Loading: true},
		{Name: "down", Error: "timeout"},
		{Name: "empty"},
	}
	var b strings.Builder
	RenderAll(&b, "1", local, remotes, "", nil, 0, 0, "dir")
	out := b.String()
	for _, tc := range []struct{ host, body string }{
		{"loading", "(loading...)"},
		{"down", "[unreachable: timeout]"},
		{"empty", "(no sessions)"},
	} {
		if strings.Index(out, tc.host) < 0 || strings.Index(out, tc.body) < 0 || strings.Index(out, tc.host) > strings.Index(out, tc.body) {
			t.Fatalf("%s heading/body order wrong:\n%s", tc.host, out)
		}
	}
}
```

- [ ] **Step 2: Run tests and verify expected failure**

Run:

```sh
go test ./... -run 'Test(FormatHostPercent|HostUsageHeadings|HostHeadingPrecedes)'
```

Expected: FAIL because heading helpers and local headings do not exist.

- [ ] **Step 3: Add shared percentage and heading helpers**

Add `math` to `render.go` imports. Add these helpers after `renderEmptyHostRow`:

```go
func formatHostPercent(value *float64) string {
	if value == nil {
		return "--"
	}
	return fmt.Sprintf("%.0f%%", math.Round(*value))
}

func renderHostHeading(w io.Writer, sec section) {
	fmt.Fprintf(w, "  %s  CPU %s  MEM %s\n",
		bold(sec.name),
		formatHostPercent(sec.hostUsage.CPUPercent),
		formatHostPercent(sec.hostUsage.MemoryPercent))
}
```

- [ ] **Step 4: Render local and remote headings in full view**

After the full-view separator at `render.go:648`, add:

```go
	renderHostHeading(w, sections[0])
```

Replace the remote heading at current `render.go:704`:

```go
		renderHostHeading(w, sections[i])
```

Keep the preceding blank line and all loading/error/empty/session branches unchanged.

- [ ] **Step 5: Render headings in intermediate and minimal views**

After the intermediate separator, add:

```go
	renderHostHeading(w, sections[0])
```

Replace its remote heading with:

```go
		renderHostHeading(w, sections[i])
```

After the minimal separator, add:

```go
	renderHostHeading(w, sections[0])
```

Replace its remote heading with:

```go
		renderHostHeading(w, sections[i])
```

- [ ] **Step 6: Run render tests and regression suite**

Run:

```sh
gofmt -w render.go render_test.go
go test ./... -run 'Test(FormatHostPercent|HostUsageHeadings|HostHeadingPrecedes|Empty|RenderAllMatches|RenderHeader)'
go build ./...
go test ./...
```

Expected: all commands PASS. Existing empty-host selection markers and count tests remain unchanged.

- [ ] **Step 7: Commit host headings**

Run:

```sh
git add render.go render_test.go
git commit -m "feat: render host CPU and memory usage" -m "Co-Authored-By: Claude <noreply@anthropic.com>"
```

Expected: output change isolated to host headings.

---

### Task 6: Wire Live TUI and One-Shot Collection, Document, and Verify

**Files:**
- Modify: `tui.go:102-123,218-283,459-462`
- Modify: `main.go:81-91`
- Modify: `README.md:184-187`
- Modify: `docs/superpowers/specs/2026-07-21-host-resource-usage-design.md:179-192,245-251` (already corrected in worktree)
- Create: `docs/superpowers/plans/2026-07-21-host-resource-usage.md` (this plan)

**Interfaces:**
- Consumes: `NewHostUsageHub`, `Snapshot`, `Pause`, `Resume`, `Kick`, `Shutdown`, and `CollectHostUsage`.
- Produces: live local metrics in TUI, synchronous local metrics in `--once`, documentation, and final release-platform verification.

- [ ] **Step 1: Wire one local hub into `RunTUI`**

After the account `usageHub` setup in `tui.go`, add:

```go
	hostUsageHub := NewHostUsageHub(interval)
	defer hostUsageHub.Shutdown()
	localName := shortHostname()
```

Replace the table-frame construction with:

```go
			frame := BuildTableFrame(viewMode, LocalHost{
				Name:      localName,
				Sessions:  local,
				HostUsage: hostUsageHub.Snapshot(),
			}, remotes, state.sel, usageHub.Snapshot(), cols, 0, sortMode)
```

Replace `makeCtx` pause/resume closures with:

```go
			pause: func() {
				hub.Pause()
				usageHub.Pause()
				hostUsageHub.Pause()
			},
			resume: func() {
				hub.Resume()
				usageHub.Resume()
				hostUsageHub.Resume()
			},
```

Add manual host refresh in the `r` key branch:

```go
			case "r", "R":
				usageHub.Kick()
				hostUsageHub.Kick()
				refresh(true)
				render()
```

- [ ] **Step 2: Wire synchronous collection into one-shot output**

Replace `cmdList` in `main.go` with:

```go
func cmdList() error {
	local, err := CollectLocal()
	if err != nil {
		return err
	}
	remotes := FetchAllRemote()
	sortMode := LoadSortMode()
	SortSessions(local, sortMode)
	remotes = sortRemotes(remotes, sortMode)
	RenderAll(os.Stdout, LoadViewMode(), LocalHost{
		Name:      shortHostname(),
		Sessions:  local,
		HostUsage: CollectHostUsage(),
	}, remotes, "", nil, 0, 0, sortMode)
	return nil
}
```

- [ ] **Step 3: Document visible behavior**

After the existing multi-host paragraph in `README.md` that says remote rows appear in their own section, add:

```markdown
Each local or remote host section starts with aggregate host resource usage,
for example `CPU 23%  MEM 61%`. CPU uses a 0–100 whole-machine scale across
all cores; unavailable metrics render as `--` without hiding session rows.
```

Keep the corrected macOS command and final-sample explanation already present in `docs/superpowers/specs/2026-07-21-host-resource-usage-design.md`.

- [ ] **Step 4: Format and run focused integration tests**

Run:

```sh
gofmt -w tui.go main.go
go test ./... -run 'Test(HostUsage|SessionsIncludes|FetchRemote|HostHeading|FormatHost|RenderAllMatches)'
```

Expected: PASS.

- [ ] **Step 5: Run full correctness gates**

Run:

```sh
go test ./...
go test -race ./...
go vet ./...
GOOS=darwin GOARCH=arm64 go build -o /tmp/claude-sessions-darwin-arm64 .
GOOS=linux GOARCH=amd64 go build -o /tmp/claude-sessions-linux-amd64 .
GOOS=linux GOARCH=arm64 go build -o /tmp/claude-sessions-linux-arm64 .
make
```

Expected:

```text
ok github.com/rainder/claude-sessions
→ darwin/arm64
→ linux/amd64
→ linux/arm64
```

No race, vet, test, or cross-build failure. Explicit `GOOS` builds are authoritative because the existing Makefile loop does not stop on an early platform failure.

- [ ] **Step 6: Smoke-test Linux one-shot output**

Run:

```sh
go run . --once | sed -n '1,10p'
```

Expected: output contains a local host heading with both labels, using numeric percentages or `--` if procfs is unavailable:

```text
  <current-hostname>  CPU <number>%  MEM <number>%
```

Also verify remote aliases show the same heading shape when configured servers return the new field.

- [ ] **Step 7: Smoke-test current CPU semantics on a real Mac**

On a macOS/arm64 host, run:

```sh
go run . --once | sed -n '1,10p'
go test ./... >/tmp/claude-sessions-go-test.log &
go run . --once | sed -n '1,10p'
wait
```

Expected: both commands complete without parser errors; the second host CPU value responds directionally to active build/test work instead of remaining a fixed first-sample value. Memory stays within `0–100%`. This runtime check validates `top -l 2 -n 0 -s 0`; cross-compilation alone cannot validate macOS output semantics.

- [ ] **Step 8: Verify dependency and scope constraints**

Run:

```sh
git diff -- go.mod go.sum
git status --short
```

Expected: no `go.mod`/`go.sum` diff. Status contains only intended code, tests, README, corrected design spec, and this plan before the final commit.

- [ ] **Step 9: Commit integration and documentation**

Run:

```sh
git add tui.go main.go README.md docs/superpowers/specs/2026-07-21-host-resource-usage-design.md docs/superpowers/plans/2026-07-21-host-resource-usage.md
git commit -m "feat: wire host resource usage" -m "Co-Authored-By: Claude <noreply@anthropic.com>"
```

Expected: clean worktree after commit.

- [ ] **Step 10: Review final branch diff**

Run:

```sh
git diff main...HEAD --stat
git diff main...HEAD -- host_usage.go host_usage_proc.go host_usage_top.go remote.go server.go render.go tui.go main.go README.md
```

Confirm:

- no new dependency;
- no per-session memory field;
- no sorting/selection/action behavior change;
- server handlers read cached usage rather than sampling;
- local empty-host selection still uses `emptyHostSelectionID("")`;
- all three render modes use `renderHostHeading`;
- macOS command uses two samples and parses the final block.
