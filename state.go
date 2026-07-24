package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// sessionState is the persisted client-side state for one Claude session, keyed
// by its stable SessionID. Zero-valued fields are omitted on write.
type sessionState struct {
	Group    int    `json:"group,omitempty"`
	Disabled bool   `json:"disabled,omitempty"`
	LastSeen string `json:"last_seen,omitempty"` // RFC3339, UTC
}

// clientState is the on-disk shape of state.json.
type clientState struct {
	Sessions map[string]sessionState `json:"sessions"`
}

// stateMaxAge is how long an unseen entry survives before load-time GC drops it.
const stateMaxAge = 30 * 24 * time.Hour

// SessionStore is the client-machine-local persisted group/disabled state at
// ~/.config/claude-sessions/state.json (alongside servers.yaml). It is only
// touched from the TUI's single-threaded event loop — and read-only by the
// `list` subcommand — so it carries no locking. Mutations save atomically (temp
// file + rename in the same directory). Two concurrent TUIs race last-writer-
// wins, which is accepted.
type SessionStore struct {
	entries map[string]sessionState
	path    string
	now     func() time.Time // injectable clock for tests; nil means time.Now
}

// LoadSessionStore reads and GCs the persisted store. Best-effort: a missing,
// unreadable, or corrupt file yields an empty store rather than an error, so a
// bad config never breaks the live view. A home-dir lookup failure disables
// persistence entirely (empty path) rather than scattering state.json into cwd.
func LoadSessionStore() *SessionStore {
	dir := ConfigDir()
	path := ""
	if dir != "" {
		path = filepath.Join(dir, "state.json")
	}
	return loadSessionStore(path, time.Now)
}

func loadSessionStore(path string, now func() time.Time) *SessionStore {
	s := &SessionStore{
		entries: map[string]sessionState{},
		path:    path,
		now:     now,
	}
	if path == "" {
		return s
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	var cs clientState
	if err := json.Unmarshal(data, &cs); err != nil {
		return s // corrupt file: start fresh rather than lose the live view
	}
	if cs.Sessions != nil {
		s.entries = cs.Sessions
	}
	s.gc(s.clock())
	return s
}

func (s *SessionStore) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func (s *SessionStore) nowStamp() string {
	return s.clock().UTC().Format(time.RFC3339)
}

// gc drops entries that carry no state (neither group nor disabled) and entries
// last seen more than stateMaxAge ago. An empty or unparseable last_seen on an
// otherwise-meaningful entry is left alone rather than risk losing state on a
// clock/format glitch.
func (s *SessionStore) gc(now time.Time) {
	cutoff := now.Add(-stateMaxAge)
	for id, e := range s.entries {
		if e.Group == 0 && !e.Disabled {
			delete(s.entries, id)
			continue
		}
		if e.LastSeen == "" {
			continue
		}
		if t, err := time.Parse(time.RFC3339, e.LastSeen); err == nil && t.Before(cutoff) {
			delete(s.entries, id)
		}
	}
}

// Group returns the group (1..9) assigned to sessionID, or 0 if none.
func (s *SessionStore) Group(sessionID string) int {
	return s.entries[sessionID].Group
}

// Disabled reports whether sessionID is marked disabled.
func (s *SessionStore) Disabled(sessionID string) bool {
	return s.entries[sessionID].Disabled
}

// OverlayDisabled sets each session's Disabled flag from the store, overwriting
// whatever the collector or a remote server reported. Sessions with no entry (or
// no SessionID) become enabled. This is the sole authority for disabled state on
// the client now.
func (s *SessionStore) OverlayDisabled(sessions []Session) {
	for i := range sessions {
		sessions[i].Disabled = s.Disabled(sessions[i].SessionID)
	}
}

// GroupsMap returns a snapshot of every grouped session's assignment, for badge
// rendering and filtering. Ungrouped sessions are absent.
func (s *SessionStore) GroupsMap() map[string]int {
	m := make(map[string]int, len(s.entries))
	for id, e := range s.entries {
		if e.Group != 0 {
			m[id] = e.Group
		}
	}
	return m
}

// SetGroup assigns group (1..9) to sessionID, or ungroups it when group equals
// its current group (single-membership toggle: assigning replaces any previous
// group). visibleIDs are the sessions currently in view, whose last_seen the
// ensuing save refreshes. A blank sessionID is ignored.
func (s *SessionStore) SetGroup(sessionID string, group int, visibleIDs []string) {
	if sessionID == "" {
		return
	}
	e := s.entries[sessionID]
	if e.Group == group {
		e.Group = 0
	} else {
		e.Group = group
	}
	s.set(sessionID, e)
	s.persist(visibleIDs)
}

// SetDisabled marks sessionID disabled or enabled. See SetGroup for visibleIDs.
func (s *SessionStore) SetDisabled(sessionID string, disabled bool, visibleIDs []string) {
	if sessionID == "" {
		return
	}
	e := s.entries[sessionID]
	e.Disabled = disabled
	s.set(sessionID, e)
	s.persist(visibleIDs)
}

// set writes an entry, stamping its last_seen, or deletes it when it no longer
// carries any state — keeping the file free of last_seen-only junk that would
// otherwise linger until the next load-time GC.
func (s *SessionStore) set(sessionID string, e sessionState) {
	if e.Group == 0 && !e.Disabled {
		delete(s.entries, sessionID)
		return
	}
	e.LastSeen = s.nowStamp()
	s.entries[sessionID] = e
}

// TouchVisible refreshes last_seen for every visible session that already has
// an entry and saves if anything changed. Mutations already do this on save;
// this exists for plain viewing, so a session that stays grouped/disabled but
// untouched for 30 days isn't dropped by load-time GC while it's still being
// looked at. The caller throttles it (see settleRows) — every visible entry
// gets the same stamp, so calling it more often than the GC horizon matters
// only as file-write churn.
func (s *SessionStore) TouchVisible(visibleIDs []string) {
	stamp := s.nowStamp()
	changed := false
	for _, id := range visibleIDs {
		if e, ok := s.entries[id]; ok && e.LastSeen != stamp {
			e.LastSeen = stamp
			s.entries[id] = e
			changed = true
		}
	}
	if changed {
		s.save()
	}
}

// persist refreshes last_seen for every visible session that already has an
// entry (never creating one for an ungrouped/enabled session), then writes the
// file atomically.
func (s *SessionStore) persist(visibleIDs []string) {
	stamp := s.nowStamp()
	for _, id := range visibleIDs {
		if e, ok := s.entries[id]; ok {
			e.LastSeen = stamp
			s.entries[id] = e
		}
	}
	s.save()
}

// save writes state.json atomically: a temp file in the same directory followed
// by rename. Best-effort — an unwritable config dir is silently ignored, like
// the other client-state writers in config.go.
func (s *SessionStore) save() {
	if s.path == "" {
		return
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	data, err := json.MarshalIndent(clientState{Sessions: s.entries}, "", "  ")
	if err != nil {
		return
	}
	tmp, err := os.CreateTemp(dir, ".state-*.json")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		_ = os.Remove(tmpName)
	}
}
