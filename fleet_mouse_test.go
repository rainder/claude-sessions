package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// fleetScreenRow is the screen row of the first rendered line holding s, -1
// when none does.
func fleetScreenRow(out, s string) int {
	return fleetRowOf(strings.Split(out, "\n"), s)
}

func fleetWheel(y int, down bool) inputEvent {
	b := mouseWheelUp
	if down {
		b = mouseWheelDown
	}
	return inputEvent{kind: eventMouse, mouse: mouseEvent{x: 5, y: y, button: b}}
}

func fleetClick(y int) inputEvent {
	return inputEvent{kind: eventMouse, mouse: mouseEvent{x: 5, y: y, button: mouseLeft}}
}

func fleetItemIndexT(t *testing.T, st *fleetViewState, id string) int {
	t.Helper()
	i := fleetItemIndex(st.items, id)
	if i < 0 {
		t.Fatalf("no item %q", id)
	}
	return i
}

// fleetTallView is a short card "a" and a card "tall" whose summary is taller
// than the panel.
func fleetTallView() fleetStateView {
	c := fleetTestCard("tall", "needs-you")
	c.Ticket = "DR-7000"
	for i := 0; i < 40; i++ {
		c.Summary += fmt.Sprintf("line %02d\n", i)
	}
	return fleetTestView(c, fleetTestCard("a", "needs-you"))
}

func TestFleetMouseWheelScrollsPanel(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	st := fleetTestState(fleetTallView(), now)
	st.sel = fleetItemIndexT(t, st, "tall")
	out := fleetRendered(st, 80, 15, now)
	y := fleetScreenRow(out, "line 00")
	if y < 0 {
		t.Fatalf("no panel line:\n%s", out)
	}
	sel := st.sel
	if cmd := st.handleEvent(fleetWheel(y, true), false, now); cmd != fleetCmdRender || st.scroll != 3 || st.sel != sel {
		t.Fatalf("wheel down: cmd = %v scroll = %d sel = %d", cmd, st.scroll, st.sel)
	}
	st.handleEvent(fleetWheel(y, false), false, now)
	st.handleEvent(fleetWheel(y, false), false, now)
	if st.scroll != 0 {
		t.Fatalf("scroll = %d after wheeling up past the top", st.scroll)
	}
	for i := 0; i < 50; i++ {
		st.handleEvent(fleetWheel(y, true), false, now)
	}
	bottom := fleetRendered(st, 80, 15, now)
	bottomScroll := st.scroll
	if !strings.Contains(bottom, "2  not-yet") || strings.Contains(bottom, "↓ more") {
		t.Fatalf("wheel did not reach the bottom:\n%s", bottom)
	}
	st.handleEvent(fleetWheel(y, true), false, now)
	fleetRendered(st, 80, 15, now)
	if st.scroll != bottomScroll {
		t.Fatalf("scroll = %d, want clamped %d", st.scroll, bottomScroll)
	}
	st.handleEvent(fleetWheel(y, false), false, now)
	if st.scroll != bottomScroll-3 {
		t.Fatalf("wheel up after the bottom: scroll = %d, want %d", st.scroll, bottomScroll-3)
	}
	if up := fleetRendered(st, 80, 15, now); !strings.Contains(up, "↓ more") {
		t.Fatalf("no ↓ more after wheel up:\n%s", up)
	}
}

