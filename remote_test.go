package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"
)

func TestFetchRemoteDecodesHostUsageAndTagsSessions(t *testing.T) {
	result := fetchRemoteFixture(t, `{
		"hostUsage":{"cpuPercent":25.5,"memoryPercent":75},
		"sessions":[{"pid":42,"sessionId":"remote-42","cwd":"/srv/app","disabled":true}]
	}`)
	assertFloatPtr(t, result.HostUsage.CPUPercent, floatPtr(25.5))
	assertFloatPtr(t, result.HostUsage.MemoryPercent, floatPtr(75))
	if len(result.Sessions) != 1 || result.Sessions[0].Host != "alias" || !result.Sessions[0].Disabled {
		t.Fatalf("sessions = %#v", result.Sessions)
	}
}

func TestFetchRemoteDecodesNestedLoadAverage(t *testing.T) {
	// A valid zero member must survive omitempty, so five-minute stays 0 rather
	// than decoding to nil.
	result := fetchRemoteFixture(t, `{
		"hostUsage":{"cpuPercent":25.5,"memoryPercent":75,"loadAverage":{"oneMinute":1.24,"fiveMinutes":0,"fifteenMinutes":0.72}},
		"sessions":[]
	}`)
	assertLoadAveragePtr(t, result.HostUsage.Load, &LoadAverage{
		OneMinute:      floatPtr(1.24),
		FiveMinutes:    floatPtr(0),
		FifteenMinutes: floatPtr(0.72),
	})
	want := bold("  1.2") + " " + dim("  0.0") + " " + dim("  0.7")
	if got := formatHostLoad(result.HostUsage.Load, result.HostUsage.NumCPU); got != want {
		t.Fatalf("formatHostLoad = %q, want %q", got, want)
	}
}

func TestFetchRemoteCompatibilityWithMissingAndPartialHostUsage(t *testing.T) {
	missing := fetchRemoteFixture(t, `{"sessions":[]}`)
	if missing.HostUsage.CPUPercent != nil || missing.HostUsage.MemoryPercent != nil {
		t.Fatalf("missing hostUsage decoded as %#v", missing.HostUsage)
	}
	if missing.HostUsage.Load != nil {
		t.Fatalf("missing loadAverage decoded as %#v", missing.HostUsage.Load)
	}

	partial := fetchRemoteFixture(t, `{"hostUsage":{"cpuPercent":0},"sessions":[]}`)
	assertFloatPtr(t, partial.HostUsage.CPUPercent, floatPtr(0))
	if partial.HostUsage.MemoryPercent != nil {
		t.Fatalf("partial memory = %v, want nil", partial.HostUsage.MemoryPercent)
	}
	// An old server without loadAverage leaves Load nil.
	if partial.HostUsage.Load != nil {
		t.Fatalf("partial loadAverage = %#v, want nil", partial.HostUsage.Load)
	}

	// A partial nested load decodes as partial data (one member present), but the
	// triple is atomic: rendering must show unavailable, never a half-populated
	// load line.
	partialLoad := fetchRemoteFixture(t, `{"hostUsage":{"loadAverage":{"oneMinute":1.24}},"sessions":[]}`)
	if partialLoad.HostUsage.Load == nil {
		t.Fatal("partial loadAverage decoded as nil, want partial object")
	}
	assertFloatPtr(t, partialLoad.HostUsage.Load.OneMinute, floatPtr(1.24))
	if partialLoad.HostUsage.Load.FiveMinutes != nil || partialLoad.HostUsage.Load.FifteenMinutes != nil {
		t.Fatalf("partial load unexpectedly populated: %#v", partialLoad.HostUsage.Load)
	}
	unavailable := colorize("", "   --") + " " + colorize("", "   --") + " " + colorize("", "   --")
	if got := formatHostLoad(partialLoad.HostUsage.Load, partialLoad.HostUsage.NumCPU); got != unavailable {
		t.Fatalf("formatHostLoad = %q, want %q", got, unavailable)
	}
}

