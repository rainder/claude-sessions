# FLEET in the TUI — Phase 1 design

Date: 2026-10-22. Status: draft for Andy's approval.

## Intent

Andy answers FLEET's decision cards on the phone through `/fleet`. At the
terminal he has to open the page in a browser to do the same thing. Phase 1
puts the actionable cards inside the existing `claude-sessions` TUI: see the
cards, choose an option, add a comment, send. Nothing else.

The page and the TUI will both answer cards on the same day. So Phase 1 also
moves the "answered" record from the page's `localStorage` to the server, so
that both clients see one list and the server can refuse a second answer to
the same card.

## Goals

1. A Fleet view in the TUI, opened with `F`, showing `explicit-yes` and
   `needs-you` cards and a Sent strip.
2. Choose an option, optionally add a comment, confirm, send through the
   local service's `POST /fleet/say`.
3. A server-side answer store shared by the TUI and the page, with a guard
   against a second answer to an unchanged card.
4. A FLEET badge in the session-list header (`FLEET 1 approve · 2 decide`).

## Non-goals (Phase 1)

- FLEET chat view, log, Start FLEET, account switch, dependencies, done
  cards, working/ended lists (the session list already is the working list).
- A free-text "other" answer. A comment always goes with an option.
- Remote hosts. Phase 1 talks to this host's own service only. The HTTP
  client must take a base URL so Phase 2 can point it at a remote server.
- Full markdown. Summaries show as wrapped plain text (see "Rendering").

## Anti-goals

- The TUI types into FLEET's pane itself. It must not: the service owns
  `sayMu`, the 1 s gap, the 10 s double-tap map, and the idle / blocked /
  question / ClickUp event lines type into FLEET under that same lock. A
  second process typing would race them.
- The TUI invents a send text. The text is always the option's `send` value
  from `state.json`, plus the optional comment, built exactly as the page
  builds it.
- A card answered on one client still looks open on the other.

## Constraints

- One Go binary, stdlib only (see CLAUDE.md "Dependencies").
- Single stdin consumer; `enableOutputProcessing` on every return to raw
  mode (CLAUDE.md "Subtle invariants").
- Strict JSON decoding on the server, as every other fleet route.
- Never log the send text (the say log records `len=`, never text).

## Part 1 — Server answer store

### `POST /fleet/say` body

Today: `{"text": string, "cardId"?: string}`, strict (unknown field is 400).
New optional fields:

- `optionKey` (string, ≤100 bytes): the chosen option's `key`.
- `cardHash` (string): the hash of the card as the client showed it, copied
  from `/fleet/state`'s `cardHashes` (below). Clients never compute it — the
  page cannot, since `crypto.subtle` is absent on the plain-http tailnet
  origin.
- `correction` (bool): this send replaces an earlier answer to the card.

`optionKey`, `cardHash` and `correction` are only meaningful with `cardId`;
with no `cardId` they are a 400 `bad request`. A request with `cardId` but
no `cardHash` (an old page tab still open across the deploy) skips the
stale-card check below and is otherwise handled the same.

### What is recorded

When a send carries `cardId`, the handler, still under `sayMu`, records the
outcome in the answer store:

| Say outcome | Recorded status |
| --- | --- |
| `ok: true` (typed and Enter pressed, including `queued`) | `sent` |
| `code: not_submitted` (typed, Enter not pressed) | `unconfirmed` |
| any other refusal | nothing |

An entry is `{cardId, cardHash, attempts: [...]}`. An attempt is
`{optionKey, label, comment?: bool, at, status}`. Keep the last 3 attempts,
newest last. `label` is looked up from the card in the current `state.json`
by `optionKey`, never taken from the client; an unknown key records `label`
empty. `comment` is only a flag that a comment was added; the text is never
stored.

