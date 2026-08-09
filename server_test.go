package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
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

func TestPresetsReturnsNamesAndCommands(t *testing.T) {
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
		Presets []CommandPreset `json:"presets"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	want := []CommandPreset{{Name: "Claude", Command: "claude"}, {Name: "Fable", Command: "claude --model fable"}}
	if len(got.Presets) != len(want) || got.Presets[0] != want[0] || got.Presets[1] != want[1] {
		t.Fatalf("presets = %#v, want %#v", got.Presets, want)
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
		_, _ = w.Write([]byte(`{"presets":[{"name":"Claude","command":"claude"},{"name":"Fable","command":"claude --model fable"}]}`))
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
	want := []CommandPreset{{Name: "Claude", Command: "claude"}, {Name: "Fable", Command: "claude --model fable"}}
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

// TestSessionsHandlerOverlaysDisabledState proves GET /sessions no longer
// hardcodes Disabled=false: it reflects the host's own FlagsStore, and
// does not mutate the shared session cache in the process.
func TestSessionsHandlerOverlaysDisabledState(t *testing.T) {
	dir := t.TempDir()
	flags := newFlagsStore(filepath.Join(dir, flagsFileName), fixedClock(time.Now()), noResolver)
	flags.SetDisabled("dis-1", true)

	s := &server{
		token: "secret",
		flags: flags,
		collect: func() ([]Session, error) {
			return []Session{
				{PID: 1, SessionID: "dis-1"},
				{PID: 2, SessionID: "dis-2"},
			}, nil
		},
	}
	req := httptest.NewRequest("GET", "/sessions", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()

	s.sessions(rec, req)

	var resp struct {
		Sessions []Session `json:"sessions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Sessions) != 2 {
		t.Fatalf("got %d sessions, want 2", len(resp.Sessions))
	}
	if !resp.Sessions[0].Disabled {
		t.Fatal("dis-1 not reported disabled")
	}
	if resp.Sessions[1].Disabled {
		t.Fatal("dis-2 reported disabled with no entry")
	}

	// A second call must see the same result — proves the first call didn't
	// mutate the cached slice in place.
	rec2 := httptest.NewRecorder()
	s.sessions(rec2, req)
	var resp2 struct {
		Sessions []Session `json:"sessions"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &resp2); err != nil {
		t.Fatalf("decode second response: %v", err)
	}
	if !resp2.Sessions[0].Disabled || resp2.Sessions[1].Disabled {
		t.Fatalf("second call diverged: %#v", resp2.Sessions)
	}
}

func TestSessionsReportsThisHostsIdentity(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	want := LoadHostID()
	s := &server{token: "secret", host: "devbox"}
	req := httptest.NewRequest(http.MethodGet, "/sessions", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.sessions(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
	}
	var got struct {
		HostID string `json:"host_id"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.HostID) != 32 {
		t.Fatalf("host_id = %q, want 32 hex chars", got.HostID)
	}
	if _, err := hex.DecodeString(got.HostID); err != nil {
		t.Fatalf("host_id = %q, not hex: %v", got.HostID, err)
	}
	if got.HostID != want {
		t.Fatalf("host_id = %q, want %q", got.HostID, want)
	}
}

func TestSessionsReportsANewIdentityWithoutRestarting(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := &server{token: "secret", host: "devbox"}

	fetchHostID := func() string {
		req := httptest.NewRequest(http.MethodGet, "/sessions", nil)
		req.Header.Set("Authorization", "Bearer secret")
		rec := httptest.NewRecorder()
		s.sessions(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
		}
		var got struct {
			HostID string `json:"host_id"`
		}
		if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		return got.HostID
	}

	first := fetchHostID()
	hostIDPath := filepath.Join(ConfigDir(), "host-id")
	original, err := os.ReadFile(hostIDPath)
	if err != nil {
		t.Fatalf("reading host-id file: %v", err)
	}
	t.Cleanup(func() {
		_ = os.WriteFile(hostIDPath, original, 0o644)
	})
	if err := os.Remove(hostIDPath); err != nil {
		t.Fatalf("removing host-id file: %v", err)
	}

	second := fetchHostID()
	if second == first {
		t.Fatalf("host_id unchanged after identity file removed: %q", second)
	}
}

