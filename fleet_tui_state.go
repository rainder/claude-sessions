package main

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// The Fleet view's state machine: keys, selection, the confirm panel and send
// results. No terminal I/O here; the modal loop draws this state and runs the
// commands it returns.

// fleetArmDelay is how long Enter is ignored after the confirm panel opens
// (the page's SHEET_ARM_MS), so one stray key cannot send.
const fleetArmDelay = 400 * time.Millisecond

const fleetPageStep = 10 // lines per PgUp/PgDn

// fleetCmd is what the modal loop does after a key.
type fleetCmd int

const (
	fleetCmdNone    fleetCmd = iota
	fleetCmdRender           // redraw
	fleetCmdClose            // back to the session list
	fleetCmdSend             // start fleetSendJob with st.send
	fleetCmdDismiss          // dismiss st.dismissID
)

type fleetToastKind int

const (
	fleetToastInfo fleetToastKind = iota // 3 s
	fleetToastWarn                       // yellow, 6 s
	fleetToastErr                        // red, 6 s
)

const (
	fleetToastSent      = "sent"
	fleetToastChanged   = "the card changed"
	fleetToastAnswered  = "already answered on another device"
	fleetToastUnclear   = "delivery not confirmed — check FLEET's chat"
	fleetToastTokenBad  = "service token rejected"
	fleetErrNoService   = "no FLEET service answered on this host — start it with: claude-sessions service restart"
	fleetToastStillSend = "the answer is still sending — wait for the result"
)

// fleetPanel is the open confirm panel: one option of one card.
type fleetPanel struct {
	item       fleetItem
	opt        int
	hash       string // cardHashes value when the panel opened
	comment    string
	openedAt   time.Time
	correction bool // fixed when the panel opens: the card already had an answer
}

type fleetViewState struct {
	view       fleetStateView
	loaded     bool
	err        string // non-empty: the one line shown instead of the cards
	items      []fleetItem
	sel        int
	scroll     int
	unlocked   string // card id a 'c' unlocked for one answer
	panel      *fleetPanel
	sending    bool
	send       *fleetSayRequest // set with fleetCmdSend; the loop takes it
	dismissID  string           // set with fleetCmdDismiss
	nextAfter  string           // card id just sent; the next applyState selects the next open card
	justSent   string           // card id our own answer reached; locked until a state shows it answered or gone
	toast      string
	toastKind  fleetToastKind
	toastUntil time.Time
}

// fleetSendResult is what a send job hands back to the loop.
type fleetSendResult struct {
	CardID   string
	CheckErr error // the pre-send state fetch failed; nothing was sent
	Stale    bool  // card gone or its hash moved; nothing was sent
	Out      fleetSayOutcome
}

// fleetCardStillAsked reports whether v still holds the decision card id with
// the hash an answer was prepared against (the page's sheetValid).
func fleetCardStillAsked(v fleetStateView, cardID, hash string) bool {
	if cardID == "" {
		return false
	}
	for _, c := range v.Cards {
		if c.ID != cardID {
			continue
		}
		if !fleetPushKind(c.Kind) {
			return false
		}
		h, ok := v.CardHashes[cardID]
		return ok && h == hash
	}
	return false
}

// fleetSendToast maps a say outcome to the toast the view shows, and whether
// the state should be fetched again now. Only fixed copy and text the client
// already stripped of escapes reach the toast.
func fleetSendToast(o fleetSayOutcome) (msg string, kind fleetToastKind, refetch bool) {
	switch {
	case o.Unclear:
		return fleetToastUnclear, fleetToastWarn, false
	case o.Status == 401:
		return fleetToastTokenBad, fleetToastErr, false
	case o.Status/100 == 2:
		r := o.Result
		if r.OK {
			if r.Queued {
				return "FLEET is busy — it reads your answer next", fleetToastInfo, true
			}
			return fleetToastSent, fleetToastInfo, true
		}
		switch r.Code {
		case "not_submitted":
			return "typed but not submitted — check FLEET's pane", fleetToastWarn, true
		case "already_answered":
			return fleetToastAnswered, fleetToastErr, true
		case "card_gone", "card_changed":
			return fleetToastChanged, fleetToastErr, true
		}
		if r.Error != "" {
			return r.Error, fleetToastErr, false
		}
		if r.Code != "" {
			return r.Code, fleetToastErr, false
		}
		return fmt.Sprintf("HTTP %d", o.Status), fleetToastErr, false
	case o.Status == 400:
		if o.Result.Error != "" {
			return o.Result.Error, fleetToastErr, false
		}
		return o.Body, fleetToastErr, false
	}
	if o.Body != "" {
		return o.Body, fleetToastErr, false
	}
	return fmt.Sprintf("HTTP %d", o.Status), fleetToastErr, false
}

