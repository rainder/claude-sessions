package main

import (
	"bytes"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
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
		"connect-src 'self'; img-src 'self' data:; manifest-src 'self'; worker-src 'self'; " +
		"base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
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

	typeText   func(target, text string) error   // nil → tmux send-keys -l
	pressEnter func(target string) error         // nil → tmux send-keys Enter
	sleep      func(time.Duration)               // nil → time.Sleep
	paneInMode func(target string) (bool, error) // nil → tmux display #{pane_in_mode}
	sayLogPath string                            // refusal log; "" → fleetSayLogDefault()

	sayMu     sync.Mutex // one send at a time, so two devices never interleave keystrokes
	lastSayAt time.Time
	saidAt    map[string]time.Time // text → completion time, last 10 s only

	// Login (fleet_login.go). secretMu guards all three: revoke rotates the
	// secret and drops the codes in one step.
	secretMu   sync.Mutex
	secret     string           // cookie value; "" → cookie auth off
	secretPath string           // where revoke saves a new secret; "" → memory only
	codes      []fleetLoginCode // live one-time codes, oldest first

	// The last FLEET session seen alive (fleet_last.go). lastMu guards
	// lastMem, the copy last written to (or first read from) lastPath, so an
	// unchanged FLEET costs no write per state poll.
	lastPath   string // "" → ~/.config/claude-sessions/fleet-last.json
	home       string // "" → os.UserHomeDir(); transcripts and the default cwd
	lastMu     sync.Mutex
	lastLoaded bool
	lastMem    fleetLast

	// POST /fleet/start (fleet_start.go). startMu is held across the whole
	// handler, so two taps cannot both pass the guards; startStateMu guards
	// the fields /fleet/state reads, so a poll never waits on a spawn.
	defaultCwd    string                                             // "" → ~/Developer/trecs-brain
	spawn         func(cwd, display, command string) (string, error) // nil → SpawnNew
	presets       func() ([]CommandPreset, error)                    // nil → LoadCommandPresets
	startMu       sync.Mutex
	startStateMu  sync.Mutex
	startInFlight bool
	lastStartAt   time.Time // last attempt that passed discovery; a failed spawn counts
	startingAt    time.Time // zero → no start pending
	startingMode  string
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

// fleetAuth accepts the bearer header or the cs_fleet cookie, which holds the
// fleet-only secret, never the server token. Only /fleet* handlers call it;
// every other route keeps s.authed, so the cookie opens nothing else.
func (s *server) fleetAuth(r *http.Request) fleetAuthKind {
	if s.authed(r) {
		return fleetAuthBearer
	}
	if sec := s.fleetSecret(); sec != "" {
		if c, err := r.Cookie(fleetCookieName); err == nil &&
			subtle.ConstantTimeCompare([]byte(c.Value), []byte(sec)) == 1 {
			return fleetAuthCookie
		}
	}
	return fleetAuthNone
}

type fleetSession struct {
	Alive     bool   `json:"alive"`
	SessionID string `json:"sessionId"`
	PID       int    `json:"pid"`
	Status    string `json:"status"`
	Reason    string `json:"reason,omitempty"`
	// Code is the machine form of Reason; empty when alive.
	Code string `json:"code,omitempty"`
	// HasLast: FLEET is not alive and fleet-last.json names a session that
	// POST /fleet/start would resume.
	HasLast bool `json:"hasLast"`
	// CanStart: not_live, no start in flight or pending, and the start rate
	// limit is free.
	CanStart bool `json:"canStart"`
	// Starting: a web start in the last 90 s, and FLEET not alive yet.
	Starting *fleetStarting `json:"starting,omitempty"`
}

// fleetReasonCodes maps discovery's human reason text onto its code.
var fleetReasonCodes = map[string]string{
	"not running":         "not_live",
	"ambiguous":           "ambiguous",
	"session list failed": "list_failed",
}

// findFleet picks the one local claude session the user named exactly FLEET.
func findFleet(sessions []Session) (Session, string) {
	var hit Session
	n := 0
	for _, sess := range sessions {
		// Only a claude row with a user-set name: a derived title or a grok
		// row that happens to read "FLEET" is not the user's FLEET.
		if sess.Name == fleetSessionName && sess.Tool == "" && sess.NameSource != "derived" {
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

// discoverFleet resolves FLEET from the cached session list. An alive FLEET
// is remembered in fleet-last.json (written only when it changed).
func (s *server) discoverFleet() (Session, fleetSession) {
	sessions, err := s.cachedSessions()
	if err != nil {
		return Session{}, fleetSessionDown("session list failed")
	}
	sess, reason := findFleet(sessions)
	if reason != "" {
		return Session{}, fleetSessionDown(reason)
	}
	s.rememberFleet(sess)
	return sess, fleetSession{Alive: true, SessionID: sess.SessionID, PID: sess.PID, Status: fleetStatus(sess)}
}

func fleetSessionDown(reason string) fleetSession {
	return fleetSession{Reason: reason, Code: fleetReasonCodes[reason]}
}

func registerFleetRoutes(mux *http.ServeMux, s *server) {
	mux.HandleFunc("GET /fleet", s.fleetPage)
	mux.HandleFunc("GET /fleet/usage", s.fleetUsage)
	mux.HandleFunc("GET /fleet/state", s.fleetState) // Task 3
	mux.HandleFunc("GET /fleet/log", s.fleetLog)     // Task 4
	mux.HandleFunc("GET /fleet/chat", s.fleetChat)   // Task 5
	mux.HandleFunc("POST /fleet/say", s.fleetSay)    // Task 6
	mux.HandleFunc("POST /fleet/start", s.fleetStart)
	mux.HandleFunc("POST /fleet/login", s.fleetLogin)
	mux.HandleFunc("POST /fleet/login/arm", s.fleetLoginArm)
	mux.HandleFunc("POST /fleet/login/revoke", s.fleetLoginRevoke)
	registerFleetPWARoutes(mux)
}

// fleetPage serves the page. With ?c=<code> it is the login page
// (fleetLoginPage), which does not use the code. The old ?t= parameter means
// nothing now: such a request is a plain visit.
func (s *server) fleetPage(w http.ResponseWriter, r *http.Request) {
	if c := r.URL.Query().Get("c"); c != "" {
		fleetLoginPage(w, c)
		return
	}
	if s.fleetAuth(r) == fleetAuthNone {
		// A later plain /fleet link tapped in another app is a cross-site
		// navigation too, so the Strict cookie is not sent. One same-site hop
		// to ?hop=1 brings it along; a second miss there is a real 401.
		if r.URL.Query().Get("hop") != "1" {
			writeFleetHop(w, "/fleet?hop=1")
			return
		}
		http.Error(w, "Unauthorized. Run `claude-sessions fleet-url` on the host and open that URL.", http.StatusUnauthorized)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", fleetPageCSP)
	h.Set("Referrer-Policy", "same-origin")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	// Marks the app page for the service worker, which caches /fleet for
	// offline start only when this is set: never the hop or login pages.
	h.Set("X-Fleet-App", "1")
	_, _ = w.Write(fleetPageHTML)
}

// writeFleetHop answers a tiny page that moves on to to by script, so the
// next request is a same-site navigation that carries the Strict cookie.
// to is a fixed path, never request data.
func writeFleetHop(w http.ResponseWriter, to string) {
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(`<!doctype html><meta charset="utf-8"><title>FLEET</title>` +
		`<meta http-equiv="refresh" content="0;url=` + to + `"><script>location.replace("` + to + `")</script>`))
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
	if !fl.Alive {
		_, fl.HasLast = s.fleetResumable()
	}
	s.fleetStartInfo(&fl)
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

const (
	fleetLogChunk   = 256 << 10
	fleetLogMaxScan = 8 << 20
)

type fleetLogEntry struct {
	At     string `json:"at"`
	Type   string `json:"type"`
	Ticket string `json:"ticket,omitempty"`
	Text   string `json:"text,omitempty"`
	URL    string `json:"url,omitempty"`
	CardID string `json:"cardId,omitempty"`
	// web-start lines only.
	Mode      string `json:"mode,omitempty"`
	SessionID string `json:"sessionId,omitempty"`
	Tmux      string `json:"tmux,omitempty"`
}

func (s *server) appendFleetLog(e fleetLogEntry) error {
	dir := s.fleetDir()
	if dir == "" {
		return fmt.Errorf("no home directory")
	}
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	s.fleet.logMu.Lock()
	defer s.fleet.logMu.Unlock()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, "log.jsonl"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}

func validFleetLogLine(b []byte) bool {
	var probe struct {
		At   string `json:"at"`
		Type string `json:"type"`
	}
	return json.Unmarshal(b, &probe) == nil && probe.At != "" && probe.Type != ""
}

// readFleetLogPage reads backward from byte offset before (-1 = end of file)
// and returns up to n valid entries, newest first. next is the absolute
// offset where the oldest returned line starts, or nil when nothing older
// remains. Offsets are absolute in an append-only file, so a cursor stays
// valid however much FLEET appends after it was issued.
func (s *server) readFleetLogPage(before int64, n int) ([]json.RawMessage, *int64, error) {
	f, err := os.Open(filepath.Join(s.fleetDir(), "log.jsonl"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	end := st.Size()
	if before > end { // the log was truncated or rotated under the cursor
		return nil, nil, nil
	}
	if before >= 0 && before < end {
		end = before
	}
	// A trailing line without '\n' is a writer mid-append: end the read at
	// the last newline.
	if end == st.Size() && end > 0 {
		end, err = lastNewlineEnd(f, end)
		if err != nil {
			return nil, nil, err
		}
	}
	type line struct {
		off int64
		raw []byte
	}
	var found []line // newest first
	scanned := int64(0)
	chunk := int64(fleetLogChunk)
	for end > 0 && len(found) < n && scanned < fleetLogMaxScan {
		start := max(0, end-chunk)
		buf := make([]byte, end-start)
		if _, err := f.ReadAt(buf, start); err != nil && err != io.EOF {
			return nil, nil, err
		}
		cut := 0
		if start > 0 {
			// buf ends on a line boundary, so its last byte is that line's
			// '\n'; the partial first line ends at the first '\n' before it.
			i := bytes.IndexByte(buf[:len(buf)-1], '\n')
			if i < 0 { // one line longer than the chunk: grow and retry
				if chunk >= fleetLogMaxScan {
					return nil, nil, fmt.Errorf("log line too long")
				}
				chunk *= 2
				continue
			}
			cut = i + 1
		}
		body := buf[cut:]
		var lines []line
		off := start + int64(cut)
		for len(body) > 0 {
			i := bytes.IndexByte(body, '\n')
			if i < 0 {
				break
			}
			lines = append(lines, line{off: off, raw: body[:i]})
			off += int64(i + 1)
			body = body[i+1:]
		}
		for i := len(lines) - 1; i >= 0 && len(found) < n; i-- {
			if validFleetLogLine(lines[i].raw) {
				found = append(found, lines[i])
			}
		}
		scanned += end - (start + int64(cut))
		end = start + int64(cut)
		chunk = fleetLogChunk
	}
	out := make([]json.RawMessage, len(found))
	for i, l := range found {
		out[i] = json.RawMessage(l.raw)
	}
	var next *int64
	switch {
	case len(found) == n && found[len(found)-1].off > 0:
		o := found[len(found)-1].off
		next = &o
	case len(found) < n && end > 0: // scan budget ran out before byte 0
		o := end
		next = &o
	}
	return out, next, nil
}

// lastNewlineEnd returns the offset just past the last '\n' at or before end.
func lastNewlineEnd(f *os.File, end int64) (int64, error) {
	for pos := end; pos > 0; {
		start := max(0, pos-4096)
		buf := make([]byte, pos-start)
		if _, err := f.ReadAt(buf, start); err != nil && err != io.EOF {
			return 0, err
		}
		if i := bytes.LastIndexByte(buf, '\n'); i >= 0 {
			return start + int64(i) + 1, nil
		}
		pos = start
	}
	return 0, nil
}

func (s *server) fleetLog(w http.ResponseWriter, r *http.Request) {
	if s.fleetAuth(r) == fleetAuthNone {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	q := r.URL.Query()
	n := 50
	if v := q.Get("n"); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil || p < 1 || p > 200 {
			http.Error(w, "bad n", http.StatusBadRequest)
			return
		}
		n = p
	}
	before := int64(-1)
	if v := q.Get("before"); v != "" {
		p, err := strconv.ParseInt(v, 10, 64)
		if err != nil || p < 0 {
			http.Error(w, "bad before", http.StatusBadRequest)
			return
		}
		before = p
	}
	entries, next, err := s.readFleetLogPage(before, n)
	if err != nil {
		http.Error(w, "cannot read log", http.StatusInternalServerError)
		return
	}
	if entries == nil {
		entries = []json.RawMessage{}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries, "next": next})
}
