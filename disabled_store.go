package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// disabledEntry is one host-owned disabled-session record. Presence in
// disabledFile.Sessions IS the disabled flag — there is no separate bool,
// mirroring state.go's zero-value-means-absent convention.
type disabledEntry struct {
	LastSeen string `json:"last_seen"` // RFC3339, UTC
}

// disabledFile is the on-disk shape of disabled.json.
type disabledFile struct {
	Sessions map[string]disabledEntry `json:"sessions"`
}

// disabledTouchInterval throttles Overlay's opportunistic last_seen refresh,
// bounding disk writes to at most one per entry per interval no matter how
// often Overlay runs (every TUI render tick, every GET /sessions poll).
const disabledTouchInterval = time.Hour

// DisabledStore is the host-owned, cross-process-safe record of which
// sessions on this host are disabled: ~/.config/claude-sessions/disabled.json.
//
// Unlike SessionStore (state.go, Group only, TUI-single-threaded by
// contract), this store is written from both the TUI (local sessions) and
// the HTTP server (remote mutations via POST /sessions/{pid}/disable) on the
// same host — two different processes. Every access takes an OS file lock
// (flock) around a fresh read from disk: no access ever trusts this
// process's in-memory copy against another process's write. Persistence is
// deliberately NOT atomic-temp-file-plus-rename like state.go's save(): flock
// only protects a stable inode, and a rename would swap the inode out from
// under a lock a concurrent reader/writer already holds on the old one — the
// well-known flock+rename incompatibility. Writes truncate and rewrite the
// locked file in place instead, trading crash-safety (a crash mid-write can
// corrupt the file) for correctness under concurrent access; a corrupt file
// is treated exactly like state.go treats one — start fresh rather than lose
// the live view.
//
// entries/loadedModTime cache the last read so a hot path (render tick, GET
// handler) doesn't decode JSON every call; a plain mutex guards them since
// flock only serializes the file, not this process's own goroutines.
type DisabledStore struct {
	mu            sync.Mutex
	path          string
	entries       map[string]disabledEntry
	loadedModTime time.Time
	now           func() time.Time // injectable clock for tests; nil means time.Now
}

// LoadDisabledStore returns a store rooted at ConfigDir()/disabled.json. A
// missing home directory disables persistence (empty path): reads report "not
// disabled" and writes are no-ops, matching SessionStore's convention of
// never breaking the live view over a config-dir problem.
func LoadDisabledStore() *DisabledStore {
	dir := ConfigDir()
	path := ""
	if dir != "" {
		path = filepath.Join(dir, "disabled.json")
	}
	return newDisabledStore(path, time.Now)
}

func newDisabledStore(path string, now func() time.Time) *DisabledStore {
	return &DisabledStore{path: path, now: now, entries: map[string]disabledEntry{}}
}

func (d *DisabledStore) clock() time.Time {
	if d.now != nil {
		return d.now()
	}
	return time.Now()
}

// Disabled reports whether sessionID is currently marked disabled on this
// host.
func (d *DisabledStore) Disabled(sessionID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.reloadIfStaleLocked()
	_, ok := d.entries[sessionID]
	return ok
}

// Overlay sets each session's Disabled field from the store (no entry ->
// false), then opportunistically touches last_seen for every
// currently-observed session that already has an entry stale by more than
// disabledTouchInterval. This is the store's only GC-keepalive mechanism:
// whichever caller passes it a live-session list (the TUI for its own local
// sessions, the server for its own collected list) keeps that session's
// entry alive past the 30-day retention window for as long as it keeps
// showing up here.
func (d *DisabledStore) Overlay(sessions []Session) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.reloadIfStaleLocked()

	now := d.clock()
	var touch []string
	for i := range sessions {
		id := sessions[i].SessionID
		if id == "" {
			continue
		}
		e, ok := d.entries[id]
		sessions[i].Disabled = ok
		if !ok {
			continue
		}
		if last, err := time.Parse(time.RFC3339, e.LastSeen); err != nil || now.Sub(last) >= disabledTouchInterval {
			touch = append(touch, id)
		}
	}
	if len(touch) == 0 {
		return
	}
	stamp := now.UTC().Format(time.RFC3339)
	d.mutateLocked(func(entries map[string]disabledEntry) bool {
		changed := false
		for _, id := range touch {
			if _, ok := entries[id]; ok {
				entries[id] = disabledEntry{LastSeen: stamp}
				changed = true
			}
		}
		return changed
	})
}

