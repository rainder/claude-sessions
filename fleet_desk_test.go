package main

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestFindNamedSession(t *testing.T) {
	chat := Session{PID: 1, Name: "CHAT", NameSource: "user", SessionID: "c"}
	grok := Session{PID: 2, Name: "CHAT", NameSource: "user", Tool: toolGrok, SessionID: "g"}
	derived := Session{PID: 3, Name: "CHAT", NameSource: "derived", SessionID: "d"}
	other := Session{PID: 4, Name: "FLEET", NameSource: "user", SessionID: "f"}
	cases := []struct {
		name     string
		sessions []Session
		pid      int
		reason   string
	}{
		{"none", []Session{other}, 0, "not running"},
		{"nil list", nil, 0, "not running"},
		{"one", []Session{other, chat}, 1, ""},
		{"two", []Session{chat, {PID: 5, Name: "CHAT", SessionID: "e"}}, 0, "ambiguous"},
		{"claude and grok", []Session{chat, grok}, 0, "ambiguous"},
		{"derived name ignored", []Session{derived, chat}, 1, ""},
		{"only derived", []Session{derived}, 0, "not running"},
		{"grok user-set", []Session{grok, derived}, 2, ""},
		{"another tool", []Session{{PID: 6, Name: "CHAT", Tool: "codex"}}, 0, "not running"},
	}
	for _, c := range cases {
		got, reason := findNamedSession(c.sessions, "CHAT")
		if reason != c.reason || got.PID != c.pid {
			t.Errorf("%s: %+v %q, want pid %d %q", c.name, got, reason, c.pid, c.reason)
		}
	}
	// findFleet is findNamedSession on FLEET.
	if got, reason := findFleet([]Session{chat, other}); reason != "" || got.PID != 4 {
		t.Fatalf("findFleet: %+v %q", got, reason)
	}
}

// deskRows is the session fixture plus the two desks, each disabled for
// FLEET (Andy's desks are not FLEET's to manage) and on no card: CHAT in tmux
// session "desk", SLACK in "slack-triage-c4a100".
func deskRows() []Session {
	return append(fleetSessionRows(fleetUUID(120)),
		Session{PID: 20, Name: "CHAT", NameSource: "user", SessionID: deskSID, Status: "idle", Tmux: "desk:0.0"},
		Session{PID: 40, Name: "SLACK", NameSource: "user", SessionID: slackSID, Status: "busy", Tmux: "slack-triage-c4a100:0.0"})
}

const (
	deskSID  = "desk-sid"
	slackSID = "slack-sid"
)

func deskDisabled(sid string) bool {
	return sid == deskSID || sid == slackSID || sid == fleetDisabledSID
}

// deskViews is /fleet/state's desks for rows, by name, after checking they
// come in fleetDeskSessionNames order and that the old "desk" is CHAT's.
func deskViews(t *testing.T, rows []Session) map[string]map[string]any {
	t.Helper()
	s := fleetTestServer(t, rows)
	s.fleet.disabled = deskDisabled
	st := getFleetJSON(t, s, "/fleet/state")
	list, ok := st["desks"].([]any)
	if !ok || len(list) != len(fleetDeskSessionNames) {
		t.Fatalf("desks: %v", st["desks"])
	}
	out := map[string]map[string]any{}
	for k, x := range list {
		d, ok := x.(map[string]any)
		if !ok || d["name"] != fleetDeskSessionNames[k] {
			t.Fatalf("desks[%d]: %v", k, x)
		}
		out[fleetDeskSessionNames[k]] = d
	}
	if old, ok := st["desk"].(map[string]any); !ok || !reflect.DeepEqual(old, out["CHAT"]) {
		t.Fatalf("desk %v, want CHAT's %v", st["desk"], out["CHAT"])
	}
	return out
}

