# Tmux Viewer Count Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Collect tmux session attached-client counts and show them in every session-list mode through the existing two-cell prefix, with whole-row reverse-video selection and no width increase.

**Architecture:** Extend the existing one-shot `tmux list-panes -a` collector so each pane PID maps to a locator plus optional attached-client count. Carry that tri-state value through `Session` JSON unchanged for local and remote rows. Centralize prefix and selection ANSI behavior in small render helpers, then use them from all three session-list renderers while preserving existing row metadata, sorting, viewport, and picker behavior.

**Tech Stack:** Go, standard library, existing `golang.org/x/sys` and `golang.org/x/term` dependencies, tmux format variables, ANSI SGR rendering.

## Global Constraints

- Count means tmux `#{session_attached}` for the whole tmux session, not exact pane visibility or human attention.
- Keep one tmux subprocess per refresh by extending existing `tmux list-panes -a` command.
- Preserve PID-self-first ancestry lookup before walking parent PIDs.
- Represent unavailable counts with `nil`, detached with pointer-to-`0`, and attached with pointer-to-positive count.
- Preserve mixed-version remote truthfulness: missing legacy JSON field must render unknown `·`, never detached `0`.
- Keep exact count in `Session`; render `+` for counts of ten or more.
- Use existing two-cell list prefix in full, intermediate, and minimal modes; do not increase row, header, or column width.
- Replace session-list and selectable empty-host arrows with one continuous reverse-video row; keep arrows in other pickers, inspectors, and modals.
- Keep unselected headless-row dimming and all existing unselected cell colors.
- Add no dependency and make no sorting, viewport, mouse-target, keyboard-navigation, server-handler, or remote-client protocol changes.

---

## File Structure

- Modify `tmux.go`: define pane metadata, parse tab-delimited tmux output, and return metadata from ancestry lookup.
- Create `tmux_test.go`: cover parser states and PID-self/ancestor metadata lookup.
- Modify `preview.go`: adapt preview and PID-to-tmux helpers to the richer ancestry result while retaining string-returning interfaces.
- Modify `session.go`: add JSON-compatible attached-count field and assign pane metadata during local collection.
- Modify `session_test.go`: lock nil/zero/positive JSON behavior.
- Modify `render.go`: add shared viewer-prefix and whole-row selection helpers; apply them to full, intermediate, minimal, and empty-host rows.
- Modify `render_test.go`: cover every prefix state, width invariants, continuous selected-row ANSI behavior, headless selection, and empty-host selection.
- No changes to `server.go` or `remote.go`: both already encode/decode `Session` directly.

---

### Task 1: Collect tmux pane metadata in one subprocess

**Files:**
- Modify: `tmux.go:9-33`
- Modify: `tmux.go:83-98`
- Modify: `preview.go:96-113`
- Modify: `preview.go:403-409`
- Modify: `session.go:162`
- Create: `tmux_test.go`

**Interfaces:**
- Consumes: tmux format variables `#{pane_pid}`, `#{session_name}`, `#{window_index}`, `#{pane_index}`, and `#{session_attached}`.
- Produces: `type tmuxPaneInfo struct { Location string; Attached *int }`.
- Produces: `parseTmuxPaneOutput(out string) map[int]tmuxPaneInfo`.
- Produces: `tmuxPaneMap() (map[int]tmuxPaneInfo, error)`.
- Produces: `walkTmuxPane(pid int, panes map[int]tmuxPaneInfo, ppid map[int]int) (tmuxPaneInfo, bool)`.
- Preserves: `captureTmuxPreview(pid int, limits PreviewLimits)`, `tmuxLocForPID(pid int) string`, and `Session.Tmux` locator behavior before Task 2 adds count propagation.

- [ ] **Step 1: Write failing parser and ancestry tests**

Create `tmux_test.go`:

```go
package main

import "testing"

func TestParseTmuxPaneOutput(t *testing.T) {
	got := parseTmuxPaneOutput(
		"101\talpha beta:0.1\t0\n" +
			"102\tsolo:1.2\t1\n" +
			"103\tpairing:2.3\t4\n" +
			"104\tcrowd:0.0\t12\n" +
			"105\tunknown:0.0\tbogus\n" +
			"106\tnegative:0.0\t-1\n" +
			"bad\tignored:0.0\t1\n" +
			"0\tignored-zero:0.0\t1\n" +
			"-2\tignored-negative:0.0\t1\n" +
			"107\t\t1\n" +
			"missing-fields\n",
	)

	if len(got) != 6 {
		t.Fatalf("parseTmuxPaneOutput returned %d panes, want 6: %#v", len(got), got)
	}

	cases := []struct {
		pid          int
		wantLocation string
		wantAttached *int
	}{
		{101, "alpha beta:0.1", intPtrForTmuxTest(0)},
		{102, "solo:1.2", intPtrForTmuxTest(1)},
		{103, "pairing:2.3", intPtrForTmuxTest(4)},
		{104, "crowd:0.0", intPtrForTmuxTest(12)},
		{105, "unknown:0.0", nil},
		{106, "negative:0.0", nil},
	}
	for _, tc := range cases {
		info, ok := got[tc.pid]
		if !ok {
			t.Fatalf("pane %d missing from %#v", tc.pid, got)
		}
		if info.Location != tc.wantLocation {
			t.Errorf("pane %d location = %q, want %q", tc.pid, info.Location, tc.wantLocation)
		}
		switch {
		case tc.wantAttached == nil && info.Attached != nil:
			t.Errorf("pane %d attached = %d, want nil", tc.pid, *info.Attached)
		case tc.wantAttached != nil && info.Attached == nil:
			t.Errorf("pane %d attached = nil, want %d", tc.pid, *tc.wantAttached)
		case tc.wantAttached != nil && *info.Attached != *tc.wantAttached:
			t.Errorf("pane %d attached = %d, want %d", tc.pid, *info.Attached, *tc.wantAttached)
		}
	}
}

func TestWalkTmuxPaneReturnsMetadata(t *testing.T) {
	selfAttached := 2
	parentAttached := 0
	panes := map[int]tmuxPaneInfo{
		42: {Location: "self:0.0", Attached: &selfAttached},
		7:  {Location: "parent:1.3", Attached: &parentAttached},
	}
	ppid := map[int]int{99: 7}

	tests := []struct {
		name         string
		pid          int
		wantFound    bool
		wantLocation string
		wantAttached int
	}{
		{"pid itself", 42, true, "self:0.0", 2},
		{"ancestor", 99, true, "parent:1.3", 0},
		{"missing", 123, false, "", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			info, found := walkTmuxPane(tc.pid, panes, ppid)
			if found != tc.wantFound {
				t.Fatalf("walkTmuxPane(%d) found = %v, want %v", tc.pid, found, tc.wantFound)
			}
			if !found {
				return
			}
			if info.Location != tc.wantLocation {
				t.Errorf("walkTmuxPane(%d) location = %q, want %q", tc.pid, info.Location, tc.wantLocation)
			}
			if info.Attached == nil || *info.Attached != tc.wantAttached {
				t.Errorf("walkTmuxPane(%d) attached = %v, want %d", tc.pid, info.Attached, tc.wantAttached)
			}
		})
	}
}

func intPtrForTmuxTest(n int) *int { return &n }
```

Empty location is the malformed-location case. Do not parse locator punctuation: tmux session names may contain spaces and punctuation.

- [ ] **Step 2: Run tests and verify failure**

Run:

```bash
go test ./... -run 'Test(ParseTmuxPaneOutput|WalkTmuxPaneReturnsMetadata)$'
```

Expected: FAIL to compile because `parseTmuxPaneOutput`, `tmuxPaneInfo`, and new `walkTmuxPane` signature do not exist.

- [ ] **Step 3: Implement metadata parsing and lookup**

Replace tmux pane collection and lookup in `tmux.go` with:

```go
type tmuxPaneInfo struct {
	Location string
	Attached *int
}

// tmuxPaneMap returns pane_pid → pane metadata for every tmux pane on the
// default server. Empty map (no error) if tmux isn't running.
func tmuxPaneMap() (map[int]tmuxPaneInfo, error) {
	out, err := exec.Command("tmux", "list-panes", "-a", "-F",
		"#{pane_pid}\t#{session_name}:#{window_index}.#{pane_index}\t#{session_attached}").Output()
	if err != nil {
		return map[int]tmuxPaneInfo{}, nil
	}
	return parseTmuxPaneOutput(string(out)), nil
}

func parseTmuxPaneOutput(out string) map[int]tmuxPaneInfo {
	panes := make(map[int]tmuxPaneInfo)
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) != 3 || fields[1] == "" {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid <= 0 {
			continue
		}

		info := tmuxPaneInfo{Location: fields[1]}
		attached, err := strconv.Atoi(strings.TrimSpace(fields[2]))
		if err == nil && attached >= 0 {
			info.Attached = &attached
		}
		panes[pid] = info
	}
	return panes
}

// walkTmuxPane returns tmux pane metadata if pid is a descendant of any tmux
// pane process. It checks pid itself first because `tmux new-session
// "claude ..."` makes claude the pane_pid directly.
func walkTmuxPane(pid int, panes map[int]tmuxPaneInfo, ppid map[int]int) (tmuxPaneInfo, bool) {
	cur := pid
	for i := 0; i < 32; i++ {
		if info, ok := panes[cur]; ok {
			return info, true
		}
		if cur <= 1 {
			return tmuxPaneInfo{}, false
		}
		cur = ppid[cur]
	}
	return tmuxPaneInfo{}, false
}
```

- [ ] **Step 4: Adapt existing string consumers without widening their interfaces**

Update `CollectLocal` in `session.go` so Task 1 remains buildable before `TmuxAttached` exists:

```go
if pane, found := walkTmuxPane(s.PID, panes, ppid); found {
	s.Tmux = pane.Location
}
```

Update `captureTmuxPreview` in `preview.go`:

```go
func captureTmuxPreview(pid int, limits PreviewLimits) (label, content string, err error) {
	panes, _ := tmuxPaneMap()
	ppid, _ := ppidMap()
	pane, found := walkTmuxPane(pid, panes, ppid)
	if !found {
		return "", "", errNoTmuxPane
	}
	out, err := exec.Command("tmux", "capture-pane", "-p", "-e",
		"-S", "-"+strconv.Itoa(limits.MaxLines), "-t", pane.Location).Output()
	if err != nil {
		return "", "", fmt.Errorf("tmux capture-pane: %w", err)
	}
	return "tmux pane " + pane.Location, string(out), nil
}
```

Update `tmuxLocForPID`:

```go
func tmuxLocForPID(pid int) string {
	panes, _ := tmuxPaneMap()
	ppid, _ := ppidMap()
	pane, found := walkTmuxPane(pid, panes, ppid)
	if !found {
		return ""
	}
	return pane.Location
}
```

- [ ] **Step 5: Format and run focused tests**

Run:

```bash
gofmt -w tmux.go tmux_test.go preview.go session.go
go test ./... -run 'Test(ParseTmuxPaneOutput|WalkTmuxPaneReturnsMetadata)$'
```

Expected: PASS.

- [ ] **Step 6: Run full tests and commit**

Run:

```bash
go test ./...
git add tmux.go tmux_test.go preview.go session.go
git commit -m "feat: collect tmux attached client counts"
```

Expected: tests PASS; commit contains collector, parser, lookup, caller adaptations, and focused tests.

---

### Task 2: Carry attached count through Session and JSON

**Files:**
- Modify: `session.go:15-50`
- Modify: `session.go:126-177`
- Modify: `session_test.go:219-234`

**Interfaces:**
- Consumes: `walkTmuxPane(...) (tmuxPaneInfo, bool)` from Task 1.
- Produces: `Session.TmuxAttached *int` with JSON name `tmuxAttached` and `omitempty`.
- Preserves: direct `Session` encoding in `server.go` and direct decoding in `remote.go`; no transport-specific code changes.

- [ ] **Step 1: Write failing JSON compatibility test**

Append to `session_test.go`:

```go
func TestSessionTmuxAttachedJSONCompatibility(t *testing.T) {
	zero := 0
	positive := 3
	cases := []struct {
		name       string
		attached   *int
		wantJSON   string
		absentJSON bool
	}{
		{"unknown omitted", nil, "", true},
		{"detached retained", &zero, `"tmuxAttached":0`, false},
		{"positive retained", &positive, `"tmuxAttached":3`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(Session{Tmux: "dev:0.0", TmuxAttached: tc.attached})
			if err != nil {
				t.Fatal(err)
			}
			if tc.absentJSON {
				if strings.Contains(string(data), "tmuxAttached") {
					t.Fatalf("marshaled JSON unexpectedly contains tmuxAttached: %s", data)
				}
			} else if !strings.Contains(string(data), tc.wantJSON) {
				t.Fatalf("marshaled JSON = %s, want field %s", data, tc.wantJSON)
			}

			var roundTrip Session
			if err := json.Unmarshal(data, &roundTrip); err != nil {
				t.Fatal(err)
			}
			switch {
			case tc.attached == nil && roundTrip.TmuxAttached != nil:
				t.Fatalf("round-trip count = %v, want nil", roundTrip.TmuxAttached)
			case tc.attached != nil && roundTrip.TmuxAttached == nil:
				t.Fatalf("round-trip count = nil, want %d", *tc.attached)
			case tc.attached != nil && *roundTrip.TmuxAttached != *tc.attached:
				t.Fatalf("round-trip count = %d, want %d", *roundTrip.TmuxAttached, *tc.attached)
			}
		})
	}

	var legacy Session
	if err := json.Unmarshal([]byte(`{"pid":1,"tmux":"legacy:0.0"}`), &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.TmuxAttached != nil {
		t.Fatalf("legacy missing field decoded as %v, want nil", legacy.TmuxAttached)
	}

	var detached Session
	if err := json.Unmarshal([]byte(`{"pid":2,"tmux":"dev:0.0","tmuxAttached":0}`), &detached); err != nil {
		t.Fatal(err)
	}
	if detached.TmuxAttached == nil || *detached.TmuxAttached != 0 {
		t.Fatalf("detached count decoded as %v, want pointer to 0", detached.TmuxAttached)
	}
}
```

- [ ] **Step 2: Run test and verify failure**

Run:

```bash
go test ./... -run '^TestSessionTmuxAttachedJSONCompatibility$'
```

Expected: FAIL to compile because `Session.TmuxAttached` does not exist.

- [ ] **Step 3: Add Session field and collection assignment**

Add beside `Session.Tmux` in `session.go`:

```go
CPU          string `json:"cpu"`
Tmux         string `json:"tmux"` // "session:win.pane" or "" if not in tmux
TmuxAttached *int   `json:"tmuxAttached,omitempty"`
```

Extend Task 1's metadata assignment in `CollectLocal`:

```go
if pane, found := walkTmuxPane(s.PID, panes, ppid); found {
	s.Tmux = pane.Location
	s.TmuxAttached = pane.Attached
}
```

No-pane rows keep `Tmux == ""` and `TmuxAttached == nil`. A valid pane with malformed count keeps its locator and a nil count.

- [ ] **Step 4: Format and run focused tests**

Run:

```bash
gofmt -w session.go session_test.go
go test ./... -run 'Test(SessionTmuxAttachedJSONCompatibility|ParseTmuxPaneOutput|WalkTmuxPaneReturnsMetadata)$'
```

Expected: PASS.

- [ ] **Step 5: Run full tests and commit**

Run:

```bash
go test ./...
git add session.go session_test.go
git commit -m "feat: expose tmux attachment counts on sessions"
```

Expected: tests PASS; zero survives JSON, nil remains absent, positive count survives transport.

---

### Task 3: Add shared viewer-prefix and selected-row helpers

**Files:**
- Modify: `render.go:3-10`
- Modify: `render.go:36-44`
- Modify: `render_test.go:54-64`

**Interfaces:**
- Consumes: `Session.Tmux` and `Session.TmuxAttached` from Task 2.
- Produces: `tmuxViewerSymbol(s Session) (symbol, sgr string)`.
- Produces: `tmuxViewerPrefix(s Session, plain bool) string` with exactly two visual cells.
- Produces: `highlightSelectedRow(row string, selected bool) string`.
- Styling contract: unknown and zero use SGR `2`; positive and `+` use SGR `1;32`; selected prefixes are plain before whole-row inversion.

- [ ] **Step 1: Write failing helper tests**

Append near other render helper tests in `render_test.go`:

```go
func TestTmuxViewerPrefix(t *testing.T) {
	zero := 0
	one := 1
	nine := 9
	ten := 10
	negative := -1
	cases := []struct {
		name  string
		s     Session
		plain bool
		want  string
	}{
		{"no tmux", Session{}, false, "  "},
		{"unknown", Session{Tmux: "dev:0.0"}, false, dim("· ")},
		{"detached", Session{Tmux: "dev:0.0", TmuxAttached: &zero}, false, dim("0 ")},
		{"one", Session{Tmux: "dev:0.0", TmuxAttached: &one}, false, colorize("1;32", "1 ")},
		{"nine", Session{Tmux: "dev:0.0", TmuxAttached: &nine}, false, colorize("1;32", "9 ")},
		{"ten", Session{Tmux: "dev:0.0", TmuxAttached: &ten}, false, colorize("1;32", "+ ")},
		{"negative unknown", Session{Tmux: "dev:0.0", TmuxAttached: &negative}, false, dim("· ")},
		{"plain unknown", Session{Tmux: "dev:0.0"}, true, "· "},
		{"plain detached", Session{Tmux: "dev:0.0", TmuxAttached: &zero}, true, "0 "},
		{"plain positive", Session{Tmux: "dev:0.0", TmuxAttached: &one}, true, "1 "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tmuxViewerPrefix(tc.s, tc.plain); got != tc.want {
				t.Errorf("tmuxViewerPrefix() = %q, want %q", got, tc.want)
			}
			if got := visualLen(tmuxViewerPrefix(tc.s, tc.plain)); got != 2 {
				t.Errorf("tmuxViewerPrefix() visual width = %d, want 2", got)
			}
		})
	}
}

