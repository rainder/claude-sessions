package main

import (
	"encoding/json"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// The FLEET page's dependency graph. FLEET writes state.json's top-level
// "deps": external nodes (a person, a PR, an event, a ticket) and waits-for
// edges, where "from" waits for "to". An edge end is an external node's id,
// or else a tmux session name. The server cleans the graph and drops every
// edge whose session end has no live row, so an edge FLEET left behind for
// an ended session never shows. The page joins session ends with the
// working list for their status and model.

const (
	fleetDepNodesMax = 100
	fleetDepEdgesMax = 200
	fleetDepLabelMax = 60  // runes
	fleetDepStoryMax = 3   // lines
	fleetDepStoryLen = 160 // runes per line
)

// fleetDepNodeIDRe: a node id. "#" is allowed so a PR node can use its
// number as its id; a tmux session name never holds "#".
var fleetDepNodeIDRe = regexp.MustCompile(`^[A-Za-z0-9._#-]{1,100}$`)

var fleetDepKinds = map[string]bool{"person": true, "pr": true, "event": true, "ticket": true}

type fleetDepNode struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Kind  string `json:"kind"`
}

type fleetDepEdge struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Label string `json:"label,omitempty"`
}

// fleetDepGraph is what /fleet/state serves as "deps". Nodes holds only the
// external nodes a kept edge names; a session end is not a node here.
// Story is FLEET's own plain-words reading of the graph: what is actually
// happening and what would unblock it, not a repeat of the edges.
// GeneratedAt is when FLEET built the graph (see fleetDepsGeneratedAt).
type fleetDepGraph struct {
	Nodes       []fleetDepNode `json:"nodes"`
	Edges       []fleetDepEdge `json:"edges"`
	Story       []string       `json:"story,omitempty"`
	GeneratedAt string         `json:"generatedAt,omitempty"`
}

// fleetDepsGeneratedAt is the age stamp of the graph, in UTC RFC 3339:
// state.json's depsAt, which fleet-state.py moves only when deps changes or
// FLEET rebuilt it. A state from before depsAt falls back to writtenAt, the
// last publish. Empty when neither is a valid time.
func fleetDepsGeneratedAt(state map[string]json.RawMessage) string {
	for _, k := range []string{"depsAt", "writtenAt"} {
		var at string
		if json.Unmarshal(state[k], &at) != nil {
			continue
		}
		if t, err := time.Parse(time.RFC3339, at); err == nil {
			return t.UTC().Format(time.RFC3339)
		}
	}
	return ""
}

// fleetDepLabel cleans one label: trimmed, cut to fleetDepLabelMax runes.
// ok is false for a value that is not a string or holds a control
// character.
func fleetDepLabel(raw json.RawMessage) (string, bool) {
	return fleetDepText(raw, fleetDepLabelMax)
}

// fleetDepText cleans one string: trimmed, cut to max runes. ok is false for
// a value that is not a string or holds a control character.
func fleetDepText(raw json.RawMessage, max int) (string, bool) {
	if raw == nil {
		return "", true
	}
	var s string
	if json.Unmarshal(raw, &s) != nil || !utf8.ValidString(s) || strings.IndexFunc(s, unicode.IsControl) >= 0 {
		return "", false
	}
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > max {
		s = strings.TrimSpace(string([]rune(s)[:max]))
	}
	return s, true
}

// fleetDependencies reads state.json's "deps" and keeps what the page can
// draw. A bad node (id, kind, label) or a bad edge (an end that is neither a
// node nor a session name, a self-loop, a repeat) is dropped, not the graph.
// With rows nil (the session list failed) session ends are kept: a failed
// list must not read as "every session ended". nil when there is no deps
// object.
func fleetDependencies(state map[string]json.RawMessage, rows []fleetWorkRow) *fleetDepGraph {
	var raw struct {
		Nodes []map[string]json.RawMessage `json:"nodes"`
		Edges []map[string]json.RawMessage `json:"edges"`
		Story []json.RawMessage            `json:"story"`
	}
	if state["deps"] == nil || json.Unmarshal(state["deps"], &raw) != nil {
		return nil
	}
	nodes := map[string]fleetDepNode{}
	for _, n := range raw.Nodes {
		if len(nodes) >= fleetDepNodesMax {
			break
		}
		var id, kind string
		if json.Unmarshal(n["id"], &id) != nil || !fleetDepNodeIDRe.MatchString(id) {
			continue
		}
		if json.Unmarshal(n["kind"], &kind) != nil || !fleetDepKinds[kind] {
			continue
		}
		label, ok := fleetDepLabel(n["label"])
		if !ok || label == "" {
			continue
		}
		if _, dup := nodes[id]; !dup {
			nodes[id] = fleetDepNode{ID: id, Label: label, Kind: kind}
		}
	}
	var live map[string]bool
	if rows != nil {
		live = make(map[string]bool, len(rows))
		for _, r := range rows {
			live[r.Tmux] = true
		}
	}
	// end reports whether one edge end can be drawn now.
	end := func(id string) bool {
		if _, ok := nodes[id]; ok {
			return true
		}
		return fleetSessionNameRe.MatchString(id) && (live == nil || live[id])
	}
	g := &fleetDepGraph{Nodes: []fleetDepNode{}, Edges: []fleetDepEdge{}, GeneratedAt: fleetDepsGeneratedAt(state)}
	seen := map[[2]string]bool{}
	used := map[string]bool{}
	for i, e := range raw.Edges {
		if i >= fleetDepEdgesMax {
			break
		}
		var from, to string
		if json.Unmarshal(e["from"], &from) != nil || json.Unmarshal(e["to"], &to) != nil {
			continue
		}
		label, ok := fleetDepLabel(e["label"])
		if !ok {
			label = ""
		}
		k := [2]string{from, to}
		if from == to || seen[k] || !end(from) || !end(to) {
			continue
		}
		seen[k] = true
		used[from], used[to] = true, true
		g.Edges = append(g.Edges, fleetDepEdge{From: from, To: to, Label: label})
	}
	for _, line := range raw.Story {
		if len(g.Story) >= fleetDepStoryMax {
			break
		}
		if t, ok := fleetDepText(line, fleetDepStoryLen); ok && t != "" {
			g.Story = append(g.Story, t)
		}
	}
	// Nodes in FLEET's order, only those a kept edge names.
	for _, n := range raw.Nodes {
		var id string
		if json.Unmarshal(n["id"], &id) != nil || !used[id] {
			continue
		}
		if node, ok := nodes[id]; ok {
			g.Nodes = append(g.Nodes, node)
			delete(used, id)
		}
	}
	return g
}
