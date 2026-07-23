package main

import (
	"encoding/json"
	"fmt"
	"net/http"
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
	// Usage is the host's Anthropic account rate-limit snapshot; nil from older
	// servers that don't report it, or before that host's first poll lands.
	Usage *AccountUsage
	// CodexUsage is the host's OpenAI Codex account rate-limit snapshot; nil from
	// older servers, from a host with no Codex auth, or before its first poll.
	CodexUsage *CodexAccountUsage
	Error      string // "" on success, short reason otherwise
	Loading    bool   // true for a placeholder slot whose first fetch hasn't returned yet
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
	var body struct {
		Sessions   []Session          `json:"sessions"`
		HostUsage  HostUsage          `json:"hostUsage"`
		Usage      *AccountUsage      `json:"usage"`       // nil from older servers
		CodexUsage *CodexAccountUsage `json:"codex_usage"` // nil from older servers
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return RemoteResult{Name: srv.Name, Error: "bad response: " + shortErr(err)}
	}
	// Tag every session with the configured host alias so ID(), selection, and
	// remote action routing remain stable even when the server hostname differs.
	for i := range body.Sessions {
		body.Sessions[i].Host = srv.Name
	}
	return RemoteResult{Name: srv.Name, Sessions: body.Sessions, HostUsage: body.HostUsage, Usage: body.Usage, CodexUsage: body.CodexUsage}
}

// FetchAllRemote polls all configured servers in parallel and returns the
// results in config order. Returns nil when no servers are configured.
func FetchAllRemote() []RemoteResult {
	cfgs, err := LoadServerConfigs()
	if err != nil || len(cfgs) == 0 {
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

type disabledOverrideKey struct {
	host      string
	sessionID string
}

type pendingDisabledOverride struct {
	disabled       bool
	protectThrough uint64
}

// RemoteHub polls remote /sessions endpoints in a background goroutine and
// streams results into per-host slots as each reply arrives, so the TUI never
// has to wait for the slowest host. A WakeFD pipe becomes readable each time
// any slot updates, letting the render loop repaint immediately instead of
// waiting for its next tick.
type RemoteHub struct {
	mu              sync.Mutex
	results         []RemoteResult
	fetchGeneration uint64
	pendingDisabled map[disabledOverrideKey]pendingDisabledOverride
	paused          atomic.Bool
	kick            chan struct{}
	stop            chan struct{}
	wakeR           int // read end: passed to unix.Select in the TUI loop
	wakeW           int // write end: signaled after each per-host update
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
		pendingDisabled: make(map[disabledOverrideKey]pendingDisabledOverride),
		kick:            make(chan struct{}, 1),
		stop:            make(chan struct{}),
		wakeR:           p[0],
		wakeW:           p[1],
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
	h.mu.Lock()
	h.fetchGeneration++
	generation := h.fetchGeneration

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
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := FetchRemote(c)
			h.storeRemoteResult(i, generation, r)
			h.signalWake()
		}()
	}
	wg.Wait()
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

func (h *RemoteHub) PatchDisabled(host, sessionID string, disabled bool) {
	if sessionID == "" {
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	if h.pendingDisabled == nil {
		h.pendingDisabled = make(map[disabledOverrideKey]pendingDisabledOverride)
	}

	key := disabledOverrideKey{host: host, sessionID: sessionID}
	h.pendingDisabled[key] = pendingDisabledOverride{
		disabled:       disabled,
		protectThrough: h.fetchGeneration,
	}

	for i := range h.results {
		if h.results[i].Name == host {
			patchDisabledBySessionID(
				h.results[i].Sessions,
				sessionID,
				disabled,
			)
		}
	}
}

func (h *RemoteHub) applyPendingDisabledLocked(
	generation uint64,
	result *RemoteResult,
) {
	if result.Error != "" {
		return
	}

	for key, pending := range h.pendingDisabled {
		if key.host != result.Name {
			continue
		}

		if generation > pending.protectThrough {
			delete(h.pendingDisabled, key)
			continue
		}

		patchDisabledBySessionID(
			result.Sessions,
			key.sessionID,
			pending.disabled,
		)
	}
}

func (h *RemoteHub) storeRemoteResult(
	index int,
	generation uint64,
	result RemoteResult,
) {
	h.mu.Lock()
	h.applyPendingDisabledLocked(generation, &result)
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
