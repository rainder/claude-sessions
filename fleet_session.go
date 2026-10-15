package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// The FLEET page's session view: the transcript of a session a FLEET card
// names or the working list holds, and a box to type into it. It types into
// sessions other than FLEET, so the target is allowlisted
// (fleetResolveSession): only a tmux session named by a card in the current
// state.json or a live working-list row, never FLEET itself, and exactly one
// live row there. The pane address always comes from that resolved row;
// the request supplies a validated name and nothing else.

const (
	fleetSessionTurnsDefault = 100
	fleetSessionPaneMax      = 64 << 10
	fleetSessionPaneLines    = 200
	fleetGrokSendNote        = "Send is not available for Grok sessions"
)

var fleetSessionNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

type fleetSessionErr struct {
	status    int
	code, msg string
}

type fleetSessionErrBody struct {
	OK     bool   `json:"ok"`
	Code   string `json:"code"`
	Error  string `json:"error"`
	Target string `json:"target,omitempty"`
}

func writeFleetSessionErr(w http.ResponseWriter, e *fleetSessionErr, target string) {
	writeJSON(w, e.status, fleetSessionErrBody{Code: e.code, Error: e.msg, Target: target})
}

// tmuxSessionPart is the session name in a "session:win.pane" address (or a
// card's "name:0.0").
func tmuxSessionPart(t string) string {
	name, _, _ := strings.Cut(strings.TrimSpace(t), ":")
	return name
}

// fleetCardInfo is what the session view shows from the card naming a session.
type fleetCardInfo struct {
	Ticket, Title, Kind string
}

// fleetSessionCards maps every tmux session named by a card in the current
// state.json (the mtime-checked loader: a bad file serves the last good copy)
// to the first card naming it. A card field of the wrong type is ignored,
// not the whole card.
func (s *server) fleetSessionCards() map[string]fleetCardInfo {
	state, _ := s.loadFleetState()
	var cards []map[string]json.RawMessage
	_ = json.Unmarshal(state["cards"], &cards)
	str := func(m map[string]json.RawMessage, k string) string {
		var v string
		_ = json.Unmarshal(m[k], &v)
		return v
	}
	out := map[string]fleetCardInfo{}
	for _, c := range cards {
		name := tmuxSessionPart(str(c, "session"))
		if name == "" {
			continue
		}
		if _, seen := out[name]; !seen {
			out[name] = fleetCardInfo{Ticket: str(c, "ticket"), Title: str(c, "title"), Kind: str(c, "kind")}
		}
	}
	return out
}

// fleetResolveSession is the allowlist, in order: a well-formed name; a card
// in the current state.json names it, or it is a live row of the working
// list (the page may open only what FLEET put on a card or what the server
// itself lists); not FLEET's own session (FLEET has its own guarded box, and
// typing into it from here would skip those guards); exactly one live row
// (claude or grok) in that tmux session, so the pane address is unambiguous.
// With no card, the ticket and title come from that row.
func (s *server) fleetResolveSession(name string, sessions []Session) (Session, fleetCardInfo, *fleetSessionErr) {
	if !fleetSessionNameRe.MatchString(name) {
		return Session{}, fleetCardInfo{}, &fleetSessionErr{http.StatusBadRequest, "bad_target", "bad session name"}
	}
	card, ok := s.fleetSessionCards()[name]
	if !ok && !s.fleetIsDerived(name, sessions) {
		return Session{}, fleetCardInfo{}, &fleetSessionErr{http.StatusForbidden, "not_allowed", "no FLEET card or live session list row names this session"}
	}
	self := &fleetSessionErr{http.StatusForbidden, "fleet_self", "this is FLEET's own session"}
	if fl, reason := findFleet(sessions); reason == "" && fl.Tmux != "" && tmuxSessionPart(fl.Tmux) == name {
		return Session{}, card, self
	}
	var hits []Session
	for _, sess := range sessions {
		if sess.Tmux == "" || tmuxSessionPart(sess.Tmux) != name {
			continue
		}
		if sess.Name == fleetSessionName {
			return Session{}, card, self
		}
		hits = append(hits, sess)
	}
	switch len(hits) {
	case 0:
		return Session{}, card, &fleetSessionErr{http.StatusNotFound, "gone", "this session has ended"}
	case 1:
		if !ok {
			card = fleetCardInfo{Ticket: strings.Join(sessionTickets(hits[0]), ", "), Title: hits[0].Name}
		}
		return hits[0], card, nil
	default:
		return Session{}, card, &fleetSessionErr{http.StatusConflict, "ambiguous", "two or more sessions share this tmux session"}
	}
}

