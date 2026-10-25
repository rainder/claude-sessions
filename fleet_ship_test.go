package main

import (
	"strings"
	"testing"
	"time"
)

// fleetShipNow is the ship tests' clock: mid-afternoon local time, so local
// midnight is a clean boundary.
var fleetShipNow = time.Date(2026, 10, 25, 15, 0, 0, 0, time.Local)

// fleetShipView is a live, busy FLEET with no card, seven working sessions
// and two done cards (one today, one yesterday).
func fleetShipView() fleetStateView {
	v := fleetTestView()
	v.Fleet.Status = "busy"
	v.Sessions = make([]fleetWorkSession, 7)
	v.Done = []fleetDoneCard{
		{At: fleetShipNow.Add(-3 * time.Hour), Ticket: "DR-5190", Title: "cache or not"},
		{At: fleetShipNow.Add(-20 * time.Hour), Ticket: "DR-5000"},
	}
	return v
}

// fleetShipSentView is fleetShipView plus n SENT cards.
func fleetShipSentView(n int) fleetStateView {
	v := fleetShipView()
	for i := range n {
		id := string(rune('a' + i))
		c := fleetTestCard(id, "needs-you")
		v.Cards = append(v.Cards, c)
		v.CardHashes[id] = "h-" + id
		v.Answered[id] = fleetAnswerWith(fleetAttemptSent)
	}
	return v
}

// fleetRowOf is the index of the first line holding sub, -1 when none.
func fleetRowOf(lines []string, sub string) int {
	for i, l := range lines {
		if strings.Contains(l, sub) {
			return i
		}
	}
	return -1
}

func fleetShipFrameAt(st *fleetViewState, frame, cols, rows int) []string {
	st.frame = frame
	return strings.Split(fleetRendered(st, cols, rows, fleetShipNow), "\n")
}

