package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Live capture from a real weekly account, sanitized. Account is not in the
// payload — parse leaves it empty; fetchGrokUsage fills it from loadGrokAuth.
func TestParseGrokUsage(t *testing.T) {
	body := []byte(`{"config":{
  "currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":"2026-08-10T00:00:00Z","end":"2026-08-17T00:00:00Z"},
  "creditUsagePercent":6.0,
  "productUsage":[{"product":"GrokBuild","usagePercent":6.0}],
  "onDemandCap":{"val":0},"onDemandUsed":{"val":0},
  "prepaidBalance":{"val":0},"isUnifiedBillingUser":true,
  "billingPeriodStart":"2026-08-01T00:00:00Z","billingPeriodEnd":"2026-09-01T00:00:00Z"
}}`)
	u, err := parseGrokUsage(body)
	if err != nil {
		t.Fatalf("parseGrokUsage: %v", err)
	}
	if u.Account != "" {
		t.Errorf("Account = %q, want empty (filled by fetch from auth, not payload)", u.Account)
	}
	if len(u.Info.Windows) != 1 {
		t.Fatalf("Windows = %d, want 1: %+v", len(u.Info.Windows), u.Info.Windows)
	}
	w := u.Info.Windows[0]
	if w.Label != "wk" {
		t.Errorf("window label = %q, want wk", w.Label)
	}
	if w.Pct != 6 {
		t.Errorf("window pct = %v, want 6", w.Pct)
	}
	wantReset := time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC)
	if !w.ResetsAt.Equal(wantReset) {
		t.Errorf("window ResetsAt = %v, want %v", w.ResetsAt, wantReset)
	}
}

func TestParseGrokUsageMonthly(t *testing.T) {
	body := []byte(`{"config":{
  "currentPeriod":{"type":"USAGE_PERIOD_TYPE_MONTHLY","start":"2026-08-01T00:00:00Z","end":"2026-09-01T00:00:00Z"},
  "creditUsagePercent":42.5
}}`)
	u, err := parseGrokUsage(body)
	if err != nil {
		t.Fatalf("parseGrokUsage: %v", err)
	}
	if len(u.Info.Windows) != 1 {
		t.Fatalf("Windows = %d, want 1", len(u.Info.Windows))
	}
	if u.Info.Windows[0].Label != "mo" {
		t.Errorf("label = %q, want mo", u.Info.Windows[0].Label)
	}
	if u.Info.Windows[0].Pct != 42.5 {
		t.Errorf("pct = %v, want 42.5", u.Info.Windows[0].Pct)
	}
}

// No currentPeriod is not an error: empty windows, which render no line.
func TestParseGrokUsageNoPeriod(t *testing.T) {
	u, err := parseGrokUsage([]byte(`{"config":{"creditUsagePercent":6.0}}`))
	if err != nil {
		t.Fatalf("parseGrokUsage: %v", err)
	}
	if len(u.Info.Windows) != 0 {
		t.Errorf("Windows = %+v, want none", u.Info.Windows)
	}
}

func TestParseGrokUsageBadJSON(t *testing.T) {
	if _, err := parseGrokUsage([]byte(`not json`)); err == nil {
		t.Error("want error for invalid JSON, got nil")
	}
}

// currentPeriod present, creditUsagePercent omitted, onDemandCap 0 → one window
// at 0% (proto3 omit-zero), not "no window".
func TestParseGrokUsageOmittedPercentIsZero(t *testing.T) {
	body := []byte(`{"config":{
  "currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":"2026-08-10T00:00:00Z","end":"2026-08-17T00:00:00Z"},
  "onDemandCap":{"val":0},"onDemandUsed":{"val":0}
}}`)
	u, err := parseGrokUsage(body)
	if err != nil {
		t.Fatalf("parseGrokUsage: %v", err)
	}
	if len(u.Info.Windows) != 1 {
		t.Fatalf("Windows = %d, want 1 (period present → 0%%, not absent)", len(u.Info.Windows))
	}
	if u.Info.Windows[0].Pct != 0 {
		t.Errorf("Pct = %v, want 0", u.Info.Windows[0].Pct)
	}
	if u.Info.Windows[0].Label != "wk" {
		t.Errorf("label = %q, want wk", u.Info.Windows[0].Label)
	}
}

