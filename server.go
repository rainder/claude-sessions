package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// HTTP server mode (-s flag). Exposes this host's sessions over JSON+bearer-
// auth so a client running elsewhere can include them in its live view.

// defaultServerPort is the port the server binds and clip-request POSTs to.
const defaultServerPort = 8765

// activeServerPort is the port this process's server is (or would be) reachable
// on. cmdServer sets it from its resolved --port so SpawnNew — called from the
// server without the port in hand — can embed the right port in the tmux paste
// binding. Stays at the default in non-server contexts (local CLI/TUI).
var activeServerPort = defaultServerPort

// actionResult is the JSON shape returned by mutating endpoints.
// Mirrors the bash version so existing scripts/clients keep working.
type actionResult struct {
	OK    bool   `json:"ok"`
	Tmux  string `json:"tmux,omitempty"`  // tmux session name for migrate/new
	Error string `json:"error,omitempty"` // human-readable failure reason
}

type sessionFlight struct {
	done       chan struct{}
	err        error
	generation uint64
}

type sessionCache struct {
	mu               sync.Mutex
	sessions         []Session
	completedAt      time.Time
	valid            bool
	cachedGeneration uint64
	generation       uint64
	flight           *sessionFlight
	now              func() time.Time
}

type server struct {
	token string
	host  string
	// hostSnapshot returns this host's latest resource usage; nil yields an
	// empty HostUsage so old clients and tests without a hub still get 200.
	hostSnapshot func() HostUsage
	// usageSnapshot returns this host's account rate-limit usage paired with the
	// account it belongs to. nil (no hub) or a nil return (no fetch yet) omits
	// the "usage" key, so old clients and tests without a hub still get 200.
	usageSnapshot func() *AccountUsage
	// previewLoader is the preview backend; nil means LoadPreview. Tests inject
	// a stub to assert bounds and header wiring without touching tmux.
	previewLoader func(int, PreviewLimits) (PreviewResult, error)

	// collect/terminate are injectable seams for tests; nil in production,
	// where they fall back to CollectLocal / KillSession.
	collect   func() ([]Session, error)
	terminate func(Session) error

	sessionCache sessionCache

	disabledMu         sync.RWMutex
	disabledSessionIDs map[string]struct{}
	disabledGeneration uint64

	// paste is the remote-image-paste broker (see paste.go); pb() lazily
	// initializes it so both cmdServer and tests get a working broker.
	pasteOnce sync.Once
	paste     *pasteBroker
}

func (s *server) authed(r *http.Request) bool {
	return subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+s.token)) == 1
}

func (s *server) collectLocalRaw() ([]Session, error) {
	if s.collect != nil {
		return s.collect()
	}
	return CollectLocal()
}

func (s *server) collectLocal() ([]Session, error) {
	s.disabledMu.RLock()
	disabledGeneration := s.disabledGeneration
	s.disabledMu.RUnlock()

	sessions, err := s.collectLocalRaw()
	if err != nil {
		return nil, err
	}
	s.annotateDisabled(sessions, disabledGeneration)
	return sessions, nil
}

func (s *server) annotateDisabled(sessions []Session, collectedGeneration uint64) {
	live := make(map[string]struct{}, len(sessions))
	for _, session := range sessions {
		if session.SessionID != "" {
			live[session.SessionID] = struct{}{}
		}
	}

	s.disabledMu.Lock()
	defer s.disabledMu.Unlock()
	if s.disabledGeneration == collectedGeneration {
		for sessionID := range s.disabledSessionIDs {
			if _, ok := live[sessionID]; !ok {
				delete(s.disabledSessionIDs, sessionID)
			}
		}
	}
	for i := range sessions {
		if sessions[i].SessionID == "" {
			sessions[i].Disabled = false
			continue
		}
		_, sessions[i].Disabled = s.disabledSessionIDs[sessions[i].SessionID]
	}
}

func (s *server) writeDisabled(sessionID string, disabled bool) {
	if sessionID == "" {
		return
	}
	s.disabledMu.Lock()
	defer s.disabledMu.Unlock()
	if s.disabledSessionIDs == nil {
		s.disabledSessionIDs = make(map[string]struct{})
	}
	if disabled {
		s.disabledSessionIDs[sessionID] = struct{}{}
	} else {
		delete(s.disabledSessionIDs, sessionID)
	}
	s.disabledGeneration++
}

