package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// approxEq compares dollar amounts with a cent-scale tolerance so float
// rounding never trips an assertion.
func approxEq(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func TestPriceFor(t *testing.T) {
	cases := []struct {
		model string
		ok    bool
		input float64
	}{
		{"claude-fable-5", true, 10},
		{"claude-fable-5[1m]", true, 10},
		{"claude-mythos-1", true, 10},
		{"claude-opus-4-8-20251101", true, 5},
		{"claude-sonnet-4-6", true, 3},
		{"claude-haiku-4-5", true, 1},
		{"gpt-4o", false, 0},
		{"", false, 0},
	}
	for _, c := range cases {
		p, ok := priceFor(c.model)
		if ok != c.ok {
			t.Errorf("priceFor(%q) ok = %v, want %v", c.model, ok, c.ok)
		}
		if ok && p.input != c.input {
			t.Errorf("priceFor(%q) input = %v, want %v", c.model, p.input, c.input)
		}
	}
}

func TestUsageCost(t *testing.T) {
	const M = 1_000_000
	split := func(five, one int) *struct {
		Ephemeral5m int `json:"ephemeral_5m_input_tokens"`
		Ephemeral1h int `json:"ephemeral_1h_input_tokens"`
	} {
		return &struct {
			Ephemeral5m int `json:"ephemeral_5m_input_tokens"`
			Ephemeral1h int `json:"ephemeral_1h_input_tokens"`
		}{five, one}
	}

	cases := []struct {
		name  string
		model string
		usage costUsage
		want  float64
	}{
		{"fable input", "claude-fable-5", costUsage{InputTokens: M}, 10},
		{"fable output", "claude-fable-5", costUsage{OutputTokens: M}, 50},
		{"fable cache read", "claude-fable-5", costUsage{CacheReadTokens: M}, 1},
		{"fable cache write fallback 5m", "claude-fable-5", costUsage{CacheCreationTokens: M}, 12.50},
		{"fable cache write split", "claude-fable-5", costUsage{CacheCreationTokens: 2 * M, CacheCreation: split(M, M)}, 32.50},
		{"fable split 1h only", "claude-fable-5", costUsage{CacheCreation: split(0, M)}, 20},
		{"opus mix", "claude-opus-4-8", costUsage{InputTokens: M, OutputTokens: M, CacheReadTokens: M}, 30.50},
		{"opus split", "claude-opus-4-8", costUsage{CacheCreation: split(M, M)}, 16.25},
		{"sonnet mix", "claude-sonnet-4-6", costUsage{InputTokens: M, OutputTokens: M}, 18},
		{"sonnet fallback", "claude-sonnet-4-6", costUsage{CacheCreationTokens: M}, 3.75},
		{"haiku mix", "claude-haiku-4-5", costUsage{InputTokens: M, OutputTokens: M, CacheReadTokens: M}, 6.10},
	}
	for _, c := range cases {
		p, ok := priceFor(c.model)
		if !ok {
			t.Fatalf("%s: priceFor(%q) not found", c.name, c.model)
		}
		if got := usageCost(p, c.usage); !approxEq(got, c.want) {
			t.Errorf("%s: usageCost = %v, want %v", c.name, got, c.want)
		}
	}
}

// asstLine builds an assistant transcript line with the given ids, model, and
// input tokens (priced at that family's input rate).
func asstLine(msgID, reqID, model string, inputTokens int) string {
	e := map[string]any{
		"type":      "assistant",
		"requestId": reqID,
		"message": map[string]any{
			"id":    msgID,
			"model": model,
			"usage": map[string]any{"input_tokens": inputTokens},
		},
	}
	b, _ := json.Marshal(e)
	return string(b)
}

func TestLineCostDedup(t *testing.T) {
	e := newCostCacheEntry()
	line := []byte(asstLine("msg-1", "req-1", "claude-fable-5", 1_000_000))

	c, ok := lineCost(line, e)
	if !ok || !approxEq(c, 10) {
		t.Fatalf("first: cost=%v ok=%v, want 10 true", c, ok)
	}
	// Same message.id+requestId → deduped.
	if c, ok := lineCost(line, e); ok || c != 0 {
		t.Errorf("dup: cost=%v ok=%v, want 0 false", c, ok)
	}
	// Different requestId with same message.id → counted.
	if c, ok := lineCost([]byte(asstLine("msg-1", "req-2", "claude-fable-5", 1_000_000)), e); !ok || !approxEq(c, 10) {
		t.Errorf("distinct req: cost=%v ok=%v, want 10 true", c, ok)
	}
}

func TestLineCostSkips(t *testing.T) {
	e := newCostCacheEntry()
	skips := []string{
		`not json`,
		`{"type":"user","message":{"role":"user"}}`,
		asstLine("m", "r", "gpt-4o", 1_000_000),                               // unknown model
		`{"type":"assistant","message":{"id":"m2","model":"claude-fable-5"}}`, // no usage block
	}
	for _, s := range skips {
		if c, ok := lineCost([]byte(s), e); ok || c != 0 {
			t.Errorf("line %q: cost=%v ok=%v, want skipped", s, c, ok)
		}
	}
}

// costOffset returns the cached byte offset for a path (0 if absent).
func costOffset(path string) int64 {
	costCacheMu.Lock()
	defer costCacheMu.Unlock()
	if e := costCache[path]; e != nil {
		return e.offset
	}
	return 0
}

func TestScanCostIncremental(t *testing.T) {
	p := writeTranscript(t,
		asstLine("m1", "r1", "claude-fable-5", 1_000_000),
		asstLine("m2", "r2", "claude-fable-5", 1_000_000),
	)
	st, _ := os.Stat(p)

	if got := scanCostIncremental(p); !approxEq(got, 20) {
		t.Fatalf("initial scan = %v, want 20", got)
	}
	if off := costOffset(p); off != st.Size() {
		t.Errorf("offset after initial scan = %d, want %d", off, st.Size())
	}

	// Append a third line; only the delta should be parsed.
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(f, asstLine("m3", "r3", "claude-fable-5", 1_000_000))
	f.Close()
	st2, _ := os.Stat(p)

	if got := scanCostIncremental(p); !approxEq(got, 30) {
		t.Errorf("after append = %v, want 30", got)
	}
	if off := costOffset(p); off != st2.Size() {
		t.Errorf("offset after append = %d, want %d", off, st2.Size())
	}

	// Re-appending an already-seen line must not double-count.
	f, err = os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(f, asstLine("m1", "r1", "claude-fable-5", 1_000_000))
	f.Close()
	if got := scanCostIncremental(p); !approxEq(got, 30) {
		t.Errorf("after dup append = %v, want 30 (deduped)", got)
	}
}

func TestScanCostTruncationReset(t *testing.T) {
	p := writeTranscript(t,
		asstLine("m1", "r1", "claude-fable-5", 1_000_000),
		asstLine("m2", "r2", "claude-fable-5", 1_000_000),
	)
	if got := scanCostIncremental(p); !approxEq(got, 20) {
		t.Fatalf("initial = %v, want 20", got)
	}
	// Shrink the file (rotation/truncation) → full rescan of new content.
	if err := os.WriteFile(p, []byte(asstLine("m9", "r9", "claude-opus-4-8", 1_000_000)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := scanCostIncremental(p); !approxEq(got, 5) {
		t.Errorf("after truncation = %v, want 5 (opus input only)", got)
	}
	st, _ := os.Stat(p)
	if off := costOffset(p); off != st.Size() {
		t.Errorf("offset after truncation = %d, want %d", off, st.Size())
	}
}

func TestScanCostMissing(t *testing.T) {
	if got := scanCostIncremental("/nonexistent/path/s.jsonl"); got != 0 {
		t.Errorf("missing file = %v, want 0", got)
	}
}

// writeLines writes newline-terminated lines to path, creating parent dirs.
func writeLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	var data []byte
	for _, l := range lines {
		data = append(data, l...)
		data = append(data, '\n')
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestScanSessionCostSubagents(t *testing.T) {
	dir := t.TempDir()
	parent := filepath.Join(dir, "sess.jsonl")
	writeLines(t, parent, asstLine("m1", "r1", "claude-fable-5", 1_000_000)) // $10

	// Subagent transcripts live in a sibling dir named after the session uuid.
	subs := filepath.Join(dir, "sess", "subagents")
	writeLines(t, filepath.Join(subs, "agent-1.jsonl"), asstLine("s1", "q1", "claude-sonnet-4-6", 1_000_000)) // $3
	writeLines(t, filepath.Join(subs, "agent-2.jsonl"), asstLine("s2", "q2", "claude-opus-4-8", 1_000_000))   // $5

	main, sub := scanSessionCost(parent)
	if !approxEq(main, 10) {
		t.Errorf("main (parent) cost = %v, want 10", main)
	}
	if !approxEq(sub, 8) {
		t.Errorf("subagents cost = %v, want 8 (3+5)", sub)
	}
}

func TestScanSessionCostNoSubagents(t *testing.T) {
	parent := writeTranscript(t, asstLine("m1", "r1", "claude-fable-5", 1_000_000))
	main, sub := scanSessionCost(parent)
	if !approxEq(main, 10) {
		t.Errorf("main cost = %v, want 10", main)
	}
	if sub != 0 {
		t.Errorf("subagents cost = %v, want 0 (no subagents dir)", sub)
	}
}
