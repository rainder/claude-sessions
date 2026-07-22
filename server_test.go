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
	"strconv"
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
		Suggestions []cwdSuggestion `json:"suggestions"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Suggestions) != 1 || got.Suggestions[0].CWD != cwd {
		t.Fatalf("suggestions = %#v", got.Suggestions)
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

func putDisabled(s *server, pid int, body string, authed bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPut, fmt.Sprintf("/sessions/%d/disabled", pid), strings.NewReader(body))
	req.SetPathValue("pid", strconv.Itoa(pid))
	if authed {
		req.Header.Set("Authorization", "Bearer secret")
	}
	rec := httptest.NewRecorder()
	s.setDisabled(rec, req)
	return rec
}

func TestSetDisabledSetsClearsAndAnnotatesSessions(t *testing.T) {
	s := &server{
		token: "secret",
		collect: func() ([]Session, error) {
			return []Session{{PID: 42, SessionID: "session-42"}}, nil
		},
	}

	rec := putDisabled(s, 42, `{"disabled":true,"sessionId":"session-42"}`, true)
	if rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), `"disabled":true`) ||
		!strings.Contains(rec.Body.String(), `"sessionId":"session-42"`) {
		t.Fatalf("set response = %d %s", rec.Code, rec.Body.String())
	}
	code, rows, err := getServerSessions(s)
	if err != nil || code != http.StatusOK || len(rows) != 1 || !rows[0].Disabled {
		t.Fatalf("annotated rows = (%d, %#v, %v)", code, rows, err)
	}

	rec = putDisabled(s, 42, `{"disabled":false,"sessionId":"session-42"}`, true)
	if rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), `"disabled":false`) ||
		!strings.Contains(rec.Body.String(), `"sessionId":"session-42"`) {
		t.Fatalf("clear response = %d %s", rec.Code, rec.Body.String())
	}
	_, rows, err = getServerSessions(s)
	if err != nil || len(rows) != 1 || rows[0].Disabled {
		t.Fatalf("cleared rows = (%#v, %v)", rows, err)
	}
}

func TestSetDisabledValidatesRequest(t *testing.T) {
	live := &server{
		token: "secret",
		collect: func() ([]Session, error) {
			return []Session{{PID: 42, SessionID: "session-42"}}, nil
		},
	}
	cases := []struct {
		name string
		pid  int
		body string
		want int
	}{
		{"malformed", 42, `{`, http.StatusBadRequest},
		{"trailing JSON", 42, `{"disabled":true,"sessionId":"session-42"} {}`, http.StatusBadRequest},
		{"missing state", 42, `{"sessionId":"session-42"}`, http.StatusBadRequest},
		{"missing identity", 42, `{"disabled":true}`, http.StatusBadRequest},
		{"empty identity", 42, `{"disabled":true,"sessionId":""}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := putDisabled(live, tc.pid, tc.body, true)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d; body=%q", rec.Code, tc.want, rec.Body.String())
			}
		})
	}

	req := httptest.NewRequest(
		http.MethodPut,
		"/sessions/not-a-pid/disabled",
		strings.NewReader(`{"disabled":true,"sessionId":"session-42"}`),
	)
	req.SetPathValue("pid", "not-a-pid")
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	live.setDisabled(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed PID status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func validationStateServer(collect func() ([]Session, error)) *server {
	return &server{
		token:   "secret",
		collect: collect,
		disabledSessionIDs: map[string]struct{}{
			"retained-session": {},
		},
		disabledGeneration: 7,
		sessionCache: sessionCache{
			sessions: []Session{{
				PID:       10,
				SessionID: "cached-session",
			}},
			completedAt:      time.Now(),
			valid:            true,
			cachedGeneration: 11,
			generation:       11,
		},
	}
}

func assertValidationStateUnchanged(t *testing.T, s *server) {
	t.Helper()

	s.disabledMu.RLock()
	_, retained := s.disabledSessionIDs["retained-session"]
	registryLen := len(s.disabledSessionIDs)
	disabledGeneration := s.disabledGeneration
	s.disabledMu.RUnlock()
	if !retained || registryLen != 1 || disabledGeneration != 7 {
		t.Fatalf(
			"validation mutated registry: registry=%#v generation=%d",
			s.disabledSessionIDs,
			disabledGeneration,
		)
	}

	s.sessionCache.mu.Lock()
	cacheValid := s.sessionCache.valid
	cacheGeneration := s.sessionCache.generation
	cachedGeneration := s.sessionCache.cachedGeneration
	cachedRows := len(s.sessionCache.sessions)
	completedAt := s.sessionCache.completedAt
	s.sessionCache.mu.Unlock()
	if !cacheValid ||
		cacheGeneration != 11 ||
		cachedGeneration != 11 ||
		cachedRows != 1 ||
		completedAt.IsZero() {
		t.Fatalf(
			"validation invalidated cache: valid=%v generation=%d cachedGeneration=%d rows=%d completedAt=%v",
			cacheValid,
			cacheGeneration,
			cachedGeneration,
			cachedRows,
			completedAt,
		)
	}
}

func TestSetDisabledUnknownPIDPreservesRegistryAndCache(t *testing.T) {
	s := validationStateServer(func() ([]Session, error) {
		return []Session{{PID: 42, SessionID: "session-42"}}, nil
	})

	rec := putDisabled(
		s,
		99,
		`{"disabled":true,"sessionId":"session-99"}`,
		true,
	)
	if rec.Code != http.StatusNotFound {
		t.Fatalf(
			"status = %d, want %d; body=%q",
			rec.Code,
			http.StatusNotFound,
			rec.Body.String(),
		)
	}
	assertValidationStateUnchanged(t, s)
}

func TestSetDisabledRejectsSessionWithoutStableID(t *testing.T) {
	s := validationStateServer(func() ([]Session, error) {
		return []Session{{PID: 42}}, nil
	})
	rec := putDisabled(
		s,
		42,
		`{"disabled":true,"sessionId":"selected-session"}`,
		true,
	)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d; body=%q", rec.Code, http.StatusConflict, rec.Body.String())
	}
	assertValidationStateUnchanged(t, s)
}

