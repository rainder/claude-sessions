package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	fleetAnsweredKeep        = 24 * time.Hour
	fleetAnsweredMaxAttempts = 3
	fleetAttemptSent         = "sent"
	fleetAttemptUnconfirmed  = "unconfirmed"
)

// fleetAttempt is one answer sent for a card. The text is never stored, only
// whether a comment rode with it.
type fleetAttempt struct {
	OptionKey string `json:"optionKey"`
	Label     string `json:"label"`
	Comment   bool   `json:"comment,omitempty"`
	At        string `json:"at"`     // RFC3339 UTC
	Status    string `json:"status"` // fleetAttemptSent | fleetAttemptUnconfirmed
}

// fleetAnswer is a card's answer history: the hash the card had at the last
// attempt, and the last fleetAnsweredMaxAttempts attempts, newest last.
type fleetAnswer struct {
	CardHash    string         `json:"cardHash"`
	CardChanged bool           `json:"cardChanged,omitempty"`
	Attempts    []fleetAttempt `json:"attempts"`
}

// latest returns the newest attempt, or the zero value when there is none.
func (a fleetAnswer) latest() fleetAttempt {
	if len(a.Attempts) == 0 {
		return fleetAttempt{}
	}
	return a.Attempts[len(a.Attempts)-1]
}

// fleetAnsweredStore remembers which cards were answered, so a second device
// or a restart does not send the same answer twice. Saved to path (0600,
// atomic) only when it changes.
type fleetAnsweredStore struct {
	path string // "" → memory only; cmdServer sets fleetAnsweredPathDefault()

	mu      sync.Mutex
	loaded  bool
	entries map[string]fleetAnswer
}

type fleetAnsweredFile struct {
	Answered map[string]fleetAnswer `json:"answered"`
}

func fleetAnsweredPathDefault() string {
	if dir := claudeSessionsStateDir(); dir != "" {
		return filepath.Join(dir, "fleet-answered.json")
	}
	return ""
}

// validAnswer keeps a loaded entry only when every field is one the store
// itself could have written, and trims it to the last attempts.
func validAnswer(id string, a *fleetAnswer) bool {
	if len(id) < 1 || len(id) > 200 || a.CardHash == "" {
		return false
	}
	var keep []fleetAttempt
	for _, at := range a.Attempts {
		if at.Status != fleetAttemptSent && at.Status != fleetAttemptUnconfirmed {
			continue
		}
		if _, err := time.Parse(time.RFC3339, at.At); err != nil {
			continue
		}
		if len(at.OptionKey) > 100 {
			continue
		}
		keep = append(keep, at)
	}
	if len(keep) == 0 {
		return false
	}
	if len(keep) > fleetAnsweredMaxAttempts {
		keep = keep[len(keep)-fleetAnsweredMaxAttempts:]
	}
	a.Attempts = keep
	return true
}

func (st *fleetAnsweredStore) loadLocked() {
	if st.loaded {
		return
	}
	st.loaded = true
	st.entries = map[string]fleetAnswer{}
	if st.path == "" {
		return
	}
	data, err := os.ReadFile(st.path)
	if err != nil {
		return
	}
	var f fleetAnsweredFile
	if json.Unmarshal(data, &f) != nil {
		return
	}
	for id, a := range f.Answered {
		if validAnswer(id, &a) {
			st.entries[id] = a
		}
	}
}

func (st *fleetAnsweredStore) saveLocked() {
	if st.path == "" {
		return
	}
	f := fleetAnsweredFile{Answered: st.entries}
	if f.Answered == nil {
		f.Answered = map[string]fleetAnswer{}
	}
	data, err := json.Marshal(f)
	if err == nil {
		err = writeFileAtomic(st.path, append(data, '\n'), 0o600)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "fleet: cannot save the answer store: %v\n", err)
	}
}

// pruneLocked drops entries that no longer say anything and reports whether
// anything changed. cards nil means the card list is unknown: nothing is
// removed or flagged. Otherwise an entry goes when its card is no longer a
// decision, when its card changed and the answer was sent, or when its latest
// attempt is older than fleetAnsweredKeep. A changed card whose answer is
// unconfirmed stays, flagged CardChanged, so Andy can look before re-sending.
func (st *fleetAnsweredStore) pruneLocked(cards map[string]fleetDecisionCard, now time.Time) bool {
	if cards == nil {
		return false
	}
	changed := false
	for id, e := range st.entries {
		cur, ok := cards[id]
		if !ok {
			delete(st.entries, id)
			changed = true
			continue
		}
		last := e.latest()
		if at, err := time.Parse(time.RFC3339, last.At); err == nil && now.Sub(at) > fleetAnsweredKeep {
			delete(st.entries, id)
			changed = true
			continue
		}
		differs := cur.Hash != e.CardHash
		if differs && last.Status == fleetAttemptSent {
			delete(st.entries, id)
			changed = true
			continue
		}
		if e.CardChanged != differs {
			e.CardChanged = differs
			st.entries[id] = e
			changed = true
		}
	}
	return changed
}

// get returns a copy of the card's entry.
func (st *fleetAnsweredStore) get(cardID string) (fleetAnswer, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	e, ok := st.entries[cardID]
	e.Attempts = append([]fleetAttempt(nil), e.Attempts...)
	return e, ok
}

// record adds an attempt for the card, keeping the last
// fleetAnsweredMaxAttempts, then prunes against cards and saves.
func (st *fleetAnsweredStore) record(cardID, cardHash string, a fleetAttempt, cards map[string]fleetDecisionCard, now time.Time) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	e := st.entries[cardID]
	e.Attempts = append(e.Attempts, a)
	if len(e.Attempts) > fleetAnsweredMaxAttempts {
		e.Attempts = append([]fleetAttempt(nil), e.Attempts[len(e.Attempts)-fleetAnsweredMaxAttempts:]...)
	}
	e.CardHash = cardHash
	e.CardChanged = false
	st.entries[cardID] = e
	st.pruneLocked(cards, now)
	st.saveLocked()
}

// view prunes against cards (nil → no prune) and returns a deep copy. It
// writes the file only when the prune changed something. Never nil.
func (st *fleetAnsweredStore) view(cards map[string]fleetDecisionCard, now time.Time) map[string]fleetAnswer {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	if st.pruneLocked(cards, now) {
		st.saveLocked()
	}
	out := make(map[string]fleetAnswer, len(st.entries))
	for id, e := range st.entries {
		e.Attempts = append([]fleetAttempt(nil), e.Attempts...)
		out[id] = e
	}
	return out
}

// dismiss forgets the card's entry and reports whether there was one.
func (st *fleetAnsweredStore) dismiss(cardID string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	if _, ok := st.entries[cardID]; !ok {
		return false
	}
	delete(st.entries, cardID)
	st.saveLocked()
	return true
}

// fleetAnswerRefused reports whether a new answer must be refused: the card
// already has a sent answer for this very question, or an unconfirmed one
// (the text may be sitting in the box, whatever the card says now). A
// correction is always allowed.
func fleetAnswerRefused(e fleetAnswer, has bool, curHash string, correction bool) bool {
	if correction || !has {
		return false
	}
	switch e.latest().Status {
	case fleetAttemptUnconfirmed:
		return true
	case fleetAttemptSent:
		return e.CardHash == curHash
	}
	return false
}
