package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestFleetHolds(t *testing.T) {
	long := strings.Repeat("é", fleetHoldReasonMax+10)
	for _, c := range []struct {
		name  string
		holds string
		want  map[string]string
	}{
		{"missing", ``, nil},
		{"null", `null`, nil},
		{"not an object", `["Fable"]`, nil},
		{"empty", `{}`, nil},
		{"valid", `{"Fable": "no new spawns", "Opus": "quota"}`, map[string]string{"Fable": "no new spawns", "Opus": "quota"}},
		{"folded to one line", `{"Fable": "  no new\nspawns;\t cx:high\u001b[31m held  "}`, map[string]string{"Fable": "no new spawns; cx:high[31m held"}},
		{"cut to the max", `{"Fable": "` + long + `"}`, map[string]string{"Fable": strings.Repeat("é", fleetHoldReasonMax)}},
		{"bad entries drop", `{"Fable": "ok", "Opus": "", "Grok": "   ", "Haiku": 1, "X": null, "Y": true, "bad\nkey": "r", "": "r"}`,
			map[string]string{"Fable": "ok"}},
		{"none valid", `{"Fable": ""}`, nil},
	} {
		st := map[string]json.RawMessage{}
		if c.holds != "" {
			st["holds"] = json.RawMessage(c.holds)
		}
		if got := fleetHolds(st); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: holds = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestFleetStateServesHolds(t *testing.T) {
	s := fleetTestServer(t, nil)
	writeFleetFile(t, s, "state.json", `{"version":1,"cards":[],"holds":{"Fable":" no new spawns ","Opus":3}}`)
	holds, ok := getFleetJSON(t, s, "/fleet/state")["holds"].(map[string]any)
	if !ok {
		t.Fatal("holds must serve as an object")
	}
	if !reflect.DeepEqual(holds, map[string]any{"Fable": "no new spawns"}) {
		t.Fatalf("holds = %v (the number Opus hold must drop)", holds)
	}
	// Bad holds never pass through raw: the server's own key replaces FLEET's.
	writeFleetFile(t, s, "state.json", `{"version":1,"cards":[],"holds":{"Fable":""}}`)
	if out := getFleetJSON(t, s, "/fleet/state"); out["holds"] != nil {
		t.Fatalf("no valid hold must serve null, got %v", out["holds"])
	}
	writeFleetFile(t, s, "state.json", `{"version":1,"cards":[],"holds":{}}`)
	if out := getFleetJSON(t, s, "/fleet/state"); out["holds"] != nil {
		t.Fatalf("an empty holds must serve null, got %v", out["holds"])
	}
}

func TestDecodeFleetStateViewHolds(t *testing.T) {
	v, err := decodeFleetStateView([]byte(`{"holds":{"Fable":"no new\nspawns \u001b[31mnow","Opus":7,"Grok":""}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(v.Holds, map[string]string{"Fable": "no new spawns now"}) {
		t.Fatalf("holds = %q", v.Holds)
	}
	for _, body := range []string{`{}`, `{"holds":null}`, `{"holds":[]}`, `{"holds":{"Fable":1}}`} {
		if v, _ := decodeFleetStateView([]byte(body)); v.Holds != nil {
			t.Errorf("%s: holds = %q, want nil", body, v.Holds)
		}
	}
}

func TestFleetHoldLine(t *testing.T) {
	if l := fleetHoldLine(nil, 80); l != "" {
		t.Errorf("no hold = %q, want empty", l)
	}
	holds := map[string]string{"Zeta": "z", "Grok": "g", "Fable": "no new spawns"}
	if got, want := stripANSI(fleetHoldLine(holds, 200)), "Fable HOLD — no new spawns · Grok HOLD — g · Zeta HOLD — z"; got != want {
		t.Errorf("line = %q, want %q", got, want)
	}
	if l := fleetHoldLine(holds, 200); !strings.HasPrefix(l, "\x1b["+fleetSGRAmber+"m") {
		t.Errorf("hold line must be amber: %q", l)
	}
	if got := stripANSI(fleetHoldLine(holds, 20)); got != "Fable HOLD — no new…" {
		t.Errorf("cut line = %q", got)
	}
}

func TestRenderFleetViewHoldLine(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	v := fleetRenderView(now)
	plain := fleetRendered(fleetTestState(v, now), 100, 40, now)
	if strings.Contains(plain, "HOLD —") {
		t.Fatalf("no hold must draw no hold line:\n%s", plain)
	}
	v.Holds = map[string]string{"Fable": "no new spawns; cx:high held"}
	out := fleetRendered(fleetTestState(v, now), 100, 40, now)
	lines := strings.Split(out, "\n")
	if len(lines) != 40 {
		t.Fatalf("frame has %d lines, want 40", len(lines))
	}
	if !strings.HasPrefix(lines[0], "FLEET ● idle") || lines[1] != "Fable HOLD — no new spawns; cx:high held" || !strings.HasPrefix(lines[2], "───") {
		t.Fatalf("hold line must sit between the header and the rule:\n%s", strings.Join(lines[:4], "\n"))
	}
	// The card list still draws, one row lower.
	if !strings.Contains(out, "▸ DR-5241") {
		t.Errorf("card list lost:\n%s", out)
	}
}