func (c *sessionCache) timeNow() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func (s *server) cachedSessions() ([]Session, error) {
	cache := &s.sessionCache
	for {
		cache.mu.Lock()
		if cache.valid && cache.cachedGeneration == cache.generation && cache.timeNow().Before(cache.completedAt.Add(time.Second)) {
			sessions := cache.sessions
			cache.mu.Unlock()
			return sessions, nil
		}
		if flight := cache.flight; flight != nil {
			cache.mu.Unlock()
			<-flight.done

			cache.mu.Lock()
			err := flight.err
			currentGeneration := cache.generation
			flightGeneration := flight.generation
			cache.mu.Unlock()
			if err != nil && currentGeneration == flightGeneration {
				return nil, err
			}
			continue
		}

		flight := &sessionFlight{done: make(chan struct{})}
		cache.flight = flight
		cache.mu.Unlock()

		for {
			cache.mu.Lock()
			generation := cache.generation
			cache.mu.Unlock()

			sessions, err := s.collectLocal()
			completedAt := cache.timeNow()

			cache.mu.Lock()
			if cache.generation != generation {
				cache.mu.Unlock()
				continue
			}
			flight.err = err
			flight.generation = generation
			if err == nil {
				cache.sessions = sessions
				cache.completedAt = completedAt
				cache.cachedGeneration = generation
				cache.valid = true
			}
			cache.flight = nil
			close(flight.done)
			cache.mu.Unlock()
			return sessions, err
		}
	}
}

func (s *server) invalidateSessions() {
	cache := &s.sessionCache
	cache.mu.Lock()
	cache.generation++
	cache.sessions = nil
	cache.completedAt = time.Time{}
	cache.valid = false
	cache.mu.Unlock()
}

func (s *server) terminateSession(target Session) error {
	if s.terminate != nil {
		return s.terminate(target)
	}
	return KillSession(target)
}

func writeJSON(w http.ResponseWriter, code int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(data)
}

