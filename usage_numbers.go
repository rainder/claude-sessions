package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"
)

// usage_numbers.go is `claude-sessions usage` and GET /usage/numbers: account
// quota numbers for Claude, Grok and Codex in one stable, machine-readable
// shape, for callers (an orchestrator session) that cannot read the TUI
// header. See CLAUDE.md's "`usage` / `GET /usage/numbers`" subsection.
//
// GET /usage stays identity-only. This endpoint is the one place a server may
// spend Anthropic budget, and only under the narrow rule usageNumbersSource
// documents.

// usageNumbersStaleAfter is the one stale bound: a reading older than two poll
// intervals means at least one poll that should have refreshed it did not.
const usageNumbersStaleAfter = 2 * usageRefreshInterval

// usageNumbersServiceTimeout bounds the CLI's request to the local service. A
// service doing a real pass waits on up to ~7s per Claude account (usage plus
// profile probe), one account after another, so this is generous rather than
// snappy. A var so tests can shorten it.
var usageNumbersServiceTimeout = 45 * time.Second

const (
	usageNumbersSourceService = "service"
	usageNumbersSourceLocal   = "local"
)

// usageNumbersResponse is the wire shape of GET /usage/numbers and of
// `usage --json`. Its json tags are the contract; the internal UsageInfo /
// GrokAccountUsage / CodexAccountUsage types are mapped into it rather than
// tagged, because their field names are what the disk caches hold.
type usageNumbersResponse struct {
	GeneratedAt time.Time              `json:"generatedAt"`
	Source      string                 `json:"source"` // "service" | "local"
	Claude      []claudeUsageNumbers   `json:"claude"`
	Grok        []providerUsageNumbers `json:"grok"`
	Codex       []providerUsageNumbers `json:"codex"`
}

type usageWindowNumbers struct {
	Label    string     `json:"label,omitempty"`
	Pct      float64    `json:"pct"`
	ResetsAt *time.Time `json:"resetsAt,omitempty"`
}

// usageCreditsNumbers is extra-usage credits in MAJOR currency units (dollars,
// not cents) — creditsInfo's minor units divided by 10^DecimalPlaces.
type usageCreditsNumbers struct {
	Enabled  bool    `json:"enabled"`
	Used     float64 `json:"used"`
	Limit    float64 `json:"limit"`
	Currency string  `json:"currency,omitempty"`
	Pct      float64 `json:"pct"`
}

type claudeUsageNumbers struct {
	Name         string               `json:"name,omitempty"` // claude-switch snapshot name
	Account      string               `json:"account"`
	Active       bool                 `json:"active"`
	FiveHour     *usageWindowNumbers  `json:"fiveHour,omitempty"`
	SevenDay     *usageWindowNumbers  `json:"sevenDay,omitempty"`
	WeeklyScoped *usageWindowNumbers  `json:"weeklyScoped,omitempty"`
	Credits      *usageCreditsNumbers `json:"credits,omitempty"`
	FetchedAt    *time.Time           `json:"fetchedAt,omitempty"`
	Stale        bool                 `json:"stale"`
	Expired      bool                 `json:"expired,omitempty"`
	Reason       string               `json:"reason,omitempty"`
}

type providerUsageNumbers struct {
	Account   string               `json:"account"`
	Windows   []usageWindowNumbers `json:"windows"`
	Credits   *usageCreditsNumbers `json:"credits,omitempty"`
	Plan      string               `json:"plan,omitempty"`
	FetchedAt *time.Time           `json:"fetchedAt,omitempty"`
	Stale     bool                 `json:"stale"`
}

// Upstream seams. Production values are the same fetches the hubs use; tests
// replace them (TestMain makes the Grok and Codex ones panic, and the live
// Claude one already ends in usageInfoFetch, which panics there too).
var (
	usageNumbersLiveFetch  = fetchUsage
	usageNumbersGrokFetch  = fetchGrokUsage
	usageNumbersCodexFetch = fetchCodexUsage
)

