package main

import "sort"

// notifyEventKind distinguishes "the user is needed" from "no longer needed".
type notifyEventKind int

const (
	notifyAlert notifyEventKind = iota
	notifyClear
)

// notifyEvent is one thing worth telling a phone about.
type notifyEvent struct {
	Kind       notifyEventKind
	SessionID  string
	PID        int
	CWD        string
	Name       string
	WaitingFor string
	// Generation increments each time this session enters a distinct wait.
	// It rides in the push payload as part of event_id so a later phase can
	// refuse to act on a notification whose prompt has since been answered.
	Generation int
}

// waitPhase is where one session sits in the wait state machine.
type waitPhase int

const (
	phaseIdle    waitPhase = iota // not waiting
	phasePending                  // waiting, not yet long enough to alert
	phaseAlerted                  // alert sent for the current generation
)

// waitEntry is the tracker's per-session memory.
//
// It keeps identity as well as phase so a clear produced by absence — where
// there is no snapshot to read from — can still say which session ended.
type waitEntry struct {
	phase      waitPhase
	waitingFor string
	generation int
	pid        int
	cwd        string
	name       string
	// missed counts consecutive ticks in which the session was absent.
	missed int
}

// missedTicksBeforeClear is how many consecutive absences mean "gone".
//
// readSessionFile silently rejects a partially-written JSON file, so a single
// absence is indistinguishable from Claude Code being mid-write. Clearing on one
// would produce a spurious clear immediately followed by a re-alert.
const missedTicksBeforeClear = 2

// waitTracker turns a stream of session snapshots into alert/clear events.
//
// It is an explicit state machine rather than a diff of the previous tick,
// because a diff cannot express the debounce: by the second waiting tick the
// transition reads waiting -> waiting, so an edge test stops firing and the
// alert is dropped instead of delayed.
//
// The tracker is not safe for concurrent use; it is owned by the single ticker
// goroutine.
type waitTracker struct {
	entries   map[string]*waitEntry
	baselined bool
	// nextGen is a monotonic counter across the tracker's whole life, not a
	// per-entry one. An entry evicted after two missed ticks would otherwise
	// restart at 1, so a session that waits, disappears, and waits again would
	// reuse an event_id — and a stale notification for the first wait would pass
	// the freshness check on the second.
	nextGen int
}

func newWaitTracker() *waitTracker {
	return &waitTracker{entries: map[string]*waitEntry{}}
}

// Tick folds one snapshot in and returns the events it produced, sorted by
// session ID so output is deterministic.
//
// The first call establishes a silent baseline: sessions already waiting when
// the process starts move straight to alerted without notifying. A restart is a
// local action taken at the keyboard, and the app's session list is ground
// truth for anything that was already waiting.
func (t *waitTracker) Tick(sessions []Session) []notifyEvent {
	var events []notifyEvent
	seen := make(map[string]bool, len(sessions))

	for _, s := range sessions {
		if s.SessionID == "" {
			continue
		}
		if seen[s.SessionID] {
			// Two rows for one session in a single snapshot would advance the
			// machine twice and defeat the debounce. Shouldn't happen, but the
			// input is a directory listing, not a guarantee.
			continue
		}
		seen[s.SessionID] = true

		e, ok := t.entries[s.SessionID]
		if !ok {
			e = &waitEntry{}
			t.entries[s.SessionID] = e
		}
		// A resumed or migrated session keeps its SessionID but gets a new PID.
		// That is a different process and a different wait: start it over rather
		// than letting it inherit phaseAlerted and never notify again.
		if e.pid != 0 && e.pid != s.PID {
			e.phase = phaseIdle
			e.waitingFor = ""
		}
		e.missed = 0
		e.pid, e.cwd, e.name = s.PID, s.CWD, s.Name

		if !s.Waiting() {
			if e.phase == phaseAlerted {
				events = append(events, t.event(notifyClear, s, e))
			}
			e.phase = phaseIdle
			e.waitingFor = ""
			continue
		}

		if !t.baselined {
			// Already waiting at startup: adopt the state without notifying.
			e.phase = phaseAlerted
			e.waitingFor = s.WaitingFor
			e.generation = t.bumpGeneration()
			continue
		}

		switch e.phase {
		case phaseIdle:
			e.phase = phasePending
			e.waitingFor = s.WaitingFor
		case phasePending:
			e.phase = phaseAlerted
			e.waitingFor = s.WaitingFor
			e.generation = t.bumpGeneration()
			events = append(events, t.event(notifyAlert, s, e))
		case phaseAlerted:
			// A different prompt without an intervening non-waiting tick is a
			// new thing to notify about.
			if e.waitingFor != s.WaitingFor {
				e.waitingFor = s.WaitingFor
				e.generation = t.bumpGeneration()
				events = append(events, t.event(notifyAlert, s, e))
			}
		}
	}

	for id, e := range t.entries {
		if seen[id] {
			continue
		}
		e.missed++
		if e.missed < missedTicksBeforeClear {
			continue
		}
		if e.phase == phaseAlerted {
			events = append(events, notifyEvent{
				Kind:       notifyClear,
				SessionID:  id,
				PID:        e.pid,
				CWD:        e.cwd,
				Name:       e.name,
				WaitingFor: e.waitingFor,
				Generation: e.generation,
			})
		}
		delete(t.entries, id)
	}

	t.baselined = true
	sort.Slice(events, func(i, j int) bool { return events[i].SessionID < events[j].SessionID })
	return events
}

// bumpGeneration hands out the next wait generation. Monotonic across the
// tracker, so an id is never reused after an entry is evicted.
func (t *waitTracker) bumpGeneration() int {
	t.nextGen++
	return t.nextGen
}

func (t *waitTracker) event(kind notifyEventKind, s Session, e *waitEntry) notifyEvent {
	return notifyEvent{
		Kind:       kind,
		SessionID:  s.SessionID,
		PID:        s.PID,
		CWD:        s.CWD,
		Name:       s.Name,
		WaitingFor: s.WaitingFor,
		Generation: e.generation,
	}
}
