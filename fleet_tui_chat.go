package main

import (
	"errors"
	"strings"
	"time"
)

// The Fleet view's chat with FLEET itself, opened with i on the card list:
// FLEET's recent conversation (GET /fleet/chat, the page's Chat tab) above a
// multi-line composer (text_editor.go) whose Enter sends a plain message
// (POST /fleet/say with no card). State, keys and drawing are here; the loop
// (fleet_tui.go) runs the jobs the commands ask for.

const (
	// fleetChatPoll is how often the open chat is read.
	fleetChatPoll = 2 * time.Second
	// fleetChatKeep is how many turns the chat keeps (and asks for).
	fleetChatKeep = 100
	// fleetHelpChat is the chat screen's key help.
	fleetHelpChat = "Enter send · alt-Enter / \\⏎ new line · PgUp/PgDn scroll · Esc close"
)

// fleetChatMode is the open chat. source is the state's Source when it
// opened: reads and sends are pinned to it like a card answer.
type fleetChatMode struct {
	source   string
	turns    []fleetChatTurn // oldest first, at most fleetChatKeep, every string sanitized
	pos      int64           // the last read's pos; 0 reads the tail again
	sid      string          // the FLEET session the turns belong to
	loaded   bool            // the first read landed
	readOnly bool            // FLEET is down: the last session, mirrored
	reason   string          // why there is no live chat, from the server
	err      string          // a fixed line when the chat could not be read
	scroll   int             // lines scrolled up from the newest
	openedAt time.Time
	// again: the last read found another FLEET session and started over
	// from its tail; the loop reads again at once.
	again bool
}

// fleetChatFetch is one chat read's result, with the chat it was read for.
type fleetChatFetch struct {
	chat  *fleetChatMode
	view  fleetChatView
	err   error
	moved bool // FLEET is on another host than the chat; nothing was read
}

// fleetChatSent is a finished chat send with the text it sent.
type fleetChatSent struct {
	text string
	res  fleetSendResult
}

// canChat reports whether i opens the chat: a loaded state from one host
// and nothing else open. FLEET may be down; the chat then shows its last
// session and refuses to send.
func (st *fleetViewState) canChat() bool {
	return st.loaded && st.err == "" && len(st.view.Ambiguous) == 0 && st.view.Source != "" &&
		st.panel == nil && st.batch == nil && st.clarify == nil && st.chat == nil && !st.shipOn()
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

// handleChatKey is handleKey while the chat is open. Esc and Ctrl-C close it
// (the draft stays for the next open); PgUp/PgDn scroll the log; every
// other key is the composer's. Enter is ignored for fleetArmDelay after the
// chat opens, so the key that opened nothing else cannot send.
func (st *fleetViewState) handleChatKey(k string, inBatch bool, now time.Time) fleetCmd {
	c := st.chat
	switch k {
	case KeyEsc, "\x03":
		st.chat = nil
		return fleetCmdRender
	case KeyPageUp:
		c.scroll += st.pageStep()
		return fleetCmdRender
	case KeyPageDown:
		c.scroll = max(c.scroll-st.pageStep(), 0)
		return fleetCmdRender
	}
	switch st.chatDraft.key(k, inBatch) {
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
	case !st.view.Fleet.Alive || c.readOnly:
		st.setToast("FLEET is not running", fleetToastWarn, now)
		return fleetCmdRender
	}
	st.chatSend = &fleetSayRequest{Source: c.source, Text: st.chatDraft.Text()}
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
	if v.Reset {
		c.turns = nil
	}
	for _, t := range v.Turns {
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
	c.pos = v.Pos
}

func fleetChatTurnIndex(turns []fleetChatTurn, uuid string) int {
	for i, t := range turns {
		if t.UUID == uuid {
			return i
		}
	}
	return -1
}

// applyChatSend takes a finished chat send. A message FLEET took (typed,
// queued behind its turn, or held in the outbox) leaves the composer when
// the composer still holds it; anything else keeps it there and toasts why.
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
	box := st.chatDraft.view(cols-2, boxMax)
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

// fleetChatHead is "Chat with FLEET" with FLEET's state at the right edge.
func fleetChatHead(st *fleetViewState, cols int) string {
	c := st.chat
	right, sgr := "live", fleetSGRAlive
	switch {
	case c.readOnly || !st.view.Fleet.Alive:
		right, sgr = "FLEET is not running · read-only", fleetSGRAmber
	case st.view.FleetTool == "grok":
		right = "live · grok: lines join"
	}
	left := "Chat with FLEET"
	if cellWidth(left)+2+cellWidth(right) > cols {
		return bold(truncateCells(left, cols))
	}
	return bold(left) + strings.Repeat(" ", cols-cellWidth(left)-cellWidth(right)) + colorize(sgr, right)
}

// fleetChatLogLines is the log, oldest first: each turn wrapped under its
// label (you cyan, fleet magenta), a blank line between turns, and a read
// error last.
func fleetChatLogLines(c *fleetChatMode, cols int) []string {
	if !c.loaded && c.err == "" {
		return []string{dim("loading…")}
	}
	w := max(cols-fleetClarifyLabelW, 1)
	pad := strings.Repeat(" ", fleetClarifyLabelW)
	label := func(sgr, word string) string {
		return colorize(sgr, word) + strings.Repeat(" ", fleetClarifyLabelW-len(word))
	}
	var out []string
	if c.loaded && len(c.turns) == 0 {
		msg := "No messages yet. Write to FLEET below."
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
			head = label(fleetSGRFork, "fleet")
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
