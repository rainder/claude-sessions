package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFleetLoginURL(t *testing.T) {
	got := fleetLoginURL("ws.example.ts.net", 8765, "a+b/c")
	if got != "http://ws.example.ts.net:8765/fleet?c=a%2Bb%2Fc" {
		t.Fatalf("got %q", got)
	}
	if got := fleetLoginURL("100.1.2.3", 9000, "Ab-_9"); got != "http://100.1.2.3:9000/fleet?c=Ab-_9" {
		t.Fatalf("got %q", got)
	}
}

func TestCmdFleetURLBadFlag(t *testing.T) {
	if code := cmdFleetURL([]string{"--port", "x"}); code != 2 {
		t.Fatalf("code %d", code)
	}
}

// fleetURLRig points cmdFleetURL at an httptest server: a temp HOME holding a
// test token, the base-URL finder and the host lookup stubbed.
func fleetURLRig(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".config", "claude-sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".config", "claude-sessions", "server-token"), []byte("tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	origFind, origHost, origServe := fleetFindServer, fleetTailscaleHost, fleetServeStatus
	t.Cleanup(func() { fleetFindServer, fleetTailscaleHost, fleetServeStatus = origFind, origHost, origServe })
	// No serve route unless a test sets one: never the real tailscale CLI.
	fleetServeStatus = func() ([]byte, error) { return nil, errors.New("no tailscale") }
	fleetFindServer = func(port int, token string) string {
		if token != "tok" {
			t.Errorf("finder got token %q", token)
		}
		return ts.URL
	}
	fleetTailscaleHost = func() string { return "ws.example.ts.net" }
}

func runFleetURL(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	stderr = captureStderr(t, func() {
		stdout = captureStdout(t, func() { code = cmdFleetURL(args) })
	})
	return code, stdout, stderr
}

func TestCmdFleetURLArmsAndPrintsCode(t *testing.T) {
	var calls []string
	fleetURLRig(t, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"code": "Ab-_9xyz", "expiresAt": "2026-10-13T10:10:00Z"})
	})
	code, out, errOut := runFleetURL(t)
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	if len(calls) != 1 || calls[0] != "POST /fleet/login/arm" {
		t.Fatalf("calls %v", calls)
	}
	if out != "http://ws.example.ts.net:8765/fleet?c=Ab-_9xyz\n" {
		t.Fatalf("stdout %q", out)
	}
	if strings.Contains(out+errOut, "tok") {
		t.Fatal("the server token leaked into the output")
	}
	if !strings.Contains(errOut, "Open this link on one device within 10 minutes and press Log in. The code works one time.") {
		t.Fatalf("stderr %q", errOut)
	}
}

func TestCmdFleetURLHostFlag(t *testing.T) {
	fleetURLRig(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"code": "c0de", "expiresAt": "2026-10-13T10:10:00Z"})
	})
	code, out, _ := runFleetURL(t, "--host", "10.0.0.5", "--port", "9000")
	if code != 0 || out != "http://10.0.0.5:9000/fleet?c=c0de\n" {
		t.Fatalf("exit %d stdout %q", code, out)
	}
}

func TestCmdFleetURLRevoke(t *testing.T) {
	var calls []string
	fleetURLRig(t, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	code, out, errOut := runFleetURL(t, "--revoke")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	if len(calls) != 1 || calls[0] != "POST /fleet/login/revoke" {
		t.Fatalf("calls %v", calls)
	}
	if out != "All FLEET page logins are revoked.\n" {
		t.Fatalf("stdout %q", out)
	}
}

func TestCmdFleetURLServerRefuses(t *testing.T) {
	fleetURLRig(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no secret", http.StatusServiceUnavailable)
	})
	if code, out, errOut := runFleetURL(t); code != 1 || out != "" || !strings.Contains(errOut, "503") {
		t.Fatalf("arm 503: exit %d stdout %q stderr %q", code, out, errOut)
	}
	if code, out, _ := runFleetURL(t, "--revoke"); code != 1 || out != "" {
		t.Fatalf("revoke 503: exit %d stdout %q", code, out)
	}
}

func TestCmdFleetURLNoServer(t *testing.T) {
	fleetURLRig(t, func(http.ResponseWriter, *http.Request) {})
	fleetFindServer = func(int, string) string { return "" }
	code, out, errOut := runFleetURL(t)
	if code != 1 || out != "" || !strings.Contains(errOut, "no server answering") {
		t.Fatalf("exit %d stdout %q stderr %q", code, out, errOut)
	}
}

