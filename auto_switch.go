package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Automatic Claude account switch. A server loop reads the Claude numbers
// the FLEET page already shows (s.fleetClaudeUsage: no fetch path of its
// own) and, when the live saved account's fresh 7-day reading reaches the
// threshold, switches to the saved account with the lowest fresh 7-day
// reading below it, through the page chip's own switch (fleetDoSwitch). A
// stale, expired or missing reading never triggers and is never a target. An
// unsaved live login is never switched away from.
//
// Fail closed: off by default, off on a missing file, a bad file keeps the
// last good config, and `claude-sessions auto-switch off` is the kill
// switch. One attempt per autoSwitchCooldown, success or failure. With no
// target, one FLEET event line per episode and one log line per
// autoSwitchNoTargetLogEvery. Logs hold names, percents and codes only,
// never emails.

const (
	autoSwitchCooldown         = 30 * time.Minute
	autoSwitchNoTargetLogEvery = 6 * time.Hour
	// The spend gate (autoSwitchNeedsFresh): far below the threshold, the
	// possibly-fetching numbers are asked at most once per
	// autoSwitchRefreshEvery; within autoSwitchNearMargin points of it, every
	// poll (the numbers source's own per-interval rule still applies).
	autoSwitchRefreshEvery = 30 * time.Minute
	autoSwitchNearMargin   = 10.0
)

type autoSwitchConfig struct {
	Enabled     bool    `json:"enabled"`
	Threshold   float64 `json:"threshold"`
	PollSeconds int     `json:"pollSeconds"`
}

func defaultAutoSwitchConfig() autoSwitchConfig {
	return autoSwitchConfig{Threshold: 95, PollSeconds: 120}
}

// parseAutoSwitchConfig reads the config over the defaults, strictly like
// parseIdleConfig: an unknown key, trailing data or a value out of range is
// an error, and the caller keeps the last good config.
func parseAutoSwitchConfig(data []byte) (autoSwitchConfig, error) {
	c := defaultAutoSwitchConfig()
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return autoSwitchConfig{}, fmt.Errorf("not valid JSON: %v", err)
	}
	if dec.More() {
		return autoSwitchConfig{}, errors.New("trailing data after the JSON object")
	}
	if math.IsNaN(c.Threshold) || c.Threshold < 50 || c.Threshold > 100 {
		return autoSwitchConfig{}, errors.New("threshold must be 50..100")
	}
	if c.PollSeconds < 60 || c.PollSeconds > 3600 {
		return autoSwitchConfig{}, errors.New("pollSeconds must be 60..3600")
	}
	return c, nil
}

// autoSwitchRecord is one switch attempt. Percents are 7-day, rounded.
type autoSwitchRecord struct {
	From      string  `json:"from"`
	To        string  `json:"to"`
	FromPct   int     `json:"fromPct"`
	ToPct     int     `json:"toPct"`
	Threshold float64 `json:"threshold"`
	At        string  `json:"at"`
	OK        bool    `json:"ok"`
	Error     string  `json:"error,omitempty"`
}

// autoSwitchState survives a restart. Last is the last attempt and
// LastManualAt the last manual claude switch from the page; the later one
// starts the cooldown. LastOK is the last successful attempt (the page shows
// it). LastNoTarget is the last "no target" log line. NoTargetEpisode: the
// definitive no-target line for this episode went out; NoTargetHedged: the
// line that says some accounts have no fresh reading went out.
type autoSwitchState struct {
	Last            *autoSwitchRecord `json:"last,omitempty"`
	LastOK          *autoSwitchRecord `json:"lastOK,omitempty"`
	LastManualAt    string            `json:"lastManualAt,omitempty"`
	LastNoTarget    string            `json:"lastNoTarget,omitempty"`
	NoTargetEpisode bool              `json:"noTargetEpisode,omitempty"`
	NoTargetHedged  bool              `json:"noTargetHedged,omitempty"`
}