func TestFleetStateDesks(t *testing.T) {
	both := deskViews(t, deskRows())
	if d := both["CHAT"]; d["alive"] != true || d["tmux"] != "desk" || d["status"] != "idle" || d["tool"] != "claude" || d["reason"] != nil {
		t.Fatalf("CHAT alive: %v", d)
	}
	if d := both["SLACK"]; d["alive"] != true || d["tmux"] != "slack-triage-c4a100" || d["status"] != "busy" || d["tool"] != "claude" || d["reason"] != nil {
		t.Fatalf("SLACK alive: %v", d)
	}
	// One desk missing: the other is still usable, and the missing one says
	// why.
	slackOnly := append(fleetSessionRows(fleetUUID(128)), Session{PID: 41, Name: "SLACK", NameSource: "user", SessionID: slackSID, Status: "idle", Tmux: "slack-triage-c4a100:0.0"})
	one := deskViews(t, slackOnly)
	if one["CHAT"]["alive"] != false || one["CHAT"]["reason"] != "not running" || one["SLACK"]["alive"] != true {
		t.Fatalf("SLACK only: %v", one)
	}
	// Both desks in one tmux session: neither is usable.
	shared := append(fleetSessionRows(fleetUUID(129)),
		Session{PID: 42, Name: "CHAT", NameSource: "user", SessionID: deskSID, Tmux: "desks:0.0"},
		Session{PID: 43, Name: "SLACK", NameSource: "user", SessionID: slackSID, Tmux: "desks:1.0"})
	for name, d := range deskViews(t, shared) {
		if d["alive"] != false || d["reason"] != "shares tmux" {
			t.Errorf("one tmux, %s: %v", name, d)
		}
	}
	// No desk at all: both listed, both not running.
	for name, d := range deskViews(t, fleetSessionRows(fleetUUID(132))) {
		if d["alive"] != false || d["reason"] != "not running" {
			t.Errorf("none, %s: %v", name, d)
		}
	}
	// A failed list is not usable either, and never a 500.
	for _, d := range fleetDesks(nil) {
		if d.Alive || d.Reason != "session list failed" {
			t.Fatalf("list failed: %+v", d)
		}
	}
}

// TestFleetStateDeskGuards holds every desk to the same guards.
func TestFleetStateDeskGuards(t *testing.T) {
	for _, name := range fleetDeskSessionNames {
		row := func(pid int, tmux string) Session {
			return Session{PID: pid, Name: name, NameSource: "user", SessionID: "x", Tmux: tmux}
		}
		grok := append(fleetSessionRows(fleetUUID(121)), Session{PID: 21, Name: name, NameSource: "user", Tool: toolGrok, SessionID: "g", Status: "busy", Tmux: "gdesk:0.0"})
		if d := deskViews(t, grok)[name]; d["alive"] != true || d["tool"] != "grok" || d["status"] != "busy" || d["tmux"] != "gdesk" {
			t.Fatalf("%s grok: %v", name, d)
		}
		remote := row(24, "rdesk:0.0")
		remote.Host = "ws"
		down := []struct {
			name   string
			rows   []Session
			reason string
		}{
			{"not running", fleetSessionRows(fleetUUID(122)), "not running"},
			{"ambiguous", append(fleetSessionRows(fleetUUID(122)), row(22, "d1:0.0"), row(23, "d2:0.0")), "ambiguous"},
			{"no tmux", append(fleetSessionRows(fleetUUID(123)), row(23, "")), "not in tmux"},
			{"remote", append(fleetSessionRows(fleetUUID(124)), remote), "remote"},
			{"FLEET's tmux", append(fleetSessionRows(fleetUUID(125)), row(25, "fleet:1.0")), "in FLEET's tmux"},
			{"bad tmux name", append(fleetSessionRows(fleetUUID(126)), row(26, "my desk:0.0")), "bad tmux name"},
			{"shared tmux", append(fleetSessionRows(fleetUUID(127)), row(27, "d3:0.0"), Session{PID: 28, SessionID: "y", Tmux: "d3:1.0"}), "shares tmux"},
		}
		for _, c := range down {
			d := deskViews(t, c.rows)[name]
			if d["alive"] != false || d["reason"] != c.reason || d["tmux"] != nil {
				t.Errorf("%s %s: %v, want reason %q", name, c.name, d, c.reason)
			}
		}
	}
}

func TestFleetSessionAllowsTheDesk(t *testing.T) {
	s := fleetTestServer(t, deskRows())
	s.fleet.disabled = deskDisabled
	writeFleetFile(t, s, "state.json", fleetSessionState)
	// Disabled and on no card, but the one usable CHAT row: allowed, with
	// the row's name as the title.
	code, out := getFleetSession(t, s, "t=desk")
	tg, _ := out["target"].(map[string]any)
	if code != 200 || tg["tmux"] != "desk" || tg["title"] != "CHAT" || tg["canSend"] != true {
		t.Fatalf("desk: %d %v", code, out)
	}
	code, out = getFleetSession(t, s, "t=slack-triage-c4a100")
	tg, _ = out["target"].(map[string]any)
	if code != 200 || tg["title"] != "SLACK" || tg["canSend"] != true {
		t.Fatalf("slack: %d %v", code, out)
	}
	// The desk allowance names the desks' tmux sessions only.
	for _, name := range []string{"off", "nobody", "fleet"} {
		if code, out := getFleetSession(t, s, "t="+name); code != 403 {
			t.Fatalf("%s: %d %v", name, code, out)
		}
	}
	// A CHAT row in FLEET's tmux session is still FLEET's session.
	self := append(fleetSessionRows(fleetUUID(127)), Session{PID: 28, Name: "CHAT", NameSource: "user", SessionID: deskSID, Tmux: "fleet:1.0"})
	s2 := fleetTestServer(t, self)
	s2.fleet.disabled = deskDisabled
	if code, out := getFleetSession(t, s2, "t=fleet"); code != 403 {
		t.Fatalf("CHAT in FLEET's tmux: %d %v", code, out)
	}
	// Two rows in CHAT's tmux session (the other disabled too, so only the
	// desk allowance could let it through): still ambiguous.
	shared := append(deskRows(), Session{PID: 29, SessionID: fleetDisabledSID, Tmux: "desk:1.0"})
	s3 := fleetTestServer(t, shared)
	s3.fleet.disabled = deskDisabled
	if code, out := getFleetSession(t, s3, "t=desk"); code != 409 || out["code"] != "ambiguous" {
		t.Fatalf("shared: %d %v", code, out)
	}
	// Two CHAT rows: neither is the desk.
	two := append(deskRows(), Session{PID: 30, Name: "CHAT", NameSource: "user", SessionID: deskSID, Tmux: "desk2:0.0"})
	s4 := fleetTestServer(t, two)
	s4.fleet.disabled = deskDisabled
	if code, out := getFleetSession(t, s4, "t=desk"); code != 403 || out["code"] != "not_allowed" {
		t.Fatalf("two CHAT rows: %d %v", code, out)
	}
}

