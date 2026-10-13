# FLEET web page Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Serve a `/fleet` web page from the claude-sessions server that shows FLEET's cards, mirrors the FLEET conversation, keeps a history timeline, and types Andy's answers into the FLEET tmux pane.

**Architecture:** New `fleet*.go` files in package `main` add six routes to the existing main mux (`server.go` route block, ~line 2546). The server reads two files FLEET writes (`~/.claude/fleet/state.json`, `~/.claude/fleet/log.jsonl`), reads the FLEET transcript through `findTranscript` (model.go:22), and types text with the existing `sendKeys` (send_keys.go:31) after a fresh `resolveLivePID` (server.go:1018). The page is one embedded HTML file with vanilla JS. Auth is the existing bearer token, plus a `cs_fleet` cookie that only `/fleet*` routes accept.

**Tech Stack:** Go stdlib only (`net/http`, `embed`, `encoding/json`), tmux, vanilla HTML/CSS/JS, Python 3 + bash for the FLEET-side helpers.

**Spec:** `docs/superpowers/specs/2026-10-13-fleet-page-design.md`

## Global Constraints

- Dependencies: only `golang.org/x/term` and `golang.org/x/sys` plus stdlib (`embed` is stdlib). No new module.
- No external CDN, no build step for the page. It must work on the tailnet with no internet.
- Never print, log, or write the server token anywhere except: the `Set-Cookie` header, and `fleet-url` stdout.
- Stale threshold: **40 minutes** (`fleetStaleAfter = 40 * time.Minute`).
- "Done today" resets at **local midnight Europe/Vilnius** (page-side, `Intl` with `timeZone: "Europe/Vilnius"`).
- FLEET discovery: exactly one local session with `Name == "FLEET"`; 0 → not alive, 2+ → not alive with reason `ambiguous`.
- `/fleet/say` text: 1..4096 bytes (`sendKeysMaxLen`), max 1 send per second, identical text within 10 s refused.
- Cookie: `cs_fleet=<token>`, `HttpOnly`, `SameSite=Strict`, `Path=/fleet`, `Max-Age=2592000` (30 days), `Secure` only when `r.TLS != nil`.
- `/fleet*` routes go on the main mux only, never on the loopback paste mux (`listenLoopbackPaste`).
- The server makes no decisions and never interprets card meaning.
- Page renders untrusted text with `textContent` only. Links only when the URL scheme is `http:` or `https:`.
- Every message to Andy (UI copy included) is in ASD-STE100 Simplified Technical English: short sentences, active voice, simple words.
- Verification for every Go task: `go build ./... && go vet ./... && go test ./...` all green.

## Review Focus

1. **Multi-line or control-character text** from the page textarea. tmux `send-keys -l` types a literal `\n` as Enter, so a two-line message would submit half of itself, and an ESC byte could drive the Claude TUI. Expected: newlines (`\r\n`, `\r`, `\n`) flatten to one space (same as `scripts/send.sh`), any other control character (< 0x20 except tab, or 0x7f) is refused with 400. Test lives in Task 6.
2. **FLEET is at a permission prompt or an AskUserQuestion dialog.** Typed text plus Enter would pick the highlighted option ("Yes"). Expected: say refuses when the fresh row has `WaitingFor != ""`, and also when the pane capture shows no empty input box. Tests live in Task 1 and Task 6.
3. **First visit from a link tapped in another app** (cross-site navigation). A 303 redirect would not carry a fresh `SameSite=Strict` cookie, so the user would land on a 401. Expected: `GET /fleet?t=` answers 200 with a tiny HTML page that does `location.replace("/fleet")` (same-site navigation, cookie sent). Test lives in Task 2.
4. **`Referrer-Policy: no-referrer` would make browsers send `Origin: null` on POST**, which the CSRF guard would refuse. Expected: the page uses `Referrer-Policy: same-origin`; external links get `rel="noopener noreferrer"`. Test lives in Task 2.
5. **The page needs quota numbers but the cookie has `Path=/fleet`**, so it never reaches `GET /usage/numbers`, and the spec forbids the cookie on other routes. Expected: an alias `GET /fleet/usage` serves the same body as `/usage/numbers` under fleet auth. Test lives in Task 2.

Spec deviations (deliberate, called out for review):

- `GET /fleet/log?before=` takes an opaque cursor (`next` from the previous page: an absolute byte offset), not an `at` timestamp. Two entries with the same second would otherwise be skipped or repeated, and an offset stays valid while FLEET appends.
- `GET /fleet/chat?since=<pos>` replaces `after=<uuid>`. `pos` is a byte position in the transcript; the server returns every turn that changed after it and the page upserts by `uuid`. Claude Code writes later lines for the same assistant message, and a uuid cursor cannot see an earlier turn that such a line updates.
- `POST /fleet/say` types the text, captures the pane again, and presses Enter only if the input box still holds text. It uses its own `typeText`/`pressEnter` seams, not `sendKeysFn`.
- `GET /fleet/usage` is added (Review Focus 5). `/sessions` is not used by the page.
- FLEET discovery uses `cachedSessions()` only. The flags overlay holds group/disabled bits, not the name, so it adds nothing here.
- Status mapping: `waiting` when `WaitingFor != ""`, `busy` when `Status == "busy"`, else `idle` (FLEET is usually `shell` because its Monitor runs; its turn is free, so `idle`). `queued: true` only when the fresh status is `busy`.


Codex review (2026-10-13, verdict DISAGREE, 7 findings; all 7 held after a check against the code and are fixed in this plan):
1. `writeTranscript` already exists (model_test.go:88) → the chat tests use `writeChatTranscript`.
2. Say trusted the cached FLEET pick → re-run `findFleet` on the fresh list and refuse an empty session id.
3. Double tap remembered one text and stamped before the send → a 10 s text map, stamped after Enter.
4. Log cursor was a window-relative line index → absolute byte offset, backward chunked read.
5. Uuid chat cursor missed updates to earlier turns → byte-position cursor that returns changed turns.
6. Usage keys were wrong (`email`, `percent`) → real tags `account`, `pct`, `windows`.
7. Dialog check was a snapshot → type, capture again, Enter only if the box holds text; residual race documented.

---

## File Structure

| File | Responsibility |
|---|---|
| `fleet_pane.go` (create) | Capture the FLEET pane with `tmux capture-pane -p -e`; parse the Claude input box; report typed text (dim placeholder text ignored). |
| `fleet.go` (create) | `fleetDeps` (seams + state), paths, FLEET discovery, fleet auth (header or cookie), `GET /fleet` page + login, `GET /fleet/usage`, `GET /fleet/state`, `GET /fleet/log`, log append, route registration. |
| `fleet_chat.go` (create) | `GET /fleet/chat` and the cursor-aware transcript extraction. |
| `fleet_say.go` (create) | `POST /fleet/say`: origin guard, body decode, text sanitizing, rate limit, discovery + resolve, pane check, send, log line. |
| `fleet.html` (create) | The embedded page. |
| `fleet_url.go` (create) | `claude-sessions fleet-url` subcommand. |
| `fleet_test.go`, `fleet_pane_test.go`, `fleet_chat_test.go`, `fleet_say_test.go`, `fleet_url_test.go` (create) | Tests next to code. |
| `testdata/fleet/pane-empty-placeholder.ansi`, `testdata/fleet/pane-typed.ansi` (already captured) | Real `capture-pane -p -e` output of a Claude Code pane: empty box with dim placeholder `Try "refactor tui.go"`, and box with typed `hello there`. |
| `server.go` (modify) | Add `fleet fleetDeps` field to `server`; call `registerFleetRoutes(mux, s)` after the route block. |
| `usage_numbers.go` (modify) | Split `usageNumbers` into auth + `serveUsageNumbers(w)`. |
| `main.go` (modify) | `case "fleet-url"` + usage line. |
| `CLAUDE.md`, `README.md` (modify) | Short "FLEET web page" section; README command list line. |
| `~/.claude/skills/fleet/scripts/fleet-state.py` (create, outside repo) | Validate a JSON draft and atomically write `state.json`. |
| `~/.claude/skills/fleet/scripts/fleet-log.sh` (create, outside repo) | Append one `log.jsonl` line. |
| `~/.claude/skills/fleet/SKILL.md` (modify, outside repo) | Section "Keep the web page current". |

Facts the engineer needs (verified 2026-10-13 against this tree):

- `server` struct: server.go:651. `authed`: server.go:768 (constant-time compare of `Authorization` to `"Bearer "+s.token`). `cachedSessions`: server.go:795 (1 s cache). `resolveLivePID(pid, wantSession) (*Session, []Session, *actionResult)`: server.go:1018, always a fresh `collectLocal()`.
- `sendKeysHandler`: server.go:1566. Seam `s.sendKeysFn func(Session, string) error`, nil → `sendKeys`.
- `transcriptTail`: server.go:1397. `extractConversationTail`, `extractTurnText`, `transcriptLine`: info_transcript.go. The user-turn filter is: `isMeta` dropped; only `type` user/assistant; content must yield text blocks (`extractTurnText` drops tool_result-only content); a user text whose trimmed form starts with `<` is dropped (system notices, command wrappers). Assistant re-emissions dedupe on `message.id + "\x00" + requestId`, last wins.
- `usageNumbers`: usage_numbers.go:484.
- `Session` fields: `PID`, `SessionID`, `Name`, `NameSource`, `Status`, `WaitingFor`, `Tmux`, `Tool` (session.go). `Waiting()` = `WaitingFor != ""`.
- Live FLEET row today: `pid 162244 name "FLEET" nameSource user status shell tmux trecs-brain-1ecace:0.0`. Another row is named `"DR-5042 FLEET Ticket Pickup Worktree"`, so the match must be exact equality.
- Claude Code input box (captured with `-e`): a rule line of `─` (U+2500) in `ESC[38;5;244m`, then `❯` (U+276F) + NBSP (U+00A0), then the input, then another rule line. The empty-box placeholder is wrapped in `ESC[2m … ESC[0m` (dim). Typed text has no SGR.
- `tailscaleBinary()` and `tailscaleCLIEnv()` are in server.go (~2168, ~2213). `defaultServerPort = 8765` (server.go:32). `readServerToken()` server.go:2101 reads without creating.
- Tests use `httptest.NewRequest` + `httptest.NewRecorder` against handler methods on a bare `&server{token: "tok", collect: ...}`.

---

### Task 1: Pane input-box parser

**Files:**
- Create: `fleet_pane.go`
- Test: `fleet_pane_test.go` (uses `testdata/fleet/*.ansi`)

**Interfaces:**
- Produces: `func paneInputText(capture string) (text string, found bool)` — `found` is false when no input box is visible; `text` is the typed input with dim (placeholder) runs, the `❯` glyph, NBSP and surrounding spaces removed. `func capturePane(target string) (string, error)` — runs `tmux capture-pane -p -e -t <target>`; refuses an empty target.

- [ ] **Step 1: Write the failing tests**

```go
// fleet_pane_test.go
package main

import (
	"os"
	"testing"
)

func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/fleet/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestPaneInputTextIgnoresDimPlaceholder(t *testing.T) {
	text, found := paneInputText(readFixture(t, "pane-empty-placeholder.ansi"))
	if !found || text != "" {
		t.Fatalf("got (%q, %v), want (\"\", true)", text, found)
	}
}

func TestPaneInputTextReportsTypedText(t *testing.T) {
	text, found := paneInputText(readFixture(t, "pane-typed.ansi"))
	if !found || text != "hello there" {
		t.Fatalf("got (%q, %v), want (\"hello there\", true)", text, found)
	}
}

func TestPaneInputTextEmptyBoxNoPlaceholder(t *testing.T) {
	rule := "\x1b[38;5;244m" + repeatRune('─', 40) + "\n"
	capture := "output\n" + rule + "\x1b[39m❯ \x1b[39m\n" + rule + "\x1b[39m  status line\n"
	text, found := paneInputText(capture)
	if !found || text != "" {
		t.Fatalf("got (%q, %v)", text, found)
	}
}

func TestPaneInputTextMultiLineInput(t *testing.T) {
	rule := repeatRune('─', 40) + "\n"
	capture := rule + "❯ first line\n  second line\n" + rule + "status\n"
	text, found := paneInputText(capture)
	if !found || text != "first line second line" {
		t.Fatalf("got (%q, %v)", text, found)
	}
}

func TestPaneInputTextNoBoxIsNotFound(t *testing.T) {
	// A permission dialog replaces the input box: one top rule, a menu with
	// the ❯ cursor on "1. Yes", no closing rule.
	capture := repeatRune('─', 40) + "\n Bash command\n Do you want to proceed?\n ❯ 1. Yes\n   2. No\n"
	if _, found := paneInputText(capture); found {
		t.Fatal("dialog without a closed input box must read as not found")
	}
}

func TestPaneInputTextBoxWithoutPromptGlyphIsNotFound(t *testing.T) {
	rule := repeatRune('─', 40) + "\n"
	capture := rule + " Select an option\n" + rule
	if _, found := paneInputText(capture); found {
		t.Fatal("a ruled region whose first line is not ❯ is not the input box")
	}
}

func TestCapturePaneRefusesEmptyTarget(t *testing.T) {
	if _, err := capturePane(""); err == nil {
		t.Fatal("empty target must error: tmux -t \"\" hits the current session")
	}
}

func repeatRune(r rune, n int) string {
	out := make([]rune, n)
	for i := range out {
		out[i] = r
	}
	return string(out)
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -run 'TestPaneInputText|TestCapturePane' ./...`
Expected: FAIL, `undefined: paneInputText`.

- [ ] **Step 3: Write the implementation**

