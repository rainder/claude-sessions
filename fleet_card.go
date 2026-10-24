package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// fleetOptionView is one answer option of a decision card, as the page's
// optionList reads it: four string fields, anything else ignored.
type fleetOptionView struct{ Key, Label, Line, Send string }

// fleetCardView is the part of a state.json card the answer path needs. It
// decodes as leniently as the page does: a field of the wrong type is empty,
// and only an object entry of `options` counts.
type fleetCardView struct {
	ID, Kind, Status, Ticket, Title, Summary, Lean, WaitingOn, Session, UpdatedAt string
	Options                                                                       []fleetOptionView

	// Priority is 1 Urgent, 2 High, 3 Normal, 4 Low; anything else decodes
	// as 3. FirstSeenAt is when fleet-state.py first saw the card id. The
	// two order the open cards (fleetItems); neither is in the card hash,
	// because they do not change what the card asks.
	Priority    int
	FirstSeenAt string

	// rawSession and rawTicket are session and ticket as decoded, before the
	// TUI sanitizes the drawn fields. The Fleet view's attach matches them
	// against the server's working list; they are never drawn.
	rawSession, rawTicket string
}

// UnmarshalJSON fails only when b is not a JSON object.
func (c *fleetCardView) UnmarshalJSON(b []byte) error {
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	*c = fleetCardView{
		ID:        fleetPushStr(m["id"]),
		Kind:      fleetPushStr(m["kind"]),
		Status:    fleetPushStr(m["status"]),
		Ticket:    fleetPushStr(m["ticket"]),
		Title:     fleetPushStr(m["title"]),
		Summary:   fleetPushStr(m["summary"]),
		Lean:      fleetPushStr(m["lean"]),
		WaitingOn: fleetPushStr(m["waitingOn"]),
		Session:   fleetPushStr(m["session"]),
		UpdatedAt: fleetPushStr(m["updatedAt"]),

		Priority:    fleetPriorityNormal,
		FirstSeenAt: fleetPushStr(m["firstSeenAt"]),
	}
	if p, ok := m["priority"].(float64); ok && p == float64(int(p)) && fleetPriorityValid(int(p)) {
		c.Priority = int(p)
	}
	c.rawSession, c.rawTicket = c.Session, c.Ticket
	if list, ok := m["options"].([]any); ok {
		for _, o := range list {
			om, ok := o.(map[string]any)
			if !ok {
				continue
			}
			c.Options = append(c.Options, fleetOptionView{
				Key:   fleetPushStr(om["key"]),
				Label: fleetPushStr(om["label"]),
				Line:  fleetPushStr(om["line"]),
				Send:  fleetPushStr(om["send"]),
			})
		}
	}
	return nil
}

// fleetPriorityNormal is the priority of a card that sets none.
const fleetPriorityNormal = 3

func fleetPriorityValid(p int) bool { return p >= 1 && p <= 4 }

// priority is the card's priority, with an out-of-range value (a card built
// in code, not decoded) read as normal.
func (c fleetCardView) priority() int {
	if fleetPriorityValid(c.Priority) {
		return c.Priority
	}
	return fleetPriorityNormal
}

// fleetCardHash identifies what a card asks, so an answer can tell whether
// the question changed under it. It covers kind, status, title, ticket,
// summary, lean and each option's key, label, line and send, and never
// updatedAt (FLEET stamps it on every write), id, session, links or waitingOn.
// 16 hex chars of sha256 over canonical JSON.
func fleetCardHash(c fleetCardView) string {
	opts := make([][4]string, 0, len(c.Options))
	for _, o := range c.Options {
		opts = append(opts, [4]string{o.Key, o.Label, o.Line, o.Send})
	}
	b, _ := json.Marshal([]any{c.Kind, c.Status, c.Title, c.Ticket, c.Summary, c.Lean, opts})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16]
}

// fleetDecisionCard is a decision card with its hash.
type fleetDecisionCard struct {
	Hash string
	Card fleetCardView
}

// fleetDecisionCards returns the decision cards (needs-you, explicit-yes) of
// a parsed state.json by id. A card that does not decode or has no id is
// skipped; when an id repeats, the first card wins. Never nil.
func fleetDecisionCards(state map[string]json.RawMessage) map[string]fleetDecisionCard {
	out := map[string]fleetDecisionCard{}
	var raws []json.RawMessage
	if json.Unmarshal(state["cards"], &raws) != nil {
		return out
	}
	for _, raw := range raws {
		var c fleetCardView
		if json.Unmarshal(raw, &c) != nil || !fleetPushKind(c.Kind) || c.ID == "" {
			continue
		}
		if _, dup := out[c.ID]; dup {
			continue
		}
		out[c.ID] = fleetDecisionCard{Hash: fleetCardHash(c), Card: c}
	}
	return out
}

// fleetCardHashes maps card id to hash. Never nil.
func fleetCardHashes(cards map[string]fleetDecisionCard) map[string]string {
	out := make(map[string]string, len(cards))
	for id, c := range cards {
		out[id] = c.Hash
	}
	return out
}

// fleetOptionLabel finds the option with the given key. The label is the
// page's optLabel rule: label, else send, else key.
func fleetOptionLabel(c fleetCardView, key string) (label, send string, ok bool) {
	for _, o := range c.Options {
		if o.Key != key {
			continue
		}
		label = o.Label
		if label == "" {
			label = o.Send
		}
		if label == "" {
			label = o.Key
		}
		return label, o.Send, true
	}
	return "", "", false
}
