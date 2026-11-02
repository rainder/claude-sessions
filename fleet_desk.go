package main

import (
	"encoding/json"
	"net/http"
)

// The chat panel's other targets: Andy's desk sessions, each the one session
// the user named exactly one of fleetDeskSessionNames. The page reads them
// from /fleet/state's "desks" and talks to them through the session view's
// routes (/fleet/session, /fleet/session/say): fleetResolveSession allows a
// desk's tmux session even when the row is off the working list (disabled for
// FLEET), so every guard of that pipeline still applies. Reasons are short
// fixed strings: the page words them per desk name.

// fleetDeskSessionNames are the desk sessions, in the switch's order after
// FLEET: CHAT is Andy's desk, SLACK his Slack triage. The first one is also
// what a page loaded before there were several meant by "desk" (see
// fleetDeskParam).
var fleetDeskSessionNames = []string{"CHAT", "SLACK"}

// fleetDeskView is one entry of /fleet/state's "desks". Tmux, Status and
// Tool are set only when Alive; Reason only when not.
type fleetDeskView struct {
	Name   string `json:"name"`
	Alive  bool   `json:"alive"`
	Tmux   string `json:"tmux,omitempty"`
	Status string `json:"status,omitempty"`
	Tool   string `json:"tool,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// fleetDeskRow is the one usable row named name in sessions, or a reason it
// has none: the list failed (nil), no or two such rows, a remote row, no
// tmux pane, a tmux name the session routes refuse, or a tmux session that
// holds FLEET.
func fleetDeskRow(sessions []Session, name string) (Session, string) {
	if sessions == nil {
		return Session{}, "session list failed"
	}
	row, reason := findNamedSession(sessions, name)
	part := tmuxSessionPart(row.Tmux)
	switch {
	case reason != "":
		return Session{}, reason
	case row.Host != "":
		return Session{}, "remote"
	case row.Tmux == "":
		return Session{}, "not in tmux"
	case !fleetSessionNameRe.MatchString(part):
		return Session{}, "bad tmux name"
	case fleetSelfTmux(sessions)[part]:
		return Session{}, "in FLEET's tmux"
	}
	return row, ""
}

// fleetDesk builds desk name's view from the list /fleet/state already uses.
// A tmux session the desk shares with another row (another desk too) is not
// usable either: the session routes would refuse it as ambiguous.
func fleetDesk(sessions []Session, name string) fleetDeskView {
	v := fleetDeskView{Name: name}
	row, reason := fleetDeskRow(sessions, name)
	if reason != "" {
		v.Reason = reason
		return v
	}
	part := tmuxSessionPart(row.Tmux)
	n := 0
	for _, sess := range sessions {
		if sess.Tmux != "" && tmuxSessionPart(sess.Tmux) == part {
			n++
		}
	}
	if n > 1 {
		v.Reason = "shares tmux"
		return v
	}
	v.Alive, v.Tmux, v.Status, v.Tool = true, part, fleetStatus(row), "claude"
	if row.IsGrok() {
		v.Tool = "grok"
	}
	return v
}

// fleetDesks is /fleet/state's "desks": every desk, in fleetDeskSessionNames
// order, usable or not.
func fleetDesks(sessions []Session) []fleetDeskView {
	out := make([]fleetDeskView, 0, len(fleetDeskSessionNames))
	for _, name := range fleetDeskSessionNames {
		out = append(out, fleetDesk(sessions, name))
	}
	return out
}

// fleetDeskAt reports whether tmux session tm holds desk name's usable row
// on sessions.
func fleetDeskAt(tm, name string, sessions []Session) bool {
	row, reason := fleetDeskRow(sessions, name)
	return reason == "" && tmuxSessionPart(row.Tmux) == tm
}

// fleetIsDesk reports whether tm is the tmux session of any usable desk row
// on sessions.
func fleetIsDesk(tm string, sessions []Session) bool {
	for _, name := range fleetDeskSessionNames {
		if fleetDeskAt(tm, name, sessions) {
			return true
		}
	}
	return false
}

// fleetDeskPin refuses a desk-view request (desk=<name>, "desk": "<name>")
// whose tmux session tm no longer holds that desk's usable row on sessions,
// the list the handler resolved from. The page binds a desk by tmux name
// between state polls; without this, a new session started in that tmux
// after the desk exited (another desk too) would be shown and typed into as
// the desk.
func fleetDeskPin(tm, name string, sessions []Session) *fleetSessionErr {
	if fleetDeskAt(tm, name, sessions) {
		return nil
	}
	return &fleetSessionErr{http.StatusConflict, "not_desk", name + " is no longer in this tmux session"}
}

// fleetDeskParam reads GET /fleet/session's desk pin: "" is none, a desk
// name pins that desk, and "1" is CHAT (the first desk), what a page loaded
// before there were several desks sends; drop "1" in the next release.
// ok is false for anything else.
func fleetDeskParam(v string) (name string, ok bool) {
	switch {
	case v == "":
		return "", true
	case v == "1":
		return fleetDeskSessionNames[0], true
	}
	for _, n := range fleetDeskSessionNames {
		if v == n {
			return n, true
		}
	}
	return "", false
}

// fleetDeskJSON reads POST /fleet/session/say's "desk": absent, null or
// false is no pin, a desk name pins that desk, and true is CHAT (as "1" in
// fleetDeskParam; drop it in the next release). ok is false for anything
// else.
func fleetDeskJSON(raw json.RawMessage) (name string, ok bool) {
	if len(raw) == 0 || string(raw) == "null" || string(raw) == "false" {
		return "", true
	}
	if string(raw) == "true" {
		return fleetDeskSessionNames[0], true
	}
	var s string
	if json.Unmarshal(raw, &s) != nil || s == "" || s == "1" {
		return "", false
	}
	return fleetDeskParam(s)
}
