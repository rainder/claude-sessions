package main

import (
	"bytes"
	"math"
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

func TestTmuxViewerPrefix(t *testing.T) {
	zero := 0
	one := 1
	nine := 9
	ten := 10
	negative := -1
	cases := []struct {
		name  string
		s     Session
		plain bool
		want  string
	}{
		{"no tmux", Session{}, false, "  "},
		{"unknown", Session{Tmux: "dev:0.0"}, false, dim("· ")},
		{"detached", Session{Tmux: "dev:0.0", TmuxAttached: &zero}, false, dim("0 ")},
		{"one", Session{Tmux: "dev:0.0", TmuxAttached: &one}, false, colorize("1;32", "1 ")},
		{"nine", Session{Tmux: "dev:0.0", TmuxAttached: &nine}, false, colorize("1;32", "9 ")},
		{"ten", Session{Tmux: "dev:0.0", TmuxAttached: &ten}, false, colorize("1;32", "+ ")},
		{"negative unknown", Session{Tmux: "dev:0.0", TmuxAttached: &negative}, false, dim("· ")},
		{"plain unknown", Session{Tmux: "dev:0.0"}, true, "· "},
		{"plain detached", Session{Tmux: "dev:0.0", TmuxAttached: &zero}, true, "0 "},
		{"plain positive", Session{Tmux: "dev:0.0", TmuxAttached: &one}, true, "1 "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tmuxViewerPrefix(tc.s, tc.plain); got != tc.want {
				t.Errorf("tmuxViewerPrefix() = %q, want %q", got, tc.want)
			}
			if got := visualLen(tmuxViewerPrefix(tc.s, tc.plain)); got != 2 {
				t.Errorf("tmuxViewerPrefix() visual width = %d, want 2", got)
			}
		})
	}
}

func TestHighlightSelectedRow(t *testing.T) {
	if got := highlightSelectedRow("2 row", false); got != "2 row" {
		t.Errorf("unselected row = %q, want unchanged", got)
	}
	want := ansiSelectedBG + "2 row" + ansiReset
	if got := highlightSelectedRow("2 row", true); got != want {
		t.Errorf("selected row = %q, want %q", got, want)
	}
}

func TestHighlightSelectedRowReappliesBackgroundAfterNestedReset(t *testing.T) {
	row := "plain " + colorize("1;31", "busy") + " tail"
	want := ansiSelectedBG + "plain " +
		"\033[1;31mbusy" + ansiReset + ansiSelectedBG +
		" tail" + ansiReset
	if got := highlightSelectedRow(row, true); got != want {
		t.Fatalf("selected styled row = %q, want %q", got, want)
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

func testLocalHost(rows ...Session) LocalHost {
	return LocalHost{Name: "local", Sessions: rows}
}

func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == '\033' && i+1 < len(s) && s[i+1] == '[' {
			i += 2
			for i < len(s) && (s[i] < '@' || s[i] > '~') {
				i++
			}
			if i < len(s) {
				i++
			}
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func assertWholeRowSelected(t *testing.T, row, prefix string) {
	t.Helper()
	if !strings.HasPrefix(row, ansiSelectedBG) {
		t.Fatalf("selected row lacks background prefix: %q", row)
	}
	if !strings.HasPrefix(stripANSI(row), prefix) {
		t.Fatalf("selected row lacks visible prefix %q: %q", prefix, row)
	}
	if !strings.HasSuffix(row, ansiReset) {
		t.Fatalf("selected row lacks final reset: %q", row)
	}
	if strings.Contains(row, ansiInvert) {
		t.Fatalf("selected row still uses reverse video: %q", row)
	}
	if strings.Contains(row, "▶") {
		t.Fatalf("selected row still contains arrow: %q", row)
	}
}

func renderSessionRowForTest(t *testing.T, mode string, s Session, selected bool) string {
	t.Helper()
	sel := ""
	if selected {
		sel = s.ID()
	}
	var b strings.Builder
	RenderAll(&b, mode, testLocalHost(s), nil, sel, nil, 0, 0, "dir")
	return findRow(t, b.String(), s.Name)
}

func stripTrailingReset(s string) string {
	return strings.TrimSuffix(s, ansiReset)
}

func TestSessionRowsHaveOneRightPaddingSpace(t *testing.T) {
	s := Session{
		PID: 42, Name: "padding", NameSource: "user", CWD: "/work/padding",
		Model: "claude-opus-4-8", Status: "busy", Entrypoint: "cli",
		UpdatedAt: time.Now().UnixMilli(), Version: "1.2.3", SessionID: "abcdef123456",
	}
	for _, mode := range []string{"1", "2", "3"} {
		for _, selected := range []bool{false, true} {
			row := stripTrailingReset(renderSessionRowForTest(t, mode, s, selected))
			if !strings.HasSuffix(row, " ") || strings.HasSuffix(row, "  ") {
				t.Errorf("mode %s selected=%v row lacks exactly one trailing space: %q", mode, selected, row)
			}
		}
	}
}

func findHeaderRow(t *testing.T, out string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "NAME") && strings.Contains(line, "AGE") {
			return line
		}
	}
	t.Fatalf("no table header in output:\n%s", out)
	return ""
}

func TestTableHeadersHaveOneRightPaddingSpace(t *testing.T) {
	for _, mode := range []string{"1", "2", "3"} {
		var b strings.Builder
		RenderAll(&b, mode, testLocalHost(Session{PID: 1, Name: "row", CWD: "/work/row"}), nil, "", nil, 0, 0, "dir")
		hdr := findHeaderRow(t, b.String())
		if !strings.HasSuffix(hdr, " ") || strings.HasSuffix(hdr, "  ") {
			t.Errorf("mode %s header lacks exactly one trailing space: %q", mode, hdr)
		}
	}
}

func TestIntermediateStatusPrecedesModel(t *testing.T) {
	s := Session{
		PID: 42, Name: "ordered", NameSource: "user", CWD: "/work/ordered",
		Model: "claude-opus-4-8", Status: "busy", Entrypoint: "cli",
		UpdatedAt: time.Now().UnixMilli(),
	}
	var b strings.Builder
	RenderAll(&b, "3", testLocalHost(s), nil, "", nil, 0, 0, "dir")
	out := b.String()
	hdr := findHeaderRow(t, out)
	if strings.Index(hdr, "STATUS") >= strings.Index(hdr, "MODEL") {
		t.Fatalf("intermediate header order = %q", hdr)
	}
	row := findRow(t, out, "ordered")
	if strings.Index(row, s.StatusDisplay()) >= strings.Index(row, shortModel(s.Model)) {
		t.Fatalf("intermediate row order = %q", row)
	}
}

func TestTmuxViewerPrefixesAcrossModes(t *testing.T) {
	now := time.Now().UnixMilli()
	zero := 0
	one := 1
	nine := 9
	ten := 10
	cases := []struct {
		name     string
		tmux     string
		attached *int
		want     string
	}{
		{"plain", "", nil, "  "},
		{"unknown", "unknown:0.0", nil, dim("· ")},
		{"detached", "detached:0.0", &zero, dim("0 ")},
		{"one", "one:0.0", &one, colorize("1;32", "1 ")},
		{"nine", "nine:0.0", &nine, colorize("1;32", "9 ")},
		{"ten", "ten:0.0", &ten, colorize("1;32", "+ ")},
	}
	for _, mode := range []string{"1", "2", "3"} {
		for i, tc := range cases {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				s := Session{
					PID:          100 + i,
					Name:         tc.name,
					NameSource:   "user",
					CWD:          "/work/" + tc.name,
					Status:       "idle",
					Entrypoint:   "cli",
					UpdatedAt:    now,
					Tmux:         tc.tmux,
					TmuxAttached: tc.attached,
				}
				row := renderSessionRowForTest(t, mode, s, false)
				if !strings.HasPrefix(row, tc.want) {
					t.Errorf("mode %s row prefix = %q, want %q in row %q", mode, row[:len(tc.want)], tc.want, row)
				}
			})
		}
	}
}

