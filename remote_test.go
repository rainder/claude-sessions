package main

import (
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

func TestRemoteHubPatchDisabledProtectsOnlyPreWriteFetchGeneration(t *testing.T) {
	h := &RemoteHub{
		fetchGeneration: 4,
		results: []RemoteResult{{
			Name:     "alias",
			Sessions: []Session{{PID: 42, SessionID: "session-42"}},
		}},
	}

	h.PatchDisabled("alias", "session-42", true)
	snapshot := h.Snapshot()
	if !snapshot[0].Sessions[0].Disabled {
		t.Fatal("immediate remote patch was not visible")
	}

	h.storeRemoteResult(0, 4, RemoteResult{
		Name:     "alias",
		Sessions: []Session{{PID: 42, SessionID: "session-42", Disabled: false}},
	})
	snapshot = h.Snapshot()
	if !snapshot[0].Sessions[0].Disabled {
		t.Fatal("pre-write fetch overwrote pending disabled state")
	}
	if len(h.pendingDisabled) != 1 {
		t.Fatalf("pending overrides = %d, want 1", len(h.pendingDisabled))
	}

	h.storeRemoteResult(0, 5, RemoteResult{
		Name:     "alias",
		Sessions: []Session{{PID: 42, SessionID: "session-42", Disabled: true}},
	})
	if len(h.pendingDisabled) != 0 {
		t.Fatalf("post-write authoritative fetch did not clear override: %#v", h.pendingDisabled)
	}
}

func TestRemoteHubNewerAuthoritativeWriteSupersedesPendingState(t *testing.T) {
	h := &RemoteHub{
		fetchGeneration: 8,
		results: []RemoteResult{{
			Name:     "alias",
			Sessions: []Session{{PID: 42, SessionID: "session-42"}},
		}},
	}
	h.PatchDisabled("alias", "session-42", true)

	h.storeRemoteResult(0, 8, RemoteResult{
		Name:     "alias",
		Sessions: []Session{{PID: 42, SessionID: "session-42", Disabled: false}},
	})
	if !h.Snapshot()[0].Sessions[0].Disabled {
		t.Fatal("pre-write fetch was not fenced")
	}

	h.storeRemoteResult(0, 9, RemoteResult{
		Name:     "alias",
		Sessions: []Session{{PID: 42, SessionID: "session-42", Disabled: false}},
	})
	snapshot := h.Snapshot()
	if snapshot[0].Sessions[0].Disabled {
		t.Fatal("later authoritative write did not supersede pending state")
	}
	if len(h.pendingDisabled) != 0 {
		t.Fatalf("superseded override was not cleared: %#v", h.pendingDisabled)
	}
}

func TestRemoteHubPendingDisabledSurvivesErrorAndClearsWhenSessionEnds(t *testing.T) {
	h := &RemoteHub{
		fetchGeneration: 2,
		results: []RemoteResult{{
			Name:     "alias",
			Sessions: []Session{{PID: 42, SessionID: "session-42"}},
		}},
	}
	h.PatchDisabled("alias", "session-42", true)
	h.storeRemoteResult(0, 3, RemoteResult{Name: "alias", Error: "timeout"})
	if len(h.pendingDisabled) != 1 {
		t.Fatal("remote error cleared pending override")
	}
	h.storeRemoteResult(0, 4, RemoteResult{Name: "alias", Sessions: nil})
	if len(h.pendingDisabled) != 0 {
		t.Fatal("successful post-write absent-session response did not clear override")
	}
}

func TestRemoteHubPendingDisabledIsScopedByHost(t *testing.T) {
	h := &RemoteHub{
		results: []RemoteResult{
			{Name: "orca", Sessions: []Session{{PID: 42, SessionID: "shared-id"}}},
			{Name: "beluga", Sessions: []Session{{PID: 42, SessionID: "shared-id"}}},
		},
	}
	h.PatchDisabled("orca", "shared-id", true)
	snapshot := h.Snapshot()
	if !snapshot[0].Sessions[0].Disabled {
		t.Fatal("target host was not patched")
	}
	if snapshot[1].Sessions[0].Disabled {
		t.Fatal("same session ID on another host was patched")
	}
	if len(h.pendingDisabled) != 1 {
		t.Fatalf("pending overrides = %#v", h.pendingDisabled)
	}

	h.PatchDisabled("orca", "", true)
	if len(h.pendingDisabled) != 1 {
		t.Fatalf("empty session ID created override: %#v", h.pendingDisabled)
	}
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

	beforePatch := h.Snapshot()
	h.PatchDisabled("alias", "session-42", true)
	if beforePatch[0].Sessions[0].Disabled {
		t.Fatal("hub patch retroactively changed prior snapshot")
	}

	callerCopy := h.Snapshot()
	callerCopy[0].Sessions[0].Disabled = false
	if !h.Snapshot()[0].Sessions[0].Disabled {
		t.Fatal("caller mutation changed hub-owned session state")
	}
}

func TestRemoteHubSnapshotPatchAndStoreAreRaceFree(t *testing.T) {
	h := &RemoteHub{
		fetchGeneration: 1,
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
	wg.Add(3)

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
			h.PatchDisabled("alias", "session-42", i%2 == 0)
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		for i := range 200 {
			h.storeRemoteResult(0, 1, RemoteResult{
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
