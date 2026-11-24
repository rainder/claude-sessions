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

func TestFleetClientChatGet(t *testing.T) {
	var queries []string
	reply := `{"turns":[{"uuid":"u1","role":"assistant","at":"t","text":"hi \u001b]8;;http://x\u0007there\u001b[31m"}],` +
		`"pos":42,"reset":true,"sessionId":"s1","readOnly":true,"reason":"why\u001b[2J"}`
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/fleet/chat" || r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("bad request %s %s", r.Method, r.URL.Path)
		}
		queries = append(queries, r.URL.RawQuery)
		w.WriteHeader(status)
		io.WriteString(w, reply)
	}))
	defer srv.Close()
	c := newFleetClient(srv.URL, "tok")
	v, err := c.ChatGet(0, 100)
	if err != nil || v.Pos != 42 || !v.Reset || v.SessionID != "s1" || !v.ReadOnly || len(v.Turns) != 1 {
		t.Fatalf("ChatGet = %+v, %v", v, err)
	}
	// Strangers' text reaches the chat: no ESC or BEL survives.
	if strings.ContainsAny(v.Turns[0].Text+v.Reason, "\x1b\x07") {
		t.Fatalf("unsanitized: %q %q", v.Turns[0].Text, v.Reason)
	}
	if _, err := c.ChatGet(7, 100); err != nil {
		t.Fatal(err)
	}
	if strings.Join(queries, "|") != "n=100|n=100&since=7" {
		t.Fatalf("queries %q", queries)
	}
	for _, bad := range []string{`null`, `[]`, `{"pos":-1}`, `nope`} {
		reply = bad
		if _, err := c.ChatGet(0, 100); err == nil {
			t.Errorf("%s decoded", bad)
		}
	}
	status = http.StatusUnauthorized
	if _, err := c.ChatGet(0, 100); !errors.Is(err, errFleetUnauthorized) {
		t.Fatalf("401 = %v", err)
	}
}

func chatTestState(now time.Time) *fleetViewState {
	v := fleetTestView(fleetTestCard("c1", "needs-you"))
	v.Source = "http://src"
	return fleetTestState(v, now)
}

// chatOpen is a state with the chat open and a first read of turns applied.
func chatOpen(now time.Time, turns ...fleetChatTurn) *fleetViewState {
	st := chatTestState(now)
	st.handleKey("i", false, now)
	st.applyChat(fleetChatFetch{chat: st.chat, view: fleetChatView{SessionID: "s1", Pos: 10, Turns: turns}}, now)
	return st
}

func TestChatKeyOpens(t *testing.T) {
	now := time.Now()
	st := chatTestState(now)
	st.setToast("old", fleetToastInfo, now)
	if cmd := st.handleKey("i", false, now); cmd != fleetCmdChatGet {
		t.Fatalf("i = %v", cmd)
	}
	if c := st.chat; c == nil || c.source != "http://src" || st.toast != "" || st.chatDraft.maxBytes != sendKeysMaxLen {
		t.Fatalf("chat = %+v toast %q", st.chat, st.toast)
	}
	if st.shipOn() {
		t.Error("ship over the chat")
	}

	// Never over another panel, a batch or Clarify.
	st = chatTestState(now)
	st.handleKey("o", false, now)
	if cmd := st.handleKey("i", false, now); st.chat != nil || cmd == fleetCmdChatGet {
		t.Error("i with a panel open opened the chat")
	}
	st = chatTestState(now)
	st.handleKey("k", false, now)
	if cmd := st.handleKey("i", false, now); st.chat != nil || cmd == fleetCmdChatGet {
		t.Error("i in Clarify opened the chat")
	}
	v := fleetTestView()
	v.Source = "http://src"
	st = fleetTestState(v, now)
	if !st.shipOn() {
		t.Fatal("test setup: no ship mode")
	}
	// The ship screen (no card needs Andy) is where the chat is wanted most.
	if cmd := st.handleKey("i", false, now); st.chat == nil || cmd != fleetCmdChatGet {
		t.Error("i on the ship screen did not open the chat")
	}
	if st.shipOn() {
		t.Error("ship over the chat")
	}
	st = chatTestState(now)
	st.view.Ambiguous = []string{"a", "b"}
	if st.handleKey("i", false, now); st.chat != nil {
		t.Error("i with two FLEETs opened the chat")
	}

	// FLEET down: the chat opens read-only and refuses to send.
	v = fleetTestView(fleetTestCard("c1", "needs-you"))
	v.Source, v.Fleet.Alive = "http://src", false
	st = fleetTestState(v, now)
	if st.handleKey("i", false, now); st.chat == nil {
		t.Fatal("chat did not open with FLEET down")
	}
	st.chatDraft.SetText("hi")
	if cmd := st.handleKey(KeyEnter, false, now.Add(time.Second)); cmd == fleetCmdChatSend || st.chatSend != nil || st.toast != "FLEET is not running" {
		t.Fatalf("send with FLEET down: %v %+v %q", cmd, st.chatSend, st.toast)
	}
	if !strings.Contains(fleetChatHead(st, 80), "not running") {
		t.Errorf("head = %q", fleetChatHead(st, 80))
	}
}

