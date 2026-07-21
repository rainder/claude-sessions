package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
