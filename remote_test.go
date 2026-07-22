package main

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
)

func TestFetchRemoteDecodesHostUsageAndTagsSessions(t *testing.T) {
	result := fetchRemoteFixture(t, `{
		"hostUsage":{"cpuPercent":25.5,"memoryPercent":75},
		"sessions":[{"pid":42,"cwd":"/srv/app"}]
	}`)
	assertFloatPtr(t, result.HostUsage.CPUPercent, floatPtr(25.5))
	assertFloatPtr(t, result.HostUsage.MemoryPercent, floatPtr(75))
	if len(result.Sessions) != 1 || result.Sessions[0].Host != "alias" {
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
