# Kill-Dialog Preview Snapshot Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Show the last few lines of a session's tmux pane inside the `k` kill-confirmation box, fetched asynchronously so a slow remote host never freezes the TUI.

**Architecture:** A new `kill_preview.go` owns an async snapshot holder (`previewPane`) built on the repo's existing self-pipe wake pattern, plus the pure functions that turn a `PreviewResult` into box-ready lines. `confirm_overlay.go` grows an optional preview block; `confirmOverlay` keeps its current signature as a thin wrapper so the four call sites that don't opt in are untouched at the source level. `actKill` and `actKillRemote` are the only opt-in callers.

**Tech Stack:** Go 1.x, stdlib only (`os`, `sync`, `strings`, `errors`) plus the already-vendored `golang.org/x/term`. No new dependencies.

## Global Constraints

- **No new module dependencies.** Single-binary distribution is the point of this repo (CLAUDE.md); stdlib + `golang.org/x/term` + `golang.org/x/sys` only.
- **Single Go package `main`.** All files sit at the repo root; tests sit next to the code.
- **`confirmOverlay(question string, wakes []wakeFD) bool` keeps its exact signature.** Four call sites (actions.go:202, actions.go:237, remote_actions.go:291, remote_actions.go:357) must compile and behave identically without edits.
- **A nil preview must render byte-for-byte what renders today.** This is the regression guard for those four callers.
- **Preview limits are `PreviewLimits{MaxLines: 60, MaxBytes: 32 << 10}`.** Note the field names are `MaxLines`/`MaxBytes` (preview.go:31-34) — the design doc wrote `Lines`/`Bytes`, which is wrong.
- **Every emitted preview line ends with `ansiReset`.** `clipLine` (render.go:1883) adds none, so an unterminated SGR sequence from `capture-pane` would bleed into the border.
- **Preview content never widens the box.** Width is driven by the question, the hint, and a 72-column floor only.
- **Verify with `go test ./...` and `go vet ./...`** after every task.

---

## File Structure

| File | Responsibility |
| --- | --- |
| `kill_preview.go` (new) | `overlayPreview` snapshot type, the pure line-preparation functions (`trimTrailingBlank`, `previewStatusLine`, `previewBlock`), the async `previewPane` holder, and the two `start*KillPreview` constructors. Knows nothing about box drawing. |
| `kill_preview_test.go` (new) | Tests for the above. |
| `confirm_overlay.go` (modify) | `renderConfirmOverlay` takes a `*overlayPreview` and renders the block; new `confirmOverlayPreview` entry point; `confirmOverlay` becomes a wrapper. Knows nothing about tmux, HTTP, or sessions. |
| `confirm_overlay_test.go` (modify) | Existing calls updated for the new parameter; new sizing/gating cases. |
| `tui_events.go` (modify) | One new `wakePreview` wake kind. |
| `actions.go` (modify) | `actKill` opts in. |
| `remote_actions.go` (modify) | `actKillRemote` opts in. |

The boundary that matters: `kill_preview.go` produces `[]string`, `confirm_overlay.go` consumes `[]string`. Neither imports the other's concerns, and every function in `kill_preview.go` except the goroutine is pure and directly testable.

---

## Task 1: Preview snapshot type and pure line preparation

**Files:**
- Create: `kill_preview.go`
- Test: `kill_preview_test.go`

**Interfaces:**
- Consumes: `dim` (render.go:50), `visualLen` (render.go:1864), `clipLine` (render.go:1883), `ansiReset` (render.go), `errSessionEnded` (preview.go:20-23).
- Produces:
  - `type overlayPreview struct { Title, Source string; Lines []string; Err error; Loaded bool }`
  - `func trimTrailingBlank(lines []string) []string`
  - `func previewStatusLine(prev overlayPreview) string` — returns `""` when real content should render instead
  - `func previewBlock(prev *overlayPreview, innerWidth, contentRows int) []string`

`previewBlock` returns the *inner* lines of the block — title, divider, up to `contentRows` content rows, divider — each already clipped to `innerWidth` and reset-terminated. The caller pads and borders them. It returns `nil` when `prev == nil`, `innerWidth < 1`, or `contentRows < 1`.

- [ ] **Step 1: Write the failing tests**

Create `kill_preview_test.go`:

```go
package main

import (
	"errors"
	"strings"
	"testing"
)

func TestTrimTrailingBlankDropsPaddingRows(t *testing.T) {
	got := trimTrailingBlank([]string{"a", "", "b", "", "   ", ""})
	want := []string{"a", "", "b"}
	if len(got) != len(want) {
		t.Fatalf("got %d lines %q, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestTrimTrailingBlankAllBlankReturnsEmpty(t *testing.T) {
	if got := trimTrailingBlank([]string{"", "  ", ""}); len(got) != 0 {
		t.Fatalf("got %q, want empty", got)
	}
}

func TestPreviewStatusLineStates(t *testing.T) {
	cases := []struct {
		name string
		prev overlayPreview
		want string
	}{
		{"loading", overlayPreview{Loaded: false}, "loading preview…"},
		{"ended", overlayPreview{Loaded: true, Err: errSessionEnded}, "session already gone"},
		{"failed", overlayPreview{Loaded: true, Err: errors.New("timeout")}, "preview unavailable: timeout"},
		{"empty", overlayPreview{Loaded: true}, "(pane empty)"},
		{"content", overlayPreview{Loaded: true, Lines: []string{"x"}}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := previewStatusLine(tc.prev)
			if !strings.Contains(got, tc.want) {
				t.Fatalf("previewStatusLine = %q, want to contain %q", got, tc.want)
			}
			if tc.want == "" && got != "" {
				t.Fatalf("previewStatusLine = %q, want empty", got)
			}
		})
	}
}

func TestPreviewBlockNilOrNoRoomReturnsNil(t *testing.T) {
	prev := &overlayPreview{Loaded: true, Lines: []string{"x"}}
	if got := previewBlock(nil, 40, 4); got != nil {
		t.Fatalf("nil preview returned %q", got)
	}
	if got := previewBlock(prev, 40, 0); got != nil {
		t.Fatalf("contentRows=0 returned %q", got)
	}
	if got := previewBlock(prev, 0, 4); got != nil {
		t.Fatalf("innerWidth=0 returned %q", got)
	}
}

func TestPreviewBlockShapeAndTailSelection(t *testing.T) {
	prev := &overlayPreview{
		Title:  "repo:branch · pid 42",
		Source: "tmux",
		Loaded: true,
		Lines:  []string{"one", "two", "three", "four", "five", "", ""},
	}
	got := previewBlock(prev, 40, 3)
	// title + divider + 3 content + divider
	if len(got) != 6 {
		t.Fatalf("got %d lines, want 6:\n%s", len(got), strings.Join(got, "\n"))
	}
	if !strings.Contains(got[0], "repo:branch · pid 42") || !strings.Contains(got[0], "tmux") {
		t.Fatalf("title row = %q", got[0])
	}
	// Trailing blanks trimmed, then the LAST 3 real lines kept.
	for i, want := range []string{"three", "four", "five"} {
		if !strings.Contains(got[2+i], want) {
			t.Fatalf("content row %d = %q, want %q", i, got[2+i], want)
		}
	}
	if !strings.HasPrefix(got[1], "─") || !strings.HasPrefix(got[5], "─") {
		t.Fatalf("dividers missing: %q / %q", got[1], got[5])
	}
}

func TestPreviewBlockFewerLinesThanRoomDoesNotPad(t *testing.T) {
	prev := &overlayPreview{Loaded: true, Title: "t", Lines: []string{"only"}}
	got := previewBlock(prev, 40, 8)
	if len(got) != 4 { // title + divider + 1 content + divider
		t.Fatalf("got %d lines, want 4:\n%s", len(got), strings.Join(got, "\n"))
	}
}

func TestPreviewBlockEveryLineEndsReset(t *testing.T) {
	prev := &overlayPreview{
		Loaded: true,
		Title:  "t",
		Source: "tmux",
		Lines:  []string{"\033[31mred and never reset"},
	}
	for i, l := range previewBlock(prev, 40, 2) {
		if !strings.HasSuffix(l, ansiReset) {
			t.Fatalf("line %d does not end with reset: %q", i, l)
		}
	}
}

func TestPreviewBlockClipsWideLinesToInnerWidth(t *testing.T) {
	prev := &overlayPreview{Loaded: true, Title: "t", Lines: []string{strings.Repeat("x", 200)}}
	for i, l := range previewBlock(prev, 20, 2) {
		if visibleWidth(l) > 20 {
			t.Fatalf("line %d is %d cols, want <=20: %q", i, visibleWidth(l), l)
		}
	}
}

func TestPreviewBlockDropsSourceWhenTitleFillsWidth(t *testing.T) {
	prev := &overlayPreview{Loaded: true, Title: strings.Repeat("t", 30), Source: "transcript"}
	got := previewBlock(prev, 20, 2)
	if strings.Contains(got[0], "transcript") {
		t.Fatalf("source should be dropped when it cannot fit: %q", got[0])
	}
}

func TestPreviewBlockStatusRendersInsteadOfContent(t *testing.T) {
	prev := &overlayPreview{Title: "t"} // not loaded
	got := previewBlock(prev, 40, 5)
	if len(got) != 4 { // title + divider + 1 status row + divider
		t.Fatalf("got %d lines, want 4:\n%s", len(got), strings.Join(got, "\n"))
	}
	if !strings.Contains(got[2], "loading preview…") {
		t.Fatalf("status row = %q", got[2])
	}
}
```

`visibleWidth` already exists at `render_inspector_test.go:13` — same package, reuse it directly.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./... -run 'TestTrimTrailingBlank|TestPreviewStatusLine|TestPreviewBlock' -v`
Expected: FAIL — `undefined: trimTrailingBlank`, `undefined: overlayPreview`, `undefined: previewStatusLine`, `undefined: previewBlock`.

- [ ] **Step 3: Write the implementation**

Create `kill_preview.go`:

```go
package main

import (
	"errors"
	"strings"
)

// overlayPreview is an immutable snapshot of a session's recent output, handed
// to renderConfirmOverlay for display inside the kill confirmation box. It is
// deliberately free of tmux/HTTP concerns: previewPane fills it in, the
// renderer only formats it.
type overlayPreview struct {
	Title  string   // "repo:branch · pid 48221" (host-qualified when remote)
	Source string   // "tmux" | "transcript"; empty while loading
	Lines  []string // sanitized pane lines, oldest first, unclipped
	Err    error    // fetch failure; Lines is nil
	Loaded bool     // false while the fetch is still in flight
}

