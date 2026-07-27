# `service` Subcommand Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `claude-sessions service install|uninstall|status`, which writes and loads a launchd LaunchAgent on macOS or a systemd `--user` unit on Linux so the server survives logout and reboot.

**Architecture:** One new file, `service.go`, holding a `serviceManager` interface with two implementations selected by a runtime `switch runtime.GOOS`. Both are pure string rendering plus `os/exec`, so both compile and golden-test on either platform. All shell-outs go through an injected `runner func(args ...string) ([]byte, error)`, making load/unload/status assertable without touching real launchd or systemd.

**Tech Stack:** Go stdlib only (`os/exec`, `encoding/xml`, `text/template` not required — plain `fmt`), plus the existing `golang.org/x/sys` if needed. No new dependencies.

Spec: `docs/2026-07-27-service-subcommand-design.md`. Read it before starting.

## Global Constraints

- **Package `main`, flat file layout.** No subpackages. Files are split by concern at the top level.
- **No new dependencies.** Only `golang.org/x/term` and `golang.org/x/sys` are permitted, and neither is needed here. Single-binary deployment is the point of the Go rewrite.
- **No build tags on the new code.** Both backends must compile on both platforms — that is what makes the golden tests runnable from a macOS dev box. (`termios_{bsd,linux}.go` use build tags because they reference non-portable ioctl constants. This code does not.)
- **Exit code convention:** `2` means usage error (bad flag, missing arg) everywhere in this repo — see `main.go:60` and the `cmd*` functions. Never reuse `2` for a semantic result.
- **Error output convention:** failures print to stderr prefixed with the subcommand name, e.g. `service: ...`, and return a non-zero code. Match `cmdKill` (`commands.go:42-57`).
- **Tests are table-driven**, in the style of `TestParseNewArgs` (`commands_test.go:8`).
- **Verification after every task:** `go test ./...` and `go vet ./...` must both pass. `gofmt -l .` must print nothing.
- **Label / unit names:** macOS label is `com.skerla.claude-sessions`; Linux unit is `claude-sessions.service`. These strings appear in the plist, the unit path, and every launchctl/systemctl invocation — define them once as constants.

---

### Task 1: Extract the shared server flag parser

`cmdServer` parses `--port`/`--bind` in an inline loop. `service install` accepts the same two flags and must not grow a second copy — the copy would silently drift, and the symptom would be a unit file missing a newly added flag.

**Deviation from the spec:** the spec proposed `parseServerFlags(args, ctx) (port, bind, code)`. Use `(serverFlags, error)` instead, matching `parseKillFlags` (`commands.go:27`). The caller prints its own prefix and chooses its own exit code, which is what every other parser in this repo does.

**Files:**
- Modify: `server.go:763-791` (replace the inline loop)
- Create: `service_flags_test.go`

**Interfaces:**
- Consumes: nothing
- Produces: `type serverFlags struct { port int; bind string }` and `func parseServerFlags(args []string) (serverFlags, error)`. Task 7 calls this. `defaultServerPort` is an existing constant.

- [ ] **Step 1: Write the failing test**

Create `service_flags_test.go`:

```go
package main

import "testing"

func TestParseServerFlags(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    serverFlags
		wantErr bool
	}{
		{
			name: "no args uses defaults",
			args: nil,
			want: serverFlags{port: defaultServerPort, bind: "127.0.0.1"},
		},
		{
			name: "port only",
			args: []string{"--port", "9999"},
			want: serverFlags{port: 9999, bind: "127.0.0.1"},
		},
		{
			name: "bind only",
			args: []string{"--bind", "tailscale"},
			want: serverFlags{port: defaultServerPort, bind: "tailscale"},
		},
		{
			name: "both flags in either order",
			args: []string{"--bind", "0.0.0.0", "--port", "1234"},
			want: serverFlags{port: 1234, bind: "0.0.0.0"},
		},
		{
			name:    "port missing its value",
			args:    []string{"--port"},
			wantErr: true,
		},
		{
			name:    "bind missing its value",
			args:    []string{"--bind"},
			wantErr: true,
		},
		{
			name:    "port is not a number",
			args:    []string{"--port", "http"},
			wantErr: true,
		},
		{
			name:    "unknown flag is an error, not a silent no-op",
			args:    []string{"--verbose"},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseServerFlags(tt.args)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseServerFlags(%q) = %+v, want error", tt.args, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseServerFlags(%q) unexpected error: %v", tt.args, err)
			}
			if got != tt.want {
				t.Errorf("parseServerFlags(%q) = %+v, want %+v", tt.args, got, tt.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./... -run TestParseServerFlags`
