# FLEET Clarify: a side chat about a decision card

Status: design, awaiting Andy's approval. Date: 2026-10-26.

## Why

FLEET puts decision cards in front of Andy: an issue, a few options, often a
lean. Sometimes Andy does not understand the card well enough to pick. He wants
to ask follow-up questions: what FLEET means, and how the parts of the system
the card talks about actually work. He wants that in the same context FLEET
had when it wrote the card.

Today the only way to ask is to type into FLEET itself. That interrupts FLEET's
work and fills its conversation with Q&A that has nothing to do with running
the fleet. And sometimes the chat leads Andy to a better answer than any
option on the card. He wants to tell FLEET that without retyping it.

## What it is

Clarify opens a side chat about one decision card. It works in the TUI's F
view and on the /fleet web page.

- The answers come from a **fork of FLEET's conversation**, taken when Clarify
  first opens on that card. The fork knows everything FLEET knew at that
  moment. FLEET itself never sees the questions and keeps working.
- The answers show **inside the Clarify component**, never in FLEET's chat.
- The fork may **look things up but never change anything** (see "Read-only").
- When the chat ends somewhere better than the card's options, Andy presses
  **Draft for FLEET**. The fork writes a short message saying what Andy
  decided. Andy edits it and sends it, and it goes to FLEET as an **Other**
  answer (`Andy decides on <cardId>: <text>`) through the existing send
  path and all its guards. **Nothing reaches FLEET without Andy's OK.**
- **One side chat per card.** A second Clarify on the same card continues the
  same chat. The chat lives on the server, so the Mac TUI and the phone see
  the same chat. It goes away when the card stops being an open decision.

## Goals

- Ask about a card without interrupting FLEET.
- Answers grounded in FLEET's own context plus live look-ups (code, git, PRs,
  tickets).
- Turn what the chat settled into an answer to FLEET with one draft-and-send
  step.
- The same chat from the TUI (local or remote FLEET) and from the web page.

## Non-goals

- Streaming answers token by token. The client shows "thinking… Ns" and the
  whole answer lands at once.
- More than one chat per card, chat history after the card is gone, or a
  search over past chats.
- Clarify on anything but an open decision card (not on working-list
  sessions, not on done or SENT cards).
- A per-card or per-day limit on questions. Andy accepted the cost: one
  question costs roughly one FLEET turn of the usage limit, mostly cache reads.
- Showing cost per answer.

## Anti-goals (a failure even if everything else works)

- Any text reaching FLEET's pane that Andy did not send himself.
- The fork changing anything: files, git state, a PR, a ticket, a session.
- A fork process showing up anywhere as a session: as a row, as FLEET, as an
  idle/question/blocked event, or in the resume picker. **The grok case is
  the sharp one:** a grok fork of a `/rename`d FLEET carries the title
  `FLEET` with `title_is_manual`, and if it is ever listed while it runs,
  discovery reads two FLEETs, goes `ambiguous`, and every send refuses.

## Decided by Andy

- TUI layout **B**: Clarify is a full screen of its own. The card's title and
  options stay pinned at the top, the chat runs below, an input line sits at
  the bottom. Esc returns to the F view with the same card selected.
- Web layout **B**: a bottom sheet like the existing answer sheet. Card title
  and options pinned at the top of the sheet, list behind it dimmed, chat and
  an input below. A footer line: "fork of FLEET · read-only · FLEET does not
  see this".
- Answers are styled apart from Andy's questions (the mockups used purple for
  the fork's turns).
- The amendment draft appears only when Andy asks for it (press to draft),
  never on the fork's own initiative.
- The fork may run shell commands, but only from a fixed list of look-ups
  (below). Andy confirmed the list is enough.
- The chat persists per card across devices until the card is answered or
  gone.

Mockups: the brainstorm screen `clarify-layout.html` (TUI B and web B are the
chosen ones).

## Read-only: the fork's tool limit

Allowed:

- Reading and searching files (Read, Grep, Glob).
- Shell commands that are only these look-ups: `git log`, `git show`,
  `git diff`, `git status`, `git blame`; `gh pr view`, `gh pr diff`,
  `gh pr checks`, `gh pr list`, `gh run view`; `cu fetch`;
  `claude-sessions list-sessions`, `claude-sessions usage`; `rg`, `grep`,
  `ls`, `cat`, `head`, `tail`, `wc`, `jq`.

Refused: every other command, every file edit or write, web fetches, MCP
tools, subagents. In headless mode a refused tool must be denied
automatically, never prompted (`--permission-mode dontAsk` or the
equivalent), and the fork is told up front to say in its answer when a
look-up was refused, so Andy knows the answer is missing something.

