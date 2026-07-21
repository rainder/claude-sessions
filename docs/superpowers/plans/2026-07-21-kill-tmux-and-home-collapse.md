# Tmux-Aware Kill and True-Home Collapse Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `k` reliably kill the selected session's entire tmux session and render local/remote working directories under each machine's true home with `~`.

**Architecture:** `KillSession` will consume trusted `Session` metadata instead of re-discovering tmux membership after confirmation. Internal dependency functions make process/tmux behavior testable without signaling real processes. Remote handlers inject collection/termination behavior through `server` fields so tests prove that server-collected metadata is used. `Session.Home` travels through existing JSON and render derivation reads that per-row value.

**Tech Stack:** Go 1.26.3, standard library (`net/http`, `net/http/httptest`, `os/exec`, `syscall`, `time`, `encoding/json`), existing `golang.org/x/sys` and `golang.org/x/term`; no new dependencies.

## Global Constraints

- Whole tmux session must terminate when selected `Session.Tmux` is non-empty.
- Known tmux failure must return an error; never fall back to PID-only termination.
- Remote client continues sending PID only; server derives trusted `Session.Tmux` itself.
- Non-tmux behavior remains SIGTERM, five one-second checks, then SIGKILL.
- Home collapse uses `os.UserHomeDir()` from each machine, never a hardcoded path or client home for remote rows.
- Exact home and children collapse; false prefixes and missing home remain unchanged.
- JSON changes remain additive and compatible with older servers.
- Preserve macOS/Linux support and existing dependency set.
- Follow TDD and commit after each independently green task.

---

## File Structure

- `migrate.go`, new `migrate_test.go`: session-based kill contract, parser, testable process/tmux dependencies.
- `actions.go`, `commands.go`, `preview.go`: local and scriptable callers preserve the resolved tmux target.
- `server.go`, new `server_test.go`: remote endpoint resolves PID against server-collected sessions.
- `session.go`, `session_test.go`: carry true home in session metadata and JSON.
- `render.go`, `render_test.go`: collapse each row's own home safely in every view.
- `docs/superpowers/plans/2026-07-21-kill-tmux-and-home-collapse.md`: repository copy of this approved plan.

---

### Task 1: Persist Approved Implementation Plan

**Files:**
- Create: `docs/superpowers/plans/2026-07-21-kill-tmux-and-home-collapse.md`

**Interfaces:** None.

- [ ] **Step 1: Copy this approved plan verbatim into the repository path**

- [ ] **Step 2: Check plan formatting**

Run: `git diff --check -- docs/superpowers/plans/2026-07-21-kill-tmux-and-home-collapse.md`

Expected: no output.

- [ ] **Step 3: Commit**

```bash
git add docs/superpowers/plans/2026-07-21-kill-tmux-and-home-collapse.md
git commit -m "docs: add tmux kill implementation plan" -m "Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 2: Make Trusted Session Metadata Drive Every Kill Path

**Files:**
- Modify: `migrate.go:103-129`
- Create: `migrate_test.go`
- Modify: `actions.go:73-101`
- Modify: `commands.go:14-41`
- Modify: `preview.go:162-172`
- Modify: `server.go:28-58,90-109`
- Create: `server_test.go`

**Interfaces:**
- Produce: `tmuxSessionName(tmux string) (string, error)`
- Change: `KillSession(s Session) error`
- Produce internally: `killSessionWith(s Session, deps killDeps) error`
- Produce: `tmuxLocForPID(pid int) string`
- Produce internally: `(*server).collectLocal() ([]Session, error)` and `(*server).terminateSession(Session) error`

- [ ] **Step 1: Write failing parser and kill-routing tests**

Create `migrate_test.go`. Cover:

```go
func TestTmuxSessionName(t *testing.T) {
	cases := []struct {
		loc     string
		want    string
		wantErr bool
	}{
		{"work:1.0", "work", false},
		{"work:3.7", "work", false},
		{":1.0", "", true},
		{"work", "", true},
		{"", "", true},
	}
	for _, tc := range cases {
		got, err := tmuxSessionName(tc.loc)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("tmuxSessionName(%q) = (%q, %v), want (%q, error=%v)", tc.loc, got, err, tc.want, tc.wantErr)
		}
	}
}
```

Define fake `killDeps` values in tests and add:

- `TestKillSessionWithTmuxKillsWholeSession`: records target `work`; records no signals.
- `TestKillSessionWithTmuxFailureDoesNotSignalPID`: fake tmux call returns `errors.New("boom")`; records no signals.
- `TestKillSessionWithMalformedTmuxDoesNothing`: malformed metadata calls neither tmux nor signal functions.
- `TestKillSessionWithoutTmuxSignalsPID`: fake `alive` returns false; records only SIGTERM.
- `TestKillSessionWithoutTmuxEscalates`: fake `alive` always true; records SIGTERM then SIGKILL.

Use this dependency shape so no test signals a real PID or sleeps:

```go
type killDeps struct {
	killTmux func(string) error
	signal   func(int, syscall.Signal) error
	alive    func(int) bool
	sleep    func(time.Duration)
}
```

- [ ] **Step 2: Run kill tests and confirm RED**

Run: `go test ./... -run 'TestTmuxSessionName|TestKillSession' -count=1`

Expected: build failures for undefined parser/dependency-aware kill functions.

- [ ] **Step 3: Implement parser and session-based kill primitive**

In `migrate.go`:

```go
var defaultKillDeps = killDeps{
	killTmux: func(name string) error {
		return exec.Command("tmux", "kill-session", "-t", name).Run()
	},
	signal: syscall.Kill,
	alive:  pidAlive,
	sleep:  time.Sleep,
}

