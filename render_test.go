package main

import (
	"strings"
	"testing"
	"time"
)

func TestUsageBar(t *testing.T) {
	cases := []struct {
		pct   float64
		width int
		want  string
	}{
		{0, 15, dim(strings.Repeat("░", 15))},
		{100, 15, strings.Repeat("█", 15)},
		{9, 15, "█" + dim(strings.Repeat("░", 14))},   // 9*15/100 = 1.35 → rounds to 1
		{13, 15, "██" + dim(strings.Repeat("░", 13))}, // 13*15/100 = 1.95 → rounds to 2
		{150, 15, strings.Repeat("█", 15)},            // clamped
		{-5, 15, dim(strings.Repeat("░", 15))},        // clamped
		{50, 10, strings.Repeat("█", 5) + dim(strings.Repeat("░", 5))},
		{100, 4, strings.Repeat("█", 4)},
	}
	for _, c := range cases {
		if got := usageBar(c.pct, c.width); got != c.want {
			t.Errorf("usageBar(%v, %d) = %q, want %q", c.pct, c.width, got, c.want)
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
	writeUsage(&b, nil, 0)
	if b.Len() != 0 {
		t.Errorf("writeUsage(nil) wrote %q, want nothing", b.String())
	}
}

// findRow returns the rendered line containing needle, failing if absent.
func findRow(t *testing.T, out, needle string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, needle) {
			return line
		}
	}
	t.Fatalf("no row containing %q in output:\n%s", needle, out)
	return ""
}

func TestHeadlessRowsDimmed(t *testing.T) {
	now := time.Now().UnixMilli()
	normal := Session{PID: 11111, Name: "my-task", CWD: "/tmp/normaldir",
		Status: "busy", Entrypoint: "cli", UpdatedAt: now}
	ghost := Session{PID: 99901, CWD: "/tmp/ghostdir",
		Entrypoint: "sdk-cli", StartedAt: now}

	for _, mode := range []string{"1", "2", "3"} {
		var b strings.Builder
		RenderAll(&b, mode, []Session{normal, ghost}, nil, "", nil, 0)
		out := b.String()

		ghostRow := findRow(t, out, "ghostdir")
		body := strings.TrimPrefix(ghostRow, "  ")
		if !strings.HasPrefix(body, ansiDim) {
			t.Errorf("mode %s: headless row not dimmed: %q", mode, ghostRow)
		}
		// A reset before the end would cancel the dim mid-row.
		if inner := strings.TrimSuffix(strings.TrimPrefix(body, ansiDim), ansiReset); strings.Contains(inner, ansiReset) {
			t.Errorf("mode %s: headless row has mid-row reset: %q", mode, ghostRow)
		}

		normalRow := findRow(t, out, "normaldir")
		if strings.HasPrefix(strings.TrimPrefix(normalRow, "  "), ansiDim) {
			t.Errorf("mode %s: interactive row unexpectedly dimmed: %q", mode, normalRow)
		}
	}
}

func TestClipLine(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		width int
		want  string
	}{
		{"fits", "hello", 10, "hello"},
		{"exact", "hello", 5, "hello"},
		{"cut", "hello world", 5, "hello"},
		{"escapes not counted", "\033[31mbusy\033[0m  32s", 6, "\033[31mbusy\033[0m  "},
		{"reset survives cut", "\033[31mbusy  32s\033[0m", 4, "\033[31mbusy\033[0m"},
		{"multibyte rune one col", "▶ abcdef", 4, "▶ ab"},
		{"zero width keeps escapes", "\033[2mhi\033[0m", 0, "\033[2m\033[0m"},
	}
	for _, c := range cases {
		if got := clipLine(c.in, c.width); got != c.want {
			t.Errorf("%s: clipLine(%q, %d) = %q, want %q", c.name, c.in, c.width, got, c.want)
		}
	}
}

