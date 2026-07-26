package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNewSessionExpandsTildeBeforeValidation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	req := httptest.NewRequest(http.MethodPost, "/sessions/new",
		strings.NewReader(`{"cwd":"~/missing"}`))
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()

	s := &server{token: "test-token"}
	s.newSession(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var got actionResult
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	want := "not a directory: " + filepath.Join(home, "missing")
	if got.Error != want {
		t.Fatalf("error = %q, want %q", got.Error, want)
	}
}

func TestCwdSuggestionsRequiresAuth(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/cwd-suggestions", nil)
	rec := httptest.NewRecorder()
	(&server{token: "secret"}).cwdSuggestions(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestCwdSuggestionsReturnsRankedHistory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := filepath.Join(home, "project")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	sessionDir := filepath.Join(home, ".claude", "sessions")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "1.json"), []byte(fmt.Sprintf(`{"pid":1,"cwd":%q}`, cwd)), 0o644); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/cwd-suggestions", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	(&server{token: "secret"}).cwdSuggestions(rec, req)

	var got struct {
		Home        string          `json:"home"`
		Suggestions []cwdSuggestion `json:"suggestions"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Suggestions) != 1 || got.Suggestions[0].CWD != cwd {
		t.Fatalf("suggestions = %#v", got.Suggestions)
	}
	if got.Home != home {
		t.Fatalf("home = %q, want %q", got.Home, home)
	}
}

func TestPresetsRequiresAuth(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/presets", nil)
	rec := httptest.NewRecorder()
	(&server{token: "secret"}).presets(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestPresetsReturnsNamesOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeCommandConfig(t, home)

	req := httptest.NewRequest(http.MethodGet, "/presets", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	(&server{token: "secret"}).presets(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	var got struct {
		Presets []string `json:"presets"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	want := []string{"Claude", "Fable"}
	if len(got.Presets) != len(want) || got.Presets[0] != want[0] || got.Presets[1] != want[1] {
		t.Fatalf("presets = %#v, want %#v", got.Presets, want)
	}
	if strings.Contains(body, "claude --model fable") {
		t.Fatal("response leaked command text, want names only")
	}
}

