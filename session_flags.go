package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// sessionFlags is one session's shared per-host flags. Zero-valued fields are
// omitted on write, so an entry carrying neither a group nor the disabled bit
// is meaningless and gets deleted rather than written back as `{}`.
type sessionFlags struct {
	Group    int    `json:"group,omitempty"`    // 1..9, 0 = ungrouped
	Disabled bool   `json:"disabled,omitempty"` // greyed + hideable in every view
	LastSeen string `json:"last_seen,omitempty"`
}

// meaningful reports whether the entry still carries state worth persisting.
func (f sessionFlags) meaningful() bool { return f.Group != 0 || f.Disabled }

// flagsFileName is the shared store's basename under ConfigDir().
const flagsFileName = "session-flags.json"

// flagsTouchInterval throttles Overlay's opportunistic last_seen refresh,
// bounding disk writes to at most one per entry per interval no matter how
// often Overlay runs (every TUI render tick, every GET /sessions poll).
const flagsTouchInterval = time.Hour

// flagsMaxAge is how long an entry survives with no keepalive touch before GC
// drops it — the same 30-day retention the legacy stores used.
const flagsMaxAge = 30 * 24 * time.Hour

// FlagsStore is the host-owned, cross-process-safe record of the per-session
// flags this host shares with every client of it: group assignment (1..9) and
// the disabled bit. On disk it is a single object keyed by session id at
// ~/.config/claude-sessions/session-flags.json:
//
//	{"<session_id>": {"group": 3, "disabled": true, "last_seen": "..."}}
//
// It replaces two older, split stores — state.json (groups, TUI-local) and
// disabled.json (disabled, host-owned) — which are merged into it once on
// first load and left behind renamed `.bak` (see migrateLegacyFlags).
//
// Both the TUI (local rows) and the HTTP server (remote mutations via POST
// /sessions/{pid}/flags and /sessions/{pid}/disable) write it on the same
// host — two different processes. Every access takes an OS file lock (flock)
// around a fresh read from disk: no access ever trusts this process's
// in-memory copy against another process's write. Persistence is deliberately
// NOT atomic-temp-file-plus-rename: flock only protects a stable inode, and a
// rename would swap the inode out from under a lock a concurrent
// reader/writer already holds on the old one — the well-known flock+rename
// incompatibility. Writes truncate and rewrite the locked file in place
// instead, trading crash-safety for correctness under concurrent access.
//
// A file that fails to parse is NOT treated as empty: unlike the two stores
// this replaces, which started fresh and overwrote a corrupt file on the next
// mutation, this one flips readOnly, complains once on stderr and refuses to
// write — the DeviceStore discipline (devices.go), applied here because a
// silent wipe now loses every group badge and disabled mark on the host at
// once.
//
// entries/loadedModTime cache the last read so a hot path (render tick, GET
// handler) doesn't decode JSON every call; a plain mutex guards them since
// flock only serializes the file, not this process's own goroutines.
type FlagsStore struct {
	mu            sync.Mutex
	path          string
	entries       map[string]sessionFlags
	loadedModTime time.Time
	readOnly      bool // set when the file on disk failed to parse
	complained    bool // stderr gets the corruption notice once per process
	now           func() time.Time
	// resolvable reports the session ids that still resolve to a live-or-
	// resumable session on this host, and whether that answer is trustworthy.
	// Entries outside the set are pruned on write. nil (tests) or ok=false
	// (no home directory, unreadable transcript dir) prunes nothing — losing
	// every badge because a directory read failed is worse than an entry that
	// outlives its session, which the last_seen GC drops anyway.
	resolvable func() (map[string]bool, bool)
}

