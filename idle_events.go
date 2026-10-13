package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Idle events for FLEET. The server watches this host's sessions and, when
// one has sat non-busy long enough, types one tagged status line into FLEET's
// pane — the job FLEET's Monitor loop over idle-stream.sh used to do, minus
// the 30-minute re-arm. Semantics follow that script: one report per idle
// episode, again only after the session goes busy and idle again. Delivery
// goes through the page's own send pipeline (fleetTypeInto), so every guard
// that protects a page send protects this one: it types only into an empty,
// visible input box and presses Enter only once the box holds the text.
//
// Fail closed everywhere: off by default, off on a bad config, nothing sent
// when the target is missing or ambiguous, and a refused send keeps the batch
// for the next poll (not_submitted excepted: see idleSendLocked). A message
// holds pids, a known status word, a plain tmux address, minutes, and a
// quoted card ticket or user-set name, sanitized — never pane or transcript
// text.

const (
	idleBatchMax    = 100 // events kept in a pending batch; later ones wait for room
	idleMessageMax  = 15  // entries in one message; the rest are "+N more"
	idleLabelMax    = 40
	idleLogEvery    = 10 * time.Minute // one log line per repeated refusal code
	idleTestMessage = fleetEventTag + " test: delivery check, no action needed"
)

type idleConfig struct {
	Enabled      bool     `json:"enabled"`
	Target       string   `json:"target"`
	IdleSeconds  int      `json:"idleSeconds"`
	ShellSeconds int      `json:"shellSeconds"`
	PollSeconds  int      `json:"pollSeconds"`
	BatchSeconds int      `json:"batchSeconds"`
	Ignore       []string `json:"ignore"`
}

// idleDefaultIgnore is the ignore list a config without an "ignore" key gets:
// the long-lived sessions Andy asked to keep out of FLEET's idle events. An
// explicit "ignore": [] clears it.
var idleDefaultIgnore = []string{"trecs-brain-1ac893", "trecs-brain-bac5ac"}

func defaultIdleConfig() idleConfig {
	return idleConfig{IdleSeconds: 60, ShellSeconds: 1800, PollSeconds: 30, BatchSeconds: 60,
		Ignore: append([]string(nil), idleDefaultIgnore...)}
}

// idleNameRe is the shape of a target or ignore entry: a tmux session name or
// a session id.
var idleNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// parseIdleConfig reads the config over the defaults. Unknown keys, values
// out of range, and enabled without a target are errors: the caller keeps
// the last good config, so a typo can only ever leave the feature as it was.
func parseIdleConfig(data []byte) (idleConfig, error) {
	c := defaultIdleConfig()
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return idleConfig{}, fmt.Errorf("not valid JSON: %v", err)
	}
	if dec.More() {
		return idleConfig{}, errors.New("trailing data after the JSON object")
	}
	inRange := func(name string, v, lo, hi int) error {
		if v < lo || v > hi {
			return fmt.Errorf("%s must be %d..%d", name, lo, hi)
		}
		return nil
	}
	for _, err := range []error{
		inRange("idleSeconds", c.IdleSeconds, 10, 86400),
		inRange("shellSeconds", c.ShellSeconds, 10, 86400),
		inRange("pollSeconds", c.PollSeconds, 5, 600),
		inRange("batchSeconds", c.BatchSeconds, 30, 3600),
	} {
		if err != nil {
			return idleConfig{}, err
		}
	}
	if c.Target != "" && !idleNameRe.MatchString(c.Target) {
		return idleConfig{}, errors.New("target must be a tmux session name or a session id")
	}
	if c.Enabled && c.Target == "" {
		return idleConfig{}, errors.New("enabled needs a target")
	}
	for _, ig := range c.Ignore {
		if !idleNameRe.MatchString(ig) {
			return idleConfig{}, fmt.Errorf("ignore entry %q is not a tmux session name or a session id", ig)
		}
	}
	return c, nil
}

