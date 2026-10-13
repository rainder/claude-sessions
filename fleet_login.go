package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"html"
	"net/http"
	"net/url"
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

// fleetLoginPageCSP: the login page has no script; its one form posts to
// this origin.
const fleetLoginPageCSP = "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'"

// fleetLoginPage is GET (and HEAD) /fleet?c=<code>: a page with one "Log in"
// button that posts the code to /fleet/login. It does not check or use the
// code. Link previews (iMessage, Slack, iOS) fetch a link with GET before
// anyone taps it; if GET used the code, the preview would log itself in and
// the tap would get 401. Checking the code here would also make GET an
// oracle for live codes.
//
// Referrer-Policy is same-origin, not no-referrer: with no-referrer the
// browser sends Origin: null on the form POST, and /fleet/login would refuse
// it. same-origin keeps the Referer (which holds the code) on this origin.
func fleetLoginPage(w http.ResponseWriter, code string) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", fleetLoginPageCSP)
	h.Set("Referrer-Policy", "same-origin")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(`<!doctype html><html lang="en"><meta charset="utf-8">` +
		`<meta name="viewport" content="width=device-width, initial-scale=1">` +
		`<title>FLEET login</title>` +
		`<style>body{font:18px system-ui,sans-serif;margin:0;padding:3em 1em;text-align:center}` +
		`button{font:inherit;font-size:1.4em;padding:.8em 3em;border-radius:.5em}</style>` +
		`<p>Press the button to log in to the FLEET page on this device.</p>` +
		`<form method="POST" action="/fleet/login">` +
		`<input type="hidden" name="c" value="` + html.EscapeString(code) + `">` +
		`<button type="submit">Log in</button></form></html>`))
}

// fleetLoginFormPage is the 401 for /fleet?hop=1 with no auth: a form to
// paste a fleet-url link or code. An installed home-screen app (iOS keeps a
// separate cookie jar for it) starts here with no cookie, and a link opened
// from another app goes to the browser, not to the installed app.
func fleetLoginFormPage(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", fleetLoginPageCSP)
	h.Set("Referrer-Policy", "same-origin")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`<!doctype html><html lang="en"><meta charset="utf-8">` +
		`<meta name="viewport" content="width=device-width, initial-scale=1">` +
		`<title>Log in to FLEET</title>` +
		`<style>body{font:18px system-ui,sans-serif;padding:3em 1em;max-width:32em;margin:auto}` +
		`input,button{font:inherit;box-sizing:border-box;width:100%;padding:.7em;border-radius:.5em}` +
		`button{margin-top:.8em;font-size:1.2em}</style>` +
		`<h1>Log in to FLEET</h1>` +
		`<p>On the workstation run <code>claude-sessions fleet-url</code>. Paste the link or the code here.</p>` +
		`<form method="POST" action="/fleet/login">` +
		`<input name="c" type="text" required autocomplete="off" autocapitalize="off" autocorrect="off" spellcheck="false" aria-label="Link or code">` +
		`<button type="submit">Log in</button></form></html>`))
}

// fleetCodeFromInput takes the code from a pasted value: a bare code, or a
// whole fleet-url link (or its query) holding c=. A code is base64url, so it
// never contains "c=" itself.
func fleetCodeFromInput(v string) string {
	v = strings.TrimSpace(v)
	if !strings.Contains(v, "c=") {
		return v
	}
	q := v
	if i := strings.IndexByte(q, '?'); i >= 0 {
		q = q[i+1:]
	}
	if i := strings.IndexByte(q, '#'); i >= 0 {
		q = q[:i]
	}
	vals, _ := url.ParseQuery(q)
	return strings.TrimSpace(vals.Get("c"))
}

// fleetLogin is POST /fleet/login (form field c): consume the code, set the
// cookie, and 303 to /fleet. The POST comes from fleetLoginPage, a
// same-origin page, so the redirect is a same-site navigation and the browser
// sends the new SameSite=Strict cookie on it. Same-origin is required, like
// /fleet/say's cookie path, and is checked before the form is read: a
// cross-site POST must not use (burn) a code.
func (s *server) fleetLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !sameOrigin(r) {
		http.Error(w, "origin mismatch", http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	// PostForm, not Form: the code comes from the body only, never the URL.
	// The field holds a bare code, or a link pasted into fleetLoginFormPage.
	sec := s.redeemFleetCode(fleetCodeFromInput(r.PostForm.Get("c")))
	if sec == "" {
		http.Error(w, fleetLoginGone, http.StatusUnauthorized)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: fleetCookieName, Value: sec, Path: "/fleet", MaxAge: fleetCookieMaxAge,
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil,
	})
	// The redirected GET needs no Referer: the page's one held the code.
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Location", "/fleet")
	w.WriteHeader(http.StatusSeeOther)
}
