// send_keys_test.go
package main

import (
	"errors"
	"os"
	"reflect"
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
	// Tool-neutral: nothing resolved for this PID, so nothing knows which
	// store would have owned it.
	if err == nil || !strings.Contains(err.Error(), "not a live session") {
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

// stubSendKeys replaces both tmux seams and records every send-keys call.
func stubSendKeys(t *testing.T, inMode bool, modeErr error) *[][]string {
	t.Helper()
	origMode, origTmux := sendKeysPaneInMode, sendKeysTmux
	t.Cleanup(func() { sendKeysPaneInMode, sendKeysTmux = origMode, origTmux })
	var calls [][]string
	sendKeysPaneInMode = func(string) (bool, error) { return inMode, modeErr }
	sendKeysTmux = func(args ...string) error {
		calls = append(calls, append([]string(nil), args...))
		return nil
	}
	return &calls
}

func TestSendKeysTypesLiteralWithDashDashThenEnter(t *testing.T) {
	calls := stubSendKeys(t, false, nil)
	if err := sendKeys(Session{PID: 1, Tmux: "work:0.1"}, "-R hello"); err != nil {
		t.Fatal(err)
	}
	// "=": an exact session match, never tmux's prefix fallback.
	want := [][]string{
		{"send-keys", "-t", "=work:0.1", "-l", "--", "-R hello"},
		{"send-keys", "-t", "=work:0.1", "Enter"},
	}
	if !reflect.DeepEqual(*calls, want) {
		t.Fatalf("calls = %q, want %q", *calls, want)
	}
}

func TestSendKeysRefusesPaneInCopyMode(t *testing.T) {
	calls := stubSendKeys(t, true, nil)
	err := sendKeys(Session{PID: 1, Tmux: "work:0.1"}, "quit")
	if err == nil || err.Error() != "pane is in scroll mode; press q in the terminal, then send again" {
		t.Fatalf("sendKeys = %v", err)
	}
	if len(*calls) != 0 {
		t.Fatalf("typed into a pane in copy-mode: %q", *calls)
	}
}

func TestSendKeysModeCheckErrorTypesNothing(t *testing.T) {
	calls := stubSendKeys(t, false, errors.New("no server running"))
	err := sendKeys(Session{PID: 1, Tmux: "work:0.1"}, "hi")
	if err == nil || !strings.Contains(err.Error(), "no server running") {
		t.Fatalf("sendKeys = %v", err)
	}
	if len(*calls) != 0 {
		t.Fatalf("typed after a failed mode check: %q", *calls)
	}
}
