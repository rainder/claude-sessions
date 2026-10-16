package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSessionTickets(t *testing.T) {
	cases := []struct {
		s    Session
		want []string
	}{
		{Session{Tmux: "spawn-dr-5187-ad-terminates-ref-30477b:0.0"}, []string{"DR-5187"}},
		{Session{Tmux: "spawn-dr-4974-x:0.0", CWD: "/home/a/repo/.claude/worktrees/DR-5158"}, []string{"DR-4974", "DR-5158"}},
		{Session{Tmux: "spawn-dr-5100-dr-5101-slug:1.2"}, []string{"DR-5100", "DR-5101"}},
		// A 4-digit group after an id is another id. A 3-digit or 5-digit
		// group is not. The hex tail is not an id. The worktree id is kept.
		{Session{Tmux: "spawn-dr-5241-5243-5242-extraction-tail-fc8477:0.0"}, []string{"DR-5241", "DR-5243", "DR-5242"}},
		{Session{Tmux: "spawn-dr-5241-5243:0.0", WorktreeName: "DR-5244"}, []string{"DR-5241", "DR-5243", "DR-5244"}},
		{Session{Tmux: "spawn-dr-5241-524-x:0.0"}, []string{"DR-5241"}},
		{Session{Tmux: "spawn-dr-5241-52435-x:0.0"}, []string{"DR-5241"}},
		{Session{Tmux: "spawn-dr-5241-5243-dr-9:0.0"}, []string{"DR-5241", "DR-5243", "DR-9"}},
		// Order: tmux, worktree name, cwd, name; once each.
		{Session{Tmux: "w:0.0", WorktreeName: "DR-2", CWD: "/x/dr-1", Name: "DR-2 and DR-3"}, []string{"DR-2", "DR-1", "DR-3"}},
		// A bare number, a hex suffix, or a longer word is no ticket.
		{Session{Tmux: "trecs-brain-1ac893:0.0", CWD: "/home/a/5187", Name: "ADR-12 and DR-12abc"}, []string{}},
		{Session{}, []string{}},
	}
	for _, c := range cases {
		if got := sessionTickets(c.s); !reflect.DeepEqual(got, c.want) {
			t.Errorf("sessionTickets(%+v) = %v, want %v", c.s, got, c.want)
		}
	}
}

func workRows() []Session {
	return []Session{
		{PID: 7, Name: "FLEET", NameSource: "user", SessionID: "fleet-sid", Status: "shell", Tmux: "fleet:0.0"},
		{PID: 8, Tool: toolGrok, Name: "FLEET", NameSource: "derived", SessionID: "g-fleet", Status: "idle", Tmux: "named-fleet:0.0"},
		{PID: 9, SessionID: "off", Status: "idle", Tmux: "off:0.0"},
		{PID: 10, SessionID: "no-tmux", Status: "idle"},
		{PID: 11, SessionID: "remote", Status: "idle", Tmux: "r:0.0", Host: "pi"},
		{PID: 20, SessionID: "busy", Status: "busy", Tmux: "a-busy:0.0", UpdatedAt: 1000, Model: "claude-opus-5-5"},
		{PID: 21, SessionID: "none", Tmux: "b-none:0.0"},
		{PID: 22, SessionID: "shell", Status: "shell", Tmux: "c-shell:0.0"},
		{PID: 23, SessionID: "idle-z", Status: "idle", Tmux: "z-idle:0.0"},
		{PID: 24, SessionID: "idle-a", Status: "idle", Tmux: "spawn-dr-5-a:0.0", Name: "Fix it", NameSource: "user",
			UpdatedAt: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC).UnixMilli(), StartedAt: time.Date(2026, 10, 13, 8, 0, 0, 0, time.UTC).UnixMilli()},
		{PID: 25, SessionID: "wait", Status: "busy", WaitingFor: "permission prompt", Tmux: "y-wait:0.0", Tool: toolGrok, Model: "grok-4"},
	}
}

