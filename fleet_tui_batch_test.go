package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// fleetBatchCard is fleetTestCard(id, kind) with its own ticket and lean.
func fleetBatchCard(id, kind, lean string) fleetCardView {
	c := fleetTestCard(id, kind)
	c.Ticket = "DR-" + id
	c.Lean = lean
	return c
}

// fleetBatchView has two sendable cards (b APPROVE, a DECIDE), two skipped
// ones (c no lean, d lean names no option) and FLEET on ws.
func fleetBatchView() fleetStateView {
	v := fleetTestView(
		fleetBatchCard("a", "needs-you", "not-yet"),
		fleetBatchCard("b", "explicit-yes", "yes"),
		fleetBatchCard("c", "needs-you", ""),
		fleetBatchCard("d", "needs-you", "maybe"),
	)
	v.Source = "http://ws:8765"
	return v
}

func fleetBatchNames(items []fleetBatchItem) []string {
	var out []string
	for _, it := range items {
		out = append(out, it.Name+"→"+it.Option)
	}
	return out
}

func TestFleetLeanBatchSet(t *testing.T) {
	now := time.Unix(1000, 0)
	tenth := fleetBatchCard("g", "needs-you", "k9")
	for i := len(tenth.Options); i < 10; i++ {
		tenth.Options = append(tenth.Options, fleetOptionView{Key: fmt.Sprintf("k%d", i), Label: "L", Send: "S"})
	}
	two := fleetBatchCard("e", "needs-you", "yes")
	two.Options[1].Key = "yes"
	noSend := fleetBatchCard("f", "needs-you", "not-yet")
	noSend.Options[1].Send = ""
	untitled := fleetBatchCard("t", "needs-you", "yes")
	untitled.Ticket = ""
	v := fleetTestView(
		fleetBatchCard("a", "needs-you", "not-yet"),
		fleetBatchCard("b", "explicit-yes", "yes"),
		fleetBatchCard("c", "needs-you", ""),
		fleetBatchCard("d", "needs-you", "maybe"),
		two, noSend, tenth,
		fleetBatchCard("h", "needs-you", "yes"), // answered
		fleetBatchCard("i", "needs-you", "yes"), // justSent
		fleetBatchCard("j", "needs-you", "yes"), // unclear
		fleetBatchCard("k", "needs-you", "yes"), // no card hash
		fleetBatchCard("l", "needs-you", "yes"), // an earlier batch reached it
		untitled,
	)
	v.Source = "http://ws:8765"
	v.Answered["h"] = fleetAnswerWith(fleetAttemptUnconfirmed)
	delete(v.CardHashes, "k")
	st := fleetTestState(v, now)
	st.justSent, st.unclear, st.batchSent = "i", "j", map[string]bool{"l": true}

	items, skipped := st.leanBatch()
	if got, want := strings.Join(fleetBatchNames(items), ","), "DR-b→Merge now,DR-a→Hold,card t→Merge now"; got != want {
		t.Fatalf("items = %q, want %q", got, want)
	}
	var reasons []string
	for _, s := range skipped {
		reasons = append(reasons, s.Name+": "+s.Reason)
	}
	want := []string{
		"DR-c: no FLEET pick",
		"DR-d: pick names no option",
		"DR-e: pick names two options",
		"DR-f: pick has nothing to send",
		"DR-g: pick is past option 9",
	}
	if strings.Join(reasons, "|") != strings.Join(want, "|") {
		t.Fatalf("skipped = %q\nwant %q", reasons, want)
	}

	// Each request is the one r makes on that card.
	for _, it := range items {
		rs := fleetTestState(v, now)
		rs.sel = fleetItemIndex(rs.items, it.Req.CardID)
		fleetDrawn(rs, now)
		if cmd := rs.handleKey("r", false, now.Add(fleetLeanPickDelay)); cmd != fleetCmdSend {
			t.Fatalf("r on %s: cmd = %v", it.Req.CardID, cmd)
		}
		if *rs.send != it.Req {
			t.Fatalf("%s: batch request %+v, r sends %+v", it.Req.CardID, it.Req, *rs.send)
		}
	}

	t.Run("FLEET not running", func(t *testing.T) {
		v := fleetBatchView()
		v.Fleet.Alive = false
		st := fleetTestState(v, now)
		if cmd := st.handleKey("R", false, now); cmd != fleetCmdNone || st.batch != nil || st.toast != "" {
			t.Fatalf("cmd = %v batch = %v toast = %q", cmd, st.batch, st.toast)
		}
	})
}

