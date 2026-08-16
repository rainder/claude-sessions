package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sync"
)

const grokUpdatesFile = "updates.jsonl"

// grokCostTicksPerUSD is Grok's documented unit: 1 USD = 10^10 ticks.
const grokCostTicksPerUSD = 10_000_000_000

// grokCostCacheEntry holds the incremental scan state for one updates.jsonl:
// the byte offset consumed so far, the running dollar cost and token count,
// and the prompt_ids already counted.
type grokCostCacheEntry struct {
	offset  int64
	costUSD float64
	tokens  int
	seen    map[string]bool // prompt_id already counted
}

func newGrokCostCacheEntry() *grokCostCacheEntry {
	return &grokCostCacheEntry{seen: map[string]bool{}}
}

var (
	grokCostCacheMu sync.Mutex
	grokCostCache   = map[string]*grokCostCacheEntry{}
)

// grokUpdatesPath locates updates.jsonl. The cheap path is the sibling of a
// summary we already found (including a fallback-scan hit). When there is no
// summary yet the derived encoding is used, same as grokEventsPath.
func grokUpdatesPath(home, cwd, sessionID string) string {
	if p, ok := grokSummaryPath(home, cwd, sessionID); ok {
		return filepath.Join(filepath.Dir(p), grokUpdatesFile)
	}
	return filepath.Join(home, grokDir, "sessions", url.PathEscape(cwd), sessionID, grokUpdatesFile)
}

// scanGrokCost returns the cumulative dollar cost and token count of one
// updates.jsonl, parsing only the bytes appended since the previous call.
// State is cached per path; a file smaller than the cached offset
// (truncation or rotation) resets the entry and forces a full rescan. Only
// complete newline-terminated lines advance the offset, so a partially
// written trailing line is re-read on the next tick. Missing file is 0, 0.
// Any open/seek error returns the cached values so far — never an error to
// the caller.
func scanGrokCost(path string) (cost float64, tokens int) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, 0
	}
	grokCostCacheMu.Lock()
	defer grokCostCacheMu.Unlock()

	e := grokCostCache[path]
	if e == nil || st.Size() < e.offset {
		e = newGrokCostCacheEntry()
		grokCostCache[path] = e
	}
	if st.Size() == e.offset {
		return e.costUSD, e.tokens
	}

	f, err := os.Open(path)
	if err != nil {
		return e.costUSD, e.tokens
	}
	defer f.Close()
	if _, err := f.Seek(e.offset, io.SeekStart); err != nil {
		return e.costUSD, e.tokens
	}

	r := bufio.NewReaderSize(f, 64*1024)
	off := e.offset
	for {
		line, err := r.ReadBytes('\n')
		if n := len(line); n > 0 && line[n-1] == '\n' {
			off += int64(n)
			if c, tok, ok := grokTurnCost(line, e); ok {
				e.costUSD += c
				e.tokens += tok
			}
		}
		if err != nil {
			break
		}
	}
	e.offset = off
	return e.costUSD, e.tokens
}

// grokTurnCost prices one updates.jsonl line. Only turn_completed lines that
// carry a non-nil costUsdTicks and/or totalTokens count. Dedup is by
// prompt_id when non-empty (first counted emission wins). Empty prompt_id
// still counts — there is no key. Negative ticks are ignored. Tokens come
// from usage.totalTokens only; cache fields are already inside that total.
func grokTurnCost(line []byte, e *grokCostCacheEntry) (float64, int, bool) {
	if !bytes.Contains(line, []byte("turn_completed")) {
		return 0, 0, false
	}
	var ev struct {
		Params struct {
			Update struct {
				SessionUpdate string `json:"sessionUpdate"`
				PromptID      string `json:"prompt_id"`
				Usage         *struct {
					CostUsdTicks *int64 `json:"costUsdTicks"`
					TotalTokens  int    `json:"totalTokens"`
				} `json:"usage"`
			} `json:"update"`
		} `json:"params"`
	}
	if err := json.Unmarshal(line, &ev); err != nil {
		return 0, 0, false
	}
	if ev.Params.Update.SessionUpdate != "turn_completed" {
		return 0, 0, false
	}
	if ev.Params.Update.Usage == nil {
		return 0, 0, false
	}
	u := ev.Params.Update.Usage
	hasTicks := u.CostUsdTicks != nil && *u.CostUsdTicks >= 0
	tokens := u.TotalTokens
	if !hasTicks && tokens == 0 {
		return 0, 0, false
	}
	if id := ev.Params.Update.PromptID; id != "" {
		if e.seen[id] {
			return 0, 0, false
		}
		e.seen[id] = true
	}
	var cost float64
	if hasTicks {
		cost = float64(*u.CostUsdTicks) / grokCostTicksPerUSD
	}
	return cost, tokens, true
}
