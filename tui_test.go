package main

import (
	"strings"
	"testing"
)

func TestSessionScreenOpenKeys(t *testing.T) {
	for _, key := range []string{KeyRight, "p", "P"} {
		if got := sessionKeyCommand(key); got != commandOpenInspector {
			t.Errorf("key %q command = %v", key, got)
		}
	}
}

func TestInspectorBackAndQuitKeys(t *testing.T) {
	for _, key := range []string{KeyEsc, "q", "Q", "p", "P"} {
		if got := inspectorKeyCommand(key); got != commandBack {
			t.Errorf("key %q command = %v", key, got)
		}
	}
	if got := inspectorKeyCommand("\x03"); got != commandQuit {
		t.Fatalf("Ctrl-C command = %v", got)
	}
}

func TestCycleSortMode(t *testing.T) {
	cases := []struct {
		mode  string
		delta int
		want  string
	}{
		{"dir", 1, "status"},
		{"status", 1, "created"},
		{"created", -1, "status"},
		{"updated-asc", 1, "dir"},
		{"dir", -1, "updated-asc"},
		{"created-asc", -1, "created"},
		{"bogus", 1, "status"},
		{"bogus", -1, "updated-asc"},
	}
	for _, c := range cases {
		if got := cycleSortMode(c.mode, c.delta); got != c.want {
			t.Errorf("cycleSortMode(%q, %d) = %q, want %q", c.mode, c.delta, got, c.want)
		}
	}
}

func TestSortDescStatus(t *testing.T) {
	if got := sortDesc("status"); got != "status (waiting → idle → busy)" {
		t.Fatalf("sortDesc(status) = %q", got)
	}
}

func TestSessionDisableFooterAndHelp(t *testing.T) {
	footer := sessionFooter()
	for _, want := range []string{"-/+ disable/enable", "1-9 only", "h1-9 hide", "⇧1-9 group"} {
		if !strings.Contains(footer, want) {
			t.Fatalf("footer %q missing %q", footer, want)
		}
	}
	if bottom := sessionBottomRow("sort: status", false); bottom != footer {
		t.Fatalf("normal bottom row = %q, want footer %q", bottom, footer)
	}
	toast := sessionBottomRow("sort: status", true)
	if !strings.Contains(toast, "sort: status") ||
		strings.Contains(toast, "-/+ disable/enable") {
		t.Fatalf("toast bottom row = %q", toast)
	}

	help := renderHelp("dir")
	for _, want := range []string{
		"- / +        disable / enable session",
		"1..9         show only group (same digit or 0 shows all)",
		"h then 1..9  hide group(s) (repeat to add/remove · last one shows all)",
		"claude-sessions preview PID",
		"claude-sessions tmux-info PID",
		"claude-sessions attach PID",
		"press any key to return",
	} {
		if !strings.Contains(help, want) {
			t.Fatalf("help missing %q:\n%s", want, help)
		}
	}
	if strings.Contains(help, "\x1b[H") ||
		strings.Contains(help, "\x1b[J") ||
		strings.Contains(help, "\x1b[2J") {
		t.Fatalf("help contains terminal positioning or clear: %q", help)
	}
}

func TestGroupFilterTransition(t *testing.T) {
	only := func(n int) groupFilter { return groupFilter{mode: filterOnly, mask: 1 << uint(n)} }
	hide := func(ns ...int) groupFilter {
		var m uint16
		for _, n := range ns {
			m |= 1 << uint(n)
		}
		return groupFilter{mode: filterHide, mask: m}
	}
	none := groupFilter{}

	cases := []struct {
		name         string
		cur          groupFilter
		armed        bool
		key          string
		wantNext     groupFilter
		wantArmed    bool
		wantConsumed bool
	}{
		// only-mode digits (unarmed).
		{"none+3 -> only3", none, false, "3", only(3), false, true},
		{"only3+3 clears", only(3), false, "3", none, false, true},
		{"only3+5 switches", only(3), false, "5", only(5), false, true},
		{"hide+digit switches to only", hide(2, 3), false, "3", only(3), false, true},
		{"only3+0 clears", only(3), false, "0", none, false, true},
		{"none+0 clears (consumed, no fall-through)", none, false, "0", none, false, true},

		// arming.
		{"none+h arms", none, false, "h", none, true, true},
		{"H arms too", none, false, "H", none, true, true},

		// armed hide toggles.
		{"armed none+3 -> hide3", none, true, "3", hide(3), false, true},
		{"armed hide3+3 clears (last bit)", hide(3), true, "3", none, false, true},
		{"armed hide{2,3}+5 adds", hide(2, 3), true, "5", hide(2, 3, 5), false, true},
		{"armed hide{2,3,5}+3 removes one", hide(2, 3, 5), true, "3", hide(2, 5), false, true},
		{"armed only3+3 starts fresh hide mask", only(3), true, "3", hide(3), false, true},

		// armed non-1..9 keys: arm cancelled, reinterpreted unarmed.
		{"armed+0 clears and disarms", only(3), true, "0", none, false, true},
		{"armed+h re-arms", none, true, "h", none, true, true},
		{"armed+k cancels arm, falls through", only(3), true, "k", only(3), false, false},

		// non-filter keys never consume.
		{"unarmed+k falls through", only(3), false, "k", only(3), false, false},
		{"unarmed+q falls through", none, false, "q", none, false, false},
		{"shift-digit not a filter key", none, false, "!", none, false, false},
	}
	for _, c := range cases {
		gotNext, gotArmed, gotConsumed := groupFilterTransition(c.cur, c.armed, c.key)
		if gotNext != c.wantNext || gotArmed != c.wantArmed || gotConsumed != c.wantConsumed {
			t.Errorf("%s: got (%+v, armed=%v, consumed=%v), want (%+v, armed=%v, consumed=%v)",
				c.name, gotNext, gotArmed, gotConsumed, c.wantNext, c.wantArmed, c.wantConsumed)
		}
	}
}

func TestRenderHelpIsPureContent(t *testing.T) {
	out := renderHelp("status")
	for _, want := range []string{"claude-sessions", "NAVIGATION", "current sort: status", "press any key to return"} {
		if !strings.Contains(out, want) {
			t.Fatalf("help missing %q: %q", want, out)
		}
	}
	if strings.Contains(out, "\x1b[H") || strings.Contains(out, "\x1b[J") || strings.Contains(out, "\x1b[2J") {
		t.Fatalf("help contains terminal positioning or clear: %q", out)
	}
}