// SetDisabled marks sessionID disabled or enabled on this host. Disabling
// creates/refreshes the entry; enabling deletes it — presence is the flag,
// so there is nothing to zero out. A blank sessionID is ignored.
func (d *DisabledStore) SetDisabled(sessionID string, disabled bool) {
	if sessionID == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	stamp := d.clock().UTC().Format(time.RFC3339)
	d.mutateLocked(func(entries map[string]disabledEntry) bool {
		if disabled {
			entries[sessionID] = disabledEntry{LastSeen: stamp}
			return true
		}
		if _, ok := entries[sessionID]; !ok {
			return false
		}
		delete(entries, sessionID)
		return true
	})
}

// reloadIfStaleLocked refreshes d.entries from disk under a shared lock if
// the file's mtime has advanced past what this instance last loaded. Called
// with d.mu already held.
func (d *DisabledStore) reloadIfStaleLocked() {
	if d.path == "" {
		return
	}
	info, err := os.Stat(d.path)
	if err != nil {
		return // missing/unreadable: keep whatever is already cached (likely empty)
	}
	if !info.ModTime().After(d.loadedModTime) {
		return
	}
	entries, modTime, ok := d.readLocked(unix.LOCK_SH)
	if !ok {
		return
	}
	d.entries = entries
	d.loadedModTime = modTime
}

// mutateLocked runs fn against a freshly-read copy of the on-disk entries —
// never d.entries, since another process may have written since this
// instance last loaded — under an exclusive lock, persists the result if fn
// (or GC) changed anything, and refreshes the in-memory cache either way.
// Called with d.mu already held.
func (d *DisabledStore) mutateLocked(fn func(map[string]disabledEntry) bool) {
	if d.path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(d.path), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(d.path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		return
	}
	defer func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN) }()

	entries := decodeDisabledFile(f)
	gcChanged := gcDisabledEntries(entries, d.clock())
	changed := fn(entries) || gcChanged
	if changed {
		data, err := json.MarshalIndent(disabledFile{Sessions: entries}, "", "  ")
		if err != nil {
			return
		}
		if err := f.Truncate(0); err != nil {
			return
		}
		if _, err := f.WriteAt(data, 0); err != nil {
			return
		}
	}
	d.entries = entries
	if info, err := f.Stat(); err == nil {
		d.loadedModTime = info.ModTime()
	}
}

// readLocked opens the file under the given flock mode (LOCK_SH or LOCK_EX),
// decodes it, and returns the entries plus the mtime observed at read time.
// ok is false on any failure to open/lock/stat — callers keep their existing
// cache rather than clobber it with an empty one.
func (d *DisabledStore) readLocked(how int) (map[string]disabledEntry, time.Time, bool) {
	f, err := os.Open(d.path)
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
	return decodeDisabledFile(f), info.ModTime(), true
}

// decodeDisabledFile reads and decodes f's full contents. A missing, empty,
// or corrupt file yields an empty map rather than an error — best-effort,
// like state.go's corrupt-file handling: start fresh rather than lose the
// live view.
func decodeDisabledFile(f *os.File) map[string]disabledEntry {
	data, err := io.ReadAll(f)
	if err != nil {
		return map[string]disabledEntry{}
	}
	var df disabledFile
	if err := json.Unmarshal(data, &df); err != nil || df.Sessions == nil {
		return map[string]disabledEntry{}
	}
	return df.Sessions
}

// gcDisabledEntries drops entries last seen more than stateMaxAge ago (the
// same 30-day retention state.go uses, state.go:24). An empty or unparseable
// last_seen is left alone rather than risk losing state on a clock/format
// glitch. Reports whether anything was dropped.
func gcDisabledEntries(entries map[string]disabledEntry, now time.Time) bool {
	cutoff := now.Add(-stateMaxAge)
	changed := false
	for id, e := range entries {
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