func TestChatComposerKeys(t *testing.T) {
	now := time.Now()
	st := chatOpen(now)
	late := now.Add(time.Second)
	for _, k := range []string{"o", "n", "e", KeyAltEnter, "t", "w", "o"} {
		st.handleKey(k, false, late)
	}
	// q, k, digits are text in the composer, not list keys.
	for _, k := range []string{" ", "q", "k", "1"} {
		if cmd := st.handleKey(k, false, late); cmd != fleetCmdRender || st.chat == nil {
			t.Fatalf("%q = %v", k, cmd)
		}
	}
	if st.chatDraft.Text() != "one\ntwo qk1" {
		t.Fatalf("draft %q", st.chatDraft.Text())
	}
	// A paste's Enter is a line break.
	st.handleKey(KeyEnter, true, late)
	if st.chatSend != nil || st.chatDraft.Text() != "one\ntwo qk1\n" {
		t.Fatalf("paste Enter: %+v %q", st.chatSend, st.chatDraft.Text())
	}
	// Enter right after opening does nothing.
	st.chat.openedAt = late
	if cmd := st.handleKey(KeyEnter, false, late); cmd != fleetCmdNone || st.chatSend != nil {
		t.Fatalf("Enter in the arm delay = %v", cmd)
	}
	if cmd := st.handleKey(KeyEnter, false, late.Add(time.Second)); cmd != fleetCmdChatSend {
		t.Fatalf("Enter = %v", cmd)
	}
	if r := st.chatSend; r == nil || *r != (fleetSayRequest{Source: "http://src", Text: "one\ntwo qk1\n"}) || !st.chatSending {
		t.Fatalf("request %+v", st.chatSend)
	}
	// A second Enter while sending is refused.
	st.chatSend = nil
	if st.handleKey(KeyEnter, false, late.Add(time.Second)); st.chatSend != nil {
		t.Fatal("second send while sending")
	}
	// Esc closes and keeps the draft for the next open.
	st.chatSending = false
	st.handleKey(KeyEsc, false, late)
	if st.chat != nil {
		t.Fatal("Esc did not close")
	}
	st.handleKey("i", false, late)
	if st.chat == nil || st.chatDraft.Text() != "one\ntwo qk1\n" {
		t.Fatalf("draft after reopen %q", st.chatDraft.Text())
	}
}

