# Persisted Server-Side Disabled State Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the disable/enable flag (`-`/`+`) authoritative on the host that owns the session, persisted to disk (`~/.config/claude-sessions/disabled.json`), surviving both client and server restarts and visible to every client polling that host.

**Architecture:** A new `DisabledStore` (own file, flock-guarded read-modify-write) replaces `Disabled` in `SessionStore`/`state.json`, which keeps only `Group`. Local sessions read/write it in-process; remote sessions go through a new `POST /sessions/{pid}/disable` that resolves identity the same way kill/migrate do and persists via the server's own `DisabledStore` instance. `GET /sessions` overlays from it instead of hardcoding `false`.

**Tech Stack:** Go stdlib (`encoding/json`, `net/http`, `os`), `golang.org/x/sys/unix` (already a dependency) for `flock`.

## Global Constraints

- No new third-party dependencies (CLAUDE.md: only `golang.org/x/term` and `golang.org/x/sys`).
- `Group` stays entirely in `SessionStore`/`state.json`, untouched by this plan except for removing `Disabled`.
- No migration of existing `state.json` disabled flags — one-time reset, accepted.
- Every new/changed handler follows the existing `actionResult` envelope + `resolveLivePID` refusal convention (`session_mismatch` / `not_live`), never raw 404/409.
- Verify with `go build ./...`, `go vet ./...`, `go test ./...`, `go test -race ./...` after every task that touches Go source.

Spec: `docs/superpowers/specs/2026-07-28-persisted-server-side-disabled-state-design.md`

---

### Task 1: `DisabledStore` core type

**Files:**
- Create: `disabled_store.go`
- Test: `disabled_store_test.go`

**Interfaces:**
- Consumes: `stateMaxAge` (state.go:24, existing 30-day constant).
- Produces: `LoadDisabledStore() *DisabledStore`, `newDisabledStore(path string, now func() time.Time) *DisabledStore` (test seam, mirrors `loadSessionStore`), `(*DisabledStore).Disabled(sessionID string) bool`, `(*DisabledStore).Overlay(sessions []Session)`, `(*DisabledStore).SetDisabled(sessionID string, disabled bool)`. Later tasks call all four.

- [ ] **Step 1: Write the failing tests**

Create `disabled_store_test.go`:

```go
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fixedClock is defined in state_test.go (same package) — reused here.

func TestDisabledStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "disabled.json")
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)

	d := newDisabledStore(path, fixedClock(now))
	d.SetDisabled("alpha", true)

	reloaded := newDisabledStore(path, fixedClock(now))
	if !reloaded.Disabled("alpha") {
		t.Fatal("alpha not disabled after reload")
	}
	if reloaded.Disabled("beta") {
		t.Fatal("beta reported disabled with no entry")
	}
}

func TestDisabledStoreSetFalseRemovesEntry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "disabled.json")
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)

	d := newDisabledStore(path, fixedClock(now))
	d.SetDisabled("alpha", true)
	d.SetDisabled("alpha", false)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var df disabledFile
	if err := json.Unmarshal(data, &df); err != nil {
		t.Fatalf("disabled.json not valid JSON: %v", err)
	}
	if _, ok := df.Sessions["alpha"]; ok {
		t.Fatalf("alpha entry survived re-enable: %#v", df.Sessions)
	}
}

func TestDisabledStoreOverlaySetsFlagAndTouchesStaleEntries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "disabled.json")
	t0 := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)

	d := newDisabledStore(path, fixedClock(t0))
	d.SetDisabled("alpha", true)

	sessions := []Session{{SessionID: "alpha"}, {SessionID: "beta"}, {SessionID: ""}}
	// A later store instance stands in for a different process (or a later
	// render tick) observing the same file after the touch interval elapses.
	later := t0.Add(2 * time.Hour)
	d2 := newDisabledStore(path, fixedClock(later))
	d2.Overlay(sessions)

	if !sessions[0].Disabled {
		t.Fatal("alpha not overlaid as disabled")
	}
	if sessions[1].Disabled {
		t.Fatal("beta (no entry) overlaid as disabled")
	}
	if sessions[2].Disabled {
		t.Fatal("blank SessionID overlaid as disabled")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var df disabledFile
	if err := json.Unmarshal(data, &df); err != nil {
		t.Fatal(err)
	}
	want := later.UTC().Format(time.RFC3339)
	if got := df.Sessions["alpha"].LastSeen; got != want {
		t.Fatalf("alpha last_seen = %q, want touched to %q", got, want)
	}
}

func TestDisabledStoreGCDropsStaleEntries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "disabled.json")
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	old := now.Add(-40 * 24 * time.Hour).UTC().Format(time.RFC3339)

	seed := disabledFile{Sessions: map[string]disabledEntry{"stale": {LastSeen: old}}}
	data, _ := json.MarshalIndent(seed, "", "  ")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	d := newDisabledStore(path, fixedClock(now))
	// Any mutation runs the read-modify-write cycle that GCs the file.
	d.SetDisabled("fresh", true)

	if d.Disabled("stale") {
		t.Fatal("stale entry survived GC")
	}
	if !d.Disabled("fresh") {
		t.Fatal("fresh entry missing")
	}
}

func TestDisabledStoreConcurrentGoroutinesSameInstance(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "disabled.json")
	d := newDisabledStore(path, fixedClock(time.Now()))

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("session-%d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.SetDisabled(id, true)
		}()
	}
	wg.Wait()

	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("session-%d", i)
		if !d.Disabled(id) {
			t.Fatalf("%s not disabled after concurrent writes", id)
		}
	}
}

// TestDisabledStoreCrossInstanceSerialization proves the file lock — not
// just the in-process mutex — is what prevents lost writes: two separate
// DisabledStore instances sharing one path (standing in for the local TUI
// process and the local server process on the same host) write different
// session ids concurrently, and both survive. Reusing SessionStore's
// whole-file-in-memory-then-save approach would have lost whichever instance
// saved second.
func TestDisabledStoreCrossInstanceSerialization(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "disabled.json")
	a := newDisabledStore(path, fixedClock(time.Now()))
	b := newDisabledStore(path, fixedClock(time.Now()))

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); a.SetDisabled("from-a", true) }()
	go func() { defer wg.Done(); b.SetDisabled("from-b", true) }()
	wg.Wait()

	verify := newDisabledStore(path, fixedClock(time.Now()))
	if !verify.Disabled("from-a") {
		t.Fatal("from-a lost to concurrent cross-instance write")
	}
	if !verify.Disabled("from-b") {
		t.Fatal("from-b lost to concurrent cross-instance write")
	}
}

func TestDisabledStoreEmptyPathDisablesPersistence(t *testing.T) {
	d := newDisabledStore("", fixedClock(time.Now()))
	d.SetDisabled("x", true) // must not panic
	if d.Disabled("x") {
		t.Fatal("empty-path store reported disabled without persistence")
	}
}

func TestDisabledStoreCorruptFileStartsFresh(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "disabled.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	d := newDisabledStore(path, fixedClock(time.Now()))
	if d.Disabled("x") {
		t.Fatal("corrupt file reported a disabled session")
	}
	d.SetDisabled("x", true)
	if !d.Disabled("x") {
		t.Fatal("write after corrupt-file recovery did not stick")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./... -run TestDisabledStore -v`
