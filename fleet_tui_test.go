package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestFleetWindow(t *testing.T) {
	cases := []struct {
		total, selStart, selEnd, rows, scroll, want int
	}{
		{50, 10, 14, 20, 0, 0},
		{50, 40, 45, 20, 0, 25},
		{50, 10, 40, 20, 0, 10},
		{50, 10, 40, 20, 5, 15},
		{50, 10, 40, 20, 99, 20},
		{50, 10, 14, 0, 0, 0},
		{50, 10, 40, 20, -3, 10},
	}
	for _, c := range cases {
		if got := fleetWindow(c.total, c.selStart, c.selEnd, c.rows, c.scroll); got != c.want {
			t.Errorf("fleetWindow(%d, %d-%d, rows %d, scroll %d) = %d, want %d",
				c.total, c.selStart, c.selEnd, c.rows, c.scroll, got, c.want)
		}
	}
}

// fleetRenderView is the layout-A example: an alive, idle FLEET, one APPROVE,
// one DECIDE and one SENT card.
func fleetRenderView(now time.Time) fleetStateView {
	approve := fleetCardView{ID: "a", Kind: "explicit-yes", Ticket: "DR-5241", Title: "Merge PR #2786 into develop",
		Summary: "CI is green.", Lean: "yes", WaitingOn: "Andy", Session: "dr-5241",
		UpdatedAt: now.Add(-4 * time.Minute).UTC().Format(time.RFC3339),
		Options: []fleetOptionView{
			{Key: "yes", Label: "Merge now", Line: "merge with --merge, delete branch", Send: "Merge now"},
			{Key: "not-yet", Label: "Hold", Line: "keep the PR open", Send: "Hold"},
		}}
	decide := fleetTestCard("b", "needs-you")
	decide.Ticket, decide.Title = "DR-5302", "Retry policy for OCR timeouts"
	sent := fleetTestCard("c", "needs-you")
	sent.Ticket, sent.Title = "DR-5190", "Cache or not"
	v := fleetTestView(approve, decide, sent)
	v.Fleet.Status = "idle"
	v.StateFileAt = now.Add(-2 * time.Minute).UTC().Format(time.RFC3339)
	v.Answered["c"] = fleetAnswer{CardHash: "h-c", Attempts: []fleetAttempt{{OptionKey: "B", Label: "keep cache",
		At: now.Add(-3 * time.Minute).UTC().Format(time.RFC3339), Status: fleetAttemptSent}}}
	return v
}

func fleetRendered(st *fleetViewState, cols, rows int, now time.Time) string {
	return stripANSI(renderFleetView(st, cols, rows, now))
}

func TestRenderFleetViewList(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	st := fleetTestState(fleetRenderView(now), now)
	out := fleetRendered(st, 100, 40, now)
	for _, want := range []string{
		"FLEET ● idle", "state 2m ago", "1 approve · 1 decide · 1 sent",
		"APPROVE", "DECIDE", "SENT — WAITING FOR FLEET",
		"▸ DR-5241", "1  yes", "★ FLEET recommends", "2  not-yet", "You chose: keep cache",
		"CI is green.", "Waiting on: Andy · session dr-5241", "merge with --merge, delete branch",
		"DR-5302  Retry policy for OCR timeouts", "(3m ago)",
		"↑↓ card · 1-9 pick option",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "▸ DR-5302") {
		t.Error("only the selected card carries the marker")
	}
	// The age of the selected card sits at the right edge.
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "▸ DR-5241") && !strings.HasSuffix(l, "4m") {
			t.Errorf("collapsed row %q does not end with its age", l)
		}
	}
	if n := len(strings.Split(out, "\n")); n != 40 {
		t.Errorf("frame has %d lines, want 40", n)
	}
}

func fleetHasLine(out, want string) bool {
	for _, l := range strings.Split(out, "\n") {
		if strings.TrimSpace(l) == want {
			return true
		}
	}
	return false
}