func TestHighlightSelectedRow(t *testing.T) {
	if got := highlightSelectedRow("2 row", false); got != "2 row" {
		t.Errorf("unselected row = %q, want unchanged", got)
	}
	want := ansiInvert + "2 row" + ansiReset
	if got := highlightSelectedRow("2 row", true); got != want {
		t.Errorf("selected row = %q, want %q", got, want)
	}
}
```

- [ ] **Step 2: Run tests and verify failure**

Run:

```bash
go test ./... -run 'Test(TmuxViewerPrefix|HighlightSelectedRow)$'
```

Expected: FAIL to compile because render helpers do not exist.

- [ ] **Step 3: Implement render helpers**

Add `strconv` to `render.go` imports:

```go
import (
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)
```

Add after `dim`:

```go
func tmuxViewerSymbol(s Session) (symbol, sgr string) {
	if s.Tmux == "" {
		return " ", ""
	}
	if s.TmuxAttached == nil || *s.TmuxAttached < 0 {
		return "·", "2"
	}
	attached := *s.TmuxAttached
	switch {
	case attached == 0:
		return "0", "2"
	case attached < 10:
		return strconv.Itoa(attached), "1;32"
	default:
		return "+", "1;32"
	}
}

func tmuxViewerPrefix(s Session, plain bool) string {
	symbol, sgr := tmuxViewerSymbol(s)
	prefix := symbol + " "
	if plain || sgr == "" {
		return prefix
	}
	return colorize(sgr, prefix)
}

