# Kill Tmux Session and Collapse True Home Directory

Date: 2026-07-21

## Goal

Make `k` terminate the entire tmux session containing the selected Claude Code session. Also render working directories under each machine's actual home directory with a leading `~`, for both local and remote rows.

## Current Behavior and Root Cause

The TUI stores tmux location metadata on each collected `Session`. The kill confirmation uses that metadata and can say that a tmux session will be killed. `KillSession`, however, accepts only a PID and performs a second process-tree and tmux lookup. If that second lookup misses, it silently falls back to terminating only the Claude process. This creates a time-of-check/time-of-use split between displayed behavior and executed behavior.

Local rendering obtains the current user's home directory through `os.UserHomeDir()`. Remote session responses do not include the remote machine's home directory, so the client cannot safely collapse remote absolute paths to `~`.

## Kill Design

### Session-Based Kill Contract

Change the kill primitive to consume trusted session metadata rather than a bare PID.

- When `Session.Tmux` is non-empty, extract the tmux session name and execute `tmux kill-session -t <name>`.
- When `Session.Tmux` is empty, preserve the existing direct process termination behavior: send SIGTERM, wait up to five seconds, then send SIGKILL if needed.
- If a known tmux target cannot be killed, return the tmux error. Do not fall back to PID-only termination because that would contradict the confirmed action and leave the tmux session alive.

### Callers

- Local TUI action passes the selected `Session` directly.
- Remote kill endpoint resolves the requested PID against server-collected live sessions and passes the matching server-derived `Session`. The client continues sending only the PID, so it cannot name an arbitrary tmux session.
- Scriptable `kill` resolves current tmux metadata before calling the session-based primitive.

### Tmux Target Parsing

`Session.Tmux` retains its existing `session:window.pane` format. Tmux session name extraction remains centralized in the kill implementation. Empty or malformed values must produce explicit behavior: empty means PID termination; malformed non-empty metadata returns an error rather than selecting an unintended tmux target.

## Home Directory Design

Add a derived `Home` field to `Session` and include it in JSON responses.

- `CollectLocal` reads `os.UserHomeDir()` once and stores that value on every returned session.
- Remote servers naturally serialize their own home directory with each session.
- Remote clients preserve the received value when adding the configured host name.
- Older servers omit the field; clients then leave the absolute path unchanged.

Rendering collapses only valid home boundaries:

- `/home/andy` becomes `~`.
- `/home/andy/project` becomes `~/project`.
- `/home/andy-other/project` remains unchanged.
- Empty home values leave paths unchanged.

The renderer uses each row's `Session.Home`, not a hardcoded path and not the client's home for remote rows.

## Error Handling and Safety

- Known tmux membership is an execution contract, not a best-effort hint.
- Tmux command failures are shown to the user and never trigger PID fallback.
- Remote tmux targets come only from server-side process inspection.
- Missing home metadata degrades to the existing absolute-path display.
- JSON compatibility is additive: old clients ignore `home`, and new clients accept old responses without it.

## Testing

Add focused unit and handler tests covering:

1. A session with tmux metadata invokes `tmux kill-session` for the expected session name.
2. A session without tmux metadata follows direct PID termination.
3. A tmux command failure returns an error without direct PID fallback.
4. The remote kill handler resolves and uses server-derived tmux metadata.
5. Local and remote session paths under their true home directories collapse to `~`.
6. Exact home paths collapse to `~`.
7. Empty home values and false prefix matches remain unchanged.
8. Existing render behavior remains compatible with sessions lacking `Home`.

Run `go test ./...` and `go vet ./...` after implementation.

## Scope

No tmux socket-selection changes, process-group redesign, configurable home overrides, or unrelated rendering refactors are included.
