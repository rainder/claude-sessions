# Full-Screen Session Inspector with Mouse Support Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a read-only full-screen inspector with live tmux output, transcript fallback, local/remote parity, keyboard scrolling, and whole-app mouse support.

**Architecture:** Keep `RunTUI` as sole terminal owner. Add a stateful typed input decoder, explicit screen/view state, render-time hit regions, a bounded/sanitized preview provider, and an asynchronous `InspectorHub` modeled after `RemoteHub`. Preserve full-frame redraws and the existing `unix.Select` loop while generalizing wake descriptors for remote, inspector, and resize signals.

**Tech Stack:** Go 1.26.3, standard library, `golang.org/x/term`, `golang.org/x/sys/unix`.

## Global Constraints

- Add no dependencies; retain only `golang.org/x/term` and `golang.org/x/sys`.
- Preserve the single-stdin-consumer invariant: no goroutine may read stdin.
- Preserve `unix.Select` wall-clock ticking; do not replace it with `os.Stdin.SetReadDeadline`.
- Re-enable output processing after every `term.MakeRaw` call.
- Disable mouse reporting before cooked prompts, tmux/SSH attach, external terminal handoff, and shutdown.
- Support macOS/BSD and Linux using existing cross-platform termios files.
- Keep remote session identity as `Session.ID()` (`host:pid`); do not add another ID scheme.
- Preserve existing uncommitted changes. Before every task, inspect assigned-file diffs; never reset, clean, or stash the working tree.
- Run `gofmt`, focused tests, `go test ./...`, and `go vet ./...` before completion.

## File Structure

### New files

- `tui_events.go` — typed keyboard/mouse decoder and multi-wake `unix.Select` polling.
- `tui_events_test.go` — chunk-boundary, SGR mouse, key, wake-fd, and malformed-input tests.
- `tui_state.go` — screen mode, table viewport, click timing, hit regions, and pure state transitions.
- `tui_state_test.go` — viewport, row click, double-click, and inspector navigation tests.
- `inspector.go` — inspector snapshot/state, local/remote fetchers, and asynchronous `InspectorHub`.
- `inspector_test.go` — hub refresh, stale retention, target identity, and follow-state tests.
- `render_inspector.go` — metadata-strip inspector renderer.
- `render_inspector_test.go` — responsive layout, status footer, viewport, and hit-region tests.
- `preview_test.go` — bounded capture, transcript fallback, and terminal sanitizer tests.

### Existing files

- `tui.go` — integrate typed events, screen dispatch, wake sources, resize source, and inspector lifecycle.
- `helpers.go` — testable mouse mode sequences and terminal handoff cleanup.
- `render.go` — produce session row metadata and vertically crop the table frame.
- `render_test.go` — list viewport and row-hit metadata tests.
- `preview.go` — bounded local preview contract, transcript bounds, and sanitizer.
- `server.go` — backward-compatible preview query limits and source response header.
- `server_test.go` — preview handler defaults, bounds, validation, and compatibility.
- `remote_actions.go` — preview-specific remote HTTP fetch; remove blocking remote preview modal.
- `actions.go` — remove blocking local preview modal; keep action helpers for kill/attach/new.
- `selection.go` — add exact target lookup helper; retain current target ID rules.
- `selection_test.go` — target lookup and empty-host behavior.

---

### Task 1: Add Stateful Keyboard and SGR Mouse Decoder

**Files:**
- Create: `tui_events.go`
- Create: `tui_events_test.go`
- Modify later, not in this task: `tui.go`

**Interfaces:**
- Produces: `inputEvent`, `mouseEvent`, `inputDecoder`, `newInputDecoder()`, `(*inputDecoder).Feed([]byte, time.Time) []inputEvent`, `(*inputDecoder).Flush(time.Time) []inputEvent`, and `(*inputDecoder).PendingTimeout(time.Time) (time.Duration, bool)`.
- Keeps existing key sentinel strings (`KeyUp`, `KeyDown`, `KeyLeft`, `KeyRight`, `KeyEsc`) and adds `KeyEnter`, `KeyHome`, `KeyEnd`, `KeyPageUp`, and `KeyPageDown` so action migration remains mechanical.

- [ ] **Step 1: Write decoder tests covering chunk boundaries and keys**

```go
func TestInputDecoderArrowSplitAtEveryBoundary(t *testing.T) {
	seq := []byte("\x1b[A")
	for split := 1; split < len(seq); split++ {
		d := newInputDecoder()
		if got := d.Feed(seq[:split], time.Unix(0, 0)); len(got) != 0 {
			t.Fatalf("split %d first feed = %#v, want none", split, got)
		}
		got := d.Feed(seq[split:], time.Unix(0, int64(time.Millisecond)))
		if len(got) != 1 || got[0].kind != eventKey || got[0].key != KeyUp {
			t.Fatalf("split %d result = %#v, want KeyUp", split, got)
		}
	}
}

func TestInputDecoderExtendedKeys(t *testing.T) {
	cases := map[string]string{
		"\r": KeyEnter, "\n": KeyEnter,
		"\x1b[H": KeyHome, "\x1b[F": KeyEnd,
		"\x1b[1~": KeyHome, "\x1b[4~": KeyEnd,
		"\x1b[5~": KeyPageUp, "\x1b[6~": KeyPageDown,
	}
	for seq, want := range cases {
		d := newInputDecoder()
		got := d.Feed([]byte(seq), time.Unix(0, 0))
		if len(got) != 1 || got[0].key != want {
			t.Errorf("%q = %#v, want %q", seq, got, want)
		}
	}
}
```

- [ ] **Step 2: Write SGR mouse and malformed-input tests**

```go
func TestInputDecoderSGRMouse(t *testing.T) {
	cases := []struct {
		seq     string
		button  mouseButton
		release bool
		x, y    int
	}{
		{"\x1b[<0;12;7M", mouseLeft, false, 11, 6},
		{"\x1b[<0;12;7m", mouseLeft, true, 11, 6},
		{"\x1b[<64;2;3M", mouseWheelUp, false, 1, 2},
		{"\x1b[<65;2;3M", mouseWheelDown, false, 1, 2},
	}
	for _, tc := range cases {
		d := newInputDecoder()
		got := d.Feed([]byte(tc.seq), time.Unix(0, 0))
		if len(got) != 1 || got[0].kind != eventMouse {
			t.Fatalf("%q = %#v", tc.seq, got)
		}
		m := got[0].mouse
		if m.button != tc.button || m.release != tc.release || m.x != tc.x || m.y != tc.y {
			t.Errorf("%q mouse = %#v", tc.seq, m)
		}
	}
}

func TestInputDecoderBareEscapeFlushes(t *testing.T) {
	d := newInputDecoder()
	now := time.Unix(0, 0)
	if got := d.Feed([]byte{'\x1b'}, now); len(got) != 0 {
		t.Fatalf("Feed(ESC) = %#v, want pending", got)
	}
	if got := d.Flush(now.Add(escapeSequenceDelay / 2)); len(got) != 0 {
		t.Fatalf("early Flush = %#v, want pending", got)
	}
	got := d.Flush(now.Add(escapeSequenceDelay))
	if len(got) != 1 || got[0].key != KeyEsc {
		t.Fatalf("Flush = %#v, want KeyEsc", got)
	}
}
```