// usageNumbersStale is the one stale rule: no timestamp, older than
// usageNumbersStaleAfter, or marked stale by the fetcher in this pass (a
// carried reading). loadAccountCache's own Stale flag is deliberately not an
// input — it is set on every disk read.
func usageNumbersStale(now, fetchedAt time.Time, marked bool) bool {
	return marked || fetchedAt.IsZero() || now.Sub(fetchedAt) > usageNumbersStaleAfter
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func usageWindowFrom(label string, b usageBucket) *usageWindowNumbers {
	return &usageWindowNumbers{Label: label, Pct: b.Pct, ResetsAt: timePtr(b.ResetsAt)}
}

// usageCreditsFrom maps creditsInfo to major units; nil unless enabled.
func usageCreditsFrom(c creditsInfo) *usageCreditsNumbers {
	if !c.Enabled {
		return nil
	}
	scale := math.Pow(10, float64(c.DecimalPlaces))
	return &usageCreditsNumbers{
		Enabled:  true,
		Used:     c.Used / scale,
		Limit:    c.Limit / scale,
		Currency: c.Currency,
		Pct:      c.Pct(),
	}
}

// fillClaudeInfo copies one UsageInfo's numbers into a wire entry.
func fillClaudeInfo(e *claudeUsageNumbers, info *UsageInfo) {
	if info == nil {
		return
	}
	e.FiveHour = usageWindowFrom("", info.FiveHour)
	e.SevenDay = usageWindowFrom("", info.SevenDay)
	if info.WeeklyScopedLabel != "" || info.WeeklyScoped != (usageBucket{}) {
		e.WeeklyScoped = usageWindowFrom(info.WeeklyScopedLabel, info.WeeklyScoped)
	}
	e.Credits = usageCreditsFrom(info.Credits)
}

// usageNumbersSource builds usageNumbersResponse. One lives for the life of
// the server; the CLI's local path builds a throwaway one.
//
// Refresh rule — never a fresh upstream fetch per request:
//   - a per-account cache file written within usageRefreshInterval means a
//     hub (the TUI) is polling, so answer from disk;
//   - otherwise, at most once per usageRefreshInterval, run ONE pass of the
//     same live and known fetchers the hubs use. They apply the shared disk
//     backoff, so an account with an armed wait gets no request at all;
//   - otherwise answer from disk.
//
// mu single-flights that decision: a concurrent caller waits, then sees the
// fresh lastRefresh and reads disk.
type usageNumbersSource struct {
	source string
	// fetchProviders lets Grok/Codex fetch once when their cache file is
	// missing or old. Only the local path sets it: the server's own Grok and
	// Codex hubs already write those files every pass.
	fetchProviders bool
	// requireLiveSlot (local only) skips the live fetch when the live account
	// has no claude-switch snapshot. Such an account has no cache file, so a
	// throwaway local source has no backoff memory for it at all: every
	// `usage --local` call would be a fresh request. The service keeps its
	// lastLive / fb* memory, which already bounds it to one pass per interval.
	requireLiveSlot bool
	// noFetch serves disk only: no Claude pass, no Grok/Codex fetch. The CLI
	// uses it when the service is alive but slow or broken, so a fallback
	// cannot start a second pass beside the one the service is running.
	noFetch bool

	mu          sync.Mutex
	live        func() (*AccountUsage, error)
	known       func() (*knownAccountsResult, error)
	lastRefresh time.Time
	// lastLive keeps the last live reading for a live account with no
	// claude-switch snapshot: such an account has no cache file, so disk has
	// nothing to serve between passes.
	lastLive *AccountUsage
}

func newUsageNumbersSource(source string) *usageNumbersSource {
	return &usageNumbersSource{
		source:          source,
		fetchProviders:  source == usageNumbersSourceLocal,
		requireLiveSlot: source == usageNumbersSourceLocal,
		// Long-lived closures, wired like NewUsageHub/NewKnownAccountsHub, so
		// the live fetcher keeps its fb* fallback memory across passes.
		live:  newUsageFetcher(func() (*AccountUsage, error) { return usageNumbersLiveFetch() }, saveAccountCache),
		known: newKnownAccountsFetcher(saveAccountCache),
	}
}

// newDiskOnlyUsageNumbersSource is the local source that never fetches.
func newDiskOnlyUsageNumbersSource() *usageNumbersSource {
	s := newUsageNumbersSource(usageNumbersSourceLocal)
	s.noFetch, s.fetchProviders = true, false
	return s
}

// usageNoSnapshotReason tags a local live entry that was not fetched because
// the live account has no claude-switch snapshot to keep backoff state in.
const usageNoSnapshotReason = "no snapshot"

// claudeUsagePass is one pass's raw fetcher output.
type claudeUsagePass struct {
	live    *AccountUsage
	liveErr error
	known   map[string]KnownAccountUsage
	knownOK bool
}

// claudeAccountCacheFreshest returns the newest mtime among this uid's
// per-account cache files (account_cache.go), zero when there are none.
// os.ReadDir plus a name filter, never filepath.Glob: the temp dir is data,
// not a pattern.
func claudeAccountCacheFreshest() time.Time {
	prefix := fmt.Sprintf("claude-sessions-account-%d-", os.Getuid())
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		return time.Time{}
	}
	var newest time.Time
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasPrefix(n, prefix) || !strings.HasSuffix(n, ".json") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if mt := info.ModTime(); mt.After(newest) {
			newest = mt
		}
	}
	return newest
}

