package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	clarifyRoleYou   = "you"
	clarifyRoleFleet = "fleet"
	clarifyRoleError = "error"
	clarifyRoleNote  = "note"

	clarifyKindAsk   = "ask"
	clarifyKindDraft = "draft"

	// clarifyMaxRunning is the global cap on forks in flight at once.
	clarifyMaxRunning = 2

	// clarifyTurnsMaxBytes bounds the turn text a chat keeps: every change
	// rewrites the whole store file, and the TUI reads a chat with a 2 MiB
	// cap. The oldest turns go first. The fork keeps its own context, so
	// this trims only what is shown.
	clarifyTurnsMaxBytes = 1 << 20

	clarifyInterrupted = "interrupted by a restart"
	clarifyCardChanged = "card changed since this chat started"
)

// clarifyTurn is one line of a card's Clarify chat. Role is you, fleet, error
// or note. At is RFC3339 UTC.
type clarifyTurn struct {
	Role string `json:"role"`
	Text string `json:"text"`
	At   string `json:"at"`
}

// clarifyPending says a fork is running for the card. Kind is ask or draft.
type clarifyPending struct {
	Kind      string `json:"kind"`
	StartedAt string `json:"startedAt"`
}

// clarifyDraft is a proposed answer the fork wrote, waiting for Andy.
type clarifyDraft struct {
	Text string `json:"text"`
	At   string `json:"at"`
}

// clarifyEntry is one card's chat and the fork session behind it. Changed
// means the card-changed note is already in Turns.
type clarifyEntry struct {
	Tool           string          `json:"tool"`
	FleetSessionID string          `json:"fleetSessionId"`
	ForkID         string          `json:"forkId"`
	Cwd            string          `json:"cwd"`
	Model          string          `json:"model"`
	CardHash       string          `json:"cardHash"`
	Turns          []clarifyTurn   `json:"turns"`
	Pending        *clarifyPending `json:"pending,omitempty"`
	Draft          *clarifyDraft   `json:"draft,omitempty"`
	Changed        bool            `json:"changed,omitempty"`
}

// clarifySummary is what /fleet/state shows per card: counts and flags, never
// the text.
type clarifySummary struct {
	Turns   int  `json:"turns"`
	Pending bool `json:"pending"`
	Draft   bool `json:"draft"`
}

// fleetClarifyStore keeps each card's Clarify chat. Saved to path (0600,
// atomic) on every change; cancels and running live in memory only.
type fleetClarifyStore struct {
	path string // "" → memory only; cmdServer sets fleetClarifyPathDefault()

	mu      sync.Mutex
	loaded  bool
	entries map[string]*clarifyEntry
	cancels map[string]context.CancelFunc // forks in flight, by card id
	tokens  map[string]uint64             // begin's token for the fork in flight, by card id
	seq     uint64                        // last token handed out
	running int                           // begun and not yet finished or pruned
}

type fleetClarifyFile struct {
	Clarify map[string]*clarifyEntry `json:"clarify"`
}

func fleetClarifyPathDefault() string {
	if dir := claudeSessionsStateDir(); dir != "" {
		return filepath.Join(dir, "fleet-clarify.json")
	}
	return ""
}

func clarifyRoleOK(r string) bool {
	switch r {
	case clarifyRoleYou, clarifyRoleFleet, clarifyRoleError, clarifyRoleNote:
		return true
	}
	return false
}

// copyClarify deep-copies an entry, so a caller never shares a slice or
// pointer with the store.
func copyClarify(e clarifyEntry) clarifyEntry {
	e.Turns = append([]clarifyTurn(nil), e.Turns...)
	if e.Pending != nil {
		p := *e.Pending
		e.Pending = &p
	}
	if e.Draft != nil {
		d := *e.Draft
		e.Draft = &d
	}
	return e
}

// appendClarifyTurn adds t to the chat, then drops the oldest turns until the
// turn text is at most clarifyTurnsMaxBytes. The newest turn always stays.
func appendClarifyTurn(e *clarifyEntry, t clarifyTurn) {
	e.Turns = append(e.Turns, t)
	total := 0
	for _, x := range e.Turns {
		total += len(x.Text)
	}
	drop := 0
	for total > clarifyTurnsMaxBytes && drop < len(e.Turns)-1 {
		total -= len(e.Turns[drop].Text)
		drop++
	}
	if drop > 0 {
		e.Turns = append([]clarifyTurn(nil), e.Turns[drop:]...)
	}
}

func (st *fleetClarifyStore) loadLocked() {
	if st.loaded {
		return
	}
	st.loaded = true
	st.entries = map[string]*clarifyEntry{}
	st.cancels = map[string]context.CancelFunc{}
	st.tokens = map[string]uint64{}
	if st.path == "" {
		return
	}
	data, err := os.ReadFile(st.path)
	if err != nil {
		return
	}
	var f fleetClarifyFile
	if json.Unmarshal(data, &f) != nil {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	for id, e := range f.Clarify {
		if e == nil || len(id) < 1 || len(id) > 200 {
			continue
		}
		var turns []clarifyTurn
		for _, t := range e.Turns {
			if clarifyRoleOK(t.Role) {
				turns = append(turns, t)
			}
		}
		e.Turns = turns
		// A fork does not survive a restart: say so instead of waiting
		// for an answer that will never come.
		if e.Pending != nil {
			e.Pending = nil
			appendClarifyTurn(e, clarifyTurn{Role: clarifyRoleError, Text: clarifyInterrupted, At: now})
		}
		st.entries[id] = e
	}
}

func (st *fleetClarifyStore) saveLocked() {
	if st.path == "" {
		return
	}
	f := fleetClarifyFile{Clarify: st.entries}
	if f.Clarify == nil {
		f.Clarify = map[string]*clarifyEntry{}
	}
	data, err := json.Marshal(f)
	if err == nil {
		err = writeFileAtomic(st.path, append(data, '\n'), 0o600)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "fleet: cannot save the clarify store: %v\n", err)
	}
}