- [ ] **Step 3: Run tests to verify failure**

Run: `go test ./... -run 'TestInputDecoder'`

Expected: FAIL because decoder types and functions do not exist.

- [ ] **Step 4: Implement typed decoder**

Use these exact public-to-package interfaces:

```go
const escapeSequenceDelay = 20 * time.Millisecond

const (
	KeyEnter    = "\x00enter"
	KeyHome     = "\x00home"
	KeyEnd      = "\x00end"
	KeyPageUp   = "\x00page-up"
	KeyPageDown = "\x00page-down"
)

type eventKind uint8
const (
	eventKey eventKind = iota
	eventMouse
)

type mouseButton uint8
const (
	mouseLeft mouseButton = iota
	mouseMiddle
	mouseRight
	mouseRelease
	mouseWheelUp
	mouseWheelDown
)

type mouseEvent struct {
	x, y    int
	button  mouseButton
	release bool
}

type inputEvent struct {
	kind  eventKind
	key   string
	mouse mouseEvent
}

type inputDecoder struct {
	buf          []byte
	pendingSince time.Time
}

func newInputDecoder() *inputDecoder { return &inputDecoder{} }
```

Implement `Feed` as a loop over buffered bytes. Preserve incomplete `ESC`, CSI, SS3, and SGR sequences. Recognize:

```go
var fixedSequences = map[string]string{
	"\x1b[A": KeyUp, "\x1b[B": KeyDown,
	"\x1b[C": KeyRight, "\x1b[D": KeyLeft,
	"\x1bOA": KeyUp, "\x1bOB": KeyDown,
	"\x1bOC": KeyRight, "\x1bOD": KeyLeft,
	"\x1b[H": KeyHome, "\x1b[F": KeyEnd,
	"\x1bOH": KeyHome, "\x1bOF": KeyEnd,
	"\x1b[1~": KeyHome, "\x1b[4~": KeyEnd,
	"\x1b[5~": KeyPageUp, "\x1b[6~": KeyPageDown,
}
```

Parse SGR reports by requiring `ESC [ <`, a final `M` or `m`, and three semicolon-separated positive decimal fields. Convert terminal coordinates to zero-based coordinates. Reject reports longer than 64 bytes and coordinates below 1. Map button codes `0`, `1`, `2`, `64`, and `65`; ignore modifier bits after masking with `& 0b1100011` only where needed for those base codes.

`Flush` emits `KeyEsc` only for a lone pending ESC after `escapeSequenceDelay`; malformed longer sequences are discarded after the same delay. `PendingTimeout` returns remaining delay whenever `buf` starts with ESC.

- [ ] **Step 5: Format and run tests**

Run: `gofmt -w tui_events.go tui_events_test.go && go test ./... -run 'TestInputDecoder'`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add tui_events.go tui_events_test.go
git commit -m "feat(tui): decode keyboard and SGR mouse events"
```

---

### Task 2: Add Multi-Wake Polling and Resize Wake Source

**Files:**
- Modify: `tui_events.go`
- Modify: `tui_events_test.go`
- Modify later, not in this task: `tui.go`

**Interfaces:**
- Consumes: `inputDecoder` from Task 1.
- Produces: `wakeKind`, `wakeFD`, `pollEvents`, and `resizeWake`.

- [ ] **Step 1: Write pipe-backed polling tests**

```go
func TestPollEventsReportsEachWakeSource(t *testing.T) {
	remoteR, remoteW := testPipe(t)
	inspectorR, inspectorW := testPipe(t)
	_, _ = unix.Write(remoteW, []byte{1})
	_, _ = unix.Write(inspectorW, []byte{1})

	_, woke := pollEvents(newInputDecoder(), 50*time.Millisecond, []wakeFD{
		{fd: remoteR, kind: wakeRemote},
		{fd: inspectorR, kind: wakeInspector},
	})
	if woke != wakeRemote|wakeInspector {
		t.Fatalf("woke = %b, want remote|inspector", woke)
	}
}

func TestResizeWakeSignals(t *testing.T) {
	r, err := newResizeWake()
	if err != nil { t.Fatal(err) }
	defer r.Close()
	r.Signal()
	_, woke := pollEvents(newInputDecoder(), 50*time.Millisecond,
		[]wakeFD{{fd: r.FD(), kind: wakeResize}})
	if woke&wakeResize == 0 {
		t.Fatalf("woke = %b, want resize", woke)
	}
}
```

Define the pipe helper the tests use:

```go
// testPipe returns a non-blocking pipe closed automatically at test end.
func testPipe(t *testing.T) (r, w int) {
	t.Helper()
	var p [2]int
	if err := unix.Pipe(p[:]); err != nil {
		t.Fatalf("pipe: %v", err)
	}
	_ = unix.SetNonblock(p[0], true)
	_ = unix.SetNonblock(p[1], true)
	t.Cleanup(func() { _ = unix.Close(p[0]); _ = unix.Close(p[1]) })
	return p[0], p[1]
}
```

- [ ] **Step 2: Run tests to verify failure**

Run: `go test ./... -run 'TestPollEvents|TestResizeWake'`

Expected: FAIL because polling and resize types do not exist.

- [ ] **Step 3: Implement wake masks and generalized polling**

```go
type wakeKind uint8
const (
	wakeNone wakeKind = 0
	wakeRemote wakeKind = 1 << iota
	wakeInspector
	wakeResize
)

type wakeFD struct {
	fd   int
	kind wakeKind
}

func pollEvents(dec *inputDecoder, timeout time.Duration, wakes []wakeFD) ([]inputEvent, wakeKind)
```

`pollEvents` must:

1. Add stdin and every non-negative wake descriptor to one `unix.FdSet`.
2. Reduce `timeout` when `dec.PendingTimeout(time.Now())` is sooner.
3. Drain every ready wake descriptor until `EAGAIN` and OR its kind into the returned mask.
4. Read up to 256 stdin bytes once and pass them to `dec.Feed`.
5. On select timeout, call `dec.Flush(time.Now())` before returning.
6. Preserve `timeout == 0` as indefinite blocking unless the decoder has a pending escape deadline.

Add:

```go
type resizeWake struct {
	wakeR int
	wakeW int
	once  sync.Once
}

