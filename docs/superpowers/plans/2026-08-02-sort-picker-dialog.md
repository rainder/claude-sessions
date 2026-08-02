# Sort Picker Dialog Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the `s`/`S` cycle-sort key binding with a bordered dialog that lists all 6 sort modes; navigate with ↑/↓, confirm with Enter, cancel with Esc.

**Architecture:** New file `sort_picker.go` mirrors the existing `account_picker.go` pattern exactly: a pure `sortPickerState.handle(key)` state machine, a `renderSortPicker(...)` pure-string renderer for a centered bordered box, and a blocking `pickSortMode(...)` loop that owns the terminal draw/read cycle. `tui.go`'s `case "s", "S":` is rewired to call it inline (same style as the existing `case "?":` help overlay), not routed through `actCtx`, since sort mode is global TUI state with no selected-row target.

**Tech Stack:** Go stdlib + `golang.org/x/term` (already a repo dependency, no new imports beyond what `account_picker.go` already uses: `fmt`, `os`, `strings`, `golang.org/x/term`).

## Global Constraints

- Spec: `docs/superpowers/specs/2026-08-02-sort-picker-dialog-design.md` (read it before starting — this plan implements it verbatim).
- Follow this repo's existing patterns exactly — `sort_picker.go` is a close structural mirror of `account_picker.go`, not a reinvention.
- `go build ./...`, `go vet ./...`, `go test ./...` must all be green before any commit that isn't purely a failing-test commit.
- Work happens in a git worktree (`.claude/worktrees/sort-picker-dialog`), never directly on `main`, per this repo's `CLAUDE.md`.
- No new dependencies.

---

### Task 1: `sortPickerState` — key handling

**Files:**
- Create: `sort_picker.go`
- Test: `sort_picker_test.go`

**Interfaces:**
- Consumes: nothing from other tasks (first task).
- Produces: `type sortPickerState struct { sel, rows int }` and `func (s *sortPickerState) handle(key string) (confirm, cancel bool)` — Task 3 (the blocking loop) constructs and drives this type.

- [ ] **Step 1: Write the failing test**

Create `sort_picker_test.go`:

```go
package main

import "testing"

// TestSortPickerStateHandle is the picker's key table: navigation wraps,
// Enter confirms, Esc/q/Ctrl-C cancel, and anything else is ignored rather
// than treated as a dismissal. Mirrors TestAccountPickerStateHandle, minus
// the empty-list cases account_picker_test.go needs — sortModeOrder is never
// empty — while still keeping the rows==0 guard itself (defensive, in case a
// caller ever zero-constructs the state).
func TestSortPickerStateHandle(t *testing.T) {
	tests := []struct {
		name        string
		state       sortPickerState
		key         string
		wantSel     int
		wantConfirm bool
		wantCancel  bool
	}{
		{name: "down moves", state: sortPickerState{sel: 0, rows: 6}, key: KeyDown, wantSel: 1},
		{name: "down wraps", state: sortPickerState{sel: 5, rows: 6}, key: KeyDown, wantSel: 0},
		{name: "up wraps", state: sortPickerState{sel: 0, rows: 6}, key: KeyUp, wantSel: 5},
		{name: "enter confirms", state: sortPickerState{sel: 2, rows: 6}, key: KeyEnter, wantSel: 2, wantConfirm: true},
		{name: "esc cancels", state: sortPickerState{rows: 6}, key: KeyEsc, wantCancel: true},
		{name: "q cancels", state: sortPickerState{rows: 6}, key: "q", wantCancel: true},
		{name: "ctrl-c cancels", state: sortPickerState{rows: 6}, key: "\x03", wantCancel: true},
		{name: "unmapped key is ignored", state: sortPickerState{sel: 1, rows: 6}, key: "x", wantSel: 1},
		{name: "enter on an empty list does nothing", state: sortPickerState{rows: 0}, key: KeyEnter},
		{name: "nav on an empty list does nothing", state: sortPickerState{rows: 0}, key: KeyDown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := tt.state
			confirm, cancel := state.handle(tt.key)
			if confirm != tt.wantConfirm || cancel != tt.wantCancel {
				t.Fatalf("handle(%q) = (%v, %v), want (%v, %v)", tt.key, confirm, cancel, tt.wantConfirm, tt.wantCancel)
			}
			if state.sel != tt.wantSel {
				t.Fatalf("sel = %d, want %d", state.sel, tt.wantSel)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./... -run TestSortPickerStateHandle -v`