func highlightSelectedRow(row string, selected bool) string {
	if !selected {
		return row
	}
	return ansiInvert + row + ansiReset
}
```

- [ ] **Step 4: Format and run focused tests**

Run:

```bash
gofmt -w render.go render_test.go
go test ./... -run 'Test(TmuxViewerPrefix|HighlightSelectedRow)$'
```

Expected: PASS.

- [ ] **Step 5: Run full tests and commit**

Run:

```bash
go test ./...
git add render.go render_test.go
git commit -m "feat: add tmux viewer rendering helpers"
```

Expected: existing renderer output remains unchanged because helpers are not wired into row rendering yet.

---

### Task 4: Render viewer counts and whole-row selection in every list mode

**Files:**
- Modify: `render.go:408-418`
- Modify: `render.go:658-700`
- Modify: `render.go:770-805`
- Modify: `render.go:892-924`
- Modify: `render_test.go:54-93`
- Modify: `render_test.go:610-698`
- Test: `render_test.go`

**Interfaces:**
- Consumes: `tmuxViewerPrefix(Session, plain)`, `highlightSelectedRow(string, bool)`, existing `modelCell(..., plain)`, and existing `ctxCell(..., plain)`.
- Produces: same textual table and `frameWriter` row metadata interfaces, except approved prefix and selection visuals.
- Selection contract: selected session and empty-host rows contain one `ansiInvert` prefix and one final `ansiReset`, with no reset inside the highlighted content.

- [ ] **Step 1: Add shared selected-row assertion and viewer integration tests**

Add after `findRow` in `render_test.go`:

```go
func assertWholeRowSelected(t *testing.T, row, prefix string) {
	t.Helper()
	if !strings.HasPrefix(row, ansiInvert+prefix) {
		t.Fatalf("selected row lacks continuous invert prefix %q: %q", prefix, row)
	}
	if !strings.HasSuffix(row, ansiReset) {
		t.Fatalf("selected row lacks final reset: %q", row)
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(row, ansiInvert), ansiReset)
	if strings.Contains(inner, ansiReset) {
		t.Fatalf("selected row contains nested reset: %q", row)
	}
	if strings.Contains(row, "▶") {
		t.Fatalf("selected row still contains arrow: %q", row)
	}
}

func renderSessionRowForTest(t *testing.T, mode string, s Session, selected bool) string {
	t.Helper()
	sel := ""
	if selected {
		sel = s.ID()
	}
	var b strings.Builder
	RenderAll(&b, mode, []Session{s}, nil, sel, nil, 0, 0, "dir")
	return findRow(t, b.String(), s.Name)
}