func TestSelectedSessionRowsHighlightWholeRow(t *testing.T) {
	now := time.Now().UnixMilli()
	attached := 2
	for _, mode := range []string{"1", "2", "3"} {
		s := Session{
			PID: 42, Name: "selected", NameSource: "user", CWD: "/work/selected",
			Status: "busy", Entrypoint: "cli", UpdatedAt: now,
			Tmux: "selected:0.0", TmuxAttached: &attached,
		}
		selectedRow := renderSessionRowForTest(t, mode, s, true)
		assertWholeRowSelected(t, selectedRow, "2 ")

		unselectedRow := renderSessionRowForTest(t, mode, s, false)
		if visualLen(selectedRow) != visualLen(unselectedRow) {
			t.Errorf("mode %s selected width = %d, unselected width = %d", mode, visualLen(selectedRow), visualLen(unselectedRow))
		}
	}
}

func TestSelectedSessionRowPreservesStatusColor(t *testing.T) {
	s := Session{
		PID: 42, Name: "selected", NameSource: "user", CWD: "/work/selected",
		Status: "busy", Entrypoint: "cli", UpdatedAt: time.Now().UnixMilli(),
	}
	row := renderSessionRowForTest(t, "3", s, true)
	if !strings.Contains(row, "\033[1;31m") {
		t.Fatalf("selected row lost busy foreground color: %q", row)
	}
	if !strings.Contains(row, ansiReset+ansiSelectedBG) {
		t.Fatalf("selected row does not restore background after status reset: %q", row)
	}
}

func TestSelectedHeadlessRowsSuppressDim(t *testing.T) {
	now := time.Now().UnixMilli()
	attached := 1
	for _, mode := range []string{"1", "2", "3"} {
		s := Session{
			PID: 77, Name: "headless", NameSource: "user", CWD: "/work/headless",
			Status: "busy", Entrypoint: "sdk-cli", StartedAt: now,
			Tmux: "headless:0.0", TmuxAttached: &attached,
		}
		row := renderSessionRowForTest(t, mode, s, true)
		assertWholeRowSelected(t, row, "1 ")
		inner := strings.TrimSuffix(strings.TrimPrefix(row, ansiSelectedBG), ansiReset)
		if strings.Contains(inner, ansiDim) {
			t.Errorf("mode %s selected headless row contains dim wrapper: %q", mode, row)
		}
	}
}