```go
// fleet_pane.go
package main

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"unicode/utf8"
)

// capturePane returns the visible content of a tmux pane with SGR escapes
// kept (-e): the dim attribute is the only thing that tells Claude Code's
// placeholder text ("Try …") apart from text a person has typed.
func capturePane(target string) (string, error) {
	if target == "" {
		return "", fmt.Errorf("no tmux pane")
	}
	out, err := exec.Command("tmux", "capture-pane", "-p", "-e", "-t", target).Output()
	if err != nil {
		return "", fmt.Errorf("capture pane: %w", err)
	}
	return string(out), nil
}

// paneInputText finds Claude Code's input box in a capture-pane -e dump —
// the region between the last two rule lines, whose first line starts with
// the ❯ prompt glyph — and returns the text typed into it. Dim runs (SGR 2)
// are the empty box's placeholder and are dropped. found is false when no
// such box is visible: a permission prompt or question dialog has replaced
// it, and typed text plus Enter would answer that dialog instead.
func paneInputText(capture string) (text string, found bool) {
	lines := strings.Split(strings.TrimRight(capture, "\n"), "\n")
	plain := make([]string, len(lines))
	for i, l := range lines {
		plain[i] = visibleText(l)
	}
	bottom := -1
	for i := len(plain) - 1; i >= 0; i-- {
		if isRuleLine(plain[i]) {
			bottom = i
			break
		}
	}
	if bottom < 0 {
		return "", false
	}
	top := -1
	for i := bottom - 1; i >= 0; i-- {
		if isRuleLine(plain[i]) {
			top = i
			break
		}
	}
	if top < 0 || bottom-top < 2 {
		return "", false
	}
	first := strings.TrimLeft(plain[top+1], " ")
	if !strings.HasPrefix(first, "❯") {
		return "", false
	}
	var parts []string
	for i := top + 1; i < bottom; i++ {
		l := plain[i]
		if i == top+1 {
			l = strings.TrimPrefix(strings.TrimLeft(l, " "), "❯")
		}
		l = strings.TrimSpace(strings.ReplaceAll(l, " ", " "))
		if l != "" {
			parts = append(parts, l)
		}
	}
	return strings.Join(parts, " "), true
}

// isRuleLine reports a line made only of ─ (at least 10), the border Claude
// Code draws above and below its input box.
func isRuleLine(s string) bool {
	s = strings.TrimSpace(s)
	if strings.Count(s, "─") < 10 {
		return false
	}
	return strings.Trim(s, "─") == ""
}

// visibleText strips CSI escape sequences from one captured line and drops
// every rune printed while the dim attribute (SGR 2) is on.
func visibleText(line string) string {
	var b strings.Builder
	dim := false
	for i := 0; i < len(line); {
		if line[i] == 0x1b && i+1 < len(line) && line[i+1] == '[' {
			j := i + 2
			for j < len(line) && (line[j] < 0x40 || line[j] > 0x7e) {
				j++
			}
			if j < len(line) && line[j] == 'm' {
				dim = applySGRDim(line[i+2:j], dim)
			}
			i = j + 1
			continue
		}
		r, size := rune(line[i]), 1
		if line[i] >= 0x80 {
			r, size = utf8.DecodeRuneInString(line[i:])
		}
		if !dim {
			b.WriteRune(r)
		}
		i += size
	}
	return b.String()
}

// applySGRDim folds one SGR parameter list into the dim state: 2 sets it,
// 0 / empty (reset) and 22 (normal intensity) clear it. 38;5;N and 38;2;R;G;B
// arguments are skipped so a colour index of 2 is not read as dim.
func applySGRDim(params string, dim bool) bool {
	if params == "" {
		return false
	}
	fields := strings.Split(params, ";")
	for i := 0; i < len(fields); i++ {
		n, err := strconv.Atoi(fields[i])
		if err != nil {
			continue
		}
		switch {
		case n == 38 || n == 48 || n == 58:
			if i+1 < len(fields) && fields[i+1] == "5" {
				i += 2
			} else if i+1 < len(fields) && fields[i+1] == "2" {
				i += 4
			}
		case n == 0 || n == 22:
			dim = false
		case n == 2:
			dim = true
		}
	}
	return dim
}
```

Note on the fixture: the placeholder line is `ESC[39m❯NBSP ESC[2mTry "refactor tui.go"ESC[0m`, the typed line is `ESC[39m❯NBSP hello there`. If `TestPaneInputTextReportsTypedText` sees extra cursor glyphs, inspect the fixture with `cat -v`; do not edit the fixture.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -run 'TestPaneInputText|TestCapturePane' ./... && go vet ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add fleet_pane.go fleet_pane_test.go testdata/fleet
git commit -m "feat(fleet): parse the Claude input box from a pane capture"
```

---

### Task 2: Fleet deps, auth, page login, discovery, usage alias, routes

**Files:**
- Create: `fleet.go`, `fleet.html` (placeholder content in this task, real page in Task 7)
- Modify: `server.go` (struct field + one call after the route block), `usage_numbers.go:484`
- Test: `fleet_test.go`

**Interfaces:**
- Consumes: `s.authed`, `s.cachedSessions`, `s.numbers`.
- Produces:
  - `type fleetDeps struct` with fields `dir string` ("" → `~/.claude/fleet`), `now func() time.Time`, `capture func(string) (string, error)` (nil → `capturePane`), plus unexported state used by Tasks 3–4: `mu sync.Mutex`, `good map[string]json.RawMessage`, `goodMod time.Time`, `goodSize int64`, `logMu sync.Mutex`. Task 6 adds the say seams and state (`typeText`, `pressEnter`, `sleep`, `sayMu`, `lastSayAt`, `saidAt`).
  - `func (s *server) fleetDir() string`, `func (s *server) fleetNow() time.Time`.
  - `type fleetAuthKind int` with `fleetAuthNone`, `fleetAuthBearer`, `fleetAuthCookie`; `func (s *server) fleetAuth(r *http.Request) fleetAuthKind`.
  - `type fleetSession struct { Alive bool; SessionID string; PID int; Status string; Reason string }` with JSON tags `alive, sessionId, pid, status, reason` (reason `omitempty`).
  - `func findFleet(sessions []Session) (Session, string)` — returns the row and "" on exactly one match; reason `"not running"` on 0, `"ambiguous"` on 2+.
  - `func fleetStatus(s Session) string` — `waiting` / `busy` / `idle`.
  - `func registerFleetRoutes(mux *http.ServeMux, s *server)`.
  - `func (s *server) serveUsageNumbers(w http.ResponseWriter)`.

- [ ] **Step 1: Write the failing tests**

```go
// fleet_test.go
package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func fleetTestServer(t *testing.T, sessions []Session) *server {
	t.Helper()
	return &server{
		token:   "tok",
		collect: func() ([]Session, error) { return sessions, nil },
		fleet:   fleetDeps{dir: t.TempDir()},
	}
}

func fleetMux(s *server) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /sessions", s.sessions)
	registerFleetRoutes(mux, s)
	return mux
}

func TestFleetAuthBearerAndCookie(t *testing.T) {
	s := fleetTestServer(t, nil)
	r := httptest.NewRequest("GET", "/fleet/state", nil)
	if s.fleetAuth(r) != fleetAuthNone {
		t.Fatal("no credentials must be none")
	}
	r.Header.Set("Authorization", "Bearer tok")
	if s.fleetAuth(r) != fleetAuthBearer {
		t.Fatal("bearer header")
	}
	r = httptest.NewRequest("GET", "/fleet/state", nil)
	r.AddCookie(&http.Cookie{Name: "cs_fleet", Value: "tok"})
	if s.fleetAuth(r) != fleetAuthCookie {
		t.Fatal("cookie")
	}
	r = httptest.NewRequest("GET", "/fleet/state", nil)
	r.AddCookie(&http.Cookie{Name: "cs_fleet", Value: "wrong"})
	if s.fleetAuth(r) != fleetAuthNone {
		t.Fatal("wrong cookie must be none")
	}
}

func TestFleetCookieRejectedOnSessions(t *testing.T) {
	s := fleetTestServer(t, nil)
	r := httptest.NewRequest("GET", "/sessions", nil)
	r.AddCookie(&http.Cookie{Name: "cs_fleet", Value: "tok"})
	w := httptest.NewRecorder()
	fleetMux(s).ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("GET /sessions with only the fleet cookie: %d, want 401", w.Code)
	}
}

func TestFleetLoginSetsCookieAndLeavesURL(t *testing.T) {
	s := fleetTestServer(t, nil)
	w := httptest.NewRecorder()
	fleetMux(s).ServeHTTP(w, httptest.NewRequest("GET", "/fleet?t=tok", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("code %d, want 200 (HTML hop, not a 303: see Review Focus 3)", w.Code)
	}
	c := w.Result().Cookies()
	if len(c) != 1 || c[0].Name != "cs_fleet" || c[0].Value != "tok" || c[0].Path != "/fleet" ||
		!c[0].HttpOnly || c[0].SameSite != http.SameSiteStrictMode || c[0].Secure || c[0].MaxAge != 2592000 {
		t.Fatalf("cookie = %+v", c)
	}
	body := w.Body.String()
	if !strings.Contains(body, `location.replace("/fleet")`) || strings.Contains(body, "tok") {
		t.Fatalf("login hop must redirect by script and never echo the token: %q", body)
	}
	if got := w.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Fatalf("login hop Referrer-Policy = %q, want no-referrer", got)
	}
}

func TestFleetLoginWrongToken(t *testing.T) {
	s := fleetTestServer(t, nil)
	w := httptest.NewRecorder()
	fleetMux(s).ServeHTTP(w, httptest.NewRequest("GET", "/fleet?t=nope", nil))
	if w.Code != http.StatusUnauthorized || len(w.Result().Cookies()) != 0 {
		t.Fatalf("code %d cookies %v", w.Code, w.Result().Cookies())
	}
}

func TestFleetPageNeedsAuthAndSetsHeaders(t *testing.T) {
	s := fleetTestServer(t, nil)
	w := httptest.NewRecorder()
	fleetMux(s).ServeHTTP(w, httptest.NewRequest("GET", "/fleet", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("no auth: %d", w.Code)
	}
	r := httptest.NewRequest("GET", "/fleet", nil)
	r.AddCookie(&http.Cookie{Name: "cs_fleet", Value: "tok"})
	w = httptest.NewRecorder()
	fleetMux(s).ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("page: %d %q", w.Code, w.Header().Get("Content-Type"))
	}
	if got := w.Header().Get("Referrer-Policy"); got != "same-origin" {
		t.Fatalf("Referrer-Policy = %q, want same-origin (no-referrer nulls Origin on POST)", got)
	}
	if !strings.Contains(w.Header().Get("Content-Security-Policy"), "default-src 'none'") {
		t.Fatalf("CSP = %q", w.Header().Get("Content-Security-Policy"))
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("page must be no-store")
	}
}

func TestFindFleet(t *testing.T) {
	one := []Session{{PID: 1, Name: "FLEET", SessionID: "a"}, {PID: 2, Name: "DR-5042 FLEET Ticket Pickup"}}
	if got, reason := findFleet(one); reason != "" || got.PID != 1 {
		t.Fatalf("one: %+v %q", got, reason)
	}
	if _, reason := findFleet(nil); reason != "not running" {
		t.Fatalf("zero: %q", reason)
	}
	two := []Session{{PID: 1, Name: "FLEET"}, {PID: 3, Name: "FLEET"}}
	if _, reason := findFleet(two); reason != "ambiguous" {
		t.Fatalf("two: %q", reason)
	}
}

func TestFleetStatus(t *testing.T) {
	cases := map[string]Session{
		"waiting": {Status: "busy", WaitingFor: "permission prompt"},
		"busy":    {Status: "busy"},
		"idle":    {Status: "shell"},
	}
	for want, sess := range cases {
		if got := fleetStatus(sess); got != want {
			t.Errorf("%+v: %q want %q", sess, got, want)
		}
	}
	if got := fleetStatus(Session{Status: "idle"}); got != "idle" {
		t.Errorf("idle: %q", got)
	}
}

