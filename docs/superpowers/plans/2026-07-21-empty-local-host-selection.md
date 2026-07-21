# Selectable Empty Local Host Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** When the local machine has no sessions, show a selectable `(no sessions)` row so pressing `n` creates the first local session.

**Architecture:** Reuse the existing empty-host `selectionTarget` machinery with host `""`. Emit an empty-local target when the local session list is empty, render it via the existing `renderEmptyHostRow` in all three views, and rely on the existing `host==""` action routing (no action code change).

**Tech Stack:** Go, single `main` package. Stdlib + `golang.org/x/term`, `golang.org/x/sys`. Tests via `go test ./...`.

## Global Constraints

- Single Go package `main`; no new dependencies.
- Empty-local host name is the empty string `""`; its selection ID is `emptyHostSelectionID("")` = `"\x00host:"`.
- The empty-local row shows **whenever local is empty**, independent of remote host state.
- No `local:` section heading is added; local rows remain headerless.
- No change to remote empty-host behavior, server API, or config schema.
- Verify with `go test ./...` and `go vet ./...`.

---

### Task 1: Emit empty-local selection target

**Files:**
- Modify: `selection.go:25-42` (`buildSelectionTargets`)
- Test: `selection_test.go`

**Interfaces:**
- Consumes: `emptyHostSelectionTarget(host string) selectionTarget`, `emptyHostSelectionID(host string) string` (existing, selection.go:17-23).
- Produces: `buildSelectionTargets(local []Session, remotes []RemoteResult) []selectionTarget` now yields a single empty-local target (id `"\x00host:"`) as the first element when `len(local)==0`.

- [ ] **Step 1: Write the failing tests**

Add to `selection_test.go`:

```go
func TestBuildSelectionTargetsEmptyLocal(t *testing.T) {
	got := targetIDs(buildSelectionTargets(nil, nil))
	want := []string{emptyHostSelectionID("")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("empty local targets = %q, want %q", got, want)
	}
}

func TestBuildSelectionTargetsEmptyLocalWithRemote(t *testing.T) {
	got := targetIDs(buildSelectionTargets(nil, []RemoteResult{
		{Name: "orca", Sessions: []Session{{PID: 20, Host: "orca"}}},
	}))
	want := []string{emptyHostSelectionID(""), "orca:20"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("empty local + remote targets = %q, want %q", got, want)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./... -run 'TestBuildSelectionTargetsEmptyLocal' -v`
Expected: FAIL — `got []` (no empty-local target emitted yet), want `["\x00host:"]`.

- [ ] **Step 3: Write minimal implementation**

In `selection.go`, insert the empty-local target after the local-session loop and before the remote loop:

```go
func buildSelectionTargets(local []Session, remotes []RemoteResult) []selectionTarget {
	targets := make([]selectionTarget, 0, len(local)+len(remotes))
	for _, session := range local {
		targets = append(targets, sessionSelectionTarget(session))
	}
	if len(local) == 0 {
		targets = append(targets, emptyHostSelectionTarget(""))
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
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./... -run 'TestBuildSelectionTargets' -v`
Expected: PASS for the two new tests **and** the existing `TestBuildSelectionTargets` (one local session ⇒ first id `"10"`, no empty-local target — the regression guard that a populated local host emits no placeholder).

- [ ] **Step 5: Commit**

```bash
git add selection.go selection_test.go
git commit -m "feat: emit selectable empty-local host target"
```

---

### Task 2: Render selectable empty-local row in all three views

**Files:**
- Modify: `render.go` — three identical `rowFn(sectionRows[0])` sites (currently lines 645, 745, 860) in `renderAllFull`, `renderAllIntermediate`, `renderAllMinimal`.
- Test: `render_test.go`

**Interfaces:**
- Consumes: `renderEmptyHostRow(w io.Writer, host, sel string)` (existing, render.go:402); each view func has params `w io.Writer` and `sel string`, and a local `sectionRows [][]drow…` whose index 0 is the local section.
- Produces: local section renders `(no sessions)` with a selection marker when `sel == emptyHostSelectionID("")`.

- [ ] **Step 1: Write the failing tests**

Add to `render_test.go` (mirrors `TestEmptyRemoteHostSelectionMarker`, using `nil` remotes so the only `(no sessions)` row is the local one):

```go
func TestEmptyLocalHostSelectionMarker(t *testing.T) {
	selected := emptyHostSelectionID("")
	for _, mode := range []string{"1", "3", "2"} {
		t.Run(mode, func(t *testing.T) {
			var b strings.Builder
			RenderAll(&b, mode, nil, nil, selected, nil, 0, 0, "dir")
			row := findRow(t, b.String(), "(no sessions)")
			if !strings.HasPrefix(row, "▶ ") {
				t.Fatalf("mode %s empty-local row lacks selection marker: %q", mode, row)
			}
			if !strings.Contains(b.String(), "0 agents, 0 sessions,") {
				t.Fatalf("mode %s empty local changed header counts:\n%s", mode, b.String())
			}
		})
	}
}

func TestEmptyLocalHostUnselectedMarker(t *testing.T) {
	var b strings.Builder
	RenderAll(&b, "1", nil, nil, "", nil, 0, 0, "dir")
	row := findRow(t, b.String(), "(no sessions)")
	if !strings.HasPrefix(row, "  ") || strings.HasPrefix(row, "▶ ") {
		t.Fatalf("unselected empty-local row has wrong marker: %q", row)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./... -run 'TestEmptyLocalHost' -v`
