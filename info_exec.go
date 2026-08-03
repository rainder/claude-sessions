package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// subprocessWaitDelay forces exec.Cmd to hard-kill a process whose stdout/
// stderr pipe is still held open by a descendant after ctx is canceled —
// exec.Cmd's WaitDelay defaults to zero, which means Wait() can otherwise
// block indefinitely past cancellation. See info_async.go's asyncSection
// doc comment for the fuller cancellation-tradeoff discussion.
const subprocessWaitDelay = 5 * time.Second

// cuFetchFunc fetches a ClickUp ticket via the `cu` CLI. Package-var seam:
// tests must swap this before calling anything that reaches it, since the
// real implementation shells out. TestMain (account_test.go) defaults it to
// panic.
var cuFetchFunc = runCuFetch

func runCuFetch(ctx context.Context, ticketID string) ([]byte, error) {
	out, err := cuFetchCmd(ctx, ticketID).Output()
	return out, wrapExecErr(out, err)
}

// wrapExecErr appends the subprocess's own failure text (first line, capped)
// to a failed exec.Cmd.Output() call, so the caller's err.Error() carries
// something more useful than the bare "exit status N" an *exec.ExitError
// renders on its own. Every caller here (info_ticket.go, info_transcript.go)
// surfaces err.Error() straight to the terminal (info_dialog.go), so without
// this every distinct failure (auth, rate limit, bad argv) looked identical.
//
// stdout is checked first, not stderr: reproduced directly against the real
// `claude` CLI, its own user-facing errors ("Not logged in · Please run
// /login") are written to stdout, and Output() still returns those bytes
// even on a non-zero exit — the caller was just discarding `out` on the
// error path. Stderr (which Output() does populate on *exec.ExitError) is
// the fallback for failures that don't fit that shape (e.g. cu's own
// errors). Either stream is untrusted subprocess output, so it goes through
// sanitizeTerminalText like every other externally-sourced string this
// package renders.
func wrapExecErr(out []byte, err error) error {
	if err == nil {
		return nil
	}
	text := string(out)
	if strings.TrimSpace(text) == "" {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			text = string(ee.Stderr)
		}
	}
	line := strings.TrimSpace(text)
	if line == "" {
		return err
	}
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	return fmt.Errorf("%w: %s", err, sanitizeTerminalText(truncBytesHead(line, 200)))
}

// cuFetchCmd builds (but does not run) the `cu fetch` command. Split out from
// runCuFetch so tests can assert on the constructed argv/WaitDelay without
// ever shelling out to the real `cu` binary.
func cuFetchCmd(ctx context.Context, ticketID string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "cu", "fetch", "--with-comments", ticketID)
	cmd.WaitDelay = subprocessWaitDelay
	return cmd
}

// claudeSummarizeFunc pipes input into `claude -p <instruction>` via stdin
// and returns its stdout. instruction must always be one of this feature's
// own fixed instruction constants — never text derived from fetched
// content, which would reopen the injection surface stdin-only piping
// closes for argv. Package-var seam, same rule as cuFetchFunc.
var claudeSummarizeFunc = runClaudeSummarize

func runClaudeSummarize(ctx context.Context, instruction string, input []byte) ([]byte, error) {
	out, err := claudeSummarizeCmd(ctx, instruction, input).Output()
	return out, wrapExecErr(out, err)
}

// claudeSystemPrompt replaces Claude Code's default system prompt for these
// one-shot summarization calls. Necessary, not cosmetic: the default system
// prompt injects per-machine context (cwd, env info, memory paths, git
// status — see `claude --help`'s --exclude-dynamic-system-prompt-sections,
// which only relocates that context, never removes it) regardless of
// --bare/--safe-mode/cmd.Dir. Reproduced directly: piping a throwaway
// "test input" from a git worktree with an interesting `git status`
// produced a response describing that git state instead of summarizing the
// stdin content — the model used ambient context injected into its own
// system prompt, not tool calls (confirmed with --tools "" and no change).
// Supplying --system-prompt replaces the default prompt outright, so none
// of that per-machine context is ever present to leak into a ticket or
// conversation summary. Verified against the real CLI: identical piped
// conversation input produces a correct, on-topic summary with this prompt
// in place, and a "nothing to summarize" response for trivial input instead
// of unrelated ambient context.
const claudeSystemPrompt = "You are a plain-English summarizer. Base your answer only on the piped stdin content and the instruction that follows; do not use tools, and do not reference any files, directories, or other context."

// claudeSummarizeCmd builds (but does not run) the `claude -p` command. Split
// out from runClaudeSummarize so tests can assert on the constructed
// argv/WaitDelay/Stdin without ever shelling out to the real `claude` binary.
//
// Deliberately NOT --bare: that flag forces Anthropic auth to strictly
// ANTHROPIC_API_KEY/apiKeyHelper and never reads OAuth or the Keychain (see
// `claude --help`'s own --bare description) — this repo's users authenticate
// via OAuth (claude-switch's whole Keychain-snapshot mechanism assumes it),
// so --bare made every summarization call fail closed with "Not logged in"
// (exit 1), reproduced directly against the real CLI.
//
// --tools "" disables all tool access — without it, a one-shot `-p` call
// still has full agentic tool access (Bash, Read, ...) by default and can
// wander the filesystem instead of/alongside processing stdin; reproduced
// directly (see claudeSystemPrompt's comment). --system-prompt is the fix
// for the *context-injection* half of that same risk (see claudeSystemPrompt).
// cmd.Dir stays a neutral, non-project directory (os.TempDir()) as a third,
// independent layer — belt and braces once --tools ""/--system-prompt make
// it no longer load-bearing on its own for this call shape.
func claudeSummarizeCmd(ctx context.Context, instruction string, input []byte) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "claude", "--model", "sonnet", "--effort", "low",
		"--tools", "", "--system-prompt", claudeSystemPrompt, "-p", instruction)
	cmd.Dir = os.TempDir()
	cmd.WaitDelay = subprocessWaitDelay
	cmd.Stdin = bytes.NewReader(input)
	return cmd
}

