package main

import (
	"strings"
	"testing"
)

func TestParseNewArgs(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    newArgs
		wantErr bool
	}{
		{
			name: "dir only",
			args: []string{"--dir", "/tmp/proj"},
			want: newArgs{dir: "/tmp/proj"},
		},
		{
			name: "cwd is a synonym for dir",
			args: []string{"--cwd", "/tmp/proj"},
			want: newArgs{dir: "/tmp/proj"},
		},
		{
			name: "full flag set plus prompt",
			args: []string{"--server", "agent-workstation", "--dir", "~/Developer/trecs-brain", "--command", "fable", "--name", "brain", "some", "initial", "prompt"},
			want: newArgs{dir: "~/Developer/trecs-brain", name: "brain", command: "fable", server: "agent-workstation", prompt: "some initial prompt"},
		},
		{
			name: "prompt before flags still joins",
			args: []string{"hello", "--dir", "/tmp/proj", "world"},
			want: newArgs{dir: "/tmp/proj", prompt: "hello world"},
		},
		{
			name:    "missing value for flag",
			args:    []string{"--dir"},
			wantErr: true,
		},
		{
			name:    "missing value for server",
			args:    []string{"--dir", "/tmp", "--server"},
			wantErr: true,
		},
		{
			name:    "unknown flag",
			args:    []string{"--dir", "/tmp", "--bogus"},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseNewArgs(tt.args)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseNewArgs(%v) error = %v, wantErr %v", tt.args, err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if got != tt.want {
				t.Errorf("parseNewArgs(%v) = %+v, want %+v", tt.args, got, tt.want)
			}
		})
	}
}

func TestParseKillFlags(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		want    killFlags
		wantErr bool
	}{
		{name: "no flags", args: nil},
		{name: "yes", args: []string{"-y"}, want: killFlags{assumeYes: true}},
		{name: "remove worktree", args: []string{"--remove-worktree"}, want: killFlags{removeWorktree: true}},
		{
			name: "both",
			args: []string{"-y", "--remove-worktree"},
			want: killFlags{assumeYes: true, removeWorktree: true},
		},
		{
			name: "both reversed",
			args: []string{"--remove-worktree", "-y"},
			want: killFlags{assumeYes: true, removeWorktree: true},
		},
		{name: "unknown flag", args: []string{"--force"}, wantErr: true},
		{name: "typo is not silently ignored", args: []string{"-Y"}, wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseKillFlags(c.args)
			if (err != nil) != c.wantErr {
				t.Fatalf("parseKillFlags(%v) error = %v, wantErr %v", c.args, err, c.wantErr)
			}
			if err == nil && got != c.want {
				t.Fatalf("parseKillFlags(%v) = %+v, want %+v", c.args, got, c.want)
			}
		})
	}
}

func TestWorktreeRemovalQuestionNamesWorktree(t *testing.T) {
	got := worktreeRemovalQuestion("/repo/.claude/worktrees/DR-860")
	if !strings.Contains(got, "DR-860") {
		t.Fatalf("question = %q, want it to name the worktree", got)
	}
}

func TestParseAccountArgs(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantName   string
		wantServer string
		wantErr    bool
	}{
		{name: "name only", args: []string{"avisoma"}, wantName: "avisoma"},
		{name: "name and server", args: []string{"avisoma", "--server", "box"}, wantName: "avisoma", wantServer: "box"},
		{name: "server before name", args: []string{"--server", "box", "avisoma"}, wantName: "avisoma", wantServer: "box"},
		{name: "server only", args: []string{"--server", "box"}, wantServer: "box"},
		{name: "no args", args: nil},
		{name: "server without a value", args: []string{"--server"}, wantErr: true},
		{name: "unknown flag", args: []string{"avisoma", "--force"}, wantErr: true},
		{name: "second positional", args: []string{"avisoma", "trecs"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			name, server, err := parseAccountArgs(tt.args)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("err = nil, want an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if name != tt.wantName || server != tt.wantServer {
				t.Fatalf("= (%q, %q), want (%q, %q)", name, server, tt.wantName, tt.wantServer)
			}
		})
	}
}

func TestAccountSwitchedLine(t *testing.T) {
	if got := accountSwitchedLine("trecs", "andy@trecs.aero"); !strings.Contains(got, "trecs") || !strings.Contains(got, "andy@trecs.aero") {
		t.Fatalf("line = %q, want the name and the new email", got)
	}
}

func TestCmdSummary(t *testing.T) {
	t.Run("no args prints current backend without changing it", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		if code := cmdSummary(nil); code != 0 {
			t.Fatalf("cmdSummary(nil) = %d, want 0", code)
		}
		if got := LoadSummaryBackend(); got != "claude" {
			t.Errorf("LoadSummaryBackend() after a bare `summary` call = %q, want unchanged %q", got, "claude")
		}
	})

	t.Run("valid backend persists", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		if code := cmdSummary([]string{"codex"}); code != 0 {
			t.Fatalf("cmdSummary([codex]) = %d, want 0", code)
		}
		if got := LoadSummaryBackend(); got != "codex" {
			t.Errorf("LoadSummaryBackend() after `summary codex` = %q, want %q", got, "codex")
		}
	})

	t.Run("unknown backend is rejected and leaves config unchanged", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		if code := cmdSummary([]string{"bogus"}); code != 2 {
			t.Fatalf("cmdSummary([bogus]) = %d, want 2", code)
		}
		if got := LoadSummaryBackend(); got != "claude" {
			t.Errorf("LoadSummaryBackend() after a rejected `summary bogus` = %q, want unchanged %q", got, "claude")
		}
	})

	t.Run("too many args is rejected", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		if code := cmdSummary([]string{"claude", "extra"}); code != 2 {
			t.Fatalf("cmdSummary([claude extra]) = %d, want 2", code)
		}
	})
}