func writeCommandConfig(t *testing.T, home string) {
	t.Helper()
	dir := filepath.Join(home, ".config", "claude-sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data := "commands:\n  - name: Claude\n    command: claude\n  - name: Fable\n    command: claude --model fable\n"
	if err := os.WriteFile(filepath.Join(dir, "servers.yaml"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestNewSessionRejectsUnknownCommandPreset(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeCommandConfig(t, home)
	req := httptest.NewRequest(http.MethodPost, "/sessions/new", strings.NewReader(fmt.Sprintf(`{"cwd":%q,"command":"Unknown"}`, home)))
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	(&server{token: "test-token"}).newSession(rec, req)
	var got actionResult
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Error != "command preset not configured: Unknown" {
		t.Fatalf("error = %q", got.Error)
	}
}

func installFakeTmux(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "tmux.log")
	script := filepath.Join(dir, "tmux")
	body := "#!/bin/sh\nfor arg in \"$@\"; do printf '<%s>' \"$arg\"; done >> \"$TMUX_LOG\"\nprintf '\\n' >> \"$TMUX_LOG\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TMUX_LOG", logPath)
	return logPath
}

func TestNewSessionMissingCommandUsesFirstPreset(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeCommandConfig(t, home)
	logPath := installFakeTmux(t)

	req := httptest.NewRequest(http.MethodPost, "/sessions/new", strings.NewReader(fmt.Sprintf(`{"cwd":%q}`, home)))
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	(&server{token: "test-token"}).newSession(rec, req)

	var got actionResult
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if !got.OK || got.Tmux == "" {
		t.Fatalf("result = %#v", got)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "<claude><Enter>") {
		t.Fatalf("tmux argv:\n%s", data)
	}
}

func TestPreviewHandlerDefaultsAndHeaders(t *testing.T) {
	var got PreviewLimits
	s := &server{token: "test", previewLoader: func(pid int, limits PreviewLimits) (PreviewResult, error) {
		got = limits
		return PreviewResult{Source: "tmux", Label: "dev:0.0", Content: "hello\n"}, nil
	}}
	req := httptest.NewRequest(http.MethodGet, "/sessions/42/preview", nil)
	req.SetPathValue("pid", "42")
	req.Header.Set("Authorization", "Bearer test")
	rec := httptest.NewRecorder()
	s.preview(rec, req)
	if got != DefaultPreviewLimits() {
		t.Fatalf("limits = %#v", got)
	}
	if rec.Header().Get("X-Claude-Sessions-Preview-Source") != "tmux" {
		t.Fatalf("headers = %#v", rec.Header())
	}
	if rec.Header().Get("X-Claude-Sessions-Preview-Label") != "dev:0.0" {
		t.Fatalf("label header = %q", rec.Header().Get("X-Claude-Sessions-Preview-Label"))
	}
	if rec.Body.String() != "hello\n" {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

func TestPreviewHandlerParsesQueryLimits(t *testing.T) {
	var got PreviewLimits
	s := &server{token: "test", previewLoader: func(pid int, limits PreviewLimits) (PreviewResult, error) {
		got = limits
		return PreviewResult{Source: "tmux", Content: "x"}, nil
	}}
	req := httptest.NewRequest(http.MethodGet, "/sessions/42/preview?lines=40&bytes=4096", nil)
	req.SetPathValue("pid", "42")
	req.Header.Set("Authorization", "Bearer test")
	rec := httptest.NewRecorder()
	s.preview(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got != (PreviewLimits{MaxLines: 40, MaxBytes: 4096}) {
		t.Fatalf("limits = %#v", got)
	}
}

func TestPreviewHandlerRejectsBadLimits(t *testing.T) {
	cases := []string{
		"lines=0", "lines=-5", "lines=2001", "lines=abc",
		"bytes=0", "bytes=1023", "bytes=524289", "bytes=xyz",
	}
	for _, q := range cases {
		s := &server{token: "test", previewLoader: func(pid int, limits PreviewLimits) (PreviewResult, error) {
			t.Fatalf("loader must not run for %q", q)
			return PreviewResult{}, nil
		}}
		req := httptest.NewRequest(http.MethodGet, "/sessions/42/preview?"+q, nil)
		req.SetPathValue("pid", "42")
		req.Header.Set("Authorization", "Bearer test")
		rec := httptest.NewRecorder()
		s.preview(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("query %q: status = %d, want 400", q, rec.Code)
		}
	}
}

func TestPreviewHandlerMapsSessionEndedTo404(t *testing.T) {
	s := &server{token: "test", previewLoader: func(pid int, limits PreviewLimits) (PreviewResult, error) {
		return PreviewResult{}, errSessionEnded
	}}
	req := httptest.NewRequest(http.MethodGet, "/sessions/42/preview", nil)
	req.SetPathValue("pid", "42")
	req.Header.Set("Authorization", "Bearer test")
	rec := httptest.NewRecorder()
	s.preview(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestPreviewHandlerMapsOtherErrorsTo500(t *testing.T) {
	s := &server{token: "test", previewLoader: func(pid int, limits PreviewLimits) (PreviewResult, error) {
		return PreviewResult{}, errors.New("tmux capture-pane: boom")
	}}
	req := httptest.NewRequest(http.MethodGet, "/sessions/42/preview", nil)
	req.SetPathValue("pid", "42")
	req.Header.Set("Authorization", "Bearer test")
	rec := httptest.NewRecorder()
	s.preview(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

// writeServerYAML points a single named remote server at addr (host:port).
func writeServerYAML(t *testing.T, home, name, host, port, token string) {
	t.Helper()
	dir := filepath.Join(home, ".config", "claude-sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data := fmt.Sprintf("servers:\n  - name: %s\n    host: %s\n    port: %s\n    token: %s\n",
		name, host, port, token)
	if err := os.WriteFile(filepath.Join(dir, "servers.yaml"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFetchRemotePreview(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		if r.URL.Query().Get("lines") == "" || r.URL.Query().Get("bytes") == "" {
			t.Errorf("missing limit query params: %s", r.URL.RawQuery)
		}
		w.Header().Set("X-Claude-Sessions-Preview-Source", "tmux")
		w.Header().Set("X-Claude-Sessions-Preview-Label", "dev:0.0")
		_, _ = w.Write([]byte("remote hello\n"))
	}))
	defer backend.Close()

	u, _ := url.Parse(backend.URL)
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeServerYAML(t, home, "box", u.Hostname(), u.Port(), "secret")

	got, err := fetchRemotePreview("box", 42, DefaultPreviewLimits())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got.Source != "tmux" || got.Label != "dev:0.0" || got.Content != "remote hello\n" {
		t.Fatalf("result = %#v", got)
	}
}

func TestFetchRemotePreviewMaps404ToSessionEnded(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "session ended", http.StatusNotFound)
	}))
	defer backend.Close()

	u, _ := url.Parse(backend.URL)
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeServerYAML(t, home, "box", u.Hostname(), u.Port(), "secret")

	_, err := fetchRemotePreview("box", 42, DefaultPreviewLimits())
	if !errors.Is(err, errSessionEnded) {
		t.Fatalf("err = %v, want errSessionEnded", err)
	}
}

func TestFetchRemotePreviewRejectsOversizedBody(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 4096)))
	}))
	defer backend.Close()

	u, _ := url.Parse(backend.URL)
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeServerYAML(t, home, "box", u.Hostname(), u.Port(), "secret")

	_, err := fetchRemotePreview("box", 42, PreviewLimits{MaxLines: 10, MaxBytes: 1024})
	if err == nil {
		t.Fatal("want error for oversized body")
	}
}