func TestFleetSessionSayToTheDesk(t *testing.T) {
	rig := newSayRig(t, deskRows())
	rig.s.fleet.disabled = deskDisabled
	writeFleetFile(t, rig.s, "state.json", fleetSessionState)
	var targets []string
	rig.s.fleet.typeText = func(target, text string) error {
		targets = append(targets, target)
		rig.typed = append(rig.typed, text)
		return nil
	}
	rig.s.fleet.pressEnter = func(target string) error { targets = append(targets, target); rig.enters++; return nil }
	code, out := rig.postSession(t, `{"target":"desk","text":"look at this"}`, nil)
	if code != 200 || out["ok"] != true || out["target"] != "desk" {
		t.Fatalf("%d %v", code, out)
	}
	if len(rig.typed) != 1 || rig.typed[0] != "look at this" || rig.enters != 1 {
		t.Fatalf("typed %q enters %d", rig.typed, rig.enters)
	}
	if strings.Join(targets, ",") != "=desk:0.0,=desk:0.0" {
		t.Fatalf("keys went to %v", targets)
	}
	targets = nil
	rig.now = rig.now.Add(time.Minute)
	code, out = rig.postSession(t, `{"target":"slack-triage-c4a100","text":"triage this","desk":"SLACK"}`, nil)
	if code != 200 || out["ok"] != true || strings.Join(targets, ",") != "=slack-triage-c4a100:0.0,=slack-triage-c4a100:0.0" {
		t.Fatalf("SLACK: %d %v keys %v", code, out, targets)
	}
	if len(rig.typed) != 2 || rig.typed[1] != "triage this" {
		t.Fatalf("typed %q", rig.typed)
	}
	// The guards still apply: a CHAT pane in copy-mode is refused.
	rig.s.fleet.paneInMode = func(string) (bool, error) { return true, nil }
	rig.now = rig.now.Add(time.Minute)
	code, out = rig.postSession(t, `{"target":"desk","text":"again"}`, nil)
	if out["ok"] != false || out["code"] != "pane_mode" || len(rig.typed) != 2 {
		t.Fatalf("copy-mode: %d %v typed %q", code, out, rig.typed)
	}
}

// replacedDeskRows: CHAT has exited and an unnamed session on the working
// list now runs in tmux session "desk", the one the page bound CHAT to.
func replacedDeskRows() []Session {
	return append(fleetSessionRows(fleetUUID(130)),
		Session{PID: 31, SessionID: fleetUUID(131), Status: "idle", Tmux: "desk:0.0"})
}

func TestFleetSessionDeskPin(t *testing.T) {
	// The real CHAT passes the pin.
	s := fleetTestServer(t, deskRows())
	s.fleet.disabled = deskDisabled
	writeFleetFile(t, s, "state.json", fleetSessionState)
	if code, out := getFleetSession(t, s, "t=desk&desk=1"); code != 200 {
		t.Fatalf("pinned CHAT: %d %v", code, out)
	}
	if code, out := getFleetSession(t, s, "t=desk&desk=yes"); code != 400 || out["code"] != "bad_request" {
		t.Fatalf("bad desk value: %d %v", code, out)
	}
	// Another session in CHAT's old tmux: the session view still opens it,
	// the CHAT view does not.
	s2 := fleetTestServer(t, replacedDeskRows())
	writeFleetFile(t, s2, "state.json", fleetSessionState)
	if code, out := getFleetSession(t, s2, "t=desk"); code != 200 {
		t.Fatalf("unpinned: %d %v", code, out)
	}
	if code, out := getFleetSession(t, s2, "t=desk&desk=1"); code != 409 || out["code"] != "not_desk" {
		t.Fatalf("pinned, not CHAT: %d %v", code, out)
	}
}