// fleetSendJob re-reads the state, refuses locally when the card is gone or
// changed, and only then posts the answer. Runs off the UI goroutine.
func fleetSendJob(c *fleetClient, req fleetSayRequest) fleetSendResult {
	r := fleetSendResult{CardID: req.CardID}
	v, err := c.GetState()
	if err != nil {
		r.CheckErr = err
		return r
	}
	if !fleetCardStillAsked(v, req.CardID, req.CardHash) {
		r.Stale = true
		return r
	}
	r.Out = c.Say(req)
	return r
}

func fleetToastDuration(kind fleetToastKind) time.Duration {
	if kind == fleetToastInfo {
		return 3 * time.Second
	}
	return 6 * time.Second
}

// setToast shows msg as one line until its kind's time runs out.
func (st *fleetViewState) setToast(msg string, kind fleetToastKind, now time.Time) {
	st.toast = strings.Join(strings.Fields(msg), " ")
	st.toastKind = kind
	st.toastUntil = now.Add(fleetToastDuration(kind))
}

// fleetItemOpen is a card that can take a first answer.
func fleetItemOpen(it fleetItem) bool { return it.Card.ID != "" && it.Answer == nil }

// fleetItemIndex is the index of the item with card id, -1 when none.
func fleetItemIndex(items []fleetItem, id string) int {
	if id == "" {
		return -1
	}
	for i, it := range items {
		if it.Card.ID == id {
			return i
		}
	}
	return -1
}

// applyState takes one /fleet/state result.
func (st *fleetViewState) applyState(v fleetStateView, err error, now time.Time) {
	st.loaded = true
	if err != nil {
		if errors.Is(err, errFleetUnauthorized) {
			st.err = fleetToastTokenBad
		} else {
			st.err = fleetErrNoService
		}
		return
	}
	old := st.items
	oldSel := st.sel
	st.err, st.view, st.items = "", v, fleetItems(v)

	// Keep the selection on its card; an id-less card keeps its index.
	selID := ""
	if oldSel >= 0 && oldSel < len(old) {
		selID = old[oldSel].Card.ID
	}
	if i := fleetItemIndex(st.items, selID); i >= 0 {
		st.sel = i
	} else {
		st.sel = oldSel
		if selID != "" {
			st.scroll = 0
		}
	}

	// After a send, move on to the next open card, in the order the list
	// had before the answered card moved to SENT.
	if st.nextAfter != "" {
		next := -1
		for j := fleetItemIndex(old, st.nextAfter) + 1; j > 0 && j < len(old) && next < 0; j++ {
			if i := fleetItemIndex(st.items, old[j].Card.ID); i >= 0 && fleetItemOpen(st.items[i]) {
				next = i
			}
		}
		if next < 0 {
			for i, it := range st.items {
				if fleetItemOpen(it) {
					next = i
					break
				}
			}
		}
		if next >= 0 && next != st.sel {
			st.sel, st.scroll = next, 0
		}
		st.nextAfter = ""
	}
	if st.sel >= len(st.items) {
		st.sel = len(st.items) - 1
	}
	if st.sel < 0 {
		st.sel = 0
	}

	if st.unlocked != "" {
		if i := fleetItemIndex(st.items, st.unlocked); i < 0 || st.items[i].Answer == nil {
			st.unlocked = ""
		}
	}

	// Our own answer ends its lock once a state shows it recorded, or the
	// card gone. A poll read before the answer landed keeps the lock.
	ownSent := st.justSent
	if st.justSent != "" {
		if i := fleetItemIndex(st.items, st.justSent); i < 0 || st.items[i].Answer != nil {
			st.justSent = ""
		}
	}

	if p := st.panel; p != nil {
		id := p.item.Card.ID
		if !fleetCardStillAsked(v, id, p.hash) {
			st.panel = nil
			st.setToast(fleetToastChanged, fleetToastErr, now)
		} else if _, answered := v.Answered[id]; answered && !p.correction {
			// The panel opened on an unanswered card and an answer landed
			// since: sending now would be a second answer. When that answer
			// is our own, "another device" would be wrong.
			st.panel = nil
			if id != ownSent {
				st.setToast(fleetToastAnswered, fleetToastErr, now)
			}
		}
	}
}

// selected is the selected item, nil when the list is empty or not shown.
func (st *fleetViewState) selected() *fleetItem {
	if st.err != "" || st.sel < 0 || st.sel >= len(st.items) {
		return nil
	}
	return &st.items[st.sel]
}

