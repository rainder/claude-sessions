package main

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// fleetSentAnswer is a sent answer whose one attempt is at at ("" = no time).
func fleetSentAnswer(at string) fleetAnswer {
	a := fleetAnswerWith(fleetAttemptSent)
	a.Attempts[0].At = at
	return a
}

// fleetCollapseView is an open card "o" and three SENT cards in state.json
// order a, b, c, answered 3h, 1h and 2h before now: b is the newest. Each
// card has its own ticket so its row can be found.
func fleetCollapseView(now time.Time) fleetStateView {
	ago := func(h int) string { return now.Add(-time.Duration(h) * time.Hour).UTC().Format(time.RFC3339) }
	var cards []fleetCardView
	for _, id := range []string{"o", "a", "b", "c"} {
		c := fleetTestCard(id, "needs-you")
		c.Ticket = "DR-" + strings.ToUpper(id)
		cards = append(cards, c)
	}
	v := fleetTestView(cards...)
	v.Answered["a"] = fleetSentAnswer(ago(3))
	v.Answered["b"] = fleetSentAnswer(ago(1))
	v.Answered["c"] = fleetSentAnswer(ago(2))
	return v
}

func fleetItemIDs(items []fleetItem) []string {
	var ids []string
	for _, it := range items {
		ids = append(ids, it.Card.ID)
	}
	return ids
}

func fleetSelID(st *fleetViewState) string {
	if it := st.selected(); it != nil {
		return it.Card.ID
	}
	return ""
}

var fleetCollapseNow = time.Unix(1_800_000_000, 0)

func TestFleetNewestSent(t *testing.T) {
	sent := func(id, at string) fleetItem {
		a := fleetSentAnswer(at)
		return fleetItem{Card: fleetCardView{ID: id}, Section: fleetSecSent, Answer: &a}
	}
	open := fleetItem{Card: fleetCardView{ID: "o"}, Section: fleetSecDecide}
	for _, tc := range []struct {
		name  string
		items []fleetItem
		want  string
	}{
		{"none", []fleetItem{open}, ""},
		{"newest time wins", []fleetItem{open, sent("a", "2026-11-01T10:00:00Z"), sent("b", "2026-11-01T12:00:00Z"), sent("c", "2026-11-01T11:00:00Z")}, "b"},
		{"tie goes to the later card", []fleetItem{sent("a", "2026-11-01T12:00:00Z"), sent("b", "2026-11-01T12:00:00Z")}, "b"},
		{"no times: the later card", []fleetItem{sent("a", ""), sent("b", "")}, "b"},
		{"a time beats none", []fleetItem{sent("a", "2026-11-01T12:00:00Z"), sent("b", "bad")}, "a"},
		{"a time beats none before it", []fleetItem{sent("a", ""), sent("b", "2026-11-01T12:00:00Z"), sent("c", "")}, "b"},
	} {
		got := ""
		if i := fleetNewestSent(tc.items); i >= 0 {
			got = tc.items[i].Card.ID
		}
		if got != tc.want {
			t.Errorf("%s: newest %q, want %q", tc.name, got, tc.want)
		}
	}
}

