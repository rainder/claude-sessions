package main

import (
	"context"
	"runtime"
	"testing"
)

func TestParseDarwinIostat(t *testing.T) {
	cases := []struct {
		name string
		out  string
		cpu  *float64
		load *LoadAverage
	}{
		{
			// Real `iostat -c 2 -w 1` capture. The since-boot first sample
			// (id 91) must be ignored in favor of the interval sample (id 85).
			name: "multi disk uses interval sample not since-boot",
			out: `              disk0              disk10               disk4       cpu    load average
    KB/t  tps  MB/s     KB/t  tps  MB/s     KB/t  tps  MB/s  us sy id   1m   5m   15m
   13.51  281  3.71    24.81    1  0.02    25.19    1  0.03   4  4 91  4.19 4.35 4.69
    6.03  396  2.33     0.00    0  0.00     0.00    0  0.00   6  9 85  4.19 4.35 4.69
`,
			cpu:  floatPtr(15),
			load: hostLoadAverage(4.19, 4.35, 4.69),
		},
		{
			// No disks: each data line is just `us sy id 1m 5m 15m` (6 fields),
			// exercising the n-6 boundary.
			name: "zero disks six fields per sample",
			out: `          cpu    load average
 us sy id   1m   5m   15m
  4  4 91  4.58 6.37 5.39
  6  7 87  4.21 6.26 5.35
`,
			cpu:  floatPtr(13),
			load: hostLoadAverage(4.21, 6.26, 5.35),
		},
		{
			name: "trailing newline and blank lines ignored",
			out: `      disk0       cpu    load average
  KB/t  tps  MB/s  us sy id   1m   5m   15m
 13.53  281  3.71   4  4 91  4.58 6.37 5.39
  5.80  413  2.34   6  7 87  4.21 6.26 5.35


`,
			cpu:  floatPtr(13),
			load: hostLoadAverage(4.21, 6.26, 5.35),
		},
		{
			// Truncated capture: only the since-boot line is present, so it is
			// the last numeric line and gets used (documented degradation).
			name: "single data line falls back to since-boot sample",
			out: `      disk0       cpu    load average
  KB/t  tps  MB/s  us sy id   1m   5m   15m
 13.53  281  3.71   4  4 91  4.58 6.37 5.39
`,
			cpu:  floatPtr(9),
			load: hostLoadAverage(4.58, 6.37, 5.39),
		},
		{name: "empty string", out: "", cpu: nil, load: nil},
		{name: "garbage lines", out: "not iostat output at all\nlorem ipsum dolor\n", cpu: nil, load: nil},
		{name: "header lines only no data", out: "          cpu    load average\n us sy id   1m   5m   15m\n", cpu: nil, load: nil},
		{name: "fewer than six numeric fields", out: " 1 2 3 4 5\n", cpu: nil, load: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseDarwinIostat(tc.out)
			assertFloatPtr(t, got.CPUPercent, tc.cpu)
			assertLoadAveragePtr(t, got.Load, tc.load)
		})
	}
}

func TestDarwinCollectorSampleComposesRunners(t *testing.T) {
	collector := &darwinHostUsageCollector{
		runIostat: func(context.Context) ([]byte, error) {
			return []byte(`          cpu    load average
 us sy id   1m   5m   15m
  4  4 91  1.00 2.00 3.00
  6  7 87  4.00 5.00 6.00
`), nil
		},
		runVMStat: func(context.Context) ([]byte, error) {
			return []byte(sampleVMStatOut), nil
		},
		memSizeBytes: 4194304 * 16384,
	}
	got := collector.Sample(context.Background())
	assertFloatPtr(t, got.CPUPercent, floatPtr(13))
	assertLoadAveragePtr(t, got.Load, hostLoadAverage(4, 5, 6))
	if got.MemoryPercent == nil || *got.MemoryPercent < 62 || *got.MemoryPercent > 63 {
		t.Fatalf("MemoryPercent = %v, want ~62.3", got.MemoryPercent)
	}
	if got.NumCPU != runtime.NumCPU() {
		t.Fatalf("NumCPU = %d, want %d", got.NumCPU, runtime.NumCPU())
	}
}

