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
