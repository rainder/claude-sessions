package main

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// The Fleet view's state machine: keys, selection, the confirm panel and send
// results. No terminal I/O here; the modal loop draws this state and runs the
// commands it returns.

// fleetArmDelay is how long Enter is ignored after the confirm panel opens
// (the page's SHEET_ARM_MS), so one stray key cannot send.
const fleetArmDelay = 400 * time.Millisecond

const fleetPageStep = 10 // lines per PgUp/PgDn before the panel height is known

// fleetCmd is what the modal loop does after a key.
type fleetCmd int

const (
	fleetCmdNone    fleetCmd = iota
	fleetCmdRender           // redraw
	fleetCmdClose            // back to the session list
	fleetCmdSend             // start fleetSendJob with st.send
	fleetCmdDismiss          // dismiss st.dismissID
	fleetCmdAttach           // attach to st.attach, then refetch
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
	fleetToastStillDism = "the dismiss is still running — wait for the result"
)

// fleetPanel is the open confirm panel: one option of one card, or (other)
// Andy's own answer about the card.
type fleetPanel struct {
	item       fleetItem
	opt        int
	other      bool   // the Other answer: comment is the whole answer and opt means nothing
	hash       string // cardHashes value when the panel opened
	source     string // the opening state's Source: the send is pinned to it
	comment    string
	openedAt   time.Time
	correction bool // fixed when the panel opens: the card already had an answer
}

type fleetViewState struct {
	view          fleetStateView
	loaded        bool
	err           string // non-empty: the one line shown instead of the cards
	items         []fleetItem
	sel           int
	scroll        int    // details panel scroll, in lines; 0 on a new selection
	listTop       int    // first list line in the list window (renderFleetView keeps it)
	panelRows     int    // the details panel's height at the last render; 0 before one
	unlocked      string // card id a 'c' unlocked for one answer
	panel         *fleetPanel
	sending       bool
	dismissing    bool
	send          *fleetSayRequest    // set with fleetCmdSend; the loop takes it
	dismissID     string              // set with fleetCmdDismiss
	dismissSource string              // the applied state's Source when x was pressed
	attach        *fleetAttachRequest // set with fleetCmdAttach; the loop takes it
	nextAfter     string              // card id just sent; the next applyState selects the next open card
	justSent      string              // card id our own answer reached; locked until a state shows it answered or gone
	unclear       string              // card id whose send result is unknown; locked until the next applied state
	toast         string
	toastKind     fleetToastKind
	toastUntil    time.Time

	// The ship screen (fleet_ship.go), shown while no card needs Andy.
	frame    int  // animation frame; the loop advances it every fleetShipFrame while the ship shows
	listMode bool // Enter on the ship screen: show the SENT cards as the normal list
}

// fleetSendResult is what a send job hands back to the loop.
type fleetSendResult struct {
	CardID   string
	CheckErr error // the pre-send state fetch failed; nothing was sent
	Stale    bool  // card gone or its hash moved; nothing was sent
	Moved    bool  // FLEET is on another host than the panel's state; nothing was sent
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
// changed, and only then posts the answer. A request with no card id is an
// Other answer, a plain line as the page sends it: it has no card to check,
// so it is posted at once. Runs off the UI goroutine.
func fleetSendJob(c *fleetClient, req fleetSayRequest) fleetSendResult {
	r := fleetSendResult{CardID: req.CardID}
	if req.CardID == "" {
		r.Out = c.Say(req)
		return r
	}
	v, err := c.GetState()
	if err != nil {
		r.CheckErr = err
		return r
	}
	if !fleetCardStillAsked(v, req.CardID, req.CardHash) {
		r.Stale = true
		return r
	}
	// Answered elsewhere since the panel opened: the server would refuse it
	// the same way, so skip the round trip and show its answer.
	if _, answered := v.Answered[req.CardID]; answered && !req.Correction {
		r.Out = fleetSayOutcome{Status: http.StatusOK, Result: fleetSayResult{Code: "already_answered"}}
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
		var down *fleetHostDownError
		switch {
		case errors.Is(err, errFleetUnauthorized):
			st.err = fleetToastTokenBad
		case errors.As(err, &down):
			st.err = down.Error() // the host is a fleetHostName
		default:
			st.err = fleetErrNoService
		}
		return
	}
	// A send whose result was unclear may or may not have reached FLEET:
	// this state decides, whatever it shows.
	st.unclear = ""
	old := st.items
	oldSel := st.sel
	st.err, st.view, st.items = "", v, fleetItems(v)
	if len(st.items) == 0 || !fleetOnlySent(st.items) {
		// An open or unconfirmed card shows the list anyway; with no card
		// at all there is nothing to list. Either way the next all-sent
		// state starts on the ship again.
		st.listMode = false
	}

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
		} else if _, answered := v.Answered[id]; answered && !p.correction && !p.other {
			// The panel opened on an unanswered card and an answer landed
			// since: sending now would be a second answer. When that answer
			// is our own, "another device" would be wrong. An Other answer is
			// a plain line the card does not track, so it may follow one.
			st.panel = nil
			if id != ownSent {
				st.setToast(fleetToastAnswered, fleetToastErr, now)
			}
		}
	}
}

