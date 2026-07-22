# Flicker-Free TUI Rendering Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace clear-before-paint TUI redraws with synchronized, line-diffed screen patches across the session list, inspector, help, and new-session picker.

**Architecture:** Add an isolated `screenRenderer` that normalizes each screen into terminal-sized logical rows, caches the last successful frame, and emits only changed rows in one synchronized-output patch. `RunTUI` keeps terminal ownership and routes list, inspector, and help content through one renderer; the picker uses its own temporary renderer because it owns the alternate screen during its input loop.

**Tech Stack:** Go standard library, existing `golang.org/x/term`, ANSI/CSI terminal sequences, existing `clipLine` renderer helper.

## Global Constraints

- Keep one Go binary and add no dependency.
- Keep `RunTUI` as the only owner of top-level terminal lifecycle and stdin.
- Preserve the single-stdin-consumer invariant.
- Preserve `enableOutputProcessing(fd)` whenever raw mode is entered.
- Do not change table, inspector, help, or picker content and layout except removal of terminal-control prefixes.
- Do not change selection, scrolling, hit regions, refresh cadence, mouse handling, prompts, or interactive handoff behavior.
- Never clear the display before an alternate-screen frame repaint.
- Every non-empty patch must use one writer `Write` call and balanced `CSI ? 2026 h` / `CSI ? 2026 l` markers.
- Unsupported synchronized-output terminals must still receive a correct line diff.
- Treat drawing as best-effort in the TUI; renderer unit tests must verify returned errors and cache invalidation.
- Design reference: `docs/superpowers/specs/2026-07-22-flicker-free-rendering-design.md`.

## File Structure

- Create `screen_renderer.go`: terminal frame normalization, cache, invalidation, diff generation, synchronized patch output, and unknown-size fallback.
- Create `screen_renderer_test.go`: complete behavior contract for renderer bytes, writer calls, cache, dimensions, ANSI rows, and errors.
- Modify `tui.go`: route session list, inspector, and help through shared renderer; invalidate at view and action ownership boundaries; make help a pure content builder.
- Modify `tui_state.go`: add pure bottom-row composition helper for toast placement.
- Modify `tui_state_test.go`: verify bottom-row padding, truncation, and replacement.
- Modify `tui_test.go`: verify help content remains user-visible and contains no cursor/display controls.
- Modify `new_picker.go`: remove embedded clear/home commands and draw picker through a local renderer using current terminal dimensions.
- Modify `new_picker_test.go`: verify picker content contains no cursor/display controls.

---

### Task 1: Screen Diff Renderer

**Files:**
- Create: `screen_renderer.go`
- Create: `screen_renderer_test.go`

**Interfaces:**
- Consumes: existing `clipLine(string, int) string` and `ansiReset` from package `main`.
- Produces: `newScreenRenderer(w io.Writer) *screenRenderer`, `(*screenRenderer).Draw(content string, cols, rows int) error`, and `(*screenRenderer).Invalidate()`.

- [ ] **Step 1: Write failing renderer tests**

Create `screen_renderer_test.go`:

```go
package main

import (
	"errors"
	"io"
	"strings"
	"testing"
)

type recordingScreenWriter struct {
	writes [][]byte
	err    error
	limit  int
}

func (w *recordingScreenWriter) Write(p []byte) (int, error) {
	w.writes = append(w.writes, append([]byte(nil), p...))
	if w.err != nil {
		return 0, w.err
	}
	if w.limit > 0 && w.limit < len(p) {
		return w.limit, nil
	}
	return len(p), nil
}

func (w *recordingScreenWriter) last() string {
	if len(w.writes) == 0 {
		return ""
	}
	return string(w.writes[len(w.writes)-1])
}

func TestScreenRendererFirstDrawAndNoOp(t *testing.T) {
	w := &recordingScreenWriter{}
	r := newScreenRenderer(w)
	if err := r.Draw("one\ntwo", 10, 3); err != nil {
		t.Fatal(err)
	}
	if len(w.writes) != 1 {
		t.Fatalf("writes = %d, want 1", len(w.writes))
	}
	out := w.last()
	for _, want := range []string{
		screenSyncBegin,
		"\x1b[1;1Hone" + ansiReset + screenEraseLine,
		"\x1b[2;1Htwo" + ansiReset + screenEraseLine,
		"\x1b[3;1H" + ansiReset + screenEraseLine,
		screenSyncEnd,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("first draw missing %q: %q", want, out)
		}
	}
	if strings.Contains(out, "\x1b[J") || strings.Contains(out, "\x1b[2J") {
		t.Fatalf("first draw clears display: %q", out)
	}
	if strings.Count(out, screenSyncBegin) != 1 || strings.Count(out, screenSyncEnd) != 1 {
		t.Fatalf("unbalanced sync markers: %q", out)
	}

	if err := r.Draw("one\ntwo", 10, 3); err != nil {
		t.Fatal(err)
	}
	if len(w.writes) != 1 {
		t.Fatalf("unchanged draw wrote again: %d writes", len(w.writes))
	}
}

func TestScreenRendererWritesOnlyChangedRows(t *testing.T) {
	w := &recordingScreenWriter{}
	r := newScreenRenderer(w)
	if err := r.Draw("one\ntwo\nthree", 10, 3); err != nil {
		t.Fatal(err)
	}
	w.writes = nil

	if err := r.Draw("one\nTWO\nthree", 10, 3); err != nil {
		t.Fatal(err)
	}
	if len(w.writes) != 1 {
		t.Fatalf("writes = %d, want 1", len(w.writes))
	}
	out := w.last()
	if !strings.Contains(out, "\x1b[2;1HTWO"+ansiReset+screenEraseLine) {
		t.Fatalf("changed row missing: %q", out)
	}
	if strings.Contains(out, "\x1b[1;1H") || strings.Contains(out, "\x1b[3;1H") {
		t.Fatalf("unchanged rows repainted: %q", out)
	}
}

func TestScreenRendererErasesShortenedAndRemovedRows(t *testing.T) {
	w := &recordingScreenWriter{}
	r := newScreenRenderer(w)
	if err := r.Draw("long value\nremove me", 20, 3); err != nil {
		t.Fatal(err)
	}
	w.writes = nil

	if err := r.Draw("x", 20, 3); err != nil {
		t.Fatal(err)
	}
	out := w.last()
	if !strings.Contains(out, "\x1b[1;1Hx"+ansiReset+screenEraseLine) {
		t.Fatalf("shortened row does not erase suffix: %q", out)
	}
	if !strings.Contains(out, "\x1b[2;1H"+ansiReset+screenEraseLine) {
		t.Fatalf("removed row not cleared: %q", out)
	}
	if strings.Contains(out, "\x1b[3;1H") {
		t.Fatalf("unchanged blank row repainted: %q", out)
	}
}

func TestScreenRendererResizeAndInvalidateForceFullPaint(t *testing.T) {
	w := &recordingScreenWriter{}
	r := newScreenRenderer(w)
	if err := r.Draw("one\ntwo", 10, 2); err != nil {
		t.Fatal(err)
	}
	w.writes = nil

	if err := r.Draw("one\ntwo", 12, 3); err != nil {
		t.Fatal(err)
	}
	out := w.last()
	for _, want := range []string{"\x1b[1;1H", "\x1b[2;1H", "\x1b[3;1H"} {
		if !strings.Contains(out, want) {
			t.Fatalf("resize omitted %q: %q", want, out)
		}
	}

	w.writes = nil
	r.Invalidate()
	if err := r.Draw("one\ntwo", 12, 3); err != nil {
		t.Fatal(err)
	}
	out = w.last()
	for _, want := range []string{"\x1b[1;1H", "\x1b[2;1H", "\x1b[3;1H"} {
		if !strings.Contains(out, want) {
			t.Fatalf("invalidated draw missing %q: %q", want, out)
		}
	}
}

func TestScreenRendererClipsStyledRowsAndResetsStyle(t *testing.T) {
	w := &recordingScreenWriter{}
	r := newScreenRenderer(w)
	if err := r.Draw("\x1b[31mabcdef", 3, 1); err != nil {
		t.Fatal(err)
	}
	out := w.last()
	if !strings.Contains(out, "\x1b[31mabc"+ansiReset+screenEraseLine) {
		t.Fatalf("styled clipped row = %q", out)
	}
}

func TestScreenRendererUnknownSizeFallback(t *testing.T) {
	w := &recordingScreenWriter{}
	r := newScreenRenderer(w)
	if err := r.Draw("one\ntwo", 0, 0); err != nil {
		t.Fatal(err)
	}
	want := screenSyncBegin + screenHome + "one\ntwo" + ansiReset + screenSyncEnd
	if got := w.last(); got != want {
		t.Fatalf("fallback = %q, want %q", got, want)
	}
	if r.valid {
		t.Fatal("unknown-size draw left cache valid")
	}
	if strings.Contains(w.last(), "\x1b[J") || strings.Contains(w.last(), "\x1b[2J") {
		t.Fatalf("fallback clears display: %q", w.last())
	}
}

func TestScreenRendererWriteFailuresInvalidateCache(t *testing.T) {
	t.Run("writer error", func(t *testing.T) {
		good := &recordingScreenWriter{}
		r := newScreenRenderer(good)
		if err := r.Draw("one", 10, 1); err != nil {
			t.Fatal(err)
		}
		boom := errors.New("boom")
		r.w = &recordingScreenWriter{err: boom}
		if err := r.Draw("two", 10, 1); !errors.Is(err, boom) {
			t.Fatalf("error = %v, want %v", err, boom)
		}
		if r.valid {
			t.Fatal("writer error left cache valid")
		}
	})

	t.Run("short write", func(t *testing.T) {
		good := &recordingScreenWriter{}
		r := newScreenRenderer(good)
		if err := r.Draw("one", 10, 1); err != nil {
			t.Fatal(err)
		}
		r.w = &recordingScreenWriter{limit: 1}
		if err := r.Draw("two", 10, 1); !errors.Is(err, io.ErrShortWrite) {
			t.Fatalf("short write error = %v, want %v", err, io.ErrShortWrite)
		}
		if r.valid {
			t.Fatal("short write left cache valid")
		}
	})
}
```

