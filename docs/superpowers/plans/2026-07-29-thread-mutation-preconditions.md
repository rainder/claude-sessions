# Thread Mutation Preconditions Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close the real TOCTOU/duplicate-spawn gap in kill, migrate, and spawn by having every call site supply the precondition fields the server (and, for local, a new in-process equivalent) already knows how to use but that no caller currently sends.

**Architecture:** Two new small helpers in `migrate.go` — `localReattest(pid int, wantSessionID string) error` (an in-process mirror of the server's `reattest`, server.go:618) and `newSpawnRequestID() string` (a properly-sized random ID, since `randomSlug()`'s 6 hex chars are too short for `validSpawnRequestID`'s 8-char minimum). `localReattest` is called immediately before `KillSession`/`MigrateLocal` at all four local call sites. Remote kill/migrate start sending `session_id` in their JSON bodies instead of `{}`; remote spawn starts sending a `request_id` from the new helper. No server-side changes — every field this plan sends is already decoded and acted on by the server, just never supplied until now.

**Tech Stack:** Go stdlib (`crypto/rand`, `encoding/hex`, `encoding/json`) — no new dependencies.

## Global Constraints

- `go build ./...`, `go vet ./...`, `go test ./...`, `go test -race ./...` must stay green after every task.
- No server-side changes anywhere in this plan — `sessionIDPrecondition`, `reattest`, `resolveLivePID`, `spawnSession`'s `request_id` handling are all pre-existing and untouched.
- An empty `SessionID` on the caller's snapshot skips the precondition (local and remote) — matches `sessionIDPrecondition`'s own existing "absent means no precondition" contract; this plan does not introduce a new empty-ID policy.
- `disableSession`/`postDisableRemote`/`actToggleDisabled` are out of scope — already correct (mandatory `session_id`) — no changes, no new tests.
- `cmdNewLocal`/`actNew`'s local (non-remote) spawn branch is out of scope — a single synchronous in-process call has no retry to dedup against.

---

### Task 1: `localReattest` helper

**Files:**
- Modify: `migrate.go`
- Test: `migrate_test.go`

**Interfaces:**
- Consumes: `readSessionByPID` (migrate.go:317, pre-existing).
- Produces: `localReattest(pid int, wantSessionID string) error` — consumed by Task 2.

- [ ] **Step 1: Write the failing test**

`readSessionByPID` (migrate.go:317) reads `~/.claude/sessions/<pid>.json` — filed by **PID**, not session id. The existing `writeSessionFixture` helper (session_test.go:464) names its file `s.SessionID+".json"`, for `CollectLocal`'s glob-everything-in-the-directory reads — wrong shape here (`readSessionByPID` has zero existing test coverage, confirmed via `grep -rn readSessionByPID *_test.go` returning nothing, so there's no PID-named fixture writer to reuse yet). Add a small dedicated one alongside the tests:

```go
func writeSessionFileForPID(t *testing.T, home string, s Session) {
	t.Helper()
	dir := filepath.Join(home, ".claude", "sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir sessions: %v", err)
	}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	path := filepath.Join(dir, strconv.Itoa(s.PID)+".json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
}

func TestLocalReattestMatchingSessionSucceeds(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	writeSessionFileForPID(t, dir, Session{PID: 123, SessionID: "sess-abc"})

	if err := localReattest(123, "sess-abc"); err != nil {
		t.Fatalf("localReattest = %v, want nil", err)
	}
}

func TestLocalReattestMismatchedSessionRefuses(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	writeSessionFileForPID(t, dir, Session{PID: 123, SessionID: "sess-new"})

	err := localReattest(123, "sess-old")
	if err == nil || !strings.Contains(err.Error(), "different session now") {
		t.Fatalf("localReattest = %v, want session-mismatch error", err)
	}
}

func TestLocalReattestGonePIDRefuses(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	// No session file written for PID 999 at all.

	err := localReattest(999, "sess-abc")
	if err == nil || !strings.Contains(err.Error(), "not a live Claude session") {
		t.Fatalf("localReattest = %v, want not-live error", err)
	}
}

func TestLocalReattestEmptyWantSkipsCheck(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	// No session file for PID 999 — if the check ran, it would refuse.

	if err := localReattest(999, ""); err != nil {
		t.Fatalf("localReattest with empty wantSessionID = %v, want nil (skip)", err)
	}
}
```