// Omitted creditUsagePercent with onDemandCap.val > 0 falls back to
// onDemandUsed/onDemandCap * 100.
func TestParseGrokUsageOnDemandFallback(t *testing.T) {
	body := []byte(`{"config":{
  "currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":"2026-08-10T00:00:00Z","end":"2026-08-17T00:00:00Z"},
  "onDemandCap":{"val":100},"onDemandUsed":{"val":25}
}}`)
	u, err := parseGrokUsage(body)
	if err != nil {
		t.Fatalf("parseGrokUsage: %v", err)
	}
	if len(u.Info.Windows) != 1 {
		t.Fatalf("Windows = %d, want 1", len(u.Info.Windows))
	}
	if u.Info.Windows[0].Pct != 25 {
		t.Errorf("Pct = %v, want 25 (onDemand fallback)", u.Info.Windows[0].Pct)
	}
}

// Missing end leaves ResetsAt zero so the renderer omits the trailer.
func TestParseGrokUsageNoReset(t *testing.T) {
	body := []byte(`{"config":{
  "currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":"2026-08-10T00:00:00Z"},
  "creditUsagePercent":12.0
}}`)
	u, err := parseGrokUsage(body)
	if err != nil {
		t.Fatalf("parseGrokUsage: %v", err)
	}
	if len(u.Info.Windows) != 1 {
		t.Fatalf("Windows = %d, want 1", len(u.Info.Windows))
	}
	if !u.Info.Windows[0].ResetsAt.IsZero() {
		t.Errorf("ResetsAt = %v, want zero time (no end)", u.Info.Windows[0].ResetsAt)
	}
	if u.Info.Windows[0].Pct != 12 {
		t.Errorf("Pct = %v, want 12", u.Info.Windows[0].Pct)
	}
}

// Live capture from GET /v1/billing (no format=credits). monthlyLimit is the
// deprecated included budget — not the extra-usage cap — so only used is kept.
func TestParseGrokMonthlyUsed(t *testing.T) {
	body := []byte(`{"config":{
  "monthlyLimit":{"val":10000},
  "used":{"val":3578},
  "onDemandCap":{"val":0}
}}`)
	if got := parseGrokMonthlyUsed(body); got != 3578 {
		t.Errorf("used = %v, want 3578", got)
	}
}

func TestParseGrokMonthlyUsedOmitted(t *testing.T) {
	if got := parseGrokMonthlyUsed([]byte(`{"config":{"monthlyLimit":{"val":10000}}}`)); got != 0 {
		t.Errorf("omitted used = %v, want 0", got)
	}
}

func TestParseGrokMonthlyUsedBadJSON(t *testing.T) {
	if got := parseGrokMonthlyUsed([]byte(`not json`)); got != 0 {
		t.Errorf("bad JSON used = %v, want 0", got)
	}
}

// Live capture from GET /v1/auto-topup-rule. Purchase amounts are negative cents.
func TestParseGrokAutoTopup(t *testing.T) {
	body := []byte(`{"rule":{
  "enabled":true,
  "minBeforeHittingSl":{"val":5000},
  "topupAmount":{"val":-20000},
  "maxAmountPerMonth":{"val":-200000}
}}`)
	on, max := parseGrokAutoTopup(body)
	if !on {
		t.Fatal("enabled = false, want true")
	}
	if max != 200000 {
		t.Errorf("max = %v, want 200000 (abs of -200000 cents = $2000)", max)
	}
}

func TestParseGrokAutoTopupDisabled(t *testing.T) {
	on, max := parseGrokAutoTopup([]byte(`{"rule":{"topupAmount":{"val":-20000}}}`))
	if on || max != 0 {
		t.Errorf("disabled rule: on=%v max=%v, want false/0", on, max)
	}
}

func TestParseGrokOnDemand(t *testing.T) {
	used, cap := parseGrokOnDemand([]byte(`{"config":{"onDemandCap":{"val":5000},"onDemandUsed":{"val":300}}}`))
	if used != 300 || cap != 5000 {
		t.Errorf("on-demand used/cap = %v/%v, want 300/5000", used, cap)
	}
}

func TestGrokExtraCreditsPrefersOnDemandCap(t *testing.T) {
	c := grokExtraCredits(300, 5000, 3578, true, 200000)
	if !c.Enabled || c.Used != 300 || c.Limit != 5000 {
		t.Errorf("got %+v, want on-demand 300/5000", c)
	}
}

func TestGrokExtraCreditsUsesAutoTopupMax(t *testing.T) {
	c := grokExtraCredits(0, 0, 3578, true, 200000)
	if !c.Enabled || c.Used != 3578 || c.Limit != 200000 {
		t.Errorf("got %+v, want auto-topup 3578/200000", c)
	}
}

func TestGrokExtraCreditsIgnoresDeprecatedMonthlyLimit(t *testing.T) {
	c := grokExtraCredits(0, 0, 3578, false, 0)
	if c.Enabled || c.Limit != 0 {
		t.Errorf("got %+v, want no bar (monthlyLimit is not the extra cap)", c)
	}
}

