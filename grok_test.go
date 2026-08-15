package main

import (
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// grokFixture writes an active_sessions.json holding body under home and
// returns home. body is the raw file text so a test can describe a corrupt or
// half-written file as easily as a well-formed one.
func grokFixture(t *testing.T, home, body string) {
	t.Helper()
	dir := filepath.Join(home, grokDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, grokActiveSessionsFile), []byte(body), 0o644); err != nil {
		t.Fatalf("write active_sessions.json: %v", err)
	}
}

// grokSummaryFixture writes a summary.json at the path grok itself uses:
// ~/.grok/sessions/<percent-encoded-cwd>/<session-id>/summary.json.
func grokSummaryFixture(t *testing.T, home, cwd, sessionID, body string) {
	t.Helper()
	dir := filepath.Join(home, grokDir, "sessions", url.PathEscape(cwd), sessionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "summary.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("write summary.json: %v", err)
	}
}

// grokEventsFixture writes events.jsonl beside that session's summary.
func grokEventsFixture(t *testing.T, home, cwd, sessionID string, lines ...string) {
	t.Helper()
	dir := filepath.Join(home, grokDir, "sessions", url.PathEscape(cwd), sessionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	body := ""
	if len(lines) > 0 {
		body = strings.Join(lines, "\n") + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, grokEventsFile), []byte(body), 0o644); err != nil {
		t.Fatalf("write events.jsonl: %v", err)
	}
}

// allPIDsAlive makes every pid in a fixture count as running, so a test can
// use readable made-up pids instead of hunting for real ones.
//
// It stubs BOTH liveness seams. Stubbing only the grok one leaves any claude
// fixture row filtered by the real pidAlive, which made a test's outcome
// depend on whether its made-up pid happened to exist on the machine running
// `go test` — green here, and green for the wrong reason.
func allPIDsAlive(t *testing.T) {
	t.Helper()
	prevGrok, prevSession := grokPIDAlive, sessionPIDAlive
	grokPIDAlive = func(int) bool { return true }
	sessionPIDAlive = func(int) bool { return true }
	t.Cleanup(func() {
		grokPIDAlive = prevGrok
		sessionPIDAlive = prevSession
	})
}

// livePIDs is allPIDsAlive's selective form for both seams: only the listed
// pids are running. Tests that turn on a pid being dead need this rather than
// the real check, for the same hermeticity reason.
func livePIDs(t *testing.T, pids ...int) {
	t.Helper()
	live := map[int]bool{}
	for _, p := range pids {
		live[p] = true
	}
	alive := func(pid int) bool { return live[pid] }
	prevGrok, prevSession := grokPIDAlive, sessionPIDAlive
	grokPIDAlive, sessionPIDAlive = alive, alive
	t.Cleanup(func() {
		grokPIDAlive = prevGrok
		sessionPIDAlive = prevSession
	})
}

// stubSessionPIDAlive overrides only the claude-side liveness seam, for the
// cases that turn on the two tools disagreeing about one pid.
func stubSessionPIDAlive(t *testing.T, fn func(int) bool) {
	t.Helper()
	prev := sessionPIDAlive
	sessionPIDAlive = fn
	t.Cleanup(func() { sessionPIDAlive = prev })
}

const grokActiveOneID = "01a00180-1735-75a1-b08a-11c777abcdd4"

const grokActiveOne = `[
  {
    "session_id": "01a00180-1735-75a1-b08a-11c777abcdd4",
    "pid": 137851,
    "cwd": "/work/trecs-brain",
    "opened_at": "2026-08-14T18:24:21.263567932Z"
  }
]`

const grokSummaryFull = `{
  "info": {"id": "01a00180-1735-75a1-b08a-11c777abcdd4", "cwd": "/work/trecs-brain"},
  "session_summary": "DR-3143 Ticket or Issue Review",
  "created_at": "2026-08-14T18:19:26.918838278Z",
  "updated_at": "2026-08-14T18:30:00.000000000Z",
  "num_messages": 138,
  "current_model_id": "grok-4.6",
  "git_root_dir": "/work/trecs-brain/",
  "head_branch": "develop",
  "last_active_at": "2026-08-14T18:34:09.451788812Z",
  "generated_title": "Ticket review",
  "reasoning_effort": "high"
}`

