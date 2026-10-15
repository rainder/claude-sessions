package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Blocked permission events for FLEET. A session that sits at a permission
// prompt is not idle. Claude keeps Status "busy", and idlePause also holds a
// row whose WaitingFor is "permission prompt", or whose pane still shows the
// approval dialog when its tickets would otherwise read as finished. This
// loop still types one line after the prompt has been up for a minute, once
// per prompt.
//
// Detection is the signal both tools already publish: WaitingFor ==
// "permission prompt". The question text is not in that field (Claude stores
// only the label; grok's events.jsonl stores only the tool name), so the line
// quotes one line taken from the pane. Delivery is idleDeliver, and the FLEET
// row is excluded by the same idleSkipped check the idle loop uses.
//
// Fail closed: off until blocked-events.json enables it, off on a bad config,
// nothing sent when the target is missing or ambiguous, and a refused send
// keeps the pending line for the next poll. not_submitted counts as
// delivered: the text is already in the box, and a retry would duplicate it
// if Andy presses Enter.

const (
	blockedWait        = 60 * time.Second
	blockedPendingMax  = 100 // due prompts kept for a retry; later ones wait
	blockedMessageMax  = 15  // entries in one message; the rest are "+N more"
	blockedQuestionMax = 120
	blockedDefaultPoll = 30
	// A dialog rule farther than this above the question is some other
	// border. Joining the lines under it would let a spinner reset the clock.
	blockedRuleWindow = 12
)

// blockedOnPermission reports a Claude Code or Grok approval prompt.
// Both tools use this exact WaitingFor string: Claude writes it in the
// session file, and grokStatusFromEvents sets it while a tool permission
// is open. "input" is an ask_user_question, not an approval, and Status
// "busy" alone is ordinary work.
func blockedOnPermission(row Session) bool {
	return row.WaitingFor == "permission prompt"
}

// claudeChoiceRe matches the "1. Yes" / "2. No, ..." rows of a Claude Code
// permission dialog, after the ❯ cursor is stripped.
var claudeChoiceRe = regexp.MustCompile(`^[0-9]+\. `)

// blockedQuestionRe is everything a question may not hold. It is the ClickUp
// title set plus a backtick, which Grok puts around the command in its
// approval title. No quote and no newline, so the field cannot close the
// quotes the message puts around it or press Enter.
var blockedQuestionRe = regexp.MustCompile(`[^A-Za-z0-9\x{00C0}-\x{00D6}\x{00D8}-\x{00F6}\x{00F8}-\x{017F} ._:/#()\-,'?!&+` + "`" + `]+`)

// isPermissionChoice reports one row of a permission menu. The ❯ or ">"
// cursor is optional: only the selected row has it.
func isPermissionChoice(line string) bool {
	s := strings.TrimSpace(line)
	if strings.HasPrefix(s, "❯") {
		s = strings.TrimSpace(strings.TrimPrefix(s, "❯"))
	} else if strings.HasPrefix(s, ">") {
		s = strings.TrimSpace(strings.TrimPrefix(s, ">"))
	}
	if claudeChoiceRe.MatchString(s) {
		rest := strings.TrimSpace(claudeChoiceRe.ReplaceAllString(s, ""))
		return strings.HasPrefix(rest, "Yes") || strings.HasPrefix(rest, "No")
	}
	switch {
	case strings.HasPrefix(s, "Allow once"),
		strings.HasPrefix(s, "Reject once"),
		strings.HasPrefix(s, "Always allow"),
		strings.HasPrefix(s, "Enable always-approve"),
		strings.HasPrefix(s, "Allow all edits"),
		s == "Yes", s == "No",
		strings.HasPrefix(s, "Yes,"),
		strings.HasPrefix(s, "No,"):
		return true
	}
	return false
}

