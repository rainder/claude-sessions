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
	update := map[string]any{
		"sessionUpdate": "turn_completed",
		"prompt_id":     promptID,
	}
	if ticks != nil {
		update["usage"] = map[string]any{"costUsdTicks": *ticks}
	} else {
		update["usage"] = map[string]any{}
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
	if got := scanGrokCost(p); !approxEq(got, want) {
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
	if got := scanGrokCost(p); !approxEq(got, want) {
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
	if got := scanGrokCost(p); !approxEq(got, want2) {
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
	if got := scanGrokCost(p); !approxEq(got, want3) {
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
	if got := scanGrokCost(p); !approxEq(got, want3) {
		t.Errorf("after dup append = %v, want %v (deduped)", got, want3)
	}
}

func TestScanGrokCostTruncationReset(t *testing.T) {
	p := filepath.Join(t.TempDir(), grokUpdatesFile)
	writeLines(t, p,
		grokTurnLine("a", ticksPtr(550018000)),
		grokTurnLine("b", ticksPtr(1261349000)),
	)
	if got := scanGrokCost(p); !approxEq(got, float64(550018000+1261349000)/grokCostTicksPerUSD) {
		t.Fatalf("initial = %v", got)
	}
	writeLines(t, p, grokTurnLine("c", ticksPtr(10_000_000_000)))
	if got := scanGrokCost(p); !approxEq(got, 1) {
		t.Errorf("after truncation = %v, want 1 (only the new turn)", got)
	}
	st, _ := os.Stat(p)
	if off := grokCostOffset(p); off != st.Size() {
		t.Errorf("offset after truncation = %d, want %d", off, st.Size())
	}
}

func TestScanGrokCostMissing(t *testing.T) {
	if got := scanGrokCost("/nonexistent/path/updates.jsonl"); got != 0 {
		t.Errorf("missing file = %v, want 0", got)
	}
}
