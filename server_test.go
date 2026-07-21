package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

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
