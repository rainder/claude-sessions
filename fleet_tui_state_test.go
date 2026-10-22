package main

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fleetTestCard is a decision card with two options; the first is "yes" →
// "Merge now".
func fleetTestCard(id, kind string) fleetCardView {
	return fleetCardView{ID: id, Kind: kind, Ticket: "DR-1", Title: "card " + id, Options: []fleetOptionView{
		{Key: "yes", Label: "Merge now", Send: "Merge now"},
		{Key: "not-yet", Label: "Hold", Send: "Hold"},
	}}
}

// fleetTestView is an alive FLEET with the given cards, each hashed "h-<id>".
func fleetTestView(cards ...fleetCardView) fleetStateView {
	v := fleetStateView{Fleet: fleetSession{Alive: true}, Cards: cards,
		CardHashes: map[string]string{}, Answered: map[string]fleetAnswer{}}
	for _, c := range cards {
		if c.ID != "" {
			v.CardHashes[c.ID] = "h-" + c.ID
		}
	}
	return v
}

func fleetAnswerWith(status string) fleetAnswer {
	return fleetAnswer{CardHash: "h", Attempts: []fleetAttempt{{OptionKey: "yes", Label: "Merge now", Status: status}}}
}

// fleetTestState is a view state that has applied v.
func fleetTestState(v fleetStateView, now time.Time) *fleetViewState {
	st := &fleetViewState{}
	st.applyState(v, nil, now)
	return st
}

func TestFleetViewDigitOpensPanel(t *testing.T) {
	now := time.Unix(1000, 0)
	t.Run("option 2", func(t *testing.T) {
		st := fleetTestState(fleetTestView(fleetTestCard("a", "needs-you")), now)
		if cmd := st.handleKey("2", false, now); cmd != fleetCmdRender {
			t.Fatalf("cmd = %v, want render", cmd)
		}
		p := st.panel
		if p == nil || p.opt != 1 || p.hash != "h-a" || p.correction || !p.openedAt.Equal(now) || p.item.Card.ID != "a" {
			t.Fatalf("panel = %+v", p)
		}
	})
	t.Run("id-less card", func(t *testing.T) {
		st := fleetTestState(fleetTestView(fleetTestCard("", "needs-you")), now)
		st.handleKey("1", false, now)
		if st.panel != nil {
			t.Fatal("an id-less card must not open a panel")
		}
	})
	t.Run("fleet not alive", func(t *testing.T) {
		v := fleetTestView(fleetTestCard("a", "needs-you"))
		v.Fleet.Alive = false
		st := fleetTestState(v, now)
		st.handleKey("1", false, now)
		if st.panel != nil {
			t.Fatal("no panel while FLEET is not alive")
		}
	})
	t.Run("empty send", func(t *testing.T) {
		c := fleetTestCard("a", "needs-you")
		c.Options[0].Send = ""
		st := fleetTestState(fleetTestView(c), now)
		st.handleKey("1", false, now)
		if st.panel != nil {
			t.Fatal("an option with no send must not open a panel")
		}
	})
	t.Run("digit past the options", func(t *testing.T) {
		st := fleetTestState(fleetTestView(fleetTestCard("a", "needs-you")), now)
		if cmd := st.handleKey("3", false, now); cmd != fleetCmdNone || st.panel != nil {
			t.Fatalf("cmd = %v panel = %+v", cmd, st.panel)
		}
	})
	t.Run("answered card, then c", func(t *testing.T) {
		v := fleetTestView(fleetTestCard("a", "needs-you"))
		v.Answered["a"] = fleetAnswerWith(fleetAttemptSent)
		st := fleetTestState(v, now)
		st.handleKey("1", false, now)
		if st.panel != nil {
			t.Fatal("an answered card must not open a panel")
		}
		if !strings.Contains(st.toast, "c") {
			t.Errorf("toast = %q, want a hint naming c", st.toast)
		}
		st.handleKey("c", false, now)
		if st.unlocked != "a" || st.toast != "pick the correction (1-9)" {
			t.Fatalf("unlocked = %q toast = %q", st.unlocked, st.toast)
		}
		st.handleKey("1", false, now)
		if st.panel == nil || !st.panel.correction || st.panel.opt != 0 {
			t.Fatalf("panel = %+v, want a correction panel", st.panel)
		}
	})
	t.Run("sending", func(t *testing.T) {
		st := fleetTestState(fleetTestView(fleetTestCard("a", "needs-you")), now)
		st.sending = true
		st.handleKey("1", false, now)
		if st.panel != nil {
			t.Fatal("no panel while an answer is sending")
		}
	})
}