// LoadFlagsStore returns the store rooted at ConfigDir()/session-flags.json,
// migrating the legacy state.json/disabled.json pair into it on first use, and
// putting the one-time migration notice on stderr. A missing home directory
// disables persistence (empty path): reads report the zero flags and writes
// are no-ops, so a config-dir problem never breaks the live view.
func LoadFlagsStore() *FlagsStore {
	s, notice := LoadFlagsStoreWithNotice()
	if notice != "" {
		fmt.Fprintf(os.Stderr, "claude-sessions: %s\n", notice)
	}
	return s
}

// LoadFlagsStoreWithNotice is LoadFlagsStore for a caller that owns the
// terminal: the one-time migration notice is handed back instead of written to
// stderr, because the TUI runs on the alternate screen and its first full
// repaint paints straight over anything printed there. Empty when no migration
// happened.
func LoadFlagsStoreWithNotice() (*FlagsStore, string) {
	dir := ConfigDir()
	path := ""
	notice := ""
	if dir != "" {
		path = filepath.Join(dir, flagsFileName)
		notice = migrateLegacyFlags(dir, time.Now)
	}
	return newFlagsStore(path, time.Now, resolvableSessionIDs), notice
}

func newFlagsStore(path string, now func() time.Time, resolvable func() (map[string]bool, bool)) *FlagsStore {
	s := &FlagsStore{
		path:       path,
		entries:    map[string]sessionFlags{},
		now:        now,
		resolvable: resolvable,
	}
	// Read once up front so a corrupt file is reported at startup rather than
	// at the first mutation, matching loadDeviceStore's timing.
	s.mu.Lock()
	s.reloadIfStaleLocked()
	s.mu.Unlock()
	return s
}

func (s *FlagsStore) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// Flags returns the flags recorded for sessionID (the zero value when none).
func (s *FlagsStore) Flags(sessionID string) sessionFlags {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadIfStaleLocked()
	return s.entries[sessionID]
}

// Disabled reports whether sessionID is currently marked disabled on this host.
func (s *FlagsStore) Disabled(sessionID string) bool { return s.Flags(sessionID).Disabled }

// Group returns the group (1..9) assigned to sessionID, or 0 if none.
func (s *FlagsStore) Group(sessionID string) int { return s.Flags(sessionID).Group }

// Overlay sets each session's Group and Disabled fields from the store (no
// entry -> zero), then opportunistically touches last_seen for every
// currently-observed session that already has an entry stale by more than
// flagsTouchInterval. This is the store's only GC-keepalive mechanism:
// whichever caller passes it a live-session list (the TUI for its own local
// sessions, the server for its own collected list) keeps that session's entry
// alive past the retention window for as long as it keeps showing up here.
func (s *FlagsStore) Overlay(sessions []Session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadIfStaleLocked()

	now := s.clock()
	var touch []string
	for i := range sessions {
		id := sessions[i].SessionID
		if id == "" {
			continue
		}
		e, ok := s.entries[id]
		sessions[i].Disabled = e.Disabled
		sessions[i].Group = e.Group
		if !ok {
			continue
		}
		if last, err := time.Parse(time.RFC3339, e.LastSeen); err != nil || now.Sub(last) >= flagsTouchInterval {
			touch = append(touch, id)
		}
	}
	if len(touch) == 0 {
		return
	}
	stamp := now.UTC().Format(time.RFC3339)
	s.mutateLocked(func(entries map[string]sessionFlags) bool {
		changed := false
		for _, id := range touch {
			if e, ok := entries[id]; ok {
				e.LastSeen = stamp
				entries[id] = e
				changed = true
			}
		}
		return changed
	})
}