Expected: FAIL — `undefined: serverFlags`, `undefined: parseServerFlags`

- [ ] **Step 3: Write the parser**

Add to `server.go`, directly above `cmdServer`:

```go
// serverFlags are the flags shared by `-s` and `service install`. Both parse
// them through parseServerFlags so a flag added to one can't go missing from
// the unit file the other writes.
type serverFlags struct {
	port int
	bind string
}

// parseServerFlags reads --port/--bind. An unrecognized flag is an error rather
// than a silent no-op, so a typo never reads as "use the default".
func parseServerFlags(args []string) (serverFlags, error) {
	f := serverFlags{port: defaultServerPort, bind: "127.0.0.1"}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--port":
			if i+1 >= len(args) {
				return f, fmt.Errorf("--port needs a value")
			}
			p, err := strconv.Atoi(args[i+1])
			if err != nil {
				return f, fmt.Errorf("bad port %q", args[i+1])
			}
			f.port = p
			i++
		case "--bind":
			if i+1 >= len(args) {
				return f, fmt.Errorf("--bind needs a value")
			}
			f.bind = args[i+1]
			i++
		default:
			return f, fmt.Errorf("unknown arg %q", args[i])
		}
	}
	return f, nil
}
```

- [ ] **Step 4: Rewrite `cmdServer` to use it**

Replace `server.go:764-791` (from `port := defaultServerPort` through the closing brace of the `for` loop) with:

```go
	flags, err := parseServerFlags(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "server: %v\n", err)
		return 2
	}
	port, bind := flags.port, flags.bind
```

Leave everything below it — the `bind == "tailscale"` resolution at `server.go:794` and onward — untouched. The existing `tok, err := loadOrCreateToken()` line below now assigns to an already-declared `err`; change it to `tok, err := ...` → `tok, errTok := ...` only if the compiler complains, otherwise leave as is.

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./... && go vet ./...`
Expected: PASS. The server's behavior is unchanged — same flags, same defaults, same exit code 2 on a bad flag.

- [ ] **Step 6: Commit**

```bash
git add server.go service_flags_test.go
git commit -m "refactor: extract parseServerFlags so -s and service install share it"
```

---

### Task 2: Binary path and PATH resolution

The unit file needs an absolute binary path and an explicit `PATH`. Both have a failure mode the spec calls out: a `go run` binary lives in a temp dir that is deleted on exit, and launchd hands an agent only `/usr/bin:/bin:/usr/sbin:/sbin` — which does not contain Homebrew's `tmux` or `~/.local/bin/claude`.

**Files:**
- Create: `service.go`
- Create: `service_test.go`

**Interfaces:**
- Consumes: `serverFlags` from Task 1
- Produces:
  - `type serviceConfig struct { BinPath string; Port int; Bind string; Path string; LogPath string }`
  - `func resolveBinPath() (string, error)`
  - `func pathWithin(p, dir string) bool`
  - `func capturePath() string`
  - Constants `serviceLabel = "com.skerla.claude-sessions"` and `systemdUnitName = "claude-sessions.service"`

- [ ] **Step 1: Write the failing test**

Create `service_test.go`:

```go
package main

import (
	"os"
	"path/filepath"
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
// macOS: a raw string prefix check against os.TempDir() misses it.
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
	exe := filepath.Join(resolved, "exe")
	if !pathWithin(exe, resolved) {
		t.Errorf("pathWithin(%q, %q) = false, want true", exe, resolved)
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./... -run 'TestPathWithin|TestCapturePath'`
Expected: FAIL — `undefined: pathWithin`, `undefined: capturePath`

- [ ] **Step 3: Write the implementation**

Create `service.go`:

```go
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

	// launchd hands an agent this and nothing else, which is also the reason
	// capturePath exists — see the PATH note below.
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
	real, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return "", fmt.Errorf("cannot resolve %s: %w", exe, err)
	}
	// `go run` builds into $TMPDIR and deletes it on exit, so a unit pointing
	// there would reference a file that no longer exists. Resolve the temp dir
	// too: on macOS /tmp is a symlink to /private/tmp.
	tmp := os.TempDir()
	if resolved, err := filepath.EvalSymlinks(tmp); err == nil {
		tmp = resolved
	}
	if pathWithin(real, tmp) {
		return "", fmt.Errorf("running from a temporary build directory (%s)\n"+
			"       install a real binary first: make install", real)
	}
	return real, nil
}

