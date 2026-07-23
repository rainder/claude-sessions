package main

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

// The live capture from a real (weekly-only) account, email sanitized. reset_at
// is a Unix epoch integer, not an RFC3339 string — the parse must convert it.
func TestParseCodexUsage(t *testing.T) {
	body := []byte(`{
		"email": "user@example.com",
		"plan_type": "pro",
		"rate_limit": {
			"allowed": true,
			"limit_reached": false,
			"primary_window": {"used_percent": 96, "limit_window_seconds": 604800, "reset_after_seconds": 442645, "reset_at": 1785258168},
			"secondary_window": null
		},
		"additional_rate_limits": [
			{"limit_name": "GPT-5.3-Codex-Spark", "metered_feature": "codex_bengalfox",
			 "rate_limit": {"allowed": true, "limit_reached": false,
				"primary_window": {"used_percent": 0, "limit_window_seconds": 604800, "reset_after_seconds": 604800, "reset_at": 1785420323},
				"secondary_window": null}}
		],
		"credits": {"has_credits": false, "unlimited": false, "balance": "0"},
		"rate_limit_reached_type": null
	}`)
	u, err := parseCodexUsage(body)
	if err != nil {
		t.Fatalf("parseCodexUsage: %v", err)
	}
	if u.Account != "user@example.com" {
		t.Errorf("Account = %q, want user@example.com", u.Account)
	}
	if u.Info.Plan != "pro" {
		t.Errorf("Plan = %q, want pro", u.Info.Plan)
	}
	// Only primary exists (secondary null); additional_rate_limits is ignored.
	if len(u.Info.Windows) != 1 {
		t.Fatalf("Windows = %d, want 1: %+v", len(u.Info.Windows), u.Info.Windows)
	}
	w := u.Info.Windows[0]
	if w.Label != "wk" {
		t.Errorf("window label = %q, want wk", w.Label)
	}
	if w.Pct != 96 {
		t.Errorf("window pct = %v, want 96", w.Pct)
	}
	wantReset := time.Unix(1785258168, 0).UTC()
	if !w.ResetsAt.Equal(wantReset) {
		t.Errorf("window ResetsAt = %v, want %v (epoch → time)", w.ResetsAt, wantReset)
	}
}

// An account with both a 5h primary and a weekly secondary yields two windows
// in order, proving the spans are read dynamically (not hardcoded 5h+weekly).
func TestParseCodexUsageTwoWindows(t *testing.T) {
	body := []byte(`{
		"email": "dev@example.com",
		"plan_type": "plus",
		"rate_limit": {
			"primary_window": {"used_percent": 42, "limit_window_seconds": 18000, "reset_at": 1785258168},
			"secondary_window": {"used_percent": 7, "limit_window_seconds": 604800, "reset_at": 1785420323}
		}
	}`)
	u, err := parseCodexUsage(body)
	if err != nil {
		t.Fatalf("parseCodexUsage: %v", err)
	}
	if len(u.Info.Windows) != 2 {
		t.Fatalf("Windows = %d, want 2: %+v", len(u.Info.Windows), u.Info.Windows)
	}
	if u.Info.Windows[0].Label != "5h" || u.Info.Windows[0].Pct != 42 {
		t.Errorf("primary = %+v, want 5h/42", u.Info.Windows[0])
	}
	if u.Info.Windows[1].Label != "wk" || u.Info.Windows[1].Pct != 7 {
		t.Errorf("secondary = %+v, want wk/7", u.Info.Windows[1])
	}
}

// A parseable body with no rate_limit is not an error: it yields an empty
// snapshot (which renders no line), rather than retrying forever.
func TestParseCodexUsageNoWindows(t *testing.T) {
	u, err := parseCodexUsage([]byte(`{"email":"x@y.z","plan_type":"free"}`))
	if err != nil {
		t.Fatalf("parseCodexUsage: %v", err)
	}
	if len(u.Info.Windows) != 0 {
		t.Errorf("Windows = %+v, want none", u.Info.Windows)
	}
	if u.Account != "x@y.z" {
		t.Errorf("Account = %q, want x@y.z", u.Account)
	}
}