func TestTmuxViewerPrefixesAcrossModes(t *testing.T) {
	now := time.Now().UnixMilli()
	zero := 0
	one := 1
	nine := 9
	ten := 10
	cases := []struct {
		name     string
		tmux     string
		attached *int
		want     string
	}{
		{"plain", "", nil, "  "},
		{"unknown", "unknown:0.0", nil, dim("· ")},
		{"detached", "detached:0.0", &zero, dim("0 ")},
		{"one", "one:0.0", &one, colorize("1;32", "1 ")},
		{"nine", "nine:0.0", &nine, colorize("1;32", "9 ")},
		{"ten", "ten:0.0", &ten, colorize("1;32", "+ ")},
	}
	for _, mode := range []string{"1", "2", "3"} {
		for i, tc := range cases {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				s := Session{
					PID:          100 + i,
					Name:         tc.name,
					NameSource:   "user",
					CWD:          "/work/" + tc.name,
					Status:       "idle",
					Entrypoint:   "cli",
					UpdatedAt:    now,
					Tmux:         tc.tmux,
					TmuxAttached: tc.attached,
				}
				row := renderSessionRowForTest(t, mode, s, false)
				if !strings.HasPrefix(row, tc.want) {
					t.Errorf("mode %s row prefix = %q, want %q in row %q", mode, row[:len(tc.want)], tc.want, row)
				}
			})
		}
	}
}

func TestSelectedSessionRowsInvertWholeRow(t *testing.T) {
	now := time.Now().UnixMilli()
	attached := 2
	for _, mode := range []string{"1", "2", "3"} {
		s := Session{
			PID: 42, Name: "selected", NameSource: "user", CWD: "/work/selected",
			Status: "busy", Entrypoint: "cli", UpdatedAt: now,
			Tmux: "selected:0.0", TmuxAttached: &attached,
		}
		selectedRow := renderSessionRowForTest(t, mode, s, true)
		assertWholeRowSelected(t, selectedRow, "2 ")

		unselectedRow := renderSessionRowForTest(t, mode, s, false)
		if visualLen(selectedRow) != visualLen(unselectedRow) {
			t.Errorf("mode %s selected width = %d, unselected width = %d", mode, visualLen(selectedRow), visualLen(unselectedRow))
		}
	}
}

func TestSelectedHeadlessRowsSuppressDim(t *testing.T) {
	now := time.Now().UnixMilli()
	attached := 1
	for _, mode := range []string{"1", "2", "3"} {
		s := Session{
			PID: 77, Name: "headless", NameSource: "user", CWD: "/work/headless",
			Status: "busy", Entrypoint: "sdk-cli", StartedAt: now,
			Tmux: "headless:0.0", TmuxAttached: &attached,
		}
		row := renderSessionRowForTest(t, mode, s, true)
		assertWholeRowSelected(t, row, "1 ")
		inner := strings.TrimSuffix(strings.TrimPrefix(row, ansiInvert), ansiReset)
		if strings.Contains(inner, ansiDim) {
			t.Errorf("mode %s selected headless row contains dim wrapper: %q", mode, row)
		}
	}
}

