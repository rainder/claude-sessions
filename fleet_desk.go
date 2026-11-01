package main

// The chat panel's second target: Andy's desk session, the one session the
// user named exactly CHAT. The page reads it from /fleet/state's "desk" and
// talks to it through the session view's routes (/fleet/session,
// /fleet/session/say): fleetResolveSession allows its tmux session even when
// the row is off the working list (disabled for FLEET), so every guard of
// that pipeline still applies. Reasons are short fixed strings: the page
// shows them as they are.

// fleetDeskView is /fleet/state's "desk". Tmux, Status and Tool are set only
// when Alive; Reason only when not.
type fleetDeskView struct {
	Name   string `json:"name"`
	Alive  bool   `json:"alive"`
	Tmux   string `json:"tmux,omitempty"`
	Status string `json:"status,omitempty"`
	Tool   string `json:"tool,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// fleetDeskRow is the one usable CHAT row in sessions, or a reason it has
// none: the list failed (nil), no or two CHAT rows, a remote row, no tmux
// pane, a tmux name the session routes refuse, or a tmux session that holds
// FLEET.
func fleetDeskRow(sessions []Session) (Session, string) {
	if sessions == nil {
		return Session{}, "session list failed"
	}
	row, reason := findNamedSession(sessions, fleetDeskSessionName)
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

// fleetDesk builds the "desk" view from the list /fleet/state already uses.
// A tmux session CHAT shares with another row is not usable either: the
// session routes would refuse it as ambiguous.
func fleetDesk(sessions []Session) fleetDeskView {
	v := fleetDeskView{Name: fleetDeskSessionName}
	row, reason := fleetDeskRow(sessions)
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

// fleetIsDesk reports whether name is the tmux session of the usable CHAT
// row on sessions.
func fleetIsDesk(name string, sessions []Session) bool {
	row, reason := fleetDeskRow(sessions)
	return reason == "" && tmuxSessionPart(row.Tmux) == name
}