func TestChatApplyReads(t *testing.T) {
	now := time.Now()
	turn := func(id, role, text string) fleetChatTurn { return fleetChatTurn{UUID: id, Role: role, Text: text} }
	st := chatOpen(now, turn("a", "user", "q"), turn("b", "assistant", "partial"))
	c := st.chat
	if !c.loaded || c.pos != 10 || len(c.turns) != 2 {
		t.Fatalf("chat %+v", c)
	}
	// An incremental read updates a turn in place and appends new ones.
	st.applyChat(fleetChatFetch{chat: c, view: fleetChatView{SessionID: "s1", Pos: 20,
		Turns: []fleetChatTurn{turn("b", "assistant", "full"), turn("c", "user", "next"), turn("", "user", "no id")}}}, now)
	if len(c.turns) != 3 || c.turns[1].Text != "full" || c.turns[2].UUID != "c" || c.pos != 20 {
		t.Fatalf("turns %+v pos %d", c.turns, c.pos)
	}
	// A read for a chat closed since is dropped.
	st.applyChat(fleetChatFetch{chat: &fleetChatMode{}, view: fleetChatView{SessionID: "s1", Turns: []fleetChatTurn{turn("x", "user", "x")}}}, now)
	if len(c.turns) != 3 {
		t.Fatal("a stale read was applied")
	}
	// No session id: a discovery blip, the chat stays.
	st.applyChat(fleetChatFetch{chat: c, view: fleetChatView{Pos: 0}}, now)
	if len(c.turns) != 3 || c.pos != 20 {
		t.Fatalf("blip: %+v", c)
	}
	// Reset replaces the turns.
	st.applyChat(fleetChatFetch{chat: c, view: fleetChatView{SessionID: "s1", Pos: 5, Reset: true, Turns: []fleetChatTurn{turn("z", "user", "z")}}}, now)
	if len(c.turns) != 1 || c.pos != 5 {
		t.Fatalf("reset: %+v", c.turns)
	}
	// Another session starts over from its tail.
	st.applyChat(fleetChatFetch{chat: c, view: fleetChatView{SessionID: "s2", Pos: 99, Turns: []fleetChatTurn{turn("n", "user", "n")}}}, now)
	if len(c.turns) != 0 || c.pos != 0 || c.sid != "s2" || !c.again {
		t.Fatalf("new session: %+v", c)
	}
	// At most fleetChatKeep turns.
	var many []fleetChatTurn
	for i := range fleetChatKeep + 5 {
		many = append(many, turn(strings.Repeat("u", i+1), "user", "t"))
	}
	st.applyChat(fleetChatFetch{chat: c, view: fleetChatView{SessionID: "s2", Pos: 1, Turns: many}}, now)
	if len(c.turns) != fleetChatKeep || c.turns[0].UUID != many[5].UUID {
		t.Fatalf("kept %d first %q", len(c.turns), c.turns[0].UUID)
	}
	// Errors keep the turns and show a fixed line.
	st.applyChat(fleetChatFetch{chat: c, err: &fleetStatusError{Status: 401}}, now)
	if c.err != fleetToastTokenBad || len(c.turns) != fleetChatKeep {
		t.Fatalf("401: %q", c.err)
	}
	st.applyChat(fleetChatFetch{chat: c, err: errors.New("dial tcp 10.0.0.1: refused")}, now)
	if c.err != "could not read FLEET's chat" {
		t.Fatalf("err %q", c.err)
	}
	// Moved closes the chat.
	st.applyChat(fleetChatFetch{chat: c, moved: true}, now)
	if st.chat != nil || st.toast != errFleetMoved.Error() {
		t.Fatalf("moved: %v %q", st.chat != nil, st.toast)
	}
}

