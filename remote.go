package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// RemoteResult is the per-host outcome of a /sessions poll.
type RemoteResult struct {
	Name      string    // server name from config
	Sessions  []Session // empty when Error != ""
	HostUsage HostUsage
	// Usage, KnownAccounts and ActiveSnapshotName describe this host's Anthropic
	// accounts. They do NOT come from /sessions — they are overlaid from a
	// separate GET /usage answer (applyRemoteUsage), because fetching rate limits
	// costs an Anthropic round trip and /sessions is polled far more often than
	// the numbers change. Every consumer still reads them off this struct, so the
	// split is invisible past the fetch layer.
	//
	// Usage is the live account's snapshot. Its Info is nil when the host hasn't
	// fetched yet, when the fetch failed, or when the caller asked for that
	// account to be skipped; its Account (the email) is populated regardless,
	// since reading it costs nothing.
	Usage *AccountUsage
	// CodexUsage is the host's OpenAI Codex account rate-limit snapshot; nil from
	// older servers, from a host with no Codex auth, or before its first poll.
	// This one does still ride /sessions — the Codex side is still polled.
	CodexUsage *CodexAccountUsage
	// GrokUsage is the host's xAI Grok account rate-limit snapshot; nil from
	// older servers, from a host with no Grok auth, or before its first poll.
	// Rides /sessions like CodexUsage — not GET /usage (Anthropic-only identity).
	GrokUsage *GrokAccountUsage
	// KnownAccounts lists usage for every other account this host holds a
	// claude-switch credential snapshot for (excludes whichever account is
	// currently live — that's still reported via Usage). An entry with a nil Info,
	// no Expired flag and no Reason is an account whose numbers the caller asked
	// to skip: still a real account, still a switch target, just unfetched — a
	// failed one always carries at least a Reason (see KnownAccountUsage).
	KnownAccounts []KnownAccountUsage
	// ActiveSnapshotName is the snapshot name (e.g. "avisoma") whose
	// account.json email matches this host's live Usage.Account, best-effort —
	// "" when unresolved (no snapshot matches, or the live email is unknown).
	// The host resolves it from its own files on every /usage call, so it is
	// never stale, even immediately after an account switch.
	ActiveSnapshotName string
	Error              string // "" on success, short reason otherwise
	Loading            bool   // true for a placeholder slot whose first fetch hasn't returned yet
	// Stale marks a result whose Sessions/HostUsage/Usage/CodexUsage/GrokUsage
	// are carried over from the last successful fetch because the current one
	// failed (see Error). Only ever set alongside a non-empty Error.
	// Note: mergeRemoteResult does not actually carry CodexUsage/GrokUsage —
	// those clear on failure so a frozen rate-limit bar never reads as live.
	Stale bool
}

// FetchRemote queries one server's /sessions endpoint. 5s timeout.
func FetchRemote(srv ServerConfig) RemoteResult {
	if srv.Host == "" || srv.Token == "" {
		return RemoteResult{Name: srv.Name, Error: "config missing host or token"}
	}
	url := fmt.Sprintf("http://%s:%d/sessions", srv.Host, srv.Port)
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+srv.Token)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return RemoteResult{Name: srv.Name, Error: shortErr(err)}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return RemoteResult{Name: srv.Name, Error: fmt.Sprintf("HTTP %d", resp.StatusCode)}
	}
	// The Anthropic account fields are deliberately not read here even if an
	// older server still sends them: they belong to GET /usage now, and letting
	// two sources write the same three fields would make which one wins depend on
	// poll ordering.
	var body struct {
		Sessions   []Session          `json:"sessions"`
		HostUsage  HostUsage          `json:"hostUsage"`
		CodexUsage *CodexAccountUsage `json:"codex_usage"` // nil from older servers
		GrokUsage  *GrokAccountUsage  `json:"grok_usage"`  // nil from older servers
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return RemoteResult{Name: srv.Name, Error: "bad response: " + shortErr(err)}
	}
	// Tag every session with the configured host alias so ID(), selection, and
	// remote action routing remain stable even when the server hostname differs.
	for i := range body.Sessions {
		body.Sessions[i].Host = srv.Name
	}
	return RemoteResult{
		Name:       srv.Name,
		Sessions:   body.Sessions,
		HostUsage:  body.HostUsage,
		CodexUsage: body.CodexUsage,
		GrokUsage:  body.GrokUsage,
	}
}

