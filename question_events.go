package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Question events for FLEET. A session often ends its turn by asking for a
// decision while a background shell, monitor, or subagent is still running.
// That row's Status is "shell", and the idle loop then waits shellSeconds
// (30 minutes) before it says anything. FLEET sees the question late. This
// loop types one line after the question has been up for a minute, once per
// question, even when the row is "shell".
//
// The turn-ended signal is the one the idle loop already uses: anything
// other than Status "busy". A permission prompt is Status "busy" with
// WaitingFor "permission prompt", so the blocked loop owns it and this loop
// does not. Claude's question is the last assistant text in the session
// transcript. Grok writes no such transcript line the server can trust, so
// Grok's question is read from the pane. Delivery is idleDeliver. The FLEET
// row is excluded by the same idleSkipped check.
//
// A row this loop owns is not also reported as idle. The question line is
// the one report for that episode. Off until question-events.json enables it.

const (
	questionWait        = 60 * time.Second
	questionPendingMax  = 100
	questionMessageMax  = 15
	questionLineMax     = 120
	questionDefaultPoll = 30
)

// decisionPhraseRes are the decision phrases, kept in one place. A line that
// ends in "?" in the last paragraph is the other half of the same test.
var decisionPhraseRes = []*regexp.Regexp{
	regexp.MustCompile(`(?i)which direction`),
	regexp.MustCompile(`(?i)which option`),
	regexp.MustCompile(`(?i)your decision`),
	regexp.MustCompile(`(?i)needs your yes`),
	// "A, B, or C" and the same shape with other single letters.
	regexp.MustCompile(`(?i)(?:^|[^A-Za-z])[A-Za-z], [A-Za-z], or [A-Za-z](?:[^A-Za-z]|$)`),
}

// questionTurnEnded reports that no turn is in progress. Status "busy" is
// that turn, for Claude and for Grok. A permission prompt is busy as well,
// and the blocked loop reports it.
func questionTurnEnded(row Session) bool {
	if row.Status == "busy" || blockedOnPermission(row) {
		return false
	}
	return true
}

