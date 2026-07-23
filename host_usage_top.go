package main

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

func parseDarwinTop(out string) HostUsage {
	var cpuLine, loadLine string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "CPU usage:") {
			cpuLine = line
		}
		if strings.HasPrefix(line, "Load Avg:") {
			loadLine = line
		}
	}
	return HostUsage{
		CPUPercent: parseDarwinCPU(cpuLine),
		Load:       parseDarwinLoadAverage(loadLine),
	}
}

// parseDarwinLoadAverage parses a macOS `top` "Load Avg: 1.24, 0.96, 0.72"
// line into a LoadAverage via hostLoadAverage. Returns nil for a missing or
// wrong prefix, a value count other than three, or any unparsable/invalid
// member (empty, non-numeric, negative, NaN, or infinite).
func parseDarwinLoadAverage(line string) *LoadAverage {
	const prefix = "Load Avg:"
	if !strings.HasPrefix(line, prefix) {
		return nil
	}
	parts := strings.Split(strings.TrimPrefix(line, prefix), ",")
	if len(parts) != 3 {
		return nil
	}
	values := make([]float64, 3)
	for i, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil
		}
		v, err := strconv.ParseFloat(part, 64)
		if err != nil {
			return nil
		}
		values[i] = v
	}
	return hostLoadAverage(values[0], values[1], values[2])
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
	runTop       func(context.Context) ([]byte, error)
	runVMStat    func(context.Context) ([]byte, error)
	memSizeBytes uint64
}

func newDarwinHostUsageCollector() hostUsageCollector {
	memSizeBytes, _ := unix.SysctlUint64("hw.memsize")
	return &darwinHostUsageCollector{
		runTop:       runDarwinTop,
		runVMStat:    runDarwinVMStat,
		memSizeBytes: memSizeBytes,
	}
}

func runDarwinTop(ctx context.Context) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "top", "-l", "2", "-n", "0", "-s", "0")
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	// Detach from the controlling terminal so iTerm's "show job name in
	// title" doesn't see `top` as the pane's foreground process and flip
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
	if out, err := c.runTop(ctx); err == nil {
		usage = parseDarwinTop(string(out))
	}
	if vmOut, err := c.runVMStat(ctx); err == nil {
		usage.MemoryPercent = parseDarwinMemory(string(vmOut), c.memSizeBytes)
	}
	usage.NumCPU = runtime.NumCPU()
	return usage
}
