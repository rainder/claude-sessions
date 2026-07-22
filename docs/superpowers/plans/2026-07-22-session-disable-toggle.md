# Session Disable Toggle Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add server-authoritative, in-memory disabled state for live sessions, toggled with `d`, rendered as an amber rail plus muted row, and always sorted below enabled sessions.

**Architecture:** Each host server owns disabled state for sessions running on that host, keyed by non-empty `SessionID`. `GET /sessions` carries state to local and remote clients; `PUT /sessions/{pid}/disabled` performs idempotent writes guarded by selected row's expected `sessionId`, preventing PID reuse from mutating a replacement session. Local TUI collection prefers loopback with a `750ms` timeout and falls back to direct collection. `RemoteHub` deep-copies snapshots and generation-fences an immediate patch against only the poll cycle already in flight, then accepts the first successful later cycle as authoritative so newer writes from other clients converge.

**Tech Stack:** Go, `net/http`, `encoding/json`, `sync`, ANSI terminal rendering, existing `golang.org/x/sys/unix` and `golang.org/x/term` dependencies only.

## Global Constraints

- Disabled state lives only in server memory and resets when server restarts.
- State is keyed by a non-empty `SessionID`, never PID; API paths continue to identify current sessions by PID. A live row without a stable ID returns `409 Conflict` and remains unchanged.
- Disabled sessions remain attachable, previewable, migratable, and killable.
- Enabled rows always precede disabled rows; selected sort applies independently inside both partitions.
- Local server address remains exactly `http://127.0.0.1:8765`; local listing timeout is exactly `750ms`; no custom local port or automatic daemon startup.
- Local server outage falls back to direct `CollectLocal`; direct rows appear enabled and `d` reports server failure.
- Combined local and remote serving uses `claude-sessions --server --bind 0.0.0.0`.
- Keep existing one-second `/sessions` cache, generation fencing, tmux viewer-count prefix, selected-row background highlighting, mouse row IDs, incremental `screenRenderer`, and single-stdin-consumer invariants.
- Add no dependencies.

---

## File Map and Interfaces

**Create**

- `server_client.go` — local-server resolution, bounded loopback collection, identity-guarded disabled-state PUT client, and authoritative response validation.
- `server_client_test.go` — loopback resolution, fallback, request, response, and error tests.

**Modify**

- `session.go` — add `Session.Disabled`; make disabled state the fixed primary sort partition.
- `session_test.go` — JSON compatibility and all-sort partition tests.
- `server.go` — in-memory disabled registry, annotation/pruning, PUT handler, route, and cache invalidation.
- `server_test.go` — set/clear, annotation, pruning, PID reuse, validation, auth, cache, and race tests.
- `remote_actions.go` — extract request helper that accepts a resolved `ServerConfig`.
- `remote.go` — deep-copy snapshots plus generation-fenced disabled overrides in `RemoteHub`.
- `remote_test.go` — disabled decode, snapshot isolation/race, stale-cycle, and superseding-write tests.
- `actions.go` — side-effect-free toggle action result and injectable transport seam.
- `actions_test.go` — local/remote/inverse/no-op/failure routing tests.
- `tui.go` — loopback-first local refresh, `d` dispatch, immediate patch/re-sort, pinned footer, and help copy.
- `tui_test.go` — footer/help copy tests.
- `tui_state.go` — explicit selection-anchor request after successful disabled-last movement.
- `tui_state_test.go` — selection identity and viewport anchoring after disabled-last reordering.
- `render.go` — fixed disabled rail column and shared row-decoration helpers across all layouts.
- `render_test.go` — amber rail, muted body, selected-row, viewer-prefix, width, and frame compatibility tests.

**Produced interfaces**

```go
// session.go: add beside other server-derived metadata.
Disabled bool `json:"disabled,omitempty"`

// actions.go
type disabledUpdate struct {
    Host      string
    SessionID string
    Disabled  bool
}

// server_client.go
type disabledState struct {
    SessionID string
    Disabled  bool
}

func fetchLocalServerSessions() ([]Session, error)
func collectClientLocal() ([]Session, error)
func setSessionDisabled(host string, pid int, sessionID string, disabled bool) (disabledState, error)
func patchDisabledBySessionID(rows []Session, sessionID string, disabled bool) bool

// actions.go
func actToggleDisabled(c *actCtx) (*disabledUpdate, error)

// remote.go
func (h *RemoteHub) PatchDisabled(host, sessionID string, disabled bool)
```

---

### Preflight: Sync Worktree With Current Main

Current feature worktree is based at `eb20f73`; `main` was `f824858` at final plan review and contains `/sessions` caching, tmux viewer counts, selected-row background highlighting, incremental screen rendering, modal wake handling, action-output positioning, footer/toast helpers, and aligned host-heading resource columns. Implement against current `main`, not stale render/server seams.

- [ ] **Step 1: Confirm only approved docs are untracked**

Run:

```bash
git status --short --branch
```

Expected: branch `worktree-feat-session-disable-toggle`; only approved spec and this plan are untracked.

- [ ] **Step 2: Fast-forward feature branch to current main**

Run:

```bash
git merge --ff-only main
```

Expected: fast-forward to current `main`; no conflict.

- [ ] **Step 3: Verify clean baseline behavior**

Run:

```bash
go test ./...
go vet ./...
go build .
```

Expected: all commands exit 0 before feature edits.

- [ ] **Step 4: Commit approved design and plan**

```bash
git add docs/superpowers/specs/2026-07-22-session-disable-toggle-design.md \
        docs/superpowers/plans/2026-07-22-session-disable-toggle.md
git commit -m "docs: plan session disable toggle"
```

Expected: one docs-only commit.

---

### Task 1: Session Field, JSON Compatibility, Sorting, and Selection

**Files:**
- Modify: `session.go:17-50, 211-261` after preflight merge
- Test: `session_test.go:92-176, 275+`
- Test: `tui_state_test.go:243+`

**Interfaces:**
- Consumes: existing `Session.ID()`, `sessionStatusRank`, `SortSessions`, `buildSelectionTargets`, `tuiState.settleSelection`.
- Produces: `Session.Disabled bool`; session-file decode that always clears persisted disabled data; enabled-first, disabled-last comparator used by every sort mode.

- [ ] **Step 1: Write failing JSON and sorting tests**

Append to `session_test.go`:

```go
func TestSessionDisabledJSONCompatibility(t *testing.T) {
    data, err := json.Marshal(Session{PID: 1, Disabled: true})
    if err != nil {
        t.Fatal(err)
    }
    if !strings.Contains(string(data), `"disabled":true`) {
        t.Fatalf("marshaled JSON missing disabled field: %s", data)
    }

    data, err = json.Marshal(Session{PID: 1})
    if err != nil {
        t.Fatal(err)
    }
    if strings.Contains(string(data), `"disabled"`) {
        t.Fatalf("false disabled field must be omitted: %s", data)
    }

    var old Session
    if err := json.Unmarshal([]byte(`{"pid":1}`), &old); err != nil {
        t.Fatal(err)
    }
    if old.Disabled {
        t.Fatal("missing disabled field decoded as true")
    }
}

func TestReadSessionFileIgnoresPersistedDisabled(t *testing.T) {
    path := filepath.Join(t.TempDir(), "session.json")
    data, err := json.Marshal(Session{
        PID:       1,
        SessionID: "persisted-disabled",
        Disabled:  true,
    })
    if err != nil {
        t.Fatal(err)
    }
    if err := os.WriteFile(path, data, 0o600); err != nil {
        t.Fatal(err)
    }

    session, ok := readSessionFile(path)
    if !ok {
        t.Fatal("session file was not decoded")
    }
    if session.Disabled {
        t.Fatal("persisted disabled state became authoritative")
    }
}

func TestSortSessionsKeepsDisabledRowsLastInEveryMode(t *testing.T) {
    fixture := []Session{
        {SessionID: "disabled-busy", CWD: "/beta", Status: "busy", StartedAt: 200, UpdatedAt: 300, Disabled: true},
        {SessionID: "enabled-busy", CWD: "/beta", Status: "busy", StartedAt: 100, UpdatedAt: 400},
        {SessionID: "disabled-idle", CWD: "/alpha", Status: "idle", StartedAt: 400, UpdatedAt: 200, Disabled: true},
        {SessionID: "enabled-idle", CWD: "/alpha", Status: "idle", StartedAt: 300, UpdatedAt: 100},
    }
    cases := []struct {
        mode string
        want []string
    }{
        {"dir", []string{"enabled-idle", "enabled-busy", "disabled-idle", "disabled-busy"}},
        {"status", []string{"enabled-idle", "enabled-busy", "disabled-idle", "disabled-busy"}},
        {"created", []string{"enabled-idle", "enabled-busy", "disabled-idle", "disabled-busy"}},
        {"created-asc", []string{"enabled-busy", "enabled-idle", "disabled-busy", "disabled-idle"}},
        {"updated", []string{"enabled-busy", "enabled-idle", "disabled-busy", "disabled-idle"}},
        {"updated-asc", []string{"enabled-idle", "enabled-busy", "disabled-idle", "disabled-busy"}},
    }
    for _, tc := range cases {
        t.Run(tc.mode, func(t *testing.T) {
            rows := append([]Session(nil), fixture...)
            SortSessions(rows, tc.mode)
            got := make([]string, len(rows))
            for i := range rows {
                got[i] = rows[i].SessionID
            }
            if !equalStrings(got, tc.want) {
                t.Fatalf("SortSessions(%q) = %v, want %v", tc.mode, got, tc.want)
            }
        })
    }
}
```

Append to `tui_state_test.go`:

```go
func TestSettleSelectionKeepsToggledSessionAfterDisabledSortMove(t *testing.T) {
    rows := []Session{
        {PID: 1, SessionID: "one", CWD: "/alpha"},
        {PID: 2, SessionID: "two", CWD: "/beta"},
        {PID: 3, SessionID: "three", CWD: "/gamma"},
    }
    SortSessions(rows, "dir")

    state := newTUIState()
    state.sel = "2"
    state.settleSelection(buildSelectionTargets(rows, nil))

    rows[1].Disabled = true
    SortSessions(rows, "dir")
    if rows[2].SessionID != "two" {
        t.Fatalf("disabled row index = %v, want session two last", rows)
    }
    state.settleSelection(buildSelectionTargets(rows, nil))
    if state.sel != "2" {
        t.Fatalf("selection = %q, want toggled session 2", state.sel)
    }
}
```

- [ ] **Step 2: Run tests and verify expected failure**

Run:

```bash
go test ./... -run 'TestSessionDisabledJSONCompatibility|TestReadSessionFileIgnoresPersistedDisabled|TestSortSessionsKeepsDisabledRowsLastInEveryMode|TestSettleSelectionKeepsToggledSessionAfterDisabledSortMove'
```

Expected: compile failure because `Session.Disabled` does not exist; after adding only the field, persisted-state test still fails until `readSessionFile` clears it.

- [ ] **Step 3: Add field, clear persisted values, and add shared comparator**

Add to `Session` beside other server-derived metadata:

```go
Disabled bool `json:"disabled,omitempty"`
```

In `readSessionFile`, clear server-derived state immediately after successful JSON decode:

```go
if err := json.Unmarshal(data, &s); err != nil {
    return Session{}, false
}
s.Disabled = false
```

This makes direct `CollectLocal` fallback authoritative only for session-file fields and prevents a persisted or injected `disabled:true` key from bypassing server ownership.

Replace `SortSessions` with one stable sort calling a shared comparator:

```go
func sessionLess(a, b Session, mode string) bool {
    if a.Disabled != b.Disabled {
        return !a.Disabled
    }
    switch mode {
    case "status":
        ra, rb := sessionStatusRank(a), sessionStatusRank(b)
        if ra != rb {
            return ra < rb
        }
        return a.Updated().After(b.Updated())
    case "created":
        return a.StartedAt > b.StartedAt
    case "created-asc":
        return a.StartedAt < b.StartedAt
    case "updated":
        return a.Updated().After(b.Updated())
    case "updated-asc":
        return a.Updated().Before(b.Updated())
    default:
        ca, cb := strings.ToLower(a.CWD), strings.ToLower(b.CWD)
        if ca != cb {
            return ca < cb
        }
        return a.StartedAt > b.StartedAt
    }
}

func SortSessions(rows []Session, mode string) {
    sort.SliceStable(rows, func(i, j int) bool {
        return sessionLess(rows[i], rows[j], mode)
    })
}
```

- [ ] **Step 4: Format, then run focused and existing sort tests**

```bash
gofmt -w session.go session_test.go tui_state_test.go
go test ./... -run 'TestSessionDisabledJSONCompatibility|TestReadSessionFileIgnoresPersistedDisabled|TestSortSessions|TestSettleSelection'
```

Expected: PASS, including persisted-state, stability, and status-order regressions.

- [ ] **Step 5: Commit**

```bash
git add session.go session_test.go tui_state_test.go
git commit -m "feat: sort disabled sessions last"
```

---

### Task 2: Server-Owned Disabled Registry and PUT API

**Files:**
- Modify: `server.go:47-148, 163-183, 269-373, 506-518`
- Test: `server_test.go:348-472, 560-924`

**Interfaces:**
- Consumes: `server.collectLocal`, `server.cachedSessions`, `server.invalidateSessions`, bearer auth, `Session.Disabled`.
- Produces: authenticated `PUT /sessions/{pid}/disabled` requiring expected `sessionId` and returning resolved identity; successful collections annotate/prune disabled state without ever storing or matching an empty `SessionID`.

- [ ] **Step 1: Write failing handler/state tests**

Add `strconv` to `server_test.go` imports, then add the helper and tests:

```go
func putDisabled(s *server, pid int, body string, authed bool) *httptest.ResponseRecorder {
    req := httptest.NewRequest(http.MethodPut, fmt.Sprintf("/sessions/%d/disabled", pid), strings.NewReader(body))
    req.SetPathValue("pid", strconv.Itoa(pid))
    if authed {
        req.Header.Set("Authorization", "Bearer secret")
    }
    rec := httptest.NewRecorder()
    s.setDisabled(rec, req)
    return rec
}

func TestSetDisabledSetsClearsAndAnnotatesSessions(t *testing.T) {
    s := &server{
        token: "secret",
        collect: func() ([]Session, error) {
            return []Session{{PID: 42, SessionID: "session-42"}}, nil
        },
    }

    rec := putDisabled(s, 42, `{"disabled":true,"sessionId":"session-42"}`, true)
    if rec.Code != http.StatusOK ||
        !strings.Contains(rec.Body.String(), `"disabled":true`) ||
        !strings.Contains(rec.Body.String(), `"sessionId":"session-42"`) {
        t.Fatalf("set response = %d %s", rec.Code, rec.Body.String())
    }
    code, rows, err := getServerSessions(s)
    if err != nil || code != http.StatusOK || len(rows) != 1 || !rows[0].Disabled {
        t.Fatalf("annotated rows = (%d, %#v, %v)", code, rows, err)
    }

    rec = putDisabled(s, 42, `{"disabled":false,"sessionId":"session-42"}`, true)
    if rec.Code != http.StatusOK ||
        !strings.Contains(rec.Body.String(), `"disabled":false`) ||
        !strings.Contains(rec.Body.String(), `"sessionId":"session-42"`) {
        t.Fatalf("clear response = %d %s", rec.Code, rec.Body.String())
    }
    _, rows, err = getServerSessions(s)
    if err != nil || len(rows) != 1 || rows[0].Disabled {
        t.Fatalf("cleared rows = (%#v, %v)", rows, err)
    }
}

func TestSetDisabledValidatesRequest(t *testing.T) {
    live := &server{
        token: "secret",
        collect: func() ([]Session, error) {
            return []Session{{PID: 42, SessionID: "session-42"}}, nil
        },
    }
    cases := []struct {
        name string
        pid  int
        body string
        want int
    }{
        {"malformed", 42, `{`, http.StatusBadRequest},
        {"trailing JSON", 42, `{"disabled":true,"sessionId":"session-42"} {}`, http.StatusBadRequest},
        {"missing state", 42, `{"sessionId":"session-42"}`, http.StatusBadRequest},
        {"missing identity", 42, `{"disabled":true}`, http.StatusBadRequest},
        {"empty identity", 42, `{"disabled":true,"sessionId":""}`, http.StatusBadRequest},
    }
    for _, tc := range cases {
        t.Run(tc.name, func(t *testing.T) {
            rec := putDisabled(live, tc.pid, tc.body, true)
            if rec.Code != tc.want {
                t.Fatalf("status = %d, want %d; body=%q", rec.Code, tc.want, rec.Body.String())
            }
        })
    }

    req := httptest.NewRequest(
        http.MethodPut,
        "/sessions/not-a-pid/disabled",
        strings.NewReader(`{"disabled":true,"sessionId":"session-42"}`),
    )
    req.SetPathValue("pid", "not-a-pid")
    req.Header.Set("Authorization", "Bearer secret")
    rec := httptest.NewRecorder()
    live.setDisabled(rec, req)
    if rec.Code != http.StatusBadRequest {
        t.Fatalf("malformed PID status = %d, want %d", rec.Code, http.StatusBadRequest)
    }
}

func validationStateServer(collect func() ([]Session, error)) *server {
    return &server{
        token:   "secret",
        collect: collect,
        disabledSessionIDs: map[string]struct{}{
            "retained-session": {},
        },
        disabledGeneration: 7,
        sessionCache: sessionCache{
            sessions: []Session{{
                PID:       10,
                SessionID: "cached-session",
            }},
            completedAt:      time.Now(),
            valid:            true,
            cachedGeneration: 11,
            generation:       11,
        },
    }
}

func assertValidationStateUnchanged(t *testing.T, s *server) {
    t.Helper()

    s.disabledMu.RLock()
    _, retained := s.disabledSessionIDs["retained-session"]
    registryLen := len(s.disabledSessionIDs)
    disabledGeneration := s.disabledGeneration
    s.disabledMu.RUnlock()
    if !retained || registryLen != 1 || disabledGeneration != 7 {
        t.Fatalf(
            "validation mutated registry: registry=%#v generation=%d",
            s.disabledSessionIDs,
            disabledGeneration,
        )
    }

    s.sessionCache.mu.Lock()
    cacheValid := s.sessionCache.valid
    cacheGeneration := s.sessionCache.generation
    cachedGeneration := s.sessionCache.cachedGeneration
    cachedRows := len(s.sessionCache.sessions)
    completedAt := s.sessionCache.completedAt
    s.sessionCache.mu.Unlock()
    if !cacheValid ||
        cacheGeneration != 11 ||
        cachedGeneration != 11 ||
        cachedRows != 1 ||
        completedAt.IsZero() {
        t.Fatalf(
            "validation invalidated cache: valid=%v generation=%d cachedGeneration=%d rows=%d completedAt=%v",
            cacheValid,
            cacheGeneration,
            cachedGeneration,
            cachedRows,
            completedAt,
        )
    }
}

func TestSetDisabledUnknownPIDPreservesRegistryAndCache(t *testing.T) {
    s := validationStateServer(func() ([]Session, error) {
        return []Session{{PID: 42, SessionID: "session-42"}}, nil
    })

    rec := putDisabled(
        s,
        99,
        `{"disabled":true,"sessionId":"session-99"}`,
        true,
    )
    if rec.Code != http.StatusNotFound {
        t.Fatalf(
            "status = %d, want %d; body=%q",
            rec.Code,
            http.StatusNotFound,
            rec.Body.String(),
        )
    }
    assertValidationStateUnchanged(t, s)
}

func TestSetDisabledRejectsSessionWithoutStableID(t *testing.T) {
    s := validationStateServer(func() ([]Session, error) {
        return []Session{{PID: 42}}, nil
    })
    rec := putDisabled(
        s,
        42,
        `{"disabled":true,"sessionId":"selected-session"}`,
        true,
    )
    if rec.Code != http.StatusConflict {
        t.Fatalf("status = %d, want %d; body=%q", rec.Code, http.StatusConflict, rec.Body.String())
    }
    assertValidationStateUnchanged(t, s)
}

func TestSetDisabledRejectsReusedPIDWhenExpectedSessionChanged(t *testing.T) {
    s := validationStateServer(func() ([]Session, error) {
        return []Session{{PID: 42, SessionID: "new-session"}}, nil
    })

    rec := putDisabled(
        s,
        42,
        `{"disabled":true,"sessionId":"old-session"}`,
        true,
    )
    if rec.Code != http.StatusConflict {
        t.Fatalf(
            "status = %d, want %d; body=%q",
            rec.Code,
            http.StatusConflict,
            rec.Body.String(),
        )
    }
    assertValidationStateUnchanged(t, s)
}

func TestSetDisabledUnauthorizedDoesNotCollectOrMutate(t *testing.T) {
    collectCalls := 0
    s := &server{
        token: "secret",
        collect: func() ([]Session, error) {
            collectCalls++
            return []Session{{PID: 42, SessionID: "session-42"}}, nil
        },
        disabledSessionIDs: map[string]struct{}{"keep": {}},
    }
    rec := putDisabled(
        s,
        42,
        `{"disabled":true,"sessionId":"session-42"}`,
        false,
    )
    if rec.Code != http.StatusUnauthorized {
        t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
    }
    if collectCalls != 0 {
        t.Fatalf("unauthorized request collected sessions %d times", collectCalls)
    }
    s.disabledMu.RLock()
    defer s.disabledMu.RUnlock()
    if len(s.disabledSessionIDs) != 1 {
        t.Fatalf("disabled state mutated: %#v", s.disabledSessionIDs)
    }
    if _, ok := s.disabledSessionIDs["keep"]; !ok {
        t.Fatalf("existing disabled state was removed: %#v", s.disabledSessionIDs)
    }
}

func TestDisabledStateFollowsSessionIDNotReusedPID(t *testing.T) {
    current := Session{PID: 42, SessionID: "old-session"}
    s := &server{
        token: "secret",
        collect: func() ([]Session, error) { return []Session{current}, nil },
    }
    if rec := putDisabled(
        s,
        42,
        `{"disabled":true,"sessionId":"old-session"}`,
        true,
    ); rec.Code != http.StatusOK {
        t.Fatalf("set status = %d", rec.Code)
    }

    current = Session{PID: 42, SessionID: "new-session"}
    _, rows, err := getServerSessions(s)
    if err != nil || len(rows) != 1 {
        t.Fatalf("rows = %#v, err=%v", rows, err)
    }
    if rows[0].Disabled {
        t.Fatal("reused PID inherited old session disabled state")
    }
    s.disabledMu.RLock()
    _, stale := s.disabledSessionIDs["old-session"]
    s.disabledMu.RUnlock()
    if stale {
        t.Fatal("ended session ID was not pruned")
    }
}

func TestCollectionErrorDoesNotPruneDisabledState(t *testing.T) {
    s := &server{
        token: "secret",
        collect: func() ([]Session, error) { return nil, errors.New("collect failed") },
        disabledSessionIDs: map[string]struct{}{"keep-me": {}},
    }
    code, _, err := getServerSessions(s)
    if err != nil || code != http.StatusInternalServerError {
        t.Fatalf("response = (%d, %v)", code, err)
    }
    s.disabledMu.RLock()
    _, kept := s.disabledSessionIDs["keep-me"]
    s.disabledMu.RUnlock()
    if !kept {
        t.Fatal("collection error pruned disabled state")
    }
}

func TestCollectionStartedBeforeWriteDoesNotPruneNewerState(t *testing.T) {
    s := &server{}
    s.collect = func() ([]Session, error) {
        s.writeDisabled("newer-session", true)
        return nil, nil
    }
    if _, err := s.collectLocal(); err != nil {
        t.Fatal(err)
    }
    s.disabledMu.RLock()
    _, kept := s.disabledSessionIDs["newer-session"]
    s.disabledMu.RUnlock()
    if !kept {
        t.Fatal("collection pruned state written after collection started")
    }
}

func TestSetDisabledInvalidatesSessionCache(t *testing.T) {
    collectCalls := 0
    s := &server{
        token: "secret",
        collect: func() ([]Session, error) {
            collectCalls++
            return []Session{{PID: 42, SessionID: "session-42"}}, nil
        },
    }
    if code, _, err := getServerSessions(s); err != nil || code != http.StatusOK {
        t.Fatalf("initial listing = (%d, %v)", code, err)
    }
    if rec := putDisabled(
        s,
        42,
        `{"disabled":true,"sessionId":"session-42"}`,
        true,
    ); rec.Code != http.StatusOK {
        t.Fatalf("put status = %d", rec.Code)
    }
    _, rows, err := getServerSessions(s)
    if err != nil || len(rows) != 1 || !rows[0].Disabled {
        t.Fatalf("listing after put = (%#v, %v)", rows, err)
    }
    if collectCalls != 3 {
        t.Fatalf("collect calls = %d, want 3 (initial, PUT resolve, invalidated GET)", collectCalls)
    }
}

func TestDisabledStateConcurrentReadsAndWrites(t *testing.T) {
    s := &server{
        token: "secret",
        collect: func() ([]Session, error) {
            return []Session{{PID: 42, SessionID: "session-42"}}, nil
        },
    }
    var workers sync.WaitGroup
    for i := 0; i < 32; i++ {
        workers.Add(2)
        go func(disabled bool) {
            defer workers.Done()
            putDisabled(
                s,
                42,
                fmt.Sprintf(
                    `{"disabled":%t,"sessionId":"session-42"}`,
                    disabled,
                ),
                true,
            )
        }(i%2 == 0)
        go func() {
            defer workers.Done()
            _, _, _ = getServerSessions(s)
        }()
    }
    workers.Wait()
}
```

- [ ] **Step 2: Run focused tests and verify expected failure**

```bash
go test ./... -run 'TestSetDisabled|TestDisabledState|TestCollection'
```

Expected: compile failure because server registry and `setDisabled` handler do not exist.

- [ ] **Step 3: Add registry fields and annotate every successful direct collection**

Add fields to `server`:

```go
disabledMu         sync.RWMutex
disabledSessionIDs map[string]struct{}
disabledGeneration uint64
```

Split raw collection from authoritative GET annotation:

```go
func (s *server) collectLocalRaw() ([]Session, error) {
    if s.collect != nil {
        return s.collect()
    }
    return CollectLocal()
}

func (s *server) collectLocal() ([]Session, error) {
    s.disabledMu.RLock()
    disabledGeneration := s.disabledGeneration
    s.disabledMu.RUnlock()

    sessions, err := s.collectLocalRaw()
    if err != nil {
        return nil, err
    }
    s.annotateDisabled(sessions, disabledGeneration)
    return sessions, nil
}
```

`GET /sessions` continues through `collectLocal` and therefore annotates/prunes. PUT resolution uses `collectLocalRaw` so `404`/`409` paths cannot prune or otherwise mutate registry state before request validation succeeds.

Add helpers:

```go
func (s *server) annotateDisabled(sessions []Session, collectedGeneration uint64) {
    live := make(map[string]struct{}, len(sessions))
    for _, session := range sessions {
        if session.SessionID != "" {
            live[session.SessionID] = struct{}{}
        }
    }

    s.disabledMu.Lock()
    defer s.disabledMu.Unlock()
    if s.disabledGeneration == collectedGeneration {
        for sessionID := range s.disabledSessionIDs {
            if _, ok := live[sessionID]; !ok {
                delete(s.disabledSessionIDs, sessionID)
            }
        }
    }
    for i := range sessions {
        if sessions[i].SessionID == "" {
            sessions[i].Disabled = false
            continue
        }
        _, sessions[i].Disabled = s.disabledSessionIDs[sessions[i].SessionID]
    }
}

func (s *server) writeDisabled(sessionID string, disabled bool) {
    if sessionID == "" {
        return
    }
    s.disabledMu.Lock()
    defer s.disabledMu.Unlock()
    if s.disabledSessionIDs == nil {
        s.disabledSessionIDs = make(map[string]struct{})
    }
    if disabled {
        s.disabledSessionIDs[sessionID] = struct{}{}
    } else {
        delete(s.disabledSessionIDs, sessionID)
    }
    s.disabledGeneration++
}
```

`disabledGeneration` prevents a collection that started before a successful PUT from pruning that newer write. If any write overlaps a collection, that collection still annotates current state but defers pruning until a later successful collection with a matching generation.

- [ ] **Step 4: Add authenticated idempotent PUT handler**

Add to `server.go`:

```go
func (s *server) setDisabled(w http.ResponseWriter, r *http.Request) {
    if !s.authed(r) {
        http.Error(w, "Unauthorized", http.StatusUnauthorized)
        return
    }
    pid, err := strconv.Atoi(r.PathValue("pid"))
    if err != nil {
        http.Error(w, "bad pid", http.StatusBadRequest)
        return
    }

    var body struct {
        Disabled  *bool   `json:"disabled"`
        SessionID *string `json:"sessionId"`
    }
    decoder := json.NewDecoder(r.Body)
    if err := decoder.Decode(&body); err != nil {
        http.Error(w, "invalid JSON body", http.StatusBadRequest)
        return
    }
    if body.Disabled == nil {
        http.Error(w, "disabled boolean required", http.StatusBadRequest)
        return
    }
    if body.SessionID == nil || *body.SessionID == "" {
        http.Error(w, "non-empty sessionId required", http.StatusBadRequest)
        return
    }
    if err := decoder.Decode(&struct{}{}); err != io.EOF {
        http.Error(w, "request body must contain one JSON object", http.StatusBadRequest)
        return
    }

    sessions, err := s.collectLocalRaw()
    if err != nil {
        http.Error(w, err.Error(), http.StatusInternalServerError)
        return
    }
    var target *Session
    for i := range sessions {
        if sessions[i].PID == pid {
            target = &sessions[i]
            break
        }
    }
    if target == nil {
        http.Error(w, fmt.Sprintf("PID %d is not a live Claude session", pid), http.StatusNotFound)
        return
    }
    if target.SessionID == "" {
        http.Error(w, fmt.Sprintf("PID %d has no stable session ID", pid), http.StatusConflict)
        return
    }
    if target.SessionID != *body.SessionID {
        http.Error(
            w,
            fmt.Sprintf(
                "PID %d now belongs to session %q, not %q",
                pid,
                target.SessionID,
                *body.SessionID,
            ),
            http.StatusConflict,
        )
        return
    }

    s.writeDisabled(target.SessionID, *body.Disabled)
    s.invalidateSessions()
    writeJSON(w, http.StatusOK, struct {
        Disabled  bool   `json:"disabled"`
        SessionID string `json:"sessionId"`
    }{
        Disabled:  *body.Disabled,
        SessionID: target.SessionID,
    })
}
```

Add `io` to `server.go` imports for the trailing-body EOF check. Register exact route in `cmdServer`:

```go
mux.HandleFunc("PUT /sessions/{pid}/disabled", s.setDisabled)
```

- [ ] **Step 5: Format, then run server tests with race detector**

```bash
gofmt -w server.go server_test.go
go test -race ./... -run 'TestSessions|TestSetDisabled|TestDisabledState|TestCollection'
```

Expected: PASS; identity mismatch returns `409` without mutation; no race report; existing cache/single-flight tests remain green.

- [ ] **Step 6: Commit**

```bash
git add server.go server_test.go
git commit -m "feat: store disabled session state on server"
```

---

### Task 3: Shared Server Client, Loopback-First Collection, and PUT Transport

**Files:**
- Create: `server_client.go`
- Create: `server_client_test.go`
- Modify: `remote_actions.go:18-56`
- Modify: `remote_test.go:13-23`

**Interfaces:**
- Consumes: `ServerConfig`, `LookupServer`, `loadOrCreateToken`, existing test helper `writeServerYAML(t *testing.T, home, name, host, port, token string)`, and existing HTTP error style.
- Produces: `disabledState`; bounded `fetchSessionsFromServer`/`fetchLocalServerSessions`; `collectClientLocal`; identity-guarded `setSessionDisabled`; `patchDisabledBySessionID`; lower-level `serverRequestWithTimeout`.

- [ ] **Step 1: Write failing transport/fallback tests**

Create `server_client_test.go`:

```go
package main

import (
    "encoding/json"
    "errors"
    "fmt"
    "net"
    "net/http"
    "net/http/httptest"
    "net/url"
    "strconv"
    "strings"
    "testing"
    "time"
)

func serverConfigForURL(t *testing.T, rawURL, token string) ServerConfig {
    t.Helper()
    parsed, err := url.Parse(rawURL)
    if err != nil {
        t.Fatal(err)
    }
    host, portText, err := net.SplitHostPort(parsed.Host)
    if err != nil {
        t.Fatal(err)
    }
    port, err := strconv.Atoi(portText)
    if err != nil {
        t.Fatal(err)
    }
    return ServerConfig{Host: host, Port: port, Token: token}
}

func TestCollectClientLocalPrefersServerAndFallsBack(t *testing.T) {
    serverRows := []Session{{PID: 1, SessionID: "server", Disabled: true}}
    directRows := []Session{{PID: 2, SessionID: "direct"}}

    got, err := collectClientLocalWith(
        func() ([]Session, error) { return serverRows, nil },
        func() ([]Session, error) {
            t.Fatal("direct collector called after server success")
            return nil, nil
        },
    )
    if err != nil || len(got) != 1 || got[0].SessionID != "server" || !got[0].Disabled {
        t.Fatalf("server result = (%#v, %v)", got, err)
    }

    got, err = collectClientLocalWith(
        func() ([]Session, error) { return nil, errors.New("server down") },
        func() ([]Session, error) { return directRows, nil },
    )
    if err != nil || len(got) != 1 || got[0].SessionID != "direct" || got[0].Disabled {
        t.Fatalf("fallback result = (%#v, %v)", got, err)
    }

    directErr := errors.New("direct collection failed")
    got, err = collectClientLocalWith(
        func() ([]Session, error) { return nil, errors.New("server down") },
        func() ([]Session, error) { return nil, directErr },
    )
    if got != nil || !errors.Is(err, directErr) {
        t.Fatalf("double failure = (%#v, %v), want direct collector error", got, err)
    }
}

func TestSessionServerConfigUsesLocalAndRemoteEndpoints(t *testing.T) {
    home := t.TempDir()
    t.Setenv("HOME", home)

    local, err := sessionServerConfig("")
    if err != nil {
        t.Fatal(err)
    }
    if local.Host != "127.0.0.1" || local.Port != 8765 || local.Token == "" {
        t.Fatalf("local config = %#v", local)
    }
    if localServerTimeout != 750*time.Millisecond {
        t.Fatalf("local timeout = %s, want 750ms", localServerTimeout)
    }

    writeServerYAML(t, home, "orca", "10.0.0.8", "9876", "remote-secret")
    remote, err := sessionServerConfig("orca")
    if err != nil {
        t.Fatal(err)
    }
    if remote.Name != "orca" || remote.Host != "10.0.0.8" ||
        remote.Port != 9876 || remote.Token != "remote-secret" {
        t.Fatalf("remote config = %#v", remote)
    }
    if _, err := sessionServerConfig("missing"); err == nil ||
        !strings.Contains(err.Error(), "unknown server: missing") {
        t.Fatalf("missing remote error = %v", err)
    }
}

func TestFetchSessionsFromServerHonorsBoundedTimeout(t *testing.T) {
    backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        <-r.Context().Done()
    }))
    defer backend.Close()

    started := time.Now()
    _, err := fetchSessionsFromServer(
        serverConfigForURL(t, backend.URL, "secret"),
        25*time.Millisecond,
    )
    if err == nil {
        t.Fatal("slow server request unexpectedly succeeded")
    }
    if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
        t.Fatalf("bounded request took %s", elapsed)
    }
}

func TestPutSessionDisabledUsesExplicitStateAndIdentity(t *testing.T) {
    type capturedRequest struct {
        method      string
        path        string
        auth        string
        contentType string
        disabled    *bool
        sessionID   *string
        decodeErr   error
    }
    requests := make(chan capturedRequest, 1)
    backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        var body struct {
            Disabled  *bool   `json:"disabled"`
            SessionID *string `json:"sessionId"`
        }
        decodeErr := json.NewDecoder(r.Body).Decode(&body)
        requests <- capturedRequest{
            method:      r.Method,
            path:        r.URL.Path,
            auth:        r.Header.Get("Authorization"),
            contentType: r.Header.Get("Content-Type"),
            disabled:    body.Disabled,
            sessionID:   body.SessionID,
            decodeErr:   decodeErr,
        }
        fmt.Fprint(w, `{"disabled":true,"sessionId":"session-42"}`)
    }))
    defer backend.Close()

    state, err := putSessionDisabled(
        serverConfigForURL(t, backend.URL, "secret"),
        42,
        "session-42",
        true,
    )
    if err != nil {
        t.Fatal(err)
    }
    got := <-requests
    if got.decodeErr != nil {
        t.Fatal(got.decodeErr)
    }
    if !state.Disabled || state.SessionID != "session-42" ||
        got.method != http.MethodPut || got.path != "/sessions/42/disabled" ||
        got.auth != "Bearer secret" || got.contentType != "application/json" {
        t.Fatalf("state=%#v request=%#v", state, got)
    }
    if got.disabled == nil || !*got.disabled ||
        got.sessionID == nil || *got.sessionID != "session-42" {
        t.Fatalf("request body = %#v", got)
    }
}

func TestPutSessionDisabledRejectsBadResponses(t *testing.T) {
    cases := []struct {
        name string
        body string
        want string
    }{
        {"malformed JSON", `{`, "bad response:"},
        {"missing disabled", `{"sessionId":"session-42"}`, "bad response: missing disabled"},
        {"missing identity", `{"disabled":true}`, "bad response: missing sessionId"},
        {"empty identity", `{"disabled":true,"sessionId":""}`, "bad response: missing sessionId"},
        {"mismatched identity", `{"disabled":true,"sessionId":"replacement"}`, "bad response: sessionId mismatch"},
    }
    for _, tc := range cases {
        t.Run(tc.name, func(t *testing.T) {
            backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
                fmt.Fprint(w, tc.body)
            }))
            defer backend.Close()
            _, err := putSessionDisabled(
                serverConfigForURL(t, backend.URL, "secret"),
                42,
                "session-42",
                true,
            )
            if err == nil || !strings.Contains(err.Error(), tc.want) {
                t.Fatalf("error = %v, want substring %q", err, tc.want)
            }
        })
    }
}

func TestPutSessionDisabledRejectsEmptyRequestIdentity(t *testing.T) {
    _, err := putSessionDisabled(ServerConfig{}, 42, "", true)
    if err == nil || err.Error() != "session ID required" {
        t.Fatalf("error = %v", err)
    }
}

func TestPutSessionDisabledPreservesHTTPError(t *testing.T) {
    backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        http.Error(w, "session ended", http.StatusNotFound)
    }))
    defer backend.Close()
    _, err := putSessionDisabled(
        serverConfigForURL(t, backend.URL, "secret"),
        42,
        "session-42",
        true,
    )
    if err == nil || !strings.Contains(err.Error(), "HTTP 404: session ended") {
        t.Fatalf("error = %v", err)
    }
}

func TestPatchDisabledBySessionID(t *testing.T) {
    rows := []Session{{SessionID: "one"}, {SessionID: "two"}}
    if !patchDisabledBySessionID(rows, "two", true) {
        t.Fatal("target session was not patched")
    }
    if rows[0].Disabled || !rows[1].Disabled {
        t.Fatalf("rows = %#v", rows)
    }
    if patchDisabledBySessionID(rows, "missing", true) {
        t.Fatal("missing session reported as patched")
    }
    rows = append(rows, Session{})
    if patchDisabledBySessionID(rows, "", true) || rows[2].Disabled {
        t.Fatal("empty session ID must never be patched")
    }
}
```

Add to `remote_test.go` by changing fixture JSON and assertion:

```go
"sessions":[{"pid":42,"sessionId":"remote-42","cwd":"/srv/app","disabled":true}]
```

```go
if len(result.Sessions) != 1 || result.Sessions[0].Host != "alias" || !result.Sessions[0].Disabled {
    t.Fatalf("sessions = %#v", result.Sessions)
}
```

- [ ] **Step 2: Run tests and verify expected failure**

```bash
go test ./... -run 'TestCollectClientLocal|TestSessionServerConfig|TestFetchSessionsFromServer|TestPutSessionDisabled|TestPatchDisabledBySessionID|TestFetchRemoteDecodes'
```

Expected: compile failure because client helpers do not exist.

- [ ] **Step 3: Extract resolved-server request helper**

In `remote_actions.go`, move existing HTTP logic into:

```go
func serverRequestWithTimeout(srv ServerConfig, path, method string, body []byte, timeout time.Duration) ([]byte, error) {
    url := fmt.Sprintf("http://%s:%d%s", srv.Host, srv.Port, path)
    var bodyReader io.Reader
    if len(body) > 0 {
        bodyReader = bytes.NewReader(body)
    }
    req, err := http.NewRequest(method, url, bodyReader)
    if err != nil {
        return nil, err
    }
    req.Header.Set("Authorization", "Bearer "+srv.Token)
    if len(body) > 0 {
        req.Header.Set("Content-Type", "application/json")
    }
    client := &http.Client{Timeout: timeout}
    resp, err := client.Do(req)
    if err != nil {
        return nil, err
    }
    defer resp.Body.Close()
    data, _ := io.ReadAll(resp.Body)
    if resp.StatusCode != http.StatusOK {
        return data, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
    }
    return data, nil
}

func remoteRequestWithTimeout(name, path, method string, body []byte, timeout time.Duration) ([]byte, error) {
    srv, ok := LookupServer(name)
    if !ok {
        return nil, fmt.Errorf("unknown server: %s", name)
    }
    return serverRequestWithTimeout(srv, path, method, body, timeout)
}
```

- [ ] **Step 4: Create local/server client helpers**

Create `server_client.go`:

```go
package main

import (
    "encoding/json"
    "errors"
    "fmt"
    "net/http"
    "time"
)

const (
    localServerHost       = "127.0.0.1"
    localServerPort       = 8765
    localServerTimeout    = 750 * time.Millisecond
    disabledWriteTimeout  = 5 * time.Second
)

type disabledState struct {
    SessionID string
    Disabled  bool
}

func sessionServerConfig(host string) (ServerConfig, error) {
    if host != "" {
        srv, ok := LookupServer(host)
        if !ok {
            return ServerConfig{}, fmt.Errorf("unknown server: %s", host)
        }
        return srv, nil
    }
    token, err := loadOrCreateToken()
    if err != nil {
        return ServerConfig{}, err
    }
    return ServerConfig{
        Host:  localServerHost,
        Port:  localServerPort,
        Token: token,
    }, nil
}

func fetchSessionsFromServer(
    srv ServerConfig,
    timeout time.Duration,
) ([]Session, error) {
    data, err := serverRequestWithTimeout(
        srv,
        "/sessions",
        http.MethodGet,
        nil,
        timeout,
    )
    if err != nil {
        return nil, err
    }
    var response struct {
        Sessions []Session `json:"sessions"`
    }
    if err := json.Unmarshal(data, &response); err != nil {
        return nil, fmt.Errorf("bad response: %w", err)
    }
    return response.Sessions, nil
}

func fetchLocalServerSessions() ([]Session, error) {
    srv, err := sessionServerConfig("")
    if err != nil {
        return nil, err
    }
    return fetchSessionsFromServer(srv, localServerTimeout)
}

func collectClientLocalWith(
    serverFetch, directCollect func() ([]Session, error),
) ([]Session, error) {
    if sessions, err := serverFetch(); err == nil {
        return sessions, nil
    }
    return directCollect()
}

func collectClientLocal() ([]Session, error) {
    return collectClientLocalWith(fetchLocalServerSessions, CollectLocal)
}

func putSessionDisabled(
    srv ServerConfig,
    pid int,
    sessionID string,
    disabled bool,
) (disabledState, error) {
    if sessionID == "" {
        return disabledState{}, errors.New("session ID required")
    }
    body, err := json.Marshal(struct {
        Disabled  bool   `json:"disabled"`
        SessionID string `json:"sessionId"`
    }{
        Disabled:  disabled,
        SessionID: sessionID,
    })
    if err != nil {
        return disabledState{}, err
    }
    data, err := serverRequestWithTimeout(
        srv,
        fmt.Sprintf("/sessions/%d/disabled", pid),
        http.MethodPut,
        body,
        disabledWriteTimeout,
    )
    if err != nil {
        return disabledState{}, err
    }
    var response struct {
        Disabled  *bool   `json:"disabled"`
        SessionID *string `json:"sessionId"`
    }
    if err := json.Unmarshal(data, &response); err != nil {
        return disabledState{}, fmt.Errorf("bad response: %w", err)
    }
    if response.Disabled == nil {
        return disabledState{}, errors.New("bad response: missing disabled")
    }
    if response.SessionID == nil || *response.SessionID == "" {
        return disabledState{}, errors.New("bad response: missing sessionId")
    }
    if *response.SessionID != sessionID {
        return disabledState{}, fmt.Errorf(
            "bad response: sessionId mismatch: got %q, want %q",
            *response.SessionID,
            sessionID,
        )
    }
    return disabledState{
        SessionID: *response.SessionID,
        Disabled:  *response.Disabled,
    }, nil
}

func setSessionDisabled(
    host string,
    pid int,
    sessionID string,
    disabled bool,
) (disabledState, error) {
    srv, err := sessionServerConfig(host)
    if err != nil {
        return disabledState{}, err
    }
    return putSessionDisabled(srv, pid, sessionID, disabled)
}

func patchDisabledBySessionID(
    rows []Session,
    sessionID string,
    disabled bool,
) bool {
    if sessionID == "" {
        return false
    }
    for i := range rows {
        if rows[i].SessionID == sessionID {
            rows[i].Disabled = disabled
            return true
        }
    }
    return false
}
```

- [ ] **Step 5: Format, then run client and existing remote-action tests**

```bash
gofmt -w server_client.go server_client_test.go remote_actions.go remote_test.go
go test ./... -run 'TestCollectClientLocal|TestSessionServerConfig|TestFetchSessionsFromServer|TestPutSessionDisabled|TestPatchDisabledBySessionID|TestFetchRemote|TestFetchRemotePreview'
```

Expected: PASS; local listing uses bounded `750ms` timeout, PUT sends expected identity, and mismatched response identity is rejected.

- [ ] **Step 6: Commit**

```bash
git add server_client.go server_client_test.go remote_actions.go remote_test.go
git commit -m "feat: add disabled session client transport"
```

---

### Task 4: RemoteHub Immediate Patch and Generation-Fenced Poll Reconciliation

**Files:**
- Modify: `remote.go:102-244`
- Test: `remote_test.go:66+`

**Interfaces:**
- Consumes: `patchDisabledBySessionID`, `RemoteResult.Name`, `Session.SessionID`.
- Produces: `RemoteHub.PatchDisabled`; generation-tagged result storage that overlays only poll cycles already started when a write completed, then accepts the first successful later cycle as authoritative.

- [ ] **Step 1: Write failing generation-fence and snapshot-isolation tests**

Append to `remote_test.go`:

```go
func TestRemoteHubPatchDisabledProtectsOnlyPreWriteFetchGeneration(t *testing.T) {
    h := &RemoteHub{
        fetchGeneration: 4,
        results: []RemoteResult{{
            Name: "alias",
            Sessions: []Session{{PID: 42, SessionID: "session-42"}},
        }},
    }

    h.PatchDisabled("alias", "session-42", true)
    snapshot := h.Snapshot()
    if !snapshot[0].Sessions[0].Disabled {
        t.Fatal("immediate remote patch was not visible")
    }

    h.storeRemoteResult(0, 4, RemoteResult{
        Name: "alias",
        Sessions: []Session{{PID: 42, SessionID: "session-42", Disabled: false}},
    })
    snapshot = h.Snapshot()
    if !snapshot[0].Sessions[0].Disabled {
        t.Fatal("pre-write fetch overwrote pending disabled state")
    }
    if len(h.pendingDisabled) != 1 {
        t.Fatalf("pending overrides = %d, want 1", len(h.pendingDisabled))
    }

    h.storeRemoteResult(0, 5, RemoteResult{
        Name: "alias",
        Sessions: []Session{{PID: 42, SessionID: "session-42", Disabled: true}},
    })
    if len(h.pendingDisabled) != 0 {
        t.Fatalf("post-write authoritative fetch did not clear override: %#v", h.pendingDisabled)
    }
}

func TestRemoteHubNewerAuthoritativeWriteSupersedesPendingState(t *testing.T) {
    h := &RemoteHub{
        fetchGeneration: 8,
        results: []RemoteResult{{
            Name: "alias",
            Sessions: []Session{{PID: 42, SessionID: "session-42"}},
        }},
    }
    h.PatchDisabled("alias", "session-42", true)

    h.storeRemoteResult(0, 8, RemoteResult{
        Name: "alias",
        Sessions: []Session{{PID: 42, SessionID: "session-42", Disabled: false}},
    })
    if !h.Snapshot()[0].Sessions[0].Disabled {
        t.Fatal("pre-write fetch was not fenced")
    }

    h.storeRemoteResult(0, 9, RemoteResult{
        Name: "alias",
        Sessions: []Session{{PID: 42, SessionID: "session-42", Disabled: false}},
    })
    snapshot := h.Snapshot()
    if snapshot[0].Sessions[0].Disabled {
        t.Fatal("later authoritative write did not supersede pending state")
    }
    if len(h.pendingDisabled) != 0 {
        t.Fatalf("superseded override was not cleared: %#v", h.pendingDisabled)
    }
}

func TestRemoteHubPendingDisabledSurvivesErrorAndClearsWhenSessionEnds(t *testing.T) {
    h := &RemoteHub{
        fetchGeneration: 2,
        results: []RemoteResult{{
            Name: "alias",
            Sessions: []Session{{PID: 42, SessionID: "session-42"}},
        }},
    }
    h.PatchDisabled("alias", "session-42", true)
    h.storeRemoteResult(0, 3, RemoteResult{Name: "alias", Error: "timeout"})
    if len(h.pendingDisabled) != 1 {
        t.Fatal("remote error cleared pending override")
    }
    h.storeRemoteResult(0, 4, RemoteResult{Name: "alias", Sessions: nil})
    if len(h.pendingDisabled) != 0 {
        t.Fatal("successful post-write absent-session response did not clear override")
    }
}

func TestRemoteHubPendingDisabledIsScopedByHost(t *testing.T) {
    h := &RemoteHub{
        results: []RemoteResult{
            {Name: "orca", Sessions: []Session{{PID: 42, SessionID: "shared-id"}}},
            {Name: "beluga", Sessions: []Session{{PID: 42, SessionID: "shared-id"}}},
        },
    }
    h.PatchDisabled("orca", "shared-id", true)
    snapshot := h.Snapshot()
    if !snapshot[0].Sessions[0].Disabled {
        t.Fatal("target host was not patched")
    }
    if snapshot[1].Sessions[0].Disabled {
        t.Fatal("same session ID on another host was patched")
    }
    if len(h.pendingDisabled) != 1 {
        t.Fatalf("pending overrides = %#v", h.pendingDisabled)
    }

    h.PatchDisabled("orca", "", true)
    if len(h.pendingDisabled) != 1 {
        t.Fatalf("empty session ID created override: %#v", h.pendingDisabled)
    }
}

func TestRemoteHubSnapshotDoesNotAliasHubState(t *testing.T) {
    h := &RemoteHub{
        results: []RemoteResult{{
            Name: "alias",
            Sessions: []Session{{
                PID:       42,
                SessionID: "session-42",
            }},
        }},
    }

    beforePatch := h.Snapshot()
    h.PatchDisabled("alias", "session-42", true)
    if beforePatch[0].Sessions[0].Disabled {
        t.Fatal("hub patch retroactively changed prior snapshot")
    }

    callerCopy := h.Snapshot()
    callerCopy[0].Sessions[0].Disabled = false
    if !h.Snapshot()[0].Sessions[0].Disabled {
        t.Fatal("caller mutation changed hub-owned session state")
    }
}

func TestRemoteHubSnapshotPatchAndStoreAreRaceFree(t *testing.T) {
    h := &RemoteHub{
        fetchGeneration: 1,
        results: []RemoteResult{{
            Name: "alias",
            Sessions: []Session{{
                PID:       42,
                SessionID: "session-42",
            }},
        }},
    }

    start := make(chan struct{})
    var wg sync.WaitGroup
    wg.Add(3)

    go func() {
        defer wg.Done()
        <-start
        for range 200 {
            snapshot := h.Snapshot()
            if len(snapshot) != 0 && len(snapshot[0].Sessions) != 0 {
                _ = snapshot[0].Sessions[0].Disabled
            }
        }
    }()
    go func() {
        defer wg.Done()
        <-start
        for i := range 200 {
            h.PatchDisabled("alias", "session-42", i%2 == 0)
        }
    }()
    go func() {
        defer wg.Done()
        <-start
        for i := range 200 {
            h.storeRemoteResult(0, 1, RemoteResult{
                Name: "alias",
                Sessions: []Session{{
                    PID:       42,
                    SessionID: "session-42",
                    Disabled:  i%2 == 0,
                }},
            })
        }
    }()

    close(start)
    wg.Wait()
}
```

Add `sync` to `remote_test.go` imports.

- [ ] **Step 2: Run tests and verify expected failure**

```bash
go test ./... -run 'TestRemoteHub'
```

Expected: compile failure because generation fields, pending map, patch, and generation-aware store helpers do not exist; after temporary stubs, snapshot isolation test fails because current `Snapshot` aliases hub-owned nested session slices.

- [ ] **Step 3: Add generation-fenced pending override model**

Add near `RemoteHub`:

```go
type disabledOverrideKey struct {
    host      string
    sessionID string
}

type pendingDisabledOverride struct {
    disabled       bool
    protectThrough uint64
}
```

Add fields to `RemoteHub`:

```go
fetchGeneration uint64
pendingDisabled map[disabledOverrideKey]pendingDisabledOverride
```

Initialize the map in `NewRemoteHub`:

```go
pendingDisabled: make(map[disabledOverrideKey]pendingDisabledOverride),
```

Replace `Snapshot` so callers never retain aliases to hub-owned nested session slices:

```go
func (h *RemoteHub) Snapshot() []RemoteResult {
    h.mu.Lock()
    defer h.mu.Unlock()

    results := make([]RemoteResult, len(h.results))
    copy(results, h.results)
    for i := range results {
        results[i].Sessions = append(
            []Session(nil),
            h.results[i].Sessions...,
        )
    }
    return results
}
```

Add methods:

```go
func (h *RemoteHub) PatchDisabled(host, sessionID string, disabled bool) {
    if sessionID == "" {
        return
    }

    h.mu.Lock()
    defer h.mu.Unlock()

    if h.pendingDisabled == nil {
        h.pendingDisabled = make(
            map[disabledOverrideKey]pendingDisabledOverride,
        )
    }

    key := disabledOverrideKey{host: host, sessionID: sessionID}
    h.pendingDisabled[key] = pendingDisabledOverride{
        disabled:       disabled,
        protectThrough: h.fetchGeneration,
    }

    for i := range h.results {
        if h.results[i].Name == host {
            patchDisabledBySessionID(
                h.results[i].Sessions,
                sessionID,
                disabled,
            )
        }
    }
}

func (h *RemoteHub) applyPendingDisabledLocked(
    generation uint64,
    result *RemoteResult,
) {
    if result.Error != "" {
        return
    }

    for key, pending := range h.pendingDisabled {
        if key.host != result.Name {
            continue
        }

        if generation > pending.protectThrough {
            delete(h.pendingDisabled, key)
            continue
        }

        patchDisabledBySessionID(
            result.Sessions,
            key.sessionID,
            pending.disabled,
        )
    }
}

func (h *RemoteHub) storeRemoteResult(
    index int,
    generation uint64,
    result RemoteResult,
) {
    h.mu.Lock()
    h.applyPendingDisabledLocked(generation, &result)
    h.results[index] = result
    h.mu.Unlock()
}
```

At the start of the existing locked setup section in `fetchAll`, increment and capture the cycle generation before publishing loading rows:

```go
h.mu.Lock()
h.fetchGeneration++
generation := h.fetchGeneration

prev := make(map[string]RemoteResult, len(h.results))
```

Keep the remainder of the existing setup section unchanged. Replace each goroutine's direct result assignment with generation-aware storage:

```go
r := FetchRemote(c)
h.storeRemoteResult(i, generation, r)
h.signalWake()
```

`RemoteHub.run` already serializes complete `fetchAll` calls, while `fetchAll` waits for all host goroutines. Therefore each cycle gets one monotonic generation, and `PatchDisabled` can fence exactly the cycle already started when the successful write completed. Results from that generation or older are overlaid; first successful later-generation result is accepted unchanged and clears pending state, even if another client has written the opposite value.

- [ ] **Step 4: Format, then run remote tests and race detector**

```bash
gofmt -w remote.go remote_test.go
go test -race ./... -run 'TestRemoteHub|TestFetchRemote'
```

Expected: PASS; snapshots are deep copies, pre-write generation remains visually fenced, first successful later generation is authoritative, errors retain pending state, absent sessions clear on a successful later generation, and race detector reports no races.

- [ ] **Step 5: Commit**

```bash
git add remote.go remote_test.go
git commit -m "feat: reconcile remote disabled toggles"
```

---

### Task 5: Toggle Action, TUI Data Flow, Footer, and Help

**Files:**
- Modify: `actions.go:18-40, 66-104`
- Test: `actions_test.go:21-181`
- Modify: `tui.go:154-270, 382-503, 607-646`
- Modify: `tui_state.go:234-260, 328-349`
- Test: `tui_test.go:55-65`
- Test: `tui_state_test.go:399+`

**Interfaces:**
- Consumes: `collectClientLocal`, `setSessionDisabled`, `patchDisabledBySessionID`, `RemoteHub.PatchDisabled`, `SortSessions`, `tuiState.settleSelection`, `tuiState.resolveListOffset`, `actCtx.prepareLineOutput`, `screenRenderer.Invalidate`, `withBottomRow`.
- Produces: `actToggleDisabled`; `tuiState.requestSelectionAnchor`; `d`/`D` hotkey; immediate local/remote patch; post-sort viewport re-anchor; permanent `d disable/enable` footer replaced temporarily by sort toast; updated pure-content help.

- [ ] **Step 1: Write failing action tests**

Append to `actions_test.go`:

```go
func TestActToggleDisabledRoutesLocalAndRemoteAndUsesServerResponse(t *testing.T) {
    cases := []struct {
        name           string
        session        Session
        wantHost       string
        wantRequest    bool
        serverDisabled bool
    }{
        {"local enable to disabled", Session{PID: 10, SessionID: "local"}, "", true, true},
        {"local disabled to enabled", Session{PID: 11, SessionID: "local-off", Disabled: true}, "", false, false},
        {"remote enable to disabled", Session{PID: 20, SessionID: "remote", Host: "orca"}, "orca", true, true},
        {"server response is authoritative", Session{PID: 30, SessionID: "authoritative"}, "", true, false},
    }
    for _, tc := range cases {
        t.Run(tc.name, func(t *testing.T) {
            target := sessionSelectionTarget(tc.session)
            c := &actCtx{targets: []selectionTarget{target}, sel: target.id}
            c.updateDisabled = func(
                host string,
                pid int,
                sessionID string,
                disabled bool,
            ) (disabledState, error) {
                if host != tc.wantHost || pid != tc.session.PID ||
                    sessionID != tc.session.SessionID || disabled != tc.wantRequest {
                    t.Fatalf(
                        "request = (%q, %d, %q, %v)",
                        host,
                        pid,
                        sessionID,
                        disabled,
                    )
                }
                return disabledState{
                    SessionID: tc.session.SessionID,
                    Disabled:  tc.serverDisabled,
                }, nil
            }
            update, err := actToggleDisabled(c)
            if err != nil {
                t.Fatal(err)
            }
            if update == nil || update.Host != tc.wantHost ||
                update.SessionID != tc.session.SessionID ||
                update.Disabled != tc.serverDisabled {
                t.Fatalf("update = %#v", update)
            }
        })
    }
}

func TestActToggleDisabledIgnoresEmptyHostAndReturnsFailure(t *testing.T) {
    empty := emptyHostSelectionTarget("orca")
    called := false
    c := &actCtx{
        targets: []selectionTarget{empty},
        sel:     empty.id,
        updateDisabled: func(string, int, string, bool) (disabledState, error) {
            called = true
            return disabledState{}, nil
        },
    }
    update, err := actToggleDisabled(c)
    if err != nil || update != nil || called {
        t.Fatalf("empty target = (%#v, %v), called=%v", update, err, called)
    }

    target := sessionSelectionTarget(Session{PID: 1, SessionID: "one"})
    c = &actCtx{
        targets: []selectionTarget{target},
        sel:     target.id,
        updateDisabled: func(string, int, string, bool) (disabledState, error) {
            return disabledState{}, errors.New("server unavailable")
        },
    }
    update, err = actToggleDisabled(c)
    if update != nil || err == nil || err.Error() != "server unavailable" {
        t.Fatalf("failed update = (%#v, %v)", update, err)
    }

    missingID := sessionSelectionTarget(Session{PID: 2})
    called = false
    c = &actCtx{
        targets: []selectionTarget{missingID},
        sel:     missingID.id,
        updateDisabled: func(string, int, string, bool) (disabledState, error) {
            called = true
            return disabledState{}, nil
        },
    }
    update, err = actToggleDisabled(c)
    if update != nil || err == nil ||
        err.Error() != "PID 2 has no stable session ID" || called {
        t.Fatalf("missing-ID update = (%#v, %v), called=%v", update, err, called)
    }
}
```

Add `errors` to `actions_test.go` imports.

- [ ] **Step 2: Write failing viewport-anchor and footer/help tests**

Append to `tui_state_test.go`:

```go
func TestRequestSelectionAnchorRevealsToggledRowAfterSortMove(t *testing.T) {
    state := newTUIState()
    state.sel = "2"
    state.listOffset = 0

    frame := tableFrame{
        lines: []string{"header", "one", "three", "two", ""},
        rows: []tableRow{
            {line: 1, targetID: "1"},
            {line: 2, targetID: "3"},
            {line: 3, targetID: "2"},
        },
    }
    state.requestSelectionAnchor()
    state.resolveListOffset(frame, 2)

    line := frame.targetLine(state.sel)
    if line < state.listOffset || line >= state.listOffset+2 {
        t.Fatalf(
            "selected line %d not visible in offset %d viewport",
            line,
            state.listOffset,
        )
    }
}
```

Append to `tui_test.go` (the file already imports `strings`):

```go
func TestSessionDisableFooterAndHelp(t *testing.T) {
    footer := sessionFooter()
    if !strings.Contains(footer, "d disable/enable") {
        t.Fatalf("footer = %q", footer)
    }
    if bottom := sessionBottomRow("sort: status", false); bottom != footer {
        t.Fatalf("normal bottom row = %q, want footer %q", bottom, footer)
    }
    toast := sessionBottomRow("sort: status", true)
    if !strings.Contains(toast, "sort: status") ||
        strings.Contains(toast, "d disable/enable") {
        t.Fatalf("toast bottom row = %q", toast)
    }

    help := renderHelp("dir")
    for _, want := range []string{
        "d            disable / enable session",
        "claude-sessions preview PID",
        "claude-sessions tmux-info PID",
        "claude-sessions attach PID",
        "press any key to return",
    } {
        if !strings.Contains(help, want) {
            t.Fatalf("help missing %q:\n%s", want, help)
        }
    }
    if strings.Contains(help, "\x1b[H") ||
        strings.Contains(help, "\x1b[J") ||
        strings.Contains(help, "\x1b[2J") {
        t.Fatalf("help contains terminal positioning or clear: %q", help)
    }
}
```

- [ ] **Step 3: Run tests and verify expected failure**

```bash
go test ./... -run 'TestActToggleDisabled|TestRequestSelectionAnchor|TestSessionDisableFooterAndHelp'
```

Expected: compile failure because action, viewport-anchor, footer, and help interfaces do not exist.

- [ ] **Step 4: Add action result and injectable transport**

Add to `actions.go`:

```go
type disabledUpdate struct {
    Host      string
    SessionID string
    Disabled  bool
}
```

Add field to `actCtx`:

```go
updateDisabled func(
    host string,
    pid int,
    sessionID string,
    disabled bool,
) (disabledState, error)
```

Add action:

```go
func actToggleDisabled(c *actCtx) (*disabledUpdate, error) {
    session := c.selected()
    if session == nil {
        return nil, nil
    }
    if session.SessionID == "" {
        return nil, fmt.Errorf("PID %d has no stable session ID", session.PID)
    }
    update := c.updateDisabled
    if update == nil {
        update = setSessionDisabled
    }
    state, err := update(
        session.Host,
        session.PID,
        session.SessionID,
        !session.Disabled,
    )
    if err != nil {
        return nil, err
    }
    return &disabledUpdate{
        Host:      session.Host,
        SessionID: state.SessionID,
        Disabled:  state.Disabled,
    }, nil
}

func showActionError(c *actCtx, label string, err error) {
    c.prepareLineOutput()
    fmt.Printf("\n%s: %v\n", label, err)
    pauseForKey(c.fd, c.oldState)
    c.enterRaw()
}
```

- [ ] **Step 5: Add explicit viewport anchoring, then split refresh from settlement**

Add to `tui_state.go` beside `settleSelection` and `navigate`:

```go
func (s *tuiState) requestSelectionAnchor() {
    s.anchorSelection = true
}
```

This remains separate from `settleSelection`: ordinary polling preserves free-scroll offset, while a successful `d` action explicitly asks the next render to reveal the row that just moved.

In `RunTUI`, define this closure before `refresh`:

```go
settleRows := func() {
    SortSessions(local, sortMode)
    remotes = sortRemotes(hub.Snapshot(), sortMode)
    targets = buildSelectionTargets(local, remotes)
    state.settleSelection(targets)
}
```

Change `refresh` collection and tail to:

```go
refresh := func(kickRemote bool) {
    if sessions, err := collectClientLocal(); err == nil {
        local = sessions
    }
    if kickRemote {
        hub.Refresh()
    }
    settleRows()
}
```

Keep existing comments about copied remote slices and pending spawn selection, updating wording to describe `settleRows` rather than duplicating logic.

- [ ] **Step 6: Add `d` dispatch with immediate patch and no failed mutation**

Insert beside `k`/`a` handling:

```go
case "d", "D":
    ctx := makeCtx()
    update, err := actToggleDisabled(ctx)
    if err != nil {
        screen.Invalidate()
        showActionError(ctx, "disable toggle failed", err)
        render()
        continue
    }
    if update == nil {
        continue
    }
    if update.Host == "" {
        patchDisabledBySessionID(local, update.SessionID, update.Disabled)
    } else {
        hub.PatchDisabled(update.Host, update.SessionID, update.Disabled)
        hub.Refresh()
    }
    settleRows()
    state.requestSelectionAnchor()
    render()
```

This branch does not call `refresh`, avoiding an unnecessary local network round-trip and preserving successful immediate state. `settleRows` reorders rows and retains selection by existing `Session.ID()`; explicit anchoring ensures moved row stays visible in current viewport. On failure, invalidate `screenRenderer` before line-oriented output; otherwise its cached frame could suppress repaint after returning to raw mode.

- [ ] **Step 7: Add persistent footer and pure-content help entry**

Add near `renderHelp`:

```go
func sessionFooter() string {
    return dim("d disable/enable  ·  ? help")
}

func sessionBottomRow(toast string, toastActive bool) string {
    if toastActive {
        return bold(toast)
    }
    return sessionFooter()
}
```

Keep `renderHelp(sortMode string) string` as a pure string-producing function. Add only this line in its `ACTIONS` section after the existing `n` line:

```go
fmt.Fprintln(&b, "    d            disable / enable session")
```

Do not add terminal-clearing or cursor-positioning escapes to `renderHelp`; the existing help modal renders its returned content through `screenRenderer`.

In session-list rendering, reserve one bottom row whenever terminal height is known, not only while a toast is active:

```go
toastActive := rows > 0 && time.Now().Before(toastUntil)
viewRows := rows
if rows > 0 {
    viewRows--
}
if viewRows < 0 {
    viewRows = 0
}
```

After cropped table output is built, let toast temporarily replace footer and keep all clipping/padding inside `screenRenderer`:

```go
if rows > 0 {
    out = withBottomRow(
        out,
        rows,
        sessionBottomRow(toast, toastActive),
    )
}
_ = screen.Draw(out, cols, rows)
```

Update the render comment to say the list always reserves bottom row for footer or active toast. Do not append manual `\033[%d;1H` escapes; that bypasses incremental frame diffing.

- [ ] **Step 8: Format, then run focused TUI/action/client tests**

```bash
gofmt -w actions.go actions_test.go tui.go tui_test.go tui_state.go tui_state_test.go
go test ./... -run 'TestActToggleDisabled|TestSessionDisableFooterAndHelp|TestCollectClientLocal|TestSettleSelectionKeepsToggledSession|TestRequestSelectionAnchor'
```

Expected: PASS; request includes selected `SessionID`, failure leaves snapshots unchanged, selection follows moved row, and explicit anchor keeps it visible.

- [ ] **Step 9: Commit**

```bash
git add actions.go actions_test.go tui.go tui_test.go \
        tui_state.go tui_state_test.go
git commit -m "feat: toggle disabled sessions from TUI"
```

---

### Task 6: Amber Rail and Muted Rows in All Render Modes

**Files:**
- Modify: `render.go:67-82, 729-850, 856-952, 982-1072`
- Test: `render_test.go:90-342, 1131-1149`

**Interfaces:**
- Consumes: `Session.Disabled`, `tmuxViewerPrefix`, `highlightSelectedRow`, existing headless/plain-cell behavior, selected-row background reapplication.
- Produces: fixed two-cell rail prefix (`"  "` enabled, amber `"│ "` disabled); muted unselected disabled viewer/body; selected disabled row retains `ansiSelectedBG`, foreground status colors, and amber rail.

- [ ] **Step 1: Write failing row-decoration tests**

Append to `render_test.go`:

```go
func TestDisabledRowsRenderAmberRailAndMutedBodyAcrossModes(t *testing.T) {
    now := time.Now().UnixMilli()
    attached := 2
    enabled := Session{
        PID: 41, SessionID: "enabled", Name: "enabled", NameSource: "user",
        CWD: "/work/enabled", Status: "busy", Entrypoint: "cli", UpdatedAt: now,
        Tmux: "enabled:0.0", TmuxAttached: &attached,
    }
    disabled := enabled
    disabled.PID = 42
    disabled.SessionID = "disabled"
    disabled.Name = "disabled"
    disabled.CWD = "/work/disabled"
    disabled.Disabled = true

    for _, mode := range []string{"1", "2", "3"} {
        t.Run(mode, func(t *testing.T) {
            enabledRow := renderSessionRowForTest(t, mode, enabled, false)
            disabledRow := renderSessionRowForTest(t, mode, disabled, false)

            if !strings.HasPrefix(stripANSI(disabledRow), "2 │ ") {
                t.Fatalf("mode %s disabled visible prefix = %q", mode, stripANSI(disabledRow))
            }
            if !strings.Contains(
                disabledRow,
                colorize("33", "│")+" "+ansiDim,
            ) {
                t.Fatalf("mode %s rail is not outside muted body: %q", mode, disabledRow)
            }
            if !strings.HasPrefix(disabledRow, ansiDim+"2 "+ansiReset) {
                t.Fatalf("mode %s viewer prefix is not muted: %q", mode, disabledRow)
            }
            if strings.Contains(enabledRow, "│") {
                t.Fatalf("mode %s enabled row contains rail: %q", mode, enabledRow)
            }
            if visualLen(enabledRow) != visualLen(disabledRow) {
                t.Fatalf(
                    "mode %s widths differ: enabled=%d disabled=%d",
                    mode,
                    visualLen(enabledRow),
                    visualLen(disabledRow),
                )
            }
        })
    }
}

func TestDisabledRailAddsFixedHeaderColumnAcrossModes(t *testing.T) {
    session := Session{
        PID: 42, SessionID: "one", Name: "one", NameSource: "user",
        CWD: "/work/one", Status: "idle",
    }
    cases := []struct {
        mode       string
        marker     string
        wantPrefix string
    }{
        {"1", "PID", "        PID"},
        {"2", "DIR▲", "    DIR▲  NAME"},
        {"3", "NAME", "    NAME"},
    }
    for _, tc := range cases {
        t.Run(tc.mode, func(t *testing.T) {
            var output strings.Builder
            RenderAll(
                &output,
                tc.mode,
                testLocalHost(session),
                nil,
                "",
                nil,
                0,
                0,
                "dir",
            )
            header := findRow(t, output.String(), tc.marker)
            if !strings.HasPrefix(header, tc.wantPrefix) {
                t.Fatalf(
                    "mode %s header = %q, want prefix %q",
                    tc.mode,
                    header,
                    tc.wantPrefix,
                )
            }
        })
    }
}

func TestHeadlessDisabledRowKeepsAmberRail(t *testing.T) {
    attached := 1
    session := Session{
        PID: 42, SessionID: "headless-disabled", Name: "headless",
        NameSource: "user", CWD: "/work/headless", Status: "busy",
        Entrypoint: "sdk-cli", Tmux: "headless:0.0",
        TmuxAttached: &attached, Disabled: true,
    }
    for _, mode := range []string{"1", "2", "3"} {
        row := renderSessionRowForTest(t, mode, session, false)
        if !strings.Contains(row, colorize("33", "│")+" ") ||
            !strings.Contains(row, ansiDim) {
            t.Fatalf(
                "mode %s headless disabled row lost rail or dim: %q",
                mode,
                row,
            )
        }
    }
}

func TestSelectedDisabledRowsKeepBackgroundColorsAndAmberRail(t *testing.T) {
    now := time.Now().UnixMilli()
    attached := 2
    for _, mode := range []string{"1", "2", "3"} {
        session := Session{
            PID: 42, SessionID: "disabled", Name: "disabled",
            NameSource: "user", CWD: "/work/disabled",
            Model: "claude-opus-4-8", Status: "busy", Entrypoint: "cli",
            UpdatedAt: now, Tmux: "disabled:0.0", TmuxAttached: &attached,
            Version: "1.2.3", CostUSD: 0.25, ContextTokens: 1000,
            Disabled: true,
        }
        row := renderSessionRowForTest(t, mode, session, true)
        assertWholeRowSelected(t, row, "2 │ ")
        if !strings.Contains(row, "\033[33m│\033[39m ") {
            t.Fatalf(
                "mode %s selected disabled row lacks amber rail: %q",
                mode,
                row,
            )
        }
        if !strings.Contains(row, "\033[1;31m") {
            t.Fatalf(
                "mode %s selected disabled row lost status color: %q",
                mode,
                row,
            )
        }
        if !strings.Contains(row, ansiReset+ansiSelectedBG) {
            t.Fatalf(
                "mode %s selected background is not restored after reset: %q",
                mode,
                row,
            )
        }
    }
}

func TestDisabledRailPreservesViewerPrefixWidth(t *testing.T) {
    attached := 3
    session := Session{
        Tmux: "dev:0.0", TmuxAttached: &attached, Disabled: true,
    }
    got := visualLen(
        tmuxViewerPrefix(session, true) + disabledRail(session, false),
    )
    if got != 4 {
        t.Fatalf("viewer + disabled rail width = %d, want 4", got)
    }
}
```

- [ ] **Step 2: Run renderer tests and verify expected failure**

```bash
go test ./... -run 'TestDisabledRows|TestSelectedDisabledRows|TestDisabledRail'
```

Expected: compile failure because `disabledRail` does not exist; rendered rows lack rail.

- [ ] **Step 3: Add shared plain/decorate helpers**

Add after `highlightSelectedRow`:

```go
func disabledRail(session Session, selected bool) string {
    if !session.Disabled {
        return "  "
    }
    if selected {
        return "\033[33m│\033[39m "
    }
    return colorize("33", "│") + " "
}

func sessionRowPlain(session Session, selected bool) bool {
    return session.Headless() || (session.Disabled && !selected)
}

func decorateSessionRow(session Session, selected bool, body string) string {
    plain := sessionRowPlain(session, selected)
    viewer := tmuxViewerPrefix(session, plain)
    rail := disabledRail(session, selected)

    var row string
    switch {
    case selected:
        row = viewer + rail + body
    case session.Disabled:
        row = dim(viewer) + rail + dim(body)
    case session.Headless():
        row = dim(viewer + rail + body)
    default:
        row = viewer + rail + body
    }
    return highlightSelectedRow(row, selected)
}
```

`selected` must not make normal rows plain: current selected rows preserve status/name foreground colors while `highlightSelectedRow` reapplies `ansiSelectedBG` after nested resets. `SGR 39` restores only foreground after selected amber rail and leaves selected background active. Unselected disabled rows suppress per-cell colors before separately dimming viewer and body, keeping amber rail bright. Existing unselected headless rows retain one continuous dim wrapper.

- [ ] **Step 4: Update full, intermediate, and minimal rows**

Replace full-view `buildHdr` and `rowFn` with:

```go
buildHdr := func() string {
    return fmt.Sprintf(
        "    %7s  %-*s  %-*s  %-*s  %-*s  %*s  %*s  %5s  %-*s  %5s  %5s  %-8s  %s ",
        "PID", nameW, "NAME", dirW, dirLabel, modelW, "MODEL",
        statusW, statusLabel, costW, "COST", agentsW, "AGENTS",
        "CTX", tmuxW, "TMUX", "CPU%", ageLabel, "VER", "SID",
    )
}

rowFn := func(rows []drowFull) {
    for _, r := range rows {
        selected := r.s.ID() == sel
        plainCells := sessionRowPlain(r.s, selected)

        tmuxStr := r.s.Tmux
        if tmuxStr == "" {
            tmuxStr = "-"
        }
        tmuxCell := fmt.Sprintf("%-*s", tmuxW, tmuxStr)
        if r.s.Tmux == "" && !plainCells {
            tmuxCell = dim(tmuxCell)
        }
        statusCell := fmt.Sprintf("%-*s", statusW, r.statusStr)
        if !plainCells {
            statusCell = colorize(statusColor[r.s.Status], statusCell)
        }
        nameCell := fmt.Sprintf("%-*s", nameW, r.nameStr)
        if r.nameDim && !plainCells {
            nameCell = dim(nameCell)
        }
        if utf8.RuneCountInString(r.cwdStr) > dirW {
            overflowing = true
        }
        sidCell := r.sidShort
        if sidCell == "" {
            sidCell = "-"
            if !plainCells {
                sidCell = dim(sidCell)
            }
        }
        body := fmt.Sprintf(
            "%7d  %s  %s  %s  %s  %s  %*s  %s  %s  %5s  %5s  %-8s  %s ",
            r.s.PID,
            nameCell,
            marqueeCell(r.cwdStr, dirW, step),
            modelCell(r.modelStr, modelW, plainCells),
            statusCell,
            costCell(r.costStr, costW),
            agentsW, r.agentsStr,
            ctxCell(r.ctxStr, r.s.ContextTokens, plainCells),
            tmuxCell,
            r.s.CPU, r.ageStr, r.s.Version, sidCell,
        )
        row := decorateSessionRow(r.s, selected, body)
        w.record(r.s.ID(), true)
        fmt.Fprintln(w, row)
    }
}
```

Replace intermediate-view `buildHdr` and `rowFn` with:

```go
buildHdr := func() string {
    return fmt.Sprintf(
        "    %-*s  %-*s  %-*s  %-*s  %*s  %*s  %5s  %5s  %5s ",
        nameW, "NAME", dirW, dirLabel, statusW, statusLabel,
        modelW, "MODEL", costW, "COST", agentsW, "AGENTS",
        "CTX", "CPU%", ageLabel,
    )
}

rowFn := func(rows []drowFull) {
    for _, r := range rows {
        selected := r.s.ID() == sel
        plainCells := sessionRowPlain(r.s, selected)

        statusCell := fmt.Sprintf("%-*s", statusW, r.statusStr)
        if !plainCells {
            statusCell = colorize(statusColor[r.s.Status], statusCell)
        }
        nameCell := fmt.Sprintf("%-*s", nameW, r.nameStr)
        if r.nameDim && !plainCells {
            nameCell = dim(nameCell)
        }
        if utf8.RuneCountInString(r.cwdStr) > dirW {
            overflowing = true
        }
        body := fmt.Sprintf(
            "%s  %s  %s  %s  %s  %*s  %s  %5s  %5s ",
            nameCell,
            marqueeCell(r.cwdStr, dirW, step),
            statusCell,
            modelCell(r.modelStr, modelW, plainCells),
            costCell(r.costStr, costW),
            agentsW, r.agentsStr,
            ctxCell(r.ctxStr, r.s.ContextTokens, plainCells),
            r.s.CPU, r.ageStr,
        )
        row := decorateSessionRow(r.s, selected, body)
        w.record(r.s.ID(), true)
        fmt.Fprintln(w, row)
    }
}
```

Replace minimal-view `buildHdr` and `rowFn` with:

```go
buildHdr := func() string {
    return fmt.Sprintf(
        "    %-*s  %-*s  %-*s  %5s ",
        dirW, dirLabel, nameW, "NAME", statusW, statusLabel, ageLabel,
    )
}

rowFn := func(rows []drowMinimal) {
    for _, r := range rows {
        selected := r.s.ID() == sel
        plainCells := sessionRowPlain(r.s, selected)

        glyph := statusGlyph[r.s.Status]
        if glyph == "" {
            glyph = "?"
        }
        statusCell := glyph + strings.Repeat(" ", statusW-1)
        if !plainCells {
            statusCell = colorize(statusColor[r.s.Status], statusCell)
        }
        nameCell := fmt.Sprintf("%-*s", nameW, r.display)
        if r.nameDim && !plainCells {
            nameCell = dim(nameCell)
        }
        if utf8.RuneCountInString(r.dir) > dirW {
            overflowing = true
        }
        body := fmt.Sprintf(
            "%s  %s  %s  %5s ",
            marqueeCell(r.dir, dirW, step),
            nameCell,
            statusCell,
            r.ageStr,
        )
        row := decorateSessionRow(r.s, selected, body)
        w.record(r.s.ID(), true)
        fmt.Fprintln(w, row)
    }
}
```

Preserve existing one-space right padding on every header and session row, full-view empty-SID `"-"` fallback, intermediate `NAME / DIR / STATUS / MODEL / COST` order, selected foreground colors, `w.record`, target IDs, empty-host rows, remote error/loading rows, and table-frame line accounting. Preserve current `f824858` host heading CPU/MEM/LOAD column alignment, weight, and load-severity colors unchanged; disabled rail changes session table headers/rows only.

- [ ] **Step 5: Format, then run all render and selection tests**

```bash
gofmt -w render.go render_test.go
go test ./... -run 'TestTmuxViewer|TestSelected|TestHeadless|TestDisabled|TestSessionRowsHaveOneRightPaddingSpace|TestFullRowWithEmptySessionID|TestTableHeadersHaveOneRightPaddingSpace|TestIntermediateStatusPrecedesModel|TestRenderAllMatchesBuildTableFrame|TestEmpty.*Selection|TestHostUsageHeadingsAllViews|TestFormatHostLoad'
```

Expected: PASS. Viewer prefix and disabled rail each remain two cells; selected rows use `ansiSelectedBG` rather than reverse video and retain status colors; headers/rows keep one trailing space; full empty SID stays `"-"`; intermediate status remains before model; host resource headings remain unchanged; frame output remains byte-identical between rendering entry points.

- [ ] **Step 6: Commit**

```bash
git add render.go render_test.go
git commit -m "feat: mark disabled sessions in TUI"
```

---

### Task 7: Full Verification and Manual Acceptance

**Files:**
- Verify all modified files
- No planned source changes

**Interfaces:**
- Consumes: complete feature.
- Produces: verified branch ready for review and delivery.

- [ ] **Step 1: Run complete automated suite**

```bash
go test ./...
go test -race ./...
go vet ./...
go build .
```

Expected: all commands exit 0; race detector reports no races.

- [ ] **Step 2: Check formatting and whitespace without mutating files**

```bash
test -z "$(gofmt -l session.go session_test.go server.go server_test.go \
  server_client.go server_client_test.go remote.go remote_test.go \
  remote_actions.go actions.go actions_test.go tui.go tui_test.go \
  tui_state.go tui_state_test.go render.go render_test.go)"
git diff --check
```

Expected: both commands print nothing and exit 0. Each task already ran `gofmt -w` before its commit; any failure here belongs in owning task commit, followed by full Step 1 rerun.

- [ ] **Step 3: Inspect final diff against approved spec**

```bash
git diff main...HEAD --stat
git diff main...HEAD -- session.go server.go server_client.go remote_actions.go \
  remote.go actions.go tui.go tui_state.go render.go
```

Verify:

- Registry key is non-empty `SessionID`; direct session-file decode always resets `Disabled` to `false`.
- Route is exactly `PUT /sessions/{pid}/disabled`.
- PUT requires explicit `disabled` and expected `sessionId`, rejects trailing JSON, and returns authoritative `disabled` plus resolved `sessionId`.
- PID reuse or missing stable identity returns `409` without registry mutation.
- Only successful writes invalidate `/sessions` cache.
- Collection errors never prune disabled state; stale-generation collections never prune newer writes.
- Local listing prefers `127.0.0.1:8765` with exactly `750ms` timeout, then falls back directly.
- Failed toggle does not patch local or remote snapshot.
- Remote stale polls cannot overwrite pending state.
- Disabled rows remain actionable; selected row ID is unchanged and moved row is re-anchored into viewport.
- Enabled partition stays above disabled partition for every sort direction.
- Viewer-count prefix remains intact; selected row keeps one continuous `ansiSelectedBG` span with status/name foreground colors preserved.
- Existing host CPU/MEM/LOAD heading alignment and severity styling remain unchanged.
- Footer and help both expose `d disable/enable`.

- [ ] **Step 4: Manual local-server/TUI acceptance**

Terminal 1:

```bash
go run . --server --bind 0.0.0.0
```

Terminal 2:

```bash
go run .
```

Expected:

1. Press `d` on a local session: row moves into disabled partition, amber `│` appears, unselected row text is muted, selection stays on same session.
2. Press `d` again: row returns to enabled partition and rail disappears.
3. Attach/preview/migrate/kill remain available for disabled row.
4. Open second TUI client: both clients converge on same disabled state after polling.
5. Stop server: TUI still lists direct local sessions as enabled; pressing `d` reports local server unavailable and leaves row unchanged.
6. Restart server: disabled registry is empty by design.

- [ ] **Step 5: Review commit sequence and status**

```bash
git log --oneline main..HEAD
git status --short
```

Expected commit order:

```text
feat: mark disabled sessions in TUI
feat: toggle disabled sessions from TUI
feat: reconcile remote disabled toggles
feat: add disabled session client transport
feat: store disabled session state on server
feat: sort disabled sessions last
docs: plan session disable toggle
```

Expected status: clean. Do not push, merge, or install until user requests delivery.