func TestWriteUsage(t *testing.T) {
	var b strings.Builder
	writeUsage(&b, &UsageInfo{
		FiveHour: usageBucket{Pct: 9, ResetsAt: time.Now().Add(2 * time.Hour)},
		SevenDay: usageBucket{Pct: 13, ResetsAt: time.Now().Add(48 * time.Hour)},
	}, 0)
	out := b.String()
	if lines := strings.Count(out, "\n"); lines != 1 {
		t.Errorf("writeUsage wrote %d lines, want 1: %q", lines, out)
	}
	if !strings.Contains(out, "5h") || !strings.Contains(out, "wk") {
		t.Errorf("missing 5h/wk labels: %q", out)
	}
	if !strings.Contains(out, "9%") || !strings.Contains(out, "13%") {
		t.Errorf("missing percentages: %q", out)
	}
	if !strings.Contains(out, "1h") && !strings.Contains(out, "2h") {
		t.Errorf("missing 5h reset countdown: %q", out)
	}
	if !strings.Contains(out, "1d") && !strings.Contains(out, "2d") {
		t.Errorf("missing weekly reset countdown: %q", out)
	}
	if strings.Contains(out, "cr") {
		t.Errorf("credits segment shown with credits disabled: %q", out)
	}
	if got := strings.Count(out, "█") + strings.Count(out, "░"); got != 2*usageBarMax {
		t.Errorf("bar cells = %d, want %d (2 bars × max width)", got, 2*usageBarMax)
	}
}

func TestWriteUsageCredits(t *testing.T) {
	var b strings.Builder
	writeUsage(&b, &UsageInfo{
		FiveHour: usageBucket{Pct: 9, ResetsAt: time.Now().Add(2 * time.Hour)},
		SevenDay: usageBucket{Pct: 13, ResetsAt: time.Now().Add(48 * time.Hour)},
		Credits:  creditsInfo{Enabled: true, Used: 2550, Limit: 100000, Currency: "USD", DecimalPlaces: 2},
	}, 0)
	out := b.String()
	if lines := strings.Count(out, "\n"); lines != 1 {
		t.Errorf("writeUsage wrote %d lines, want 1: %q", lines, out)
	}
	if !strings.Contains(out, "cr") {
		t.Errorf("missing cr label: %q", out)
	}
	if !strings.Contains(out, "3%") {
		t.Errorf("missing rounded credits percentage: %q", out)
	}
	if !strings.Contains(out, "$26/1k") {
		t.Errorf("missing spent/limit figure: %q", out)
	}
}

func TestWriteUsageAdaptiveWidth(t *testing.T) {
	u := &UsageInfo{
		FiveHour: usageBucket{Pct: 9, ResetsAt: time.Now().Add(2 * time.Hour)},
		SevenDay: usageBucket{Pct: 13, ResetsAt: time.Now().Add(48 * time.Hour)},
	}
	bars := func(cols int) int {
		var b strings.Builder
		writeUsage(&b, u, cols)
		return strings.Count(b.String(), "█") + strings.Count(b.String(), "░")
	}
	if got := bars(200); got != 2*usageBarMax {
		t.Errorf("wide terminal: bar cells = %d, want %d", got, 2*usageBarMax)
	}
	if got := bars(35); got >= 2*usageBarMax || got < 2*usageBarMin {
		t.Errorf("narrow terminal: bar cells = %d, want shrunk into [%d, %d)", got, 2*usageBarMin, 2*usageBarMax)
	}
	if got := bars(10); got != 2*usageBarMin {
		t.Errorf("tiny terminal: bar cells = %d, want floor %d", got, 2*usageBarMin)
	}
}

func TestMoneyCompact(t *testing.T) {
	cases := []struct {
		minor  float64
		places int
		want   string
	}{
		{0, 2, "0"},
		{2550, 2, "26"},
		{100000, 2, "1k"},
		{155000, 2, "1.6k"},
		{500, 0, "500"},
		{1500, 0, "1.5k"},
	}
	for _, c := range cases {
		if got := moneyCompact(c.minor, c.places); got != c.want {
			t.Errorf("moneyCompact(%v, %d) = %q, want %q", c.minor, c.places, got, c.want)
		}
	}
}

func TestFormatUntil(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		t    time.Time
		want string
	}{
		{"past", now.Add(-time.Hour), "<1m"},
		{"seconds", now.Add(30 * time.Second), "<1m"},
		{"minutes", now.Add(42*time.Minute + 30*time.Second), "42m"},
		{"hours", now.Add(2*time.Hour + 5*time.Minute + 30*time.Second), "2h"},
		{"days", now.Add(3*24*time.Hour + 4*time.Hour + 30*time.Minute), "3d"},
	}
	for _, c := range cases {
		if got := formatUntil(c.t); got != c.want {
			t.Errorf("%s: formatUntil = %q, want %q", c.name, got, c.want)
		}
	}
}
