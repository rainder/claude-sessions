package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// FLEET Clarify: a read-only side chat about one decision card. The server
// forks FLEET's own session with `claude -p` and a tool set that can look
// things up and cannot change anything. This file is the bottom layer: the
// argv, the preamble and the fork runner. Nothing here knows about HTTP or
// the store.

const (
	clarifyTimeout   = 5 * time.Minute
	clarifyOutputMax = 4 << 20 // bytes of fork stdout; more is a bad answer
	clarifyStderrMax = 8 << 10 // bytes of fork stderr kept for the server log
)

var (
	errClarifyTimeout   = errors.New("clarify: timeout")
	errClarifyBadOutput = errors.New("clarify: bad output")
	errClarifyExit      = errors.New("clarify: fork exited with an error")
)

// clarifyDenyJSON is the --settings value. Bash(*--output*): git diff, log
// and show --output write a file. The rest are the tools that change the
// fleet or start more work. rg is not a Bash look-up at all: --pre and
// --hostname-bin run a program, and the Grep tool already searches.
const clarifyDenyJSON = `{"permissions":{"deny":["Bash(*--output*)","Edit","Write","NotebookEdit","WebFetch","WebSearch","Agent","Task","Workflow","ScheduleWakeup","Skill","ListAgents","ReportFindings"]}}`

// clarifyAllowedBash are the Bash look-ups the fork may run, each as a
// `Bash(<cmd>:*)` prefix rule. The cu rule is added by clarifyArgv, since it
// depends on the configured path.
var clarifyAllowedBash = []string{
	"git log", "git show", "git diff", "git status", "git blame",
	"gh pr view", "gh pr diff", "gh pr checks", "gh pr list", "gh run view",
}

var clarifyAllowedBashTail = []string{
	"claude-sessions list-sessions", "claude-sessions usage",
	"grep", "ls", "cat", "head", "tail", "wc", "jq",
}

type clarifyRunParams struct {
	ResumeID string // FLEET's session id, or the fork's own on a later question
	Fork     bool   // true on the first question only
	Model    string // FLEET's model; "" means opus
	CuPath   string // the cu binary the fork may run `fetch` with
	Preamble string
	Question string
}

// clarifyArgv returns the arguments after the binary name. The `--` before
// the question is required: --allowedTools is variadic and would eat it.
func clarifyArgv(p clarifyRunParams) []string {
	model := p.Model
	if model == "" {
		model = "opus"
	}
	argv := []string{"-p", "--output-format", "json", "--resume", p.ResumeID}
	if p.Fork {
		argv = append(argv, "--fork-session")
	}
	argv = append(argv,
		"--model", model,
		"--setting-sources", "",
		"--strict-mcp-config",
		"--disable-slash-commands",
		"--permission-mode", "dontAsk",
		"--tools", "Bash,Read,Grep,Glob",
		"--settings", clarifyDenyJSON,
		"--allowedTools", "Read", "Grep", "Glob",
	)
	for _, c := range clarifyAllowedBash {
		argv = append(argv, "Bash("+c+":*)")
	}
	argv = append(argv, "Bash("+p.CuPath+" fetch:*)")
	for _, c := range clarifyAllowedBashTail {
		argv = append(argv, "Bash("+c+":*)")
	}
	return append(argv, "--append-system-prompt", p.Preamble, "--", p.Question)
}

// clarifyPreamble tells the fork what it is and what card Andy is asking
// about. The card text is FLEET's own, so it is data for the fork to read.
func clarifyPreamble(c fleetCardView) string {
	var b strings.Builder
	b.WriteString("You are a read-only side chat forked from FLEET. Andy is asking you about one decision card, and only about that card. ")
	b.WriteString("Answer plainly. You may look things up with the tools you have; if a look-up is refused, say so plainly in your answer. ")
	b.WriteString("You must not act on the fleet: do not change files, start sessions or send messages. ")
	b.WriteString("Never resume, re-run or finish any step FLEET left unfinished when you were forked; that work is not yours. ")
	b.WriteString("Nothing you write reaches FLEET.\n\n")
	b.WriteString("The card:\n")
	fmt.Fprintf(&b, "id: %s\n", c.ID)
	if c.Ticket != "" {
		fmt.Fprintf(&b, "ticket: %s\n", c.Ticket)
	}
	fmt.Fprintf(&b, "title: %s\n", c.Title)
	fmt.Fprintf(&b, "summary: %s\n", c.Summary)
	if len(c.Options) > 0 {
		b.WriteString("options:\n")
		for _, o := range c.Options {
			fmt.Fprintf(&b, "- %s: %s\n", o.Key, o.Label)
		}
	}
	if c.Lean != "" {
		fmt.Fprintf(&b, "FLEET leans %s\n", c.Lean)
	}
	return b.String()
}

type clarifyRunResult struct {
	SessionID string
	Text      string
	IsError   bool
	Denials   int
}

// clarifyClaudeBin is the claude binary on PATH, "" when there is none.
func clarifyClaudeBin() string {
	p, err := exec.LookPath("claude")
	if err != nil {
		return ""
	}
	return p
}

// clarifyCuPath is the cu the fork may fetch with: clickup-events.json's
// choice, else cu. A bare cu fails under the service (no keyring), which is
// why the configured wrapper wins.
func clarifyCuPath() string {
	cfg := defaultClickupConfig()
	if path := clickupConfigPathDefault(); path != "" {
		if data, err := os.ReadFile(path); err == nil {
			if c, err := parseClickupConfig(data); err == nil {
				cfg = c
			}
		}
	}
	if p := resolveClickupCu(cfg); p != "" {
		return p
	}
	return "cu"
}

// runClarifyFork runs the fork in cwd with stdin from /dev/null. The context
// kills its whole process group, so a child it started cannot outlive it.
// stderr is capped and goes to the server's own stderr, never to a caller.
func runClarifyFork(ctx context.Context, bin, cwd string, argv []string) (clarifyRunResult, error) {
	cmd := exec.CommandContext(ctx, bin, argv...)
	cmd.Dir = cwd
	cmd.Stdin = nil
	out := &cappedBuffer{max: clarifyOutputMax}
	errBuf := &cappedBuffer{max: clarifyStderrMax}
	cmd.Stdout = out
	cmd.Stderr = errBuf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	if errBuf.buf.Len() > 0 {
		fmt.Fprintf(os.Stderr, "clarify: fork stderr: %s\n", bytes.TrimSpace(errBuf.buf.Bytes()))
	}
	if cerr := ctx.Err(); cerr != nil {
		if errors.Is(cerr, context.DeadlineExceeded) {
			return clarifyRunResult{}, errClarifyTimeout
		}
		return clarifyRunResult{}, cerr
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "clarify: fork failed: %v\n", err)
		return clarifyRunResult{}, errClarifyExit
	}
	if out.over {
		return clarifyRunResult{}, errClarifyBadOutput
	}
	var j struct {
		SessionID         string            `json:"session_id"`
		Result            string            `json:"result"`
		IsError           bool              `json:"is_error"`
		PermissionDenials []json.RawMessage `json:"permission_denials"`
	}
	if err := json.Unmarshal(out.buf.Bytes(), &j); err != nil {
		return clarifyRunResult{}, errClarifyBadOutput
	}
	return clarifyRunResult{SessionID: j.SessionID, Text: j.Result, IsError: j.IsError, Denials: len(j.PermissionDenials)}, nil
}
