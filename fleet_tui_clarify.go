package main

import (
	"errors"
	"strings"
	"time"
)

// The Fleet view's Clarify side chat: a per-card conversation with a fork of
// FLEET, opened with k on an open decision card. This file is keys and state
// only; fleet_tui.go draws it and runs the commands it returns.

// fleetClarifyMode is the open side chat. item is the card it was opened on
// (kept fresh by applyState), source the service that state came from: the
// requests are pinned to it like a send.
type fleetClarifyMode struct {
	item     fleetItem
	source   string
	view     fleetClarifyView
	loaded   bool // the first GET landed
	input    string
	openedAt time.Time
	scroll   int
	err      string // a fixed line when the chat could not be read
}

// fleetClarifyRequest is a question to ask; the loop takes it.
type fleetClarifyRequest struct{ Source, CardID, Text string }

// fleetClarifyToasts is the toast for each refusal code.
var fleetClarifyToasts = map[string]string{
	"busy":             "a question is already running",
	"card_gone":        "card is gone",
	"not_live":         "FLEET is not running",
	"ambiguous":        "two FLEETs are running",
	"unsupported_tool": "Clarify needs a Claude FLEET",
	"empty":            "nothing to draft yet",
	"too_long":         "question too long",
	"not_found":        "no draft",
}

// canClarify reports whether k opens Clarify on the selected item: an open
// decision card (APPROVE or DECIDE, not SENT) with an id on a live claude
// FLEET, with nothing else open. The answer locks do not apply: a chat is no
// answer.
func (st *fleetViewState) canClarify() bool {
	it := st.selected()
	return it != nil && it.Card.ID != "" && it.Section != fleetSecSent &&
		st.view.Fleet.Alive && st.view.FleetTool != "grok" &&
		st.panel == nil && st.clarify == nil && !st.shipOn()
}

func (st *fleetViewState) openClarify(now time.Time) fleetCmd {
	if !st.canClarify() {
		return fleetCmdNone
	}
	st.clarify = &fleetClarifyMode{item: *st.selected(), source: st.view.Source, openedAt: now}
	st.toast, st.toastUntil = "", time.Time{}
	return fleetCmdClarifyGet
}

// busy is true while a fork runs for the card.
func (c *fleetClarifyMode) busy() bool { return c.view.Pending != nil }

// applyClarify takes a chat read. A late answer for a chat that has since
// closed, or been reopened on another card, is dropped.
func (st *fleetViewState) applyClarify(v fleetClarifyView, err error, now time.Time) {
	c := st.clarify
	if c == nil {
		return
	}
	if err != nil {
		if errors.Is(err, errFleetUnauthorized) {
			c.err = fleetToastTokenBad
		} else {
			c.err = "could not read the chat"
		}
		return
	}
	if v.Card != "" && v.Card != c.item.Card.ID {
		return
	}
	c.view, c.loaded, c.err = v, true, ""
}

// applyClarifyRefusal toasts a refusal code from ask, draft or discard.
func (st *fleetViewState) applyClarifyRefusal(code string, now time.Time) {
	msg, ok := fleetClarifyToasts[code]
	if !ok {
		msg = "clarify failed"
	}
	st.setToast(msg, fleetToastWarn, now)
}

// handleClarifyKey is handleKey while the side chat is open. Esc, q and
// Ctrl-C never close the view from here: Esc and Ctrl-C go back to the list,
// q is a letter.
func (st *fleetViewState) handleClarifyKey(k string, inBatch bool, now time.Time) fleetCmd {
	c := st.clarify
	id := c.item.Card.ID
	fits := func(t string) bool { return len(t) <= clarifyQuestionMax }
	switch k {
	case KeyEsc, "\x03":
		st.clarify = nil
		return fleetCmdRender
	case KeyPageUp:
		c.scroll = max(c.scroll-st.pageStep(), 0)
		return fleetCmdRender
	case KeyPageDown:
		c.scroll += st.pageStep()
		return fleetCmdRender
	case KeyEnter:
		if inBatch {
			c.input, _ = fleetEditText(c.input, " ", fits)
			return fleetCmdRender
		}
		if now.Before(c.openedAt.Add(fleetArmDelay)) || strings.TrimSpace(c.input) == "" || c.busy() {
			return fleetCmdNone
		}
		st.clarifyReq = &fleetClarifyRequest{Source: c.source, CardID: id, Text: c.input}
		c.input = ""
		return fleetCmdClarifyAsk
	case "\x04": // ctrl-d: draft an answer from the chat so far
		if len(c.view.Turns) == 0 || c.busy() {
			return fleetCmdNone
		}
		return fleetCmdClarifyDraft
	case "s", "x":
		if c.input == "" && c.view.Draft != nil {
			if k == "x" {
				return fleetCmdClarifyDiscard
			}
			return st.draftToOther(now)
		}
	}
	var edit bool
	c.input, edit = fleetEditText(c.input, k, fits)
	if edit {
		return fleetCmdRender
	}
	return fleetCmdNone
}

// draftToOther leaves the chat and opens the Other panel on the card with
// the draft as its answer, so Andy reads it once more and sends it through
// the usual path. The comment is set directly: the Other box's ASCII filter
// is for typed keys. It is cut to what a send may hold.
func (st *fleetViewState) draftToOther(now time.Time) fleetCmd {
	c := st.clarify
	draft := strings.Join(strings.Fields(c.view.Draft.Text), " ")
	if it := st.selected(); it == nil || it.Card.ID != c.item.Card.ID {
		return fleetCmdNone
	}
	st.clarify = nil
	cmd := st.openOther(now)
	if st.panel == nil {
		// Without options there is no Other panel; stay in the chat.
		st.clarify = c
		st.setToast("this card has no Other answer", fleetToastWarn, now)
		return fleetCmdRender
	}
	id := c.item.Card.ID
	st.panel.comment = truncBytesHead(draft, sendKeysMaxLen-len(fleetOtherText(id, "")))
	return cmd
}
