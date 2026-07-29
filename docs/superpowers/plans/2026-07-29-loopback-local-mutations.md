# Loopback-Server-Only Local Mutations Implementation Plan

> **SUPERSEDED — do not execute.** See `docs/superpowers/specs/2026-07-29-servers-only-client-design.md`'s rejection note and `2026-07-29-thread-mutation-preconditions-design.md` for what replaced it. The design this plan implements was built on a disproven premise (remote already has the same unguarded gap this plan claimed only local had) and has at least one confirmed defect (Task 4's dead-code deletion breaks `worktree_test.go:348,365`). Kept for the task-decomposition reasoning trail only.

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Route local session mutations (kill/migrate/spawn/disable) through the loopback `-s` daemon's HTTP API instead of calling `KillSession`/`MigrateLocal`/`SpawnNew`/`DisabledStore.SetDisabled` directly from the client, so local mutations get the same `sessionIDPrecondition`/`reattest`/`spawnDedupe` protection remote mutations already have — refusing loudly if the daemon isn't reachable rather than silently falling back to the unguarded direct call.

**Architecture:** Four new plain functions in `local_mutations.go` (`killLocal`, `migrateLocal`, `spawnLocal`, `disableLocal`) build the same JSON request bodies `remote_actions.go` already sends to named remote servers, but target the loopback daemon via `sessionServerConfig("")` + `localServerRequestWithTimeout` (both already exist in `server_client.go` for reads). `actions.go`'s local branches (`actKill`, `actAttach`'s migrate-first case, `actNew`, `actToggleDisabled`) and `commands.go`'s equivalents (`cmdKill`, `cmdMigrate`, `cmdNewLocal`, `cmdAttach`'s migrate-first case) swap their direct calls for these. Local `tmux attach` itself stays a direct client-side call — no HTTP, no SSH — since attaching to a tmux session on your own machine has no server-side state to protect. No new server-side endpoints: every mutation the client needs (`/sessions/{pid}/kill`, `/migrate`, `/disable`, `/sessions/new`) is already registered in `cmdServer`'s mux.

**Tech Stack:** Go stdlib (`net/http`, `encoding/json`), existing `golang.org/x/sys`/`golang.org/x/term` — no new dependencies.

## Global Constraints

- `go build ./...`, `go vet ./...`, `go test ./...`, and `go test -race ./...` must stay green after every task.
- Never hit a real network listener in unit tests. Stub `localServerRequestAttempt`/`localTailscaleIPv4` (the package-level seams already defined in `server_client.go:20-21`), the same pattern `server_client_test.go`'s `stubLocalServerFallback` already uses.
- `actKill`, `actAttach`, `actNew`, `actToggleDisabled` (actions.go) and `cmdKill`, `cmdMigrate`, `cmdNewLocal`, `cmdAttach` (commands.go) keep their existing signatures — `tui.go` and `main.go`'s dispatch call sites do not change.
- Local **mutations** (kill/migrate/spawn/disable) refuse loudly when the loopback daemon is unreachable: no fallback to the direct in-process call. Local **reads** (`collectClientLocal`) are untouched and keep their existing silent fallback — this plan does not touch `collectClientLocal`, `fetchLocalServerSessions`, or their tests.
- A transport-level failure (connection refused, timeout, non-2xx) from a local mutation helper is distinct from a decoded `actionResult{OK:false}` business refusal (`session_mismatch`/`not_live`/etc.): only the former gets the "run `claude-sessions service install`" message; the latter shows the server's own `r.Error` exactly as the remote path already does.

---

### Task 1: `localMutateRequest` helper

**Files:**
- Modify: `server_client.go`
- Test: `server_client_test.go`

**Interfaces:**
- Produces: `localMutateTimeout` (const, `time.Duration`), `localMutateRequest(path, method string, body []byte) ([]byte, error)` — used by every function in Task 2.

- [ ] **Step 1: Write the failing test**

Add to `server_client_test.go`:

```go
func TestLocalMutateRequestUsesLoopbackConfigAndTimeout(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	var gotSrv ServerConfig
	var gotPath, gotMethod string
	var gotBody []byte
	stubLocalServerFallback(t,
		func(_ context.Context, srv ServerConfig, path, method string, body []byte) ([]byte, bool, error) {
			gotSrv, gotPath, gotMethod, gotBody = srv, path, method, body
			return []byte(`{"ok":true}`), true, nil
		},
		func(context.Context) string {
			t.Fatal("Tailscale resolved after loopback success")
			return ""
		},
	)

	got, err := localMutateRequest("/sessions/42/kill", http.MethodPost, []byte(`{}`))
	if err != nil || string(got) != `{"ok":true}` {
		t.Fatalf("localMutateRequest = (%q, %v)", got, err)
	}
	if gotSrv.Host != localServerHost || gotSrv.Port != localServerPort || gotSrv.Token == "" {
		t.Fatalf("request config = %#v", gotSrv)
	}
	if gotPath != "/sessions/42/kill" || gotMethod != http.MethodPost || string(gotBody) != `{}` {
		t.Fatalf("request = (%q, %q, %q)", gotPath, gotMethod, gotBody)
	}
}

func TestLocalMutateRequestPropagatesUnreachableError(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	unreachable := errors.New("connection refused")
	stubLocalServerFallback(t,
		func(context.Context, ServerConfig, string, string, []byte) ([]byte, bool, error) {
			return nil, false, unreachable
		},
		func(context.Context) string { return "" },
	)

	_, err := localMutateRequest("/sessions/42/kill", http.MethodPost, []byte(`{}`))
	if !errors.Is(err, unreachable) {
		t.Fatalf("error = %v, want %v", err, unreachable)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run TestLocalMutateRequest ./...`
