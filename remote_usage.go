package main

import (
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// RemoteUsageHub polls remote /usage endpoints in a background goroutine so the
// header's account bars never block the render loop.
//
// It is RemoteHub's smaller sibling, deliberately without the wake pipe: a
// session appearing or vanishing wants an immediate repaint, a rate-limit
// percentage does not, and the TUI's own tick paints the new numbers within a
// couple of seconds anyway. What is left is usagePoller's plain ticker + kick
// shape, at usagePoller's cadence, over RemoteHub's per-host fan-out.
type RemoteUsageHub struct {
	mu      sync.Mutex
	results map[string]usageResponse
	paused  atomic.Bool
	kick    chan struct{}
	stop    chan struct{}
	// ignore yields the account emails this machine already has good numbers
	// for, recomputed per tick (see localFreshAccountEmails).
	ignore func() []string
}

// NewRemoteUsageHub starts the poller and returns immediately; the first fetch
// is kicked off asynchronously, so the header simply has no remote account bars
// until it lands.
//
// ignore may be nil, in which case every host is asked for everything.
func NewRemoteUsageHub(ignore func() []string) *RemoteUsageHub {
	h := &RemoteUsageHub{
		kick:   make(chan struct{}, 1),
		stop:   make(chan struct{}),
		ignore: ignore,
	}
	go h.run()
	h.Refresh()
	return h
}

// remoteUsagePhaseOffset separates this hub's recurring ticks from
// KnownAccountsHub's. Both are started within a few lines of each other in the
// same client process (tui.go), both run at usageRefreshInterval, and a remote
// host has no ticker of its own — its /usage fetches happen only when this hub
// asks. So without an offset, this machine's own fetch of an account and every
// remote's fetch of that same account land in one tight window every two
// minutes, which is exactly the shape that turns a shared account's per-token
// budget into mutual 429s. Half a minute is enough to unstack them and far too
// small for anyone to see in the header.
const remoteUsagePhaseOffset = 30 * time.Second

// run polls on usageRefreshInterval, phase-shifted by remoteUsagePhaseOffset.
//
// The offset applies to the recurring ticker only: NewRemoteUsageHub's kick
// still fetches immediately, so the first paint is not delayed. Until the
// one-shot timer fires, tick is nil — a receive on a nil channel blocks
// forever, which is precisely "no ticker yet" — and stop and kick stay live
// throughout, so Pause/Resume/Shutdown are never held up by the offset window.
func (h *RemoteUsageHub) run() {
	offset := time.NewTimer(remoteUsagePhaseOffset)
	defer offset.Stop()
	var t *time.Ticker
	var tick <-chan time.Time
	defer func() {
		if t != nil {
			t.Stop()
		}
	}()
	for {
		select {
		case <-h.stop:
			return
		case <-offset.C:
			// The offset elapsing is not itself a fetch: it only starts the clock
			// the first real tick runs off, one full interval from here.
			t = time.NewTicker(usageRefreshInterval)
			tick = t.C
			continue
		case <-tick:
		case <-h.kick:
		}
		if h.paused.Load() {
			continue
		}
		h.fetchAll()
	}
}

// Pause makes the poller ignore ticks and kicks — used while an external
// program (tmux attach, ssh) owns the terminal and nothing renders.
func (h *RemoteUsageHub) Pause() { h.paused.Store(true) }

// Resume re-enables polling and kicks an immediate refetch so the first repaint
// after the pause shows fresh numbers.
func (h *RemoteUsageHub) Resume() {
	h.paused.Store(false)
	h.Refresh()
}

// Refresh requests an immediate refetch of all servers. Non-blocking;
// coalesces when a kick is already pending.
func (h *RemoteUsageHub) Refresh() {
	select {
	case h.kick <- struct{}{}:
	default:
	}
}

// Shutdown stops the background goroutine.
func (h *RemoteUsageHub) Shutdown() { close(h.stop) }

// fetchAll asks every configured host in parallel and replaces the whole result
// set at the end of the pass.
//
// Whole-set replacement, rather than RemoteHub's per-slot streaming, is what
// makes a failed host's numbers disappear instead of freezing: usage bars have
// no "stale" rendering, so a carried-forward percentage would silently read as
// live (the same reasoning mergeRemoteResult documents for the session list's
// Usage fields). A host dropped from servers.yaml falls out for free. And
// nothing flickers in the meantime, because the previous set stays visible
// until the new one is complete.
func (h *RemoteUsageHub) fetchAll() {
	cfgs, err := LoadServerConfigs()
	if err != nil {
		h.store(nil)
		return
	}
	h.fetchFrom(dropSelfServer(cfgs))
}

// fetchFrom is fetchAll with the host list already resolved.
func (h *RemoteUsageHub) fetchFrom(cfgs []ServerConfig) {
	if len(cfgs) == 0 {
		h.store(nil)
		return
	}
	// One ignore list for the whole pass: every host is answering about the same
	// set of accounts this machine already knows.
	var ignore []string
	if h.ignore != nil {
		ignore = h.ignore()
	}
	got := make([]*usageResponse, len(cfgs))
	eachRemoteUsage(cfgs, ignore, func(i int, u usageResponse, err error) {
		if err != nil {
			return
		}
		got[i] = &u
	})
	fresh := make(map[string]usageResponse, len(cfgs))
	for i, u := range got {
		if u != nil {
			fresh[cfgs[i].Name] = *u
		}
	}
	h.store(fresh)
}

func (h *RemoteUsageHub) store(results map[string]usageResponse) {
	h.mu.Lock()
	h.results = results
	h.mu.Unlock()
}

// Snapshot returns the latest pass's results keyed by server name, as a copy —
// the caller reads it on the render path while the poller may be replacing it.
func (h *RemoteUsageHub) Snapshot() map[string]usageResponse {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.results) == 0 {
		return nil
	}
	out := make(map[string]usageResponse, len(h.results))
	for name, u := range h.results {
		out[name] = u
	}
	return out
}