func TestFleetWorkRowsFilterAndOrder(t *testing.T) {
	s := fleetTestServer(t, nil)
	s.fleet.disabled = func(sid string) bool { return sid == "off" }
	rows := s.fleetWorkRows(workRows(), time.Now())
	var got []string
	for _, r := range rows {
		got = append(got, r.Tmux+"/"+r.Status)
	}
	want := []string{"y-wait/waiting", "spawn-dr-5-a/idle", "z-idle/idle", "c-shell/shell", "b-none/", "a-busy/busy"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rows = %v, want %v", got, want)
	}
	r := rows[1]
	if r.PID != 24 || r.Name != "Fix it" || r.NameSource != "user" || !reflect.DeepEqual(r.Tickets, []string{"DR-5"}) ||
		r.Tool != "claude" || r.Since != "2026-10-13T09:00:00Z" || r.StartedAt != "2026-10-13T08:00:00Z" {
		t.Fatalf("idle row = %+v", r)
	}
	if rows[0].Tool != "grok" || rows[0].Model != "grok-4" {
		t.Fatalf("grok row = %+v", rows[0])
	}
	if busy := rows[5]; busy.Since != "" || busy.Model != "claude-opus-5-5" || busy.Tickets == nil {
		t.Fatalf("busy row = %+v", busy)
	}
	if s.fleetWorkRows(nil, time.Now()) != nil {
		t.Fatal("a failed list must give nil rows")
	}
	if rows := s.fleetWorkRows([]Session{}, time.Now()); rows == nil || len(rows) != 0 {
		t.Fatalf("an empty list must give [] rows, got %v", rows)
	}
}

func TestFleetNotes(t *testing.T) {
	state := map[string]json.RawMessage{"notes": json.RawMessage(`{
		"spawn-dr-5-a": "  waits on review  ",
		"gone-session": "not shown",
		"bad name": "x",
		"z-idle": 7,
		"c-shell": "line\nbreak",
		"b-none": "` + strings.Repeat("é", 250) + `"}`)}
	notes := fleetNotes(state)
	if notes["spawn-dr-5-a"] != "waits on review" {
		t.Fatalf("note = %q", notes["spawn-dr-5-a"])
	}
	for _, k := range []string{"bad name", "z-idle", "c-shell"} {
		if _, ok := notes[k]; ok {
			t.Fatalf("note %q must be dropped", k)
		}
	}
	if n := []rune(notes["b-none"]); len(n) != fleetNoteMax {
		t.Fatalf("long note has %d runes", len(n))
	}
	s := fleetTestServer(t, nil)
	rows := s.fleetWorkRows(workRows(), time.Now())
	fleetApplyNotes(rows, notes)
	byTmux := map[string]string{}
	for _, r := range rows {
		byTmux[r.Tmux] = r.Note
	}
	if byTmux["spawn-dr-5-a"] != "waits on review" || byTmux["z-idle"] != "" || len(byTmux) != len(rows) {
		t.Fatalf("joined notes = %v", byTmux)
	}
	if fleetNotes(map[string]json.RawMessage{"notes": json.RawMessage(`[1]`)}) != nil {
		t.Fatal("notes that are not an object must be ignored")
	}
}

func row(pid int, sid, tmux string, tickets ...string) fleetWorkRow {
	if tickets == nil {
		tickets = []string{}
	}
	return fleetWorkRow{Tmux: tmux, PID: pid, Name: tmux + " name", Tickets: tickets, Tool: "claude", key: idleKey(Session{PID: pid, SessionID: sid})}
}

// observeTwice is one look at at and a second fleetEndedGrace later: what a
// row must miss to count as ended.
func observeTwice(tr *fleetEndedTracker, rows []fleetWorkRow, all map[string]bool, at time.Time) {
	tr.observe(rows, all, at)
	tr.observe(rows, all, at.Add(fleetEndedGrace))
}