func TestFetchRemotePresets(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"presets":["Claude","Fable"]}`))
	}))
	defer backend.Close()

	u, _ := url.Parse(backend.URL)
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeServerYAML(t, home, "box", u.Hostname(), u.Port(), "secret")

	got, err := fetchRemotePresets("box")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	want := []string{"Claude", "Fable"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("presets = %#v, want %#v", got, want)
	}
}

func TestFetchRemotePresetsMaps404ToSentinel(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer backend.Close()

	u, _ := url.Parse(backend.URL)
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeServerYAML(t, home, "box", u.Hostname(), u.Port(), "secret")

	_, err := fetchRemotePresets("box")
	if !errors.Is(err, errPresetsUnavailable) {
		t.Fatalf("err = %v, want errPresetsUnavailable", err)
	}
}

func TestNewSessionKnownPresetUsesItsCommand(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeCommandConfig(t, home)
	logPath := installFakeTmux(t)

	req := httptest.NewRequest(http.MethodPost, "/sessions/new", strings.NewReader(fmt.Sprintf(`{"cwd":%q,"command":"Fable"}`, home)))
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	(&server{token: "test-token"}).newSession(rec, req)

	var got actionResult
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if !got.OK || got.Tmux == "" {
		t.Fatalf("result = %#v", got)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "<claude --model fable><Enter>") {
		t.Fatalf("tmux argv:\n%s", data)
	}
}

// TestNewSessionPromptIsShellQuoted: a prompt is appended to the preset
// command as a single shell-quoted argument, so shell metacharacters in the
// prompt (backticks, $(), quotes) land as literal text typed into the fresh
// pane's shell rather than executing.
func TestNewSessionPromptIsShellQuoted(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeCommandConfig(t, home)
	logPath := installFakeTmux(t)

	body, _ := json.Marshal(map[string]string{
		"cwd":    home,
		"prompt": `fix the $(whoami) bug; say 'hi'`,
	})
	req := httptest.NewRequest(http.MethodPost, "/sessions/new", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	(&server{token: "test-token"}).newSession(rec, req)

	var got actionResult
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if !got.OK || got.Tmux == "" {
		t.Fatalf("result = %#v", got)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	want := "<claude " + shellQuote(`fix the $(whoami) bug; say 'hi'`) + "><Enter>"
	if !strings.Contains(string(data), want) {
		t.Fatalf("tmux argv missing quoted prompt:\ngot:  %s\nwant substring: %s", data, want)
	}
}

// TestKillHandlerUsesServerDerivedSession: the kill handler resolves the PID
// against server-collected rows and terminates that exact server-derived
// session — never a client-supplied one.
func TestKillHandlerUsesServerDerivedSession(t *testing.T) {
	want := Session{PID: 55, Tmux: "remote-work:2.1"}
	var got Session
	terminated := false
	s := &server{
		token: "secret",
		collect: func() ([]Session, error) {
			return []Session{want}, nil
		},
		terminate: func(target Session) error {
			got = target
			terminated = true
			return nil
		},
	}
	req := httptest.NewRequest("POST", "/sessions/55/kill", nil)
	req.SetPathValue("pid", "55")
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()

	s.kill(rec, req)

	if !terminated {
		t.Fatalf("terminate not called")
	}
	if got != want {
		t.Fatalf("terminated session = %#v, want %#v", got, want)
	}
	var r actionResult
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !r.OK {
		t.Fatalf("response OK = false, error = %q", r.Error)
	}
}

// TestKillHandlerUnknownPIDDoesNotTerminate: a PID missing from the collected
// rows yields the not-live error and never calls terminate.
func TestKillHandlerUnknownPIDDoesNotTerminate(t *testing.T) {
	terminated := false
	s := &server{
		token:     "secret",
		collect:   func() ([]Session, error) { return nil, nil },
		terminate: func(Session) error { terminated = true; return nil },
	}
	req := httptest.NewRequest("POST", "/sessions/55/kill", nil)
	req.SetPathValue("pid", "55")
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()

	s.kill(rec, req)

	if terminated {
		t.Fatalf("terminate called for unknown pid")
	}
	var r actionResult
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if r.OK {
		t.Fatalf("response OK = true for unknown pid")
	}
	if r.Error == "" {
		t.Fatalf("expected not-live error, got empty")
	}
}

// TestKillHandlerCollectionErrorDoesNotTerminate: a collection failure is
// surfaced in actionResult.Error and terminate is never called.
func TestKillHandlerCollectionErrorDoesNotTerminate(t *testing.T) {
	terminated := false
	s := &server{
		token:     "secret",
		collect:   func() ([]Session, error) { return nil, errors.New("collect boom") },
		terminate: func(Session) error { terminated = true; return nil },
	}
	req := httptest.NewRequest("POST", "/sessions/55/kill", nil)
	req.SetPathValue("pid", "55")
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()

	s.kill(rec, req)

	if terminated {
		t.Fatalf("terminate called despite collection error")
	}
	var r actionResult
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if r.Error != "collect boom" {
		t.Fatalf("Error = %q, want %q", r.Error, "collect boom")
	}
}

// TestKillHandlerUnauthorized: a missing bearer token returns HTTP 401 and
// never touches the collector or terminator.
func TestKillHandlerUnauthorized(t *testing.T) {
	terminated := false
	collected := false
	s := &server{
		token:     "secret",
		collect:   func() ([]Session, error) { collected = true; return nil, nil },
		terminate: func(Session) error { terminated = true; return nil },
	}
	req := httptest.NewRequest("POST", "/sessions/55/kill", nil)
	req.SetPathValue("pid", "55")
	rec := httptest.NewRecorder()

	s.kill(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	if collected {
		t.Fatalf("collect called without auth")
	}
	if terminated {
		t.Fatalf("terminate called without auth")
	}
}

func TestSessionsIncludesHostUsage(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cpu, memory := 12.5, 67.25
	s := &server{
		token: "secret",
		host:  "devbox",
		hostSnapshot: func() HostUsage {
			return HostUsage{CPUPercent: &cpu, MemoryPercent: &memory}
		},
	}
	req := httptest.NewRequest(http.MethodGet, "/sessions", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.sessions(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
	}
	var got struct {
		Hostname  string    `json:"hostname"`
		HostUsage HostUsage `json:"hostUsage"`
		Sessions  []Session `json:"sessions"`
		TS        int64     `json:"ts"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Hostname != "devbox" || got.TS == 0 {
		t.Fatalf("response metadata = %#v", got)
	}
	assertFloatPtr(t, got.HostUsage.CPUPercent, &cpu)
	assertFloatPtr(t, got.HostUsage.MemoryPercent, &memory)
}