func TestTmuxViewerPrefixPreservesWidth(t *testing.T) {
	now := time.Now().UnixMilli()
	attached := 3
	for _, mode := range []string{"1", "2", "3"} {
		unknown := Session{
			PID: 88, Name: "width", NameSource: "user", CWD: "/work/width",
			Status: "idle", Entrypoint: "cli", UpdatedAt: now, Tmux: "width:0.0",
		}
		known := unknown
		known.TmuxAttached = &attached

		var unknownOut, knownOut strings.Builder
		RenderAll(&unknownOut, mode, []Session{unknown}, nil, "", nil, 0, 0, "dir")
		RenderAll(&knownOut, mode, []Session{known}, nil, "", nil, 0, 0, "dir")
		unknownRow := findRow(t, unknownOut.String(), unknown.Name)
		knownRow := findRow(t, knownOut.String(), known.Name)
		if visualLen(unknownRow) != visualLen(knownRow) {
			t.Errorf("mode %s unknown width = %d, known width = %d", mode, visualLen(unknownRow), visualLen(knownRow))
		}

		headerNeedle := "PID"
		if mode != "1" {
			headerNeedle = "DIR"
		}
		unknownHeader := findRow(t, unknownOut.String(), headerNeedle)
		knownHeader := findRow(t, knownOut.String(), headerNeedle)
		if unknownHeader != knownHeader {
			t.Errorf("mode %s header changed with viewer count:\nunknown: %q\nknown:   %q", mode, unknownHeader, knownHeader)
		}
	}
}
```

- [ ] **Step 2: Update empty-host selection tests to require whole-row inversion**

Replace arrow assertions in `TestEmptyRemoteHostSelectionMarker` and `TestEmptyLocalHostSelectionMarker` with:

```go
assertWholeRowSelected(t, row, "  ")
```

Rename both tests from `...SelectionMarker` to `...SelectionHighlight`.

In `TestEmptyLocalAndRemoteCoexist`, replace selected/unselected arrow checks with:

```go
assertWholeRowSelected(t, rows[0], "  ")
if strings.HasPrefix(rows[1], ansiInvert) {
	t.Fatalf("unselected empty-remote row wrongly highlighted: %q", rows[1])
}
```

Keep existing header-count assertions unchanged. Keep unselected empty-host tests unchanged; they still verify two leading spaces and no arrow.

- [ ] **Step 3: Run integration tests and verify failure**

Run:

```bash
go test ./... -run 'Test(TmuxViewerPrefixesAcrossModes|SelectedSessionRowsInvertWholeRow|SelectedHeadlessRowsSuppressDim|TmuxViewerPrefixPreservesWidth|Empty.*SelectionHighlight|EmptyLocalAndRemoteCoexist)$'
```

Expected: FAIL because renderers still emit arrows/dots and do not invert complete rows.

- [ ] **Step 4: Implement empty-host whole-row selection**

Replace `renderEmptyHostRow` in `render.go`:

```go
// renderEmptyHostRow prints the selectable "(no sessions)" placeholder for a
// reachable local or remote host.
func renderEmptyHostRow(w *frameWriter, host, sel string) {
	selected := sel == emptyHostSelectionID(host)
	body := "(no sessions)"
	row := "  " + dim(body)
	if selected {
		row = highlightSelectedRow("  "+body, true)
	}
	w.record(emptyHostSelectionID(host), false)
	fmt.Fprintln(w, row)
}
```

- [ ] **Step 5: Replace full-view row rendering**

Replace `renderAllFull`'s `rowFn` with:

```go
rowFn := func(rows []drowFull) {
	for _, r := range rows {
		selected := r.s.ID() == sel
		ghost := r.s.Headless()
		plainCells := selected || ghost

		tmuxStr := r.s.Tmux
		if tmuxStr == "" {
			tmuxStr = "-"
		}
		tmuxCell := fmt.Sprintf("%-*s", tmuxW, tmuxStr)
		if r.s.Tmux == "" && !plainCells {
			tmuxCell = dim(tmuxCell)
		}
		statusCell := fmt.Sprintf("%-*s", statusW, r.statusStr)
		if !plainCells {
			statusCell = colorize(statusColor[r.s.Status], statusCell)
		}
		nameCell := fmt.Sprintf("%-*s", nameW, r.nameStr)
		if r.nameDim && !plainCells {
			nameCell = dim(nameCell)
		}
		if utf8.RuneCountInString(r.cwdStr) > dirW {
			overflowing = true
		}
		body := fmt.Sprintf("%7d  %s  %s  %s  %s  %s  %*s  %s  %s  %5s  %5s  %-8s  %s",
			r.s.PID,
			nameCell,
			marqueeCell(r.cwdStr, dirW, step),
			modelCell(r.modelStr, modelW, plainCells),
			statusCell,
			costCell(r.costStr, costW),
			agentsW, r.agentsStr,
			ctxCell(r.ctxStr, r.s.ContextTokens, plainCells),
			tmuxCell,
			r.s.CPU, r.ageStr, r.s.Version, r.sidShort)
		if ghost && !selected {
			body = dim(body)
		}
		row := tmuxViewerPrefix(r.s, selected) + body
		row = highlightSelectedRow(row, selected)
		w.record(r.s.ID(), true)
		fmt.Fprintln(w, row)
	}
}
```

- [ ] **Step 6: Replace intermediate-view row rendering**

Replace `renderAllIntermediate`'s `rowFn` with:

```go
rowFn := func(rows []drowFull) {
	for _, r := range rows {
		selected := r.s.ID() == sel
		ghost := r.s.Headless()
		plainCells := selected || ghost

		statusCell := fmt.Sprintf("%-*s", statusW, r.statusStr)
		if !plainCells {
			statusCell = colorize(statusColor[r.s.Status], statusCell)
		}
		nameCell := fmt.Sprintf("%-*s", nameW, r.nameStr)
		if r.nameDim && !plainCells {
			nameCell = dim(nameCell)
		}
		if utf8.RuneCountInString(r.cwdStr) > dirW {
			overflowing = true
		}
		body := fmt.Sprintf("%s  %s  %s  %s  %s  %*s  %s  %5s  %5s",
			nameCell,
			marqueeCell(r.cwdStr, dirW, step),
			modelCell(r.modelStr, modelW, plainCells),
			statusCell,
			costCell(r.costStr, costW),
			agentsW, r.agentsStr,
			ctxCell(r.ctxStr, r.s.ContextTokens, plainCells),
			r.s.CPU, r.ageStr)
		if ghost && !selected {
			body = dim(body)
		}
		row := tmuxViewerPrefix(r.s, selected) + body
		row = highlightSelectedRow(row, selected)
		w.record(r.s.ID(), true)
		fmt.Fprintln(w, row)
	}
}
```

- [ ] **Step 7: Replace minimal-view row rendering**

Replace `renderAllMinimal`'s `rowFn` with:

```go
rowFn := func(rows []drowMinimal) {
	for _, r := range rows {
		selected := r.s.ID() == sel
		ghost := r.s.Headless()
		plainCells := selected || ghost

		glyph := statusGlyph[r.s.Status]
		if glyph == "" {
			glyph = "?"
		}
		statusCell := glyph
		if !plainCells {
			statusCell = colorize(statusColor[r.s.Status], glyph)
		}
		nameCell := fmt.Sprintf("%-*s", nameW, r.display)
		if r.nameDim && !plainCells {
			nameCell = dim(nameCell)
		}
		if utf8.RuneCountInString(r.dir) > dirW {
			overflowing = true
		}
		body := fmt.Sprintf("%s  %s  %s  %5s",
			marqueeCell(r.dir, dirW, step), nameCell, statusCell, r.ageStr)
		if ghost && !selected {
			body = dim(body)
		}
		row := tmuxViewerPrefix(r.s, selected) + body
		row = highlightSelectedRow(row, selected)
		w.record(r.s.ID(), true)
		fmt.Fprintln(w, row)
	}
}
```

- [ ] **Step 8: Format and run focused rendering tests**

Run:

```bash
gofmt -w render.go render_test.go
go test ./... -run 'Test(TmuxViewerPrefix|HighlightSelectedRow|TmuxViewerPrefixesAcrossModes|SelectedSessionRowsInvertWholeRow|SelectedHeadlessRowsSuppressDim|TmuxViewerPrefixPreservesWidth|HeadlessRowsDimmed|Empty.*|RenderAllMatchesBuildTableFrame)$'
```

Expected: PASS. `TestHeadlessRowsDimmed` confirms unselected headless bodies remain one continuous dim span. `TestRenderAllMatchesBuildTableFrame` confirms text/frame compatibility and row metadata path remain synchronized.

- [ ] **Step 9: Run full tests and commit**

Run:

```bash
go test ./...
git add render.go render_test.go
git commit -m "feat: show tmux viewer counts in session list"
```

Expected: tests PASS; session-list arrows are gone, viewer counts remain visible while selected, and empty-host selection uses the same whole-row highlight.

---

### Task 5: Verify complete feature

**Files:**
- Verify only; no source changes expected.

**Interfaces:**
- Verifies collector, model, JSON transport, renderer behavior, static analysis, and final binary compilation together.

- [ ] **Step 1: Run clean test suite**

Run:

```bash
go test ./... -count=1
```

Expected: PASS with no cached test results.

- [ ] **Step 2: Run static analysis**

Run:

```bash
go vet ./...
```

Expected: PASS with no output.

- [ ] **Step 3: Build host binary**

Run:

```bash
go build .
```

Expected: PASS and produce local `claude-sessions` binary if default output is not redirected by module settings.

- [ ] **Step 4: Check formatting and worktree state**

Run:

```bash
gofmt -d tmux.go tmux_test.go preview.go session.go session_test.go render.go render_test.go
git diff --check
git status --short
```

Expected: `gofmt -d` and `git diff --check` print nothing. `git status --short` shows only an optional untracked build artifact; remove that artifact before delivery if `go build .` created it.

- [ ] **Step 5: Review final commit sequence**

Run:

```bash
git log --oneline --decorate -6
git diff --stat HEAD~4..HEAD
```

Expected: four focused feature commits after plan/design commits, covering tmux metadata, Session transport, render helpers, and renderer integration.