func TestFleetEndedTracker(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state", "fleet-ended.json")
	tr := &fleetEndedTracker{path: path}
	t0 := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	a, b := row(1, "a", "sess-a", "DR-1"), row(2, "b", "sess-b")
	a.TicketStatus = map[string]string{"DR-1": "in development"}

	tr.observe([]fleetWorkRow{a, b}, nil, t0)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("nothing ended: no write")
	}
	tr.observe(nil, nil, t0.Add(time.Minute)) // a failed list changes nothing
	observeTwice(tr, []fleetWorkRow{b}, nil, t0.Add(2*time.Minute))
	got := tr.list(t0.Add(3 * time.Minute))
	if len(got) != 1 || got[0].Tmux != "sess-a" || got[0].EndedAt != "2026-10-13T10:00:00Z" ||
		got[0].TicketStatus["DR-1"] != "in development" || !reflect.DeepEqual(got[0].Tickets, []string{"DR-1"}) {
		t.Fatalf("ended = %+v (endedAt is the last time it was seen)", got)
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("saved file: %v %v", st, err)
	}

	// No change: no write.
	mt := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	_ = os.Chtimes(path, mt, mt)
	tr.observe([]fleetWorkRow{b}, nil, t0.Add(3*time.Minute))
	if st, _ := os.Stat(path); !st.ModTime().Equal(mt) {
		t.Fatal("unchanged ended list was written")
	}

	// Reload from disk.
	tr2 := &fleetEndedTracker{path: path}
	if got := tr2.list(t0.Add(3 * time.Minute)); len(got) != 1 || got[0].Tmux != "sess-a" {
		t.Fatalf("reloaded = %+v", got)
	}

	// A new live row in the ended row's tmux session drops it.
	a2 := row(3, "a2", "sess-a")
	tr2.observe([]fleetWorkRow{a2, b}, nil, t0.Add(4*time.Minute))
	if got := tr2.list(t0.Add(4 * time.Minute)); len(got) != 0 {
		t.Fatalf("live again must drop: %+v", got)
	}

	// Two rows end in one tmux session at different times: the newest stays.
	c1, c2 := row(4, "c1", "sess-c"), row(5, "c2", "sess-c")
	tr3 := &fleetEndedTracker{}
	tr3.observe([]fleetWorkRow{c1, c2, b}, nil, t0)
	observeTwice(tr3, []fleetWorkRow{c2, b}, nil, t0.Add(time.Minute)) // c1 gone, but c2 still live there
	if got := tr3.list(t0.Add(2 * time.Minute)); len(got) != 0 {
		t.Fatalf("a live row in the same tmux session: %+v", got)
	}
	observeTwice(tr3, []fleetWorkRow{b}, nil, t0.Add(2*time.Minute))
	got = tr3.list(t0.Add(3 * time.Minute))
	if len(got) != 1 || got[0].EndedAt != "2026-10-13T10:01:15Z" {
		t.Fatalf("one per tmux session: %+v", got)
	}

	// A row that left the list but still runs (disabled) did not end.
	tr4 := &fleetEndedTracker{}
	tr4.observe([]fleetWorkRow{c1, b}, nil, t0)
	observeTwice(tr4, []fleetWorkRow{b}, map[string]bool{c1.key: true, b.key: true}, t0.Add(time.Minute))
	if got := tr4.list(t0.Add(2 * time.Minute)); len(got) != 0 {
		t.Fatalf("a still-running row ended: %+v", got)
	}

	// 24 h later the row is gone, from the list and from the file.
	tr5 := &fleetEndedTracker{path: filepath.Join(dir, "five.json")}
	tr5.observe([]fleetWorkRow{c1}, nil, t0)
	observeTwice(tr5, []fleetWorkRow{}, nil, t0.Add(time.Minute))
	if got := tr5.list(t0.Add(24 * time.Hour)); len(got) != 1 {
		t.Fatalf("inside 24 h: %+v", got)
	}
	tr5.observe([]fleetWorkRow{}, nil, t0.Add(25*time.Hour))
	if got := tr5.list(t0.Add(25 * time.Hour)); len(got) != 0 {
		t.Fatalf("after 24 h: %+v", got)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "five.json"))
	if !strings.Contains(string(data), `"ended":[]`) {
		t.Fatalf("file after prune = %s", data)
	}
}

// One missing look must not end a row: the observers read lists of
// different age, and a torn grok registry read drops every grok row once.
func TestFleetEndedTrackerDebounce(t *testing.T) {
	t0 := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	a, b := row(1, "a", "sess-a"), row(2, "b", "sess-b")

	// A single miss, then back: nothing ended, nothing written.
	path := filepath.Join(t.TempDir(), "ended.json")
	tr := &fleetEndedTracker{path: path}
	tr.observe([]fleetWorkRow{a, b}, nil, t0)
	tr.observe([]fleetWorkRow{b}, nil, t0.Add(time.Second))
	tr.observe([]fleetWorkRow{a, b}, nil, t0.Add(30*time.Second))
	tr.observe([]fleetWorkRow{b}, nil, t0.Add(40*time.Second)) // a new first miss
	tr.observe([]fleetWorkRow{a, b}, nil, t0.Add(60*time.Second))
	if got := tr.list(t0.Add(time.Minute)); len(got) != 0 {
		t.Fatalf("a single miss ended a row: %+v", got)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("a single miss wrote the file")
	}

	// Two misses less than the grace apart: not ended yet.
	tr2 := &fleetEndedTracker{}
	tr2.observe([]fleetWorkRow{a, b}, nil, t0)
	tr2.observe([]fleetWorkRow{b}, nil, t0.Add(time.Second))
	tr2.observe([]fleetWorkRow{b}, nil, t0.Add(10*time.Second))
	if got := tr2.list(t0.Add(10 * time.Second)); len(got) != 0 {
		t.Fatalf("ended inside the grace: %+v", got)
	}
	// Still missing 20 s after the first miss: ended, at the last time seen.
	tr2.observe([]fleetWorkRow{b}, nil, t0.Add(21*time.Second))
	got := tr2.list(t0.Add(21 * time.Second))
	if len(got) != 1 || got[0].Tmux != "sess-a" || got[0].EndedAt != "2026-10-13T10:00:00Z" {
		t.Fatalf("ended = %+v", got)
	}

	// Every grok row vanishes for one poll (a torn registry read): nothing ends.
	g1, g2 := row(3, "g1", "grok-1"), row(4, "g2", "grok-2")
	g1.Tool, g2.Tool = "grok", "grok"
	tr3 := &fleetEndedTracker{}
	tr3.observe([]fleetWorkRow{a, g1, g2}, nil, t0)
	tr3.observe([]fleetWorkRow{a}, nil, t0.Add(30*time.Second))
	tr3.observe([]fleetWorkRow{a, g1, g2}, nil, t0.Add(60*time.Second))
	tr3.observe([]fleetWorkRow{a, g1, g2}, nil, t0.Add(90*time.Second))
	if got := tr3.list(t0.Add(90 * time.Second)); len(got) != 0 {
		t.Fatalf("a torn grok read ended rows: %+v", got)
	}
}

