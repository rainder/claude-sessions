package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ClickUp events for FLEET. The server runs `cu backlog --json` every poll,
// compares the answer with what it saw before, and types one tagged line into
// FLEET when a backlog ticket is new or has just turned urgent — so FLEET
// learns about new work without a watcher of its own. Delivery is the idle
// events' (idleDeliver): the same target, the same send pipeline, the same
// guards.
//
// The known tickets are read from the state file on every tick, never kept
// in memory. That is deliberate: an operator can remove one id from the file,
// or set a stored priority to "high", and the next tick produces a real event
// without anyone creating a ClickUp ticket.
//
// Fail closed and fail quiet: off by default, off on a bad config, a failed
// or odd cu answer changes nothing and sends nothing, a first run or an
// unreadable state file only records a baseline, and a ticket missing from
// one answer is kept, so a short answer can never re-announce the backlog.
// Titles and creator names are text strangers control and this goes into an
// LLM's prompt: every field is constrained before it is typed.

const (
	clickupCuTimeout   = 45 * time.Second
	clickupOutputMax   = 8 << 20 // bytes of cu stdout; more is a bad answer
	clickupPendingMax  = 100     // events kept for a retry; the oldest go first
	clickupMessageMax  = 15      // entries in one message; the rest are "+N more"
	clickupTitleMax    = 100
	clickupCxMax       = 3
	clickupPruneAfter  = 30 * 24 * time.Hour // a ticket unseen this long is forgotten
	clickupArgsMax     = 16
	clickupDefaultPoll = 60
)

type clickupConfig struct {
	Enabled     bool     `json:"enabled"`
	PollSeconds int      `json:"pollSeconds"`
	Cu          string   `json:"cu"`
	Args        []string `json:"args"`
}

func defaultClickupConfig() clickupConfig {
	return clickupConfig{PollSeconds: clickupDefaultPoll, Args: []string{"backlog", "--json"}}
}

// clickupArgRe is the shape of one cu argument: no spaces, no shell or flag
// tricks beyond what a plain subcommand and its options need.
var clickupArgRe = regexp.MustCompile(`^[A-Za-z0-9._=:/-]{1,128}$`)

// parseClickupConfig reads the config over the defaults, as strictly as
// parseIdleConfig: unknown keys, values out of range, a relative cu path and
// a bad or empty args list are errors, and the caller keeps the last good
// config. An empty args list is refused because bare `cu` is the TUI.
func parseClickupConfig(data []byte) (clickupConfig, error) {
	c := defaultClickupConfig()
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return clickupConfig{}, fmt.Errorf("not valid JSON: %v", err)
	}
	if dec.More() {
		return clickupConfig{}, errors.New("trailing data after the JSON object")
	}
	if c.PollSeconds < 30 || c.PollSeconds > 3600 {
		return clickupConfig{}, errors.New("pollSeconds must be 30..3600")
	}
	if c.Cu != "" && (!filepath.IsAbs(c.Cu) || filepath.Clean(c.Cu) != c.Cu) {
		return clickupConfig{}, errors.New("cu must be an absolute, clean path")
	}
	if len(c.Args) == 0 || len(c.Args) > clickupArgsMax {
		return clickupConfig{}, fmt.Errorf("args must hold 1..%d entries", clickupArgsMax)
	}
	for _, a := range c.Args {
		if !clickupArgRe.MatchString(a) {
			return clickupConfig{}, fmt.Errorf("args entry %q has a character outside [A-Za-z0-9._=:/-]", a)
		}
	}
	return c, nil
}

// clickupTicket is one element of `cu backlog --json`. Unknown fields are
// ignored (the contract may grow); a field of the wrong type is a bad answer.
type clickupTicket struct {
	ID          string   `json:"id"`
	CustomID    string   `json:"custom_id"`
	Name        string   `json:"name"`
	Priority    string   `json:"priority"`
	Tags        []string `json:"tags"`
	Creator     string   `json:"creator"`
	DateCreated string   `json:"date_created"`
	DateUpdated string   `json:"date_updated"`
	URL         string   `json:"url"`
}

var errClickupBadOutput = errors.New("bad output")

