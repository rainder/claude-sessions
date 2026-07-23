package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestSpawnNewSendsConfiguredCommand(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "tmux.log")
	script := filepath.Join(dir, "tmux")
	body := "#!/bin/sh\nfor arg in \"$@\"; do printf '<%s>' \"$arg\"; done >> \"$TMUX_LOG\"\nprintf '\\n' >> \"$TMUX_LOG\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TMUX_LOG", logPath)

	if _, err := SpawnNew(dir, "test", "claude --model fable"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "<send-keys>") || !strings.Contains(string(data), "<claude --model fable><Enter>") {
		t.Fatalf("tmux argv:\n%s", data)
	}
}

// withFastTrustPromptTiming shrinks dismissTrustPrompt's poll interval and
// timeout for the duration of a test so it doesn't eat the real 3s default.
func withFastTrustPromptTiming(t *testing.T) {
	t.Helper()
	oldInterval, oldTimeout := trustPromptPollInterval, trustPromptTimeout
	trustPromptPollInterval = time.Millisecond
	trustPromptTimeout = 20 * time.Millisecond
	t.Cleanup(func() {
		trustPromptPollInterval = oldInterval
		trustPromptTimeout = oldTimeout
	})
}

// installFakeTmuxCapture writes a fake `tmux` on PATH whose `capture-pane`
// always prints paneOutput and whose `send-keys` appends "sent\n" to a log
// file, returning the log's path.
func installFakeTmuxCapture(t *testing.T, paneOutput string) string {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "sent.log")
	script := filepath.Join(dir, "tmux")
	body := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"  capture-pane) printf '%s' \"$PANE_OUTPUT\" ;;\n" +
		"  send-keys) echo sent >> \"$SENT_LOG\" ;;\n" +
		"esac\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("PANE_OUTPUT", paneOutput)
	t.Setenv("SENT_LOG", logPath)
	return logPath
}

func TestDismissTrustPromptSendsEnterWhenDialogShown(t *testing.T) {
	withFastTrustPromptTiming(t)
	logPath := installFakeTmuxCapture(t, "Quick safety check... 1. Yes, I trust this folder")

	dismissTrustPrompt("some-session")

	data, err := os.ReadFile(logPath)
	if err != nil || !strings.Contains(string(data), "sent") {
		t.Fatalf("expected Enter to be sent, log = %q err = %v", data, err)
	}
}

func TestDismissTrustPromptNoOpWhenDialogNeverShown(t *testing.T) {
	withFastTrustPromptTiming(t)
	logPath := installFakeTmuxCapture(t, "$ claude 'fix bug'\nWelcome back!")

	dismissTrustPrompt("some-session")

	if _, err := os.ReadFile(logPath); !os.IsNotExist(err) {
		t.Fatalf("expected no send-keys call, log err = %v", err)
	}
}

func TestDismissTrustPromptReturnsOnCaptureError(t *testing.T) {
	withFastTrustPromptTiming(t)
	dir := t.TempDir()
	script := filepath.Join(dir, "tmux")
	body := "#!/bin/sh\nexit 1\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	done := make(chan struct{})
	go func() {
		dismissTrustPrompt("gone-session")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("dismissTrustPrompt did not return promptly on capture-pane error")
	}
}

func TestTmuxSessionName(t *testing.T) {
	cases := []struct {
		loc     string
		want    string
		wantErr bool
	}{
		{"work:1.0", "work", false},
		{"work:3.7", "work", false},
		{":1.0", "", true},
		{"work", "", true},
		{"", "", true},
	}
	for _, tc := range cases {
		got, err := tmuxSessionName(tc.loc)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("tmuxSessionName(%q) = (%q, %v), want (%q, error=%v)", tc.loc, got, err, tc.want, tc.wantErr)
		}
	}
}

