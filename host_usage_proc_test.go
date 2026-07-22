package main

import (
	"context"
	"testing"
	"time"
)

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
