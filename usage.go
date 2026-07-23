package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// usageBucket is one rate-limit window from the OAuth usage endpoint.
type usageBucket struct {
	Pct      float64
	ResetsAt time.Time
}

// creditsInfo is the extra-usage (pay-as-you-go credits) state from the
// usage endpoint. Amounts are in minor currency units (e.g. cents when
// DecimalPlaces is 2).
type creditsInfo struct {
	Enabled       bool
	Used          float64
	Limit         float64
	Currency      string
	DecimalPlaces int
}

// Pct is credits utilization 0–100 (the endpoint's own utilization field is
// often null, so it's derived from used/limit).
func (c creditsInfo) Pct() float64 {
	if c.Limit <= 0 {
		return 0
	}
	return c.Used / c.Limit * 100
}

// UsageInfo is the parsed account rate-limit snapshot shown in the header.
type UsageInfo struct {
	FiveHour usageBucket
	SevenDay usageBucket
	// WeeklyScoped is the model-scoped weekly limit (the "limits" array's
	// weekly_scoped entry). Zero value with an empty WeeklyScopedLabel means
	// the account has no scoped limit and the header hides that bar.
	WeeklyScoped      usageBucket
	WeeklyScopedLabel string
	Credits           creditsInfo
}

// AccountUsage pairs a rate-limit snapshot with the account it belongs to, so a
// remote host's limits stay attributable when it runs a different Anthropic
// account than the client. Account is the login email ("" when the identity
// couldn't be read); Info is the snapshot (nil before the first fetch lands).
type AccountUsage struct {
	Account string     `json:"account"` // email, "" when unknown
	Info    *UsageInfo `json:"info"`
}

// parseUsage decodes the /api/oauth/usage response body. The overall
// five_hour and seven_day buckets are kept; per-model buckets are ignored by
// design. The model-scoped weekly limit is pulled from the "limits" array's
// weekly_scoped entry (label carried from its scope.model.display_name); its
// absence is not an error — the header just hides that bar.
func parseUsage(body []byte) (*UsageInfo, error) {
	type bucket struct {
		Utilization float64   `json:"utilization"`
		ResetsAt    time.Time `json:"resets_at"`
	}
	var raw struct {
		FiveHour   *bucket `json:"five_hour"`
		SevenDay   *bucket `json:"seven_day"`
		ExtraUsage *struct {
			IsEnabled     bool    `json:"is_enabled"`
			MonthlyLimit  float64 `json:"monthly_limit"`
			UsedCredits   float64 `json:"used_credits"`
			Currency      string  `json:"currency"`
			DecimalPlaces int     `json:"decimal_places"`
		} `json:"extra_usage"`
		Limits []struct {
			Kind     string    `json:"kind"`
			Group    string    `json:"group"`
			Percent  float64   `json:"percent"`
			ResetsAt time.Time `json:"resets_at"`
			Scope    *struct {
				Model *struct {
					DisplayName string `json:"display_name"`
				} `json:"model"`
			} `json:"scope"`
		} `json:"limits"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	if raw.FiveHour == nil || raw.SevenDay == nil {
		return nil, fmt.Errorf("usage response missing five_hour/seven_day")
	}
	u := &UsageInfo{
		FiveHour: usageBucket{Pct: raw.FiveHour.Utilization, ResetsAt: raw.FiveHour.ResetsAt},
		SevenDay: usageBucket{Pct: raw.SevenDay.Utilization, ResetsAt: raw.SevenDay.ResetsAt},
	}
	// First weekly_scoped entry wins (fallback: any weekly limit with a named
	// model scope). is_active is intentionally ignored — the live response's
	// scoped entry is often is_active:false, and filtering it would drop the
	// bar.
	for _, l := range raw.Limits {
		scoped := l.Kind == "weekly_scoped" ||
			(l.Group == "weekly" && l.Scope != nil && l.Scope.Model != nil)
		if !scoped || l.Scope == nil || l.Scope.Model == nil || l.Scope.Model.DisplayName == "" {
			continue
		}
		u.WeeklyScoped = usageBucket{Pct: l.Percent, ResetsAt: l.ResetsAt}
		u.WeeklyScopedLabel = l.Scope.Model.DisplayName
		break
	}
	if e := raw.ExtraUsage; e != nil {
		u.Credits = creditsInfo{
			Enabled:       e.IsEnabled,
			Used:          e.UsedCredits,
			Limit:         e.MonthlyLimit,
			Currency:      e.Currency,
			DecimalPlaces: e.DecimalPlaces,
		}
	}
	return u, nil
}

// loadAccountEmail reads the logged-in Anthropic account's email from
// oauthAccount.emailAddress in $HOME/.claude.json (Claude Code's top-level
// config — note this is NOT ~/.claude/.claude.json). Returns "" on any error;
// the header just renders the bars without an account label. Read-only, like
// the token — Claude Code owns this file.
func loadAccountEmail() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		return ""
	}
	var raw struct {
		OAuthAccount struct {
			EmailAddress string `json:"emailAddress"`
		} `json:"oauthAccount"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return ""
	}
	return raw.OAuthAccount.EmailAddress
}

// usageURL is the endpoint Claude Code's /usage command reads.
const usageURL = "https://api.anthropic.com/api/oauth/usage"