Expected: FAIL — build error, `sortPickerState` undefined (no `sort_picker.go` yet).

- [ ] **Step 3: Write minimal implementation**

Create `sort_picker.go`:

```go
package main

// The 's'/'S' sort-mode dialog: a small bordered overlay listing every entry
// in sortModeOrder, current mode marked, Enter applies immediately.
//
// Style follows the existing small overlays (account_picker.go /
// confirm_overlay.go) rather than the full searchable resume picker: there
// are exactly 6 fixed modes, so there is nothing to search.

// sortPickerState is the picker's pure key-handling core: which row is
// selected, and what a keystroke does to it. Structurally identical to
// accountPickerState (account_picker.go) — same four-key contract.
type sortPickerState struct {
	sel  int
	rows int
}

// handle applies one key event. confirm means "apply the selected row";
// cancel means "close, change nothing". Any other key (including nav on an
// empty list) just redraws, so an unmapped keystroke never dismisses the
// overlay. rows is always len(sortModeOrder) (6) in production, never 0 —
// the rows==0 guard is kept anyway since sortModeOrder is a mutable package
// var, not a const, so a future zero-value construction should redraw
// instead of panicking on an out-of-range index.
func (s *sortPickerState) handle(key string) (confirm, cancel bool) {
	switch key {
	case KeyEsc, "q", "Q", "\x03", "\x04":
		return false, true
	case KeyUp:
		if s.rows > 0 {
			s.sel = (s.sel - 1 + s.rows) % s.rows
		}
		return false, false
	case KeyDown:
		if s.rows > 0 {
			s.sel = (s.sel + 1) % s.rows
		}
		return false, false
	case KeyEnter, "\r", "\n":
		if s.rows == 0 {
			return false, false
		}
		return true, false
	default:
		return false, false
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./... -run TestSortPickerStateHandle -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add sort_picker.go sort_picker_test.go
git commit -m "feat: add sortPickerState key handling"
```

---

### Task 2: `renderSortPicker` — bordered box rendering

**Files:**
- Modify: `sort_picker.go` (append)
- Test: `sort_picker_test.go` (append)

**Interfaces:**
- Consumes: `sortModeOrder []string` (tui.go:809), `sortDesc(mode string) string` (tui.go:827-842) — both already exist, unchanged.
- Produces: `func renderSortPicker(current string, sel, cols, rows int) string` — Task 3's `pickSortMode` calls this every draw.

- [ ] **Step 1: Write the failing test**

Append to `sort_picker_test.go`:

```go
// TestRenderSortPicker proves every mode's label appears and the marker sits
// on the row matching `current`, not necessarily the row matching `sel` —
// the cursor can move away from the live mode before Enter is pressed.
func TestRenderSortPicker(t *testing.T) {
	out := renderSortPicker("created", 0, 80, 24)
	if !strings.Contains(out, "sort by") {
		t.Fatalf("title missing:\n%s", out)
	}
	for _, mode := range sortModeOrder {
		if !strings.Contains(out, sortDesc(mode)) {
			t.Fatalf("output missing label for %q:\n%s", mode, out)
		}
	}
	for i, line := range strings.Split(out, "\n") {
		_ = i
		if strings.Contains(line, sortDesc("created")) && !strings.Contains(line, sortPickerActiveGlyph) {
			t.Fatalf("current mode row is not marked: %q", line)
		}
		if strings.Contains(line, sortDesc("status")) && strings.Contains(line, sortPickerActiveGlyph) {
			t.Fatalf("non-current mode row is marked: %q", line)
		}
	}
}

// TestRenderSortPickerUnknownSize mirrors renderConfirmOverlay's fallback: an
// unknown terminal size emits the box unpositioned rather than panicking on
// negative padding.
func TestRenderSortPickerUnknownSize(t *testing.T) {
	out := renderSortPicker("dir", 0, 0, 0)
	if !strings.HasPrefix(out, confirmBoxTL) {
		t.Fatalf("want an unpositioned box, got:\n%s", out)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./... -run TestRenderSortPicker -v`
