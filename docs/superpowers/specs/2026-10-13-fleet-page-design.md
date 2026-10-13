# FLEET web page — design

Date: 2026-10-13. Status: draft for Andy's review.

## Goal

FLEET is a long-running Claude session that runs Andy's fleet of detached
Claude and Grok sessions (scan, relay decisions, spawn, merge, kill). Today
all of its state lives in the terminal. Three problems, in Andy's words:

- **Hard to scan.** Long messages scroll away. No single view shows all open
  decisions.
- **Away from desk.** Andy cannot see state or decide from the phone.
- **Lost history.** No record of decisions, merges, kills and filed tickets
  over time.

The page fixes these three. It shows the current state as cards, lets Andy
answer with one click or in free text, mirrors the FLEET conversation, and
keeps a history timeline.

## Decisions already made (brainstorm, 2026-10-13)

| Topic | Decision |
|---|---|
| Hosting | Self-hosted, reachable only over Tailscale. |
| Where the code lives | A `/fleet` page inside the claude-sessions server (approach A). |
| Input path | Page input is typed into the FLEET tmux pane. It arrives as Andy's own message in the FLEET conversation, so an explicit yes on the page counts as an explicit yes. |
| FLEET view | Structured cards + a chat mirror of the FLEET transcript + history. |

## Non-goals (v1)

- Multi-user access. Andy is the only user.
- Actions on worker sessions from the page. FLEET stays the only actor; the
  page never relays, kills or merges by itself.
- Push notifications. FLEET already uses `PushNotification`.
- Any decision logic in the server. The server reads files and types text.

## Architecture

```
browser (phone / laptop, on the tailnet)
   │  bearer token (cookie after first visit)
   ▼
claude-sessions server, main mux (Tailscale listener)
   ├─ GET  /fleet            embedded HTML+JS (go:embed fleet.html)
   ├─ GET  /fleet/state      fleet-state.json  +  FLEET session liveness
   ├─ GET  /fleet/chat       FLEET transcript turns after a cursor
   ├─ GET  /fleet/log        fleet-log.jsonl entries (newest first, paged)
   ├─ POST /fleet/say        sendKeys into the FLEET pane
   └─ (reused) GET /usage/numbers, GET /sessions
```

Files (written by FLEET, read by the server):

- `~/.claude/fleet/state.json` — the current cards. FLEET rewrites it
  atomically (write temp file, rename) at the end of every cycle and after
  every relay.
- `~/.claude/fleet/log.jsonl` — append-only history, one JSON object per line.
  The server also appends one `web-say` entry per `POST /fleet/say`.

The server makes no decisions. It never parses card meaning; it only serves
the JSON and types text.

## FLEET session discovery

The server finds the FLEET session in the local session list
(`cachedSessions()` + flags overlay) by `Name == "FLEET"` (the name FLEET
already carries in the list). Exactly one match is required:

- 0 matches → `state.fleet.alive = false`; `/fleet/say` returns an error.
- 2+ matches → same, with reason `ambiguous`; the page shows the reason.

The server stores the matched `session_id` and re-resolves it before every
send with `resolveLivePID`, so a recycled pid never receives text.

## Data contracts

### `state.json`

```json
{
  "version": 1,
  "writtenAt": "2026-10-13T09:40:00Z",
  "cycleNote": "one line: what FLEET is doing now",
  "cards": [
    {
      "id": "dr-5011-sandbox-retry",
      "kind": "explicit-yes | needs-you | working | waiting | done",
      "ticket": "DR-5011",
      "title": "TRAX print class — sandbox retry + check",
      "summary": "2–4 short lines, STE100",
      "lean": "A",
      "options": [
        { "key": "A", "label": "Yes, run it", "line": "one-line effect",
          "send": "Andy decides: DR-5011 sandbox retry + check — yes (explicit)." },
        { "key": "H", "label": "Hold", "line": "keep waiting",
          "send": "Andy decides: DR-5011 sandbox retry — hold." }
      ],
      "links": [ { "label": "PR #2744", "url": "https://github.com/..." } ],
      "session": "spawn-dr-5011-trax-print-class-bf5179",
      "waitingOn": "Rinoj | Dzhamal | Vish | Andy | null",
      "updatedAt": "2026-10-13T09:38:00Z"
    }
  ]
}
```

