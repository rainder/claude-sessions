# Resume Session Picker — Design

Date: 2026-07-24. Status: approved.

## Goal

`r` key in the TUI opens a searchable picker of past (ended) Claude Code
sessions on the local host and every configured remote host. Selecting one
resumes it via `claude --resume <session-id>` inside a fresh tmux session on
the host that owns the transcript.

## Data source

Claude Code transcripts: `~/.claude/projects/<encoded-cwd>/<session-id>.jsonl`.
Session ID = filename stem; age = file mtime. The encoded dir name is lossy
(hyphen mangling), so `cwd` is always read out of the transcript head, never
reconstructed from the dir name (see picker.go:70 warning).

## New collector — `resume.go`

```go
type ResumableSession struct {
    SessionID    string    `json:"session_id"`
    CWD          string    `json:"cwd"`
    GitBranch    string    `json:"git_branch,omitempty"`
    FirstPrompt  string    `json:"first_prompt,omitempty"`
    MessageCount int       `json:"message_count"`
    ModifiedAt   time.Time `json:"modified_at"`
    Host         string    `json:"-"` // "" local, set client-side for remote rows
}

func CollectResumable() []ResumableSession
```

Rules:
- Glob `~/.claude/projects/*/*.jsonl`.
- Skip: zero-byte files; mtime older than 30 days; SessionID currently live
  (present in `CollectLocal()` output); unreadable/corrupt files (skip
  silently, never fail the whole scan).
- Parse first ~30 lines for `cwd`, `gitBranch`, and first user-role prompt
  (truncated to ~60 chars, whitespace-collapsed). Extend the approach of
  `extractCWDFromJSONL` (picker.go:151) rather than duplicating it.
- MessageCount = newline count of the file (30-day window keeps this cheap).
- Sort mtime desc, cap at 100 entries.

## Server — two endpoints (server.go, same Bearer auth via `authed`)

- `GET /resumable` → `{"sessions": [ResumableSession...]}` from
  `CollectResumable()` in-process.
- `POST /sessions/resume` body `{"session_id": "...", "cwd": "..."}` →
  validates the transcript file exists for that session id, refuses (409) if
  the SessionID is already live, then spawns using `MigrateLocal`'s tmux
  pattern (migrate.go:81-84): `tmux new-session -d -s <name> -c <cwd>` +
  `send-keys "claude --resume <id>" Enter`, name via `MakeTmuxName`.
  Returns tmux session name.

Both the handler and the local TUI path call one shared primitive
`ResumeSession(sessionID, cwd string) (tmuxName string, err error)` in
`resume.go` (validation + liveness check + spawn) — no duplicated logic,
matching the repo's server/subcommand symmetry rule.

## TUI — `r` key

- Key `r`/`R` in the main event loop (tui.go event loop; `r` confirmed
  unbound) opens the picker. Reuse the generic `newPickerState` engine
  (new_picker.go): lines-based list, built-in incremental case-insensitive
  substring filter (typing filters immediately — no `/` prefix needed),
  arrow/enter/esc handling.
- Row format: `AGE  HOST  PROJECT-DIR  BRANCH  #MSGS  FIRST-PROMPT`,
  columns aligned; age humanized (`5m`, `3h`, `2d`); project dir shortened
  with `~`. Local rows first, then remote, all merged into one filterable
  list; remote rows carry host label like the main session table.
- Remote lists fetched concurrently on picker open (goroutine per host,
  short timeout), pattern of `fetchRemotePresets` (remote_actions.go:204).
  Unreachable host → note in picker footer, others still listed.
- Enter on local row → local spawn (same primitive the server handler uses).
  Enter on remote row → `POST /sessions/resume` to that host, then offer
  ssh-attach exactly like existing remote spawn flow (`actNewRemote`
  routing, remote_actions.go:379).
- Esc cancels. Empty result set → message + any-key dismiss.

## Errors

- Corrupt/unparsable jsonl → skipped.
- Resume POST failure → error line in TUI, picker state preserved is not
  required (return to main table is fine).
- Already-live session chosen from a stale list → server 409, shown as error.

## Testing

- `resume_test.go`: fixture jsonl files (normal, corrupt line, missing cwd,
  zero-byte, old mtime) → collector output assertions.
- First-prompt extraction + truncation; age formatting.
- Row building/filter integration test in render/picker style of existing
  tests.
- `go test ./...`, `go vet ./...`, race run per repo convention.

## Out of scope

Resuming into an *existing* tmux window, transcript migration across hosts,
deleting old transcripts.
