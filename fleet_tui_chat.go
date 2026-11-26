package main

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// The Fleet view's chat with FLEET itself, opened with f on the card list:
// FLEET's recent conversation (GET /fleet/chat, the page's Chat tab) above a
// multi-line composer (text_editor.go) whose Enter sends a plain message
// (POST /fleet/say with no card). i or → on a card opens the same panel on the
// card's session instead (fleetCardSession): its transcript or pane (GET
// /fleet/session, the page's session view) and its say (POST
// /fleet/session/say), pinned to the row's session id. Each target keeps its
// own composer. State, keys and drawing are here; the loop (fleet_tui.go)
// runs the jobs the commands ask for.

const (
	// fleetChatPoll is how often the open chat is read.
	fleetChatPoll = 2 * time.Second
	// fleetChatKeep is how many turns the chat keeps (and asks for).
	fleetChatKeep = 100
	// fleetHelpChat is the chat screen's key help.
	fleetHelpChat = composeHint + " · PgUp/PgDn"
)

// fleetChatTarget is the session a session chat talks to: its tmux session,
// the session id it is pinned to ("" is no pin) and its display name
// (sanitized, one line; "" when unknown).
type fleetChatTarget struct {
	Tmux, SID, Name string
}

// key is the target's composer key: one draft per tmux session and session.
func (t fleetChatTarget) key() string { return t.Tmux + "\x00" + t.SID }

// fleetChatMode is the open chat. source is the state's Source when it
// opened: reads and sends are pinned to it like a card answer.
type fleetChatMode struct {
	source   string
	target   *fleetChatTarget // the session chat's session; nil is the chat with FLEET
	pane     *string          // a session read from its pane (no transcript): the sanitized text
	tool     string           // the session's tool, from its last read
	ended    bool             // the session refused the chat for good: no more reads or sends
	turns    []fleetChatTurn  // oldest first, at most fleetChatKeep, every string sanitized
	pos      int64            // the last read's pos; 0 reads the tail again
	sid      string           // the FLEET session the turns belong to
	loaded   bool             // the first read landed
	readOnly bool             // FLEET is down: the last session, mirrored
	reason   string           // why there is no live chat, from the server
	err      string           // a fixed line when the chat could not be read
	scroll   int              // lines scrolled up from the newest
	openedAt time.Time
	// again: the last read found another FLEET session and started over
	// from its tail; the loop reads again at once.
	again bool
}

// fleetChatFetch is one chat read's result, with the chat it was read for.
type fleetChatFetch struct {
	chat    *fleetChatMode
	view    fleetChatView
	session fleetSessionView // a session chat's read
	err     error
	moved   bool // FLEET is on another host than the chat; nothing was read
}

// fleetChatSent is a finished chat send with the text it sent, and the
// session it went to (nil: FLEET).
type fleetChatSent struct {
	text   string
	res    fleetSendResult
	target *fleetChatTarget
}

// canChat reports whether f opens the chat: a loaded state from one host
// and nothing else open; the ship screen counts as nothing open. FLEET may
// be down; the chat then shows its last session and refuses to send.
func (st *fleetViewState) canChat() bool {
	return st.loaded && st.err == "" && len(st.view.Ambiguous) == 0 && st.view.Source != "" &&
		st.panel == nil && st.batch == nil && st.clarify == nil && st.chat == nil
}

func (st *fleetViewState) openChat(now time.Time) fleetCmd {
	if !st.canChat() {
		return fleetCmdNone
	}
	st.chat = &fleetChatMode{source: st.view.Source, openedAt: now}
	if st.chatDraft.maxBytes == 0 {
		st.chatDraft = newTextEditor(sendKeysMaxLen)
	}
	st.toast, st.toastUntil = "", time.Time{}
	return fleetCmdChatGet
}