`migrate_test.go` already imports `"os"`, `"path/filepath"`, `"strings"` — add `"encoding/json"` and `"strconv"` to its import block.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run TestLocalReattest ./...`
Expected: FAIL with "undefined: localReattest".

- [ ] **Step 3: Write minimal implementation**

Add to `migrate.go`, near `readSessionByPID` (migrate.go:317):

```go
// localReattest re-reads the session file for pid immediately before a
// destructive local action (kill/migrate) and compares it against the
// session identity the caller last observed. Mirrors the server's reattest
// (server.go:618) — same two refusal cases, just in-process instead of over
// HTTP: the caller's snapshot can be however old its confirmation dialog
// took to resolve, and the PID can have been recycled to a different
// session in that window. An empty wantSessionID skips the check —
// matches sessionIDPrecondition's own "absent means no precondition"
// contract, not a new policy invented for this path.
func localReattest(pid int, wantSessionID string) error {
	if wantSessionID == "" {
		return nil
	}
	sess, ok := readSessionByPID(pid)
	if !ok {
		return fmt.Errorf("PID %d is not a live Claude session", pid)
	}
	if sess.SessionID != wantSessionID {
		return fmt.Errorf("PID %d is a different session now", pid)
	}
	return nil
}
```

Check `migrate.go`'s existing imports include `fmt` (it almost certainly does, given `randomSlug`'s `fmt.Sprintf` fallback at migrate.go:49) — add it if not.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -run TestLocalReattest ./...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add migrate.go migrate_test.go
git commit -m "feat: add localReattest, an in-process mirror of the server's reattest"
```

---

### Task 2: Wire `localReattest` into the four local call sites

**Files:**
- Modify: `actions.go` (`actKill:165`, `actAttach:235`)
- Modify: `commands.go` (`cmdKill:42`, `cmdMigrate:126`)

**Interfaces:**
- Consumes: `localReattest` (Task 1).
- Produces: no new interfaces — `actKill`, `actAttach`, `cmdKill`, `cmdMigrate` keep their exact signatures.

No unit test for this task: it's a one-line insertion at four call sites, and — as established while designing the previous (rejected) plan — `actKill`/`actAttach` involve `confirmOverlayPreview`, which this codebase has no headless test seam for; `cmdKill`/`cmdMigrate` are untested at this granularity today too (`commands_test.go` only covers pure argument parsing). `localReattest` itself is already fully unit-tested (Task 1). Verified by build/vet plus a manual check that a killed/migrated PID whose session file was swapped out from under it (simulate by editing the session JSON between confirm and execute, or trust the unit tests plus a code read) is refused, not acted on.

- [ ] **Step 1: Confirm the baseline builds**

```bash
go build ./... && go vet ./...
```

- [ ] **Step 2: (no test-fails-first step — see rationale above; proceed to Step 3)**

- [ ] **Step 3: Write minimal implementation**

In `actKill` (actions.go), immediately before the existing `if err := KillSession(*s); err != nil {` line:

```go
	if err := localReattest(s.PID, s.SessionID); err != nil {
		fmt.Printf("\nkill failed: %v\n", err)
		pauseForKey(c.fd, c.oldState)
		c.enterRaw()
		return
	}
```

In `actAttach`'s migrate-first branch (actions.go), immediately before the existing `tname, err := MigrateLocal(s.PID)` line:

```go
	if err := localReattest(s.PID, s.SessionID); err != nil {
		fmt.Printf("\nmigrate failed: %v\n", err)
		pauseForKey(c.fd, c.oldState)
		c.enterRaw()
		return
	}
```

In `cmdKill` (commands.go), immediately before the existing `if err := KillSession(sess); err != nil {` line:

```go
	if err := localReattest(pid, sess.SessionID); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
```

In `cmdMigrate` (commands.go), immediately before the existing `out, err := MigrateLocal(pid)` line:

```go
	if err := localReattest(pid, sess.SessionID); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
```

Read each function in full before editing (actions.go:165-231 for `actKill`, actions.go:235-272 for `actAttach`, commands.go:42-102 for `cmdKill`, commands.go:126-156 for `cmdMigrate`) to confirm the exact variable name in scope at each insertion point (`s` vs `sess`) and that the insertion sits after the confirmation/prompt step and immediately before the destructive call, not before the confirmation (the whole point is to check as late as possible, right before the action).

- [ ] **Step 4: Run tests to verify nothing regressed**

Run: `go test ./... && go build ./... && go vet ./...`
Expected: PASS — existing tests for these four functions (e.g. `TestSessionActionsIgnoreEmptyHostTarget`, actions_test.go:125) are unaffected since they exercise the empty-host early-return path, before this insertion point.

- [ ] **Step 5: Commit**

```bash
git add actions.go commands.go
git commit -m "feat: reattest local session identity before kill/migrate"
```

---

### Task 3: Remote kill/migrate send `session_id`

