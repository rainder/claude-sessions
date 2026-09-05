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
// the human-readable span (wk / mo / 1d / use, see grokPeriodLabel); ResetsAt is
// currentPeriod.end. JSON tags carry it through server→client propagation and
// the disk cache — not the API's field names, which parseGrokUsage translates.
type grokWindow struct {
	Label    string    `json:"label"`
	Pct      float64   `json:"pct"`
	ResetsAt time.Time `json:"resetsAt"`
}

// GrokUsageInfo is the parsed Grok account usage snapshot shown in the header.
// Windows holds at most one entry — the current period from config.currentPeriod
// (weekly / monthly / daily / credits). Credits is extra-usage spend against the
// user-set cap (on-demand cap, else auto-topup maxAmountPerMonth), in cents.
// An enabled auto-topup with no maxAmountPerMonth (proto3 omit-zero) uses
// grokDefaultMonthlyCreditsLimit rather than hiding the bar. Deprecated
// monthlyLimit is the included budget and is not the cap. Zero value hides
// the cr bar. Unlike Codex there is no Plan field.
type GrokUsageInfo struct {
	Windows []grokWindow `json:"windows"`
	Credits creditsInfo  `json:"credits,omitempty"`
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
// Unknown types fall to "use" rather than inventing a span. "cr" is reserved
// for the monthly spend segment (grokSegs), matching claudeSegs.
func grokPeriodLabel(periodType string) string {
	switch periodType {
	case "USAGE_PERIOD_TYPE_WEEKLY":
		return "wk"
	case "USAGE_PERIOD_TYPE_MONTHLY":
		return "mo"
	case "USAGE_PERIOD_TYPE_DAILY":
		return "1d"
	default:
		return "use"
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

// parseGrokMonthlyUsed reads calendar-month extra spend from GET /v1/billing
// (no format=credits), in cents. monthlyLimit in that payload is the deprecated
// included budget, not the extra-usage cap — it is ignored. Bad JSON or a
// missing used field → 0.
func parseGrokMonthlyUsed(body []byte) float64 {
	type rawVal struct {
		Val float64 `json:"val"`
	}
	var raw struct {
		Config *struct {
			Used rawVal `json:"used"`
		} `json:"config"`
	}
	if err := json.Unmarshal(body, &raw); err != nil || raw.Config == nil {
		return 0
	}
	return raw.Config.Used.Val
}

// parseGrokOnDemand reads pay-as-you-go used/cap from the format=credits body.
func parseGrokOnDemand(body []byte) (used, cap float64) {
	type rawVal struct {
		Val float64 `json:"val"`
	}
	var raw struct {
		Config *struct {
			OnDemandCap  rawVal `json:"onDemandCap"`
			OnDemandUsed rawVal `json:"onDemandUsed"`
		} `json:"config"`
	}
	if err := json.Unmarshal(body, &raw); err != nil || raw.Config == nil {
		return 0, 0
	}
	return raw.Config.OnDemandUsed.Val, raw.Config.OnDemandCap.Val
}

// parseGrokAutoTopup reads GET /v1/auto-topup-rule. Purchase amounts are
// negative cents on the ledger; the returned max is the absolute value.
// A missing/false enabled field (proto3 omit-false) is disabled.
func parseGrokAutoTopup(body []byte) (enabled bool, max float64) {
	type rawVal struct {
		Val float64 `json:"val"`
	}
	var raw struct {
		Rule *struct {
			Enabled           bool   `json:"enabled"`
			MaxAmountPerMonth rawVal `json:"maxAmountPerMonth"`
		} `json:"rule"`
	}
	if err := json.Unmarshal(body, &raw); err != nil || raw.Rule == nil || !raw.Rule.Enabled {
		return false, 0
	}
	max = raw.Rule.MaxAmountPerMonth.Val
	if max < 0 {
		max = -max
	}
	return true, max
}

// grokDefaultMonthlyCreditsLimit is the display cap, in cents, when auto-topup
// is on but maxAmountPerMonth is omitted. $10,000 — not a live billing write.
const grokDefaultMonthlyCreditsLimit = 1_000_000

// grokExtraCredits picks the extra-usage bar. On-demand cap wins when set
// (classic PAYG). Else an enabled auto-topup max is the user-set monthly cap
// (unified billing). An enabled rule with no max uses
// grokDefaultMonthlyCreditsLimit. monthlyLimit is not a candidate.
func grokExtraCredits(onDemandUsed, onDemandCap, monthlyUsed float64, topupOn bool, topupMax float64) creditsInfo {
	var used, limit float64
	switch {
	case onDemandCap > 0:
		used, limit = onDemandUsed, onDemandCap
	case topupOn:
		if topupMax <= 0 {
			topupMax = grokDefaultMonthlyCreditsLimit
		}
		used, limit = monthlyUsed, topupMax
	default:
		return creditsInfo{}
	}
	return creditsInfo{
		Enabled:       true,
		Used:          used,
		Limit:         limit,
		Currency:      "USD",
		DecimalPlaces: 2,
	}
}

// grokUsageURL is the endpoint the Grok CLI polls for the current-period
// percent bar. grokBillingURL is the same path without format=credits (month
// spend). grokAutoTopupURL is the user-set monthly extra-usage cap. All
// unofficial; every failure is non-fatal (no bar, never a crash).
const (
	grokUsageURL     = "https://cli-chat-proxy.grok.com/v1/billing?format=credits"
	grokBillingURL   = "https://cli-chat-proxy.grok.com/v1/billing"
	grokAutoTopupURL = "https://cli-chat-proxy.grok.com/v1/auto-topup-rule"
)

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

// grokBillingGet GETs url with the Grok CLI's billing headers. 5s timeout,
// 1MB cap, non-200 is an error.
func grokBillingGet(tok, url string) ([]byte, error) {
	req, err := http.NewRequest("GET", url, nil)
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
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

// fetchGrokUsage hits the Grok billing endpoints with the current token.
// format=credits supplies the period window and any on-demand cap; /v1/billing
// supplies calendar-month extra spend; auto-topup-rule supplies the user-set
// monthly cap when on-demand is unset. An enabled rule with no max still
// yields a bar at grokDefaultMonthlyCreditsLimit. The last two are best-effort
// (a failure leaves Credits zero, weekly still shows). The account email comes
// from loadGrokAuth, not the payload.
func fetchGrokUsage() (*GrokAccountUsage, error) {
	tok, email, err := loadGrokAuth()
	if err != nil {
		return nil, err
	}
	body, err := grokBillingGet(tok, grokUsageURL)
	if err != nil {
		return nil, err
	}
	u, err := parseGrokUsage(body)
	if err != nil {
		return nil, err
	}
	if u.Info != nil {
		odUsed, odCap := parseGrokOnDemand(body)
		var monthlyUsed float64
		if monthly, err := grokBillingGet(tok, grokBillingURL); err == nil {
			monthlyUsed = parseGrokMonthlyUsed(monthly)
		}
		var topupOn bool
		var topupMax float64
		if rule, err := grokBillingGet(tok, grokAutoTopupURL); err == nil {
			topupOn, topupMax = parseGrokAutoTopup(rule)
		}
		u.Info.Credits = grokExtraCredits(odUsed, odCap, monthlyUsed, topupOn, topupMax)
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