`cardHash` is computed from the card as the server reads it from
`state.json` at send time, over the same fields as the page's `cardSig`:
`kind`, `status`, `title`, `ticket`, `summary`, `lean`, and each option's
`key`, `label`, `line`, `send`. Not `updatedAt` (`fleet-state.py` restamps
it every write), not `id`, `session`, `links`. Any stable hash over a
canonical JSON of those fields is fine (sha256, first 16 hex chars).

### Double-answer guard

Before typing, if the card already has an entry whose latest attempt is
`sent` and whose `cardHash` equals the card's current hash, and the request
does not set `correction: true`, refuse with
`{ok: false, code: "already_answered"}` (HTTP 200, like other refusals).
Nothing is typed. An `unconfirmed` latest attempt also refuses without
`correction` — Andy must check the chat first, as the page says today.

A card whose `id` is not in the current `state.json`, or is not a decision
kind, refuses with `code: "card_gone"`. The page already blocks this
client-side (`sheetValid`); the server check closes the gap between two
clients.

**Stale-card check.** A send can wait on `sayMu` while an idle, blocked,
question or ClickUp line types, and FLEET can rewrite the card in that
time. So, under `sayMu` and immediately before typing, the handler re-reads
`state.json` (the cached (mtime, size) copy is fine) and compares the
card's current hash to the request's `cardHash`. A mismatch refuses with
`code: "card_changed"` and nothing is typed. Without this the old option's
`send` text would be typed and recorded as an answer to the new question.
Order inside the handler: existing text guards → `card_gone` →
`card_changed` → `already_answered` → the existing FLEET resolve and typing
pipeline → record.

### When an entry goes

Pruned on every `/fleet/state` read and on every store write:

- its card is no longer in `state.json`, or no longer `needs-you` /
  `explicit-yes`;
- its `cardHash` differs from the card's current hash — the card returns to
  Decide as a new question. Exception: an entry whose latest attempt is
  `unconfirmed` survives a hash change and is reported with
  `cardChanged: true`, as the page does today;
- its latest attempt is older than 24 h.

A bad or unreadable `state.json` prunes nothing (the last good copy rules,
as everywhere else).

### Dismiss

`POST /fleet/card/dismiss` `{"cardId": string}` removes the entry. Same auth
and same-origin rule for the cookie path as `/fleet/say`; strict body, 4 KiB.
Answers `{ok: true}` or `{ok: false, code: "not_found"}`. Used for an
`unconfirmed` entry after Andy checked the chat.

### `/fleet/state`

