package main

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"
)

// The outbox holds plain page messages to FLEET (POST /fleet/say with no
// cardId) that could not be typed at once: the input box held text, a
// dialog was open, a grok turn was busy, the pane was in copy mode, the 1 s
// gap had not run, or earlier messages were still held (FIFO). A worker
// (runFleetOutbox) types the head item when the box is clear. Card answers
// never come here: their guards must run at the moment of typing, and the
// answer store has no queued state.
//
// In memory only: the text is never written to disk, so a restart drops
// every queued item. The page then sees the id missing from /fleet/state's
// outbox and marks the bubble unconfirmed.
const (
	fleetOutboxMax      = 20               // queued items; more → outbox_full
	fleetOutboxTTL      = 60 * time.Minute // a queued item older than this is dropped (expired)
	fleetOutboxKeepDone = 10 * time.Minute // finished items stay visible this long
	fleetOutboxEvery    = 2 * time.Second  // worker tick
	fleetOutboxLogEvery = 10 * time.Minute // one say-log entry per refusal code
)

const (
	fleetOutboxQueued      = "queued"
	fleetOutboxSent        = "sent"
	fleetOutboxUnconfirmed = "unconfirmed"
	fleetOutboxDropped     = "dropped"
)

// fleetOutboxHoldCode reports a fleetTypeInto refusal that holds a plain
// page message instead of refusing it: the session is there, only its box
// is not ready.
func fleetOutboxHoldCode(code string) bool {
	switch code {
	case "input_busy", "dialog", "busy", "pane_mode":
		return true
	}
	return false
}

type fleetOutboxItem struct {
	id       string
	text     string
	queuedAt time.Time
	attempts int
	lastCode string
	status   string
	doneAt   time.Time
	// inflight: the worker is typing it now (head → retry/finish/release).
	// pruneLocked never expires it then: an item past the TTL mid-send
	// would read dropped while it may be landing in the box.
	inflight bool
	// revoked: a login revoke ran while the item was in flight. If that
	// attempt does not deliver it, it is dropped, never queued again.
	revoked   bool
	revokedAt time.Time
}

// fleetOutboxView is one item in /fleet/state's outbox. No text: the page
// keeps its own copy.
type fleetOutboxView struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	Code     string `json:"code,omitempty"`
	QueuedAt string `json:"queuedAt"`
	DoneAt   string `json:"doneAt,omitempty"`
	Attempts int    `json:"attempts"`
}

// fleetOutbox: mu guards everything below it. Lock order: sayMu, then mu;
// /fleet/state takes mu only.
type fleetOutbox struct {
	mu    sync.Mutex
	items []*fleetOutboxItem // oldest first
	wake  chan struct{}
	logAt map[string]time.Time // refusal code → last say-log entry (worker only)
}

func (o *fleetOutbox) wakeChan() chan struct{} {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.wake == nil {
		o.wake = make(chan struct{}, 1)
	}
	return o.wake
}

// pruneLocked drops a queued item past fleetOutboxTTL (dropped, expired) and
// forgets a finished one fleetOutboxKeepDone after it finished.
func (o *fleetOutbox) pruneLocked(now time.Time) {
	keep := o.items[:0]
	for _, it := range o.items {
		if it.status == fleetOutboxQueued && !it.inflight && now.Sub(it.queuedAt) > fleetOutboxTTL {
			it.status, it.lastCode, it.doneAt, it.text = fleetOutboxDropped, "expired", now, ""
		}
		if it.status != fleetOutboxQueued && now.Sub(it.doneAt) > fleetOutboxKeepDone {
			continue
		}
		keep = append(keep, it)
	}
	for i := len(keep); i < len(o.items); i++ {
		o.items[i] = nil
	}
	o.items = keep
}

func (o *fleetOutbox) queuedCount(now time.Time) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.pruneLocked(now)
	n := 0
	for _, it := range o.items {
		if it.status == fleetOutboxQueued {
			n++
		}
	}
	return n
}

