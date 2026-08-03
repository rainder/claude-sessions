package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"reflect"
	"testing"
)

func TestCuFetchFuncSeamIsSwappable(t *testing.T) {
	prev := cuFetchFunc
	t.Cleanup(func() { cuFetchFunc = prev })
	called := false
	cuFetchFunc = func(ctx context.Context, ticketID string) ([]byte, error) {
		called = true
		if ticketID != "DR-1" {
			t.Errorf("ticketID = %q, want DR-1", ticketID)
		}
		return []byte("ticket body"), nil
	}
	out, err := cuFetchFunc(context.Background(), "DR-1")
	if err != nil || string(out) != "ticket body" || !called {
		t.Errorf("got (%q, %v), called=%v", out, err, called)
	}
}

func TestClaudeSummarizeFuncSeamIsSwappable(t *testing.T) {
	prev := claudeSummarizeFunc
	t.Cleanup(func() { claudeSummarizeFunc = prev })
	claudeSummarizeFunc = func(ctx context.Context, instruction string, input []byte) ([]byte, error) {
		if instruction != "instr" || string(input) != "data" {
			return nil, errors.New("wrong args")
		}
		return []byte("summary"), nil
	}
	out, err := claudeSummarizeFunc(context.Background(), "instr", []byte("data"))
	if err != nil || string(out) != "summary" {
		t.Errorf("got (%q, %v)", out, err)
	}
}

// TestCuFetchCmdArgs verifies the real argv `cu fetch --with-comments <id>`
// constructed by cuFetchCmd, and its WaitDelay — coverage the swappable-seam
// tests above don't provide, since they never call the real runner.
func TestCuFetchCmdArgs(t *testing.T) {
	tests := []struct {
		name     string
		ticketID string
		wantArgs []string
	}{
		{
			name:     "simple ticket id",
			ticketID: "DR-1",
			wantArgs: []string{"cu", "fetch", "--with-comments", "DR-1"},
		},
		{
			name:     "different ticket id",
			ticketID: "DR-2500",
			wantArgs: []string{"cu", "fetch", "--with-comments", "DR-2500"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := cuFetchCmd(context.Background(), tt.ticketID)
			if !reflect.DeepEqual(cmd.Args, tt.wantArgs) {
				t.Errorf("Args = %v, want %v", cmd.Args, tt.wantArgs)
			}
			if cmd.WaitDelay != subprocessWaitDelay {
				t.Errorf("WaitDelay = %v, want %v", cmd.WaitDelay, subprocessWaitDelay)
			}
		})
	}
}

// TestClaudeSummarizeCmdArgs verifies the real argv
// `claude --model sonnet --effort low --tools "" --system-prompt <prompt> -p
// <instruction>` constructed by claudeSummarizeCmd, its Stdin wiring, its Dir
// (neutral, not --bare — see claudeSummarizeCmd's doc comment for why --bare
// broke OAuth auth, and claudeSystemPrompt's doc comment for why --tools ""
// and --system-prompt are both required to stop ambient cwd/git context from
// leaking into a summary), and its WaitDelay.
func TestClaudeSummarizeCmdArgs(t *testing.T) {
	tests := []struct {
		name        string
		instruction string
		input       []byte
		wantArgs    []string
	}{
		{
			name:        "summarize instruction",
			instruction: "summarize this ticket",
			input:       []byte("ticket body"),
			wantArgs:    []string{"claude", "--model", "sonnet", "--effort", "low", "--tools", "", "--system-prompt", claudeSystemPrompt, "-p", "summarize this ticket"},
		},
		{
			name:        "empty input",
			instruction: "instr",
			input:       []byte(""),
			wantArgs:    []string{"claude", "--model", "sonnet", "--effort", "low", "--tools", "", "--system-prompt", claudeSystemPrompt, "-p", "instr"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := claudeSummarizeCmd(context.Background(), tt.instruction, tt.input)
			if !reflect.DeepEqual(cmd.Args, tt.wantArgs) {
				t.Errorf("Args = %v, want %v", cmd.Args, tt.wantArgs)
			}
			wantDir := os.TempDir()
			if cmd.Dir != wantDir {
				t.Errorf("Dir = %q, want %q (neutral dir, not the invoking project's cwd)", cmd.Dir, wantDir)
			}
			if cmd.WaitDelay != subprocessWaitDelay {
				t.Errorf("WaitDelay = %v, want %v", cmd.WaitDelay, subprocessWaitDelay)
			}
			gotStdin, err := io.ReadAll(cmd.Stdin)
			if err != nil {
				t.Fatalf("reading cmd.Stdin: %v", err)
			}
			if !bytes.Equal(gotStdin, tt.input) {
				t.Errorf("Stdin = %q, want %q", gotStdin, tt.input)
			}
		})
	}
}