func TestSessionsEmitsNestedLoadAverage(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cpu, memory := 12.5, 67.25
	s := &server{
		token: "secret",
		host:  "devbox",
		hostSnapshot: func() HostUsage {
			return HostUsage{CPUPercent: &cpu, MemoryPercent: &memory, Load: hostLoadAverage(1.24, 0.96, 0.72)}
		},
	}
	req := httptest.NewRequest(http.MethodGet, "/sessions", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.sessions(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
	}
	// Navigate the raw JSON so the exact wire key names are asserted, not just
	// that HostUsage's struct tags happen to round-trip.
	var raw map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	hostUsage, ok := raw["hostUsage"].(map[string]any)
	if !ok {
		t.Fatalf("hostUsage not an object: %#v", raw["hostUsage"])
	}
	if hostUsage["cpuPercent"] != 12.5 || hostUsage["memoryPercent"] != 67.25 {
		t.Fatalf("CPU/MEM not preserved alongside load: %#v", hostUsage)
	}
	load, ok := hostUsage["loadAverage"].(map[string]any)
	if !ok {
		t.Fatalf("loadAverage not an object: %#v", hostUsage["loadAverage"])
	}
	if load["oneMinute"] != 1.24 || load["fiveMinutes"] != 0.96 || load["fifteenMinutes"] != 0.72 {
		t.Fatalf("loadAverage keys/values wrong: %#v", load)
	}
}

func TestSessionsIncludesEmptyHostUsageWhenUnavailable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := &server{token: "secret", host: "devbox"}
	req := httptest.NewRequest(http.MethodGet, "/sessions", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.sessions(rec, req)
	if !strings.Contains(rec.Body.String(), `"hostUsage":{}`) {
		t.Fatalf("response missing empty hostUsage object: %s", rec.Body.String())
	}
}

func TestSessionsIncludesUsageWhenSnapshotPresent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := &server{
		token: "secret",
		host:  "devbox",
		usageSnapshot: func() *AccountUsage {
			return &AccountUsage{
				Account: "andy@work.com",
				Info:    &UsageInfo{FiveHour: usageBucket{Pct: 42}},
			}
		},
	}
	req := httptest.NewRequest(http.MethodGet, "/sessions", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.sessions(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
	}
	var raw map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	usage, ok := raw["usage"].(map[string]any)
	if !ok {
		t.Fatalf("usage not an object: %#v", raw["usage"])
	}
	if usage["account"] != "andy@work.com" {
		t.Fatalf("usage.account = %#v, want andy@work.com", usage["account"])
	}
	if _, ok := usage["info"].(map[string]any); !ok {
		t.Fatalf("usage.info not an object: %#v", usage["info"])
	}
}

func TestSessionsOmitsUsageWhenSnapshotNilOrHubAbsent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cases := map[string]*server{
		"no hub":       {token: "secret", host: "devbox"},
		"nil snapshot": {token: "secret", host: "devbox", usageSnapshot: func() *AccountUsage { return nil }},
	}
	for name, s := range cases {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/sessions", nil)
			req.Header.Set("Authorization", "Bearer secret")
			rec := httptest.NewRecorder()
			s.sessions(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
			}
			var raw map[string]any
			if err := json.NewDecoder(rec.Body).Decode(&raw); err != nil {
				t.Fatal(err)
			}
			if _, present := raw["usage"]; present {
				t.Fatalf("usage key present when it should be omitted: %s", rec.Body.String())
			}
		})
	}
}