func TestSetDisabledRejectsReusedPIDWhenExpectedSessionChanged(t *testing.T) {
	s := validationStateServer(func() ([]Session, error) {
		return []Session{{PID: 42, SessionID: "new-session"}}, nil
	})

	rec := putDisabled(
		s,
		42,
		`{"disabled":true,"sessionId":"old-session"}`,
		true,
	)
	if rec.Code != http.StatusConflict {
		t.Fatalf(
			"status = %d, want %d; body=%q",
			rec.Code,
			http.StatusConflict,
			rec.Body.String(),
		)
	}
	assertValidationStateUnchanged(t, s)
}

func TestSetDisabledUnauthorizedDoesNotCollectOrMutate(t *testing.T) {
	collectCalls := 0
	s := &server{
		token: "secret",
		collect: func() ([]Session, error) {
			collectCalls++
			return []Session{{PID: 42, SessionID: "session-42"}}, nil
		},
		disabledSessionIDs: map[string]struct{}{"keep": {}},
	}
	rec := putDisabled(
		s,
		42,
		`{"disabled":true,"sessionId":"session-42"}`,
		false,
	)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	if collectCalls != 0 {
		t.Fatalf("unauthorized request collected sessions %d times", collectCalls)
	}
	s.disabledMu.RLock()
	defer s.disabledMu.RUnlock()
	if len(s.disabledSessionIDs) != 1 {
		t.Fatalf("disabled state mutated: %#v", s.disabledSessionIDs)
	}
	if _, ok := s.disabledSessionIDs["keep"]; !ok {
		t.Fatalf("existing disabled state was removed: %#v", s.disabledSessionIDs)
	}
}

