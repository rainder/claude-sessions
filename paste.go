package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Remote image paste. A remote Claude session's terminal can't carry an image
// on a Ctrl+V paste, so we relay it via this host's HTTP server using a push
// model:
//
//  1. A tmux root-table binding on this host intercepts Ctrl+V and runs
//     `claude-sessions clip-request '#{pane_id}' <port>`, which POSTs
//     /paste-request.
//  2. The attached laptop long-polls GET /paste-wait?session=<tname>. When a
//     request for a pane in its own tmux session arrives it reads its clipboard
//     image and POSTs the PNG to /paste?pane=<id>.
//  3. The server writes the PNG to a temp file and types the path into the pane
//     with `tmux send-keys -l`.
//
// If nobody is polling (no waiter) or the laptop reports an empty clipboard, the
// server passes the keystroke through unchanged (`send-keys C-v`) so native
// behavior is preserved. A /paste upload only lands if it was preceded by a
// matching /paste-wait claim (the stale-upload guard), so an authed POST can't
// type into an arbitrary pane on its own.

const (
	// pasteRequestTTL bounds how long a queued request stays claimable. An entry
	// older than this is dropped, never passed through late.
	pasteRequestTTL = 3 * time.Second
	// pasteWaitTimeout is the server's /paste-wait long-poll window.
	pasteWaitTimeout = 25 * time.Second
	// pasteQueueCap bounds the pending-request FIFO; overflow drops the oldest.
	pasteQueueCap = 8
	// pasteClaimTTL is how long a claim recorded by /paste-wait authorizes a
	// matching /paste upload before it's considered stale.
	pasteClaimTTL = 30 * time.Second
	// pasteMaxBytes caps an uploaded image (bytes).
	pasteMaxBytes = 20 << 20 // 20 MB
	// pasteGCMaxAge is how long a written paste file survives before GC.
	pasteGCMaxAge = 24 * time.Hour
	// pasteTmpDir is where paste images are written and GC'd. The path is typed
	// into the remote prompt, so it must be a real path the remote claude reads.
	pasteTmpDir = "/tmp"
)

// pasteAction is the decision /paste-request makes for one keystroke.
type pasteAction int

const (
	// pasteActionPassthrough: no waiter, send the raw Ctrl+V to the pane.
	pasteActionPassthrough pasteAction = iota
	// pasteActionQueued: a waiter is connected and will push the clipboard.
	pasteActionQueued
)

// pasteRequest is one pending paste from a remote tmux pane.
type pasteRequest struct {
	paneID string
	ts     time.Time
}

// pasteWaiter is one laptop-side long-poller blocked in wait(). Its buffered
// wake channel is signalled (coalescing) whenever any request is enqueued.
type pasteWaiter struct {
	wake chan struct{}
}

// sessionResolver maps a tmux pane id to its owning session name. Injected so
// the broker's session-scoped delivery is testable without tmux.
type sessionResolver func(paneID string) string

// pasteBroker holds the pending-request FIFO and coordinates waiters. Its
// decision methods (request/claimLocked/consumeClaim) are tmux-free (the one
// tmux dependency, session resolution, is the injectable resolve func) so they
// unit-test directly; the HTTP handlers below layer the remaining tmux side
// effects on top.
type pasteBroker struct {
	mu      sync.Mutex
	queue   []*pasteRequest // pending requests, oldest first
	waiters map[*pasteWaiter]struct{}
	claims  map[string]time.Time // paneID -> time a /paste-wait claimed it
	ttl     time.Duration
	now     func() time.Time // injectable clock for TTL tests; nil => time.Now
	resolve sessionResolver  // paneID -> tmux session; nil in accept-any tests
}

func newPasteBroker() *pasteBroker {
	return &pasteBroker{
		waiters: make(map[*pasteWaiter]struct{}),
		claims:  make(map[string]time.Time),
		ttl:     pasteRequestTTL,
		now:     time.Now,
		resolve: tmuxSessionForPane,
	}
}

func (b *pasteBroker) timeNow() time.Time {
	if b.now != nil {
		return b.now()
	}
	return time.Now()
}

// activeWaiters reports how many handlers are currently blocked in wait().
func (b *pasteBroker) activeWaiters() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.waiters)
}

// request records a paste request from paneID. With no waiter connected it
// returns pasteActionPassthrough and enqueues nothing. Otherwise it appends the
// request to the FIFO (dropping the oldest on overflow), wakes every waiter, and
// returns pasteActionQueued. Waking all waiters (rather than one) means the
// waiter whose tmux session matches claims it while the rest re-check and keep
// waiting — no keystroke is lost to a wake handed to a departing waiter.
func (b *pasteBroker) request(paneID string) pasteAction {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.waiters) == 0 {
		return pasteActionPassthrough
	}
	b.queue = append(b.queue, &pasteRequest{paneID: paneID, ts: b.timeNow()})
	if len(b.queue) > pasteQueueCap {
		b.queue = b.queue[1:] // drop oldest
	}
	for w := range b.waiters {
		select {
		case w.wake <- struct{}{}:
		default: // already signalled; the waiter will re-check the queue
		}
	}
	return pasteActionQueued
}