func tmuxSessionName(tmux string) (string, error) {
	i := strings.IndexByte(tmux, ':')
	if i <= 0 {
		return "", fmt.Errorf("malformed tmux metadata %q", tmux)
	}
	return tmux[:i], nil
}

func KillSession(s Session) error {
	return killSessionWith(s, defaultKillDeps)
}

func killSessionWith(s Session, deps killDeps) error {
	if s.Tmux != "" {
		name, err := tmuxSessionName(s.Tmux)
		if err != nil {
			return err
		}
		if err := deps.killTmux(name); err != nil {
			return fmt.Errorf("tmux kill-session %s: %w", name, err)
		}
		return nil
	}
	if err := deps.signal(s.PID, syscall.SIGTERM); err != nil {
		return fmt.Errorf("SIGTERM %d: %w", s.PID, err)
	}
	for i := 0; i < 5; i++ {
		deps.sleep(time.Second)
		if !deps.alive(s.PID) {
			return nil
		}
	}
	_ = deps.signal(s.PID, syscall.SIGKILL)
	deps.sleep(time.Second)
	return nil
}
```

Delete kill-time `tmuxPaneMap`/`ppidMap`/`walkTmuxPane` re-discovery.

- [ ] **Step 4: Add reusable full tmux-location lookup**

In `preview.go`:

```go
func tmuxLocForPID(pid int) string {
	panes, _ := tmuxPaneMap()
	ppid, _ := ppidMap()
	return walkTmuxPane(pid, panes, ppid)
}

func tmuxSessionForPID(pid int) string {
	loc := tmuxLocForPID(pid)
	if loc == "" {
		return ""
	}
	name, err := tmuxSessionName(loc)
	if err != nil {
		return ""
	}
	return name
}
```

- [ ] **Step 5: Update local and scriptable callers without intermediate PID-only behavior**

In `actKill`, parse non-empty selected metadata before confirmation. If malformed, print `kill failed: <error>`, pause, and return without killing. Use parsed name in tmux prompt and call `KillSession(*s)`.

In `cmdKill`, retain `sess` from `readSessionByPID`, set `sess.Tmux = tmuxLocForPID(pid)`, use tmux-aware confirmation/result text when non-empty, and call `KillSession(sess)`.

- [ ] **Step 6: Write failing remote handler tests**

Create `server_test.go` using `httptest.NewRecorder`, `httptest.NewRequest`, `req.SetPathValue("pid", "55")`, and bearer auth. Tests:

- `TestKillHandlerUsesServerDerivedSession`: injected collector returns `Session{PID:55, Tmux:"remote-work:2.1"}`; injected terminator records that exact session; response has `OK:true`.
- `TestKillHandlerUnknownPIDDoesNotTerminate`: empty collection returns existing not-live error.
- `TestKillHandlerCollectionErrorDoesNotTerminate`: collection error is returned in `actionResult.Error`.
- `TestKillHandlerUnauthorized`: missing bearer token returns HTTP 401.

- [ ] **Step 7: Add injectable server production fallbacks and collected-session resolution**

Extend `server`:

```go
type server struct {
	token     string
	host      string
	collect   func() ([]Session, error)
	terminate func(Session) error
}