func TestCollectGrokLocalReadsRegistryAndSummary(t *testing.T) {
	allPIDsAlive(t)
	home := t.TempDir()
	grokFixture(t, home, grokActiveOne)
	grokSummaryFixture(t, home, "/work/trecs-brain", "01a00180-1735-75a1-b08a-11c777abcdd4", grokSummaryFull)

	rows := collectGrokLocal(home)
	if len(rows) != 1 {
		t.Fatalf("collectGrokLocal returned %d rows, want 1", len(rows))
	}
	s := rows[0]
	if !s.IsGrok() || s.Tool != toolGrok {
		t.Errorf("Tool = %q, want %q", s.Tool, toolGrok)
	}
	if s.PID != 137851 {
		t.Errorf("PID = %d, want 137851", s.PID)
	}
	if s.SessionID != "01a00180-1735-75a1-b08a-11c777abcdd4" {
		t.Errorf("SessionID = %q", s.SessionID)
	}
	if s.CWD != "/work/trecs-brain" {
		t.Errorf("CWD = %q", s.CWD)
	}
	if want := mustMillis(t, "2026-08-14T18:24:21.263567932Z"); s.StartedAt != want {
		t.Errorf("StartedAt = %d, want %d", s.StartedAt, want)
	}
	// last_active_at wins over updated_at: it is the field grok stamps on
	// every turn, and the one the AGE column is meant to reflect.
	if want := mustMillis(t, "2026-08-14T18:34:09.451788812Z"); s.UpdatedAt != want {
		t.Errorf("UpdatedAt = %d, want %d", s.UpdatedAt, want)
	}
	if s.Name != "Ticket review" || s.NameSource != "derived" {
		t.Errorf("Name/NameSource = %q/%q, want %q/%q", s.Name, s.NameSource, "Ticket review", "derived")
	}
	if s.Model != "grok-4.6" {
		t.Errorf("Model = %q, want grok-4.6", s.Model)
	}
	if s.Headless() {
		t.Error("grok row reported Headless; it should render as an interactive session")
	}
	// The shared enrichment pass in CollectLocal owns these — the collector
	// must not pre-fill them with a different answer.
	if s.CPU != "" || s.Tmux != "" || s.Home != "" || s.GitRoot != "" {
		t.Errorf("collector filled an enrichment field: %+v", s)
	}
	// No events.jsonl in this fixture: status stays empty so the TUI keeps
	// the "-" placeholder rather than inventing a phase.
	if s.Status != "" || s.WaitingFor != "" {
		t.Errorf("Status/WaitingFor = %q/%q, want empty without events.jsonl", s.Status, s.WaitingFor)
	}
}

func mustMillis(t *testing.T, ts string) int64 {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		t.Fatalf("parse %q: %v", ts, err)
	}
	return parsed.UnixMilli()
}

func TestCollectGrokLocalSkipsDeadPIDs(t *testing.T) {
	home := t.TempDir()
	prev := grokPIDAlive
	grokPIDAlive = func(pid int) bool { return pid == 222 }
	t.Cleanup(func() { grokPIDAlive = prev })
	grokFixture(t, home, `[
	  {"session_id": "dead-one", "pid": 111, "cwd": "/work/a", "opened_at": "2026-08-14T18:00:00Z"},
	  {"session_id": "live-one", "pid": 222, "cwd": "/work/b", "opened_at": "2026-08-14T18:00:00Z"}
	]`)

	rows := collectGrokLocal(home)
	if len(rows) != 1 || rows[0].SessionID != "live-one" {
		t.Fatalf("collectGrokLocal = %+v, want only the live session", rows)
	}
}

func TestCollectGrokLocalSkipsScratchCWDs(t *testing.T) {
	allPIDsAlive(t)
	home := t.TempDir()
	grokFixture(t, home, `[
	  {"session_id": "scratch", "pid": 111, "cwd": "/tmp/throwaway", "opened_at": "2026-08-14T18:00:00Z"},
	  {"session_id": "real", "pid": 222, "cwd": "/work/b", "opened_at": "2026-08-14T18:00:00Z"}
	]`)

	rows := collectGrokLocal(home)
	if len(rows) != 1 || rows[0].SessionID != "real" {
		t.Fatalf("collectGrokLocal = %+v, want the scratch cwd filtered out", rows)
	}
}

// A missing, unreadable or half-written registry all mean "no grok rows this
// pass" — never an error, because the caller is really trying to list claude
// sessions and a file this tool does not own must not be able to blank that.
func TestCollectGrokLocalToleratesMissingAndCorruptRegistry(t *testing.T) {
	allPIDsAlive(t)
	cases := []struct {
		name  string
		write bool
		body  string
	}{
		{"no ~/.grok at all", false, ""},
		{"torn mid-write read", true, `[{"session_id": "a", "pid": 1,`},
		{"not an array", true, `{"session_id": "a", "pid": 1}`},
		{"empty file", true, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home := t.TempDir()
			if c.write {
				grokFixture(t, home, c.body)
			}
			if rows := collectGrokLocal(home); rows != nil {
				t.Fatalf("collectGrokLocal = %+v, want nil", rows)
			}
		})
	}
}

func TestCollectGrokLocalKeepsRowWithoutASummary(t *testing.T) {
	allPIDsAlive(t)
	home := t.TempDir()
	grokFixture(t, home, grokActiveOne)

	rows := collectGrokLocal(home)
	if len(rows) != 1 {
		t.Fatalf("collectGrokLocal returned %d rows, want 1", len(rows))
	}
	s := rows[0]
	if s.PID != 137851 || s.CWD != "/work/trecs-brain" {
		t.Errorf("basic row lost its registry fields: %+v", s)
	}
	if s.Name != "" || s.Model != "" || s.UpdatedAt != 0 {
		t.Errorf("row without a summary invented metadata: %+v", s)
	}
	// DisplayName's own chain still resolves a label, exactly as it does for a
	// claude session with no name of its own.
	if label, dimmed := s.DisplayName(); label != "-" || !dimmed {
		t.Errorf("DisplayName = (%q, %v), want (\"-\", true)", label, dimmed)
	}
}

