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
