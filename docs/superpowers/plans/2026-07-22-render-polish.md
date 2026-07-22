# Render Polish Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make selected rows use a subtle background, add one right-padding cell, and place STATUS before MODEL in intermediate view.

**Architecture:** Keep changes inside existing table renderer. Add one ANSI selected-background constant and make `highlightSelectedRow` reapply that background after nested resets, allowing existing status/model/name foreground styles to survive. Adjust format strings for three table modes and lock behavior with focused render tests.

**Tech Stack:** Go standard library, ANSI SGR terminal escapes, existing Go test suite.

## Global Constraints

- Use ANSI 256-color dark-gray background `48;5;236`; do not use truecolor or reverse video.
- Preserve existing foreground colors and text styling inside selected rows.
- Add exactly one visible trailing space to full, intermediate, and minimal table headers and session rows.
- Keep trailing padding inside selected-row background.
- Intermediate order must be `NAME DIR STATUS MODEL COST AGENTS CTX CPU AGE`.
- Full and minimal column order must remain unchanged.
- No session data, sorting, input, viewport, remote behavior, or dependency changes.

---

### Task 1: Selected Background and Foreground Preservation

**Files:**
- Modify: `render.go:14-20,75-80,772-816,886-920,1009-1040`
- Test: `render_test.go:90-221`

**Interfaces:**
- Consumes: existing `colorize(code, s string) string`, `highlightSelectedRow(row string, selected bool) string`, and per-mode `plainCells` rendering paths.
- Produces: `ansiSelectedBG string` and updated `highlightSelectedRow` behavior; signature remains `func highlightSelectedRow(row string, selected bool) string`.

- [ ] **Step 1: Write failing selected-background tests**

Replace reverse-video expectations and add foreground-preservation coverage:

```go
func TestHighlightSelectedRow(t *testing.T) {
	if got := highlightSelectedRow("2 row", false); got != "2 row" {
		t.Errorf("unselected row = %q, want unchanged", got)
	}
	want := ansiSelectedBG + "2 row" + ansiReset
	if got := highlightSelectedRow("2 row", true); got != want {
		t.Errorf("selected row = %q, want %q", got, want)
	}
}

func TestHighlightSelectedRowReappliesBackgroundAfterNestedReset(t *testing.T) {
	row := "plain " + colorize("1;31", "busy") + " tail"
	want := ansiSelectedBG + "plain " +
		"\033[1;31mbusy" + ansiReset + ansiSelectedBG +
		" tail" + ansiReset
	if got := highlightSelectedRow(row, true); got != want {
		t.Fatalf("selected styled row = %q, want %q", got, want)
	}
}
```

Rename `assertWholeRowSelected` checks from inverse-specific parsing to selected-background parsing:

```go
func assertWholeRowSelected(t *testing.T, row, prefix string) {
	t.Helper()
	if !strings.HasPrefix(row, ansiSelectedBG+prefix) {
		t.Fatalf("selected row lacks background prefix %q: %q", prefix, row)
	}
	if !strings.HasSuffix(row, ansiReset) {
		t.Fatalf("selected row lacks final reset: %q", row)
	}
	if strings.Contains(row, ansiInvert) {
		t.Fatalf("selected row still uses reverse video: %q", row)
	}
	if strings.Contains(row, "▶") {
		t.Fatalf("selected row still contains arrow: %q", row)
	}
}
```

Rename `TestSelectedSessionRowsInvertWholeRow` to `TestSelectedSessionRowsHighlightWholeRow`.

Add normal selected-row foreground assertion:

```go
func TestSelectedSessionRowPreservesStatusColor(t *testing.T) {
	s := Session{
		PID: 42, Name: "selected", NameSource: "user", CWD: "/work/selected",
		Status: "busy", Entrypoint: "cli", UpdatedAt: time.Now().UnixMilli(),
	}
	row := renderSessionRowForTest(t, "3", s, true)
	if !strings.Contains(row, "\033[1;31m") {
		t.Fatalf("selected row lost busy foreground color: %q", row)
	}
	if !strings.Contains(row, ansiReset+ansiSelectedBG) {
		t.Fatalf("selected row does not restore background after status reset: %q", row)
	}
}
```

- [ ] **Step 2: Run focused tests and verify failure**

Run:

```bash
go test ./... -run 'TestHighlightSelectedRow|TestSelectedSessionRowsHighlightWholeRow|TestSelectedSessionRowPreservesStatusColor'
```

Expected: compile failure for undefined `ansiSelectedBG`, then assertion failures until renderer changes land.

- [ ] **Step 3: Implement selected background**

Change ANSI constants and selection wrapper:

```go
const (
	ansiReset      = "\033[0m"
	ansiBold       = "\033[1m"
	ansiDim        = "\033[2m"
	ansiInvert     = "\033[7m"
	ansiSelectedBG = "\033[48;5;236m"
)

func highlightSelectedRow(row string, selected bool) string {
	if !selected {
		return row
	}
	row = strings.ReplaceAll(row, ansiReset, ansiReset+ansiSelectedBG)
	return ansiSelectedBG + row + ansiReset
}
```

Keep `ansiInvert` because tests explicitly guard against its use and other code may still reference it. In full and intermediate renderers, change:

```go
plainCells := selected || ghost
```

to:

```go
plainCells := ghost
```

This restores status, model, tmux, context, and derived-name foreground styling for selected normal rows. Leave minimal renderer unchanged at this step only if its status foreground is separately restored in Task 2; preferred implementation changes its `plainCells` to `ghost` now too. Selected headless rows still suppress nested styling because `ghost` remains true.

- [ ] **Step 4: Run focused tests and verify pass**

Run:

```bash
go test ./... -run 'TestHighlightSelectedRow|TestSelectedSessionRowsHighlightWholeRow|TestSelectedSessionRowPreservesStatusColor|TestSelectedHeadlessRowsSuppressDim'
```

Expected: PASS.

- [ ] **Step 5: Commit selected-background change**

```bash
git add render.go render_test.go
git commit -m "style: soften selected row highlight"
```

---

### Task 2: Right Padding and Intermediate Column Order

**Files:**
- Modify: `render.go:759-814,874-918,998-1038`
- Test: `render_test.go`

**Interfaces:**
- Consumes: existing `RenderAll`, `visualLen`, `findRow`, and table-mode formatters.
- Produces: padded full/intermediate/minimal headers and rows; intermediate STATUS-before-MODEL order.

- [ ] **Step 1: Write failing trailing-padding tests**

Add helper and table-mode test:

```go
func stripTrailingReset(s string) string {
	return strings.TrimSuffix(s, ansiReset)
}

func TestSessionRowsHaveOneRightPaddingSpace(t *testing.T) {
	s := Session{
		PID: 42, Name: "padding", NameSource: "user", CWD: "/work/padding",
		Model: "claude-opus-4-8", Status: "busy", Entrypoint: "cli",
		UpdatedAt: time.Now().UnixMilli(), Version: "1.2.3", SessionID: "abcdef123456",
	}
	for _, mode := range []string{"1", "2", "3"} {
		for _, selected := range []bool{false, true} {
			row := stripTrailingReset(renderSessionRowForTest(t, mode, s, selected))
			if !strings.HasSuffix(row, " ") || strings.HasSuffix(row, "  ") {
				t.Errorf("mode %s selected=%v row lacks exactly one trailing space: %q", mode, selected, row)
			}
		}
	}
}
```

Add a header finder and header padding assertion:

```go
func findHeaderRow(t *testing.T, out string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "NAME") && strings.Contains(line, "AGE") {
			return line
		}
	}
	t.Fatalf("no table header in output:\n%s", out)
	return ""
}

func TestTableHeadersHaveOneRightPaddingSpace(t *testing.T) {
	for _, mode := range []string{"1", "2", "3"} {
		var b strings.Builder
		RenderAll(&b, mode, testLocalHost(Session{PID: 1, Name: "row", CWD: "/work/row"}), nil, "", nil, 0, 0, "dir")
		hdr := findHeaderRow(t, b.String())
		if !strings.HasSuffix(hdr, " ") || strings.HasSuffix(hdr, "  ") {
			t.Errorf("mode %s header lacks exactly one trailing space: %q", mode, hdr)
		}
	}
}
```

- [ ] **Step 2: Write failing intermediate-order test**

