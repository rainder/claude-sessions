# `service` subcommand — design

Date: 2026-07-27

## Problem

The server is a foreground process. Keeping it alive across logout and reboot
currently means hand-copying a plist or a systemd unit out of the README,
editing the binary path, and running a `launchctl` incantation that the README
itself gets wrong (`launchctl load` has been deprecated since macOS 10.11).

Every install is a chance to typo the path. Every flag change is a re-edit.

## Shape

```
claude-sessions service install [--port N] [--bind ADDR]
claude-sessions service uninstall
claude-sessions service status
```

`install` writes the unit *and* loads it — the server is running when the
command returns. Re-running `install` overwrites and reloads; that is the
supported way to change `--port`/`--bind` after the fact.

## Platform dispatch

One new file, `service.go`, dispatching on `runtime.GOOS` at run time. **Not**
build tags, despite `termios_{bsd,linux}.go` setting that precedent.

The termios files need build tags because they reference OS-specific ioctl
constants that do not compile on the other platform. The service backends do
not: both are string rendering plus `os/exec`. A runtime switch keeps both
renderers compiled and unit-testable on either platform, so the systemd unit
has golden-test coverage from a macOS dev box.

Unsupported `GOOS` exits 2 with a message naming the two supported platforms.

```go
type runner func(args ...string) error

type serviceManager interface {
	UnitPath() string
	Render(cfg serviceConfig) string
	Load(run runner) error
	Unload(run runner) error
	Status(run runner) (serviceStatus, error)
}

type serviceConfig struct {
	BinPath string
	Port    int
	Bind    string
	LogPath string // empty on Linux — journald
}
```

`runner` is injected so `Load`/`Unload`/`Status` are testable by asserting the
command sequence, with no real launchd or systemd involved.

Implementations: `launchdService` (darwin), `systemdService` (linux).

| | macOS | Linux |
| --- | --- | --- |
| unit path | `~/Library/LaunchAgents/com.skerla.claude-sessions.plist` | `~/.config/systemd/user/claude-sessions.service` |
| label | `com.skerla.claude-sessions` | `claude-sessions.service` |
| load | `launchctl bootout gui/$UID/<label>` (error ignored), then `launchctl bootstrap gui/$UID <plist>` | `systemctl --user daemon-reload`, `loginctl enable-linger $USER`, `systemctl --user enable --now claude-sessions.service` |
| unload | `launchctl bootout gui/$UID/<label>` | `systemctl --user disable --now claude-sessions.service`, `daemon-reload` |
| status | `launchctl print gui/$UID/<label>` | `systemctl --user is-active` + `is-enabled` |
| restart policy | `KeepAlive` | `Restart=always`, `RestartSec=5` |
| logs | `~/Library/Logs/claude-sessions.log` (both stdout and stderr) | journald |

`bootout`/`bootstrap` replace the README's `launchctl load`. The pre-emptive
`bootout` makes reinstall idempotent; its failure means "was not loaded", which
is not an error.

macOS logs go to `~/Library/Logs`, not `/tmp` as the README currently suggests
— `/tmp` is periodically swept, so the log vanishes exactly when you go looking
for why the service died last week.

### Linux linger

`install` runs `loginctl enable-linger $USER`. Without it a `--user` unit is
killed when the last login session ends, which defeats the purpose on the
headless boxes this feature targets. It may trigger a polkit prompt; that is
surfaced, not suppressed.

`uninstall` does **not** disable linger — other user services may depend on it.
It prints that linger was left enabled and how to remove it.

## Flag handling

The `--port`/`--bind` parse loop currently inlined in `cmdServer`
(`server.go:766-791`) is extracted to:

```go
func parseServerFlags(args []string, ctx string) (port int, bind string, code int)
```

`cmdServer` and `service install` both call it. The alternative — a second copy
of the loop — drifts the first time a server flag is added, and the drift is
silent: the unit file would simply omit the new flag.

`ctx` supplies the error prefix (`"server:"` vs `"service:"`).

**`--bind tailscale` is written to the unit file verbatim.** It is not resolved
to an IPv4 at install time. The server already resolves it at startup
(`server.go:794`), and a Tailscale address can change across re-auth or node
migration; a baked-in IP produces a service that starts, binds nothing
reachable, and reports no error.

Bind resolution failure at install time is therefore not checked. If Tailscale
is down, the service fails at start and says so in its log — the same behavior
as running `-s --bind tailscale` by hand.

## Binary path resolution

`os.Executable()` then `filepath.EvalSymlinks`. A unit file pointing at a
symlink that later moves is a confusing failure, and `~/.local/bin` entries are
often symlinks.

If the resolved path is under `os.TempDir()`, `install` refuses: `go run` builds
into a temp directory that is deleted on exit, so the unit would point at a file
that no longer exists. The error names the fix (`make install`).

## Command output

`install` prints what it did, one line per action, then where the logs are:

```
wrote   ~/Library/LaunchAgents/com.skerla.claude-sessions.plist
loaded  com.skerla.claude-sessions
logs    ~/Library/Logs/claude-sessions.log
```

`status` prints unit path, whether the file exists, whether it is loaded, and
the running PID when there is one. Exit code is 0 when running, 1 when
installed but not running, 2 when not installed — so it is usable in a script.

## Errors

Every failure path returns a non-zero exit code and writes to stderr with a
`service:` prefix, matching the other `cmd*` functions. Specifically handled:
unsupported GOOS, temp-dir binary, unwritable unit directory, loader command
failure (the underlying stderr is passed through, not swallowed).

A partial install — file written, load failed — leaves the file in place and
says so, because the fix is usually to run the loader by hand and see the real
error.

## Testing

`service_test.go`:

- golden render of the plist, including a bind value that needs XML escaping
- golden render of the systemd unit
- both of the above run on any GOOS
- `parseServerFlags` shared between server and service, including the error paths
- temp-dir binary rejection
- `Load`/`Unload` command sequences via a fake `runner`, asserting `bootout`
  precedes `bootstrap` and that a `bootout` error does not fail the install
- `Status` parsing for running / installed-not-running / not-installed

## Also touched

- `main.go` — `case "service"` dispatch, usage block entry
- `README.md` — "Running as a service" leads with the subcommand; the manual
  unit templates stay as reference, with `launchctl load` corrected to
  `bootstrap`
- `CHANGELOG.md` — entry under Unreleased

## Out of scope

- A system-wide (`/etc/systemd/system`) unit. Tmux sessions are per-user; a
  root-owned service puts them in a namespace where `ssh host tmux attach`
  finds nothing, which is the failure mode the README already documents.
- Windows.
- `service restart` / `service logs`. `launchctl kickstart -k` and `tail` exist;
  wrapping them earns nothing yet.
