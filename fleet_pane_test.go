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

func TestStripPaneEscapesKeepsDimText(t *testing.T) {
	got := stripPaneEscapes("\x1b[39m❯ \x1b[2mTry \"x\"\x1b[0m\x07end")
	if got != "❯ Try \"x\"end" {
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