// trimTrailingBlank drops blank lines from the end of a pane capture. tmux
// capture-pane pads its output to the full pane height, so without this the
// preview block renders mostly empty rows.
func trimTrailingBlank(lines []string) []string {
	end := len(lines)
	for end > 0 && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	return lines[:end]
}

// previewStatusLine returns the dimmed placeholder row to show in place of
// content, or "" when the snapshot has real lines to render.
func previewStatusLine(prev overlayPreview) string {
	switch {
	case !prev.Loaded:
		return dim("loading preview…")
	case errors.Is(prev.Err, errSessionEnded):
		return dim("session already gone")
	case prev.Err != nil:
		return dim("preview unavailable: " + prev.Err.Error())
	case len(trimTrailingBlank(prev.Lines)) == 0:
		return dim("(pane empty)")
	default:
		return ""
	}
}

// previewBlock builds the inner lines of the preview block — title, divider,
// up to contentRows content rows, divider — clipped to innerWidth and
// reset-terminated so an unterminated SGR sequence cannot bleed into the box
// border. The caller supplies the padding and the border itself. Returns nil
// when there is no preview or no room for one.
func previewBlock(prev *overlayPreview, innerWidth, contentRows int) []string {
	if prev == nil || innerWidth < 1 || contentRows < 1 {
		return nil
	}

	body := []string{previewStatusLine(*prev)}
	if body[0] == "" {
		body = trimTrailingBlank(prev.Lines)
		if len(body) > contentRows {
			body = body[len(body)-contentRows:]
		}
	}

	divider := strings.Repeat(confirmBoxH, innerWidth)
	out := make([]string, 0, len(body)+3)
	out = append(out, previewTitleRow(*prev, innerWidth), divider)
	out = append(out, body...)
	out = append(out, divider)

	for i, l := range out {
		out[i] = clipLine(l, innerWidth) + ansiReset
	}
	return out
}