// openSessionChat is i or → on the list: the selected card's session
// (fleetCardSession, as a resolves it) in the chat panel. A paste never
// opens it. A card with no one session toasts why. The first open per card
// id per run tells FLEET (tellFleetChat).
func (st *fleetViewState) openSessionChat(inBatch bool, now time.Time) fleetCmd {
	if inBatch || !st.canChat() {
		return fleetCmdNone
	}
	it := st.selected()
	if it == nil {
		return fleetCmdNone
	}
	pick := fleetCardSession(it.Card, st.view.Sessions)
	if msg := fleetCardPickRefusal(pick, it.Card.Ticket); msg != "" {
		st.setToast(msg, fleetToastWarn, now)
		return fleetCmdRender
	}
	// A row id the server would refuse as a pin is no pin; without one the
	// server still resolves the tmux session through its allowlist.
	sid := pick.SID
	if !resumeSessionIDRe.MatchString(sid) {
		sid = ""
	}
	t := &fleetChatTarget{Tmux: pick.Tmux, SID: sid, Name: strings.Join(strings.Fields(fleetDisplayText(pick.Name)), " ")}
	st.chat = &fleetChatMode{source: st.view.Source, openedAt: now, target: t}
	st.sessionDraft(t.key())
	st.toast, st.toastUntil = "", time.Time{}
	st.tellFleetChat(it.Card.ID, t.Tmux)
	return fleetCmdChatGet
}

// tellFleetChat mirrors the page's openCardChat: once per card id per run,
// and only while FLEET can take a send, FLEET gets "Andy opened chat on card
// <id> (<tmux>)" as a plain message (no card id, so never an answer), to
// re-read that session and maybe close the card. The loop sends it with the
// chat's first read (st.chatNotice), fire-and-forget. An id or name outside
// fleetSessionNameRe is never typed.
func (st *fleetViewState) tellFleetChat(id, tmux string) {
	if !st.view.Fleet.Alive || st.chatTold[id] || !fleetSessionNameRe.MatchString(id) || !fleetSessionNameRe.MatchString(tmux) {
		return
	}
	if st.chatTold == nil {
		st.chatTold = map[string]bool{}
	}
	st.chatTold[id] = true
	st.chatNotice = &fleetSayRequest{Source: st.view.Source, Text: "Andy opened chat on card " + id + " (" + tmux + ")"}
}

// sessionDraft is the composer of the session chat with key, made on first
// use; memory only.
func (st *fleetViewState) sessionDraft(key string) *textEditor {
	if st.sessionDrafts == nil {
		st.sessionDrafts = map[string]*textEditor{}
	}
	e := st.sessionDrafts[key]
	if e == nil {
		ed := newTextEditor(sendKeysMaxLen)
		e = &ed
		st.sessionDrafts[key] = e
	}
	return e
}

// chatEditor is the open chat's composer: FLEET's, or the session's own.
func (st *fleetViewState) chatEditor() *textEditor {
	if c := st.chat; c != nil && c.target != nil {
		return st.sessionDraft(c.target.key())
	}
	return &st.chatDraft
}

// handleChatKey is handleKey while the chat is open. Esc closes it and the
// draft stays for the next open; Ctrl-C closes it and clears the draft;
// PgUp/PgDn scroll the log; every other key is the composer's. Enter is ignored for fleetArmDelay after the
// chat opens, so the key that opened nothing else cannot send.
func (st *fleetViewState) handleChatKey(k string, inBatch bool, now time.Time) fleetCmd {
	c := st.chat
	switch k {
	case KeyEsc:
		st.chat = nil
		return fleetCmdRender
	case "\x03":
		st.chatEditor().Clear()
		st.chat = nil
		return fleetCmdRender
	case KeyPageUp:
		c.scroll += st.pageStep()
		return fleetCmdRender
	case KeyPageDown:
		c.scroll = max(c.scroll-st.pageStep(), 0)
		return fleetCmdRender
	}
	switch st.chatEditor().key(k, inBatch) {
	case editorSubmit:
		if now.Before(c.openedAt.Add(fleetArmDelay)) {
			return fleetCmdNone
		}
		return st.chatSubmit(now)
	case editorEdited:
		return fleetCmdRender
	}
	return fleetCmdNone
}

