// send_keys_test.go
package main

import (
	"os"
	"strings"
	"testing"
)

func TestResolveLivePIDLocalMatchingSessionSucceeds(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	pid := os.Getpid()
	writeSessionFileForPID(t, dir, Session{PID: pid, SessionID: "sess-abc", CWD: "/home/testuser/project"})

	s, err := resolveLivePIDLocal(pid, "sess-abc")
	if err != nil {
		t.Fatalf("resolveLivePIDLocal = %v, want nil error", err)
	}
	if s.SessionID != "sess-abc" {
		t.Fatalf("SessionID = %q, want sess-abc", s.SessionID)
	}
}

func TestResolveLivePIDLocalMismatchedSessionRefuses(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	pid := os.Getpid()
	writeSessionFileForPID(t, dir, Session{PID: pid, SessionID: "sess-new", CWD: "/home/testuser/project"})

	_, err := resolveLivePIDLocal(pid, "sess-old")
	if err == nil || !strings.Contains(err.Error(), "different session now") {
		t.Fatalf("resolveLivePIDLocal = %v, want session-mismatch error", err)
	}
}

func TestResolveLivePIDLocalGonePIDRefuses(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	// No session file written for this PID at all.

	_, err := resolveLivePIDLocal(999999, "sess-abc")
	if err == nil || !strings.Contains(err.Error(), "not a live Claude session") {
		t.Fatalf("resolveLivePIDLocal = %v, want not-live error", err)
	}
}

func TestResolveLivePIDLocalEmptySessionIDRefuses(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	_, err := resolveLivePIDLocal(os.Getpid(), "")
	if err == nil || !strings.Contains(err.Error(), "session_id is required") {
		t.Fatalf("resolveLivePIDLocal = %v, want session_id-required error", err)
	}
}

func TestSendKeysEmptyTmuxRefuses(t *testing.T) {
	err := sendKeys(Session{PID: 4242}, "hello")
	if err == nil || !strings.Contains(err.Error(), "no tmux pane") {
		t.Fatalf("sendKeys = %v, want no-tmux-pane error", err)
	}
}