Expected: FAIL — build error, `renderSortPicker` / `sortPickerActiveGlyph` undefined.

- [ ] **Step 3: Write minimal implementation**

Append to `sort_picker.go`:

```go
// sortPickerHint is the fixed hint row drawn below the mode rows.
const sortPickerHint = "↑/↓ select    ⏎ confirm    esc cancel"

// sortPickerActiveGlyph marks the row matching the currently live sort mode.
const sortPickerActiveGlyph = "●"

// sortPickerTitle is the dialog's fixed title.
const sortPickerTitle = "sort by"

// renderSortPicker draws the bordered box centered in a cols x rows
// terminal: the title, one line per sortModeOrder entry, a blank separator,
// then the dimmed hint. Positioning, clipping and the unknown-size fallback
// match renderAccountPicker / renderConfirmOverlay, so every small overlay in
// this app looks like the same kind of dialog.
func renderSortPicker(current string, sel, cols, rows int) string {
	body := make([]string, 0, len(sortModeOrder))
	for _, mode := range sortModeOrder {
		marker := " "
		if mode == current {
			marker = sortPickerActiveGlyph
		}
		body = append(body, marker+" "+sortDesc(mode))
	}

	innerWidth := visualLen(sortPickerTitle)
	for _, l := range append(append([]string{}, body...), sortPickerHint) {
		if w := visualLen(l); w > innerWidth {
			innerWidth = w
		}
	}
	if cols > 0 {
		max := cols - 4 // border + one space of padding each side
		if max < 1 {
			max = 1
		}
		if innerWidth > max {
			innerWidth = max
		}
	}

	pad := func(s string, selected bool) string {
		s = clipLine(s, innerWidth)
		line := s + strings.Repeat(" ", innerWidth-visualLen(s))
		if selected {
			line = highlightSelectedRow(line, true)
		}
		return confirmBoxV + " " + line + " " + confirmBoxV
	}

	box := make([]string, 0, len(body)+5)
	box = append(box, confirmBoxTL+strings.Repeat(confirmBoxH, innerWidth+2)+confirmBoxTR)
	box = append(box, pad(bold(sortPickerTitle), false))
	box = append(box, pad("", false))
	for i, l := range body {
		box = append(box, pad(l, i == sel))
	}
	box = append(box, pad("", false))
	box = append(box, pad(dim(sortPickerHint), false))
	box = append(box, confirmBoxBL+strings.Repeat(confirmBoxH, innerWidth+2)+confirmBoxBR)

	if cols <= 0 || rows <= 0 {
		return strings.Join(box, "\n")
	}

	boxWidth := innerWidth + 4
	left := (cols - boxWidth) / 2
	if left < 0 {
		left = 0
	}
	leftPad := strings.Repeat(" ", left)
	for i, l := range box {
		box[i] = leftPad + l
	}
	top := (rows - len(box)) / 2
	if top < 0 {
		top = 0
	}
	lines := make([]string, 0, top+len(box))
	for i := 0; i < top; i++ {
		lines = append(lines, "")
	}
	lines = append(lines, box...)
	return strings.Join(lines, "\n")
}
```

Add `"strings"` to `sort_picker.go`'s import block (needed for `strings.Repeat`/`strings.Join`):

```go
package main

import "strings"
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./... -run TestRenderSortPicker -v`
Expected: PASS. Also run `go test ./... -run TestSortPickerStateHandle -v` to confirm Task 1 still passes.

- [ ] **Step 5: Commit**

