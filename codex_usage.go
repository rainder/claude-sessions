package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// codexWindow is one rate-limit window from the Codex usage endpoint. Label is
// the human-readable span (5h / wk / mo …, see codexWindowLabel); ResetsAt is
// the absolute reset time (the API's reset_at Unix epoch, converted). JSON tags
// carry it through server→client propagation and the disk cache — note these are
// NOT the API's field names, which parseCodexUsage translates from.
type codexWindow struct {
	Label    string    `json:"label"`
	Pct      float64   `json:"pct"`
	ResetsAt time.Time `json:"resetsAt"`
}

// CodexUsageInfo is the parsed Codex account rate-limit snapshot shown in the
// header. Windows is whichever of the endpoint's primary/secondary windows
// exist (the spans are dynamic — some accounts run 5h+weekly, others weekly
// only — so nothing is hardcoded). Plan is the account's plan_type; it is parsed
// but not rendered.
type CodexUsageInfo struct {
	Windows []codexWindow `json:"windows"`
	Plan    string        `json:"plan"`
}

// CodexAccountUsage pairs a Codex snapshot with the account it belongs to, so a
// remote host's limits stay attributable when it runs a different Codex account
// than the client. Account is the login email from the usage payload ("" when
// absent); Info is the snapshot (nil before the first fetch lands). Mirrors
// AccountUsage for the Codex provider — but unlike Anthropic's, the account
// email comes free in the usage response, so no separate identity read is
// needed and the poller holds this paired shape directly.
type CodexAccountUsage struct {
	Account string          `json:"account"` // email, "" when unknown
	Info    *CodexUsageInfo `json:"info"`
}

// codexWindowLabel maps a window's limit_window_seconds to a short label:
// 18000→"5h", 604800→"wk", 2592000→"mo"; anything else is generic, "<n>h" below
// 48 hours else "<n>d". The endpoint's spans are dynamic, so the generic branch
// keeps an unfamiliar window legible rather than mislabeling it.
func codexWindowLabel(seconds int) string {
	switch seconds {
	case 18000:
		return "5h"
	case 604800:
		return "wk"
	case 2592000:
		return "mo"
	}
	if seconds < 48*3600 {
		return fmt.Sprintf("%dh", seconds/3600)
	}
	return fmt.Sprintf("%dd", seconds/86400)
}

// parseCodexUsage decodes the Codex /usage response. The endpoint's shape
// differs from Anthropic's: percentages are used_percent (not utilization),
// resets are reset_at Unix-epoch integers (not RFC3339 strings — a time.Time
// field would refuse the bare number), and the windows nest under rate_limit.
// So a dedicated raw struct decodes the wire shape and this builds the domain
// snapshot from it. primary_window then secondary_window are appended when
// present (either may be null); additional_rate_limits and credits are out of
// scope. A body with no windows is not an error — it yields an empty snapshot
// that renders no line, rather than retrying forever.
func parseCodexUsage(body []byte) (*CodexAccountUsage, error) {
	type rawWindow struct {
		UsedPercent        float64 `json:"used_percent"`
		LimitWindowSeconds int     `json:"limit_window_seconds"`
		ResetAt            int64   `json:"reset_at"`
	}
	var raw struct {
		Email     string `json:"email"`
		PlanType  string `json:"plan_type"`
		RateLimit *struct {
			PrimaryWindow   *rawWindow `json:"primary_window"`
			SecondaryWindow *rawWindow `json:"secondary_window"`
		} `json:"rate_limit"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	info := &CodexUsageInfo{Plan: raw.PlanType}
	if raw.RateLimit != nil {
		for _, w := range []*rawWindow{raw.RateLimit.PrimaryWindow, raw.RateLimit.SecondaryWindow} {
			if w == nil {
				continue
			}
			// A missing reset_at (wire 0) stays a zero time.Time — converting it to
			// time.Unix(0,0) would render as an imminent "<1m" reset; the renderer
			// drops the trailer instead.
			var resetsAt time.Time
			if w.ResetAt != 0 {
				resetsAt = time.Unix(w.ResetAt, 0).UTC()
			}
			info.Windows = append(info.Windows, codexWindow{
				Label:    codexWindowLabel(w.LimitWindowSeconds),
				Pct:      w.UsedPercent,
				ResetsAt: resetsAt,
			})
		}
	}
	return &CodexAccountUsage{Account: raw.Email, Info: info}, nil
}

// codexUsageURL is the endpoint the Codex CLI polls for usage. Unofficial and
// undocumented, so every failure is non-fatal (no bar, never a crash).
const codexUsageURL = "https://chatgpt.com/backend-api/wham/usage"

// loadCodexAuth reads Codex CLI's OAuth access token and ChatGPT account id from
// ~/.codex/auth.json (tokens.access_token / tokens.account_id). A missing file
// or empty token means Codex isn't installed/authenticated — returned as an
// error the caller treats as "no codex bars", silently. Read-only and re-read on
// every fetch: Codex owns the token's refresh/rotation, so a stale/expired token
// just yields no bar; we never write or refresh it.
func loadCodexAuth() (token, accountID string, err error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", err
	}
	data, err := os.ReadFile(filepath.Join(home, ".codex", "auth.json"))
	if err != nil {
		return "", "", err
	}
	var raw struct {
		Tokens struct {
			AccessToken string `json:"access_token"`
			AccountID   string `json:"account_id"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return "", "", fmt.Errorf("parse codex auth: %w", err)
	}
	if raw.Tokens.AccessToken == "" {
		return "", "", fmt.Errorf("no codex access token")
	}
	return raw.Tokens.AccessToken, raw.Tokens.AccountID, nil
}