func TestTmuxViewerPrefixPreservesWidth(t *testing.T) {
	now := time.Now().UnixMilli()
	attached := 3
	for _, mode := range []string{"1", "2", "3"} {
		unknown := Session{
			PID: 88, Name: "width", NameSource: "user", CWD: "/work/width",
			Status: "idle", Entrypoint: "cli", UpdatedAt: now, Tmux: "width:0.0",
		}
		known := unknown
		known.TmuxAttached = &attached

		var unknownOut, knownOut strings.Builder
		RenderAll(&unknownOut, mode, testLocalHost(unknown), nil, "", nil, 0, 0, "dir")
		RenderAll(&knownOut, mode, testLocalHost(known), nil, "", nil, 0, 0, "dir")
		unknownRow := findRow(t, unknownOut.String(), unknown.Name)
		knownRow := findRow(t, knownOut.String(), known.Name)
		if visualLen(unknownRow) != visualLen(knownRow) {
			t.Errorf("mode %s unknown width = %d, known width = %d", mode, visualLen(unknownRow), visualLen(knownRow))
		}

		headerNeedle := "PID"
		if mode != "1" {
			headerNeedle = "DIR"
		}
		unknownHeader := findRow(t, unknownOut.String(), headerNeedle)
		knownHeader := findRow(t, knownOut.String(), headerNeedle)
		if unknownHeader != knownHeader {
			t.Errorf("mode %s header changed with viewer count:\nunknown: %q\nknown:   %q", mode, unknownHeader, knownHeader)
		}
	}
}

func TestHeadlessRowsDimmed(t *testing.T) {
	now := time.Now().UnixMilli()
	attached := 1
	normal := Session{PID: 11111, Name: "my-task", CWD: "/tmp/normaldir",
		Status: "busy", Entrypoint: "cli", UpdatedAt: now}
	ghost := Session{PID: 99901, CWD: "/tmp/ghostdir",
		Entrypoint: "sdk-cli", StartedAt: now,
		Tmux: "ghost:0.0", TmuxAttached: &attached}

	for _, mode := range []string{"1", "2", "3"} {
		var b strings.Builder
		RenderAll(&b, mode, testLocalHost(normal, ghost), nil, "", nil, 0, 0, "dir")
		out := b.String()

		ghostRow := findRow(t, out, "ghostdir")
		if !strings.HasPrefix(ghostRow, ansiDim+"1 ") {
			t.Errorf("mode %s: headless row and viewer prefix not dimmed: %q", mode, ghostRow)
		}
		// A reset before the end would cancel the dim mid-row.
		if inner := strings.TrimSuffix(strings.TrimPrefix(ghostRow, ansiDim), ansiReset); strings.Contains(inner, ansiReset) {
			t.Errorf("mode %s: headless row has mid-row reset: %q", mode, ghostRow)
		}

		normalRow := findRow(t, out, "normaldir")
		if strings.HasPrefix(strings.TrimPrefix(normalRow, "  "), ansiDim) {
			t.Errorf("mode %s: interactive row unexpectedly dimmed: %q", mode, normalRow)
		}
	}
}