Expected: FAIL with "undefined: localMutateRequest" (and `localMutateTimeout` if referenced elsewhere).

- [ ] **Step 3: Write minimal implementation**

Add to `server_client.go`, near the existing `localServerTimeout` const:

```go
// localMutateTimeout bounds a local mutation call (kill/migrate/spawn/
// disable). Longer than localServerTimeout's 750ms — migrate and spawn do
// real work (tmux + process spawn) server-side, not just a cache read —
// matching remoteRequest's 30s default for the equivalent named-server call.
const localMutateTimeout = 30 * time.Second

// localMutateRequest sends a mutation (kill/migrate/spawn/disable) to this
// host's own loopback daemon, reusing the same loopback-then-Tailscale
// fallback fetchLocalServerSessions relies on for reads. Unlike reads, a
// caller here treats any returned error as final — see the "refuse loudly"
// global constraint — there is no direct-call fallback for mutations.
func localMutateRequest(path, method string, body []byte) ([]byte, error) {
	srv, err := sessionServerConfig("")
	if err != nil {
		return nil, err
	}
	return localServerRequestWithTimeout(srv, path, method, body, localMutateTimeout)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -run TestLocalMutateRequest ./...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add server_client.go server_client_test.go
git commit -m "feat: add localMutateRequest for local mutation calls"
```

---

### Task 2: `local_mutations.go` — kill/migrate/spawn/disable helpers

**Files:**
- Create: `local_mutations.go`
- Test: `local_mutations_test.go`

**Interfaces:**
- Consumes: `localMutateRequest` (Task 1), `actionResult` (server.go:42).
- Produces: `killLocal(pid int) (actionResult, error)`, `migrateLocal(pid int) (actionResult, error)`, `spawnLocal(cwd, name, command, prompt string) (actionResult, error)`, `disableLocal(pid int, sessionID string, disabled bool) (actionResult, error)`, `localServerUnreachableError(err error) error` — all consumed by Tasks 3-7.

- [ ] **Step 1: Write the failing test**

Create `local_mutations_test.go`:

```go
package main

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

func TestKillLocalSendsEmptyBodyToKillEndpoint(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	var gotPath, gotMethod string
	var gotBody []byte
	stubLocalServerFallback(t,
		func(_ context.Context, _ ServerConfig, path, method string, body []byte) ([]byte, bool, error) {
			gotPath, gotMethod, gotBody = path, method, body
			return []byte(`{"ok":true}`), true, nil
		},
		func(context.Context) string { return "" },
	)

	r, err := killLocal(42)
	if err != nil || !r.OK {
		t.Fatalf("killLocal = (%#v, %v)", r, err)
	}
	if gotPath != "/sessions/42/kill" || gotMethod != http.MethodPost || string(gotBody) != "{}" {
		t.Fatalf("request = (%q, %q, %q)", gotPath, gotMethod, gotBody)
	}
}

func TestMigrateLocalReturnsTmuxName(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	stubLocalServerFallback(t,
		func(_ context.Context, _ ServerConfig, path, method string, body []byte) ([]byte, bool, error) {
			if path != "/sessions/7/migrate" || method != http.MethodPost {
				t.Fatalf("request = (%q, %q)", path, method)
			}
			return []byte(`{"ok":true,"tmux":"cs-7"}`), true, nil
		},
		func(context.Context) string { return "" },
	)

	r, err := migrateLocal(7)
	if err != nil || !r.OK || r.Tmux != "cs-7" {
		t.Fatalf("migrateLocal = (%#v, %v)", r, err)
	}
}

func TestSpawnLocalSendsCwdCommandAndPrompt(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	var gotBody []byte
	stubLocalServerFallback(t,
		func(_ context.Context, _ ServerConfig, path, method string, body []byte) ([]byte, bool, error) {
			gotBody = body
			if path != "/sessions/new" || method != http.MethodPost {
				t.Fatalf("request = (%q, %q)", path, method)
			}
			return []byte(`{"ok":true,"tmux":"cs-new"}`), true, nil
		},
		func(context.Context) string { return "" },
	)

	r, err := spawnLocal("/work/dir", "", "Claude", "hello")
	if err != nil || !r.OK || r.Tmux != "cs-new" {
		t.Fatalf("spawnLocal = (%#v, %v)", r, err)
	}
	want := `{"command":"Claude","cwd":"/work/dir","name":"","prompt":"hello"}`
	if string(gotBody) != want {
		t.Fatalf("body = %s, want %s", gotBody, want)
	}
}

func TestDisableLocalSendsSessionIDAndDisabled(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	var gotPath string
	var gotBody []byte
	stubLocalServerFallback(t,
		func(_ context.Context, _ ServerConfig, path, method string, body []byte) ([]byte, bool, error) {
			gotPath, gotBody = path, body
			if method != http.MethodPost {
				t.Fatalf("method = %q", method)
			}
			return []byte(`{"ok":true,"session_id":"abc","disabled":true}`), true, nil
		},
		func(context.Context) string { return "" },
	)

	r, err := disableLocal(9, "abc", true)
	if err != nil || !r.OK || r.SessionID != "abc" || r.Disabled == nil || !*r.Disabled {
		t.Fatalf("disableLocal = (%#v, %v)", r, err)
	}
	if gotPath != "/sessions/9/disable" {
		t.Fatalf("path = %q", gotPath)
	}
	want := `{"disabled":true,"session_id":"abc"}`
	if string(gotBody) != want {
		t.Fatalf("body = %s, want %s", gotBody, want)
	}
}

func TestLocalMutationHelpersPropagateUnreachableError(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	unreachable := errors.New("connection refused")
	stubLocalServerFallback(t,
		func(context.Context, ServerConfig, string, string, []byte) ([]byte, bool, error) {
			return nil, false, unreachable
		},
		func(context.Context) string { return "" },
	)

	if _, err := killLocal(1); !errors.Is(err, unreachable) {
		t.Fatalf("killLocal error = %v", err)
	}
	if _, err := migrateLocal(1); !errors.Is(err, unreachable) {
		t.Fatalf("migrateLocal error = %v", err)
	}
	if _, err := spawnLocal("/d", "", "", ""); !errors.Is(err, unreachable) {
		t.Fatalf("spawnLocal error = %v", err)
	}
	if _, err := disableLocal(1, "x", true); !errors.Is(err, unreachable) {
		t.Fatalf("disableLocal error = %v", err)
	}
}

func TestLocalServerUnreachableErrorMessage(t *testing.T) {
	err := localServerUnreachableError(errors.New("connection refused"))
	want := "local server not reachable (connection refused) — run 'claude-sessions service install'"
	if err.Error() != want {
		t.Fatalf("message = %q, want %q", err.Error(), want)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run 'TestKillLocal|TestMigrateLocal|TestSpawnLocal|TestDisableLocal|TestLocalMutationHelpers|TestLocalServerUnreachableError' ./...`
