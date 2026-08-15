package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// grokWindow is one rate-limit window from the Grok billing endpoint. Label is
// the human-readable span (wk / mo / 1d / cr, see grokPeriodLabel); ResetsAt is
// currentPeriod.end. JSON tags carry it through server→client propagation and
// the disk cache — not the API's field names, which parseGrokUsage translates.
type grokWindow struct {
	Label    string    `json:"label"`
	Pct      float64   `json:"pct"`
	ResetsAt time.Time `json:"resetsAt"`
}

// GrokUsageInfo is the parsed Grok account usage snapshot shown in the header.
// Windows holds at most one entry — the current period from config.currentPeriod
// (weekly / monthly / daily / credits). Unlike Codex there is no Plan field.
type GrokUsageInfo struct {
	Windows []grokWindow `json:"windows"`
}

// GrokAccountUsage pairs a Grok snapshot with the account it belongs to, so a
// remote host's limits stay attributable when it runs a different Grok login
// than the client. Account is the email from loadGrokAuth ("" when unknown);
// the billing JSON never carries it. Info is the snapshot (nil before the first
// fetch lands). Mirrors CodexAccountUsage for the Grok provider.
type GrokAccountUsage struct {
	Account string         `json:"account"` // email, "" when unknown
	Info    *GrokUsageInfo `json:"info"`
}

// grokPeriodLabel maps config.currentPeriod.type to a short header label.
// Unknown types fall to "cr" (credits) rather than inventing a span.
func grokPeriodLabel(periodType string) string {
	switch periodType {
	case "USAGE_PERIOD_TYPE_WEEKLY":
		return "wk"
	case "USAGE_PERIOD_TYPE_MONTHLY":
		return "mo"
	case "USAGE_PERIOD_TYPE_DAILY":
		return "1d"
	default:
		return "cr"
	}
}

// parseGrokUsage decodes the Grok /v1/billing?format=credits response. The
// account email is not in the payload — Account is left empty for the caller
// (fetchGrokUsage) to fill from loadGrokAuth. One window is built from
// config.currentPeriod when present; productUsage is ignored (no extra bars).
//
// creditUsagePercent is a *float64 so an omitted field (proto3 zero-elided as
// 0%) is distinguishable from an explicit 0: when the pointer is nil and
// onDemandCap.val > 0, fall back to onDemandUsed/onDemandCap; when a period
// exists but neither source is usable, still emit one window at 0% rather than
// "no window". Missing/unparseable end → zero ResetsAt (renderer omits trailer).
// No config or no currentPeriod → empty Windows, not an error.
func parseGrokUsage(body []byte) (*GrokAccountUsage, error) {
	type rawVal struct {
		Val float64 `json:"val"`
	}
	type rawPeriod struct {
		Type  string `json:"type"`
		Start string `json:"start"`
		End   string `json:"end"`
	}
	var raw struct {
		Config *struct {
			CurrentPeriod      *rawPeriod `json:"currentPeriod"`
			CreditUsagePercent *float64   `json:"creditUsagePercent"`
			OnDemandCap        rawVal     `json:"onDemandCap"`
			OnDemandUsed       rawVal     `json:"onDemandUsed"`
		} `json:"config"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	info := &GrokUsageInfo{}
	if raw.Config != nil && raw.Config.CurrentPeriod != nil {
		p := raw.Config.CurrentPeriod
		var pct float64
		switch {
		case raw.Config.CreditUsagePercent != nil:
			pct = *raw.Config.CreditUsagePercent
		case raw.Config.OnDemandCap.Val > 0:
			pct = raw.Config.OnDemandUsed.Val / raw.Config.OnDemandCap.Val * 100
		}
		// else pct stays 0 — period present, sources unusable (proto3 omit-zero).
		var resetsAt time.Time
		if p.End != "" {
			if t, err := time.Parse(time.RFC3339, p.End); err == nil {
				resetsAt = t.UTC()
			}
		}
		info.Windows = append(info.Windows, grokWindow{
			Label:    grokPeriodLabel(p.Type),
			Pct:      pct,
			ResetsAt: resetsAt,
		})
	}
	return &GrokAccountUsage{Info: info}, nil
}

// grokUsageURL is the endpoint the Grok CLI polls for billing. Unofficial and
// undocumented, so every failure is non-fatal (no bar, never a crash).
const grokUsageURL = "https://cli-chat-proxy.grok.com/v1/billing?format=credits"

// loadGrokAuth reads the Grok CLI bearer token and email from ~/.grok/auth.json.
// The file is a map keyed by issuer URL; each entry has key (JWT), email, and
// optional expires_at. Prefer the first entry whose map key starts with
// "https://auth.x.ai::" and has a non-empty key; else the first entry with a
// non-empty key. Missing file / empty token → error the caller treats as "no
// grok bars". expires_at is deliberately not checked (Codex does not either) —
// a 401 just yields no bar. Read-only; never write or refresh the token.
func loadGrokAuth() (token, email string, err error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", err
	}
	data, err := os.ReadFile(filepath.Join(home, ".grok", "auth.json"))
	if err != nil {
		return "", "", err
	}
	var raw map[string]struct {
		Key       string `json:"key"`
		Email     string `json:"email"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return "", "", fmt.Errorf("parse grok auth: %w", err)
	}
	const preferredPrefix = "https://auth.x.ai::"
	var fallbackToken, fallbackEmail string
	for issuer, entry := range raw {
		if entry.Key == "" {
			continue
		}
		if strings.HasPrefix(issuer, preferredPrefix) {
			return entry.Key, entry.Email, nil
		}
		if fallbackToken == "" {
			fallbackToken, fallbackEmail = entry.Key, entry.Email
		}
	}
	if fallbackToken == "" {
		return "", "", fmt.Errorf("no grok access token")
	}
	return fallbackToken, fallbackEmail, nil
}

