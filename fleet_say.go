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
	// fleetLandPolls × fleetLandEvery is how long a typed text has to show
	// in the box before Enter: 3 s, for a slow-to-redraw FLEET.
	fleetLandPolls = 30
	fleetLandEvery = 100 * time.Millisecond
	// After Enter the box is read fleetEnterPolls times, fleetEnterEvery
	// apart. The exact text that landed still there on two reads in a row
	// gets one more Enter, at most fleetEnterRetries times (never for grok).
	fleetEnterPolls   = 10
	fleetEnterEvery   = 150 * time.Millisecond
	fleetEnterRetries = 2
	// fleetLeftoverTTL: how long a text this server typed but could not
	// submit may still be submitted by a later send (fleetRecoverLeftover).
	fleetLeftoverTTL = 30 * time.Minute
	// fleetLeftoverMinRunes: a leftover box shorter than this must be the
	// whole text, so a short word someone else typed is never taken for a
	// piece of ours.
	fleetLeftoverMinRunes = 24
	// fleetLeftoverTries: recovery Enters per leftover, across every send.
	// After that the text is never submitted by the server again; it stays
	// remembered until the box is empty or the TTL passes.
	fleetLeftoverTries = 2
)

type fleetSayResult struct {
	OK     bool   `json:"ok"`
	Queued bool   `json:"queued,omitempty"`
	Error  string `json:"error,omitempty"`
	Code   string `json:"code,omitempty"`
	// Held: a plain page message went to the server outbox
	// (fleet_outbox.go) instead of being typed now.
	Held     bool   `json:"held,omitempty"`
	OutboxID string `json:"outboxId,omitempty"`
}