// parseClickupBacklog decodes cu's answer: exactly one JSON array of objects,
// nothing after it. An element without an id is skipped; anything else that
// does not fit — not an array, null, an element that is not an object, a
// field of the wrong type — makes the whole answer bad, so a changed contract
// can never read as "these tickets are gone" or "these are all new".
func parseClickupBacklog(data []byte) ([]clickupTicket, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, errClickupBadOutput
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	var raw []json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return nil, errClickupBadOutput
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errClickupBadOutput
	}
	out := make([]clickupTicket, 0, len(raw))
	for _, r := range raw {
		r = bytes.TrimSpace(r)
		if len(r) == 0 || r[0] != '{' {
			return nil, errClickupBadOutput
		}
		var t clickupTicket
		if err := json.Unmarshal(r, &t); err != nil {
			return nil, errClickupBadOutput
		}
		if t.ID == "" {
			continue
		}
		out = append(out, t)
	}
	return out, nil
}

// clickupTicketState is what the state file remembers per ticket id.
type clickupTicketState struct {
	CustomID string `json:"customId,omitempty"`
	Priority string `json:"priority,omitempty"`
	LastSeen string `json:"lastSeen"`
}

// clickupEvent is one detected event waiting for delivery. Every field is
// already sanitized, and the message builder sanitizes again, so a hand-edited
// state file cannot put anything else into FLEET's prompt either.
type clickupEvent struct {
	Kind     string `json:"kind"` // "new" or "urgent"
	ID       string `json:"id"`
	Ref      string `json:"ref"`
	Priority string `json:"priority"`
	Cx       string `json:"cx"`
	By       string `json:"by"`
	Title    string `json:"title"`
	At       string `json:"at"`
}

type clickupStateFile struct {
	Tickets  map[string]clickupTicketState `json:"tickets"`
	Pending  []clickupEvent                `json:"pending"`
	LastSent string                        `json:"lastSent,omitempty"`
}

// clickupDeps is the ClickUp loop's seams and state. The zero value is the
// production configuration. Only the config is cached; the known tickets are
// read from disk on every tick.
type clickupDeps struct {
	configPath string                                        // "" → ~/.config/claude-sessions/clickup-events.json
	statePath  string                                        // "" → <state dir>/clickup-events-state.json
	logPath    string                                        // "" → <state dir>/clickup-events.log
	now        func() time.Time                              // nil → time.Now
	sleep      func(time.Duration)                           // nil → time.Sleep
	run        func(ctx context.Context) ([]byte, error)     // nil → the cu binary
	deliver    func(target, text string) (code, tmux string) // nil → s.idleDeliver

	mu         sync.Mutex // one tick at a time; guards everything below
	cfg        clickupConfig
	cfgSeen    bool
	cfgMissing bool
	cfgMod     time.Time
	cfgSize    int64
	logAt      map[string]time.Time // last log line per failure or refusal code
}

func clickupConfigPathDefault() string {
	if dir := ConfigDir(); dir != "" {
		return filepath.Join(dir, "clickup-events.json")
	}
	return ""
}

func (d *clickupDeps) paths() (cfg, state, log string) {
	cfg, state, log = d.configPath, d.statePath, d.logPath
	if cfg == "" {
		cfg = clickupConfigPathDefault()
	}
	if dir := claudeSessionsStateDir(); dir != "" {
		if state == "" {
			state = filepath.Join(dir, "clickup-events-state.json")
		}
		if log == "" {
			log = filepath.Join(dir, "clickup-events.log")
		}
	}
	return
}

func (d *clickupDeps) clock() time.Time {
	if d.now != nil {
		return d.now()
	}
	return time.Now()
}

// clickupLog appends one line to the ClickUp events log (0600, rotated at
// 1 MiB). Counts and codes only: never a title, never cu's stderr.
func (d *clickupDeps) clickupLog(format string, args ...any) {
	_, _, path := d.paths()
	if path == "" {
		return
	}
	appendFleetSayLog(path, d.clock().UTC().Format(time.RFC3339)+" "+fmt.Sprintf(format, args...)+"\n")
}