// permissionQuestion reads a permission dialog out of a pane capture.
// question is the one line above the menu ("Do you want to proceed?",
// "Allow `go test`", "Allow Edit to main.go"). fingerprint identifies the
// prompt: that line plus the dialog body under a nearby rule. Scrollback
// above the rule is left out on purpose — it changes every poll (a spinner,
// a token) and must not look like a new prompt, which would reset the
// one-minute clock forever. A rule counts only inside blockedRuleWindow.
// With no rule in that window the question line is the whole fingerprint.
// Grok's title already carries the command, and a changing line above it
// must not reset the clock. ok is false when the capture is not a menu.
func permissionQuestion(capture string) (question, fingerprint string, ok bool) {
	raw := strings.Split(strings.TrimRight(capture, "\n"), "\n")
	lines := make([]string, len(raw))
	for i, l := range raw {
		lines[i] = strings.TrimSpace(visibleText(l))
	}
	startMin := 0
	if len(lines) > 25 {
		startMin = len(lines) - 25
	}
	// The lowest menu is the live dialog. An older one can still sit in the
	// same 25 lines, and treating that one as current would change the
	// fingerprint when it scrolls away — a second event for the same prompt.
	menuAt := -1
	for i := startMin; i < len(lines); i++ {
		if !isPermissionChoice(lines[i]) {
			continue
		}
		choices := 0
		for j := i; j < len(lines) && j < i+8; j++ {
			if lines[j] == "" {
				continue
			}
			if !isPermissionChoice(lines[j]) {
				break
			}
			choices++
		}
		if choices >= 2 {
			menuAt = i
		}
	}
	if menuAt < 0 {
		return "", "", false
	}
	qAt := -1
	for i := menuAt - 1; i >= 0; i-- {
		if lines[i] == "" || isRuleLine(lines[i]) || isPermissionChoice(lines[i]) {
			continue
		}
		qAt = i
		break
	}
	if qAt < 0 {
		return "", "", false
	}
	question = strings.Join(strings.Fields(lines[qAt]), " ")
	if question == "" {
		return "", "", false
	}
	var body []string
	ruleAt := -1
	for i := qAt - 1; i >= 0 && qAt-i <= blockedRuleWindow; i-- {
		if isRuleLine(lines[i]) {
			ruleAt = i
			break
		}
	}
	if ruleAt >= 0 {
		for i := ruleAt + 1; i < qAt; i++ {
			if lines[i] == "" || isRuleLine(lines[i]) || isPermissionChoice(lines[i]) {
				continue
			}
			body = append(body, strings.Join(strings.Fields(lines[i]), " "))
		}
	}
	if len(body) > 12 {
		body = body[:12]
	}
	fingerprint = question
	if len(body) > 0 {
		fingerprint += "\n" + strings.Join(body, "\n")
	}
	return question, fingerprint, true
}

// blockedStoredQuestion is the question line kept in a fingerprint.
// "unparsed" means the pane has not yielded one yet.
func blockedStoredQuestion(fp string) (string, bool) {
	if fp == "" || fp == "unparsed" {
		return "", false
	}
	line, _, _ := strings.Cut(fp, "\n")
	line = strings.TrimSpace(line)
	if line == "" {
		return "", false
	}
	return line, true
}

type blockedConfig struct {
	Enabled     bool `json:"enabled"`
	PollSeconds int  `json:"pollSeconds"`
}

func defaultBlockedConfig() blockedConfig {
	return blockedConfig{PollSeconds: blockedDefaultPoll}
}

// parseBlockedConfig reads the config over the defaults, as strictly as
// parseIdleConfig. Unknown keys and a poll out of range are errors: the
// caller keeps the last good config. The one-minute wait is not a field —
// the approved behaviour is a fixed minute, not a tuning knob.
func parseBlockedConfig(data []byte) (blockedConfig, error) {
	c := defaultBlockedConfig()
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return blockedConfig{}, fmt.Errorf("not valid JSON: %v", err)
	}
	if dec.More() {
		return blockedConfig{}, errors.New("trailing data after the JSON object")
	}
	if c.PollSeconds < 5 || c.PollSeconds > 600 {
		return blockedConfig{}, errors.New("pollSeconds must be 5..600")
	}
	return c, nil
}

// blockedEpisode is one prompt on one session. FP "unparsed" means the pane
// has not yielded a question yet; the first good read fills it in without
// starting the clock over. A later different FP is a new prompt.
type blockedEpisode struct {
	FP       string
	Since    time.Time
	Reported bool
}

type blockedEpisodeFile struct {
	Key      string `json:"key"`
	FP       string `json:"fp"`
	Since    string `json:"since"`
	Reported bool   `json:"reported"`
}

