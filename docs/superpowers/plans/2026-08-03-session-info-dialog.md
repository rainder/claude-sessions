# Session info dialog ('i' hotkey) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an `i`/`I` hotkey that opens a modal showing a session's ticket summary and conversation summary, both generated via `claude -p --bare`.

**Architecture:** Two independent async fetch pipelines (ticket via `cu`+`claude`, conversation via transcript-read-or-remote-fetch+`claude`) each wrapped in a new `asyncSection` (a `previewPane` plus an owned `context.CancelFunc`), rendered in a new bordered modal modeled on `resumePromptsOverlay`. Both pipelines route their expensive step (the `claude -p` call) through a small bounded single-flight+TTL cache. Remote sessions fetch raw transcript text from a new server endpoint; summarization always happens locally.

**Tech Stack:** Go stdlib only (`os/exec`, `net/http`, `encoding/json`, `bufio`) — matches the repo's existing "stdlib + golang.org/x/term + golang.org/x/sys only" constraint.

## Global Constraints

- All `claude -p` invocations use `--model sonnet --effort low --bare` exactly.
- All subprocess calls use `exec.Command`/`exec.CommandContext` with argv arguments — never `sh -c`, never a hand-built shell string.
- Untrusted fetched content (ticket text, transcript excerpts) is always passed to `claude -p` via **stdin**, never concatenated into the `-p` argument string. The `-p` argument is always one of the two fixed instruction constants defined in Task 1/Task 4.
- Every external CLI invocation (`cu`, `claude`) goes through a package-var seam (`cuFetchFunc`, `claudeSummarizeFunc`) so no test ever shells out for real. `TestMain` (account_test.go) defaults both to panic, matching the existing `keychainRead`/`keychainWrite`/`usageInfoFetch` seams.
- `n` (turn count) on the new `/transcript-tail` endpoint is server-side clamped to `1..10`, default `5` — mirrors `previewLimitsFromRequest`'s clamp-or-400 convention (server.go:1033-1051).
- No prefetching anywhere in this feature — every fetch fires only from an explicit `i` keypress.
- Go version / module: match whatever `go.mod` already declares; no new dependencies.

---

## File Structure

**New files:**
- `info_ticket.go` — ticket-id detection regex + the `cu`→`claude` ticket-summary pipeline.
- `info_exec.go` — the two subprocess seams (`cuFetchFunc`, `claudeSummarizeFunc`) shared by both pipelines.
- `info_transcript.go` — bounded tail-scan transcript extractor, conversation-summary pipeline (local + remote), cache-key helper.
- `info_cache.go` — a small bounded single-flight+TTL cache type, instantiated once for tickets and once for conversation summaries.
- `info_async.go` — `asyncSection` (wraps `previewPane` with a `context.CancelFunc`) and `modalWakesWithAll`.
- `info_dialog.go` — header assembly, the new bordered-box renderer, and `showInfoDialog` (the modal's own event loop, modeled on `resumePromptsOverlay`).
- Matching `_test.go` file per new file above.

**Modified files:**
- `server.go` — new `transcriptTailResponse` type, `s.transcriptTail` handler, one new route registration line.
- `remote_actions.go` — new `fetchRemoteTranscriptTail` function, modeled directly on the existing `fetchRemotePreview` (remote_actions.go:136-170).
- `tui.go` — one new `case "i", "I":` in the main key switch.
- `account_test.go` — two new panic defaults added to the existing `TestMain`.

---

### Task 1: Ticket-id detection

**Files:**
- Create: `info_ticket.go`
- Test: `info_ticket_test.go`

**Interfaces:**
- Consumes: `worktreeName(cwd string) string` (session.go:124-135, already exists).
- Produces: `detectTicketID(cwd, name string) string` — used by Task 6 (`showInfoDialog`) and Task 3 (nothing else in this task's file needs it yet).

- [ ] **Step 1: Write the failing tests**

```go
// info_ticket_test.go
package main

import "testing"

func TestDetectTicketID(t *testing.T) {
	cases := []struct {
		name string
		cwd  string
		sess string
		want string
	}{
		{"worktree basename match", "/Users/x/repo/.claude/worktrees/DR-860-fix-thing", "irrelevant", "DR-860"},
		{"session name fallback", "/Users/x/repo", "Fix DR-2213 login bug", "DR-2213"},
		{"worktree takes precedence over name", "/Users/x/repo/.claude/worktrees/DR-1", "mentions DR-999 too", "DR-1"},
		{"no match", "/Users/x/repo", "just a normal session", ""},
		{"rejects ADR prefix", "/Users/x/repo/.claude/worktrees/ADR-123", "ADR-123 design doc", ""},
		{"rejects trailing alnum suffix", "/Users/x/repo", "DR-123abc not a ticket", ""},
		{"accepts short and long digit runs", "/Users/x/repo", "DR-12 and DR-123456 both match, first wins", "DR-12"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := detectTicketID(c.cwd, c.sess); got != c.want {
				t.Errorf("detectTicketID(%q, %q) = %q, want %q", c.cwd, c.sess, got, c.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./... -run TestDetectTicketID -v`
Expected: FAIL — `detectTicketID` undefined.

- [ ] **Step 3: Write the implementation**

```go
// info_ticket.go
package main

import "regexp"

// ticketIDRe matches a ClickUp custom id like DR-860. \b is an ASCII word
// boundary in Go's RE2 engine, so this rejects ADR-123 (no boundary between
// "A" and "D") and DR-123abc (no boundary between "3" and "a") — a bare
// `DR-\d+` would have matched both.
var ticketIDRe = regexp.MustCompile(`\bDR-\d{2,6}\b`)

// detectTicketID looks for a ClickUp ticket id in the worktree directory
// basename first (the authoritative source — pickup-next-ticket names
// worktrees by ticket id), falling back to the session name. "" if neither
// matches.
func detectTicketID(cwd, name string) string {
	if wt := worktreeName(cwd); wt != "" {
		if id := ticketIDRe.FindString(wt); id != "" {
			return id
		}
	}
	return ticketIDRe.FindString(name)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./... -run TestDetectTicketID -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add info_ticket.go info_ticket_test.go
git commit -m "feat: detect DR-XXXX ticket ids for the info dialog"
```

---

### Task 2: Subprocess exec seams

**Files:**
- Create: `info_exec.go`
- Test: `info_exec_test.go`
- Modify: `account_test.go` (add two lines to the existing `TestMain`)

**Interfaces:**
- Produces: `var cuFetchFunc func(ctx context.Context, ticketID string) ([]byte, error)`, `var claudeSummarizeFunc func(ctx context.Context, instruction string, input []byte) ([]byte, error)` — consumed by Task 3 (`fetchTicketSummary`) and Task 4 (`summarizeTurns`).

- [ ] **Step 1: Write the failing test**

```go
// info_exec_test.go
package main

import (
	"context"
	"errors"
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./... -run 'TestCuFetchFuncSeamIsSwappable|TestClaudeSummarizeFuncSeamIsSwappable' -v`
Expected: FAIL — `cuFetchFunc`/`claudeSummarizeFunc` undefined.

- [ ] **Step 3: Write the implementation**

```go
// info_exec.go
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
	cmd := exec.CommandContext(ctx, "cu", "fetch", "--with-comments", ticketID)
	cmd.WaitDelay = subprocessWaitDelay
	return cmd.Output()
}

// claudeSummarizeFunc pipes input into `claude -p <instruction>` via stdin
// and returns its stdout. instruction must always be one of this feature's
// own fixed instruction constants — never text derived from fetched
// content, which would reopen the injection surface stdin-only piping
// closes for argv. Package-var seam, same rule as cuFetchFunc.
var claudeSummarizeFunc = runClaudeSummarize

func runClaudeSummarize(ctx context.Context, instruction string, input []byte) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "claude", "--model", "sonnet", "--effort", "low", "--bare", "-p", instruction)
	cmd.WaitDelay = subprocessWaitDelay
	cmd.Stdin = bytes.NewReader(input)
	return cmd.Output()
}
```

- [ ] **Step 4: Wire the TestMain defaults**

Read `account_test.go` first (it already exists), then add two lines inside the existing `TestMain` function, next to the `usageInfoFetch` default:

```go
// account_test.go — inside func TestMain(m *testing.M), after the existing
// usageInfoFetch panic default:
	cuFetchFunc = func(context.Context, string) ([]byte, error) { panic("test reached the real cu CLI") }
	claudeSummarizeFunc = func(context.Context, string, []byte) ([]byte, error) { panic("test reached the real claude CLI") }
```

(`context` is likely already imported in account_test.go for other reasons; if not, add `"context"` to its import block.)

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./... -run 'TestCuFetchFuncSeamIsSwappable|TestClaudeSummarizeFuncSeamIsSwappable' -v`
Expected: PASS

- [ ] **Step 6: Run the full test suite to confirm the TestMain edit didn't break anything else**

Run: `go test ./...`
Expected: PASS (all existing tests still green)

- [ ] **Step 7: Commit**

```bash
git add info_exec.go info_exec_test.go account_test.go
git commit -m "feat: add cu/claude subprocess seams for the info dialog"
```

---

### Task 3: Ticket summary pipeline

**Files:**
- Modify: `info_ticket.go` (append to the file created in Task 1)
- Test: `info_ticket_test.go` (append)

**Interfaces:**
- Consumes: `cuFetchFunc`, `claudeSummarizeFunc` (Task 2); `PreviewResult` struct (preview.go:42-50, already exists: `{Source, Label, Content string}`); `trunc(s string, n int) string` (preview.go:395, already exists).
- Produces: `func fetchTicketSummary(ctx context.Context, ticketID string) (PreviewResult, error)` — consumed by Task 5 (`fetchTicketSummaryCached`).

- [ ] **Step 1: Write the failing tests**

```go
// info_ticket_test.go — append
func TestFetchTicketSummarySuccess(t *testing.T) {
	prevCu, prevClaude := cuFetchFunc, claudeSummarizeFunc
	t.Cleanup(func() { cuFetchFunc, claudeSummarizeFunc = prevCu, prevClaude })

	cuFetchFunc = func(ctx context.Context, id string) ([]byte, error) {
		return []byte("raw ticket text"), nil
	}
	claudeSummarizeFunc = func(ctx context.Context, instruction string, input []byte) ([]byte, error) {
		if instruction != ticketSummaryInstruction {
			t.Errorf("instruction = %q", instruction)
		}
		if string(input) != "raw ticket text" {
			t.Errorf("input = %q", input)
		}
		return []byte("  short summary  \n"), nil
	}

	got, err := fetchTicketSummary(context.Background(), "DR-1")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got.Content != "short summary" {
		t.Errorf("Content = %q, want trimmed summary", got.Content)
	}
	if got.Label != "DR-1" {
		t.Errorf("Label = %q", got.Label)
	}
}

func TestFetchTicketSummaryCuFails(t *testing.T) {
	prevCu := cuFetchFunc
	t.Cleanup(func() { cuFetchFunc = prevCu })
	cuFetchFunc = func(ctx context.Context, id string) ([]byte, error) {
		return nil, errors.New("network down")
	}
	_, err := fetchTicketSummary(context.Background(), "DR-1")
	if err == nil {
		t.Fatal("want non-nil error when cu fetch fails")
	}
}

func TestFetchTicketSummaryClaudeFailsFallsBackToRaw(t *testing.T) {
	prevCu, prevClaude := cuFetchFunc, claudeSummarizeFunc
	t.Cleanup(func() { cuFetchFunc, claudeSummarizeFunc = prevCu, prevClaude })
	cuFetchFunc = func(ctx context.Context, id string) ([]byte, error) {
		return []byte("raw ticket text"), nil
	}
	claudeSummarizeFunc = func(ctx context.Context, instruction string, input []byte) ([]byte, error) {
		return nil, errors.New("rate limited")
	}
	got, err := fetchTicketSummary(context.Background(), "DR-1")
	if err != nil {
		t.Fatalf("err = %v, want nil (raw fallback is not an error)", err)
	}
	if !strings.Contains(got.Content, "raw ticket text") {
		t.Errorf("Content = %q, want it to contain the raw text", got.Content)
	}
	if !strings.Contains(got.Content, "summary unavailable") {
		t.Errorf("Content = %q, want a summary-unavailable note", got.Content)
	}
}
```

Add `"errors"` and `"strings"` to `info_ticket_test.go`'s imports.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./... -run TestFetchTicketSummary -v`
Expected: FAIL — `fetchTicketSummary`/`ticketSummaryInstruction` undefined.

- [ ] **Step 3: Write the implementation**

```go
// info_ticket.go — append
import (
	"context"
	"fmt"
	"strings"
)

const (
	ticketSummaryInstruction = "what is being fixed? short version like i am 25"
	ticketRawCap             = 4000 // chars of raw `cu fetch` text kept on a summarization failure
)

// fetchTicketSummary runs `cu fetch --with-comments <id>` piped into
// `claude -p` to produce a short plain-English summary. Three outcomes:
//   - cu fetch fails: returns a non-nil error (the caller's asyncSection
//     surfaces this as "unavailable" — there's no raw text to fall back to).
//   - cu succeeds but claude fails: returns the raw cu output (truncated)
//     with a "summary unavailable" note, and a nil error — the dialog never
//     blanks when only the LLM leg failed.
//   - both succeed: returns the trimmed summary.
func fetchTicketSummary(ctx context.Context, ticketID string) (PreviewResult, error) {
	raw, err := cuFetchFunc(ctx, ticketID)
	if err != nil {
		return PreviewResult{}, fmt.Errorf("cu fetch failed: %w", err)
	}
	summary, err := claudeSummarizeFunc(ctx, ticketSummaryInstruction, raw)
	if err != nil {
		content := fmt.Sprintf("[summary unavailable: %s]\n\n%s", err, trunc(string(raw), ticketRawCap))
		return PreviewResult{Source: "ticket", Label: ticketID, Content: content}, nil
	}
	return PreviewResult{Source: "ticket", Label: ticketID, Content: strings.TrimSpace(string(summary))}, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./... -run TestFetchTicketSummary -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add info_ticket.go info_ticket_test.go
git commit -m "feat: cu-fetch + claude-summarize pipeline for ticket section"
```

---

### Task 4: Conversation tail extractor

**Files:**
- Create: `info_transcript.go`
- Test: `info_transcript_test.go`

**Interfaces:**
- Consumes: `writeTranscript(t *testing.T, lines ...string) string` (model_test.go:88-100, already exists — reuse in tests, do not redefine).
- Produces: `type transcriptTurn struct { Role, Text string }` (with json tags — reused by Task 7's server response and Task 8's client fetch), `func extractConversationTail(path string, n int) ([]transcriptTurn, error)` — consumed by Task 5.

- [ ] **Step 1: Write the failing tests**

```go
// info_transcript_test.go
package main

import "testing"

func TestExtractConversationTailBasic(t *testing.T) {
	p := writeTranscript(t,
		`{"type":"user","message":{"role":"user","content":"first question"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"first answer"}]}}`,
		`{"type":"user","message":{"role":"user","content":"second question"}}`,
	)
	turns, err := extractConversationTail(p, 5)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	want := []transcriptTurn{
		{Role: "user", Text: "first question"},
		{Role: "assistant", Text: "first answer"},
		{Role: "user", Text: "second question"},
	}
	if len(turns) != len(want) {
		t.Fatalf("got %d turns, want %d: %+v", len(turns), len(want), turns)
	}
	for i := range want {
		if turns[i] != want[i] {
			t.Errorf("turn %d = %+v, want %+v", i, turns[i], want[i])
		}
	}
}

func TestExtractConversationTailStripsToolBlocks(t *testing.T) {
	p := writeTranscript(t,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"thinking","thinking":"pondering"},{"type":"tool_use","name":"Bash","input":{}},{"type":"text","text":"the real answer"}]}}`,
	)
	turns, err := extractConversationTail(p, 5)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(turns) != 1 || turns[0].Text != "the real answer" {
		t.Errorf("got %+v, want single turn with only the text block", turns)
	}
}

