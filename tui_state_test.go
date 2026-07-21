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