// blockedEvent is one due prompt waiting to be typed. Question and Tmux are
// already sanitized. Since is when this prompt was first seen, so minutes
// are counted at send time.
type blockedEvent struct {
	Key       string    `json:"key"`
	PID       int       `json:"pid"`
	SessionID string    `json:"sessionId"`
	Tmux      string    `json:"tmux"`
	Since     time.Time `json:"since"`
	Question  string    `json:"question"`
}

type blockedStateFile struct {
	Episodes []blockedEpisodeFile `json:"episodes"`
	Pending  []blockedEvent       `json:"pending"`
	LastSent string               `json:"lastSent,omitempty"`
}

// blockedDeps is the loop's seams and state. The zero value is production.
type blockedDeps struct {
	configPath string
	statePath  string
	logPath    string
	now        func() time.Time
	sleep      func(time.Duration)
	capture    func(target string) (string, error)        // nil → capturePane
	deliver    func(target, text string) (string, string) // nil → idleDeliver

	mu         sync.Mutex
	cfg        blockedConfig
	cfgSeen    bool
	cfgMissing bool
	cfgMod     time.Time
	cfgSize    int64
	loaded     bool
	wasOn      bool
	episodes   map[string]blockedEpisode
	pending    []blockedEvent
	lastSent   time.Time

	logMu sync.Mutex
	logAt map[string]time.Time
}

func blockedConfigPathDefault() string {
	if dir := ConfigDir(); dir != "" {
		return filepath.Join(dir, "blocked-events.json")
	}
	return ""
}

func blockedStatePathDefault() string {
	if dir := claudeSessionsStateDir(); dir != "" {
		return filepath.Join(dir, "blocked-events-state.json")
	}
	return ""
}

func blockedLogPathDefault() string {
	if dir := claudeSessionsStateDir(); dir != "" {
		return filepath.Join(dir, "blocked-events.log")
	}
	return ""
}

func (d *blockedDeps) paths() (cfg, state, log string) {
	cfg, state, log = d.configPath, d.statePath, d.logPath
	if cfg == "" {
		cfg = blockedConfigPathDefault()
	}
	if state == "" {
		state = blockedStatePathDefault()
	}
	if log == "" {
		log = blockedLogPathDefault()
	}
	return
}

func (d *blockedDeps) clock() time.Time {
	if d.now != nil {
		return d.now()
	}
	return time.Now()
}

// blockedLog appends one line (0600, rotated at 1 MiB like the say log).
// Counts and codes only — the question can be a shell command.
func (d *blockedDeps) blockedLog(format string, args ...any) {
	_, _, path := d.paths()
	if path == "" {
		return
	}
	appendFleetSayLog(path, d.clock().UTC().Format(time.RFC3339)+" "+fmt.Sprintf(format, args...)+"\n")
}

func (d *blockedDeps) logGateLocked(key string) bool {
	d.logMu.Lock()
	defer d.logMu.Unlock()
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

// reloadConfigLocked re-reads the config when its (mtime, size) changed.
// A missing file is off. A bad file is logged once per change, and the last
// good config stays.
func (d *blockedDeps) reloadConfigLocked() blockedConfig {
	path, _, _ := d.paths()
	st, err := os.Stat(path)
	if path == "" || err != nil {
		if !d.cfgMissing || !d.cfgSeen {
			if path != "" && err != nil && !os.IsNotExist(err) {
				d.blockedLog("config error: cannot read the config file")
			}
			d.cfg, d.cfgMissing, d.cfgSeen = defaultBlockedConfig(), true, true
		}
		return d.cfg
	}
	if d.cfgSeen && !d.cfgMissing && st.ModTime().Equal(d.cfgMod) && st.Size() == d.cfgSize {
		return d.cfg
	}
	if !d.cfgSeen {
		d.cfg = defaultBlockedConfig()
	}
	d.cfgSeen, d.cfgMissing, d.cfgMod, d.cfgSize = true, false, st.ModTime(), st.Size()
	data, err := os.ReadFile(path)
	if err != nil {
		d.blockedLog("config error: cannot read the config file")
		return d.cfg
	}
	c, err := parseBlockedConfig(data)
	if err != nil {
		d.blockedLog("config error: %v", err)
		fmt.Fprintf(os.Stderr, "blocked events: %s: %v; keeping the last good config\n", path, err)
		return d.cfg
	}
	d.cfg = c
	return c
}

func (d *blockedDeps) loadStateLocked() {
	if d.loaded {
		return
	}
	d.loaded = true
	d.episodes = map[string]blockedEpisode{}
	_, path, _ := d.paths()
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		d.blockedLog("state error: starting empty")
		return
	}
	var st blockedStateFile
	if json.Unmarshal(data, &st) != nil {
		d.blockedLog("state error: starting empty")
		return
	}
	for _, e := range st.Episodes {
		if e.Key == "" {
			continue
		}
		since, _ := time.Parse(time.RFC3339, e.Since)
		d.episodes[e.Key] = blockedEpisode{FP: e.FP, Since: since, Reported: e.Reported}
	}
	d.pending = st.Pending
	d.lastSent, _ = time.Parse(time.RFC3339, st.LastSent)
}