func TestFleetBatchNothingToSend(t *testing.T) {
	now := time.Unix(1000, 0)
	t.Run("skipped only", func(t *testing.T) {
		st := fleetTestState(fleetTestView(fleetBatchCard("c", "needs-you", ""), fleetBatchCard("d", "needs-you", "maybe")), now)
		if cmd := st.handleKey("R", false, now); cmd != fleetCmdRender || st.batch != nil {
			t.Fatalf("cmd = %v batch = %v", cmd, st.batch)
		}
		if st.toast != "no FLEET picks to send · 2 skipped" {
			t.Fatalf("toast = %q", st.toast)
		}
	})
	t.Run("answered only", func(t *testing.T) {
		v := fleetTestView(fleetBatchCard("a", "needs-you", "yes"), fleetBatchCard("b", "needs-you", "yes"))
		v.Answered["a"] = fleetAnswerWith(fleetAttemptUnconfirmed)
		st := fleetTestState(v, now)
		st.justSent = "b"
		if cmd := st.handleKey("R", false, now); cmd != fleetCmdRender || st.batch != nil || st.toast != "no FLEET picks to send" {
			t.Fatalf("cmd = %v batch = %v toast = %q", cmd, st.batch, st.toast)
		}
	})
}

func TestFleetBatchPanelKeys(t *testing.T) {
	now := time.Unix(1000, 0)
	open := func() *fleetViewState {
		st := fleetTestState(fleetBatchView(), now)
		if cmd := st.handleKey("R", false, now); cmd != fleetCmdRender || st.batch == nil {
			t.Fatalf("R: cmd = %v batch = %v", cmd, st.batch)
		}
		return st
	}

	t.Run("arm delay and paste", func(t *testing.T) {
		st := open()
		if cmd := st.handleKey(KeyEnter, false, now.Add(fleetArmDelay-time.Millisecond)); cmd != fleetCmdNone || st.batch == nil || st.sending {
			t.Fatalf("Enter before arm: cmd = %v", cmd)
		}
		if cmd := st.handleKey(KeyEnter, true, now.Add(time.Second)); cmd != fleetCmdNone || st.batch == nil || st.sending {
			t.Fatalf("Enter in a paste: cmd = %v", cmd)
		}
		// The arm keeps the loop's wait short enough to redraw the countdown.
		if d := fleetLoopTimeout(st, time.Time{}, now); d != fleetArmDelay {
			t.Fatalf("loop timeout = %v, want %v", d, fleetArmDelay)
		}
		if cmd := st.handleKey(KeyEnter, false, now.Add(fleetArmDelay)); cmd != fleetCmdSendBatch {
			t.Fatalf("Enter after arm: cmd = %v", cmd)
		}
		if st.batch != nil || !st.sending || len(st.batchSend) != 2 {
			t.Fatalf("batch = %v sending = %v batchSend = %v", st.batch, st.sending, st.batchSend)
		}
		for _, it := range st.batchSend {
			if it.Req.Source != "http://ws:8765" || it.Req.CardHash != "h-"+it.Req.CardID || it.Req.Correction {
				t.Fatalf("request %+v", it.Req)
			}
		}
	})

	for _, k := range []string{KeyEsc, "q", "\x03"} {
		t.Run(fmt.Sprintf("cancel %q", k), func(t *testing.T) {
			st := open()
			if cmd := st.handleKey(k, false, now.Add(time.Second)); cmd != fleetCmdRender || st.batch != nil || st.sending || st.batchSend != nil {
				t.Fatalf("cmd = %v batch = %v sending = %v", cmd, st.batch, st.sending)
			}
		})
	}

	t.Run("other keys do nothing", func(t *testing.T) {
		st := open()
		for _, k := range []string{"1", "r", "R", "x", "c", "o", "a", "k", KeyDown} {
			if cmd := st.handleKey(k, false, now.Add(time.Second)); cmd != fleetCmdNone || st.batch == nil || st.panel != nil || st.sending {
				t.Fatalf("%q: cmd = %v batch = %v panel = %v", k, cmd, st.batch, st.panel)
			}
		}
	})

	t.Run("mouse ignored", func(t *testing.T) {
		st := open()
		renderFleetView(st, 120, 40, now)
		sel := st.sel
		for _, m := range []mouseEvent{{button: mouseWheelDown, y: 4}, {button: mouseLeft, y: 4}, {button: mouseWheelUp, y: 30}} {
			if cmd := st.handleMouse(m); cmd != fleetCmdNone || st.sel != sel {
				t.Fatalf("%+v: cmd = %v sel = %d", m, cmd, st.sel)
			}
		}
	})

	t.Run("a state with every card answered keeps the panel", func(t *testing.T) {
		st := open()
		v := fleetBatchView()
		for _, id := range []string{"a", "b", "c", "d"} {
			v.Answered[id] = fleetAnswerWith(fleetAttemptSent)
		}
		st.applyState(v, nil, now)
		if st.shipOn() || st.batch == nil {
			t.Fatalf("shipOn = %v batch = %v", st.shipOn(), st.batch)
		}
	})
}

