package main

import (
	"bytes"
	"context"
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

// claudeSummarizeCmd builds (but does not run) the `claude -p` command. Split
// out from runClaudeSummarize so tests can assert on the constructed
// argv/WaitDelay/Stdin without ever shelling out to the real `claude` binary.
func claudeSummarizeCmd(ctx context.Context, instruction string, input []byte) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "claude", "--model", "sonnet", "--effort", "low", "--bare", "-p", instruction)
	cmd.WaitDelay = subprocessWaitDelay
	cmd.Stdin = bytes.NewReader(input)
	return cmd
}
