package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

const (
	localServerHost      = "127.0.0.1"
	localServerPort      = 8765
	localServerTimeout   = 750 * time.Millisecond
	disabledWriteTimeout = 5 * time.Second
)

var (
	// Test seams let local fallback behavior be exercised without a running
	// Tailscale daemon or any network listener.
	localServerRequestAttempt = serverRequestAttempt
	localTailscaleIPv4        = tailscaleIPv4Context
)

type disabledState struct {
	SessionID string
	Disabled  bool
}

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

type serverRequestWithTimeoutFunc func(ServerConfig, string, string, []byte, time.Duration) ([]byte, error)

func putSessionDisabledWithRequest(
	srv ServerConfig,
	pid int,
	sessionID string,
	disabled bool,
	request serverRequestWithTimeoutFunc,
) (disabledState, error) {
	if sessionID == "" {
		return disabledState{}, errors.New("session ID required")
	}
	body, err := json.Marshal(struct {
		Disabled  bool   `json:"disabled"`
		SessionID string `json:"sessionId"`
	}{
		Disabled:  disabled,
		SessionID: sessionID,
	})
	if err != nil {
		return disabledState{}, err
	}
	data, err := request(
		srv,
		fmt.Sprintf("/sessions/%d/disabled", pid),
		http.MethodPut,
		body,
		disabledWriteTimeout,
	)
	if err != nil {
		return disabledState{}, err
	}
	var response struct {
		Disabled  *bool   `json:"disabled"`
		SessionID *string `json:"sessionId"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return disabledState{}, fmt.Errorf("bad response: %w", err)
	}
	if response.Disabled == nil {
		return disabledState{}, errors.New("bad response: missing disabled")
	}
	if response.SessionID == nil || *response.SessionID == "" {
		return disabledState{}, errors.New("bad response: missing sessionId")
	}
	if *response.SessionID != sessionID {
		return disabledState{}, fmt.Errorf(
			"bad response: sessionId mismatch: got %q, want %q",
			*response.SessionID,
			sessionID,
		)
	}
	return disabledState{
		SessionID: *response.SessionID,
		Disabled:  *response.Disabled,
	}, nil
}

func putSessionDisabled(
	srv ServerConfig,
	pid int,
	sessionID string,
	disabled bool,
) (disabledState, error) {
	return putSessionDisabledWithRequest(
		srv,
		pid,
		sessionID,
		disabled,
		serverRequestWithTimeout,
	)
}

func putLocalSessionDisabled(
	srv ServerConfig,
	pid int,
	sessionID string,
	disabled bool,
) (disabledState, error) {
	return putSessionDisabledWithRequest(
		srv,
		pid,
		sessionID,
		disabled,
		localServerRequestWithTimeout,
	)
}

func setSessionDisabled(
	host string,
	pid int,
	sessionID string,
	disabled bool,
) (disabledState, error) {
	srv, err := sessionServerConfig(host)
	if err != nil {
		return disabledState{}, err
	}
	if host == "" {
		return putLocalSessionDisabled(srv, pid, sessionID, disabled)
	}
	return putSessionDisabled(srv, pid, sessionID, disabled)
}

func patchDisabledBySessionID(
	rows []Session,
	sessionID string,
	disabled bool,
) bool {
	if sessionID == "" {
		return false
	}
	for i := range rows {
		if rows[i].SessionID == sessionID {
			rows[i].Disabled = disabled
			return true
		}
	}
	return false
}
