# FLEET in the TUI — Phase 1 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Andy answers FLEET's decision cards from the claude-sessions TUI. The answer store moves to the server, shared by the TUI and the `/fleet` page, and the server refuses a second answer to a card that has not changed.

**Architecture:** The service gets an answer store (`fleet_answered.go`). `/fleet/say` checks it under `sayMu` and records into it. A new `POST /fleet/card/dismiss` route removes an entry. `/fleet/state` gains two server keys, `cardHashes` and `answered`. The page swaps its localStorage answer store for the server's. The TUI gets a bearer-token client bound to a base URL, a 10 s badge poller in the session-list header, and an `F` full-screen modal. The modal sends only through the service's `POST /fleet/say` and never types into a pane.

**Tech Stack:** Go 1.26, one package `main`, stdlib plus `golang.org/x/term` and `golang.org/x/sys`. The page is the embedded vanilla-JS `fleet.html`.

**Spec:** `docs/superpowers/specs/2026-10-22-fleet-tui-phase1-design.md`

## Global Constraints

- One package `main`. Only stdlib, `x/term` and `x/sys`. Tests sit beside the code. Every task ends with `go test ./...` and `go vet ./...` green.
- Server bodies decode strictly: `DisallowUnknownFields`, and a trailing token is a 400. A refusal is HTTP 200 `{ok:false, code, error}`.
- The send text is never logged or stored. The say log records `len=`; the store keeps only a `comment` flag.
- The TUI never types into a pane. It sends through `POST /fleet/say` with `Authorization: Bearer <token>`, never a cookie.
- Send text: the option's `send`, or `send + " Andy adds: " + comment`. In the comment, every run of whitespace becomes one space and the ends are trimmed. The whole text is at most `sendKeysMaxLen` (4096) bytes.
- Option keystrokes are the digits `1`–`9` in option order, never the option's `key`.
- `Enter` is ignored for 400 ms after the confirm panel opens (`SHEET_ARM_MS`).
- HTTP timeouts: say 15 s, state 5 s, dismiss 5 s.
- Store: the last 3 attempts per card, newest last. An entry expires 24 h after its latest attempt. File `$XDG_STATE_HOME/claude-sessions/fleet-answered.json`, mode 0600, temp file + rename, written only when the store changes.
- Card hash: sha256 over canonical JSON of `kind, status, title, ticket, summary, lean` and each option's `key, label, line, send`, cut to the first 16 hex chars. It never covers `updatedAt`, `id`, `session`, `links` or `waitingOn`.
- Decision kinds are exactly `explicit-yes` (APPROVE) and `needs-you` (DECIDE). Use `fleetPushKind` (fleet_push.go).
- Single stdin consumer: the modal reads keys through its own `inputDecoder` and `pollEvents`. Background goroutines never read stdin. The modal stays in raw mode the whole time.
- Commits: subject `fleet: …`. Every message ends with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.

## Review Focus

1. **The Chat button on a page card.** `openCardChat` (fleet.html) sends "Andy opened chat on card X (tmux)" with `cardId` X. Once the server records sends that carry a `cardId`, that note would count as an answer and move the card to SENT, or be refused as `already_answered`. Expected: the note goes out with no `cardId`. Test: Task 3, page test.
2. **A pasted comment that holds a newline.** The decoder turns the newline into `KeyEnter` in the same read. Expected: it does not send. Test: Task 7, `TestFleetViewEnterInPasteBatchIgnored`.
3. **Card fields of the wrong JSON type** (numeric `id`, `options: "x"`, a card or option that is not an object). Expected: they read like the page's `str()`/`arr()`, and one bad field never drops the whole state. Tests: Task 1 `TestFleetCardViewLenient`, Task 4 `TestDecodeFleetStateViewLenient`.
4. **Esc/q while a send is in flight.** Expected: refused with a toast, so the outcome (for example `not_submitted`) is always shown. Test: Task 7, `TestFleetViewEscWhileSending`.
5. **Two cards with the same `id`.** Expected: the first one wins everywhere (`cardHashes`, the say lookup, the TUI), so the hash a client echoes is the hash the server checks. Test: Task 1, `TestFleetDecisionCardsFirstIDWins`.

## Task boundaries (where they depart from the obvious split)

- `fleetCardView` and its lenient reader live in Task 1. The server's hash and the TUI's decode share them.
- In `fleetSay`, the card checks run right before `fleetTypeInto`, after FLEET discovery and `resolveLivePID`. The spec's order line puts them before the resolve. But `resolveLivePID` runs a fresh `collectLocal` (up to about 1 s), and FLEET can rewrite the card during it; the spec also says "immediately before typing". The order card_gone → card_changed → already_answered is kept. The existing rate gate still runs first, so tests advance the clock 11 s between two sends of the same text.
- The view is split in two. Task 7 is the state machine, the results table and the send job, all table-tested. Task 8 is rendering, the modal loop and the `F` key.
- The page and TUI manual checks need the deployed server, so they are in Task 9, after the CLAUDE.md Workflow deploy.

---

### Task 1: Card hash and answer store

**Files:**
- Create: `fleet_card.go`, `fleet_answered.go`
- Test: `fleet_card_test.go`, `fleet_answered_test.go`

**Interfaces:**
- Consumes: `fleetPushStr(v any) string`, `fleetPushKind(kind string) bool` (fleet_push.go); `writeFileAtomic(path string, data []byte, perm os.FileMode) error` (account.go); `claudeSessionsStateDir() string` (idle_events.go).
- Produces:

```go
type fleetOptionView struct{ Key, Label, Line, Send string }
type fleetCardView struct {
	ID, Kind, Status, Ticket, Title, Summary, Lean, WaitingOn, Session, UpdatedAt string
	Options []fleetOptionView
}
func (c *fleetCardView) UnmarshalJSON(b []byte) error // error only when b is not a JSON object
func fleetCardHash(c fleetCardView) string
type fleetDecisionCard struct {
	Hash string
	Card fleetCardView
}
func fleetDecisionCards(state map[string]json.RawMessage) map[string]fleetDecisionCard // never nil
func fleetCardHashes(cards map[string]fleetDecisionCard) map[string]string          // never nil
func fleetOptionLabel(c fleetCardView, key string) (label, send string, ok bool)

const (
	fleetAnsweredKeep        = 24 * time.Hour
	fleetAnsweredMaxAttempts = 3
	fleetAttemptSent         = "sent"
	fleetAttemptUnconfirmed  = "unconfirmed"
)
type fleetAttempt struct {
	OptionKey string `json:"optionKey"`
	Label     string `json:"label"`
	Comment   bool   `json:"comment,omitempty"`
	At        string `json:"at"`     // RFC3339 UTC
	Status    string `json:"status"` // fleetAttemptSent | fleetAttemptUnconfirmed
}
type fleetAnswer struct {
	CardHash    string         `json:"cardHash"`
	CardChanged bool           `json:"cardChanged,omitempty"`
	Attempts    []fleetAttempt `json:"attempts"`
}
func (a fleetAnswer) latest() fleetAttempt // zero value when no attempts
type fleetAnsweredStore struct {
	path    string // "" → memory only; cmdServer sets fleetAnsweredPathDefault()
	mu      sync.Mutex
	loaded  bool
	entries map[string]fleetAnswer
}
func fleetAnsweredPathDefault() string
func (st *fleetAnsweredStore) get(cardID string) (fleetAnswer, bool)
func (st *fleetAnsweredStore) record(cardID, cardHash string, a fleetAttempt, cards map[string]fleetDecisionCard, now time.Time)
func (st *fleetAnsweredStore) view(cards map[string]fleetDecisionCard, now time.Time) map[string]fleetAnswer // nil cards → no prune; deep copy, never nil
func (st *fleetAnsweredStore) dismiss(cardID string) bool
func fleetAnswerRefused(e fleetAnswer, has bool, curHash string, correction bool) bool
```

