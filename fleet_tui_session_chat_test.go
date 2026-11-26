package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const sessChatSID = "0123abcd-0000-4000-8000-000000000001"

// sessChatView is an alive FLEET with card p naming its session only in tmux
// (the live cards' shape), card q naming another session by display name,
// and card e whose tmux session ended.
func sessChatView() fleetStateView {
	v := fleetTestView(
		fleetTmuxCard("p", "promote-prod-1", "", "DR-1"),
		fleetTmuxCard("q", "", "Fix \x1b[31mtwo", ""),
		fleetTmuxCard("e", "gone-1", "", ""),
	)
	v.Sessions = []fleetWorkSession{
		{Tmux: "promote-prod-1", Name: "Promote \x1b]0;x\x07prod", SessionID: sessChatSID},
		{Tmux: "fix-two", Name: "Fix \x1b[31mtwo", SessionID: "sid-two"},
	}
	v.Source = "http://src"
	return v
}

func TestFleetClientSessionGetAndSay(t *testing.T) {
	var (
		mu      sync.Mutex
		queries []string
		posts   []map[string]any
	)
	reply, status := `{"target":{"tmux":"a","tool":"claude","sessionId":"s1"},"source":"transcript",`+
		`"turns":[{"uuid":"u1","role":"assistant","text":"hi\u001b[2J"}],"pos":42,"reset":true}`, http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("no bearer")
		}
		switch {
		case r.Method == "GET" && r.URL.Path == "/fleet/session":
			queries = append(queries, r.URL.RawQuery)
			w.WriteHeader(status)
			io.WriteString(w, reply)
		case r.Method == "POST" && r.URL.Path == "/fleet/session/say":
			var m map[string]any
			_ = json.NewDecoder(r.Body).Decode(&m)
			posts = append(posts, m)
			w.WriteHeader(status)
			io.WriteString(w, reply)
		default:
			t.Errorf("bad request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	c := newFleetClient(srv.URL, "tok")
	v, err := c.SessionGet("a", "s1", 0, 100)
	if err != nil || v.Pos != 42 || !v.Reset || v.Tool != "claude" || v.Pane != nil || len(v.Turns) != 1 || strings.Contains(v.Turns[0].Text, "\x1b") {
		t.Fatalf("SessionGet = %+v, %v", v, err)
	}
	if _, err := c.SessionGet("a", "", 7, 100); err != nil {
		t.Fatal(err)
	}
	if strings.Join(queries, "|") != "n=100&sid=s1&t=a|n=100&since=7&t=a" {
		t.Fatalf("queries %q", queries)
	}
	reply = `{"target":{"tool":"grok"},"source":"pane","turns":[],"pos":0,"reset":true,"pane":"screen\u001b[31m line\nnext"}`
	if v, err := c.SessionGet("a", "", 0, 100); err != nil || v.Pane == nil || *v.Pane != "screen line\nnext" || v.Tool != "grok" {
		t.Fatalf("pane = %+v, %v", v, err)
	}
	for _, bad := range []string{`null`, `[]`, `{"pos":-1}`, `nope`} {
		reply = bad
		if _, err := c.SessionGet("a", "", 0, 100); err == nil {
			t.Errorf("%s decoded", bad)
		}
	}
	reply, status = `{"ok":false,"code":"gone","error":"this session has ended"}`, http.StatusNotFound
	var ref *fleetSessionRefusal
	if _, err := c.SessionGet("a", "", 0, 100); !errors.As(err, &ref) || ref.Code != "gone" || ref.Status != 404 {
		t.Fatalf("gone = %v", err)
	}
	status = http.StatusUnauthorized
	if _, err := c.SessionGet("a", "", 0, 100); !errors.Is(err, errFleetUnauthorized) {
		t.Fatalf("401 = %v", err)
	}

	// Say: target, text with its line break, sid; an empty sid is omitted.
	reply, status = `{"ok":true,"queued":true,"target":"a"}`, http.StatusOK
	o := c.SessionSay(fleetSessionSayRequest{Source: "http://src", Target: "a", Text: "one\ntwo", SID: "s1"})
	if o.Status != 200 || !o.Result.OK || !o.Result.Queued || o.Unclear {
		t.Fatalf("say = %+v", o)
	}
	c.SessionSay(fleetSessionSayRequest{Target: "a", Text: "x"})
	if len(posts) != 2 || posts[0]["target"] != "a" || posts[0]["text"] != "one\ntwo" || posts[0]["sid"] != "s1" || len(posts[0]) != 3 {
		t.Fatalf("posts %v", posts)
	}
	if _, has := posts[1]["sid"]; has {
		t.Fatalf("empty sid sent: %v", posts[1])
	}
	reply, status = `{"ok":false,"code":"not_session","error":"this tmux session now runs another session","target":"a"}`, http.StatusConflict
	if o := c.SessionSay(fleetSessionSayRequest{Target: "a", Text: "x"}); o.Status != 409 || o.Result.OK || o.Result.Code != "not_session" || o.Unclear {
		t.Fatalf("refusal = %+v", o)
	}
}

func TestSessionChatKeyOpens(t *testing.T) {
	now := time.Unix(1000, 0)
	st := fleetTestState(sessChatView(), now)
	st.setToast("old", fleetToastInfo, now)
	if cmd := st.handleKey("i", false, now); cmd != fleetCmdChatGet {
		t.Fatalf("i = %v", cmd)
	}
	c := st.chat
	if c == nil || c.source != "http://src" || c.target == nil || *c.target != (fleetChatTarget{Tmux: "promote-prod-1", SID: sessChatSID, Name: "Promote prod"}) || st.toast != "" {
		t.Fatalf("chat = %+v target %+v toast %q", c, c.target, st.toast)
	}
	// Opening tells FLEET nothing: the first message that goes through does.
	if c.card != "p" || len(st.chatTold) != 0 {
		t.Fatalf("open: card %q told %v", c.card, st.chatTold)
	}
	if n := st.chatNoticeFor("p", "promote-prod-1", "http://src"); n == nil || *n != (fleetSayRequest{Source: "http://src", Text: "Andy opened chat on card p (promote-prod-1)"}) {
		t.Fatalf("notice = %+v", n)
	}
	head := fleetChatHead(st, 80)
	if !strings.Contains(stripANSI(head), "Chat with Promote prod · promote-prod-1") || strings.Contains(head, "\x07") {
		t.Fatalf("head %q", head)
	}
	// Once per card per run; another host's send tells nothing.
	st.markChatTold("p")
	if n := st.chatNoticeFor("p", "promote-prod-1", "http://src"); n != nil {
		t.Fatalf("told twice: %+v", n)
	}
	if n := st.chatNoticeFor("q", "fix-two", "http://other"); n != nil {
		t.Fatalf("moved host: %+v", n)
	}
	st.handleKey(KeyEsc, false, now)

	// The display-name card: the row's tmux, and its own notice.
	st.handleKey(KeyDown, false, now)
	if cmd := st.handleKey("i", false, now); cmd != fleetCmdChatGet || st.chat.target.Tmux != "fix-two" || st.chat.target.SID != "sid-two" {
		t.Fatalf("display name: %v %+v", cmd, st.chat)
	}
	if st.chat.card != "q" {
		t.Fatalf("card %q", st.chat.card)
	}
	st.handleKey(KeyEsc, false, now)

	// The ended card: a toast, no chat.
	st.handleKey(KeyDown, false, now)
	if cmd := st.handleKey("i", false, now); cmd != fleetCmdRender || st.chat != nil || st.toast != "this card's session has ended" {
		t.Fatalf("ended: %v %+v %q", cmd, st.chat, st.toast)
	}

	// A paste never opens it.
	st = fleetTestState(sessChatView(), now)
	if cmd := st.handleKey("i", true, now); cmd != fleetCmdNone || st.chat != nil {
		t.Fatalf("pasted i: %v", cmd)
	}
	// FLEET down: the chat opens, FLEET is not told.
	v := sessChatView()
	v.Fleet.Alive = false
	st = fleetTestState(v, now)
	if st.handleKey("i", false, now); st.chat == nil || st.chatNoticeFor("p", "promote-prod-1", "http://src") != nil {
		t.Fatalf("FLEET down: chat %+v", st.chat)
	}
	// A card id the page would not type is never typed.
	v = sessChatView()
	v.Cards[0].ID = "p q"
	v.CardHashes["p q"] = "h"
	st = fleetTestState(v, now)
	if st.handleKey("i", false, now); st.chat == nil || st.chatNoticeFor(st.chat.card, st.chat.target.Tmux, "http://src") != nil {
		t.Fatalf("bad id: chat %+v", st.chat)
	}
	// A row id outside resumeSessionIDRe is no pin.
	v = sessChatView()
	v.Sessions[0].SessionID = "bad id"
	st = fleetTestState(v, now)
	if st.handleKey("i", false, now); st.chat == nil || st.chat.target.SID != "" {
		t.Fatalf("bad sid: %+v", st.chat)
	}

	// The panel takes i and → as comment text and a cursor key; Clarify
	// takes i as text.
	for _, k := range []string{"i", KeyRight} {
		st = fleetTestState(sessChatView(), now)
		st.handleKey("1", false, now)
		if st.handleKey(k, false, now); st.chat != nil {
			t.Fatalf("%q on the panel: chat %+v", k, st.chat)
		}
		st = fleetTestState(sessChatView(), now)
		st.handleKey("k", false, now)
		if st.handleKey(k, false, now); st.chat != nil {
			t.Fatalf("%q in Clarify opened the session chat", k)
		}
	}
	st = fleetTestState(sessChatView(), now)
	st.handleKey("1", false, now)
	if st.handleKey("i", false, now); st.panel.comment != "i" {
		t.Fatalf("panel comment %q", st.panel.comment)
	}
	// In the FLEET chat, i is text and → moves the cursor.
	st = fleetTestState(sessChatView(), now)
	st.handleKey("f", false, now)
	st.handleKey(KeyRight, false, now)
	if st.handleKey("i", false, now); st.chat.target != nil || st.chatDraft.Text() != "i" {
		t.Fatalf("FLEET chat: %+v %q", st.chat.target, st.chatDraft.Text())
	}
	// s no longer opens anything on the list.
	st = fleetTestState(sessChatView(), now)
	if cmd := st.handleKey("s", false, now); cmd != fleetCmdNone || st.chat != nil {
		t.Fatalf("s = %v chat %+v", cmd, st.chat)
	}
	// → is i: the same chat, and never from a paste.
	st = fleetTestState(sessChatView(), now)
	if cmd := st.handleKey(KeyRight, false, now); cmd != fleetCmdChatGet || st.chat == nil || st.chat.target == nil || st.chat.target.Tmux != "promote-prod-1" {
		t.Fatalf("→ = %v chat %+v", cmd, st.chat)
	}
	st = fleetTestState(sessChatView(), now)
	if cmd := st.handleKey(KeyRight, true, now); cmd != fleetCmdNone || st.chat != nil {
		t.Fatalf("pasted → = %v", cmd)
	}
}

// Each target keeps its own composer: FLEET's, and one per session.
func TestSessionChatDraftsSeparate(t *testing.T) {
	now := time.Unix(1000, 0)
	late := now.Add(time.Second)
	st := fleetTestState(sessChatView(), now)
	st.handleKey("f", false, now)
	st.handleKey("f", false, late)
	st.handleKey(KeyEsc, false, late)
	st.handleKey("i", false, now)
	if st.chatEditor().Text() != "" {
		t.Fatalf("session draft starts with %q", st.chatEditor().Text())
	}
	st.handleKey("p", false, late)
	st.handleKey(KeyEsc, false, late)
	st.handleKey(KeyDown, false, now)
	st.handleKey("i", false, now)
	st.handleKey("q", false, late)
	st.handleKey(KeyEsc, false, late)
	st.handleKey(KeyUp, false, now)
	st.handleKey("i", false, now)
	if got := st.chatEditor().Text(); got != "p" {
		t.Fatalf("p's draft %q", got)
	}
	if !strings.Contains(stripANSI(strings.Join(fleetChatLines(st, 80, 20, late), "\n")), "› p") {
		t.Fatal("p's draft not drawn")
	}
	st.handleKey(KeyEsc, false, late)
	st.handleKey("f", false, now)
	if got := st.chatEditor().Text(); got != "f" || st.chatDraft.Text() != "f" {
		t.Fatalf("FLEET draft %q", got)
	}
	if len(st.sessionDrafts) != 2 || st.sessionDrafts[(fleetChatTarget{Tmux: "fix-two", SID: "sid-two"}).key()].Text() != "q" {
		t.Fatalf("drafts %v", st.sessionDrafts)
	}
}

func TestSessionChatApplyRead(t *testing.T) {
	now := time.Unix(1000, 0)
	st := fleetTestState(sessChatView(), now)
	st.handleKey("i", false, now)
	c := st.chat
	st.applyChat(fleetChatFetch{chat: c, session: fleetSessionView{Tool: "claude", Pos: 10, Reset: true,
		Turns: []fleetChatTurn{{UUID: "u1", Role: "user", Text: "hi"}, {UUID: "u2", Role: "assistant", Text: "on it"}}}}, now)
	if !c.loaded || c.pos != 10 || len(c.turns) != 2 || c.ended {
		t.Fatalf("first read %+v", c)
	}
	st.applyChat(fleetChatFetch{chat: c, session: fleetSessionView{Pos: 20,
		Turns: []fleetChatTurn{{UUID: "u2", Role: "assistant", Text: "on it, done"}}}}, now)
	if c.pos != 20 || len(c.turns) != 2 || c.turns[1].Text != "on it, done" {
		t.Fatalf("incremental %+v", c.turns)
	}
	out := stripANSI(strings.Join(fleetChatLines(st, 80, 20, now), "\n"))
	if !strings.Contains(out, "agent on it, done") || !strings.Contains(out, "you   hi") {
		t.Fatalf("log\n%s", out)
	}
	// A pane read replaces the log; a transcript read after it starts over.
	pane := "$ make\nok"
	st.applyChat(fleetChatFetch{chat: c, session: fleetSessionView{Tool: "grok", Pane: &pane, Reset: true}}, now)
	if c.pane == nil || c.turns != nil || c.pos != 0 {
		t.Fatalf("pane %+v", c)
	}
	out = stripANSI(strings.Join(fleetChatLines(st, 80, 20, now), "\n"))
	if !strings.Contains(out, "$ make") || !strings.Contains(fleetChatHead(st, 80), "grok: lines join") {
		t.Fatalf("pane log\n%s", out)
	}
	st.applyChat(fleetChatFetch{chat: c, session: fleetSessionView{Pos: 5, Turns: []fleetChatTurn{{UUID: "u9", Role: "user", Text: "x"}}}}, now)
	if c.pane != nil || len(c.turns) != 1 || c.pos != 5 {
		t.Fatalf("after pane %+v", c)
	}
	// A transient failure keeps the chat polling.
	st.applyChat(fleetChatFetch{chat: c, err: errors.New("net")}, now)
	if c.ended || c.err != "could not read the session" {
		t.Fatalf("net err %+v", c)
	}
	// The session ended or its tmux session runs another one: the chat ends
	// with a toast, the log stays, and a send is refused.
	for _, code := range []string{"gone", "not_session", "not_allowed"} {
		st := fleetTestState(sessChatView(), now)
		st.handleKey("i", false, now)
		c := st.chat
		st.applyChat(fleetChatFetch{chat: c, session: fleetSessionView{Pos: 1, Turns: []fleetChatTurn{{UUID: "u", Role: "user", Text: "hi"}}}}, now)
		st.applyChat(fleetChatFetch{chat: c, err: &fleetSessionRefusal{Status: 404, Code: code}}, now)
		if !c.ended || len(c.turns) != 1 || st.toast != fleetSessionRefusalText(code) || st.toastKind != fleetToastWarn {
			t.Fatalf("%s: %+v toast %q", code, c, st.toast)
		}
		st.chatEditor().SetText("more")
		if cmd := st.handleKey(KeyEnter, false, now.Add(time.Second)); cmd == fleetCmdChatSend || st.sessionSend != nil {
			t.Fatalf("%s: sent to an ended chat", code)
		}
		if !strings.Contains(fleetChatHead(st, 80), "ended") {
			t.Fatalf("%s: head %q", code, fleetChatHead(st, 80))
		}
	}
}

func TestSessionSendToast(t *testing.T) {
	cases := []struct {
		o    fleetSayOutcome
		msg  string
		kind fleetToastKind
	}{
		{fleetSayOutcome{Status: 200, Result: fleetSayResult{OK: true}}, fleetToastSent, fleetToastInfo},
		{fleetSayOutcome{Status: 200, Result: fleetSayResult{OK: true, Queued: true}}, "the session is busy — it reads your message next", fleetToastInfo},
		{fleetSayOutcome{Unclear: true}, "delivery not confirmed — check the session", fleetToastWarn},
		{fleetSayOutcome{Status: 401}, fleetToastTokenBad, fleetToastErr},
		{fleetSayOutcome{Status: 404, Result: fleetSayResult{Code: "gone", Error: "x"}}, "this session has ended", fleetToastWarn},
		{fleetSayOutcome{Status: 409, Result: fleetSayResult{Code: "not_session"}}, "this tmux session now runs another session", fleetToastWarn},
		{fleetSayOutcome{Status: 200, Result: fleetSayResult{Code: "not_submitted"}}, "typed but not submitted — check the session's pane", fleetToastWarn},
		{fleetSayOutcome{Status: 200, Result: fleetSayResult{Code: "input_busy", Error: "The session input box is not empty"}}, "The session input box is not empty", fleetToastErr},
		{fleetSayOutcome{Status: 400, Result: fleetSayResult{Code: "bad_text"}}, "bad_text", fleetToastErr},
		{fleetSayOutcome{Status: 502}, "HTTP 502", fleetToastErr},
	}
	for _, c := range cases {
		if msg, kind := fleetSessionSendToast(c.o); msg != c.msg || kind != c.kind {
			t.Errorf("%+v: %q %v, want %q %v", c.o, msg, kind, c.msg, c.kind)
		}
	}
}

// sessChatCall is one call a fake session endpoint saw.
type sessChatCall struct {
	source string
	t      fleetChatTarget
	since  int64
}

func TestSessionChatLoop(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	var (
		mu      sync.Mutex
		reads   []sessChatCall
		says    []fleetSessionSayRequest
		notices []fleetSayRequest
	)
	sayRes := fleetSendResult{Out: fleetSayOutcome{Status: 200, Result: fleetSayResult{OK: true}}}
	noticeRes := fleetSendResult{Out: fleetSayOutcome{Status: 200, Result: fleetSayResult{OK: true}}}
	readErr := error(nil)
	l := &fleetLoop{st: fleetTestState(sessChatView(), now), interval: time.Hour,
		state: func() (fleetStateView, error) { return fleetStateView{}, errors.New("no state in this test") },
		send: func(r fleetSayRequest) fleetSendResult {
			mu.Lock()
			defer mu.Unlock()
			notices = append(notices, r)
			return noticeRes
		},
		chatGet: func(string, int64) fleetChatFetch {
			t.Error("the session chat read FLEET's chat")
			return fleetChatFetch{}
		},
		sessionGet: func(source string, tg fleetChatTarget, since int64) fleetChatFetch {
			mu.Lock()
			defer mu.Unlock()
			reads = append(reads, sessChatCall{source, tg, since})
			return fleetChatFetch{session: fleetSessionView{Pos: int64(10 * len(reads))}, err: readErr}
		},
		sessionSay: func(r fleetSessionSayRequest) fleetSendResult {
			mu.Lock()
			defer mu.Unlock()
			says = append(says, r)
			return sayRes
		},
	}
	l.nextPoll = now.Add(time.Hour)
	defer l.closeAll()
	l.run(l.st.handleKey("i", false, now), now)
	if len(l.chats) != 1 || len(l.notices) != 0 {
		t.Fatalf("open: reads %d notices %d", len(l.chats), len(l.notices))
	}
	waitFleetJobs(t, l)
	l.reap(now)
	want := fleetChatTarget{Tmux: "promote-prod-1", SID: sessChatSID, Name: "Promote prod"}
	if len(reads) != 1 || reads[0] != (sessChatCall{"http://src", want, 0}) {
		t.Fatalf("reads %+v", reads)
	}
	if len(notices) != 0 {
		t.Fatalf("open told FLEET: %+v", notices)
	}
	l.tick(now.Add(fleetChatPoll))
	waitFleetJobs(t, l)
	l.reap(now.Add(fleetChatPoll))
	if len(reads) != 2 || reads[1].since != 10 {
		t.Fatalf("poll reads %+v", reads)
	}

	// A multi-line send goes to the session, pinned to its id.
	late := now.Add(3 * time.Second)
	for _, k := range []string{"o", "k", KeyAltEnter, "g", "o"} {
		l.st.handleKey(k, false, late)
	}
	l.run(l.st.handleKey(KeyEnter, false, late), late)
	if len(l.chatSends) != 1 || l.st.sessionSend != nil || l.st.chatSend != nil {
		t.Fatalf("send not started: %d", len(l.chatSends))
	}
	waitFleetJobs(t, l)
	l.reap(late)
	if len(says) != 1 || says[0] != (fleetSessionSayRequest{Source: "http://src", Target: "promote-prod-1", Text: "ok\ngo", SID: sessChatSID}) {
		t.Fatalf("says %+v", says)
	}
	if l.st.chatSending || l.st.chatEditor().Text() != "" || l.st.toast != fleetToastSent {
		t.Fatalf("after send: sending %v draft %q toast %q", l.st.chatSending, l.st.chatEditor().Text(), l.st.toast)
	}
	// FLEET hears of the chat only now, after Andy's message went through.
	if len(l.notices) != 1 || !l.st.chatTold["p"] {
		t.Fatalf("notice after send: %d told %v", len(l.notices), l.st.chatTold)
	}
	waitFleetJobs(t, l)
	l.reap(late)
	if len(notices) != 1 || notices[0] != (fleetSayRequest{Source: "http://src", Text: "Andy opened chat on card p (promote-prod-1)"}) {
		t.Fatalf("notices %+v", notices)
	}
	if l.st.toast != fleetToastSent {
		t.Fatalf("accepted notice toasted %q", l.st.toast)
	}
	// A second send in the same chat tells FLEET nothing new.
	l.st.chatEditor().SetText("more")
	l.run(l.st.handleKey(KeyEnter, false, late), late)
	waitFleetJobs(t, l)
	l.reap(late)
	waitFleetJobs(t, l)
	l.reap(late)
	if len(says) != 2 || len(notices) != 1 {
		t.Fatalf("second send: says %d notices %d", len(says), len(notices))
	}

	// The session went away under the send: the chat ends, the text stays.
	sayRes = fleetSendResult{Out: fleetSayOutcome{Status: 404, Result: fleetSayResult{Code: "gone", Error: "this session has ended"}}}
	l.st.chatEditor().SetText("again")
	later := late.Add(2 * time.Second)
	l.run(l.st.handleKey(KeyEnter, false, later), later)
	waitFleetJobs(t, l)
	l.reap(later)
	if !l.st.chat.ended || l.st.chatEditor().Text() != "again" || l.st.toast != "this session has ended" {
		t.Fatalf("gone: %+v draft %q toast %q", l.st.chat, l.st.chatEditor().Text(), l.st.toast)
	}
	n := len(reads)
	l.tick(later.Add(time.Minute))
	waitFleetJobs(t, l)
	if len(reads) != n || len(l.chats) != 0 {
		t.Fatal("an ended chat was read again")
	}
	l.st.toast, l.st.toastUntil = "", time.Time{}
	if d := l.timeout(later); d <= fleetChatPoll {
		t.Errorf("timeout with an ended chat = %v", d)
	}

	// The gone send told FLEET nothing.
	if len(notices) != 1 {
		t.Fatalf("failed send told FLEET: %+v", notices)
	}

	// The next card: a failed send marks nothing; the first send that goes
	// through tells FLEET; a refused notice is a quiet toast and never
	// closes the panel.
	l.st.chat = nil
	l.st.handleKey(KeyDown, false, later)
	l.run(l.st.handleKey("i", false, later), later)
	waitFleetJobs(t, l)
	l.reap(later)
	armed := later.Add(3 * time.Second)
	sayRes = fleetSendResult{Out: fleetSayOutcome{Status: 200, Result: fleetSayResult{Code: "rate_limited", Error: "wait a moment"}}}
	l.st.chatEditor().SetText("hi")
	l.run(l.st.handleKey(KeyEnter, false, armed), armed)
	waitFleetJobs(t, l)
	l.reap(armed)
	if len(notices) != 1 || l.st.chatTold["q"] {
		t.Fatalf("refused send: notices %d told %v", len(notices), l.st.chatTold)
	}
	sayRes = fleetSendResult{Out: fleetSayOutcome{Status: 200, Result: fleetSayResult{OK: true}}}
	noticeRes = fleetSendResult{Out: fleetSayOutcome{Status: 200, Result: fleetSayResult{Code: "rate_limited"}}}
	l.run(l.st.handleKey(KeyEnter, false, armed), armed)
	waitFleetJobs(t, l)
	l.reap(armed)
	waitFleetJobs(t, l)
	l.reap(armed)
	if l.st.chat == nil || l.st.toast != "FLEET was not told about this chat" || l.st.toastKind != fleetToastInfo || len(notices) != 2 {
		t.Fatalf("refused notice: chat %+v toast %q notices %d", l.st.chat, l.st.toast, len(notices))
	}
	if notices[1].Text != "Andy opened chat on card q (fix-two)" || says[len(says)-1].Target != "fix-two" {
		t.Fatalf("second card: notice %+v say %+v", notices[1], says[len(says)-1])
	}
}

// The job bodies talk to a fake service, pinned to the chat's host.
func TestSessionChatViaFakeServer(t *testing.T) {
	var mu sync.Mutex
	var gets, says []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/fleet/state":
			io.WriteString(w, `{"fleet":{"alive":true},"cards":[]}`)
		case "/fleet/session":
			gets = append(gets, r.URL.RawQuery)
			io.WriteString(w, `{"target":{"tmux":"dr-1","tool":"claude"},"source":"transcript","turns":[{"uuid":"u","role":"user","text":"hi"}],"pos":3,"reset":true}`)
		case "/fleet/session/say":
			b, _ := io.ReadAll(r.Body)
			says = append(says, string(b))
			io.WriteString(w, `{"ok":true,"target":"dr-1"}`)
		}
	}))
	defer srv.Close()
	src := &fleetLocal{connect: func() (*fleetClient, fleetStateView, error) {
		c := newFleetClient(srv.URL, "tok")
		v, err := c.GetState()
		return c, v, err
	}}
	tg := fleetChatTarget{Tmux: "dr-1", SID: "sid-1"}
	get, say := fleetSessionChatVia(src), fleetSessionSayVia(src)
	if f := get("http://elsewhere", tg, 0); !f.moved {
		t.Fatalf("pinned read: %+v", f)
	}
	if r := say(fleetSessionSayRequest{Source: "http://elsewhere", Target: "dr-1", Text: "x"}); !r.Moved {
		t.Fatalf("pinned send: %+v", r)
	}
	if len(gets)+len(says) != 0 {
		t.Fatalf("moved calls reached the server: %v %v", gets, says)
	}
	if f := get(srv.URL, tg, 0); f.moved || f.err != nil || f.session.Pos != 3 || len(f.session.Turns) != 1 {
		t.Fatalf("same host: %+v", f)
	}
	if r := say(fleetSessionSayRequest{Source: srv.URL, Target: "dr-1", Text: "a\nb", SID: "sid-1"}); !r.Out.Result.OK {
		t.Fatalf("send: %+v", r)
	}
	if len(gets) != 1 || gets[0] != "n=100&sid=sid-1&t=dr-1" || len(says) != 1 || says[0] != `{"target":"dr-1","text":"a\nb","sid":"sid-1"}` {
		t.Fatalf("gets %v says %v", gets, says)
	}
}

func TestSessionChatHintInHelpRow(t *testing.T) {
	now := time.Unix(1000, 0)
	st := fleetTestState(sessChatView(), now)
	if row := stripANSI(fleetBottomRow(st, now)); !strings.Contains(row, "a attach · i/→ chat") {
		t.Fatalf("row %q", row)
	}
	st.handleKey(KeyDown, false, now)
	st.handleKey(KeyDown, false, now)
	if row := stripANSI(fleetBottomRow(st, now)); strings.Contains(row, "i/→ chat") {
		t.Fatalf("ended card shows the hint: %q", row)
	}
}

// The chat closes when FLEET's host moves, like the FLEET chat.
func TestSessionChatClosesWhenHostMoves(t *testing.T) {
	now := time.Unix(1000, 0)
	st := fleetTestState(sessChatView(), now)
	st.handleKey("i", false, now)
	v := sessChatView()
	v.Source = "http://other"
	st.applyState(v, nil, now)
	if st.chat != nil || !strings.Contains(st.toast, "moved") {
		t.Fatalf("chat %+v toast %q", st.chat, st.toast)
	}
}