func TestExtractConversationTailKeepsOnlyLastN(t *testing.T) {
	var lines []string
	for i := 0; i < 8; i++ {
		lines = append(lines, `{"type":"user","message":{"role":"user","content":"msg"}}`)
	}
	p := writeTranscript(t, lines...)
	turns, err := extractConversationTail(p, 3)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(turns) != 3 {
		t.Errorf("got %d turns, want 3", len(turns))
	}
}

func TestExtractConversationTailDedupesLastWins(t *testing.T) {
	p := writeTranscript(t,
		`{"type":"assistant","requestId":"r1","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"partial"}]}}`,
		`{"type":"assistant","requestId":"r1","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"partial complete now"}]}}`,
	)
	turns, err := extractConversationTail(p, 5)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("got %d turns, want 1 (deduped)", len(turns))
	}
	if turns[0].Text != "partial complete now" {
		t.Errorf("Text = %q, want the later (more complete) re-emission to win", turns[0].Text)
	}
}

func TestExtractConversationTailSkipsNonTextEntries(t *testing.T) {
	p := writeTranscript(t,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Bash"}]}}`,
		`{"type":"user","message":{"role":"user","content":"real question"}}`,
	)
	turns, err := extractConversationTail(p, 5)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(turns) != 1 || turns[0].Text != "real question" {
		t.Errorf("got %+v, want only the real user turn", turns)
	}
}