// chatSubmit starts the send of the composer's text. The text stays in the
// composer until the send is taken (applyChatSend), so a refusal loses
// nothing.
func (st *fleetViewState) chatSubmit(now time.Time) fleetCmd {
	c := st.chat
	switch {
	case st.chatSending:
		st.setToast("the message is still sending — wait for the result", fleetToastWarn, now)
		return fleetCmdRender
	case st.view.Source != c.source:
		st.chat = nil
		st.setToast(errFleetMoved.Error(), fleetToastWarn, now)
		return fleetCmdRender
	case c.target != nil && c.ended:
		msg := c.err
		if msg == "" {
			msg = "this session has ended"
		}
		st.setToast(msg, fleetToastWarn, now)
		return fleetCmdRender
	case c.target == nil && (!st.view.Fleet.Alive || c.readOnly):
		st.setToast("FLEET is not running", fleetToastWarn, now)
		return fleetCmdRender
	}
	if t := c.target; t != nil {
		st.sessionSend = &fleetSessionSayRequest{Source: c.source, Target: t.Tmux, Text: st.chatEditor().Text(), SID: t.SID}
	} else {
		st.chatSend = &fleetSayRequest{Source: c.source, Text: st.chatDraft.Text()}
	}
	st.chatSending = true
	return fleetCmdChatSend
}

// applyChat takes one chat read. A read for a chat that has since closed or
// been reopened is dropped.
func (st *fleetViewState) applyChat(f fleetChatFetch, now time.Time) {
	c := st.chat
	if c == nil || c != f.chat {
		return
	}
	if f.moved {
		st.chat = nil
		st.setToast(errFleetMoved.Error(), fleetToastWarn, now)
		return
	}
	if c.target != nil {
		st.applySessionRead(c, f, now)
		return
	}
	if f.err != nil {
		if errors.Is(f.err, errFleetUnauthorized) {
			c.err = fleetToastTokenBad
		} else {
			c.err = "could not read FLEET's chat"
		}
		return
	}
	v := f.view
	c.err, c.loaded, c.readOnly, c.reason = "", true, v.ReadOnly, v.Reason
	if v.SessionID == "" && c.sid != "" {
		// A brief discovery failure, not a new session: keep the chat.
		return
	}
	if v.SessionID != "" && c.sid != "" && v.SessionID != c.sid {
		// Another FLEET session: start over from the tail of its transcript.
		c.sid, c.turns, c.pos, c.scroll, c.again = v.SessionID, nil, 0, 0, true
		return
	}
	c.sid = v.SessionID
	c.merge(v.Turns, v.Reset, v.Pos)
}

// merge takes one read's turns: reset replaces the log, otherwise a turn
// replaces the one with its uuid or is appended; the newest fleetChatKeep
// stay. pos is where the next read starts.
func (c *fleetChatMode) merge(turns []fleetChatTurn, reset bool, pos int64) {
	if reset {
		c.turns = nil
	}
	for _, t := range turns {
		if t.UUID == "" {
			continue
		}
		if i := fleetChatTurnIndex(c.turns, t.UUID); i >= 0 {
			c.turns[i] = t
			continue
		}
		c.turns = append(c.turns, t)
	}
	if n := len(c.turns) - fleetChatKeep; n > 0 {
		c.turns = append([]fleetChatTurn(nil), c.turns[n:]...)
	}
	c.pos = pos
}

// fleetSessionRefusalText is the fixed line for a session endpoint's
// refusal code; code is sanitized.
func fleetSessionRefusalText(code string) string {
	switch code {
	case "gone":
		return "this session has ended"
	case "not_session":
		return "this tmux session now runs another session"
	case "not_allowed":
		return "this session is not on a card or the session list any more"
	case "ambiguous":
		return "two or more sessions share this tmux session"
	case "fleet_self":
		return "this is FLEET's own session"
	}
	return "the server refused the chat: " + truncateCells(code, 40)
}

// applySessionRead takes one read of a session chat. A refusal (the session
// ended, its tmux session runs another one, it left the allowlist) ends the
// chat with a toast: no more reads or sends, the log stays. A pane read
// replaces the log with the pane's text.
func (st *fleetViewState) applySessionRead(c *fleetChatMode, f fleetChatFetch, now time.Time) {
	if f.err != nil {
		var ref *fleetSessionRefusal
		switch {
		case errors.Is(f.err, errFleetUnauthorized):
			c.err = fleetToastTokenBad
		case errors.As(f.err, &ref):
			c.ended, c.err = true, fleetSessionRefusalText(ref.Code)
			st.setToast(c.err, fleetToastWarn, now)
		default:
			c.err = "could not read the session"
		}
		return
	}
	v := f.session
	c.err, c.loaded, c.tool = "", true, v.Tool
	if v.Pane != nil {
		c.pane, c.turns, c.pos = v.Pane, nil, 0
		return
	}
	reset := v.Reset || c.pane != nil
	c.pane = nil
	c.merge(v.Turns, reset, v.Pos)
}