func TestFleetBatchPanelLines(t *testing.T) {
	now := time.Unix(1000, 0)
	st := fleetTestState(fleetBatchView(), now)
	st.handleKey("R", false, now)

	frame := stripANSI(renderFleetView(st, 100, 40, now))
	for _, want := range []string{
		"Send FLEET's pick on 2 cards?",
		"DR-b → Merge now",
		"DR-a → Hold",
		"Skipped:",
		"DR-c — no FLEET pick",
		"DR-d — pick names no option",
		"Enter send all (ready in 0.4 s) · Esc cancel",
	} {
		if !strings.Contains(frame, want) {
			t.Errorf("frame lacks %q:\n%s", want, frame)
		}
	}
	if strings.Index(frame, "DR-b → Merge now") > strings.Index(frame, "DR-a → Hold") {
		t.Error("cards not in list order")
	}
	frame = stripANSI(renderFleetView(st, 100, 40, now.Add(fleetArmDelay)))
	if !strings.Contains(frame, "Enter send all · Esc cancel") {
		t.Errorf("armed frame:\n%s", frame)
	}
	if st.drawn.id != "" {
		t.Error("the R panel counted as drawing a card for r")
	}

	t.Run("one card", func(t *testing.T) {
		st := fleetTestState(fleetTestView(fleetBatchCard("a", "needs-you", "yes")), now)
		st.handleKey("R", false, now)
		lines := fleetBatchPanelLines(st.batch, -1, now.Add(time.Second))
		if stripANSI(lines[0]) != "Send FLEET's pick on 1 card?" || strings.Contains(strings.Join(lines, "\n"), "Skipped") {
			t.Fatalf("lines = %q", lines)
		}
	})

	t.Run("scrolls with the key line pinned", func(t *testing.T) {
		var cards []fleetCardView
		for i := range 12 {
			cards = append(cards, fleetBatchCard(fmt.Sprintf("%02d", i), "needs-you", "yes"))
		}
		st := fleetTestState(fleetTestView(cards...), now)
		st.handleKey("R", false, now)
		at := now.Add(time.Second)
		const h = 6
		top := fleetBatchPanelLines(st.batch, h, at)
		if len(top) != h || stripANSI(top[h-1]) != "Enter send all · Esc cancel" || !strings.Contains(stripANSI(top[h-3]), "↓ more") {
			t.Fatalf("top = %q", top)
		}
		st.panelRows = h
		st.handleKey(KeyPageDown, false, at)
		st.handleKey(KeyPageDown, false, at)
		st.handleKey(KeyPageDown, false, at)
		st.handleKey(KeyPageDown, false, at)
		down := fleetBatchPanelLines(st.batch, h, at)
		if len(down) != h || stripANSI(down[h-1]) != "Enter send all · Esc cancel" || !strings.Contains(stripANSI(down[0]), "↑ more") {
			t.Fatalf("scrolled = %q", down)
		}
		if !strings.Contains(stripANSI(down[h-3]), "DR-11") {
			t.Fatalf("the last card is not reachable: %q", down)
		}
		st.handleKey(KeyPageUp, false, at)
		st.handleKey(KeyPageUp, false, at)
		st.handleKey(KeyPageUp, false, at)
		if st.batch.scroll != 0 {
			t.Fatalf("scroll = %d after PgUp", st.batch.scroll)
		}
	})
}

// fleetFakeClock is a clock whose sleep only moves it.
type fleetFakeClock struct{ t time.Time }

