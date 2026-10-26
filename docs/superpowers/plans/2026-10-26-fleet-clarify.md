# FLEET Clarify Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A per-card side chat ("Clarify") in the TUI F view and on the
/fleet page. A read-only headless fork of FLEET's Claude session answers it,
and a "draft for FLEET" can be handed to the existing Other path.

**Architecture:**
- The server owns everything:
  - a store of chats per card (`fleet_clarify.go`),
  - a fork runner that builds the exact `claude -p` argv and runs it with a
    timeout and process-group kill (`fleet_clarify_run.go`),
  - three authed routes plus a `clarify` summary key on `/fleet/state`.
- Both clients poll the chat and never run a CLI.
- The TUI adds a clarify mode to `fleetViewState`, drawn by
  `renderFleetView` and driven by the existing `fleetLoop` jobs.
- The page adds a second bottom sheet.

**Tech Stack:** Go stdlib only (single package `main`), embedded
`fleet.html` (vanilla JS, no HTML sinks).

**Spec:** `docs/superpowers/specs/2026-10-26-fleet-clarify-design.md`. Its
"Amendment: live checks" section wins over the text below it.

## Global Constraints

- No new dependencies. Stdlib only.
- Claude FLEET only:
  - A grok FLEET gets HTTP 409 `{"code":"unsupported_tool"}` on the first
    ask.
  - The TUI hides `k`, and the page hides the Clarify button, when the
    state's `fleet.tool == "grok"`.
- The fork never goes through a shell.
  - Run it with `exec.Command` and an argv slice, stdin from `/dev/null`,
    `Setpgid: true`, and the whole group killed on timeout or cancel.
  - The question is the last argv element, after a literal `--`.
- The exact argv, deny list and allow list are in Task 1. Do not add, drop or
  reorder rules without updating the spec.
- Limits:
  - one fork in flight per card (`busy`);
  - at most 2 forks in flight in all (`busy`);
  - 5 min per fork (`clarifyTimeout = 5 * time.Minute`);
  - a question is at most 4000 runes (`too_long`), and an empty or
    whitespace-only one is `empty`;
  - a stored answer is cut to 32 KiB.
- Auth on every route is the existing pattern:
  - `s.fleetAuth(r)`: none gives 401.
  - A cookie request that fails `sameOrigin(r)` gives 403
    `{"code":"origin"}`.
  - Errors are `writeJSON(w, code, map[string]string{"code": …})`.
  - Paths, stderr and process errors go to the server's stderr only, never
    into a response or a stored turn.
- Store file: `claudeSessionsStateDir()/fleet-clarify.json`, mode 0600,
  written with `writeFileAtomic` and only on a change. `cmdServer` sets the
  path; tests keep path "" (memory only).
- No test runs a real `claude`. The runner is the seam
  `fleetDeps.clarifyRun`; nil means the real runner.
- Every string from the server or the fork that the TUI draws goes through
  `fleetDisplayText`. The page renders fork text only through `renderRich`.
  `fleet_page_test.go`'s forbidden-sinks list still passes.