```bash
git add sort_picker.go sort_picker_test.go
git commit -m "feat: add renderSortPicker bordered-box rendering"
```

---

### Task 3: `pickSortMode` — blocking input loop

**Files:**
- Modify: `sort_picker.go` (append, extend imports)
- Test: none (this is a blocking terminal I/O loop — same as `pickAccount`, which `account_picker.go` also leaves untested at this level; covered by the manual TUI check in Task 4 instead).

**Interfaces:**
- Consumes: `sortPickerState` + `handle` (Task 1), `renderSortPicker` (Task 2), `newScreenRenderer`, `newInputDecoder`, `readModalEvents(dec *inputDecoder, wakes []wakeFD) ([]string, wakeKind)` (tui.go:47), `wakeFD` type, `term.GetSize` (`golang.org/x/term`).
- Produces: `func pickSortMode(current string, wakes []wakeFD) (picked string, ok bool)` — Task 4 (tui.go wiring) calls this directly.

- [ ] **Step 1: Extend imports and write the loop**

Change `sort_picker.go`'s import block from:

```go
package main

import "strings"
```

to:

```go
package main

import (
	"os"
	"strings"

	"golang.org/x/term"
)
```

Append to `sort_picker.go`:

```go
// pickSortMode drives the blocking picker, mirroring pickAccount's read/handle
// loop (account_picker.go). Must be called in raw mode; it never leaves raw
// or the alt-screen, so the caller's next render() paints over it. wakes
// carries the modal wake sources (resize) so the box stays centered across a
// live resize.
//
// The selection starts on the mode matching `current`, so Enter without
// moving reports that same mode back — the caller is responsible for
// treating "confirmed but unchanged" as a no-op (see tui.go's wiring).
func pickSortMode(current string, wakes []wakeFD) (picked string, ok bool) {
	state := sortPickerState{rows: len(sortModeOrder)}
	for i, mode := range sortModeOrder {
		if mode == current {
			state.sel = i
			break
		}
	}
	renderer := newScreenRenderer(os.Stdout)
	decoder := newInputDecoder()
	fd := int(os.Stdin.Fd())

	for {
		cols, rows, err := term.GetSize(fd)
		if err != nil {
			cols, rows = 0, 0
		}
		_ = renderer.Draw(renderSortPicker(current, state.sel, cols, rows), cols, rows)
		keys, _ := readModalEvents(decoder, wakes)
		for _, key := range keys {
			confirm, cancel := state.handle(key)
			if cancel {
				return "", false
			}
			if confirm {
				return sortModeOrder[state.sel], true
			}
		}
	}
}
```

- [ ] **Step 2: Build to verify it compiles**

Run: `go build ./...`
Expected: builds clean (this task has no dedicated test — `pickSortMode` is a blocking terminal loop, same as `pickAccount`, exercised manually in Task 4).

- [ ] **Step 3: Run existing tests to confirm no regression**