// FetchRemoteUsage queries one server's /usage endpoint — account identity
// only; the handler never calls Anthropic, so the response never carries
// rate-limit numbers for a remote host (see server.go). Same shape as
// FetchRemote — bearer auth — but a returned error instead of an Error field,
// since the account fields are overlaid onto a RemoteResult that may well have
// succeeded on its own. The 8s timeout (longer than FetchRemote's 5s) predates
// the handler becoming pure disk reads and is left generous rather than
// tightened.
//
// ignore names accounts (by email) the caller already holds good numbers for.
// The server no longer acts on it — see RemoteUsageHub.ignore's doc — but it
// is still sent, as a repeated parameter rather than one comma-joined value
// because an email's local-part may legally contain a comma.
//
// A 404 from a server predating this route is just another per-host failure:
// that host contributes no usage and everything else keeps working. The reverse
// mismatch needs no handling — this repo's deploy order always upgrades the
// local machine before pushing to a remote.
func FetchRemoteUsage(srv ServerConfig, ignore []string) (usageResponse, error) {
	if srv.Host == "" || srv.Token == "" {
		return usageResponse{}, fmt.Errorf("config missing host or token")
	}
	q := url.Values{}
	for _, email := range ignore {
		q.Add("ignore", email)
	}
	endpoint := fmt.Sprintf("http://%s:%d/usage", srv.Host, srv.Port)
	if encoded := q.Encode(); encoded != "" {
		endpoint += "?" + encoded
	}
	req, err := http.NewRequest("GET", endpoint, nil)
	if err != nil {
		return usageResponse{}, err
	}
	req.Header.Set("Authorization", "Bearer "+srv.Token)

	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return usageResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return usageResponse{}, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var body usageResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return usageResponse{}, fmt.Errorf("bad response: %w", err)
	}
	return body, nil
}

// applyRemoteUsage overlays one host's /usage answer onto its row. This is the
// single place the three account fields are written on the client — the TUI's
// background hub and every one-shot CLI path all funnel through it.
func applyRemoteUsage(r RemoteResult, u usageResponse) RemoteResult {
	r.Usage = u.Usage
	r.KnownAccounts = u.KnownAccounts
	r.ActiveSnapshotName = u.ActiveSnapshotName
	return r
}

// eachRemoteUsage fetches /usage from every cfg in parallel and hands each
// answer to fn, which is called from the fetching goroutine and must therefore
// only touch index i of whatever it writes into.
func eachRemoteUsage(cfgs []ServerConfig, ignore []string, fn func(i int, u usageResponse, err error)) {
	var wg sync.WaitGroup
	for i, c := range cfgs {
		i, c := i, c
		wg.Add(1)
		go func() {
			defer wg.Done()
			u, err := FetchRemoteUsage(c, ignore)
			fn(i, u, err)
		}()
	}
	wg.Wait()
}

// mergeRemoteUsage overlays each host's /usage answer onto the /sessions
// results a one-shot render is about to print. The live TUI does the same thing
// from a background hub (RemoteUsageHub); a one-shot command has nowhere to put
// a poller, so it pays for one extra parallel round of requests instead — and
// without this its remote rate-limit bars would simply be blank, since
// FetchRemote no longer carries them.
//
// A host whose /sessions fetch already failed is skipped (it is unreachable;
// a second doomed 5s wait buys nothing), and a /usage failure is silent: the
// row keeps its sessions and its error, it just has no bars. Overwriting Error
// here would put a usage-fetch failure next to a perfectly good session list.
//
// The overlay is in place: remotes is written through and returned, not copied.
// Every caller hands it a slice it already owns (FetchAllRemote's result, or
// sortRemotes' copy).
func mergeRemoteUsage(remotes []RemoteResult) []RemoteResult {
	if len(remotes) == 0 {
		return remotes
	}
	cfgs, err := LoadServerConfigs()
	if err != nil {
		return remotes
	}
	byName := make(map[string]ServerConfig, len(cfgs))
	for _, c := range cfgs {
		byName[c.Name] = c
	}
	var targets []ServerConfig
	var slots []int
	for i := range remotes {
		cfg, ok := byName[remotes[i].Name]
		if !ok || remotes[i].Error != "" {
			continue
		}
		targets = append(targets, cfg)
		slots = append(slots, i)
	}
	eachRemoteUsage(targets, nil, func(i int, u usageResponse, err error) {
		if err != nil {
			return
		}
		remotes[slots[i]] = applyRemoteUsage(remotes[slots[i]], u)
	})
	return remotes
}

