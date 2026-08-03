# Inspector send-keys Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let the user press `i` in the fullscreen inspector to compose a single line of text and Enter to send it as literal keystrokes into the selected session's tmux pane, for both local and remote sessions.

**Architecture:** A shared `sendKeys(Session, string) error` primitive (two `tmux send-keys` calls: `-l text` then `Enter`) is called directly for local sessions after a fresh local identity re-check, and through a new authed `POST /sessions/{pid}/send-keys` endpoint for remote sessions — mirroring exactly how `kill`/`migrate` already split local-vs-remote dispatch. The inspector's `i` key arms a compose sub-state that steals keyboard input until Enter/Esc; the compose result renders in the inspector's own footer.

**Tech Stack:** Go stdlib (`net/http`, `encoding/json`, `os/exec`) + `golang.org/x/term`/`golang.org/x/sys` only, per this repo's dependency policy. No new dependencies.

**Design doc:** `docs/superpowers/specs/2026-08-03-inspector-send-keys-design.md`

## Global Constraints

- Follow CLAUDE.md's workflow: all work happens in a git worktree (`.claude/worktrees/<name>`), never directly on `main`.
- No shell interpolation anywhere in this feature — `exec.Command` argument slices only, matching the existing `tmuxSendLiteral` (paste.go:403-405).
- `session_id` is **required** (not optional) on every new code path this plan adds (`resolveLivePIDLocal`, the new server endpoint) — there is no legacy caller predating the identity guard, unlike kill/migrate's fail-open empty-`session_id` behavior.
- Match this repo's existing seam-for-testing convention: server-side mutating primitives get an injectable `func` field on `*server` (nil falls back to the real implementation), so handler tests never touch real tmux or the developer's real `~/.claude/sessions`.
- `go build ./...`, `go vet ./...`, and `go test ./...` must stay green after every task.
- Every new exported/package-level function gets a doc comment in this codebase's existing voice (WHY, not WHAT — see any existing function in server.go/migrate.go for the tone).

---

### Task 1: `sendKeys` primitive + local fresh-attest resolver

**Files:**
- Create: `send_keys.go`
- Create: `send_keys_test.go`

**Interfaces:**
- Produces: `sendKeys(s Session, text string) error` — injects `text` as literal keystrokes plus Enter into `s.Tmux`'s pane. Used by Task 2 (server handler) and Task 6 (TUI local dispatch).
- Produces: `resolveLivePIDLocal(pid int, wantSessionID string) (Session, error)` — fresh `CollectLocal()`-based identity check returning a `Session` with a current `Tmux` field. Used by Task 6 (TUI local dispatch).
- Produces: `const sendKeysMaxLen = 4096` — shared bound Task 2's server-side validator also uses.
- Consumes: `Session` (session.go), `CollectLocal()` (session.go:157).

- [ ] **Step 1: Write the failing tests**

```go
// send_keys_test.go
package main

import (
	"os"
	"strings"
	"testing"
)

func TestResolveLivePIDLocalMatchingSessionSucceeds(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	pid := os.Getpid()
	writeSessionFileForPID(t, dir, Session{PID: pid, SessionID: "sess-abc", CWD: "/home/testuser/project"})

	s, err := resolveLivePIDLocal(pid, "sess-abc")
	if err != nil {
		t.Fatalf("resolveLivePIDLocal = %v, want nil error", err)
	}
	if s.SessionID != "sess-abc" {
		t.Fatalf("SessionID = %q, want sess-abc", s.SessionID)
	}
}

func TestResolveLivePIDLocalMismatchedSessionRefuses(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	pid := os.Getpid()
	writeSessionFileForPID(t, dir, Session{PID: pid, SessionID: "sess-new", CWD: "/home/testuser/project"})

	_, err := resolveLivePIDLocal(pid, "sess-old")
	if err == nil || !strings.Contains(err.Error(), "different session now") {
		t.Fatalf("resolveLivePIDLocal = %v, want session-mismatch error", err)
	}
}

func TestResolveLivePIDLocalGonePIDRefuses(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	// No session file written for this PID at all.

	_, err := resolveLivePIDLocal(999999, "sess-abc")
	if err == nil || !strings.Contains(err.Error(), "not a live Claude session") {
		t.Fatalf("resolveLivePIDLocal = %v, want not-live error", err)
	}
}

func TestResolveLivePIDLocalEmptySessionIDRefuses(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	_, err := resolveLivePIDLocal(os.Getpid(), "")
	if err == nil || !strings.Contains(err.Error(), "session_id is required") {
		t.Fatalf("resolveLivePIDLocal = %v, want session_id-required error", err)
	}
}
```

`writeSessionFileForPID` already exists in migrate_test.go (same package) — reuse it, don't redefine it.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./... -run TestResolveLivePIDLocal -v`
Expected: FAIL — `resolveLivePIDLocal` undefined.

- [ ] **Step 3: Write the implementation**