func (s *server) collectLocal() ([]Session, error) {
	if s.collect != nil {
		return s.collect()
	}
	return CollectLocal()
}

func (s *server) terminateSession(target Session) error {
	if s.terminate != nil {
		return s.terminate(target)
	}
	return KillSession(target)
}
```

Use `s.collectLocal()` in both `sessions` and `kill`. In `kill`, find requested PID in collected rows and pass that full row to `s.terminateSession`. Do not decode or accept tmux metadata from request body.

- [ ] **Step 8: Run focused and full tests**

Run:

```bash
gofmt -w migrate.go migrate_test.go actions.go commands.go preview.go server.go server_test.go
go test ./... -run 'TestTmuxSessionName|TestKillSession|TestKillHandler' -count=1
go test ./...
```

Expected: all tests pass; `grep -R "KillSession(pid" -n -- *.go` returns no matches.

- [ ] **Step 9: Commit**

```bash
git add migrate.go migrate_test.go actions.go commands.go preview.go server.go server_test.go
git commit -m "fix: make trusted session metadata drive kills" -m "Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 3: Carry True Home Through Session Collection and JSON

**Files:**
- Modify: `session.go:15-49,119-164`
- Modify: `session_test.go`

**Interfaces:**
- Produce: `Session.Home string` with `json:"home,omitempty"`
- Consume: existing `home` from `os.UserHomeDir()` in `CollectLocal`

- [ ] **Step 1: Write failing collection and JSON compatibility tests**

Add `TestCollectLocalSetsHome`:

```go
home := t.TempDir()
t.Setenv("HOME", home)
dir := filepath.Join(home, ".claude", "sessions")
if err := os.MkdirAll(dir, 0o755); err != nil { t.Fatal(err) }
pid := os.Getpid()
data, err := json.Marshal(Session{PID: pid, SessionID: "home-test", CWD: filepath.Join(home, "project"), StartedAt: time.Now().UnixMilli()})
if err != nil { t.Fatal(err) }
if err := os.WriteFile(filepath.Join(dir, strconv.Itoa(pid)+".json"), data, 0o644); err != nil { t.Fatal(err) }
rows, err := CollectLocal()
if err != nil { t.Fatal(err) }
// Find pid and assert row.Home == home; fail if row absent.
```

Add `TestSessionHomeJSONCompatibility`:

```go
data, err := json.Marshal(Session{Home: "/home/andy"})
if err != nil { t.Fatal(err) }
if !strings.Contains(string(data), `"home":"/home/andy"`) { t.Fatalf(...) }
var old Session
if err := json.Unmarshal([]byte(`{"pid":1,"cwd":"/home/andy/project"}`), &old); err != nil { t.Fatal(err) }
if old.Home != "" { t.Errorf("Home = %q, want empty", old.Home) }
```

- [ ] **Step 2: Run tests and confirm RED**

Run: `go test ./... -run 'TestCollectLocalSetsHome|TestSessionHomeJSONCompatibility' -count=1`

Expected: build failure because `Session.Home` does not exist.

- [ ] **Step 3: Add and populate home metadata**

In `Session`:

```go
Home string `json:"home,omitempty"` // collector's home, used for local/remote ~ collapse
Host string `json:"-"`              // client-only remote host label
```

In `CollectLocal`, set `s.Home = home` before appending each live row.

No `remote.go` change: existing JSON decoding preserves `Home`; older payloads leave it empty.

- [ ] **Step 4: Run focused and full tests**

Run:

```bash
gofmt -w session.go session_test.go
go test ./... -run 'TestCollectLocalSetsHome|TestSessionHomeJSONCompatibility' -count=1
go test ./...
```

Expected: all tests pass.

- [ ] **Step 5: Commit**