// With no parseable opened_at and no summary there is nothing on disk to date
// the row by, and a zero StartedAt would make Session.Updated answer the epoch
// — an AGE column reading ~20679d. Collection time is the honest stand-in.
func TestCollectGrokLocalStampsARowWithNoUsableTimestamp(t *testing.T) {
	allPIDsAlive(t)
	home := t.TempDir()
	before := time.Now().UnixMilli()
	grokFixture(t, home, `[
	  {"session_id": "no-stamp", "pid": 111, "cwd": "/work/a", "opened_at": "not-a-timestamp"}
	]`)

	rows := collectGrokLocal(home)
	if len(rows) != 1 {
		t.Fatalf("collectGrokLocal returned %d rows, want 1", len(rows))
	}
	after := time.Now().UnixMilli()
	if got := rows[0].StartedAt; got < before || got > after {
		t.Fatalf("StartedAt = %d, want a collection-time stamp in [%d, %d]", got, before, after)
	}
	if age := time.Since(rows[0].Updated()); age > time.Minute {
		t.Fatalf("Updated() is %v old, want ~now", age)
	}
}

func TestGrokSummaryNamePrefersGeneratedTitle(t *testing.T) {
	cases := []struct {
		name string
		sum  grokSummary
		want string
	}{
		{"title wins", grokSummary{GeneratedTitle: "Title", SessionSummary: "Summary"}, "Title"},
		{"summary is the fallback", grokSummary{SessionSummary: "Summary"}, "Summary"},
		{"neither leaves it to DisplayName", grokSummary{}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := grokSummaryName(c.sum); got != c.want {
				t.Errorf("grokSummaryName = %q, want %q", got, c.want)
			}
		})
	}
}

// The per-cwd directory name is url.PathEscape of the absolute path — verified
// against the live ~/.grok/sessions directories, including a worktree path
// whose "." and "-" are left unescaped and whose every "/" becomes %2F.
func TestGrokSummaryPathMatchesGroksOwnEncoding(t *testing.T) {
	home := t.TempDir()
	const cwd = "/work/trecs-brain/.claude/worktrees/DR-3143"
	const sid = "sess-1"
	grokSummaryFixture(t, home, cwd, sid, `{"current_model_id": "grok-4.6"}`)

	encoded := "%2Fwork%2Ftrecs-brain%2F.claude%2Fworktrees%2FDR-3143"
	want := filepath.Join(home, grokDir, "sessions", encoded, sid, "summary.json")
	got, ok := grokSummaryPath(home, cwd, sid)
	if !ok || got != want {
		t.Fatalf("grokSummaryPath = (%q, %v), want (%q, true)", got, ok, want)
	}
}

// The exact encoding is not load-bearing: a summary filed under a directory
// name this tool would not have derived is still found by the fallback scan,
// so a grok release that changes the encoding costs one os.ReadDir rather than
// every row's metadata.
func TestGrokSummaryPathFallsBackToScanningTheSessionsDir(t *testing.T) {
	allPIDsAlive(t)
	home := t.TempDir()
	const sid = "01a00180-1735-75a1-b08a-11c777abcdd4"
	dir := filepath.Join(home, grokDir, "sessions", "some-other-encoding", sid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "summary.json"), []byte(grokSummaryFull), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	grokFixture(t, home, grokActiveOne)

	rows := collectGrokLocal(home)
	if len(rows) != 1 {
		t.Fatalf("collectGrokLocal returned %d rows, want 1", len(rows))
	}
	if rows[0].Model != "grok-4.6" {
		t.Errorf("Model = %q, want grok-4.6 (fallback scan did not find the summary)", rows[0].Model)
	}
}

// countGrokScans replaces the fallback scan's directory listing with a
// counter, so a test can prove the scan does not run rather than merely that
// the answer came out right. CollectLocal runs every 2s, so "did not scan" is
// the property that matters on a host with thousands of per-cwd directories.
func countGrokScans(t *testing.T) *int {
	t.Helper()
	var n int
	prev := grokSessionsReadDir
	grokSessionsReadDir = func(name string) ([]os.DirEntry, error) {
		n++
		return prev(name)
	}
	t.Cleanup(func() { grokSessionsReadDir = prev })
	return &n
}

// The common shape of a brand-new session: grok has made the per-cwd directory
// but has not written summary.json yet (it writes it on the first turn). The
// encoding is therefore known-good and there is nothing a scan could find, so
// no scan may run — otherwise every 2s tick would list the whole sessions dir.
func TestGrokSummaryPathDoesNotScanWhenTheCwdDirExists(t *testing.T) {
	allPIDsAlive(t)
	scans := countGrokScans(t)
	home := t.TempDir()
	const cwd = "/work/trecs-brain"
	// The per-cwd directory and the session's own directory exist; only
	// summary.json is missing.
	dir := filepath.Join(home, grokDir, "sessions", url.PathEscape(cwd), "01a00180-1735-75a1-b08a-11c777abcdd4")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	grokFixture(t, home, grokActiveOne)

	rows := collectGrokLocal(home)
	if len(rows) != 1 || rows[0].Name != "" {
		t.Fatalf("collectGrokLocal = %+v, want one row with no summary metadata", rows)
	}
	if *scans != 0 {
		t.Fatalf("fallback scan ran %d times for a summary-less session, want 0", *scans)
	}
}