// logGateLocked: one line per key per idleLogEvery. A send clears it.
func (d *clickupDeps) logGateLocked(key string) bool {
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

// reloadConfigLocked re-reads the config when its (mtime, size) changed, as
// the idle loop does: one stat per poll, no restart to enable, a bad file
// logged once per change with the last good config kept, a missing file the
// defaults (disabled).
func (d *clickupDeps) reloadConfigLocked() clickupConfig {
	path, _, _ := d.paths()
	st, err := os.Stat(path)
	if path == "" || err != nil {
		if !d.cfgMissing || !d.cfgSeen {
			if path != "" && err != nil && !os.IsNotExist(err) {
				d.clickupLog("config error: cannot read the config file")
			}
			d.cfg, d.cfgMissing, d.cfgSeen = defaultClickupConfig(), true, true
		}
		return d.cfg
	}
	if d.cfgSeen && !d.cfgMissing && st.ModTime().Equal(d.cfgMod) && st.Size() == d.cfgSize {
		return d.cfg
	}
	if !d.cfgSeen {
		d.cfg = defaultClickupConfig()
	}
	d.cfgSeen, d.cfgMissing, d.cfgMod, d.cfgSize = true, false, st.ModTime(), st.Size()
	data, err := os.ReadFile(path)
	if err != nil {
		d.clickupLog("config error: cannot read the config file")
		return d.cfg
	}
	c, err := parseClickupConfig(data)
	if err != nil {
		d.clickupLog("config error: %v", err)
		fmt.Fprintf(os.Stderr, "clickup events: %s: %v; keeping the last good config\n", path, err)
		return d.cfg
	}
	d.cfg = c
	return c
}

// resolveClickupCu is the cu binary to run: the configured path, else cu on
// PATH, else ~/.cargo/bin/cu (the service unit's PATH may not hold
// ~/.cargo/bin). "" when none exists.
func resolveClickupCu(cfg clickupConfig) string {
	if cfg.Cu != "" {
		return cfg.Cu
	}
	if p, err := exec.LookPath("cu"); err == nil {
		return p
	}
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, ".cargo", "bin", "cu")
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			return p
		}
	}
	return ""
}

// cappedBuffer keeps the first max bytes and notes that more came. It never
// fails a write, so cu is not killed by a broken pipe mid-answer.
type cappedBuffer struct {
	buf  bytes.Buffer
	max  int
	over bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.buf.Len(); room < len(p) {
		b.over = true
		if room > 0 {
			b.buf.Write(p[:room])
		}
		return len(p), nil
	}
	return b.buf.Write(p)
}

var (
	errClickupNoCu    = errors.New("not found")
	errClickupTimeout = errors.New("timeout")
)

// runClickupCu runs cu with stdin from /dev/null and stderr discarded: its
// stderr is never read, since it could hold anything. The context kills it
// on timeout.
func runClickupCu(ctx context.Context, path string, args []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, path, args...)
	out := &cappedBuffer{max: clickupOutputMax}
	cmd.Stdout = out
	// The timeout kills cu's whole process group, so a child it started
	// cannot outlive it; WaitDelay bounds the wait for one that still holds
	// stdout open.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil, errClickupTimeout
	}
	if err != nil {
		return nil, err
	}
	if out.over {
		return nil, errClickupBadOutput
	}
	return out.buf.Bytes(), nil
}

// clickupFailCode is the log code for a failed run: "exit N", "timeout",
// "bad output", "not found", or "start" for anything else.
func clickupFailCode(err error) string {
	var exit *exec.ExitError
	switch {
	case errors.Is(err, errClickupTimeout), errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.As(err, &exit):
		return fmt.Sprintf("exit %d", exit.ExitCode())
	case errors.Is(err, errClickupBadOutput):
		return "bad output"
	case errors.Is(err, errClickupNoCu):
		return "not found"
	}
	return "start"
}

// fetchLocked runs cu once (the run seam, or the binary) and parses the
// answer.
func (d *clickupDeps) fetchLocked(cfg clickupConfig) ([]clickupTicket, error) {
	ctx, cancel := context.WithTimeout(context.Background(), clickupCuTimeout)
	defer cancel()
	var out []byte
	var err error
	if d.run != nil {
		out, err = d.run(ctx)
	} else if path := resolveClickupCu(cfg); path == "" {
		err = errClickupNoCu
	} else {
		out, err = runClickupCu(ctx, path, cfg.Args)
	}
	if err != nil {
		return nil, err
	}
	return parseClickupBacklog(out)
}

