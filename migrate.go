package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// MakeTmuxName generates a tmux-safe session name with a 6-char suffix to
// guarantee uniqueness against any existing session.
//
//	name   — user-set display name (preferred when present)
//	cwd    — falls back to basename of cwd
//	suffix — 6 chars of session id or random
func MakeTmuxName(cwd, suffix, name string) string {
	var base string
	switch {
	case name != "":
		base = sanitizeForTmux(name)
	case cwd != "" && cwd != "/":
		base = sanitizeForTmux(filepath.Base(strings.TrimRight(cwd, "/")))
	default:
		base = "claude"
	}
	if len(suffix) > 6 {
		suffix = suffix[:6]
	}
	if suffix == "" {
		suffix = randomSlug()
	}
	return base + "-" + suffix
}

// randomSlug returns 6 hex chars. Used as a tmux-name suffix for `cmd new`
// where there's no session id yet.
func randomSlug() string {
	b := make([]byte, 3)
	if _, err := rand.Read(b); err != nil {
		// Fallback to time-based — never panic on a non-essential helper.
		return fmt.Sprintf("%06x", time.Now().UnixNano()&0xffffff)
	}
	return hex.EncodeToString(b)
}

// newSpawnRequestID returns a fresh idempotency key for /sessions/new's
// optional request_id, sized to satisfy validSpawnRequestID (server.go:389,
// 8-128 chars of [A-Za-z0-9_-]). randomSlug's 6 hex chars are too short and
// serve a different purpose (a tmux-name suffix) — this gets its own helper
// rather than widening that one's contract.
func newSpawnRequestID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%032x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// migrateSignal/migrateAlive/migrateSleep are MigrateLocal's side effects,
// injectable so the kill-then-verify sequence can be tested without signalling a
// real process or spending its waits in real time.
var (
	migrateSignal = syscall.Kill
	migrateAlive  = pidPresent
	migrateSleep  = time.Sleep
)

// pidPresent reports whether pid still exists, and unlike pidAlive it treats
// EPERM as present.
//
// kill(pid, 0) answers EPERM when the process is there but this user may not
// signal it. pidAlive folds that into "gone", which is the right default for
// listing sessions — a process we cannot touch is not one of ours — and the
// wrong one for deciding a migrate may proceed, where "gone" is the permission
// to create a second session on the same transcript. Here the dangerous
// direction is the optimistic one, so anything short of a definite ESRCH counts
// as alive.
func pidPresent(pid int) bool {
	return pidPresentErr(syscall.Kill(pid, 0))
}

// pidPresentErr is the classification pidPresent applies, split out so it can be
// tested without a real process in either state.
func pidPresentErr(err error) bool {
	if err == nil {
		return true
	}
	return !errors.Is(err, syscall.ESRCH)
}

// errMigrateSessionMismatch is returned when the session file re-read no longer
// holds the id the caller attested. The server handler maps it to the same
// machine-readable code its own earlier check uses, so a client sees one failure
// kind regardless of which of the two checks caught the substitution.
var errMigrateSessionMismatch = errors.New("that PID is a different session now")

// MigrateLocal stops the Claude process at pid and spawns a new tmux session
// running `claude --resume <sessionId>` in the same cwd. Returns the tmux
// session name on success.
func MigrateLocal(pid int) (string, error) {
	return MigrateLocalAttested(pid, "")
}

// MigrateLocalAttested is MigrateLocal with the session id the caller already
// verified for this PID.
//
// The re-read below is not redundant with a caller's check. Whoever resolved
// this PID did so against a list that took real I/O to build, and this function
// then re-reads the session file and would otherwise adopt whatever it now says
// — so a pane recycled in between would be killed *and* have its transcript
// resumed under the caller's intent. Passing the attested id makes the re-read
// verify rather than adopt. "" keeps the pre-existing unconditional behaviour
// for the local TUI and CLI paths, which resolve the PID and act on it within
// the same keystroke.
func MigrateLocalAttested(pid int, wantSession string) (string, error) {
	sess, ok := readSessionByPID(pid)
	if !ok {
		return "", fmt.Errorf("no session file for PID %d", pid)
	}
	if sess.SessionID == "" || sess.CWD == "" {
		return "", fmt.Errorf("session file missing sessionId or cwd")
	}
	if wantSession != "" && sess.SessionID != wantSession {
		return "", fmt.Errorf("%w: PID %d", errMigrateSessionMismatch, pid)
	}

	tname := MakeTmuxName(sess.CWD, sess.SessionID, sess.Name)

	// SIGTERM, wait up to 5s, then SIGKILL fallback.
	_ = migrateSignal(pid, syscall.SIGTERM)
	for i := 0; i < 5; i++ {
		migrateSleep(time.Second)
		if !migrateAlive(pid) {
			break
		}
	}
	if migrateAlive(pid) {
		_ = migrateSignal(pid, syscall.SIGKILL)
		migrateSleep(time.Second)
	}
	// Confirm it actually died. Both signals above discard their errors — a
	// permission failure or a PID that changed hands under us reads exactly like
	// success — and proceeding here would leave the old process running while a
	// second one resumes the same transcript. Two live sessions on one
	// transcript corrupt it; a failed migrate does not.
	if migrateAlive(pid) {
		return "", fmt.Errorf("PID %d is still running; not migrating", pid)
	}
	migrateSleep(time.Second) // let state flush to disk

	cols, rows := resolveSpawnSize()
	if err := tmuxNewDetachedSession(tname, sess.CWD, cols, rows); err != nil {
		return "", fmt.Errorf("tmux new-session: %w", err)
	}
	if err := exec.Command("tmux", "send-keys", "-t", tname,
		"claude --resume "+sess.SessionID, "Enter").Run(); err != nil {
		// Same partial commit SpawnNew guards against: the session exists but
		// was never told what to run.
		killTmuxSession(tname)
		return "", fmt.Errorf("tmux send-keys: %w", err)
	}
	return tname, nil
}

