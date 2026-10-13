package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	fleetSayGap       = time.Second
	fleetSayDoubleTap = 10 * time.Second
	fleetLandPolls    = 15
	fleetLandEvery    = 100 * time.Millisecond
)

type fleetSayResult struct {
	OK     bool   `json:"ok"`
	Queued bool   `json:"queued,omitempty"`
	Error  string `json:"error,omitempty"`
	Code   string `json:"code,omitempty"`
}

// sanitizeFleetText makes page text safe to type as one prompt: a literal
// newline would press Enter mid-message, and other control bytes (ESC) could
// drive the Claude TUI, so newlines flatten to spaces (as scripts/send.sh
// does) and every other control character is refused.
func sanitizeFleetText(s string) (string, error) {
	if !utf8.ValidString(s) {
		return "", errors.New("bad text")
	}
	s = strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ", "\t", " ").Replace(s)
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return "", errors.New("bad text")
		}
	}
	s = strings.TrimSpace(s)
	if s == "" || len(s) > sendKeysMaxLen {
		return "", errors.New("bad text")
	}
	return s, nil
}

// sameOrigin is the CSRF guard for cookie-authenticated sends.
func sameOrigin(r *http.Request) bool {
	src := r.Header.Get("Origin")
	if src == "" {
		src = r.Header.Get("Referer")
	}
	if src == "" || src == "null" {
		return false
	}
	u, err := url.Parse(src)
	return err == nil && u.Host != "" && strings.EqualFold(u.Host, r.Host)
}

func decodeFleetSayBody(w http.ResponseWriter, r *http.Request) (text, cardID string, err error) {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	dec.DisallowUnknownFields()
	var body *struct {
		Text   string  `json:"text"`
		CardID *string `json:"cardId"`
	}
	if err := dec.Decode(&body); err != nil || body == nil {
		return "", "", errors.New("bad json")
	}
	if _, err := dec.Token(); err != io.EOF {
		return "", "", errors.New("bad json")
	}
	if body.CardID != nil {
		cardID = *body.CardID
	}
	return body.Text, cardID, nil
}

// tmuxTypeLiteral types text into target as literal keys. The "--" ends
// tmux's flag parsing: without it a message that starts with "-" is read as
// flags ("-R hello" fails with "invalid flag"; others could change what
// send-keys does).
func tmuxTypeLiteral(target, text string) error {
	if target == "" {
		return errors.New("no tmux pane")
	}
	return exec.Command("tmux", "send-keys", "-t", tmuxExactTarget(target), "-l", "--", text).Run()
}

// fleetTextLanded reports whether the input box holds what was just typed.
// Claude may wrap a long line or still be drawing it, so the box text only
// has to be a non-empty prefix of the sent text, whitespace ignored. A long
// paste shows as a "[Pasted text ...]" placeholder instead.
func fleetTextLanded(box, sent string) bool {
	if strings.HasPrefix(box, "[Pasted text") {
		return true
	}
	b, t := stripSpace(box), stripSpace(sent)
	return b != "" && strings.HasPrefix(t, b)
}

func stripSpace(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
}

func tmuxPressEnter(target string) error {
	if target == "" {
		return errors.New("no tmux pane")
	}
	return exec.Command("tmux", "send-keys", "-t", tmuxExactTarget(target), "Enter").Run()
}

// fleetEventTag marks a line the server itself typed into FLEET (idle
// events). FLEET treats a tagged line as a status report, so a page send must
// never be able to forge one.
const fleetEventTag = "[claude-sessions event]"

// fleetEventTagRe finds the tag anywhere, in any case, with any whitespace
// inside the brackets (newlines too: flattening turns a later line into
// mid-text, so "only at the start" would not be enough).
var fleetEventTagRe = regexp.MustCompile(`(?i)\[\s*claude-sessions\s+event\s*\]`)

const fleetReservedTagMsg = "Text may not contain [claude-sessions event]. That tag is reserved for server events."

