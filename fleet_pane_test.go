package main

import (
	"os"
	"testing"
)

func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/fleet/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestPaneInputTextIgnoresDimPlaceholder(t *testing.T) {
	text, found := paneInputText(readFixture(t, "pane-empty-placeholder.ansi"))
	if !found || text != "" {
		t.Fatalf("got (%q, %v), want (\"\", true)", text, found)
	}
}

func TestPaneInputTextReportsTypedText(t *testing.T) {
	text, found := paneInputText(readFixture(t, "pane-typed.ansi"))
	if !found || text != "hello there" {
		t.Fatalf("got (%q, %v), want (\"hello there\", true)", text, found)
	}
}

func TestPaneInputTextEmptyBoxNoPlaceholder(t *testing.T) {
	rule := "\x1b[38;5;244m" + repeatRune('─', 40) + "\n"
	capture := "output\n" + rule + "\x1b[39m❯ \x1b[39m\n" + rule + "\x1b[39m  status line\n"
	text, found := paneInputText(capture)
	if !found || text != "" {
		t.Fatalf("got (%q, %v)", text, found)
	}
}

func TestPaneInputTextMultiLineInput(t *testing.T) {
	rule := repeatRune('─', 40) + "\n"
	capture := rule + "❯ first line\n  second line\n" + rule + "status\n"
	text, found := paneInputText(capture)
	if !found || text != "first line second line" {
		t.Fatalf("got (%q, %v)", text, found)
	}
}

func TestPaneInputTextNoBoxIsNotFound(t *testing.T) {
	// A permission dialog replaces the input box: one top rule, a menu with
	// the ❯ cursor on "1. Yes", no closing rule.
	capture := repeatRune('─', 40) + "\n Bash command\n Do you want to proceed?\n ❯ 1. Yes\n   2. No\n"
	if _, found := paneInputText(capture); found {
		t.Fatal("dialog without a closed input box must read as not found")
	}
}

func TestPaneInputTextBoxWithoutPromptGlyphIsNotFound(t *testing.T) {
	rule := repeatRune('─', 40) + "\n"
	capture := rule + " Select an option\n" + rule
	if _, found := paneInputText(capture); found {
		t.Fatal("a ruled region whose first line is not ❯ is not the input box")
	}
}

// TestPaneInputTextFixtures runs every captured or synthetic pane state. The
// non-synth ones are real `capture-pane -p -e` dumps of a live Claude Code
// pane; pane-synth-* copy their escape style for states that could not be
// produced on demand (a panel under the box, a notice above the prompt, a
// wrapped rule in a narrow pane, a permission dialog).
func TestPaneInputTextFixtures(t *testing.T) {
	cases := []struct {
		file  string
		text  string
		found bool
	}{
		{"pane-empty-placeholder.ansi", "", true},
		{"pane-typed.ansi", "hello there", true},
		{"pane-idle-empty.ansi", "", true},
		// The slash menu draws above the box with its own "❯" cursor row;
		// the box's prompt is the last ❯ line.
		{"pane-slash-menu.ansi", "/", true},
		{"pane-at-menu.ansi", "@", true},
		{"pane-shortcuts-below.ansi", "", true},
		{"pane-synth-task-panel-below.ansi", "", true},
		{"pane-synth-notice-above-prompt.ansi", "", true},
		{"pane-synth-queued-preview.ansi", "", true},
		{"pane-synth-clear-hint.ansi", "", true},
		{"pane-synth-narrow-double-rule.ansi", "", true},
		// A named session: Claude Code writes the name into the top rule
		// ("──── FLEET ─"), and the box sits under a busy turn.
		{"pane-named-session-busy.ansi", "", true},
	}
	for _, c := range cases {
		text, found := paneInputText(readFixture(t, c.file))
		if text != c.text || found != c.found {
			t.Errorf("%s: got (%q, %v), want (%q, %v)", c.file, text, found, c.text, c.found)
		}
	}
}

