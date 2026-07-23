package main

import "strings"

const emptyHostSelectionPrefix = "\x00host:"

type selectionTarget struct {
	id      string
	host    string
	session *Session
}

func sessionSelectionTarget(s Session) selectionTarget {
	return selectionTarget{id: s.ID(), host: s.Host, session: &s}
}

func emptyHostSelectionID(host string) string {
	return emptyHostSelectionPrefix + host
}

func emptyHostSelectionTarget(host string) selectionTarget {
	return selectionTarget{id: emptyHostSelectionID(host), host: host}
}

func buildSelectionTargets(local []Session, remotes []RemoteResult) []selectionTarget {
	targets := make([]selectionTarget, 0, len(local)+len(remotes))
	for _, session := range local {
		targets = append(targets, sessionSelectionTarget(session))
	}
	if len(local) == 0 {
		targets = append(targets, emptyHostSelectionTarget(""))
	}
	for _, remote := range remotes {
		if len(remote.Sessions) > 0 {
			for _, session := range remote.Sessions {
				targets = append(targets, sessionSelectionTarget(session))
			}
			continue
		}
		if !remote.Loading && remote.Error == "" {
			targets = append(targets, emptyHostSelectionTarget(remote.Name))
		}
	}
	return targets
}

// findSelectionTarget returns the target whose id matches, or nil if none does.
// The returned pointer aliases the caller's slice element.
func findSelectionTarget(targets []selectionTarget, id string) *selectionTarget {
	for i := range targets {
		if targets[i].id == id {
			return &targets[i]
		}
	}
	return nil
}

func navTargets(targets []selectionTarget, sel string, delta int) string {
	n := len(targets)
	if n == 0 {
		return ""
	}
	if sel == "" {
		if delta > 0 {
			return targets[0].id
		}
		return targets[n-1].id
	}
	for i, target := range targets {
		if target.id == sel {
			next := ((i+delta)%n + n) % n
			return targets[next].id
		}
	}
	return targets[0].id
}

// selectionForTmux returns the id of the target whose session is running in
// the given tmux session (matched on the host + "<name>:" prefix of its Tmux
// pane string), or "" if no target has picked it up yet. Used after spawning
// a new tmux session so the caller can move the selection onto it once it
// shows up in a fresh snapshot.
func selectionForTmux(targets []selectionTarget, host, tmuxSession string) string {
	if tmuxSession == "" {
		return ""
	}
	prefix := tmuxSession + ":"
	for _, target := range targets {
		if target.host == host && target.session != nil && strings.HasPrefix(target.session.Tmux, prefix) {
			return target.id
		}
	}
	return ""
}

// firstIdleTarget returns the id of the topmost target whose session is idle
// and not disabled, or "" if none qualifies.
func firstIdleTarget(targets []selectionTarget) string {
	for _, target := range targets {
		if target.session != nil && target.session.Status == "idle" && !target.session.Disabled {
			return target.id
		}
	}
	return ""
}

func validateTargetSel(targets []selectionTarget, sel string) string {
	for _, target := range targets {
		if target.id == sel {
			return sel
		}
	}
	if strings.HasPrefix(sel, emptyHostSelectionPrefix) {
		host := strings.TrimPrefix(sel, emptyHostSelectionPrefix)
		for _, target := range targets {
			if target.session != nil && target.session.Host == host {
				return target.id
			}
		}
	}
	if len(targets) > 0 {
		return targets[0].id
	}
	return ""
}