Rules:

- `id` is stable across rewrites, so the page can keep a card's "sent"
  marker until FLEET updates or removes it.
- `options` is present only for `explicit-yes` and `needs-you`.
- `send` is the exact text typed into the FLEET pane. FLEET writes it; the
  page never builds decision text itself (only free chat).
- A `lean` key must match one option key. The page adds a "Lean" button that
  sends the lean option's text with " (lean)" appended.

### `log.jsonl`

```json
{"at":"2026-10-13T09:12:00Z","type":"merge|kill|spawn|ticket|decision|ruling|relay|web-say|note",
 "ticket":"DR-4916","text":"PR #2621 merged as 0893d3dd4a","url":"https://github.com/..."}
```

FLEET appends one line per merge, kill, spawn, filed ticket, relayed decision
and expert ruling. The server appends `web-say` lines. Nothing rewrites the
file. `GET /fleet/log?before=<at>&n=50` pages backwards.

### `/fleet/state` response

`state.json` as stored, plus a server-built block:

```json
{ "fleet": { "alive": true, "sessionId": "1ecacebf-…", "status": "busy|idle|waiting",
             "reason": null },
  "stale": false,
  "stateError": null }
```

- `stale` is true when `writtenAt` is older than 40 minutes (FLEET's idle
  monitor re-arms every 30 minutes, so a healthy FLEET writes at least that
  often).
- On unreadable or invalid JSON the server returns the last good copy from
  memory and sets `stateError`. It never returns 500 for bad state content.

### `/fleet/chat`

Reuses `findTranscript` and the turn extraction behind `GET /transcript-tail`,
with two changes for this route only: `n` up to 200, and a `after=<uuid>`
cursor so the page polls only new turns. User turns that are tool results or
system notices are filtered out (same rule as the existing tail). Turns are
returned as `{uuid, role, at, text}`; long texts are not capped (the page
folds them).

## Page

Single embedded HTML file with inline CSS and vanilla JS. No build step, no
external CDN (works on the tailnet without internet).

Layout, top to bottom (phone stacks in this order; desktop puts the card
columns side by side):

1. **Header:** "FLEET", last state write ("3 min ago"), FLEET status
   (busy / idle / not running), quota from `/usage/numbers`
   (Claude 5h, Claude 7d, Grok).
2. **Banner** when `stale`, `fleet.alive=false` or `stateError`.
3. **Needs your explicit yes** (red), then **Needs you** (amber): one card
   each. Collapsed card = title line, lean, one button per option, "Lean"
   button, 💬 button. Tap the title to expand summary + links.
4. **Chat:** mirror of the FLEET conversation, newest at the bottom, long
   turns folded after ~12 lines. Input box + Send. 💬 on a card pre-fills
   "About DR-XXXX: ".
5. **Working / Waiting / Done today:** compact lists (title, session, waiting-on).
6. **History:** the log, newest first, filter chips by `type`, link per entry.

Polling: `/fleet/state` every 5 s, `/fleet/chat?after=` every 3 s,
`/usage/numbers` every 60 s. Pause polling when the tab is hidden.

### Decision flow

1. Andy presses an option button.
2. The page shows a confirm sheet with the exact `send` text.
   - `needs-you`: one tap on "Send" sends it.
   - `explicit-yes`: the sheet names the action in red and needs a second,
     separate confirm tap ("Yes, I approve this write").
3. `POST /fleet/say {text, cardId}`.
4. The card shows "sent · waiting for FLEET" until a newer `state.json`
   changes or drops that card id.
5. The text appears in the chat mirror as Andy's turn; FLEET's answer follows.

## `/fleet/say` behaviour

