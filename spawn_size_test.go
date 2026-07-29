package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// pinSpawnSize forces resolveSpawnSize's inputs for the duration of a test: no
// live tty, and cols×rows as the persisted hint (pass 0,0 for "no hint either",
// which lands on the fixed fallback). Every test that asserts on the argv of a
// real tmux new-session needs this — otherwise the flags depend on whether the
// machine running `go test` happens to hand the test binary a terminal.
func pinSpawnSize(t *testing.T, cols, rows int) {
	t.Helper()
	oldTTY, oldHint := spawnSizeTTY, spawnSizeHint
	spawnSizeTTY = func() (int, int, bool) { return 0, 0, false }
	spawnSizeHint = func() (int, int, bool) {
		if cols > 0 && rows > 0 {
			return cols, rows, true
		}
		return 0, 0, false
	}
	t.Cleanup(func() { spawnSizeTTY, spawnSizeHint = oldTTY, oldHint })
}

// TestResolveSpawnSizePrefersTTY: a live terminal is the most direct evidence
// there is, and outranks a hint file left over from a differently-sized run.
func TestResolveSpawnSizePrefersTTY(t *testing.T) {
	oldTTY, oldHint := spawnSizeTTY, spawnSizeHint
	spawnSizeTTY = func() (int, int, bool) { return 211, 61, true }
	spawnSizeHint = func() (int, int, bool) { return 100, 30, true }
	defer func() { spawnSizeTTY, spawnSizeHint = oldTTY, oldHint }()

	if cols, rows := resolveSpawnSize(); cols != 211 || rows != 61 {
		t.Errorf("resolveSpawnSize() = %d,%d, want the live tty 211,61", cols, rows)
	}
}

// TestResolveSpawnSizeFallsBackToHint: the headless server has no terminal, so
// the size the TUI last ran at on this host is the next best thing.
func TestResolveSpawnSizeFallsBackToHint(t *testing.T) {
	pinSpawnSize(t, 180, 55)

	if cols, rows := resolveSpawnSize(); cols != 180 || rows != 55 {
		t.Errorf("resolveSpawnSize() = %d,%d, want the persisted hint 180,55", cols, rows)
	}
}

// TestResolveSpawnSizeFallsBackToFixedDefault: no terminal and no hint — a
// server on a machine whose TUI has never run — still must not leave tmux on
// its own 80x24.
func TestResolveSpawnSizeFallsBackToFixedDefault(t *testing.T) {
	pinSpawnSize(t, 0, 0)

	cols, rows := resolveSpawnSize()
	if cols != spawnSizeFallbackCols || rows != spawnSizeFallbackRows {
		t.Errorf("resolveSpawnSize() = %d,%d, want %d,%d",
			cols, rows, spawnSizeFallbackCols, spawnSizeFallbackRows)
	}
	if cols <= 80 || rows <= 24 {
		t.Errorf("fallback %d,%d is no better than tmux's own default", cols, rows)
	}
}

// TestCurrentTTYSizeSkipsInsideTmux: a pane's own reported size can't be
// trusted — a detached, never-attached pane reports a perfectly valid 80x24,
// which is exactly the size this whole chain exists to avoid trusting.
func TestCurrentTTYSizeSkipsInsideTmux(t *testing.T) {
	oldGetSize := ttyGetSize
	ttyGetSize = func(int) (int, int, error) { return 211, 61, nil }
	defer func() { ttyGetSize = oldGetSize }()

	t.Setenv("TMUX", "/tmp/tmux-501/default,1234,0")
	if cols, rows, ok := currentTTYSize(); ok {
		t.Errorf("currentTTYSize() = %d,%d,true inside tmux, want no result", cols, rows)
	}
}

// TestCurrentTTYSizeUsesGetSizeOutsideTmux: the counterpart to the above —
// confirms the gate is specifically $TMUX, not a break in the underlying probe.
func TestCurrentTTYSizeUsesGetSizeOutsideTmux(t *testing.T) {
	oldGetSize := ttyGetSize
	ttyGetSize = func(int) (int, int, error) { return 211, 61, nil }
	defer func() { ttyGetSize = oldGetSize }()

	t.Setenv("TMUX", "")
	if cols, rows, ok := currentTTYSize(); !ok || cols != 211 || rows != 61 {
		t.Errorf("currentTTYSize() = %d,%d,%v, want 211,61,true outside tmux", cols, rows, ok)
	}
}

func TestParseTUISize(t *testing.T) {
	tests := []struct {
		name       string
		in         string
		cols, rows int
		ok         bool
	}{
		{"well formed", "180 55\n", 180, 55, true},
		{"no trailing newline", "180 55", 180, 55, true},
		{"extra whitespace", "  180   55  \n", 180, 55, true},
		{"empty", "", 0, 0, false},
		{"whitespace only", "  \n", 0, 0, false},
		{"one field", "180\n", 0, 0, false},
		{"trailing junk", "180 55 junk\n", 0, 0, false},
		{"non numeric", "wide tall\n", 0, 0, false},
		{"zero", "0 0\n", 0, 0, false},
		{"negative", "-1 55\n", 0, 0, false},
		{"zero rows", "180 0\n", 0, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cols, rows, ok := parseTUISize(tt.in)
			if ok != tt.ok || cols != tt.cols || rows != tt.rows {
				t.Errorf("parseTUISize(%q) = %d,%d,%v, want %d,%d,%v",
					tt.in, cols, rows, ok, tt.cols, tt.rows, tt.ok)
			}
		})
	}
}