func fleetChatTurnIndex(turns []fleetChatTurn, uuid string) int {
	for i, t := range turns {
		if t.UUID == uuid {
			return i
		}
	}
	return -1
}

// applyChatSend takes a finished chat send. A message FLEET (or the
// session) took (typed, queued behind its turn, or held in the outbox)
// leaves its composer when the composer still holds it; anything else keeps
// it there and toasts why.
func (st *fleetViewState) applyChatSend(d fleetChatSent, now time.Time) {
	st.chatSending = false
	r := d.res
	switch {
	case r.CheckErr != nil:
		msg := "could not reach FLEET's service"
		if errors.Is(r.CheckErr, errFleetUnauthorized) {
			msg = fleetToastTokenBad
		}
		st.setToast(msg, fleetToastErr, now)
		return
	case r.Moved:
		st.chat = nil
		st.setToast(errFleetMoved.Error(), fleetToastWarn, now)
		return
	}
	if d.target != nil {
		st.applySessionSend(d, now)
		return
	}
	msg, kind, _ := fleetSendToast(r.Out)
	switch {
	case r.Out.Result.OK && r.Out.Result.Held:
		msg, kind = "FLEET is busy — the message waits in the outbox", fleetToastInfo
	case r.Out.Result.OK && r.Out.Result.Queued:
		msg, kind = "FLEET is busy — it reads your message next", fleetToastInfo
	}
	if r.Out.Result.OK && st.chatDraft.Text() == d.text {
		st.chatDraft.Clear()
	}
	st.setToast(msg, kind, now)
}

// applySessionSend is applyChatSend for a send to a session. The draft it
// clears is the one of the session it went to, whichever chat is open now;
// a gone or not_session refusal ends that session's chat when it is open.
func (st *fleetViewState) applySessionSend(d fleetChatSent, now time.Time) {
	o := d.res.Out
	msg, kind := fleetSessionSendToast(o)
	if o.Result.OK {
		if e := st.sessionDrafts[d.target.key()]; e != nil && e.Text() == d.text {
			e.Clear()
		}
	}
	if code := o.Result.Code; !o.Result.OK && (code == "gone" || code == "not_session") {
		if c := st.chat; c != nil && c.target != nil && c.target.key() == d.target.key() {
			c.ended, c.err = true, msg
		}
	}
	st.setToast(msg, kind, now)
}

// fleetSessionSendToast maps a session say outcome to its toast. Only fixed
// copy and text the client stripped of escapes reach it.
func fleetSessionSendToast(o fleetSayOutcome) (string, fleetToastKind) {
	r := o.Result
	switch {
	case o.Unclear:
		return "delivery not confirmed — check the session", fleetToastWarn
	case o.Status == 401:
		return fleetToastTokenBad, fleetToastErr
	case r.OK && r.Queued:
		return "the session is busy — it reads your message next", fleetToastInfo
	case r.OK:
		return fleetToastSent, fleetToastInfo
	case r.Code == "gone", r.Code == "not_session":
		return fleetSessionRefusalText(r.Code), fleetToastWarn
	case r.Code == "not_submitted":
		return "typed but not submitted — check the session's pane", fleetToastWarn
	case r.Error != "":
		return r.Error, fleetToastErr
	case r.Code != "":
		return r.Code, fleetToastErr
	case o.Body != "":
		return o.Body, fleetToastErr
	}
	return fmt.Sprintf("HTTP %d", o.Status), fleetToastErr
}