// SetFlags applies the requested changes to sessionID and returns the entry as
// it stands afterwards, plus whether the store could actually persist it. A
// nil group or disabled leaves that field alone — "absent means unchanged",
// the same distinction POST /sessions/{pid}/flags draws on the wire. group 0
// clears the assignment. An entry left carrying neither a group nor the
// disabled bit is deleted rather than written back as `{}`. A blank sessionID
// is ignored.
//
// ok is false when there is nowhere to write (no config dir) or the file is
// being left alone because it failed to parse. Callers that answer a client
// must say so rather than report a success that no reload will show.
func (s *FlagsStore) SetFlags(sessionID string, group *int, disabled *bool) (sessionFlags, bool) {
	if sessionID == "" {
		return sessionFlags{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	stamp := s.clock().UTC().Format(time.RFC3339)
	var result sessionFlags
	ok := s.mutateLocked(func(entries map[string]sessionFlags) bool {
		before, existed := entries[sessionID]
		e := before
		if group != nil {
			e.Group = *group
		}
		if disabled != nil {
			e.Disabled = *disabled
		}
		if !e.meaningful() {
			result = sessionFlags{}
			if !existed {
				return false
			}
			delete(entries, sessionID)
			return true
		}
		e.LastSeen = stamp
		entries[sessionID] = e
		result = e
		return !existed || before.Group != e.Group || before.Disabled != e.Disabled
	})
	return result, ok
}

// SetDisabled marks sessionID disabled or enabled on this host, leaving its
// group alone. Reports whether the change reached disk.
func (s *FlagsStore) SetDisabled(sessionID string, disabled bool) bool {
	_, ok := s.SetFlags(sessionID, nil, &disabled)
	return ok
}

// SetGroup assigns group (1..9) to sessionID, or clears the assignment when
// group is 0, leaving the disabled bit alone. Reports whether the change
// reached disk.
func (s *FlagsStore) SetGroup(sessionID string, group int) bool {
	_, ok := s.SetFlags(sessionID, &group, nil)
	return ok
}

// toggleGroup resolves a group keypress against the group a row already
// carries: the same digit again ungroups it, any other digit replaces it
// (single membership).
func toggleGroup(current, pressed int) int {
	if current == pressed {
		return 0
	}
	return pressed
}

// reloadIfStaleLocked refreshes s.entries from disk under a shared lock if the
// file's mtime has advanced past what this instance last loaded. Called with
// s.mu already held.
func (s *FlagsStore) reloadIfStaleLocked() {
	if s.path == "" {
		return
	}
	info, err := os.Stat(s.path)
	if err != nil {
		return // missing/unreadable: keep whatever is already cached (likely empty)
	}
	if !info.ModTime().After(s.loadedModTime) {
		return
	}
	entries, modTime, ok := s.readLocked(unix.LOCK_SH)
	if !ok {
		return
	}
	s.entries = entries
	s.loadedModTime = modTime
}

// mutateLocked runs fn against a freshly-read copy of the on-disk entries —
// never s.entries, since another process may have written since this instance
// last loaded — under an exclusive lock, persists the result if fn (or
// GC/pruning) changed anything, and refreshes the in-memory cache either way.
// A file that fails to parse stops the whole thing: the mutation is dropped
// and the bytes on disk are left exactly as they are. Reports whether the
// result is on disk — false covers "nowhere to write", "not touching a corrupt
// file", and every I/O failure, so a caller answering a client never claims a
// write that no reload will show. Called with s.mu already held.
func (s *FlagsStore) mutateLocked(fn func(map[string]sessionFlags) bool) bool {
	if s.path == "" || s.readOnly {
		return false
	}
	// Resolve liveness BEFORE taking the lock: it walks the session and
	// transcript directories, and Overlay takes this same lock on every render
	// tick and every GET /sessions.
	var live map[string]bool
	liveOK := false
	if s.resolvable != nil {
		live, liveOK = s.resolvable()
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return false
	}
	f, err := os.OpenFile(s.path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return false
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		return false
	}
	defer func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN) }()

	entries, ok := decodeFlagsFile(f)
	if !ok {
		s.markCorrupt()
		return false
	}
	changed := fn(entries)
	if pruneFlagEntries(entries, live, liveOK, s.clock()) {
		changed = true
	}
	written := true
	if changed {
		written = writeFlagsFile(f, entries)
	}
	s.entries = entries
	if info, err := f.Stat(); err == nil {
		s.loadedModTime = info.ModTime()
	}
	return written
}