func (c *fleetFakeClock) sleep(d time.Duration) { c.t = c.t.Add(d) }

func fleetBatchItems(ids ...string) []fleetBatchItem {
	var out []fleetBatchItem
	for _, id := range ids {
		out = append(out, fleetBatchItem{Name: "DR-" + id, Option: "Merge now",
			Req: fleetSayRequest{Source: "http://ws:8765", Text: "Merge now", CardID: id, CardHash: "h-" + id, OptionKey: "yes"}})
	}
	return out
}

func fleetOK(id string) fleetSendResult {
	return fleetSendResult{CardID: id, Out: fleetSayOutcome{Status: 200, Result: fleetSayResult{OK: true}}}
}

func TestFleetBatchJob(t *testing.T) {
	start := time.Unix(1000, 0)

	t.Run("in order with gaps", func(t *testing.T) {
		clock := &fleetFakeClock{t: start}
		var ids []string
		var at []time.Time
		res := fleetBatchJob(fleetBatchItems("a", "b", "c"), func(r fleetSayRequest) fleetSendResult {
			ids, at = append(ids, r.CardID), append(at, clock.t)
			return fleetOK(r.CardID)
		}, clock.sleep)
		if strings.Join(ids, ",") != "a,b,c" || len(res.Results) != 3 {
			t.Fatalf("sent %v, %d results", ids, len(res.Results))
		}
		if !at[0].Equal(start) {
			t.Fatalf("first send waited %v", at[0].Sub(start))
		}
		for i := 1; i < len(at); i++ {
			if gap := at[i].Sub(at[i-1]); gap < 1100*time.Millisecond {
				t.Fatalf("gap %d = %v", i, gap)
			}
		}
	})

	t.Run("stale card is reported, the rest continue", func(t *testing.T) {
		clock := &fleetFakeClock{t: start}
		res := fleetBatchJob(fleetBatchItems("a", "b", "c"), func(r fleetSayRequest) fleetSendResult {
			if r.CardID == "b" {
				return fleetSendResult{CardID: "b", Stale: true}
			}
			return fleetOK(r.CardID)
		}, clock.sleep)
		if len(res.Results) != 3 {
			t.Fatalf("%d results", len(res.Results))
		}
		st := fleetTestState(fleetTestView(fleetBatchCard("a", "needs-you", "yes"), fleetBatchCard("b", "needs-you", "yes"), fleetBatchCard("c", "needs-you", "yes")), start)
		st.sending = true
		if !st.applyBatch(res, start) {
			t.Fatal("no refetch")
		}
		if st.sending || st.toast != "sent 2 of 3 · DR-b: card changed" || st.toastKind != fleetToastWarn {
			t.Fatalf("sending = %v toast = %q kind = %v", st.sending, st.toast, st.toastKind)
		}
		if !st.batchSent["a"] || st.batchSent["b"] || !st.batchSent["c"] {
			t.Fatalf("batchSent = %v", st.batchSent)
		}
	})

	stops := []struct {
		name  string
		res   fleetSendResult
		toast string
	}{
		{"host moved", fleetSendResult{CardID: "b", Moved: true}, "sent 1 of 3 · DR-b: " + errFleetMoved.Error()},
		{"re-check failed", fleetSendResult{CardID: "b", CheckErr: errors.New("down")}, "sent 1 of 3 · DR-b: could not re-check the card"},
		{"unclear", fleetSendResult{CardID: "b", Out: fleetSayOutcome{Unclear: true}}, "sent 1 of 3 · DR-b: " + fleetToastUnclear},
		{"token rejected", fleetSendResult{CardID: "b", Out: fleetSayOutcome{Status: 401}}, "sent 1 of 3 · DR-b: " + fleetToastTokenBad},
		{"not submitted", fleetSendResult{CardID: "b", Out: fleetSayOutcome{Status: 200, Result: fleetSayResult{Code: "not_submitted"}}},
			"sent 1 of 3 · DR-b: typed but not submitted — check FLEET's pane"},
	}
	for _, tc := range stops {
		t.Run(tc.name+" stops the batch", func(t *testing.T) {
			clock := &fleetFakeClock{t: start}
			var ids []string
			res := fleetBatchJob(fleetBatchItems("a", "b", "c"), func(r fleetSayRequest) fleetSendResult {
				ids = append(ids, r.CardID)
				if r.CardID == "b" {
					return tc.res
				}
				return fleetOK(r.CardID)
			}, clock.sleep)
			if strings.Join(ids, ",") != "a,b" || len(res.Results) != 2 {
				t.Fatalf("sent %v", ids)
			}
			st := fleetTestState(fleetTestView(fleetBatchCard("a", "needs-you", "yes"), fleetBatchCard("b", "needs-you", "yes"), fleetBatchCard("c", "needs-you", "yes")), start)
			st.sending = true
			st.applyBatch(res, start)
			if st.toast != tc.toast {
				t.Fatalf("toast = %q, want %q", st.toast, tc.toast)
			}
			if !st.batchSent["a"] || st.batchSent["c"] {
				t.Fatalf("batchSent = %v", st.batchSent)
			}
			// not_submitted was recorded by the server; unclear may have been.
			switch tc.name {
			case "not submitted":
				if !st.batchSent["b"] {
					t.Fatal("not_submitted card not locked")
				}
			case "unclear":
				if st.unclear != "b" || st.batchSent["b"] {
					t.Fatalf("unclear = %q batchSent = %v", st.unclear, st.batchSent)
				}
			default:
				if st.batchSent["b"] || st.unclear != "" {
					t.Fatalf("b locked: unclear = %q batchSent = %v", st.unclear, st.batchSent)
				}
			}
		})
	}

	t.Run("all sent", func(t *testing.T) {
		st := fleetTestState(fleetTestView(fleetBatchCard("a", "needs-you", "yes"), fleetBatchCard("b", "needs-you", "yes")), start)
		st.applyBatch(fleetBatchResult{Items: fleetBatchItems("a", "b"), Results: []fleetSendResult{fleetOK("a"), fleetOK("b")}}, start)
		if st.toast != "sent 2 of 2" || st.toastKind != fleetToastInfo {
			t.Fatalf("toast = %q kind = %v", st.toast, st.toastKind)
		}
	})
}