func TestFetchRemoteDecodesOptionalUsage(t *testing.T) {
	// Build the body exactly as the server does — marshal an AccountUsage — so
	// the test tracks the real serialization (UsageInfo has no JSON tags, so its
	// wire keys are the Go field names) instead of a hand-written shape.
	usage := AccountUsage{Account: "bot@ci.com", Info: &UsageInfo{FiveHour: usageBucket{Pct: 42}}}
	payload, err := json.Marshal(map[string]any{"sessions": []Session{}, "usage": usage})
	if err != nil {
		t.Fatal(err)
	}
	got := fetchRemoteFixture(t, string(payload))
	if got.Usage == nil {
		t.Fatal("usage decoded as nil, want populated")
	}
	if got.Usage.Account != "bot@ci.com" {
		t.Fatalf("account = %q, want bot@ci.com", got.Usage.Account)
	}
	if got.Usage.Info == nil || got.Usage.Info.FiveHour.Pct != 42 {
		t.Fatalf("usage.info wrong: %#v", got.Usage.Info)
	}

	// An older server that omits "usage" leaves it nil — the client keeps working.
	if old := fetchRemoteFixture(t, `{"sessions":[]}`); old.Usage != nil {
		t.Fatalf("missing usage decoded as %#v, want nil", old.Usage)
	}
}

func TestFetchRemoteDecodesOptionalCodexUsage(t *testing.T) {
	// Marshal a CodexAccountUsage exactly as the server does so the test tracks
	// the real "codex_usage" serialization.
	codex := CodexAccountUsage{Account: "bot@ci.com", Info: &CodexUsageInfo{
		Plan:    "pro",
		Windows: []codexWindow{{Label: "wk", Pct: 88}},
	}}
	payload, err := json.Marshal(map[string]any{"sessions": []Session{}, "codex_usage": codex})
	if err != nil {
		t.Fatal(err)
	}
	got := fetchRemoteFixture(t, string(payload))
	if got.CodexUsage == nil {
		t.Fatal("codex_usage decoded as nil, want populated")
	}
	if got.CodexUsage.Account != "bot@ci.com" {
		t.Fatalf("account = %q, want bot@ci.com", got.CodexUsage.Account)
	}
	if got.CodexUsage.Info == nil || len(got.CodexUsage.Info.Windows) != 1 || got.CodexUsage.Info.Windows[0].Pct != 88 {
		t.Fatalf("codex_usage.info wrong: %#v", got.CodexUsage.Info)
	}

	// An older server (or one with no Codex auth) omits the key → nil, no error.
	if old := fetchRemoteFixture(t, `{"sessions":[]}`); old.CodexUsage != nil {
		t.Fatalf("missing codex_usage decoded as %#v, want nil", old.CodexUsage)
	}
}

func fetchRemoteFixture(t *testing.T, body string) RemoteResult {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer token" {
			http.Error(w, "bad auth", http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, body)
	}))
	defer server.Close()
	u, err := url.Parse(server.URL)
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
	result := FetchRemote(ServerConfig{Name: "alias", Host: host, Port: port, Token: "token"})
	if result.Error != "" {
		t.Fatalf("FetchRemote error = %q", result.Error)
	}
	return result
}

func TestRemoteHubSnapshotDoesNotAliasHubState(t *testing.T) {
	h := &RemoteHub{
		results: []RemoteResult{{
			Name: "alias",
			Sessions: []Session{{
				PID:       42,
				SessionID: "session-42",
			}},
		}},
	}

	// A snapshot taken before a store must not observe the later result.
	before := h.Snapshot()
	h.storeRemoteResult(0, RemoteResult{
		Name:     "alias",
		Sessions: []Session{{PID: 42, SessionID: "session-42", Disabled: true}},
	})
	if before[0].Sessions[0].Disabled {
		t.Fatal("hub store retroactively changed prior snapshot")
	}

	// Mutating a returned snapshot must not touch hub-owned state.
	callerCopy := h.Snapshot()
	if !callerCopy[0].Sessions[0].Disabled {
		t.Fatal("snapshot missing stored state")
	}
	callerCopy[0].Sessions[0].Disabled = false
	if !h.Snapshot()[0].Sessions[0].Disabled {
		t.Fatal("caller mutation changed hub-owned session state")
	}
}

func TestRemoteHubSnapshotAndStoreAreRaceFree(t *testing.T) {
	h := &RemoteHub{
		results: []RemoteResult{{
			Name: "alias",
			Sessions: []Session{{
				PID:       42,
				SessionID: "session-42",
			}},
		}},
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		<-start
		for range 200 {
			snapshot := h.Snapshot()
			if len(snapshot) != 0 && len(snapshot[0].Sessions) != 0 {
				_ = snapshot[0].Sessions[0].Disabled
			}
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		for i := range 200 {
			h.storeRemoteResult(0, RemoteResult{
				Name: "alias",
				Sessions: []Session{{
					PID:       42,
					SessionID: "session-42",
					Disabled:  i%2 == 0,
				}},
			})
		}
	}()

	close(start)
	wg.Wait()
}