- Commit messages end with
  `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
- Gate before each commit: `go build ./... && go vet ./... && go test ./...`.

## Review Focus

1. **A card leaves state.json (or is answered `sent`) while its fork runs.**
   The fork is cancelled. No turn is written into the pruned entry. A later
   Clarify on a new card with the same id starts clean. Pinned by
   `TestClarifyPruneCancelsRunningFork` (Task 3).
2. **The server restarts with a question pending.** On load the entry has no
   `pending` and ends with an `error` turn, "interrupted by a restart".
   Pinned by `TestClarifyStoreLoadTurnsPendingIntoError` (Task 2).
3. **The fork fails in some way:** non-zero exit, not JSON, `is_error:
   true`, or a timeout. The chat gets one `error` turn with a short fixed
   text, never stderr or a path. Pinned by `TestClarifyRunFailureModes`
   (Task 1) and `TestClarifyAskErrorTurn` (Task 3).
4. **A hostile answer:** fork text holding ESC sequences or HTML. The TUI
   strips them (`fleetDisplayText`), and the page uses `renderRich` only.
   Pinned by `TestClarifyViewSanitizes` (Task 5) and the page-test needles
   (Task 7).
5. **FLEET is down or restarted after a chat exists.** Later asks still
   resume the stored fork id and cwd, and do not need a live FLEET. Pinned
   by `TestClarifyLaterAskNeedsNoLiveFleet` (Task 3).
6. **A question starting with `-`** is still the question, because it sits
   after `--`. Pinned in `TestClarifyArgv` (Task 1).

---

### Task 1: Fork runner and argv

**Files:**
- Create: `fleet_clarify_run.go`
- Test: `fleet_clarify_run_test.go`

**Interfaces:**
- Produces:
  - `type clarifyRunParams struct { ResumeID string; Fork bool; Model string; CuPath string; Preamble string; Question string }`
  - `func clarifyArgv(p clarifyRunParams) []string`, which returns the
    arguments after the binary name.
  - `type clarifyRunResult struct { SessionID string; Text string; IsError bool; Denials int }`
  - `func runClarifyFork(ctx context.Context, bin, cwd string, argv []string) (clarifyRunResult, error)`
  - `func clarifyPreamble(c fleetCardView) string`
  - `var errClarifyTimeout`, `errClarifyBadOutput`, `errClarifyExit`
    (sentinels; the caller maps each to a fixed turn text)
  - `const clarifyTimeout = 5 * time.Minute`
  - `func clarifyClaudeBin() string`: `exec.LookPath("claude")`, "" when
    missing
  - `func clarifyCuPath() string`: `resolveClickupCu(<parsed
    clickup-events.json, or defaultClickupConfig() on any error>)`,
    falling back to `"cu"` when that returns ""

- [ ] **Step 1: Write the failing tests**

`TestClarifyArgv`:
- With `{ResumeID:"abc", Fork:true, Model:"claude-opus-5-5", CuPath:"/x/cu-with-token", Preamble:"P", Question:"-rf what?"}`, assert the exact slice:
  ```
  -p --output-format json --resume abc --fork-session --model claude-opus-5-5
  --setting-sources "" --strict-mcp-config --disable-slash-commands
  --permission-mode dontAsk --tools Bash,Read,Grep,Glob
  --settings {"permissions":{"deny":["Bash(*--output*)","Bash(rg *--pre*)","Edit","Write","NotebookEdit","WebFetch","WebSearch","Agent","Task","Workflow","ScheduleWakeup","Skill","ListAgents","ReportFindings"]}}
  --allowedTools Read Grep Glob Bash(git log:*) Bash(git show:*) Bash(git diff:*) Bash(git status:*) Bash(git blame:*)
    Bash(gh pr view:*) Bash(gh pr diff:*) Bash(gh pr checks:*) Bash(gh pr list:*) Bash(gh run view:*)
    Bash(/x/cu-with-token fetch:*) Bash(claude-sessions list-sessions:*) Bash(claude-sessions usage:*)
    Bash(rg:*) Bash(grep:*) Bash(ls:*) Bash(cat:*) Bash(head:*) Bash(tail:*) Bash(wc:*) Bash(jq:*)
  --append-system-prompt P -- -rf what?
  ```
  - The `--setting-sources` value is the empty string `""` as its own
    element, not the two-character string `""`.
  - The `--settings` value is that exact JSON, compact.
- With `Fork:false` there is no `--fork-session`.
- With `Model:""`, the value after `--model` is `"opus"`.
- The last two elements are always `"--"` and the question.

`TestClarifyPreamble`: for a card with ID `DR-5302`, Title `T`, Summary `S`,
Lean `B` and options `A x`/`B y`, the preamble contains each of:
- `read-only side chat`
- `forked from FLEET`
- `Never resume, re-run or finish any step FLEET left unfinished`
- `say so plainly in your answer`
- `Nothing you write reaches FLEET`
- `DR-5302`, `T`, `S`, `A: x`, `B: y` and `FLEET leans B`

`TestClarifyRunParsesJSON`: `bin` is a test script written to `t.TempDir()`
that prints `{"session_id":"f1","result":"hi","is_error":false,"permission_denials":[{},{}]}`.
- The result is `{SessionID:"f1", Text:"hi", IsError:false, Denials:2}`.
- The script also records `pwd` and its stdin (`cat`). Assert the cwd
  matches and stdin is empty.

`TestClarifyRunFailureModes`:
- exit 3 with no stdout: `errors.Is(err, errClarifyExit)`
- stdout `not json`: `errClarifyBadOutput`
- `{"is_error":true,"result":"x"}`: no error, `IsError == true`
- a script that runs `sleep 30`, under a ctx with a 200 ms deadline:
  `errClarifyTimeout` within 2 s. The script also starts a child
  (`sleep 30 &` then `wait`), so check the child is gone too: the script
  writes the child's pid to a file, then assert
  `syscall.Kill(pid, 0) != nil`.

- [ ] **Step 2: Run them; expect FAIL** (undefined names):
  `go test -run 'TestClarify(Argv|Preamble|Run)' ./...`
- [ ] **Step 3: Implement the interfaces above.**
  - Copy `runClarifyFork`'s shape from `runClickupCu`
    (clickup_events.go:337): `exec.CommandContext`, `Setpgid`, a
    `cmd.Cancel` that kills `-pid`, `WaitDelay` 5 s, stdout capped with
    `cappedBuffer` at 4 MiB.
  - Add `cmd.Dir = cwd` and `cmd.Stdin = nil` (which gives `/dev/null`).
  - Cap stderr at 8 KiB and write it to the server's stderr as
    `clarify: fork stderr: …`.
  - The JSON struct has fields `session_id`, `result`, `is_error`, and
    `permission_denials` as `[]json.RawMessage`.
  - `ctx.Err() == context.DeadlineExceeded` gives `errClarifyTimeout`. A
    plain cancel returns `ctx.Err()`.
- [ ] **Step 4: Run them; expect PASS.**
- [ ] **Step 5: Commit** `fleet clarify: fork runner and argv`.

### Task 2: Clarify store

**Files:**
- Create: `fleet_clarify.go`
- Test: `fleet_clarify_test.go`

**Interfaces:**
- Consumes: `fleetDecisionCard` and `fleetDecisionCards`
  (fleet_card.go:113), `fleetAnswer` (fleet_answered.go:31),
  `claudeSessionsStateDir`, `writeFileAtomic`.
- Produces:
  - `type clarifyTurn struct { Role string `json:"role"`; Text string `json:"text"`; At string `json:"at"` }`.
    Role is one of `"you"`, `"fleet"`, `"error"`, `"note"`. `At` is
    RFC3339 UTC.
  - `type clarifyPending struct { Kind string `json:"kind"`; StartedAt string `json:"startedAt"` }`.
    Kind is `"ask"` or `"draft"`.
  - `type clarifyDraft struct { Text string `json:"text"`; At string `json:"at"` }`
  - `type clarifyEntry struct { Tool, FleetSessionID, ForkID, Cwd, Model, CardHash string; Turns []clarifyTurn; Pending *clarifyPending; Draft *clarifyDraft; Changed bool }`.
    JSON names are camelCase. `Changed` means "card-changed note already
    added".
  - `type fleetClarifyStore struct { path string; mu sync.Mutex; loaded bool; entries map[string]*clarifyEntry; cancels map[string]context.CancelFunc; running int }`.
    `cancels` and `running` are never saved.
  - `func fleetClarifyPathDefault() string` returns
    `<state dir>/fleet-clarify.json`, or "" when there is no state dir.
  - `func (st *fleetClarifyStore) get(cardID string) (clarifyEntry, bool)`
    returns a deep copy.
  - `func (st *fleetClarifyStore) begin(cardID, kind string, init *clarifyEntry, question string, cancel context.CancelFunc, now time.Time) (clarifyEntry, string)`.
    It returns a copy of the entry and a refusal code: `""`, `"busy"`, or
    `"empty"` (a draft on an entry with no turns).
    - `init` is used only when there is no entry yet.
    - When kind is `"ask"`, it appends a `you` turn with the question.
    - It sets `Pending` and records `cancel`.
    - It refuses `"busy"` when the entry is pending or `running >= 2`.
  - `func (st *fleetClarifyStore) finish(cardID string, forkID string, turn *clarifyTurn, draft *clarifyDraft, now time.Time)`
    - It clears `Pending` and the cancel, and decrements `running`.
    - It sets `ForkID` when the entry has none and `forkID != ""`.
    - It appends `turn` and/or sets `Draft`.
    - It does nothing when the entry is gone.
  - `func (st *fleetClarifyStore) discardDraft(cardID string) bool`
  - `func (st *fleetClarifyStore) prune(cards map[string]fleetDecisionCard, answered map[string]fleetAnswer, now time.Time)`
    - `cards == nil` (state.json unreadable) means no-op.
    - It drops an entry whose id is not in `cards`, or whose `answered`
      entry's last attempt has `Status == "sent"`. It calls that entry's
      cancel when one is recorded.
    - When an entry's card hash differs from `cards[id].Hash` and
      `!Changed`, it appends the note turn `card changed since this chat
      started` and sets `Changed = true`.
  - `func (st *fleetClarifyStore) summary() map[string]clarifySummary`,
    with `type clarifySummary struct { Turns int `json:"turns"`; Pending bool `json:"pending"`; Draft bool `json:"draft"` }`.
  - Persistence is `loadLocked`, run lazily once, and `saveLocked`, which
    is called by every mutator that changed something.
    - Load turns any saved `Pending` into an `error` turn, "interrupted by
      a restart", and clears it.

- [ ] **Step 1: Write the failing tests**
  - `TestClarifyStoreBeginFinish`:
    - Begin an ask with an init entry. Assert the `you` turn and
      `Pending.Kind == "ask"`.
    - A second begin answers `busy`.
    - `finish` with forkID `f1` and a `fleet` turn sets `ForkID == "f1"`,
      2 turns, `Pending == nil`.
    - A later `finish` with forkID `f2` keeps `f1`.
  - `TestClarifyStoreGlobalCap`: two different cards begin, and the third
    gets `busy`. After one finishes, the third succeeds.
  - `TestClarifyStoreDraftNeedsTurns`:
    - A draft on a new card answers `empty`.
    - After a finished ask, a draft begin succeeds.
    - `finish` with a draft sets `Draft.Text`.
    - `discardDraft` clears it, and returns false the second time.
  - `TestClarifyStorePrune`:
    - A card missing from `cards` is dropped, and its cancel is called (a
      counter).
    - A card whose answer's last attempt is `sent` is dropped; one whose
      last attempt is `unconfirmed` stays.
    - A hash change adds exactly one `note` turn, even across two prunes.
    - `cards == nil` drops nothing.
  - `TestClarifyStoreLoadTurnsPendingIntoError`:
    - Write a file holding an entry with `pending` set to `t.TempDir()`.
    - A store on that path's `get` shows no pending and a last turn
      `{Role:"error", Text:"interrupted by a restart"}`.
    - After any mutation the file's mode is 0600.
  - `TestClarifyStoreFinishAfterPruneIsNoop`: begin, prune it away, then
    finish. `get` reports not found, and nothing panics.
- [ ] **Step 2: Run them; expect FAIL.**
- [ ] **Step 3: Implement.** Model it on `fleetAnsweredStore`.
  - Use a fresh `map` on first use.
  - `running` counts begun-but-unfinished entries, and a prune of a pending
    entry decrements it.
- [ ] **Step 4: Run them; expect PASS.**
- [ ] **Step 5: Commit** `fleet clarify: per-card chat store`.

### Task 3: Server routes and the ask worker

**Files:**
- Modify:
  - `fleet.go`: add `fleetDeps` fields; register routes in
    `registerFleetRoutes` at :287; add a `clarify` key next to :453-459 and
    call `prune` there.
  - `server.go`: in `cmdServer`, set `s.fleet.clarify.path =
    fleetClarifyPathDefault()` next to `answered.path` at ~:2588.
- Create: `fleet_clarify_http.go`
- Test: `fleet_clarify_http_test.go`

**Interfaces:**
- Consumes: Tasks 1 and 2, `findFleet` (fleet.go:222), `s.collectLocal`
  (fresh), `findTranscript(home, sid)`, `readResumableHeadFn`,
  `fleetDecisionCards`, `fleetCardHash`, `s.fleet.answered`.
- Produces:
  - `fleetDeps.clarify fleetClarifyStore`
  - `fleetDeps.clarifyRun func(ctx context.Context, cwd string, argv []string) (clarifyRunResult, error)`.
    nil means `runClarifyFork(ctx, clarifyClaudeBin(), cwd, argv)`; with no
    binary that is an `errClarifyExit`.
  - `fleetDeps.clarifyRetryDelay time.Duration`: 0 means 5 s; tests set
    1 ms.
  - `fleetDeps.clarifyGo func(func())`: nil means `go f()`; tests run it
    inline or collect it.
  - Routes, wire shapes for Tasks 5-7:
    - `GET /fleet/clarify?card=<id>` answers 200
      `{"card":id,"tool":"","turns":[…],"pending":{…}|null,"draft":{…}|null}`.
      An unknown id gets `turns: []`.
    - `POST /fleet/clarify/ask` with body `{"cardId","text"}` answers 202
      `{"pending":{…}}`.
    - `POST /fleet/clarify/draft` with body `{"cardId"}` answers 202
      `{"pending":{…}}`. With body `{"cardId","discard":true}` it answers
      200 `{"ok":true}`, or 404 `{"code":"not_found"}` when there is no
      draft.
    - `/fleet/state` gains `"clarify": {id: {turns, pending, draft}}`.
  - Refusal codes and statuses:

    | Code | Status |
    | --- | --- |
    | `bad_request` | 400 (strict decode, 32 KiB body cap, missing `cardId`) |
    | `empty` | 400 |
    | `too_long` | 400 |
    | `card_gone` | 409 (not an open decision in the current state.json) |
    | `not_live` | 409 |
    | `ambiguous` | 409 |
    | `unsupported_tool` | 409 |
    | `busy` | 409 |

    `not_live`, `ambiguous` and `unsupported_tool` apply to a first ask
    only.

The ask flow (`func (s *server) clarifyStart(cardID, kind, question string) (clarifyEntry, string, int)`):
1. Load state with `s.loadFleetState()`, get `fleetDecisionCards`, and look
   up the card. If it is missing, answer `card_gone`.
2. If there is no entry yet, collect fresh with `s.collectLocal()` and run
   `findFleet`:
   - "not running" gives `not_live`; "ambiguous" gives `ambiguous`.
   - `Tool == toolGrok` gives `unsupported_tool`.
   - Init `{Tool:"", FleetSessionID: sess.SessionID, Model: sess.Model, CardHash: card.Hash}`.
   - Cwd is the head cwd of `findTranscript(s.fleet.home or the user home,
     sid)`, read with `readResumableHeadFn`, else `sess.CWD`.
3. Call `begin` with a `context.WithTimeout(context.Background(),
   clarifyTimeout)` and its cancel.
4. Through `clarifyGo`, run the fork:
   - Params: resume the fork id when one exists, else the FLEET session id
     with `Fork:true`; cwd is the entry's.
   - The preamble comes from the card as read at step 1.
   - The question is the user's text, or for a draft the fixed
     `clarifyDraftPrompt(cardID)`: `Write the decision I reached in this
     chat about card <id> as one short paragraph addressed to FLEET. No
     greeting, no preamble: output only the message.`
   - On a first ask that failed with `errClarifyExit` or
     `errClarifyBadOutput`, wait `clarifyRetryDelay` and retry once.
   - Map the outcome:

     | Outcome | Result |
     | --- | --- |
     | OK, not `IsError` | `fleet` turn with `Text` cut to 32 KiB, or for a draft a `Draft` |
     | `IsError` | `error` turn `the fork answered with an error` |
     | timeout | `error` turn `no answer within 5 minutes` |
     | cancelled | `finish` is a no-op (pruned) |
     | failed after retry, first ask | `error` turn `FLEET is mid-step; ask again in a moment` |
     | other failure | `error` turn `the fork failed; see the server log` |

     Every case calls `finish` with `res.SessionID` as the fork id.
5. Call `/fleet/state`'s prune with `cards` (nil on a state error) and
   `s.fleet.answered`'s view.

- [ ] **Step 1: Write the failing tests.** Use the existing fleet test
  harness (`newFleetTestServer` or whatever `fleet_say_test.go` uses: copy
  its setup, a fake `collect`, and state.json in a temp dir). Set
  `clarifyRun` to a fake that records `(cwd, argv)` and returns scripted
  results, and set `clarifyGo` to run inline.
  - `TestClarifyAuth`:
    - No auth on all three routes gives 401.
    - A cookie POST with a foreign `Origin` gives 403 `origin`.
  - `TestClarifyAskFirstForksFleet`:
    - Run with a live FLEET row (claude, session `S`, model `M`) and a
      transcript whose head cwd is `/w`.
    - Ask on an open card; the response is 202.
    - The fake saw cwd `/w`, and an argv holding `--resume S
      --fork-session --model M`, ending `-- <question>`.
    - `GET` then shows turns `you`, `fleet` and pending null.
  - `TestClarifyLaterAskNeedsNoLiveFleet`:
    - After the first ask (fork id `F`), make `collect` return no FLEET.
    - A second ask is 202. The argv has `--resume F` and no
      `--fork-session`, and the cwd is still `/w`.
  - `TestClarifyAskRefusals`, each of these is refused:
    - an unknown card gives `card_gone`;
    - with no chat, a grok FLEET gives `unsupported_tool`, no FLEET gives
      `not_live`, and two FLEETs give `ambiguous`;
    - a 4001-rune text gives `too_long`, and `"  "` gives `empty`;
    - an unknown body field gives `bad_request`;
    - a second ask while the first is pending gives `busy`. For this one
      `clarifyGo` collects the func and does not run it.
  - `TestClarifyAskErrorTurn`, for each of: fake `IsError`,
    `errClarifyTimeout`, and `errClarifyExit` twice on a first ask. Assert
    that turn text, and that the retry happened exactly once (2 calls).
  - `TestClarifyDraftAndDiscard`:
    - A draft before any turn gives `empty`.
    - After an ask, a draft gives 202. The fake saw the
      `clarifyDraftPrompt` text as its last argv element. `GET` shows
      `draft.text`, and the number of turns is unchanged.
    - Discard gives 200; a second discard gives 404 `not_found`.
  - `TestClarifyStateSummaryAndPrune`:
    - `/fleet/state` has `clarify[id] = {turns:2, pending:false,
      draft:false}`.
    - Remove the card from state.json and fetch the state again: `clarify`
      is empty, and `GET` shows `turns: []`.
  - `TestClarifyPruneCancelsRunningFork`:
    - `clarifyGo` collects the func. Ask, then remove the card and hit
      `/fleet/state`.
    - Now run the collected func with a fake that blocks on `ctx.Done()`
      and returns `ctx.Err()`. It returns promptly, and `GET` shows
      `turns: []`.
- [ ] **Step 2: Run them; expect FAIL.**
- [ ] **Step 3: Implement** in `fleet_clarify_http.go`.
  - Handlers: `fleetClarifyGet`, `fleetClarifyAsk`, `fleetClarifyDraft`.
  - Decode strictly: `decodeStrictJSON` caps at 4 KiB, so add
    `decodeStrictJSONLimit(w, r, dst, limit int64) error` in
    fleet_push.go. Make `decodeStrictJSON` call it with 4 KiB, and use
    32 KiB here.
- [ ] **Step 4: Run them; expect PASS**, plus the full `go test ./...`.
- [ ] **Step 5: Commit** `fleet clarify: routes, ask worker, state summary`.

### Task 4: Pin the fork's invisibility

**Files:**
- Test: `fleet_clarify_invisible_test.go` (tests only; change code only if
  a test fails)

- [ ] **Step 1: Write the tests.** Use the existing hermetic home helpers
  the session and resume tests use (stub `sessionPIDAlive` and
  `grokPIDAlive` together, per CLAUDE.md).
  - `TestClarifyForkSessionFileIsNotARow`:
    - Set up a live FLEET session file (`entrypoint: cli`, name `FLEET`,
      nameSource `user`), plus a fork-shaped one: a different pid, same
      cwd, `entrypoint: sdk-cli`, `kind: interactive`, name `FLEET`,
      nameSource `user`.
    - `CollectLocal` lists only the first.
    - `findFleet` on that list returns the first, with no "ambiguous".
    - `CollectLocalLite` also omits the fork.
  - `TestClarifyForkTranscriptNotResumable`: a transcript under
    `~/.claude/projects/<dir>/<forkid>.jsonl` whose lines all carry
    `"entrypoint":"sdk-cli"`, plus a `{"type":"custom-title","customTitle":"FLEET"}`
    line. `collectResumableFrom` does not list `<forkid>`.
- [ ] **Step 2: Run them.** Expect PASS. If one fails, fix the filter at
  the point the code map names (session.go:233 or 327, resume.go:427) and
  re-run.
- [ ] **Step 3: Commit** `fleet clarify: pin that a fork is never a session row`.

### Task 5: TUI client and clarify state (no drawing)

**Files:**
- Modify:
  - `fleet_client.go`: client methods; `fleetStateView` gains
    `Clarify map[string]clarifySummary` and `FleetTool string` (from
    `fleet.tool`); sanitize.
  - `fleet_tui_state.go`: the `k` key and clarify mode.
- Test: `fleet_tui_clarify_test.go`

**Interfaces:**
- Consumes: Task 3's wire shapes.
- Produces:
  - `type fleetClarifyView struct { Card string; Turns []clarifyTurn; Pending *clarifyPending; Draft *clarifyDraft }`.
    All text has passed through `fleetDisplayText`.
  - `func (c *fleetClient) ClarifyGet(cardID string) (fleetClarifyView, error)`
  - `func (c *fleetClient) ClarifyAsk(cardID, text string) (string, error)`,
    which returns the refusal code (`""` means accepted).
  - `func (c *fleetClient) ClarifyDraft(cardID string, discard bool) (string, error)`
  - `type fleetClarifyMode struct { item fleetItem; source string; view fleetClarifyView; loaded bool; input string; openedAt time.Time; scroll int; err string }`
  - `fleetViewState.clarify *fleetClarifyMode`
  - New `fleetCmd` values: `fleetCmdClarifyGet`, `fleetCmdClarifyAsk`,
    `fleetCmdClarifyDraft`, `fleetCmdClarifyDiscard`.
  - `fleetViewState.clarifyReq *fleetClarifyRequest`, where
    `type fleetClarifyRequest struct { Source, CardID, Text string }`.
  - `func (st *fleetViewState) applyClarify(v fleetClarifyView, err error, now time.Time)`
  - `func (st *fleetViewState) applyClarifyRefusal(code string, now time.Time)`.
    It toasts a fixed text per code:

    | Code | Toast |
    | --- | --- |
    | `busy` | `a question is already running` |
    | `card_gone` | `card is gone` |
    | `not_live` | `FLEET is not running` |
    | `ambiguous` | `two FLEETs are running` |
    | `unsupported_tool` | `Clarify needs a Claude FLEET` |
    | `empty` | `nothing to draft yet` |
    | `too_long` | `question too long` |
    | other | `clarify failed` |

Keys:
- **List, `k`** works only when the selected item is an open decision card
  with an id, `FleetTool != "grok"`, and no panel is open. It sets
  `st.clarify` (with `openedAt` and the source of the applied state), clears
  the toast, and returns `fleetCmdClarifyGet`. Otherwise `k` does nothing.
- **Clarify mode**, handled by `handleClarifyKey(k, inBatch, now)`:
  - Printable ASCII appends to `input`, using `fleetEditOther`'s editing
    rules.
  - Backspace deletes. Inside a batch, Enter types a space.
  - A lone Enter, past `fleetArmDelay` since `openedAt`, with non-blank
    input and no `Pending`, sets
    `clarifyReq{Source, CardID, Text: input}`, clears `input` and returns
    `fleetCmdClarifyAsk`.
  - `ctrl-d`, with at least one turn and no pending, returns
    `fleetCmdClarifyDraft`.
  - `s` with a draft and an empty input closes clarify mode and opens the
    Other panel on that card (`openOther`) with `panel.comment` set to the
    draft text, collapsed to one line with single spaces. Returns
    `fleetCmdRender`.
  - `x` with a draft and an empty input returns `fleetCmdClarifyDiscard`.
  - `s` and `x` with a non-empty input are ordinary typed letters.
  - PgUp/PgDn change `scroll`.
  - Esc closes clarify mode, keeping the list selection, and returns
    `fleetCmdRender`.
  - Esc, `q` and Ctrl-C never close the whole view from clarify mode.
- **`applyState`:** when clarify is open and its card is no longer in the
  view's decision items, close it with the toast `card is gone`.

- [ ] **Step 1: Write the failing tests**
  - `TestFleetClientClarify`: an `httptest` server asserts the method, the
    path, the bearer header and the JSON bodies of all three calls.
    Refusal bodies (`409 {"code":"busy"}`) come back as the code with a nil
    error.
  - `TestClarifyViewSanitizes`: a GET answer with
    `"text":"a\u001b[31mred\u001b]8;;x\u001b\\b"` decodes to text with no
    ESC byte.
  - `TestClarifyKeyOpens`:
    - `k` on an open card returns `fleetCmdClarifyGet` and sets
      `st.clarify.item`.
    - `k` on a SENT item, or with `FleetTool == "grok"`, does nothing.
  - `TestClarifyKeyAskArmAndBatch`:
    - Enter before 400 ms does nothing.
    - Enter in a batch types a space.
    - A lone Enter after 400 ms gives `fleetCmdClarifyAsk`, with
      `clarifyReq.Text == "why b"` and the input cleared.
    - Enter while `Pending` is set does nothing.
  - `TestClarifyDraftToOther`:
    - With `Draft.Text = "Pick B,\n only scans"`, `s` gives
      `st.clarify == nil`, `st.panel.other == true`, and
      `st.panel.comment == "Pick B, only scans"`.
    - `x` gives `fleetCmdClarifyDiscard`.
    - With input `"s"` already typed, `s` appends instead.
  - `TestClarifyEscReturnsToList`: Esc gives `st.clarify == nil` and the
    same `st.sel`.
  - `TestClarifyClosesWhenCardGone`.
- [ ] **Step 2: Run them; expect FAIL.**
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run them; expect PASS.**
- [ ] **Step 5: Commit** `fleet tui: clarify client and key handling`.

### Task 6: TUI drawing and loop wiring

**Files:**
- Modify:
  - `fleet_tui.go`: `renderFleetView` branches to `fleetClarifyLines` when
    `st.clarify != nil`; `fleetLoop` gets a clarify job list and the 2 s
    poll; row marker; footer; `runFleetView` hands the source pin.
- Test: `fleet_tui_clarify_test.go` (append)

**Interfaces:**
- Consumes: Task 5.
- Produces:
  - `func fleetClarifyLines(st *fleetViewState, cols, rows int, now time.Time) []string`
  - `fleetLoop.clarify func(fleetClarifyRequest, kind fleetCmd) fleetClarifyJobResult`
  - `fleetClarifyVia(src *fleetLocal) func(fleetClarifyRequest, fleetCmd) fleetClarifyJobResult`,
    pinned like `fleetSendVia`: a different `c.base` gives
    `{Moved:true}`, which leads to the "FLEET moved" toast and a refetch.
  - `type fleetClarifyJobResult struct { Kind fleetCmd; View fleetClarifyView; Code string; Err error; Moved bool }`.
    After an ask, draft or discard, the job does a `ClarifyGet` itself, so
    every result carries a fresh `View`.

Layout (layout B), exactly `rows` lines:
1. Line 1: `Clarify  <ticket>  <title>` with `fork · read-only`
   right-aligned and dim.
2. Line 2, dim: `  ` plus the options as `<key> <label>` joined with
   ` · `, with ` (FLEET's pick)` after the lean option. Cut to `cols`.
3. A rule.
4. The turns, wrapped to `cols-6` under a 6-column label:
   - `you` is cyan, `fleet` is magenta, `error` is yellow with the label
     `!`, and `note` is dim with no label.
   - They are bottom-anchored with `scroll` offset, with `↑ more` /
     `↓ more` dim markers as in the panel.
   - While pending, the last line is `fleet thinking… <N>s`, with N counted
     from `Pending.StartedAt`.
   - A draft draws as a yellow box titled `draft for FLEET` above the
     input.
5. A rule, then `› <input>`.
6. The help row:
   - `Enter ask · Ctrl-D draft for FLEET · Esc back`
   - with a draft: `Enter ask · s use draft as answer · x discard draft · Esc back`
   - the toast wins, as in `fleetBottomRow`.

On the list, a card row with `Clarify[id].Turns > 0` shows a dim ` ✎` after
its title.

The `k clarify` hint is added to the footer help rows only when `k` would
open (same predicate as Task 5; factor it as
`func (st *fleetViewState) canClarify() bool`).

Loop:
- While `st.clarify != nil && st.clarify.view.Pending != nil`, a
  `fleetCmdClarifyGet` runs every 2 s (`fleetClarifyPoll = 2 *
  time.Second`). `timeout` caps the wait at the next clarify poll. No
  clarify poll runs otherwise.
- The animation timer is unchanged. While clarify is open the ship screen is
  never drawn.

- [ ] **Step 1: Write the failing tests**
  - `TestFleetClarifyLinesLayout`: at 80×24 with 3 turns, a pending ask
    and a draft, assert:
    - the frame is 24 lines, each with `visualLen <= 80`;
    - line 1 holds `Clarify` and the ticket;
    - some line holds `fleet thinking…`, and some line holds
      `draft for FLEET`;
    - the last line is the draft help row.
  - `TestFleetClarifyLinesTiny`: at 40×6 there are 6 lines and no panic.
    The input line and the help row survive.
  - `TestFleetClarifyPollTimer`:
    - With pending, `l.timeout(now)` is at most 2 s and the poll starts a
      get job.
    - Without pending, no clarify job starts on tick.
  - `TestFleetClarifyViaMoved`: a request with a Source different from the
    chosen client's base gives `Moved`.
  - `TestFleetClarifyMarkerAndHint`: a card with turns shows `✎`, and
    `canClarify` toggles the `k clarify` hint.
- [ ] **Step 2: Run them; expect FAIL.**
- [ ] **Step 3: Implement.** Pass the wrap through the existing
  `fleetWrap*` helpers, so labelled lines indent like summaries.
- [ ] **Step 4: Run them; expect PASS**, plus the full suite.
- [ ] **Step 5: Manual check.**
  - Build: `go build -o /tmp/cs-clarify .`
  - Run the server in a temp HOME with a fake state.json that has one
    decision card, and with a stub `claude` early on PATH that prints canned
    JSON after `sleep 3`.
  - Open the TUI against it with `F`, then `k`.
  - Ask, and watch `thinking…` turn into the answer.
  - Press Ctrl-D, then `s`, and check that the Other panel opens with the
    draft in its box. Esc out without sending.
  - Record what you saw in the report.
- [ ] **Step 6: Commit** `fleet tui: clarify screen`.

### Task 7: Web page Clarify sheet

**Files:**
- Modify: `fleet.html`
- Test: `fleet_page_test.go`

**Interfaces:**
- Consumes: Task 3's routes and the `clarify` and `fleet.tool` state keys.

Page behavior:
- **Button.** In `decisionCard` (fleet.html:2077), next to the Other button
  (:2165), add a `clarify-btn` button labelled `Clarify`, or `Clarify · <n>`
  when `state.clarify[id].turns > 0`. Leave it out when
  `state.fleet.tool === "grok"` or the card has no id.
- **The sheet** is a new `<dialog class="sheet" id="clarify-sheet">`.
  - Pinned head: `Clarify · <ticket>`, the title through `linkText`, and the
    options joined with ` · `, the lean option bold with a ★.
  - The turns:
    - `you` turns are right-aligned bubbles of plain text (`textContent`).
    - `fleet` turns are left-aligned with a purple left border, and go
      through `renderRich(t.text)`.
    - `error` turns are warn-colored plain text; `note` turns are dim plain
      text.
  - A `thinking… Ns` line while `pending`, ticking every second.
  - A draft box with dashed yellow edges and the label `DRAFT FOR FLEET`,
    holding plain text and two buttons, `Use as answer` and `Discard`.
  - A textarea `Ask about this card…`, an `Ask` button, and a
    `Draft for FLEET` button once there is a turn.
  - Footer: `fork of FLEET · read-only · FLEET does not see this`.
- **Functions:**
  - `openClarify(c)` shows the modal sheet and fetches
    `GET /fleet/clarify?card=`.
  - `closeClarify()`.
  - `clarifyAsk()` POSTs to `/fleet/clarify/ask` with
    `credentials:"same-origin"` and JSON.
  - `clarifyDraft()` and `clarifyDiscard()`.
  - Refusal codes show in the sheet's note line with the texts in Task 5's
    table.
- **Polling:** while the sheet is open and `pending` is set, poll GET every
  2000 ms (`CLARIFY_POLL = 2000`). Stop on close.
- **Use as answer:** `closeClarify()`, then
  `prefillChat("Andy decides on " + id + ": " + draft.text.replace(/\s+/g, " ").trim())`.
- When a state poll shows the card gone while the sheet is open, close it
  with the toast `Card is gone.`

- [ ] **Step 1: Add needles to `TestFleetPageIsSelfContainedAndSafe`'s
  `need` list:**
  - `"/fleet/clarify"`, `"/fleet/clarify/ask"`, `"/fleet/clarify/draft"`
  - `"id=\"clarify-sheet\""`, `"function openClarify("`
  - `"renderRich(t.text)"` (already present), `"CLARIFY_POLL"`
  - `"fork of FLEET · read-only · FLEET does not see this"`
  - `"\"Andy decides on \" + id + \": \" + "`
  - `"state.fleet.tool === \"grok\""`
  
  The forbidden list is unchanged.
- [ ] **Step 2: Run the test; expect FAIL.**
- [ ] **Step 3: Implement**, with DOM via `el(...)` only.
- [ ] **Step 4: Run it; expect PASS.**
- [ ] **Step 5: Browser check.** Use Playwright with the recipe in memory
  `playwright-on-agent-workstation`: chromium-1200, playwright-core from
  lauko-darzelis.
  - Run the server as in Task 6's manual check, with the stub `claude`.
  - Log in with a `fleet-url` code from the test server.
  - Check, on a 390×844 viewport:
    1. Open Clarify.
    2. Ask, and see `thinking…` and then the answer.
    3. Press Draft, then Use as answer. The sheet closes and the chat input
       holds `Andy decides on <id>: <draft>`.
  - Take screenshots into the scratchpad and report their paths.
- [ ] **Step 6: Commit** `fleet page: clarify sheet`.

### Task 8: Docs

**Files:**
- Modify:
  - `CLAUDE.md`: a new `**Clarify**` paragraph group, placed after "Session
    view" under "FLEET web page", plus a short TUI note under "FLEET view
    in the TUI".
  - `CHANGELOG.md` (`## [Unreleased]` → `### Added`).

- [ ] **Step 1: Write the docs.** Cover:
  - what Clarify is;
  - Claude-only, and why grok is refused (the amendment's four findings, in
    one paragraph);
  - the exact argv shape and why each flag is there: `--setting-sources ""`,
    the `--`, `--model`, the two deny rules and why not `*--pre*`;
  - the cwd rule;
  - the limits;
  - the store file, pruning, and the restart turn;
  - why no fork-id list is needed (`sdk-cli` everywhere; pinned by Task 4's
    tests);
  - that the draft goes through the client's Other path, and the server
    does not track it;
  - the TUI keys (`k`, Ctrl-D, `s`, `x`, Esc) and the 2 s poll;
  - the CLI versions checked: claude 2.1.310 and grok 1.0.70, on
    2026-10-26.
  
  Match CLAUDE.md's existing voice: short plain sentences, and the reason
  next to each rule.
- [ ] **Step 2: Commit** `docs: FLEET Clarify`.