// autoSwitchDeps is the loop's seams and state. The zero value is the
// production configuration.
type autoSwitchDeps struct {
	configPath string              // "" → ~/.config/claude-sessions/auto-switch.json
	statePath  string              // "" → <state dir>/auto-switch-state.json
	logPath    string              // "" → <state dir>/auto-switch.log
	now        func() time.Time    // nil → time.Now
	sleep      func(time.Duration) // nil → time.Sleep
	// cachedUsage is the disk-only Claude numbers; nil →
	// newCachedUsageNumbersSource().build().Claude.
	cachedUsage func() []claudeUsageNumbers

	// mu guards everything below. It is never held across a switch or a
	// FLEET delivery, so /fleet/state never waits on one.
	mu         sync.Mutex
	cfg        autoSwitchConfig
	cfgSeen    bool
	cfgMissing bool
	cfgMod     time.Time
	cfgSize    int64
	loaded     bool
	state      autoSwitchState
	seenOff    bool // a tick saw the feature off; the next on tick is a new start
	// noTargetCode is the last logged refusal for the no-target event, so a
	// FLEET that stays down logs once, not every poll. Not persisted.
	noTargetCode string
}

func autoSwitchConfigPathDefault() string {
	if dir := ConfigDir(); dir != "" {
		return filepath.Join(dir, "auto-switch.json")
	}
	return ""
}

func (d *autoSwitchDeps) paths() (cfg, state, log string) {
	cfg, state, log = d.configPath, d.statePath, d.logPath
	if cfg == "" {
		cfg = autoSwitchConfigPathDefault()
	}
	dir := claudeSessionsStateDir()
	if state == "" && dir != "" {
		state = filepath.Join(dir, "auto-switch-state.json")
	}
	if log == "" && dir != "" {
		log = filepath.Join(dir, "auto-switch.log")
	}
	return
}

func (d *autoSwitchDeps) clock() time.Time {
	if d.now != nil {
		return d.now()
	}
	return time.Now()
}

// logf appends one line to auto-switch.log (0600, rotated like
// idle-events.log). Names, percents and codes only.
func (d *autoSwitchDeps) logf(format string, args ...any) {
	_, _, path := d.paths()
	if path == "" {
		return
	}
	appendFleetSayLog(path, d.clock().UTC().Format(time.RFC3339)+" "+fmt.Sprintf(format, args...)+"\n")
}

// reloadConfigLocked re-reads the config when its (mtime, size) changed. A
// missing file is the defaults (off); a bad one is logged once per change
// and the last good config stays.
func (d *autoSwitchDeps) reloadConfigLocked() autoSwitchConfig {
	path, _, _ := d.paths()
	st, err := os.Stat(path)
	if path == "" || err != nil {
		if !d.cfgMissing || !d.cfgSeen {
			if path != "" && err != nil && !os.IsNotExist(err) {
				d.logf("config error: cannot read the config file")
			}
			d.cfg, d.cfgMissing, d.cfgSeen = defaultAutoSwitchConfig(), true, true
		}
		return d.cfg
	}
	if d.cfgSeen && !d.cfgMissing && st.ModTime().Equal(d.cfgMod) && st.Size() == d.cfgSize {
		return d.cfg
	}
	if !d.cfgSeen {
		d.cfg = defaultAutoSwitchConfig()
	}
	d.cfgSeen, d.cfgMissing, d.cfgMod, d.cfgSize = true, false, st.ModTime(), st.Size()
	data, err := os.ReadFile(path)
	if err != nil {
		d.logf("config error: cannot read the config file")
		return d.cfg
	}
	c, err := parseAutoSwitchConfig(data)
	if err != nil {
		d.logf("config error: %v", err)
		fmt.Fprintf(os.Stderr, "auto switch: %s: %v; keeping the last good config\n", path, err)
		return d.cfg
	}
	d.cfg = c
	return c
}

func loadAutoSwitchState(path string) (autoSwitchState, error) {
	var st autoSwitchState
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
		return autoSwitchState{}, err
	}
	return st, nil
}

