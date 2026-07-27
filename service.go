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
	"strconv"
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

// validate rejects values that cannot be represented safely in a unit file.
// A unit file is line-oriented — systemd splits lines before it unquotes — so a
// newline in any rendered value ends the directive and turns whatever follows
// into a fresh one. Neither input is checked upstream: Bind is stored verbatim
// by parseServerFlags, and Path is the invoking shell's $PATH.
func (cfg serviceConfig) validate() error {
	for _, f := range []struct{ name, val string }{
		{"binary path", cfg.BinPath},
		{"--bind value", cfg.Bind},
		{"PATH", cfg.Path},
		{"log path", cfg.LogPath},
	} {
		if strings.ContainsAny(f.val, "\n\r") {
			return fmt.Errorf("%s contains a newline, which cannot be written to a service unit file", f.name)
		}
	}
	return nil
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

// launchdService installs a per-user LaunchAgent. It is constructed with home
// and uid rather than reading them itself so tests can render a plist for a
// fixed identity from any machine.
type launchdService struct {
	home string
	uid  int
}

func (s *launchdService) Label() string { return serviceLabel }

func (s *launchdService) UnitPath() string {
	return filepath.Join(s.home, "Library", "LaunchAgents", serviceLabel+".plist")
}

// defaultLogPath is where install points StandardOutPath/StandardErrorPath.
// ~/Library/Logs rather than /tmp: /tmp is swept periodically, so the log
// disappears exactly when you go looking for why the service died last week.
func (s *launchdService) defaultLogPath() string {
	return filepath.Join(s.home, "Library", "Logs", "claude-sessions.log")
}

func (s *launchdService) Render(cfg serviceConfig) string {
	logPath := cfg.LogPath
	if logPath == "" {
		// serviceConfig.LogPath is documented empty on Linux, where journald
		// captures stdout/stderr instead. launchd has no such fallback: an
		// empty StandardOutPath/StandardErrorPath can't be opened, so the job
		// fails to spawn. Substitute this backend's own default rather than
		// emit a path that breaks the service at load time.
		logPath = s.defaultLogPath()
	}
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString(`<plist version="1.0">` + "\n<dict>\n")
	fmt.Fprintf(&b, "  <key>Label</key><string>%s</string>\n", xmlEscape(serviceLabel))
	b.WriteString("  <key>ProgramArguments</key>\n  <array>\n")
	for _, arg := range serviceArgs(cfg) {
		fmt.Fprintf(&b, "    <string>%s</string>\n", xmlEscape(arg))
	}
	b.WriteString("  </array>\n")
	b.WriteString("  <key>EnvironmentVariables</key>\n  <dict>\n")
	fmt.Fprintf(&b, "    <key>PATH</key><string>%s</string>\n", xmlEscape(cfg.Path))
	b.WriteString("  </dict>\n")
	b.WriteString("  <key>RunAtLoad</key><true/>\n")
	b.WriteString("  <key>KeepAlive</key><true/>\n")
	fmt.Fprintf(&b, "  <key>StandardOutPath</key><string>%s</string>\n", xmlEscape(logPath))
	fmt.Fprintf(&b, "  <key>StandardErrorPath</key><string>%s</string>\n", xmlEscape(logPath))
	b.WriteString("</dict>\n</plist>\n")
	return b.String()
}

// serviceArgs is the argv both backends render: the binary, then exactly the
// flags `-s` itself accepts. Shared so the plist and the systemd ExecStart can
// never disagree about how the server is invoked.
func serviceArgs(cfg serviceConfig) []string {
	return []string{
		cfg.BinPath,
		"-s",
		"--port", strconv.Itoa(cfg.Port),
		"--bind", cfg.Bind,
	}
}

// xmlEscape escapes the five XML metacharacters. encoding/xml's EscapeText
// writes &#xA; for newlines and is aimed at a Writer; this is a plist, the
// values are single-line, and a tiny replacer keeps the golden files readable.
func xmlEscape(s string) string {
	return strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&apos;",
	).Replace(s)
}

// systemdService installs a --user unit. Constructed with home and user for the
// same reason launchdService is: so its unit renders identically from any
// machine under test.
type systemdService struct {
	home string
	user string
}

func (s *systemdService) Label() string { return systemdUnitName }

func (s *systemdService) UnitPath() string {
	return filepath.Join(s.home, ".config", "systemd", "user", systemdUnitName)
}

// defaultLogPath is empty: journald owns the logs on Linux, so there is no file
// to name and nothing for uninstall to leave behind.
func (s *systemdService) defaultLogPath() string { return "" }

// systemdQuote renders one value for any systemd unit line: quote the whole
// thing, doubling backslashes and quotes so they survive the quoting, and
// double any % since systemd expands % specifiers everywhere in a unit file.
// It does NOT touch $ — that expansion is ExecStart-specific, see
// systemdQuoteExecStart. Neither value reaching here is validated upstream:
// BinPath is whatever os.Executable() resolved, and PATH is the invoking
// shell's, verbatim; serviceConfig.validate rejects the one thing quoting
// cannot fix, a literal newline.
func systemdQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "%", "%%")
	return `"` + s + `"`
}

// systemdQuoteExecStart renders one ExecStart argv element. ExecStart is the
// one unit-file value line that undergoes $VAR / ${VAR} expansion after
// unquoting, so a literal $ (e.g. a path component literally named "$foo")
// must become $$ or it is silently substituted or erased. This must NOT be
// applied to Environment= — that line is not $-expanded, so a $$ written
// there would bake a literal "$$" into the running process's PATH instead of
// restoring the single $ the operator intended.
func systemdQuoteExecStart(s string) string {
	return systemdQuote(strings.ReplaceAll(s, "$", "$$"))
}

func (s *systemdService) Render(cfg serviceConfig) string {
	var b strings.Builder
	b.WriteString("[Unit]\nDescription=claude-sessions server\n\n")
	b.WriteString("[Service]\n")
	// No After=network-online.target: a --user unit isn't ordered against
	// system targets, so it would be a no-op that reads as protection.
	args := serviceArgs(cfg)
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = systemdQuoteExecStart(arg)
	}
	fmt.Fprintf(&b, "ExecStart=%s\n", strings.Join(quoted, " "))
	fmt.Fprintf(&b, "Environment=%s\n", systemdQuote("PATH="+cfg.Path))
	// Restart=always covers the boot race where this starts before tailscaled;
	// RestartSec=5 keeps the default start limit (5 in 10s) from tripping
	// before it converges.
	b.WriteString("Restart=always\nRestartSec=5\n\n")
	b.WriteString("[Install]\nWantedBy=default.target\n")
	return b.String()
}