// fleetOpenPanel opens option 1 of the first card at now.
func fleetOpenPanel(t *testing.T, now time.Time) *fleetViewState {
	t.Helper()
	st := fleetTestState(fleetTestView(fleetTestCard("a", "needs-you"), fleetTestCard("b", "needs-you")), now)
	st.handleKey("1", false, now)
	if st.panel == nil {
		t.Fatal("panel did not open")
	}
	return st
}

func TestFleetViewEnterArm(t *testing.T) {
	now := time.Unix(1000, 0)
	st := fleetOpenPanel(t, now)
	for _, k := range []string{"h", "i"} {
		st.handleKey(k, false, now)
	}
	if st.panel.comment != "hi" {
		t.Fatalf("comment = %q", st.panel.comment)
	}
	if cmd := st.handleKey(KeyEnter, false, now.Add(399*time.Millisecond)); cmd != fleetCmdNone || st.panel == nil || st.send != nil {
		t.Fatalf("early Enter: cmd = %v panel = %v send = %v", cmd, st.panel, st.send)
	}
	if cmd := st.handleKey(KeyEnter, false, now.Add(fleetArmDelay)); cmd != fleetCmdSend {
		t.Fatalf("armed Enter: cmd = %v", cmd)
	}
	want := fleetSayRequest{Text: "Merge now Andy adds: hi", CardID: "a", CardHash: "h-a", OptionKey: "yes", Correction: false}
	if st.send == nil || *st.send != want {
		t.Fatalf("send = %+v, want %+v", st.send, want)
	}
	if st.panel != nil || !st.sending || st.toast != "sending…" {
		t.Fatalf("panel = %v sending = %v toast = %q", st.panel, st.sending, st.toast)
	}
}

func TestFleetViewEnterInPasteBatchIgnored(t *testing.T) {
	now := time.Unix(1000, 0)
	st := fleetOpenPanel(t, now)
	if cmd := st.handleKey(KeyEnter, true, now.Add(time.Second)); cmd != fleetCmdNone || st.send != nil || st.panel == nil {
		t.Fatalf("batched Enter: cmd = %v send = %v panel = %v", cmd, st.send, st.panel)
	}
}

func TestFleetViewPanelEscAndEdit(t *testing.T) {
	now := time.Unix(1000, 0)
	st := fleetOpenPanel(t, now)
	// q is comment text inside the panel, not a close key.
	st.handleKey("q", false, now)
	st.handleKey("\x7f", false, now)
	st.handleKey("\x01", false, now)
	if st.panel == nil || st.panel.comment != "" {
		t.Fatalf("panel = %+v", st.panel)
	}
	if cmd := st.handleKey(KeyEsc, false, now); cmd != fleetCmdRender || st.panel != nil {
		t.Fatalf("Esc: cmd = %v panel = %v", cmd, st.panel)
	}
	st.handleKey("1", false, now)
	if cmd := st.handleKey("\x03", false, now); cmd != fleetCmdRender || st.panel != nil {
		t.Fatalf("Ctrl-C: cmd = %v panel = %v", cmd, st.panel)
	}
}

