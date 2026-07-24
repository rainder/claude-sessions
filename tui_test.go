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
	for _, want := range []string{"-/+ disable/enable", "1-9 only", "h1-9 hide", "⇧1-9 group", "/ search"} {
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
		"/            filter rows by text (type to narrow · Enter commits · Esc clears)",
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

func TestTextFilterTransition(t *testing.T) {
	editing := func(buf, committed string) textFilterState {
		return textFilterState{editing: true, buffer: buf, committed: committed}
	}
	committed := func(q string) textFilterState { return textFilterState{committed: q} }

	cases := []struct {
		name         string
		cur          textFilterState
		key          string
		wantNext     textFilterState
		wantConsumed bool
	}{
		// Not editing: only '/' is consumed, preloading the committed query.
		{"idle '/' enters preloaded with committed", committed("web"), "/", editing("web", "web"), true},
		{"idle '/' with empty committed", textFilterState{}, "/", editing("", ""), true},
		{"idle non-'/' key falls through", committed("web"), "k", committed("web"), false},
		{"idle digit falls through", textFilterState{}, "1", textFilterState{}, false},

		// Editing: text input appends (including multi-byte as-is).
		{"append printable", editing("ap", "x"), "i", editing("api", "x"), true},
		{"append space", editing("a", ""), " ", editing("a ", ""), true},
		{"append multi-byte rune as-is", editing("", ""), "é", editing("é", ""), true},

		// Backspace / Ctrl+U edit the buffer.
		{"DEL backspace deletes last rune", editing("api", "x"), "\x7f", editing("ap", "x"), true},
		{"BS backspace deletes last rune", editing("api", "x"), "\x08", editing("ap", "x"), true},
		{"backspace on empty buffer is a no-op", editing("", "x"), "\x7f", editing("", "x"), true},
		{"backspace deletes a whole multi-byte rune", editing("aé", ""), "\x7f", editing("a", ""), true},
		{"Ctrl+U clears the buffer", editing("api", "x"), "\x15", editing("", "x"), true},

		// Enter commits (empty buffer clears), Esc cancels + clears.
		{"Enter commits the buffer", editing("api", "old"), KeyEnter, committed("api"), true},
		{"Enter with empty buffer clears the filter", editing("", "old"), KeyEnter, committed(""), true},
		{"Esc cancels and clears the query", editing("api", "old"), KeyEsc, textFilterState{}, true},

		// Hotkeys are suspended while editing: printable ones (digits, letters like
		// 'k'/'q') become text and append; only control/navigation keys are
		// swallowed unchanged.
		{"digit appends as text while editing", editing("api", ""), "1", editing("api1", ""), true},
		{"letter hotkey appends as text while editing", editing("api", ""), "k", editing("apik", ""), true},
		{"navigation key swallowed while editing", editing("api", ""), KeyUp, editing("api", ""), true},

		// Hard-quit passes through mid-edit so Ctrl+C / Ctrl+D still exit the TUI.
		{"Ctrl+C falls through while editing", editing("api", "x"), "\x03", editing("api", "x"), false},
		{"Ctrl+D falls through while editing", editing("api", "x"), "\x04", editing("api", "x"), false},
	}
	for _, c := range cases {
		gotNext, gotConsumed := textFilterTransition(c.cur, c.key)
		if gotNext != c.wantNext || gotConsumed != c.wantConsumed {
			t.Errorf("%s: got (%+v, consumed=%v), want (%+v, consumed=%v)",
				c.name, gotNext, gotConsumed, c.wantNext, c.wantConsumed)
		}
	}
}

func TestTextFilterEffectiveQueryAndPrompt(t *testing.T) {
	// Committed query drives the filter when not editing.
	if got := (textFilterState{committed: "web"}).effectiveQuery(); got != "web" {
		t.Errorf("committed effectiveQuery = %q, want %q", got, "web")
	}
	// While editing, the live buffer drives it (rows narrow as you type).
	if got := (textFilterState{editing: true, buffer: "ap", committed: "web"}).effectiveQuery(); got != "ap" {
		t.Errorf("editing effectiveQuery = %q, want live buffer %q", got, "ap")
	}
	if got := textFilterPrompt("api", 80); got != "/api▌" {
		t.Errorf("textFilterPrompt = %q, want %q", got, "/api▌")
	}
	// A buffer wider than the terminal keeps its tail so the cursor stays visible.
	if got := textFilterPrompt("abcdefgh", 6); got != "/efgh▌" {
		t.Errorf("textFilterPrompt narrow = %q, want %q", got, "/efgh▌")
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