// pathWithin reports whether p is dir or sits underneath it. Uses filepath.Rel
// rather than string prefixing so /var/tmpfoo isn't treated as being inside
// /var/tmp.
func pathWithin(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	if err != nil {
		return false
	}
	return rel == "." || !strings.HasPrefix(rel, "..")
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
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./... -run 'TestPathWithin|TestCapturePath' -v && go vet ./...`
Expected: PASS, all five subtests

- [ ] **Step 5: Commit**

```bash
git add service.go service_test.go
git commit -m "feat: add service config, binary path, and PATH resolution"
```

---

### Task 3: launchd plist rendering

**Files:**
- Modify: `service.go`
- Modify: `service_test.go`

**Interfaces:**
- Consumes: `serviceConfig` (Task 2)
- Produces: `type launchdService struct { home string; uid int }` with methods `UnitPath() string`, `Label() string`, `Render(serviceConfig) string`. Task 5 adds `Load`/`Unload`, Task 6 adds `Status`.

- [ ] **Step 1: Write the failing test**

Append to `service_test.go`:

```go
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
// line, not the cause.
func TestLaunchdRenderEscapesXML(t *testing.T) {
	svc := &launchdService{home: "/Users/andy", uid: 501}
	got := svc.Render(serviceConfig{
		BinPath: "/Users/andy/bin/claude & sessions",
		Port:    8765,
		Bind:    "<broken>",
		Path:    "/usr/bin",
		LogPath: "/tmp/log",
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
}

func TestLaunchdUnitPath(t *testing.T) {
	svc := &launchdService{home: "/Users/andy", uid: 501}
	want := "/Users/andy/Library/LaunchAgents/com.skerla.claude-sessions.plist"
	if got := svc.UnitPath(); got != want {
		t.Errorf("UnitPath() = %q, want %q", got, want)
	}
}
```

Add `"strings"` to the test file's imports.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./... -run TestLaunchd`
Expected: FAIL — `undefined: launchdService`

- [ ] **Step 3: Write the implementation**

Append to `service.go`:

```go
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
	fmt.Fprintf(&b, "  <key>StandardOutPath</key><string>%s</string>\n", xmlEscape(cfg.LogPath))
	fmt.Fprintf(&b, "  <key>StandardErrorPath</key><string>%s</string>\n", xmlEscape(cfg.LogPath))
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
```

Add `"strconv"` to `service.go`'s imports.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./... -run TestLaunchd -v && go vet ./...`
Expected: PASS, three tests

- [ ] **Step 5: Commit**

```bash
git add service.go service_test.go
git commit -m "feat: render the launchd plist, with XML escaping and an explicit PATH"
```

---

### Task 4: systemd unit rendering

**Files:**
- Modify: `service.go`
- Modify: `service_test.go`

**Interfaces:**
- Consumes: `serviceConfig` (Task 2), `serviceArgs` (Task 3)
- Produces: `type systemdService struct { home string; user string }` with `UnitPath() string`, `Label() string`, `Render(serviceConfig) string`

- [ ] **Step 1: Write the failing test**

Append to `service_test.go`:

```go
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
ExecStart=/home/andy/.local/bin/claude-sessions -s --port 8765 --bind tailscale
Environment=PATH=/home/andy/.local/bin:/usr/bin:/bin
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./... -run TestSystemd`
Expected: FAIL — `undefined: systemdService`

- [ ] **Step 3: Write the implementation**

Append to `service.go`:

```go
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

func (s *systemdService) Render(cfg serviceConfig) string {
	var b strings.Builder
	b.WriteString("[Unit]\nDescription=claude-sessions server\n\n")
	b.WriteString("[Service]\n")
	// No After=network-online.target: a --user unit isn't ordered against
	// system targets, so it would be a no-op that reads as protection.
	fmt.Fprintf(&b, "ExecStart=%s\n", strings.Join(serviceArgs(cfg), " "))
	fmt.Fprintf(&b, "Environment=PATH=%s\n", cfg.Path)
	// Restart=always covers the boot race where this starts before tailscaled;
	// RestartSec=5 keeps the default start limit (5 in 10s) from tripping
	// before it converges.
	b.WriteString("Restart=always\nRestartSec=5\n\n")
	b.WriteString("[Install]\nWantedBy=default.target\n")
	return b.String()
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./... -run TestSystemd -v && go vet ./...`
Expected: PASS, four tests

- [ ] **Step 5: Commit**

```bash
git add service.go service_test.go
git commit -m "feat: render the systemd --user unit"
```

---

### Task 5: The runner, and the load/unload sequences

The three subtlest bugs in this feature live here: `bootout` tears down asynchronously so an immediate `bootstrap` can hit `EALREADY`; `systemctl --user enable --now` only *starts*, so on a reinstall the already-running unit keeps the old flags; and `daemon-reload` fails when the user manager isn't up, which `enable-linger` is what starts.

> **Correction (applied during execution).** The code below uses exit status
> `149` for `EALREADY`. That is wrong — there is no such Darwin errno.
> `/usr/include/sys/errno.h` gives `EINPROGRESS` 36 and `EALREADY` **37**, and
> launchctl prints the errno as its exit status. As written, the numeric branch
> was dead and the retry fired only on the English string match; the test did
> not catch it because it scripted a plain `errors.New` rather than an
> `*exec.ExitError`, so `errors.As` never matched either. The shipped code uses
> a `darwinEALREADY = 37` constant and a test built on a real `*exec.ExitError`.

**Files:**
- Modify: `service.go`
- Modify: `service_test.go`

**Interfaces:**
- Consumes: `launchdService` (Task 3), `systemdService` (Task 4)
- Produces:
  - `type runner func(args ...string) ([]byte, error)`
  - `func execRunner(args ...string) ([]byte, error)`
  - `func exitCode(err error) int`
  - `(*launchdService).Load(runner) error`, `.Unload(runner) error`
  - `(*systemdService).Load(runner) error`, `.Unload(runner) error`
  - `type serviceManager interface { UnitPath() string; Label() string; Render(serviceConfig) string; defaultLogPath() string; Load(runner) error; Unload(runner) error; Status(runner) (serviceStatus, error) }` — `Status` lands in Task 6; declare the interface there, after both types satisfy it.

- [ ] **Step 1: Write the failing test**

Append to `service_test.go`:

```go
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
```

Add `"errors"` to the test file's imports.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./... -run 'TestLaunchdLoad|TestLaunchdUnload|TestSystemdLoad|TestSystemdUnload'`
Expected: FAIL — `svc.Load undefined`

- [ ] **Step 3: Write the implementation**

Append to `service.go`:

```go
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

// exitCode extracts a process exit status, or -1 if err isn't an exit error.
func exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

const (
	bootstrapRetries = 5
	bootstrapBackoff = 200 * time.Millisecond
)

func (s *launchdService) domain() string { return fmt.Sprintf("gui/%d", s.uid) }

func (s *launchdService) target() string {
	return fmt.Sprintf("%s/%s", s.domain(), serviceLabel)
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
		// bootout tears down asynchronously; EALREADY (149) means we lost that
		// race, so back off and try again rather than reporting a bogus failure.
		if !isBootstrapRetryable(out, err) {
			break
		}
		time.Sleep(bootstrapBackoff)
	}

	// gui/<uid> only exists with a console login. Installing over SSH to a Mac
	// where nobody is logged in fails here, and launchctl's own message
	// ("Bootstrap failed: 5: Input/output error") names nothing useful.
	if _, err := run("launchctl", "print", s.domain()); err != nil {
		return fmt.Errorf("no GUI login session for uid %d — launchd user agents "+
			"need someone logged in at the console\n"+
			"       log in on the Mac itself, then re-run this command", s.uid)
	}
	return fmt.Errorf("launchctl bootstrap failed: %v\n%s", lastErr, strings.TrimSpace(string(lastOut)))
}

func isBootstrapRetryable(out []byte, err error) bool {
	if exitCode(err) == 149 {
		return true
	}
	return strings.Contains(string(out), "EALREADY") ||
		strings.Contains(string(out), "Operation already in progress")
}

func (s *launchdService) Unload(run runner) error {
	out, err := run("launchctl", "bootout", s.target())
	if err != nil && !strings.Contains(string(out), "No such process") {
		return fmt.Errorf("launchctl bootout failed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (s *systemdService) Load(run runner) error {
	// Linger first: on a box where the per-user manager isn't running yet,
	// this is what starts it, and every `systemctl --user` below fails until
	// it exists. A polkit refusal only means the service dies at logout, which
	// is worth a warning rather than aborting the install.
	if out, err := run("loginctl", "enable-linger", s.user); err != nil {
		fmt.Fprintf(os.Stderr, "service: warning: could not enable linger for %s: %v\n%s\n",
			s.user, err, strings.TrimSpace(string(out)))
		fmt.Fprintf(os.Stderr, "service: the server will stop when your last login session ends\n")
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
	if out, err := run("systemctl", "--user", "disable", "--now", systemdUnitName); err != nil {
		return fmt.Errorf("systemctl disable failed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
	if out, err := run("systemctl", "--user", "daemon-reload"); err != nil {
		return fmt.Errorf("systemctl daemon-reload failed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
```

Add `"errors"`, `"os/exec"`, and `"time"` to `service.go`'s imports.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./... -run 'TestLaunchd|TestSystemd' -v && go vet ./...`
Expected: PASS

Note: `TestLaunchdLoadRetriesEALREADY` sleeps `5 × 200ms`. If total test time becomes annoying, that is expected, not a bug.

- [ ] **Step 5: Commit**

```bash
git add service.go service_test.go
git commit -m "feat: add service load/unload with EALREADY retry and an explicit systemd restart"
```

---

### Task 6: Status

**Files:**
- Modify: `service.go`
- Modify: `service_test.go`

**Interfaces:**
- Consumes: everything from Tasks 3-5
- Produces: `type serviceStatus struct { Installed bool; Running bool; PID int }`, `(*launchdService).Status(runner) (serviceStatus, error)`, `(*systemdService).Status(runner) (serviceStatus, error)`, the `serviceManager` interface, and `func newServiceManager() (serviceManager, error)`

- [ ] **Step 1: Write the failing test**

Append to `service_test.go`:

```go
func TestLaunchdStatus(t *testing.T) {
	running := `com.skerla.claude-sessions = {
	active count = 1
	pid = 4242
	state = running
}`
	tests := []struct {
		name    string
		out     string
		err     error
		want    serviceStatus
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
			name: "not loaded",
			out:  "Could not find service \"com.skerla.claude-sessions\"",
			err:  errors.New("exit status 113"),
			want: serviceStatus{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeRunner()
			f.fail("launchctl print", tt.out, tt.err)
			svc := &launchdService{home: "/Users/andy", uid: 501}
			got, err := svc.Status(f.run)
			if err != nil {
				t.Fatalf("Status() error: %v", err)
			}
			if got != tt.want {
				t.Errorf("Status() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestSystemdStatus(t *testing.T) {
	tests := []struct {
		name string
		out  string
		err  error
		want serviceStatus
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
			name: "not found",
			out:  "LoadState=not-found\nActiveState=inactive\nMainPID=0\n",
			want: serviceStatus{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeRunner()
			f.fail("systemctl", tt.out, tt.err)
			svc := &systemdService{home: "/home/andy", user: "andy"}
			got, err := svc.Status(f.run)
			if err != nil {
				t.Fatalf("Status() error: %v", err)
			}
			if got != tt.want {
				t.Errorf("Status() = %+v, want %+v", got, tt.want)
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

// Both backends must satisfy the interface on every platform — that's the
// whole point of not using build tags here.
func TestBothBackendsSatisfyServiceManager(t *testing.T) {
	var _ serviceManager = &launchdService{}
	var _ serviceManager = &systemdService{}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./... -run 'TestLaunchdStatus|TestSystemdStatus|TestServiceStatusExitCode|TestBothBackends'`
Expected: FAIL — `undefined: serviceStatus`

- [ ] **Step 3: Write the implementation**

Append to `service.go`:

```go
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
}

func newServiceManager() (serviceManager, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("cannot determine home directory: %w", err)
	}
	switch runtime.GOOS {
	case "darwin":
		return &launchdService{home: home, uid: os.Getuid()}, nil
	case "linux":
		u, err := user.Current()
		if err != nil {
			return nil, fmt.Errorf("cannot determine current user: %w", err)
		}
		return &systemdService{home: home, user: u.Username}, nil
	default:
		return nil, fmt.Errorf("service management is only supported on macOS and Linux (this is %s)", runtime.GOOS)
	}
}

var launchdPIDRe = regexp.MustCompile(`(?m)^\s*pid\s*=\s*(\d+)\s*$`)

func (s *launchdService) Status(run runner) (serviceStatus, error) {
	out, err := run("launchctl", "print", s.target())
	if err != nil {
		// launchctl exits non-zero when the service isn't loaded at all. That
		// is an answer, not a failure.
		return serviceStatus{}, nil
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

func (s *systemdService) Status(run runner) (serviceStatus, error) {
	// `show` rather than `is-active`: one call yields load state, active state,
	// and the PID, and it exits zero even for a unit that doesn't exist.
	out, _ := run("systemctl", "--user", "show", systemdUnitName,
		"--property=LoadState", "--property=ActiveState", "--property=MainPID")
	fields := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok {
			fields[k] = v
		}
	}
	if fields["LoadState"] == "" || fields["LoadState"] == "not-found" {
		return serviceStatus{}, nil
	}
	st := serviceStatus{Installed: true}
	if fields["ActiveState"] == "active" {
		st.Running = true
		if pid, err := strconv.Atoi(fields["MainPID"]); err == nil && pid > 0 {
			st.PID = pid
		}
	}
	return st, nil
}
```

Add `"os/user"`, `"regexp"`, and `"runtime"` to `service.go`'s imports.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./... -run 'TestLaunchdStatus|TestSystemdStatus|TestServiceStatusExitCode|TestBothBackends' -v && go vet ./...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add service.go service_test.go
git commit -m "feat: add service status parsing and scriptable exit codes"
```

---

### Task 7: Wire up `cmdService` and dispatch

**Files:**
- Modify: `service.go`
- Modify: `main.go:17-61` (dispatch), `main.go:64-92` (usage)
- Modify: `service_test.go`

**Interfaces:**
- Consumes: everything above
- Produces: `func cmdService(args []string) int`, `func portInUse(bind string, port int) bool`

- [ ] **Step 1: Write the failing test**

Append to `service_test.go`:

```go
func TestCmdServiceUsageErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "no verb", args: nil},
		{name: "unknown verb", args: []string{"reload"}},
		{name: "unknown flag on install", args: []string{"install", "--verbose"}},
		{name: "flags on uninstall", args: []string{"uninstall", "--port", "1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cmdService(tt.args); got != 2 {
				t.Errorf("cmdService(%q) = %d, want 2 (usage error)", tt.args, got)
			}
		})
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

	if !portInUse("127.0.0.1", port) {
		t.Errorf("portInUse(127.0.0.1, %d) = false while listening, want true", port)
	}
	ln.Close()
	if portInUse("127.0.0.1", port) {
		t.Errorf("portInUse(127.0.0.1, %d) = true after close, want false", port)
	}
}

