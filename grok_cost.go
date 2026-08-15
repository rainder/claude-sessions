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
// the byte offset consumed so far, the running dollar cost, and the prompt_ids
// already counted.
type grokCostCacheEntry struct {
	offset  int64
	costUSD float64
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

// scanGrokCost returns the cumulative dollar cost of one updates.jsonl,
// parsing only the bytes appended since the previous call. State is cached
// per path; a file smaller than the cached offset (truncation or rotation)
// resets the entry and forces a full rescan. Only complete newline-terminated
// lines advance the offset, so a partially written trailing line is re-read
// on the next tick. Missing file is 0. Any open/seek error returns the
// cached cost so far — never an error to the caller.
func scanGrokCost(path string) float64 {
	st, err := os.Stat(path)
	if err != nil {
		return 0
	}
	grokCostCacheMu.Lock()
	defer grokCostCacheMu.Unlock()

	e := grokCostCache[path]
	if e == nil || st.Size() < e.offset {
		e = newGrokCostCacheEntry()
		grokCostCache[path] = e
	}
	if st.Size() == e.offset {
		return e.costUSD
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
			if c, ok := grokTurnCost(line, e); ok {
				e.costUSD += c
			}
		}
		if err != nil {
			break
		}
	}
	e.offset = off
	return e.costUSD
}

// grokTurnCost prices one updates.jsonl line. Only turn_completed lines that
// carry a non-nil costUsdTicks count. Dedup is by prompt_id when non-empty
// (first counted emission wins). Empty prompt_id still counts — there is no
// key. Negative ticks are ignored.
func grokTurnCost(line []byte, e *grokCostCacheEntry) (float64, bool) {
	if !bytes.Contains(line, []byte("turn_completed")) {
		return 0, false
	}
	var ev struct {
		Params struct {
			Update struct {
				SessionUpdate string `json:"sessionUpdate"`
				PromptID      string `json:"prompt_id"`
				Usage         *struct {
					CostUsdTicks *int64 `json:"costUsdTicks"`
				} `json:"usage"`
			} `json:"update"`
		} `json:"params"`
	}
	if err := json.Unmarshal(line, &ev); err != nil {
		return 0, false
	}
	if ev.Params.Update.SessionUpdate != "turn_completed" {
		return 0, false
	}
	if ev.Params.Update.Usage == nil || ev.Params.Update.Usage.CostUsdTicks == nil {
		return 0, false
	}
	ticks := *ev.Params.Update.Usage.CostUsdTicks
	if ticks < 0 {
		return 0, false
	}
	if id := ev.Params.Update.PromptID; id != "" {
		if e.seen[id] {
			return 0, false
		}
		e.seen[id] = true
	}
	return float64(ticks) / grokCostTicksPerUSD, true
}
