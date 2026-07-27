# `service` subcommand — design

Date: 2026-07-27

## Problem

The server is a foreground process. Keeping it alive across logout and reboot
currently means hand-copying a plist or a systemd unit out of the README,
editing the binary path, and running a `launchctl` incantation that the README
itself gets wrong (`launchctl load` is the superseded launchctl 1.0 interface;
`bootstrap`/`bootout` replaced it).

Every install is a chance to typo the path. Every flag change is a re-edit.

The README's plist is also missing a `PATH`, which is a latent bug in the
current documented setup — see "Inherited PATH" below.

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
type runner func(args ...string) ([]byte, error)

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
	Path    string // PATH baked into the unit — see "Inherited PATH"
	LogPath string // empty on Linux — journald
}
```

`runner` is injected so `Load`/`Unload`/`Status` are testable by asserting the
command sequence, with no real launchd or systemd involved.

It returns `([]byte, error)`, not a bare `error`: `Status` has to parse
`launchctl print` / `systemctl is-active` output to report a PID and to tell
running from merely installed. A bare `error` cannot carry that, and discovering
so during implementation would mean changing every call site.

Implementations: `launchdService` (darwin), `systemdService` (linux).

| | macOS | Linux |
| --- | --- | --- |
| unit path | `~/Library/LaunchAgents/com.skerla.claude-sessions.plist` | `~/.config/systemd/user/claude-sessions.service` |
| label | `com.skerla.claude-sessions` | `claude-sessions.service` |
| load | `launchctl bootout gui/$UID/<label>` (error ignored), then `launchctl bootstrap gui/$UID <plist>` | `loginctl enable-linger $USER`, `systemctl --user daemon-reload`, `systemctl --user enable claude-sessions.service`, `systemctl --user restart claude-sessions.service` |
| unload | `launchctl bootout gui/$UID/<label>` | `systemctl --user disable --now claude-sessions.service`, `daemon-reload` |
| status | `launchctl print gui/$UID/<label>` | `systemctl --user is-active` + `is-enabled` |
| restart policy | `KeepAlive` | `Restart=always`, `RestartSec=5` |
| logs | `~/Library/Logs/claude-sessions.log` (both stdout and stderr) | journald |

`bootout`/`bootstrap` replace the README's `launchctl load`. The pre-emptive
`bootout` makes reinstall idempotent; its failure means "was not loaded", which
is not an error. `bootout` tears down asynchronously, so an immediate
`bootstrap` can fail with `EALREADY` (149); the loader retries the `bootstrap`
a few times with a short backoff before giving up.

`gui/$UID` requires a GUI login session. Installing over SSH to a Mac with no
one logged in at the console fails, and the raw launchctl error is unhelpful.
That case is detected and reported as such, naming the constraint rather than
passing through `Bootstrap failed: 5: Input/output error`.

The Linux sequence uses an explicit `restart` rather than relying on
`enable --now`, because `--now` only *starts* the unit — on a reinstall the
unit is already running and `start` is a no-op, so the old process would keep
serving with the old `--port`/`--bind`. That silently breaks the documented
"re-run install to change flags" path.

`enable-linger` runs *before* `daemon-reload`: on a box where the per-user
manager isn't running yet, linger is what starts it, and `systemctl --user`
fails until it exists.

macOS logs go to `~/Library/Logs`, not `/tmp` as the README currently suggests
— `/tmp` is periodically swept, so the log vanishes exactly when you go looking
for why the service died last week. launchd does not rotate it; `install` notes
this and `uninstall` leaves it in place, since it is usually what you want to
read after a service you just removed misbehaved.

### Inherited PATH

launchd gives a LaunchAgent `/usr/bin:/bin:/usr/sbin:/sbin` and nothing else
(`launchctl getenv PATH` is empty on this machine, confirming the default).
The binary shells out to `tmux` by bare name in 16 places, plus `tailscale`,
`pngpaste`, and — when spawning sessions — `claude`. On a Homebrew Mac `tmux`
is `/opt/homebrew/bin/tmux` and `claude` is `~/.local/bin/claude`; neither is on
that PATH.

Under the README's current plist this is already a live bug: tmux pane
detection silently returns nothing, spawning a new session fails, and
`--bind tailscale` cannot resolve, so `KeepAlive` produces a permanent crash
loop rather than the converges-once-tailscaled-is-up behavior assumed elsewhere
in this document. (`git`, `ps`, `sysctl`, `vm_stat`, `iostat`, `security`, and
`osascript` all live under `/usr/bin` or `/usr/sbin` and are unaffected.)

So `install` captures the PATH of the invoking shell and bakes it into the
unit: `EnvironmentVariables`/`PATH` on macOS, `Environment=PATH=` on Linux. The
installing shell is the environment in which the user has already verified the
tools work, which makes it the right thing to reproduce — and it keeps the
binary out of the business of guessing at Homebrew prefixes.

systemd's user manager has the same class of problem with a different default,
and gets the same fix.

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

This is only tolerable because the supervisor retries. At boot the service may
well start before `tailscaled` is up; `KeepAlive` and `Restart=always` are what
turn that into a few seconds of retrying instead of a dead service.
`RestartSec=5` keeps systemd's default start-limit (5 starts in 10s) from
tripping. The one thing that converts this from "converges" to "loops forever"
is a missing `tailscale` binary, which is the PATH problem above, not a timing
problem.

`install` also checks whether something is already listening on the target
port — typically a foreground `-s` the user forgot about — and refuses rather
than installing a service that will crash-loop on bind failure.

## Binary path resolution

`os.Executable()` then `filepath.EvalSymlinks`, which resolves to the real
binary rather than a symlink pointing at it. That is a deliberate trade: a unit
pinned to the real file keeps working if the symlink is removed, but does *not*
follow a `make install` that re-points the symlink elsewhere. Re-running
`service install` after an upgrade is the documented answer, and it is already
the documented answer for changing flags.

If the resolved path is under the temp directory, `install` refuses: `go run`
builds into `$TMPDIR` and deletes it on exit, so the unit would point at a file
that no longer exists. The error names the fix (`make install`). The comparison
runs `EvalSymlinks` over `os.TempDir()` as well before matching prefixes —
on macOS `/tmp` is a symlink to `/private/tmp`, so a raw string prefix check
misses it.

## Command output

`install` prints what it did, one line per action, then where the logs are:

```
wrote   ~/Library/LaunchAgents/com.skerla.claude-sessions.plist
loaded  com.skerla.claude-sessions
logs    ~/Library/Logs/claude-sessions.log
```

The unit directory is created with `MkdirAll` — `~/Library/LaunchAgents` and
`~/.config/systemd/user` do not exist on a fresh box. The unit file is written
`0644`: it is read by the user's own launchd/systemd and contains no secrets
(the server's token lives elsewhere).

`status` prints unit path, whether the file exists, whether it is loaded, and
the running PID when there is one. Exit code is 0 when running, 1 when
installed but not running, 3 when not installed — so it is usable in a script.
Not 2: this repo already uses exit 2 for usage errors (`main.go:60`, the `cmd*`
functions), and overloading it would make a typo'd flag indistinguishable from
a real answer.

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
- both renders carry the captured PATH
- `parseServerFlags` shared between server and service, including the error paths
- temp-dir binary rejection, including the `/tmp` → `/private/tmp` symlink case
- `Load`/`Unload` command sequences via a fake `runner`, asserting `bootout`
  precedes `bootstrap`, that a `bootout` error does not fail the install, and
  that `EALREADY` from `bootstrap` is retried
- the Linux sequence issues `restart`, and orders `enable-linger` before
  `daemon-reload`
- `Status` parsing for running / installed-not-running / not-installed, and the
  3/1/0 exit codes

## Also touched

- `main.go` — `case "service"` dispatch, usage block entry
- `README.md` — "Running as a service" leads with the subcommand; the manual
  unit templates stay as reference, with three corrections: `launchctl load` →
  `bootstrap`, a `PATH` added to both templates, and `After=network-online.target`
  dropped from the systemd unit (it is a no-op in a `--user` manager, whose
  units are not ordered against system targets)
- `CHANGELOG.md` — entry under Unreleased

## Out of scope

- A system-wide (`/etc/systemd/system`) unit. Tmux sessions are per-user; a
  root-owned service puts them in a namespace where `ssh host tmux attach`
  finds nothing, which is the failure mode the README already documents.
- Windows.
- `service restart` / `service logs`. `launchctl kickstart -k` and `tail` exist;
  wrapping them earns nothing yet.