// claudeHubAlive reports whether some poller wrote a per-account cache file
// within one interval.
func claudeHubAlive(now time.Time) bool {
	newest := claudeAccountCacheFreshest()
	return !newest.IsZero() && now.Sub(newest) < usageRefreshInterval
}

func (s *usageNumbersSource) build() usageNumbersResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := usageClockNow()
	var pass *claudeUsagePass
	if !s.noFetch && !claudeHubAlive(now) && (s.lastRefresh.IsZero() || now.Sub(s.lastRefresh) >= usageRefreshInterval) {
		pass = s.runClaudePass()
		s.lastRefresh = now
	}
	return usageNumbersResponse{
		GeneratedAt: now,
		Source:      s.source,
		Claude:      s.claudeNumbers(now, pass),
		Grok:        grokUsageNumbers(now, s.fetchProviders),
		Codex:       codexUsageNumbers(now, s.fetchProviders),
	}
}

func (s *usageNumbersSource) runClaudePass() *claudeUsagePass {
	p := &claudeUsagePass{}
	// requireLiveSlot: no snapshot, no live fetch (liveNumbers then answers
	// identity only).
	if name, _ := resolveActiveSnapshotName(loadAccountEmail()); !s.requireLiveSlot || name != "" {
		p.live, p.liveErr = s.live()
		if p.live != nil && p.live.Info != nil {
			s.lastLive = p.live
		}
	}
	if res, err := s.known(); err == nil && res != nil {
		p.knownOK = true
		p.known = make(map[string]KnownAccountUsage, len(res.Accounts))
		for _, a := range res.Accounts {
			p.known[a.Name] = a
		}
	}
	return p
}

// claudeNumbers lists the live account first, then every other snapshot, by
// name. A snapshot whose email matches the live account is skipped, the same
// rule the known-accounts hub and GET /usage apply.
func (s *usageNumbersSource) claudeNumbers(now time.Time, pass *claudeUsagePass) []claudeUsageNumbers {
	liveEmail := loadAccountEmail()
	names, _ := snapshotAccountNames()
	sort.Strings(names)
	activeName := ""
	var others []string
	emails := make(map[string]string, len(names))
	for _, name := range names {
		email := snapshotAccountEmail(name)
		emails[name] = email
		if emailMatchesLive(email, liveEmail) {
			if activeName == "" {
				activeName = name
			}
			continue
		}
		others = append(others, name)
	}

	out := make([]claudeUsageNumbers, 0, len(others)+1)
	if e, ok := s.liveNumbers(now, liveEmail, activeName, pass); ok {
		out = append(out, e)
	}
	for _, name := range others {
		out = append(out, knownNumbers(now, name, emails[name], pass))
	}
	return out
}

// diskClaudeReading is what one account's cache file says, trusted only when
// its Account matches the identity it is read for (entryIdentityMatches).
func diskClaudeReading(name, email string) (info *UsageInfo, fetchedAt time.Time, reason string) {
	if name == "" {
		return nil, time.Time{}, ""
	}
	c, _ := loadAccountCache(name)
	if !entryIdentityMatches(c, email) {
		return nil, time.Time{}, ""
	}
	if c.BackoffStreak > 0 {
		// A streak only ever counts consecutive throttles.
		reason = usageRateLimitedReason
	}
	return c.Info, c.FetchedAt, reason
}