// addLocked reports no change when a newer ended row for the same tmux
// session is already there, so observe writes nothing.
func TestFleetEndedAddKeepsTheNewer(t *testing.T) {
	tr := &fleetEndedTracker{loaded: true}
	if !tr.addLocked(fleetEndedRow{Tmux: "x", EndedAt: "2026-10-13T10:05:00Z"}) {
		t.Fatal("first add")
	}
	if tr.addLocked(fleetEndedRow{Tmux: "x", EndedAt: "2026-10-13T10:00:00Z"}) {
		t.Fatal("an older row must not change the list")
	}
	if !tr.addLocked(fleetEndedRow{Tmux: "x", EndedAt: "2026-10-13T10:06:00Z"}) || len(tr.ended) != 1 || tr.ended[0].EndedAt != "2026-10-13T10:06:00Z" {
		t.Fatalf("newer replaces: %+v", tr.ended)
	}

	// Through observe: the older end of the same tmux session is no write.
	path := filepath.Join(t.TempDir(), "ended.json")
	t0 := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	tr2 := &fleetEndedTracker{path: path, loaded: true, live: map[string]fleetEndedLive{},
		ended: []fleetEndedRow{{Tmux: "sess-a", EndedAt: "2026-10-13T10:10:00Z", Tickets: []string{}}}}
	tr2.live["1:a"] = fleetEndedLive{row: row(1, "a", "sess-a"), lastSeen: t0, missingSince: t0.Add(time.Second)}
	tr2.observe([]fleetWorkRow{}, nil, t0.Add(time.Minute))
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("an end older than the stored one was written")
	}
}

