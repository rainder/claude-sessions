package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// usageNumbersWorld is a hermetic HOME logged into liveEmail with a matching
// claude-switch snapshot liveName, plus a private TMPDIR so every cache file
// (per-account, Grok, Codex) stays inside the test.
func usageNumbersWorld(t *testing.T, liveName, liveEmail string) string {
	t.Helper()
	home := loginAsWithSnapshot(t, liveName, liveEmail)
	t.Setenv("TMPDIR", t.TempDir())
	return home
}

// stubLiveFetch replaces the live Claude fetch and counts calls.
func stubLiveFetch(t *testing.T, fn func() (*AccountUsage, error)) *atomic.Int32 {
	t.Helper()
	var n atomic.Int32
	prev := usageNumbersLiveFetch
	usageNumbersLiveFetch = func() (*AccountUsage, error) {
		n.Add(1)
		return fn()
	}
	t.Cleanup(func() { usageNumbersLiveFetch = prev })
	return &n
}

// stubKnownFetch replaces the known-accounts usage leg and counts calls.
func stubKnownFetch(t *testing.T, info *UsageInfo) *atomic.Int32 {
	t.Helper()
	var n atomic.Int32
	swapFetch(t, func(string) (*UsageInfo, error) {
		n.Add(1)
		c := *info
		return &c, nil
	})
	return &n
}

// noLiveFetch stubs the live fetch for a test that must not make one. TestMain
// makes the real seam panic; this turns that into a test failure with a name.
func noLiveFetch(t *testing.T) *atomic.Int32 {
	t.Helper()
	return stubLiveFetch(t, func() (*AccountUsage, error) {
		t.Errorf("unexpected live fetch")
		return nil, &usageHTTPError{Status: http.StatusTooManyRequests}
	})
}

func liveReading(email string, info *UsageInfo) func() (*AccountUsage, error) {
	return func() (*AccountUsage, error) {
		c := *info
		return &AccountUsage{Account: email, Info: &c, FetchedAt: usageClockNow()}, nil
	}
}

func sampleUsageInfo(five float64) *UsageInfo {
	return &UsageInfo{
		FiveHour: usageBucket{Pct: five, ResetsAt: time.Now().Add(time.Hour).UTC()},
		SevenDay: usageBucket{Pct: 40, ResetsAt: time.Now().Add(72 * time.Hour).UTC()},
	}
}

func TestUsageNumbersWireMapping(t *testing.T) {
	reset := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	info := &UsageInfo{
		FiveHour:          usageBucket{Pct: 12, ResetsAt: reset},
		SevenDay:          usageBucket{Pct: 34},
		WeeklyScoped:      usageBucket{Pct: 56, ResetsAt: reset},
		WeeklyScopedLabel: "Fable",
		Credits:           creditsInfo{Enabled: true, Used: 2550, Limit: 10000, Currency: "USD", DecimalPlaces: 2},
	}
	var e claudeUsageNumbers
	fillClaudeInfo(&e, info)
	if e.FiveHour == nil || e.FiveHour.Pct != 12 || e.FiveHour.ResetsAt == nil || !e.FiveHour.ResetsAt.Equal(reset) {
		t.Errorf("fiveHour = %+v", e.FiveHour)
	}
	if e.SevenDay == nil || e.SevenDay.ResetsAt != nil {
		t.Errorf("sevenDay = %+v, want a zero reset to be omitted", e.SevenDay)
	}
	if e.WeeklyScoped == nil || e.WeeklyScoped.Label != "Fable" || e.WeeklyScoped.Pct != 56 {
		t.Errorf("weeklyScoped = %+v, want label Fable", e.WeeklyScoped)
	}
	if c := e.Credits; c == nil || c.Used != 25.5 || c.Limit != 100 || c.Currency != "USD" || c.Pct != 25.5 {
		t.Errorf("credits = %+v, want 25.5/100 USD at 25.5%%", c)
	}

	var bare claudeUsageNumbers
	fillClaudeInfo(&bare, &UsageInfo{Credits: creditsInfo{Used: 5, Limit: 10}})
	if bare.WeeklyScoped != nil {
		t.Errorf("weeklyScoped = %+v, want nil with no label and a zero bucket", bare.WeeklyScoped)
	}
	if bare.Credits != nil {
		t.Errorf("credits = %+v, want nil when not enabled", bare.Credits)
	}
}

