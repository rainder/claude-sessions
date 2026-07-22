package main

import (
	"math"
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