func TestDisabledStateFollowsSessionIDNotReusedPID(t *testing.T) {
	current := Session{PID: 42, SessionID: "old-session"}
	s := &server{
		token:   "secret",
		collect: func() ([]Session, error) { return []Session{current}, nil },
	}
	if rec := putDisabled(
		s,
		42,
		`{"disabled":true,"sessionId":"old-session"}`,
		true,
	); rec.Code != http.StatusOK {
		t.Fatalf("set status = %d", rec.Code)
	}

	current = Session{PID: 42, SessionID: "new-session"}
	_, rows, err := getServerSessions(s)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %#v, err=%v", rows, err)
	}
	if rows[0].Disabled {
		t.Fatal("reused PID inherited old session disabled state")
	}
	s.disabledMu.RLock()
	_, stale := s.disabledSessionIDs["old-session"]
	s.disabledMu.RUnlock()
	if stale {
		t.Fatal("ended session ID was not pruned")
	}
}

func TestCollectionErrorDoesNotPruneDisabledState(t *testing.T) {
	s := &server{
		token:              "secret",
		collect:            func() ([]Session, error) { return nil, errors.New("collect failed") },
		disabledSessionIDs: map[string]struct{}{"keep-me": {}},
	}
	code, _, err := getServerSessions(s)
	if err != nil || code != http.StatusInternalServerError {
		t.Fatalf("response = (%d, %v)", code, err)
	}
	s.disabledMu.RLock()
	_, kept := s.disabledSessionIDs["keep-me"]
	s.disabledMu.RUnlock()
	if !kept {
		t.Fatal("collection error pruned disabled state")
	}
}

func TestCollectionStartedBeforeWriteDoesNotPruneNewerState(t *testing.T) {
	s := &server{}
	s.collect = func() ([]Session, error) {
		s.writeDisabled("newer-session", true)
		return nil, nil
	}
	if _, err := s.collectLocal(); err != nil {
		t.Fatal(err)
	}
	s.disabledMu.RLock()
	_, kept := s.disabledSessionIDs["newer-session"]
	s.disabledMu.RUnlock()
	if !kept {
		t.Fatal("collection pruned state written after collection started")
	}
}

func TestSetDisabledInvalidatesSessionCache(t *testing.T) {
	collectCalls := 0
	s := &server{
		token: "secret",
		collect: func() ([]Session, error) {
			collectCalls++
			return []Session{{PID: 42, SessionID: "session-42"}}, nil
		},
	}
	if code, _, err := getServerSessions(s); err != nil || code != http.StatusOK {
		t.Fatalf("initial listing = (%d, %v)", code, err)
	}
	if rec := putDisabled(
		s,
		42,
		`{"disabled":true,"sessionId":"session-42"}`,
		true,
	); rec.Code != http.StatusOK {
		t.Fatalf("put status = %d", rec.Code)
	}
	_, rows, err := getServerSessions(s)
	if err != nil || len(rows) != 1 || !rows[0].Disabled {
		t.Fatalf("listing after put = (%#v, %v)", rows, err)
	}
	if collectCalls != 3 {
		t.Fatalf("collect calls = %d, want 3 (initial, PUT resolve, invalidated GET)", collectCalls)
	}
}

func TestDisabledStateConcurrentReadsAndWrites(t *testing.T) {
	s := &server{
		token: "secret",
		collect: func() ([]Session, error) {
			return []Session{{PID: 42, SessionID: "session-42"}}, nil
		},
	}
	var workers sync.WaitGroup
	for i := 0; i < 32; i++ {
		workers.Add(2)
		go func(disabled bool) {
			defer workers.Done()
			putDisabled(
				s,
				42,
				fmt.Sprintf(
					`{"disabled":%t,"sessionId":"session-42"}`,
					disabled,
				),
				true,
			)
		}(i%2 == 0)
		go func() {
			defer workers.Done()
			_, _, _ = getServerSessions(s)
		}()
	}
	workers.Wait()
}