- [ ] **Step 1: Write the failing tests**
  - `fleet_card_test.go`:
    - `TestFleetCardViewLenient`:
      - `{"id":5,"kind":"needs-you","title":["x"],"options":"no"}` → `ID=="5"`, `Title==""`, `Options==nil`.
      - `{"options":[1,{"key":"a","label":"A"}]}` → exactly `[{Key:"a",Label:"A"}]`.
      - `[1]` → `UnmarshalJSON` returns an error.
    - `TestFleetCardHashIgnoresVolatileFields`: changing only `updatedAt`, `id`, `session`, `links` or `waitingOn` keeps the hash. The hash matches `^[0-9a-f]{16}$`.
    - `TestFleetCardHashChangesOnEachField`: table of 11 mutations: `kind`, `status`, `title`, `ticket`, `summary`, `lean`, `options[0]` `key`/`label`/`line`/`send`, and one added option. Each hash differs from the base.
    - `TestFleetDecisionCardsFirstIDWins`: cards are needs-you `a` (title "first"), explicit-yes `a` (title "second"), done `b`, and an id-less needs-you. Result keys are exactly `{a}`, with `Card.Title=="first"`. `fleetCardHashes` gives `{"a": <hash>}`.
    - `TestFleetOptionLabel`: returns `label`; an empty label falls back to `send`; both empty falls back to `key`; an unknown key gives `ok==false`.
  - `fleet_answered_test.go`:
    - `TestFleetAnsweredRecordKeepsLastThree`: 4 records with hashes h1–h4 → attempts are keys 2, 3, 4 (newest last), `CardHash=="h4"`, `CardChanged==false`.
    - `TestFleetAnsweredPrune`, a table over `view(cards, now)`:
      - card missing → removed;
      - card no longer a decision → removed;
      - hash differs and latest is `sent` → removed;
      - hash differs and latest is `unconfirmed` → kept with `CardChanged==true`;
      - hash equal again → `CardChanged==false`;
      - latest `At` = now−24h−1s → removed; now−24h+1s → kept;
      - `cards==nil` → nothing removed or flagged.
    - `TestFleetAnswerRefused`, table:
      - no entry → false;
      - `sent` with the same hash → true;
      - `sent` with another hash → false;
      - `unconfirmed` with either hash → true;
      - `correction=true` → always false.
    - `TestFleetAnsweredDismiss`: `false` on an unknown id; `true` after a record; then `get` reports no entry.
    - `TestFleetAnsweredPersistence` (path `TempDir/st/fleet-answered.json`):
      - after a record the file mode is 0600, and a new store on the same path returns an equal `view(nil, now)`;
      - a file holding `not json` loads as an empty store;
      - a file with one entry each of bad status, no attempts, empty `cardHash`, `at:"soon"`, and 4 valid attempts loads only the valid entry, keeping its last 3 attempts.
    - `TestFleetAnsweredWritesOnlyOnChange`:
      - `view` on an empty store with the path set → no file is created;
      - after a record, `os.Chtimes` the file to an old time, then `view` with unchanged cards → mtime unchanged.

- [ ] **Step 2: Run the tests to verify they fail**
  Run: `go test -run 'TestFleetCard|TestFleetDecisionCards|TestFleetOptionLabel|TestFleetAnswer' .`
  Expected: FAIL, build error `undefined: fleetCardView`.

- [ ] **Step 3: Implement `fleet_card.go`**
  - `UnmarshalJSON`: decode into `map[string]any`. Read each string field with `fleetPushStr`. `options` keeps only object entries, like the page's `optionList`.
  - `fleetCardHash`: `json.Marshal([]any{kind, status, title, ticket, summary, lean, opts})`, where `opts` is a non-nil `[][4]string{key, label, line, send}`. Take sha256, hex-encode, keep the first 16 chars.
  - `fleetDecisionCards`: unmarshal `state["cards"]` into `[]json.RawMessage`. Skip entries that fail to decode, are not `fleetPushKind`, or have an empty ID. The first ID wins.
  - `fleetOptionLabel`: the page's `optLabel` rule (label, else send, else key).

- [ ] **Step 4: Implement `fleet_answered.go`**
  - File shape: `{"answered": {"<cardId>": fleetAnswer}}`.
  - `loadLocked` follows `fleetEndedTracker.loadLocked`. An entry is valid when the id is 1–200 bytes, the hash is non-empty, each attempt's status is in the set, `At` parses as RFC3339, and `OptionKey` is ≤100 bytes. Keep the last 3 valid attempts; drop an entry with none.
  - `saveLocked` follows `fleetEndedTracker.saveLocked`: `writeFileAtomic` with 0o600; on error, a stderr line `fleet: cannot save the answer store: …`.
  - A private `pruneLocked(cards, now) (changed bool)` applies the spec's rules.
  - `record` appends the attempt, sets `CardHash`, clears `CardChanged`, prunes, then saves.
  - `view` saves only when the prune changed something.
  - `fleetAnsweredPathDefault` is `claudeSessionsStateDir()/fleet-answered.json`, or `""` when there is no state dir.

- [ ] **Step 5: Run the tests to verify they pass**
  Run: the Step 2 command, then `go vet ./...`.
  Expected: PASS.

- [ ] **Step 6: Commit**
  `git add fleet_card.go fleet_card_test.go fleet_answered.go fleet_answered_test.go && git commit -m "fleet: card hash and server answer store" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"`

### Task 2: `/fleet/say` guards and records, dismiss route, `/fleet/state` keys

**Files:**
- Modify: `fleet_say.go` (`decodeFleetSayBody`, `fleetSay`; new `fleetCardDismiss`)
- Modify: `fleet.go` (`fleetDeps`, `registerFleetRoutes`, `fleetState`)
- Modify: `server.go:2585` (cmdServer, beside `s.fleet.ended.path`)
- Test: `fleet_say_test.go`, `fleet_test.go`

**Interfaces:**
- Consumes (Task 1): `fleetDecisionCards`, `fleetCardHashes`, `fleetOptionLabel`, `fleetAnsweredStore.{get,record,view,dismiss}`, `fleetAnswerRefused`, `fleetAttempt`, `fleetAttemptSent`, `fleetAttemptUnconfirmed`.
- Produces:

```go
// fleetDeps
answered fleetAnsweredStore // cmdServer sets answered.path

type fleetSayBody struct {
	Text, CardID, OptionKey, CardHash string
	Correction                        bool
}
func decodeFleetSayBody(w http.ResponseWriter, r *http.Request) (fleetSayBody, error) // err text is the 400 body: "bad json" | "bad request"
func (s *server) fleetCardDismiss(w http.ResponseWriter, r *http.Request)
```

Wire contract, used by Tasks 3 and 4:

| Route | Request | Response |
| --- | --- | --- |
| `POST /fleet/say` | `{text, cardId?, optionKey?, cardHash?, correction?}` | new refusal codes `card_gone`, `card_changed`, `already_answered` |
| `POST /fleet/card/dismiss` | `{cardId}` | `{ok:true}` or `{ok:false, code:"not_found"}` |
| `GET /fleet/state` | — | new keys `cardHashes: {id: hash}` and `answered: {id: fleetAnswer}`, both `{}` when empty |