// claimLocked returns the oldest fresh request whose owning tmux session matches
// the waiter's session, removing it from the FIFO. Expired entries are dropped
// (never delivered or passed through late); non-matching fresh entries stay
// queued for another waiter. An empty session accepts any request (old client /
// no scoping). Caller holds b.mu.
func (b *pasteBroker) claimLocked(session string) *pasteRequest {
	now := b.timeNow()
	var claimed *pasteRequest
	kept := b.queue[:0]
	for _, req := range b.queue {
		if now.Sub(req.ts) > b.ttl {
			continue // expired: drop
		}
		if claimed == nil && b.sessionMatchesLocked(session, req.paneID) {
			claimed = req
			continue
		}
		kept = append(kept, req)
	}
	for i := len(kept); i < len(b.queue); i++ {
		b.queue[i] = nil // let dropped/claimed pointers be collected
	}
	b.queue = kept
	return claimed
}

// sessionMatchesLocked reports whether a request for paneID should be delivered
// to a waiter scoped to waiterSession. Empty session accepts any. Resolution
// runs under b.mu; it's a fast, rare, effectively-uncontended tmux call.
func (b *pasteBroker) sessionMatchesLocked(waiterSession, paneID string) bool {
	if waiterSession == "" {
		return true
	}
	if b.resolve == nil {
		return false
	}
	return b.resolve(paneID) == waiterSession
}

// recordClaimLocked stamps paneID as claimed now, and opportunistically expires
// stale claims so the map can't grow unbounded. Caller holds b.mu.
func (b *pasteBroker) recordClaimLocked(paneID string) {
	if b.claims == nil {
		b.claims = make(map[string]time.Time)
	}
	now := b.timeNow()
	for pane, t := range b.claims {
		if now.Sub(t) > pasteClaimTTL {
			delete(b.claims, pane)
		}
	}
	b.claims[paneID] = now
}

// consumeClaim reports whether paneID has a fresh (< pasteClaimTTL) claim from a
// preceding /paste-wait, removing it either way. A missing or stale claim yields
// false so an authed /paste can't type into a pane without a preceding Ctrl+V.
func (b *pasteBroker) consumeClaim(paneID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.claims[paneID]
	if !ok {
		return false
	}
	delete(b.claims, paneID)
	return b.timeNow().Sub(t) <= pasteClaimTTL
}

// wait blocks until a request for the given tmux session is available, the
// timeout elapses, or ctx is cancelled. It returns nil in the latter two cases.
// A matching request already queued is claimed immediately. A successful claim
// records the claim that authorizes the follow-up /paste upload.
func (b *pasteBroker) wait(ctx context.Context, timeout time.Duration, session string) *pasteRequest {
	b.mu.Lock()
	if req := b.claimLocked(session); req != nil {
		b.recordClaimLocked(req.paneID)
		b.mu.Unlock()
		return req
	}
	w := &pasteWaiter{wake: make(chan struct{}, 1)}
	b.waiters[w] = struct{}{}
	b.mu.Unlock()

	defer func() {
		b.mu.Lock()
		delete(b.waiters, w)
		b.mu.Unlock()
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case <-w.wake:
			b.mu.Lock()
			req := b.claimLocked(session)
			if req != nil {
				b.recordClaimLocked(req.paneID)
			}
			b.mu.Unlock()
			if req != nil {
				return req
			}
			// Nothing for us (claimed by another waiter, or not our session):
			// keep waiting.
		case <-timer.C:
			return nil
		case <-ctx.Done():
			return nil
		}
	}
}

// pb returns the server's paste broker, lazily creating it so both cmdServer
// and tests that construct a bare server get a working broker.
func (s *server) pb() *pasteBroker {
	s.pasteOnce.Do(func() {
		if s.paste == nil {
			s.paste = newPasteBroker()
		}
	})
	return s.paste
}

// pasteWait is GET /paste-wait?session=<tname>: the laptop-side long-poll. It
// blocks up to pasteWaitTimeout for a request in the given tmux session (or any
// session when the param is absent), replying with its pane id or 204.
func (s *server) pasteWait(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	session := r.URL.Query().Get("session")
	req := s.pb().wait(r.Context(), pasteWaitTimeout, session)
	if req == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"pane_id": req.paneID,
		"ts":      req.ts.Unix(),
	})
}

