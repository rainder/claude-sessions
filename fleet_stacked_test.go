package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// fleetStackedView has cards of very different heights: a one-line card, a
// card with a long summary, one with nine options, an id-less one and a SENT
// one.
func fleetStackedView(now time.Time) fleetStateView {
	short := fleetTestCard("s", "explicit-yes")
	short.Ticket, short.Title = "DR-100", "Short card"
	long := fleetTestCard("l", "needs-you")
	long.Ticket, long.Title = "DR-200", "Long summary card"
	for i := 0; i < 30; i++ {
		long.Summary += fmt.Sprintf("summary line %02d\n", i)
	}
	long.WaitingOn, long.Session = "Andy", "dr-200"
	many := fleetTestCard("m", "needs-you")
	many.Ticket, many.Title = "DR-300", "Nine options card"
	many.Options = nil
	for i := 1; i <= 9; i++ {
		many.Options = append(many.Options, fleetOptionView{Key: fmt.Sprintf("k%d", i), Label: fmt.Sprintf("Label %d", i),
			Line: "what this option does", Send: "s"})
	}
	many.Lean = "k4"
	bare := fleetTestCard("", "needs-you")
	bare.Ticket, bare.Title, bare.Options = "DR-400", "Chat only card", nil
	sent := fleetTestCard("x", "needs-you")
	sent.Ticket, sent.Title = "DR-500", "Already sent card"
	v := fleetTestView(short, long, many, bare, sent)
	v.Fleet.Status = "idle"
	v.Answered["x"] = fleetAnswer{CardHash: "h-x", Attempts: []fleetAttempt{{OptionKey: "yes", Label: "Merge now",
		At: now.Add(-3 * time.Minute).UTC().Format(time.RFC3339), Status: fleetAttemptSent}}}
	return v
}

// fleetRuleRow is the index of the rule between the list and the panel: the
// first line after the header's own rule that starts with ─.
func fleetRuleRow(out string) int {
	for i, l := range strings.Split(out, "\n") {
		if i > 1 && strings.HasPrefix(l, "─") {
			return i
		}
	}
	return -1
}

// fleetCheckFrame fails when a frame is not exactly rows lines or a line is
// wider than cols.
func fleetCheckFrame(t *testing.T, raw string, cols, rows int) {
	t.Helper()
	lines := strings.Split(raw, "\n")
	if len(lines) != rows {
		t.Errorf("%dx%d: frame has %d lines:\n%s", cols, rows, len(lines), stripANSI(raw))
	}
	for _, l := range lines {
		if w := visualLen(l); w > cols {
			t.Errorf("%dx%d: line is %d wide: %q", cols, rows, w, stripANSI(l))
		}
	}
}

// The property the layout exists for: the panel starts on the same screen row
// whichever card is selected.
func TestRenderFleetViewPanelRowFixed(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	for _, size := range [][2]int{{100, 30}, {60, 18}, {80, 12}, {40, 50}} {
		cols, rows := size[0], size[1]
		st := fleetTestState(fleetStackedView(now), now)
		want := -1
		for sel := 0; sel < len(st.items); sel++ {
			st.sel, st.scroll = sel, 0
			raw := renderFleetView(st, cols, rows, now)
			fleetCheckFrame(t, raw, cols, rows)
			out := stripANSI(raw)
			rule := fleetRuleRow(out)
			if want < 0 {
				want = rule
			}
			if rule != want || rule < 0 {
				t.Errorf("%dx%d sel %d: panel rule at row %d, want %d:\n%s", cols, rows, sel, rule, want, out)
			}
			lines := strings.Split(out, "\n")
			title := st.items[sel].Card.Title
			if rule+1 >= len(lines) || !strings.Contains(lines[rule+1], title) {
				t.Errorf("%dx%d sel %d: panel does not start with %q:\n%s", cols, rows, sel, title, out)
			}
			// One list line per card: the selected row is a single line.
			marked := 0
			for _, l := range lines[:rule] {
				if strings.HasPrefix(l, "▸ ") {
					marked++
				}
			}
			if marked != 1 {
				t.Errorf("%dx%d sel %d: %d selected rows in the list:\n%s", cols, rows, sel, marked, out)
			}
		}
	}
}

