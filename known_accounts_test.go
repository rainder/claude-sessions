package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeSnapshotFixture creates one claude-switch credential snapshot (and, when
// email is non-empty, its account.json sibling) under home. It goes through
// snapshotCredentialPath so the fixture matches whichever platform the test
// runs on — hardcoding ".keychain-cred" would break every Linux build.
func writeSnapshotFixture(t *testing.T, home, name, token, email string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0755); err != nil {
		t.Fatal(err)
	}
	creds := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q}}`, token)
	if err := os.WriteFile(snapshotCredentialPath(home, name), []byte(creds), 0600); err != nil {
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

func TestFetchKnownAccountsWithNoSnapshots(t *testing.T) {
	// A fresh machine with no claude-switch setup: an empty list, never an error
	// — an error here would put the poller into backoff over nothing.
	t.Setenv("HOME", t.TempDir())
	got, err := fetchKnownAccounts()
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

// stubUsageFetch returns a fetch func that answers per token: a nil entry means
// that token fails.
func stubUsageFetch(byToken map[string]*UsageInfo) func(string) (*UsageInfo, error) {
	return func(token string) (*UsageInfo, error) {
		if info, ok := byToken[token]; ok && info != nil {
			return info, nil
		}
		return nil, fmt.Errorf("usage endpoint: HTTP 401")
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
		got, skipped := knownAccountUsage("avisoma", "Andy@Avisoma.com", fetch)
		if !skipped {
			t.Fatal("skipped = false, want true for the live account's own snapshot")
		}
		if got != nil {
			t.Fatalf("a skipped account must contribute no entry, got %#v", got)
		}
	})

	t.Run("populated Info on success", func(t *testing.T) {
		got, skipped := knownAccountUsage("avisoma", "andy@trecs.aero", fetch)
		if skipped {
			t.Fatal("skipped = true, want false for a non-live account")
		}
		if got.Expired {
			t.Fatalf("Expired = true on a successful fetch: %#v", got)
		}
		if got.Name != "avisoma" || got.Account != "andy@avisoma.com" {
			t.Fatalf("identity wrong: %#v", got)
		}
		if got.Info == nil || got.Info.FiveHour.Pct != 41 {
			t.Fatalf("Info = %#v, want the fetched snapshot", got.Info)
		}
	})

	t.Run("HTTP failure marks the account expired, keeps its identity", func(t *testing.T) {
		got, _ := knownAccountUsage("trecs", "andy@avisoma.com", fetch)
		if !got.Expired || got.Info != nil {
			t.Fatalf("failed fetch = %#v, want Expired with no Info", got)
		}
		if got.Name != "trecs" || got.Account != "andy@trecs.aero" {
			t.Fatalf("identity lost on failure: %#v", got)
		}
	})

	t.Run("unreadable credentials mark the account expired without fetching", func(t *testing.T) {
		got, _ := knownAccountUsage("broken", "andy@avisoma.com", fetch)
		if !got.Expired || got.Info != nil {
			t.Fatalf("unparseable snapshot = %#v, want Expired with no Info", got)
		}
		if got.Account != "andy@broken.dev" {
			t.Fatalf("account = %q, want the email account.json still holds", got.Account)
		}
	})

	t.Run("an unknown email never matches the live account", func(t *testing.T) {
		got, skipped := knownAccountUsage("nameless", "andy@avisoma.com", fetch)
		if skipped {
			t.Fatal("a snapshot with no readable email must not be skipped as live")
		}
		if got.Account != "" || got.Info == nil {
			t.Fatalf("got %#v, want an unnamed but fetched account", got)
		}
	})
}

func TestAllKnownAccountsNeverFailsOnOneBadAccount(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeSnapshotFixture(t, home, "avisoma", "tok-a", "andy@avisoma.com")
	writeSnapshotFixture(t, home, "dead", "tok-dead", "andy@dead.dev")
	writeSnapshotFixture(t, home, "trecs", "tok-t", "andy@trecs.aero")
	fetch := stubUsageFetch(map[string]*UsageInfo{
		"tok-a": {FiveHour: usageBucket{Pct: 41}},
		"tok-t": {FiveHour: usageBucket{Pct: 22}},
	})
	// The real per-account path, only with the HTTP leg stubbed.
	one := func(name, liveEmail string) (*KnownAccountUsage, bool) {
		return knownAccountUsage(name, liveEmail, fetch)
	}

	got, active := allKnownAccounts([]string{"avisoma", "dead", "trecs"}, "someone@else.com", one)
	if len(got) != 3 {
		t.Fatalf("len = %d, want every account represented: %#v", len(got), got)
	}
	if active != "" {
		t.Fatalf("active snapshot = %q, want empty when no snapshot is the live account", active)
	}
	// Order follows names, so a failing account can't shuffle its neighbours.
	if got[0].Name != "avisoma" || got[1].Name != "dead" || got[2].Name != "trecs" {
		t.Fatalf("order = %q,%q,%q, want names order", got[0].Name, got[1].Name, got[2].Name)
	}
	if got[1].Expired != true || got[1].Info != nil {
		t.Fatalf("failing account = %#v, want Expired", got[1])
	}
	// The whole point: one dead account suppresses nobody else's healthy data.
	if got[0].Info == nil || got[0].Info.FiveHour.Pct != 41 {
		t.Fatalf("healthy account before the failure lost its Info: %#v", got[0])
	}
	if got[2].Info == nil || got[2].Info.FiveHour.Pct != 22 {
		t.Fatalf("healthy account after the failure lost its Info: %#v", got[2])
	}

	// The live account drops out entirely; everyone else still reports. The case
	// difference proves the match is case-insensitive, like the dedupe key — and
	// the skipped name comes straight back out as the active snapshot, so the two
	// can never disagree about which account was left out.
	live, active := allKnownAccounts([]string{"avisoma", "dead", "trecs"}, "Andy@Avisoma.com", one)
	if len(live) != 2 || live[0].Name != "dead" || live[1].Name != "trecs" {
		t.Fatalf("live-account skip wrong: %#v", live)
	}
	if active != "avisoma" {
		t.Fatalf("active snapshot = %q, want the skipped name", active)
	}

	// No names at all is an empty result, never a nil-deref or an error path.
	if got, active := allKnownAccounts(nil, "", one); len(got) != 0 || active != "" {
		t.Fatalf("no names = %#v/%q, want empty", got, active)
	}
}

func TestKnownAccountsCacheRoundTrip(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	res := knownAccountsResult{
		Accounts: []KnownAccountUsage{
			{Name: "avisoma", Account: "andy@avisoma.com", Info: &UsageInfo{FiveHour: usageBucket{Pct: 41}}},
		},
		ActiveName: "trecs",
	}
	saveKnownAccountsCache(&res)
	got := loadKnownAccountsCache()
	if got == nil {
		t.Fatal("cache round-trip returned nil")
	}
	if len(got.Accounts) != 1 || got.Accounts[0].Name != "avisoma" {
		t.Fatalf("cached accounts = %#v", got.Accounts)
	}
	if got.Accounts[0].Info == nil || got.Accounts[0].Info.FiveHour.Pct != 41 {
		t.Fatalf("cached info = %#v", got.Accounts[0].Info)
	}
	// Which account is logged in can change while the process is down, so the
	// active name is never seeded from disk — the first fetch resolves it.
	if got.ActiveName != "" {
		t.Fatalf("seeded active snapshot = %q, want empty", got.ActiveName)
	}
}

func TestKnownAccountsCacheNeverPersistsExpired(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	res := knownAccountsResult{Accounts: []KnownAccountUsage{
		{Name: "avisoma", Account: "andy@avisoma.com", Info: &UsageInfo{FiveHour: usageBucket{Pct: 41}}},
		{Name: "trecs", Account: "andy@trecs.aero", Expired: true},
	}}
	saveKnownAccountsCache(&res)
	got := loadKnownAccountsCache()
	if got == nil {
		t.Fatal("cache round-trip returned nil")
	}
	if len(got.Accounts) != 1 || got.Accounts[0].Name != "avisoma" {
		t.Fatalf("expired entry survived the cache: %#v", got.Accounts)
	}

	// Nothing but expired entries leaves no usable cache at all — an expired
	// account starts as "no data yet" after a restart.
	onlyExpired := knownAccountsResult{Accounts: []KnownAccountUsage{{Name: "trecs", Account: "andy@trecs.aero", Expired: true}}}
	saveKnownAccountsCache(&onlyExpired)
	if got := loadKnownAccountsCache(); got != nil {
		t.Fatalf("all-expired cache seeded %#v, want nil", *got)
	}
}

func TestKnownAccountsCacheExpiryAndCorruption(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	stale := cachedKnownAccounts{
		FetchedAt: time.Now().Add(-usageCacheMaxAge - time.Minute),
		Accounts:  []KnownAccountUsage{{Name: "avisoma", Info: &UsageInfo{}}},
	}
	data, err := json.Marshal(stale)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(knownAccountsCachePath(), data, 0600); err != nil {
		t.Fatal(err)
	}
	if got := loadKnownAccountsCache(); got != nil {
		t.Fatalf("stale cache seeded %#v, want nil", *got)
	}

	// An entry that decodes without Info is a miss, not a healthy-looking zero.
	nilInfo, err := json.Marshal(cachedKnownAccounts{
		FetchedAt: time.Now(),
		Accounts:  []KnownAccountUsage{{Name: "avisoma"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(knownAccountsCachePath(), nilInfo, 0600); err != nil {
		t.Fatal(err)
	}
	if got := loadKnownAccountsCache(); got != nil {
		t.Fatalf("info-less cache seeded %#v, want nil", *got)
	}

	if err := os.WriteFile(knownAccountsCachePath(), []byte("not json"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := loadKnownAccountsCache(); got != nil {
		t.Fatalf("corrupt cache seeded %#v, want nil", *got)
	}

	if err := os.Remove(knownAccountsCachePath()); err != nil {
		t.Fatal(err)
	}
	if got := loadKnownAccountsCache(); got != nil {
		t.Fatalf("absent cache seeded %#v, want nil", *got)
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