Two new server keys (server keys win over FLEET's own, as today):

- `cardHashes`: a map `cardId → hash` for every decision-kind card with an
  `id`. Clients echo the value as `cardHash` when they send, and compare it
  to decide whether a card changed under an open confirm panel.
- `answered`: a map `cardId → {cardHash, cardChanged?, attempts}` after
  pruning.

The answer store has its own mutex; prune on a `/fleet/state` read takes it,
not `sayMu`.

### Persistence

`$XDG_STATE_HOME/claude-sessions/fleet-answered.json`, 0600, temp file +
rename, written only when the store changes — the same pattern as
`fleet-ended.json`. `cmdServer` sets the path; tests keep the store in
memory. A missing or bad file is an empty store. Reason to persist: without
it a service restart puts every answered card back in Decide, which is the
double-answer risk this part exists to remove.

### Page change

`fleet.html` reads `answered` from `/fleet/state` and drops its own
`fleet.pending.v1` store for `sent` and `unconfirmed`. It keeps a
client-side entry only while its own request is in flight (`sending`), and
for a send whose response never arrived (network error, timeout): that
shows as "delivery not confirmed" until the next state poll; if the server
recorded it, the server entry replaces it. The page sends `optionKey`, sets
`correction: true` from "Send a correction", and calls the dismiss route
from Dismiss. The page's own `cardSig` is replaced by the server's
`cardHashes` value. `already_answered` gets "Already answered on another
device"; `card_gone` and `card_changed` get "The card changed". The "You
chose / Correction" lines come from `attempts`.

**Deploy migration.** The server store starts empty. So on its first load
after the deploy, the page reads its old `fleet.pending.v1` once and keeps
honouring its `sent` / `unconfirmed` entries locally (with the old
`cardSig` rule) until each one prunes or expires within 24 h; it never
writes new entries there. That stops the phone from offering a second
answer to a card it answered just before the deploy. The TUI did not exist
before, so it has nothing to migrate; for those ≤24 h it can still offer a
card the phone answered pre-deploy — accepted.

## Part 2 — TUI client

### Talking to the service

A small fleet client in a new file (for example `fleet_client.go`) with
`base URL + token`. Phase 1 finds the local service the way
`fetchServiceUsageNumbers` does: the token file, loopback first, then this
host's Tailscale address on a connection error or 404. Factor that lookup
out of `usage_numbers.go` rather than copy it. Bearer header, never cookie;
bearer skips the Origin check.

Calls: `GET /fleet/state`, `POST /fleet/say`, `POST /fleet/card/dismiss`.
Timeout 15 s for say (same as the page), 5 s for state.

### Badge

A background poller (follow `RemoteUsageHub`'s shape: goroutine, ticker,
`Snapshot()`, no wake pipe) fetches `/fleet/state` every 10 s while the TUI
runs. It counts open cards: decision-kind cards with an `id` and no
`answered` entry, plus cards whose entry is `unconfirmed`. The session-list
header shows `FLEET 1 approve · 2 decide` — `approve` red when > 0, `decide`
yellow. Nothing is shown when both are 0, when no service answers, or when
FLEET is not alive. The badge never blocks a render.

### Opening the view

`F` (and `f`) in the session list opens the Fleet view as a full-screen
modal loop, following `pickResumeSession`'s pattern (own event loop, re-render
every frame at the live width, resize wakes). `Esc` or `q` returns to the
session list. If no service answers, the view shows one line saying so and
how to start it (`claude-sessions service restart`).

### Layout (option A, chosen by Andy)

```
FLEET ● idle  state 2m ago                 1 approve · 2 decide · 1 sent
────────────────────────────────────────────────────────────────────
APPROVE
▸ DR-5241  Merge PR #2786 into develop                          4m
   CI is green. Two review threads were skipped (style only).
   Waiting on: Andy · session dr-5241

   1  yes      Merge now            ★ FLEET recommends
               merge with --merge, delete branch
   2  not-yet  Hold                 keep the PR open
DECIDE
  DR-5302  Retry policy for OCR timeouts                     12m
  DR-5288  Which fixture for the empty-page test?             1h
SENT — WAITING FOR FLEET
  DR-5190  You chose: B — keep cache  (3m ago)
────────────────────────────────────────────────────────────────────
↑↓ card · 1-9 pick option · Esc back
```

- One column. Sections in this order: APPROVE (`explicit-yes`), DECIDE
  (`needs-you`), SENT. Card order inside a section is `state.json` order.
- Collapsed card: ticket, title, age of `updatedAt`. The selected card
  expands in place: summary, `Waiting on: <waitingOn>`, `session`, options.
- Option row: `<n>`, the option's `key` dim, `label`, `line` under it,
  `★ FLEET recommends` on the `lean` option. If `lean` matches no option,
  show `Lean: <lean>`.
- Option keystrokes are always the digits `1`–`9` in option order, never
  the option's own `key`. The live `state.json` (2026-10-22) has keys like
  `A`, `A+gate`, `A-then-B`, `yes`, `not-yet`, `file`, `none`; every
  `explicit-yes` card has multi-character keys. Digits are one rule with no
  case question and no collision with view keys. A card with more than 9
  options shows the rest greyed with "answer on the page" (none today).
  The mockup's `[y]` / `[h]` rows become `1  yes  Merge now` style.
