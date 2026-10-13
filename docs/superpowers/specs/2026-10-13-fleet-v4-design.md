# FLEET page v4 — design spec

Repo: claude-sessions (Go, package main, stdlib only). Read CLAUDE.md section
"FLEET web page" first, plus `docs/superpowers/specs/2026-10-13-fleet-page-design.md`.
The GitHub repo is PUBLIC: never commit real pane captures, FLEET state, chat
text or tokens. Fixtures must be invented text.

Work is split in two branches that are merged at the end:
- **GO** (`feat/fleet-v4-go`): every `.go` file, new static assets
  (`fleet_sw.js`, icons), CSP, routes, tests, CLAUDE.md.
- **UI** (`feat/fleet-v4-ui`): `fleet.html` only.
Each side codes against the contracts below and must not edit the other
side's files.

## Contracts (both sides)

### C1. `fleet` object in `GET /fleet/state` (existing `fleetSession`, extended)
```
fleet: {
  alive: bool, sessionId: string, pid: int, status: "idle"|"busy"|"waiting",
  reason?: string,              // unchanged human text
  code?: "not_live"|"ambiguous"|"list_failed",   // NEW, empty when alive
  canStart: bool,               // NEW: code=="not_live" && no start in flight && rate limit free
  hasLast: bool,                // NEW: fleet-last.json holds a usable session
  starting?: { at: RFC3339, mode: "resume"|"fresh" }  // NEW: a web start in the last 90 s and FLEET not alive yet
}
```

### C2. `POST /fleet/start`
Auth: bearer, or cookie + `sameOrigin` (same rule as `/fleet/say`). Body empty
or `{}` (ignore content; cap 4 KiB). Responses (JSON, always `{ok, ...}`):
- 200 `{ok:true, mode:"resume"|"fresh", tmux:"<name>"}`
- 409 `{ok:false, code:"already_running"|"ambiguous", error}`
- 429 `{ok:false, code:"rate_limited", retryAfter:<seconds int>, error}`
- 500 `{ok:false, code:"start_failed", error}` (error text must not contain paths under $HOME beyond what is already public; a short message is fine)

### C3. `GET /fleet/chat` read-only fallback
When discovery code is `not_live` and fleet-last.json names a session whose
transcript exists, answer exactly as for a live FLEET (same `turns`, `pos`,
`reset`, `sessionId` = last id, `since` cursor semantics) plus `readOnly:true`
and `reason`. When alive, `readOnly` is absent/false. `ambiguous` and other
codes keep today's empty answer (no fallback: we do not know which one).

### C4. PWA assets (served by GO, referenced by UI)
- `GET /fleet/manifest.webmanifest` → `application/manifest+json`
- `GET /fleet/sw.js` → `text/javascript`, header `Service-Worker-Allowed: /fleet`
- `GET /fleet/icon-192.png`, `/fleet/icon-512.png`, `/fleet/icon-maskable-512.png`,
  `/fleet/apple-touch-icon.png` (180x180) → `image/png`
- These need NO auth (static, no secrets), `Cache-Control: public, max-age=86400`
  except sw.js which is `no-cache`.
- UI registers: `navigator.serviceWorker.register("/fleet/sw.js", {scope: "/fleet"})`
  inside try/catch, only when `"serviceWorker" in navigator`.
- Page CSP gains `manifest-src 'self'; worker-src 'self'`.

### C5. Usage (no backend change)
`GET /fleet/usage` already returns `claude[]` with `active`, `fiveHour`,
`sevenDay`, and `weeklyScoped: {label, pct, resetsAt}` (label is "Fable").
Read the shape from `usage_numbers.go` (`usageWindowNumbers`).

## GO side requirements

G1. **fleet-last.json** at `~/.config/claude-sessions/fleet-last.json` (path is a
`fleetDeps` seam, e.g. `lastPath string`; empty = default). Content:
`{"sessionId","cwd","tmux","seenAt"}`. Written atomically (temp + rename, 0600,
dir 0700) whenever discovery finds an alive FLEET **and** sessionId/cwd/tmux
differ from the last written values (keep the last written copy in memory; no
write per tick). `seenAt` is only refreshed on a write. Read errors = no last
session. Validate the session id with the existing resume id format check
before using it anywhere.

G2. **Discovery code**: add `Code` to `fleetSession` (`json:"code,omitempty"`):
"not running" → `not_live`, "ambiguous" → `ambiguous`, "session list failed" →
`list_failed`. Keep `reason` text unchanged (existing tests). Add `canStart`,
`hasLast`, `starting` per C1.

G3. **POST /fleet/start** handler (new file `fleet_start.go`), order:
1. auth (bearer, or cookie+sameOrigin else 403 like say).
2. rate limit: if the previous start attempt is < 60 s ago → 429 with
   retryAfter (ceil seconds). Hold a mutex across the whole handler so two
   taps cannot both pass.