func TestChatApplySend(t *testing.T) {
	now := time.Now()
	ok := func(r fleetSayResult) fleetSendResult {
		return fleetSendResult{Out: fleetSayOutcome{Status: 200, Result: r}}
	}
	for name, tc := range map[string]struct {
		res   fleetSendResult
		toast string
		clear bool
	}{
		"sent":    {ok(fleetSayResult{OK: true}), fleetToastSent, true},
		"queued":  {ok(fleetSayResult{OK: true, Queued: true}), "FLEET is busy — it reads your message next", true},
		"held":    {ok(fleetSayResult{OK: true, Held: true}), "FLEET is busy — the message waits in the outbox", true},
		"refused": {ok(fleetSayResult{Code: "not_submitted"}), "typed but not submitted — check FLEET's pane", false},
		"400":     {fleetSendResult{Out: fleetSayOutcome{Status: 400, Body: "text must not start with ! or #"}}, "text must not start with ! or #", false},
		"no conn": {fleetSendResult{CheckErr: errors.New("x")}, "could not reach FLEET's service", false},
	} {
		st := chatOpen(now)
		st.chatDraft.SetText("hello\nthere")
		st.chatSending = true
		st.applyChatSend(fleetChatSent{text: "hello\nthere", res: tc.res}, now)
		if st.chatSending || st.toast != tc.toast || (st.chatDraft.Text() == "") != tc.clear {
			t.Errorf("%s: sending %v toast %q draft %q", name, st.chatSending, st.toast, st.chatDraft.Text())
		}
	}
	// A draft edited while the send ran stays.
	st := chatOpen(now)
	st.chatDraft.SetText("hello more")
	st.applyChatSend(fleetChatSent{text: "hello", res: ok(fleetSayResult{OK: true})}, now)
	if st.chatDraft.Text() != "hello more" {
		t.Fatalf("draft %q", st.chatDraft.Text())
	}
	st.applyChatSend(fleetChatSent{text: "x", res: fleetSendResult{Moved: true}}, now)
	if st.chat != nil {
		t.Fatal("moved send left the chat open")
	}
}

func TestChatClosesWhenFleetMoves(t *testing.T) {
	now := time.Now()
	st := chatOpen(now)
	v := fleetTestView(fleetTestCard("c1", "needs-you"))
	v.Source = "http://other"
	st.applyState(v, nil, now)
	if st.chat != nil || st.toast != errFleetMoved.Error() {
		t.Fatalf("chat %v toast %q", st.chat != nil, st.toast)
	}
}

func TestFleetChatLinesRenderAndSanitize(t *testing.T) {
	now := time.Now()
	evil := fleetDisplayText("look \x1b]52;c;ZXZpbA==\x07here \x1b[2J**bold**")
	st := chatOpen(now,
		fleetChatTurn{UUID: "a", Role: "user", Text: "first\nsecond"},
		fleetChatTurn{UUID: "b", Role: "assistant", Text: evil})
	st.chatDraft.SetText("draft one\ndraft two")
	lines := fleetChatLines(st, 60, 20, now)
	if len(lines) != 20 {
		t.Fatalf("%d lines", len(lines))
	}
	all := strings.Join(lines, "\n")
	for _, l := range lines {
		if visualLen(l) > 60 {
			t.Errorf("line wider than 60: %q", l)
		}
	}
	plain := stripANSI(all)
	for _, want := range []string{"Chat with FLEET", "you", "first", "second", "fleet", "look", "› draft one", "  draft two", "Enter send"} {
		if !strings.Contains(plain, want) {
			t.Errorf("missing %q in\n%s", want, plain)
		}
	}
	for _, bad := range []string{"\x1b]", "\x1b[2J", "\x07"} {
		if strings.Contains(all, bad) {
			t.Errorf("unsafe %q drawn", bad)
		}
	}
	// The composer sits right above the help row; the log fills the rest.
	if !strings.HasPrefix(stripANSI(lines[18]), "  draft two") || !strings.HasPrefix(stripANSI(lines[19]), "Enter send") {
		t.Fatalf("bottom = %q / %q", stripANSI(lines[18]), stripANSI(lines[19]))
	}
	if wide := fleetChatLines(st, 80, 20, now); !strings.HasSuffix(stripANSI(wide[19]), "Esc close") {
		t.Errorf("help at 80 = %q", stripANSI(wide[19]))
	}
	// A short screen still draws the composer and help, never more rows.
	if lines := fleetChatLines(st, 60, 4, now); len(lines) != 4 || !strings.Contains(stripANSI(lines[3]), "Enter send") {
		t.Fatalf("tiny = %q", lines)
	}
	// Before the first read: loading; with no turns: the hint.
	st.handleKey(KeyEsc, false, now)
	st.handleKey("i", false, now)
	if !strings.Contains(stripANSI(strings.Join(fleetChatLines(st, 60, 20, now), "\n")), "loading") {
		t.Fatal("no loading line")
	}
	st.applyChat(fleetChatFetch{chat: st.chat, view: fleetChatView{SessionID: "s1"}}, now)
	if !strings.Contains(stripANSI(strings.Join(fleetChatLines(st, 60, 20, now), "\n")), "No messages yet") {
		t.Fatal("no empty hint")
	}
}

