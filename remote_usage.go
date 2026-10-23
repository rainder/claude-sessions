package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// RemoteUsageHub polls remote /usage endpoints in a background goroutine so
// the header's per-host account label and the Ctrl+W switch picker's remote
// entries never block the render loop.
//
// /usage never calls Anthropic (see server.go): it reports each host's account
// identity only, never rate-limit percentages — names, emails, and which one
// is live.
//
// The same pass also asks each host's GET /usage/numbers?cached=1: the Claude
// numbers that host already holds on disk, served without any Anthropic call.
// They are display-only fallback for a local header line that has none of its
// own (see dedupeAccounts); no credential moves.
//
// It is RemoteHub's smaller sibling, deliberately without the wake pipe: an
// account switch on a remote host wants a repaint eventually, not instantly,
// and the TUI's own tick paints the refreshed identity within a couple of
// seconds anyway. What is left is usagePoller's plain ticker + kick shape, at
// usagePoller's cadence, over RemoteHub's per-host fan-out.
type RemoteUsageHub struct {
	mu      sync.Mutex
	results map[string]usageResponse
	cached  map[string][]remoteClaudeReading
	paused  atomic.Bool
	kick    chan struct{}
	stop    chan struct{}
	// ignore yields the account emails this machine already has good numbers
	// for, recomputed per tick (see localFreshAccountEmails). Sent to the
	// server but no longer acted on there — see the doc on that field.
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
// same client process (tui.go) and both run at usageRefreshInterval. This
// predates GET /usage becoming fetch-free (see server.go) — back when a remote
// host's /usage answer meant a real Anthropic round trip on this machine's
// behalf, an unstaggered tick here would land in the same window as this
// machine's own account poll, doubling up against a shared per-token budget.
// That no longer happens — a remote /usage poll spends nothing — so the offset
// is now just a mild, harmless spread of two identity-refresh ticks. Left in
// place rather than removed: half a minute costs nothing and undoing it buys
// nothing either.
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
		h.store(nil, nil)
		return
	}
	h.fetchFrom(dropSelfServer(cfgs))
}

// fetchFrom is fetchAll with the host list already resolved. Each host's
// /usage and /usage/numbers?cached=1 run concurrently and fail independently:
// a host whose cached numbers fail (or 404 on an older server) simply has
// none this pass, and its identity still lands.
func (h *RemoteUsageHub) fetchFrom(cfgs []ServerConfig) {
	if len(cfgs) == 0 {
		h.store(nil, nil)
		return
	}
	// One ignore list for the whole pass: every host is answering about the same
	// set of accounts this machine already knows.
	var ignore []string
	if h.ignore != nil {
		ignore = h.ignore()
	}
	got := make([]*usageResponse, len(cfgs))
	readings := make([][]remoteClaudeReading, len(cfgs))
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		eachRemoteCachedClaude(cfgs, func(i int, rs []remoteClaudeReading, err error) {
			if err == nil {
				readings[i] = rs
			}
		})
	}()
	eachRemoteUsage(cfgs, ignore, func(i int, u usageResponse, err error) {
		if err != nil {
			return
		}
		got[i] = &u
	})
	wg.Wait()
	fresh := make(map[string]usageResponse, len(cfgs))
	for i, u := range got {
		if u != nil {
			fresh[cfgs[i].Name] = *u
		}
	}
	cached := make(map[string][]remoteClaudeReading, len(cfgs))
	for i, rs := range readings {
		if len(rs) > 0 {
			cached[cfgs[i].Name] = rs
		}
	}
	h.store(fresh, cached)
}

func (h *RemoteUsageHub) store(results map[string]usageResponse, cached map[string][]remoteClaudeReading) {
	h.mu.Lock()
	h.results, h.cached = results, cached
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

// CachedSnapshot returns the latest pass's cached Claude readings keyed by
// server name, as a copy of the map. The slices are never written after the
// pass that built them, so sharing them is safe.
func (h *RemoteUsageHub) CachedSnapshot() map[string][]remoteClaudeReading {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.cached) == 0 {
		return nil
	}
	out := make(map[string][]remoteClaudeReading, len(h.cached))
	for name, rs := range h.cached {
		out[name] = rs
	}
	return out
}

// remoteClaudeReading is one Claude account's numbers as a remote host holds
// them on disk. Account is used only to match a local header line, and Info
// carries only what claudeInfoFromNumbers lets through: the remote's name,
// reason and expired fields are never read.
type remoteClaudeReading struct {
	Account   string
	Info      *UsageInfo
	FetchedAt time.Time
	// Stale is the remote's own stale verdict (usageNumbersStale on its side).
	// The header also applies the age bound itself at render time.
	Stale bool
}

// remoteCachedUsageTimeout matches FetchRemoteUsage's.
const remoteCachedUsageTimeout = 8 * time.Second

// FetchRemoteCachedClaude asks one host for the Claude numbers it already
// holds (GET /usage/numbers?cached=1, bearer auth). That mode never calls
// Anthropic on the remote. A server older than the cached mode ignores the
// parameter and may run one Claude pass per interval of its own until it is
// upgraded; a server older than the route answers 404, which is just an error
// here, as is every unusable entry being dropped.
func FetchRemoteCachedClaude(srv ServerConfig) ([]remoteClaudeReading, error) {
	if srv.Host == "" || srv.Token == "" {
		return nil, fmt.Errorf("config missing host or token")
	}
	ctx, cancel := context.WithTimeout(context.Background(), remoteCachedUsageTimeout)
	defer cancel()
	resp, _, err := usageNumbersAttempt(ctx, srv, true)
	if err != nil {
		return nil, err
	}
	return remoteClaudeReadings(resp.Claude), nil
}

// remoteClaudeReadings keeps the entries with an account and usable numbers.
func remoteClaudeReadings(entries []claudeUsageNumbers) []remoteClaudeReading {
	var out []remoteClaudeReading
	for _, e := range entries {
		if strings.TrimSpace(e.Account) == "" {
			continue
		}
		info, at, ok := claudeInfoFromNumbers(e)
		if !ok {
			continue
		}
		out = append(out, remoteClaudeReading{Account: e.Account, Info: info, FetchedAt: at, Stale: e.Stale})
	}
	return out
}

// eachRemoteCachedClaude is eachRemoteUsage for FetchRemoteCachedClaude.
func eachRemoteCachedClaude(cfgs []ServerConfig, fn func(i int, rs []remoteClaudeReading, err error)) {
	var wg sync.WaitGroup
	for i, c := range cfgs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rs, err := FetchRemoteCachedClaude(c)
			fn(i, rs, err)
		}()
	}
	wg.Wait()
}

// localFreshAccountEmails lists the accounts this machine currently holds good
// numbers for: the live account and every credential snapshot whose own poll
// succeeded. Historically this was what a remote host could be told to skip
// fetching; GET /usage no longer fetches at all (see server.go), so the list
// this builds is sent as `ignore` but has nothing left to act on server-side.
// Kept rather than removed — see RemoteUsageHub.ignore's doc.
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
	if live != nil && live.Info != nil && !live.Stale {
		add(live.Account)
	}
	for _, k := range known {
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

// overlayRemoteCachedUsage copies each host's cached Claude readings onto its
// row, and clears them on a host with none this pass, so a failed host's
// numbers drop out rather than freeze. TUI only: the one-shot paths never
// fetch them.
func overlayRemoteCachedUsage(remotes []RemoteResult, cached map[string][]remoteClaudeReading) []RemoteResult {
	for i := range remotes {
		remotes[i].CachedClaude = cached[remotes[i].Name]
	}
	return remotes
}
