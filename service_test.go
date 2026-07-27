package main

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func TestPathWithin(t *testing.T) {
	tests := []struct {
		name string
		p    string
		dir  string
		want bool
	}{
		{name: "direct child", p: "/var/tmp/go-build123/exe", dir: "/var/tmp", want: true},
		{name: "identical path", p: "/var/tmp", dir: "/var/tmp", want: true},
		{name: "outside", p: "/usr/local/bin/claude-sessions", dir: "/var/tmp", want: false},
		{name: "sibling with shared prefix is not within", p: "/var/tmpfoo/x", dir: "/var/tmp", want: false},
		{name: "parent is not within child", p: "/var", dir: "/var/tmp", want: false},
		{name: "child directory name starting with dotdot is not an escape", p: "/var/tmp/..foo/exe", dir: "/var/tmp", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pathWithin(tt.p, tt.dir); got != tt.want {
				t.Errorf("pathWithin(%q, %q) = %v, want %v", tt.p, tt.dir, got, tt.want)
			}
		})
	}
}

// The temp-dir guard must survive /tmp being a symlink to /private/tmp on
// macOS: a raw string prefix check against os.TempDir() misses it. The
// relationship that matters is asymmetric — an exe path under the *resolved*
// dir is NOT within the *unresolved* link, which is exactly why
// resolveBinPath has to resolve the temp dir before comparing.
func TestPathWithinResolvesSymlinkedTempDir(t *testing.T) {
	realDir := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(link)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", link, err)
	}
	exeUnderResolved := filepath.Join(resolved, "exe")
	if pathWithin(exeUnderResolved, link) {
		t.Errorf("pathWithin(%q, %q) = true, want false (unresolved link path must not match a resolved exe path)", exeUnderResolved, link)
	}
	if !pathWithin(exeUnderResolved, resolved) {
		t.Errorf("pathWithin(%q, %q) = false, want true", exeUnderResolved, resolved)
	}
}

func TestCapturePathFallsBackWhenUnset(t *testing.T) {
	t.Setenv("PATH", "")
	got := capturePath()
	if got == "" {
		t.Fatal("capturePath() = \"\", want a non-empty fallback")
	}
}

func TestCapturePathUsesEnvironment(t *testing.T) {
	t.Setenv("PATH", "/opt/homebrew/bin:/usr/bin")
	if got, want := capturePath(), "/opt/homebrew/bin:/usr/bin"; got != want {
		t.Errorf("capturePath() = %q, want %q", got, want)
	}
}

// The `go test` binary itself runs from under a temp dir (os.TempDir(), or
// GOTMPDIR when set), so resolveBinPath must refuse it — that refusal is the
// entire point of the function. Skipped if GOTMPDIR points somewhere other
// than os.TempDir(), since that would move the test binary's build output out
// from under the directory this test can observe.
func TestResolveBinPathRefusesTempDir(t *testing.T) {
	if gotmp := os.Getenv("GOTMPDIR"); gotmp != "" {
		resolvedGotmp, err := filepath.EvalSymlinks(gotmp)
		if err != nil {
			t.Fatalf("EvalSymlinks(GOTMPDIR=%q): %v", gotmp, err)
		}
		resolvedTemp, err := filepath.EvalSymlinks(os.TempDir())
		if err != nil {
			t.Fatalf("EvalSymlinks(os.TempDir()): %v", err)
		}
		if resolvedGotmp != resolvedTemp {
			t.Skipf("GOTMPDIR=%q points outside os.TempDir(); premise doesn't hold here", gotmp)
		}
	}

	_, err := resolveBinPath()
	if err == nil {
		t.Fatal("resolveBinPath() = nil error, want an error (the go test binary runs from a temp dir)")
	}
	if !strings.Contains(err.Error(), "temporary") {
		t.Errorf("resolveBinPath() error = %q, want it to mention the temp directory", err.Error())
	}
}