- [ ] **Step 1: Write the failing tests**
  - Helpers in `fleet_say_test.go`:
    - `sayCards(t, rig, cardsJSON string)` writes `state.json` (`version`, `writtenAt` = `rig.now`, `cards`), then `os.Chtimes` +1 s per call so the (mtime, size) cache sees every rewrite.
    - `rigHash(t, rig, id) string` reads the hash via `fleetDecisionCards(rig.s.loadFleetState())`.
    - `rigAnswered(t, rig) map[string]fleetAnswer` reads `answered` via `GET /fleet/state`.
    - Fixture `dr1Card`: needs-you `dr-1`; option `yes` (label "Merge now", send "Andy decides: DR-1 — yes (explicit).") and option `not-yet`.
  - Update `TestFleetSaySendsExactTextAndLogs` to call `sayCards(dr1Card)` first. Without it the send is now `card_gone`.
  - `TestFleetSayBodyFieldsNeedCardID`:
    - each of `{"text":"a","optionKey":"y"}`, `{"text":"a","cardHash":"h"}`, `{"text":"a","correction":false}` and `{"text":"a","cardId":"","optionKey":"y"}` → 400;
    - with a cardId, a 101-byte `optionKey` → 400 and a 65-byte `cardHash` → 400.
  - `TestFleetSayRecordsSentAndRefusesSecond`:
    - post `{text, cardId:"dr-1", cardHash, optionKey:"yes"}` → `OK`;
    - `answered["dr-1"]` has `CardHash==rigHash` and one attempt `{OptionKey:"yes", Label:"Merge now", Comment:false, Status:"sent", At:rig.now}`;
    - `rig.now += 11s`, the same post → `already_answered` and `len(rig.typed)==1`;
    - with `correction:true` → `OK`, 2 attempts.
  - `TestFleetSayRecordsUnconfirmed`: a capture where the text never lands → `not_submitted`, latest status `unconfirmed`. Then a post without correction → `already_answered`; with correction → `OK`, latest status `sent`.
  - `TestFleetSayCommentFlagAndUnknownKey`: text = send + `" Andy adds: hi"` → `Comment==true`. `optionKey:"zzz"` → recorded with `Label==""`.
  - `TestFleetSayCardGone`: an unknown id, or a card of kind `done` → `card_gone`, nothing typed.
  - `TestFleetSayCardChangedWhileWaitingOnSayMu`:
    - `rig.s.fleet.sayMu.Lock()`;
    - post the old hash in a goroutine, then `time.Sleep(50ms)` so it parks on the lock;
    - `sayCards` with dr-1's summary changed, then `Unlock`;
    - result `card_changed`, `rig.typed` empty, no `dr-1` in `answered`.
  - `TestFleetSayCardChangedDuringResolve`: `rig.s.collect` rewrites state.json on its second call (the `collectPhases` shape) → `card_changed`, nothing typed.
  - `TestFleetSayNoCardHashSkipsStaleCheck`: an old-tab body `{text, cardId}` → `OK`, recorded with the current hash.
  - `TestFleetSayCorrectionWithoutEntry`: `correction:true` on an unanswered card → `OK`, recorded.
  - `TestFleetSayOtherRefusalRecordsNothing`: capture `boxCapture("x")` → `input_busy`, `answered` stays empty.
  - `TestFleetCardDismiss`:
    - no auth → 401;
    - cookie with no Origin → 403, code `origin`; bearer skips the Origin check;
    - an extra field → 400; a body over 4 KiB → 400; an empty `cardId` → 400;
    - an unknown id → `{ok:false, code:"not_found"}`;
    - after a recorded send → `{ok:true}` and `answered` is empty.
  - `TestFleetStateCardHashesAndAnswered` (fleet_test.go):
    - cards are needs-you `a`, explicit-yes `b`, done `c`, and an id-less needs-you → `cardHashes` keys are exactly `{a, b}`, and `answered` is `{}`, not null;
    - after `s.fleet.answered.record` on `a`, write state.json as invalid JSON → `answered` still has `a` and `stateError` is set;
    - then a valid state without `a` → `answered` is `{}`.

- [ ] **Step 2: Run the tests to verify they fail**
  Run: `go test -run 'TestFleetSay|TestFleetCardDismiss|TestFleetState' .`
  Expected: FAIL. The build fails on `fleetSayBody`, or the new tests get the wrong codes.

- [ ] **Step 3: Implement `decodeFleetSayBody` and the `fleetSay` changes in `fleet_say.go`**
  - `decodeFleetSayBody`: use pointer fields to tell when a field is present. Any of `optionKey`/`cardHash`/`correction` present while `cardId` is empty → `"bad request"`.
  - The handler sends 400 for an `optionKey` over 100 bytes (`bad optionKey`) or a `cardHash` over 64 bytes (`bad cardHash`). The existing 200-byte `cardId` limit stays.
  - When `CardID != ""`, right before `fleetTypeInto` (after the `fleet_changed` check):
    1. `state, stateErr := s.loadFleetState()` and `cards := fleetDecisionCards(state)`.
    2. Missing id → `card_gone`, error "The card is no longer open.".
    3. `CardHash != "" && CardHash != card.Hash` → `card_changed`, error "The card changed.".
    4. `fleetAnswerRefused(...)` → `already_answered`, error "Already answered.".
  - After `fleetTypeInto`, when `res.OK` (status `sent`) or `res.Code=="not_submitted"` (status `unconfirmed`), call `record(id, card.Hash, …)`. The attempt's `Label` comes from `fleetOptionLabel`. `Comment` is true when the key is known and the text differs from `fleetCleanSayText(send)`'s text. `At` is `s.fleetNow().UTC()`. Pass `cards` as nil when `stateErr != ""`.
  - Lock order: `sayMu` → `answered.mu`. `/fleet/state` and dismiss take `answered.mu` only.

- [ ] **Step 4: Implement `fleetCardDismiss` and the remaining wiring**
  - `fleetCardDismiss`:
    - auth: `fleetAuth`; a cookie request also needs `sameOrigin`, exactly like `fleetSay`;
    - body: `MaxBytesReader` 4<<10, `DisallowUnknownFields`, trailing-token check, `cardId` 1–200 bytes;
    - answer: `fleetSayResult{OK:true}` or `{Code:"not_found", Error:"no answer for that card"}`.
  - Register the route: `mux.HandleFunc("POST /fleet/card/dismiss", s.fleetCardDismiss)`.
  - In `fleetState`, after `caps`:
    - `out["cardHashes"] = fleetCardHashes(cards)`;
    - `out["answered"] = s.fleet.answered.view(cards, now)`, with `cards` passed as nil when `stateErr != ""`.
  - cmdServer: `s.fleet.answered.path = fleetAnsweredPathDefault()`.

- [ ] **Step 5: Run the tests to verify they pass**
  Run: `go test ./... && go vet ./...`
  Expected: PASS.

- [ ] **Step 6: Commit**
  `git add fleet_say.go fleet.go server.go fleet_say_test.go fleet_test.go && git commit -m "fleet: say refuses a second answer and records answers; dismiss route" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"`

### Task 3: The page reads the server store

**Files:**
- Modify: `fleet.html` — pending store (~1263–1309), `decisionCard` (~1897–1932), `answered` (~1991), `openCardChat` (~2036), `cardSig`/`inStrip`/`sentRow` (~2642–2670), `applyState`/`prunePending` (~2819–2863), sheet (~2869–3035), `say` (~3079)
- Test: `fleet_page_test.go`

**Interfaces:**
- Consumes: Task 2's wire contract.
- Produces: page functions `answerOf(id)`, `hashOf(c)`, `sayCard(text, id, optionKey, cardHash, correction)`, `loadLegacyPending()`.

- [ ] **Step 1: Write the failing tests**
  - In `TestFleetPageIsSelfContainedAndSafe`:
    - the `need` list gains `"/fleet/card/dismiss"`, `"state.answered"`, `"state.cardHashes"`, `"optionKey:"`, `"correction:"`, `"already_answered:"`, `"card_changed:"`, `"card_gone:"`, `"Already answered on another device"`, `"function loadLegacyPending("`, `"function answerOf("`, `"function hashOf("` and `` `" + tm + ")", null)` ``;
    - the `bad` list gains `"savePending("` and `` `" + tm + ")", id)` ``.
  - New `TestFleetPageReadsRealAnswerKeys`, in the style of `TestFleetPageReadsRealUsageKeys`:
    - marshal `fleetAnswer{CardHash:"h", CardChanged:true, Attempts:[{OptionKey:"k", Label:"l", Comment:true, At:"2026-10-22T10:00:00Z", Status:"sent"}]}`;
    - for each of `cardHash`, `cardChanged`, `attempts`, `optionKey`, `label`, `status`: the JSON has `"key"` and the page contains `"."+key`.

- [ ] **Step 2: Run the tests to verify they fail**
  Run: `go test -run TestFleetPage .`
  Expected: FAIL with the missing strings listed.