```go
func TestIntermediateStatusPrecedesModel(t *testing.T) {
	s := Session{
		PID: 42, Name: "ordered", NameSource: "user", CWD: "/work/ordered",
		Model: "claude-opus-4-8", Status: "busy", Entrypoint: "cli",
		UpdatedAt: time.Now().UnixMilli(),
	}
	var b strings.Builder
	RenderAll(&b, "3", testLocalHost(s), nil, "", nil, 0, 0, "dir")
	out := b.String()
	hdr := findHeaderRow(t, out)
	if strings.Index(hdr, "STATUS") >= strings.Index(hdr, "MODEL") {
		t.Fatalf("intermediate header order = %q", hdr)
	}
	row := findRow(t, out, "ordered")
	if strings.Index(row, s.StatusDisplay()) >= strings.Index(row, shortModel(s.Model)) {
		t.Fatalf("intermediate row order = %q", row)
	}
}
```

- [ ] **Step 3: Run new tests and verify failure**

Run:

```bash
go test ./... -run 'TestSessionRowsHaveOneRightPaddingSpace|TestTableHeadersHaveOneRightPaddingSpace|TestIntermediateStatusPrecedesModel'
```

Expected: FAIL because current rows/headers end at final value and intermediate MODEL precedes STATUS.

- [ ] **Step 4: Add one trailing padding cell**

Append one literal space to each mode's header and body format string:

```go
// Full header/body final fragments
"... %-8s  %s "

// Intermediate header/body final fragments
"... %5s  %5s "

// Minimal header/body final fragments
"... %-*s  %5s "
```

Do not append padding after `highlightSelectedRow`; padding must remain inside selected background. Separator remains `strings.Repeat("-", visualLen(hdr))`, so it automatically matches padded header width.

- [ ] **Step 5: Move STATUS before MODEL in intermediate header and body**

Update intermediate header arguments:

```go
return fmt.Sprintf("  %-*s  %-*s  %-*s  %-*s  %*s  %*s  %5s  %5s  %5s ",
	nameW, "NAME", dirW, dirLabel, statusW, statusLabel, modelW, "MODEL",
	costW, "COST", agentsW, "AGENTS", "CTX", "CPU%", ageLabel)
```

Update intermediate body arguments:

```go
body := fmt.Sprintf("%s  %s  %s  %s  %s  %*s  %s  %5s  %5s ",
	nameCell,
	marqueeCell(r.cwdStr, dirW, step),
	statusCell,
	modelCell(r.modelStr, modelW, plainCells),
	costCell(r.costStr, costW),
	agentsW, r.agentsStr,
	ctxCell(r.ctxStr, r.s.ContextTokens, plainCells),
	r.s.CPU, r.ageStr)
```

- [ ] **Step 6: Run focused tests and verify pass**

Run:

```bash
go test ./... -run 'TestSessionRowsHaveOneRightPaddingSpace|TestTableHeadersHaveOneRightPaddingSpace|TestIntermediateStatusPrecedesModel|TestSelectedSessionRowsHighlightWholeRow'
```

Expected: PASS.

- [ ] **Step 7: Commit layout changes**

```bash
git add render.go render_test.go
git commit -m "style: polish table spacing and order"
```

---

### Task 3: Full Verification and Delivery

**Files:**
- Verify: all Go source and tests
- Merge target: `main`

**Interfaces:**
- Consumes: completed worktree commits.
- Produces: verified, pushed, locally installed `main`.

- [ ] **Step 1: Run full verification**

```bash
go test ./...
go vet ./...
go build .
```

Expected: all commands exit 0.

- [ ] **Step 2: Review final diff and worktree state**

```bash
git diff main...HEAD -- render.go render_test.go docs/superpowers/specs/2026-07-22-render-polish-design.md docs/superpowers/plans/2026-07-22-render-polish.md
git status --short
```

Expected: only planned files changed; status clean after plan commit.

- [ ] **Step 3: Commit implementation plan if still uncommitted**

```bash
git add docs/superpowers/plans/2026-07-22-render-polish.md
git commit -m "docs: plan render polish"
```

Expected: plan commit created before merge.

- [ ] **Step 4: Merge worktree branch into main checkout**

From original checkout:

```bash
git switch main
git pull --ff-only
git merge --no-ff worktree-render-polish
```

Expected: merge succeeds without conflicts.

- [ ] **Step 5: Verify merged main**

```bash
go test ./...
go vet ./...
go build .
```

Expected: all commands exit 0 on merged `main`.

- [ ] **Step 6: Push and install**

```bash
git push origin main
make install
```

Expected: push succeeds; host binary copied to `~/.local/bin`.