Expected: FAIL — `disabled_store.go` doesn't exist yet (compile error: undefined `newDisabledStore`, `disabledFile`, etc.)

- [ ] **Step 3: Write the implementation**

Create `disabled_store.go`:

```go
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
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./... -run TestDisabledStore -v && go test ./... -race -run TestDisabledStore -v`
Expected: PASS (all `TestDisabledStore*` cases, clean under `-race`)

- [ ] **Step 5: Commit**

```bash
git add disabled_store.go disabled_store_test.go
git commit -m "$(cat <<'EOF'
feat: add flock-guarded DisabledStore

Host-owned, cross-process-safe persistence for the disabled/enable
flag, replacing the client-side-only state.json entry that only
survived on the machine that toggled it.
EOF
)"
```

---

### Task 2: Server-side overlay on `GET /sessions`

**Files:**
- Modify: `server.go:323-378` (server struct), `server.go:391-396` (`collectLocal` doc comment), `server.go:592-622` (`sessions` handler), `server.go:1428-1436` (`cmdServer` construction)
- Test: `server_test.go` (new test near the existing `sessions` handler tests)

**Interfaces:**
- Consumes: `DisabledStore.Overlay(sessions []Session)` (Task 1).
- Produces: `server.disabled *DisabledStore` field, used by Task 3's new handler.

- [ ] **Step 1: Write the failing test**

Add to `server_test.go` (near the other `sessions`-handler tests):