// A resolved scan is memoized per cwd, so an encoding this tool cannot derive
// costs one scan for the life of the process rather than one per pass.
func TestGrokSummaryPathMemoizesAResolvedScan(t *testing.T) {
	allPIDsAlive(t)
	scans := countGrokScans(t)
	home := t.TempDir()
	const sid = "01a00180-1735-75a1-b08a-11c777abcdd4"
	dir := filepath.Join(home, grokDir, "sessions", "some-other-encoding", sid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "summary.json"), []byte(grokSummaryFull), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	grokFixture(t, home, grokActiveOne)

	for pass := 1; pass <= 3; pass++ {
		rows := collectGrokLocal(home)
		if len(rows) != 1 || rows[0].Model != "grok-4.6" {
			t.Fatalf("pass %d: collectGrokLocal = %+v, want the scanned summary", pass, rows)
		}
	}
	if *scans != 1 {
		t.Fatalf("fallback scan ran %d times over three passes, want 1 (memoized)", *scans)
	}
}

func TestGrokSessionByPID(t *testing.T) {
	allPIDsAlive(t)
	home := t.TempDir()
	grokFixture(t, home, grokActiveOne)

	s, ok := grokSessionByPID(home, 137851)
	if !ok || s.SessionID != "01a00180-1735-75a1-b08a-11c777abcdd4" {
		t.Fatalf("grokSessionByPID(137851) = (%+v, %v), want the registry entry", s, ok)
	}
	if _, ok := grokSessionByPID(home, 999); ok {
		t.Error("grokSessionByPID resolved a pid the registry does not list")
	}
}

// A registry entry left behind by a crashed grok must not attest a pid that
// now belongs to somebody else — unlike claude's session file, whose presence
// never claimed the pid was still in use.
func TestGrokSessionByPIDRefusesADeadPID(t *testing.T) {
	home := t.TempDir()
	prev := grokPIDAlive
	grokPIDAlive = func(int) bool { return false }
	t.Cleanup(func() { grokPIDAlive = prev })
	grokFixture(t, home, grokActiveOne)

	if _, ok := grokSessionByPID(home, 137851); ok {
		t.Error("grokSessionByPID attested a stale entry whose process is gone")
	}
}

// stubGrokLookup points grokSessionLookup at a fixed answer for the duration
// of a test, so the reattestation paths can be driven without a real ~/.grok.
func stubGrokLookup(t *testing.T, rows ...Session) {
	t.Helper()
	prev := grokSessionLookup
	grokSessionLookup = func(pid int) (Session, bool) {
		for _, s := range rows {
			if s.PID == pid {
				return s, true
			}
		}
		return Session{}, false
	}
	t.Cleanup(func() { grokSessionLookup = prev })
}

func TestLocalReattestSessionRoutesGrokToItsOwnRegistry(t *testing.T) {
	live := Session{Tool: toolGrok, PID: 4242, SessionID: "grok-sess", CWD: "/work/a"}
	stubGrokLookup(t, live)

	if err := localReattestSession(live); err != nil {
		t.Fatalf("matching grok reattest = %v, want nil", err)
	}

	moved := live
	moved.SessionID = "grok-other"
	err := localReattestSession(moved)
	if err == nil || !strings.Contains(err.Error(), "different session") {
		t.Fatalf("mismatched grok reattest = %v, want a session-mismatch error", err)
	}

	gone := Session{Tool: toolGrok, PID: 9999, SessionID: "grok-sess"}
	err = localReattestSession(gone)
	if err == nil || !strings.Contains(err.Error(), "not a live grok session") {
		t.Fatalf("absent grok reattest = %v, want a not-live error", err)
	}
}

// An empty session id means "no precondition", the same contract localReattest
// and the server's sessionIDPrecondition already follow.
func TestLocalReattestGrokSkipsWithoutASessionID(t *testing.T) {
	stubGrokLookup(t) // nothing live at all
	if err := localReattestSession(Session{Tool: toolGrok, PID: 4242}); err != nil {
		t.Fatalf("grok reattest with no session id = %v, want nil (skip)", err)
	}
}

func TestServerReattestRoutesGrokToItsOwnRegistry(t *testing.T) {
	stubGrokLookup(t, Session{Tool: toolGrok, PID: 4242, SessionID: "grok-sess"})
	// A claude attestation must never be consulted for a grok row: wire the
	// seam to a value that would produce the wrong verdict if it were.
	s := &server{attest: func(int) (Session, bool) {
		return Session{PID: 4242, SessionID: "claude-sess"}, true
	}}

	if refusal := s.reattest(4242, "grok-sess", toolGrok); refusal != nil {
		t.Fatalf("matching grok reattest refused: %+v", refusal)
	}
	refusal := s.reattest(4242, "grok-other", toolGrok)
	if refusal == nil || refusal.Code != codeSessionMismatch {
		t.Fatalf("mismatched grok reattest = %+v, want %s", refusal, codeSessionMismatch)
	}
	refusal = s.reattest(9999, "grok-sess", toolGrok)
	if refusal == nil || refusal.Code != codeNotLive {
		t.Fatalf("absent grok reattest = %+v, want %s", refusal, codeNotLive)
	}
	if refusal != nil && !strings.Contains(refusal.Error, "live grok session") {
		t.Errorf("refusal names the wrong tool: %q", refusal.Error)
	}
	// A claude row still reads claude's own store, unchanged.
	if refusal := s.reattest(4242, "claude-sess", ""); refusal != nil {
		t.Fatalf("claude reattest refused: %+v", refusal)
	}
}