func TestFleetEndedTrackerLoadDropsBadRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ended.json")
	body := `{"ended":[{"tmux":"ok","endedAt":"2026-10-13T10:00:00Z","tickets":["DR-1","x;y"],"ticketStatus":{"DR-1":"done","DR-2":"BAD!"}},
		{"tmux":"bad name","endedAt":"2026-10-13T10:00:00Z"},{"tmux":"no-time","endedAt":"soon"}]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	tr := &fleetEndedTracker{path: path}
	got := tr.list(time.Date(2026, 10, 13, 11, 0, 0, 0, time.UTC))
	if len(got) != 1 || !reflect.DeepEqual(got[0].Tickets, []string{"DR-1"}) || !reflect.DeepEqual(got[0].TicketStatus, map[string]string{"DR-1": "done"}) {
		t.Fatalf("loaded = %+v", got)
	}
}

func TestFleetStateSessionsAndEnded(t *testing.T) {
	now := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	rows := []Session{fleetRow(), {PID: 30, SessionID: "w", Status: "idle", Tmux: "spawn-dr-9-x:0.0"}}
	s := fleetTestServer(t, nil)
	s.collect = func() ([]Session, error) { return append([]Session(nil), rows...), nil }
	s.sessionCache.now = func() time.Time { return now }
	s.fleet.now = func() time.Time { return now }
	s.fleet.tickets.record("DR-9", "in development", true, now)
	writeFleetFile(t, s, "state.json", `{"writtenAt":"2026-10-13T09:40:00Z","cards":[],"notes":{"spawn-dr-9-x":"on it","fleet":"no"}}`)
	out := getFleetJSON(t, s, "/fleet/state")
	list, ok := out["sessions"].([]any)
	if !ok || len(list) != 1 {
		t.Fatalf("sessions = %v", out["sessions"])
	}
	r := list[0].(map[string]any)
	if r["tmux"] != "spawn-dr-9-x" || r["note"] != "on it" || r["tool"] != "claude" ||
		!reflect.DeepEqual(r["tickets"], []any{"DR-9"}) || !reflect.DeepEqual(r["ticketStatus"], map[string]any{"DR-9": "in development"}) {
		t.Fatalf("row = %v", r)
	}
	if e, ok := out["ended"].([]any); !ok || len(e) != 0 {
		t.Fatalf("ended = %v", out["ended"])
	}
	if _, ok := out["notes"]; !ok {
		t.Fatal("FLEET's notes pass through")
	}

	// The worker ends; the status cache moves on. One missing look is not
	// an end; a second one past the grace is.
	rows = rows[:1]
	now = now.Add(2 * time.Second)
	s.fleet.tickets.record("DR-9", "testing", true, now)
	out = getFleetJSON(t, s, "/fleet/state")
	if e := out["ended"].([]any); len(e) != 0 {
		t.Fatalf("ended after one miss = %v", e)
	}
	now = now.Add(fleetEndedGrace + time.Second)
	out = getFleetJSON(t, s, "/fleet/state")
	ended := out["ended"].([]any)
	if len(ended) != 1 {
		t.Fatalf("ended = %v", ended)
	}
	e := ended[0].(map[string]any)
	if e["tmux"] != "spawn-dr-9-x" || e["endedAt"] != "2026-10-13T10:00:00Z" || e["ticketStatus"].(map[string]any)["DR-9"] != "testing" {
		t.Fatalf("ended row = %v", e)
	}
	if l := out["sessions"].([]any); len(l) != 0 {
		t.Fatalf("sessions after end = %v", l)
	}

	// A failed list: sessions is null, ended is still there.
	s.collect = func() ([]Session, error) { return nil, errors.New("boom") }
	now = now.Add(2 * time.Second)
	out = getFleetJSON(t, s, "/fleet/state")
	if v, ok := out["sessions"]; !ok || v != nil {
		t.Fatalf("failed list: sessions = %v", v)
	}
	if e, ok := out["ended"].([]any); !ok || len(e) != 1 {
		t.Fatalf("failed list: ended = %v", out["ended"])
	}
}

func TestParseTicketStatus(t *testing.T) {
	cases := []struct {
		out, want string
		ok        bool
	}{
		{"DR-5187 Fix it\nStatus: in development\nPriority: high\nStatus: done\n", "in development", true},
		{"Status:   TESTING  \r\n", "testing", true},
		{"no status here\n", "", false},
		// Custom statuses: digits, "/", "_" and "-" are fine.
		{"Status: in-dev\n", "in-dev", true},
		{"Status: In Review/QA 2\n", "in review/qa 2", true},
		{"Status: dev_done\n", "dev_done", true},
		{"Status: done!\n", "", false},
		{"Status: -done\n", "", false},
		{"Status: " + strings.Repeat("a", 33) + "\n", "", false},
		{"  Status: done\n", "", false}, // not at the line start
		// Only the header block counts: after the first blank line comes the
		// description, which strangers write.
		{"DR-5187  Title\nPriority: High\n\nDescription\nStatus: done\n", "", false},
		{"DR-5187  Title\nStatus: testing\nPriority: High\n\nDescription\nStatus: done\n", "testing", true},
		{"\nStatus: done\n", "", false},
	}
	for _, c := range cases {
		got, err := parseTicketStatus(c.out)
		if got != c.want || (err == nil) != c.ok {
			t.Errorf("parseTicketStatus(%q) = %q, %v", c.out, got, err)
		}
	}
}

func TestTicketStatusDueRules(t *testing.T) {
	now := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	c := &fleetTicketCache{}
	c.record("DR-1", "done", true, now.Add(-20*time.Minute))        // stale try
	c.record("DR-2", "done", true, now.Add(-5*time.Minute))         // fresh
	c.record("DR-3", "done", true, now.Add(-5*time.Minute))         // fresh, but went quiet since
	c.record("DR-4", "", false, now.Add(-30*time.Minute))           // failed, oldest try
	c.record("DR-5", "in progress", true, now.Add(-10*time.Minute)) // fresh, quiet before the try
	wants := []fleetTicketWant{
		{ticket: "DR-1"}, {ticket: "DR-2"}, {ticket: "DR-3", quiet: now.Add(-time.Minute)}, {ticket: "DR-4"},
		{ticket: "DR-5", quiet: now.Add(-11 * time.Minute)}, {ticket: "DR-6"}, {ticket: "DR-7"},
	}
	got := c.dueTickets(wants, now, 10)
	want := []string{"DR-6", "DR-7", "DR-4", "DR-1", "DR-3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("due = %v, want %v", got, want)
	}
	if got := c.dueTickets(wants, now, fleetTicketFetchMax); len(got) != 4 {
		t.Fatalf("cap: %v", got)
	}
}

// statusRig is a server with the ClickUp log in a temp dir and a fake cu.
func statusRig(t *testing.T, rows []Session) (*server, *[]string, map[string]string) {
	t.Helper()
	s := fleetTestServer(t, rows)
	s.clickup.logPath = filepath.Join(t.TempDir(), "clickup-events.log")
	s.clickup.statePath = filepath.Join(t.TempDir(), "clickup-state.json")
	answers := map[string]string{}
	var asked []string
	s.clickup.fetchStatus = func(_ context.Context, ticket string) (string, error) {
		asked = append(asked, ticket)
		a, ok := answers[ticket]
		if !ok {
			return "", errClickupTimeout
		}
		return a, nil
	}
	return s, &asked, answers
}

func TestRefreshTicketStatuses(t *testing.T) {
	now := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	var rows []Session
	for i := 1; i <= 6; i++ {
		rows = append(rows, Session{PID: 100 + i, SessionID: "s" + string(rune('0'+i)), Status: "busy",
			Tmux: "spawn-dr-" + string(rune('0'+i)) + "-x:0.0"})
	}
	s, asked, answers := statusRig(t, rows)
	s.sessionCache.now = func() time.Time { return now }
	for i := 1; i <= 6; i++ {
		answers["DR-"+string(rune('0'+i))] = "Title\nStatus: in development\n"
	}
	answers["DR-2"] = "garbage"
	cfg := defaultClickupConfig()
	s.refreshTicketStatuses(context.Background(), cfg, now)
	if len(*asked) != fleetTicketFetchMax {
		t.Fatalf("asked %v, want %d", *asked, fleetTicketFetchMax)
	}
	if s.fleet.tickets.get("DR-1", now) != "in development" || s.fleet.tickets.get("DR-2", now) != "" {
		t.Fatalf("cache DR-1=%q DR-2=%q", s.fleet.tickets.get("DR-1", now), s.fleet.tickets.get("DR-2", now))
	}
	logData, _ := os.ReadFile(s.clickup.logPath)
	if !strings.Contains(string(logData), "status fetch failed: bad output") {
		t.Fatalf("log = %q", logData)
	}

	// Next call: the two never tried go first; the failure is not asked again.
	*asked = nil
	s.refreshTicketStatuses(context.Background(), cfg, now.Add(time.Minute))
	if !reflect.DeepEqual(*asked, []string{"DR-5", "DR-6"}) {
		t.Fatalf("second pass asked %v", *asked)
	}

	// A failure keeps the old status.
	delete(answers, "DR-1")
	*asked = nil
	later := now.Add(20 * time.Minute)
	s.sessionCache.now = func() time.Time { return later }
	s.refreshTicketStatuses(context.Background(), cfg, later)
	if s.fleet.tickets.get("DR-1", later) != "in development" {
		t.Fatalf("failure dropped the status: %q", s.fleet.tickets.get("DR-1", later))
	}
	if strings.Count(string(mustRead(t, s.clickup.logPath)), "status fetch failed: timeout") != 1 {
		t.Fatalf("log = %q", mustRead(t, s.clickup.logPath))
	}

	// A session that goes idle after the last try is checked on the next call.
	*asked = nil
	quietAt := later.Add(time.Minute)
	rows[2].Status, rows[2].UpdatedAt = "idle", quietAt.UnixMilli()
	s.collect = func() ([]Session, error) { return append([]Session(nil), rows...), nil }
	s.invalidateSessions()
	s.refreshTicketStatuses(context.Background(), cfg, quietAt.Add(time.Minute))
	// DR-5 and DR-6 are due by age (the 20-minute pass hit the cap first);
	// DR-3 was tried a minute ago and is due only because it went quiet.
	if !reflect.DeepEqual(*asked, []string{"DR-5", "DR-6", "DR-3"}) {
		t.Fatalf("quiet pass asked %v", *asked)
	}

	// Entries no row names are dropped.
	s.collect = func() ([]Session, error) { return rows[:1], nil }
	s.invalidateSessions()
	*asked = nil
	s.refreshTicketStatuses(context.Background(), cfg, quietAt.Add(90*time.Second))
	s.invalidateSessions()
	s.refreshTicketStatuses(context.Background(), cfg, quietAt.Add(2*time.Minute)) // past the grace: ended
	s.fleet.tickets.mu.Lock()
	n := len(s.fleet.tickets.m)
	s.fleet.tickets.mu.Unlock()
	// DR-1 is live; DR-2..DR-6 ended, so the ended group still names them.
	if n == 0 || s.fleet.tickets.get("DR-1", quietAt) == "" {
		t.Fatalf("cache size %d", n)
	}
	s.fleet.ended.mu.Lock()
	s.fleet.ended.ended = nil
	s.fleet.ended.mu.Unlock()
	s.refreshTicketStatuses(context.Background(), cfg, quietAt.Add(3*time.Minute))
	s.fleet.tickets.mu.Lock()
	keys := len(s.fleet.tickets.m)
	_, has1 := s.fleet.tickets.m["DR-1"]
	s.fleet.tickets.mu.Unlock()
	if keys != 1 || !has1 {
		t.Fatalf("cache after prune: %d entries", keys)
	}
}

func TestRefreshTicketStatusesRefusesABadTicket(t *testing.T) {
	now := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	s, asked, _ := statusRig(t, []Session{})
	s.fleet.ended.loaded = true
	s.fleet.ended.ended = []fleetEndedRow{{Tmux: "x", EndedAt: now.Format(time.RFC3339), Tickets: []string{"DR-1;rm", "--help", "dr-2"}}}
	s.refreshTicketStatuses(context.Background(), defaultClickupConfig(), now)
	if len(*asked) != 0 {
		t.Fatalf("a bad ticket reached cu: %v", *asked)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestIdleMessageFinishedEntry(t *testing.T) {
	now := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	status := map[string]string{"DR-1": "testing", "DR-2": "in development", "DR-3": "done"}
	lookup := func(t string) string { return status[t] }
	events := []idleEvent{
		{PID: 1, Status: "idle", Tmux: "spawn-dr-1:0.0", Since: now.Add(-3 * time.Minute), Label: "DR-1", Tickets: []string{"DR-1"}},
		{PID: 2, Status: "idle", Tmux: "spawn-dr-2:0.0", Since: now.Add(-3 * time.Minute), Label: "DR-2", Tickets: []string{"DR-2"}},
		{PID: 3, Status: "shell", Tmux: "w:0.0", Since: now, Label: "x", Tickets: []string{"DR-3", "DR-1"}, Reminder: true},
		{PID: 4, Status: "idle", Tmux: "q:0.0", Since: now, Label: "-", Tickets: []string{"bad;id"}},
		{PID: 5, Status: "idle", Tmux: "n:0.0", Since: now, Label: "-"},
	}
	got := idleMessage(events, now, lookup)
	want := `[claude-sessions event] idle finished: 1 spawn-dr-1:0.0 DR-1 testing ; ` +
		`idle: 2 idle spawn-dr-2:0.0 (3 min) "DR-2" ; ` +
		`finished (reminder, 4h same message): 3 w:0.0 DR-3 done ; ` +
		`idle: 4 idle q:0.0 (0 min) "-" ; ` +
		`idle: 5 idle n:0.0 (0 min) "-"`
	if got != want {
		t.Fatalf("message\n got %q\nwant %q", got, want)
	}
	// A cold cache is a plain idle line.
	if got := idleMessage(events[:1], now, func(string) string { return "" }); got != `[claude-sessions event] idle idle: 1 idle spawn-dr-1:0.0 (3 min) "DR-1"` {
		t.Fatalf("cold cache: %q", got)
	}
	// A status outside the fixed shape is ignored.
	if got := idleMessage(events[:1], now, func(string) string { return "Done\n" }); strings.Contains(got, "finished") {
		t.Fatalf("bad status: %q", got)
	}
}

// Finished needs every one of the row's own tickets (tmux session name and
// worktree name) to be finished; a ticket in the cwd or the name does not
// count.
func TestIdleFinishedUsesOwnTicketsOnly(t *testing.T) {
	status := map[string]string{
		"DR-100": "done", "DR-200": "in development", "DR-1": "done", "DR-2": "testing", "DR-3": "in development",
		"DR-5241": "testing", "DR-5243": "done", "DR-5242": "in development", "DR-5244": "testing",
	}
	lookup := func(t string) string { return status[t] }
	cases := []struct {
		row    Session
		ticket string
		ok     bool
	}{
		// The name and cwd mention a done ticket; the session's own is not done.
		{Session{Tmux: "spawn-dr-200-x:0.0", Name: "follow-up to DR-100", CWD: "/r/.claude/worktrees/DR-100"}, "", false},
		{Session{Tmux: "spawn-dr-1-dr-2:0.0"}, "DR-1", true},
		{Session{Tmux: "spawn-dr-1-dr-3:0.0"}, "", false},
		{Session{Tmux: "plain:0.0", WorktreeName: "DR-2"}, "DR-2", true},
		{Session{Tmux: "plain:0.0", Name: "DR-1"}, "", false}, // no own ticket
		// One open id in the grouped name, or only on the worktree, blocks finished.
		{Session{Tmux: "spawn-dr-5241-5243-5242-extraction-tail-fc8477:0.0"}, "", false},
		{Session{Tmux: "spawn-dr-5241-5243:0.0", WorktreeName: "DR-5242"}, "", false},
	}
	for _, c := range cases {
		e := idleEvent{Tickets: idleOwnTickets(c.row)}
		tk, _, ok := idleFinished(e, lookup)
		if ok != c.ok || tk != c.ticket {
			t.Errorf("%+v: finished=%v %q, want %v %q", c.row, ok, tk, c.ok, c.ticket)
		}
	}
	if got := sessionOwnTickets(Session{Tmux: "spawn-dr-5-x:0.0", WorktreeName: "DR-6", CWD: "/x/DR-7", Name: "DR-8"}); !reflect.DeepEqual(got, []string{"DR-5", "DR-6"}) {
		t.Fatalf("own tickets = %v", got)
	}
	grouped := Session{Tmux: "spawn-dr-5241-5243-5242-extraction-tail-fc8477:0.0", WorktreeName: "DR-5244"}
	if got := sessionOwnTickets(grouped); !reflect.DeepEqual(got, []string{"DR-5241", "DR-5243", "DR-5242", "DR-5244"}) {
		t.Fatalf("grouped own tickets = %v", got)
	}
	// Every id finished: the line names the first one.
	allDone := func(id string) string {
		if id == "DR-5241" || id == "DR-5243" || id == "DR-5242" {
			return "testing"
		}
		return ""
	}
	tk, st, ok := idleFinished(idleEvent{Tickets: idleOwnTickets(Session{Tmux: grouped.Tmux})}, allDone)
	if !ok || tk != "DR-5241" || st != "testing" {
		t.Fatalf("all done: ok=%v %q %q", ok, tk, st)
	}
	// A missing status is unknown, so the row stays idle.
	unknown := func(id string) string {
		if id == "DR-5242" {
			return ""
		}
		return "done"
	}
	if _, _, ok := idleFinished(idleEvent{Tickets: idleOwnTickets(Session{Tmux: "spawn-dr-5241-5242:0.0"})}, unknown); ok {
		t.Fatal("unknown status counted as finished")
	}
}

func TestTicketStatusExpires(t *testing.T) {
	now := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	c := &fleetTicketCache{}
	c.record("DR-1", "done", true, now)
	if c.get("DR-1", now.Add(fleetTicketStatusMaxAge)) != "done" {
		t.Fatal("inside the max age")
	}
	late := now.Add(fleetTicketStatusMaxAge + time.Minute)
	// A failed try later does not make an old status fresh.
	c.record("DR-1", "", false, late)
	if c.get("DR-1", late) != "" {
		t.Fatal("an old status must not be served")
	}
	if got := c.statuses([]string{"DR-1"}, map[string]string{"DR-1": "testing"}, late); got["DR-1"] != "testing" {
		t.Fatalf("an old status falls back to the stored one: %v", got)
	}
	if got := c.statuses([]string{"DR-1"}, nil, late); got != nil {
		t.Fatalf("an old status with no fallback: %v", got)
	}
	// The idle line reads the same rule.
	s := fleetTestServer(t, nil)
	s.fleet.tickets.record("DR-9", "done", true, now)
	if s.idleTicketStatus("DR-9", now) != "done" || s.idleTicketStatus("DR-9", late) != "" {
		t.Fatal("idleTicketStatus ignores the max age")
	}
}

func TestIdleEventsSendFinishedFromTheCache(t *testing.T) {
	rig := newIdleRig(t, idleCfgOn)
	rig.rows = []Session{fleetRow(), worker(4242, "w1", "idle", "spawn-dr-77-x:0.0")}
	rig.s.fleet.tickets.record("DR-77", "complete", true, rig.now)
	rig.run(3 * time.Minute)
	want := `[claude-sessions event] idle finished: 4242 spawn-dr-77-x:0.0 DR-77 complete`
	if len(rig.typed) != 1 || rig.typed[0] != want {
		t.Fatalf("typed %q", rig.typed)
	}
	// The idle tick lets the ended tracker see its fresh list.
	rig.rows = []Session{fleetRow()}
	rig.tick(30 * time.Second)
	rig.tick(30 * time.Second) // the second miss, past the grace
	if got := rig.s.fleet.ended.list(rig.now); len(got) != 1 || got[0].Tmux != "spawn-dr-77-x" {
		t.Fatalf("ended after idle tick = %+v", got)
	}
}

func TestIdleLabelFallsBackToTheDerivedTicket(t *testing.T) {
	r := Session{Tmux: "spawn-dr-5187-ref:0.0", Name: "My name", NameSource: "user"}
	if got := idleLabel(r, nil); got != "DR-5187" {
		t.Fatalf("no card: %q", got)
	}
	cards := map[string]fleetCardInfo{"spawn-dr-5187-ref": {Ticket: "DR-1"}}
	if got := idleLabel(r, cards); got != "DR-1" {
		t.Fatalf("card wins: %q", got)
	}
	r.Tmux = "plain:0.0"
	if got := idleLabel(r, nil); got != "My name" {
		t.Fatalf("no ticket: %q", got)
	}
}