func (s *server) sessions(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	sessions, err := s.cachedSessions()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	hostUsage := HostUsage{}
	if s.hostSnapshot != nil {
		hostUsage = s.hostSnapshot()
	}
	resp := map[string]any{
		"hostname":  s.host,
		"ts":        time.Now().Unix(),
		"hostUsage": hostUsage,
		"sessions":  sessions,
	}
	// "usage" is optional: present only once this host's poller has a snapshot,
	// so a client can attribute the limits to this host's account. Omitted when
	// absent — older clients ignore it and it never nulls out the response.
	if s.usageSnapshot != nil {
		if u := s.usageSnapshot(); u != nil {
			resp["usage"] = u
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *server) setDisabled(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	pid, err := strconv.Atoi(r.PathValue("pid"))
	if err != nil {
		http.Error(w, "bad pid", http.StatusBadRequest)
		return
	}

	var body struct {
		Disabled  *bool   `json:"disabled"`
		SessionID *string `json:"sessionId"`
	}
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if body.Disabled == nil {
		http.Error(w, "disabled boolean required", http.StatusBadRequest)
		return
	}
	if body.SessionID == nil || *body.SessionID == "" {
		http.Error(w, "non-empty sessionId required", http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		http.Error(w, "request body must contain one JSON object", http.StatusBadRequest)
		return
	}

	sessions, err := s.collectLocalRaw()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var target *Session
	for i := range sessions {
		if sessions[i].PID == pid {
			target = &sessions[i]
			break
		}
	}
	if target == nil {
		http.Error(w, fmt.Sprintf("PID %d is not a live Claude session", pid), http.StatusNotFound)
		return
	}
	if target.SessionID == "" {
		http.Error(w, fmt.Sprintf("PID %d has no stable session ID", pid), http.StatusConflict)
		return
	}
	if target.SessionID != *body.SessionID {
		http.Error(
			w,
			fmt.Sprintf(
				"PID %d now belongs to session %q, not %q",
				pid,
				target.SessionID,
				*body.SessionID,
			),
			http.StatusConflict,
		)
		return
	}

	s.writeDisabled(target.SessionID, *body.Disabled)
	s.invalidateSessions()
	writeJSON(w, http.StatusOK, struct {
		Disabled  bool   `json:"disabled"`
		SessionID string `json:"sessionId"`
	}{
		Disabled:  *body.Disabled,
		SessionID: target.SessionID,
	})
}

func (s *server) cwdSuggestions(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	home, _ := os.UserHomeDir()
	writeJSON(w, http.StatusOK, struct {
		Home        string          `json:"home"`
		Suggestions []cwdSuggestion `json:"suggestions"`
	}{Home: home, Suggestions: collectCwdSuggestions()})
}

// presets lists this host's configured command preset names (not the command
// text itself — that's local shell input a remote client has no business
// seeing or replaying). Used by remote clients to validate `--command` and
// populate the new-session picker without guessing at this host's config.
func (s *server) presets(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	presets, err := LoadCommandPresets()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	names := make([]string, len(presets))
	for i, p := range presets {
		names[i] = p.Name
	}
	writeJSON(w, http.StatusOK, struct {
		Presets []string `json:"presets"`
	}{Presets: names})
}

func (s *server) preview(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	pid, err := strconv.Atoi(r.PathValue("pid"))
	if err != nil {
		http.Error(w, "bad pid", http.StatusBadRequest)
		return
	}
	limits, err := previewLimitsFromRequest(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	load := s.previewLoader
	if load == nil {
		load = LoadPreview
	}
	result, err := load(pid, limits)
	if err != nil {
		if errors.Is(err, errSessionEnded) {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Claude-Sessions-Preview-Source", result.Source)
	w.Header().Set("X-Claude-Sessions-Preview-Label", result.Label)
	_, _ = w.Write([]byte(result.Content))
}

// previewLimitsFromRequest reads optional lines/bytes query params, defaulting
// to DefaultPreviewLimits. Values are accepted only within 1..2000 lines and
// 1024..524288 bytes; anything else (non-numeric, negative, out of range) is an
// error the handler turns into 400.
func previewLimitsFromRequest(r *http.Request) (PreviewLimits, error) {
	limits := DefaultPreviewLimits()
	q := r.URL.Query()
	if v := q.Get("lines"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 2000 {
			return PreviewLimits{}, fmt.Errorf("bad lines value: %s", v)
		}
		limits.MaxLines = n
	}
	if v := q.Get("bytes"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1024 || n > 524288 {
			return PreviewLimits{}, fmt.Errorf("bad bytes value: %s", v)
		}
		limits.MaxBytes = n
	}
	return limits, nil
}

func (s *server) tmuxInfo(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	pid, err := strconv.Atoi(r.PathValue("pid"))
	if err != nil {
		http.Error(w, "bad pid", http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":   true,
		"tmux": tmuxSessionForPID(pid),
	})
}

func (s *server) kill(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	pid, err := strconv.Atoi(r.PathValue("pid"))
	if err != nil {
		http.Error(w, "bad pid", http.StatusBadRequest)
		return
	}
	// Trust only server-collected metadata: resolve the PID against this host's
	// own sessions and terminate that full row. The request body carries no
	// tmux metadata — the client cannot steer which target we signal.
	sessions, err := s.collectLocal()
	if err != nil {
		writeJSON(w, http.StatusOK, actionResult{Error: err.Error()})
		return
	}
	var target *Session
	for i := range sessions {
		if sessions[i].PID == pid {
			target = &sessions[i]
			break
		}
	}
	if target == nil {
		writeJSON(w, http.StatusOK, actionResult{Error: fmt.Sprintf("PID %d is not a live Claude session", pid)})
		return
	}
	if err := s.terminateSession(*target); err != nil {
		writeJSON(w, http.StatusOK, actionResult{Error: err.Error()})
		return
	}
	s.invalidateSessions()
	writeJSON(w, http.StatusOK, actionResult{OK: true})
}

func (s *server) migrate(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	pid, err := strconv.Atoi(r.PathValue("pid"))
	if err != nil {
		http.Error(w, "bad pid", http.StatusBadRequest)
		return
	}
	tname, err := MigrateLocal(pid)
	if err != nil {
		writeJSON(w, http.StatusOK, actionResult{Error: err.Error()})
		return
	}
	s.invalidateSessions()
	writeJSON(w, http.StatusOK, actionResult{OK: true, Tmux: tname})
}

func (s *server) newSession(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	var body struct {
		CWD     string `json:"cwd"`
		Name    string `json:"name"`
		Command string `json:"command"` // preset name, never raw command text
		Prompt  string `json:"prompt"`  // free text; shell-quoted before use, never interpreted
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if body.CWD == "" {
		http.Error(w, "cwd required", http.StatusBadRequest)
		return
	}
	body.CWD = expandTilde(body.CWD)
	if !isDir(body.CWD) {
		writeJSON(w, http.StatusOK, actionResult{Error: "not a directory: " + body.CWD})
		return
	}
	presets, err := LoadCommandPresets()
	if err != nil {
		writeJSON(w, http.StatusOK, actionResult{Error: err.Error()})
		return
	}
	// LoadCommandPresets always yields a non-empty slice (falls back to the
	// default Claude preset), so presets[0] is a safe backward-compatible
	// default for clients that omit command. A named command must match this
	// server's own allowlist — raw command text is never accepted.
	preset := presets[0]
	if body.Command != "" {
		var ok bool
		preset, ok = findCommandPreset(presets, body.Command)
		if !ok {
			writeJSON(w, http.StatusOK, actionResult{Error: "command preset not configured: " + body.Command})
			return
		}
	}
	command := preset.Command
	if body.Prompt != "" {
		command = command + " " + shellQuote(body.Prompt)
	}
	tname, err := SpawnNew(body.CWD, body.Name, command)
	if err != nil {
		writeJSON(w, http.StatusOK, actionResult{Error: err.Error()})
		return
	}
	if body.Prompt != "" {
		// The client won't attach to this session, so nobody's there to accept
		// a first-run workspace trust dialog for body.CWD — dismiss it here if
		// it shows, without blocking the response on the poll.
		go dismissTrustPrompt(tname)
	}
	s.invalidateSessions()
	writeJSON(w, http.StatusOK, actionResult{OK: true, Tmux: tname})
}

// serverTokenPath is the on-disk location of the shared bearer token, or "" if
// there's no home directory.
func serverTokenPath() string {
	dir := ConfigDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "server-token")
}

// readServerToken reads the existing server token without creating one. Used by
// same-host tooling (clip-request) that must not mint a token the running
// server never loaded.
func readServerToken() (string, error) {
	path := serverTokenPath()
	if path == "" {
		return "", fmt.Errorf("no home directory")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	tok := strings.TrimSpace(string(data))
	if tok == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return tok, nil
}

// loadOrCreateToken reads ~/.config/claude-sessions/server-token, creating it
// (0600) with a random value if missing. Returns the token on stdout for the
// admin to copy to client config.
func loadOrCreateToken() (string, error) {
	dir := ConfigDir()
	if dir == "" {
		return "", fmt.Errorf("no home directory")
	}
	path := serverTokenPath()
	if data, err := os.ReadFile(path); err == nil {
		tok := strings.TrimSpace(string(data))
		if tok == "" {
			return "", fmt.Errorf("%s exists but is empty; delete it to regenerate", path)
		}
		return tok, nil
	}
	b := make([]byte, 18) // 24 base64url chars
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	tok := base64.RawURLEncoding.EncodeToString(b)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
		return "", err
	}
	return tok, nil
}

// tailscaleIPv4 returns the host's Tailscale IPv4 address (or "" if Tailscale
// isn't installed/connected). Defense-in-depth alongside the bearer token.
func tailscaleIPv4() string {
	return tailscaleIPv4Context(context.Background())
}

// tailscaleIPv4Context is the context-bounded variant used by local client
// fallback, so address resolution cannot outlive its total operation deadline.
func tailscaleIPv4Context(ctx context.Context) string {
	out, err := exec.CommandContext(ctx, "tailscale", "ip", "-4").Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		s := strings.TrimSpace(line)
		if s != "" {
			return s
		}
	}
	return ""
}

// shortHostname returns hostname without the domain suffix.
func shortHostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	if i := strings.Index(h, "."); i >= 0 {
		return h[:i]
	}
	return h
}

// cmdServer is the -s subcommand: starts the HTTP server in the foreground.
//
// Default bind is 127.0.0.1 (safe). For remote access:
//
//	--bind tailscale    auto-detect this host's Tailscale IPv4
//	--bind 0.0.0.0      every interface (not recommended)
//	--bind <addr>       any explicit address
func cmdServer(args []string) int {
	port := defaultServerPort
	bind := "127.0.0.1"
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--port":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "server: --port needs a value")
				return 2
			}
			p, err := strconv.Atoi(args[i+1])
			if err != nil {
				fmt.Fprintf(os.Stderr, "server: bad port %q\n", args[i+1])
				return 2
			}
			port = p
			i++
		case "--bind":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "server: --bind needs a value")
				return 2
			}
			bind = args[i+1]
			i++
		default:
			fmt.Fprintf(os.Stderr, "server: unknown arg %q\n", args[i])
			return 2
		}
	}

	// Magic value: resolve "tailscale" to this host's Tailscale IPv4.
	if bind == "tailscale" {
		ts := tailscaleIPv4()
		if ts == "" {
			fmt.Fprintln(os.Stderr, "server: --bind tailscale requested but no Tailscale IPv4 found")
			fmt.Fprintln(os.Stderr, "        is tailscaled running and authenticated?")
			return 1
		}
		bind = ts
	}

	tok, err := loadOrCreateToken()
	if err != nil {
		fmt.Fprintln(os.Stderr, "server:", err)
		return 1
	}
	host := shortHostname()

	bindHint := ""
	if bind == "127.0.0.1" || bind == "localhost" {
		bindHint = "  " + dim("(loopback — pass --bind tailscale or --bind 0.0.0.0 for remote access)")
	}

	fmt.Printf(`claude-sessions server
  bind:     %s:%d%s
  hostname: %s
  token:    %s

add to client's ~/.config/claude-sessions/servers.yaml:
  servers:
    - name: %s
      host: %s
      port: %d
      token: %s

`, bind, port, bindHint, host, tok, host, bind, port, tok)

	hostUsageHub := NewHostUsageHub(hostUsageInterval)
	defer hostUsageHub.Shutdown()

	// Account rate-limit usage: the same background poller the client uses, so a
	// remote host can surface its own account's limits (which may differ from the
	// client's account) in the client's header. The login email is read once at
	// startup — it's stable for the process's lifetime.
	usageHub := NewUsageHub()
	defer usageHub.Shutdown()
	accountEmail := loadAccountEmail()

	s := &server{
		token:        tok,
		host:         host,
		hostSnapshot: hostUsageHub.Snapshot,
		usageSnapshot: func() *AccountUsage {
			info := usageHub.Snapshot()
			if info == nil {
				return nil
			}
			return &AccountUsage{Account: accountEmail, Info: info}
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /sessions", s.sessions)
	mux.HandleFunc("PUT /sessions/{pid}/disabled", s.setDisabled)
	mux.HandleFunc("GET /cwd-suggestions", s.cwdSuggestions)
	mux.HandleFunc("GET /presets", s.presets)
	mux.HandleFunc("GET /sessions/{pid}/preview", s.preview)
	mux.HandleFunc("GET /sessions/{pid}/tmux-info", s.tmuxInfo)
	mux.HandleFunc("POST /sessions/{pid}/kill", s.kill)
	mux.HandleFunc("POST /sessions/{pid}/migrate", s.migrate)
	mux.HandleFunc("POST /sessions/new", s.newSession)
	mux.HandleFunc("GET /paste-wait", s.pasteWait)
	mux.HandleFunc("POST /paste-request", s.pasteRequest)
	mux.HandleFunc("POST /paste", s.pasteUpload)

	// Publish the resolved port so SpawnNew (invoked without it) embeds the right
	// port in the tmux paste binding. Intercept Ctrl+V in tmux so remote-image
	// paste works, and drop any paste temp files left behind by an earlier run.
	// Both are linux-only no-ops elsewhere. Re-assert the binding periodically in
	// case the tmux server was restarted (or first started) after us.
	activeServerPort = port
	installPasteBinding(port)
	gcOldPastes(time.Now(), pasteGCMaxAge)
	if runtime.GOOS == "linux" {
		go func() {
			t := time.NewTicker(60 * time.Second)
			defer t.Stop()
			for range t.C {
				installPasteBinding(port)
			}
		}()
	}

	addr := fmt.Sprintf("%s:%d", bind, port)
	fmt.Fprintf(os.Stderr, "listening on %s\n", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		fmt.Fprintln(os.Stderr, "server:", err)
		return 1
	}
	return 0
}
