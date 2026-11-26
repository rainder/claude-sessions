package main

import (
	"strings"
	"testing"
	"time"
)

// The key help offers c and x only when they would act on the selected card,
// through the predicates the keys use.
func TestFleetHelpRowCorrectDismiss(t *testing.T) {
	now := time.Unix(1000, 0)
	row := func(st *fleetViewState) string { return stripANSI(fleetBottomRow(st, now)) }

	open := fleetTestState(fleetTestView(fleetTestCard("a", "needs-you")), now)
	if r := row(open); strings.Contains(r, fleetHelpCorrectHint) || strings.Contains(r, fleetHelpDismissHint) {
		t.Fatalf("unanswered card offers c or x: %q", r)
	}
	if !strings.HasSuffix(row(open), "Esc back · ? keys") {
		t.Fatalf("row lacks ? keys: %q", row(open))
	}

	v := fleetTestView(fleetTestCard("a", "needs-you"))
	v.Answered["a"] = fleetAnswerWith(fleetAttemptSent)
	sent := fleetTestState(v, now)
	sent.listMode = true // the SENT list, opened from the ship screen
	if r := row(sent); !strings.Contains(r, "t raw text · c correct · PgUp") || strings.Contains(r, fleetHelpDismissHint) {
		t.Fatalf("confirmed answer: %q", r)
	}
	if !sent.canCorrect() || sent.canDismiss() {
		t.Fatal("predicates disagree with the row")
	}

	v = fleetTestView(fleetTestCard("a", "needs-you"))
	v.Answered["a"] = fleetAnswerWith(fleetAttemptUnconfirmed)
	unconf := fleetTestState(v, now)
	unconf.listMode = true
	if r := row(unconf); !strings.Contains(r, "c correct · x dismiss") {
		t.Fatalf("unconfirmed answer: %q", r)
	}
	// While a dismiss runs, x would do nothing, and the row says so.
	unconf.dismissing = true
	if r := row(unconf); strings.Contains(r, fleetHelpDismissHint) || !strings.Contains(r, fleetHelpCorrectHint) {
		t.Fatalf("dismissing: %q", r)
	}
	if cmd := unconf.handleKey("x", false, now); cmd != fleetCmdNone {
		t.Fatalf("x while dismissing = %v", cmd)
	}
}

// A narrow screen drops the generic parts of the key help first and keeps
// the card keys and "? keys"; the frame never grows past the width.
func TestFleetHelpRowFits(t *testing.T) {
	now := time.Unix(1000, 0)
	v := sessChatView()
	v.Source = "http://src"
	st := fleetTestState(v, now)
	full := stripANSI(fleetBottomRow(st, now))
	for _, want := range []string{"a attach", "i/→ chat", "f FLEET chat", "k clarify", "? keys"} {
		if !strings.Contains(full, want) {
			t.Fatalf("full row lacks %q: %q", want, full)
		}
	}
	cols := cellWidth(full) - 20
	fit := stripANSI(fleetBottomRowFit(st, now, cols))
	if cellWidth(fit) > cols || strings.Contains(fit, "PgUp/PgDn scroll") {
		t.Fatalf("fit at %d = %q", cols, fit)
	}
	for _, want := range []string{"a attach", "i/→ chat", "f FLEET chat", "? keys"} {
		if !strings.Contains(fit, want) {
			t.Fatalf("fit lost %q: %q", want, fit)
		}
	}
	if got := fleetFitHelp("a · b", 3); got != "a · b" {
		t.Fatalf("no droppable part: %q", got)
	}
	for _, l := range strings.Split(fleetRendered(st, 60, 20, now), "\n") {
		if cellWidth(l) > 60 {
			t.Fatalf("line wider than 60: %q", l)
		}
	}
}

// f opens the chat with FLEET; i no longer does.
func TestFleetFOpensFleetChat(t *testing.T) {
	now := time.Unix(1000, 0)
	v := fleetTestView(fleetTestCard("a", "needs-you"))
	v.Source = "http://src"
	st := fleetTestState(v, now)
	if cmd := st.handleKey("f", false, now); cmd != fleetCmdChatGet || st.chat == nil || st.chat.target != nil {
		t.Fatalf("f = %v chat %+v", cmd, st.chat)
	}
	// i on a card with no session is the session chat's refusal, never FLEET's chat.
	st = fleetTestState(v, now)
	if st.handleKey("i", false, now); st.chat != nil {
		t.Fatalf("i opened %+v", st.chat)
	}
}