func TestFleetMouseWheelOverListMovesSelection(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	st := fleetTestState(fleetRenderView(now), now)
	out := fleetRendered(st, 100, 20, now)
	row := fleetScreenRow(out, "Merge PR #2786")
	head := fleetScreenRow(out, stripANSI(fleetSectionTitles[st.items[0].Section]))
	if row < 0 || head < 0 {
		t.Fatalf("no list rows:\n%s", out)
	}
	if st.sel != 0 {
		t.Fatalf("sel = %d", st.sel)
	}
	if cmd := st.handleEvent(fleetWheel(row, false), false, now); cmd != fleetCmdRender || st.sel != 0 {
		t.Fatalf("wheel up at the first card: cmd = %v sel = %d", cmd, st.sel)
	}
	st.scroll = 6
	if st.handleEvent(fleetWheel(row, true), false, now); st.sel != 1 || st.scroll != 0 {
		t.Fatalf("wheel down: sel = %d scroll = %d", st.sel, st.scroll)
	}
	// A heading is part of the list area.
	fleetRendered(st, 100, 20, now)
	if st.handleEvent(fleetWheel(head, true), false, now); st.sel != 2 {
		t.Fatalf("wheel down over a heading: sel = %d", st.sel)
	}
	fleetRendered(st, 100, 20, now)
	if st.handleEvent(fleetWheel(row, true), false, now); st.sel != 2 {
		t.Fatalf("wheel down at the last card: sel = %d", st.sel)
	}
}

func TestFleetMouseClickSelectsCard(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	st := fleetTestState(fleetRenderView(now), now)
	out := fleetRendered(st, 100, 20, now)
	st.scroll = 4
	y := fleetScreenRow(out, "Retry policy")
	cmd := st.handleEvent(fleetClick(y), false, now)
	if want := fleetItemIndexT(t, st, "b"); cmd != fleetCmdRender || st.sel != want || st.scroll != 0 {
		t.Fatalf("click: cmd = %v sel = %d (want %d) scroll = %d", cmd, st.sel, want, st.scroll)
	}
	if st.panel != nil || st.send != nil || st.sending {
		t.Fatal("a click opened or sent something")
	}
	fleetRendered(st, 100, 20, now)
	if st.handleEvent(fleetClick(fleetScreenRow(out, "You chose")), false, now); st.sel != fleetItemIndexT(t, st, "c") {
		t.Fatalf("click on the SENT row: sel = %d", st.sel)
	}
}

func TestFleetMouseClickOffCardRowsDoesNothing(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	st := fleetTestState(fleetRenderView(now), now)
	out := fleetRendered(st, 100, 20, now)
	lines := strings.Split(out, "\n")
	ruleRow := -1
	for i := 2; i < len(lines)-2; i++ {
		if strings.HasPrefix(lines[i], "────") {
			ruleRow = i
			break
		}
	}
	panelRow := fleetScreenRow(out, "CI is green.")
	rows := map[string]int{
		"header":  0,
		"top sep": 1,
		"heading": fleetScreenRow(out, stripANSI(fleetSectionTitles[st.items[0].Section])),
		"rule":    ruleRow,
		"panel":   panelRow,
		"footer":  len(lines) - 1,
		"bottom":  len(lines) - 2,
		"outside": len(lines) + 3,
	}
	for name, y := range rows {
		if y < 0 {
			t.Fatalf("%s row not found:\n%s", name, out)
		}
		if cmd := st.handleEvent(fleetClick(y), false, now); cmd != fleetCmdNone || st.sel != 0 {
			t.Errorf("click on %s (row %d): cmd = %v sel = %d", name, y, cmd, st.sel)
		}
	}
	// Only the left button selects.
	y := fleetScreenRow(out, "Retry policy")
	for _, b := range []mouseButton{mouseMiddle, mouseRight} {
		ev := inputEvent{kind: eventMouse, mouse: mouseEvent{x: 5, y: y, button: b}}
		if cmd := st.handleEvent(ev, false, now); cmd != fleetCmdNone || st.sel != 0 {
			t.Errorf("button %d: cmd = %v sel = %d", b, cmd, st.sel)
		}
	}
	// Wheel over the header or footer does nothing either.
	for _, yy := range []int{0, len(lines) - 1} {
		if cmd := st.handleEvent(fleetWheel(yy, true), false, now); cmd != fleetCmdNone || st.sel != 0 || st.scroll != 0 {
			t.Errorf("wheel on row %d: cmd = %v sel = %d scroll = %d", yy, cmd, st.sel, st.scroll)
		}
	}
}