// TestKillSessionWithTmuxKillsWholeSession: a session with trusted tmux
// metadata kills the whole tmux session and never signals the PID.
func TestKillSessionWithTmuxKillsWholeSession(t *testing.T) {
	var killed string
	var signals []syscall.Signal
	deps := killDeps{
		killTmux: func(name string) error { killed = name; return nil },
		signal:   func(_ int, sig syscall.Signal) error { signals = append(signals, sig); return nil },
		alive:    func(int) bool { return false },
		sleep:    func(time.Duration) {},
	}
	if err := killSessionWith(Session{PID: 42, Tmux: "work:1.0"}, deps); err != nil {
		t.Fatalf("killSessionWith: %v", err)
	}
	if killed != "work" {
		t.Fatalf("killed tmux target = %q, want %q", killed, "work")
	}
	if len(signals) != 0 {
		t.Fatalf("signals = %v, want none", signals)
	}
}

// TestKillSessionWithTmuxFailureDoesNotSignalPID: a failed tmux kill returns an
// error and must not fall through to signalling the PID (the TOCTOU hazard).
func TestKillSessionWithTmuxFailureDoesNotSignalPID(t *testing.T) {
	var signals []syscall.Signal
	deps := killDeps{
		killTmux: func(string) error { return errors.New("boom") },
		signal:   func(_ int, sig syscall.Signal) error { signals = append(signals, sig); return nil },
		alive:    func(int) bool { return true },
		sleep:    func(time.Duration) {},
	}
	if err := killSessionWith(Session{PID: 42, Tmux: "work:1.0"}, deps); err == nil {
		t.Fatalf("expected error from tmux kill failure")
	}
	if len(signals) != 0 {
		t.Fatalf("signals = %v, want none (must not fall through to PID signal)", signals)
	}
}

// TestKillSessionWithMalformedTmuxDoesNothing: malformed metadata is a hard
// error that touches neither tmux nor the PID.
func TestKillSessionWithMalformedTmuxDoesNothing(t *testing.T) {
	tmuxCalled := false
	signalCalled := false
	deps := killDeps{
		killTmux: func(string) error { tmuxCalled = true; return nil },
		signal:   func(int, syscall.Signal) error { signalCalled = true; return nil },
		alive:    func(int) bool { return false },
		sleep:    func(time.Duration) {},
	}
	if err := killSessionWith(Session{PID: 42, Tmux: "malformed"}, deps); err == nil {
		t.Fatalf("expected error for malformed tmux metadata")
	}
	if tmuxCalled {
		t.Fatalf("killTmux called for malformed metadata")
	}
	if signalCalled {
		t.Fatalf("signal called for malformed metadata")
	}
}

// TestKillSessionWithoutTmuxSignalsPID: a non-tmux session that dies on SIGTERM
// records exactly one signal and never escalates.
func TestKillSessionWithoutTmuxSignalsPID(t *testing.T) {
	tmuxCalled := false
	var signals []syscall.Signal
	deps := killDeps{
		killTmux: func(string) error { tmuxCalled = true; return nil },
		signal:   func(_ int, sig syscall.Signal) error { signals = append(signals, sig); return nil },
		alive:    func(int) bool { return false },
		sleep:    func(time.Duration) {},
	}
	if err := killSessionWith(Session{PID: 42}, deps); err != nil {
		t.Fatalf("killSessionWith: %v", err)
	}
	if tmuxCalled {
		t.Fatalf("killTmux called for non-tmux session")
	}
	if len(signals) != 1 || signals[0] != syscall.SIGTERM {
		t.Fatalf("signals = %v, want [SIGTERM]", signals)
	}
}

// TestKillSessionWithoutTmuxEscalates: a non-tmux session that stays alive gets
// SIGTERM then SIGKILL.
func TestKillSessionWithoutTmuxEscalates(t *testing.T) {
	var signals []syscall.Signal
	deps := killDeps{
		killTmux: func(string) error { return nil },
		signal:   func(_ int, sig syscall.Signal) error { signals = append(signals, sig); return nil },
		alive:    func(int) bool { return true },
		sleep:    func(time.Duration) {},
	}
	if err := killSessionWith(Session{PID: 42}, deps); err != nil {
		t.Fatalf("killSessionWith: %v", err)
	}
	if len(signals) != 2 || signals[0] != syscall.SIGTERM || signals[1] != syscall.SIGKILL {
		t.Fatalf("signals = %v, want [SIGTERM SIGKILL]", signals)
	}
}