func TestFleetStackedLayout(t *testing.T) {
	cases := []struct {
		body, need    int
		confirm       bool
		listH, panelH int
		rule          bool
	}{
		{26, 8, false, 8, 17, true},   // all rows fit in 40%
		{26, 30, false, 10, 15, true}, // 40% of 26 is 10.4, floored
		{14, 30, false, 5, 8, true},
		{8, 30, false, 3, 4, true}, // 40% of 8 is 3.2
		{6, 30, false, 3, 2, true},
		{4, 30, false, 2, 1, true}, // tiny: the list gives way so the panel keeps a row
		{3, 30, false, 1, 1, true},
		{2, 30, false, 1, 1, false},
		{1, 30, false, 1, 0, false},
		{0, 30, false, 0, 0, false},
		{4, 30, true, 1, 2, true}, // the confirm panel keeps its comment and key lines
		{2, 30, true, 0, 2, false},
		{26, 30, true, 10, 15, true}, // a normal screen: same list as without the panel
		{26, 1, false, 1, 24, true},
	}
	for _, c := range cases {
		listH, rule, panelH := fleetStackedLayout(c.body, c.need, c.confirm)
		if listH != c.listH || rule != c.rule || panelH != c.panelH {
			t.Errorf("fleetStackedLayout(%d, %d, %v) = %d, %v, %d; want %d, %v, %d",
				c.body, c.need, c.confirm, listH, rule, panelH, c.listH, c.rule, c.panelH)
		}
		if c.body >= 0 {
			used := listH + panelH
			if rule {
				used++
			}
			if used != c.body {
				t.Errorf("fleetStackedLayout(%d, %d, %v) uses %d rows", c.body, c.need, c.confirm, used)
			}
		}
	}
}

func TestFleetListWindow(t *testing.T) {
	cases := []struct {
		top, sel, need, h int
		headAbove         bool
		want              int
	}{
		{0, 3, 20, 5, false, 0},  // inside: no move
		{0, 4, 20, 5, false, 0},  // last visible row
		{0, 5, 20, 5, false, 1},  // one below: move by one
		{10, 6, 20, 5, false, 6}, // above: selected row at the top
		{10, 6, 20, 5, true, 5},  // above, first of its section: its heading too
		{10, 1, 20, 1, true, 1},  // a one-row window has no room for the heading
		{18, 19, 20, 5, false, 15},
		{3, 2, 4, 5, false, 0}, // everything fits
		{0, 0, 3, 0, false, 0},
	}
	for _, c := range cases {
		if got := fleetListWindow(c.top, c.sel, c.need, c.h, c.headAbove); got != c.want {
			t.Errorf("fleetListWindow(top %d, sel %d, need %d, h %d, head %v) = %d, want %d",
				c.top, c.sel, c.need, c.h, c.headAbove, got, c.want)
		}
	}
}

func TestRenderFleetViewListScrolls(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	var cards []fleetCardView
	for i := 0; i < 30; i++ {
		c := fleetTestCard(fmt.Sprintf("c%02d", i), "needs-you")
		c.Ticket = fmt.Sprintf("DR-%d", 6000+i)
		cards = append(cards, c)
	}
	st := fleetTestState(fleetTestView(cards...), now)
	const cols, rows = 80, 20 // body 16: list 6 rows
	first := strings.Split(fleetRendered(st, cols, rows, now), "\n")[2]
	wantRule := -1
	for i := 0; i < 30; i++ {
		raw := renderFleetView(st, cols, rows, now)
		fleetCheckFrame(t, raw, cols, rows)
		out := stripANSI(raw)
		lines := strings.Split(out, "\n")
		rule := fleetRuleRow(out)
		if wantRule < 0 {
			wantRule = rule
		}
		if rule != wantRule {
			t.Fatalf("sel %d: rule at %d, want %d", i, rule, wantRule)
		}
		ticket := fmt.Sprintf("▸ DR-%d", 6000+i)
		found := false
		for _, l := range lines[2:rule] {
			found = found || strings.HasPrefix(l, ticket)
		}
		if !found {
			t.Fatalf("sel %d: selected row is outside the list window:\n%s", i, out)
		}
		// The window moves only once the selection leaves it: the heading
		// and first five cards fill the six rows, so it stays put to card 4.
		if i <= 4 && lines[2] != first {
			t.Errorf("sel %d: list moved while the selection was visible:\n%s", i, out)
		}
		st.handleKey(KeyDown, false, now)
	}
	// Back up: the window follows only once the selection leaves its top.
	st.sel = 29
	_ = renderFleetView(st, cols, rows, now)
	top := st.listTop
	st.handleKey(KeyUp, false, now)
	_ = renderFleetView(st, cols, rows, now)
	if st.listTop != top {
		t.Errorf("Up inside the window moved it: %d → %d", top, st.listTop)
	}
	if out := fleetRendered(st, cols, rows, now); !strings.Contains(out, "↑") {
		t.Errorf("a list scrolled down does not say so on its rule:\n%s", out)
	}
}