func (d *autoSwitchDeps) loadStateLocked() {
	if d.loaded {
		return
	}
	d.loaded = true
	_, path, _ := d.paths()
	st, err := loadAutoSwitchState(path)
	if err != nil {
		// A torn file may have held a switch made a minute ago: start a
		// cooldown (in memory until the next save), never switch at once.
		d.logf("state error: starting empty with a cooldown")
		st = autoSwitchState{Last: &autoSwitchRecord{At: d.clock().UTC().Format(time.RFC3339), Error: "state_reset"}}
	}
	d.state = st
}

// update changes the state under mu and saves it (dir 0700, file 0600,
// atomic).
func (d *autoSwitchDeps) update(fn func(st *autoSwitchState)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.loadStateLocked()
	fn(&d.state)
	d.saveLocked()
}

func (d *autoSwitchDeps) saveLocked() {
	_, path, _ := d.paths()
	if path == "" {
		return
	}
	data, err := json.Marshal(d.state)
	if err == nil {
		err = writeFileAtomic(path, append(data, '\n'), 0o600)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "auto switch: cannot save state: %v\n", err)
	}
}

// autoSwitchPct is a percent for a log or event line.
func autoSwitchPct(p float64) int { return int(math.Round(p)) }

// autoSwitchThreshold prints a threshold without a needless ".0".
func autoSwitchThreshold(t float64) string { return strconv.FormatFloat(t, 'f', -1, 64) }

// autoSwitchUsable: a saved account's fresh 7-day reading.
func autoSwitchUsable(e claudeUsageNumbers) bool {
	return e.Name != "" && accountNameOK(e.Name) && !e.Stale && !e.Expired && e.SevenDay != nil
}

// autoSwitchActive is the live account's entry, and whether it may trigger:
// saved, a fresh 7-day reading, and that window not already past its reset
// (a reading from before the weekly reset says nothing about the new week).
func autoSwitchActive(entries []claudeUsageNumbers, now time.Time) (claudeUsageNumbers, bool) {
	for _, e := range entries {
		if e.Active {
			return e, autoSwitchUsable(e) && (e.SevenDay.ResetsAt == nil || e.SevenDay.ResetsAt.After(now))
		}
	}
	return claudeUsageNumbers{}, false
}

// autoSwitchNeedsFresh is the spend gate, decided on the disk-only numbers:
// the possibly-fetching source is asked only when the live account's
// reading is missing, past its weekly reset, autoSwitchRefreshEvery old, or
// within autoSwitchNearMargin of the threshold. The stale flag is not an
// input (it is set after a few minutes); the reading's age is. An unsaved
// live login never triggers, so it is never worth a fetch.
func autoSwitchNeedsFresh(cached []claudeUsageNumbers, threshold float64, now time.Time) bool {
	for _, e := range cached {
		if !e.Active {
			continue
		}
		if !accountNameOK(e.Name) {
			return false
		}
		if e.SevenDay == nil || e.FetchedAt == nil || now.Sub(*e.FetchedAt) >= autoSwitchRefreshEvery {
			return true
		}
		if e.SevenDay.ResetsAt != nil && !e.SevenDay.ResetsAt.After(now) {
			return true
		}
		return e.SevenDay.Pct >= threshold-autoSwitchNearMargin
	}
	return true
}

// autoSwitchTarget is the saved account to switch to: another name in
// names with a fresh reading whose 7-day, 5-hour and model-scoped weekly
// windows are all below the threshold (a missing 5-hour or scoped reading
// counts as below), the lowest 7-day; a tie goes to name order. unfresh
// counts the other saved accounts left out for a stale, expired or missing
// reading.
func autoSwitchTarget(entries []claudeUsageNumbers, active string, names []string, threshold float64) (target claudeUsageNumbers, ok bool, unfresh int) {
	byName := map[string]claudeUsageNumbers{}
	for _, e := range entries {
		if _, dup := byName[e.Name]; e.Name != "" && !e.Active && !dup {
			byName[e.Name] = e
		}
	}
	var out []claudeUsageNumbers
	for _, n := range names {
		if n == active {
			continue
		}
		e, found := byName[n]
		if !found || !autoSwitchUsable(e) {
			unfresh++
			continue
		}
		if e.SevenDay.Pct >= threshold || (e.FiveHour != nil && e.FiveHour.Pct >= threshold) ||
			(e.WeeklyScoped != nil && e.WeeklyScoped.Pct >= threshold) {
			continue
		}
		out = append(out, e)
	}
	if len(out) == 0 {
		return claudeUsageNumbers{}, false, unfresh
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].SevenDay.Pct != out[j].SevenDay.Pct {
			return out[i].SevenDay.Pct < out[j].SevenDay.Pct
		}
		return out[i].Name < out[j].Name
	})
	return out[0], true, unfresh
}