// flagsFile is the part of the already-locked *os.File writeFlagsFile uses.
// An interface only so a test can stand in a writer whose disk fails.
type flagsFile interface {
	WriteAt(b []byte, off int64) (int, error)
	Truncate(size int64) error
}

// writeFlagsFile rewrites the already-locked f in place: the new bytes go down
// FIRST, and the file is shortened to fit them only once that write succeeded.
//
// The order is the whole point and must not be tidied back. Truncating first
// would mean any plain write failure — ENOSPC, EIO, no crash needed — leaves a
// 0-byte file, and decodeFlagsFile reads 0 bytes as a legitimately empty store
// rather than a corrupt one: the read-only latch never engages, the next write
// cements it, and every group badge and disabled mark on the host is silently
// gone. Writing first leaves the old bytes in place instead, and a write that
// landed but could not be shortened leaves the old tail past the new data,
// which fails to parse and latches the read-only/corrupt state this store
// exists to have. Neither order saves a write torn part-way through: the
// surviving old tail can, over a file of the same shape, happen to leave valid
// JSON describing the wrong flags. That is inherent to rewriting in place,
// which this store chose over atomic rename to keep flock meaningful (see the
// FlagsStore doc comment).
func writeFlagsFile(f flagsFile, entries map[string]sessionFlags) bool {
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return false
	}
	if _, err := f.WriteAt(data, 0); err != nil {
		return false
	}
	if err := f.Truncate(int64(len(data))); err != nil {
		return false
	}
	return true
}

// saveError explains why a write did not reach disk, for the one-line action
// error the TUI shows. The two states the store knows about are "nowhere to
// write" and the read-only latch a file that failed to parse leaves behind;
// anything else was an I/O failure against a path the caller can go look at.
func (s *FlagsStore) saveError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.path == "":
		return fmt.Errorf("no config directory to save session flags in")
	case s.readOnly:
		return fmt.Errorf("%s is corrupt — repair or remove it", s.path)
	}
	return fmt.Errorf("could not write %s", s.path)
}

// markCorrupt latches read-only mode and says so once. Called with s.mu held.
func (s *FlagsStore) markCorrupt() {
	s.readOnly = true
	if s.complained {
		return
	}
	s.complained = true
	fmt.Fprintf(os.Stderr, "claude-sessions: %s is corrupt — leaving it untouched; group and disabled changes will not persist until it is repaired or removed\n", s.path)
}

// readLocked opens the file under the given flock mode (LOCK_SH or LOCK_EX),
// decodes it, and returns the entries plus the mtime observed at read time.
// ok is false on any failure to open/lock/stat, and on a parse failure — which
// also latches read-only mode, so nothing overwrites a file we could not read.
// Callers keep their existing cache rather than clobber it with an empty one.
// Called with s.mu held.
func (s *FlagsStore) readLocked(how int) (map[string]sessionFlags, time.Time, bool) {
	f, err := os.Open(s.path)
	if err != nil {
		return nil, time.Time{}, false
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), how); err != nil {
		return nil, time.Time{}, false
	}
	defer func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN) }()

	info, err := f.Stat()
	if err != nil {
		return nil, time.Time{}, false
	}
	entries, ok := decodeFlagsFile(f)
	if !ok {
		s.markCorrupt()
		return nil, time.Time{}, false
	}
	return entries, info.ModTime(), true
}

// decodeFlagsFile reads and decodes f's full contents from offset 0. An empty
// (or whitespace-only) file is a legitimately empty store; anything that fails
// to parse reports ok=false so the caller can refuse to touch it.
func decodeFlagsFile(f *os.File) (map[string]sessionFlags, bool) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, false
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, false
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return map[string]sessionFlags{}, true
	}
	var entries map[string]sessionFlags
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, false
	}
	if entries == nil { // a literal `null` decodes into a nil map
		return map[string]sessionFlags{}, true
	}
	return entries, true
}