// fleetLeftover is a text this server typed into a session but could not
// submit (not_submitted). Keyed by tmux session name in fleetDeps.leftover,
// guarded by sayMu.
type fleetLeftover struct {
	text  string
	at    time.Time
	tries int // recovery Enters pressed on it so far (fleetLeftoverTries)
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

// fleetTextLandedFor reports whether the input box holds what was just
// typed: the box text is a non-empty contiguous piece of the sent text,
// anywhere in it, whitespace ignored. Claude or grok may wrap a long line or
// still be drawing it (a prefix), and both scroll a long text so the box
// shows only a later part of it (Claude Code's ❯ line then starts mid-text).
// The box was empty before the text was typed, so whatever it holds now came
// from this send. A long paste shows in Claude Code as a "[Pasted text ...]"
// placeholder instead; grok wraps it inside the box.
func fleetTextLandedFor(row Session, box, sent string) bool {
	if !row.IsGrok() && strings.HasPrefix(box, "[Pasted text") {
		return true
	}
	b := stripSpace(box)
	return b != "" && strings.Contains(stripSpace(sent), b)
}

// fleetLeftoverMatches is the stricter check before Enter is pressed on a
// box this send did not just type into: the box may hold someone else's
// text by now. It must be a contiguous piece of our text, whitespace
// ignored, and either the whole text or at least fleetLeftoverMinRunes long.
// A "[Pasted text" placeholder never matches: anyone's paste looks like that.
func fleetLeftoverMatches(box, sent string) bool {
	b, t := stripSpace(box), stripSpace(sent)
	if b == "" || !strings.Contains(t, b) {
		return false
	}
	return b == t || utf8.RuneCountInString(b) >= fleetLeftoverMinRunes
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
	// refuseBusyGrok keeps an automatic sender from pressing Enter into a
	// busy Grok turn. A page send leaves it false: Enter queues the text.
	refuseBusyGrok bool
}

var fleetSayFLEET = fleetSayWho{subject: "FLEET", pane: "the FLEET pane", paneLog: true}

// fleetPaneSeams are fleetDeps' tmux seams with the real defaults filled in.
type fleetPaneSeams struct {
	capture    func(string) (string, error)
	typeText   func(target, text string) error
	pressEnter func(target string) error
	sleep      func(time.Duration)
	inMode     func(target string) (bool, error)
}

func (s *server) fleetPaneSeams() fleetPaneSeams {
	f := &s.fleet
	sc := fleetPaneSeams{capture: f.capture, typeText: f.typeText, pressEnter: f.pressEnter, sleep: f.sleep, inMode: f.paneInMode}
	if sc.capture == nil {
		sc.capture = capturePane
	}
	if sc.typeText == nil {
		sc.typeText = tmuxTypeLiteral
	}
	if sc.pressEnter == nil {
		sc.pressEnter = tmuxPressEnter
	}
	if sc.sleep == nil {
		sc.sleep = time.Sleep
	}
	if sc.inMode == nil {
		sc.inMode = tmuxPaneInMode
	}
	return sc
}

// fleetTypeInto is the pane half of every page send: refuse a session that
// waits on a dialog, is a grok session this sender must not type into, has
// no pane, shows no empty input box (claude's or grok's, paneInputTextFor),
// or is in copy mode; type the text; press Enter only once the box holds it;
// then check that Enter took (fleetVerifyEnter). A box that still holds a
// text this server typed earlier and could not submit is submitted first
// (fleetRecoverLeftover); when that text is this send's own text, that
// Enter was this send's delivery and nothing is typed. Any other text in the
// box refuses as input_busy.
// row must be a freshly resolved session: its Tmux is the only pane address
// used. Caller holds sayMu. The result is the response envelope; ok false
// with code not_submitted means the text was typed but is still in the box.
func (s *server) fleetTypeInto(row Session, text string, who fleetSayWho) fleetSayResult {
	f := &s.fleet
	refuse := func(code, msg string) fleetSayResult { return fleetSayResult{Code: code, Error: msg} }
	if row.Waiting() {
		return refuse("dialog", who.subject+" shows a dialog. Answer it in the terminal.")
	}
	// Enter queues a message in the Grok composer. A page send may do that
	// while the turn is busy. The box looks the same idle or busy, so an
	// empty status (no readable events.jsonl yet) and waiting still refuse.
	// Idle events set refuseBusyGrok: an automatic Enter stays out of a
	// busy turn.
	if row.IsGrok() && row.Status != "idle" && row.Status != "shell" && (row.Status != "busy" || who.refuseBusyGrok) {
		return refuse("busy", who.subject+" is busy. Wait until its turn ends.")
	}
	if row.Tmux == "" {
		return refuse("no_pane", who.subject+" has no tmux pane")
	}
	pane := tmuxExactTarget(row.Tmux) // "=" so a gone session never prefix-matches another
	key := tmuxSessionPart(row.Tmux)
	sc := s.fleetPaneSeams()
	screen, err := sc.capture(pane)
	if err != nil {
		return refuse("no_pane", "cannot read "+who.pane)
	}
	typed, found := paneInputTextFor(row, screen)
	if found && typed != "" {
		if old, ok := s.fleetRecoverLeftover(row, pane, key, typed, sc); ok {
			// The leftover was this very text (a resend after not_submitted):
			// submitting it was this send's delivery. Typing it again would
			// deliver it twice.
			if old == text {
				return fleetSayResult{OK: true, Queued: row.Status == "busy"}
			}
			// Our old text left the box. Read it again for this send.
			if screen, err = sc.capture(pane); err != nil {
				return refuse("no_pane", "cannot read "+who.pane)
			}
			typed, found = paneInputTextFor(row, screen)
		}
	}
	if !found {
		s.logFleetSendRefusal(who, "dialog", row.Tmux, screen, text)
		return refuse("dialog", who.subject+" input box is not visible")
	} else if typed != "" {
		s.logFleetSendRefusal(who, "input_busy", row.Tmux, screen, text)
		return refuse("input_busy", "input box not empty")
	}
	delete(f.leftover, key) // an empty box holds no leftover
	if mode, err := sc.inMode(pane); err != nil {
		return refuse("no_pane", "cannot read "+who.pane)
	} else if mode {
		return refuse("pane_mode", strings.ToUpper(who.pane[:1])+who.pane[1:]+" is in scroll mode. Press q in the terminal, then try again.")
	}

	if err := sc.typeText(pane, text); err != nil {
		return refuse("send_failed", "send text: "+err.Error())
	}
	notSubmitted := func(screen string) fleetSayResult {
		if f.leftover == nil {
			f.leftover = map[string]fleetLeftover{}
		}
		f.leftover[key] = fleetLeftover{text: text, at: s.fleetNow()}
		s.logFleetSendRefusal(who, "not_submitted", row.Tmux, screen, text)
		return refuse("not_submitted", "Text was typed but not sent. "+who.subject+" changed state. Check the terminal.")
	}
	// The first capture is a snapshot: a dialog may have opened since, and
	// Enter would pick its highlighted option. Press Enter only once the box
	// is still there and holds what was just typed (it was empty before).
	// Other text in the box is not ours: keep polling, then give up.
	landed, landedBox := false, ""
	lastScreen := ""
	for i := 0; i < fleetLandPolls && !landed; i++ {
		if i > 0 {
			sc.sleep(fleetLandEvery)
		}
		screen, err := sc.capture(pane)
		if err != nil {
			continue
		}
		lastScreen = screen
		typed, found := paneInputTextFor(row, screen)
		if !found {
			break
		}
		landed = fleetTextLandedFor(row, typed, text)
		landedBox = typed
	}
	if !landed {
		return notSubmitted(lastScreen)
	}
	if err := sc.pressEnter(pane); err != nil {
		return refuse("send_failed", "send enter: "+err.Error())
	}
	stuck, lastScreen, err := s.fleetVerifyEnter(row, pane, landedBox, true, sc)
	if err != nil {
		return refuse("send_failed", "send enter: "+err.Error())
	}
	if stuck {
		return notSubmitted(lastScreen)
	}
	return fleetSayResult{OK: true, Queued: row.Status == "busy"}
}

// fleetVerifyEnter reads the box after Enter was pressed on landed (the
// box text read right before that Enter). The text left (no box: a turn
// started; an empty box, which includes Claude Code's dim "Press up to edit
// queued messages" placeholder) → delivered. Any box that is not exactly
// landed, whitespace ignored (shorter, longer, foreign: someone typing, or
// an autocomplete) → delivered too: the server presses Enter only on the
// very text it saw before its own Enter, never on anything else. Exactly
// landed on two reads in a row → Enter again, at most fleetEnterRetries
// times, when retry is set and the row is not grok: a first Enter can make
// a grok row busy, and a bare Enter in a busy grok composer can force-send
// a queued message, so grok is only read. stuck is true when landed is
// still in the box at the end; screen is the last screen read.
func (s *server) fleetVerifyEnter(row Session, pane, landed string, retry bool, sc fleetPaneSeams) (stuck bool, screen string, err error) {
	retry = retry && !row.IsGrok()
	want := stripSpace(landed)
	ours, retries, lastOurs := 0, 0, false
	for i := 0; i < fleetEnterPolls; i++ {
		sc.sleep(fleetEnterEvery)
		got, cerr := sc.capture(pane)
		if cerr != nil {
			ours = 0 // an unread poll breaks the run
			continue
		}
		screen = got
		box, found := paneInputTextFor(row, got)
		if !found || want == "" || stripSpace(box) != want {
			return false, screen, nil
		}
		lastOurs = true
		if ours++; ours < 2 || !retry {
			continue
		}
		if retries >= fleetEnterRetries {
			return true, screen, nil
		}
		if err := sc.pressEnter(pane); err != nil {
			return false, screen, err
		}
		retries++
		ours = 0
	}
	return lastOurs, screen, nil
}

// fleetRecoverLeftover submits a text this server typed into the session
// under key earlier and could not submit, when the box (box) still holds it:
// one Enter, then fleetVerifyEnter with no retry. It never presses Enter on
// a box that is in copy mode or holds anything but a piece of that text
// (fleetLeftoverMatches), and never more than fleetLeftoverTries times per
// leftover, across every call. ok means our text left the box (memory
// dropped, one say-log line); text is the leftover's text, so the caller
// can tell a resend of the same text from a new one. Caller holds sayMu.
func (s *server) fleetRecoverLeftover(row Session, pane, key, box string, sc fleetPaneSeams) (text string, ok bool) {
	f := &s.fleet
	lo, ok := f.leftover[key]
	if !ok {
		return "", false
	}
	if s.fleetNow().Sub(lo.at) > fleetLeftoverTTL {
		delete(f.leftover, key)
		return "", false
	}
	if lo.tries >= fleetLeftoverTries || !fleetLeftoverMatches(box, lo.text) {
		return "", false
	}
	if mode, err := sc.inMode(pane); err != nil || mode {
		return "", false
	}
	lo.tries++
	f.leftover[key] = lo
	if err := sc.pressEnter(pane); err != nil {
		return "", false
	}
	if stuck, _, err := s.fleetVerifyEnter(row, pane, box, false, sc); err != nil || stuck {
		return "", false
	}
	delete(f.leftover, key)
	if path := s.fleetSayLogPath(); path != "" {
		appendFleetSayLog(path, fmt.Sprintf("%s recovered target=%s len=%d\n",
			s.fleetNow().UTC().Format(time.RFC3339), row.Tmux, utf8.RuneCountInString(lo.text)))
	}
	return lo.text, true
}

func (s *server) fleetSayLogPath() string {
	if s.fleet.sayLogPath != "" {
		return s.fleet.sayLogPath
	}
	return fleetSayLogDefault()
}

// logFleetSendRefusal logs a pane refusal. FLEET's own entries keep the
// screen lines (logFleetRefusal). Any other session's entry is the header
// line only, with the text length: its pane can hold the typed text, and
// the say log must never hold what was sent to a session. who.logGate, when
// set, applies to both.
func (s *server) logFleetSendRefusal(who fleetSayWho, code, target, screen, text string) {
	if who.logGate != nil && !who.logGate(code) {
		return
	}
	if who.paneLog {
		s.logFleetRefusal(code, target, screen)
		return
	}
	path := s.fleetSayLogPath()
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
	// A plain message (no cardId) that cannot be typed now goes to the
	// outbox (fleet_outbox.go): hold is the code that held it. A card answer
	// is never held: its guards must run at the moment of typing.
	hold := ""
	if res := s.fleetSayGateLocked(text); res != nil {
		if cardID != "" || res.Code != "rate_limited" {
			writeJSON(w, http.StatusOK, *res)
			return
		}
		// The gate reports the 1 s gap before a double tap; a second tap
		// right after the first finished is still a double tap.
		if _, dup := f.saidAt[text]; dup {
			refuse("double_tap", "same text sent less than 10 s ago")
			return
		}
		hold = res.Code
	}
	if cardID == "" && f.outbox.hasQueued(text, now) {
		refuse("double_tap", "same text is already queued")
		return
	}

	fresh, code, msg := s.fleetSendTarget()
	if fresh == nil {
		refuse(code, msg)
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
	if cardID == "" && hold == "" && f.outbox.queuedCount(now) > 0 {
		hold = "queued" // FIFO: wait behind the messages already held
	}
	if hold != "" {
		s.fleetHold(w, text, hold, false, now)
		return
	}
	res := s.fleetTypeInto(*fresh, text, fleetSayFLEET)
	if cardID == "" && !res.OK && fleetOutboxHoldCode(res.Code) {
		s.fleetHold(w, text, res.Code, true, now)
		return
	}
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

// fleetSendTarget resolves FLEET for a send: discovery on the cached list,
// then a fresh resolveLivePID (a recycled pid refuses), then findFleet again
// on that fresh list (a second FLEET or a rename refuses as fleet_changed).
// An empty session id is refused: it would switch off resolveLivePID's
// check. nil with the refusal code and message when FLEET cannot take a
// send; the code is "" when the session list failed.
func (s *server) fleetSendTarget() (*Session, string, string) {
	row, fl := s.discoverFleet()
	if !fl.Alive {
		if fl.Reason == "ambiguous" {
			return nil, "ambiguous", "two sessions are named FLEET"
		}
		return nil, "not_live", "FLEET is not running"
	}
	if row.SessionID == "" {
		return nil, "not_live", "FLEET has no session id"
	}
	fresh, all, refusal := s.resolveLivePID(row.PID, row.SessionID)
	if refusal != nil {
		return nil, refusal.Code, refusal.Error
	}
	if again, reason := findFleet(all); reason != "" || again.PID != row.PID || again.SessionID != row.SessionID {
		return nil, "fleet_changed", "FLEET changed. Try again."
	}
	return fresh, "", ""
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
