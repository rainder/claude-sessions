package main

import (
	"strings"
	"testing"
	"time"
)

func TestUsageBar(t *testing.T) {
	cases := []struct {
		pct  float64
		want string
	}{
		{0, strings.Repeat("░", 20)},
		{100, strings.Repeat("█", 20)},
		{9, "██" + strings.Repeat("░", 18)},   // 9/5 = 1.8 → rounds to 2
		{13, "███" + strings.Repeat("░", 17)}, // 13/5 = 2.6 → rounds to 3
		{150, strings.Repeat("█", 20)},        // clamped
		{-5, strings.Repeat("░", 20)},         // clamped
	}
	for _, c := range cases {
		if got := usageBar(c.pct); got != c.want {
			t.Errorf("usageBar(%v) = %q, want %q", c.pct, got, c.want)
		}
	}
}

func TestUsageColor(t *testing.T) {
	cases := []struct {
		pct  float64
		want string
	}{
		{0, ""}, {69.9, ""}, {70, "33"}, {89.9, "33"}, {90, "1;31"}, {100, "1;31"},
	}
	for _, c := range cases {
		if got := usageColor(c.pct); got != c.want {
			t.Errorf("usageColor(%v) = %q, want %q", c.pct, got, c.want)
		}
	}
}

func TestWriteUsageNil(t *testing.T) {
	var b strings.Builder
	writeUsage(&b, nil)
	if b.Len() != 0 {
		t.Errorf("writeUsage(nil) wrote %q, want nothing", b.String())
	}
}

func TestWriteUsage(t *testing.T) {
	var b strings.Builder
	writeUsage(&b, &UsageInfo{
		FiveHour: usageBucket{Pct: 9, ResetsAt: time.Now().Add(2 * time.Hour)},
		SevenDay: usageBucket{Pct: 13, ResetsAt: time.Now().Add(48 * time.Hour)},
	})
	out := b.String()
	if lines := strings.Count(out, "\n"); lines != 2 {
		t.Errorf("writeUsage wrote %d lines, want 2: %q", lines, out)
	}
	if !strings.Contains(out, "5h") || !strings.Contains(out, "wk") {
		t.Errorf("missing 5h/wk labels: %q", out)
	}
	if !strings.Contains(out, "9%") || !strings.Contains(out, "13%") {
		t.Errorf("missing percentages: %q", out)
	}
	if strings.Count(out, "resets ") != 2 {
		t.Errorf("missing reset times: %q", out)
	}
}