// idleEvent is one reported episode waiting in the batch. Key and Since
// name the episode, so a send can drop one that ended (went busy, went away,
// or became a new episode) while it waited, and compute minutes at send time.
// Status, Tmux and Label are already sanitized.
type idleEvent struct {
	Key    string    `json:"key"`
	PID    int       `json:"pid"`
	Status string    `json:"status"`
	Tmux   string    `json:"tmux"`
	Since  time.Time `json:"since"`
	Label  string    `json:"label"`
}

// idleStateFile is what survives a restart: the reported episodes (so a
// restart does not report them again), the pending batch, and when the last
// message went out. `since` is not kept: a restart starts the clock again.
type idleStateFile struct {
	Reported    []string    `json:"reported"`
	Batch       []idleEvent `json:"batch"`
	BatchOpened string      `json:"batchOpened,omitempty"`
	LastSent    string      `json:"lastSent,omitempty"`
}

// idleDeps is the idle-event loop's seams and state. The zero value is the
// production configuration.
type idleDeps struct {
	configPath string                // "" → ~/.config/claude-sessions/idle-events.json
	statePath  string                // "" → <state dir>/idle-events-state.json
	logPath    string                // "" → <state dir>/idle-events.log
	now        func() time.Time      // nil → time.Now
	disabled   func(sid string) bool // nil → the flags store's disabled bit
	sleep      func(time.Duration)   // nil → time.Sleep

	mu         sync.Mutex // one tick at a time; guards everything below
	cfg        idleConfig
	cfgSeen    bool // cfgMod/cfgSize describe a file already read
	cfgMissing bool
	cfgMod     time.Time
	cfgSize    int64
	loaded     bool // state file read
	since      map[string]time.Time
	reported   map[string]bool
	batch      []idleEvent
	opened     time.Time
	lastSent   time.Time
	wasOn      bool                 // the last tick saw the feature enabled
	logAt      map[string]time.Time // last log line per refusal code
}

// claudeSessionsStateDir is $XDG_STATE_HOME/claude-sessions, or
// ~/.local/state/claude-sessions; "" with no home.
func claudeSessionsStateDir() string {
	if x := os.Getenv("XDG_STATE_HOME"); filepath.IsAbs(x) {
		return filepath.Join(x, "claude-sessions")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "state", "claude-sessions")
}

func idleConfigPathDefault() string {
	if dir := ConfigDir(); dir != "" {
		return filepath.Join(dir, "idle-events.json")
	}
	return ""
}

func idleStatePathDefault() string {
	if dir := claudeSessionsStateDir(); dir != "" {
		return filepath.Join(dir, "idle-events-state.json")
	}
	return ""
}

func idleLogPathDefault() string {
	if dir := claudeSessionsStateDir(); dir != "" {
		return filepath.Join(dir, "idle-events.log")
	}
	return ""
}

func (d *idleDeps) paths() (cfg, state, log string) {
	cfg, state, log = d.configPath, d.statePath, d.logPath
	if cfg == "" {
		cfg = idleConfigPathDefault()
	}
	if state == "" {
		state = idleStatePathDefault()
	}
	if log == "" {
		log = idleLogPathDefault()
	}
	return
}

func (d *idleDeps) clock() time.Time {
	if d.now != nil {
		return d.now()
	}
	return time.Now()
}

// idleLog appends one line to the idle-events log (0600, rotated at 1 MiB
// like the say log). Counts and codes only, never labels.
func (d *idleDeps) idleLog(format string, args ...any) {
	_, _, path := d.paths()
	if path == "" {
		return
	}
	appendFleetSayLog(path, d.clock().UTC().Format(time.RFC3339)+" "+fmt.Sprintf(format, args...)+"\n")
}

