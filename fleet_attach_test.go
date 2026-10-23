package main

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// fleetAttachCard is a decision card whose raw session and ticket are set as
// a decoded state.json card would have them.
func fleetAttachCard(id, session, ticket string) fleetCardView {
	c := fleetTestCard(id, "needs-you")
	c.Session, c.Ticket = session, ticket
	c.rawSession, c.rawTicket = session, ticket
	return c
}

func TestFleetAttachTarget(t *testing.T) {
	work := []fleetWorkSession{
		{Tmux: "dr-100-fix", Tickets: []string{"DR-100"}},
		{Tmux: "dr-200-a", Tickets: []string{"DR-200"}},
		{Tmux: "dr-200-b", Tickets: []string{"DR-200", "DR-300"}},
		// Two rows in one tmux session are one place to attach to.
		{Tmux: "dr-400", Tickets: []string{"DR-400"}},
		{Tmux: "dr-400", Tickets: []string{"DR-400"}},
		// A row the server should never list: a bad name never matches.
		{Tmux: "x;rm -rf ~", Tickets: []string{"DR-500"}},
		{Tmux: "-foo", Tickets: []string{"DR-600"}},
	}
	cases := []struct {
		name            string
		session, ticket string
		sessions        []fleetWorkSession
		want            fleetAttachPick
	}{
		{"session field", "dr-100-fix", "", work, fleetAttachPick{Tmux: "dr-100-fix"}},
		{"session pane form", "dr-100-fix:0.0", "", work, fleetAttachPick{Tmux: "dr-100-fix"}},
		{"session wins over an ambiguous ticket", "dr-200-a", "DR-200", work, fleetAttachPick{Tmux: "dr-200-a"}},
		{"session not live falls back to ticket", "gone-session", "DR-100", work, fleetAttachPick{Tmux: "dr-100-fix"}},
		{"session not live and no ticket", "gone-session", "", work, fleetAttachPick{}},
		{"ticket one row", "", "DR-300", work, fleetAttachPick{Tmux: "dr-200-b"}},
		{"ticket is case-insensitive", "", " dr-100 ", work, fleetAttachPick{Tmux: "dr-100-fix"}},
		{"ticket two rows", "", "DR-200", work, fleetAttachPick{Many: 2}},
		{"ticket two rows in one tmux session", "", "DR-400", work, fleetAttachPick{Tmux: "dr-400"}},
		{"ticket no row", "", "DR-999", work, fleetAttachPick{}},
		{"no session no ticket", "", "", work, fleetAttachPick{}},
		{"bad session name", "x;rm -rf ~", "", work, fleetAttachPick{}},
		{"bad row name via ticket", "", "DR-500", work, fleetAttachPick{}},
		{"dash name passes the regex", "-foo", "", work, fleetAttachPick{Tmux: "-foo"}},
		{"null sessions", "dr-100-fix", "DR-100", nil, fleetAttachPick{}},
		{"empty sessions", "dr-100-fix", "DR-100", []fleetWorkSession{}, fleetAttachPick{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := fleetAttachTarget(fleetAttachCard("a", tc.session, tc.ticket), tc.sessions)
			if got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// The match uses the raw card fields, not the sanitized ones drawn: an escape
// stripped for display must not turn a bad name into a good one.
func TestFleetAttachTargetUsesRawFields(t *testing.T) {
	work := []fleetWorkSession{{Tmux: "ab", Tickets: []string{"DR-1"}}}
	c := fleetTestCard("a", "needs-you")
	c.Session, c.rawSession = "ab", "a\x1b[31mb"
	c.Ticket, c.rawTicket = "DR-1", "DR-\x1b[0m1"
	if got := fleetAttachTarget(c, work); got != (fleetAttachPick{}) {
		t.Fatalf("got %+v, want no match", got)
	}
}

func TestFleetAttachArgv(t *testing.T) {
	srv := &ServerConfig{Name: "ws", Host: "10.0.0.2", Port: 8765, SSHHost: "ws.example", SSHUser: "andy"}
	cases := []struct {
		name   string
		srv    *ServerConfig
		tmux   string
		inside bool
		want   []string
	}{
		{"local attach", nil, "dr-1", false, []string{"tmux", "attach", "-t", "=dr-1"}},
		{"local inside tmux", nil, "dr-1", true, []string{"tmux", "switch-client", "-t", "=dr-1"}},
		{"local dash name", nil, "-foo", false, []string{"tmux", "attach", "-t", "=-foo"}},
		{"remote", srv, "dr-1", false, append(append([]string{"ssh"}, sshMuxFlags...),
			"-t", "andy@ws.example", "tmux", "attach", "-t", "'=dr-1'")},
		{"remote inside tmux still attaches over ssh", srv, "-foo", true, append(append([]string{"ssh"}, sshMuxFlags...),
			"-t", "andy@ws.example", "tmux", "attach", "-t", "'=-foo'")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := fleetAttachArgv(tc.srv, tc.tmux, tc.inside)
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("argv = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
	for _, bad := range []string{"", "x;rm -rf ~", "a b", "a'b", "$(id)", "a:0.0", "=a", "a.b", strings.Repeat("a", 129)} {
		for _, s := range []*ServerConfig{nil, srv} {
			if got, err := fleetAttachArgv(s, bad, false); err == nil || got != nil {
				t.Fatalf("argv(%q, remote=%v) = %q, %v; want refused", bad, s != nil, got, err)
			}
		}
	}
}

// fleetAttachCall is one call the fake attach saw.
type fleetAttachCall struct {
	srv  *ServerConfig
	tmux string
}

func TestFleetAttachViaDispatch(t *testing.T) {
	local := newFleetTestHost(t, "local", true, 0, 0)
	ws := newFleetTestHost(t, "ws", true, 0, 0)
	var calls []fleetAttachCall
	run := func(srv *ServerConfig, tmux string) error {
		calls = append(calls, fleetAttachCall{srv, tmux})
		return nil
	}

	// Local FLEET: no ServerConfig, so the local path.
	src := fleetTestSource(local, ws)
	v, err := src.State()
	if err != nil || v.Host != "" {
		t.Fatalf("State = %+v, %v", v, err)
	}
	if err := fleetAttachVia(src, run)(fleetAttachRequest{Tmux: "dr-1", Source: v.Source}); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].srv != nil || calls[0].tmux != "dr-1" {
		t.Fatalf("local calls = %+v", calls)
	}

	// Remote FLEET: the chosen host's ServerConfig.
	local.alive.Store(false)
	v, err = src.State()
	if err != nil || v.Host != "ws" {
		t.Fatalf("State = %+v, %v", v, err)
	}
	if err := fleetAttachVia(src, run)(fleetAttachRequest{Tmux: "dr-2", Source: v.Source}); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[1].srv == nil || calls[1].srv.Name != "ws" || calls[1].srv.Host != ws.cfg.Host || calls[1].tmux != "dr-2" {
		t.Fatalf("remote calls = %+v", calls[1:])
	}

	// The remembered remote down: still that host, never the local service.
	ws.status.Store(500)
	if _, err := src.State(); err == nil {
		t.Fatal("want host down")
	}
	if err := fleetAttachVia(src, run)(fleetAttachRequest{Tmux: "dr-3", Source: v.Source}); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 3 || calls[2].srv == nil || calls[2].srv.Name != "ws" {
		t.Fatalf("sticky calls = %+v", calls[2:])
	}
}

func TestFleetAttachViaPinnedToHost(t *testing.T) {
	a := newFleetTestHost(t, "a", true, 0, 0)
	b := newFleetTestHost(t, "b", false, 0, 0)
	src := fleetTestSource(nil, a, b)
	va, err := src.State()
	if err != nil || va.Host != "a" {
		t.Fatalf("first: %+v, %v", va, err)
	}
	a.alive.Store(false)
	b.alive.Store(true)
	if v, err := src.State(); err != nil || v.Host != "b" {
		t.Fatalf("moved: %+v, %v", v, err)
	}
	called := 0
	run := func(*ServerConfig, string) error { called++; return nil }
	if err := fleetAttachVia(src, run)(fleetAttachRequest{Tmux: "dr-1", Source: va.Source}); !errors.Is(err, errFleetMoved) {
		t.Fatalf("err = %v, want errFleetMoved", err)
	}
	// No state applied yet, and a fresh source with no host chosen.
	if err := fleetAttachVia(src, run)(fleetAttachRequest{Tmux: "dr-1"}); !errors.Is(err, errFleetMoved) {
		t.Fatalf("empty source: err = %v", err)
	}
	if err := fleetAttachVia(fleetTestSource(nil), run)(fleetAttachRequest{Tmux: "dr-1", Source: va.Source}); !errors.Is(err, errFleetMoved) {
		t.Fatalf("no host chosen: err = %v", err)
	}
	// A bad name never reaches the runner, even on the right host.
	vb, _ := src.State()
	if err := fleetAttachVia(src, run)(fleetAttachRequest{Tmux: "x;rm -rf ~", Source: vb.Source}); err == nil {
		t.Fatal("bad name: want refused")
	}
	if called != 0 {
		t.Fatalf("runner called %d times", called)
	}
}

// fleetAttachView is an alive FLEET with card a (session dr-1), card b
// (ticket DR-2, two live sessions) and card c (no live session).
func fleetAttachView() fleetStateView {
	v := fleetTestView(fleetAttachCard("a", "dr-1", "DR-1"), fleetAttachCard("b", "", "DR-2"), fleetAttachCard("c", "", "DR-9"))
	v.Sessions = []fleetWorkSession{
		{Tmux: "dr-1", Tickets: []string{"DR-1"}},
		{Tmux: "dr-2-a", Tickets: []string{"DR-2"}},
		{Tmux: "dr-2-b", Tickets: []string{"DR-2"}},
	}
	v.Source = "http://host-a:8765"
	return v
}

func TestFleetViewAttachKey(t *testing.T) {
	now := time.Unix(1000, 0)

	st := fleetTestState(fleetAttachView(), now)
	if cmd := st.handleKey("a", false, now); cmd != fleetCmdAttach {
		t.Fatalf("cmd = %v, want attach", cmd)
	}
	if st.attach == nil || *st.attach != (fleetAttachRequest{Tmux: "dr-1", Source: "http://host-a:8765"}) {
		t.Fatalf("attach = %+v", st.attach)
	}

	// A paste that holds an "a" never attaches.
	st = fleetTestState(fleetAttachView(), now)
	if cmd := st.handleKey("a", true, now); cmd != fleetCmdNone || st.attach != nil {
		t.Fatalf("pasted a: cmd %v attach %+v", cmd, st.attach)
	}

	st = fleetTestState(fleetAttachView(), now)
	st.handleKey(KeyDown, false, now)
	if cmd := st.handleKey("a", false, now); cmd != fleetCmdRender || st.attach != nil {
		t.Fatalf("two sessions: cmd %v attach %+v", cmd, st.attach)
	}
	if st.toastKind != fleetToastWarn || st.toast != "2 sessions for DR-2" {
		t.Fatalf("toast = %q %v", st.toast, st.toastKind)
	}

	st = fleetTestState(fleetAttachView(), now)
	st.handleKey(KeyDown, false, now)
	st.handleKey(KeyDown, false, now)
	if cmd := st.handleKey("a", false, now); cmd != fleetCmdNone || st.attach != nil {
		t.Fatalf("no session: cmd %v attach %+v", cmd, st.attach)
	}

	// The server's list failed: nothing to attach to.
	v := fleetAttachView()
	v.Sessions = nil
	st = fleetTestState(v, now)
	if cmd := st.handleKey("a", false, now); cmd != fleetCmdNone || st.attach != nil {
		t.Fatalf("null sessions: cmd %v attach %+v", cmd, st.attach)
	}

	// The panel takes "a" as comment text.
	st = fleetTestState(fleetAttachView(), now)
	st.handleKey("1", false, now)
	if cmd := st.handleKey("a", false, now); cmd == fleetCmdAttach || st.attach != nil || st.panel.comment != "a" {
		t.Fatalf("panel: cmd %v attach %+v comment %q", cmd, st.attach, st.panel.comment)
	}
}

func TestFleetViewAttachRefusedWhileBusy(t *testing.T) {
	now := time.Unix(1000, 0)
	for _, busy := range []string{"sending", "dismissing"} {
		st := fleetTestState(fleetAttachView(), now)
		if busy == "sending" {
			st.sending = true
		} else {
			st.dismissing = true
		}
		if cmd := st.handleKey("a", false, now); cmd != fleetCmdRender || st.attach != nil {
			t.Fatalf("%s: cmd %v attach %+v", busy, cmd, st.attach)
		}
		if st.toastKind != fleetToastWarn || st.toast == "" {
			t.Fatalf("%s: toast = %q %v", busy, st.toast, st.toastKind)
		}
	}
}

func TestFleetViewAttachHint(t *testing.T) {
	now := time.Unix(1000, 0)
	st := fleetTestState(fleetAttachView(), now)
	if out := fleetRendered(st, 160, 30, now); !strings.Contains(out, "a attach") {
		t.Fatalf("one live session: no hint in\n%s", out)
	}
	for _, down := range []int{1, 2} {
		st := fleetTestState(fleetAttachView(), now)
		for range down {
			st.handleKey(KeyDown, false, now)
		}
		if out := fleetRendered(st, 160, 30, now); strings.Contains(out, "a attach") {
			t.Fatalf("card %d: hint shown in\n%s", down, out)
		}
	}
	v := fleetAttachView()
	v.Sessions = nil
	if out := fleetRendered(fleetTestState(v, now), 160, 30, now); strings.Contains(out, "a attach") {
		t.Fatalf("null sessions: hint shown in\n%s", out)
	}
}

func TestFleetLoopAttach(t *testing.T) {
	now := time.Unix(1000, 0)
	var got []fleetAttachRequest
	states := 0
	l := &fleetLoop{
		st:    fleetTestState(fleetAttachView(), now),
		state: func() (fleetStateView, error) { states++; return fleetAttachView(), nil },
		attach: func(r fleetAttachRequest) error {
			got = append(got, r)
			return nil
		},
		interval: time.Hour,
	}
	defer l.closeAll()
	if l.run(l.st.handleKey("a", false, now), now) {
		t.Fatal("attach closed the view")
	}
	if len(got) != 1 || got[0].Tmux != "dr-1" || l.st.attach != nil {
		t.Fatalf("attach calls = %+v, pending %+v", got, l.st.attach)
	}
	if len(l.states) != 1 {
		t.Fatalf("state fetches after attach = %d, want 1 refetch", len(l.states))
	}
	waitFleetJobs(t, l)
	l.reap(now)

	// FLEET moved: the warn toast, and a refetch.
	l.attach = func(fleetAttachRequest) error { return errFleetMoved }
	l.run(l.st.handleKey("a", false, now), now)
	if l.st.toastKind != fleetToastWarn || !strings.Contains(l.st.toast, "moved") || len(l.states) != 1 {
		t.Fatalf("moved: toast %q %v, fetches %d", l.st.toast, l.st.toastKind, len(l.states))
	}
	waitFleetJobs(t, l)
	l.reap(now)

	// A failed attach says so, cleaned for the terminal.
	l.attach = func(fleetAttachRequest) error { return errors.New("exit status 1\x1b[31m") }
	l.run(l.st.handleKey("a", false, now), now)
	if l.st.toastKind != fleetToastErr || l.st.toast != "attach failed: exit status 1" {
		t.Fatalf("failed: toast %q %v", l.st.toast, l.st.toastKind)
	}
	waitFleetJobs(t, l)
	l.reap(now)
}

func TestDecodeFleetStateViewSessions(t *testing.T) {
	body := `{"fleet":{"alive":true},"cards":[{"id":"a","kind":"needs-you","session":"s\u001b[31m:0.0","ticket":"DR-\u001b[0m1"}],` +
		`"sessions":[5,{"tmux":"dr-1","tickets":["DR-1",7]},{"tickets":["DR-2"]},{"tmux":"dr-3","tickets":"x"}]}`
	v, err := decodeFleetStateView([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	want := []fleetWorkSession{{Tmux: "dr-1", Tickets: []string{"DR-1"}}, {Tmux: "dr-3"}}
	if !reflect.DeepEqual(v.Sessions, want) {
		t.Fatalf("sessions = %+v, want %+v", v.Sessions, want)
	}
	c := v.Cards[0]
	if c.rawSession != "s\x1b[31m:0.0" || c.rawTicket != "DR-\x1b[0m1" {
		t.Fatalf("raw = %q %q", c.rawSession, c.rawTicket)
	}
	assertNoEscapes(t, "session", c.Session)
	assertNoEscapes(t, "ticket", c.Ticket)

	for _, body := range []string{`{"sessions":null}`, `{}`, `{"sessions":{}}`} {
		v, err := decodeFleetStateView([]byte(body))
		if err != nil || v.Sessions != nil {
			t.Fatalf("%s: sessions = %#v, %v; want nil", body, v.Sessions, err)
		}
	}
	if v, _ := decodeFleetStateView([]byte(`{"sessions":[]}`)); v.Sessions == nil || len(v.Sessions) != 0 {
		t.Fatalf("empty list = %#v, want empty non-nil", v.Sessions)
	}
}