Expected: FAIL with "undefined: killLocal" (etc.) — `local_mutations.go` doesn't exist yet.

- [ ] **Step 3: Write minimal implementation**

Create `local_mutations.go`:

```go
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// Local mutation helpers. Each mirrors the JSON shape remote_actions.go
// already sends to a named remote server (actKillRemote, actAttachRemote's
// migrate call, actNewRemote, postDisableRemote), but targets this host's
// own loopback daemon via localMutateRequest. Centralized here (rather than
// inlined per call site, the way actKillRemote/actNewRemote are) because
// each of these has two real callers — a TUI action in actions.go and a
// scriptable subcommand in commands.go — that must send byte-identical
// requests; duplicating the body-building twice per action is exactly the
// local/remote drift this change exists to close.
//
// Contract: a non-nil error means the request never produced a decoded
// actionResult (transport failure, bad JSON) — callers should wrap it with
// localServerUnreachableError. A nil error with actionResult.OK == false is
// a business refusal (session_mismatch, not_live, ...) and carries its own
// Error string; that is not this host being unreachable.

func killLocal(pid int) (actionResult, error) {
	resp, err := localMutateRequest(fmt.Sprintf("/sessions/%d/kill", pid), http.MethodPost, []byte("{}"))
	if err != nil {
		return actionResult{}, err
	}
	var r actionResult
	if err := json.Unmarshal(resp, &r); err != nil {
		return actionResult{}, err
	}
	return r, nil
}

func migrateLocal(pid int) (actionResult, error) {
	resp, err := localMutateRequest(fmt.Sprintf("/sessions/%d/migrate", pid), http.MethodPost, []byte("{}"))
	if err != nil {
		return actionResult{}, err
	}
	var r actionResult
	if err := json.Unmarshal(resp, &r); err != nil {
		return actionResult{}, err
	}
	return r, nil
}

// spawnLocal matches actNewRemote/cmdNewRemote's body shape: command is a
// preset NAME (resolved server-side against this host's own presets, never
// raw command text), prompt is sent separately and shell-quoted server-side.
func spawnLocal(cwd, name, command, prompt string) (actionResult, error) {
	body, err := json.Marshal(map[string]string{
		"cwd":     cwd,
		"name":    name,
		"command": command,
		"prompt":  prompt,
	})
	if err != nil {
		return actionResult{}, err
	}
	resp, err := localMutateRequest("/sessions/new", http.MethodPost, body)
	if err != nil {
		return actionResult{}, err
	}
	var r actionResult
	if err := json.Unmarshal(resp, &r); err != nil {
		return actionResult{}, err
	}
	return r, nil
}

func disableLocal(pid int, sessionID string, disabled bool) (actionResult, error) {
	body, err := json.Marshal(map[string]any{"session_id": sessionID, "disabled": disabled})
	if err != nil {
		return actionResult{}, err
	}
	resp, err := localMutateRequest(fmt.Sprintf("/sessions/%d/disable", pid), http.MethodPost, body)
	if err != nil {
		return actionResult{}, err
	}
	var r actionResult
	if err := json.Unmarshal(resp, &r); err != nil {
		return actionResult{}, err
	}
	return r, nil
}

// localServerUnreachableError wraps a transport-level failure from one of
// the functions above with a consistent, actionable message. Only for a
// non-nil error from those functions — never for a decoded actionResult
// with OK == false, which already carries its own Error string.
func localServerUnreachableError(err error) error {
	return fmt.Errorf("local server not reachable (%v) — run 'claude-sessions service install'", err)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -run 'TestKillLocal|TestMigrateLocal|TestSpawnLocal|TestDisableLocal|TestLocalMutationHelpers|TestLocalServerUnreachableError' ./...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add local_mutations.go local_mutations_test.go
git commit -m "feat: add killLocal/migrateLocal/spawnLocal/disableLocal helpers"
```

---

### Task 3: `actToggleDisabled` goes through `disableLocal`

**Files:**
- Modify: `actions.go:142-154` (`actToggleDisabled`), `tui.go:340-361` (`makeCtx`)
- Modify (delete field): `actions.go:19-51` (`actCtx.disabled` field)
- Test: `actions_test.go:10-60`

