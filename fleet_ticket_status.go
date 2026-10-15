package main

import (
	"context"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// The ClickUp status of each ticket a working or ended row names, for the
// FLEET page and for the idle "finished" entry. cu has no JSON status, so the
// server runs `cu fetch <ticket>` (plain text, about 1.3 s) and reads its
// "Status:" line. The ClickUp events loop calls refreshTicketStatuses after
// each tick, and only while clickup-events is enabled, so the status needs
// the same cu setup. At most fleetTicketFetchMax fetches per call; a failure
// keeps the old status and still counts as a try, so a broken cu is asked at
// most once per fleetTicketRetry per ticket.

const (
	fleetTicketFetchMax = 4
	fleetTicketRetry    = 15 * time.Minute
	// fleetTicketStatusMaxAge: an older status is not served, so a failing
	// cu or a disabled ClickUp loop does not show old statuses for ever.
	fleetTicketStatusMaxAge = 2 * time.Hour
)

// ticketStatusRe is the shape of a status the cache keeps: lower case, with
// digits, spaces, "/", "_" and "-" for custom statuses.
var ticketStatusRe = regexp.MustCompile(`^[a-z0-9][a-z0-9 /_-]{0,31}$`)

var ticketStatusLineRe = regexp.MustCompile(`^Status:\s*(.+)$`)

// parseTicketStatus reads the "Status:" line of `cu fetch` output: trimmed,
// lower case, and only when it fits ticketStatusRe. Only the header block
// counts — the "<ID>  <title>" line and the "Key: value" lines up to the
// first blank line. The description comes later and is text strangers write,
// so a "Status: done" line in it must never be read as the status.
func parseTicketStatus(out string) (string, error) {
	for _, ln := range strings.Split(out, "\n") {
		ln = strings.TrimRight(ln, "\r")
		if strings.TrimSpace(ln) == "" {
			break // end of the header block
		}
		m := ticketStatusLineRe.FindStringSubmatch(ln)
		if m == nil {
			continue
		}
		st := strings.ToLower(strings.TrimSpace(m[1]))
		if !ticketStatusRe.MatchString(st) {
			return "", errClickupBadOutput
		}
		return st, nil
	}
	return "", errClickupBadOutput
}

// fleetTicketFinished is the set of statuses for which an idle session's work
// is done: the idle line names it "finished".
var fleetTicketFinished = map[string]bool{"testing": true, "done": true, "closed": true, "complete": true}

type fleetTicketEntry struct {
	status    string
	fetchedAt time.Time // last success; zero when none
	triedAt   time.Time // last attempt, success or not
}

// fleetTicketCache is the in-memory status per ticket. Readers take mu for
// a lookup only; the refresher never holds it across a fetch.
type fleetTicketCache struct {
	mu sync.Mutex
	m  map[string]fleetTicketEntry
}

// freshLocked is the cached status of ticket, or "" when there is none or it
// was fetched more than fleetTicketStatusMaxAge before now.
func (c *fleetTicketCache) freshLocked(ticket string, now time.Time) string {
	e := c.m[ticket]
	if e.status == "" || now.Sub(e.fetchedAt) > fleetTicketStatusMaxAge {
		return ""
	}
	return e.status
}

// get is the fresh cached status of ticket, or "".
func (c *fleetTicketCache) get(ticket string, now time.Time) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.freshLocked(ticket, now)
}

// statuses maps each ticket to its fresh cached status, else to its status
// in fallback. nil when none is known.
func (c *fleetTicketCache) statuses(tickets []string, fallback map[string]string, now time.Time) map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out map[string]string
	for _, t := range tickets {
		st := c.freshLocked(t, now)
		if st == "" {
			st = fallback[t]
		}
		if st == "" {
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		out[t] = st
	}
	return out
}

// fleetTicketWant is one ticket a row names, with the latest time a row
// naming it went quiet (not busy), so a session that just stopped gets its
// ticket checked on the next call.
type fleetTicketWant struct {
	ticket string
	quiet  time.Time
}