func TestDerivedNameDimmed(t *testing.T) {
	now := time.Now().UnixMilli()
	derived := Session{PID: 100, Name: "der-name", NameSource: "derived",
		CWD: "/tmp/derdir", Status: "busy", Entrypoint: "cli", UpdatedAt: now}
	userSet := Session{PID: 200, Name: "usr-name", NameSource: "user",
		CWD: "/tmp/usrdir", Status: "busy", Entrypoint: "cli", UpdatedAt: now}
	fallback := Session{PID: 300, Tmux: "tmux-sess:0.1",
		CWD: "/tmp/fbdir", Status: "busy", Entrypoint: "cli", UpdatedAt: now}

	for _, mode := range []string{"1", "2", "3"} {
		var b strings.Builder
		RenderAll(&b, mode, testLocalHost(derived, userSet, fallback), nil, "", nil, 0, 0, "dir")
		out := b.String()

		if row := findRow(t, out, "derdir"); !strings.Contains(row, ansiDim+"der-name") {
			t.Errorf("mode %s: derived name not dimmed: %q", mode, row)
		}
		if row := findRow(t, out, "usrdir"); strings.Contains(row, ansiDim+"usr-name") {
			t.Errorf("mode %s: user-set name unexpectedly dimmed: %q", mode, row)
		}
		// A session with nothing but a tmux locator falls all the way through to
		// the "-" placeholder — the tmux session name is never a name fallback.
		row := findRow(t, out, "fbdir")
		if strings.Contains(row, "tmux-sess") && mode != "1" {
			t.Errorf("mode %s: tmux name leaked outside the TMUX column: %q", mode, row)
		}
		if !strings.Contains(row, ansiDim+"-") {
			t.Errorf("mode %s: tmux-only session did not fall back to dimmed %q: %q", mode, "-", row)
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

func TestWriteUsageScopedWeekly(t *testing.T) {
	var b strings.Builder
	writeUsage(&b, &UsageInfo{
		FiveHour:          usageBucket{Pct: 9, ResetsAt: time.Now().Add(2 * time.Hour)},
		SevenDay:          usageBucket{Pct: 13, ResetsAt: time.Now().Add(48 * time.Hour)},
		WeeklyScoped:      usageBucket{Pct: 10, ResetsAt: time.Now().Add(72 * time.Hour)},
		WeeklyScopedLabel: "Fable",
	}, 0)
	out := b.String()
	if lines := strings.Count(out, "\n"); lines != 1 {
		t.Errorf("writeUsage wrote %d lines, want 1: %q", lines, out)
	}
	if !strings.Contains(out, "Fable") {
		t.Errorf("missing scoped weekly label: %q", out)
	}
	if !strings.Contains(out, "10%") {
		t.Errorf("missing scoped weekly percentage: %q", out)
	}
	if got := strings.Count(out, "█") + strings.Count(out, "░"); got != 3*usageBarMax {
		t.Errorf("bar cells = %d, want %d (3 bars × max width)", got, 3*usageBarMax)
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
	if !strings.Contains(out, "$26") || strings.Contains(out, "/") {
		t.Errorf("want spent-only figure $26, no limit: %q", out)
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

func TestMoneyGrouped(t *testing.T) {
	cases := []struct {
		minor  float64
		places int
		want   string
	}{
		{0, 2, "0"},
		{2550, 2, "26"},
		{100000, 2, "1,000"},
		{112345, 2, "1,123"},
		{155000, 2, "1,550"},
		{500, 0, "500"},
		{1500, 0, "1,500"},
		{123456789, 2, "1,234,568"},
	}
	for _, c := range cases {
		if got := moneyGrouped(c.minor, c.places); got != c.want {
			t.Errorf("moneyGrouped(%v, %d) = %q, want %q", c.minor, c.places, got, c.want)
		}
	}
}

func TestFormatTokens(t *testing.T) {
	cases := []struct {
		n    int
		want string
	}{
		{0, "-"},
		{-5, "-"},
		{1, "1"},
		{999, "999"},
		{1000, "1k"},
		{1499, "1k"},
		{1500, "2k"},
		{124362, "124k"},
		{999999, "1000k"},
		{1000000, "1.0M"},
		{1234567, "1.2M"},
	}
	for _, c := range cases {
		if got := formatTokens(c.n); got != c.want {
			t.Errorf("formatTokens(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

func TestFormatCost(t *testing.T) {
	cases := []struct {
		main, sub float64
		want      string
	}{
		{0, 0, "—"},
		{-1, 0, "—"},
		{0.5, 0, "$0.50"},
		{1.234, 0, "$1.23"},
		{99.99, 0, "$99.99"},
		{100, 0, "$100"},
		{1234.5, 0, "$1234"},
		{17.36, 2.56, "$17.36 (+$2.56)"},
		{5, 0.004, "$5.00"},          // subagent part under a cent → suffix omitted
		{5, 0.006, "$5.00 (+$0.01)"}, // just over the threshold → shown
		{150, 20, "$150 (+$20.00)"},  // magnitude rule applies per part
		{0, 3, "$0.00 (+$3.00)"},     // main zero but subagents present
	}
	for _, c := range cases {
		if got := formatCost(c.main, c.sub); got != c.want {
			t.Errorf("formatCost(%v, %v) = %q, want %q", c.main, c.sub, got, c.want)
		}
	}
}

func TestRenderCostColumn(t *testing.T) {
	priced := Session{PID: 1, Name: "paid", CWD: "/tmp/paid", Status: "idle",
		Model: "claude-fable-5", CostUSD: 12.34, CostSubagentsUSD: 2.56,
		UpdatedAt: time.Now().UnixMilli()}
	free := Session{PID: 2, Name: "free", CWD: "/tmp/free", Status: "idle",
		Model: "claude-fable-5", CostUSD: 0, UpdatedAt: time.Now().UnixMilli()}

	// Full and intermediate views carry the column; minimal does not.
	for _, view := range []string{"1", "3"} {
		var b strings.Builder
		RenderAll(&b, view, testLocalHost(priced, free), nil, "", nil, 0, 0, "dir")
		out := b.String()
		if !strings.Contains(out, "COST") {
			t.Errorf("view %s: missing COST header:\n%s", view, out)
		}
		if !strings.Contains(findRow(t, out, "paid"), "$12.34 (+$2.56)") {
			t.Errorf("view %s: missing split cost:\n%s", view, out)
		}
		if !strings.Contains(findRow(t, out, "free"), "—") {
			t.Errorf("view %s: zero cost not rendered as em-dash:\n%s", view, out)
		}
	}

	var b strings.Builder
	RenderAll(&b, "2", testLocalHost(priced), nil, "", nil, 0, 0, "dir")
	if strings.Contains(b.String(), "COST") {
		t.Errorf("minimal view unexpectedly has COST column:\n%s", b.String())
	}
}

func TestMarqueeCell(t *testing.T) {
	cases := []struct {
		name   string
		s      string
		width  int
		offset int
		want   string
	}{
		{"fits pads", "abc", 5, 0, "abc  "},
		{"fits offset ignored", "abc", 5, 3, "abc  "},
		{"exact fit", "hello", 5, 0, "hello"},
		// "abcdef" w3: d=3, pause=3, period=11. pos by t:
		// 0,0,0, 1,2,3, 3,3,3, 2,1 then wrap.
		{"overflow hold at start", "abcdef", 3, 0, "abc"},
		{"overflow still holding", "abcdef", 3, 2, "abc"},
		{"overflow first slide", "abcdef", 3, 3, "bcd"},
		{"overflow tail reached", "abcdef", 3, 5, "def"},
		{"overflow hold at tail", "abcdef", 3, 8, "def"},
		{"overflow slide back", "abcdef", 3, 9, "cde"},
		{"overflow last step back", "abcdef", 3, 10, "bcd"},
		{"period wraps to zero", "abcdef", 3, 11, "abc"},
		{"multibyte static", "αβ", 4, 0, "αβ  "},
		{"multibyte overflow", "αβγδε", 3, 0, "αβγ"},
		{"multibyte tail", "αβγδε", 3, 4, "γδε"},   // d=2, t=4 → pos 2
		{"multibyte return", "αβγδε", 3, 8, "βγδ"}, // period 9, t=8 → pos 1
		{"zero width", "abcdef", 0, 0, ""},
		{"negative width", "abc", -3, 0, ""},
	}
	for _, c := range cases {
		if got := marqueeCell(c.s, c.width, c.offset); got != c.want {
			t.Errorf("%s: marqueeCell(%q, %d, %d) = %q, want %q", c.name, c.s, c.width, c.offset, got, c.want)
		}
	}
}

func TestShrinkDirW(t *testing.T) {
	cases := []struct {
		name              string
		dirW, lineW, cols int
		want              int
	}{
		{"unknown width", 40, 120, 0, 40},
		{"fits", 40, 100, 120, 40},
		{"exact fit", 40, 120, 120, 40},
		{"shrink by deficit", 40, 130, 120, 30}, // over 10 → 40-10
		{"clamp at min", 40, 200, 120, minDirW}, // over 80 → 40-80 < 16 → 16
		{"never widens", 10, 130, 120, 10},      // dirW already < minDirW: clamp must not grow it
	}
	for _, c := range cases {
		if got := shrinkDirW(c.dirW, c.lineW, c.cols); got != c.want {
			t.Errorf("%s: shrinkDirW(%d, %d, %d) = %d, want %d", c.name, c.dirW, c.lineW, c.cols, got, c.want)
		}
	}
}

func TestDisplayCWD(t *testing.T) {
	cases := []struct{ cwd, home, want string }{
		{"/home/andy", "/home/andy", "~"},
		{"/home/andy/project", "/home/andy", "~/project"},
		{"/home/andy-other/project", "/home/andy", "/home/andy-other/project"},
		{"/var/tmp", "/home/andy", "/var/tmp"},
		{"/home/andy/project", "", "/home/andy/project"},
	}
	for _, tc := range cases {
		if got := displayCWD(tc.cwd, tc.home); got != tc.want {
			t.Errorf("displayCWD(%q, %q) = %q, want %q", tc.cwd, tc.home, got, tc.want)
		}
	}
}

func TestDeriveFullUsesSessionHome(t *testing.T) {
	now := time.Unix(100, 0)
	cases := []struct {
		name string
		s    Session
		want string
	}{
		{"local", Session{CWD: "/home/andy/project", Home: "/home/andy"}, "~/project"},
		{"remote", Session{CWD: "/home/rue/service", Home: "/home/rue", Host: "beluga"}, "~/service"},
		// No Home (older server): no tilde collapse. cwdStr is post-squashPath,
		// which abbreviates every non-tail component of the absolute path.
		{"old remote", Session{CWD: "/home/rue/service", Host: "beluga"}, "/h/r/service"},
	}
	for _, tc := range cases {
		row := deriveFull(tc.s, now, "dir")
		if row.cwdStr != tc.want {
			t.Errorf("%s cwd = %q, want %q", tc.name, row.cwdStr, tc.want)
		}
	}
}

func TestRenderMarqueeOverflow(t *testing.T) {
	// A long, character-varied basename forces the minimal-view DIR cell to
	// overflow once the column is shrunk to its floor on a narrow terminal.
	longDir := "abcdefghijklmnopqrstuvwxyz0123456789ABCD" // 40 distinct-ish runes
	s := Session{PID: 1, Name: "marq", CWD: "/tmp/" + longDir, Status: "idle",
		UpdatedAt: time.Now().UnixMilli()}

	// Wide terminal: no shrink, DIR fits, nothing overflows.
	var wide strings.Builder
	if RenderAll(&wide, "2", testLocalHost(s), nil, "", nil, 200, 0, "dir") {
		t.Errorf("wide terminal reported overflow: %s", wide.String())
	}
	if !strings.Contains(wide.String(), longDir) {
		t.Errorf("wide terminal clipped the full path: %s", wide.String())
	}

	// Narrow terminal: DIR shrinks and the cell marquees, so RenderAll reports
	// overflow and successive steps render different windows.
	frame := func(step int) string {
		var b strings.Builder
		if !RenderAll(&b, "2", testLocalHost(s), nil, "", nil, 30, step, "dir") {
			t.Fatalf("narrow terminal did not report overflow at step %d", step)
		}
		return findRow(t, b.String(), "marq")
	}
	if frame(0) == frame(3) {
		t.Errorf("marquee did not advance between steps 0 and 3:\n%s", frame(0))
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

func TestSortIndicator(t *testing.T) {
	now := time.Now()
	// Started 2h ago, updated just now: the AGE cell distinguishes the
	// created basis ("2h") from the updated basis ("0s").
	s := Session{PID: 7, Name: "srt", CWD: "/tmp/srt", Status: "idle",
		StartedAt: now.Add(-2 * time.Hour).UnixMilli(), UpdatedAt: now.UnixMilli()}

	renderWith := func(view, mode string) string {
		var b strings.Builder
		RenderAll(&b, view, testLocalHost(s), nil, "", nil, 0, 0, mode)
		return b.String()
	}

	for _, view := range []string{"1", "2", "3"} {
		if out := renderWith(view, "dir"); !strings.Contains(out, "DIR▲") || strings.Contains(out, "AGE▲") || strings.Contains(out, "AGE▼") {
			t.Errorf("view %s dir: want DIR▲ only, got header in:\n%s", view, out)
		}
		if out := renderWith(view, "updated"); !strings.Contains(out, "AGE▼") || strings.Contains(out, "DIR▲") {
			t.Errorf("view %s updated: want AGE▼ only:\n%s", view, out)
		}
		if out := renderWith(view, "created-asc"); !strings.Contains(out, "AGE▲") {
			t.Errorf("view %s created-asc: want AGE▲:\n%s", view, out)
		}
		row := findRow(t, renderWith(view, "created"), "srt")
		if !strings.Contains(row, "2h") {
			t.Errorf("view %s created: AGE should count from start (2h): %q", view, row)
		}
		row = findRow(t, renderWith(view, "updated"), "srt")
		if strings.Contains(row, "2h") {
			t.Errorf("view %s updated: AGE should count from update, not start: %q", view, row)
		}
		out := renderWith(view, "status")
		if view == "2" {
			if !strings.Contains(out, "S▲") {
				t.Errorf("view %s status: want S▲:\n%s", view, out)
			}
		} else if !strings.Contains(out, "STATUS▲") {
			t.Errorf("view %s status: want STATUS▲:\n%s", view, out)
		}
		if strings.Contains(out, "DIR▲") || strings.Contains(out, "AGE▲") || strings.Contains(out, "AGE▼") {
			t.Errorf("view %s status: only status column should carry arrow:\n%s", view, out)
		}
	}
}

func TestFormatAgents(t *testing.T) {
	if got := formatAgents(0); got != "" {
		t.Errorf("formatAgents(0) = %q, want empty", got)
	}
	if got := formatAgents(-1); got != "" {
		t.Errorf("formatAgents(-1) = %q, want empty", got)
	}
	if got := formatAgents(3); got != "3" {
		t.Errorf("formatAgents(3) = %q, want 3", got)
	}
}

func TestRenderAgentsColumnAndHeaderTotal(t *testing.T) {
	local := []Session{
		{PID: 100, SessionID: "aaaa", CWD: "/w1", Status: "busy", StartedAt: 1, AgentsRunning: 3},
		{PID: 200, SessionID: "bbbb", CWD: "/w2", Status: "idle", StartedAt: 2},
	}
	var buf bytes.Buffer
	RenderAll(&buf, "1", testLocalHost(local...), nil, "", nil, 0, 0, "dir")
	out := buf.String()

	if !strings.Contains(out, "AGENTS") {
		t.Errorf("full view missing AGENTS column header:\n%s", out)
	}
	// 2 sessions + 3 running subagents = 5 concurrent agent loops; one main
	// loop occupied (busy). The busy count is colorized, so match it apart
	// from the plain-text prefix.
	if !strings.Contains(out, "5 agents, 2 sessions,") || !strings.Contains(out, "1 busy") {
		t.Errorf("header missing grand total:\n%s", out)
	}

	// Intermediate view carries the column too.
	buf.Reset()
	RenderAll(&buf, "3", testLocalHost(local...), nil, "", nil, 0, 0, "dir")
	if !strings.Contains(buf.String(), "AGENTS") {
		t.Errorf("intermediate view missing AGENTS column header")
	}

	// Minimal view: no column, but header total still present.
	buf.Reset()
	RenderAll(&buf, "2", testLocalHost(local...), nil, "", nil, 0, 0, "dir")
	out = buf.String()
	if strings.Contains(out, "AGENTS") {
		t.Errorf("minimal view must not have AGENTS column:\n%s", out)
	}
	if !strings.Contains(out, "5 agents, 2 sessions,") {
		t.Errorf("minimal header missing grand total:\n%s", out)
	}
}

func TestRenderHeaderTotalNoSubagents(t *testing.T) {
	local := []Session{
		{PID: 100, SessionID: "aaaa", CWD: "/w1", Status: "idle", StartedAt: 1},
	}
	var buf bytes.Buffer
	RenderAll(&buf, "1", testLocalHost(local...), nil, "", nil, 0, 0, "dir")
	out := buf.String()
	if !strings.Contains(out, "1 agent, 1 session,") {
		t.Errorf("singular zero-subagent form missing:\n%s", out)
	}
	if !strings.Contains(out, "0 busy") {
		t.Errorf("idle-only header missing busy count:\n%s", out)
	}
	if strings.Contains(out, "1 agents") {
		t.Errorf("singular count must not pluralize:\n%s", out)
	}
}

func TestCtxCell(t *testing.T) {
	cases := []struct {
		name   string
		ctxStr string
		tokens int
		plain  bool
		want   string
	}{
		{"low usage uncolored", "50k", 50_000, false, "  50k"},
		{"warn at 70%", "210k", 210_000, false, colorize("33", " 210k")},
		{"hot at 90%", "280k", 280_000, false, colorize("1;31", " 280k")},
		{"ghost stays plain", "280k", 280_000, true, " 280k"},
		{"empty tokens plain", "-", 0, false, "    -"},
	}
	for _, c := range cases {
		if got := ctxCell(c.ctxStr, c.tokens, c.plain); got != c.want {
			t.Errorf("%s: ctxCell(%q, %d, %v) = %q, want %q", c.name, c.ctxStr, c.tokens, c.plain, got, c.want)
		}
	}
}

func TestEmptyRemoteHostSelectionHighlight(t *testing.T) {
	// A populated local session keeps the local section from rendering its own
	// "(no sessions)" row, isolating this test to the remote empty-host row.
	local := []Session{{PID: 1, CWD: "/local"}}
	remotes := []RemoteResult{{Name: "beluga"}}
	selected := emptyHostSelectionID("beluga")

	for _, mode := range []string{"1", "3", "2"} {
		t.Run(mode, func(t *testing.T) {
			var b strings.Builder
			RenderAll(&b, mode, testLocalHost(local...), remotes, selected, nil, 0, 0, "dir")
			row := findRow(t, b.String(), "(no sessions)")
			assertWholeRowSelected(t, row, "  ")
			// The empty remote host must not inflate counts: 1 local session
			// (1 agent), 0 from beluga.
			if !strings.Contains(b.String(), "1 agent, 1 session,") {
				t.Fatalf("mode %s empty host changed header counts:\n%s", mode, b.String())
			}
		})
	}
}

func TestEmptyLocalHostSelectionHighlight(t *testing.T) {
	selected := emptyHostSelectionID("")
	for _, mode := range []string{"1", "3", "2"} {
		t.Run(mode, func(t *testing.T) {
			var b strings.Builder
			RenderAll(&b, mode, testLocalHost(), nil, selected, nil, 0, 0, "dir")
			row := findRow(t, b.String(), "(no sessions)")
			assertWholeRowSelected(t, row, "  ")
			if !strings.Contains(b.String(), "0 agents, 0 sessions,") {
				t.Fatalf("mode %s empty local changed header counts:\n%s", mode, b.String())
			}
		})
	}
}

func TestEmptyLocalHostUnselectedMarker(t *testing.T) {
	var b strings.Builder
	RenderAll(&b, "1", testLocalHost(), nil, "", nil, 0, 0, "dir")
	row := findRow(t, b.String(), "(no sessions)")
	if !strings.HasPrefix(row, "  ") || strings.HasPrefix(row, "▶ ") {
		t.Fatalf("unselected empty-local row has wrong marker: %q", row)
	}
}

func TestEmptyRemoteHostUnselectedMarker(t *testing.T) {
	// Populated local keeps the empty-local row out of the way so findRow lands
	// on beluga's row — the one this test is about.
	local := []Session{{PID: 1, CWD: "/local"}}
	var b strings.Builder
	RenderAll(&b, "1", testLocalHost(local...), []RemoteResult{{Name: "beluga"}}, "", nil, 0, 0, "dir")
	row := findRow(t, b.String(), "(no sessions)")
	if !strings.HasPrefix(row, "  ") || strings.HasPrefix(row, "▶ ") {
		t.Fatalf("unselected empty-host row has wrong marker: %q", row)
	}
}

func TestEmptyLocalAndRemoteCoexist(t *testing.T) {
	// Empty local + empty remote both render "(no sessions)". Selecting the
	// local row marks only it, and two empty hosts must not inflate the counts.
	var b strings.Builder
	RenderAll(&b, "1", testLocalHost(), []RemoteResult{{Name: "beluga"}}, emptyHostSelectionID(""), nil, 0, 0, "dir")
	out := b.String()

	var rows []string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "(no sessions)") {
			rows = append(rows, line)
		}
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 (no sessions) rows (local + beluga), got %d:\n%s", len(rows), out)
	}
	// Local is section 0, so its row renders first and is the selected one.
	assertWholeRowSelected(t, rows[0], "  ")
	if strings.HasPrefix(rows[1], ansiInvert) {
		t.Fatalf("unselected empty-remote row wrongly highlighted: %q", rows[1])
	}
	if !strings.Contains(out, "0 agents, 0 sessions,") {
		t.Fatalf("two empty hosts inflated header counts:\n%s", out)
	}
}

func TestFormatHostPercent(t *testing.T) {
	cases := []struct {
		name string
		in   *float64
		want string
	}{
		{"unavailable", nil, "--"},
		{"zero", floatPtr(0), "0%"},
		{"round down", floatPtr(42.4), "42%"},
		{"round half up", floatPtr(42.5), "43%"},
		{"hundred", floatPtr(100), "100%"},
		{"clamp above hundred", floatPtr(250), "100%"},
		{"clamp below zero", floatPtr(-0.2), "0%"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatHostPercent(tc.in); got != tc.want {
				t.Fatalf("formatHostPercent() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFormatHostLoad(t *testing.T) {
	const unavailable = "-- -- --"
	cases := []struct {
		name string
		in   *LoadAverage
		want string
	}{
		{"nil object", nil, unavailable},
		{"nil one minute", &LoadAverage{OneMinute: nil, FiveMinutes: floatPtr(1), FifteenMinutes: floatPtr(1)}, unavailable},
		{"nil five minute", &LoadAverage{OneMinute: floatPtr(1), FiveMinutes: nil, FifteenMinutes: floatPtr(1)}, unavailable},
		{"nil fifteen minute", &LoadAverage{OneMinute: floatPtr(1), FiveMinutes: floatPtr(1), FifteenMinutes: nil}, unavailable},
		{"normal", hostLoadAverage(1.24, 0.96, 0.72), "1.24 0.96 0.72"},
		{"all zero", hostLoadAverage(0, 0, 0), "0.00 0.00 0.00"},
		{"negative zero normalized", &LoadAverage{OneMinute: floatPtr(math.Copysign(0, -1)), FiveMinutes: floatPtr(0), FifteenMinutes: floatPtr(0)}, "0.00 0.00 0.00"},
		{"above hundred unclamped", hostLoadAverage(128.5, 100, 250.75), "128.50 100.00 250.75"},
		{"negative one minute", &LoadAverage{OneMinute: floatPtr(-0.01), FiveMinutes: floatPtr(1), FifteenMinutes: floatPtr(1)}, unavailable},
		{"negative five minute", &LoadAverage{OneMinute: floatPtr(1), FiveMinutes: floatPtr(-1), FifteenMinutes: floatPtr(1)}, unavailable},
		{"negative fifteen minute", &LoadAverage{OneMinute: floatPtr(1), FiveMinutes: floatPtr(1), FifteenMinutes: floatPtr(-1)}, unavailable},
		{"nan", &LoadAverage{OneMinute: floatPtr(math.NaN()), FiveMinutes: floatPtr(1), FifteenMinutes: floatPtr(1)}, unavailable},
		{"positive inf", &LoadAverage{OneMinute: floatPtr(1), FiveMinutes: floatPtr(math.Inf(1)), FifteenMinutes: floatPtr(1)}, unavailable},
		{"negative inf", &LoadAverage{OneMinute: floatPtr(1), FiveMinutes: floatPtr(1), FifteenMinutes: floatPtr(math.Inf(-1))}, unavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatHostLoad(tc.in); got != tc.want {
				t.Fatalf("formatHostLoad() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestHostUsageHeadingsAllViews(t *testing.T) {
	local := LocalHost{
		Name:      "workstation",
		Sessions:  []Session{{PID: 1, CWD: "/local-dir"}},
		HostUsage: HostUsage{CPUPercent: floatPtr(12.5), MemoryPercent: floatPtr(50), Load: hostLoadAverage(1.24, 0.96, 0.72)},
	}
	remotes := []RemoteResult{{
		Name:      "beluga",
		Sessions:  []Session{{PID: 2, Host: "beluga", CWD: "/remote-dir"}},
		HostUsage: HostUsage{CPUPercent: floatPtr(0)},
	}}
	for _, mode := range []string{"1", "2", "3"} {
		t.Run(mode, func(t *testing.T) {
			var b strings.Builder
			RenderAll(&b, mode, local, remotes, "", nil, 0, 0, "dir")
			out := b.String()
			localHeading := findRow(t, out, "workstation")
			if !strings.Contains(localHeading, "CPU 13%  MEM 50%  LOAD 1.24 0.96 0.72") {
				t.Fatalf("local heading = %q", localHeading)
			}
			remoteHeading := findRow(t, out, "beluga")
			if !strings.Contains(remoteHeading, "CPU 0%  MEM --  LOAD -- -- --") {
				t.Fatalf("remote heading = %q", remoteHeading)
			}
			if strings.Index(out, "workstation") > strings.Index(out, "local-dir") {
				t.Fatal("local heading rendered after local row")
			}
			if strings.Index(out, "beluga") > strings.Index(out, "remote-dir") {
				t.Fatal("remote heading rendered after remote row")
			}
		})
	}
}

func TestHostHeadingPrecedesRemoteStates(t *testing.T) {
	// Keep local populated so the only "(no sessions)" body belongs to the
	// empty remote section under test.
	local := LocalHost{Name: "local", Sessions: []Session{{PID: 1, CWD: "/local-session"}}}
	remotes := []RemoteResult{
		{Name: "loading", Loading: true},
		{Name: "down", Error: "timeout"},
		{Name: "empty"},
	}
	var b strings.Builder
	RenderAll(&b, "1", local, remotes, "", nil, 0, 0, "dir")
	out := b.String()
	for _, tc := range []struct{ host, body string }{
		{"loading", "(loading...)"},
		{"down", "[unreachable: timeout]"},
		{"empty", "(no sessions)"},
	} {
		if strings.Index(out, tc.host) < 0 || strings.Index(out, tc.body) < 0 || strings.Index(out, tc.host) > strings.Index(out, tc.body) {
			t.Fatalf("%s heading/body order wrong:\n%s", tc.host, out)
		}
	}
}

// TestRenderAllMatchesBuildTableFrame locks the compatibility invariant: the
// text RenderAll writes must be byte-identical to the joined frame lines, and
// its overflow return must match the frame's, across all three views and a mix
// of local + empty-remote rows.
func TestRenderAllMatchesBuildTableFrame(t *testing.T) {
	now := time.Now().UnixMilli()
	local := []Session{
		{PID: 1, Name: "one", CWD: "/tmp/one", Status: "busy", UpdatedAt: now},
		{PID: 2, Name: "two", CWD: "/tmp/two", Status: "idle", UpdatedAt: now},
	}
	remotes := []RemoteResult{{Name: "dev"}}
	for _, mode := range []string{"1", "2", "3"} {
		var b strings.Builder
		overflow := RenderAll(&b, mode, testLocalHost(local...), remotes, "1", nil, 120, 0, "dir")
		frame := BuildTableFrame(mode, testLocalHost(local...), remotes, "1", nil, 120, 0, "dir")
		if got := strings.Join(frame.lines, "\n"); got != b.String() {
			t.Errorf("mode %s: RenderAll text diverged from frame:\nRenderAll: %q\nframe:     %q", mode, b.String(), got)
		}
		if frame.overflowing != overflow {
			t.Errorf("mode %s: overflow mismatch RenderAll=%v frame=%v", mode, overflow, frame.overflowing)
		}
	}
}
