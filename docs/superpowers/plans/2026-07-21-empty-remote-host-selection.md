# Selectable Empty Remote Hosts Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make a reachable remote host with zero sessions selectable so pressing `n` creates its first remote session.

**Architecture:** Add a focused `selectionTarget` model separate from `Session`. Navigation and action routing consume targets, while rendering keeps concrete sessions and uses a collision-proof empty-host target ID only to draw the cursor on `(no sessions)`.

**Tech Stack:** Go, standard library, existing `golang.org/x/term`, package-local unit tests.

## Global Constraints

- Preserve all unrelated uncommitted changes in the current working tree.
- Do not commit unless the user explicitly requests a commit.
- Add no dependencies; single-binary deployment remains unchanged.
- Keep loading and errored remote hosts unselectable.
- Never represent an empty host as `Session{PID: 0}`.
- `kill`, `attach`, and `preview` must remain process-session actions only.
- Empty-host remote creation must accept a remote path without local filesystem validation.
- Preserve existing local creation, populated-remote creation, API, SSH, sorting, counts, and view-mode behavior.

## File Structure

- Create `selection.go`: selection target type, target construction, navigation, and refresh reconciliation.
- Create `selection_test.go`: target construction, navigation, collision namespace, and empty-to-populated reconciliation tests.
- Modify `actions.go`: action context resolves targets and derives remote-new destination without a fake session.
- Create `actions_test.go`: action-resolution and routing tests for local, populated remote, and empty remote targets.
- Modify `remote_actions.go`: `actNewRemote` accepts explicit host and default CWD.
- Modify `tui.go`: build targets during refresh and use them for navigation/action context.
- Modify `remote.go`: remove obsolete `AllSessions` session-only flattening helper.
- Modify `render.go`: render cursor marker on selected empty-host rows in all three modes.
- Modify `render_test.go`: verify marker and unchanged header counts in every mode.

---

### Task 1: Add Selection Target Model

**Files:**
- Create: `selection.go`
- Create: `selection_test.go`

**Interfaces:**
- Consumes: `Session`, `RemoteResult`, and `Session.ID()`.
- Produces:
  - `type selectionTarget struct { id string; host string; session *Session }`
  - `func emptyHostSelectionID(host string) string`
  - `func emptyHostSelectionTarget(host string) selectionTarget`
  - `func sessionSelectionTarget(s Session) selectionTarget`
  - `func buildSelectionTargets(local []Session, remotes []RemoteResult) []selectionTarget`
  - `func navTargets(targets []selectionTarget, sel string, delta int) string`
  - `func validateTargetSel(targets []selectionTarget, sel string) string`

- [ ] **Step 1: Write failing selection tests**

Create `selection_test.go`:

```go
package main

import (
    "reflect"
    "strings"
    "testing"
)

func targetIDs(targets []selectionTarget) []string {
    ids := make([]string, len(targets))
    for i, target := range targets {
        ids[i] = target.id
    }
    return ids
}

func TestBuildSelectionTargets(t *testing.T) {
    local := []Session{{PID: 10, CWD: "/local"}}
    remotes := []RemoteResult{
        {Name: "beluga"},
        {Name: "loading", Loading: true},
        {Name: "broken", Error: "connection refused"},
        {Name: "orca", Sessions: []Session{{PID: 20, Host: "orca", CWD: "/remote"}}},
        {Name: "narwhal"},
    }

    got := targetIDs(buildSelectionTargets(local, remotes))
    want := []string{
        "10",
        emptyHostSelectionID("beluga"),
        "orca:20",
        emptyHostSelectionID("narwhal"),
    }
    if !reflect.DeepEqual(got, want) {
        t.Fatalf("target IDs = %q, want %q", got, want)
    }
}

func TestEmptyHostSelectionIDUsesReservedNamespace(t *testing.T) {
    id := emptyHostSelectionID("42")
    if !strings.HasPrefix(id, "\x00host:") {
        t.Fatalf("empty-host ID %q lacks reserved prefix", id)
    }
    if id == "42" || id == "host:42" {
        t.Fatalf("empty-host ID %q can collide with a session ID", id)
    }
}

func TestNavTargetsIncludesEmptyHostsAndWraps(t *testing.T) {
    targets := buildSelectionTargets(
        []Session{{PID: 10}},
        []RemoteResult{{Name: "beluga"}, {Name: "narwhal"}},
    )

    beluga := emptyHostSelectionID("beluga")
    narwhal := emptyHostSelectionID("narwhal")
    cases := []struct {
        name  string
        sel   string
        delta int
        want  string
    }{
        {"down enters empty host", "10", 1, beluga},
        {"down enters next empty host", beluga, 1, narwhal},
        {"down wraps", narwhal, 1, "10"},
        {"up wraps", "10", -1, narwhal},
        {"empty selection down", "", 1, "10"},
        {"empty selection up", "", -1, narwhal},
    }
    for _, tc := range cases {
        t.Run(tc.name, func(t *testing.T) {
            if got := navTargets(targets, tc.sel, tc.delta); got != tc.want {
                t.Fatalf("navTargets(%q, %d) = %q, want %q", tc.sel, tc.delta, got, tc.want)
            }
        })
    }
}

func TestValidateTargetSelFollowsPopulatedHost(t *testing.T) {
    targets := buildSelectionTargets(nil, []RemoteResult{
        {Name: "beluga", Sessions: []Session{
            {PID: 30, Host: "beluga"},
            {PID: 31, Host: "beluga"},
        }},
    })

    got := validateTargetSel(targets, emptyHostSelectionID("beluga"))
    if got != "beluga:30" {
        t.Fatalf("validateTargetSel followed empty host to %q, want %q", got, "beluga:30")
    }
}

func TestValidateTargetSelUsesExistingFallbackForOtherMissingRows(t *testing.T) {
    targets := buildSelectionTargets([]Session{{PID: 10}, {PID: 11}}, nil)
    if got := validateTargetSel(targets, "999"); got != "10" {
        t.Fatalf("validateTargetSel missing session = %q, want first target", got)
    }
    if got := validateTargetSel(nil, "999"); got != "" {
        t.Fatalf("validateTargetSel empty targets = %q, want empty", got)
    }
}
```

- [ ] **Step 2: Run tests and verify expected compile failure**

Run:

```sh
go test ./... -run 'Test(BuildSelectionTargets|EmptyHostSelectionID|NavTargets|ValidateTargetSel)'
```

Expected: FAIL because `selectionTarget`, `buildSelectionTargets`, `navTargets`, and `validateTargetSel` are undefined.

- [ ] **Step 3: Implement selection target model**

Create `selection.go`:

```go
package main

import "strings"

const emptyHostSelectionPrefix = "\x00host:"

type selectionTarget struct {
    id      string
    host    string
    session *Session
}

func sessionSelectionTarget(s Session) selectionTarget {
    return selectionTarget{id: s.ID(), host: s.Host, session: &s}
}

func emptyHostSelectionID(host string) string {
    return emptyHostSelectionPrefix + host
}

func emptyHostSelectionTarget(host string) selectionTarget {
    return selectionTarget{id: emptyHostSelectionID(host), host: host}
}

func buildSelectionTargets(local []Session, remotes []RemoteResult) []selectionTarget {
    targets := make([]selectionTarget, 0, len(local)+len(remotes))
    for _, session := range local {
        targets = append(targets, sessionSelectionTarget(session))
    }
    for _, remote := range remotes {
        if len(remote.Sessions) > 0 {
            for _, session := range remote.Sessions {
                targets = append(targets, sessionSelectionTarget(session))
            }
            continue
        }
        if !remote.Loading && remote.Error == "" {
            targets = append(targets, emptyHostSelectionTarget(remote.Name))
        }
    }
    return targets
}

func navTargets(targets []selectionTarget, sel string, delta int) string {
    n := len(targets)
    if n == 0 {
        return ""
    }
    if sel == "" {
        if delta > 0 {
            return targets[0].id
        }
        return targets[n-1].id
    }
    for i, target := range targets {
        if target.id == sel {
            next := ((i+delta)%n + n) % n
            return targets[next].id
        }
    }
    return targets[0].id
}

func validateTargetSel(targets []selectionTarget, sel string) string {
    for _, target := range targets {
        if target.id == sel {
            return sel
        }
    }
    if strings.HasPrefix(sel, emptyHostSelectionPrefix) {
        host := strings.TrimPrefix(sel, emptyHostSelectionPrefix)
        for _, target := range targets {
            if target.session != nil && target.session.Host == host {
                return target.id
            }
        }
    }
    if len(targets) > 0 {
        return targets[0].id
    }
    return ""
}
```

- [ ] **Step 4: Run focused selection tests**

Run:

```sh
gofmt -w selection.go selection_test.go
go test ./... -run 'Test(BuildSelectionTargets|EmptyHostSelectionID|NavTargets|ValidateTargetSel)'
```

Expected: PASS.

- [ ] **Step 5: Review task diff without staging unrelated files**

Run:

```sh
git diff -- selection.go selection_test.go
```

Expected: only selection model and tests shown. Do not commit.

---

### Task 2: Route Actions Through Selection Targets

**Files:**
- Modify: `actions.go:11-46,167-175`
- Modify: `remote_actions.go:190-198`
- Create: `actions_test.go`

**Interfaces:**
- Consumes: `selectionTarget` and constructors from Task 1.
- Produces:
  - `actCtx.targets []selectionTarget`
  - `func (c *actCtx) selectedTarget() *selectionTarget`
  - existing `func (c *actCtx) selected() *Session`, now target-backed
  - `func (c *actCtx) selectedRemoteNewTarget() (host, defaultCWD string, ok bool)`
  - `func actNewRemote(c *actCtx, host, defaultCWD string)`

- [ ] **Step 1: Write failing action-resolution tests**

Create `actions_test.go`:

```go
package main

import (
    "testing"
    "time"
)

func TestActCtxEmptyHostSelectionIsNotSession(t *testing.T) {
    target := emptyHostSelectionTarget("beluga")
    c := &actCtx{targets: []selectionTarget{target}, sel: target.id}

    if got := c.selectedTarget(); got == nil || got.host != "beluga" {
        t.Fatalf("selectedTarget() = %#v, want beluga target", got)
    }
    if got := c.selected(); got != nil {
        t.Fatalf("selected() = %#v, want nil for empty host", got)
    }
}

func TestSelectedRemoteNewTarget(t *testing.T) {
    local := sessionSelectionTarget(Session{PID: 10, CWD: "/local"})
    remote := sessionSelectionTarget(Session{PID: 20, Host: "orca", CWD: "/remote"})
    empty := emptyHostSelectionTarget("beluga")

    cases := []struct {
        name       string
        target     *selectionTarget
        wantHost   string
        wantCWD    string
        wantRemote bool
    }{
        {"no selection", nil, "", "", false},
        {"local session", &local, "", "", false},
        {"remote session", &remote, "orca", "/remote", true},
        {"empty remote host", &empty, "beluga", "", true},
    }
    for _, tc := range cases {
        t.Run(tc.name, func(t *testing.T) {
            c := &actCtx{}
            if tc.target != nil {
                c.targets = []selectionTarget{*tc.target}
                c.sel = tc.target.id
            }
            host, cwd, ok := c.selectedRemoteNewTarget()
            if host != tc.wantHost || cwd != tc.wantCWD || ok != tc.wantRemote {
                t.Fatalf("selectedRemoteNewTarget() = (%q, %q, %v), want (%q, %q, %v)",
                    host, cwd, ok, tc.wantHost, tc.wantCWD, tc.wantRemote)
            }
        })
    }
}

func TestSessionActionsIgnoreEmptyHostTarget(t *testing.T) {
    target := emptyHostSelectionTarget("beluga")
    c := &actCtx{targets: []selectionTarget{target}, sel: target.id}

    actKill(c)
    actAttach(c)
    actPreview(c, time.Millisecond)

    if got := c.selected(); got != nil {
        t.Fatalf("session-only actions resolved empty host as %#v", got)
    }
}
```

- [ ] **Step 2: Run tests and verify expected compile failure**

Run:

```sh
go test ./... -run 'Test(ActCtxEmptyHostSelection|SelectedRemoteNewTarget|SessionActionsIgnoreEmptyHostTarget)'
```

Expected: FAIL because `actCtx.targets`, `selectedTarget`, and `selectedRemoteNewTarget` do not exist.

- [ ] **Step 3: Make `actCtx` target-backed**

In `actions.go`, replace the `sessions` field and `selected` implementation with:

```go
type actCtx struct {
    fd       int
    oldState *term.State
    targets  []selectionTarget
    sel      string

    pause  func()
    resume func()
}

func (c *actCtx) selectedTarget() *selectionTarget {
    for i := range c.targets {
        if c.targets[i].id == c.sel {
            return &c.targets[i]
        }
    }
    return nil
}

func (c *actCtx) selected() *Session {
    target := c.selectedTarget()
    if target == nil {
        return nil
    }
    return target.session
}

func (c *actCtx) selectedRemoteNewTarget() (host, defaultCWD string, ok bool) {
    target := c.selectedTarget()
    if target == nil || target.host == "" {
        return "", "", false
    }
    if target.session != nil {
        defaultCWD = target.session.CWD
    }
    return target.host, defaultCWD, true
}
```

