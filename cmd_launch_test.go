package main

import (
	"strings"
	"testing"
)

func TestValidateCmdBinary(t *testing.T) {
	for _, name := range []string{"claude", "grok", "claudex", "claudexx"} {
		if err := validateCmdBinary(name); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	for _, name := range []string{"bash", "/usr/bin/claude", "../grok", "claude/foo", ""} {
		if err := validateCmdBinary(name); err == nil {
			t.Fatalf("%q: want error", name)
		}
	}
}

func TestLaunchFromCmdQuotesEachArg(t *testing.T) {
	got, err := launchFromCmd("grok", []string{"-m", "grok-4.6", "--reasoning-effort", "high"}, "DR-1234")
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"grok",
		shellQuote("-m"),
		shellQuote("grok-4.6"),
		shellQuote("--reasoning-effort"),
		shellQuote("high"),
		shellQuote("DR-1234"),
	}, " ")
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestLaunchFromCmdQuotesThePrompt(t *testing.T) {
	got, err := launchFromCmd("claude", []string{"--model=fable"}, "say $HOME")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, shellQuote("say $HOME")) {
		t.Fatalf("prompt not quoted: %q", got)
	}
}