- [ ] **Step 3: Implement in `fleet.html`**
  1. **Local entries.** `pending` becomes memory-only: card id → `{att:{key, label, state}, inflight, settled}`. An entry exists only while its request is in flight (`sending`), or after a send whose response never came (`unconfirmed`). The first `applyState` after it settles drops it. Remove `savePending` and `loadPending`.
  2. **Legacy migration.** `loadLegacyPending()` runs once at load. It reads `PEND_KEY`, and keeps `sent`/`unconfirmed` entries younger than `PEND_MAX` in a `legacy` Map along with their `csig`. Legacy entries prune by the old rule:
     - card gone, or no longer a decision → dropped;
     - `cardSig` moved → dropped, unless the entry is `unconfirmed`, which stays and is flagged `cardChanged`.
     The key is rewritten with only the surviving subset and removed when none are left; nothing new is ever added. `cardSig` stays for this use only.
  3. **One read path.** `answerOf(id)` returns `{attempts:[{key, label, state}], cardChanged, src}` or null. Precedence: local entry → `obj(state.answered)[id]` (`status` maps to `state`; `label || optionKey`) → legacy entry. `decisionCard`, the "You chose:"/"Correction:" lines (last 3 attempts), `answered()`, `inStrip()`, `sentRow` and `nextOpenKey` read only `answerOf`.
  4. **Card identity.** `hashOf(c)` = `str(obj(state.cardHashes)[str(c.id)])`. `openSheet` stores `sheet.hash = hashOf(c)`. `sheetValid` = the card is present, is a decision kind, and `hashOf(cur) === sheet.hash`.
  5. **Corrections.** A `correcting` Set replaces `p.changing`. "Send a correction" adds the card id. The send posts `correction: true` whenever `answerOf(id)` is non-null.
  6. **Sending.** `sayCard` posts `{text, cardId, optionKey, cardHash, correction}` through `postSay` with codeText:
     - `{busy: GROK_BUSY}`;
     - `already_answered: "Already answered on another device."`;
     - `card_gone` and `card_changed`: `"The card changed. Check it again."`.

     `sheetSend` uses `sayCard`. On `ok`, `not_submitted`, `already_answered` or `card_*` it calls `afterSend()`, so the server entry shows. `say(text, cardId)` stays for chat lines.
  7. **Chat note.** `openCardChat` sends its note with `null` for `cardId`.
  8. **Dismiss.** For a server entry: POST `/fleet/card/dismiss {cardId}` through `postSay`, then `runNow("state")`. For a local or legacy entry: delete it in memory (and rewrite the legacy key).

- [ ] **Step 4: Run the tests to verify they pass**
  Run: `go test ./... && go vet ./...`
  Expected: PASS.

- [ ] **Step 5: Commit**
  `git add fleet.html fleet_page_test.go && git commit -m "fleet: page reads answers from the server store" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"`

### Task 4: Shared local-service lookup and the FLEET client

**Files:**
- Modify: `server_client.go` — new `localServiceConfig` and `tryLocalService`; `fetchListSessionsFromService` and `listSessionsServiceConfig` use them.
- Modify: `usage_numbers.go` — `fetchServiceUsageNumbers` and `usageNumbersServiceConfig` use them.
- Create: `fleet_client.go`
- Test: `server_client_test.go`, `fleet_client_test.go`

**Interfaces:**
- Consumes: `fleetCardView`, `fleetAnswer` (Task 1); `fleetSession` (fleet.go); `fleetSayResult` (fleet_say.go); `ServerConfig`, `hostPort`, `readServerToken`, `isTimeoutErr`, `localTailscaleIPv4`, `listSessionsTailscaleIPv4`.
- Produces:

```go
func localServiceConfig() (ServerConfig, error) // readServerToken + localServerHost:localServerPort
func tryLocalService(ctx context.Context, srv ServerConfig, tailscale func(context.Context) string,
	attempt func(context.Context, ServerConfig) (status int, err error)) (ServerConfig, int, error)

var (
	fleetStateTimeout = 5 * time.Second
	fleetSayTimeout   = 15 * time.Second
	fleetLocalConfig  = localServiceConfig
)
var errFleetUnauthorized = errors.New("service token rejected")
type fleetStateView struct {
	Fleet       fleetSession
	Stale       bool
	StateFileAt string // null → ""
	StateError  string // null → ""
	Cards       []fleetCardView
	CardHashes  map[string]string
	Answered    map[string]fleetAnswer
}
func decodeFleetStateView(data []byte) (fleetStateView, error) // error only when the body is not an object
type fleetClient struct{ base, token string } // base "http://host:port"
func newFleetClient(base, token string) *fleetClient
func (c *fleetClient) GetState() (fleetStateView, error)
type fleetSayRequest struct {
	Text       string `json:"text"`
	CardID     string `json:"cardId"`
	CardHash   string `json:"cardHash,omitempty"`
	OptionKey  string `json:"optionKey,omitempty"`
	Correction bool   `json:"correction,omitempty"`
}
type fleetSayOutcome struct {
	Status  int            // 0 when no response arrived
	Result  fleetSayResult // when the body was JSON
	Body    string         // trimmed body text, ≤300 bytes
	Unclear bool           // no response, timeout, or a 2xx without a boolean ok
}
func (c *fleetClient) Say(req fleetSayRequest) fleetSayOutcome
func (c *fleetClient) Dismiss(cardID string) (fleetSayResult, error)
func connectLocalFleet() (*fleetClient, fleetStateView, error)
type fleetLocal struct {
	mu      sync.Mutex
	c       *fleetClient
	connect func() (*fleetClient, fleetStateView, error) // nil → connectLocalFleet
}
func (l *fleetLocal) State() (fleetStateView, error)
func (l *fleetLocal) Client() (*fleetClient, error)
```

- [ ] **Step 1: Write the failing tests**
  - `TestTryLocalService` (server_client_test.go): a fake `attempt` records the hosts it was called with.

    | Loopback result | Tailscale | Hosts tried |
    | --- | --- | --- |
    | 200 | any | `[127.0.0.1]` |
    | status 0, non-timeout error | `100.64.0.1` | `[127.0.0.1, 100.64.0.1]` |
    | 404 | `100.64.0.1` | `[127.0.0.1, 100.64.0.1]` |
    | 401 or 500 | `100.64.0.1` | `[127.0.0.1]` |
    | `context.DeadlineExceeded` | `100.64.0.1` | `[127.0.0.1]` |
    | status 0 | `""` | `[127.0.0.1]` |
    | status 0 | `127.0.0.1` | `[127.0.0.1]` |

    The returned `ServerConfig.Host` is the host that answered.
  - The existing `TestCollectUsageNumbersFallsBack` and list-sessions tests stay unchanged and green.
  - `TestDecodeFleetStateViewLenient`: `{"stateFileAt":null,"stateError":null,"fleet":{"alive":true,"status":"idle"},"cards":[5,{"id":"a","kind":"needs-you","options":"x"}]}` → one card with `Options==nil`, `StateFileAt==""`, `Fleet.Alive==true`. A body of `[]` → error.
  - `TestFleetClientGetState`: the httptest handler asserts path `/fleet/state`, `Authorization: Bearer tok`, and no `Cookie` header. A 401 gives `errors.Is(err, errFleetUnauthorized)`; a 500 gives an error.
  - `TestFleetClientSayOutcomes`: table of handlers.

    | Response | Outcome |
    | --- | --- |
    | 200 `{"ok":true,"queued":true}` | `Status 200`, `Result.OK`, `Result.Queued` |
    | 200 `{"ok":false,"code":"already_answered"}` | `Result.Code` = `already_answered` |
    | 400 `bad text\n` | `Status 400`, `Body "bad text"` |
    | 400 `{"ok":false,"code":"reserved_tag","error":"…"}` | `Result.Code` = `reserved_tag` |
    | 200 `nope` | `Unclear` |
    | 200 `{"x":1}` | `Unclear` |
    | handler blocks past `fleetSayTimeout` (set to 50 ms) | `Unclear`, `Status 0` |
    | 401 | `Status 401` |

    The handler decodes the request body strictly; it has exactly the keys `text`, `cardId`, `cardHash`, `optionKey`, `correction`.
  - `TestFleetClientDismiss`: path `/fleet/card/dismiss`, body exactly `{"cardId":"x"}`. `{ok:true}` → `OK`; `not_found` → `Result.Code`.
  - `TestFleetLocalReconnects`:
    - a fake `connect` counts its calls and binds to httptest server A;
    - `State()` returns connect's state;
    - close A → `State()` errors and `l.c` becomes nil;
    - the next `State()` calls `connect` again (count 2).
  - `TestConnectLocalFleetFallsBackToTailscale`:
    - `fleetLocalConfig` points at `127.0.0.1:<port>` of a server that 404s for Host `127.0.0.1` and serves state for `localhost`;
    - `localTailscaleIPv4` returns `"localhost"`;
    - the client's `base` is `"http://localhost:<port>"`.

