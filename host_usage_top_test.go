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