// handleKey applies one key. inBatch is true while the key arrived in the
// same read as others (a paste), which never presses Enter on the panel.
func (st *fleetViewState) handleKey(k string, inBatch bool, now time.Time) fleetCmd {
	if st.panel != nil {
		return st.handlePanelKey(k, inBatch, now)
	}
	switch k {
	case KeyUp:
		if st.sel > 0 {
			st.sel--
		}
		st.scroll = 0
		return fleetCmdRender
	case KeyDown:
		if st.sel < len(st.items)-1 {
			st.sel++
		}
		st.scroll = 0
		return fleetCmdRender
	case KeyPageUp:
		st.scroll -= fleetPageStep
		if st.scroll < 0 {
			st.scroll = 0
		}
		return fleetCmdRender
	case KeyPageDown:
		st.scroll += fleetPageStep
		return fleetCmdRender
	case KeyEsc, "q", "Q", "\x03":
		if st.sending {
			st.setToast(fleetToastStillSend, fleetToastWarn, now)
			return fleetCmdRender
		}
		if st.unlocked != "" {
			st.unlocked = ""
			return fleetCmdRender
		}
		return fleetCmdClose
	case "c":
		it := st.selected()
		if it == nil || it.Answer == nil || st.sending {
			return fleetCmdNone
		}
		st.unlocked = it.Card.ID
		st.setToast("pick the correction (1-9)", fleetToastInfo, now)
		return fleetCmdRender
	case "x":
		it := st.selected()
		if it == nil || st.sending || it.Answer == nil || it.Answer.latest().Status != fleetAttemptUnconfirmed {
			return fleetCmdNone
		}
		st.dismissID = it.Card.ID
		return fleetCmdDismiss
	}
	it := st.selected()
	if it == nil {
		return fleetCmdNone
	}
	opt, ok := fleetOptionIndex(k, len(it.Card.Options))
	if !ok || it.Card.ID == "" || !st.view.Fleet.Alive || st.sending {
		return fleetCmdNone
	}
	if it.Card.ID == st.justSent {
		// The list may still show our own answer's card as open until the
		// refetch lands; a panel now would be a second answer.
		st.setToast("sent — waiting for FLEET", fleetToastInfo, now)
		return fleetCmdRender
	}
	if it.Answer != nil && st.unlocked != it.Card.ID {
		st.setToast("answered — press c to send a correction", fleetToastInfo, now)
		return fleetCmdRender
	}
	// Without the server's hash the send could never pass the re-check.
	if it.Card.Options[opt].Send == "" || st.view.CardHashes[it.Card.ID] == "" {
		return fleetCmdNone
	}
	st.panel = &fleetPanel{
		item:       *it,
		opt:        opt,
		hash:       st.view.CardHashes[it.Card.ID],
		openedAt:   now,
		correction: it.Answer != nil,
	}
	return fleetCmdRender
}

func (st *fleetViewState) handlePanelKey(k string, inBatch bool, now time.Time) fleetCmd {
	p := st.panel
	opt := p.item.Card.Options[p.opt]
	switch k {
	case KeyEsc, "\x03":
		st.panel = nil
		return fleetCmdRender
	case KeyEnter:
		if inBatch || now.Before(p.openedAt.Add(fleetArmDelay)) {
			return fleetCmdNone
		}
		st.send = &fleetSayRequest{
			Text:       fleetSendText(opt.Send, p.comment),
			CardID:     p.item.Card.ID,
			CardHash:   p.hash,
			OptionKey:  opt.Key,
			Correction: p.correction,
		}
		st.panel = nil
		st.sending = true
		st.setToast("sending…", fleetToastInfo, now)
		return fleetCmdSend
	}
	// The bool says the key is an edit key, not that the comment changed.
	var edit bool
	p.comment, edit = fleetEditComment(p.comment, k, opt.Send)
	if edit {
		return fleetCmdRender
	}
	return fleetCmdNone
}

// applySend takes a finished send job and reports whether to fetch the state
// again now.
func (st *fleetViewState) applySend(r fleetSendResult, now time.Time) (refetch bool) {
	st.sending = false
	if r.CheckErr != nil {
		st.setToast("could not re-check the card: "+fleetDisplayText(r.CheckErr.Error()), fleetToastErr, now)
		return false
	}
	if r.Stale {
		st.setToast(fleetToastChanged, fleetToastErr, now)
		return true
	}
	msg, kind, refetch := fleetSendToast(r.Out)
	st.setToast(msg, kind, now)
	if r.Out.Unclear {
		return refetch
	}
	if r.Out.Result.OK {
		st.nextAfter = r.CardID
		st.unlocked = ""
	}
	// The server records an OK and a not_submitted answer before it replies.
	if r.Out.Result.OK || r.Out.Result.Code == "not_submitted" {
		st.justSent = r.CardID
	}
	return refetch
}