func newResizeWake() (*resizeWake, error)
func (r *resizeWake) FD() int
func (r *resizeWake) Signal()
func (r *resizeWake) Close()
```

Use a non-blocking close-on-exec pipe matching `NewRemoteHub`. `Signal` performs a best-effort single-byte `unix.Write`; a full pipe is acceptable.

- [ ] **Step 4: Format and run focused tests**

Run: `gofmt -w tui_events.go tui_events_test.go && go test ./... -run 'TestPollEvents|TestResizeWake|TestInputDecoder'`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add tui_events.go tui_events_test.go
git commit -m "refactor(tui): multiplex input and wake events"
```

---

### Task 3: Make Mouse Reporting Lifecycle Explicit and Testable

**Files:**
- Modify: `helpers.go:14-28,52-59,104-120`
- Create: `helpers_test.go`
- Modify later, not in this task: `tui.go`

**Interfaces:**
- Produces: `mouseEnableSequence`, `mouseDisableSequence`, `writeMouseMode(io.Writer, bool)`.
- Changes: `enterCooked` disables mouse before restoring terminal; `runInteractive` disables before handoff and enables after TUI restoration. `enterRaw` remains mouse-neutral so `pauseForKey` cannot leave partial mouse reports in stdin.

- [ ] **Step 1: Write sequence tests**

```go
func TestWriteMouseMode(t *testing.T) {
	var b strings.Builder
	writeMouseMode(&b, true)
	writeMouseMode(&b, false)
	want := "\x1b[?1000h\x1b[?1006h\x1b[?1006l\x1b[?1000l"
	if b.String() != want {
		t.Fatalf("mouse sequences = %q, want %q", b.String(), want)
	}
}
```

- [ ] **Step 2: Run test to verify failure**

Run: `go test ./... -run TestWriteMouseMode`

Expected: FAIL because `writeMouseMode` does not exist.

- [ ] **Step 3: Add lifecycle helper and update handoff sequences**

```go
const (
	mouseEnableSequence  = "\x1b[?1000h\x1b[?1006h"
	mouseDisableSequence = "\x1b[?1006l\x1b[?1000l"
)

func writeMouseMode(w io.Writer, enabled bool) {
	if enabled {
		_, _ = io.WriteString(w, mouseEnableSequence)
		return
	}
	_, _ = io.WriteString(w, mouseDisableSequence)
}
```

Update `enterCooked`:

```go
func enterCooked(fd int, oldState *term.State) {
	writeMouseMode(os.Stdout, false)
	_ = term.Restore(fd, oldState)
	fmt.Print("\033[?25h")
}
```

Update `runInteractive` so its exit sequence begins with `mouseDisableSequence` and its re-entry sequence ends with `mouseEnableSequence`. Do not add mouse enabling to `enterRaw`; callers returning to the main TUI will explicitly call `writeMouseMode(os.Stdout, true)` during Task 8.

- [ ] **Step 4: Format and run tests**

Run: `gofmt -w helpers.go helpers_test.go && go test ./... -run 'TestWriteMouseMode|TestSanitizeForTmux'`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add helpers.go helpers_test.go
git commit -m "feat(tui): manage terminal mouse reporting"
```

---

### Task 4: Add Screen State, Table Viewport, and Render Hit Regions

**Files:**
- Create: `tui_state.go`
- Create: `tui_state_test.go`
- Modify: `selection.go:7-82`
- Modify: `selection_test.go`
- Modify: `render.go:446-463` and each full/intermediate/minimal row writer
- Modify: `render_test.go`

**Interfaces:**
- Consumes: `mouseEvent` from Task 1.
- Produces: `screenMode`, `hitAction`, `hitRegion`, `tableFrame`, `BuildTableFrame`, `cropTableFrame`, `tuiState`, and pure list-event transitions.

- [ ] **Step 1: Add exact target lookup tests**

```go
func TestFindSelectionTarget(t *testing.T) {
	targets := []selectionTarget{
		sessionSelectionTarget(Session{PID: 11}),
		emptyHostSelectionTarget("dev"),
	}
	if got := findSelectionTarget(targets, "11"); got == nil || got.session.PID != 11 {
		t.Fatalf("find local = %#v", got)
	}
	if got := findSelectionTarget(targets, emptyHostSelectionID("dev")); got == nil || got.session != nil {
		t.Fatalf("find empty host = %#v", got)
	}
	if got := findSelectionTarget(targets, "missing"); got != nil {
		t.Fatalf("find missing = %#v", got)
	}
}
```

Implement:

```go
func findSelectionTarget(targets []selectionTarget, id string) *selectionTarget {
	for i := range targets {
		if targets[i].id == id { return &targets[i] }
	}
	return nil
}
```

Change `actCtx.selectedTarget` later to call this helper rather than maintaining another scan.

- [ ] **Step 2: Write table-frame and viewport tests**

```go
func TestBuildTableFrameRecordsSessionAndEmptyHostRows(t *testing.T) {
	frame := BuildTableFrame("2",
		[]Session{{PID: 11, CWD: "/tmp/local"}},
		[]RemoteResult{{Name: "dev"}}, "11", nil, 100, 0, "dir")
	if frame.targetLine("11") < 0 {
		t.Fatal("local target row missing")
	}
	if frame.targetLine(emptyHostSelectionID("dev")) < 0 {
		t.Fatal("empty-host target row missing")
	}
}

func TestCropTableFrameMapsVisibleRows(t *testing.T) {
	frame := tableFrame{
		lines: []string{"header", "row-a", "row-b", "row-c"},
		rows: []tableRow{{line: 1, targetID: "a", openable: true}, {line: 3, targetID: "c", openable: true}},
	}
	visible := cropTableFrame(frame, 1, 2, 80)
	if visible.text != "row-a\nrow-b" { t.Fatalf("text = %q", visible.text) }
	if len(visible.hits) != 1 || visible.hits[0].targetID != "a" || visible.hits[0].y0 != 0 {
		t.Fatalf("hits = %#v", visible.hits)
	}
}
```

- [ ] **Step 3: Write click and double-click tests**

```go
func TestListMouseSingleSelectThenDoubleClickOpen(t *testing.T) {
	now := time.Unix(100, 0)
	s := newTUIState()
	s.hits = []hitRegion{{x0: 0, y0: 4, x1: 79, y1: 4,
		action: hitSelectSession, targetID: "42", openable: true}}

	cmd := s.handleListMouse(mouseEvent{x: 10, y: 4, button: mouseLeft}, now)
	if s.sel != "42" || cmd != commandRender { t.Fatalf("first click: state=%#v cmd=%v", s, cmd) }
	cmd = s.handleListMouse(mouseEvent{x: 10, y: 4, button: mouseLeft}, now.Add(200*time.Millisecond))
	if cmd != commandOpenInspector { t.Fatalf("second click command = %v", cmd) }
}