// TestTUISizeRoundTrip: what the TUI records is what a later spawn reads back.
func TestTUISizeRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	rec := newTUISizeRecorder()
	if !rec.record(190, 58) {
		t.Fatal("record(190,58) did not write")
	}
	cols, rows, ok := loadTUISize()
	if !ok || cols != 190 || rows != 58 {
		t.Fatalf("loadTUISize() = %d,%d,%v, want 190,58,true", cols, rows, ok)
	}

	// And a later resize replaces it rather than appending.
	if !rec.record(100, 30) {
		t.Fatal("record(100,30) after a resize did not write")
	}
	if cols, rows, ok = loadTUISize(); !ok || cols != 100 || rows != 30 {
		t.Fatalf("loadTUISize() after resize = %d,%d,%v, want 100,30,true", cols, rows, ok)
	}
}

// TestTUISizeRecorderDebounce: the TUI offers the size on every frame, several
// times a second. Only a genuine change may reach the disk.
func TestTUISizeRecorderDebounce(t *testing.T) {
	writes := 0
	rec := &tuiSizeRecorder{
		path: filepath.Join(t.TempDir(), "tui-size"),
		write: func(string, int, int) error {
			writes++
			return nil
		},
	}

	if !rec.record(180, 55) {
		t.Fatal("first record did not write")
	}
	for i := 0; i < 5; i++ {
		if rec.record(180, 55) {
			t.Fatalf("record #%d rewrote an unchanged size", i+2)
		}
	}
	if writes != 1 {
		t.Fatalf("writes = %d, want 1", writes)
	}

	if !rec.record(180, 56) {
		t.Fatal("a changed size was not written")
	}
	if writes != 2 {
		t.Fatalf("writes = %d after a real resize, want 2", writes)
	}
}

// TestTUISizeRecorderRejectsNonPositive: the TUI's render path substitutes 0,0
// when term.GetSize fails. Persisting that would poison every later spawn with a
// hint that parses cleanly and means nothing.
func TestTUISizeRecorderRejectsNonPositive(t *testing.T) {
	writes := 0
	rec := &tuiSizeRecorder{
		path: filepath.Join(t.TempDir(), "tui-size"),
		write: func(string, int, int) error {
			writes++
			return nil
		},
	}

	for _, tt := range [][2]int{{0, 0}, {0, 55}, {180, 0}, {-1, -1}} {
		if rec.record(tt[0], tt[1]) {
			t.Errorf("record(%d,%d) wrote a non-positive size", tt[0], tt[1])
		}
	}
	if writes != 0 {
		t.Fatalf("writes = %d, want 0", writes)
	}
}

// TestTUISizeRecorderRetriesAfterAFailedWrite: a write that failed was never
// remembered, so the next frame must try again rather than believe the disk
// already holds the current size.
func TestTUISizeRecorderRetriesAfterAFailedWrite(t *testing.T) {
	attempts := 0
	fail := true
	rec := &tuiSizeRecorder{
		path: filepath.Join(t.TempDir(), "tui-size"),
		write: func(string, int, int) error {
			attempts++
			if fail {
				return errors.New("disk full")
			}
			return nil
		},
	}

	if rec.record(180, 55) {
		t.Fatal("record reported a write that failed")
	}
	fail = false
	if !rec.record(180, 55) {
		t.Fatal("record did not retry the same size after a failed write")
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}

// TestTUISizeRecorderWithoutAHome: an unresolvable home dir disables the hint
// rather than scattering the file into cwd, matching SessionStore's empty-path
// behaviour.
func TestTUISizeRecorderWithoutAHome(t *testing.T) {
	if (&tuiSizeRecorder{write: func(string, int, int) error {
		t.Fatal("wrote with no path")
		return nil
	}}).record(180, 55) {
		t.Error("record with an empty path reported a write")
	}
}

// TestLoadTUISizeUnusableFiles: every reason the hint can be unreadable falls
// through to "no hint" so resolveSpawnSize reaches its fixed default.
func TestLoadTUISizeUnusableFiles(t *testing.T) {
	tests := []struct {
		name    string
		content string // "" means: don't create the file at all
		create  bool
	}{
		{name: "missing"},
		{name: "empty", create: true},
		{name: "malformed", content: "not a size at all\n", create: true},
		{name: "zeroed", content: "0 0\n", create: true},
		{name: "truncated", content: "180", create: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			if tt.create {
				dir := filepath.Join(home, ".config", "claude-sessions")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "tui-size"), []byte(tt.content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if cols, rows, ok := loadTUISize(); ok {
				t.Fatalf("loadTUISize() = %d,%d,true, want no hint", cols, rows)
			}

			// And the chain lands on the fixed default rather than 0,0.
			oldTTY := spawnSizeTTY
			spawnSizeTTY = func() (int, int, bool) { return 0, 0, false }
			defer func() { spawnSizeTTY = oldTTY }()
			if cols, rows := resolveSpawnSize(); cols != spawnSizeFallbackCols || rows != spawnSizeFallbackRows {
				t.Fatalf("resolveSpawnSize() = %d,%d, want the fixed default", cols, rows)
			}
		})
	}
}

// TestWriteTUISizeLeavesNoTempBehind: the write goes through a temp file so the
// target is only ever replaced whole. This checks the observable half of that —
// exact content, and no temp file left in the directory to be picked up or to
// accumulate one per resize.
func TestWriteTUISizeLeavesNoTempBehind(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tui-size")
	if err := writeTUISize(path, 180, 55); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "tui-size" {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("dir contains %v, want just tui-size (no temp left behind)", names)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "180 55\n" {
		t.Fatalf("file = %q, want %q", data, "180 55\n")
	}
}
