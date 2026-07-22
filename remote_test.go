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

func TestFetchRemoteCompatibilityWithMissingAndPartialHostUsage(t *testing.T) {
	missing := fetchRemoteFixture(t, `{"sessions":[]}`)
	if missing.HostUsage.CPUPercent != nil || missing.HostUsage.MemoryPercent != nil {
		t.Fatalf("missing hostUsage decoded as %#v", missing.HostUsage)
	}

	partial := fetchRemoteFixture(t, `{"hostUsage":{"cpuPercent":0},"sessions":[]}`)
	assertFloatPtr(t, partial.HostUsage.CPUPercent, floatPtr(0))
	if partial.HostUsage.MemoryPercent != nil {
		t.Fatalf("partial memory = %v, want nil", partial.HostUsage.MemoryPercent)
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
