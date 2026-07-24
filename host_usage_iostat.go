package main

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// parseDarwinIostat extracts whole-host CPU and load averages from the output
// of `iostat -c 2 -w 1`.
//
// Why iostat rather than `top -l 2` (the previous source): top's two-sample
// invocation performs a full mach task scan per sample to build the per-process
// figures we never use, costing ~1.15s of mostly-sys CPU on every poll
// (host_usage.go hostUsageInterval, every 2s in both the TUI and server).
// iostat reads the same host CPU aggregate and load averages for ~0.00s CPU.
//
// iostat prints two header lines (per-disk labels, then column units) followed
// by one data line per sample. The first data line is the since-boot average
// and must be ignored; the second — the last data line here — is the 1-second
// interval sample we want. The disk-column count varies by machine (0..N
// disks), so we parse from the end: the trailing six fields are always
// `us sy id 1m 5m 15m` regardless of disk count. Header lines carry
// non-numeric words and so never match numericFields, leaving the last
// all-numeric line as the final data sample. With only one data line present
// (a truncated capture) that line is the last one and gets used, yielding
// since-boot figures instead of nothing — an acceptable degradation.
func parseDarwinIostat(out string) HostUsage {
	var fields []float64
	for _, line := range strings.Split(out, "\n") {
		if nums, ok := numericFields(line); ok {
			fields = nums
		}
	}
	n := len(fields)
	if n < 6 {
		return HostUsage{}
	}
	// f[n-6..n-1] = us sy id 1m 5m 15m. CPU% follows the old "100 - idle"
	// convention; hostPercent / hostLoadAverage reject any NaN/Inf that slips
	// through as a parseable-but-nonsensical value.
	return HostUsage{
		CPUPercent: hostPercent(100 - fields[n-4]),
		Load:       hostLoadAverage(fields[n-3], fields[n-2], fields[n-1]),
	}
}

// numericFields splits line on whitespace and parses every field as a float,
// returning the values and true only when the line is non-empty and all fields
// are numeric. iostat's header and blank lines fail this, so scanning for the
// last line that satisfies it isolates the final data sample.
func numericFields(line string) ([]float64, bool) {
	parts := strings.Fields(line)
	if len(parts) == 0 {
		return nil, false
	}
	values := make([]float64, len(parts))
	for i, part := range parts {
		v, err := strconv.ParseFloat(part, 64)
		if err != nil {
			return nil, false
		}
		values[i] = v
	}
	return values, true
}

// parseDarwinMemory computes real memory pressure from `vm_stat` output the
// way Activity Monitor does: wired + compressed + (anonymous - purgeable),
// over total physical pages. `top`'s PhysMem "used" figure (the previous
// source) lumps in reclaimable inactive/file-cache pages as "used", which on
// a machine with a lot of disk cache wildly overstates real memory pressure
// (observed: top said 97% used while Activity Monitor showed 61%).
func parseDarwinMemory(vmStatOut string, memSizeBytes uint64) *float64 {
	if memSizeBytes == 0 {
		return nil
	}
	pageSize, ok := parseDarwinPageSize(vmStatOut)
	if !ok || pageSize == 0 {
		return nil
	}
	wired, ok := vmStatPages(vmStatOut, "Pages wired down")
	if !ok {
		return nil
	}
	compressed, ok := vmStatPages(vmStatOut, "Pages occupied by compressor")
	if !ok {
		return nil
	}
	anonymous, ok := vmStatPages(vmStatOut, "Anonymous pages")
	if !ok {
		return nil
	}
	purgeable, ok := vmStatPages(vmStatOut, "Pages purgeable")
	if !ok {
		return nil
	}
	if purgeable > anonymous {
		purgeable = anonymous
	}
	totalPages := memSizeBytes / pageSize
	if totalPages == 0 {
		return nil
	}
	usedPages := wired + compressed + (anonymous - purgeable)
	return hostPercent(float64(usedPages) / float64(totalPages) * 100)
}

// parseDarwinPageSize extracts the page size (bytes) from vm_stat's header
// line, e.g. "Mach Virtual Memory Statistics: (page size of 16384 bytes)".
func parseDarwinPageSize(vmStatOut string) (uint64, bool) {
	const marker = "page size of "
	i := strings.Index(vmStatOut, marker)
	if i < 0 {
		return 0, false
	}
	rest := vmStatOut[i+len(marker):]
	j := strings.Index(rest, " bytes")
	if j < 0 {
		return 0, false
	}
	return parseDarwinUint(rest[:j])
}

// vmStatPages returns the page count for a "Label:  NNN." line in vm_stat
// output.
func vmStatPages(vmStatOut, label string) (uint64, bool) {
	prefix := label + ":"
	for _, line := range strings.Split(vmStatOut, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		return parseDarwinUint(strings.TrimSuffix(strings.TrimSpace(strings.TrimPrefix(line, prefix)), "."))
	}
	return 0, false
}

func parseDarwinUint(s string) (uint64, bool) {
	v, err := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

type darwinHostUsageCollector struct {
	runIostat    func(context.Context) ([]byte, error)
	runVMStat    func(context.Context) ([]byte, error)
	memSizeBytes uint64
}

func newDarwinHostUsageCollector() hostUsageCollector {
	memSizeBytes, _ := readDarwinMemSize()
	return &darwinHostUsageCollector{
		runIostat:    runDarwinIostat,
		runVMStat:    runDarwinVMStat,
		memSizeBytes: memSizeBytes,
	}
}

// readDarwinMemSize shells out to `sysctl -n hw.memsize` for total physical
// memory in bytes. Shelling out — rather than x/sys/unix's SysctlUint64,
// which only exists on darwin — keeps this file buildable when
// cross-compiling for linux; the collector it feeds is only ever
// constructed at runtime when GOOS is actually darwin (see host_usage.go).
func readDarwinMemSize() (uint64, bool) {
	out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
	if err != nil {
		return 0, false
	}
	return parseDarwinUint(string(out))
}

func runDarwinIostat(ctx context.Context) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "iostat", "-c", "2", "-w", "1")
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	// Detach from the controlling terminal so iTerm's "show job name in
	// title" doesn't see `iostat` as the pane's foreground process and flip
	// the window title every poll interval (host_usage.go hostUsageInterval).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd.Output()
}

func runDarwinVMStat(ctx context.Context) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "vm_stat")
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	return cmd.Output()
}

func (c *darwinHostUsageCollector) Sample(ctx context.Context) HostUsage {
	usage := HostUsage{}
	if out, err := c.runIostat(ctx); err == nil {
		usage = parseDarwinIostat(string(out))
	}
	if vmOut, err := c.runVMStat(ctx); err == nil {
		usage.MemoryPercent = parseDarwinMemory(string(vmOut), c.memSizeBytes)
	}
	usage.NumCPU = runtime.NumCPU()
	return usage
}