// fetchCodexUsage hits the Codex usage endpoint with the current token. The
// three headers mirror what the Codex CLI sends; the User-Agent is set
// explicitly because Go's client otherwise sends its own default. 5s HTTP
// timeout, 1MB response cap, non-200 is an error.
func fetchCodexUsage() (*CodexAccountUsage, error) {
	tok, accountID, err := loadCodexAuth()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest("GET", codexUsageURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("ChatGPT-Account-Id", accountID)
	req.Header.Set("User-Agent", "codex-cli")
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("codex usage endpoint: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	return parseCodexUsage(body)
}

// codexUsageCachePath is where the last successful Codex fetch is persisted so a
// restart during an endpoint throttle still has something to show. Separate file
// from the Anthropic cache; UID in the name keeps multi-user /tmp collisions
// away.
func codexUsageCachePath() string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("claude-sessions-codex-usage-%d.json", os.Getuid()))
}

// cachedCodexUsage is the on-disk envelope: the snapshot (account included) plus
// when it was fetched.
type cachedCodexUsage struct {
	FetchedAt time.Time         `json:"fetched_at"`
	Usage     CodexAccountUsage `json:"usage"`
}

// saveCodexUsageCache persists a successful fetch. Best-effort: a read-only
// /tmp just means no warm start next launch.
func saveCodexUsageCache(u *CodexAccountUsage) {
	data, err := json.Marshal(cachedCodexUsage{FetchedAt: time.Now(), Usage: *u})
	if err != nil {
		return
	}
	_ = os.WriteFile(codexUsageCachePath(), data, 0600)
}

// loadCodexUsageCache returns the cached snapshot, or nil if absent, unreadable,
// or older than usageCacheMaxAge (shared with the Anthropic cache).
func loadCodexUsageCache() *CodexAccountUsage {
	data, err := os.ReadFile(codexUsageCachePath())
	if err != nil {
		return nil
	}
	var c cachedCodexUsage
	if err := json.Unmarshal(data, &c); err != nil {
		return nil
	}
	if c.FetchedAt.IsZero() || time.Since(c.FetchedAt) > usageCacheMaxAge {
		return nil
	}
	return &c.Usage
}

// CodexUsageHub polls the Codex usage endpoint in the background, mirroring
// UsageHub for the Codex provider (see usagePoller for the shared mechanism).
// It holds the account-paired snapshot directly since the email is in the
// payload. The public surface — NewCodexUsageHub, Snapshot, Pause, Resume, Kick,
// Shutdown — matches UsageHub so every TUI call site treats them alike.
type CodexUsageHub = usagePoller[CodexAccountUsage]

// NewCodexUsageHub starts the poller, seeded from a recent disk cache.
func NewCodexUsageHub() *CodexUsageHub {
	return newUsagePoller(loadCodexUsageCache(), fetchCodexUsage, saveCodexUsageCache)
}
