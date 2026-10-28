package main

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func waitingFromJSON(t *testing.T, w string) *fleetWaiting {
	t.Helper()
	return cardFromJSON(t, `{"id":"a","kind":"needs-you","waiting":`+w+`}`).Waiting
}

func TestFleetDecodeWaiting(t *testing.T) {
	since := time.Date(2026, 10, 28, 8, 33, 0, 0, time.UTC)
	answered := time.Date(2026, 10, 28, 10, 2, 0, 0, time.UTC)
	long := strings.Repeat("é", 50)
	for _, c := range []struct {
		name, json string
		want       *fleetWaiting
	}{
		{"valid", `{"who":"Rinoj","since":"2026-10-28T08:33:00Z"}`, &fleetWaiting{Who: "Rinoj", Since: since}},
		{"answered with note", `{"who":" Rinoj ","since":"2026-10-28T08:33:00Z","answeredAt":"2026-10-28T10:02:00Z","note":"Andy's question"}`,
			&fleetWaiting{Who: "Rinoj", Since: since, AnsweredAt: answered, Note: "Andy's question"}},
		{"missing who", `{"since":"2026-10-28T08:33:00Z"}`, nil},
		{"blank who", `{"who":"  \n ","since":"2026-10-28T08:33:00Z"}`, nil},
		{"who not a string", `{"who":7,"since":"2026-10-28T08:33:00Z"}`, nil},
		{"bad since", `{"who":"Rinoj","since":"yesterday"}`, nil},
		{"missing since", `{"who":"Rinoj"}`, nil},
		{"not an object", `"Rinoj"`, nil},
		{"bad answeredAt kept as waiting", `{"who":"Rinoj","since":"2026-10-28T08:33:00Z","answeredAt":"soon","note":3}`,
			&fleetWaiting{Who: "Rinoj", Since: since}},
		{"long who and note cut", `{"who":"` + long + `","since":"2026-10-28T08:33:00Z","note":"a\n b ` + long + long + long + `"}`,
			&fleetWaiting{Who: strings.Repeat("é", 40), Since: since, Note: string([]rune("a b " + long + long + long)[:120])}},
	} {
		got := waitingFromJSON(t, c.json)
		if (got == nil) != (c.want == nil) {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
			continue
		}
		if got == nil {
			continue
		}
		if got.Who != c.want.Who || got.Note != c.want.Note || !got.Since.Equal(c.want.Since) || !got.AnsweredAt.Equal(c.want.AnsweredAt) {
			t.Errorf("%s: got %+v, want %+v", c.name, *got, *c.want)
		}
	}
}

func TestFleetWaitingText(t *testing.T) {
	loc := time.FixedZone("EET", 2*3600)
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, loc)
	since := time.Date(2026, 10, 28, 8, 33, 0, 0, time.UTC) // 10:33 local, today
	for _, c := range []struct {
		w    fleetWaiting
		want string
	}{
		{fleetWaiting{Who: "Rinoj", Since: since}, "waiting on Rinoj since 10:33"},
		{fleetWaiting{Who: "Rinoj", Since: since, Note: "Andy's question"}, "waiting on Rinoj since 10:33 · Andy's question"},
		{fleetWaiting{Who: "Rinoj", Since: since.Add(-24 * time.Hour)}, "waiting on Rinoj since 10-27 10:33"},
		// 23:30 UTC on the 27th is 01:30 on the 28th here: today.
		{fleetWaiting{Who: "Rinoj", Since: time.Date(2026, 10, 27, 23, 30, 0, 0, time.UTC)}, "waiting on Rinoj since 01:30"},
		{fleetWaiting{Who: "Rinoj", Since: since, AnsweredAt: since.Add(89 * time.Minute)}, "Rinoj answered 12:02"},
		{fleetWaiting{Who: "Rinoj", Since: since, AnsweredAt: since.Add(89 * time.Minute), Note: "Andy's question"},
			"Rinoj answered 12:02 (Andy's question)"},
		{fleetWaiting{Who: "Rinoj", Since: since, AnsweredAt: since.Add(-48 * time.Hour)}, "Rinoj answered 10-26 10:33"},
	} {
		if got := fleetWaitingText(&c.w, now); got != c.want {
			t.Errorf("%+v: got %q, want %q", c.w, got, c.want)
		}
	}
	if got := fleetWaitingText(nil, now); got != "" {
		t.Errorf("nil: %q", got)
	}
}

func TestFleetWaitingNotInHash(t *testing.T) {
	base := fleetCardHash(cardFromJSON(t, `{"kind":"needs-you","title":"T"}`))
	for _, w := range []string{
		`{"who":"Rinoj","since":"2026-10-28T08:33:00Z"}`,
		`{"who":"Rinoj","since":"2026-10-28T08:33:00Z","answeredAt":"2026-10-28T10:02:00Z","note":"n"}`,
	} {
		if got := fleetCardHash(cardFromJSON(t, `{"kind":"needs-you","title":"T","waiting":`+w+`}`)); got != base {
			t.Errorf("waiting %s changed the hash", w)
		}
	}
}