func TestFleetBatchSentLocks(t *testing.T) {
	now := time.Unix(1000, 0)
	v := fleetBatchView()
	st := fleetTestState(v, now)
	st.sel = fleetItemIndex(st.items, "b")
	st.applyBatch(fleetBatchResult{Items: fleetBatchItems("b", "a"), Results: []fleetSendResult{fleetOK("b"), fleetOK("a")}}, now)
	if st.nextAfter != "b" {
		t.Fatalf("nextAfter = %q, want the selected sent card", st.nextAfter)
	}

	// A poll read before the answers landed: both stay locked.
	st.applyState(v, nil, now)
	for _, id := range []string{"a", "b"} {
		it := &st.items[fleetItemIndex(st.items, id)]
		if msg, _, blocked := st.pickRefusal(it); !blocked || msg != "sent — waiting for FLEET" {
			t.Fatalf("%s: blocked = %v msg = %q", id, blocked, msg)
		}
	}
	if items, _ := st.leanBatch(); len(items) != 0 {
		t.Fatalf("R would send locked cards again: %v", fleetBatchNames(items))
	}
	// A locked card's options are greyed, so its lean loses its colour.
	if opts := strings.Join(fleetOptionLines(st, st.items[fleetItemIndex(st.items, "a")], 100), "\n"); strings.Contains(opts, colorize(fleetSGRDecide, "★ FLEET recommends")) {
		t.Fatalf("locked card's options not greyed: %q", opts)
	}

	// A state with a recorded unlocks a; b is gone and unlocks too.
	v2 := fleetBatchView()
	v2.Cards = []fleetCardView{v2.Cards[0], v2.Cards[2], v2.Cards[3]}
	v2.Answered["a"] = fleetAnswerWith(fleetAttemptSent)
	st.applyState(v2, nil, now)
	if len(st.batchSent) != 0 {
		t.Fatalf("batchSent = %v after the state showed both", st.batchSent)
	}
}