func TestRenderFleetViewGreyedAndNotes(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	render := func(v fleetStateView) string {
		return fleetRendered(fleetTestState(v, now), 120, 60, now)
	}
	t.Run("lean matches no option", func(t *testing.T) {
		c := fleetTestCard("a", "needs-you")
		c.Lean = "zzz"
		if out := render(fleetTestView(c)); !strings.Contains(out, "Lean: zzz") {
			t.Errorf("no Lean line:\n%s", out)
		}
	})
	t.Run("lean matches an option", func(t *testing.T) {
		c := fleetTestCard("a", "needs-you")
		c.Lean = "yes"
		if out := render(fleetTestView(c)); strings.Contains(out, "Lean:") {
			t.Errorf("a matching lean is a star, not a Lean line:\n%s", out)
		}
	})
	t.Run("id-less card", func(t *testing.T) {
		if out := render(fleetTestView(fleetTestCard("", "needs-you"))); !strings.Contains(out, "answer in the chat") {
			t.Errorf("no chat note:\n%s", out)
		}
	})
	t.Run("ten options", func(t *testing.T) {
		c := fleetTestCard("a", "needs-you")
		c.Options = nil
		for i := 1; i <= 10; i++ {
			c.Options = append(c.Options, fleetOptionView{Key: fmt.Sprintf("k%d", i), Label: fmt.Sprintf("L%d", i), Send: "s"})
		}
		out := render(fleetTestView(c))
		if !strings.Contains(out, "answer on the page") {
			t.Errorf("no page note:\n%s", out)
		}
		for _, l := range strings.Split(out, "\n") {
			if strings.Contains(l, "L9") && strings.Contains(l, "answer on the page") {
				t.Errorf("option 9 is answerable here: %q", l)
			}
		}
	})
	t.Run("fleet not alive", func(t *testing.T) {
		v := fleetTestView(fleetTestCard("a", "needs-you"))
		v.Fleet = fleetSession{Alive: false, Code: "not_live"}
		out := render(v)
		for _, want := range []string{"FLEET ○ not running (not_live)", "FLEET is not running (not_live) — answers are off"} {
			if !strings.Contains(out, want) {
				t.Errorf("lacks %q:\n%s", want, out)
			}
		}
	})
	t.Run("unconfirmed", func(t *testing.T) {
		v := fleetTestView(fleetTestCard("a", "needs-you"))
		v.Answered["a"] = fleetAnswerWith(fleetAttemptUnconfirmed)
		out := render(v)
		for _, want := range []string{"delivery not confirmed — check FLEET's chat", "c send a correction · x dismiss", "You chose: Merge now"} {
			if !strings.Contains(out, want) {
				t.Errorf("lacks %q:\n%s", want, out)
			}
		}
		if strings.Contains(out, "Card changed") {
			t.Error("Card changed shows only with cardChanged")
		}
	})
	t.Run("unconfirmed and card changed", func(t *testing.T) {
		v := fleetTestView(fleetTestCard("a", "needs-you"))
		a := fleetAnswerWith(fleetAttemptUnconfirmed)
		a.CardChanged = true
		v.Answered["a"] = a
		if out := render(v); !strings.Contains(out, "Card changed · delivery of your last answer is still unknown — check the chat") {
			t.Errorf("no card-changed line:\n%s", out)
		}
	})
	t.Run("corrections", func(t *testing.T) {
		v := fleetTestView(fleetTestCard("a", "needs-you"))
		v.Answered["a"] = fleetAnswer{CardHash: "h", Attempts: []fleetAttempt{
			{Label: "one", Status: fleetAttemptSent}, {Label: "two", Status: fleetAttemptSent}, {Label: "three", Status: fleetAttemptSent},
		}}
		out := render(v)
		for _, want := range []string{"You chose: one", "Correction: two", "Correction: three"} {
			if !strings.Contains(out, want) {
				t.Errorf("lacks %q:\n%s", want, out)
			}
		}
	})
	t.Run("state error", func(t *testing.T) {
		v := fleetTestView(fleetTestCard("a", "needs-you"))
		v.StateError = "bad"
		if out := render(v); !fleetHasLine(out, "bad") {
			t.Errorf("no stateError line:\n%s", out)
		}
	})
	t.Run("no cards", func(t *testing.T) {
		if out := render(fleetTestView()); !strings.Contains(out, "No cards wait for you.") {
			t.Errorf("no empty note:\n%s", out)
		}
	})
	t.Run("stale state", func(t *testing.T) {
		v := fleetTestView()
		v.Stale = true
		v.StateFileAt = now.Add(-45 * time.Minute).UTC().Format(time.RFC3339)
		raw := renderFleetView(fleetTestState(v, now), 120, 20, now)
		if !strings.Contains(raw, colorize("33", "state 45m ago")) {
			t.Errorf("stale stamp is not amber: %q", raw)
		}
	})
}