// remoteUsageRow turns one /usage answer into a RemoteResult carrying nothing
// but the account fields. Unlike mergeRemoteUsage, a failure here does become
// the row's Error: with no session list on the row, a silent skip would read as
// "this host has no account snapshots" rather than "this host is unreachable".
func remoteUsageRow(name string, u usageResponse, err error) RemoteResult {
	if err != nil {
		return RemoteResult{Name: name, Error: shortErr(err)}
	}
	return applyRemoteUsage(RemoteResult{Name: name}, u)
}

// oneRemoteUsage is remoteUsageRow for a single named server (`account list
// --server`).
func oneRemoteUsage(srv ServerConfig) RemoteResult {
	u, err := FetchRemoteUsage(srv, nil)
	return remoteUsageRow(srv.Name, u, err)
}

// FetchAllRemoteUsage asks every configured server for its accounts alone, with
// no /sessions poll behind it — what `account list` needs, since the table is
// built purely from the three account fields.
func FetchAllRemoteUsage() []RemoteResult {
	cfgs, err := LoadServerConfigs()
	if err != nil || len(cfgs) == 0 {
		return nil
	}
	cfgs = dropSelfServer(cfgs)
	if len(cfgs) == 0 {
		return nil
	}
	out := make([]RemoteResult, len(cfgs))
	eachRemoteUsage(cfgs, nil, func(i int, u usageResponse, err error) {
		out[i] = remoteUsageRow(cfgs[i].Name, u, err)
	})
	return out
}

// dropSelfServer filters out a configured server entry that points back at
// this same host. A servers.yaml entry naming the local machine (needed so
// CLI --server flows can target "this host" explicitly) would otherwise
// double up local sessions with an identical remote row in the TUI, which
// already shows them via CollectLocal.
func dropSelfServer(cfgs []ServerConfig) []ServerConfig {
	local := localAddrs()
	out := cfgs[:0:0]
	for _, c := range cfgs {
		if local[c.Host] {
			continue
		}
		out = append(out, c)
	}
	return out
}

// localAddrs returns loopback plus every IP address bound to a local network
// interface, keyed for direct string lookup against a ServerConfig.Host.
func localAddrs() map[string]bool {
	addrs := map[string]bool{"127.0.0.1": true, "localhost": true, "::1": true}
	ifaceAddrs, err := net.InterfaceAddrs()
	if err != nil {
		return addrs
	}
	for _, a := range ifaceAddrs {
		ipNet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		addrs[ipNet.IP.String()] = true
	}
	return addrs
}

// FetchAllRemote polls all configured servers in parallel and returns the
// results in config order. Returns nil when no servers are configured.
func FetchAllRemote() []RemoteResult {
	cfgs, err := LoadServerConfigs()
	if err != nil || len(cfgs) == 0 {
		return nil
	}
	cfgs = dropSelfServer(cfgs)
	if len(cfgs) == 0 {
		return nil
	}
	results := make([]RemoteResult, len(cfgs))
	var wg sync.WaitGroup
	for i, c := range cfgs {
		i, c := i, c
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = FetchRemote(c)
		}()
	}
	wg.Wait()
	return results
}

// shortErr trims long error strings (URLError wrappers can be verbose).
func shortErr(err error) string {
	s := err.Error()
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 60 {
		s = s[:60]
	}
	return s
}

// LookupServer finds a configured server by name.
func LookupServer(name string) (ServerConfig, bool) {
	cfgs, _ := LoadServerConfigs()
	for _, c := range cfgs {
		if c.Name == name {
			return c, true
		}
	}
	return ServerConfig{}, false
}

// RemoteHub polls remote /sessions endpoints in a background goroutine and
// streams results into per-host slots as each reply arrives, so the TUI never
// has to wait for the slowest host. A WakeFD pipe becomes readable each time
// any slot updates, letting the render loop repaint immediately instead of
// waiting for its next tick.
type RemoteHub struct {
	mu      sync.Mutex
	results []RemoteResult
	paused  atomic.Bool
	kick    chan struct{}
	stop    chan struct{}
	wakeR   int // read end: passed to unix.Select in the TUI loop
	wakeW   int // write end: signaled after each per-host update
}

// NewRemoteHub starts the background poller and returns immediately. The
// first fetch is kicked off asynchronously so the caller can paint local
// sessions right away; each remote row populates as its host responds.
func NewRemoteHub(interval time.Duration) (*RemoteHub, error) {
	var p [2]int
	if err := unix.Pipe(p[:]); err != nil {
		return nil, fmt.Errorf("remote hub pipe: %w", err)
	}
	syscall.CloseOnExec(p[0])
	syscall.CloseOnExec(p[1])
	// Both ends non-blocking. Write: dropping a wake when the buffer is
	// full is fine — we'll signal again on the next update. Read: the TUI
	// drains in a loop until EAGAIN; a blocking read end would hang on
	// the second iteration.
	_ = unix.SetNonblock(p[0], true)
	_ = unix.SetNonblock(p[1], true)
	h := &RemoteHub{
		kick:  make(chan struct{}, 1),
		stop:  make(chan struct{}),
		wakeR: p[0],
		wakeW: p[1],
	}
	go h.run(interval)
	h.Refresh()
	return h, nil
}

