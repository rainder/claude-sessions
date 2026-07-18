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
