package main

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// R on the Fleet view's card list: send FLEET's pick (the lean option r
// sends) on every open card at once, after one confirm panel. The set is
// what r would accept on each card; the job sends the cards one by one
// through the same send path as r, spaced past the server's /fleet/say gap.

// fleetBatchGap is the wait between two sends of one batch: the server's
// /fleet/say refuses a send within fleetSayGap (1 s) of the last one and does
// not hold card answers for later.
const fleetBatchGap = 1100 * time.Millisecond

// fleetBatchItem is one card of the batch: the request r would make for it,
// snapshotted when the panel opened, and the names the panel and the result
// toast show (already sanitized view fields).
type fleetBatchItem struct {
	Name   string // the card's ticket, else its title
	Option string // the option's label
	Req    fleetSayRequest
}

// fleetBatchSkip is an open, unanswered card whose lean cannot be sent.
type fleetBatchSkip struct {
	Name, Reason string
}

// fleetBatchPanel is the open R confirm panel.
type fleetBatchPanel struct {
	items    []fleetBatchItem
	skipped  []fleetBatchSkip
	openedAt time.Time
	scroll   int
}

// fleetBatchResult is what the batch job hands back: the items it was given
// and one result per item it tried, in order. A send that stops the batch
// is the last result; the items after it were not tried.
type fleetBatchResult struct {
	Items   []fleetBatchItem
	Results []fleetSendResult
}

// fleetCardName is how the batch panel and toast name a card.
func fleetCardName(c fleetCardView) string {
	if c.Ticket != "" {
		return c.Ticket
	}
	return c.Title
}

// fleetLeanSkipReason is the batch panel's short reason why card c's lean
// cannot be sent, "" when it can (fleetLeanRefusal's cases, shorter).
func fleetLeanSkipReason(c fleetCardView) string {
	if c.Lean == "" {
		return "no FLEET pick"
	}
	n := 0
	for _, o := range c.Options {
		if o.Key == c.Lean {
			n++
		}
	}
	switch {
	case n == 0:
		return "pick names no option"
	case n > 1:
		return "pick names two options"
	}
	_, msg := fleetLeanRefusal(c)
	switch {
	case msg == "":
		return ""
	case strings.HasPrefix(msg, "FLEET's pick has nothing"):
		return "pick has nothing to send"
	}
	return "pick is past option 9"
}

// leanBatch is what R would send: every open card in list order that r would
// answer (unanswered, not locked, pickRefusal passes, a card hash, a lean that
// resolves to one sendable option), each with r's request, and the open
// unanswered cards whose lean does not resolve. Answered, locked and
// hash-less cards are left out of both.
func (st *fleetViewState) leanBatch() (items []fleetBatchItem, skipped []fleetBatchSkip) {
	if st.err != "" {
		return nil, nil
	}
	for i := range st.items {
		it := &st.items[i]
		if !fleetItemOpen(*it) {
			continue
		}
		if _, _, blocked := st.pickRefusal(it); blocked {
			continue
		}
		name := fleetCardName(it.Card)
		opt, msg := fleetLeanRefusal(it.Card)
		if msg != "" {
			skipped = append(skipped, fleetBatchSkip{Name: name, Reason: fleetLeanSkipReason(it.Card)})
			continue
		}
		items = append(items, fleetBatchItem{
			Name:   name,
			Option: it.Card.Options[opt].Label,
			Req:    fleetOptionRequest(*it, opt, st.view.CardHashes[it.Card.ID], st.view.Source, "", false),
		})
	}
	return items, skipped
}

// batchPickable reports whether R would open its panel with a card to send,
// which is when the key help offers it.
func (st *fleetViewState) batchPickable() bool {
	if st.panel != nil || st.batch != nil || st.clarify != nil || st.shipOn() || !st.view.Fleet.Alive {
		return false
	}
	items, _ := st.leanBatch()
	return len(items) > 0
}

// openBatch is R: open the confirm panel over the cards leanBatch finds, or
// say there is nothing to send.
func (st *fleetViewState) openBatch(now time.Time) fleetCmd {
	if st.err != "" || !st.loaded || len(st.view.Ambiguous) > 0 || !st.view.Fleet.Alive || st.sending {
		return fleetCmdNone
	}
	items, skipped := st.leanBatch()
	if len(items) == 0 {
		msg := "no FLEET picks to send"
		if len(skipped) > 0 {
			msg += fmt.Sprintf(" · %d skipped", len(skipped))
		}
		st.setToast(msg, fleetToastInfo, now)
		return fleetCmdRender
	}
	st.batch = &fleetBatchPanel{items: items, skipped: skipped, openedAt: now}
	return fleetCmdRender
}

// handleBatchKey is handleKey while the R panel is open. Enter has the
// option panel's arming: nothing for fleetArmDelay after the panel opened,
// and never in a paste.
func (st *fleetViewState) handleBatchKey(k string, inBatch bool, now time.Time) fleetCmd {
	b := st.batch
	switch k {
	case KeyEsc, "q", "Q", "\x03":
		st.batch = nil
		return fleetCmdRender
	case KeyPageUp:
		b.scroll = max(b.scroll-st.pageStep(), 0)
		return fleetCmdRender
	case KeyPageDown:
		b.scroll += st.pageStep()
		return fleetCmdRender
	case KeyEnter:
		if inBatch || now.Before(b.openedAt.Add(fleetArmDelay)) {
			return fleetCmdNone
		}
		st.batchSend = b.items
		st.batch = nil
		st.sending = true
		st.setToast(fmt.Sprintf("sending %d answers…", len(b.items)), fleetToastInfo, now)
		return fleetCmdSendBatch
	}
	return fleetCmdNone
}

