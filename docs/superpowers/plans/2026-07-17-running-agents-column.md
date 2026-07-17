# Running Agents Column Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Show per-session count of currently running subagents (AGENTS column) and a header grand total of concurrent agent loops (sessions + subagents), local + remote.

**Architecture:** Piggyback on the existing incremental cost scanner (`cost.go`): while parsing transcript lines for usage, also track `Agent` tool_use ids with no matching `tool_result` ("pending"). A new `scanSessionAgents` (new file `agents.go`) unions pending ids across the parent transcript and all `subagents/*.jsonl`, then counts `subagents/*.meta.json` entries whose `toolUseId` is still pending AND whose transcript mtime is within 5 minutes (crash-staleness guard). The count is an exported `Session` field, so the HTTP server's JSON and the remote client pick it up with zero API work. Rendering adds an AGENTS column (full + intermediate views, blank at zero) and a grand-total segment on the title line.

**Tech Stack:** Go stdlib only (existing constraint — no new dependencies).

**Spec:** `docs/superpowers/specs/2026-07-17-running-agents-column-design.md`

## Global Constraints

- Stdlib + existing `golang.org/x/{term,sys}` only; no new dependencies.
- Single package `main`; files split by concern.
- Freshness window: exactly `5 * time.Minute` (`agentFreshWindow`).
- Column blank (empty string, not "0" or "—") when count is zero.
- Header formats, copied verbatim from spec: `9 agents (4 sessions + 5 sub)`; zero-subagent degraded form `4 agents (4 sessions)`.
- The subagent-spawning tool is named `Agent` in transcripts (NOT `Task`).
- Verification for every task: `go test ./... && go vet ./...`.

## Verified on-disk facts (do not re-derive)

- Transcript line with a spawn: `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_01Qc...","name":"Agent",...}]}}`
- Completion line: `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_01Qc...",...}]}}`
- Meta file `<uuid>/subagents/agent-<hexid>.meta.json`: `{"agentType":"Explore","description":"...","toolUseId":"toolu_01Xj...","spawnDepth":1}`
- Nested subagents (spawnDepth 2+) live FLAT in the same `subagents/` dir; their spawning tool_use/tool_result pairs live in the spawning agent's own `agent-*.jsonl`. No recursive directory walk needed.
- Each subagent's transcript is `agent-<hexid>.jsonl`, sibling of its `.meta.json`.

---

### Task 1: Track pending Agent tool_use ids in the cost scanner

**Files:**
- Modify: `cost.go` (struct `costCacheEntry` ~line 113, `lineCost` ~line 82, `scanCostIncremental` reset sites ~line 141)
- Test: `cost_test.go` (update existing `lineCost` callers), `agents_test.go` (create)

**Interfaces:**
- Consumes: existing `costCacheEntry`, `lineCost`, `scanCostIncremental`.
- Produces: `costCacheEntry.agentPending map[string]bool` (unmatched Agent tool_use ids per file); `lineCost(line []byte, e *costCacheEntry) (float64, bool)` — NEW SIGNATURE (was `(line []byte, seen map[string]bool)`); helper `newCostCacheEntry() *costCacheEntry`. Task 2 reads `agentPending` via a `pendingAgents` accessor it defines.

- [ ] **Step 1: Write the failing test** — create `agents_test.go`:

```go
package main

import "testing"

// agentUseLine builds an assistant line spawning a subagent via the Agent tool.
func agentUseLine(toolUseID string) string {
	return `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"` + toolUseID + `","name":"Agent","input":{}}]}}`
}

// toolResultLine builds a user line completing a tool call.
func toolResultLine(toolUseID string) string {
	return `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"` + toolUseID + `","content":"done"}]}}`
}

func TestLineCostAgentPending(t *testing.T) {
	e := newCostCacheEntry()

	lineCost([]byte(agentUseLine("toolu_1")), e)
	lineCost([]byte(agentUseLine("toolu_2")), e)
	if len(e.agentPending) != 2 || !e.agentPending["toolu_1"] || !e.agentPending["toolu_2"] {
		t.Fatalf("after two spawns pending = %v, want {toolu_1, toolu_2}", e.agentPending)
	}

	lineCost([]byte(toolResultLine("toolu_1")), e)
	if len(e.agentPending) != 1 || e.agentPending["toolu_1"] {
		t.Fatalf("after result pending = %v, want {toolu_2}", e.agentPending)
	}

	// Non-Agent tool_use must not be tracked; its result is a no-op.
	lineCost([]byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_3","name":"Bash","input":{}}]}}`), e)
	lineCost([]byte(toolResultLine("toolu_3")), e)
	if len(e.agentPending) != 1 {
		t.Fatalf("after Bash call pending = %v, want {toolu_2}", e.agentPending)
	}

	// String-content user lines (normal user prompts) must not panic or match.
	lineCost([]byte(`{"type":"user","message":{"content":"hello"}}`), e)
	if len(e.agentPending) != 1 {
		t.Fatalf("after plain user line pending = %v, want {toolu_2}", e.agentPending)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./... -run TestLineCostAgentPending -v`
Expected: FAIL to compile — `newCostCacheEntry` undefined, `lineCost` signature mismatch.

- [ ] **Step 3: Implement in `cost.go`**

Extend the cache entry and add a constructor (replaces both inline `&costCacheEntry{seen: ...}` literals):

```go
// costCacheEntry holds the incremental scan state for one transcript file: the
// byte offset consumed so far, the running dollar cost, the dedup set of
// message.id+requestId keys already counted, and the set of Agent tool_use ids
// spawned in this file that have no tool_result yet (a subagent's spawn and its
// completion always land in the same transcript).
type costCacheEntry struct {
	offset       int64
	costUSD      float64
	seen         map[string]bool
	agentPending map[string]bool
}

func newCostCacheEntry() *costCacheEntry {
	return &costCacheEntry{seen: map[string]bool{}, agentPending: map[string]bool{}}
}
```

In `scanCostIncremental`, replace the reset branch:

```go
	e := costCache[path]
	if e == nil || st.Size() < e.offset {
		e = newCostCacheEntry()
		costCache[path] = e
	}
```

Rewrite `lineCost` to parse content blocks in the same unmarshal and take the entry:

```go
// lineCost prices a single transcript line, returning its dollar cost and
// whether it counted (a known-model assistant usage line not already seen).
// e.seen dedupes streaming re-emissions by message.id+requestId. As a side
// effect the line's content blocks maintain e.agentPending: an assistant
// tool_use named "Agent" adds its id; a user tool_result removes its
// tool_use_id (only Agent ids ever enter the set, so the delete is
// unconditional). User lines whose content is a plain string fail the
// unmarshal and are skipped, same as before.
func lineCost(line []byte, e *costCacheEntry) (cost float64, ok bool) {
	var ev struct {
		Type      string `json:"type"`
		RequestID string `json:"requestId"`
		Message   struct {
			ID      string     `json:"id"`
			Model   string     `json:"model"`
			Usage   *costUsage `json:"usage"`
			Content []struct {
				Type      string `json:"type"`
				ID        string `json:"id"`
				Name      string `json:"name"`
				ToolUseID string `json:"tool_use_id"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(line, &ev); err != nil {
		return 0, false
	}
	for _, b := range ev.Message.Content {
		switch {
		case ev.Type == "assistant" && b.Type == "tool_use" && b.Name == "Agent":
			e.agentPending[b.ID] = true
		case ev.Type == "user" && b.Type == "tool_result":
			delete(e.agentPending, b.ToolUseID)
		}
	}
	if ev.Type != "assistant" || ev.Message.Usage == nil {
		return 0, false
	}
	price, known := priceFor(ev.Message.Model)
	if !known {
		return 0, false
	}
	key := ev.Message.ID + "\x00" + ev.RequestID
	if e.seen[key] {
		return 0, false
	}
	e.seen[key] = true
	return usageCost(price, *ev.Message.Usage), true
}
```

Update the call site in `scanCostIncremental` from `lineCost(line, e.seen)` to `lineCost(line, e)`.

- [ ] **Step 4: Fix existing tests in `cost_test.go`**

`TestLineCostDedup` and `TestLineCostSkips` construct `seen := map[string]bool{}` and call `lineCost(line, seen)`. Change both to:

```go
	e := newCostCacheEntry()
	// ... every lineCost(x, seen) becomes lineCost(x, e)
```

- [ ] **Step 5: Run full suite**

Run: `go test ./... && go vet ./...`
Expected: all PASS (including untouched `TestScanCostIncremental` etc.).

- [ ] **Step 6: Commit**

```bash
git add cost.go cost_test.go agents_test.go
git commit -m "feat: track pending Agent tool_use ids in cost scanner"
```

---

### Task 2: scanSessionAgents — running-subagent count per session

**Files:**
- Create: `agents.go`
- Test: `agents_test.go` (extend)

**Interfaces:**
- Consumes: `scanCostIncremental(path string) float64`, `costCache`/`costCacheMu`, `costCacheEntry.agentPending` (Task 1).
- Produces: `scanSessionAgents(path string, now time.Time) int` and `const agentFreshWindow = 5 * time.Minute`. Task 3 calls `scanSessionAgents`.

- [ ] **Step 1: Write the failing tests** — append to `agents_test.go`:

```go
import (
	"os"
	"path/filepath"
	"testing"
	"time"
)
// (merge imports with the existing block)

// makeSubagent writes agent-<id>.meta.json and agent-<id>.jsonl under
// dir/subagents, returning the jsonl path.
func makeSubagent(t *testing.T, dir, id, toolUseID string, depth int, lines ...string) string {
	t.Helper()
	subs := filepath.Join(dir, "subagents")
	meta := `{"agentType":"scout","description":"d","toolUseId":"` + toolUseID + `","spawnDepth":` + fmt.Sprint(depth) + `}`
	if err := os.MkdirAll(subs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subs, "agent-"+id+".meta.json"), []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(subs, "agent-"+id+".jsonl")
	writeLines(t, p, lines...)
	return p
}

func TestScanSessionAgentsRunning(t *testing.T) {
	dir := t.TempDir()
	parent := filepath.Join(dir, "sess.jsonl")
	// Two spawns; toolu_a completed, toolu_b still pending.
	writeLines(t, parent,
		agentUseLine("toolu_a"),
		toolResultLine("toolu_a"),
		agentUseLine("toolu_b"),
	)
	makeSubagent(t, filepath.Join(dir, "sess"), "aaa", "toolu_a", 1, `{"type":"assistant"}`)
	makeSubagent(t, filepath.Join(dir, "sess"), "bbb", "toolu_b", 1, `{"type":"assistant"}`)

	if got := scanSessionAgents(parent, time.Now()); got != 1 {
		t.Errorf("running = %d, want 1 (toolu_b pending, toolu_a done)", got)
	}
}

func TestScanSessionAgentsNested(t *testing.T) {
	dir := t.TempDir()
	parent := filepath.Join(dir, "sess.jsonl")
	writeLines(t, parent, agentUseLine("toolu_p")) // parent spawns agent p, unfinished
	// Agent p itself spawned agent n (nested, depth 2), also unfinished; the
	// nested spawn's tool_use lives in p's own transcript.
	makeSubagent(t, filepath.Join(dir, "sess"), "ppp", "toolu_p", 1, agentUseLine("toolu_n"))
	makeSubagent(t, filepath.Join(dir, "sess"), "nnn", "toolu_n", 2, `{"type":"assistant"}`)

	if got := scanSessionAgents(parent, time.Now()); got != 2 {
		t.Errorf("running = %d, want 2 (direct + nested)", got)
	}
}

func TestScanSessionAgentsStale(t *testing.T) {
	dir := t.TempDir()
	parent := filepath.Join(dir, "sess.jsonl")
	writeLines(t, parent, agentUseLine("toolu_s")) // never completed
	p := makeSubagent(t, filepath.Join(dir, "sess"), "sss", "toolu_s", 1, `{"type":"assistant"}`)
	// Crash-stale: transcript last touched 10 minutes ago.
	old := time.Now().Add(-10 * time.Minute)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}

	if got := scanSessionAgents(parent, time.Now()); got != 0 {
		t.Errorf("running = %d, want 0 (unmatched but stale)", got)
	}
}

func TestScanSessionAgentsNoSubagents(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "sess.jsonl")
	writeLines(t, parent, `{"type":"assistant"}`)
	if got := scanSessionAgents(parent, time.Now()); got != 0 {
		t.Errorf("running = %d, want 0", got)
	}
}
```

Note: `writeLines` already exists in `cost_test.go` (same package). Add `"fmt"` to the import merge for `fmt.Sprint(depth)`.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./... -run TestScanSessionAgents -v`
Expected: FAIL to compile — `scanSessionAgents` undefined.

- [ ] **Step 3: Create `agents.go`**

```go
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// agentFreshWindow is how recently a subagent's transcript must have been
// written for an unmatched Agent tool_use to count as running. A live agent —
// even one blocked on a long tool call — touches its transcript far more often
// than this; a crashed or killed session leaves its unmatched tool_use behind
// with an mtime that goes hours stale, so the window filters phantoms without
// needing to be tight.
const agentFreshWindow = 5 * time.Minute

// pendingAgents merges the unmatched Agent tool_use ids recorded for path
// (by scanCostIncremental) into the given set.
func pendingAgents(path string, into map[string]bool) {
	costCacheMu.Lock()
	defer costCacheMu.Unlock()
	if e := costCache[path]; e != nil {
		for id := range e.agentPending {
			into[id] = true
		}
	}
}

// scanSessionAgents counts the session's currently running subagents,
// including nested subagents-of-subagents. A subagent is running iff its
// spawning Agent tool_use has no matching tool_result yet (per the pending
// sets the cost scanner maintains for the parent transcript and every
// subagents/*.jsonl — a nested agent's spawn pair lives in its parent agent's
// transcript, and all transcripts sit flat in the same subagents dir) AND its
// own transcript was written within agentFreshWindow of now. The
// scanCostIncremental calls are idempotent: when CollectLocal has already
// scanned for cost this tick they reduce to a stat.
func scanSessionAgents(path string, now time.Time) int {
	subDir := filepath.Join(strings.TrimSuffix(path, ".jsonl"), "subagents")
	metas, _ := filepath.Glob(filepath.Join(subDir, "*.meta.json"))
	if len(metas) == 0 {
		return 0
	}

	scanCostIncremental(path)
	pending := map[string]bool{}
	pendingAgents(path, pending)
	subs, _ := filepath.Glob(filepath.Join(subDir, "*.jsonl"))
	for _, f := range subs {
		scanCostIncremental(f)
		pendingAgents(f, pending)
	}

	n := 0
	for _, m := range metas {
		data, err := os.ReadFile(m)
		if err != nil {
			continue
		}
		var meta struct {
			ToolUseID string `json:"toolUseId"`
		}
		if json.Unmarshal(data, &meta) != nil || !pending[meta.ToolUseID] {
			continue
		}
		// Freshness comes from the agent's own transcript; a just-spawned
		// agent that hasn't written one yet falls back to the meta file.
		ref := strings.TrimSuffix(m, ".meta.json") + ".jsonl"
		st, err := os.Stat(ref)
		if err != nil {
			st, err = os.Stat(m)
		}
		if err == nil && now.Sub(st.ModTime()) <= agentFreshWindow {
			n++
		}
	}
	return n
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./... -run TestScanSessionAgents -v` then `go test ./... && go vet ./...`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add agents.go agents_test.go
git commit -m "feat: scanSessionAgents counts running subagents per session"
```

---

### Task 3: Session.AgentsRunning field + collection wiring

**Files:**
- Modify: `session.go` (struct ~line 38, `CollectLocal` ~line 150)
- Test: `agents_test.go` (extend)

**Interfaces:**
- Consumes: `scanSessionAgents` (Task 2).
- Produces: `Session.AgentsRunning int` with JSON tag `agentsRunning,omitempty`. Remote rows get it for free: the server marshals `[]Session` and the client unmarshals the same type — no server.go/remote.go change needed. Task 4 renders from this field.

- [ ] **Step 1: Write the failing test** — append to `agents_test.go`:

```go
func TestSessionAgentsRunningJSONRoundTrip(t *testing.T) {
	s := Session{PID: 1, AgentsRunning: 3}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"agentsRunning":3`) {
		t.Errorf("marshal missing agentsRunning: %s", b)
	}
	var back Session
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.AgentsRunning != 3 {
		t.Errorf("round-trip AgentsRunning = %d, want 3", back.AgentsRunning)
	}
	// omitempty: zero count stays out of the wire format.
	b, _ = json.Marshal(Session{PID: 1})
	if strings.Contains(string(b), "agentsRunning") {
		t.Errorf("zero count serialized: %s", b)
	}
}
```

Add `"encoding/json"` and `"strings"` to the agents_test.go import merge.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./... -run TestSessionAgentsRunningJSONRoundTrip -v`
Expected: FAIL to compile — `AgentsRunning` undefined.

- [ ] **Step 3: Implement** — in `session.go`, directly below `CostSubagentsUSD`:

```go
	// AgentsRunning is the number of currently running Task/Agent-tool
	// subagents (incl. nested), per scanSessionAgents. Computed at collection
	// time so remote rows render from the JSON as-is.
	AgentsRunning int `json:"agentsRunning,omitempty"`
```

In `CollectLocal`, inside the `if p := findTranscript(...)` block, after the `scanSessionCost` line:

```go
			s.CostUSD, s.CostSubagentsUSD = scanSessionCost(p)
			s.AgentsRunning = scanSessionAgents(p, time.Now())
```

- [ ] **Step 4: Run full suite**

Run: `go test ./... && go vet ./...`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add session.go agents_test.go
git commit -m "feat: collect running-subagent count into Session"
```

---

### Task 4: AGENTS column + header grand total

**Files:**
- Modify: `render.go` (`renderHeader` ~line 392, `drowFull`/`deriveFull` ~line 477, `renderAllFull` ~line 525, `renderAllIntermediate` ~line 637)
- Test: `render_test.go` (extend)

**Interfaces:**
- Consumes: `Session.AgentsRunning` (Task 3).
- Produces: user-visible output only. `formatAgents(n int) string` helper (blank at ≤0). Minimal view intentionally unchanged.

- [ ] **Step 1: Write the failing tests** — append to `render_test.go` (same package; use existing import set, add `"bytes"`/`"strings"` only if not present):

```go
func TestFormatAgents(t *testing.T) {
	if got := formatAgents(0); got != "" {
		t.Errorf("formatAgents(0) = %q, want empty", got)
	}
	if got := formatAgents(-1); got != "" {
		t.Errorf("formatAgents(-1) = %q, want empty", got)
	}
	if got := formatAgents(3); got != "3" {
		t.Errorf("formatAgents(3) = %q, want 3", got)
	}
}

func TestRenderAgentsColumnAndHeaderTotal(t *testing.T) {
	local := []Session{
		{PID: 100, SessionID: "aaaa", CWD: "/w1", Status: "busy", StartedAt: 1, AgentsRunning: 3},
		{PID: 200, SessionID: "bbbb", CWD: "/w2", Status: "idle", StartedAt: 2},
	}
	var buf bytes.Buffer
	RenderAll(&buf, "1", local, nil, "", nil, 0, 0, "dir")
	out := buf.String()

	if !strings.Contains(out, "AGENTS") {
		t.Errorf("full view missing AGENTS column header:\n%s", out)
	}
	// 2 sessions + 3 running subagents = 5 concurrent agent loops.
	if !strings.Contains(out, "5 agents (2 sessions + 3 sub)") {
		t.Errorf("header missing grand total:\n%s", out)
	}

	// Intermediate view carries the column too.
	buf.Reset()
	RenderAll(&buf, "3", local, nil, "", nil, 0, 0, "dir")
	if !strings.Contains(buf.String(), "AGENTS") {
		t.Errorf("intermediate view missing AGENTS column header")
	}

	// Minimal view: no column, but header total still present.
	buf.Reset()
	RenderAll(&buf, "2", local, nil, "", nil, 0, 0, "dir")
	out = buf.String()
	if strings.Contains(out, "AGENTS") {
		t.Errorf("minimal view must not have AGENTS column:\n%s", out)
	}
	if !strings.Contains(out, "5 agents (2 sessions + 3 sub)") {
		t.Errorf("minimal header missing grand total:\n%s", out)
	}
}

func TestRenderHeaderTotalNoSubagents(t *testing.T) {
	local := []Session{
		{PID: 100, SessionID: "aaaa", CWD: "/w1", Status: "idle", StartedAt: 1},
	}
	var buf bytes.Buffer
	RenderAll(&buf, "1", local, nil, "", nil, 0, 0, "dir")
	if !strings.Contains(buf.String(), "1 agents (1 sessions)") {
		t.Errorf("degraded zero-subagent form missing:\n%s", buf.String())
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./... -run 'TestFormatAgents|TestRenderAgents|TestRenderHeaderTotal' -v`
Expected: FAIL — `formatAgents` undefined.

- [ ] **Step 3: Implement in `render.go`**

Helper, next to `formatTokens`:

```go
// formatAgents renders a running-subagent count: blank at zero so idle rows
// stay quiet, plain digits otherwise.
func formatAgents(n int) string {
	if n <= 0 {
		return ""
	}
	return fmt.Sprintf("%d", n)
}
```

`renderHeader`: sum subagents in the existing counting loop and print the grand total before the `[mode]` tag. Replace the function body's loop and printf:

```go
	live, tmuxCount, busy, shell, subs := 0, 0, 0, 0, 0
	for _, sec := range sections {
		for _, s := range sec.rows {
			live++
			subs += s.AgentsRunning
			if s.Tmux != "" {
				tmuxCount++
			}
			switch s.Status {
			case "busy":
				busy++
			case "shell":
				shell++
			}
		}
	}
	// colorize ends with a full reset, so re-assert bold after each count to
	// keep the rest of the title line bold.
	busyStr := colorize(statusColor["busy"], fmt.Sprintf("%d busy", busy)) + ansiBold
	shellStr := colorize(statusColor["shell"], fmt.Sprintf("%d shell", shell)) + ansiBold
	// Grand total of concurrent agent loops: each live session is one, plus
	// every running subagent (incl. nested), across local and remote.
	agentsStr := fmt.Sprintf("%d agents (%d sessions)", live, live)
	if subs > 0 {
		agentsStr = fmt.Sprintf("%d agents (%d sessions + %d sub)", live+subs, live, subs)
	}
	fmt.Fprintf(w, "%sClaude sessions  %s  (%d live, %d in tmux, %s, %s)  %s  %s%s\n",
		ansiBold, time.Now().Format("15:04:05"), live, tmuxCount, busyStr, shellStr,
		agentsStr, ansiReset, dim("["+mode+"]"))
	writeUsage(w, usage, cols)
	fmt.Fprintln(w)
```

`drowFull` (used by full AND intermediate): add field + derive:

```go
	agentsStr string // running-subagent count, "" when zero
```

In `deriveFull`'s returned literal, after `costStr`:

```go
		agentsStr: formatAgents(s.AgentsRunning),
```

`renderAllFull`: add width var and measurement —

```go
	nameW, dirW, modelW, costW, agentsW, statusW, tmuxW := len("NAME"), utf8.RuneCountInString(dirLabel), len("MODEL"), len("COST"), len("AGENTS"), len("STATUS"), len("TMUX")
	for _, r := range all {
		// ...existing maxes...
		agentsW = max(agentsW, len(r.agentsStr))
	}
```

Header line — insert AGENTS between COST and CTX (right-aligned like COST):

```go
		return fmt.Sprintf("  %7s  %-*s  %-*s  %-*s  %-*s  %*s  %*s  %5s  %-*s  %5s  %5s  %-8s  %s",
			"PID", nameW, "NAME", dirW, dirLabel, modelW, "MODEL", statusW, "STATUS", costW, "COST", agentsW, "AGENTS", "CTX", tmuxW, "TMUX",
			"CPU%", ageLabel, "VER", "SID")
```

Row line — insert matching cell after `costCell(...)`:

```go
			row := fmt.Sprintf("%7d  %s  %s  %s  %s  %s  %*s  %s  %s  %5s  %5s  %-8s  %s",
				r.s.PID,
				nameCell,
				marqueeCell(r.cwdStr, dirW, step),
				modelCell(r.modelStr, modelW, ghost),
				statusCell,
				costCell(r.costStr, costW),
				agentsW, r.agentsStr,
				ctxCell(r.ctxStr, r.s.ContextTokens, ghost),
				tmuxCell,
				r.s.CPU, r.ageStr, r.s.Version, r.sidShort)
```

`renderAllIntermediate`: same three edits —

```go
	nameW, dirW, modelW, costW, agentsW, statusW := len("NAME"), utf8.RuneCountInString(dirLabel), len("MODEL"), len("COST"), len("AGENTS"), len("STATUS")
	// in the measuring loop:
		agentsW = max(agentsW, len(r.agentsStr))
	// header:
		return fmt.Sprintf("  %-*s  %-*s  %-*s  %-*s  %*s  %*s  %5s  %5s  %5s",
			nameW, "NAME", dirW, dirLabel, modelW, "MODEL", statusW, "STATUS", costW, "COST", agentsW, "AGENTS", "CTX", "CPU%", ageLabel)
	// row:
		row := fmt.Sprintf("%s  %s  %s  %s  %s  %*s  %s  %5s  %5s",
			nameCell,
			marqueeCell(r.cwdStr, dirW, step),
			modelCell(r.modelStr, modelW, ghost),
			statusCell,
			costCell(r.costStr, costW),
			agentsW, r.agentsStr,
			ctxCell(r.ctxStr, r.s.ContextTokens, ghost),
			r.s.CPU, r.ageStr)
```

Minimal view (`renderAllMinimal`, `drowMinimal`): NO changes — the header total comes from the shared `renderHeader`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./... && go vet ./...`
Expected: all PASS (existing render tests must survive the format-string changes; if a fixed-width assertion breaks, the assertion updates to include the AGENTS column — behavior, not test, is the source of truth here per the spec).

- [ ] **Step 5: Visual smoke check**

Run: `go build . && ./claude-sessions --once 2>/dev/null | head -20` (or `go run . --once`)
Expected: table shows AGENTS header between COST and CTX; header line ends `...N agents (N sessions)  [full]` (sub segment only if something is running agents right now).

- [ ] **Step 6: Commit**

```bash
git add render.go render_test.go
git commit -m "feat(tui): AGENTS column and header grand total of running agents"
```

---

## Self-review notes

- Spec coverage: detection hybrid (Task 1+2), 5-min window (Task 2), nested via flat dir + parent-agent transcript (Task 2 nested test), Session field + remote-for-free (Task 3), AGENTS column blank-at-zero in full+intermediate, hidden in minimal (Task 4), header grand total + degraded form (Task 4), out-of-scope items untouched.
- Type consistency: `lineCost(line []byte, e *costCacheEntry)` used in Tasks 1–2; `scanSessionAgents(path string, now time.Time) int` in Tasks 2–3; `AgentsRunning int` in Tasks 3–4; `formatAgents` only in Task 4.
- `--once` flag verified: `main.go:22` handles `-1`/`--once` (print local sessions and exit).