// fleetIsDerived reports whether a working-list row runs in tmux session
// name, on the list the caller resolved from.
func (s *server) fleetIsDerived(name string, sessions []Session) bool {
	for _, row := range s.fleetDerivedSessions(sessions) {
		if tmuxSessionPart(row.Tmux) == name {
			return true
		}
	}
	return false
}

// fleetSessionTags are the wrapper blocks Claude Code puts inside turns:
// reminders, slash-command and ! bash plumbing, hook output and task
// notifications, not words anyone typed.
var fleetSessionTags = []string{"system-reminder", "command-name", "command-message", "command-args",
	"local-command-stdout", "local-command-stderr", "local-command-caveat",
	"bash-input", "bash-stdout", "bash-stderr", "task-notification", "user-prompt-submit-hook"}

var fleetSessionTagRe = func() *regexp.Regexp {
	alt := strings.Join(fleetSessionTags, "|")
	return regexp.MustCompile(`(?s)<(?:` + alt + `)>.*?</(?:` + alt + `)>`)
}()

// stripSessionTags removes every wrapper block. With cutUnclosed (user
// text), an opening tag with no close drops everything after it, so a
// cut-off reminder never leaks; assistant text keeps a tag it only quotes.
func stripSessionTags(text string, cutUnclosed bool) string {
	text = fleetSessionTagRe.ReplaceAllString(text, "")
	if cutUnclosed {
		for _, tag := range fleetSessionTags {
			if i := strings.Index(text, "<"+tag+">"); i >= 0 {
				text = text[:i]
			}
		}
	}
	return strings.TrimSpace(text)
}

// contentHasBlock reports whether a message content array holds a block of
// type typ.
func contentHasBlock(raw json.RawMessage, typ string) bool {
	var blocks []struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return false
	}
	for _, b := range blocks {
		if b.Type == typ {
			return true
		}
	}
	return false
}

// fleetSessionText is the session view's filter: text blocks only (never
// thinking or tool_use); no compact summary and no user line carrying a
// tool_result; wrapper blocks removed; a turn empty after that, or a user
// turn still starting with "<", is skipped. isMeta lines are skipped by
// scanFleetTurns.
func fleetSessionText(e *transcriptLine) (string, bool) {
	user := e.Type == "user"
	if e.IsCompactSummary || (user && contentHasBlock(e.Message.Content, "tool_result")) {
		return "", false
	}
	text, ok := extractTurnText(e.Message.Content)
	if !ok {
		return "", false
	}
	text = stripSessionTags(text, user)
	// A user line that still opens with a tag is plumbing this list does not
	// know yet (the FLEET chat's rule).
	if text == "" || (user && strings.HasPrefix(text, "<")) {
		return "", false
	}
	return text, true
}

// tmuxPaneHistory captures the last fleetSessionPaneLines lines of a pane,
// wrapped lines joined, without escapes.
func tmuxPaneHistory(target string) (string, error) {
	if target == "" {
		return "", errors.New("no tmux pane")
	}
	out, err := exec.Command("tmux", "capture-pane", "-p", "-J", "-S", "-"+strconv.Itoa(fleetSessionPaneLines), "-t", tmuxExactTarget(target)).Output()
	if err != nil {
		return "", fmt.Errorf("capture pane: %w", err)
	}
	return string(out), nil
}

// fleetSessionPane is the pane text for the view: control bytes and escapes
// stripped per line, trailing blanks dropped, at most fleetSessionPaneMax
// bytes from the end, cut at a line start.
func (s *server) fleetSessionPane(target string) string {
	hist := s.fleet.paneHistory
	if hist == nil {
		hist = tmuxPaneHistory
	}
	raw, err := hist(tmuxExactTarget(target))
	if err != nil {
		return ""
	}
	lines := strings.Split(raw, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(stripPaneEscapes(l), " ")
	}
	text := strings.TrimRight(strings.Join(lines, "\n"), "\n")
	if len(text) > fleetSessionPaneMax {
		text = text[len(text)-fleetSessionPaneMax:]
		if i := strings.IndexByte(text, '\n'); i >= 0 {
			text = text[i+1:]
		}
		for len(text) > 0 && !utf8.RuneStart(text[0]) { // one huge line: do not start mid-rune
			text = text[1:]
		}
	}
	return text
}

