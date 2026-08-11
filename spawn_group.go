package main

import (
	"os/exec"
	"time"
)

// Group assignment for a freshly spawned session.
//
// A spawn hands back a tmux session name, but the flags store (session_flags.go)
// is keyed by session id — and that id only exists once the new claude process
// has written its own ~/.claude/sessions/<pid>.json, which happens some seconds
// after `tmux new-session` returns (longer still when a first-run workspace
// trust dialog is in the way; see dismissTrustPrompt). So the group a caller
// asked for at spawn time cannot be applied until that file shows up, which is
// what resolveSpawnedSessionID waits for.
//
// Everything here is best effort and reports a warning, never an error: the
// session was created and is usable, and a missing group badge is not worth
// turning a spawn that already happened into a failure the caller might retry.

// spawnGroupMin/spawnGroupMax bound the group a spawn may ask for — the same
// 1..9 range the TUI's digit keys and POST /sessions/{pid}/flags accept. 0
// ("ungrouped") is how a caller omits the field, not something it may request:
// there is nothing to clear on a session that did not exist a moment ago.
const (
	spawnGroupMin = 1
	spawnGroupMax = 9
)

// validSpawnGroup reports whether n names a group a spawn may request.
func validSpawnGroup(n int) bool { return n >= spawnGroupMin && n <= spawnGroupMax }

// spawnGroupPollInterval/spawnGroupTimeout bound the wait for the spawned
// session to appear. Vars rather than consts so tests can shrink them instead
// of spending the real timeout, exactly like trustPromptPollInterval /
// trustPromptTimeout (migrate.go) next door.
var (
	spawnGroupPollInterval = 500 * time.Millisecond
	spawnGroupTimeout      = 15 * time.Second
)

// spawnGroupCollect is the seam resolveSpawnedSessionID observes this host
// through. Its default is the real collector — the readResumableHeadFn
// precedent, not the keychainRead one — because nothing behind it is
// destructive: a test that forgets to override it reads session files and
// finds no match, it does not touch anything.
var spawnGroupCollect = CollectLocal

// spawnGroupTmuxAlive reports whether the tmux session a spawn created is
// still there. It is what stops a command that died on its first line — a bad
// preset, a shell that exits immediately — from costing the full
// spawnGroupTimeout: there is no session coming, so there is nothing to wait
// for.
//
// The plain `-t name` form is used rather than tmux's exact `-t =name`: older
// tmux matches a prefix there, and a prefix match can only ever say "still
// alive", which keeps waiting. Erring toward the wait is the safe direction —
// the early bail must never fire on a session that is merely slow.
var spawnGroupTmuxAlive = func(tname string) bool {
	return exec.Command("tmux", "has-session", "-t", tname).Run() == nil
}

// The two ways a requested group fails to land. Both begin by saying the spawn
// itself worked, because that is the part the caller must not misread: the CLI
// has already printed the tmux name to stdout by the time one of these reaches
// stderr, and the server sends them on a success response.
const (
	spawnGroupNotSeenWarning  = "spawned OK, group not set: session did not appear in time"
	spawnGroupNotSavedWarning = "spawned OK, group not set: this host could not save session flags"
)

// setGroupAfterSpawn assigns group to whatever claude session comes up inside
// the tmux session tname, waiting up to spawnGroupTimeout for it to appear.
// Returns "" when the group is on disk, otherwise the one-line warning the
// caller shows the user. group 0 is "no group asked for" and does nothing.
func setGroupAfterSpawn(store *FlagsStore, tname string, group int) string {
	if group == 0 {
		return ""
	}
	if tname == "" {
		// A spawn that reported success without naming its session: there is
		// nothing to resolve the group against, and silence is the one answer
		// this function never gives when a group was actually asked for.
		return spawnGroupNotSeenWarning
	}
	if store == nil {
		// No store to write to (no config directory, or a caller that has
		// none) — say so now rather than after a pointless wait.
		return spawnGroupNotSavedWarning
	}
	id, ok := resolveSpawnedSessionID(tname)
	if !ok {
		return spawnGroupNotSeenWarning
	}
	if !store.SetGroup(id, group) {
		return spawnGroupNotSavedWarning
	}
	return ""
}

// resolveSpawnedSessionID polls this host for the session whose tmux pane sits
// in the tmux session named tname, and returns its session id.
//
// The collector is asked once before any sleeping, so a caller that has
// already waited (the CLI dismisses the trust prompt first) usually pays
// nothing. A collection failure is treated like "not there yet" rather than
// aborting: ps or tmux hiccupping once should not lose the group.
//
// A tmux session that is gone ends the wait immediately: the command died on
// its first line and no session id is ever going to appear, so the remaining
// timeout would be spent watching nothing.
//
// A tmux session running more than one claude pane resolves to whichever the
// collector lists first. That cannot happen to a session this call spawned —
// it is seconds old and has exactly one pane — and picking one of two is a
// better answer than picking none.
func resolveSpawnedSessionID(tname string) (string, bool) {
	deadline := time.Now().Add(spawnGroupTimeout)
	for {
		sessions, err := spawnGroupCollect()
		if err == nil {
			for _, s := range sessions {
				if s.SessionID == "" {
					continue
				}
				if name, err := tmuxSessionName(s.Tmux); err == nil && name == tname {
					return s.SessionID, true
				}
			}
		}
		if !spawnGroupTmuxAlive(tname) {
			return "", false
		}
		if !time.Now().Before(deadline) {
			return "", false
		}
		time.Sleep(spawnGroupPollInterval)
	}
}