// Migrate means "kill it and respawn as `claude --resume <id>`", which no grok
// session can be. The refusal lives in MigrateLocalAttested so every entry
// point — the server handler, cmdMigrate and actAttach's migrate branch — gets
// it from one place.
func TestMigrateRefusesGrokSessions(t *testing.T) {
	stubGrokLookup(t, Session{Tool: toolGrok, PID: 999999, SessionID: "grok-sess"})
	_, err := MigrateLocalAttested(999999, "grok-sess")
	if err == nil || !strings.Contains(err.Error(), "not supported for grok") {
		t.Fatalf("MigrateLocalAttested on a grok pid = %v, want a grok refusal", err)
	}
}

// `attach PID` on a grok session with no tmux pane must not point the user at
// `migrate`, which can only refuse. The refusal branch returns before anything
// touches tmux, so this is exercisable without a terminal.
func TestCmdAttachRefusesToSuggestMigrateForAGrokSession(t *testing.T) {
	stubGrokLookup(t, Session{Tool: toolGrok, PID: 999999, SessionID: "grok-sess"})
	if code := cmdAttach([]string{"999999"}); code != 1 {
		t.Fatalf("cmdAttach = %d, want 1", code)
	}
}

func TestMigrateStillReportsAMissingSessionFile(t *testing.T) {
	stubGrokLookup(t) // no grok session at this pid either
	_, err := MigrateLocalAttested(999999, "")
	if err == nil || !strings.Contains(err.Error(), "no session file") {
		t.Fatalf("MigrateLocalAttested on an unknown pid = %v, want the unchanged message", err)
	}
}

// The kill endpoint needs no grok-specific handler: resolveLivePID finds the
// row in the same collected list claude rows come from, and the only per-tool
// step — the last-moment reattestation — routes off the row's own Tool.
func TestKillHandlerAttestsAGrokRowAgainstItsOwnRegistry(t *testing.T) {
	live := Session{Tool: toolGrok, PID: 55, SessionID: "grok-sess", Tmux: "work:1.0"}
	stubGrokLookup(t, live)
	var got Session
	s := &server{
		token:   "secret",
		collect: func() ([]Session, error) { return []Session{live}, nil },
		// Claude's attestation seam would answer wrongly for this pid; it must
		// never be consulted for a grok row.
		attest:    func(int) (Session, bool) { return Session{PID: 55, SessionID: "claude-sess"}, true },
		terminate: func(target Session) error { got = target; return nil },
	}
	rec := httptest.NewRecorder()
	s.kill(rec, killRequest(t, 55, `{"session_id":"grok-sess"}`))

	if got != live {
		t.Fatalf("terminated %#v, want %#v", got, live)
	}
	if r := decodeAction(t, rec); !r.OK || r.Code != "" {
		t.Fatalf("result = %#v, want ok with no code", r)
	}
}

func TestKillHandlerRefusesAGrokRowWhoseRegistryEntryMoved(t *testing.T) {
	live := Session{Tool: toolGrok, PID: 55, SessionID: "grok-sess", Tmux: "work:1.0"}
	// The row still says grok-sess, but the registry has already moved on.
	stubGrokLookup(t, Session{Tool: toolGrok, PID: 55, SessionID: "grok-other"})
	terminated := false
	s := &server{
		token:     "secret",
		collect:   func() ([]Session, error) { return []Session{live}, nil },
		terminate: func(Session) error { terminated = true; return nil },
	}
	rec := httptest.NewRecorder()
	s.kill(rec, killRequest(t, 55, `{"session_id":"grok-sess"}`))

	if terminated {
		t.Fatal("terminate called after the registry entry moved")
	}
	if r := decodeAction(t, rec); r.OK || r.Code != codeSessionMismatch {
		t.Fatalf("result = %#v, want %s", r, codeSessionMismatch)
	}
}