// hasQueued reports a queued item with this exact text.
func (o *fleetOutbox) hasQueued(text string, now time.Time) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.pruneLocked(now)
	for _, it := range o.items {
		if it.status == fleetOutboxQueued && it.text == text {
			return true
		}
	}
	return false
}

// enqueue adds text behind the queued items. "" when the outbox is full.
func (o *fleetOutbox) enqueue(text, code string, tried bool, now time.Time) string {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.pruneLocked(now)
	n := 0
	for _, it := range o.items {
		if it.status == fleetOutboxQueued {
			n++
		}
	}
	if n >= fleetOutboxMax {
		return ""
	}
	it := &fleetOutboxItem{id: newFleetOutboxID(), text: text, queuedAt: now, lastCode: code, status: fleetOutboxQueued}
	if tried {
		it.attempts = 1
	}
	o.items = append(o.items, it)
	if o.wake != nil {
		select {
		case o.wake <- struct{}{}:
		default:
		}
	}
	return it.id
}

// head returns the oldest queued item's id and text, after pruning, and
// marks it in flight until retry, finish or release.
func (o *fleetOutbox) head(now time.Time) (id, text string, ok bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.pruneLocked(now)
	for _, it := range o.items {
		if it.status == fleetOutboxQueued {
			it.inflight = true
			return it.id, it.text, true
		}
	}
	return "", "", false
}

// release clears an item's in-flight mark (a tick that panicked).
func (o *fleetOutbox) release(id string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, it := range o.items {
		if it.id == id {
			it.inflight = false
			o.dropIfRevokedLocked(it)
		}
	}
}

// retry records a failed attempt that keeps the item queued.
func (o *fleetOutbox) retry(id, code string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, it := range o.items {
		if it.id == id && it.status == fleetOutboxQueued {
			it.attempts++
			it.lastCode = code
			it.inflight = false
			o.dropIfRevokedLocked(it)
		}
	}
}

// finish ends an item: sent or unconfirmed. The text is dropped from memory.
func (o *fleetOutbox) finish(id, status, code string, at time.Time) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, it := range o.items {
		if it.id == id && it.status == fleetOutboxQueued {
			it.attempts++
			it.status, it.lastCode, it.doneAt, it.text = status, code, at, ""
			it.inflight = false
		}
	}
}

// dropQueued drops every queued item that is not in flight, with code. A
// login revoke uses it: a message queued by a device that was just logged
// out must not be typed later. An in-flight item is flagged instead: if the
// attempt in progress does not deliver it, retry or release drops it.
func (o *fleetOutbox) dropQueued(code string, at time.Time) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, it := range o.items {
		if it.status != fleetOutboxQueued {
			continue
		}
		if it.inflight {
			it.revoked, it.revokedAt = true, at
			continue
		}
		it.status, it.lastCode, it.doneAt, it.text = fleetOutboxDropped, code, at, ""
	}
}

// dropIfRevokedLocked drops a queued item that a revoke flagged while it was
// in flight. Caller holds o.mu.
func (o *fleetOutbox) dropIfRevokedLocked(it *fleetOutboxItem) {
	if it.revoked && it.status == fleetOutboxQueued {
		it.status, it.lastCode, it.doneAt, it.text = fleetOutboxDropped, "revoked", it.revokedAt, ""
	}
}

// view is /fleet/state's outbox: every item still kept, oldest first, never
// nil.
func (o *fleetOutbox) view(now time.Time) []fleetOutboxView {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.pruneLocked(now)
	out := make([]fleetOutboxView, 0, len(o.items))
	for _, it := range o.items {
		v := fleetOutboxView{ID: it.id, Status: it.status, Code: it.lastCode,
			QueuedAt: it.queuedAt.UTC().Format(time.RFC3339), Attempts: it.attempts}
		if !it.doneAt.IsZero() {
			v.DoneAt = it.doneAt.UTC().Format(time.RFC3339)
		}
		out = append(out, v)
	}
	return out
}