func TestFleetChatScroll(t *testing.T) {
	now := time.Now()
	var turns []fleetChatTurn
	for i := range 30 {
		turns = append(turns, fleetChatTurn{UUID: strings.Repeat("x", i+1), Role: "assistant", Text: "line"})
	}
	st := chatOpen(now, turns...)
	fleetChatLines(st, 60, 20, now)
	if cmd := st.handleKey(KeyPageUp, false, now); cmd != fleetCmdRender || st.chat.scroll == 0 {
		t.Fatalf("PgUp: %v scroll %d", cmd, st.chat.scroll)
	}
	st.handleKey(KeyPageDown, false, now)
	st.handleKey(KeyPageDown, false, now)
	if st.chat.scroll != 0 {
		t.Fatalf("scroll %d", st.chat.scroll)
	}
	if cmd := st.handleMouse(mouseEvent{button: mouseWheelUp}); cmd != fleetCmdRender || st.chat.scroll != fleetMouseStep {
		t.Fatalf("wheel: %v %d", cmd, st.chat.scroll)
	}
}

func TestFleetChatHintInHelpRow(t *testing.T) {
	now := time.Now()
	st := chatTestState(now)
	if row := stripANSI(fleetBottomRow(st, now)); !strings.Contains(row, fleetHelpChatHint) {
		t.Fatalf("row %q", row)
	}
	st.handleKey("o", false, now)
	if row := stripANSI(fleetBottomRow(st, now)); strings.Contains(row, fleetHelpChatHint) {
		t.Fatalf("hint with a panel open: %q", row)
	}
}

func chatLoop(now time.Time, get func(string, int64) fleetChatFetch, send func(fleetSayRequest) fleetSendResult) *fleetLoop {
	return &fleetLoop{st: chatTestState(now), interval: time.Hour, chatGet: get, send: send,
		state: func() (fleetStateView, error) { return fleetStateView{}, errors.New("no state in this test") }}
}

func TestFleetChatLoopPollsWhileOpen(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	var sinces []int64
	pos := int64(0)
	get := func(source string, since int64) fleetChatFetch {
		if source != "http://src" {
			t.Errorf("source %q", source)
		}
		sinces = append(sinces, since)
		pos += 10
		return fleetChatFetch{view: fleetChatView{SessionID: "s1", Pos: pos}}
	}
	l := chatLoop(now, get, nil)
	defer l.closeAll()
	l.nextPoll = now.Add(time.Hour)
	l.run(l.st.handleKey("i", false, now), now)
	if len(l.chats) != 1 {
		t.Fatal("open did not read the chat")
	}
	l.run(fleetCmdChatGet, now) // one read at a time
	if len(l.chats) != 1 {
		t.Fatal("a second read started beside the first")
	}
	waitFleetJobs(t, l)
	l.reap(now)
	if !l.st.chat.loaded || l.st.chat.pos != 10 {
		t.Fatalf("chat %+v", l.st.chat)
	}
	if d := l.timeout(now); d <= 0 || d > fleetChatPoll {
		t.Fatalf("timeout with the chat open = %v", d)
	}
	l.tick(now.Add(time.Second))
	if len(l.chats) != 0 {
		t.Fatal("polled before 2 s")
	}
	l.tick(now.Add(fleetChatPoll))
	waitFleetJobs(t, l)
	l.reap(now.Add(fleetChatPoll))
	if len(sinces) != 2 || sinces[1] != 10 || l.st.chat.pos != 20 {
		t.Fatalf("sinces %v pos %d", sinces, l.st.chat.pos)
	}
	// Closed: no more reads and no chat deadline.
	l.st.handleKey(KeyEsc, false, now)
	l.tick(now.Add(time.Minute))
	if len(l.chats) != 0 || len(sinces) != 2 {
		t.Fatalf("polled while closed: %v", sinces)
	}
	if d := l.timeout(now.Add(time.Minute)); d != time.Hour-time.Minute {
		t.Errorf("timeout closed = %v", d)
	}
}

