package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"reflect"
	"strings"
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

// TestWrapExecErr covers the three shapes wrapExecErr must distinguish:
// stdout carrying the real failure text (the `claude` CLI's own shape —
// "Not logged in" is written to stdout, not stderr, and Output() still
// returns it alongside the *exec.ExitError), a stderr fallback for
// processes that follow the usual convention, and a nil/empty-both case
// that must pass the original error through unchanged rather than panic
// or produce an empty-looking wrap.
func TestWrapExecErr(t *testing.T) {
	t.Run("nil error passes through", func(t *testing.T) {
		if err := wrapExecErr([]byte("anything"), nil); err != nil {
			t.Errorf("got %v, want nil", err)
		}
	})

	t.Run("stdout text wins over exit-status-only message", func(t *testing.T) {
		_, err := exec.Command("sh", "-c", "printf 'Not logged in - Please run /login\\nmore\\n'; exit 1").Output()
		if err == nil {
			t.Fatal("expected the sh command to fail")
		}
		out := []byte("Not logged in - Please run /login\nmore\n")
		got := wrapExecErr(out, err)
		if !strings.Contains(got.Error(), "Not logged in - Please run /login") {
			t.Errorf("got %q, want it to contain the stdout failure line", got.Error())
		}
		if strings.Contains(got.Error(), "more") {
			t.Errorf("got %q, want only the first line kept", got.Error())
		}
	})

	t.Run("falls back to stderr when stdout is empty", func(t *testing.T) {
		cmd := exec.Command("sh", "-c", "echo boom >&2; exit 1")
		_, err := cmd.Output()
		if err == nil {
			t.Fatal("expected the sh command to fail")
		}
		got := wrapExecErr(nil, err)
		if !strings.Contains(got.Error(), "boom") {
			t.Errorf("got %q, want it to contain the stderr text", got.Error())
		}
	})

	t.Run("both empty returns the original error unchanged", func(t *testing.T) {
		cmd := exec.Command("sh", "-c", "exit 1")
		_, err := cmd.Output()
		if err == nil {
			t.Fatal("expected the sh command to fail")
		}
		got := wrapExecErr(nil, err)
		if got != err {
			t.Errorf("got %v, want the original error unchanged", got)
		}
	})
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

// TestCodexSummarizeCmdArgs verifies the real argv `codex exec --sandbox
// read-only --skip-git-repo-check --ephemeral -C <dir> -m gpt-5.6-luna -c
// model_reasoning_effort=low -o <outPath> <preamble+instruction>` constructed
// by codexSummarizeCmd — the codex counterpart to TestClaudeSummarizeCmdArgs.
func TestCodexSummarizeCmdArgs(t *testing.T) {
	tests := []struct {
		name        string
		instruction string
		input       []byte
		outPath     string
	}{
		{
			name:        "summarize instruction",
			instruction: "summarize this ticket",
			input:       []byte("ticket body"),
			outPath:     "/tmp/out-1.txt",
		},
		{
			name:        "empty input",
			instruction: "instr",
			input:       []byte(""),
			outPath:     "/tmp/out-2.txt",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := codexSummarizeCmd(context.Background(), tt.instruction, tt.input, tt.outPath)
			wantArgs := []string{"codex", "exec", "--sandbox", "read-only", "--skip-git-repo-check",
				"--ephemeral", "-C", os.TempDir(), "-m", "gpt-5.6-luna", "-c", "model_reasoning_effort=low",
				"-o", tt.outPath, codexSystemPreamble + tt.instruction}
			if !reflect.DeepEqual(cmd.Args, wantArgs) {
				t.Errorf("Args = %v, want %v", cmd.Args, wantArgs)
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

// TestCodexFailureLine covers the shape a real failing `codex exec` run
// produces (banner lines, then an "ERROR:"-prefixed line — see the
// codexFailureLine doc comment) plus the fallback shapes: no ERROR line at
// all, and an empty buffer.
func TestCodexFailureLine(t *testing.T) {
	t.Run("picks the ERROR line over the banner", func(t *testing.T) {
		stderr := []byte("Reading additional input from stdin...\nOpenAI Codex v0.145.0\n--------\nworkdir: /tmp\n" +
			`ERROR: {"type":"error","status":400,"error":{"message":"bad model"}}` + "\n")
		got := codexFailureLine(stderr)
		want := `ERROR: {"type":"error","status":400,"error":{"message":"bad model"}}`
		if string(got) != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("falls back to the last non-empty line when there is no ERROR line", func(t *testing.T) {
		stderr := []byte("some banner text\nlast diagnostic line\n\n")
		got := codexFailureLine(stderr)
		if string(got) != "last diagnostic line" {
			t.Errorf("got %q, want %q", got, "last diagnostic line")
		}
	})

	t.Run("empty stderr returns nil", func(t *testing.T) {
		if got := codexFailureLine(nil); got != nil {
			t.Errorf("got %q, want nil", got)
		}
	})
}

// TestResolveSummarizeFunc covers both branches of the summary-backend
// switch (config.go's LoadSummaryBackend/SaveSummaryBackend) — the reason
// resolveSummarizeFunc exists at all.
func TestResolveSummarizeFunc(t *testing.T) {
	prevBackend := summaryBackendFunc
	t.Cleanup(func() { summaryBackendFunc = prevBackend })

	t.Run("codex backend", func(t *testing.T) {
		summaryBackendFunc = func() string { return "codex" }
		got := reflect.ValueOf(resolveSummarizeFunc()).Pointer()
		want := reflect.ValueOf(codexSummarizeFunc).Pointer()
		if got != want {
			t.Errorf("resolveSummarizeFunc() with backend=codex did not return codexSummarizeFunc")
		}
	})

	t.Run("claude backend (default)", func(t *testing.T) {
		summaryBackendFunc = func() string { return "claude" }
		got := reflect.ValueOf(resolveSummarizeFunc()).Pointer()
		want := reflect.ValueOf(claudeSummarizeFunc).Pointer()
		if got != want {
			t.Errorf("resolveSummarizeFunc() with backend=claude did not return claudeSummarizeFunc")
		}
	})

	t.Run("unrecognized value falls back to claude", func(t *testing.T) {
		summaryBackendFunc = func() string { return "bogus" }
		got := reflect.ValueOf(resolveSummarizeFunc()).Pointer()
		want := reflect.ValueOf(claudeSummarizeFunc).Pointer()
		if got != want {
			t.Errorf("resolveSummarizeFunc() with an unrecognized backend did not fall back to claudeSummarizeFunc")
		}
	})
}