**Interfaces:**
- Consumes: `disableLocal`, `localServerUnreachableError` (Task 2), `showActionError` (actions.go:156, unchanged).
- Produces: `actToggleDisabled(c *actCtx) bool` — unchanged signature, tui.go:561's call site (`actToggleDisabled(makeCtx())`) does not change.

This is the smallest of the four actions to convert (no confirmation dialog, no worktree logic) — good first real conversion. It also retires `actCtx.disabled`: after this task, `c.disabled` (the local `*DisabledStore` field) has no remaining reader — the disable write now happens server-side inside `disableSession`, on the same on-disk store the server and any other local process already share (see `docs/superpowers/specs/2026-07-28-persisted-server-side-disabled-state-design.md` — one file, flock-protected, read fresh on every access). `disabledStore` itself (the `LoadDisabledStore()` instance in `tui.go`) stays — it's still used by `settleRows`'s `disabledStore.Overlay(local)` to overlay disabled state onto the **direct**-`CollectLocal()` fallback path when the server-first `collectClientLocal()` read is unreachable; only `actCtx`'s copy of it goes away.

- [ ] **Step 1: Write the failing test**

Replace `TestActToggleDisabledTogglesStore` and `TestActToggleDisabledIgnoresEmptyHostAndMissingID` in `actions_test.go` (delete the old versions, they assert direct `DisabledStore` writes that no longer happen):

```go
func TestActToggleDisabledSendsRequestAndReportsSuccess(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cases := []struct {
		name        string
		session     Session
		wantDisable bool // the "disabled" value the request should carry
	}{
		{"enable to disabled", Session{PID: 10, SessionID: "local"}, true},
		{"disabled to enabled", Session{PID: 11, SessionID: "local-off", Disabled: true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath string
			var gotBody []byte
			stubLocalServerFallback(t,
				func(_ context.Context, _ ServerConfig, path, method string, body []byte) ([]byte, bool, error) {
					gotPath, gotBody = path, body
					return []byte(`{"ok":true}`), true, nil
				},
				func(context.Context) string { return "" },
			)

			target := sessionSelectionTarget(tc.session)
			c := &actCtx{targets: []selectionTarget{target}, sel: target.id}
			if !actToggleDisabled(c) {
				t.Fatal("actToggleDisabled = false, want true")
			}
			if gotPath != fmt.Sprintf("/sessions/%d/disable", tc.session.PID) {
				t.Fatalf("path = %q", gotPath)
			}
			wantBody := fmt.Sprintf(`{"disabled":%v,"session_id":%q}`, tc.wantDisable, tc.session.SessionID)
			if string(gotBody) != wantBody {
				t.Fatalf("body = %s, want %s", gotBody, wantBody)
			}
		})
	}
}

func TestActToggleDisabledIgnoresEmptyHostAndMissingID(t *testing.T) {
	empty := emptyHostSelectionTarget("orca")
	c := &actCtx{targets: []selectionTarget{empty}, sel: empty.id}
	if actToggleDisabled(c) {
		t.Fatal("empty-host target toggled")
	}

	missingID := sessionSelectionTarget(Session{PID: 2})
	c = &actCtx{targets: []selectionTarget{missingID}, sel: missingID.id}
	if actToggleDisabled(c) {
		t.Fatal("missing-SessionID target toggled")
	}
}

func TestActToggleDisabledReportsUnreachableServer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	unreachable := errors.New("connection refused")
	stubLocalServerFallback(t,
		func(context.Context, ServerConfig, string, string, []byte) ([]byte, bool, error) {
			return nil, false, unreachable
		},
		func(context.Context) string { return "" },
	)

	target := sessionSelectionTarget(Session{PID: 10, SessionID: "local"})
	c := &actCtx{targets: []selectionTarget{target}, sel: target.id, fd: -1}
	if actToggleDisabled(c) {
		t.Fatal("actToggleDisabled = true, want false on unreachable server")
	}
}
```

Add `"context"`, `"errors"`, and `"fmt"` to `actions_test.go`'s import block if not already present (the file currently imports `"bytes"`, `"path/filepath"`, `"strings"`, `"testing"` — `"path/filepath"` becomes unused once `TestActToggleDisabledIgnoresEmptyHostAndMissingID` no longer builds a `DisabledStore` path; remove it if no other test in the file still uses `filepath`).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run TestActToggleDisabled ./...`
Expected: FAIL — `actToggleDisabled` still writes to `c.disabled` directly, so no HTTP request is made and `gotPath`/`gotBody` stay empty.

- [ ] **Step 3: Write minimal implementation**

In `actions.go`, remove the `disabled *DisabledStore` field and its comment from `actCtx` (lines 46-50), then replace `actToggleDisabled`:

```go
// actToggleDisabled flips the disabled flag for the selected session by
// asking this host's own loopback daemon to set it — the same
// sessionIDPrecondition/reattest-protected path a remote row already uses,
// just targeting 127.0.0.1 instead of a named server. A row with no
// selection or no stable SessionID is ignored (it can't be keyed). Reports
// whether anything changed; the caller MUST settleRows()/refresh()
// afterwards to re-overlay the new value onto the live rows — nothing
// in-memory is patched here.
func actToggleDisabled(c *actCtx) bool {
	session := c.selected()
	if session == nil || session.SessionID == "" {
		return false
	}
	if session.Host != "" {
		return actToggleDisabledRemote(c)
	}
	r, err := disableLocal(session.PID, session.SessionID, !session.Disabled)
	if err != nil {
		showActionError(c, "disable", localServerUnreachableError(err))
		return false
	}
	if !r.OK {
		showActionError(c, "disable", fmt.Errorf("%s", r.Error))
		return false
	}
	return true
}
```