```bash
git add session.go session_test.go
git commit -m "feat: carry host home in session metadata" -m "Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 4: Collapse Each Row's Own Home in Every Render Mode

**Files:**
- Modify: `render.go:3-10,372-379,518-519,554-563,672-681,783-808`
- Modify: `render_test.go`

**Interfaces:**
- Change: `displayCWD(cwd, home string) string`
- Change: `deriveFull(s Session, now time.Time, sortMode string) drowFull`
- Change: `deriveMinimal(s Session, now time.Time, sortMode string) drowMinimal`
- Consume: `Session.Home`

- [ ] **Step 1: Write failing home-boundary tests**

Add table-driven `TestDisplayCWD`:

```go
cases := []struct{ cwd, home, want string }{
	{"/home/andy", "/home/andy", "~"},
	{"/home/andy/project", "/home/andy", "~/project"},
	{"/home/andy-other/project", "/home/andy", "/home/andy-other/project"},
	{"/var/tmp", "/home/andy", "/var/tmp"},
	{"/home/andy/project", "", "/home/andy/project"},
}
for _, tc := range cases {
	if got := displayCWD(tc.cwd, tc.home); got != tc.want {
		t.Errorf("displayCWD(%q, %q) = %q, want %q", tc.cwd, tc.home, got, tc.want)
	}
}
```

- [ ] **Step 2: Run boundary test and confirm RED**

Run: `go test ./... -run TestDisplayCWD -count=1`

Expected: build failure because old `displayCWD` requires three arguments.

- [ ] **Step 3: Implement safe per-row collapse**

```go
func displayCWD(cwd, home string) string {
	if home == "" {
		return cwd
	}
	if cwd == home {
		return "~"
	}
	if strings.HasPrefix(cwd, home+"/") {
		return "~" + strings.TrimPrefix(cwd, home)
	}
	return cwd
}
```

- [ ] **Step 4: Write failing local/remote derivation tests**

Add `TestDeriveFullUsesSessionHome`:

```go
now := time.Unix(100, 0)
cases := []struct {
	name string
	s    Session
	want string
}{
	{"local", Session{CWD: "/home/andy/project", Home: "/home/andy"}, "~/project"},
	{"remote", Session{CWD: "/home/rue/service", Home: "/home/rue", Host: "beluga"}, "~/service"},
	{"old remote", Session{CWD: "/home/rue/service", Host: "beluga"}, "/home/rue/service"},
}
for _, tc := range cases {
	row := deriveFull(tc.s, now, "dir")
	if row.cwdStr != tc.want {
		t.Errorf("%s cwd = %q, want %q", tc.name, row.cwdStr, tc.want)
	}
}
```

- [ ] **Step 5: Thread `Session.Home` through full, intermediate, and minimal views**

- Change `deriveFull` to `deriveFull(s Session, now time.Time, sortMode string)` and call `displayCWD(s.CWD, s.Home)`.
- Update both full/intermediate call sites.
- Change `deriveMinimal` to `deriveMinimal(s Session, now time.Time, sortMode string)` and call `displayCWD(s.CWD, s.Home)`.
- Update minimal call site.
- Remove all three render-side `os.UserHomeDir()` calls and unused `os` import.

- [ ] **Step 6: Run render and full tests**

Run:

```bash
gofmt -w render.go render_test.go
go test ./... -run 'TestDisplayCWD|TestDeriveFullUsesSessionHome' -count=1
go test ./...
```

Expected: all tests pass; rows from older servers with empty `Home` remain absolute.

- [ ] **Step 7: Commit**

```bash
git add render.go render_test.go
git commit -m "feat: collapse each host home to tilde" -m "Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 5: Verify and Review Entire Change

**Files:** All modified Go files, tests, and plan documentation.

**Interfaces:** Verification gate only.

- [ ] **Step 1: Check stale behavior and formatting**

Run:

```bash
gofmt -w *.go
grep -R "KillSession(pid" -n -- *.go || true
grep -R "displayCWD(.*Host" -n -- *.go || true
grep -n "os.UserHomeDir" render.go || true
git diff --check
```

Expected: grep commands and `git diff --check` produce no output.

- [ ] **Step 2: Run independent verification**

Run:

```bash
go test ./...
go vet ./...
```

Expected: both commands exit 0.

- [ ] **Step 3: Review final diff against approved spec**

Confirm:

- Local action confirms and kills same `Session.Tmux` value.
- CLI resolves tmux location before confirmation.
- Remote client sends PID only; server selects full collected row.
- Non-empty tmux errors cannot enter PID branch.
- Non-tmux SIGTERM/SIGKILL behavior remains unchanged.
- `CollectLocal` sets `Home`; remote JSON preserves it.
- Every render mode reads `Session.Home`; false prefixes remain unchanged.
- No new dependency appears in `go.mod` or `go.sum`.

- [ ] **Step 4: Commit only if verification required cleanup**

```bash
git add <only-cleanup-files>
git commit -m "test: finalize tmux kill verification" -m "Co-Authored-By: Claude <noreply@anthropic.com>"
```

Skip this commit when tree is already clean.

- [ ] **Step 5: Confirm repository state**

Run: `git status --short && git log --oneline -6`

Expected: clean working tree and focused commits for plan, kill behavior, home metadata, and rendering.
