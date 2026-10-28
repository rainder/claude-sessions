package main

import (
	"encoding/json"
	"reflect"
	"regexp"
	"sort"
	"testing"
)

func TestFleetCardViewLenient(t *testing.T) {
	var c fleetCardView
	if err := json.Unmarshal([]byte(`{"id":5,"kind":"needs-you","title":["x"],"options":"no"}`), &c); err != nil {
		t.Fatal(err)
	}
	if c.ID != "5" || c.Title != "" || c.Options != nil {
		t.Fatalf("got %+v", c)
	}

	var d fleetCardView
	if err := json.Unmarshal([]byte(`{"options":[1,{"key":"a","label":"A"}]}`), &d); err != nil {
		t.Fatal(err)
	}
	want := []fleetOptionView{{Key: "a", Label: "A"}}
	if !reflect.DeepEqual(d.Options, want) {
		t.Fatalf("options = %+v, want %+v", d.Options, want)
	}

	var e fleetCardView
	if err := e.UnmarshalJSON([]byte(`[1]`)); err == nil {
		t.Fatal("array must be an error")
	}
}

func cardFromJSON(t *testing.T, s string) fleetCardView {
	t.Helper()
	var c fleetCardView
	if err := json.Unmarshal([]byte(s), &c); err != nil {
		t.Fatal(err)
	}
	return c
}

const baseCardJSON = `{"id":"a","kind":"needs-you","status":"open","title":"T","ticket":"DR-1",
"summary":"S","lean":"L","updatedAt":"2026-10-22T10:00:00Z","session":"s1","links":["x"],"waitingOn":"w",
"waiting":{"who":"Rinoj","since":"2026-10-28T08:33:00Z"},
"options":[{"key":"k","label":"Lab","line":"Ln","send":"Snd"}]}`

func TestFleetCardHashIgnoresVolatileFields(t *testing.T) {
	base := fleetCardHash(cardFromJSON(t, baseCardJSON))
	if !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(base) {
		t.Fatalf("hash %q", base)
	}
	for _, field := range []string{"updatedAt", "id", "session", "links", "waitingOn", "waiting"} {
		m := map[string]any{}
		if err := json.Unmarshal([]byte(baseCardJSON), &m); err != nil {
			t.Fatal(err)
		}
		m[field] = "changed"
		b, _ := json.Marshal(m)
		if got := fleetCardHash(cardFromJSON(t, string(b))); got != base {
			t.Errorf("%s changed the hash", field)
		}
	}
}

func TestFleetCardHashChangesOnEachField(t *testing.T) {
	base := fleetCardHash(cardFromJSON(t, baseCardJSON))
	muts := map[string]func(m map[string]any){
		"kind":    func(m map[string]any) { m["kind"] = "explicit-yes" },
		"status":  func(m map[string]any) { m["status"] = "x" },
		"title":   func(m map[string]any) { m["title"] = "x" },
		"ticket":  func(m map[string]any) { m["ticket"] = "x" },
		"summary": func(m map[string]any) { m["summary"] = "x" },
		"lean":    func(m map[string]any) { m["lean"] = "x" },
		"key":     func(m map[string]any) { m["options"].([]any)[0].(map[string]any)["key"] = "x" },
		"label":   func(m map[string]any) { m["options"].([]any)[0].(map[string]any)["label"] = "x" },
		"line":    func(m map[string]any) { m["options"].([]any)[0].(map[string]any)["line"] = "x" },
		"send":    func(m map[string]any) { m["options"].([]any)[0].(map[string]any)["send"] = "x" },
		"added": func(m map[string]any) {
			m["options"] = append(m["options"].([]any), map[string]any{"key": "z"})
		},
	}
	for name, mut := range muts {
		m := map[string]any{}
		if err := json.Unmarshal([]byte(baseCardJSON), &m); err != nil {
			t.Fatal(err)
		}
		mut(m)
		b, _ := json.Marshal(m)
		if got := fleetCardHash(cardFromJSON(t, string(b))); got == base {
			t.Errorf("%s did not change the hash", name)
		}
	}
}