func TestFleetHelpPanel(t *testing.T) {
	now := time.Unix(1000, 0)
	v := fleetTestView(fleetTestCard("a", "needs-you"))
	v.Source = "http://src"
	st := fleetTestState(v, now)

	if cmd := st.handleKey("?", true, now); cmd != fleetCmdNone || st.help != nil {
		t.Fatal("a pasted ? opened the help")
	}
	if cmd := st.handleKey("?", false, now); cmd != fleetCmdRender || st.help == nil {
		t.Fatalf("? = %v", cmd)
	}
	// Every other key is swallowed: no panel, no send, no chat.
	for _, k := range []string{"1", "r", "R", "a", "f", "i", KeyRight, "x", "c", "k", "o", "t", KeyEnter} {
		if cmd := st.handleKey(k, false, now.Add(time.Hour)); cmd != fleetCmdNone {
			t.Fatalf("%q in the help = %v", k, cmd)
		}
	}
	if st.panel != nil || st.chat != nil || st.clarify != nil || st.batch != nil || st.send != nil || st.unlocked != "" {
		t.Fatalf("a key acted under the help: %+v", st)
	}

	// The frame is the help, exactly rows lines, and shows no card to r.
	st.drawn = fleetDrawnCard{}
	out := fleetRendered(st, 80, 12, now)
	lines := strings.Split(out, "\n")
	if len(lines) != 12 || !strings.HasPrefix(lines[0], "FLEET VIEW · KEYS") || !strings.Contains(lines[11], "Esc / q / ? close") {
		t.Fatalf("help frame:\n%s", out)
	}
	if !strings.Contains(out, "CARD LIST") || !strings.Contains(lines[11], "more") {
		t.Fatalf("help body:\n%s", out)
	}
	if !st.drawn.at.IsZero() {
		t.Fatal("the help counted as drawing a card for r")
	}

	// Scrolling is clamped at both ends.
	st.handleKey(KeyEnd, false, now)
	out = fleetRendered(st, 80, 12, now)
	if !strings.Contains(out, "any other key  back") {
		t.Fatalf("End did not reach the last line:\n%s", out)
	}
	st.handleKey(KeyUp, false, now)
	if st.help.scroll != len(fleetHelpText)-8-1 {
		t.Fatalf("Up after End: scroll %d", st.help.scroll)
	}
	st.handleKey(KeyHome, false, now)
	st.handleKey(KeyUp, false, now)
	if st.help.scroll != 0 {
		t.Fatalf("Up at the top: scroll %d", st.help.scroll)
	}
	st.handleKey(KeyPageDown, false, now)
	if st.help.scroll == 0 {
		t.Fatal("PgDn did not scroll")
	}
	if cmd := st.handleMouse(mouseEvent{button: mouseWheelUp}); cmd != fleetCmdRender {
		t.Fatalf("wheel = %v", cmd)
	}

	for _, k := range []string{KeyEsc, "q", "?", "\x03"} {
		st.help = &fleetHelpMode{}
		if cmd := st.handleKey(k, false, now); cmd != fleetCmdRender || st.help != nil {
			t.Fatalf("%q did not close the help", k)
		}
	}
	// Closing the help never closes the view.
	if cmd := st.handleKey(KeyEsc, false, now); cmd != fleetCmdClose {
		t.Fatalf("Esc after the help = %v", cmd)
	}

	// The full frame at a size the help fits in whole: no "more".
	st.help = &fleetHelpMode{}
	if out := fleetRendered(st, 80, len(fleetHelpText)+4, now); strings.Contains(out, "more ·") {
		t.Fatalf("whole help says more:\n%s", out)
	}
}

// ? opens the help from the ship screen too; in a chat or Clarify it is text.
func TestFleetHelpKeyOwners(t *testing.T) {
	now := time.Unix(1000, 0)
	ship := fleetTestView()
	ship.Source = "http://src"
	st := fleetTestState(ship, now)
	if !st.shipOn() {
		t.Fatal("test setup: no ship screen")
	}
	if st.handleKey("?", false, now); st.help == nil {
		t.Fatal("? on the ship screen did not open the help")
	}

	v := fleetTestView(fleetTestCard("a", "needs-you"))
	v.Source = "http://src"
	st = fleetTestState(v, now)
	st.handleKey("f", false, now)
	if st.handleKey("?", false, now); st.help != nil || st.chatDraft.Text() != "?" {
		t.Fatalf("? in the chat: help %v draft %q", st.help, st.chatDraft.Text())
	}
	st = fleetTestState(v, now)
	st.handleKey("k", false, now)
	if st.handleKey("?", false, now); st.help != nil || st.clarify.input != "?" {
		t.Fatalf("? in Clarify: help %v", st.help)
	}
	st = fleetTestState(v, now)
	st.handleKey("o", false, now)
	if st.handleKey("?", false, now); st.help != nil {
		t.Fatal("? on the Other panel opened the help")
	}
}

