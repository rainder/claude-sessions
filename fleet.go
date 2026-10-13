package main

import (
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

//go:embed fleet.html
var fleetPageHTML []byte

const (
	fleetCookieName   = "cs_fleet"
	fleetCookieMaxAge = 30 * 24 * 60 * 60
	fleetSessionName  = "FLEET"
	// fleetStaleAfter: FLEET's idle monitor re-arms every 30 minutes, so a
	// healthy FLEET writes state.json at least that often.
	fleetStaleAfter = 40 * time.Minute
	fleetPageCSP    = "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; " +
		"connect-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
)

// fleetDeps holds the /fleet routes' seams and their small in-memory state.
// The zero value is the production configuration.
type fleetDeps struct {
	dir     string                       // "" → ~/.claude/fleet
	now     func() time.Time             // nil → time.Now
	capture func(string) (string, error) // nil → capturePane

	mu       sync.Mutex // guards the last good state.json copy
	good     map[string]json.RawMessage
	goodMod  time.Time
	goodSize int64

	logMu sync.Mutex // serializes this process's log.jsonl appends
}

func (s *server) fleetDir() string {
	if s.fleet.dir != "" {
		return s.fleet.dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "fleet")
}

func (s *server) fleetNow() time.Time {
	if s.fleet.now != nil {
		return s.fleet.now()
	}
	return time.Now()
}

type fleetAuthKind int

const (
	fleetAuthNone fleetAuthKind = iota
	fleetAuthBearer
	fleetAuthCookie
)

// fleetAuth accepts the bearer header or the cs_fleet cookie. Only /fleet*
// handlers call it; every other route keeps s.authed, so the cookie opens
// nothing else.
func (s *server) fleetAuth(r *http.Request) fleetAuthKind {
	if s.authed(r) {
		return fleetAuthBearer
	}
	if c, err := r.Cookie(fleetCookieName); err == nil &&
		subtle.ConstantTimeCompare([]byte(c.Value), []byte(s.token)) == 1 {
		return fleetAuthCookie
	}
	return fleetAuthNone
}

type fleetSession struct {
	Alive     bool   `json:"alive"`
	SessionID string `json:"sessionId"`
	PID       int    `json:"pid"`
	Status    string `json:"status"`
	Reason    string `json:"reason,omitempty"`
}

// findFleet picks the one local session named exactly FLEET.
func findFleet(sessions []Session) (Session, string) {
	var hit Session
	n := 0
	for _, sess := range sessions {
		if sess.Name == fleetSessionName {
			hit = sess
			n++
		}
	}
	switch n {
	case 0:
		return Session{}, "not running"
	case 1:
		return hit, ""
	default:
		return Session{}, "ambiguous"
	}
}

// fleetStatus maps a row onto the page's three states. FLEET usually reads
// "shell" (its Monitor runs in the background) while its turn is free, so
// anything that is neither waiting nor busy is idle.
func fleetStatus(sess Session) string {
	switch {
	case sess.Waiting():
		return "waiting"
	case sess.Status == "busy":
		return "busy"
	default:
		return "idle"
	}
}

// discoverFleet resolves FLEET from the cached session list.
func (s *server) discoverFleet() (Session, fleetSession) {
	sessions, err := s.cachedSessions()
	if err != nil {
		return Session{}, fleetSession{Reason: "session list failed"}
	}
	sess, reason := findFleet(sessions)
	if reason != "" {
		return Session{}, fleetSession{Reason: reason}
	}
	return sess, fleetSession{Alive: true, SessionID: sess.SessionID, PID: sess.PID, Status: fleetStatus(sess)}
}

func registerFleetRoutes(mux *http.ServeMux, s *server) {
	mux.HandleFunc("GET /fleet", s.fleetPage)
	mux.HandleFunc("GET /fleet/usage", s.fleetUsage)
	mux.HandleFunc("GET /fleet/state", s.fleetState) // Task 3
	mux.HandleFunc("GET /fleet/log", s.fleetLog)     // Task 4
	mux.HandleFunc("GET /fleet/chat", s.fleetChat)   // Task 5
	mux.HandleFunc("POST /fleet/say", s.fleetSay)    // Task 6
}

// fleetPage serves the page. With ?t=<token> it is the one-time login: set
// the cookie and leave the URL by script. A 303 would not work from a link
// tapped in another app: the redirect is part of a cross-site navigation, so
// the browser would not send the new SameSite=Strict cookie on it.
func (s *server) fleetPage(w http.ResponseWriter, r *http.Request) {
	if t := r.URL.Query().Get("t"); t != "" {
		if subtle.ConstantTimeCompare([]byte(t), []byte(s.token)) != 1 {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name: fleetCookieName, Value: s.token, Path: "/fleet", MaxAge: fleetCookieMaxAge,
			HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil,
		})
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><meta charset="utf-8"><title>FLEET</title>` +
			`<meta http-equiv="refresh" content="0;url=/fleet"><script>location.replace("/fleet")</script>`))
		return
	}
	if s.fleetAuth(r) == fleetAuthNone {
		http.Error(w, "Unauthorized. Run `claude-sessions fleet-url` on the host and open that URL.", http.StatusUnauthorized)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", fleetPageCSP)
	h.Set("Referrer-Policy", "same-origin")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	_, _ = w.Write(fleetPageHTML)
}

// fleetUsage is GET /usage/numbers under fleet auth: the cookie has
// Path=/fleet, so the page cannot reach /usage/numbers itself.
func (s *server) fleetUsage(w http.ResponseWriter, r *http.Request) {
	if s.fleetAuth(r) == fleetAuthNone {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	s.serveUsageNumbers(w)
}

const fleetStateMaxBytes = 1 << 20

func emptyFleetState() map[string]json.RawMessage {
	return map[string]json.RawMessage{"version": json.RawMessage("1"), "cards": json.RawMessage("[]")}
}

// loadFleetState returns the state object to serve and a stateError ("" when
// fine). A bad file never replaces the last good copy, and never causes a 500.
func (s *server) loadFleetState() (map[string]json.RawMessage, string) {
	f := &s.fleet
	f.mu.Lock()
	defer f.mu.Unlock()
	fallback := func(msg string) (map[string]json.RawMessage, string) {
		if f.good != nil {
			return f.good, msg
		}
		return emptyFleetState(), msg
	}
	path := filepath.Join(s.fleetDir(), "state.json")
	st, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) && f.good == nil {
			return emptyFleetState(), "FLEET has not written state yet"
		}
		return fallback("state.json is invalid: cannot read")
	}
	if f.good != nil && st.ModTime().Equal(f.goodMod) && st.Size() == f.goodSize {
		return f.good, ""
	}
	if st.Size() > fleetStateMaxBytes {
		return fallback("state.json is invalid: too large")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fallback("state.json is invalid: cannot read")
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil || obj == nil {
		return fallback("state.json is invalid: not a JSON object")
	}
	var cards []json.RawMessage
	if err := json.Unmarshal(obj["cards"], &cards); err != nil || cards == nil {
		return fallback("state.json is invalid: cards is not an array")
	}
	f.good, f.goodMod, f.goodSize = obj, st.ModTime(), st.Size()
	return obj, ""
}

func (s *server) fleetState(w http.ResponseWriter, r *http.Request) {
	if s.fleetAuth(r) == fleetAuthNone {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	state, stateErr := s.loadFleetState()
	out := make(map[string]any, len(state)+3)
	for k, v := range state {
		out[k] = v
	}
	_, fl := s.discoverFleet()
	out["fleet"] = fl
	out["stale"] = fleetStateStale(state, stateErr, s.fleetNow())
	if stateErr != "" {
		out["stateError"] = stateErr
	} else {
		out["stateError"] = nil
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, out)
}

// fleetStateStale: no parseable writtenAt, or older than fleetStaleAfter. The
// "not written yet" placeholder is not stale; its own banner explains it.
func fleetStateStale(state map[string]json.RawMessage, stateErr string, now time.Time) bool {
	if stateErr == "FLEET has not written state yet" {
		return false
	}
	var at string
	if err := json.Unmarshal(state["writtenAt"], &at); err != nil {
		return true
	}
	t, err := time.Parse(time.RFC3339, at)
	if err != nil {
		return true
	}
	return now.Sub(t) > fleetStaleAfter
}

// Stubs until each route lands; every one is replaced in the commit that
// adds its real handler.
func (s *server) fleetLog(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "not implemented", http.StatusNotImplemented)
}

func (s *server) fleetChat(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "not implemented", http.StatusNotImplemented)
}

func (s *server) fleetSay(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "not implemented", http.StatusNotImplemented)
}