func decisionPhrase(s string) bool {
	for _, re := range decisionPhraseRes {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

// lastParagraph is the last non-empty block of lines. Blocks are split by a
// blank line. The decision test looks here, so a "?" earlier in the message
// does not count.
func lastParagraph(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	var paras []string
	var cur []string
	flush := func() {
		if len(cur) == 0 {
			return
		}
		paras = append(paras, strings.Join(cur, "\n"))
		cur = nil
	}
	for _, ln := range strings.Split(text, "\n") {
		if strings.TrimSpace(ln) == "" {
			flush()
			continue
		}
		cur = append(cur, strings.TrimSpace(ln))
	}
	flush()
	if len(paras) == 0 {
		return ""
	}
	return paras[len(paras)-1]
}

func lineEndsWithQuestion(ln string) bool {
	ln = strings.TrimRight(ln, " \t\"'`”’")
	return strings.HasSuffix(ln, "?")
}

// decisionQuestion reports the question line in text, if the last paragraph
// asks for a decision. The line is the last line that ends in "?", or else
// the line (or the paragraph) that holds a decision phrase.
func decisionQuestion(text string) (string, bool) {
	para := lastParagraph(text)
	if para == "" {
		return "", false
	}
	var qLine, phraseLine string
	for _, ln := range strings.Split(para, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		if decisionPhrase(ln) {
			phraseLine = ln
		}
		if lineEndsWithQuestion(ln) {
			qLine = ln
		}
	}
	switch {
	case qLine != "":
		return strings.Join(strings.Fields(qLine), " "), true
	case phraseLine != "":
		return strings.Join(strings.Fields(phraseLine), " "), true
	}
	flat := strings.Join(strings.Fields(para), " ")
	if decisionPhrase(flat) {
		return flat, true
	}
	return "", false
}

// questionPaneText is a pane capture with escapes, rules, and the ❯ prompt
// removed, so the last paragraph is the assistant text and not the input box.
func questionPaneText(capture string) string {
	raw := strings.Split(strings.TrimRight(capture, "\n"), "\n")
	lines := make([]string, 0, len(raw))
	for _, ln := range raw {
		plain := strings.TrimSpace(visibleText(ln))
		if plain == "" {
			lines = append(lines, "")
			continue
		}
		if isRuleLine(plain) || strings.HasPrefix(plain, "❯") {
			continue
		}
		lines = append(lines, plain)
	}
	return strings.Join(lines, "\n")
}

func questionHash(line string) string {
	sum := sha256.Sum256([]byte(line))
	return hex.EncodeToString(sum[:8])
}

// lastAssistantText reads the tail of a Claude transcript and returns the
// last assistant message id and text. A later user text turn means the
// question was answered, so ok is false. Tool-only lines and user lines that
// are only wrapper markup are skipped, matching extractConversationTail.
func lastAssistantText(path string) (id, text string, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", false
	}
	defer f.Close()
	seeked := false
	if st, err := f.Stat(); err == nil && st.Size() > infoTailBytes {
		if _, err := f.Seek(st.Size()-infoTailBytes, io.SeekStart); err == nil {
			seeked = true
		}
	}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	if seeked {
		scanner.Scan()
	}
	var role, msgID, body string
	have := false
	for scanner.Scan() {
		var e transcriptLine
		if json.Unmarshal(scanner.Bytes(), &e) != nil {
			continue
		}
		if e.IsMeta || e.IsCompactSummary {
			continue
		}
		if e.Type != "user" && e.Type != "assistant" {
			continue
		}
		turn, turnOK := extractTurnText(e.Message.Content)
		if !turnOK {
			continue
		}
		if e.Type == "user" && strings.HasPrefix(strings.TrimSpace(turn), "<") {
			continue
		}
		role, body = e.Type, turn
		if e.Type == "assistant" {
			msgID = e.Message.ID
		} else {
			msgID = ""
		}
		have = true
	}
	if !have || role != "assistant" || strings.TrimSpace(body) == "" {
		return "", "", false
	}
	return msgID, body, true
}

// questionSource is the question line for one row. Claude uses the
// transcript. Grok uses the pane. qid is the assistant message id when the
// transcript has one, otherwise a hash of the line.
func (s *server) questionSource(row Session) (qid, line string, ok bool) {
	if row.IsGrok() {
		if row.Tmux == "" {
			return "", "", false
		}
		capFn := s.question.capture
		if capFn == nil {
			capFn = capturePane
		}
		text, err := capFn(row.Tmux)
		if err != nil || text == "" {
			return "", "", false
		}
		line, ok = decisionQuestion(questionPaneText(text))
		if !ok {
			return "", "", false
		}
		return questionHash(line), line, true
	}
	home := row.Home
	if home == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return "", "", false
		}
	}
	path := findTranscript(home, row.SessionID)
	if path == "" {
		return "", "", false
	}
	id, text, found := lastAssistantText(path)
	if !found {
		return "", "", false
	}
	line, ok = decisionQuestion(text)
	if !ok {
		return "", "", false
	}
	if id == "" {
		id = questionHash(line)
	}
	return id, line, true
}

type questionConfig struct {
	Enabled     bool `json:"enabled"`
	PollSeconds int  `json:"pollSeconds"`
}

func defaultQuestionConfig() questionConfig {
	return questionConfig{PollSeconds: questionDefaultPoll}
}

// parseQuestionConfig reads the config over the defaults, as strictly as
// parseBlockedConfig. The one-minute wait is not a field.
func parseQuestionConfig(data []byte) (questionConfig, error) {
	c := defaultQuestionConfig()
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return questionConfig{}, fmt.Errorf("not valid JSON: %v", err)
	}
	if dec.More() {
		return questionConfig{}, errors.New("trailing data after the JSON object")
	}
	if c.PollSeconds < 5 || c.PollSeconds > 600 {
		return questionConfig{}, errors.New("pollSeconds must be 5..600")
	}
	return c, nil
}

// questionEpisode is one question on one session. QID is the message id or
// the line hash. Reported means it is queued or already sent.
type questionEpisode struct {
	QID      string
	Since    time.Time
	Reported bool
}

type questionEpisodeFile struct {
	Key      string `json:"key"`
	QID      string `json:"qid"`
	Since    string `json:"since"`
	Reported bool   `json:"reported"`
}

type questionSentFile struct {
	Key string `json:"key"`
	QID string `json:"qid"`
}

// questionEvent is one due question waiting to be typed. Question and Tmux
// are already sanitized.
type questionEvent struct {
	Key       string    `json:"key"`
	QID       string    `json:"qid"`
	PID       int       `json:"pid"`
	SessionID string    `json:"sessionId"`
	Tmux      string    `json:"tmux"`
	Since     time.Time `json:"since"`
	Question  string    `json:"question"`
}

