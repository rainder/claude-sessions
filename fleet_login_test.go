package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

func redeemFleetCode(s *server, code string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	fleetMux(s).ServeHTTP(w, httptest.NewRequest("GET", "/fleet?c="+code, nil))
	return w
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
	if w.Code != http.StatusOK {
		t.Fatalf("code %d, want 200 (HTML hop, not a 303)", w.Code)
	}
	c := w.Result().Cookies()
	if len(c) != 1 || c[0].Name != "cs_fleet" || c[0].Value != "sec" || c[0].Path != "/fleet" ||
		!c[0].HttpOnly || c[0].SameSite != http.SameSiteStrictMode || c[0].Secure || c[0].MaxAge != 2592000 {
		t.Fatalf("cookie = %+v", c)
	}
	body := w.Body.String()
	if !strings.Contains(body, `location.replace("/fleet")`) ||
		strings.Contains(body, code) || strings.Contains(body, "sec") || strings.Contains(body, "tok") {
		t.Fatalf("login hop must redirect by script and echo no secret: %q", body)
	}
	if got := w.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Fatalf("login hop Referrer-Policy = %q, want no-referrer", got)
	}
	wantFleetLoginRefused(t, redeemFleetCode(s, code))
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
	if w := redeemFleetCode(s, early); w.Code != http.StatusOK {
		t.Fatalf("inside the TTL: %d", w.Code)
	}
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
		if w := redeemFleetCode(s, c); w.Code != http.StatusOK {
			t.Fatalf("newer code refused: %d", w.Code)
		}
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
	if c := w.Result().Cookies(); w.Code != http.StatusOK || len(c) != 1 || c[0].Value != fresh {
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