func (d *blockedDeps) saveStateLocked() {
	_, path, _ := d.paths()
	if path == "" {
		return
	}
	st := blockedStateFile{
		Episodes: make([]blockedEpisodeFile, 0, len(d.episodes)),
		Pending:  d.pending,
	}
	if st.Pending == nil {
		st.Pending = []blockedEvent{}
	}
	keys := make([]string, 0, len(d.episodes))
	for k := range d.episodes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		e := d.episodes[k]
		since := ""
		if !e.Since.IsZero() {
			since = e.Since.UTC().Format(time.RFC3339)
		}
		st.Episodes = append(st.Episodes, blockedEpisodeFile{Key: k, FP: e.FP, Since: since, Reported: e.Reported})
	}
	if !d.lastSent.IsZero() {
		st.LastSent = d.lastSent.UTC().Format(time.RFC3339)
	}
	data, err := json.Marshal(st)
	if err == nil {
		err = writeFileAtomic(path, append(data, '\n'), 0o600)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "blocked events: cannot save state: %v\n", err)
	}
}

// noteOffLocked drops a pending batch when the feature is turned off. The
// episodes stay on disk: a prompt already reported must not fire again just
// because the switch was toggled. The in-memory copy is cleared so the next
// enable reads the file. A batch held across the off stretch would describe
// a world that has moved on, so pending is cleared and saved.
func (d *blockedDeps) noteOffLocked() {
	if !d.wasOn {
		return
	}
	d.wasOn = false
	if len(d.pending) > 0 {
		d.pending = nil
		d.saveStateLocked()
	}
	d.episodes = nil
	d.loaded = false
}

func (d *blockedDeps) dropPendingLocked(key string) bool {
	for i, e := range d.pending {
		if e.Key == key {
			d.pending = append(d.pending[:i:i], d.pending[i+1:]...)
			return true
		}
	}
	return false
}

func (d *blockedDeps) dropEpisodeLocked(key string) bool {
	_, had := d.episodes[key]
	delete(d.episodes, key)
	return had || d.dropPendingLocked(key)
}

func (d *blockedDeps) enqueueLocked(e blockedEvent) bool {
	for _, o := range d.pending {
		if o.Key == e.Key {
			return true
		}
	}
	if len(d.pending) >= blockedPendingMax {
		return false
	}
	d.pending = append(d.pending, e)
	return true
}

func (d *blockedDeps) livePendingLocked() []blockedEvent {
	out := make([]blockedEvent, 0, len(d.pending))
	for _, e := range d.pending {
		ep, ok := d.episodes[e.Key]
		if ok && ep.Reported {
			out = append(out, e)
		}
	}
	return out
}

// blockedQuestionLocked captures the row's pane. A miss is not an error:
// the clock still runs, and the send falls back to "permission prompt".
func (s *server) blockedQuestionLocked(row Session) (question, fp string, ok bool) {
	if row.Tmux == "" {
		return "", "", false
	}
	capFn := s.blocked.capture
	if capFn == nil {
		capFn = capturePane
	}
	text, err := capFn(row.Tmux)
	if err != nil || text == "" {
		return "", "", false
	}
	return permissionQuestion(text)
}

