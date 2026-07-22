package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func serverConfigForURL(t *testing.T, rawURL, token string) ServerConfig {
	t.Helper()
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	host, portText, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	return ServerConfig{Host: host, Port: port, Token: token}
}

func TestCollectClientLocalPrefersServerAndFallsBack(t *testing.T) {
	serverRows := []Session{{PID: 1, SessionID: "server", Disabled: true}}
	directRows := []Session{{PID: 2, SessionID: "direct"}}

	got, err := collectClientLocalWith(
		func() ([]Session, error) { return serverRows, nil },
		func() ([]Session, error) {
			t.Fatal("direct collector called after server success")
			return nil, nil
		},
	)
	if err != nil || len(got) != 1 || got[0].SessionID != "server" || !got[0].Disabled {
		t.Fatalf("server result = (%#v, %v)", got, err)
	}

	got, err = collectClientLocalWith(
		func() ([]Session, error) { return nil, errors.New("server down") },
		func() ([]Session, error) { return directRows, nil },
	)
	if err != nil || len(got) != 1 || got[0].SessionID != "direct" || got[0].Disabled {
		t.Fatalf("fallback result = (%#v, %v)", got, err)
	}

	directErr := errors.New("direct collection failed")
	got, err = collectClientLocalWith(
		func() ([]Session, error) { return nil, errors.New("server down") },
		func() ([]Session, error) { return nil, directErr },
	)
	if got != nil || !errors.Is(err, directErr) {
		t.Fatalf("double failure = (%#v, %v), want direct collector error", got, err)
	}
}

func TestSessionServerConfigUsesLocalAndRemoteEndpoints(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	local, err := sessionServerConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if local.Host != "127.0.0.1" || local.Port != 8765 || local.Token == "" {
		t.Fatalf("local config = %#v", local)
	}
	if localServerTimeout != 750*time.Millisecond {
		t.Fatalf("local timeout = %s, want 750ms", localServerTimeout)
	}

	writeServerYAML(t, home, "orca", "10.0.0.8", "9876", "remote-secret")
	remote, err := sessionServerConfig("orca")
	if err != nil {
		t.Fatal(err)
	}
	if remote.Name != "orca" || remote.Host != "10.0.0.8" ||
		remote.Port != 9876 || remote.Token != "remote-secret" {
		t.Fatalf("remote config = %#v", remote)
	}
	if _, err := sessionServerConfig("missing"); err == nil ||
		!strings.Contains(err.Error(), "unknown server: missing") {
		t.Fatalf("missing remote error = %v", err)
	}
}

func TestFetchSessionsFromServerHonorsBoundedTimeout(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer backend.Close()

	started := time.Now()
	_, err := fetchSessionsFromServer(
		serverConfigForURL(t, backend.URL, "secret"),
		25*time.Millisecond,
	)
	if err == nil {
		t.Fatal("slow server request unexpectedly succeeded")
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("bounded request took %s", elapsed)
	}
}

func TestPutSessionDisabledUsesExplicitStateAndIdentity(t *testing.T) {
	type capturedRequest struct {
		method      string
		path        string
		auth        string
		contentType string
		disabled    *bool
		sessionID   *string
		decodeErr   error
	}
	requests := make(chan capturedRequest, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Disabled  *bool   `json:"disabled"`
			SessionID *string `json:"sessionId"`
		}
		decodeErr := json.NewDecoder(r.Body).Decode(&body)
		requests <- capturedRequest{
			method:      r.Method,
			path:        r.URL.Path,
			auth:        r.Header.Get("Authorization"),
			contentType: r.Header.Get("Content-Type"),
			disabled:    body.Disabled,
			sessionID:   body.SessionID,
			decodeErr:   decodeErr,
		}
		fmt.Fprint(w, `{"disabled":true,"sessionId":"session-42"}`)
	}))
	defer backend.Close()

	state, err := putSessionDisabled(
		serverConfigForURL(t, backend.URL, "secret"),
		42,
		"session-42",
		true,
	)
	if err != nil {
		t.Fatal(err)
	}
	got := <-requests
	if got.decodeErr != nil {
		t.Fatal(got.decodeErr)
	}
	if !state.Disabled || state.SessionID != "session-42" ||
		got.method != http.MethodPut || got.path != "/sessions/42/disabled" ||
		got.auth != "Bearer secret" || got.contentType != "application/json" {
		t.Fatalf("state=%#v request=%#v", state, got)
	}
	if got.disabled == nil || !*got.disabled ||
		got.sessionID == nil || *got.sessionID != "session-42" {
		t.Fatalf("request body = %#v", got)
	}
}

func TestPutSessionDisabledRejectsBadResponses(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"malformed JSON", `{`, "bad response:"},
		{"missing disabled", `{"sessionId":"session-42"}`, "bad response: missing disabled"},
		{"missing identity", `{"disabled":true}`, "bad response: missing sessionId"},
		{"empty identity", `{"disabled":true,"sessionId":""}`, "bad response: missing sessionId"},
		{"mismatched identity", `{"disabled":true,"sessionId":"replacement"}`, "bad response: sessionId mismatch"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, tc.body)
			}))
			defer backend.Close()
			_, err := putSessionDisabled(
				serverConfigForURL(t, backend.URL, "secret"),
				42,
				"session-42",
				true,
			)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestPutSessionDisabledRejectsEmptyRequestIdentity(t *testing.T) {
	_, err := putSessionDisabled(ServerConfig{}, 42, "", true)
	if err == nil || err.Error() != "session ID required" {
		t.Fatalf("error = %v", err)
	}
}

func TestPutSessionDisabledPreservesHTTPError(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "session ended", http.StatusNotFound)
	}))
	defer backend.Close()
	_, err := putSessionDisabled(
		serverConfigForURL(t, backend.URL, "secret"),
		42,
		"session-42",
		true,
	)
	if err == nil || !strings.Contains(err.Error(), "HTTP 404: session ended") {
		t.Fatalf("error = %v", err)
	}
}

func TestPatchDisabledBySessionID(t *testing.T) {
	rows := []Session{{SessionID: "one"}, {SessionID: "two"}}
	if !patchDisabledBySessionID(rows, "two", true) {
		t.Fatal("target session was not patched")
	}
	if rows[0].Disabled || !rows[1].Disabled {
		t.Fatalf("rows = %#v", rows)
	}
	if patchDisabledBySessionID(rows, "missing", true) {
		t.Fatal("missing session reported as patched")
	}
	rows = append(rows, Session{})
	if patchDisabledBySessionID(rows, "", true) || rows[2].Disabled {
		t.Fatal("empty session ID must never be patched")
	}
}