func TestCmdFleetURLNoToken(t *testing.T) {
	fleetURLRig(t, func(http.ResponseWriter, *http.Request) {})
	t.Setenv("HOME", t.TempDir())
	code, out, errOut := runFleetURL(t)
	if code != 1 || out != "" || !strings.Contains(errOut, "no server token") {
		t.Fatalf("exit %d stdout %q stderr %q", code, out, errOut)
	}
}

// fleetServeFixture is `tailscale serve status --json` shaped like a host
// with :443 routed to another app and :8443 to this server on its Tailscale
// address. Invented names and addresses.
const fleetServeFixture = `{"TCP":{"443":{"HTTPS":true},"8443":{"HTTPS":true}},` +
	`"Web":{"ws.example.ts.net:443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:3000"}}},` +
	`"ws.example.ts.net:8443":{"Handlers":{"/":{"Proxy":"http://100.64.0.7:8765"}}}}}`

// fleetServeOne is a config with one https route on :8443 proxying to target.
func fleetServeOne(target, extra string) string {
	return `{"TCP":{"8443":{"HTTPS":true}},"Web":{"h.example.ts.net:8443":{"Handlers":{"/":{"Proxy":"` +
		target + `"}}}}` + extra + `}`
}

func TestFleetServeHTTPS(t *testing.T) {
	ts := fleetRouteTargets("http://100.64.0.7:8765", "ws.example.ts.net.")
	lo := fleetRouteTargets("http://127.0.0.1:8765", "ws.example.ts.net")
	cases := []struct {
		name    string
		status  string
		port    int
		targets []string
		want    string
		funnel  string
	}{
		{"this server on 8443", fleetServeFixture, 8765, ts, "ws.example.ts.net:8443", ""},
		{"443 to another app never matches", fleetServeFixture, 3000, ts, "", ""},
		{"loopback target, server reached on loopback", fleetServeOne("http://127.0.0.1:8765", ""), 8765, lo, "h.example.ts.net:8443", ""},
		{"bare port target is loopback", fleetServeOne("8765", ""), 8765, lo, "h.example.ts.net:8443", ""},
		{"loopback target, server reached on tailscale", fleetServeOne("http://127.0.0.1:8765", ""), 8765, ts, "", ""},
		{"tailscale target, server reached on loopback", fleetServeOne("http://100.64.0.7:8765", ""), 8765, lo, "", ""},
		{"MagicDNS name target", fleetServeOne("http://WS.example.ts.net:8765", ""), 8765, ts, "h.example.ts.net:8443", ""},
		{"another host's 8765", fleetServeOne("http://100.64.0.99:8765", ""), 8765, ts, "", ""},
		{"another host's name", fleetServeOne("http://other.example.ts.net:8765", ""), 8765, ts, "", ""},
		{"443 match drops the port",
			`{"TCP":{"443":{"HTTPS":true}},"Web":{"h.example.ts.net:443":{"Handlers":{"/":{"Proxy":"http://100.64.0.7:8765"}}}}}`,
			8765, ts, "h.example.ts.net", ""},
		{"not https", `{"TCP":{"8443":{"HTTP":true}},"Web":{"h.example.ts.net:8443":{"Handlers":{"/":{"Proxy":"http://100.64.0.7:8765"}}}}}`, 8765, ts, "", ""},
		{"not the root handler", `{"TCP":{"8443":{"HTTPS":true}},"Web":{"h.example.ts.net:8443":{"Handlers":{"/x":{"Proxy":"http://100.64.0.7:8765"}}}}}`, 8765, ts, "", ""},
		{"funnel route skipped", fleetServeOne("http://100.64.0.7:8765", `,"AllowFunnel":{"h.example.ts.net:8443":true}`), 8765, ts, "", "h.example.ts.net:8443"},
		{"funnel off", fleetServeOne("http://100.64.0.7:8765", `,"AllowFunnel":{"h.example.ts.net:8443":false}`), 8765, ts, "h.example.ts.net:8443", ""},
		{"lowest port wins", `{"TCP":{"8443":{"HTTPS":true},"10000":{"HTTPS":true}},"Web":{` +
			`"h.example.ts.net:10000":{"Handlers":{"/":{"Proxy":"http://100.64.0.7:8765"}}},` +
			`"h.example.ts.net:8443":{"Handlers":{"/":{"Proxy":"http://100.64.0.7:8765"}}}}}`, 8765, ts, "h.example.ts.net:8443", ""},
		{"empty config", `{}`, 8765, ts, "", ""},
		{"not json", `nope`, 8765, ts, "", ""},
	}
	for _, c := range cases {
		got, funnel := fleetServeHTTPS([]byte(c.status), c.port, c.targets)
		if got != c.want || strings.Join(funnel, ",") != c.funnel {
			t.Errorf("%s: %q funnel %v, want %q funnel %q", c.name, got, funnel, c.want, c.funnel)
		}
	}
}

