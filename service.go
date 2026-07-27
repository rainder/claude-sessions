package main

// Supervised-service installation: writes and loads a launchd LaunchAgent on
// macOS or a systemd --user unit on Linux, so `-s` survives logout and reboot.
//
// Platform choice is a runtime switch on GOOS rather than build tags. The
// termios files need tags because they name non-portable ioctl constants; both
// backends here are string rendering plus os/exec, so keeping them compiled on
// every platform is what makes their golden tests runnable from one dev box.

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"
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
// EvalSymlinks buys a normalized absolute path that doesn't depend on how the
// binary was invoked, and makes the temp-directory check below comparable
// against an equally resolved os.TempDir(). Re-running `service install` after
// an upgrade is the documented answer regardless of paths — neither launchd nor
// systemd restarts a running service because the file underneath it changed.
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

// defaultLogPath is where install points StandardErrorPath (see Render for
// why StandardOutPath is deliberately never set). ~/Library/Logs rather than
// /tmp: /tmp is swept periodically, so the log disappears exactly when you go
// looking for why the service died last week.
func (s *launchdService) defaultLogPath() string {
	return filepath.Join(s.home, "Library", "Logs", "claude-sessions.log")
}

// usesLinger reports whether Unload leaves a standing grant behind that
// serviceUninstall should mention. launchd has no such concept.
func (s *launchdService) usesLinger() bool { return false }

// afterRemove is a no-op: launchctl bootout already forgets the job, and
// launchd keeps no cached copy of a plist that outlives the file. See
// systemdService.afterRemove for the case this hook exists for.
func (s *launchdService) afterRemove(run runner) error { return nil }

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
	// StandardOutPath is deliberately never set: cmdServer prints the auth
	// token to stdout twice at startup (the banner and the servers.yaml
	// snippet), and launchd creates StandardOutPath 0644 with no rotation —
	// exactly the file someone pastes into a bug report, re-stamped with the
	// token on every KeepAlive restart. Nothing operational is lost: the
	// "listening on" line server.go prints already goes to stderr, and
	// bind/hostname are in this very unit file.
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

// usesLinger reports whether Unload leaves a standing grant behind that
// serviceUninstall should mention. systemdService.Load enables linger so the
// --user manager survives logout; disabling it isn't Unload's call to make
// (other user services may depend on it), so uninstall only surfaces it as a
// note. See serviceUninstall.
func (s *systemdService) usesLinger() bool { return true }