func TestFleetMouseReleaseIgnored(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	st := fleetTestState(fleetRenderView(now), now)
	out := fleetRendered(st, 100, 20, now)
	ev := fleetClick(fleetScreenRow(out, "Retry policy"))
	ev.mouse.release = true
	if cmd := st.handleEvent(ev, false, now); cmd != fleetCmdNone || st.sel != 0 {
		t.Fatalf("release: cmd = %v sel = %d", cmd, st.sel)
	}
}

func TestFleetMousePanelOpenOwnsInput(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	st := fleetTestState(fleetRenderView(now), now)
	out := fleetRendered(st, 100, 20, now)
	other := fleetScreenRow(out, "Retry policy")
	st.handleKey("1", false, now)
	if st.panel == nil {
		t.Fatal("panel did not open")
	}
	panelOut := fleetRendered(st, 100, 20, now)
	for name, ev := range map[string]inputEvent{
		"click list":  fleetClick(other),
		"wheel list":  fleetWheel(other, true),
		"wheel panel": fleetWheel(fleetScreenRow(panelOut, "FLEET receives"), true),
		"click panel": fleetClick(fleetScreenRow(panelOut, "Enter send")),
	} {
		if cmd := st.handleEvent(ev, false, now); cmd != fleetCmdNone || st.sel != 0 || st.panel == nil || st.scroll != 0 || st.send != nil {
			t.Errorf("%s: cmd = %v sel = %d panel = %v scroll = %d", name, cmd, st.sel, st.panel != nil, st.scroll)
		}
	}
	if !st.panel.openedAt.Equal(now) {
		t.Fatal("a mouse event moved the arm delay")
	}
}

func TestFleetMouseShipAndErrorBodiesIgnored(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	v := fleetTestView(fleetTestCard("a", "needs-you"))
	v.Answered["a"] = fleetAnswerWith(fleetAttemptSent)
	st := fleetTestState(v, now)
	if !st.shipOn() {
		t.Fatal("ship screen is not on")
	}
	out := fleetRendered(st, 100, 20, now)
	for y := range strings.Split(out, "\n") {
		for _, ev := range []inputEvent{fleetClick(y), fleetWheel(y, true), fleetWheel(y, false)} {
			if cmd := st.handleEvent(ev, false, now); cmd != fleetCmdNone || st.listMode || st.sel != 0 || st.scroll != 0 {
				t.Fatalf("ship row %d: cmd = %v", y, cmd)
			}
		}
	}

	st = fleetTestState(fleetRenderView(now), now)
	fleetRendered(st, 100, 20, now)
	st.applyState(fleetStateView{}, errFleetUnauthorized, now)
	out = fleetRendered(st, 100, 20, now)
	for y := range strings.Split(out, "\n") {
		if cmd := st.handleEvent(fleetWheel(y, true), false, now); cmd != fleetCmdNone || st.sel != 0 {
			t.Fatalf("error row %d: cmd = %v sel = %d", y, cmd, st.sel)
		}
	}
	amb := fleetRenderView(now)
	amb.Ambiguous = []string{"ws", "pi"}
	st = fleetTestState(amb, now)
	out = fleetRendered(st, 100, 20, now)
	for y := range strings.Split(out, "\n") {
		if cmd := st.handleEvent(fleetClick(y), false, now); cmd != fleetCmdNone {
			t.Fatalf("ambiguous row %d: cmd = %v", y, cmd)
		}
	}
}

// A state applied after the last draw moves the rows, so the map the draw
// left must not be read until the next one.
func TestFleetMouseIgnoredUntilRedrawAfterState(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	st := fleetTestState(fleetRenderView(now), now)
	out := fleetRendered(st, 100, 20, now)
	y := fleetScreenRow(out, "Retry policy")
	st.applyState(fleetRenderView(now), nil, now)
	if cmd := st.handleEvent(fleetClick(y), false, now); cmd != fleetCmdNone || st.sel != 0 {
		t.Fatalf("stale click: cmd = %v sel = %d", cmd, st.sel)
	}
	fleetRendered(st, 100, 20, now)
	if st.handleEvent(fleetClick(y), false, now); st.sel != fleetItemIndexT(t, st, "b") {
		t.Fatalf("click after redraw: sel = %d", st.sel)
	}
}