type questionStateFile struct {
	Episodes []questionEpisodeFile `json:"episodes"`
	Sent     []questionSentFile    `json:"sent"`
	Pending  []questionEvent       `json:"pending"`
	LastSent string                `json:"lastSent,omitempty"`
}

// questionDeps is the loop's seams and state. The zero value is production.
// The idle loop's mutex guards this state: questionTick and idleTick both
// take it, so a question and an idle line cannot pass each other.
type questionDeps struct {
	configPath string
	statePath  string
	logPath    string
	now        func() time.Time
	sleep      func(time.Duration)
	capture    func(target string) (string, error)
	deliver    func(target, text string) (string, string)

	cfg        questionConfig
	cfgSeen    bool
	cfgMissing bool
	cfgMod     time.Time
	cfgSize    int64
	loaded     bool
	wasOn      bool
	episodes   map[string]questionEpisode
	sent       map[string]string // session key -> qid already delivered
	pending    []questionEvent
	lastSent   time.Time
	// owners is the set of session keys whose idle episode this loop owns.
	// Guarded by idle.mu, like the rest of this struct.
	owners map[string]bool

	logMu sync.Mutex
	logAt map[string]time.Time
}

func (d *questionDeps) ownsIdle(key string) bool { return d.owners[key] }

func questionConfigPathDefault() string {
	if dir := ConfigDir(); dir != "" {
		return filepath.Join(dir, "question-events.json")
	}
	return ""
}

func questionStatePathDefault() string {
	if dir := claudeSessionsStateDir(); dir != "" {
		return filepath.Join(dir, "question-events-state.json")
	}
	return ""
}

func questionLogPathDefault() string {
	if dir := claudeSessionsStateDir(); dir != "" {
		return filepath.Join(dir, "question-events.log")
	}
	return ""
}

func (d *questionDeps) paths() (cfg, state, log string) {
	cfg, state, log = d.configPath, d.statePath, d.logPath
	if cfg == "" {
		cfg = questionConfigPathDefault()
	}
	if state == "" {
		state = questionStatePathDefault()
	}
	if log == "" {
		log = questionLogPathDefault()
	}
	return
}

func (d *questionDeps) clock() time.Time {
	if d.now != nil {
		return d.now()
	}
	return time.Now()
}

// questionLog appends one line (0600, rotated at 1 MiB). Counts and codes
// only — the question line can be a shell command, so it is never written.
func (d *questionDeps) questionLog(format string, args ...any) {
	_, _, path := d.paths()
	if path == "" {
		return
	}
	appendFleetSayLog(path, d.clock().UTC().Format(time.RFC3339)+" "+fmt.Sprintf(format, args...)+"\n")
}

func (d *questionDeps) logGateLocked(key string) bool {
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

func (d *questionDeps) reloadConfigLocked() questionConfig {
	path, _, _ := d.paths()
	st, err := os.Stat(path)
	if path == "" || err != nil {
		if !d.cfgMissing || !d.cfgSeen {
			if path != "" && err != nil && !os.IsNotExist(err) {
				d.questionLog("config error: cannot read the config file")
			}
			d.cfg, d.cfgMissing, d.cfgSeen = defaultQuestionConfig(), true, true
		}
		return d.cfg
	}
	if d.cfgSeen && !d.cfgMissing && st.ModTime().Equal(d.cfgMod) && st.Size() == d.cfgSize {
		return d.cfg
	}
	if !d.cfgSeen {
		d.cfg = defaultQuestionConfig()
	}
	d.cfgSeen, d.cfgMissing, d.cfgMod, d.cfgSize = true, false, st.ModTime(), st.Size()
	data, err := os.ReadFile(path)
	if err != nil {
		d.questionLog("config error: cannot read the config file")
		return d.cfg
	}
	c, err := parseQuestionConfig(data)
	if err != nil {
		d.questionLog("config error: %v", err)
		fmt.Fprintf(os.Stderr, "question events: %s: %v; keeping the last good config\n", path, err)
		return d.cfg
	}
	d.cfg = c
	return c
}

func (d *questionDeps) loadStateLocked() {
	if d.loaded {
		return
	}
	d.loaded = true
	d.episodes = map[string]questionEpisode{}
	d.sent = map[string]string{}
	_, path, _ := d.paths()
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		d.questionLog("state error: starting empty")
		return
	}
	var st questionStateFile
	if json.Unmarshal(data, &st) != nil {
		d.questionLog("state error: starting empty")
		return
	}
	for _, e := range st.Episodes {
		if e.Key == "" {
			continue
		}
		since, _ := time.Parse(time.RFC3339, e.Since)
		d.episodes[e.Key] = questionEpisode{QID: e.QID, Since: since, Reported: e.Reported}
	}
	for _, e := range st.Sent {
		if e.Key != "" && e.QID != "" {
			d.sent[e.Key] = e.QID
		}
	}
	d.pending = st.Pending
	d.lastSent, _ = time.Parse(time.RFC3339, st.LastSent)
}