// pruneFlagEntries drops entries that no longer describe anything: those
// carrying no state at all, those whose session id does not resolve to a
// live-or-resumable session on this host, and those last seen more than
// flagsMaxAge ago. Pruning by liveness only happens when the caller could
// actually resolve one (liveOK) and found something — an empty answer is
// treated as "could not tell" rather than "nothing exists", so a machine whose
// session directory momentarily reads as empty keeps its state. An empty or
// unparseable last_seen is left alone rather than risk losing state on a
// clock/format glitch. Reports whether anything was dropped.
func pruneFlagEntries(entries map[string]sessionFlags, live map[string]bool, liveOK bool, now time.Time) bool {
	byLiveness := liveOK && len(live) > 0
	cutoff := now.Add(-flagsMaxAge)
	changed := false
	for id, e := range entries {
		if !e.meaningful() {
			delete(entries, id)
			changed = true
			continue
		}
		if byLiveness && !live[id] {
			delete(entries, id)
			changed = true
			continue
		}
		if e.LastSeen == "" {
			continue
		}
		if t, err := time.Parse(time.RFC3339, e.LastSeen); err == nil && t.Before(cutoff) {
			delete(entries, id)
			changed = true
		}
	}
	return changed
}

// resolvableSessionIDs is the production pruning resolver: every session id
// this host can still show or resume. That is the live sessions plus every
// transcript modified inside resumableMaxAge — a deliberate superset of what
// CollectResumable returns, because that one caps its list at
// resumableMaxCount and parses each transcript head, neither of which a prune
// decision should depend on: the cap would silently drop legitimate state, and
// the parsing would make every write read the transcripts as well as list
// them.
//
// ok is false when the home directory, the session files or the transcript
// glob cannot be read, which prunes nothing.
func resolvableSessionIDs() (map[string]bool, bool) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, false
	}
	sessions, err := CollectLocalLite()
	if err != nil {
		return nil, false
	}
	set := map[string]bool{}
	for _, s := range sessions {
		if s.SessionID != "" {
			set[s.SessionID] = true
		}
	}
	matches, err := filepath.Glob(filepath.Join(home, ".claude", "projects", "*", "*.jsonl"))
	if err != nil {
		return nil, false
	}
	cutoff := time.Now().Add(-resumableMaxAge)
	for _, path := range matches {
		info, err := os.Stat(path)
		if err != nil || info.IsDir() || info.Size() == 0 || info.ModTime().Before(cutoff) {
			continue
		}
		if id := strings.TrimSuffix(filepath.Base(path), ".jsonl"); id != "" {
			set[id] = true
		}
	}
	return set, true
}

// --- migration from the two legacy stores --------------------------------
//
// The shapes below are the on-disk format this store replaces. They exist only
// to be read once, by migrateLegacyFlags, and are never written again.

// legacyDisabledEntry is one record of the old disabled.json, where presence
// in the map WAS the disabled flag.
type legacyDisabledEntry struct {
	LastSeen string `json:"last_seen"`
}

// legacyDisabledFile is the on-disk shape of disabled.json.
type legacyDisabledFile struct {
	Sessions map[string]legacyDisabledEntry `json:"sessions"`
}