**Files:**
- Modify: `remote_actions.go` (`actKillRemote:259`, `actAttachRemote:374`'s migrate call)
- Test: `remote_actions_test.go` (already exists — has `TestPostDisableRemoteSendsResolvedIdentity`/`TestPostDisableRemoteSurfacesRefusal` covering `postDisableRemote`, an `httptest.NewServer` + `writeServerYAML` pattern to mirror exactly)

**Interfaces:**
- Consumes: `writeServerYAML` (remote_actions_test.go's existing test helper).
- Produces: `killRemote(host string, pid int, sessionID string) (actionResult, error)`, `migrateRemote(host string, pid int, sessionID string) (actionResult, error)` — extracted from `actKillRemote`/`actAttachRemote`'s inline request-building, mirroring `postDisableRemote`'s existing shape ("Mirrors removeRemoteWorktree's plain-function shape so the network+parse logic is unit-testable without a terminal" — remote_actions.go:334). `actKillRemote`/`actAttachRemote` keep their exact signatures; they now call these instead of inlining.

`actKillRemote`/`actAttachRemote`/`actNewRemote` themselves have zero unit tests today (confirmed: `grep -rn "TestActKillRemote\|TestActAttachRemote\|TestActNewRemote" *_test.go` returns nothing) — they're confirm-dialog-driven, and this codebase has no headless seam for that (documented in Task 2's rationale for the local side). `postDisableRemote` already establishes the pattern for the *testable* part: extract the body-build/request/parse into a plain function, tested directly, called by the modal-driven action. This task applies that same extraction to kill and migrate.

- [ ] **Step 1: Write the failing test**

Add to `remote_actions_test.go`:

```go
func TestKillRemoteSendsSessionIDWhenKnown(t *testing.T) {
	var gotBody []byte
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()

	u, _ := url.Parse(backend.URL)
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeServerYAML(t, home, "box", u.Hostname(), u.Port(), "secret")

	r, err := killRemote("box", 42, "sess-1")
	if err != nil || !r.OK {
		t.Fatalf("killRemote = (%#v, %v)", r, err)
	}
	if string(gotBody) != `{"session_id":"sess-1"}` {
		t.Fatalf("body = %s, want session_id sent", gotBody)
	}
}

func TestKillRemoteOmitsSessionIDWhenUnknown(t *testing.T) {
	var gotBody []byte
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()

	u, _ := url.Parse(backend.URL)
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeServerYAML(t, home, "box", u.Hostname(), u.Port(), "secret")

	if _, err := killRemote("box", 42, ""); err != nil {
		t.Fatalf("killRemote err = %v", err)
	}
	if string(gotBody) != `{}` {
		t.Fatalf("body = %s, want bare {} when session id unknown", gotBody)
	}
}

func TestMigrateRemoteSendsSessionIDWhenKnown(t *testing.T) {
	var gotBody []byte
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"tmux":"cs-1"}`))
	}))
	defer backend.Close()

	u, _ := url.Parse(backend.URL)
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeServerYAML(t, home, "box", u.Hostname(), u.Port(), "secret")

	r, err := migrateRemote("box", 7, "sess-2")
	if err != nil || !r.OK || r.Tmux != "cs-1" {
		t.Fatalf("migrateRemote = (%#v, %v)", r, err)
	}
	if string(gotBody) != `{"session_id":"sess-2"}` {
		t.Fatalf("body = %s, want session_id sent", gotBody)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run 'TestKillRemote|TestMigrateRemote' ./...`
Expected: FAIL with "undefined: killRemote" / "undefined: migrateRemote".

- [ ] **Step 3: Write minimal implementation**

Add to `remote_actions.go`, near `postDisableRemote` (remote_actions.go:336):

```go
// killRemote asks host's server to kill pid, sending sessionID as the
// precondition when known so the server's reattest (server.go:618) actually
// fires — an empty sessionID falls back to the bare {} body, matching
// sessionIDPrecondition's own "absent means no precondition" contract.
func killRemote(host string, pid int, sessionID string) (actionResult, error) {
	body := []byte(`{}`)
	if sessionID != "" {
		var err error
		body, err = json.Marshal(map[string]string{"session_id": sessionID})
		if err != nil {
			return actionResult{}, err
		}
	}
	resp, err := remoteRequest(host, fmt.Sprintf("/sessions/%d/kill", pid), "POST", body)
	if err != nil {
		return actionResult{}, err
	}
	var r actionResult
	if err := json.Unmarshal(resp, &r); err != nil {
		return actionResult{}, err
	}
	return r, nil
}

// migrateRemote asks host's server to migrate pid, same sessionID contract
// as killRemote.
func migrateRemote(host string, pid int, sessionID string) (actionResult, error) {
	body := []byte(`{}`)
	if sessionID != "" {
		var err error
		body, err = json.Marshal(map[string]string{"session_id": sessionID})
		if err != nil {
			return actionResult{}, err
		}
	}
	resp, err := remoteRequest(host, fmt.Sprintf("/sessions/%d/migrate", pid), "POST", body)
	if err != nil {
		return actionResult{}, err
	}
	var r actionResult
	if err := json.Unmarshal(resp, &r); err != nil {
		return actionResult{}, err
	}
	return r, nil
}
```

In `actKillRemote`, replace the inline `resp, err := remoteRequest(host, fmt.Sprintf("/sessions/%d/kill", pid), "POST", []byte(`{}`))` + its following `json.Unmarshal(resp, &r)` with a single `r, err := killRemote(host, pid, s.SessionID)`, keeping the rest of the function (confirmation, worktree handling) unchanged.

In `actAttachRemote`'s migrate branch, replace the inline `remoteRequest(host, fmt.Sprintf("/sessions/%d/migrate", pid), "POST", []byte(`{}`))` + its `json.Unmarshal` with `r, err := migrateRemote(host, pid, s.SessionID)`, keeping the rest unchanged (it already reads `r.OK`/`r.Tmux`/`r.Error` from the parsed variable — confirm the local variable name matches, adjust `mresp`/`merr`/`r` naming to fit around the new call rather than renaming unrelated code).

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./... && go build ./... && go vet ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add remote_actions.go remote_actions_test.go
git commit -m "feat: send session_id precondition on remote kill/migrate"
```

---

### Task 4: Remote spawn sends `request_id`

**Files:**
- Modify: `migrate.go` (new `newSpawnRequestID` helper)
- Modify: `remote_actions.go` (`actNewRemote:483`)
- Modify: `commands.go` (`cmdNewRemote:272`)
- Test: `migrate_test.go`

**Interfaces:**
- Produces: `newSpawnRequestID() string`.
- Consumes: `validSpawnRequestID` (server.go:389, pre-existing) — only in the test, to confirm the generated ID satisfies it.

- [ ] **Step 1: Write the failing test**

Add to `migrate_test.go`:

```go
func TestNewSpawnRequestIDSatisfiesServerValidation(t *testing.T) {
	id := newSpawnRequestID()
	if !validSpawnRequestID(id) {
		t.Fatalf("newSpawnRequestID() = %q, fails validSpawnRequestID", id)
	}
	if id2 := newSpawnRequestID(); id2 == id {
		t.Fatalf("two calls returned the same id: %q", id)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run TestNewSpawnRequestID ./...`
Expected: FAIL with "undefined: newSpawnRequestID".

- [ ] **Step 3: Write minimal implementation**

Add to `migrate.go`, near `randomSlug` (migrate.go:45):

```go
// newSpawnRequestID returns a fresh idempotency key for /sessions/new's
// optional request_id, sized to satisfy validSpawnRequestID (server.go:389,
// 8-128 chars of [A-Za-z0-9_-]). randomSlug's 6 hex chars are too short and
// serve a different purpose (a tmux-name suffix) — this gets its own helper
// rather than widening that one's contract.
func newSpawnRequestID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%032x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
```

In `actNewRemote` (remote_actions.go, the body-building for `/sessions/new`), add `"request_id": newSpawnRequestID()` to the existing `map[string]string` literal.

In `cmdNewRemote` (commands.go, the equivalent body-building), add the same field to its `map[string]string` literal.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./... && go build ./... && go vet ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add migrate.go migrate_test.go remote_actions.go commands.go
git commit -m "feat: send request_id on remote spawn for dedup protection"
```

---

### Task 5: Full verification and manual smoke check

**Files:** none (verification only)

- [ ] **Step 1: Full automated verification**

```bash
go build ./...
go vet ./...
go test ./...
go test -race ./...
```

Expected: all green.

- [ ] **Step 2: Manual smoke test — local kill/migrate reattest**

Start a live local session, note its PID and session id (`cat ~/.claude/sessions/<pid>.json`). Open the TUI, select it, press `k` to bring up the kill confirmation — while the dialog is open, in another terminal edit that session file's `session_id` field to a different value (simulating a pane recycle). Confirm the kill. Expected: refused with "PID %d is a different session now", the process is NOT killed. Repeat for migrate (`a` on a non-tmux session) with the same file-swap trick.

- [ ] **Step 3: Manual smoke test — remote kill sends the precondition**

With a remote `-s` server configured in servers.yaml, start a session on that host, select its row in the TUI, kill it normally (no tampering) — expect success as before (this is a regression check: the precondition is now sent, but for a stable target it should never fire). Then, if feasible, repeat the file-swap trick against the remote host's session file to confirm a `session_mismatch` refusal comes back from the server (not from local `localReattest`, which doesn't run for remote rows).

- [ ] **Step 4: Manual smoke test — remote spawn**

Spawn a new session on a configured remote server via the TUI (`n`) and via `claude-sessions new --server <name> --dir ...`; confirm both succeed and the resulting tmux session is created exactly once (no duplicate).

- [ ] **Step 5: Commit** (only if smoke testing surfaced fixes)

```bash
git add -A
git commit -m "fix: address issues found in precondition-threading smoke testing"
```

If smoke testing passed clean, there is nothing to commit for this task.
