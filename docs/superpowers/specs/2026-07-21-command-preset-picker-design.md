# Configurable command presets in the new-session picker

Date: 2026-07-21
Status: approved design, pre-implementation

## Goal

Let users configure terminal commands used to start new tmux sessions and cycle
between those commands with the left and right arrow keys inside the `n`
new-session view.

The selected preset applies to local and remote session creation. The last
confirmed preset is remembered. Existing installations without command config
continue to run `claude`.

## Configuration

Extend the existing restricted YAML configuration with a second top-level flat
list:

```yaml
commands:
  - name: Claude
    command: claude
  - name: ClaudeX
    command: claudex
  - name: Fable
    command: claude --model fable

servers:
  - name: beluga
    host: 100.64.0.2
    port: 8765
    token: example
```

Each command preset has:

- `name` — human-readable picker label and stable preset identifier.
- `command` — exact trusted terminal text entered into the new tmux pane.

Validation rules:

- Trim surrounding whitespace from both fields.
- Ignore entries with a blank name or blank command.
- Keep the first entry when names are duplicated; ignore later duplicates.
- Preserve valid entries in file order.
- If the `commands:` block is missing or contains no valid entries, expose one
  fallback preset: `Claude` with command `claude`.

Both top-level lists remain in the existing `servers.yaml` file; the filename is
retained for compatibility even though it now also stores command presets. The
parser remains intentionally limited to top-level lists of flat scalar mappings.
No flow style, anchors, nested values, multiline command strings, or
shell-argument arrays are added.

## Loading API

Parse the file into one internal configuration value containing both servers and
command presets. Keep existing server callers source-compatible through
`LoadServerConfigs`, and add a command-specific loader for new-session flows.

Command preset loading is needed by both client mode and server mode:

- the local client renders and resolves its configured presets;
- the remote server resolves a requested preset name against its own allowlist.

The same preset names must exist on the client and remote server. Command text
may differ by machine, allowing wrappers or executable paths to be host-specific.

## Remembered selection

Persist the last confirmed preset name in the existing config directory, using a
small state file separate from the YAML configuration.

- Missing state selects the first configured preset.
- A remembered name no longer present selects the first configured preset.
- Confirming the new-session modal saves the selected name immediately.
- Canceling the modal does not change the saved name.
- Spawn failure or later cancellation of manual path input does not roll back the
  confirmed preset preference.

The YAML file order therefore defines the initial and fallback default.

## Two-axis new-session modal

Replace the one-dimensional CWD picker with a dedicated new-session modal:

```text
 New session on beluga

 Command:  ◀ Fable ▶
           claude --model fable

 ▶ enter path manually…

 ↑/↓ cwd · ←/→ command · Enter select · q cancel
```

Keyboard behavior:

- Up/down cycles CWD rows with wraparound.
- Left/right cycles command presets with wraparound.
- Enter confirms the selected CWD row and command preset.
- Existing numeric shortcuts confirm that CWD row with the currently selected
  preset.
- `q`, `Q`, Escape, and Ctrl-C cancel.

The header displays both the preset name and the command text from the local
configuration. On a remote target, this is a preview of the local mapping; the
remote executes its own allowlisted command text for the same preset name. The
picker stores indices, so command cycling does not disturb the selected CWD and
CWD cycling does not disturb the selected command.

Use a small pure picker-state unit for key transitions and a terminal-rendering
wrapper for the blocking modal. This keeps wraparound and confirmation behavior
unit-testable without a real terminal.

## CWD rows by target

### Local target

Keep the existing recent/default CWD list plus `enter path manually…`.

Selecting a listed directory launches immediately after modal confirmation.
Selecting manual entry switches to cooked input, expands local `~`, validates
locally, and then launches.

### Populated remote target

Replace the current direct cooked `readLine` prompt with the same two-axis modal
used locally. Place the selected remote session's CWD first, merge the remote
server's ranked historical suggestions, deduplicate, then append
`enter path manually…`.

Selecting a listed directory sends it directly. Selecting manual entry switches
to cooked input. Remote tilde expansion and directory validation remain on the
remote server.

### Empty remote target

Show the remote server's ranked historical suggestions plus
`enter path manually…`. This lets a reachable host with no live sessions offer
previously used project paths instead of forcing manual entry.

## Remote CWD suggestion endpoint

Add an authenticated, on-demand endpoint:

```http
GET /cwd-suggestions
```

```json
{
  "suggestions": [
    {"cwd": "/home/andy/Developer/claude", "count": 8},
    {"cwd": "/home/andy/Developer/other", "count": 3}
  ]
}
```

The endpoint reuses the same session-file and transcript-history signals as the
local picker. Refactor collection/ranking into a shared pure-enough unit so local
and server paths cannot drift. The shared collector applies `isDir` to every
source, including session JSON files; this intentionally removes stale missing
session paths from the local picker as well as the remote response.

Server response rules:

- include existing directories only, regardless of source;
- exclude hidden temporary/private paths using the existing `hiddenCwd` rule;
- include historical/session-derived entries with positive frequency counts;
- exclude the server daemon's current working directory fallback;
- sort by count descending, then path ascending;
- cap the response at 15 entries;
- require the same bearer authentication as other remote endpoints;
- accept no path/query input, preventing arbitrary filesystem browsing.