func TestSessionsIncludesCodexUsageWhenSnapshotPresent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := &server{
		token: "secret",
		host:  "devbox",
		codexUsageSnapshot: func() *CodexAccountUsage {
			return &CodexAccountUsage{
				Account: "bot@ci.com",
				Info:    &CodexUsageInfo{Plan: "pro", Windows: []codexWindow{{Label: "wk", Pct: 88}}},
			}
		},
	}
	req := httptest.NewRequest(http.MethodGet, "/sessions", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.sessions(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
	}
	var raw map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	codex, ok := raw["codex_usage"].(map[string]any)
	if !ok {
		t.Fatalf("codex_usage not an object: %#v", raw["codex_usage"])
	}
	if codex["account"] != "bot@ci.com" {
		t.Fatalf("codex_usage.account = %#v, want bot@ci.com", codex["account"])
	}
	if _, ok := codex["info"].(map[string]any); !ok {
		t.Fatalf("codex_usage.info not an object: %#v", codex["info"])
	}
}

func TestSessionsOmitsCodexUsageWhenSnapshotNilOrHubAbsent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cases := map[string]*server{
		"no hub":       {token: "secret", host: "devbox"},
		"nil snapshot": {token: "secret", host: "devbox", codexUsageSnapshot: func() *CodexAccountUsage { return nil }},
	}
	for name, s := range cases {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/sessions", nil)
			req.Header.Set("Authorization", "Bearer secret")
			rec := httptest.NewRecorder()
			s.sessions(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
			}
			var raw map[string]any
			if err := json.NewDecoder(rec.Body).Decode(&raw); err != nil {
				t.Fatal(err)
			}
			if _, present := raw["codex_usage"]; present {
				t.Fatalf("codex_usage key present when it should be omitted: %s", rec.Body.String())
			}
		})
	}
}

