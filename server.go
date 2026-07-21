package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// HTTP server mode (-s flag). Exposes this host's sessions over JSON+bearer-
// auth so a client running elsewhere can include them in its live view.

// actionResult is the JSON shape returned by mutating endpoints.
// Mirrors the bash version so existing scripts/clients keep working.
type actionResult struct {
	OK     bool   `json:"ok"`
	Tmux   string `json:"tmux,omitempty"`  // tmux session name for migrate/new
	Error  string `json:"error,omitempty"` // human-readable failure reason
}

type server struct {
	token string
	host  string
}

func (s *server) authed(r *http.Request) bool {
	return r.Header.Get("Authorization") == "Bearer "+s.token
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
	sessions, err := CollectLocal()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"hostname": s.host,
		"ts":       time.Now().Unix(),
		"sessions": sessions,
	})
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
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(PreviewContent(pid)))
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
	if _, ok := readSessionByPID(pid); !ok {
		writeJSON(w, http.StatusOK, actionResult{Error: fmt.Sprintf("PID %d is not a live Claude session", pid)})
		return
	}
	if err := KillSession(pid); err != nil {
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
	writeJSON(w, http.StatusOK, actionResult{OK: true, Tmux: tname})
}

func (s *server) newSession(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	var body struct {
		CWD  string `json:"cwd"`
		Name string `json:"name"`
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
	tname, err := SpawnNew(body.CWD, body.Name)
	if err != nil {
		writeJSON(w, http.StatusOK, actionResult{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, actionResult{OK: true, Tmux: tname})
}

// loadOrCreateToken reads ~/.config/claude-sessions/server-token, creating it
// (0600) with a random value if missing. Returns the token on stdout for the
// admin to copy to client config.
func loadOrCreateToken() (string, error) {
	dir := ConfigDir()
	if dir == "" {
		return "", fmt.Errorf("no home directory")
	}
	path := filepath.Join(dir, "server-token")
	if data, err := os.ReadFile(path); err == nil {
		return strings.TrimSpace(string(data)), nil
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
	out, err := exec.Command("tailscale", "ip", "-4").Output()
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
	port := 8765
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

	s := &server{token: tok, host: host}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /sessions", s.sessions)
	mux.HandleFunc("GET /sessions/{pid}/preview", s.preview)
	mux.HandleFunc("GET /sessions/{pid}/tmux-info", s.tmuxInfo)
	mux.HandleFunc("POST /sessions/{pid}/kill", s.kill)
	mux.HandleFunc("POST /sessions/{pid}/migrate", s.migrate)
	mux.HandleFunc("POST /sessions/new", s.newSession)

	addr := fmt.Sprintf("%s:%d", bind, port)
	fmt.Fprintf(os.Stderr, "listening on %s\n", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		fmt.Fprintln(os.Stderr, "server:", err)
		return 1
	}
	return 0
}
