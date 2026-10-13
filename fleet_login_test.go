package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

var fleetCodeRe = regexp.MustCompile(`^[A-Za-z0-9_-]{22}$`)

// armFleetCode posts /fleet/login/arm with the bearer token and returns the code.
func armFleetCode(t *testing.T, s *server) string {
	t.Helper()
	r := httptest.NewRequest("POST", "/fleet/login/arm", nil)
	r.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	fleetMux(s).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("arm: %d %s", w.Code, w.Body.String())
	}
	var out struct {
		Code      string `json:"code"`
		ExpiresAt string `json:"expiresAt"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !fleetCodeRe.MatchString(out.Code) {
		t.Fatalf("code %q is not 16 bytes of unpadded base64url", out.Code)
	}
	if _, err := time.Parse(time.RFC3339, out.ExpiresAt); err != nil {
		t.Fatalf("expiresAt %q: %v", out.ExpiresAt, err)
	}
	return out.Code
}

// postFleetLogin posts the login form the way the GET page's button does,
// with origin as the Origin header ("" sends none).
func postFleetLogin(s *server, code, origin string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/fleet/login", strings.NewReader(url.Values{"c": {code}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	w := httptest.NewRecorder()
	fleetMux(s).ServeHTTP(w, r)
	return w
}

// redeemFleetCode is a same-origin login POST (httptest's Host is example.com).
func redeemFleetCode(s *server, code string) *httptest.ResponseRecorder {
	return postFleetLogin(s, code, "http://example.com")
}

func wantFleetLoginOK(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/fleet" {
		t.Fatalf("login: %d Location %q, want 303 to /fleet", w.Code, w.Header().Get("Location"))
	}
}

func wantFleetLoginRefused(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusUnauthorized || len(w.Result().Cookies()) != 0 {
		t.Fatalf("code %d cookies %v, want 401 and no cookie", w.Code, w.Result().Cookies())
	}
	if !strings.Contains(w.Body.String(), "This login link is used or expired. Run claude-sessions fleet-url on the host again.") {
		t.Fatalf("body %q", w.Body.String())
	}
}

func TestFleetLoginArmNeedsBearer(t *testing.T) {
	s := fleetTestServer(t, nil)
	w := httptest.NewRecorder()
	fleetMux(s).ServeHTTP(w, httptest.NewRequest("POST", "/fleet/login/arm", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("no auth: %d", w.Code)
	}
	// The cookie opens the page, not the door to more logins.
	r := httptest.NewRequest("POST", "/fleet/login/arm", nil)
	r.AddCookie(&http.Cookie{Name: "cs_fleet", Value: "sec"})
	w = httptest.NewRecorder()
	fleetMux(s).ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("cookie: %d, want 401", w.Code)
	}
	if len(s.fleet.codes) != 0 {
		t.Fatalf("a refused arm left %d codes", len(s.fleet.codes))
	}
}

func TestFleetLoginArmReturnsCodeAndExpiry(t *testing.T) {
	s := fleetTestServer(t, nil)
	now := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	s.fleet.now = func() time.Time { return now }
	r := httptest.NewRequest("POST", "/fleet/login/arm", nil)
	r.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	fleetMux(s).ServeHTTP(w, r)
	if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("arm: %d %v", w.Code, w.Header())
	}
	var out map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["expiresAt"] != "2026-10-13T10:10:00Z" {
		t.Fatalf("expiresAt %q, want 10 minutes later", out["expiresAt"])
	}
	if a, b := armFleetCode(t, s), armFleetCode(t, s); a == b {
		t.Fatal("two arms gave the same code")
	}
}

func TestFleetLoginArmRefusedWithoutSecret(t *testing.T) {
	s := fleetTestServer(t, nil)
	s.fleet.secret = ""
	r := httptest.NewRequest("POST", "/fleet/login/arm", nil)
	r.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	fleetMux(s).ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("arm with no secret: %d, want 503", w.Code)
	}
}

func TestFleetLoginRedeemSetsSecretCookieOnce(t *testing.T) {
	s := fleetTestServer(t, nil)
	code := armFleetCode(t, s)
	w := redeemFleetCode(s, code)
	wantFleetLoginOK(t, w)
	c := w.Result().Cookies()
	if len(c) != 1 || c[0].Name != "cs_fleet" || c[0].Value != "sec" || c[0].Path != "/fleet" ||
		!c[0].HttpOnly || c[0].SameSite != http.SameSiteStrictMode || c[0].Secure || c[0].MaxAge != 2592000 {
		t.Fatalf("cookie = %+v", c)
	}
	body := w.Body.String()
	if strings.Contains(body, code) || strings.Contains(body, "sec") || strings.Contains(body, "tok") {
		t.Fatalf("login answer must echo no secret: %q", body)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("login Cache-Control = %q, want no-store", got)
	}
	wantFleetLoginRefused(t, redeemFleetCode(s, code))
}

// A link preview (iMessage, Slack, iOS) fetches the link with GET or HEAD
// before Andy taps it. That fetch must not use the code: GET only shows a
// form, and only its POST redeems.
func TestFleetLoginGetShowsFormAndKeepsCode(t *testing.T) {
	s := fleetTestServer(t, nil)
	code := armFleetCode(t, s)
	for _, method := range []string{"GET", "HEAD", "GET"} {
		w := httptest.NewRecorder()
		fleetMux(s).ServeHTTP(w, httptest.NewRequest(method, "/fleet?c="+code, nil))
		if w.Code != http.StatusOK || len(w.Result().Cookies()) != 0 {
			t.Fatalf("%s ?c=: %d cookies %v, want 200 and no cookie", method, w.Code, w.Result().Cookies())
		}
		h := w.Header()
		if h.Get("Content-Type") != "text/html; charset=utf-8" || h.Get("Cache-Control") != "no-store" ||
			h.Get("Referrer-Policy") != "same-origin" || h.Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("%s ?c= headers %v", method, h)
		}
		if got, want := h.Get("Content-Security-Policy"),
			"default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'"; got != want {
			t.Fatalf("%s ?c= CSP = %q, want %q", method, got, want)
		}
		if method == "GET" {
			body := w.Body.String()
			for _, want := range []string{
				"<title>FLEET login</title>",
				"Press the button to log in to the FLEET page on this device.",
				`<form method="POST" action="/fleet/login">`,
				`<input type="hidden" name="c" value="` + code + `">`,
				">Log in</button>",
			} {
				if !strings.Contains(body, want) {
					t.Fatalf("login page lacks %q: %q", want, body)
				}
			}
			rest := strings.ReplaceAll(body, code, "") // a random code may spell "sec"
			if strings.Contains(rest, "<script") || strings.Contains(rest, "sec") || strings.Contains(rest, "tok") {
				t.Fatalf("login page must have no script and no secret: %q", body)
			}
		}
	}
	wantFleetLoginOK(t, redeemFleetCode(s, code))
}

// GET does not check the code: an unknown one gets the same page (no oracle),
// HTML-escaped, since it is request data.
func TestFleetLoginGetEscapesAndDoesNotCheckCode(t *testing.T) {
	s := fleetTestServer(t, nil)
	w := httptest.NewRecorder()
	fleetMux(s).ServeHTTP(w, httptest.NewRequest("GET", "/fleet?c="+url.QueryEscape(`"><b>x`), nil))
	body := w.Body.String()
	if w.Code != http.StatusOK || !strings.Contains(body, `value="&#34;&gt;&lt;b&gt;x"`) || strings.Contains(body, "<b>") {
		t.Fatalf("unknown code page: %d %q", w.Code, body)
	}
}

