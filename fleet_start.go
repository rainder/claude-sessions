package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const (
	// fleetStartGap: one start attempt per minute. A start that spawned a
	// session needs seconds before claude writes its session file, and a
	// second tap in that window would see "not running" and start a second
	// FLEET.
	fleetStartGap = 60 * time.Second
	// fleetStartingFor: how long /fleet/state reports a start as pending
	// while FLEET has not shown up yet.
	fleetStartingFor = 90 * time.Second
	fleetStartPreset = "Opus"
	// fleetStartFallback is the command prefix when no "Opus" preset exists.
	fleetStartFallback = "claude --model=opus"
)

type fleetStarting struct {
	At   string `json:"at"`
	Mode string `json:"mode"`
}

type fleetStartResult struct {
	OK         bool   `json:"ok"`
	Mode       string `json:"mode,omitempty"`
	Tmux       string `json:"tmux,omitempty"`
	Code       string `json:"code,omitempty"`
	Error      string `json:"error,omitempty"`
	RetryAfter int    `json:"retryAfter,omitempty"`
	// LastMissing: fleet-last.json named a session, but its transcript or
	// folder is gone, so this started a fresh FLEET instead.
	LastMissing bool `json:"lastMissing,omitempty"`
}

func (s *server) fleetTmuxAlive(name string) bool {
	if s.fleet.tmuxAlive != nil {
		return s.fleet.tmuxAlive(name)
	}
	return spawnGroupTmuxAlive(name)
}

// fleetStartPending reports whether the last web start may still become
// FLEET: inside the 90 s window, or its tmux session still exists and FLEET
// has not appeared. A claude stuck at a trust dialog is still that start; a
// second spawn beside it would make two FLEETs. alive (FLEET seen) or a gone
// tmux session ends it for good. Callers must not hold startStateMu.
func (s *server) fleetStartPending(alive bool) (pending bool, starting *fleetStarting) {
	f := &s.fleet
	now := s.fleetNow()
	f.startStateMu.Lock()
	if alive {
		f.startingAt, f.startingMode, f.startedTmux = time.Time{}, "", ""
	}
	if !f.startingAt.IsZero() && now.Sub(f.startingAt) >= fleetStartingFor {
		f.startingAt, f.startingMode = time.Time{}, ""
	}
	if !f.startingAt.IsZero() {
		starting = &fleetStarting{At: f.startingAt.UTC().Format(time.RFC3339), Mode: f.startingMode}
		f.startStateMu.Unlock()
		return true, starting
	}
	tname := f.startedTmux
	f.startStateMu.Unlock()
	if tname == "" {
		return false, nil
	}
	// has-session runs outside the lock: a poll must not hold it on a fork.
	if s.fleetTmuxAlive(tname) {
		return true, nil
	}
	f.startStateMu.Lock()
	if f.startedTmux == tname {
		f.startedTmux = ""
	}
	f.startStateMu.Unlock()
	return false, nil
}

// fleetStartInfo fills fl's canStart, starting and lastLive. sessions is the
// list discovery used (nil when it failed).
func (s *server) fleetStartInfo(fl *fleetSession, sessions []Session) {
	pending, starting := s.fleetStartPending(fl.Alive)
	fl.Starting = starting
	if fl.Code == "not_live" {
		if l, ok := readFleetLast(s.fleetLastPath()); ok {
			fl.LastLive = sessionIDLive(sessions, l.SessionID)
		}
	}
	f := &s.fleet
	now := s.fleetNow()
	f.startStateMu.Lock()
	defer f.startStateMu.Unlock()
	fl.CanStart = fl.Code == "not_live" && !fl.LastLive && !pending && !f.startInFlight &&
		(f.lastStartAt.IsZero() || now.Sub(f.lastStartAt) >= fleetStartGap)
}

func sessionIDLive(sessions []Session, sid string) bool {
	for _, sess := range sessions {
		if sess.SessionID == sid {
			return true
		}
	}
	return false
}

func (s *server) fleetDefaultCwd() (string, error) {
	if s.fleet.defaultCwd != "" {
		return s.fleet.defaultCwd, nil
	}
	home, err := s.fleetHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Developer", "trecs-brain"), nil
}

// fleetStartPrefix is the "Opus" command preset, the same presets `new`
// offers, or fleetStartFallback when there is none.
func (s *server) fleetStartPrefix() string {
	load := s.fleet.presets
	if load == nil {
		load = LoadCommandPresets
	}
	if presets, err := load(); err == nil {
		if p, ok := findCommandPreset(presets, fleetStartPreset); ok && p.Command != "" {
			return p.Command
		}
	}
	return fleetStartFallback
}