type fleetSessionTarget struct {
	Tmux      string `json:"tmux"`
	Ticket    string `json:"ticket"`
	Title     string `json:"title"`
	Kind      string `json:"kind"`
	Model     string `json:"model"`
	Status    string `json:"status"`
	Tool      string `json:"tool"`
	SessionID string `json:"sessionId"`
	CanSend   bool   `json:"canSend"`
	SendNote  string `json:"sendNote,omitempty"`
}

func fleetSessionTargetOf(name string, row Session, card fleetCardInfo) fleetSessionTarget {
	t := fleetSessionTarget{Tmux: name, Ticket: card.Ticket, Title: card.Title, Kind: card.Kind,
		Model: row.Model, Status: fleetStatus(row), Tool: "claude", SessionID: row.SessionID, CanSend: true}
	if row.IsGrok() {
		t.Tool, t.CanSend, t.SendNote = "grok", false, fleetGrokSendNote
	}
	return t
}

type fleetSessionViewBody struct {
	Target fleetSessionTarget `json:"target"`
	Source string             `json:"source"`
	Turns  []fleetChatTurn    `json:"turns"`
	Pos    int64              `json:"pos"`
	Reset  bool               `json:"reset"`
	Older  int64              `json:"older"`
	Pane   *string            `json:"pane,omitempty"`
}

// fleetSessionGet is GET /fleet/session?t=<tmux>[&since=][&before=][&n=].
func (s *server) fleetSessionGet(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.fleetAuth(r) == fleetAuthNone {
		writeJSON(w, http.StatusUnauthorized, fleetSessionErrBody{Code: "unauthorized", Error: "Unauthorized"})
		return
	}
	q := r.URL.Query()
	name := q.Get("t")
	num := func(key string, lo, hi int64, def int64) (int64, bool) {
		v := q.Get(key)
		if v == "" {
			return def, true
		}
		p, err := strconv.ParseInt(v, 10, 64)
		return p, err == nil && p >= lo && p <= hi
	}
	since, ok1 := num("since", 0, 1<<62, 0)
	before, ok2 := num("before", 1, 1<<62, -1)
	n, ok3 := num("n", 1, 200, fleetSessionTurnsDefault)
	if !ok1 || !ok2 || !ok3 {
		writeJSON(w, http.StatusBadRequest, fleetSessionErrBody{Code: "bad_request", Error: "bad since, before or n"})
		return
	}
	sessions, err := s.cachedSessions()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, fleetSessionErrBody{Code: "list_failed", Error: "session list failed"})
		return
	}
	row, card, rerr := s.fleetResolveSession(name, sessions)
	if rerr != nil {
		writeFleetSessionErr(w, rerr, "")
		return
	}
	out := fleetSessionViewBody{Target: fleetSessionTargetOf(name, row, card), Source: "pane", Turns: []fleetChatTurn{}, Reset: true}
	if path := s.fleetSessionTranscript(row); path != "" {
		var turns []fleetChatTurn
		var err error
		if before > 0 {
			turns, out.Older, err = extractTurnsBefore(path, before, int(n), fleetSessionText)
			out.Reset = false
		} else {
			turns, out.Pos, out.Reset, out.Older, err = extractTurns(path, since, int(n), fleetChatTailBytes, fleetSessionText)
		}
		if err == nil {
			out.Source = "transcript"
			if turns != nil {
				out.Turns = turns
			}
		}
	}
	if out.Source == "pane" {
		out.Pos, out.Reset, out.Older = 0, true, 0
		pane := s.fleetSessionPane(row.Tmux)
		out.Pane = &pane
	}
	writeJSON(w, http.StatusOK, out)
}

