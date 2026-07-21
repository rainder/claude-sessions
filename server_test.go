package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