// SpawnNew creates a fresh tmux session at cwd and sends command to it inside
// the user's shell. Returns the tmux session name.
func SpawnNew(cwd, displayName, command string) (string, error) {
	return SpawnNewWithSuffix(cwd, displayName, command, "")
}

// SpawnNewWithSuffix is SpawnNew with a caller-chosen tmux name suffix, so a
// retry of the same logical request lands on the same session name. "" keeps the
// random slug, which is what every local caller wants.
func SpawnNewWithSuffix(cwd, displayName, command, suffix string) (string, error) {
	tname := MakeTmuxName(cwd, suffix, displayName)
	cols, rows := resolveSpawnSize()
	if err := tmuxNewDetachedSession(tname, cwd, cols, rows); err != nil {
		return "", fmt.Errorf("tmux new-session: %w", err)
	}
	if err := exec.Command("tmux", "send-keys", "-t", tname, command, "Enter").Run(); err != nil {
		// Partial commit: the session exists but was never told what to run, and
		// this call is about to report failure. Callers are entitled to treat a
		// failed spawn as "nothing was created" — the request_id ledger
		// deliberately forgets failures so a retry genuinely re-runs — so leaving
		// this behind would make the retry create a second session. Tear it down.
		killTmuxSession(tname)
		return "", fmt.Errorf("tmux send-keys: %w", err)
	}
	// A brand-new tmux session may be what started the tmux server; (re)assert
	// the Ctrl+V paste binding on it. Linux-only no-op elsewhere.
	installPasteBinding(activeServerPort)
	return tname, nil
}

// tmuxNewDetachedSession creates a detached tmux session named name at cwd,
// sized cols×rows.
//
// The size is not cosmetic. A session nobody has attached to yet takes tmux's
// `default-size` (80x24) until a real client arrives, so a spawned, migrated or
// resumed session that is only ever *previewed* stays 80x24 forever — a tiny
// buffer rendered inside the much larger inspector frame. Sizing it at creation
// is the only fix available: `tmux resize-window` afterwards would set the
// window to manual sizing permanently, overriding the normal auto-resize when a
// client does eventually attach.
//
// Non-positive dimensions omit the flags and leave tmux's default in place,
// which is a worse pane but never a failed spawn.
func tmuxNewDetachedSession(name, cwd string, cols, rows int) error {
	args := []string{"new-session", "-d", "-s", name, "-c", cwd}
	if cols > 0 && rows > 0 {
		args = append(args, "-x", strconv.Itoa(cols), "-y", strconv.Itoa(rows))
	}
	return exec.Command("tmux", args...).Run()
}

// killTmuxSession tears down a tmux session created moments ago by a spawn or
// migrate that then failed. Best effort: the caller is already returning an
// error and the cleanup failing does not change what it reports.
func killTmuxSession(tname string) {
	// Bounded: this runs on a path that is already returning an error, and a
	// tmux that hangs here would hold the caller — and, for a spawn, the
	// request_id slot every joiner is waiting on — open for as long as it hangs.
	ctx, cancel := context.WithTimeout(context.Background(), tmuxCleanupTimeout)
	defer cancel()
	if err := exec.CommandContext(ctx, "tmux", "kill-session", "-t", tname).Run(); err != nil {
		// Loud on purpose. The caller reports failure and the request_id ledger
		// forgets failures, so nothing else will ever mention this session
		// again; the name is what a human needs to clean it up by hand. A retry
		// of the same request_id will collide with it rather than duplicate it
		// (see spawnSuffix), which is the safe outcome but still needs saying.
		fmt.Fprintf(os.Stderr, "claude-sessions: could not clean up tmux session %q after a failed spawn: %v\n", tname, err)
	}
}

// tmuxCleanupTimeout bounds the best-effort teardown above.
var tmuxCleanupTimeout = 5 * time.Second

