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
	"time"
)

// usageBucket is one rate-limit window from the OAuth usage endpoint.
type usageBucket struct {
	Pct      float64
	ResetsAt time.Time
}

// UsageInfo is the parsed account rate-limit snapshot shown in the header.
type UsageInfo struct {
	FiveHour usageBucket
	SevenDay usageBucket
}

// parseUsage decodes the /api/oauth/usage response body. Only the overall
// five_hour and seven_day buckets are kept; per-model buckets are ignored
// by design (see the spec).
func parseUsage(body []byte) (*UsageInfo, error) {
	type bucket struct {
		Utilization float64   `json:"utilization"`
		ResetsAt    time.Time `json:"resets_at"`
	}
	var raw struct {
		FiveHour *bucket `json:"five_hour"`
		SevenDay *bucket `json:"seven_day"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	if raw.FiveHour == nil || raw.SevenDay == nil {
		return nil, fmt.Errorf("usage response missing five_hour/seven_day")
	}
	return &UsageInfo{
		FiveHour: usageBucket{Pct: raw.FiveHour.Utilization, ResetsAt: raw.FiveHour.ResetsAt},
		SevenDay: usageBucket{Pct: raw.SevenDay.Utilization, ResetsAt: raw.SevenDay.ResetsAt},
	}, nil
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

// fetchUsage hits the usage endpoint with the current token. 5s timeout.
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

// UsageHub polls the usage endpoint in a background goroutine so the render
// loop never blocks on credentials or the network (RemoteHub's pattern,
// minus the wake pipe — the TUI repaints on its own tick, and a slightly
// stale percentage is fine). Snapshot is nil until the first fetch lands, so
// the bar lazily appears on a later repaint; a failed refresh keeps the
// previous value visible instead of blinking the bar away.
type UsageHub struct {
	mu   sync.Mutex
	info *UsageInfo
	kick chan struct{}
	stop chan struct{}
}

// NewUsageHub starts the poller and returns immediately; the first fetch is
// kicked off asynchronously.
func NewUsageHub() *UsageHub {
	h := &UsageHub{
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
	for {
		select {
		case <-h.stop:
			return
		case <-t.C:
		case <-h.kick:
		}
		if info, err := fetchUsage(); err == nil {
			h.mu.Lock()
			h.info = info
			h.mu.Unlock()
		}
	}
}

// Snapshot returns the last successful fetch, or nil if none yet.
func (h *UsageHub) Snapshot() *UsageInfo {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.info
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
