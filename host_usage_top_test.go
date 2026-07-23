package main

import "testing"

func TestParseDarwinTopUsesFinalSample(t *testing.T) {
	out := `Processes: 500 total
CPU usage: 99.0% user, 0.0% sys, 1.0% idle
Processes: 501 total
CPU usage: 12.5% user, 7.5% sys, 80.0% idle
`
	got := parseDarwinTop(out)
	assertFloatPtr(t, got.CPUPercent, floatPtr(20))
	assertLoadAveragePtr(t, got.Load, nil)
}

func TestParseDarwinTopBoundariesAndUnits(t *testing.T) {
	cases := []struct {
		name string
		out  string
		cpu  *float64
	}{
		{"all idle", "CPU usage: 0% user, 0% sys, 100% idle\n", floatPtr(0)},
		{"all busy", "CPU usage: 100% user, 0% sys, 0% idle\n", floatPtr(100)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseDarwinTop(tc.out)
			assertFloatPtr(t, got.CPUPercent, tc.cpu)
		})
	}
}

func TestParseDarwinTopRejectsMalformedOutput(t *testing.T) {
	for _, out := range []string{
		"CPU usage: nope idle\n",
		"CPU usage: 10,0% user, 90,0% idle\n",
	} {
		got := parseDarwinTop(out)
		if got.CPUPercent != nil {
			t.Fatalf("parseDarwinTop(%q) = %#v, want unavailable", out, got)
		}
	}
}

func TestParseDarwinLoadAverageValid(t *testing.T) {
	cases := []struct {
		name string
		line string
		want *LoadAverage
	}{
		{"normal", "Load Avg: 1.24, 0.96, 0.72", hostLoadAverage(1.24, 0.96, 0.72)},
		{"all zero", "Load Avg: 0, 0, 0", hostLoadAverage(0, 0, 0)},
		{"above 100", "Load Avg: 150.5, 200, 999.99", hostLoadAverage(150.5, 200, 999.99)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseDarwinLoadAverage(tc.line)
			assertLoadAveragePtr(t, got, tc.want)
		})
	}
}

func TestParseDarwinLoadAverageRejectsMalformed(t *testing.T) {
	cases := []struct {
		name string
		line string
	}{
		{"missing prefix", "1.24, 0.96, 0.72"},
		{"localized prefix", "Charge moy.: 1.24, 0.96, 0.72"},
		{"too few", "Load Avg: 1.24, 0.96"},
		{"too many", "Load Avg: 1.24, 0.96, 0.72, 0.50"},
		{"empty member", "Load Avg: 1.24, , 0.72"},
		{"malformed numeric", "Load Avg: nope, 0.96, 0.72"},
		{"negative", "Load Avg: -1.24, 0.96, 0.72"},
		{"NaN", "Load Avg: NaN, 0.96, 0.72"},
		{"positive infinity", "Load Avg: +Inf, 0.96, 0.72"},
		{"negative infinity", "Load Avg: -Inf, 0.96, 0.72"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseDarwinLoadAverage(tc.line)
			if got != nil {
				t.Fatalf("parseDarwinLoadAverage(%q) = %#v, want nil", tc.line, got)
			}
		})
	}
}

func TestParseDarwinTopLoadUsesFinalSample(t *testing.T) {
	out := `Processes: 500 total
CPU usage: 99.0% user, 0.0% sys, 1.0% idle
Load Avg: 1.00, 2.00, 3.00
Processes: 501 total
CPU usage: 12.5% user, 7.5% sys, 80.0% idle
Load Avg: 4.00, 5.00, 6.00
`
	got := parseDarwinTop(out)
	assertLoadAveragePtr(t, got.Load, hostLoadAverage(4, 5, 6))
	assertFloatPtr(t, got.CPUPercent, floatPtr(20))
}

func TestParseDarwinTopLoadIndependentFromCPU(t *testing.T) {
	malformedLoadValidRest := `CPU usage: 0.0% user, 0.0% sys, 100.0% idle
Load Avg: nope, 0.96, 0.72
`
	got := parseDarwinTop(malformedLoadValidRest)
	assertFloatPtr(t, got.CPUPercent, floatPtr(0))
	assertLoadAveragePtr(t, got.Load, nil)

	loadOnly := parseDarwinTop("Load Avg: 1.24, 0.96, 0.72\n")
	assertFloatPtr(t, loadOnly.CPUPercent, nil)
	assertLoadAveragePtr(t, loadOnly.Load, hostLoadAverage(1.24, 0.96, 0.72))

	validLoadMalformedRest := `CPU usage: nope idle
Load Avg: 1.24, 0.96, 0.72
`
	got = parseDarwinTop(validLoadMalformedRest)
	assertFloatPtr(t, got.CPUPercent, nil)
	assertLoadAveragePtr(t, got.Load, hostLoadAverage(1.24, 0.96, 0.72))
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
