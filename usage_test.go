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

func TestParseUsageScopedWeekly(t *testing.T) {
	body := []byte(`{
		"five_hour": {"utilization": 9.0,  "resets_at": "2026-06-10T15:19:59+00:00"},
		"seven_day": {"utilization": 13.0, "resets_at": "2026-06-10T18:00:00+00:00"},
		"limits": [
			{"kind":"session","group":"session","percent":41,"severity":"normal","resets_at":"2026-07-08T20:00:00+00:00","scope":null,"is_active":true},
			{"kind":"weekly_all","group":"weekly","percent":9,"severity":"normal","resets_at":"2026-07-15T17:59:59+00:00","scope":null,"is_active":false},
			{"kind":"weekly_scoped","group":"weekly","percent":10,"severity":"normal","resets_at":"2026-07-15T17:59:59.879088+00:00","scope":{"model":{"id":null,"display_name":"Fable"},"surface":null},"is_active":false}
		]
	}`)
	u, err := parseUsage(body)
	if err != nil {
		t.Fatalf("parseUsage: %v", err)
	}
	if u.WeeklyScopedLabel != "Fable" {
		t.Errorf("WeeklyScopedLabel = %q, want Fable", u.WeeklyScopedLabel)
	}
	if u.WeeklyScoped.Pct != 10 {
		t.Errorf("WeeklyScoped.Pct = %v, want 10", u.WeeklyScoped.Pct)
	}
	wantReset := time.Date(2026, 7, 15, 17, 59, 59, 879088000, time.UTC)
	if !u.WeeklyScoped.ResetsAt.Equal(wantReset) {
		t.Errorf("WeeklyScoped.ResetsAt = %v, want %v", u.WeeklyScoped.ResetsAt, wantReset)
	}
}

func TestParseUsageNoScopedWeekly(t *testing.T) {
	// No limits array at all, and a limits array with no weekly_scoped entry,
	// both leave the scoped bucket empty without erroring.
	bodies := [][]byte{
		[]byte(`{
			"five_hour": {"utilization": 9.0,  "resets_at": "2026-06-10T15:19:59+00:00"},
			"seven_day": {"utilization": 13.0, "resets_at": "2026-06-10T18:00:00+00:00"}
		}`),
		[]byte(`{
			"five_hour": {"utilization": 9.0,  "resets_at": "2026-06-10T15:19:59+00:00"},
			"seven_day": {"utilization": 13.0, "resets_at": "2026-06-10T18:00:00+00:00"},
			"limits": [
				{"kind":"weekly_all","group":"weekly","percent":9,"resets_at":"2026-07-15T17:59:59+00:00","scope":null,"is_active":false}
			]
		}`),
	}
	for i, body := range bodies {
		u, err := parseUsage(body)
		if err != nil {
			t.Fatalf("case %d parseUsage: %v", i, err)
		}
		if u.WeeklyScopedLabel != "" || u.WeeklyScoped.Pct != 0 {
			t.Errorf("case %d: WeeklyScoped = %+v/%q, want empty", i, u.WeeklyScoped, u.WeeklyScopedLabel)
		}
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
		FiveHour:          usageBucket{Pct: 85, ResetsAt: time.Now().Add(time.Hour).UTC()},
		SevenDay:          usageBucket{Pct: 46, ResetsAt: time.Now().Add(48 * time.Hour).UTC()},
		WeeklyScoped:      usageBucket{Pct: 10, ResetsAt: time.Now().Add(72 * time.Hour).UTC()},
		WeeklyScopedLabel: "Fable",
		Credits:           creditsInfo{Enabled: true, Used: 2550, Limit: 100000, Currency: "USD", DecimalPlaces: 2},
	}
	saveUsageCache(want)
	got := loadUsageCache()
	if got == nil {
		t.Fatal("loadUsageCache after save = nil")
	}
	if got.FiveHour.Pct != 85 || got.SevenDay.Pct != 46 || !got.Credits.Enabled || got.Credits.Used != 2550 {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	if got.WeeklyScopedLabel != "Fable" || got.WeeklyScoped.Pct != 10 {
		t.Errorf("scoped weekly round-trip mismatch: %+v/%q", got.WeeklyScoped, got.WeeklyScopedLabel)
	}
}

// A cache written before the scoped-weekly fields existed must still load,
// degrading to no scoped bar rather than erroring.
func TestUsageCacheOldFormatNoScoped(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	old, _ := json.Marshal(cachedUsage{
		FetchedAt: time.Now(),
		Info: UsageInfo{
			FiveHour: usageBucket{Pct: 85, ResetsAt: time.Now().Add(time.Hour).UTC()},
			SevenDay: usageBucket{Pct: 46, ResetsAt: time.Now().Add(48 * time.Hour).UTC()},
		},
	})
	if err := os.WriteFile(usageCachePath(), old, 0600); err != nil {
		t.Fatal(err)
	}
	got := loadUsageCache()
	if got == nil {
		t.Fatal("loadUsageCache of pre-scoped cache = nil")
	}
	if got.WeeklyScopedLabel != "" || got.WeeklyScoped.Pct != 0 {
		t.Errorf("scoped fields = %+v/%q, want empty", got.WeeklyScoped, got.WeeklyScopedLabel)
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
