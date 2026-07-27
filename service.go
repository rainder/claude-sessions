package main

// Supervised-service installation: writes and loads a launchd LaunchAgent on
// macOS or a systemd --user unit on Linux, so `-s` survives logout and reboot.
//
// Platform choice is a runtime switch on GOOS rather than build tags. The
// termios files need tags because they name non-portable ioctl constants; both
// backends here are string rendering plus os/exec, so keeping them compiled on
// every platform is what makes their golden tests runnable from one dev box.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	serviceLabel    = "com.skerla.claude-sessions"
	systemdUnitName = "claude-sessions.service"

	// fallbackPath is used only when the environment hands us no PATH at all
	// (capturePath's os.Getenv("PATH") returned ""). It is not the launchd
	// default described on capturePath below — launchd's default is
	// /usr/bin:/bin:/usr/sbin:/sbin; this adds /usr/local/bin on top as a
	// slightly more useful last resort.
	fallbackPath = "/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
)

// serviceConfig is everything that varies between one installation and the
// next. Rendering is a pure function of it, which is what makes the unit files
// golden-testable.
type serviceConfig struct {
	BinPath string
	Port    int
	Bind    string
	Path    string // baked into the unit; see capturePath
	LogPath string // empty on Linux — journald handles it
}

// resolveBinPath returns the absolute, symlink-resolved path to this binary.
//
// EvalSymlinks pins the real file rather than a symlink pointing at it, so
// removing the symlink doesn't break the service. The trade is that a later
// `make install` that re-points the symlink is not picked up; re-running
// `service install` is the documented answer, as it already is for flags.
func resolveBinPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("cannot determine own path: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return "", fmt.Errorf("cannot resolve %s: %w", exe, err)
	}
	// `go run` builds into $GOTMPDIR if that is set and non-empty, else the OS
	// temp dir, and deletes it on exit, so a unit pointing there would
	// reference a file that no longer exists. Resolve each candidate too: on
	// macOS /tmp is a symlink to /private/tmp, and a custom GOTMPDIR could
	// equally be one.
	tmpDirs := []string{os.TempDir()}
	if gotmp := os.Getenv("GOTMPDIR"); gotmp != "" {
		tmpDirs = append(tmpDirs, gotmp)
	}
	for _, tmp := range tmpDirs {
		if t, err := filepath.EvalSymlinks(tmp); err == nil {
			tmp = t
		}
		if pathWithin(resolved, tmp) {
			return "", fmt.Errorf("running from a temporary build directory (%s)\n"+
				"       install a real binary first: make install", resolved)
		}
	}
	return resolved, nil
}

// pathWithin reports whether p is dir or sits underneath it. Uses filepath.Rel
// rather than string prefixing so /var/tmpfoo isn't treated as being inside
// /var/tmp. The prefix check is against ".."+separator, not a bare "..", so a
// legitimate child whose first path element happens to start with ".." (e.g.
// "/var/tmp/..foo/exe") isn't mistaken for an escape.
func pathWithin(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// capturePath returns the PATH to bake into the unit file.
//
// launchd gives a LaunchAgent only /usr/bin:/bin:/usr/sbin:/sbin, and systemd's
// user manager has an equally thin default. This binary shells out to tmux by
// bare name in sixteen places, plus tailscale, pngpaste, and claude — on a
// Homebrew Mac tmux is /opt/homebrew/bin/tmux and claude is ~/.local/bin/claude,
// neither of which is on that default. Without this, tmux detection silently
// finds nothing and `--bind tailscale` can't resolve, which KeepAlive turns into
// a permanent crash loop.
//
// The installing shell's PATH is the environment where the user has already
// verified these tools work, which makes it the right thing to reproduce — and
// it keeps this code out of the business of guessing Homebrew prefixes.
func capturePath() string {
	if p := os.Getenv("PATH"); p != "" {
		return p
	}
	return fallbackPath
}
