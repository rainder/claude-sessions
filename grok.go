package main

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// toolGrok is the Session.Tool value for a session owned by the xAI Grok CLI.
// The empty string means Claude Code — every session file written before this
// field existed, and every row an older server sends over the wire, decodes as
// claude with no migration step.
const toolGrok = "grok"

// grokDir is the Grok CLI's state directory under the collector's home.
const grokDir = ".grok"

// grokActiveSessionsFile is the Grok CLI's live-session registry: a JSON array
// with one entry per running session, rewritten whenever a session opens or
// closes. It sits beside an active_sessions.lock that grok takes while it
// writes; this collector never touches that lock — see collectGrokLocal for
// why a torn read is treated as "no grok sessions" instead of an error.
const grokActiveSessionsFile = "active_sessions.json"

// grokActiveSession is one entry of active_sessions.json.
type grokActiveSession struct {
	SessionID string `json:"session_id"`
	PID       int    `json:"pid"`
	CWD       string `json:"cwd"`
	OpenedAt  string `json:"opened_at"` // RFC 3339, nanosecond precision
}

// grokSummary is the subset of ~/.grok/sessions/<cwd>/<id>/summary.json this
// tool reads. The file carries a good deal more (git remotes, head commit,
// prompt ids, sandbox profile); everything not listed here is deliberately
// ignored so a schema addition on grok's side is never a parse failure.
type grokSummary struct {
	SessionSummary string `json:"session_summary"`
	GeneratedTitle string `json:"generated_title"`
	CurrentModelID string `json:"current_model_id"`
	LastActiveAt   string `json:"last_active_at"`
	UpdatedAt      string `json:"updated_at"`
}

// grokPIDAlive is collectGrokLocal's liveness check, injectable so the
// collector's tests can describe a dead pid without needing a real one. Same
// seam pattern as migrate.go's migrateAlive.
var grokPIDAlive = pidAlive

// collectGrokLocal reads the Grok CLI's live-session registry under home and
// returns one Session per running grok session. CPU, tmux, Home and GitRoot
// are deliberately left zero: CollectLocal enriches these rows through the
// same shared pass it runs over claude rows, so a grok row carries the pane
// address every action (attach, preview, send-keys, resize, kill) depends on.
//
// It never returns an error. A missing ~/.grok, an unreadable file, and a
// half-written one (grok rewrites the whole array under its own lock, which
// this reader deliberately does not take) all mean the same thing to a caller
// that is really trying to list claude sessions: no grok rows this pass. The
// alternative — failing CollectLocal — would let a torn read of a file this
// tool does not own blank the entire session list.
func collectGrokLocal(home string) []Session {
	entries, ok := readGrokRegistry(home)
	if !ok {
		return nil
	}
	out := make([]Session, 0, len(entries))
	for _, e := range entries {
		if s, ok := grokSessionFrom(home, e); ok {
			out = append(out, s)
		}
	}
	return out
}

// grokSessionFrom turns one registry entry into a Session, applying the same
// visibility filters CollectLocal applies to claude rows. ok=false means the
// entry describes nothing worth showing: a session that has already exited, a
// malformed entry, or a scratch cwd.
func grokSessionFrom(home string, e grokActiveSession) (Session, bool) {
	if e.PID == 0 || e.SessionID == "" {
		return Session{}, false
	}
	if !grokPIDAlive(e.PID) {
		return Session{}, false
	}
	if isScratchCWD(e.CWD) {
		return Session{}, false
	}
	s := Session{
		Tool:      toolGrok,
		PID:       e.PID,
		SessionID: e.SessionID,
		CWD:       e.CWD,
		// The Grok CLI is interactive, so these rows are the grok equivalent
		// of claude's "cli" entrypoint — never a headless run, which is what
		// Session.Headless keys off to dim a row.
		Entrypoint: "cli",
		StartedAt:  grokMillis(e.OpenedAt),
	}
	// An unparseable or absent opened_at leaves StartedAt at 0, and a session
	// with no summary either has nothing else to derive an age from — so
	// Session.Updated() would answer the epoch and the AGE column would read
	// ~20679d. Stamping collection time instead makes the row say "just seen",
	// which is both true and the least misleading thing available.
	if s.StartedAt == 0 {
		s.StartedAt = time.Now().UnixMilli()
	}
	// Everything below is best effort: a session whose summary.json has not
	// been written yet (grok writes it on the first turn) still renders as a
	// row with its pid, cwd and age.
	if sum, ok := readGrokSummary(home, e.CWD, e.SessionID); ok {
		s.Name = grokSummaryName(sum)
		// The title is grok's own generated one, never something the user
		// typed, so it takes claude's "derived" marker and renders dimmed.
		if s.Name != "" {
			s.NameSource = "derived"
		}
		s.Model = sum.CurrentModelID
		s.UpdatedAt = grokMillis(firstNonEmpty(sum.LastActiveAt, sum.UpdatedAt))
	}
	return s, true
}