// "tailscale" is a magic bind value resolved at server start, not an address.
// The pre-flight check must skip it rather than trying to listen on it.
func TestPortInUseSkipsUnresolvableBind(t *testing.T) {
	if portInUse("tailscale", 8765) {
		t.Error("portInUse(tailscale, ...) = true, want false — the literal isn't an address")
	}
}
```

Add `"net"` to the test file's imports.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./... -run 'TestCmdService|TestPortInUse'`
Expected: FAIL — `undefined: cmdService`

- [ ] **Step 3: Write the implementation**

Append to `service.go`:

```go
const serviceUsage = `usage: claude-sessions service <install|uninstall|status> [--port N] [--bind ADDR]`

func cmdService(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, serviceUsage)
		return 2
	}
	mgr, err := newServiceManager()
	if err != nil {
		fmt.Fprintln(os.Stderr, "service:", err)
		return 1
	}
	switch args[0] {
	case "install":
		return serviceInstall(mgr, args[1:])
	case "uninstall":
		return serviceUninstall(mgr, args[1:])
	case "status":
		return serviceStatusCmd(mgr, args[1:])
	default:
		fmt.Fprintf(os.Stderr, "service: unknown verb %q\n%s\n", args[0], serviceUsage)
		return 2
	}
}

func serviceInstall(mgr serviceManager, args []string) int {
	flags, err := parseServerFlags(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "service: %v\n%s\n", err, serviceUsage)
		return 2
	}
	bin, err := resolveBinPath()
	if err != nil {
		fmt.Fprintln(os.Stderr, "service:", err)
		return 1
	}
	// A foreground `-s` the user forgot about would make the new service
	// crash-loop on bind failure. Refuse rather than install something broken.
	if portInUse(flags.bind, flags.port) {
		fmt.Fprintf(os.Stderr, "service: something is already listening on %s:%d\n", flags.bind, flags.port)
		fmt.Fprintf(os.Stderr, "         stop it first, or pass a different --port\n")
		return 1
	}

	cfg := serviceConfig{
		BinPath: bin,
		Port:    flags.port,
		Bind:    flags.bind,
		Path:    capturePath(),
		LogPath: mgr.defaultLogPath(),
	}
	unitPath := mgr.UnitPath()
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "service: cannot create %s: %v\n", filepath.Dir(unitPath), err)
		return 1
	}
	// 0644: read by the user's own launchd/systemd, and it holds no secrets —
	// the server's token lives elsewhere.
	if err := os.WriteFile(unitPath, []byte(mgr.Render(cfg)), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "service: cannot write %s: %v\n", unitPath, err)
		return 1
	}
	fmt.Printf("wrote   %s\n", unitPath)

	if err := mgr.Load(execRunner); err != nil {
		fmt.Fprintln(os.Stderr, "service:", err)
		// The unit file stays: the usual fix is to run the loader by hand and
		// read the real error.
		fmt.Fprintf(os.Stderr, "service: the unit file was written and left in place\n")
		return 1
	}
	fmt.Printf("loaded  %s\n", mgr.Label())
	if cfg.LogPath != "" {
		fmt.Printf("logs    %s  %s\n", cfg.LogPath, dim("(not rotated)"))
	} else {
		fmt.Printf("logs    journalctl --user -u %s\n", mgr.Label())
	}
	return 0
}

func serviceUninstall(mgr serviceManager, args []string) int {
	if len(args) > 0 {
		fmt.Fprintf(os.Stderr, "service: uninstall takes no flags\n%s\n", serviceUsage)
		return 2
	}
	if err := mgr.Unload(execRunner); err != nil {
		fmt.Fprintln(os.Stderr, "service:", err)
		return 1
	}
	fmt.Printf("unloaded %s\n", mgr.Label())

	unitPath := mgr.UnitPath()
	if err := os.Remove(unitPath); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "service: cannot remove %s: %v\n", unitPath, err)
		return 1
	}
	fmt.Printf("removed  %s\n", unitPath)
	if runtime.GOOS == "linux" {
		// Other user services may depend on linger, so removing it is not ours
		// to decide.
		fmt.Printf("%s\n", dim("linger left enabled; remove with: loginctl disable-linger $USER"))
	}
	if lp := mgr.defaultLogPath(); lp != "" {
		fmt.Printf("%s\n", dim("log kept at "+lp))
	}
	return 0
}

func serviceStatusCmd(mgr serviceManager, args []string) int {
	if len(args) > 0 {
		fmt.Fprintf(os.Stderr, "service: status takes no flags\n%s\n", serviceUsage)
		return 2
	}
	st, err := mgr.Status(execRunner)
	if err != nil {
		fmt.Fprintln(os.Stderr, "service:", err)
		return 1
	}
	unitPath := mgr.UnitPath()
	_, statErr := os.Stat(unitPath)
	fmt.Printf("unit    %s\n", unitPath)
	fmt.Printf("file    %s\n", yesNo(statErr == nil))
	fmt.Printf("loaded  %s\n", yesNo(st.Installed))
	if st.Running {
		fmt.Printf("running yes (pid %d)\n", st.PID)
	} else {
		fmt.Printf("running no\n")
	}
	return st.exitCode()
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// portInUse reports whether something already holds bind:port. A bind value
// that isn't an address — notably the magic "tailscale", resolved at server
// start — can't be checked, and an unusable check must not block the install.
func portInUse(bind string, port int) bool {
	if net.ParseIP(bind) == nil && bind != "localhost" {
		return false
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(bind, strconv.Itoa(port)))
	if err != nil {
		return true
	}
	ln.Close()
	return false
}
```

