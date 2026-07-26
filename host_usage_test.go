package main

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func TestHostLoadAverage(t *testing.T) {
	cases := []struct {
		name               string
		one, five, fifteen float64
		want               *LoadAverage
	}{
		{
			name: "normal",
			one:  1.24, five: 0.96, fifteen: 0.72,
			want: &LoadAverage{
				OneMinute:      floatPtr(1.24),
				FiveMinutes:    floatPtr(0.96),
				FifteenMinutes: floatPtr(0.72),
			},
		},
		{
			name: "all zero preserved",
			one:  0, five: 0, fifteen: 0,
			want: &LoadAverage{
				OneMinute:      floatPtr(0),
				FiveMinutes:    floatPtr(0),
				FifteenMinutes: floatPtr(0),
			},
		},
		{
			name: "above 100 unclamped",
			one:  128.5, five: 100, fifteen: 250.75,
			want: &LoadAverage{
				OneMinute:      floatPtr(128.5),
				FiveMinutes:    floatPtr(100),
				FifteenMinutes: floatPtr(250.75),
			},
		},
		{"negative one minute", -0.01, 1, 1, nil},
		{"negative five minute", 1, -1, 1, nil},
		{"negative fifteen minute", 1, 1, -1, nil},
		{"nan one minute", math.NaN(), 1, 1, nil},
		{"positive inf five minute", 1, math.Inf(1), 1, nil},
		{"negative inf fifteen minute", 1, 1, math.Inf(-1), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := hostLoadAverage(tc.one, tc.five, tc.fifteen)
			assertLoadAverage(t, got, tc.want)
		})
	}
}

func assertLoadAverage(t *testing.T, got, want *LoadAverage) {
	t.Helper()
	if got == nil || want == nil {
		if got != nil || want != nil {
			t.Fatalf("got %#v, want %#v", got, want)
		}
		return
	}
	assertFloatPtr(t, got.OneMinute, want.OneMinute)
	assertFloatPtr(t, got.FiveMinutes, want.FiveMinutes)
	assertFloatPtr(t, got.FifteenMinutes, want.FifteenMinutes)
}

// TestHostUsageMatchesGolden pins the hostUsage wire shape for the iOS client.
//
// sessions-golden.json deliberately carries a hostUsage with no loadAverage, so
// the load triple's own field names are unpinned there. They are documented in
// the README and rendered by the app, so they get their own fixture. The Swift
// package decodes this same file.
func TestHostUsageMatchesGolden(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "hostusage-golden.json"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	var got map[string]HostUsage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("golden does not decode against the current shape: %v", err)
	}

	full, ok := got["full"]
	if !ok {
		t.Fatalf("golden is missing the full case")
	}
	if full.CPUPercent == nil || *full.CPUPercent != 23.4 {
		t.Fatalf("cpuPercent = %v, want 23.4", full.CPUPercent)
	}
	if full.MemoryPercent == nil || *full.MemoryPercent != 61.2 {
		t.Fatalf("memoryPercent = %v, want 61.2", full.MemoryPercent)
	}
	if full.NumCPU != 10 {
		t.Fatalf("numCPU = %d, want 10", full.NumCPU)
	}
	if full.Load == nil {
		t.Fatalf("loadAverage did not decode — check the field names")
	}
	if full.Load.OneMinute == nil || *full.Load.OneMinute != 1.24 {
		t.Fatalf("oneMinute = %v, want 1.24", full.Load.OneMinute)
	}
	if full.Load.FiveMinutes == nil || *full.Load.FiveMinutes != 0.96 {
		t.Fatalf("fiveMinutes = %v, want 0.96", full.Load.FiveMinutes)
	}
	if full.Load.FifteenMinutes == nil || *full.Load.FifteenMinutes != 0.72 {
		t.Fatalf("fifteenMinutes = %v, want 0.72", full.Load.FifteenMinutes)
	}

	// A partial triple must survive as a partial triple: the client renders
	// LOAD atomically (all three or none), which it can only decide if the
	// individual absences reach it.
	partial := got["partialLoad"]
	if partial.Load == nil || partial.Load.OneMinute == nil {
		t.Fatalf("partial load did not decode: %+v", partial.Load)
	}
	if partial.Load.FiveMinutes != nil || partial.Load.FifteenMinutes != nil {
		t.Fatalf("partial load invented values: %+v", partial.Load)
	}

	// Everything unavailable is a valid response, not an error.
	if empty := got["empty"]; empty.CPUPercent != nil || empty.Load != nil || empty.NumCPU != 0 {
		t.Fatalf("empty case decoded to %+v", empty)
	}
}
