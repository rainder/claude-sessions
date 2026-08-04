package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/term"
)

func TestActToggleDisabledTogglesStore(t *testing.T) {
	cases := []struct {
		name    string
		session Session
		want    bool // disabled value expected after the toggle
	}{
		{"local enable to disabled", Session{PID: 10, SessionID: "local"}, true},
		{"local disabled to enabled", Session{PID: 11, SessionID: "local-off", Disabled: true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// An empty path makes FlagsStore writes no-ops (mutateLocked
			// bails on s.path == ""), so this needs a real file to actually
			// exercise SetDisabled/Disabled round-tripping.
			flags := newFlagsStore(filepath.Join(t.TempDir(), flagsFileName), nil, noResolver)
			if tc.session.Disabled {
				flags.SetDisabled(tc.session.SessionID, true)
			}
			target := sessionSelectionTarget(tc.session)
			c := &actCtx{targets: []selectionTarget{target}, sel: target.id, flags: flags}
			if !actToggleDisabled(c) {
				t.Fatal("actToggleDisabled = false, want true")
			}
			// The store is the sole authority — the live rows pick the value up
			// via the caller's settleRows()/refresh() re-overlay, not an
			// in-place patch.
			if got := flags.Disabled(tc.session.SessionID); got != tc.want {
				t.Fatalf("store.Disabled = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestActToggleDisabledIgnoresEmptyHostAndMissingID(t *testing.T) {
	flags := newFlagsStore(filepath.Join(t.TempDir(), flagsFileName), nil, noResolver)

	empty := emptyHostSelectionTarget("orca")
	c := &actCtx{targets: []selectionTarget{empty}, sel: empty.id, flags: flags}
	if actToggleDisabled(c) {
		t.Fatal("empty-host target toggled")
	}

	missingID := sessionSelectionTarget(Session{PID: 2})
	c = &actCtx{targets: []selectionTarget{missingID}, sel: missingID.id, flags: flags}
	if actToggleDisabled(c) {
		t.Fatal("missing-SessionID target toggled")
	}
	if len(flags.entries) != 0 {
		t.Fatalf("store mutated on ignored toggles: %#v", flags.entries)
	}
}

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

func TestActSetGroupWritesStore(t *testing.T) {
	cases := []struct {
		name    string
		session Session
		press   int
		want    int // group expected in the store after the keypress
	}{
		{"ungrouped takes the group", Session{PID: 10, SessionID: "a"}, 3, 3},
		{"same group again ungroups", Session{PID: 11, SessionID: "b", Group: 3}, 3, 0},
		{"another group replaces it", Session{PID: 12, SessionID: "c", Group: 3}, 5, 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			flags := newFlagsStore(filepath.Join(t.TempDir(), flagsFileName), nil, noResolver)
			if tc.session.Group != 0 {
				flags.SetGroup(tc.session.SessionID, tc.session.Group)
			}
			target := sessionSelectionTarget(tc.session)
			c := &actCtx{targets: []selectionTarget{target}, sel: target.id, flags: flags}
			if !actSetGroup(c, tc.press) {
				t.Fatal("actSetGroup = false, want true")
			}
			// The store is the sole authority — the live rows pick the value up
			// via the caller's refresh() re-overlay, not an in-place patch.
			if got := flags.Group(tc.session.SessionID); got != tc.want {
				t.Fatalf("store.Group = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestActSetGroupLeavesDisabledAlone: the two flags share a file now, so a
// group keypress must not resurrect or clear a session's disabled mark.
func TestActSetGroupLeavesDisabledAlone(t *testing.T) {
	flags := newFlagsStore(filepath.Join(t.TempDir(), flagsFileName), nil, noResolver)
	flags.SetDisabled("a", true)

	target := sessionSelectionTarget(Session{PID: 10, SessionID: "a", Disabled: true})
	c := &actCtx{targets: []selectionTarget{target}, sel: target.id, flags: flags}
	if !actSetGroup(c, 2) {
		t.Fatal("actSetGroup = false, want true")
	}

	if got := flags.Flags("a"); got.Group != 2 || !got.Disabled {
		t.Fatalf("flags = %#v, want group 2 and still disabled", got)
	}
}

// captureStdout runs fn with os.Stdout redirected to a pipe and returns what
// it printed. showActionError writes there directly rather than through
// terminalOutput, so this is the only way to see whether the user was told
// anything at all.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()
	fn()
	os.Stdout = orig
	_ = w.Close()
	out := <-done
	_ = r.Close()
	return out
}

// TestActFlagKeysReportAStoreThatCannotSave: a session-flags.json that failed
// to parse latches the store read-only for the rest of the process, so a local
// row's -/+ and ⇧1..9 must say so instead of looking dead forever — the same
// one-line action error a remote row already gets when its host refuses.
func TestActFlagKeysReportAStoreThatCannotSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), flagsFileName)
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	flags := newFlagsStore(path, nil, noResolver)
	target := sessionSelectionTarget(Session{PID: 10, SessionID: "alpha"})

	for _, tc := range []struct {
		label string
		act   func(*actCtx) bool
	}{
		{"disable", actToggleDisabled},
		{"group", func(c *actCtx) bool { return actSetGroup(c, 3) }},
	} {
		t.Run(tc.label, func(t *testing.T) {
			var sink bytes.Buffer
			terminalOutput = &sink
			t.Cleanup(func() { terminalOutput = os.Stdout })
			// A real (zero) state: enterCooked hands it to term.Restore, which
			// dereferences it.
			c := &actCtx{targets: []selectionTarget{target}, sel: target.id, flags: flags, oldState: &term.State{}}

			var changed bool
			out := captureStdout(t, func() { changed = tc.act(c) })

			if changed {
				t.Fatalf("%s reported success while the store refused to write", tc.label)
			}
			if !strings.Contains(out, tc.label) || !strings.Contains(out, "corrupt") {
				t.Fatalf("%s printed no usable error, got %q", tc.label, out)
			}
		})
	}
}

func TestActSetGroupIgnoresEmptyHostAndMissingID(t *testing.T) {
	flags := newFlagsStore(filepath.Join(t.TempDir(), flagsFileName), nil, noResolver)

	empty := emptyHostSelectionTarget("orca")
	c := &actCtx{targets: []selectionTarget{empty}, sel: empty.id, flags: flags}
	if actSetGroup(c, 1) {
		t.Fatal("empty-host target grouped")
	}

	missingID := sessionSelectionTarget(Session{PID: 2})
	c = &actCtx{targets: []selectionTarget{missingID}, sel: missingID.id, flags: flags}
	if actSetGroup(c, 1) {
		t.Fatal("missing-SessionID target grouped")
	}
	if len(flags.entries) != 0 {
		t.Fatalf("store mutated on ignored keypresses: %#v", flags.entries)
	}
}