// fleetSessionTranscript is the claude transcript of row, or "" (grok, an
// unusable id, or none on disk): the view then shows the pane.
func (s *server) fleetSessionTranscript(row Session) string {
	if row.IsGrok() || validateResumeSessionID(row.SessionID) != nil {
		return ""
	}
	home, err := s.fleetHome()
	if err != nil {
		return ""
	}
	return findTranscript(home, row.SessionID)
}

type fleetSessionSayResult struct {
	fleetSayResult
	Target string `json:"target"`
}

func decodeFleetSessionSayBody(w http.ResponseWriter, r *http.Request) (target, text string, err error) {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	dec.DisallowUnknownFields()
	var body *struct {
		Target string `json:"target"`
		Text   string `json:"text"`
	}
	if err := dec.Decode(&body); err != nil || body == nil {
		return "", "", errors.New("bad json")
	}
	if _, err := dec.Token(); err != io.EOF {
		return "", "", errors.New("bad json")
	}
	return body.Target, body.Text, nil
}

var fleetSayOther = fleetSayWho{subject: "The session", pane: "the session pane"}

// fleetSessionSay is POST /fleet/session/say {target, text}: the /fleet/say
// pipeline (fleetSayGateLocked, fleetTypeInto) aimed at an allowlisted
// session, resolved from a fresh collect and a fresh state.json read. Sends
// are logged to the say log as target and length only, never the text, and
// never to log.jsonl: that log is FLEET's own record.
func (s *server) fleetSessionSay(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	kind := s.fleetAuth(r)
	if kind == fleetAuthNone {
		writeJSON(w, http.StatusUnauthorized, fleetSessionErrBody{Code: "unauthorized", Error: "Unauthorized"})
		return
	}
	if kind == fleetAuthCookie && !sameOrigin(r) {
		writeJSON(w, http.StatusForbidden, fleetSessionErrBody{Code: "origin", Error: "origin mismatch"})
		return
	}
	name, raw, err := decodeFleetSessionSayBody(w, r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, fleetSessionErrBody{Code: "bad_json", Error: "bad json"})
		return
	}
	text, code, msg := fleetCleanSayText(raw)
	if msg != "" {
		writeJSON(w, http.StatusBadRequest, fleetSessionErrBody{Code: code, Error: msg})
		return
	}
	if !fleetSessionNameRe.MatchString(name) {
		writeFleetSessionErr(w, &fleetSessionErr{http.StatusBadRequest, "bad_target", "bad session name"}, "")
		return
	}

	f := &s.fleet
	f.sayMu.Lock()
	defer f.sayMu.Unlock()
	key := name + "\x00" + text // never equals a FLEET key: text holds no control bytes
	if res := s.fleetSayGateLocked(key); res != nil {
		writeJSON(w, http.StatusOK, fleetSessionSayResult{*res, name})
		return
	}
	// Fresh, not cached: the pane address must describe the session now.
	sessions, err := s.collectLocal()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, fleetSessionErrBody{Code: "list_failed", Error: "session list failed", Target: name})
		return
	}
	row, _, rerr := s.fleetResolveSession(name, sessions)
	if rerr != nil {
		writeFleetSessionErr(w, rerr, name)
		return
	}
	if row.IsGrok() {
		// fleetTypeInto's input-box parser knows only Claude Code's screen.
		writeFleetSessionErr(w, &fleetSessionErr{http.StatusConflict, "send_unsupported", fleetGrokSendNote}, name)
		return
	}
	res := s.fleetTypeInto(row, text, fleetSayOther)
	if res.OK {
		done := s.fleetSayDoneLocked(key)
		s.logFleetSessionSend(done, name, text)
	}
	writeJSON(w, http.StatusOK, fleetSessionSayResult{res, name})
}

// logFleetSessionSend appends "<time> sent target=<tmux> len=<n>" to the say
// log: enough to see that a send happened, without the words.
func (s *server) logFleetSessionSend(at time.Time, name, text string) {
	path := s.fleet.sayLogPath
	if path == "" {
		path = fleetSayLogDefault()
	}
	if path == "" {
		return
	}
	appendFleetSayLog(path, fmt.Sprintf("%s sent target=%s len=%d\n", at.UTC().Format(time.RFC3339), name, utf8.RuneCountInString(text)))
}