- [ ] **Step 2: Run the tests to verify they fail**
  Run: `go test -run 'TestTryLocalService|TestDecodeFleetStateView|TestFleetClient|TestFleetLocal|TestConnectLocalFleet' .`
  Expected: FAIL, build error `undefined: tryLocalService`.

- [ ] **Step 3: Implement `tryLocalService` and switch both callers to it**
  - `tryLocalService` takes over the fallback rule that `fetchServiceUsageNumbers` and `fetchListSessionsFromService` each carried: retry on Tailscale only when the loopback status is 0 or 404, the error is not a timeout, and the Tailscale address is non-empty and differs from `srv.Host`. Each caller passes its own Tailscale seam.
  - `usageNumbersServiceConfig` and `listSessionsServiceConfig` default to `localServiceConfig`.

- [ ] **Step 4: Implement `fleet_client.go`**
  - Use `http.DefaultClient` with a timeout context per call.
  - Read bodies through `io.LimitReader`: 2 MiB for state, 64 KiB for say and dismiss.
  - `connectLocalFleet` calls `tryLocalService` with `localTailscaleIPv4`. Its attempt is a GET `/fleet/state` that returns the HTTP status.
  - `fleetLocal.State()` connects when `c == nil`. It forgets `c` only after an error that carried no HTTP status. `Client()` connects when needed.

- [ ] **Step 5: Run the tests to verify they pass**
  Run: `go test ./... && go vet ./...`
  Expected: PASS.

- [ ] **Step 6: Commit**
  `git add server_client.go usage_numbers.go fleet_client.go server_client_test.go fleet_client_test.go && git commit -m "fleet: TUI client for the local service; share the loopback-then-Tailscale lookup" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"`

### Task 5: Pure view model

**Files:**
- Create: `fleet_view.go`
- Test: `fleet_view_test.go`

**Interfaces:**
- Consumes: `fleetStateView`, `fleetCardView`, `fleetAnswer`, the attempt status constants, `fleetPushKind`, `sendKeysMaxLen`, `wrapRunes(s string, width int) []string` (resume.go).
- Produces:

```go
type fleetSection int
const (
	fleetSecApprove fleetSection = iota // explicit-yes
	fleetSecDecide                      // needs-you
	fleetSecSent                        // latest attempt sent
)
type fleetItem struct {
	Card    fleetCardView
	Section fleetSection
	Answer  *fleetAnswer // nil → no entry
}
func fleetItems(v fleetStateView) []fleetItem
type fleetCardCounts struct{ Approve, Decide, Sent int }
func countFleetCards(v fleetStateView) fleetCardCounts
func fleetOptionIndex(key string, n int) (int, bool) // "1".."9" → 0..8 while < n
func fleetSendText(send, comment string) string
func fleetEditComment(comment, key, send string) (string, bool)
func fleetSummaryLines(s string, width int) []string
```

Rules:
- **`fleetItems`**: decision-kind cards in `state.json` order. `Answer` is `v.Answered[ID]` when the ID is non-empty. The section is SENT when the latest attempt is `sent`, otherwise the card's kind. The list is stable-sorted APPROVE, DECIDE, SENT.
- **`countFleetCards`**: Approve and Decide count items that have an ID and either no entry or a latest attempt that is `unconfirmed`. Sent counts SENT items.
- **`fleetEditComment`**: a printable ASCII byte (0x20–0x7e) is appended only while `len(comment)+1 ≤ sendKeysMaxLen` and `len(fleetSendText(send, comment+key)) ≤ sendKeysMaxLen`. `"\x7f"`/`"\x08"` drop the last byte. Any other key leaves the comment unchanged and returns `false`. This is the byte rule `handleInspectorCompose` uses.
- **`fleetSummaryLines`**, per source line:
  - a line whose trimmed text starts with ```` ``` ```` toggles a fence; fence lines and everything inside are kept verbatim and not wrapped;
  - outside a fence, remove `**` and backticks;
  - a list line (`^\s*([-*] |\d+\. )`) wraps at `width` minus the marker width, with continuation lines indented by the marker width;
  - other lines go through `wrapRunes`; an empty line stays `""`.

- [ ] **Step 1: Write the failing tests**
  - `TestFleetItemsSections`: cards explicit-yes `a`, needs-you `b` (unconfirmed), needs-you `c` (sent), done `d`, id-less needs-you `e`, explicit-yes `f`. Expected order and sections: `a` APPROVE, `f` APPROVE, `b` DECIDE, `e` DECIDE, `c` SENT.
  - `TestCountFleetCards`: the same fixture → `{Approve:2, Decide:1, Sent:1}`.
  - `TestFleetOptionIndex`:
    - `("1",3)` → `0,true`; `("3",3)` → `2,true`; `("9",12)` → `8,true`;
    - `("4",3)`, `("0",3)`, `("a",3)`, `("10",12)` and `(KeyUp,3)` → `false`.
  - `TestFleetSendText`: `("Merge now","")` → `"Merge now"`; `("x","  a \t b  ")` → `"x Andy adds: a b"`; `("x","   ")` → `"x"`.
  - `TestFleetEditComment`:
    - `("ab","c")` → `"abc"`; `("ab","\x7f")` → `"a"`; `("","\x7f")` → `""`;
    - `"\x1b"`, `KeyUp` and `"\xc3"` → unchanged, `false`;
    - with `send := strings.Repeat("x", 4083)`: `("","a")` is accepted (4096 bytes) and `("a","b")` is refused.
  - `TestFleetSummaryLines` at width 20:
    - `"**Bold** and `code`"` → `["Bold and code"]`;
    - `"one\n\ntwo"` → `["one","","two"]`;
    - `"- alpha beta gamma delta epsilon"` → first line starts `"- "`, every later line starts with 2 spaces, and every line is ≤20 runes;
    - `"12. alpha beta gamma delta"` → continuation lines indented 4;
    - a fence holding a 30-char line and `**x**` → both are kept verbatim on one line each.

- [ ] **Step 2: Run the tests to verify they fail**
  Run: `go test -run 'TestFleetItems|TestCountFleetCards|TestFleetOptionIndex|TestFleetSendText|TestFleetEditComment|TestFleetSummaryLines' .`
  Expected: FAIL, `undefined: fleetItems`.

- [ ] **Step 3: Implement the produced functions in `fleet_view.go` per the rules above**

- [ ] **Step 4: Run the tests to verify they pass**
  Run: `go test ./... && go vet ./...`
  Expected: PASS.

- [ ] **Step 5: Commit**
  `git add fleet_view.go fleet_view_test.go && git commit -m "fleet: TUI view model (sections, counts, digits, send text, summary wrap)" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"`

### Task 6: Badge poller and header badge

**Files:**
- Create: `fleet_badge.go`
- Modify: `render.go` — `groupView`, `renderHeader` and its 3 call sites (lines 2188, 2308, 2446)
- Modify: `tui.go` — create the poller, set the badge in render, add it to pause/resume
- Test: `fleet_badge_test.go`, `render_test.go`

**Interfaces:**
- Consumes: `fleetLocal.State` (Task 4); `countFleetCards`, `fleetCardCounts` (Task 5); `colorize`, `dim`, `ansiBold`.
- Produces:

```go
const fleetBadgeInterval = 10 * time.Second
type FleetBadgeHub struct {
	mu       sync.Mutex
	counts   fleetCardCounts
	ok       bool
	paused   atomic.Bool
	kick     chan struct{}
	stop     chan struct{}
	fetch    func() (fleetStateView, error)
	interval time.Duration
}
func NewFleetBadgeHub(fetch func() (fleetStateView, error), interval time.Duration) *FleetBadgeHub
func (h *FleetBadgeHub) Snapshot() (fleetCardCounts, bool)
func (h *FleetBadgeHub) Pause()
func (h *FleetBadgeHub) Resume()   // un-pauses and kicks a fetch
func (h *FleetBadgeHub) Refresh()  // non-blocking kick
func (h *FleetBadgeHub) Shutdown()
func fleetBadgeText(c fleetCardCounts, ok bool) string
// render.go
// groupView gains: fleetBadge string
func renderHeader(w io.Writer, sections []section, mode string, accounts []accountUsageLine, codexAccounts []codexAccountLine, grokAccounts []grokAccountLine, cols int, filter groupFilter, groupSort bool, query, fleetBadge string)
```

- [ ] **Step 1: Write the failing tests**
  - `TestFleetBadgeText`:
    - `(anything, false)` → `""`;
    - `({0,0,3}, true)` → `""`;
    - `({1,2,0}, true)` → `stripANSI` gives `"FLEET 1 approve · 2 decide"`, and the text contains `colorize("1;31","1 approve")` and `colorize("1;33","2 decide")`;
    - `({0,2,0}, true)` → contains `dim("0 approve")`.
  - `TestFleetBadgeHubSnapshot`, with interval `time.Hour` and `Refresh()`, polling `Snapshot` for up to 1 s:
    - an alive view with one open explicit-yes card → `({1,0,0}, true)`;
    - a fetch error → `ok==false`;
    - `Fleet.Alive==false` → `ok==false`;
    - paused → `Refresh` does not call fetch.
  - `TestRenderHeaderShowsFleetBadge` (render_test.go):
    - `BuildTableFrame` with `gv.fleetBadge = "FLEET 1 approve · 0 decide"` → `stripANSI(frame.lines[0])` contains it before `"[full]"`;
    - with an empty badge, `lines[0]` does not contain `"FLEET"`.

- [ ] **Step 2: Run the tests to verify they fail**
  Run: `go test -run 'TestFleetBadge|TestRenderHeaderShowsFleetBadge' .`
  Expected: FAIL, `undefined: fleetBadgeText`.

- [ ] **Step 3: Implement**
  - The hub has `RemoteUsageHub`'s shape: one goroutine, a ticker at `interval`, a kick channel, a paused flag, no wake pipe, and a first fetch kicked at start. `ok` = `err == nil && view.Fleet.Alive`.
  - `renderHeader` appends `"  " + fleetBadge + ansiBold` after `filterStr` when the badge is non-empty.
  - `tui.go`:
    - after `remoteUsageHub`: `fleetSrc := &fleetLocal{}` and `fleetBadgeHub := NewFleetBadgeHub(fleetSrc.State, fleetBadgeInterval)`, with a deferred `Shutdown()`;
    - the render closure's `groupView` literal sets `fleetBadge: fleetBadgeText(fleetBadgeHub.Snapshot())`;
    - `makeCtx`'s `pause`/`resume` call `fleetBadgeHub.Pause`/`Resume`.

- [ ] **Step 4: Run the tests to verify they pass**
  Run: `go test ./... && go vet ./...`
  Expected: PASS.

- [ ] **Step 5: Commit**
  `git add fleet_badge.go fleet_badge_test.go render.go render_test.go tui.go && git commit -m "fleet: FLEET badge in the session-list header" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"`

### Task 7: Fleet view state, keys, results and send job

**Files:**
- Create: `fleet_tui_state.go`
- Test: `fleet_tui_state_test.go`

**Interfaces:**
- Consumes: Task 4 (`fleetClient`, `fleetSayRequest`, `fleetSayOutcome`, `errFleetUnauthorized`, `fleetStateView`); Task 5 (`fleetItem`, `fleetItems`, `fleetOptionIndex`, `fleetSendText`, `fleetEditComment`); key constants `KeyUp`, `KeyDown`, `KeyPageUp`, `KeyPageDown`, `KeyEnter`, `KeyEsc`.
- Produces:

```go
const fleetArmDelay = 400 * time.Millisecond
const fleetPageStep = 10 // lines per PgUp/PgDn
type fleetCmd int
const (
	fleetCmdNone fleetCmd = iota
	fleetCmdRender
	fleetCmdClose
	fleetCmdSend
	fleetCmdDismiss
)
type fleetToastKind int
const (
	fleetToastInfo fleetToastKind = iota // 3 s
	fleetToastWarn                       // yellow, 6 s
	fleetToastErr                        // red, 6 s
)
type fleetPanel struct {
	item       fleetItem
	opt        int
	hash       string // cardHashes value when the panel opened
	comment    string
	openedAt   time.Time
	correction bool
}
type fleetViewState struct {
	view       fleetStateView
	loaded     bool
	err        string // non-empty: the one line shown instead of the cards
	items      []fleetItem
	sel        int
	scroll     int
	unlocked   string // card id a 'c' unlocked for one answer
	panel      *fleetPanel
	sending    bool
	send       *fleetSayRequest // set with fleetCmdSend; the loop takes it
	dismissID  string           // set with fleetCmdDismiss
	nextAfter  string           // card id just sent; the next applyState selects the next open card
	toast      string
	toastKind  fleetToastKind
	toastUntil time.Time
}
type fleetSendResult struct {
	CardID   string
	CheckErr error // the pre-send state fetch failed; nothing was sent
	Stale    bool  // card gone or its hash moved; nothing was sent
	Out      fleetSayOutcome
}
func fleetCardStillAsked(v fleetStateView, cardID, hash string) bool // present, decision kind, CardHashes[id]==hash
func fleetSendToast(o fleetSayOutcome) (msg string, kind fleetToastKind, refetch bool)
func fleetSendJob(c *fleetClient, req fleetSayRequest) fleetSendResult // GetState → check → Say
func (st *fleetViewState) setToast(msg string, kind fleetToastKind, now time.Time)
func (st *fleetViewState) applyState(v fleetStateView, err error, now time.Time)
func (st *fleetViewState) handleKey(k string, inBatch bool, now time.Time) fleetCmd
func (st *fleetViewState) applySend(r fleetSendResult, now time.Time) (refetch bool)
```

**Key rules, list mode** (all keys act on the selected item):
- `KeyUp`/`KeyDown`: move `sel` within bounds and reset `scroll`.
- `KeyPageUp`/`KeyPageDown`: change `scroll` by `fleetPageStep`.
- A digit opens a panel only when all of these hold: FLEET is alive, the card has an ID, nothing is sending, the option's `Send` is non-empty, and either `Answer == nil` or `unlocked == ID`. The panel gets `{hash: view.CardHashes[ID], openedAt: now, correction: Answer != nil}`. A digit on a locked card only toasts "answered — press c to send a correction".
- `c`: the card has an `Answer` and nothing is sending → `unlocked = ID` and toast "pick the correction (1-9)".
- `x`: the latest attempt is `unconfirmed` → set `dismissID` and return `fleetCmdDismiss`.
- `KeyEsc`, `q`, `Q`, `\x03`:
  - while sending → warn toast "the answer is still sending — wait for the result";
  - else, when `unlocked` is set → clear it;
  - else → `fleetCmdClose`.

**Key rules, panel mode:**
- `KeyEsc`/`\x03`: close the panel.
- `KeyEnter`: ignored when `inBatch` or `now < openedAt+fleetArmDelay`. Otherwise:
  - set `send = &fleetSayRequest{Text: fleetSendText(opt.Send, comment), CardID, CardHash: panel.hash, OptionKey: opt.Key, Correction}`;
  - close the panel, set `sending = true`, toast "sending…";
  - return `fleetCmdSend`.
- Any other key goes to `fleetEditComment`.

**`applyState`:**
- `errFleetUnauthorized` → `err = "service token rejected"`.
- Any other error → `err = "no FLEET service answered on this host — start it with: claude-sessions service restart"`.
- On success:
  - set `err = ""`, `view = v`, `items = fleetItems(v)`;
  - keep the selection by card ID (an id-less card keeps its index, within bounds);
  - when `nextAfter` is set, select the first open item (has an ID, no `Answer`) after that card's old index, else the first open item, then clear `nextAfter`;
  - clear `unlocked` when its card has no entry;
  - an open panel whose card fails `fleetCardStillAsked` closes, with error toast "the card changed".