// grokSummaryName resolves the NAME label from a summary: grok's generated
// title first, its rolling session summary second. "" falls through to
// Session.DisplayName's own chain (worktree name, then "-"), which is what
// every claude row with no name of its own already does.
func grokSummaryName(sum grokSummary) string {
	return firstNonEmpty(sum.GeneratedTitle, sum.SessionSummary)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// grokMillis parses one of grok's RFC 3339 timestamps into the millis-since-
// epoch form Session carries. An unparseable or absent stamp yields 0, which
// every caller treats as "unknown": Session.Updated falls back to StartedAt
// for a zero UpdatedAt, and grokSessionFrom stamps collection time for a zero
// StartedAt rather than let the AGE column render the epoch.
func grokMillis(ts string) int64 {
	if ts == "" {
		return 0
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return 0
	}
	return t.UnixMilli()
}

// readGrokSummary loads the per-session metadata grok keeps at
// ~/.grok/sessions/<encoded-cwd>/<session-id>/summary.json.
func readGrokSummary(home, cwd, sessionID string) (grokSummary, bool) {
	path, ok := grokSummaryPath(home, cwd, sessionID)
	if !ok {
		return grokSummary{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return grokSummary{}, false
	}
	var sum grokSummary
	if err := json.Unmarshal(data, &sum); err != nil {
		return grokSummary{}, false
	}
	return sum, true
}

// grokSessionsReadDir lists the per-cwd session directories for the fallback
// scan below, injectable so a test can prove how often that scan actually
// runs — which is the whole cost question this function is shaped around.
var grokSessionsReadDir = os.ReadDir

// grokScanned memoizes a per-cwd directory name the fallback scan resolved,
// keyed by "<sessions root>\x00<cwd>" so two homes (and two tests) never share
// an entry. Successes only, never pruned: it can only ever hold one short
// string per cwd whose encoding this tool could not derive, which is zero
// entries on every grok release seen so far.
var (
	grokScanMu  sync.Mutex
	grokScanned = map[string]string{}
)

func grokScanKey(root, cwd string) string { return root + "\x00" + cwd }

func grokScanLookup(root, cwd string) (string, bool) {
	grokScanMu.Lock()
	defer grokScanMu.Unlock()
	name, ok := grokScanned[grokScanKey(root, cwd)]
	return name, ok
}

func grokScanRemember(root, cwd, dirName string) {
	grokScanMu.Lock()
	defer grokScanMu.Unlock()
	grokScanned[grokScanKey(root, cwd)] = dirName
}

// grokSummaryPath locates a session's summary.json.
//
// Grok names the per-cwd directory by percent-encoding the absolute path,
// which url.PathEscape reproduces byte for byte — verified against the live
// directories on this machine, including a worktree path carrying "." and "-"
// (neither is escaped) and every "/" (each becomes %2F). The direct join is
// therefore the fast path and needs no directory listing at all.
//
// The order below exists because CollectLocal runs on a 2s tick and a host can
// hold thousands of per-cwd directories, so a scan on a routine miss would be
// thousands of syscalls per tick — the same failure shape the resume
// collector's laziness exists to avoid. So:
//
//  1. the summary under the derived (or previously scanned) directory name;
//  2. otherwise, if that DIRECTORY exists, the encoding is right and the
//     summary simply has not been written yet — grok writes it on the first
//     turn — so answer not-found without scanning anything;
//  3. only a per-cwd directory that is missing entirely can mean the encoding
//     changed, and only then is the scan worth its cost.
//
// A successful scan is memoized per cwd, so an encoding change costs one scan
// per cwd per process rather than one per pass. A scan that finds nothing is
// deliberately NOT memoized — the directory may appear as soon as grok's first
// turn lands, and a negative memo would hide it for the life of the process.
// That leaves a bounded residual cost: one scan per pass for a brand-new
// session under an unknown encoding, until its directory exists.
//
// The scan uses os.ReadDir and never filepath.Glob — home is data, not a
// pattern, and a home path containing "[" or "*" would otherwise either error
// out or match into sibling directories (same reasoning as
// snapshotAccountNames).
func grokSummaryPath(home, cwd, sessionID string) (string, bool) {
	root := filepath.Join(home, grokDir, "sessions")

	candidates := []string{url.PathEscape(cwd)}
	if name, ok := grokScanLookup(root, cwd); ok && name != candidates[0] {
		candidates = append(candidates, name)
	}
	for _, name := range candidates {
		p := filepath.Join(root, name, sessionID, "summary.json")
		if _, err := os.Stat(p); err == nil {
			return p, true
		}
	}
	// Step 2: a per-cwd directory we already know about means the name is
	// right and only the summary is missing. Nothing to scan for.
	for _, name := range candidates {
		if fi, err := os.Stat(filepath.Join(root, name)); err == nil && fi.IsDir() {
			return "", false
		}
	}

	dirs, err := grokSessionsReadDir(root)
	if err != nil {
		return "", false
	}
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		p := filepath.Join(root, d.Name(), sessionID, "summary.json")
		if _, err := os.Stat(p); err == nil {
			grokScanRemember(root, cwd, d.Name())
			return p, true
		}
	}
	return "", false
}

// grokLiveSessionIDs returns the session id of every live grok session under
// home, and nothing else. It is deliberately cheaper than collectGrokLocal:
// one registry read plus the liveness check, with no summary.json join at all,
// because its caller (resolvableSessionIDs, session_flags.go) runs on every
// flag write and needs identity only. A missing or unparseable registry yields
// nil, exactly as the collector does.
func grokLiveSessionIDs(home string) []string {
	entries, ok := readGrokRegistry(home)
	if !ok {
		return nil
	}
	var ids []string
	for _, e := range entries {
		if e.SessionID == "" || e.PID == 0 || !grokPIDAlive(e.PID) {
			continue
		}
		ids = append(ids, e.SessionID)
	}
	return ids
}

// readGrokRegistry parses ~/.grok/active_sessions.json under home. ok=false
// covers missing, unreadable and half-written alike — see collectGrokLocal for
// why that is never an error.
func readGrokRegistry(home string) ([]grokActiveSession, bool) {
	data, err := os.ReadFile(filepath.Join(home, grokDir, grokActiveSessionsFile))
	if err != nil {
		return nil, false
	}
	var entries []grokActiveSession
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, false
	}
	return entries, true
}