func TestFleetViewEscWhileSending(t *testing.T) {
	now := time.Unix(1000, 0)
	st := fleetTestState(fleetTestView(fleetTestCard("a", "needs-you")), now)
	st.sending = true
	for _, k := range []string{KeyEsc, "q", "Q", "\x03"} {
		st.toast = ""
		if cmd := st.handleKey(k, false, now); cmd == fleetCmdClose {
			t.Fatalf("%q closed the view while sending", k)
		}
		if st.toast != "the answer is still sending — wait for the result" || st.toastKind != fleetToastWarn {
			t.Errorf("%q: toast = %q kind = %v", k, st.toast, st.toastKind)
		}
	}
	st.sending = false
	st.unlocked = "a"
	if cmd := st.handleKey(KeyEsc, false, now); cmd == fleetCmdClose || st.unlocked != "" {
		t.Fatalf("first Esc: cmd = %v unlocked = %q", cmd, st.unlocked)
	}
	if cmd := st.handleKey(KeyEsc, false, now); cmd != fleetCmdClose {
		t.Fatalf("second Esc: cmd = %v", cmd)
	}
}

func TestFleetViewDismissKey(t *testing.T) {
	now := time.Unix(1000, 0)
	v := fleetTestView(fleetTestCard("a", "needs-you"), fleetTestCard("b", "needs-you"))
	v.Answered["a"] = fleetAnswerWith(fleetAttemptUnconfirmed)
	v.Answered["b"] = fleetAnswerWith(fleetAttemptSent)
	st := fleetTestState(v, now)
	if st.items[st.sel].Card.ID != "a" {
		t.Fatalf("selected %q, want a", st.items[st.sel].Card.ID)
	}
	if cmd := st.handleKey("x", false, now); cmd != fleetCmdDismiss || st.dismissID != "a" {
		t.Fatalf("x on unconfirmed: cmd = %v dismissID = %q", cmd, st.dismissID)
	}
	st.dismissID = ""
	st.handleKey(KeyDown, false, now)
	if st.items[st.sel].Card.ID != "b" {
		t.Fatalf("selected %q, want b", st.items[st.sel].Card.ID)
	}
	if cmd := st.handleKey("x", false, now); cmd != fleetCmdNone || st.dismissID != "" {
		t.Fatalf("x on sent: cmd = %v dismissID = %q", cmd, st.dismissID)
	}
}

func TestFleetViewMoveAndScroll(t *testing.T) {
	now := time.Unix(1000, 0)
	st := fleetTestState(fleetTestView(fleetTestCard("a", "needs-you"), fleetTestCard("b", "needs-you")), now)
	st.handleKey(KeyUp, false, now)
	if st.sel != 0 {
		t.Fatalf("sel = %d after Up at the top", st.sel)
	}
	st.handleKey(KeyPageDown, false, now)
	if st.scroll != fleetPageStep {
		t.Fatalf("scroll = %d after PgDn", st.scroll)
	}
	st.handleKey(KeyDown, false, now)
	if st.sel != 1 || st.scroll != 0 {
		t.Fatalf("sel = %d scroll = %d after Down", st.sel, st.scroll)
	}
	st.handleKey(KeyDown, false, now)
	if st.sel != 1 {
		t.Fatalf("sel = %d after Down at the bottom", st.sel)
	}
	st.handleKey(KeyPageUp, false, now)
	if st.scroll != 0 {
		t.Fatalf("scroll = %d after PgUp at 0", st.scroll)
	}
}

func TestFleetViewApplyStateClosesStalePanel(t *testing.T) {
	now := time.Unix(1000, 0)
	st := fleetOpenPanel(t, now)
	v := fleetTestView(fleetTestCard("a", "needs-you"), fleetTestCard("b", "needs-you"))
	v.CardHashes["a"] = "other"
	st.applyState(v, nil, now)
	if st.panel != nil || st.toast != "the card changed" || st.toastKind != fleetToastErr {
		t.Fatalf("panel = %v toast = %q kind = %v", st.panel, st.toast, st.toastKind)
	}
}