func TestFleetLoginPostNeedsSameOrigin(t *testing.T) {
	s := fleetTestServer(t, nil)
	code := armFleetCode(t, s)
	for _, origin := range []string{"", "null", "http://evil.example", "http://example.com.evil.example"} {
		w := postFleetLogin(s, code, origin)
		if w.Code != http.StatusForbidden || len(w.Result().Cookies()) != 0 {
			t.Fatalf("Origin %q: %d cookies %v, want 403 and no cookie", origin, w.Code, w.Result().Cookies())
		}
	}
	// A refused POST did not use the code.
	wantFleetLoginOK(t, redeemFleetCode(s, code))
}

func TestFleetLoginPostRejectsLargeBody(t *testing.T) {
	s := fleetTestServer(t, nil)
	code := armFleetCode(t, s)
	r := httptest.NewRequest("POST", "/fleet/login",
		strings.NewReader("c="+code+"&pad="+strings.Repeat("x", 5000)))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "http://example.com")
	w := httptest.NewRecorder()
	fleetMux(s).ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest || len(w.Result().Cookies()) != 0 {
		t.Fatalf("oversized form: %d %v, want 400 and no cookie", w.Code, w.Result().Cookies())
	}
	wantFleetLoginOK(t, redeemFleetCode(s, code))
}

