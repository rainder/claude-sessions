package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func ticksPtr(n int64) *int64 { return &n }

// grokTurnLine builds one turn_completed ACP line. A nil ticks pointer
// omits costUsdTicks (absence is not free). Extra keys are ignored.
func grokTurnLine(promptID string, ticks *int64) string {
	return grokTurnLineUsage(promptID, ticks, 0)
}

// grokTurnLineUsage is grokTurnLine plus usage.totalTokens. Zero omits the
// field so existing cost-only tests stay tokens=0.
func grokTurnLineUsage(promptID string, ticks *int64, totalTokens int) string {
	return grokTurnLineUsageMap(promptID, ticks, totalTokens, nil)
}

func grokTurnLineUsageMap(promptID string, ticks *int64, totalTokens int, extra map[string]any) string {
	usage := map[string]any{}
	if ticks != nil {
		usage["costUsdTicks"] = *ticks
	}
	if totalTokens != 0 {
		usage["totalTokens"] = totalTokens
	}
	for k, v := range extra {
		usage[k] = v
	}
	update := map[string]any{
		"sessionUpdate": "turn_completed",
		"prompt_id":     promptID,
		"usage":         usage,
	}
	b, _ := json.Marshal(map[string]any{
		"method": "session/update",
		"params": map[string]any{"update": update},
	})
	return string(b)
}

func grokTurnLineNullTicks(promptID string) string {
	return `{"method":"session/update","params":{"update":{"sessionUpdate":"turn_completed","prompt_id":"` + promptID + `","usage":{"costUsdTicks":null}}}}`
}

func grokToolCallLine(ticks int64) string {
	return fmt.Sprintf(`{"method":"session/update","params":{"update":{"sessionUpdate":"tool_call","prompt_id":"tool","usage":{"costUsdTicks":%d}}}}`, ticks)
}

func TestScanGrokCostSumsTurnCompletedTicks(t *testing.T) {
	p := filepath.Join(t.TempDir(), grokUpdatesFile)
	writeLines(t, p,
		grokTurnLine("a", ticksPtr(550018000)),
		grokToolCallLine(999999999),
		grokTurnLine("b", ticksPtr(1261349000)),
	)
	want := float64(550018000+1261349000) / grokCostTicksPerUSD
	if got, _ := scanGrokCost(p); !approxEq(got, want) {
		t.Errorf("scanGrokCost = %v, want %v", got, want)
	}
}

func TestScanGrokCostSkipsMissingTicks(t *testing.T) {
	p := filepath.Join(t.TempDir(), grokUpdatesFile)
	writeLines(t, p,
		grokTurnLine("with", ticksPtr(550018000)),
		grokTurnLine("omit", nil),
		grokTurnLineNullTicks("null"),
	)
	want := float64(550018000) / grokCostTicksPerUSD
	if got, _ := scanGrokCost(p); !approxEq(got, want) {
		t.Errorf("scanGrokCost = %v, want %v (only the turn that carried ticks)", got, want)
	}
}

func grokCostOffset(path string) int64 {
	grokCostCacheMu.Lock()
	defer grokCostCacheMu.Unlock()
	if e := grokCostCache[path]; e != nil {
		return e.offset
	}
	return 0
}

func TestScanGrokCostIncremental(t *testing.T) {
	p := filepath.Join(t.TempDir(), grokUpdatesFile)
	writeLines(t, p,
		grokTurnLine("a", ticksPtr(550018000)),
		grokTurnLine("b", ticksPtr(1261349000)),
	)
	st, _ := os.Stat(p)

	want2 := float64(550018000+1261349000) / grokCostTicksPerUSD
	if got, _ := scanGrokCost(p); !approxEq(got, want2) {
		t.Fatalf("initial scan = %v, want %v", got, want2)
	}
	if off := grokCostOffset(p); off != st.Size() {
		t.Errorf("offset after initial scan = %d, want %d", off, st.Size())
	}

	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(f, grokTurnLine("c", ticksPtr(10_000_000_000)))
	f.Close()
	st2, _ := os.Stat(p)

	want3 := want2 + 1
	if got, _ := scanGrokCost(p); !approxEq(got, want3) {
		t.Errorf("after append = %v, want %v", got, want3)
	}
	if off := grokCostOffset(p); off != st2.Size() {
		t.Errorf("offset after append = %d, want %d", off, st2.Size())
	}

	f, err = os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(f, grokTurnLine("a", ticksPtr(550018000)))
	f.Close()
	if got, _ := scanGrokCost(p); !approxEq(got, want3) {
		t.Errorf("after dup append = %v, want %v (deduped)", got, want3)
	}
}

