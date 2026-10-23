package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakePane is a Claude Code pane whose input box is box: typing appends to
// it, Enter clears it unless stuck (or until enters reaches takeAt, when
// takeAt is set). hideTyped keeps typed text out of the box (it never lands).
type fakePane struct {
	box       string
	stuck     bool
	takeAt    int
	hideTyped bool
	typed     []string
	enters    int
}

func (p *fakePane) install(rig *sayRig) {
	rig.s.fleet.capture = func(string) (string, error) {
		if p.box == "" {
			return emptyBoxCapture, nil
		}
		return boxCapture(p.box), nil
	}
	rig.s.fleet.typeText = func(_ string, text string) error {
		p.typed = append(p.typed, text)
		if !p.hideTyped {
			p.box += text
		}
		return nil
	}
	rig.s.fleet.pressEnter = func(string) error {
		p.enters++
		if !p.stuck && (p.takeAt == 0 || p.enters >= p.takeAt) {
			p.box = ""
		}
		return nil
	}
}

func newPaneRig(t *testing.T, sessions []Session) (*sayRig, *fakePane) {
	rig := newSayRig(t, sessions)
	p := &fakePane{}
	p.install(rig)
	return rig, p
}

const longSay = "Andy decides: merge DR-5658 after the tests pass, then start DR-5659."

// The scrolled box from the stuck 2026-10-23 pane: the ❯ line starts
// mid-text. It lands for the text that was sent, so Enter is pressed.
func TestFleetSayScrolledLongTextLands(t *testing.T) {
	scrolled := readFixture(t, "pane-synth-scrolled-long.ansi")
	rig := newSayRig(t, []Session{fleetRow()})
	rig.captures = []string{emptyBoxCapture, scrolled, emptyBoxCapture}
	body := sayBody(t, map[string]any{"text": scrolledSent})
	if _, res := rig.post(t, body, nil); !res.OK || res.Held || rig.enters != 1 {
		t.Fatalf("%+v enters %d", res, rig.enters)
	}
}

func TestFleetSayEnterNotTakenRetriesThenNotSubmitted(t *testing.T) {
	rig, p := newPaneRig(t, []Session{fleetRow()})
	p.stuck = true
	if _, res := rig.post(t, sayBody(t, map[string]any{"text": longSay}), nil); res.Code != "not_submitted" {
		t.Fatalf("%+v", res)
	}
	if p.enters != 1+fleetEnterRetries {
		t.Fatalf("enters %d, want %d", p.enters, 1+fleetEnterRetries)
	}
	if lo, ok := rig.s.fleet.leftover["fleet"]; !ok || lo.text != longSay {
		t.Fatalf("leftover = %+v", rig.s.fleet.leftover)
	}
	data, _ := os.ReadFile(filepath.Join(rig.s.fleet.dir, "log.jsonl"))
	if !strings.Contains(string(data), "not submitted") {
		t.Fatalf("log %q", data)
	}
	if out := readSayLog(t, rig); !strings.Contains(out, "refused code=not_submitted target=fleet:0.0\n") {
		t.Fatalf("say log %q", out)
	}

	// Enter takes on the second press.
	rig, p = newPaneRig(t, []Session{fleetRow()})
	p.takeAt = 2
	if _, res := rig.post(t, `{"text":"hello"}`, nil); !res.OK || p.enters != 2 {
		t.Fatalf("%+v enters %d", res, p.enters)
	}
	if len(rig.s.fleet.leftover) != 0 {
		t.Fatalf("leftover = %+v", rig.s.fleet.leftover)
	}
}

// After Enter, text that is not ours, or no box at all, counts as delivered.
func TestFleetSayEnterVerifyOtherTextOrNoBox(t *testing.T) {
	for name, after := range map[string]string{
		"other text": boxCapture("zzz"),
		"no box":     dialogCapture,
		// Claude Code queued it: the box shows the dim placeholder.
		"queued placeholder": readFixture(t, "pane-queued-placeholder.ansi"),
	} {
		rig := newSayRig(t, []Session{fleetRow()})
		rig.captures = []string{emptyBoxCapture, boxCapture("hello"), after}
		if _, res := rig.post(t, `{"text":"hello"}`, nil); !res.OK || rig.enters != 1 {
			t.Errorf("%s: %+v enters %d", name, res, rig.enters)
		}
	}
}