// Esc closes a chat and keeps its draft; Ctrl-C closes it and clears the
// draft, FLEET's and a session's alike, and never closes the view.
func TestFleetChatCtrlCClearsDraft(t *testing.T) {
	now := time.Unix(1000, 0)
	late := now.Add(time.Second)
	v := sessChatView()
	st := fleetTestState(v, now)

	st.handleKey("f", false, now)
	st.handleKey("a", false, late)
	st.handleKey(KeyEsc, false, late)
	if st.chat != nil || st.chatDraft.Text() != "a" {
		t.Fatalf("Esc: chat %+v draft %q", st.chat, st.chatDraft.Text())
	}
	st.handleKey("f", false, now)
	if cmd := st.handleKey("\x03", false, late); cmd != fleetCmdRender || st.chat != nil || st.chatDraft.Text() != "" {
		t.Fatalf("Ctrl-C: %v chat %+v draft %q", cmd, st.chat, st.chatDraft.Text())
	}

	st.handleKey("i", false, now)
	key := st.chat.target.key()
	st.handleKey("b", false, late)
	st.handleKey(KeyEsc, false, late)
	if st.sessionDrafts[key].Text() != "b" {
		t.Fatalf("session Esc draft %q", st.sessionDrafts[key].Text())
	}
	st.handleKey("i", false, now)
	if st.chatEditor().Text() != "b" {
		t.Fatalf("session draft not restored: %q", st.chatEditor().Text())
	}
	if cmd := st.handleKey("\x03", false, late); cmd != fleetCmdRender || st.chat != nil || st.sessionDrafts[key].Text() != "" {
		t.Fatalf("session Ctrl-C: %v draft %q", cmd, st.sessionDrafts[key].Text())
	}
}

// Clarify keeps the typed question per card on Esc and restores it on that
// card's next open; Ctrl-C and an asked question drop it.
func TestFleetClarifyDraftPerCard(t *testing.T) {
	now := time.Unix(1000, 0)
	late := now.Add(time.Second)
	st := fleetTestState(fleetTestView(fleetTestCard("a", "needs-you"), fleetTestCard("b", "needs-you")), now)

	st.handleKey("k", false, now)
	for _, k := range []string{"w", "h", "y"} {
		st.handleKey(k, false, late)
	}
	st.handleKey(KeyEsc, false, late)
	if st.clarify != nil || st.clarifyDrafts["a"] != "why" {
		t.Fatalf("Esc: drafts %v", st.clarifyDrafts)
	}
	// Another card's Clarify starts empty.
	st.handleKey(KeyDown, false, now)
	st.handleKey("k", false, now)
	if st.clarify.input != "" {
		t.Fatalf("b starts with %q", st.clarify.input)
	}
	st.handleKey(KeyEsc, false, late)
	if _, ok := st.clarifyDrafts["b"]; ok {
		t.Fatal("an empty input was kept")
	}
	st.handleKey(KeyUp, false, now)
	st.handleKey("k", false, now)
	if st.clarify.input != "why" {
		t.Fatalf("a's input not restored: %q", st.clarify.input)
	}
	st.handleKey("\x03", false, late)
	st.handleKey("k", false, now)
	if st.clarify.input != "" || len(st.clarifyDrafts) != 0 {
		t.Fatalf("Ctrl-C kept %q / %v", st.clarify.input, st.clarifyDrafts)
	}
	// An asked question is gone from the input and from the drafts.
	st.handleKey("q", false, late)
	if cmd := st.handleKey(KeyEnter, false, late); cmd != fleetCmdClarifyAsk {
		t.Fatalf("Enter = %v", cmd)
	}
	st.handleKey(KeyEsc, false, late)
	if len(st.clarifyDrafts) != 0 {
		t.Fatalf("asked question kept: %v", st.clarifyDrafts)
	}
}