// TestSessionsAdvertisesAPIHandshake pins the capability handshake a client uses
// to tell an old host from a misconfigured one. The list is asserted literally,
// not against capabilities(), because comparing the payload to its own source
// would pass whatever the source said. Every name here is a promise a client
// gates a control on, so a rename or a reorder is a wire break: change this test
// only together with the clients that read it.
func TestSessionsAdvertisesAPIHandshake(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := &server{token: "secret", host: "devbox"}
	req := httptest.NewRequest(http.MethodGet, "/sessions", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.sessions(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
	}

	// A pointer: an absent "api" object decodes to nil, which is what an old
	// server looks like on the wire and must not be confused with an empty one.
	var got struct {
		API *struct {
			Schema       int      `json:"schema"`
			Capabilities []string `json:"capabilities"`
		} `json:"api"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.API == nil {
		t.Fatalf("no api object in the payload — a client cannot tell this host from an un-upgraded one")
	}
	if got.API.Schema != 2 {
		t.Fatalf("api.schema = %d, want 2", got.API.Schema)
	}
	// Only routes that answer today. A name is added here by the change that
	// lands its endpoint, so this literal failing is the intended alarm when a
	// capability is advertised ahead of the handler that serves it.
	want := []string{"kill", "migrate", "spawn", "resume", "worktree-remove", "preview-range", "attach", "flags", "test-push"}
	if !slices.Equal(got.API.Capabilities, want) {
		t.Fatalf("api.capabilities = %v, want %v", got.API.Capabilities, want)
	}
}

// TestCapabilitiesIsNotSharedState guards the one place the list lives: it is a
// wire contract, and a caller that trims or appends to the returned slice must
// not be able to change what the next response advertises.
func TestCapabilitiesIsNotSharedState(t *testing.T) {
	first := capabilities()
	if len(first) == 0 {
		t.Fatal("capabilities() is empty")
	}
	first[0] = "clobbered"
	if second := capabilities(); second[0] == "clobbered" {
		t.Fatalf("capabilities() hands out shared state: %v", second)
	}
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

// TestSessionsOmitsAnthropicAccountFields pins the transport split: the
// Anthropic account keys moved to GET /usage, and /sessions must not carry them
// even though a client still reads the same three RemoteResult fields.
func TestSessionsOmitsAnthropicAccountFields(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := &server{token: "secret", host: "devbox"}
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
	for _, key := range []string{"usage", "knownAccounts", "activeSnapshotName"} {
		if _, present := raw[key]; present {
			t.Fatalf("%q still in the /sessions body: %s", key, rec.Body.String())
		}
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

// newUsageServer builds a bare server for /usage tests. Unlike before, the
// handler makes no Anthropic call at all, so there is no fetch to stub out.
func newUsageServer() *server {
	return &server{token: "secret", host: "devbox"}
}

// getUsage issues one authorized GET /usage and decodes the body.
func getUsage(t *testing.T, s *server) usageResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/usage", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.usage(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
	}
	var out usageResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return out
}

func findKnownAccount(t *testing.T, resp usageResponse, name string) KnownAccountUsage {
	t.Helper()
	for _, k := range resp.KnownAccounts {
		if k.Name == name {
			return k
		}
	}
	t.Fatalf("account %q missing from knownAccounts %#v", name, resp.KnownAccounts)
	return KnownAccountUsage{}
}

// GET /usage never calls Anthropic — it reports which accounts this host
// knows about, never their numbers. A remote caller repeatedly hitting this
// endpoint must not multiply the round trips an account already pays for on
// the machine actually using it.
func TestUsageEndpointReportsKnownAccountsWithNoNumbers(t *testing.T) {
	f := newAccountFixture(t)
	f.setIdentity("andy@avisoma.com")
	f.snapshot("avisoma", "tok-a", "andy@avisoma.com")
	f.snapshot("trecs", "tok-t", "andy@trecs.aero")
	f.snapshot("side", "tok-s", "andy@side.dev")

	resp := getUsage(t, newUsageServer())

	if len(resp.KnownAccounts) != 2 {
		t.Fatalf("knownAccounts = %#v, want the two non-live snapshots", resp.KnownAccounts)
	}
	trecs := findKnownAccount(t, resp, "trecs")
	if trecs.Account != "andy@trecs.aero" || trecs.Info != nil || trecs.Expired || trecs.Reason != "" {
		t.Fatalf("trecs entry = %#v, want the email with no fetch and no classification", trecs)
	}
	side := findKnownAccount(t, resp, "side")
	if side.Account != "andy@side.dev" || side.Info != nil {
		t.Fatalf("side entry = %#v, want the email with no numbers", side)
	}
}

// The live account's email is a free file read, so it is always reported —
// with no numbers, since fetching them is exactly what this endpoint no
// longer does.
func TestUsageEndpointReportsTheLiveEmailWithNoNumbers(t *testing.T) {
	f := newAccountFixture(t)
	f.setIdentity("andy@avisoma.com")
	f.snapshot("avisoma", "tok-a", "andy@avisoma.com")

	resp := getUsage(t, newUsageServer())

	if resp.Usage == nil || resp.Usage.Account != "andy@avisoma.com" {
		t.Fatalf("usage = %#v, want the live email reported", resp.Usage)
	}
	if resp.Usage.Info != nil {
		t.Fatalf("usage.info = %#v, want nil — this endpoint never fetches", resp.Usage.Info)
	}
	if resp.ActiveSnapshotName != "avisoma" {
		t.Fatalf("activeSnapshotName = %q, want avisoma", resp.ActiveSnapshotName)
	}
}

// activeSnapshotName is resolved from disk on every call and never cached, so a
// switch is visible on the very next request.
func TestUsageEndpointResolvesTheActiveSnapshotOnEveryCall(t *testing.T) {
	f := newAccountFixture(t)
	f.setIdentity("andy@avisoma.com")
	f.snapshot("avisoma", "tok-a", "andy@avisoma.com")
	f.snapshot("trecs", "tok-t", "andy@trecs.aero")

	s := newUsageServer()

	first := getUsage(t, s)
	if first.ActiveSnapshotName != "avisoma" {
		t.Fatalf("activeSnapshotName = %q, want avisoma", first.ActiveSnapshotName)
	}
	// The live snapshot is reported through Usage, never duplicated into the
	// known list.
	if len(first.KnownAccounts) != 1 || first.KnownAccounts[0].Name != "trecs" {
		t.Fatalf("knownAccounts = %#v, want the live account left out", first.KnownAccounts)
	}

	f.setIdentity("andy@trecs.aero")

	second := getUsage(t, s)
	if second.ActiveSnapshotName != "trecs" {
		t.Fatalf("activeSnapshotName = %q after a switch, want trecs — nothing here may be cached", second.ActiveSnapshotName)
	}
	if second.Usage == nil || second.Usage.Account != "andy@trecs.aero" {
		t.Fatalf("usage = %#v, want the new live account", second.Usage)
	}
	if len(second.KnownAccounts) != 1 || second.KnownAccounts[0].Name != "avisoma" {
		t.Fatalf("knownAccounts = %#v, want the previously-live account back in the list", second.KnownAccounts)
	}
}

func TestUsageEndpointUnauthorized(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/usage", nil)
	rec := httptest.NewRecorder()
	newUsageServer().usage(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
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
// a silent client break with no compile error on either side. The Swift package
// keeps its own copy of this fixture (not byte-identical — it also carries an
// `api` block, pinned separately by TestSessionsAdvertisesAPIHandshake on this
// side, since the anonymous struct below doesn't decode `api` at all). If this
// test fails because the shape legitimately changed, update both fixtures AND
// the Swift test.
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
	if full.CostUSD != 3.4712 || full.CostSubagentsUSD != 1.208 {
		t.Fatalf("enriched session lost cost: %+v", full)
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

// testDeviceToken is a syntactically valid APNs token: 64 hex characters.
const testDeviceToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestRegisterDeviceStoresToken(t *testing.T) {
	store := loadDeviceStore("", fixedClock(time.Now()))
	s := &server{token: "secret", devices: store}

	body := strings.NewReader(`{"device_token":"` + testDeviceToken + `","environment":"sandbox","platform":"ios"}`)
	req := httptest.NewRequest(http.MethodPost, "/devices", body)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()

	s.registerDevice(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	got := store.List()
	if len(got) != 1 || got[0].Token != testDeviceToken {
		t.Fatalf("devices = %+v, want one token %s", got, testDeviceToken)
	}
	if got[0].Environment != "sandbox" {
		t.Fatalf("Environment = %q, want %q", got[0].Environment, "sandbox")
	}
}

// Re-registering the same token is the normal path: the app registers on every
// launch because APNs tokens change on restore, reinstall, and some upgrades.
func TestRegisterDeviceIsIdempotent(t *testing.T) {
	store := loadDeviceStore("", fixedClock(time.Now()))
	s := &server{token: "secret", devices: store}

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodPost, "/devices",
			strings.NewReader(`{"device_token":"`+testDeviceToken+`","environment":"production"}`))
		req.Header.Set("Authorization", "Bearer secret")
		rec := httptest.NewRecorder()
		s.registerDevice(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d on attempt %d", rec.Code, i)
		}
	}
	if got := store.List(); len(got) != 1 {
		t.Fatalf("devices = %+v, want one after repeated registration", got)
	}
}

func TestRegisterDeviceRejectsBadInput(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"empty token", `{"device_token":""}`},
		{"too short", `{"device_token":"abc"}`},
		{"not hex", `{"device_token":"` + strings.Repeat("z", 64) + `"}`},
		{"bad environment", `{"device_token":"` + testDeviceToken + `","environment":"staging"}`},
		{"bad platform", `{"device_token":"` + testDeviceToken + `","platform":"android"}`},
		{"trailing json", `{"device_token":"` + testDeviceToken + `"}{"device_token":"x"}`},
		{"not json", `nope`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := loadDeviceStore("", fixedClock(time.Now()))
			s := &server{token: "secret", devices: store}
			req := httptest.NewRequest(http.MethodPost, "/devices", strings.NewReader(tt.body))
			req.Header.Set("Authorization", "Bearer secret")
			rec := httptest.NewRecorder()

			s.registerDevice(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
			}
			if got := store.List(); len(got) != 0 {
				t.Fatalf("stored a device from bad input: %+v", got)
			}
		})
	}
}

func TestRegisterDeviceUnauthorized(t *testing.T) {
	store := loadDeviceStore("", fixedClock(time.Now()))
	s := &server{token: "secret", devices: store}
	req := httptest.NewRequest(http.MethodPost, "/devices", strings.NewReader(`{"device_token":"abc"}`))
	rec := httptest.NewRecorder()

	s.registerDevice(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	if got := store.List(); len(got) != 0 {
		t.Fatalf("stored a device without auth: %+v", got)
	}
}

func TestUnregisterDeviceRemovesToken(t *testing.T) {
	store := loadDeviceStore("", fixedClock(time.Now()))
	store.Upsert(Device{Token: testDeviceToken})
	s := &server{token: "secret", devices: store}

	req := httptest.NewRequest(http.MethodDelete, "/devices/"+testDeviceToken, nil)
	req.SetPathValue("token", testDeviceToken)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()

	s.unregisterDevice(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	if got := store.List(); len(got) != 0 {
		t.Fatalf("devices = %+v, want empty", got)
	}
}

func TestUnregisterDeviceUnauthorized(t *testing.T) {
	store := loadDeviceStore("", fixedClock(time.Now()))
	store.Upsert(Device{Token: testDeviceToken})
	s := &server{token: "secret", devices: store}

	req := httptest.NewRequest(http.MethodDelete, "/devices/"+testDeviceToken, nil)
	req.SetPathValue("token", testDeviceToken)
	rec := httptest.NewRecorder()

	s.unregisterDevice(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	if got := store.List(); len(got) != 1 {
		t.Fatalf("unauthenticated delete removed a device: %+v", got)
	}
}

// A server built without notification support must not panic on these routes.
func TestDeviceRoutesWithoutRegistry(t *testing.T) {
	s := &server{token: "secret"}
	req := httptest.NewRequest(http.MethodPost, "/devices", strings.NewReader(`{"device_token":"abc"}`))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()

	s.registerDevice(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}

	req = httptest.NewRequest(http.MethodPost, "/devices/"+testDeviceToken+"/test", nil)
	req.SetPathValue("token", testDeviceToken)
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()

	s.sendTestPush(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("test-push status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

// testPushServer builds a server whose push path ends in a fake sender, wired
// exactly as cmdServer wires the real one.
func testPushServer(t *testing.T, sender pushSender, devices ...Device) (*server, *DeviceStore) {
	t.Helper()
	store := loadDeviceStore("", fixedClock(time.Now()))
	for _, d := range devices {
		store.Upsert(d)
	}
	return &server{
		token:    "secret",
		host:     "delta",
		hostID:   "host-1",
		bundleID: "com.skerla.claude-sessions",
		devices:  store,
		pusher:   sender,
	}, store
}

func postTestPush(s *server, token string, auth bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/devices/"+token+"/test", nil)
	req.SetPathValue("token", token)
	if auth {
		req.Header.Set("Authorization", "Bearer secret")
	}
	rec := httptest.NewRecorder()
	s.sendTestPush(rec, req)
	return rec
}

// The happy path: one push, to the named device only, carrying the same topic,
// push type, priority, collapse id and per-device environment a real alert from
// this host would carry.
func TestSendTestPushSendsToOneDevice(t *testing.T) {
	sender := &fakeSender{}
	other := strings.Repeat("a", 64)
	s, _ := testPushServer(t, sender,
		Device{Token: testDeviceToken, Environment: "sandbox", Platform: "ios"},
		Device{Token: other, Environment: "production", Platform: "ios"},
	)

	rec := postTestPush(s, testDeviceToken, true)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got, want := strings.TrimSpace(rec.Body.String()), `{"ok":true}`; got != want {
		t.Fatalf("body = %s, want %s", got, want)
	}
	sent := sender.requests()
	if len(sent) != 1 {
		t.Fatalf("sent %d pushes, want exactly one (the named device)", len(sent))
	}
	req := sent[0]
	if req.DeviceToken != testDeviceToken {
		t.Fatalf("DeviceToken = %q, want %q", req.DeviceToken, testDeviceToken)
	}
	// The stored environment, not a request-supplied one: a sandbox token
	// registered against a production gateway is the failure this endpoint
	// exists to diagnose, so it must be reproduced faithfully.
	if req.Environment != "sandbox" {
		t.Fatalf("Environment = %q, want the registered %q", req.Environment, "sandbox")
	}
	if req.Topic != "com.skerla.claude-sessions" {
		t.Fatalf("Topic = %q, want the configured bundle id", req.Topic)
	}
	if req.PushType != "alert" || req.Priority != "10" {
		t.Fatalf("PushType/Priority = %q/%q, want alert/10", req.PushType, req.Priority)
	}
	if want := "host-1:notify-test"; req.CollapseID != want {
		t.Fatalf("CollapseID = %q, want %q", req.CollapseID, want)
	}
	if !strings.Contains(string(req.Payload), "notify-test") {
		t.Fatalf("payload does not look like the notify-test alert: %s", req.Payload)
	}
}

// Apple's reason string is the diagnosis. It reaches the client exactly as the
// sender produced it — no wrapping, no rephrasing, no "push failed" prefix.
func TestSendTestPushPassesApplesReasonThrough(t *testing.T) {
	reason := "apns: 400 BadDeviceToken"
	sender := &fakeSender{err: errors.New(reason)}
	s, _ := testPushServer(t, sender, Device{Token: testDeviceToken, Environment: "production"})

	rec := postTestPush(s, testDeviceToken, true)

	// A failed push is a reported outcome, not a broken request: 200 with the
	// {ok,error} envelope, same as every other action on this server.
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var got actionResult
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (body %s)", err, rec.Body.String())
	}
	if got.OK {
		t.Fatalf("ok = true after a failed send")
	}
	if got.Error != reason {
		t.Fatalf("error = %q, want the sender's own string %q", got.Error, reason)
	}
}

// A device Apple calls dead stays registered. Removing it here would make the
// next diagnostic call answer 404 "unknown token" when the truth was "Apple
// rejected a token this host has" — the endpoint would destroy the evidence it
// exists to produce. A sandbox/production mismatch reads as exactly this.
func TestSendTestPushKeepsAGoneDevice(t *testing.T) {
	sender := &fakeSender{err: fmt.Errorf("%w (%s)", errDeviceGone, "BadDeviceToken")}
	s, store := testPushServer(t, sender, Device{Token: testDeviceToken, Environment: "sandbox"})

	rec := postTestPush(s, testDeviceToken, true)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var got actionResult
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if want := "apns: device token no longer valid (BadDeviceToken)"; got.Error != want {
		t.Fatalf("error = %q, want %q", got.Error, want)
	}
	if len(store.List()) != 1 {
		t.Fatalf("a test push pruned the device it was diagnosing: %+v", store.List())
	}
}

// An unknown token is a 404, and nothing is sent. A malformed token lands here
// too — it cannot be in the registry — so there is no separate 400 path.
func TestSendTestPushUnknownTokenIs404(t *testing.T) {
	sender := &fakeSender{}
	s, _ := testPushServer(t, sender, Device{Token: testDeviceToken, Environment: "sandbox"})

	for _, token := range []string{strings.Repeat("b", 64), "nonsense"} {
		rec := postTestPush(s, token, true)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d for token %q, want %d", rec.Code, token, http.StatusNotFound)
		}
	}
	if len(sender.requests()) != 0 {
		t.Fatalf("pushed to an unregistered token: %+v", sender.requests())
	}
}

func TestSendTestPushUnauthorized(t *testing.T) {
	sender := &fakeSender{}
	s, _ := testPushServer(t, sender, Device{Token: testDeviceToken})

	rec := postTestPush(s, testDeviceToken, false)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	if len(sender.requests()) != 0 {
		t.Fatalf("pushed without auth: %+v", sender.requests())
	}
}

// A host with a device registry but no APNs key — the common unconfigured
// case, and the one the settings screen hits — reports 503 rather than a
// failed push.
func TestSendTestPushWithoutAPNsClient(t *testing.T) {
	store := loadDeviceStore("", fixedClock(time.Now()))
	store.Upsert(Device{Token: testDeviceToken})
	s := &server{token: "secret", devices: store}

	rec := postTestPush(s, testDeviceToken, true)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

// The registry must not grow without bound, but a device already registered
// must still be able to re-register — which it does on every app launch.
func TestRegisterDeviceEnforcesCap(t *testing.T) {
	store := loadDeviceStore("", fixedClock(time.Now()))
	for i := 0; i < maxRegisteredDevices; i++ {
		store.Upsert(Device{Token: fmt.Sprintf("%064x", i)})
	}
	s := &server{token: "secret", devices: store}

	post := func(token string) int {
		req := httptest.NewRequest(http.MethodPost, "/devices",
			strings.NewReader(`{"device_token":"`+token+`"}`))
		req.Header.Set("Authorization", "Bearer secret")
		rec := httptest.NewRecorder()
		s.registerDevice(rec, req)
		return rec.Code
	}

	if code := post(testDeviceToken); code != http.StatusConflict {
		t.Fatalf("status = %d for a new device at the cap, want %d", code, http.StatusConflict)
	}
	existing := fmt.Sprintf("%064x", 0)
	if code := post(existing); code != http.StatusNoContent {
		t.Fatalf("status = %d re-registering an existing device at the cap, want %d", code, http.StatusNoContent)
	}
}

// A zero-length hostname is permitted — a UTS namespace can set one — and it
// reaches the pairing QR as a missing trailing field. shortHostname is used as
// a display name, so it must never be empty.
func TestShortHostnameIsNeverEmpty(t *testing.T) {
	if got := shortHostname(); got == "" {
		t.Fatalf("shortHostname() = %q, want a non-empty label", got)
	}
}

// hostPort must bracket an IPv6 literal. Plain "%s:%d" concatenation turned
// `--bind ::` into ":::8765", which is not the address the user asked for.
func TestHostPortBracketsIPv6(t *testing.T) {
	tests := []struct {
		host string
		want string
	}{
		{"127.0.0.1", "127.0.0.1:8765"},
		{"100.80.11.125", "100.80.11.125:8765"},
		{"0.0.0.0", "0.0.0.0:8765"},
		{"localhost", "localhost:8765"},
		{"::", "[::]:8765"},
		{"[::]", "[::]:8765"},
		{"::1", "[::1]:8765"},
		{"", ":8765"},
	}
	for _, tt := range tests {
		if got := hostPort(tt.host, 8765); got != tt.want {
			t.Errorf("hostPort(%q, 8765) = %q, want %q", tt.host, got, tt.want)
		}
	}
}

// The startup banner is the only thing the server writes to stdout, and it
// carries the auth token. Under a supervisor that stream is a log file or the
// journal, so the token is only printed when stdout is a terminal.
func TestServerBannerWithdrawsTokenOffTerminal(t *testing.T) {
	const tok = "s3cr3t-token-value-not-for-logs"

	onTTY := serverBanner("delta", "127.0.0.1", 8765, tok, "", true)
	if !strings.Contains(onTTY, tok) {
		t.Error("banner on a terminal omitted the token; it is how a user configures servers.yaml")
	}

	offTTY := serverBanner("delta", "127.0.0.1", 8765, tok, "", false)
	if strings.Contains(offTTY, tok) {
		t.Errorf("banner off a terminal leaked the token:\n%s", offTTY)
	}
	// Withdrawing it is only half the job — the user still has to be able to
	// find it, or the redirected banner is just broken.
	if !strings.Contains(offTTY, serverTokenPath()) {
		t.Errorf("banner off a terminal must name where to read the token, got:\n%s", offTTY)
	}
	// Everything that is not the token is still worth logging.
	for _, want := range []string{"delta", "127.0.0.1", "8765"} {
		if !strings.Contains(offTTY, want) {
			t.Errorf("banner off a terminal dropped %q, which is not a secret", want)
		}
	}
}

// The macOS GUI builds ship the CLI inside the app bundle with no symlink, so
// a fully-authenticated Mac can have nothing named `tailscale` on PATH. These
// pin that the fallback exists and that PATH still wins.
func TestTailscaleBinaryFallsBackToBundledPath(t *testing.T) {
	writeExe := func(dir, name string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
		return p
	}

	t.Run("PATH wins over the bundle", func(t *testing.T) {
		pathDir := t.TempDir()
		onPath := writeExe(pathDir, "tailscale")
		bundled := writeExe(t.TempDir(), "Tailscale")
		t.Setenv("PATH", pathDir)
		defer swapBundledPaths(t, []string{bundled})()

		if got := tailscaleBinary(); got != onPath {
			t.Errorf("tailscaleBinary() = %q, want the PATH copy %q", got, onPath)
		}
	})

	t.Run("bundle used when PATH has none", func(t *testing.T) {
		bundled := writeExe(t.TempDir(), "Tailscale")
		t.Setenv("PATH", t.TempDir())
		defer swapBundledPaths(t, []string{bundled})()

		if got := tailscaleBinary(); got != bundled {
			t.Errorf("tailscaleBinary() = %q, want the bundled copy %q", got, bundled)
		}
	})

	t.Run("non-executable and missing entries are skipped", func(t *testing.T) {
		dir := t.TempDir()
		notExec := filepath.Join(dir, "tailscale")
		if err := os.WriteFile(notExec, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", t.TempDir())
		defer swapBundledPaths(t, []string{filepath.Join(dir, "absent"), dir, notExec})()

		if got := tailscaleBinary(); got != "" {
			t.Errorf("tailscaleBinary() = %q, want \"\" — nothing runnable", got)
		}
	})
}

func swapBundledPaths(t *testing.T, paths []string) func() {
	t.Helper()
	orig := tailscaleBundledPaths
	tailscaleBundledPaths = paths
	return func() { tailscaleBundledPaths = orig }
}

// The two causes need different fixes, and conflating them sent users to check
// a daemon that was running fine.
func TestTailscaleBindFailureNamesTheActualCause(t *testing.T) {
	t.Run("no command found", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		defer swapBundledPaths(t, nil)()

		msg := tailscaleBindFailure()
		if !strings.Contains(msg, "no tailscale command was found") {
			t.Errorf("message does not name the missing command:\n%s", msg)
		}
		if strings.Contains(msg, "is tailscaled running") {
			t.Errorf("message blames the daemon when the command is missing:\n%s", msg)
		}
	})

	t.Run("command present but no address", func(t *testing.T) {
		dir := t.TempDir()
		bin := filepath.Join(dir, "tailscale")
		if err := os.WriteFile(bin, []byte("#!/bin/sh\necho 'nothing useful here'\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", dir)
		defer swapBundledPaths(t, nil)()

		msg := tailscaleBindFailure()
		if !strings.Contains(msg, "is tailscaled running") {
			t.Errorf("message does not point at the daemon:\n%s", msg)
		}
		if !strings.Contains(msg, bin) {
			t.Errorf("message does not name which binary it ran (%s):\n%s", bin, msg)
		}
		// Quoting the CLI's own words is what would have shown "The Tailscale
		// GUI failed to start" the first time instead of a guess.
		if !strings.Contains(msg, "it said: nothing useful here") {
			t.Errorf("message does not quote what the CLI printed:\n%s", msg)
		}
	})
}

// The macOS app bundle's CLI exits 0 and prints "The Tailscale GUI failed to
// start: ..." on stdout when it cannot reach the GUI — which is what happens
// under launchd. Trusting the first non-empty line handed that sentence back
// as an address, and the bind failed with a DNS lookup of it.
func TestTailscaleIPv4OnlyAcceptsAnIPv4(t *testing.T) {
	tests := []struct {
		name   string
		stdout string
		want   string
	}{
		{name: "plain address", stdout: "100.107.145.1\n", want: "100.107.145.1"},
		{name: "GUI error on stdout, exit 0", stdout: "The Tailscale GUI failed to start: The operation couldn't be completed. (Tailscale.CLIError error 3.)\n"},
		{name: "empty output", stdout: ""},
		{name: "IPv6 only is not an IPv4 bind", stdout: "fd7a:115c:a1e0::1\n"},
		{name: "IPv6 first, IPv4 second", stdout: "fd7a:115c:a1e0::1\n100.107.145.1\n", want: "100.107.145.1"},
		{name: "leading blank lines", stdout: "\n\n100.64.0.9\n", want: "100.64.0.9"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			// The fake cats a fixture file rather than embedding a heredoc, so
			// nothing in the payload can be read as shell syntax. /bin/cat is
			// absolute because PATH below is narrowed to this dir, which would
			// otherwise make the fake exit 127 and every case return "" — the
			// negative cases would then pass for the wrong reason.
			payload := filepath.Join(dir, "out.txt")
			if err := os.WriteFile(payload, []byte(tt.stdout), 0o644); err != nil {
				t.Fatal(err)
			}
			fake := filepath.Join(dir, "tailscale")
			if err := os.WriteFile(fake, []byte("#!/bin/sh\nexec /bin/cat "+payload+"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir)
			defer swapBundledPaths(t, nil)()

			if got := tailscaleIPv4Context(context.Background()); got != tt.want {
				t.Errorf("tailscaleIPv4Context() = %q, want %q", got, tt.want)
			}
		})
	}
}

// --- TASK6: session_id preconditions on kill/migrate -----------------------

// killRequest builds an authed kill request for pid with the given raw body.
// A nil body reproduces the eight pre-existing callers (and `curl -X POST`
// with no -d), which must keep meaning "no precondition".
func killRequest(t *testing.T, pid int, body string) *http.Request {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(http.MethodPost, fmt.Sprintf("/sessions/%d/kill", pid), nil)
	} else {
		req = httptest.NewRequest(http.MethodPost, fmt.Sprintf("/sessions/%d/kill", pid), strings.NewReader(body))
	}
	req.SetPathValue("pid", strconv.Itoa(pid))
	req.Header.Set("Authorization", "Bearer secret")
	return req
}

func decodeAction(t *testing.T, rec *httptest.ResponseRecorder) actionResult {
	t.Helper()
	var got actionResult
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
	return got
}

// TestKillHandlerMatchingSessionIDTerminates: a precondition naming the session
// the server itself resolved for that PID proceeds exactly as an unconditional
// kill does.
func TestKillHandlerMatchingSessionIDTerminates(t *testing.T) {
	live := Session{PID: 55, SessionID: "aaaa-1111", Tmux: "work:1.0"}
	var got Session
	s := &server{
		token:     "secret",
		collect:   func() ([]Session, error) { return []Session{live}, nil },
		attest:    func(int) (Session, bool) { return Session{PID: 55, SessionID: "aaaa-1111"}, true },
		terminate: func(target Session) error { got = target; return nil },
	}
	rec := httptest.NewRecorder()
	s.kill(rec, killRequest(t, 55, `{"session_id":"aaaa-1111"}`))

	if got != live {
		t.Fatalf("terminated %#v, want %#v", got, live)
	}
	if r := decodeAction(t, rec); !r.OK || r.Code != "" {
		t.Fatalf("result = %#v, want ok with no code", r)
	}
}

// TestKillHandlerRecycledPIDRefuses is the case D14 describes and the reason
// this precondition exists: the session the phone was looking at exited, the
// pane was recycled, and PID 55 now belongs to somebody else. The kill must
// refuse rather than terminate the innocent occupant.
func TestKillHandlerRecycledPIDRefuses(t *testing.T) {
	occupant := Session{PID: 55, SessionID: "bbbb-2222", Tmux: "someone-else:0.0"}
	terminated := false
	s := &server{
		token:     "secret",
		collect:   func() ([]Session, error) { return []Session{occupant}, nil },
		terminate: func(Session) error { terminated = true; return nil },
	}
	rec := httptest.NewRecorder()
	// The id the phone last saw at this PID, ten minutes ago.
	s.kill(rec, killRequest(t, 55, `{"session_id":"aaaa-1111"}`))

	if terminated {
		t.Fatal("terminate called for a recycled PID")
	}
	r := decodeAction(t, rec)
	if r.OK {
		t.Fatalf("result = %#v, want refusal", r)
	}
	if r.Code != codeSessionMismatch {
		t.Fatalf("code = %q, want %q", r.Code, codeSessionMismatch)
	}
	if r.Error == "" {
		t.Fatal("refusal carries no human-readable error")
	}
	// The refusal must not leak the occupant's identity back to a client that
	// guessed at it — the client learns only that its target is gone.
	if strings.Contains(r.Error, occupant.SessionID) {
		t.Fatalf("error %q leaks the occupant session id", r.Error)
	}
}

// TestKillHandlerSessionIDForDeadPIDRefuses: a precondition against a PID that
// holds no live session at all is `not_live`, not a mismatch.
func TestKillHandlerSessionIDForDeadPIDRefuses(t *testing.T) {
	terminated := false
	s := &server{
		token:     "secret",
		collect:   func() ([]Session, error) { return nil, nil },
		terminate: func(Session) error { terminated = true; return nil },
	}
	rec := httptest.NewRecorder()
	s.kill(rec, killRequest(t, 55, `{"session_id":"aaaa-1111"}`))

	if terminated {
		t.Fatal("terminate called for a PID with no live session")
	}
	r := decodeAction(t, rec)
	if r.OK || r.Code != codeNotLive {
		t.Fatalf("result = %#v, want refusal with code %q", r, codeNotLive)
	}
	if r.Error != "PID 55 is not a live Claude session" {
		t.Fatalf("error = %q, want the pre-existing not-live wording", r.Error)
	}
}

// TestKillHandlerEmptyPreconditionKeepsLegacyBehaviour: the desktop sends `{}`
// and scripted callers send nothing at all. Both must still mean "kill whatever
// is at this PID", byte-compatibly with the pre-TASK6 server.
func TestKillHandlerEmptyPreconditionKeepsLegacyBehaviour(t *testing.T) {
	for _, body := range []string{"", "{}", `{"session_id":""}`, "   ", "\n"} {
		t.Run(fmt.Sprintf("body=%q", body), func(t *testing.T) {
			live := Session{PID: 55, SessionID: "aaaa-1111"}
			var got Session
			s := &server{
				token:     "secret",
				collect:   func() ([]Session, error) { return []Session{live}, nil },
				terminate: func(target Session) error { got = target; return nil },
			}
			rec := httptest.NewRecorder()
			s.kill(rec, killRequest(t, 55, body))

			if got != live {
				t.Fatalf("terminated %#v, want %#v", got, live)
			}
			if r := decodeAction(t, rec); !r.OK || r.Code != "" {
				t.Fatalf("result = %#v, want a plain ok", r)
			}
		})
	}
}

// TestKillHandlerMalformedPreconditionIsBadRequest: an empty body is "no
// precondition", but non-empty junk is a client bug and gets the same 400 the
// other handlers give it. Terminate is never reached.
func TestKillHandlerMalformedPreconditionIsBadRequest(t *testing.T) {
	for _, body := range []string{"not json", `{"session_id":`, `{"session_id":42}`, "[]"} {
		t.Run(body, func(t *testing.T) {
			terminated := false
			collected := false
			s := &server{
				token:     "secret",
				collect:   func() ([]Session, error) { collected = true; return nil, nil },
				terminate: func(Session) error { terminated = true; return nil },
			}
			rec := httptest.NewRecorder()
			s.kill(rec, killRequest(t, 55, body))

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %q)", rec.Code, rec.Body.String())
			}
			if collected || terminated {
				t.Fatalf("malformed body reached the session list (collected=%v terminated=%v)", collected, terminated)
			}
		})
	}
}

// TestKillHandlerPreconditionCheckedBeforeWorktree: the mismatch refusal
// happens before worktreeCleanupTarget is consulted, so a refused kill never
// invites the client to remove a worktree that is still in use.
func TestKillHandlerPreconditionCheckedBeforeWorktree(t *testing.T) {
	s := &server{
		token: "secret",
		collect: func() ([]Session, error) {
			return []Session{{PID: 55, SessionID: "bbbb-2222", CWD: "/tmp/wt"}}, nil
		},
		terminate: func(Session) error { return nil },
	}
	rec := httptest.NewRecorder()
	s.kill(rec, killRequest(t, 55, `{"session_id":"aaaa-1111"}`))

	r := decodeAction(t, rec)
	if r.Worktree != nil {
		t.Fatalf("refused kill offered a worktree: %#v", r.Worktree)
	}
}

// migrateRequest builds an authed migrate request for pid with a raw body.
func migrateRequest(t *testing.T, pid int, body string) *http.Request {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(http.MethodPost, fmt.Sprintf("/sessions/%d/migrate", pid), nil)
	} else {
		req = httptest.NewRequest(http.MethodPost, fmt.Sprintf("/sessions/%d/migrate", pid), strings.NewReader(body))
	}
	req.SetPathValue("pid", strconv.Itoa(pid))
	req.Header.Set("Authorization", "Bearer secret")
	return req
}

// TestMigrateHandlerWithoutPreconditionDoesNotCollect: the desktop path costs
// exactly what it cost before — no session list is collected when no
// precondition was supplied.
func TestMigrateHandlerWithoutPreconditionDoesNotCollect(t *testing.T) {
	for _, body := range []string{"", "{}", `{"session_id":""}`} {
		t.Run(fmt.Sprintf("body=%q", body), func(t *testing.T) {
			t.Setenv("HOME", t.TempDir()) // no session file: MigrateLocal fails fast
			collected := false
			s := &server{
				token:   "secret",
				collect: func() ([]Session, error) { collected = true; return nil, nil },
			}
			rec := httptest.NewRecorder()
			s.migrate(rec, migrateRequest(t, 999999, body))

			if collected {
				t.Fatal("collect called for a migrate with no precondition")
			}
			r := decodeAction(t, rec)
			if r.Error != "no session file for PID 999999" {
				t.Fatalf("error = %q, want MigrateLocal's own error", r.Error)
			}
			if r.Code != "" {
				t.Fatalf("code = %q, want empty for a MigrateLocal failure", r.Code)
			}
		})
	}
}

// TestMigrateHandlerMatchingPreconditionReachesMigrate: a matching precondition
// falls through to MigrateLocal, proved by MigrateLocal's own error surfacing.
func TestMigrateHandlerMatchingPreconditionReachesMigrate(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := &server{
		token: "secret",
		collect: func() ([]Session, error) {
			return []Session{{PID: 999999, SessionID: "aaaa-1111"}}, nil
		},
	}
	rec := httptest.NewRecorder()
	s.migrate(rec, migrateRequest(t, 999999, `{"session_id":"aaaa-1111"}`))

	r := decodeAction(t, rec)
	if r.Error != "no session file for PID 999999" {
		t.Fatalf("error = %q, want to have reached MigrateLocal", r.Error)
	}
}

// TestMigrateHandlerRecycledPIDRefuses: the same recycled-pane case as kill.
// MigrateLocal is never reached — proved by its distinctive error being absent,
// and by the tmux stub never being invoked.
func TestMigrateHandlerRecycledPIDRefuses(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	logPath := installFakeTmux(t)
	s := &server{
		token: "secret",
		collect: func() ([]Session, error) {
			return []Session{{PID: 999999, SessionID: "bbbb-2222"}}, nil
		},
	}
	rec := httptest.NewRecorder()
	s.migrate(rec, migrateRequest(t, 999999, `{"session_id":"aaaa-1111"}`))

	r := decodeAction(t, rec)
	if r.OK || r.Code != codeSessionMismatch {
		t.Fatalf("result = %#v, want refusal with code %q", r, codeSessionMismatch)
	}
	if strings.Contains(r.Error, "no session file") {
		t.Fatalf("error = %q: MigrateLocal was reached", r.Error)
	}
	if _, err := os.Stat(logPath); err == nil {
		data, _ := os.ReadFile(logPath)
		t.Fatalf("refused migrate invoked tmux:\n%s", data)
	}
}

// TestMigrateHandlerPreconditionForDeadPIDRefuses: `not_live`, and MigrateLocal
// is never reached even though it would have produced its own error.
func TestMigrateHandlerPreconditionForDeadPIDRefuses(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := &server{
		token:   "secret",
		collect: func() ([]Session, error) { return nil, nil },
	}
	rec := httptest.NewRecorder()
	s.migrate(rec, migrateRequest(t, 999999, `{"session_id":"aaaa-1111"}`))

	r := decodeAction(t, rec)
	if r.OK || r.Code != codeNotLive {
		t.Fatalf("result = %#v, want refusal with code %q", r, codeNotLive)
	}
	if strings.Contains(r.Error, "no session file") {
		t.Fatalf("error = %q: MigrateLocal was reached", r.Error)
	}
}

// TestMigrateHandlerMalformedPreconditionIsBadRequest mirrors kill's.
func TestMigrateHandlerMalformedPreconditionIsBadRequest(t *testing.T) {
	collected := false
	s := &server{
		token:   "secret",
		collect: func() ([]Session, error) { collected = true; return nil, nil },
	}
	rec := httptest.NewRecorder()
	s.migrate(rec, migrateRequest(t, 999999, "not json"))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if collected {
		t.Fatal("malformed body reached the session list")
	}
}

// TestMigrateHandlerCollectionErrorDoesNotMigrate: a collection failure with a
// precondition supplied refuses rather than falling through unchecked.
func TestMigrateHandlerCollectionErrorDoesNotMigrate(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := &server{
		token:   "secret",
		collect: func() ([]Session, error) { return nil, errors.New("collect boom") },
	}
	rec := httptest.NewRecorder()
	s.migrate(rec, migrateRequest(t, 999999, `{"session_id":"aaaa-1111"}`))

	r := decodeAction(t, rec)
	if r.OK || r.Error != "collect boom" {
		t.Fatalf("result = %#v, want the collection error", r)
	}
	if strings.Contains(r.Error, "no session file") {
		t.Fatal("MigrateLocal was reached despite an unverifiable precondition")
	}
}

// --- TASK6: request_id makes spawn idempotent ------------------------------

// newRequest builds an authed /sessions/new request from a body map.
func newSessionRequest(t *testing.T, body map[string]string) *http.Request {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/sessions/new", strings.NewReader(string(data)))
	req.Header.Set("Authorization", "Bearer secret")
	return req
}

// spawnRecorder is a server whose spawn seam counts calls and answers from a
// script, so the dedupe can be exercised without starting tmux.
func spawnRecorder(t *testing.T, result func(n int) (string, error)) (*server, func() int) {
	t.Helper()
	var mu sync.Mutex
	calls := 0
	s := &server{
		token: "secret",
		spawn: func(cwd, name, command, suffix string) (string, error) {
			mu.Lock()
			calls++
			n := calls
			mu.Unlock()
			return result(n)
		},
	}
	return s, func() int {
		mu.Lock()
		defer mu.Unlock()
		return calls
	}
}

// TestNewSessionSameRequestIDSpawnsOnce: the phone gave up and tapped again.
// The replayed response is byte-identical and nothing new was created.
func TestNewSessionSameRequestIDSpawnsOnce(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	s, calls := spawnRecorder(t, func(int) (string, error) { return "work-abc123", nil })

	body := map[string]string{"cwd": home, "request_id": "task6-verify-001"}
	first := httptest.NewRecorder()
	s.newSession(first, newSessionRequest(t, body))
	second := httptest.NewRecorder()
	s.newSession(second, newSessionRequest(t, body))

	if got := calls(); got != 1 {
		t.Fatalf("spawn called %d times, want 1", got)
	}
	if first.Body.String() != second.Body.String() {
		t.Fatalf("replay differs:\nfirst  %s\nsecond %s", first.Body.String(), second.Body.String())
	}
	if r := decodeAction(t, first); !r.OK || r.Tmux != "work-abc123" {
		t.Fatalf("result = %#v", r)
	}
}

// TestNewSessionSameRequestIDConcurrentlySpawnsOnce is the racing interleaving:
// two requests arriving close enough together that the first has not finished.
// The second must join the first's flight, not start a second spawn.
func TestNewSessionSameRequestIDConcurrentlySpawnsOnce(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	release := make(chan struct{})
	entered := make(chan struct{}, 8)
	var mu sync.Mutex
	calls := 0
	s := &server{
		token: "secret",
		spawn: func(string, string, string, string) (string, error) {
			mu.Lock()
			calls++
			mu.Unlock()
			entered <- struct{}{}
			<-release // hold the flight open until every caller has arrived
			return "work-abc123", nil
		},
	}

	const racers = 8
	body := map[string]string{"cwd": home, "request_id": "task6-concurrent"}
	bodies := make([]string, racers)
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := httptest.NewRecorder()
			s.newSession(rec, newSessionRequest(t, body))
			bodies[i] = rec.Body.String()
		}(i)
	}
	<-entered // the winner is inside SpawnNew; the rest are joining or about to
	// Give the losers time to reach the dedupe before letting the winner finish,
	// so this exercises the join rather than the post-completion replay.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 1 {
		t.Fatalf("spawn called %d times, want 1", got)
	}
	for i, b := range bodies {
		if b != bodies[0] {
			t.Fatalf("racer %d got %s, racer 0 got %s", i, b, bodies[0])
		}
	}
	var r actionResult
	if err := json.Unmarshal([]byte(bodies[0]), &r); err != nil {
		t.Fatal(err)
	}
	if !r.OK || r.Tmux != "work-abc123" {
		t.Fatalf("result = %#v", r)
	}
}

// TestNewSessionConcurrentFailureStillSpawnsOnce: the in-flight join covers
// failures too. Both callers see the same failure and SpawnNew ran once —
// otherwise a retry storm against a broken tmux multiplies the damage.
func TestNewSessionConcurrentFailureStillSpawnsOnce(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	release := make(chan struct{})
	entered := make(chan struct{}, 4)
	var mu sync.Mutex
	calls := 0
	s := &server{
		token: "secret",
		spawn: func(string, string, string, string) (string, error) {
			mu.Lock()
			calls++
			mu.Unlock()
			entered <- struct{}{}
			<-release
			return "", errors.New("tmux new-session: exit status 1")
		},
	}

	body := map[string]string{"cwd": home, "request_id": "task6-concurrent-fail"}
	bodies := make([]string, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := httptest.NewRecorder()
			s.newSession(rec, newSessionRequest(t, body))
			bodies[i] = rec.Body.String()
		}(i)
	}
	<-entered
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 1 {
		t.Fatalf("spawn called %d times, want 1", got)
	}
	if bodies[0] != bodies[1] {
		t.Fatalf("joined failure differs: %s vs %s", bodies[0], bodies[1])
	}
	var r actionResult
	if err := json.Unmarshal([]byte(bodies[0]), &r); err != nil {
		t.Fatal(err)
	}
	if r.OK || !strings.Contains(r.Error, "tmux new-session") {
		t.Fatalf("result = %#v, want the spawn failure", r)
	}
}

// TestNewSessionFailedSpawnIsNotRemembered: nothing was created, so a retry
// must genuinely re-run. Caching a transient failure for ten minutes would
// leave the user unable to retry a spawn that would now work.
func TestNewSessionFailedSpawnIsNotRemembered(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	s, calls := spawnRecorder(t, func(n int) (string, error) {
		if n == 1 {
			return "", errors.New("tmux new-session: exit status 1")
		}
		return "work-abc123", nil
	})

	body := map[string]string{"cwd": home, "request_id": "task6-retry"}
	first := httptest.NewRecorder()
	s.newSession(first, newSessionRequest(t, body))
	second := httptest.NewRecorder()
	s.newSession(second, newSessionRequest(t, body))

	if got := calls(); got != 2 {
		t.Fatalf("spawn called %d times, want 2", got)
	}
	if r := decodeAction(t, first); r.OK {
		t.Fatalf("first result = %#v, want failure", r)
	}
	if r := decodeAction(t, second); !r.OK || r.Tmux != "work-abc123" {
		t.Fatalf("second result = %#v, want the retry to succeed", r)
	}
}

// TestNewSessionDifferentRequestIDsSpawnTwice: dedupe is per id, not global.
func TestNewSessionDifferentRequestIDsSpawnTwice(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	s, calls := spawnRecorder(t, func(n int) (string, error) {
		return fmt.Sprintf("work-%06d", n), nil
	})

	for _, id := range []string{"task6-aaaa", "task6-bbbb"} {
		rec := httptest.NewRecorder()
		s.newSession(rec, newSessionRequest(t, map[string]string{"cwd": home, "request_id": id}))
		if r := decodeAction(t, rec); !r.OK {
			t.Fatalf("id %q: %#v", id, r)
		}
	}
	if got := calls(); got != 2 {
		t.Fatalf("spawn called %d times, want 2", got)
	}
}

// TestNewSessionWithoutRequestIDNeverDedupes: the pre-TASK6 caller sends no id
// and every call must still create a session.
func TestNewSessionWithoutRequestIDNeverDedupes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	s, calls := spawnRecorder(t, func(n int) (string, error) {
		return fmt.Sprintf("work-%06d", n), nil
	})

	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		s.newSession(rec, newSessionRequest(t, map[string]string{"cwd": home}))
		if r := decodeAction(t, rec); !r.OK {
			t.Fatalf("call %d: %#v", i, r)
		}
	}
	if got := calls(); got != 3 {
		t.Fatalf("spawn called %d times, want 3", got)
	}
}

// TestNewSessionRequestIDBounds sits on both edges of the accepted length and
// character set: 8 and 128 are in, 7 and 129 are out, and anything outside
// [A-Za-z0-9_-] is out because this value becomes a long-lived map key.
func TestNewSessionRequestIDBounds(t *testing.T) {
	home := t.TempDir()
	tests := []struct {
		name string
		id   string
		want int
	}{
		{"seven chars", strings.Repeat("a", 7), http.StatusBadRequest},
		{"eight chars", strings.Repeat("a", 8), http.StatusOK},
		{"128 chars", strings.Repeat("a", 128), http.StatusOK},
		{"129 chars", strings.Repeat("a", 129), http.StatusBadRequest},
		{"four chars", "abcd", http.StatusBadRequest},
		{"slash", "task6/verify", http.StatusBadRequest},
		{"dot", "task6.verify", http.StatusBadRequest},
		{"space", "task6 verify", http.StatusBadRequest},
		{"newline", "task6\nverify", http.StatusBadRequest},
		{"underscore and dash", "task6_verify-001", http.StatusOK},
		{"unicode", "task6-vérify", http.StatusBadRequest},
		{"nul", "task6\x00verify", http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", home)
			s, calls := spawnRecorder(t, func(int) (string, error) { return "work-abc123", nil })
			rec := httptest.NewRecorder()
			s.newSession(rec, newSessionRequest(t, map[string]string{"cwd": home, "request_id": tt.id}))

			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tt.want, rec.Body.String())
			}
			if tt.want == http.StatusBadRequest {
				if strings.TrimSpace(rec.Body.String()) != "bad request_id" {
					t.Fatalf("body = %q, want %q", rec.Body.String(), "bad request_id")
				}
				if got := calls(); got != 0 {
					t.Fatalf("spawn called %d times for a rejected request_id", got)
				}
			}
		})
	}
}

// TestNewSessionRequestIDMissingIsNotMalformed: an absent id is not a short id.
// The empty string must read as "no idempotency", never as a 400.
func TestNewSessionRequestIDMissingIsNotMalformed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	s, calls := spawnRecorder(t, func(int) (string, error) { return "work-abc123", nil })
	rec := httptest.NewRecorder()
	s.newSession(rec, newSessionRequest(t, map[string]string{"cwd": home, "request_id": ""}))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if got := calls(); got != 1 {
		t.Fatalf("spawn called %d times, want 1", got)
	}
}

// TestNewSessionReplayDoesNotInvalidateTwice: the replay returns the stored
// result without re-running the spawn's side effects, of which the session-cache
// invalidation is the observable one.
func TestNewSessionReplayDoesNotInvalidateTwice(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	s, _ := spawnRecorder(t, func(int) (string, error) { return "work-abc123", nil })

	body := map[string]string{"cwd": home, "request_id": "task6-invalidate"}
	s.newSession(httptest.NewRecorder(), newSessionRequest(t, body))
	before := s.sessionCache.generation
	s.newSession(httptest.NewRecorder(), newSessionRequest(t, body))

	if after := s.sessionCache.generation; after != before {
		t.Fatalf("cache generation moved on replay: %d -> %d", before, after)
	}
}

// TestNewSessionDedupeExpiresAtTTL sits on the TTL edge: a replay one
// nanosecond inside ten minutes still replays, and one nanosecond past it
// spawns again.
func TestNewSessionDedupeExpiresAtTTL(t *testing.T) {
	home := t.TempDir()
	tests := []struct {
		name  string
		after time.Duration
		want  int
	}{
		{"just inside the TTL", spawnDedupeTTL - time.Nanosecond, 1},
		{"exactly at the TTL", spawnDedupeTTL, 2},
		{"past the TTL", spawnDedupeTTL + time.Nanosecond, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", home)
			base := time.Unix(1700000000, 0)
			now := base
			s, calls := spawnRecorder(t, func(n int) (string, error) {
				return fmt.Sprintf("work-%06d", n), nil
			})
			s.spawns.now = func() time.Time { return now }

			body := map[string]string{"cwd": home, "request_id": "task6-ttl-edge"}
			s.newSession(httptest.NewRecorder(), newSessionRequest(t, body))
			now = base.Add(tt.after)
			s.newSession(httptest.NewRecorder(), newSessionRequest(t, body))

			if got := calls(); got != tt.want {
				t.Fatalf("spawn called %d times, want %d", got, tt.want)
			}
		})
	}
}

// TestNewSessionDedupeIsBounded sits on the cap: the most recent
// spawnDedupeMax ids are remembered and the oldest is forgotten, so a
// long-running server cannot accumulate request ids without limit.
func TestNewSessionDedupeIsBounded(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	now := time.Unix(1700000000, 0)
	s, calls := spawnRecorder(t, func(n int) (string, error) {
		return fmt.Sprintf("work-%06d", n), nil
	})
	s.spawns.now = func() time.Time { return now }

	fire := func(id string) {
		s.newSession(httptest.NewRecorder(), newSessionRequest(t,
			map[string]string{"cwd": home, "request_id": id}))
	}

	// Fill exactly to the cap, one second apart so eviction order is defined.
	for i := 0; i < spawnDedupeMax; i++ {
		fire(fmt.Sprintf("task6-cap-%04d", i))
		now = now.Add(time.Second)
	}
	if got := calls(); got != spawnDedupeMax {
		t.Fatalf("spawn called %d times filling the cap, want %d", got, spawnDedupeMax)
	}
	if n := s.spawns.len(); n > spawnDedupeMax {
		t.Fatalf("dedupe holds %d entries, cap is %d", n, spawnDedupeMax)
	}
	// The newest is still remembered.
	fire(fmt.Sprintf("task6-cap-%04d", spawnDedupeMax-1))
	if got := calls(); got != spawnDedupeMax {
		t.Fatalf("spawn called %d times, want the newest id to replay", got)
	}
	// One more distinct id must evict something rather than grow the map.
	fire("task6-cap-overflow")
	if n := s.spawns.len(); n > spawnDedupeMax {
		t.Fatalf("dedupe grew to %d entries past the cap of %d", n, spawnDedupeMax)
	}
	// And what it evicted is the oldest, which therefore spawns again.
	before := calls()
	fire("task6-cap-0000")
	if got := calls(); got != before+1 {
		t.Fatalf("oldest id replayed instead of being evicted (calls %d -> %d)", before, got)
	}
}

// TestNewSessionDedupeNeverEvictsAnInflightSpawn: eviction pressure must not
// drop the entry a joiner is waiting on, or the join degrades into a second
// spawn of the same user action — the exact thing request_id exists to stop.
func TestNewSessionDedupeNeverEvictsAnInflightSpawn(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	var mu sync.Mutex
	calls := map[string]int{}
	s := &server{
		token: "secret",
		spawn: func(cwd, name, command, suffix string) (string, error) {
			mu.Lock()
			calls[name]++
			held := name == "held"
			mu.Unlock()
			if held {
				entered <- struct{}{}
				<-release
			}
			return "work-" + name, nil
		},
	}

	// Start a spawn and hold it open inside SpawnNew.
	go s.newSession(httptest.NewRecorder(), newSessionRequest(t,
		map[string]string{"cwd": home, "name": "held", "request_id": "task6-inflight-hold"}))
	<-entered

	// Push far past the cap with completed spawns while the first is in flight.
	for i := 0; i < spawnDedupeMax*2; i++ {
		s.newSession(httptest.NewRecorder(), newSessionRequest(t, map[string]string{
			"cwd": home, "name": fmt.Sprintf("filler-%d", i),
			"request_id": fmt.Sprintf("task6-inflight-%04d", i),
		}))
	}

	// A joiner on the held id must still join, not start a second spawn.
	joined := make(chan string, 1)
	go func() {
		rec := httptest.NewRecorder()
		s.newSession(rec, newSessionRequest(t,
			map[string]string{"cwd": home, "name": "held", "request_id": "task6-inflight-hold"}))
		joined <- rec.Body.String()
	}()
	time.Sleep(50 * time.Millisecond)
	close(release)

	body := <-joined
	mu.Lock()
	held := calls["held"]
	mu.Unlock()
	if held != 1 {
		t.Fatalf("held spawn ran %d times, want 1 — eviction dropped an in-flight entry", held)
	}
	var r actionResult
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatal(err)
	}
	if !r.OK || r.Tmux != "work-held" {
		t.Fatalf("joined result = %#v", r)
	}
}

// TestNewSessionDedupeDoesNotReplayAcrossDifferentRequests: the id is the whole
// key, so a caller reusing an id for a different directory gets the first
// result. That is the documented contract; this pins it so a future change to
// key on the body is a deliberate one.
func TestNewSessionSameRequestIDIgnoresBodyChanges(t *testing.T) {
	home := t.TempDir()
	other := t.TempDir()
	t.Setenv("HOME", home)
	s, calls := spawnRecorder(t, func(n int) (string, error) {
		return fmt.Sprintf("work-%06d", n), nil
	})

	id := "task6-same-id"
	s.newSession(httptest.NewRecorder(), newSessionRequest(t, map[string]string{"cwd": home, "request_id": id}))
	rec := httptest.NewRecorder()
	s.newSession(rec, newSessionRequest(t, map[string]string{"cwd": other, "request_id": id}))

	if got := calls(); got != 1 {
		t.Fatalf("spawn called %d times, want 1", got)
	}
	if r := decodeAction(t, rec); r.Tmux != "work-000001" {
		t.Fatalf("result = %#v, want the first spawn replayed", r)
	}
}

// TestSpawnSeamFallsBackToSpawnNew: a nil seam is production, matching the
// collect/terminate/removeTree pattern. Proved through the fake tmux on PATH
// rather than by really starting a session.
func TestSpawnSeamFallsBackToSpawnNew(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeCommandConfig(t, home)
	logPath := installFakeTmux(t)

	rec := httptest.NewRecorder()
	(&server{token: "secret"}).newSession(rec, newSessionRequest(t,
		map[string]string{"cwd": home, "request_id": "task6-realspawn"}))

	if r := decodeAction(t, rec); !r.OK || r.Tmux == "" {
		t.Fatalf("result = %#v", r)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "<new-session>") {
		t.Fatalf("tmux argv:\n%s", data)
	}
}

// --- TASK6 review: strict precondition decoding (finding D) -----------------

// TestKillPreconditionRejectsFailOpenBodies: every one of these decodes today
// to an empty session id, which the handler reads as "no precondition" and acts
// on unconditionally. The camelCase one is not hypothetical — `sessionId` is
// exactly the spelling the iOS Session model uses, so one typo in a client turns
// a guarded kill into a blind one.
func TestKillPreconditionRejectsFailOpenBodies(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"explicit null", `{"session_id":null}`},
		{"camelCase typo", `{"sessionId":"aaaa-1111"}`},
		{"unknown field", `{"session":"aaaa-1111"}`},
		{"unknown field alongside", `{"session_id":"aaaa-1111","oops":1}`},
		{"trailing object", `{"session_id":"aaaa-1111"}{"session_id":"bbbb"}`},
		{"trailing junk", `{"session_id":"aaaa-1111"}garbage`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			terminated := false
			collected := false
			s := &server{
				token:     "secret",
				collect:   func() ([]Session, error) { collected = true; return nil, nil },
				terminate: func(Session) error { terminated = true; return nil },
			}
			rec := httptest.NewRecorder()
			s.kill(rec, killRequest(t, 55, tt.body))

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %q)", rec.Code, rec.Body.String())
			}
			if collected || terminated {
				t.Fatalf("fail-open body acted (collected=%v terminated=%v)", collected, terminated)
			}
		})
	}
}

// TestMigratePreconditionRejectsFailOpenBodies mirrors it.
func TestMigratePreconditionRejectsFailOpenBodies(t *testing.T) {
	for _, body := range []string{`{"session_id":null}`, `{"sessionId":"a"}`, `{"session_id":"a"}junk`} {
		t.Run(body, func(t *testing.T) {
			collected := false
			s := &server{token: "secret", collect: func() ([]Session, error) { collected = true; return nil, nil }}
			rec := httptest.NewRecorder()
			s.migrate(rec, migrateRequest(t, 999999, body))

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
			if collected {
				t.Fatal("fail-open body reached the session list")
			}
		})
	}
}

// --- TASK6 review: re-attestation before the syscall (finding A) ------------

// attestingServer wires both seams: collect answers the precondition check,
// attest answers the re-read immediately before the destructive act.
func attestingServer(collected []Session, attested map[int]Session, terminated *[]Session) *server {
	return &server{
		token:   "secret",
		collect: func() ([]Session, error) { return collected, nil },
		attest: func(pid int) (Session, bool) {
			sess, ok := attested[pid]
			return sess, ok
		},
		terminate: func(target Session) error {
			*terminated = append(*terminated, target)
			return nil
		},
	}
}

// TestKillReattestsImmediatelyBeforeTerminating is the substitution race the
// precondition alone cannot catch: the check passes against a snapshot that
// CollectLocal spent real I/O building (git root, transcript scan, cost and
// agent scans per row), and by the time the handler reaches the syscall the PID
// belongs to someone else. The re-read closes everything except the syscall
// itself.
func TestKillReattestsImmediatelyBeforeTerminating(t *testing.T) {
	var terminated []Session
	s := attestingServer(
		[]Session{{PID: 55, SessionID: "aaaa-1111", Tmux: "work:1.0"}},
		// The world moved on while the snapshot was being enriched.
		map[int]Session{55: {PID: 55, SessionID: "bbbb-2222"}},
		&terminated)

	rec := httptest.NewRecorder()
	s.kill(rec, killRequest(t, 55, `{"session_id":"aaaa-1111"}`))

	if len(terminated) != 0 {
		t.Fatalf("terminated %#v after the target was substituted", terminated)
	}
	r := decodeAction(t, rec)
	if r.OK || r.Code != codeSessionMismatch {
		t.Fatalf("result = %#v, want a mismatch refusal", r)
	}
}

// TestKillReattestsAgainstADisappearedSession: the session file is gone by the
// time we are about to signal, so there is nothing left to attest to.
func TestKillReattestsAgainstADisappearedSession(t *testing.T) {
	var terminated []Session
	s := attestingServer(
		[]Session{{PID: 55, SessionID: "aaaa-1111"}},
		map[int]Session{}, // vanished between check and act
		&terminated)

	rec := httptest.NewRecorder()
	s.kill(rec, killRequest(t, 55, `{"session_id":"aaaa-1111"}`))

	if len(terminated) != 0 {
		t.Fatalf("terminated %#v after the session vanished", terminated)
	}
	if r := decodeAction(t, rec); r.OK || r.Code != codeNotLive {
		t.Fatalf("result = %#v, want not_live", r)
	}
}

// TestKillReattestationAgreeingStillTerminates: the happy path still kills, and
// still kills the fully-enriched server-collected row (tmux metadata included),
// not the bare row the re-read returns.
func TestKillReattestationAgreeingStillTerminates(t *testing.T) {
	var terminated []Session
	collected := Session{PID: 55, SessionID: "aaaa-1111", Tmux: "work:1.0"}
	s := attestingServer(
		[]Session{collected},
		map[int]Session{55: {PID: 55, SessionID: "aaaa-1111"}},
		&terminated)

	rec := httptest.NewRecorder()
	s.kill(rec, killRequest(t, 55, `{"session_id":"aaaa-1111"}`))

	if len(terminated) != 1 || terminated[0] != collected {
		t.Fatalf("terminated %#v, want the enriched row %#v", terminated, collected)
	}
	if r := decodeAction(t, rec); !r.OK {
		t.Fatalf("result = %#v", r)
	}
}

// TestKillWithoutPreconditionDoesNotReattest: there is nothing to attest
// against, so the unconditional path must not acquire a new way to fail.
func TestKillWithoutPreconditionDoesNotReattest(t *testing.T) {
	var terminated []Session
	attestCalls := 0
	s := &server{
		token:   "secret",
		collect: func() ([]Session, error) { return []Session{{PID: 55, SessionID: "aaaa-1111"}}, nil },
		attest: func(int) (Session, bool) {
			attestCalls++
			return Session{}, false
		},
		terminate: func(target Session) error { terminated = append(terminated, target); return nil },
	}
	rec := httptest.NewRecorder()
	s.kill(rec, killRequest(t, 55, "{}"))

	if attestCalls != 0 {
		t.Fatalf("attest called %d times without a precondition", attestCalls)
	}
	if len(terminated) != 1 {
		t.Fatalf("terminated %#v, want the unconditional kill to proceed", terminated)
	}
}

// --- TASK6 review: migrate threads the attested id (finding B) --------------

// TestMigrateLocalRefusesWhenTheRereadDisagrees: MigrateLocal re-reads the PID's
// session file and used to adopt whatever it said. A substitution between the
// handler's check and that re-read migrated the wrong session — it would kill
// the new occupant and resume the new occupant's transcript.
func TestMigrateLocalRefusesWhenTheRereadDisagrees(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	logPath := installFakeTmux(t)
	writeMigrateSession(t, home, 999999, "bbbb-2222")

	_, err := MigrateLocalAttested(999999, "aaaa-1111")

	if err == nil {
		t.Fatal("migrated a session the re-read said was somebody else")
	}
	if !errors.Is(err, errMigrateSessionMismatch) {
		t.Fatalf("err = %v, want errMigrateSessionMismatch", err)
	}
	if _, statErr := os.Stat(logPath); statErr == nil {
		data, _ := os.ReadFile(logPath)
		t.Fatalf("refused migrate still invoked tmux:\n%s", data)
	}
}

// TestMigrateLocalProceedsWhenTheRereadAgrees, and an empty attestation keeps
// the pre-existing unconditional behaviour the TUI relies on.
func TestMigrateLocalAttestationAgreementCases(t *testing.T) {
	for _, tt := range []struct{ name, want string }{
		{"matching id", "aaaa-1111"},
		{"no attestation", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			logPath := installFakeTmux(t)
			writeMigrateSession(t, home, 999999, "aaaa-1111")

			tname, err := MigrateLocalAttested(999999, tt.want)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if tname == "" {
				t.Fatal("no tmux name")
			}
			data, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), "<new-session>") {
				t.Fatalf("tmux argv:\n%s", data)
			}
		})
	}
}

// TestMigrateLocalFailsWhenTheProcessSurvives: the kill step discarded its
// errors and never confirmed the process had actually gone before creating the
// new tmux session. Two live processes on the same transcript is worse than a
// failed migrate.
func TestMigrateLocalFailsWhenTheProcessSurvives(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	logPath := installFakeTmux(t)
	writeMigrateSession(t, home, 999999, "aaaa-1111")

	restore := stubMigrateKill(t, func(int, syscall.Signal) error { return nil }, func(int) bool { return true })
	defer restore()

	_, err := MigrateLocalAttested(999999, "aaaa-1111")

	if err == nil {
		t.Fatal("migrated while the old process was still running")
	}
	if !strings.Contains(err.Error(), "still running") {
		t.Fatalf("err = %v, want a liveness failure", err)
	}
	if _, statErr := os.Stat(logPath); statErr == nil {
		data, _ := os.ReadFile(logPath)
		t.Fatalf("migrate created a tmux session anyway:\n%s", data)
	}
}

// --- TASK6 review: spawn partial commit and joiner safety (finding C) -------

// TestSpawnNewCleansUpWhenSendKeysFails: SpawnNew creates the tmux session and
// only then sends the command. A send-keys failure used to leave the session
// behind while reporting failure — and because failures are deliberately not
// remembered, the retry then made a second one. Cleanup is what makes "failures
// are not remembered" safe.
func TestSpawnNewCleansUpWhenSendKeysFails(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	logPath := installFakeTmuxFailing(t, "send-keys")

	_, err := SpawnNew(home, "test", "claude")

	if err == nil {
		t.Fatal("expected send-keys to fail")
	}
	data, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(data), "<kill-session>") {
		t.Fatalf("orphaned tmux session left behind; argv:\n%s", data)
	}
}

// TestNewSessionPanicDoesNotStrandJoiners: a joiner blocks on the flight's done
// channel. If the owner panics without publishing, every joiner blocks forever
// and those connections never return.
func TestNewSessionPanicDoesNotStrandJoiners(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	s := &server{
		token: "secret",
		spawn: func(string, string, string, string) (string, error) {
			entered <- struct{}{}
			// Held open so the joiner below is provably parked on the flight
			// before the owner dies — otherwise the owner panics first, the
			// ledger forgets the failure, and the "joiner" is really just the
			// next owner.
			<-release
			panic("tmux exploded")
		},
	}
	body := map[string]string{"cwd": home, "request_id": "task6-panic-01"}

	go func() {
		defer func() { _ = recover() }()
		s.newSession(httptest.NewRecorder(), newSessionRequest(t, body))
	}()
	<-entered

	joined := make(chan string, 1)
	go func() {
		defer func() { _ = recover() }()
		rec := httptest.NewRecorder()
		s.newSession(rec, newSessionRequest(t, body))
		joined <- rec.Body.String()
	}()
	time.Sleep(50 * time.Millisecond)
	close(release)

	select {
	case got := <-joined:
		var r actionResult
		if err := json.Unmarshal([]byte(got), &r); err != nil {
			t.Fatal(err)
		}
		if r.OK {
			t.Fatalf("a panicked spawn reported success: %s", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("joiner stranded by a panicking spawn")
	}
}

// TestNewSessionJoinerGivesUpWithItsRequest: a hung spawn must not hold every
// joiner's connection open for as long as it hangs.
func TestNewSessionJoinerReleasesOnClientDisconnect(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	release := make(chan struct{})
	defer close(release)
	entered := make(chan struct{}, 1)
	s := &server{
		token: "secret",
		spawn: func(string, string, string, string) (string, error) {
			entered <- struct{}{}
			<-release
			return "work-abc123", nil
		},
	}
	body := map[string]string{"cwd": home, "request_id": "task6-hang-01"}
	go s.newSession(httptest.NewRecorder(), newSessionRequest(t, body))
	<-entered

	ctx, cancel := context.WithCancel(context.Background())
	returned := make(chan struct{})
	go func() {
		s.newSession(httptest.NewRecorder(), newSessionRequest(t, body).WithContext(ctx))
		close(returned)
	}()
	cancel()

	select {
	case <-returned:
	case <-time.After(3 * time.Second):
		t.Fatal("joiner ignored its own request being cancelled")
	}
}

// --- TASK6 review: the cap cannot be honoured by eviction (finding E) -------

// TestNewSessionRefusesBeyondTheInflightCap: with every slot held by a running
// spawn there is nothing safe to evict — dropping an in-flight entry would turn
// its joiners into duplicate spawns, which is the whole point of the ledger. So
// a new id is refused with a retryable 503 rather than admitted past the bound.
func TestNewSessionRefusesBeyondTheInflightCap(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	release := make(chan struct{})
	entered := make(chan struct{}, spawnDedupeMax)
	var mu sync.Mutex
	calls := 0
	s := &server{
		token: "secret",
		spawn: func(string, string, string, string) (string, error) {
			mu.Lock()
			calls++
			mu.Unlock()
			entered <- struct{}{}
			<-release
			return "work-abc123", nil
		},
	}

	var wg sync.WaitGroup
	for i := 0; i < spawnDedupeMax; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s.newSession(httptest.NewRecorder(), newSessionRequest(t, map[string]string{
				"cwd": home, "request_id": fmt.Sprintf("task6-inflight-cap-%04d", i)}))
		}(i)
	}
	for i := 0; i < spawnDedupeMax; i++ {
		<-entered
	}

	// The 33rd distinct id arrives with every slot occupied.
	rec := httptest.NewRecorder()
	s.newSession(rec, newSessionRequest(t, map[string]string{
		"cwd": home, "request_id": "task6-inflight-cap-overflow"}))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body %q)", rec.Code, rec.Body.String())
	}
	if n := s.spawns.len(); n > spawnDedupeMax {
		t.Fatalf("dedupe grew to %d past the cap of %d", n, spawnDedupeMax)
	}
	if got := decodeAction(t, rec); got.OK || got.Error == "" {
		t.Fatalf("refusal = %#v, want a retryable message", got)
	}

	close(release)
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	// The refusal must not have started a spawn, and none of the 32 held slots
	// may have been evicted into a second one.
	if calls != spawnDedupeMax {
		t.Fatalf("spawn called %d times, want %d", calls, spawnDedupeMax)
	}
}

// TestInflightEntriesAreNeverEvictedForCapacity: the refusal above is only
// correct if eviction genuinely declines to touch a running spawn.
func TestInflightEntriesAreNeverEvictedForCapacity(t *testing.T) {
	d := &spawnDedupe{now: func() time.Time { return time.Unix(1700000000, 0) }}
	for i := 0; i < spawnDedupeMax; i++ {
		if _, claim := d.begin(fmt.Sprintf("task6-held-%04d", i)); claim != spawnClaimed {
			t.Fatalf("id %d: claim = %v, want claimed", i, claim)
		}
	}
	if _, claim := d.begin("task6-held-overflow"); claim != spawnClaimRefused {
		t.Fatalf("claim = %v, want refused at the cap", claim)
	}
	if n := d.len(); n != spawnDedupeMax {
		t.Fatalf("len = %d, want exactly the cap", n)
	}
}

// --- helpers for the review tests ------------------------------------------

// writeMigrateSession plants the ~/.claude/sessions/<pid>.json MigrateLocal's
// own re-read looks for, on top of resume_test.go's writeSessionFile.
func writeMigrateSession(t *testing.T, home string, pid int, sessionID string) {
	t.Helper()
	writeSessionFile(t, home, strconv.Itoa(pid), fmt.Sprintf(
		`{"pid":%d,"sessionId":%q,"cwd":%q,"status":"busy","waitingFor":"","entrypoint":"cli"}`,
		pid, sessionID, home))
}

// installFakeTmuxFailing is installFakeTmux with one subcommand rigged to fail,
// so a partial commit (session created, command never sent) can be reproduced
// without breaking a real tmux.
func installFakeTmuxFailing(t *testing.T, failOn string) string {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "tmux.log")
	script := filepath.Join(dir, "tmux")
	body := "#!/bin/sh\nfor arg in \"$@\"; do printf '<%s>' \"$arg\"; done >> \"$TMUX_LOG\"\n" +
		"printf '\\n' >> \"$TMUX_LOG\"\n" +
		"[ \"$1\" = \"" + failOn + "\" ] && exit 1\nexit 0\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TMUX_LOG", logPath)
	return logPath
}

// stubMigrateKill swaps MigrateLocal's signalling side effects, and makes its
// waits instant so the liveness path costs no wall clock.
func stubMigrateKill(t *testing.T, signal func(int, syscall.Signal) error, alive func(int) bool) func() {
	t.Helper()
	oldSignal, oldAlive, oldSleep := migrateSignal, migrateAlive, migrateSleep
	migrateSignal, migrateAlive, migrateSleep = signal, alive, func(time.Duration) {}
	return func() { migrateSignal, migrateAlive, migrateSleep = oldSignal, oldAlive, oldSleep }
}

// TestMigrateRereadMismatchCarriesTheCode: the substitution can be caught by
// either of migrate's two identity checks, and a client must not have to tell
// them apart by matching on prose.
func TestMigrateRereadMismatchCarriesTheCode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	installFakeTmux(t)
	writeMigrateSession(t, home, 999999, "bbbb-2222")
	s := &server{
		token: "secret",
		// The first check agrees: this is the stale snapshot the handler built.
		collect: func() ([]Session, error) {
			return []Session{{PID: 999999, SessionID: "aaaa-1111"}}, nil
		},
	}
	rec := httptest.NewRecorder()
	s.migrate(rec, migrateRequest(t, 999999, `{"session_id":"aaaa-1111"}`))

	r := decodeAction(t, rec)
	if r.OK || r.Code != codeSessionMismatch {
		t.Fatalf("result = %#v, want the mismatch code from the re-read", r)
	}
}

// --- TASK6 final round --------------------------------------------------

// TestKillPreconditionRejectsABareNullBody: decoding a top-level `null` into a
// struct is a silent no-op in Go, so the precondition came back empty and the
// handler read that as "no precondition" — a bare `null` disarmed the guard
// exactly like the other fail-open forms did.
func TestKillPreconditionRejectsABareNullBody(t *testing.T) {
	for _, body := range []string{"null", " null ", "{\"session_id\":\"a\"}]", "{}]", "{} {}"} {
		t.Run(body, func(t *testing.T) {
			terminated := false
			collected := false
			s := &server{
				token:     "secret",
				collect:   func() ([]Session, error) { collected = true; return nil, nil },
				terminate: func(Session) error { terminated = true; return nil },
			}
			rec := httptest.NewRecorder()
			s.kill(rec, killRequest(t, 55, body))

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %q)", rec.Code, rec.Body.String())
			}
			if collected || terminated {
				t.Fatalf("body %q acted (collected=%v terminated=%v)", body, collected, terminated)
			}
		})
	}
}

// TestPidPresentTreatsEPERMAsAlive: kill(pid, 0) answering EPERM means the
// process exists and we may not signal it — which is precisely when proceeding
// to create a second session on the same transcript is wrong. Reading it as
// "gone" is the dangerous direction.
func TestPidPresentTreatsEPERMAsAlive(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"no error means alive", nil, true},
		{"EPERM means alive but unsignalable", syscall.EPERM, true},
		{"ESRCH means genuinely gone", syscall.ESRCH, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pidPresentErr(tt.err); got != tt.want {
				t.Fatalf("pidPresentErr(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestMigrateLocalFailsWhenTheProcessIsOnlyUnsignalable: the end-to-end version
// of the above through MigrateLocal's own liveness gate.
func TestMigrateLocalFailsWhenTheProcessIsOnlyUnsignalable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	logPath := installFakeTmux(t)
	writeMigrateSession(t, home, 999999, "aaaa-1111")

	restore := stubMigrateKill(t,
		func(int, syscall.Signal) error { return syscall.EPERM },
		func(pid int) bool { return pidPresentErr(syscall.EPERM) })
	defer restore()

	if _, err := MigrateLocalAttested(999999, "aaaa-1111"); err == nil {
		t.Fatal("migrated a process we could not confirm dead")
	}
	if _, statErr := os.Stat(logPath); statErr == nil {
		data, _ := os.ReadFile(logPath)
		t.Fatalf("created a tmux session anyway:\n%s", data)
	}
}

// TestSpawnTmuxNameIsDerivedFromTheRequestID: cleanup after a partial spawn can
// itself fail, and the ledger forgets failures. A random tmux name would let the
// retry create a duplicate beside the orphan; a name derived from the request id
// collides with it instead, so the retry errors rather than doubling up.
func TestSpawnTmuxNameIsDerivedFromTheRequestID(t *testing.T) {
	a := spawnSuffix("task6-verify-001")
	if a == "" || len(a) != 6 {
		t.Fatalf("suffix = %q, want 6 chars", a)
	}
	if b := spawnSuffix("task6-verify-001"); b != a {
		t.Fatalf("same request_id gave %q then %q", a, b)
	}
	if c := spawnSuffix("task6-verify-002"); c == a {
		t.Fatalf("different request_ids collided on %q", a)
	}
	// No request id means no idempotency, so the name must stay random.
	if spawnSuffix("") != "" {
		t.Fatalf("empty request_id produced a fixed suffix")
	}
}

// TestNewSessionRetryAfterFailedCleanupCollidesRatherThanDuplicating: the whole
// point of the derived name. The first attempt half-creates a session and its
// cleanup fails; the retry must hit the surviving session and error, not build a
// second one alongside it.
func TestNewSessionRetryAfterFailedCleanupCollidesRatherThanDuplicating(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var names []string
	s := &server{
		token: "secret",
		spawn: func(cwd, name, command, suffix string) (string, error) {
			tname := MakeTmuxName(cwd, suffix, name)
			names = append(names, tname)
			return "", fmt.Errorf("tmux new-session: duplicate session: %s", tname)
		},
	}
	body := map[string]string{"cwd": home, "request_id": "task6-collide-01"}
	s.newSession(httptest.NewRecorder(), newSessionRequest(t, body))
	s.newSession(httptest.NewRecorder(), newSessionRequest(t, body))

	if len(names) != 2 {
		t.Fatalf("spawn ran %d times, want 2 (failures are not remembered)", len(names))
	}
	if names[0] != names[1] {
		t.Fatalf("retry used a different tmux name: %q then %q", names[0], names[1])
	}
}

// blockingWriter is an http.ResponseWriter whose Write parks until released, so
// a client that is slow to read can be reproduced.
type blockingWriter struct {
	header  http.Header
	release chan struct{}
	entered chan struct{}
}

func newBlockingWriter() *blockingWriter {
	return &blockingWriter{
		header:  http.Header{},
		release: make(chan struct{}),
		entered: make(chan struct{}, 1),
	}
}

func (b *blockingWriter) Header() http.Header { return b.header }
func (b *blockingWriter) WriteHeader(int)     {}
func (b *blockingWriter) Write(p []byte) (int, error) {
	select {
	case b.entered <- struct{}{}:
	default:
	}
	<-b.release
	return len(p), nil
}

// TestCompletedSpawnFreesItsSlotBeforeTheResponseIsWritten: publishing after
// writeJSON means a slow client holds a finished spawn's slot open, and a later
// request is refused for capacity that is not actually in use.
func TestCompletedSpawnFreesItsSlotBeforeTheResponseIsWritten(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	held := make(chan struct{})
	defer close(held)
	entered := make(chan struct{}, spawnDedupeMax)
	s := &server{
		token: "secret",
		spawn: func(cwd, name, command, suffix string) (string, error) {
			if name == "slow-client" {
				return "work-slow", nil // completes immediately
			}
			entered <- struct{}{}
			<-held
			return "work-held", nil
		},
	}

	// One spawn that has finished but whose response is still being written.
	slow := newBlockingWriter()
	go s.newSession(slow, newSessionRequest(t, map[string]string{
		"cwd": home, "name": "slow-client", "request_id": "task6-slowclient-01"}))
	<-slow.entered
	defer close(slow.release)

	// Fill every remaining slot with genuinely running spawns.
	for i := 0; i < spawnDedupeMax-1; i++ {
		go s.newSession(httptest.NewRecorder(), newSessionRequest(t, map[string]string{
			"cwd": home, "request_id": fmt.Sprintf("task6-slowfill-%04d", i)}))
	}
	for i := 0; i < spawnDedupeMax-1; i++ {
		<-entered
	}

	// The completed-but-unwritten entry is not occupying capacity, so this fits.
	rec := httptest.NewRecorder()
	s.newSession(rec, newSessionRequest(t, map[string]string{
		"cwd": home, "name": "slow-client", "request_id": "task6-slowclient-new"}))

	if rec.Code == http.StatusServiceUnavailable {
		t.Fatal("refused for capacity held by a spawn that had already finished")
	}
	if r := decodeAction(t, rec); !r.OK {
		t.Fatalf("result = %#v", r)
	}
}

func TestDisableHandlerSetsDisabledState(t *testing.T) {
	dir := t.TempDir()
	flags := newFlagsStore(filepath.Join(dir, flagsFileName), fixedClock(time.Now()), noResolver)
	s := &server{
		token: "secret",
		flags: flags,
		collect: func() ([]Session, error) {
			return []Session{{PID: 55, SessionID: "sess-55"}}, nil
		},
	}
	body := strings.NewReader(`{"session_id":"sess-55","disabled":true}`)
	req := httptest.NewRequest("POST", "/sessions/55/disable", body)
	req.SetPathValue("pid", "55")
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()

	s.disableSession(rec, req)

	var r actionResult
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !r.OK || r.SessionID != "sess-55" || r.Disabled == nil || !*r.Disabled {
		t.Fatalf("response = %#v, want OK with sess-55 disabled", r)
	}
	if !flags.Disabled("sess-55") {
		t.Fatal("store not updated")
	}
}

func TestDisableHandlerSessionMismatchRefusesWithoutMutating(t *testing.T) {
	dir := t.TempDir()
	flags := newFlagsStore(filepath.Join(dir, flagsFileName), fixedClock(time.Now()), noResolver)
	s := &server{
		token: "secret",
		flags: flags,
		collect: func() ([]Session, error) {
			return []Session{{PID: 55, SessionID: "new-session"}}, nil
		},
	}
	body := strings.NewReader(`{"session_id":"stale-session","disabled":true}`)
	req := httptest.NewRequest("POST", "/sessions/55/disable", body)
	req.SetPathValue("pid", "55")
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()

	s.disableSession(rec, req)

	var r actionResult
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if r.OK || r.Code != codeSessionMismatch {
		t.Fatalf("response = %#v, want session_mismatch refusal", r)
	}
	if flags.Disabled("new-session") || flags.Disabled("stale-session") {
		t.Fatal("store mutated on a refused request")
	}
}

func TestDisableHandlerNotLiveRefusesWithoutMutating(t *testing.T) {
	dir := t.TempDir()
	flags := newFlagsStore(filepath.Join(dir, flagsFileName), fixedClock(time.Now()), noResolver)
	s := &server{
		token:   "secret",
		flags:   flags,
		collect: func() ([]Session, error) { return nil, nil },
	}
	body := strings.NewReader(`{"session_id":"ghost","disabled":true}`)
	req := httptest.NewRequest("POST", "/sessions/55/disable", body)
	req.SetPathValue("pid", "55")
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()

	s.disableSession(rec, req)

	var r actionResult
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if r.OK || r.Code != codeNotLive {
		t.Fatalf("response = %#v, want not_live refusal", r)
	}
}

func TestDisableHandlerRejectsMalformedBody(t *testing.T) {
	s := &server{token: "secret", collect: func() ([]Session, error) { return nil, nil }}
	cases := []struct {
		name string
		body string
	}{
		{"missing session_id", `{"disabled":true}`},
		{"missing disabled", `{"session_id":"x"}`},
		{"null session_id", `{"session_id":null,"disabled":true}`},
		{"empty session_id", `{"session_id":"","disabled":true}`},
		{"unknown field", `{"session_id":"x","disabled":true,"extra":1}`},
		{"trailing content", `{"session_id":"x","disabled":true}{}`},
		{"disabled not a bool", `{"session_id":"x","disabled":"yes"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/sessions/55/disable", strings.NewReader(tc.body))
			req.SetPathValue("pid", "55")
			req.Header.Set("Authorization", "Bearer secret")
			rec := httptest.NewRecorder()

			s.disableSession(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
			}
		})
	}
}

func TestDisableHandlerUnauthorized(t *testing.T) {
	s := &server{token: "secret"}
	req := httptest.NewRequest("POST", "/sessions/55/disable", strings.NewReader(`{"session_id":"x","disabled":true}`))
	req.SetPathValue("pid", "55")
	rec := httptest.NewRecorder()

	s.disableSession(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

// TestAccountSwitchHandler covers POST /account/switch's four outcomes. The
// switch itself is injected: without that seam this test would perform a real
// account switch against the machine running `go test`.
func TestAccountSwitchHandler(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		switchErr  error
		warnings   []string
		wantStatus int
		wantOK     bool
		wantCode   string
		wantCalled bool
	}{
		{
			name:       "valid name switches and reports the new email",
			body:       `{"name":"avisoma"}`,
			wantStatus: http.StatusOK,
			wantOK:     true,
			wantCalled: true,
		},
		{
			// A successful switch that raised a warning carries it on the
			// success response — refusing would block the very case it warns
			// about (see runningSessionsWarning).
			name:       "warnings ride the success response",
			body:       `{"name":"avisoma"}`,
			warnings:   []string{"2 Claude Code sessions are still running (pid 11, 12)."},
			wantStatus: http.StatusOK,
			wantOK:     true,
			wantCalled: true,
		},
		{
			name:       "unknown snapshot is a bad request, nothing touched",
			body:       `{"name":"nope"}`,
			switchErr:  fmt.Errorf("%w for %q (known: avisoma)", errUnknownAccount, "nope"),
			wantStatus: http.StatusBadRequest,
			wantCode:   codeUnknownAccount,
			wantCalled: true,
		},
		{
			name:       "a failed switch is a server error",
			body:       `{"name":"avisoma"}`,
			switchErr:  errors.New("keychain write: exit status 1"),
			wantStatus: http.StatusInternalServerError,
			wantCode:   codeSwitchFailed,
			wantCalled: true,
		},
		{
			name:       "an empty name never reaches the switch",
			body:       `{}`,
			wantStatus: http.StatusBadRequest,
			wantCode:   codeUnknownAccount,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			s := &server{
				token: "secret",
				switchAcct: func(name string) (string, []string, error) {
					called = true
					if tc.switchErr != nil {
						return "", nil, tc.switchErr
					}
					return "andy@" + name + ".example", tc.warnings, nil
				},
			}
			req := httptest.NewRequest("POST", "/account/switch", strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer secret")
			rec := httptest.NewRecorder()

			s.accountSwitch(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.wantStatus, rec.Body)
			}
			if called != tc.wantCalled {
				t.Fatalf("switch called = %v, want %v", called, tc.wantCalled)
			}
			var r accountSwitchResult
			if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if r.OK != tc.wantOK {
				t.Fatalf("ok = %v, want %v", r.OK, tc.wantOK)
			}
			if r.Code != tc.wantCode {
				t.Fatalf("code = %q, want %q", r.Code, tc.wantCode)
			}
			if tc.wantOK && r.Account != "andy@avisoma.example" {
				t.Fatalf("account = %q, want the new live email", r.Account)
			}
			if !tc.wantOK && r.Message == "" {
				t.Fatal("failure carries no message")
			}
			if !slices.Equal(r.Warnings, tc.warnings) {
				t.Fatalf("warnings = %v, want %v", r.Warnings, tc.warnings)
			}
		})
	}
}

func TestAccountSwitchHandlerUnauthorized(t *testing.T) {
	called := false
	s := &server{
		token:      "secret",
		switchAcct: func(string) (string, []string, error) { called = true; return "", nil, nil },
	}
	req := httptest.NewRequest("POST", "/account/switch", strings.NewReader(`{"name":"avisoma"}`))
	rec := httptest.NewRecorder()

	s.accountSwitch(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	if called {
		t.Fatal("an unauthorized request reached the switch")
	}
}

func TestAccountSwitchHandlerBadJSON(t *testing.T) {
	s := &server{
		token:      "secret",
		switchAcct: func(string) (string, []string, error) { t.Fatal("switch called for a bad body"); return "", nil, nil },
	}
	req := httptest.NewRequest("POST", "/account/switch", strings.NewReader(`{"name":`))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()

	s.accountSwitch(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestTranscriptTailHandler(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".claude", "projects", "proj1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	sid := "sess-remote-1"
	if err := os.WriteFile(filepath.Join(dir, sid+".jsonl"),
		[]byte(`{"type":"user","message":{"role":"user","content":"remote hi"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	s := &server{token: "test-token"}
	req := httptest.NewRequest("GET", "/transcript-tail?session_id="+sid+"&n=5", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	s.transcriptTail(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body transcriptTailResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Turns) != 1 || body.Turns[0].Text != "remote hi" {
		t.Errorf("Turns = %+v", body.Turns)
	}
	if body.Size == 0 {
		t.Error("Size should be non-zero")
	}
}

func TestTranscriptTailHandlerBadSessionID(t *testing.T) {
	s := &server{token: "test-token"}
	req := httptest.NewRequest("GET", "/transcript-tail?session_id=../../etc/passwd&n=5", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	s.transcriptTail(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestTranscriptTailHandlerNClamped(t *testing.T) {
	s := &server{token: "test-token"}
	req := httptest.NewRequest("GET", "/transcript-tail?session_id=abc&n=999", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	s.transcriptTail(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for out-of-range n", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "bad n value") {
		t.Errorf("body = %q, want it to contain %q", rec.Body.String(), "bad n value")
	}
}

func TestTranscriptTailHandlerNotFound(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	s := &server{token: "test-token"}
	req := httptest.NewRequest("GET", "/transcript-tail?session_id=nope&n=5", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	s.transcriptTail(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestTranscriptTailHandlerUnauthorized(t *testing.T) {
	s := &server{token: "test-token"}
	req := httptest.NewRequest("GET", "/transcript-tail?session_id=abc&n=5", nil)
	rec := httptest.NewRecorder()
	s.transcriptTail(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestSendKeysHandlerRequiresSessionID(t *testing.T) {
	s := &server{token: "secret"}
	req := httptest.NewRequest("POST", "/sessions/42/send-keys", strings.NewReader(`{"text":"hello"}`))
	req.Header.Set("Authorization", "Bearer secret")
	req.SetPathValue("pid", "42")
	w := httptest.NewRecorder()

	s.sendKeysHandler(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestSendKeysHandlerRequiresText(t *testing.T) {
	s := &server{token: "secret"}
	req := httptest.NewRequest("POST", "/sessions/42/send-keys", strings.NewReader(`{"session_id":"sess-1"}`))
	req.Header.Set("Authorization", "Bearer secret")
	req.SetPathValue("pid", "42")
	w := httptest.NewRecorder()

	s.sendKeysHandler(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestSendKeysHandlerMismatchedSessionRefuses(t *testing.T) {
	s := &server{
		token: "secret",
		collect: func() ([]Session, error) {
			return []Session{{PID: 42, SessionID: "sess-current", Tmux: "work:0.0"}}, nil
		},
	}
	req := httptest.NewRequest("POST", "/sessions/42/send-keys", strings.NewReader(`{"session_id":"sess-stale","text":"hello"}`))
	req.Header.Set("Authorization", "Bearer secret")
	req.SetPathValue("pid", "42")
	w := httptest.NewRecorder()

	s.sendKeysHandler(w, req)

	var r actionResult
	if err := json.NewDecoder(w.Body).Decode(&r); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if r.OK || r.Code != codeSessionMismatch {
		t.Fatalf("result = %+v, want refusal with codeSessionMismatch", r)
	}
}

func TestSendKeysHandlerSuccessCallsSendKeysFn(t *testing.T) {
	var gotSess Session
	var gotText string
	s := &server{
		token: "secret",
		collect: func() ([]Session, error) {
			return []Session{{PID: 42, SessionID: "sess-1", Tmux: "work:0.0"}}, nil
		},
		sendKeysFn: func(sess Session, text string) error {
			gotSess, gotText = sess, text
			return nil
		},
	}
	req := httptest.NewRequest("POST", "/sessions/42/send-keys", strings.NewReader(`{"session_id":"sess-1","text":"hello"}`))
	req.Header.Set("Authorization", "Bearer secret")
	req.SetPathValue("pid", "42")
	w := httptest.NewRecorder()

	s.sendKeysHandler(w, req)

	var r actionResult
	if err := json.NewDecoder(w.Body).Decode(&r); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !r.OK {
		t.Fatalf("result = %+v, want ok", r)
	}
	if gotSess.PID != 42 || gotText != "hello" {
		t.Fatalf("sendKeysFn called with (%+v, %q), want (PID 42, \"hello\")", gotSess, gotText)
	}
}

func TestSendKeysHandlerUnauthorized(t *testing.T) {
	s := &server{token: "secret"}
	req := httptest.NewRequest("POST", "/sessions/42/send-keys", strings.NewReader(`{"session_id":"sess-1","text":"hello"}`))
	req.SetPathValue("pid", "42")
	w := httptest.NewRecorder()

	s.sendKeysHandler(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestSendKeysHandlerRejectsControlCharacters(t *testing.T) {
	s := &server{token: "secret"}
	cases := []struct {
		name string
		body string
	}{
		{"newline", `{"session_id":"sess-1","text":"hello\nworld"}`},
		{"carriage return", `{"session_id":"sess-1","text":"hello\rworld"}`},
		{"nul byte", `{"session_id":"sess-1","text":"hello\u0000world"}`},
		{"escape byte", `{"session_id":"sess-1","text":"hello\u001bworld"}`},
		{"delete byte", `{"session_id":"sess-1","text":"hello\u007fworld"}`},
		{"tab byte passes JSON but not the C0 filter", `{"session_id":"sess-1","text":"hello\tworld"}`},
		{"unknown field", `{"session_id":"sess-1","text":"hi","extra":1}`},
		{"trailing content", `{"session_id":"sess-1","text":"hi"}{}`},
		{"empty text", `{"session_id":"sess-1","text":""}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/sessions/42/send-keys", strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer secret")
			req.SetPathValue("pid", "42")
			w := httptest.NewRecorder()

			s.sendKeysHandler(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", w.Code)
			}
		})
	}
}

func TestSendKeysHandlerNotLiveRefuses(t *testing.T) {
	s := &server{
		token:   "secret",
		collect: func() ([]Session, error) { return nil, nil },
	}
	req := httptest.NewRequest("POST", "/sessions/42/send-keys", strings.NewReader(`{"session_id":"sess-1","text":"hello"}`))
	req.Header.Set("Authorization", "Bearer secret")
	req.SetPathValue("pid", "42")
	w := httptest.NewRecorder()

	s.sendKeysHandler(w, req)

	var r actionResult
	if err := json.NewDecoder(w.Body).Decode(&r); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if r.OK || r.Code != codeNotLive {
		t.Fatalf("result = %+v, want refusal with codeNotLive", r)
	}
}

func TestSendKeysHandlerRejectsOverLengthText(t *testing.T) {
	s := &server{token: "secret"}
	longText := strings.Repeat("a", sendKeysMaxLen+1)
	bodyBytes, err := json.Marshal(struct {
		SessionID string `json:"session_id"`
		Text      string `json:"text"`
	}{SessionID: "sess-1", Text: longText})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest("POST", "/sessions/42/send-keys", strings.NewReader(string(bodyBytes)))
	req.Header.Set("Authorization", "Bearer secret")
	req.SetPathValue("pid", "42")
	w := httptest.NewRecorder()

	s.sendKeysHandler(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for text exceeding sendKeysMaxLen", w.Code)
	}
}

func TestResizeBodyRequiresSessionID(t *testing.T) {
	req := httptest.NewRequest("POST", "/sessions/42/resize", strings.NewReader(`{"cols":120,"rows":40}`))
	w := httptest.NewRecorder()
	_, _, _, _, err := resizeBody(w, req)
	if err == nil || !strings.Contains(err.Error(), "session_id is required") {
		t.Fatalf("resizeBody err = %v, want session_id-required error", err)
	}
}

func TestResizeBodyRequiresPositiveColsRowsUnlessRevert(t *testing.T) {
	cases := []string{
		`{"session_id":"sess-1","cols":0,"rows":40}`,
		`{"session_id":"sess-1","cols":120,"rows":0}`,
		`{"session_id":"sess-1","cols":-1,"rows":40}`,
	}
	for _, body := range cases {
		req := httptest.NewRequest("POST", "/sessions/42/resize", strings.NewReader(body))
		w := httptest.NewRecorder()
		_, _, _, _, err := resizeBody(w, req)
		if err == nil || !strings.Contains(err.Error(), "cols and rows must be positive") {
			t.Fatalf("resizeBody(%s) err = %v, want cols/rows-positive error", body, err)
		}
	}
}

func TestResizeBodyRevertIgnoresColsRows(t *testing.T) {
	req := httptest.NewRequest("POST", "/sessions/42/resize", strings.NewReader(`{"session_id":"sess-1","revert":true}`))
	w := httptest.NewRecorder()
	sessionID, _, _, revert, err := resizeBody(w, req)
	if err != nil {
		t.Fatalf("resizeBody err = %v, want nil", err)
	}
	if sessionID != "sess-1" || !revert {
		t.Fatalf("resizeBody = (%q, revert=%v), want (sess-1, true)", sessionID, revert)
	}
}

func TestResizeBodyValidResizeSucceeds(t *testing.T) {
	req := httptest.NewRequest("POST", "/sessions/42/resize", strings.NewReader(`{"session_id":"sess-1","cols":120,"rows":40}`))
	w := httptest.NewRecorder()
	sessionID, cols, rows, revert, err := resizeBody(w, req)
	if err != nil {
		t.Fatalf("resizeBody err = %v, want nil", err)
	}
	if sessionID != "sess-1" || cols != 120 || rows != 40 || revert {
		t.Fatalf("resizeBody = (%q, %d, %d, revert=%v), want (sess-1, 120, 40, false)", sessionID, cols, rows, revert)
	}
}

func TestResizeBodyRejectsUnknownFieldsAndTrailingContent(t *testing.T) {
	cases := []string{
		`{"session_id":"sess-1","cols":120,"rows":40,"extra":1}`,
		`{"session_id":"sess-1","cols":120,"rows":40}{}`,
	}
	for _, body := range cases {
		req := httptest.NewRequest("POST", "/sessions/42/resize", strings.NewReader(body))
		w := httptest.NewRecorder()
		_, _, _, _, err := resizeBody(w, req)
		if err == nil {
			t.Fatalf("resizeBody(%s) err = nil, want error", body)
		}
	}
}

func TestResizeHandlerRequiresSessionID(t *testing.T) {
	s := &server{token: "secret"}
	req := httptest.NewRequest("POST", "/sessions/42/resize", strings.NewReader(`{"cols":120,"rows":40}`))
	req.Header.Set("Authorization", "Bearer secret")
	req.SetPathValue("pid", "42")
	w := httptest.NewRecorder()

	s.resizeHandler(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestResizeHandlerRequiresPositiveColsRowsUnlessRevert(t *testing.T) {
	s := &server{token: "secret"}
	req := httptest.NewRequest("POST", "/sessions/42/resize", strings.NewReader(`{"session_id":"sess-1","cols":0,"rows":40}`))
	req.Header.Set("Authorization", "Bearer secret")
	req.SetPathValue("pid", "42")
	w := httptest.NewRecorder()

	s.resizeHandler(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestResizeHandlerMismatchedSessionRefuses(t *testing.T) {
	s := &server{
		token: "secret",
		collect: func() ([]Session, error) {
			return []Session{{PID: 42, SessionID: "sess-current", Tmux: "work:0.0"}}, nil
		},
	}
	req := httptest.NewRequest("POST", "/sessions/42/resize", strings.NewReader(`{"session_id":"sess-stale","cols":120,"rows":40}`))
	req.Header.Set("Authorization", "Bearer secret")
	req.SetPathValue("pid", "42")
	w := httptest.NewRecorder()

	s.resizeHandler(w, req)

	var r actionResult
	if err := json.NewDecoder(w.Body).Decode(&r); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if r.OK || r.Code != codeSessionMismatch {
		t.Fatalf("result = %+v, want refusal with codeSessionMismatch", r)
	}
}

func TestResizeHandlerSuccessCallsResizeFn(t *testing.T) {
	var gotSess Session
	var gotCols, gotRows int
	var gotRevert bool
	s := &server{
		token: "secret",
		collect: func() ([]Session, error) {
			return []Session{{PID: 42, SessionID: "sess-1", Tmux: "work:0.0"}}, nil
		},
		resizeFn: func(sess Session, cols, rows int, revert bool) error {
			gotSess, gotCols, gotRows, gotRevert = sess, cols, rows, revert
			return nil
		},
	}
	req := httptest.NewRequest("POST", "/sessions/42/resize", strings.NewReader(`{"session_id":"sess-1","cols":120,"rows":40}`))
	req.Header.Set("Authorization", "Bearer secret")
	req.SetPathValue("pid", "42")
	w := httptest.NewRecorder()

	s.resizeHandler(w, req)

	var r actionResult
	if err := json.NewDecoder(w.Body).Decode(&r); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !r.OK {
		t.Fatalf("result = %+v, want ok", r)
	}
	if gotSess.PID != 42 || gotCols != 120 || gotRows != 40 || gotRevert {
		t.Fatalf("resizeFn called with (%+v, %d, %d, revert=%v), want (PID 42, 120, 40, false)", gotSess, gotCols, gotRows, gotRevert)
	}
}

func TestResizeHandlerRevertCallsResizeFnWithRevertTrue(t *testing.T) {
	var gotRevert bool
	s := &server{
		token: "secret",
		collect: func() ([]Session, error) {
			return []Session{{PID: 42, SessionID: "sess-1", Tmux: "work:0.0"}}, nil
		},
		resizeFn: func(sess Session, cols, rows int, revert bool) error {
			gotRevert = revert
			return nil
		},
	}
	req := httptest.NewRequest("POST", "/sessions/42/resize", strings.NewReader(`{"session_id":"sess-1","revert":true}`))
	req.Header.Set("Authorization", "Bearer secret")
	req.SetPathValue("pid", "42")
	w := httptest.NewRecorder()

	s.resizeHandler(w, req)

	var r actionResult
	if err := json.NewDecoder(w.Body).Decode(&r); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !r.OK || !gotRevert {
		t.Fatalf("result = %+v, gotRevert = %v, want ok with revert=true", r, gotRevert)
	}
}

func TestResizeHandlerUnauthorized(t *testing.T) {
	s := &server{token: "secret"}
	req := httptest.NewRequest("POST", "/sessions/42/resize", strings.NewReader(`{"session_id":"sess-1","cols":120,"rows":40}`))
	req.SetPathValue("pid", "42")
	w := httptest.NewRecorder()

	s.resizeHandler(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestResizeHandlerNotLiveRefuses(t *testing.T) {
	s := &server{
		token:   "secret",
		collect: func() ([]Session, error) { return nil, nil },
	}
	req := httptest.NewRequest("POST", "/sessions/42/resize", strings.NewReader(`{"session_id":"sess-1","cols":120,"rows":40}`))
	req.Header.Set("Authorization", "Bearer secret")
	req.SetPathValue("pid", "42")
	w := httptest.NewRecorder()

	s.resizeHandler(w, req)

	var r actionResult
	if err := json.NewDecoder(w.Body).Decode(&r); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if r.OK || r.Code != codeNotLive {
		t.Fatalf("result = %+v, want refusal with codeNotLive", r)
	}
}

// TestPreviewHandlerParsesOffset covers the paging param: it rides alongside
// lines/bytes rather than replacing them, and 0 is the same request as none.
func TestPreviewHandlerParsesOffset(t *testing.T) {
	var got PreviewLimits
	s := &server{token: "test", previewLoader: func(pid int, limits PreviewLimits) (PreviewResult, error) {
		got = limits
		return PreviewResult{Source: "tmux", Content: "x"}, nil
	}}
	req := httptest.NewRequest(http.MethodGet, "/sessions/42/preview?lines=40&bytes=4096&offset=120", nil)
	req.SetPathValue("pid", "42")
	req.Header.Set("Authorization", "Bearer test")
	rec := httptest.NewRecorder()
	s.preview(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got != (PreviewLimits{MaxLines: 40, MaxBytes: 4096, Offset: 120}) {
		t.Fatalf("limits = %#v", got)
	}
}

// TestPreviewHandlerOffsetZeroMatchesNoOffset: a client that always sends the
// param must reach the byte-for-byte old request when it is paged to the live
// tail.
func TestPreviewHandlerOffsetZeroMatchesNoOffset(t *testing.T) {
	var got PreviewLimits
	s := &server{token: "test", previewLoader: func(pid int, limits PreviewLimits) (PreviewResult, error) {
		got = limits
		return PreviewResult{Source: "tmux", Content: "x"}, nil
	}}
	req := httptest.NewRequest(http.MethodGet, "/sessions/42/preview?offset=0", nil)
	req.SetPathValue("pid", "42")
	req.Header.Set("Authorization", "Bearer test")
	rec := httptest.NewRecorder()
	s.preview(rec, req)
	if got != DefaultPreviewLimits() {
		t.Fatalf("limits = %#v, want the unpaged defaults", got)
	}
}

func TestPreviewHandlerRejectsBadOffset(t *testing.T) {
	for _, q := range []string{"offset=-1", "offset=abc", "offset=1000001", "offset=1e6"} {
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

// TestPreviewHandlerServesAnEmptyPageAsOK: an exhausted page-back is a 200 with
// an empty body — the client reads "no more history", not an error.
func TestPreviewHandlerServesAnEmptyPageAsOK(t *testing.T) {
	s := &server{token: "test", previewLoader: func(pid int, limits PreviewLimits) (PreviewResult, error) {
		return PreviewResult{Source: "tmux", Label: "tmux pane dev:0.0"}, nil
	}}
	req := httptest.NewRequest(http.MethodGet, "/sessions/42/preview?offset=9000", nil)
	req.SetPathValue("pid", "42")
	req.Header.Set("Authorization", "Bearer test")
	rec := httptest.NewRecorder()
	s.preview(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "" {
		t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Claude-Sessions-Preview-Source") != "tmux" {
		t.Fatalf("source header = %q", rec.Header().Get("X-Claude-Sessions-Preview-Source"))
	}
}

// TestPreviewHandlerCountsTheLinesItServed pins the line count the client pages
// by. The rule it must never have to guess at: a trailing newline ends the last
// line rather than opening an empty one, a body cut mid-line still served that
// line, and a blank line is a line.
func TestPreviewHandlerCountsTheLinesItServed(t *testing.T) {
	cases := []struct{ body, want string }{
		{"", "0"},
		{"\n", "1"},
		{"one\n", "1"},
		{"one\ntwo\n", "2"},
		{"one\ntwo", "2"},
		{"one\n\nthree\n", "3"},
	}
	for _, c := range cases {
		body := c.body
		s := &server{token: "test", previewLoader: func(pid int, limits PreviewLimits) (PreviewResult, error) {
			return PreviewResult{Source: "tmux", Label: "dev:0.0", Content: body}, nil
		}}
		req := httptest.NewRequest(http.MethodGet, "/sessions/42/preview?offset=10", nil)
		req.SetPathValue("pid", "42")
		req.Header.Set("Authorization", "Bearer test")
		rec := httptest.NewRecorder()
		s.preview(rec, req)
		if got := rec.Header().Get("X-Claude-Sessions-Preview-Lines"); got != c.want {
			t.Fatalf("body %q: lines header = %q, want %q", c.body, got, c.want)
		}
	}
}

// TestPreviewHandlerLineCountAtTheEdgeOfHistory runs the count through the real
// loader at the one place it decides something: the last line of a transcript.
// The client is told it received one line, adds one to its offset, and the next
// request is the empty page — a 200 with a count of zero, which is how it learns
// history has run out. A count that was one out here would make it either skip
// that line or ask for it forever.
func TestPreviewHandlerLineCountAtTheEdgeOfHistory(t *testing.T) {
	old := previewTmuxCapture
	t.Cleanup(func() { previewTmuxCapture = old })
	previewTmuxCapture = func(int, PreviewLimits) (string, string, error) {
		return "", "", errNoTmuxPane
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	sessDir := filepath.Join(home, ".claude", "sessions")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessDir, "79.json"),
		[]byte(`{"pid":79,"sessionId":"sid-preview-lines"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	projDir := filepath.Join(home, ".claude", "projects", "-tmp-proj")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for i := 1; i <= 20; i++ {
		fmt.Fprintf(&b, `{"type":"user","message":{"role":"user","content":"entry-%d"}}`+"\n", i)
	}
	path := filepath.Join(projDir, "sid-preview-lines.jsonl")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	// Derived, not written down: renderEntry owns how many lines an entry is.
	full, err := formatTranscriptTail(path, 20, DefaultPreviewLimits())
	if err != nil {
		t.Fatal(err)
	}
	total := strings.Count(full, "\n")

	get := func(offset int) *httptest.ResponseRecorder {
		t.Helper()
		s := &server{token: "test"} // no injected loader: the real LoadPreview
		req := httptest.NewRequest(http.MethodGet,
			fmt.Sprintf("/sessions/79/preview?offset=%d", offset), nil)
		req.SetPathValue("pid", "79")
		req.Header.Set("Authorization", "Bearer test")
		rec := httptest.NewRecorder()
		s.preview(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("offset %d: status = %d", offset, rec.Code)
		}
		return rec
	}

	rec := get(total - 1)
	if n := strings.Count(rec.Body.String(), "\n"); n != 1 || rec.Body.Len() == 0 {
		t.Fatalf("last page = %q, want the one oldest line", rec.Body.String())
	}
	if got := rec.Header().Get("X-Claude-Sessions-Preview-Lines"); got != "1" {
		t.Fatalf("last page: lines header = %q, want 1", got)
	}

	rec = get(total)
	if rec.Body.String() != "" {
		t.Fatalf("first empty page = %q", rec.Body.String())
	}
	if got := rec.Header().Get("X-Claude-Sessions-Preview-Lines"); got != "0" {
		t.Fatalf("first empty page: lines header = %q, want 0", got)
	}
}

// TestFetchRemotePreviewOmitsOffsetWhenUnpaged keeps the TUI's own remote
// request identical to what it sent before paging existed.
func TestFetchRemotePreviewOmitsOffsetWhenUnpaged(t *testing.T) {
	var raw string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw = r.URL.RawQuery
		_, _ = w.Write([]byte("hi\n"))
	}))
	defer backend.Close()

	u, _ := url.Parse(backend.URL)
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeServerYAML(t, home, "box", u.Hostname(), u.Port(), "secret")

	if _, err := fetchRemotePreview("box", 42, DefaultPreviewLimits()); err != nil {
		t.Fatalf("err = %v", err)
	}
	if raw != "lines=2000&bytes=524288" {
		t.Fatalf("query = %q, want the pre-paging query", raw)
	}
}

func TestFetchRemotePreviewSendsOffsetWhenPaged(t *testing.T) {
	var raw string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw = r.URL.RawQuery
		_, _ = w.Write([]byte("hi\n"))
	}))
	defer backend.Close()

	u, _ := url.Parse(backend.URL)
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeServerYAML(t, home, "box", u.Hostname(), u.Port(), "secret")

	limits := PreviewLimits{MaxLines: 100, MaxBytes: 4096, Offset: 300}
	if _, err := fetchRemotePreview("box", 42, limits); err != nil {
		t.Fatalf("err = %v", err)
	}
	if raw != "lines=100&bytes=4096&offset=300" {
		t.Fatalf("query = %q", raw)
	}
}

// flagsRequestFor builds an authorized POST /sessions/{pid}/flags request.
func flagsRequestFor(pid int, body string) *http.Request {
	req := httptest.NewRequest("POST", fmt.Sprintf("/sessions/%d/flags", pid), strings.NewReader(body))
	req.SetPathValue("pid", strconv.Itoa(pid))
	req.Header.Set("Authorization", "Bearer secret")
	return req
}

// flagsServer is a server whose only live session is sess-55 at PID 55, with a
// real store behind it.
func flagsServer(t *testing.T) (*server, *FlagsStore) {
	t.Helper()
	store := newFlagsStore(filepath.Join(t.TempDir(), flagsFileName), fixedClock(time.Now()), noResolver)
	return &server{
		token: "secret",
		flags: store,
		collect: func() ([]Session, error) {
			return []Session{{PID: 55, SessionID: "sess-55"}}, nil
		},
	}, store
}

func TestFlagsHandlerSetsGroupAndDisabled(t *testing.T) {
	s, store := flagsServer(t)
	rec := httptest.NewRecorder()

	s.sessionFlagsHandler(rec, flagsRequestFor(55, `{"session_id":"sess-55","group":4,"disabled":true}`))

	r := decodeAction(t, rec)
	if !r.OK || r.SessionID != "sess-55" {
		t.Fatalf("response = %#v, want OK for sess-55", r)
	}
	if r.Group == nil || *r.Group != 4 || r.Disabled == nil || !*r.Disabled {
		t.Fatalf("response = %#v, want group 4 and disabled echoed back", r)
	}
	if store.Group("sess-55") != 4 || !store.Disabled("sess-55") {
		t.Fatalf("store = %#v, want group 4 and disabled", store.Flags("sess-55"))
	}
}

// TestFlagsHandlerAbsentFieldLeavesFlagUnchanged is the distinction the whole
// pointer-decoding dance exists for: `group: 0` clears the assignment, while a
// group key missing from the JSON entirely means "leave it alone" — and the
// same for disabled.
func TestFlagsHandlerAbsentFieldLeavesFlagUnchanged(t *testing.T) {
	s, store := flagsServer(t)
	_, _ = store.SetFlags("sess-55", intPtr(4), boolPtr(true))

	// Only disabled named: the group survives.
	rec := httptest.NewRecorder()
	s.sessionFlagsHandler(rec, flagsRequestFor(55, `{"session_id":"sess-55","disabled":false}`))
	if r := decodeAction(t, rec); !r.OK || r.Group == nil || *r.Group != 4 {
		t.Fatalf("response = %#v, want group 4 untouched by a disabled-only write", r)
	}
	if store.Group("sess-55") != 4 || store.Disabled("sess-55") {
		t.Fatalf("store = %#v, want group 4 kept and enabled", store.Flags("sess-55"))
	}

	// Explicit 0 clears the group, and says nothing about disabled.
	store.SetDisabled("sess-55", true)
	rec = httptest.NewRecorder()
	s.sessionFlagsHandler(rec, flagsRequestFor(55, `{"session_id":"sess-55","group":0}`))
	if r := decodeAction(t, rec); !r.OK || r.Group == nil || *r.Group != 0 || r.Disabled == nil || !*r.Disabled {
		t.Fatalf("response = %#v, want group cleared and still disabled", r)
	}
	if store.Group("sess-55") != 0 || !store.Disabled("sess-55") {
		t.Fatalf("store = %#v, want ungrouped and still disabled", store.Flags("sess-55"))
	}
}

// TestFlagsHandlerWithNoFlagsChangesNothing: a body naming only the
// precondition is accepted and reports what the session carries, which is what
// a client re-reading after a race wants.
func TestFlagsHandlerWithNoFlagsChangesNothing(t *testing.T) {
	s, store := flagsServer(t)
	_, _ = store.SetFlags("sess-55", intPtr(7), boolPtr(true))

	rec := httptest.NewRecorder()
	s.sessionFlagsHandler(rec, flagsRequestFor(55, `{"session_id":"sess-55"}`))

	r := decodeAction(t, rec)
	if !r.OK || r.Group == nil || *r.Group != 7 || r.Disabled == nil || !*r.Disabled {
		t.Fatalf("response = %#v, want the unchanged state echoed back", r)
	}
	if store.Group("sess-55") != 7 || !store.Disabled("sess-55") {
		t.Fatalf("store = %#v, want it untouched", store.Flags("sess-55"))
	}
}

func TestFlagsHandlerSessionMismatchRefusesWithoutMutating(t *testing.T) {
	s, store := flagsServer(t)
	rec := httptest.NewRecorder()

	s.sessionFlagsHandler(rec, flagsRequestFor(55, `{"session_id":"stale-session","group":3}`))

	r := decodeAction(t, rec)
	if r.OK || r.Code != codeSessionMismatch {
		t.Fatalf("response = %#v, want session_mismatch refusal", r)
	}
	if store.Group("sess-55") != 0 || store.Group("stale-session") != 0 {
		t.Fatal("store mutated on a refused request")
	}
}

func TestFlagsHandlerNotLiveRefusesWithoutMutating(t *testing.T) {
	s, store := flagsServer(t)
	s.collect = func() ([]Session, error) { return nil, nil }
	rec := httptest.NewRecorder()

	s.sessionFlagsHandler(rec, flagsRequestFor(55, `{"session_id":"ghost","group":3}`))

	r := decodeAction(t, rec)
	if r.OK || r.Code != codeNotLive {
		t.Fatalf("response = %#v, want not_live refusal", r)
	}
	if store.Group("ghost") != 0 {
		t.Fatal("store mutated on a refused request")
	}
}

func TestFlagsHandlerRejectsMalformedBody(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"empty body", ``},
		{"missing session_id", `{"group":3}`},
		{"null session_id", `{"session_id":null,"group":3}`},
		{"empty session_id", `{"session_id":"","group":3}`},
		{"session_id not a string", `{"session_id":42,"group":3}`},
		{"null body", `null`},
		{"unknown field", `{"session_id":"sess-55","extra":1}`},
		{"trailing content", `{"session_id":"sess-55"}{}`},
		{"null group", `{"session_id":"sess-55","group":null}`},
		{"group too high", `{"session_id":"sess-55","group":10}`},
		{"group negative", `{"session_id":"sess-55","group":-1}`},
		{"group not a number", `{"session_id":"sess-55","group":"3"}`},
		{"null disabled", `{"session_id":"sess-55","disabled":null}`},
		{"disabled not a bool", `{"session_id":"sess-55","disabled":"yes"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, store := flagsServer(t)
			rec := httptest.NewRecorder()

			s.sessionFlagsHandler(rec, flagsRequestFor(55, tc.body))

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", rec.Code, tc.body)
			}
			if store.Group("sess-55") != 0 || store.Disabled("sess-55") {
				t.Fatalf("store mutated by a rejected body: %#v", store.Flags("sess-55"))
			}
		})
	}
}

func TestFlagsHandlerRequiresAuth(t *testing.T) {
	s, store := flagsServer(t)
	req := flagsRequestFor(55, `{"session_id":"sess-55","group":3}`)
	req.Header.Del("Authorization")
	rec := httptest.NewRecorder()

	s.sessionFlagsHandler(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if store.Group("sess-55") != 0 {
		t.Fatal("store mutated by an unauthenticated request")
	}
}

// TestSessionsHandlerEmbedsGroup is the read half of the shared state: a badge
// set on this host has to reach every client polling it, in the same row the
// disabled bit rides in.
func TestSessionsHandlerEmbedsGroup(t *testing.T) {
	store := newFlagsStore(filepath.Join(t.TempDir(), flagsFileName), fixedClock(time.Now()), noResolver)
	store.SetGroup("grp-1", 6)
	s := &server{
		token: "secret",
		flags: store,
		collect: func() ([]Session, error) {
			return []Session{{PID: 1, SessionID: "grp-1"}, {PID: 2, SessionID: "grp-2"}}, nil
		},
	}
	req := httptest.NewRequest("GET", "/sessions", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()

	s.sessions(rec, req)

	var resp struct {
		Sessions []Session `json:"sessions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Sessions) != 2 {
		t.Fatalf("got %d sessions, want 2", len(resp.Sessions))
	}
	if resp.Sessions[0].Group != 6 {
		t.Fatalf("grp-1 group = %d, want 6", resp.Sessions[0].Group)
	}
	if resp.Sessions[1].Group != 0 {
		t.Fatalf("grp-2 group = %d, want 0 with no entry", resp.Sessions[1].Group)
	}
	// An ungrouped session must not carry the key at all — that absence is
	// what an older client reads as "no groups here".
	if strings.Contains(rec.Body.String(), `"group":0`) {
		t.Fatalf("ungrouped row emitted an explicit zero: %s", rec.Body.String())
	}
}

// TestFlagsHandlerReportsAStoreItCannotWrite: a session-flags.json this host
// refuses to touch (it failed to parse) must produce a refusal, not a success
// the client's next poll contradicts.
func TestFlagsHandlerReportsAStoreItCannotWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, flagsFileName)
	corrupt := []byte("{not json")
	if err := os.WriteFile(path, corrupt, 0o644); err != nil {
		t.Fatal(err)
	}
	s := &server{
		token:   "secret",
		flags:   newFlagsStore(path, fixedClock(time.Now()), noResolver),
		collect: func() ([]Session, error) { return []Session{{PID: 55, SessionID: "sess-55"}}, nil },
	}
	rec := httptest.NewRecorder()

	s.sessionFlagsHandler(rec, flagsRequestFor(55, `{"session_id":"sess-55","group":4}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with an envelope", rec.Code)
	}
	r := decodeAction(t, rec)
	if r.OK || r.Error == "" {
		t.Fatalf("response = %#v, want a refusal naming the failure", r)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != string(corrupt) {
		t.Fatalf("corrupt store was rewritten (err=%v):\n%s", err, got)
	}
}

// Identity-mismatch and verified-email-label behavior for a fetched reading
// live entirely client-side now (usage.go / known_accounts.go), since GET
// /usage no longer fetches anything to verify — see TestVerifiedIdentityMismatch
// and the known_accounts_test.go coverage of knownAccountUsage.