func TestDarwinCollectorSampleIostatFailureLeavesCPUAndLoadNil(t *testing.T) {
	collector := &darwinHostUsageCollector{
		runIostat: func(context.Context) ([]byte, error) {
			return nil, errTestReadFailure
		},
		runVMStat: func(context.Context) ([]byte, error) {
			return []byte(sampleVMStatOut), nil
		},
		memSizeBytes: 4194304 * 16384,
	}
	got := collector.Sample(context.Background())
	if got.CPUPercent != nil {
		t.Fatalf("CPUPercent = %v, want nil after iostat failure", got.CPUPercent)
	}
	if got.Load != nil {
		t.Fatalf("Load = %+v, want nil after iostat failure", got.Load)
	}
	if got.MemoryPercent == nil {
		t.Fatal("MemoryPercent should still be set when only iostat fails")
	}
}

const sampleVMStatOut = `Mach Virtual Memory Statistics: (page size of 16384 bytes)
Pages free:                                    79771.
Pages active:                                1818265.
Pages inactive:                              1803375.
Pages speculative:                             14828.
Pages throttled:                                   0.
Pages wired down:                             317713.
Pages purgeable:                              103516.
Pages purged:                                2439728.
File-backed pages:                           1339485.
Anonymous pages:                             2296983.
Pages stored in compressor:                   233464.
Pages occupied by compressor:                 101425.
`

func TestParseDarwinMemoryMatchesActivityMonitorNotTopUsed(t *testing.T) {
	// total pages = memSizeBytes / pageSize
	const memSizeBytes = 4194304 * 16384 // 4194304 pages, matching the sample
	got := parseDarwinMemory(sampleVMStatOut, memSizeBytes)
	// used = wired(317713) + compressed(101425) + (anon(2296983) - purgeable(103516)) = 2612605
	// 2612605 / 4194304 * 100 = 62.29...
	if got == nil {
		t.Fatalf("parseDarwinMemory() = nil, want ~62.3")
	}
	if *got < 62 || *got > 63 {
		t.Fatalf("parseDarwinMemory() = %v, want ~62.3 (Activity-Monitor-style, not top's inflated used/unused)", *got)
	}
}

func TestParseDarwinMemoryBoundariesAndUnits(t *testing.T) {
	vmStat := `Mach Virtual Memory Statistics: (page size of 4096 bytes)
Pages wired down:                             100.
Pages occupied by compressor:                 0.
Anonymous pages:                              50.
Pages purgeable:                              0.
`
	// total pages = 1000, used = 100 + 0 + (50 - 0) = 150 -> 15%
	got := parseDarwinMemory(vmStat, 1000*4096)
	assertFloatPtr(t, got, floatPtr(15))
}

func TestParseDarwinMemoryClampsPurgeableAboveAnonymous(t *testing.T) {
	vmStat := `Mach Virtual Memory Statistics: (page size of 4096 bytes)
Pages wired down:                             100.
Pages occupied by compressor:                 0.
Anonymous pages:                              50.
Pages purgeable:                              999.
`
	// purgeable clamped to anonymous, so used = 100 + 0 + 0 = 100 -> 10%
	got := parseDarwinMemory(vmStat, 1000*4096)
	assertFloatPtr(t, got, floatPtr(10))
}

func TestParseDarwinMemoryRejectsMalformedOutput(t *testing.T) {
	for _, out := range []string{
		"",
		"no page size header here\n",
		"Mach Virtual Memory Statistics: (page size of 16384 bytes)\n",
		"Mach Virtual Memory Statistics: (page size of 16384 bytes)\nPages wired down: nope.\n",
	} {
		got := parseDarwinMemory(out, 1024*1024*1024)
		if got != nil {
			t.Fatalf("parseDarwinMemory(%q) = %#v, want nil", out, got)
		}
	}
}

func TestParseDarwinMemoryRejectsMissingMemSize(t *testing.T) {
	got := parseDarwinMemory(sampleVMStatOut, 0)
	if got != nil {
		t.Fatalf("parseDarwinMemory() with memSizeBytes=0 = %#v, want nil", got)
	}
}