// A grok composer never gets a second Enter, whatever its status: the
// first Enter can make the row busy, and a bare Enter in a busy composer can
// force-send a queued message. It is only read.
func TestFleetSayGrokNoExtraEnter(t *testing.T) {
	empty := readFixture(t, "grok-busy-empty.ansi")
	for _, st := range []string{"busy", "idle", "shell"} {
		row := Session{PID: 7, Tool: toolGrok, Name: "FLEET", NameSource: "user", SessionID: "gsid", Status: st, Tmux: "gfleet:0.0"}
		rig := newSayRig(t, []Session{row})
		rig.s.fleet.capture = func(string) (string, error) {
			if len(rig.typed) > 0 {
				return grokBox("│ ❯ " + rig.typed[0] + " │"), nil // Enter never takes
			}
			return empty, nil
		}
		if _, res := rig.post(t, `{"text":"hi"}`, nil); res.Code != "not_submitted" || rig.enters != 1 {
			t.Fatalf("%s: %+v enters %d", st, res, rig.enters)
		}
	}
}

// Enter takes, then the box shows a short piece of the text (someone
// typing, or an autocomplete): not the exact landed text, so no second
// Enter.
func TestFleetSayEnterRetryNeedsExactLandedText(t *testing.T) {
	rig := newSayRig(t, []Session{fleetRow()})
	rig.captures = []string{emptyBoxCapture, boxCapture("ok then"), boxCapture("o"), boxCapture("ok")}
	if _, res := rig.post(t, `{"text":"ok then"}`, nil); !res.OK || rig.enters != 1 {
		t.Fatalf("%+v enters %d", res, rig.enters)
	}
	// A longer box is not ours either.
	rig = newSayRig(t, []Session{fleetRow()})
	rig.captures = []string{emptyBoxCapture, boxCapture("ok"), boxCapture("ok then"), boxCapture("ok then")}
	if _, res := rig.post(t, `{"text":"ok then"}`, nil); !res.OK || rig.enters != 1 {
		t.Fatalf("longer: %+v enters %d", res, rig.enters)
	}
}

// A resend of a text that was typed but not submitted: the recovery Enter
// is its delivery, and it is not typed a second time.
func TestFleetSayResendOfLeftoverIsDelivery(t *testing.T) {
	rig, p := newPaneRig(t, []Session{fleetRow()})
	p.stuck = true
	body := sayBody(t, map[string]any{"text": longSay})
	if _, res := rig.post(t, body, nil); res.Code != "not_submitted" {
		t.Fatalf("%+v", res)
	}
	p.stuck, p.enters = false, 0
	rig.now = rig.now.Add(time.Minute)
	if _, res := rig.post(t, body, nil); !res.OK || res.Held || res.Queued {
		t.Fatalf("%+v", res)
	}
	if len(p.typed) != 1 || p.enters != 1 || p.box != "" {
		t.Fatalf("typed %q enters %d box %q", p.typed, p.enters, p.box)
	}
	if len(rig.s.fleet.leftover) != 0 {
		t.Fatalf("leftover = %+v", rig.s.fleet.leftover)
	}
	data, _ := os.ReadFile(filepath.Join(rig.s.fleet.dir, "log.jsonl"))
	if !strings.Contains(string(data), `"type":"web-say"`) {
		t.Fatalf("log %q", data)
	}
}

