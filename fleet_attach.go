package main

import (
	"errors"
	"strings"
)

// Attach from the Fleet view: `a` on a card opens the tmux session that card
// belongs to, and the view comes back when Andy detaches.
//
// A card's session and ticket are written by an LLM from text strangers
// control, and on a remote host the name is parsed by that host's login
// shell. So a name reaches a command line only when it matches
// fleetSessionNameRe AND is the tmux name of a row in the server's own
// working list (/fleet/state's "sessions"): a card can only ever point at a
// session the server itself lists. The target carries tmux's "=" exact
// prefix (tmuxExactTarget), so a gone session never prefix-matches another
// and a name that starts with "-" is never read as a flag.

// fleetWorkSession is one working-list row. Tmux and Tickets are raw as the
// server sent them, for the attach's matching, and never drawn. The rest is
// what the harbor (the view with no cards) draws, already passed through
// fleetDisplayText with its whitespace collapsed by the decoder: Label is
// the first ticket, else the tmux name; Since and StartedAt are RFC 3339
// stamps ("" when absent).
type fleetWorkSession struct {
	Tmux    string
	Tickets []string

	Label, Name, NameSource, Tool, Model, Status, Since, StartedAt string
}

// fleetAttachPick is where a card's attach goes: Tmux for exactly one live
// session, Many ≥ 2 when that many live sessions name the card's ticket, and
// the zero value when nothing does.
type fleetAttachPick struct {
	Tmux string
	Many int
}

// fleetAttachRequest is one attach the view asked for, pinned like a send to
// the Source of the state it was resolved against.
type fleetAttachRequest struct {
	Tmux   string
	Source string
}

var errFleetAttachName = errors.New("bad tmux session name")

// fleetAttachTarget resolves the session card c belongs to, against the
// server's working list (nil: the list failed, so nothing). First the card's
// session field (a "name:0.0" form counts only before ":", as the server
// reads it) when a live row has that tmux name. A session field that names
// no live row falls back to the ticket: the one tmux session whose row lists
// the card's ticket (case-insensitive). Two rows in one tmux session are one
// place to attach to; two tmux sessions are Many.
func fleetAttachTarget(c fleetCardView, sessions []fleetWorkSession) fleetAttachPick {
	if name := tmuxSessionPart(c.rawSession); fleetSessionNameRe.MatchString(name) {
		for _, s := range sessions {
			if s.Tmux == name {
				return fleetAttachPick{Tmux: name}
			}
		}
	}
	ticket := strings.TrimSpace(c.rawTicket)
	if ticket == "" {
		return fleetAttachPick{}
	}
	var hits []string
	seen := map[string]bool{}
	for _, s := range sessions {
		if seen[s.Tmux] || !fleetSessionNameRe.MatchString(s.Tmux) {
			continue
		}
		for _, t := range s.Tickets {
			if strings.EqualFold(strings.TrimSpace(t), ticket) {
				seen[s.Tmux] = true
				hits = append(hits, s.Tmux)
				break
			}
		}
	}
	switch len(hits) {
	case 0:
		return fleetAttachPick{}
	case 1:
		return fleetAttachPick{Tmux: hits[0]}
	}
	return fleetAttachPick{Many: len(hits)}
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