// reloadConfigLocked re-reads the config when its (mtime, size) changed: one
// stat per poll, so enabling needs no restart. A missing file is the
// defaults (disabled). A bad file is logged once per change, and the last
// good config stays.
func (d *idleDeps) reloadConfigLocked() idleConfig {
	path, _, _ := d.paths()
	st, err := os.Stat(path)
	if path == "" || err != nil {
		if !d.cfgMissing || !d.cfgSeen {
			if path != "" && err != nil && !os.IsNotExist(err) {
				d.idleLog("config error: cannot read the config file")
			}
			d.cfg, d.cfgMissing, d.cfgSeen = defaultIdleConfig(), true, true
		}
		return d.cfg
	}
	if d.cfgSeen && !d.cfgMissing && st.ModTime().Equal(d.cfgMod) && st.Size() == d.cfgSize {
		return d.cfg
	}
	if !d.cfgSeen {
		d.cfg = defaultIdleConfig()
	}
	d.cfgSeen, d.cfgMissing, d.cfgMod, d.cfgSize = true, false, st.ModTime(), st.Size()
	data, err := os.ReadFile(path)
	if err != nil {
		d.idleLog("config error: cannot read the config file")
		return d.cfg
	}
	c, err := parseIdleConfig(data)
	if err != nil {
		d.idleLog("config error: %v", err)
		fmt.Fprintf(os.Stderr, "idle events: %s: %v; keeping the last good config\n", path, err)
		return d.cfg
	}
	d.cfg = c
	return c
}

func loadIdleState(path string) (idleStateFile, error) {
	var st idleStateFile
	if path == "" {
		return st, nil
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return idleStateFile{}, err
	}
	return st, nil
}

func (d *idleDeps) loadStateLocked() {
	if d.loaded {
		return
	}
	d.loaded = true
	d.since, d.reported = map[string]time.Time{}, map[string]bool{}
	_, path, _ := d.paths()
	st, err := loadIdleState(path)
	if err != nil {
		d.idleLog("state error: starting empty")
	}
	for _, k := range st.Reported {
		d.reported[k] = true
	}
	d.batch = st.Batch
	d.opened, _ = time.Parse(time.RFC3339, st.BatchOpened)
	d.lastSent, _ = time.Parse(time.RFC3339, st.LastSent)
}

func (d *idleDeps) saveStateLocked() {
	_, path, _ := d.paths()
	if path == "" {
		return
	}
	st := idleStateFile{Reported: make([]string, 0, len(d.reported)), Batch: d.batch}
	for k := range d.reported {
		st.Reported = append(st.Reported, k)
	}
	sort.Strings(st.Reported)
	if st.Batch == nil {
		st.Batch = []idleEvent{}
	}
	if !d.opened.IsZero() {
		st.BatchOpened = d.opened.UTC().Format(time.RFC3339)
	}
	if !d.lastSent.IsZero() {
		st.LastSent = d.lastSent.UTC().Format(time.RFC3339)
	}
	data, err := json.Marshal(st)
	if err == nil {
		err = writeFileAtomic(path, append(data, '\n'), 0o600)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "idle events: cannot save state: %v\n", err)
	}
}

// idleLabelRe is everything a label may not hold. Quotes are not in it, so
// a label cannot close the quotes the message puts around it.
var idleLabelRe = regexp.MustCompile(`[^A-Za-z0-9 ._:/#()-]+`)

// idleSanitize is the label scrub: it keeps [A-Za-z0-9 ._:/#()-], collapses
// spaces, cuts to 40 runes, and breaks up the tag's words, so a name cannot
// smuggle the event tag, a quote, a newline, or an escape into FLEET's
// prompt. "" becomes "-".
func idleSanitize(s string) string {
	s = strings.Join(strings.Fields(idleLabelRe.ReplaceAllString(s, " ")), " ")
	if utf8.RuneCountInString(s) > idleLabelMax {
		s = string([]rune(s)[:idleLabelMax])
		s = strings.TrimSpace(s)
	}
	s = idleScrubTag(s)
	if s == "" {
		return "-"
	}
	return s
}

// idleStatus passes only the statuses a report can have; anything else a
// session file says is "-".
func idleStatus(st string) string {
	switch st {
	case "idle", "shell", "waiting":
		return st
	}
	return "-"
}

var idleTmuxRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}:\d+\.\d+$`)

// idleTmux passes a plain session:window.pane address and nothing else.
func idleTmux(t string) string {
	if !idleTmuxRe.MatchString(t) || fleetHasEventPhrase(t) {
		return "-"
	}
	return t
}

// idlePhraseRe is fleetEventPhraseRe for text that is already sanitized
// ASCII with single spaces, where folding is only lowercasing.
var idlePhraseRe = regexp.MustCompile(`(?i)claude[^A-Za-z0-9]{0,3}sessions?[^A-Za-z0-9]{0,3}event`)

// idleScrubTag replaces every run of the tag's words with "(x)". The "x" is
// alphanumeric, so a replacement can never join its neighbours into a new
// match, and each one shortens the text, so the loop ends. The final check is
// the page's own fold, so the two definitions cannot drift apart silently: if
// they ever disagree, the text goes and "-" stays.
func idleScrubTag(s string) string {
	for idlePhraseRe.MatchString(s) {
		s = idlePhraseRe.ReplaceAllString(s, "(x)")
	}
	if fleetHasEventPhrase(s) {
		return "-"
	}
	return s
}

func idleKey(row Session) string { return fmt.Sprintf("%d:%s", row.PID, row.SessionID) }

// idleSkipped: rows that never start an episode — the delivery target,
// FLEET itself, disabled sessions, and ignored tmux sessions or ids.
func (s *server) idleSkipped(cfg idleConfig, row Session, fleet Session, fleetFound bool) bool {
	part := tmuxSessionPart(row.Tmux)
	if cfg.Target != "" && ((part != "" && part == cfg.Target) || row.SessionID == cfg.Target) {
		return true
	}
	if fleetFound && row.PID == fleet.PID && row.SessionID == fleet.SessionID {
		return true
	}
	for _, ig := range cfg.Ignore {
		if (part != "" && part == ig) || (row.SessionID != "" && row.SessionID == ig) {
			return true
		}
	}
	if row.SessionID != "" {
		if d := s.idle.disabled; d != nil {
			return d(row.SessionID)
		}
		if s.flags != nil && s.flags.Disabled(row.SessionID) {
			return true
		}
	}
	return false
}

// idleDetectLocked runs one poll of idle-stream.sh's state machine over the
// live rows and queues an event per newly reported episode. It reports
// whether the persisted state changed.
func (s *server) idleDetectLocked(cfg idleConfig, sessions []Session, now time.Time) bool {
	d := &s.idle
	changed := false
	fleet, reason := findFleet(sessions)
	var cards map[string]fleetCardInfo
	live := map[string]bool{}
	for _, row := range sessions {
		key := idleKey(row)
		live[key] = true
		if s.idleSkipped(cfg, row, fleet, reason == "") {
			// A skipped row keeps its report, so un-skipping it does not
			// report the same episode again without a busy in between. Its
			// clock stops, and a queued event for it is withdrawn.
			delete(d.since, key)
			if d.dropFromBatchLocked(key) {
				changed = true
			}
			continue
		}
		if row.Status == "busy" {
			delete(d.since, key)
			if d.reported[key] {
				delete(d.reported, key)
				changed = true
			}
			continue
		}
		if d.reported[key] {
			continue
		}
		since, ok := d.since[key]
		if !ok {
			since = now
			d.since[key] = now
		}
		need := time.Duration(cfg.IdleSeconds) * time.Second
		if row.Status == "shell" {
			need = time.Duration(cfg.ShellSeconds) * time.Second
		}
		if now.Sub(since) < need {
			continue
		}
		if cards == nil {
			cards = s.fleetSessionCards()
		}
		if !d.queueLocked(idleEvent{
			Key:    key,
			PID:    row.PID,
			Status: idleStatus(row.Status),
			Tmux:   idleTmux(row.Tmux),
			Since:  since,
			Label:  idleLabel(row, cards),
		}, now) {
			continue // the batch is full: report it once there is room
		}
		d.reported[key] = true
		changed = true
	}
	for k := range d.since {
		if !live[k] {
			delete(d.since, k)
		}
	}
	for k := range d.reported {
		if !live[k] {
			delete(d.reported, k)
			changed = true
		}
	}
	return changed
}

// idleLabel is the ticket of the card naming the row's tmux session, else
// the session's user-set name, else "-"; sanitized.
func idleLabel(row Session, cards map[string]fleetCardInfo) string {
	if c, ok := cards[tmuxSessionPart(row.Tmux)]; ok && row.Tmux != "" && c.Ticket != "" {
		return idleSanitize(c.Ticket)
	}
	if row.NameSource != "derived" && row.Name != "" {
		return idleSanitize(row.Name)
	}
	return "-"
}

// queueLocked adds e to the batch, replacing an older entry for the same
// key, and opens the batch window when it was empty. It reports false when
// the batch is full even after dropping ended episodes.
func (d *idleDeps) queueLocked(e idleEvent, now time.Time) bool {
	d.dropFromBatchLocked(e.Key)
	if len(d.batch) >= idleBatchMax {
		d.batch = d.liveBatchLocked()
		if len(d.batch) >= idleBatchMax {
			return false
		}
	}
	if len(d.batch) == 0 {
		d.opened = now
	}
	d.batch = append(d.batch, e)
	return true
}

func (d *idleDeps) dropFromBatchLocked(key string) bool {
	for i, e := range d.batch {
		if e.Key == key {
			d.batch = append(d.batch[:i:i], d.batch[i+1:]...)
			return true
		}
	}
	return false
}

// liveBatchLocked is the batch without ended episodes (key no longer
// reported) and without duplicate keys (the newest wins).
func (d *idleDeps) liveBatchLocked() []idleEvent {
	out := make([]idleEvent, 0, len(d.batch))
	at := map[string]int{}
	for _, e := range d.batch {
		if !d.reported[e.Key] {
			continue
		}
		if i, dup := at[e.Key]; dup {
			out[i] = e
			continue
		}
		at[e.Key] = len(out)
		out = append(out, e)
	}
	return out
}

// idleMessage is the one-line message for a batch; minutes are counted to
// now.
func idleMessage(events []idleEvent, now time.Time) string {
	parts := make([]string, 0, min(len(events), idleMessageMax))
	for i, e := range events {
		if i == idleMessageMax {
			break
		}
		parts = append(parts, fmt.Sprintf("idle: %d %s %s (%d min) \"%s\"",
			e.PID, e.Status, e.Tmux, int(now.Sub(e.Since)/time.Minute), e.Label))
	}
	body := strings.Join(parts, " ; ")
	if extra := len(events) - idleMessageMax; extra > 0 {
		body += fmt.Sprintf(" ; +%d more", extra)
	}
	// Each field is clean, but neighbours could still spell the tag's words
	// together: scrub the whole body as well. The tag goes in front only
	// after that.
	return fleetEventTag + " idle " + idleScrubTag(body)
}

var fleetSayIdle = fleetSayWho{subject: "The idle-event target", pane: "the target pane"}

// logGateLocked reports whether a line under key may be logged now: one per
// key per idleLogEvery, so a target that refuses every poll for an hour
// writes six lines, not 120. A send clears it.
func (d *idleDeps) logGateLocked(key string) bool {
	now := d.clock()
	if at, ok := d.logAt[key]; ok && now.Sub(at) < idleLogEvery {
		return false
	}
	if d.logAt == nil {
		d.logAt = map[string]time.Time{}
	}
	d.logAt[key] = now
	return true
}

// idleDeliver types text into the target through the page send pipeline.
// Under sayMu, like /fleet/session/say: a fresh collect, exactly one row
// whose tmux session or session id is target (else target_missing /
// target_ambiguous, nothing typed), the shared 1 s gap, and fleetTypeInto
// (exact "=" pane, dialog and copy-mode refusals, an empty input box, Enter
// only when the box holds the text). It skips fleetCleanSayText on purpose:
// that refuses the event tag. code is "sent" or the refusal code; tmux names
// the target row when one was resolved.
func (s *server) idleDeliver(target, text string) (code, tmux string) {
	if target == "" {
		return "target_missing", ""
	}
	f := &s.fleet
	f.sayMu.Lock()
	defer f.sayMu.Unlock()
	sessions, err := s.collectLocal()
	if err != nil {
		return "list_failed", ""
	}
	var hits []Session
	for _, row := range sessions {
		if (row.Tmux != "" && tmuxSessionPart(row.Tmux) == target) || (row.SessionID != "" && row.SessionID == target) {
			hits = append(hits, row)
		}
	}
	switch {
	case len(hits) == 0:
		return "target_missing", ""
	case len(hits) > 1:
		return "target_ambiguous", ""
	}
	row := hits[0]
	tmux = tmuxSessionPart(row.Tmux)
	if row.IsGrok() { // the input-box parser knows only Claude Code's screen
		return "send_unsupported", tmux
	}
	key := "\x00idle-events\x00" + text
	if res := s.fleetSayGateLocked(key); res != nil {
		return res.Code, tmux
	}
	who := fleetSayIdle
	who.logGate = func(code string) bool { return s.idle.logGateLocked("say:" + code) }
	if res := s.fleetTypeInto(row, text, who); !res.OK {
		return res.Code, tmux
	}
	s.fleetSayDoneLocked(key)
	return "sent", tmux
}

// idleSendLocked sends the pending batch when its window has run and the last
// message is at least batchSeconds old. Ended episodes are dropped first;
// when none is left, nothing is sent. A refusal keeps the batch for the next
// poll, except not_submitted: the text sits typed in the box, and sending it
// again would duplicate it if Andy presses Enter. It reports whether the
// persisted state changed.
func (s *server) idleSendLocked(cfg idleConfig, now time.Time) bool {
	d := &s.idle
	window := time.Duration(cfg.BatchSeconds) * time.Second
	if len(d.batch) == 0 || now.Sub(d.opened) < window || (!d.lastSent.IsZero() && now.Sub(d.lastSent) < window) {
		return false
	}
	live := d.liveBatchLocked()
	if len(live) == 0 {
		d.batch, d.opened = nil, time.Time{}
		return true
	}
	changed := len(live) != len(d.batch)
	d.batch = live
	n := len(live)
	code, tmux := s.idleDeliver(cfg.Target, idleMessage(live, now))
	switch code {
	case "sent", "not_submitted":
		if code == "sent" {
			d.idleLog("sent n=%d target=%s", n, tmux)
			d.logAt = nil
		} else {
			if d.logGateLocked("idle:" + code) {
				d.idleLog("not submitted n=%d target=%s; the batch counts as delivered", n, tmux)
			}
			_ = s.appendFleetLog(fleetLogEntry{At: now.UTC().Format(time.RFC3339), Type: "note",
				Text: "Idle events were typed into FLEET but not submitted"})
		}
		d.batch, d.opened, d.lastSent = nil, time.Time{}, now
		return true
	case "target_missing":
		if d.logGateLocked("idle:" + code) {
			d.idleLog("target missing n=%d", n)
		}
	case "target_ambiguous":
		if d.logGateLocked("idle:" + code) {
			d.idleLog("target ambiguous n=%d", n)
		}
	default:
		if d.logGateLocked("idle:" + code) {
			d.idleLog("refused code=%s n=%d", code, n)
		}
	}
	return changed
}

// idleTick is one poll. While disabled it costs one stat of the config file.
func (s *server) idleTick() {
	d := &s.idle
	d.mu.Lock()
	defer d.mu.Unlock()
	cfg := d.reloadConfigLocked()
	if !cfg.Enabled {
		d.since = nil // re-enabling starts every clock again
		if d.wasOn {
			// Turning it off drops what was waiting: a batch held across an
			// off stretch would describe a world that has moved on.
			d.wasOn = false
			if d.loaded && len(d.batch) > 0 {
				d.batch, d.opened = nil, time.Time{}
				d.saveStateLocked()
			}
		}
		return
	}
	d.wasOn = true
	d.loadStateLocked()
	if d.since == nil {
		d.since = map[string]time.Time{}
	}
	now := d.clock()
	sessions, err := s.collectLocal()
	if err != nil {
		return
	}
	changed := s.idleDetectLocked(cfg, sessions, now)
	if s.idleSendLocked(cfg, now) {
		changed = true
	}
	if changed {
		d.saveStateLocked()
	}
}

// runIdleEvents polls forever; started by cmdServer. A panic in one tick is
// logged and the loop goes on: it must never take the server down.
func (s *server) runIdleEvents() {
	d := &s.idle
	sleep := d.sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	for {
		func() {
			defer func() {
				if r := recover(); r != nil {
					fmt.Fprintf(os.Stderr, "idle events: tick failed: %v\n", r)
				}
			}()
			s.idleTick()
		}()
		d.mu.Lock()
		poll := d.cfg.PollSeconds
		d.mu.Unlock()
		if poll <= 0 {
			poll = defaultIdleConfig().PollSeconds
		}
		sleep(time.Duration(poll) * time.Second)
	}
}

// cmdIdleEvents is `claude-sessions idle-events status|test --target T`.
func cmdIdleEvents(args []string) int {
	usage := "usage: claude-sessions idle-events status | test --target <tmux session|session id>"
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	d := &idleDeps{}
	cfgPath, statePath, _ := d.paths()
	switch args[0] {
	case "status":
		if len(args) != 1 {
			fmt.Fprintln(os.Stderr, usage)
			return 2
		}
		cfg, note := defaultIdleConfig(), "missing (defaults: disabled)"
		if data, err := os.ReadFile(cfgPath); err == nil {
			if c, perr := parseIdleConfig(data); perr != nil {
				note = "invalid (" + perr.Error() + "); the server keeps its last good config"
			} else {
				cfg, note = c, "ok"
			}
		} else if !os.IsNotExist(err) {
			note = "unreadable"
		}
		st, err := loadIdleState(statePath)
		stateNote := ""
		if err != nil {
			stateNote = " (state file unreadable)"
		}
		printIdleStatus(cfgPath, note, cfg, st, stateNote)
		return 0
	case "test":
		fs := flag.NewFlagSet("idle-events test", flag.ContinueOnError)
		target := fs.String("target", "", "tmux session name or session id to type the test line into")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		if *target == "" || !idleNameRe.MatchString(*target) || fs.NArg() != 0 {
			fmt.Fprintln(os.Stderr, "idle-events test: --target <tmux session|session id> is required")
			return 2
		}
		s := &server{}
		code, tmux := s.idleDeliver(*target, idleTestMessage)
		if code == "sent" {
			s.idle.idleLog("test sent target=%s", tmux)
			fmt.Printf("sent target=%s\n", tmux)
			return 0
		}
		s.idle.idleLog("test refused code=%s", code)
		fmt.Printf("refused code=%s\n", code)
		return 1
	default:
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
}

func printIdleStatus(cfgPath, note string, cfg idleConfig, st idleStateFile, stateNote string) {
	state := "disabled"
	if cfg.Enabled {
		state = "enabled"
	}
	target := cfg.Target
	if target == "" {
		target = "-"
	}
	ignore := "-"
	if len(cfg.Ignore) > 0 {
		ignore = strings.Join(cfg.Ignore, ", ")
	}
	last := st.LastSent
	if last == "" {
		last = "never"
	}
	fmt.Printf("config:     %s (%s)\n", cfgPath, note)
	fmt.Printf("state:      %s\n", state)
	fmt.Printf("target:     %s\n", target)
	fmt.Printf("thresholds: idle %ds, shell %ds, poll %ds, batch %ds\n", cfg.IdleSeconds, cfg.ShellSeconds, cfg.PollSeconds, cfg.BatchSeconds)
	fmt.Printf("ignore:     %s\n", ignore)
	fmt.Printf("reported:   %d%s\n", len(st.Reported), stateNote)
	fmt.Printf("pending:    %d\n", len(st.Batch))
	fmt.Printf("last sent:  %s\n", last)
}