func TestFleetUsageAliasUsesFleetAuth(t *testing.T) {
	s := fleetTestServer(t, nil) // s.numbers nil → 503 once authed
	w := httptest.NewRecorder()
	fleetMux(s).ServeHTTP(w, httptest.NewRequest("GET", "/fleet/usage", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("no auth: %d", w.Code)
	}
	r := httptest.NewRequest("GET", "/fleet/usage", nil)
	r.AddCookie(&http.Cookie{Name: "cs_fleet", Value: "tok"})
	w = httptest.NewRecorder()
	fleetMux(s).ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("cookie auth reaches the usage body: %d, want 503 with nil source", w.Code)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -run 'TestFleet|TestFindFleet' ./...`
Expected: FAIL, `undefined: fleetDeps`.

- [ ] **Step 3: Write the implementation**

`fleet.html` for now (Task 7 replaces it):

```html
<!doctype html><meta charset="utf-8"><title>FLEET</title><p>FLEET</p>
```

`fleet.go`:

```go
// fleet.go
package main

import (
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

//go:embed fleet.html
var fleetPageHTML []byte

const (
	fleetCookieName   = "cs_fleet"
	fleetCookieMaxAge = 30 * 24 * 60 * 60
	fleetSessionName  = "FLEET"
	// fleetStaleAfter: FLEET's idle monitor re-arms every 30 minutes, so a
	// healthy FLEET writes state.json at least that often.
	fleetStaleAfter = 40 * time.Minute
	fleetPageCSP    = "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; " +
		"connect-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
)

// fleetDeps holds the /fleet routes' seams and their small in-memory state.
// The zero value is the production configuration.
type fleetDeps struct {
	dir     string                       // "" → ~/.claude/fleet
	now     func() time.Time             // nil → time.Now
	capture func(string) (string, error) // nil → capturePane

	mu       sync.Mutex // guards the last good state.json copy
	good     map[string]json.RawMessage
	goodMod  time.Time
	goodSize int64

	logMu sync.Mutex // serializes this process's log.jsonl appends
}

func (s *server) fleetDir() string {
	if s.fleet.dir != "" {
		return s.fleet.dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "fleet")
}

func (s *server) fleetNow() time.Time {
	if s.fleet.now != nil {
		return s.fleet.now()
	}
	return time.Now()
}

type fleetAuthKind int

const (
	fleetAuthNone fleetAuthKind = iota
	fleetAuthBearer
	fleetAuthCookie
)

// fleetAuth accepts the bearer header or the cs_fleet cookie. Only /fleet*
// handlers call it; every other route keeps s.authed, so the cookie opens
// nothing else.
func (s *server) fleetAuth(r *http.Request) fleetAuthKind {
	if s.authed(r) {
		return fleetAuthBearer
	}
	if c, err := r.Cookie(fleetCookieName); err == nil &&
		subtle.ConstantTimeCompare([]byte(c.Value), []byte(s.token)) == 1 {
		return fleetAuthCookie
	}
	return fleetAuthNone
}

type fleetSession struct {
	Alive     bool   `json:"alive"`
	SessionID string `json:"sessionId"`
	PID       int    `json:"pid"`
	Status    string `json:"status"`
	Reason    string `json:"reason,omitempty"`
}

// findFleet picks the one local session named exactly FLEET.
func findFleet(sessions []Session) (Session, string) {
	var hit Session
	n := 0
	for _, sess := range sessions {
		if sess.Name == fleetSessionName {
			hit = sess
			n++
		}
	}
	switch n {
	case 0:
		return Session{}, "not running"
	case 1:
		return hit, ""
	default:
		return Session{}, "ambiguous"
	}
}

// fleetStatus maps a row onto the page's three states. FLEET usually reads
// "shell" (its Monitor runs in the background) while its turn is free, so
// anything that is neither waiting nor busy is idle.
func fleetStatus(sess Session) string {
	switch {
	case sess.Waiting():
		return "waiting"
	case sess.Status == "busy":
		return "busy"
	default:
		return "idle"
	}
}

// discoverFleet resolves FLEET from the cached session list.
func (s *server) discoverFleet() (Session, fleetSession) {
	sessions, err := s.cachedSessions()
	if err != nil {
		return Session{}, fleetSession{Reason: "session list failed"}
	}
	sess, reason := findFleet(sessions)
	if reason != "" {
		return Session{}, fleetSession{Reason: reason}
	}
	return sess, fleetSession{Alive: true, SessionID: sess.SessionID, PID: sess.PID, Status: fleetStatus(sess)}
}

func registerFleetRoutes(mux *http.ServeMux, s *server) {
	mux.HandleFunc("GET /fleet", s.fleetPage)
	mux.HandleFunc("GET /fleet/usage", s.fleetUsage)
	mux.HandleFunc("GET /fleet/state", s.fleetState) // Task 3
	mux.HandleFunc("GET /fleet/log", s.fleetLog)     // Task 4
	mux.HandleFunc("GET /fleet/chat", s.fleetChat)   // Task 5
	mux.HandleFunc("POST /fleet/say", s.fleetSay)    // Task 6
}

// fleetPage serves the page. With ?t=<token> it is the one-time login: set
// the cookie and leave the URL by script. A 303 would not work from a link
// tapped in another app: the redirect is part of a cross-site navigation, so
// the browser would not send the new SameSite=Strict cookie on it.
func (s *server) fleetPage(w http.ResponseWriter, r *http.Request) {
	if t := r.URL.Query().Get("t"); t != "" {
		if subtle.ConstantTimeCompare([]byte(t), []byte(s.token)) != 1 {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name: fleetCookieName, Value: s.token, Path: "/fleet", MaxAge: fleetCookieMaxAge,
			HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil,
		})
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><meta charset="utf-8"><title>FLEET</title>` +
			`<meta http-equiv="refresh" content="0;url=/fleet"><script>location.replace("/fleet")</script>`))
		return
	}
	if s.fleetAuth(r) == fleetAuthNone {
		http.Error(w, "Unauthorized. Run `claude-sessions fleet-url` on the host and open that URL.", http.StatusUnauthorized)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", fleetPageCSP)
	h.Set("Referrer-Policy", "same-origin")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	_, _ = w.Write(fleetPageHTML)
}

// fleetUsage is GET /usage/numbers under fleet auth: the cookie has
// Path=/fleet, so the page cannot reach /usage/numbers itself.
func (s *server) fleetUsage(w http.ResponseWriter, r *http.Request) {
	if s.fleetAuth(r) == fleetAuthNone {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	s.serveUsageNumbers(w)
}
```

In `usage_numbers.go`, replace the body of `usageNumbers` after the auth check with a call to a new method, so the alias shares it:

```go
func (s *server) usageNumbers(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	s.serveUsageNumbers(w)
}

func (s *server) serveUsageNumbers(w http.ResponseWriter) {
	if s.numbers == nil {
		http.Error(w, "usage numbers unavailable", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, s.numbers.build())
}
```

In `server.go`: add to the `server` struct, after `paste *pasteBroker`:

```go
	// fleet backs the /fleet routes (fleet.go): seams plus the last good
	// state.json copy and the say rate limit.
	fleet fleetDeps
```

and after `mux.HandleFunc("POST /paste", s.pasteUpload)`:

```go
	registerFleetRoutes(mux, s)
```

To compile this task alone, add stub handlers at the bottom of `fleet.go` for `fleetState`, `fleetLog`, `fleetChat`, `fleetSay` that answer `http.Error(w, "not implemented", http.StatusNotImplemented)`. Tasks 3–6 replace each stub (delete the stub in the same commit that adds the real one).

- [ ] **Step 4: Run tests to verify they pass**

Run: `go build ./... && go vet ./... && go test -run 'TestFleet|TestFindFleet|TestUsageNumbers' ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add fleet.go fleet.html fleet_test.go server.go usage_numbers.go
git commit -m "feat(fleet): add /fleet page login, fleet auth, discovery and usage alias"
```

---

### Task 3: `GET /fleet/state`

**Files:**
- Modify: `fleet.go` (replace the `fleetState` stub)
- Test: `fleet_test.go`

**Interfaces:**
- Consumes: `fleetDeps`, `discoverFleet`, `fleetNow`, `fleetDir`.
- Produces: `func (s *server) loadFleetState() (map[string]json.RawMessage, string)` — the object to serve and a `stateError` string ("" when fine). Response = that object plus keys `fleet` (`fleetSession`), `stale` (bool), `stateError` (string or null).

Rules:
- Missing file, no good copy → `{"version":1,"cards":[]}` + `stateError: "FLEET has not written state yet"`.
- Unreadable / invalid JSON / not an object / `cards` missing or not an array → last good copy if any (else the empty object) + `stateError: "state.json is invalid: <short reason>"`. Never 500.
- Cache: re-read only when `(mtime, size)` changed; a valid read replaces the good copy.
- `stale`: `writtenAt` of the served object, parsed RFC 3339; missing or unparseable → `true`; older than `fleetStaleAfter` → `true`. With the "not written yet" empty object, `stale` is `false` (the banner already says why).
- File size cap: refuse to read more than 1 MiB (treat as invalid).
- Server keys always overwrite same-named keys in the file.

- [ ] **Step 1: Write the failing tests**

```go
func writeFleetFile(t *testing.T, s *server, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(s.fleet.dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func getFleetJSON(t *testing.T, s *server, path string) map[string]any {
	t.Helper()
	r := httptest.NewRequest("GET", path, nil)
	r.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	fleetMux(s).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestFleetStateValidFreshAndAlive(t *testing.T) {
	now := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	s := fleetTestServer(t, []Session{{PID: 7, Name: "FLEET", SessionID: "sid", Status: "busy"}})
	s.fleet.now = func() time.Time { return now }
	writeFleetFile(t, s, "state.json", `{"version":1,"writtenAt":"2026-10-13T09:40:00Z","cards":[{"id":"a"}],"fleet":"spoof"}`)
	out := getFleetJSON(t, s, "/fleet/state")
	if out["stale"] != false || out["stateError"] != nil {
		t.Fatalf("stale/stateError: %v %v", out["stale"], out["stateError"])
	}
	fl := out["fleet"].(map[string]any)
	if fl["alive"] != true || fl["sessionId"] != "sid" || fl["status"] != "busy" {
		t.Fatalf("fleet block = %v (server keys must overwrite file keys)", fl)
	}
	if len(out["cards"].([]any)) != 1 {
		t.Fatal("cards pass through")
	}
}

func TestFleetStateStaleAfter40Minutes(t *testing.T) {
	s := fleetTestServer(t, nil)
	s.fleet.now = func() time.Time { return time.Date(2026, 10, 13, 10, 20, 1, 0, time.UTC) }
	writeFleetFile(t, s, "state.json", `{"version":1,"writtenAt":"2026-10-13T09:40:00Z","cards":[]}`)
	if out := getFleetJSON(t, s, "/fleet/state"); out["stale"] != true {
		t.Fatal("40m01s old must be stale")
	}
}

func TestFleetStateMissingWrittenAtIsStale(t *testing.T) {
	s := fleetTestServer(t, nil)
	writeFleetFile(t, s, "state.json", `{"version":1,"cards":[]}`)
	if out := getFleetJSON(t, s, "/fleet/state"); out["stale"] != true {
		t.Fatal("no writtenAt must be stale")
	}
}

func TestFleetStateMissingFile(t *testing.T) {
	s := fleetTestServer(t, nil)
	out := getFleetJSON(t, s, "/fleet/state")
	if out["stateError"] != "FLEET has not written state yet" || len(out["cards"].([]any)) != 0 {
		t.Fatalf("missing: %v", out)
	}
	if fl := out["fleet"].(map[string]any); fl["alive"] != false || fl["reason"] != "not running" {
		t.Fatalf("fleet = %v", fl)
	}
}

func TestFleetStateInvalidKeepsLastGood(t *testing.T) {
	s := fleetTestServer(t, nil)
	writeFleetFile(t, s, "state.json", `{"version":1,"writtenAt":"2026-10-13T09:40:00Z","cards":[{"id":"good"}]}`)
	getFleetJSON(t, s, "/fleet/state")
	// Different size so the (mtime,size) cache key changes even on a coarse clock.
	writeFleetFile(t, s, "state.json", `{"version":1,"cards":[{"id":"torn"`)
	out := getFleetJSON(t, s, "/fleet/state")
	cards := out["cards"].([]any)
	if len(cards) != 1 || cards[0].(map[string]any)["id"] != "good" {
		t.Fatalf("want last good copy, got %v", cards)
	}
	if e, _ := out["stateError"].(string); !strings.HasPrefix(e, "state.json is invalid") {
		t.Fatalf("stateError = %v", out["stateError"])
	}
}

func TestFleetStateInvalidWithNoGoodCopy(t *testing.T) {
	s := fleetTestServer(t, nil)
	writeFleetFile(t, s, "state.json", `[1,2]`)
	out := getFleetJSON(t, s, "/fleet/state")
	if len(out["cards"].([]any)) != 0 || out["stateError"] == nil {
		t.Fatalf("got %v", out)
	}
}

func TestFleetStateCardsNotArrayIsInvalid(t *testing.T) {
	s := fleetTestServer(t, nil)
	writeFleetFile(t, s, "state.json", `{"version":1,"cards":{}}`)
	if out := getFleetJSON(t, s, "/fleet/state"); out["stateError"] == nil {
		t.Fatal("cards must be an array")
	}
}

func TestFleetStateAmbiguous(t *testing.T) {
	s := fleetTestServer(t, []Session{{PID: 1, Name: "FLEET"}, {PID: 2, Name: "FLEET"}})
	out := getFleetJSON(t, s, "/fleet/state")
	if fl := out["fleet"].(map[string]any); fl["alive"] != false || fl["reason"] != "ambiguous" {
		t.Fatalf("fleet = %v", fl)
	}
}
```

(Add imports `encoding/json`, `os`, `path/filepath`, `time` to `fleet_test.go`.)

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -run TestFleetState ./...`
Expected: FAIL (stub answers 501).

- [ ] **Step 3: Write the implementation** (in `fleet.go`, replacing the `fleetState` stub)

```go
const fleetStateMaxBytes = 1 << 20

func emptyFleetState() map[string]json.RawMessage {
	return map[string]json.RawMessage{"version": json.RawMessage("1"), "cards": json.RawMessage("[]")}
}

// loadFleetState returns the state object to serve and a stateError ("" when
// fine). A bad file never replaces the last good copy, and never causes a 500.
func (s *server) loadFleetState() (map[string]json.RawMessage, string) {
	f := &s.fleet
	f.mu.Lock()
	defer f.mu.Unlock()
	fallback := func(msg string) (map[string]json.RawMessage, string) {
		if f.good != nil {
			return f.good, msg
		}
		return emptyFleetState(), msg
	}
	path := filepath.Join(s.fleetDir(), "state.json")
	st, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) && f.good == nil {
			return emptyFleetState(), "FLEET has not written state yet"
		}
		return fallback("state.json is invalid: cannot read")
	}
	if f.good != nil && st.ModTime().Equal(f.goodMod) && st.Size() == f.goodSize {
		return f.good, ""
	}
	if st.Size() > fleetStateMaxBytes {
		return fallback("state.json is invalid: too large")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fallback("state.json is invalid: cannot read")
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil || obj == nil {
		return fallback("state.json is invalid: not a JSON object")
	}
	var cards []json.RawMessage
	if err := json.Unmarshal(obj["cards"], &cards); err != nil || cards == nil {
		return fallback("state.json is invalid: cards is not an array")
	}
	f.good, f.goodMod, f.goodSize = obj, st.ModTime(), st.Size()
	return obj, ""
}

func (s *server) fleetState(w http.ResponseWriter, r *http.Request) {
	if s.fleetAuth(r) == fleetAuthNone {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	state, stateErr := s.loadFleetState()
	out := make(map[string]any, len(state)+3)
	for k, v := range state {
		out[k] = v
	}
	_, fl := s.discoverFleet()
	out["fleet"] = fl
	out["stale"] = fleetStateStale(state, stateErr, s.fleetNow())
	if stateErr != "" {
		out["stateError"] = stateErr
	} else {
		out["stateError"] = nil
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, out)
}

// fleetStateStale: no parseable writtenAt, or older than fleetStaleAfter. The
// "not written yet" placeholder is not stale; its own banner explains it.
func fleetStateStale(state map[string]json.RawMessage, stateErr string, now time.Time) bool {
	if stateErr == "FLEET has not written state yet" {
		return false
	}
	var at string
	if err := json.Unmarshal(state["writtenAt"], &at); err != nil {
		return true
	}
	t, err := time.Parse(time.RFC3339, at)
	if err != nil {
		return true
	}
	return now.Sub(t) > fleetStaleAfter
}
```

Note: the "missing file but a good copy exists" case (FLEET deleted the file) serves the good copy with `stateError: "state.json is invalid: cannot read"`. That is fine.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go build ./... && go vet ./... && go test -run 'TestFleet' ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add fleet.go fleet_test.go
git commit -m "feat(fleet): serve state.json with liveness, stale flag and last good copy"
```

---

### Task 4: `GET /fleet/log` and the log append helper

**Files:**
- Modify: `fleet.go` (replace the `fleetLog` stub; add `appendFleetLog`, `readFleetLogPage`)
- Test: `fleet_test.go`

**Interfaces:**
- Produces:
  - `type fleetLogEntry struct { At string; Type string; Ticket string; Text string; URL string; CardID string }` with JSON tags `at, type, ticket, text, url, cardId` (all but `at`/`type` `omitempty`).
  - `func (s *server) appendFleetLog(e fleetLogEntry) error` — creates the dir (0700) and file (0600), one `json.Marshal` line + `\n` in one `Write` with `O_APPEND`.
  - `func (s *server) readFleetLogPage(before int64, n int) (entries []json.RawMessage, next *int64, err error)` — newest first.
  - `GET /fleet/log?before=<cursor>&n=<1..200, default 50>` → `{"entries":[…newest first…],"next":<int>|null}`.

Cursor (Codex review, finding 4): the cursor is an **absolute byte offset** into the append-only file — the offset where the oldest returned line starts. `before=K` returns the newest `n` valid lines that start before byte `K`. Absent `before` means "from the end". An absolute offset never shifts when FLEET appends, so pages never repeat or skip, and old history stays reachable. A line index relative to a tail window would drift.

Read strategy: read backward from `before` in chunks of `fleetLogChunk = 256 KiB`. A chunk that does not start at byte 0 drops its partial first line (the next older chunk ends at that line's start, so the line is read whole there). Stop when `n` valid lines are collected, byte 0 is reached, or `fleetLogMaxScan = 8 MiB` has been read in this request (then `next` = the start of the oldest line read, so the next request continues there). Lines longer than one chunk: grow the chunk (double, cap 8 MiB) until one complete line fits.

Invalid lines (not JSON, or no `at`/`type`) are skipped. A trailing line with no `\n` (a writer mid-append) is ignored. Missing file → `{"entries":[],"next":null}`. `before` < 0 or not an integer → 400; `before` > file size → treat as file size.

- [ ] **Step 1: Write the failing tests**

```go
func fleetLogLines(from, to int) string {
	var b strings.Builder
	for i := from; i < to; i++ {
		fmt.Fprintf(&b, `{"at":"2026-10-13T09:00:%02dZ","type":"merge","text":"m%d"}`+"\n", i, i)
	}
	return b.String()
}

func logTexts(out map[string]any) []string {
	var got []string
	for _, e := range out["entries"].([]any) {
		got = append(got, e.(map[string]any)["text"].(string))
	}
	return got
}

func TestFleetLogPagesNewestFirst(t *testing.T) {
	s := fleetTestServer(t, nil)
	body := fleetLogLines(0, 2) + "not json\n" + `{"text":"no at"}` + "\n" + fleetLogLines(2, 5) + `{"at":"partial`
	writeFleetFile(t, s, "log.jsonl", body)

	out := getFleetJSON(t, s, "/fleet/log?n=2")
	if got := logTexts(out); strings.Join(got, ",") != "m4,m3" {
		t.Fatalf("page 1 = %v", got)
	}
	next := int64(out["next"].(float64))
	out = getFleetJSON(t, s, fmt.Sprintf("/fleet/log?n=2&before=%d", next))
	if got := logTexts(out); strings.Join(got, ",") != "m2,m1" {
		t.Fatalf("page 2 = %v", got)
	}
	next = int64(out["next"].(float64))
	out = getFleetJSON(t, s, fmt.Sprintf("/fleet/log?n=2&before=%d", next))
	if got := logTexts(out); strings.Join(got, ",") != "m0" || out["next"] != nil {
		t.Fatalf("page 3 = %v next %v", got, out["next"])
	}
}

func TestFleetLogCursorSurvivesAppends(t *testing.T) {
	s := fleetTestServer(t, nil)
	writeFleetFile(t, s, "log.jsonl", fleetLogLines(0, 5))
	out := getFleetJSON(t, s, "/fleet/log?n=2")
	next := int64(out["next"].(float64))
	f, err := os.OpenFile(filepath.Join(s.fleet.dir, "log.jsonl"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(fleetLogLines(5, 40))
	f.Close()
	out = getFleetJSON(t, s, fmt.Sprintf("/fleet/log?n=2&before=%d", next))
	if got := logTexts(out); strings.Join(got, ",") != "m2,m1" {
		t.Fatalf("after appends page 2 = %v", got)
	}
}

func TestFleetLogSpansChunks(t *testing.T) {
	s := fleetTestServer(t, nil)
	pad := strings.Repeat("x", 300*1024) // one line bigger than a chunk
	body := fleetLogLines(0, 1) + `{"at":"2026-10-13T09:00:01Z","type":"note","text":"` + pad + `"}` + "\n" + fleetLogLines(2, 3)
	writeFleetFile(t, s, "log.jsonl", body)
	out := getFleetJSON(t, s, "/fleet/log?n=3")
	if got := out["entries"].([]any); len(got) != 3 {
		t.Fatalf("want 3 entries across chunks, got %d", len(got))
	}
}

func TestFleetLogMissingFile(t *testing.T) {
	s := fleetTestServer(t, nil)
	out := getFleetJSON(t, s, "/fleet/log")
	if len(out["entries"].([]any)) != 0 || out["next"] != nil {
		t.Fatalf("got %v", out)
	}
}

func TestFleetLogBadParams(t *testing.T) {
	s := fleetTestServer(t, nil)
	for _, q := range []string{"?n=0", "?n=201", "?before=-1", "?before=x"} {
		r := httptest.NewRequest("GET", "/fleet/log"+q, nil)
		r.Header.Set("Authorization", "Bearer tok")
		w := httptest.NewRecorder()
		fleetMux(s).ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", q, w.Code)
		}
	}
}

func TestAppendFleetLogCreatesDirAndAppends(t *testing.T) {
	s := fleetTestServer(t, nil)
	s.fleet.dir = filepath.Join(t.TempDir(), "fleet") // does not exist yet
	if err := s.appendFleetLog(fleetLogEntry{At: "2026-10-13T09:00:00Z", Type: "web-say", Text: "a\nb"}); err != nil {
		t.Fatal(err)
	}
	if err := s.appendFleetLog(fleetLogEntry{At: "2026-10-13T09:00:01Z", Type: "web-say", Text: "c", CardID: "x"}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(s.fleet.dir, "log.jsonl"))
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 || !strings.Contains(lines[1], `"cardId":"x"`) {
		t.Fatalf("log = %q", data)
	}
	if st, _ := os.Stat(filepath.Join(s.fleet.dir, "log.jsonl")); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode().Perm())
	}
}
```

(Add `fmt` to the test imports.)

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -run 'TestFleetLog|TestAppendFleetLog' ./...`
Expected: FAIL.

- [ ] **Step 3: Write the implementation**

```go
const (
	fleetLogChunk   = 256 << 10
	fleetLogMaxScan = 8 << 20
)

type fleetLogEntry struct {
	At     string `json:"at"`
	Type   string `json:"type"`
	Ticket string `json:"ticket,omitempty"`
	Text   string `json:"text,omitempty"`
	URL    string `json:"url,omitempty"`
	CardID string `json:"cardId,omitempty"`
}

func (s *server) appendFleetLog(e fleetLogEntry) error {
	dir := s.fleetDir()
	if dir == "" {
		return fmt.Errorf("no home directory")
	}
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	s.fleet.logMu.Lock()
	defer s.fleet.logMu.Unlock()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, "log.jsonl"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}

func validFleetLogLine(b []byte) bool {
	var probe struct {
		At   string `json:"at"`
		Type string `json:"type"`
	}
	return json.Unmarshal(b, &probe) == nil && probe.At != "" && probe.Type != ""
}

// readFleetLogPage reads backward from byte offset before (-1 = end of file)
// and returns up to n valid entries, newest first. next is the absolute
// offset where the oldest returned line starts, or nil when nothing older
// remains. Offsets are absolute in an append-only file, so a cursor stays
// valid however much FLEET appends after it was issued.
func (s *server) readFleetLogPage(before int64, n int) ([]json.RawMessage, *int64, error) {
	f, err := os.Open(filepath.Join(s.fleetDir(), "log.jsonl"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	end := st.Size()
	if before >= 0 && before < end {
		end = before
	}
	// A trailing line without '\n' is a writer mid-append: end the read at
	// the last newline.
	if end == st.Size() && end > 0 {
		end, err = lastNewlineEnd(f, end)
		if err != nil {
			return nil, nil, err
		}
	}
	type line struct {
		off int64
		raw []byte
	}
	var found []line // newest first
	scanned := int64(0)
	chunk := int64(fleetLogChunk)
	for end > 0 && len(found) < n && scanned < fleetLogMaxScan {
		start := max(0, end-chunk)
		buf := make([]byte, end-start)
		if _, err := f.ReadAt(buf, start); err != nil && err != io.EOF {
			return nil, nil, err
		}
		cut := 0
		if start > 0 {
			// buf ends on a line boundary, so its last byte is that line's
			// '\n'; the partial first line ends at the first '\n' before it.
			i := bytes.IndexByte(buf[:len(buf)-1], '\n')
			if i < 0 { // one line longer than the chunk: grow and retry
				if chunk >= fleetLogMaxScan {
					return nil, nil, fmt.Errorf("log line too long")
				}
				chunk *= 2
				continue
			}
			cut = i + 1
		}
		body := buf[cut:]
		var lines []line
		off := start + int64(cut)
		for len(body) > 0 {
			i := bytes.IndexByte(body, '\n')
			if i < 0 {
				break
			}
			lines = append(lines, line{off: off, raw: body[:i]})
			off += int64(i + 1)
			body = body[i+1:]
		}
		for i := len(lines) - 1; i >= 0 && len(found) < n; i-- {
			if validFleetLogLine(lines[i].raw) {
				found = append(found, lines[i])
			}
		}
		scanned += end - (start + int64(cut))
		end = start + int64(cut)
		chunk = fleetLogChunk
	}
	out := make([]json.RawMessage, len(found))
	for i, l := range found {
		out[i] = json.RawMessage(l.raw)
	}
	var next *int64
	switch {
	case len(found) == n && found[len(found)-1].off > 0:
		o := found[len(found)-1].off
		next = &o
	case len(found) < n && end > 0: // scan budget ran out before byte 0
		o := end
		next = &o
	}
	return out, next, nil
}

// lastNewlineEnd returns the offset just past the last '\n' at or before end.
func lastNewlineEnd(f *os.File, end int64) (int64, error) {
	for pos := end; pos > 0; {
		start := max(0, pos-4096)
		buf := make([]byte, pos-start)
		if _, err := f.ReadAt(buf, start); err != nil && err != io.EOF {
			return 0, err
		}
		if i := bytes.LastIndexByte(buf, '\n'); i >= 0 {
			return start + int64(i) + 1, nil
		}
		pos = start
	}
	return 0, nil
}

func (s *server) fleetLog(w http.ResponseWriter, r *http.Request) {
	if s.fleetAuth(r) == fleetAuthNone {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	q := r.URL.Query()
	n := 50
	if v := q.Get("n"); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil || p < 1 || p > 200 {
			http.Error(w, "bad n", http.StatusBadRequest)
			return
		}
		n = p
	}
	before := int64(-1)
	if v := q.Get("before"); v != "" {
		p, err := strconv.ParseInt(v, 10, 64)
		if err != nil || p < 0 {
			http.Error(w, "bad before", http.StatusBadRequest)
			return
		}
		before = p
	}
	entries, next, err := s.readFleetLogPage(before, n)
	if err != nil {
		http.Error(w, "cannot read log", http.StatusInternalServerError)
		return
	}
	if entries == nil {
		entries = []json.RawMessage{}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries, "next": next})
}
```

(Imports: `bytes`, `fmt`, `io`, `strconv`. `min`/`max` are builtins; `go.mod` says go 1.26.)

Check on `next` when the page ends exactly at the oldest valid line but invalid lines sit before it: `next` is non-nil and the following request returns `[]` with `next: null`. That is acceptable (the page hides "Load more" on an empty page).

- [ ] **Step 4: Run tests**

Run: `go build ./... && go vet ./... && go test -run 'TestFleet|TestAppendFleetLog' ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add fleet.go fleet_test.go
git commit -m "feat(fleet): page log.jsonl by byte offset and append web-say entries"
```

---

### Task 5: `GET /fleet/chat`

**Files:**
- Create: `fleet_chat.go` (move the `fleetChat` stub out of `fleet.go`)
- Modify: `info_transcript.go` (add two fields to `transcriptLine`)
- Test: `fleet_chat_test.go`

**Interfaces:**
- Consumes: `findTranscript(home, sid)` (model.go:22), `extractTurnText`, `transcriptLine`, `discoverFleet`.
- Produces:
  - `type fleetChatTurn struct { UUID string; Role string; At string; Text string }` JSON `uuid, role, at, text`.
  - `func extractChatTurns(path string, since int64, n int, tailBytes int64) (turns []fleetChatTurn, pos int64, reset bool, err error)`.
  - `GET /fleet/chat?since=<pos>&n=<1..200, default 50>` → `{"turns":[…],"pos":<int>,"reset":bool,"sessionId":"…","reason":"…"}`.

Cursor (Codex review, finding 5): the cursor is a **byte position** in the append-only transcript, not a turn uuid. The server returns every turn that **changed** at or after `since` — new turns and turns whose text a later line replaced — and `pos` for the next poll. The page upserts by `uuid`. A uuid cursor cannot see an earlier turn that a later line updates (assistant A, queued user B, then a re-emission of A).

Rules:
- Same filter as `extractConversationTail` (isMeta out; only user/assistant; `extractTurnText` must succeed; a user text starting with `<` after trim is out).
- A turn's `uuid` and `at` come from its **first** line (stable). An assistant re-emission with the same `message.id + "\x00" + requestId` replaces the text in place (last wins) and moves the turn's `changedAt` offset to that line.
- `pos` = offset just past the last complete line (a trailing line with no `\n` is Claude Code mid-write; it is re-read next poll).
- `since > 0 && since == pos` → `turns: []`, `reset: false`, **no scan** (idle FLEET costs one `stat`).
- `since` absent/0, `since > file size` (new transcript), or `since` older than the scanned window start → newest `n` turns, `reset: true`.
- Else → turns with `changedAt >= since`, oldest first; if more than `n`, keep the newest `n` and set `reset: true`.
- `since` must parse as a non-negative int64, else 400.
- Tail budget for this route: `fleetChatTailBytes = 4 << 20` (FLEET transcripts carry large tool results; 256 KiB holds too few turns).
- No FLEET session: 200 with `turns: []`, `reset: true`, `pos: 0`, `reason`. Transcript not found: same with `reason: "transcript not found"`.
- Texts are not capped.

`transcriptLine` gains `UUID string \`json:"uuid"\`` and `Timestamp string \`json:"timestamp"\``. Existing users ignore them.

- [ ] **Step 1: Write the failing tests**

Note: `writeTranscript` already exists in `model_test.go:88`. Do not redefine it; this file uses `writeChatTranscript`.

```go
// fleet_chat_test.go
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func chatLine(t *testing.T, typ, uuid, ts string, content any, extra map[string]any) string {
	t.Helper()
	m := map[string]any{"type": typ, "uuid": uuid, "timestamp": ts,
		"message": map[string]any{"role": typ, "content": content}}
	for k, v := range extra {
		if k == "message.id" {
			m["message"].(map[string]any)["id"] = v
			continue
		}
		m[k] = v
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func chatText(s string) []map[string]any { return []map[string]any{{"type": "text", "text": s}} }

func writeChatTranscript(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func appendChat(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		t.Fatal(err)
	}
}

func sampleTranscript(t *testing.T) string {
	return writeChatTranscript(t,
		chatLine(t, "user", "u1", "2026-10-13T09:00:00Z", "hello fleet", nil),
		chatLine(t, "assistant", "a1", "2026-10-13T09:00:01Z", chatText("draft"), map[string]any{"message.id": "m1", "requestId": "r1"}),
		chatLine(t, "user", "u2", "2026-10-13T09:00:02Z", []map[string]any{{"type": "tool_result", "content": "x"}}, nil),
		chatLine(t, "assistant", "a1b", "2026-10-13T09:00:03Z", chatText("final answer"), map[string]any{"message.id": "m1", "requestId": "r1"}),
		chatLine(t, "user", "u3", "2026-10-13T09:00:04Z", "<task-notification>done</task-notification>", nil),
		chatLine(t, "user", "u4", "2026-10-13T09:00:05Z", "meta", map[string]any{"isMeta": true}),
		chatLine(t, "user", "u5", "2026-10-13T09:00:06Z", "Andy decides: yes", nil),
	)
}

func TestExtractChatTurnsFiltersAndDedupes(t *testing.T) {
	path := sampleTranscript(t)
	turns, pos, reset, err := extractChatTurns(path, 0, 50, 1<<20)
	if err != nil || !reset {
		t.Fatalf("err %v reset %v", err, reset)
	}
	if st, _ := os.Stat(path); pos != st.Size() {
		t.Fatalf("pos %d, size %d", pos, st.Size())
	}
	want := []fleetChatTurn{
		{UUID: "u1", Role: "user", At: "2026-10-13T09:00:00Z", Text: "hello fleet"},
		{UUID: "a1", Role: "assistant", At: "2026-10-13T09:00:01Z", Text: "final answer"},
		{UUID: "u5", Role: "user", At: "2026-10-13T09:00:06Z", Text: "Andy decides: yes"},
	}
	if len(turns) != len(want) {
		t.Fatalf("turns = %+v", turns)
	}
	for i := range want {
		if turns[i] != want[i] {
			t.Errorf("turn %d = %+v want %+v", i, turns[i], want[i])
		}
	}
}

func TestExtractChatTurnsNoChangeSkipsScan(t *testing.T) {
	path := sampleTranscript(t)
	_, pos, _, _ := extractChatTurns(path, 0, 50, 1<<20)
	turns, pos2, reset, err := extractChatTurns(path, pos, 50, 1<<20)
	if err != nil || reset || len(turns) != 0 || pos2 != pos {
		t.Fatalf("turns %+v pos %d reset %v err %v", turns, pos2, reset, err)
	}
}

func TestExtractChatTurnsReturnsNewAndUpdatedTurns(t *testing.T) {
	// Assistant A (draft), then a queued user message B, then a later line
	// of A's same message. A uuid cursor at B would never resend A.
	path := writeChatTranscript(t,
		chatLine(t, "assistant", "a1", "2026-10-13T09:00:01Z", chatText("draft"), map[string]any{"message.id": "m1", "requestId": "r1"}),
		chatLine(t, "user", "b1", "2026-10-13T09:00:02Z", "queued question", nil),
	)
	_, pos, _, _ := extractChatTurns(path, 0, 50, 1<<20)
	appendChat(t, path,
		chatLine(t, "assistant", "a1x", "2026-10-13T09:00:03Z", chatText("final"), map[string]any{"message.id": "m1", "requestId": "r1"}),
		chatLine(t, "user", "c1", "2026-10-13T09:00:04Z", "next", nil),
	)
	turns, _, reset, err := extractChatTurns(path, pos, 50, 1<<20)
	if err != nil || reset {
		t.Fatalf("err %v reset %v", err, reset)
	}
	if len(turns) != 2 || turns[0].UUID != "a1" || turns[0].Text != "final" || turns[1].UUID != "c1" {
		t.Fatalf("turns = %+v (want updated a1, then c1; not b1)", turns)
	}
}

func TestExtractChatTurnsIgnoresPartialLastLine(t *testing.T) {
	path := sampleTranscript(t)
	st, _ := os.Stat(path)
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	_, _ = f.WriteString(`{"type":"user","uuid":"half"`)
	f.Close()
	turns, pos, _, _ := extractChatTurns(path, 0, 50, 1<<20)
	if pos != st.Size() || turns[len(turns)-1].UUID != "u5" {
		t.Fatalf("pos %d want %d, last %+v", pos, st.Size(), turns[len(turns)-1])
	}
}

func TestExtractChatTurnsTooManyKeepsNewestAndResets(t *testing.T) {
	turns, _, reset, _ := extractChatTurns(sampleTranscript(t), 0, 2, 1<<20)
	if !reset || len(turns) != 2 || turns[0].UUID != "a1" || turns[1].UUID != "u5" {
		t.Fatalf("reset %v turns %+v", reset, turns)
	}
}

func TestExtractChatTurnsSinceBeyondFileResets(t *testing.T) {
	turns, _, reset, _ := extractChatTurns(sampleTranscript(t), 1<<40, 50, 1<<20)
	if !reset || len(turns) != 3 {
		t.Fatalf("reset %v turns %+v", reset, turns)
	}
}

func TestFleetChatNoFleet(t *testing.T) {
	s := fleetTestServer(t, nil)
	out := getFleetJSON(t, s, "/fleet/chat")
	if len(out["turns"].([]any)) != 0 || out["reason"] != "not running" || out["reset"] != true {
		t.Fatalf("got %v", out)
	}
}

func TestFleetChatBadCursor(t *testing.T) {
	s := fleetTestServer(t, nil)
	for _, q := range []string{"?since=x", "?since=-1", "?n=0", "?n=201"} {
		r := httptest.NewRequest("GET", "/fleet/chat"+q, nil)
		r.Header.Set("Authorization", "Bearer tok")
		w := httptest.NewRecorder()
		fleetMux(s).ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", q, w.Code)
		}
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -run 'TestExtractChatTurns|TestFleetChat' ./...`
Expected: FAIL, `undefined: extractChatTurns`.

- [ ] **Step 3: Write the implementation**

```go
// fleet_chat.go
package main

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
)

const fleetChatTailBytes = 4 << 20

type fleetChatTurn struct {
	UUID string `json:"uuid"`
	Role string `json:"role"`
	At   string `json:"at"`
	Text string `json:"text"`
}

// extractChatTurns reads the transcript tail and returns the turns that
// changed at or after byte offset since — new turns, and turns whose text a
// later line of the same assistant message replaced — plus pos, the offset
// for the next call. A turn keeps the uuid of its first line, so the page can
// upsert. reset tells the page to replace its list instead of merging.
func extractChatTurns(path string, since int64, n int, tailBytes int64) ([]fleetChatTurn, int64, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, true, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, 0, true, err
	}
	size := st.Size()
	if since > 0 && since == size {
		return nil, size, false, nil
	}
	start := int64(0)
	if size > tailBytes {
		start = size - tailBytes
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, 0, true, err
	}
	rd := bufio.NewReaderSize(f, 64*1024)
	off := start
	if start > 0 { // drop the partial first line
		skipped, err := rd.ReadBytes('\n')
		off += int64(len(skipped))
		if err != nil {
			return nil, off, true, nil
		}
	}
	windowStart := off

	type turn struct {
		fleetChatTurn
		changedAt int64
	}
	var turns []turn
	idx := map[string]int{}
	pos := off
	for {
		line, err := rd.ReadBytes('\n')
		if err != nil { // EOF: a line without '\n' is mid-write; leave it for the next poll
			break
		}
		lineOff := off
		off += int64(len(line))
		pos = off
		var e transcriptLine
		if json.Unmarshal(line, &e) != nil || e.IsMeta {
			continue
		}
		if e.Type != "user" && e.Type != "assistant" {
			continue
		}
		text, ok := extractTurnText(e.Message.Content)
		if !ok {
			continue
		}
		if e.Type == "user" && strings.HasPrefix(strings.TrimSpace(text), "<") {
			continue
		}
		if e.Type == "assistant" {
			key := e.Message.ID + "\x00" + e.RequestID
			if i, ok := idx[key]; ok {
				turns[i].Text = text
				turns[i].changedAt = lineOff
				continue
			}
			idx[key] = len(turns)
		}
		turns = append(turns, turn{fleetChatTurn{UUID: e.UUID, Role: e.Type, At: e.Timestamp, Text: text}, lineOff})
	}

	reset := since <= 0 || since > size || since < windowStart
	var out []fleetChatTurn
	for _, t := range turns {
		if reset || t.changedAt >= since {
			out = append(out, t.fleetChatTurn)
		}
	}
	if len(out) > n {
		out, reset = out[len(out)-n:], true
	}
	return out, pos, reset, nil
}

func (s *server) fleetChat(w http.ResponseWriter, r *http.Request) {
	if s.fleetAuth(r) == fleetAuthNone {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	q := r.URL.Query()
	since := int64(0)
	if v := q.Get("since"); v != "" {
		p, err := strconv.ParseInt(v, 10, 64)
		if err != nil || p < 0 {
			http.Error(w, "bad since", http.StatusBadRequest)
			return
		}
		since = p
	}
	n := 50
	if v := q.Get("n"); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil || p < 1 || p > 200 {
			http.Error(w, "bad n", http.StatusBadRequest)
			return
		}
		n = p
	}
	w.Header().Set("Cache-Control", "no-store")
	empty := func(sid, reason string) {
		writeJSON(w, http.StatusOK, map[string]any{"turns": []fleetChatTurn{}, "pos": 0, "reset": true, "sessionId": sid, "reason": reason})
	}
	_, fl := s.discoverFleet()
	if !fl.Alive {
		empty("", fl.Reason)
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		empty(fl.SessionID, "no home dir")
		return
	}
	path := findTranscript(home, fl.SessionID)
	if path == "" {
		empty(fl.SessionID, "transcript not found")
		return
	}
	turns, pos, reset, err := extractChatTurns(path, since, n, fleetChatTailBytes)
	if err != nil {
		empty(fl.SessionID, "transcript unreadable")
		return
	}
	if turns == nil {
		turns = []fleetChatTurn{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"turns": turns, "pos": pos, "reset": reset, "sessionId": fl.SessionID})
}
```

Page note: when `sessionId` in the response differs from the one the page holds (FLEET restarted), the page clears its list and sends `since=0` next time. A transcript of a new session can be shorter than the old `pos`; the server then sees `since > size` and resets on its own, but a longer one would not, so the page check is required.

- [ ] **Step 4: Run tests**

Run: `go build ./... && go vet ./... && go test -run 'TestExtractChatTurns|TestFleetChat|TestExtractConversationTail' ./...`
Expected: PASS (existing info_transcript tests still pass).

- [ ] **Step 5: Commit**

```bash
git add fleet_chat.go fleet_chat_test.go fleet.go info_transcript.go
git commit -m "feat(fleet): mirror the FLEET transcript with a byte-position cursor"
```

---

### Task 6: `POST /fleet/say`

**Files:**
- Create: `fleet_say.go` (move the `fleetSay` stub out of `fleet.go`)
- Modify: `fleet.go` (`fleetDeps` seams listed below)
- Test: `fleet_say_test.go`

**Interfaces:**
- Consumes: `fleetAuth`, `discoverFleet`, `findFleet`, `s.resolveLivePID(pid, sid)` (fresh collect; returns the fresh list too), `s.fleet.capture` (nil → `capturePane`), `paneInputText`, `appendFleetLog`, `fleetNow`, `sendKeysMaxLen`.
- Produces:
  - `type fleetSayResult struct { OK bool; Queued bool; Error string; Code string }` JSON `ok, queued, error, code` (`queued`, `error`, `code` omitempty).
  - `func sanitizeFleetText(s string) (string, error)`, `func sameOrigin(r *http.Request) bool`.
  - `fleetDeps` seams (added in this task): `typeText func(target, text string) error` (nil → `tmux send-keys -t <target> -l <text>`), `pressEnter func(target string) error` (nil → `tmux send-keys -t <target> Enter`), `sleep func(time.Duration)` (nil → `time.Sleep`). State: `saidAt map[string]time.Time` (text → completion time, pruned to the last 10 s), `lastSayAt time.Time`.

Why typing and Enter are split (Codex review, finding 7): the pane check is a snapshot. A permission dialog can open between the capture and the Enter key, and Enter would then pick the dialog's highlighted "Yes". So the handler types the text, then captures again and presses Enter only if the input box is still visible and now holds text. This narrows the race to the few milliseconds between that second capture and Enter; it cannot close it, because nothing locks Claude Code's own state. The residual window is documented in CLAUDE.md (Task 10).

Order (each refusal returns at once):
1. `fleetAuth == none` → 401.
2. Auth by cookie → `Origin` must be present and its host (`url.Parse(origin).Host`) must equal `r.Host`; if `Origin` is absent, `Referer`'s host must equal `r.Host`; `null` or absent both → 403 `{"error":"origin mismatch","code":"origin"}`. Bearer auth skips this check (not a browser; no ambient credential).
3. Body: `http.MaxBytesReader(w, r.Body, 16<<10)`, `DisallowUnknownFields`, decode into a pointer to `struct{Text string; CardID *string}` (JSON `text`, `cardId`), then `dec.Token()` must return `io.EOF` (same strictness as `sessionIDPrecondition`). Fail → 400 `bad json`. `cardId` over 200 bytes → 400.
4. `sanitizeFleetText`: replace `\r\n`, `\r`, `\n`, `\t` with a space; any other rune < 0x20, or 0x7f, or invalid UTF-8 → 400 `bad text`; trim; empty or > `sendKeysMaxLen` bytes → 400 `bad text`.
5. Take `s.fleet.sayMu` for the rest of the handler (two devices never interleave keystrokes).
6. Prune `saidAt` entries older than 10 s. `now - lastSayAt < 1s` → `{code:"rate_limited", error:"too fast"}`. `saidAt[text]` within 10 s → `{code:"double_tap", error:"same text sent less than 10 s ago"}`. (Codex finding 3: a map, so A, B, A within 10 s refuses the second A.)
7. `discoverFleet()` not alive → `not_live` "FLEET is not running", or `ambiguous` "two sessions are named FLEET". Row with empty `SessionID` → `not_live` "FLEET has no session id" (an empty id would switch off `resolveLivePID`'s identity check).
8. `fresh, all, refusal := s.resolveLivePID(row.PID, row.SessionID)`; refusal → `{code: refusal.Code, error: refusal.Error}`.
9. Re-run discovery on the fresh list (Codex finding 2): `again, reason := findFleet(all)`; `reason != ""` or `again.PID != row.PID` or `again.SessionID != row.SessionID` → `{code:"fleet_changed", error:"FLEET changed. Try again."}`.
10. `fresh.Waiting()` → `{code:"dialog", error:"FLEET shows a dialog. Answer it in the terminal."}`.
11. `fresh.Tmux == ""` → `{code:"no_pane", error:"FLEET has no tmux pane"}`.
12. Capture; error → `no_pane` "cannot read the FLEET pane". `paneInputText` not found → `dialog` "FLEET input box is not visible"; found with text → `input_busy` "input box not empty".
13. `typeText(fresh.Tmux, text)`; error → `send_failed`.
14. Verify before Enter: up to 15 times, every 100 ms (`sleep`), capture and run `paneInputText`. Box found with non-empty text → landed, stop. Box not found → stop at once (a dialog). Capture error → keep polling. Not landed → `{code:"not_submitted", error:"Text was typed but not sent. FLEET changed state. Check the terminal."}` and append a log line `{type:"note", text:"web-say typed but not submitted", cardId}` (no user text in it). Do not compare the box text with the sent text: Claude Code may show a long typed text as `[Pasted text #1 +N lines]`. The box was empty at step 12, so text in it now is ours.
15. `pressEnter(fresh.Tmux)`; error → `send_failed`.
16. Stamp `lastSayAt = fleetNow()` and `saidAt[text] = fleetNow()` **after** Enter (Codex finding 3: a slow send must not shorten the next request's 1 s gap).
17. `appendFleetLog(fleetLogEntry{At: now.UTC().Format(time.RFC3339), Type: "web-say", Text: text, CardID: cardID})`; an error does not fail the response (the text is already sent) — print one stderr line without the text.
18. 200 `{ok:true, queued: fresh.Status == "busy"}`.

All refusals in steps 6–15 are HTTP 200 with `ok:false` (the `actionResult` envelope convention). Spec deviation: the spec names the `sendKeysFn` seam; this route uses its own `typeText` / `pressEnter` seams because it must capture between the two keystroke calls. `sendKeys` and `sendKeysFn` stay unchanged for `/sessions/{pid}/send-keys`.

- [ ] **Step 1: Write the failing tests**

```go
// fleet_say_test.go
package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	emptyBoxCapture = "out\n────────────────────\n❯ \n────────────────────\n status\n"
	typedBoxCapture = "out\n────────────────────\n❯ hi\n────────────────────\n status\n"
	dialogCapture   = "────────────────────\n Do you want to proceed?\n ❯ 1. Yes\n   2. No\n"
)

type sayRig struct {
	s        *server
	typed    []string
	enters   int
	now      time.Time
	captures []string // consumed in order; the last one repeats
}

func newSayRig(t *testing.T, sessions []Session) *sayRig {
	rig := &sayRig{now: time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)}
	rig.s = fleetTestServer(t, sessions)
	rig.s.fleet.now = func() time.Time { return rig.now }
	rig.s.fleet.sleep = func(time.Duration) {}
	rig.s.fleet.capture = func(string) (string, error) {
		// Before typing: empty box. After typing: the box holds text.
		if len(rig.captures) > 0 {
			c := rig.captures[0]
			if len(rig.captures) > 1 {
				rig.captures = rig.captures[1:]
			}
			return c, nil
		}
		if len(rig.typed) > rig.enters {
			return typedBoxCapture, nil
		}
		return emptyBoxCapture, nil
	}
	rig.s.fleet.typeText = func(_ string, text string) error { rig.typed = append(rig.typed, text); return nil }
	rig.s.fleet.pressEnter = func(string) error { rig.enters++; return nil }
	return rig
}

func fleetRow() Session {
	return Session{PID: 7, Name: "FLEET", SessionID: "sid", Status: "shell", Tmux: "fleet:0.0"}
}

func (rig *sayRig) post(t *testing.T, body string, mod func(*http.Request)) (int, fleetSayResult) {
	t.Helper()
	r := httptest.NewRequest("POST", "http://ws.ts.net:8765/fleet/say", strings.NewReader(body))
	r.AddCookie(&http.Cookie{Name: "cs_fleet", Value: "tok"})
	r.Header.Set("Origin", "http://ws.ts.net:8765")
	if mod != nil {
		mod(r)
	}
	w := httptest.NewRecorder()
	fleetMux(rig.s).ServeHTTP(w, r)
	var res fleetSayResult
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	return w.Code, res
}

func TestFleetSaySendsExactTextAndLogs(t *testing.T) {
	rig := newSayRig(t, []Session{fleetRow()})
	code, res := rig.post(t, `{"text":"Andy decides: DR-1 — yes (explicit).","cardId":"dr-1"}`, nil)
	if code != 200 || !res.OK || res.Queued {
		t.Fatalf("%d %+v", code, res)
	}
	if len(rig.typed) != 1 || rig.typed[0] != "Andy decides: DR-1 — yes (explicit)." || rig.enters != 1 {
		t.Fatalf("typed %q enters %d", rig.typed, rig.enters)
	}
	data, _ := os.ReadFile(filepath.Join(rig.s.fleet.dir, "log.jsonl"))
	if !strings.Contains(string(data), `"type":"web-say"`) || !strings.Contains(string(data), `"cardId":"dr-1"`) {
		t.Fatalf("log %q", data)
	}
}

func TestFleetSayQueuedWhenBusy(t *testing.T) {
	row := fleetRow()
	row.Status = "busy"
	rig := newSayRig(t, []Session{row})
	if _, res := rig.post(t, `{"text":"hi"}`, nil); !res.OK || !res.Queued {
		t.Fatalf("%+v", res)
	}
}

func TestFleetSayFlattensNewlinesAndRefusesControl(t *testing.T) {
	rig := newSayRig(t, []Session{fleetRow()})
	if _, res := rig.post(t, `{"text":"line one\r\nline two\nthree"}`, nil); !res.OK {
		t.Fatalf("%+v", res)
	}
	if rig.typed[0] != "line one line two three" {
		t.Fatalf("typed %q", rig.typed[0])
	}
	rig.now = rig.now.Add(time.Minute)
	if code, _ := rig.post(t, `{"text":"esc \u001b[2J"}`, nil); code != http.StatusBadRequest {
		t.Fatalf("ESC must be 400, got %d", code)
	}
	if code, _ := rig.post(t, `{"text":"   "}`, nil); code != http.StatusBadRequest {
		t.Fatalf("blank must be 400, got %d", code)
	}
	if code, _ := rig.post(t, `{"text":"`+strings.Repeat("a", 4097)+`"}`, nil); code != http.StatusBadRequest {
		t.Fatalf("4097 bytes must be 400, got %d", code)
	}
	if len(rig.typed) != 1 {
		t.Fatalf("nothing else may be typed: %q", rig.typed)
	}
}

func TestFleetSayStrictBody(t *testing.T) {
	rig := newSayRig(t, []Session{fleetRow()})
	for _, b := range []string{`{"text":"a","extra":1}`, `{"text":"a"} x`, `null`, `[]`} {
		if code, _ := rig.post(t, b, nil); code != http.StatusBadRequest {
			t.Errorf("%s: %d", b, code)
		}
	}
}

func TestFleetSayNotAliveAmbiguousAndNoSessionID(t *testing.T) {
	rig := newSayRig(t, nil)
	if _, res := rig.post(t, `{"text":"hi"}`, nil); res.OK || res.Code != "not_live" {
		t.Fatalf("%+v", res)
	}
	rig = newSayRig(t, []Session{fleetRow(), {PID: 8, Name: "FLEET", SessionID: "x", Tmux: "b:0.0"}})
	if _, res := rig.post(t, `{"text":"hi"}`, nil); res.OK || res.Code != "ambiguous" {
		t.Fatalf("%+v", res)
	}
	row := fleetRow()
	row.SessionID = ""
	rig = newSayRig(t, []Session{row})
	if _, res := rig.post(t, `{"text":"hi"}`, nil); res.OK || res.Code != "not_live" {
		t.Fatalf("%+v", res)
	}
	if len(rig.typed) != 0 {
		t.Fatal("nothing may be typed")
	}
}

// collectPhases returns the cached-discovery list on the first collect and
// the fresh list on every later one (resolveLivePID collects directly).
func collectPhases(first, fresh []Session) func() ([]Session, error) {
	calls := 0
	return func() ([]Session, error) {
		calls++
		if calls == 1 {
			return first, nil
		}
		return fresh, nil
	}
}

func TestFleetSayRefusesRecycledPid(t *testing.T) {
	rig := newSayRig(t, nil)
	rig.s.collect = collectPhases([]Session{fleetRow()},
		[]Session{{PID: 7, Name: "other", SessionID: "new", Tmux: "x:0.0"}})
	if _, res := rig.post(t, `{"text":"hi"}`, nil); res.OK || res.Code != codeSessionMismatch {
		t.Fatalf("%+v", res)
	}
	if len(rig.typed) != 0 {
		t.Fatal("nothing may be typed into a recycled pid")
	}
}

func TestFleetSayRefusesWhenFreshListDisagrees(t *testing.T) {
	second := Session{PID: 9, Name: "FLEET", SessionID: "other", Tmux: "y:0.0"}
	renamed := fleetRow()
	renamed.Name = "not fleet any more"
	for name, fresh := range map[string][]Session{
		"second FLEET appeared": {fleetRow(), second},
		"renamed":               {renamed},
	} {
		rig := newSayRig(t, nil)
		rig.s.collect = collectPhases([]Session{fleetRow()}, fresh)
		if _, res := rig.post(t, `{"text":"hi"}`, nil); res.OK || res.Code != "fleet_changed" {
			t.Errorf("%s: %+v", name, res)
		}
		if len(rig.typed) != 0 {
			t.Errorf("%s: typed %q", name, rig.typed)
		}
	}
}

func TestFleetSayRefusesDialog(t *testing.T) {
	row := fleetRow()
	row.Status, row.WaitingFor = "busy", "permission prompt"
	rig := newSayRig(t, []Session{row})
	if _, res := rig.post(t, `{"text":"hi"}`, nil); res.OK || res.Code != "dialog" {
		t.Fatalf("%+v", res)
	}
	rig = newSayRig(t, []Session{fleetRow()})
	rig.captures = []string{dialogCapture}
	if _, res := rig.post(t, `{"text":"hi"}`, nil); res.OK || res.Code != "dialog" {
		t.Fatalf("%+v", res)
	}
	if len(rig.typed) != 0 {
		t.Fatal("nothing may be typed into a dialog")
	}
}

func TestFleetSayNoEnterWhenDialogOpensAfterTyping(t *testing.T) {
	rig := newSayRig(t, []Session{fleetRow()})
	rig.captures = []string{emptyBoxCapture, dialogCapture}
	_, res := rig.post(t, `{"text":"hi"}`, nil)
	if res.OK || res.Code != "not_submitted" {
		t.Fatalf("%+v", res)
	}
	if len(rig.typed) != 1 || rig.enters != 0 {
		t.Fatalf("typed %q enters %d: Enter must not be pressed into a dialog", rig.typed, rig.enters)
	}
	data, _ := os.ReadFile(filepath.Join(rig.s.fleet.dir, "log.jsonl"))
	if !strings.Contains(string(data), "not submitted") || strings.Contains(string(data), `"text":"hi"`) {
		t.Fatalf("log %q", data)
	}
}

func TestFleetSayNoEnterWhenTextNeverLands(t *testing.T) {
	rig := newSayRig(t, []Session{fleetRow()})
	rig.captures = []string{emptyBoxCapture} // repeats: the box stays empty
	if _, res := rig.post(t, `{"text":"hi"}`, nil); res.Code != "not_submitted" || rig.enters != 0 {
		t.Fatalf("%+v enters %d", res, rig.enters)
	}
}

func TestFleetSayRefusesNonEmptyInput(t *testing.T) {
	rig := newSayRig(t, []Session{fleetRow()})
	rig.captures = []string{"────────────────────\n❯ half typed\n────────────────────\n"}
	if _, res := rig.post(t, `{"text":"hi"}`, nil); res.OK || res.Code != "input_busy" || res.Error != "input box not empty" {
		t.Fatalf("%+v", res)
	}
}

func TestFleetSaySendFailure(t *testing.T) {
	rig := newSayRig(t, []Session{fleetRow()})
	rig.s.fleet.typeText = func(string, string) error { return errors.New("tmux gone") }
	if _, res := rig.post(t, `{"text":"hi"}`, nil); res.OK || res.Code != "send_failed" {
		t.Fatalf("%+v", res)
	}
}

func TestFleetSayRateLimitAndDoubleTap(t *testing.T) {
	rig := newSayRig(t, []Session{fleetRow()})
	rig.post(t, `{"text":"A"}`, nil)
	rig.now = rig.now.Add(500 * time.Millisecond)
	if _, res := rig.post(t, `{"text":"B"}`, nil); res.Code != "rate_limited" {
		t.Fatalf("%+v", res)
	}
	rig.now = rig.now.Add(1500 * time.Millisecond) // t=2s
	if _, res := rig.post(t, `{"text":"B"}`, nil); !res.OK {
		t.Fatalf("B at 2s: %+v", res)
	}
	rig.now = rig.now.Add(2 * time.Second) // t=4s: A, B, A
	if _, res := rig.post(t, `{"text":"A"}`, nil); res.Code != "double_tap" {
		t.Fatalf("A again at 4s must be a double tap: %+v", res)
	}
	rig.now = rig.now.Add(7 * time.Second) // t=11s
	if _, res := rig.post(t, `{"text":"A"}`, nil); !res.OK {
		t.Fatalf("after 10 s the same text is allowed: %+v", res)
	}
}

func TestFleetSayStampsAtCompletion(t *testing.T) {
	rig := newSayRig(t, []Session{fleetRow()})
	rig.s.fleet.pressEnter = func(string) error {
		rig.enters++
		rig.now = rig.now.Add(3 * time.Second) // slow send
		return nil
	}
	rig.post(t, `{"text":"one"}`, nil)
	rig.now = rig.now.Add(500 * time.Millisecond)
	if _, res := rig.post(t, `{"text":"two"}`, nil); res.Code != "rate_limited" {
		t.Fatalf("0.5 s after a slow send finished must still be rate limited: %+v", res)
	}
}

func TestFleetSayOriginGuard(t *testing.T) {
	rig := newSayRig(t, []Session{fleetRow()})
	if code, _ := rig.post(t, `{"text":"hi"}`, func(r *http.Request) { r.Header.Set("Origin", "http://evil.example") }); code != http.StatusForbidden {
		t.Fatalf("wrong origin: %d", code)
	}
	if code, _ := rig.post(t, `{"text":"hi"}`, func(r *http.Request) { r.Header.Set("Origin", "null") }); code != http.StatusForbidden {
		t.Fatalf("null origin: %d", code)
	}
	if code, _ := rig.post(t, `{"text":"hi"}`, func(r *http.Request) { r.Header.Del("Origin") }); code != http.StatusForbidden {
		t.Fatalf("no origin, no referer: %d", code)
	}
	code, res := rig.post(t, `{"text":"hi"}`, func(r *http.Request) {
		r.Header.Del("Origin")
		r.Header.Set("Referer", "http://ws.ts.net:8765/fleet")
	})
	if code != 200 || !res.OK {
		t.Fatalf("matching referer: %d %+v", code, res)
	}
	if len(rig.typed) != 1 {
		t.Fatalf("typed %q", rig.typed)
	}
}

func TestFleetSayBearerSkipsOrigin(t *testing.T) {
	rig := newSayRig(t, []Session{fleetRow()})
	r := httptest.NewRequest("POST", "http://ws.ts.net:8765/fleet/say", strings.NewReader(`{"text":"hi"}`))
	r.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	fleetMux(rig.s).ServeHTTP(w, r)
	if w.Code != 200 || len(rig.typed) != 1 {
		t.Fatalf("%d %q", w.Code, rig.typed)
	}
}

func TestFleetSayUnauthorized(t *testing.T) {
	rig := newSayRig(t, []Session{fleetRow()})
	r := httptest.NewRequest("POST", "/fleet/say", strings.NewReader(`{"text":"hi"}`))
	w := httptest.NewRecorder()
	fleetMux(rig.s).ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized || len(rig.typed) != 0 {
		t.Fatalf("%d", w.Code)
	}
}
```

Seam note: `cachedSessions` (server.go:795) calls `collectLocal` once per cache fill, and `resolveLivePID` (server.go:1018) calls `collectLocal` directly, so `collectPhases` maps call 1 to discovery and every later call to the fresh resolve. If `cachedSessions` turns out to collect more than once per fill, make `collectPhases` switch on a flag set from inside the first `capture` call instead; keep the intent (discovery sees the first list, the fresh resolve sees the second).

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -run TestFleetSay ./...`
Expected: FAIL.

- [ ] **Step 3: Write the implementation**

Add to `fleetDeps` in `fleet.go` (after the `logMu` field from Task 2):

```go
	typeText   func(target, text string) error // nil → tmux send-keys -l
	pressEnter func(target string) error       // nil → tmux send-keys Enter
	sleep      func(time.Duration)             // nil → time.Sleep

	sayMu     sync.Mutex // one send at a time, so two devices never interleave keystrokes
	lastSayAt time.Time
	saidAt    map[string]time.Time // text → completion time, last 10 s only
```

```go
// fleet_say.go
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	fleetSayGap       = time.Second
	fleetSayDoubleTap = 10 * time.Second
	fleetLandPolls    = 15
	fleetLandEvery    = 100 * time.Millisecond
)

type fleetSayResult struct {
	OK     bool   `json:"ok"`
	Queued bool   `json:"queued,omitempty"`
	Error  string `json:"error,omitempty"`
	Code   string `json:"code,omitempty"`
}

// sanitizeFleetText makes page text safe to type as one prompt: a literal
// newline would press Enter mid-message, and other control bytes (ESC) could
// drive the Claude TUI, so newlines flatten to spaces (as scripts/send.sh
// does) and every other control character is refused.
func sanitizeFleetText(s string) (string, error) {
	if !utf8.ValidString(s) {
		return "", errors.New("bad text")
	}
	s = strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ", "\t", " ").Replace(s)
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return "", errors.New("bad text")
		}
	}
	s = strings.TrimSpace(s)
	if s == "" || len(s) > sendKeysMaxLen {
		return "", errors.New("bad text")
	}
	return s, nil
}

// sameOrigin is the CSRF guard for cookie-authenticated sends.
func sameOrigin(r *http.Request) bool {
	src := r.Header.Get("Origin")
	if src == "" {
		src = r.Header.Get("Referer")
	}
	if src == "" || src == "null" {
		return false
	}
	u, err := url.Parse(src)
	return err == nil && u.Host != "" && strings.EqualFold(u.Host, r.Host)
}

func decodeFleetSayBody(w http.ResponseWriter, r *http.Request) (text, cardID string, err error) {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	dec.DisallowUnknownFields()
	var body *struct {
		Text   string  `json:"text"`
		CardID *string `json:"cardId"`
	}
	if err := dec.Decode(&body); err != nil || body == nil {
		return "", "", errors.New("bad json")
	}
	if _, err := dec.Token(); err != io.EOF {
		return "", "", errors.New("bad json")
	}
	if body.CardID != nil {
		cardID = *body.CardID
	}
	return body.Text, cardID, nil
}

func tmuxTypeLiteral(target, text string) error {
	if target == "" {
		return errors.New("no tmux pane")
	}
	return exec.Command("tmux", "send-keys", "-t", target, "-l", text).Run()
}

func tmuxPressEnter(target string) error {
	if target == "" {
		return errors.New("no tmux pane")
	}
	return exec.Command("tmux", "send-keys", "-t", target, "Enter").Run()
}

func (s *server) fleetSay(w http.ResponseWriter, r *http.Request) {
	kind := s.fleetAuth(r)
	if kind == fleetAuthNone {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if kind == fleetAuthCookie && !sameOrigin(r) {
		writeJSON(w, http.StatusForbidden, fleetSayResult{Error: "origin mismatch", Code: "origin"})
		return
	}
	raw, cardID, err := decodeFleetSayBody(w, r)
	if err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if len(cardID) > 200 {
		http.Error(w, "bad cardId", http.StatusBadRequest)
		return
	}
	text, err := sanitizeFleetText(raw)
	if err != nil {
		http.Error(w, "bad text", http.StatusBadRequest)
		return
	}

	f := &s.fleet
	f.sayMu.Lock()
	defer f.sayMu.Unlock()
	refuse := func(code, msg string) { writeJSON(w, http.StatusOK, fleetSayResult{Error: msg, Code: code}) }

	now := s.fleetNow()
	for t, at := range f.saidAt {
		if now.Sub(at) >= fleetSayDoubleTap {
			delete(f.saidAt, t)
		}
	}
	if !f.lastSayAt.IsZero() && now.Sub(f.lastSayAt) < fleetSayGap {
		refuse("rate_limited", "too fast")
		return
	}
	if _, dup := f.saidAt[text]; dup {
		refuse("double_tap", "same text sent less than 10 s ago")
		return
	}

	row, fl := s.discoverFleet()
	if !fl.Alive {
		if fl.Reason == "ambiguous" {
			refuse("ambiguous", "two sessions are named FLEET")
		} else {
			refuse("not_live", "FLEET is not running")
		}
		return
	}
	if row.SessionID == "" {
		refuse("not_live", "FLEET has no session id")
		return
	}
	fresh, all, refusal := s.resolveLivePID(row.PID, row.SessionID)
	if refusal != nil {
		refuse(refusal.Code, refusal.Error)
		return
	}
	if again, reason := findFleet(all); reason != "" || again.PID != row.PID || again.SessionID != row.SessionID {
		refuse("fleet_changed", "FLEET changed. Try again.")
		return
	}
	if fresh.Waiting() {
		refuse("dialog", "FLEET shows a dialog. Answer it in the terminal.")
		return
	}
	if fresh.Tmux == "" {
		refuse("no_pane", "FLEET has no tmux pane")
		return
	}
	capture := f.capture
	if capture == nil {
		capture = capturePane
	}
	screen, err := capture(fresh.Tmux)
	if err != nil {
		refuse("no_pane", "cannot read the FLEET pane")
		return
	}
	if typed, found := paneInputText(screen); !found {
		refuse("dialog", "FLEET input box is not visible")
		return
	} else if typed != "" {
		refuse("input_busy", "input box not empty")
		return
	}

	typeText, pressEnter, sleep := f.typeText, f.pressEnter, f.sleep
	if typeText == nil {
		typeText = tmuxTypeLiteral
	}
	if pressEnter == nil {
		pressEnter = tmuxPressEnter
	}
	if sleep == nil {
		sleep = time.Sleep
	}
	if err := typeText(fresh.Tmux, text); err != nil {
		refuse("send_failed", "send text: "+err.Error())
		return
	}
	// The first capture is a snapshot: a dialog may have opened since, and
	// Enter would pick its highlighted option. Press Enter only once the box
	// is still there and holds what was just typed (it was empty before).
	landed := false
	for i := 0; i < fleetLandPolls && !landed; i++ {
		if i > 0 {
			sleep(fleetLandEvery)
		}
		screen, err := capture(fresh.Tmux)
		if err != nil {
			continue
		}
		typed, found := paneInputText(screen)
		if !found {
			break
		}
		landed = typed != ""
	}
	if !landed {
		_ = s.appendFleetLog(fleetLogEntry{At: now.UTC().Format(time.RFC3339), Type: "note",
			Text: "web-say typed but not submitted", CardID: cardID})
		refuse("not_submitted", "Text was typed but not sent. FLEET changed state. Check the terminal.")
		return
	}
	if err := pressEnter(fresh.Tmux); err != nil {
		refuse("send_failed", "send enter: "+err.Error())
		return
	}
	done := s.fleetNow()
	f.lastSayAt = done
	if f.saidAt == nil {
		f.saidAt = map[string]time.Time{}
	}
	f.saidAt[text] = done
	if err := s.appendFleetLog(fleetLogEntry{At: done.UTC().Format(time.RFC3339), Type: "web-say", Text: text, CardID: cardID}); err != nil {
		fmt.Fprintf(os.Stderr, "fleet: web-say log append failed: %v\n", err)
	}
	writeJSON(w, http.StatusOK, fleetSayResult{OK: true, Queued: fresh.Status == "busy"})
}
```

- [ ] **Step 4: Run tests**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add fleet_say.go fleet_say_test.go fleet.go
git commit -m "feat(fleet): type page answers into the FLEET pane with guards"
```

---

### Task 7: The page (`fleet.html`)

**Files:**
- Modify: `fleet.html` (replace the placeholder)
- Test: `fleet_test.go` (static checks on the embedded file)

This task is user-facing UI. Implement with an opus-tier agent. All UI copy in ASD-STE100 (short, active, simple words).

**Interfaces:**
- Consumes: `GET /fleet/state`, `GET /fleet/chat?since=&n=`, `GET /fleet/log?before=&n=`, `GET /fleet/usage`, `POST /fleet/say` (all same-origin, cookie sent automatically; use `fetch(url, {credentials: "same-origin"})`).
- `/fleet/usage` shape (`usageNumbersResponse`, usage_numbers.go:50-95, json tags verified): `{generatedAt, source, claude:[{name, account, active, fiveHour:{label, pct, resetsAt}, sevenDay:{…}, weeklyScoped:{…}, credits, fetchedAt, stale, expired, reason}], grok:[{account, windows:[{label, pct, resetsAt}], credits, plan, fetchedAt, stale}], codex:[same as grok]}`. Window objects can be absent. Header: Claude 5h and 7d `pct` of the `active` entry; Grok: the highest `pct` in the first entry's `windows`, labelled with that window's `label`. Add a Go test that serializes a real `usageNumbersResponse` and checks that `fleet.html` reads the keys `account`, `pct`, `windows` (static `strings.Contains` checks are enough).

Required behaviour (spec "Page" + "Decision flow"):
1. One file, inline `<style>` and one inline `<script>`. No external URL anywhere (no fonts, no CDN). Mobile first: one column under 900 px; at ≥ 900 px the card sections sit in two columns beside the chat.
2. Header: "FLEET", "state written N min ago", FLEET status (busy / idle / waiting / not running), quota chips (Claude 5h, Claude 7d, Grok), an "offline" dot when the last fetch failed.
3. Banner (top): red when `fleet.alive=false` ("FLEET is not running." or "Two sessions are named FLEET."); amber when `stale` ("FLEET state is N min old."); amber when `stateError`. Show all that apply.
4. Sections in order: "Needs your explicit yes" (red border), "Needs you" (amber), Chat, "Working", "Waiting", "Done today", "History".
5. Card (explicit-yes / needs-you): title line (ticket + title), "Lean: X" when `lean` is set, one button per option (`key · label`), a "Lean" button when `lean` matches an option key, a 💬 button. Tap the title to expand `summary`, option `line`s, `links`, `session`, `waitingOn`.
6. Confirm sheet (modal) shows the exact text to send. For `needs-you`: one "Send" button. For `explicit-yes`: the text in a red box, then a first button "I read this", which reveals a second, separate button "Yes, I approve this write" that sends. "Cancel" always closes. "Lean" sends `option.send + " (lean)"`.
7. After a successful send, the card shows "sent · waiting for FLEET" until a newer state either drops that card id or changes its `updatedAt`. Track in a `Map(cardId → updatedAt at send time)`.
8. Chat: newest at bottom; turns upsert by `uuid`; `reset:true` replaces the list. Each turn shows role ("Andy" for user, "FLEET" for assistant) and local time. A turn longer than 12 lines is folded with a "Show all" toggle. Input: `<textarea>` + Send. Enter inserts a newline; Ctrl/Cmd+Enter sends. 💬 on a card pre-fills `About <ticket or title>: ` and focuses the box. On a refusal, keep the text and show the server's `error` in a toast. Disable Send while `fleet.alive=false`.
9. Working / Waiting / Done today: compact rows (ticket, title, session, waitingOn). "Done today" = `kind == "done"` and `updatedAt` falls on today's date in Europe/Vilnius:
   ```js
   const vilniusDay = d => new Intl.DateTimeFormat("en-CA", {timeZone: "Europe/Vilnius", year: "numeric", month: "2-digit", day: "2-digit"}).format(d);
   const isToday = iso => { const d = new Date(iso); return !isNaN(d) && vilniusDay(d) === vilniusDay(new Date()); };
   ```
10. History: newest first, filter chips per `type` (merge, kill, spawn, ticket, decision, ruling, relay, web-say, note), "Load more" uses `next`. Each entry: time, type, ticket, text, link.
11. Polling: state every 5 s, chat every 3 s (`since=<pos from the last response>`; first call without `since`, `n=50`; when the response `sessionId` differs from the held one, clear the list and use `since=0`), usage every 60 s, history on load and after each successful send. Stop all timers on `visibilitychange` to hidden; on visible, fetch everything at once and restart. On a failed fetch, back off (double the interval, cap 60 s) and show the offline dot; reset on success. A 401 shows "Session expired. Run claude-sessions fleet-url on the host." and stops polling.
12. Safety: build every node with `document.createElement` + `textContent`. Never use `innerHTML`, `outerHTML`, `insertAdjacentHTML` or `document.write`. Links:
    ```js
    function safeLink(url, label) {
      let u;
      try { u = new URL(url); } catch { return document.createTextNode(label); }
      if (u.protocol !== "http:" && u.protocol !== "https:") return document.createTextNode(label);
      const a = document.createElement("a");
      a.href = u.href; a.textContent = label; a.target = "_blank"; a.rel = "noopener noreferrer";
      return a;
    }
    ```
13. POST: `fetch("/fleet/say", {method: "POST", credentials: "same-origin", headers: {"Content-Type": "application/json"}, body: JSON.stringify({text, cardId})})`. Treat `ok:false` and non-200 the same way (toast with `error` or the status text).
14. Render the page defensively: a card missing `options`, `title` or `kind` must not throw; unknown `kind` goes to "Working".

- [ ] **Step 1: Write the failing static test**

```go
func TestFleetPageIsSelfContainedAndSafe(t *testing.T) {
	page := string(fleetPageHTML)
	for _, bad := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "src=\"http", "href=\"http", "@import", "eval("} {
		if strings.Contains(page, bad) {
			t.Errorf("fleet.html must not contain %q", bad)
		}
	}
	for _, need := range []string{"/fleet/state", "/fleet/chat", "/fleet/log", "/fleet/usage", "/fleet/say",
		"Europe/Vilnius", "noopener noreferrer", "visibilitychange", "Yes, I approve this write",
		"since=", ".pct", ".account", ".windows"} {
		if !strings.Contains(page, need) {
			t.Errorf("fleet.html must contain %q", need)
		}
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test -run TestFleetPageIsSelfContainedAndSafe ./...`
Expected: FAIL (placeholder page).

- [ ] **Step 3: Write `fleet.html`** per the 14 points above.

- [ ] **Step 4: Run the static test and a render check**

Run: `go test -run TestFleetPage ./... && go build ./...`

Render check (local, no real FLEET): start the server on a free port with a temp HOME that holds a sample `~/.claude/fleet/state.json` (two cards: one `explicit-yes`, one `needs-you` with `lean`, one `working`, one `done` with today's `updatedAt`) and a `log.jsonl` with three lines, and a copy of the real `~/.config/claude-sessions/server-token` is **not** needed — write a test token into the temp HOME's `.config/claude-sessions/server-token`. Then:

```bash
CHROME=~/.cache/ms-playwright/chromium-1200/chrome-linux64/chrome
"$CHROME" --headless=new --disable-gpu --window-size=420,1600 \
  --screenshot="$SCRATCH/fleet-phone.png" "http://127.0.0.1:$PORT/fleet?t=$TESTTOKEN"
it2cat "$SCRATCH/fleet-phone.png"
```

Expected: header, banner "FLEET is not running.", two decision cards, empty chat, compact lists and history visible; no console errors (add `--enable-logging=stderr --v=0` and grep stderr for `Uncaught`). Fix any layout or script error. Also take a 1400 px wide screenshot.

- [ ] **Step 5: Commit**

```bash
git add fleet.html fleet_test.go
git commit -m "feat(fleet): add the FLEET web page"
```

---

### Task 8: `claude-sessions fleet-url`

**Files:**
- Create: `fleet_url.go`, `fleet_url_test.go`
- Modify: `main.go` (dispatch `case "fleet-url": os.Exit(cmdFleetURL(args[1:]))` and one usage line: `  fleet-url [--host H] [--port N]  print the one-time FLEET page login URL (contains the token)`)

**Interfaces:**
- Consumes: `readServerToken()` (server.go:2101), `tailscaleBinary()`, `tailscaleCLIEnv()`, `tailscaleIPv4()`, `defaultServerPort`.
- Produces: `func cmdFleetURL(args []string) int`; `func fleetLoginURL(host string, port int, token string) string`; seam `var fleetTailscaleHost = defaultFleetTailscaleHost` (`func() string`: MagicDNS name from `tailscale status --json` → `.Self.DNSName` with the trailing dot trimmed; else `tailscaleIPv4()`; else "").

Rules: `--host` overrides discovery; `--port` defaults to `defaultServerPort`; no host found → exit 1 with "no Tailscale address; pass --host"; no token → exit 1 with "no server token; start the server once" (never create one: `readServerToken`, not `loadOrCreateToken`). Output: the URL alone on stdout, newline-terminated. Stderr gets one hint line: "Open this URL once per device. It contains the server token; do not share it." The token appears nowhere else.

- [ ] **Step 1: Write the failing tests**

```go
// fleet_url_test.go
package main

import "testing"

func TestFleetLoginURL(t *testing.T) {
	got := fleetLoginURL("ws.example.ts.net", 8765, "a+b/c")
	if got != "http://ws.example.ts.net:8765/fleet?t=a%2Bb%2Fc" {
		t.Fatalf("got %q", got)
	}
	if got := fleetLoginURL("100.1.2.3", 9000, "tok"); got != "http://100.1.2.3:9000/fleet?t=tok" {
		t.Fatalf("got %q", got)
	}
}

func TestCmdFleetURLBadFlag(t *testing.T) {
	if code := cmdFleetURL([]string{"--port", "x"}); code != 2 {
		t.Fatalf("code %d", code)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test -run 'TestFleetLoginURL|TestCmdFleetURL' ./...`
Expected: FAIL.

- [ ] **Step 3: Implement**

```go
// fleet_url.go
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

var fleetTailscaleHost = defaultFleetTailscaleHost

func defaultFleetTailscaleHost() string {
	if bin := tailscaleBinary(); bin != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, "status", "--json")
		cmd.Env = tailscaleCLIEnv(os.Environ())
		if out, err := cmd.Output(); err == nil {
			var st struct {
				Self struct {
					DNSName string `json:"DNSName"`
				} `json:"Self"`
			}
			if json.Unmarshal(out, &st) == nil {
				if name := strings.TrimSuffix(st.Self.DNSName, "."); name != "" {
					return name
				}
			}
		}
	}
	return tailscaleIPv4()
}

func fleetLoginURL(host string, port int, token string) string {
	u := url.URL{Scheme: "http", Host: net.JoinHostPort(host, strconv.Itoa(port)), Path: "/fleet",
		RawQuery: url.Values{"t": {token}}.Encode()}
	return u.String()
}

func cmdFleetURL(args []string) int {
	fs := flag.NewFlagSet("fleet-url", flag.ContinueOnError)
	host := fs.String("host", "", "host name or address the browser uses")
	port := fs.Int("port", defaultServerPort, "server port")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	h := *host
	if h == "" {
		h = fleetTailscaleHost()
	}
	if h == "" {
		fmt.Fprintln(os.Stderr, "fleet-url: no Tailscale address; pass --host")
		return 1
	}
	tok, err := readServerToken()
	if err != nil {
		fmt.Fprintln(os.Stderr, "fleet-url: no server token; start the server once")
		return 1
	}
	fmt.Println(fleetLoginURL(h, *port, tok))
	fmt.Fprintln(os.Stderr, "Open this URL once per device. It contains the server token; do not share it.")
	return 0
}
```

Check `tailscaleBinary()`'s return contract (server.go ~2168) — if it returns "" when not found, the code above is right; if it returns a fallback name, keep the `exec` call and let it fail. Do not print `err` from `readServerToken` (it names the path, which is fine, but keep the message fixed).

- [ ] **Step 4: Run tests**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add fleet_url.go fleet_url_test.go main.go
git commit -m "feat(fleet): add fleet-url to print the one-time page login URL"
```

---

### Task 9: FLEET-side helpers and SKILL.md (outside this repo)

**Files (not in git; under `~/.claude/skills/fleet/`):**
- Create: `scripts/fleet-state.py`, `scripts/fleet-log.sh`
- Modify: `SKILL.md` (new section after "## State file")

**`scripts/fleet-state.py`** — usage: `python3 fleet-state.py <draft.json>` (or `-` for stdin). Validates, stamps, writes atomically.

Validation (exit 2 with one line per problem on stderr; the old file stays):
- top level object; `version == 1`; `cycleNote` string (≤ 300 chars); `cards` list.
- each card: `id` non-empty string matching `^[A-Za-z0-9._-]{1,100}$`, unique; `kind` in `explicit-yes, needs-you, working, waiting, done`; `title` non-empty string; `summary` string if present; `ticket`, `session`, `waitingOn`, `lean` string or null if present; `updatedAt` RFC 3339 string (add `now` if missing).
- `options`: required non-empty list for `explicit-yes` / `needs-you`, absent or empty otherwise. Each option: `key` non-empty string, unique in the card; `label` non-empty; `send` non-empty string ≤ 4096 bytes with no newline or control character (the server would flatten or refuse it); `line` string if present.
- `lean`, when set, equals one option key.
- `links`: list of `{label, url}` with `url` starting `http://` or `https://`.
- Stamps `writtenAt` = now UTC (`%Y-%m-%dT%H:%M:%SZ`), overwriting any draft value.

Write: `~/.claude/fleet/` (mkdir 0700) → `tempfile.NamedTemporaryFile(dir=that, delete=False)` → `json.dump(..., ensure_ascii=False, indent=1)` → `flush` + `os.fsync` → `os.chmod(0o600)` → `os.replace(tmp, state.json)`. Print `ok: N cards` on success.

**`scripts/fleet-log.sh`** — usage: `bash fleet-log.sh <type> <ticket|-> <text> [url]`. `type` must be one of `merge kill spawn ticket decision ruling relay note` (web-say is server-only). `-` means no ticket. Builds the line with `jq -nc --arg …` (never by string concatenation), adds `at` = `date -u +%Y-%m-%dT%H:%M:%SZ`, refuses a url that is not http(s), `mkdir -p -m 700 ~/.claude/fleet`, appends with `>>` (one `printf '%s\n'` call). Exit 2 on bad args.

**SKILL.md section** (STE100, short):

```markdown
## Keep the web page current

Andy reads FLEET on `/fleet` (claude-sessions server, Tailscale). The page shows your cards and history. Keep both current.

- **State:** at the end of every cycle and after every relay, write the cards. Make a JSON draft with `version: 1`, `cycleNote`, and `cards` (kinds `explicit-yes`, `needs-you`, `working`, `waiting`, `done`). Then run `python3 ~/.claude/skills/fleet/scripts/fleet-state.py <draft>`. It validates the draft and replaces `~/.claude/fleet/state.json` atomically. A bad draft does not replace the file; fix it and run again.
- **Card rules:** keep a card `id` stable across rewrites. Give `options` only to `explicit-yes` and `needs-you`. Each option `send` is the exact one-line text the page types to you, starting "Andy decides:". Set `lean` to one option key.
- **History:** append one line for each merge, kill, spawn, filed ticket, relayed decision and expert ruling: `bash ~/.claude/skills/fleet/scripts/fleet-log.sh <type> <ticket|-> "<text>" [url]`.
- **Page input:** text from the page arrives as a normal typed message from Andy. Apply the same rules, including the explicit-yes rule.
- **Login link:** `claude-sessions fleet-url` prints it. Never paste its output into a log, a ticket or Slack.
```

- [ ] **Step 1: Write a test script first** in the scratchpad (`$SCRATCH/test-fleet-helpers.sh`) that, with `HOME` pointed at a temp dir: (a) a valid draft → exit 0, `state.json` exists, mode 600, `writtenAt` set; (b) an invalid draft (lean not an option key; `send` with a newline; `javascript:` link; duplicate id) → exit 2, previous `state.json` unchanged byte for byte; (c) `fleet-log.sh merge DR-1 "text with \"quotes\"" https://x` → one valid JSON line, `jq -e .text` round-trips the quotes; (d) `fleet-log.sh bogus - x` → exit 2; (e) `fleet-log.sh note - x javascript:alert(1)` → exit 2.
- [ ] **Step 2: Run it; expect failures (scripts missing).**
- [ ] **Step 3: Write both scripts and the SKILL.md section.**
- [ ] **Step 4: Run the test script; all five cases pass.** Also run `python3 -m py_compile` on the script and `bash -n` on the shell script.
- [ ] **Step 5: No git commit** (outside the repo). Record the result in the final report.

---

### Task 10: Docs

**Files:**
- Modify: `CLAUDE.md` (new `### FLEET web page` section before `### Usage polling`), `README.md` (one line in the command list for `fleet-url` and one line for the `/fleet` route if the README lists routes).

CLAUDE.md section content (keep it to what a later change needs to know):
- Routes and files; FLEET-side writers (`fleet-state.py`, `fleet-log.sh`).
- Auth: bearer or `cs_fleet` cookie, cookie only on `/fleet*`; `?t=` login answers an HTML hop, not a 303, because a cross-site navigation would not carry the new Strict cookie on the redirect.
- `Referrer-Policy: same-origin` on the page, not `no-referrer`, because `no-referrer` makes browsers send `Origin: null` on POST and the CSRF guard would refuse every send.
- `/fleet/usage` exists because the cookie path is `/fleet`.
- Say guards, in order, and why: newline flatten (a literal newline is Enter), `WaitingFor` + pane input-box check (typed Enter would answer a permission dialog), input box not empty (another device or Andy mid-typing), rate limit and double tap, fresh `resolveLivePID` (recycled pid).
- Input-box parser: the box is the region between the last two `─` rules starting with `❯`; dim text (SGR 2) is Claude Code's placeholder. If Claude Code changes its prompt rendering, `paneInputText` refuses every send (fails closed) — refresh `testdata/fleet/*.ansi` with `tmux capture-pane -p -e`.
- Say presses Enter only after a second capture shows the input box still there and holding text. Residual race, accepted: a dialog that opens in the milliseconds between that capture and Enter still takes the Enter. Nothing can lock Claude Code's own state; the `WaitingFor` check and both captures narrow the window.
- Say identity: discovery from the cached list, then `resolveLivePID` (fresh), then `findFleet` again on the fresh list; an empty session id is refused (it would switch off `resolveLivePID`'s check).
- Chat cursor is a transcript byte position; the server returns changed turns and the page upserts by uuid (assistant re-emissions can update an earlier turn).
- Log cursor is an absolute byte offset, not a timestamp or a line index (both drift or collide).

- [ ] **Step 1: Write both edits.**
- [ ] **Step 2: Run** `go build ./... && go vet ./... && go test ./...` (no code change; confirms the tree).
- [ ] **Step 3: Commit**

```bash
git add CLAUDE.md README.md
git commit -m "docs: describe the FLEET web page"
```

---

## Finish (after all tasks)

1. Independent verifier run: `go build ./... && go vet ./... && go test ./...`.
2. Opus `code-reviewer` pass on `git diff main...HEAD`; triage and fix findings that hold.
3. Repo CLAUDE.md finish flow: merge `feat/fleet-page` into `main` locally, `make install`, `git push origin main`, deploy to agent-workstation (`git pull --ff-only && make install && PATH="$HOME/.local/bin:$PATH" claude-sessions service restart`), remove this worktree and branch, remove `.claude/worktrees/fleet-page-spec` and branch `docs/fleet-page-spec`.
4. Manual check over Tailscale with curl (never print the token; read it into a shell variable from `claude-sessions fleet-url` output):
   - `GET /fleet?t=…` → 200 + `Set-Cookie: cs_fleet`.
   - With the cookie: `/fleet`, `/fleet/state`, `/fleet/chat`, `/fleet/log`, `/fleet/usage` → 200.
   - `GET /sessions` with only the cookie → 401.
   - `POST /fleet/say` with the cookie and `Origin: http://evil.example` → 403.
   - `POST /fleet/say` refusal with no FLEET match: only if FLEET is not running at that moment; otherwise verify the no-match path through the unit test and say so. **Never POST real text into the FLEET pane.** A safe live probe: `POST` with a body that fails validation (`{"text":"\u001b"}` → 400) proves the route is live without typing anything.