Expected: FAIL — `findRow` cannot find `(no sessions)` (the local section currently renders nothing when empty).

- [ ] **Step 3: Write minimal implementation**

In `render.go`, replace **all three** identical `rowFn(sectionRows[0])` local-section calls with the empty-aware branch. All three view functions use the same `w`, `sel`, and `sectionRows` identifiers, so a single replace-all is exact:

Replace each:

```go
	rowFn(sectionRows[0])
```

with:

```go
	if len(sectionRows[0]) == 0 {
		renderEmptyHostRow(w, "", sel)
	} else {
		rowFn(sectionRows[0])
	}
```

(Editor: `Edit render.go` with `replace_all: true` on the exact string `\trowFn(sectionRows[0])`. Confirm exactly 3 replacements — one per view.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./... -run 'TestEmptyLocalHost|TestEmptyRemoteHost' -v`
Expected: PASS for both new local tests and the existing remote-host render tests (unchanged).

- [ ] **Step 5: Commit**

```bash
git add render.go render_test.go
git commit -m "feat: render selectable empty-local (no sessions) row"
```

---

### Task 3: Confirm empty-local target routes to the local new-session path

**Files:**
- Test: `actions_test.go`
- Modify: none expected — `actNew` (actions.go:195) already routes `host==""` to the local `SpawnNew` path, and `buildCwdPicker` (picker.go:41) is nil-session-safe. This task pins that behavior with tests; add a guard only if a test reveals a deref.

**Interfaces:**
- Consumes: `actCtx.selectedRemoteNewTarget() (host, defaultCWD string, ok bool)` (actions.go:62), `actCtx.selected() *Session` (actions.go:50), `buildCwdPicker(selected *Session) cwdPicker` (picker.go:36).
- Produces: verified contract — empty-local target ⇒ `selectedRemoteNewTarget` returns `ok==false` (local branch) and `selected()` returns `nil` (nil-safe picker input).

- [ ] **Step 1: Write the failing test**

Add to `actions_test.go`:

```go
func TestActNewEmptyLocalTargetRoutesLocal(t *testing.T) {
	target := emptyHostSelectionTarget("")
	c := &actCtx{targets: []selectionTarget{target}, sel: target.id}

	// Empty-local must NOT take the remote-new branch.
	if _, _, ok := c.selectedRemoteNewTarget(); ok {
		t.Fatalf("empty-local target routed to remote new")
	}
	// The local branch feeds c.selected() into buildCwdPicker; it is nil here
	// and must be tolerated without a panic.
	if got := c.selected(); got != nil {
		t.Fatalf("selected() = %#v, want nil for empty-local target", got)
	}
	_ = buildCwdPicker(c.selected())
}
```

Also extend the table in `TestSelectedRemoteNewTarget` with an empty-local case. Add near the other target vars:

```go
	emptyLocal := emptyHostSelectionTarget("")
```

and add this row to the `cases` slice:

```go
		{"empty local host", &emptyLocal, "", "", false},
```

- [ ] **Step 2: Run tests to verify status**

Run: `go test ./... -run 'TestActNewEmptyLocalTargetRoutesLocal|TestSelectedRemoteNewTarget' -v`
Expected: PASS immediately (behavior already correct). If `TestActNewEmptyLocalTargetRoutesLocal` panics or fails, the local path derefs the nil session — proceed to Step 3; otherwise skip to Step 4.

- [ ] **Step 3: Add guard only if Step 2 failed**

If and only if Step 2 revealed a nil deref, add an explicit nil check at the point of the deref (do not change routing). Re-run Step 2 to green. If Step 2 passed, make no code change.

- [ ] **Step 4: Commit**

```bash
git add actions_test.go
git commit -m "test: pin empty-local target to local new-session path"
```

---

### Task 4: Full verification and manual smoke check

**Files:** none (verification only).

- [ ] **Step 1: Run the full suite and vet**

Run:
```bash
go test ./...
go vet ./...
```
Expected: `ok  github.com/rainder/claude-sessions`, no vet output.

- [ ] **Step 2: Build and smoke-check the empty-local case**

Run:
```bash
go build -o /tmp/cs-empty-local .
```
Expected: builds clean. If the current host genuinely has zero local sessions, run `/tmp/cs-empty-local` briefly and confirm a top-of-table `(no sessions)` row appears, arrow keys highlight it (`▶`), and `q` exits. If the host has local sessions, this manual step is not reproducible — note that and rely on the automated render/selection tests instead.

- [ ] **Step 3: Confirm the diff is scoped**

Run: `git diff --stat main`
Expected: only `selection.go`, `selection_test.go`, `render.go`, `render_test.go`, `actions_test.go`, and the two `docs/superpowers/` files. No other files touched.

## Self-Review

- **Spec coverage:** selection-model emit (Task 1) ✓; render in 3 views, no heading (Task 2) ✓; action routing + nil-safety (Task 3) ✓; reconciliation after populate — covered for free by existing `validateTargetSel` NUL-prefix logic (no task needed; asserted by existing `TestValidateTargetSelFollowsPopulatedHost` pattern, host `""` behaves identically); "always when local empty" (Task 1 impl, ungated) ✓; verification (Task 4) ✓.
- **Placeholder scan:** none — all steps carry real code and exact commands.
- **Type consistency:** `emptyHostSelectionTarget("")`, `emptyHostSelectionID("")`, `renderEmptyHostRow(w, "", sel)`, `selectedRemoteNewTarget()`, `buildCwdPicker(*Session)` all match their existing definitions.