// autoSwitchCoolingDown: within autoSwitchCooldown of the later of the last
// attempt and the last manual claude switch from the page.
func autoSwitchCoolingDown(st autoSwitchState, now time.Time) bool {
	var last time.Time
	if st.Last != nil {
		if at, err := time.Parse(time.RFC3339, st.Last.At); err == nil {
			last = at
		}
	}
	if at, err := time.Parse(time.RFC3339, st.LastManualAt); err == nil && at.After(last) {
		last = at
	}
	return !last.IsZero() && now.Sub(last) < autoSwitchCooldown
}

// autoSwitchEventText is the line typed into FLEET after an auto switch.
// Names go through idleSanitize, as in fleetSwitchEventText.
func autoSwitchEventText(old, name string, fromPct, toPct int, threshold float64) string {
	return fleetEventTag + " account claude auto-switched from " + idleSanitize(old) + " to " + idleSanitize(name) +
		fmt.Sprintf(" (7-day %d%% -> %d%%, threshold %s%%); ", fromPct, toPct, autoSwitchThreshold(threshold)) +
		fleetSwitchNoteClaude
}

// autoSwitchFailedText is the line typed into FLEET after a failed attempt.
// code is a fixed tag.
func autoSwitchFailedText(r *autoSwitchRecord) string {
	return fleetEventTag + " account claude auto-switch from " + idleSanitize(r.From) + " to " + idleSanitize(r.To) +
		" failed (" + r.Error + ")" + fmt.Sprintf("; 7-day %d%%, threshold %s%%; next try in %d min",
		r.FromPct, autoSwitchThreshold(r.Threshold), int(autoSwitchCooldown/time.Minute))
}

// autoSwitchNoTargetText is the no-target line. With saved accounts that
// have no fresh reading it says so, since one of them may well be below.
func autoSwitchNoTargetText(pct int, threshold float64, unfresh int) string {
	s := fleetEventTag + fmt.Sprintf(" account claude at %d%% 7-day, threshold %s%%; ", pct, autoSwitchThreshold(threshold))
	if unfresh > 0 {
		return s + fmt.Sprintf("no saved account with a fresh reading below it (%d without a fresh reading)", unfresh)
	}
	return s + "no saved account below it to switch to"
}

// autoSwitchCached is the disk-only Claude numbers: no fetch, ever.
func (s *server) autoSwitchCached() []claudeUsageNumbers {
	if s.auto.cachedUsage != nil {
		return s.auto.cachedUsage()
	}
	return newCachedUsageNumbersSource().build().Claude
}