// previewTitleRow renders the identity row: title on the left, dimmed source
// marker flush right. The source is dropped rather than wrapped when the two
// cannot share the row.
func previewTitleRow(prev overlayPreview, innerWidth int) string {
	gap := innerWidth - visualLen(prev.Title) - visualLen(prev.Source)
	if prev.Source == "" || gap < 1 {
		return prev.Title
	}
	return prev.Title + strings.Repeat(" ", gap) + dim(prev.Source)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./... -run 'TestTrimTrailingBlank|TestPreviewStatusLine|TestPreviewBlock' -v`
Expected: PASS, all cases.

Then run the full suite to confirm nothing else moved: `go test ./... && go vet ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add kill_preview.go kill_preview_test.go
git commit -m "feat: preview snapshot type and line preparation for the kill dialog"
```

---

## Task 2: Render the preview block inside the confirm box

**Files:**
- Modify: `confirm_overlay.go:45-107` (`renderConfirmOverlay`), `confirm_overlay.go:125` (its one internal caller)
- Modify: `confirm_overlay_test.go` (6 existing `renderConfirmOverlay` calls at lines 39, 48, 57, 76, 84)
- Test: `confirm_overlay_test.go`

**Interfaces:**
- Consumes: `previewBlock`, `overlayPreview` (Task 1).
- Produces: `func renderConfirmOverlay(question string, prev *overlayPreview, cols, rows int) string`.

Sizing rules, exactly:

```
contentRows = 0                        when prev == nil or rows <= 0
contentRows = clamp(rows-12, 0, 12)    otherwise
hasPreview  = prev != nil && contentRows > 0
innerWidth  = max(questionWidth, hintWidth)
innerWidth  = max(innerWidth, 72)      only when hasPreview
innerWidth  = min(innerWidth, cols-4)  when cols > 0   (existing behaviour)
```

The 72-column floor is applied **before** the `cols-4` cap so a narrow terminal still clips correctly, and **only** when `hasPreview` — otherwise the four non-opt-in callers and the short-terminal fallback would silently get wider boxes. `rows <= 0` means an unknown terminal size, which is treated as "too short" so the fallback is the conservative one.

- [ ] **Step 1: Update the existing test calls so the package compiles**

In `confirm_overlay_test.go`, add `nil` as the second argument to all five existing call sites:

```go
out := renderConfirmOverlay("kill PID 1234?", nil, 80, 24)
out := renderConfirmOverlay("line one\nline two", nil, 80, 24)
out := renderConfirmOverlay("kill it?", nil, 0, 0)
out := renderConfirmOverlay("a very long question that will not fit", nil, size.cols, size.rows)
out := renderConfirmOverlay("hi", nil, 40, 10)
```

These are the byte-for-byte regression guard — their assertions must not be relaxed.

- [ ] **Step 2: Write the failing tests**

Append to `confirm_overlay_test.go`:

```go
func TestRenderConfirmOverlayNilPreviewKeepsNarrowBox(t *testing.T) {
	out := renderConfirmOverlay("hi", nil, 120, 40)
	for _, ln := range strings.Split(out, "\n") {
		if strings.Contains(ln, confirmBoxTL) && visibleWidth(strings.TrimLeft(ln, " ")) >= 72 {
			t.Fatalf("nil preview widened the box to %d cols: %q", visibleWidth(ln), ln)
		}
	}
}

func TestRenderConfirmOverlayShowsPreviewRows(t *testing.T) {
	prev := &overlayPreview{
		Title:  "repo · pid 42",
		Source: "tmux",
		Loaded: true,
		Lines:  []string{"alpha", "bravo", "charlie"},
	}
	out := renderConfirmOverlay("kill PID 42?", prev, 120, 40)
	for _, want := range []string{"repo · pid 42", "alpha", "bravo", "charlie", "kill PID 42?", "[y] yes"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

func TestRenderConfirmOverlayPreviewAppliesWidthFloor(t *testing.T) {
	prev := &overlayPreview{Title: "t", Loaded: true, Lines: []string{"x"}}
	out := renderConfirmOverlay("hi", prev, 120, 40)
	var top string
	for _, ln := range strings.Split(out, "\n") {
		if strings.Contains(ln, confirmBoxTL) {
			top = strings.TrimLeft(ln, " ")
			break
		}
	}
	if got := visibleWidth(top); got != 76 { // 72 inner + 4 border/padding
		t.Fatalf("box width = %d, want 76: %q", got, top)
	}
}

func TestRenderConfirmOverlayShortTerminalDropsPreview(t *testing.T) {
	prev := &overlayPreview{Title: "should-not-appear", Loaded: true, Lines: []string{"nope"}}
	out := renderConfirmOverlay("hi", prev, 120, 10)
	if strings.Contains(out, "should-not-appear") || strings.Contains(out, "nope") {
		t.Fatalf("preview rendered on a 10-row terminal:\n%s", out)
	}
	if plain := renderConfirmOverlay("hi", nil, 120, 10); out != plain {
		t.Fatalf("short-terminal output differs from the nil-preview output:\n%s\n---\n%s", out, plain)
	}
}

func TestRenderConfirmOverlayUnknownSizeDropsPreview(t *testing.T) {
	prev := &overlayPreview{Title: "should-not-appear", Loaded: true, Lines: []string{"nope"}}
	out := renderConfirmOverlay("hi", prev, 0, 0)
	if strings.Contains(out, "should-not-appear") {
		t.Fatalf("preview rendered at unknown terminal size:\n%s", out)
	}
}

func TestRenderConfirmOverlayPreviewNeverWidensBox(t *testing.T) {
	prev := &overlayPreview{Title: "t", Loaded: true, Lines: []string{strings.Repeat("x", 400)}}
	out := renderConfirmOverlay("hi", prev, 100, 40)
	for _, ln := range strings.Split(out, "\n") {
		if visibleWidth(ln) > 100 {
			t.Fatalf("line exceeds terminal width: %d cols", visibleWidth(ln))
		}
	}
}

func TestRenderConfirmOverlayPreviewCapsAtTwelveRows(t *testing.T) {
	lines := make([]string, 40)
	for i := range lines {
		lines[i] = fmt.Sprintf("line-%02d", i)
	}
	prev := &overlayPreview{Title: "t", Loaded: true, Lines: lines}
	out := renderConfirmOverlay("hi", prev, 120, 200)
	if strings.Contains(out, "line-27") {
		t.Fatalf("more than 12 content rows rendered:\n%s", out)
	}
	if !strings.Contains(out, "line-39") || !strings.Contains(out, "line-28") {
		t.Fatalf("last 12 rows not rendered:\n%s", out)
	}
}
```

Add `"fmt"` to the test file's imports if it is not already there.

- [ ] **Step 3: Write the implementation**

Replace `renderConfirmOverlay`'s signature and width/box assembly in `confirm_overlay.go`. Update the doc comment, then:

```go
func renderConfirmOverlay(question string, prev *overlayPreview, cols, rows int) string {
	contentRows := 0
	if prev != nil && rows > 0 {
		contentRows = rows - 12 // box chrome + question + blank + hint
		if contentRows > 12 {
			contentRows = 12
		}
		if contentRows < 0 {
			contentRows = 0
		}
	}
	hasPreview := prev != nil && contentRows > 0

	qLines := strings.Split(question, "\n")
	innerWidth := visualLen(confirmHint)
	for _, l := range qLines {
		if w := visualLen(l); w > innerWidth {
			innerWidth = w
		}
	}
	// A preview block needs room to be legible; the floor applies only when one
	// actually renders, so the callers that pass nil keep today's narrow box.
	if hasPreview && innerWidth < previewBoxMinInner {
		innerWidth = previewBoxMinInner
	}
	if cols > 0 {
		max := cols - 4 // border + 1 space of padding on each side
		if max < 1 {
			max = 1
		}
		if innerWidth > max {
			innerWidth = max
		}
	}

	pad := func(s string) string {
		s = clipLine(s, innerWidth)
		return confirmBoxV + " " + s + strings.Repeat(" ", innerWidth-visualLen(s)) + " " + confirmBoxV
	}

	block := previewBlock(prev, innerWidth, contentRows)

	box := make([]string, 0, len(qLines)+len(block)+4)
	box = append(box, confirmBoxTL+strings.Repeat(confirmBoxH, innerWidth+2)+confirmBoxTR)
	for _, l := range block {
		box = append(box, pad(l))
	}
	for _, l := range qLines {
		box = append(box, pad(l))
	}
	box = append(box, pad(""))
	box = append(box, pad(dim(confirmHint)))
	box = append(box, confirmBoxBL+strings.Repeat(confirmBoxH, innerWidth+2)+confirmBoxBR)

	// ... rest of the function (centering) unchanged
```

Note `hasPreview` gates only the width floor — `previewBlock` independently returns nil for the same conditions, so the block is empty whenever the floor does not apply.

Add the constant next to the box characters in `confirm_overlay.go`:

```go
// previewBoxMinInner is the inner width the box widens to when it carries a
// preview block, so pane output is legible rather than shredded by clipping.
const previewBoxMinInner = 72
```

Update the internal caller at `confirm_overlay.go:125`:

```go
		_ = renderer.Draw(renderConfirmOverlay(question, nil, cols, rows), cols, rows)
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./... -run TestRenderConfirmOverlay -v`
Expected: PASS, including the five pre-existing cases.

Run: `go test ./... && go vet ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add confirm_overlay.go confirm_overlay_test.go
git commit -m "feat: render an optional preview block in the confirm overlay"
```

---

## Task 3: Async preview pane with a wake pipe

**Files:**
- Modify: `kill_preview.go`
- Modify: `tui_events.go:326-333` (wake kinds)
- Test: `kill_preview_test.go`

**Interfaces:**
- Consumes: `wakeFD` / `wakeKind` (tui_events.go:326-342), `PreviewResult` and `PreviewLimits` (preview.go:31-50), `LoadPreview` (preview.go:62), `fetchRemotePreview` (remote_actions.go:136), `Session.DisplayName` (session.go:102), `overlayPreview` (Task 1).
- Produces:
  - `type previewFetch func() (PreviewResult, error)`
  - `func startPreviewPane(title string, fetch previewFetch) *previewPane`
  - `func startLocalKillPreview(s Session) *previewPane`
  - `func startRemoteKillPreview(s Session) *previewPane`
  - `func (p *previewPane) snapshot() overlayPreview`
  - `func (p *previewPane) wake() wakeFD`
  - `func (p *previewPane) close()`
  - `var killPreviewLimits = PreviewLimits{MaxLines: 60, MaxBytes: 32 << 10}`
  - `wakePreview` wake kind

All four methods are nil-safe so callers never branch on a nil pane.

- [ ] **Step 1: Add the wake kind**

In `tui_events.go`, extend the const block (currently `wakeNone`, `wakeRemote`, `wakeInspector`, `wakeResize`) with one entry after `wakeResize`:

```go
	wakePreview
```

It participates in the same `1 << iota` sequence, so it needs no explicit value. Update the block's doc comment to mention that a preview fetch completing is now a wake source.

- [ ] **Step 2: Write the failing tests**

Append to `kill_preview_test.go`:

```go
func TestPreviewPaneSnapshotBeforeFetchIsUnloaded(t *testing.T) {
	release := make(chan struct{})
	p := startPreviewPane("t", func() (PreviewResult, error) {
		<-release
		return PreviewResult{Source: "tmux", Content: "done"}, nil
	})
	defer func() { close(release); p.close() }()

	snap := p.snapshot()
	if snap.Loaded {
		t.Fatal("snapshot reported Loaded before the fetch returned")
	}
	if snap.Title != "t" {
		t.Fatalf("Title = %q, want %q", snap.Title, "t")
	}
}

func TestPreviewPaneSnapshotAfterFetchCarriesContent(t *testing.T) {
	p := startPreviewPane("t", func() (PreviewResult, error) {
		return PreviewResult{Source: "tmux", Content: "alpha\nbravo"}, nil
	})
	defer p.close()

	snap := waitLoaded(t, p)
	if snap.Source != "tmux" {
		t.Fatalf("Source = %q, want tmux", snap.Source)
	}
	if len(snap.Lines) != 2 || snap.Lines[0] != "alpha" || snap.Lines[1] != "bravo" {
		t.Fatalf("Lines = %q", snap.Lines)
	}
}

func TestPreviewPaneFetchErrorIsCaptured(t *testing.T) {
	want := errors.New("boom")
	p := startPreviewPane("t", func() (PreviewResult, error) { return PreviewResult{}, want })
	defer p.close()

	snap := waitLoaded(t, p)
	if !errors.Is(snap.Err, want) {
		t.Fatalf("Err = %v, want %v", snap.Err, want)
	}
}

func TestPreviewPaneWakeFiresOnCompletion(t *testing.T) {
	p := startPreviewPane("t", func() (PreviewResult, error) {
		return PreviewResult{Content: "x"}, nil
	})
	defer p.close()

	w := p.wake()
	if w.fd < 0 || w.kind != wakePreview {
		t.Fatalf("wake() = %+v, want a live fd with wakePreview", w)
	}
	// The pipe must become readable once the fetch lands.
	deadline := time.Now().Add(2 * time.Second)
	for {
		var set unix.FdSet
		set.Zero()
		set.Set(w.fd)
		tv := unix.Timeval{Usec: 50000}
		n, err := unix.Select(w.fd+1, &set, nil, nil, &tv)
		if err == nil && n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("wake pipe never became readable after the fetch completed")
		}
	}
}

func TestPreviewPaneCloseBeforeFetchDoesNotPanic(t *testing.T) {
	release := make(chan struct{})
	finished := make(chan struct{})
	p := startPreviewPane("t", func() (PreviewResult, error) {
		<-release
		defer close(finished)
		return PreviewResult{Content: "late"}, nil
	})
	p.close()
	close(release)

	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("fetch goroutine leaked after close")
	}
}

func TestPreviewPaneCloseIsIdempotent(t *testing.T) {
	p := startPreviewPane("t", func() (PreviewResult, error) { return PreviewResult{}, nil })
	p.close()
	p.close()
	if got := p.wake(); got.fd >= 0 {
		t.Fatalf("wake() after close = %+v, want a negative fd", got)
	}
}

func TestPreviewPaneNilIsSafe(t *testing.T) {
	var p *previewPane
	if snap := p.snapshot(); snap.Loaded {
		t.Fatal("nil pane snapshot should be zero-valued")
	}
	if got := p.wake(); got.fd >= 0 {
		t.Fatalf("nil pane wake = %+v, want a negative fd", got)
	}
	p.close() // must not panic
}

// waitLoaded polls until the pane's fetch has landed, failing the test on timeout.
func waitLoaded(t *testing.T, p *previewPane) overlayPreview {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if snap := p.snapshot(); snap.Loaded {
			return snap
		}
		if time.Now().After(deadline) {
			t.Fatal("fetch never completed")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
```

Add `"time"` and `"golang.org/x/sys/unix"` to the test file's imports.

- [ ] **Step 3: Run the tests to verify they fail**

Run: `go test ./... -run TestPreviewPane -v`
Expected: FAIL — `undefined: startPreviewPane`, `undefined: previewPane`, `undefined: wakePreview`.

- [ ] **Step 4: Write the implementation**

Append to `kill_preview.go` (and add `"fmt"`, `"os"`, `"sync"` to its imports):

```go
// killPreviewLimits bounds the kill-dialog fetch. At most 12 lines are ever
// rendered; 60 leaves headroom for trailing-blank trimming while keeping the
// remote response body far smaller than DefaultPreviewLimits' 2000 lines.
var killPreviewLimits = PreviewLimits{MaxLines: 60, MaxBytes: 32 << 10}

// previewFetch is the injectable seam around the actual preview lookup, so
// previewPane's lifecycle can be tested without tmux or a remote host.
type previewFetch func() (PreviewResult, error)

// previewPane holds an in-flight preview fetch and the self-pipe the modal
// loop selects on, following RemoteHub's wake-pipe pattern (remote.go:121).
// Every method is nil-safe so callers never have to branch on a nil pane.
type previewPane struct {
	mu     sync.Mutex
	snap   overlayPreview
	wakeR  *os.File
	wakeW  *os.File
	closed bool
}

// startPreviewPane kicks off fetch in the background and returns immediately.
// If the pipe cannot be created the pane still works — it simply never wakes
// the modal, so the placeholder stays until the next keypress redraws.
func startPreviewPane(title string, fetch previewFetch) *previewPane {
	p := &previewPane{snap: overlayPreview{Title: title}}
	if r, w, err := os.Pipe(); err == nil {
		p.wakeR, p.wakeW = r, w
	}
	go func() {
		res, err := fetch()
		p.mu.Lock()
		p.snap.Loaded = true
		if err != nil {
			p.snap.Err = err
		} else {
			p.snap.Source = res.Source
			p.snap.Lines = strings.Split(res.Content, "\n")
		}
		// The write happens under the same lock that close() takes, so the fd
		// can never be closed and reused between the check and the write.
		if !p.closed && p.wakeW != nil {
			_, _ = p.wakeW.Write([]byte{1})
		}
		p.mu.Unlock()
	}()
	return p
}

// snapshot returns the current state by value; the caller may hold it across
// a render without racing the fetch goroutine.
func (p *previewPane) snapshot() overlayPreview {
	if p == nil {
		return overlayPreview{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.snap
}

// wake exposes the read end for unix.Select. A negative fd means "no source"
// and is skipped by pollEvents.
func (p *previewPane) wake() wakeFD {
	if p == nil {
		return wakeFD{fd: -1, kind: wakePreview}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.wakeR == nil {
		return wakeFD{fd: -1, kind: wakePreview}
	}
	return wakeFD{fd: int(p.wakeR.Fd()), kind: wakePreview}
}

// close releases the pipe. Idempotent, and safe to call while the fetch is
// still running — the goroutine's write is guarded by the same mutex.
func (p *previewPane) close() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	p.closed = true
	if p.wakeR != nil {
		p.wakeR.Close()
	}
	if p.wakeW != nil {
		p.wakeW.Close()
	}
}

// killPreviewTitle is the identity row: the session's display label plus the
// pid, host-qualified for remote rows.
func killPreviewTitle(s Session) string {
	label, _ := s.DisplayName()
	if s.Host != "" {
		return fmt.Sprintf("%s · %s:%d", label, s.Host, s.PID)
	}
	return fmt.Sprintf("%s · pid %d", label, s.PID)
}

// startLocalKillPreview fetches the local pane snapshot for a kill dialog.
func startLocalKillPreview(s Session) *previewPane {
	return startPreviewPane(killPreviewTitle(s), func() (PreviewResult, error) {
		return LoadPreview(s.PID, killPreviewLimits)
	})
}

// startRemoteKillPreview fetches the snapshot over HTTP. The 5s client timeout
// in fetchRemotePreview bounds how long the goroutine can outlive the dialog.
func startRemoteKillPreview(s Session) *previewPane {
	host, pid := s.Host, s.PID
	return startPreviewPane(killPreviewTitle(s), func() (PreviewResult, error) {
		return fetchRemotePreview(host, pid, killPreviewLimits)
	})
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./... -run TestPreviewPane -v`
Expected: PASS.

Run: `go test -race ./... && go vet ./...`
Expected: PASS with no race reports. The `-race` run matters here — this is the only concurrent code in the change.

- [ ] **Step 6: Commit**

```bash
git add kill_preview.go kill_preview_test.go tui_events.go
git commit -m "feat: async preview pane with a self-pipe wake for the kill dialog"
```

---

## Task 4: Wire the pane into the overlay loop

**Files:**
- Modify: `confirm_overlay.go:109-134` (`confirmOverlay`)
- Test: `confirm_overlay_test.go`

**Interfaces:**
- Consumes: `previewPane`, `snapshot`, `wake` (Task 3); `renderConfirmOverlay` (Task 2).
- Produces: `func confirmOverlayPreview(question string, p *previewPane, wakes []wakeFD) bool`. `confirmOverlay` keeps its signature and delegates.

- [ ] **Step 1: Write the failing test**

The loop itself needs a terminal, so the testable contract is the one that has actually bitten this repo before: the shared `c.modalWakes` slice must never be mutated by appending the pane's wake. Append to `confirm_overlay_test.go`:

```go
func TestModalWakesAppendDoesNotMutateCallerSlice(t *testing.T) {
	// Capacity headroom is what makes a naive append() clobber the caller's
	// backing array; modalWakes is built once in RunTUI and shared by every
	// modal, so this must copy.
	base := make([]wakeFD, 1, 4)
	base[0] = wakeFD{fd: 7, kind: wakeResize}

	p := startPreviewPane("t", func() (PreviewResult, error) { return PreviewResult{}, nil })
	defer p.close()

	got := modalWakesWith(base, p)
	if len(base) != 1 || base[0].kind != wakeResize {
		t.Fatalf("caller slice mutated: %+v", base)
	}
	if len(got) != 2 || got[1].kind != wakePreview {
		t.Fatalf("modalWakesWith = %+v, want base plus the preview wake", got)
	}
	if nilCase := modalWakesWith(base, nil); len(nilCase) != 1 {
		t.Fatalf("nil pane should add no wake, got %+v", nilCase)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./... -run TestModalWakesAppend -v`
Expected: FAIL — `undefined: modalWakesWith`.

- [ ] **Step 3: Write the implementation**

In `confirm_overlay.go`, replace `confirmOverlay` with:

```go
// modalWakesWith returns wakes plus the pane's wake source, copying rather
// than appending in place: modalWakes is built once in RunTUI (tui.go:333) and
// shared by every modal, so an in-place append would corrupt it for the next
// dialog.
func modalWakesWith(wakes []wakeFD, p *previewPane) []wakeFD {
	w := p.wake()
	if w.fd < 0 {
		return wakes
	}
	out := make([]wakeFD, len(wakes), len(wakes)+1)
	copy(out, wakes)
	return append(out, w)
}

// confirmOverlay drives a blocking y/n dialog rendered as a centered overlay
// box, mirroring pickNewSession's read/handle loop shape. Must be called in
// raw mode; it never leaves raw or the alt-screen, so the caller's next
// render() paints over it. wakes lets the caller pass modal wake sources
// (e.g. resize) so the box stays correctly positioned across a live resize.
func confirmOverlay(question string, wakes []wakeFD) bool {
	return confirmOverlayPreview(question, nil, wakes)
}

// confirmOverlayPreview is confirmOverlay with an optional preview block. The
// pane fetches in the background and wakes the loop when it lands, so a slow
// or unreachable remote host never delays the dialog appearing. A nil pane
// renders exactly what confirmOverlay renders.
func confirmOverlayPreview(question string, p *previewPane, wakes []wakeFD) bool {
	state := confirmState{}
	renderer := newScreenRenderer(os.Stdout)
	decoder := newInputDecoder()
	fd := int(os.Stdin.Fd())
	modalWakes := modalWakesWith(wakes, p)

	for {
		cols, rows, err := term.GetSize(fd)
		if err != nil {
			cols, rows = 0, 0
		}
		var prev *overlayPreview
		if p != nil {
			snap := p.snapshot()
			prev = &snap
		}
		_ = renderer.Draw(renderConfirmOverlay(question, prev, cols, rows), cols, rows)
		keys, _ := readModalEvents(decoder, modalWakes)
		for _, key := range keys {
			confirmed, done := state.handle(key)
			if done {
				return confirmed
			}
		}
	}
}
```

`readModalEvents` returns on any non-`wakeNone` wake, so a `wakePreview` fire simply re-enters the loop and redraws with the new snapshot. `screenRenderer.Draw` full-redraws when the row count changes (screen_renderer.go:32), so the box growing from placeholder to content leaves no ghosting.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./... -run 'TestModalWakesAppend|TestRenderConfirmOverlay' -v`
Expected: PASS.

Run: `go test -race ./... && go vet ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add confirm_overlay.go confirm_overlay_test.go
git commit -m "feat: confirmOverlayPreview drives the box with a live preview pane"
```

---

## Task 5: Opt the two kill paths in

**Files:**
- Modify: `actions.go:184` (`actKill`)
- Modify: `remote_actions.go:266` (`actKillRemote`)
- Test: `actions_test.go` (or `kill_preview_test.go` if `actKill` has no existing coverage to extend)

**Interfaces:**
- Consumes: `startLocalKillPreview`, `startRemoteKillPreview`, `previewPane.close` (Task 3); `confirmOverlayPreview` (Task 4).
- Produces: no new exported surface.

The pane is closed explicitly right after the confirm returns rather than via `defer`, so the pipe is released before the kill runs and before the second worktree confirm opens — that dialog deliberately gets no preview, since by then the session is dead.

- [ ] **Step 1: Change the local call site**

In `actions.go`, replace:

```go
	if !confirmOverlay(question, c.modalWakes) {
		return
	}
```

with:

```go
	pane := startLocalKillPreview(*s)
	confirmed := confirmOverlayPreview(question, pane, c.modalWakes)
	pane.close()
	if !confirmed {
		return
	}
```

Leave the rest of `actKill` — the worktree resolution, `prepareLineOutput`, `KillSession`, `enterRaw`, and the second `confirmOverlay(worktreeRemovalQuestion(...))` at line 202 — untouched.

- [ ] **Step 2: Change the remote call site**

In `remote_actions.go`, replace:

```go
	if !confirmOverlay(fmt.Sprintf("kill PID %d on %s?", pid, host), c.modalWakes) {
		return
	}
```

with:

```go
	pane := startRemoteKillPreview(*s)
	confirmed := confirmOverlayPreview(fmt.Sprintf("kill PID %d on %s?", pid, host), pane, c.modalWakes)
	pane.close()
	if !confirmed {
		return
	}
```

Leave the second `confirmOverlay(worktreeRemovalQuestion(r.Worktree.Path), ...)` at line 291 untouched.

- [ ] **Step 3: Write the title test**

`killPreviewTitle` is the only new pure logic at these call sites. Append to `kill_preview_test.go`:

```go
func TestKillPreviewTitle(t *testing.T) {
	local := Session{PID: 4242, Name: "my-session", NameSource: "user"}
	if got := killPreviewTitle(local); !strings.Contains(got, "my-session") || !strings.Contains(got, "pid 4242") {
		t.Fatalf("local title = %q", got)
	}
	remote := Session{PID: 99, Host: "pi", Name: "my-session", NameSource: "user"}
	got := killPreviewTitle(remote)
	if !strings.Contains(got, "pi:99") {
		t.Fatalf("remote title = %q, want host-qualified", got)
	}
	if strings.Contains(got, "pid 99") {
		t.Fatalf("remote title should not use the local pid form: %q", got)
	}
}
```

Check `session_test.go` for the exact field name that makes `DisplayName` return a user-set name without dimming — `NameSource` must be anything other than `"derived"` (session.go:107). Adjust the fixture if the real field values differ.

- [ ] **Step 4: Run the tests**

Run: `go test ./... -run 'TestKillPreviewTitle|TestActKill' -v`
Expected: PASS.

Run: `go test -race ./... && go vet ./...`
Expected: PASS, full suite green.

- [ ] **Step 5: Build and check it by hand**

```bash
make install
```

Then in a terminal with at least 30 rows: launch the TUI, select a tmux-backed session, press `k`. Expected — the box appears immediately, briefly shows `loading preview…`, then grows to show the pane tail with `tmux` marked dim on the title row. Press `n`. Repeat on a remote row, and once on a terminal shrunk below 14 rows to confirm the plain box comes back.

- [ ] **Step 6: Commit**

```bash
git add actions.go remote_actions.go kill_preview_test.go
git commit -m "feat: show a session preview in the kill confirmation dialog"
```

---

## Self-review notes

Checked against `docs/2026-07-27-kill-preview-design.md`:

- **Spec coverage** — layout (T2), async fetch + wake pipe (T3, T4), sizing rules and the floor-gating fix (T2), line preparation with the per-line reset (T1), fetch limits (T3), lifetime/cancellation (T3), failure states (T1), scope limited to the two kill confirms (T5). All covered.
- **Deviation from the spec, deliberate:** the spec's failure table listed both `no preview available` and `preview unavailable: <reason>`. `LoadPreview` returns a single error when tmux and transcript both fail, so these are one branch; the plan renders `preview unavailable: <err>` for it and drops the separate string. One fewer state, same information.
- **Spec correction:** the spec wrote `PreviewLimits{Lines, Bytes}`. The real field names are `MaxLines`/`MaxBytes` (preview.go:31-34). The plan uses the real ones.
- **Signature ripple:** `renderConfirmOverlay` gaining a parameter breaks five existing calls in `confirm_overlay_test.go` and one in `confirm_overlay.go`. Task 2 Step 1 updates them before anything else, so the package never sits broken between steps.
