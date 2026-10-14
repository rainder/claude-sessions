package main

import (
	"encoding/json"
	"time"
)

// fleetLiveSession is the server's own view of a session a card names, for
// the page's badge. It never comes from FLEET: FLEET writes the card, this
// says whether the session behind it still runs and what it does now.
type fleetLiveSession struct {
	// Status is busy, idle, shell or waiting (StatusDisplay), "" when the
	// row reports none, gone when no live row sits in that tmux session,
	// and ambiguous when two or more do.
	Status string `json:"status"`
	// Since is when the row last changed state (its session file's
	// updatedAt), for a row that is not busy. Empty when unknown.
	Since string `json:"since,omitempty"`
}

// fleetStateFileAt is the mtime of the state.json copy being served: the
// write time to show when the file has no valid writtenAt. Zero when no copy
// was ever read.
func (s *server) fleetStateFileAt() time.Time {
	s.fleet.mu.Lock()
	defer s.fleet.mu.Unlock()
	if s.fleet.good == nil {
		return time.Time{}
	}
	return s.fleet.goodMod
}

// fleetCardSessionNames lists, in card order and once each, the tmux
// session names that cards in state carry. A malformed name is skipped: the
// page cannot open it either.
func fleetCardSessionNames(state map[string]json.RawMessage) []string {
	var cards []map[string]json.RawMessage
	_ = json.Unmarshal(state["cards"], &cards)
	var out []string
	seen := map[string]bool{}
	for _, c := range cards {
		var v string
		_ = json.Unmarshal(c["session"], &v)
		name := tmuxSessionPart(v)
		if !fleetSessionNameRe.MatchString(name) || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

// fleetLiveSessions maps each name to the live local row in that tmux
// session. sessions is the list discovery just used; nil means the list
// failed, and then the answer is nil (unknown), never "gone" for everyone.
func fleetLiveSessions(names []string, sessions []Session) map[string]fleetLiveSession {
	if sessions == nil {
		return nil
	}
	out := make(map[string]fleetLiveSession, len(names))
	for _, name := range names {
		var hit []Session
		for _, row := range sessions {
			if row.Host == "" && row.Tmux != "" && tmuxSessionPart(row.Tmux) == name {
				hit = append(hit, row)
			}
		}
		switch len(hit) {
		case 0:
			out[name] = fleetLiveSession{Status: "gone"}
		case 1:
			row := hit[0]
			ls := fleetLiveSession{Status: row.StatusDisplay()}
			if ls.Status != "busy" && row.UpdatedAt > 0 {
				ls.Since = time.UnixMilli(row.UpdatedAt).UTC().Format(time.RFC3339)
			}
			out[name] = ls
		default:
			out[name] = fleetLiveSession{Status: "ambiguous"}
		}
	}
	return out
}
