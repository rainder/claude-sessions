package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
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
	return cuFetchCmd(ctx, ticketID).Output()
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
	return claudeSummarizeCmd(ctx, instruction, input).Output()
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
