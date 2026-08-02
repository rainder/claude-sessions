package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// usageEndpointFixture stands up a /usage server that records the ignore
// parameters it was sent and answers with body. It returns the ServerConfig
// pointing at it.
func usageEndpointFixture(t *testing.T, name string, body any) (ServerConfig, *[]string) {
	t.Helper()
	var mu sync.Mutex
	seen := []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer token" {
			http.Error(w, "bad auth", http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/usage" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		mu.Lock()
		seen = append(seen, r.URL.Query()["ignore"]...)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return serverConfigFor(t, name, srv.URL), &seen
}

func serverConfigFor(t *testing.T, name, rawURL string) ServerConfig {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	host, portText, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	return ServerConfig{Name: name, Host: host, Port: port, Token: "token"}
}

// writeServersConfig points LoadServerConfigs at the given entries. HOME must
// already be a temp dir.
func writeServersConfig(t *testing.T, home string, cfgs ...ServerConfig) {
	t.Helper()
	dir := filepath.Join(home, ".config", "claude-sessions")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString("servers:\n")
	for _, c := range cfgs {
		fmt.Fprintf(&b, "  - name: %s\n    host: %s\n    port: %d\n    token: %s\n", c.Name, c.Host, c.Port, c.Token)
	}
	if err := os.WriteFile(filepath.Join(dir, "servers.yaml"), []byte(b.String()), 0644); err != nil {
		t.Fatal(err)
	}
}

func sampleUsageResponse() usageResponse {
	return usageResponse{
		Usage: &AccountUsage{Account: "bot@ci.com", Info: &UsageInfo{FiveHour: usageBucket{Pct: 42}}},
		KnownAccounts: []KnownAccountUsage{
			{Name: "trecs", Account: "andy@trecs.aero", Expired: true},
			{Name: "avisoma", Account: "andy@avisoma.com", Info: &UsageInfo{FiveHour: usageBucket{Pct: 41}}},
			// An account the caller asked to skip: present, unfetched.
			{Name: "side", Account: "andy@side.dev"},
		},
		ActiveSnapshotName: "work",
	}
}

func TestFetchRemoteUsageDecodesTheAccountFields(t *testing.T) {
	cfg, _ := usageEndpointFixture(t, "alias", sampleUsageResponse())

	got, err := FetchRemoteUsage(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Usage == nil || got.Usage.Account != "bot@ci.com" || got.Usage.Info.FiveHour.Pct != 42 {
		t.Fatalf("usage = %#v", got.Usage)
	}
	if len(got.KnownAccounts) != 3 {
		t.Fatalf("knownAccounts = %#v, want three entries", got.KnownAccounts)
	}
	if !got.KnownAccounts[0].Expired || got.KnownAccounts[0].Info != nil {
		t.Fatalf("expired entry = %#v", got.KnownAccounts[0])
	}
	skipped := got.KnownAccounts[2]
	if skipped.Name != "side" || skipped.Expired || skipped.Info != nil {
		t.Fatalf("skipped entry = %#v, want present, unfetched and not expired", skipped)
	}
	if got.ActiveSnapshotName != "work" {
		t.Fatalf("activeSnapshotName = %q, want work", got.ActiveSnapshotName)
	}
}

// One repeated parameter per email, never a comma-joined value: an email's
// local-part may legally contain a comma.
func TestFetchRemoteUsageSendsOneIgnoreParamPerEmail(t *testing.T) {
	cfg, seen := usageEndpointFixture(t, "alias", usageResponse{})

	if _, err := FetchRemoteUsage(cfg, []string{"a,b@x.com", "andy@trecs.aero"}); err != nil {
		t.Fatal(err)
	}
	want := []string{"a,b@x.com", "andy@trecs.aero"}
	if len(*seen) != len(want) {
		t.Fatalf("ignore params = %#v, want %#v", *seen, want)
	}
	for i, w := range want {
		if (*seen)[i] != w {
			t.Fatalf("ignore param %d = %q, want %q", i, (*seen)[i], w)
		}
	}
}

// A server predating the route is just another per-host failure.
func TestFetchRemoteUsageTreatsAMissingRouteAsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	if _, err := FetchRemoteUsage(serverConfigFor(t, "alias", srv.URL), nil); err == nil {
		t.Fatal("err = nil for a 404, want a per-host failure")
	}
}

func TestFetchRemoteNoLongerCarriesTheAccountFields(t *testing.T) {
	// Even if an older server still sends them, /sessions is no longer their
	// source — two writers for one field would make the winner depend on poll
	// ordering.
	payload, err := json.Marshal(map[string]any{
		"sessions":           []Session{},
		"usage":              AccountUsage{Account: "bot@ci.com", Info: &UsageInfo{}},
		"knownAccounts":      []KnownAccountUsage{{Name: "trecs", Account: "andy@trecs.aero"}},
		"activeSnapshotName": "work",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := fetchRemoteFixture(t, string(payload))
	if got.Usage != nil || got.KnownAccounts != nil || got.ActiveSnapshotName != "" {
		t.Fatalf("FetchRemote populated the account fields: %#v", got)
	}
}

func TestApplyRemoteUsageOverlaysAllThreeFields(t *testing.T) {
	r := applyRemoteUsage(RemoteResult{Name: "alias", Sessions: []Session{{PID: 1}}}, sampleUsageResponse())
	if r.Usage == nil || r.Usage.Account != "bot@ci.com" {
		t.Fatalf("usage = %#v", r.Usage)
	}
	if len(r.KnownAccounts) != 3 || r.ActiveSnapshotName != "work" {
		t.Fatalf("known/active = %#v / %q", r.KnownAccounts, r.ActiveSnapshotName)
	}
	if len(r.Sessions) != 1 {
		t.Fatalf("sessions = %#v, want the overlay to leave them alone", r.Sessions)
	}
}

func TestOverlayRemoteUsageOnlyTouchesHostsItHasData(t *testing.T) {
	remotes := []RemoteResult{{Name: "alias"}, {Name: "other"}}
	remotes = overlayRemoteUsage(remotes, map[string]usageResponse{"alias": sampleUsageResponse()})
	if remotes[0].ActiveSnapshotName != "work" {
		t.Fatalf("alias = %#v, want the overlay applied", remotes[0])
	}
	if remotes[1].Usage != nil || remotes[1].KnownAccounts != nil {
		t.Fatalf("other = %#v, want it left untouched", remotes[1])
	}
}

// End-to-end for the one-shot CLI paths: a host's account bars must still reach
// the rendered table now that /sessions no longer carries them.
func TestMergeRemoteUsageRestoresAccountBarsForOneShotRendering(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg, _ := usageEndpointFixture(t, "alias", sampleUsageResponse())
	writeServersConfig(t, home, cfg)

	remotes := mergeRemoteUsage([]RemoteResult{{Name: "alias", Sessions: []Session{{PID: 42, Host: "alias"}}}})

	if len(remotes[0].Sessions) != 1 {
		t.Fatalf("sessions = %#v, want the /sessions result preserved", remotes[0].Sessions)
	}
	lines := dedupeAccounts(AccountUsage{}, nil, remotes)
	var labels []string
	for _, l := range lines {
		labels = append(labels, l.label)
	}
	// bot@ci.com's live line, trecs' expired placeholder and avisoma's healthy
	// snapshot line, in the order the host reported them; "side" was reported
	// unfetched, so it contributes no bar at all. The two "andy" local-parts
	// collide, so dedupeAccounts promotes both labels to the full email.
	want := []string{"bot", "andy@trecs.aero", "andy@avisoma.com"}
	if len(labels) != len(want) {
		t.Fatalf("account lines = %v, want %v", labels, want)
	}
	for i, w := range want {
		if labels[i] != w {
			t.Fatalf("account line %d = %q, want %q (all: %v)", i, labels[i], w, labels)
		}
	}
	if err := remotes[0].Error; err != "" {
		t.Fatalf("Error = %q, want the merge to leave it empty", err)
	}
}

// A /usage failure must not become the row's error — the session list beside it
// is perfectly good.
func TestMergeRemoteUsageLeavesAFailedHostAlone(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	writeServersConfig(t, home, serverConfigFor(t, "alias", srv.URL))

	remotes := mergeRemoteUsage([]RemoteResult{{Name: "alias", Sessions: []Session{{PID: 42}}}})

	if remotes[0].Error != "" {
		t.Fatalf("Error = %q, want a usage failure to stay silent", remotes[0].Error)
	}
	if len(remotes[0].Sessions) != 1 || remotes[0].Usage != nil {
		t.Fatalf("row = %#v, want sessions kept and no usage", remotes[0])
	}
}

// An unreachable host in `account list` must say so rather than read as a host
// with no snapshots.
func TestOneRemoteUsageReportsAnUnreachableHostAsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	got := oneRemoteUsage(serverConfigFor(t, "alias", srv.URL))

	if got.Error == "" {
		t.Fatalf("row = %#v, want an Error", got)
	}
	if listing := remoteAccountListing(got); listing.Error == "" {
		t.Fatalf("listing = %#v, want the error surfaced in the table", listing)
	}
}

func TestOneRemoteUsageBuildsTheAccountListingRows(t *testing.T) {
	cfg, _ := usageEndpointFixture(t, "alias", sampleUsageResponse())

	listing := remoteAccountListing(oneRemoteUsage(cfg))

	if listing.Error != "" {
		t.Fatalf("listing error = %q", listing.Error)
	}
	// Three snapshots plus the active one, which the host reports separately.
	byName := map[string]accountRow{}
	for _, row := range listing.Rows {
		byName[row.Name] = row
	}
	if len(byName) != 4 {
		t.Fatalf("rows = %#v, want every snapshot plus the active account", listing.Rows)
	}
	if !byName["work"].Active || byName["work"].Email != "bot@ci.com" {
		t.Fatalf("active row = %#v", byName["work"])
	}
	// An unfetched account is still a switch target.
	if _, ok := byName["side"]; !ok {
		t.Fatalf("rows = %#v, want the unfetched account listed", listing.Rows)
	}
}

func TestLocalFreshAccountEmailsSkipsWhatLocalCannotVouchFor(t *testing.T) {
	live := &AccountUsage{Account: "Andy@Avisoma.com", Info: &UsageInfo{}}
	known := []KnownAccountUsage{
		{Name: "trecs", Account: "andy@trecs.aero", Info: &UsageInfo{}},
		// Locally expired: telling a remote to skip this one would suppress the
		// healthy numbers it may well have for the same account.
		{Name: "side", Account: "andy@side.dev", Expired: true},
		// Fetched nothing yet.
		{Name: "new", Account: "andy@new.dev"},
		// No identity file: an empty ignore entry names nothing.
		{Name: "anon", Account: "", Info: &UsageInfo{}},
	}

	got := localFreshAccountEmails(live, known)

	want := []string{"andy@avisoma.com", "andy@trecs.aero"}
	if len(got) != len(want) {
		t.Fatalf("ignore list = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ignore list = %v, want %v", got, want)
		}
	}
}

func TestLocalFreshAccountEmailsSkipsAnUnfetchedLiveAccount(t *testing.T) {
	if got := localFreshAccountEmails(&AccountUsage{Account: "andy@avisoma.com"}, nil); len(got) != 0 {
		t.Fatalf("ignore list = %v, want nothing while local has no numbers of its own", got)
	}
	if got := localFreshAccountEmails(nil, nil); len(got) != 0 {
		t.Fatalf("ignore list = %v, want nothing before the first poll lands", got)
	}
}

// fetchFrom rather than fetchAll: an httptest server is on 127.0.0.1, which
// dropSelfServer exists to filter out, so the config-resolution half cannot be
// driven from a test at all.
func TestRemoteUsageHubFetchesAndClearsPerPass(t *testing.T) {
	var mu sync.Mutex
	fail := false
	seenIgnore := [][]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seenIgnore = append(seenIgnore, r.URL.Query()["ignore"])
		failing := fail
		mu.Unlock()
		if failing {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(sampleUsageResponse())
	}))
	defer srv.Close()
	cfgs := []ServerConfig{serverConfigFor(t, "alias", srv.URL)}

	h := &RemoteUsageHub{
		kick:   make(chan struct{}, 1),
		stop:   make(chan struct{}),
		ignore: func() []string { return []string{"andy@avisoma.com"} },
	}
	h.fetchFrom(cfgs)

	snap := h.Snapshot()
	if got, ok := snap["alias"]; !ok || got.ActiveSnapshotName != "work" {
		t.Fatalf("snapshot = %#v, want alias' answer", snap)
	}
	mu.Lock()
	sent := append([][]string(nil), seenIgnore...)
	mu.Unlock()
	if len(sent) != 1 || len(sent[0]) != 1 || sent[0][0] != "andy@avisoma.com" {
		t.Fatalf("ignore sent = %#v, want the one email local vouches for", sent)
	}

	// A failed pass clears rather than freezing: usage bars have no stale
	// rendering, so a carried-forward percentage would silently read as live.
	mu.Lock()
	fail = true
	mu.Unlock()
	h.fetchFrom(cfgs)
	if got := h.Snapshot(); len(got) != 0 {
		t.Fatalf("snapshot = %#v, want a failed pass to clear the host", got)
	}
}