// localFreshAccountEmails lists the accounts this machine currently holds good
// numbers for: the live account and every credential snapshot whose own poll
// succeeded. Those are what a remote host can be told to skip — if two machines
// are logged into the same account, only one of them needs to ask Anthropic
// about it, and the client's own dedupeAccounts would throw the second answer
// away regardless (a live local line always beats a remote's snapshot copy).
//
// An account whose local fetch FAILED is deliberately absent. Including it would
// tell every remote host not to fetch the very account this machine has no
// numbers for, turning one transient 429 here into a blank (or "auth expired")
// bar everywhere — the opposite of what fewer pollers is meant to achieve. Same
// for an account with no email: an empty ignore entry names nothing.
func localFreshAccountEmails(live *AccountUsage, known []KnownAccountUsage) []string {
	seen := make(map[string]bool)
	var out []string
	add := func(email string) {
		key := strings.ToLower(strings.TrimSpace(email))
		if key == "" || seen[key] {
			return
		}
		seen[key] = true
		out = append(out, key)
	}
	if live != nil && live.Info != nil {
		add(live.Account)
	}
	for _, k := range known {
		// Stale counts as "cannot vouch for it", exactly like expired: carried-
		// forward numbers are this machine's memory of an account it currently
		// cannot reach, and telling every remote to skip it would spread one
		// local blip everywhere instead of letting a healthy host answer.
		if k.Expired || k.Stale || k.Info == nil {
			continue
		}
		add(k.Account)
	}
	// Stable order so two consecutive ticks produce the same request URL.
	sort.Strings(out)
	return out
}

// overlayRemoteUsage copies each host's latest /usage answer onto its row. A
// host with no entry yet (first pass still in flight, or its fetch failed) keeps
// its zero-valued account fields, which every consumer already reads as "no
// data for this host".
func overlayRemoteUsage(remotes []RemoteResult, usage map[string]usageResponse) []RemoteResult {
	if len(usage) == 0 {
		return remotes
	}
	for i := range remotes {
		if u, ok := usage[remotes[i].Name]; ok {
			remotes[i] = applyRemoteUsage(remotes[i], u)
		}
	}
	return remotes
}