func TestFleetSessionSayDeskPin(t *testing.T) {
	// The real CHAT, flagged: typed.
	rig := newSayRig(t, deskRows())
	rig.s.fleet.disabled = deskDisabled
	writeFleetFile(t, rig.s, "state.json", fleetSessionState)
	code, out := rig.postSession(t, `{"target":"desk","text":"to chat","desk":true}`, nil)
	if code != 200 || out["ok"] != true || len(rig.typed) != 1 {
		t.Fatalf("pinned CHAT: %d %v typed %q", code, out, rig.typed)
	}
	// Another session in CHAT's old tmux: the flagged send is refused before
	// any key; the unflagged one (the session view) still types.
	rig2 := newSayRig(t, replacedDeskRows())
	writeFleetFile(t, rig2.s, "state.json", fleetSessionState)
	code, out = rig2.postSession(t, `{"target":"desk","text":"to chat","desk":true}`, nil)
	if code != 409 || out["code"] != "not_desk" || out["target"] != "desk" || out["error"] != "CHAT is no longer in this tmux session" || len(rig2.typed) != 0 {
		t.Fatalf("pinned, not CHAT: %d %v typed %q", code, out, rig2.typed)
	}
	code, out = rig2.postSession(t, `{"target":"desk","text":"to the view"}`, nil)
	if code != 200 || out["ok"] != true || len(rig2.typed) != 1 {
		t.Fatalf("unpinned: %d %v typed %q", code, out, rig2.typed)
	}
	// The pin is a desk name (or true, CHAT, from an older page), nothing
	// else.
	for _, bad := range []string{`"1"`, `""`, `"FLEET"`, `"chat"`, `1`, `["CHAT"]`} {
		if code, out := rig2.postSession(t, `{"target":"desk","text":"x","desk":`+bad+`}`, nil); code != 400 || len(rig2.typed) != 1 {
			t.Fatalf("desk %s: %d %v", bad, code, out)
		}
	}
}

// TestFleetSessionDeskPinPerDesk: each desk view pins its own desk. A send
// written for one desk never reaches the other, even when the other now
// holds the tmux session the page bound.
func TestFleetSessionDeskPinPerDesk(t *testing.T) {
	s := fleetTestServer(t, deskRows())
	s.fleet.disabled = deskDisabled
	writeFleetFile(t, s, "state.json", fleetSessionState)
	for _, c := range []struct {
		q, code string
		status  int
	}{
		{"t=desk&desk=CHAT", "", 200},
		{"t=desk&desk=1", "", 200}, // an older page's CHAT pin
		{"t=slack-triage-c4a100&desk=SLACK", "", 200},
		{"t=slack-triage-c4a100&desk=CHAT", "not_desk", 409},
		{"t=slack-triage-c4a100&desk=1", "not_desk", 409},
		{"t=desk&desk=SLACK", "not_desk", 409},
		{"t=desk&desk=FLEET", "bad_request", 400},
		{"t=desk&desk=slack", "bad_request", 400},
	} {
		code, out := getFleetSession(t, s, c.q)
		if code != c.status || (c.code != "" && out["code"] != c.code) {
			t.Errorf("%s: %d %v", c.q, code, out)
		}
	}
	if _, out := getFleetSession(t, s, "t=desk&desk=SLACK"); out["error"] != "SLACK is no longer in this tmux session" {
		t.Errorf("not_desk names the desk: %v", out)
	}

	// SLACK has exited and CHAT now runs in SLACK's old tmux session: a
	// draft written for SLACK is refused before any key, CHAT's own send
	// types.
	moved := append(fleetSessionRows(fleetUUID(133)),
		Session{PID: 44, Name: "CHAT", NameSource: "user", SessionID: deskSID, Status: "idle", Tmux: "slack-triage-c4a100:0.0"})
	rig := newSayRig(t, moved)
	rig.s.fleet.disabled = deskDisabled
	writeFleetFile(t, rig.s, "state.json", fleetSessionState)
	code, out := rig.postSession(t, `{"target":"slack-triage-c4a100","text":"for slack","desk":"SLACK"}`, nil)
	if code != 409 || out["code"] != "not_desk" || len(rig.typed) != 0 {
		t.Fatalf("SLACK draft to CHAT: %d %v typed %q", code, out, rig.typed)
	}
	rig.now = rig.now.Add(time.Minute)
	code, out = rig.postSession(t, `{"target":"slack-triage-c4a100","text":"for chat","desk":"CHAT"}`, nil)
	if code != 200 || out["ok"] != true || len(rig.typed) != 1 || rig.typed[0] != "for chat" {
		t.Fatalf("CHAT: %d %v typed %q", code, out, rig.typed)
	}
}