func TestFleetMouseGeometryAfterListScroll(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	var cards []fleetCardView
	for i := 0; i < 30; i++ {
		c := fleetTestCard(fmt.Sprintf("c%02d", i), "needs-you")
		c.Ticket = fmt.Sprintf("DR-%d", 6000+i)
		cards = append(cards, c)
	}
	st := fleetTestState(fleetTestView(cards...), now)
	for i := 0; i < 29; i++ {
		st.handleKey(KeyDown, false, now)
	}
	out := fleetRendered(st, 80, 15, now)
	if st.listTop == 0 {
		t.Fatalf("list window did not move:\n%s", out)
	}
	y := fleetScreenRow(out, "DR-6027")
	if y < 0 {
		t.Fatalf("DR-6027 not shown:\n%s", out)
	}
	if st.handleEvent(fleetClick(y), false, now); st.sel != fleetItemIndexT(t, st, "c27") {
		t.Fatalf("click on DR-6027: sel = %d", st.sel)
	}
	out = fleetRendered(st, 80, 15, now)
	if !strings.Contains(out, "▸ DR-6027") {
		t.Fatalf("clicked card not selected on screen:\n%s", out)
	}
	if st.handleEvent(fleetWheel(fleetScreenRow(out, "DR-6027"), false), false, now); st.sel != fleetItemIndexT(t, st, "c26") {
		t.Fatalf("wheel up: sel = %d", st.sel)
	}
}

// A mouse event in the same read as Enter does not make the read a paste.
func TestFleetMouseNotInBatch(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	st := fleetOpenPanel(t, now)
	fleetRendered(st, 100, 20, now)
	later := now.Add(time.Second)
	events := []inputEvent{fleetWheel(5, true), fleetClick(3), {kind: eventKey, key: KeyEnter}}
	inBatch := fleetKeyBatch(events)
	if inBatch {
		t.Fatal("two mouse events and one key count as a batch")
	}
	var last fleetCmd
	for _, ev := range events {
		last = st.handleEvent(ev, inBatch, later)
	}
	if last != fleetCmdSend || st.send == nil {
		t.Fatalf("Enter after mouse events: cmd = %v send = %v", last, st.send)
	}
	if !fleetKeyBatch([]inputEvent{{kind: eventKey, key: "a"}, fleetClick(1), {kind: eventKey, key: KeyEnter}}) {
		t.Fatal("two keys are a batch whatever lies between them")
	}
}

// At an unknown size the frame shows everything: the panel ends where the
// footer starts, and a click still maps to its card row.
func TestFleetMouseUnknownSize(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	st := fleetTestState(fleetTallView(), now)
	out := fleetRendered(st, 80, 0, now)
	lines := strings.Split(out, "\n")
	for _, y := range []int{len(lines) - 1, len(lines) - 2} {
		if cmd := st.handleEvent(fleetWheel(y, true), false, now); cmd != fleetCmdNone || st.scroll != 0 {
			t.Fatalf("wheel on footer row %d: cmd = %v scroll = %d", y, cmd, st.scroll)
		}
	}
	if cmd := st.handleEvent(fleetWheel(fleetScreenRow(out, "line 05"), true), false, now); cmd != fleetCmdRender {
		t.Fatalf("wheel on the panel: cmd = %v", cmd)
	}
	y := fleetScreenRow(out, "card a")
	if st.handleEvent(fleetClick(y), false, now); st.sel != fleetItemIndexT(t, st, "a") {
		t.Fatalf("click: sel = %d", st.sel)
	}
}