// fleetCleanSayText makes raw safe to type as one prompt. When it is not,
// code is "reserved_tag" or "bad_text" and msg the 400 reason. Every page
// send goes through here; the idle-event sender does not.
func fleetCleanSayText(raw string) (text, code, msg string) {
	if fleetEventTagRe.MatchString(raw) {
		return "", "reserved_tag", fleetReservedTagMsg
	}
	text, err := sanitizeFleetText(raw)
	if err != nil {
		return "", "bad_text", "bad text"
	}
	// Claude Code switches to bash mode on a leading "!" and memory mode on
	// "#". The box glyph changes, so every later send would refuse until the
	// box is cleared at the terminal.
	if text[0] == '!' || text[0] == '#' {
		return "", "bad_text", "text must not start with ! or #"
	}
	return text, "", ""
}

// fleetSayGateLocked applies the limits every page send shares: a 1 s gap
// after any send (FLEET or another session), and no second send of the same
// key within 10 s. Caller holds sayMu.
func (s *server) fleetSayGateLocked(key string) *fleetSayResult {
	f := &s.fleet
	now := s.fleetNow()
	for k, at := range f.saidAt {
		if now.Sub(at) >= fleetSayDoubleTap {
			delete(f.saidAt, k)
		}
	}
	if !f.lastSayAt.IsZero() && now.Sub(f.lastSayAt) < fleetSayGap {
		return &fleetSayResult{Code: "rate_limited", Error: "too fast"}
	}
	if _, dup := f.saidAt[key]; dup {
		return &fleetSayResult{Code: "double_tap", Error: "same text sent less than 10 s ago"}
	}
	return nil
}

// fleetSayDoneLocked stamps a completed send for the gate. Caller holds sayMu.
func (s *server) fleetSayDoneLocked(key string) time.Time {
	f := &s.fleet
	done := s.fleetNow()
	f.lastSayAt = done
	if f.saidAt == nil {
		f.saidAt = map[string]time.Time{}
	}
	f.saidAt[key] = done
	return done
}

// fleetSayWho names the send target in refusal messages.
type fleetSayWho struct {
	subject string // "FLEET"
	pane    string // "the FLEET pane"
	paneLog bool   // refusal log entries may hold screen lines
}

var fleetSayFLEET = fleetSayWho{subject: "FLEET", pane: "the FLEET pane", paneLog: true}

// fleetTypeInto is the pane half of every page send: refuse a session that
// waits on a dialog, has no pane, shows no empty input box, or is in copy
// mode; type the text; press Enter only once the box holds it. row must be a
// freshly resolved session: its Tmux is the only pane address used. Caller
// holds sayMu. The result is the response envelope; ok false with code
// not_submitted means the text was typed but Enter was not pressed.
func (s *server) fleetTypeInto(row Session, text string, who fleetSayWho) fleetSayResult {
	f := &s.fleet
	refuse := func(code, msg string) fleetSayResult { return fleetSayResult{Code: code, Error: msg} }
	if row.Waiting() {
		return refuse("dialog", who.subject+" shows a dialog. Answer it in the terminal.")
	}
	if row.Tmux == "" {
		return refuse("no_pane", who.subject+" has no tmux pane")
	}
	pane := tmuxExactTarget(row.Tmux) // "=" so a gone session never prefix-matches another
	capture := f.capture
	if capture == nil {
		capture = capturePane
	}
	screen, err := capture(pane)
	if err != nil {
		return refuse("no_pane", "cannot read "+who.pane)
	}
	if typed, found := paneInputText(screen); !found {
		s.logFleetSendRefusal(who, "dialog", row.Tmux, screen, text)
		return refuse("dialog", who.subject+" input box is not visible")
	} else if typed != "" {
		s.logFleetSendRefusal(who, "input_busy", row.Tmux, screen, text)
		return refuse("input_busy", "input box not empty")
	}
	inMode := f.paneInMode
	if inMode == nil {
		inMode = tmuxPaneInMode
	}
	if mode, err := inMode(pane); err != nil {
		return refuse("no_pane", "cannot read "+who.pane)
	} else if mode {
		return refuse("pane_mode", strings.ToUpper(who.pane[:1])+who.pane[1:]+" is in scroll mode. Press q in the terminal, then try again.")
	}

	typeText, pressEnter, sleep := f.typeText, f.pressEnter, f.sleep
	if typeText == nil {
		typeText = tmuxTypeLiteral
	}
	if pressEnter == nil {
		pressEnter = tmuxPressEnter
	}
	if sleep == nil {
		sleep = time.Sleep
	}
	if err := typeText(pane, text); err != nil {
		return refuse("send_failed", "send text: "+err.Error())
	}
	// The first capture is a snapshot: a dialog may have opened since, and
	// Enter would pick its highlighted option. Press Enter only once the box
	// is still there and holds what was just typed (it was empty before).
	// Other text in the box is not ours: keep polling, then give up.
	landed := false
	lastScreen := ""
	for i := 0; i < fleetLandPolls && !landed; i++ {
		if i > 0 {
			sleep(fleetLandEvery)
		}
		screen, err := capture(pane)
		if err != nil {
			continue
		}
		lastScreen = screen
		typed, found := paneInputText(screen)
		if !found {
			break
		}
		landed = fleetTextLanded(typed, text)
	}
	if !landed {
		s.logFleetSendRefusal(who, "not_submitted", row.Tmux, lastScreen, text)
		return refuse("not_submitted", "Text was typed but not sent. "+who.subject+" changed state. Check the terminal.")
	}
	if err := pressEnter(pane); err != nil {
		return refuse("send_failed", "send enter: "+err.Error())
	}
	return fleetSayResult{OK: true, Queued: row.Status == "busy"}
}