// At most fleetLeftoverTries recovery Enters per leftover, across sends;
// the memory stays until the box is empty.
func TestFleetSayLeftoverRecoveryIsCapped(t *testing.T) {
	rig, p := newPaneRig(t, []Session{fleetRow()})
	sayCards(t, rig, dr1Card)
	p.stuck = true
	rig.post(t, sayBody(t, map[string]any{"text": longSay}), nil)
	p.enters = 0
	for i := 0; i < 4; i++ {
		rig.now = rig.now.Add(time.Minute)
		if _, res := rig.post(t, cardHi, nil); res.Code != "input_busy" {
			t.Fatalf("%d: %+v", i, res)
		}
	}
	if p.enters != fleetLeftoverTries {
		t.Fatalf("enters %d, want %d", p.enters, fleetLeftoverTries)
	}
	if lo, ok := rig.s.fleet.leftover["fleet"]; !ok || lo.tries != fleetLeftoverTries {
		t.Fatalf("leftover = %+v", rig.s.fleet.leftover)
	}
	p.stuck, p.box = false, ""
	rig.now = rig.now.Add(time.Minute)
	if _, res := rig.post(t, cardHi, nil); !res.OK || p.enters != fleetLeftoverTries+1 || len(rig.s.fleet.leftover) != 0 {
		t.Fatalf("%+v enters %d leftover %+v", res, p.enters, rig.s.fleet.leftover)
	}
}

// A text this server typed and could not submit is submitted by the next
// send, which then types its own text. Any other text in the box refuses.
func TestFleetSayRecoversOwnLeftover(t *testing.T) {
	rig, p := newPaneRig(t, []Session{fleetRow()})
	p.stuck = true
	if _, res := rig.post(t, sayBody(t, map[string]any{"text": longSay}), nil); res.Code != "not_submitted" {
		t.Fatalf("%+v", res)
	}
	p.stuck = false
	p.enters = 0
	rig.now = rig.now.Add(time.Minute)
	if _, res := rig.post(t, `{"text":"next one"}`, nil); !res.OK || res.Held {
		t.Fatalf("%+v", res)
	}
	if p.enters != 2 || len(p.typed) != 2 || p.typed[1] != "next one" {
		t.Fatalf("enters %d typed %q", p.enters, p.typed)
	}
	if len(rig.s.fleet.leftover) != 0 {
		t.Fatalf("leftover = %+v", rig.s.fleet.leftover)
	}
	out := readSayLog(t, rig)
	// One header line, never the text (FLEET's own refusal entries above
	// keep the pane lines, as before).
	if !strings.HasSuffix(out, "\n\n2026-10-13T10:01:00Z recovered target=fleet:0.0 len=69\n") {
		t.Fatalf("say log %q", out)
	}
}

func TestFleetSayLeftoverNeverEntersOtherText(t *testing.T) {
	for name, tc := range map[string]struct {
		box   string
		after time.Duration
	}{
		"foreign text":  {"half typed by Andy", time.Minute},
		"short piece":   {"DR-5659.", time.Minute}, // ours, but too short to tell
		"pasted":        {"[Pasted text #1 +2 lines]", time.Minute},
		"after the TTL": {longSay, 31 * time.Minute},
	} {
		rig, p := newPaneRig(t, []Session{fleetRow()})
		sayCards(t, rig, dr1Card)
		p.stuck = true
		if _, res := rig.post(t, sayBody(t, map[string]any{"text": longSay}), nil); res.Code != "not_submitted" {
			t.Fatalf("%s: %+v", name, res)
		}
		p.stuck, p.enters, p.box = false, 0, tc.box
		rig.now = rig.now.Add(tc.after)
		if _, res := rig.post(t, cardHi, nil); res.Code != "input_busy" || p.enters != 0 || len(p.typed) != 1 {
			t.Fatalf("%s: %+v enters %d typed %q", name, res, p.enters, p.typed)
		}
	}
}

// A leftover Enter that does not take refuses as input_busy and keeps the
// memory; copy mode never gets the Enter.
func TestFleetSayLeftoverStuckOrCopyMode(t *testing.T) {
	rig, p := newPaneRig(t, []Session{fleetRow()})
	sayCards(t, rig, dr1Card)
	p.stuck = true
	rig.post(t, sayBody(t, map[string]any{"text": longSay}), nil)
	p.enters = 0
	rig.now = rig.now.Add(time.Minute)
	// One Enter: a recovery never retries (fleetLeftoverTries caps it).
	if _, res := rig.post(t, cardHi, nil); res.Code != "input_busy" || p.enters != 1 {
		t.Fatalf("%+v enters %d", res, p.enters)
	}
	if _, ok := rig.s.fleet.leftover["fleet"]; !ok {
		t.Fatal("a leftover that did not leave must be kept")
	}
	p.stuck, p.enters = false, 0
	rig.s.fleet.paneInMode = func(string) (bool, error) { return true, nil }
	rig.now = rig.now.Add(time.Minute)
	if _, res := rig.post(t, cardHi, nil); res.Code != "input_busy" || p.enters != 0 {
		t.Fatalf("copy mode: %+v enters %d", res, p.enters)
	}
}