// dueTickets picks at most max tickets to fetch now: never tried first, then
// the oldest try. A ticket is due when it was never tried, its last try is
// older than fleetTicketRetry, or a row naming it went quiet after that try.
// The last try, not the last success, so a failing cu is not asked again on
// every call.
func (c *fleetTicketCache) dueTickets(wants []fleetTicketWant, now time.Time, max int) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	type cand struct {
		ticket  string
		triedAt time.Time
	}
	var due []cand
	for _, w := range wants {
		e, ok := c.m[w.ticket]
		if !ok || e.triedAt.IsZero() || now.Sub(e.triedAt) >= fleetTicketRetry || w.quiet.After(e.triedAt) {
			due = append(due, cand{w.ticket, e.triedAt})
		}
	}
	sort.SliceStable(due, func(i, j int) bool {
		a, b := due[i].triedAt, due[j].triedAt
		if a.IsZero() != b.IsZero() {
			return a.IsZero()
		}
		return a.Before(b)
	})
	out := make([]string, 0, min(len(due), max))
	for i := 0; i < len(due) && i < max; i++ {
		out = append(out, due[i].ticket)
	}
	return out
}

// record stores one fetch result. A failure keeps the old status and only
// moves triedAt.
func (c *fleetTicketCache) record(ticket, status string, ok bool, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]fleetTicketEntry{}
	}
	e := c.m[ticket]
	e.triedAt = now
	if ok {
		e.status, e.fetchedAt = status, now
	}
	c.m[ticket] = e
}

// keepOnly drops every entry no row names any more.
func (c *fleetTicketCache) keepOnly(want map[string]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for t := range c.m {
		if !want[t] {
			delete(c.m, t)
		}
	}
}

// fetchTicketStatusOutput runs `cu fetch <ticket>` through the backlog's cu
// (the fetchStatus seam in tests) and returns its stdout.
func (s *server) fetchTicketStatusOutput(ctx context.Context, cfg clickupConfig, ticket string) (string, error) {
	if f := s.clickup.fetchStatus; f != nil {
		return f(ctx, ticket)
	}
	path := resolveClickupCu(cfg)
	if path == "" {
		return "", errClickupNoCu
	}
	out, err := runClickupCu(ctx, path, []string{"fetch", ticket})
	return string(out), err
}

// refreshTicketStatuses is one status pass, run by the ClickUp events loop
// after its tick while clickup-events is enabled, never holding the loop's
// lock across a fetch. It reads the cached session list, lets the ended
// tracker see it, and fetches up to fleetTicketFetchMax due tickets from the
// live and ended rows. Each fetch has its own clickupCuTimeout. A ticket that
// does not fit fleetTicketOK is never passed to cu. Entries no row names any
// more are dropped.
func (s *server) refreshTicketStatuses(ctx context.Context, cfg clickupConfig, now time.Time) {
	sessions, err := s.cachedSessions()
	if err != nil {
		return
	}
	if sessions == nil {
		sessions = []Session{}
	}
	rows := s.fleetObserveWork(sessions, now)
	ended := s.fleetEndedRows(now)
	want := map[string]bool{}
	quiet := map[string]time.Time{}
	var order []string
	add := func(t string, q time.Time) {
		if !fleetTicketOK.MatchString(t) {
			return
		}
		if !want[t] {
			want[t] = true
			order = append(order, t)
		}
		if q.After(quiet[t]) {
			quiet[t] = q
		}
	}
	for _, r := range rows {
		for _, t := range r.Tickets {
			add(t, r.since)
		}
	}
	for _, e := range ended {
		for _, t := range e.Tickets {
			add(t, time.Time{})
		}
	}
	tc := &s.fleet.tickets
	tc.keepOnly(want)
	wants := make([]fleetTicketWant, 0, len(order))
	for _, t := range order {
		wants = append(wants, fleetTicketWant{ticket: t, quiet: quiet[t]})
	}
	for _, t := range tc.dueTickets(wants, now, fleetTicketFetchMax) {
		if ctx.Err() != nil {
			return
		}
		fctx, cancel := context.WithTimeout(ctx, clickupCuTimeout)
		out, err := s.fetchTicketStatusOutput(fctx, cfg, t)
		cancel()
		st := ""
		if err == nil {
			st, err = parseTicketStatus(out)
		}
		tc.record(t, st, err == nil, now)
		if err != nil {
			d := &s.clickup
			code := clickupFailCode(err)
			d.mu.Lock()
			if d.logGateLocked("status:" + code) {
				d.clickupLog("status fetch failed: %s", code)
			}
			d.mu.Unlock()
		}
	}
}