**`applySend`:**
- Always sets `sending = false`.
- `CheckErr` → error toast "could not re-check the card: <err>"; no refetch.
- `Stale` → "the card changed"; refetch.
- Otherwise use `fleetSendToast`. On `Out.Result.OK`, also set `nextAfter = CardID` and clear `unlocked`.

**`fleetSendToast`** (exact copy):

| Outcome | msg | kind | refetch |
| --- | --- | --- | --- |
| `Unclear` | `delivery not confirmed — check FLEET's chat` | warn | false |
| Status 401 | `service token rejected` | err | false |
| 200 `ok` | `sent` | info | true |
| 200 `ok`, `queued` | `FLEET is busy — it reads your answer next` | info | true |
| 200 `not_submitted` | `typed but not submitted — check FLEET's pane` | warn | true |
| 200 `already_answered` | `already answered on another device` | err | true |
| 200 `card_gone` / `card_changed` | `the card changed` | err | true |
| 200, any other code | `Result.Error` | err | false |
| 400 | `Result.Error` if non-empty, else `Body` | err | false |
| any other status | `Body`, else `HTTP <n>` | err | false |

- [ ] **Step 1: Write the failing tests**
  - `TestFleetViewDigitOpensPanel`, subtests:
    - `"2"` → `panel.opt==1`, `panel.hash==CardHashes[id]`, `correction==false`;
    - an id-less card → no panel;
    - FLEET not alive → no panel;
    - an option with empty `Send` → no panel;
    - an answered card → no panel, and the toast contains `"c"`;
    - after `"c"` → a panel with `correction==true`.
  - `TestFleetViewEnterArm`: Enter at `openedAt+399ms` → `fleetCmdNone` and the panel stays open. At `+400ms` → `fleetCmdSend`, `*st.send == {Text:"Merge now Andy adds: hi", CardID, CardHash, OptionKey:"yes", Correction:false}`, the panel is closed, `sending==true`.
  - `TestFleetViewEnterInPasteBatchIgnored`: after the arm delay, `handleKey(KeyEnter, true, now)` → no send and the panel stays open.
  - `TestFleetViewEscWhileSending`:
    - with `sending` set, `KeyEsc` and `"q"` do not return `fleetCmdClose`, and the toast is set;
    - with `unlocked` set, the first Esc clears it;
    - otherwise Esc → `fleetCmdClose`.
  - `TestFleetViewDismissKey`: `x` on an unconfirmed card → `fleetCmdDismiss` with `dismissID`. On a sent card → `fleetCmdNone`.
  - `TestFleetViewApplyStateClosesStalePanel`: a new state with a different hash → panel nil, toast `"the card changed"`.
  - `TestFleetViewSelectsNextOpenAfterSend`: open items A, B, C. `applySend` with OK on A, then `applyState` with A answered `sent` → the selected item is B.
  - `TestFleetViewServiceErrors`: `errFleetUnauthorized` → `err=="service token rejected"`. Any other error → `err` contains `"claude-sessions service restart"`.
  - `TestFleetSendToast`: one row per line of the table above, with the exact strings.
  - `TestFleetSendJob` (httptest, `/fleet/state` returns `cardHashes {"a":"h2"}`):
    - request hash `"h1"` → `Stale`, no say hits;
    - hash `"h2"` → one say hit and `Out.Result.OK`;
    - state returns 500 → `CheckErr != nil`, no say hits.

- [ ] **Step 2: Run the tests to verify they fail**
  Run: `go test -run 'TestFleetView|TestFleetSendToast|TestFleetSendJob' .`
  Expected: FAIL, `undefined: fleetViewState`.

- [ ] **Step 3: Implement the produced types and methods in `fleet_tui_state.go` per the rules above**

- [ ] **Step 4: Run the tests to verify they pass**
  Run: `go test ./... && go vet ./...`
  Expected: PASS.

- [ ] **Step 5: Commit**
  `git add fleet_tui_state.go fleet_tui_state_test.go && git commit -m "fleet: TUI view state, keys and send results" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"`

### Task 8: Fleet view rendering, modal loop and the `F` key

**Files:**
- Create: `fleet_tui.go`
- Modify: `tui_events.go` (add `wakeFleet` after `wakeKill`)
- Modify: `tui.go` (key switch, `renderHelp`)
- Test: `fleet_tui_test.go`

**Interfaces:**
- Consumes: Task 7 (all); `fleetLocal` (Task 4); `countFleetCards`, `fleetSummaryLines`, `fleetSendText` (Task 5); `fleetBadgeInterval` (Task 6); `pollEvents`, `newInputDecoder`, `newScreenRenderer`, `term.GetSize`, `formatAge`, `colorize`, `dim`, `bold`, `clipLine`.
- Produces:

```go
wakeFleet wakeKind // after wakeKill
type fleetJob[T any] struct {
	mu     sync.Mutex
	done   bool
	result T
	wakeR  int
	wakeW  int
	closed bool
}
func startFleetJob[T any](run func() T) *fleetJob[T] // killJob's wake-pipe pattern
func (j *fleetJob[T]) snapshot() (T, bool)
func (j *fleetJob[T]) wake() wakeFD // fd -1 when closed or no pipe
func (j *fleetJob[T]) close()       // idempotent
func fleetWindow(total, selStart, selEnd, rows, scroll int) int
func renderFleetView(st *fleetViewState, cols, rows int, now time.Time) string
func runFleetView(src *fleetLocal, wakes []wakeFD)
```

**Render rules** (spec layout A):
- **Header line:** `FLEET ● <fleet.status>`, or `FLEET ○ not running (<fleet.code>)`. Then `state <formatAge> ago` from `StateFileAt`, in amber (`"33"`) when `Stale`. On the right, `<a> approve · <d> decide · <s> sent`. A `StateError` is one red line under the header. Then a separator of `─` the full width.
- **When `st.err` is set:** only the header and the `err` line.
- **Sections:** `APPROVE`, `DECIDE`, `SENT — WAITING FOR FLEET`, each shown only when non-empty.
  - Collapsed row: `▸ ` on the selected card, else two spaces; then `ticket  title`, with the age of `UpdatedAt` right-aligned.
  - SENT row: `ticket  You chose: <label>  (<age> ago)`.
  - No decision cards at all: dim `No cards wait for you.`.
- **The selected card expands in place:**
  - summary via `fleetSummaryLines(…, cols-3)`, indented 3;
  - `Waiting on: <waitingOn> · session <session>`, with only the parts that are present;
  - the last 3 attempts as `You chose: <label>` / `Correction: <label>`;
  - options as `<n>  <key, dim>  <label>`, with `★ FLEET recommends` when `Key == Lean`, and `line` dim on the row below;
  - `Lean: <lean>` when the lean matches no option.
- **Greyed options:**
  - options 10 and up: dim, with `answer on the page`;
  - an id-less card: all dim, with `answer in the chat`;
  - FLEET not alive: all dim, with `FLEET is not running (<code>) — answers are off`;
  - a locked card (has an entry and is not unlocked): all dim.
- **An unconfirmed latest attempt** shows a yellow `delivery not confirmed — check FLEET's chat` and a dim `c send a correction · x dismiss`. With `CardChanged`, it also shows `Card changed · delivery of your last answer is still unknown — check the chat`.
- **The panel** replaces the list:
  - title `Approve this write?` (explicit-yes) or `Send this to FLEET?` (needs-you), then `  <ticket> · <n> <key> · <label>`;
  - `effect: <line>` when the option has a line;
  - `FLEET receives:`, then the `fleetSendText` value wrapped and indented 2;
  - `Comment: <comment>▌`;
  - `Enter send (ready in 0.N s) · Esc cancel · comment is optional`, where the `(ready in …)` part is dropped once armed.