func (d *questionDeps) saveStateLocked() {
	_, path, _ := d.paths()
	if path == "" {
		return
	}
	st := questionStateFile{
		Episodes: make([]questionEpisodeFile, 0, len(d.episodes)),
		Sent:     make([]questionSentFile, 0, len(d.sent)),
		Pending:  d.pending,
	}
	if st.Pending == nil {
		st.Pending = []questionEvent{}
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
		st.Episodes = append(st.Episodes, questionEpisodeFile{Key: k, QID: e.QID, Since: since, Reported: e.Reported})
	}
	keys = keys[:0]
	for k := range d.sent {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		st.Sent = append(st.Sent, questionSentFile{Key: k, QID: d.sent[k]})
	}
	if !d.lastSent.IsZero() {
		st.LastSent = d.lastSent.UTC().Format(time.RFC3339)
	}
	data, err := json.Marshal(st)
	if err == nil {
		err = writeFileAtomic(path, append(data, '\n'), 0o600)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "question events: cannot save state: %v\n", err)
	}
}

// noteOffLocked drops a pending batch when the feature is turned off. The
// episodes and sent ids stay on disk, so a question already reported does
// not fire again just because the switch was toggled.
func (d *questionDeps) noteOffLocked() {
	if !d.wasOn {
		return
	}
	d.wasOn = false
	if len(d.pending) > 0 {
		d.pending = nil
		d.saveStateLocked()
	}
	d.episodes = nil
	d.sent = nil
	d.loaded = false
}

func (d *questionDeps) dropPendingLocked(key string) bool {
	for i, e := range d.pending {
		if e.Key == key {
			d.pending = append(d.pending[:i:i], d.pending[i+1:]...)
			return true
		}
	}
	return false
}

func (d *questionDeps) dropEpisodeLocked(key string) bool {
	_, had := d.episodes[key]
	delete(d.episodes, key)
	return had || d.dropPendingLocked(key)
}

func (d *questionDeps) enqueueLocked(e questionEvent) bool {
	for _, o := range d.pending {
		if o.Key == e.Key {
			return true
		}
	}
	if len(d.pending) >= questionPendingMax {
		return false
	}
	d.pending = append(d.pending, e)
	return true
}

func (d *questionDeps) livePendingLocked() []questionEvent {
	out := make([]questionEvent, 0, len(d.pending))
	for _, e := range d.pending {
		ep, ok := d.episodes[e.Key]
		if ok && ep.Reported && ep.QID == e.QID {
			out = append(out, e)
		}
	}
	return out
}

// questionSyncIdleLocked keeps the idle loop from also reporting a row this
// loop owns. Caller holds idle.mu. owners are rows whose last turn currently
// asks a decision. reset are rows that went busy or went away after this
// loop had claimed them: their idle episode starts over. A row that stops
// asking before a line was sent releases the claim. A row that already got
// a question line keeps the idle "reported" bit so the idle loop does not
// send a second line for the same episode.
func (s *server) questionSyncIdleLocked(owners, reset map[string]bool) {
	q := &s.question
	d := &s.idle
	d.loadStateLocked()
	changed := false
	clearClock := func(key string) {
		if _, ok := d.since[key]; ok {
			delete(d.since, key)
			changed = true
		}
		if d.dropFromBatchLocked(key) {
			changed = true
		}
	}
	setReported := func(key string, on bool) {
		if d.reported == nil {
			d.reported = map[string]bool{}
		}
		if on == d.reported[key] {
			return
		}
		if on {
			d.reported[key] = true
		} else {
			delete(d.reported, key)
		}
		changed = true
	}
	for key := range owners {
		clearClock(key)
	}
	for key := range reset {
		clearClock(key)
		setReported(key, false)
	}
	for key := range q.owners {
		if owners[key] || reset[key] {
			continue
		}
		clearClock(key)
		// The question went away before a send. Let the idle clock start.
		if q.sent[key] == "" {
			setReported(key, false)
		}
	}
	if owners == nil {
		owners = map[string]bool{}
	}
	q.owners = owners
	if changed {
		d.saveStateLocked()
	}
}