// An empty box drops the memory: a later identical text is not ours to
// submit any more.
func TestFleetSayEmptyBoxDropsLeftover(t *testing.T) {
	rig, p := newPaneRig(t, []Session{fleetRow()})
	p.stuck = true
	rig.post(t, sayBody(t, map[string]any{"text": longSay}), nil)
	p.stuck, p.box = false, ""
	rig.now = rig.now.Add(time.Minute)
	if _, res := rig.post(t, `{"text":"next"}`, nil); !res.OK {
		t.Fatalf("%+v", res)
	}
	if len(rig.s.fleet.leftover) != 0 {
		t.Fatalf("leftover = %+v", rig.s.fleet.leftover)
	}
}

type outboxState struct {
	Outbox []map[string]any `json:"outbox"`
}

func rigOutbox(t *testing.T, rig *sayRig) ([]map[string]any, string) {
	t.Helper()
	r := httptest.NewRequest("GET", "/fleet/state", nil)
	r.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	fleetMux(rig.s).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("state: %d", w.Code)
	}
	var out outboxState
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Outbox == nil {
		t.Fatalf("outbox must be [], not null: %s", w.Body.String())
	}
	return out.Outbox, w.Body.String()
}

func TestFleetOutboxHoldsAndDeliversWhenBoxClears(t *testing.T) {
	rig, p := newPaneRig(t, []Session{fleetRow()})
	if ob, _ := rigOutbox(t, rig); len(ob) != 0 {
		t.Fatalf("outbox %v", ob)
	}
	p.box = "Andy is typing"
	_, res := rig.post(t, `{"text":"secret hello"}`, nil)
	if !res.OK || !res.Held || len(res.OutboxID) != 16 || res.Queued {
		t.Fatalf("%+v", res)
	}
	ob, raw := rigOutbox(t, rig)
	if len(ob) != 1 || ob[0]["id"] != res.OutboxID || ob[0]["status"] != "queued" || ob[0]["code"] != "input_busy" ||
		ob[0]["attempts"] != 1.0 || ob[0]["queuedAt"] != "2026-10-13T10:00:00Z" {
		t.Fatalf("outbox %v", ob)
	}
	if _, has := ob[0]["text"]; has || strings.Contains(raw, "secret hello") {
		t.Fatalf("the outbox must not carry the text: %s", raw)
	}

	rig.now = rig.now.Add(2 * time.Second)
	rig.s.fleetOutboxTick()
	if len(p.typed) != 0 {
		t.Fatalf("typed %q into a busy box", p.typed)
	}
	if ob, _ := rigOutbox(t, rig); ob[0]["attempts"] != 2.0 || ob[0]["status"] != "queued" {
		t.Fatalf("outbox %v", ob)
	}

	p.box = ""
	rig.now = rig.now.Add(2 * time.Second)
	rig.s.fleetOutboxTick()
	if len(p.typed) != 1 || p.typed[0] != "secret hello" || p.enters != 1 {
		t.Fatalf("typed %q enters %d", p.typed, p.enters)
	}
	ob, raw = rigOutbox(t, rig)
	if ob[0]["status"] != "sent" || ob[0]["doneAt"] != "2026-10-13T10:00:04Z" || ob[0]["attempts"] != 3.0 {
		t.Fatalf("outbox %v", ob)
	}
	if strings.Contains(raw, "secret hello") {
		t.Fatalf("state leaks the text: %s", raw)
	}
	data, _ := os.ReadFile(filepath.Join(rig.s.fleet.dir, "log.jsonl"))
	if !strings.Contains(string(data), `"type":"web-say","text":"secret hello"`) {
		t.Fatalf("log %q", data)
	}
	// The delivered text counts for the double tap.
	rig.now = rig.now.Add(2 * time.Second)
	if _, res := rig.post(t, `{"text":"secret hello"}`, nil); res.Code != "double_tap" {
		t.Fatalf("%+v", res)
	}
	// A sent item is forgotten 10 minutes after it finished.
	rig.now = rig.now.Add(11 * time.Minute)
	if ob, _ := rigOutbox(t, rig); len(ob) != 0 {
		t.Fatalf("outbox %v", ob)
	}
}