In `tui.go`, remove the `disabled: disabledStore,` line from `makeCtx` (tui.go:347) — `disabledStore` itself stays declared and used by `settleRows`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -run TestActToggleDisabled ./... && go build ./... && go vet ./...`
Expected: PASS, clean build (confirms no other reader of `actCtx.disabled` was missed).

- [ ] **Step 5: Commit**

```bash
git add actions.go actions_test.go tui.go
git commit -m "feat: route local disable toggle through loopback daemon"
```

---

### Task 4: `actKill`'s local branch goes through `killLocal`

**Files:**
- Modify: `actions.go:163-231` (`actKill`, `worktreeRemovalQuestion` unchanged)
- Modify (delete dead code): `worktree.go:124-142` (`localWorktreeCleanupTarget`)
- Modify: `commands.go` is NOT touched in this task (Task 7 covers `cmdKill`)

**Interfaces:**
- Consumes: `killLocal` (Task 2), `localServerUnreachableError` (Task 2), `RemoveWorktree` (worktree.go, unchanged — still a direct filesystem call, see below).
- Produces: `actKill(c *actCtx)` — unchanged signature.

`actKill`'s worktree-removal **offer** now comes from the server's `actionResult.Worktree` field instead of a client-side `localWorktreeCleanupTarget(*s)` call made before the kill — this is the behavior change flagged in the design spec: the server decides from its own freshly-collected session list at kill time, which is strictly fresher than the client's pre-kill snapshot. The actual removal, if the user confirms, stays a direct `RemoveWorktree(path)` call (a local `git worktree remove` — no session-identity race to protect, unlike kill/migrate/spawn/disable).

**No new unit test for `actKill` itself in this task.** Checked first: `actKillRemote`/`actAttachRemote`/`actNewRemote` (remote_actions.go) — the functions this task's local branch is converging toward — have **zero** unit tests today (`grep -rn "TestActKillRemote\|TestActAttachRemote\|TestActNewRemote" *_test.go` returns nothing). That's because they call `confirmOverlayPreview`/`confirmOverlay`, which read real terminal input, and this codebase has no input-injection seam for them — `TestSessionActionsIgnoreEmptyHostTarget` (actions_test.go:125-135) only exercises the empty-host early-return path, before any modal opens. Inventing a new terminal-injection harness just for this task would be new test infrastructure beyond this plan's scope, and asymmetric with the remote path it's mirroring. The logic that actually changed — the HTTP request/response handling — is exactly `killLocal`, already fully unit-tested in Task 2. This task is a thin rewire verified by build/vet plus Task 9's manual smoke test, matching how `actKillRemote` itself is verified today.

- [ ] **Step 1: Confirm the baseline builds**

```bash
go build ./... && go vet ./...
```

Expected: clean (confirms the starting point before editing `actKill`).

- [ ] **Step 2: (no test-fails-first step — see rationale above; proceed to Step 3)**

- [ ] **Step 3: Write minimal implementation**

Replace `actKill`'s local branch (actions.go:174-224, everything after the `s.Host != ""` early return) with:

```go
	var question string
	if s.Tmux != "" {
		sessName, err := tmuxSessionName(s.Tmux)
		if err != nil {
			c.prepareLineOutput()
			fmt.Printf("\nkill failed: %v\n", err)
			pauseForKey(c.fd, c.oldState)
			c.enterRaw()
			return
		}
		question = fmt.Sprintf("kill tmux session %q (PID %d)?", sessName, s.PID)
	} else {
		question = fmt.Sprintf("kill PID %d?", s.PID)
	}
	if s.NotIdle() {
		question = colorize(statusColor[s.Status], fmt.Sprintf("⚠ session is %s, not idle — killing will interrupt it", s.StatusDisplay())) + "\n" + question
	}
	pane := startLocalPreview(*s)
	confirmed := confirmOverlayPreview(question, pane, c.modalWakes, s.NotIdle())
	pane.close()
	if !confirmed {
		return
	}
	c.prepareLineOutput()
	defer c.enterRaw()

	fmt.Print("\nsending kill... ")
	r, err := killLocal(s.PID)
	if err != nil {
		fmt.Printf("failed: %v\n", localServerUnreachableError(err))
		pauseForKey(c.fd, c.oldState)
		return
	}
	if !r.OK {
		fmt.Printf("failed: %s\n", r.Error)
		pauseForKey(c.fd, c.oldState)
		return
	}
	if r.Worktree == nil {
		return
	}
	// The server decided this kill emptied a worktree; ask, then remove it
	// directly — a local filesystem op, unlike the kill/migrate/spawn/disable
	// mutations above, has no session-identity race to protect against.
	c.enterRaw()
	if !confirmOverlay(worktreeRemovalQuestion(r.Worktree.Path), c.modalWakes) {
		return
	}
	c.prepareLineOutput()
	fmt.Print("\nremoving worktree... ")
	if err := RemoveWorktree(r.Worktree.Path); err != nil {
		fmt.Printf("\n%v\n", err)
		pauseForKey(c.fd, c.oldState)
	}