func getServerSessions(s *server) (int, []Session, error) {
	req := httptest.NewRequest(http.MethodGet, "/sessions", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.sessions(rec, req)
	if rec.Code != http.StatusOK {
		return rec.Code, nil, nil
	}
	var response struct {
		Sessions []Session `json:"sessions"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		return rec.Code, nil, err
	}
	return rec.Code, response.Sessions, nil
}

func TestSessionsCachesSuccessfulCollectionForOneSecond(t *testing.T) {
	now := time.Date(2026, time.July, 22, 12, 0, 0, 0, time.UTC)
	collectCalls := 0
	hostSnapshots := 0
	s := &server{
		token: "secret",
		collect: func() ([]Session, error) {
			collectCalls++
			if collectCalls == 1 {
				now = now.Add(200 * time.Millisecond)
			}
			return []Session{{PID: collectCalls}}, nil
		},
		hostSnapshot: func() HostUsage {
			hostSnapshots++
			return HostUsage{}
		},
	}
	s.sessionCache.now = func() time.Time { return now }

	code, sessions, err := getServerSessions(s)
	if err != nil {
		t.Fatal(err)
	}
	if code != http.StatusOK || len(sessions) != 1 || sessions[0].PID != 1 {
		t.Fatalf("first response = (%d, %#v), want PID 1", code, sessions)
	}

	now = now.Add(999 * time.Millisecond)
	code, sessions, err = getServerSessions(s)
	if err != nil {
		t.Fatal(err)
	}
	if code != http.StatusOK || len(sessions) != 1 || sessions[0].PID != 1 {
		t.Fatalf("response before TTL = (%d, %#v), want PID 1", code, sessions)
	}
	if collectCalls != 1 {
		t.Fatalf("collect calls before TTL = %d, want 1", collectCalls)
	}

	now = now.Add(time.Millisecond)
	code, sessions, err = getServerSessions(s)
	if err != nil {
		t.Fatal(err)
	}
	if code != http.StatusOK || len(sessions) != 1 || sessions[0].PID != 2 {
		t.Fatalf("response at TTL = (%d, %#v), want refreshed PID 2", code, sessions)
	}
	if collectCalls != 2 {
		t.Fatalf("collect calls at TTL = %d, want 2", collectCalls)
	}
	if hostSnapshots != 3 {
		t.Fatalf("host snapshots = %d, want one per request", hostSnapshots)
	}
}

func TestSessionsDoesNotCacheCollectionErrors(t *testing.T) {
	collectCalls := 0
	s := &server{
		token: "secret",
		collect: func() ([]Session, error) {
			collectCalls++
			if collectCalls == 1 {
				return nil, errors.New("collect failed")
			}
			return []Session{{PID: 2}}, nil
		},
	}

	code, _, err := getServerSessions(s)
	if err != nil {
		t.Fatal(err)
	}
	if code != http.StatusInternalServerError {
		t.Fatalf("first status = %d, want %d", code, http.StatusInternalServerError)
	}

	code, sessions, err := getServerSessions(s)
	if err != nil {
		t.Fatal(err)
	}
	if code != http.StatusOK || len(sessions) != 1 || sessions[0].PID != 2 {
		t.Fatalf("second response = (%d, %#v), want successful retry", code, sessions)
	}
	if collectCalls != 2 {
		t.Fatalf("collect calls = %d, want 2", collectCalls)
	}
}

func TestSessionsSharesConcurrentCollectionError(t *testing.T) {
	flightStarted := make(chan struct{})
	releaseFlight := make(chan struct{})
	secondRequestStarted := make(chan struct{})
	flightErr := errors.New("collect failed")
	var collectMu sync.Mutex
	collectCalls := 0
	s := &server{
		token: "secret",
		collect: func() ([]Session, error) {
			collectMu.Lock()
			collectCalls++
			call := collectCalls
			collectMu.Unlock()
			if call == 1 {
				close(flightStarted)
				<-releaseFlight
				return nil, flightErr
			}
			return []Session{{PID: 2}}, nil
		},
	}

	type result struct {
		code     int
		sessions []Session
		err      error
	}
	firstResult := make(chan result, 1)
	secondResult := make(chan result, 1)
	go func() {
		code, sessions, err := getServerSessions(s)
		firstResult <- result{code: code, sessions: sessions, err: err}
	}()
	<-flightStarted
	go func() {
		close(secondRequestStarted)
		code, sessions, err := getServerSessions(s)
		secondResult <- result{code: code, sessions: sessions, err: err}
	}()
	<-secondRequestStarted
	time.Sleep(100 * time.Millisecond)
	close(releaseFlight)

	for _, result := range []result{<-firstResult, <-secondResult} {
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.code != http.StatusInternalServerError {
			t.Fatalf("concurrent status = %d, want %d", result.code, http.StatusInternalServerError)
		}
	}
	collectMu.Lock()
	if collectCalls != 1 {
		collectMu.Unlock()
		t.Fatalf("collect calls for shared failed flight = %d, want 1", collectCalls)
	}
	collectMu.Unlock()

	code, sessions, err := getServerSessions(s)
	if err != nil {
		t.Fatal(err)
	}
	if code != http.StatusOK || len(sessions) != 1 || sessions[0].PID != 2 {
		t.Fatalf("later response = (%d, %#v), want retry success", code, sessions)
	}
	collectMu.Lock()
	defer collectMu.Unlock()
	if collectCalls != 2 {
		t.Fatalf("collect calls after later retry = %d, want 2", collectCalls)
	}
}

func TestSessionsSharesConcurrentColdCollection(t *testing.T) {
	const requests = 16

	collectionStarted := make(chan struct{})
	secondCollectionStarted := make(chan struct{})
	releaseCollection := make(chan struct{})
	var mu sync.Mutex
	collectCalls := 0
	s := &server{
		token: "secret",
		collect: func() ([]Session, error) {
			mu.Lock()
			collectCalls++
			call := collectCalls
			mu.Unlock()
			if call == 1 {
				close(collectionStarted)
			} else if call == 2 {
				close(secondCollectionStarted)
			}
			<-releaseCollection
			return []Session{{PID: 42}}, nil
		},
	}

	type result struct {
		code     int
		sessions []Session
		err      error
	}
	results := make(chan result, requests)
	start := make(chan struct{})
	var ready, workers sync.WaitGroup
	ready.Add(requests)
	workers.Add(requests)
	for range requests {
		go func() {
			defer workers.Done()
			ready.Done()
			<-start
			code, sessions, err := getServerSessions(s)
			results <- result{code: code, sessions: sessions, err: err}
		}()
	}
	ready.Wait()
	close(start)
	<-collectionStarted

	select {
	case <-secondCollectionStarted:
		close(releaseCollection)
		t.Fatal("second cold request started its own collection")
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseCollection)
	workers.Wait()
	close(results)

	for result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.code != http.StatusOK || len(result.sessions) != 1 || result.sessions[0].PID != 42 {
			t.Fatalf("response = (%d, %#v), want cached session", result.code, result.sessions)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if collectCalls != 1 {
		t.Fatalf("collect calls = %d, want 1", collectCalls)
	}
}

func TestKillInvalidatesCachedSessionsOnlyAfterSuccess(t *testing.T) {
	for _, test := range []struct {
		name          string
		terminateErr  error
		wantCalls     int
		wantListingID int
	}{
		{name: "success", wantCalls: 3, wantListingID: 2},
		{name: "failure", terminateErr: errors.New("kill failed"), wantCalls: 2, wantListingID: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			collectCalls := 0
			s := &server{
				token: "secret",
				collect: func() ([]Session, error) {
					collectCalls++
					switch collectCalls {
					case 1, 2:
						return []Session{{PID: 1}}, nil
					default:
						return []Session{{PID: 2}}, nil
					}
				},
				terminate: func(Session) error { return test.terminateErr },
			}

			code, _, err := getServerSessions(s)
			if err != nil || code != http.StatusOK {
				t.Fatalf("initial listing = (%d, %v)", code, err)
			}
			killRequest := httptest.NewRequest(http.MethodPost, "/sessions/1/kill", nil)
			killRequest.SetPathValue("pid", "1")
			killRequest.Header.Set("Authorization", "Bearer secret")
			killRecorder := httptest.NewRecorder()
			s.kill(killRecorder, killRequest)

			code, sessions, err := getServerSessions(s)
			if err != nil {
				t.Fatal(err)
			}
			if code != http.StatusOK || len(sessions) != 1 || sessions[0].PID != test.wantListingID {
				t.Fatalf("listing after kill = (%d, %#v), want PID %d", code, sessions, test.wantListingID)
			}
			if collectCalls != test.wantCalls {
				t.Fatalf("collect calls = %d, want %d", collectCalls, test.wantCalls)
			}
		})
	}
}

func TestSessionsRetriesAfterInvalidationDuringCollection(t *testing.T) {
	firstCollectionStarted := make(chan struct{})
	releaseFirstCollection := make(chan struct{})
	var mu sync.Mutex
	collectCalls := 0
	s := &server{
		token: "secret",
		collect: func() ([]Session, error) {
			mu.Lock()
			collectCalls++
			call := collectCalls
			mu.Unlock()
			switch call {
			case 1:
				close(firstCollectionStarted)
				<-releaseFirstCollection
				return []Session{{PID: 1}}, nil
			case 2:
				return []Session{{PID: 1}}, nil // fresh row used by kill
			case 3:
				return []Session{{PID: 2}}, nil // fresh listing after invalidation
			default:
				return nil, fmt.Errorf("unexpected collect call %d", call)
			}
		},
		terminate: func(Session) error { return nil },
	}

	type result struct {
		code     int
		sessions []Session
		err      error
	}
	listing := make(chan result, 1)
	go func() {
		code, sessions, err := getServerSessions(s)
		listing <- result{code: code, sessions: sessions, err: err}
	}()
	<-firstCollectionStarted

	killRequest := httptest.NewRequest(http.MethodPost, "/sessions/1/kill", nil)
	killRequest.SetPathValue("pid", "1")
	killRequest.Header.Set("Authorization", "Bearer secret")
	killRecorder := httptest.NewRecorder()
	s.kill(killRecorder, killRequest)
	if killRecorder.Code != http.StatusOK {
		t.Fatalf("kill status = %d", killRecorder.Code)
	}

	close(releaseFirstCollection)
	got := <-listing
	if got.err != nil {
		t.Fatal(got.err)
	}
	if got.code != http.StatusOK || len(got.sessions) != 1 || got.sessions[0].PID != 2 {
		t.Fatalf("listing = (%d, %#v), want fresh PID 2", got.code, got.sessions)
	}
	mu.Lock()
	defer mu.Unlock()
	if collectCalls != 3 {
		t.Fatalf("collect calls = %d, want 3", collectCalls)
	}
}

func TestKillHandlerReportsEmptiedWorktree(t *testing.T) {
	repo := t.TempDir()
	root := makeWorktree(t, repo, "DR-860")
	target := Session{PID: 55, CWD: root}
	s := &server{
		token:     "secret",
		collect:   func() ([]Session, error) { return []Session{target}, nil },
		terminate: func(Session) error { return nil },
	}
	req := httptest.NewRequest("POST", "/sessions/55/kill", nil)
	req.SetPathValue("pid", "55")
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()

	s.kill(rec, req)

	var r actionResult
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !r.OK {
		t.Fatalf("response not OK: %s", r.Error)
	}
	if r.Worktree == nil {
		t.Fatal("worktree missing from kill response")
	}
	if r.Worktree.Path != root || r.Worktree.Name != "DR-860" {
		t.Fatalf("worktree = %+v, want path %q name DR-860", *r.Worktree, root)
	}
}

func TestKillHandlerOmitsOccupiedWorktree(t *testing.T) {
	repo := t.TempDir()
	root := makeWorktree(t, repo, "DR-860")
	sessions := []Session{{PID: 55, CWD: root}, {PID: 56, CWD: filepath.Join(root, "app")}}
	s := &server{
		token:     "secret",
		collect:   func() ([]Session, error) { return sessions, nil },
		terminate: func(Session) error { return nil },
	}
	req := httptest.NewRequest("POST", "/sessions/55/kill", nil)
	req.SetPathValue("pid", "55")
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()

	s.kill(rec, req)

	var r actionResult
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if r.Worktree != nil {
		t.Fatalf("worktree = %+v, want none while another session runs in it", *r.Worktree)
	}
}

func TestRemoveWorktreeHandlerRequiresAuth(t *testing.T) {
	removed := false
	s := &server{token: "secret", removeTree: func(string) error { removed = true; return nil }}
	req := httptest.NewRequest("POST", "/worktree/remove", strings.NewReader(`{"path":"/repo/.claude/worktrees/x"}`))
	rec := httptest.NewRecorder()

	s.removeWorktree(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if removed {
		t.Fatal("removeTree called without auth")
	}
}

func TestRemoveWorktreeHandlerRejectsBadPaths(t *testing.T) {
	repo := t.TempDir()
	root := makeWorktree(t, repo, "DR-860")
	for _, path := range []string{
		"",
		"relative/.claude/worktrees/DR-860",
		root + "/../../../../etc",
		filepath.Join(root, "sub"),
		repo,
		filepath.Join(repo, ".claude", "worktrees", "ghost"),
	} {
		removed := false
		s := &server{
			token:      "secret",
			collect:    func() ([]Session, error) { return nil, nil },
			removeTree: func(string) error { removed = true; return nil },
		}
		body, _ := json.Marshal(map[string]string{"path": path})
		req := httptest.NewRequest("POST", "/worktree/remove", strings.NewReader(string(body)))
		req.Header.Set("Authorization", "Bearer secret")
		rec := httptest.NewRecorder()

		s.removeWorktree(rec, req)

		var r actionResult
		if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
			t.Fatalf("%q: decode response: %v", path, err)
		}
		if r.OK || removed {
			t.Fatalf("%q accepted, want rejection", path)
		}
	}
}

func TestRemoveWorktreeHandlerRefusesWhileInUse(t *testing.T) {
	repo := t.TempDir()
	root := makeWorktree(t, repo, "DR-860")
	removed := false
	s := &server{
		token:      "secret",
		collect:    func() ([]Session, error) { return []Session{{PID: 77, CWD: filepath.Join(root, "app")}}, nil },
		removeTree: func(string) error { removed = true; return nil },
	}
	body, _ := json.Marshal(map[string]string{"path": root})
	req := httptest.NewRequest("POST", "/worktree/remove", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()

	s.removeWorktree(rec, req)

	var r actionResult
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if r.OK || removed {
		t.Fatal("removed a worktree that still has a live session")
	}
	if !strings.Contains(r.Error, "77") {
		t.Fatalf("error = %q, want the occupying PID", r.Error)
	}
}

func TestRemoveWorktreeHandlerRemoves(t *testing.T) {
	repo := t.TempDir()
	root := makeWorktree(t, repo, "DR-860")
	got := ""
	s := &server{
		token:      "secret",
		collect:    func() ([]Session, error) { return nil, nil },
		removeTree: func(path string) error { got = path; return nil },
	}
	body, _ := json.Marshal(map[string]string{"path": root})
	req := httptest.NewRequest("POST", "/worktree/remove", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()

	s.removeWorktree(rec, req)

	var r actionResult
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !r.OK {
		t.Fatalf("response not OK: %s", r.Error)
	}
	if got != root {
		t.Fatalf("removed %q, want %q", got, root)
	}
}

// TestSessionsResponseMatchesGolden pins the wire shape the iOS client decodes.
//
// Session is both the on-disk model and the HTTP DTO, so a field rename here is
// a silent client break with no compile error on either side. The same fixture
// is decoded by a test in the Swift package, so the two fail together. If this
// test fails because the shape legitimately changed, update the fixture AND the
// Swift test.
func TestSessionsResponseMatchesGolden(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "sessions-golden.json"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	var resp struct {
		Hostname  string    `json:"hostname"`
		TS        int64     `json:"ts"`
		HostUsage HostUsage `json:"hostUsage"`
		Sessions  []Session `json:"sessions"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("golden does not decode against the current shape: %v", err)
	}
	if len(resp.Sessions) != 4 {
		t.Fatalf("len(sessions) = %d, want the four documented shapes", len(resp.Sessions))
	}

	// Fully enriched.
	full := resp.Sessions[0]
	if full.Model != "claude-opus-5" || full.ContextTokens != 84213 {
		t.Fatalf("enriched session lost model/context: %+v", full)
	}
	if full.CostUSD != 3.4712 || full.CostSubagentsUSD != 1.208 || full.AgentsRunning != 2 {
		t.Fatalf("enriched session lost cost/agents: %+v", full)
	}
	if full.TmuxAttached == nil || *full.TmuxAttached != 1 {
		t.Fatalf("tmuxAttached = %v, want a pointer to 1", full.TmuxAttached)
	}
	if !full.Waiting() {
		t.Fatalf("session with waitingFor set must report Waiting()")
	}

	// Minimal: every omitempty field absent.
	minimal := resp.Sessions[1]
	if minimal.Model != "" || minimal.CostUSD != 0 || minimal.TmuxAttached != nil {
		t.Fatalf("minimal session gained values from nowhere: %+v", minimal)
	}
	if minimal.Waiting() {
		t.Fatalf("session with empty waitingFor must not report Waiting()")
	}

	// A newer server's unknown key must not break decoding.
	future := resp.Sessions[3]
	if future.Name != "from a newer server" {
		t.Fatalf("unknown key broke decoding: %+v", future)
	}

	// Partial host usage: cpu present, load absent, rendered as dashes client-side.
	if resp.HostUsage.CPUPercent == nil || *resp.HostUsage.CPUPercent != 23.4 {
		t.Fatalf("cpuPercent = %v, want 23.4", resp.HostUsage.CPUPercent)
	}
	if resp.HostUsage.Load != nil {
		t.Fatalf("loadAverage = %v, want absent", resp.HostUsage.Load)
	}
	if resp.HostUsage.MemoryPercent != nil {
		t.Fatalf("memoryPercent = %v, want absent", resp.HostUsage.MemoryPercent)
	}

	// The always-present keys are the client's hard contract: no omitempty, so
	// they appear even at zero value. Losing one silently changes the wire shape.
	encoded, err := json.Marshal(minimal)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	var keys map[string]any
	if err := json.Unmarshal(encoded, &keys); err != nil {
		t.Fatalf("decode re-encoded: %v", err)
	}
	for _, k := range []string{
		"pid", "sessionId", "cwd", "status", "waitingFor", "version",
		"entrypoint", "name", "nameSource", "startedAt", "updatedAt", "cpu", "tmux",
	} {
		if _, ok := keys[k]; !ok {
			t.Fatalf("key %q vanished from the wire shape", k)
		}
	}
}