```go
// TestSessionsHandlerOverlaysDisabledState proves GET /sessions no longer
// hardcodes Disabled=false: it reflects the host's own DisabledStore, and
// does not mutate the shared session cache in the process.
func TestSessionsHandlerOverlaysDisabledState(t *testing.T) {
	dir := t.TempDir()
	disabled := newDisabledStore(filepath.Join(dir, "disabled.json"), fixedClock(time.Now()))
	disabled.SetDisabled("dis-1", true)

	s := &server{
		token:    "secret",
		disabled: disabled,
		collect: func() ([]Session, error) {
			return []Session{
				{PID: 1, SessionID: "dis-1"},
				{PID: 2, SessionID: "dis-2"},
			}, nil
		},
	}
	req := httptest.NewRequest("GET", "/sessions", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()

	s.sessions(rec, req)

	var resp struct {
		Sessions []Session `json:"sessions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Sessions) != 2 {
		t.Fatalf("got %d sessions, want 2", len(resp.Sessions))
	}
	if !resp.Sessions[0].Disabled {
		t.Fatal("dis-1 not reported disabled")
	}
	if resp.Sessions[1].Disabled {
		t.Fatal("dis-2 reported disabled with no entry")
	}

	// A second call must see the same result — proves the first call didn't
	// mutate the cached slice in place.
	rec2 := httptest.NewRecorder()
	s.sessions(rec2, req)
	var resp2 struct {
		Sessions []Session `json:"sessions"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &resp2); err != nil {
		t.Fatalf("decode second response: %v", err)
	}
	if !resp2.Sessions[0].Disabled || resp2.Sessions[1].Disabled {
		t.Fatalf("second call diverged: %#v", resp2.Sessions)
	}
}
```

Add `"path/filepath"` to `server_test.go`'s imports if not already present (it is, per commands.go pattern — check the existing import block first and only add if missing).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./... -run TestSessionsHandlerOverlaysDisabledState -v`
Expected: FAIL — `server.disabled` field doesn't exist yet (compile error)

- [ ] **Step 3: Add the field and wire it in**

In `server.go`, add to the `server` struct (after the `attest` field, server.go:357, before `devices`):

```go
	// disabled is this host's persisted disabled-session flag store (see
	// disabled_store.go). Nil only in tests that don't exercise it — Overlay
	// on a nil pointer would panic, so callers below guard for that case only
	// where a test constructs a bare &server{} without one; production always
	// sets it in cmdServer.
	disabled *DisabledStore
```

Update the stale comment at server.go:391-393 (it currently claims Disabled is client-side and hardcoded false — that stopped being accurate once the `sessions` handler overlays below):

```go
// collectLocal returns this host's live sessions, exactly as collected — it
// never carries Disabled state. That's overlaid separately in the `sessions`
// HTTP handler from this host's DisabledStore, so collectLocal's result stays
// the same trusted-metadata source resolveLivePID and friends already use.
func (s *server) collectLocal() ([]Session, error) {
	return s.collectLocalRaw()
}
```

In `cmdServer` (server.go, where `devices := LoadDeviceStore()` and `s := &server{...}` are constructed), add:

```go
	devices := LoadDeviceStore()
	disabledStore := LoadDisabledStore()

	s := &server{
		token:              tok,
		host:               host,
		hostSnapshot:       hostUsageHub.Snapshot,
		usageSnapshot:      usageHub.Snapshot,
		codexUsageSnapshot: codexUsageHub.Snapshot,
		devices:            devices,
		disabled:           disabledStore,
	}
```

In the `sessions` handler (server.go:592-622), overlay onto a copy right after `cachedSessions()`:

```go
func (s *server) sessions(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	sessions, err := s.cachedSessions()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Copy before overlaying: sessions is the shared cache slice, and another
	// concurrent request encoding it must never see a partially-overlaid row.
	sessions = append([]Session(nil), sessions...)
	if s.disabled != nil {
		s.disabled.Overlay(sessions)
	}
	hostUsage := HostUsage{}
	// ... rest unchanged
```

(Leave everything from `hostUsage := HostUsage{}` onward exactly as it is today.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./... -run TestSessionsHandlerOverlaysDisabledState -v && go build ./... && go vet ./...`
Expected: PASS, clean build/vet

- [ ] **Step 5: Commit**

```bash
git add server.go server_test.go
git commit -m "$(cat <<'EOF'
feat: overlay host-owned disabled state onto GET /sessions

Replaces the hardcoded Disabled=false with this host's own
DisabledStore, on a copy of the cached slice so concurrent requests
never see a partially-overlaid row.
EOF
)"
```

---

### Task 3: `POST /sessions/{pid}/disable` endpoint

**Files:**
- Modify: `server.go:42-56` (`actionResult` struct), `server.go` (new handler + mux registration near server.go:1461-1479)
- Test: `server_test.go`

**Interfaces:**
- Consumes: `s.resolveLivePID` (server.go:558), `s.disabled.SetDisabled` (Task 1/2).
- Produces: `POST /sessions/{pid}/disable`, response shape `actionResult{OK, SessionID, Disabled, Error, Code}` — Task 6's `postDisableRemote` decodes this.

- [ ] **Step 1: Write the failing tests**

Add to `server_test.go`:

```go
func TestDisableHandlerSetsDisabledState(t *testing.T) {
	dir := t.TempDir()
	disabled := newDisabledStore(filepath.Join(dir, "disabled.json"), fixedClock(time.Now()))
	s := &server{
		token:    "secret",
		disabled: disabled,
		collect: func() ([]Session, error) {
			return []Session{{PID: 55, SessionID: "sess-55"}}, nil
		},
	}
	body := strings.NewReader(`{"session_id":"sess-55","disabled":true}`)
	req := httptest.NewRequest("POST", "/sessions/55/disable", body)
	req.SetPathValue("pid", "55")
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()

	s.disableSession(rec, req)

	var r actionResult
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !r.OK || r.SessionID != "sess-55" || r.Disabled == nil || !*r.Disabled {
		t.Fatalf("response = %#v, want OK with sess-55 disabled", r)
	}
	if !disabled.Disabled("sess-55") {
		t.Fatal("store not updated")
	}
}

func TestDisableHandlerSessionMismatchRefusesWithoutMutating(t *testing.T) {
	dir := t.TempDir()
	disabled := newDisabledStore(filepath.Join(dir, "disabled.json"), fixedClock(time.Now()))
	s := &server{
		token:    "secret",
		disabled: disabled,
		collect: func() ([]Session, error) {
			return []Session{{PID: 55, SessionID: "new-session"}}, nil
		},
	}
	body := strings.NewReader(`{"session_id":"stale-session","disabled":true}`)
	req := httptest.NewRequest("POST", "/sessions/55/disable", body)
	req.SetPathValue("pid", "55")
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()

	s.disableSession(rec, req)

	var r actionResult
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if r.OK || r.Code != codeSessionMismatch {
		t.Fatalf("response = %#v, want session_mismatch refusal", r)
	}
	if disabled.Disabled("new-session") || disabled.Disabled("stale-session") {
		t.Fatal("store mutated on a refused request")
	}
}

func TestDisableHandlerNotLiveRefusesWithoutMutating(t *testing.T) {
	dir := t.TempDir()
	disabled := newDisabledStore(filepath.Join(dir, "disabled.json"), fixedClock(time.Now()))
	s := &server{
		token:    "secret",
		disabled: disabled,
		collect:  func() ([]Session, error) { return nil, nil },
	}
	body := strings.NewReader(`{"session_id":"ghost","disabled":true}`)
	req := httptest.NewRequest("POST", "/sessions/55/disable", body)
	req.SetPathValue("pid", "55")
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()

	s.disableSession(rec, req)

	var r actionResult
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if r.OK || r.Code != codeNotLive {
		t.Fatalf("response = %#v, want not_live refusal", r)
	}
}

func TestDisableHandlerRejectsMalformedBody(t *testing.T) {
	s := &server{token: "secret", collect: func() ([]Session, error) { return nil, nil }}
	cases := []struct {
		name string
		body string
	}{
		{"missing session_id", `{"disabled":true}`},
		{"missing disabled", `{"session_id":"x"}`},
		{"null session_id", `{"session_id":null,"disabled":true}`},
		{"empty session_id", `{"session_id":"","disabled":true}`},
		{"unknown field", `{"session_id":"x","disabled":true,"extra":1}`},
		{"trailing content", `{"session_id":"x","disabled":true}{}`},
		{"disabled not a bool", `{"session_id":"x","disabled":"yes"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/sessions/55/disable", strings.NewReader(tc.body))
			req.SetPathValue("pid", "55")
			req.Header.Set("Authorization", "Bearer secret")
			rec := httptest.NewRecorder()

			s.disableSession(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
			}
		})
	}
}

func TestDisableHandlerUnauthorized(t *testing.T) {
	s := &server{token: "secret"}
	req := httptest.NewRequest("POST", "/sessions/55/disable", strings.NewReader(`{"session_id":"x","disabled":true}`))
	req.SetPathValue("pid", "55")
	rec := httptest.NewRecorder()

	s.disableSession(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./... -run TestDisableHandler -v`
Expected: FAIL — `s.disableSession` undefined, `actionResult.SessionID`/`.Disabled` undefined (compile errors)

- [ ] **Step 3: Implement**

Extend `actionResult` (server.go:42-56), adding two fields after `Worktree`:

```go
type actionResult struct {
	OK    bool   `json:"ok"`
	Tmux  string `json:"tmux,omitempty"`  // tmux session name for migrate/new
	Error string `json:"error,omitempty"` // human-readable failure reason
	Code  string `json:"code,omitempty"`
	Worktree *worktreeInfo `json:"worktree,omitempty"`
	// SessionID/Disabled are set by disableSession's success response, the
	// same way migrate sets Tmux: the server's own resolved identity and the
	// state it actually applied, so the client never has to trust its own
	// guess of what "disabled" now means. *bool (not bool) so an explicit
	// false is distinguishable from "field absent" under omitempty.
	SessionID string `json:"session_id,omitempty"`
	Disabled  *bool  `json:"disabled,omitempty"`
}
```

Add the strict body decoder and handler, near `sessionIDPrecondition` (server.go, after it):

```go
// decodeDisableRequest strictly decodes the required {"session_id": "...",
// "disabled": true} body for POST /sessions/{pid}/disable. Unlike
// sessionIDPrecondition (kill/migrate's optional precondition), both fields
// are mandatory here — a disable write is meaningless without an explicit
// target and an explicit desired state — so absence, an explicit null, an
// unknown field, or trailing content are all rejected rather than treated as
// a no-op.
func decodeDisableRequest(w http.ResponseWriter, r *http.Request) (sessionID string, disabled bool, err error) {
	var body *struct {
		SessionID json.RawMessage `json:"session_id"`
		Disabled  json.RawMessage `json:"disabled"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		return "", false, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return "", false, fmt.Errorf("unexpected trailing json")
	}
	if body == nil {
		return "", false, fmt.Errorf("body must be an object")
	}
	if len(body.SessionID) == 0 || string(body.SessionID) == "null" {
		return "", false, fmt.Errorf("session_id is required")
	}
	if len(body.Disabled) == 0 || string(body.Disabled) == "null" {
		return "", false, fmt.Errorf("disabled is required")
	}
	if err := json.Unmarshal(body.SessionID, &sessionID); err != nil {
		return "", false, fmt.Errorf("session_id must be a string")
	}
	if sessionID == "" {
		return "", false, fmt.Errorf("session_id must not be empty")
	}
	if err := json.Unmarshal(body.Disabled, &disabled); err != nil {
		return "", false, fmt.Errorf("disabled must be a boolean")
	}
	return sessionID, disabled, nil
}

// disableSession handles POST /sessions/{pid}/disable: marks a live session
// disabled or enabled on this host, persisted in s.disabled. session_id and
// disabled are both required — see decodeDisableRequest. Identity resolution
// follows kill/migrate's resolveLivePID convention: the request can only
// narrow the target (via session_id), never widen it.
func (s *server) disableSession(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	pid, err := strconv.Atoi(r.PathValue("pid"))
	if err != nil {
		http.Error(w, "bad pid", http.StatusBadRequest)
		return
	}
	wantSession, disabled, err := decodeDisableRequest(w, r)
	if err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	target, _, refusal := s.resolveLivePID(pid, wantSession)
	if refusal != nil {
		writeJSON(w, http.StatusOK, *refusal)
		return
	}
	if s.disabled != nil {
		s.disabled.SetDisabled(target.SessionID, disabled)
	}
	d := disabled
	writeJSON(w, http.StatusOK, actionResult{OK: true, SessionID: target.SessionID, Disabled: &d})
}
```

Register the route in `cmdServer`'s mux (server.go, alongside the other `mux.HandleFunc` calls):

```go
	mux.HandleFunc("POST /sessions/{pid}/disable", s.disableSession)
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./... -run TestDisableHandler -v && go build ./... && go vet ./...`
Expected: PASS, clean build/vet

- [ ] **Step 5: Commit**

```bash
git add server.go server_test.go
git commit -m "$(cat <<'EOF'
feat: add POST /sessions/{pid}/disable endpoint

Both session_id and disabled are required (unlike kill/migrate's
optional precondition) — a disable write is meaningless without an
explicit target and desired state. Follows the same
resolveLivePID/refusal-envelope convention as kill and migrate.
EOF
)"
```

---

### Task 4: Remove `Disabled` from `SessionStore`

**Files:**
- Modify: `state.go` (whole file — Disabled leaves entirely, Group stays)
- Modify: `state_test.go`

**Interfaces:**
- Consumes: nothing new.
- Produces: `SessionStore` now Group-only. `SetDisabled`/`Disabled`/`OverlayDisabled` methods and the `Disabled` field on `sessionState` are gone — Task 5/6/7 must not reference them.

- [ ] **Step 1: Update the tests first**

Rewrite `state_test.go` in full:

```go
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fixedClock returns a now func pinned to t.
func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

func TestSessionStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)

	s := loadSessionStore(path, fixedClock(now))
	s.SetGroup("alpha", 3, []string{"alpha"})

	// Reload from disk: assignment survives.
	got := loadSessionStore(path, fixedClock(now))
	if got.Group("alpha") != 3 {
		t.Fatalf("alpha group = %d, want 3", got.Group("alpha"))
	}
	if got.Group("beta") != 0 {
		t.Fatalf("cross-contamination: %#v", got.entries)
	}
	if m := got.GroupsMap(); len(m) != 1 || m["alpha"] != 3 {
		t.Fatalf("GroupsMap = %#v, want {alpha:3}", m)
	}
}

func TestSessionStoreAtomicSaveLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)

	s := loadSessionStore(path, fixedClock(now))
	s.SetGroup("alpha", 1, []string{"alpha"})

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "state.json" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("dir contents = %v, want only state.json", names)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cs clientState
	if err := json.Unmarshal(data, &cs); err != nil {
		t.Fatalf("state.json not valid JSON: %v", err)
	}
	e, ok := cs.Sessions["alpha"]
	if !ok || e.Group != 1 || e.LastSeen != now.Format(time.RFC3339) {
		t.Fatalf("persisted alpha = %#v", cs.Sessions)
	}
}

func TestSessionStoreGCOnLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	old := now.Add(-40 * 24 * time.Hour).Format(time.RFC3339)
	recent := now.Add(-1 * time.Hour).Format(time.RFC3339)

	seed := clientState{Sessions: map[string]sessionState{
		"empty": {LastSeen: recent},        // no group: drop
		"stale": {Group: 2, LastSeen: old},  // grouped but expired: drop
		"kept":  {Group: 5, LastSeen: recent}, // grouped + fresh: keep
	}}
	data, _ := json.MarshalIndent(seed, "", "  ")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	s := loadSessionStore(path, fixedClock(now))
	want := map[string]bool{"kept": true}
	if len(s.entries) != len(want) {
		t.Fatalf("post-GC entries = %#v, want %v", s.entries, want)
	}
	for id := range want {
		if _, ok := s.entries[id]; !ok {
			t.Fatalf("GC dropped %q which should survive: %#v", id, s.entries)
		}
	}
}

func TestSessionStoreGroupToggleAndReplace(t *testing.T) {
	s := loadSessionStore("", fixedClock(time.Now()))

	// Assign, then same group again ungroups.
	s.SetGroup("x", 3, nil)
	if s.Group("x") != 3 {
		t.Fatalf("after assign group = %d, want 3", s.Group("x"))
	}
	s.SetGroup("x", 3, nil)
	if s.Group("x") != 0 {
		t.Fatalf("re-assigning same group did not ungroup: %d", s.Group("x"))
	}
	if _, ok := s.entries["x"]; ok {
		t.Fatalf("ungrouped entry not dropped: %#v", s.entries)
	}

	// Single membership: a new group replaces the old.
	s.SetGroup("y", 2, nil)
	s.SetGroup("y", 7, nil)
	if s.Group("y") != 7 {
		t.Fatalf("replace group = %d, want 7", s.Group("y"))
	}
}

func TestSessionStoreLastSeenRefreshesOnlyExistingVisible(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	t0 := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	clock := t0

	s := loadSessionStore(path, func() time.Time { return clock })
	s.SetGroup("grouped", 1, []string{"grouped", "ungrouped"})

	// Advance time and save again with both sessions visible.
	clock = t0.Add(48 * time.Hour)
	s.SetGroup("grouped", 1, []string{"grouped", "ungrouped"})
	s.SetGroup("grouped", 1, []string{"grouped", "ungrouped"}) // re-assign same group: no-op toggle, still saves

	if e := s.entries["grouped"]; e.LastSeen != clock.Format(time.RFC3339) {
		t.Fatalf("grouped last_seen = %q, want %q", e.LastSeen, clock.Format(time.RFC3339))
	}
	if _, ok := s.entries["ungrouped"]; ok {
		t.Fatalf("ungrouped visible session got a junk entry: %#v", s.entries)
	}
}