Add `"net"` to `service.go`'s imports.

- [ ] **Step 4: Wire dispatch into `main.go`**

In `main.go`, add a case after `case "pair":` (line 55-56):

```go
	case "service":
		os.Exit(cmdService(args[1:]))
```

In the `usage` const, add after the `pair` line (line 83):

```
  service install|uninstall|status [--port N] [--bind ADDR]
                                  run the server as a supervised background
                                  service (launchd on macOS, systemd --user on
                                  Linux); install also starts it
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./... && go vet ./... && gofmt -l .`
Expected: PASS, and `gofmt -l .` prints nothing

- [ ] **Step 6: Verify by hand on this machine**

Build into the temp directory `os.TempDir()` actually reports — on macOS that is
`$TMPDIR` (`/var/folders/...`), **not** `/tmp`. A binary built anywhere else is
outside the guard, and `service install` will really install and load a service
on this machine.

```bash
CS_CHECK="${TMPDIR:-/tmp}/cs-check"
go build -o "$CS_CHECK" . && "$CS_CHECK" service status; echo "exit=$?"
```
Expected: reports the unit path, `file no`, `loaded no`, `running no`, `exit=3`

```bash
"$CS_CHECK" service install --port 9999
```
Expected: **refuses** — the binary sits under `os.TempDir()`, so `resolveBinPath` rejects it and names `make install`. This confirms the temp-dir guard on a real binary.