func TestFleetChatLoopNewSessionReadsAgain(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	var sinces []int64
	get := func(_ string, since int64) fleetChatFetch {
		sinces = append(sinces, since)
		sid := "s1"
		if len(sinces) > 1 {
			sid = "s2"
		}
		return fleetChatFetch{view: fleetChatView{SessionID: sid, Pos: 50}}
	}
	l := chatLoop(now, get, nil)
	defer l.closeAll()
	l.run(l.st.handleKey("i", false, now), now)
	waitFleetJobs(t, l)
	l.reap(now)
	l.tick(now.Add(fleetChatPoll)) // since 50: answers s2
	waitFleetJobs(t, l)
	l.reap(now.Add(fleetChatPoll))
	l.tick(now.Add(fleetChatPoll)) // at once, from the tail
	waitFleetJobs(t, l)
	l.reap(now.Add(fleetChatPoll))
	if len(sinces) != 3 || sinces[1] != 50 || sinces[2] != 0 || l.st.chat.sid != "s2" {
		t.Fatalf("sinces %v sid %q", sinces, l.st.chat.sid)
	}
}

func TestFleetChatLoopSend(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	var sent []fleetSayRequest
	var reads atomic.Int32
	get := func(string, int64) fleetChatFetch {
		reads.Add(1)
		return fleetChatFetch{view: fleetChatView{SessionID: "s1", Pos: 1}}
	}
	send := func(r fleetSayRequest) fleetSendResult {
		sent = append(sent, r)
		return fleetSendResult{Out: fleetSayOutcome{Status: 200, Result: fleetSayResult{OK: true}}}
	}
	l := chatLoop(now, get, send)
	defer l.closeAll()
	l.run(l.st.handleKey("i", false, now), now)
	waitFleetJobs(t, l)
	l.reap(now)
	late := now.Add(time.Second)
	for _, k := range []string{"h", "i", KeyAltEnter, "x"} {
		l.st.handleKey(k, false, late)
	}
	l.run(l.st.handleKey(KeyEnter, false, late), late)
	if len(l.chatSends) != 1 || l.st.chatSend != nil {
		t.Fatalf("send not started: %d", len(l.chatSends))
	}
	waitFleetJobs(t, l)
	l.reap(late)
	if len(sent) != 1 || sent[0] != (fleetSayRequest{Source: "http://src", Text: "hi\nx"}) {
		t.Fatalf("sent %+v", sent)
	}
	if l.st.chatSending || l.st.chatDraft.Text() != "" || l.st.toast != fleetToastSent {
		t.Fatalf("after send: sending %v draft %q toast %q", l.st.chatSending, l.st.chatDraft.Text(), l.st.toast)
	}
	// The chat is read at once after a send.
	l.tick(late)
	waitFleetJobs(t, l)
	if reads.Load() != 2 {
		t.Fatalf("reads %d", reads.Load())
	}
}

func TestFleetChatViaMoved(t *testing.T) {
	var chatCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/fleet/state":
			io.WriteString(w, `{"fleet":{"alive":true},"cards":[]}`)
		case "/fleet/chat":
			chatCalls.Add(1)
			io.WriteString(w, `{"turns":[{"uuid":"u","role":"user","text":"hi"}],"pos":3,"sessionId":"s"}`)
		}
	}))
	defer srv.Close()
	src := &fleetLocal{connect: func() (*fleetClient, fleetStateView, error) {
		c := newFleetClient(srv.URL, "tok")
		v, err := c.GetState()
		return c, v, err
	}}
	via := fleetChatVia(src)
	if f := via("http://elsewhere", 0); !f.moved || chatCalls.Load() != 0 {
		t.Fatalf("pinned read: %+v calls %d", f, chatCalls.Load())
	}
	if f := via(srv.URL, 0); f.moved || f.err != nil || f.view.Pos != 3 || len(f.view.Turns) != 1 {
		t.Fatalf("same host: %+v", f)
	}
}
