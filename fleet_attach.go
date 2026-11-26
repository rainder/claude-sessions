package main

import (
	"errors"
	"fmt"
	"strings"
)

// Attach from the Fleet view: `a` on a card opens the tmux session that card
// belongs to, and the view comes back when Andy detaches. `i` (or →) opens the
// same session in the chat panel (fleet_tui_chat.go).
//
// A card's session and ticket are written by an LLM from text strangers
// control, and on a remote host the name is parsed by that host's login
// shell. So a name reaches a command line only when it matches
// fleetSessionNameRe AND is the tmux name of a row in the server's own
// working list (/fleet/state's "sessions"): a card can only ever point at a
// session the server itself lists. The target carries tmux's "=" exact
// prefix (tmuxExactTarget), so a gone session never prefix-matches another
// and a name that starts with "-" is never read as a flag.

// fleetWorkSession is one working-list row as the attach and the session
// chat need it, raw as the server sent it and never drawn as is.
type fleetWorkSession struct {
	Tmux      string
	Name      string // display name
	SessionID string // the session the row runs; the chat's sid pin
	Tickets   []string
}

// fleetCardPick is where a card's session resolves (fleetCardSession): Tmux
// for exactly one live tmux session, with SID and Name from its row when
// exactly one row runs there; Many ≥ 2 when that many tmux sessions match;
// Ended when the card's tmux field names a session that is not live; the
// zero value when nothing matches.
type fleetCardPick struct {
	Tmux, SID, Name string
	Many            int
	Ended           bool
}

// fleetAttachRequest is one attach the view asked for, pinned like a send to
// the Source of the state it was resolved against.
type fleetAttachRequest struct {
	Tmux   string
	Source string
}

var errFleetAttachName = errors.New("bad tmux session name")

// fleetCardSession resolves the session card c belongs to against the
// server's working list (nil: the list failed, so nothing), as the page's
// cardSession does; a and s share it. Rows whose tmux fails
// fleetSessionNameRe never count. The card's tmux field decides when set
// (its part before ":"): live, or Ended, never a fallback. Otherwise its
// session field: the rows whose display name equals it or whose tmux equals
// its part before ":"; one tmux session, or Many. Only a card with neither
// field falls back to its ticket (the TUI's own rule, not the page's): the
// one tmux session whose row lists it (case-insensitive). Two rows in one
// tmux session are one place.
func fleetCardSession(c fleetCardView, sessions []fleetWorkSession) fleetCardPick {
	var rows []fleetWorkSession
	for _, s := range sessions {
		if fleetSessionNameRe.MatchString(s.Tmux) {
			rows = append(rows, s)
		}
	}
	if tm := fleetCardTmuxPart(c.rawTmux); tm != "" {
		if p := fleetPickTmux(rows, tm); p.Tmux != "" {
			return p
		}
		return fleetCardPick{Ended: true}
	}
	var match func(fleetWorkSession) bool
	if name := strings.TrimSpace(c.rawSession); name != "" {
		tm := fleetCardTmuxPart(name)
		match = func(r fleetWorkSession) bool { return r.Name == name || r.Tmux == tm }
	} else if ticket := strings.TrimSpace(c.rawTicket); ticket != "" {
		match = func(r fleetWorkSession) bool {
			for _, t := range r.Tickets {
				if strings.EqualFold(strings.TrimSpace(t), ticket) {
					return true
				}
			}
			return false
		}
	} else {
		return fleetCardPick{}
	}
	var hits []string
	seen := map[string]bool{}
	for _, r := range rows {
		if !seen[r.Tmux] && match(r) {
			seen[r.Tmux] = true
			hits = append(hits, r.Tmux)
		}
	}
	switch len(hits) {
	case 0:
		return fleetCardPick{}
	case 1:
		return fleetPickTmux(rows, hits[0])
	}
	return fleetCardPick{Many: len(hits)}
}

// fleetPickTmux is the pick for tmux session tm when a row runs there, with
// that row's session id and name when it is the only one; zero otherwise.
func fleetPickTmux(rows []fleetWorkSession, tm string) fleetCardPick {
	var p fleetCardPick
	n := 0
	for _, r := range rows {
		if r.Tmux == tm {
			n++
			p = fleetCardPick{Tmux: tm, SID: r.SessionID, Name: r.Name}
		}
	}
	if n > 1 {
		p.SID, p.Name = "", ""
	}
	return p
}

// fleetCardPickRefusal is the toast for a pick that names no one session,
// "" for one that does. ticket is the card's sanitized ticket.
func fleetCardPickRefusal(p fleetCardPick, ticket string) string {
	switch {
	case p.Tmux != "":
		return ""
	case p.Many > 0 && ticket != "":
		return fmt.Sprintf("%d sessions for %s", p.Many, ticket)
	case p.Many > 0:
		return fmt.Sprintf("%d sessions match this card", p.Many)
	case p.Ended:
		return "this card's session has ended"
	}
	return "no session for this card"
}

// fleetAttachArgv is the command that attaches to tmux session name: on this
// host (srv nil) `tmux attach`, or `tmux switch-client` from inside tmux; on
// a remote host `ssh -t <target> tmux attach`, as runRemoteAttach does. A
// name that fails fleetSessionNameRe is refused. ssh hands its remote words
// to the remote login shell, and zsh reads a word that starts with "=" as a
// command path (EQUALS), so the remote target is single-quoted; the regex
// admits no quote, so the quotes cannot be closed early.
func fleetAttachArgv(srv *ServerConfig, name string, insideTmux bool) ([]string, error) {
	// tmux reads "." in a target as window.pane; it never puts one in a
	// session name, so a name holding one is refused, not split.
	if !fleetSessionNameRe.MatchString(name) || strings.Contains(name, ".") {
		return nil, errFleetAttachName
	}
	target := tmuxExactTarget(name)
	if srv == nil {
		if insideTmux {
			return []string{"tmux", "switch-client", "-t", target}, nil
		}
		return []string{"tmux", "attach", "-t", target}, nil
	}
	return append([]string{"ssh"}, sshInteractiveArgs(*srv, "tmux", "attach", "-t", "'"+target+"'")...), nil
}

// fleetAttachVia is the attach body, pinned like a send: it runs only on the
// host the request's state came from, and refuses with errFleetMoved when
// FLEET's chosen host is another one (or none). It reads the chosen host
// without fleetLocal's mutex, since it runs on the view's loop. run gets the
// remote's ServerConfig, nil for this host.
func fleetAttachVia(src *fleetLocal, run func(srv *ServerConfig, tmux string) error) func(fleetAttachRequest) error {
	return func(req fleetAttachRequest) error {
		h, ok := src.chosenHost()
		if !ok || req.Source == "" || h.base != req.Source {
			return errFleetMoved
		}
		if !fleetSessionNameRe.MatchString(req.Tmux) {
			return errFleetAttachName
		}
		return run(h.srv, req.Tmux)
	}
}

// runFleetAttach hands the terminal to the attach command and returns when
// Andy detaches. A remote attach relays image pastes like runRemoteAttach.
func runFleetAttach(c *actCtx, srv *ServerConfig, name string) error {
	inside, _ := isInsideTmux()
	argv, err := fleetAttachArgv(srv, name, inside)
	if err != nil {
		return err
	}
	if srv == nil {
		return c.runInteractive(argv[0], argv[1:]...)
	}
	return runSSHWithPasteRelay(c, *srv, name, argv[1:])
}
