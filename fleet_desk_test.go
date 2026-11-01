package main

import (
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

// deskRows is the session fixture plus a CHAT row in tmux session "desk",
// disabled for FLEET (Andy's desk is not FLEET's to manage) and on no card.
func deskRows() []Session {
	return append(fleetSessionRows(fleetUUID(120)),
		Session{PID: 20, Name: "CHAT", NameSource: "user", SessionID: deskSID, Status: "idle", Tmux: "desk:0.0"})
}

const deskSID = "desk-sid"

func deskDisabled(sid string) bool { return sid == deskSID || sid == fleetDisabledSID }

func deskView(t *testing.T, rows []Session) map[string]any {
	t.Helper()
	s := fleetTestServer(t, rows)
	s.fleet.disabled = deskDisabled
	d, ok := getFleetJSON(t, s, "/fleet/state")["desk"].(map[string]any)
	if !ok {
		t.Fatal("no desk object")
	}
	return d
}

func TestFleetStateDesk(t *testing.T) {
	d := deskView(t, deskRows())
	if d["name"] != "CHAT" || d["alive"] != true || d["tmux"] != "desk" || d["status"] != "idle" || d["tool"] != "claude" || d["reason"] != nil {
		t.Fatalf("alive: %v", d)
	}
	grok := append(fleetSessionRows(fleetUUID(121)), Session{PID: 21, Name: "CHAT", NameSource: "user", Tool: toolGrok, SessionID: "g", Status: "busy", Tmux: "gdesk:0.0"})
	if d := deskView(t, grok); d["alive"] != true || d["tool"] != "grok" || d["status"] != "busy" || d["tmux"] != "gdesk" {
		t.Fatalf("grok: %v", d)
	}
	down := []struct {
		name   string
		rows   []Session
		reason string
	}{
		{"not running", fleetSessionRows(fleetUUID(122)), "not running"},
		{"ambiguous", append(deskRows(), Session{PID: 22, Name: "CHAT", NameSource: "user", SessionID: "x", Tmux: "desk2:0.0"}), "ambiguous"},
		{"no tmux", append(fleetSessionRows(fleetUUID(123)), Session{PID: 23, Name: "CHAT", NameSource: "user", SessionID: "x"}), "not in tmux"},
		{"remote", append(fleetSessionRows(fleetUUID(124)), Session{PID: 24, Host: "ws", Name: "CHAT", NameSource: "user", SessionID: "x", Tmux: "desk:0.0"}), "remote"},
		{"FLEET's tmux", append(fleetSessionRows(fleetUUID(125)), Session{PID: 25, Name: "CHAT", NameSource: "user", SessionID: "x", Tmux: "fleet:1.0"}), "in FLEET's tmux"},
		{"bad tmux name", append(fleetSessionRows(fleetUUID(126)), Session{PID: 26, Name: "CHAT", NameSource: "user", SessionID: "x", Tmux: "my desk:0.0"}), "bad tmux name"},
		{"shared tmux", append(deskRows(), Session{PID: 27, SessionID: "y", Tmux: "desk:1.0"}), "shares tmux"},
	}
	for _, c := range down {
		d := deskView(t, c.rows)
		if d["alive"] != false || d["reason"] != c.reason || d["tmux"] != nil || d["name"] != "CHAT" {
			t.Errorf("%s: %v, want reason %q", c.name, d, c.reason)
		}
	}
	// A failed list is not usable either, and never a 500.
	if d := fleetDesk(nil); d.Alive || d.Reason != "session list failed" {
		t.Fatalf("list failed: %+v", d)
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
	// The desk allowance names one tmux session only.
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
	// The guards still apply: a CHAT pane in copy-mode is refused.
	rig.s.fleet.paneInMode = func(string) (bool, error) { return true, nil }
	rig.now = rig.now.Add(time.Minute)
	code, out = rig.postSession(t, `{"target":"desk","text":"again"}`, nil)
	if out["ok"] != false || out["code"] != "pane_mode" || len(rig.typed) != 1 {
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
	if code != 409 || out["code"] != "not_desk" || out["target"] != "desk" || len(rig2.typed) != 0 {
		t.Fatalf("pinned, not CHAT: %d %v typed %q", code, out, rig2.typed)
	}
	code, out = rig2.postSession(t, `{"target":"desk","text":"to the view"}`, nil)
	if code != 200 || out["ok"] != true || len(rig2.typed) != 1 {
		t.Fatalf("unpinned: %d %v typed %q", code, out, rig2.typed)
	}
	// The flag is a bool, nothing else.
	if code, out := rig2.postSession(t, `{"target":"desk","text":"x","desk":"1"}`, nil); code != 400 {
		t.Fatalf("string desk: %d %v", code, out)
	}
}