Keep existing pause/resume comments and `runInteractive` behavior unchanged.

- [ ] **Step 4: Route `actNew` through explicit remote target data**

Replace the opening branch in `actNew` with:

```go
func actNew(c *actCtx) {
    if host, defaultCWD, ok := c.selectedRemoteNewTarget(); ok {
        actNewRemote(c, host, defaultCWD)
        return
    }
    picker := buildCwdPicker(c.selected())
```

Leave the remaining local picker, local directory validation, spawn, and attach code unchanged.

- [ ] **Step 5: Refactor remote-new signature**

In `remote_actions.go`, replace the function header and selected-session extraction with:

```go
// actNewRemote prompts for a cwd and POSTs /sessions/new to the named remote
// server. A populated remote row supplies defaultCWD; an empty host does not.
func actNewRemote(c *actCtx, host, defaultCWD string) {
    enterCooked(c.fd, c.oldState)
    defer enterRaw(c.fd)
```

Keep the prompt, JSON request, error handling, `LookupServer(host)`, and SSH/tmux attach code unchanged. The existing empty-input branch already emits `no cwd` when `defaultCWD == ""`; do not add local `isDir` validation.

- [ ] **Step 6: Run focused action tests**

Run:

```sh
gofmt -w actions.go actions_test.go remote_actions.go
go test ./... -run 'Test(ActCtxEmptyHostSelection|SelectedRemoteNewTarget|SessionActionsIgnoreEmptyHostTarget)'
```

Expected: PASS.

- [ ] **Step 7: Review task diff without staging unrelated files**

Run:

```sh
git diff -- actions.go actions_test.go remote_actions.go
```

Expected: target resolution and remote-new signature changes only. Do not commit.

---

### Task 3: Integrate Targets Into TUI and Rendering

**Files:**
- Modify: `tui.go:132-166,190-260,319-324`
- Modify: `remote.go:88-98`
- Modify: `render.go:636-648,735-747,850-862`
- Modify: `render_test.go`
- Test: `selection_test.go`

**Interfaces:**
- Consumes: `buildSelectionTargets`, `navTargets`, `validateTargetSel`, `emptyHostSelectionID`, and target-backed `actCtx`.
- Produces: one shared target snapshot per TUI refresh and selected empty-host rendering in all view modes.

- [ ] **Step 1: Write failing render tests**

Append to `render_test.go`:

```go
func TestEmptyRemoteHostSelectionMarker(t *testing.T) {
    remotes := []RemoteResult{{Name: "beluga"}}
    selected := emptyHostSelectionID("beluga")

    for _, mode := range []string{"1", "3", "2"} {
        t.Run(mode, func(t *testing.T) {
            var b strings.Builder
            RenderAll(&b, mode, nil, remotes, selected, nil, 0, 0, "dir")
            row := findRow(t, b.String(), "(no sessions)")
            if !strings.HasPrefix(row, "▶ ") {
                t.Fatalf("mode %s empty-host row lacks selection marker: %q", mode, row)
            }
            if !strings.Contains(b.String(), "0 agents, 0 sessions,") {
                t.Fatalf("mode %s empty host changed header counts:\n%s", mode, b.String())
            }
        })
    }
}

func TestEmptyRemoteHostUnselectedMarker(t *testing.T) {
    var b strings.Builder
    RenderAll(&b, "1", nil, []RemoteResult{{Name: "beluga"}}, "", nil, 0, 0, "dir")
    row := findRow(t, b.String(), "(no sessions)")
    if !strings.HasPrefix(row, "  ") || strings.HasPrefix(row, "▶ ") {
        t.Fatalf("unselected empty-host row has wrong marker: %q", row)
    }
}
```

- [ ] **Step 2: Run render tests and verify marker failure**

Run:

```sh
go test ./... -run 'TestEmptyRemoteHost(SelectionMarker|UnselectedMarker)'
```

Expected: `TestEmptyRemoteHostSelectionMarker` FAILS because current `(no sessions)` branches always start with two spaces. Unselected test may already pass.

- [ ] **Step 3: Add one empty-host row renderer**

Add near `buildSections` in `render.go`:

```go
func renderEmptyHostRow(w io.Writer, host, sel string) {
    marker := "  "
    if sel == emptyHostSelectionID(host) {
        marker = "▶ "
    }
    fmt.Fprintln(w, marker+dim("(no sessions)"))
}
```

Replace each of the three empty-section branches:

```go
case len(sectionRows[i]) == 0:
    renderEmptyHostRow(w, sections[i].label, sel)
```

Do not change loading or unreachable rendering.

- [ ] **Step 4: Replace session-only TUI selection state**

In `RunTUI`, add target state beside `local`, `remotes`, and `sel`:

```go
var local []Session
var remotes []RemoteResult
var targets []selectionTarget
sel := ""
```

At the end of `refresh`, replace `validateSel(AllSessions(...))` with:

```go
targets = buildSelectionTargets(local, remotes)
sel = validateTargetSel(targets, sel)
```

In `makeCtx`, replace `sessions: AllSessions(local, remotes)` with:

```go
targets: targets,
```

Replace both navigation cases with:

```go
case KeyUp:
    sel = navTargets(targets, sel, -1)
    render()
case KeyDown:
    sel = navTargets(targets, sel, 1)
    render()
```

Delete the old session-only `nav` and `validateSel` functions from `tui.go`.

- [ ] **Step 5: Remove obsolete flattening helper**

Delete `AllSessions` and its comment from `remote.go:88-98`. Confirm no remaining call sites:

```sh
rg 'AllSessions|validateSel|nav\(' --glob '*.go'
```

Expected: no `AllSessions` or `validateSel` matches; `navTargets` matches remain. Other unrelated words containing `nav` may appear.

- [ ] **Step 6: Run focused selection and rendering tests**

Run:

```sh
gofmt -w tui.go remote.go render.go render_test.go
go test ./... -run 'Test(BuildSelectionTargets|EmptyHostSelectionID|NavTargets|ValidateTargetSel|EmptyRemoteHost)'
```

Expected: PASS.

- [ ] **Step 7: Run action regression tests after TUI integration**

Run:

```sh
go test ./... -run 'Test(ActCtxEmptyHostSelection|SelectedRemoteNewTarget|SessionActionsIgnoreEmptyHostTarget|CycleSortMode)'
```

Expected: PASS.

- [ ] **Step 8: Review integration diff without staging unrelated files**

Run:

```sh
git diff -- selection.go selection_test.go actions.go actions_test.go remote_actions.go tui.go remote.go render.go render_test.go
```

Expected: only approved empty-host selection changes plus pre-existing edits already present in those files. Compare carefully rather than reverting unrelated hunks. Do not commit.

---

### Task 4: Full Verification and Manual TUI Check

**Files:**
- Verify only; no planned code changes.

**Interfaces:**
- Consumes: completed Tasks 1-3.
- Produces: evidence that tests, vet, build, and real TUI interaction all work.

- [ ] **Step 1: Run full unit test suite**

Run:

```sh
go test ./...
```

Expected: PASS with no failing packages.

- [ ] **Step 2: Run static analysis**

Run:

```sh
go vet ./...
```

Expected: exit status 0 with no diagnostics.

- [ ] **Step 3: Build binary**

Run:

```sh
go build .
```

Expected: exit status 0.

- [ ] **Step 4: Inspect final scoped diff and status**

Run:

```sh
git status --short
git diff --check
git diff -- selection.go selection_test.go actions.go actions_test.go remote_actions.go tui.go remote.go render.go render_test.go docs/superpowers/specs/2026-07-21-empty-remote-host-selection-design.md docs/superpowers/plans/2026-07-21-empty-remote-host-selection.md
```

Expected: `git diff --check` exits 0. Existing unrelated modified/untracked files remain intact. No files are staged or committed.

- [ ] **Step 5: Run real TUI against an empty reachable remote**

Launch through the project run workflow or `go run .`, then verify:

1. Arrow keys move cursor onto beluga's `(no sessions)` line.
2. `k`, `a`, and `p` on that line do nothing and do not prompt.
3. `n` shows `New tmux+claude session on beluga`.
4. Empty Enter reports `no cwd`; entered path is sent to beluga without local path validation.
5. Successful creation attaches through SSH/tmux.
6. After detaching and refresh, cursor remains in beluga section on its first session.
7. Local `n` and populated-remote `n` retain existing behavior.

Expected: all seven checks pass.

- [ ] **Step 6: Report outcome without committing**

Report changed files, test/vet/build results, manual-check result, and any skipped check. Do not claim manual verification if no reachable empty remote was available.
