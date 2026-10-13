package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func fleetTestServer(t *testing.T, sessions []Session) *server {
	t.Helper()
	return &server{
		token:   "tok",
		collect: func() ([]Session, error) { return sessions, nil },
		fleet:   fleetDeps{dir: t.TempDir()},
	}
}

func fleetMux(s *server) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /sessions", s.sessions)
	registerFleetRoutes(mux, s)
	return mux
}

func TestFleetAuthBearerAndCookie(t *testing.T) {
	s := fleetTestServer(t, nil)
	r := httptest.NewRequest("GET", "/fleet/state", nil)
	if s.fleetAuth(r) != fleetAuthNone {
		t.Fatal("no credentials must be none")
	}
	r.Header.Set("Authorization", "Bearer tok")
	if s.fleetAuth(r) != fleetAuthBearer {
		t.Fatal("bearer header")
	}
	r = httptest.NewRequest("GET", "/fleet/state", nil)
	r.AddCookie(&http.Cookie{Name: "cs_fleet", Value: "tok"})
	if s.fleetAuth(r) != fleetAuthCookie {
		t.Fatal("cookie")
	}
	r = httptest.NewRequest("GET", "/fleet/state", nil)
	r.AddCookie(&http.Cookie{Name: "cs_fleet", Value: "wrong"})
	if s.fleetAuth(r) != fleetAuthNone {
		t.Fatal("wrong cookie must be none")
	}
}

func TestFleetCookieRejectedOnSessions(t *testing.T) {
	s := fleetTestServer(t, nil)
	r := httptest.NewRequest("GET", "/sessions", nil)
	r.AddCookie(&http.Cookie{Name: "cs_fleet", Value: "tok"})
	w := httptest.NewRecorder()
	fleetMux(s).ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("GET /sessions with only the fleet cookie: %d, want 401", w.Code)
	}
}

func TestFleetLoginSetsCookieAndLeavesURL(t *testing.T) {
	s := fleetTestServer(t, nil)
	w := httptest.NewRecorder()
	fleetMux(s).ServeHTTP(w, httptest.NewRequest("GET", "/fleet?t=tok", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("code %d, want 200 (HTML hop, not a 303: see Review Focus 3)", w.Code)
	}
	c := w.Result().Cookies()
	if len(c) != 1 || c[0].Name != "cs_fleet" || c[0].Value != "tok" || c[0].Path != "/fleet" ||
		!c[0].HttpOnly || c[0].SameSite != http.SameSiteStrictMode || c[0].Secure || c[0].MaxAge != 2592000 {
		t.Fatalf("cookie = %+v", c)
	}
	body := w.Body.String()
	if !strings.Contains(body, `location.replace("/fleet")`) || strings.Contains(body, "tok") {
		t.Fatalf("login hop must redirect by script and never echo the token: %q", body)
	}
	if got := w.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Fatalf("login hop Referrer-Policy = %q, want no-referrer", got)
	}
}

func TestFleetLoginWrongToken(t *testing.T) {
	s := fleetTestServer(t, nil)
	w := httptest.NewRecorder()
	fleetMux(s).ServeHTTP(w, httptest.NewRequest("GET", "/fleet?t=nope", nil))
	if w.Code != http.StatusUnauthorized || len(w.Result().Cookies()) != 0 {
		t.Fatalf("code %d cookies %v", w.Code, w.Result().Cookies())
	}
}

func TestFleetPageNeedsAuthAndSetsHeaders(t *testing.T) {
	s := fleetTestServer(t, nil)
	w := httptest.NewRecorder()
	fleetMux(s).ServeHTTP(w, httptest.NewRequest("GET", "/fleet", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("no auth: %d", w.Code)
	}
	r := httptest.NewRequest("GET", "/fleet", nil)
	r.AddCookie(&http.Cookie{Name: "cs_fleet", Value: "tok"})
	w = httptest.NewRecorder()
	fleetMux(s).ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("page: %d %q", w.Code, w.Header().Get("Content-Type"))
	}
	if got := w.Header().Get("Referrer-Policy"); got != "same-origin" {
		t.Fatalf("Referrer-Policy = %q, want same-origin (no-referrer nulls Origin on POST)", got)
	}
	if !strings.Contains(w.Header().Get("Content-Security-Policy"), "default-src 'none'") {
		t.Fatalf("CSP = %q", w.Header().Get("Content-Security-Policy"))
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("page must be no-store")
	}
}

func TestFindFleet(t *testing.T) {
	one := []Session{{PID: 1, Name: "FLEET", SessionID: "a"}, {PID: 2, Name: "DR-5042 FLEET Ticket Pickup"}}
	if got, reason := findFleet(one); reason != "" || got.PID != 1 {
		t.Fatalf("one: %+v %q", got, reason)
	}
	if _, reason := findFleet(nil); reason != "not running" {
		t.Fatalf("zero: %q", reason)
	}
	two := []Session{{PID: 1, Name: "FLEET"}, {PID: 3, Name: "FLEET"}}
	if _, reason := findFleet(two); reason != "ambiguous" {
		t.Fatalf("two: %q", reason)
	}
}

func TestFleetStatus(t *testing.T) {
	cases := map[string]Session{
		"waiting": {Status: "busy", WaitingFor: "permission prompt"},
		"busy":    {Status: "busy"},
		"idle":    {Status: "shell"},
	}
	for want, sess := range cases {
		if got := fleetStatus(sess); got != want {
			t.Errorf("%+v: %q want %q", sess, got, want)
		}
	}
	if got := fleetStatus(Session{Status: "idle"}); got != "idle" {
		t.Errorf("idle: %q", got)
	}
}

func TestFleetUsageAliasUsesFleetAuth(t *testing.T) {
	s := fleetTestServer(t, nil) // s.numbers nil → 503 once authed
	w := httptest.NewRecorder()
	fleetMux(s).ServeHTTP(w, httptest.NewRequest("GET", "/fleet/usage", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("no auth: %d", w.Code)
	}
	r := httptest.NewRequest("GET", "/fleet/usage", nil)
	r.AddCookie(&http.Cookie{Name: "cs_fleet", Value: "tok"})
	w = httptest.NewRecorder()
	fleetMux(s).ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("cookie auth reaches the usage body: %d, want 503 with nil source", w.Code)
	}
}