// A held message keeps its place: a later one waits behind it even when
// the box is empty.
func TestFleetOutboxFIFO(t *testing.T) {
	rig, p := newPaneRig(t, []Session{fleetRow()})
	p.box = "busy"
	if _, res := rig.post(t, `{"text":"one"}`, nil); !res.Held {
		t.Fatalf("%+v", res)
	}
	p.box = ""
	rig.now = rig.now.Add(2 * time.Second)
	_, res := rig.post(t, `{"text":"two"}`, nil)
	if !res.OK || !res.Held || len(p.typed) != 0 {
		t.Fatalf("%+v typed %q", res, p.typed)
	}
	if ob, _ := rigOutbox(t, rig); len(ob) != 2 || ob[1]["code"] != nil || ob[1]["attempts"] != 0.0 {
		t.Fatalf("outbox %v", ob)
	}
	rig.s.fleetOutboxTick()
	rig.now = rig.now.Add(500 * time.Millisecond)
	rig.s.fleetOutboxTick() // inside the 1 s gap: waits
	if len(p.typed) != 1 {
		t.Fatalf("typed %q", p.typed)
	}
	if ob, _ := rigOutbox(t, rig); ob[1]["code"] != "rate_limited" || ob[1]["status"] != "queued" {
		t.Fatalf("outbox %v", ob)
	}
	rig.now = rig.now.Add(2 * time.Second)
	rig.s.fleetOutboxTick()
	if len(p.typed) != 2 || p.typed[0] != "one" || p.typed[1] != "two" {
		t.Fatalf("typed %q", p.typed)
	}
}

func TestFleetOutboxRateLimitedIsHeld(t *testing.T) {
	rig, p := newPaneRig(t, []Session{fleetRow()})
	rig.post(t, `{"text":"A"}`, nil)
	rig.now = rig.now.Add(500 * time.Millisecond)
	_, res := rig.post(t, `{"text":"B"}`, nil)
	if !res.OK || !res.Held || len(p.typed) != 1 {
		t.Fatalf("%+v typed %q", res, p.typed)
	}
	if ob, _ := rigOutbox(t, rig); ob[0]["code"] != "rate_limited" || ob[0]["attempts"] != 0.0 {
		t.Fatalf("outbox %v", ob)
	}
	// The same text again while it is queued is a double tap.
	if _, res := rig.post(t, `{"text":"B"}`, nil); res.Code != "double_tap" || res.Held {
		t.Fatalf("%+v", res)
	}
	rig.now = rig.now.Add(time.Second)
	rig.s.fleetOutboxTick()
	if len(p.typed) != 2 || p.typed[1] != "B" {
		t.Fatalf("typed %q", p.typed)
	}
}

func TestFleetOutboxFull(t *testing.T) {
	rig, p := newPaneRig(t, []Session{fleetRow()})
	p.box = "busy"
	for i := 0; i < fleetOutboxMax; i++ {
		rig.now = rig.now.Add(2 * time.Second)
		if _, res := rig.post(t, sayBody(t, map[string]any{"text": "message " + string(rune('a'+i))}), nil); !res.Held {
			t.Fatalf("%d: %+v", i, res)
		}
	}
	rig.now = rig.now.Add(2 * time.Second)
	_, res := rig.post(t, `{"text":"one more"}`, nil)
	if res.OK || res.Code != "outbox_full" || res.Error != "Too many queued messages. Wait until FLEET reads them." {
		t.Fatalf("%+v", res)
	}
	if len(p.typed) != 0 {
		t.Fatalf("typed %q", p.typed)
	}
}

