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
		s.logFleetRefusal("dialog", screen)
		refuse("dialog", "FLEET input box is not visible")
		return
	} else if typed != "" {
		s.logFleetRefusal("input_busy", screen)
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
		s.logFleetRefusal("not_submitted", lastScreen)
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

// fleetRefusalLogLines is how much of the screen a refused say logs.
const fleetRefusalLogLines = 15

// logFleetRefusal writes the bottom of the pane that made a say refuse to the
// server's stderr, so a refusal nobody can reproduce later ("the box is not
// visible", but it looks fine now) still leaves the screen behind. Stderr
// only, never the HTTP response: the pane can hold anything. Escapes are
// stripped but dim text is kept — the placeholder is part of what was shown.
func (s *server) logFleetRefusal(code, screen string) {
	logf := s.fleet.logf
	if logf == nil {
		logf = func(format string, args ...any) { fmt.Fprintf(os.Stderr, format, args...) }
	}
	lines := strings.Split(strings.TrimRight(screen, "\n"), "\n")
	if len(lines) > fleetRefusalLogLines {
		lines = lines[len(lines)-fleetRefusalLogLines:]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "fleet: say refused (%s); last %d pane lines:\n", code, fleetRefusalLogLines)
	for _, l := range lines {
		l = strings.ReplaceAll(stripPaneEscapes(l), "\u00a0", " ")
		b.WriteString("  | " + strings.TrimRight(l, " ") + "\n")
	}
	logf("%s", b.String())
}
