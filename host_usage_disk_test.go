package main

import "testing"

func TestDiskUsedPercent(t *testing.T) {
	cases := []struct {
		name                string
		blocks, free, avail uint64
		want                *float64
	}{
		{"half used", 100, 50, 50, floatPtr(50)},
		// 10 blocks reserved for root: df counts used/(used+avail) = 60/80.
		{"reserved blocks excluded", 100, 40, 20, floatPtr(75)},
		{"full for users", 100, 5, 0, floatPtr(100)},
		{"empty", 100, 100, 100, floatPtr(0)},
		{"zero blocks", 0, 0, 0, nil},
		{"free above total", 10, 20, 5, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertFloatPtr(t, diskUsedPercent(tc.blocks, tc.free, tc.avail), tc.want)
		})
	}
}

func TestHomeDiskPercentReadsHome(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if homeDiskPercent() == nil {
		t.Fatal("homeDiskPercent() = nil for a real temp HOME")
	}
}
