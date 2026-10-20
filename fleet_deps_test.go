package main

import (
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestFleetDependencies(t *testing.T) {
	state := map[string]json.RawMessage{"deps": json.RawMessage(`{
		"nodes": [
			{"id": "andy", "label": "  Andy  ", "kind": "person"},
			{"id": "#3049", "label": "#3049", "kind": "pr"},
			{"id": "train", "label": "dev train 03:00Z", "kind": "event"},
			{"id": "orphan", "label": "nobody waits", "kind": "ticket"},
			{"id": "bad kind", "label": "x", "kind": "person"},
			{"id": "robot", "label": "x", "kind": "robot"},
			{"id": "nolabel", "kind": "person"},
			{"id": "ctrl", "label": "a\nb", "kind": "person"},
			{"id": "long", "label": "` + strings.Repeat("é", 80) + `", "kind": "event"},
			{"id": "andy", "label": "Second Andy", "kind": "person"}
		],
		"edges": [
			{"from": "spawn-dr-5-a", "to": "andy", "label": "explicit yes"},
			{"from": "spawn-dr-5-a", "to": "andy", "label": "repeat"},
			{"from": "z-idle", "to": "spawn-dr-5-a"},
			{"from": "z-idle", "to": "#3049", "label": 7},
			{"from": "gone-session", "to": "andy"},
			{"from": "c-shell", "to": "missing#node"},
			{"from": "c-shell", "to": "c-shell"},
			{"from": "#3049", "to": "train"},
			{"from": "a-busy", "to": "long"},
			{"from": 5, "to": "andy"}
		]}`)}
	s := fleetTestServer(t, nil)
	rows := s.fleetWorkRows(workRows(), time.Now())
	g := fleetDependencies(state, rows)
	if g == nil {
		t.Fatal("deps must parse")
	}
	wantEdges := []fleetDepEdge{
		{From: "spawn-dr-5-a", To: "andy", Label: "explicit yes"},
		{From: "z-idle", To: "spawn-dr-5-a"},
		{From: "z-idle", To: "#3049"},
		{From: "#3049", To: "train"},
		{From: "a-busy", To: "long"},
	}
	if !reflect.DeepEqual(g.Edges, wantEdges) {
		t.Fatalf("edges = %+v", g.Edges)
	}
	ids := []string{}
	for _, n := range g.Nodes {
		ids = append(ids, n.ID)
	}
	if !reflect.DeepEqual(ids, []string{"andy", "#3049", "train", "long"}) {
		t.Fatalf("nodes = %v (only nodes a kept edge names, first entry of a repeat)", ids)
	}
	if g.Nodes[0].Label != "Andy" {
		t.Fatalf("label not trimmed: %q", g.Nodes[0].Label)
	}
	if n := []rune(g.Nodes[3].Label); len(n) != fleetDepLabelMax {
		t.Fatalf("long label has %d runes", len(n))
	}

	// A failed session list keeps session ends: it must not read as every
	// session ended.
	if g := fleetDependencies(state, nil); len(g.Edges) != 6 || g.Edges[2].From != "z-idle" {
		t.Fatalf("nil rows edges = %+v", g.Edges)
	}
	// An empty live list drops every session edge, and the nodes only they
	// named.
	if g := fleetDependencies(state, []fleetWorkRow{}); len(g.Edges) != 1 || len(g.Nodes) != 2 {
		t.Fatalf("no live rows: %+v", g)
	}
	for _, bad := range []string{"", `[1]`, `"x"`} {
		st := map[string]json.RawMessage{}
		if bad != "" {
			st["deps"] = json.RawMessage(bad)
		}
		if fleetDependencies(st, rows) != nil {
			t.Fatalf("deps %q must give nil", bad)
		}
	}
	if g := fleetDependencies(map[string]json.RawMessage{"deps": json.RawMessage(`{}`)}, rows); g == nil || g.Edges == nil || g.Nodes == nil {
		t.Fatalf("empty deps must give empty lists, got %+v", g)
	}
}

func TestFleetDependenciesCaps(t *testing.T) {
	var edges []string
	for i := 0; i < fleetDepEdgesMax+50; i++ {
		edges = append(edges, `{"from":"s`+strings.Repeat("1", i%7+1)+string(rune('a'+i%26))+strings.Repeat("z", i/26)+`","to":"t"}`)
	}
	raw := `{"edges":[` + strings.Join(edges, ",") + `]}`
	g := fleetDependencies(map[string]json.RawMessage{"deps": json.RawMessage(raw)}, nil)
	if len(g.Edges) != fleetDepEdgesMax {
		t.Fatalf("edges = %d, want the first %d", len(g.Edges), fleetDepEdgesMax)
	}
	// Past fleetDepNodesMax a node is dropped, so an edge to it is not an
	// edge to a node; with "#" in the id it is not a session name either.
	var nodes []string
	edges = nil
	for i := 0; i < fleetDepNodesMax+20; i++ {
		id := "#" + strconv.Itoa(i)
		nodes = append(nodes, `{"id":"`+id+`","label":"L","kind":"pr"}`)
		edges = append(edges, `{"from":"s","to":"`+id+`"}`)
	}
	raw = `{"nodes":[` + strings.Join(nodes, ",") + `],"edges":[` + strings.Join(edges, ",") + `]}`
	g = fleetDependencies(map[string]json.RawMessage{"deps": json.RawMessage(raw)}, nil)
	if len(g.Nodes) != fleetDepNodesMax || len(g.Edges) != fleetDepNodesMax {
		t.Fatalf("nodes = %d, edges = %d, want %d each", len(g.Nodes), len(g.Edges), fleetDepNodesMax)
	}
}

func TestFleetDependenciesStory(t *testing.T) {
	long := strings.Repeat("x", fleetDepStoryLen+20)
	raw := `{"story":["  Four dev checks wait for the dev queue. ", 7, "bad\u0007", "", "` + long + `", "third kept line", "past the cap"]}`
	g := fleetDependencies(map[string]json.RawMessage{"deps": json.RawMessage(raw)}, nil)
	want := []string{"Four dev checks wait for the dev queue.", strings.Repeat("x", fleetDepStoryLen), "third kept line"}
	if len(g.Story) != len(want) {
		t.Fatalf("story = %q, want %q", g.Story, want)
	}
	for i := range want {
		if g.Story[i] != want[i] {
			t.Fatalf("story[%d] = %q, want %q", i, g.Story[i], want[i])
		}
	}
	if g = fleetDependencies(map[string]json.RawMessage{"deps": json.RawMessage(`{}`)}, nil); g.Story != nil {
		t.Fatalf("no story must stay nil, got %q", g.Story)
	}
}

func TestFleetStateServesDeps(t *testing.T) {
	s := fleetTestServer(t, []Session{
		{PID: 20, SessionID: "a", Status: "busy", Tmux: "a-busy:0.0"},
		{PID: 21, SessionID: "b", Status: "idle", Tmux: "b-idle:0.0"},
	})
	writeFleetFile(t, s, "state.json", `{"version":1,"cards":[],"deps":{
		"nodes":[{"id":"andy","label":"Andy","kind":"person"}],
		"edges":[{"from":"a-busy","to":"b-idle"},{"from":"b-idle","to":"andy"},{"from":"ended-one","to":"andy"}]}}`)
	out := getFleetJSON(t, s, "/fleet/state")
	deps, ok := out["deps"].(map[string]any)
	if !ok {
		t.Fatalf("deps = %v", out["deps"])
	}
	if edges := deps["edges"].([]any); len(edges) != 2 {
		t.Fatalf("served edges = %v (the ended session's edge must go)", edges)
	}
	writeFleetFile(t, s, "state.json", `{"version":1,"cards":[]}`)
	if out := getFleetJSON(t, s, "/fleet/state"); out["deps"] != nil {
		t.Fatalf("no deps must serve null, got %v", out["deps"])
	}
}