func TestRenderFleetViewNoService(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	st := fleetTestState(fleetRenderView(now), now)
	st.applyState(fleetStateView{}, errors.New("dial tcp: refused"), now)
	out := fleetRendered(st, 120, 30, now)
	if !strings.Contains(out, "claude-sessions service restart") {
		t.Errorf("no restart hint:\n%s", out)
	}
	for _, bad := range []string{"APPROVE", "DR-5241", "FLEET ●"} {
		if strings.Contains(out, bad) {
			t.Errorf("an error view shows %q:\n%s", bad, out)
		}
	}
}

func TestRenderFleetViewPanel(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	st := fleetTestState(fleetRenderView(now), now)
	opened := now
	st.handleKey("1", false, opened)
	if st.panel == nil {
		t.Fatal("no panel")
	}
	st.handleKey("h", false, opened)
	st.handleKey("i", false, opened)

	out := fleetRendered(st, 100, 30, opened.Add(100*time.Millisecond))
	for _, want := range []string{
		"Approve this write?", "DR-5241 · 1 yes · Merge now", "effect: merge with --merge, delete branch",
		"FLEET receives:", "Merge now Andy adds: hi", "Comment: hi▌", "ready in 0.3 s", "Esc cancel · comment is optional",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("panel lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "APPROVE") {
		t.Errorf("the panel replaces the list:\n%s", out)
	}
	if out := fleetRendered(st, 100, 30, opened.Add(500*time.Millisecond)); strings.Contains(out, "ready in") {
		t.Errorf("armed panel still counts down:\n%s", out)
	}

	st.panel = nil
	st.handleKey(KeyDown, false, now)
	st.handleKey("1", false, now)
	if out := fleetRendered(st, 100, 30, now); !strings.Contains(out, "Send this to FLEET?") {
		t.Errorf("needs-you panel title:\n%s", out)
	}
}

func TestRenderFleetViewNarrow(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	v := fleetRenderView(now)
	v.Cards[0].Summary = strings.Repeat("word ", 40)
	st := fleetTestState(v, now)
	for _, cols := range []int{20, 33, 47} {
		out := renderFleetView(st, cols, 25, now)
		for _, l := range strings.Split(out, "\n") {
			if w := visualLen(l); w > cols {
				t.Errorf("cols %d: line is %d wide: %q", cols, w, stripANSI(l))
			}
		}
		if !strings.Contains(stripANSI(out), "▸ DR-5241") {
			t.Errorf("cols %d: the selected row is gone:\n%s", cols, stripANSI(out))
		}
	}
	// The panel at a narrow width keeps the cursor of a long comment.
	st.handleKey("1", false, now)
	for _, r := range strings.Repeat("x", 60) + "END" {
		st.handleKey(string(r), false, now)
	}
	out := fleetRendered(st, 30, 25, now)
	if !strings.Contains(out, "END▌") {
		t.Errorf("long comment hides its tail:\n%s", out)
	}
}

func TestRenderFleetViewKeepsSelectionOnScreen(t *testing.T) {
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
	if !strings.Contains(out, "▸ DR-6029") {
		t.Errorf("selected last card is off screen:\n%s", out)
	}
	if n := len(strings.Split(out, "\n")); n != 15 {
		t.Errorf("frame has %d lines, want 15", n)
	}

	// A card taller than the screen scrolls inside with PgDn, and the scroll
	// is clamped so PgUp works at once.
	c := fleetTestCard("tall", "needs-you")
	c.Ticket = "DR-7000"
	for i := 0; i < 40; i++ {
		c.Summary += fmt.Sprintf("line %02d\n", i)
	}
	st = fleetTestState(fleetTestView(c), now)
	top := fleetRendered(st, 80, 15, now)
	if !strings.Contains(top, "▸ DR-7000") || strings.Contains(top, "line 20") {
		t.Errorf("tall card top:\n%s", top)
	}
	for i := 0; i < 20; i++ {
		st.handleKey(KeyPageDown, false, now)
	}
	bottom := fleetRendered(st, 80, 15, now)
	if !strings.Contains(bottom, "2  not-yet") || strings.Contains(bottom, "▸ DR-7000") {
		t.Errorf("tall card bottom:\n%s", bottom)
	}
	st.handleKey(KeyPageUp, false, now)
	if up := fleetRendered(st, 80, 15, now); up == bottom {
		t.Error("PgUp after over-scrolling does nothing")
	}
}

func TestRenderFleetViewToast(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	st := fleetTestState(fleetRenderView(now), now)
	st.setToast("the card changed", fleetToastErr, now)
	raw := renderFleetView(st, 100, 20, now)
	if !strings.Contains(raw, colorize("31", "the card changed")) {
		t.Errorf("error toast is not red: %q", raw)
	}
	if out := fleetRendered(st, 100, 20, now.Add(7*time.Second)); strings.Contains(out, "the card changed") {
		t.Error("an expired toast still shows")
	}
	st.sending = true
	if out := fleetRendered(st, 100, 20, now.Add(7*time.Second)); !strings.Contains(out, "sending…") {
		t.Errorf("a send in flight shows nothing:\n%s", out)
	}
}

func TestFleetJobWakes(t *testing.T) {
	j := startFleetJob(func() int { return 7 })
	w := j.wake()
	if w.fd < 0 || w.kind != wakeFleet {
		t.Fatalf("wake = %+v", w)
	}
	var set unix.FdSet
	set.Set(w.fd)
	tv := unix.NsecToTimeval(time.Second.Nanoseconds())
	n, err := unix.Select(w.fd+1, &set, nil, nil, &tv)
	if err != nil || n != 1 {
		t.Fatalf("select = %d, %v", n, err)
	}
	if v, ok := j.snapshot(); !ok || v != 7 {
		t.Fatalf("snapshot = %d, %v", v, ok)
	}
	j.close()
	j.close()
	if w := j.wake(); w.fd != -1 {
		t.Fatalf("wake after close = %+v", w)
	}
}

// waitFleetJobs waits until every job of l has finished.
func waitFleetJobs(t *testing.T, l *fleetLoop) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		running := false
		for _, j := range l.states {
			if _, ok := j.snapshot(); !ok {
				running = true
			}
		}
		for _, j := range l.sends {
			if _, ok := j.snapshot(); !ok {
				running = true
			}
		}
		for _, j := range l.dismisses {
			if _, ok := j.snapshot(); !ok {
				running = true
			}
		}
		if !running {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("jobs did not finish")
}

func TestFleetLoopDropsPollStartedBeforeSend(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	open := fleetTestView(fleetTestCard("a", "needs-you"), fleetTestCard("b", "needs-you"))
	answered := fleetTestView(fleetTestCard("a", "needs-you"), fleetTestCard("b", "needs-you"))
	answered.Answered["a"] = fleetAnswerWith(fleetAttemptSent)

	release := make(chan fleetStateView, 3)
	sendGo := make(chan struct{})
	l := &fleetLoop{
		st:       fleetTestState(open, now),
		interval: time.Hour,
		state:    func() (fleetStateView, error) { return <-release, nil },
		send: func(r fleetSayRequest) fleetSendResult {
			<-sendGo
			return fleetSendResult{CardID: r.CardID, Out: fleetSayOutcome{Status: 200, Result: fleetSayResult{OK: true}}}
		},
	}
	defer l.closeAll()

	// Open the panel on "a" and send.
	l.st.handleKey("1", false, now)
	if l.run(l.st.handleKey(KeyEnter, false, now.Add(time.Second)), now) {
		t.Fatal("send closed the view")
	}
	if l.st.send != nil {
		t.Fatal("the loop must clear st.send once it takes it")
	}
	// A poll starts while the send is in flight; it reads the card as open.
	l.startState()
	release <- open
	waitFleetJobs(t, &fleetLoop{states: l.states})
	close(sendGo)
	waitFleetJobs(t, l)
	l.reap(now)
	if l.st.sending {
		t.Fatal("the send result was not applied")
	}
	if l.st.nextAfter != "a" {
		t.Fatal("a poll started before the send landed was applied after it and took nextAfter")
	}
	// The poll started before the send landed is dropped, a fresh one runs.
	release <- answered
	waitFleetJobs(t, l)
	l.reap(now)
	if _, ok := l.st.view.Answered["a"]; !ok {
		t.Fatal("the fresh poll was not applied")
	}
	if l.st.nextAfter != "" || l.st.items[l.st.sel].Card.ID != "b" {
		t.Fatalf("nextAfter %q sel %d; the fresh poll should move to card b", l.st.nextAfter, l.st.sel)
	}
	if len(l.states)+len(l.sends) != 0 {
		t.Fatalf("finished jobs are kept: %d states, %d sends", len(l.states), len(l.sends))
	}
}

func TestFleetLoopDropsOlderPoll(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	first := fleetTestView(fleetTestCard("a", "needs-you"))
	second := fleetTestView(fleetTestCard("a", "needs-you"), fleetTestCard("b", "needs-you"))
	gates := []chan fleetStateView{make(chan fleetStateView, 1), make(chan fleetStateView, 1)}
	l := &fleetLoop{st: &fleetViewState{}, interval: time.Hour}
	l.state = func() (fleetStateView, error) { return <-gates[0], nil }
	l.startState()
	l.state = func() (fleetStateView, error) { return <-gates[1], nil }
	l.startState()
	gates[1] <- second
	for {
		if _, ok := l.states[1].snapshot(); ok {
			break
		}
		time.Sleep(time.Millisecond)
	}
	l.reap(now)
	gates[0] <- first
	waitFleetJobs(t, l)
	l.reap(now)
	if len(l.st.items) != 2 {
		t.Fatalf("an older poll overwrote a newer one: %d items", len(l.st.items))
	}
}

func TestFleetLoopPollsOnInterval(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	var n int
	l := &fleetLoop{st: &fleetViewState{}, interval: 10 * time.Second,
		state: func() (fleetStateView, error) { n++; return fleetTestView(), nil }}
	l.nextPoll = now.Add(10 * time.Second)
	l.tick(now)
	if len(l.states) != 0 {
		t.Fatal("polled before nextPoll")
	}
	l.tick(now.Add(10 * time.Second))
	if len(l.states) != 1 || !l.nextPoll.Equal(now.Add(20*time.Second)) {
		t.Fatalf("states %d nextPoll %v", len(l.states), l.nextPoll)
	}
	waitFleetJobs(t, l)
	l.tick(now.Add(time.Minute)) // finished but not reaped yet: still counts as running
	if len(l.states) != 1 {
		t.Fatal("a second poll started beside the first")
	}
	l.reap(now)
	if n != 1 || !l.st.loaded {
		t.Fatalf("calls %d loaded %v", n, l.st.loaded)
	}
}

func TestFleetLoopDismiss(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	v := fleetTestView(fleetTestCard("a", "needs-you"))
	v.Answered["a"] = fleetAnswerWith(fleetAttemptUnconfirmed)
	var got string
	l := &fleetLoop{st: fleetTestState(v, now), interval: time.Hour,
		state:   func() (fleetStateView, error) { return fleetTestView(fleetTestCard("a", "needs-you")), nil },
		dismiss: func(id string) (fleetSayResult, error) { got = id; return fleetSayResult{OK: true}, nil }}
	defer l.closeAll()
	if l.run(l.st.handleKey("x", false, now), now) {
		t.Fatal("dismiss closed the view")
	}
	if l.st.dismissID != "" {
		t.Fatal("the loop must clear st.dismissID once it takes it")
	}
	waitFleetJobs(t, l)
	l.reap(now)
	if got != "a" || l.st.toast != "dismissed" {
		t.Fatalf("dismissed %q toast %q", got, l.st.toast)
	}
	if len(l.states) != 1 {
		t.Fatal("no state fetch after a dismiss")
	}
}

func TestFleetDismissToast(t *testing.T) {
	cases := []struct {
		r    fleetDismissResult
		msg  string
		kind fleetToastKind
	}{
		{fleetDismissResult{Res: fleetSayResult{OK: true}}, "dismissed", fleetToastInfo},
		{fleetDismissResult{Res: fleetSayResult{Code: "not_found", Error: "no answer for that card"}}, "already gone", fleetToastInfo},
		{fleetDismissResult{Res: fleetSayResult{Code: "x", Error: "nope"}}, "nope", fleetToastErr},
		{fleetDismissResult{Res: fleetSayResult{Code: "x"}}, "x", fleetToastErr},
		{fleetDismissResult{Err: errors.New("dial: refused")}, "dial: refused", fleetToastErr},
	}
	for _, c := range cases {
		if msg, kind := fleetDismissToast(c.r); msg != c.msg || kind != c.kind {
			t.Errorf("%+v → %q %v, want %q %v", c.r, msg, kind, c.msg, c.kind)
		}
	}
}

func TestFleetLoopSendClientError(t *testing.T) {
	src := &fleetLocal{connect: func() (*fleetClient, fleetStateView, error) {
		return nil, fleetStateView{}, errors.New("no service")
	}}
	r := fleetSendVia(src)(fleetSayRequest{CardID: "a"})
	if r.CardID != "a" || r.CheckErr == nil {
		t.Fatalf("result = %+v", r)
	}
	res, err := fleetDismissVia(src)("a")
	if err == nil || res.OK {
		t.Fatalf("dismiss = %+v, %v", res, err)
	}
}

func TestFleetLoopTimeout(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	st := &fleetViewState{}
	if d := fleetLoopTimeout(st, now.Add(10*time.Second), now); d != 10*time.Second {
		t.Errorf("poll only: %v", d)
	}
	st.setToast("x", fleetToastInfo, now) // 3 s
	if d := fleetLoopTimeout(st, now.Add(10*time.Second), now); d != 3*time.Second {
		t.Errorf("toast: %v", d)
	}
	st.panel = &fleetPanel{openedAt: now.Add(-100 * time.Millisecond)}
	if d := fleetLoopTimeout(st, now.Add(10*time.Second), now); d != 300*time.Millisecond {
		t.Errorf("arming panel: %v", d)
	}
	st.panel.openedAt = now.Add(-time.Second)
	if d := fleetLoopTimeout(st, now.Add(10*time.Second), now); d != 3*time.Second {
		t.Errorf("armed panel: %v", d)
	}
	if d := fleetLoopTimeout(&fleetViewState{}, now.Add(-time.Second), now); d != time.Millisecond {
		t.Errorf("overdue poll: %v", d)
	}
}