// migrateLegacyFlags folds the old state.json (groups, written by the TUI) and
// disabled.json (disabled, written by the server and the TUI) into the shared
// session-flags.json, once, then renames both legacy files to `.bak` — kept,
// never deleted, so a migration that got something wrong is still readable by
// a human.
//
// It runs only when the new file does not exist yet, and takes the new file's
// exclusive lock before deciding, so a TUI and a server starting at the same
// moment cannot both migrate. Nothing is pruned here: the merge is a straight
// copy, and the first real mutation applies the usual pruning rules.
//
// Returns the one-time notice a migration that actually ran deserves, empty
// otherwise. It is returned rather than printed so each caller can put it
// where its own user will see it (see LoadFlagsStoreWithNotice); it is one
// line, front-loaded with the thing the user has to do, because the TUI shows
// it in a single bottom row that a narrow terminal clips.
func migrateLegacyFlags(dir string, now func() time.Time) string {
	path := filepath.Join(dir, flagsFileName)
	// Size, not mere existence: a zero-byte file is what a migration whose
	// write failed leaves behind, and treating that as "already migrated"
	// would strand state.json unread and un-renamed forever. This matches the
	// same test done under the lock below.
	if info, err := os.Stat(path); err == nil && info.Size() > 0 {
		return "" // already migrated
	}
	statePath := filepath.Join(dir, "state.json")
	disabledPath := filepath.Join(dir, "disabled.json")
	_, stateErr := os.Stat(statePath)
	_, disabledErr := os.Stat(disabledPath)
	if stateErr != nil && disabledErr != nil {
		return "" // nothing to migrate
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ""
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return ""
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		return ""
	}
	defer func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN) }()
	if info, err := f.Stat(); err != nil || info.Size() > 0 {
		return "" // another process migrated while we waited for the lock
	}

	data, err := json.MarshalIndent(mergeLegacyFlags(statePath, disabledPath, now), "", "  ")
	if err != nil {
		return ""
	}
	if _, err := f.WriteAt(data, 0); err != nil {
		return ""
	}
	// Renamed only once the new file is safely written: until then the old
	// files are the only copy.
	for _, p := range []string{statePath, disabledPath} {
		if _, err := os.Stat(p); err != nil {
			continue
		}
		if err := os.Rename(p, p+".bak"); err != nil {
			fmt.Fprintf(os.Stderr, "claude-sessions: cannot rename %s: %v\n", p, err)
		}
	}
	// Said out loud, once, for the same reason a corrupt file is: the one
	// thing this migration cannot carry over is a group set on a session
	// another host owns — it was only ever recorded on the machine it was set
	// from, and the first write here prunes it. A user who sees this line can
	// set those again; a silent migration would just lose the badges. The ask
	// comes first so it survives a clipped terminal row.
	return fmt.Sprintf("groups set on sessions another host owns did not carry over — set them again. "+
		"This host's own groups and disabled marks are now in %s (%s and %s kept)",
		path, filepath.Base(statePath)+".bak", filepath.Base(disabledPath)+".bak")
}

// mergeLegacyFlags reads both legacy files into one entry map. Either file
// being absent, unreadable or corrupt contributes nothing rather than aborting
// the migration — the file itself survives as `.bak` for anyone who wants to
// look at it. last_seen is carried over so the move does not reset the
// retention clock; an entry with neither source's stamp gets now's.
func mergeLegacyFlags(statePath, disabledPath string, now func() time.Time) map[string]sessionFlags {
	entries := map[string]sessionFlags{}
	for id, e := range loadSessionStore(statePath, now).entries {
		if e.Group == 0 {
			continue
		}
		entries[id] = sessionFlags{Group: e.Group, LastSeen: e.LastSeen}
	}
	for id, e := range readLegacyDisabled(disabledPath) {
		f := entries[id]
		f.Disabled = true
		if f.LastSeen == "" {
			f.LastSeen = e.LastSeen
		}
		entries[id] = f
	}
	stamp := now().UTC().Format(time.RFC3339)
	for id, e := range entries {
		if e.LastSeen == "" {
			e.LastSeen = stamp
			entries[id] = e
		}
	}
	return entries
}

// readLegacyDisabled decodes the old disabled.json. A missing or corrupt file
// yields no entries.
func readLegacyDisabled(path string) map[string]legacyDisabledEntry {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var df legacyDisabledFile
	if err := json.Unmarshal(data, &df); err != nil {
		fmt.Fprintf(os.Stderr, "claude-sessions: %s is corrupt (%v) — its disabled marks were not migrated; it is kept as %s.bak\n", path, err, path)
		return nil
	}
	return df.Sessions
}
