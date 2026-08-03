package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestBuildTableFrameRecordsSessionAndEmptyHostRows(t *testing.T) {
	frame := BuildTableFrame("2",
		LocalHost{Name: "local", Sessions: []Session{{PID: 11, CWD: "/tmp/local"}}},
		[]RemoteResult{{Name: "dev"}}, "11", nil, 100, 0, "dir", groupView{})
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
		rows:  []tableRow{{line: 1, targetID: "a", openable: true}, {line: 3, targetID: "c", openable: true}},
	}
	visible := cropTableFrame(frame, 1, 2, 80)
	if visible.text != "row-a\nrow-b" {
		t.Fatalf("text = %q", visible.text)
	}
	if len(visible.hits) != 1 || visible.hits[0].targetID != "a" || visible.hits[0].y0 != 0 {
		t.Fatalf("hits = %#v", visible.hits)
	}
}

func TestListMouseSingleSelectThenDoubleClickOpen(t *testing.T) {
	now := time.Unix(100, 0)
	s := newTUIState()
	s.hits = []hitRegion{{x0: 0, y0: 4, x1: 79, y1: 4,
		action: hitSelectSession, targetID: "42", openable: true}}

	cmd := s.handleListMouse(mouseEvent{x: 10, y: 4, button: mouseLeft}, now)
	if s.sel != "42" || cmd != commandRender {
		t.Fatalf("first click: state=%#v cmd=%v", s, cmd)
	}
	cmd = s.handleListMouse(mouseEvent{x: 10, y: 4, button: mouseLeft}, now.Add(200*time.Millisecond))
	if cmd != commandOpenInspector {
		t.Fatalf("second click command = %v", cmd)
	}
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

func TestInspectorKeyHandlers(t *testing.T) {
	setup := func() *tuiState {
		s := newTUIState()
		s.inspector = newInspectorViewState("42")
		s.inspector.resize(3)
		s.inspector.applySnapshot(InspectorSnapshot{
			TargetID: "42",
			Lines:    []string{"1", "2", "3", "4", "5", "6"},
		})
		// follow mode parks the view at the bottom: top = 6 - 3 = 3.
		return s
	}

	s := setup()
	if cmd := s.handleInspectorKey(KeyUp); cmd != commandRender || s.inspector.top != 2 || s.inspector.follow {
		t.Fatalf("KeyUp: cmd=%v view=%#v", cmd, s.inspector)
	}
	if cmd := s.handleInspectorKey(KeyDown); cmd != commandRender || s.inspector.top != 3 || !s.inspector.follow {
		t.Fatalf("KeyDown: cmd=%v view=%#v", cmd, s.inspector)
	}
	if cmd := s.handleInspectorKey(KeyPageUp); cmd != commandRender || s.inspector.top != 0 {
		t.Fatalf("KeyPageUp: cmd=%v view=%#v", cmd, s.inspector)
	}
	if cmd := s.handleInspectorKey(KeyHome); cmd != commandRender || s.inspector.top != 0 || s.inspector.follow {
		t.Fatalf("KeyHome: cmd=%v view=%#v", cmd, s.inspector)
	}

	// Follow / refresh / back defer to the render loop via their commands and
	// do not mutate the view state directly.
	s = setup()
	s.handleInspectorKey(KeyUp) // leave follow mode so End has an effect to defer
	if cmd := s.handleInspectorKey(KeyEnd); cmd != commandFollowInspector || s.inspector.follow {
		t.Fatalf("KeyEnd: cmd=%v view=%#v", cmd, s.inspector)
	}
	if cmd := s.handleInspectorKey("r"); cmd != commandRefreshInspector {
		t.Fatalf("r: cmd=%v", cmd)
	}
	if cmd := s.handleInspectorKey("q"); cmd != commandBack {
		t.Fatalf("q: cmd=%v", cmd)
	}
	if cmd := s.handleInspectorKey(KeyEsc); cmd != commandBack {
		t.Fatalf("esc: cmd=%v", cmd)
	}
	if cmd := s.handleInspectorKey("z"); cmd != commandNone {
		t.Fatalf("unmapped key: cmd=%v", cmd)
	}

	// 'k'/'K' defers to the render loop to open the kill confirmation, and no
	// longer scrolls (j/k scrolling was removed in favor of arrows only).
	s = setup()
	if cmd := s.handleInspectorKey("k"); cmd != commandKillInspector || s.inspector.top != 3 {
		t.Fatalf("k: cmd=%v view=%#v", cmd, s.inspector)
	}
	if cmd := s.handleInspectorKey("K"); cmd != commandKillInspector {
		t.Fatalf("K: cmd=%v", cmd)
	}
	if cmd := s.handleInspectorKey("j"); cmd != commandNone || s.inspector.top != 3 {
		t.Fatalf("j should no longer scroll: cmd=%v view=%#v", cmd, s.inspector)
	}
}

func TestHandleInspectorComposeAppendsPrintableAndBackspaces(t *testing.T) {
	s := &tuiState{inspector: inspectorViewState{composing: true}}
	for _, k := range []string{"h", "i"} {
		if submit, cancel := s.handleInspectorCompose(k); submit || cancel {
			t.Fatalf("handleInspectorCompose(%q) = (%v,%v), want (false,false)", k, submit, cancel)
		}
	}
	if s.inspector.composeText != "hi" {
		t.Fatalf("composeText = %q, want hi", s.inspector.composeText)
	}
	s.handleInspectorCompose("\x7f")
	if s.inspector.composeText != "h" {
		t.Fatalf("composeText after backspace = %q, want h", s.inspector.composeText)
	}
}

func TestHandleInspectorComposeEnterOnEmptyIsNoop(t *testing.T) {
	s := &tuiState{inspector: inspectorViewState{composing: true}}
	submit, cancel := s.handleInspectorCompose(KeyEnter)
	if submit || cancel {
		t.Fatalf("handleInspectorCompose(Enter on empty) = (%v,%v), want (false,false)", submit, cancel)
	}
}

func TestHandleInspectorComposeEnterOnNonEmptySubmitsAndPreservesText(t *testing.T) {
	s := &tuiState{inspector: inspectorViewState{composing: true, composeText: "hello"}}
	submit, cancel := s.handleInspectorCompose(KeyEnter)
	if !submit || cancel {
		t.Fatalf("handleInspectorCompose(Enter on \"hello\") = (%v,%v), want (true,false)", submit, cancel)
	}
	if !s.inspector.composing || s.inspector.composeText != "hello" {
		t.Fatalf("state after submit = composing=%v text=%q, want composing=true text=\"hello\" (caller clears on success)", s.inspector.composing, s.inspector.composeText)
	}
}

func TestHandleInspectorComposeEscCancelsAndDiscards(t *testing.T) {
	s := &tuiState{inspector: inspectorViewState{composing: true, composeText: "hello"}}
	submit, cancel := s.handleInspectorCompose(KeyEsc)
	if submit || !cancel {
		t.Fatalf("handleInspectorCompose(Esc) = (%v,%v), want (false,true)", submit, cancel)
	}
	if s.inspector.composing || s.inspector.composeText != "" {
		t.Fatalf("state after Esc = composing=%v text=%q, want composing=false text=\"\"", s.inspector.composing, s.inspector.composeText)
	}
}

func TestHandleInspectorComposeCtrlCCancelsAndDiscards(t *testing.T) {
	s := &tuiState{inspector: inspectorViewState{composing: true, composeText: "hello"}}
	submit, cancel := s.handleInspectorCompose("\x03")
	if submit || !cancel {
		t.Fatalf("handleInspectorCompose(Ctrl-C) = (%v,%v), want (false,true)", submit, cancel)
	}
}

func TestHandleInspectorComposeIgnoresNonPrintableBytes(t *testing.T) {
	s := &tuiState{inspector: inspectorViewState{composing: true}}
	s.handleInspectorCompose("\x01")
	if s.inspector.composeText != "" {
		t.Fatalf("composeText = %q, want empty (control byte ignored)", s.inspector.composeText)
	}
}

// TestHandleInspectorComposeStopsAppendingAtMaxLen confirms the local compose
// box enforces the same sendKeysMaxLen cap the server's own validator does,
// so a long paste fails the same way locally as it would over the network
// instead of succeeding locally and then 400ing on submit.
func TestHandleInspectorComposeStopsAppendingAtMaxLen(t *testing.T) {
	s := &tuiState{inspector: inspectorViewState{
		composing:   true,
		composeText: strings.Repeat("x", sendKeysMaxLen),
	}}
	submit, cancel := s.handleInspectorCompose("y")
	if submit || cancel {
		t.Fatalf("handleInspectorCompose(append at cap) = (%v,%v), want (false,false)", submit, cancel)
	}
	if len(s.inspector.composeText) != sendKeysMaxLen {
		t.Fatalf("composeText len = %d, want %d (append past cap must be a no-op)", len(s.inspector.composeText), sendKeysMaxLen)
	}
}

// TestHandleInspectorComposeCancelClearsStaleStatus confirms Esc/Ctrl-C wipe
// a leftover composeStatus/composeStatusUntil from a prior failed send, so
// the footer doesn't keep showing a stale failure message after the user has
// already cancelled and moved on.
func TestHandleInspectorComposeCancelClearsStaleStatus(t *testing.T) {
	s := &tuiState{inspector: inspectorViewState{
		composing:          true,
		composeText:        "hello",
		composeStatus:      "send failed: broken pipe",
		composeStatusUntil: time.Now().Add(4 * time.Second),
	}}
	s.handleInspectorCompose(KeyEsc)
	if s.inspector.composeStatus != "" {
		t.Fatalf("composeStatus after Esc = %q, want empty", s.inspector.composeStatus)
	}
	if !s.inspector.composeStatusUntil.IsZero() {
		t.Fatalf("composeStatusUntil after Esc = %v, want zero", s.inspector.composeStatusUntil)
	}
}

// TestArmComposeArmsWithTmuxAndHintsWithout covers the shared helper both the
// 'i' keypress and the footer's clickable Compose control now route through.
func TestArmComposeArmsWithTmuxAndHintsWithout(t *testing.T) {
	s := &tuiState{inspector: inspectorViewState{
		snapshot: InspectorSnapshot{Session: Session{Tmux: "work:0.0"}},
	}}
	s.armCompose()
	if !s.inspector.composing {
		t.Fatal("armCompose with Tmux set: composing = false, want true")
	}

	s2 := &tuiState{inspector: inspectorViewState{
		snapshot: InspectorSnapshot{Session: Session{Tmux: ""}},
	}}
	s2.armCompose()
	if s2.inspector.composing {
		t.Fatal("armCompose with no Tmux: composing = true, want false")
	}
	if s2.inspector.composeStatus != "no tmux pane" {
		t.Fatalf("composeStatus = %q, want %q", s2.inspector.composeStatus, "no tmux pane")
	}
	if !s2.inspector.composeStatusUntil.After(time.Now()) {
		t.Fatal("composeStatusUntil not set to a future deadline")
	}
}

func TestInspectorMouseHandlers(t *testing.T) {
	s := newTUIState()
	s.inspector = newInspectorViewState("42")
	s.inspector.resize(3)
	s.inspector.applySnapshot(InspectorSnapshot{
		TargetID: "42",
		Lines:    []string{"1", "2", "3", "4", "5", "6"},
	})
	// top parked at 3 by follow mode.

	if cmd := s.handleInspectorMouse(mouseEvent{button: mouseWheelUp}); cmd != commandRender || s.inspector.top != 0 {
		t.Fatalf("wheel up: cmd=%v top=%d", cmd, s.inspector.top)
	}
	if cmd := s.handleInspectorMouse(mouseEvent{button: mouseWheelDown}); cmd != commandRender || s.inspector.top != 3 {
		t.Fatalf("wheel down: cmd=%v top=%d", cmd, s.inspector.top)
	}
	if cmd := s.handleInspectorMouse(mouseEvent{button: mouseLeft, release: true}); cmd != commandNone {
		t.Fatalf("release ignored: cmd=%v", cmd)
	}

	s.hits = []hitRegion{
		{x0: 0, y0: 0, x1: 4, y1: 0, action: hitInspectorBack},
		{x0: 6, y0: 0, x1: 12, y1: 0, action: hitInspectorRefresh},
		{x0: 14, y0: 0, x1: 20, y1: 0, action: hitInspectorFollow},
		{x0: 22, y0: 0, x1: 29, y1: 0, action: hitInspectorCompose},
	}
	if cmd := s.handleInspectorMouse(mouseEvent{x: 2, y: 0, button: mouseLeft}); cmd != commandBack {
		t.Fatalf("back button: cmd=%v", cmd)
	}
	if cmd := s.handleInspectorMouse(mouseEvent{x: 8, y: 0, button: mouseLeft}); cmd != commandRefreshInspector {
		t.Fatalf("refresh button: cmd=%v", cmd)
	}
	if cmd := s.handleInspectorMouse(mouseEvent{x: 16, y: 0, button: mouseLeft}); cmd != commandFollowInspector {
		t.Fatalf("follow button: cmd=%v", cmd)
	}
	if cmd := s.handleInspectorMouse(mouseEvent{x: 24, y: 0, button: mouseLeft}); cmd != commandComposeInspector {
		t.Fatalf("compose button: cmd=%v", cmd)
	}
	if cmd := s.handleInspectorMouse(mouseEvent{x: 40, y: 0, button: mouseLeft}); cmd != commandNone {
		t.Fatalf("click outside hit: cmd=%v", cmd)
	}
}

// listTestFrame builds a table frame with rowCount selectable rows after a
// single header line, plus the phantom trailing "" that BuildTableFrame's
// newline split produces. Row i sits on line i+1 with target ID "r<i>".
func listTestFrame(rowCount int) tableFrame {
	lines := []string{"HEADER"}
	rows := make([]tableRow, 0, rowCount)
	for i := 0; i < rowCount; i++ {
		id := fmt.Sprintf("r%d", i)
		rows = append(rows, tableRow{line: len(lines), targetID: id, openable: true})
		lines = append(lines, id)
	}
	lines = append(lines, "") // phantom trailing line
	return tableFrame{lines: lines, rows: rows}
}

func TestListWheelScrollFreeAndPreserved(t *testing.T) {
	frame := listTestFrame(20) // effLines 21, viewRows 10 -> maxOff 11
	viewRows := 10
	s := newTUIState()
	s.sel = "r0"

	// A selection change anchors r0 (line 1) into view: offset 0.
	s.anchorSelection = true
	s.resolveListOffset(frame, viewRows)
	if s.listOffset != 0 {
		t.Fatalf("anchor to r0: offset=%d want 0", s.listOffset)
	}

	// Wheel-down three times: free scroll, selection stays put, and the render
	// path does not drag the viewport back to the selection.
	for i := 0; i < 3; i++ {
		if cmd := s.handleListMouse(mouseEvent{button: mouseWheelDown}, time.Unix(0, 0)); cmd != commandRender {
			t.Fatalf("wheel %d cmd=%v", i, cmd)
		}
		s.resolveListOffset(frame, viewRows)
	}
	if s.sel != "r0" {
		t.Fatalf("wheel changed selection to %q", s.sel)
	}
	if s.anchorSelection {
		t.Fatal("wheel set anchorSelection")
	}
	if s.listOffset != 9 { // 3 wheels x 3 lines, below maxOff 11
		t.Fatalf("wheel offset=%d want 9", s.listOffset)
	}
	// r0 (line 1) is now above the viewport, and a plain re-render preserves it.
	s.resolveListOffset(frame, viewRows)
	if s.listOffset != 9 {
		t.Fatalf("re-render offset=%d want 9 (preserved)", s.listOffset)
	}
}

func TestListSelectionChangeReAnchors(t *testing.T) {
	frame := listTestFrame(20)
	viewRows := 10
	s := newTUIState()
	s.listOffset = 9 // scrolled away from the top by the wheel

	// Selecting the last row (as Down / click does) requests a re-anchor; the
	// next render scrolls it into view exactly once and clears the request.
	s.sel = "r19" // line 20
	s.anchorSelection = true
	s.resolveListOffset(frame, viewRows)

	if s.listOffset != 11 { // 20 - 10 + 1
		t.Fatalf("anchor to r19: offset=%d want 11", s.listOffset)
	}
	if s.anchorSelection {
		t.Fatal("anchorSelection not cleared after consume")
	}
	if line := frame.targetLine("r19"); line < s.listOffset || line >= s.listOffset+viewRows {
		t.Fatalf("r19 (line %d) not visible in [%d,%d)", line, s.listOffset, s.listOffset+viewRows)
	}
}

func TestListMouseWheelNoAnchorClickAnchors(t *testing.T) {
	s := newTUIState()
	s.hits = []hitRegion{{x0: 0, y0: 3, x1: 79, y1: 3,
		action: hitSelectSession, targetID: "r7", openable: true}}

	s.handleListMouse(mouseEvent{button: mouseWheelDown}, time.Unix(100, 0))
	s.handleListMouse(mouseEvent{button: mouseWheelUp}, time.Unix(100, 0))
	if s.anchorSelection {
		t.Fatal("wheel set anchorSelection")
	}

	if cmd := s.handleListMouse(mouseEvent{x: 5, y: 3, button: mouseLeft}, time.Unix(100, 0)); cmd != commandRender {
		t.Fatalf("click cmd=%v", cmd)
	}
	if s.sel != "r7" || !s.anchorSelection {
		t.Fatalf("click did not select+anchor: sel=%q anchor=%v", s.sel, s.anchorSelection)
	}
}

func TestSettleSelectionPendingSpawnSettlesWhenTargetAppears(t *testing.T) {
	s := newTUIState()
	s.sel = "11"
	s.pending = &pendingSpawn{host: "", tmux: "spawn-abc"}

	// Phase 1: the spawned tmux session has not surfaced yet and the old row is
	// still present, so the selection and the pending intent are both retained
	// and nothing is anchored.
	absent := buildSelectionTargets([]Session{{PID: 11, CWD: "/tmp/a"}}, nil)
	s.settleSelection(absent)
	if s.sel != "11" {
		t.Fatalf("phase1 sel = %q, want 11 (retained)", s.sel)
	}
	if s.pending == nil {
		t.Fatal("phase1 cleared pending while target absent")
	}
	if s.anchorSelection {
		t.Fatal("phase1 anchored without a selection change")
	}

	// Phase 2: the new session shows up with a matching tmux pane; selection
	// jumps to it, anchors once, and the pending intent clears.
	present := buildSelectionTargets([]Session{
		{PID: 11, CWD: "/tmp/a"},
		{PID: 22, CWD: "/tmp/b", Tmux: "spawn-abc:0.0"},
	}, nil)
	s.settleSelection(present)
	if s.sel != "22" {
		t.Fatalf("phase2 sel = %q, want 22", s.sel)
	}
	if s.pending != nil {
		t.Fatal("phase2 did not clear pending after settling")
	}
	if !s.anchorSelection {
		t.Fatal("phase2 did not anchor the new selection")
	}
}

func TestSettleSelectionRemoteHostMatch(t *testing.T) {
	s := newTUIState()
	s.sel = "11"
	s.pending = &pendingSpawn{host: "dev", tmux: "spawn-xyz"}

	// A local session shares the tmux name but not the host, so it must NOT
	// match; only the remote session on host "dev" is a valid landing spot.
	targets := buildSelectionTargets(
		[]Session{{PID: 11, CWD: "/tmp/a", Tmux: "spawn-xyz:0.0"}},
		[]RemoteResult{{Name: "dev", Sessions: []Session{
			{PID: 5, CWD: "/tmp/r", Tmux: "spawn-xyz:0.0", Host: "dev"},
		}}},
	)
	s.settleSelection(targets)
	if s.sel != "dev:5" {
		t.Fatalf("sel = %q, want dev:5 (remote host match, not the local tmux twin)", s.sel)
	}
	if s.pending != nil {
		t.Fatal("did not clear pending after remote match")
	}
	if !s.anchorSelection {
		t.Fatal("did not anchor the remote selection")
	}
}

func TestSettleSelectionRetainsPendingWithFallbackWhenOldRowVanishes(t *testing.T) {
	s := newTUIState()
	s.sel = "99" // a row that is about to vanish
	s.pending = &pendingSpawn{host: "", tmux: "spawn-def"}

	// The pending tmux session has not surfaced and the previously-selected row
	// is gone: validateTargetSel falls back to the first row (anchoring the
	// change) while the pending intent survives for a later refresh.
	targets := buildSelectionTargets([]Session{{PID: 11, CWD: "/tmp/a"}}, nil)
	s.settleSelection(targets)
	if s.sel != "11" {
		t.Fatalf("sel = %q, want 11 (validateTargetSel fallback)", s.sel)
	}
	if s.pending == nil {
		t.Fatal("cleared pending while the spawn target was still absent")
	}
	if !s.anchorSelection {
		t.Fatal("fallback did not anchor the changed selection")
	}
}

func TestNavigateCancelsPending(t *testing.T) {
	targets := buildSelectionTargets([]Session{
		{PID: 11, CWD: "/tmp/a"},
		{PID: 22, CWD: "/tmp/b"},
	}, nil)
	s := newTUIState()
	s.sel = "11"
	s.pending = &pendingSpawn{host: "", tmux: "spawn-abc"}

	s.navigate(targets, 1)
	if s.sel != "22" {
		t.Fatalf("navigate sel = %q, want 22", s.sel)
	}
	if s.pending != nil {
		t.Fatal("navigate did not cancel pending intent")
	}
	if !s.anchorSelection {
		t.Fatal("navigate did not anchor the new selection")
	}
}

func TestListMouseRowSelectCancelsPendingButWheelDoesNot(t *testing.T) {
	now := time.Unix(100, 0)
	s := newTUIState()
	s.hits = []hitRegion{{x0: 0, y0: 3, x1: 79, y1: 3,
		action: hitSelectSession, targetID: "r7", openable: true}}

	// Wheel scroll is free scrolling, not a selection change: pending survives.
	s.pending = &pendingSpawn{host: "", tmux: "spawn-abc"}
	s.handleListMouse(mouseEvent{button: mouseWheelDown}, now)
	if s.pending == nil {
		t.Fatal("wheel scroll cancelled pending")
	}

	// A left click that selects a row is explicit navigation: pending clears.
	s.handleListMouse(mouseEvent{x: 5, y: 3, button: mouseLeft}, now)
	if s.sel != "r7" {
		t.Fatalf("click sel = %q, want r7", s.sel)
	}
	if s.pending != nil {
		t.Fatal("row click did not cancel pending")
	}

	// A click on empty space hits nothing and must not cancel pending.
	s.pending = &pendingSpawn{host: "", tmux: "spawn-abc"}
	s.handleListMouse(mouseEvent{x: 5, y: 40, button: mouseLeft}, now)
	if s.pending == nil {
		t.Fatal("click on empty space cancelled pending")
	}
}

func TestEnsureLineVisible(t *testing.T) {
	cases := []struct {
		name                          string
		offset, line, viewport, total int
		want                          int
	}{
		{"already visible", 5, 7, 10, 100, 5},
		{"above scrolls up to line", 5, 2, 10, 100, 2},
		{"below scrolls to last visible", 0, 15, 10, 100, 6},
		{"clamped at max offset", 0, 99, 10, 100, 90},
		{"small total floors at zero", 3, 0, 10, 5, 0},
		{"zero viewport is a no-op", 4, 99, 0, 100, 4},
	}
	for _, c := range cases {
		if got := ensureLineVisible(c.offset, c.line, c.viewport, c.total); got != c.want {
			t.Errorf("%s: ensureLineVisible(%d,%d,%d,%d) = %d, want %d",
				c.name, c.offset, c.line, c.viewport, c.total, got, c.want)
		}
	}
}

func TestRequestSelectionAnchorRevealsToggledRowAfterSortMove(t *testing.T) {
	state := newTUIState()
	state.sel = "2"
	state.listOffset = 0

	frame := tableFrame{
		lines: []string{"header", "one", "three", "two", ""},
		rows: []tableRow{
			{line: 1, targetID: "1"},
			{line: 2, targetID: "3"},
			{line: 3, targetID: "2"},
		},
	}
	state.requestSelectionAnchor()
	state.resolveListOffset(frame, 2)

	line := frame.targetLine(state.sel)
	if line < state.listOffset || line >= state.listOffset+2 {
		t.Fatalf(
			"selected line %d not visible in offset %d viewport",
			line,
			state.listOffset,
		)
	}
}

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

func TestSettleSelectionKeepsToggledSessionAfterDisabledSortMove(t *testing.T) {
	rows := []Session{
		{PID: 1, SessionID: "one", CWD: "/alpha"},
		{PID: 2, SessionID: "two", CWD: "/beta"},
		{PID: 3, SessionID: "three", CWD: "/gamma"},
	}
	SortSessions(rows, "dir")

	state := newTUIState()
	state.sel = "2"
	state.settleSelection(buildSelectionTargets(rows, nil))

	rows[1].Disabled = true
	SortSessions(rows, "dir")
	if rows[2].SessionID != "two" {
		t.Fatalf("disabled row index = %v, want session two last", rows)
	}
	state.settleSelection(buildSelectionTargets(rows, nil))
	if state.sel != "2" {
		t.Fatalf("selection = %q, want toggled session 2", state.sel)
	}
}