// Collapsed, the SENT section draws only its newest card and a "+N earlier"
// row; s expands to every SENT card in state.json order and back.
func TestFleetSentCollapsedAndToggle(t *testing.T) {
	now := fleetCollapseNow
	st := fleetTestState(fleetCollapseView(now), now)
	if got := fleetItemIDs(st.items); !slices.Equal(got, []string{"o", "b"}) {
		t.Fatalf("collapsed items %v", got)
	}
	out := fleetRendered(st, 100, 30, now)
	if !strings.Contains(out, "DR-B  You chose") || strings.Contains(out, "DR-A  You chose") || strings.Contains(out, "DR-C  You chose") {
		t.Fatalf("collapsed list:\n%s", out)
	}
	if !strings.Contains(out, "+2 earlier · s show") || strings.Contains(out, "s hide earlier") {
		t.Fatalf("no +2 earlier row:\n%s", out)
	}
	// The toggle row is SENT's last row, right under the newest card.
	lines := strings.Split(out, "\n")
	if r := fleetRowOf(lines, "+2 earlier"); r != fleetRowOf(lines, "DR-B  You chose")+1 {
		t.Fatalf("toggle row at %d:\n%s", r, out)
	}

	if cmd := st.handleKey("s", false, now); cmd != fleetCmdRender || !st.sentOpen {
		t.Fatalf("s: cmd %v open %v", cmd, st.sentOpen)
	}
	if got := fleetItemIDs(st.items); !slices.Equal(got, []string{"o", "a", "b", "c"}) {
		t.Fatalf("expanded items %v", got)
	}
	out = fleetRendered(st, 100, 30, now)
	lines = strings.Split(out, "\n")
	a, b, c := fleetRowOf(lines, "DR-A  You chose"), fleetRowOf(lines, "DR-B  You chose"), fleetRowOf(lines, "DR-C  You chose")
	if a < 0 || b != a+1 || c != b+1 || fleetRowOf(lines, "s hide earlier") != c+1 || strings.Contains(out, "earlier · s show") {
		t.Fatalf("expanded list:\n%s", out)
	}
	if fleetSelID(st) != "o" {
		t.Fatalf("s moved the selection to %q", fleetSelID(st))
	}

	// The expanded state stays across polls.
	st.applyState(fleetCollapseView(now), nil, now)
	if !st.sentOpen || len(st.items) != 4 {
		t.Fatalf("poll: open %v items %v", st.sentOpen, fleetItemIDs(st.items))
	}
	st.handleKey("s", false, now)
	if st.sentOpen || len(st.items) != 2 {
		t.Fatalf("second s: open %v items %v", st.sentOpen, fleetItemIDs(st.items))
	}
	st.applyState(fleetCollapseView(now), nil, now)
	if st.sentOpen || len(st.items) != 2 {
		t.Fatalf("poll collapsed: open %v items %v", st.sentOpen, fleetItemIDs(st.items))
	}

	// A paste never toggles.
	if cmd := st.handleKey("s", true, now); cmd != fleetCmdNone || st.sentOpen {
		t.Fatalf("s in a paste: cmd %v open %v", cmd, st.sentOpen)
	}
}

// With one SENT card or none there is nothing earlier: no toggle row, no
// footer hint, and s does nothing, expanded or not.
func TestFleetSentNoEarlierNoRow(t *testing.T) {
	now := fleetCollapseNow
	one := fleetCollapseView(now)
	delete(one.Answered, "a")
	delete(one.Answered, "c")
	none := fleetCollapseView(now)
	none.Answered = map[string]fleetAnswer{}
	for name, v := range map[string]fleetStateView{"one": one, "none": none} {
		for _, open := range []bool{false, true} {
			st := fleetTestState(v, now)
			st.sentOpen = open
			st.refilter()
			out := fleetRendered(st, 200, 30, now)
			if strings.Contains(out, "earlier") {
				t.Fatalf("%s open=%v: an earlier row or hint:\n%s", name, open, out)
			}
			if cmd := st.handleKey("s", false, now); cmd != fleetCmdNone || st.sentOpen != open {
				t.Fatalf("%s open=%v: s cmd %v open %v", name, open, cmd, st.sentOpen)
			}
			if len(st.items) != 4 {
				t.Fatalf("%s: items %v", name, fleetItemIDs(st.items))
			}
		}
	}
}

// ↑↓, the mouse wheel and a click reach only the cards the list shows, and
// the card keys act on the selected visible card.
func TestFleetSentHiddenNotNavigable(t *testing.T) {
	now := fleetCollapseNow
	st := fleetTestState(fleetCollapseView(now), now)
	for range 5 {
		st.handleKey(KeyDown, false, now)
	}
	if fleetSelID(st) != "b" || st.sel != 1 {
		t.Fatalf("↓ to the end: sel %d on %q", st.sel, fleetSelID(st))
	}
	for range 5 {
		st.handleKey(KeyUp, false, now)
	}
	if fleetSelID(st) != "o" {
		t.Fatalf("↑ to the top: on %q", fleetSelID(st))
	}
	out := fleetRendered(st, 100, 30, now)
	y := fleetScreenRow(out, "DR-O")
	for range 5 {
		st.handleEvent(fleetWheel(y, true), false, now)
	}
	if fleetSelID(st) != "b" {
		t.Fatalf("wheel to the end: on %q", fleetSelID(st))
	}
	// c corrects the selected (visible) card.
	st.handleKey("c", false, now)
	if st.unlocked != "b" {
		t.Fatalf("c unlocked %q", st.unlocked)
	}
	// The details panel shows the selected card, never a hidden one.
	if d := strings.Join(fleetDetailLines(st, *st.selected(), 100, now), "\n"); !strings.Contains(d, "DR-B") {
		t.Fatalf("details:\n%s", d)
	}
}