func (s *usageNumbersSource) liveNumbers(now time.Time, liveEmail, activeName string, pass *claudeUsagePass) (claudeUsageNumbers, bool) {
	e := claudeUsageNumbers{Name: activeName, Account: liveEmail, Active: true}
	if s.requireLiveSlot && activeName == "" {
		// Nothing was fetched and there is no file to read: identity only.
		if liveEmail == "" {
			return e, false
		}
		e.Stale, e.Reason = true, usageNoSnapshotReason
		return e, true
	}
	info, fetchedAt, reason := diskClaudeReading(activeName, liveEmail)
	if activeName == "" && s.lastLive != nil && liveEmail != "" && strings.EqualFold(s.lastLive.Account, liveEmail) {
		info, fetchedAt = s.lastLive.Info, s.lastLive.FetchedAt
	}
	marked := false
	if pass != nil {
		switch {
		case pass.liveErr != nil:
			expired, why := classifyUsageErr(pass.liveErr)
			e.Expired, reason, marked = expired, why, true
			if expired {
				// Bars beside a dead credential imply it still works.
				info, fetchedAt = nil, time.Time{}
			}
		case pass.live != nil && pass.live.Info != nil:
			info, fetchedAt, marked = pass.live.Info, pass.live.FetchedAt, pass.live.Stale
			if pass.live.Account != "" {
				e.Account = pass.live.Account
			}
			if !pass.live.Stale {
				reason = ""
			}
		case pass.live != nil:
			// An identity-only placeholder: a wait is armed and nothing was
			// safe to re-serve.
			marked = true
		}
	}
	if e.Account == "" && info == nil {
		return e, false
	}
	fillClaudeInfo(&e, info)
	e.FetchedAt = timePtr(fetchedAt)
	e.Stale = usageNumbersStale(now, fetchedAt, marked)
	e.Reason = reason
	return e, true
}

func knownNumbers(now time.Time, name, email string, pass *claudeUsagePass) claudeUsageNumbers {
	e := claudeUsageNumbers{Name: name, Account: email}
	var info *UsageInfo
	var fetchedAt time.Time
	marked := false
	if r, ok := pass.knownEntry(name); ok {
		info, fetchedAt, marked = r.Info, r.FetchedAt, r.Stale
		e.Expired, e.Reason = r.Expired, r.Reason
		if r.Account != "" {
			e.Account = r.Account
		}
	} else {
		info, fetchedAt, e.Reason = diskClaudeReading(name, email)
	}
	fillClaudeInfo(&e, info)
	e.FetchedAt = timePtr(fetchedAt)
	e.Stale = usageNumbersStale(now, fetchedAt, marked)
	return e
}

func (p *claudeUsagePass) knownEntry(name string) (KnownAccountUsage, bool) {
	if p == nil || !p.knownOK {
		return KnownAccountUsage{}, false
	}
	r, ok := p.known[name]
	return r, ok
}

func providerWindows[W grokWindow | codexWindow](ws []W) []usageWindowNumbers {
	out := make([]usageWindowNumbers, 0, len(ws))
	for _, w := range ws {
		// Both window types share one field set; a conversion through
		// grokWindow keeps this generic without reflection.
		g := grokWindow(w)
		out = append(out, usageWindowNumbers{Label: g.Label, Pct: g.Pct, ResetsAt: timePtr(g.ResetsAt)})
	}
	return out
}

// providerNeedsFetch reports whether a provider cache file is missing or
// older than one poll interval.
func providerNeedsFetch(now, fetchedAt time.Time, present bool) bool {
	return !present || fetchedAt.IsZero() || now.Sub(fetchedAt) >= usageRefreshInterval
}

// grokUsageNumbers reads the Grok hub's cache file and, when fetch is set and
// the file is missing or old, fetches once and saves it. A failed fetch (or no
// Grok login) leaves whatever was cached — never an error.
func grokUsageNumbers(now time.Time, fetch bool) []providerUsageNumbers {
	u, at := loadGrokUsageCacheEntry()
	if fetch && providerNeedsFetch(now, at, u != nil) {
		if f, err := usageNumbersGrokFetch(); err == nil && f != nil {
			saveGrokUsageCache(f)
			u, at = f, now
		}
	}
	out := make([]providerUsageNumbers, 0, 1)
	if u == nil || u.Info == nil {
		return out
	}
	return append(out, providerUsageNumbers{
		Account:   u.Account,
		Windows:   providerWindows(u.Info.Windows),
		Credits:   usageCreditsFrom(u.Info.Credits),
		FetchedAt: timePtr(at),
		Stale:     usageNumbersStale(now, at, false),
	})
}