// autoSwitchTick is one poll. While off it costs one stat of the config.
// While on and far below the threshold it reads disk only, and asks the
// possibly-fetching source once per autoSwitchRefreshEvery.
func (s *server) autoSwitchTick() {
	d := &s.auto
	d.mu.Lock()
	cfg := d.reloadConfigLocked()
	if !cfg.Enabled {
		d.seenOff = true
		d.mu.Unlock()
		return
	}
	d.loadStateLocked()
	if d.seenOff {
		// Off, then on: a new start, so the next no-target episode is
		// reported again.
		d.seenOff = false
		if d.state.NoTargetEpisode || d.state.NoTargetHedged {
			d.state.NoTargetEpisode, d.state.NoTargetHedged = false, false
			d.saveLocked()
		}
	}
	st := d.state
	d.mu.Unlock()

	now := d.clock()
	if !autoSwitchNeedsFresh(s.autoSwitchCached(), cfg.Threshold, now) {
		return
	}
	usage := s.fleetClaudeUsage()
	active, ok := autoSwitchActive(usage, now)
	if !ok {
		return // no fresh reading of a saved live account: never trigger
	}
	if active.SevenDay.Pct < cfg.Threshold {
		if st.NoTargetEpisode || st.NoTargetHedged {
			d.update(func(st *autoSwitchState) { st.NoTargetEpisode, st.NoTargetHedged = false, false })
		}
		d.mu.Lock()
		d.noTargetCode = ""
		d.mu.Unlock()
		return
	}
	names, err := s.fleetAccountNames(accountToolClaude)
	if err != nil {
		// A partial list could hide the target: decide nothing, say nothing.
		fmt.Fprintf(os.Stderr, "auto switch: claude account list failed: %v\n", err)
		return
	}
	target, ok, unfresh := autoSwitchTarget(usage, active.Name, names, cfg.Threshold)
	if !ok {
		s.autoSwitchNoTarget(cfg, active, unfresh, st, now)
		return
	}
	if autoSwitchCoolingDown(st, now) {
		return
	}
	// `auto-switch off` may have landed during the reads above.
	d.mu.Lock()
	on := d.reloadConfigLocked().Enabled
	d.mu.Unlock()
	if !on {
		return
	}
	s.autoSwitchRun(cfg, active, target, now)
}

// autoSwitchNoTarget: one log line per autoSwitchNoTargetLogEvery, and one
// FLEET line per episode (sent or not_submitted counts). A hedged line
// (some saved accounts have no fresh reading) goes once per episode too, but
// does not end it: a later fresh reading can still switch, or send the
// definitive line.
func (s *server) autoSwitchNoTarget(cfg autoSwitchConfig, active claudeUsageNumbers, unfresh int, st autoSwitchState, now time.Time) {
	d := &s.auto
	pct := autoSwitchPct(active.SevenDay.Pct)
	logIt := true
	if at, err := time.Parse(time.RFC3339, st.LastNoTarget); err == nil && now.Sub(at) < autoSwitchNoTargetLogEvery {
		logIt = false
	}
	if logIt {
		d.logf("no target pct=%d unfresh=%d", pct, unfresh)
	}
	hedged := unfresh > 0
	sent := false
	if !st.NoTargetEpisode && !(hedged && st.NoTargetHedged) {
		code := s.fleetSwitchNotify(accountToolClaude, autoSwitchNoTargetText(pct, cfg.Threshold, unfresh))
		sent = code == "sent" || code == "not_submitted"
		d.mu.Lock()
		logRefusal := !sent && code != d.noTargetCode
		if logRefusal {
			d.noTargetCode = code
		}
		d.mu.Unlock()
		if logRefusal {
			d.logf("no target event refused code=%s", code)
		}
	}
	if logIt || sent {
		d.update(func(st *autoSwitchState) {
			if logIt {
				st.LastNoTarget = now.UTC().Format(time.RFC3339)
			}
			if sent && hedged {
				st.NoTargetHedged = true
			} else if sent {
				st.NoTargetEpisode = true
			}
		})
	}
}