- Body: `{ "text": string (1..4096), "cardId": string|null }`.
- Re-resolve the FLEET session (see discovery). Refuse with a clear error if
  not alive or ambiguous.
- Type with the existing `sendKeys` (`send-keys -l` + `Enter`). Claude Code
  queues a message typed while it is busy, so no server-side queue is needed.
  The response says `queued: true` when the status was busy.
- Refuse when the FLEET pane's input line already holds text (another
  device or Andy at the terminal is mid-typing): capture the pane, read the
  prompt line, and return `{error: "input box not empty"}`. The page shows it
  and keeps the text.
- Append a `web-say` log line `{text, cardId}`.
- Rate limit: max 1 send per second; a second identical text within 10 s is
  refused as a double tap.

## Auth and security

- Reuse the existing bearer token (`~/.config/claude-sessions/server-token`,
  `s.authed`). No new secret.
- First visit: `GET /fleet?t=<token>`. The server checks it, sets cookie
  `cs_fleet=<token>` (`HttpOnly; Secure` only when served over https;
  `SameSite=Strict`; `Path=/fleet`; 30 days) and redirects to `/fleet`
  without the query, so the token leaves the URL bar and history.
- `/fleet/*` routes accept the bearer header OR that cookie. No other route
  accepts the cookie.
- `POST /fleet/say` additionally requires `Origin` (or `Referer`) to match
  the request host, as a CSRF guard on top of `SameSite=Strict`.
- Routes go on the main mux only (Tailscale listener). They are not added to
  the loopback paste mux.
- A helper `claude-sessions fleet-url` prints the one-time login URL with the
  token, for Andy to open on each device. It never writes the token anywhere
  else.
- The page renders all card and chat text as text (no `innerHTML` with
  untrusted content), except link URLs, which must be `http(s)`.

## FLEET-side changes (outside this repo)

In `~/.claude/skills/fleet/`:

- Write `state.json` at the end of each cycle and after each relay, from the
  same three buckets FLEET already reports. A small helper
  `scripts/fleet-state.py` validates and atomically writes the file from a
  JSON draft FLEET produces, so a malformed draft never replaces a good file.
- Append to `log.jsonl` on merge, kill, spawn, ticket, relay, ruling (helper
  `scripts/fleet-log.sh <type> <ticket> <text> [url]`).
- SKILL.md gains one short section: "Keep the web page current".
- A message that arrives from the page looks like any typed message. FLEET
  treats it the same way, including the explicit-yes rule.

## Error handling summary

| Case | Server | Page |
|---|---|---|
| FLEET not running | `alive=false`, say refused | red banner, Send disabled, cards read-only |
| Two FLEET sessions | `alive=false`, reason `ambiguous` | banner names the reason |
| state.json missing | empty cards, `stateError` | banner "FLEET has not written state yet" |
| state.json invalid | last good copy + `stateError` | warning banner |
| state older than 40 min | `stale=true` | amber banner with age |
| Pane input not empty | say refused | toast, text kept in box |
| Network error | — | retry with backoff, "offline" dot in header |

## Testing

- Go unit tests (httptest, existing seams):
  - auth: header ok, cookie ok on `/fleet/*`, cookie rejected on `/sessions`,
    `?t=` sets cookie and redirects, wrong token 401;
  - discovery: 0 / 1 / 2 FLEET sessions;
  - state: valid, missing, invalid (last good copy), stale flag;
  - chat: cursor returns only newer turns, tool-result turns filtered;
  - say: not alive, input-box-not-empty, double-tap refusal, Origin mismatch,
    log line appended; `sendKeysFn` seam asserts the exact text.
- Manual: open on phone over Tailscale, answer a test card, see the text in
  the FLEET terminal and the reply in the chat mirror.
- `go build ./... && go vet ./... && go test ./...`, then the repo's normal
  merge → `make install` → push → service restart flow.

## Open points for Andy

1. Is the 40-minute stale threshold right?
2. Should "Done today" reset at local midnight (Europe/Vilnius) or UTC?