The client fetches this endpoint synchronously only when `n` opens a remote
new-session modal. Use a dedicated 5-second HTTP timeout for this passive lookup
rather than the 30-second mutating-action timeout. It does not add suggestion
scanning to periodic remote polling.

Client merge rules:

1. add the selected remote session CWD first when non-empty;
2. append server suggestions in response order;
3. deduplicate by exact path;
4. append `enter path manually…`;
5. preserve the selected session CWD as the initial row.

If the request fails or the response cannot be decoded, creation remains usable:
the modal contains the selected session CWD when available plus manual entry, and
displays a dim `remote suggestions unavailable` note. An empty host therefore
falls back to manual entry only.

## Spawn execution

Change `SpawnNew` to receive explicit command text:

```go
SpawnNew(cwd, displayName, command string) (string, error)
```

It creates the tmux session as today, then passes the configured command as one
`tmux send-keys` argument followed by `Enter`. The command is intentionally
shell text: tmux types it into the pane's shell, allowing configured flags and
wrappers such as `claude --model fable` or `claudex`.

Command presets are trusted local administrator configuration. They may contain
shell syntax deliberately. The UI never accepts arbitrary command text from the
interactive user.

Existing callers change as follows:

- local TUI resolves the selected preset and passes its command;
- shell `new` subcommand uses the first configured preset, preserving `claude`
  when no commands are configured;
- migration/resume behavior is unchanged and continues to use its existing
  Claude resume command.

Adding a `new --command` CLI option is out of scope.

## Remote protocol and allowlist

Extend `POST /sessions/new` with the preset name, not raw command text. The
request struct should comment this field explicitly as a preset name because its
JSON key remains `command` for concise wire compatibility:

```json
{
  "cwd": "~/Developer/claude",
  "name": "",
  "command": "Fable"
}
```

The server:

1. authenticates the request;
2. expands and validates the remote CWD as today;
3. loads its configured command presets;
4. resolves the requested preset name;
5. rejects an unknown or blank preset with a clear action error;
6. passes the server-side command text to `SpawnNew`.

This keeps `/sessions/new` as a preset allowlist instead of turning the bearer
token into an arbitrary remote shell-command endpoint.

For backward compatibility, a request that omits `command` resolves the first
configured preset. With no `commands:` block, that remains `Claude → claude`.
This allows an updated server to accept older clients.

An updated client talking to an older server sends an extra JSON field that the
Go decoder ignores, so the older server continues to launch plain `claude`.

## Error behavior

- Invalid local command config: invalid entries are ignored; fallback applies if
  none remain.
- Remembered preset removed: first configured preset is selected.
- Remote preset name absent from server allowlist: return
  `command preset not configured: <name>` without creating tmux.
- Invalid CWD: keep existing local or remote path error behavior.
- `tmux new-session` or `tmux send-keys` failure: keep existing error wrapping.
- Modal cancel: no state change and no spawn.

## Testing

### Configuration tests

- Parse interleaved top-level `commands:` and `servers:` blocks.
- Preserve command order and quoted scalar command text.
- Ignore blank and duplicate presets.
- Missing or wholly invalid command list returns `Claude → claude`.
- Existing server parsing remains unchanged.

### Persistence tests

- Missing remembered state selects index zero.
- Valid remembered name resolves to its current index.
- Stale remembered name falls back to index zero.
- Save/load round trip preserves preset name.

### Picker tests

- Left/right wrap command indices without changing CWD index.
- Up/down wrap CWD indices without changing command index.
- Enter and numeric keys confirm both current values.
- Cancel keys return cancellation without changing persisted selection.
- Rendered modal contains preset label, command text, and updated key hint.

### Spawn tests

Use a fake `tmux` executable on `PATH` to capture argv:

- `SpawnNew` sends selected command as the `send-keys` argument.
- Default fallback sends `claude`.
- Command with flags remains one tmux argument before `Enter`.

### Remote tests

- Missing command field resolves first configured preset.
- Known preset resolves server-side command text.
- Unknown preset returns allowlist error before tmux creation.
- Request cannot supply raw command text outside a configured preset.
- Existing remote tilde-expansion regression remains green.
- CWD suggestion endpoint rejects unauthenticated requests.
- Suggestion response is ranked, filters hidden/missing paths, excludes daemon
  working directory fallback, and caps at 15.
- Client merge places selected remote CWD first and deduplicates it against
  server history.
- Empty remote host uses historical suggestions.
- Fetch/decode failure still yields a usable manual-entry modal with the
  unavailable note.

### Full verification

Run:

```sh
go test ./...
go vet ./...
go build .
```

## Documentation

Update README configuration examples and new-session controls:

- document `commands:` syntax and fallback;
- document that remote servers need matching preset names;
- document `←/→` command cycling in the `n` view;
- document on-demand remote path suggestions and graceful manual fallback;
- note that command text is trusted shell input from configuration.

## Out of scope

- Per-host command lists or global-plus-host overrides.
- Arbitrary command entry in the TUI.
- Sending raw command text over the remote API.
- A `new --command` CLI flag.
- Inferring a preset from the selected session's running process.
- Changing migration/resume commands.
- Nested YAML, argument arrays, environment-variable maps, or multiline commands.
