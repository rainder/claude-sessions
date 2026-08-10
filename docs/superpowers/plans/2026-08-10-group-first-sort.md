# Group-first sort Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a persisted "group first" sort toggle, controlled from inside the existing `s`/`S` sort dialog via a `g` key, that orders rows by group number (1-9, ungrouped last) before applying the currently selected sort mode.

**Architecture:** `SortSessions`/`sessionLess` gain a `groupSort bool` tier that ranks by `Session.Group` before falling through to the existing per-mode comparator; the state is loaded/saved via a new `group-sort` config file (mirroring `sort-mode`); the toggle lives entirely inside `sort_picker.go`'s dialog (no new global keybinding); a small header badge shows when it's active.

**Tech Stack:** Go stdlib only (`sort.SliceStable`), matching the rest of this repo.

## Global Constraints

- Follow the repo's existing persisted-setting pattern exactly: `ConfigDir()`, best-effort read/write, silent fallback to a safe default (config.go's `LoadSortMode`/`SaveSortMode`).
- `sort.SliceStable` is required everywhere sessions are sorted (existing invariant — ties must preserve input order; several existing tests, e.g. `TestSortSessionsStable`, pin this).
- No new global keybinding — the toggle only exists inside the sort dialog (`g`/`G` while `sort_picker.go`'s overlay is open).
- Disabled sessions must continue to sort last in every mode and with group-first on or off (existing invariant, pinned by `TestSortSessionsKeepsDisabledRowsLastInEveryMode`).
- Every production call site of `SortSessions`/`sortRemotes`/`renderHeader`/`renderHelp`/`pickSortMode`/`renderSortPicker` that this plan touches must be updated in the same task that changes its signature, so `go build ./...` and `go test ./...` stay green at the end of every task.

---

## File Structure

- **config.go** — add `LoadGroupSort()` / `SaveGroupSort(bool)`.
- **session.go** — add `groupSortRank(int) int` and `sessionLessGrouped(a, b Session, mode string, groupSort bool) bool`; change `SortSessions`'s signature.
- **sort_picker.go** — add a `groupSort` field to `sortPickerState`, a `g`/`G` case to `handle`, a toggle line to `renderSortPicker`, and change `pickSortMode`'s signature to return the toggle state too.
- **render.go** — add a `groupSort` field to `groupView`, add a `groupSort bool` parameter to `renderHeader`, render a badge.
- **tui.go** — load/hold `groupSortOn`, thread it through `settleRows`, the `s`/`S` key handler, both `groupView{...}` literals, `sortRemotes`, and `renderHelp`.
- **main.go**, **commands.go** — thread `LoadGroupSort()` through the two CLI list paths (`cmdList`, `cmdListSessions`).
- Test files updated alongside each production file: **config_test.go**, **session_test.go**, **sort_picker_test.go**, **render_test.go**, **tui_test.go**, **tui_state_test.go**, **session_flags_test.go**.

---

### Task 1: Persisted config

**Files:**
- Modify: `config.go`
- Test: `config_test.go`

**Interfaces:**
- Produces: `LoadGroupSort() bool` (default `false`), `SaveGroupSort(on bool)`.

- [ ] **Step 1: Write the failing tests**

Add to `config_test.go` (needs a new `fmt` import alongside the existing `os`, `path/filepath`, `testing`):

```go
func TestLoadGroupSortMissing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if got := LoadGroupSort(); got != false {
		t.Errorf("LoadGroupSort() with no file = %v, want false", got)
	}
}

func TestLoadGroupSortGarbage(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".config", "claude-sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "group-sort"), []byte("nonsense\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := LoadGroupSort(); got != false {
		t.Errorf("LoadGroupSort() with garbage value = %v, want false", got)
	}
}

func TestGroupSortRoundTrip(t *testing.T) {
	for _, on := range []bool{true, false} {
		t.Run(fmt.Sprintf("%v", on), func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			SaveGroupSort(on)
			if got := LoadGroupSort(); got != on {
				t.Errorf("LoadGroupSort() after SaveGroupSort(%v) = %v, want %v", on, got, on)
			}
		})
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./... -run TestLoadGroupSort -v` and `go test ./... -run TestGroupSortRoundTrip -v`
Expected: FAIL with `undefined: LoadGroupSort` (compile error) — expected at this stage, since the functions don't exist yet.

- [ ] **Step 3: Implement**

Add to `config.go`, right after `SaveSortMode`:

```go
// LoadGroupSort reports whether group-first sort is enabled ("on" in the
// group-sort file). Defaults to false (off) on any error or unrecognized
// value.
func LoadGroupSort() bool {
	data, err := os.ReadFile(filepath.Join(ConfigDir(), "group-sort"))
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(data)) == "on"
}

// SaveGroupSort persists the group-first sort toggle. Best-effort, like
// SaveSortMode.
func SaveGroupSort(on bool) {
	dir := ConfigDir()
	if dir == "" {
		return
	}
	_ = os.MkdirAll(dir, 0o755)
	v := "off"
	if on {
		v = "on"
	}
	_ = os.WriteFile(filepath.Join(dir, "group-sort"), []byte(v+"\n"), 0o644)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./... -run 'TestLoadGroupSort|TestGroupSortRoundTrip' -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add config.go config_test.go
git commit -m "feat: add persisted group-sort config toggle"
```

---

### Task 2: Comparator and `SortSessions` signature

**Files:**
- Modify: `session.go:217`, `session.go:308-312` (`SortSessions`)
- Test: `session_test.go`, `tui_state_test.go:620,627`, `session_flags_test.go:739-740`

**Interfaces:**
- Consumes: nothing new.
- Produces: `groupSortRank(group int) int`, `sessionLessGrouped(a, b Session, mode string, groupSort bool) bool`, `SortSessions(rows []Session, mode string, groupSort bool)` (signature changed — was 2 args, now 3).

- [ ] **Step 1: Update `SortSessions`'s signature and implement the new comparator**

In `session.go`, add right after `sessionLess` (which stays unchanged) and before the old `SortSessions`:

```go
// groupSortRank maps a group number to its group-first sort key: groups 1-9
// rank by their own number; ungrouped (0, or any out-of-range value) ranks
// last (10), sinking below every named group.
func groupSortRank(group int) int {
	if group < 1 || group > 9 {
		return 10
	}
	return group
}

// sessionLessGrouped wraps sessionLess with an optional group-first tier:
// disabled rows still sort last unconditionally (unchanged from sessionLess),
// then, when groupSort is on, rows rank by group number ascending with
// ungrouped rows sinking to the bottom, then ties fall through to the normal
// per-mode comparison via sessionLess.
func sessionLessGrouped(a, b Session, mode string, groupSort bool) bool {
	if a.Disabled != b.Disabled {
		return !a.Disabled
	}
	if groupSort {
		ra, rb := groupSortRank(a.Group), groupSortRank(b.Group)
		if ra != rb {
			return ra < rb
		}
	}
	return sessionLess(a, b, mode)
}
```

Replace the existing `SortSessions`:

```go
func SortSessions(rows []Session, mode string, groupSort bool) {
	sort.SliceStable(rows, func(i, j int) bool {
		return sessionLessGrouped(rows[i], rows[j], mode, groupSort)
	})
}
```

Update the one production caller inside `session.go` (in the function ending at line ~217, the server-side default sort — stays group-agnostic since group-first is a client display preference, not a collection-time default):

```go
	// Sort by cwd (case-insensitive), newest-started first as tiebreaker. This
	// is the server-side default; the client re-sorts per its own mode.
	SortSessions(out, "dir", false)
	return out, nil
}
```

- [ ] **Step 2: Fix every existing call site so the package compiles and old tests stay green**

In `session_test.go`, add a third argument `false` to every existing `SortSessions(...)` call:
- line 116: `SortSessions(rows, c.mode)` → `SortSessions(rows, c.mode, false)`
- line 139: `SortSessions(rows, "status")` → `SortSessions(rows, "status", false)`
- line 156: `SortSessions(rows, "status")` → `SortSessions(rows, "status", false)`
- line 170: `SortSessions(rows, "created")` → `SortSessions(rows, "created", false)`
- line 425: `SortSessions(rows, tc.mode)` → `SortSessions(rows, tc.mode, false)`

In `tui_state_test.go`:
- line 620: `SortSessions(rows, "dir")` → `SortSessions(rows, "dir", false)`
- line 627: `SortSessions(rows, "dir")` → `SortSessions(rows, "dir", false)`

In `session_flags_test.go`:
- line 739: `SortSessions(before, "dir")` → `SortSessions(before, "dir", false)`
- line 740: `SortSessions(after, "dir")` → `SortSessions(after, "dir", false)`

`main.go`, `commands.go`, and `tui.go` also call `SortSessions` directly (or indirectly through `sortRemotes`, whose own body calls `SortSessions`). `sortRemotes`'s own signature isn't changing yet — that happens in Task 5 — so its internal `SortSessions` call just needs the same third argument. Make these mechanical fixes now, temporarily passing `false` at every one; Task 5 (`tui.go`) and Task 6 (`main.go`, `commands.go`) will later replace these `false` literals with real `groupSortOn` plumbing:
- `tui.go:357`: `SortSessions(local, sortMode)` → `SortSessions(local, sortMode, false)`
- `tui.go:1115` (inside `sortRemotes`): `SortSessions(sorted, mode)` → `SortSessions(sorted, mode, false)`
- `commands.go:485`: `SortSessions(local, sortMode)` → `SortSessions(local, sortMode, false)`
- `main.go:135`: `SortSessions(local, sortMode)` → `SortSessions(local, sortMode, false)`

- [ ] **Step 3: Run the full test suite to verify nothing broke**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS (all pre-existing behavior unchanged, since every call site passes `false`).

- [ ] **Step 4: Write the new failing tests for group-first behavior**

Add to `session_test.go`:

```go
func TestSessionLessGroupedOrdersByGroupFirst(t *testing.T) {
	fixture := []Session{
		{SessionID: "g2", CWD: "/z", Group: 2, StartedAt: 100},
		{SessionID: "g1-old", CWD: "/a", Group: 1, StartedAt: 100},
		{SessionID: "ungrouped", CWD: "/m", Group: 0, StartedAt: 500},
		{SessionID: "g1-new", CWD: "/a", Group: 1, StartedAt: 200},
	}
	rows := append([]Session(nil), fixture...)
	SortSessions(rows, "dir", true)
	got := make([]string, len(rows))
	for i, s := range rows {
		got[i] = s.SessionID
	}
	// Group 1 rows first (tied on cwd within "dir" mode, StartedAt desc
	// breaks the tie), then group 2, then ungrouped last regardless of its
	// own recency (StartedAt 500, the newest of all four).
	want := []string{"g1-new", "g1-old", "g2", "ungrouped"}
	if !equalStrings(got, want) {
		t.Fatalf("group-first order = %v, want %v", got, want)
	}
}

func TestSessionLessGroupedDisabledStillLast(t *testing.T) {
	rows := []Session{
		{SessionID: "disabled-g1", Group: 1, Disabled: true},
		{SessionID: "enabled-g2", Group: 2},
		{SessionID: "enabled-g1", Group: 1},
	}
	SortSessions(rows, "dir", true)
	got := make([]string, len(rows))
	for i, s := range rows {
		got[i] = s.SessionID
	}
	want := []string{"enabled-g1", "enabled-g2", "disabled-g1"}
	if !equalStrings(got, want) {
		t.Fatalf("disabled precedence under group-sort = %v, want %v", got, want)
	}
}
```

- [ ] **Step 5: Run the new tests to verify they pass**

Run: `go test ./... -run 'TestSessionLessGrouped' -v`
Expected: PASS (the implementation from Step 1 already covers this).

- [ ] **Step 6: Commit**

```bash
git add session.go session_test.go tui.go commands.go main.go tui_state_test.go session_flags_test.go
git commit -m "feat: add group-first sort tier to SortSessions"
```

---

### Task 3: Sort dialog toggle (`sort_picker.go`)

**Files:**
- Modify: `sort_picker.go`
- Test: `sort_picker_test.go`

**Interfaces:**
- Consumes: `sortModeOrder`, `sortDesc` (unchanged, from tui.go).
- Produces: `pickSortMode(current string, currentGroupSort bool, wakes []wakeFD) (picked string, groupSort bool, ok bool)` (signature changed — was `(current string, wakes []wakeFD) (string, bool)`); `renderSortPicker(current string, sel int, groupSort bool, cols, rows int) string` (signature changed — gained the `groupSort` parameter after `sel`); `sortPickerState.groupSort bool` field.

- [ ] **Step 1: Write the failing tests**

Add to `sort_picker_test.go`:

```go
// TestSortPickerStateHandleTogglesGroupSort proves 'g'/'G' flips groupSort
// without confirming or cancelling the dialog — it's a redraw-only toggle,
// same shape as up/down navigation.
func TestSortPickerStateHandleTogglesGroupSort(t *testing.T) {
	state := sortPickerState{rows: 6}
	confirm, cancel := state.handle("g")
	if confirm || cancel {
		t.Fatalf("handle(g) = (%v, %v), want (false, false)", confirm, cancel)
	}
	if !state.groupSort {
		t.Fatal("handle(g) did not set groupSort")
	}
	confirm, cancel = state.handle("G")
	if confirm || cancel {
		t.Fatalf("handle(G) = (%v, %v), want (false, false)", confirm, cancel)
	}
	if state.groupSort {
		t.Fatal("handle(G) did not clear groupSort (expected toggle back off)")
	}
}

// TestRenderSortPickerShowsGroupSortToggle proves the box reflects the
// groupSort argument's on/off state and the hint documents the g key.
func TestRenderSortPickerShowsGroupSortToggle(t *testing.T) {
	off := renderSortPicker("created", 0, false, 80, 24)
	if !strings.Contains(off, "group first: off") {
		t.Fatalf("off state missing:\n%s", off)
	}
	on := renderSortPicker("created", 0, true, 80, 24)
	if !strings.Contains(on, "group first: on") {
		t.Fatalf("on state missing:\n%s", on)
	}
	if !strings.Contains(off, "g group first") {
		t.Fatalf("hint missing g binding:\n%s", off)
	}
}
```

Update the existing calls to `renderSortPicker` to pass the new `groupSort` argument (all `false`, preserving today's rendered output exactly):
- line 50: `renderSortPicker("created", 0, 80, 24)` → `renderSortPicker("created", 0, false, 80, 24)`
- line 84: `renderSortPicker("created", 0, 80, 24)` → `renderSortPicker("created", 0, false, 80, 24)`
- line 89: `renderSortPicker("created", 2, 80, 24)` → `renderSortPicker("created", 2, false, 80, 24)`
- line 99: `renderSortPicker("dir", 0, 0, 0)` → `renderSortPicker("dir", 0, false, 0, 0)`

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./... -run 'TestSortPickerStateHandleTogglesGroupSort|TestRenderSortPickerShowsGroupSortToggle' -v`
Expected: FAIL — compile error (`too many arguments`/`undefined field groupSort`) since `sortPickerState` and `renderSortPicker` haven't changed yet.

- [ ] **Step 3: Implement**

In `sort_picker.go`, update `sortPickerState`:

```go
type sortPickerState struct {
	sel       int
	rows      int
	groupSort bool
}
```

Add a case to `handle` (after the `KeyDown` case, before `KeyEnter`):

```go
	case "g", "G":
		s.groupSort = !s.groupSort
		return false, false
```

Update the hint constant:

```go
// sortPickerHint is the fixed hint row drawn below the mode rows.
const sortPickerHint = "↑/↓ select    g group first    ⏎ confirm    esc cancel"
```

Update `renderSortPicker` to take and show the toggle:

```go
func renderSortPicker(current string, sel int, groupSort bool, cols, rows int) string {
	body := make([]string, 0, len(sortModeOrder))
	for _, mode := range sortModeOrder {
		marker := " "
		if mode == current {
			marker = sortPickerActiveGlyph
		}
		body = append(body, marker+" "+sortDesc(mode))
	}

	toggleLine := "group first: off"
	if groupSort {
		toggleLine = "group first: on"
	}

	innerWidth := visualLen(sortPickerTitle)
	for _, l := range append(append([]string{toggleLine}, body...), sortPickerHint) {
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

	box := make([]string, 0, len(body)+6)
	box = append(box, confirmBoxTL+strings.Repeat(confirmBoxH, innerWidth+2)+confirmBoxTR)
	box = append(box, pad(bold(sortPickerTitle), false))
	box = append(box, pad(dim(toggleLine), false))
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

Update `pickSortMode`:

```go
// pickSortMode drives the blocking picker, mirroring pickAccount's read/handle
// loop (account_picker.go). Must be called in raw mode; it never leaves raw
// or the alt-screen, so the caller's next render() paints over it. wakes
// carries the modal wake sources (resize) so the box stays centered across a
// live resize.
//
// The selection starts on the mode matching `current`, seeded with
// currentGroupSort as the toggle's starting state. Enter without changing
// anything reports the same mode and toggle state back — the caller is
// responsible for treating "confirmed but unchanged" as a no-op (see tui.go's
// wiring).
func pickSortMode(current string, currentGroupSort bool, wakes []wakeFD) (picked string, groupSort bool, ok bool) {
	state := sortPickerState{rows: len(sortModeOrder), groupSort: currentGroupSort}
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
		_ = renderer.Draw(renderSortPicker(current, state.sel, state.groupSort, cols, rows), cols, rows)
		keys, _ := readModalEvents(decoder, wakes)
		for _, key := range keys {
			confirm, cancel := state.handle(key)
			if cancel {
				return "", false, false
			}
			if confirm {
				return sortModeOrder[state.sel], state.groupSort, true
			}
		}
	}
}
```

`pickSortMode`'s only caller is `tui.go`'s `case "s", "S"` block, which Task 5 updates — leave `tui.go` uncompiled at the end of this task's Step 3 is not acceptable, so also make this one mechanical fix now:

In `tui.go`, temporarily change the `s`/`S` case's call from
`if picked, ok := pickSortMode(sortMode, modalWakes); ok && picked != sortMode {`
to
`if picked, _, ok := pickSortMode(sortMode, false, modalWakes); ok && picked != sortMode {`
(Task 5 replaces this whole block with the real two-way wiring.)

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go build ./... && go test ./...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add sort_picker.go sort_picker_test.go tui.go
git commit -m "feat: add group-first toggle to the sort dialog"
```

---

### Task 4: Header badge (`render.go`)

**Files:**
- Modify: `render.go`
- Test: `render_test.go`

**Interfaces:**
- Consumes: `dim` (render.go:54), `ansiBold` (render.go:21).
- Produces: `groupView.groupSort bool` field; `renderHeader`'s signature gains a `groupSort bool` parameter (after `filter groupFilter`, before `query string`).

- [ ] **Step 1: Write the failing test**

Add to `render_test.go`, near `TestGroupFilterHeaderIndicator`:

```go
func TestGroupSortHeaderIndicator(t *testing.T) {
	local := testLocalHost(Session{PID: 1, Name: "n", CWD: "/w", SessionID: "s"})

	on := frameText(BuildTableFrame("1", local, nil, "", nil, 0, 0, "dir", groupView{groupSort: true}))
	want := dim("group\u2191") // "group↑"
	if !strings.Contains(on, want) {
		t.Fatalf("header missing group-sort badge %q:\n%s", want, on)
	}

	off := frameText(BuildTableFrame("1", local, nil, "", nil, 0, 0, "dir", groupView{}))
	if strings.Contains(off, "group\u2191") {
		t.Fatalf("badge shown with group-sort off:\n%s", off)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./... -run TestGroupSortHeaderIndicator -v`
Expected: FAIL — `unknown field groupSort in struct literal` (compile error), since `groupView` has no such field yet.

- [ ] **Step 3: Implement**

In `render.go`, add the field to `groupView` (after `hideDisabled`):

```go
type groupView struct {
	groups       map[string]int
	filter       groupFilter
	query        string
	hideDisabled bool
	groupSort    bool
	showBadge    bool
	showRail     bool
	showNoTmux   bool
}
```

Update `renderHeader`'s signature and body:

```go
func renderHeader(w io.Writer, sections []section, mode string, accounts []accountUsageLine, codexAccounts []codexAccountLine, cols int, filter groupFilter, groupSort bool, query string) {
	live, busy := 0, 0
	for _, sec := range sections {
		for _, s := range sec.rows {
			live++
			// "busy" here means the main loop is occupied: working or in a shell.
			if s.Status == "busy" || s.Status == "shell" {
				busy++
			}
		}
	}
	// Two counts: live sessions (local + remote) and how many are occupied.
	// colorize ends with a full reset, so re-assert bold after the busy count
	// to keep the title bold.
	busyStr := colorize(statusColor["busy"], fmt.Sprintf("%d busy", busy)) + ansiBold
	// An active group filter shows a colored "only ③" / "hide ②③" indicator, a
	// dim "group↑" badge follows when group-first sort is on, and an active
	// text query appends a dim "/query" after that (each ends in a reset), so
	// re-assert bold after every segment to keep the trailing [mode] bright.
	filterStr := ""
	if ind := groupFilterIndicator(filter); ind != "" {
		filterStr = "  " + ind + ansiBold
	}
	if groupSort {
		filterStr += "  " + dim("group\u2191") + ansiBold
	}
	if query != "" {
		filterStr += "  " + dim("/"+query) + ansiBold
	}
	fmt.Fprintf(w, "%sClaude sessions  %s  (%s, %s)%s  %s%s\n",
		ansiBold, time.Now().Format("15:04:05"),
		plural(live, "session"), busyStr,
		filterStr, ansiReset, dim("["+mode+"]"))
	writeUsageHeader(w, accounts, codexAccounts, cols)
	fmt.Fprintln(w)
}
```

Update its three call sites:
- line ~1874: `renderHeader(w, sections, "full", accounts, codexAccounts, cols, gv.filter, gv.query)` → `renderHeader(w, sections, "full", accounts, codexAccounts, cols, gv.filter, gv.groupSort, gv.query)`
- line ~1997: same change with `"intermediate"`.
- line ~2128: same change with `"minimal"`.

- [ ] **Step 4: Run the test to verify it passes**

Run: `go build ./... && go test ./...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add render.go render_test.go
git commit -m "feat: show a header badge when group-first sort is on"
```

---

### Task 5: Wire the toggle into the live TUI (`tui.go`)

**Files:**
- Modify: `tui.go`
- Test: `tui_test.go`

**Interfaces:**
- Consumes: `LoadGroupSort`/`SaveGroupSort` (Task 1), `SortSessions(rows, mode, groupSort)` (Task 2), `pickSortMode(current, currentGroupSort, wakes) (mode, groupSort, ok)` (Task 3), `groupView.groupSort` (Task 4).
- Produces: `sortRemotes(remotes []RemoteResult, mode string, groupSort bool) []RemoteResult` (signature changed — gained `groupSort`); `renderHelp(sortMode string, groupSortOn bool) string` (signature changed — gained `groupSortOn`).

- [ ] **Step 1: Write the failing tests**

Update the two existing `renderHelp` calls in `tui_test.go`:
- line 199: `help := renderHelp("dir")` → `help := renderHelp("dir", false)`
- line 355: `out := renderHelp("status")` → `out := renderHelp("status", false)`

Add a new test to `tui_test.go`:

```go
func TestRenderHelpShowsGroupSortState(t *testing.T) {
	off := renderHelp("dir", false)
	if !strings.Contains(off, "group first: off") {
		t.Fatalf("help missing off state:\n%s", off)
	}
	if !strings.Contains(off, "g group first") {
		t.Fatalf("help missing g binding in sort-dialog line:\n%s", off)
	}
	on := renderHelp("dir", true)
	if !strings.Contains(on, "group first: on") {
		t.Fatalf("help missing on state:\n%s", on)
	}
}

// TestSortRemotesGroupFirst proves sortRemotes threads groupSort into each
// host's own SortSessions call — the wrapper itself has no direct test
// today (only its per-mode SortSessions delegate is covered elsewhere).
func TestSortRemotesGroupFirst(t *testing.T) {
	remotes := []RemoteResult{{Name: "dev", Sessions: []Session{
		{SessionID: "ungrouped", CWD: "/a", Group: 0, StartedAt: 200},
		{SessionID: "g1", CWD: "/z", Group: 1, StartedAt: 100},
	}}}
	out := sortRemotes(remotes, "dir", true)
	got := []string{out[0].Sessions[0].SessionID, out[0].Sessions[1].SessionID}
	want := []string{"g1", "ungrouped"}
	if !equalStrings(got, want) {
		t.Fatalf("sortRemotes(groupSort=true) order = %v, want %v", got, want)
	}
	// sortRemotes must not mutate the caller's slice — same invariant the
	// existing implementation already relies on (settleRows sorts a copy so
	// it never races the remote hub goroutine that owns the original).
	if remotes[0].Sessions[0].SessionID != "ungrouped" {
		t.Fatalf("sortRemotes mutated the input slice: %v", remotes[0].Sessions)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./... -run 'TestRenderHelp|TestRenderHelpIsPureContent|TestSortRemotesGroupFirst' -v`
Expected: FAIL — compile error (`too many arguments`/`not enough arguments`) since `renderHelp` and `sortRemotes` still take their old argument counts.

- [ ] **Step 3: Implement**

In `tui.go`, add the state variable right after `sortMode := LoadSortMode()` (line 117):

```go
	sortMode := LoadSortMode()
	groupSortOn := LoadGroupSort()
```

Update `settleRows` (lines 357, 361):

```go
	SortSessions(local, sortMode, groupSortOn)
	// Snapshot() returns the hub's shared slices; sort remotes on copies so
	// we never race the hub goroutine that owns them.
	snap := hub.Snapshot()
	remotes = sortRemotes(snap, sortMode, groupSortOn)
```

Update both `groupView{...}` literals (lines 375 and 460) to add `groupSort: groupSortOn`:

```go
		gv := groupView{groups: groups, filter: groupFilterState, query: textFilter.effectiveQuery(), hideDisabled: hideDisabled, groupSort: groupSortOn}
```

and, at the `BuildTableFrame` call site:

```go
		}, cols, 0, sortMode, groupView{groups: groups, filter: groupFilterState, query: textFilter.effectiveQuery(), hideDisabled: hideDisabled, groupSort: groupSortOn})
```

Replace the `case "s", "S":` block (the one Task 3 temporarily patched) with:

```go
			case "s", "S":
				screen.Invalidate()
				if picked, gs, ok := pickSortMode(sortMode, groupSortOn, modalWakes); ok && (picked != sortMode || gs != groupSortOn) {
					sortMode = picked
					groupSortOn = gs
					SaveSortMode(sortMode)
					SaveGroupSort(groupSortOn)
					toast = "sort: " + sortDesc(sortMode)
					if groupSortOn {
						toast += " · group first"
					}
					toastUntil = time.Now().Add(4 * time.Second)
					refresh(false)
				}
				screen.Invalidate()
				render()
```

Update `sortRemotes`:

```go
func sortRemotes(remotes []RemoteResult, mode string, groupSort bool) []RemoteResult {
	out := make([]RemoteResult, len(remotes))
	for i, r := range remotes {
		sorted := append([]Session(nil), r.Sessions...)
		SortSessions(sorted, mode, groupSort)
		r.Sessions = sorted
		out[i] = r
	}
	return out
}
```

`sortRemotes`'s signature just changed from 2 args to 3, so every other caller needs fixing too, not just `settleRows` above. `main.go:136` and `commands.go:486` both still call `sortRemotes(remotes, sortMode)` at this point — temporarily pass `false` there (the same pattern Task 2 used for `SortSessions`'s other callers); Task 6 replaces these with real `groupSortOn` wiring:
- `main.go:136`: `remotes = sortRemotes(remotes, sortMode)` → `remotes = sortRemotes(remotes, sortMode, false)`
- `commands.go:486`: `remotes = sortRemotes(remotes, sortMode)` → `remotes = sortRemotes(remotes, sortMode, false)`

Update the `renderHelp` call site (around line 954):

```go
				_ = screen.Draw(renderHelp(sortMode, groupSortOn), cols, rows)
```

Update `renderHelp`'s signature and its sort-dialog lines (1344, 1378-1379):

```go
func renderHelp(sortMode string, groupSortOn bool) string {
	var b strings.Builder
	fmt.Fprintln(&b, bold("claude-sessions  ·  help"))
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "  "+bold("NAVIGATION"))
	fmt.Fprintln(&b, "    ↑ / ↓        move selection")
	fmt.Fprintln(&b, "    Tab          jump to topmost idle (or shell) session")
	fmt.Fprintln(&b, "    mouse click  select row · double-click opens")
	fmt.Fprintln(&b, "    mouse wheel  scroll list or inspector")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "  "+bold("ACTIONS")+"  (on selected row)")
	fmt.Fprintln(&b, "    n            new tmux session (↑/↓ cwd · ←/→ command · p prompt in background)")
	fmt.Fprintln(&b, "    r            resume a past session (searchable · local + remote)")
	fmt.Fprintln(&b, "    - / +        disable / enable session")
	fmt.Fprintln(&b, "    Shift-1..9   assign session to group ①..⑨ (same group again ungroups)")
	fmt.Fprintln(&b, "    Ctrl-X       kill the session (tmux-aware)")
	fmt.Fprintln(&b, "    a            attach (or migrate to tmux first)")
	fmt.Fprintln(&b, "    Enter / p    open full-screen inspector")
	fmt.Fprintln(&b, "    i            show session info (ticket + conversation summary)")
	fmt.Fprintln(&b, "    Ctrl-W       switch that host's Claude account (⏎ applies · esc cancels)")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "  "+bold("INSPECTOR"))
	fmt.Fprintln(&b, "    Home / End   oldest output / resume live follow")
	fmt.Fprintln(&b, "    PgUp / PgDn  scroll inspector by page")
	fmt.Fprintln(&b, "    r            refresh now")
	fmt.Fprintln(&b, "    Ctrl-X       kill the session (tmux-aware)")
	fmt.Fprintln(&b, "    Esc / q / p  return from inspector")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "  "+bold("VIEW"))
	fmt.Fprintln(&b, "    m            cycle mode (full → intermediate → minimal)  ·  persisted")
	fmt.Fprintln(&b, "    1..9         show only group (same digit or 0 shows all)")
	fmt.Fprintln(&b, "    h then 1..9  hide group(s) (repeat to add/remove · last one shows all)")
	fmt.Fprintln(&b, "    d            hide/show disabled sessions")
	fmt.Fprintln(&b, "    /            filter rows by text (type to narrow · Enter commits · Esc clears)")
	fmt.Fprintln(&b, "    s / S        open sort-by dialog (↑/↓ select · g group first · ⏎ confirm · esc cancel)")
	groupSortLabel := "off"
	if groupSortOn {
		groupSortLabel = "on"
	}
	fmt.Fprintln(&b, "                 current sort: "+sortMode+"  ·  group first: "+groupSortLabel)
	fmt.Fprintln(&b, "    q / Ctrl-C   quit")
	fmt.Fprintln(&b, "    ?            this help")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "  "+bold("SUBCOMMANDS")+"  (from the shell)")
```

(Leave everything after the `SUBCOMMANDS` heading — not shown above — untouched.)

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add tui.go tui_test.go main.go commands.go
git commit -m "feat: wire group-first sort toggle into the live TUI"
```

---

### Task 6: CLI list paths (`main.go`, `commands.go`)

**Files:**
- Modify: `main.go:122-136` (`cmdList`), `commands.go:458-486` (`cmdListSessions`)

**Interfaces:**
- Consumes: `LoadGroupSort` (Task 1), `SortSessions(rows, mode, groupSort)` / `sortRemotes(remotes, mode, groupSort)` (Tasks 2/5).

These two non-interactive paths already call `LoadSortMode()` once and reuse it for both `SortSessions` and `sortRemotes`; group-first sort follows the same pattern so `claude-sessions list` / `list-sessions` render row order identically to what the TUI would show. Both paths render through `RenderAll` (main.go:143, commands.go:522), which always builds its own `groupView{}` internally (render.go:1719) rather than taking one from its caller — so the `group↑` header badge (Task 4) never appears in CLI output, the same way the existing group-filter badge never appears there either today. This is intentional, existing scope for `RenderAll`, not a gap this task needs to close — row order is what these two commands promise, and that is fully wired by Task 5's `groupSortOn`/`SortSessions`/`sortRemotes` plumbing plus this task's two call sites below.

- [ ] **Step 1: Implement**

In `main.go`'s `cmdList`:

```go
	LoadFlagsStore().Overlay(local)
	sortMode := LoadSortMode()
	groupSortOn := LoadGroupSort()
	SortSessions(local, sortMode, groupSortOn)
	remotes = sortRemotes(remotes, sortMode, groupSortOn)
```

In `commands.go`'s `cmdListSessions`:

```go
	LoadFlagsStore().Overlay(local)
	sortMode := LoadSortMode()
	groupSortOn := LoadGroupSort()
	SortSessions(local, sortMode, groupSortOn)
	remotes = sortRemotes(remotes, sortMode, groupSortOn)
```

- [ ] **Step 2: Run the full suite to verify nothing broke**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS — no new test is needed here since `cmdList`/`cmdListSessions` have no existing dedicated unit tests exercising sort order (verified: no test file references `cmdList(` or `cmdListSessions(` directly with sort-order assertions); this task's correctness rides entirely on `SortSessions` (Task 2) and `sortRemotes` (Task 5's `TestSortRemotesGroupFirst`) already being covered, plus the build/vet/test gate here confirming the wiring compiles and nothing else regresses.

- [ ] **Step 3: Commit**

```bash
git add main.go commands.go
git commit -m "feat: honor group-first sort in list/list-sessions CLI output"
```

---

## Final verification (after all tasks)

- [ ] Run `go build ./... && go vet ./... && go test ./...` once more from a clean tree.
- [ ] Manually smoke-test: `make run`, open the TUI, assign a couple of sessions to different groups (Shift-1, Shift-2), press `s`, press `g` to turn group-first on, press Enter, confirm rows re-order group-first with ungrouped sessions at the bottom, confirm the header shows the `group↑` badge, confirm `?` help reflects `group first: on`. Quit and relaunch — confirm the toggle is still on (persistence).
