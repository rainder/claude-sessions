package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"
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
	// Worktree is set by kill when the killed session was the last one running
	// in a git worktree, so the client can offer to remove it. Omitted
	// otherwise; older clients ignore it, and a new client against an old
	// server simply never sees it.
	Worktree *worktreeInfo `json:"worktree,omitempty"`
}

// worktreeInfo describes a worktree a kill has just left idle.
type worktreeInfo struct {
	Path string `json:"path"` // worktree checkout root
	Name string `json:"name"` // last path element, for the prompt
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
	// codexUsageSnapshot returns this host's OpenAI Codex account usage. nil (no
	// hub) or a nil return (no fetch yet, or no Codex auth) omits the
	// "codex_usage" key — the account email rides in the snapshot itself.
	codexUsageSnapshot func() *CodexAccountUsage
	// previewLoader is the preview backend; nil means LoadPreview. Tests inject
	// a stub to assert bounds and header wiring without touching tmux.
	previewLoader func(int, PreviewLimits) (PreviewResult, error)

	// collect/terminate are injectable seams for tests; nil in production,
	// where they fall back to CollectLocal / KillSession.
	collect   func() ([]Session, error)
	terminate func(Session) error
	// removeTree removes a worktree checkout; nil falls back to RemoveWorktree,
	// so tests exercise the handler without a real git repo.
	removeTree func(string) error

	// devices is the push registry; nil when this server was built without
	// notification support, in which case the /devices routes report 503 rather
	// than panicking.
	devices *DeviceStore

	// pairing is the in-flight pairing offer, nil unless `pair` has armed one.
	// Guarded because arm and exchange arrive on different connections.
	pairingMu sync.Mutex
	pairing   *pairingCode

	sessionCache sessionCache

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

// collectLocal returns this host's live sessions. Disabled state is entirely
// client-side now (see state.go / the TUI overlay), so the server no longer
// annotates it — every session it reports has Disabled=false.
func (s *server) collectLocal() ([]Session, error) {
	return s.collectLocalRaw()
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

func (s *server) removeWorktreeAt(path string) error {
	if s.removeTree != nil {
		return s.removeTree(path)
	}
	return RemoveWorktree(path)
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
	// "codex_usage" is the OpenAI Codex equivalent, same optionality.
	if s.codexUsageSnapshot != nil {
		if u := s.codexUsageSnapshot(); u != nil {
			resp["codex_usage"] = u
		}
	}
	writeJSON(w, http.StatusOK, resp)
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
	writeJSON(w, http.StatusOK, struct {
		Presets []CommandPreset `json:"presets"`
	}{Presets: presets})
}

// resumable returns this host's resumable (past, ended) sessions, collected
// in-process — the same primitive the local TUI path uses.
func (s *server) resumable(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Sessions []ResumableSession `json:"sessions"`
	}{Sessions: CollectResumable()})
}