// autoSwitchRun makes one attempt. A busy switch lock skips this poll and
// starts no cooldown; every other outcome is recorded and starts one. A
// failure (but not already_live) is typed into FLEET.
func (s *server) autoSwitchRun(cfg autoSwitchConfig, active, target claudeUsageNumbers, now time.Time) {
	d := &s.auto
	fromPct, toPct := autoSwitchPct(active.SevenDay.Pct), autoSwitchPct(target.SevenDay.Pct)
	t := autoSwitchThreshold(cfg.Threshold)
	done, err := s.fleetDoSwitch(accountToolClaude, target.Name, active.Name, func(old string) (string, string, string) {
		return "auto-switch",
			fmt.Sprintf("claude account %s -> %s (7-day %d%% -> %d%%, threshold %s%%)", old, target.Name, fromPct, toPct, t),
			autoSwitchEventText(old, target.Name, fromPct, toPct, cfg.Threshold)
	})
	if errors.Is(err, errFleetSwitchBusy) {
		d.logf("busy: another switch is running; retry next poll")
		return
	}
	rec := &autoSwitchRecord{From: active.Name, To: target.Name, FromPct: fromPct, ToPct: toPct,
		Threshold: cfg.Threshold, At: now.UTC().Format(time.RFC3339), OK: err == nil && !done.Noop}
	switch {
	case errors.Is(err, errFleetSwitchMoved):
		rec.Error = "moved"
	case errors.Is(err, errUnknownAccount):
		rec.Error = "unknown_account"
	case err != nil:
		rec.Error = "switch_failed"
	case done.Noop:
		rec.Error = "already_live"
	}
	d.update(func(st *autoSwitchState) {
		st.Last = rec
		if rec.OK {
			st.LastOK = rec
			st.NoTargetEpisode, st.NoTargetHedged = false, false
		}
	})
	if rec.OK {
		d.mu.Lock()
		d.noTargetCode = ""
		d.mu.Unlock()
		d.logf("switched from=%s to=%s pct=%d to_pct=%d threshold=%s event=%s warnings=%d",
			rec.From, rec.To, fromPct, toPct, t, done.Event, done.Warnings)
		return
	}
	event := "-"
	if rec.Error != "already_live" {
		event = s.fleetSwitchNotify(accountToolClaude, autoSwitchFailedText(rec))
	}
	d.logf("switch failed code=%s from=%s to=%s pct=%d event=%s", rec.Error, rec.From, rec.To, fromPct, event)
}

// runAutoSwitch polls forever; started by cmdServer. A panic in one tick is
// logged and the loop goes on.
func (s *server) runAutoSwitch() {
	d := &s.auto
	sleep := d.sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	for {
		func() {
			defer func() {
				if r := recover(); r != nil {
					fmt.Fprintf(os.Stderr, "auto switch: tick failed: %v\n", r)
				}
			}()
			s.autoSwitchTick()
		}()
		d.mu.Lock()
		poll := d.cfg.PollSeconds
		d.mu.Unlock()
		if poll <= 0 {
			poll = defaultAutoSwitchConfig().PollSeconds
		}
		sleep(time.Duration(poll) * time.Second)
	}
}

// autoSwitchPageLast is the last successful switch as /fleet/state shows
// it: names and percents, never emails.
type autoSwitchPageLast struct {
	From    string `json:"from"`
	To      string `json:"to"`
	FromPct int    `json:"fromPct"`
	ToPct   int    `json:"toPct"`
	At      string `json:"at"`
}

type autoSwitchPageView struct {
	Enabled   bool                `json:"enabled"`
	Threshold float64             `json:"threshold"`
	Last      *autoSwitchPageLast `json:"last"`
}

// autoSwitchView is /fleet/state's autoSwitch key: the loop's config (last
// good, re-read on change) and its last successful switch.
func (s *server) autoSwitchView() autoSwitchPageView {
	d := &s.auto
	d.mu.Lock()
	defer d.mu.Unlock()
	cfg := d.reloadConfigLocked()
	d.loadStateLocked()
	v := autoSwitchPageView{Enabled: cfg.Enabled, Threshold: cfg.Threshold}
	if r := d.state.LastOK; r != nil {
		v.Last = &autoSwitchPageLast{From: r.From, To: r.To, FromPct: r.FromPct, ToPct: r.ToPct, At: r.At}
	}
	return v
}