func TestExtractConversationTailMissingFile(t *testing.T) {
	_, err := extractConversationTail("/nonexistent/path.jsonl", 5)
	if err == nil {
		t.Fatal("want error for missing file")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./... -run TestExtractConversationTail -v`
Expected: FAIL — `extractConversationTail`/`transcriptTurn` undefined.

- [ ] **Step 3: Write the implementation**

```go
// info_transcript.go
package main

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"strings"
)

// transcriptTurn is one user/assistant text turn. json tags are load-bearing:
// this type is also the wire shape for the /transcript-tail server response
// (Task 7) and its client decode (Task 8), not just a local value type.
type transcriptTurn struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

// infoTailBytes bounds how much of the transcript tail extractConversationTail
// scans, mirroring model.go's modelTailBytes/scanTranscript convention
// (seek-then-scan a fixed byte budget) rather than accumulating the whole
// file the way preview.go's formatTranscriptTail does.
const infoTailBytes = 256 * 1024

type transcriptLine struct {
	Type      string `json:"type"`
	RequestID string `json:"requestId"`
	Message   struct {
		ID      string          `json:"id"`
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// extractTurnText pulls plain text out of a message.content value, which is
// either a bare string or an array of content blocks. Only "text" blocks are
// kept — thinking/tool_use/tool_result are dropped. Returns ok=false if
// there's no usable text (e.g. a tool-only assistant turn).
func extractTurnText(contentRaw json.RawMessage) (string, bool) {
	var s string
	if err := json.Unmarshal(contentRaw, &s); err == nil {
		s = strings.TrimSpace(s)
		if s == "" {
			return "", false
		}
		return s, true
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(contentRaw, &blocks); err != nil {
		return "", false
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" {
			if t := strings.TrimSpace(b.Text); t != "" {
				parts = append(parts, t)
			}
		}
	}
	if len(parts) == 0 {
		return "", false
	}
	return strings.Join(parts, "\n"), true
}

// turnBuilder keeps only the most recent max turns, deduplicating assistant
// streaming re-emissions by dedupKey with last-wins semantics: a later
// re-emission of the same message.id+requestId overwrites the earlier one
// in place (same position), since it's typically the more complete text.
// This matches scanTranscript's "tracked independently (last wins)"
// convention (model.go:92-93) rather than cost.go's first-seen-wins, which
// is correct for accounting but wrong for picking the most complete text.
type turnBuilder struct {
	turns    []transcriptTurn
	idxByKey map[string]int
	max      int
}

func newTurnBuilder(max int) *turnBuilder {
	return &turnBuilder{idxByKey: make(map[string]int), max: max}
}

func (b *turnBuilder) add(role, text, dedupKey string) {
	if dedupKey != "" {
		if i, ok := b.idxByKey[dedupKey]; ok {
			b.turns[i].Text = text
			return
		}
	}
	b.turns = append(b.turns, transcriptTurn{Role: role, Text: text})
	if dedupKey != "" {
		b.idxByKey[dedupKey] = len(b.turns) - 1
	}
	if len(b.turns) > b.max {
		b.turns = b.turns[1:]
		for k, i := range b.idxByKey {
			if i == 0 {
				delete(b.idxByKey, k)
			} else {
				b.idxByKey[k] = i - 1
			}
		}
	}
}

// extractConversationTail returns the last n user/assistant text-only turns
// from the transcript at path. Seeks to the file's tail before scanning
// (see infoTailBytes) rather than accumulating the whole file.
func extractConversationTail(path string, n int) ([]transcriptTurn, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	seeked := false
	if st, err := f.Stat(); err == nil && st.Size() > infoTailBytes {
		if _, err := f.Seek(st.Size()-infoTailBytes, io.SeekStart); err == nil {
			seeked = true
		}
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	if seeked {
		scanner.Scan() // discard the partial first line
	}

	b := newTurnBuilder(n)
	for scanner.Scan() {
		var e transcriptLine
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			continue
		}
		if e.Type != "user" && e.Type != "assistant" {
			continue
		}
		text, ok := extractTurnText(e.Message.Content)
		if !ok {
			continue
		}
		dedupKey := ""
		if e.Type == "assistant" {
			dedupKey = e.Message.ID + "\x00" + e.RequestID
		}
		b.add(e.Type, text, dedupKey)
	}
	return b.turns, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./... -run TestExtractConversationTail -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add info_transcript.go info_transcript_test.go
git commit -m "feat: bounded-tail conversation turn extractor"
```

---

### Task 5: Bounded summary cache

**Files:**
- Create: `info_cache.go`
- Test: `info_cache_test.go`

**Interfaces:**
- Consumes: `PreviewResult` (preview.go:42-50).
- Produces: `type summaryCache struct{...}`, `func newSummaryCache(successTTL, failTTL, fetchTimeout time.Duration, max int) *summaryCache` (see the post-implementation correction note at the end of this task — the initial TDD steps below build a 2-arg version that fix round 1 replaces), `func (c *summaryCache) getOrFetch(ctx context.Context, key string, fetch func(context.Context) (PreviewResult, error)) (PreviewResult, error)` — consumed by Task 6 (`fetchTicketSummaryCached`) and Task 7 (`fetchConversationSummaryLocal`/`Remote`).

- [ ] **Step 1: Write the failing tests**

```go
// info_cache_test.go
package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestSummaryCacheHitAvoidsRefetch(t *testing.T) {
	c := newSummaryCache(time.Hour, 10)
	var calls int32
	fetch := func(ctx context.Context) (PreviewResult, error) {
		atomic.AddInt32(&calls, 1)
		return PreviewResult{Content: "result"}, nil
	}
	for i := 0; i < 3; i++ {
		got, err := c.getOrFetch(context.Background(), "k1", fetch)
		if err != nil || got.Content != "result" {
			t.Fatalf("call %d: got (%+v, %v)", i, got, err)
		}
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("fetch called %d times, want 1", n)
	}
}

func TestSummaryCacheDifferentKeysDontShare(t *testing.T) {
	c := newSummaryCache(time.Hour, 10)
	var calls int32
	fetch := func(ctx context.Context) (PreviewResult, error) {
		atomic.AddInt32(&calls, 1)
		return PreviewResult{Content: "result"}, nil
	}
	c.getOrFetch(context.Background(), "k1", fetch)
	c.getOrFetch(context.Background(), "k2", fetch)
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Errorf("fetch called %d times, want 2", n)
	}
}

func TestSummaryCacheExpiredEntryRefetches(t *testing.T) {
	c := newSummaryCache(time.Millisecond, 10)
	var calls int32
	fetch := func(ctx context.Context) (PreviewResult, error) {
		atomic.AddInt32(&calls, 1)
		return PreviewResult{Content: "result"}, nil
	}
	c.getOrFetch(context.Background(), "k1", fetch)
	time.Sleep(5 * time.Millisecond)
	c.getOrFetch(context.Background(), "k1", fetch)
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Errorf("fetch called %d times, want 2 after expiry", n)
	}
}

func TestSummaryCacheEvictsOverCapacity(t *testing.T) {
	c := newSummaryCache(time.Hour, 2)
	fetch := func(ctx context.Context) (PreviewResult, error) {
		return PreviewResult{Content: "result"}, nil
	}
	c.getOrFetch(context.Background(), "k1", fetch)
	c.getOrFetch(context.Background(), "k2", fetch)
	c.getOrFetch(context.Background(), "k3", fetch)
	c.mu.Lock()
	n := len(c.entries)
	c.mu.Unlock()
	if n > 2 {
		t.Errorf("cache has %d entries, want <= 2 (bounded)", n)
	}
}

func TestSummaryCacheConcurrentCallersJoinOneFlight(t *testing.T) {
	c := newSummaryCache(time.Hour, 10)
	var calls int32
	start := make(chan struct{})
	fetch := func(ctx context.Context) (PreviewResult, error) {
		atomic.AddInt32(&calls, 1)
		<-start
		return PreviewResult{Content: "result"}, nil
	}
	done := make(chan struct{}, 5)
	for i := 0; i < 5; i++ {
		go func() {
			c.getOrFetch(context.Background(), "k1", fetch)
			done <- struct{}{}
		}()
	}
	time.Sleep(10 * time.Millisecond) // let all 5 reach getOrFetch
	close(start)
	for i := 0; i < 5; i++ {
		<-done
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("fetch called %d times, want 1 (single-flight)", n)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./... -run TestSummaryCache -v`
Expected: FAIL — `newSummaryCache` undefined.

- [ ] **Step 3: Write the implementation**

```go
// info_cache.go
package main

import (
	"context"
	"sort"
	"sync"
	"time"
)

// summaryCacheEntry holds one in-flight-or-completed fetch. done is closed
// once fetch returns; a caller that arrives before that selects on it,
// joining the same flight rather than starting a second one.
type summaryCacheEntry struct {
	result  PreviewResult
	err     error
	expires time.Time
	done    chan struct{}
}

// summaryCache is a small single-flight + TTL + bounded-size cache, modeled
// on usage_cache.go's GetOrFetch *shape* (single-flight + TTL), not reused
// literally — usage_cache.go's key space ("accounts this host holds a
// snapshot for") is small and explicitly documented as needing no size
// bound; this cache's key spaces (ticket ids, and (host,session,mtime,size)
// tuples) are not, so eviction-over-capacity is new logic here.
//
// The underlying fetch always runs to its own bounded completion,
// independent of any individual caller's context — ctx here only bounds how
// long THIS caller waits to join, not the flight's lifetime. This means a
// caller that cancels while it happens to be the one that started the
// fetch does not abort the underlying subprocess; the fetch keeps running,
// bounded by whatever timeout its own closure applies (e.g.
// infoDialogTimeout), to finish populating the cache for the next lookup.
// This is an intentional "narrow the race, don't guess" tradeoff, the same
// shape CLAUDE.md documents for this repo's kill/migrate preconditions —
// not a leak, since it always terminates and its result is useful.
type summaryCache struct {
	mu      sync.Mutex
	entries map[string]*summaryCacheEntry
	ttl     time.Duration
	max     int
}

func newSummaryCache(ttl time.Duration, max int) *summaryCache {
	return &summaryCache{entries: make(map[string]*summaryCacheEntry), ttl: ttl, max: max}
}

func (c *summaryCache) getOrFetch(ctx context.Context, key string, fetch func(context.Context) (PreviewResult, error)) (PreviewResult, error) {
	c.mu.Lock()
	if e, ok := c.entries[key]; ok && time.Now().Before(e.expires) {
		c.mu.Unlock()
		select {
		case <-e.done:
			return e.result, e.err
		case <-ctx.Done():
			return PreviewResult{}, ctx.Err()
		}
	} else if ok {
		delete(c.entries, key)
	}
	e := &summaryCacheEntry{done: make(chan struct{})}
	c.entries[key] = e
	c.prune()
	c.mu.Unlock()

	e.result, e.err = fetch(ctx)
	e.expires = time.Now().Add(c.ttl)
	close(e.done)
	return e.result, e.err
}

// prune drops expired entries, then — if still over capacity — evicts
// completed entries with the soonest expiry until back at c.max. An
// in-flight entry (done not yet closed) is never evicted: its key is the
// only thing joiners have to find it by. Caller holds c.mu.
func (c *summaryCache) prune() {
	now := time.Now()
	for k, e := range c.entries {
		select {
		case <-e.done:
			if now.After(e.expires) {
				delete(c.entries, k)
			}
		default:
		}
	}
	if len(c.entries) <= c.max {
		return
	}
	type kv struct {
		key     string
		expires time.Time
	}
	var evictable []kv
	for k, e := range c.entries {
		select {
		case <-e.done:
			evictable = append(evictable, kv{k, e.expires})
		default:
		}
	}
	sort.Slice(evictable, func(i, j int) bool { return evictable[i].expires.Before(evictable[j].expires) })
	for _, item := range evictable {
		if len(c.entries) <= c.max {
			break
		}
		delete(c.entries, item.key)
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./... -run TestSummaryCache -v -race`
Expected: PASS (run with `-race` since this task's own tests exercise concurrent callers)

- [ ] **Step 5: Commit**

```bash
git add info_cache.go info_cache_test.go
git commit -m "feat: bounded single-flight cache for info dialog summaries"
```

**Post-implementation correction (fix round 1, commit 9843d5d):** the TDD steps above show the *initial* reference code. Review found the initiating caller's `ctx` wrongly governed the shared flight's lifetime — the fetch must run under a context the cache owns itself, or one caller closing its dialog can kill a fetch a different caller (or a fast reopen of the same dialog) is joined on. The actual, final constructor is:

```go
func newSummaryCache(successTTL, failTTL, fetchTimeout time.Duration, max int) *summaryCache
```

`successTTL`/`failTTL` replace the single `ttl` field (a failed fetch is cached for a shorter window than a success, mirroring `usage_cache.go`'s `usageCacheTTL`/`usageCacheFailTTL` split). `fetchTimeout` bounds the cache-owned context the actual `fetch(...)` call runs under — never the caller's own `ctx`, which now only bounds how long that caller *waits*. Publication is also now panic-safe (wrapped in a `defer`/`recover`), and `prune`'s never-evict-in-flight invariant has direct test coverage. Every call site below uses this final signature.

---

### Task 6: Wire the ticket cache

**Files:**
- Modify: `info_ticket.go` (append)
- Test: `info_ticket_test.go` (append)

**Interfaces:**
- Consumes: `fetchTicketSummary` (Task 3), `newSummaryCache`/`getOrFetch` (Task 5).
- Produces: `func fetchTicketSummaryCached(ctx context.Context, ticketID string) (PreviewResult, error)` — consumed by Task 10 (`showInfoDialog`).

- [ ] **Step 1: Write the failing test**

```go
// info_ticket_test.go — append
func TestFetchTicketSummaryCachedAvoidsRefetch(t *testing.T) {
	prevCu, prevClaude := cuFetchFunc, claudeSummarizeFunc
	t.Cleanup(func() { cuFetchFunc, claudeSummarizeFunc = prevCu, prevClaude })
	var calls int
	cuFetchFunc = func(ctx context.Context, id string) ([]byte, error) {
		calls++
		return []byte("raw"), nil
	}
	claudeSummarizeFunc = func(ctx context.Context, instruction string, input []byte) ([]byte, error) {
		return []byte("summary"), nil
	}
	ticketCache = newSummaryCache(time.Hour, 15*time.Second, 20*time.Second, 64) // fresh cache, isolated from other tests
	fetchTicketSummaryCached(context.Background(), "DR-99")
	fetchTicketSummaryCached(context.Background(), "DR-99")
	if calls != 1 {
		t.Errorf("cu fetch called %d times, want 1", calls)
	}
}
```

Add `"time"` to `info_ticket_test.go`'s imports if not already present.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./... -run TestFetchTicketSummaryCachedAvoidsRefetch -v`
Expected: FAIL — `fetchTicketSummaryCached`/`ticketCache` undefined.

- [ ] **Step 3: Write the implementation**

```go
// info_ticket.go — append
import "time"

const (
	ticketCacheTTL         = time.Hour // ticket text rarely changes turn to turn
	ticketCacheFailTTL     = 15 * time.Second
	ticketCacheFetchTimeout = 20 * time.Second
	ticketCacheMax         = 64
)

var ticketCache = newSummaryCache(ticketCacheTTL, ticketCacheFailTTL, ticketCacheFetchTimeout, ticketCacheMax)

// fetchTicketSummaryCached wraps fetchTicketSummary in ticketCache, keyed by
// ticket id — the whole cu+claude pipeline is cached as one unit, since
// there's no cheap separate "has this ticket changed" check worth doing
// before deciding to refetch.
func fetchTicketSummaryCached(ctx context.Context, ticketID string) (PreviewResult, error) {
	return ticketCache.getOrFetch(ctx, ticketID, func(fetchCtx context.Context) (PreviewResult, error) {
		return fetchTicketSummary(fetchCtx, ticketID)
	})
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./... -run TestFetchTicketSummaryCachedAvoidsRefetch -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add info_ticket.go info_ticket_test.go
git commit -m "feat: cache the ticket summary pipeline by ticket id"
```

---

### Task 7: Conversation summarization + local pipeline

**Files:**
- Modify: `info_transcript.go` (append to the file created in Task 4)
- Test: `info_transcript_test.go` (append)

**Interfaces:**
- Consumes: `claudeSummarizeFunc` (Task 2), `extractConversationTail` (Task 4), `newSummaryCache`/`getOrFetch` (Task 5), `findTranscript(home, sid string) string` (model.go:22-55, already exists).
- Produces: `func summarizeTurns(ctx context.Context, turns []transcriptTurn) (PreviewResult, error)`, `func conversationCacheKey(host, sessionID string, mtime time.Time, size int64) string`, `func fetchConversationSummaryLocal(ctx context.Context, home, sessionID string) (PreviewResult, error)` — consumed by Task 8 (`fetchConversationSummaryRemote` reuses `summarizeTurns`/`conversationCacheKey`) and Task 10 (`showInfoDialog`).

- [ ] **Step 1: Write the failing tests**

```go
// info_transcript_test.go — append
func TestSummarizeTurnsEmpty(t *testing.T) {
	got, err := summarizeTurns(context.Background(), nil)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got.Content == "" {
		t.Error("want a non-empty placeholder for zero turns")
	}
}

func TestSummarizeTurnsCallsClaude(t *testing.T) {
	prev := claudeSummarizeFunc
	t.Cleanup(func() { claudeSummarizeFunc = prev })
	claudeSummarizeFunc = func(ctx context.Context, instruction string, input []byte) ([]byte, error) {
		if instruction != conversationSummaryInstruction {
			t.Errorf("instruction = %q", instruction)
		}
		if !strings.Contains(string(input), "hello") {
			t.Errorf("input = %q, want it to contain the turn text", input)
		}
		return []byte(" summary text "), nil
	}
	got, err := summarizeTurns(context.Background(), []transcriptTurn{{Role: "user", Text: "hello"}})
	if err != nil || got.Content != "summary text" {
		t.Errorf("got (%+v, %v)", got, err)
	}
}

func TestSummarizeTurnsClaudeFails(t *testing.T) {
	prev := claudeSummarizeFunc
	t.Cleanup(func() { claudeSummarizeFunc = prev })
	claudeSummarizeFunc = func(ctx context.Context, instruction string, input []byte) ([]byte, error) {
		return nil, errors.New("timeout")
	}
	_, err := summarizeTurns(context.Background(), []transcriptTurn{{Role: "user", Text: "hi"}})
	if err == nil {
		t.Fatal("want error propagated when claude fails")
	}
}

func TestConversationCacheKeyDiffersOnMtime(t *testing.T) {
	t1 := time.Now()
	t2 := t1.Add(time.Second)
	k1 := conversationCacheKey("", "sid", t1, 100)
	k2 := conversationCacheKey("", "sid", t2, 100)
	if k1 == k2 {
		t.Error("keys should differ when mtime differs")
	}
}

func TestFetchConversationSummaryLocalNoTranscript(t *testing.T) {
	home := t.TempDir()
	_, err := fetchConversationSummaryLocal(context.Background(), home, "no-such-session")
	if err == nil {
		t.Fatal("want error when no transcript is found")
	}
}

func TestFetchConversationSummaryLocalSuccess(t *testing.T) {
	prev := claudeSummarizeFunc
	t.Cleanup(func() { claudeSummarizeFunc = prev })
	claudeSummarizeFunc = func(ctx context.Context, instruction string, input []byte) ([]byte, error) {
		return []byte("what's happening"), nil
	}
	home := t.TempDir()
	dir := filepath.Join(home, ".claude", "projects", "proj1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	sid := "sess-local-1"
	if err := os.WriteFile(filepath.Join(dir, sid+".jsonl"),
		[]byte(`{"type":"user","message":{"role":"user","content":"hi"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	conversationCache = newSummaryCache(time.Hour, 15*time.Second, 20*time.Second, 256) // fresh cache, isolated from other tests
	got, err := fetchConversationSummaryLocal(context.Background(), home, sid)
	if err != nil || got.Content != "what's happening" {
		t.Errorf("got (%+v, %v)", got, err)
	}
}
```

Add `"errors"`, `"os"`, `"path/filepath"`, `"strings"`, `"time"` to `info_transcript_test.go`'s imports as needed.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./... -run 'TestSummarizeTurns|TestConversationCacheKey|TestFetchConversationSummaryLocal' -v`
Expected: FAIL — the new functions/vars are undefined.

- [ ] **Step 3: Write the implementation**

```go
// info_transcript.go — append
import (
	"context"
	"fmt"
	"time"
)

const (
	conversationSummaryInstruction  = "what's happening in this conversation right now? short version like i am 25"
	conversationTailTurns           = 5
	conversationTurnCap             = 1500 // chars kept per turn before piping
	conversationPromptCap           = 6000 // total chars fed to claude -p
	conversationCacheTTL            = 24 * time.Hour // safety-net upper bound; a content change already changes the cache key via mtime/size
	conversationCacheFailTTL        = 15 * time.Second
	conversationCacheFetchTimeout   = 20 * time.Second
	conversationCacheMax            = 256
)

var conversationCache = newSummaryCache(conversationCacheTTL, conversationCacheFailTTL, conversationCacheFetchTimeout, conversationCacheMax)

var errTranscriptNotFound = fmt.Errorf("transcript not found")

func formatTurnsForPrompt(turns []transcriptTurn) string {
	var b strings.Builder
	for _, t := range turns {
		fmt.Fprintf(&b, "%s: %s\n\n", t.Role, trunc(t.Text, conversationTurnCap))
	}
	s := b.String()
	if len(s) > conversationPromptCap {
		s = s[:conversationPromptCap]
	}
	return s
}

// summarizeTurns pipes turns into claude -p to produce a short summary.
// Shared by the local and remote conversation pipelines (Task 8) — neither
// cares how the turns were obtained.
func summarizeTurns(ctx context.Context, turns []transcriptTurn) (PreviewResult, error) {
	if len(turns) == 0 {
		return PreviewResult{Source: "conversation", Content: "(no conversation turns yet)"}, nil
	}
	input := formatTurnsForPrompt(turns)
	summary, err := claudeSummarizeFunc(ctx, conversationSummaryInstruction, []byte(input))
	if err != nil {
		return PreviewResult{}, fmt.Errorf("summarize conversation: %w", err)
	}
	return PreviewResult{Source: "conversation", Content: strings.TrimSpace(string(summary))}, nil
}

// conversationCacheKey identifies one revision of a session's transcript.
// host is "" for local sessions, the remote server name otherwise — without
// it, the same session id on two different hosts (unusual, but possible for
// a resumed transcript) would collide.
func conversationCacheKey(host, sessionID string, mtime time.Time, size int64) string {
	return host + "\x00" + sessionID + "\x00" + mtime.UTC().Format(time.RFC3339Nano) + "\x00" + fmt.Sprint(size)
}

// fetchConversationSummaryLocal reads the session's own transcript directly
// and summarizes it. The raw read (stat + extractConversationTail) happens
// on every call; only the expensive claude -p step is cached, keyed by the
// transcript's (mtime, size) so a new message invalidates the cache
// automatically.
func fetchConversationSummaryLocal(ctx context.Context, home, sessionID string) (PreviewResult, error) {
	path := findTranscript(home, sessionID)
	if path == "" {
		return PreviewResult{}, errTranscriptNotFound
	}
	st, err := os.Stat(path)
	if err != nil {
		return PreviewResult{}, fmt.Errorf("stat transcript: %w", err)
	}
	key := conversationCacheKey("", sessionID, st.ModTime(), st.Size())
	return conversationCache.getOrFetch(ctx, key, func(fetchCtx context.Context) (PreviewResult, error) {
		turns, err := extractConversationTail(path, conversationTailTurns)
		if err != nil {
			return PreviewResult{}, fmt.Errorf("read transcript: %w", err)
		}
		return summarizeTurns(fetchCtx, turns)
	})
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./... -run 'TestSummarizeTurns|TestConversationCacheKey|TestFetchConversationSummaryLocal' -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add info_transcript.go info_transcript_test.go
git commit -m "feat: local conversation summarization pipeline with cache"
```

---

### Task 8: Remote endpoint + remote pipeline

**Files:**
- Modify: `server.go` (add handler + route registration)
- Modify: `remote_actions.go` (add `fetchRemoteTranscriptTail`)
- Modify: `info_transcript.go` (add `fetchConversationSummaryRemote`)
- Test: `server_test.go` (append — handler test)
- Test: `remote_actions_test.go` (append — client fetch test)
- Test: `info_transcript_test.go` (append — remote pipeline test)

**Interfaces:**
- Consumes: `s.authed(r) bool` (server.go:505-507), `resumeSessionIDRe` (resume.go:369), `findTranscript` (model.go:22-55), `extractConversationTail` (Task 4), `writeJSON` (server.go:719-723), `LookupServer(name string) (ServerConfig, bool)` (remote.go:339), `errSessionEnded` (preview.go:18), `summarizeTurns`/`conversationCacheKey`/`conversationCache`/`conversationTailTurns` (Task 7).
- Produces: `type transcriptTailResponse struct{...}`, `func (s *server) transcriptTail(w http.ResponseWriter, r *http.Request)`, `func fetchRemoteTranscriptTail(host, sessionID string, n int) ([]transcriptTurn, time.Time, int64, error)`, `func fetchConversationSummaryRemote(ctx context.Context, host, sessionID string) (PreviewResult, error)` — the last one consumed by Task 10 (`showInfoDialog`).

- [ ] **Step 1: Write the failing server handler test**

```go
// server_test.go — append
func TestTranscriptTailHandler(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".claude", "projects", "proj1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	sid := "sess-remote-1"
	if err := os.WriteFile(filepath.Join(dir, sid+".jsonl"),
		[]byte(`{"type":"user","message":{"role":"user","content":"remote hi"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	s := &server{token: "test-token"}
	req := httptest.NewRequest("GET", "/transcript-tail?session_id="+sid+"&n=5", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	s.transcriptTail(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body transcriptTailResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Turns) != 1 || body.Turns[0].Text != "remote hi" {
		t.Errorf("Turns = %+v", body.Turns)
	}
	if body.Size == 0 {
		t.Error("Size should be non-zero")
	}
}

func TestTranscriptTailHandlerBadSessionID(t *testing.T) {
	s := &server{token: "test-token"}
	req := httptest.NewRequest("GET", "/transcript-tail?session_id=../../etc/passwd&n=5", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	s.transcriptTail(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestTranscriptTailHandlerNClamped(t *testing.T) {
	s := &server{token: "test-token"}
	req := httptest.NewRequest("GET", "/transcript-tail?session_id=abc&n=999", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	s.transcriptTail(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for out-of-range n", rec.Code)
	}
}

func TestTranscriptTailHandlerNotFound(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	s := &server{token: "test-token"}
	req := httptest.NewRequest("GET", "/transcript-tail?session_id=nope&n=5", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	s.transcriptTail(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestTranscriptTailHandlerUnauthorized(t *testing.T) {
	s := &server{token: "test-token"}
	req := httptest.NewRequest("GET", "/transcript-tail?session_id=abc&n=5", nil)
	rec := httptest.NewRecorder()
	s.transcriptTail(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}
```

Check `server_test.go`'s existing imports and add any of `"encoding/json"`, `"net/http"`, `"net/http/httptest"`, `"os"`, `"path/filepath"` that aren't already there.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./... -run TestTranscriptTailHandler -v`
Expected: FAIL — `s.transcriptTail`/`transcriptTailResponse` undefined.

- [ ] **Step 3: Write the server handler**

Read `server.go` around line 995-1027 (the `preview` handler) first, to place the new handler near it and match its style, then add:

```go
// server.go — add near the preview/resumable handlers
type transcriptTailResponse struct {
	Turns      []transcriptTurn `json:"turns"`
	ModifiedAt time.Time        `json:"modifiedAt"`
	Size       int64            `json:"size"`
}

// transcriptTail serves the raw last-n user/assistant turns of a session's
// transcript, for the info dialog's remote conversation pipeline.
// Summarization never happens here — only on the client, so remote hosts
// never need `claude`/`cu` installed and never spend their own tokens.
func (s *server) transcriptTail(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	sessionID := r.URL.Query().Get("session_id")
	if !resumeSessionIDRe.MatchString(sessionID) {
		http.Error(w, "bad session_id", http.StatusBadRequest)
		return
	}
	n := 5
	if v := r.URL.Query().Get("n"); v != "" {
		parsed, err := strconv.Atoi(v)
		if err != nil || parsed < 1 || parsed > 10 {
			http.Error(w, "bad n value: "+v, http.StatusBadRequest)
			return
		}
		n = parsed
	}
	home, err := os.UserHomeDir()
	if err != nil {
		http.Error(w, "no home dir", http.StatusInternalServerError)
		return
	}
	path := findTranscript(home, sessionID)
	if path == "" {
		http.Error(w, "transcript not found", http.StatusNotFound)
		return
	}
	st, err := os.Stat(path)
	if err != nil {
		http.Error(w, "transcript not found", http.StatusNotFound)
		return
	}
	turns, err := extractConversationTail(path, n)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, transcriptTailResponse{
		Turns:      turns,
		ModifiedAt: st.ModTime(),
		Size:       st.Size(),
	})
}
```

Then add the route registration next to the other `mux.HandleFunc` lines (server.go:1840-1861 area):

```go
mux.HandleFunc("GET /transcript-tail", s.transcriptTail)
```

- [ ] **Step 4: Run server tests to verify they pass**

Run: `go test ./... -run TestTranscriptTailHandler -v`
Expected: PASS

- [ ] **Step 5: Write the failing client fetch test**

```go
// remote_actions_test.go — append
func TestFetchRemoteTranscriptTail(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Query().Get("session_id") != "sid1" || r.URL.Query().Get("n") != "5" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(transcriptTailResponse{
			Turns:      []transcriptTurn{{Role: "user", Text: "hi"}},
			ModifiedAt: time.Unix(1000, 0).UTC(),
			Size:       42,
		})
	}))
	defer backend.Close()

	u, _ := url.Parse(backend.URL)
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeServerYAML(t, home, "test-host", u.Hostname(), u.Port(), "secret")

	turns, mtime, size, err := fetchRemoteTranscriptTail("test-host", "sid1", 5)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(turns) != 1 || turns[0].Text != "hi" {
		t.Errorf("Turns = %+v", turns)
	}
	if size != 42 {
		t.Errorf("Size = %d, want 42", size)
	}
	if !mtime.Equal(time.Unix(1000, 0).UTC()) {
		t.Errorf("ModifiedAt = %v", mtime)
	}
}

func TestFetchRemoteTranscriptTailUnknownServer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	_, _, _, err := fetchRemoteTranscriptTail("no-such-server", "sid1", 5)
	if err == nil {
		t.Fatal("want error for unknown server")
	}
}
```

`writeServerYAML(t *testing.T, home, name, host, port, token string)` already exists (server_test.go:290-301) — it writes a `servers.yaml` `LookupServer` reads via `HOME`. This is the file's own established pattern (see `TestFetchRemotePreviewSanitizesBody`, remote_actions_test.go:17-37) — `url`, `time`, `encoding/json`, `net/http`, `net/http/httptest` are already imported in this file per its existing header (remote_actions_test.go:1-12); add `"time"` if it isn't already there.

- [ ] **Step 6: Run test to verify it fails**

Run: `go test ./... -run TestFetchRemoteTranscriptTail -v`
Expected: FAIL — `fetchRemoteTranscriptTail` undefined.

- [ ] **Step 7: Write the client fetch implementation**

Read `remote_actions.go:136-170` (`fetchRemotePreview`) first — this is copied almost verbatim, swapping the endpoint shape and response type:

```go
// remote_actions.go — append, and add "net/url" to the import block
const transcriptTailMaxBytes = 256 * 1024

// fetchRemoteTranscriptTail retrieves the raw last-n conversation turns from
// the named server, modeled directly on fetchRemotePreview. A 404 (no
// transcript for that session) maps to errTranscriptNotFound.
func fetchRemoteTranscriptTail(host, sessionID string, n int) ([]transcriptTurn, time.Time, int64, error) {
	srv, ok := LookupServer(host)
	if !ok {
		return nil, time.Time{}, 0, fmt.Errorf("unknown server: %s", host)
	}
	endpoint := fmt.Sprintf("http://%s:%d/transcript-tail?session_id=%s&n=%d",
		srv.Host, srv.Port, url.QueryEscape(sessionID), n)
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, time.Time{}, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+srv.Token)
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, time.Time{}, 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, time.Time{}, 0, errTranscriptNotFound
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, transcriptTailMaxBytes+1))
	if err != nil {
		return nil, time.Time{}, 0, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, time.Time{}, 0, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if len(data) > transcriptTailMaxBytes {
		return nil, time.Time{}, 0, fmt.Errorf("transcript-tail response exceeds %d bytes", transcriptTailMaxBytes)
	}
	var body transcriptTailResponse
	if err := json.Unmarshal(data, &body); err != nil {
		return nil, time.Time{}, 0, fmt.Errorf("bad response: %w", err)
	}
	return body.Turns, body.ModifiedAt, body.Size, nil
}
```

- [ ] **Step 8: Run test to verify it passes**

Run: `go test ./... -run TestFetchRemoteTranscriptTail -v`
Expected: PASS

- [ ] **Step 9: Write the failing remote pipeline test**

```go
// info_transcript_test.go — append
func TestFetchConversationSummaryRemote(t *testing.T) {
	prevClaude := claudeSummarizeFunc
	t.Cleanup(func() { claudeSummarizeFunc = prevClaude })
	claudeSummarizeFunc = func(ctx context.Context, instruction string, input []byte) ([]byte, error) {
		return []byte("remote summary"), nil
	}

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(transcriptTailResponse{
			Turns:      []transcriptTurn{{Role: "user", Text: "hi"}},
			ModifiedAt: time.Unix(2000, 0).UTC(),
			Size:       10,
		})
	}))
	defer backend.Close()

	u, _ := url.Parse(backend.URL)
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeServerYAML(t, home, "remote-host", u.Hostname(), u.Port(), "secret")

	conversationCache = newSummaryCache(time.Hour, 15*time.Second, 20*time.Second, 256) // fresh cache
	got, err := fetchConversationSummaryRemote(context.Background(), "remote-host", "sid1")
	if err != nil || got.Content != "remote summary" {
		t.Errorf("got (%+v, %v)", got, err)
	}
}
```

Same `writeServerYAML` pattern as Step 5 above. Add `"encoding/json"`, `"net/http"`, `"net/http/httptest"`, `"net/url"` to `info_transcript_test.go`'s imports as needed.

- [ ] **Step 10: Run test to verify it fails**

Run: `go test ./... -run TestFetchConversationSummaryRemote -v`
Expected: FAIL — `fetchConversationSummaryRemote` undefined.

- [ ] **Step 11: Write the remote pipeline implementation**

```go
// info_transcript.go — append
// fetchConversationSummaryRemote fetches raw turns from host's server and
// summarizes them locally — summarization never runs on the remote host.
func fetchConversationSummaryRemote(ctx context.Context, host, sessionID string) (PreviewResult, error) {
	turns, mtime, size, err := fetchRemoteTranscriptTail(host, sessionID, conversationTailTurns)
	if err != nil {
		return PreviewResult{}, fmt.Errorf("fetch remote transcript: %w", err)
	}
	key := conversationCacheKey(host, sessionID, mtime, size)
	return conversationCache.getOrFetch(ctx, key, func(fetchCtx context.Context) (PreviewResult, error) {
		return summarizeTurns(fetchCtx, turns)
	})
}
```

- [ ] **Step 12: Run test to verify it passes**

Run: `go test ./... -run TestFetchConversationSummaryRemote -v`
Expected: PASS

- [ ] **Step 13: Run the full test suite**

Run: `go test ./...`
Expected: PASS

- [ ] **Step 14: Commit**

```bash
git add server.go remote_actions.go info_transcript.go server_test.go remote_actions_test.go info_transcript_test.go
git commit -m "feat: remote transcript-tail endpoint and client pipeline"
```

---

### Task 9: asyncSection + wake merging

**Files:**
- Create: `info_async.go`
- Test: `info_async_test.go`

**Interfaces:**
- Consumes: `previewPane`, `startPreviewPane`, `previewFetch` (kill_preview.go:101,109-152), `modalWakesWith` (confirm_overlay.go:189-201), `wakeFD` (tui_events.go:341-346), `PreviewResult` (preview.go:42-50).
- Produces: `const infoDialogTimeout`, `type asyncSection struct{...}`, `func startAsyncSection(title string, run func(context.Context) (PreviewResult, error)) *asyncSection`, `func (a *asyncSection) close()`, `func (a *asyncSection) pane() *previewPane` (nil-safe accessor, added in fix round 1 — see the post-implementation note at the end of this task), `func modalWakesWithAll(wakes []wakeFD, sections ...*asyncSection) []wakeFD` — all consumed by Task 10/11.

- [ ] **Step 1: Write the failing tests**

```go
// info_async_test.go
package main

import (
	"context"
	"testing"
	"time"
)

func TestAsyncSectionDeliversResult(t *testing.T) {
	sec := startAsyncSection("t", func(ctx context.Context) (PreviewResult, error) {
		return PreviewResult{Content: "done"}, nil
	})
	defer sec.close()
	deadline := time.After(time.Second)
	for {
		snap := sec.snapshot()
		if snap.Loaded {
			// startPreviewPane splits PreviewResult.Content into
			// overlayPreview.Lines (kill_preview.go:141-143,
			// strings.Split(res.Content, "\n")) — a one-line result is
			// Lines == []string{"done"}.
			if len(snap.Lines) != 1 || snap.Lines[0] != "done" {
				t.Errorf("Lines = %+v, want [\"done\"]", snap.Lines)
			}
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for fetch to land")
		case <-time.After(time.Millisecond):
		}
	}
}

func TestAsyncSectionCloseCancelsContext(t *testing.T) {
	ctxCh := make(chan context.Context, 1)
	sec := startAsyncSection("t", func(ctx context.Context) (PreviewResult, error) {
		ctxCh <- ctx
		<-ctx.Done()
		return PreviewResult{}, ctx.Err()
	})
	ctx := <-ctxCh
	sec.close()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("context was not canceled by close()")
	}
}

func TestAsyncSectionNilIsSafe(t *testing.T) {
	var sec *asyncSection
	sec.close() // must not panic
	snap := sec.snapshot()
	if snap.Loaded {
		t.Error("nil section snapshot should not report Loaded")
	}
}

func TestModalWakesWithAllChaining(t *testing.T) {
	sec1 := startAsyncSection("a", func(ctx context.Context) (PreviewResult, error) {
		<-ctx.Done()
		return PreviewResult{}, ctx.Err()
	})
	sec2 := startAsyncSection("b", func(ctx context.Context) (PreviewResult, error) {
		<-ctx.Done()
		return PreviewResult{}, ctx.Err()
	})
	defer sec1.close()
	defer sec2.close()
	base := []wakeFD{{fd: -1, kind: wakeResize}}
	merged := modalWakesWithAll(base, sec1.previewPane, sec2.previewPane)
	if len(merged) != 3 {
		t.Fatalf("got %d wake fds, want 3 (1 base + 2 panes)", len(merged))
	}
	if len(base) != 1 {
		t.Errorf("modalWakesWithAll must not mutate the caller's base slice, got len %d", len(base))
	}
}
```

Fix the first test's stray comment block before running — it references a nonexistent `Content()` method; replace the assertion with a check against `snap.Lines` (a `[]string`, per `overlayPreview`'s actual shape at kill_preview.go:17-23): assert `len(snap.Lines) > 0 && snap.Lines[0] == "done"`.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./... -run 'TestAsyncSection|TestModalWakesWithAll' -v`
Expected: FAIL — `startAsyncSection`/`modalWakesWithAll` undefined.

- [ ] **Step 3: Write the implementation**

```go
// info_async.go
package main

import (
	"context"
	"time"
)

// infoDialogTimeout bounds a single fetch pipeline (cu+claude, or
// transcript-read/fetch+claude) even if the dialog is never closed —
// e.g. a wedged subprocess whose pipe close() can't force shut immediately
// (see info_exec.go's subprocessWaitDelay doc comment for why close() alone
// isn't a hard guarantee).
const infoDialogTimeout = 20 * time.Second

// asyncSection wraps previewPane (kill_preview.go) — reused for its
// wake-pipe + snapshot mechanics, not its rendering (previewBlock/
// renderConfirmOverlay are single-block and preview-specific; the info
// dialog's own renderer is new, see info_dialog.go) — with an owned
// context.CancelFunc, so closing the dialog actually signals the
// underlying subprocess(es) to stop instead of just tearing down the wake
// pipe. close() is idempotent and nil-receiver-safe, matching previewPane's
// own contract, so callers never need to nil-check before calling it.
type asyncSection struct {
	*previewPane
	cancel context.CancelFunc
}

func startAsyncSection(title string, run func(ctx context.Context) (PreviewResult, error)) *asyncSection {
	ctx, cancel := context.WithTimeout(context.Background(), infoDialogTimeout)
	pane := startPreviewPane(title, func() (PreviewResult, error) { return run(ctx) })
	return &asyncSection{previewPane: pane, cancel: cancel}
}

func (a *asyncSection) close() {
	if a == nil {
		return
	}
	a.cancel()
	a.previewPane.close()
}

// modalWakesWithAll merges the wake fds of multiple preview panes onto
// wakes, for a modal (like the info dialog) with more than one asyncSection.
// Safe to chain: each call to modalWakesWith allocates a fresh backing
// array (len+1 capacity) before appending, so it never mutates a shared
// slice regardless of which of its two branches ran — see
// confirm_overlay.go:189-201.
func modalWakesWithAll(wakes []wakeFD, panes ...*previewPane) []wakeFD {
	for _, p := range panes {
		wakes = modalWakesWith(wakes, p)
	}
	return wakes
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./... -run 'TestAsyncSection|TestModalWakesWithAll' -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add info_async.go info_async_test.go
git commit -m "feat: asyncSection wraps previewPane with cancel-on-close"
```

**Post-implementation correction (fix round 1, commit fa0aef9):** review found that `modalWakesWithAll(wakes []wakeFD, panes ...*previewPane)` forced every call site to extract `.previewPane` from a possibly-nil `*asyncSection` — panicking before the function's own nil-safety could help, defeating the whole point for Task 11's nil ticket section. Final signature:

```go
func (a *asyncSection) pane() *previewPane {
	if a == nil {
		return nil
	}
	return a.previewPane
}

func modalWakesWithAll(wakes []wakeFD, sections ...*asyncSection) []wakeFD {
	for _, sec := range sections {
		wakes = modalWakesWith(wakes, sec.pane())
	}
	return wakes
}
```

Callers now pass `*asyncSection` values directly — `modalWakesWithAll(wakes, ticketSec, convoSec)` — with no per-call-site nil check, even when `ticketSec` is nil. `close()` also gained a nil guard on `a.cancel` for hand-built values. Task 11's usage below reflects this final signature.

---

### Task 10: Header assembly and rendering

**Files:**
- Create: `info_dialog.go`
- Test: `info_dialog_test.go`

**Interfaces:**
- Consumes: `dirDisplay(cwd, home, gitRoot string) string` (render.go:1274-1279), `bold`/`dim` (render.go:49-50), `wrapRunes` (resume.go:1101-1135), `confirmBoxTL/TR/BL/BR/H/V` (confirm_overlay.go:53-60), `Session` struct (session.go:17-55), `overlayPreview` (kill_preview.go:17-23), `asyncSection` (Task 9).
- Produces: `func infoDialogHeader(s Session) []string`, `func renderInfoDialog(header []string, ticketSec, convoSec *asyncSection, cols, rows int) string` — consumed by Task 11 (`showInfoDialog`).

- [ ] **Step 1: Write the failing tests**

```go
// info_dialog_test.go
package main

import (
	"strings"
	"testing"
)

func TestInfoDialogHeaderUsesUpdatedAtWhenPresent(t *testing.T) {
	s := Session{Name: "my-session", CWD: "/tmp/nogit", StartedAt: 1000, UpdatedAt: 2000}
	lines := infoDialogHeader(s)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "my-session") {
		t.Errorf("header missing name: %v", lines)
	}
	if !strings.Contains(joined, "updated:") {
		t.Errorf("header missing 'updated:' label: %v", lines)
	}
}

func TestInfoDialogHeaderFallsBackToStartedAt(t *testing.T) {
	s := Session{Name: "s2", CWD: "/tmp/nogit", StartedAt: 1000, UpdatedAt: 0}
	lines := infoDialogHeader(s)
	if !strings.Contains(strings.Join(lines, "\n"), "updated:") {
		t.Errorf("want 'updated:' label even when falling back to StartedAt: %v", lines)
	}
}

func TestInfoDialogHeaderShowsHostOnlyWhenRemote(t *testing.T) {
	local := infoDialogHeader(Session{Name: "s", CWD: "/tmp", Host: ""})
	for _, l := range local {
		if strings.HasPrefix(l, "host:") {
			t.Errorf("local session header should not show a host line: %v", local)
		}
	}
	remote := infoDialogHeader(Session{Name: "s", CWD: "/tmp", Host: "myhost"})
	found := false
	for _, l := range remote {
		if l == "host: myhost" {
			found = true
		}
	}
	if !found {
		t.Errorf("remote session header should show its host: %v", remote)
	}
}

func TestRenderInfoDialogOmitsNilTicketSection(t *testing.T) {
	header := []string{"name", "dir"}
	convoSec := startAsyncSection("conversation", func(ctx context.Context) (PreviewResult, error) {
		return PreviewResult{Content: "convo summary"}, nil
	})
	defer convoSec.close()
	out := renderInfoDialog(header, nil, convoSec, 80, 24)
	if strings.Contains(out, "ticket") {
		t.Errorf("no ticket id -> no ticket section, got:\n%s", out)
	}
}

func TestRenderInfoDialogShowsLoadingBeforeFetchLands(t *testing.T) {
	header := []string{"name"}
	block := make(chan struct{})
	convoSec := startAsyncSection("conversation", func(ctx context.Context) (PreviewResult, error) {
		<-block
		return PreviewResult{Content: "done"}, nil
	})
	defer func() { close(block); convoSec.close() }()
	out := renderInfoDialog(header, nil, convoSec, 80, 24)
	if !strings.Contains(out, "loading") {
		t.Errorf("want a loading indicator before the fetch lands, got:\n%s", out)
	}
}
```

Add `"context"` to `info_dialog_test.go`'s imports.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./... -run 'TestInfoDialogHeader|TestRenderInfoDialog' -v`
Expected: FAIL — `infoDialogHeader`/`renderInfoDialog` undefined.

- [ ] **Step 3: Write the implementation**

Read `resume.go:992-1057` (`renderResumePromptsOverlay`) and `confirm_overlay.go:105` (`renderConfirmOverlay`'s box geometry) first, then write:

```go
// info_dialog.go
package main

import (
	"strconv"
	"strings"
	"time"
)

func infoDialogHeader(s Session) []string {
	lines := []string{
		bold(s.Name),
		dirDisplay(s.CWD, s.Home, s.GitRoot),
	}
	if s.Host != "" {
		lines = append(lines, "host: "+s.Host)
	}
	updated := s.StartedAt
	if s.UpdatedAt != 0 {
		updated = s.UpdatedAt
	}
	lines = append(lines, "updated: "+time.UnixMilli(updated).Format("2006-01-02 15:04"))
	return lines
}

const (
	infoDialogInnerWidth = 72 // default box width, clamped down on a narrow terminal
	infoDialogChrome     = 6  // top/bottom border + padding rows, mirrors resumePromptsChrome's role
)

// infoDialogSectionLines renders one section's current state: a loading
// placeholder, an error line, or its word-wrapped content. title is shown
// only when the section has something to say (loading/error/content) so an
// omitted (nil) section produces no output at the call site.
func infoDialogSectionLines(title string, sec *asyncSection, width int) []string {
	if sec == nil {
		return nil
	}
	snap := sec.snapshot()
	var out []string
	out = append(out, bold(title+":"))
	switch {
	case !snap.Loaded:
		out = append(out, dim("loading…"))
	case snap.Err != nil:
		out = append(out, dim("unavailable: "+snap.Err.Error()))
	case len(snap.Lines) == 0:
		out = append(out, dim("(empty)"))
	default:
		text := strings.Join(snap.Lines, "\n")
		for _, l := range wrapRunes(text, width) {
			out = append(out, l)
		}
	}
	return out
}

// renderInfoDialog draws the bordered info box: header, then an optional
// ticket section, then the conversation section, separated by a divider
// line. Modeled on renderResumePromptsOverlay's geometry (box glyphs,
// clamp-to-terminal-width, trailing "…" on overflow) but built fresh — the
// preview.go single-block renderer (previewBlock/renderConfirmOverlay)
// isn't built for multiple independently word-wrapped sections.
func renderInfoDialog(header []string, ticketSec, convoSec *asyncSection, cols, rows int) string {
	innerWidth := infoDialogInnerWidth
	if cols > 0 {
		max := cols - 4
		if max < 1 {
			max = 1
		}
		if innerWidth > max {
			innerWidth = max
		}
	}

	divider := strings.Repeat(confirmBoxH, innerWidth)
	var body []string
	body = append(body, header...)
	if ticketSec != nil {
		body = append(body, divider)
		body = append(body, infoDialogSectionLines("ticket", ticketSec, innerWidth)...)
	}
	body = append(body, divider)
	body = append(body, infoDialogSectionLines("conversation", convoSec, innerWidth)...)

	if rows > 0 {
		capacity := rows - infoDialogChrome
		if capacity < 1 {
			capacity = 1
		}
		if capacity < len(body) {
			body = body[:capacity]
			body[capacity-1] = dim("…")
		}
	}

	// pad mirrors renderConfirmOverlay's own row-padding idiom exactly
	// (confirm_overlay.go:140-142): visualLen skips ANSI escape sequences
	// when measuring width, so bold()/dim() content still right-pads
	// correctly instead of counting escape bytes as visible characters.
	pad := func(s string) string {
		s = clipLine(s, innerWidth)
		return confirmBoxV + " " + s + strings.Repeat(" ", innerWidth-visualLen(s)) + " " + confirmBoxV
	}

	var b strings.Builder
	b.WriteString(confirmBoxTL + strings.Repeat(confirmBoxH, innerWidth+2) + confirmBoxTR + "\n")
	for _, line := range body {
		b.WriteString(pad(line) + "\n")
	}
	b.WriteString(confirmBoxBL + strings.Repeat(confirmBoxH, innerWidth+2) + confirmBoxBR + "\n")
	return b.String()
}
```

`visualLen(s string) int` and `clipLine(s string, width int) string` already exist (render.go:2180, render.go:2199) — do not redefine them.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./... -run 'TestInfoDialogHeader|TestRenderInfoDialog' -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add info_dialog.go info_dialog_test.go
git commit -m "feat: info dialog header and bordered section renderer"
```

---

### Task 11: Modal loop and hotkey wiring

**Files:**
- Modify: `info_dialog.go` (append `showInfoDialog`)
- Modify: `tui.go` (add `case "i", "I":`)

**Interfaces:**
- Consumes: `detectTicketID` (Task 1), `fetchTicketSummaryCached` (Task 6), `fetchConversationSummaryLocal`/`fetchConversationSummaryRemote` (Task 7/8), `startAsyncSection`/`modalWakesWithAll` (Task 9), `renderInfoDialog`/`infoDialogHeader` (Task 10), `resumePromptsClose` (resume.go:979-985), `newScreenRenderer`, `newInputDecoder`, `readModalEvents`, `term.GetSize` (all already used identically by `resumePromptsOverlay`, resume.go:954-972), `actCtx` struct with its `modalWakes []wakeFD` field and `func (c *actCtx) selected() *Session` method (actions.go:19-57, 113-121), `makeCtx func() *actCtx` (the local closure built once in `RunTUI`, tui.go:375).
- Produces: `func showInfoDialog(s Session, wakes []wakeFD)` and `func actInfo(c *actCtx)` — the latter called from `tui.go`'s new `case "i", "I":`, matching the exact shape of the existing `case "k", "K":` → `actKill(makeCtx())` and `case "a", "A":` → `actAttach(makeCtx())` (tui.go:614-623).

- [ ] **Step 1: Write the implementation**

No new unit test for this task: `showInfoDialog` is an interactive terminal loop with the same shape as `resumePromptsOverlay`, which itself has no direct unit test in this codebase — its testable pieces (`infoDialogHeader`, `renderInfoDialog`, `detectTicketID`, every pipeline function) are already covered by Tasks 1–10. This task is verified by Task 12's build/vet/manual-smoke-test pass.

```go
// info_dialog.go — append
import (
	"context"
	"fmt"
	"os"

	"golang.org/x/term"
)

// showInfoDialog opens the 'i'-hotkey info modal for s: a deterministic
// header, an optional ticket summary (only if a DR-XXXX id is detected),
// and a conversation summary. Modeled directly on resumePromptsOverlay
// (resume.go:954-972) — same self-contained renderer/decoder/loop shape,
// same close-key set.
func showInfoDialog(s Session, wakes []wakeFD) {
	header := infoDialogHeader(s)
	ticketID := detectTicketID(s.CWD, s.Name)

	var ticketSec *asyncSection
	if ticketID != "" {
		ticketSec = startAsyncSection("ticket", func(ctx context.Context) (PreviewResult, error) {
			return fetchTicketSummaryCached(ctx, ticketID)
		})
	}

	convoSec := startAsyncSection("conversation", func(ctx context.Context) (PreviewResult, error) {
		if s.Host == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return PreviewResult{}, fmt.Errorf("resolve home dir: %w", err)
			}
			return fetchConversationSummaryLocal(ctx, home, s.SessionID)
		}
		return fetchConversationSummaryRemote(ctx, s.Host, s.SessionID)
	})

	defer func() {
		ticketSec.close() // nil-safe when no ticket id was detected
		convoSec.close()
	}()

	renderer := newScreenRenderer(os.Stdout)
	decoder := newInputDecoder()
	fd := int(os.Stdin.Fd())

	for {
		cols, rows, err := term.GetSize(fd)
		if err != nil {
			cols, rows = 0, 0
		}
		_ = renderer.Draw(renderInfoDialog(header, ticketSec, convoSec, cols, rows), cols, rows)
		// modalWakesWithAll takes *asyncSection directly (post-Task-9-fix-round
		// signature) — nil-safe, so ticketSec being nil here needs no guard.
		modalW := modalWakesWithAll(wakes, ticketSec, convoSec)
		keys, _ := readModalEvents(decoder, modalW)
		for _, key := range keys {
			if resumePromptsClose(key) {
				return
			}
		}
	}
}

// actInfo is the action-handler wrapper showInfoDialog needs to fit this
// codebase's established convention (actKill/actAttach/actResume all take
// *actCtx and resolve the selected row via c.selected() — see actions.go:
// 171-175, 247-251). A no-op when nothing is selected, same as those.
func actInfo(c *actCtx) {
	s := c.selected()
	if s == nil {
		return
	}
	showInfoDialog(*s, c.modalWakes)
}
```

- [ ] **Step 2: Wire the hotkey**

Read `tui.go` around the `case "k", "K":` / `case "a", "A":` block (tui.go:614-623) first to confirm it still matches, then add a new case in the same style — `screen.Invalidate()`, the action call, `render()` (no `refresh(true)`: unlike kill/attach, opening the info dialog doesn't change session state, so there's nothing new to re-collect):

```go
// tui.go — inside the main key switch, alongside case "k", "K": / case "a", "A":
case "i", "I":
	screen.Invalidate()
	actInfo(makeCtx())
	screen.Invalidate()
	render()
```

- [ ] **Step 3: Build and vet**

Run: `go build ./... && go vet ./...`
Expected: no errors

- [ ] **Step 4: Run the full test suite**

Run: `go test ./...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add info_dialog.go tui.go
git commit -m "feat: wire 'i' hotkey to the session info dialog"
```

---

### Task 12: Manual smoke test

**Files:** none (verification only)

**Interfaces:** none

- [ ] **Step 1: Full verification build**

Run: `go build ./... && go vet ./... && go test ./... -race`
Expected: all green

- [ ] **Step 2: Manual TUI check — local session, ticket + conversation**

```bash
make run
```
In the TUI: select a local session whose worktree or name contains a real `DR-XXXX` id (or start one via a worktree named that way), press `i`. Confirm:
- The header appears instantly (name, dir, updated time).
- The ticket section shows "loading…" then a short plain-English summary.
- The conversation section shows "loading…" then a short plain-English summary of the last few turns.
- Esc closes the dialog and returns to the table.

- [ ] **Step 3: Manual TUI check — no ticket id**

Select a session whose name/worktree has no `DR-XXXX`. Press `i`. Confirm the ticket section is entirely absent (no "unavailable" line, no empty section) and the conversation section still renders normally.

- [ ] **Step 4: Manual TUI check — cache hit**

Reopen the same session's dialog a second time within an hour. Confirm both sections load near-instantly (cache hit) rather than re-running `cu`/`claude`.

- [ ] **Step 5: Manual TUI check — remote session (if a configured remote host is available)**

Select a remote row, press `i`. Confirm the conversation section populates via the new `/transcript-tail` endpoint (check the remote host's `claude-sessions -s` logs show no `claude`/`cu` process spawned there) and the summary still renders correctly.

- [ ] **Step 6: Manual check — abandoned dialog doesn't leak**

Press `i`, then Esc immediately (before either section finishes loading). Run `ps aux | grep -E 'claude --model sonnet|cu fetch'` a few seconds later — confirm no orphaned process remains after `infoDialogTimeout` (20s) elapses at the latest.

- [ ] **Step 7: Report results**

If every check in Steps 2–6 passes, the feature is done. If any check fails, note exactly which one and the observed vs. expected behavior before treating this task as complete — do not mark it done on a partial pass.
