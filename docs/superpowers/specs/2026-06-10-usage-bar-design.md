# Usage bar: account rate-limit progress bars in the TUI header

**Date:** 2026-06-10
**Status:** approved

## Goal

Show the Claude account's rate-limit utilization — the same 5-hour and weekly
numbers `/usage` reports — as two compact progress bars at the top of the live
TUI, refreshed in the background so the UI never blocks on the network.

## Data source

Anthropic's OAuth usage endpoint (the one Claude Code's `/usage` uses):

```
GET https://api.anthropic.com/api/oauth/usage
Authorization: Bearer <accessToken>
anthropic-beta: oauth-2025-04-20
```

Response fields we consume (everything else ignored):

```json
{
  "five_hour": {"utilization": 9.0,  "resets_at": "2026-06-10T15:19:59Z"},
  "seven_day": {"utilization": 13.0, "resets_at": "2026-06-10T18:00:00Z"}
}
```

`utilization` is a percentage (0–100). Per-model buckets
(`seven_day_sonnet`, …) are deliberately not shown.

### Credentials

The access token is Claude Code's own OAuth token; we only read it, never
refresh or rotate it (Claude Code manages its lifecycle).

- **darwin:** `security find-generic-password -s "Claude Code-credentials" -w`
  (exec'd, no cgo — single-binary deployment is preserved)
- **linux/other:** read `~/.claude/.credentials.json`

Both yield JSON with `claudeAiOauth.accessToken`. The token is re-read on
every fetch so rotation by Claude Code is picked up automatically.

## Architecture

New file `usage.go`, mirroring the `RemoteHub` pattern (background poller +
mutex-protected snapshot read at render time):

- `UsageInfo` — parsed result: two buckets, each `{Pct float64, ResetsAt time.Time}`.
- `loadOAuthToken() (string, error)` — per-OS credential read as above.
- `fetchUsage() (*UsageInfo, error)` — HTTP GET, 5s timeout.
- `UsageHub` — goroutine started by `RunTUI`:
  - fetches every **120s** (independent of the TUI tick interval)
  - `Kick()` for an immediate refetch, wired to the `r` key
  - `Snapshot() *UsageInfo` returns the last successful result (nil until the
    first fetch lands, or if usage is unavailable)
  - no wake pipe: the TUI repaints on its own tick anyway, and a slightly
    stale percentage is acceptable. The render loop is never blocked — first
    paint happens with no bar, and the bar lazily appears on a subsequent
    repaint once the first fetch completes.
  - `Shutdown()` stops the goroutine.

## Rendering

`RenderAll` gains a `usage *UsageInfo` parameter. When non-nil, all three view
modes (full / intermediate / minimal) print two lines between the title line
and the column header:

```
Claude sessions  14:02:11  (4 live, 3 in tmux)
5h  ██░░░░░░░░░░░░░░░░░░   9%  resets 17:19
wk  ███░░░░░░░░░░░░░░░░░  13%  resets Wed 20:00
```

- Bar is 20 cells: `█` filled, `░` empty, filled = round(pct/5).
- Color by utilization: default <70%, yellow (33) 70–89%, red (1;31) ≥90%.
- Reset times rendered in local time: `HH:MM` for the 5h bucket,
  `Ddd HH:MM` for the weekly bucket.

## Failure handling

Missing token, exec failure, network error, non-200, or unparseable body all
result in `Snapshot() == nil` → the two lines are omitted entirely. No error
text, no layout reservation; the table just sits one section higher. A
previously successful snapshot is kept visible if a later refresh fails
(same "don't blink to blank" behavior as remote sections).

## Out of scope

- `list --once` / scriptable subcommands never fetch usage (callers pass nil).
- Server role does not expose usage; remote hosts' accounts are assumed to be
  the same account (revisit if that changes).
- Per-model buckets, extra-usage credits.

## Verification

`go vet ./...`, `go build .`, then run the TUI and confirm: bar appears
within a few seconds of startup, UI is responsive before it appears, `r`
refreshes it, and deleting/renaming the credential gracefully hides it.
