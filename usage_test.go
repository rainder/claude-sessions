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
	want := &AccountUsage{
		Account: "andy@work.com",
		Info: &UsageInfo{
			FiveHour:          usageBucket{Pct: 85, ResetsAt: time.Now().Add(time.Hour).UTC()},
			SevenDay:          usageBucket{Pct: 46, ResetsAt: time.Now().Add(48 * time.Hour).UTC()},
			WeeklyScoped:      usageBucket{Pct: 10, ResetsAt: time.Now().Add(72 * time.Hour).UTC()},
			WeeklyScopedLabel: "Fable",
			Credits:           creditsInfo{Enabled: true, Used: 2550, Limit: 100000, Currency: "USD", DecimalPlaces: 2},
		},
	}
	saveUsageCache(want)
	got := loadUsageCache()
	if got == nil {
		t.Fatal("loadUsageCache after save = nil")
	}
	if got.Account != "andy@work.com" {
		t.Errorf("round-trip account = %q, want andy@work.com", got.Account)
	}
	if got.Info == nil || got.Info.FiveHour.Pct != 85 || got.Info.SevenDay.Pct != 46 || !got.Info.Credits.Enabled || got.Info.Credits.Used != 2550 {
		t.Errorf("round-trip mismatch: %+v", got.Info)
	}
	if got.Info.WeeklyScopedLabel != "Fable" || got.Info.WeeklyScoped.Pct != 10 {
		t.Errorf("scoped weekly round-trip mismatch: %+v/%q", got.Info.WeeklyScoped, got.Info.WeeklyScopedLabel)
	}
}

// A pre-relogin cache stored the bare snapshot under "info" (no account). The
// new envelope keys it under "usage"; the old file must decode to a miss (nil),
// not an error or a bogus empty-account snapshot.
func TestUsageCacheOldEnvelopeMigratesToMiss(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	old, _ := json.Marshal(map[string]any{
		"fetched_at": time.Now(),
		"info": map[string]any{
			"FiveHour": map[string]any{"Pct": 85},
			"SevenDay": map[string]any{"Pct": 46},
		},
	})
	if err := os.WriteFile(usageCachePath(), old, 0600); err != nil {
		t.Fatal(err)
	}
	if got := loadUsageCache(); got != nil {
		t.Errorf("old-format cache should decode to a miss, got %+v", got)
	}
}

func TestUsageCacheExpiry(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	stale, _ := json.Marshal(cachedUsage{
		FetchedAt: time.Now().Add(-usageCacheMaxAge - time.Minute),
		Usage:     AccountUsage{Account: "a@b.c", Info: &UsageInfo{FiveHour: usageBucket{Pct: 85}}},
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

// A new-envelope cache whose nested snapshot is null (only the account written)
// is a miss too, so the poller waits for a live fetch rather than seeding an
// info-less bar.
func TestUsageCacheNilInfoIsMiss(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	nilInfo, _ := json.Marshal(cachedUsage{
		FetchedAt: time.Now(),
		Usage:     AccountUsage{Account: "a@b.c", Info: nil},
	})
	if err := os.WriteFile(usageCachePath(), nilInfo, 0600); err != nil {
		t.Fatal(err)
	}
	if got := loadUsageCache(); got != nil {
		t.Errorf("nil-Info cache should be a miss, got %+v", got)
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

// parseOAuthCredentials is shared by the live token read (loadOAuthToken) and
// every claude-switch snapshot read (snapshotToken), so both reject the same
// shapes.
func TestParseOAuthCredentials(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		want    string
		wantErr bool
	}{
		{"valid", `{"claudeAiOauth":{"accessToken":"sk-ant-oat01-abc"}}`, "sk-ant-oat01-abc", false},
		{"extra fields ignored", `{"claudeAiOauth":{"accessToken":"tok","refreshToken":"r","expiresAt":1},"other":true}`, "tok", false},
		{"empty token", `{"claudeAiOauth":{"accessToken":""}}`, "", true},
		{"missing oauth object", `{}`, "", true},
		{"malformed json", `not json`, "", true},
		{"empty body", ``, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseOAuthCredentials([]byte(tc.body))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("err = nil, want an error (token = %q)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if got != tc.want {
				t.Fatalf("token = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		header string
		want   time.Time
	}{
		{"delta-seconds", "90", now.Add(90 * time.Second)},
		{"delta-seconds with surrounding space", "  30 ", now.Add(30 * time.Second)},
		// Both are "retry immediately", which is no opinion at all — the caller's
		// own schedule decides, exactly as if the header had been absent.
		{"zero seconds", "0", time.Time{}},
		{"a negative delta", "-5", time.Time{}},
		{"an RFC 1123 date", "Sun, 02 Aug 2026 12:05:00 GMT", now.Add(5 * time.Minute)},
		{"an ANSI C date", "Sun Aug  2 12:05:00 2026", now.Add(5 * time.Minute)},
		{"absent", "", time.Time{}},
		{"garbage", "soon-ish", time.Time{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseRetryAfter(c.header, now)
			if !got.Equal(c.want) {
				t.Fatalf("parseRetryAfter(%q) = %v, want %v", c.header, got, c.want)
			}
		})
	}
}

func TestUsageBackoffUntil(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		streak  int
		retryAt time.Time
		want    time.Duration // wait from now
	}{
		{"the first throttle costs nothing of its own", 1, time.Time{}, 0},
		// An explicit Retry-After is the endpoint stating a wait, and it is honoured
		// from the first 429 — the schedule only declines to invent one.
		{"but a first throttle still honours an explicit Retry-After", 1, now.Add(3 * time.Minute), 3 * time.Minute},
		{"a zero streak is the same", 0, time.Time{}, 0},
		{"the second consecutive one waits", 2, time.Time{}, usageBackoffSecond},
		{"the third caps the schedule", 3, time.Time{}, usageBackoffMax},
		{"and every one after it", 9, time.Time{}, usageBackoffMax},
		// The endpoint's own opinion may only lengthen the wait: the schedule
		// exists because that opinion is usually absent or optimistic.
		{"a longer Retry-After wins", 2, now.Add(6 * time.Minute), 6 * time.Minute},
		{"a shorter one does not", 3, now.Add(time.Minute), usageBackoffMax},
		{"a past one does not", 2, now.Add(-time.Hour), usageBackoffSecond},
		// A bogus header must not be able to park an account for the life of the
		// process.
		{"an absurd Retry-After is capped", 2, now.Add(9 * time.Hour), usageBackoffCeiling},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := usageBackoffUntil(now, c.streak, c.retryAt)
			if want := now.Add(c.want); !got.Equal(want) {
				t.Fatalf("usageBackoffUntil(streak %d) = +%v, want +%v", c.streak, got.Sub(now), c.want)
			}
		})
	}
}