// questionMarkSentLocked records that the question line for these keys has
// gone out, so the idle loop must not send its own line for the episode.
func (s *server) questionMarkSentLocked(keys []string) {
	d := &s.idle
	d.loadStateLocked()
	changed := false
	if d.reported == nil {
		d.reported = map[string]bool{}
	}
	for _, key := range keys {
		if _, ok := d.since[key]; ok {
			delete(d.since, key)
			changed = true
		}
		if d.dropFromBatchLocked(key) {
			changed = true
		}
		if !d.reported[key] {
			d.reported[key] = true
			changed = true
		}
	}
	if changed {
		d.saveStateLocked()
	}
}

// questionDetectLocked updates episodes from one poll and queues questions
// that have waited a minute. It reports whether the question state changed.
func (s *server) questionDetectLocked(idleCfg idleConfig, sessions []Session, now time.Time) bool {
	q := &s.question
	if q.episodes == nil {
		q.episodes = map[string]questionEpisode{}
	}
	if q.sent == nil {
		q.sent = map[string]string{}
	}
	changed := false
	fleet, reason := findFleet(sessions)
	live := map[string]bool{}
	owners := map[string]bool{}
	reset := map[string]bool{}
	for _, row := range sessions {
		key := idleKey(row)
		live[key] = true
		if s.idleSkipped(idleCfg, row, fleet, reason == "") {
			if q.dropPendingLocked(key) {
				changed = true
			}
			continue
		}
		if !questionTurnEnded(row) {
			if q.owners[key] || q.sent[key] != "" {
				reset[key] = true
			}
			if q.dropEpisodeLocked(key) {
				changed = true
			}
			continue
		}
		qid, line, ok := s.questionSource(row)
		if !ok {
			if q.dropEpisodeLocked(key) {
				changed = true
			}
			continue
		}
		if q.sent[key] == qid {
			owners[key] = true
			continue
		}
		ep, exists := q.episodes[key]
		if !exists || ep.QID != qid {
			if q.dropPendingLocked(key) {
				changed = true
			}
			ep = questionEpisode{QID: qid, Since: now}
			q.episodes[key] = ep
			changed = true
		}
		if ep.Since.After(now) {
			ep.Since = now
			q.episodes[key] = ep
			changed = true
		}
		owners[key] = true
		ep = q.episodes[key]
		if ep.Reported || now.Sub(ep.Since) < questionWait {
			continue
		}
		ev := questionEvent{
			Key:       key,
			QID:       qid,
			PID:       row.PID,
			SessionID: row.SessionID,
			Tmux:      idleTmux(row.Tmux),
			Since:     ep.Since,
			Question:  scrubEventField(line, blockedQuestionRe, questionLineMax),
		}
		if !q.enqueueLocked(ev) {
			continue
		}
		ep.Reported = true
		q.episodes[key] = ep
		changed = true
	}
	for key := range q.episodes {
		if live[key] {
			continue
		}
		reset[key] = true
		if q.dropEpisodeLocked(key) {
			changed = true
		}
	}
	for key := range q.sent {
		if !live[key] {
			delete(q.sent, key)
			reset[key] = true
			changed = true
		}
	}
	for key := range q.owners {
		if !live[key] {
			reset[key] = true
		}
	}
	s.questionSyncIdleLocked(owners, reset)
	return changed
}

// questionMessage is the one line for every question that came due together.
func questionMessage(events []questionEvent, now time.Time) string {
	parts := make([]string, 0, min(len(events), questionMessageMax))
	for i, e := range events {
		if i == questionMessageMax {
			break
		}
		mins := int(now.Sub(e.Since) / time.Minute)
		if mins < 1 {
			mins = 1
		}
		q := scrubEventField(e.Question, blockedQuestionRe, questionLineMax)
		parts = append(parts, fmt.Sprintf("%d %s (%d min) \"%s\"", e.PID, e.Tmux, mins, q))
	}
	body := strings.Join(parts, " ; ")
	if extra := len(events) - questionMessageMax; extra > 0 {
		body += fmt.Sprintf(" ; +%d more", extra)
	}
	return fleetEventTag + " question " + idleScrubTag(body)
}

