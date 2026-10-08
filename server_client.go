package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	localServerHost    = "127.0.0.1"
	localServerPort    = 8765
	localServerTimeout = 750 * time.Millisecond
	// listSessionsServerTimeout is the budget for one GET /sessions from
	// list-sessions. The TUI's 750ms budget is a paint tick. This command
	// can wait for one collect inside the long-lived service. A miss falls
	// back to a local collect that does not scan cost logs.
	listSessionsServerTimeout = 15 * time.Second
)

var (
	// Test seams let local fallback behavior be exercised without a running
	// Tailscale daemon or any network listener.
	localServerRequestAttempt = serverRequestAttempt
	localTailscaleIPv4        = tailscaleIPv4Context
)

func sessionServerConfig(host string) (ServerConfig, error) {
	if host != "" {
		srv, ok := LookupServer(host)
		if !ok {
			return ServerConfig{}, fmt.Errorf("unknown server: %s", host)
		}
		return srv, nil
	}
	token, err := loadOrCreateToken()
	if err != nil {
		return ServerConfig{}, err
	}
	return ServerConfig{
		Host:  localServerHost,
		Port:  localServerPort,
		Token: token,
	}, nil
}

// localServerRequestWithTimeout tries loopback first. It falls back to this
// host's Tailscale IPv4 only when the loopback transport did not receive an HTTP
// response, and both attempts share one operation deadline.
func localServerRequestWithTimeout(srv ServerConfig, path, method string, body []byte, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	data, responseReceived, err := localServerRequestAttempt(ctx, srv, path, method, body)
	if err == nil || responseReceived {
		return data, err
	}

	tailscaleHost := localTailscaleIPv4(ctx)
	if tailscaleHost == "" {
		return data, err
	}
	fallback := srv
	fallback.Host = tailscaleHost
	data, _, err = localServerRequestAttempt(ctx, fallback, path, method, body)
	return data, err
}

func parseServerSessions(data []byte) ([]Session, error) {
	var response struct {
		Sessions []Session `json:"sessions"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return nil, fmt.Errorf("bad response: %w", err)
	}
	return response.Sessions, nil
}

func fetchSessionsFromServer(
	srv ServerConfig,
	timeout time.Duration,
) ([]Session, error) {
	data, err := serverRequestWithTimeout(
		srv,
		"/sessions",
		http.MethodGet,
		nil,
		timeout,
	)
	if err != nil {
		return nil, err
	}
	return parseServerSessions(data)
}

func fetchLocalServerSessions() ([]Session, error) {
	srv, err := sessionServerConfig("")
	if err != nil {
		return nil, err
	}
	data, err := localServerRequestWithTimeout(
		srv,
		"/sessions",
		http.MethodGet,
		nil,
		localServerTimeout,
	)
	if err != nil {
		return nil, err
	}
	return parseServerSessions(data)
}

func collectClientLocalWith(
	serverFetch, directCollect func() ([]Session, error),
) ([]Session, error) {
	if sessions, err := serverFetch(); err == nil {
		return sessions, nil
	}
	return directCollect()
}

func collectClientLocal() ([]Session, error) {
	return collectClientLocalWith(fetchLocalServerSessions, CollectLocal)
}

// listSessionsFromService is what list-sessions calls for this host's rows.
// Tests replace it. A real call would read the developer's own service and
// hide the temp-home session files the command tests set up. TestMain
// installs a miss so those tests stay on the local fallback.
var listSessionsFromService = fetchListSessionsFromService

// listSessionsServiceConfig reads the existing server token. It does not
// create one: a missing token means there is no service to ask.
var listSessionsServiceConfig = func() (ServerConfig, error) {
	tok, err := readServerToken()
	if err != nil {
		return ServerConfig{}, err
	}
	return ServerConfig{Host: localServerHost, Port: localServerPort, Token: tok}, nil
}

// listSessionsAttempt is one GET /sessions. status is the HTTP status when
// a response arrived, and 0 when none did.
var listSessionsAttempt = doListSessionsAttempt

// listSessionsTailscaleIPv4 resolves this host's Tailscale address. Tests
// replace it so the 404 retry does not run the real tailscale binary.
var listSessionsTailscaleIPv4 = tailscaleIPv4Context

func doListSessionsAttempt(ctx context.Context, srv ServerConfig) ([]byte, int, error) {
	u := fmt.Sprintf("http://%s/sessions", hostPort(srv.Host, srv.Port))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+srv.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if resp.StatusCode != http.StatusOK {
		return data, resp.StatusCode, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return data, resp.StatusCode, nil
}

// fetchListSessionsFromService asks the local service for this host's rows.
// Loopback is first. A 404 there is the paste-only listener a
// --bind tailscale service leaves on 127.0.0.1, so that status retries on
// this host's Tailscale address. The usage command uses the same rule.
// A timeout does not retry: the deadline is already spent.
//
// ok is false when no usable answer comes back (no token, unreachable,
// 401, 404 on both, timeout, 5xx, or a body that is not JSON). The caller
// then lists this host without reading cost logs.
func fetchListSessionsFromService() (sessions []Session, usage HostUsage, ok bool) {
	srv, err := listSessionsServiceConfig()
	if err != nil {
		return nil, HostUsage{}, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), listSessionsServerTimeout)
	defer cancel()
	data, status, err := listSessionsAttempt(ctx, srv)
	if err != nil && (status == 0 || status == http.StatusNotFound) && !isTimeoutErr(err) {
		if ts := listSessionsTailscaleIPv4(ctx); ts != "" && ts != srv.Host {
			srv.Host = ts
			data, status, err = listSessionsAttempt(ctx, srv)
		}
	}
	if err != nil {
		return nil, HostUsage{}, false
	}
	var resp struct {
		HostUsage HostUsage `json:"hostUsage"`
		Sessions  []Session `json:"sessions"`
	}
	if json.Unmarshal(data, &resp) != nil {
		return nil, HostUsage{}, false
	}
	if resp.Sessions == nil {
		resp.Sessions = []Session{}
	}
	return resp.Sessions, resp.HostUsage, true
}