// afterRemove drops the unit from systemd's cache once serviceUninstall has
// deleted the file.
//
// Unload's own daemon-reload runs while the unit file is still on disk, so it
// re-reads the unit rather than forgetting it: systemd keeps reporting
// LoadState=loaded, and a `service status` immediately after `service
// uninstall` prints "file no / loaded yes" and exits 1 (loaded but stopped)
// instead of 3 (not loaded). Only a reload with the file already gone clears
// that. A manager method rather than a runtime.GOOS branch in
// serviceUninstall, for the same reason usesLinger is one — a GOOS branch
// there would be untestable from a single dev box.
func (s *systemdService) afterRemove(run runner) error {
	if out, err := run("systemctl", "--user", "daemon-reload"); err != nil {
		return fmt.Errorf("systemctl daemon-reload failed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// manualLoadHint is the literal command to run by hand to load the unit
// serviceInstall just wrote, mirroring launchdService.manualLoadHint.
//
// It mirrors Load's own systemctl steps, restart included, rather than the
// shorter `enable --now`: --now only *starts*, and starting an already-running
// unit is a no-op, so an operator following this hint after a failed reinstall
// would leave the old process serving the old flags and believe it worked. See
// Load, which uses restart for exactly that reason.
func (s *systemdService) manualLoadHint() string {
	return fmt.Sprintf("systemctl --user daemon-reload && systemctl --user enable %s && systemctl --user restart %s",
		systemdUnitName, systemdUnitName)
}

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
	// StandardOutput=null: cmdServer prints the auth token to stdout twice at
	// startup (the banner and the servers.yaml snippet), and journald would
	// retain it durably — readable by root and the journal group, re-stamped
	// on every KeepAlive/Restart. StandardError keeps its default (journald),
	// which is all server.go's own "listening on" line needs — that already
	// goes to stderr.
	b.WriteString("StandardOutput=null\n")
	// Restart=always covers the boot race where this starts before tailscaled;
	// RestartSec=5 keeps the default start limit (5 in 10s) from tripping
	// before it converges.
	b.WriteString("Restart=always\nRestartSec=5\n\n")
	b.WriteString("[Install]\nWantedBy=default.target\n")
	return b.String()
}

// runner executes a command and returns its combined output. Injected rather
// than called directly so the load sequences are assertable in tests without a
// real launchd or systemd.
//
// It returns output, not just an error, because Status has to parse
// `launchctl print` / `systemctl show` to report a PID.
type runner func(args ...string) ([]byte, error)

func execRunner(args ...string) ([]byte, error) {
	if len(args) == 0 {
		return nil, errors.New("empty command")
	}
	return exec.Command(args[0], args[1:]...).CombinedOutput()
}

// processExitCode extracts a process exit status, or -1 if err isn't an exit
// error. Named apart from (serviceStatus).exitCode below — that one produces
// an exit code for this process to return from `service status`; this one
// reads an exit code off a child process's error. Opposite directions, and
// Task 7 calls both, so a shared name would be a standing trap.
func processExitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

const (
	bootstrapRetries = 5
	bootstrapBackoff = 200 * time.Millisecond

	// darwinEALREADY is EALREADY from /usr/include/sys/errno.h on macOS.
	// launchctl exits with it — and also echoes the number in its own message,
	// "Bootstrap failed: 37: Operation already in progress" — when bootstrap
	// loses the race against an in-flight, asynchronous bootout.
	darwinEALREADY = 37

	// darwinESRCH is ESRCH from /usr/include/sys/errno.h on macOS. launchctl
	// bootout exits with it ("Boot-out failed: 3: No such process") when there
	// was nothing loaded to tear down — the normal case on a first install or
	// a repeat uninstall.
	darwinESRCH = 3
)

func (s *launchdService) domain() string { return fmt.Sprintf("gui/%d", s.uid) }

func (s *launchdService) target() string {
	return fmt.Sprintf("%s/%s", s.domain(), serviceLabel)
}

// manualLoadHint is the literal command to run by hand to load the unit
// serviceInstall just wrote. Named in its partial-install error message so
// the operator doesn't have to reconstruct Load's own steps from memory.
func (s *launchdService) manualLoadHint() string {
	return fmt.Sprintf("launchctl bootstrap %s %s", s.domain(), s.UnitPath())
}

func (s *launchdService) Load(run runner) error {
	// A failed bootout means "wasn't loaded" — the normal first-install case.
	// Ignoring it is what makes reinstall idempotent.
	_, _ = run("launchctl", "bootout", s.target())

	var lastOut []byte
	var lastErr error
	for attempt := 0; attempt < bootstrapRetries; attempt++ {
		out, err := run("launchctl", "bootstrap", s.domain(), s.UnitPath())
		if err == nil {
			return nil
		}
		lastOut, lastErr = out, err
		// bootout tears down asynchronously; EALREADY means we lost that race,
		// so back off and try again rather than reporting a bogus failure.
		if !isBootstrapRetryable(out, err) {
			break
		}
		// Don't sleep after the attempt that is about to exit the loop anyway —
		// that would just add 200ms of dead latency to every exhausted retry.
		if attempt < bootstrapRetries-1 {
			time.Sleep(bootstrapBackoff)
		}
	}

	// gui/<uid> only exists with a console login. Installing over SSH to a Mac
	// where nobody is logged in fails here, and launchctl's own message
	// ("Bootstrap failed: 5: Input/output error") names nothing useful.
	if _, err := run("launchctl", "print", s.domain()); err != nil {
		// The probe itself can fail for reasons that have nothing to do with a
		// missing GUI domain (launchctl off PATH, a transient XPC error, a
		// race) — append the real bootstrap failure rather than discard it, so
		// a wrong diagnosis still comes with the evidence to correct it.
		return fmt.Errorf("no GUI login session for uid %d — launchd user agents "+
			"need someone logged in at the console\n"+
			"       log in on the Mac itself, then re-run this command\n"+
			"       (bootstrap said: %v\n%s)",
			s.uid, lastErr, strings.TrimSpace(string(lastOut)))
	}
	return fmt.Errorf("launchctl bootstrap failed: %v\n%s", lastErr, strings.TrimSpace(string(lastOut)))
}

func isBootstrapRetryable(out []byte, err error) bool {
	if processExitCode(err) == darwinEALREADY {
		return true
	}
	return strings.Contains(string(out), "EALREADY") ||
		strings.Contains(string(out), "Operation already in progress")
}

func (s *launchdService) Unload(run runner) error {
	out, err := run("launchctl", "bootout", s.target())
	// "No such process" / ESRCH means nothing was loaded — the normal case for
	// a repeat uninstall. Checking the exit code as well as the string means a
	// reworded or localized launchctl message doesn't turn idempotent
	// uninstall into an error.
	if err != nil && processExitCode(err) != darwinESRCH && !strings.Contains(string(out), "No such process") {
		return fmt.Errorf("launchctl bootout failed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// serviceErr is where Load's non-fatal warnings, and every cmdService error,
// go. A package-level var rather than a direct os.Stderr call so tests can
// capture and assert on the text without redirecting the process's real
// stderr.
var serviceErr io.Writer = os.Stderr

// serviceOut is where cmdService's success and status output goes — the
// stdout counterpart to serviceErr, for the same testability reason.
var serviceOut io.Writer = os.Stdout

// serviceDim styles a parenthetical hint, but only when serviceOut is a real
// terminal. `service` is a scriptable subcommand and no other cmd* in this
// repo writes escape sequences into piped output, so an unconditional dim()
// would put a literal ESC[2m in every redirected log. Gated rather than
// dropped outright: on a terminal these lines really are asides, and the
// styling is what keeps them from reading as another fact.
//
// The check is on serviceOut's own descriptor rather than os.Stdout, so a test
// swapping in a bytes.Buffer gets clean text without a second flag to set.
func serviceDim(s string) string {
	f, ok := serviceOut.(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) {
		return s
	}
	return dim(s)
}

// resolveBin is resolveBinPath, indirected through a var so tests can swap in
// a fixed path. resolveBinPath rejects any real `go test` binary outright
// (Task 2's TestResolveBinPathRefusesTempDir pins that: the test binary
// itself always runs from a temp dir), so exercising serviceInstall's later
// steps under `go test` requires overriding this.
var resolveBin = resolveBinPath

// newManager is newServiceManager, indirected the same way as resolveBin.
// cmdService is the only caller, and every verb function below it already
// takes its manager and runner as parameters — but cmdService itself would
// otherwise always construct a manager tied to this process's real home
// directory. A test exercising cmdService past its usage-error cases needs
// to swap that out, the same way installing a real accidental service during
// this feature's manual verification showed a hardcoded call here can bite.
var newManager = newServiceManager

func (s *systemdService) Load(run runner) error {
	// Linger first: on a box where the per-user manager isn't running yet,
	// this is what starts it, and every `systemctl --user` below fails until
	// it exists. A polkit refusal only means the service dies at logout, which
	// is worth a warning rather than aborting the install.
	if out, err := run("loginctl", "enable-linger", s.user); err != nil {
		fmt.Fprintf(serviceErr, "service: warning: could not enable linger for %s: %v\n%s\n",
			s.user, err, strings.TrimSpace(string(out)))
		fmt.Fprintf(serviceErr, "service: the server will stop when your last login session ends\n")
	}
	steps := [][]string{
		{"systemctl", "--user", "daemon-reload"},
		{"systemctl", "--user", "enable", systemdUnitName},
		// restart, not `enable --now`: --now only *starts*, and starting an
		// already-running unit is a no-op, so a reinstall would leave the old
		// process serving with the old flags.
		{"systemctl", "--user", "restart", systemdUnitName},
	}
	for _, step := range steps {
		if out, err := run(step...); err != nil {
			return fmt.Errorf("%s failed: %v\n%s",
				strings.Join(step, " "), err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

func (s *systemdService) Unload(run runner) error {
	// A second `uninstall`, or one after a half-finished install, finds the
	// unit already gone — systemctl exits nonzero for that, unlike launchd's
	// bootout. Tolerate it the same way launchdService.Unload tolerates "No
	// such process", so uninstall is idempotent on both platforms.
	if out, err := run("systemctl", "--user", "disable", "--now", systemdUnitName); err != nil && !systemdUnitAlreadyGone(string(out)) {
		return fmt.Errorf("systemctl disable failed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
	if out, err := run("systemctl", "--user", "daemon-reload"); err != nil {
		return fmt.Errorf("systemctl daemon-reload failed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// systemdUnitAlreadyGone reports whether systemctl's failure output describes
// a unit that was never loaded in the first place.
func systemdUnitAlreadyGone(out string) bool {
	return strings.Contains(out, "does not exist") || strings.Contains(out, "not loaded")
}

// serviceStatus is what `service status` reports, and what its exit code
// encodes.
type serviceStatus struct {
	Installed bool
	Running   bool
	PID       int
}

// exitCode makes `service status` scriptable. 2 is deliberately unused: this
// repo reserves it for usage errors (main.go:60).
func (s serviceStatus) exitCode() int {
	switch {
	case s.Running:
		return 0
	case s.Installed:
		return 1
	default:
		return 3
	}
}

// serviceManager is the platform-specific half of the feature. Both
// implementations compile on every platform; newServiceManager picks one.
type serviceManager interface {
	UnitPath() string
	Label() string
	Render(cfg serviceConfig) string
	defaultLogPath() string
	Load(run runner) error
	Unload(run runner) error
	Status(run runner) (serviceStatus, error)
	afterRemove(run runner) error
	usesLinger() bool
	manualLoadHint() string
}

var (
	_ serviceManager = (*launchdService)(nil)
	_ serviceManager = (*systemdService)(nil)
)

// newServiceManagerFor holds the pure GOOS switch, split out from
// newServiceManager so the branch itself — and the exact error for an
// unsupported platform — is testable without depending on the actual host OS
// or a real os/user lookup.
func newServiceManagerFor(goos, home, username string) (serviceManager, error) {
	switch goos {
	case "darwin":
		return &launchdService{home: home, uid: os.Getuid()}, nil
	case "linux":
		return &systemdService{home: home, user: username}, nil
	default:
		return nil, fmt.Errorf("service management is only supported on macOS and Linux (this is %s)", goos)
	}
}

func newServiceManager() (serviceManager, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("cannot determine home directory: %w", err)
	}
	// user.Current() is Linux-only work: it shells out to nss/cgo on some
	// platforms and darwin doesn't need a username at all, so skip it there.
	var username string
	if runtime.GOOS == "linux" {
		u, err := user.Current()
		if err != nil {
			return nil, fmt.Errorf("cannot determine current user: %w", err)
		}
		username = u.Username
	}
	return newServiceManagerFor(runtime.GOOS, home, username)
}

var launchdPIDRe = regexp.MustCompile(`(?m)^\s*pid\s*=\s*(\d+)\s*$`)

func (s *launchdService) Status(run runner) (serviceStatus, error) {
	out, err := run("launchctl", "print", s.target())
	if err != nil {
		// launchctl print exits non-zero both when the service simply isn't
		// loaded and when it couldn't answer at all — e.g. `ssh mac service
		// status` with nobody logged in at the console says "Could not find
		// domain for gui/501", which is Load's own GUI-session problem
		// (service.go's launchdService.Load), not an absent service. Only the
		// former is a clean answer; anything else must surface as an error,
		// the same way systemdUnitAlreadyGone below distinguishes "already
		// gone" from a real failure in Unload.
		if strings.Contains(string(out), "Could not find service") {
			return serviceStatus{}, nil
		}
		return serviceStatus{}, fmt.Errorf("launchctl print failed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
	st := serviceStatus{Installed: true}
	if m := launchdPIDRe.FindSubmatch(out); m != nil {
		if pid, convErr := strconv.Atoi(string(m[1])); convErr == nil && pid > 0 {
			st.PID = pid
			st.Running = true
		}
	}
	return st, nil
}

// serviceUsage scopes the flags to `install` deliberately: serviceUninstall,
// serviceRestart, and serviceStatusCmd reject any argument at all with exit 2.
const serviceUsage = `usage: claude-sessions service install [--port N] [--bind ADDR]
       claude-sessions service <uninstall|restart|status>`

// serviceVerbs is the verb table cmdService dispatches through. A table rather
// than a switch inline in cmdService so the verb can be validated *before* a
// manager is constructed: newManager fails on an unsupported GOOS, and a
// typo'd verb has to stay a usage error (exit 2) there rather than being
// reported as a platform error (exit 1).
var serviceVerbs = map[string]func(mgr serviceManager, run runner, args []string) int{
	"install":   serviceInstall,
	"uninstall": serviceUninstall,
	"restart":   serviceRestart,
	"status":    serviceStatusCmd,
}

// cmdService is the `service` subcommand's entry point — the shape main.go
// calls. It owns picking the real platform manager and the real command
// runner; everything below it takes both as parameters so the verbs are
// testable without a real launchd/systemd or filesystem outside a temp dir.
func cmdService(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(serviceErr, serviceUsage)
		return 2
	}
	verb, ok := serviceVerbs[args[0]]
	if !ok {
		fmt.Fprintf(serviceErr, "service: unknown verb %q\n%s\n", args[0], serviceUsage)
		return 2
	}
	mgr, err := newManager()
	if err != nil {
		fmt.Fprintln(serviceErr, "service:", err)
		return 1
	}
	return verb(mgr, execRunner, args[1:])
}

// validateServicePort rejects a port no TCP listener could ever hold, plus 0,
// which means "pick any free port" — legitimate for a one-off `-s`, never for
// a unit file that is supposed to describe what is running. Split out as a
// pure function of the value so the boundaries are table-testable without
// driving a whole install.
func validateServicePort(port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("port %d is out of range: --port must be between 1 and 65535", port)
	}
	return nil
}

func serviceInstall(mgr serviceManager, run runner, args []string) int {
	flags, err := parseServerFlags(args)
	if err != nil {
		fmt.Fprintf(serviceErr, "service: %v\n%s\n", err, serviceUsage)
		return 2
	}
	// Reject an unusable port before anything is written or booted out. The
	// check lives here rather than in parseServerFlags because `-s` accepts
	// what it accepts today, and install is the case that needs more: it tears
	// down the running service and installs a unit that only ever fails at
	// bind time, which KeepAlive/Restart=always turns into a permanent crash
	// loop. --port 0 is worse than a typo — it installs cleanly and the server
	// then binds a different ephemeral port on every restart, so the unit no
	// longer describes what is running. Neither the portInUse pre-flight below
	// nor Load catches these: the pre-flight is skipped for a --bind value
	// that isn't an address (`--bind tailscale`, the recommended setup) and
	// again when Status says we are already running (the documented reinstall
	// path).
	if err := validateServicePort(flags.port); err != nil {
		fmt.Fprintln(serviceErr, "service:", err)
		return 2
	}
	bin, err := resolveBin()
	if err != nil {
		fmt.Fprintln(serviceErr, "service:", err)
		return 1
	}

	// A foreign listener on the target port would make the new service
	// crash-loop on bind failure — refuse rather than install something
	// broken. But re-running install is the documented way to pick up a new
	// binary (resolveBinPath's own doc comment) or change flags
	// (systemdService.Load restarts rather than merely enabling, for exactly
	// this reason), and on a normal box the thing already holding the port is
	// OUR OWN previous install. Skip the check when Status says we're already
	// running — Load is about to restart it anyway. A Status error here
	// (couldn't even ask) is not treated as "running", so the check still
	// runs, same as before Status was consulted here.
	st, _ := mgr.Status(run)
	if !st.Running {
		occupied, err := portInUse(flags.bind, flags.port)
		if err != nil {
			fmt.Fprintf(serviceErr, "service: cannot check whether %s:%d is free: %v\n", flags.bind, flags.port, err)
			return 1
		}
		if occupied {
			fmt.Fprintf(serviceErr, "service: something is already listening on %s:%d\n", flags.bind, flags.port)
			fmt.Fprintf(serviceErr, "         stop it first, or pass a different --port\n")
			return 1
		}
	}

	cfg := serviceConfig{
		BinPath: bin,
		Port:    flags.port,
		Bind:    flags.bind,
		Path:    capturePath(),
		LogPath: mgr.defaultLogPath(),
	}
	// Reject anything a rendered unit file can't represent safely BEFORE
	// touching the filesystem — see serviceConfig.validate. Neither BinPath,
	// Bind, nor Path is checked upstream: Bind is stored verbatim by
	// parseServerFlags, and Path is the invoking shell's $PATH.
	if err := cfg.validate(); err != nil {
		fmt.Fprintln(serviceErr, "service:", err)
		return 1
	}

	unitPath := mgr.UnitPath()
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
		fmt.Fprintf(serviceErr, "service: cannot create %s: %v\n", filepath.Dir(unitPath), err)
		return 1
	}
	// 0644: read by the user's own launchd/systemd. The unit file itself
	// holds no secret — the server's auth token lives in a separate 0600
	// token file — but see launchdService.Render/systemdService.Render for
	// why stdout (which does carry the token, via cmdServer's own startup
	// banner) is deliberately never wired to a file this permissive.
	if err := os.WriteFile(unitPath, []byte(mgr.Render(cfg)), 0o644); err != nil {
		fmt.Fprintf(serviceErr, "service: cannot write %s: %v\n", unitPath, err)
		return 1
	}
	fmt.Fprintf(serviceOut, "wrote   %s\n", unitPath)

	if err := mgr.Load(run); err != nil {
		fmt.Fprintln(serviceErr, "service:", err)
		// The unit file stays at unitPath — named again here, not just in the
		// "wrote" line above, because that line went to stdout and a script
		// capturing only stderr never saw it. The fix is to address whatever
		// Load reported, then load it by hand rather than repeating the whole
		// install.
		fmt.Fprintf(serviceErr, "service: the unit file was written to %s and left in place\n", unitPath)
		fmt.Fprintf(serviceErr, "service: once fixed, load it by hand: %s\n", mgr.manualLoadHint())
		return 1
	}
	fmt.Fprintf(serviceOut, "loaded  %s\n", mgr.Label())
	if cfg.LogPath != "" {
		fmt.Fprintf(serviceOut, "logs    %s  %s\n", cfg.LogPath, serviceDim("(not rotated)"))
	} else {
		fmt.Fprintf(serviceOut, "logs    journalctl --user -u %s\n", mgr.Label())
	}
	return 0
}

func serviceUninstall(mgr serviceManager, run runner, args []string) int {
	if len(args) > 0 {
		fmt.Fprintf(serviceErr, "service: uninstall takes no flags\n%s\n", serviceUsage)
		return 2
	}
	if err := mgr.Unload(run); err != nil {
		fmt.Fprintln(serviceErr, "service:", err)
		return 1
	}
	fmt.Fprintf(serviceOut, "unloaded %s\n", mgr.Label())

	unitPath := mgr.UnitPath()
	if err := os.Remove(unitPath); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(serviceErr, "service: cannot remove %s: %v\n", unitPath, err)
		return 1
	}
	// Reported before the cache drop below, because by this point the file is
	// gone whether or not that succeeds — and an operator who sees only an
	// error has no way to tell, so they retry and hit a second, unrelated
	// failure in Unload.
	fmt.Fprintf(serviceOut, "removed  %s\n", unitPath)
	// Drop the unit from the manager's cache now that the file is gone. This
	// must happen AFTER the os.Remove above: Unload's own daemon-reload ran
	// while the file was still on disk, which re-reads the unit instead of
	// forgetting it — see systemdService.afterRemove. Treated as fatal for the
	// same reason Unload treats its own reload failure as fatal: a stale cache
	// makes `service status` report a service that no longer exists.
	if err := mgr.afterRemove(run); err != nil {
		fmt.Fprintln(serviceErr, "service:", err)
		return 1
	}
	if mgr.usesLinger() {
		// Other user services may depend on linger, so removing it is not ours
		// to decide.
		fmt.Fprintf(serviceOut, "%s\n", serviceDim("linger left enabled; remove with: loginctl disable-linger $USER"))
	}
	if lp := mgr.defaultLogPath(); lp != "" {
		fmt.Fprintf(serviceOut, "%s\n", serviceDim("log kept at "+lp))
	}
	return 0
}

// serviceRestart reloads the already-installed unit — launchd bootout+
// bootstrap or systemd daemon-reload+enable+restart, via the same mgr.Load
// serviceInstall calls. It never re-renders the unit file: that would need
// resolveBin/capturePath/flag parsing all over again, and a plain restart is
// not the place to silently pick up a new --port/--bind or a rebuilt binary —
// `service install` remains the documented way to do that (see resolveBinPath
// and systemdService.Load's own doc comments).
func serviceRestart(mgr serviceManager, run runner, args []string) int {
	if len(args) > 0 {
		fmt.Fprintf(serviceErr, "service: restart takes no flags\n%s\n", serviceUsage)
		return 2
	}
	st, err := mgr.Status(run)
	if err != nil {
		fmt.Fprintln(serviceErr, "service:", err)
		return 1
	}
	if !st.Installed {
		fmt.Fprintln(serviceErr, "service: not installed — run `service install` first")
		return 1
	}
	if err := mgr.Load(run); err != nil {
		fmt.Fprintln(serviceErr, "service:", err)
		return 1
	}
	fmt.Fprintf(serviceOut, "restarted %s\n", mgr.Label())
	return 0
}

func serviceStatusCmd(mgr serviceManager, run runner, args []string) int {
	if len(args) > 0 {
		fmt.Fprintf(serviceErr, "service: status takes no flags\n%s\n", serviceUsage)
		return 2
	}
	st, err := mgr.Status(run)
	if err != nil {
		// Status distinguishes "absent" (serviceStatus{}, nil) from "couldn't
		// ask" (an error) — e.g. ssh to a Mac with no console session, or Linux
		// with no user D-Bus session yet. Only the former is exit 3; this is a
		// real failure to answer the question, not an answer of "not installed".
		fmt.Fprintln(serviceErr, "service:", err)
		return 1
	}
	unitPath := mgr.UnitPath()
	_, statErr := os.Stat(unitPath)
	fmt.Fprintf(serviceOut, "unit    %s\n", unitPath)
	fmt.Fprintf(serviceOut, "file    %s\n", yesNo(statErr == nil))
	fmt.Fprintf(serviceOut, "loaded  %s\n", yesNo(st.Installed))
	switch {
	case st.Running && st.PID != 0:
		fmt.Fprintf(serviceOut, "running yes (pid %d)\n", st.PID)
	case st.Running:
		// MainPID=0 with ActiveState=active is systemd's documented "no main
		// process" state (see systemdService.Status) — Running is genuinely
		// true, but there is no pid worth printing.
		fmt.Fprintf(serviceOut, "running yes\n")
	default:
		fmt.Fprintf(serviceOut, "running no\n")
	}
	return st.exitCode()
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// portInUse reports whether bind:port is already occupied by a live
// listener. A bind value that isn't an address — notably the magic
// "tailscale", resolved at server start — can't be checked, and an unusable
// check must not block the install, so that case returns false, nil rather
// than an error.
//
// Only EADDRINUSE (see portOccupied) counts as "occupied". net.Listen fails
// for other reasons that describe no listener to stop at all — EACCES for a
// privileged --port as a non-root user, EADDRNOTAVAIL for a --bind address
// not on this host, a plain range error for an out-of-range --port — and
// those are returned as errors so the caller reports them for what they
// actually are instead of "something is already listening ... stop it
// first", which names a process that does not exist.
// The bind value is interpreted through the same unbracket/hostPort helpers
// cmdServer uses, so `--bind [::]` and `--bind ::` mean here exactly what they
// will mean when the unit starts. Reimplementing the parse would drift, and the
// drift is silent: a bind the server accepts would skip the pre-flight.
func portInUse(bind string, port int) (bool, error) {
	if net.ParseIP(unbracket(bind)) == nil && bind != "localhost" {
		return false, nil
	}
	ln, err := net.Listen("tcp", hostPort(bind, port))
	if err != nil {
		if portOccupied(err) {
			return true, nil
		}
		return false, err
	}
	ln.Close()
	return false, nil
}

// portOccupied classifies a net.Listen failure as "a live listener already
// holds this address" versus anything else. Split out as a pure function of
// the error, rather than inlined in portInUse, so the classification is
// testable with synthetic errno values — genuine EACCES/EADDRNOTAVAIL
// conditions depend on privilege and interface configuration this test
// suite can't portably control.
func portOccupied(err error) bool {
	return errors.Is(err, syscall.EADDRINUSE)
}

func (s *systemdService) Status(run runner) (serviceStatus, error) {
	// `show` rather than `is-active`: one call yields load state, active state,
	// and the PID, and it exits zero even for a unit that doesn't exist.
	out, err := run("systemctl", "--user", "show", systemdUnitName,
		"--property=LoadState", "--property=ActiveState", "--property=MainPID")
	fields := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok {
			fields[k] = v
		}
	}
	loadState, parsed := fields["LoadState"]
	if err != nil && !parsed {
		// systemctl failed AND produced none of its usual key=value output —
		// e.g. no user D-Bus session yet ("Failed to connect to bus: No
		// medium found"). That is "couldn't ask", not "not installed". A
		// parsed LoadState=not-found below, by contrast, is systemctl
		// successfully answering "no such unit" and stays a clean absence.
		return serviceStatus{}, fmt.Errorf("systemctl show failed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
	if loadState == "" || loadState == "not-found" {
		return serviceStatus{}, nil
	}
	st := serviceStatus{Installed: true}
	if fields["ActiveState"] == "active" {
		st.Running = true
		// MainPID=0 is systemd's documented "no main process" state, not a
		// parse failure — Running stays true and PID reports the literal 0
		// rather than being suppressed by a pid>0 guard.
		if pid, err := strconv.Atoi(fields["MainPID"]); err == nil && pid >= 0 {
			st.PID = pid
		}
	}
	return st, nil
}
