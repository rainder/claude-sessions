package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFleetStateLiveSessions(t *testing.T) {
	idleAt := time.Date(2026, 10, 13, 9, 50, 0, 0, time.UTC)
	s := fleetTestServer(t, []Session{
		{PID: 7, Name: "FLEET", SessionID: "sid", Status: "busy", Tmux: "fleet-main:0.0"},
		{PID: 10, SessionID: "a", Status: "busy", Tmux: "work-busy:0.0", UpdatedAt: idleAt.UnixMilli()},
		{PID: 11, SessionID: "b", Status: "idle", Tmux: "work-idle:0.0", UpdatedAt: idleAt.UnixMilli()},
		{PID: 12, SessionID: "c", Status: "busy", WaitingFor: "permission prompt", Tmux: "work-wait:0.0", UpdatedAt: idleAt.UnixMilli()},
		{PID: 13, SessionID: "d", Status: "shell", Tmux: "work-shell:0.0"},
		{PID: 14, SessionID: "e", Status: "idle", Tmux: "work-two:0.0"},
		{PID: 15, SessionID: "f", Status: "idle", Tmux: "work-two:1.0"},
		{PID: 16, SessionID: "g", Status: "idle", Tmux: "work-remote:0.0", Host: "pi"},
	})
	writeFleetFile(t, s, "state.json", `{"version":1,"writtenAt":"2026-10-13T09:40:00Z","cards":[
		{"id":"1","kind":"working","session":"work-busy"},
		{"id":"2","kind":"working","session":"work-idle:0.0"},
		{"id":"3","kind":"waiting","session":"work-wait"},
		{"id":"4","kind":"working","session":"work-shell"},
		{"id":"5","kind":"working","session":"work-two"},
		{"id":"6","kind":"working","session":"work-ended"},
		{"id":"7","kind":"working","session":"work-remote"},
		{"id":"8","kind":"working","session":"bad name;rm"},
		{"id":"9","kind":"working","session":7},
		{"id":"10","kind":"working"}
	],"liveSessions":"spoof","stateFileAt":"spoof"}`)
	out := getFleetJSON(t, s, "/fleet/state")
	live, ok := out["liveSessions"].(map[string]any)
	if !ok {
		t.Fatalf("liveSessions = %v (server keys must overwrite file keys)", out["liveSessions"])
	}
	want := map[string][2]string{
		"work-busy":   {"busy", ""},
		"work-idle":   {"idle", "2026-10-13T09:50:00Z"},
		"work-wait":   {"waiting", "2026-10-13T09:50:00Z"},
		"work-shell":  {"shell", ""},
		"work-two":    {"ambiguous", ""},
		"work-ended":  {"gone", ""},
		"work-remote": {"gone", ""},
	}
	if len(live) != len(want) {
		t.Fatalf("liveSessions has %d names, want %d: %v", len(live), len(want), live)
	}
	for name, w := range want {
		e, _ := live[name].(map[string]any)
		since, _ := e["since"].(string)
		if e["status"] != w[0] || since != w[1] {
			t.Errorf("%s = %v, want status %q since %q", name, e, w[0], w[1])
		}
	}
	at, _ := out["stateFileAt"].(string)
	if _, err := time.Parse(time.RFC3339, at); err != nil {
		t.Fatalf("stateFileAt = %v", out["stateFileAt"])
	}
}

func TestFleetStateFileAtIsTheFileMtime(t *testing.T) {
	s := fleetTestServer(t, nil)
	writeFleetFile(t, s, "state.json", `{"version":1,"cards":[]}`)
	mt := time.Date(2026, 10, 13, 8, 15, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(s.fleet.dir, "state.json"), mt, mt); err != nil {
		t.Fatal(err)
	}
	if out := getFleetJSON(t, s, "/fleet/state"); out["stateFileAt"] != "2026-10-13T08:15:00Z" {
		t.Fatalf("stateFileAt = %v", out["stateFileAt"])
	}
	// A bad rewrite keeps the last good copy, and its time.
	writeFleetFile(t, s, "state.json", `{not json`)
	if out := getFleetJSON(t, s, "/fleet/state"); out["stateFileAt"] != "2026-10-13T08:15:00Z" {
		t.Fatalf("stateFileAt after a bad write = %v", out["stateFileAt"])
	}
}

func TestFleetStateNoFileHasNoFileAt(t *testing.T) {
	s := fleetTestServer(t, nil)
	if out := getFleetJSON(t, s, "/fleet/state"); out["stateFileAt"] != nil {
		t.Fatalf("stateFileAt = %v", out["stateFileAt"])
	}
}

func TestFleetStateListFailureIsNotGone(t *testing.T) {
	s := fleetTestServer(t, nil)
	s.collect = func() ([]Session, error) { return nil, errors.New("ps failed") }
	writeFleetFile(t, s, "state.json", `{"version":1,"cards":[{"id":"1","kind":"working","session":"work-a"}]}`)
	if out := getFleetJSON(t, s, "/fleet/state"); out["liveSessions"] != nil {
		t.Fatalf("liveSessions = %v, want null when the list failed", out["liveSessions"])
	}
}

func TestFleetStateEmptyListIsGone(t *testing.T) {
	s := fleetTestServer(t, nil)
	writeFleetFile(t, s, "state.json", `{"version":1,"cards":[{"id":"1","kind":"working","session":"work-a"}]}`)
	live, _ := getFleetJSON(t, s, "/fleet/state")["liveSessions"].(map[string]any)
	if e, _ := live["work-a"].(map[string]any); e["status"] != "gone" {
		t.Fatalf("liveSessions = %v, want work-a gone", live)
	}
}
