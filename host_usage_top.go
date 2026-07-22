package main

import (
	"context"
	"os"
	"os/exec"
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