func TestFleetViewApplyStateClosesPanelAnsweredElsewhere(t *testing.T) {
	now := time.Unix(1000, 0)
	st := fleetOpenPanel(t, now)
	v := fleetTestView(fleetTestCard("a", "needs-you"), fleetTestCard("b", "needs-you"))
	v.Answered["a"] = fleetAnswerWith(fleetAttemptSent)
	st.applyState(v, nil, now)
	if st.panel != nil || st.toast != "already answered on another device" || st.toastKind != fleetToastErr {
		t.Fatalf("panel = %v toast = %q kind = %v", st.panel, st.toast, st.toastKind)
	}

	// A correction panel stays open when the answer list moves on.
	st.handleKey("c", false, now)
	st.handleKey("1", false, now)
	if st.panel == nil || !st.panel.correction {
		t.Fatalf("correction panel = %+v", st.panel)
	}
	v2 := fleetTestView(fleetTestCard("a", "needs-you"), fleetTestCard("b", "needs-you"))
	v2.Answered["a"] = fleetAnswer{CardHash: "h", Attempts: []fleetAttempt{
		{OptionKey: "yes", Status: fleetAttemptSent}, {OptionKey: "not-yet", Status: fleetAttemptSent}}}
	st.applyState(v2, nil, now)
	if st.panel == nil {
		t.Fatal("a correction panel must stay open")
	}
}

func TestFleetViewKeepsSelectionByID(t *testing.T) {
	now := time.Unix(1000, 0)
	st := fleetTestState(fleetTestView(fleetTestCard("a", "needs-you"), fleetTestCard("b", "needs-you")), now)
	st.handleKey(KeyDown, false, now)
	st.scroll = 5
	// A new card ahead of b: the selection follows b.
	st.applyState(fleetTestView(fleetTestCard("z", "explicit-yes"), fleetTestCard("a", "needs-you"), fleetTestCard("b", "needs-you")), nil, now)
	if st.items[st.sel].Card.ID != "b" || st.scroll != 5 {
		t.Fatalf("selected %q scroll %d, want b and the scroll kept", st.items[st.sel].Card.ID, st.scroll)
	}
	// b goes: the index stays within bounds.
	st.applyState(fleetTestView(fleetTestCard("a", "needs-you")), nil, now)
	if st.sel != 0 {
		t.Fatalf("sel = %d, want 0", st.sel)
	}
	// unlocked clears when its card has no entry.
	st.unlocked = "a"
	st.applyState(fleetTestView(fleetTestCard("a", "needs-you")), nil, now)
	if st.unlocked != "" {
		t.Fatalf("unlocked = %q, want cleared", st.unlocked)
	}
}

func TestFleetViewSelectsNextOpenAfterSend(t *testing.T) {
	now := time.Unix(1000, 0)
	cards := []fleetCardView{fleetTestCard("a", "needs-you"), fleetTestCard("b", "needs-you"), fleetTestCard("c", "needs-you")}
	st := fleetTestState(fleetTestView(cards...), now)
	st.handleKey("1", false, now)
	if cmd := st.handleKey(KeyEnter, false, now.Add(time.Second)); cmd != fleetCmdSend {
		t.Fatalf("cmd = %v", cmd)
	}
	refetch := st.applySend(fleetSendResult{CardID: "a", Out: fleetSayOutcome{Status: 200, Result: fleetSayResult{OK: true}}}, now)
	if !refetch || st.sending || st.nextAfter != "a" || st.toast != "sent" {
		t.Fatalf("refetch = %v sending = %v nextAfter = %q toast = %q", refetch, st.sending, st.nextAfter, st.toast)
	}
	v := fleetTestView(cards...)
	v.Answered["a"] = fleetAnswerWith(fleetAttemptSent)
	st.applyState(v, nil, now)
	if got := st.items[st.sel].Card.ID; got != "b" || st.nextAfter != "" {
		t.Fatalf("selected %q nextAfter %q, want b and cleared", got, st.nextAfter)
	}
}

