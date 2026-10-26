package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var clarifyNow = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

func clarifyAskDone(t *testing.T, st *fleetClarifyStore, id string) {
	t.Helper()
	_, tok, code := st.begin(id, "ask", &clarifyEntry{CardHash: "h1"}, "why?", nil, clarifyNow)
	if code != "" {
		t.Fatalf("begin %s: %q", id, code)
	}
	st.finish(id, tok, "f1", &clarifyTurn{Role: "fleet", Text: "because", At: "x"}, nil, clarifyNow)
}

func TestClarifyStoreBeginFinish(t *testing.T) {
	st := &fleetClarifyStore{}
	e, tok, code := st.begin("c1", "ask", &clarifyEntry{Tool: "claude", CardHash: "h1"}, "why?", nil, clarifyNow)
	if code != "" {
		t.Fatalf("code %q", code)
	}
	if len(e.Turns) != 1 || e.Turns[0].Role != "you" || e.Turns[0].Text != "why?" {
		t.Fatalf("turns %+v", e.Turns)
	}
	if e.Pending == nil || e.Pending.Kind != "ask" {
		t.Fatalf("pending %+v", e.Pending)
	}
	if _, _, code := st.begin("c1", "ask", nil, "again", nil, clarifyNow); code != "busy" {
		t.Fatalf("second begin: %q", code)
	}
	st.finish("c1", tok, "f1", &clarifyTurn{Role: "fleet", Text: "a", At: "x"}, nil, clarifyNow)
	got, ok := st.get("c1")
	if !ok || got.ForkID != "f1" || len(got.Turns) != 2 || got.Pending != nil {
		t.Fatalf("after finish: %+v", got)
	}
	_, tok2, code := st.begin("c1", "ask", nil, "more", nil, clarifyNow)
	if code != "" {
		t.Fatalf("begin 2: %q", code)
	}
	st.finish("c1", tok2, "f2", &clarifyTurn{Role: "fleet", Text: "b", At: "x"}, nil, clarifyNow)
	if got, _ := st.get("c1"); got.ForkID != "f1" {
		t.Fatalf("ForkID %q, want f1", got.ForkID)
	}
}

func TestClarifyStoreAskWithNoEntryNeedsInit(t *testing.T) {
	st := &fleetClarifyStore{}
	if _, _, code := st.begin("c1", "ask", nil, "q", nil, clarifyNow); code != "card_gone" {
		t.Fatalf("ask with no entry and no init: %q", code)
	}
	if _, ok := st.get("c1"); ok || st.running != 0 {
		t.Fatalf("refused ask left state: running %d", st.running)
	}
}

func TestClarifyStoreGetIsACopy(t *testing.T) {
	st := &fleetClarifyStore{}
	clarifyAskDone(t, st, "c1")
	a, _ := st.get("c1")
	a.Turns[0].Text = "changed"
	if b, _ := st.get("c1"); b.Turns[0].Text == "changed" {
		t.Fatal("get shares the turns slice")
	}
}

func TestClarifyStoreGlobalCap(t *testing.T) {
	st := &fleetClarifyStore{}
	toks := map[string]uint64{}
	for _, id := range []string{"a", "b"} {
		_, tok, code := st.begin(id, "ask", &clarifyEntry{}, "q", nil, clarifyNow)
		if code != "" {
			t.Fatalf("begin %s: %q", id, code)
		}
		toks[id] = tok
	}
	if _, _, code := st.begin("c", "ask", &clarifyEntry{}, "q", nil, clarifyNow); code != "busy" {
		t.Fatalf("third: %q", code)
	}
	st.finish("a", toks["a"], "", nil, nil, clarifyNow)
	if _, _, code := st.begin("c", "ask", &clarifyEntry{}, "q", nil, clarifyNow); code != "" {
		t.Fatalf("third after finish: %q", code)
	}
}

