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

func TestWriteActionOutputPosition(t *testing.T) {
	tests := []struct {
		name string
		rows int
		want string
	}{
		{"full terminal", 24, "\x1b[24;1H\x1b[K\x1b[23;1H"},
		{"two rows", 2, "\x1b[2;1H\x1b[K\x1b[1;1H"},
		{"one row", 1, "\x1b[1;1H\x1b[K"},
		{"unknown size", 0, "\x1b[9999;1H\x1b[K\x1b[1A\r"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out strings.Builder
			writeActionOutputPosition(&out, tt.rows)
			if got := out.String(); got != tt.want {
				t.Fatalf("position output = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestActionOutputPositionFollowsIncrementalScreenPatch(t *testing.T) {
	w := &recordingScreenWriter{}
	r := newScreenRenderer(w)
	if err := r.Draw("header\nold session", 80, 2); err != nil {
		t.Fatal(err)
	}
	w.writes = nil

	if err := r.Draw("header\nchanged session", 80, 2); err != nil {
		t.Fatal(err)
	}
	if got := w.last(); !strings.Contains(got, "\x1b[2;1Hchanged session") || strings.Contains(got, "\x1b[1;1H") {
		t.Fatalf("incremental patch = %q, want only changed row", got)
	}
	writeActionOutputPosition(w, 2)
	_, _ = w.Write([]byte("\nprompt > "))

	if len(w.writes) != 3 {
		t.Fatalf("writes = %d, want patch, position, and prompt", len(w.writes))
	}
	if got, want := string(w.writes[1]), "\x1b[2;1H\x1b[K\x1b[1;1H"; got != want {
		t.Fatalf("action output position = %q, want %q", got, want)
	}
	if got, want := string(w.writes[2]), "\nprompt > "; got != want {
		t.Fatalf("prompt output = %q, want %q", got, want)
	}
}

func TestActionOutputPositionFollowsPickerRedraw(t *testing.T) {
	w := &recordingScreenWriter{}
	r := newScreenRenderer(w)
	lines := []string{"/first", "enter path manually…"}
	presets := []CommandPreset{{Name: "Claude", Command: "claude"}}
	state := newPickerState{Row: 0, Preset: 0, RowCount: len(lines), PresetCount: len(presets)}
	if err := r.Draw(renderNewPickerViewport("New session", lines, presets, state, "", 8), 80, 8); err != nil {
		t.Fatal(err)
	}
	w.writes = nil

	state.Row = 1
	if err := r.Draw(renderNewPickerViewport("New session", lines, presets, state, "", 8), 80, 8); err != nil {
		t.Fatal(err)
	}
	if len(w.writes) != 1 || !strings.Contains(w.last(), "enter path manually…") {
		t.Fatalf("picker redraw = %q, want a patch for the manual path row", w.last())
	}
	writeActionOutputPosition(w, 8)
	_, _ = w.Write([]byte("\ncwd path (q=cancel) > "))

	if len(w.writes) != 3 {
		t.Fatalf("writes = %d, want patch, position, and prompt", len(w.writes))
	}
	if got, want := string(w.writes[1]), "\x1b[8;1H\x1b[K\x1b[7;1H"; got != want {
		t.Fatalf("picker action output position = %q, want %q", got, want)
	}
	if got, want := string(w.writes[2]), "\ncwd path (q=cancel) > "; got != want {
		t.Fatalf("picker prompt output = %q, want %q", got, want)
	}
}

func TestRemoteNewRowsSuggestionsAndFallback(t *testing.T) {
	lines, start, entries := remoteNewRows("/selected", []cwdSuggestion{{CWD: "/history", Count: 4}, {CWD: "/selected", Count: 2}}, "")
	if start != 0 || len(lines) != 3 || !strings.Contains(lines[0], "/history") || !strings.Contains(lines[1], "/selected") {
		t.Fatalf("rows = %#v start=%d", lines, start)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %#v", entries)
	}
	fallback, _, fallbackEntries := remoteNewRows("", nil, "")
	if len(fallback) != 1 || fallback[0] != "enter path manually…" {
		t.Fatalf("fallback rows = %#v", fallback)
	}
	if len(fallbackEntries) != 0 {
		t.Fatalf("fallback entries = %#v", fallbackEntries)
	}
}

// TestRemoteNewRowsCollapsesHome proves the remote picker collapses the remote
// host's $HOME to "~" in the DISPLAYED row while keeping the real absolute
// remote path in entries[i].cwd for the POST body. A blank home leaves the
// display untouched.
func TestRemoteNewRowsCollapsesHome(t *testing.T) {
	home := "/home/bob"
	inside := home + "/Developer/repo"
	outside := "/srv/data"
	suggestions := []cwdSuggestion{{CWD: inside, Count: 3}, {CWD: outside, Count: 1}}

	lines, _, entries := remoteNewRows("", suggestions, home)
	if !strings.Contains(lines[0], "~/Developer/repo") || strings.Contains(lines[0], home) {
		t.Fatalf("home not collapsed in display: %q", lines[0])
	}
	if !strings.Contains(lines[1], outside) {
		t.Fatalf("non-home path should stay raw: %q", lines[1])
	}
	if entries[0].cwd != inside {
		t.Fatalf("stored cwd = %q, want raw remote path %q", entries[0].cwd, inside)
	}

	// Unknown home: display stays raw, no zero-value collapsing.
	rawLines, _, _ := remoteNewRows("", suggestions, "")
	if !strings.Contains(rawLines[0], inside) {
		t.Fatalf("blank home should leave path raw: %q", rawLines[0])
	}
}