Run: `go test ./... -v`
Expected: PASS (all existing tests plus Task 1/2's new tests).

- [ ] **Step 4: Commit**

```bash
git add sort_picker.go
git commit -m "feat: add pickSortMode blocking input loop"
```

---

### Task 4: Wire `s`/`S` into `tui.go`, update help text

**Files:**
- Modify: `tui.go:703-713` (the `case "s", "S":` block)
- Modify: `tui.go` (renderHelp's sort-key hint line, currently around line 1122 — search for `cycle sort forward / back`)
- Modify: `main.go:114` (CLI help text)

**Interfaces:**
- Consumes: `pickSortMode(current string, wakes []wakeFD) (string, bool)` (Task 3), `sortDesc(mode string) string` (tui.go:827-842, unchanged), `screen`, `fd`, `modalWakes`, `sortMode`, `toast`, `toastUntil`, `SaveSortMode` — all pre-existing loop-local vars/functions already in scope inside `RunTUI`.
- Produces: nothing consumed by a later task (last task).

- [ ] **Step 1: Replace the `case "s", "S":` block**

In `tui.go`, find:

```go
			case "s", "S":
				delta := 1 // s cycles forward, shift-s backward
				if k == "S" {
					delta = -1
				}
				sortMode = cycleSortMode(sortMode, delta)
				SaveSortMode(sortMode)
				toast = "sort: " + sortDesc(sortMode)
				toastUntil = time.Now().Add(4 * time.Second)
				refresh(false)
				render()
```

Replace with:

```go
			case "s", "S":
				screen.Invalidate()
				if picked, ok := pickSortMode(sortMode, modalWakes); ok && picked != sortMode {
					sortMode = picked
					SaveSortMode(sortMode)
					toast = "sort: " + sortDesc(sortMode)
					toastUntil = time.Now().Add(4 * time.Second)
					refresh(false)
				}
				screen.Invalidate()
				render()
```

This mirrors the existing `case "?":` block (tui.go:725-740) for the invalidate-draw-invalidate-render shape. `cycleSortMode` (tui.go:813-823) is no longer called from this switch after this change — leave the function itself in place; it stays covered by its own existing test and is harmless dead-from-the-switch code that a later cleanup can remove if nothing else calls it (confirm with `grep -rn cycleSortMode` before deleting anything — out of scope for this task to avoid an unreviewed removal).

- [ ] **Step 2: Update `renderHelp`'s sort-key line**

In `tui.go`, inside `renderHelp` (around line 1122), find:

```go
	fmt.Fprintln(&b, "    s / S        cycle sort forward / back (dir → status → created → updated, +asc)")
	fmt.Fprintln(&b, "                 current sort: "+sortMode)
```

Replace with:

```go
	fmt.Fprintln(&b, "    s / S        open sort-by dialog (↑/↓ select · ⏎ confirm · esc cancel)")
	fmt.Fprintln(&b, "                 current sort: "+sortMode)
```

- [ ] **Step 3: Update `main.go`'s CLI help text**

In `main.go`, find:

```go
  s    cycle sort   r  refresh
```

Replace with:

```go
  s    sort menu    r  refresh
```

- [ ] **Step 4: Build and run the full test suite**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: all green.

- [ ] **Step 5: Manual TUI check**

Run: `go run . ` (or `make run`) against a real terminal with at least a couple of sessions visible.

Verify each of these, per the spec's testing section:
1. Press `s` — dialog opens, bordered box titled "sort by", 6 rows, current mode's row marked with `●`, selection cursor starts on that same row.
2. Press `S` (shift-s) — same dialog opens (not a separate/old cycle behavior).
3. ↑/↓ move the selection, wrapping at both ends.
4. Move to a *different* mode and press Enter — dialog closes, sort actually reorders the session list, a toast reading `sort: <label>` appears and is visible (not just set-and-vanished — this is the bug the design doc's Codex review caught), and the new mode survives a restart (persisted via `SaveSortMode`).
5. Open the dialog again, move the cursor away from the current mode, then press Enter *without* landing back on it in a way that changes nothing — actually: reopen and press Enter immediately (cursor still on the current mode) — verify no toast appears and nothing refreshes (true no-op).
6. Open the dialog, press Esc — dialog closes, sort mode is unchanged, no toast.
7. Open the dialog, resize the terminal — box stays centered.
8. Shrink the terminal to fewer than ~12 rows tall while the dialog is open — confirm the selected row is still visible (not clipped out of view) or note the observed behavior if it isn't (this is a known-risk case flagged in the design doc, not a hard blocker).
9. Press `?` for help — confirm the sort-key line now describes the dialog, not "cycle forward/back".

- [ ] **Step 6: Commit**

```bash
git add tui.go main.go
git commit -m "feat: replace sort cycle with sort-by dialog on s/S"
```

---

## Post-plan repo workflow (per this repo's CLAUDE.md, not a plan task)

Once all 4 tasks are done and verified:
1. Merge the worktree branch into `main` locally.
2. `make install`.
3. `git push origin main`.
4. Deploy to `agent-workstation` per the deploy memory.
5. Remove the worktree and its branch.
