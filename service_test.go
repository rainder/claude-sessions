package main

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
  <key>StandardOutPath</key><string>/Users/andy/Library/Logs/claude-sessions.log</string>
  <key>StandardErrorPath</key><string>/Users/andy/Library/Logs/claude-sessions.log</string>
</dict>
</plist>
`
	if got != want {
		t.Errorf("Render() mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// A path or bind value containing XML metacharacters must not produce a
// malformed plist — launchd rejects the whole file, and the error names the
// line, not the cause. Every interpolation site in Render gets its own
// metacharacter here — BinPath and Bind land in ProgramArguments, Path lands
// in the PATH dict entry, and LogPath lands in both StandardOutPath and
// StandardErrorPath — so escaping can't be dropped from any one site without
// this test catching it.
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
	if n := strings.Count(got, "user&apos;s log"); n != 2 {
		t.Errorf("escaped log path appears %d times, want 2 (StandardOutPath and StandardErrorPath)", n)
	}
}

// LogPath is documented empty on Linux, where journald captures
// stdout/stderr. launchd has no such fallback — an empty
// StandardOutPath/StandardErrorPath can't be opened, so the job fails to
// spawn. Render must substitute defaultLogPath() rather than emit an empty
// path.
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
	if n := strings.Count(got, "<string>"+want+"</string>"); n != 2 {
		t.Errorf("expected defaultLogPath() %q to appear twice (StandardOutPath and StandardErrorPath), got %d", want, n)
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
Restart=always
RestartSec=5

[Install]
WantedBy=default.target
`
	if got != want {
		t.Errorf("Render() mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
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
type fakeRunner struct {
	calls   [][]string
	results map[string]struct {
		out []byte
		err error
	}
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{results: map[string]struct {
		out []byte
		err error
	}{}}
}

// fail scripts a result for any command whose joined form contains substr.
func (f *fakeRunner) fail(substr string, out string, err error) {
	f.results[substr] = struct {
		out []byte
		err error
	}{[]byte(out), err}
}

func (f *fakeRunner) run(args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)
	joined := strings.Join(args, " ")
	for substr, res := range f.results {
		if strings.Contains(joined, substr) {
			return res.out, res.err
		}
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
// error on every reinstall.
func TestLaunchdLoadRetriesEALREADY(t *testing.T) {
	f := newFakeRunner()
	f.fail("bootstrap", "Bootstrap failed: 149: Operation already in progress", errors.New("exit status 149"))
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
	if bootstraps < 2 {
		t.Errorf("bootstrap attempted %d times, want retries", bootstraps)
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
// dying at logout — worth a warning, not worth failing the whole install.
func TestSystemdLoadSurvivesLingerRefusal(t *testing.T) {
	f := newFakeRunner()
	f.fail("enable-linger", "Failed to enable linger: Access denied", errors.New("exit status 1"))
	svc := &systemdService{home: "/home/andy", user: "andy"}
	if err := svc.Load(f.run); err != nil {
		t.Fatalf("Load() error: %v, want nil — linger refusal is not fatal", err)
	}
	if len(f.calls) != 4 {
		t.Errorf("Load() ran %q, want the remaining systemctl commands to still run", f.joined())
	}
}

func TestSystemdLoadFailsOnRestartError(t *testing.T) {
	f := newFakeRunner()
	f.fail("restart", "Job for claude-sessions.service failed", errors.New("exit status 1"))
	svc := &systemdService{home: "/home/andy", user: "andy"}
	if err := svc.Load(f.run); err == nil {
		t.Fatal("Load() = nil, want an error when restart fails")
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