// pasteRequest is POST /paste-request: called by clip-request on this host. It
// either passes the keystroke through (no waiter) or queues it for the waiters.
func (s *server) pasteRequest(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	paneID := r.FormValue("pane_id")
	if !validPaneID(paneID) {
		http.Error(w, "bad pane_id", http.StatusBadRequest)
		return
	}
	switch s.pb().request(paneID) {
	case pasteActionPassthrough:
		_ = tmuxSendPassthrough(paneID)
		writeJSON(w, http.StatusOK, map[string]string{"action": "passthrough"})
	default:
		writeJSON(w, http.StatusOK, map[string]string{"action": "queued"})
	}
}

// pasteUpload is POST /paste?pane=<id>: the laptop pushes the clipboard image
// here (or empty=1 when it has none). It proceeds only when a matching claim
// from /paste-wait exists (else 410 Gone). A PNG body is written to a temp file
// whose path is typed into the pane; empty=1 passes the keystroke through.
func (s *server) pasteUpload(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	paneID := r.URL.Query().Get("pane")
	if !validPaneID(paneID) {
		http.Error(w, "bad pane", http.StatusBadRequest)
		return
	}
	if !s.pb().consumeClaim(paneID) {
		http.Error(w, "no pending paste for pane", http.StatusGone)
		return
	}
	if r.URL.Query().Get("empty") == "1" {
		_ = tmuxSendPassthrough(paneID)
		writeJSON(w, http.StatusOK, map[string]string{"action": "passthrough"})
		return
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, pasteMaxBytes+1))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	if len(data) == 0 {
		http.Error(w, "empty body", http.StatusBadRequest)
		return
	}
	if len(data) > pasteMaxBytes {
		http.Error(w, "image too large", http.StatusRequestEntityTooLarge)
		return
	}
	f, err := os.CreateTemp(pasteTmpDir, "claude-paste-*.png")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	path := f.Name()
	_, werr := f.Write(data)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = os.Remove(path)
		http.Error(w, werr.Error(), http.StatusInternalServerError)
		return
	}
	gcOldPastes(time.Now(), pasteGCMaxAge) // opportunistic
	if err := tmuxSendLiteral(paneID, path+" "); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"path": path})
}

// validPaneID reports whether s is a tmux pane id of the form %<digits>. The id
// reaches tmux as an exec arg (not a shell string), but we validate anyway.
func validPaneID(s string) bool {
	if len(s) < 2 || s[0] != '%' {
		return false
	}
	for i := 1; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// tmuxSessionForPane returns the tmux session name owning paneID, or "" if the
// pane is gone. Used to route a paste request only to the laptop attached to
// that pane's session.
func tmuxSessionForPane(paneID string) string {
	out, err := exec.Command("tmux", "display-message", "-p", "-t", paneID, "#{session_name}").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// tmuxSendPassthrough sends a literal Ctrl+V to the pane, preserving native
// paste behavior when no image relay is available.
func tmuxSendPassthrough(paneID string) error {
	return exec.Command("tmux", "send-keys", "-t", paneID, "C-v").Run()
}

// tmuxSendLiteral types text into the pane verbatim (no key-name interpretation).
func tmuxSendLiteral(paneID, text string) error {
	return exec.Command("tmux", "send-keys", "-t", paneID, "-l", text).Run()
}

// gcOldPastes removes paste temp files older than maxAge. Best-effort.
func gcOldPastes(now time.Time, maxAge time.Duration) {
	matches, err := filepath.Glob(filepath.Join(pasteTmpDir, "claude-paste-*.png"))
	if err != nil {
		return
	}
	for _, p := range matches {
		info, err := os.Stat(p)
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) > maxAge {
			_ = os.Remove(p)
		}
	}
}

// installPasteBinding binds Ctrl+V in tmux's root table to invoke clip-request
// with the triggering pane's id and the server port to reach. Linux-only: on
// darwin servers native Ctrl+V already works locally and rebinding it would
// break that. `-b` runs it detached so Ctrl+V never blocks the tmux command
// queue. Errors are ignored (the tmux server may not be running yet).
func installPasteBinding(port int) {
	if runtime.GOOS != "linux" {
		return
	}
	self, err := os.Executable()
	if err != nil {
		return
	}
	// tmux run-shell runs this via /bin/sh after expanding #{pane_id}. Quote the
	// binary path for the shell; leave the format specifier unquoted for tmux.
	runShell := fmt.Sprintf("%s clip-request '#{pane_id}' %d", shellQuote(self), port)
	_ = exec.Command("tmux", "bind-key", "-n", "C-v", "run-shell", "-b", runShell).Run()
}