func TestFleetRouteTargets(t *testing.T) {
	if got := fleetRouteTargets("http://100.64.0.7:8765", "ws.example.ts.net."); strings.Join(got, ",") != "100.64.0.7,ws.example.ts.net" {
		t.Errorf("tailscale: %v", got)
	}
	if got := fleetRouteTargets("http://127.0.0.1:8765", "ws.example.ts.net"); strings.Join(got, ",") != "127.0.0.1,::1,localhost" {
		t.Errorf("loopback: %v", got)
	}
	if got := fleetRouteTargets("http://100.64.0.7:8765", "100.64.0.7"); strings.Join(got, ",") != "100.64.0.7" {
		t.Errorf("no MagicDNS name: %v", got)
	}
}

const fleetHTTPNote = "no tailscale https route to this server; printing the http link. " +
	"A device that logged in over https must log in over https again."

// The rig's server is an httptest server, so fleet-url reaches it on
// loopback: only a loopback proxy target is a route to it.
func TestCmdFleetURLPrefersServeHTTPS(t *testing.T) {
	arm := func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"code": "c0de", "expiresAt": "2026-10-13T10:10:00Z"})
	}
	route := func(cfg string) func() ([]byte, error) { return func() ([]byte, error) { return []byte(cfg), nil } }
	cases := []struct {
		name    string
		serve   func() ([]byte, error)
		args    []string
		want    string
		note    bool
		warning string
	}{
		{"https route", route(fleetServeOne("http://127.0.0.1:8765", "")), nil,
			"https://h.example.ts.net:8443/fleet?c=c0de\n", false, ""},
		{"route to a different server", route(fleetServeFixture), nil,
			"http://ws.example.ts.net:8765/fleet?c=c0de\n", true, ""},
		{"funnel route", route(fleetServeOne("http://127.0.0.1:8765", `,"AllowFunnel":{"h.example.ts.net:8443":true}`)), nil,
			"http://ws.example.ts.net:8765/fleet?c=c0de\n", true, "skipped the https route h.example.ts.net:8443: it is open to Funnel"},
		{"--http forces http", route(fleetServeOne("http://127.0.0.1:8765", "")), []string{"--http"},
			"http://ws.example.ts.net:8765/fleet?c=c0de\n", false, ""},
		{"--host keeps http", route(fleetServeOne("http://127.0.0.1:8765", "")), []string{"--host", "10.0.0.5"},
			"http://10.0.0.5:8765/fleet?c=c0de\n", false, ""},
		{"serve status fails", func() ([]byte, error) { return nil, errors.New("timeout") }, nil,
			"http://ws.example.ts.net:8765/fleet?c=c0de\n", true, ""},
		{"no route for this port", route(fleetServeOne("http://127.0.0.1:8765", "")), []string{"--port", "9000"},
			"http://ws.example.ts.net:9000/fleet?c=c0de\n", true, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fleetURLRig(t, arm)
			fleetServeStatus = c.serve
			code, out, errOut := runFleetURL(t, c.args...)
			if code != 0 || out != c.want {
				t.Fatalf("exit %d stdout %q stderr %q", code, out, errOut)
			}
			if strings.Contains(errOut, fleetHTTPNote) != c.note {
				t.Errorf("http note shown = %v, want %v: %q", !c.note, c.note, errOut)
			}
			if c.warning != "" && !strings.Contains(errOut, c.warning) {
				t.Errorf("stderr %q has no %q", errOut, c.warning)
			}
		})
	}
}
