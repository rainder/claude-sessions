package main

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
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
		// Pins the actual regression: a live 429 was observed asking for
		// Retry-After: 1895 (~31.6 minutes). The old 15-minute ceiling silently
		// truncated that to 15 minutes, so this account's own wait was shorter
		// than the wait the endpoint had just asked for — the endpoint's opinion
		// must be honoured in full whenever it fits under the ceiling.
		{"a real long Retry-After is honoured, not silently truncated", 2, now.Add(1895 * time.Second), 1895 * time.Second},
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

func TestUsageBackoffDueSafetyMargin(t *testing.T) {
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name        string
		nextAttempt time.Time
		want        bool
	}{
		{"nextAttempt exactly now is not due — the margin must still be met", now, false},
		{"nextAttempt just under the margin is not due", now.Add(-59 * time.Second), false},
		{"nextAttempt exactly one margin in the past is due", now.Add(-time.Minute), true},
		{"nextAttempt well past the margin is due", now.Add(-time.Hour), true},
		{"a streak-1 'free' wait (nextAttempt == the arming instant) is not due before the margin elapses", now, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := usageBackoff{streak: 1, nextAttempt: c.nextAttempt}
			if got := b.due(now); got != c.want {
				t.Fatalf("due() = %v, want %v (nextAttempt %v, now %v)", got, c.want, c.nextAttempt, now)
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

// liveUsageFixtureAged dates a reading, for tests that need to show carrying
// numbers works at any age, not just a fresh one.
func liveUsageFixtureAged(pct float64, age time.Duration) *AccountUsage {
	return &AccountUsage{
		Account:   "andy@trecs.aero",
		Info:      &UsageInfo{FiveHour: usageBucket{Pct: pct}},
		FetchedAt: time.Now().Add(-age),
	}
}

// carriedFrom asserts a backed-off pass re-served want: the same numbers
// under the same account, marked stale so they cannot pass as live, keeping
// want's ORIGINAL timestamp. Info is compared by value, not pointer identity:
// newUsageFetcher now reloads every account's entry fresh from disk on every
// pass (see its doc comment), so a genuinely carried reading round-trips
// through JSON and is never the same *UsageInfo pointer the caller started
// with — pointer equality would fail even when the numbers are byte-identical.
func carriedFrom(t *testing.T, u *AccountUsage, want *AccountUsage) {
	t.Helper()
	if u == nil {
		t.Fatal("backed-off pass = nil, want the last good numbers carried forward")
	}
	if u.Account != want.Account || !reflect.DeepEqual(u.Info, want.Info) {
		t.Fatalf("re-served = %#v, want %#v's numbers", u, want)
	}
	if !u.Stale {
		t.Error("carried-forward numbers must be marked stale, or they pass as live")
	}
	if !u.FetchedAt.Equal(want.FetchedAt) {
		t.Errorf("re-serve restamped FetchedAt (%v, want %v): a permanently throttled account would look permanently fresh", u.FetchedAt, want.FetchedAt)
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

// loginAsWithSnapshot sets up a temp HOME logged into email AND holding a
// claude-switch snapshot under name for that same email, so
// resolveActiveSnapshotName can find a slot to persist into. Without a
// matching snapshot newUsageFetcher has nowhere to read or write anything
// (see its doc comment's "no matching snapshot at all" paragraph), so every
// test below that exercises cross-pass persistence needs this instead of the
// bare loginAs.
func loginAsWithSnapshot(t *testing.T, name, email string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeLiveAccount(t, home, email)
	writeSnapshotFixture(t, home, name, "tok-"+name, email)
	return home
}

// fakeClock points usageClockNow — shared by newUsageFetcher and
// newKnownAccountsFetcher — at a controllable instant, so a test can
// simulate elapsed wall-clock time (past the backoff safety margin, or past
// the coalesce window) without a real sleep. The returned pointer IS the
// fetcher's "now" for every call until advanced with clock.Add; t.Cleanup
// restores the real clock so later tests in the same run are unaffected.
func fakeClock(t *testing.T, start time.Time) *time.Time {
	t.Helper()
	now := start
	orig := usageClockNow
	usageClockNow = func() time.Time { return now }
	t.Cleanup(func() { usageClockNow = orig })
	return &now
}

func TestFakeClockControlsUsageClockNow(t *testing.T) {
	start := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	clock := fakeClock(t, start)
	if got := usageClockNow(); !got.Equal(start) {
		t.Fatalf("usageClockNow() = %v, want %v", got, start)
	}
	*clock = clock.Add(90 * time.Second)
	if got := usageClockNow(); !got.Equal(start.Add(90 * time.Second)) {
		t.Fatalf("usageClockNow() after advance = %v, want %v", got, start.Add(90*time.Second))
	}
}

// The armed wait is 4 minutes, so nothing here can wait one out; the ordering
// itself is the proof, exactly as in the known-accounts equivalent. Two
// consecutive throttles are what arm the wait, so a pass that still fetches
// after an intervening success is the evidence that success cleared the
// streak. Real persistence (saveAccountCache), not a no-op: this fetcher
// keeps no in-memory state of its own any more, so cross-pass continuity
// requires the disk round-trip via the account's own resolved snapshot slot.
func TestUsageFetcherBacksOffRepeatedThrottles(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	loginAsWithSnapshot(t, "trecs", "andy@trecs.aero")
	calls := 0
	throttled := true
	good := liveUsageFixture(21)
	fetcher := newUsageFetcher(func() (*AccountUsage, error) {
		calls++
		if throttled {
			return nil, &usageHTTPError{Status: 429}
		}
		return good, nil
	}, saveAccountCache)

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
// eligible. No snapshot needed: a non-throttle failure always clears the
// streak back to zero, so due() stays true regardless of whether there is
// anywhere to persist it.
func TestUsageFetcherBacksOffOnlyOnThrottles(t *testing.T) {
	loginAs(t, "andy@trecs.aero")
	calls := 0
	fetcher := newUsageFetcher(func() (*AccountUsage, error) {
		calls++
		return nil, &usageHTTPError{Status: 503}
	}, noSaveAccountCache)
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
// mechanism exists for, and the one it used to miss entirely: with nothing on
// disk yet there was nothing to re-serve, so every backed-off pass fell
// through to a real fetch and the 429 came back as an error — which
// usagePoller.run answers with its own 5s-doubling retry, i.e. the wait was
// armed and bought nothing at all.
//
// Now the wait holds regardless of whether there is anything to show. The pass
// reports identity only, as a success.
func TestUsageFetcherColdStartStopsFetchingOnceBackedOff(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	loginAsWithSnapshot(t, "trecs", "andy@trecs.aero")
	calls := 0
	fetcher := newUsageFetcher(func() (*AccountUsage, error) {
		calls++
		return nil, &usageHTTPError{Status: 429}
	}, saveAccountCache)
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

// Identity unconfirmable — an unreadable ~/.claude.json — never fetches,
// unconditionally, regardless of whatever might otherwise be on disk: there
// is no email to resolve a slot from (see newUsageFetcher's doc comment), so
// there is nothing to even check. The placeholder shows no numbers, so there
// is nothing to misattribute, and it costs no request.
func TestUsageFetcherPlaceholdsWhenIdentityIsUnconfirmable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("HOME", home)
	writeLiveAccount(t, home, "andy@trecs.aero")
	writeSnapshotFixture(t, home, "trecs", "tok-trecs", "andy@trecs.aero")

	calls := 0
	fetcher := newUsageFetcher(func() (*AccountUsage, error) {
		calls++
		return nil, &usageHTTPError{Status: 429}
	}, saveAccountCache)
	for i := 1; i <= 2; i++ {
		if _, err := fetcher(); err == nil {
			t.Fatalf("pass %d = nil error, want the throttle reported", i)
		}
	}
	// Whose numbers these are can no longer be established.
	if err := os.Remove(filepath.Join(home, ".claude.json")); err != nil {
		t.Fatal(err)
	}
	u, err := fetcher()
	if calls != 2 {
		t.Fatalf("fetches = %d, want an unconfirmable identity to hold the wait, not spend a request", calls)
	}
	placeholded(t, u, err, "")
}

// The carry has no age bound: numbers from hours into an outage still beat a
// bare "rate limited" placeholder, since the Stale marker already tells the
// header (and localFreshAccountEmails) not to trust them as current.
func TestUsageFetcherCarriesNumbersRegardlessOfAge(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	loginAsWithSnapshot(t, "trecs", "andy@trecs.aero")
	old := liveUsageFixtureAged(37, 6*time.Hour)
	saveAccountCache("trecs", accountCacheEntry{Account: old.Account, Info: old.Info, FetchedAt: old.FetchedAt})
	calls := 0
	fetcher := newUsageFetcher(func() (*AccountUsage, error) {
		calls++
		return nil, &usageHTTPError{Status: 429}
	}, saveAccountCache)
	for i := 1; i <= 2; i++ {
		if _, err := fetcher(); err == nil {
			t.Fatalf("pass %d = nil error, want the throttle reported", i)
		}
	}
	u, err := fetcher()
	if calls != 2 {
		t.Fatalf("fetches = %d, want a still-carryable reading to hold the wait rather than force a fetch", calls)
	}
	if err != nil {
		t.Fatalf("backed-off pass = %v, want a success", err)
	}
	carriedFrom(t, u, old)
}

// A carry must not consume itself: re-serving hands out a copy, so a second
// pass carries the identical entry again rather than something already
// marked stale from the first re-serve.
func TestUsageFetcherReservesRepeatedlyWithoutMarkingItsOwnCopy(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	loginAsWithSnapshot(t, "trecs", "andy@trecs.aero")
	good := liveUsageFixture(37)
	saveAccountCache("trecs", accountCacheEntry{Account: good.Account, Info: good.Info, FetchedAt: good.FetchedAt})
	calls := 0
	fetcher := newUsageFetcher(func() (*AccountUsage, error) {
		calls++
		return nil, &usageHTTPError{Status: 429}
	}, saveAccountCache)
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

// A restart landing mid-throttle re-serves whatever is on disk for the
// account's resolved slot — no separate seed argument needed any more, since
// every pass reloads that slot from disk regardless of whether it is the
// very first call.
func TestUsageFetcherReservesAPersistedEntry(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	loginAsWithSnapshot(t, "trecs", "andy@trecs.aero")
	seed := liveUsageFixture(37)
	saveAccountCache("trecs", accountCacheEntry{Account: seed.Account, Info: seed.Info, FetchedAt: seed.FetchedAt})
	calls := 0
	fetcher := newUsageFetcher(func() (*AccountUsage, error) {
		calls++
		return nil, &usageHTTPError{Status: 429}
	}, saveAccountCache)
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
		t.Fatalf("backed-off pass = %v, want the persisted entry re-served as a success", err)
	}
	carriedFrom(t, u, seed)
}

// The whole point: a fresh disk entry (written moments ago, by this process
// or another) means a pass that would otherwise fetch skips the network
// call entirely and re-serves those numbers as fresh, not stale.
func TestUsageFetcherCoalescesARecentReading(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	loginAsWithSnapshot(t, "trecs", "andy@trecs.aero")
	clock := fakeClock(t, time.Now())
	good := liveUsageFixture(37)
	// Stamp from the fetcher's own (frozen) clock, not real time: liveUsageFixture
	// stamps FetchedAt with a fresh time.Now() call, which lands a few
	// nanoseconds AFTER the instant fakeClock froze — real clocks always
	// advance between two sequential time.Now() calls. Left as real time, that
	// makes last.FetchedAt.After(now) true and the coalescing branch's future-
	// timestamp guard (deliberately strict — see its comment) refuses to fire,
	// failing this test for a reason that has nothing to do with coalescing
	// itself. Backdating to the frozen clock is what "no advance" (this test's
	// whole premise) actually means.
	good.FetchedAt = *clock
	saveAccountCache("trecs", accountCacheEntry{Account: good.Account, Info: good.Info, FetchedAt: good.FetchedAt})
	calls := 0
	fetcher := newUsageFetcher(func() (*AccountUsage, error) {
		calls++
		return nil, &usageHTTPError{Status: 429}
	}, noSaveAccountCache)

	u, err := fetcher()
	if calls != 0 {
		t.Fatalf("fetches = %d, want a fresh disk entry to be coalesced instead of fetched", calls)
	}
	if err != nil {
		t.Fatalf("coalesced pass = %v, want a success", err)
	}
	if u == nil || u.Stale {
		t.Fatalf("coalesced result = %#v, want fresh (non-stale) numbers", u)
	}
	if u.Account != good.Account || !reflect.DeepEqual(u.Info, good.Info) {
		t.Fatalf("coalesced numbers = %#v, want %#v", u, good)
	}
	if !u.FetchedAt.Equal(good.FetchedAt) {
		t.Errorf("coalesced pass restamped FetchedAt (%v, want %v)", u.FetchedAt, good.FetchedAt)
	}
}

// Past the coalesce window, a disk entry is old enough that this pass must
// fetch for real — coalescing must not suppress ordinary polling forever.
func TestUsageFetcherFetchesPastTheCoalesceWindow(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	loginAsWithSnapshot(t, "trecs", "andy@trecs.aero")
	good := liveUsageFixture(37)
	saveAccountCache("trecs", accountCacheEntry{Account: good.Account, Info: good.Info, FetchedAt: good.FetchedAt})
	clock := fakeClock(t, time.Now())
	*clock = clock.Add(usageCoalesceWindow + time.Second)
	fresh := liveUsageFixture(55)
	calls := 0
	fetcher := newUsageFetcher(func() (*AccountUsage, error) {
		calls++
		return fresh, nil
	}, noSaveAccountCache)

	u, err := fetcher()
	if calls != 1 {
		t.Fatalf("fetches = %d, want an entry past the coalesce window to trigger a real fetch", calls)
	}
	if err != nil || u != fresh {
		t.Fatalf("pass = (%#v, %v), want the fresh fetch's own numbers", u, err)
	}
}

// A lone process must never coalesce against its own prior fetch at its next
// natural tick — usageCoalesceWindow is half of usageRefreshInterval
// specifically so this can't happen. Regression pin for the self-throttle
// bug an independent review caught in the first version of this design.
func TestUsageFetcherDoesNotSelfCoalesceAtItsOwnNextTick(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	loginAsWithSnapshot(t, "trecs", "andy@trecs.aero")
	clock := fakeClock(t, time.Now())
	first := liveUsageFixture(21)
	calls := 0
	fetcher := newUsageFetcher(func() (*AccountUsage, error) {
		calls++
		if calls == 1 {
			return first, nil
		}
		return liveUsageFixture(99), nil
	}, saveAccountCache)

	if _, err := fetcher(); err != nil {
		t.Fatalf("pass 1 = %v, want a success", err)
	}
	if calls != 1 {
		t.Fatalf("fetches = %d, want the first pass to fetch", calls)
	}
	*clock = clock.Add(usageRefreshInterval)
	if _, err := fetcher(); err != nil {
		t.Fatalf("pass 2 (one full interval later) = %v, want a success", err)
	}
	if calls != 2 {
		t.Fatalf("fetches = %d, want a lone process's own next natural tick to fetch for real, not coalesce against itself", calls)
	}
}

// An identity mismatch must never be coalesced onto — the same rule that
// already governs the carry-forward path. Reuses liveCarryable, so this is
// really pinning that reuse rather than new logic.
func TestUsageFetcherDoesNotCoalesceAMismatchedEntry(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	loginAsWithSnapshot(t, "trecs", "andy@trecs.aero")
	saveAccountCache("trecs", accountCacheEntry{
		Account:   "someone@else.example",
		Info:      &UsageInfo{FiveHour: usageBucket{Pct: 99}},
		FetchedAt: time.Now(),
	})
	fresh := liveUsageFixture(21)
	calls := 0
	fetcher := newUsageFetcher(func() (*AccountUsage, error) {
		calls++
		return fresh, nil
	}, noSaveAccountCache)
	u, err := fetcher()
	if calls != 1 {
		t.Fatalf("fetches = %d, want a mismatched entry to be ignored, forcing a real fetch", calls)
	}
	if err != nil || u != fresh {
		t.Fatalf("pass = (%#v, %v), want the fresh fetch's own numbers", u, err)
	}
}

// A switch during an armed wait must end the re-serving: bars carried under
// the wrong email misattribute usage across accounts, which is the failure
// carryable exists to prevent for snapshot accounts. Both accounts need their
// own snapshot fixture — the incoming one has to resolve to its own slot for
// the switch to even register as a name change.
func TestUsageFetcherDropsAnotherAccountsNumbers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TMPDIR", t.TempDir())
	writeLiveAccount(t, home, "andy@trecs.aero")
	writeSnapshotFixture(t, home, "trecs", "tok-trecs", "andy@trecs.aero")
	writeSnapshotFixture(t, home, "avisoma", "tok-avisoma", "andy@avisoma.com")

	calls := 0
	seed := liveUsageFixture(37)
	saveAccountCache("trecs", accountCacheEntry{Account: seed.Account, Info: seed.Info, FetchedAt: seed.FetchedAt})
	fetcher := newUsageFetcher(func() (*AccountUsage, error) {
		calls++
		return nil, &usageHTTPError{Status: 429}
	}, saveAccountCache)
	for i := 1; i <= 2; i++ {
		if _, err := fetcher(); err == nil {
			t.Fatalf("pass %d = nil error, want the throttle reported", i)
		}
	}
	u, err := fetcher()
	if calls != 2 || err != nil {
		t.Fatalf("backed-off pass = %v after %d fetches, want the persisted entry re-served", err, calls)
	}
	carriedFrom(t, u, seed)

	// Ctrl+W, or a relogin: this pass now resolves to avisoma's own slot, which
	// has nothing armed against it yet.
	writeLiveAccount(t, home, "andy@avisoma.com")
	if _, err := fetcher(); err == nil {
		t.Fatal("pass after the switch = nil error, want a real fetch's throttle")
	}
	if calls != 3 {
		t.Fatalf("fetches = %d, want the switch to have forced a fetch", calls)
	}
	// avisoma's first throttle imposes no wait at all, so the very next pass is
	// due again — proof this is genuinely a fresh slot, not trecs's streak
	// carried over under a new name.
	if _, err := fetcher(); err == nil {
		t.Fatal("pass after the switch's throttle = nil error, want a real fetch")
	}
	if calls != 4 {
		t.Fatalf("fetches = %d, want the new account's first throttle to impose no wait", calls)
	}
}

// liveCarryable's identity check is the last line of defense once storage is
// keyed by snapshot name rather than email: a stale or corrupted entry whose
// Account field doesn't match who is actually live must never be carried,
// even though resolveActiveSnapshotName already matched on email to find
// this slot in the first place. Defense in depth — the same reasoning
// liveCarryable's own doc comment gives.
// A loaded entry whose Account doesn't match who's live must not be trusted
// for EITHER half — not just its numbers (the original identity gate), but
// its backoff too. An independent review caught an earlier version of this
// fix only gating the numbers: the armed wait from a mismatched entry (e.g.
// a claude-switch snapshot name reassigned to a different account via
// `account save --force`) still held the NEW account back from ever being
// fetched, even though it was never itself throttled. The correct outcome
// is a real fetch, not a placeholder that just waits out someone else's wait.
func TestUsageFetcherDropsCarryWhenCachedAccountDoesNotMatchLive(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	loginAsWithSnapshot(t, "trecs", "andy@trecs.aero")
	saveAccountCache("trecs", accountCacheEntry{
		Account:            "someone@else.example",
		Info:               &UsageInfo{FiveHour: usageBucket{Pct: 99}},
		FetchedAt:          time.Now(),
		BackoffStreak:      3,
		BackoffNextAttempt: time.Now().Add(10 * time.Minute),
	})
	good := liveUsageFixture(21)
	calls := 0
	fetcher := newUsageFetcher(func() (*AccountUsage, error) {
		calls++
		return good, nil
	}, noSaveAccountCache)
	u, err := fetcher()
	if calls != 1 {
		t.Fatalf("fetches = %d, want a mismatched entry's armed wait discarded along with its numbers — a real fetch, not a wait for someone else's throttle", calls)
	}
	if err != nil || u != good {
		t.Fatalf("pass = (%#v, %v), want the fresh numbers", u, err)
	}
}

// The actual point of persisting the backoff: a process restarted mid-wait
// must not fire a real request before the persisted deadline passes. Seeded
// via a direct saveAccountCache write, exactly what a prior process
// instance's own save call would have left on disk — numbers and backoff
// together, as one entry.
func TestUsageFetcherHonorsSeededBackoffAcrossRestart(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	loginAsWithSnapshot(t, "trecs", "andy@trecs.aero")
	good := liveUsageFixture(21)
	saveAccountCache("trecs", accountCacheEntry{
		Account:            good.Account,
		Info:               good.Info,
		FetchedAt:          good.FetchedAt,
		BackoffStreak:      3,
		BackoffNextAttempt: time.Now().Add(10 * time.Minute),
	})
	calls := 0
	fetcher := newUsageFetcher(func() (*AccountUsage, error) {
		calls++
		return good, nil
	}, noSaveAccountCache)
	u, err := fetcher()
	if calls != 0 {
		t.Fatalf("fetches = %d, want a persisted future deadline to withhold the very first call", calls)
	}
	if err != nil {
		t.Fatalf("seeded-backoff pass = %v, want a success re-serving the seed", err)
	}
	carriedFrom(t, u, good)
}

// A persisted deadline that has already elapsed — the wait ran out while the
// process was down — must not hold anything back.
func TestUsageFetcherFetchesWhenSeededBackoffHasElapsed(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	loginAsWithSnapshot(t, "trecs", "andy@trecs.aero")
	good := liveUsageFixture(21)
	saveAccountCache("trecs", accountCacheEntry{
		BackoffStreak:      3,
		BackoffNextAttempt: time.Now().Add(-time.Minute),
	})
	calls := 0
	fetcher := newUsageFetcher(func() (*AccountUsage, error) {
		calls++
		return good, nil
	}, noSaveAccountCache)
	u, err := fetcher()
	if calls != 1 {
		t.Fatalf("fetches = %d, want an already-elapsed persisted deadline to allow a real fetch", calls)
	}
	if err != nil || u != good {
		t.Fatalf("pass = (%#v, %v), want the fresh numbers", u, err)
	}
}

// save is called at the transitions that change the wait, so the persisted
// deadline tracks the in-memory one closely enough that a restart between
// passes sees an accurate picture: armed on the first throttle (streak 1,
// still due immediately, but a restart before the second throttle should
// still know one already happened), cleared on the success that ends the
// streak.
func TestUsageFetcherPersistsBackoffTransitions(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	loginAsWithSnapshot(t, "trecs", "andy@trecs.aero")
	type saveCall struct {
		name   string
		streak int
	}
	var saved []saveCall
	save := func(name string, e accountCacheEntry) {
		saved = append(saved, saveCall{name, e.BackoffStreak})
	}
	throttled := true
	good := liveUsageFixture(21)
	fetcher := newUsageFetcher(func() (*AccountUsage, error) {
		if throttled {
			return nil, &usageHTTPError{Status: 429}
		}
		return good, nil
	}, save)

	if _, err := fetcher(); err == nil {
		t.Fatal("pass 1 = nil error, want the throttle reported")
	}
	if len(saved) != 1 || saved[0].name != "trecs" || saved[0].streak != 1 {
		t.Fatalf("saved after first throttle = %+v, want [{trecs 1}]", saved)
	}
	throttled = false
	if _, err := fetcher(); err != nil {
		t.Fatalf("pass 2 = %v, want a success", err)
	}
	if len(saved) != 2 || saved[1].streak != 0 {
		t.Fatalf("saved after success = %+v, want the wait cleared", saved)
	}
}

// A live account that has never been `account save`d has no snapshot to
// resolve a disk slot from, and independent review caught an earlier version
// of the per-account rewrite silently losing backoff protection for exactly
// this case: with no in-memory state at all, every pass loaded nothing,
// treated the account as never-throttled, and fetched for real — every
// single tick, forever, never building past streak 1. This is the direct A/B
// regression test that caught it (six fetches on the broken version against
// a permanently-429ing endpoint, matching main's own two).
func TestUsageFetcherBacksOffEvenWithNoMatchingSnapshot(t *testing.T) {
	loginAs(t, "andy@trecs.aero") // no snapshot for trecs — deliberately
	calls := 0
	fetcher := newUsageFetcher(func() (*AccountUsage, error) {
		calls++
		return nil, &usageHTTPError{Status: 429}
	}, noSaveAccountCache)

	// First throttle imposes no wait; the second arms it.
	for i := 1; i <= 2; i++ {
		if _, err := fetcher(); err == nil {
			t.Fatalf("pass %d = nil error, want the throttle reported", i)
		}
	}
	if calls != 2 {
		t.Fatalf("fetches = %d, want both throttled passes to have fetched", calls)
	}
	if _, err := fetcher(); err != nil {
		t.Fatalf("backed-off pass = %v, want a success (in-memory fallback holds the wait)", err)
	}
	if calls != 2 {
		t.Fatalf("fetches = %d, want the pass inside the armed wait to skip the endpoint even with no snapshot to persist into", calls)
	}
}

// A disk pass for one snapshotted account must never clobber a *different*,
// still-unsnapshotted account's own fallback wait. Independent review caught
// this as a variant of the no-matching-snapshot bug above: the fallback vars
// are shared across every account this fetcher is ever asked about, and an
// earlier version mirrored disk outcomes into them unconditionally — so
// switching away to a snapshotted account and back lost the unsnapshotted
// account's armed streak, even though nothing about that account's own state
// ever changed.
func TestUsageFetcherFallbackSurvivesAnUnrelatedDiskPass(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TMPDIR", t.TempDir())
	// avisoma has no snapshot (fallback path); trecs has one (disk path).
	writeLiveAccount(t, home, "andy@avisoma.com")
	writeSnapshotFixture(t, home, "trecs", "tok-trecs", "andy@trecs.aero")

	calls := 0
	fetcher := newUsageFetcher(func() (*AccountUsage, error) {
		calls++
		if calls <= 2 {
			return nil, &usageHTTPError{Status: 429}
		}
		return &AccountUsage{Account: "andy@trecs.aero", Info: &UsageInfo{FiveHour: usageBucket{Pct: 55}}, FetchedAt: time.Now()}, nil
	}, noSaveAccountCache)

	// Arm avisoma's fallback streak to 2 (first throttle imposes no wait, the
	// second does).
	for i := 1; i <= 2; i++ {
		if _, err := fetcher(); err == nil {
			t.Fatalf("avisoma pass %d = nil error, want the throttle reported", i)
		}
	}
	if calls != 2 {
		t.Fatalf("fetches = %d, want both avisoma passes to have fetched", calls)
	}

	// Switch to trecs, which resolves to the disk path and has nothing
	// cached — this used to unconditionally overwrite the shared fallback
	// vars with trecs's own (empty) state.
	writeLiveAccount(t, home, "andy@trecs.aero")
	if _, err := fetcher(); err != nil {
		t.Fatalf("trecs pass = %v, want a fresh fetch to succeed", err)
	}
	if calls != 3 {
		t.Fatalf("fetches = %d, want the trecs pass to have fetched", calls)
	}

	// Switch back to avisoma: its wait (armed two passes ago) must still be
	// honored, not reset by the intervening trecs pass.
	writeLiveAccount(t, home, "andy@avisoma.com")
	if _, err := fetcher(); err != nil {
		t.Fatalf("avisoma pass after unrelated trecs pass = %v, want a success (fallback wait still armed)", err)
	}
	if calls != 3 {
		t.Fatalf("fetches = %d, want avisoma's armed wait to still hold after an unrelated account's disk pass", calls)
	}
}

// The unconfirmable-identity variant of the same clobber: a wait armed while
// live == "" was itself written into fbArmedFor as "", indistinguishable
// from "untracked" — so a later resolved account's disk pass (whose own
// mirror guard treats fbArmedFor == "" as safe to overwrite) could still
// erase it. Independent review caught this as a residual gap in the fix
// above. Fixed with the fbUnconfirmedOwner sentinel: an unconfirmable
// identity's armed wait is recorded under a value that can never equal a
// real email, so it reads as "tracked" (not "untracked") to every guard
// that checks fbArmedFor == "".
func TestUsageFetcherUnconfirmedIdentityWaitSurvivesALaterResolvedAccount(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TMPDIR", t.TempDir())
	writeSnapshotFixture(t, home, "trecs", "tok-trecs", "andy@trecs.aero")
	// No ~/.claude.json yet: identity starts unconfirmable.

	calls := 0
	fetcher := newUsageFetcher(func() (*AccountUsage, error) {
		calls++
		if calls <= 2 {
			return nil, &usageHTTPError{Status: 429}
		}
		return &AccountUsage{Account: "andy@trecs.aero", Info: &UsageInfo{FiveHour: usageBucket{Pct: 55}}, FetchedAt: time.Now()}, nil
	}, noSaveAccountCache)

	// Arm the ownerless wait to streak 2.
	for i := 1; i <= 2; i++ {
		if _, err := fetcher(); err == nil {
			t.Fatalf("unconfirmed pass %d = nil error, want the throttle reported", i)
		}
	}
	if calls != 2 {
		t.Fatalf("fetches = %d, want both unconfirmed passes to have fetched", calls)
	}

	// Identity becomes readable and resolves to a snapshotted account: the
	// disk path engages and, on the old unguarded write, would have mirrored
	// its own state over the ownerless wait (both looked like "" before the
	// sentinel).
	writeLiveAccount(t, home, "andy@trecs.aero")
	if _, err := fetcher(); err != nil {
		t.Fatalf("trecs pass = %v, want a fresh fetch to succeed", err)
	}
	if calls != 3 {
		t.Fatalf("fetches = %d, want the trecs pass to have fetched", calls)
	}

	// Identity becomes unconfirmable again: the ownerless wait armed at the
	// start must still be honored.
	if err := os.Remove(filepath.Join(home, ".claude.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := fetcher(); err != nil {
		t.Fatalf("unconfirmed pass after trecs = %v, want a success (ownerless wait still armed)", err)
	}
	if calls != 3 {
		t.Fatalf("fetches = %d, want the ownerless wait to still hold after an unrelated resolved account's disk pass", calls)
	}
}

// An unconfirmable identity with NOTHING armed must still attempt a fetch:
// the file read isn't the only route to identity (fetchVerifiedUsageInfo's
// profile probe can attribute numbers to a verified account the file alone
// couldn't confirm — see usageAccountLabel), so refusing unconditionally
// would permanently blank the header the instant ~/.claude.json becomes
// unreadable. Only an already-armed wait should refuse — independent review
// caught an earlier version of this refusing regardless.
func TestUsageFetcherFetchesWithUnconfirmableIdentityWhenNothingArmed(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // no ~/.claude.json at all
	good := liveUsageFixture(21)
	calls := 0
	fetcher := newUsageFetcher(func() (*AccountUsage, error) {
		calls++
		return good, nil
	}, noSaveAccountCache)
	u, err := fetcher()
	if calls != 1 {
		t.Fatalf("fetches = %d, want an unconfirmable identity with nothing armed to still attempt a fetch", calls)
	}
	if err != nil || u != good {
		t.Fatalf("pass = (%#v, %v), want the fresh numbers", u, err)
	}
}

// Verified must survive a carried/failed pass rather than being silently
// recomputed to false: UsageInfo.VerifiedAccount is json:"-", so anything
// derived from a JSON-round-tripped Info is always empty — independent
// review caught persist() deriving Verified from u.Info.VerifiedAccount even
// on the carry-forward path, where u came from disk, not a fresh fetch.
func TestUsageFetcherPreservesVerifiedAcrossACarry(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	loginAsWithSnapshot(t, "trecs", "andy@trecs.aero")
	good := liveUsageFixture(21)
	var saved accountCacheEntry
	save := func(name string, e accountCacheEntry) { saved = e }
	saveAccountCache("trecs", accountCacheEntry{
		Account: good.Account, Info: good.Info, FetchedAt: good.FetchedAt, Verified: true,
	})
	fetcher := newUsageFetcher(func() (*AccountUsage, error) {
		return nil, &usageHTTPError{Status: 503} // non-throttle failure: due() stays true, carries last
	}, save)
	if _, err := fetcher(); err == nil {
		t.Fatal("pass = nil error, want the failure reported")
	}
	if !saved.Verified {
		t.Errorf("persisted entry after a carried failure = %+v, want Verified preserved from the loaded entry", saved)
	}
}

// TestVerifiedUsageInfoBindsIdentityToTheToken covers the composition fix 2
// rests on: the profile probe rides the same token as the numbers, it only runs
// once the numbers actually arrived, and a probe that fails costs nothing but
// the upgrade.
func TestVerifiedUsageInfoBindsIdentityToTheToken(t *testing.T) {
	t.Run("verified email lands on the reading", func(t *testing.T) {
		var probed string
		info, err := verifiedUsageInfo("tok-live",
			func(string) (*UsageInfo, error) { return &UsageInfo{FiveHour: usageBucket{Pct: 12}}, nil },
			func(tok string) (string, error) { probed = tok; return "andy@trecs.aero", nil })
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if probed != "tok-live" {
			t.Fatalf("probed %q, want the same token the numbers were fetched with", probed)
		}
		if info.VerifiedAccount != "andy@trecs.aero" {
			t.Fatalf("VerifiedAccount = %q", info.VerifiedAccount)
		}
	})

	t.Run("a failed probe is not a failed fetch", func(t *testing.T) {
		info, err := verifiedUsageInfo("tok-live",
			func(string) (*UsageInfo, error) { return &UsageInfo{FiveHour: usageBucket{Pct: 12}}, nil },
			func(string) (string, error) { return "", &usageHTTPError{Status: 429} })
		if err != nil {
			t.Fatalf("err = %v, want a probe failure to be non-fatal", err)
		}
		if info.VerifiedAccount != "" {
			t.Fatalf("VerifiedAccount = %q, want it unset", info.VerifiedAccount)
		}
	})

	t.Run("a failed usage fetch never pays for a probe", func(t *testing.T) {
		_, err := verifiedUsageInfo("tok-live",
			func(string) (*UsageInfo, error) { return nil, &usageHTTPError{Status: 429} },
			func(string) (string, error) { t.Fatal("probed after a failed usage fetch"); return "", nil })
		if err == nil {
			t.Fatal("err = nil, want the usage failure")
		}
	})
}

// TestUsageAccountLabel pins the attribution rule: proof beats the file, and a
// disagreement is reported as the verified account rather than smoothed over —
// that visible, unexpected address in the header is how a clobbered credential
// announces itself.
func TestUsageAccountLabel(t *testing.T) {
	tests := []struct {
		name string
		file string
		info *UsageInfo
		want string
	}{
		{name: "no reading at all falls back to the file", file: "andy@avisoma.com", want: "andy@avisoma.com"},
		{name: "unverified reading falls back to the file", file: "andy@avisoma.com",
			info: &UsageInfo{}, want: "andy@avisoma.com"},
		{name: "verified reading wins", file: "andy@avisoma.com",
			info: &UsageInfo{VerifiedAccount: "andy@avisoma.com"}, want: "andy@avisoma.com"},
		{name: "a clobber is labelled with the truth", file: "andy@avisoma.com",
			info: &UsageInfo{VerifiedAccount: "andy@trecs.aero"}, want: "andy@trecs.aero"},
		{name: "verified identity survives an unreadable file",
			info: &UsageInfo{VerifiedAccount: "andy@trecs.aero"}, want: "andy@trecs.aero"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := usageAccountLabel(tt.file, tt.info); got != tt.want {
				t.Fatalf("= %q, want %q", got, tt.want)
			}
		})
	}
}

// TestVerifiedIdentityMismatch pins the other half: a disagreement needs both
// sides to be known, and it is case-insensitive like every other email
// comparison in this package.
func TestVerifiedIdentityMismatch(t *testing.T) {
	tests := []struct {
		name    string
		claimed string
		info    *UsageInfo
		want    bool
	}{
		{name: "agreement", claimed: "andy@trecs.aero", info: &UsageInfo{VerifiedAccount: "andy@trecs.aero"}},
		{name: "case-insensitive agreement", claimed: "Andy@Trecs.Aero", info: &UsageInfo{VerifiedAccount: "andy@trecs.aero"}},
		{name: "disagreement", claimed: "andy@trecs.aero",
			info: &UsageInfo{VerifiedAccount: "andy@avisoma.com"}, want: true},
		{name: "nothing claimed, nothing to contradict", info: &UsageInfo{VerifiedAccount: "andy@avisoma.com"}},
		{name: "nothing verified, nothing proven", claimed: "andy@trecs.aero", info: &UsageInfo{}},
		{name: "no reading", claimed: "andy@trecs.aero"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := verifiedIdentityMismatch(tt.claimed, tt.info); got != tt.want {
				t.Fatalf("= %v, want %v", got, tt.want)
			}
		})
	}
}

// TestParseOAuthCredentialBlob covers the fields fix 3's validation reads. An
// absent expiry decodes to the zero time rather than to 1970, which is what
// keeps a snapshot written before those fields existed from reading as expired.
func TestParseOAuthCredentialBlob(t *testing.T) {
	creds, err := parseOAuthCredentialBlob([]byte(
		`{"claudeAiOauth":{"accessToken":"a","refreshToken":"r","expiresAt":1785942392167}}`))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if creds.AccessToken != "a" || creds.RefreshToken != "r" {
		t.Fatalf("creds = %+v", creds)
	}
	if got := msExpiry(creds.ExpiresAt); got.UnixMilli() != 1785942392167 {
		t.Fatalf("expiresAt = %v", got)
	}
	if got := msExpiry(creds.RefreshTokenExpiresAt); !got.IsZero() {
		t.Fatalf("absent refreshTokenExpiresAt = %v, want the zero time", got)
	}
}

// TestProfileEmailCache covers finding 4's whole point: verification must not
// double this process's request volume forever. The answer is a property of the
// token, so one probe per distinct token is enough — and a rotated token is a
// different key, which is why no TTL is needed to stay correct.
func TestProfileEmailCache(t *testing.T) {
	t.Run("one probe per token", func(t *testing.T) {
		calls := 0
		stubProfileEmail(t, func(string) (string, error) {
			calls++
			return "andy@trecs.aero", nil
		})
		for i := 0; i < 4; i++ {
			got, err := profileEmails.emailFor("tok-a")
			if err != nil || got != "andy@trecs.aero" {
				t.Fatalf("emailFor = (%q, %v)", got, err)
			}
		}
		if calls != 1 {
			t.Fatalf("probes = %d, want exactly one", calls)
		}
	})

	t.Run("a rotated token re-probes", func(t *testing.T) {
		calls := 0
		stubProfileEmail(t, func(tok string) (string, error) {
			calls++
			return tok + "@example.com", nil
		})
		if _, err := profileEmails.emailFor("tok-old"); err != nil {
			t.Fatal(err)
		}
		got, err := profileEmails.emailFor("tok-new")
		if err != nil {
			t.Fatal(err)
		}
		if got != "tok-new@example.com" {
			t.Fatalf("emailFor = %q, want the new token's own answer", got)
		}
		if calls != 2 {
			t.Fatalf("probes = %d, want one per distinct token", calls)
		}
	})

	t.Run("a failure is never cached", func(t *testing.T) {
		calls := 0
		stubProfileEmail(t, func(string) (string, error) {
			calls++
			if calls == 1 {
				return "", &usageHTTPError{Status: 429}
			}
			return "andy@trecs.aero", nil
		})
		if _, err := profileEmails.emailFor("tok-a"); err == nil {
			t.Fatal("err = nil, want the throttle")
		}
		got, err := profileEmails.emailFor("tok-a")
		if err != nil || got != "andy@trecs.aero" {
			t.Fatalf("retry = (%q, %v), want the probe to have run again", got, err)
		}
	})

	t.Run("the map stays bounded", func(t *testing.T) {
		stubProfileEmail(t, func(tok string) (string, error) { return tok + "@example.com", nil })
		for i := 0; i < profileEmailCacheMax*2; i++ {
			if _, err := profileEmails.emailFor(fmt.Sprintf("tok-%d", i)); err != nil {
				t.Fatal(err)
			}
		}
		profileEmails.mu.Lock()
		n := len(profileEmails.byKey)
		profileEmails.mu.Unlock()
		if n > profileEmailCacheMax {
			t.Fatalf("entries = %d, want at most %d", n, profileEmailCacheMax)
		}
	})
}
