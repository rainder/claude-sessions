package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestActCtxEmptyHostSelectionIsNotSession(t *testing.T) {
	target := emptyHostSelectionTarget("beluga")
	c := &actCtx{targets: []selectionTarget{target}, sel: target.id}

	if got := c.selectedTarget(); got == nil || got.host != "beluga" {
		t.Fatalf("selectedTarget() = %#v, want beluga target", got)
	}
	if got := c.selected(); got != nil {
		t.Fatalf("selected() = %#v, want nil for empty host", got)
	}
}

func TestActNewEmptyLocalTargetRoutesLocal(t *testing.T) {
	target := emptyHostSelectionTarget("")
	c := &actCtx{targets: []selectionTarget{target}, sel: target.id}

	// Empty-local must NOT take the remote-new branch.
	if _, _, ok := c.selectedRemoteNewTarget(); ok {
		t.Fatalf("empty-local target routed to remote new")
	}
	// The local branch feeds c.selected() into buildCwdPicker; it is nil here
	// and must be tolerated without a panic.
	if got := c.selected(); got != nil {
		t.Fatalf("selected() = %#v, want nil for empty-local target", got)
	}
	_ = buildCwdPicker(c.selected())
}

func TestSelectedRemoteNewTarget(t *testing.T) {
	local := sessionSelectionTarget(Session{PID: 10, CWD: "/local"})
	remote := sessionSelectionTarget(Session{PID: 20, Host: "orca", CWD: "/remote"})
	empty := emptyHostSelectionTarget("beluga")
	emptyLocal := emptyHostSelectionTarget("")

	cases := []struct {
		name       string
		target     *selectionTarget
		wantHost   string
		wantCWD    string
		wantRemote bool
	}{
		{"no selection", nil, "", "", false},
		{"local session", &local, "", "", false},
		{"remote session", &remote, "orca", "/remote", true},
		{"empty remote host", &empty, "beluga", "", true},
		{"empty local host", &emptyLocal, "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &actCtx{}
			if tc.target != nil {
				c.targets = []selectionTarget{*tc.target}
				c.sel = tc.target.id
			}
			host, cwd, ok := c.selectedRemoteNewTarget()
			if host != tc.wantHost || cwd != tc.wantCWD || ok != tc.wantRemote {
				t.Fatalf("selectedRemoteNewTarget() = (%q, %q, %v), want (%q, %q, %v)",
					host, cwd, ok, tc.wantHost, tc.wantCWD, tc.wantRemote)
			}
		})
	}
}

func TestSessionActionsIgnoreEmptyHostTarget(t *testing.T) {
	target := emptyHostSelectionTarget("beluga")
	c := &actCtx{targets: []selectionTarget{target}, sel: target.id}

	actKill(c)
	actAttach(c)

	if got := c.selected(); got != nil {
		t.Fatalf("session-only actions resolved empty host as %#v", got)
	}
}

func TestActCtxEnterRawEnablesMouse(t *testing.T) {
	var buf bytes.Buffer
	prev := terminalOutput
	terminalOutput = &buf
	t.Cleanup(func() { terminalOutput = prev })

	// fd -1: term.MakeRaw no-ops on a non-terminal; the mouse-enable write is
	// the behavior under test and goes to the injected terminalOutput.
	c := &actCtx{fd: -1}
	c.enterRaw()

	if !strings.Contains(buf.String(), mouseEnableSequence) {
		t.Fatalf("enterRaw did not write mouse-enable sequence; got %q", buf.String())
	}
}

func TestRemoteNewRowsSuggestionsAndFallback(t *testing.T) {
	lines, start, entries := remoteNewRows("/selected", []cwdSuggestion{{CWD: "/history", Count: 4}, {CWD: "/selected", Count: 2}})
	if start != 0 || len(lines) != 3 || !strings.Contains(lines[0], "/history") || !strings.Contains(lines[1], "/selected") {
		t.Fatalf("rows = %#v start=%d", lines, start)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %#v", entries)
	}
	fallback, _, fallbackEntries := remoteNewRows("", nil)
	if len(fallback) != 1 || fallback[0] != "enter path manually…" {
		t.Fatalf("fallback rows = %#v", fallback)
	}
	if len(fallbackEntries) != 0 {
		t.Fatalf("fallback entries = %#v", fallbackEntries)
	}
}