func TestFleetViewServiceErrors(t *testing.T) {
	now := time.Unix(1000, 0)
	st := &fleetViewState{}
	st.applyState(fleetStateView{}, &fleetStatusError{Status: 401}, now)
	if st.err != "service token rejected" || !st.loaded {
		t.Fatalf("err = %q loaded = %v", st.err, st.loaded)
	}
	st.applyState(fleetStateView{}, errors.New("dial tcp: refused"), now)
	if !strings.Contains(st.err, "claude-sessions service restart") {
		t.Fatalf("err = %q", st.err)
	}
	st.applyState(fleetTestView(fleetTestCard("a", "needs-you")), nil, now)
	if st.err != "" || len(st.items) != 1 {
		t.Fatalf("err = %q items = %d", st.err, len(st.items))
	}
}

func TestFleetViewApplySendFailures(t *testing.T) {
	now := time.Unix(1000, 0)
	st := fleetTestState(fleetTestView(fleetTestCard("a", "needs-you")), now)
	st.sending = true
	if st.applySend(fleetSendResult{CardID: "a", CheckErr: errors.New("timeout\x1b[2J")}, now) {
		t.Error("a failed re-check must not refetch")
	}
	if st.sending || st.toast != "could not re-check the card: timeout" || st.toastKind != fleetToastErr {
		t.Fatalf("sending = %v toast = %q kind = %v", st.sending, st.toast, st.toastKind)
	}
	if !st.applySend(fleetSendResult{CardID: "a", Stale: true}, now) || st.toast != "the card changed" {
		t.Fatalf("stale: toast = %q", st.toast)
	}
	st.unlocked = "a"
	st.applySend(fleetSendResult{CardID: "a", Out: fleetSayOutcome{Unclear: true}}, now)
	if st.nextAfter != "" || st.unlocked != "a" || st.toastKind != fleetToastWarn {
		t.Fatalf("unclear: nextAfter = %q unlocked = %q kind = %v", st.nextAfter, st.unlocked, st.toastKind)
	}
	if !st.toastUntil.Equal(now.Add(6 * time.Second)) {
		t.Errorf("warn toast until %v, want +6s", st.toastUntil)
	}
	st.applySend(fleetSendResult{CardID: "a", Out: fleetSayOutcome{Status: 200, Result: fleetSayResult{OK: true}}}, now)
	if st.unlocked != "" || !st.toastUntil.Equal(now.Add(3*time.Second)) {
		t.Errorf("ok: unlocked = %q until = %v", st.unlocked, st.toastUntil)
	}
}

