package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	fetchedAt := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
	want := &AccountUsage{
		Account:   "andy@work.com",
		FetchedAt: fetchedAt,
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
	// The snapshot's own timestamp has to survive the file, or a warm-start seed
	// reads as unstamped and liveCarryable refuses to carry it at all.
	if !got.FetchedAt.Equal(fetchedAt) {
		t.Errorf("round-trip FetchedAt = %v, want %v", got.FetchedAt, fetchedAt)
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
		// A date whose wait has already elapsed is the same "no opinion" as a
		// non-positive delta, and normalizes to the same zero time here rather than
		// leaving a stale instant for a caller to re-check.
		{"a date already in the past", "Sun, 02 Aug 2026 11:55:00 GMT", time.Time{}},
		{"a date of exactly now", "Sun, 02 Aug 2026 12:00:00 GMT", time.Time{}},
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

// liveUsageFixture is one live-account reading, distinct enough that a test can
// tell a re-served snapshot from a freshly fetched one by its numbers.
// FetchedAt is stamped because a reading with none is deliberately not
// carryable (liveCarryable's zero check), so an unstamped fixture would make
// every re-serve fall through to the identity placeholder and quietly gut the
// tests below.
func liveUsageFixture(pct float64) *AccountUsage {
	return liveUsageFixtureAged(pct, 0)
}

// liveUsageFixtureAged dates a reading, for the bound liveCarryable puts on how
// long a carry may go on.
func liveUsageFixtureAged(pct float64, age time.Duration) *AccountUsage {
	return &AccountUsage{
		Account:   "andy@trecs.aero",
		Info:      &UsageInfo{FiveHour: usageBucket{Pct: pct}},
		FetchedAt: time.Now().Add(-age),
	}
}

// carriedFrom asserts a backed-off pass re-served want: the same numbers under
// the same account, marked stale so they cannot pass as live, keeping want's
// ORIGINAL timestamp — and as a copy, so the fetcher's own memory of want stays
// unmarked and keeps aging.
func carriedFrom(t *testing.T, u *AccountUsage, want *AccountUsage) {
	t.Helper()
	if u == nil {
		t.Fatal("backed-off pass = nil, want the last good numbers carried forward")
	}
	if u == want {
		t.Fatal("re-serve returned the stored pointer; only a copy keeps last unmarked and its timestamp honest")
	}
	if u.Info != want.Info || u.Account != want.Account {
		t.Fatalf("re-served = %#v, want %#v's numbers", u, want)
	}
	if !u.Stale {
		t.Error("carried-forward numbers must be marked stale, or they pass as live")
	}
	if !u.FetchedAt.Equal(want.FetchedAt) {
		t.Errorf("re-serve restamped FetchedAt (%v, want %v): a permanently throttled account would look permanently fresh", u.FetchedAt, want.FetchedAt)
	}
	if want.Stale {
		t.Error("the stored reading itself got marked stale; the flag belongs only to the copy")
	}
}

// placeholded asserts a backed-off pass with nothing safe to re-serve answered
// with identity alone. It must be a SUCCESS: an error is what engages
// usagePoller.run's 5s-doubling retry, the request burst the wait exists to
// prevent.
func placeholded(t *testing.T, u *AccountUsage, err error, email string) {
	t.Helper()
	if err != nil {
		t.Fatalf("backed-off pass = %v, want a success — an error engages the generic retry burst", err)
	}
	if u == nil {
		t.Fatal("backed-off pass = nil, want an identity placeholder")
	}
	if u.Info != nil {
		t.Fatalf("placeholder carries numbers (%+v); nothing safe to re-serve means nothing shown", u.Info)
	}
	if u.Account != email {
		t.Errorf("placeholder account = %q, want %q", u.Account, email)
	}
}

// loginAs points loadAccountEmail at a temp HOME logged into email, since
// newUsageFetcher re-serves a reading only while that account is still the live
// one. Without it these tests would read the developer's own ~/.claude.json and
// pass or fail on whose account that names.
func loginAs(t *testing.T, email string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeLiveAccount(t, home, email)
}

// writeLiveAccount rewrites the temp HOME's ~/.claude.json, standing in for a
// relogin or a Ctrl+W switch mid-run.
func writeLiveAccount(t *testing.T, home, email string) {
	t.Helper()
	body := fmt.Sprintf(`{"oauthAccount":{"emailAddress":%q}}`, email)
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

// The armed wait is 4 minutes, so nothing here can wait one out; the ordering
// itself is the proof, exactly as in the known-accounts equivalent. Two
// consecutive throttles are what arm the wait, so a pass that still fetches
// after an intervening success is the evidence that success cleared the streak.
func TestUsageFetcherBacksOffRepeatedThrottles(t *testing.T) {
	loginAs(t, "andy@trecs.aero")
	calls := 0
	throttled := true
	good := liveUsageFixture(21)
	fetcher := newUsageFetcher(nil, func() (*AccountUsage, error) {
		calls++
		if throttled {
			return nil, &usageHTTPError{Status: 429}
		}
		return good, nil
	})

	// A first throttle imposes no wait at all — most heal within one tick.
	if _, err := fetcher(); err == nil {
		t.Fatal("pass 1 = nil error, want the throttle reported")
	}
	if calls != 1 {
		t.Fatalf("fetches = %d, want the first pass to fetch", calls)
	}
	// A success ends the streak before it can build.
	throttled = false
	u, err := fetcher()
	if err != nil || u != good {
		t.Fatalf("pass 2 = (%#v, %v), want the fresh numbers", u, err)
	}
	if calls != 2 {
		t.Fatalf("fetches = %d, want the pass after one throttle to fetch", calls)
	}

	// Now two consecutive throttles. The second is what arms the wait — which
	// also proves the success above cleared the streak, since otherwise this
	// second pass would already have been skipped.
	throttled = true
	for i := 3; i <= 4; i++ {
		if _, err := fetcher(); err == nil {
			t.Fatalf("pass %d = nil error, want the throttle reported", i)
		}
	}
	if calls != 4 {
		t.Fatalf("fetches = %d, want both throttled passes to have fetched", calls)
	}

	// Inside the armed wait the endpoint is not touched, and the skip reads as an
	// ordinary success carrying the last good numbers: an error here would engage
	// usagePoller.run's generic 5s-doubling retry, which is the burst this whole
	// mechanism exists to prevent.
	u, err = fetcher()
	if calls != 4 {
		t.Fatalf("fetches = %d, want the pass inside the backoff to skip the endpoint", calls)
	}
	if err != nil {
		t.Fatalf("backed-off pass = %v, want the last good reading re-served as a success", err)
	}
	carriedFrom(t, u, good)
}

// Only a throttle is answered with a wait. Every other failure is a transient
// the generic retry handles correctly, so it must leave the account immediately
// eligible.
func TestUsageFetcherBacksOffOnlyOnThrottles(t *testing.T) {
	loginAs(t, "andy@trecs.aero")
	calls := 0
	fetcher := newUsageFetcher(liveUsageFixture(21), func() (*AccountUsage, error) {
		calls++
		return nil, &usageHTTPError{Status: 503}
	})
	for i := 1; i <= 3; i++ {
		if _, err := fetcher(); err == nil {
			t.Fatalf("pass %d = nil error, want the failure reported", i)
		}
	}
	if calls != 3 {
		t.Fatalf("fetches = %d, want every non-throttled failure to have fetched", calls)
	}
}

// A cold start against an already-throttled account is the case this whole
// mechanism exists for, and the one it used to miss entirely: with no seed there
// was nothing to re-serve, so every backed-off pass fell through to a real fetch
// and the 429 came back as an error — which usagePoller.run answers with its own
// 5s-doubling retry, i.e. the wait was armed and bought nothing at all.
//
// Now the wait holds regardless of whether there is anything to show. The pass
// reports identity only, as a success.
func TestUsageFetcherColdStartStopsFetchingOnceBackedOff(t *testing.T) {
	loginAs(t, "andy@trecs.aero")
	calls := 0
	fetcher := newUsageFetcher(nil, func() (*AccountUsage, error) {
		calls++
		return nil, &usageHTTPError{Status: 429}
	})
	// The first throttle imposes no wait; the second is what arms it. Both are
	// real attempts, and both report the error they actually got.
	for i := 1; i <= 2; i++ {
		if _, err := fetcher(); err == nil {
			t.Fatalf("pass %d = nil error, want the throttle reported", i)
		}
	}
	if calls != 2 {
		t.Fatalf("fetches = %d, want both throttled passes to have fetched", calls)
	}
	u, err := fetcher()
	if calls != 2 {
		t.Fatalf("fetches = %d, want the pass inside the backoff to skip the endpoint even with nothing to re-serve", calls)
	}
	placeholded(t, u, err, "andy@trecs.aero")
}

// Identity unconfirmable — an unreadable ~/.claude.json — used to fall through
// to a real fetch, on the grounds that showing an account's bars means knowing
// whose they are. The placeholder is strictly better: it shows no numbers, so
// there is nothing to misattribute, and it costs no request against an endpoint
// that just said stop.
func TestUsageFetcherPlaceholdsWhenIdentityIsUnconfirmable(t *testing.T) {
	loginAs(t, "andy@trecs.aero")
	calls := 0
	good := liveUsageFixture(37)
	fetcher := newUsageFetcher(good, func() (*AccountUsage, error) {
		calls++
		return nil, &usageHTTPError{Status: 429}
	})
	for i := 1; i <= 2; i++ {
		if _, err := fetcher(); err == nil {
			t.Fatalf("pass %d = nil error, want the throttle reported", i)
		}
	}
	// Whose numbers these are can no longer be established.
	if err := os.Remove(filepath.Join(os.Getenv("HOME"), ".claude.json")); err != nil {
		t.Fatal(err)
	}
	u, err := fetcher()
	if calls != 2 {
		t.Fatalf("fetches = %d, want an unconfirmable identity to hold the wait, not spend a request", calls)
	}
	placeholded(t, u, err, "")
}

// The carry is bounded by the age of the numbers themselves, the same bound a
// warm start from disk obeys. Past it the reading is dropped rather than shown —
// and the wait still stands, since numbers going stale is no reason to override
// an endpoint that is throttling.
func TestUsageFetcherWillNotCarryNumbersPastTheAgeBound(t *testing.T) {
	loginAs(t, "andy@trecs.aero")
	calls := 0
	old := liveUsageFixtureAged(37, usageCacheMaxAge+time.Minute)
	fetcher := newUsageFetcher(old, func() (*AccountUsage, error) {
		calls++
		return nil, &usageHTTPError{Status: 429}
	})
	for i := 1; i <= 2; i++ {
		if _, err := fetcher(); err == nil {
			t.Fatalf("pass %d = nil error, want the throttle reported", i)
		}
	}
	u, err := fetcher()
	if calls != 2 {
		t.Fatalf("fetches = %d, want an expired carry to hold the wait rather than force a fetch", calls)
	}
	placeholded(t, u, err, "andy@trecs.aero")
}

// A carry must not consume itself: re-serving hands out a copy, so the stored
// reading keeps its own timestamp and its unmarked state and can be carried
// again on the next pass. Marking the stored copy in place would make the second
// re-serve report numbers already flagged stale as its own fresh memory, and
// would leak a stale reading into the disk cache through saveOnceUsage.
func TestUsageFetcherReservesRepeatedlyWithoutMarkingItsOwnCopy(t *testing.T) {
	loginAs(t, "andy@trecs.aero")
	calls := 0
	good := liveUsageFixture(37)
	fetcher := newUsageFetcher(good, func() (*AccountUsage, error) {
		calls++
		return nil, &usageHTTPError{Status: 429}
	})
	for i := 1; i <= 2; i++ {
		if _, err := fetcher(); err == nil {
			t.Fatalf("pass %d = nil error, want the throttle reported", i)
		}
	}
	first, err := fetcher()
	if err != nil {
		t.Fatalf("first re-serve = %v, want a success", err)
	}
	carriedFrom(t, first, good)
	second, err := fetcher()
	if err != nil {
		t.Fatalf("second re-serve = %v, want a success", err)
	}
	carriedFrom(t, second, good)
	if calls != 2 {
		t.Fatalf("fetches = %d, want every re-serve to skip the endpoint", calls)
	}
	if first == second {
		t.Error("both re-serves handed out one shared copy; each pass must own its own")
	}
}

// A restart landing mid-throttle re-serves the disk-cached snapshot, which is
// what the seed is for.
func TestUsageFetcherReservesTheSeed(t *testing.T) {
	loginAs(t, "andy@trecs.aero")
	calls := 0
	seed := liveUsageFixture(37)
	fetcher := newUsageFetcher(seed, func() (*AccountUsage, error) {
		calls++
		return nil, &usageHTTPError{Status: 429}
	})
	for i := 1; i <= 2; i++ {
		if _, err := fetcher(); err == nil {
			t.Fatalf("pass %d = nil error, want the throttle reported", i)
		}
	}
	u, err := fetcher()
	if calls != 2 {
		t.Fatalf("fetches = %d, want the pass inside the backoff to skip the endpoint", calls)
	}
	if err != nil {
		t.Fatalf("backed-off pass = %v, want the seeded snapshot re-served as a success", err)
	}
	carriedFrom(t, u, seed)
}

// last is keyed by nothing but "whoever was live last time", so a switch during
// an armed wait must end the re-serving: bars carried under the wrong email
// misattribute usage across accounts, which is the failure carryable exists to
// prevent for snapshot accounts.
func TestUsageFetcherDropsAnotherAccountsNumbers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeLiveAccount(t, home, "andy@trecs.aero")

	calls := 0
	seed := liveUsageFixture(37)
	fetcher := newUsageFetcher(seed, func() (*AccountUsage, error) {
		calls++
		return nil, &usageHTTPError{Status: 429}
	})
	for i := 1; i <= 2; i++ {
		if _, err := fetcher(); err == nil {
			t.Fatalf("pass %d = nil error, want the throttle reported", i)
		}
	}
	u, err := fetcher()
	if calls != 2 || err != nil {
		t.Fatalf("backed-off pass = %v after %d fetches, want the seed re-served", err, calls)
	}
	carriedFrom(t, u, seed)

	// Ctrl+W, or a relogin: the numbers on hand belong to the account that just
	// left, and the armed wait was earned against its budget, not this one's.
	writeLiveAccount(t, home, "andy@avisoma.com")
	if _, err := fetcher(); err == nil {
		t.Fatal("pass after the switch = nil error, want a real fetch's throttle")
	}
	if calls != 3 {
		t.Fatalf("fetches = %d, want the switch to have forced a fetch", calls)
	}
	// The streak must have restarted with the account, not been inherited from
	// it. That throttle was the new account's FIRST, and a first 429 imposes no
	// wait at all, so the very next pass is due again. Without the reset the
	// streak would have carried on at 3 and armed the 8-minute wait instead —
	// and since a nil last no longer falls through to a fetch, that pass would
	// answer with the placeholder rather than asking the endpoint. Dropping last
	// alone cannot produce this; only clearing backoff can.
	if _, err := fetcher(); err == nil {
		t.Fatal("pass after the switch's throttle = nil error, want a real fetch")
	}
	if calls != 4 {
		t.Fatalf("fetches = %d, want the new account's first throttle to impose no wait", calls)
	}
}

// The poller saves on every success, and a re-served snapshot is a success — so
// without this wrapper a long throttle would restamp FetchedAt every two
// minutes and usageCacheMaxAge would stop bounding a warm start.
func TestSaveOnceUsageSkipsAReservedSnapshot(t *testing.T) {
	seed := liveUsageFixture(37)
	fresh := liveUsageFixture(21)
	var saved []*AccountUsage
	save := saveOnceUsage(seed, func(u *AccountUsage) { saved = append(saved, u) })

	save(seed) // re-served warm-start seed: already on disk
	if len(saved) != 0 {
		t.Fatalf("saves = %d, want the seeded snapshot left alone", len(saved))
	}
	save(fresh)
	save(fresh) // re-served after the backoff armed
	save(fresh)
	if len(saved) != 1 || saved[0] != fresh {
		t.Fatalf("saves = %#v, want one write for the one real fetch", saved)
	}
	newer := liveUsageFixture(44)
	save(newer)
	if len(saved) != 2 || saved[1] != newer {
		t.Fatalf("saves = %#v, want the next real fetch written", saved)
	}
}

// Pointer identity alone stopped recognising a re-serve once newUsageFetcher
// began handing out a fresh copy per pass, and a backed-off pass with nothing to
// carry hands out a fresh placeholder besides. Both are new pointers, so both
// would reach the disk cache: the copy restamping the envelope every two minutes
// with numbers that never moved (the exact failure this wrapper exists to
// prevent), the placeholder overwriting a good cache file with a nil-Info entry
// loadUsageCache then reads as a miss — destroying the warm start rather than
// merely staling it.
func TestSaveOnceUsageSkipsCarriedAndEmptySnapshots(t *testing.T) {
	fresh := liveUsageFixture(21)
	var saved []*AccountUsage
	save := saveOnceUsage(nil, func(u *AccountUsage) { saved = append(saved, u) })

	save(fresh)
	if len(saved) != 1 {
		t.Fatalf("saves = %d, want the real fetch written", len(saved))
	}

	carried := *fresh
	carried.Stale = true
	save(&carried)

	save(&AccountUsage{Account: "andy@trecs.aero"}) // backed off with nothing to carry

	if len(saved) != 1 {
		t.Fatalf("saves = %#v, want only the one genuinely fetched reading on disk", saved)
	}
}