func TestFleetDecisionCardsFirstIDWins(t *testing.T) {
	state := map[string]json.RawMessage{
		"cards": json.RawMessage(`[
			{"id":"a","kind":"needs-you","title":"first"},
			{"id":"a","kind":"explicit-yes","title":"second"},
			{"id":"b","kind":"done","title":"done"},
			{"kind":"needs-you","title":"no id"},
			7
		]`),
	}
	got := fleetDecisionCards(state)
	if len(got) != 1 || got["a"].Card.Title != "first" {
		t.Fatalf("got %+v", got)
	}
	hashes := fleetCardHashes(got)
	if len(hashes) != 1 || hashes["a"] != got["a"].Hash || hashes["a"] == "" {
		t.Fatalf("hashes %+v", hashes)
	}
	keys := []string{}
	for k := range got {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if !reflect.DeepEqual(keys, []string{"a"}) {
		t.Fatalf("keys %v", keys)
	}
	if fleetDecisionCards(nil) == nil || fleetDecisionCards(map[string]json.RawMessage{"cards": json.RawMessage(`"x"`)}) == nil {
		t.Fatal("must never be nil")
	}
	if fleetCardHashes(nil) == nil {
		t.Fatal("hashes must never be nil")
	}
}

func TestFleetOptionLabel(t *testing.T) {
	c := fleetCardView{Options: []fleetOptionView{
		{Key: "a", Label: "A", Send: "sa"},
		{Key: "b", Send: "sb"},
		{Key: "c"},
	}}
	cases := []struct {
		key, label, send string
		ok               bool
	}{
		{"a", "A", "sa", true},
		{"b", "sb", "sb", true},
		{"c", "c", "", true},
		{"zz", "", "", false},
	}
	for _, tc := range cases {
		label, send, ok := fleetOptionLabel(c, tc.key)
		if label != tc.label || send != tc.send || ok != tc.ok {
			t.Errorf("%s: got %q %q %v", tc.key, label, send, ok)
		}
	}
}

func TestFleetCardViewDecodesPriority(t *testing.T) {
	for _, c := range []struct {
		raw  string
		want int
	}{
		{`{"id":"x"}`, 3},
		{`{"priority":1}`, 1},
		{`{"priority":2}`, 2},
		{`{"priority":4}`, 4},
		{`{"priority":0}`, 3},
		{`{"priority":5}`, 3},
		{`{"priority":"2"}`, 3},
		{`{"priority":2.5}`, 3},
		{`{"priority":true}`, 3},
		{`{"priority":null}`, 3},
	} {
		var v fleetCardView
		if err := json.Unmarshal([]byte(c.raw), &v); err != nil {
			t.Fatalf("%s: %v", c.raw, err)
		}
		if v.Priority != c.want {
			t.Errorf("%s: priority %d, want %d", c.raw, v.Priority, c.want)
		}
	}
	var v fleetCardView
	_ = json.Unmarshal([]byte(`{"firstSeenAt":"2026-10-24T10:00:00Z"}`), &v)
	if v.FirstSeenAt != "2026-10-24T10:00:00Z" {
		t.Fatalf("firstSeenAt %q", v.FirstSeenAt)
	}
}

// Priority and firstSeenAt order the cards; they do not change what a card
// asks, so an answer recorded before FLEET set them still stands.
func TestFleetCardHashIgnoresPriorityAndFirstSeen(t *testing.T) {
	var a, b fleetCardView
	_ = json.Unmarshal([]byte(`{"kind":"needs-you","title":"t"}`), &a)
	_ = json.Unmarshal([]byte(`{"kind":"needs-you","title":"t","priority":1,"firstSeenAt":"2026-10-24T10:00:00Z"}`), &b)
	if fleetCardHash(a) != fleetCardHash(b) {
		t.Fatal("hash moved with priority or firstSeenAt")
	}
}