// grokSessionByPID resolves one live grok session from the registry under
// home. It is the grok counterpart of readSessionByPID (migrate.go) and is
// what a reattestation of a grok row re-reads immediately before a destructive
// act.
//
// Unlike readSessionByPID it also checks liveness, which NARROWS the
// stale-entry window rather than closing it: the check proves some process
// holds the pid, never that the process is this grok session, so an entry left
// behind by a crash whose pid has since been recycled still attests. That is
// the same accepted residual window a claude kill against a session with no
// tmux name already has (see "Known residual windows, accepted" in CLAUDE.md);
// the check is worth having because claude's session file lingering after exit
// never claimed anything about the pid at all, while grok's entry is supposed
// to vanish with the session, so a surviving one is already evidence something
// went wrong.
func grokSessionByPID(home string, pid int) (Session, bool) {
	entries, ok := readGrokRegistry(home)
	if !ok {
		return Session{}, false
	}
	for _, e := range entries {
		if e.PID != pid {
			continue
		}
		return grokSessionFrom(home, e)
	}
	return Session{}, false
}

// grokSessionLookup resolves a pid against this host's own grok registry.
// A package var so tests can answer without a real ~/.grok, following
// preview.go's previewSessionLookup rather than the TestMain-panic seams —
// nothing behind it does anything more dangerous than read a file.
var grokSessionLookup = func(pid int) (Session, bool) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Session{}, false
	}
	return grokSessionByPID(home, pid)
}

// sessionToolLabel names a Session.Tool value for a human-facing message.
// "" is Claude Code, so the wording of every message that predates the Tool
// field is unchanged.
func sessionToolLabel(tool string) string {
	if tool == toolGrok {
		return "grok"
	}
	return "Claude"
}

// lookupLiveSessionByPID resolves a pid to the session that owns it, trying
// claude's per-pid session file first and grok's registry second. The
// scriptable subcommands take a bare pid with no tool named alongside it, so
// this is where they learn which store to trust.
//
// Claude wins only while its own claim is still plausible. readSessionByPID
// checks nothing about liveness — Claude Code leaves the file behind when the
// process exits — so a pid it once used and grok now owns would otherwise
// resolve to the dead claude session, and `kill PID` would act on the wrong
// row. When the claude file is present but the pid is NOT alive and grok's
// registry holds a live session there, grok's row is the truthful one. A dead
// claude pid with no grok session at all still resolves to the claude row,
// exactly as before: that is the shape `migrate` legitimately uses to resume a
// session whose process died.
func lookupLiveSessionByPID(pid int) (Session, bool) {
	s, ok := readSessionByPID(pid)
	if !ok {
		return grokSessionLookup(pid)
	}
	if !sessionPIDAlive(pid) {
		if g, live := grokSessionLookup(pid); live {
			return g, true
		}
	}
	return s, true
}
