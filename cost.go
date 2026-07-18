package main

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// modelPricing is a per-family price sheet in dollars per million tokens
// (MTok). Cache writes carry two rates for the 5-minute and 1-hour TTLs.
type modelPricing struct {
	input        float64
	output       float64
	cacheRead    float64
	cacheWrite5m float64
	cacheWrite1h float64
}

// pricingTable maps a model-id family prefix to its price sheet ($/MTok),
// matched by prefix on the model id (see priceFor). The families are
// disjoint, so first-match is unambiguous. Kept in sync with Anthropic's
// published list pricing; unknown models contribute nothing to a row's cost.
var pricingTable = []struct {
	prefix string
	price  modelPricing
}{
	{"claude-fable-", modelPricing{10, 50, 1.00, 12.50, 20.00}},
	{"claude-mythos-", modelPricing{10, 50, 1.00, 12.50, 20.00}},
	{"claude-opus-", modelPricing{5, 25, 0.50, 6.25, 10.00}},
	{"claude-sonnet-", modelPricing{3, 15, 0.30, 3.75, 6.00}},
	{"claude-haiku-", modelPricing{1, 5, 0.10, 1.25, 2.00}},
}

// priceFor returns the price sheet for a model id and whether the family is
// known. Unknown models report ok=false so their tokens are ignored.
func priceFor(model string) (modelPricing, bool) {
	for _, e := range pricingTable {
		if strings.HasPrefix(model, e.prefix) {
			return e.price, true
		}
	}
	return modelPricing{}, false
}

// costUsage mirrors the message.usage fields that drive pricing. CacheCreation
// is a pointer so its absence (older transcript format) is distinguishable
// from an all-zero breakdown.
type costUsage struct {
	InputTokens         int `json:"input_tokens"`
	OutputTokens        int `json:"output_tokens"`
	CacheReadTokens     int `json:"cache_read_input_tokens"`
	CacheCreationTokens int `json:"cache_creation_input_tokens"`
	CacheCreation       *struct {
		Ephemeral5m int `json:"ephemeral_5m_input_tokens"`
		Ephemeral1h int `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
}

// usageCost prices one usage block in dollars. When the 5m/1h cache-write
// breakdown is present it is priced at the two rates separately; otherwise all
// cache-creation tokens fall back to the 5m rate.
func usageCost(p modelPricing, u costUsage) float64 {
	cost := float64(u.InputTokens)*p.input +
		float64(u.OutputTokens)*p.output +
		float64(u.CacheReadTokens)*p.cacheRead
	if u.CacheCreation != nil {
		cost += float64(u.CacheCreation.Ephemeral5m)*p.cacheWrite5m +
			float64(u.CacheCreation.Ephemeral1h)*p.cacheWrite1h
	} else {
		cost += float64(u.CacheCreationTokens) * p.cacheWrite5m
	}
	return cost / 1_000_000
}

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
		// The subagent-spawning tool is named "Agent" in current Claude Code
		// transcripts (earlier builds used "Task"). This match is coupled to
		// that name: if a future build renames the tool again, this silently
		// detects zero running agents instead of erroring.
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

// costCache and its mutex mirror metaCache's concurrency model: collection can
// run from multiple goroutines, so every access is guarded.
var (
	costCacheMu sync.Mutex
	costCache   = map[string]*costCacheEntry{}
)

// scanCostIncremental returns the cumulative dollar cost of one transcript
// file (parent or subagent), parsing only the bytes appended since the
// previous call. State is cached per path; a file smaller than the cached
// offset (truncation or rotation) resets the entry and forces a full rescan.
// Only complete newline-terminated lines advance the offset, so a partially
// written trailing line is re-read on the next tick. Returns 0 on any error.
func scanCostIncremental(path string) float64 {
	st, err := os.Stat(path)
	if err != nil {
		return 0
	}
	costCacheMu.Lock()
	defer costCacheMu.Unlock()

	e := costCache[path]
	if e == nil || st.Size() < e.offset {
		e = newCostCacheEntry()
		costCache[path] = e
	}
	if st.Size() == e.offset {
		return e.costUSD // nothing appended since last scan
	}

	f, err := os.Open(path)
	if err != nil {
		return e.costUSD
	}
	defer f.Close()
	if _, err := f.Seek(e.offset, io.SeekStart); err != nil {
		return e.costUSD
	}

	r := bufio.NewReaderSize(f, 64*1024)
	off := e.offset
	for {
		line, err := r.ReadBytes('\n')
		if n := len(line); n > 0 && line[n-1] == '\n' {
			off += int64(n)
			if c, ok := lineCost(line, e); ok {
				e.costUSD += c
			}
		}
		if err != nil {
			// io.EOF or a partial trailing line: stop. The offset stays at the
			// last complete line so the partial gets re-read next tick.
			break
		}
	}
	e.offset = off
	return e.costUSD
}

// scanSessionCost totals a session's cost, split by transcript source: main is
// the parent transcript (the session's main loop), subagents is the summed
// cost of every Task-tool subagent transcript (siblings under
// <uuid>/subagents/*.jsonl, which Claude Code creates mid-session). Each file
// flows through the same per-path incremental cache; the files are disjoint,
// so no cross-file dedup is needed.
func scanSessionCost(path string) (main, subagents float64) {
	main = scanCostIncremental(path)
	subDir := strings.TrimSuffix(path, ".jsonl")
	subs, _ := filepath.Glob(filepath.Join(subDir, "subagents", "*.jsonl"))
	for _, f := range subs {
		subagents += scanCostIncremental(f)
	}
	return main, subagents
}