func TestSessionStoreIgnoresEmptySessionID(t *testing.T) {
	s := loadSessionStore("", fixedClock(time.Now()))
	s.SetGroup("", 4, nil)
	if len(s.entries) != 0 {
		t.Fatalf("empty sessionID created entries: %#v", s.entries)
	}
}

func TestLoadSessionStoreCorruptOrMissing(t *testing.T) {
	missing := loadSessionStore(filepath.Join(t.TempDir(), "nope.json"), fixedClock(time.Now()))
	if len(missing.entries) != 0 {
		t.Fatalf("missing file yielded %#v", missing.entries)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	corrupt := loadSessionStore(path, fixedClock(time.Now()))
	if len(corrupt.entries) != 0 {
		t.Fatalf("corrupt file yielded %#v", corrupt.entries)
	}
}

func TestSessionStoreEmptyPathDisablesPersistence(t *testing.T) {
	s := loadSessionStore("", fixedClock(time.Now()))
	s.SetGroup("x", 1, []string{"x"})
	if s.Group("x") != 1 {
		t.Fatalf("in-memory group lost: %d", s.Group("x"))
	}
}
```

(Removed: `TestSessionStoreOverlayDisabled`, all `SetDisabled`/`Disabled`/`Disabled:` references. `TestSessionStoreLastSeenRefreshesOnlyExistingVisible` now uses a second `SetGroup` call — same group, still a mutation that re-saves — instead of `SetDisabled` to advance `last_seen`.)

- [ ] **Step 2: Run test to verify it fails to compile**

Run: `go build ./... 2>&1 | head -30`
Expected: FAIL — `state.go` still defines `SetDisabled`/`Disabled`/`OverlayDisabled`/the `Disabled` field, but nothing calls them from the new test file, which is fine; the *next* step's edits to `state.go` are what could break other callers. Actually run the full test suite instead to confirm nothing currently depends on the removed methods from `state_test.go` itself:

Run: `go test ./... -run TestSessionStore -v`
Expected: PASS already (this step only rewrote the test file; state.go is unchanged so far) — confirms the rewritten tests are valid before touching state.go.

- [ ] **Step 3: Remove `Disabled` from `state.go`**

Apply these edits to `state.go`:

1. `sessionState` struct — remove the `Disabled` field:
```go
type sessionState struct {
	Group    int    `json:"group,omitempty"`
	LastSeen string `json:"last_seen,omitempty"` // RFC3339, UTC
}
```

2. Doc comment above `sessionState` — drop the `Disabled` mention:
```go
// sessionState is the persisted client-side state for one Claude session,
// keyed by its stable SessionID. Zero-valued fields are omitted on write.
```

3. `SessionStore` doc comment (state.go:26-31) — narrow to Group:
```go
// SessionStore is the client-machine-local persisted group assignment at
// ~/.config/claude-sessions/state.json (alongside servers.yaml). It is only
// touched from the TUI's single-threaded event loop — and read-only by the
// `list` subcommand — so it carries no locking. Mutations save atomically (temp
// file + rename in the same directory). Two concurrent TUIs race last-writer-
// wins, which is accepted.
```

4. `gc` — drop the `Disabled` half of the "carries no state" check:
```go
func (s *SessionStore) gc(now time.Time) {
	cutoff := now.Add(-stateMaxAge)
	for id, e := range s.entries {
		if e.Group == 0 {
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
```
Update the comment above `gc` too (drop "and entries" wording change is minimal — just remove the word "disabled" if present; re-read the current comment and adjust only the phrase "neither group nor disabled" to "no group").

5. Remove entirely: the `Disabled(sessionID string) bool` method, the `OverlayDisabled(sessions []Session)` method, and the `SetDisabled(sessionID string, disabled bool, visibleIDs []string)` method.

6. `set` — drop the `Disabled` half of its zero-value check:
```go
func (s *SessionStore) set(sessionID string, e sessionState) {
	if e.Group == 0 {
		delete(s.entries, sessionID)
		return
	}
	e.LastSeen = s.nowStamp()
	s.entries[sessionID] = e
}
```

7. `TouchVisible` doc comment — change "a session that stays grouped/disabled but untouched" to "a session that stays grouped but untouched":
```go
// TouchVisible refreshes last_seen for every visible session that already has
// an entry and saves if anything changed. Mutations already do this on save;
// this exists for plain viewing, so a session that stays grouped but
// untouched for 30 days isn't dropped by load-time GC while it's still being
// looked at. The caller throttles it (see settleRows); every visible entry
// gets the same stamp, so calling it more often than the GC horizon matters
// only as file-write churn.
```

8. `persist` doc comment — change "never creating one for an ungrouped/enabled session" to "never creating one for an ungrouped session":
```go
// persist refreshes last_seen for every visible session that already has an
// entry (never creating one for an ungrouped session), then writes the
// file atomically.
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./... -run TestSessionStore -v && go build ./... && go vet ./...`
Expected: PASS, clean build/vet. Note: `go build ./...` will still fail here if `actions.go`/`tui.go`/`main.go`/`commands.go` still call the now-removed `SetDisabled`/`Disabled`/`OverlayDisabled` — that's expected and fixed in Tasks 5/7/8. If this task is done standalone, temporarily confirm just `state.go`/`state_test.go` compile in isolation is not possible in Go (whole-package build) — proceed directly to Task 5 before attempting a full build; the two are meant to land together in practice even though they're separate commits for review granularity. Run `go vet ./...` only after Task 8 lands.

- [ ] **Step 5: Commit**

```bash
git add state.go state_test.go
git commit -m "$(cat <<'EOF'
refactor: remove Disabled from SessionStore

Disabled moves to the new host-owned DisabledStore (disabled_store.go).
SessionStore now holds only Group -- the one thing that's genuinely
client/viewer-side, since a view can group sessions from multiple
hosts. Callers are updated in the following commits.
EOF
)"
```

---

### Task 5: `actCtx` gets a `DisabledStore`; `actToggleDisabled` branches local/remote

**Files:**
- Modify: `actions.go:46-51` (`actCtx` struct doc + field), `actions.go:133-149` (`actToggleDisabled`)
- Modify: `actions_test.go`

**Interfaces:**
- Consumes: `DisabledStore.SetDisabled` (Task 1).
- Produces: `actCtx.disabled *DisabledStore` field (Task 7 populates it in `makeCtx`). `actToggleDisabled(c *actCtx) bool` — unchanged signature/contract (true = something changed), now branches to `actToggleDisabledRemote` (Task 6) for `Host != ""` rows.

- [ ] **Step 1: Update the tests first**

Replace `TestActToggleDisabledTogglesStore` and `TestActToggleDisabledIgnoresEmptyHostAndMissingID` in `actions_test.go`:

```go
func TestActToggleDisabledTogglesStore(t *testing.T) {
	cases := []struct {
		name    string
		session Session
		want    bool // disabled value expected after the toggle
	}{
		{"local enable to disabled", Session{PID: 10, SessionID: "local"}, true},
		{"local disabled to enabled", Session{PID: 11, SessionID: "local-off", Disabled: true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			disabled := newDisabledStore("", nil)
			if tc.session.Disabled {
				disabled.SetDisabled(tc.session.SessionID, true)
			}
			target := sessionSelectionTarget(tc.session)
			c := &actCtx{targets: []selectionTarget{target}, sel: target.id, disabled: disabled}
			if !actToggleDisabled(c) {
				t.Fatal("actToggleDisabled = false, want true")
			}
			// The store is the sole authority — the live rows pick the value up
			// via the caller's settleRows()/refresh() re-overlay, not an
			// in-place patch.
			if got := disabled.Disabled(tc.session.SessionID); got != tc.want {
				t.Fatalf("store.Disabled = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestActToggleDisabledIgnoresEmptyHostAndMissingID(t *testing.T) {
	disabled := newDisabledStore("", nil)

	empty := emptyHostSelectionTarget("orca")
	c := &actCtx{targets: []selectionTarget{empty}, sel: empty.id, disabled: disabled}
	if actToggleDisabled(c) {
		t.Fatal("empty-host target toggled")
	}

	missingID := sessionSelectionTarget(Session{PID: 2})
	c = &actCtx{targets: []selectionTarget{missingID}, sel: missingID.id, disabled: disabled}
	if actToggleDisabled(c) {
		t.Fatal("missing-SessionID target toggled")
	}
	if len(disabled.entries) != 0 {
		t.Fatalf("store mutated on ignored toggles: %#v", disabled.entries)
	}
}
```

(The old "remote enable to disabled" case is removed: a remote row no longer writes the local store at all — it goes over HTTP, exercised separately by Task 6's `postDisableRemote` test against an `httptest` server, the same way `actKillRemote`'s network call isn't unit-tested at the TUI-dispatch layer either — see remote_actions_test.go's existing pattern.)

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./... -run TestActToggleDisabled -v`
Expected: FAIL — `actCtx.disabled` field undefined (compile error)

- [ ] **Step 3: Implement**

In `actions.go`, replace the `store`/`visibleSessionIDs` doc comment and add the new field (actions.go:46-51):

```go
	// store is the client-side group state; group assignment stays
	// per-viewer (a view can group sessions from multiple hosts). Disabled
	// state moved to the host-owned DisabledStore below.
	store             *SessionStore
	visibleSessionIDs []string
	// disabled is this host's DisabledStore, written directly for local rows
	// (Host == ""). Remote rows never write it — they go through
	// actToggleDisabledRemote instead. May be nil in tests that don't
	// exercise it.
	disabled *DisabledStore
```

Replace `actToggleDisabled` (actions.go:133-149):

```go
// actToggleDisabled flips the disabled flag for the selected session. Local
// rows (Host == "") write directly to this host's DisabledStore — an
// instant local write with no HTTP, mirroring the local branch every other
// action (actKill, actAttach) takes. Remote rows delegate to
// actToggleDisabledRemote, which makes the same write over HTTP against the
// row's own host. Either way the store write is authoritative: nothing
// in-memory is patched here (c.selected() points into a throwaway targets
// copy), so the caller MUST settleRows()/refresh() afterwards to re-overlay
// the new value onto the live rows. A row with no selection or no stable
// SessionID is ignored (it can't be keyed). Reports whether anything changed.
func actToggleDisabled(c *actCtx) bool {
	session := c.selected()
	if session == nil || session.SessionID == "" {
		return false
	}
	if session.Host != "" {
		return actToggleDisabledRemote(c)
	}
	if c.disabled != nil {
		c.disabled.SetDisabled(session.SessionID, !session.Disabled)
	}
	return true
}
```

This introduces a forward reference to `actToggleDisabledRemote`, defined in Task 6 (`remote_actions.go`, same package) — the build won't succeed until that task lands. Do Task 6 immediately after this one before attempting `go build ./...`.

- [ ] **Step 4: Run tests to verify they pass**

This task alone won't compile (forward reference to `actToggleDisabledRemote`). Proceed directly to Task 6, then run:

Run: `go test ./... -run TestActToggleDisabled -v`
Expected: PASS (verified at the end of Task 6)

- [ ] **Step 5: Commit**

Commit together with Task 6 (see Task 6's Step 5) — splitting them would leave a non-compiling intermediate commit.

---

### Task 6: `actToggleDisabledRemote` + `postDisableRemote`

**Files:**
- Modify: `remote_actions.go` (new functions, alongside `actKillRemote`/`removeRemoteWorktree`)
- Modify: `remote_actions_test.go`

**Interfaces:**
- Consumes: `remoteRequest` (remote_actions.go:124), `actionResult` (Task 3), `showActionError` (actions.go:151-156), `writeServerYAML` test helper (remote_test.go).
- Produces: `postDisableRemote(host string, pid int, sessionID string, disabled bool) (actionResult, error)` — plain, unit-testable core, same shape as `removeRemoteWorktree`. `actToggleDisabledRemote(c *actCtx) bool` — completes Task 5's forward reference.

- [ ] **Step 1: Write the failing test**

Add to `remote_actions_test.go`:

```go
func TestPostDisableRemoteSendsResolvedIdentity(t *testing.T) {
	var gotBody []byte
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"session_id":"sess-1","disabled":true}`))
	}))
	defer backend.Close()

	u, _ := url.Parse(backend.URL)
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeServerYAML(t, home, "box", u.Hostname(), u.Port(), "secret")

	r, err := postDisableRemote("box", 42, "sess-1", true)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !r.OK || r.SessionID != "sess-1" || r.Disabled == nil || !*r.Disabled {
		t.Fatalf("response = %#v", r)
	}
	var sent struct {
		SessionID string `json:"session_id"`
		Disabled  bool   `json:"disabled"`
	}
	if err := json.Unmarshal(gotBody, &sent); err != nil {
		t.Fatalf("decode sent body: %v", err)
	}
	if sent.SessionID != "sess-1" || !sent.Disabled {
		t.Fatalf("sent body = %#v, want session_id=sess-1 disabled=true", sent)
	}
}

func TestPostDisableRemoteSurfacesRefusal(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":false,"error":"PID 42 is not a live Claude session","code":"not_live"}`))
	}))
	defer backend.Close()

	u, _ := url.Parse(backend.URL)
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeServerYAML(t, home, "box", u.Hostname(), u.Port(), "secret")

	r, err := postDisableRemote("box", 42, "sess-1", true)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if r.OK || r.Code != codeNotLive {
		t.Fatalf("response = %#v, want not_live refusal", r)
	}
}
```

Add `"encoding/json"` and `"io"` to `remote_actions_test.go`'s import block (both are new to this file — `net/http`, `net/http/httptest`, `net/url`, `reflect`, `testing` are already there).

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./... -run TestPostDisableRemote -v`
Expected: FAIL — `postDisableRemote` undefined (compile error)