func TestRenderFleetViewPanelScroll(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	st := fleetTestState(fleetStackedView(now), now)
	st.sel = 1 // the long summary card
	const cols, rows = 80, 18
	top := fleetRendered(st, cols, rows, now)
	if !strings.Contains(top, "summary line 00") || !strings.Contains(top, "↓ more") || strings.Contains(top, "↑ more") {
		t.Errorf("panel top:\n%s", top)
	}
	lines := strings.Split(top, "\n")
	if l := lines[rows-3]; strings.TrimSpace(l) != "↓ more" {
		t.Errorf("last panel line is %q, want ↓ more:\n%s", l, top)
	}
	st.handleKey(KeyPageDown, false, now)
	mid := fleetRendered(st, cols, rows, now)
	if !strings.Contains(mid, "↑ more") || strings.Contains(mid, "summary line 00") {
		t.Errorf("panel after PgDn:\n%s", mid)
	}
	// No line is skipped by a page: the last line shown before PgDn is still
	// on screen after it.
	lastShown := strings.TrimSpace(lines[rows-4])
	if !strings.Contains(mid, lastShown) {
		t.Errorf("PgDn skipped %q:\n%s", lastShown, mid)
	}
	for i := 0; i < 20; i++ {
		st.handleKey(KeyPageDown, false, now)
	}
	bottom := fleetRendered(st, cols, rows, now)
	if !strings.Contains(bottom, "Other — write your own answer") || strings.Contains(bottom, "↓ more") {
		t.Errorf("panel bottom:\n%s", bottom)
	}
	// The list does not move while the panel scrolls.
	rule := fleetRuleRow(top)
	tl, bl := strings.Split(top, "\n"), strings.Split(bottom, "\n")
	for i := 0; i <= rule; i++ {
		if tl[i] != bl[i] {
			t.Errorf("row %d moved with the panel scroll: %q → %q", i, tl[i], bl[i])
		}
	}
	// PgUp after over-scrolling moves at once.
	st.handleKey(KeyPageUp, false, now)
	if up := fleetRendered(st, cols, rows, now); up == bottom {
		t.Error("PgUp after over-scrolling does nothing")
	}
	// A new selection starts its panel at the top.
	st.handleKey(KeyDown, false, now)
	if st.scroll != 0 {
		t.Fatalf("scroll = %d after Down", st.scroll)
	}
	st.handleKey(KeyUp, false, now)
	if again := fleetRendered(st, cols, rows, now); again != top {
		t.Errorf("back on the long card, the panel is not at its top:\n%s", again)
	}
}

// The confirm and Other panels draw in the panel area; the list stays.
func TestRenderFleetViewConfirmInPanelArea(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	for _, key := range []string{"1", "o"} {
		for _, size := range [][2]int{{100, 30}, {60, 18}} {
			cols, rows := size[0], size[1]
			st := fleetTestState(fleetStackedView(now), now)
			before := fleetRendered(st, cols, rows, now)
			st.handleKey(key, false, now)
			if st.panel == nil {
				t.Fatalf("key %s: no panel", key)
			}
			raw := renderFleetView(st, cols, rows, now.Add(time.Second))
			fleetCheckFrame(t, raw, cols, rows)
			after := stripANSI(raw)
			rule := fleetRuleRow(before)
			if r := fleetRuleRow(after); r != rule {
				t.Errorf("key %s %dx%d: rule moved %d → %d", key, cols, rows, rule, r)
			}
			bl, al := strings.Split(before, "\n"), strings.Split(after, "\n")
			for i := 0; i <= rule; i++ {
				if bl[i] != al[i] {
					t.Errorf("key %s %dx%d: list row %d changed: %q → %q", key, cols, rows, i, bl[i], al[i])
				}
			}
			panel := strings.Join(al[rule+1:], "\n")
			for _, want := range []string{"Approve this write?", "FLEET receives:", "Enter send"} {
				if strings.Contains(want, "Approve") && key == "o" {
					want = "Send this to FLEET?"
				}
				if !strings.Contains(panel, want) {
					t.Errorf("key %s %dx%d: panel area lacks %q:\n%s", key, cols, rows, want, after)
				}
			}
		}
	}
}

func TestRenderFleetViewTiny(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	for _, rows := range []int{1, 2, 3, 4, 5, 6, 7, 8, 12, 16} {
		for _, cols := range []int{10, 30, 60} {
			for _, panel := range []bool{false, true} {
				st := fleetTestState(fleetStackedView(now), now)
				st.sel = 1
				if panel {
					st.handleKey("1", false, now)
				}
				for i := 0; i < 3; i++ {
					st.handleKey(KeyPageDown, false, now)
				}
				raw := renderFleetView(st, cols, rows, now.Add(time.Second))
				fleetCheckFrame(t, raw, cols, rows)
				out := stripANSI(raw)
				if rows >= 5 && !panel && !strings.Contains(out, "▸") {
					t.Errorf("%dx%d: the selected row is gone:\n%s", cols, rows, out)
				}
				if rows >= 6 && panel && !strings.Contains(out, "Comment:") {
					t.Errorf("%dx%d: the comment box is gone:\n%s", cols, rows, out)
				}
			}
		}
	}
}