func TestClarifyStoreDraftNeedsTurns(t *testing.T) {
	st := &fleetClarifyStore{}
	if _, _, code := st.begin("c1", "draft", &clarifyEntry{}, "", nil, clarifyNow); code != "empty" {
		t.Fatalf("new card draft: %q", code)
	}
	if _, ok := st.get("c1"); ok {
		t.Fatal("refused draft created an entry")
	}
	// Turns but no fork yet (the first ask failed): a draft would fork
	// FLEET afresh, which knows nothing of the chat.
	_, tok0, _ := st.begin("c1", "ask", &clarifyEntry{CardHash: "h1"}, "why?", nil, clarifyNow)
	st.finish("c1", tok0, "", &clarifyTurn{Role: "error", Text: "failed", At: "x"}, nil, clarifyNow)
	if _, _, code := st.begin("c1", "draft", nil, "", nil, clarifyNow); code != "empty" {
		t.Fatalf("draft with no fork: %q", code)
	}
	st.prune(map[string]fleetDecisionCard{}, nil, clarifyNow)
	clarifyAskDone(t, st, "c1")
	e, tok, code := st.begin("c1", "draft", nil, "", nil, clarifyNow)
	if code != "" || e.Pending == nil || e.Pending.Kind != "draft" || len(e.Turns) != 1+1 {
		t.Fatalf("draft begin: %q %+v", code, e)
	}
	st.finish("c1", tok, "", nil, &clarifyDraft{Text: "go with A", At: "x"}, clarifyNow)
	if got, _ := st.get("c1"); got.Draft == nil || got.Draft.Text != "go with A" || len(got.Turns) != 2 {
		t.Fatalf("after draft: %+v", got)
	}
	if !st.discardDraft("c1") {
		t.Fatal("discard: false")
	}
	if st.discardDraft("c1") {
		t.Fatal("second discard: true")
	}
}

func TestClarifyStorePrune(t *testing.T) {
	st := &fleetClarifyStore{}
	cancelled := 0
	cancel := func() { cancelled++ }
	for _, id := range []string{"gone", "sent", "unc", "chg"} {
		_, tok, code := st.begin(id, "ask", &clarifyEntry{CardHash: "h1"}, "q", cancel, clarifyNow)
		if code != "" {
			// the cap is 2: finish each so the next can begin
			t.Fatalf("begin %s: %q", id, code)
		}
		if id != "gone" {
			st.finish(id, tok, "", &clarifyTurn{Role: "fleet", Text: "a", At: "x"}, nil, clarifyNow)
		}
	}
	cards := map[string]fleetDecisionCard{
		"sent": {Hash: "h1"}, "unc": {Hash: "h1"}, "chg": {Hash: "h2"},
	}
	answered := map[string]fleetAnswer{
		"sent": {Attempts: []fleetAttempt{{Status: "sent"}}},
		"unc":  {Attempts: []fleetAttempt{{Status: "unconfirmed"}}},
	}
	st.prune(cards, answered, clarifyNow)
	if cancelled != 1 {
		t.Fatalf("cancelled %d, want 1", cancelled)
	}
	if st.running != 0 {
		t.Fatalf("running %d", st.running)
	}
	if _, ok := st.get("gone"); ok {
		t.Fatal("gone kept")
	}
	if _, ok := st.get("sent"); ok {
		t.Fatal("sent kept")
	}
	if _, ok := st.get("unc"); !ok {
		t.Fatal("unconfirmed dropped")
	}
	st.prune(cards, answered, clarifyNow)
	e, ok := st.get("chg")
	if !ok || len(e.Turns) != 3 || e.Turns[2].Role != "note" || e.Turns[2].Text != "card changed since this chat started" || !e.Changed {
		t.Fatalf("chg: %+v", e)
	}
	if u, _ := st.get("unc"); len(u.Turns) != 2 {
		t.Fatalf("unchanged card got a note: %+v", u.Turns)
	}
	st.prune(nil, nil, clarifyNow)
	if _, ok := st.get("chg"); !ok {
		t.Fatal("nil cards dropped an entry")
	}
	st.prune(map[string]fleetDecisionCard{}, nil, clarifyNow)
	if len(st.summary()) != 0 {
		t.Fatalf("empty cards should drop all: %+v", st.summary())
	}
}

