package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/term"
)

// spawnSizeFallbackCols/Rows are the last-resort dimensions for a detached tmux
// session when neither a live tty nor a remembered TUI size is available — the
// headless server on a machine whose TUI has never run. Generous on purpose:
// tmux's own default is 80x24, and the fullscreen inspector renders whatever the
// pane holds, so guessing small is the failure mode being fixed here.
const (
	spawnSizeFallbackCols = 160
	spawnSizeFallbackRows = 50
)

// spawnSizeTTY and spawnSizeHint are resolveSpawnSize's two inputs, injectable
// so the fallback chain can be exercised without a real terminal or a real file.
var (
	spawnSizeTTY  = currentTTYSize
	spawnSizeHint = loadTUISize
)

// ttyGetSize is term.GetSize, injectable so currentTTYSize's $TMUX gate can be
// tested without a real terminal.
var ttyGetSize = term.GetSize

// resolveSpawnSize picks the dimensions a newly created detached tmux session
// should be given, preferring the most direct evidence available: this process's
// own terminal, then the size the TUI last ran at on this host, then a fixed
// default.
func resolveSpawnSize() (cols, rows int) {
	if c, r, ok := spawnSizeTTY(); ok {
		return c, r
	}
	if c, r, ok := spawnSizeHint(); ok {
		return c, r
	}
	return spawnSizeFallbackCols, spawnSizeFallbackRows
}

// currentTTYSize reports the size of this process's terminal. Stdout is asked
// first and stdin second: they are the same tty for every interactive caller,
// and checking both is what keeps a scripted `claude-sessions new </dev/null` or
// `| tee` from falling through to the persisted hint needlessly. Neither being a
// terminal — the server daemon, or a fully redirected subcommand — is the
// expected case, not an error.
//
// A process running inside a tmux pane (own $TMUX set) is refused even when its
// tty reads as valid: a detached, never-attached pane reports its own 80x24 —
// exactly the size this whole chain exists to avoid — and there is no reliable
// tmux query to tell "a real client is attached" from "nobody ever was" (tried
// `display-message client_width`; it happily answers with an unrelated client's
// size instead of erroring). Falling through to the persisted hint or the fixed
// default is always at least as good as trusting a pane's self-report.
func currentTTYSize() (cols, rows int, ok bool) {
	if os.Getenv("TMUX") != "" {
		return 0, 0, false
	}
	for _, f := range []*os.File{os.Stdout, os.Stdin} {
		c, r, err := ttyGetSize(int(f.Fd()))
		if err == nil && c > 0 && r > 0 {
			return c, r, true
		}
	}
	return 0, 0, false
}

// tuiSizePath is ~/.config/claude-sessions/tui-size, alongside the other
// client-machine-local state (view-mode, sort-mode, state.json). "" when the
// home dir can't be resolved, which disables both halves of the hint.
func tuiSizePath() string {
	dir := ConfigDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "tui-size")
}

// loadTUISize reads the remembered TUI dimensions. Best-effort in every
// direction — missing, empty, malformed and non-positive all read as "no hint"
// rather than an error, because the only consequence is falling through to the
// fixed default.
func loadTUISize() (cols, rows int, ok bool) {
	path := tuiSizePath()
	if path == "" {
		return 0, 0, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, false
	}
	return parseTUISize(string(data))
}

// parseTUISize reads the "<cols> <rows>" line writeTUISize produces. Exactly two
// fields: trailing junk means a file written by something other than this
// program, which is not a hint worth trusting.
func parseTUISize(s string) (cols, rows int, ok bool) {
	fields := strings.Fields(s)
	if len(fields) != 2 {
		return 0, 0, false
	}
	c, err := strconv.Atoi(fields[0])
	if err != nil {
		return 0, 0, false
	}
	r, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, 0, false
	}
	if c <= 0 || r <= 0 {
		return 0, 0, false
	}
	return c, r, true
}

// writeTUISize persists cols×rows atomically — temp file in the same directory
// then rename, like SessionStore.save — so a spawn reading the hint at the same
// moment a resize writes it can never see a half-written line.
func writeTUISize(path string, cols, rows int) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tui-size-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := fmt.Fprintf(tmp, "%d %d\n", cols, rows); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

// tuiSizeRecorder is the write half of the hint. The TUI offers it the live
// dimensions on every frame, so it remembers what it last wrote and only touches
// the disk when the size actually changed.
type tuiSizeRecorder struct {
	path       string
	cols, rows int
	write      func(path string, cols, rows int) error
}

// newTUISizeRecorder wires the production writer. A path of "" (no resolvable
// home) leaves a recorder that silently records nothing.
func newTUISizeRecorder() *tuiSizeRecorder {
	return &tuiSizeRecorder{path: tuiSizePath(), write: writeTUISize}
}

// record persists cols×rows, reporting whether it wrote. Non-positive input is
// dropped rather than stored: the TUI's render path substitutes 0,0 when
// term.GetSize fails, and persisting that would poison every later spawn with a
// hint that parses cleanly and means nothing.
func (r *tuiSizeRecorder) record(cols, rows int) bool {
	if r == nil || r.path == "" || cols <= 0 || rows <= 0 {
		return false
	}
	if cols == r.cols && rows == r.rows {
		return false
	}
	// A failed write leaves the remembered pair alone so the next frame retries.
	if err := r.write(r.path, cols, rows); err != nil {
		return false
	}
	r.cols, r.rows = cols, rows
	return true
}