// A selected card that collapsing hides hands the selection to the newest
// SENT card, panel scroll back at the top; a new state that makes another
// SENT card the newest does the same.
func TestFleetSentSelectionMovesOffHidden(t *testing.T) {
	now := fleetCollapseNow
	st := fleetTestState(fleetCollapseView(now), now)
	st.handleKey("s", false, now)
	st.sel = fleetItemIndexT(t, st, "a")
	st.scroll = 3
	st.handleKey("s", false, now)
	if fleetSelID(st) != "b" || st.scroll != 0 {
		t.Fatalf("collapse: on %q scroll %d", fleetSelID(st), st.scroll)
	}
	// A visible card keeps the selection on collapse.
	st.handleKey("s", false, now)
	st.sel, st.scroll = fleetItemIndexT(t, st, "o"), 2
	st.handleKey("s", false, now)
	if fleetSelID(st) != "o" || st.scroll != 2 {
		t.Fatalf("collapse on a visible card: on %q scroll %d", fleetSelID(st), st.scroll)
	}

	// c is answered again, after b: it becomes the one SENT card shown.
	st.sel, st.scroll = fleetItemIndexT(t, st, "b"), 4
	v := fleetCollapseView(now)
	v.Answered["c"] = fleetSentAnswer(now.UTC().Format(time.RFC3339))
	st.applyState(v, nil, now)
	if got := fleetItemIDs(st.items); !slices.Equal(got, []string{"o", "c"}) {
		t.Fatalf("items %v", got)
	}
	if fleetSelID(st) != "c" || st.scroll != 0 {
		t.Fatalf("reorder: on %q scroll %d", fleetSelID(st), st.scroll)
	}
}

// A Clarify chat on a SENT card the list then hides stays open: the card is
// hidden, not gone.
func TestFleetSentHiddenClarifyStays(t *testing.T) {
	now := fleetCollapseNow
	st := fleetTestState(fleetCollapseView(now), now)
	st.clarify = &fleetClarifyMode{item: st.items[fleetItemIndexT(t, st, "b")], openedAt: now}
	v := fleetCollapseView(now)
	v.Answered["c"] = fleetSentAnswer(now.UTC().Format(time.RFC3339))
	st.applyState(v, nil, now)
	if st.clarify == nil || st.clarify.item.Card.ID != "b" {
		t.Fatalf("clarify %+v, toast %q", st.clarify, st.toast)
	}
}

// The SENT list opened with Enter from the ship shows every SENT card, has
// no toggle row, and s does nothing there; back on the ship and in again it
// still shows them all.
func TestFleetSentListModeShowsAll(t *testing.T) {
	now := fleetShipNow
	v := fleetShipSentView(3)
	for i, id := range []string{"a", "b", "c"} {
		v.Answered[id] = fleetSentAnswer(now.Add(-time.Duration(i+1) * time.Hour).UTC().Format(time.RFC3339))
	}
	st := fleetTestState(v, now)
	if !st.shipOn() {
		t.Fatal("no ship")
	}
	for range 2 {
		st.handleKey(KeyEnter, false, now)
		if got := fleetItemIDs(st.items); !slices.Equal(got, []string{"a", "b", "c"}) {
			t.Fatalf("list mode items %v", got)
		}
		out := fleetRendered(st, 200, 30, now)
		if strings.Contains(out, "earlier") {
			t.Fatalf("list mode draws a toggle row or hint:\n%s", out)
		}
		st.handleKey(KeyDown, false, now)
		st.handleKey(KeyDown, false, now)
		if fleetSelID(st) != "c" {
			t.Fatalf("↓↓ in list mode: on %q", fleetSelID(st))
		}
		if cmd := st.handleKey("s", false, now); cmd != fleetCmdNone || st.sentOpen || len(st.items) != 3 {
			t.Fatalf("s in list mode: cmd %v open %v items %d", cmd, st.sentOpen, len(st.items))
		}
		st.applyState(v, nil, now)
		if len(st.items) != 3 || fleetSelID(st) != "c" {
			t.Fatalf("poll in list mode: items %v on %q", fleetItemIDs(st.items), fleetSelID(st))
		}
		st.handleKey(KeyEsc, false, now)
		if !st.shipOn() {
			t.Fatal("Esc: no ship")
		}
		// The ship still counts every SENT card.
		if out := fleetRendered(st, 100, 30, now); !strings.Contains(out, "Enter sent cards") {
			t.Fatalf("ship help:\n%s", out)
		}
	}
	if n := len(st.all); n != 3 {
		t.Fatalf("all %d", n)
	}
}

