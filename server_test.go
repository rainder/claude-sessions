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
	"testing"
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