// Only the form body carries the code: a ?c= on the POST URL is ignored.
func TestFleetLoginPostIgnoresQueryCode(t *testing.T) {
	s := fleetTestServer(t, nil)
	code := armFleetCode(t, s)
	r := httptest.NewRequest("POST", "/fleet/login?c="+code, nil)
	r.Header.Set("Origin", "http://example.com")
	w := httptest.NewRecorder()
	fleetMux(s).ServeHTTP(w, r)
	wantFleetLoginRefused(t, w)
	wantFleetLoginOK(t, redeemFleetCode(s, code))
}

func TestFleetLoginUnknownCode(t *testing.T) {
	s := fleetTestServer(t, nil)
	armFleetCode(t, s)
	wantFleetLoginRefused(t, redeemFleetCode(s, "AAAAAAAAAAAAAAAAAAAAAA"))
}

func TestFleetLoginExpiredCode(t *testing.T) {
	s := fleetTestServer(t, nil)
	now := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	s.fleet.now = func() time.Time { return now }
	early, late := armFleetCode(t, s), armFleetCode(t, s)
	now = now.Add(10*time.Minute - time.Second)
	wantFleetLoginOK(t, redeemFleetCode(s, early))
	now = now.Add(2 * time.Second)
	wantFleetLoginRefused(t, redeemFleetCode(s, late))
}

func TestFleetLoginKeepsNewestFourCodes(t *testing.T) {
	s := fleetTestServer(t, nil)
	var codes []string
	for range 5 {
		codes = append(codes, armFleetCode(t, s))
	}
	wantFleetLoginRefused(t, redeemFleetCode(s, codes[0]))
	for _, c := range codes[1:] {
		wantFleetLoginOK(t, redeemFleetCode(s, c))
	}
}

// The old ?t=<server token> login is gone: it is a plain visit now.
func TestFleetLoginOldTokenParamSetsNoCookie(t *testing.T) {
	s := fleetTestServer(t, nil)
	w := httptest.NewRecorder()
	fleetMux(s).ServeHTTP(w, httptest.NewRequest("GET", "/fleet?t=tok", nil))
	if len(w.Result().Cookies()) != 0 {
		t.Fatalf("?t= set a cookie: %v", w.Result().Cookies())
	}
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `location.replace("/fleet?hop=1")`) {
		t.Fatalf("?t= must act like a plain unauthenticated visit: %d %q", w.Code, w.Body.String())
	}
}

func TestFleetLoginRevokeRotatesSecretAndDropsCodes(t *testing.T) {
	s := fleetTestServer(t, nil)
	s.fleet.secretPath = filepath.Join(t.TempDir(), "cfg", "fleet-secret")
	pending := armFleetCode(t, s)

	revoke := func(mod func(*http.Request)) int {
		r := httptest.NewRequest("POST", "/fleet/login/revoke", nil)
		mod(r)
		w := httptest.NewRecorder()
		fleetMux(s).ServeHTTP(w, r)
		return w.Code
	}
	if c := revoke(func(*http.Request) {}); c != http.StatusUnauthorized {
		t.Fatalf("no auth: %d", c)
	}
	if c := revoke(func(r *http.Request) { r.AddCookie(&http.Cookie{Name: "cs_fleet", Value: "sec"}) }); c != http.StatusUnauthorized {
		t.Fatalf("cookie: %d, want 401", c)
	}
	if s.fleet.secret != "sec" {
		t.Fatal("a refused revoke rotated the secret")
	}
	if c := revoke(func(r *http.Request) { r.Header.Set("Authorization", "Bearer tok") }); c != http.StatusNoContent {
		t.Fatalf("bearer: %d, want 204", c)
	}

	fresh := s.fleet.secret
	if fresh == "" || fresh == "sec" || fresh == "tok" {
		t.Fatalf("secret after revoke = %q", fresh)
	}
	data, err := os.ReadFile(s.fleet.secretPath)
	if err != nil || strings.TrimSpace(string(data)) != fresh {
		t.Fatalf("secret file = %q, %v", data, err)
	}
	if st, _ := os.Stat(s.fleet.secretPath); st.Mode().Perm() != 0o600 {
		t.Fatalf("secret file mode %v", st.Mode().Perm())
	}

	r := httptest.NewRequest("GET", "/fleet/state", nil)
	r.AddCookie(&http.Cookie{Name: "cs_fleet", Value: "sec"})
	w := httptest.NewRecorder()
	fleetMux(s).ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("old cookie after revoke: %d, want 401", w.Code)
	}
	wantFleetLoginRefused(t, redeemFleetCode(s, pending))

	w = redeemFleetCode(s, armFleetCode(t, s))
	if c := w.Result().Cookies(); w.Code != http.StatusSeeOther || len(c) != 1 || c[0].Value != fresh {
		t.Fatalf("login after revoke: %d %v", w.Code, c)
	}
}

