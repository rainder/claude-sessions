package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// noSaveAccountCache is the save arg for tests that don't exercise disk
// persistence — the real saveAccountCache writes to os.TempDir(), and several
// tests in this file set HOME via loginAs without also setting TMPDIR, so
// passing the real function here would write to the developer's actual temp
// directory.
func noSaveAccountCache(string, accountCacheEntry) {}

// writeSnapshotFixture creates one claude-switch credential snapshot (and, when
// email is non-empty, its account.json sibling) under home. It goes through
// snapshotCredentialPath so the fixture matches whichever platform the test
// runs on — hardcoding ".keychain-cred" would break every Linux build.
func writeSnapshotFixture(t *testing.T, home, name, token, email string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0755); err != nil {
		t.Fatal(err)
	}
	// credBlob, not a bare access token: switchAccount validates that a snapshot
	// it is about to install can still refresh, and a fixture without a refresh
	// token would fail every switch test for a reason none of them is about.
	// Sharing the helper also keeps a snapshot byte-identical to what a switch
	// installs, which is what makes "the live credential is now that snapshot" a
	// plain string comparison.
	if err := os.WriteFile(snapshotCredentialPath(home, name), []byte(credBlob(token)), 0600); err != nil {
		t.Fatal(err)
	}
	if email == "" {
		return
	}
	account := fmt.Sprintf(`{"oauthAccount":{"emailAddress":%q}}`, email)
	if err := os.WriteFile(snapshotAccountPath(home, name), []byte(account), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotAccountNames(t *testing.T) {
	t.Run("no snapshots is an empty list, not an error", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		if err := os.MkdirAll(filepath.Join(home, ".claude"), 0755); err != nil {
			t.Fatal(err)
		}
		names, err := snapshotAccountNames()
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if len(names) != 0 {
			t.Fatalf("names = %v, want none", names)
		}
	})

	t.Run("missing ~/.claude is an empty list, not an error", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		names, err := snapshotAccountNames()
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if len(names) != 0 {
			t.Fatalf("names = %v, want none", names)
		}
	})

	t.Run("snapshots are listed, the live credential is not", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		writeSnapshotFixture(t, home, "avisoma", "tok-a", "andy@avisoma.com")
		writeSnapshotFixture(t, home, "trecs", "tok-t", "andy@trecs.aero")
		// Both platforms' bare live credential names, so whichever suffix this
		// platform uses has the file it could conceivably collide with sitting
		// right there.
		for _, live := range []string{".credentials.json", ".keychain-cred"} {
			if err := os.WriteFile(filepath.Join(home, ".claude", live), []byte(`{"claudeAiOauth":{"accessToken":"live"}}`), 0600); err != nil {
				t.Fatal(err)
			}
		}
		names, err := snapshotAccountNames()
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"avisoma", "trecs"} // os.ReadDir order is lexical
		if len(names) != len(want) {
			t.Fatalf("names = %v, want %v", names, want)
		}
		for i := range want {
			if names[i] != want[i] {
				t.Fatalf("names = %v, want %v", names, want)
			}
		}
	})

	t.Run("the rescue slot is not an account", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		writeSnapshotFixture(t, home, "avisoma", "tok-a", "andy@avisoma.com")
		// switchAccount (and claude-switch before it) parks the outgoing
		// credential here. It has the file-name shape of a snapshot but nobody is
		// ever logged into it, so it must never reach the poller, `account list`,
		// or the Ctrl+W picker.
		writeSnapshotFixture(t, home, rescueSnapshotName, "tok-rescued", "")
		names, err := snapshotAccountNames()
		if err != nil {
			t.Fatal(err)
		}
		if len(names) != 1 || names[0] != "avisoma" {
			t.Fatalf("names = %v, want just [avisoma]", names)
		}
	})

	t.Run("a home path with glob metacharacters is data, not a pattern", func(t *testing.T) {
		parent := t.TempDir()
		// A sibling of both homes below, holding a snapshot of its own: a "*"
		// home treated as a pattern matches across into it and hands back
		// another directory's credential snapshots.
		writeSnapshotFixture(t, filepath.Join(parent, "an[dy"), "leaked", "tok-l", "andy@leaked.dev")

		// "br[oken" is an unmatched bracket — the one thing filepath.Glob calls an
		// error, which would be this poller's only error path and would take every
		// account down with it; "an*dy" is the cross-directory match. Errorf, not
		// Fatalf, so one failing case never hides the other.
		for _, dir := range []string{"br[oken", "an*dy"} {
			home := filepath.Join(parent, dir)
			t.Setenv("HOME", home)
			writeSnapshotFixture(t, home, "own", "tok-o", "andy@own.dev")
			names, err := snapshotAccountNames()
			if err != nil {
				t.Errorf("home %q: err = %v, want nil", home, err)
				continue
			}
			if len(names) != 1 || names[0] != "own" {
				t.Errorf("home %q: names = %v, want just its own snapshot", home, names)
			}
		}
	})
}

func TestSnapshotTokenAndEmail(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeSnapshotFixture(t, home, "avisoma", "tok-a", "andy@avisoma.com")
	// A snapshot whose account.json never landed: token readable, email unknown.
	writeSnapshotFixture(t, home, "nameless", "tok-n", "")

	tok, err := snapshotToken("avisoma")
	if err != nil {
		t.Fatalf("snapshotToken err = %v", err)
	}
	if tok != "tok-a" {
		t.Fatalf("token = %q, want tok-a", tok)
	}
	if got := snapshotAccountEmail("avisoma"); got != "andy@avisoma.com" {
		t.Fatalf("email = %q, want andy@avisoma.com", got)
	}
	if got := snapshotAccountEmail("nameless"); got != "" {
		t.Fatalf("email of a snapshot with no account.json = %q, want empty", got)
	}
	if _, err := snapshotToken("missing"); err == nil {
		t.Fatal("snapshotToken of an absent snapshot returned no error")
	}
}