- [ ] **Step 2: Run renderer tests and verify failure**

Run:

```sh
go test ./... -run '^TestScreenRenderer' -count=1
```

Expected: build fails because `newScreenRenderer`, `screenSyncBegin`, `screenSyncEnd`, `screenEraseLine`, and `screenHome` do not exist.

- [ ] **Step 3: Implement renderer**

Create `screen_renderer.go`:

```go
package main

import (
	"fmt"
	"io"
	"strings"
)

const (
	screenSyncBegin = "\x1b[?2026h"
	screenSyncEnd   = "\x1b[?2026l"
	screenHome      = "\x1b[H"
	screenEraseLine = "\x1b[K"
)

type screenRenderer struct {
	w        io.Writer
	previous []string
	cols     int
	rows     int
	valid    bool
}

func newScreenRenderer(w io.Writer) *screenRenderer {
	return &screenRenderer{w: w}
}

func (r *screenRenderer) Invalidate() {
	r.valid = false
}

func (r *screenRenderer) Draw(content string, cols, rows int) error {
	if cols <= 0 || rows <= 0 {
		r.Invalidate()
		return r.write(screenSyncBegin + screenHome + content + ansiReset + screenSyncEnd)
	}

	next := normalizedScreenRows(content, cols, rows)
	full := !r.valid || r.cols != cols || r.rows != rows || len(r.previous) != rows

	var patch strings.Builder
	for i, line := range next {
		if !full && line == r.previous[i] {
			continue
		}
		if patch.Len() == 0 {
			patch.WriteString(screenSyncBegin)
		}
		fmt.Fprintf(&patch, "\x1b[%d;1H", i+1)
		patch.WriteString(line)
		patch.WriteString(ansiReset)
		patch.WriteString(screenEraseLine)
	}
	if patch.Len() == 0 {
		return nil
	}
	patch.WriteString(screenSyncEnd)
	if err := r.write(patch.String()); err != nil {
		return err
	}

	r.previous = append(r.previous[:0], next...)
	r.cols = cols
	r.rows = rows
	r.valid = true
	return nil
}

func normalizedScreenRows(content string, cols, rows int) []string {
	input := strings.Split(content, "\n")
	out := make([]string, rows)
	if len(input) > rows {
		input = input[:rows]
	}
	for i, line := range input {
		out[i] = clipLine(line, cols)
	}
	return out
}

func (r *screenRenderer) write(payload string) error {
	n, err := r.w.Write([]byte(payload))
	if err != nil {
		r.Invalidate()
		return err
	}
	if n != len(payload) {
		r.Invalidate()
		return io.ErrShortWrite
	}
	return nil
}
```