func TestFleetShipArtBySize(t *testing.T) {
	cases := []struct {
		cols, body, text int
		want             *fleetShipArt
	}{
		{100, 26, 3, &fleetShipArts[0]},
		{100, 15, 3, &fleetShipArts[0]}, // 11 + blank + 3
		{100, 14, 3, &fleetShipArts[1]},
		{38, 26, 3, &fleetShipArts[1]}, // too narrow for the tall ship
		{70, 13, 3, &fleetShipArts[1]}, // 9 + blank + 3
		{70, 13, 4, &fleetShipArts[2]}, // a fourth text line no longer fits
		{20, 26, 3, &fleetShipArts[2]}, // too narrow for the sailboat
		{40, 5, 3, &fleetShipArts[2]},
		{40, 4, 3, nil},
		{11, 26, 3, nil},
	}
	for _, c := range cases {
		if got := fleetShipPick(c.cols, c.body, c.text); got != c.want {
			t.Errorf("pick(%d, %d, %d) = %v, want %v", c.cols, c.body, c.text, got, c.want)
		}
	}
	// Through the renderer: the tall ship, the sailboat, the one-liner, text.
	st := fleetTestState(fleetShipView(), fleetShipNow)
	for _, c := range []struct {
		cols, rows int
		want, not  string
	}{
		{100, 30, `_____|____|____|____\\\__`, `|\`},
		{70, 18, `__|___\__`, `)_)`},
		{40, 10, `~~~ _|\_ ~~~`, `__|___\__`},
		{40, 7, fleetShipTitle, `_|\_`},
	} {
		out := fleetRendered(st, c.cols, c.rows, fleetShipNow)
		if !strings.Contains(out, c.want) || strings.Contains(out, c.not) {
			t.Errorf("%dx%d: want %q, not %q:\n%s", c.cols, c.rows, c.want, c.not, out)
		}
	}
}

// The art is exactly the requested shapes at rest (frame 0).
func TestFleetShipArtShapes(t *testing.T) {
	want := map[int][]string{
		0: {
			`              |    |    |`,
			`             )_)  )_)  )_)`,
			`            )___))___))___)\`,
			`           )____)____)_____)\\`,
			`         _____|____|____|____\\\__`,
			`---------\                   /---------`,
			`  ^^^^^ ^^^^^^^^^^^^^^^^^^^^^`,
			`    ^^^^      ^^^^     ^^^    ^^`,
			`         ^^^^      ^^^`,
		},
		1: {
			`        |>`,
			`        |\`,
			`        | \`,
			`        |  \`,
			`      __|___\__`,
			`   ~~~\_______/~~~`,
			`~   ~~    ~   ~~   ~`,
		},
		2: {`~~~ _|\_ ~~~`},
	}
	for i, a := range fleetShipArts {
		got := fleetShipScene(a, 0)
		for j := range got {
			got[j] = stripANSI(got[j])
		}
		if a.sky {
			if got[0] != "" || got[1] != "" {
				t.Errorf("art %d: sky and bob rows not empty at frame 0: %q", i, got[:2])
			}
			got = got[2:]
		}
		if strings.Join(got, "\n") != strings.Join(want[i], "\n") {
			t.Errorf("art %d at frame 0:\n%s\nwant\n%s", i, strings.Join(got, "\n"), strings.Join(want[i], "\n"))
		}
	}
}

// The scene reserves its bob row, so the text lines sit on the same screen
// row in every frame, and the ship (not the sea) rises one row and back.
func TestFleetShipBobKeepsTextFixed(t *testing.T) {
	for _, size := range [][2]int{{100, 30}, {70, 18}, {40, 10}, {100, 16}} {
		st := fleetTestState(fleetShipSentView(2), fleetShipNow)
		base := fleetShipFrameAt(st, 0, size[0], size[1])
		titleRow, sentRow := fleetRowOf(base, fleetShipTitle), fleetRowOf(base, "2 sent")
		if titleRow < 0 || sentRow < 0 {
			t.Fatalf("%v: text missing:\n%s", size, strings.Join(base, "\n"))
		}
		for f := 1; f <= 130; f++ {
			out := fleetShipFrameAt(st, f, size[0], size[1])
			if fleetRowOf(out, fleetShipTitle) != titleRow || fleetRowOf(out, "2 sent") != sentRow {
				t.Fatalf("%v frame %d: text moved:\n%s", size, f, strings.Join(out, "\n"))
			}
		}
	}
	st := fleetTestState(fleetShipView(), fleetShipNow)
	for _, c := range []struct {
		cols, rows  int
		mast, floor string
	}{
		{100, 30, `|    |    |`, `^^^^      ^^^`},
		{70, 18, `|\`, `~   ~~`},
	} {
		rest, up := fleetShipFrameAt(st, 0, c.cols, c.rows), fleetShipFrameAt(st, fleetShipBobHalf, c.cols, c.rows)
		if r, u := fleetRowOf(rest, c.mast), fleetRowOf(up, c.mast); r < 0 || u != r-1 {
			t.Errorf("%dx%d: mast row %d at rest, %d raised", c.cols, c.rows, r, u)
		}
		if back := fleetShipFrameAt(st, 2*fleetShipBobHalf, c.cols, c.rows); fleetRowOf(back, c.mast) != fleetRowOf(rest, c.mast) {
			t.Errorf("%dx%d: the ship did not come back down", c.cols, c.rows)
		}
		// The lowest sea row stays on its row.
		if fleetRowOf(rest, c.floor) < 0 {
			t.Fatalf("%dx%d: no sea row %q", c.cols, c.rows, c.floor)
		}
	}
	for f := range 12 {
		if want := (f % 6) / 3; fleetShipBob(f) != want {
			t.Errorf("bob(%d) = %d, want %d", f, fleetShipBob(f), want)
		}
	}
}

// Waves scroll, each strip at its own speed, and each repeats with its period.
func TestFleetShipWaves(t *testing.T) {
	for i, a := range fleetShipArts {
		first := map[int]bool{} // first frame each strip changes
		hull := len(a.ship) - 1
		if a.sky {
			hull += 2
		}
		strips := append([]fleetSea{a.water}, a.seas...)
		for j, s := range strips {
			row := hull + j
			plain := func(f int) string { return stripANSI(fleetShipScene(a, f)[row]) }
			if j == 0 && strings.Trim(s.pattern, string([]rune(s.pattern)[0])) == "" {
				continue // a uniform strip ("-") looks the same at every shift
			}
			if j == 0 {
				// The water row also holds the hull, which the bob moves;
				// compare frames where the ship is at rest.
				p := s.period()
				for p%(2*fleetShipBobHalf) != 0 {
					p += s.period()
				}
				if plain(0) != plain(p) {
					t.Errorf("art %d water: frame 0 and %d differ", i, p)
				}
				moved := false
				for f := 2 * fleetShipBobHalf; f < p; f += 2 * fleetShipBobHalf {
					moved = moved || plain(f) != plain(0)
				}
				if !moved {
					t.Errorf("art %d water does not move", i)
				}
				continue
			}
			for f := 0; f < 3*s.period(); f++ {
				if plain(f) != plain(f+s.period()) {
					t.Fatalf("art %d sea %d: frame %d and %d differ", i, j, f, f+s.period())
				}
			}
			ch := 0
			for f := 1; f <= s.period(); f++ {
				if plain(f) != plain(0) {
					ch = f
					break
				}
			}
			if ch == 0 {
				t.Errorf("art %d sea %d never moves", i, j)
			}
			first[ch] = true
		}
		if len(a.seas) > 1 && len(first) < 2 {
			t.Errorf("art %d: every sea strip moves at the same speed (no parallax)", i)
		}
	}
	// The sailboat's water line and its sea line move at different speeds.
	if fleetShipSail.water.every == fleetShipSail.seas[0].every {
		t.Error("sailboat: water and sea at the same speed")
	}
}

func TestFleetShipFlagFlutters(t *testing.T) {
	flag := func(f int) string {
		row := stripANSI(fleetShipScene(fleetShipSail, f)[2-fleetShipBob(f)])
		return strings.TrimSpace(row)
	}
	for f, want := range []string{"|>", "|>", "|~", "|~", "|>", "|>", "|~"} {
		if got := flag(f); got != want {
			t.Errorf("frame %d: flag %q, want %q", f, got, want)
		}
	}
	// The tall ship has no flag to flutter.
	for f := range 8 {
		if strings.Contains(strings.Join(fleetShipScene(fleetShipTall, f), ""), "|~") {
			t.Fatalf("tall ship flag at frame %d", f)
		}
	}
}

func TestFleetShipGullCrosses(t *testing.T) {
	for _, a := range []fleetShipArt{fleetShipTall, fleetShipSail} {
		var xs []int
		firstSeen := -1
		for f := range fleetShipGullEvery {
			sky := stripANSI(fleetShipScene(a, f)[0])
			if x := strings.IndexByte(sky, 'v'); x >= 0 {
				if firstSeen < 0 {
					firstSeen = f
				}
				xs = append(xs, x)
				if strings.Count(sky, "v") != 1 {
					t.Fatalf("frame %d: sky %q", f, sky)
				}
			} else if sky != "" {
				t.Fatalf("frame %d: sky without a gull is %q", f, sky)
			}
			if again := stripANSI(fleetShipScene(a, f+fleetShipGullEvery)[0]); again != sky {
				t.Fatalf("frame %d: the gull does not repeat", f)
			}
		}
		if firstSeen <= 0 || len(xs) != fleetShipGullFrames {
			t.Fatalf("gull first seen at %d, %d frames", firstSeen, len(xs))
		}
		if xs[0] != 0 || xs[len(xs)-1] != a.width-1 {
			t.Errorf("gull runs %d..%d, want 0..%d", xs[0], xs[len(xs)-1], a.width-1)
		}
		for i := 1; i < len(xs); i++ {
			if xs[i] < xs[i-1] {
				t.Fatalf("gull goes back: %v", xs)
			}
		}
	}
}

func TestFleetShipTextLines(t *testing.T) {
	plain := func(v fleetStateView, sent int, now time.Time) []string {
		var out []string
		for _, l := range fleetShipTextLines(v, sent, now) {
			out = append(out, l.plain)
		}
		return out
	}
	join := func(s []string) string { return strings.Join(s, " | ") }

	got := plain(fleetShipView(), 0, fleetShipNow)
	want := "all clear, captain | FLEET busy · 7 sessions · 1 done today | last done: DR-5190 · 3h ago"
	if join(got) != want {
		t.Errorf("lines = %q\nwant    %q", join(got), want)
	}

	v := fleetShipView()
	v.Sessions = nil // the server's list failed
	v.Fleet.Status = ""
	if got := plain(v, 0, fleetShipNow); got[1] != "FLEET · 1 done today" {
		t.Errorf("null sessions: %q", got[1])
	}
	v.Sessions = []fleetWorkSession{{Tmux: "x"}}
	if got := plain(v, 0, fleetShipNow); got[1] != "FLEET · 1 session · 1 done today" {
		t.Errorf("one session: %q", got[1])
	}

	v = fleetShipView()
	v.Done = nil
	if got := plain(v, 0, fleetShipNow); len(got) != 2 || got[1] != "FLEET busy · 7 sessions · 0 done today" {
		t.Errorf("no done cards: %q", join(got))
	}

	// Done today: at local midnight counts, a second before does not.
	midnight := time.Date(2026, 10, 25, 0, 0, 0, 0, time.Local)
	v.Done = []fleetDoneCard{{At: midnight, Ticket: "DR-1"}, {At: midnight.Add(-time.Second), Ticket: "DR-2"}}
	if got := plain(v, 0, fleetShipNow); got[1] != "FLEET busy · 7 sessions · 1 done today" || !strings.HasPrefix(got[2], "last done: DR-1 · ") {
		t.Errorf("midnight boundary: %q", join(got))
	}

	// No ticket: the title, cut to 30 runes. Neither: just the age.
	v.Done = []fleetDoneCard{{At: fleetShipNow.Add(-time.Hour), Title: "a very long card title that goes on and on"}}
	if got := plain(v, 0, fleetShipNow); got[2] != "last done: a very long card title that g… · 1h ago" {
		t.Errorf("title fallback: %q", got[2])
	}
	v.Done = []fleetDoneCard{{At: fleetShipNow.Add(-time.Hour)}}
	if got := plain(v, 0, fleetShipNow); got[2] != "last done 1h ago" {
		t.Errorf("no ticket or title: %q", got[2])
	}

	// The SENT line comes last.
	got = plain(fleetShipView(), 2, fleetShipNow)
	if len(got) != 4 || got[3] != "2 sent · waiting for FLEET · Enter shows them" {
		t.Errorf("sent line: %q", join(got))
	}
}

// Rows running short: last done drops first, then the SENT line, then the
// status line; the title stays.
func TestFleetShipTextDropOrder(t *testing.T) {
	st := fleetTestState(fleetShipSentView(1), fleetShipNow)
	for _, c := range []struct {
		body int
		want []string
		not  []string
	}{
		{4, []string{fleetShipTitle, "7 sessions", "last done", "1 sent ·"}, nil},
		{3, []string{fleetShipTitle, "7 sessions", "1 sent ·"}, []string{"last done"}},
		{2, []string{fleetShipTitle, "7 sessions"}, []string{"last done", "1 sent ·"}},
		{1, []string{fleetShipTitle}, []string{"7 sessions", "last done", "1 sent ·"}},
	} {
		out := fleetRendered(st, 60, c.body+4, fleetShipNow)
		for _, w := range c.want {
			if !strings.Contains(out, w) {
				t.Errorf("body %d lacks %q:\n%s", c.body, w, out)
			}
		}
		for _, n := range c.not {
			if strings.Contains(out, n) {
				t.Errorf("body %d still has %q:\n%s", c.body, n, out)
			}
		}
	}
}

func TestFleetShipSanitizes(t *testing.T) {
	body := `{"fleet":{"alive":true,"status":"busy"},"sessions":[{"tmux":"dr-1"}],"cards":[` +
		`{"id":"d1","kind":"done","ticket":"DR-\u001b[38;5;99m5190\u001b]0;evil\u0007","title":"x","doneAt":"2026-10-25T06:00:00Z"},` +
		`{"id":"d2","kind":"done","title":"\u001b[2Jwipe\n  the\u001b[38;5;99m screen","updatedAt":"2026-10-25T07:00:00Z"},` +
		`{"id":"d3","kind":"done","doneAt":5,"updatedAt":"2026-10-24T06:00:00Z"},` +
		`{"id":"d4","kind":"done"},` +
		`{"id":"w","kind":"working","doneAt":"2026-10-25T06:00:00Z"}]}`
	v, err := decodeFleetStateView([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Done) != 3 {
		t.Fatalf("done = %+v", v.Done)
	}
	for i, want := range []time.Time{
		time.Date(2026, 10, 25, 6, 0, 0, 0, time.UTC), // doneAt
		time.Date(2026, 10, 25, 7, 0, 0, 0, time.UTC), // no doneAt: updatedAt
		time.Date(2026, 10, 24, 6, 0, 0, 0, time.UTC), // doneAt not a string: updatedAt
	} {
		if !v.Done[i].At.Equal(want) {
			t.Errorf("done[%d].At = %v, want %v", i, v.Done[i].At, want)
		}
	}
	if v.Done[0].Ticket != "DR-5190" || v.Done[1].Title != "wipe the screen" {
		t.Fatalf("not sanitized: %+v", v.Done)
	}
	if items := fleetItems(v); len(items) != 0 {
		t.Fatalf("done cards became items: %+v", items)
	}
	now := time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)
	st := fleetTestState(v, now)
	raw := renderFleetView(st, 100, 30, now)
	if strings.Contains(raw, "38;5;99") || strings.Contains(raw, "\x1b]") || strings.Contains(raw, "\x1b[2J") || strings.Contains(raw, "evil") {
		t.Fatalf("an escape reached the frame: %q", raw)
	}
	if out := stripANSI(raw); !strings.Contains(out, "last done: wipe the screen · 2h ago") {
		t.Fatalf("last done line missing:\n%s", out)
	}
}

func TestFleetShipTinyFrames(t *testing.T) {
	for _, cols := range []int{10, 30, 60, 100} {
		for rows := 1; rows <= 16; rows++ {
			for _, v := range []fleetStateView{fleetShipView(), fleetShipSentView(3), fleetTestView()} {
				st := fleetTestState(v, fleetShipNow)
				for _, f := range []int{0, 2, 3, 47} {
					st.frame = f
					fleetCheckFrame(t, renderFleetView(st, cols, rows, fleetShipNow), cols, rows)
				}
			}
		}
	}
	// Unknown size: everything, the largest art.
	st := fleetTestState(fleetShipView(), fleetShipNow)
	if out := fleetRendered(st, 100, 0, fleetShipNow); !strings.Contains(out, `)_)`) || !strings.Contains(out, "last done") {
		t.Fatalf("rows 0:\n%s", out)
	}
}

// Which states show the ship: no open and no unconfirmed card. SENT cards
// alone do not block it, and the SENT line counts them.
func TestFleetShipWhen(t *testing.T) {
	sentOnly := fleetShipSentView(2)
	st := fleetTestState(sentOnly, fleetShipNow)
	out := fleetRendered(st, 100, 30, fleetShipNow)
	if !st.shipOn() || !strings.Contains(out, "2 sent · waiting for FLEET · Enter shows them") || strings.Contains(out, "SENT — WAITING") {
		t.Fatalf("sent only: ship %v\n%s", st.shipOn(), out)
	}
	if !strings.HasSuffix(out, fleetHelpShipSent) {
		t.Errorf("help row:\n%s", out)
	}

	unconfirmed := fleetShipSentView(2)
	unconfirmed.Answered["b"] = fleetAnswerWith(fleetAttemptUnconfirmed)
	if st := fleetTestState(unconfirmed, fleetShipNow); st.shipOn() {
		t.Error("an unconfirmed answer needs Andy: the list must show")
	}
	open := fleetShipSentView(1)
	open.Cards = append(open.Cards, fleetTestCard("z", "explicit-yes"))
	open.CardHashes["z"] = "h-z"
	if st := fleetTestState(open, fleetShipNow); st.shipOn() {
		t.Error("an open card: the list must show")
	}
	for name, mod := range map[string]func(*fleetStateView){
		"not running": func(v *fleetStateView) { v.Fleet.Alive = false },
		"ambiguous":   func(v *fleetStateView) { v.Ambiguous = []string{"a", "b"} },
	} {
		v := fleetShipView()
		mod(&v)
		if st := fleetTestState(v, fleetShipNow); st.shipOn() {
			t.Errorf("%s: ship shows", name)
		}
	}
	errSt := &fleetViewState{}
	errSt.applyState(fleetStateView{}, errFleetUnauthorized, fleetShipNow)
	if errSt.shipOn() {
		t.Error("error view: ship shows")
	}
	if (&fleetViewState{}).shipOn() {
		t.Error("loading: ship shows")
	}
}

// Enter on the ship lists the SENT cards; Esc goes back to the ship, a
// second Esc closes the view.
func TestFleetShipEnterListsSent(t *testing.T) {
	st := fleetTestState(fleetShipSentView(2), fleetShipNow)
	if cmd := st.handleKey(KeyEnter, false, fleetShipNow); cmd != fleetCmdRender || st.shipOn() {
		t.Fatalf("Enter: cmd %v ship %v", cmd, st.shipOn())
	}
	out := fleetRendered(st, 100, 30, fleetShipNow)
	if !strings.Contains(out, "SENT — WAITING FOR FLEET") || strings.Contains(out, fleetShipTitle) {
		t.Fatalf("list mode:\n%s", out)
	}
	st.handleKey(KeyDown, false, fleetShipNow)
	if st.sel != 1 {
		t.Errorf("↓ in list mode: sel %d", st.sel)
	}
	// A poll that still shows only SENT cards keeps the list.
	st.applyState(fleetShipSentView(2), nil, fleetShipNow)
	if st.shipOn() {
		t.Fatal("a poll closed the SENT list")
	}
	if cmd := st.handleKey(KeyEsc, false, fleetShipNow); cmd != fleetCmdRender || !st.shipOn() {
		t.Fatalf("Esc in list mode: cmd %v ship %v", cmd, st.shipOn())
	}
	if cmd := st.handleKey(KeyEsc, false, fleetShipNow); cmd != fleetCmdClose {
		t.Fatalf("second Esc: %v", cmd)
	}

	// An open card resets the mode: once it is gone the ship comes back.
	st = fleetTestState(fleetShipSentView(2), fleetShipNow)
	st.handleKey(KeyEnter, false, fleetShipNow)
	open := fleetShipSentView(2)
	open.Cards = append(open.Cards, fleetTestCard("z", "needs-you"))
	open.CardHashes["z"] = "h-z"
	st.applyState(open, nil, fleetShipNow)
	if st.listMode || st.shipOn() {
		t.Fatalf("open card: listMode %v ship %v", st.listMode, st.shipOn())
	}
	if cmd := st.handleKey(KeyEsc, false, fleetShipNow); cmd != fleetCmdClose {
		t.Fatalf("Esc on the normal list: %v", cmd)
	}
	st.applyState(fleetShipSentView(2), nil, fleetShipNow)
	if !st.shipOn() {
		t.Fatal("all SENT again: the ship must show")
	}

	// With no card at all Enter does nothing.
	st = fleetTestState(fleetShipView(), fleetShipNow)
	if cmd := st.handleKey(KeyEnter, false, fleetShipNow); cmd != fleetCmdNone || st.listMode {
		t.Fatalf("Enter with no card: %v %v", cmd, st.listMode)
	}
}

// On the ship screen ↑↓, a, digits and the card keys do nothing.
func TestFleetShipKeysIdle(t *testing.T) {
	for _, v := range []fleetStateView{fleetShipView(), fleetShipSentView(2)} {
		v.Sessions = []fleetWorkSession{{Tmux: "dr-1", Tickets: []string{"DR-1"}}}
		v.Source = "http://src"
		st := fleetTestState(v, fleetShipNow)
		for _, k := range []string{KeyUp, KeyDown, "a", "1", "2", "9", "c", "x", "o", KeyPageUp, KeyPageDown} {
			if cmd := st.handleKey(k, false, fleetShipNow); cmd != fleetCmdNone {
				t.Errorf("%q: cmd %v", k, cmd)
			}
			if st.attach != nil || st.panel != nil || st.unlocked != "" || st.dismissing || st.sel != 0 || st.scroll != 0 || !st.shipOn() {
				t.Fatalf("%q changed the state: %+v", k, st)
			}
		}
	}
}

// The frame timer runs while, and only while, the ship screen shows.
func TestFleetLoopFrameTimer(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	l := &fleetLoop{st: fleetTestState(fleetShipView(), now), interval: time.Hour, nextPoll: now.Add(time.Hour)}
	l.animate(now)
	if d := l.timeout(now); d != fleetShipFrame {
		t.Fatalf("ship: timeout %v, want %v", d, fleetShipFrame)
	}
	l.animate(now.Add(100 * time.Millisecond))
	if l.st.frame != 0 {
		t.Fatalf("frame advanced early: %d", l.st.frame)
	}
	l.animate(now.Add(fleetShipFrame))
	if l.st.frame != 1 || !l.nextFrame.Equal(now.Add(2*fleetShipFrame)) {
		t.Fatalf("frame %d next %v", l.st.frame, l.nextFrame.Sub(now))
	}
	// After a stall (an attach) one frame, then the cadence again.
	late := now.Add(10 * time.Second)
	l.animate(late)
	if l.st.frame != 2 || !l.nextFrame.Equal(late.Add(fleetShipFrame)) {
		t.Fatalf("after a stall: frame %d next %v", l.st.frame, l.nextFrame.Sub(late))
	}

	// List mode: no frame timer, the wait is the poll's again.
	l.st.applyState(fleetShipSentView(1), nil, late)
	l.animate(late)
	if l.nextFrame.IsZero() {
		t.Fatal("sent only: the ship shows, its timer must run")
	}
	l.st.handleKey(KeyEnter, false, late)
	l.animate(late)
	if !l.nextFrame.IsZero() {
		t.Fatal("list mode: a frame timer runs")
	}
	if d := l.timeout(late); d != now.Add(time.Hour).Sub(late) {
		t.Fatalf("list mode: timeout %v", d)
	}
	frame := l.st.frame
	l.animate(late.Add(time.Second))
	if l.st.frame != frame {
		t.Fatal("list mode: the frame moved")
	}

	// Cards: no frame timer.
	cards := &fleetLoop{st: fleetTestState(fleetTestView(fleetTestCard("a", "needs-you")), now), interval: time.Hour, nextPoll: now.Add(time.Hour)}
	cards.animate(now)
	if !cards.nextFrame.IsZero() || cards.timeout(now) != time.Hour {
		t.Fatalf("cards: next frame %v, timeout %v", cards.nextFrame, cards.timeout(now))
	}
	// Not running, error, closed panel states: none either.
	down := fleetShipView()
	down.Fleet.Alive = false
	dl := &fleetLoop{st: fleetTestState(down, now), interval: time.Hour, nextPoll: now.Add(time.Hour)}
	dl.animate(now)
	if !dl.nextFrame.IsZero() {
		t.Fatal("not running: a frame timer runs")
	}
	// The ship coming back starts the timer again.
	l.st.handleKey(KeyEsc, false, late)
	l.animate(late)
	if l.nextFrame.IsZero() {
		t.Fatal("back on the ship: no frame timer")
	}
}
