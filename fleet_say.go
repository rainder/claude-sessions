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
	return exec.Command("tmux", "send-keys", "-t", target, "-l", "--", text).Run()
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
	return exec.Command("tmux", "send-keys", "-t", target, "Enter").Run()
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
	text, err := sanitizeFleetText(raw)
	if err != nil {
		http.Error(w, "bad text", http.StatusBadRequest)
		return
	}
	// Claude Code switches to bash mode on a leading "!" and memory mode on
	// "#". The box glyph changes, so every later send would refuse until the
	// box is cleared at the terminal.
	if text[0] == '!' || text[0] == '#' {
		http.Error(w, "text must not start with ! or #", http.StatusBadRequest)
		return
	}

	f := &s.fleet
	f.sayMu.Lock()
	defer f.sayMu.Unlock()
	refuse := func(code, msg string) { writeJSON(w, http.StatusOK, fleetSayResult{Error: msg, Code: code}) }

	now := s.fleetNow()
	for t, at := range f.saidAt {
		if now.Sub(at) >= fleetSayDoubleTap {
			delete(f.saidAt, t)
		}
	}
	if !f.lastSayAt.IsZero() && now.Sub(f.lastSayAt) < fleetSayGap {
		refuse("rate_limited", "too fast")
		return
	}
	if _, dup := f.saidAt[text]; dup {
		refuse("double_tap", "same text sent less than 10 s ago")
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
	if fresh.Waiting() {
		refuse("dialog", "FLEET shows a dialog. Answer it in the terminal.")
		return
	}
	if fresh.Tmux == "" {
		refuse("no_pane", "FLEET has no tmux pane")
		return
	}
	capture := f.capture
	if capture == nil {
		capture = capturePane
	}
	screen, err := capture(fresh.Tmux)
	if err != nil {
		refuse("no_pane", "cannot read the FLEET pane")
		return
	}
	if typed, found := paneInputText(screen); !found {
		s.logFleetRefusal("dialog", fresh.Tmux, screen)
		refuse("dialog", "FLEET input box is not visible")
		return
	} else if typed != "" {
		s.logFleetRefusal("input_busy", fresh.Tmux, screen)
		refuse("input_busy", "input box not empty")
		return
	}
	inMode := f.paneInMode
	if inMode == nil {
		inMode = tmuxPaneInMode
	}
	if mode, err := inMode(fresh.Tmux); err != nil {
		refuse("no_pane", "cannot read the FLEET pane")
		return
	} else if mode {
		refuse("pane_mode", "The FLEET pane is in scroll mode. Press q in the terminal, then try again.")
		return
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
	if err := typeText(fresh.Tmux, text); err != nil {
		refuse("send_failed", "send text: "+err.Error())
		return
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
		screen, err := capture(fresh.Tmux)
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
		_ = s.appendFleetLog(fleetLogEntry{At: now.UTC().Format(time.RFC3339), Type: "note",
			Text: "web-say typed but not submitted", CardID: cardID})
		s.logFleetRefusal("not_submitted", fresh.Tmux, lastScreen)
		refuse("not_submitted", "Text was typed but not sent. FLEET changed state. Check the terminal.")
		return
	}
	if err := pressEnter(fresh.Tmux); err != nil {
		refuse("send_failed", "send enter: "+err.Error())
		return
	}
	done := s.fleetNow()
	f.lastSayAt = done
	if f.saidAt == nil {
		f.saidAt = map[string]time.Time{}
	}
	f.saidAt[text] = done
	if err := s.appendFleetLog(fleetLogEntry{At: done.UTC().Format(time.RFC3339), Type: "web-say", Text: text, CardID: cardID}); err != nil {
		fmt.Fprintf(os.Stderr, "fleet: web-say log append failed: %v\n", err)
	}
	writeJSON(w, http.StatusOK, fleetSayResult{OK: true, Queued: fresh.Status == "busy"})
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
