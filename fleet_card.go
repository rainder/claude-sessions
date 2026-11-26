package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
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

	// Waiting is the card's "waiting on X since T" line; nil when the card
	// has no valid `waiting` object. When set, it supersedes WaitingOn. It is
	// not in the card hash.
	Waiting *fleetWaiting

	// Envs is the environments the card affects (`envs`), allowlisted,
	// deduplicated and in fleetEnvOrder; nil when none is set. Not in the
	// card hash.
	Envs []string

	// rawTmux, rawSession and rawTicket are tmux, session and ticket as
	// decoded, before the TUI sanitizes the drawn fields. The Fleet view's
	// attach and session chat match them against the server's working list
	// (fleetCardSession); they are never drawn.
	rawTmux, rawSession, rawTicket string
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
	c.Waiting = fleetDecodeWaiting(m["waiting"])
	c.Envs = fleetDecodeEnvs(m["envs"])
	c.rawTmux, c.rawSession, c.rawTicket = fleetPushStr(m["tmux"]), c.Session, c.Ticket
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

// fleetWaiting is a card's `waiting` object: who FLEET waits on, since when,
// and when they answered (zero = not yet). Who and Note are one line,
// trimmed and capped.
type fleetWaiting struct {
	Who, Note         string
	Since, AnsweredAt time.Time
}

const (
	fleetWaitingWhoMax  = 40  // runes
	fleetWaitingNoteMax = 120 // runes
)

// fleetDecodeWaiting reads a card's `waiting` value leniently: anything but
// an object with a non-empty string `who` and an RFC 3339 `since` is nil. A
// bad `answeredAt` or `note` is dropped alone.
func fleetDecodeWaiting(v any) *fleetWaiting {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	who, _ := m["who"].(string)
	w := &fleetWaiting{Who: fleetWaitingClip(who, fleetWaitingWhoMax)}
	if w.Who == "" {
		return nil
	}
	since, _ := m["since"].(string)
	t, err := time.Parse(time.RFC3339, since)
	if err != nil {
		return nil
	}
	w.Since = t
	if s, ok := m["answeredAt"].(string); ok {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			w.AnsweredAt = t
		}
	}
	if s, ok := m["note"].(string); ok {
		w.Note = fleetWaitingClip(s, fleetWaitingNoteMax)
	}
	return w
}

// fleetWaitingClip folds s to one line (runs of white space become one
// space, ends trimmed) and cuts it to max runes.
func fleetWaitingClip(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		s = strings.TrimSpace(string(r[:max]))
	}
	return s
}

// fleetWaitingText is the card's waiting line: "waiting on <who> since <T>"
// (" · <note>"), or once answered "<who> answered <T>" (" (<note>)"). A time
// on now's calendar day in now's location shows as HH:MM, any other as
// MM-DD HH:MM.
func fleetWaitingText(w *fleetWaiting, now time.Time) string {
	if w == nil {
		return ""
	}
	if !w.AnsweredAt.IsZero() {
		s := w.Who + " answered " + fleetWaitingStamp(w.AnsweredAt, now)
		if w.Note != "" {
			s += " (" + w.Note + ")"
		}
		return s
	}
	s := "waiting on " + w.Who + " since " + fleetWaitingStamp(w.Since, now)
	if w.Note != "" {
		s += " · " + w.Note
	}
	return s
}

func fleetWaitingStamp(t, now time.Time) string {
	t = t.In(now.Location())
	if y, m, d := t.Date(); y == now.Year() && m == now.Month() && d == now.Day() {
		return t.Format("15:04")
	}
	return t.Format("01-02 15:04")
}

// fleetEnvOrder is every value a card's `envs` may hold, in display order.
// "none" means the decision touches no environment (a mail, a ticket).
var fleetEnvOrder = []string{"prod", "sandbox", "dev", "predev", "local", "none"}

// fleetDecodeEnvs reads a card's `envs` value leniently, as the page's
// envList does: anything but an array is nil, unknown values and repeats are
// dropped, the rest comes back in fleetEnvOrder. fleet-state.py refuses
// "none" next to another value; should one get through, "none" is dropped.
func fleetDecodeEnvs(v any) []string {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	have := map[string]bool{}
	for _, x := range list {
		if s, ok := x.(string); ok {
			have[s] = true
		}
	}
	var out []string
	for _, e := range fleetEnvOrder {
		if have[e] && (e != "none" || len(out) == 0) {
			out = append(out, e)
		}
	}
	return out
}

// fleetEnvSGR is each environment's colour in the TUI, as the page colours
// its chips: prod red, sandbox amber, dev blue, predev magenta. local and
// none are dim.
var fleetEnvSGR = map[string]string{
	"prod":    fleetSGRRed,
	"sandbox": fleetSGRAmber,
	"dev":     fleetSGRURL,
	"predev":  fleetSGRRef,
}

// fleetEnvText is a card's affects line as plain text, "Affects: prod, dev";
// "" when the card has no envs (an older card).
func fleetEnvText(envs []string) string {
	if len(envs) == 0 {
		return ""
	}
	return "Affects: " + strings.Join(envs, ", ")
}

// fleetEnvLines is the affects line for the details panel, wrapped at cols,
// the label and local/none dim, every other environment in its fleetEnvSGR
// colour; nil when the card has no envs. Its words are fleetEnvOrder's fixed
// labels, never card text.
func fleetEnvLines(envs []string, cols int) []string {
	text := fleetEnvText(envs)
	if text == "" {
		return nil
	}
	var out []string
	for _, l := range wrapRunes(text, cols) {
		words := strings.Split(l, " ")
		for i, w := range words {
			name := strings.TrimSuffix(w, ",")
			if code, ok := fleetEnvSGR[name]; ok {
				words[i] = colorize(code, name) + w[len(name):]
			} else {
				words[i] = dim(name) + w[len(name):]
			}
		}
		out = append(out, strings.Join(words, " "))
	}
	return out
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
// updatedAt (FLEET stamps it on every write), id, session, links, waitingOn,
// waiting or envs.
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