// fleetChatLines draws the open chat as the whole view: a head line, the
// log anchored at the bottom (PgUp/PgDn scroll it, fleetClarifyWindow), the
// composer (at most textEditorMaxRows rows, fewer on a short screen) and the
// help row, which a toast or "sending…" replaces. Every string drawn is
// fixed, or from the sanitized chat view, or the composer's own text (which
// holds printable runes only).
func fleetChatLines(st *fleetViewState, cols, rows int, now time.Time) []string {
	c := st.chat
	sep := dim(strings.Repeat("─", cols))
	top := []string{fleetChatHead(st, cols), sep}
	help := fleetToastLine(st, now)
	switch {
	case help != "":
	case st.chatSending:
		help = "sending…"
	default:
		help = dim(truncateCells(fleetHelpChat, cols))
	}
	boxMax := textEditorMaxRows
	if rows > 0 {
		boxMax = min(boxMax, max(1, (rows-len(top)-2)/2))
	}
	box := st.chatEditor().view(cols-2, boxMax)
	for i, l := range box {
		if i == 0 {
			box[i] = "› " + l
		} else {
			box[i] = "  " + l
		}
	}
	bottom := append(append([]string{sep}, box...), help)

	h := -1
	if rows > 0 {
		for len(top)+len(bottom) > rows && len(top) > 0 {
			top = top[:len(top)-1]
		}
		h = max(rows-len(top)-len(bottom), 0)
	}
	log := fleetChatLogLines(c, cols)
	var body []string
	if h < 0 {
		body = log
		c.scroll = 0
	} else {
		st.panelRows = h
		body, c.scroll = fleetClarifyWindow(log, h, c.scroll)
	}
	out := append(append(append([]string(nil), top...), body...), bottom...)
	if rows > 0 && len(out) > rows {
		out = out[len(out)-rows:]
	}
	return out
}

// fleetChatHead is "Chat with FLEET" (or with the session: its name and
// tmux session) with its state at the right edge.
func fleetChatHead(st *fleetViewState, cols int) string {
	c := st.chat
	right, sgr := "live", fleetSGRAlive
	left := "Chat with FLEET"
	if t := c.target; t != nil {
		left = "Chat with " + t.Tmux
		if t.Name != "" && t.Name != t.Tmux {
			left = "Chat with " + t.Name + " · " + t.Tmux
		}
		switch {
		case c.ended:
			right, sgr = "ended", fleetSGRAmber
		case c.tool == "grok":
			right = "live · grok: lines join"
		}
	} else {
		switch {
		case c.readOnly || !st.view.Fleet.Alive:
			right, sgr = "FLEET is not running · read-only", fleetSGRAmber
		case st.view.FleetTool == "grok":
			right = "live · grok: lines join"
		}
	}
	if cellWidth(left)+2+cellWidth(right) > cols {
		return bold(truncateCells(left, cols))
	}
	return bold(left) + strings.Repeat(" ", cols-cellWidth(left)-cellWidth(right)) + colorize(sgr, right)
}

// fleetChatLogLines is the log, oldest first: each turn wrapped under its
// label (you cyan, fleet or agent magenta), a blank line between turns, and
// a read error last. A session read from its pane shows the pane's text.
func fleetChatLogLines(c *fleetChatMode, cols int) []string {
	if !c.loaded && c.err == "" {
		return []string{dim("loading…")}
	}
	if c.pane != nil {
		out := []string{dim(truncateCells("no transcript — the session's screen:", cols))}
		for _, line := range strings.Split(*c.pane, "\n") {
			out = append(out, wrapRunes(line, cols)...)
		}
		if c.err != "" {
			out = append(out, colorize(fleetSGRRed, c.err))
		}
		return out
	}
	w := max(cols-fleetClarifyLabelW, 1)
	pad := strings.Repeat(" ", fleetClarifyLabelW)
	label := func(sgr, word string) string {
		return colorize(sgr, word) + strings.Repeat(" ", fleetClarifyLabelW-len(word))
	}
	var out []string
	if c.loaded && len(c.turns) == 0 {
		msg := "No messages yet. Write to FLEET below."
		if c.target != nil {
			msg = "No messages yet. Write to the session below."
		}
		if c.reason != "" {
			msg = "No chat: " + c.reason
		}
		for _, l := range wrapRunes(msg, cols) {
			out = append(out, dim(l))
		}
	}
	for i, t := range c.turns {
		if i > 0 {
			out = append(out, "")
		}
		head := pad
		switch t.Role {
		case "user":
			head = label(fleetSGRYou, "you")
		case "assistant":
			who := "fleet"
			if c.target != nil {
				who = "agent"
			}
			head = label(fleetSGRFork, who)
		}
		for j, l := range fleetSummaryStyledLines(t.Text, w) {
			if j > 0 {
				head = pad
			}
			out = append(out, head+l)
		}
	}
	if c.err != "" {
		out = append(out, colorize(fleetSGRRed, c.err))
	}
	return out
}
