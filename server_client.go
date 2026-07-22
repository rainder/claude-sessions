package main

import (
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
	var response struct {
		Sessions []Session `json:"sessions"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return nil, fmt.Errorf("bad response: %w", err)
	}
	return response.Sessions, nil
}

func fetchLocalServerSessions() ([]Session, error) {
	srv, err := sessionServerConfig("")
	if err != nil {
		return nil, err
	}
	return fetchSessionsFromServer(srv, localServerTimeout)
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

func putSessionDisabled(
	srv ServerConfig,
	pid int,
	sessionID string,
	disabled bool,
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
	data, err := serverRequestWithTimeout(
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
