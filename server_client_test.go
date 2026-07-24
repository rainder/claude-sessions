package main

import (
	"context"
	"errors"
	"fmt"
	"io"
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

func stubLocalServerFallback(
	t *testing.T,
	request func(context.Context, ServerConfig, string, string, []byte) ([]byte, bool, error),
	resolve func(context.Context) string,
) {
	t.Helper()
	previousRequest := localServerRequestAttempt
	previousResolve := localTailscaleIPv4
	localServerRequestAttempt = request
	localTailscaleIPv4 = resolve
	t.Cleanup(func() {
		localServerRequestAttempt = previousRequest
		localTailscaleIPv4 = previousResolve
	})
}

func TestLocalServerRequestLoopbackSuccessSkipsTailscale(t *testing.T) {
	var attempts []ServerConfig
	stubLocalServerFallback(t,
		func(_ context.Context, srv ServerConfig, path, method string, body []byte) ([]byte, bool, error) {
			attempts = append(attempts, srv)
			if path != "/sessions" || method != http.MethodGet || body != nil {
				t.Fatalf("request = (%q, %q, %q)", path, method, body)
			}
			return []byte(`{"sessions":[]}`), true, nil
		},
		func(context.Context) string {
			t.Fatal("Tailscale resolved after loopback success")
			return ""
		},
	)

	srv := ServerConfig{Host: localServerHost, Port: localServerPort, Token: "secret"}
	got, err := localServerRequestWithTimeout(srv, "/sessions", http.MethodGet, nil, localServerTimeout)
	if err != nil || string(got) != `{"sessions":[]}` {
		t.Fatalf("request = (%q, %v)", got, err)
	}
	if len(attempts) != 1 || attempts[0] != srv {
		t.Fatalf("attempts = %#v, want only loopback %#v", attempts, srv)
	}
}

func TestLocalServerRequestRetriesTailscaleAfterResponseLessFailure(t *testing.T) {
	loopbackErr := errors.New("connection refused")
	srv := ServerConfig{Host: localServerHost, Port: localServerPort, Token: "secret"}
	var deadlines []time.Time
	var resolvedDeadline time.Time
	var attempts []ServerConfig
	stubLocalServerFallback(t,
		func(ctx context.Context, attempt ServerConfig, path, method string, body []byte) ([]byte, bool, error) {
			deadline, ok := ctx.Deadline()
			if !ok {
				t.Fatal("request context has no deadline")
			}
			deadlines = append(deadlines, deadline)
			attempts = append(attempts, attempt)
			if attempt.Host == localServerHost {
				return nil, false, loopbackErr
			}
			if attempt.Host != "100.64.0.5" || attempt.Port != localServerPort || attempt.Token != srv.Token ||
				path != "/sessions/42/kill" || method != http.MethodPost || string(body) != `{"y":true}` {
				t.Fatalf("fallback request = (%#v, %q, %q, %q)", attempt, path, method, body)
			}
			return []byte(`{"ok":true}`), true, nil
		},
		func(ctx context.Context) string {
			var ok bool
			resolvedDeadline, ok = ctx.Deadline()
			if !ok {
				t.Fatal("resolver context has no deadline")
			}
			return "100.64.0.5"
		},
	)

	got, err := localServerRequestWithTimeout(srv, "/sessions/42/kill", http.MethodPost, []byte(`{"y":true}`), localServerTimeout)
	if err != nil || string(got) != `{"ok":true}` {
		t.Fatalf("request = (%q, %v)", got, err)
	}
	if len(attempts) != 2 || attempts[0].Host != localServerHost || attempts[1].Host != "100.64.0.5" {
		t.Fatalf("attempts = %#v", attempts)
	}
	if len(deadlines) != 2 || !deadlines[0].Equal(resolvedDeadline) || !deadlines[0].Equal(deadlines[1]) {
		t.Fatalf("deadlines = %#v resolver=%s; want one shared operation deadline", deadlines, resolvedDeadline)
	}
}

func TestLocalServerRequestDoesNotRetryAfterResponse(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "HTTP status error", err: errors.New("HTTP 404: session ended")},
		{name: "body read error", err: io.ErrUnexpectedEOF},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubLocalServerFallback(t,
				func(context.Context, ServerConfig, string, string, []byte) ([]byte, bool, error) {
					return nil, true, tc.err
				},
				func(context.Context) string {
					t.Fatal("Tailscale resolved after HTTP response")
					return ""
				},
			)

			_, err := localServerRequestWithTimeout(
				ServerConfig{Host: localServerHost, Port: localServerPort},
				"/sessions",
				http.MethodGet,
				nil,
				localServerTimeout,
			)
			if !errors.Is(err, tc.err) {
				t.Fatalf("error = %v, want %v", err, tc.err)
			}
		})
	}
}

func TestFetchLocalServerSessionsFallsBackToDirectCollectionAfterBothEndpointsFail(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	attempts := 0
	stubLocalServerFallback(t,
		func(context.Context, ServerConfig, string, string, []byte) ([]byte, bool, error) {
			attempts++
			return nil, false, errors.New("unreachable")
		},
		func(context.Context) string { return "100.64.0.5" },
	)

	directRows := []Session{{PID: 1, SessionID: "direct"}}
	got, err := collectClientLocalWith(fetchLocalServerSessions, func() ([]Session, error) {
		return directRows, nil
	})
	if err != nil || len(got) != 1 || got[0].SessionID != "direct" {
		t.Fatalf("result = (%#v, %v)", got, err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want loopback and Tailscale", attempts)
	}
}

func TestServerRequestAttemptReportsBodyReadAsResponseReceived(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		fmt.Fprint(rw, "HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\nx")
		if err := rw.Flush(); err != nil {
			t.Fatal(err)
		}
	}))
	defer backend.Close()

	_, responseReceived, err := serverRequestAttempt(
		context.Background(),
		serverConfigForURL(t, backend.URL, "secret"),
		"/sessions",
		http.MethodGet,
		nil,
	)
	if !responseReceived {
		t.Fatal("responseReceived = false after HTTP headers")
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("error = %v, want unexpected EOF", err)
	}
}
