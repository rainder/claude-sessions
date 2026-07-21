package main

import (
	"reflect"
	"testing"
)

func TestParseConfigYAMLCommandsAndServers(t *testing.T) {
	cfg := parseConfigYAML(`
commands:
  - name: Claude
    command: claude
  - name: Fable
    command: "claude --model fable"
servers:
  - name: beluga
    host: 100.64.0.2
    port: 9000
    token: secret
`)

	wantCommands := []CommandPreset{
		{Name: "Claude", Command: "claude"},
		{Name: "Fable", Command: "claude --model fable"},
	}
	if !reflect.DeepEqual(cfg.Commands, wantCommands) {
		t.Fatalf("commands = %#v, want %#v", cfg.Commands, wantCommands)
	}
	if len(cfg.Servers) != 1 || cfg.Servers[0].Name != "beluga" || cfg.Servers[0].Port != 9000 {
		t.Fatalf("servers = %#v", cfg.Servers)
	}
}

func TestParseConfigYAMLCommandValidation(t *testing.T) {
	cfg := parseConfigYAML(`
commands:
  - name: "  Fable  "
    command: "  claude --model fable  "
  - name: Fable
    command: ignored-duplicate
  - name: ""
    command: blank-name
  - name: BlankCommand
    command: ""
`)
	want := []CommandPreset{{Name: "Fable", Command: "claude --model fable"}}
	if !reflect.DeepEqual(cfg.Commands, want) {
		t.Fatalf("commands = %#v, want %#v", cfg.Commands, want)
	}
}

func TestParseConfigYAMLCommandFallback(t *testing.T) {
	for _, input := range []string{"", "servers:\n", "commands:\n  - name: broken\n"} {
		cfg := parseConfigYAML(input)
		want := []CommandPreset{{Name: "Claude", Command: "claude"}}
		if !reflect.DeepEqual(cfg.Commands, want) {
			t.Fatalf("parseConfigYAML(%q).Commands = %#v, want %#v", input, cfg.Commands, want)
		}
	}
}

func TestFindCommandPreset(t *testing.T) {
	presets := []CommandPreset{{Name: "Claude", Command: "claude"}, {Name: "Fable", Command: "claude --model fable"}}
	got, ok := findCommandPreset(presets, "Fable")
	if !ok || got.Command != "claude --model fable" {
		t.Fatalf("findCommandPreset = %#v, %v", got, ok)
	}
	if _, ok := findCommandPreset(presets, "missing"); ok {
		t.Fatal("missing preset unexpectedly resolved")
	}
}
