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
	assertLoadAveragePtr(t, got.Load, nil)
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
PhysMem: 15G used (2G wired), 1G unused.
Load Avg: 1.00, 2.00, 3.00
Processes: 501 total
CPU usage: 12.5% user, 7.5% sys, 80.0% idle
PhysMem: 12G used (2G wired), 4G unused.
Load Avg: 4.00, 5.00, 6.00
`
	got := parseDarwinTop(out)
	assertLoadAveragePtr(t, got.Load, hostLoadAverage(4, 5, 6))
	assertFloatPtr(t, got.CPUPercent, floatPtr(20))
	assertFloatPtr(t, got.MemoryPercent, floatPtr(75))
}

func TestParseDarwinTopLoadIndependentFromCPUMem(t *testing.T) {
	malformedLoadValidRest := `CPU usage: 0.0% user, 0.0% sys, 100.0% idle
PhysMem: 1.5G used, 512M unused.
Load Avg: nope, 0.96, 0.72
`
	got := parseDarwinTop(malformedLoadValidRest)
	assertFloatPtr(t, got.CPUPercent, floatPtr(0))
	assertFloatPtr(t, got.MemoryPercent, floatPtr(75))
	assertLoadAveragePtr(t, got.Load, nil)

	loadOnly := parseDarwinTop("Load Avg: 1.24, 0.96, 0.72\n")
	assertFloatPtr(t, loadOnly.CPUPercent, nil)
	assertFloatPtr(t, loadOnly.MemoryPercent, nil)
	assertLoadAveragePtr(t, loadOnly.Load, hostLoadAverage(1.24, 0.96, 0.72))

	validLoadMalformedRest := `CPU usage: nope idle
PhysMem: nope used, 1G unused.
Load Avg: 1.24, 0.96, 0.72
`
	got = parseDarwinTop(validLoadMalformedRest)
	assertFloatPtr(t, got.CPUPercent, nil)
	assertFloatPtr(t, got.MemoryPercent, nil)
	assertLoadAveragePtr(t, got.Load, hostLoadAverage(1.24, 0.96, 0.72))
}
