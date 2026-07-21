package main

import (
	"errors"
	"syscall"
	"testing"
	"time"
)

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