// blockedDetectLocked updates episodes from one poll and queues prompts that
// have waited a minute. It reports whether the persisted state changed.
func (s *server) blockedDetectLocked(idleCfg idleConfig, sessions []Session, now time.Time) bool {
	d := &s.blocked
	if d.episodes == nil {
		d.episodes = map[string]blockedEpisode{}
	}
	changed := false
	fleet, reason := findFleet(sessions)
	live := map[string]bool{}
	for _, row := range sessions {
		key := idleKey(row)
		live[key] = true
		// A skipped row keeps its episode. Disable, ignore, or the FLEET
		// exclusion is not a new prompt, so un-skipping must not send again.
		// Pending is withdrawn: a disabled session must not be typed about.
		if s.idleSkipped(idleCfg, row, fleet, reason == "") {
			if d.dropPendingLocked(key) {
				changed = true
			}
			continue
		}
		if !blockedOnPermission(row) {
			if d.dropEpisodeLocked(key) {
				changed = true
			}
			continue
		}
		q, fp, ok := s.blockedQuestionLocked(row)
		ep, exists := d.episodes[key]
		switch {
		case !exists:
			ep = blockedEpisode{FP: "unparsed", Since: now}
			if ok {
				ep.FP = fp
			}
			d.episodes[key] = ep
			changed = true
		case ok && ep.FP == "unparsed":
			// Same prompt; the pane just became readable. Keep the clock.
			ep.FP = fp
			d.episodes[key] = ep
			changed = true
		case ok && ep.FP != fp:
			// A different dialog with no poll that saw the gap.
			d.dropPendingLocked(key)
			ep = blockedEpisode{FP: fp, Since: now}
			d.episodes[key] = ep
			changed = true
		}
		ep = d.episodes[key]
		if ep.Since.After(now) {
			ep.Since = now
			d.episodes[key] = ep
			changed = true
		}
		if ep.Reported || now.Sub(ep.Since) < blockedWait {
			continue
		}
		if !ok || q == "" {
			// A miss on this poll still has the question from an earlier
			// read. The fingerprint's first line is that question.
			if line, okLine := blockedStoredQuestion(ep.FP); okLine {
				q = line
			} else {
				q = "permission prompt"
			}
		}
		ev := blockedEvent{
			Key:       key,
			PID:       row.PID,
			SessionID: row.SessionID,
			Tmux:      idleTmux(row.Tmux),
			Since:     ep.Since,
			Question:  scrubEventField(q, blockedQuestionRe, blockedQuestionMax),
		}
		if !d.enqueueLocked(ev) {
			continue
		}
		ep.Reported = true
		d.episodes[key] = ep
		changed = true
	}
	for k := range d.episodes {
		if !live[k] {
			if d.dropEpisodeLocked(k) {
				changed = true
			}
		}
	}
	return changed
}

// blockedMessage is the one line for every prompt that came due together.
// The body is scrubbed before the tag is added, so a question cannot leave
// the reserved words in front of an opening bracket that the tag supplies.
func blockedMessage(events []blockedEvent, now time.Time) string {
	parts := make([]string, 0, min(len(events), blockedMessageMax))
	for i, e := range events {
		if i == blockedMessageMax {
			break
		}
		mins := int(now.Sub(e.Since) / time.Minute)
		if mins < 1 {
			mins = 1
		}
		// Scrub again here. The pending question may come from a hand-edited
		// state file, and a newline in it would press Enter in the pane.
		q := scrubEventField(e.Question, blockedQuestionRe, blockedQuestionMax)
		parts = append(parts, fmt.Sprintf("%d %s (%d min) \"%s\"", e.PID, e.Tmux, mins, q))
	}
	body := strings.Join(parts, " ; ")
	if extra := len(events) - blockedMessageMax; extra > 0 {
		body += fmt.Sprintf(" ; +%d more", extra)
	}
	return fleetEventTag + " blocked " + idleScrubTag(body)
}