func TestFleetSendToast(t *testing.T) {
	res := func(ok bool, code, msg string) fleetSayResult { return fleetSayResult{OK: ok, Code: code, Error: msg} }
	cases := []struct {
		name    string
		o       fleetSayOutcome
		msg     string
		kind    fleetToastKind
		refetch bool
	}{
		{"unclear", fleetSayOutcome{Unclear: true}, "delivery not confirmed — check FLEET's chat", fleetToastWarn, false},
		{"401", fleetSayOutcome{Status: 401, Body: "Unauthorized"}, "service token rejected", fleetToastErr, false},
		{"ok", fleetSayOutcome{Status: 200, Result: res(true, "", "")}, "sent", fleetToastInfo, true},
		{"queued", fleetSayOutcome{Status: 200, Result: fleetSayResult{OK: true, Queued: true}}, "FLEET is busy — it reads your answer next", fleetToastInfo, true},
		{"not_submitted", fleetSayOutcome{Status: 200, Result: res(false, "not_submitted", "x")}, "typed but not submitted — check FLEET's pane", fleetToastWarn, true},
		{"already_answered", fleetSayOutcome{Status: 200, Result: res(false, "already_answered", "x")}, "already answered on another device", fleetToastErr, true},
		{"card_gone", fleetSayOutcome{Status: 200, Result: res(false, "card_gone", "x")}, "the card changed", fleetToastErr, true},
		{"card_changed", fleetSayOutcome{Status: 200, Result: res(false, "card_changed", "x")}, "the card changed", fleetToastErr, true},
		{"other code", fleetSayOutcome{Status: 200, Result: res(false, "busy", "FLEET is busy.")}, "FLEET is busy.", fleetToastErr, false},
		{"other code, no error text", fleetSayOutcome{Status: 200, Result: res(false, "busy", "")}, "busy", fleetToastErr, false},
		{"refusal, no code or text", fleetSayOutcome{Status: 200, Body: `{"ok":false}`}, "HTTP 200", fleetToastErr, false},
		{"400 error", fleetSayOutcome{Status: 400, Body: `{"ok":false}`, Result: res(false, "reserved_tag", "Reserved tag.")}, "Reserved tag.", fleetToastErr, false},
		{"400 body", fleetSayOutcome{Status: 400, Body: "bad text"}, "bad text", fleetToastErr, false},
		{"500 body", fleetSayOutcome{Status: 500, Body: "boom"}, "boom", fleetToastErr, false},
		{"502 empty", fleetSayOutcome{Status: 502}, "HTTP 502", fleetToastErr, false},
	}
	for _, c := range cases {
		msg, kind, refetch := fleetSendToast(c.o)
		if msg != c.msg || kind != c.kind || refetch != c.refetch {
			t.Errorf("%s: got (%q, %v, %v), want (%q, %v, %v)", c.name, msg, kind, refetch, c.msg, c.kind, c.refetch)
		}
	}
}

func TestFleetCardStillAsked(t *testing.T) {
	v := fleetTestView(fleetTestCard("a", "needs-you"), fleetTestCard("w", "working"))
	v.CardHashes["w"] = "h-w"
	cases := []struct {
		id, hash string
		want     bool
	}{
		{"a", "h-a", true},
		{"a", "h-x", false},
		{"gone", "h-a", false},
		{"w", "h-w", false},
		{"", "", false},
	}
	for _, c := range cases {
		if got := fleetCardStillAsked(v, c.id, c.hash); got != c.want {
			t.Errorf("fleetCardStillAsked(%q, %q) = %v, want %v", c.id, c.hash, got, c.want)
		}
	}
}

