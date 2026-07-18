package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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

func TestLineCostAgentPendingReplay(t *testing.T) {
	e := newCostCacheEntry()
	// Streaming re-emission: the same spawn line appears twice.
	lineCost([]byte(agentUseLine("toolu_r")), e)
	lineCost([]byte(agentUseLine("toolu_r")), e)
	if len(e.agentPending) != 1 || !e.agentPending["toolu_r"] {
		t.Fatalf("after duplicate spawns pending = %v, want {toolu_r}", e.agentPending)
	}
	// The result replayed twice: second delete is a safe no-op.
	lineCost([]byte(toolResultLine("toolu_r")), e)
	lineCost([]byte(toolResultLine("toolu_r")), e)
	if len(e.agentPending) != 0 {
		t.Fatalf("after duplicate results pending = %v, want empty", e.agentPending)
	}
}

func TestScanSessionAgentsJustSpawned(t *testing.T) {
	dir := t.TempDir()
	parent := filepath.Join(dir, "sess.jsonl")
	writeLines(t, parent, agentUseLine("toolu_j")) // spawned, no result yet
	subs := filepath.Join(dir, "sess", "subagents")
	if err := os.MkdirAll(subs, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := `{"agentType":"scout","description":"d","toolUseId":"toolu_j","spawnDepth":1}`
	if err := os.WriteFile(filepath.Join(subs, "agent-jjj.meta.json"), []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}
	// No agent-jjj.jsonl: freshness falls back to the meta file's mtime.
	if got := scanSessionAgents(parent, time.Now()); got != 1 {
		t.Errorf("running = %d, want 1 (just-spawned, meta-mtime fallback)", got)
	}
}
