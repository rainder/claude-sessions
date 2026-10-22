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

// fleetSayBody is a /fleet/say request. OptionKey, CardHash and Correction
// mean something only for a card, so any of them without CardID is a 400.
type fleetSayBody struct {
	Text, CardID, OptionKey, CardHash string
	Correction                        bool
}

// decodeFleetSayBody reads one strict JSON object. The error text is the 400
// body: "bad json" or "bad request".
func decodeFleetSayBody(w http.ResponseWriter, r *http.Request) (fleetSayBody, error) {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	dec.DisallowUnknownFields()
	// Pointers tell a field that is absent from one sent empty or false.
	var body *struct {
		Text       string  `json:"text"`
		CardID     *string `json:"cardId"`
		OptionKey  *string `json:"optionKey"`
		CardHash   *string `json:"cardHash"`
		Correction *bool   `json:"correction"`
	}
	if err := dec.Decode(&body); err != nil || body == nil {
		return fleetSayBody{}, errors.New("bad json")
	}
	if _, err := dec.Token(); err != io.EOF {
		return fleetSayBody{}, errors.New("bad json")
	}
	out := fleetSayBody{Text: body.Text}
	if body.CardID != nil {
		out.CardID = *body.CardID
	}
	if body.OptionKey != nil || body.CardHash != nil || body.Correction != nil {
		if out.CardID == "" {
			return fleetSayBody{}, errors.New("bad request")
		}
		if body.OptionKey != nil {
			out.OptionKey = *body.OptionKey
		}
		if body.CardHash != nil {
			out.CardHash = *body.CardHash
		}
		if body.Correction != nil {
			out.Correction = *body.Correction
		}
	}
	return out, nil
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

// fleetTextLandedFor is fleetTextLanded for row's tool. Grok caps its
// composer at about 30 lines and scrolls it, so a long text shows only a
// later part of it: for grok the box text has to be a non-empty piece of the
// sent text, anywhere in it, whitespace ignored. The box was empty before
// the text was typed, so whatever it holds now came from this send.
func fleetTextLandedFor(row Session, box, sent string) bool {
	if row.IsGrok() {
		b := stripSpace(box)
		return b != "" && strings.Contains(stripSpace(sent), b)
	}
	return fleetTextLanded(box, sent)
}

// fleetTextLanded reports whether the input box holds what was just typed.
// Claude or grok may wrap a long line or still be drawing it, so the box
// text only has to be a non-empty prefix of the sent text, whitespace
// ignored. A long paste shows in Claude Code as a "[Pasted text ...]"
// placeholder instead; grok wraps it inside the box (fleetTextLandedFor).
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

// fleetEventPhraseRe finds the tag's words in folded text (fleetFoldText),
// with any run of non-alphanumeric characters between them (a bounded run let
// "claude----sessions" through). FLEET is an LLM
// and reads text the way a person does, so "claude sessions event" or
// "claudesessions event" reads as the tag's words as much as the exact bytes
// do.
var fleetEventPhraseRe = regexp.MustCompile(`claude[^a-z0-9]*sessions?[^a-z0-9]*event`)

// fleetEventTagRe is the page-send refusal: the phrase after an opening
// bracket of any kind (folded to [ ( { <), with any non-alphanumeric run
// between (spaces, "--", "**"). A
// bare "claude-sessions events stopped" is ordinary text; the bracket is what
// makes it read as a tag. Anywhere in the text: flattening turns a later line
// into mid-text, so "only at the start" would not be enough.
var fleetEventTagRe = regexp.MustCompile(`[\[({<][^a-z0-9]*claude[^a-z0-9]*sessions?[^a-z0-9]*event`)

// fleetLookalikes maps Cyrillic, Greek and other letters that look like the
// Latin letters of the tag's words (c l a u d e s i o n v t) onto them. It is
// applied after lowercasing.
var fleetLookalikes = map[rune]rune{
	// Cyrillic
	'а': 'a', 'с': 'c', 'е': 'e', 'ё': 'e', 'і': 'i', 'ї': 'i', 'о': 'o', 'р': 'p', 'ѕ': 's',
	'у': 'y', 'х': 'x', 'ԁ': 'd', 'ӏ': 'l', 'п': 'n', 'ѵ': 'v', 'т': 't', 'ս': 'u',
	// Greek
	'α': 'a', 'ε': 'e', 'ι': 'i', 'ο': 'o', 'ν': 'v', 'τ': 't', 'υ': 'u', 'η': 'n',
	'ϲ': 'c', 'ς': 's', 'ϵ': 'e',
	// Latin and letterlike variants
	'ı': 'i', 'ɩ': 'i', 'ℓ': 'l', 'ℯ': 'e', 'ⅽ': 'c', 'ⅾ': 'd', 'ⅰ': 'i', 'ⅴ': 'v', 'ⅼ': 'l',
	'ᴄ': 'c', 'ᴅ': 'd', 'ᴇ': 'e', 'ꜱ': 's', 'ᴏ': 'o', 'ᴜ': 'u', 'ᴠ': 'v', 'ᴛ': 't', 'ʟ': 'l', 'ɴ': 'n',
	'ᴀ': 'a', 'ɪ': 'i',
	// Cherokee capitals that read as Latin
	'Ꮯ': 'c', 'Ꮮ': 'l', 'Ꭺ': 'a', 'Ꭼ': 'e', 'Ꮪ': 's', 'Ꭰ': 'd', 'Ꮩ': 'v', 'Ꭲ': 't',
}

// fleetMathAlnum maps Mathematical Alphanumeric Symbols (U+1D400–U+1D7FF:
// bold, italic, script, fraktur, double-struck, sans, monospace) onto the
// ASCII letter or digit, or the Greek letter, they style. ok is false
// outside that block or for a symbol with no plain form.
func fleetMathAlnum(r rune) (rune, bool) {
	switch {
	case r >= 0x1D400 && r < 0x1D6A4: // 13 Latin styles of A–Z, a–z
		off := (r - 0x1D400) % 52
		if off < 26 {
			return 'a' + off, true
		}
		return 'a' + off - 26, true
	case r == 0x1D6A4:
		return 'i', true
	case r == 0x1D6A5:
		return 'j', true
	case r >= 0x1D6A8 && r < 0x1D7CA: // 5 Greek styles, 58 symbols each
		off := (r - 0x1D6A8) % 58
		switch {
		case off < 25:
			return unicode.ToLower(0x391 + off), true
		case off >= 26 && off < 51:
			return 0x3B1 + off - 26, true
		}
		return 0, false
	case r >= 0x1D7CE && r <= 0x1D7FF: // 5 digit styles
		return '0' + (r-0x1D7CE)%10, true
	}
	return 0, false
}

// fleetFoldText maps lookalikes to the ASCII they look like, for the tag
// check only (the text that is typed is not changed). It drops format and
// zero-width characters (category Cf, which holds U+200B–U+200F, U+2060,
// U+FEFF and the soft hyphen) and combining marks (Mn); maps fullwidth ASCII
// U+FF01–U+FF5E and mathematical letters and digits to ASCII; maps dash
// variants to '-', middle dots to '.', opening and closing bracket lookalikes
// to '[' and ']'; collapses every run of Unicode whitespace to one space;
// lowercases; and maps Cyrillic and Greek lookalikes of the tag's letters.
func fleetFoldText(s string) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		switch {
		case unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Mn, r):
			continue
		case unicode.IsSpace(r):
			space = true
			continue
		case r >= 0xFF01 && r <= 0xFF5E:
			r -= 0xFEE0
		case r >= 0x1D400 && r <= 0x1D7FF:
			if m, ok := fleetMathAlnum(r); ok {
				r = m
			}
		case r >= 0x2010 && r <= 0x2015, r == 0x2043, r == 0x2212, r == 0xFE58, r == 0xFE63, r == 0x2E3A, r == 0x2E3B:
			r = '-'
		case strings.ContainsRune("⁅〈《「『【〔〖〘〚⟦⟨⟪⟬⟮⦃⦅⦇⦉⦋⦍⦏⦑⦓⦕⦗﹇﹝｢❨❪❬❮❰❲❴⁽₍﹙﹛﹤‹«〈", r):
			r = '['
		case strings.ContainsRune("⁆〉》」』】〕〗〙〛⟧⟩⟫⟭⟯⦄⦆⦈⦊⦌⦎⦐⦒⦔⦖⦘﹈﹞｣❩❫❭❯❱❳❵⁾₎﹚﹜﹥›»〉", r):
			r = ']'
		case r == 0x2024 || r == 0xFE52 || r == 0x00B7 || r == 0x2027 || r == 0x30FB || r == 0x2219 || r == 0x22C5:
			r = '.'
		case r == 0xFE4D || r == 0xFE4E || r == 0xFE4F:
			r = '_'
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		// Look up before and after lowercasing: a Cherokee capital lowercases
		// to a different-looking small letter.
		if m, ok := fleetLookalikes[r]; ok {
			r = m
		} else if m, ok := fleetLookalikes[unicode.ToLower(r)]; ok {
			r = m
		} else {
			r = unicode.ToLower(r)
		}
		b.WriteRune(r)
	}
	return b.String()
}