- **Bottom row:** the active toast, coloured by kind. Otherwise `↑↓ card · 1-9 pick option · c correct · x dismiss · PgUp/PgDn scroll · Esc back`.
- **`fleetWindow`:**
  - `rows <= 0` → 0;
  - a card taller than `rows` → `selStart + clamp(scroll, 0, (selEnd-selStart)-rows)`;
  - otherwise the smallest start that shows `[selStart, selEnd)`: 0 when `selEnd <= rows`, else `selEnd-rows`.

- [ ] **Step 1: Write the failing tests** (assert on `stripANSI` output)
  - `TestFleetWindow`:

    | total | selStart–selEnd | rows | scroll | result |
    | --- | --- | --- | --- | --- |
    | 50 | 10–14 | 20 | 0 | 0 |
    | 50 | 40–45 | 20 | 0 | 25 |
    | 50 | 10–40 | 20 | 0 | 10 |
    | 50 | 10–40 | 20 | 5 | 15 |
    | 50 | 10–40 | 20 | 99 | 20 |

  - `TestRenderFleetViewList`: an alive, idle FLEET with `stateFileAt` = now−2m. Cards: explicit-yes `DR-5241` (lean `yes`; options `yes` "Merge now" and `not-yet` "Hold"), needs-you `DR-5302`, and needs-you `DR-5190` answered `sent` with label "keep cache". Output contains `FLEET ● idle`, `state 2m ago`, `1 approve · 1 decide · 1 sent`, `APPROVE`, `DECIDE`, `SENT — WAITING FOR FLEET`, `▸ DR-5241`, `1  yes`, `★ FLEET recommends`, `2  not-yet`, `You chose: keep cache`.
  - `TestRenderFleetViewGreyedAndNotes`:
    - lean `zzz` → `Lean: zzz`;
    - an id-less card → `answer in the chat`;
    - 10 options → `answer on the page`;
    - not alive with code `not_live` → `not_live`;
    - unconfirmed → `delivery not confirmed — check FLEET's chat`;
    - `stateError "bad"` → a line `bad`.
  - `TestRenderFleetViewNoService`: with `st.err` set, the output contains `claude-sessions service restart` and not `APPROVE`.
  - `TestRenderFleetViewPanel`:
    - explicit-yes → `Approve this write?`, `FLEET receives:`, `Merge now Andy adds: hi`;
    - at `openedAt+100ms` → `ready in 0.3 s`; at `+500ms` → no `ready in`;
    - needs-you → `Send this to FLEET?`.
  - `TestFleetJobWakes`:
    - a job that returns 7 → its wake fd is readable within 1 s (`unix.Select`), and `snapshot()==(7,true)`;
    - `close()` twice does not panic;
    - after close, `wake().fd==-1`.

- [ ] **Step 2: Run the tests to verify they fail**
  Run: `go test -run 'TestFleetWindow|TestRenderFleetView|TestFleetJobWakes' .`
  Expected: FAIL, `undefined: fleetWindow`.

- [ ] **Step 3: Implement `fleetJob`, `fleetWindow` and `renderFleetView` per the rules above**

- [ ] **Step 4: Implement `runFleetView`**
  The loop is not fixed by the signatures, so here it is in full:
  1. Create its own `inputDecoder` and `screenRenderer`, and `st := &fleetViewState{}`. Start a state job (`src.State`) at once. `nextPoll = now + fleetBadgeInterval`.
  2. Each pass:
     - draw `renderFleetView` at `term.GetSize`;
     - `timeout` = the earliest of `nextPoll`, the panel's arm deadline while it is not armed, and `toastUntil`, at least 1 ms;
     - call `pollEvents(dec, timeout, wakes + every live job's wake())`.
  3. Finished jobs:
     - state → `applyState`;
     - send → `applySend`, and start a state job when it returns `true`;
     - dismiss → toast `dismissed`, `already gone` (`not_found`) or the error, then start a state job.

     Close each finished job.
  4. When `nextPoll` has passed and no state job is running, start one and reset `nextPoll`.
  5. For each `eventKey`, `cmd := st.handleKey(k, len(keys) > 1, time.Now())`:
     - `fleetCmdClose` → close every job and return;
     - `fleetCmdSend` → start a send job: `src.Client()`, then `fleetSendJob`; a `Client()` error becomes `fleetSendResult{CheckErr}`;
     - `fleetCmdDismiss` → start a dismiss job (`Client().Dismiss`).

     Mouse events are ignored.

- [ ] **Step 5: Wire `tui.go`**
  - Key switch: `case "f", "F": screen.Invalidate(); runFleetView(fleetSrc, modalWakes); fleetBadgeHub.Refresh(); screen.Invalidate(); render()`.
  - `renderHelp`, under ACTIONS: `    F            FLEET decision cards (1-9 pick · Enter send · Esc back)`.

- [ ] **Step 6: Run the tests to verify they pass**
  Run: `go test ./... && go vet ./... && go build ./...`
  Expected: PASS.

- [ ] **Step 7: Commit**
  `git add fleet_tui.go fleet_tui_test.go tui_events.go tui.go && git commit -m "fleet: F opens the FLEET decision view in the TUI" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"`

### Task 9: Docs, then the live check after deploy

**Files:**
- Modify: `CLAUDE.md`
  - add `POST /fleet/card/dismiss` to the route list in "FLEET web page" (~857);
  - replace the "**Sent strip** (page only)" paragraph (~1084) with an "**Answer store**" paragraph;
  - add a new `### FLEET view in the TUI` section before `### Idle events for FLEET`.
- Modify: `README.md` (Live-view keys table)
- Modify: `CHANGELOG.md` (`[Unreleased]` → `### Added`)

**Interfaces:** none (documentation).

- [ ] **Step 1: Write the "Answer store" paragraph**
  It covers:
  - the file path and its shape;
  - what is recorded (`sent` / `unconfirmed`), and that the label comes from state;
  - the guard order and where it sits: right before `fleetTypeInto`, with the reason;
  - the prune rules, and that a bad `state.json` prunes nothing;
  - the lock order `sayMu` → `answered.mu`;
  - the `cardHashes` and `answered` keys and the dismiss route;
  - the page's `fleet.pending.v1` migration;
  - why the Chat-button note carries no `cardId`.

- [ ] **Step 2: Write the "FLEET view in the TUI" section**
  It covers:
  - the files and what each holds;
  - `tryLocalService`, now shared with `usage` and `list-sessions`;
  - `fleetLocal`, shared by the badge and the view;
  - the modal's own decoder, with `pollEvents` timers and `fleetJob` + `wakeFleet`;
  - the digits-only rule, the 400 ms arm, the rule that an Enter inside a multi-key read never sends, and Esc being refused while a send is in flight;
  - the base-URL client, kept for Phase 2.

- [ ] **Step 3: Update README and CHANGELOG**
  - README row: `| F | FLEET decision cards (1-9 pick · Enter send · Esc back) |`.
  - CHANGELOG: two Added entries, one for the server answer store and dismiss route, one for the TUI FLEET view and badge.

- [ ] **Step 4: Verify the build is still green**
  Run: `go test ./... && go vet ./...`
  Expected: PASS.

- [ ] **Step 5: Commit**
  `git add CLAUDE.md README.md CHANGELOG.md && git commit -m "fleet: document the answer store and the TUI FLEET view" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"`

- [ ] **Step 6: Live check after the CLAUDE.md Workflow deploy to agent-workstation**
  Andy does the sends, on cards he chooses.
  1. **Phone page:**
     - a card answered on the page moves to SENT;
     - Chat on a card does not move it to SENT;
     - Dismiss on an unconfirmed answer clears it.
  2. **TUI:**
     - the badge counts match the page's Approve/Decide counts;
     - `F` opens the view; ↑↓ expands cards;
     - a digit opens the panel; Enter within 0.4 s does nothing;
     - pasting `a<newline>b` into the comment does not send;
     - Esc cancels.
  3. **Cross-device:**
     - a send from the TUI with a comment shows `sent`, and the card is in SENT on both clients within 10 s;
     - answering the same card from the phone shows "Already answered on another device".
  4. **Service restart while the view is open:**
     - the badge disappears, and the view shows the restart line;
     - both come back after the restart;
     - a terminal resize redraws.