// WakeFD returns a file descriptor that becomes readable each time any remote
// row has been updated. The caller drains it on read.
func (h *RemoteHub) WakeFD() int { return h.wakeR }

func (h *RemoteHub) run(interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-h.stop:
			return
		case <-t.C:
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
func (h *RemoteHub) Pause() { h.paused.Store(true) }

// Resume re-enables polling and kicks an immediate refetch so the first
// repaint after the pause shows fresh data.
func (h *RemoteHub) Resume() {
	h.paused.Store(false)
	h.Refresh()
}

// fetchAll spawns one goroutine per configured server and lets each update
// its own slot independently. Previous values are preserved by name across
// fetches so a slow host's row doesn't blink to blank between cycles.
func (h *RemoteHub) fetchAll() {
	cfgs, err := LoadServerConfigs()
	if err != nil || len(cfgs) == 0 {
		return
	}
	cfgs = dropSelfServer(cfgs)
	if len(cfgs) == 0 {
		return
	}
	h.mu.Lock()
	prev := make(map[string]RemoteResult, len(h.results))
	for _, r := range h.results {
		prev[r.Name] = r
	}
	h.results = make([]RemoteResult, len(cfgs))
	for i, c := range cfgs {
		if r, ok := prev[c.Name]; ok {
			// Prior fetch's data stays visible while the new one is in flight.
			h.results[i] = r
		} else {
			// Never fetched before — show "loading..." until the first reply.
			h.results[i] = RemoteResult{Name: c.Name, Loading: true}
		}
	}
	h.mu.Unlock()

	var wg sync.WaitGroup
	for i, c := range cfgs {
		i, c := i, c
		priorResult, hadPrior := prev[c.Name]
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := mergeRemoteResult(FetchRemote(c), priorResult, hadPrior)
			h.storeRemoteResult(i, r)
			h.signalWake()
		}()
	}
	wg.Wait()
}

// mergeRemoteResult implements the non-destructive fetch: on failure (e.g. a
// flaky connection), the session list stays on screen instead of blanking,
// marked Stale, while the error message is still surfaced. Only the session
// list carries forward — CodexUsage and GrokUsage feed header rate-limit bars
// (see dedupeCodexAccounts / dedupeGrokAccounts), which have no "stale"
// rendering of their own, so a frozen reading there would silently pass as
// live; they are left to clear as before, and RemoteUsageHub applies the same
// rule to the Anthropic side.
// hasData excludes a slot that never had a successful fetch (still
// Loading, or errored with nothing yet to carry forward).
func mergeRemoteResult(r, prior RemoteResult, hadPrior bool) RemoteResult {
	if r.Error == "" {
		return r
	}
	hasData := hadPrior && !prior.Loading && (prior.Error == "" || prior.Stale)
	if !hasData {
		return r
	}
	r.Sessions = prior.Sessions
	r.HostUsage = prior.HostUsage
	r.Stale = true
	return r
}

// Snapshot returns the most recent results. Some slots may still hold prior
// values while their host's current fetch is in flight.
func (h *RemoteHub) Snapshot() []RemoteResult {
	h.mu.Lock()
	defer h.mu.Unlock()

	results := make([]RemoteResult, len(h.results))
	copy(results, h.results)
	for i := range results {
		results[i].Sessions = append(
			[]Session(nil),
			h.results[i].Sessions...,
		)
	}
	return results
}

func (h *RemoteHub) storeRemoteResult(index int, result RemoteResult) {
	h.mu.Lock()
	h.results[index] = result
	h.mu.Unlock()
}

// Refresh requests an immediate refetch of all servers. Non-blocking;
// coalesces when a kick is already pending.
func (h *RemoteHub) Refresh() {
	select {
	case h.kick <- struct{}{}:
	default:
	}
}

func (h *RemoteHub) signalWake() {
	_, _ = unix.Write(h.wakeW, []byte{1})
}

// Shutdown stops the background goroutine and closes the wake pipe.
// Idempotent only when called once.
func (h *RemoteHub) Shutdown() {
	close(h.stop)
	_ = unix.Close(h.wakeW)
	_ = unix.Close(h.wakeR)
}
