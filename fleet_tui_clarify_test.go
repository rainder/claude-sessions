package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
	st.applyClarify(v, nil, now)
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
	st.applyClarify(fleetClarifyView{Card: "c1", Pending: &clarifyPending{Kind: "ask"}}, nil, late)
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
	st.applyClarify(fleetClarifyView{Card: "c1", Turns: []clarifyTurn{{}}, Pending: &clarifyPending{Kind: "draft"}}, nil, now)
	if cmd := st.handleKey("\x04", false, now); cmd != fleetCmdNone {
		t.Errorf("ctrl-d while pending = %v", cmd)
	}
	st.handleKey(KeyPageDown, false, now)
	if st.clarify.scroll != fleetPageStep {
		t.Errorf("scroll = %d", st.clarify.scroll)
	}
	st.handleKey(KeyPageUp, false, now)
	st.handleKey(KeyPageUp, false, now)
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
	st.applyState(fleetTestView(fleetTestCard("c1", "needs-you")), nil, now)
	if st.clarify == nil {
		t.Fatal("closed while the card is still there")
	}
	st.applyState(fleetTestView(fleetTestCard("c2", "needs-you")), nil, now)
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
	st.applyClarify(fleetClarifyView{Card: "other"}, nil, now) // another card's answer
	if len(st.clarify.view.Turns) != 1 {
		t.Error("a stale answer replaced the view")
	}
	st.applyClarify(fleetClarifyView{}, &fleetStatusError{Status: 502}, now)
	if st.clarify.err == "" {
		t.Error("no error line")
	}
	st.applyClarify(fleetClarifyView{Card: "c1"}, nil, now)
	if st.clarify.err != "" {
		t.Error("error line kept after a good read")
	}
	st.clarify = nil
	st.applyClarify(fleetClarifyView{Card: "c1"}, nil, now) // no panic with the chat closed

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