// resume spawns `claude --resume <id>` in a fresh tmux session for the given
// session id + cwd. A session that's already live is refused with 409;
// validation and the spawn go through the shared ResumeSession primitive.
func (s *server) resume(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	var body struct {
		SessionID string `json:"session_id"`
		CWD       string `json:"cwd"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if body.SessionID == "" {
		http.Error(w, "session_id required", http.StatusBadRequest)
		return
	}
	tname, err := ResumeSession(body.SessionID, body.CWD)
	if err != nil {
		if errors.Is(err, errResumeSessionLive) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		writeJSON(w, http.StatusOK, actionResult{Error: err.Error()})
		return
	}
	s.invalidateSessions()
	writeJSON(w, http.StatusOK, actionResult{OK: true, Tmux: tname})
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
	// Whether this kill empties a worktree is decided here, from the same
	// server-collected list: the client never gets to assert it.
	worktree := worktreeCleanupTarget(*target, sessions)
	if err := s.terminateSession(*target); err != nil {
		writeJSON(w, http.StatusOK, actionResult{Error: err.Error()})
		return
	}
	s.invalidateSessions()
	result := actionResult{OK: true}
	if worktree != "" {
		result.Worktree = &worktreeInfo{Path: worktree, Name: filepath.Base(worktree)}
	}
	writeJSON(w, http.StatusOK, result)
}

// removeWorktree handles POST /worktree/remove. The path arrives from a client,
// so it is validated (absolute, clean, worktree-shaped, a real git worktree)
// before anything touches disk, and re-checked against the live session list so
// a session started since the kill blocks the removal.
func (s *server) removeWorktree(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}
	if err := validateWorktreePath(req.Path); err != nil {
		writeJSON(w, http.StatusOK, actionResult{Error: err.Error()})
		return
	}
	sessions, err := s.collectLocal()
	if err != nil {
		writeJSON(w, http.StatusOK, actionResult{Error: err.Error()})
		return
	}
	for _, sess := range sessions {
		if worktreeRoot(sess.CWD) == req.Path {
			writeJSON(w, http.StatusOK, actionResult{
				Error: fmt.Sprintf("worktree still in use by PID %d", sess.PID),
			})
			return
		}
	}
	if err := s.removeWorktreeAt(req.Path); err != nil {
		writeJSON(w, http.StatusOK, actionResult{Error: err.Error()})
		return
	}
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

// registerDevice records an APNs device token for push delivery.
// POST /devices {"device_token": "...", "environment": "...", "platform": "..."}
//
// Upsert-by-token, so the app can (and should) re-register on every launch:
// APNs tokens change on restore, reinstall, and some OS upgrades, and a
// registration treated as permanent becomes a phone that silently stops
// receiving alerts.
func (s *server) registerDevice(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if s.devices == nil {
		http.Error(w, "notifications not configured on this host", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		DeviceToken string `json:"device_token"`
		Platform    string `json:"platform"`
		Environment string `json:"environment"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	if err := dec.Decode(&body); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if dec.More() {
		http.Error(w, "unexpected trailing json", http.StatusBadRequest)
		return
	}
	// APNs device tokens are hex. Validating the shape here keeps junk out of
	// the registry, where it would otherwise sit forever failing to deliver.
	if !isAPNsDeviceToken(body.DeviceToken) {
		http.Error(w, "device_token must be 64-200 hex characters", http.StatusBadRequest)
		return
	}
	switch body.Environment {
	case "", "production", "sandbox":
	default:
		http.Error(w, "environment must be production or sandbox", http.StatusBadRequest)
		return
	}
	switch body.Platform {
	case "", "ios":
	default:
		http.Error(w, "platform must be ios", http.StatusBadRequest)
		return
	}
	// The registry is unbounded otherwise: anything holding the bearer token
	// could grow it without limit, and every entry costs a push per alert.
	if len(s.devices.List()) >= maxRegisteredDevices && !s.devices.Has(body.DeviceToken) {
		http.Error(w, "too many registered devices", http.StatusConflict)
		return
	}
	s.devices.Upsert(Device{
		Token:       body.DeviceToken,
		Platform:    body.Platform,
		Environment: body.Environment,
	})
	w.WriteHeader(http.StatusNoContent)
}

// unregisterDevice drops a device token. DELETE /devices/{token}
func (s *server) unregisterDevice(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if s.devices == nil {
		http.Error(w, "notifications not configured on this host", http.StatusServiceUnavailable)
		return
	}
	s.devices.Remove(r.PathValue("token"))
	w.WriteHeader(http.StatusNoContent)
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

// tailscaleBundledPaths are places the CLI lives when it is not on PATH.
//
// The macOS GUI builds (App Store and standalone) ship the binary inside the
// app bundle and install no symlink, so a Mac can be fully authenticated to a
// tailnet with nothing named `tailscale` on any PATH. Without this the failure
// is indistinguishable from "Tailscale is down", and under a supervisor it is a
// permanent restart loop rather than one confusing line.
var tailscaleBundledPaths = []string{
	"/Applications/Tailscale.app/Contents/MacOS/Tailscale",
	"/usr/local/bin/tailscale",
	"/opt/homebrew/bin/tailscale",
}

// tailscaleBinary returns a runnable tailscale CLI path, or "" if there is
// none. PATH wins; the bundled locations are the fallback.
func tailscaleBinary() string {
	if p, err := exec.LookPath("tailscale"); err == nil {
		return p
	}
	for _, p := range tailscaleBundledPaths {
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
			return p
		}
	}
	return ""
}

// tailscaleBindFailure explains why `--bind tailscale` could not resolve. The
// two causes need different fixes and used to share one message: "is tailscaled
// running and authenticated?" sends you to check a daemon that is often running
// fine, when the real problem is that nothing named `tailscale` is executable.
func tailscaleBindFailure() string {
	if tailscaleBinary() == "" {
		return "--bind tailscale requested but no tailscale command was found\n" +
			"        it is not on PATH, and not at any of: " + strings.Join(tailscaleBundledPaths, ", ") + "\n" +
			"        the macOS app ships it inside the bundle — symlink it onto PATH,\n" +
			"        or pass the address directly: --bind <your-tailscale-ip>"
	}
	return "--bind tailscale requested but " + tailscaleBinary() + " reported no IPv4\n" +
		"        is tailscaled running and authenticated?"
}

// tailscaleIPv4Context is the context-bounded variant used by local client
// fallback, so address resolution cannot outlive its total operation deadline.
func tailscaleIPv4Context(ctx context.Context) string {
	bin := tailscaleBinary()
	if bin == "" {
		return ""
	}
	out, err := exec.CommandContext(ctx, bin, "ip", "-4").Output()
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
		h = h[:i]
	}
	// A zero-length hostname is permitted (a UTS namespace can set one), and it
	// reaches the pairing QR as a missing trailing field. Callers treat this as
	// a display name, so give them something rather than nothing.
	if h == "" {
		return "unknown"
	}
	return h
}

