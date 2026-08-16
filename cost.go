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

// usageTokens sums one usage block. Cache writes use the 5m+1h split when
// present, else cache_creation_input_tokens — the same split/fallback as
// usageCost, never both.
func usageTokens(u costUsage) int {
	n := u.InputTokens + u.OutputTokens + u.CacheReadTokens
	if u.CacheCreation != nil {
		n += u.CacheCreation.Ephemeral5m + u.CacheCreation.Ephemeral1h
	} else {
		n += u.CacheCreationTokens
	}
	return n
}

// lineCost prices a single transcript line, returning its dollar cost, token
// count, and whether it counted (an assistant usage line not already seen).
// Unknown models still count tokens at $0. e.seen dedupes streaming
// re-emissions by message.id+requestId. User lines whose content is a plain
// string fail the unmarshal and are skipped, same as before.
func lineCost(line []byte, e *costCacheEntry) (cost float64, tokens int, ok bool) {
	var ev struct {
		Type      string `json:"type"`
		RequestID string `json:"requestId"`
		Message   struct {
			ID    string     `json:"id"`
			Model string     `json:"model"`
			Usage *costUsage `json:"usage"`
		} `json:"message"`
	}
	if err := json.Unmarshal(line, &ev); err != nil {
		return 0, 0, false
	}
	if ev.Type != "assistant" || ev.Message.Usage == nil {
		return 0, 0, false
	}
	key := ev.Message.ID + "\x00" + ev.RequestID
	if e.seen[key] {
		return 0, 0, false
	}
	e.seen[key] = true
	tokens = usageTokens(*ev.Message.Usage)
	price, known := priceFor(ev.Message.Model)
	if !known {
		return 0, tokens, true
	}
	return usageCost(price, *ev.Message.Usage), tokens, true
}

// costCacheEntry holds the incremental scan state for one transcript file: the
// byte offset consumed so far, the running dollar cost and token count, and
// the dedup set of message.id+requestId keys already counted.
type costCacheEntry struct {
	offset  int64
	costUSD float64
	tokens  int
	seen    map[string]bool
}

func newCostCacheEntry() *costCacheEntry {
	return &costCacheEntry{seen: map[string]bool{}}
}

// costCache and its mutex mirror metaCache's concurrency model: collection can
// run from multiple goroutines, so every access is guarded.
var (
	costCacheMu sync.Mutex
	costCache   = map[string]*costCacheEntry{}
)

// scanCostIncremental returns the cumulative dollar cost and token count of
// one transcript file (parent or subagent), parsing only the bytes appended
// since the previous call. State is cached per path; a file smaller than the
// cached offset (truncation or rotation) resets the entry and forces a full
// rescan. Only complete newline-terminated lines advance the offset, so a
// partially written trailing line is re-read on the next tick. Returns 0, 0
// on any error.
func scanCostIncremental(path string) (cost float64, tokens int) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, 0
	}
	costCacheMu.Lock()
	defer costCacheMu.Unlock()

	e := costCache[path]
	if e == nil || st.Size() < e.offset {
		e = newCostCacheEntry()
		costCache[path] = e
	}
	if st.Size() == e.offset {
		return e.costUSD, e.tokens // nothing appended since last scan
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
			if c, tok, ok := lineCost(line, e); ok {
				e.costUSD += c
				e.tokens += tok
			}
		}
		if err != nil {
			// io.EOF or a partial trailing line: stop. The offset stays at the
			// last complete line so the partial gets re-read next tick.
			break
		}
	}
	e.offset = off
	return e.costUSD, e.tokens
}

// scanSessionCost totals a session's cost and tokens. main/subagents split
// dollars by transcript source (parent vs Task-tool siblings under
// <uuid>/subagents/*.jsonl). tokens is the sum across both. Each file flows
// through the same per-path incremental cache; the files are disjoint, so no
// cross-file dedup is needed.
func scanSessionCost(path string) (main, subagents float64, tokens int) {
	var mainTok int
	main, mainTok = scanCostIncremental(path)
	subDir := strings.TrimSuffix(path, ".jsonl")
	subs, _ := filepath.Glob(filepath.Join(subDir, "subagents", "*.jsonl"))
	subTok := 0
	for _, f := range subs {
		c, t := scanCostIncremental(f)
		subagents += c
		subTok += t
	}
	return main, subagents, mainTok + subTok
}