func TestFleetWaitingSanitized(t *testing.T) {
	e := fleetEvilJSON
	body := `{"cards":[{"id":"a","kind":"needs-you","waiting":{"who":` + e + `,"note":` + e + `,"since":"2026-10-28T08:33:00Z"}},` +
		`{"id":"b","kind":"needs-you","waiting":{"who":"\u001b[2J","since":"2026-10-28T08:33:00Z"}}]}`
	v, err := decodeFleetStateView([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	w := v.Cards[0].Waiting
	if w == nil {
		t.Fatal("waiting dropped")
	}
	assertNoEscapes(t, "who", w.Who)
	assertNoEscapes(t, "note", w.Note)
	if w.Who != "ABredC" || w.Note != "ABredC" {
		t.Errorf("who %q note %q", w.Who, w.Note)
	}
	// A who that is nothing but an escape is empty once drawn: no line.
	if v.Cards[1].Waiting != nil {
		t.Errorf("escape-only who kept: %+v", *v.Cards[1].Waiting)
	}
}

func TestFleetWaitingShownInDetailsAndRaw(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	c := fleetTestCard("a", "needs-you")
	c.WaitingOn = "Andy"
	c.Waiting = &fleetWaiting{Who: "Rinoj", Since: time.Date(2026, 10, 28, 8, 33, 0, 0, time.UTC), Note: "Andy's question"}
	it := fleetItem{Card: c, Section: fleetSecDecide}
	const line = "waiting on Rinoj since 08:33 · Andy's question"
	details := stripANSI(strings.Join(fleetDetailLines(&fleetViewState{}, it, 120, now), "\n"))
	raw := strings.Join(fleetRawLines(it, now), "\n")
	for name, s := range map[string]string{"details": details, "raw": raw} {
		if !strings.Contains(s, line) {
			t.Errorf("%s lacks %q:\n%s", name, line, s)
		}
		if strings.Contains(s, "Waiting on: Andy") {
			t.Errorf("%s shows waitingOn next to a valid waiting:\n%s", name, s)
		}
	}
	// No valid waiting: waitingOn renders as before.
	it.Card.Waiting = nil
	details = stripANSI(strings.Join(fleetDetailLines(&fleetViewState{}, it, 120, now), "\n"))
	raw = strings.Join(fleetRawLines(it, now), "\n")
	for name, s := range map[string]string{"details": details, "raw": raw} {
		if !strings.Contains(s, "Waiting on: Andy") || strings.Contains(s, "Rinoj") {
			t.Errorf("%s without waiting:\n%s", name, s)
		}
	}
}

// TestFleetPageWaitingLine runs the page's waitingLine (fleet.html) under
// node on the decode and format cases, so the page and the TUI agree.
// Skipped where node is not installed.
func TestFleetPageWaitingLine(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	page := string(fleetPageHTML)
	i := strings.Index(page, "  const vilniusDay = ")
	j := strings.Index(page, "  // waitSpan:")
	if i < 0 || j < i {
		t.Fatal("fleet.html: vilniusDay .. waitSpan not found in order")
	}
	long := strings.Repeat("é", 50)
	cards := []map[string]any{
		{"waiting": map[string]any{"who": "Rinoj", "since": "2026-10-28T08:33:00Z"}},
		{"waiting": map[string]any{"who": "Rinoj", "since": "2026-10-28T08:33:00Z", "note": "Andy's question"}},
		{"waiting": map[string]any{"who": "Rinoj", "since": "2026-10-27T08:33:00Z"}},
		{"waiting": map[string]any{"who": "Rinoj", "since": "2026-10-28T08:33:00Z", "answeredAt": "2026-10-28T10:02:00Z", "note": "Andy's question"}},
		{"waiting": map[string]any{"who": "Rinoj", "since": "2026-10-28T08:33:00Z", "answeredAt": "2026-10-28T10:02:00Z"}},
		{"waiting": map[string]any{"who": "Rinoj", "since": "2026-10-28T08:33:00Z", "answeredAt": "soon", "note": 3}},
		{"waiting": map[string]any{"who": " ", "since": "2026-10-28T08:33:00Z"}},
		{"waiting": map[string]any{"who": "Rinoj", "since": "Oct 28 2026"}},
		{"waiting": "Rinoj"},
		{"waitingOn": "Andy"},
		{"waiting": map[string]any{"who": long, "since": "2026-10-28T08:33:00Z", "note": "a\n b " + long + long + long}},
	}
	js, _ := json.Marshal(cards)
	// Noon in Vilnius (EET, UTC+2 on 2026-10-28).
	src := `const str = v => typeof v === "string" ? v : "";` +
		`const obj = v => (v && typeof v === "object" && !Array.isArray(v)) ? v : null;` +
		page[i:j] + `const now = new Date("2026-10-28T10:00:00Z");` +
		`console.log(JSON.stringify(` + string(js) + `.map(c => waitingLine(c, now))));`
	out, err := exec.Command(node, "-e", src).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v: %s", err, out)
	}
	var got []*struct {
		Answered bool
		Text     string
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("node output %q: %v", out, err)
	}
	vilnius, err := time.LoadLocation("Europe/Vilnius")
	if err != nil {
		t.Skip("no Europe/Vilnius zone data")
	}
	now := time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC).In(vilnius)
	if len(got) != len(cards) {
		t.Fatalf("got %d results for %d cards", len(got), len(cards))
	}
	for k, card := range cards {
		b, _ := json.Marshal(card)
		want := fleetWaitingText(cardFromJSON(t, string(b)).Waiting, now)
		text := ""
		if got[k] != nil {
			text = got[k].Text
			if answered := strings.Contains(text, " answered "); got[k].Answered != answered {
				t.Errorf("card %d: answered = %v for %q", k, got[k].Answered, text)
			}
		}
		if text != want {
			t.Errorf("card %d: page %q, TUI %q", k, text, want)
		}
	}
	if got[0] == nil || got[0].Text != "waiting on Rinoj since 10:33" {
		t.Errorf("card 0: %+v", got[0])
	}
}