// Claude does not delete a session file when its process exits, so a pid it
// once used and grok now owns would produce two rows sharing one Session.ID()
// — which selection, kill routing and the TUI's row bookkeeping all assume is
// unique. Claude's row wins.
func TestCollectLocalDropsAGrokRowWhosePIDAClaudeRowHolds(t *testing.T) {
	allPIDsAlive(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeClaudeSessionFile(t, home, Session{
		PID: 137851, SessionID: "claude-sess", CWD: "/work/a",
		Status: "idle", Entrypoint: "cli", Name: "mine", NameSource: "user",
	})
	grokFixture(t, home, grokActiveOne) // same pid, 137851

	rows, err := CollectLocal()
	if err != nil {
		t.Fatalf("CollectLocal: %v", err)
	}
	var atPID []Session
	for _, s := range rows {
		if s.PID == 137851 {
			atPID = append(atPID, s)
		}
	}
	if len(atPID) != 1 {
		t.Fatalf("PID 137851 produced %d rows, want 1: %+v", len(atPID), atPID)
	}
	if atPID[0].IsGrok() || atPID[0].SessionID != "claude-sess" {
		t.Fatalf("row = %+v, want the claude session to win", atPID[0])
	}
}

// writeClaudeSessionFile drops a ~/.claude/sessions/<pid>.json under home, the
// shape CollectLocal reads for claude rows.
func writeClaudeSessionFile(t *testing.T, home string, s Session) {
	t.Helper()
	dir := filepath.Join(home, ".claude", "sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal session: %v", err)
	}
	path := filepath.Join(dir, strconv.Itoa(s.PID)+".json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// A grok row can be grouped or disabled from the TUI exactly like a claude
// one, so its id has to resolve for the flag store's prune — otherwise the
// very next flag write deletes the badge the user just set. The resolver is
// the real resolvableSessionIDs here: a fake one would prove nothing about
// the union this test exists for.
func TestFlagsStoreKeepsAGrokSessionsFlagsThroughAPrune(t *testing.T) {
	allPIDsAlive(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeClaudeSessionFile(t, home, Session{
		PID: 4242, SessionID: "claude-sess", CWD: "/work/a",
		Status: "idle", Entrypoint: "cli",
	})
	grokFixture(t, home, grokActiveOne)

	path := filepath.Join(t.TempDir(), flagsFileName)
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	seed := newFlagsStore(path, fixedClock(now), noResolver)
	seed.SetGroup(grokActiveOneID, 3)
	seed.SetGroup("long-gone", 4)

	// Any write through a real resolver runs the prune.
	pruning := newFlagsStore(path, fixedClock(now), resolvableSessionIDs)
	pruning.SetGroup("claude-sess", 5)

	entries := readFlagsFile(t, path)
	if got := entries[grokActiveOneID].Group; got != 3 {
		t.Fatalf("grok session group = %d, want 3 (it was pruned): %#v", got, entries)
	}
	if _, ok := entries["long-gone"]; ok {
		t.Fatalf("an unresolvable entry survived, so the prune never ran: %#v", entries)
	}
}

// readSessionByPID checks nothing about liveness — Claude Code leaves the file
// behind when the process exits — so a pid it once used and grok now owns must
// not resolve to the dead claude session. `kill PID` would otherwise act on
// the wrong row entirely.
func TestLookupLiveSessionPrefersLiveGrokOverADeadClaudeFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeClaudeSessionFile(t, home, Session{PID: 4242, SessionID: "claude-sess", CWD: "/work/a"})
	stubSessionPIDAlive(t, func(int) bool { return false })
	stubGrokLookup(t, Session{Tool: toolGrok, PID: 4242, SessionID: "grok-sess"})

	got, ok := lookupLiveSessionByPID(4242)
	if !ok || !got.IsGrok() || got.SessionID != "grok-sess" {
		t.Fatalf("lookupLiveSessionByPID = (%+v, %v), want the live grok session", got, ok)
	}
}

// The narrowness of that rule matters: a dead claude pid with nothing in
// grok's registry still resolves to the claude row, which is the shape
// `migrate` legitimately uses to resume a session whose process died.
func TestLookupLiveSessionKeepsADeadClaudeSessionWithNoGrokAtThatPID(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeClaudeSessionFile(t, home, Session{PID: 4242, SessionID: "claude-sess", CWD: "/work/a"})
	stubSessionPIDAlive(t, func(int) bool { return false })
	stubGrokLookup(t) // nothing live in grok's registry

	got, ok := lookupLiveSessionByPID(4242)
	if !ok || got.IsGrok() || got.SessionID != "claude-sess" {
		t.Fatalf("lookupLiveSessionByPID = (%+v, %v), want the claude session", got, ok)
	}
}

// Same substitution, seen from migrate: SIGTERMing the live grok process and
// resuming a stranger's transcript in its place is the worst outcome available
// on this path, so it refuses rather than adopting the stale claude file.
func TestMigrateRefusesADeadClaudePIDOwnedByALiveGrokSession(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeClaudeSessionFile(t, home, Session{PID: 4242, SessionID: "claude-sess", CWD: "/work/a"})
	stubSessionPIDAlive(t, func(int) bool { return false })
	stubGrokLookup(t, Session{Tool: toolGrok, PID: 4242, SessionID: "grok-sess"})

	_, err := MigrateLocalAttested(4242, "claude-sess")
	if err == nil || !strings.Contains(err.Error(), "not supported for grok") {
		t.Fatalf("MigrateLocalAttested = %v, want a grok refusal", err)
	}
}

// A grok session sitting in a worktree is a live process in that checkout, so
// it must keep the worktree from being removed exactly as a claude one does.
func TestWorktreeSurvivorsCountGrokSessions(t *testing.T) {
	const wt = "/repo/.claude/worktrees/DR-1/sub"
	target := Session{PID: 1, CWD: wt}
	grok := Session{Tool: toolGrok, PID: 2, CWD: wt}

	survivors := worktreeSurvivors(target, []Session{target, grok})
	if len(survivors) != 1 || survivors[0].PID != 2 {
		t.Fatalf("worktreeSurvivors = %+v, want the grok session", survivors)
	}
}

// A snapshot is restored by resuming each entry's claude transcript, and a
// grok session has none — capturing one would only produce an entry whose
// restore is guaranteed to fail.
func TestSaveSnapshotSkipsGrokSessions(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	sessions := []Session{
		{PID: 1, SessionID: "claude-sess", CWD: "/work/a"},
		{Tool: toolGrok, PID: 2, SessionID: "grok-sess", CWD: "/work/b"},
	}
	if _, n, err := saveSnapshotFrom("grok-test", sessions); err != nil {
		t.Fatalf("saveSnapshotFrom: %v", err)
	} else if n != 1 {
		t.Fatalf("saved %d entries, want 1", n)
	}
	snap, err := loadSnapshot("grok-test")
	if err != nil {
		t.Fatalf("loadSnapshot: %v", err)
	}
	if len(snap.Entries) != 1 || snap.Entries[0].SessionID != "claude-sess" {
		t.Fatalf("snapshot entries = %+v, want only the claude session", snap.Entries)
	}
}

// CollectLocal is where a grok row picks up the CPU reading and tmux pane
// address every action depends on — the collector deliberately leaves those
// blank so this one shared pass fills them for both tools.
func TestCollectLocalIncludesEnrichedGrokRows(t *testing.T) {
	allPIDsAlive(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	grokFixture(t, home, grokActiveOne)
	grokSummaryFixture(t, home, "/work/trecs-brain", "01a00180-1735-75a1-b08a-11c777abcdd4", grokSummaryFull)

	rows, err := CollectLocal()
	if err != nil {
		t.Fatalf("CollectLocal: %v", err)
	}
	var got *Session
	for i := range rows {
		if rows[i].PID == 137851 {
			got = &rows[i]
		}
	}
	if got == nil {
		t.Fatalf("CollectLocal did not include the grok row: %+v", rows)
	}
	if !got.IsGrok() {
		t.Errorf("row lost its Tool: %+v", got)
	}
	if got.Home != home {
		t.Errorf("Home = %q, want %q", got.Home, home)
	}
	if got.CPU == "" {
		t.Error("CPU was never filled by the shared enrichment pass")
	}
	// Model came from grok's summary and must survive the pass that skips the
	// claude transcript scan.
	if got.Model != "grok-4.6" {
		t.Errorf("Model = %q, want grok-4.6", got.Model)
	}
	if got.CostUSD != 0 || got.ContextTokens != 0 {
		t.Errorf("grok row picked up claude transcript metrics: %+v", got)
	}
}

// The account-switch warning is about processes holding the outgoing Anthropic
// token. A grok session holds none, so it must not appear there.
func TestCollectLocalLiteExcludesGrokSessions(t *testing.T) {
	allPIDsAlive(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	grokFixture(t, home, grokActiveOne)

	rows, err := CollectLocalLite()
	if err != nil {
		t.Fatalf("CollectLocalLite: %v", err)
	}
	for _, s := range rows {
		if s.IsGrok() {
			t.Fatalf("CollectLocalLite listed a grok session: %+v", s)
		}
	}
}

func TestGrokStatusFromEvents(t *testing.T) {
	tests := []struct {
		name       string
		lines      []string
		status     string
		waitingFor string
	}{
		{name: "empty", status: "", waitingFor: ""},
		{
			name: "turn_ended is idle even after a streaming phase",
			lines: []string{
				`{"type":"phase_changed","phase":"streaming_text"}`,
				`{"type":"turn_ended","outcome":"completed"}`,
			},
			status: "idle",
		},
		{
			name: "a busy phase after turn_ended is a new turn",
			lines: []string{
				`{"type":"turn_ended","outcome":"completed"}`,
				`{"type":"phase_changed","phase":"waiting_for_model"}`,
			},
			status: "busy",
		},
		{
			name: "a non-busy trailer after turn_ended stays idle",
			lines: []string{
				`{"type":"phase_changed","phase":"streaming_text"}`,
				`{"type":"turn_ended","outcome":"completed"}`,
				`{"type":"tool_completed","tool_name":"read_file"}`,
			},
			status: "idle",
		},
		{
			name: "streaming_reasoning is busy",
			lines: []string{
				`{"type":"turn_started"}`,
				`{"type":"phase_changed","phase":"streaming_reasoning"}`,
			},
			status: "busy",
		},
		{
			name:   "tool_execution is busy",
			lines:  []string{`{"type":"phase_changed","phase":"tool_execution"}`},
			status: "busy",
		},
		{
			name:   "waiting_for_model is busy",
			lines:  []string{`{"type":"phase_changed","phase":"waiting_for_model"}`},
			status: "busy",
		},
		{
			name: "a tool permission prompt is busy, not waiting",
			lines: []string{
				`{"type":"phase_changed","phase":"permission_prompt"}`,
				`{"type":"permission_requested","tool_name":"run_terminal_command"}`,
			},
			status: "busy",
		},
		{
			name: "a nameless tool permission is still busy",
			lines: []string{
				`{"type":"permission_requested"}`,
			},
			status: "busy",
		},
		{
			name: "ask_user_question is waiting for the user",
			lines: []string{
				`{"type":"tool_started","tool_name":"ask_user_question"}`,
			},
			status:     "waiting",
			waitingFor: "input",
		},
		{
			name: "a finished ask_user_question is not left waiting",
			lines: []string{
				`{"type":"tool_started","tool_name":"ask_user_question"}`,
				`{"type":"tool_completed","tool_name":"ask_user_question"}`,
				`{"type":"phase_changed","phase":"streaming_text"}`,
			},
			status: "busy",
		},
		{
			name: "resolved permission falls through to the next phase",
			lines: []string{
				`{"type":"permission_requested","tool_name":"read_file"}`,
				`{"type":"permission_resolved","tool_name":"read_file","decision":"allow"}`,
				`{"type":"phase_changed","phase":"tool_execution"}`,
			},
			status: "busy",
		},
		{
			name: "turn_ended clears a leftover permission",
			lines: []string{
				`{"type":"permission_requested","tool_name":"read_file"}`,
				`{"type":"turn_ended","outcome":"completed"}`,
			},
			status: "idle",
		},
		{
			name: "a torn line does not blank a later valid event",
			lines: []string{
				`{not json`,
				`{"type":"turn_ended","outcome":"completed"}`,
			},
			status: "idle",
		},
		{
			name:   "an unknown event is not a status",
			lines:  []string{`{"type":"mcp_init_completed"}`},
			status: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := ""
			if len(tt.lines) > 0 {
				body = strings.Join(tt.lines, "\n") + "\n"
			}
			status, waitingFor := grokStatusFromEvents([]byte(body))
			if status != tt.status || waitingFor != tt.waitingFor {
				t.Errorf("grokStatusFromEvents = (%q, %q), want (%q, %q)",
					status, waitingFor, tt.status, tt.waitingFor)
			}
		})
	}
}

func TestCollectGrokLocalDerivesStatusFromEvents(t *testing.T) {
	allPIDsAlive(t)
	home := t.TempDir()
	grokFixture(t, home, grokActiveOne)
	grokSummaryFixture(t, home, "/work/trecs-brain", grokActiveOneID, grokSummaryFull)
	grokEventsFixture(t, home, "/work/trecs-brain", grokActiveOneID,
		`{"type":"phase_changed","phase":"streaming_text"}`,
		`{"type":"turn_ended","outcome":"completed"}`,
	)

	rows := collectGrokLocal(home)
	if len(rows) != 1 {
		t.Fatalf("collectGrokLocal returned %d rows, want 1", len(rows))
	}
	if rows[0].Status != "idle" || rows[0].WaitingFor != "" {
		t.Errorf("Status/WaitingFor = %q/%q, want idle/", rows[0].Status, rows[0].WaitingFor)
	}
}

func TestCollectGrokLocalTreatsAToolPermissionAsBusy(t *testing.T) {
	allPIDsAlive(t)
	home := t.TempDir()
	grokFixture(t, home, grokActiveOne)
	grokSummaryFixture(t, home, "/work/trecs-brain", grokActiveOneID, grokSummaryFull)
	grokEventsFixture(t, home, "/work/trecs-brain", grokActiveOneID,
		`{"type":"phase_changed","phase":"permission_prompt"}`,
		`{"type":"permission_requested","tool_name":"run_terminal_command"}`,
	)

	rows := collectGrokLocal(home)
	if len(rows) != 1 {
		t.Fatalf("collectGrokLocal returned %d rows, want 1", len(rows))
	}
	if rows[0].Status != "busy" || rows[0].WaitingFor != "" {
		t.Errorf("Status/WaitingFor = %q/%q, want busy/", rows[0].Status, rows[0].WaitingFor)
	}
	if rows[0].Waiting() {
		t.Error("Waiting() is true for a tool permission; that is a user-input signal")
	}
}

func TestCollectGrokLocalDerivesWaitingFromAskUserQuestion(t *testing.T) {
	allPIDsAlive(t)
	home := t.TempDir()
	grokFixture(t, home, grokActiveOne)
	grokSummaryFixture(t, home, "/work/trecs-brain", grokActiveOneID, grokSummaryFull)
	grokEventsFixture(t, home, "/work/trecs-brain", grokActiveOneID,
		`{"type":"tool_started","tool_name":"ask_user_question"}`,
	)

	rows := collectGrokLocal(home)
	if len(rows) != 1 {
		t.Fatalf("collectGrokLocal returned %d rows, want 1", len(rows))
	}
	if rows[0].Status != "waiting" || rows[0].WaitingFor != "input" {
		t.Errorf("Status/WaitingFor = %q/%q, want waiting/input",
			rows[0].Status, rows[0].WaitingFor)
	}
	if !rows[0].Waiting() {
		t.Error("Waiting() is false; sessionStatusRank would bury this row")
	}
}

// events.jsonl is written during the first turn, often before summary.json.
// Status must not wait on the summary, and looking for it must not scan.
func TestCollectGrokLocalReadsEventsWithoutASummary(t *testing.T) {
	allPIDsAlive(t)
	scans := countGrokScans(t)
	home := t.TempDir()
	grokFixture(t, home, grokActiveOne)
	grokEventsFixture(t, home, "/work/trecs-brain", grokActiveOneID,
		`{"type":"phase_changed","phase":"tool_execution"}`,
	)

	rows := collectGrokLocal(home)
	if len(rows) != 1 {
		t.Fatalf("collectGrokLocal returned %d rows, want 1", len(rows))
	}
	if rows[0].Status != "busy" {
		t.Errorf("Status = %q, want busy", rows[0].Status)
	}
	if *scans != 0 {
		t.Fatalf("fallback scan ran %d times for an events-only session, want 0", *scans)
	}
}

func TestReadGrokEventsTailCapsTheRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, grokEventsFile)
	// Far larger than the tail window, so a whole-file ReadFile would fail
	// the length check below.
	var b strings.Builder
	line := `{"type":"phase_changed","phase":"streaming_text"}` + "\n"
	for b.Len() < grokEventsTailSize*4 {
		b.WriteString(line)
	}
	b.WriteString(`{"type":"turn_ended","outcome":"completed"}` + "\n")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	data := readGrokEventsTail(path)
	if len(data) == 0 {
		t.Fatal("readGrokEventsTail returned nothing")
	}
	if len(data) > grokEventsTailSize {
		t.Fatalf("readGrokEventsTail returned %d bytes, cap is %d", len(data), grokEventsTailSize)
	}
	status, _ := grokStatusFromEvents(data)
	if status != "idle" {
		t.Errorf("status from tail = %q, want idle (last event is turn_ended)", status)
	}
}
