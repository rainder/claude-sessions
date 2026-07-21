package main

import (
	"testing"
	"time"
)

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
	if cmd := s.handleInspectorMouse(mouseEvent{x: 40, y: 0, button: mouseLeft}); cmd != commandNone {
		t.Fatalf("click outside hit: cmd=%v", cmd)
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