func TestListMouseEmptyHostNeverOpens(t *testing.T) {
	s := newTUIState()
	s.hits = []hitRegion{{x0: 0, y0: 2, x1: 79, y1: 2,
		action: hitSelectSession, targetID: emptyHostSelectionID("dev"), openable: false}}
	now := time.Unix(100, 0)
	_ = s.handleListMouse(mouseEvent{x: 1, y: 2, button: mouseLeft}, now)
	if cmd := s.handleListMouse(mouseEvent{x: 1, y: 2, button: mouseLeft}, now.Add(100*time.Millisecond)); cmd == commandOpenInspector {
		t.Fatal("empty host opened inspector")
	}
}
```

- [ ] **Step 4: Run tests to verify failure**

Run: `go test ./... -run 'TestFindSelectionTarget|TestBuildTableFrame|TestCropTableFrame|TestListMouse'`

Expected: FAIL because state/frame interfaces do not exist.

- [ ] **Step 5: Implement state and frame types**

```go
type screenMode uint8
const (
	screenSessions screenMode = iota
	screenInspector
)

type hitAction uint8
const (
	hitSelectSession hitAction = iota
	hitInspectorBack
	hitInspectorRefresh
	hitInspectorFollow
)

type hitRegion struct {
	x0, y0, x1, y1 int
	action          hitAction
	targetID        string
	openable        bool
}

type tableRow struct {
	line     int
	targetID string
	openable bool
}

type tableFrame struct {
	lines       []string
	rows        []tableRow
	overflowing bool
}

type visibleFrame struct {
	text string
	hits []hitRegion
}
```

Add a lookup method used by tests and the render path:

```go
// targetLine returns the frame line index for a target ID, or -1 if absent.
func (f tableFrame) targetLine(id string) int {
	for _, r := range f.rows {
		if r.targetID == id {
			return r.line
		}
	}
	return -1
}
```

Add `BuildTableFrame` as a sibling to `RenderAll`. Use a line-counting writer around `strings.Builder`; each full/intermediate/minimal row writer records `Session.ID()` immediately before printing its row. `renderEmptyHostRow` records `emptyHostSelectionID(host)` with `openable: false`. Keep `RenderAll` as a compatibility wrapper that writes `strings.Join(frame.lines, "\n")` and returns `frame.overflowing`, so `--once` and current tests retain their API.

Implement `cropTableFrame(frame tableFrame, offset, rows, cols int) visibleFrame`: clamp offset, select at most `rows` lines, call `clipLine` per visible line, and convert each retained `tableRow.line` into a full-width zero-based `hitRegion` at `line-offset`.

Add state:

```go
const doubleClickWindow = 350 * time.Millisecond

type tuiCommand uint8
const (
	commandNone tuiCommand = iota
	commandRender
	commandOpenInspector
	commandBack
	commandRefreshInspector
	commandFollowInspector
)

type tuiState struct {
	mode        screenMode
	sel         string
	listOffset  int
	hits        []hitRegion
	lastClickID string
	lastClickAt time.Time
	inspector   inspectorViewState // introduced in Task 6; temporarily omit field until then
}
```

```go
// newTUIState starts on the session list with no selection.
func newTUIState() *tuiState {
	return &tuiState{mode: screenSessions}
}
```

For this task, define all fields except `inspector`; Task 6 adds it. `handleListMouse` ignores release events, scrolls three lines on wheel events, selects on first left press, and returns `commandOpenInspector` only for a second press on the same openable target within `doubleClickWindow`.

Add `ensureLineVisible(offset, line, viewportRows, totalLines int) int` and tests for above/below/clamped cases. Keyboard selection integration happens in Task 8.

- [ ] **Step 6: Format and run tests**

Run: `gofmt -w selection.go selection_test.go tui_state.go tui_state_test.go render.go render_test.go && go test ./... -run 'TestFindSelectionTarget|TestBuildTableFrame|TestCropTableFrame|TestListMouse|TestEnsureLineVisible'`

Expected: PASS.

- [ ] **Step 7: Run render regression tests**

Run: `go test ./... -run 'Test.*Render|TestHeadless|TestEmptyRemote|TestUsage'`

Expected: PASS with existing rendered text unchanged when `RenderAll` is used.

- [ ] **Step 8: Commit**

```bash
git add selection.go selection_test.go tui_state.go tui_state_test.go render.go render_test.go
git commit -m "feat(tui): add table viewport and mouse hit regions"
```

---

### Task 5: Add Safe Bounded Preview Contract and Remote Server Support

**Files:**
- Modify: `preview.go:12-78`
- Create: `preview_test.go`
- Modify: `server.go:60-72`
- Modify: `server_test.go`
- Modify: `remote_actions.go:19-50`

**Interfaces:**
- Produces: `PreviewLimits`, `PreviewResult`, `LoadPreview`, `sanitizeTerminalText`, `fetchRemotePreview`.
- Preserves: `PreviewContent(pid int) string` as compatibility wrapper for shell subcommand and older clients.

- [ ] **Step 1: Write sanitizer tests**

```go
func TestSanitizeTerminalTextPreservesSGRAndStripsControls(t *testing.T) {
	in := "ok\x1b[31mred\x1b[0m" +
		"\x1b]0;owned\x07" +
		"\x1b]8;;https://example.com\x1b\\link\x1b]8;;\x1b\\" +
		"\x1b[2J\x1b[?1000hEND\r\n"
	want := "ok\x1b[31mred\x1b[0mlinkEND\n"
	if got := sanitizeTerminalText(in); got != want {
		t.Fatalf("sanitize = %q, want %q", got, want)
	}
}