- [ ] **Step 3: Implement**

Add to `remote_actions.go`, near `removeRemoteWorktree`:

```go
// postDisableRemote asks host's server to set pid's disabled state, and
// returns the state it actually applied — the server's own resolved
// SessionID and the value it wrote, never the caller's guess of what
// "disabled" now means. Mirrors removeRemoteWorktree's plain-function shape
// so the network+parse logic is unit-testable without a terminal.
func postDisableRemote(host string, pid int, sessionID string, disabled bool) (actionResult, error) {
	body, err := json.Marshal(map[string]any{"session_id": sessionID, "disabled": disabled})
	if err != nil {
		return actionResult{}, err
	}
	resp, err := remoteRequest(host, fmt.Sprintf("/sessions/%d/disable", pid), "POST", body)
	if err != nil {
		return actionResult{}, err
	}
	var r actionResult
	if err := json.Unmarshal(resp, &r); err != nil {
		return actionResult{}, err
	}
	return r, nil
}

// actToggleDisabledRemote handles "-"/"+" on a remote-selected row. No
// confirmation dialog — unlike kill, disabling isn't destructive. Reports
// whether anything changed, matching actToggleDisabled's local-path contract.
func actToggleDisabledRemote(c *actCtx) bool {
	s := c.selected()
	if s == nil || s.SessionID == "" {
		return false
	}
	r, err := postDisableRemote(s.Host, s.PID, s.SessionID, !s.Disabled)
	if err != nil {
		showActionError(c, "disable", err)
		return false
	}
	if !r.OK {
		showActionError(c, "disable", fmt.Errorf("%s", r.Error))
		return false
	}
	return true
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./... -run 'TestPostDisableRemote|TestActToggleDisabled' -v && go build ./... && go vet ./...`
Expected: PASS. `go build ./...` may still fail if Tasks 7/8 (tui.go, main.go, commands.go) still reference the removed `SessionStore.OverlayDisabled`/`Disabled`/`SetDisabled` — if so, that's expected at this point; proceed to Task 7.