// questionSendLocked types the pending questions as one line. sent and
// not_submitted clear pending and remember the qid. Any other code keeps
// the line for the next poll.
func (s *server) questionSendLocked(now time.Time) bool {
	q := &s.question
	if len(q.pending) == 0 {
		return false
	}
	live := q.livePendingLocked()
	changed := len(live) != len(q.pending)
	q.pending = live
	if len(q.pending) == 0 {
		return true
	}
	sort.Slice(q.pending, func(i, j int) bool {
		if q.pending[i].PID != q.pending[j].PID {
			return q.pending[i].PID < q.pending[j].PID
		}
		return q.pending[i].SessionID < q.pending[j].SessionID
	})
	n := len(q.pending)
	text := questionMessage(q.pending, now)
	deliver := q.deliver
	if deliver == nil {
		deliver = s.idleDeliver
	}
	code, tmux := deliver(s.fleetEventTarget(), text)
	switch code {
	case "sent", "not_submitted":
		keys := make([]string, 0, n)
		for _, e := range q.pending {
			q.sent[e.Key] = e.QID
			keys = append(keys, e.Key)
		}
		if code == "sent" {
			q.questionLog("sent n=%d target=%s", n, tmux)
			q.logMu.Lock()
			q.logAt = nil
			q.logMu.Unlock()
		} else if q.logGateLocked("question:" + code) {
			q.questionLog("not submitted n=%d target=%s; the batch counts as delivered", n, tmux)
		}
		q.pending = nil
		q.lastSent = now
		s.questionMarkSentLocked(keys)
		return true
	case "target_missing":
		if q.logGateLocked("question:" + code) {
			q.questionLog("target missing n=%d", n)
		}
	case "target_ambiguous":
		if q.logGateLocked("question:" + code) {
			q.questionLog("target ambiguous n=%d", n)
		}
	default:
		if q.logGateLocked("question:" + code) {
			q.questionLog("refused code=%s n=%d", code, n)
		}
	}
	return changed
}

// questionTick is one poll. While disabled it costs one stat of the config
// file. It holds idle.mu for the whole tick so it can claim rows the idle
// loop would otherwise report.
func (s *server) questionTick() {
	s.idle.mu.Lock()
	defer s.idle.mu.Unlock()
	q := &s.question
	cfg := q.reloadConfigLocked()
	if !cfg.Enabled {
		if q.wasOn && len(q.owners) > 0 {
			s.questionSyncIdleLocked(nil, nil)
		}
		q.noteOffLocked()
		return
	}
	q.wasOn = true
	q.loadStateLocked()
	idleCfg := s.idle.reloadConfigLocked()
	sessions, err := s.collectLocal()
	if err != nil {
		return
	}
	now := q.clock()
	changed := s.questionDetectLocked(idleCfg, sessions, now)
	if s.questionSendLocked(now) {
		changed = true
	}
	if changed {
		q.saveStateLocked()
	}
}

// runQuestionEvents polls forever; started by cmdServer. A panic in one tick
// is logged and the loop goes on: it must never take the server down.
func (s *server) runQuestionEvents() {
	q := &s.question
	sleep := q.sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	for {
		func() {
			defer func() {
				if r := recover(); r != nil {
					fmt.Fprintf(os.Stderr, "question events: tick failed: %v\n", r)
				}
			}()
			s.questionTick()
		}()
		s.idle.mu.Lock()
		poll := q.cfg.PollSeconds
		s.idle.mu.Unlock()
		if poll <= 0 {
			poll = questionDefaultPoll
		}
		sleep(time.Duration(poll) * time.Second)
	}
}

// cmdQuestionEvents is `claude-sessions question-events status`.
func cmdQuestionEvents(args []string) int {
	if len(args) != 1 || args[0] != "status" {
		fmt.Fprintln(os.Stderr, "usage: claude-sessions question-events status")
		return 2
	}
	d := &questionDeps{}
	cfgPath, statePath, _ := d.paths()
	cfg, note := defaultQuestionConfig(), "missing (defaults: disabled)"
	if data, err := os.ReadFile(cfgPath); err == nil {
		if c, perr := parseQuestionConfig(data); perr != nil {
			note = "invalid (" + perr.Error() + "); the server keeps its last good config"
		} else {
			cfg, note = c, "ok"
		}
	} else if !os.IsNotExist(err) {
		note = "unreadable"
	}
	var st questionStateFile
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
	fmt.Printf("wait:      %s\n", questionWait)
	fmt.Printf("episodes:  %d%s\n", len(st.Episodes), stateNote)
	fmt.Printf("sent:      %d\n", len(st.Sent))
	fmt.Printf("pending:   %d\n", len(st.Pending))
	fmt.Printf("last sent: %s\n", last)
	return 0
}