// get returns a deep copy of the card's entry.
func (st *fleetClarifyStore) get(cardID string) (clarifyEntry, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	e, ok := st.entries[cardID]
	if !ok {
		return clarifyEntry{}, false
	}
	return copyClarify(*e), true
}

// begin marks a fork as started for the card. init seeds an entry that does
// not exist yet and is ignored otherwise. An ask adds the question as a you
// turn. It returns a copy of the entry, a token for finish (a fork that was
// pruned away and replaced must not finish into its successor) and a refusal
// code: "busy" (the card
// already has a fork, or clarifyMaxRunning are in flight), "empty" (a draft of
// a chat with no turns or no fork yet: a draft must come from a fork that
// remembers the chat) or "card_gone" (an ask with no entry and no init: the
// chat was pruned since the caller looked). cancel is called if the entry is
// pruned while the fork runs; it may be nil.
func (st *fleetClarifyStore) begin(cardID, kind string, init *clarifyEntry, question string, cancel context.CancelFunc, now time.Time) (clarifyEntry, uint64, string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	e, ok := st.entries[cardID]
	if ok && e.Pending != nil {
		return clarifyEntry{}, 0, "busy"
	}
	if kind == clarifyKindDraft && (!ok || len(e.Turns) == 0 || e.ForkID == "") {
		return clarifyEntry{}, 0, "empty"
	}
	if !ok && init == nil {
		return clarifyEntry{}, 0, "card_gone"
	}
	if st.running >= clarifyMaxRunning {
		return clarifyEntry{}, 0, "busy"
	}
	if !ok {
		seed := copyClarify(*init)
		e = &seed
		st.entries[cardID] = e
	}
	at := now.UTC().Format(time.RFC3339)
	if kind == clarifyKindAsk {
		appendClarifyTurn(e, clarifyTurn{Role: clarifyRoleYou, Text: question, At: at})
	}
	e.Pending = &clarifyPending{Kind: kind, StartedAt: at}
	if cancel != nil {
		st.cancels[cardID] = cancel
	}
	st.seq++
	st.tokens[cardID] = st.seq
	st.running++
	st.saveLocked()
	return copyClarify(*e), st.seq, ""
}

// finish ends the card's fork: it clears Pending, sets ForkID when the entry
// has none, and adds the turn and/or the draft. It does nothing, and does not
// touch running, unless the entry exists, has a fork in flight and token is
// the one begin handed out for it (a prune already ended any other).
func (st *fleetClarifyStore) finish(cardID string, token uint64, forkID string, turn *clarifyTurn, draft *clarifyDraft, now time.Time) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	e, ok := st.entries[cardID]
	if !ok || e.Pending == nil || st.tokens[cardID] != token {
		return
	}
	e.Pending = nil
	delete(st.cancels, cardID)
	delete(st.tokens, cardID)
	if st.running > 0 {
		st.running--
	}
	if e.ForkID == "" && forkID != "" {
		e.ForkID = forkID
	}
	if turn != nil {
		appendClarifyTurn(e, *turn)
	}
	if draft != nil {
		d := *draft
		e.Draft = &d
	}
	st.saveLocked()
}

// discardDraft drops the card's draft and reports whether there was one.
func (st *fleetClarifyStore) discardDraft(cardID string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	e, ok := st.entries[cardID]
	if !ok || e.Draft == nil {
		return false
	}
	e.Draft = nil
	st.saveLocked()
	return true
}

// dropLocked removes the card's entry, ending its fork if one is running.
func (st *fleetClarifyStore) dropLocked(id string) {
	if e := st.entries[id]; e != nil && e.Pending != nil && st.running > 0 {
		st.running--
	}
	if cancel := st.cancels[id]; cancel != nil {
		cancel()
	}
	delete(st.cancels, id)
	delete(st.tokens, id)
	delete(st.entries, id)
}

// prune drops the chats whose card is no longer a decision or whose answer was
// sent, cancelling a fork still running for them. A chat whose card changed
// gets one note turn. cards nil means state.json was unreadable: nothing
// changes.
func (st *fleetClarifyStore) prune(cards map[string]fleetDecisionCard, answered map[string]fleetAnswer, now time.Time) {
	if cards == nil {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	changed := false
	for id, e := range st.entries {
		cur, ok := cards[id]
		if !ok {
			st.dropLocked(id)
			changed = true
			continue
		}
		if a, has := answered[id]; has && a.latest().Status == fleetAttemptSent {
			st.dropLocked(id)
			changed = true
			continue
		}
		if e.CardHash != cur.Hash && !e.Changed {
			appendClarifyTurn(e, clarifyTurn{Role: clarifyRoleNote, Text: clarifyCardChanged, At: now.UTC().Format(time.RFC3339)})
			e.Changed = true
			changed = true
		}
	}
	if changed {
		st.saveLocked()
	}
}

// summary maps card id to its chat's counts, for /fleet/state.
func (st *fleetClarifyStore) summary() map[string]clarifySummary {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	out := make(map[string]clarifySummary, len(st.entries))
	for id, e := range st.entries {
		out[id] = clarifySummary{Turns: len(e.Turns), Pending: e.Pending != nil, Draft: e.Draft != nil}
	}
	return out
}