- [ ] **Step 7: Commit**

```bash
git add service.go service_test.go main.go
git commit -m "feat: add the service install/uninstall/status subcommand"
```

---

### Task 8: Documentation

**Files:**
- Modify: `README.md:272-317`
- Modify: `CHANGELOG.md`

- [ ] **Step 1: Rewrite the README's "Running as a service" section**

Replace the whole section (`README.md:272-317`) with:

````markdown
### Running as a service

The server is a foreground process. A watchdog that dies silently is
indistinguishable from a quiet week, so supervise it:

```sh
claude-sessions service install --bind tailscale
claude-sessions service status      # 0 running, 1 installed but stopped, 3 not installed
claude-sessions service uninstall
```

`install` writes a launchd LaunchAgent on macOS or a systemd `--user` unit on
Linux, then loads it — the server is running when the command returns. Re-run
`install` to change `--port`/`--bind`, or after upgrading the binary.

It bakes your current `PATH` into the unit. Supervisors start services with a
near-empty `PATH`, and this binary shells out to `tmux`, `tailscale`, and
`claude` by name; without it, tmux detection silently finds nothing.

On Linux, `install` also runs `loginctl enable-linger`, so the server survives
logout instead of dying with your last session.

Logs are `~/Library/Logs/claude-sessions.log` on macOS (launchd does not rotate
it) and `journalctl --user -u claude-sessions` on Linux.