// codexUsageNumbers is grokUsageNumbers for Codex.
func codexUsageNumbers(now time.Time, fetch bool) []providerUsageNumbers {
	u, at := loadCodexUsageCacheEntry()
	if fetch && providerNeedsFetch(now, at, u != nil) {
		if f, err := usageNumbersCodexFetch(); err == nil && f != nil {
			saveCodexUsageCache(f)
			u, at = f, now
		}
	}
	out := make([]providerUsageNumbers, 0, 1)
	if u == nil || u.Info == nil {
		return out
	}
	return append(out, providerUsageNumbers{
		Account:   u.Account,
		Windows:   providerWindows(u.Info.Windows),
		Plan:      u.Info.Plan,
		FetchedAt: timePtr(at),
		Stale:     usageNumbersStale(now, at, false),
	})
}

// usageNumbers is GET /usage/numbers. Bearer auth always, loopback included:
// unlike GET /usage it can spend upstream budget.
func (s *server) usageNumbers(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if s.numbers == nil {
		http.Error(w, "usage numbers unavailable", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, s.numbers.build())
}

// usageNumbersServiceConfig names the local service the CLI asks first. A
// seam so tests can point it at an httptest server.
var usageNumbersServiceConfig = func() (ServerConfig, error) {
	tok, err := readServerToken()
	if err != nil {
		return ServerConfig{}, err
	}
	return ServerConfig{Host: localServerHost, Port: localServerPort, Token: tok}, nil
}

// usageNumbersAttempt makes one GET /usage/numbers. status is the HTTP status
// when a response arrived, 0 when none did.
func usageNumbersAttempt(ctx context.Context, srv ServerConfig) (resp usageNumbersResponse, status int, err error) {
	u := fmt.Sprintf("http://%s/usage/numbers", hostPort(srv.Host, srv.Port))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return resp, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+srv.Token)
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		return resp, 0, err
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		return resp, r.StatusCode, fmt.Errorf("HTTP %d", r.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&resp); err != nil {
		return resp, r.StatusCode, fmt.Errorf("bad response: %w", err)
	}
	return resp, r.StatusCode, nil
}

func isTimeoutErr(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// usageNumbersFallback is what the CLI does when the service did not answer.
type usageNumbersFallback int

const (
	// usageNumbersFallbackFull: no service is there to ask (unreachable, no
	// token, 401, or 404 from an older server) — build locally, fetches
	// allowed.
	usageNumbersFallbackFull usageNumbersFallback = iota
	// usageNumbersFallbackDisk: a service is there but slow or broken
	// (timeout, 5xx, bad body). It may be mid-pass, so a local build must not
	// start a second one: serve disk only.
	usageNumbersFallbackDisk
)

// fetchServiceUsageNumbers asks the local service: loopback first, then this
// host's Tailscale address, since an installed service usually binds
// `--bind tailscale` and leaves nothing on 127.0.0.1. On failure it says which
// fallback fits.
func fetchServiceUsageNumbers() (usageNumbersResponse, usageNumbersFallback, error) {
	srv, err := usageNumbersServiceConfig()
	if err != nil {
		return usageNumbersResponse{}, usageNumbersFallbackFull, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), usageNumbersServiceTimeout)
	defer cancel()
	resp, status, err := usageNumbersAttempt(ctx, srv)
	if err != nil && status == 0 && !isTimeoutErr(err) {
		if ts := localTailscaleIPv4(ctx); ts != "" {
			srv.Host = ts
			resp, status, err = usageNumbersAttempt(ctx, srv)
		}
	}
	switch {
	case err == nil:
		return resp, usageNumbersFallbackFull, nil
	case status == http.StatusNotFound || status == http.StatusUnauthorized:
		return resp, usageNumbersFallbackFull, err
	case status != 0, isTimeoutErr(err):
		return resp, usageNumbersFallbackDisk, err
	default:
		return resp, usageNumbersFallbackFull, err
	}
}

// buildLocalUsageNumbers is the no-service path; diskOnly selects the source
// that never fetches.
var buildLocalUsageNumbers = func(diskOnly bool) usageNumbersResponse {
	if diskOnly {
		return newDiskOnlyUsageNumbersSource().build()
	}
	return newUsageNumbersSource(usageNumbersSourceLocal).build()
}

// collectUsageNumbers asks the service unless localOnly. A full local build
// runs only when no service is there to ask; a slow or broken one gets a
// disk-only build (see usageNumbersFallback).
func collectUsageNumbers(localOnly bool) usageNumbersResponse {
	if localOnly {
		return buildLocalUsageNumbers(false)
	}
	resp, fallback, err := fetchServiceUsageNumbers()
	if err == nil {
		return resp
	}
	return buildLocalUsageNumbers(fallback == usageNumbersFallbackDisk)
}