- [ ] **Step 5: Commit** (covers Task 5 + Task 6 together, since Task 5 alone doesn't compile)

```bash
git add actions.go actions_test.go remote_actions.go remote_actions_test.go
git commit -m "$(cat <<'EOF'
feat: route disable/enable through DisabledStore, local and remote

actToggleDisabled branches on Host the same way actKill does: local
rows write this host's DisabledStore directly, remote rows POST to
the row's own host via the new actToggleDisabledRemote /
postDisableRemote, which decode the server's authoritative resolved
identity and applied state rather than trusting the client's guess.
EOF
)"
```

---

### Task 7: Wire `DisabledStore` into the TUI

**Files:**
- Modify: `tui.go:106` (store construction), `tui.go:198-206` (`settleRows` overlay), `tui.go:337-359` (`makeCtx`), `tui.go:560-567` (dispatch case)

**Interfaces:**
- Consumes: `LoadDisabledStore()`, `(*DisabledStore).Overlay` (Task 1), `actToggleDisabled` (Task 5), `refresh` (tui.go:233, pre-existing closure).
- Produces: nothing new consumed by later tasks — this is the last TUI-facing wiring point.

- [ ] **Step 1: No new automated test** — this task is TUI event-loop wiring with no unit-test seam (the existing TUI has no test harness for `RunTUI` itself; `settleRows`'s overlay behavior is already covered indirectly by Task 1's `DisabledStore` tests and Task 5's `actToggleDisabled` tests). Verify manually per Step 4.

- [ ] **Step 2: N/A** (no failing test to run first for this task)

- [ ] **Step 3: Implement**

At tui.go:106, alongside `store := LoadSessionStore()`:

```go
	store := LoadSessionStore()
	disabledStore := LoadDisabledStore()
```

In `settleRows` (tui.go:198-206), replace:

```go
		// Refresh the group snapshot and overlay the store's disabled flags onto
		// both local and remote sessions before sorting — the overlay is
		// authoritative and overwrites any (now always-false) server-reported
		// value, and the sort orders disabled rows last.
		groups = store.GroupsMap()
		store.OverlayDisabled(local)
		SortSessions(local, sortMode)
		// Snapshot() returns the hub's shared slices; sort remotes on copies so
		// we never race the hub goroutine that owns them.
		snap := hub.Snapshot()
		for i := range snap {
			store.OverlayDisabled(snap[i].Sessions)
		}
		remotes = sortRemotes(snap, sortMode)
```

with:

```go
		// Refresh the group snapshot and overlay this host's disabled flags
		// onto local sessions before sorting — the sort orders disabled rows
		// last. Remote sessions already carry authoritative Disabled from the
		// wire (each remote host's own DisabledStore, applied server-side in
		// GET /sessions), so no client-side overlay is needed for them.
		groups = store.GroupsMap()
		disabledStore.Overlay(local)
		SortSessions(local, sortMode)
		// Snapshot() returns the hub's shared slices; sort remotes on copies so
		// we never race the hub goroutine that owns them.
		snap := hub.Snapshot()
		remotes = sortRemotes(snap, sortMode)
```

In `makeCtx` (tui.go:337-359), add the new field:

```go
	makeCtx := func() *actCtx {
		return &actCtx{
			fd:                fd,
			oldState:          oldState,
			targets:           targets,
			sel:               state.sel,
			modalWakes:        modalWakes,
			store:             store,
			disabled:          disabledStore,
			visibleSessionIDs: visibleSessionIDs(local, remotes),
```

(rest of the literal unchanged)

In the dispatch switch (tui.go:560-567), replace:

```go
			case "-", "+":
				if actToggleDisabled(makeCtx()) {
					// The store write is authoritative; settleRows re-overlays it
					// onto local + remotes, so no per-host patch is needed.
					settleRows()
					state.requestSelectionAnchor()
					render()
				}
```

with:

```go
			case "-", "+":
				screen.Invalidate()
				if actToggleDisabled(makeCtx()) {
					// The store write is authoritative; refresh(true) re-overlays
					// it onto local (via settleRows) and kicks an immediate remote
					// refetch so a remote toggle shows up promptly — the same
					// convention actKill/actAttach already follow, including the
					// same brief eventual-consistency window if a poll was already
					// in flight when the write landed.
					refresh(true)
					state.requestSelectionAnchor()
					render()
				}
```

- [ ] **Step 4: Manual verification**

Run: `go build ./... && go vet ./...`
Expected: clean build/vet (this is the point where the whole package should compile again, assuming Task 8 lands alongside — see note below)

Then smoke-test interactively:
```sh
go run . 
```
- Press `-`/`+` on a local session row: it should flip disabled/enabled and re-sort immediately.
- Restart the TUI (`Ctrl-C`, `go run .` again): the disabled row should still show disabled — proves the persistence half of the design goal.
- If a remote server is configured (`servers.yaml`), toggle a remote row's disabled state from two different terminals (both running the TUI) and confirm both eventually show the same state — proves the cross-client half.

- [ ] **Step 5: Commit** (covers Task 7 + Task 8 together — `tui.go` alone still references nothing broken, but `main.go`/`commands.go` still call the removed `SessionStore.OverlayDisabled`, so the package won't build until Task 8 lands too)

Commit after Task 8's Step 3 (see Task 8's Step 5).

---

### Task 8: Update `main.go` / `commands.go` overlay call sites

**Files:**
- Modify: `main.go:104-116` (`cmdList`)
- Modify: `commands.go:464-476` (`cmdListSessions`)

**Interfaces:**
- Consumes: `LoadDisabledStore()`, `(*DisabledStore).Overlay` (Task 1).
- Produces: nothing further consumed — this is the last call site.

- [ ] **Step 1: No new automated test** — both functions are one-shot CLI output paths with no existing unit test harness (confirmed: neither `cmdList` nor `cmdListSessions` has a corresponding `_test.go` entry today). Verified manually in Step 4, consistent with how this code was tested before this change.

- [ ] **Step 2: N/A**

- [ ] **Step 3: Implement**

In `main.go`'s `cmdList` (main.go:104-116), replace:

```go
func cmdList() error {
	local, err := CollectLocal()
	if err != nil {
		return err
	}
	remotes := FetchAllRemote()
	// Disabled state is client-side; overlay it read-only so the scriptable
	// list matches the TUI. Groups don't affect this output.
	store := LoadSessionStore()
	store.OverlayDisabled(local)
	for i := range remotes {
		store.OverlayDisabled(remotes[i].Sessions)
	}
	sortMode := LoadSortMode()
```

with:

```go
func cmdList() error {
	local, err := CollectLocal()
	if err != nil {
		return err
	}
	remotes := FetchAllRemote()
	// Disabled state is host-owned (disabled_store.go). Local sessions are
	// this host, so overlay directly; remote sessions already carry
	// authoritative Disabled from the wire (each remote host's own
	// DisabledStore, applied server-side in GET /sessions). Groups don't
	// affect this output.
	LoadDisabledStore().Overlay(local)
	sortMode := LoadSortMode()
```

In `commands.go`'s `cmdListSessions` (commands.go:464-477), apply the identical replacement (same comment, same single line, same removed `store`/loop).

- [ ] **Step 4: Manual verification**

Run: `go build ./... && go vet ./... && go test ./... && go test -race ./...`
Expected: clean build, vet, and full test suite passes (this is the first point since Task 4 where the whole package is expected to compile and all tests pass together)

Then:
```sh
go run . -- list          # or however cmdList is invoked per commands.go's flag parsing
go run . list-sessions
go run . list-sessions --json
```
Confirm a locally-disabled session (toggled via the TUI in Task 7's manual check) shows up disabled in both outputs.

- [ ] **Step 5: Commit** (covers Tasks 7 + 8 — the first commit since Task 4 where the full package builds and all tests pass)

```bash
git add tui.go main.go commands.go
git commit -m "$(cat <<'EOF'
feat: wire DisabledStore through TUI and scriptable list output

Local sessions overlay directly from this host's DisabledStore in the
TUI, `list`, and `list-sessions`. Remote sessions no longer get a
client-side overlay at all -- GET /sessions already carries
authoritative Disabled state from each remote host's own store. The
"-"/"+ " dispatch now calls refresh(true) instead of settleRows(),
matching how kill/attach already make remote changes visible promptly.
EOF
)"
```

---

### Task 9: Full verification pass

**Files:** none (verification only)

- [ ] **Step 1: Full build/vet/test sweep**

```sh
go build ./...
go vet ./...
go test ./...
go test -race ./...
gofmt -l .
```

Expected: all clean, `gofmt -l .` prints nothing (no unformatted files).

- [ ] **Step 2: Manual end-to-end check**

Repeat the TUI/CLI smoke tests from Tasks 7 and 8's Step 4 in one pass:
1. Start the TUI locally, disable a local session, quit, restart the TUI — still disabled.
2. If a remote host is configured, disable one of its sessions from this client, then poll `GET /sessions` on that host directly (`curl -H "Authorization: Bearer <token>" http://<host>:<port>/sessions`) — confirm `"disabled":true` appears in the JSON.
3. Restart the remote `-s` daemon (or simulate by re-running `claude-sessions -s` after killing it) — confirm the disabled flag survives (reads `disabled.json` on next `GET /sessions`).
4. From a second client machine (or a second `servers.yaml` entry pointed at the same host), confirm it also sees the session disabled — proves cross-client visibility.

- [ ] **Step 3: Update CLAUDE.md if warranted**

If any invariant here turns out subtle enough to bite a future change (e.g. the flock+no-rename trade-off, or the Overlay-does-opportunistic-writes design), add a short note to CLAUDE.md's file-by-file section once the code is settled — judgment call at implementation time, not a mandatory step.

- [ ] **Step 4: No commit** — this task only verifies; Task 8's commit is the last code change.

---

## Self-Review Notes

- **Spec coverage:** every section of the design doc maps to a task — `DisabledStore` (Task 1), `GET /sessions` overlay (Task 2), `POST /sessions/{pid}/disable` (Task 3), `SessionStore` narrowing (Task 4), client dispatch branching (Tasks 5–6), TUI wiring (Task 7), scriptable output (Task 8).
- **Type consistency checked:** `DisabledStore.SetDisabled(sessionID string, disabled bool)` (Task 1) has no `visibleIDs` parameter, unlike the old `SessionStore.SetDisabled` — every call site in Tasks 5/6 matches the 2-arg form. `actionResult.Disabled` is `*bool` throughout (Task 3's handler, Task 6's client decode) — never a plain `bool`, so callers must nil-check before dereferencing (`Task 3`'s and `Task 6`'s test assertions do `r.Disabled == nil || !*r.Disabled`).
- **Task ordering dependency:** Tasks 4, 5, 6 leave the package non-compiling at intermediate points (Task 4 removes methods Tasks 5/7/8 haven't stopped calling yet; Task 5 forward-references Task 6). This is called out explicitly in each task's steps — an implementer working strictly task-by-task with a build gate between every task must do 4→5→6 as one uninterrupted sequence before running `go build ./...`, and likewise land 7+8 together. `subagent-driven-development`'s per-task review gate should treat {4,5,6} and {7,8} as two atomic units if it insists on a green build between every single task.