// fleetStart is POST /fleet/start: start FLEET in a new tmux session. It
// resumes the session in fleet-last.json when its transcript and folder still
// exist, else starts a fresh one running /fleet. Guards, in order: auth
// (cookie needs same origin), one attempt per minute, a fresh (uncached)
// discovery that refuses when FLEET runs or two sessions claim the name, a
// still-pending earlier start, and the last session running under another
// name.
func (s *server) fleetStart(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	kind := s.fleetAuth(r)
	if kind == fleetAuthNone {
		writeJSON(w, http.StatusUnauthorized, fleetStartResult{Code: "unauthorized", Error: "Unauthorized"})
		return
	}
	if kind == fleetAuthCookie && !sameOrigin(r) {
		writeJSON(w, http.StatusForbidden, fleetStartResult{Code: "origin", Error: "origin mismatch"})
		return
	}
	// The body carries nothing; read it only to bound it.
	if _, err := io.Copy(io.Discard, http.MaxBytesReader(w, r.Body, 4<<10)); err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, fleetStartResult{Code: "bad_request", Error: "body too large"})
		return
	}

	f := &s.fleet
	f.startMu.Lock()
	defer f.startMu.Unlock()
	f.startStateMu.Lock()
	f.startInFlight = true
	last := f.lastStartAt
	f.startStateMu.Unlock()
	defer func() {
		f.startStateMu.Lock()
		f.startInFlight = false
		f.startStateMu.Unlock()
	}()

	now := s.fleetNow()
	if !last.IsZero() && now.Sub(last) < fleetStartGap {
		wait := int((fleetStartGap - now.Sub(last) + time.Second - 1) / time.Second)
		w.Header().Set("Retry-After", strconv.Itoa(wait))
		writeJSON(w, http.StatusTooManyRequests, fleetStartResult{Code: "rate_limited", RetryAfter: wait,
			Error: fmt.Sprintf("A start was tried less than a minute ago. Try again in %d s.", wait)})
		return
	}

	// Fresh, not cached: the cached list can be a second old, and FLEET may
	// have been started from the terminal since.
	sessions, err := s.collectLocal()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, fleetStartResult{Code: "start_failed", Error: "session list failed"})
		return
	}
	_, reason := findFleet(sessions)
	if pending, _ := s.fleetStartPending(reason == ""); pending {
		writeJSON(w, http.StatusConflict, fleetStartResult{Code: "starting",
			Error: "FLEET is still starting. Wait, or check its tmux session."})
		return
	}
	switch reason {
	case "":
		writeJSON(w, http.StatusConflict, fleetStartResult{Code: "already_running", Error: "FLEET is already running"})
		return
	case "ambiguous":
		writeJSON(w, http.StatusConflict, fleetStartResult{Code: "ambiguous", Error: "two sessions are named FLEET"})
		return
	}
	l, resumeCwd, valid, resumable := s.fleetLastPlan()
	// Resuming a live id would give two processes one transcript, and a fresh
	// FLEET beside it would be a second FLEET.
	if valid && sessionIDLive(sessions, l.SessionID) {
		writeJSON(w, http.StatusConflict, fleetStartResult{Code: "last_live",
			Error: "the last FLEET session is running without the FLEET name"})
		return
	}

	f.startStateMu.Lock()
	f.lastStartAt = now
	f.startStateMu.Unlock()

	prefix := s.fleetStartPrefix()
	mode, cwd, command, sid := "fresh", "", "", ""
	if resumable {
		// l.SessionID is a UUID (readFleetLast), so it is safe to put in a
		// shell command unquoted.
		mode, cwd, sid = "resume", resumeCwd, l.SessionID
		command = prefix + " --resume " + sid + " -n " + fleetSessionName
	} else {
		cwd, err = s.fleetDefaultCwd()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, fleetStartResult{Code: "start_failed", Error: "no home directory"})
			return
		}
		if st, err := os.Stat(cwd); err != nil || !st.IsDir() {
			writeJSON(w, http.StatusInternalServerError, fleetStartResult{Code: "start_failed",
				Error: "the default FLEET folder does not exist"})
			return
		}
		command = prefix + " -n " + fleetSessionName + " " + shellQuote("/fleet")
	}
	lastMissing := valid && !resumable

	spawn := f.spawn
	if spawn == nil {
		spawn = SpawnNew
	}
	tname, err := spawn(cwd, fleetSessionName, command)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fleet: start (%s) failed: %v\n", mode, err)
		writeJSON(w, http.StatusInternalServerError, fleetStartResult{Code: "start_failed", Error: "could not start FLEET"})
		return
	}
	text := "Started a new FLEET session"
	switch {
	case mode == "resume":
		text = "Resumed the last FLEET session"
	case lastMissing:
		text = "Started a new FLEET session (the last one could not be resumed)"
	}
	if err := s.appendFleetLog(fleetLogEntry{At: now.UTC().Format(time.RFC3339), Type: "web-start", Text: text,
		Mode: mode, SessionID: sid, Tmux: tname}); err != nil {
		fmt.Fprintf(os.Stderr, "fleet: web-start log append failed: %v\n", err)
	}
	f.startStateMu.Lock()
	f.startingAt, f.startingMode, f.startedTmux = now, mode, tname
	f.startStateMu.Unlock()
	s.invalidateSessions()
	writeJSON(w, http.StatusOK, fleetStartResult{OK: true, Mode: mode, Tmux: tname, LastMissing: lastMissing})
}
