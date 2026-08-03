# Preview-mode tmux resize Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** When the user opens inspector/preview mode on a session (local or remote), resize that session's tmux window to match the inspector's inner viewport (the local terminal, minus inspector chrome), so preview content wraps at the size the viewer can actually see; revert the resize when the user leaves preview mode.

**Architecture:** One shared primitive pair (`resizeTmuxTarget` / `revertTmuxTarget` in a new `resize.go`, mirroring the existing `send_keys.go` pattern) called two ways: directly for a local target (`resolveLivePIDLocal` → primitive), and over a new authenticated `POST /sessions/{pid}/resize` endpoint for a remote target (mirroring the existing send-keys endpoint exactly: dedicated body decoder, `resolveLivePID`, injectable seam, route registration). `tui.go`'s `openInspector`/`closeInspector` fire the resize/revert once each, non-fatally.

**Tech Stack:** Go stdlib (`os/exec`, `net/http`, `encoding/json`), `golang.org/x/term` (already used for local terminal size). No new dependencies.

## Global Constraints

- Do all implementation work in a git worktree (`.claude/worktrees/<name>`), never directly on `main` (this repo's main checkout is also the live dev environment).
- Before merging: `go build ./...`, `go vet ./...`, and `go test ./...` must all be green.
- Follow existing patterns exactly where one already exists for the same shape of problem (send-keys is the template for nearly every task below) — don't invent a new shape.
- No new dependencies; stdlib + `golang.org/x/term` + `golang.org/x/sys` only.
- `session_id` is **required** on the new `/resize` endpoint (no legacy bare-`{}` compatibility) — this is a brand-new endpoint with no pre-existing caller, same reasoning `sendKeysBody` documents.
- A resize/revert failure must never block entering or leaving preview mode — always best-effort, errors discarded at the `tui.go` call site (preview itself works via `capture-pane` regardless of pane size).

---

### Task 1: tmux resize/revert primitives

**Files:**
- Create: `resize.go`
- Test: `resize_test.go`

**Interfaces:**
- Consumes: `Session` type (existing, `session.go` — has `.PID int`, `.Tmux string`).
- Produces:
  - `resizeTmuxTarget(s Session, cols, rows int) error`
  - `revertTmuxTarget(s Session) error`
  - `resizeSession(s Session, cols, rows int, revert bool) error` — dispatches to the two above; this is the single function later tasks' seams point at.

- [ ] **Step 1: Write the failing tests**

```go
// resize_test.go
package main

import (
	"strings"
	"testing"
)

func TestResizeTmuxTargetEmptyTmuxRefuses(t *testing.T) {
	err := resizeTmuxTarget(Session{PID: 4242}, 120, 40)
	if err == nil || !strings.Contains(err.Error(), "no tmux pane") {
		t.Fatalf("resizeTmuxTarget = %v, want no-tmux-pane error", err)
	}
}

func TestResizeTmuxTargetInvalidSizeRefuses(t *testing.T) {
	err := resizeTmuxTarget(Session{PID: 4242, Tmux: "work:0.0"}, 0, 40)
	if err == nil || !strings.Contains(err.Error(), "invalid size") {
		t.Fatalf("resizeTmuxTarget = %v, want invalid-size error", err)
	}
	err = resizeTmuxTarget(Session{PID: 4242, Tmux: "work:0.0"}, 120, 0)
	if err == nil || !strings.Contains(err.Error(), "invalid size") {
		t.Fatalf("resizeTmuxTarget = %v, want invalid-size error", err)
	}
}

func TestRevertTmuxTargetEmptyTmuxRefuses(t *testing.T) {
	err := revertTmuxTarget(Session{PID: 4242})
	if err == nil || !strings.Contains(err.Error(), "no tmux pane") {
		t.Fatalf("revertTmuxTarget = %v, want no-tmux-pane error", err)
	}
}

func TestResizeSessionDispatchesToRevertTmuxTarget(t *testing.T) {
	// revert=true with an empty Tmux still refuses via revertTmuxTarget's own
	// guard — proves resizeSession(revert=true) reaches revertTmuxTarget, not
	// resizeTmuxTarget (which would additionally complain about cols/rows).
	err := resizeSession(Session{PID: 1}, 0, 0, true)
	if err == nil || !strings.Contains(err.Error(), "no tmux pane") {
		t.Fatalf("resizeSession(revert) = %v, want no-tmux-pane error", err)
	}
}

func TestResizeSessionDispatchesToResizeTmuxTarget(t *testing.T) {
	// revert=false with an empty Tmux refuses via resizeTmuxTarget's own guard.
	err := resizeSession(Session{PID: 1}, 120, 40, false)
	if err == nil || !strings.Contains(err.Error(), "no tmux pane") {
		t.Fatalf("resizeSession = %v, want no-tmux-pane error", err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./... -run 'TestResizeTmuxTarget|TestRevertTmuxTarget|TestResizeSession' -v`
Expected: FAIL — `resizeTmuxTarget`/`revertTmuxTarget`/`resizeSession` undefined.

- [ ] **Step 3: Write the implementation**

```go
// resize.go
package main

import (
	"fmt"
	"os/exec"
	"strconv"
)

// resizeTmuxTarget resizes s's tmux window to cols x rows via `tmux
// resize-window`. This pins the window to manual-size mode (tmux stops
// auto-sizing it to the largest attached client) until revertTmuxTarget
// undoes it — see docs/superpowers/specs/2026-08-03-preview-resize-design.md
// for the accepted tradeoffs (scrollback rewrap, a real attach seeing a
// pinned size until revert fires).
//
// s.Tmux is the pane-qualified "session:window.pane" location captureTmuxPreview
// already resolves and uses directly for capture-pane (preview.go); tmux's
// resize-window accepts a pane-qualified target and resolves it to the
// containing window, so no separate window-only lookup is needed.
//
// s.Tmux must be non-empty, matching every other call site against
// Session.Tmux in this codebase (sendKeys, send_keys.go:32).
func resizeTmuxTarget(s Session, cols, rows int) error {
	if s.Tmux == "" {
		return fmt.Errorf("PID %d has no tmux pane", s.PID)
	}
	if cols <= 0 || rows <= 0 {
		return fmt.Errorf("invalid size %dx%d", cols, rows)
	}
	if err := exec.Command("tmux", "resize-window", "-t", s.Tmux,
		"-x", strconv.Itoa(cols), "-y", strconv.Itoa(rows)).Run(); err != nil {
		return fmt.Errorf("resize-window: %w", err)
	}
	return nil
}

// revertTmuxTarget un-pins s's tmux window from manual-size mode via `tmux
// resize-window -A`, restoring normal auto-resize-to-largest-attached-client
// behavior.
func revertTmuxTarget(s Session) error {
	if s.Tmux == "" {
		return fmt.Errorf("PID %d has no tmux pane", s.PID)
	}
	if err := exec.Command("tmux", "resize-window", "-t", s.Tmux, "-A").Run(); err != nil {
		return fmt.Errorf("resize-window -A: %w", err)
	}
	return nil
}

// resizeSession dispatches to resizeTmuxTarget or revertTmuxTarget. This is
// the single function both the server handler's injectable seam and the
// local TUI call site point at, so entry (revert=false) and exit
// (revert=true) share one call shape.
func resizeSession(s Session, cols, rows int, revert bool) error {
	if revert {
		return revertTmuxTarget(s)
	}
	return resizeTmuxTarget(s, cols, rows)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./... -run 'TestResizeTmuxTarget|TestRevertTmuxTarget|TestResizeSession' -v`
Expected: PASS (all 5 tests).

- [ ] **Step 5: Commit**

```bash
git add resize.go resize_test.go
git commit -m "feat: add tmux resize/revert primitives"
```

---

### Task 2: Server body decoder for `/resize`

**Files:**
- Modify: `server.go` — insert after `sendKeysBody` ends (currently `server.go:220`)
- Test: `server_test.go`

**Interfaces:**
- Consumes: nothing new (stdlib `encoding/json`, `net/http`, already imported in `server.go`).
- Produces: `resizeBody(w http.ResponseWriter, r *http.Request) (sessionID string, cols, rows int, revert bool, err error)` — Task 3's handler calls this.

- [ ] **Step 1: Write the failing tests**

```go
// add to server_test.go
func TestResizeBodyRequiresSessionID(t *testing.T) {
	req := httptest.NewRequest("POST", "/sessions/42/resize", strings.NewReader(`{"cols":120,"rows":40}`))
	w := httptest.NewRecorder()
	_, _, _, _, err := resizeBody(w, req)
	if err == nil || !strings.Contains(err.Error(), "session_id is required") {
		t.Fatalf("resizeBody err = %v, want session_id-required error", err)
	}
}

func TestResizeBodyRequiresPositiveColsRowsUnlessRevert(t *testing.T) {
	cases := []string{
		`{"session_id":"sess-1","cols":0,"rows":40}`,
		`{"session_id":"sess-1","cols":120,"rows":0}`,
		`{"session_id":"sess-1","cols":-1,"rows":40}`,
	}
	for _, body := range cases {
		req := httptest.NewRequest("POST", "/sessions/42/resize", strings.NewReader(body))
		w := httptest.NewRecorder()
		_, _, _, _, err := resizeBody(w, req)
		if err == nil || !strings.Contains(err.Error(), "cols and rows must be positive") {
			t.Fatalf("resizeBody(%s) err = %v, want cols/rows-positive error", body, err)
		}
	}
}

func TestResizeBodyRevertIgnoresColsRows(t *testing.T) {
	req := httptest.NewRequest("POST", "/sessions/42/resize", strings.NewReader(`{"session_id":"sess-1","revert":true}`))
	w := httptest.NewRecorder()
	sessionID, _, _, revert, err := resizeBody(w, req)
	if err != nil {
		t.Fatalf("resizeBody err = %v, want nil", err)
	}
	if sessionID != "sess-1" || !revert {
		t.Fatalf("resizeBody = (%q, revert=%v), want (sess-1, true)", sessionID, revert)
	}
}

func TestResizeBodyValidResizeSucceeds(t *testing.T) {
	req := httptest.NewRequest("POST", "/sessions/42/resize", strings.NewReader(`{"session_id":"sess-1","cols":120,"rows":40}`))
	w := httptest.NewRecorder()
	sessionID, cols, rows, revert, err := resizeBody(w, req)
	if err != nil {
		t.Fatalf("resizeBody err = %v, want nil", err)
	}
	if sessionID != "sess-1" || cols != 120 || rows != 40 || revert {
		t.Fatalf("resizeBody = (%q, %d, %d, revert=%v), want (sess-1, 120, 40, false)", sessionID, cols, rows, revert)
	}
}

func TestResizeBodyRejectsUnknownFieldsAndTrailingContent(t *testing.T) {
	cases := []string{
		`{"session_id":"sess-1","cols":120,"rows":40,"extra":1}`,
		`{"session_id":"sess-1","cols":120,"rows":40}{}`,
	}
	for _, body := range cases {
		req := httptest.NewRequest("POST", "/sessions/42/resize", strings.NewReader(body))
		w := httptest.NewRecorder()
		_, _, _, _, err := resizeBody(w, req)
		if err == nil {
			t.Fatalf("resizeBody(%s) err = nil, want error", body)
		}
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./... -run TestResizeBody -v`
Expected: FAIL — `resizeBody` undefined.

- [ ] **Step 3: Write the implementation**

Insert into `server.go` immediately after `sendKeysBody`'s closing brace (currently line 220):

```go
// resizeBody reads the required {"session_id","cols","rows","revert"} body
// for POST /sessions/{pid}/resize. session_id is mandatory — like
// sendKeysBody, this endpoint has no legacy caller predating an identity
// guard, so there is no reason to allow an unguarded call. cols/rows are
// required and must be positive unless revert is true, in which case they
// are ignored (a revert is always "undo whatever size is currently set").
func resizeBody(w http.ResponseWriter, r *http.Request) (sessionID string, cols, rows int, revert bool, err error) {
	var body struct {
		SessionID string `json:"session_id"`
		Cols      int    `json:"cols"`
		Rows      int    `json:"rows"`
		Revert    bool   `json:"revert"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		return "", 0, 0, false, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return "", 0, 0, false, fmt.Errorf("unexpected trailing json")
	}
	if body.SessionID == "" {
		return "", 0, 0, false, fmt.Errorf("session_id is required")
	}
	if !body.Revert && (body.Cols <= 0 || body.Rows <= 0) {
		return "", 0, 0, false, fmt.Errorf("cols and rows must be positive")
	}
	return body.SessionID, body.Cols, body.Rows, body.Revert, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./... -run TestResizeBody -v`
Expected: PASS (all 5 tests/subtests).

- [ ] **Step 5: Commit**

```bash
git add server.go server_test.go
git commit -m "feat: add resizeBody decoder for /sessions/{pid}/resize"
```

---

### Task 3: Server handler, seam, and route for `/resize`

**Files:**
- Modify: `server.go` — struct field after `sendKeysFn` (currently line 520); handler after `sendKeysHandler` (currently ends line 1267); route after the send-keys route (currently line 1997)
- Test: `server_test.go`

**Interfaces:**
- Consumes: `resizeBody` (Task 2), `resizeSession` (Task 1), `s.resolveLivePID(pid int, wantSession string) (*Session, []Session, *actionResult)` (existing, `server.go:739`), `actionResult` (existing, `server.go:42`), `codeSessionMismatch` / `codeNotLive` (existing).
- Produces: `(s *server) resizeHandler(w http.ResponseWriter, r *http.Request)`, registered at `POST /sessions/{pid}/resize`; `server.resizeFn func(Session, int, int, bool) error` field (nil in production, falls back to `resizeSession`).

- [ ] **Step 1: Write the failing tests**

```go
// add to server_test.go
func TestResizeHandlerRequiresSessionID(t *testing.T) {
	s := &server{token: "secret"}
	req := httptest.NewRequest("POST", "/sessions/42/resize", strings.NewReader(`{"cols":120,"rows":40}`))
	req.Header.Set("Authorization", "Bearer secret")
	req.SetPathValue("pid", "42")
	w := httptest.NewRecorder()

	s.resizeHandler(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestResizeHandlerRequiresPositiveColsRowsUnlessRevert(t *testing.T) {
	s := &server{token: "secret"}
	req := httptest.NewRequest("POST", "/sessions/42/resize", strings.NewReader(`{"session_id":"sess-1","cols":0,"rows":40}`))
	req.Header.Set("Authorization", "Bearer secret")
	req.SetPathValue("pid", "42")
	w := httptest.NewRecorder()

	s.resizeHandler(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestResizeHandlerMismatchedSessionRefuses(t *testing.T) {
	s := &server{
		token: "secret",
		collect: func() ([]Session, error) {
			return []Session{{PID: 42, SessionID: "sess-current", Tmux: "work:0.0"}}, nil
		},
	}
	req := httptest.NewRequest("POST", "/sessions/42/resize", strings.NewReader(`{"session_id":"sess-stale","cols":120,"rows":40}`))
	req.Header.Set("Authorization", "Bearer secret")
	req.SetPathValue("pid", "42")
	w := httptest.NewRecorder()

	s.resizeHandler(w, req)

	var r actionResult
	if err := json.NewDecoder(w.Body).Decode(&r); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if r.OK || r.Code != codeSessionMismatch {
		t.Fatalf("result = %+v, want refusal with codeSessionMismatch", r)
	}
}

func TestResizeHandlerSuccessCallsResizeFn(t *testing.T) {
	var gotSess Session
	var gotCols, gotRows int
	var gotRevert bool
	s := &server{
		token: "secret",
		collect: func() ([]Session, error) {
			return []Session{{PID: 42, SessionID: "sess-1", Tmux: "work:0.0"}}, nil
		},
		resizeFn: func(sess Session, cols, rows int, revert bool) error {
			gotSess, gotCols, gotRows, gotRevert = sess, cols, rows, revert
			return nil
		},
	}
	req := httptest.NewRequest("POST", "/sessions/42/resize", strings.NewReader(`{"session_id":"sess-1","cols":120,"rows":40}`))
	req.Header.Set("Authorization", "Bearer secret")
	req.SetPathValue("pid", "42")
	w := httptest.NewRecorder()

	s.resizeHandler(w, req)

	var r actionResult
	if err := json.NewDecoder(w.Body).Decode(&r); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !r.OK {
		t.Fatalf("result = %+v, want ok", r)
	}
	if gotSess.PID != 42 || gotCols != 120 || gotRows != 40 || gotRevert {
		t.Fatalf("resizeFn called with (%+v, %d, %d, revert=%v), want (PID 42, 120, 40, false)", gotSess, gotCols, gotRows, gotRevert)
	}
}

func TestResizeHandlerRevertCallsResizeFnWithRevertTrue(t *testing.T) {
	var gotRevert bool
	s := &server{
		token: "secret",
		collect: func() ([]Session, error) {
			return []Session{{PID: 42, SessionID: "sess-1", Tmux: "work:0.0"}}, nil
		},
		resizeFn: func(sess Session, cols, rows int, revert bool) error {
			gotRevert = revert
			return nil
		},
	}
	req := httptest.NewRequest("POST", "/sessions/42/resize", strings.NewReader(`{"session_id":"sess-1","revert":true}`))
	req.Header.Set("Authorization", "Bearer secret")
	req.SetPathValue("pid", "42")
	w := httptest.NewRecorder()

	s.resizeHandler(w, req)

	var r actionResult
	if err := json.NewDecoder(w.Body).Decode(&r); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !r.OK || !gotRevert {
		t.Fatalf("result = %+v, gotRevert = %v, want ok with revert=true", r, gotRevert)
	}
}

func TestResizeHandlerUnauthorized(t *testing.T) {
	s := &server{token: "secret"}
	req := httptest.NewRequest("POST", "/sessions/42/resize", strings.NewReader(`{"session_id":"sess-1","cols":120,"rows":40}`))
	req.SetPathValue("pid", "42")
	w := httptest.NewRecorder()

	s.resizeHandler(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestResizeHandlerNotLiveRefuses(t *testing.T) {
	s := &server{
		token:   "secret",
		collect: func() ([]Session, error) { return nil, nil },
	}
	req := httptest.NewRequest("POST", "/sessions/42/resize", strings.NewReader(`{"session_id":"sess-1","cols":120,"rows":40}`))
	req.Header.Set("Authorization", "Bearer secret")
	req.SetPathValue("pid", "42")
	w := httptest.NewRecorder()

	s.resizeHandler(w, req)

	var r actionResult
	if err := json.NewDecoder(w.Body).Decode(&r); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if r.OK || r.Code != codeNotLive {
		t.Fatalf("result = %+v, want refusal with codeNotLive", r)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./... -run TestResizeHandler -v`
Expected: FAIL — `resizeHandler`/`resizeFn` undefined.

- [ ] **Step 3: Write the implementation**

Add the struct field immediately after `sendKeysFn` in the `server` struct (currently `server.go:520`):

```go
	// resizeFn is an injectable seam for tests; nil in production, where it
	// falls back to the package-level resizeSession (resize.go). Same pattern
	// as sendKeysFn above.
	resizeFn func(Session, int, int, bool) error
```

Add the handler immediately after `sendKeysHandler`'s closing brace (currently `server.go:1267`):

```go
// resizeHandler handles POST /sessions/{pid}/resize: resizes (revert=false)
// or un-pins (revert=true) pid's tmux window to match the inspector viewer's
// terminal. session_id is required (resizeBody), then resolved the same way
// send-keys resolves its target (s.resolveLivePID, server.go:739) — a
// best-effort display enhancement, not a destructive action, so no extra
// reattest beyond the single fresh resolveLivePID check.
func (s *server) resizeHandler(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	pid, err := strconv.Atoi(r.PathValue("pid"))
	if err != nil {
		http.Error(w, "bad pid", http.StatusBadRequest)
		return
	}
	sessionID, cols, rows, revert, err := resizeBody(w, r)
	if err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	target, _, refusal := s.resolveLivePID(pid, sessionID)
	if refusal != nil {
		writeJSON(w, http.StatusOK, *refusal)
		return
	}
	fn := s.resizeFn
	if fn == nil {
		fn = resizeSession
	}
	if err := fn(*target, cols, rows, revert); err != nil {
		writeJSON(w, http.StatusOK, actionResult{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, actionResult{OK: true})
}
```

Add the route immediately after the send-keys route (currently `server.go:1997`):

```go
	mux.HandleFunc("POST /sessions/{pid}/resize", s.resizeHandler)
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./... -run TestResizeHandler -v`
Expected: PASS (all 7 tests).

- [ ] **Step 5: Commit**

```bash
git add server.go server_test.go
git commit -m "feat: add POST /sessions/{pid}/resize endpoint"
```

---

### Task 4: Remote client call

**Files:**
- Modify: `remote_actions.go` — insert after `sendKeysRemote` ends (currently line 464)
- Test: `remote_actions_test.go`

**Interfaces:**
- Consumes: `remoteRequest(name, path, method string, body []byte) ([]byte, error)` (existing, `remote_actions.go:125`), `actionResult` (existing).
- Produces: `resizeRemote(host string, pid int, sessionID string, cols, rows int, revert bool) (actionResult, error)` — Task 5's `tui.go` wiring calls this.

- [ ] **Step 1: Write the failing tests**

```go
// add to remote_actions_test.go
func TestResizeRemoteSendsSessionIDColsRowsRevert(t *testing.T) {
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

	r, err := resizeRemote("box", 42, "sess-1", 120, 40, false)
	if err != nil || !r.OK {
		t.Fatalf("resizeRemote = (%#v, %v)", r, err)
	}
	if string(gotBody) != `{"cols":120,"revert":false,"rows":40,"session_id":"sess-1"}` {
		t.Fatalf("body = %s, want session_id/cols/rows/revert", gotBody)
	}
}

func TestResizeRemotePropagatesRefusal(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":false,"error":"PID 42 is a different session now","code":"session_mismatch"}`))
	}))
	defer backend.Close()

	u, _ := url.Parse(backend.URL)
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeServerYAML(t, home, "box", u.Hostname(), u.Port(), "secret")

	r, err := resizeRemote("box", 42, "sess-stale", 120, 40, false)
	if err != nil {
		t.Fatalf("resizeRemote err = %v", err)
	}
	if r.OK || r.Code != codeSessionMismatch {
		t.Fatalf("result = %+v, want refusal with codeSessionMismatch", r)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./... -run TestResizeRemote -v`
Expected: FAIL — `resizeRemote` undefined.

- [ ] **Step 3: Write the implementation**

Insert into `remote_actions.go` immediately after `sendKeysRemote`'s closing brace (currently line 464):

```go
// resizeRemote asks host's server to resize (revert=false) or un-pin
// (revert=true) pid's tmux window. Mirrors sendKeysRemote: sessionID is
// never optional, since this is a new endpoint with no legacy caller to
// stay compatible with (see resizeBody, server.go).
func resizeRemote(host string, pid int, sessionID string, cols, rows int, revert bool) (actionResult, error) {
	body, err := json.Marshal(map[string]any{
		"session_id": sessionID,
		"cols":       cols,
		"rows":       rows,
		"revert":     revert,
	})
	if err != nil {
		return actionResult{}, err
	}
	resp, err := remoteRequest(host, fmt.Sprintf("/sessions/%d/resize", pid), "POST", body)
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

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./... -run TestResizeRemote -v`
Expected: PASS (both tests).

- [ ] **Step 5: Commit**

```bash
git add remote_actions.go remote_actions_test.go
git commit -m "feat: add resizeRemote client for /sessions/{pid}/resize"
```

---

### Task 5: Wire resize/revert into inspector open/close

**Files:**
- Modify: `tui.go` — new closure before `openInspector` (currently `tui.go:434`); one insertion inside `openInspector` (currently ends `tui.go:466`); one change inside `closeInspector` (currently `tui.go:471-484`)

**Interfaces:**
- Consumes: `resolveLivePIDLocal(pid int, wantSessionID string) (Session, error)` (existing, `send_keys.go:51`), `resizeSession` (Task 1), `resizeRemote` (Task 4), `inspectorChromeRows` (existing constant, `tui.go:68`), `term.GetSize(fd int) (int, int, error)` (existing import, already used at `tui.go:311`).
- Produces: nothing new consumed elsewhere — this is the final caller.

This task has no new unit tests: `openInspector`/`closeInspector`/`sendKeysToInspected` are unexported closures inside `RunTUI` with no existing dedicated tests (verified via `grep -rn "openInspector\|closeInspector" tui_test.go` returning nothing) — the existing pattern for this code is build + full regression suite + manual verification, not isolated unit tests. Follow that pattern rather than inventing new test scaffolding for it.

- [ ] **Step 1: Add the `resizeInspected` dispatch closure**

Insert immediately before the `// openInspector enters...` comment (currently `tui.go:434`):

```go
	// resizeInspected fires a best-effort tmux resize (or, with revert=true,
	// an un-pin) for sess, dispatching local vs. remote exactly like
	// sendKeysToInspected below. Errors are never surfaced: this is a display
	// enhancement, not something that can block entering or leaving preview —
	// preview content renders via capture-pane regardless of the pane's
	// current size (see docs/superpowers/specs/
	// 2026-08-03-preview-resize-design.md).
	resizeInspected := func(sess Session, cols, rows int, revert bool) {
		if sess.Host == "" {
			live, err := resolveLivePIDLocal(sess.PID, sess.SessionID)
			if err != nil {
				return
			}
			_ = resizeSession(live, cols, rows, revert)
			return
		}
		_, _ = resizeRemote(sess.Host, sess.PID, sess.SessionID, cols, rows, revert)
	}

```

- [ ] **Step 2: Fire the resize on inspector entry**

In `openInspector`, insert immediately before `state.mode = screenInspector` (currently `tui.go:461`, right after the `if ticketID := ...; ticketID != "" { ... }` block closes):

```go
		if cols, rows, err := term.GetSize(fd); err == nil && cols > 0 {
			// Best-effort only: the ticket-summary block hasn't loaded yet at
			// this point, so this slightly under-reserves rows on first open —
			// the accepted one-shot tradeoff documented in the design spec.
			if innerRows := rows - inspectorChromeRows; innerRows > 0 {
				resizeInspected(sess, cols, innerRows, false)
			}
		}
```

(`sess` and `fd` are already in scope — `sess` from `sess := *target.session` earlier in `openInspector`, `fd` from `RunTUI`'s outer scope, same one `render()` uses.)

- [ ] **Step 3: Fire the revert on inspector exit**

Replace the start of `closeInspector` (currently `tui.go:471-475`):

```go
	closeInspector := func() {
		if inspectorHub != nil {
			inspectorHub.Shutdown()
			inspectorHub = nil
		}
```

with:

```go
	closeInspector := func() {
		if inspectorHub != nil {
			resizeInspected(state.inspector.snapshot.Session, 0, 0, true)
			inspectorHub.Shutdown()
			inspectorHub = nil
		}
```

- [ ] **Step 4: Build and run the full test suite**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: build succeeds, vet is clean, all tests (old and new) pass.

- [ ] **Step 5: Manual verification**

1. Start (or reuse) a local tmux session running a Claude session tracked by `claude-sessions`.
2. Note its current tmux window size: `tmux list-windows -t <session> -F '#{window_width}x#{window_height}'`.
3. Run the TUI (`make run` or `go run .`) in a terminal window of a different size than the target's tmux window.
4. Open inspector/preview on that session (arrow to select, then Enter/'p' per the existing keybinding).
5. Re-run `tmux list-windows -t <session> -F '#{window_width}x#{window_height}'` from another terminal — confirm it now matches the inspector's inner viewport (terminal cols x (terminal rows - `inspectorChromeRows`)).
6. Leave preview mode (Esc/back).
7. Re-run `tmux list-windows -t <session> -F '#{window_width}x#{window_height} #{window_size}'` — the window should be back to automatic sizing (`#{window_size}` should no longer report `manual`).
8. Repeat steps 3–7 against a **remote** session (a host in `servers.yaml`) to confirm the same behavior over the `/resize` endpoint.

- [ ] **Step 6: Commit**

```bash
git add tui.go
git commit -m "feat: resize previewed session's tmux window to match inspector viewport"
```

---

## Self-Review

**Spec coverage:**
- Both local and remote scope → Tasks 1 (shared primitive), 4 (remote client), 5 (dispatches on `sess.Host`). ✓
- Auto-revert on preview exit → Task 5, Step 3. ✓
- One-shot on entry, no poll-tick/live-resize repetition → Task 5, Steps 2–3 fire exactly once each (`openInspector`/`closeInspector`, not `render()`). ✓
- Inner-viewport sizing (not full terminal) → Task 5, Step 2 subtracts `inspectorChromeRows`. ✓
- Scrollback-rewrap and manual-size-mode limitations accepted, documented → noted in Task 1 and Task 5 comments, full detail already in the design spec. ✓
- `POST /sessions/{pid}/resize` endpoint, `session_id` required, injectable seam → Tasks 2–3. ✓
- Testing plan (unit tests for primitives, handler tests, manual check) → Tasks 1–4 unit tests, Task 5 manual verification. ✓

**Placeholder scan:** No TBD/TODO; every step has literal code or an exact shell command.

**Type consistency:** `resizeSession(Session, int, int, bool) error` is the same signature everywhere it's referenced — `server.resizeFn` field (Task 3), the fallback assignment in `resizeHandler` (Task 3), and Task 1's own definition. `resizeRemote(host string, pid int, sessionID string, cols, rows int, revert bool) (actionResult, error)` matches between Task 4's definition and Task 5's call site. `resizeBody`'s five return values (`sessionID string, cols, rows int, revert bool, err error`) match between Task 2's definition and Task 3's call site.

**Scope check:** Single cohesive feature, five tasks each independently testable and committable. No decomposition needed.