```

Note this drops the pre-kill `worktree, _ := localWorktreeCleanupTarget(*s)` call entirely — the offer now comes from `r.Worktree`, populated after the kill succeeds, exactly mirroring `actKillRemote`'s existing shape (remote_actions.go:259-309) almost line for line. `enterRaw`/`defer c.enterRaw()` placement follows `actKillRemote`'s pattern (a single `defer` after the confirmation, since there's no longer an intermediate `enterRaw()` between the kill call and the worktree question — match `actKillRemote` exactly, including its explicit `c.enterRaw()` right before the worktree `confirmOverlay`, which needs raw mode for its own fullscreen modal per the existing comment on that function).

In `worktree.go`, delete `localWorktreeCleanupTarget` (lines 124-142, including its doc comment) — grep first to confirm zero remaining callers:

```bash
grep -rn "localWorktreeCleanupTarget" *.go
```

Expected after this task's actions.go change: only `commands.go:85` remains (removed in Task 7). If Task 7 hasn't landed yet, leave the function in place until it does — do not delete it until both callers are gone. Re-run the grep as this task's last check; only proceed to delete if it now shows zero matches.

- [ ] **Step 4: Run tests to verify nothing regressed**

Run: `go test ./... && go build ./... && go vet ./...`
Expected: PASS — `local_mutations_test.go`'s `TestKillLocal*` (Task 2) still cover the reused logic; this step confirms the rewire didn't break anything else (e.g. `TestSessionActionsIgnoreEmptyHostTarget`, which still calls `actKill` on an empty-host target).

- [ ] **Step 5: Commit**

```bash
git add actions.go worktree.go
git commit -m "feat: route local kill through loopback daemon"
```

---

### Task 5: `actAttach`'s migrate-first case goes through `migrateLocal`

**Files:**
- Modify: `actions.go:233-272` (`actAttach`)

**Interfaces:**
- Consumes: `migrateLocal`, `localServerUnreachableError` (Task 2).
- Produces: `actAttach(c *actCtx)` — unchanged signature. The already-in-tmux case (`s.Tmux != ""`) is untouched — no mutation involved, stays a direct `runTmuxAttach`.

**No new unit test for `actAttach` itself, same rationale as Task 4**: its migrate-first branch calls `confirmOverlayPreview`, which this codebase has no headless test seam for (`actAttachRemote`, the function it mirrors, has zero unit tests today for the same reason). `migrateLocal` is already fully unit-tested (Task 2). Verified by build/vet plus Task 9's manual smoke test.

- [ ] **Step 1: Confirm the baseline builds**

```bash
go build ./... && go vet ./...
```

- [ ] **Step 2: (no test-fails-first step — see rationale above; proceed to Step 3)**

- [ ] **Step 3: Write minimal implementation**

Replace the migration call in `actAttach` (actions.go:260-269):

```go
	c.prepareLineOutput()
	fmt.Printf("\nmigrating PID %d... ", s.PID)
	r, err := migrateLocal(s.PID)
	if err != nil {
		fmt.Printf("\nmigrate failed: %v\n", localServerUnreachableError(err))
		pauseForKey(c.fd, c.oldState)
		c.enterRaw()
		return
	}
	if !r.OK || r.Tmux == "" {
		fmt.Printf("\nmigrate failed: %s\n", r.Error)
		pauseForKey(c.fd, c.oldState)
		c.enterRaw()
		return
	}
	fmt.Printf("ok → %s\n", r.Tmux)
	c.enterRaw()
	runTmuxAttach(c, r.Tmux)
```

This mirrors `actAttachRemote`'s migrate branch (remote_actions.go:404-431) exactly.

- [ ] **Step 4: Run tests to verify nothing regressed**

Run: `go test ./... && go build ./... && go vet ./...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add actions.go
git commit -m "feat: route local migrate-before-attach through loopback daemon"
```

---

### Task 6: `actNew`'s local branch goes through `spawnLocal`

**Files:**
- Modify: `actions.go:284-362` (`actNew`)

**Interfaces:**
- Consumes: `spawnLocal`, `localServerUnreachableError` (Task 2).
- Produces: `actNew(c *actCtx)` — unchanged signature. The picker/preset-loading logic (`LoadCommandPresets`, `buildCwdPicker`, `pickNewSession`) is UI, stays local and unchanged — only the final spawn call changes.

**No new unit test for `actNew` itself, same rationale as Tasks 4-5**: reaching its spawn call requires driving `pickNewSession` (a fullscreen picker modal), and `actNewRemote` — the function this converges toward — has zero unit tests today for the identical reason. `spawnLocal` is already fully unit-tested (Task 2), including the exact body shape (`TestSpawnLocalSendsCwdCommandAndPrompt`). Verified by build/vet plus Task 9's manual smoke test.

- [ ] **Step 1: Confirm the baseline builds**

```bash
go build ./... && go vet ./...
```

- [ ] **Step 2: (no test-fails-first step — see rationale above; proceed to Step 3)**

- [ ] **Step 3: Write minimal implementation**

Extract the post-picker spawn logic into a small pure-ish function so it's testable independent of the picker UI, then call it from `actNew`. Replace the tail of `actNew` (actions.go:341-361, from `command := preset.Command` onward):

```go
	fmt.Printf("\nspawning in %s... ", cwd)
	r, err := spawnLocal(cwd, "", preset.Name, prompt)
	if err != nil {
		fmt.Printf("failed: %v\n", localServerUnreachableError(err))
		pauseForKey(c.fd, c.oldState)
		return
	}
	if !r.OK || r.Tmux == "" {
		fmt.Printf("failed: %s\n", r.Error)
		pauseForKey(c.fd, c.oldState)
		return
	}
	c.spawnedTmux = r.Tmux
	if prompt != "" {
		fmt.Printf("ok → %s (running in background)\n", r.Tmux)
		c.spawnedBackground = true
		go dismissTrustPrompt(r.Tmux)
		return
	}
	fmt.Printf("ok → %s\n", r.Tmux)
	c.enterRaw()
	runTmuxAttach(c, r.Tmux)
