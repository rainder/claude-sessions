# Status Sort Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a persisted TUI status sort ordered waiting, idle, busy, unknown, with recently active sessions first inside each group.

**Architecture:** `SortSessions` gains a small semantic status-rank helper and a stable `status` comparator. Existing sort persistence and cycle infrastructure accept the new mode. Render header helpers expose the active status sort in full/intermediate views as `STATUS▲` and in minimal view as `S▲`, while retaining current directory and age indicators for every existing mode.

**Tech Stack:** Go 1.26.3, standard library only, existing table-driven Go tests.

## Global Constraints

- Semantic order is waiting, idle, busy, unknown.
- Any non-empty `Session.WaitingFor` classifies the session as waiting, regardless of base `Session.Status`.
- `idle` and `busy` matching is case-insensitive.
- Blank and unrecognized statuses sort last without errors.
- Equal status groups sort by `Session.Updated()` descending; exact ties retain stable input order.
- `status` appears immediately after `dir` in the `s`/`S` cycle and has one semantic direction only.
- Local and every remote host section use the same sort behavior.
- Full/intermediate status headers show `STATUS▲`; minimal status header shows `S▲`.
- Existing sort modes, age bases, persistence fallback, dependencies, and session JSON remain unchanged.
- Preserve current uncommitted spawned-session selection and interactive-handoff fixes; do not rewrite or discard their hunks.

---

## File Structure

- `session.go`, `session_test.go`: semantic status rank and in-place stable ordering.
- `config.go`, `config_test.go`: accept persisted `status` mode.
- `tui.go`, `tui_test.go`: cycle position, toast description, and help text.
- `render.go`, `render_test.go`: active status-column indicators in all three views.

---

### Task 1: Add Semantic Status Ordering and Persistence

**Files:**
- Modify: `session.go:168-202`
- Modify: `session_test.go:92-140`
- Modify: `config.go:43-55`
- Modify: `config_test.go:32-39`

**Interfaces:**
- Produce: `sessionStatusRank(s Session) int`
- Extend: `SortSessions(rows []Session, mode string)` with mode `"status"`
- Extend: `LoadSortMode() string` accepted values with `"status"`

- [ ] **Step 1: Write failing status-order tests**

Add focused cases to `session_test.go`:

```go
func TestSortSessionsStatus(t *testing.T) {
	rows := []Session{
		{SessionID: "unknown", Status: "paused", UpdatedAt: 900},
		{SessionID: "busy-old", Status: "BUSY", UpdatedAt: 500},
		{SessionID: "idle-old", Status: "idle", UpdatedAt: 100},
		{SessionID: "waiting", Status: "busy", WaitingFor: "permission prompt", UpdatedAt: 50},
		{SessionID: "idle-new", Status: "IDLE", UpdatedAt: 300},
		{SessionID: "busy-new", Status: "busy", UpdatedAt: 700},
		{SessionID: "blank", UpdatedAt: 1000},
	}

	SortSessions(rows, "status")
	got := make([]string, len(rows))
	for i, s := range rows {
		got[i] = s.SessionID
	}
	want := []string{"waiting", "idle-new", "idle-old", "busy-new", "busy-old", "blank", "unknown"}
	if !equalStrings(got, want) {
		t.Fatalf("status order = %v, want %v", got, want)
	}
}

func TestSortSessionsStatusStable(t *testing.T) {
	rows := []Session{
		{SessionID: "a", Status: "idle", UpdatedAt: 100},
		{SessionID: "b", Status: "IDLE", UpdatedAt: 100},
		{SessionID: "c", Status: "idle", UpdatedAt: 100},
	}
	SortSessions(rows, "status")
	got := []string{rows[0].SessionID, rows[1].SessionID, rows[2].SessionID}
	if want := []string{"a", "b", "c"}; !equalStrings(got, want) {
		t.Fatalf("stable status order = %v, want %v", got, want)
	}
}
```

Update `TestLoadSortModeValid` in `config_test.go` to table-test both `"updated"` and `"status"`:

```go
func TestLoadSortModeValid(t *testing.T) {
	for _, mode := range []string{"updated", "status"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			SaveSortMode(mode)
			if got := LoadSortMode(); got != mode {
				t.Errorf("LoadSortMode() after SaveSortMode = %q, want %q", got, mode)
			}
		})
	}
}
```

- [ ] **Step 2: Run focused tests and confirm RED**

Run:

```bash
go test ./... -run 'TestSortSessionsStatus|TestLoadSortModeValid' -count=1
```

Expected: status sort uses the directory fallback, and persisted `status` reloads as `dir`.

- [ ] **Step 3: Implement semantic rank and comparator**

Update the `SortSessions` contract comment and add:

```go
func sessionStatusRank(s Session) int {
	if s.WaitingFor != "" {
		return 0
	}
	switch strings.ToLower(s.Status) {
	case "idle":
		return 1
	case "busy":
		return 2
	default:
		return 3
	}
}
```

Add this branch before the time-sort branches in `SortSessions`:

```go
case "status":
	sort.SliceStable(rows, func(i, j int) bool {
		ri, rj := sessionStatusRank(rows[i]), sessionStatusRank(rows[j])
		if ri != rj {
			return ri < rj
		}
		return rows[i].Updated().After(rows[j].Updated())
	})
```

Extend `LoadSortMode` documentation and accepted values:

```go
case "dir", "status", "created", "created-asc", "updated", "updated-asc":
	return v
```

- [ ] **Step 4: Run focused and full tests**

Run:

```bash
gofmt -w session.go session_test.go config.go config_test.go
go test ./... -run 'TestSortSessions|TestLoadSortMode' -count=1
go test ./...
```

Expected: all tests pass.

- [ ] **Step 5: Commit**

```bash
git add session.go session_test.go config.go config_test.go
git commit -m "feat: add semantic status sorting" -m "Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 2: Add Status Mode to TUI Cycle and Copy

**Files:**
- Modify: `tui.go:527-559,594-598`
- Modify: `tui_test.go:24-42`

**Interfaces:**
- Extend: `sortModeOrder`
- Extend: `sortDesc(mode string) string`

- [ ] **Step 1: Update cycle tests and add description test**

Replace `TestCycleSortMode` cases with:

```go
cases := []struct {
	mode  string
	delta int
	want  string
}{
	{"dir", 1, "status"},
	{"status", 1, "created"},
	{"created", -1, "status"},
	{"updated-asc", 1, "dir"},
	{"dir", -1, "updated-asc"},
	{"created-asc", -1, "created"},
	{"bogus", 1, "status"},
	{"bogus", -1, "updated-asc"},
}
```

Add:

```go
func TestSortDescStatus(t *testing.T) {
	if got := sortDesc("status"); got != "status (waiting → idle → busy)" {
		t.Fatalf("sortDesc(status) = %q", got)
	}
}
```

- [ ] **Step 2: Run focused tests and confirm RED**

Run:

```bash
go test ./... -run 'TestCycleSortMode|TestSortDescStatus' -count=1
```

Expected: `dir` still cycles to `created`, and status description falls through to directory text.

- [ ] **Step 3: Implement cycle, description, and help text**

Change:

```go
var sortModeOrder = []string{"dir", "status", "created", "created-asc", "updated", "updated-asc"}
```

Add to `sortDesc`:

```go
case "status":
	return "status (waiting → idle → busy)"
```

Change help copy to:

```go
fmt.Println("    s / S        cycle sort forward / back (dir → status → created → updated, +asc)")
```

- [ ] **Step 4: Run focused and full tests**

Run:

```bash
gofmt -w tui.go tui_test.go
go test ./... -run 'TestCycleSortMode|TestSortDescStatus' -count=1
go test ./...
```

Expected: all tests pass.

- [ ] **Step 5: Commit**

```bash
git add tui.go tui_test.go
git commit -m "feat: add status sort to TUI cycle" -m "Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 3: Mark Active Status Sort in Every Table View

**Files:**
- Modify: `render.go:524-537,627-648,745-760,872-918`
- Modify: `render_test.go:487-519`

**Interfaces:**
- Change: `sortLabels(sortMode string) (dirLabel, statusLabel, ageLabel string)`
- Produce: `minimalStatusLabel(sortMode string) string`

- [ ] **Step 1: Write failing header-indicator tests**

Extend `TestSortIndicator` inside its view loop:

```go
out := renderWith(view, "status")
if view == "2" {
	if !strings.Contains(out, "S▲") {
		t.Errorf("view %s status: want S▲:\n%s", view, out)
	}
} else if !strings.Contains(out, "STATUS▲") {
	t.Errorf("view %s status: want STATUS▲:\n%s", view, out)
}
if strings.Contains(out, "DIR▲") || strings.Contains(out, "AGE▲") || strings.Contains(out, "AGE▼") {
	t.Errorf("view %s status: only status column should carry arrow:\n%s", view, out)
}
```