// formatResetIn renders time left as two units: "<1m", "42m", "1h12m", "3d4h".
func formatResetIn(d time.Duration) string {
	if d < time.Minute {
		return "<1m"
	}
	mins := int(d.Minutes())
	switch {
	case mins < 60:
		return fmt.Sprintf("%dm", mins)
	case mins < 24*60:
		return fmt.Sprintf("%dh%dm", mins/60, mins%60)
	default:
		return fmt.Sprintf("%dd%dh", mins/(24*60), (mins%(24*60))/60)
	}
}

func usageResetsCell(t *time.Time, now time.Time) string {
	if t == nil || !t.After(now) {
		return "-"
	}
	local := t.Local()
	clock := local.Format("15:04")
	if t.Sub(now) >= 24*time.Hour {
		clock = local.Format("Mon 15:04")
	}
	return fmt.Sprintf("in %s (%s)", formatResetIn(t.Sub(now)), clock)
}

func usageFetchedCell(t *time.Time, now time.Time) string {
	if t == nil {
		return "-"
	}
	return formatAge(now.Sub(*t).Seconds()) + " ago"
}

func usageStaleCell(stale, expired bool, reason string) string {
	if expired {
		// Always spelled out: a dead credential is the one actionable state.
		reason = usageExpiredReason
		stale = true
	}
	s := "no"
	if stale {
		s = "yes"
	}
	if reason != "" {
		s += " (" + reason + ")"
	}
	return s
}

func usageCreditsAmount(c *usageCreditsNumbers) string {
	amt := fmt.Sprintf("used %.2f/%.2f", c.Used, c.Limit)
	if c.Currency != "" {
		amt += " " + c.Currency
	}
	return amt
}

// renderUsageNumbers prints one row per window: TOOL ACCOUNT WINDOW PCT
// RESETS FETCHED STALE. An account with no numbers still gets one row.
func renderUsageNumbers(w io.Writer, resp usageNumbersResponse, now time.Time) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TOOL\tACCOUNT\tWINDOW\tPCT\tRESETS\tFETCHED\tSTALE")
	row := func(tool, account, window, pct, resets, fetched, stale string) {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", tool, account, window, pct, resets, fetched, stale)
	}
	pct := func(p float64) string { return fmt.Sprintf("%.0f%%", p) }

	for _, c := range resp.Claude {
		account := c.Account
		if account == "" {
			account = c.Name
		}
		if c.Active {
			account += " (live)"
		}
		fetched := usageFetchedCell(c.FetchedAt, now)
		stale := usageStaleCell(c.Stale, c.Expired, c.Reason)
		n := 0
		for _, win := range []struct {
			name string
			w    *usageWindowNumbers
		}{{"5h", c.FiveHour}, {"7d", c.SevenDay}, {"wk:" + labelOf(c.WeeklyScoped), c.WeeklyScoped}} {
			if win.w == nil {
				continue
			}
			row("claude", account, win.name, pct(win.w.Pct), usageResetsCell(win.w.ResetsAt, now), fetched, stale)
			n++
		}
		if c.Credits != nil {
			row("claude", account, "credits", pct(c.Credits.Pct), usageCreditsAmount(c.Credits), fetched, stale)
			n++
		}
		if n == 0 {
			row("claude", account, "-", "-", "-", fetched, stale)
		}
	}
	providers := []struct {
		tool    string
		entries []providerUsageNumbers
	}{{"grok", resp.Grok}, {"codex", resp.Codex}}
	for _, p := range providers {
		for _, e := range p.entries {
			fetched := usageFetchedCell(e.FetchedAt, now)
			stale := usageStaleCell(e.Stale, false, "")
			n := 0
			for _, win := range e.Windows {
				row(p.tool, e.Account, win.Label, pct(win.Pct), usageResetsCell(win.ResetsAt, now), fetched, stale)
				n++
			}
			if e.Credits != nil {
				row(p.tool, e.Account, "credits", pct(e.Credits.Pct), usageCreditsAmount(e.Credits), fetched, stale)
				n++
			}
			if n == 0 {
				row(p.tool, e.Account, "-", "-", "-", fetched, stale)
			}
		}
	}
	_ = tw.Flush()
}

func labelOf(w *usageWindowNumbers) string {
	if w == nil {
		return ""
	}
	return w.Label
}