```

Note this drops the client-side `command := preset.Command; if prompt != "" { command = command + " " + shellQuote(prompt) }` composition entirely — `spawnLocal` sends `preset.Name` and `prompt` as separate fields, exactly like `actNewRemote` (remote_actions.go:483-560) and `cmdNewRemote` (commands.go:272-327) already do, letting the server (`spawnSession`, server.go:1108) resolve the preset and shell-quote the prompt itself. This also means `shellQuote` may become unused in `actions.go` if this was its only call site there — check `grep -n shellQuote actions.go commands.go` and leave the function in whichever file still calls it (likely `commands.go`, if `cmdNewLocal` also stops composing the command string in Task 7).

- [ ] **Step 4: Run tests to verify nothing regressed**

Run: `go test ./... && go build ./... && go vet ./...`
Expected: PASS — `TestActNewEmptyLocalTargetRoutesLocal` (actions_test.go:74-88) still covers the pre-spawn routing logic untouched by this task.

- [ ] **Step 5: Commit**

```bash
git add actions.go
git commit -m "feat: route local new-session spawn through loopback daemon"
```

---

### Task 7: `commands.go` scriptable subcommands

**Files:**
- Modify: `commands.go` (`cmdKill:42-102`, `cmdMigrate:126-156`, `cmdNewLocal:226-269`, `cmdAttach:362-394`)
- Modify (delete dead code): `worktree.go` (`localWorktreeCleanupTarget`, if Task 4 deferred its removal)

**Interfaces:**
- Consumes: `killLocal`, `migrateLocal`, `spawnLocal`, `localServerUnreachableError` (Task 2).
- Produces: `cmdKill([]string) int`, `cmdMigrate([]string) int`, `cmdNewLocal(newArgs) int`, `cmdAttach([]string) int` — unchanged signatures, `main.go`'s dispatch is untouched.

`commands_test.go` today only tests pure argument-parsing (`TestParseNewArgs`, `TestParseKillFlags`) — `cmdKill`/`cmdMigrate`/`cmdNewLocal`/`cmdAttach` themselves are untested at this level (they mix `confirm()` stdin prompts, `fmt.Println`, and process/tmux introspection that the existing test suite doesn't attempt to harness). This task keeps that convention: the new HTTP-mutation logic these functions call (`killLocal`/`migrateLocal`/`spawnLocal`) is already fully unit-tested in `local_mutations_test.go` (Task 2); these four functions stay thin, verified by `go build`/`go vet` plus the manual smoke test in Task 9, not new stdin/stdout-scraping tests.

- [ ] **Step 1: Confirm the baseline builds**

```bash
go build ./... && go vet ./...
```

- [ ] **Step 2: (no test-fails-first step — see rationale above; proceed to Step 3)**

- [ ] **Step 3: Write minimal implementation**

In `cmdKill` (commands.go:85-99), replace:

```go
	worktree, err := localWorktreeCleanupTarget(sess)
	if err != nil {
		fmt.Fprintf(os.Stderr, "worktree check skipped: %v\n", err)
	}
	if err := KillSession(sess); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if tmuxName != "" {
		fmt.Printf("killed tmux session %s (PID %d)\n", tmuxName, pid)
	} else {
		fmt.Printf("killed PID %d\n", pid)
	}
	if worktree != "" {
		cleanupWorktreeCLI(worktree, flags)
	}
	return 0
```

with:

```go
	r, err := killLocal(pid)
	if err != nil {
		fmt.Fprintln(os.Stderr, localServerUnreachableError(err))
		return 1
	}
	if !r.OK {
		fmt.Fprintln(os.Stderr, r.Error)
		return 1
	}
	if tmuxName != "" {
		fmt.Printf("killed tmux session %s (PID %d)\n", tmuxName, pid)
	} else {
		fmt.Printf("killed PID %d\n", pid)
	}
	if r.Worktree != nil {
		cleanupWorktreeCLI(r.Worktree.Path, flags)
	}
	return 0
```

In `cmdMigrate` (commands.go:149-155), replace:

```go
	out, err := MigrateLocal(pid)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println(out)
	return 0
```

with:

```go
	r, err := migrateLocal(pid)
	if err != nil {
		fmt.Fprintln(os.Stderr, localServerUnreachableError(err))
		return 1
	}
	if !r.OK || r.Tmux == "" {
		fmt.Fprintln(os.Stderr, r.Error)
		return 1
	}
	fmt.Println(r.Tmux)
	return 0
```

In `cmdNewLocal` (commands.go:226-269), replace the preset-resolution and spawn (everything from `presets, err := LoadCommandPresets()` through `fmt.Println(tname)`) with the same shape `cmdNewRemote` already uses — send the raw preset name and let the server resolve it, matching Task 6's `spawnLocal` call exactly:

```go
	r, err := spawnLocal(dir, a.name, a.command, a.prompt)
	if err != nil {
		fmt.Fprintln(os.Stderr, localServerUnreachableError(err))
		return 1
	}
	if !r.OK || r.Tmux == "" {
		fmt.Fprintln(os.Stderr, r.Error)
		return 1
	}
	if a.prompt != "" {
		dismissTrustPrompt(r.Tmux)
	}
	fmt.Println(r.Tmux)
	return 0