// logFleetSendRefusal logs a pane refusal. FLEET's own entries keep the
// screen lines (logFleetRefusal). Any other session's entry is the header
// line only, with the text length: its pane can hold the typed text, and
// the say log must never hold what was sent to a session.
func (s *server) logFleetSendRefusal(who fleetSayWho, code, target, screen, text string) {
	if who.paneLog {
		s.logFleetRefusal(code, target, screen)
		return
	}
	path := s.fleet.sayLogPath
	if path == "" {
		path = fleetSayLogDefault()
	}
	if path == "" {
		return
	}
	appendFleetSayLog(path, fmt.Sprintf("%s refused code=%s target=%s len=%d\n",
		s.fleetNow().UTC().Format(time.RFC3339), code, tmuxSessionPart(target), utf8.RuneCountInString(text)))
}

func (s *server) fleetSay(w http.ResponseWriter, r *http.Request) {
	kind := s.fleetAuth(r)
	if kind == fleetAuthNone {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if kind == fleetAuthCookie && !sameOrigin(r) {
		writeJSON(w, http.StatusForbidden, fleetSayResult{Error: "origin mismatch", Code: "origin"})
		return
	}
	raw, cardID, err := decodeFleetSayBody(w, r)
	if err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if len(cardID) > 200 {
		http.Error(w, "bad cardId", http.StatusBadRequest)
		return
	}
	text, code, msg := fleetCleanSayText(raw)
	if code == "reserved_tag" {
		writeJSON(w, http.StatusBadRequest, fleetSayResult{Code: code, Error: msg})
		return
	}
	if msg != "" {
		http.Error(w, msg, http.StatusBadRequest)
		return
	}

	f := &s.fleet
	f.sayMu.Lock()
	defer f.sayMu.Unlock()
	refuse := func(code, msg string) { writeJSON(w, http.StatusOK, fleetSayResult{Error: msg, Code: code}) }

	now := s.fleetNow()
	if res := s.fleetSayGateLocked(text); res != nil {
		writeJSON(w, http.StatusOK, *res)
		return
	}

	row, fl := s.discoverFleet()
	if !fl.Alive {
		if fl.Reason == "ambiguous" {
			refuse("ambiguous", "two sessions are named FLEET")
		} else {
			refuse("not_live", "FLEET is not running")
		}
		return
	}
	if row.SessionID == "" {
		refuse("not_live", "FLEET has no session id")
		return
	}
	fresh, all, refusal := s.resolveLivePID(row.PID, row.SessionID)
	if refusal != nil {
		refuse(refusal.Code, refusal.Error)
		return
	}
	if again, reason := findFleet(all); reason != "" || again.PID != row.PID || again.SessionID != row.SessionID {
		refuse("fleet_changed", "FLEET changed. Try again.")
		return
	}
	res := s.fleetTypeInto(*fresh, text, fleetSayFLEET)
	if !res.OK {
		if res.Code == "not_submitted" {
			_ = s.appendFleetLog(fleetLogEntry{At: now.UTC().Format(time.RFC3339), Type: "note",
				Text: "web-say typed but not submitted", CardID: cardID})
		}
		writeJSON(w, http.StatusOK, res)
		return
	}
	done := s.fleetSayDoneLocked(text)
	if err := s.appendFleetLog(fleetLogEntry{At: done.UTC().Format(time.RFC3339), Type: "web-say", Text: text, CardID: cardID}); err != nil {
		fmt.Fprintf(os.Stderr, "fleet: web-say log append failed: %v\n", err)
	}
	writeJSON(w, http.StatusOK, res)
}

const (
	// fleetRefusalLogLines is how much of the screen a refused say logs as
	// text; fleetRefusalRawLines how much of it also goes in raw, escapes and
	// all, for when the parser itself is what is wrong.
	fleetRefusalLogLines = 15
	fleetRefusalRawLines = 6
	// fleetSayLogMax: past this size the log moves to .1 before an append.
	fleetSayLogMax = 1 << 20
)

// logFleetRefusal appends the bottom of the pane that made a say refuse to
// the say log file, so a refusal nobody can reproduce later ("the box is not
// visible", but it looks fine now) still leaves the screen behind. A file,
// not stderr: the systemd unit runs with StandardOutput=null, and in practice
// journald showed nothing from the server, so the stderr log reached nobody.
// Never the HTTP response: the pane can hold anything. The
// text lines have escapes stripped but keep dim text (the placeholder is part
// of what was shown); the raw lines keep every byte, %q-quoted.
func (s *server) logFleetRefusal(code, target, screen string) {
	path := s.fleet.sayLogPath
	if path == "" {
		path = fleetSayLogDefault()
	}
	if path == "" {
		return
	}
	lines := strings.Split(strings.TrimRight(screen, "\n"), "\n")
	var b strings.Builder
	fmt.Fprintf(&b, "%s refused code=%s target=%s\n", s.fleetNow().UTC().Format(time.RFC3339), code, target)
	for _, l := range lastN(lines, fleetRefusalLogLines) {
		l = strings.ReplaceAll(stripPaneEscapes(l), "\u00a0", " ")
		b.WriteString("  | " + strings.TrimRight(l, " ") + "\n")
	}
	for _, l := range lastN(lines, fleetRefusalRawLines) {
		fmt.Fprintf(&b, "  raw %q\n", l)
	}
	b.WriteString("\n")
	appendFleetSayLog(path, b.String())
}

func lastN(lines []string, n int) []string {
	if len(lines) > n {
		return lines[len(lines)-n:]
	}
	return lines
}

// fleetSayLogDefault is $XDG_STATE_HOME/claude-sessions/fleet-say.log, or
// ~/.local/state/claude-sessions/fleet-say.log when that is unset (or not
// absolute, which the XDG spec says to ignore). "" when there is no home.
func fleetSayLogDefault() string {
	if x := os.Getenv("XDG_STATE_HOME"); filepath.IsAbs(x) {
		return filepath.Join(x, "claude-sessions", "fleet-say.log")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "state", "claude-sessions", "fleet-say.log")
}

// appendFleetSayLog appends one whole entry with a single O_APPEND write
// (dir 0700, file 0600). A file over fleetSayLogMax first moves to path+".1",
// replacing an older one. Errors are dropped: a log that cannot be written
// must never fail or change a say.
func appendFleetSayLog(path, entry string) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	if st, err := os.Stat(path); err == nil && st.Size() > fleetSayLogMax {
		_ = os.Rename(path, path+".1")
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return
	}
	_, _ = f.WriteString(entry)
	_ = f.Close()
}
