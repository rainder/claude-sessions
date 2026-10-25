package main

import (
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

// fleetHarborNow is the harbor tests' clock: mid-morning local time, so local
// midnight is a clean boundary.
var fleetHarborNow = time.Date(2026, 10, 25, 9, 0, 0, 0, time.Local)

func fleetHarborAgo(d time.Duration) string {
	return fleetHarborNow.Add(-d).UTC().Format(time.RFC3339)
}

// fleetHarborSessions is a working list of nine rows in no particular order,
// as the decoder leaves it (Label already set).
func fleetHarborSessions() []fleetWorkSession {
	row := func(tmux, ticket, name, tool, model, status string, since time.Duration) fleetWorkSession {
		w := fleetWorkSession{Tmux: tmux, Label: tmux, Name: name, NameSource: "user", Tool: tool, Model: model, Status: status}
		if ticket != "" {
			w.Tickets, w.Label = []string{ticket}, ticket
		}
		if status == "busy" {
			w.StartedAt = fleetHarborAgo(since) // the server sends no since for a busy row
		} else if since > 0 {
			w.Since = fleetHarborAgo(since)
		}
		return w
	}
	return []fleetWorkSession{
		row("dr-5651", "DR-5651", "queue for re-OCR", "claude", "opus", "idle", 2*time.Hour),
		row("dr-5718", "DR-5718", "TRAX AD raw-row subjects", "claude", "opus", "busy", 12*time.Minute),
		row("dr-5690", "DR-5690", "fixture for empty page", "claude", "opus", "waiting", 41*time.Minute),
		row("dr-5702", "DR-5702", "OCR retry policy", "grok", "", "busy", 3*time.Minute),
		row("scratch", "", "scratch notes", "claude", "sonnet", "", 0),
		row("dr-5600", "DR-5600", "nightly backfill", "claude", "opus", "shell", 25*time.Minute),
		row("dr-5610", "DR-5610", "pdf splitter", "claude", "haiku", "idle", 10*time.Minute),
		row("dr-5620", "DR-5620", "label audit", "claude", "opus", "busy", 50*time.Minute),
		row("dr-5630", "DR-5630", "cache warmup", "claude", "opus", "idle", 5*time.Hour),
	}
}

func fleetHarborView() fleetStateView {
	v := fleetTestView()
	v.Sessions = fleetHarborSessions()
	v.Source = "http://src"
	v.DoneAt = []time.Time{fleetHarborNow.Add(-time.Hour), fleetHarborNow.Add(-2 * time.Hour)}
	return v
}

func fleetHarborState(v fleetStateView) *fleetViewState {
	return fleetTestState(v, fleetHarborNow)
}

func fleetHarborTmuxOrder(rows []fleetWorkSession) []string {
	out := make([]string, len(rows))
	for i, w := range rows {
		out[i] = w.Tmux
	}
	return out
}

func TestDecodeFleetStateViewHarborFields(t *testing.T) {
	body := `{"fleet":{"alive":true},"cards":[` +
		`{"id":"d1","kind":"done","doneAt":"2026-10-25T06:00:00Z","updatedAt":"2026-10-20T06:00:00Z"},` +
		`{"id":"d2","kind":"done","doneAt":5,"updatedAt":"2026-10-24T06:00:00Z"},` +
		`{"id":"d3","kind":"done"},` +
		`{"id":"w","kind":"working","doneAt":"2026-10-25T06:00:00Z"}],` +
		`"sessions":[{"tmux":"dr-1","tickets":["DR-\u001b[38;5;99m1"],"name":"\u001b]0;evil\u0007Fix\u001b[38;5;99m  OCR\n retry","nameSource":"user",` +
		`"tool":"claude","model":"claude-opus-4-1","status":"busy","since":"2026-10-25T06:00:00Z","startedAt":"2026-10-25T05:00:00Z","pid":7},` +
		`{"tmux":"\u001b[38;5;99mscratch","name":5,"status":["x"]}]}`
	v, err := decodeFleetStateView([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	want := []fleetWorkSession{
		{Tmux: "dr-1", Tickets: []string{"DR-\x1b[38;5;99m1"}, Label: "DR-1", Name: "Fix OCR retry", NameSource: "user",
			Tool: "claude", Model: "claude-opus-4-1", Status: "busy", Since: "2026-10-25T06:00:00Z", StartedAt: "2026-10-25T05:00:00Z"},
		{Tmux: "\x1b[38;5;99mscratch", Label: "scratch"},
	}
	if !reflect.DeepEqual(v.Sessions, want) {
		t.Fatalf("sessions =\n%+v\nwant\n%+v", v.Sessions, want)
	}
	wantDone := []time.Time{
		time.Date(2026, 10, 25, 6, 0, 0, 0, time.UTC), // doneAt wins
		time.Date(2026, 10, 24, 6, 0, 0, 0, time.UTC), // doneAt not a string: updatedAt
	}
	if len(v.DoneAt) != len(wantDone) {
		t.Fatalf("doneAt = %v", v.DoneAt)
	}
	for i := range wantDone {
		if !v.DoneAt[i].Equal(wantDone[i]) {
			t.Fatalf("doneAt[%d] = %v, want %v", i, v.DoneAt[i], wantDone[i])
		}
	}
	if items := fleetItems(v); len(items) != 0 {
		t.Fatalf("done cards became items: %+v", items)
	}

	// No escape from a session reaches the frame.
	st := fleetTestState(v, fleetHarborNow)
	raw := renderFleetView(st, 100, 20, fleetHarborNow)
	if strings.Contains(raw, "38;5;99") || strings.Contains(raw, "\x1b]") || strings.Contains(raw, "evil") {
		t.Fatalf("an escape reached the frame: %q", raw)
	}
	if out := stripANSI(raw); !strings.Contains(out, "Fix OCR retry") || !strings.Contains(out, "scratch") {
		t.Fatalf("harbor rows missing:\n%s", out)
	}
}

func TestFleetHarborGlyphs(t *testing.T) {
	for status, want := range map[string]string{
		"busy": "⛵", "shell": "⛵", "idle": "⚓", "waiting": "⏳", "": "·", "weird": "·",
	} {
		if got := fleetHarborGlyph(status); got != want {
			t.Errorf("glyph(%q) = %q, want %q", status, got, want)
		}
	}
}

func TestCountFleetHarbor(t *testing.T) {
	v := fleetHarborView()
	midnight := time.Date(2026, 10, 25, 0, 0, 0, 0, time.Local)
	v.DoneAt = []time.Time{midnight, midnight.Add(-time.Second), midnight.Add(5 * time.Hour), midnight.Add(-30 * time.Hour)}
	got := countFleetHarbor(v, fleetHarborNow)
	want := fleetHarborCounts{AtSea: 4, InPort: 3, Waiting: 1, DoneToday: 2}
	if got != want {
		t.Fatalf("counts = %+v, want %+v", got, want)
	}
	out := fleetRendered(fleetHarborState(v), 100, 24, fleetHarborNow)
	if !strings.Contains(out, "⛵ at sea  4      ⚓ in port  3      ⏳ waiting  1      ✓ done today  2") {
		t.Fatalf("counters line:\n%s", out)
	}
}

func TestFleetHarborOrder(t *testing.T) {
	got := fleetHarborTmuxOrder(fleetHarborOrder(fleetHarborSessions()))
	want := []string{
		"dr-5690",                                  // waiting
		"dr-5620", "dr-5600", "dr-5718", "dr-5702", // at sea, oldest first (busy by startedAt)
		"dr-5630", "dr-5651", "dr-5610", // in port, oldest first
		"scratch", // unknown
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v\nwant    %v", got, want)
	}
	// Ties by tmux; a row with no stamp after those with one.
	ties := []fleetWorkSession{
		{Tmux: "b", Status: "idle", Since: fleetHarborAgo(time.Hour)},
		{Tmux: "c", Status: "idle"},
		{Tmux: "a", Status: "idle", Since: fleetHarborAgo(time.Hour)},
	}
	if got := fleetHarborTmuxOrder(fleetHarborOrder(ties)); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Fatalf("ties = %v", got)
	}
	if fleetHarborOrder(nil) != nil {
		t.Fatal("nil list must stay nil")
	}
}

func TestFleetHarborRows(t *testing.T) {
	st := fleetHarborState(fleetHarborView())
	out := fleetRendered(st, 100, 24, fleetHarborNow)
	for _, want := range []string{
		"~  nothing needs you  ~",
		"▸ ⏳ DR-5690  fixture for empty page",
		"⛵ DR-5702  OCR retry policy",
		"⚓ DR-5630  cache warmup",
		"·  scratch  scratch notes",
		"↑↓ session · a attach · Esc back",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("lacks %q:\n%s", want, out)
		}
	}
	// Columns: model (grok's tool when it has none), status word, age.
	for _, re := range []string{`DR-5690\s+fixture for empty page\s+opus\s+waiting\s+41m`, `DR-5702\s+OCR retry policy\s+grok\s+busy\s+3m`} {
		if !regexpMatch(re, out) {
			t.Errorf("no row matching %s:\n%s", re, out)
		}
	}
	// A card-only key help is not shown.
	if strings.Contains(out, "1-9 pick option") || strings.Contains(out, "No cards wait") {
		t.Errorf("card help in the harbor:\n%s", out)
	}
}

func regexpMatch(re, s string) bool {
	for _, l := range strings.Split(s, "\n") {
		if regexp.MustCompile(re).MatchString(l) {
			return true
		}
	}
	return false
}

func TestFleetHarborNarrowColumns(t *testing.T) {
	st := fleetHarborState(fleetHarborView())
	row := func(cols int) string {
		for _, l := range strings.Split(fleetRendered(st, cols, 30, fleetHarborNow), "\n") {
			if strings.Contains(l, "DR-5690") {
				return l
			}
		}
		t.Fatalf("%d cols: no DR-5690 row", cols)
		return ""
	}
	wide := row(100)
	if !strings.Contains(wide, "opus") || !strings.Contains(wide, "waiting") || !strings.Contains(wide, "fixture for empty page") {
		t.Fatalf("wide row = %q", wide)
	}
	// The fixture's widths: 37 fixed columns with every column, 24 for the
	// longest name; the model takes 8 of them, the status word 9.
	mid := row(56)
	if strings.Contains(mid, "opus") || !strings.Contains(mid, "waiting") || !strings.Contains(mid, "fixture for empty page") {
		t.Fatalf("56 cols: model must go first: %q", mid)
	}
	narrow := row(48)
	if strings.Contains(narrow, "opus") || strings.Contains(narrow, "waiting") || !strings.Contains(narrow, "fixture for empty page") || !strings.Contains(narrow, "41m") {
		t.Fatalf("48 cols: status must go next, before the name is cut: %q", narrow)
	}
	tiny := row(30)
	if !strings.Contains(tiny, "…") {
		t.Fatalf("30 cols: name must be cut: %q", tiny)
	}
	for cols := 10; cols <= 120; cols++ {
		for _, l := range strings.Split(renderFleetView(st, cols, 30, fleetHarborNow), "\n") {
			if w := visualLen(l); w > cols {
				t.Fatalf("%d cols: line %d wide: %q", cols, w, stripANSI(l))
			}
		}
	}
}

// ⛵ ⚓ ⏳ take two columns: a row's width math counts them so.
func TestFleetHarborWideGlyphWidth(t *testing.T) {
	rows := fleetHarborOrder(fleetHarborSessions())
	for _, cols := range []int{30, 47, 60, 100} {
		l := fleetHarborColumns(rows, cols, fleetHarborNow)
		for i, w := range rows {
			r := fleetHarborRow(w, l, i == 0, fleetHarborNow)
			if got, want := visualLen(r), l.width(); got != want {
				t.Errorf("%d cols, %s: row is %d wide, layout says %d: %q", cols, w.Tmux, got, want, stripANSI(r))
			}
			if visualLen(r) > cols && cols >= l.fixed() {
				t.Errorf("%d cols: row wider than the screen: %q", cols, stripANSI(r))
			}
		}
	}
	if got := visualLen(fleetHarborCounters(fleetHarborCounts{1, 2, 3, 4}, 200)); got > 200 {
		t.Fatalf("counters %d wide", got)
	}
	c := stripANSI(fleetHarborCounters(fleetHarborCounts{1, 2, 3, 4}, 100))
	if cellWidth(strings.TrimLeft(c, " "))+2*((100-cellWidth(strings.TrimLeft(c, " ")))/2) > 100 {
		t.Fatalf("counters not centered by cells: %q", c)
	}
}

func TestFleetHarborSelection(t *testing.T) {
	v := fleetHarborView()
	st := fleetHarborState(v)
	if st.harborSel != 0 || st.harborSelected().Tmux != "dr-5690" {
		t.Fatalf("start sel = %d", st.harborSel)
	}
	if cmd := st.handleKey(KeyUp, false, fleetHarborNow); cmd != fleetCmdRender || st.harborSel != 0 {
		t.Fatalf("up at top: cmd %v sel %d", cmd, st.harborSel)
	}
	for range 3 {
		st.handleKey(KeyDown, false, fleetHarborNow)
	}
	if got := st.harborSelected().Tmux; got != "dr-5718" {
		t.Fatalf("after 3 down: %s", got)
	}
	if st.sel != 0 {
		t.Fatalf("card selection moved: %d", st.sel)
	}
	for range 20 {
		st.handleKey(KeyDown, false, fleetHarborNow)
	}
	if got := st.harborSelected().Tmux; got != "scratch" {
		t.Fatalf("down at bottom: %s", got)
	}

	// Survives a refetch by tmux name, even when its index moves.
	st.harborSel = 3 // dr-5718
	v2 := fleetHarborView()
	for i := range v2.Sessions {
		if v2.Sessions[i].Tmux == "dr-5651" {
			v2.Sessions[i].Status, v2.Sessions[i].Since = "waiting", fleetHarborAgo(3*time.Hour)
		}
	}
	st.applyState(v2, nil, fleetHarborNow)
	if got := st.harborSelected().Tmux; got != "dr-5718" || st.harborSel != 4 {
		t.Fatalf("after refetch: %s at %d", got, st.harborSel)
	}

	// Clamps when rows vanish.
	v3 := fleetHarborView()
	v3.Sessions = v3.Sessions[:2]
	st.harborSel = 8
	st.applyState(v3, nil, fleetHarborNow)
	if st.harborSel != 1 {
		t.Fatalf("clamped sel = %d", st.harborSel)
	}
	v3.Sessions = []fleetWorkSession{}
	st.applyState(v3, nil, fleetHarborNow)
	if st.harborSel != 0 || st.harborSelected() != nil {
		t.Fatalf("empty list sel = %d", st.harborSel)
	}
}

func TestFleetHarborAttach(t *testing.T) {
	var got []fleetAttachRequest
	l := &fleetLoop{
		st:    fleetHarborState(fleetHarborView()),
		state: func() (fleetStateView, error) { return fleetHarborView(), nil },
		attach: func(r fleetAttachRequest) error {
			got = append(got, r)
			return nil
		},
		interval: time.Hour,
	}
	defer l.closeAll()
	l.st.handleKey(KeyDown, false, fleetHarborNow)
	if l.run(l.st.handleKey("a", false, fleetHarborNow), fleetHarborNow) {
		t.Fatal("attach closed the view")
	}
	if len(got) != 1 || got[0] != (fleetAttachRequest{Tmux: "dr-5620", Source: "http://src"}) {
		t.Fatalf("attach calls = %+v", got)
	}
	waitFleetJobs(t, l)
	l.reap(fleetHarborNow)

	// A paste holding "a" never attaches.
	if cmd := l.st.handleKey("a", true, fleetHarborNow); cmd != fleetCmdNone {
		t.Fatalf("batched a: %v", cmd)
	}

	// Nothing selected: no rows, or the list failed.
	for name, sessions := range map[string][]fleetWorkSession{"empty": {}, "null": nil} {
		v := fleetHarborView()
		v.Sessions = sessions
		st := fleetHarborState(v)
		if cmd := st.handleKey("a", false, fleetHarborNow); cmd != fleetCmdNone || st.attach != nil {
			t.Fatalf("%s: cmd %v attach %+v", name, cmd, st.attach)
		}
		if out := fleetRendered(st, 100, 24, fleetHarborNow); strings.Contains(out, "a attach") {
			t.Fatalf("%s: hint shown:\n%s", name, out)
		}
	}

	// A raw tmux name the attach would refuse is not offered.
	for _, bad := range []string{"a.b", "bad name", "-x;rm", ""} {
		v := fleetHarborView()
		v.Sessions = []fleetWorkSession{{Tmux: bad, Label: "x", Status: "idle"}}
		st := fleetHarborState(v)
		if cmd := st.handleKey("a", false, fleetHarborNow); cmd != fleetCmdNone || st.attach != nil {
			t.Fatalf("%q: cmd %v attach %+v", bad, cmd, st.attach)
		}
		if out := fleetRendered(st, 100, 24, fleetHarborNow); strings.Contains(out, "a attach") {
			t.Fatalf("%q: hint shown:\n%s", bad, out)
		}
	}
}

func TestFleetHarborCardKeysIgnored(t *testing.T) {
	st := fleetHarborState(fleetHarborView())
	for _, k := range []string{"1", "2", "9", "o", "c", "x", KeyPageDown, KeyPageUp} {
		if cmd := st.handleKey(k, false, fleetHarborNow); cmd != fleetCmdNone || st.panel != nil || st.dismissing || st.unlocked != "" {
			t.Fatalf("%q: cmd %v", k, cmd)
		}
	}
	if cmd := st.handleKey(KeyEsc, false, fleetHarborNow); cmd != fleetCmdClose {
		t.Fatalf("Esc: %v", cmd)
	}
}

func TestFleetHarborOverflow(t *testing.T) {
	st := fleetHarborState(fleetHarborView())
	// 14 rows: header, rule, 10 body rows, rule, help. The body is title,
	// gap, counters, gap, 5 rows for the list (4 sessions and the marker),
	// and the wave.
	out := fleetRendered(st, 100, 14, fleetHarborNow)
	lines := strings.Split(out, "\n")
	if !strings.Contains(lines[10], "⇣ 5 more · a attach") || strings.Contains(lines[10], "⇡") {
		t.Fatalf("marker line = %q\n%s", lines[10], out)
	}
	if !strings.HasPrefix(lines[10], "      ") {
		t.Fatalf("marker not right-aligned: %q", lines[10])
	}
	if !strings.Contains(lines[11], "~") || strings.TrimSpace(lines[11]) == "" {
		t.Fatalf("wave row = %q", lines[11])
	}
	for range 6 {
		st.handleKey(KeyDown, false, fleetHarborNow)
	}
	out = fleetRendered(st, 100, 14, fleetHarborNow)
	if !strings.Contains(out, "▸ ⚓ DR-5651") {
		t.Fatalf("selection scrolled out of view:\n%s", out)
	}
	if !strings.Contains(out, "⇡ 3 more · ⇣ 2 more · a attach") {
		t.Fatalf("both markers:\n%s", out)
	}
	for range 10 {
		st.handleKey(KeyDown, false, fleetHarborNow)
	}
	out = fleetRendered(st, 100, 14, fleetHarborNow)
	if !strings.Contains(out, "▸ ·  scratch") || !strings.Contains(out, "⇡ 5 more · a attach") || strings.Contains(out, "⇣") {
		t.Fatalf("bottom:\n%s", out)
	}
}

func TestFleetHarborSessionsUnavailable(t *testing.T) {
	v := fleetHarborView()
	v.Sessions = nil
	out := fleetRendered(fleetHarborState(v), 100, 24, fleetHarborNow)
	for _, want := range []string{"nothing needs you", "⛵ at sea  0", "✓ done today  2", "session list unavailable"} {
		if !strings.Contains(out, want) {
			t.Errorf("lacks %q:\n%s", want, out)
		}
	}
}

func TestFleetHarborWavePhase(t *testing.T) {
	st := fleetHarborState(fleetHarborView())
	wave := func() string {
		lines := strings.Split(fleetRendered(st, 60, 20, fleetHarborNow), "\n")
		return lines[len(lines)-3]
	}
	w0 := wave()
	if !strings.Contains(w0, "~") {
		t.Fatalf("no wave: %q", w0)
	}
	// Redraws and keys do not move it.
	st.handleKey(KeyDown, false, fleetHarborNow)
	if w := wave(); w != w0 {
		t.Fatalf("a key moved the wave: %q → %q", w0, w)
	}
	// A failed fetch is not a new state.
	st.applyState(fleetStateView{}, errFleetUnauthorized, fleetHarborNow)
	st.applyState(fleetHarborView(), nil, fleetHarborNow)
	w1 := wave()
	if w1 == w0 || w1 != fleetHarborShift(w0, 1) {
		t.Fatalf("one applied state: %q → %q", w0, w1)
	}
	st.applyState(fleetHarborView(), nil, fleetHarborNow)
	if w := wave(); w != fleetHarborShift(w0, 2) {
		t.Fatalf("two applied states: %q → %q", w0, w)
	}
}

// fleetHarborShift is wave s moved on by n phases: the wave is
// fleetHarborWave at some phase p, and this is the same width at p+n.
func fleetHarborShift(s string, n int) string {
	for p := range len(fleetHarborWavePat) {
		if stripANSI(fleetHarborWave(len(s), p)) == s {
			return stripANSI(fleetHarborWave(len(s), p+n))
		}
	}
	return ""
}

func TestFleetHarborTinyFrames(t *testing.T) {
	for _, cols := range []int{10, 30, 60} {
		for rows := 1; rows <= 16; rows++ {
			for _, sessions := range [][]fleetWorkSession{fleetHarborSessions(), nil, {}} {
				v := fleetHarborView()
				v.Sessions = sessions
				st := fleetHarborState(v)
				st.handleKey(KeyDown, false, fleetHarborNow)
				fleetCheckFrame(t, renderFleetView(st, cols, rows, fleetHarborNow), cols, rows)
			}
		}
	}
	// Degrade order: with 3 body rows the wave and the gaps are gone, the
	// title, counters and one list row stay.
	st := fleetHarborState(fleetHarborView())
	out := strings.Split(fleetRendered(st, 100, 7, fleetHarborNow), "\n")
	body := out[2:5]
	if !strings.Contains(body[0], "nothing needs you") || !strings.Contains(body[1], "at sea") || !strings.Contains(body[2], "▸ ⏳ DR-5690") {
		t.Fatalf("3 body rows:\n%s", strings.Join(out, "\n"))
	}
	// One body row: the selected session alone.
	out = strings.Split(fleetRendered(st, 100, 5, fleetHarborNow), "\n")
	if !strings.Contains(out[2], "▸ ⏳ DR-5690") {
		t.Fatalf("1 body row:\n%s", strings.Join(out, "\n"))
	}
	// Unknown size: everything.
	all := fleetRendered(st, 100, 0, fleetHarborNow)
	for _, want := range []string{"nothing needs you", "at sea", "DR-5690", "scratch", "~   ."} {
		if !strings.Contains(all, want) {
			t.Fatalf("rows 0 lacks %q:\n%s", want, all)
		}
	}
}

// Cards present: the stacked layout, exactly as without the working list.
func TestFleetHarborCardsReturnStacked(t *testing.T) {
	card := fleetTestCard("a", "needs-you")
	card.Ticket, card.rawTicket = "DR-9999", "DR-9999"
	with := fleetHarborView()
	with.Cards = []fleetCardView{card}
	with.CardHashes = map[string]string{"a": "h-a"}
	without := with
	without.Sessions, without.DoneAt = nil, nil
	for _, size := range [][2]int{{100, 24}, {60, 14}, {30, 6}} {
		a := renderFleetView(fleetHarborState(with), size[0], size[1], fleetHarborNow)
		b := renderFleetView(fleetHarborState(without), size[0], size[1], fleetHarborNow)
		if a != b {
			t.Fatalf("%v: working list changed the card view:\n%s\n---\n%s", size, stripANSI(a), stripANSI(b))
		}
		if strings.Contains(stripANSI(a), "nothing needs you") {
			t.Fatalf("harbor with a card:\n%s", stripANSI(a))
		}
	}
	// ↑↓ move the card selection again, not the harbor's.
	with.Cards = append(with.Cards, fleetTestCard("b", "needs-you"))
	with.CardHashes["b"] = "h-b"
	st := fleetHarborState(with)
	st.handleKey(KeyDown, false, fleetHarborNow)
	if st.sel != 1 || st.harborSel != 0 {
		t.Fatalf("sel %d harborSel %d", st.sel, st.harborSel)
	}
	if cmd := st.handleKey("1", false, fleetHarborNow); cmd != fleetCmdRender || st.panel == nil {
		t.Fatalf("digit with cards: %v", cmd)
	}
}