// loadOAuthToken reads Claude Code's OAuth access token: from the login
// Keychain on macOS (exec'd `security`, no cgo), from
// ~/.claude/.credentials.json elsewhere. Read-only — Claude Code owns the
// token's refresh/rotation, which is why this is re-read on every fetch.
func loadOAuthToken() (string, error) {
	var data []byte
	if runtime.GOOS == "darwin" {
		out, err := exec.Command("security", "find-generic-password",
			"-s", "Claude Code-credentials", "-w").Output()
		if err != nil {
			return "", fmt.Errorf("keychain read: %w", err)
		}
		data = out
	} else {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		data, err = os.ReadFile(filepath.Join(home, ".claude", ".credentials.json"))
		if err != nil {
			return "", err
		}
	}
	var creds struct {
		ClaudeAiOauth struct {
			AccessToken string `json:"accessToken"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(data, &creds); err != nil {
		return "", fmt.Errorf("parse credentials: %w", err)
	}
	if creds.ClaudeAiOauth.AccessToken == "" {
		return "", fmt.Errorf("no access token in credentials")
	}
	return creds.ClaudeAiOauth.AccessToken, nil
}

// fetchUsage hits the usage endpoint with the current token. The HTTP leg
// has a 5s timeout; credential loading (macOS Keychain) is unbounded but
// runs off the render path in UsageHub's background goroutine.
func fetchUsage() (*UsageInfo, error) {
	tok, err := loadOAuthToken()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest("GET", usageURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("usage endpoint: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	return parseUsage(body)
}

// usageRefreshInterval is how often the background poller refetches. Usage
// percentages move slowly; 2 minutes keeps the bar fresh without hammering
// the endpoint.
const usageRefreshInterval = 2 * time.Minute

// usageRetryMin seeds the failed-fetch backoff. The endpoint 429s readily
// (every Claude Code session shares the account's per-token budget), so a
// failed fetch retries at 5s, 10s, 20s… capped at the refresh interval,
// instead of leaving the header bar blank until the next 2-minute tick.
const usageRetryMin = 5 * time.Second

// usageCacheMaxAge bounds how stale a disk-cached snapshot may be and still
// seed the header on startup. Beyond this the percentages are more likely to
// mislead than inform, so the bar waits for a live fetch instead.
const usageCacheMaxAge = 15 * time.Minute

// usageCachePath is where the last successful fetch is persisted so a
// restart during an endpoint throttle still has something to show. UID in
// the name keeps multi-user /tmp collisions (and permission errors) away.
func usageCachePath() string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("claude-sessions-usage-%d.json", os.Getuid()))
}

// cachedUsage is the on-disk envelope: the snapshot plus when it was fetched.
type cachedUsage struct {
	FetchedAt time.Time `json:"fetched_at"`
	Info      UsageInfo `json:"info"`
}

// saveUsageCache persists a successful fetch. Best-effort: a read-only /tmp
// just means no warm start next launch.
func saveUsageCache(info *UsageInfo) {
	data, err := json.Marshal(cachedUsage{FetchedAt: time.Now(), Info: *info})
	if err != nil {
		return
	}
	_ = os.WriteFile(usageCachePath(), data, 0600)
}

// loadUsageCache returns the cached snapshot, or nil if absent, unreadable,
// or older than usageCacheMaxAge.
func loadUsageCache() *UsageInfo {
	data, err := os.ReadFile(usageCachePath())
	if err != nil {
		return nil
	}
	var c cachedUsage
	if err := json.Unmarshal(data, &c); err != nil {
		return nil
	}
	if c.FetchedAt.IsZero() || time.Since(c.FetchedAt) > usageCacheMaxAge {
		return nil
	}
	return &c.Info
}

// UsageHub polls the usage endpoint in a background goroutine so the render
// loop never blocks on credentials or the network (RemoteHub's pattern,
// minus the wake pipe — the TUI repaints on its own tick, and a slightly
// stale percentage is fine). Snapshot is nil until the first fetch lands, so
// the bar lazily appears on a later repaint; a failed refresh keeps the
// previous value visible instead of blinking the bar away.
type UsageHub struct {
	mu     sync.Mutex
	info   *UsageInfo
	paused atomic.Bool
	kick   chan struct{}
	stop   chan struct{}
}

// NewUsageHub starts the poller and returns immediately; the first fetch is
// kicked off asynchronously. A recent disk-cached snapshot seeds the header
// so a restart while the endpoint is throttling still shows a (stale) bar.
func NewUsageHub() *UsageHub {
	h := &UsageHub{
		info: loadUsageCache(),
		kick: make(chan struct{}, 1),
		stop: make(chan struct{}),
	}
	go h.run()
	h.Kick()
	return h
}

func (h *UsageHub) run() {
	t := time.NewTicker(usageRefreshInterval)
	defer t.Stop()
	backoff := usageRetryMin
	var retry <-chan time.Time
	for {
		select {
		case <-h.stop:
			return
		case <-t.C:
		case <-h.kick:
		case <-retry:
		}
		retry = nil
		if h.paused.Load() {
			continue
		}
		if info, err := fetchUsage(); err == nil {
			h.mu.Lock()
			h.info = info
			h.mu.Unlock()
			saveUsageCache(info)
			backoff = usageRetryMin
		} else {
			retry = time.After(backoff)
			backoff *= 2
			if backoff > usageRefreshInterval {
				backoff = usageRefreshInterval
			}
		}
	}
}

// Snapshot returns the last successful fetch, or nil if none yet.
func (h *UsageHub) Snapshot() *UsageInfo {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.info
}

// Pause makes the poller ignore ticks and kicks — used while an external
// program owns the terminal and nothing renders.
func (h *UsageHub) Pause() { h.paused.Store(true) }

// Resume re-enables polling and kicks an immediate refetch.
func (h *UsageHub) Resume() {
	h.paused.Store(false)
	h.Kick()
}

// Kick requests an immediate refetch. Non-blocking; coalesces when one is
// already pending.
func (h *UsageHub) Kick() {
	select {
	case h.kick <- struct{}{}:
	default:
	}
}

// Shutdown stops the background goroutine.
func (h *UsageHub) Shutdown() {
	close(h.stop)
}