// A click on the toggle row does what s does, on both of its texts.
func TestFleetSentToggleClick(t *testing.T) {
	now := fleetCollapseNow
	st := fleetTestState(fleetCollapseView(now), now)
	out := fleetRendered(st, 100, 30, now)
	y := fleetScreenRow(out, "+2 earlier")
	if y < 0 {
		t.Fatalf("no toggle row:\n%s", out)
	}
	if cmd := st.handleEvent(fleetClick(y), false, now); cmd != fleetCmdRender || !st.sentOpen || fleetSelID(st) != "o" {
		t.Fatalf("click: cmd %v open %v on %q", cmd, st.sentOpen, fleetSelID(st))
	}
	out = fleetRendered(st, 100, 30, now)
	y = fleetScreenRow(out, "s hide earlier")
	if cmd := st.handleEvent(fleetClick(y), false, now); cmd != fleetCmdRender || st.sentOpen {
		t.Fatalf("click hide: cmd %v open %v", cmd, st.sentOpen)
	}
	// A confirm panel owns the mouse: the click does nothing.
	out = fleetRendered(st, 100, 30, now)
	y = fleetScreenRow(out, "+2 earlier")
	st.handleKey("1", false, now)
	if st.panel == nil {
		t.Fatal("no panel")
	}
	fleetRendered(st, 100, 30, now)
	if cmd := st.handleEvent(fleetClick(y), false, now); cmd != fleetCmdNone || st.sentOpen {
		t.Fatalf("click under a panel: cmd %v open %v", cmd, st.sentOpen)
	}
	// s on the panel is comment text, not the toggle.
	st.handleKey("s", false, now)
	if st.sentOpen || st.panel.comment != "s" {
		t.Fatalf("s on the panel: open %v comment %q", st.sentOpen, st.panel.comment)
	}
}

// The footer offers s only while the toggle row shows, and fleetFitHelp
// drops it first; the ? panel and the main help list s.
func TestFleetSentHelpHint(t *testing.T) {
	now := fleetCollapseNow
	st := fleetTestState(fleetCollapseView(now), now)
	if row := stripANSI(fleetBottomRowFit(st, now, 0)); !strings.Contains(row, " · "+fleetHelpSentHint+" · ? keys") {
		t.Fatalf("collapsed footer %q", row)
	}
	st.handleKey("s", false, now)
	if row := stripANSI(fleetBottomRowFit(st, now, 0)); !strings.Contains(row, fleetHelpSentHint) {
		t.Fatalf("expanded footer %q", row)
	}
	one := fleetCollapseView(now)
	delete(one.Answered, "a")
	delete(one.Answered, "c")
	st.applyState(one, nil, now)
	if row := stripANSI(fleetBottomRowFit(st, now, 0)); strings.Contains(row, fleetHelpSentHint) {
		t.Fatalf("one SENT card footer %q", row)
	}

	st.applyState(fleetCollapseView(now), nil, now)
	// s goes with the later parts, after the generic ones: it survives at
	// common widths.
	full := stripANSI(fleetBottomRowFit(st, now, 0))
	fit := fleetFitHelp(full, cellWidth(full)-1)
	if !strings.Contains(fit, fleetHelpSentHint) || strings.Contains(fit, "PgUp/PgDn scroll") {
		t.Fatalf("fit drops PgUp/PgDn before %q: %q", fleetHelpSentHint, fit)
	}
	for _, cols := range []int{80, 100} {
		if row := stripANSI(fleetBottomRowFit(st, now, cols)); !strings.Contains(row, fleetHelpSentHint) {
			t.Fatalf("%d cols footer %q", cols, row)
		}
	}

	if !slices.ContainsFunc(fleetHelpText, func(l string) bool { return strings.HasPrefix(l, "  s ") }) {
		t.Fatal("the ? panel has no s line")
	}
}