func TestLoadOrCreateFleetSecret(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cfg", "fleet-secret")
	first, err := loadOrCreateFleetSecret(path)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`).MatchString(first) {
		t.Fatalf("secret %q is not 32 bytes of unpadded base64url", first)
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("secret file: %v %v", st, err)
	}
	if dir, _ := os.Stat(filepath.Dir(path)); dir.Mode().Perm() != 0o755 {
		t.Fatalf("config dir mode %v", dir.Mode().Perm())
	}
	again, err := loadOrCreateFleetSecret(path)
	if err != nil || again != first {
		t.Fatalf("second load = %q, %v; want the same secret", again, err)
	}
	if err := os.WriteFile(path, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrCreateFleetSecret(path); err == nil {
		t.Fatal("an empty secret file must be an error, not an empty secret")
	}
}

func TestFleetLoginAcceptsPastedLinkOrCode(t *testing.T) {
	cases := map[string]func(code string) string{
		"bare code":         func(c string) string { return c },
		"code with spaces":  func(c string) string { return "  " + c + "\n" },
		"full link":         func(c string) string { return "http://100.64.0.1:7777/fleet?c=" + c },
		"link with spaces":  func(c string) string { return " https://host.example/fleet?c=" + c + " " },
		"link, more params": func(c string) string { return "http://h/fleet?x=1&c=" + c + "#top" },
		"query only":        func(c string) string { return "c=" + c },
	}
	for name, paste := range cases {
		t.Run(name, func(t *testing.T) {
			s := fleetTestServer(t, nil)
			code := armFleetCode(t, s)
			wantFleetLoginOK(t, redeemFleetCode(s, paste(code)))
		})
	}
}

func TestFleetLoginPastedLinkWithoutCodeRefused(t *testing.T) {
	s := fleetTestServer(t, nil)
	armFleetCode(t, s)
	wantFleetLoginRefused(t, redeemFleetCode(s, "http://h/fleet?c="))
}

func TestFleetHopWithoutAuthShowsLoginForm(t *testing.T) {
	s := fleetTestServer(t, nil)
	w := httptest.NewRecorder()
	fleetMux(s).ServeHTTP(w, httptest.NewRequest("GET", "/fleet?hop=1", nil))
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("%d %q", w.Code, w.Header().Get("Content-Type"))
	}
	body := w.Body.String()
	for _, need := range []string{"<title>Log in to FLEET</title>", "claude-sessions fleet-url", "Paste the link or the code here.",
		`<form method="POST" action="/fleet/login">`, `name="c"`, `autocomplete="off"`, `autocapitalize="off"`, ">Log in</button>"} {
		if !strings.Contains(body, need) {
			t.Errorf("form page must contain %q", need)
		}
	}
	if strings.Contains(body, "<script") {
		t.Error("form page must have no script")
	}
	h := w.Header()
	if !strings.Contains(h.Get("Content-Security-Policy"), "form-action 'self'") ||
		strings.Contains(h.Get("Content-Security-Policy"), "script-src") ||
		h.Get("Referrer-Policy") != "same-origin" || h.Get("Cache-Control") != "no-store" {
		t.Fatalf("headers = %v", h)
	}
}