func TestLimitPreviewKeepsNewestLinesWithinBytes(t *testing.T) {
	in := strings.Repeat("old\n", 20) + "new-a\nnew-b\n"
	got := limitPreview(in, PreviewLimits{MaxLines: 2, MaxBytes: 64})
	if got != "new-a\nnew-b\n" { t.Fatalf("limit = %q", got) }
}
```

- [ ] **Step 2: Write preview-provider tests**

Introduce injectable package variables only around external effects:

```go
var previewTmuxCapture = captureTmuxPreview
var previewSessionLookup = readSessionByPID
```

Tests replace and restore them with `t.Cleanup`.

```go
func TestLoadPreviewUsesBoundedTmuxCapture(t *testing.T) {
	old := previewTmuxCapture
	t.Cleanup(func() { previewTmuxCapture = old })
	previewTmuxCapture = func(pid int, limits PreviewLimits) (string, string, error) {
		if limits.MaxLines != 2000 || limits.MaxBytes != 512<<10 { t.Fatalf("limits = %#v", limits) }
		return "tmux pane dev:0.0", "hello\n", nil
	}
	got, err := LoadPreview(42, DefaultPreviewLimits())
	if err != nil || got.Source != "tmux" || got.Content != "hello\n" {
		t.Fatalf("result=%#v err=%v", got, err)
	}
}
```

Add transcript-fallback test using a temporary JSONL transcript and existing `HOME` test pattern from `model_test.go`.

- [ ] **Step 3: Write server compatibility and bounds tests**

Inject handler dependency through `server.previewLoader`:

```go
type server struct {
	token string
	host  string
	previewLoader func(int, PreviewLimits) (PreviewResult, error)
}
```

When nil, handler uses `LoadPreview`.

```go
func TestPreviewHandlerDefaultsAndHeaders(t *testing.T) {
	var got PreviewLimits
	s := &server{token: "test", previewLoader: func(pid int, limits PreviewLimits) (PreviewResult, error) {
		got = limits
		return PreviewResult{Source: "tmux", Label: "dev:0.0", Content: "hello\n"}, nil
	}}
	req := httptest.NewRequest(http.MethodGet, "/sessions/42/preview", nil)
	req.SetPathValue("pid", "42")
	req.Header.Set("Authorization", "Bearer test")
	rec := httptest.NewRecorder()
	s.preview(rec, req)
	if got != DefaultPreviewLimits() { t.Fatalf("limits = %#v", got) }
	if rec.Header().Get("X-Claude-Sessions-Preview-Source") != "tmux" { t.Fatalf("headers = %#v", rec.Header()) }
	if rec.Body.String() != "hello\n" { t.Fatalf("body = %q", rec.Body.String()) }
}
```

Add `?lines=40&bytes=4096` test and bad/negative/out-of-range query tests expecting `400 Bad Request`. Clamp accepted values to `1..2000` lines and `1024..524288` bytes.

- [ ] **Step 4: Run tests to verify failure**

Run: `go test ./... -run 'TestSanitizeTerminalText|TestLimitPreview|TestLoadPreview|TestPreviewHandler'`

Expected: FAIL because preview contracts do not exist.

- [ ] **Step 5: Implement preview result and bounds**

```go
type PreviewLimits struct {
	MaxLines int
	MaxBytes int
}

func DefaultPreviewLimits() PreviewLimits {
	return PreviewLimits{MaxLines: 2000, MaxBytes: 512 << 10}
}

type PreviewResult struct {
	Source  string
	Label   string
	Content string
}

func LoadPreview(pid int, limits PreviewLimits) (PreviewResult, error)
```

`LoadPreview` behavior:

1. Try tmux using existing `tmuxPaneMap`, `ppidMap`, and `walkTmuxPane`.
2. Execute `tmux capture-pane -p -e -S -<MaxLines> -t <loc>`.
3. Sanitize output, then retain newest lines/bytes through `limitPreview`.
4. If no tmux pane exists, resolve session/transcript using existing helpers.
5. Render recent user/assistant entries, sanitize, and apply identical final bounds.
6. Return errors rather than embedding error strings in `Content`.

`PreviewContent(pid)` calls `LoadPreview(pid, DefaultPreviewLimits())`; on success it emits the existing human-readable `source:` prefix plus content, preserving CLI behavior. On failure it returns `<error>\n`.

`sanitizeTerminalText` preserves only complete CSI SGR sequences whose final byte is `m`. It strips all OSC sequences through BEL or ST, all non-SGR CSI sequences through their final byte, DCS/APC/PM sequences through ST, CR, and disallowed C0 controls. Expand tabs to four spaces and preserve newline.

- [ ] **Step 6: Implement backward-compatible server handler**

Parse optional `lines` and `bytes` with a helper:

```go
func previewLimitsFromRequest(r *http.Request) (PreviewLimits, error)
```

Without query parameters, use `DefaultPreviewLimits`. Return `400` for invalid values. On success set:

```go
w.Header().Set("Content-Type", "text/plain; charset=utf-8")
w.Header().Set("X-Claude-Sessions-Preview-Source", result.Source)
w.Header().Set("X-Claude-Sessions-Preview-Label", result.Label)
```

Return `404` when local session/transcript no longer exists and `500` for tmux capture/read errors.

- [ ] **Step 7: Add preview-specific remote fetcher**

```go
func fetchRemotePreview(host string, pid int, limits PreviewLimits) (PreviewResult, error)
```

Use `LookupServer`, `http.NewRequest`, five-second timeout, authorization header, and `io.LimitReader(resp.Body, int64(limits.MaxBytes)+1)`. Reject oversized responses. Read source and label headers. Treat `404` as `errSessionEnded`; return other non-200 responses with the same concise HTTP error style as `remoteRequest`.

- [ ] **Step 8: Format and run focused tests**

Run: `gofmt -w preview.go preview_test.go server.go server_test.go remote_actions.go && go test ./... -run 'TestSanitizeTerminalText|TestLimitPreview|TestLoadPreview|TestPreviewHandler|TestFetchRemotePreview'`

Expected: PASS.

- [ ] **Step 9: Run command/server regressions**

Run: `go test ./... -run 'TestNewSession|TestFindTranscript|TestScanTranscript|TestPidPart'`

Expected: PASS.

- [ ] **Step 10: Commit**

```bash
git add preview.go preview_test.go server.go server_test.go remote_actions.go
git commit -m "feat(preview): bound and sanitize session snapshots"
```

---

### Task 6: Add Inspector View State and Asynchronous Hub

**Files:**
- Create: `inspector.go`
- Create: `inspector_test.go`
- Modify: `tui_state.go`
- Modify: `tui_state_test.go`

**Interfaces:**
- Consumes: `PreviewResult`, `PreviewLimits`, `fetchRemotePreview`, `selectionTarget`.
- Produces: `InspectorSnapshot`, `InspectorHub`, `inspectorViewState`, scrolling/follow transitions.

- [ ] **Step 1: Write inspector viewport tests**

```go
func TestInspectorApplySnapshotFollowsBottom(t *testing.T) {
	v := newInspectorViewState("42")
	v.viewportRows = 3
	v.applySnapshot(InspectorSnapshot{TargetID: "42", Lines: []string{"1", "2", "3", "4"}})
	if v.top != 1 || !v.follow { t.Fatalf("view = %#v", v) }
	v.applySnapshot(InspectorSnapshot{TargetID: "42", Lines: []string{"1", "2", "3", "4", "5"}})
	if v.top != 2 { t.Fatalf("top = %d, want 2", v.top) }
}