**The fork runs without Andy's own settings** (my call). `~/.claude/settings.json`
allows things like `Bash(gh pr merge:*)`, `Bash(claude-sessions:*)` (which
covers `kill`, `new` and `account switch`), the spawn script and
`mcp__clickup__update_task`, and trecs-brain's project settings add more.
`--allowedTools` adds to those rules rather than replacing them, so under
`dontAsk` an inherited allow would let the fork merge a PR. So the claude fork
runs with `--setting-sources ""` (no user, project or local settings: no
inherited allows, no hooks), `--strict-mcp-config` with no MCP config, and
`--disable-slash-commands`. Not `--bare`: check whether it drops OAuth login
before using it. What the fork knows about FLEET comes from the transcript,
which it keeps; skills FLEET loaded are already in that transcript. The
builder checks live that an isolated fork still logs in with the OAuth
credential and still sees FLEET's conversation. Grok gets the equivalent:
no inherited allow rules (check what grok reads from `~/.grok` and from the
project's CLAUDE.md-compatible files, and turn off what grants tools).

The allowlist is enforced by the CLI's own permission rules, not by asking the
model nicely:

- Claude: `--allowedTools` with `Bash(<prefix>:*)` rules plus the read tools,
  `--disallowedTools` for Edit/Write/NotebookEdit/WebFetch/WebSearch/Task and
  MCP, `--permission-mode dontAsk`.
- Grok: `--allow` rules, `--deny`, `--disallowed-tools`, `--permission-mode
  dontAsk`, plus `--sandbox read-only` **if** that profile still lets `gh` and
  `cu` reach the network. If it blocks the network, use the allowlist alone
  and say so in the code.

**Known escape hatches the builder must close or prove closed**, each with a
test or a recorded live check: write options of allowed commands
(`git diff --output=<file>`, `git log --output`, `rg --pre <cmd>`,
`rg --pre-glob`), output redirection (`cat x > y`, `>>`), compound commands
(`git log; rm -rf`, `&&`, `|` into a non-listed command, `$(...)`,
backticks), and `gh` subcommands that write (`gh pr merge`, `gh pr comment`,
`gh api` with a method). If the CLI's rule matcher cannot refuse one of these,
add a deny rule for it. Record the CLI versions the checks were run against,
as CLAUDE.md does for the account-switch note.

**The look-ups must work under the service** (my call on the fixes; the
builder checks each one live from the running service, not from a shell).
The fork inherits the systemd unit's environment: its baked `PATH` and
`TMPDIR`, and nothing from `~/.zshenv`. So:

- `cu`: the service has no ClickUp token, which is why clickup-events runs
  `~/.local/bin/cu-with-token`. The allowlist names `cu-with-token fetch`
  (the configured `cu` path from clickup-events.json when one is set, else
  `cu fetch`), and the preamble tells the fork to use it.
- `gh`: check it is logged in from the service (its token lives in
  `~/.config/gh`, which should not need the shell's environment).
- `claude` and `grok`: resolve them on the unit's `PATH` and check that both
  log in from the service.

A look-up that cannot work there is dropped from the list, not left in to be
refused every time.

## How a question runs (server)

The server owns every fork. The clients never run a CLI; they call the
server, which is FLEET's host. That makes remote FLEET work with no extra
machinery: the TUI already talks to whichever host runs FLEET.

1. **First question on a card.** Resolve FLEET as `/fleet/say` does (fresh
   discovery; must be exactly one live FLEET). Take its session id and tool.
   Run the fork in **FLEET's transcript folder**: the cwd from the transcript
   head, the way `fleet_start.go` resolves a resume folder
   (`readResumableHeadFn`), falling back to the session's cwd; a grok FLEET
   uses its session cwd. A claude transcript is keyed to the folder it
   started in, so resuming from anywhere else does not find it.
   - Claude: `claude -p --output-format json --resume <fleetId>
     --fork-session --model <FLEET's model> <tool flags>
     --append-system-prompt <clarify preamble> <question>`.
   - Grok: `grok --output-format json --resume <fleetId> --fork-session
     <tool flags> -p <question>`, with the preamble through grok's
     append-rules flag.
   - Store the fork's new session id (`session_id` for claude, `sessionId`
     for grok, both checked live 2026-10-26).
2. **Later questions** resume the fork: `--resume <forkId>` without
   `--fork-session`, same cwd and tool flags. The fork remembers the earlier
   turns (checked live for both tools).
3. Arguments go to `exec` directly, never through a shell. Andy's text is an
   argument, so no quoting rule applies.
4. **The preamble** tells the fork that it is a read-only side chat forked
   from FLEET; names the card (id, title, summary, options, lean); says Andy
   is asking about this card only; says to answer plainly and say when a
   look-up was refused; says it must not act on the fleet and that nothing it
   writes reaches FLEET.
5. **Draft for FLEET** is the same call with a fixed request: write, in one
   short paragraph, the decision Andy reached in this chat, as a message to
   FLEET about card <id>, with no greeting. The result is stored as the
   card's draft, not as a chat turn.

Measured: an answer takes about 10–60 s.

**FLEET is usually mid-turn when Andy asks**, so the first question will
often fork a transcript whose last assistant line is a `tool_use` with no
result yet (or, for grok, a `chat_history.jsonl` being rewritten). The
builder checks this live on a throwaway session made busy on purpose, not
on FLEET. Expected: the fork resumes and simply lacks the unfinished step.
If the fork fails instead (my call on the fallback): retry once after 5 s,
then answer an `error` turn "FLEET is mid-step; ask again in a moment". Do
not refuse every first question while FLEET is busy: FLEET is busy most of
the time, and that would make the feature unusable.

### Limits (builder's defaults, change if they bite)

- One question in flight per card. A second ask while one runs is refused
  (`busy`).
- At most 2 forks running across all cards; a third is refused (`busy`).
- A question is killed after 5 minutes (process group, like `cu`'s timeout)
  and becomes an error turn.
- Andy's question: at most 4000 runes.

## State (server)

A store beside the answer store, same patterns (memory plus
`$XDG_STATE_HOME/claude-sessions/fleet-clarify.json`, 0600, atomic, written
only on a change; `cmdServer` sets the path; tests keep it in memory). Per
card id:

- `tool`, `fleetSessionId` (what was forked), `forkId`, `cwd`
- `cardHash` when the chat started
- `turns`: `{role: "you"|"fleet"|"error"|"note", text, at}`
- `pending`: `{kind: "ask"|"draft", startedAt}` while a fork runs, else absent
- `draft`: `{text, at}` or absent (discard: `POST /fleet/clarify/draft`
  with `{cardId, discard: true}`)

Turn text is stored on disk: this is Andy's own host and his own chat.

**Pruning** runs where the answer store prunes (on `/fleet/state`): an entry
goes when its card is no longer an open decision (answered with `sent`, gone
from state.json, or not a decision kind). A bad state.json prunes nothing.
An `unconfirmed` answer keeps the chat, as the answer store keeps the card.
A running fork for a pruned card is killed. The fork's own session files are
left on disk.

**The card changes** (its hash moves) while a chat exists: keep the chat and
add one `note` turn, "card changed since this chat started". The fork does
not see the new card text unless Andy asks it to look.

**FLEET restarts** (a new session id): an existing chat keeps resuming its own
fork. A new chat forks the new FLEET.

**Server restart**: a `pending` entry found at load becomes an `error` turn
("interrupted by a restart").

## HTTP

All on the main mux only, with fleet auth: bearer header or the `cs_fleet`
cookie, and a cookie POST needs `sameOrigin`, like `/fleet/say`. Strict JSON
bodies (one value, no unknown field, 4 KiB plus the text cap). Errors are
JSON `{code}`; paths and process errors go to stderr only.

- `GET /fleet/clarify?card=<id>` → `{card: <id>, tool, turns, pending,
  draft}`; an unknown card answers an empty chat (not an error).
- `POST /fleet/clarify/ask` `{cardId, text}` → 202 `{pending}`. Refusals:
  `card_gone` (not an open decision), `fleet_not_live` / `ambiguous` (first
  question only: nothing to fork), `busy`, `too_long`, `empty`.
- `POST /fleet/clarify/draft` `{cardId}` → 202 `{pending}`; refuses a chat
  with no turns (`empty`) and the same codes as ask.
- Sending the draft is **not** a clarify route, and the server does not
  track it (my call). The draft is handed to the client's existing Other
  flow, which does the send exactly as it does today (see TUI and Web). That
  flow already owns the bubble, the outbox, Send again and every guard, and
  a held or later-dropped message is already shown there truthfully. A
  `draftSent` stamp would have to guess at an outcome that flow owns, so
  there is none: the draft stays in the chat until Andy discards it or the
  card goes.

`/fleet/state` gains `clarify: {<cardId>: {turns: n, pending: bool, draft:
bool}}` so a card can show that it has a chat without fetching it.

## The fork must stay invisible

Every collector must keep a running or finished fork out of every list:

- Claude headless runs carry a non-`cli` entrypoint, which `CollectLocal` and
  the resume picker already drop. **Verify it live** for `-p --resume
  --fork-session`; do not assume.
- Grok: a `-p` run may register in `active_sessions.json` and its summary
  may carry FLEET's manual title. Verify live what a grok fork writes
  (registry entry, `session_kind`, `parent_session_id`, `title_is_manual`)
  and make sure the fork is never a row, never FLEET, never in the working
  list, never reported by the idle, blocked or question loops, and never in
  the resume picker. The server knows its fork ids and pids; filtering by
  those is acceptable if nothing on disk marks a fork.
- **After the chat is gone** (my call): pruning forgets the fork id, but a
  finished grok fork's `summary.json` keeps `FLEET` with `title_is_manual`
  for the picker's 30 days, and resuming it would make a second FLEET. So
  the server keeps a small permanent list of fork ids
  (`fleet-clarify-forks.json` beside the store, ids only, entries dropped
  after 31 days) that survives pruning, and the resume collector, the live
  collectors and FLEET discovery skip every id on it. Because `/resumable`
  is answered by the same server, remote pickers get the filter for free.
  The fork's files are left on disk (deleting inside `~/.grok` or
  `~/.claude/projects` is not this tool's business). If a live check shows
  claude's `-p` forks are already filtered by entrypoint, the list is still
  applied to both tools: one rule, no tool branch.
- A test for each of these.

## TUI

- Key `k` on a selected open decision card opens Clarify (`c` is already
  correction). The footer shows `k clarify` only on such a card. A card row
  with a chat shows a small marker (builder's choice of glyph, dim).
- Clarify screen (layout B), drawn by its own renderer and owned by the F
  view's modal loop like the confirm and Other panels:
  - Top: `Clarify  <ticket>  <title>` and a dim line with the options and
    FLEET's lean, pinned.
  - Middle: the turns, wrapped, newest at the bottom, scrolled with PgUp/PgDn
    and the wheel; `you` and `fleet` labels in different colors; an `error`
    turn in the warn color; a `note` turn dim.
  - While pending: a `fleet thinking… Ns` line.
  - A draft shows as a boxed block above the input.
  - Bottom: the input line, then the help row:
    `Enter ask · Ctrl-D draft for FLEET · Esc back`, and with a draft
    `s use draft as answer · x discard draft`.
- Enter on the input asks (same paste rules as the Other panel: never on a
  key batch, and the 400 ms arm). `s` closes Clarify and opens the card's
  existing Other panel (`o`) with the draft already in its box. Andy edits
  it there and sends with the armed Enter, as with any Other answer: the
  same send job, host pin and "FLEET moved" refusal. There is no separate
  `e`: the Other box is the editor.
- Answer text is untrusted LLM text: it goes through `fleetDisplayText`, like
  every other string the view draws. Links follow the summary rules.
- While Clarify is open the view polls `GET /fleet/clarify` every 2 s while
  something is pending, else at the normal state poll. Closing Clarify with a
  question pending is fine; the answer is there when it is reopened.

## Web

- A **Clarify** button beside Other on each open decision card. A card with a
  chat shows a small "Clarify · n" count.
- The bottom sheet (layout B) reuses the answer sheet's structure:
  - Pinned head: ticket, title, the options with FLEET's lean marked.
  - The chat: Andy's turns right, the fork's left with its own accent; turns
    go through `renderRich` (the existing renderer, no new HTML sinks).
  - While pending: "thinking… Ns".
  - Input and Ask button; a **Draft for FLEET** button once there is a chat.
  - A draft shows in a box with **Use as answer** and **Discard**. Use as
    answer does what the Other button does today: it closes the sheet and
    prefills the FLEET chat composer with `Andy decides on <cardId>: `
    followed by the draft (`prefillChat`). Andy edits and sends from the
    composer, so the bubble, outbox and Send again work as they do now.
  - Footer: "fork of FLEET · read-only · FLEET does not see this".
- Polls `GET /fleet/clarify` every 2 s while the sheet is open and something
  is pending.
- `fleet_page_test.go`'s HTML-sink rule covers the new code.

## Left to the builder

- Exact store and route names, file split (a `fleet_clarify.go` beside
  `fleet_answered.go` is the natural home), the marker glyph, colors.
- How the fork runner is injected for tests (a seam like `s.spawn`; no test
  runs a real CLI).
- The exact allow/deny rule syntax per CLI, proved by the escape-hatch checks
  above.
- Whether grok's preamble rides `--rules` or a prompt prefix.

## Docs

CLAUDE.md gets a "Clarify" section under the FLEET web page and the TUI view:
the fork, the cwd rule, the tool limit and its checks, the invisibility rule,
the store and pruning, and the draft-through-Other rule. CHANGELOG entry.