// A permission dialog drawn between two rules has a "❯ 1. Yes" line. It must
// never read as an empty box: either not found or non-empty, both refuse.
func TestPaneInputTextPermissionDialogRefuses(t *testing.T) {
	text, found := paneInputText(readFixture(t, "pane-synth-permission-dialog.ansi"))
	if found && text == "" {
		t.Fatal("a permission dialog read as an empty input box")
	}
}

func TestPaneInputTextPromptWithoutRuleBelowIsNotFound(t *testing.T) {
	rule := repeatRune('─', 40) + "\n"
	if _, found := paneInputText(rule + "❯ \n status\n"); found {
		t.Fatal("a prompt line with no rule below is not a closed box")
	}
	if _, found := paneInputText("❯ \n" + rule); found {
		t.Fatal("a prompt line with no rule above is not a closed box")
	}
}

func TestPaneInputTextNamedRules(t *testing.T) {
	rule := repeatRune('─', 40) + "\n"
	named := "\x1b[38;5;244m" + repeatRune('─', 60) + " FLEET ─\n"
	cases := []struct {
		name, capture, text string
	}{
		{"named top rule, typed text", "out\n" + named + "\x1b[39m❯ hello there\n" + rule + "status\n", "hello there"},
		{"named bottom rule", "out\n" + rule + "❯ \n" + named + "status\n", ""},
		{"both rules named", named + "❯ hi\n" + named, "hi"},
	}
	for _, c := range cases {
		text, found := paneInputText(c.capture)
		if !found || text != c.text {
			t.Errorf("%s: got (%q, %v), want (%q, true)", c.name, text, found, c.text)
		}
	}
}

func TestIsRuleLine(t *testing.T) {
	cases := []struct {
		line string
		want bool
	}{
		{repeatRune('─', 10), true},
		{repeatRune('─', 9), false},
		{"  " + repeatRune('─', 40) + "  ", true},
		{repeatRune('─', 60) + " FLEET ─", true},
		{"─ FLEET " + repeatRune('─', 30), true},
		{repeatRune('─', 5) + " my session " + repeatRune('─', 5), true},
		{repeatRune('─', 5) + " a " + repeatRune('─', 5) + " b " + repeatRune('─', 5), false},
		{"─── text", false},
		{"text " + repeatRune('─', 30), false},
		{repeatRune('─', 30) + " text", false},
		{repeatRune('─', 4) + " FLEET ─", false},
		{repeatRune('─', 10) + " " + repeatRune('x', 79) + " ─", false},
		{repeatRune('─', 10) + repeatRune('x', 80) + "─", true},
		{repeatRune('─', 10) + repeatRune('x', 81) + "─", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isRuleLine(c.line); got != c.want {
			t.Errorf("isRuleLine(%q) = %v, want %v", c.line, got, c.want)
		}
	}
}

func TestStripPaneEscapesKeepsDimText(t *testing.T) {
	got := stripPaneEscapes("\x1b[39m❯ \x1b[2mTry \"x\"\x1b[0m\x07end")
	if got != "❯ Try \"x\"end" {
		t.Fatalf("got %q", got)
	}
}

// C1 controls (U+0080..U+009F) go too: U+009B is a one-byte CSI that some
// terminals act on when the server runs in the foreground. A raw 0x9B byte
// that is not valid UTF-8 already decodes to U+FFFD.
func TestStripPaneEscapesDropsC1Controls(t *testing.T) {
	got := stripPaneEscapes("a\u009b31mb\u0080c\u009fd\u00a0e")
	if got != "a31mbcd\u00a0e" {
		t.Fatalf("got %q", got)
	}
}

func TestCapturePaneRefusesEmptyTarget(t *testing.T) {
	if _, err := capturePane(""); err == nil {
		t.Fatal("empty target must error: tmux -t \"\" hits the current session")
	}
}

func repeatRune(r rune, n int) string {
	out := make([]rune, n)
	for i := range out {
		out[i] = r
	}
	return string(out)
}