func TestFleetBatchKeysWhileSending(t *testing.T) {
	now := time.Unix(1000, 0)
	v := fleetBatchView()
	v.Answered["c"] = fleetAnswerWith(fleetAttemptUnconfirmed)
	st := fleetTestState(v, now)
	fleetDrawn(st, now)
	st.handleKey("R", false, now)
	if cmd := st.handleKey(KeyEnter, false, now.Add(time.Second)); cmd != fleetCmdSendBatch {
		t.Fatalf("Enter: cmd = %v", cmd)
	}
	at := now.Add(5 * time.Second)
	fleetDrawn(st, at)
	at = at.Add(fleetLeanPickDelay)
	for _, k := range []string{KeyEsc, "q", "\x03"} {
		st.toast = ""
		if cmd := st.handleKey(k, false, at); cmd == fleetCmdClose || st.toast != fleetToastStillSend {
			t.Fatalf("%q while sending: cmd = %v toast = %q", k, cmd, st.toast)
		}
	}
	for _, k := range []string{"1", "2", "r", "R", "x", "c"} {
		for _, id := range []string{"a", "c"} {
			st.sel = fleetItemIndex(st.items, id)
			if cmd := st.handleKey(k, false, at); cmd == fleetCmdSend || cmd == fleetCmdSendBatch || cmd == fleetCmdDismiss ||
				st.panel != nil || st.batch != nil || st.unlocked != "" {
				t.Fatalf("%q on %s while sending: cmd = %v panel = %v batch = %v unlocked = %q", k, id, cmd, st.panel, st.batch, st.unlocked)
			}
		}
	}
	if row := stripANSI(fleetBottomRow(st, at.Add(time.Hour))); row != "sending…" {
		t.Fatalf("bottom row = %q", row)
	}
}

func TestFleetBatchHelpHint(t *testing.T) {
	now := time.Unix(1000, 0)
	st := fleetTestState(fleetBatchView(), now)
	st.sel = fleetItemIndex(st.items, "c") // no lean: r is off, R is on
	if row := stripANSI(fleetBottomRow(st, now)); !strings.Contains(row, "1-9 pick option · R all picks · o other") {
		t.Fatalf("help row = %q", row)
	}
	st.handleKey("R", false, now)
	if row := fleetBottomRow(st, now); row != "" {
		t.Fatalf("panel open: bottom row = %q", row)
	}
	st.batch = nil

	none := fleetTestState(fleetTestView(fleetBatchCard("c", "needs-you", ""), fleetBatchCard("d", "needs-you", "maybe")), now)
	if row := stripANSI(fleetBottomRow(none, now)); strings.Contains(row, "R all picks") {
		t.Fatalf("nothing to send: help row = %q", row)
	}
	st.sending = true
	if st.batchPickable() {
		t.Fatal("batchPickable while sending")
	}
}

func TestFleetLoopRunsBatch(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	v := fleetBatchView()
	states := make(chan fleetStateView, 1)
	var slept []time.Duration
	var sent []fleetSayRequest
	l := &fleetLoop{
		st:       fleetTestState(v, now),
		interval: time.Hour,
		state:    func() (fleetStateView, error) { return <-states, nil },
		send: func(r fleetSayRequest) fleetSendResult {
			sent = append(sent, r)
			return fleetOK(r.CardID)
		},
		sleep: func(d time.Duration) { slept = append(slept, d) },
	}
	defer l.closeAll()
	l.st.handleKey("R", false, now)
	if l.run(l.st.handleKey(KeyEnter, false, now.Add(time.Second)), now) {
		t.Fatal("the batch closed the view")
	}
	if l.st.batchSend != nil || len(l.batches) != 1 {
		t.Fatalf("batchSend = %v batches = %d", l.st.batchSend, len(l.batches))
	}
	waitFleetJobs(t, l)
	l.reap(now)
	if l.st.sending || l.st.toast != "sent 2 of 2" {
		t.Fatalf("sending = %v toast = %q", l.st.sending, l.st.toast)
	}
	if len(sent) != 2 || sent[0].CardID != "b" || sent[1].CardID != "a" || len(slept) != 1 || slept[0] != fleetBatchGap {
		t.Fatalf("sent %+v slept %v", sent, slept)
	}
	if len(l.batches) != 0 || len(l.states) != 1 {
		t.Fatalf("batches = %d states = %d, want the job reaped and a refetch", len(l.batches), len(l.states))
	}
	v.Answered["a"] = fleetAnswerWith(fleetAttemptSent)
	v.Answered["b"] = fleetAnswerWith(fleetAttemptSent)
	states <- v
	waitFleetJobs(t, l)
	l.reap(now)
	if len(l.st.batchSent) != 0 {
		t.Fatalf("batchSent = %v after the refetch", l.st.batchSent)
	}
}