```go
// send_keys.go
package main

import (
	"fmt"
	"os/exec"
)

// sendKeysMaxLen bounds a single compose-box message. Kept here so both the
// TUI (which can reject before paying a round trip) and the server's own
// validator (server.go's sendKeysBody) enforce the identical limit.
const sendKeysMaxLen = 4096

// sendKeys injects text into s's tmux pane as literal keystrokes followed by
// Enter — two calls, matching the existing tmuxSendLiteral (paste.go:403-405).
// The -l flag on the first call is required: without it tmux parses text as
// key names, so a message that happens to read "Enter" or "C-c" would trigger
// that tmux action instead of being typed literally. Neither call goes
// through a shell (exec.Command argument slices), so there is no
// shell-injection surface regardless of message content.
func sendKeys(s Session, text string) error {
	if err := exec.Command("tmux", "send-keys", "-t", s.Tmux, "-l", text).Run(); err != nil {
		return fmt.Errorf("send text: %w", err)
	}
	if err := exec.Command("tmux", "send-keys", "-t", s.Tmux, "Enter").Run(); err != nil {
		return fmt.Errorf("send enter: %w", err)
	}
	return nil
}

// resolveLivePIDLocal is the local-TUI-process counterpart of the server's
// resolveLivePID (server.go:691): a fresh CollectLocal() so the returned
// Session.Tmux is current, not the possibly-stale pane address an inspector
// snapshot may still be holding. wantSessionID is mandatory — the TUI always
// has a real one from the live inspector snapshot, so unlike localReattest
// (migrate.go:360, which kill/migrate's legacy optional-precondition callers
// still need), there is no "no precondition" mode to support here.
func resolveLivePIDLocal(pid int, wantSessionID string) (Session, error) {
	if wantSessionID == "" {
		return Session{}, fmt.Errorf("session_id is required")
	}
	sessions, err := CollectLocal()
	if err != nil {
		return Session{}, err
	}
	for _, s := range sessions {
		if s.PID != pid {
			continue
		}
		if s.SessionID != wantSessionID {
			return Session{}, fmt.Errorf("PID %d is a different session now", pid)
		}
		return s, nil
	}
	return Session{}, fmt.Errorf("PID %d is not a live Claude session", pid)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./... -run TestResolveLivePIDLocal -v`
Expected: PASS (all four cases).

- [ ] **Step 5: Commit**

```bash
git add send_keys.go send_keys_test.go
git commit -m "feat: add sendKeys tmux primitive and local fresh-attest resolver"
```

---

### Task 2: Server endpoint `POST /sessions/{pid}/send-keys`

**Files:**
- Modify: `server.go` (add `sendKeysFn` seam field to `server` struct near line 451-472, add `sendKeysBody` decoder near `sessionIDPrecondition` at line 107-176, add `sendKeysHandler` near `kill` at line 1130-1183, register route near line 1910-1912)
- Test: `server_test.go`

**Interfaces:**
- Consumes: `sendKeys` and `sendKeysMaxLen` (Task 1, send_keys.go), `s.resolveLivePID` (server.go:691, existing), `actionResult`, `codeSessionMismatch`, `codeNotLive` (server.go, existing).
- Produces: route `POST /sessions/{pid}/send-keys` — body `{"session_id": "...", "text": "..."}`, both required; response is the existing `actionResult` envelope (`{"ok":true}` or `{"error":"...","code":"..."}`). Used by Task 3 (remote client).

- [ ] **Step 1: Write the failing tests**

Add to server_test.go (mirror the existing kill-handler test style — check `TestKillHandler...` in that file for the harness helpers `newTestServer`/`doRequest` before writing these; use the same ones):

```go
func TestSendKeysHandlerRequiresSessionID(t *testing.T) {
	s := &server{token: "secret"}
	req := httptest.NewRequest("POST", "/sessions/42/send-keys", strings.NewReader(`{"text":"hello"}`))
	req.Header.Set("Authorization", "Bearer secret")
	req.SetPathValue("pid", "42")
	w := httptest.NewRecorder()

	s.sendKeysHandler(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestSendKeysHandlerRequiresText(t *testing.T) {
	s := &server{token: "secret"}
	req := httptest.NewRequest("POST", "/sessions/42/send-keys", strings.NewReader(`{"session_id":"sess-1"}`))
	req.Header.Set("Authorization", "Bearer secret")
	req.SetPathValue("pid", "42")
	w := httptest.NewRecorder()

	s.sendKeysHandler(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestSendKeysHandlerMismatchedSessionRefuses(t *testing.T) {
	s := &server{
		token: "secret",
		collect: func() ([]Session, error) {
			return []Session{{PID: 42, SessionID: "sess-current", Tmux: "work:0.0"}}, nil
		},
	}
	req := httptest.NewRequest("POST", "/sessions/42/send-keys", strings.NewReader(`{"session_id":"sess-stale","text":"hello"}`))
	req.Header.Set("Authorization", "Bearer secret")
	req.SetPathValue("pid", "42")
	w := httptest.NewRecorder()

	s.sendKeysHandler(w, req)

	var r actionResult
	if err := json.NewDecoder(w.Body).Decode(&r); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if r.OK || r.Code != codeSessionMismatch {
		t.Fatalf("result = %+v, want refusal with codeSessionMismatch", r)
	}
}

func TestSendKeysHandlerSuccessCallsSendKeysFn(t *testing.T) {
	var gotSess Session
	var gotText string
	s := &server{
		token: "secret",
		collect: func() ([]Session, error) {
			return []Session{{PID: 42, SessionID: "sess-1", Tmux: "work:0.0"}}, nil
		},
		sendKeysFn: func(sess Session, text string) error {
			gotSess, gotText = sess, text
			return nil
		},
	}
	req := httptest.NewRequest("POST", "/sessions/42/send-keys", strings.NewReader(`{"session_id":"sess-1","text":"hello"}`))
	req.Header.Set("Authorization", "Bearer secret")
	req.SetPathValue("pid", "42")
	w := httptest.NewRecorder()

	s.sendKeysHandler(w, req)

	var r actionResult
	if err := json.NewDecoder(w.Body).Decode(&r); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !r.OK {
		t.Fatalf("result = %+v, want ok", r)
	}
	if gotSess.PID != 42 || gotText != "hello" {
		t.Fatalf("sendKeysFn called with (%+v, %q), want (PID 42, \"hello\")", gotSess, gotText)
	}
}
```