// pageStep is PgUp/PgDn's scroll in the details panel: its height less the
// two more lines, so a page skips no line; fleetPageStep before a render.
func (st *fleetViewState) pageStep() int {
	if st.panelRows <= 0 {
		return fleetPageStep
	}
	return max(st.panelRows-2, 1)
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
	if st.shipOn() {
		if cmd, ok := st.handleShipKey(k); ok {
			return cmd
		}
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
		st.scroll -= st.pageStep()
		if st.scroll < 0 {
			st.scroll = 0
		}
		return fleetCmdRender
	case KeyPageDown:
		st.scroll += st.pageStep()
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
		if st.listMode {
			// The SENT list opened from the ship screen: one level back.
			st.listMode = false
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
	case "a":
		// A paste that holds an "a" must not hand the terminal away.
		if inBatch {
			return fleetCmdNone
		}
		// The attach hands the terminal away until Andy detaches; a send or
		// dismiss result would land unseen behind it.
		if st.sending || st.dismissing {
			msg := fleetToastStillSend
			if !st.sending {
				msg = fleetToastStillDism
			}
			st.setToast(msg, fleetToastWarn, now)
			return fleetCmdRender
		}
		it := st.selected()
		if it == nil {
			return fleetCmdNone
		}
		pick := fleetAttachTarget(it.Card, st.view.Sessions)
		if pick.Many > 0 {
			// Card.Ticket is the sanitized copy, safe to draw.
			st.setToast(fmt.Sprintf("%d sessions for %s", pick.Many, it.Card.Ticket), fleetToastWarn, now)
			return fleetCmdRender
		}
		if pick.Tmux == "" {
			return fleetCmdNone
		}
		st.attach = &fleetAttachRequest{Tmux: pick.Tmux, Source: st.view.Source}
		return fleetCmdAttach
	case "x":
		it := st.selected()
		if it == nil || st.sending || st.dismissing || it.Answer == nil || it.Answer.latest().Status != fleetAttemptUnconfirmed {
			return fleetCmdNone
		}
		st.dismissID, st.dismissSource = it.Card.ID, st.view.Source
		st.dismissing = true
		return fleetCmdDismiss
	case "o":
		return st.openOther(now)
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
	if it.Card.ID == st.unclear {
		// The last send may have landed; the refetch it asked for decides.
		st.setToast("delivery not confirmed — checking the card first", fleetToastWarn, now)
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
		source:     st.view.Source,
		openedAt:   now,
		correction: it.Answer != nil,
	}
	return fleetCmdRender
}

// openOther opens the Other panel on the selected card: Andy's own answer,
// for a card with options when none fits. Like the page's Other button it
// stays on while the options are locked (answered, just sent, unclear): the
// answer is a plain line the card does not track, so it is never a second
// answer. The card hash only lets a poll close the panel when the card moves.
func (st *fleetViewState) openOther(now time.Time) fleetCmd {
	it := st.selected()
	if it == nil || len(it.Card.Options) == 0 || it.Card.ID == "" || !st.view.Fleet.Alive || st.sending {
		return fleetCmdNone
	}
	hash := st.view.CardHashes[it.Card.ID]
	if hash == "" {
		return fleetCmdNone
	}
	st.panel = &fleetPanel{item: *it, other: true, hash: hash, source: st.view.Source, openedAt: now}
	return fleetCmdRender
}

// handleOtherKey is handlePanelKey for the Other panel. A pasted newline
// becomes a space; Enter on an empty answer does nothing.
func (st *fleetViewState) handleOtherKey(k string, inBatch bool, now time.Time) fleetCmd {
	p := st.panel
	id := p.item.Card.ID
	switch k {
	case KeyEsc, "\x03":
		st.panel = nil
		return fleetCmdRender
	case KeyEnter:
		if inBatch {
			p.comment, _ = fleetEditOther(p.comment, " ", id)
			return fleetCmdRender
		}
		if now.Before(p.openedAt.Add(fleetArmDelay)) || strings.TrimSpace(p.comment) == "" {
			return fleetCmdNone
		}
		// No card id: the server takes it as a plain line, so it neither
		// records an answer nor locks the card (the page's Other does the same).
		st.send = &fleetSayRequest{Source: p.source, Text: fleetOtherText(id, p.comment)}
		st.panel = nil
		st.sending = true
		st.setToast("sending…", fleetToastInfo, now)
		return fleetCmdSend
	}
	var edit bool
	p.comment, edit = fleetEditOther(p.comment, k, id)
	if edit {
		return fleetCmdRender
	}
	return fleetCmdNone
}

func (st *fleetViewState) handlePanelKey(k string, inBatch bool, now time.Time) fleetCmd {
	p := st.panel
	if p.other {
		return st.handleOtherKey(k, inBatch, now)
	}
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
			Source:     p.source,
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

// attachable reports whether the selected card resolves to exactly one live
// session, which is when the view offers the attach key.
func (st *fleetViewState) attachable() bool {
	it := st.selected()
	return it != nil && st.panel == nil && fleetAttachTarget(it.Card, st.view.Sessions).Tmux != ""
}

// applyAttach takes the attach's outcome once the terminal is back.
func (st *fleetViewState) applyAttach(err error, now time.Time) {
	switch {
	case err == nil:
	case errors.Is(err, errFleetMoved):
		st.setToast(errFleetMoved.Error(), fleetToastWarn, now)
	default:
		st.setToast("attach failed: "+fleetDisplayText(err.Error()), fleetToastErr, now)
	}
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
	if r.Moved {
		st.setToast(errFleetMoved.Error(), fleetToastWarn, now)
		return true
	}
	msg, kind, refetch := fleetSendToast(r.Out)
	st.setToast(msg, kind, now)
	if r.CardID == "" {
		// An Other answer: no card records it, so nothing locks, moves on
		// or unlocks, and an earlier card send's locks stay as they are.
		return refetch
	}
	if r.Out.Unclear {
		// The answer may have reached FLEET: lock the card until the next
		// applied state, and fetch that now rather than at the next poll
		// (this overrides the toast table's refetch for Unclear).
		st.unclear = r.CardID
		return true
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