func TestLaunchdRender(t *testing.T) {
	svc := &launchdService{home: "/Users/andy", uid: 501}
	got := svc.Render(serviceConfig{
		BinPath: "/Users/andy/.local/bin/claude-sessions",
		Port:    8765,
		Bind:    "tailscale",
		Path:    "/opt/homebrew/bin:/usr/bin:/bin",
		LogPath: "/Users/andy/Library/Logs/claude-sessions.log",
	})
	want := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>com.skerla.claude-sessions</string>
  <key>ProgramArguments</key>
  <array>
    <string>/Users/andy/.local/bin/claude-sessions</string>
    <string>-s</string>
    <string>--port</string>
    <string>8765</string>
    <string>--bind</string>
    <string>tailscale</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key><string>/opt/homebrew/bin:/usr/bin:/bin</string>
  </dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardErrorPath</key><string>/Users/andy/Library/Logs/claude-sessions.log</string>
</dict>
</plist>
`
	if got != want {
		t.Errorf("Render() mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// cmdServer prints the auth token to stdout twice at startup (the banner and
// the servers.yaml snippet). StandardOutPath would durably capture that in
// an unrotated, 0644 file that launchd re-stamps on every KeepAlive
// restart — exactly the file someone pastes into a bug report. This must
// never silently reappear; StandardErrorPath alone is enough for server.go's
// own "listening on" line, which already goes to stderr.
func TestLaunchdRenderDoesNotRouteStdoutToDurableSink(t *testing.T) {
	svc := &launchdService{home: "/Users/andy", uid: 501}
	got := svc.Render(serviceConfig{
		BinPath: "/Users/andy/.local/bin/claude-sessions",
		Port:    8765,
		Bind:    "tailscale",
		Path:    "/usr/bin",
		LogPath: "/Users/andy/Library/Logs/claude-sessions.log",
	})
	if strings.Contains(got, "StandardOutPath") {
		t.Error("Render() sets StandardOutPath — that durably captures the auth token cmdServer prints to stdout at startup")
	}
	if !strings.Contains(got, "StandardErrorPath") {
		t.Error("Render() dropped StandardErrorPath too — stderr diagnostics need somewhere to go")
	}
}

// A path or bind value containing XML metacharacters must not produce a
// malformed plist — launchd rejects the whole file, and the error names the
// line, not the cause. Every interpolation site in Render gets its own
// metacharacter here — BinPath and Bind land in ProgramArguments, Path lands
// in the PATH dict entry, and LogPath lands in StandardErrorPath (the only
// place it appears — StandardOutPath is intentionally never rendered, see
// TestLaunchdRenderDoesNotRouteStdoutToDurableSink) — so escaping can't be
// dropped from any one site without this test catching it.
func TestLaunchdRenderEscapesXML(t *testing.T) {
	svc := &launchdService{home: "/Users/andy", uid: 501}
	got := svc.Render(serviceConfig{
		BinPath: "/Users/andy/bin/claude & sessions",
		Port:    8765,
		Bind:    "<broken>",
		Path:    `/usr/bin:"unsafe"`,
		LogPath: "/var/log/user's log",
	})
	if strings.Contains(got, "claude & sessions") {
		t.Error("raw & left unescaped in ProgramArguments")
	}
	if !strings.Contains(got, "claude &amp; sessions") {
		t.Error("& not escaped to &amp;")
	}
	if strings.Contains(got, "<string><broken></string>") {
		t.Error("raw angle brackets left unescaped in bind value")
	}
	if !strings.Contains(got, "&lt;broken&gt;") {
		t.Error("angle brackets not escaped")
	}
	if strings.Contains(got, `<string>/usr/bin:"unsafe"</string>`) {
		t.Error("raw double quote left unescaped in PATH")
	}
	if !strings.Contains(got, "/usr/bin:&quot;unsafe&quot;") {
		t.Error("double quote not escaped in PATH")
	}
	if strings.Contains(got, "user's log") {
		t.Error("raw apostrophe left unescaped in log path")
	}
	if !strings.Contains(got, "user&apos;s log") {
		t.Error("apostrophe not escaped in log path")
	}
	if n := strings.Count(got, "user&apos;s log"); n != 1 {
		t.Errorf("escaped log path appears %d times, want 1 (StandardErrorPath only)", n)
	}
}

// LogPath is documented empty on Linux, where journald captures
// stdout/stderr. launchd has no such fallback — an empty StandardErrorPath
// can't be opened, so the job fails to spawn. Render must substitute
// defaultLogPath() rather than emit an empty path.
func TestLaunchdRenderFallsBackToDefaultLogPath(t *testing.T) {
	svc := &launchdService{home: "/Users/andy", uid: 501}
	got := svc.Render(serviceConfig{
		BinPath: "/Users/andy/.local/bin/claude-sessions",
		Port:    8765,
		Bind:    "tailscale",
		Path:    "/usr/bin",
		LogPath: "",
	})
	want := svc.defaultLogPath()
	if strings.Contains(got, "<string></string>") {
		t.Error("Render() emitted an empty <string></string>, launchd cannot open that path")
	}
	if n := strings.Count(got, "<string>"+want+"</string>"); n != 1 {
		t.Errorf("expected defaultLogPath() %q to appear once (StandardErrorPath), got %d", want, n)
	}
}

func TestLaunchdUnitPath(t *testing.T) {
	svc := &launchdService{home: "/Users/andy", uid: 501}
	want := "/Users/andy/Library/LaunchAgents/com.skerla.claude-sessions.plist"
	if got := svc.UnitPath(); got != want {
		t.Errorf("UnitPath() = %q, want %q", got, want)
	}
}

func TestLaunchdDefaultLogPath(t *testing.T) {
	svc := &launchdService{home: "/Users/andy", uid: 501}
	want := "/Users/andy/Library/Logs/claude-sessions.log"
	if got := svc.defaultLogPath(); got != want {
		t.Errorf("defaultLogPath() = %q, want %q", got, want)
	}
}

func TestSystemdRender(t *testing.T) {
	svc := &systemdService{home: "/home/andy", user: "andy"}
	got := svc.Render(serviceConfig{
		BinPath: "/home/andy/.local/bin/claude-sessions",
		Port:    8765,
		Bind:    "tailscale",
		Path:    "/home/andy/.local/bin:/usr/bin:/bin",
	})
	want := `[Unit]
Description=claude-sessions server

[Service]
ExecStart="/home/andy/.local/bin/claude-sessions" "-s" "--port" "8765" "--bind" "tailscale"
Environment="PATH=/home/andy/.local/bin:/usr/bin:/bin"
StandardOutput=null
Restart=always
RestartSec=5

[Install]
WantedBy=default.target
`
	if got != want {
		t.Errorf("Render() mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// Same rationale as TestLaunchdRenderDoesNotRouteStdoutToDurableSink:
// cmdServer's startup banner prints the auth token to stdout, and journald
// would retain it durably (readable by root and the journal group,
// re-stamped on every Restart). StandardOutput=null keeps stdout off any
// durable sink while StandardError keeps its default, still reaching
// journald.
func TestSystemdRenderDoesNotRouteStdoutToDurableSink(t *testing.T) {
	svc := &systemdService{home: "/home/andy", user: "andy"}
	got := svc.Render(serviceConfig{BinPath: "/bin/x", Port: 1, Bind: "127.0.0.1", Path: "/usr/bin"})
	if !strings.Contains(got, "StandardOutput=null") {
		t.Error("Render() does not set StandardOutput=null — stdout (which carries the auth token banner) would land in journald durably")
	}
}

func TestSystemdUnitPath(t *testing.T) {
	svc := &systemdService{home: "/home/andy", user: "andy"}
	want := "/home/andy/.config/systemd/user/claude-sessions.service"
	if got := svc.UnitPath(); got != want {
		t.Errorf("UnitPath() = %q, want %q", got, want)
	}
}

// RestartSec=5 keeps systemd's default start limit (5 starts in 10s) from
// tripping while tailscaled is still coming up at boot. Without it the unit
// gives up permanently instead of converging.
func TestSystemdRenderHasRestartBackoff(t *testing.T) {
	svc := &systemdService{home: "/home/andy", user: "andy"}
	got := svc.Render(serviceConfig{BinPath: "/bin/x", Port: 1, Bind: "127.0.0.1", Path: "/usr/bin"})
	for _, want := range []string{"Restart=always", "RestartSec=5"} {
		if !strings.Contains(got, want) {
			t.Errorf("unit missing %q", want)
		}
	}
}

// A --user unit is not ordered against system targets, so After= would be a
// no-op that reads as protection it doesn't provide.
func TestSystemdRenderOmitsNetworkTarget(t *testing.T) {
	svc := &systemdService{home: "/home/andy", user: "andy"}
	got := svc.Render(serviceConfig{BinPath: "/bin/x", Port: 1, Bind: "127.0.0.1", Path: "/usr/bin"})
	if strings.Contains(got, "network-online.target") {
		t.Error("unit sets After=network-online.target, a no-op in a --user manager")
	}
}

// systemd re-splits ExecStart on whitespace. A BinPath containing a space
// (e.g. an installer landing under "/Users/andy/My Tools/") would silently
// produce a two-element argv unless the rendered element stays quoted as one
// unit.
func TestSystemdRenderQuotesPathWithSpace(t *testing.T) {
	svc := &systemdService{home: "/home/andy", user: "andy"}
	got := svc.Render(serviceConfig{
		BinPath: "/home/andy/My Tools/claude-sessions",
		Port:    8765,
		Bind:    "tailscale",
		Path:    "/usr/bin",
	})
	if !strings.Contains(got, `ExecStart="/home/andy/My Tools/claude-sessions" "-s" "--port" "8765" "--bind" "tailscale"`) {
		t.Errorf("ExecStart did not quote the space-containing BinPath as one element:\n%s", got)
	}
}

// systemd expands % specifiers in unit files, so a literal % in a captured
// PATH must survive as %% or it is silently eaten/misinterpreted.
func TestSystemdRenderEscapesPercent(t *testing.T) {
	svc := &systemdService{home: "/home/andy", user: "andy"}
	got := svc.Render(serviceConfig{
		BinPath: "/bin/x",
		Port:    1,
		Bind:    "127.0.0.1",
		Path:    "/opt/weird%dir/bin:/usr/bin",
	})
	if !strings.Contains(got, "/opt/weird%%dir/bin:/usr/bin") {
		t.Errorf("Environment did not escape %% as %%%%:\n%s", got)
	}
	// Every maximal run of consecutive % characters must have even length:
	// %% is one escaped literal, but %%% (odd) still leaves a lone specifier
	// for systemd to expand. A "does this % have a %-neighbor" check would
	// pass %%% since each % has at least one neighboring %, so this walks
	// full runs instead.
	run := 0
	for i := 0; i <= len(got); i++ {
		if i < len(got) && got[i] == '%' {
			run++
			continue
		}
		if run%2 != 0 {
			t.Errorf("odd-length run of %d %% characters ending at byte %d:\n%s", run, i, got)
		}
		run = 0
	}
}

func TestSystemdQuote(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "plain value", in: "tailscale", want: `"tailscale"`},
		{name: "value with a space", in: "My Tools", want: `"My Tools"`},
		{name: "value with a quote", in: `say "hi"`, want: `"say \"hi\""`},
		{name: "value with a backslash", in: `C:\bin`, want: `"C:\\bin"`},
		{name: "value with a percent", in: "50%done", want: `"50%%done"`},
		{name: "several at once", in: `\"%`, want: `"\\\"%%"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := systemdQuote(tt.in); got != tt.want {
				t.Errorf("systemdQuote(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestSystemdDefaultLogPath(t *testing.T) {
	svc := &systemdService{home: "/home/andy", user: "andy"}
	if got := svc.defaultLogPath(); got != "" {
		t.Errorf("defaultLogPath() = %q, want \"\" (journald owns Linux logs)", got)
	}
}

// systemd expands $VAR/${VAR} in ExecStart after unquoting, so a literal $ in
// an argv element (e.g. a path component literally named "$foo") must survive
// as $$ or it is silently substituted or erased.
func TestSystemdRenderEscapesDollarInExecStart(t *testing.T) {
	svc := &systemdService{home: "/home/andy", user: "andy"}
	got := svc.Render(serviceConfig{
		BinPath: "/home/andy/$weird/claude-sessions",
		Port:    8765,
		Bind:    "tail$scale",
		Path:    "/usr/bin",
	})
	if !strings.Contains(got, `ExecStart="/home/andy/$$weird/claude-sessions"`) {
		t.Errorf("ExecStart did not escape $ in argv[0]:\n%s", got)
	}
	// argv[0] is not the only element that can carry a $: --bind is a value a
	// user actually supplies, and an implementation that only escaped BinPath
	// would still pass a test that never puts a $ anywhere else.
	if !strings.Contains(got, `"tail$$scale"`) {
		t.Errorf("ExecStart did not escape $ in a non-zeroth argv element (--bind):\n%s", got)
	}
}

// Environment= is NOT $-expanded by systemd, unlike ExecStart. A $ in cfg.Path
// must stay a single $ on the Environment= line — doubling it there would bake
// a literal "$$" into the running process's PATH instead of restoring the $.
// This is the test that would catch someone "helpfully" applying the
// ExecStart $-escaping everywhere.
func TestSystemdRenderLeavesDollarInEnvironment(t *testing.T) {
	svc := &systemdService{home: "/home/andy", user: "andy"}
	got := svc.Render(serviceConfig{
		BinPath: "/bin/x",
		Port:    1,
		Bind:    "127.0.0.1",
		Path:    "/opt/$weird/bin:/usr/bin",
	})
	if !strings.Contains(got, `Environment="PATH=/opt/$weird/bin:/usr/bin"`) {
		t.Errorf("Environment line altered a literal $ it should have left alone:\n%s", got)
	}
	if strings.Contains(got, "$$weird") {
		t.Error("Environment line doubled $ as $$, but Environment= is not $-expanded")
	}
}

func TestServiceConfigValidate(t *testing.T) {
	clean := serviceConfig{
		BinPath: "/home/andy/.local/bin/claude-sessions",
		Port:    8765,
		Bind:    "tailscale",
		Path:    "/home/andy/.local/bin:/usr/bin:/bin",
		LogPath: "/home/andy/Library/Logs/claude-sessions.log",
	}
	if err := clean.validate(); err != nil {
		t.Errorf("validate() on a clean config = %v, want nil", err)
	}

	tests := []struct {
		name       string
		mutate     func(cfg *serviceConfig, nl string)
		errContain string
	}{
		{name: "newline in BinPath", mutate: func(cfg *serviceConfig, nl string) { cfg.BinPath += nl + "ExecStartPre=/bin/sh -c evil" }, errContain: "binary path"},
		{name: "newline in Bind", mutate: func(cfg *serviceConfig, nl string) { cfg.Bind += nl + "ExecStartPre=/bin/sh -c evil" }, errContain: "--bind value"},
		{name: "newline in Path", mutate: func(cfg *serviceConfig, nl string) { cfg.Path += nl + "ExecStartPre=/bin/sh -c evil" }, errContain: "PATH"},
		{name: "newline in LogPath", mutate: func(cfg *serviceConfig, nl string) { cfg.LogPath += nl + "ExecStartPre=/bin/sh -c evil" }, errContain: "log path"},
	}
	for _, tt := range tests {
		for _, nl := range []string{"\n", "\r"} {
			t.Run(tt.name+" "+strconv.QuoteRune([]rune(nl)[0]), func(t *testing.T) {
				cfg := clean
				tt.mutate(&cfg, nl)
				err := cfg.validate()
				if err == nil {
					t.Fatalf("validate() = nil, want an error naming %q", tt.errContain)
				}
				if !strings.Contains(err.Error(), tt.errContain) {
					t.Errorf("validate() error = %q, want it to mention %q", err.Error(), tt.errContain)
				}
			})
		}
	}
}

// fakeRunner records every command it is handed and replays scripted results,
// so the load sequences can be asserted without a real launchd or systemd.
//
// scripts is an ordered slice, not a map: matching walks it in registration
// order and the first substring match wins. A map here would make the
// outcome of two overlapping scripted substrings (e.g. a test that scripts
// both "enable" and "enable-linger") depend on Go's randomized map
// iteration — a flake waiting for the next test that needs two rules active
// at once.
type fakeRunner struct {
	calls   [][]string
	scripts []*fakeScript
}

type fakeResult struct {
	out []byte
	err error
}

// fakeScript is one substring rule. remain counts how many more matching
// calls should still get result: negative means forever, zero means
// exhausted (matches fall through — to a later script, or to success if
// none match).
type fakeScript struct {
	substr string
	result fakeResult
	remain int
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{}
}

// fail scripts a result for any command whose joined form contains substr,
// forever.
func (f *fakeRunner) fail(substr string, out string, err error) {
	f.scripts = append(f.scripts, &fakeScript{substr: substr, result: fakeResult{[]byte(out), err}, remain: -1})
}

// failN scripts a result for exactly the next n commands whose joined form
// contains substr; calls after that succeed. This is what lets a test
// express "bootstrap fails once with EALREADY, then the retry succeeds" —
// the actual behavior Load's retry loop exists for.
func (f *fakeRunner) failN(substr string, n int, out string, err error) {
	f.scripts = append(f.scripts, &fakeScript{substr: substr, result: fakeResult{[]byte(out), err}, remain: n})
}

func (f *fakeRunner) run(args ...string) ([]byte, error) {
	// Copy args: the caller (e.g. systemdService.Load's steps slice) may reuse
	// or mutate its backing array after this call returns, and recorded calls
	// must not alias it.
	f.calls = append(f.calls, append([]string(nil), args...))
	joined := strings.Join(args, " ")
	for _, s := range f.scripts {
		if !strings.Contains(joined, s.substr) {
			continue
		}
		if s.remain == 0 {
			continue // exhausted — fall through to a later script or success
		}
		if s.remain > 0 {
			s.remain--
		}
		return s.result.out, s.result.err
	}
	return nil, nil
}

func (f *fakeRunner) joined() []string {
	out := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		out = append(out, strings.Join(c, " "))
	}
	return out
}

// fakeExitError builds a real *exec.ExitError carrying the given exit code,
// by actually running a subprocess that exits with it. processExitCode's
// numeric branch reads ee.ExitCode() via errors.As, which only a genuine
// *exec.ExitError satisfies — a plain errors.New("exit status N") does not,
// so tests that need to exercise that branch specifically (as opposed to the
// output-string fallback) need the real thing.
func fakeExitError(t *testing.T, code int) *exec.ExitError {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", fmt.Sprintf("exit %d", code))
	err := cmd.Run()
	ee, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("expected /bin/sh -c 'exit %d' to produce an *exec.ExitError, got %T (%v)", code, err, err)
	}
	return ee
}