// fetchGrokUsage hits the Grok billing endpoint with the current token. Headers
// mirror what the Grok CLI sends. 5s HTTP timeout, 1MB response cap, non-200 is
// an error. The account email comes from loadGrokAuth, not the payload.
func fetchGrokUsage() (*GrokAccountUsage, error) {
	tok, email, err := loadGrokAuth()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest("GET", grokUsageURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("x-xai-token-auth", "xai-grok-cli")
	req.Header.Set("User-Agent", "xai-grok-cli")
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("grok usage endpoint: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	u, err := parseGrokUsage(body)
	if err != nil {
		return nil, err
	}
	u.Account = email
	return u, nil
}

// grokUsageCachePath is where the last successful Grok fetch is persisted so a
// restart during an endpoint throttle still has something to show. Separate file
// from the Anthropic/Codex caches; UID in the name keeps multi-user /tmp
// collisions away.
func grokUsageCachePath() string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("claude-sessions-grok-usage-%d.json", os.Getuid()))
}

// cachedGrokUsage is the on-disk envelope: the snapshot (account included) plus
// when it was fetched.
type cachedGrokUsage struct {
	FetchedAt time.Time        `json:"fetched_at"`
	Usage     GrokAccountUsage `json:"usage"`
}

// saveGrokUsageCache persists a successful fetch. Best-effort: a read-only
// /tmp just means no warm start next launch.
func saveGrokUsageCache(u *GrokAccountUsage) {
	data, err := json.Marshal(cachedGrokUsage{FetchedAt: time.Now(), Usage: *u})
	if err != nil {
		return
	}
	_ = os.WriteFile(grokUsageCachePath(), data, 0600)
}

// loadGrokUsageCache returns the cached snapshot, or nil if absent, unreadable,
// or older than usageCacheMaxAge. Same bound Codex uses — the Anthropic side
// went unbounded for carry-forward; this is still the constant's use for warm
// restarts of non-Anthropic pollers.
func loadGrokUsageCache() *GrokAccountUsage {
	data, err := os.ReadFile(grokUsageCachePath())
	if err != nil {
		return nil
	}
	var c cachedGrokUsage
	if err := json.Unmarshal(data, &c); err != nil {
		return nil
	}
	if c.FetchedAt.IsZero() || time.Since(c.FetchedAt) > usageCacheMaxAge {
		return nil
	}
	return &c.Usage
}

// GrokUsageHub polls the Grok billing endpoint in the background, mirroring
// CodexUsageHub for the Grok provider (see usagePoller for the shared mechanism).
// It holds the account-paired snapshot directly; the email is filled at fetch
// from auth.json. The public surface matches UsageHub so every TUI call site
// treats them alike.
type GrokUsageHub = usagePoller[GrokAccountUsage]

// NewGrokUsageHub starts the poller, seeded from a recent disk cache.
func NewGrokUsageHub() *GrokUsageHub {
	return newUsagePoller(loadGrokUsageCache(), fetchGrokUsage, saveGrokUsageCache)
}