If `newTestServer`/a different harness pattern already exists in server_test.go for the kill handler, use that harness instead of constructing `&server{}` by hand — read the file first and match its existing convention exactly.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./... -run TestSendKeysHandler -v`
Expected: FAIL — `sendKeysHandler`/`sendKeysFn` undefined.

- [ ] **Step 3: Write the implementation**

Add to the `server` struct (server.go, alongside `terminate func(Session) error` at line 454):

```go
	// sendKeysFn is an injectable seam for tests; nil in production, where it
	// falls back to the package-level sendKeys (send_keys.go). Same pattern as
	// collect/terminate above.
	sendKeysFn func(Session, string) error
```

Add near `sessionIDPrecondition` (server.go, after line 176):

```go
// sendKeysBody reads the required {"session_id","text"} body for POST
// /sessions/{pid}/send-keys. Unlike sessionIDPrecondition, session_id is
// mandatory: this endpoint has no legacy caller predating the identity
// guard, so there is no reason to allow an unguarded send. text is bounded
// and rejected if empty or if it contains a CR, LF, or NUL byte — send-keys
// is single-line by design; a caller wanting those bytes belongs on the
// tmux-key-name surface sendKeys's -l flag deliberately avoids, not in
// message content.
func sendKeysBody(w http.ResponseWriter, r *http.Request) (sessionID, text string, err error) {
	var body struct {
		SessionID string `json:"session_id"`
		Text      string `json:"text"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, int64(sendKeysMaxLen)+256))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		return "", "", err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return "", "", fmt.Errorf("unexpected trailing json")
	}
	if body.SessionID == "" {
		return "", "", fmt.Errorf("session_id is required")
	}
	if body.Text == "" {
		return "", "", fmt.Errorf("text must not be empty")
	}
	if len(body.Text) > sendKeysMaxLen {
		return "", "", fmt.Errorf("text exceeds %d bytes", sendKeysMaxLen)
	}
	if strings.ContainsAny(body.Text, "\r\n\x00") {
		return "", "", fmt.Errorf("text must not contain control characters")
	}
	return body.SessionID, body.Text, nil
}
```

Add near `kill` (server.go, after line 1183):

```go
// sendKeysHandler handles POST /sessions/{pid}/send-keys: send text as
// literal keystrokes plus Enter into pid's tmux pane. session_id is required
// (sendKeysBody), then resolved the same way kill resolves its target
// (s.resolveLivePID, server.go:691) so the pane address is current, not
// whatever the client's inspector snapshot last saw.
func (s *server) sendKeysHandler(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	pid, err := strconv.Atoi(r.PathValue("pid"))
	if err != nil {
		http.Error(w, "bad pid", http.StatusBadRequest)
		return
	}
	sessionID, text, err := sendKeysBody(w, r)
	if err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	target, _, refusal := s.resolveLivePID(pid, sessionID)
	if refusal != nil {
		writeJSON(w, http.StatusOK, *refusal)
		return
	}
	fn := s.sendKeysFn
	if fn == nil {
		fn = sendKeys
	}
	if err := fn(*target, text); err != nil {
		writeJSON(w, http.StatusOK, actionResult{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, actionResult{OK: true})
}
```

Register the route (server.go, alongside line 1911):

```go
	mux.HandleFunc("POST /sessions/{pid}/send-keys", s.sendKeysHandler)
```

Check server.go's existing imports already include `strings` and `errors`/`io` (used by `sessionIDPrecondition` already) — no new imports should be needed.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./... -run TestSendKeysHandler -v`
Expected: PASS (all four cases).

- [ ] **Step 5: Run the full test suite and vet**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: all green.

- [ ] **Step 6: Commit**

```bash
git add server.go server_test.go
git commit -m "feat: add POST /sessions/{pid}/send-keys endpoint"
```

---

### Task 3: Remote client `sendKeysRemote`

**Files:**
- Modify: `remote_actions.go` (add function near `killRemote` at line 426-441)
- Test: `remote_actions_test.go`

**Interfaces:**
- Consumes: `remoteRequest` (remote.go, existing), `actionResult` (server.go, existing).
- Produces: `sendKeysRemote(host string, pid int, sessionID, text string) (actionResult, error)`. Used by Task 6 (TUI dispatch).

- [ ] **Step 1: Write the failing test**

Add to remote_actions_test.go, next to `TestKillRemoteSendsSessionIDWhenKnown` (line 123):

```go
func TestSendKeysRemoteSendsSessionIDAndText(t *testing.T) {
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

	r, err := sendKeysRemote("box", 42, "sess-1", "hello")
	if err != nil || !r.OK {
		t.Fatalf("sendKeysRemote = (%#v, %v)", r, err)
	}
	if string(gotBody) != `{"session_id":"sess-1","text":"hello"}` {
		t.Fatalf("body = %s, want session_id and text", gotBody)
	}
}

func TestSendKeysRemotePropagatesRefusal(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":false,"error":"PID 42 is a different session now","code":"session_mismatch"}`))
	}))
	defer backend.Close()

	u, _ := url.Parse(backend.URL)
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeServerYAML(t, home, "box", u.Hostname(), u.Port(), "secret")

	r, err := sendKeysRemote("box", 42, "sess-stale", "hello")
	if err != nil {
		t.Fatalf("sendKeysRemote err = %v", err)
	}
	if r.OK || r.Code != codeSessionMismatch {
		t.Fatalf("result = %+v, want refusal with codeSessionMismatch", r)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./... -run TestSendKeysRemote -v`
Expected: FAIL — `sendKeysRemote` undefined.

- [ ] **Step 3: Write the implementation**

Add to remote_actions.go, after `killRemote` (line 441):

```go
// sendKeysRemote asks host's server to send text as literal keystrokes plus
// Enter into pid's tmux pane. Unlike killRemote/migrateRemote, sessionID is
// never optional here — there is no legacy caller this must stay compatible
// with (see sendKeysBody, server.go), so there is no bare-{} fallback body.
func sendKeysRemote(host string, pid int, sessionID, text string) (actionResult, error) {
	body, err := json.Marshal(map[string]string{"session_id": sessionID, "text": text})
	if err != nil {
		return actionResult{}, err
	}
	resp, err := remoteRequest(host, fmt.Sprintf("/sessions/%d/send-keys", pid), "POST", body)
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

Run: `go test ./... -run TestSendKeysRemote -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add remote_actions.go remote_actions_test.go
git commit -m "feat: add sendKeysRemote client for the send-keys endpoint"
```

---

### Task 4: Inspector compose state + key handling

**Files:**
- Modify: `inspector.go` (add fields to `inspectorViewState`, lines 38-45)
- Modify: `tui_state.go` (add `handleInspectorCompose` near `handleInspectorKey`, line 376-402)
- Test: `tui_state_test.go`

**Interfaces:**
- Consumes: `KeyEnter`, `KeyEsc` (tui.go:40, tui_events.go:27).
- Produces: `inspectorViewState.composing bool`, `.composeText string`, `.composeStatus string`, `.composeStatusUntil time.Time`. Consumed by Task 5 (event routing) and Task 7 (rendering).
- Produces: `(*tuiState) handleInspectorCompose(key string) (submit, cancel bool)` — pure state transform, mirrors `newPickerState.handlePrompt` (new_picker.go:101-121)'s edit rules. Consumed by Task 5.

- [ ] **Step 1: Write the failing tests**

Add to tui_state_test.go, near the existing `TestHandleInspectorKey...` tests:

```go
func TestHandleInspectorComposeAppendsPrintableAndBackspaces(t *testing.T) {
	s := &tuiState{inspector: inspectorViewState{composing: true}}
	for _, k := range []string{"h", "i"} {
		if submit, cancel := s.handleInspectorCompose(k); submit || cancel {
			t.Fatalf("handleInspectorCompose(%q) = (%v,%v), want (false,false)", k, submit, cancel)
		}
	}
	if s.inspector.composeText != "hi" {
		t.Fatalf("composeText = %q, want hi", s.inspector.composeText)
	}
	s.handleInspectorCompose("\x7f")
	if s.inspector.composeText != "h" {
		t.Fatalf("composeText after backspace = %q, want h", s.inspector.composeText)
	}
}

func TestHandleInspectorComposeEnterOnEmptyIsNoop(t *testing.T) {
	s := &tuiState{inspector: inspectorViewState{composing: true}}
	submit, cancel := s.handleInspectorCompose(KeyEnter)
	if submit || cancel {
		t.Fatalf("handleInspectorCompose(Enter on empty) = (%v,%v), want (false,false)", submit, cancel)
	}
}

func TestHandleInspectorComposeEnterOnNonEmptySubmitsAndPreservesText(t *testing.T) {
	s := &tuiState{inspector: inspectorViewState{composing: true, composeText: "hello"}}
	submit, cancel := s.handleInspectorCompose(KeyEnter)
	if !submit || cancel {
		t.Fatalf("handleInspectorCompose(Enter on \"hello\") = (%v,%v), want (true,false)", submit, cancel)
	}
	if !s.inspector.composing || s.inspector.composeText != "hello" {
		t.Fatalf("state after submit = composing=%v text=%q, want composing=true text=\"hello\" (caller clears on success)", s.inspector.composing, s.inspector.composeText)
	}
}

func TestHandleInspectorComposeEscCancelsAndDiscards(t *testing.T) {
	s := &tuiState{inspector: inspectorViewState{composing: true, composeText: "hello"}}
	submit, cancel := s.handleInspectorCompose(KeyEsc)
	if submit || !cancel {
		t.Fatalf("handleInspectorCompose(Esc) = (%v,%v), want (false,true)", submit, cancel)
	}
	if s.inspector.composing || s.inspector.composeText != "" {
		t.Fatalf("state after Esc = composing=%v text=%q, want composing=false text=\"\"", s.inspector.composing, s.inspector.composeText)
	}
}

func TestHandleInspectorComposeCtrlCCancelsAndDiscards(t *testing.T) {
	s := &tuiState{inspector: inspectorViewState{composing: true, composeText: "hello"}}
	submit, cancel := s.handleInspectorCompose("\x03")
	if submit || !cancel {
		t.Fatalf("handleInspectorCompose(Ctrl-C) = (%v,%v), want (false,true)", submit, cancel)
	}
}

func TestHandleInspectorComposeIgnoresNonPrintableBytes(t *testing.T) {
	s := &tuiState{inspector: inspectorViewState{composing: true}}
	s.handleInspectorCompose("\x01")
	if s.inspector.composeText != "" {
		t.Fatalf("composeText = %q, want empty (control byte ignored)", s.inspector.composeText)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./... -run TestHandleInspectorCompose -v`
Expected: FAIL — `handleInspectorCompose` undefined, `inspectorViewState` has no `composing`/`composeText` fields.

- [ ] **Step 3: Write the implementation**

Add fields to `inspectorViewState` (inspector.go, in the struct at line 38-45):

```go
type inspectorViewState struct {
	targetID     string
	snapshot     InspectorSnapshot
	top          int
	viewportRows int
	follow       bool
	newLines     int
	// composing, composeText, composeStatus, and composeStatusUntil back the
	// 'i'-key compose box (send-keys, see docs/superpowers/specs/
	// 2026-08-03-inspector-send-keys-design.md). composeText is deliberately
	// NOT cleared on a failed send — the caller (handleInspectorEvent) leaves
	// composing true and the text intact so the user can correct and resend
	// without retyping.
	composing          bool
	composeText        string
	composeStatus      string
	composeStatusUntil time.Time
}
```

Add to tui_state.go, after `handleInspectorKey` (line 402):

```go
// handleInspectorCompose applies one key event while the inspector's send-keys
// compose box is active, editing composeText. Byte-level edit rule matches
// newPickerState.handlePrompt (new_picker.go:101-121): single-byte printable
// ASCII (0x20-0x7e) appends, DEL/BS (\x7f/\x08) backspaces. Enter on a
// non-empty buffer returns submit=true but does NOT itself clear composing or
// composeText — the caller (handleInspectorEvent) owns that, because a failed
// send must leave both in place for the user to retry. Enter on an empty
// buffer is a no-op, mirroring handlePrompt's own empty-Prompt Enter rule.
// Esc and Ctrl-C both cancel: composing is cleared and composeText discarded
// immediately, since a cancel has nothing left to retry.
func (s *tuiState) handleInspectorCompose(key string) (submit, cancel bool) {
	switch key {
	case "\r", "\n", KeyEnter:
		if s.inspector.composeText == "" {
			return false, false
		}
		return true, false
	case KeyEsc, "\x03":
		s.inspector.composing = false
		s.inspector.composeText = ""
		return false, true
	case "\x7f", "\x08":
		if s.inspector.composeText != "" {
			s.inspector.composeText = s.inspector.composeText[:len(s.inspector.composeText)-1]
		}
	default:
		if len(key) == 1 && key[0] >= 0x20 && key[0] <= 0x7e {
			s.inspector.composeText += key
		}
	}
	return false, false
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./... -run TestHandleInspectorCompose -v`
Expected: PASS (all six cases).

- [ ] **Step 5: Commit**

```bash
git add inspector.go tui_state.go tui_state_test.go
git commit -m "feat: add inspector compose-box state and key handling"
```

---

### Task 5: `handleInspectorEvent` compose routing

**Files:**
- Modify: `tui.go` (restructure `handleInspectorEvent`, lines 759-806)
- Test: `tui_test.go`

**Interfaces:**
- Consumes: `handleInspectorCompose` (Task 4), `inspectorViewState.composing`/`.composeText`/`.composeStatus`/`.composeStatusUntil` (Task 4).
- Produces: new `handleInspectorEvent` signature `func(ev inputEvent, state *tuiState, hubPtr **InspectorHub, closeInspector, render, attach, kill func(), sendText func(Session, string) (bool, string)) (quit, absorbBatch bool)`. `absorbBatch` is `true` exactly when this event ended compose mode (submit-success or cancel) — the caller (Task 6) must stop feeding the rest of the current input batch to `handleInspectorEvent` when it sees `absorbBatch`, so trailing bytes from a paste that included the compose-submitting newline can't leak into kill/back key dispatch. `sendText` is called with the inspector's current snapshot `Session` and the composed text; it returns `(true, "")` on success or `(false, <message>)` on failure — this function has no opinion on local-vs-remote, that's Task 6's job.

- [ ] **Step 1: Write the failing tests**

Add to tui_test.go, near the existing `inspectorKeyCommand` tests:

```go
func TestHandleInspectorEventArmsComposeOnIWhenTmuxPresent(t *testing.T) {
	state := &tuiState{mode: screenInspector, inspector: inspectorViewState{
		snapshot: InspectorSnapshot{Session: Session{Tmux: "work:0.0"}},
	}}
	var hub *InspectorHub
	noop := func() {}
	ev := inputEvent{kind: eventKey, key: "i"}

	quit, absorb := handleInspectorEvent(ev, state, &hub, noop, noop, noop, noop, nil)

	if quit || absorb {
		t.Fatalf("handleInspectorEvent('i') = (%v,%v), want (false,false)", quit, absorb)
	}
	if !state.inspector.composing {
		t.Fatal("composing = false, want true")
	}
}

func TestHandleInspectorEventIgnoresIWhenNoTmux(t *testing.T) {
	state := &tuiState{mode: screenInspector, inspector: inspectorViewState{
		snapshot: InspectorSnapshot{Session: Session{Tmux: ""}},
	}}
	var hub *InspectorHub
	noop := func() {}
	ev := inputEvent{kind: eventKey, key: "i"}

	handleInspectorEvent(ev, state, &hub, noop, noop, noop, noop, nil)

	if state.inspector.composing {
		t.Fatal("composing = true, want false (no tmux pane to send into)")
	}
}

func TestHandleInspectorEventSubmitCallsSendTextAndClearsOnSuccess(t *testing.T) {
	state := &tuiState{mode: screenInspector, inspector: inspectorViewState{
		composing:   true,
		composeText: "hello",
		snapshot:    InspectorSnapshot{Session: Session{PID: 42, Tmux: "work:0.0"}},
	}}
	var hub *InspectorHub
	noop := func() {}
	var gotText string
	sendText := func(sess Session, text string) (bool, string) {
		gotText = text
		return true, ""
	}
	ev := inputEvent{kind: eventKey, key: KeyEnter}

	quit, absorb := handleInspectorEvent(ev, state, &hub, noop, noop, noop, noop, sendText)

	if quit || !absorb {
		t.Fatalf("handleInspectorEvent(Enter submit) = (%v,%v), want (false,true)", quit, absorb)
	}
	if gotText != "hello" {
		t.Fatalf("sendText got %q, want hello", gotText)
	}
	if state.inspector.composing || state.inspector.composeText != "" {
		t.Fatalf("state after success = composing=%v text=%q, want cleared", state.inspector.composing, state.inspector.composeText)
	}
	if state.inspector.composeStatus != "sent" {
		t.Fatalf("composeStatus = %q, want sent", state.inspector.composeStatus)
	}
}

func TestHandleInspectorEventSubmitPreservesTextOnFailure(t *testing.T) {
	state := &tuiState{mode: screenInspector, inspector: inspectorViewState{
		composing:   true,
		composeText: "hello",
		snapshot:    InspectorSnapshot{Session: Session{PID: 42, Tmux: "work:0.0"}},
	}}
	var hub *InspectorHub
	noop := func() {}
	sendText := func(sess Session, text string) (bool, string) {
		return false, "PID 42 is a different session now"
	}
	ev := inputEvent{kind: eventKey, key: KeyEnter}

	quit, absorb := handleInspectorEvent(ev, state, &hub, noop, noop, noop, noop, sendText)

	if quit || absorb {
		t.Fatalf("handleInspectorEvent(Enter, failed send) = (%v,%v), want (false,false) — still composing, nothing to absorb", quit, absorb)
	}
	if !state.inspector.composing || state.inspector.composeText != "hello" {
		t.Fatalf("state after failure = composing=%v text=%q, want composing=true text=hello (preserved for retry)", state.inspector.composing, state.inspector.composeText)
	}
	if state.inspector.composeStatus != "PID 42 is a different session now" {
		t.Fatalf("composeStatus = %q, want the failure message", state.inspector.composeStatus)
	}
}

func TestHandleInspectorEventEscCancelsCompose(t *testing.T) {
	state := &tuiState{mode: screenInspector, inspector: inspectorViewState{
		composing:   true,
		composeText: "hello",
	}}
	var hub *InspectorHub
	noop := func() {}
	ev := inputEvent{kind: eventKey, key: KeyEsc}

	quit, absorb := handleInspectorEvent(ev, state, &hub, noop, noop, noop, noop, nil)

	if quit || !absorb {
		t.Fatalf("handleInspectorEvent(Esc cancel) = (%v,%v), want (false,true)", quit, absorb)
	}
	if state.inspector.composing || state.inspector.composeText != "" {
		t.Fatalf("state after cancel = composing=%v text=%q, want cleared", state.inspector.composing, state.inspector.composeText)
	}
}

func TestHandleInspectorEventCtrlDStillQuitsWhileComposing(t *testing.T) {
	state := &tuiState{mode: screenInspector, inspector: inspectorViewState{composing: true, composeText: "hello"}}
	var hub *InspectorHub
	noop := func() {}
	ev := inputEvent{kind: eventKey, key: "\x04"}

	quit, _ := handleInspectorEvent(ev, state, &hub, noop, noop, noop, noop, nil)

	if !quit {
		t.Fatal("quit = false, want true (Ctrl-D quits even while composing)")
	}
}

func TestHandleInspectorEventPlainCharWhileComposingDoesNotDispatchHotkey(t *testing.T) {
	// 'k' is the kill hotkey outside compose mode. While composing it must be
	// buffered as text, not trigger kill.
	killCalled := false
	state := &tuiState{mode: screenInspector, inspector: inspectorViewState{composing: true}}
	var hub *InspectorHub
	noop := func() {}
	kill := func() { killCalled = true }
	ev := inputEvent{kind: eventKey, key: "k"}

	handleInspectorEvent(ev, state, &hub, noop, noop, noop, kill, nil)

	if killCalled {
		t.Fatal("kill was called for 'k' typed while composing, want buffered as text")
	}
	if state.inspector.composeText != "k" {
		t.Fatalf("composeText = %q, want k", state.inspector.composeText)
	}
}
```

Check `inputEvent`'s exact field names (`kind`, `key`, `mouse`) and `eventKey`/`eventMouse` constant names in tui_events.go before writing these — use whatever the actual names are if they differ from `kind`/`key`.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./... -run TestHandleInspectorEvent -v`
Expected: FAIL — wrong number of arguments / return values for `handleInspectorEvent`.

- [ ] **Step 3: Write the implementation**

Replace `handleInspectorEvent` (tui.go, lines 759-806) with:

```go
// handleInspectorEvent dispatches one decoded event while the inspector screen
// is active. It returns quit=true when the app should exit (Ctrl-C/Ctrl-D
// outside compose mode, or Ctrl-D while composing — composing itself
// intercepts a bare Ctrl-C as cancel, not quit). absorbBatch is true exactly
// when this event ended compose mode (a successful submit or a cancel): the
// caller must then drop any remaining events already queued from the same
// input read, because a paste containing an embedded newline mid-buffer would
// otherwise submit on that newline and let its trailing bytes fall through to
// this screen's normal hotkeys (e.g. 'k' triggering kill) instead of being
// discarded along with the rest of the paste. Back commands close the
// inspector; refresh/follow touch the hub or viewport; scrolling keys and the
// wheel mutate the view and repaint. hubPtr is the loop's inspectorHub
// variable so a Refresh reaches the live hub. Enter attaches to the session
// (mirroring the session-list Enter binding) and closes the inspector — but
// only outside compose mode, where Enter instead submits the compose buffer.
// 'k'/'K' opens the kill confirmation (mirroring the session-list 'k'
// binding) and closes the inspector. sendText performs the actual send
// (local vs. remote is the caller's concern, not this function's) and is
// only ever invoked with a non-empty compose buffer.
func handleInspectorEvent(ev inputEvent, state *tuiState, hubPtr **InspectorHub, closeInspector, render, attach, kill func(), sendText func(Session, string) (bool, string)) (quit, absorbBatch bool) {
	if ev.kind == eventMouse {
		switch state.handleInspectorMouse(ev.mouse) {
		case commandBack:
			closeInspector()
		case commandRefreshInspector:
			if *hubPtr != nil {
				(*hubPtr).Refresh()
			}
		case commandFollowInspector:
			state.inspector.followBottom()
			render()
		case commandRender:
			render()
		}
		return false, false
	}

	if state.inspector.composing {
		if ev.key == "\x04" {
			return true, false
		}
		submit, cancel := state.handleInspectorCompose(ev.key)
		switch {
		case cancel:
			render()
			return false, true
		case submit:
			text := state.inspector.composeText
			sess := state.inspector.snapshot.Session
			state.inspector.composeStatus = "sending…"
			render()
			ok, msg := sendText(sess, text)
			state.inspector.composeStatusUntil = time.Now().Add(4 * time.Second)
			if ok {
				state.inspector.composing = false
				state.inspector.composeText = ""
				state.inspector.composeStatus = "sent"
			} else {
				state.inspector.composeStatus = msg
			}
			render()
			return false, !state.inspector.composing
		default:
			render()
			return false, false
		}
	}

	if ev.key == KeyEnter {
		attach()
		closeInspector()
		return false, false
	}

	if ev.key == "i" && state.inspector.snapshot.Session.Tmux != "" {
		state.inspector.composing = true
		state.inspector.composeText = ""
		state.inspector.composeStatus = ""
		render()
		return false, false
	}

	switch inspectorKeyCommand(ev.key) {
	case commandQuit:
		return true, false
	case commandBack:
		closeInspector()
		return false, false
	}

	switch state.handleInspectorKey(ev.key) {
	case commandBack:
		closeInspector()
	case commandKillInspector:
		kill()
		closeInspector()
	case commandRefreshInspector:
		if *hubPtr != nil {
			(*hubPtr).Refresh()
		}
	case commandFollowInspector:
		state.inspector.followBottom()
		render()
	case commandRender:
		render()
	}
	return false, false
}
```

`tui.go` already imports `"time"` (used elsewhere in the file) — no new import needed.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./... -run TestHandleInspectorEvent -v`
Expected: PASS (all eight cases).

- [ ] **Step 5: Commit**

```bash
git add tui.go tui_test.go
git commit -m "feat: route inspector compose input through handleInspectorEvent"
```

---

### Task 6: Wire the main loop — local/remote dispatch, batch-absorb, status-deadline

**Files:**
- Modify: `tui.go` (RunTUI: add `sendKeysToInspected` closure near `openInspector`/`closeInspector` at lines 421-458; update the `handleInspectorEvent` call site and its batch loop at lines 531-545; generalize the toast-deadline calculation at lines 470-477)

**Interfaces:**
- Consumes: `resolveLivePIDLocal`, `sendKeys` (Task 1), `sendKeysRemote` (Task 3), the new `handleInspectorEvent` signature (Task 5).

This task is glue inside `RunTUI`, which owns the real terminal and is not unit-tested directly in this codebase (existing tests exercise its sub-functions — `handleInspectorKey`, `handleListMouse`, `handleInspectorEvent`, etc. — not the raw event loop itself). Verify with `go build`/`go vet` plus a manual smoke test (Step 4), consistent with how this file's other event-loop changes are checked.

- [ ] **Step 1: Add the dispatch closure**

In tui.go, immediately after `closeInspector` (line 458), add:

```go
	// sendKeysToInspected sends text into the currently-inspected session's
	// tmux pane, dispatching local vs. remote exactly like every other action
	// in this loop (target.Host != ""). Local goes through a fresh
	// resolveLivePIDLocal (send_keys.go) so the pane address is current, not
	// whatever this Session snapshot last polled; remote goes through the
	// authed POST /sessions/{pid}/send-keys endpoint (server.go).
	sendKeysToInspected := func(sess Session, text string) (bool, string) {
		if sess.Host == "" {
			live, err := resolveLivePIDLocal(sess.PID, sess.SessionID)
			if err != nil {
				return false, err.Error()
			}
			if err := sendKeys(live, text); err != nil {
				return false, err.Error()
			}
			return true, ""
		}
		r, err := sendKeysRemote(sess.Host, sess.PID, sess.SessionID, text)
		if err != nil {
			return false, err.Error()
		}
		if !r.OK {
			return false, r.Error
		}
		return true, ""
	}
```

- [ ] **Step 2: Update the call site and absorb the rest of the batch**

Replace the inspector branch of the event loop (tui.go, lines 531-545):

```go
		for _, ev := range events {
			if state.mode == screenInspector {
				quit, absorb := handleInspectorEvent(ev, state, &inspectorHub, closeInspector, render, func() {
					screen.Invalidate()
					actAttach(makeCtx())
					refresh(true)
				}, func() {
					screen.Invalidate()
					actKill(makeCtx())
					refresh(true)
				}, sendKeysToInspected)
				if quit {
					return nil
				}
				if absorb {
					// A paste's trailing bytes after the newline that just
					// submitted (or cancelled) compose mode belong to the
					// compose box, not this screen's hotkeys — drop them
					// rather than let e.g. a trailing 'k' fall through to
					// kill. See handleInspectorEvent's doc comment.
					break
				}
				continue
			}
```

- [ ] **Step 3: Generalize the toast-deadline wait so composeStatus expires on schedule**

Replace the toast-deadline block (tui.go, lines 470-477):

```go
		// While a toast (session-list screen) or a compose-send status
		// (inspector screen) is showing, wake at its deadline so the message
		// clears on time. toastTick marks a wait capped for that reason: its
		// expiry repaints only, leaving the wall-clock cadence untouched.
		statusUntil := toastUntil
		if state.mode == screenInspector {
			statusUntil = state.inspector.composeStatusUntil
		}
		toastTick := false
		if until := time.Until(statusUntil); until > 0 && until < timeout {
			timeout = until
			toastTick = true
		}
```

- [ ] **Step 4: Build, vet, and manually smoke-test**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: all green.

Manual smoke test (both require a real tmux pane, so do this outside the sandboxed test run):
1. `make run` (or `go run .`) inside a tmux session running this tool itself in a second pane.
2. Open the inspector on a local session (`h` or click), press `i`, type a line, press Enter. Confirm the line appears in the target pane and "sent" briefly shows in the inspector's footer.
3. Press `i` again, type a line, press Esc. Confirm nothing was sent and the compose box clears.
4. If a remote host is configured (`servers.yaml`), repeat against a remote session and confirm the same behavior.

- [ ] **Step 5: Commit**

```bash
git add tui.go
git commit -m "feat: wire inspector send-keys into the TUI event loop"
```

---

### Task 7: Render the compose box and send-status

**Files:**
- Modify: `render_inspector.go` (add `time` import; branch `RenderInspector`'s footer row at line 64-65; add `inspectorComposeBar`; update `inspectorFooterRight` at line 205-216)
- Test: `render_inspector_test.go` (create if it doesn't already exist — check first)

**Interfaces:**
- Consumes: `inspectorViewState.composing`/`.composeText`/`.composeStatus`/`.composeStatusUntil` (Task 4).
- Produces: `inspectorComposeBar(w io.Writer, view inspectorViewState, cols int) []hitRegion` (always returns `nil` — no clickable regions while composing).

- [ ] **Step 1: Write the failing tests**

```go
func TestRenderInspectorComposingShowsInputBar(t *testing.T) {
	var buf strings.Builder
	view := inspectorViewState{
		viewportRows: 10,
		composing:    true,
		composeText:  "hello",
		snapshot:     InspectorSnapshot{Session: Session{PID: 1}},
	}
	RenderInspector(&buf, view, 80, 14)
	if !strings.Contains(buf.String(), "> hello") {
		t.Fatalf("output = %q, want it to contain the compose prompt", buf.String())
	}
}

func TestInspectorFooterRightShowsComposeStatusBeforeExpiry(t *testing.T) {
	view := inspectorViewState{
		composeStatus:      "sent",
		composeStatusUntil: time.Now().Add(1 * time.Minute),
	}
	if got := inspectorFooterRight(view); got != "sent" {
		t.Fatalf("inspectorFooterRight = %q, want sent", got)
	}
}

func TestInspectorFooterRightFallsBackAfterExpiry(t *testing.T) {
	view := inspectorViewState{
		composeStatus:      "sent",
		composeStatusUntil: time.Now().Add(-1 * time.Minute),
		follow:             true,
	}
	if got := inspectorFooterRight(view); got != "LIVE ↓" {
		t.Fatalf("inspectorFooterRight = %q, want LIVE ↓ (status expired, fall back to freshness text)", got)
	}
}
```

Check whether `render_inspector_test.go` already exists — if so, add these to it instead of creating a duplicate file.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./... -run 'TestRenderInspectorComposing|TestInspectorFooterRight' -v`
Expected: FAIL — `TestRenderInspectorComposing...` fails because the footer still shows Back/Refresh/Follow, not `"> hello"`; the expiry test fails because `inspectorFooterRight` doesn't check `composeStatusUntil` yet.

- [ ] **Step 3: Write the implementation**

Add `"time"` to render_inspector.go's imports.

Replace the footer line in `RenderInspector` (render_inspector.go, lines 64-65):

```go
	// Row rows-1: footer — the compose input bar while composing, otherwise
	// the normal Back/Refresh/Follow controls.
	if view.composing {
		return inspectorComposeBar(w, view, cols)
	}
	return inspectorFooter(w, view, cols, rows-1)
```

Add near `inspectorFooter` (after line 201):

```go
// inspectorComposeBar draws the send-keys compose row in place of the normal
// footer while view.composing is true: a "> text_" prompt on the same
// reverse-video bar the footer uses, styled after new_picker.go's
// renderPromptInput (new_picker.go:221-230). No hit regions — clicking during
// compose is out of scope; the box only responds to the keyboard.
func inspectorComposeBar(w io.Writer, view inspectorViewState, cols int) []hitRegion {
	line := "> " + view.composeText + dim("_")
	fmt.Fprintln(w, ansiPreviewBar+clipLine(line, cols)+ansiReset)
	return nil
}
```

Replace `inspectorFooterRight` (render_inspector.go, lines 205-216):

```go
// inspectorFooterRight is the source-plus-freshness-status text shown on the
// footer's right when it fits, e.g. "tmux · LIVE ↓". A recent compose-send
// result (composeStatus, while composeStatusUntil hasn't passed) takes
// priority over the normal freshness text, so "sent" or a failure message is
// what the user sees right after pressing Enter — then it falls back to the
// ordinary status once the deadline passes, without needing a separate
// render path.
func inspectorFooterRight(view inspectorViewState) string {
	if view.composeStatus != "" && time.Now().Before(view.composeStatusUntil) {
		return view.composeStatus
	}
	status := inspectorStatusText(view)
	src := view.snapshot.Source
	switch {
	case src != "" && status != "":
		return src + " · " + status
	case status != "":
		return status
	default:
		return src
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./... -run 'TestRenderInspectorComposing|TestInspectorFooterRight' -v`
Expected: PASS.

- [ ] **Step 5: Run the full suite**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: all green.

- [ ] **Step 6: Commit**

```bash
git add render_inspector.go render_inspector_test.go
git commit -m "feat: render inspector compose box and send-status"
```

---

### Task 8: Update README / help text if this repo documents inspector keybindings there

**Files:**
- Modify: `README.md` (only if it already lists inspector keybindings — check first with `grep -n "Follow\|Refresh\|inspector" README.md`)

- [ ] **Step 1: Check whether README.md documents the inspector's keybindings**

Run: `grep -n -i "inspector" README.md`

- [ ] **Step 2: If it does, add `i` to that list**

Add a line for `i` — "compose and send text into the session's tmux pane" — next to the existing entries for `r`/`g`/`G`/`k` in whatever format that section already uses. Match its existing style exactly (don't introduce a new formatting convention for one line).

If README.md doesn't document inspector keybindings at all, skip this task — there's nothing to update.

- [ ] **Step 3: Commit (only if Step 2 made a change)**

```bash
git add README.md
git commit -m "docs: document the inspector's 'i' send-keys binding"
```