// trustPromptMarker is unique text from Claude Code's first-run workspace
// trust dialog ("Is this a project you created or one you trust?"). A
// directory that has never been opened with Claude before shows this dialog
// and waits for a keypress before doing anything else — including consuming
// a seeded initial prompt passed as a CLI argument.
const trustPromptMarker = "Yes, I trust this folder"

// trustPromptPollInterval/trustPromptTimeout bound dismissTrustPrompt's poll
// loop. Vars rather than consts so tests can shrink them instead of eating
// the full real-time timeout on every run.
var (
	trustPromptPollInterval = 250 * time.Millisecond
	trustPromptTimeout      = 3 * time.Second
)

// dismissTrustPrompt polls the tmux pane for up to trustPromptTimeout and, if the workspace
// trust dialog appears, accepts its default "trust this folder" option so the
// session can proceed to the prompt it was launched with. It's a no-op
// (settling quickly) when the dialog never shows, e.g. because the directory
// was already trusted by an earlier session.
//
// Only the background/prompt spawn path calls this: an attached launch has
// the user right there to see and decide on the dialog themselves within a
// second or two, same as before this existed. An unattended background
// launch has nobody watching, so without this it hangs at the dialog forever
// and the seeded prompt never reaches Claude — call it in a goroutine right
// after a successful SpawnNew so the caller doesn't block waiting on it.
func dismissTrustPrompt(tname string) {
	deadline := time.Now().Add(trustPromptTimeout)
	for time.Now().Before(deadline) {
		time.Sleep(trustPromptPollInterval)
		out, err := exec.Command("tmux", "capture-pane", "-t", tname, "-p").Output()
		if err != nil {
			return // session already gone; nothing to dismiss
		}
		if strings.Contains(string(out), trustPromptMarker) {
			_ = exec.Command("tmux", "send-keys", "-t", tname, "Enter").Run()
			return
		}
	}
}

// killDeps are the side-effecting operations KillSession performs, injected so
// the kill routing can be tested without signalling a real PID or sleeping.
type killDeps struct {
	killTmux func(string) error
	signal   func(int, syscall.Signal) error
	alive    func(int) bool
	sleep    func(time.Duration)
}

// defaultKillDeps wires the production side effects.
var defaultKillDeps = killDeps{
	killTmux: func(name string) error {
		return exec.Command("tmux", "kill-session", "-t", name).Run()
	},
	signal: syscall.Kill,
	alive:  pidAlive,
	sleep:  time.Sleep,
}

// tmuxSessionName extracts the tmux session name from a "session:win.pane"
// location string. Malformed metadata (no colon, or an empty session name) is a
// hard error so callers never guess at a target.
func tmuxSessionName(tmux string) (string, error) {
	i := strings.IndexByte(tmux, ':')
	if i <= 0 {
		return "", fmt.Errorf("malformed tmux metadata %q", tmux)
	}
	return tmux[:i], nil
}

// KillSession kills the Claude session using the session's own trusted metadata
// (no live re-discovery): if s.Tmux is set, kill the whole tmux session (which
// SIGHUPs the pane process); otherwise SIGTERM the pid, escalating to SIGKILL
// after a few seconds.
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
		// tmux kill-session returns as soon as tmux tears down the pane, not
		// once the pane's process has actually exited (it SIGHUPs and moves
		// on). A caller that checks worktree survivors right after — see
		// worktreeCleanupTarget / removeWorktree — needs the PID to actually
		// be gone, so wait for it here instead of racing.
		for i := 0; i < 5; i++ {
			if !deps.alive(s.PID) {
				return nil
			}
			deps.sleep(time.Second)
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

// localReattest re-reads the session file for pid immediately before a
// destructive local action (kill/migrate) and compares it against the
// session identity the caller last observed. Mirrors the server's reattest
// (server.go:618) — same two refusal cases, just in-process instead of over
// HTTP: the caller's snapshot can be however old its confirmation dialog
// took to resolve, and the PID can have been recycled to a different
// session in that window. An empty wantSessionID skips the check —
// matches sessionIDPrecondition's own "absent means no precondition"
// contract, not a new policy invented for this path.
func localReattest(pid int, wantSessionID string) error {
	if wantSessionID == "" {
		return nil
	}
	sess, ok := readSessionByPID(pid)
	if !ok {
		return fmt.Errorf("PID %d is not a live Claude session", pid)
	}
	if sess.SessionID != wantSessionID {
		return fmt.Errorf("PID %d is a different session now", pid)
	}
	return nil
}

// readSessionByPID reads a single ~/.claude/sessions/<pid>.json file.
// Returns ok=false if missing or malformed.
func readSessionByPID(pid int) (Session, bool) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Session{}, false
	}
	return readSessionFile(filepath.Join(home, ".claude", "sessions", strconv.Itoa(pid)+".json"))
}