- A card with no `id` shows its options greyed with "answer in the chat".
- An `unconfirmed` card stays in its section with a yellow
  "delivery not confirmed — check FLEET's chat" line and locked options;
  `c` sends a correction (unlocks options for one answer), `x` dismisses.
- A `sent` card (in SENT) shows its attempts; selecting it and pressing `c`
  unlocks a correction.
- Header: FLEET status from `fleet.alive` / `fleet.status`; `state Nm ago`
  from `stateFileAt`; amber when `stale`; a `stateError` shows as one red
  line. When FLEET is not alive, options are greyed and a line says why
  (`fleet.code`).
- Scrolling: the selected card is always kept on screen; when the expanded
  card is taller than the screen, `PgUp`/`PgDn` scroll inside it.

### Confirm panel

Pressing an option key opens a panel below the header (replacing the list
until it closes):

```
Approve this write?  DR-5241 · 1 yes · Merge now         (needs-you: "Send this to FLEET?")
effect: merge with --merge, delete branch

FLEET receives:
  <send text> Andy adds: <comment>

Comment: <line editor>▌

Enter send (ready in 0.4 s) · Esc cancel · comment is optional
```

- Text sent: `send`, or `send + " Andy adds: " + comment` when the comment
  is non-blank, with whitespace in the comment collapsed to single spaces
  and trimmed — the page's exact rule. The preview updates as Andy types.
- The comment field is a raw-mode line editor in the same style as the
  inspector compose (`composeText`), not a cooked-mode `readLine`. It must
  refuse control bytes and must not grow the whole text past 4096 bytes.
- `Enter` is ignored for 400 ms after the panel opens (`SHEET_ARM_MS`).
- On `Enter` the TUI re-fetches `/fleet/state` and refuses locally if the
  card is gone or its `cardHashes` value changed (same as the page's
  `sheetValid`), then posts `{text, cardId, cardHash, optionKey,
  correction}` with the hash the panel was opened on. The server's
  `card_changed` check covers the gap after that. The status line shows
  "sending…" and the TUI stays responsive (send in a goroutine, wake on
  result, like the kill job).

### Results

| Response | TUI shows |
| --- | --- |
| `ok: true` | toast "sent" (or "FLEET is busy — it reads your answer next" when `queued`); state refetched; card moves to SENT; selection moves to the next open card |
| `code: not_submitted` | yellow "typed but not submitted — check FLEET's pane"; card shows unconfirmed |
| `already_answered` | "already answered on another device"; state refetched |
| `card_gone` / `card_changed` | "the card changed"; state refetched |
| other refusal | the server's `error` text as a toast; nothing changes |
| HTTP 400 (`reserved_tag`, bad text, …) | the body as a toast |
| timeout / network error | "delivery not confirmed — check FLEET's chat"; state refetched on the next poll decides |
| 401 | "service token rejected" |

### Rendering

Summaries are plain text, wrapped to the width, newlines kept. Phase 1
strips the simplest markdown noise: `**` and backticks are removed, `- ` and
`1. ` list lines keep their marker and get a hanging indent, fenced blocks
show as-is with no wrap. Tables and links show as their raw text. A real
renderer is Phase 3.

## Testing

- Server: unit tests beside `fleet_say_test.go` for record / refuse /
  correction / `card_changed` (card rewritten while the send waits) /
  prune / dismiss / persistence, using the existing
  `fleetDeps` seams; the hash ignores `updatedAt` and changes on each listed
  field.
- Page: extend `fleet_page_test.go` only for what it already checks (no
  HTML sinks). Manual check on the phone after deploy.
- TUI: pure functions tested directly — card filtering and sectioning,
  option-key mapping, send-text building, badge counts, wrap. The modal loop
  itself is checked by hand on agent-workstation against the live service.

## Left to the builder

- File split and type names.
- Exact hash function and its encoding.
- The poller's exact interval within 5–15 s.
- Colours beyond red approve / yellow decide / dim sent.
- Whether the badge poller shares code with the view's fetch.