func TestInspectorPausedPreservesTopAndCountsNewLines(t *testing.T) {
	v := newInspectorViewState("42")
	v.viewportRows = 2
	v.applySnapshot(InspectorSnapshot{TargetID: "42", Lines: []string{"1", "2", "3"}})
	v.scroll(-1)
	v.applySnapshot(InspectorSnapshot{TargetID: "42", Lines: []string{"1", "2", "3", "4", "5"}})
	if v.top != 0 || v.follow || v.newLines != 2 { t.Fatalf("view = %#v", v) }
	v.followBottom()
	if !v.follow || v.newLines != 0 || v.top != 3 { t.Fatalf("followed view = %#v", v) }
}
```

- [ ] **Step 2: Write hub stale-retention and target-ID tests**

Use an injected fetcher:

```go
type inspectorFetcher func(selectionTarget, PreviewLimits) (PreviewResult, error)
```

```go
func TestInspectorHubRetainsSnapshotOnRefreshError(t *testing.T) {
	calls := 0
	fetch := func(target selectionTarget, _ PreviewLimits) (PreviewResult, error) {
		calls++
		if calls == 1 { return PreviewResult{Source: "tmux", Content: "ok\n"}, nil }
		return PreviewResult{}, errors.New("offline")
	}
	h, err := newInspectorHub(sessionSelectionTarget(Session{PID: 42}), time.Hour, fetch)
	if err != nil { t.Fatal(err) }
	defer h.Shutdown()
	waitForInspectorSnapshot(t, h, func(s InspectorSnapshot) bool { return len(s.Lines) == 1 })
	h.Refresh()
	got := waitForInspectorSnapshot(t, h, func(s InspectorSnapshot) bool { return s.Stale })
	if strings.Join(got.Lines, "\n") != "ok" || got.Error != "offline" { t.Fatalf("snapshot = %#v", got) }
}
```

Add a test with local PID `42` and remote `dev:42` proving snapshots retain distinct `TargetID` values.

Define the polling helper both hub tests use:

```go
// waitForInspectorSnapshot polls Snapshot() until cond passes or 2s elapse.
func waitForInspectorSnapshot(t *testing.T, h *InspectorHub, cond func(InspectorSnapshot) bool) InspectorSnapshot {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s := h.Snapshot(); cond(s) {
			return s
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("snapshot condition not met; last = %#v", h.Snapshot())
	return InspectorSnapshot{}
}
```

- [ ] **Step 3: Run tests to verify failure**

Run: `go test ./... -run 'TestInspector'`

Expected: FAIL because inspector types do not exist.

- [ ] **Step 4: Implement snapshot and view state**

```go
var errSessionEnded = errors.New("session ended")

type InspectorSnapshot struct {
	TargetID string
	Session  Session
	Source   string
	Label    string
	Lines    []string
	Loading  bool
	Stale    bool
	Ended    bool
	Error    string
	Updated  time.Time
}

type inspectorViewState struct {
	targetID     string
	snapshot     InspectorSnapshot
	top          int
	viewportRows int
	follow       bool
	newLines     int
}

// newInspectorViewState starts in follow mode with no content.
func newInspectorViewState(targetID string) inspectorViewState {
	return inspectorViewState{targetID: targetID, follow: true}
}
```

Implement `scroll(delta int)`, `page(delta int)`, `home()`, `followBottom()`, `resize(rows int)`, and `applySnapshot`. Split `PreviewResult.Content` with normalized trailing-empty-line handling. Clamp all offsets through one helper.

- [ ] **Step 5: Implement InspectorHub**

Mirror `RemoteHub` fields and pipe setup:

```go
type InspectorHub struct {
	mu       sync.Mutex
	target   selectionTarget
	snapshot InspectorSnapshot
	limits   PreviewLimits
	fetch    inspectorFetcher
	kick     chan struct{}
	stop     chan struct{}
	wakeR    int
	wakeW    int
	once     sync.Once
}

func NewInspectorHub(target selectionTarget, interval time.Duration) (*InspectorHub, error)
func newInspectorHub(target selectionTarget, interval time.Duration, fetch inspectorFetcher) (*InspectorHub, error)
func (h *InspectorHub) WakeFD() int
func (h *InspectorHub) Snapshot() InspectorSnapshot
func (h *InspectorHub) Refresh()
func (h *InspectorHub) Shutdown()
```

Default fetcher dispatches by `target.session.Host`: local calls `LoadPreview`, remote calls `fetchRemotePreview`. Copy line slices from `Snapshot` so callers cannot race hub ownership.

On fetch error:

- `errors.Is(err, errSessionEnded)`: preserve lines, set `Ended=true`, `Loading=false`.
- Prior successful lines: preserve lines, set `Stale=true`, set concise `Error`.
- No prior lines: set `Loading=false`, set `Error`.

Every state change writes one wake byte. `Shutdown` uses `sync.Once`.

- [ ] **Step 6: Add inspector state to `tuiState`**

Add `inspector inspectorViewState` and pure handlers for inspector keys/mouse. Wheel moves three lines; Up/Down one line; Page keys one viewport; Home oldest; End/follow newest. Hit actions return `commandBack`, `commandRefreshInspector`, or `commandFollowInspector`.

- [ ] **Step 7: Format and run tests**

Run: `gofmt -w inspector.go inspector_test.go tui_state.go tui_state_test.go && go test ./... -run 'TestInspector|TestListMouse|TestEnsureLineVisible'`

Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add inspector.go inspector_test.go tui_state.go tui_state_test.go
git commit -m "feat(tui): add asynchronous inspector state"
```

---

### Task 7: Render Metadata-Strip Inspector and Clickable Footer

**Files:**
- Create: `render_inspector.go`
- Create: `render_inspector_test.go`

**Interfaces:**
- Consumes: `inspectorViewState`, `hitRegion`, existing `clipLine`, `bold`, `dim`, model/cost/context formatting helpers.
- Produces: `RenderInspector(w io.Writer, view inspectorViewState, cols, rows int) []hitRegion`.

- [ ] **Step 1: Write normal and narrow-layout tests**

```go
func TestRenderInspectorMetadataAndLiveFooter(t *testing.T) {
	v := newInspectorViewState("dev:42")
	v.snapshot = InspectorSnapshot{
		TargetID: "dev:42", Session: Session{PID: 42, Host: "dev", Name: "api-refactor", Model: "claude-opus-4-8", Status: "busy", ContextTokens: 42000, CostUSD: 1.28},
		Source: "tmux", Label: "dev:0.0", Lines: []string{"one", "two"},
	}
	v.viewportRows = 10
	var b strings.Builder
	hits := RenderInspector(&b, v, 100, 20)
	out := b.String()
	for _, want := range []string{"api-refactor", "PID 42", "dev", "opus", "busy", "LIVE", "Back", "Refresh", "Follow"} {
		if !strings.Contains(out, want) { t.Errorf("output missing %q:\n%s", want, out) }
	}
	if !hasHit(hits, hitInspectorBack) || !hasHit(hits, hitInspectorRefresh) || !hasHit(hits, hitInspectorFollow) {
		t.Fatalf("footer hits = %#v", hits)
	}
}

func TestRenderInspectorNarrowDropsMetadataBeforeControls(t *testing.T) {
	v := populatedInspectorView()
	var b strings.Builder
	hits := RenderInspector(&b, v, 38, 10)
	if strings.Contains(b.String(), "$1.28") { t.Fatalf("cost not collapsed:\n%s", b.String()) }
	if !strings.Contains(b.String(), "Back") || !hasHit(hits, hitInspectorBack) { t.Fatalf("Back missing") }
}
```

- [ ] **Step 2: Write status and minimum-size tests**

Add table cases for `Loading`, `Stale`, `Ended`, paused with new lines, and error without content. Add terminal size `20x4` expecting concise `terminal too small` plus Back hit region.

Define the shared test helpers used in Steps 1–2:

```go
// populatedInspectorView returns a view with full metadata for layout tests.
func populatedInspectorView() inspectorViewState {
	v := newInspectorViewState("dev:42")
	v.snapshot = InspectorSnapshot{
		TargetID: "dev:42",
		Session: Session{PID: 42, Host: "dev", Name: "api-refactor",
			Model: "claude-opus-4-8", Status: "busy",
			ContextTokens: 42000, CostUSD: 1.28},
		Source: "tmux", Label: "dev:0.0",
		Lines: []string{"one", "two"},
	}
	v.viewportRows = 6
	return v
}

// hasHit reports whether any region carries the given action.
func hasHit(hits []hitRegion, action hitAction) bool {
	for _, h := range hits {
		if h.action == action {
			return true
		}
	}
	return false
}
```

- [ ] **Step 3: Run tests to verify failure**

Run: `go test ./... -run TestRenderInspector`

Expected: FAIL because renderer does not exist.

- [ ] **Step 4: Implement inspector renderer**

```go
func RenderInspector(w io.Writer, view inspectorViewState, cols, rows int) []hitRegion
```

Render exactly:

1. Title: display name, PID, host when non-empty.
2. Metadata line: model, status, context, cost. At widths below 80 drop cost; below 64 drop context; below 48 show only status.
3. Separator.
4. Content lines from `view.top` for remaining body rows.
5. Footer on final row: `Back  Refresh  Follow` on left; source and status on right when space allows.

Status text priority: `SESSION ENDED`, `STALE`, `PAUSED · N new`, `LOADING`, `LIVE ↓`. Use `clipLine` for every emitted line. Return zero-based footer hit regions matching visible label columns. Follow remains clickable even while already following.

Never print cursor movement or alternate-screen sequences inside this function; `RunTUI` owns screen positioning.

- [ ] **Step 5: Format and run tests**

Run: `gofmt -w render_inspector.go render_inspector_test.go && go test ./... -run TestRenderInspector`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add render_inspector.go render_inspector_test.go
git commit -m "feat(tui): render fullscreen session inspector"
```

---

### Task 8: Integrate List and Inspector Screens into RunTUI

**Files:**
- Modify: `tui.go:30-130,134-340`
- Modify: `actions.go:37-56,149-190`
- Modify: `remote_actions.go:145-188`
- Modify: `helpers.go:65-101`
- Modify: `tui_test.go`
- Modify: `actions_test.go`

**Interfaces:**
- Consumes every prior task.
- Removes old blocking `actPreview` and `actPreviewRemote` modal loops.
- Produces one event loop owning both screens.

- [ ] **Step 1: Add pure command-dispatch tests before changing RunTUI**

Extract key-to-command mapping:

```go
func TestSessionScreenOpenKeys(t *testing.T) {
	for _, key := range []string{KeyEnter, "p", "P"} {
		if got := sessionKeyCommand(key); got != commandOpenInspector {
			t.Errorf("key %q command = %v", key, got)
		}
	}
}

func TestInspectorBackAndQuitKeys(t *testing.T) {
	for _, key := range []string{KeyEsc, "q", "Q", "p", "P"} {
		if got := inspectorKeyCommand(key); got != commandBack {
			t.Errorf("key %q command = %v", key, got)
		}
	}
	if got := inspectorKeyCommand("\x03"); got != commandQuit {
		t.Fatalf("Ctrl-C command = %v", got)
	}
}
```

Add `commandQuit` to `tuiCommand`.

- [ ] **Step 2: Run tests to verify failure**

Run: `go test ./... -run 'TestSessionScreenOpenKeys|TestInspectorBackAndQuitKeys'`

Expected: FAIL because key-command helpers do not exist.

- [ ] **Step 3: Replace old polling functions**

Remove `parseEvents`, `readEvents`, and their comments from `tui.go`. Change `readEventBlocking` to create a decoder and call `pollEvents(dec, 0, nil)`, returning only key strings for existing `pickMenu`. Ignore mouse events in modal menus.

Implement key-command helpers in `tui_state.go`; keep kill/attach/new/sort/view handling in `RunTUI`, since those require runtime dependencies rather than pure state.

- [ ] **Step 4: Initialize terminal, decoder, resize, and mouse lifecycle**

After raw mode and OPOST setup:

```go
writeMouseMode(os.Stdout, true)
fmt.Print("\033[?1049h\033[?25l\033[?7l")
defer func() {
	writeMouseMode(os.Stdout, false)
	fmt.Print("\033[?7h\033[?25h\033[?1049l")
}()
```

Create `resizeWake`, register `signal.Notify(resizeSignals, syscall.SIGWINCH)`, and run one goroutine that only translates resize signals to `resizeWake.Signal()`. It must never read stdin. On cleanup, `signal.Stop`, close the stop channel, then close the pipe.

Create one `inputDecoder` and `tuiState`. Keep `RemoteHub` and `UsageHub` lifecycle unchanged.

- [ ] **Step 5: Replace render closure with screen-aware renderer**

For session screen:

1. Call `BuildTableFrame`.
2. Find selected target's full-frame line.
3. Call `ensureLineVisible` to update `state.listOffset`.
4. Reserve bottom row for toast when active.
5. Call `cropTableFrame` with terminal height.
6. Save returned hit regions.

For inspector screen:

1. Copy `inspectorHub.Snapshot()` into `state.inspector.applySnapshot` when hub exists.
2. Set viewport rows from terminal height minus inspector chrome.
3. Call `RenderInspector` and save returned hit regions.

Both paths print one `"\033[H\033[J" + out` frame.

- [ ] **Step 6: Generalize event wait and wake handling**

Build wake descriptors each loop:

```go
wakes := []wakeFD{
	{fd: hub.WakeFD(), kind: wakeRemote},
	{fd: resizeWake.FD(), kind: wakeResize},
}
if inspectorHub != nil {
	wakes = append(wakes, wakeFD{fd: inspectorHub.WakeFD(), kind: wakeInspector})
}
events, woke := pollEvents(decoder, timeout, wakes)
```

Handling:

- `wakeRemote`: refresh local/list snapshots. If inspector target no longer exists, preserve last inspector content and mark ended.
- `wakeInspector`: apply latest inspector snapshot.
- `wakeResize`: call `term.GetSize`, clamp viewports, redraw without resetting wall-clock refresh.
- Timeout: preserve current wall-clock and toast behavior.

- [ ] **Step 7: Implement open/back inspector lifecycle**

Open only when `findSelectionTarget(targets, state.sel)` has non-nil `session`. Create `InspectorHub` from a copy of that target, set `state.mode = screenInspector`, initialize `state.inspector`, and render loading state immediately.

Back closes and nils the hub, resets inspector state, returns to `screenSessions`, refreshes list snapshots, and redraws. `Ctrl-C` shuts down active hub and exits. `r` in inspector calls only `inspectorHub.Refresh`; list `r` retains usage and remote refresh behavior.

- [ ] **Step 8: Route mouse events and restore mouse after actions**

Session screen:

- Wheel and left press call `state.handleListMouse`.
- Double-click command opens inspector.
- After keyboard navigation, update `state.sel` then make selected line visible on render.

Inspector screen:

- Wheel calls inspector scrolling.
- Footer hit actions back, refresh, or follow.

Before every existing cooked prompt, `enterCooked` disables mouse. Every return to main raw TUI must call:

```go
enterRaw(fd)
writeMouseMode(os.Stdout, true)
```

Add `actCtx.enterRaw()` helper to centralize those two calls, replace all `defer enterRaw(c.fd)` and direct `enterRaw(c.fd)` uses in `actions.go` and `remote_actions.go`, and leave `pauseForKey` mouse-neutral.

- [ ] **Step 9: Remove blocking preview actions**

Delete `actPreview` from `actions.go` and `actPreviewRemote` from `remote_actions.go`. Remove now-unused `time`/`os` imports. `p` is handled exclusively as a screen transition in `RunTUI`.

Change `actCtx.selectedTarget` to call `findSelectionTarget`.

- [ ] **Step 10: Update help text and tests**

Update `renderHelp` with:

```text
Enter / p    open full-screen inspector
mouse click  select row · double-click opens
mouse wheel  scroll list or inspector
Home / End   oldest output / resume live follow
PgUp / PgDn  scroll inspector by page
Esc / q / p  return from inspector
```

Add tests that `actCtx.enterRaw` calls mouse-enable writer through an injectable `terminalOutput io.Writer` package variable; restore it with `t.Cleanup`.

- [ ] **Step 11: Format and run focused integration tests**

Run: `gofmt -w tui.go tui_state.go tui_test.go actions.go actions_test.go remote_actions.go helpers.go && go test ./... -run 'TestSessionScreen|TestInspectorBack|TestListMouse|TestWriteMouseMode|TestActCtx'`

Expected: PASS.

- [ ] **Step 12: Run full automated verification**

Run: `go test ./...`

Expected: PASS.

Run: `go vet ./...`

Expected: PASS with no output.

- [ ] **Step 13: Commit**

```bash
git add tui.go tui_state.go tui_test.go actions.go actions_test.go remote_actions.go helpers.go
git commit -m "feat(tui): integrate fullscreen mouse inspector"
```

---

### Task 9: Cross-Platform Regression and Manual TUI Verification

**Files:**
- Modify only if verification finds defects: files from Tasks 1–8 and their paired tests.
- Update: `README.md` keyboard/mouse help section if it documents TUI controls.

**Interfaces:**
- Verifies complete feature; introduces no new architecture.

- [ ] **Step 1: Run formatting and repository checks**

```bash
gofmt -w *.go
go test ./...
go vet ./...
git diff --check
```

Expected: all commands pass; `git diff --check` prints nothing.

- [ ] **Step 2: Build every supported target**

Run: `make`

Expected: macOS/Linux amd64/arm64 binaries build successfully under existing Makefile targets.

- [ ] **Step 3: Run local manual smoke test**

Run: `go run .`

Verify in order:

1. Arrow navigation still works under continuous typing and refresh ticks still fire.
2. Single-click selects exact local and remote rows.
3. Double-click opens only session-backed rows.
4. Wheel scrolls long table without changing target identity.
5. `Enter` and `p` open inspector.
6. Tmux content retains colors but cannot clear screen or alter title/mouse mode.
7. Transcript fallback opens for non-tmux/headless session.
8. Scrolling pauses follow; new output does not move viewport; `End` resumes.
9. Back/Refresh/Follow footer controls work by mouse.
10. Resize immediately recalculates viewport and hit regions.
11. `Esc`, `q`, and `p` return to list; `Ctrl-C` exits.
12. Kill/new/attach prompts accept normal input and mouse reports do not leak.
13. Detaching from tmux/SSH restores raw mode, OPOST, alt screen, cursor, wrapping, and mouse support.

- [ ] **Step 4: Verify remote parity**

With one configured reachable server:

1. Open remote inspector.
2. Confirm source header and bounded content.
3. Stop server or disconnect network; prior content remains with `STALE`.
4. Restart server and press Refresh; inspector returns to `LIVE`.
5. End remote session; final content remains with `SESSION ENDED`.

- [ ] **Step 5: Verify terminal matrix**

Run smoke test on macOS Terminal or iTerm2, inside tmux, and on one Linux terminal. Keyboard-only operation must work when mouse reporting is unsupported or disabled.

- [ ] **Step 6: Add README controls if present**

If README has a controls table, add exact bindings from Task 8. Do not create a new long tutorial; link behavior to `p`/Enter inspector and mouse controls.

- [ ] **Step 7: Run final checks after any fixes**

Run: `go test ./... && go vet ./... && git diff --check`

Expected: PASS with no vet or whitespace output.

- [ ] **Step 8: Commit verification fixes/docs**

If no files changed, skip commit. Otherwise:

```bash
git add README.md '*.go'
git commit -m "docs(tui): document fullscreen inspector controls"
```

## Completion Criteria

- Full-screen inspector works for local and remote rows without blocking input.
- Tmux snapshot is primary; transcript fallback works when no pane exists.
- Mouse supports row selection, double-click opening, list/inspector wheel scrolling, and footer controls.
- Keyboard offers every mouse action.
- Follow, paused-new-output, stale, loading, error, and ended states render correctly.
- Captured data preserves SGR styling but cannot issue terminal-control side effects.
- Resize and all background refresh sources coexist through `unix.Select`.
- Prompt and interactive attach flows disable and restore mouse mode correctly.
- Existing TUI behavior, tests, builds, and dependency policy remain intact.