// serverFlags are the flags shared by `-s` and `service install`. Both parse
// them through parseServerFlags so a flag added to one can't go missing from
// the unit file the other writes.
type serverFlags struct {
	port int
	bind string
}

// parseServerFlags reads --port/--bind. An unrecognized flag is an error rather
// than a silent no-op, so a typo never reads as "use the default".
func parseServerFlags(args []string) (serverFlags, error) {
	f := serverFlags{port: defaultServerPort, bind: "127.0.0.1"}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--port":
			if i+1 >= len(args) {
				return f, fmt.Errorf("--port needs a value")
			}
			p, err := strconv.Atoi(args[i+1])
			if err != nil {
				return f, fmt.Errorf("bad port %q", args[i+1])
			}
			f.port = p
			i++
		case "--bind":
			if i+1 >= len(args) {
				return f, fmt.Errorf("--bind needs a value")
			}
			f.bind = args[i+1]
			i++
		default:
			return f, fmt.Errorf("unknown arg %q", args[i])
		}
	}
	return f, nil
}

// unbracket strips one layer of IPv6 literal brackets, so `::` and `[::]` are
// the same host to everything downstream.
func unbracket(host string) string {
	return strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
}

// hostPort joins a bind host and port into a listen address. Plain
// concatenation turns `--bind ::` into `:::8765`, which is not the address the
// user asked for; net.JoinHostPort brackets the literal properly.
func hostPort(host string, port int) string {
	return net.JoinHostPort(unbracket(host), strconv.Itoa(port))
}

// serverBanner renders the startup banner. showToken is false when stdout is
// not a terminal, and then the token is replaced by the path to read it from.
//
// The banner is the only thing this process writes to stdout, and it carries
// the auth token. Under a supervisor stdout is a log file or the journal —
// durable, unrotated, re-stamped on every restart, and exactly the file that
// gets attached to a bug report. `service install` keeps stdout off both, but
// a plain `claude-sessions -s | tee log` would still copy a token that lives
// 0600 on disk (serverTokenPath) into whatever mode the shell creates.
func serverBanner(host, bind string, port int, tok, bindHint string, showToken bool) string {
	shown := tok
	if !showToken {
		shown = "(hidden — stdout is not a terminal; read " + serverTokenPath() + ")"
	}
	return fmt.Sprintf(`claude-sessions server
  bind:     %s:%d%s
  hostname: %s
  token:    %s

add to client's ~/.config/claude-sessions/servers.yaml:
  servers:
    - name: %s
      host: %s
      port: %d
      token: %s

`, bind, port, bindHint, host, shown, host, bind, port, shown)
}

