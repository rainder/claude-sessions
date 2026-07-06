package main

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestParseUsage(t *testing.T) {
	body := []byte(`{
		"five_hour": {"utilization": 9.0,  "resets_at": "2026-06-10T15:19:59.696947+00:00"},
		"seven_day": {"utilization": 13.0, "resets_at": "2026-06-10T18:00:00.696977+00:00"},
		"seven_day_sonnet": {"utilization": 1.0, "resets_at": "2026-06-10T18:00:00.696987+00:00"},
		"extra_usage": {"is_enabled": false}
	}`)
	u, err := parseUsage(body)
	if err != nil {
		t.Fatalf("parseUsage: %v", err)
	}
	if u.FiveHour.Pct != 9.0 {
		t.Errorf("FiveHour.Pct = %v, want 9.0", u.FiveHour.Pct)
	}
	if u.SevenDay.Pct != 13.0 {
		t.Errorf("SevenDay.Pct = %v, want 13.0", u.SevenDay.Pct)
	}
	wantReset := time.Date(2026, 6, 10, 15, 19, 59, 696947000, time.UTC)
	if !u.FiveHour.ResetsAt.Equal(wantReset) {
		t.Errorf("FiveHour.ResetsAt = %v, want %v", u.FiveHour.ResetsAt, wantReset)
	}
	wantWeeklyReset := time.Date(2026, 6, 10, 18, 0, 0, 696977000, time.UTC)
	if !u.SevenDay.ResetsAt.Equal(wantWeeklyReset) {
		t.Errorf("SevenDay.ResetsAt = %v, want %v", u.SevenDay.ResetsAt, wantWeeklyReset)
	}
	if u.Credits.Enabled {
		t.Error("Credits.Enabled = true, want false")
	}
}

func TestParseUsageCredits(t *testing.T) {
	body := []byte(`{
		"five_hour": {"utilization": 9.0,  "resets_at": "2026-06-10T15:19:59+00:00"},
		"seven_day": {"utilization": 13.0, "resets_at": "2026-06-10T18:00:00+00:00"},
		"extra_usage": {
			"is_enabled": true,
			"monthly_limit": 100000,
			"used_credits": 2550.0,
			"utilization": null,
			"currency": "USD",
			"decimal_places": 2
		}
	}`)
	u, err := parseUsage(body)
	if err != nil {
		t.Fatalf("parseUsage: %v", err)
	}
	c := u.Credits
	if !c.Enabled {
		t.Fatal("Credits.Enabled = false, want true")
	}
	if c.Used != 2550 || c.Limit != 100000 {
		t.Errorf("Credits used/limit = %v/%v, want 2550/100000", c.Used, c.Limit)
	}
	if c.Currency != "USD" || c.DecimalPlaces != 2 {
		t.Errorf("Credits currency/places = %q/%d, want USD/2", c.Currency, c.DecimalPlaces)
	}
	if got := c.Pct(); got != 2.55 {
		t.Errorf("Credits.Pct() = %v, want 2.55", got)
	}
}

func TestParseUsageNoExtraUsage(t *testing.T) {
	body := []byte(`{
		"five_hour": {"utilization": 9.0,  "resets_at": "2026-06-10T15:19:59+00:00"},
		"seven_day": {"utilization": 13.0, "resets_at": "2026-06-10T18:00:00+00:00"}
	}`)
	u, err := parseUsage(body)
	if err != nil {
		t.Fatalf("parseUsage: %v", err)
	}
	if u.Credits.Enabled || u.Credits.Pct() != 0 {
		t.Errorf("Credits = %+v, want zero value", u.Credits)
	}
}

func TestUsageCacheRoundTrip(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	if got := loadUsageCache(); got != nil {
		t.Fatalf("loadUsageCache with no file = %+v, want nil", got)
	}
	want := &UsageInfo{
		FiveHour: usageBucket{Pct: 85, ResetsAt: time.Now().Add(time.Hour).UTC()},
		SevenDay: usageBucket{Pct: 46, ResetsAt: time.Now().Add(48 * time.Hour).UTC()},
		Credits:  creditsInfo{Enabled: true, Used: 2550, Limit: 100000, Currency: "USD", DecimalPlaces: 2},
	}
	saveUsageCache(want)
	got := loadUsageCache()
	if got == nil {
		t.Fatal("loadUsageCache after save = nil")
	}
	if got.FiveHour.Pct != 85 || got.SevenDay.Pct != 46 || !got.Credits.Enabled || got.Credits.Used != 2550 {
		t.Errorf("round-trip mismatch: %+v", got)
	}
}

func TestUsageCacheExpiry(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	stale, _ := json.Marshal(cachedUsage{
		FetchedAt: time.Now().Add(-usageCacheMaxAge - time.Minute),
		Info:      UsageInfo{FiveHour: usageBucket{Pct: 85}},
	})
	if err := os.WriteFile(usageCachePath(), stale, 0600); err != nil {
		t.Fatal(err)
	}
	if got := loadUsageCache(); got != nil {
		t.Errorf("stale cache returned %+v, want nil", got)
	}
	if err := os.WriteFile(usageCachePath(), []byte("not json"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := loadUsageCache(); got != nil {
		t.Errorf("corrupt cache returned %+v, want nil", got)
	}
}

func TestParseUsageMissingBuckets(t *testing.T) {
	if _, err := parseUsage([]byte(`{}`)); err == nil {
		t.Error("want error for body without five_hour/seven_day, got nil")
	}
}

func TestParseUsageBadJSON(t *testing.T) {
	if _, err := parseUsage([]byte(`not json`)); err == nil {
		t.Error("want error for invalid JSON, got nil")
	}
}