func TestClarifyStoreLoadTurnsPendingIntoError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fleet-clarify.json")
	raw := `{"clarify":{"c1":{"tool":"claude","fleetSessionId":"s","forkId":"f","cwd":"/x","model":"opus","cardHash":"h1",` +
		`"turns":[{"role":"you","text":"q","at":"2026-10-26T12:00:00Z"}],"pending":{"kind":"ask","startedAt":"2026-10-26T12:00:00Z"}}}}`
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	st := &fleetClarifyStore{path: path}
	e, ok := st.get("c1")
	if !ok || e.Pending != nil || len(e.Turns) != 2 {
		t.Fatalf("loaded: %+v", e)
	}
	if last := e.Turns[1]; last.Role != "error" || last.Text != "interrupted by a restart" {
		t.Fatalf("last turn %+v", last)
	}
	if s := st.summary()["c1"]; s.Pending || s.Turns != 2 {
		t.Fatalf("summary %+v", s)
	}
	if st.running != 0 {
		t.Fatalf("running %d", st.running)
	}
	if _, _, code := st.begin("c1", "ask", nil, "again", nil, clarifyNow); code != "" {
		t.Fatalf("begin after load: %q", code)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v err %v", fi, err)
	}
	var f fleetClarifyFile
	data, _ := os.ReadFile(path)
	if err := json.Unmarshal(data, &f); err != nil || f.Clarify["c1"] == nil || f.Clarify["c1"].Pending == nil {
		t.Fatalf("saved file: %s (%v)", data, err)
	}
}

func TestClarifyStoreFinishAfterPruneIsNoop(t *testing.T) {
	st := &fleetClarifyStore{}
	if _, _, code := st.begin("c1", "ask", &clarifyEntry{}, "q", func() {}, clarifyNow); code != "" {
		t.Fatal(code)
	}
	st.prune(map[string]fleetDecisionCard{}, nil, clarifyNow)
	st.finish("c1", 0, "f1", &clarifyTurn{Role: "fleet", Text: "a", At: "x"}, nil, clarifyNow)
	if _, ok := st.get("c1"); ok {
		t.Fatal("entry came back")
	}
	if st.running != 0 {
		t.Fatalf("running %d after prune+finish", st.running)
	}
}

func TestClarifyStoreStaleFinishIgnored(t *testing.T) {
	st := &fleetClarifyStore{}
	cancelled := 0
	_, t1, code := st.begin("c1", "ask", &clarifyEntry{}, "q1", func() { cancelled++ }, clarifyNow)
	if code != "" {
		t.Fatal(code)
	}
	st.prune(map[string]fleetDecisionCard{}, nil, clarifyNow)
	if cancelled != 1 || st.running != 0 {
		t.Fatalf("after prune: cancelled %d running %d", cancelled, st.running)
	}
	_, t2, code := st.begin("c1", "ask", &clarifyEntry{}, "q2", nil, clarifyNow)
	if code != "" || t2 == t1 {
		t.Fatalf("second begin: %q tokens %d %d", code, t1, t2)
	}
	st.finish("c1", t1, "stale", &clarifyTurn{Role: "fleet", Text: "old", At: "x"}, nil, clarifyNow)
	e, _ := st.get("c1")
	if e.Pending == nil || len(e.Turns) != 1 || e.ForkID != "" || st.running != 1 {
		t.Fatalf("stale finish landed: %+v running %d", e, st.running)
	}
	st.finish("c1", t2, "f2", &clarifyTurn{Role: "fleet", Text: "new", At: "x"}, nil, clarifyNow)
	e, _ = st.get("c1")
	if e.Pending != nil || len(e.Turns) != 2 || e.Turns[1].Text != "new" || e.ForkID != "f2" || st.running != 0 {
		t.Fatalf("real finish: %+v running %d", e, st.running)
	}
}