func TestKnownAccountsFetcherWithNoSnapshots(t *testing.T) {
	// A fresh machine with no claude-switch setup: an empty list, never an error
	// — an error here would put the poller into backoff over nothing.
	t.Setenv("HOME", t.TempDir())
	got, err := newKnownAccountsFetcher(noSaveAccountCache)()
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if got == nil || len(got.Accounts) != 0 {
		t.Fatalf("accounts = %#v, want an empty list", got)
	}
	if got.ActiveName != "" {
		t.Fatalf("active snapshot = %q, want empty with no snapshots", got.ActiveName)
	}
}

// swapFetch points the poller's HTTP leg at a stub for the duration of one
// test, restoring it afterwards. Nothing here may reach the real endpoint.
func swapFetch(t *testing.T, fetch func(string) (*UsageInfo, error)) {
	t.Helper()
	prev := usageInfoFetch
	usageInfoFetch = fetch
	t.Cleanup(func() { usageInfoFetch = prev })
}

// stubUsageFetch returns a fetch func that answers per token: a nil entry means
// that token fails with a genuine auth rejection (the typed error, not a
// look-alike string — classifyUsageErr reads the status, and a plain error
// would classify as merely unreachable).
func stubUsageFetch(byToken map[string]*UsageInfo) func(string) (*UsageInfo, error) {
	return stubUsageFetchErr(byToken, &usageHTTPError{Status: 401})
}

// stubUsageFetchErr is stubUsageFetch with the failure mode chosen, so a test
// can drive the transient branches (429, 5xx, network) as well as expiry.
func stubUsageFetchErr(byToken map[string]*UsageInfo, err error) func(string) (*UsageInfo, error) {
	return func(token string) (*UsageInfo, error) {
		if info, ok := byToken[token]; ok && info != nil {
			return info, nil
		}
		return nil, err
	}
}