func TestUsageNumbersStaleRule(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name      string
		fetchedAt time.Time
		marked    bool
		want      bool
	}{
		{"no timestamp", time.Time{}, false, true},
		{"fresh", now.Add(-time.Minute), false, false},
		{"exactly at the bound", now.Add(-usageNumbersStaleAfter), false, false},
		{"older than the bound", now.Add(-usageNumbersStaleAfter - time.Second), false, true},
		{"fresh but marked by the fetcher", now.Add(-time.Second), true, true},
	} {
		if got := usageNumbersStale(now, tt.fetchedAt, tt.marked); got != tt.want {
			t.Errorf("%s: stale = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// A per-account cache file written within one interval means a hub is
// polling: no request of any kind.
func TestUsageNumbersHubAliveMakesNoFetch(t *testing.T) {
	home := usageNumbersWorld(t, "main", "live@example.com")
	writeSnapshotFixture(t, home, "other", "tok-other", "other@example.com")
	saveAccountCache("main", accountCacheEntry{
		Account: "live@example.com", FetchedAt: time.Now().Add(-30 * time.Second), Info: sampleUsageInfo(10),
	})
	live := stubLiveFetch(t, func() (*AccountUsage, error) { t.Fatal("live fetch with a hub alive"); return nil, nil })
	known := stubKnownFetch(t, sampleUsageInfo(1))

	resp := newUsageNumbersSource(usageNumbersSourceService).build()
	if live.Load() != 0 || known.Load() != 0 {
		t.Fatalf("fetches = live %d known %d, want none", live.Load(), known.Load())
	}
	if resp.Source != usageNumbersSourceService {
		t.Errorf("source = %q", resp.Source)
	}
	if len(resp.Claude) != 2 {
		t.Fatalf("claude = %+v, want live + other", resp.Claude)
	}
	l := resp.Claude[0]
	if !l.Active || l.Name != "main" || l.FiveHour == nil || l.FiveHour.Pct != 10 || l.Stale || l.FetchedAt == nil {
		t.Errorf("live entry = %+v, want fresh disk numbers", l)
	}
	o := resp.Claude[1]
	if o.Name != "other" || o.Account != "other@example.com" || o.FiveHour != nil || !o.Stale {
		t.Errorf("other entry = %+v, want an account-only stale row", o)
	}
}

// No hub: exactly one pass, and a second build right after makes no request.
func TestUsageNumbersNoHubRunsOnePass(t *testing.T) {
	home := usageNumbersWorld(t, "main", "live@example.com")
	writeSnapshotFixture(t, home, "other", "tok-other", "other@example.com")
	live := stubLiveFetch(t, liveReading("live@example.com", sampleUsageInfo(20)))
	known := stubKnownFetch(t, sampleUsageInfo(30))

	src := newUsageNumbersSource(usageNumbersSourceService)
	resp := src.build()
	if live.Load() != 1 || known.Load() != 1 {
		t.Fatalf("first build fetches = live %d known %d, want 1 and 1", live.Load(), known.Load())
	}
	if resp.Claude[0].FiveHour.Pct != 20 || resp.Claude[0].Stale {
		t.Errorf("live = %+v, want fresh 20%%", resp.Claude[0])
	}
	if resp.Claude[1].FiveHour == nil || resp.Claude[1].FiveHour.Pct != 30 || resp.Claude[1].Stale {
		t.Errorf("other = %+v, want fresh 30%%", resp.Claude[1])
	}

	resp = src.build()
	if live.Load() != 1 || known.Load() != 1 {
		t.Fatalf("second build fetches = live %d known %d, want no new pass", live.Load(), known.Load())
	}
	if resp.Claude[0].FiveHour == nil || resp.Claude[0].FiveHour.Pct != 20 || resp.Claude[1].FiveHour.Pct != 30 {
		t.Errorf("second build = %+v, want the pass's numbers from disk", resp.Claude)
	}
}

// With no cache file at all (a live account with no snapshot), only
// lastRefresh gates the pass: none within the interval, one after it.
func TestUsageNumbersLastRefreshGatesThePass(t *testing.T) {
	loginAs(t, "solo@example.com")
	t.Setenv("TMPDIR", t.TempDir())
	clock := fakeClock(t, time.Now())
	live := stubLiveFetch(t, liveReading("solo@example.com", sampleUsageInfo(5)))

	src := newUsageNumbersSource(usageNumbersSourceService)
	src.build()
	*clock = clock.Add(usageRefreshInterval / 2)
	resp := src.build()
	if live.Load() != 1 {
		t.Fatalf("fetches within the interval = %d, want 1", live.Load())
	}
	if len(resp.Claude) != 1 || resp.Claude[0].FiveHour == nil || resp.Claude[0].Name != "" {
		t.Errorf("claude = %+v, want the remembered live reading with no snapshot name", resp.Claude)
	}
	*clock = clock.Add(usageRefreshInterval)
	src.build()
	if live.Load() != 2 {
		t.Fatalf("fetches after the interval = %d, want 2", live.Load())
	}
}

// Concurrent callers share one pass.
func TestUsageNumbersSingleFlight(t *testing.T) {
	loginAs(t, "solo@example.com")
	t.Setenv("TMPDIR", t.TempDir())
	release := make(chan struct{})
	live := stubLiveFetch(t, func() (*AccountUsage, error) {
		<-release
		return liveReading("solo@example.com", sampleUsageInfo(5))()
	})
	src := newUsageNumbersSource(usageNumbersSourceService)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); src.build() }()
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()
	if live.Load() != 1 {
		t.Fatalf("fetches = %d, want 1", live.Load())
	}
}

func TestUsageNumbersLiveFirstAndSkipsLiveSnapshots(t *testing.T) {
	home := usageNumbersWorld(t, "zz", "live@example.com")
	writeSnapshotFixture(t, home, "zz-alias", "tok-alias", "LIVE@example.com")
	writeSnapshotFixture(t, home, "beta", "tok-beta", "beta@example.com")
	writeSnapshotFixture(t, home, "alpha", "tok-alpha", "alpha@example.com")
	stubLiveFetch(t, liveReading("live@example.com", sampleUsageInfo(1)))
	stubKnownFetch(t, sampleUsageInfo(2))

	resp := newUsageNumbersSource(usageNumbersSourceService).build()
	var got []string
	for _, c := range resp.Claude {
		got = append(got, c.Name+":"+strconv.FormatBool(c.Active))
	}
	want := "zz:true,alpha:false,beta:false"
	if strings.Join(got, ",") != want {
		t.Errorf("order = %v, want %s", got, want)
	}
}

// A throttled live fetch keeps the last numbers, marked stale with a reason.
func TestUsageNumbersLiveFailureKeepsDiskNumbers(t *testing.T) {
	usageNumbersWorld(t, "main", "live@example.com")
	saveAccountCache("main", accountCacheEntry{
		Account: "live@example.com", FetchedAt: time.Now().Add(-10 * time.Minute), Info: sampleUsageInfo(70),
	})
	// Age the file so no hub looks alive.
	old := time.Now().Add(-10 * time.Minute)
	if err := os.Chtimes(accountCachePath("main"), old, old); err != nil {
		t.Fatal(err)
	}
	stubLiveFetch(t, func() (*AccountUsage, error) { return nil, &usageHTTPError{Status: http.StatusTooManyRequests} })

	resp := newUsageNumbersSource(usageNumbersSourceService).build()
	l := resp.Claude[0]
	if l.FiveHour == nil || l.FiveHour.Pct != 70 || !l.Stale || l.Reason != usageRateLimitedReason || l.Expired {
		t.Errorf("live = %+v, want carried 70%% marked stale/rate limited", l)
	}
}

func TestUsageNumbersJSONShape(t *testing.T) {
	at := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	resp := usageNumbersResponse{
		GeneratedAt: at,
		Source:      "service",
		Claude: []claudeUsageNumbers{{
			Name: "main", Account: "a@example.com", Active: true,
			FiveHour:     &usageWindowNumbers{Pct: 10, ResetsAt: &at},
			WeeklyScoped: &usageWindowNumbers{Label: "Fable", Pct: 5},
			Credits:      &usageCreditsNumbers{Enabled: true, Used: 1.5, Limit: 10, Currency: "USD", Pct: 15},
			FetchedAt:    &at,
		}},
		Grok:  []providerUsageNumbers{},
		Codex: []providerUsageNumbers{{Account: "c@example.com", Windows: []usageWindowNumbers{{Label: "5h", Pct: 3}}, Plan: "pro", Stale: true}},
	}
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"generatedAt":"2026-10-07T12:00:00Z","source":"service",` +
		`"claude":[{"name":"main","account":"a@example.com","active":true,` +
		`"fiveHour":{"pct":10,"resetsAt":"2026-10-07T12:00:00Z"},` +
		`"weeklyScoped":{"label":"Fable","pct":5},` +
		`"credits":{"enabled":true,"used":1.5,"limit":10,"currency":"USD","pct":15},` +
		`"fetchedAt":"2026-10-07T12:00:00Z","stale":false}],` +
		`"grok":[],` +
		`"codex":[{"account":"c@example.com","windows":[{"label":"5h","pct":3}],"plan":"pro","stale":true}]}`
	if string(b) != want {
		t.Errorf("json =\n%s\nwant\n%s", b, want)
	}
}

// An empty world still answers [] for every list, never null.
func TestUsageNumbersEmptyListsAreArrays(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TMPDIR", t.TempDir())
	// No ~/.claude.json: the service still runs its live fetcher, which fails.
	stubLiveFetch(t, func() (*AccountUsage, error) { return nil, os.ErrNotExist })
	resp := newUsageNumbersSource(usageNumbersSourceService).build()
	b, _ := json.Marshal(resp)
	for _, k := range []string{`"claude":[]`, `"grok":[]`, `"codex":[]`} {
		if !strings.Contains(string(b), k) {
			t.Errorf("json %s lacks %s", b, k)
		}
	}
}

func writeProviderCache(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestUsageNumbersProviders(t *testing.T) {
	t.Run("service mode reads the hub cache and never fetches", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		t.Setenv("TMPDIR", t.TempDir())
		old := time.Now().Add(-time.Hour)
		writeProviderCache(t, grokUsageCachePath(), cachedGrokUsage{FetchedAt: old, Usage: GrokAccountUsage{
			Account: "g@example.com",
			Info: &GrokUsageInfo{
				Windows: []grokWindow{{Label: "wk", Pct: 42}},
				Credits: creditsInfo{Enabled: true, Used: 1234, Limit: 5000, Currency: "USD", DecimalPlaces: 2},
			},
		}})
		stubLiveFetch(t, func() (*AccountUsage, error) { return nil, os.ErrNotExist })
		resp := newUsageNumbersSource(usageNumbersSourceService).build()
		if len(resp.Grok) != 1 {
			t.Fatalf("grok = %+v", resp.Grok)
		}
		g := resp.Grok[0]
		if g.Account != "g@example.com" || len(g.Windows) != 1 || g.Windows[0].Pct != 42 || !g.Stale || g.Credits == nil || g.Credits.Used != 12.34 {
			t.Errorf("grok entry = %+v", g)
		}
		if len(resp.Codex) != 0 {
			t.Errorf("codex = %+v, want empty with no cache", resp.Codex)
		}
	})

	t.Run("local mode fetches once when the cache is old", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		t.Setenv("TMPDIR", t.TempDir())
		writeProviderCache(t, codexUsageCachePath(), cachedCodexUsage{FetchedAt: time.Now().Add(-time.Hour), Usage: CodexAccountUsage{
			Account: "c@example.com", Info: &CodexUsageInfo{Plan: "plus", Windows: []codexWindow{{Label: "5h", Pct: 1}}},
		}})
		var codexCalls, grokCalls int
		prevC, prevG := usageNumbersCodexFetch, usageNumbersGrokFetch
		usageNumbersCodexFetch = func() (*CodexAccountUsage, error) {
			codexCalls++
			return &CodexAccountUsage{Account: "c@example.com", Info: &CodexUsageInfo{Plan: "pro", Windows: []codexWindow{{Label: "5h", Pct: 9}}}}, nil
		}
		usageNumbersGrokFetch = func() (*GrokAccountUsage, error) {
			grokCalls++
			return nil, os.ErrNotExist // no grok login
		}
		t.Cleanup(func() { usageNumbersCodexFetch, usageNumbersGrokFetch = prevC, prevG })
		noLiveFetch(t)

		resp := newUsageNumbersSource(usageNumbersSourceLocal).build()
		if codexCalls != 1 || grokCalls != 1 {
			t.Fatalf("calls = codex %d grok %d, want 1 each", codexCalls, grokCalls)
		}
		if len(resp.Codex) != 1 || resp.Codex[0].Plan != "pro" || resp.Codex[0].Windows[0].Pct != 9 || resp.Codex[0].Stale {
			t.Errorf("codex = %+v, want the fresh fetch", resp.Codex)
		}
		if len(resp.Grok) != 0 {
			t.Errorf("grok = %+v, want empty after a failed fetch with no cache", resp.Grok)
		}
		// The fetch was saved: a second local build serves it without a request.
		newUsageNumbersSource(usageNumbersSourceLocal).build()
		if codexCalls != 1 {
			t.Errorf("codex calls after a fresh save = %d, want 1", codexCalls)
		}
	})
}

func TestUsageNumbersRouteRequiresToken(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	stubLiveFetch(t, func() (*AccountUsage, error) { return nil, os.ErrNotExist })
	s := &server{token: "secret", numbers: newUsageNumbersSource(usageNumbersSourceService)}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/usage/numbers", nil)
	req.RemoteAddr = "127.0.0.1:5555"
	s.usageNumbers(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: status = %d, want 401", rec.Code)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/usage/numbers", nil)
	req.Header.Set("Authorization", "Bearer secret")
	s.usageNumbers(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("with token: status = %d, body %q", rec.Code, rec.Body.String())
	}
	var resp usageNumbersResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Source != usageNumbersSourceService || resp.Claude == nil || resp.Grok == nil || resp.Codex == nil {
		t.Errorf("resp = %+v", resp)
	}
}

// localBuilds counts which local fallback the CLI took.
type localBuilds struct{ full, disk int }

// pointUsageNumbersAt aims the CLI's service request at addr and stubs the
// local build so the test sees which path answered.
func pointUsageNumbersAt(t *testing.T, addr string) *localBuilds {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(portStr)
	prevCfg, prevBuild, prevTS := usageNumbersServiceConfig, buildLocalUsageNumbers, localTailscaleIPv4
	usageNumbersServiceConfig = func() (ServerConfig, error) {
		return ServerConfig{Host: host, Port: port, Token: "secret"}, nil
	}
	lb := &localBuilds{}
	buildLocalUsageNumbers = func(diskOnly bool) usageNumbersResponse {
		if diskOnly {
			lb.disk++
		} else {
			lb.full++
		}
		return usageNumbersResponse{Source: usageNumbersSourceLocal}
	}
	localTailscaleIPv4 = func(context.Context) string { return "" }
	t.Cleanup(func() {
		usageNumbersServiceConfig, buildLocalUsageNumbers, localTailscaleIPv4 = prevCfg, prevBuild, prevTS
	})
	return lb
}

func TestCollectUsageNumbersFallsBack(t *testing.T) {
	for _, tt := range []struct {
		name     string
		handler  http.HandlerFunc
		wantDisk bool
	}{
		{"404 from an older server is a full build", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", http.StatusNotFound) }, false},
		{"401 is a full build", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", http.StatusUnauthorized) }, false},
		{"5xx is disk only", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom", http.StatusInternalServerError) }, true},
		{"bad body is disk only", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("not json")) }, true},
		{"timeout is disk only", func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(2 * time.Second):
			}
		}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			prevTimeout := usageNumbersServiceTimeout
			usageNumbersServiceTimeout = 100 * time.Millisecond
			t.Cleanup(func() { usageNumbersServiceTimeout = prevTimeout })
			srv := httptest.NewServer(tt.handler)
			defer srv.Close()
			u, _ := url.Parse(srv.URL)
			lb := pointUsageNumbersAt(t, u.Host)
			got := collectUsageNumbers(false)
			want := localBuilds{full: 1}
			if tt.wantDisk {
				want = localBuilds{disk: 1}
			}
			if got.Source != usageNumbersSourceLocal || *lb != want {
				t.Errorf("source = %q, builds = %+v, want %+v", got.Source, *lb, want)
			}
		})
	}

	t.Run("connection refused is a full build", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		ln.Close()
		lb := pointUsageNumbersAt(t, addr)
		if got := collectUsageNumbers(false); got.Source != usageNumbersSourceLocal || *lb != (localBuilds{full: 1}) {
			t.Errorf("source = %q, builds = %+v, want one full build", got.Source, *lb)
		}
	})

	t.Run("service answers", func(t *testing.T) {
		var hits atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			if r.URL.Path != "/usage/numbers" || r.Header.Get("Authorization") != "Bearer secret" {
				http.Error(w, "bad", http.StatusBadRequest)
				return
			}
			writeJSON(w, http.StatusOK, usageNumbersResponse{Source: usageNumbersSourceService,
				Claude: []claudeUsageNumbers{{Account: "a@example.com", Active: true}}})
		}))
		defer srv.Close()
		u, _ := url.Parse(srv.URL)
		lb := pointUsageNumbersAt(t, u.Host)
		got := collectUsageNumbers(false)
		if got.Source != usageNumbersSourceService || *lb != (localBuilds{}) || len(got.Claude) != 1 {
			t.Errorf("got %+v, builds %+v, want the service answer", got, *lb)
		}
		if collectUsageNumbers(true); hits.Load() != 1 || *lb != (localBuilds{full: 1}) {
			t.Errorf("--local: service hits %d, builds %+v, want 1 and one full build", hits.Load(), *lb)
		}
	})
}

// The local source never fetches a live account with no snapshot slot: it has
// no file to keep backoff in, so each `usage --local` would be a new request.
// The known-accounts pass still runs.
func TestUsageNumbersLocalSkipsLiveWithoutSnapshot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TMPDIR", t.TempDir())
	writeLiveAccount(t, home, "solo@example.com")
	writeSnapshotFixture(t, home, "other", "tok-other", "other@example.com")
	live := noLiveFetch(t)
	known := stubKnownFetch(t, sampleUsageInfo(30))
	prevC, prevG := usageNumbersCodexFetch, usageNumbersGrokFetch
	usageNumbersCodexFetch = func() (*CodexAccountUsage, error) { return nil, os.ErrNotExist }
	usageNumbersGrokFetch = func() (*GrokAccountUsage, error) { return nil, os.ErrNotExist }
	t.Cleanup(func() { usageNumbersCodexFetch, usageNumbersGrokFetch = prevC, prevG })

	resp := newUsageNumbersSource(usageNumbersSourceLocal).build()
	if live.Load() != 0 || known.Load() != 1 {
		t.Fatalf("fetches = live %d known %d, want 0 and 1", live.Load(), known.Load())
	}
	if len(resp.Claude) != 2 {
		t.Fatalf("claude = %+v", resp.Claude)
	}
	l := resp.Claude[0]
	if !l.Active || l.Account != "solo@example.com" || l.Name != "" || !l.Stale || l.Reason != usageNoSnapshotReason || l.FiveHour != nil {
		t.Errorf("live = %+v, want identity only, stale, %q", l, usageNoSnapshotReason)
	}
	if o := resp.Claude[1]; o.FiveHour == nil || o.FiveHour.Pct != 30 {
		t.Errorf("other = %+v, want the known pass's numbers", o)
	}
}

// The disk-only source makes no request of any kind, even with no hub alive
// and every provider cache old.
func TestUsageNumbersDiskOnlyMakesNoFetch(t *testing.T) {
	home := usageNumbersWorld(t, "main", "live@example.com")
	writeSnapshotFixture(t, home, "other", "tok-other", "other@example.com")
	saveAccountCache("main", accountCacheEntry{
		Account: "live@example.com", FetchedAt: time.Now().Add(-time.Hour), Info: sampleUsageInfo(55),
	})
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(accountCachePath("main"), old, old); err != nil {
		t.Fatal(err)
	}
	writeProviderCache(t, codexUsageCachePath(), cachedCodexUsage{FetchedAt: old, Usage: CodexAccountUsage{
		Account: "c@example.com", Info: &CodexUsageInfo{Windows: []codexWindow{{Label: "5h", Pct: 1}}},
	}})
	live := noLiveFetch(t)
	known := stubKnownFetch(t, sampleUsageInfo(1))
	// TestMain's Grok/Codex seams panic, so any provider fetch fails the test.

	resp := newDiskOnlyUsageNumbersSource().build()
	if live.Load() != 0 || known.Load() != 0 {
		t.Fatalf("fetches = live %d known %d, want none", live.Load(), known.Load())
	}
	if resp.Source != usageNumbersSourceLocal {
		t.Errorf("source = %q, want local", resp.Source)
	}
	if l := resp.Claude[0]; l.FiveHour == nil || l.FiveHour.Pct != 55 || !l.Stale {
		t.Errorf("live = %+v, want old disk numbers marked stale", l)
	}
	if len(resp.Codex) != 1 || !resp.Codex[0].Stale {
		t.Errorf("codex = %+v, want the old cache entry, stale", resp.Codex)
	}
}

func TestRenderUsageNumbers(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	reset := now.Add(72 * time.Minute)
	fetched := now.Add(-3 * time.Minute)
	var b strings.Builder
	renderUsageNumbers(&b, usageNumbersResponse{
		Claude: []claudeUsageNumbers{
			{Name: "main", Account: "a@example.com", Active: true,
				FiveHour:     &usageWindowNumbers{Pct: 12, ResetsAt: &reset},
				WeeklyScoped: &usageWindowNumbers{Label: "Fable", Pct: 7},
				Credits:      &usageCreditsNumbers{Enabled: true, Used: 1.5, Limit: 10, Currency: "USD", Pct: 15},
				FetchedAt:    &fetched},
			{Name: "other", Account: "b@example.com", Stale: true, Reason: "rate limited"},
			{Name: "dead", Account: "d@example.com", Stale: true, Expired: true},
		},
		Codex: []providerUsageNumbers{{Account: "c@example.com", Windows: []usageWindowNumbers{{Label: "wk", Pct: 3}}}},
	}, now)
	out := b.String()
	for _, want := range []string{
		"TOOL", "a@example.com (live)", "5h", "12%", "in 1h12m (", "3m ago",
		"wk:Fable", "credits", "used 1.50/10.00 USD", "yes (rate limited)", "yes (auth expired)", "codex",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}