- [ ] **Step 4: Run renderer tests and full render tests**

Run:

```sh
go test ./... -run '^(TestScreenRenderer|TestClipLine|TestRenderAllMatchesBuildTableFrame)' -count=1
```

Expected: PASS.

- [ ] **Step 5: Format and commit renderer**

Run:

```sh
gofmt -w screen_renderer.go screen_renderer_test.go
git add screen_renderer.go screen_renderer_test.go
git commit -m "feat: add flicker-free screen renderer" -m "Co-Authored-By: Claude <noreply@anthropic.com>"
```

Expected: one commit containing renderer and focused tests.

---

### Task 2: Main TUI, Inspector, Toast, and Help Integration

**Files:**
- Modify: `tui.go:79-492`
- Modify: `tui.go:591-630`
- Modify: `tui_state.go:64-113`
- Modify: `tui_state_test.go`
- Modify: `tui_test.go`

**Interfaces:**
- Consumes: `newScreenRenderer(io.Writer)`, `(*screenRenderer).Draw`, and `(*screenRenderer).Invalidate` from Task 1.
- Produces: `withBottomRow(content string, rows int, bottom string) string` and pure `renderHelp(sortMode string) string`.

- [ ] **Step 1: Write failing toast and help tests**

Append to `tui_state_test.go`:

```go
func TestWithBottomRowPadsAndPlacesBottomLine(t *testing.T) {
	got := withBottomRow("one\ntwo", 5, "toast")
	want := "one\ntwo\n\n\ntoast"
	if got != want {
		t.Fatalf("withBottomRow = %q, want %q", got, want)
	}
}

func TestWithBottomRowTruncatesContent(t *testing.T) {
	got := withBottomRow("one\ntwo\nthree", 2, "toast")
	want := "one\ntoast"
	if got != want {
		t.Fatalf("withBottomRow = %q, want %q", got, want)
	}
	if got := withBottomRow("one", 1, "toast"); got != "toast" {
		t.Fatalf("one-row screen = %q, want toast", got)
	}
}
```

Change `tui_test.go` imports to:

```go
import (
	"strings"
	"testing"
)
```

Append:

```go
func TestRenderHelpIsPureContent(t *testing.T) {
	out := renderHelp("status")
	for _, want := range []string{"claude-sessions", "NAVIGATION", "current sort: status", "press any key to return"} {
		if !strings.Contains(out, want) {
			t.Fatalf("help missing %q: %q", want, out)
		}
	}
	if strings.Contains(out, "\x1b[H") || strings.Contains(out, "\x1b[J") || strings.Contains(out, "\x1b[2J") {
		t.Fatalf("help contains terminal positioning or clear: %q", out)
	}
}
```

- [ ] **Step 2: Run focused tests and verify failure**

Run:

```sh
go test ./... -run '^(TestWithBottomRow|TestRenderHelpIsPureContent)' -count=1
```

Expected: build fails because `withBottomRow` does not exist and `renderHelp` does not return a value.

- [ ] **Step 3: Add bottom-row composition helper**

Add to `tui_state.go` after `cropTableFrame`:

```go
// withBottomRow pads or truncates content so bottom occupies the final terminal
// row. The screen renderer performs width clipping and final frame padding.
func withBottomRow(content string, rows int, bottom string) string {
	if rows <= 0 {
		return content
	}
	lines := strings.Split(content, "\n")
	bodyRows := rows - 1
	if len(lines) > bodyRows {
		lines = lines[:bodyRows]
	}
	for len(lines) < bodyRows {
		lines = append(lines, "")
	}
	lines = append(lines, bottom)
	return strings.Join(lines, "\n")
}
```

- [ ] **Step 4: Convert help to a pure content builder**

Replace `renderHelp` in `tui.go` with:

```go
// renderHelp builds help-screen content. RunTUI owns terminal positioning and
// sends this content through screenRenderer.
func renderHelp(sortMode string) string {
	var b strings.Builder
	fmt.Fprintln(&b, bold("claude-sessions  ·  help"))
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "  "+bold("NAVIGATION"))
	fmt.Fprintln(&b, "    ↑ / ↓        move selection")
	fmt.Fprintln(&b, "    mouse click  select row · double-click opens")
	fmt.Fprintln(&b, "    mouse wheel  scroll list or inspector")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "  "+bold("ACTIONS")+"  (on selected row)")
	fmt.Fprintln(&b, "    n            new tmux session (↑/↓ cwd · ←/→ command)")
	fmt.Fprintln(&b, "    k            kill the session (tmux-aware)")
	fmt.Fprintln(&b, "    a            attach (or migrate to tmux first)")
	fmt.Fprintln(&b, "    Enter / p    open full-screen inspector")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "  "+bold("INSPECTOR"))
	fmt.Fprintln(&b, "    Home / End   oldest output / resume live follow")
	fmt.Fprintln(&b, "    PgUp / PgDn  scroll inspector by page")
	fmt.Fprintln(&b, "    r            refresh now")
	fmt.Fprintln(&b, "    Esc / q / p  return from inspector")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "  "+bold("VIEW"))
	fmt.Fprintln(&b, "    m            cycle mode (full → intermediate → minimal)  ·  persisted")
	fmt.Fprintln(&b, "    s / S        cycle sort forward / back (dir → status → created → updated, +asc)")
	fmt.Fprintln(&b, "                 current sort: "+sortMode)
	fmt.Fprintln(&b, "    r            refresh now")
	fmt.Fprintln(&b, "    q / Ctrl-C   quit")
	fmt.Fprintln(&b, "    ?            this help")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "  "+bold("SUBCOMMANDS")+"  (from the shell)")
	fmt.Fprintln(&b, "    claude-sessions kill PID [-y]")
	fmt.Fprintln(&b, "    claude-sessions migrate PID [-y]")
	fmt.Fprintln(&b, "    claude-sessions new --cwd PATH [--name NAME]")
	fmt.Fprintln(&b, "    claude-sessions preview PID")
	fmt.Fprintln(&b, "    claude-sessions tmux-info PID")
	fmt.Fprintln(&b, "    claude-sessions attach PID")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, dim("press any key to return"))
	return b.String()
}
```

- [ ] **Step 5: Route list and inspector frames through one renderer**

In `RunTUI`, immediately after `state := newTUIState()`, add:

```go
	screen := newScreenRenderer(os.Stdout)
```

In the inspector branch of `render`, replace:

```go
			fmt.Print("\033[H\033[J" + buf.String())
```

with:

```go
			_ = screen.Draw(buf.String(), cols, rows)
```

In the session-list branch, replace:

```go
		if toastActive {
			out += fmt.Sprintf("\033[%d;1H%s", rows, clipLine(bold(toast), cols))
		}
		fmt.Print("\033[H\033[J" + out)
```

with:

```go
		if toastActive {
			out = withBottomRow(out, rows, bold(toast))
		}
		_ = screen.Draw(out, cols, rows)
```

Keep `fmt` imported because `RunTUI`, sort descriptions, and `renderHelp` still use it.

- [ ] **Step 6: Invalidate renderer at explicit ownership boundaries**

In `openInspector`, add `screen.Invalidate()` immediately before `render()`:

```go
		state.inspector = newInspectorViewState(target.id)
		state.inspectorTargetGone = false
		screen.Invalidate()
		render()
```

In `closeInspector`, add `screen.Invalidate()` immediately before `render()`:

```go
		state.inspectorTargetGone = false
		refresh(false)
		screen.Invalidate()
		render()
```

Invalidate before each action that may draw outside the renderer:

```go
			case "k", "K":
				screen.Invalidate()
				actKill(makeCtx())
				refresh(true)
				render()
			case "a", "A":
				screen.Invalidate()
				actAttach(makeCtx())
				refresh(true)
				render()
			case "n", "N":
				screen.Invalidate()
				ctx := makeCtx()
				actNew(ctx)
```

Do not change existing post-spawn selection logic after `actNew(ctx)`.

Replace the help key branch with:

```go
			case "?":
				screen.Invalidate()
				cols, rows, err := term.GetSize(fd)
				if err != nil {
					cols, rows = 0, 0
				}
				_ = screen.Draw(renderHelp(sortMode), cols, rows)
				readEventBlocking()
				screen.Invalidate()
				render()
```

- [ ] **Step 7: Run focused TUI tests**

Run:

```sh
gofmt -w tui.go tui_state.go tui_state_test.go tui_test.go
go test ./... -run '^(TestScreenRenderer|TestWithBottomRow|TestRenderHelpIsPureContent|TestCropTableFrame|TestRenderInspector|TestSessionScreen|TestInspectorBack)' -count=1
```

Expected: PASS.

- [ ] **Step 8: Run complete test suite and commit main integration**

Run:

```sh
go test ./...
git add tui.go tui_state.go tui_state_test.go tui_test.go
git commit -m "feat: render TUI frames without screen clears" -m "Co-Authored-By: Claude <noreply@anthropic.com>"
```

Expected: all tests pass and one integration commit is created.

---

### Task 3: Flicker-Free New-Session Picker

**Files:**
- Modify: `new_picker.go:1-89`
- Modify: `new_picker_test.go:38-47`

**Interfaces:**
- Consumes: `newScreenRenderer(io.Writer)` and `(*screenRenderer).Draw` from Task 1; existing `readEventBlocking()`; existing `term.GetSize` dependency.
- Produces: picker content with no cursor/display controls and picker redraws through a local renderer.

- [ ] **Step 1: Extend picker content test to reject terminal controls**

Append to `TestRenderNewPicker` in `new_picker_test.go`, after existing content assertions:

```go
	if strings.Contains(out, "\x1b[H") || strings.Contains(out, "\x1b[J") || strings.Contains(out, "\x1b[2J") {
		t.Fatalf("picker contains terminal positioning or clear: %q", out)
	}
```

- [ ] **Step 2: Run picker test and verify failure**

Run:

```sh
go test ./... -run '^TestRenderNewPicker$' -count=1
```

Expected: FAIL because current picker content begins with `\x1b[H\x1b[J`.

- [ ] **Step 3: Remove terminal controls from picker content**

In `renderNewPicker`, replace:

```go
	b.WriteString("\033[H\033[J\n " + bold(title) + "\n\n")
```

with:

```go
	b.WriteString("\n " + bold(title) + "\n\n")
```

- [ ] **Step 4: Draw picker through a local renderer**

Change `new_picker.go` imports to:

```go
import (
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"
)
```

In `pickNewSession`, add renderer and terminal fd setup immediately before the input loop:

```go
	renderer := newScreenRenderer(os.Stdout)
	fd := int(os.Stdin.Fd())
```

Replace the direct print at the start of the loop:

```go
		fmt.Print(renderNewPicker(title, lines, presets, state, note))
```

with:

```go
		cols, rows, err := term.GetSize(fd)
		if err != nil {
			cols, rows = 0, 0
		}
		_ = renderer.Draw(renderNewPicker(title, lines, presets, state, note), cols, rows)
```

`fmt` remains required by `renderNewPicker` formatting.

- [ ] **Step 5: Run picker and renderer tests**

Run:

```sh
gofmt -w new_picker.go new_picker_test.go
go test ./... -run '^(TestNewPicker|TestRenderNewPicker|TestScreenRenderer)' -count=1
```

Expected: PASS.

- [ ] **Step 6: Run full static verification**

Run:

```sh
go test ./...
go vet ./...
go build .
```

Expected: all commands exit 0.

- [ ] **Step 7: Inspect for forbidden redraw controls**

Run:

```sh
rg -n '\\033\[H\\033\[J|\\x1b\[H\\x1b\[J' --glob '*.go'
```

Expected: no session-list, inspector, help, or picker redraw path matches. Existing primary-screen interactive handoff `\033[2J\033[H` in `helpers.go` remains allowed because it occurs after leaving the alternate screen and is outside this feature's redraw scope.

- [ ] **Step 8: Commit picker integration**

Run:

```sh
git add new_picker.go new_picker_test.go
git commit -m "feat: diff new-session picker frames" -m "Co-Authored-By: Claude <noreply@anthropic.com>"
```

Expected: one picker integration commit.

---

### Task 4: Final Verification and Manual TUI Acceptance

**Files:**
- Verify only; modify code only if a failing check exposes a defect, then repeat the affected task's test-first cycle and commit that fix separately.

**Interfaces:**
- Consumes: complete implementation from Tasks 1-3.
- Produces: verification evidence for automated checks, direct-terminal behavior, and tmux behavior.

- [ ] **Step 1: Confirm worktree diff and commit structure**

Run:

```sh
git status --short
git log --oneline --decorate -5
```

Expected: clean worktree; plan commit and three implementation commits follow design commit `090bf0a`.

- [ ] **Step 2: Run automated acceptance gate**

Run:

```sh
go test ./... && go vet ./... && go build .
```

Expected: exit 0 with no test, vet, or build failures.

- [ ] **Step 3: Run direct-terminal smoke test**

Run:

```sh
go run .
```

Verify all of these before quitting:

1. Hold Up and Down; no blank frame appears.
2. Wait through automatic refresh; unchanged rows remain visually stable.
3. Press `p`, scroll inspector with Page Up/Page Down and mouse wheel, then return with Escape; no display clear appears.
4. Press `m`, `s`, and `S`; view/sort transitions repaint cleanly and toast stays on final terminal row.
5. Press `?`, then any key; help and list transitions contain no blank flash.
6. Press `n`, navigate rows and command presets, then cancel with `q`; picker changes only affected rows and list restores fully.
7. Resize terminal smaller and larger; stale rows and suffixes do not remain.
8. Enter a kill/attach confirmation and cancel; list fully restores after prompt.

Expected: every alternate-screen redraw is free of visible clear-screen flashes and stale content.

- [ ] **Step 4: Run tmux smoke test**

From a tmux client, run:

```sh
go run .
```

Repeat navigation, automatic refresh, inspector scrolling, help, picker, resize, and prompt-return checks from Step 3.

Expected: same stable rendering through tmux. Synchronized output may make multi-row patches atomic; line diff remains correct regardless.

- [ ] **Step 5: Review final diff**

Run:

```sh
git diff main...HEAD --stat
git diff main...HEAD --check
git status --short
```

Expected: only approved design, renderer, TUI integration, picker integration, and tests changed; no whitespace errors; clean worktree.