Keep all existing directory/time indicator and age-basis assertions.

- [ ] **Step 2: Run render test and confirm RED**

Run:

```bash
go test ./... -run TestSortIndicator -count=1
```

Expected: status mode falls back to `DIR▲`; no status header arrow appears.

- [ ] **Step 3: Extend header-label helpers**

Replace `sortLabels` with:

```go
func sortLabels(sortMode string) (dirLabel, statusLabel, ageLabel string) {
	switch sortMode {
	case "status":
		return "DIR", "STATUS▲", "AGE"
	case "created", "updated":
		return "DIR", "STATUS", "AGE▼"
	case "created-asc", "updated-asc":
		return "DIR", "STATUS", "AGE▲"
	default:
		return "DIR▲", "STATUS", "AGE"
	}
}

func minimalStatusLabel(sortMode string) string {
	if sortMode == "status" {
		return "S▲"
	}
	return "S"
}
```

Update the full and intermediate renderers to read all three labels, seed `statusW` from `statusLabel`, and print `statusLabel` instead of the literal `"STATUS"`:

```go
dirLabel, statusLabel, ageLabel := sortLabels(sortMode)
// ...
statusW := utf8.RuneCountInString(statusLabel)
// ...
statusW, statusLabel
```

Preserve their existing row-width calculations and all other headers.

- [ ] **Step 4: Update minimal header and row width**

In `renderAllMinimal`:

```go
dirLabel, _, ageLabel := sortLabels(sortMode)
statusLabel := minimalStatusLabel(sortMode)
statusW := utf8.RuneCountInString(statusLabel)
```

Build the header with a width-aware status column:

```go
return fmt.Sprintf("  %-*s  %-*s  %-*s  %5s",
	dirW, dirLabel, nameW, "NAME", statusW, statusLabel, ageLabel)
```

Pad the one-cell status glyph before optional colorization so data rows remain aligned when `S▲` makes the header two cells wide:

```go
statusCell := glyph + strings.Repeat(" ", statusW-1)
if !ghost {
	statusCell = colorize(statusColor[r.s.Status], statusCell)
}
```

Keep the row format otherwise unchanged.

- [ ] **Step 5: Run render and full tests**

Run:

```bash
gofmt -w render.go render_test.go
go test ./... -run TestSortIndicator -count=1
go test ./...
```

Expected: all three views show the status indicator only on their status column, and every existing mode remains green.

- [ ] **Step 6: Commit**

```bash
git add render.go render_test.go
git commit -m "feat: show active status sort in headers" -m "Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 4: Verify Combined Branch and Install

**Files:** All modified Go/test files plus existing branch commits.

**Interfaces:** Verification and delivery gate only.

- [ ] **Step 1: Check formatting, stale labels, and scope**

Run:

```bash
gofmt -w *.go
rg -n 'sortModeOrder|status \(waiting → idle → busy\)|STATUS▲|S▲' --glob '*.go'
git diff --check
git status --short
```

Expected: status mode appears in cycle/copy/render/tests; no formatting errors. Existing spawned-selection and interactive-handoff changes remain present unless already committed.

- [ ] **Step 2: Run independent verification**

Run:

```bash
go test ./...
go vet ./...
go build ./...
```

Expected: all commands exit 0.

- [ ] **Step 3: Review branch behavior**

Confirm:

- Waiting classification uses `WaitingFor`, not display-string parsing.
- Unknown status values sort last.
- Within-group order uses `Updated()` descending and remains stable on ties.
- Local and copied remote sections both flow through `SortSessions`.
- Status persistence accepts `status`; unknown persisted values still return `dir`.
- `s`/`S` cycle wraps correctly with status after directory.
- Full/intermediate/minimal headers show only the active column arrow.
- Current spawned-session selection and interactive handoff fixes remain intact.
- No dependency changes.

- [ ] **Step 4: Install and smoke-test**

Run:

```bash
make install
$HOME/.local/bin/claude-sessions --once
```

Expected: install succeeds and one-shot output renders normally.

- [ ] **Step 5: Finish requested delivery**

After final review, use `superpowers:finishing-a-development-branch` with the user's already-explicit choice: merge `fix/post-spawn-selection` into `main`, rerun `go test ./...`, push `origin/main`, then delete the local feature branch.