// blockedSendLocked types the pending prompts as one line. sent and
// not_submitted clear pending. Any other code keeps it for the next poll.
func (s *server) blockedSendLocked(now time.Time) bool {
	d := &s.blocked
	if len(d.pending) == 0 {
		return false
	}
	live := d.livePendingLocked()
	changed := len(live) != len(d.pending)
	d.pending = live
	if len(d.pending) == 0 {
		return true
	}
	sort.Slice(d.pending, func(i, j int) bool {
		if d.pending[i].PID != d.pending[j].PID {
			return d.pending[i].PID < d.pending[j].PID
		}
		return d.pending[i].SessionID < d.pending[j].SessionID
	})
	n := len(d.pending)
	text := blockedMessage(d.pending, now)
	deliver := d.deliver
	if deliver == nil {
		deliver = s.idleDeliver
	}
	code, tmux := deliver(s.fleetEventTarget(), text)
	switch code {
	case "sent", "not_submitted":
		if code == "sent" {
			d.blockedLog("sent n=%d target=%s", n, tmux)
			d.logMu.Lock()
			d.logAt = nil
			d.logMu.Unlock()
		} else if d.logGateLocked("blocked:" + code) {
			d.blockedLog("not submitted n=%d target=%s; the batch counts as delivered", n, tmux)
		}
		d.pending = nil
		d.lastSent = now
		return true
	case "target_missing":
		if d.logGateLocked("blocked:" + code) {
			d.blockedLog("target missing n=%d", n)
		}
	case "target_ambiguous":
		if d.logGateLocked("blocked:" + code) {
			d.blockedLog("target ambiguous n=%d", n)
		}
	default:
		if d.logGateLocked("blocked:" + code) {
			d.blockedLog("refused code=%s n=%d", code, n)
		}
	}
	return changed
}

// blockedTick is one poll. While disabled it costs one stat of the config file.
func (s *server) blockedTick() {
	d := &s.blocked
	d.mu.Lock()
	defer d.mu.Unlock()
	cfg := d.reloadConfigLocked()
	if !cfg.Enabled {
		d.noteOffLocked()
		return
	}
	d.wasOn = true
	d.loadStateLocked()
	s.idle.mu.Lock()
	idleCfg := s.idle.reloadConfigLocked()
	s.idle.mu.Unlock()
	sessions, err := s.collectLocal()
	if err != nil {
		return
	}
	now := d.clock()
	changed := s.blockedDetectLocked(idleCfg, sessions, now)
	if s.blockedSendLocked(now) {
		changed = true
	}
	if changed {
		d.saveStateLocked()
	}
}

// runBlockedEvents polls forever; started by cmdServer. A panic in one tick
// is logged and the loop goes on: it must never take the server down.
func (s *server) runBlockedEvents() {
	d := &s.blocked
	sleep := d.sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	for {
		func() {
			defer func() {
				if r := recover(); r != nil {
					fmt.Fprintf(os.Stderr, "blocked events: tick failed: %v\n", r)
				}
			}()
			s.blockedTick()
		}()
		d.mu.Lock()
		poll := d.cfg.PollSeconds
		d.mu.Unlock()
		if poll <= 0 {
			poll = blockedDefaultPoll
		}
		sleep(time.Duration(poll) * time.Second)
	}
}

// cmdBlockedEvents is `claude-sessions blocked-events status`.
func cmdBlockedEvents(args []string) int {
	if len(args) != 1 || args[0] != "status" {
		fmt.Fprintln(os.Stderr, "usage: claude-sessions blocked-events status")
		return 2
	}
	d := &blockedDeps{}
	cfgPath, statePath, _ := d.paths()
	cfg, note := defaultBlockedConfig(), "missing (defaults: disabled)"
	if data, err := os.ReadFile(cfgPath); err == nil {
		if c, perr := parseBlockedConfig(data); perr != nil {
			note = "invalid (" + perr.Error() + "); the server keeps its last good config"
		} else {
			cfg, note = c, "ok"
		}
	} else if !os.IsNotExist(err) {
		note = "unreadable"
	}
	var st blockedStateFile
	stateNote := ""
	if data, err := os.ReadFile(statePath); err == nil {
		if json.Unmarshal(data, &st) != nil {
			stateNote = " (state file unreadable)"
		}
	} else if !os.IsNotExist(err) {
		stateNote = " (state file unreadable)"
	}
	state := "disabled"
	if cfg.Enabled {
		state = "enabled"
	}
	last := st.LastSent
	if last == "" {
		last = "never"
	}
	fmt.Printf("config:    %s (%s)\n", cfgPath, note)
	fmt.Printf("state:     %s\n", state)
	fmt.Printf("poll:      %ds\n", cfg.PollSeconds)
	fmt.Printf("wait:      %s\n", blockedWait)
	fmt.Printf("episodes:  %d%s\n", len(st.Episodes), stateNote)
	fmt.Printf("pending:   %d\n", len(st.Pending))
	fmt.Printf("last sent: %s\n", last)
	return 0
}