// summaryBackendFunc resolves which backend resolveSummarizeFunc picks.
// Package-var seam so tests aren't at the mercy of this machine's real
// ~/.config/claude-sessions/summary-backend file; TestMain defaults it to
// the hermetic "claude", which is what every existing claudeSummarizeFunc
// override already assumes.
var summaryBackendFunc = LoadSummaryBackend

// resolveSummarizeFunc picks the exec seam matching the configured backend
// (LoadSummaryBackend/SaveSummaryBackend, config.go). Called fresh on every
// summarization request rather than cached, so a mid-session backend switch
// takes effect on the next ticket/conversation summary without a restart.
func resolveSummarizeFunc() func(context.Context, string, []byte) ([]byte, error) {
	if summaryBackendFunc() == "codex" {
		return codexSummarizeFunc
	}
	return claudeSummarizeFunc
}

// codexSummarizeFunc pipes input into `codex exec` and returns its final
// message, the codex counterpart to claudeSummarizeFunc. Package-var seam,
// same rule as claudeSummarizeFunc: instruction must always be one of this
// feature's own fixed instruction constants, never text derived from fetched
// content.
var codexSummarizeFunc = runCodexSummarize

// codexSystemPreamble stands in for claudeSystemPrompt: `codex exec` has no
// --system-prompt override, so the same anti-injection framing is prepended
// to the prompt argument instead. This is weaker than the claude path's
// --system-prompt + --tools "" combination — codex's --sandbox read-only
// restricts filesystem writes and shell access, not what the prompt tells it
// to attend to — and is accepted as a known limitation of this backend.
const codexSystemPreamble = "You are a plain-English summarizer. Base your answer only on the piped stdin content and the instruction that follows; do not use tools, and do not reference any files, directories, or other context.\n\n"

func runCodexSummarize(ctx context.Context, instruction string, input []byte) ([]byte, error) {
	outFile, err := os.CreateTemp("", "claude-sessions-codex-summary-*.txt")
	if err != nil {
		return nil, err
	}
	outPath := outFile.Name()
	_ = outFile.Close()
	defer os.Remove(outPath)

	var stderr bytes.Buffer
	cmd := codexSummarizeCmd(ctx, instruction, input, outPath)
	cmd.Stdout = io.Discard
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, wrapExecErr(codexFailureLine(stderr.Bytes()), err)
	}
	last, err := os.ReadFile(outPath)
	if err != nil {
		return nil, fmt.Errorf("codex exec produced no output: %w", err)
	}
	if len(bytes.TrimSpace(last)) == 0 {
		return nil, errors.New("codex exec returned an empty summary")
	}
	return last, nil
}

// codexFailureLine picks the line of a failed `codex exec` run's stderr that
// wrapExecErr should surface. Unlike claude's stdout (which puts its own
// user-facing error, e.g. "Not logged in", on the first line), codex's
// stderr opens with several lines of session banner before the actual
// failure — reproduced directly (a bad --model run prints "Reading
// additional input from stdin..." / "OpenAI Codex vN.N.N" / etc. before its
// "ERROR: {...}" line). wrapExecErr on its own only ever keeps the first
// line, which would render every codex failure as that banner instead of the
// error. This returns the last "ERROR:"-prefixed line if there is one, else
// the last non-empty line, so wrapExecErr's first-line trim keeps the right
// text.
func codexFailureLine(stderr []byte) []byte {
	lines := bytes.Split(stderr, []byte("\n"))
	var lastNonEmpty []byte
	for _, line := range lines {
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 {
			continue
		}
		lastNonEmpty = trimmed
		if bytes.HasPrefix(trimmed, []byte("ERROR:")) {
			return trimmed
		}
	}
	return lastNonEmpty
}

// codexSummarizeCmd builds (but does not run) the `codex exec` command. Split
// out from runCodexSummarize so tests can assert on the constructed
// argv/WaitDelay/Stdin without ever shelling out to the real `codex` binary.
//
// --sandbox read-only + --skip-git-repo-check (cmd.Dir is os.TempDir(), a
// non-git directory) + --ephemeral (no persisted session record of the piped
// content) mirror the claude path's own defensive posture as closely as
// `codex exec` allows — see codexSystemPreamble's comment for where the two
// paths diverge. -o writes just the final assistant message to outPath;
// codex's normal stdout is a full session transcript (banner, echoed prompt,
// token counts), not usable as a summary on its own.
func codexSummarizeCmd(ctx context.Context, instruction string, input []byte, outPath string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "codex", "exec",
		"--sandbox", "read-only",
		"--skip-git-repo-check",
		"--ephemeral",
		"-C", os.TempDir(),
		"-m", "gpt-5.6-luna",
		"-c", "model_reasoning_effort=low",
		"-o", outPath,
		codexSystemPreamble+instruction)
	cmd.Dir = os.TempDir()
	cmd.WaitDelay = subprocessWaitDelay
	cmd.Stdin = bytes.NewReader(input)
	return cmd
}
