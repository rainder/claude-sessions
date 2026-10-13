package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The FLEET page login. The server token never travels in a URL or a cookie:
// a URL lands in shell scrollback, browser history and sync, and a cookie
// holding the token would be the token. Instead `fleet-url` asks the running
// server (bearer auth) for a short-lived one-time code, and redeeming that
// code sets a cookie holding a separate fleet-only secret. That secret opens
// /fleet* and nothing else, and revoking it does not touch the token every
// other client uses.
const (
	fleetCodeTTL   = 10 * time.Minute
	fleetCodeMax   = 4 // live codes kept; arming another drops the oldest
	fleetLoginGone = "This login link is used or expired. Run claude-sessions fleet-url on the host again."
)

type fleetLoginCode struct {
	code    string
	expires time.Time
}

// fleetSecretPath is where the cookie secret lives, beside the server token.
func fleetSecretPath() string {
	dir := ConfigDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "fleet-secret")
}

// randomURLToken returns n random bytes as unpadded base64url.
func randomURLToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// writeFleetSecret writes the secret atomically (temp file + rename), so a
// crash mid-revoke never leaves an empty or half-written file behind.
func writeFleetSecret(path, secret string) error {
	if path == "" {
		return fmt.Errorf("no home directory")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return writeFileAtomic(path, []byte(secret+"\n"), 0o600)
}

// loadOrCreateFleetSecret reads the cookie secret, creating it on first use.
// An empty file is an error rather than an empty secret: an empty secret would
// match an empty cookie. Errors never carry the value.
func loadOrCreateFleetSecret(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("no home directory")
	}
	if data, err := os.ReadFile(path); err == nil {
		sec := strings.TrimSpace(string(data))
		if sec == "" {
			return "", fmt.Errorf("%s exists but is empty; delete it to regenerate", path)
		}
		return sec, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	sec, err := randomURLToken(32)
	if err != nil {
		return "", err
	}
	if err := writeFleetSecret(path, sec); err != nil {
		return "", err
	}
	return sec, nil
}

// fleetSecret returns the current cookie secret ("" = cookie auth off).
func (s *server) fleetSecret() string {
	s.fleet.secretMu.Lock()
	defer s.fleet.secretMu.Unlock()
	return s.fleet.secret
}

// pruneFleetCodesLocked drops expired codes. Caller holds secretMu.
func (s *server) pruneFleetCodesLocked(now time.Time) {
	live := s.fleet.codes[:0]
	for _, c := range s.fleet.codes {
		if now.Before(c.expires) {
			live = append(live, c)
		}
	}
	s.fleet.codes = live
}

// redeemFleetCode consumes code if it is live and returns the secret the
// cookie should hold; "" when the code is unknown, used or expired. Every
// live code is compared in constant time, with no early exit.
func (s *server) redeemFleetCode(code string) string {
	f := &s.fleet
	f.secretMu.Lock()
	defer f.secretMu.Unlock()
	s.pruneFleetCodesLocked(s.fleetNow())
	hit := -1
	for i, c := range f.codes {
		if subtle.ConstantTimeCompare([]byte(code), []byte(c.code)) == 1 {
			hit = i
		}
	}
	if hit < 0 || f.secret == "" {
		return ""
	}
	f.codes = append(f.codes[:hit], f.codes[hit+1:]...)
	return f.secret
}

// fleetLoginArm is POST /fleet/login/arm: bearer only, so a cookie holder
// cannot mint logins for more devices. In memory only; a restart drops codes.
func (s *server) fleetLoginArm(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	code, err := randomURLToken(16)
	if err != nil {
		http.Error(w, "cannot generate a code", http.StatusInternalServerError)
		return
	}
	f := &s.fleet
	f.secretMu.Lock()
	if f.secret == "" {
		f.secretMu.Unlock()
		// The code would set an empty cookie that opens nothing.
		http.Error(w, "FLEET page login is disabled: the fleet secret could not be loaded", http.StatusServiceUnavailable)
		return
	}
	now := s.fleetNow()
	s.pruneFleetCodesLocked(now)
	expires := now.Add(fleetCodeTTL)
	f.codes = append(f.codes, fleetLoginCode{code: code, expires: expires})
	if n := len(f.codes); n > fleetCodeMax {
		f.codes = append(f.codes[:0], f.codes[n-fleetCodeMax:]...)
	}
	f.secretMu.Unlock()
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]string{"code": code, "expiresAt": expires.UTC().Format(time.RFC3339)})
}

// fleetLoginRevoke is POST /fleet/login/revoke: rotate the cookie secret and
// drop every live code, so every device logged in so far is logged out.
func (s *server) fleetLoginRevoke(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	sec, err := randomURLToken(32)
	if err != nil {
		http.Error(w, "cannot generate a secret", http.StatusInternalServerError)
		return
	}
	f := &s.fleet
	f.secretMu.Lock()
	defer f.secretMu.Unlock()
	// The new secret takes effect in memory even if saving it fails: the old
	// cookies stop working now, and the 500 says a restart would bring them
	// back. A server with no secretPath (tests) rotates in memory only.
	f.secret, f.codes = sec, nil
	if f.secretPath != "" {
		if err := writeFleetSecret(f.secretPath, sec); err != nil {
			http.Error(w, "revoked until restart; saving the new secret failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// fleetLogin is GET /fleet?c=<code>: consume the code, set the cookie, and
// leave the URL by script. A 303 would not work from a link tapped in
// another app: the redirect is part of a cross-site navigation, so the
// browser would not send the new SameSite=Strict cookie on it.
func (s *server) fleetLogin(w http.ResponseWriter, r *http.Request, code string) {
	sec := s.redeemFleetCode(code)
	if sec == "" {
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, fleetLoginGone, http.StatusUnauthorized)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: fleetCookieName, Value: sec, Path: "/fleet", MaxAge: fleetCookieMaxAge,
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil,
	})
	writeFleetHop(w, "/fleet")
}
