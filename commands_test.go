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