func TestLaunchdLoadBootsOutBeforeBootstrapping(t *testing.T) {
	f := newFakeRunner()
	svc := &launchdService{home: "/Users/andy", uid: 501}
	if err := svc.Load(f.run); err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	got := f.joined()
	want := []string{
		"launchctl bootout gui/501/com.skerla.claude-sessions",
		"launchctl bootstrap gui/501 /Users/andy/Library/LaunchAgents/com.skerla.claude-sessions.plist",
	}
	if len(got) != len(want) {
		t.Fatalf("Load() ran %d commands %q, want %d %q", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("command %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// bootout failing means "it wasn't loaded", which is the normal first-install
// case, not an error.
func TestLaunchdLoadIgnoresBootoutFailure(t *testing.T) {
	f := newFakeRunner()
	f.fail("bootout", "Boot-out failed: 3: No such process", errors.New("exit status 3"))
	svc := &launchdService{home: "/Users/andy", uid: 501}
	if err := svc.Load(f.run); err != nil {
		t.Fatalf("Load() error: %v, want nil — a failed bootout is not fatal", err)
	}
	if len(f.calls) != 2 {
		t.Fatalf("Load() ran %q, want bootstrap to still have run", f.joined())
	}
}

// bootout tears down asynchronously, so an immediate bootstrap can lose the
// race and return EALREADY. Retrying is the fix; failing is a spurious install
// error on every reinstall. This scripts EALREADY via the message text only
// (a plain error, not an *exec.ExitError), so it exercises isBootstrapRetryable's
// string-fallback branch specifically — see
// TestLaunchdLoadRetriesOnEALREADYExitCodeAlone for the numeric branch.
func TestLaunchdLoadRetriesEALREADY(t *testing.T) {
	f := newFakeRunner()
	f.fail("bootstrap", "Bootstrap failed: 37: Operation already in progress", errors.New("exit status 37"))
	svc := &launchdService{home: "/Users/andy", uid: 501}
	err := svc.Load(f.run)
	if err == nil {
		t.Fatal("Load() = nil, want an error after retries are exhausted")
	}
	got := f.joined()
	bootstraps := 0
	for _, c := range got {
		if strings.Contains(c, "bootstrap") {
			bootstraps++
		}
	}
	if bootstraps != bootstrapRetries {
		t.Errorf("bootstrap attempted %d times, want %d (all retries exhausted)", bootstraps, bootstrapRetries)
	}
	// Exhausting retries is supposed to lead into the GUI-session probe, not
	// just stop. Deleting that probe call must not leave this test green.
	if len(got) == 0 || !strings.Contains(got[len(got)-1], "launchctl print gui/501") {
		t.Errorf("Load() commands = %q, want the sequence to end with the launchctl print probe", got)
	}
}

// isBootstrapRetryable's numeric branch (processExitCode(err) == darwinEALREADY)
// exists because launchctl's exit code carries EALREADY even when its
// message wording changes. Script output with none of the retry strings in
// it at all, so only the numeric check can make this retry — deleting that
// branch must turn this red. This is also processExitCode's only direct
// coverage.
func TestLaunchdLoadRetriesOnEALREADYExitCodeAlone(t *testing.T) {
	f := newFakeRunner()
	f.fail("bootstrap", "Bootstrap failed: something unrelated to any wording this code matches on", fakeExitError(t, 37))
	svc := &launchdService{home: "/Users/andy", uid: 501}
	err := svc.Load(f.run)
	if err == nil {
		t.Fatal("Load() = nil, want an error after retries are exhausted")
	}
	bootstraps := 0
	for _, c := range f.joined() {
		if strings.Contains(c, "bootstrap") {
			bootstraps++
		}
	}
	if bootstraps != bootstrapRetries {
		t.Errorf("bootstrap attempted %d times, want %d (retried purely via exit code 37)", bootstraps, bootstrapRetries)
	}
}

// The retry loop's entire reason to exist: bootstrap loses the async-bootout
// race once, then the very next attempt succeeds. A mutation like "only ever
// retry once" or "give up after the first failure" must turn this red.
func TestLaunchdLoadRetriesThenSucceeds(t *testing.T) {
	f := newFakeRunner()
	f.failN("bootstrap", 1, "Bootstrap failed: 37: Operation already in progress", errors.New("exit status 37"))
	svc := &launchdService{home: "/Users/andy", uid: 501}
	if err := svc.Load(f.run); err != nil {
		t.Fatalf("Load() error: %v, want nil — the second bootstrap attempt should succeed", err)
	}
	got := f.joined()
	want := []string{
		"launchctl bootout gui/501/com.skerla.claude-sessions",
		"launchctl bootstrap gui/501 /Users/andy/Library/LaunchAgents/com.skerla.claude-sessions.plist",
		"launchctl bootstrap gui/501 /Users/andy/Library/LaunchAgents/com.skerla.claude-sessions.plist",
	}
	if len(got) != len(want) {
		t.Fatalf("Load() ran %d commands %q, want %d %q", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("command %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// When bootstrap is exhausted AND the GUI-session probe itself also fails,
// the error must still carry the real bootstrap failure. The probe can fail
// for reasons unrelated to a missing GUI session (launchctl off PATH, a
// transient XPC error), and discarding lastErr/lastOut there hands the
// operator a confident, wrong diagnosis with zero forensic output.
func TestLaunchdLoadReportsBootstrapFailureWhenProbeAlsoFails(t *testing.T) {
	f := newFakeRunner()
	f.fail("bootstrap", "Bootstrap failed: 5: Input/output error", errors.New("exit status 5"))
	f.fail("print", "launchctl print: some XPC error", errors.New("exit status 1"))
	svc := &launchdService{home: "/Users/andy", uid: 501}
	err := svc.Load(f.run)
	if err == nil {
		t.Fatal("Load() = nil, want an error")
	}
	if !strings.Contains(err.Error(), "GUI login session") {
		t.Errorf("Load() error = %q, want it to mention a GUI login session", err.Error())
	}
	if !strings.Contains(err.Error(), "Bootstrap failed: 5: Input/output error") {
		t.Errorf("Load() error = %q, want it to still carry the original bootstrap failure output, not just the GUI hint", err.Error())
	}
}

func TestLaunchdUnload(t *testing.T) {
	f := newFakeRunner()
	svc := &launchdService{home: "/Users/andy", uid: 501}
	if err := svc.Unload(f.run); err != nil {
		t.Fatalf("Unload() error: %v", err)
	}
	want := []string{"launchctl bootout gui/501/com.skerla.claude-sessions"}
	if got := f.joined(); len(got) != 1 || got[0] != want[0] {
		t.Errorf("Unload() ran %q, want %q", got, want)
	}
}

// bootout reporting "No such process" means nothing was loaded — the normal
// case for a repeat uninstall, or one after a half-finished install. Unload
// must tolerate it.
func TestLaunchdUnloadIgnoresNoSuchProcess(t *testing.T) {
	f := newFakeRunner()
	f.fail("bootout", "Boot-out failed: 3: No such process", errors.New("exit status 3"))
	svc := &launchdService{home: "/Users/andy", uid: 501}
	if err := svc.Unload(f.run); err != nil {
		t.Fatalf("Unload() error: %v, want nil — nothing was loaded", err)
	}
}

// launchctl reports "nothing was loaded" as exit code 3 (ESRCH), not just the
// English string. A reworded or localized message must still be tolerated via
// the exit code alone, so this scripts output with none of the tolerated
// wording in it at all.
func TestLaunchdUnloadIgnoresESRCHExitCodeAlone(t *testing.T) {
	f := newFakeRunner()
	f.fail("bootout", "some unrelated wording entirely", fakeExitError(t, 3))
	svc := &launchdService{home: "/Users/andy", uid: 501}
	if err := svc.Unload(f.run); err != nil {
		t.Fatalf("Unload() error: %v, want nil — exit code 3 (ESRCH) alone should be tolerated", err)
	}
}

// A bootout failure that is neither "No such process" nor ESRCH is a real
// failure and must be reported, not swallowed alongside the idempotent case.
func TestLaunchdUnloadReportsOtherFailures(t *testing.T) {
	f := newFakeRunner()
	f.fail("bootout", "Boot-out failed: 1: Operation not permitted", errors.New("exit status 1"))
	svc := &launchdService{home: "/Users/andy", uid: 501}
	if err := svc.Unload(f.run); err == nil {
		t.Fatal("Unload() = nil, want an error for a real bootout failure")
	}
}

// enable --now only starts; on a reinstall the unit is already running, so the
// old process would keep serving with the old --port/--bind. An explicit
// restart is what makes "re-run install to change flags" actually work.
func TestSystemdLoadRestartsAndOrdersLingerFirst(t *testing.T) {
	f := newFakeRunner()
	svc := &systemdService{home: "/home/andy", user: "andy"}
	if err := svc.Load(f.run); err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	want := []string{
		"loginctl enable-linger andy",
		"systemctl --user daemon-reload",
		"systemctl --user enable claude-sessions.service",
		"systemctl --user restart claude-sessions.service",
	}
	got := f.joined()
	if len(got) != len(want) {
		t.Fatalf("Load() ran %d commands %q, want %d %q", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("command %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// polkit can refuse linger on some hosts. That degrades the service to
// dying at logout — worth a warning, not worth failing the whole install. The
// warning is the entire user-visible payload of this path, so this captures
// serviceErr rather than just checking the call count: deleting both Fprintf
// calls must turn this red too, not just silently degrade the install.
func TestSystemdLoadSurvivesLingerRefusal(t *testing.T) {
	var buf bytes.Buffer
	orig := serviceErr
	serviceErr = &buf
	defer func() { serviceErr = orig }()

	f := newFakeRunner()
	f.fail("enable-linger", "Failed to enable linger: Access denied", errors.New("exit status 1"))
	svc := &systemdService{home: "/home/andy", user: "andy"}
	if err := svc.Load(f.run); err != nil {
		t.Fatalf("Load() error: %v, want nil — linger refusal is not fatal", err)
	}
	if len(f.calls) != 4 {
		t.Errorf("Load() ran %q, want the remaining systemctl commands to still run", f.joined())
	}
	warning := buf.String()
	if !strings.Contains(warning, "service: warning") {
		t.Errorf("warning output = %q, want it to contain %q", warning, "service: warning")
	}
	if !strings.Contains(warning, "andy") {
		t.Errorf("warning output = %q, want it to name the user %q", warning, "andy")
	}
}

func TestSystemdLoadFailsOnRestartError(t *testing.T) {
	f := newFakeRunner()
	f.fail("restart", "Job for claude-sessions.service failed", errors.New("exit status 1"))
	svc := &systemdService{home: "/home/andy", user: "andy"}
	err := svc.Load(f.run)
	if err == nil {
		t.Fatal("Load() = nil, want an error when restart fails")
	}
	if !strings.Contains(err.Error(), "systemctl --user restart") {
		t.Errorf("Load() error = %q, want it to name the failing command", err.Error())
	}
	if !strings.Contains(err.Error(), "Job for claude-sessions.service failed") {
		t.Errorf("Load() error = %q, want it to carry the scripted systemctl output", err.Error())
	}
}

func TestSystemdUnload(t *testing.T) {
	f := newFakeRunner()
	svc := &systemdService{home: "/home/andy", user: "andy"}
	if err := svc.Unload(f.run); err != nil {
		t.Fatalf("Unload() error: %v", err)
	}
	want := []string{
		"systemctl --user disable --now claude-sessions.service",
		"systemctl --user daemon-reload",
	}
	got := f.joined()
	if len(got) != len(want) {
		t.Fatalf("Unload() ran %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("command %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// systemctl exits nonzero for "disable" when the unit is already gone —
// e.g. a second `uninstall`, or one run after a half-finished install. That
// must not hard-fail the way it otherwise would: launchd's Unload already
// tolerates the equivalent "No such process" case, and systemd's isn't
// idempotent without the same treatment.
func TestSystemdUnloadTreatsAlreadyGoneAsSuccess(t *testing.T) {
	tests := []struct {
		name string
		out  string
	}{
		{name: "unit file missing", out: "Failed to disable unit: Unit file claude-sessions.service does not exist."},
		{name: "unit not loaded", out: "Unit claude-sessions.service not loaded."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeRunner()
			f.fail("disable", tt.out, errors.New("exit status 1"))
			svc := &systemdService{home: "/home/andy", user: "andy"}
			if err := svc.Unload(f.run); err != nil {
				t.Fatalf("Unload() error: %v, want nil — the unit is already gone", err)
			}
		})
	}
}

// A disable failure that isn't "already gone" is a real failure and must be
// reported, not swallowed alongside the idempotent case.
func TestSystemdUnloadReportsOtherFailures(t *testing.T) {
	f := newFakeRunner()
	f.fail("disable", "Failed to disable unit: Access denied", errors.New("exit status 1"))
	svc := &systemdService{home: "/home/andy", user: "andy"}
	if err := svc.Unload(f.run); err == nil {
		t.Fatal("Unload() = nil, want an error for a real disable failure")
	}
}

// launchdWantCmd is the exact argv Status must send launchctl — pinning it
// (rather than the substring "launchctl print" the fake matches on) is what
// makes a broken s.target() (e.g. dropping the gui/<uid> domain) show up as a
// failure instead of silently querying the wrong thing.
var launchdWantCmd = []string{"launchctl print gui/501/com.skerla.claude-sessions"}

func TestLaunchdStatus(t *testing.T) {
	// Modeled on real `launchctl print` output, which is much larger than a
	// bare "pid = N" line and full of near-miss keys (pid-local endpoints,
	// per-argument "pid" mentions inside arguments, an unrelated exit-code
	// field) that a looser regex could latch onto.
	running := `com.skerla.claude-sessions = {
	active count = 1
	pid-local endpoints = {
	}
	pid = 4242
	arguments = {
		/usr/local/bin/claude-sessions
		-s
	}
	state = running
	last exit code = 0
}`
	tests := []struct {
		name    string
		out     string
		err     error
		want    serviceStatus
		wantErr bool
	}{
		{
			name: "running reports its pid",
			out:  running,
			want: serviceStatus{Installed: true, Running: true, PID: 4242},
		},
		{
			name: "loaded but not running",
			out:  "com.skerla.claude-sessions = {\n\tstate = not running\n}",
			want: serviceStatus{Installed: true},
		},
		{
			name: "pid = 0 is not a running pid",
			out:  "com.skerla.claude-sessions = {\n\tpid = 0\n\tstate = not running\n}",
			want: serviceStatus{Installed: true},
		},
		{
			name: "not loaded",
			out:  "Could not find service \"com.skerla.claude-sessions\" in domain for port",
			err:  errors.New("exit status 113"),
			want: serviceStatus{},
		},
		{
			// The real-world case the fix in this round exists for: `service
			// status` over ssh to a Mac with nobody logged in at the
			// console. launchctl fails, but not because the service is
			// absent — it couldn't even ask.
			name:    "could not even ask — no GUI session",
			out:     "Could not find domain for gui/501",
			err:     errors.New("exit status 113"),
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeRunner()
			f.fail(launchdWantCmd[0], tt.out, tt.err)
			svc := &launchdService{home: "/Users/andy", uid: 501}
			got, err := svc.Status(f.run)
			if tt.wantErr {
				if err == nil {
					t.Fatal("Status() error = nil, want an error — launchctl couldn't answer at all, that is not \"not installed\"")
				}
				return
			}
			if err != nil {
				t.Fatalf("Status() error: %v", err)
			}
			if got != tt.want {
				t.Errorf("Status() = %+v, want %+v", got, tt.want)
			}
			if joined := f.joined(); len(joined) != len(launchdWantCmd) || joined[0] != launchdWantCmd[0] {
				t.Errorf("commands = %q, want %q", joined, launchdWantCmd)
			}
		})
	}
}

// systemdWantCmd is the exact argv Status must send systemctl. Pinning it
// (rather than the substring "systemctl" the fake matches on) is what makes
// dropping --user show up as a failure instead of silently querying the
// system manager, where a --user unit always reads LoadState=not-found.
var systemdWantCmd = []string{"systemctl --user show claude-sessions.service --property=LoadState --property=ActiveState --property=MainPID"}

func TestSystemdStatus(t *testing.T) {
	tests := []struct {
		name    string
		out     string
		err     error
		want    serviceStatus
		wantErr bool
	}{
		{
			name: "active reports its pid",
			out:  "LoadState=loaded\nActiveState=active\nMainPID=4242\n",
			want: serviceStatus{Installed: true, Running: true, PID: 4242},
		},
		{
			name: "loaded but inactive",
			out:  "LoadState=loaded\nActiveState=inactive\nMainPID=0\n",
			want: serviceStatus{Installed: true},
		},
		{
			// systemd's documented "no main process yet/anymore" state.
			// Running is still true; PID is the literal 0, not suppressed.
			name: "active with MainPID=0 is still running",
			out:  "LoadState=loaded\nActiveState=active\nMainPID=0\n",
			want: serviceStatus{Installed: true, Running: true, PID: 0},
		},
		{
			name: "not found",
			out:  "LoadState=not-found\nActiveState=inactive\nMainPID=0\n",
			want: serviceStatus{},
		},
		{
			// The systemd analog of the launchd "no GUI session" case: no
			// user D-Bus session yet, so systemctl can't even reach the
			// manager to ask. Must not read as "not installed".
			name:    "could not even ask — no user bus",
			out:     "Failed to connect to bus: No medium found\n",
			err:     errors.New("exit status 1"),
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeRunner()
			f.fail(systemdWantCmd[0], tt.out, tt.err)
			svc := &systemdService{home: "/home/andy", user: "andy"}
			got, err := svc.Status(f.run)
			if tt.wantErr {
				if err == nil {
					t.Fatal("Status() error = nil, want an error — systemctl couldn't answer at all, that is not \"not installed\"")
				}
				return
			}
			if err != nil {
				t.Fatalf("Status() error: %v", err)
			}
			if got != tt.want {
				t.Errorf("Status() = %+v, want %+v", got, tt.want)
			}
			if joined := f.joined(); len(joined) != len(systemdWantCmd) || joined[0] != systemdWantCmd[0] {
				t.Errorf("commands = %q, want %q", joined, systemdWantCmd)
			}
		})
	}
}

// 2 is this repo's usage-error code (main.go:60). Overloading it would make a
// typo'd flag indistinguishable from a real answer, so not-installed is 3.
func TestServiceStatusExitCode(t *testing.T) {
	tests := []struct {
		name string
		st   serviceStatus
		want int
	}{
		{name: "running", st: serviceStatus{Installed: true, Running: true, PID: 1}, want: 0},
		{name: "installed but stopped", st: serviceStatus{Installed: true}, want: 1},
		{name: "not installed", st: serviceStatus{}, want: 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.st.exitCode(); got != tt.want {
				t.Errorf("exitCode() = %d, want %d", got, tt.want)
			}
		})
	}
}

// newServiceManagerFor's three branches (darwin, linux, unsupported) and the
// home/username plumbing through them are otherwise unasserted — replacing
// the whole function body with `return nil, nil` leaves the rest of the
// suite green.
func TestNewServiceManagerFor(t *testing.T) {
	tests := []struct {
		name    string
		goos    string
		home    string
		user    string
		wantErr bool
	}{
		{name: "darwin", goos: "darwin", home: "/Users/andy", user: ""},
		{name: "linux", goos: "linux", home: "/home/andy", user: "andy"},
		{name: "unsupported platform names both supported ones", goos: "windows", home: "/tmp", user: "andy", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := newServiceManagerFor(tt.goos, tt.home, tt.user)
			if tt.wantErr {
				if err == nil {
					t.Fatal("newServiceManagerFor() error = nil, want an error")
				}
				if !strings.Contains(err.Error(), "macOS") || !strings.Contains(err.Error(), "Linux") {
					t.Errorf("error %q does not name both supported platforms", err)
				}
				if !strings.Contains(err.Error(), tt.goos) {
					t.Errorf("error %q does not name the unsupported GOOS %q", err, tt.goos)
				}
				return
			}
			if err != nil {
				t.Fatalf("newServiceManagerFor() error: %v", err)
			}
			switch tt.goos {
			case "darwin":
				svc, ok := got.(*launchdService)
				if !ok {
					t.Fatalf("got %T, want *launchdService", got)
				}
				if svc.home != tt.home {
					t.Errorf("home = %q, want %q", svc.home, tt.home)
				}
				if svc.uid != os.Getuid() {
					t.Errorf("uid = %d, want %d (this process's uid)", svc.uid, os.Getuid())
				}
			case "linux":
				svc, ok := got.(*systemdService)
				if !ok {
					t.Fatalf("got %T, want *systemdService", got)
				}
				if svc.home != tt.home {
					t.Errorf("home = %q, want %q", svc.home, tt.home)
				}
				if svc.user != tt.user {
					t.Errorf("user = %q, want %q", svc.user, tt.user)
				}
			}
		})
	}
}

// Every case here must return 2 before reaching a verb body. cmdService still
// routes through newManager()/execRunner rather than a hardcoded
// newServiceManager() (see newManager's own comment) precisely because a
// fifth case that got further than parseServerFlags/len(args)==0 here would
// shell out to the real launchctl/systemctl against this process's real home
// directory — which is exactly how this feature's manual verification
// installed a real service the first time around. Any new case added to this
// table must keep returning 2 before a verb body runs, or must swap
// newManager for a test double first.
func TestCmdServiceUsageErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "no verb", args: nil},
		{name: "unknown verb", args: []string{"reload"}},
		{name: "unknown flag on install", args: []string{"install", "--verbose"}},
		{name: "flags on uninstall", args: []string{"uninstall", "--port", "1"}},
		{name: "flags on restart", args: []string{"restart", "--port", "1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cmdService(tt.args); got != 2 {
				t.Errorf("cmdService(%q) = %d, want 2 (usage error)", tt.args, got)
			}
		})
	}
}

// cmdService must actually route through newManager, not merely have the var
// exist unused — otherwise overriding it in a test (or ever needing to) would
// have no effect, and cmdService would still touch the real platform manager.
func TestCmdServiceUsesInjectedManager(t *testing.T) {
	orig := newManager
	newManager = func() (serviceManager, error) { return nil, errors.New("boom") }
	defer func() { newManager = orig }()

	if got := cmdService([]string{"status"}); got != 1 {
		t.Errorf("cmdService([status]) = %d, want 1 when newManager fails", got)
	}
}

// A free port must not read as occupied — otherwise install refuses for no
// reason on a clean box.
func TestPortInUse(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port

	occupied, err := portInUse("127.0.0.1", port)
	if err != nil {
		t.Fatalf("portInUse(127.0.0.1, %d) error = %v, want nil while listening", port, err)
	}
	if !occupied {
		t.Errorf("portInUse(127.0.0.1, %d) = false while listening, want true", port)
	}
	ln.Close()
	occupied, err = portInUse("127.0.0.1", port)
	if err != nil {
		t.Fatalf("portInUse(127.0.0.1, %d) error = %v, want nil after close", port, err)
	}
	if occupied {
		t.Errorf("portInUse(127.0.0.1, %d) = true after close, want false", port)
	}
}

// "tailscale" is a magic bind value resolved at server start, not an address.
// The pre-flight check must skip it rather than trying to listen on it.
func TestPortInUseSkipsUnresolvableBind(t *testing.T) {
	occupied, err := portInUse("tailscale", 8765)
	if err != nil {
		t.Errorf("portInUse(tailscale, ...) error = %v, want nil", err)
	}
	if occupied {
		t.Error("portInUse(tailscale, ...) = true, want false — the literal isn't an address")
	}
}

// An IPv6 literal is a real address whether or not the user bracketed it, so
// the pre-flight must actually run for both spellings. Using a bare ParseIP
// here instead of unbracket would silently skip the check for `[::]` — a bind
// cmdServer itself accepts, via the same helpers this now shares with it.
func TestPortInUseRunsForBracketedIPv6(t *testing.T) {
	ln, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback on this host: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	for _, bind := range []string{"::1", "[::1]"} {
		occupied, err := portInUse(bind, port)
		if err != nil {
			t.Errorf("portInUse(%q, %d) error = %v, want nil", bind, port, err)
			continue
		}
		if !occupied {
			t.Errorf("portInUse(%q, %d) = false, want true — the pre-flight skipped a real address", bind, port)
		}
	}
}

// portOccupied's classification: only a real EADDRINUSE means "occupied".
// EACCES (privileged port, non-root) and EADDRNOTAVAIL (--bind address not on
// this host) describe no listener to stop at all, and folding them into
// "already listening ... stop it first" would name a process that does not
// exist. Synthetic errno values, not genuine OS conditions, because EACCES
// and EADDRNOTAVAIL depend on privilege and interface configuration this
// suite can't portably control — see TestPortInUseDoesNotTreatPermissionDeniedAsOccupied
// for the one genuine-condition case that's safe to rely on everywhere.
func TestPortOccupied(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "bare EADDRINUSE", err: syscall.EADDRINUSE, want: true},
		{
			name: "EADDRINUSE wrapped the way net.Listen actually wraps it",
			err:  &net.OpError{Op: "listen", Err: &os.SyscallError{Syscall: "bind", Err: syscall.EADDRINUSE}},
			want: true,
		},
		{name: "EACCES is not occupied", err: syscall.EACCES, want: false},
		{name: "EADDRNOTAVAIL is not occupied", err: syscall.EADDRNOTAVAIL, want: false},
		{name: "a plain non-errno error is not occupied", err: errors.New("invalid port"), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := portOccupied(tt.err); got != tt.want {
				t.Errorf("portOccupied(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// portOccupied must also classify the real error shape net.Listen hands back
// for a genuine double bind, not just the hand-constructed *net.OpError
// above — this is what would break silently if net's own error wrapping ever
// changed.
func TestPortOccupiedOnRealDoubleBind(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	_, dialErr := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if dialErr == nil {
		t.Fatal("second Listen on an already-bound port succeeded, want EADDRINUSE")
	}
	if !portOccupied(dialErr) {
		t.Errorf("portOccupied(%v) = false, want true for a real double-bind error", dialErr)
	}
}

// A privileged --port fails for a completely different reason than "already
// listening" — this must not read as occupied, or the error message would
// send the operator to kill a process that was never there. Binding port 1
// needs root on both macOS and Linux, so this is a genuine EACCES condition
// this suite CAN rely on: `go test` runs unprivileged in the normal case.
func TestPortInUseDoesNotTreatPermissionDeniedAsOccupied(t *testing.T) {
	ln, listenErr := net.Listen("tcp", "127.0.0.1:1")
	if listenErr == nil {
		ln.Close()
		t.Skip("running with permission to bind privileged ports (root?); this check needs an unprivileged process")
	}

	occupied, err := portInUse("127.0.0.1", 1)
	if occupied {
		t.Error("portInUse(127.0.0.1, 1) = true, want false — EACCES is not \"occupied\"")
	}
	if err == nil {
		t.Error("portInUse(127.0.0.1, 1) = nil error, want the permission error surfaced")
	}
}

// If cfg.validate() didn't run before Render/WriteFile, a newline sneaking in
// via --bind (parseServerFlags stores it verbatim) would end the unit file's
// directive line, and whatever follows becomes a fresh directive that
// launchd runs at login. This proves the whole install refuses to write
// anything — not merely that it reports an error — which is the property a
// future refactor could silently break while still returning nonzero.
func TestServiceInstallRejectsNewlineInBind(t *testing.T) {
	orig := resolveBin
	resolveBin = func() (string, error) { return "/usr/local/bin/claude-sessions", nil }
	defer func() { resolveBin = orig }()

	var buf bytes.Buffer
	origErr := serviceErr
	serviceErr = &buf
	defer func() { serviceErr = origErr }()

	svc := &launchdService{home: t.TempDir(), uid: 501}
	f := newFakeRunner()

	rc := serviceInstall(svc, f.run, []string{"--bind", "127.0.0.1\nExecStartPre=/bin/sh -c evil"})
	if rc != 1 {
		t.Fatalf("serviceInstall() = %d, want 1 (validate should reject the newline)", rc)
	}
	if !strings.Contains(buf.String(), "newline") {
		t.Errorf("error output = %q, want it to mention the newline validate() rejected", buf.String())
	}
	if _, err := os.Stat(svc.UnitPath()); !os.IsNotExist(err) {
		t.Fatalf("unit file exists at %s after a rejected config, want nothing written", svc.UnitPath())
	}
	// serviceInstall's own pre-flight Status() call (item 1 of this round's
	// fixes) legitimately runs before validate() rejects the config — so
	// assert Load specifically never ran (bootout/bootstrap), not that zero
	// commands ran at all.
	for _, c := range f.joined() {
		if strings.Contains(c, "bootout") || strings.Contains(c, "bootstrap") {
			t.Errorf("Load ran %q after a rejected config, want it never called — the unit was never even written", f.joined())
			break
		}
	}
}

// A default install binds 127.0.0.1:8765 and keeps running. Re-running
// install — the documented way to pick up a new binary after `make install`
// (resolveBinPath's own doc comment) or to change flags (systemdService.Load
// uses `restart` rather than `enable --now` for exactly this reason) — must
// not read its own listener as a foreign one to refuse. This proves the
// actual conflict case: Status reporting Running:true AND a real listener
// present on the target port, so the port pre-flight is provably skipped, not
// merely lucky because nothing happened to be listening.
func TestServiceInstallSkipsPortCheckWhenAlreadyRunning(t *testing.T) {
	orig := resolveBin
	resolveBin = func() (string, error) { return "/usr/local/bin/claude-sessions", nil }
	defer func() { resolveBin = orig }()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	svc := &launchdService{home: t.TempDir(), uid: 501}
	f := newFakeRunner()
	// Scripted onto "launchctl print" specifically (the Status call), not
	// "launchctl" generally, so it can't accidentally also answer Load's
	// bootout/bootstrap calls.
	f.fail("launchctl print", "com.skerla.claude-sessions = {\n\tpid = 4242\n\tstate = running\n}", nil)

	rc := serviceInstall(svc, f.run, []string{"--port", strconv.Itoa(port)})
	if rc != 0 {
		t.Fatalf("serviceInstall() = %d, want 0 — reinstalling over our own running instance must succeed", rc)
	}
	if _, err := os.Stat(svc.UnitPath()); err != nil {
		t.Errorf("unit file not written: %v", err)
	}
}

// A bind failure that isn't EADDRINUSE (here: a privileged port, which is
// portable to test unprivileged — see TestPortInUseDoesNotTreatPermissionDeniedAsOccupied)
// must be reported for what it is, not folded into "already listening ...
// stop it first", which names a process that was never there. Also confirms
// the unit file is never written when the pre-flight itself errors.
func TestServiceInstallReportsNonOccupiedPortErrorsVerbatim(t *testing.T) {
	ln, listenErr := net.Listen("tcp", "127.0.0.1:1")
	if listenErr == nil {
		ln.Close()
		t.Skip("running with permission to bind privileged ports (root?); this check needs an unprivileged process")
	}

	orig := resolveBin
	resolveBin = func() (string, error) { return "/usr/local/bin/claude-sessions", nil }
	defer func() { resolveBin = orig }()

	var buf bytes.Buffer
	origErr := serviceErr
	serviceErr = &buf
	defer func() { serviceErr = origErr }()

	svc := &launchdService{home: t.TempDir(), uid: 501}
	f := newFakeRunner()

	rc := serviceInstall(svc, f.run, []string{"--port", "1"})
	if rc != 1 {
		t.Fatalf("serviceInstall() = %d, want 1", rc)
	}
	msg := buf.String()
	if strings.Contains(msg, "already listening") {
		t.Errorf("output = %q, want it NOT to claim something is already listening — this is a permission error, not a busy port", msg)
	}
	if _, err := os.Stat(svc.UnitPath()); !os.IsNotExist(err) {
		t.Error("unit file was written despite the port pre-flight itself failing")
	}
}

// A Load failure must not discard the unit file's location or the concrete
// recovery command (manualLoadHint) — the "wrote" line naming the path went
// to stdout, so a script capturing only stderr never saw it.
func TestServiceInstallReportsRecoveryOnLoadFailure(t *testing.T) {
	orig := resolveBin
	resolveBin = func() (string, error) { return "/usr/local/bin/claude-sessions", nil }
	defer func() { resolveBin = orig }()

	var buf bytes.Buffer
	origErr := serviceErr
	serviceErr = &buf
	defer func() { serviceErr = origErr }()

	svc := &launchdService{home: t.TempDir(), uid: 501}
	f := newFakeRunner()
	f.fail("bootstrap", "Bootstrap failed: 5: Input/output error", errors.New("exit status 5"))
	// Status reports Running, which is the documented reinstall path and
	// skips the port pre-flight by design (see
	// TestServiceInstallSkipsPortCheckWhenAlreadyRunning). That is what keeps
	// this test off the network entirely — it must never bind, or contend
	// with, a port on the machine running the suite. Scripted onto "launchctl
	// print" specifically so it can't also answer Load's bootstrap calls; the
	// GUI-session probe Load falls back to is a `launchctl print gui/501`
	// without a service, so it matches this rule too and reports the same
	// running transcript, meaning Load's error stays the plain bootstrap
	// failure rather than the GUI diagnosis.
	f.fail("launchctl print", "com.skerla.claude-sessions = {\n\tpid = 4242\n\tstate = running\n}", nil)

	rc := serviceInstall(svc, f.run, []string{"--port", "8765"})
	if rc != 1 {
		t.Fatalf("serviceInstall() = %d, want 1", rc)
	}
	msg := buf.String()
	if !strings.Contains(msg, svc.UnitPath()) {
		t.Errorf("error output = %q, want it to name the unit path %q", msg, svc.UnitPath())
	}
	if !strings.Contains(msg, svc.manualLoadHint()) {
		t.Errorf("error output = %q, want it to contain the manual load command %q", msg, svc.manualLoadHint())
	}
	if _, err := os.Stat(svc.UnitPath()); err != nil {
		t.Errorf("unit file should remain on disk after a Load failure: %v", err)
	}
}

// Task 6's Status distinguishes "the service is absent" (serviceStatus{}, nil)
// from "I could not ask" (an error) — the latter happens over ssh to a Mac
// with no console session. That error must surface as exit 1, not fall
// through to the not-installed exit code 3. Uses the real launchdService
// against a fakeRunner scripted with the actual transcript from
// TestLaunchdStatus's "could not even ask" case, so this exercises the
// production Status() path rather than a stand-in that just hands back an
// error.
func TestServiceStatusCmdSurfacesStatusError(t *testing.T) {
	f := newFakeRunner()
	f.fail(launchdWantCmd[0], "Could not find domain for gui/501", errors.New("exit status 113"))
	svc := &launchdService{home: t.TempDir(), uid: 501}

	rc := serviceStatusCmd(svc, f.run, nil)
	if rc != 1 {
		t.Errorf("serviceStatusCmd() = %d, want 1 (a Status error is not \"not installed\")", rc)
	}
}

// systemd's MainPID=0 with ActiveState=active is a real, documented state —
// Task 6's Status returns Running:true, PID:0 for it, not a parse failure.
// Printing "running yes (pid 0)" reads as a strange half-alive process; the
// pid clause must be omitted when PID is 0 and kept when it isn't. Drives
// this through the real systemdService + fakeRunner (the same shape
// TestSystemdStatus already covers) so it also protects Status() → command
// wiring, not just a formatting helper in isolation.
func TestServiceStatusCmdOmitsPidZero(t *testing.T) {
	tests := []struct {
		name       string
		out        string
		want       string
		wantAbsent string
	}{
		{
			name:       "pid zero omits the clause",
			out:        "LoadState=loaded\nActiveState=active\nMainPID=0\n",
			want:       "running yes\n",
			wantAbsent: "running yes (pid 0)",
		},
		{
			name: "nonzero pid still shown",
			out:  "LoadState=loaded\nActiveState=active\nMainPID=4242\n",
			want: "running yes (pid 4242)\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			orig := serviceOut
			serviceOut = &buf
			defer func() { serviceOut = orig }()

			f := newFakeRunner()
			f.fail(systemdWantCmd[0], tt.out, nil)
			svc := &systemdService{home: t.TempDir(), user: "andy"}

			if rc := serviceStatusCmd(svc, f.run, nil); rc != 0 {
				t.Fatalf("serviceStatusCmd() = %d, want 0 (running)", rc)
			}
			if !strings.Contains(buf.String(), tt.want) {
				t.Errorf("output = %q, want it to contain %q", buf.String(), tt.want)
			}
			if tt.wantAbsent != "" && strings.Contains(buf.String(), tt.wantAbsent) {
				t.Errorf("output = %q, want it NOT to contain %q", buf.String(), tt.wantAbsent)
			}
		})
	}
}

// The linger note is a systemd-only concept and must come from the manager
// (usesLinger), not runtime.GOOS — otherwise a *systemdService driven from a
// macOS dev box, which is exactly what every Render/Status test in this file
// already does, could never exercise it. Constructing a *systemdService here
// regardless of the host this test suite happens to run on is the point.
func TestServiceUninstallPrintsLingerNoteForSystemdRegardlessOfHostOS(t *testing.T) {
	var buf bytes.Buffer
	orig := serviceOut
	serviceOut = &buf
	defer func() { serviceOut = orig }()

	svc := &systemdService{home: t.TempDir(), user: "andy"}
	f := newFakeRunner()
	if rc := serviceUninstall(svc, f.run, nil); rc != 0 {
		t.Fatalf("serviceUninstall() = %d, want 0", rc)
	}
	if !strings.Contains(buf.String(), "linger") {
		t.Errorf("output = %q, want it to mention linger for a systemd manager", buf.String())
	}
}

// launchd has no linger concept; the note must not appear for it regardless
// of the host this test runs on.
func TestServiceUninstallOmitsLingerNoteForLaunchd(t *testing.T) {
	var buf bytes.Buffer
	orig := serviceOut
	serviceOut = &buf
	defer func() { serviceOut = orig }()

	svc := &launchdService{home: t.TempDir(), uid: 501}
	f := newFakeRunner()
	if rc := serviceUninstall(svc, f.run, nil); rc != 0 {
		t.Fatalf("serviceUninstall() = %d, want 0", rc)
	}
	if strings.Contains(buf.String(), "linger") {
		t.Errorf("output = %q, want no linger mention for a launchd manager", buf.String())
	}
}

// The range boundaries themselves, as a pure function — the exit-code and
// no-side-effects half lives in TestServiceInstallRejectsPortOutOfRange.
func TestValidateServicePort(t *testing.T) {
	tests := []struct {
		name    string
		port    int
		wantErr bool
	}{
		{name: "zero means pick any free port, useless in a unit file", port: 0, wantErr: true},
		{name: "lowest usable port", port: 1},
		{name: "a normal port", port: 8765},
		{name: "highest usable port", port: 65535},
		{name: "one past the top of the range", port: 65536, wantErr: true},
		{name: "a typo that overflows the range", port: 87655, wantErr: true},
		{name: "negative", port: -1, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateServicePort(tt.port)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("validateServicePort(%d) = nil, want an error", tt.port)
				}
				if !strings.Contains(err.Error(), strconv.Itoa(tt.port)) {
					t.Errorf("validateServicePort(%d) error = %q, want it to name the rejected value", tt.port, err)
				}
				return
			}
			if err != nil {
				t.Errorf("validateServicePort(%d) = %v, want nil", tt.port, err)
			}
		})
	}
}

// A one-character typo (`--port 87655`) must not boot out a working service and
// install a unit that crash-loops under KeepAlive, and `--port 0` must not
// install a service that binds a different ephemeral port on every restart.
// Neither is caught downstream: the portInUse pre-flight is skipped when --bind
// isn't an address (`--bind tailscale`, the recommended setup) and again when
// Status reports the service already running (the documented reinstall path) —
// which is exactly the state scripted here, so a rejected port is provably
// rejected by the range check and not by a lucky bind failure.
//
// Exit 2, not 1: this is a usage error, the same code main.go uses for one
// everywhere else.
func TestServiceInstallRejectsPortOutOfRange(t *testing.T) {
	tests := []struct {
		name       string
		port       string
		wantReject bool
	}{
		{name: "zero", port: "0", wantReject: true},
		{name: "lowest usable port", port: "1"},
		{name: "highest usable port", port: "65535"},
		{name: "one past the top of the range", port: "65536", wantReject: true},
		{name: "a plausible typo", port: "87655", wantReject: true},
		{name: "negative", port: "-1", wantReject: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			orig := resolveBin
			resolveBin = func() (string, error) { return "/usr/local/bin/claude-sessions", nil }
			defer func() { resolveBin = orig }()

			var errBuf, outBuf bytes.Buffer
			origErr, origOut := serviceErr, serviceOut
			serviceErr, serviceOut = &errBuf, &outBuf
			defer func() { serviceErr, serviceOut = origErr, origOut }()

			svc := &launchdService{home: t.TempDir(), uid: 501}
			f := newFakeRunner()
			// Status says we're already running, so the port pre-flight is
			// skipped by design and nothing in this test ever touches the
			// network — an accepted port is never actually bound.
			f.fail("launchctl print", "com.skerla.claude-sessions = {\n\tpid = 4242\n\tstate = running\n}", nil)

			rc := serviceInstall(svc, f.run, []string{"--port", tt.port})
			if !tt.wantReject {
				if rc != 0 {
					t.Fatalf("serviceInstall(--port %s) = %d, want 0 — %s is a usable port\nstderr: %s", tt.port, rc, tt.port, errBuf.String())
				}
				return
			}
			if rc != 2 {
				t.Fatalf("serviceInstall(--port %s) = %d, want 2 (usage error)", tt.port, rc)
			}
			if msg := errBuf.String(); !strings.Contains(msg, tt.port) {
				t.Errorf("error output = %q, want it to name the rejected port %q", msg, tt.port)
			}
			// The whole point is that nothing is disturbed: no unit written,
			// and above all no bootout tearing down a service that was working
			// a moment ago.
			if _, err := os.Stat(svc.UnitPath()); !os.IsNotExist(err) {
				t.Errorf("unit file exists at %s after a rejected port, want nothing written", svc.UnitPath())
			}
			if len(f.calls) != 0 {
				t.Errorf("serviceInstall ran %q, want no commands at all — the running service must not be booted out", f.joined())
			}
		})
	}
}

// systemd caches a unit until a daemon-reload. Unload's own reload runs while
// the file is still on disk, so it re-reads the unit rather than forgetting
// it: without a second reload after the file is removed, `service status` run
// straight after `service uninstall` prints "file no / loaded yes" and exits 1
// instead of 3. The reload must come last, hence the exact ordering assertion
// rather than a "contains daemon-reload" check.
//
// launchd needs no such hook and must not grow a spurious command — pinning
// both backends here is what keeps the fix on the manager (afterRemove) rather
// than in a runtime.GOOS branch inside serviceUninstall.
func TestServiceUninstallCommandOrder(t *testing.T) {
	tests := []struct {
		name string
		mgr  serviceManager
		want []string
	}{
		{
			name: "launchd",
			mgr:  &launchdService{home: t.TempDir(), uid: 501},
			want: []string{"launchctl bootout gui/501/com.skerla.claude-sessions"},
		},
		{
			name: "systemd",
			mgr:  &systemdService{home: t.TempDir(), user: "andy"},
			want: []string{
				"systemctl --user disable --now claude-sessions.service",
				// Unload's reload, with the unit file still present.
				"systemctl --user daemon-reload",
				// afterRemove's reload, with the file gone — the one that
				// actually drops the unit from systemd's cache.
				"systemctl --user daemon-reload",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			orig := serviceOut
			serviceOut = &buf
			defer func() { serviceOut = orig }()

			// A real unit file on disk, so the removal this ordering is about
			// is a removal and not a no-op.
			unitPath := tt.mgr.UnitPath()
			if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
				t.Fatalf("MkdirAll: %v", err)
			}
			if err := os.WriteFile(unitPath, []byte("placeholder\n"), 0o644); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}

			f := newFakeRunner()
			if rc := serviceUninstall(tt.mgr, f.run, nil); rc != 0 {
				t.Fatalf("serviceUninstall() = %d, want 0", rc)
			}
			if _, err := os.Stat(unitPath); !os.IsNotExist(err) {
				t.Errorf("unit file still at %s after uninstall", unitPath)
			}
			got := f.joined()
			if len(got) != len(tt.want) {
				t.Fatalf("serviceUninstall ran %d commands %q, want %d %q", len(got), got, len(tt.want), tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Errorf("command %d = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// serviceRestart must reload through mgr.Load — the same bootout+bootstrap or
// daemon-reload+enable+restart sequence serviceInstall uses — without ever
// touching the unit file. No WriteFile happens here (unlike
// TestServiceUninstallCommandOrder, which pre-seeds a real file): if restart
// re-rendered the unit, this test would still pass, but a nil-config render
// would crash, and it doesn't.
func TestServiceRestartCommandOrder(t *testing.T) {
	t.Run("launchd", func(t *testing.T) {
		var buf bytes.Buffer
		orig := serviceOut
		serviceOut = &buf
		defer func() { serviceOut = orig }()

		mgr := &launchdService{home: t.TempDir(), uid: 501}
		want := []string{
			"launchctl print gui/501/com.skerla.claude-sessions", // Status probe
			"launchctl bootout gui/501/com.skerla.claude-sessions",
			"launchctl bootstrap gui/501 " + mgr.UnitPath(),
		}

		f := newFakeRunner()
		if rc := serviceRestart(mgr, f.run, nil); rc != 0 {
			t.Fatalf("serviceRestart() = %d, want 0", rc)
		}
		if got := f.joined(); !stringSlicesEqual(got, want) {
			t.Errorf("commands = %q, want %q", got, want)
		}
		if !strings.Contains(buf.String(), mgr.Label()) {
			t.Errorf("output = %q, want it to mention %q", buf.String(), mgr.Label())
		}
	})

	t.Run("systemd", func(t *testing.T) {
		var buf bytes.Buffer
		orig := serviceOut
		serviceOut = &buf
		defer func() { serviceOut = orig }()

		mgr := &systemdService{home: t.TempDir(), user: "andy"}
		want := []string{
			systemdWantCmd[0], // Status probe
			"loginctl enable-linger andy",
			"systemctl --user daemon-reload",
			"systemctl --user enable claude-sessions.service",
			"systemctl --user restart claude-sessions.service",
		}

		f := newFakeRunner()
		f.fail(systemdWantCmd[0], "LoadState=loaded\nActiveState=active\nMainPID=4242\n", nil)
		if rc := serviceRestart(mgr, f.run, nil); rc != 0 {
			t.Fatalf("serviceRestart() = %d, want 0", rc)
		}
		if got := f.joined(); !stringSlicesEqual(got, want) {
			t.Errorf("commands = %q, want %q", got, want)
		}
		if !strings.Contains(buf.String(), mgr.Label()) {
			t.Errorf("output = %q, want it to mention %q", buf.String(), mgr.Label())
		}
	})
}

// Restarting a service that was never installed must fail rather than
// silently install one — that is `install`'s job, and its own doc comment on
// serviceRestart is explicit about not re-rendering the unit.
func TestServiceRestartFailsWhenNotInstalled(t *testing.T) {
	t.Run("launchd", func(t *testing.T) {
		var errBuf bytes.Buffer
		orig := serviceErr
		serviceErr = &errBuf
		defer func() { serviceErr = orig }()

		mgr := &launchdService{home: t.TempDir(), uid: 501}
		f := newFakeRunner()
		f.fail("launchctl print", "Could not find service", errors.New("exit status 1"))
		if rc := serviceRestart(mgr, f.run, nil); rc != 1 {
			t.Fatalf("serviceRestart() = %d, want 1", rc)
		}
		if len(f.joined()) != 1 {
			t.Errorf("commands = %q, want only the Status probe — not-installed must not call Load", f.joined())
		}
		if !strings.Contains(errBuf.String(), "not installed") {
			t.Errorf("stderr = %q, want it to mention \"not installed\"", errBuf.String())
		}
	})

	t.Run("systemd", func(t *testing.T) {
		var errBuf bytes.Buffer
		orig := serviceErr
		serviceErr = &errBuf
		defer func() { serviceErr = orig }()

		mgr := &systemdService{home: t.TempDir(), user: "andy"}
		f := newFakeRunner()
		f.fail(systemdWantCmd[0], "LoadState=not-found\nActiveState=inactive\nMainPID=0\n", nil)
		if rc := serviceRestart(mgr, f.run, nil); rc != 1 {
			t.Fatalf("serviceRestart() = %d, want 1", rc)
		}
		if len(f.joined()) != 1 {
			t.Errorf("commands = %q, want only the Status probe — not-installed must not call Load", f.joined())
		}
		if !strings.Contains(errBuf.String(), "not installed") {
			t.Errorf("stderr = %q, want it to mention \"not installed\"", errBuf.String())
		}
	})
}

// A failing post-remove reload leaves systemd reporting a unit that no longer
// exists — the exact condition afterRemove exists to prevent — so it must
// surface, not be swallowed into a zero exit.
func TestServiceUninstallReportsAfterRemoveFailure(t *testing.T) {
	var outBuf, errBuf bytes.Buffer
	origOut, origErr := serviceOut, serviceErr
	serviceOut, serviceErr = &outBuf, &errBuf
	defer func() { serviceOut, serviceErr = origOut, origErr }()

	svc := &systemdService{home: t.TempDir(), user: "andy"}
	f := newFakeRunner()
	// Only the second daemon-reload (afterRemove's) fails; Unload's succeeds,
	// so this can't pass by way of Unload's own error path.
	f.scripts = append(f.scripts, &fakeScript{substr: "daemon-reload", result: fakeResult{nil, nil}, remain: 1})
	f.fail("daemon-reload", "Failed to connect to bus: No medium found", errors.New("exit status 1"))

	if rc := serviceUninstall(svc, f.run, nil); rc != 1 {
		t.Fatalf("serviceUninstall() = %d, want 1 when the post-remove reload fails", rc)
	}
	if !strings.Contains(errBuf.String(), "daemon-reload") {
		t.Errorf("error output = %q, want it to name the failing reload", errBuf.String())
	}
}

// The hint is what an operator runs by hand after a failed install, so it has
// to load the unit the same way Load does. `enable --now` does not: --now only
// starts, and starting an already-running unit is a no-op, which leaves the
// old process serving the old flags. Derived from Load's actual command
// sequence rather than a hardcoded string, so the two cannot drift apart
// again.
func TestSystemdManualLoadHintMatchesLoad(t *testing.T) {
	f := newFakeRunner()
	svc := &systemdService{home: "/home/andy", user: "andy"}
	if err := svc.Load(f.run); err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	hint := svc.manualLoadHint()
	for _, cmd := range f.joined() {
		// linger is a standing grant, not part of loading the unit, and Load
		// only warns when it fails — the hint deliberately omits it.
		if strings.HasPrefix(cmd, "loginctl") {
			continue
		}
		if !strings.Contains(hint, cmd) {
			t.Errorf("manualLoadHint() = %q, missing Load's step %q", hint, cmd)
		}
	}
	if strings.Contains(hint, "--now") {
		t.Errorf("manualLoadHint() = %q, must not recommend `enable --now` — it will not replace a running process", hint)
	}
}

// An unsupported GOOS makes newManager fail, but a mistyped verb is a usage
// error on every platform. Validating the verb only after the manager is built
// reports the platform error (exit 1) for `service typo`, hiding the typo.
func TestCmdServiceRejectsUnknownVerbBeforeBuildingManager(t *testing.T) {
	var buf bytes.Buffer
	origErr := serviceErr
	serviceErr = &buf
	defer func() { serviceErr = origErr }()

	built := false
	orig := newManager
	newManager = func() (serviceManager, error) {
		built = true
		return nil, errors.New("service management is only supported on macOS and Linux (this is windows)")
	}
	defer func() { newManager = orig }()

	if got := cmdService([]string{"typo"}); got != 2 {
		t.Errorf("cmdService([typo]) = %d, want 2 (usage error) even where the platform is unsupported", got)
	}
	if built {
		t.Error("cmdService built a manager before validating the verb")
	}
}

// `service` is a scriptable subcommand: piped or redirected output must not
// carry literal ESC[2m, which no other cmd* in this repo emits. serviceDim
// gates on serviceOut being a terminal, so every one of these lines is plain
// text whenever the tests (or a shell pipeline) capture it.
func TestServiceOutputCarriesNoANSIWhenNotATerminal(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T, mgr serviceManager, f *fakeRunner) int
		mgr  func(home string) serviceManager
	}{
		{
			name: "launchd install names the log file",
			mgr:  func(home string) serviceManager { return &launchdService{home: home, uid: 501} },
			run: func(t *testing.T, mgr serviceManager, f *fakeRunner) int {
				orig := resolveBin
				resolveBin = func() (string, error) { return "/usr/local/bin/claude-sessions", nil }
				defer func() { resolveBin = orig }()
				f.fail("launchctl print", "com.skerla.claude-sessions = {\n\tpid = 4242\n\tstate = running\n}", nil)
				return serviceInstall(mgr, f.run, []string{"--port", "8765"})
			},
		},
		{
			name: "launchd uninstall keeps the log",
			mgr:  func(home string) serviceManager { return &launchdService{home: home, uid: 501} },
			run: func(t *testing.T, mgr serviceManager, f *fakeRunner) int {
				return serviceUninstall(mgr, f.run, nil)
			},
		},
		{
			name: "systemd uninstall notes linger",
			mgr:  func(home string) serviceManager { return &systemdService{home: home, user: "andy"} },
			run: func(t *testing.T, mgr serviceManager, f *fakeRunner) int {
				return serviceUninstall(mgr, f.run, nil)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			orig := serviceOut
			serviceOut = &buf
			defer func() { serviceOut = orig }()

			mgr := tt.mgr(t.TempDir())
			if rc := tt.run(t, mgr, newFakeRunner()); rc != 0 {
				t.Fatalf("command = %d, want 0", rc)
			}
			if strings.Contains(buf.String(), "\x1b") {
				t.Errorf("output contains a raw escape sequence: %q", buf.String())
			}
			// Guard against passing vacuously: these paths are only interesting
			// because they print a hint that used to be dimmed.
			if buf.Len() == 0 {
				t.Error("no output captured at all, so nothing was actually checked")
			}
		})
	}
}