// cmdServer is the -s subcommand: starts the HTTP server in the foreground.
//
// Default bind is 127.0.0.1 (safe). For remote access:
//
//	--bind tailscale    auto-detect this host's Tailscale IPv4
//	--bind 0.0.0.0      every interface (not recommended)
//	--bind <addr>       any explicit address
func cmdServer(args []string) int {
	flags, err := parseServerFlags(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "server: %v\n", err)
		return 2
	}
	port, bind := flags.port, flags.bind

	// Magic value: resolve "tailscale" to this host's Tailscale IPv4.
	if bind == "tailscale" {
		ts := tailscaleIPv4()
		if ts == "" {
			fmt.Fprintln(os.Stderr, "server: "+tailscaleBindFailure())
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

	fmt.Print(serverBanner(host, bind, port, tok, bindHint, term.IsTerminal(int(os.Stdout.Fd()))))

	hostUsageHub := NewHostUsageHub(hostUsageInterval)
	defer hostUsageHub.Shutdown()

	// Account rate-limit usage: the same background poller the client uses, so a
	// remote host can surface its own account's limits (which may differ from the
	// client's account) in the client's header. The login email is read once at
	// startup — it's stable for the process's lifetime.
	usageHub := NewUsageHub()
	defer usageHub.Shutdown()

	// Codex account usage: same background poller, so a remote host also surfaces
	// its own Codex account's limits. Both snapshots are account-paired at fetch
	// time (the Anthropic email is re-read each fetch, the Codex email rides in
	// its payload), so a mid-run relogin re-attributes the limits and no separate
	// startup identity read is needed.
	codexUsageHub := NewCodexUsageHub()
	defer codexUsageHub.Shutdown()

	// The registry is shared: the /devices handlers write it and the push hub
	// reads it, so they must be the same store, not two views of one file.
	devices := LoadDeviceStore()

	s := &server{
		token:              tok,
		host:               host,
		hostSnapshot:       hostUsageHub.Snapshot,
		usageSnapshot:      usageHub.Snapshot,
		codexUsageSnapshot: codexUsageHub.Snapshot,
		devices:            devices,
	}

	// Push notifications are optional. Every failure here logs one line and
	// leaves the rest of the server untouched — a host without an APNs key runs
	// exactly as it always has.
	if cfg, err := LoadAPNsConfig(); err != nil {
		fmt.Fprintf(os.Stderr, "push notifications disabled (%v)\n", err)
	} else if client, err := newAPNsClient(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "push notifications disabled (%v)\n", err)
	} else {
		notifier := newNotifyHub(notifyHubOptions{
			HostName: host,
			HostID:   LoadHostID(),
			BundleID: cfg.BundleID,
			Devices:  devices,
			Sender:   client,
		})
		notifier.Start()
		defer notifier.Shutdown()
		fmt.Fprintf(os.Stderr, "push notifications enabled (%s, %d device(s))\n",
			cfg.Environment, len(devices.List()))
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /sessions", s.sessions)
	mux.HandleFunc("GET /cwd-suggestions", s.cwdSuggestions)
	mux.HandleFunc("GET /presets", s.presets)
	mux.HandleFunc("GET /resumable", s.resumable)
	mux.HandleFunc("POST /sessions/resume", s.resume)
	mux.HandleFunc("GET /sessions/{pid}/preview", s.preview)
	mux.HandleFunc("GET /sessions/{pid}/tmux-info", s.tmuxInfo)
	mux.HandleFunc("POST /sessions/{pid}/kill", s.kill)
	mux.HandleFunc("POST /sessions/{pid}/migrate", s.migrate)
	mux.HandleFunc("POST /sessions/new", s.newSession)
	mux.HandleFunc("POST /worktree/remove", s.removeWorktree)
	mux.HandleFunc("POST /devices", s.registerDevice)
	mux.HandleFunc("DELETE /devices/{token}", s.unregisterDevice)
	mux.HandleFunc("POST /pair/arm", s.armPairing)
	mux.HandleFunc("POST /pair/disarm", s.disarmPairing)
	mux.HandleFunc("POST /pair/exchange", s.pairExchange)
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

	// A non-loopback bind leaves clip-request's same-host POST with nothing to
	// dial, so serve /paste-request on loopback as well. Best-effort: a failure
	// here costs remote image paste, not the server.
	if lb := loopbackPasteAddr(bind, port); lb != "" {
		if ln, err := listenLoopbackPaste(lb, s); err != nil {
			fmt.Fprintf(os.Stderr, "remote image paste disabled (%s: %v)\n", lb, err)
		} else {
			defer ln.Close()
			fmt.Fprintf(os.Stderr, "paste requests also on %s\n", lb)
		}
	}

	addr := hostPort(bind, port)
	fmt.Fprintf(os.Stderr, "listening on %s\n", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		fmt.Fprintln(os.Stderr, "server:", err)
		return 1
	}
	return 0
}