// A toggle rebuilds the list, so the rest of the same read must not hit the
// rows the last frame drew: a double click toggles once, and a row click
// after s selects nothing until the next render.
func TestFleetSentToggleClearsHitMap(t *testing.T) {
	now := fleetCollapseNow
	st := fleetTestState(fleetCollapseView(now), now)
	out := fleetRendered(st, 100, 30, now)
	y := fleetScreenRow(out, "+2 earlier")
	st.handleEvent(fleetClick(y), false, now)
	if cmd := st.handleEvent(fleetClick(y), false, now); cmd != fleetCmdNone || !st.sentOpen {
		t.Fatalf("second press in the read: cmd %v open %v", cmd, st.sentOpen)
	}

	// Expanded: a row click after s in the same read must not select by the
	// old indexes (the c row's line now shows nothing it showed).
	out = fleetRendered(st, 100, 30, now)
	yc := fleetScreenRow(out, "DR-C  You chose")
	if yc < 0 {
		t.Fatalf("no c row:\n%s", out)
	}
	st.handleKey("s", false, now)
	if cmd := st.handleEvent(fleetClick(yc), false, now); cmd != fleetCmdNone || fleetSelID(st) != "o" {
		t.Fatalf("row click after s: cmd %v on %q", cmd, fleetSelID(st))
	}
	// The next frame's rows work again.
	out = fleetRendered(st, 100, 30, now)
	if st.handleEvent(fleetClick(fleetScreenRow(out, "DR-B  You chose")), false, now); fleetSelID(st) != "b" {
		t.Fatalf("click after the redraw: on %q", fleetSelID(st))
	}
}

// Clarify's draft still opens the Other panel on its own card after a new
// state hides that SENT card and moves the selection off it.
func TestFleetSentHiddenClarifyDraft(t *testing.T) {
	now := fleetCollapseNow
	st := fleetTestState(fleetCollapseView(now), now)
	st.sel = fleetItemIndexT(t, st, "b")
	st.clarify = &fleetClarifyMode{item: st.items[st.sel], openedAt: now,
		view: fleetClarifyView{Draft: &clarifyDraft{Text: "Pick B"}}}
	v := fleetCollapseView(now)
	v.Answered["c"] = fleetSentAnswer(now.UTC().Format(time.RFC3339))
	st.applyState(v, nil, now)
	if fleetSelID(st) != "c" || st.clarify == nil {
		t.Fatalf("on %q clarify %v", fleetSelID(st), st.clarify)
	}
	if cmd := st.handleKey("s", false, now); cmd != fleetCmdRender || st.panel == nil ||
		!st.panel.other || st.panel.item.Card.ID != "b" || st.panel.comment != "Pick B" || st.clarify != nil {
		t.Fatalf("s: cmd %v panel %+v clarify %v", cmd, st.panel, st.clarify)
	}

	// A card no state holds any more: a toast, the chat stays.
	st = fleetTestState(fleetCollapseView(now), now)
	gone := fleetItem{Card: fleetTestCard("gone", "needs-you")}
	st.clarify = &fleetClarifyMode{item: gone, openedAt: now,
		view: fleetClarifyView{Draft: &clarifyDraft{Text: "Pick B"}}}
	if cmd := st.handleKey("s", false, now); cmd != fleetCmdRender || st.panel != nil || st.clarify == nil || st.toast != "card is gone" {
		t.Fatalf("gone card: cmd %v panel %v toast %q", cmd, st.panel, st.toast)
	}
}

// On a screen whose list window holds fewer lines than the list, selecting
// the last card brings SENT's toggle row into view with it.
func TestFleetSentToggleRowScrollsIntoView(t *testing.T) {
	now := fleetCollapseNow
	v := fleetCollapseView(now)
	for _, id := range []string{"p", "q", "r", "u"} {
		c := fleetTestCard(id, "needs-you")
		c.Ticket = "DR-" + strings.ToUpper(id)
		v.Cards = append(v.Cards, c)
		v.CardHashes[id] = "h-" + id
	}
	st := fleetTestState(v, now)
	out := fleetRendered(st, 100, 16, now)
	if strings.Contains(out, "+2 earlier") {
		t.Fatalf("the toggle row fits at the top; make the screen shorter:\n%s", out)
	}
	for range len(st.items) {
		st.handleKey(KeyDown, false, now)
		out = fleetRendered(st, 100, 16, now)
	}
	if fleetSelID(st) != "b" {
		t.Fatalf("on %q", fleetSelID(st))
	}
	lines := strings.Split(out, "\n")
	if b, r := fleetRowOf(lines, "DR-B  You chose"), fleetRowOf(lines, "+2 earlier"); b < 0 || r != b+1 {
		t.Fatalf("last card at %d, toggle row at %d:\n%s", b, r, out)
	}
	// And the row works there.
	if st.handleEvent(fleetClick(fleetScreenRow(out, "+2 earlier")), false, now); !st.sentOpen {
		t.Fatal("click on the scrolled-in toggle row")
	}
}