// Live capture 2026-09-05: auto-topup enabled, $500 topups, no
// maxAmountPerMonth (proto3 omit-zero). That is not "no cap" — it is an
// unset field, so the extra-usage bar still renders against $10,000.
func TestGrokExtraCreditsDefaultsMissingAutoTopupMax(t *testing.T) {
	c := grokExtraCredits(0, 0, 91353, true, 0)
	if !c.Enabled || c.Used != 91353 || c.Limit != 1_000_000 {
		t.Errorf("got %+v, want default $10k 91353/1000000", c)
	}
}

func TestGrokPeriodLabel(t *testing.T) {
	cases := []struct {
		typ  string
		want string
	}{
		{"USAGE_PERIOD_TYPE_WEEKLY", "wk"},
		{"USAGE_PERIOD_TYPE_MONTHLY", "mo"},
		{"USAGE_PERIOD_TYPE_DAILY", "1d"},
		{"USAGE_PERIOD_TYPE_UNKNOWN", "use"},
		{"", "use"},
		{"something_else", "use"},
	}
	for _, c := range cases {
		if got := grokPeriodLabel(c.typ); got != c.want {
			t.Errorf("grokPeriodLabel(%q) = %q, want %q", c.typ, got, c.want)
		}
	}
}

func TestGrokUsageCacheRoundTrip(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	if got := loadGrokUsageCache(); got != nil {
		t.Fatalf("loadGrokUsageCache with no file = %+v, want nil", got)
	}
	want := &GrokAccountUsage{
		Account: "dev@example.com",
		Info: &GrokUsageInfo{
			Windows: []grokWindow{
				{Label: "wk", Pct: 6, ResetsAt: time.Now().Add(5 * 24 * time.Hour).UTC()},
			},
			Credits: creditsInfo{Enabled: true, Used: 368, Limit: 10000, Currency: "USD", DecimalPlaces: 2},
		},
	}
	saveGrokUsageCache(want)
	got := loadGrokUsageCache()
	if got == nil {
		t.Fatal("loadGrokUsageCache after save = nil")
	}
	if got.Account != "dev@example.com" {
		t.Errorf("round-trip account mismatch: %+v", got)
	}
	if len(got.Info.Windows) != 1 || got.Info.Windows[0].Label != "wk" || got.Info.Windows[0].Pct != 6 {
		t.Errorf("round-trip windows mismatch: %+v", got.Info.Windows)
	}
	if !got.Info.Credits.Enabled || got.Info.Credits.Used != 368 || got.Info.Credits.Limit != 10000 {
		t.Errorf("round-trip credits mismatch: %+v", got.Info.Credits)
	}
}

func TestGrokUsageCacheExpiry(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	stale, _ := json.Marshal(cachedGrokUsage{
		FetchedAt: time.Now().Add(-usageCacheMaxAge - time.Minute),
		Usage:     GrokAccountUsage{Account: "a@b.c", Info: &GrokUsageInfo{}},
	})
	if err := os.WriteFile(grokUsageCachePath(), stale, 0600); err != nil {
		t.Fatal(err)
	}
	if got := loadGrokUsageCache(); got != nil {
		t.Errorf("stale cache returned %+v, want nil", got)
	}
	if err := os.WriteFile(grokUsageCachePath(), []byte("not json"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := loadGrokUsageCache(); got != nil {
		t.Errorf("corrupt cache returned %+v, want nil", got)
	}
}

func TestLoadGrokAuthMissingFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if _, _, err := loadGrokAuth(); err == nil {
		t.Error("want error for missing auth.json, got nil")
	}
}

// Prefer the entry whose key starts with https://auth.x.ai:: over other issuers.
func TestLoadGrokAuthPrefersAuthXAI(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".grok")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	// Map order in JSON is insertion order for encoding/json encode, but Go map
	// iteration is random — write with the non-preferred key first in the
	// object text so a naive "first key in file" also exercises preference.
	auth := []byte(`{
  "https://accounts.x.ai/sign-in": {
    "key": "other-token",
    "email": "other@example.com"
  },
  "https://auth.x.ai::default": {
    "key": "preferred-token",
    "email": "pref@example.com"
  }
}`)
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), auth, 0600); err != nil {
		t.Fatal(err)
	}
	tok, email, err := loadGrokAuth()
	if err != nil {
		t.Fatalf("loadGrokAuth: %v", err)
	}
	if tok != "preferred-token" {
		t.Errorf("token = %q, want preferred-token", tok)
	}
	if email != "pref@example.com" {
		t.Errorf("email = %q, want pref@example.com", email)
	}
}
