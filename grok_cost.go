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

// grokPromptCost is the last counted turn_completed for one prompt_id.
// Last-wins: grok may emit an incomplete snapshot (no costUsdTicks) and
// later a complete one for the same prompt; first-wins would keep the
// short count forever.
type grokPromptCost struct {
	cost   float64
	tokens int
}

// grokCostCacheEntry holds the incremental scan state for one updates.jsonl:
// the byte offset consumed so far, the running dollar cost and token count,
// the last counted totals per prompt_id, and the background task ids still
// open (task_backgrounded with no matching task_completed).
type grokCostCacheEntry struct {
	offset  int64
	costUSD float64
	tokens  int
	seen    map[string]grokPromptCost // last counted totals per prompt_id
	open    map[string]bool           // background task_id still running
}

func newGrokCostCacheEntry() *grokCostCacheEntry {
	return &grokCostCacheEntry{seen: map[string]grokPromptCost{}, open: map[string]bool{}}
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

const grokSubagentsDir = "subagents"
const grokSubagentMetaFile = "meta.json"

// grokChildUpdatesPaths lists each spawned subagent's updates.jsonl. Grok
// writes child sessions as their own trees under ~/.grok/sessions, not as
// sibling jsonl files the way Claude Code does. Identity comes from
// subagents/<id>/meta.json (child_session_id + child_cwd). A missing dir,
// unreadable meta, or empty child id is skipped — never an error. One
// level only, matching scanSessionCost's glob; nested grandchildren stay
// on their own parent. os.ReadDir, not Glob: home is data, not a pattern.
func grokChildUpdatesPaths(home, cwd, sessionID string) []string {
	parent := grokUpdatesPath(home, cwd, sessionID)
	ents, err := os.ReadDir(filepath.Join(filepath.Dir(parent), grokSubagentsDir))
	if err != nil {
		return nil
	}
	var out []string
	for _, ent := range ents {
		if !ent.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(filepath.Dir(parent), grokSubagentsDir, ent.Name(), grokSubagentMetaFile))
		if err != nil {
			continue
		}
		var meta struct {
			ChildSessionID string `json:"child_session_id"`
			ChildCWD       string `json:"child_cwd"`
		}
		if json.Unmarshal(data, &meta) != nil {
			continue
		}
		id := meta.ChildSessionID
		if id == "" {
			id = ent.Name()
		}
		if id == "" || id == sessionID {
			continue
		}
		childCWD := meta.ChildCWD
		if childCWD == "" {
			childCWD = cwd
		}
		out = append(out, grokUpdatesPath(home, childCWD, id))
	}
	return out
}

// scanGrokSessionCost totals a grok session's cost and tokens. main is the
// parent updates.jsonl; subagents is the sum of each child session's own
// updates.jsonl. tokens is the sum across both. Each file uses the same
// per-path incremental cache as scanGrokCost. Parent turn_completed.usage
// does not include child sessions.
func scanGrokSessionCost(home, cwd, sessionID string) (main, subagents float64, tokens int) {
	var mainTok int
	main, mainTok = scanGrokCost(grokUpdatesPath(home, cwd, sessionID))
	subTok := 0
	for _, p := range grokChildUpdatesPaths(home, cwd, sessionID) {
		c, t := scanGrokCost(p)
		subagents += c
		subTok += t
	}
	return main, subagents, mainTok + subTok
}

// scanGrokCost returns the cumulative dollar cost and token count of one
// updates.jsonl, parsing only the bytes appended since the previous call.
// The same pass updates the open-background set that grokHasOpenBackground
// reads. State is cached per path; a file smaller than the cached offset
// (truncation or rotation) resets the entry and forces a full rescan. Only
// complete newline-terminated lines advance the offset, so a partially
// written trailing line is re-read on the next tick. Missing file is 0, 0.
// Any open/seek error returns the cached values so far — never an error to
// the caller.
func scanGrokCost(path string) (cost float64, tokens int) {
	st, err := os.Stat(path)
	if err != nil {
		grokCostCacheMu.Lock()
		delete(grokCostCache, path)
		grokCostCacheMu.Unlock()
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
			grokApplyBackground(line, e)
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

// grokHasOpenBackground reports whether updates.jsonl still has a
// task_backgrounded id with no matching task_completed. It shares
// scanGrokCost's incremental cache so a CollectLocal tick reads the
// new bytes once. A missing file is false.
func grokHasOpenBackground(path string) bool {
	scanGrokCost(path)
	grokCostCacheMu.Lock()
	defer grokCostCacheMu.Unlock()
	e := grokCostCache[path]
	return e != nil && len(e.open) > 0
}

// grokApplyBackground updates e.open from one updates.jsonl line.
// task_backgrounded adds task_id; task_completed removes
// task_snapshot.task_id. Empty ids and torn lines are ignored.
func grokApplyBackground(line []byte, e *grokCostCacheEntry) {
	if !bytes.Contains(line, []byte("task_backgrounded")) &&
		!bytes.Contains(line, []byte("task_completed")) {
		return
	}
	var ev struct {
		Params struct {
			Update struct {
				SessionUpdate string `json:"sessionUpdate"`
				TaskID        string `json:"task_id"`
				TaskSnapshot  struct {
					TaskID string `json:"task_id"`
				} `json:"task_snapshot"`
			} `json:"update"`
		} `json:"params"`
	}
	if json.Unmarshal(line, &ev) != nil {
		return
	}
	switch ev.Params.Update.SessionUpdate {
	case "task_backgrounded":
		if id := ev.Params.Update.TaskID; id != "" {
			e.open[id] = true
		}
	case "task_completed":
		if id := ev.Params.Update.TaskSnapshot.TaskID; id != "" {
			delete(e.open, id)
		}
	}
}

// grokTurnCost prices one updates.jsonl line. Only turn_completed lines that
// carry a non-nil costUsdTicks and/or totalTokens count. Dedup is last-wins
// by prompt_id when non-empty (a later emission replaces the earlier one,
// returning the delta so the incremental sum stays correct), except a later
// incomplete snapshot (tokens, no ticks) must not wipe a priced complete
// turn. Empty prompt_id still counts — there is no key. Negative ticks are
// ignored. Tokens come from usage.totalTokens only; cache fields are
// already inside that total.
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
	var cost float64
	if hasTicks {
		cost = float64(*u.CostUsdTicks) / grokCostTicksPerUSD
	}
	if id := ev.Params.Update.PromptID; id != "" {
		prev := e.seen[id]
		// A later incomplete snapshot (tokens, no ticks) must not wipe a
		// priced complete turn. Tokens may still grow; cost stays.
		if prev.cost > 0 && !hasTicks {
			if tokens > prev.tokens {
				e.seen[id] = grokPromptCost{cost: prev.cost, tokens: tokens}
				return 0, tokens - prev.tokens, true
			}
			return 0, 0, false
		}
		e.seen[id] = grokPromptCost{cost: cost, tokens: tokens}
		return cost - prev.cost, tokens - prev.tokens, true
	}
	return cost, tokens, true
}