// fleetHasEventTag reports whether s, once folded, holds the tag: the
// phrase after an opening bracket. This is the page-send refusal.
func fleetHasEventTag(s string) bool {
	return fleetEventTagRe.MatchString(fleetFoldText(s))
}

// fleetHasEventPhrase reports whether s, once folded, holds the tag's words
// with or without a bracket. The idle-event sender keeps every field free of
// this, which is stricter than the page-send check.
func fleetHasEventPhrase(s string) bool {
	return fleetEventPhraseRe.MatchString(fleetFoldText(s))
}

const fleetReservedTagMsg = "Text may not contain [claude-sessions event]. That tag is reserved for server events."

// fleetCleanSayText makes raw safe to type as one prompt. When it is not,
// code is "reserved_tag" or "bad_text" and msg the 400 reason. Every page
// send goes through here; the idle-event sender does not.
func fleetCleanSayText(raw string) (text, code, msg string) {
	if fleetHasEventTag(raw) {
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
	// logGate, when set, decides whether a refusal is written to the say log
	// at all; the idle-event sender uses it to write a repeated refusal
	// once per 10 minutes rather than on every poll.
	logGate func(code string) bool
}

var fleetSayFLEET = fleetSayWho{subject: "FLEET", pane: "the FLEET pane", paneLog: true}

// fleetTypeInto is the pane half of every page send: refuse a session that
// waits on a dialog, is a busy grok session, has no pane, shows no empty
// input box (claude's or grok's, paneInputTextFor), or is in copy mode; type
// the text; press Enter only once the box holds it. row must be a freshly
// resolved session: its Tmux is the only pane address used. Caller
// holds sayMu. The result is the response envelope; ok false with code
// not_submitted means the text was typed but Enter was not pressed.
func (s *server) fleetTypeInto(row Session, text string, who fleetSayWho) fleetSayResult {
	f := &s.fleet
	refuse := func(code, msg string) fleetSayResult { return fleetSayResult{Code: code, Error: msg} }
	if row.Waiting() {
		return refuse("dialog", who.subject+" shows a dialog. Answer it in the terminal.")
	}
	// Claude queues a message typed mid-turn. Grok does not always: while
	// its turn waits on background work or a subagent, Enter with text
	// cancels that turn, and a bare Enter can force-send a queued one. Its
	// box looks the same idle or busy, so the status is the only guard, and
	// it fails closed: only idle, or shell (turn over, background work open),
	// types. An empty status (no readable events.jsonl yet) refuses too.
	if row.IsGrok() && row.Status != "idle" && row.Status != "shell" {
		return refuse("busy", who.subject+" is busy. Grok can cancel a running turn on Enter, so wait until its turn ends.")
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
	if typed, found := paneInputTextFor(row, screen); !found {
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
		typed, found := paneInputTextFor(row, screen)
		if !found {
			break
		}
		landed = fleetTextLandedFor(row, typed, text)
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
	if who.logGate != nil && !who.logGate(code) {
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
	body, err := decodeFleetSayBody(w, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	cardID := body.CardID
	if len(cardID) > 200 {
		http.Error(w, "bad cardId", http.StatusBadRequest)
		return
	}
	if len(body.OptionKey) > 100 {
		http.Error(w, "bad optionKey", http.StatusBadRequest)
		return
	}
	if len(body.CardHash) > 64 {
		http.Error(w, "bad cardHash", http.StatusBadRequest)
		return
	}
	text, code, msg := fleetCleanSayText(body.Text)
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
	// A card answer: the card must still be open and still ask what the
	// client showed, and must not have an answer already. Checked here,
	// under sayMu and right before typing, because the send may have waited
	// on sayMu (another send, an event line) while FLEET rewrote the card.
	var (
		cards map[string]fleetDecisionCard
		card  fleetDecisionCard
	)
	if cardID != "" {
		state, stateErr := s.loadFleetState()
		cards = fleetDecisionCards(state)
		var ok bool
		if card, ok = cards[cardID]; !ok {
			refuse("card_gone", "The card is no longer open.")
			return
		}
		if body.CardHash != "" && body.CardHash != card.Hash {
			refuse("card_changed", "The card changed.")
			return
		}
		prev, has := f.answered.get(cardID)
		if fleetAnswerRefused(prev, has, card.Hash, body.Correction) {
			refuse("already_answered", "Already answered.")
			return
		}
		if stateErr != "" {
			cards = nil // a bad state.json prunes nothing
		}
	}
	res := s.fleetTypeInto(*fresh, text, fleetSayFLEET)
	if cardID != "" && (res.OK || res.Code == "not_submitted") {
		s.fleetRecordAnswer(cardID, card, cards, body, text, res.OK)
	}
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

// fleetRecordAnswer stores a card answer that was typed: sent when Enter was
// pressed, unconfirmed when it was not. The label comes from the card, never
// the client; an unknown key records an empty one. The text itself is never
// stored, only whether a comment rode on the option's send text. cards nil
// (a bad state.json) prunes nothing. Caller holds sayMu.
func (s *server) fleetRecordAnswer(cardID string, card fleetDecisionCard, cards map[string]fleetDecisionCard,
	body fleetSayBody, text string, sent bool) {
	at := fleetAttempt{OptionKey: body.OptionKey, At: s.fleetNow().UTC().Format(time.RFC3339), Status: fleetAttemptUnconfirmed}
	if sent {
		at.Status = fleetAttemptSent
	}
	if label, send, ok := fleetOptionLabel(card.Card, body.OptionKey); ok {
		at.Label = label
		clean, _, _ := fleetCleanSayText(send)
		at.Comment = text != clean
	}
	s.fleet.answered.record(cardID, card.Hash, at, cards, s.fleetNow())
}

func (s *server) fleetCardDismiss(w http.ResponseWriter, r *http.Request) {
	kind := s.fleetAuth(r)
	if kind == fleetAuthNone {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if kind == fleetAuthCookie && !sameOrigin(r) {
		writeJSON(w, http.StatusForbidden, fleetSayResult{Error: "origin mismatch", Code: "origin"})
		return
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
	dec.DisallowUnknownFields()
	var body *struct {
		CardID string `json:"cardId"`
	}
	if err := dec.Decode(&body); err != nil || body == nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if _, err := dec.Token(); err != io.EOF {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if body.CardID == "" || len(body.CardID) > 200 {
		http.Error(w, "bad cardId", http.StatusBadRequest)
		return
	}
	if !s.fleet.answered.dismiss(body.CardID) {
		writeJSON(w, http.StatusOK, fleetSayResult{Code: "not_found", Error: "no answer for that card"})
		return
	}
	writeJSON(w, http.StatusOK, fleetSayResult{OK: true})
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