func TestFleetOutboxExpires(t *testing.T) {
	rig, p := newPaneRig(t, []Session{fleetRow()})
	p.box = "busy"
	rig.post(t, `{"text":"old"}`, nil)
	rig.now = rig.now.Add(61 * time.Minute)
	p.box = ""
	rig.s.fleetOutboxTick()
	if len(p.typed) != 0 {
		t.Fatalf("an expired item must not be typed: %q", p.typed)
	}
	ob, _ := rigOutbox(t, rig)
	if len(ob) != 1 || ob[0]["status"] != "dropped" || ob[0]["code"] != "expired" || ob[0]["doneAt"] != "2026-10-13T11:01:00Z" {
		t.Fatalf("outbox %v", ob)
	}
}

// An item the worker is typing is never expired under it: a view taken
// mid-send, past the TTL, leaves it in flight, and it ends sent.
func TestFleetOutboxInFlightNeverExpires(t *testing.T) {
	rig, p := newPaneRig(t, []Session{fleetRow()})
	p.box = "busy"
	rig.post(t, `{"text":"just in time"}`, nil)
	rig.now = rig.now.Add(59*time.Minute + 59*time.Second)
	p.box = ""
	typeText := rig.s.fleet.typeText
	var mid []fleetOutboxView
	rig.s.fleet.typeText = func(target, text string) error {
		rig.now = rig.now.Add(2 * time.Second)
		mid = rig.s.fleet.outbox.view(rig.now)
		return typeText(target, text)
	}
	rig.s.fleetOutboxTick()
	if len(mid) != 1 || mid[0].Status != "queued" {
		t.Fatalf("mid-send view %+v", mid)
	}
	ob, _ := rigOutbox(t, rig)
	if len(ob) != 1 || ob[0]["status"] != "sent" || len(p.typed) != 1 {
		t.Fatalf("outbox %v typed %q", ob, p.typed)
	}
}

func TestFleetOutboxNotSubmittedIsUnconfirmed(t *testing.T) {
	rig, p := newPaneRig(t, []Session{fleetRow()})
	p.box = "busy"
	rig.post(t, `{"text":"lost words"}`, nil)
	p.box, p.hideTyped = "", true
	rig.now = rig.now.Add(2 * time.Second)
	rig.s.fleetOutboxTick()
	if p.enters != 0 || len(p.typed) != 1 {
		t.Fatalf("typed %q enters %d", p.typed, p.enters)
	}
	ob, _ := rigOutbox(t, rig)
	if ob[0]["status"] != "unconfirmed" || ob[0]["code"] != "not_submitted" {
		t.Fatalf("outbox %v", ob)
	}
	data, _ := os.ReadFile(filepath.Join(rig.s.fleet.dir, "log.jsonl"))
	if !strings.Contains(string(data), `"type":"note","text":"web-say typed but not submitted"`) || strings.Contains(string(data), "lost words") {
		t.Fatalf("log %q", data)
	}
	// Never typed again.
	rig.now = rig.now.Add(2 * time.Second)
	rig.s.fleetOutboxTick()
	if len(p.typed) != 1 {
		t.Fatalf("typed %q", p.typed)
	}
}

// Not running, ambiguous or changed at enqueue time stays a refusal.
func TestFleetOutboxNotLiveIsRefused(t *testing.T) {
	rig, _ := newPaneRig(t, nil)
	if _, res := rig.post(t, `{"text":"hi"}`, nil); res.OK || res.Held || res.Code != "not_live" {
		t.Fatalf("%+v", res)
	}
}

// The worker keeps an item queued while FLEET is gone, and types it once
// FLEET is back.
func TestFleetOutboxWorkerWaitsForFleet(t *testing.T) {
	rows := []Session{fleetRow()}
	rig, p := newPaneRig(t, nil)
	rig.s.collect = func() ([]Session, error) { return rows, nil }
	p.box = "busy"
	rig.post(t, `{"text":"later"}`, nil)
	rows = nil
	p.box = ""
	rig.now = rig.now.Add(2 * time.Second)
	rig.s.fleetOutboxTick()
	if ob, _ := rigOutbox(t, rig); ob[0]["code"] != "not_live" || ob[0]["status"] != "queued" || len(p.typed) != 0 {
		t.Fatalf("outbox %v typed %q", ob, p.typed)
	}
	rows = []Session{fleetRow()}
	rig.now = rig.now.Add(2 * time.Second)
	rig.s.fleetOutboxTick()
	if len(p.typed) != 1 {
		t.Fatalf("typed %q", p.typed)
	}
}