// loadClickupState reads the state file. missing reports that there is none
// yet (a first run); err that it could not be read or parsed.
func loadClickupState(path string) (st clickupStateFile, missing bool, err error) {
	if path == "" {
		return st, true, nil
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return st, true, nil
	}
	if err != nil {
		return st, false, err
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return clickupStateFile{}, false, err
	}
	return st, false, nil
}

// saveStateLocked writes the state file atomically. The caller decides what
// a failure stops: the state is read from disk on every tick, so an event
// whose save failed would be found again next tick.
func (d *clickupDeps) saveStateLocked(st clickupStateFile) error {
	_, path, _ := d.paths()
	if path == "" {
		return errors.New("no state path")
	}
	if st.Pending == nil {
		st.Pending = []clickupEvent{}
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err == nil {
		err = writeFileAtomic(path, append(data, '\n'), 0o600)
	}
	return err
}

// saveFailedLocked logs a failed state save, once per idleLogEvery.
func (d *clickupDeps) saveFailedLocked(err error) {
	if d.logGateLocked("state:save") {
		d.clickupLog("state error: save failed")
	}
	fmt.Fprintf(os.Stderr, "clickup events: cannot save state: %v\n", err)
}

var (
	clickupCustomIDRe = regexp.MustCompile(`^[A-Z][A-Z0-9]{0,9}-\d{1,7}$`)
	clickupIDRe       = regexp.MustCompile(`^[a-z0-9]{1,32}$`)
	clickupCxRe       = regexp.MustCompile(`^cx:[a-z]{1,16}$`)
	clickupURLRe      = regexp.MustCompile(`^https://app\.clickup\.com/t/[A-Za-z0-9]{1,32}(/[A-Za-z0-9-]{1,32})?$`)
	// clickupTitleRe is everything a title may not hold. ASCII letters and
	// digits stay, the Latin-1 Supplement and Latin Extended-A letters
	// (U+00C0–U+017F without × and ÷: Lithuanian and the other European
	// alphabets), and a few marks. Not every Unicode letter: Lisu capitals,
	// small capitals and modifier letters (ˮ looks like a quote) can spell the
	// tag or fake an entry where no fold catches them. No quote, no bracket,
	// no control or format character, and no "@": an open "@name" token can
	// leave Claude Code's file-mention menu open when Enter is pressed.
	clickupTitleRe = regexp.MustCompile(`[^A-Za-z0-9\x{00C0}-\x{00D6}\x{00D8}-\x{00F6}\x{00F8}-\x{017F} ._:/#()\-,'?!&+]+`)
)

// clickupPriority passes ClickUp's four priorities; anything else is "none".
func clickupPriority(p string) string {
	switch p {
	case "urgent", "high", "normal", "low":
		return p
	}
	return "none"
}

// clickupRef is the custom id (DR-1234) when it has that shape, else the
// ClickUp id when it is plain, else "-".
func clickupRef(customID, id string) string {
	switch {
	case clickupCustomIDRe.MatchString(customID):
		return idleScrubTag(customID)
	case clickupIDRe.MatchString(id):
		return idleScrubTag(id)
	}
	return "-"
}

// clickupCx is the task's cx:* tags joined with "+", "no-cx" when none.
func clickupCx(tags []string) string {
	var out []string
	seen := map[string]bool{}
	for _, t := range tags {
		if clickupCxRe.MatchString(t) && !seen[t] && len(out) < clickupCxMax {
			seen[t] = true
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return "no-cx"
	}
	return idleScrubTag(strings.Join(out, "+"))
}

// clickupTitle scrubs a title: ASCII and Latin-1/Extended-A letters, digits
// and a few marks,
// whitespace collapsed, cut to 100 runes, the tag's words broken up (a
// lookalike spelling of them leaves "-"). No ellipsis on a cut.
func clickupTitle(s string) string {
	return scrubEventField(s, clickupTitleRe, clickupTitleMax)
}

// clickupEventFrom builds the sanitized event for t.
func clickupEventFrom(kind string, t clickupTicket, at time.Time) clickupEvent {
	return clickupEvent{
		Kind:     kind,
		ID:       t.ID,
		Ref:      clickupRef(t.CustomID, t.ID),
		Priority: clickupPriority(t.Priority),
		Cx:       clickupCx(t.Tags),
		By:       idleSanitize(t.Creator),
		Title:    clickupTitle(t.Name),
		At:       at.UTC().Format(time.RFC3339),
	}
}

// clickupClean re-sanitizes an event's fields (they may come from a
// hand-edited state file). Every sanitizer is idempotent on its own output.
func clickupClean(e clickupEvent) clickupEvent {
	if e.Kind != "urgent" {
		e.Kind = "new"
	}
	ref := "-"
	if clickupCustomIDRe.MatchString(e.Ref) || clickupIDRe.MatchString(e.Ref) {
		ref = idleScrubTag(e.Ref)
	}
	e.Ref = ref
	e.Priority = clickupPriority(e.Priority)
	if e.Cx != "no-cx" {
		e.Cx = clickupCx(strings.Split(e.Cx, "+"))
	}
	e.By = idleSanitize(e.By)
	e.Title = clickupTitle(e.Title)
	if fleetHasEventPhrase(e.By + " \"" + e.Title) {
		// The creator and the title are the only free-text neighbours, with
		// only ` "` (or ": " on the page) between them: drop the title rather
		// than leave the whole-body scrub to cut through the quote.
		e.Title = "-"
	}
	return e
}

// clickupEntry is one event in the message:
// `new DR-1234 high cx:low by Andrius "title"`.
func clickupEntry(e clickupEvent) string {
	e = clickupClean(e)
	return fmt.Sprintf("%s %s %s %s by %s \"%s\"", e.Kind, e.Ref, e.Priority, e.Cx, e.By, e.Title)
}

// clickupMessage is the one-line message for the pending events: entries
// joined with " ; " (a newline would press Enter in the pane), at most 15 and
// within the send size, then " ; +N more".
func clickupMessage(events []clickupEvent) string {
	const prefix = fleetEventTag + " ticket "
	// Room for the prefix and the longest possible "+N more".
	budget := sendKeysMaxLen - len(prefix) - len(fmt.Sprintf(" ; +%d more", clickupPendingMax))
	parts := make([]string, 0, min(len(events), clickupMessageMax))
	size := 0
	for i, e := range events {
		if i == clickupMessageMax {
			break
		}
		entry := clickupEntry(e)
		add := len(entry)
		if i > 0 {
			add += len(" ; ")
		}
		if size+add > budget {
			break
		}
		size += add
		parts = append(parts, entry)
	}
	body := strings.Join(parts, " ; ")
	if extra := len(events) - len(parts); extra > 0 {
		if body != "" {
			body += " ; "
		}
		body += fmt.Sprintf("+%d more", extra)
	}
	// Each field is clean, but neighbours could still spell the tag's words
	// together: scrub the whole body too, as idleMessage does.
	return prefix + idleScrubTag(body)
}

// clickupFleetLogText is the /fleet history line for an event:
// `new DR-1234 (high, cx:low) by Andrius: title`.
func clickupFleetLogText(e clickupEvent) string {
	e = clickupClean(e)
	return fmt.Sprintf("%s %s (%s, %s) by %s: %s", e.Kind, e.Ref, e.Priority, e.Cx, e.By, e.Title)
}

// clickupDiff compares the answer with the known tickets. It updates every
// seen ticket in known, keeps unseen ones, prunes those unseen for 30 days,
// and returns the events: an id not known is "new"; a known one whose stored
// priority is not urgent and is urgent now is "urgent". A new ticket that is
// already urgent is only "new".
func clickupDiff(known map[string]clickupTicketState, tickets []clickupTicket, now time.Time) []clickupEvent {
	var events []clickupEvent
	stamp := now.UTC().Format(time.RFC3339)
	for _, t := range tickets {
		prio := clickupPriority(t.Priority)
		prev, ok := known[t.ID]
		switch {
		case !ok:
			events = append(events, clickupEventFrom("new", t, now))
		case prev.Priority != "urgent" && prio == "urgent":
			events = append(events, clickupEventFrom("urgent", t, now))
		}
		custom := ""
		if clickupCustomIDRe.MatchString(t.CustomID) {
			custom = t.CustomID
		}
		known[t.ID] = clickupTicketState{CustomID: custom, Priority: prio, LastSeen: stamp}
	}
	for id, st := range known {
		seen, err := time.Parse(time.RFC3339, st.LastSeen)
		if err != nil || now.Sub(seen) > clickupPruneAfter {
			delete(known, id)
		}
	}
	return events
}

// clickupQueue adds events to pending: one entry per (kind, id), the newest
// wins, and at most clickupPendingMax, the oldest dropped.
func clickupQueue(pending, events []clickupEvent) []clickupEvent {
	out := make([]clickupEvent, 0, len(pending)+len(events))
	for _, e := range append(append([]clickupEvent(nil), pending...), events...) {
		for i, o := range out {
			if o.Kind == e.Kind && o.ID == e.ID {
				out = append(out[:i:i], out[i+1:]...)
				break
			}
		}
		out = append(out, e)
	}
	if extra := len(out) - clickupPendingMax; extra > 0 {
		out = out[extra:]
	}
	return out
}

func (s *server) clickupDeliver(target, text string) (code, tmux string) {
	if d := s.clickup.deliver; d != nil {
		return d(target, text)
	}
	return s.idleDeliver(target, text)
}

// clickupTick is one poll. While disabled it costs one stat of the config
// file.
func (s *server) clickupTick() {
	d := &s.clickup
	d.mu.Lock()
	defer d.mu.Unlock()
	cfg := d.reloadConfigLocked()
	if !cfg.Enabled {
		return
	}
	tickets, err := d.fetchLocked(cfg)
	if err != nil {
		code := clickupFailCode(err)
		if d.logGateLocked("cu:" + code) {
			d.clickupLog("cu failed: %s", code)
		}
		return
	}
	now := d.clock()
	_, statePath, _ := d.paths()
	st, missing, err := loadClickupState(statePath)
	if err != nil {
		if d.logGateLocked("state") {
			d.clickupLog("state error: rebaseline")
		}
		missing = true
	}
	if missing {
		// A first run, or a state file that cannot be parsed: record what is
		// there and announce nothing. An empty answer is no baseline — the
		// next real one would read every ticket as new. A file that parses
		// with no tickets is valid state, not this.
		if len(tickets) == 0 {
			return
		}
		known := map[string]clickupTicketState{}
		clickupDiff(known, tickets, now)
		if err := d.saveStateLocked(clickupStateFile{Tickets: known}); err != nil {
			d.saveFailedLocked(err)
			return
		}
		d.clickupLog("baseline n=%d", len(known))
		return
	}
	if st.Tickets == nil {
		st.Tickets = map[string]clickupTicketState{}
	}
	events := clickupDiff(st.Tickets, tickets, now)
	st.Pending = clickupQueue(st.Pending, events)
	// Persist the events before anything leaves: a save that keeps failing
	// would otherwise find, log and send the same events on every tick.
	if err := d.saveStateLocked(st); err != nil {
		d.saveFailedLocked(err)
		return
	}
	for _, e := range events {
		// The page shows each event when it is found, even while FLEET is
		// down; delivery below may wait.
		entry := fleetLogEntry{At: e.At, Type: "clickup", Text: clickupFleetLogText(e)}
		if t := findClickupTicket(tickets, e.ID); t != nil && clickupURLRe.MatchString(t.URL) {
			entry.URL = t.URL
		}
		if err := s.appendFleetLog(entry); err != nil {
			fmt.Fprintf(os.Stderr, "clickup events: fleet log append failed: %v\n", err)
		}
	}
	if len(events) > 0 {
		d.clickupLog("detected n=%d", len(events))
	}
	if n := len(st.Pending); n > 0 {
		code, tmux := s.clickupDeliver(s.fleetEventTarget(), clickupMessage(st.Pending))
		switch code {
		case "sent":
			d.clickupLog("sent n=%d target=%s", n, tmux)
			d.logAt = nil
			st.Pending, st.LastSent = nil, now.UTC().Format(time.RFC3339)
		case "not_submitted":
			// The text sits typed in the box: a retry would duplicate it if
			// Andy presses Enter on the leftover, so it counts as delivered.
			if d.logGateLocked("send:" + code) {
				d.clickupLog("not submitted n=%d target=%s; counts as delivered", n, tmux)
			}
			_ = s.appendFleetLog(fleetLogEntry{At: now.UTC().Format(time.RFC3339), Type: "note",
				Text: "ClickUp events were typed into FLEET but not submitted"})
			st.Pending, st.LastSent = nil, now.UTC().Format(time.RFC3339)
		default:
			if d.logGateLocked("send:" + code) {
				d.clickupLog("refused code=%s n=%d", code, n)
			}
			return // pending is already on disk
		}
		// Delivered: clear pending on disk. If this save fails, the next tick
		// sends the line again — the one duplicate a broken disk can cause.
		if err := d.saveStateLocked(st); err != nil {
			d.saveFailedLocked(err)
		}
	}
}

func findClickupTicket(tickets []clickupTicket, id string) *clickupTicket {
	for i := range tickets {
		if tickets[i].ID == id {
			return &tickets[i]
		}
	}
	return nil
}

// runClickupEvents polls forever; started by cmdServer. A panic in one tick
// is logged and the loop goes on: it must never take the server down.
func (s *server) runClickupEvents() {
	d := &s.clickup
	sleep := d.sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	for {
		func() {
			defer func() {
				if r := recover(); r != nil {
					fmt.Fprintf(os.Stderr, "clickup events: tick failed: %v\n", r)
				}
			}()
			s.clickupTick()
		}()
		d.mu.Lock()
		poll := d.cfg.PollSeconds
		d.mu.Unlock()
		if poll <= 0 {
			poll = clickupDefaultPoll
		}
		sleep(time.Duration(poll) * time.Second)
	}
}

// cmdClickupEvents is `claude-sessions clickup-events status`.
func cmdClickupEvents(args []string) int {
	if len(args) != 1 || args[0] != "status" {
		fmt.Fprintln(os.Stderr, "usage: claude-sessions clickup-events status")
		return 2
	}
	d := &clickupDeps{}
	cfgPath, statePath, _ := d.paths()
	cfg, note := defaultClickupConfig(), "missing (defaults: disabled)"
	if data, err := os.ReadFile(cfgPath); err == nil {
		if c, perr := parseClickupConfig(data); perr != nil {
			note = "invalid (" + perr.Error() + "); the server keeps its last good config"
		} else {
			cfg, note = c, "ok"
		}
	} else if !os.IsNotExist(err) {
		note = "unreadable"
	}
	st, missing, err := loadClickupState(statePath)
	stateNote := ""
	switch {
	case err != nil:
		stateNote = " (state file unreadable; the next tick rebaselines)"
	case missing:
		stateNote = " (no state yet; the first tick records a baseline)"
	}
	printClickupStatus(cfgPath, note, cfg, resolveClickupCu(cfg), st, stateNote)
	return 0
}

func printClickupStatus(cfgPath, note string, cfg clickupConfig, cu string, st clickupStateFile, stateNote string) {
	state := "disabled"
	if cfg.Enabled {
		state = "enabled"
	}
	if cu == "" {
		cu = "not found"
	}
	last := st.LastSent
	if last == "" {
		last = "never"
	}
	fmt.Printf("config:     %s (%s)\n", cfgPath, note)
	fmt.Printf("state:      %s\n", state)
	fmt.Printf("poll:       %ds\n", cfg.PollSeconds)
	fmt.Printf("cu:         %s %s\n", cu, strings.Join(cfg.Args, " "))
	fmt.Printf("tickets:    %d%s\n", len(st.Tickets), stateNote)
	fmt.Printf("pending:    %d\n", len(st.Pending))
	fmt.Printf("last sent:  %s\n", last)
}