Installing over SSH to a Mac needs someone logged in at the console — launchd
user agents live in the `gui/<uid>` domain, which doesn't exist otherwise.

<details>
<summary>Equivalent unit files, if you'd rather install by hand</summary>

macOS — `~/Library/LaunchAgents/com.skerla.claude-sessions.plist`:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>com.skerla.claude-sessions</string>
  <key>ProgramArguments</key>
  <array>
    <string>/Users/YOU/.local/bin/claude-sessions</string>
    <string>-s</string>
    <string>--bind</string>
    <string>tailscale</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key><string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
  </dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>/Users/YOU/Library/Logs/claude-sessions.log</string>
  <key>StandardErrorPath</key><string>/Users/YOU/Library/Logs/claude-sessions.log</string>
</dict>
</plist>
```

`launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.skerla.claude-sessions.plist`

Linux — `~/.config/systemd/user/claude-sessions.service`:

```ini
[Unit]
Description=claude-sessions server

[Service]
ExecStart=%h/.local/bin/claude-sessions -s --bind tailscale
Environment=PATH=%h/.local/bin:/usr/local/bin:/usr/bin:/bin
Restart=always
RestartSec=5

[Install]
WantedBy=default.target
```

```sh
loginctl enable-linger "$USER"
systemctl --user enable --now claude-sessions
```

</details>
````

Three corrections are folded in versus the previous text: `launchctl load` → `bootstrap`, an explicit `PATH` in both templates, and `After=network-online.target` dropped (a no-op in a `--user` manager, which isn't ordered against system targets).

- [ ] **Step 2: Add the CHANGELOG entry**

Add under the top `## [Unreleased]` heading, creating it if absent:

```markdown
### Added
- `service install|uninstall|status` runs the server as a supervised background
  service — a launchd LaunchAgent on macOS, a systemd `--user` unit on Linux.
  `install` writes the unit and starts it; re-run it to change flags.

### Fixed
- The service unit now carries an explicit `PATH`. Supervisors start services
  with a near-empty `PATH`, so the documented hand-written plist left `tmux`,
  `tailscale`, and `claude` unresolvable — tmux detection silently found
  nothing and `--bind tailscale` crash-looped.
```

- [ ] **Step 3: Verify the docs build clean**

Run: `go test ./... && go vet ./... && gofmt -l .`
Expected: PASS, no output from gofmt

- [ ] **Step 4: Commit**

```bash
git add README.md CHANGELOG.md
git commit -m "docs: document the service subcommand and fix the manual unit templates"
```

---

## Verification

After Task 8, on the macOS box:

```bash
make install
claude-sessions service install --bind tailscale   # or --port N for a scratch port
claude-sessions service status                      # expect: running yes (pid N), exit 0
curl -s localhost:8765/sessions | head -c 100       # expect JSON, not a connection error
```

The PATH fix is the one worth confirming explicitly, since it is invisible until
something shells out — with the service running, a session list that shows tmux
data proves `tmux` resolved under launchd:

```bash
claude-sessions service status && claude-sessions list --once
```

Then clean up or leave it installed:

```bash
claude-sessions service uninstall
```
