package main

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestFleetCaps(t *testing.T) {
	for _, c := range []struct {
		name string
		caps string
		want map[string]int
	}{
		{"missing", ``, nil},
		{"null", `null`, nil},
		{"not an object", `[8, 12]`, nil},
		{"empty", `{}`, nil},
		{"valid", `{"Fable": 8, "Opus": 12, "Grok": 32}`, map[string]int{"Fable": 8, "Opus": 12, "Grok": 32}},
		{"bad entries drop", `{"Fable": 8, "Opus": 0, "Grok": -1, "Haiku": 2.5, "Sonnet": "12", "X": null, "Big": 1001, "bad\nkey": 3, "": 4}`,
			map[string]int{"Fable": 8}},
		{"none valid", `{"Fable": 0}`, nil},
	} {
		st := map[string]json.RawMessage{}
		if c.caps != "" {
			st["caps"] = json.RawMessage(c.caps)
		}
		if got := fleetCaps(st); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: caps = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestFleetStateServesCaps(t *testing.T) {
	s := fleetTestServer(t, nil)
	writeFleetFile(t, s, "state.json", `{"version":1,"cards":[],"caps":{"Fable":8,"Opus":12,"Grok":"32"}}`)
	caps, ok := getFleetJSON(t, s, "/fleet/state")["caps"].(map[string]any)
	if !ok {
		t.Fatal("caps must serve as an object")
	}
	if !reflect.DeepEqual(caps, map[string]any{"Fable": 8.0, "Opus": 12.0}) {
		t.Fatalf("caps = %v (the string Grok cap must drop)", caps)
	}
	// Bad caps never pass through raw: the server's own key replaces FLEET's.
	writeFleetFile(t, s, "state.json", `{"version":1,"cards":[],"caps":{"Fable":0}}`)
	if out := getFleetJSON(t, s, "/fleet/state"); out["caps"] != nil {
		t.Fatalf("no valid cap must serve null, got %v", out["caps"])
	}
	writeFleetFile(t, s, "state.json", `{"version":1,"cards":[]}`)
	if out := getFleetJSON(t, s, "/fleet/state"); out["caps"] != nil {
		t.Fatalf("no caps must serve null, got %v", out["caps"])
	}
}