func TestFleetSendJob(t *testing.T) {
	var says, stateStatus atomic.Int32
	stateStatus.Store(200)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/fleet/state":
			if s := int(stateStatus.Load()); s != 200 {
				http.Error(w, "no", s)
				return
			}
			io.WriteString(w, `{"fleet":{"alive":true},"cards":[{"id":"a","kind":"needs-you","options":[{"key":"yes","send":"Merge now"}]}],"cardHashes":{"a":"h2"}}`)
		case "/fleet/say":
			says.Add(1)
			io.WriteString(w, `{"ok":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := newFleetClient(srv.URL, "tok")
	req := fleetSayRequest{Text: "Merge now", CardID: "a", CardHash: "h1", OptionKey: "yes"}

	r := fleetSendJob(c, req)
	if !r.Stale || r.CardID != "a" || says.Load() != 0 {
		t.Fatalf("hash h1: %+v says = %d", r, says.Load())
	}
	req.CardHash = "h2"
	r = fleetSendJob(c, req)
	if r.Stale || r.CheckErr != nil || !r.Out.Result.OK || says.Load() != 1 {
		t.Fatalf("hash h2: %+v says = %d", r, says.Load())
	}
	stateStatus.Store(500)
	r = fleetSendJob(c, req)
	if r.CheckErr == nil || says.Load() != 1 {
		t.Fatalf("state 500: %+v says = %d", r, says.Load())
	}
}

func TestFleetViewOwnAnswerLocksCardUntilStateShowsIt(t *testing.T) {
	now := time.Unix(1000, 0)
	cards := []fleetCardView{fleetTestCard("a", "needs-you"), fleetTestCard("b", "needs-you")}
	for _, out := range []fleetSayOutcome{
		{Status: 200, Result: fleetSayResult{OK: true}},
		{Status: 200, Result: fleetSayResult{Code: "not_submitted", Error: "x"}},
	} {
		st := fleetTestState(fleetTestView(cards...), now)
		st.applySend(fleetSendResult{CardID: "a", Out: out}, now)
		// The old list still shows a as open: a digit there must not open a
		// panel that would later read as answered elsewhere.
		st.sel = 0
		st.handleKey("1", false, now)
		if st.panel != nil {
			t.Fatalf("%+v: a digit on the card just sent opened a panel", out)
		}
		if st.toast != "sent — waiting for FLEET" {
			t.Errorf("%+v: toast = %q", out, st.toast)
		}
		// A poll that has not seen the answer yet keeps the lock.
		st.applyState(fleetTestView(cards...), nil, now)
		st.sel = fleetItemIndex(st.items, "a")
		st.handleKey("1", false, now)
		if st.panel != nil || st.justSent != "a" {
			t.Fatalf("%+v: panel = %v justSent = %q before the state shows the answer", out, st.panel, st.justSent)
		}
		// The state shows the answer: the lock goes, and no
		// "answered on another device" toast is shown for it.
		st.toast = ""
		v := fleetTestView(cards...)
		v.Answered["a"] = fleetAnswerWith(fleetAttemptSent)
		st.applyState(v, nil, now)
		if st.justSent != "" || st.toast == fleetToastAnswered {
			t.Fatalf("%+v: justSent = %q toast = %q", out, st.justSent, st.toast)
		}
	}
	// A panel open on the card we sent (not reachable through keys, but the
	// guard must hold) still closes, without the "another device" copy.
	{
		st := fleetTestState(fleetTestView(cards...), now)
		st.handleKey("1", false, now)
		st.justSent = "a"
		st.toast = ""
		v := fleetTestView(cards...)
		v.Answered["a"] = fleetAnswerWith(fleetAttemptSent)
		st.applyState(v, nil, now)
		if st.panel != nil || st.toast == fleetToastAnswered {
			t.Fatalf("own-answer panel: panel = %v toast = %q", st.panel, st.toast)
		}
	}
	// A card that is gone also ends the lock.
	st := fleetTestState(fleetTestView(cards...), now)
	st.applySend(fleetSendResult{CardID: "a", Out: fleetSayOutcome{Status: 200, Result: fleetSayResult{OK: true}}}, now)
	st.applyState(fleetTestView(cards[1]), nil, now)
	if st.justSent != "" {
		t.Fatalf("justSent = %q after the card left", st.justSent)
	}
}

func TestFleetViewDismissRefusedWhileSending(t *testing.T) {
	now := time.Unix(1000, 0)
	v := fleetTestView(fleetTestCard("a", "needs-you"))
	v.Answered["a"] = fleetAnswerWith(fleetAttemptUnconfirmed)
	st := fleetTestState(v, now)
	st.sending = true
	if cmd := st.handleKey("x", false, now); cmd != fleetCmdNone || st.dismissID != "" {
		t.Fatalf("x while sending: cmd = %v dismissID = %q", cmd, st.dismissID)
	}
}

func TestFleetViewDigitNeedsCardHash(t *testing.T) {
	now := time.Unix(1000, 0)
	for _, hash := range []*string{nil, new(string)} {
		v := fleetTestView(fleetTestCard("a", "needs-you"))
		delete(v.CardHashes, "a")
		if hash != nil {
			v.CardHashes["a"] = *hash
		}
		st := fleetTestState(v, now)
		if cmd := st.handleKey("1", false, now); cmd != fleetCmdNone || st.panel != nil {
			t.Fatalf("hash %v: cmd = %v panel = %+v", hash, cmd, st.panel)
		}
	}
}