func TestKnownAccountUsage(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeSnapshotFixture(t, home, "avisoma", "tok-a", "andy@avisoma.com")
	writeSnapshotFixture(t, home, "trecs", "tok-t", "andy@trecs.aero")
	writeSnapshotFixture(t, home, "nameless", "tok-n", "")
	// A snapshot whose credential file is unparseable: known account, dead token.
	if err := os.WriteFile(snapshotCredentialPath(home, "broken"), []byte("not json"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(snapshotAccountPath(home, "broken"), []byte(`{"oauthAccount":{"emailAddress":"andy@broken.dev"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	fetch := stubUsageFetch(map[string]*UsageInfo{
		"tok-a": {FiveHour: usageBucket{Pct: 41}},
		"tok-n": {FiveHour: usageBucket{Pct: 7}},
	})

	t.Run("skips the snapshot standing for the live account", func(t *testing.T) {
		got, skipped := knownAccountUsage("avisoma", "Andy@Avisoma.com", nil, fetch)
		if !skipped {
			t.Fatal("skipped = false, want true for the live account's own snapshot")
		}
		if got != nil {
			t.Fatalf("a skipped account must contribute no entry, got %#v", got)
		}
	})

	t.Run("populated Info on success", func(t *testing.T) {
		got, skipped := knownAccountUsage("avisoma", "andy@trecs.aero", nil, fetch)
		if skipped {
			t.Fatal("skipped = true, want false for a non-live account")
		}
		if got.Expired || got.Stale || got.Reason != "" {
			t.Fatalf("a successful fetch must be clean: %#v", got)
		}
		if got.Name != "avisoma" || got.Account != "andy@avisoma.com" {
			t.Fatalf("identity wrong: %#v", got)
		}
		if got.Info == nil || got.Info.FiveHour.Pct != 41 {
			t.Fatalf("Info = %#v, want the fetched snapshot", got.Info)
		}
		// The timestamp is what every staleness decision is made against, so a
		// success has to stamp it.
		if time.Since(got.FetchedAt) > time.Minute {
			t.Fatalf("FetchedAt = %v, want the time of this fetch", got.FetchedAt)
		}
	})

	t.Run("401 marks the account expired, keeps its identity", func(t *testing.T) {
		got, _ := knownAccountUsage("trecs", "andy@avisoma.com", nil, fetch)
		if !got.Expired || got.Info != nil {
			t.Fatalf("failed fetch = %#v, want Expired with no Info", got)
		}
		if got.Reason != usageExpiredReason {
			t.Fatalf("reason = %q, want %q", got.Reason, usageExpiredReason)
		}
		if got.Name != "trecs" || got.Account != "andy@trecs.aero" {
			t.Fatalf("identity lost on failure: %#v", got)
		}
	})

	t.Run("a 429 is not an expired credential", func(t *testing.T) {
		// The bug this whole state split exists for: every Claude Code session
		// shares the account's per-token budget, so a throttle is routine — and
		// telling the user to re-login over one is wrong.
		throttled := stubUsageFetchErr(nil, &usageHTTPError{Status: 429})
		got, _ := knownAccountUsage("trecs", "andy@avisoma.com", nil, throttled)
		if got.Expired {
			t.Fatalf("429 = %#v, want it classified as transient, not expired", got)
		}
		if got.Reason != "rate limited" || got.Info != nil {
			t.Fatalf("throttled entry = %#v, want the reason with no numbers", got)
		}
	})

	t.Run("a transient failure re-serves the previous numbers, marked stale", func(t *testing.T) {
		fetched := time.Now().Add(-3 * time.Minute)
		prev := &KnownAccountUsage{
			Name:      "trecs",
			Account:   "andy@trecs.aero",
			Info:      &UsageInfo{FiveHour: usageBucket{Pct: 63}},
			FetchedAt: fetched,
		}
		throttled := stubUsageFetchErr(nil, &usageHTTPError{Status: 429})
		got, _ := knownAccountUsage("trecs", "andy@avisoma.com", prev, throttled)
		if !got.Stale || got.Expired {
			t.Fatalf("carried-forward entry = %#v, want Stale and not Expired", got)
		}
		if got.Info == nil || got.Info.FiveHour.Pct != 63 {
			t.Fatalf("Info = %#v, want the previous numbers", got.Info)
		}
		if got.Reason != "rate limited" {
			t.Fatalf("reason = %q, want the failure that caused the carry", got.Reason)
		}
		// Restamping would let an account failing forever look forever fresh.
		if !got.FetchedAt.Equal(fetched) {
			t.Fatalf("FetchedAt = %v, want the original %v", got.FetchedAt, fetched)
		}
	})

	t.Run("a snapshot name reused for a different account never carries the old account's numbers", func(t *testing.T) {
		// The bug: prev is keyed by snapshot NAME in the fetcher's memory, not by
		// account identity. If "trecs" used to belong to a different account and
		// got re-saved onto today's andy@trecs.aero, a transient failure on the
		// very next poll must not re-serve the OLD account's numbers under the
		// NEW account's email — that is misattribution, worse than the
		// over-broad "auth expired" this mechanism replaced.
		prev := &KnownAccountUsage{
			Name:      "trecs",
			Account:   "someone-else@old-owner.example",
			Info:      &UsageInfo{FiveHour: usageBucket{Pct: 63}},
			FetchedAt: time.Now().Add(-3 * time.Minute),
		}
		throttled := stubUsageFetchErr(nil, &usageHTTPError{Status: 429})
		got, _ := knownAccountUsage("trecs", "andy@avisoma.com", prev, throttled)
		if got.Info != nil || got.Stale {
			t.Fatalf("mismatched-identity carry = %#v, want no numbers at all", got)
		}
		if got.Account != "andy@trecs.aero" {
			t.Fatalf("account = %q, want the snapshot's CURRENT email, not prev's", got.Account)
		}
		if got.Reason != "rate limited" {
			t.Fatalf("reason = %q, want the failure classification preserved", got.Reason)
		}
	})

	t.Run("numbers of any age are carried forward", func(t *testing.T) {
		prev := &KnownAccountUsage{
			Name:      "trecs",
			Account:   "andy@trecs.aero",
			Info:      &UsageInfo{FiveHour: usageBucket{Pct: 63}},
			FetchedAt: time.Now().Add(-6 * time.Hour),
		}
		throttled := stubUsageFetchErr(nil, &usageHTTPError{Status: 429})
		got, _ := knownAccountUsage("trecs", "andy@avisoma.com", prev, throttled)
		if got.Info == nil || got.Info.FiveHour.Pct != 63 || !got.Stale {
			t.Fatalf("aged entry = %#v, want prev's numbers carried and marked stale", got)
		}
		if !got.FetchedAt.Equal(prev.FetchedAt) {
			t.Fatalf("FetchedAt = %v, want prev's original %v", got.FetchedAt, prev.FetchedAt)
		}
		if got.Reason != "rate limited" {
			t.Fatalf("reason = %q, want the failure classification preserved", got.Reason)
		}
	})

	t.Run("an untimestamped previous result is never carried forward", func(t *testing.T) {
		// Numbers of unknown vintage: with no FetchedAt there is nothing to
		// judge staleness against, so they are treated as having none.
		prev := &KnownAccountUsage{Name: "trecs", Info: &UsageInfo{FiveHour: usageBucket{Pct: 63}}}
		throttled := stubUsageFetchErr(nil, &usageHTTPError{Status: 503})
		got, _ := knownAccountUsage("trecs", "andy@avisoma.com", prev, throttled)
		if got.Info != nil || got.Stale {
			t.Fatalf("untimestamped entry = %#v, want no numbers", got)
		}
		if got.Reason != "server error" {
			t.Fatalf("reason = %q, want the 5xx classification", got.Reason)
		}
	})

	t.Run("an expired credential never shows numbers beside it", func(t *testing.T) {
		// A dead token with recent numbers cached: showing bars next to "auth
		// expired" would imply the credential still works.
		prev := &KnownAccountUsage{
			Name:      "trecs",
			Info:      &UsageInfo{FiveHour: usageBucket{Pct: 63}},
			FetchedAt: time.Now(),
		}
		got, _ := knownAccountUsage("trecs", "andy@avisoma.com", prev, fetch)
		if !got.Expired || got.Info != nil || got.Stale {
			t.Fatalf("expired entry = %#v, want Expired alone", got)
		}
	})

	t.Run("unreadable credentials name the snapshot, not the network", func(t *testing.T) {
		// A credential file that won't parse is not an answer from the endpoint
		// about the token, so it is never Expired — and it is not a network
		// problem either, so "unreachable" would point the user nowhere.
		got, _ := knownAccountUsage("broken", "andy@avisoma.com", nil, fetch)
		if got.Expired || got.Info != nil {
			t.Fatalf("unparseable snapshot = %#v, want a non-expired failure with no Info", got)
		}
		if got.Reason != usageBadSnapshotReason {
			t.Fatalf("reason = %q, want %q", got.Reason, usageBadSnapshotReason)
		}
		if got.Account != "andy@broken.dev" {
			t.Fatalf("account = %q, want the email account.json still holds", got.Account)
		}
	})

	t.Run("an unknown email never matches the live account", func(t *testing.T) {
		got, skipped := knownAccountUsage("nameless", "andy@avisoma.com", nil, fetch)
		if skipped {
			t.Fatal("a snapshot with no readable email must not be skipped as live")
		}
		if got.Account != "" || got.Info == nil {
			t.Fatalf("got %#v, want an unnamed but fetched account", got)
		}
	})
}

func TestClassifyUsageErr(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		expired bool
		reason  string
	}{
		{"401 is a dead credential", &usageHTTPError{Status: 401}, true, usageExpiredReason},
		{"403 is a dead credential", &usageHTTPError{Status: 403}, true, usageExpiredReason},
		{"429 is the shared budget, not the token", &usageHTTPError{Status: 429}, false, "rate limited"},
		{"500 is theirs", &usageHTTPError{Status: 500}, false, "server error"},
		{"503 is theirs", &usageHTTPError{Status: 503}, false, "server error"},
		{"an unexpected status names itself", &usageHTTPError{Status: 418}, false, "HTTP 418"},
		{"a wrapped status still classifies", fmt.Errorf("fetching: %w", &usageHTTPError{Status: 401}), true, usageExpiredReason},
		// What fetchUsageInfo's own http.Client{Timeout: 5s} actually produces on
		// expiry — a *url.Error wrapping a context deadline. Without a net.Error
		// check this fell through to "unreachable", which reads as a network fault
		// rather than what actually happened.
		{"the HTTP client's own deadline", &url.Error{Op: "Get", URL: "https://api.anthropic.com/api/oauth/usage", Err: context.DeadlineExceeded}, false, "timed out"},
		{"anything else", errors.New("dial tcp: no route to host"), false, "unreachable"},
		{"no error at all", nil, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			expired, reason := classifyUsageErr(c.err)
			if expired != c.expired || reason != c.reason {
				t.Fatalf("classify(%v) = %v/%q, want %v/%q", c.err, expired, reason, c.expired, c.reason)
			}
		})
	}
}

// The reason tag is a fixed classification, never the underlying error's text:
// a network failure stringifies as a *url.Error carrying the whole request URL,
// which would end up in a one-line header placeholder and in the /usage JSON.
func TestClassifyUsageErrLeaksNoErrorText(t *testing.T) {
	err := &url.Error{
		Op:  "Get",
		URL: "https://api.anthropic.com/api/oauth/usage?token=hunter2",
		Err: errors.New("dial tcp 1.2.3.4:443: connect: connection refused"),
	}
	_, reason := classifyUsageErr(err)
	if reason != "unreachable" {
		t.Fatalf("reason = %q, want the fixed tag", reason)
	}
	if strings.Contains(reason, "anthropic.com") || strings.Contains(reason, "hunter2") {
		t.Fatalf("reason leaked the request URL: %q", reason)
	}
}

// Direct successor to the removed allKnownAccounts' own version, against the
// real per-account walk inside newKnownAccountsFetcher rather than the
// dead standalone helper: one dead account must not suppress its
// neighbours' healthy data.
func TestKnownAccountsFetcherNeverFailsOnOneBadAccount(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TMPDIR", t.TempDir())
	writeSnapshotFixture(t, home, "avisoma", "tok-a", "andy@avisoma.com")
	writeSnapshotFixture(t, home, "dead", "tok-dead", "andy@dead.dev")
	writeSnapshotFixture(t, home, "trecs", "tok-t", "andy@trecs.aero")
	swapFetch(t, stubUsageFetch(map[string]*UsageInfo{
		"tok-a": {FiveHour: usageBucket{Pct: 41}},
		"tok-t": {FiveHour: usageBucket{Pct: 22}},
	}))

	res, err := newKnownAccountsFetcher(noSaveAccountCache)()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Accounts) != 3 {
		t.Fatalf("accounts = %#v, want all three represented", res.Accounts)
	}
	byName := map[string]KnownAccountUsage{}
	for _, a := range res.Accounts {
		byName[a.Name] = a
	}
	if !byName["dead"].Expired || byName["dead"].Info != nil {
		t.Fatalf("dead account = %#v, want Expired with no numbers", byName["dead"])
	}
	// The whole point: one dead account suppresses nobody else's healthy data.
	if byName["avisoma"].Info == nil || byName["avisoma"].Info.FiveHour.Pct != 41 {
		t.Fatalf("avisoma lost its numbers beside a failing neighbour: %#v", byName["avisoma"])
	}
	if byName["trecs"].Info == nil || byName["trecs"].Info.FiveHour.Pct != 22 {
		t.Fatalf("trecs lost its numbers beside a failing neighbour: %#v", byName["trecs"])
	}
	if res.ActiveName != "" {
		t.Fatalf("active snapshot = %q, want empty when no snapshot is the live account", res.ActiveName)
	}
}

// The live account drops out entirely; everyone else still reports. The case
// difference proves the match is case-insensitive, like the dedupe key — and
// the skipped name comes back out as the active snapshot.
func TestKnownAccountsFetcherSkipsLiveAccountAndReportsItActive(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TMPDIR", t.TempDir())
	writeSnapshotFixture(t, home, "avisoma", "tok-a", "andy@avisoma.com")
	writeSnapshotFixture(t, home, "trecs", "tok-t", "andy@trecs.aero")
	writeLiveAccount(t, home, "Andy@Avisoma.com")
	swapFetch(t, stubUsageFetch(map[string]*UsageInfo{
		"tok-t": {FiveHour: usageBucket{Pct: 22}},
	}))

	res, err := newKnownAccountsFetcher(noSaveAccountCache)()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Accounts) != 1 || res.Accounts[0].Name != "trecs" {
		t.Fatalf("accounts = %#v, want just trecs (avisoma is live, excluded)", res.Accounts)
	}
	if res.ActiveName != "avisoma" {
		t.Fatalf("active snapshot = %q, want avisoma (the skipped name)", res.ActiveName)
	}
}

// Direct successor to the removed allKnownAccounts' own threading test: each
// account's persisted entry reaches its own fetch and nobody else's.
func TestKnownAccountsFetcherThreadsEachAccountsOwnEntry(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TMPDIR", t.TempDir())
	writeSnapshotFixture(t, home, "avisoma", "tok-a", "andy@avisoma.com")
	writeSnapshotFixture(t, home, "trecs", "tok-t", "andy@trecs.aero")
	saveAccountCache("trecs", accountCacheEntry{
		Account: "andy@trecs.aero", Info: &UsageInfo{FiveHour: usageBucket{Pct: 63}}, FetchedAt: time.Now(),
	})
	swapFetch(t, func(string) (*UsageInfo, error) { return nil, &usageHTTPError{Status: 429} })

	res, err := newKnownAccountsFetcher(noSaveAccountCache)()
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]KnownAccountUsage{}
	for _, a := range res.Accounts {
		byName[a.Name] = a
	}
	if got := byName["trecs"]; got.Info == nil || got.Info.FiveHour.Pct != 63 {
		t.Fatalf("trecs = %#v, want its own persisted 63%% carried", got)
	}
	if got := byName["avisoma"]; got.Info != nil {
		t.Fatalf("avisoma = %#v, want no numbers — it must not inherit trecs' entry", got)
	}
}

// A loaded entry whose Account doesn't match this snapshot's own currently
// claimed email (e.g. `account save --force` reassigned the name to a
// different account) must not have its backoff trusted either — the numbers
// half was already gated (knownAccountUsage's own carryable check), but the
// backoff half was extracted unconditionally until independent review caught
// it. The correct outcome is a real fetch, not a wait held over from
// whichever account previously owned this name.
func TestKnownAccountsFetcherDropsBackoffWhenCachedAccountDoesNotMatchSnapshot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TMPDIR", t.TempDir())
	writeSnapshotFixture(t, home, "avisoma", "tok-a", "andy@avisoma.com")
	saveAccountCache("avisoma", accountCacheEntry{
		Account:            "someone@else.example",
		Info:               &UsageInfo{FiveHour: usageBucket{Pct: 99}},
		FetchedAt:          time.Now(),
		BackoffStreak:      3,
		BackoffNextAttempt: time.Now().Add(10 * time.Minute),
	})
	calls := 0
	swapFetch(t, func(string) (*UsageInfo, error) {
		calls++
		return &UsageInfo{FiveHour: usageBucket{Pct: 5}}, nil
	})

	res, err := newKnownAccountsFetcher(noSaveAccountCache)()
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("fetches = %d, want a mismatched entry's armed wait discarded — a real fetch, not a wait for someone else's throttle", calls)
	}
	if len(res.Accounts) != 1 || res.Accounts[0].Info == nil || res.Accounts[0].Info.FiveHour.Pct != 5 {
		t.Fatalf("accounts = %#v, want avisoma's fresh numbers, not the mismatched entry's 99%%", res.Accounts)
	}
}

func TestReconcileFetchOrder(t *testing.T) {
	t.Run("keeps prior order for survivors and appends new names at the end", func(t *testing.T) {
		got := reconcileFetchOrder([]string{"bravo", "alpha"}, []string{"alpha", "bravo", "charlie"})
		want := []string{"bravo", "alpha", "charlie"}
		if !equalStrings(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})
	t.Run("a name prior remembered but that no longer exists just drops out", func(t *testing.T) {
		got := reconcileFetchOrder([]string{"bravo", "gone", "alpha"}, []string{"alpha", "bravo"})
		want := []string{"bravo", "alpha"}
		if !equalStrings(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})
	t.Run("a nil prior is every name in names order", func(t *testing.T) {
		got := reconcileFetchOrder(nil, []string{"alpha", "bravo"})
		want := []string{"alpha", "bravo"}
		if !equalStrings(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})
}

func TestReorderFailedFirst(t *testing.T) {
	order := []string{"alpha", "bravo", "charlie", "delta"}
	got := reorderFailedFirst(order, map[string]bool{"charlie": true, "alpha": true})
	want := []string{"alpha", "charlie", "bravo", "delta"}
	if !equalStrings(got, want) {
		t.Fatalf("got %v, want %v — failed names first, both groups keeping their own relative order", got, want)
	}
	// Nothing failed: the order is unchanged.
	if got := reorderFailedFirst(order, nil); !equalStrings(got, order) {
		t.Fatalf("got %v, want the input order unchanged when nothing failed", got)
	}
}

// The fetch is sequential now, and any account this pass could not get fresh
// numbers for — a genuine failure here — moves to the front of the next
// pass's queue, ahead of accounts that were fine. A chronically struggling
// account must not settle at the tail of a long list.
func TestKnownAccountsFetcherRetriesAFailedAccountFirst(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TMPDIR", t.TempDir())
	writeSnapshotFixture(t, home, "alpha", "tok-alpha", "andy@alpha.example")
	writeSnapshotFixture(t, home, "bravo", "tok-bravo", "andy@bravo.example")
	writeSnapshotFixture(t, home, "charlie", "tok-charlie", "andy@charlie.example")

	var order []string
	failBravo := true
	swapFetch(t, func(token string) (*UsageInfo, error) {
		order = append(order, token)
		if token == "tok-bravo" && failBravo {
			return nil, &usageHTTPError{Status: 429}
		}
		return &UsageInfo{FiveHour: usageBucket{Pct: 1}}, nil
	})

	fetcher := newKnownAccountsFetcher(noSaveAccountCache)
	if _, err := fetcher(); err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	want1 := []string{"tok-alpha", "tok-bravo", "tok-charlie"}
	if !equalStrings(order, want1) {
		t.Fatalf("pass 1 fetch order = %v, want %v (plain directory order)", order, want1)
	}

	order = nil
	failBravo = false
	if _, err := fetcher(); err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	if len(order) == 0 || order[0] != "tok-bravo" {
		t.Fatalf("pass 2 fetch order = %v, want bravo first after failing pass 1", order)
	}
}

// The actual point of persisting the backoff: a process restarted mid-wait
// must not fire a real request for an account before its persisted deadline
// passes. Seeded via a direct saveAccountCache write, exactly what a prior
// process instance's own save call would have left on disk.
func TestKnownAccountsFetcherHonorsSeededBackoffAcrossRestart(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TMPDIR", t.TempDir())
	writeSnapshotFixture(t, home, "avisoma", "tok-avisoma", "andy@avisoma.com")

	calls := 0
	swapFetch(t, func(string) (*UsageInfo, error) {
		calls++
		return &UsageInfo{FiveHour: usageBucket{Pct: 1}}, nil
	})

	saveAccountCache("avisoma", accountCacheEntry{
		BackoffStreak: 3, BackoffNextAttempt: time.Now().Add(10 * time.Minute),
	})
	fetcher := newKnownAccountsFetcher(noSaveAccountCache)
	res, err := fetcher()
	if err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("fetches = %d, want a persisted future deadline to withhold the round trip", calls)
	}
	if len(res.Accounts) != 1 || res.Accounts[0].Reason != usageRateLimitedReason {
		t.Fatalf("accounts = %#v, want a synthesized rate-limited entry with no round trip", res.Accounts)
	}
}

// A persisted deadline that has already elapsed — the wait ran out while the
// process was down — must not hold anything back.
func TestKnownAccountsFetcherFetchesWhenSeededBackoffHasElapsed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TMPDIR", t.TempDir())
	writeSnapshotFixture(t, home, "avisoma", "tok-avisoma", "andy@avisoma.com")

	calls := 0
	swapFetch(t, func(string) (*UsageInfo, error) {
		calls++
		return &UsageInfo{FiveHour: usageBucket{Pct: 1}}, nil
	})

	saveAccountCache("avisoma", accountCacheEntry{
		BackoffStreak: 3, BackoffNextAttempt: time.Now().Add(-time.Minute),
	})
	fetcher := newKnownAccountsFetcher(noSaveAccountCache)
	res, err := fetcher()
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("fetches = %d, want an already-elapsed persisted deadline to allow a real fetch", calls)
	}
	if len(res.Accounts) != 1 || res.Accounts[0].Info == nil {
		t.Fatalf("accounts = %#v, want fresh numbers", res.Accounts)
	}
}

// save is called for each account attempted this pass, so a restart between
// passes sees an accurate picture of who is currently waiting: armed after
// the first throttle (streak 1, still due immediately — the next ordinary
// tick fetches for real — but a restart before the second throttle should
// still know one already happened), cleared once a fetch succeeds.
func TestKnownAccountsFetcherPersistsBackoffAfterEachPass(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TMPDIR", t.TempDir())
	writeSnapshotFixture(t, home, "avisoma", "tok-avisoma", "andy@avisoma.com")

	throttled := true
	swapFetch(t, func(string) (*UsageInfo, error) {
		if throttled {
			return nil, &usageHTTPError{Status: 429}
		}
		return &UsageInfo{FiveHour: usageBucket{Pct: 1}}, nil
	})

	type saveCall struct {
		name   string
		streak int
	}
	var saved []saveCall
	save := func(name string, e accountCacheEntry) {
		saved = append(saved, saveCall{name, e.BackoffStreak})
	}
	fetcher := newKnownAccountsFetcher(save)
	if _, err := fetcher(); err != nil {
		t.Fatal(err)
	}
	if len(saved) != 1 || saved[0].name != "avisoma" || saved[0].streak != 1 {
		t.Fatalf("saved after first throttle = %+v, want avisoma streak 1", saved)
	}

	throttled = false
	if _, err := fetcher(); err != nil {
		t.Fatal(err)
	}
	if len(saved) != 2 || saved[1].streak != 0 {
		t.Fatalf("saved after success = %+v, want the wait cleared", saved)
	}
}

// The hub's fetcher is the only thing with memory across polls: a failed second
// pass must re-serve the first pass' numbers, and a snapshot that disappears
// from disk must stop being remembered.
func TestKnownAccountsFetcherCarriesNumbersAcrossPolls(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TMPDIR", t.TempDir())
	writeSnapshotFixture(t, home, "trecs", "tok-t", "andy@trecs.aero")
	writeSnapshotFixture(t, home, "spare", "tok-s", "andy@spare.dev")

	healthy := map[string]*UsageInfo{
		"tok-t": {FiveHour: usageBucket{Pct: 63}},
		"tok-s": {FiveHour: usageBucket{Pct: 12}},
	}
	var mu sync.Mutex
	throttled := false
	swapFetch(t, func(token string) (*UsageInfo, error) {
		mu.Lock()
		defer mu.Unlock()
		if throttled {
			return nil, &usageHTTPError{Status: 429}
		}
		return healthy[token], nil
	})

	// Real persistence, not a no-op: this test's whole point is that pass 2
	// reads back what pass 1 wrote, so the numbers survive a throttle.
	fetcher := newKnownAccountsFetcher(saveAccountCache)
	first, err := fetcher()
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Accounts) != 2 || first.Accounts[0].Info == nil {
		t.Fatalf("first pass = %#v, want both accounts fetched", first.Accounts)
	}

	mu.Lock()
	throttled = true
	mu.Unlock()
	second, err := fetcher()
	if err != nil {
		t.Fatalf("a throttled pass must not fail the batch: %v", err)
	}
	if len(second.Accounts) != 2 {
		t.Fatalf("second pass = %#v, want both accounts still listed", second.Accounts)
	}
	for _, a := range second.Accounts {
		if !a.Stale || a.Info == nil || a.Expired {
			t.Fatalf("throttled account = %#v, want stale numbers carried forward", a)
		}
		if a.Reason != "rate limited" {
			t.Fatalf("reason = %q, want the throttle named", a.Reason)
		}
	}

	// A snapshot deleted from disk drops out of the listing, and with it out of
	// the fetcher's memory — nothing keeps re-serving numbers for an account
	// this host no longer has.
	if err := os.Remove(snapshotCredentialPath(home, "spare")); err != nil {
		t.Fatal(err)
	}
	third, err := fetcher()
	if err != nil {
		t.Fatal(err)
	}
	if len(third.Accounts) != 1 || third.Accounts[0].Name != "trecs" {
		t.Fatalf("third pass = %#v, want only the surviving snapshot", third.Accounts)
	}
}

// A warm start carries forward too: each account's own disk cache is what the
// fetcher reads on its very first pass (no separate seed needed — the fetcher
// has no in-memory state of its own at all), so a first poll that lands
// mid-throttle shows the cached numbers rather than a placeholder.
func TestKnownAccountsFetcherSeedsFromCache(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TMPDIR", t.TempDir())
	writeSnapshotFixture(t, home, "trecs", "tok-t", "andy@trecs.aero")
	swapFetch(t, func(string) (*UsageInfo, error) { return nil, &usageHTTPError{Status: 429} })

	saveAccountCache("trecs", accountCacheEntry{
		Account:   "andy@trecs.aero",
		Info:      &UsageInfo{FiveHour: usageBucket{Pct: 63}},
		FetchedAt: time.Now().Add(-time.Minute),
	})
	res, err := newKnownAccountsFetcher(noSaveAccountCache)()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Accounts) != 1 {
		t.Fatalf("accounts = %#v, want one", res.Accounts)
	}
	if got := res.Accounts[0]; !got.Stale || got.Info == nil || got.Info.FiveHour.Pct != 63 {
		t.Fatalf("first pass after a warm start = %#v, want the seeded numbers, stale", got)
	}
}

func TestDerefKnownAccounts(t *testing.T) {
	if got := derefKnownAccounts(nil); got != nil {
		t.Fatalf("nil snapshot = %#v, want nil", got)
	}
	if got := activeSnapshotNameOf(nil); got != "" {
		t.Fatalf("nil snapshot active name = %q, want empty", got)
	}
	res := knownAccountsResult{Accounts: []KnownAccountUsage{{Name: "avisoma"}}, ActiveName: "trecs"}
	if got := derefKnownAccounts(&res); len(got) != 1 || got[0].Name != "avisoma" {
		t.Fatalf("deref = %#v", got)
	}
	if got := activeSnapshotNameOf(&res); got != "trecs" {
		t.Fatalf("active name = %q, want trecs", got)
	}
}

// An account the endpoint keeps throttling stops being asked every tick. The
// same account is commonly held as a snapshot on more than one host, so a
// chronic 429 otherwise costs a failed round trip per host per poll forever.
//
// Wall-clock time is real here, and deliberately: every pass below runs inside
// milliseconds, which is far inside any armed wait, so "was the endpoint
// touched" is exactly what the call count reports. The schedule's arithmetic is
// tested separately (TestUsageBackoffUntil).
func TestKnownAccountsFetcherBacksOffRepeatedThrottles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TMPDIR", t.TempDir())
	writeSnapshotFixture(t, home, "trecs", "tok-t", "andy@trecs.aero")

	var mu sync.Mutex
	calls := 0
	throttled := true
	swapFetch(t, func(string) (*UsageInfo, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if throttled {
			return nil, &usageHTTPError{Status: 429}
		}
		return &UsageInfo{FiveHour: usageBucket{Pct: 21}}, nil
	})
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return calls
	}
	setThrottled := func(v bool) {
		mu.Lock()
		defer mu.Unlock()
		throttled = v
	}
	// Real persistence, not a no-op: this fetcher keeps no in-memory state at
	// all any more, so building a real streak across passes (and carrying
	// numbers past a throttle) requires the disk round-trip.
	fetcher := newKnownAccountsFetcher(saveAccountCache)
	pass := func(t *testing.T, n int) KnownAccountUsage {
		t.Helper()
		res, err := fetcher()
		if err != nil {
			t.Fatalf("pass %d: %v", n, err)
		}
		if len(res.Accounts) != 1 {
			t.Fatalf("pass %d: accounts = %#v, want the one snapshot listed", n, res.Accounts)
		}
		return res.Accounts[0]
	}

	// A first throttle imposes no wait at all — most heal within one tick.
	pass(t, 1)
	if count() != 1 {
		t.Fatalf("fetches = %d, want the first pass to fetch", count())
	}
	// A success ends the streak before it can build.
	setThrottled(false)
	if got := pass(t, 2); got.Info == nil || got.Reason != "" {
		t.Fatalf("pass 2 = %#v, want fresh numbers", got)
	}
	if count() != 2 {
		t.Fatalf("fetches = %d, want the pass after one throttle to fetch", count())
	}

	// Now two consecutive throttles. The second is what arms the wait — which
	// also proves the success above cleared the streak, since otherwise this pass
	// would already have been skipped.
	setThrottled(true)
	pass(t, 3)
	pass(t, 4)
	if count() != 4 {
		t.Fatalf("fetches = %d, want both throttled passes to have fetched", count())
	}

	// Inside the armed wait the endpoint is not touched, and the entry still
	// reads exactly as a carried-forward throttle: the header shows the numbers
	// it already showed, marked stale, not a blank line.
	got := pass(t, 5)
	if count() != 4 {
		t.Fatalf("fetches = %d, want the pass inside the backoff to skip the endpoint", count())
	}
	if !got.Stale || got.Info == nil || got.Info.FiveHour.Pct != 21 {
		t.Fatalf("backed-off entry = %#v, want the last good numbers carried forward", got)
	}
	if got.Reason != usageRateLimitedReason || got.Expired {
		t.Fatalf("backed-off entry = %#v, want the throttle named and no re-login prompt", got)
	}
}

// A snapshot that becomes the live account is fetched through the live path, so
// whatever wait was armed against it means nothing — and, critically, it must
// still be recognised as the active snapshot. A backed-off name that were simply
// dropped from the batch would leave ActiveName empty and the host heading
// unlabelled.
func TestKnownAccountsFetcherResolvesActiveNameWhileBackedOff(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TMPDIR", t.TempDir())
	writeSnapshotFixture(t, home, "trecs", "tok-t", "andy@trecs.aero")

	var mu sync.Mutex
	calls := 0
	swapFetch(t, func(string) (*UsageInfo, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		return nil, &usageHTTPError{Status: 429}
	})
	// Real persistence: two consecutive throttles must actually arm a wait
	// (streak building requires the disk round-trip, since this fetcher keeps
	// no in-memory state), which is the "backed off" precondition this test
	// is named for.
	fetcher := newKnownAccountsFetcher(saveAccountCache)
	for i := 1; i <= 2; i++ {
		if _, err := fetcher(); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}

	// Log in as that very account: ~/.claude.json is what loadAccountEmail reads.
	live := `{"oauthAccount":{"emailAddress":"andy@trecs.aero"}}`
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(live), 0600); err != nil {
		t.Fatal(err)
	}
	res, err := fetcher()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Accounts) != 0 {
		t.Fatalf("accounts = %#v, want the live account left out", res.Accounts)
	}
	if res.ActiveName != "trecs" {
		t.Fatalf("ActiveName = %q, want the backed-off snapshot recognised as live", res.ActiveName)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 2 {
		t.Fatalf("fetches = %d, want the live account never fetched from its snapshot", calls)
	}
}

// TestKnownAccountUsageWrongIdentity is fix 2's snapshot half. A snapshot whose
// token the profile endpoint attributes to somebody else must not put that
// somebody's bars on screen under this account's email; it reads as its own
// classification instead, beside whatever this account's own last verified
// reading was.
func TestKnownAccountUsageWrongIdentity(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeSnapshotFixture(t, home, "trecs", "tok-trecs", "andy@trecs.aero")

	stranger := &UsageInfo{
		FiveHour:        usageBucket{Pct: 91},
		VerifiedAccount: "someone@elsewhere.example",
	}
	fetch := func(string) (*UsageInfo, error) { return stranger, nil }

	t.Run("fresh numbers from the wrong account are never shown", func(t *testing.T) {
		got, skip := knownAccountUsage("trecs", "andy@avisoma.com", nil, fetch)
		if skip {
			t.Fatal("skip = true, want the entry reported")
		}
		if got.Info != nil {
			t.Fatalf("Info = %+v, want the stranger's numbers dropped", got.Info)
		}
		if got.Expired {
			t.Fatal("Expired = true; a working credential under the wrong name is not a dead one")
		}
		if got.Reason != usageWrongIdentityReason {
			t.Fatalf("Reason = %q, want %q", got.Reason, usageWrongIdentityReason)
		}
		if got.Account != "andy@trecs.aero" {
			t.Fatalf("Account = %q, want the snapshot's own claimed email", got.Account)
		}
	})

	t.Run("this account's own VERIFIED last reading carries forward", func(t *testing.T) {
		prev := &KnownAccountUsage{
			Name:      "trecs",
			Account:   "andy@trecs.aero",
			Info:      &UsageInfo{FiveHour: usageBucket{Pct: 40}},
			Verified:  true,
			FetchedAt: time.Now().Add(-time.Minute),
		}
		got, _ := knownAccountUsage("trecs", "andy@avisoma.com", prev, fetch)
		if got.Info == nil || got.Info.FiveHour.Pct != 40 {
			t.Fatalf("Info = %+v, want this account's own carried-forward numbers", got.Info)
		}
		if !got.Stale || got.Reason != usageWrongIdentityReason {
			t.Fatalf("got = %+v, want a stale entry tagged %q", got, usageWrongIdentityReason)
		}
		if !got.FetchedAt.Equal(prev.FetchedAt) {
			t.Fatalf("FetchedAt = %v, want prev's original timestamp", got.FetchedAt)
		}
		if !got.Verified {
			t.Fatal("Verified = false; a carry keeps how its numbers' identity was established")
		}
	})

	// The whole reason KnownAccountUsage.Verified exists. Two passes: the first
	// fetches the stranger's numbers with the probe unavailable, so nothing
	// contradicts the claimed email and the entry looks ordinary; the second
	// reaches the probe and detects the misattribution. carryable compares the
	// claimed email on both sides and cannot tell those numbers from this
	// account's own, so without the Verified gate pass 2 would carry the
	// stranger's bars forward under this account's email — the exact thing
	// dropping the fresh reading exists to prevent.
	t.Run("an unverified earlier pass never carries a stranger's numbers", func(t *testing.T) {
		unreachableProbe := func(string) (*UsageInfo, error) {
			return &UsageInfo{FiveHour: usageBucket{Pct: 91}}, nil
		}
		pass1, _ := knownAccountUsage("trecs", "andy@avisoma.com", nil, unreachableProbe)
		if pass1.Info == nil || pass1.Verified {
			t.Fatalf("pass 1 = %+v, want unverified numbers stored", pass1)
		}
		// Age it so freshness is not what decides the carry.
		pass1.FetchedAt = time.Now().Add(-time.Minute)

		pass2, _ := knownAccountUsage("trecs", "andy@avisoma.com", pass1, fetch)
		if pass2.Reason != usageWrongIdentityReason {
			t.Fatalf("pass 2 reason = %q, want %q", pass2.Reason, usageWrongIdentityReason)
		}
		if pass2.Info != nil {
			t.Fatalf("pass 2 Info = %+v, want no bars carried from an unverified pass", pass2.Info)
		}
		if pass2.Stale {
			t.Fatal("pass 2 marked Stale, but nothing was carried")
		}
	})

	t.Run("agreement is an ordinary success", func(t *testing.T) {
		agreeing := func(string) (*UsageInfo, error) {
			return &UsageInfo{FiveHour: usageBucket{Pct: 40}, VerifiedAccount: "ANDY@trecs.aero"}, nil
		}
		got, _ := knownAccountUsage("trecs", "andy@avisoma.com", nil, agreeing)
		if got.Info == nil || got.Reason != "" {
			t.Fatalf("got = %+v, want fresh numbers and no reason", got)
		}
	})

	t.Run("an unverified reading changes nothing", func(t *testing.T) {
		unverified := func(string) (*UsageInfo, error) {
			return &UsageInfo{FiveHour: usageBucket{Pct: 40}}, nil
		}
		got, _ := knownAccountUsage("trecs", "andy@avisoma.com", nil, unverified)
		if got.Info == nil || got.Reason != "" {
			t.Fatalf("got = %+v, want the pre-verification behaviour", got)
		}
	})
}