// logGate lets one say-log entry per refusal code through per
// fleetOutboxLogEvery, so a box that stays busy for an hour does not write
// one entry per tick.
func (o *fleetOutbox) logGate(code string, now time.Time) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if at, ok := o.logAt[code]; ok && now.Sub(at) < fleetOutboxLogEvery {
		return false
	}
	if o.logAt == nil {
		o.logAt = map[string]time.Time{}
	}
	o.logAt[code] = now
	return true
}

func newFleetOutboxID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// fleetHold answers a plain page message that goes to the outbox: 200
// {ok, held, outboxId}, or outbox_full. code is what held it ("queued" when
// it only waits behind earlier items); tried says a typing attempt already
// ran. Caller holds sayMu.
func (s *server) fleetHold(w http.ResponseWriter, text, code string, tried bool, now time.Time) {
	if code == "queued" {
		code = ""
	}
	id := s.fleet.outbox.enqueue(text, code, tried, now)
	if id == "" {
		writeJSON(w, http.StatusOK, fleetSayResult{Code: "outbox_full", Error: "Too many queued messages. Wait until FLEET reads them."})
		return
	}
	writeJSON(w, http.StatusOK, fleetSayResult{OK: true, Held: true, OutboxID: id})
}

// fleetOutboxTick tries the head item once, under sayMu: the 1 s gap (not
// the double tap: the item passed that when it was queued), a fresh FLEET
// (fleetSendTarget), then fleetTypeInto with refusal logging gated. OK →
// sent, plus the web-say log line; not_submitted or send_failed →
// unconfirmed, plus the note line (the text may sit in the box, and typing
// it again would duplicate it); anything else keeps the item queued.
func (s *server) fleetOutboxTick() {
	f := &s.fleet
	f.sayMu.Lock()
	defer f.sayMu.Unlock()
	ob := &f.outbox
	now := s.fleetNow()
	id, text, ok := ob.head(now)
	if !ok {
		return
	}
	defer ob.release(id)
	if !f.lastSayAt.IsZero() && now.Sub(f.lastSayAt) < fleetSayGap {
		ob.retry(id, "rate_limited")
		return
	}
	row, code, _ := s.fleetSendTarget()
	if row == nil {
		if code == "" {
			code = "list_failed"
		}
		ob.retry(id, code)
		return
	}
	// An automatic sender: Enter stays out of a busy grok turn, as for
	// idle events.
	who := fleetSayFLEET
	who.refuseBusyGrok = true
	who.logGate = func(code string) bool { return ob.logGate(code, s.fleetNow()) }
	res := s.fleetTypeInto(*row, text, who)
	switch {
	case res.OK:
		done := s.fleetSayDoneLocked(text)
		ob.finish(id, fleetOutboxSent, "", done)
		if err := s.appendFleetLog(fleetLogEntry{At: done.UTC().Format(time.RFC3339), Type: "web-say", Text: text}); err != nil {
			fmt.Fprintf(os.Stderr, "fleet: web-say log append failed: %v\n", err)
		}
	case res.Code == "not_submitted" || res.Code == "send_failed":
		at := s.fleetNow()
		ob.finish(id, fleetOutboxUnconfirmed, res.Code, at)
		_ = s.appendFleetLog(fleetLogEntry{At: at.UTC().Format(time.RFC3339), Type: "note",
			Text: "web-say typed but not submitted"})
	default:
		ob.retry(id, res.Code)
	}
}

// runFleetOutbox ticks forever, every fleetOutboxEvery and at once when an
// item is queued; started by cmdServer. A panic in one tick is logged and
// the loop goes on.
func (s *server) runFleetOutbox() {
	wake := s.fleet.outbox.wakeChan()
	for {
		func() {
			defer func() {
				if r := recover(); r != nil {
					fmt.Fprintf(os.Stderr, "fleet outbox: tick failed: %v\n", r)
				}
			}()
			s.fleetOutboxTick()
		}()
		select {
		case <-wake:
		case <-time.After(fleetOutboxEvery):
		}
	}
}