3. FRESH discovery (call `s.collectLocal()`/the uncached collector, not the
   cached list) → `findFleet`. alive → 409 already_running; ambiguous → 409
   ambiguous; list failure → 500 start_failed.
4. stamp the attempt time now (a failed spawn still counts against the 60 s).
5. choose mode: fleet-last.json valid, its cwd is an existing directory, and
   `findTranscript(home, id)` finds the transcript → **resume**; else **fresh**.
6. command prefix = the "Opus" command preset (`findCommandPreset` over the
   loaded config presets, same loader the `new` command uses), fallback
   `claude --model=opus` if missing.
   - resume: `<prefix> --resume <id> -n FLEET`, cwd = last.cwd.
   - fresh: `<prefix> -n FLEET '/fleet'` (single-quote the prompt with the
     repo's existing shell-quote helper if there is one), cwd =
     `fleetDefaultCwd` = `$HOME/Developer/trecs-brain` (deps seam).
   - Verified live: `claude -n NAME` writes `nameSource:"user"`, so
     `findFleet` will match the new session.
7. spawn via `SpawnNew(cwd, "FLEET", command)` behind a deps seam
   (`spawn func(cwd, display, command string) (string, error)`).
   The resume path must also refuse if the id is somehow live (it is not,
   since step 3 found no FLEET, but keep `resumeCommon`'s live-id check
   semantics if you reuse it; using SpawnNew directly is fine).
8. append a log line to `~/.claude/fleet/log.jsonl` the same way `web-say`
   lines are appended: `{"ts":..., "kind":"web-start", "mode":..., "sessionId":..., "tmux":...}`
   (match the existing web-say line's field names/style).
9. record `starting = {at, mode}`; clear it when discovery sees FLEET alive or
   after 90 s. Invalidate the session cache if there is one so the next state
   poll re-collects.
10. 200 response.

G4. **Chat fallback** per C3, in `fleet_chat.go`.

G5. **PWA routes** per C4. Icons: generate once with a throwaway Go program
(scratch dir, not committed) using `image`/`image/png`/`image/draw` only: a
rounded dark square (#111827 bg) with a bold geometric white "F" built from
rectangles; maskable variant keeps the glyph inside the 80% safe zone with full
bleed bg. Commit the PNGs under `fleet_assets/` and `//go:embed` them. Keep
each PNG small (< 20 KB).
Manifest: `name:"FLEET"`, `short_name:"FLEET"`, `id:"/fleet"`,
`start_url:"/fleet"`, `scope:"/fleet"`, `display:"standalone"`,
`background_color` and `theme_color` matching the page's light `--bg`
(read it from fleet.html `:root`), icons (192, 512 any, 512 maskable).
`fleet_sw.js` (embedded file):
- install: cache the app shell `["/fleet/manifest.webmanifest", icons]` only.
  Do NOT precache `/fleet` (its response depends on auth).
- fetch: only handle GET. Never touch `/fleet/state`, `/fleet/chat`,
  `/fleet/log`, `/fleet/usage`, `/fleet/say`, `/fleet/start`, `/fleet/login*`,
  any request with a `c=` query, or anything outside `/fleet`. Navigation to
  `/fleet` (mode "navigate"): network first; on a 200 HTML response that is
  the app (not the hop or login page — e.g. check a response header the GO
  side sets on the app page only, `X-Fleet-App: 1`) put it in the cache; on
  network failure serve the cached shell. Static assets: cache first.
- activate: delete old cache versions; `clients.claim()`.
- version string in the cache name; bump it when the file changes.

G6. **Login inside an installed app.** iOS home-screen apps keep their own
cookie jar, so a phone may open the app with no cookie. Today `/fleet?hop=1`
with no auth is a bare 401. Change it to a 401 HTML page (no script, its own
CSP like the login page, `form-action 'self'`, `Referrer-Policy: same-origin`)
titled "Log in to FLEET" with:
- short steps: "On the workstation run `claude-sessions fleet-url`. Paste the
  link or the code here." (STE-style plain words)
- one text input `c` (autocomplete off, autocapitalize off) and a "Log in"
  button that POSTs to `/fleet/login`.
`POST /fleet/login` must accept either a bare code or a full pasted link: if
the value contains `c=`, parse it as a URL/query and take `c`. Trim spaces.
All existing login guards stay (sameOrigin first, 4 KiB cap, one-time use).
The cookie is already persistent (Max-Age 30 days) — keep it.

G7. Tests (table-driven where natural, reuse `fleetTestServer` etc.):
- state `code`/`canStart`/`hasLast` for alive, not_live, ambiguous.
- start: alive → 409; ambiguous → 409; not_live + last → resume command
  contains `--resume <id>` and `-n FLEET`, cwd = last cwd; not_live + no last
  → fresh command contains `-n FLEET` and `/fleet`, cwd = default; last whose
  cwd is missing → fresh; second start within 60 s → 429 with retryAfter;
  failed spawn still rate limited; cookie without same origin → 403; log line
  `web-start` written; preset fallback.
- fleet-last.json written on discovery, not rewritten when unchanged,
  rewritten when id changes; bad id in file ignored.
- chat fallback: not_live + last → turns + readOnly:true; ambiguous → empty, no
  readOnly; alive → no readOnly.
- PWA routes: no auth needed, content types, `Service-Worker-Allowed`,
  manifest JSON parses with required fields; app page has `X-Fleet-App: 1`,
  hop/login pages do not; CSP contains manifest-src and worker-src.
- login: pasted full link works; bare code works; hop=1 without auth → 401
  HTML containing the form.
No test may spawn tmux or claude (use the seams).

G8. CLAUDE.md "FLEET web page": add short paragraphs for Start FLEET
(guards and why), fleet-last.json, read-only chat, PWA + service worker
rules (what is never cached and why), login inside the installed app.
Match the section's existing style.

G9. `go build ./... && go vet ./... && go test ./...` green. Commit in logical
commits, each ending with
`Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.

## UI side requirements (fleet.html only)

U1. **PWA hooks**: `<link rel="manifest" href="/fleet/manifest.webmanifest">`,
`<link rel="apple-touch-icon" href="/fleet/apple-touch-icon.png">`,
`<meta name="apple-mobile-web-app-capable" content="yes">`,
`<meta name="mobile-web-app-capable" content="yes">`,
`<meta name="apple-mobile-web-app-title" content="FLEET">`,
`<meta name="apple-mobile-web-app-status-bar-style" content="default">`,
viewport with `viewport-fit=cover`. SW registration per C4.

U2. **Fable usage chip** in the header from the active Claude account's
`weeklyScoped` (label from the data, e.g. "Fable wk"). Colors for every usage
chip: green < 60 %, amber 60–84 %, red ≥ 85 %. Title tooltip shows reset time.
Missing data → chip hidden (not "0 %").

U3. **Mobile views** (≤ 760 px wide): four views with a fixed bottom nav:
**Decide** (needs-you + yes cards), **Chat**, **Fleet** (working / waiting /
done today), **History** (log). Each nav item = glyph + label, ≥ 44 px tall
targets, a count badge on Decide when cards need you. Selected view in
`location.hash` (`#decide` etc.) so reload keeps it; default Decide when it
has cards, else Chat. Safe-area insets (`env(safe-area-inset-*)`) on header,
nav and chat input. No horizontal scroll at 320–430 px. In Chat the input is
sticky above the nav and stays above the on-screen keyboard (use
`visualViewport` resize to adjust bottom offset where needed). A refresh
control (button in the header) that calls `runNow` for all pollers; optional
pull-to-refresh is not required. Desktop (> 760 px) keeps today's columns;
the nav is hidden there.

U4. **Start FLEET**: when `fleet.code == "not_live"`, show a banner
"FLEET is not running" with a "Start FLEET" button, enabled only when
`fleet.canStart`. Tap → a confirm sheet (bottom sheet on mobile, dialog on
desktop): text says "Resume the last FLEET session" when `hasLast`, else
"Start a new FLEET session in trecs-brain"; buttons Cancel / Start. Start →
`POST /fleet/start` (JSON `{}`, same headers as say). While `fleet.starting`
is set or the POST is in flight, show progress ("Starting FLEET…" with a
spinner) and poll state fast (every 2 s) until `fleet.alive`, then a toast
"FLEET is running". Map 409/429/500 codes to short messages (429 shows the
seconds). `ambiguous` shows no start button, only the existing reason text.

U5. **Read-only chat**: when the chat answer has `readOnly:true`, show the
turns greyed (reduced opacity), a notice "FLEET is not running" above a
disabled input, and do not allow send. When FLEET returns, the normal chat
resumes (handle the session id change: reset turns when `sessionId` differs).

U6. Keep all v3 behavior: renderRich safe markdown, cursors, polling backoff,
CSRF headers. Do not use innerHTML with data (text nodes only, as today).
Light and dark themes both work for every new element.

U7. Verification: there is no JS test harness, and you must not run the real
server against the live FLEET. Use Playwright with a static mock: in the
scratch dir, write a tiny node HTTP server that serves `fleet.html` at
`/fleet` and mock JSON for every `/fleet/*` route per the contracts (alive,
not_live with/without last, starting, readOnly chat, 3 cards with long
multi-line summaries and 3 options, 50+ chat turns, 100 log lines, usage with
weeklyScoped). Playwright + chromium:
`~/.cache/ms-playwright/chromium-1200/chrome-linux64/chrome`, node_modules
from `~/Developer/lauko-darzelis/node_modules` (set NODE_PATH). Screenshot
390x844 and 1440x900, light and dark, each view, and check
`document.documentElement.scrollWidth <= innerWidth` on mobile. Keep the mock
server and scripts in the scratch dir (not committed) and report their paths.
Commit fleet.html changes with the Co-Authored-By line above.