```

This drops `cmdNewLocal`'s own `LoadCommandPresets`/`findCommandPreset` resolution and `shellQuote`-based command composition entirely — the server resolves `a.command` against its own presets exactly as it already does for `cmdNewRemote`'s identical body shape (`{"cwd","name","command","prompt"}`). If `findCommandPreset`/`shellQuote` have no remaining callers in `commands.go` after this change, leave them — they're still used elsewhere (`LoadCommandPresetIndex` and `actions.go`'s picker-facing code use `findCommandPreset`; verify with `grep -rn findCommandPreset\|shellQuote *.go` before removing anything, and only remove what's genuinely unreferenced).

`cmdAttach` (commands.go:362-394) has no migrate-first case today — unlike `actAttach`, it errors out with instructions (`"run: claude-sessions migrate", pid`) rather than migrating itself. No change needed here; leave as-is. (Correcting the plan's earlier assumption — re-verify this against the actual function body before editing anything, since this plan was written expecting a migrate branch that `cmdAttach` in the current commands.go does not have.)

Finally, re-run the dead-code check from Task 4:

```bash
grep -rn "localWorktreeCleanupTarget" *.go
```

Expected: zero matches now. Delete `localWorktreeCleanupTarget` from `worktree.go` (if Task 4 left it in place pending this task's removal of its last caller).

- [ ] **Step 4: Run test to verify it passes**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: clean build, all tests pass (no test asserted the old behavior for these four functions, per Step 1's rationale).

- [ ] **Step 5: Commit**

```bash
git add commands.go worktree.go
git commit -m "feat: route scriptable kill/migrate/new subcommands through loopback daemon"
```

---

### Task 8: Consistent read path for scriptable list output

**Files:**
- Modify: `main.go:104-105` (`cmdList`)
- Modify: `commands.go:464` (`cmdListSessions`)

**Interfaces:**
- Consumes: `collectClientLocal()` (server_client.go:119, pre-existing, untouched).
- Produces: no new interfaces — a pure internal swap.

Both call sites currently call `CollectLocal()` directly, while the TUI already calls `collectClientLocal()` (tui.go:237) for the equivalent read — meaning scriptable output and the live TUI can show local sessions from different sources on the same box. `collectClientLocal()` has the identical signature (`() ([]Session, error)`), so this is a pure substitution with no other code changes required. Both places already independently apply `LoadDisabledStore().Overlay(local)` afterward regardless of source — safe to keep as-is, since the client and any local `-s` daemon share the same on-disk `disabled.json` (flock-protected, see the `2026-07-28-persisted-server-side-disabled-state-design.md` history note) — overlaying twice with the same underlying data is idempotent, not a correctness risk.

- [ ] **Step 1: Write the failing test**

This is a one-line-per-site substitution with no independently observable behavior difference in a unit test (both functions return `[]Session`; the only difference is which process does the collecting, which a test can't distinguish without a running loopback server). Skip a dedicated new test; rely on existing `TestCollectClientLocalPrefersServerAndFallsBack` (server_client_test.go:35) already covering `collectClientLocal`'s own contract, and this task's Step 4 full-suite run to confirm nothing else broke.

- [ ] **Step 2: Run test to verify it fails**

N/A — go straight to Step 3.

- [ ] **Step 3: Write minimal implementation**

In `main.go:105`:

```go
	local, err := collectClientLocal()
```

In `commands.go:464`:

```go
	local, err := collectClientLocal()
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go build ./... && go vet ./... && go test ./... && go test -race ./...`
Expected: clean build, all tests pass.

- [ ] **Step 5: Commit**

```bash
git add main.go commands.go
git commit -m "fix: cmdList/cmdListSessions use collectClientLocal for parity with the TUI"
```

---

### Task 9: Full verification and manual smoke check

**Files:** none (verification only)

**Interfaces:** none

- [ ] **Step 1: Full automated verification**

```bash
go build ./...
go vet ./...
go test ./...
go test -race ./...
```

Expected: all green. This is the final gate before merging — every prior task already ran a scoped version of this; this step catches any cross-task interaction (e.g. Task 4's dead-code removal colliding with Task 7's).

- [ ] **Step 2: Manual smoke test — daemon running**

```bash
claude-sessions -s &          # or: claude-sessions service install && service status
sleep 1
claude-sessions new --dir /tmp --command Claude   # or whatever preset is configured
claude-sessions list-sessions
claude-sessions kill <pid-from-above> -y
```

Expected: spawn, list, and kill all succeed and visibly go through the daemon (check the daemon's own stdout/log for the incoming `/sessions/new` and `/sessions/{pid}/kill` requests if it logs them; otherwise confirm behaviorally — a killed session actually disappears from a subsequent `list-sessions`).

- [ ] **Step 3: Manual smoke test — daemon NOT running**

```bash
pkill -f 'claude-sessions -s'   # or service stop, whichever this box uses
claude-sessions kill <some-live-pid> -y
```

Expected: refuses with `local server not reachable (...) — run 'claude-sessions service install'`, exit code 1, and the target session is confirmed still alive afterward (the direct `KillSession` fallback must NOT have fired).

- [ ] **Step 4: TUI smoke test**

Launch the interactive TUI (`claude-sessions`) with the daemon running, exercise kill (`k`), attach to a non-tmux session (`a`), new (`n`), and disable toggle (`-`/`+`) on a local row; confirm each round-trips through the daemon and the row updates correctly after `refresh()`. Repeat with the daemon stopped mid-session to confirm each action shows the unreachable-server message instead of silently mutating.

- [ ] **Step 5: Commit** (only if smoke testing surfaced fixes)

```bash
git add -A
git commit -m "fix: address issues found in loopback-mutation smoke testing"
```

If smoke testing passed clean, there is nothing to commit for this task.