func TestParseCodexUsageBadJSON(t *testing.T) {
	if _, err := parseCodexUsage([]byte(`not json`)); err == nil {
		t.Error("want error for invalid JSON, got nil")
	}
}

// A window without reset_at (wire value absent → 0) leaves ResetsAt a zero
// time.Time, not the Unix epoch — otherwise the renderer shows a misleading
// imminent "<1m" reset.
func TestParseCodexUsageNoResetAt(t *testing.T) {
	body := []byte(`{
		"email": "x@y.z",
		"rate_limit": {"primary_window": {"used_percent": 12, "limit_window_seconds": 604800}}
	}`)
	u, err := parseCodexUsage(body)
	if err != nil {
		t.Fatalf("parseCodexUsage: %v", err)
	}
	if len(u.Info.Windows) != 1 {
		t.Fatalf("Windows = %d, want 1", len(u.Info.Windows))
	}
	if !u.Info.Windows[0].ResetsAt.IsZero() {
		t.Errorf("ResetsAt = %v, want zero time (no reset_at)", u.Info.Windows[0].ResetsAt)
	}
	if u.Info.Windows[0].Pct != 12 {
		t.Errorf("Pct = %v, want 12", u.Info.Windows[0].Pct)
	}
}

func TestCodexWindowLabel(t *testing.T) {
	cases := []struct {
		seconds int
		want    string
	}{
		{18000, "5h"},    // exact 5h
		{604800, "wk"},   // exact weekly
		{2592000, "mo"},  // exact monthly
		{3600, "1h"},     // generic hours
		{7200, "2h"},     // generic hours
		{86400, "24h"},   // 24h < 48h boundary stays in hours
		{172800, "2d"},   // 48h flips to days
		{259200, "3d"},   // generic days
	}
	for _, c := range cases {
		if got := codexWindowLabel(c.seconds); got != c.want {
			t.Errorf("codexWindowLabel(%d) = %q, want %q", c.seconds, got, c.want)
		}
	}
}

func TestCodexUsageCacheRoundTrip(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	if got := loadCodexUsageCache(); got != nil {
		t.Fatalf("loadCodexUsageCache with no file = %+v, want nil", got)
	}
	want := &CodexAccountUsage{
		Account: "dev@example.com",
		Info: &CodexUsageInfo{
			Plan: "pro",
			Windows: []codexWindow{
				{Label: "5h", Pct: 42, ResetsAt: time.Now().Add(time.Hour).UTC()},
				{Label: "wk", Pct: 7, ResetsAt: time.Now().Add(72 * time.Hour).UTC()},
			},
		},
	}
	saveCodexUsageCache(want)
	got := loadCodexUsageCache()
	if got == nil {
		t.Fatal("loadCodexUsageCache after save = nil")
	}
	if got.Account != "dev@example.com" || got.Info.Plan != "pro" {
		t.Errorf("round-trip account/plan mismatch: %+v", got)
	}
	if len(got.Info.Windows) != 2 || got.Info.Windows[0].Label != "5h" || got.Info.Windows[1].Pct != 7 {
		t.Errorf("round-trip windows mismatch: %+v", got.Info.Windows)
	}
}

func TestCodexUsageCacheExpiry(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	stale, _ := json.Marshal(cachedCodexUsage{
		FetchedAt: time.Now().Add(-usageCacheMaxAge - time.Minute),
		Usage:     CodexAccountUsage{Account: "a@b.c", Info: &CodexUsageInfo{}},
	})
	if err := os.WriteFile(codexUsageCachePath(), stale, 0600); err != nil {
		t.Fatal(err)
	}
	if got := loadCodexUsageCache(); got != nil {
		t.Errorf("stale cache returned %+v, want nil", got)
	}
	if err := os.WriteFile(codexUsageCachePath(), []byte("not json"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := loadCodexUsageCache(); got != nil {
		t.Errorf("corrupt cache returned %+v, want nil", got)
	}
}
