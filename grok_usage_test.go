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

func TestGrokPeriodLabel(t *testing.T) {
	cases := []struct {
		typ  string
		want string
	}{
		{"USAGE_PERIOD_TYPE_WEEKLY", "wk"},
		{"USAGE_PERIOD_TYPE_MONTHLY", "mo"},
		{"USAGE_PERIOD_TYPE_DAILY", "1d"},
		{"USAGE_PERIOD_TYPE_UNKNOWN", "cr"},
		{"", "cr"},
		{"something_else", "cr"},
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