// autoSwitchSetEnabled rewrites the config with enabled set, keeping the
// threshold and poll, creating the file when missing (atomic, 0600). A file
// that does not parse: on refuses (it would enable settings nobody can
// read); off writes the defaults turned off, since off is the kill switch.
func autoSwitchSetEnabled(path string, on bool) (autoSwitchConfig, string, error) {
	if path == "" {
		return autoSwitchConfig{}, "", errors.New("no config directory (no home)")
	}
	c, note := defaultAutoSwitchConfig(), ""
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		parsed, perr := parseAutoSwitchConfig(data)
		if perr != nil {
			if on {
				return autoSwitchConfig{}, "", fmt.Errorf("%s is invalid (%v); fix it first", path, perr)
			}
			note = "the old file was invalid (" + perr.Error() + "); wrote the defaults, off"
		} else {
			c = parsed
		}
	case !os.IsNotExist(err):
		if on {
			return autoSwitchConfig{}, "", fmt.Errorf("cannot read %s: %v", path, err)
		}
		note = "the old file was unreadable; wrote the defaults, off"
	}
	c.Enabled = on
	out, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return autoSwitchConfig{}, "", err
	}
	if err := writeFileAtomic(path, append(out, '\n'), 0o600); err != nil {
		return autoSwitchConfig{}, "", err
	}
	return c, note, nil
}

// cmdAutoSwitch is `claude-sessions auto-switch status|on|off`.
func cmdAutoSwitch(args []string) int {
	usage := "usage: claude-sessions auto-switch status | on | off"
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	d := &autoSwitchDeps{}
	cfgPath, statePath, _ := d.paths()
	switch args[0] {
	case "status":
		cfg, note := defaultAutoSwitchConfig(), "missing (defaults: disabled)"
		if data, err := os.ReadFile(cfgPath); err == nil {
			if c, perr := parseAutoSwitchConfig(data); perr != nil {
				note = "invalid (" + perr.Error() + "); the server keeps its last good config"
			} else {
				cfg, note = c, "ok"
			}
		} else if !os.IsNotExist(err) {
			note = "unreadable"
		}
		st, err := loadAutoSwitchState(statePath)
		stateNote := ""
		if err != nil {
			stateNote = " (state file unreadable)"
		}
		printAutoSwitchStatus(os.Stdout, cfgPath, note, cfg, st, stateNote)
		return 0
	case "on", "off":
		c, note, err := autoSwitchSetEnabled(cfgPath, args[0] == "on")
		if err != nil {
			fmt.Fprintln(os.Stderr, "auto-switch:", err)
			return 1
		}
		if note != "" {
			fmt.Fprintln(os.Stderr, "auto-switch:", note)
		}
		fmt.Printf("auto switch %s (threshold %s%% 7-day, poll %ds); the server reads it on its next poll\n",
			args[0], autoSwitchThreshold(c.Threshold), c.PollSeconds)
		return 0
	default:
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
}

func autoSwitchRecordLine(r *autoSwitchRecord) string {
	if r == nil {
		return "never"
	}
	if r.From == "" {
		return "none recorded; " + r.Error + " at " + r.At
	}
	line := fmt.Sprintf("%s -> %s (7-day %d%% -> %d%%, threshold %s%%) at %s", r.From, r.To, r.FromPct, r.ToPct,
		autoSwitchThreshold(r.Threshold), r.At)
	if r.OK {
		return line + ", ok"
	}
	return line + ", failed: " + r.Error
}

func printAutoSwitchStatus(w io.Writer, cfgPath, note string, cfg autoSwitchConfig, st autoSwitchState, stateNote string) {
	state := "disabled"
	if cfg.Enabled {
		state = "enabled"
	}
	noTarget := st.LastNoTarget
	if noTarget == "" {
		noTarget = "never"
	}
	fmt.Fprintf(w, "config:      %s (%s)\n", cfgPath, note)
	fmt.Fprintf(w, "state:       %s\n", state)
	fmt.Fprintf(w, "threshold:   %s%% 7-day\n", autoSwitchThreshold(cfg.Threshold))
	fmt.Fprintf(w, "poll:        %ds\n", cfg.PollSeconds)
	fmt.Fprintf(w, "last switch: %s%s\n", autoSwitchRecordLine(st.Last), stateNote)
	if st.LastOK != nil && (st.Last == nil || !st.Last.OK) {
		fmt.Fprintf(w, "last ok:     %s\n", autoSwitchRecordLine(st.LastOK))
	}
	if st.LastManualAt != "" {
		fmt.Fprintf(w, "manual:      %s (page switch; starts the cooldown)\n", st.LastManualAt)
	}
	fmt.Fprintf(w, "no target:   %s\n", noTarget)
}
