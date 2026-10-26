package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestFleetClientClarify(t *testing.T) {
	type call struct{ method, path, query, body string }
	var got []call
	reply, status := "", http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if r.Header.Get("Authorization") != "Bearer tok" || r.Header.Get("Cookie") != "" {
			t.Errorf("bad auth on %s", r.URL.Path)
		}
		got = append(got, call{r.Method, r.URL.Path, r.URL.Query().Get("card"), string(b)})
		w.WriteHeader(status)
		io.WriteString(w, reply)
	}))
	defer srv.Close()
	c := newFleetClient(srv.URL, "tok")

	reply = `{"card":"c1","tool":"","turns":[{"role":"andy","text":"why","at":"t"}],"pending":{"kind":"ask","startedAt":"s"},"draft":null}`
	v, err := c.ClarifyGet("c1")
	if err != nil || v.Card != "c1" || len(v.Turns) != 1 || v.Pending == nil || v.Pending.Kind != "ask" || v.Draft != nil {
		t.Fatalf("ClarifyGet = %+v, %v", v, err)
	}
	status, reply = http.StatusAccepted, `{"pending":{"kind":"ask","startedAt":"s"}}`
	if code, err := c.ClarifyAsk("c1", "why b"); code != "" || err != nil {
		t.Errorf("ask = %q, %v", code, err)
	}
	if code, err := c.ClarifyDraft("c1", false); code != "" || err != nil {
		t.Errorf("draft = %q, %v", code, err)
	}
	status, reply = http.StatusOK, `{"ok":true}`
	if code, err := c.ClarifyDraft("c1", true); code != "" || err != nil {
		t.Errorf("discard = %q, %v", code, err)
	}
	want := []call{
		{"GET", "/fleet/clarify", "c1", ""},
		{"POST", "/fleet/clarify/ask", "", `{"cardId":"c1","text":"why b"}`},
		{"POST", "/fleet/clarify/draft", "", `{"cardId":"c1"}`},
		{"POST", "/fleet/clarify/draft", "", `{"cardId":"c1","discard":true}`},
	}
	if len(got) != len(want) {
		t.Fatalf("calls = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("call %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	// A refusal is a code with a nil error; a bare status is an error.
	status, reply = http.StatusConflict, `{"code":"busy"}`
	if code, err := c.ClarifyAsk("c1", "x"); code != "busy" || err != nil {
		t.Errorf("busy = %q, %v", code, err)
	}
	status, reply = http.StatusNotFound, `{"code":"not_found"}`
	if code, err := c.ClarifyDraft("c1", true); code != "not_found" || err != nil {
		t.Errorf("not_found = %q, %v", code, err)
	}
	status, reply = http.StatusUnauthorized, `{"code":"unauthorized"}`
	if code, err := c.ClarifyAsk("c1", "x"); code != "" || fleetErrorStatus(err) != 401 {
		t.Errorf("401 = %q, %v", code, err)
	}
	status, reply = http.StatusBadGateway, `oops`
	if code, err := c.ClarifyDraft("c1", false); code != "" || fleetErrorStatus(err) != 502 {
		t.Errorf("502 = %q, %v", code, err)
	}
	if _, err := c.ClarifyGet("c1"); fleetErrorStatus(err) != 502 {
		t.Errorf("get 502 = %v", err)
	}
}

func TestClarifyViewSanitizes(t *testing.T) {
	v, err := decodeFleetClarifyView([]byte(`{"card":"c1","turns":[{"role":"fleet","text":"a\u001b[31mred\u001b]8;;x\u001b\\b","at":"t\u001b"}],` +
		`"pending":{"kind":"ask\u001b","startedAt":"s"},"draft":{"text":"d\u001b[0mone","at":"a"}}`))
	if err != nil {
		t.Fatal(err)
	}
	all := v.Turns[0].Text + v.Turns[0].At + v.Pending.Kind + v.Draft.Text
	if strings.Contains(all, "\x1b") || v.Turns[0].Text != "aredb" || v.Draft.Text != "done" {
		t.Errorf("not sanitized: %q %q", v.Turns[0].Text, v.Draft.Text)
	}
}

func TestStateViewDecodesClarify(t *testing.T) {
	v, err := decodeFleetStateView([]byte(`{"fleet":{"alive":true,"tool":"grok"},"cards":[],` +
		`"clarify":{"c1":{"turns":2,"pending":true,"draft":false},"c2":"junk"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if v.FleetTool != "grok" || v.Clarify["c1"] != (clarifySummary{Turns: 2, Pending: true}) || len(v.Clarify) != 1 {
		t.Errorf("FleetTool %q Clarify %+v", v.FleetTool, v.Clarify)
	}
}

// clarifyTestState is a state on an open card c1 (and a SENT card c2).
func clarifyTestState(now time.Time) *fleetViewState {
	v := fleetTestView(fleetTestCard("c1", "needs-you"), fleetTestCard("c2", "needs-you"))
	v.Source = "http://src"
	v.Answered["c2"] = fleetAnswerWith(fleetAttemptSent)
	return fleetTestState(v, now)
}

// clarifyOpen is a state with the chat open on c1 and its first read applied.
func clarifyOpen(now time.Time, v fleetClarifyView) *fleetViewState {
	st := clarifyTestState(now)
	st.handleKey("k", false, now)
	v.Card = "c1"
	st.applyClarify("c1", v, nil, now)
	return st
}

func TestClarifyKeyOpens(t *testing.T) {
	now := time.Now()
	st := clarifyTestState(now)
	st.setToast("old", fleetToastInfo, now)
	if cmd := st.handleKey("k", false, now); cmd != fleetCmdClarifyGet {
		t.Fatalf("k = %v", cmd)
	}
	if c := st.clarify; c == nil || c.item.Card.ID != "c1" || c.source != "http://src" || !c.openedAt.Equal(now) || st.toast != "" {
		t.Errorf("clarify = %+v toast %q", st.clarify, st.toast)
	}

	st = clarifyTestState(now)
	st.sel = fleetItemIndex(st.items, "c2")
	if cmd := st.handleKey("k", false, now); cmd != fleetCmdNone || st.clarify != nil {
		t.Errorf("k on SENT = %v %+v", cmd, st.clarify)
	}

	st = clarifyTestState(now)
	st.view.FleetTool = "grok"
	if cmd := st.handleKey("k", false, now); cmd != fleetCmdNone || st.clarify != nil {
		t.Errorf("k on grok = %v", cmd)
	}

	st = clarifyTestState(now)
	st.handleKey("o", false, now)
	if cmd := st.handleKey("k", false, now); st.clarify != nil || st.panel == nil || cmd == fleetCmdClarifyGet {
		t.Errorf("k with a panel open opened clarify")
	}
}

func TestClarifyKeyAskArmAndBatch(t *testing.T) {
	now := time.Now()
	st := clarifyOpen(now, fleetClarifyView{})
	for _, k := range "why" {
		st.handleKey(string(k), false, now)
	}
	if cmd := st.handleKey(KeyEnter, false, now.Add(100*time.Millisecond)); cmd != fleetCmdNone || st.clarifyReq != nil {
		t.Errorf("early Enter = %v", cmd)
	}
	st.handleKey(KeyEnter, true, now.Add(time.Second)) // a paste: a space
	st.handleKey("b", false, now)
	if st.clarify.input != "why b" {
		t.Fatalf("input = %q", st.clarify.input)
	}
	late := now.Add(time.Second)
	if cmd := st.handleKey(KeyEnter, false, late); cmd != fleetCmdClarifyAsk {
		t.Fatalf("Enter = %v", cmd)
	}
	if r := st.clarifyReq; r == nil || r.Text != "why b" || r.CardID != "c1" || r.Source != "http://src" || st.clarify.input != "" {
		t.Errorf("req = %+v input %q", st.clarifyReq, st.clarify.input)
	}

	// Blank input and a pending fork do nothing.
	if cmd := st.handleKey(KeyEnter, false, late); cmd != fleetCmdNone {
		t.Errorf("blank Enter = %v", cmd)
	}
	st.applyClarify("c1", fleetClarifyView{Card: "c1", Pending: &clarifyPending{Kind: "ask"}}, nil, late)
	st.handleKey("x", false, late)
	st.clarifyReq = nil
	if cmd := st.handleKey(KeyEnter, false, late); cmd != fleetCmdNone || st.clarifyReq != nil {
		t.Errorf("Enter while pending = %v", cmd)
	}
}

func TestClarifyKeyDraftAndScroll(t *testing.T) {
	now := time.Now()
	st := clarifyOpen(now, fleetClarifyView{})
	if cmd := st.handleKey("\x04", false, now); cmd != fleetCmdNone {
		t.Errorf("ctrl-d with no turns = %v", cmd)
	}
	st = clarifyOpen(now, fleetClarifyView{Turns: []clarifyTurn{{Role: "andy", Text: "q"}}})
	if cmd := st.handleKey("\x04", false, now); cmd != fleetCmdClarifyDraft {
		t.Errorf("ctrl-d = %v", cmd)
	}
	st.applyClarify("c1", fleetClarifyView{Card: "c1", Turns: []clarifyTurn{{}}, Pending: &clarifyPending{Kind: "draft"}}, nil, now)
	if cmd := st.handleKey("\x04", false, now); cmd != fleetCmdNone {
		t.Errorf("ctrl-d while pending = %v", cmd)
	}
	// scroll counts lines up from the newest: PgUp goes back in the chat.
	st.handleKey(KeyPageUp, false, now)
	if st.clarify.scroll != fleetPageStep {
		t.Errorf("scroll = %d", st.clarify.scroll)
	}
	st.handleKey(KeyPageDown, false, now)
	st.handleKey(KeyPageDown, false, now)
	if st.clarify.scroll != 0 {
		t.Errorf("scroll = %d", st.clarify.scroll)
	}
}

func TestClarifyDraftToOther(t *testing.T) {
	now := time.Now()
	draft := fleetClarifyView{Draft: &clarifyDraft{Text: "Pick B,\n only scans é"}}

	st := clarifyOpen(now, draft)
	if cmd := st.handleKey("s", false, now); cmd != fleetCmdRender {
		t.Fatalf("s = %v", cmd)
	}
	if st.clarify != nil || st.panel == nil || !st.panel.other || st.panel.comment != "Pick B, only scans é" || st.panel.item.Card.ID != "c1" {
		t.Errorf("clarify %v panel %+v", st.clarify, st.panel)
	}
	// Enter sends it through the Other path.
	if cmd := st.handleKey(KeyEnter, false, now.Add(time.Second)); cmd != fleetCmdSend ||
		st.send.Text != "Andy decides on c1: Pick B, only scans é" || st.send.CardID != "" {
		t.Errorf("send = %v %+v", cmd, st.send)
	}

	st = clarifyOpen(now, draft)
	if cmd := st.handleKey("x", false, now); cmd != fleetCmdClarifyDiscard {
		t.Errorf("x = %v", cmd)
	}

	// With text typed, s and x are letters.
	st = clarifyOpen(now, draft)
	st = clarifyOpen(now, draft)
	st.clarify.input = "s"
	st.handleKey("s", false, now)
	st.handleKey("x", false, now)
	if st.clarify == nil || st.clarify.input != "ssx" || st.panel != nil {
		t.Errorf("input = %+v", st.clarify)
	}

	// No draft: s and x are letters too.
	st = clarifyOpen(now, fleetClarifyView{})
	if cmd := st.handleKey("s", false, now); cmd != fleetCmdRender || st.clarify.input != "s" {
		t.Errorf("s without draft = %v %q", cmd, st.clarify.input)
	}

	// A long draft is cut to what a send may hold.
	st = clarifyOpen(now, fleetClarifyView{Draft: &clarifyDraft{Text: strings.Repeat("é", 5000)}})
	st.handleKey("s", false, now)
	if len(fleetOtherText("c1", st.panel.comment)) > sendKeysMaxLen {
		t.Errorf("comment %d bytes", len(st.panel.comment))
	}
}

func TestClarifyEscReturnsToList(t *testing.T) {
	now := time.Now()
	st := clarifyOpen(now, fleetClarifyView{})
	st.sel = 1
	for _, k := range []string{KeyEsc, "\x03"} {
		st.clarify = &fleetClarifyMode{item: st.items[1]}
		if cmd := st.handleKey(k, false, now); cmd != fleetCmdRender || st.clarify != nil || st.sel != 1 {
			t.Errorf("%q = %v clarify %v sel %d", k, cmd, st.clarify, st.sel)
		}
	}
	// q is a letter, never a close.
	st.clarify = &fleetClarifyMode{item: st.items[1]}
	if cmd := st.handleKey("q", false, now); cmd == fleetCmdClose || st.clarify.input != "q" {
		t.Errorf("q = %v", cmd)
	}
}

func TestClarifyClosesWhenCardGone(t *testing.T) {
	now := time.Now()
	st := clarifyOpen(now, fleetClarifyView{})
	same := fleetTestView(fleetTestCard("c1", "needs-you"))
	same.Source = "http://src"
	st.applyState(same, nil, now)
	if st.clarify == nil {
		t.Fatal("closed while the card is still there")
	}
	gone := fleetTestView(fleetTestCard("c2", "needs-you"))
	gone.Source = "http://src"
	st.applyState(gone, nil, now)
	if st.clarify != nil || st.toast != "card is gone" {
		t.Errorf("clarify %v toast %q", st.clarify, st.toast)
	}
}

func TestClarifyApplyAndRefusals(t *testing.T) {
	now := time.Now()
	st := clarifyOpen(now, fleetClarifyView{Turns: []clarifyTurn{{Text: "a"}}})
	if !st.clarify.loaded || len(st.clarify.view.Turns) != 1 {
		t.Fatalf("view not applied: %+v", st.clarify)
	}
	st.applyClarify("other", fleetClarifyView{Card: "other"}, nil, now) // another card's answer
	st.applyClarify("other", fleetClarifyView{}, &fleetStatusError{Status: 502}, now)
	if st.clarify.err != "" {
		t.Error("another card's error landed")
	}
	if len(st.clarify.view.Turns) != 1 {
		t.Error("a stale answer replaced the view")
	}
	st.applyClarify("c1", fleetClarifyView{}, &fleetStatusError{Status: 502}, now)
	if st.clarify.err == "" {
		t.Error("no error line")
	}
	st.applyClarify("c1", fleetClarifyView{Card: "c1"}, nil, now)
	if st.clarify.err != "" {
		t.Error("error line kept after a good read")
	}
	st.clarify = nil
	st.applyClarify("c1", fleetClarifyView{Card: "c1"}, nil, now) // no panic with the chat closed

	for code, want := range map[string]string{
		"busy": "a question is already running", "card_gone": "card is gone", "not_live": "FLEET is not running",
		"ambiguous": "two FLEETs are running", "unsupported_tool": "Clarify needs a Claude FLEET",
		"empty": "nothing to draft yet", "too_long": "question too long", "not_found": "no draft", "weird": "clarify failed",
	} {
		st.applyClarifyRefusal(code, now)
		if st.toast != want {
			t.Errorf("%s toast = %q, want %q", code, st.toast, want)
		}
	}
}

func TestClarifyDraftSourcePin(t *testing.T) {
	now := time.Now()
	draft := fleetClarifyView{Draft: &clarifyDraft{Text: "Pick B"}}
	st := clarifyOpen(now, draft)
	st.view.Source = "http://elsewhere"
	if cmd := st.handleKey("s", false, now); cmd != fleetCmdRender || st.clarify != nil || st.panel != nil || st.toast != errFleetMoved.Error() {
		t.Errorf("s after a move: %v clarify %v panel %v toast %q", cmd, st.clarify, st.panel, st.toast)
	}

	st = clarifyOpen(now, draft)
	v := fleetTestView(fleetTestCard("c1", "needs-you"))
	v.Source = "http://elsewhere"
	st.applyState(v, nil, now)
	if st.clarify != nil || st.toast != errFleetMoved.Error() {
		t.Errorf("applyState on another host: clarify %v toast %q", st.clarify, st.toast)
	}
}

func TestClarifyDraftToastCases(t *testing.T) {
	now := time.Now()
	draft := fleetClarifyView{Draft: &clarifyDraft{Text: "Pick B"}}

	card := fleetTestCard("c1", "needs-you")
	card.Options = nil
	v := fleetTestView(card)
	st := fleetTestState(v, now)
	st.handleKey("k", false, now)
	st.applyClarify("c1", fleetClarifyView{Card: "c1", Draft: draft.Draft}, nil, now)
	if cmd := st.handleKey("s", false, now); cmd != fleetCmdRender || st.clarify == nil || st.panel != nil || st.toast != "this card has no Other answer" {
		t.Errorf("no options: %v toast %q", cmd, st.toast)
	}

	// Options but no card hash: openOther refuses.
	st = clarifyOpen(now, draft)
	delete(st.view.CardHashes, "c1")
	if cmd := st.handleKey("s", false, now); cmd != fleetCmdRender || st.clarify == nil || st.panel != nil || st.toast != "cannot answer this card now" {
		t.Errorf("refused: %v toast %q", cmd, st.toast)
	}
}

func TestClarifyHidesShip(t *testing.T) {
	now := time.Now()
	st := clarifyOpen(now, fleetClarifyView{})
	st.applyState(fleetTestView(), nil, now) // no card left
	if st.clarify != nil {
		t.Fatal("chat stayed with the card gone")
	}
	st.clarify = &fleetClarifyMode{}
	if st.shipOn() {
		t.Error("ship over the chat")
	}
	st.clarify = nil
	if !st.shipOn() {
		t.Error("ship should show with no cards")
	}
}

// clarifyBusyView is a chat with three turns, an ask still running and a
// draft waiting.
func clarifyBusyView(now time.Time) fleetClarifyView {
	return fleetClarifyView{
		Turns: []clarifyTurn{
			{Role: clarifyRoleYou, Text: "why not merge now?"},
			{Role: clarifyRoleFleet, Text: "CI is still running on the **last** commit; merging now skips it."},
			{Role: clarifyRoleNote, Text: "the card changed"},
		},
		Pending: &clarifyPending{Kind: "ask", StartedAt: now.Add(-7 * time.Second).UTC().Format(time.RFC3339)},
		Draft:   &clarifyDraft{Text: "Hold until CI is green, then merge."},
	}
}

func TestFleetClarifyLinesLayout(t *testing.T) {
	now := time.Now()
	st := clarifyOpen(now, clarifyBusyView(now))
	lines := fleetClarifyLines(st, 80, 24, now)
	if len(lines) != 24 {
		t.Fatalf("%d lines, want 24", len(lines))
	}
	plain := make([]string, len(lines))
	for i, l := range lines {
		if w := visualLen(l); w > 80 {
			t.Errorf("line %d is %d wide: %q", i, w, stripANSI(l))
		}
		plain[i] = stripANSI(l)
	}
	if !strings.Contains(plain[0], "Clarify") || !strings.Contains(plain[0], "DR-1") || !strings.HasSuffix(plain[0], "fork · read-only") {
		t.Errorf("head = %q", plain[0])
	}
	if !strings.Contains(plain[1], "yes Merge now") || !strings.Contains(plain[1], "not-yet Hold") {
		t.Errorf("options = %q", plain[1])
	}
	all := strings.Join(plain, "\n")
	for _, want := range []string{"fleet thinking… 7s", "draft for FLEET", "Hold until CI is green", "you   why not merge now?", "fleet CI is still running on the last commit"} {
		if !strings.Contains(all, want) {
			t.Errorf("no %q in\n%s", want, all)
		}
	}
	if last := plain[23]; last != fleetHelpClarifyDraft {
		t.Errorf("help = %q", last)
	}
	if !strings.HasPrefix(plain[22], "› ") {
		t.Errorf("input = %q", plain[22])
	}
	// The draft box sits right above the input's rule.
	if !strings.HasPrefix(plain[20], "└") || !strings.HasPrefix(plain[21], "─") {
		t.Errorf("box bottom / rule = %q / %q", plain[20], plain[21])
	}
	// renderFleetView draws the chat in place of the list.
	if got := strings.Split(stripANSI(renderFleetView(st, 80, 24, now)), "\n"); len(got) != 24 || got[0] != plain[0] {
		t.Errorf("renderFleetView did not draw the chat: %q", got[0])
	}

	// Without a draft the help row has no draft keys; a toast wins.
	v := clarifyBusyView(now)
	v.Draft = nil
	st = clarifyOpen(now, v)
	if last := stripANSI(fleetClarifyLines(st, 80, 24, now)[23]); last != fleetHelpClarify {
		t.Errorf("help = %q", last)
	}
	st.setToast("question too long", fleetToastWarn, now)
	if last := stripANSI(fleetClarifyLines(st, 80, 24, now)[23]); last != "question too long" {
		t.Errorf("toast row = %q", last)
	}
}

func TestFleetClarifyLinesScroll(t *testing.T) {
	now := time.Now()
	v := fleetClarifyView{}
	for i := 0; i < 30; i++ {
		v.Turns = append(v.Turns, clarifyTurn{Role: clarifyRoleFleet, Text: fmt.Sprintf("answer %d", i)})
	}
	st := clarifyOpen(now, v)
	plain := func() []string {
		var out []string
		for _, l := range fleetClarifyLines(st, 60, 20, now) {
			out = append(out, stripANSI(l))
		}
		return out
	}
	p := plain()
	// Bottom-anchored: the newest turn is right above the input's rule.
	if p[16] != "fleet answer 29" || p[3] != "↑ more" {
		t.Fatalf("bottom: %q / %q", p[3], p[16])
	}
	st.handleKey(KeyPageUp, false, now)
	p = plain()
	if p[16] != "↓ more" || st.clarify.scroll == 0 {
		t.Errorf("after PgUp: %q scroll %d", p[16], st.clarify.scroll)
	}
	st.clarify.scroll = 1000 // clamped to the top
	p = plain()
	if p[3] != "fleet answer 0" || st.clarify.scroll == 1000 {
		t.Errorf("top: %q scroll %d", p[3], st.clarify.scroll)
	}
	// A short chat sits at the bottom with blank rows above it.
	st = clarifyOpen(now, fleetClarifyView{Turns: []clarifyTurn{{Role: clarifyRoleError, Text: "the fork failed"}}})
	p = plain()
	if p[3] != "" || p[16] != "!     the fork failed" {
		t.Errorf("short: %q / %q", p[3], p[16])
	}
}

func TestFleetClarifyLinesTiny(t *testing.T) {
	now := time.Now()
	for _, size := range [][2]int{{40, 6}, {40, 3}, {40, 2}, {10, 8}, {3, 10}} {
		st := clarifyOpen(now, clarifyBusyView(now))
		lines := fleetClarifyLines(st, size[0], size[1], now)
		if len(lines) != size[1] {
			t.Fatalf("%v: %d lines", size, len(lines))
		}
		for i, l := range lines {
			if w := visualLen(clipLine(l, size[0])); w > size[0] {
				t.Errorf("%v line %d is %d wide", size, i, w)
			}
		}
		last := stripANSI(lines[len(lines)-1])
		if !strings.HasPrefix(fleetHelpClarifyDraft, last) || last == "" && size[0] > 0 {
			t.Errorf("%v: help row = %q", size, last)
		}
		if size[1] >= 2 && !strings.HasPrefix(stripANSI(lines[len(lines)-2]), "›") {
			t.Errorf("%v: input row = %q", size, stripANSI(lines[len(lines)-2]))
		}
	}
	st := clarifyOpen(now, clarifyBusyView(now))
	lines := fleetClarifyLines(st, 40, 6, now)
	if !strings.Contains(stripANSI(lines[0]), "Clarify") {
		t.Errorf("head = %q", stripANSI(lines[0]))
	}
}

func TestFleetClarifyLinesWide(t *testing.T) {
	now := time.Now()
	st := clarifyOpen(now, fleetClarifyView{
		Turns: []clarifyTurn{{Role: clarifyRoleFleet, Text: strings.Repeat("⛵船", 40)}},
		Draft: &clarifyDraft{Text: strings.Repeat("船", 50)},
	})
	st.clarify.item.Card.Title = strings.Repeat("船", 50)
	for _, l := range fleetClarifyLines(st, 50, 16, now) {
		if w := visualLen(clipLine(l, 50)); w > 50 {
			t.Errorf("%d wide: %q", w, stripANSI(l))
		}
	}
	head := fleetClarifyLines(st, 50, 16, now)[0]
	if visualLen(head) > 50 || !strings.HasSuffix(stripANSI(head), "fork · read-only") {
		t.Errorf("head = %q (%d)", stripANSI(head), visualLen(head))
	}
	for _, l := range fleetClarifyLines(st, 50, 16, now) {
		if p := stripANSI(l); strings.HasPrefix(p, "│") && visualLen(l) != 50 {
			t.Errorf("box line %d wide: %q", visualLen(l), p)
		}
	}
}

// clarifyLoop is a loop on an open chat whose jobs answer through run.
func clarifyLoop(now time.Time, v fleetClarifyView, run func(fleetClarifyRequest, fleetCmd) fleetClarifyJobResult) *fleetLoop {
	return &fleetLoop{st: clarifyOpen(now, v), interval: time.Hour, clarify: run,
		state: func() (fleetStateView, error) { return fleetStateView{}, errors.New("no state in this test") }}
}

func TestFleetClarifyPollTimer(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	var kinds []fleetCmd
	run := func(r fleetClarifyRequest, k fleetCmd) fleetClarifyJobResult {
		kinds = append(kinds, k)
		return fleetClarifyJobResult{Kind: k, View: fleetClarifyView{Card: r.CardID}}
	}
	l := clarifyLoop(now, fleetClarifyView{Pending: &clarifyPending{Kind: "ask"}}, run)
	defer l.closeAll()
	l.nextPoll = now.Add(time.Hour)
	if d := l.timeout(now); d <= 0 || d > fleetClarifyPoll {
		t.Fatalf("timeout with a fork running = %v", d)
	}
	l.tick(now)
	if len(l.clarifies) != 0 {
		t.Fatal("polled before the 2 s were up")
	}
	l.tick(now.Add(fleetClarifyPoll))
	if len(l.clarifies) != 1 {
		t.Fatal("no clarify read after 2 s")
	}
	l.tick(now.Add(10 * fleetClarifyPoll)) // still running: no second read
	if len(l.clarifies) != 1 {
		t.Fatal("a second read started beside the first")
	}
	waitFleetJobs(t, l)
	l.reap(now.Add(fleetClarifyPoll))
	if len(kinds) != 1 || kinds[0] != fleetCmdClarifyGet || l.st.clarify.view.Pending != nil {
		t.Fatalf("kinds %v pending %+v", kinds, l.st.clarify.view.Pending)
	}

	// Nothing pending: no clarify read on any tick, and no clarify deadline.
	l.tick(now.Add(time.Minute))
	l.tick(now.Add(time.Hour - time.Second))
	if len(l.clarifies) != 0 || len(kinds) != 1 {
		t.Fatalf("polled with nothing pending: %v", kinds)
	}
	if d := l.timeout(now.Add(time.Minute)); d != time.Hour-time.Minute {
		t.Errorf("timeout without a fork = %v", d)
	}
}

func TestFleetClarifyLoopJobs(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	gate := make(chan struct{})
	var got []fleetClarifyRequest
	code := ""
	run := func(r fleetClarifyRequest, k fleetCmd) fleetClarifyJobResult {
		<-gate
		got = append(got, r)
		return fleetClarifyJobResult{Kind: k, Code: code, View: fleetClarifyView{Card: r.CardID,
			Turns: []clarifyTurn{{Role: clarifyRoleYou, Text: r.Text}}, Pending: &clarifyPending{Kind: "ask"}}}
	}
	l := clarifyLoop(now, fleetClarifyView{}, run)
	defer l.closeAll()
	late := now.Add(time.Second)
	for _, k := range "why" {
		l.st.handleKey(string(k), false, late)
	}
	l.run(l.st.handleKey(KeyEnter, false, late), late)
	if l.st.clarifyReq != nil || len(l.clarifies) != 1 {
		t.Fatalf("ask not taken: req %+v jobs %d", l.st.clarifyReq, len(l.clarifies))
	}
	// One job at a time: a second ask is refused and its text kept.
	for _, k := range "more" {
		l.st.handleKey(string(k), false, late)
	}
	l.run(l.st.handleKey(KeyEnter, false, late), late)
	if len(l.clarifies) != 1 || l.st.clarify.input != "more" {
		t.Fatalf("second ask: jobs %d input %q", len(l.clarifies), l.st.clarify.input)
	}
	close(gate)
	waitFleetJobs(t, l)
	l.reap(late)
	if len(got) != 1 || got[0] != (fleetClarifyRequest{Source: "http://src", CardID: "c1", Text: "why"}) {
		t.Fatalf("requests = %+v", got)
	}
	if c := l.st.clarify; !c.loaded || len(c.view.Turns) != 1 || c.view.Pending == nil {
		t.Fatalf("view = %+v", c.view)
	}

	// A refusal toasts and still shows the fresh view.
	l.st.clarify.view.Pending = nil
	code = "too_long"
	l.st.clarify.input = "x"
	l.run(l.st.handleKey(KeyEnter, false, late), late)
	waitFleetJobs(t, l)
	l.reap(late)
	if l.st.toast != "question too long" || l.st.clarify.view.Pending == nil {
		t.Fatalf("refusal: toast %q view %+v", l.st.toast, l.st.clarify.view)
	}

	// Moved: the chat closes with the toast and the state is fetched again.
	l.clarify = func(r fleetClarifyRequest, k fleetCmd) fleetClarifyJobResult {
		return fleetClarifyJobResult{Kind: k, Moved: true}
	}
	l.st.clarify.view.Pending = nil
	l.run(fleetCmdClarifyGet, late)
	waitFleetJobs(t, l)
	l.reap(late)
	if l.st.clarify != nil || l.st.toast != errFleetMoved.Error() || len(l.states) != 1 {
		t.Fatalf("moved: clarify %v toast %q states %d", l.st.clarify != nil, l.st.toast, len(l.states))
	}
}

// A result for a chat that has since moved to another card is dropped, and
// the open chat's skipped first read runs once the job is gone.
func TestFleetClarifyLoopDropsOtherCard(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	gate := make(chan struct{})
	var reads []string
	run := func(r fleetClarifyRequest, k fleetCmd) fleetClarifyJobResult {
		if r.CardID == "c1" {
			<-gate
		}
		reads = append(reads, r.CardID)
		return fleetClarifyJobResult{Kind: k, View: fleetClarifyView{Card: r.CardID,
			Turns: []clarifyTurn{{Role: clarifyRoleFleet, Text: "about " + r.CardID}}}}
	}
	v := fleetTestView(fleetTestCard("c1", "needs-you"), fleetTestCard("c3", "needs-you"))
	v.Source = "http://src"
	l := &fleetLoop{st: fleetTestState(v, now), interval: time.Hour, clarify: run}
	defer l.closeAll()
	l.run(l.st.handleKey("k", false, now), now) // chat on c1, its read held
	l.st.handleKey(KeyEsc, false, now)
	l.st.handleKey(KeyDown, false, now)
	l.run(l.st.handleKey("k", false, now), now) // chat on c3: skipped, c1's read runs
	if c := l.st.clarify; c == nil || c.item.Card.ID != "c3" || len(l.clarifies) != 1 {
		t.Fatalf("clarify %+v jobs %d", l.st.clarify, len(l.clarifies))
	}
	close(gate)
	waitFleetJobs(t, l)
	l.reap(now)
	if l.st.clarify.loaded {
		t.Fatal("c1's chat was shown on c3")
	}
	waitFleetJobs(t, l)
	l.reap(now)
	if c := l.st.clarify; !c.loaded || c.view.Turns[0].Text != "about c3" || len(reads) != 2 {
		t.Fatalf("view %+v reads %v", c.view, reads)
	}
}

func TestFleetClarifyViaMoved(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/fleet/state":
			io.WriteString(w, `{"fleet":{"alive":true},"cards":[]}`)
		case "/fleet/clarify":
			calls.Add(1)
			io.WriteString(w, `{"card":"c1","turns":[{"role":"fleet","text":"hi","at":"t"}]}`)
		default:
			calls.Add(1)
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	defer srv.Close()
	src := &fleetLocal{connect: func() (*fleetClient, fleetStateView, error) {
		c := newFleetClient(srv.URL, "tok")
		v, err := c.GetState()
		return c, v, err
	}}
	via := fleetClarifyVia(src)
	for _, k := range []fleetCmd{fleetCmdClarifyGet, fleetCmdClarifyAsk, fleetCmdClarifyDraft, fleetCmdClarifyDiscard} {
		if r := via(fleetClarifyRequest{Source: "http://elsewhere", CardID: "c1", Text: "q"}, k); !r.Moved || r.Kind != k {
			t.Errorf("kind %v: %+v, want Moved", k, r)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("a pinned request reached the host: %d calls", calls.Load())
	}
	r := via(fleetClarifyRequest{Source: srv.URL, CardID: "c1", Text: "q"}, fleetCmdClarifyAsk)
	if r.Moved || r.Err != nil || r.Code != "" || len(r.View.Turns) != 1 || calls.Load() != 2 {
		t.Fatalf("same host: %+v calls %d", r, calls.Load())
	}
}

func TestFleetClarifyMarkerAndHint(t *testing.T) {
	now := time.Now()
	st := clarifyTestState(now)
	st.view.Clarify = map[string]clarifySummary{"c1": {Turns: 2}}
	list, _, _ := fleetListLines(st, 60, now)
	var row string
	for _, l := range list {
		if strings.Contains(stripANSI(l), "card c1") {
			row = l
		}
	}
	if !strings.Contains(stripANSI(row), "card c1 ✎") || visualLen(row) > 60 {
		t.Errorf("row with a chat = %q", stripANSI(row))
	}
	st.view.Clarify = nil
	list, _, _ = fleetListLines(st, 60, now)
	if strings.Contains(stripANSI(strings.Join(list, "\n")), "✎") {
		t.Error("marker without a chat")
	}

	if !st.canClarify() || !strings.Contains(stripANSI(fleetBottomRow(st, now)), "k clarify") {
		t.Errorf("hint missing: %q", stripANSI(fleetBottomRow(st, now)))
	}
	st.view.FleetTool = "grok"
	if st.canClarify() || strings.Contains(stripANSI(fleetBottomRow(st, now)), "k clarify") {
		t.Errorf("hint on a grok FLEET: %q", stripANSI(fleetBottomRow(st, now)))
	}
}

func TestFleetClarifyWheel(t *testing.T) {
	now := time.Now()
	st := clarifyOpen(now, fleetClarifyView{})
	if cmd := st.handleMouse(mouseEvent{button: mouseWheelUp}); cmd != fleetCmdRender || st.clarify.scroll != fleetMouseStep {
		t.Errorf("wheel up: %v scroll %d", cmd, st.clarify.scroll)
	}
	st.handleMouse(mouseEvent{button: mouseWheelDown})
	st.handleMouse(mouseEvent{button: mouseWheelDown})
	if st.clarify.scroll != 0 {
		t.Errorf("scroll = %d", st.clarify.scroll)
	}
	if cmd := st.handleMouse(mouseEvent{button: mouseLeft}); cmd != fleetCmdNone {
		t.Errorf("click = %v", cmd)
	}
}