// Card answers are never held.
func TestFleetOutboxNeverHoldsCardAnswers(t *testing.T) {
	rig, p := newPaneRig(t, []Session{fleetRow()})
	sayCards(t, rig, dr1Card)
	p.box = "busy"
	if _, res := rig.post(t, cardHi, nil); res.OK || res.Held || res.Code != "input_busy" {
		t.Fatalf("%+v", res)
	}
	rig.now = rig.now.Add(500 * time.Millisecond)
	p.box = ""
	rig.post(t, `{"text":"plain"}`, nil) // sent; the gap now runs
	rig.now = rig.now.Add(200 * time.Millisecond)
	if _, res := rig.post(t, `{"text":"hi","cardId":"dr-1"}`, nil); res.Held || res.Code != "rate_limited" {
		t.Fatalf("%+v", res)
	}
	if ob, _ := rigOutbox(t, rig); len(ob) != 0 {
		t.Fatalf("outbox %v", ob)
	}
}

// The worker logs a repeated refusal once per code per 10 minutes; the page
// path logs every one.
func TestFleetOutboxWorkerRefusalLogIsGated(t *testing.T) {
	rig, p := newPaneRig(t, []Session{fleetRow()})
	p.box = "busy"
	rig.post(t, `{"text":"hi"}`, nil)
	for i := 0; i < 3; i++ {
		rig.now = rig.now.Add(2 * time.Second)
		rig.s.fleetOutboxTick()
	}
	if n := strings.Count(readSayLog(t, rig), "refused code=input_busy"); n != 2 {
		t.Fatalf("%d input_busy entries, want 2 (page + one worker)", n)
	}
	rig.now = rig.now.Add(11 * time.Minute)
	rig.s.fleetOutboxTick()
	if n := strings.Count(readSayLog(t, rig), "refused code=input_busy"); n != 3 {
		t.Fatalf("%d input_busy entries, want 3", n)
	}
}

// The wake channel is signalled on enqueue, so the worker does not wait for
// its 2 s tick.
func TestFleetOutboxEnqueueWakes(t *testing.T) {
	rig, p := newPaneRig(t, []Session{fleetRow()})
	wake := rig.s.fleet.outbox.wakeChan()
	p.box = "busy"
	rig.post(t, `{"text":"hi"}`, nil)
	select {
	case <-wake:
	default:
		t.Fatal("enqueue must signal the worker")
	}
}

func TestFleetOutboxRevokeOutlastsInFlightAttempt(t *testing.T) {
	var o fleetOutbox
	now := time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)
	a := o.enqueue("first", "input_busy", true, now)
	b := o.enqueue("second", "input_busy", true, now)
	if id, _, ok := o.head(now); !ok || id != a {
		t.Fatalf("head = %q %v, want %q", id, ok, a)
	}
	o.dropQueued("revoked", now) // a is in flight, b is not
	o.retry(a, "input_busy")     // the attempt in flight did not deliver
	if _, _, ok := o.head(now); ok {
		t.Fatal("an item survived the revoke")
	}
	for _, v := range o.view(now) {
		if (v.ID == a || v.ID == b) && (v.Status != fleetOutboxDropped || v.Code != "revoked") {
			t.Fatalf("%s = %+v, want dropped/revoked", v.ID, v)
		}
	}

	// An in-flight item that the attempt delivers stays sent.
	c := o.enqueue("third", "input_busy", true, now)
	o.head(now)
	o.dropQueued("revoked", now)
	o.finish(c, fleetOutboxSent, "", now)
	for _, v := range o.view(now) {
		if v.ID == c && v.Status != fleetOutboxSent {
			t.Fatalf("delivered in-flight item = %+v, want sent", v)
		}
	}
}