// fleetBatchJob sends items in order through send (fleetSendVia: a state
// re-read and the Stale check per card, pinned to the request's Source),
// sleeping fleetBatchGap between two sends. It stops after a send that says
// the rest would fail or must not go: FLEET moved, the card re-check failed,
// the result is unknown, the token was rejected, or the text was typed but
// not submitted (FLEET's box now holds it). A card-level refusal (card
// changed or gone, already answered, ...) is reported and the rest continue.
func fleetBatchJob(items []fleetBatchItem, send func(fleetSayRequest) fleetSendResult, sleep func(time.Duration)) fleetBatchResult {
	out := fleetBatchResult{Items: items}
	for i, it := range items {
		if i > 0 {
			sleep(fleetBatchGap)
		}
		r := send(it.Req)
		out.Results = append(out.Results, r)
		if fleetBatchStops(r) {
			break
		}
	}
	return out
}

func fleetBatchStops(r fleetSendResult) bool {
	return r.Moved || r.CheckErr != nil || r.Out.Unclear || r.Out.Status == 401 ||
		r.Out.Result.Code == "not_submitted"
}

// fleetBatchReason is a failed batch send's reason in the result toast.
func fleetBatchReason(r fleetSendResult) string {
	switch {
	case r.CheckErr != nil:
		return "could not re-check the card"
	case r.Stale:
		return "card changed"
	case r.Moved:
		return errFleetMoved.Error()
	}
	msg, _, _ := fleetSendToast(r.Out)
	if msg == fleetToastChanged {
		return "card changed"
	}
	return msg
}

// applyBatch takes a finished batch job: each answer that reached FLEET locks
// its card as a single send's does, an unknown result locks its card as
// unclear, and the toast says "sent N of M" with the first failure. It always
// asks for a fresh state.
func (st *fleetViewState) applyBatch(r fleetBatchResult, now time.Time) (refetch bool) {
	st.sending = false
	selID, _ := st.selectedIdentity()
	sent, failure := 0, ""
	for i, res := range r.Results {
		id := r.Items[i].Req.CardID
		ok := res.CheckErr == nil && !res.Stale && !res.Moved && !res.Out.Unclear &&
			res.Out.Status/100 == 2 && res.Out.Result.OK
		if ok {
			sent++
			if id == selID {
				st.nextAfter = id
			}
		} else if failure == "" {
			failure = r.Items[i].Name + ": " + fleetBatchReason(res)
		}
		if res.CheckErr != nil || res.Stale || res.Moved {
			continue
		}
		if res.Out.Unclear {
			st.unclear = id
			continue
		}
		// The server records an OK and a not_submitted answer before it replies.
		if res.Out.Result.OK || res.Out.Result.Code == "not_submitted" {
			if st.batchSent == nil {
				st.batchSent = map[string]bool{}
			}
			st.batchSent[id] = true
		}
	}
	msg := fmt.Sprintf("sent %d of %d", sent, len(r.Items))
	kind := fleetToastInfo
	if failure != "" || sent < len(r.Items) {
		kind = fleetToastWarn
	}
	if failure != "" {
		msg += " · " + failure
	}
	st.setToast(msg, kind, now)
	return true
}

// fleetBatchPanelLines is the R confirm panel in h rows (h < 0: all of it):
// the title, one line per card, the skipped cards, then the key line, which
// stays on the last row while the rest scrolls (PgUp/PgDn, b.scroll).
func fleetBatchPanelLines(b *fleetBatchPanel, h int, now time.Time) []string {
	n := len(b.items)
	noun := "cards"
	if n == 1 {
		noun = "card"
	}
	lines := []string{bold(fmt.Sprintf("Send FLEET's pick on %d %s?", n, noun))}
	for _, it := range b.items {
		lines = append(lines, "  "+it.Name+" → "+it.Option)
	}
	if len(b.skipped) > 0 {
		lines = append(lines, "", "Skipped:")
		for _, s := range b.skipped {
			lines = append(lines, dim("  "+s.Name+" — "+s.Reason))
		}
	}
	keys := "Enter send all"
	if arm := b.openedAt.Add(fleetArmDelay); now.Before(arm) {
		tenths := math.Ceil(float64(arm.Sub(now)) / float64(100*time.Millisecond))
		keys += fmt.Sprintf(" (ready in %.1f s)", tenths/10)
	}
	keyLine := dim(keys + " · Esc cancel")
	switch {
	case h < 0:
		return append(lines, "", keyLine)
	case h <= 0:
		return nil
	case h == 1:
		return []string{keyLine}
	}
	body, scroll := fleetPanelWindow(lines, h-2, b.scroll)
	b.scroll = scroll
	return append(body, "", keyLine)
}