func TestScanGrokCostTruncationReset(t *testing.T) {
	p := filepath.Join(t.TempDir(), grokUpdatesFile)
	writeLines(t, p,
		grokTurnLine("a", ticksPtr(550018000)),
		grokTurnLine("b", ticksPtr(1261349000)),
	)
	if got, _ := scanGrokCost(p); !approxEq(got, float64(550018000+1261349000)/grokCostTicksPerUSD) {
		t.Fatalf("initial = %v", got)
	}
	writeLines(t, p, grokTurnLine("c", ticksPtr(10_000_000_000)))
	if got, _ := scanGrokCost(p); !approxEq(got, 1) {
		t.Errorf("after truncation = %v, want 1 (only the new turn)", got)
	}
	st, _ := os.Stat(p)
	if off := grokCostOffset(p); off != st.Size() {
		t.Errorf("offset after truncation = %d, want %d", off, st.Size())
	}
}

func TestScanGrokCostMissing(t *testing.T) {
	if got, _ := scanGrokCost("/nonexistent/path/updates.jsonl"); got != 0 {
		t.Errorf("missing file = %v, want 0", got)
	}
}

func TestScanGrokCostMissingClearsWarmCache(t *testing.T) {
	p := filepath.Join(t.TempDir(), grokUpdatesFile)
	writeLines(t, p, grokTurnLine("a", ticksPtr(550018000)))
	if got, _ := scanGrokCost(p); !approxEq(got, float64(550018000)/grokCostTicksPerUSD) {
		t.Fatalf("seed scan = %v", got)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if got, _ := scanGrokCost(p); got != 0 {
		t.Errorf("removed file = %v, want 0", got)
	}
}

func TestGrokHasOpenBackground(t *testing.T) {
	p := filepath.Join(t.TempDir(), grokUpdatesFile)
	writeLines(t, p,
		`{"params":{"update":{"sessionUpdate":"task_backgrounded","task_id":"t1"}}}`,
		`{"params":{"update":{"sessionUpdate":"task_backgrounded","task_id":"t2"}}}`,
		`{"params":{"update":{"sessionUpdate":"task_completed","task_snapshot":{"task_id":"t1"}}}}`,
	)
	if !grokHasOpenBackground(p) {
		t.Fatal("open t2 should report background")
	}

	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(f, `{"params":{"update":{"sessionUpdate":"task_completed","task_snapshot":{"task_id":"t2"}}}}`)
	f.Close()
	if grokHasOpenBackground(p) {
		t.Error("after t2 completed, want no open background")
	}
}

func TestGrokHasOpenBackgroundMissing(t *testing.T) {
	if grokHasOpenBackground("/nonexistent/path/updates.jsonl") {
		t.Error("missing file should not report an open background")
	}
}

func TestGrokHasOpenBackgroundMissingClearsWarmCache(t *testing.T) {
	p := filepath.Join(t.TempDir(), grokUpdatesFile)
	writeLines(t, p,
		`{"params":{"update":{"sessionUpdate":"task_backgrounded","task_id":"t1"}}}`,
	)
	if !grokHasOpenBackground(p) {
		t.Fatal("seed write should report background")
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if grokHasOpenBackground(p) {
		t.Error("removed file should not keep a stale open background")
	}
}

func TestGrokHasOpenBackgroundIgnoresEmptyID(t *testing.T) {
	p := filepath.Join(t.TempDir(), grokUpdatesFile)
	writeLines(t, p,
		`{"params":{"update":{"sessionUpdate":"task_backgrounded","task_id":""}}}`,
		`{"params":{"update":{"sessionUpdate":"task_completed","task_snapshot":{"task_id":"ghost"}}}}`,
	)
	if grokHasOpenBackground(p) {
		t.Error("empty id and unmatched complete should not report background")
	}
}

func TestScanGrokCostSumsTotalTokens(t *testing.T) {
	p := filepath.Join(t.TempDir(), grokUpdatesFile)
	writeLines(t, p,
		grokTurnLineUsage("a", ticksPtr(550018000), 1000),
		grokToolCallLine(999999999),
		grokTurnLineUsage("b", ticksPtr(1261349000), 2000),
	)
	_, tok := scanGrokCost(p)
	if tok != 3000 {
		t.Errorf("tokens = %d, want 3000 (tool_call skipped)", tok)
	}
}

func TestScanGrokCostDoesNotAddCacheFields(t *testing.T) {
	p := filepath.Join(t.TempDir(), grokUpdatesFile)
	writeLines(t, p, grokTurnLineUsageMap("a", ticksPtr(550018000), 5000, map[string]any{
		"cachedReadTokens":    1000,
		"cacheCreationTokens": 2000,
		"inputTokens":         3000,
		"outputTokens":        4000,
	}))
	_, tok := scanGrokCost(p)
	if tok != 5000 {
		t.Errorf("tokens = %d, want 5000 (cache/input/output not added)", tok)
	}
}

func TestScanGrokCostCountsTokensWithoutTicks(t *testing.T) {
	p := filepath.Join(t.TempDir(), grokUpdatesFile)
	writeLines(t, p, grokTurnLineUsage("a", nil, 5000))
	cost, tok := scanGrokCost(p)
	if cost != 0 || tok != 5000 {
		t.Errorf("cost=%v tokens=%d, want 0 5000", cost, tok)
	}
}

func TestScanGrokCostIncrementalTokens(t *testing.T) {
	p := filepath.Join(t.TempDir(), grokUpdatesFile)
	writeLines(t, p,
		grokTurnLineUsage("a", ticksPtr(550018000), 1000),
		grokTurnLineUsage("b", ticksPtr(1261349000), 2000),
	)
	_, tok := scanGrokCost(p)
	if tok != 3000 {
		t.Fatalf("initial tokens = %d, want 3000", tok)
	}

	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(f, grokTurnLineUsage("c", ticksPtr(10_000_000_000), 4000))
	f.Close()
	_, tok = scanGrokCost(p)
	if tok != 7000 {
		t.Errorf("after append tokens = %d, want 7000", tok)
	}

	f, err = os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(f, grokTurnLineUsage("a", ticksPtr(550018000), 1000))
	f.Close()
	_, tok = scanGrokCost(p)
	if tok != 7000 {
		t.Errorf("after dup append tokens = %d, want 7000", tok)
	}
}

func TestScanGrokCostTruncationResetTokens(t *testing.T) {
	p := filepath.Join(t.TempDir(), grokUpdatesFile)
	writeLines(t, p,
		grokTurnLineUsage("a", ticksPtr(550018000), 1000),
		grokTurnLineUsage("b", ticksPtr(1261349000), 2000),
	)
	if _, tok := scanGrokCost(p); tok != 3000 {
		t.Fatalf("initial tokens = %d, want 3000", tok)
	}
	writeLines(t, p, grokTurnLineUsage("c", ticksPtr(10_000_000_000), 500))
	if _, tok := scanGrokCost(p); tok != 500 {
		t.Errorf("after truncation tokens = %d, want 500", tok)
	}
}

func TestScanGrokCostMissingTokens(t *testing.T) {
	_, tok := scanGrokCost("/nonexistent/path/updates.jsonl")
	if tok != 0 {
		t.Errorf("missing file tokens = %d, want 0", tok)
	}
}
