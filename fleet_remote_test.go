package main

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fleetTestHost is one httptest service for the remote discovery tests: it
// answers /fleet/state with its state (or status when non-zero) after delay,
// and /fleet/say and /fleet/card/dismiss with ok. It counts all three. alive,
// status and delay can change between polls; gate, when set, runs before a
// state answer.
type fleetTestHost struct {
	srv      *httptest.Server
	cfg      ServerConfig
	alive    atomic.Bool
	status   atomic.Int32
	delay    atomic.Int64
	gate     atomic.Pointer[func()]
	states   atomic.Int32
	says     atomic.Int32
	dismisss atomic.Int32
}

func newFleetTestHost(t *testing.T, name string, alive bool, status int, delay time.Duration) *fleetTestHost {
	t.Helper()
	h := &fleetTestHost{}
	h.alive.Store(alive)
	h.status.Store(int32(status))
	h.delay.Store(int64(delay))
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok-"+name {
			http.Error(w, "bad", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/fleet/state":
			h.states.Add(1)
			if g := h.gate.Load(); g != nil {
				(*g)()
			}
			time.Sleep(time.Duration(h.delay.Load()))
			if st := h.status.Load(); st != 0 {
				http.Error(w, "no", int(st))
				return
			}
			io.WriteString(w, `{"fleet":{"alive":`+strconv.FormatBool(h.alive.Load())+`},`+
				`"cards":[{"id":"a","kind":"needs-you","options":[{"key":"yes","label":"Yes","send":"yes"}]}],`+
				`"cardHashes":{"a":"h-a"},"answered":{}}`)
		case "/fleet/say":
			h.says.Add(1)
			io.WriteString(w, `{"ok":true}`)
		case "/fleet/card/dismiss":
			h.dismisss.Add(1)
			io.WriteString(w, `{"ok":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(h.srv.Close)
	u, _ := url.Parse(h.srv.URL)
	port, _ := strconv.Atoi(u.Port())
	h.cfg = ServerConfig{Name: name, Host: u.Hostname(), Port: port, Token: "tok-" + name}
	return h
}

// fleetTestSource is a fleetLocal whose local service is local (nil: none
// answers) and whose remotes are hosts.
func fleetTestSource(local *fleetTestHost, hosts ...*fleetTestHost) *fleetLocal {
	var cfgs []ServerConfig
	for _, h := range hosts {
		cfgs = append(cfgs, h.cfg)
	}
	return &fleetLocal{
		connect: func() (*fleetClient, fleetStateView, error) {
			if local == nil {
				return nil, fleetStateView{}, errors.New("dial tcp: connection refused")
			}
			c := newFleetClient(local.srv.URL, local.cfg.Token)
			v, err := c.GetState()
			if err != nil {
				return nil, fleetStateView{}, err
			}
			return c, v, nil
		},
		remotes: func() []ServerConfig { return cfgs },
	}
}

func TestFleetDiscoveryLocalWins(t *testing.T) {
	local := newFleetTestHost(t, "local", true, 0, 0)
	ws := newFleetTestHost(t, "ws", true, 0, 0)
	src := fleetTestSource(local, ws)
	for range 2 {
		v, err := src.State()
		if err != nil || !v.Fleet.Alive || v.Host != "" || len(v.Ambiguous) != 0 {
			t.Fatalf("State = %+v, %v", v, err)
		}
	}
	if n := ws.states.Load(); n != 0 {
		t.Fatalf("remote asked %d times while local FLEET is live", n)
	}
	c, err := src.Client()
	if err != nil || c.base != local.srv.URL {
		t.Fatalf("Client = %v, %v", c, err)
	}
}

func TestFleetDiscoveryOneRemote(t *testing.T) {
	local := newFleetTestHost(t, "local", false, 0, 0)
	ws := newFleetTestHost(t, "ws", true, 0, 0)
	pi := newFleetTestHost(t, "pi", false, 0, 0)
	src := fleetTestSource(local, pi, ws)
	v, err := src.State()
	if err != nil || !v.Fleet.Alive || v.Host != "ws" || v.Source != ws.srv.URL {
		t.Fatalf("State = %+v, %v", v, err)
	}
	r := fleetSendVia(src)(fleetSayRequest{Text: "yes", CardID: "a", CardHash: "h-a", OptionKey: "yes", Source: v.Source})
	if r.CheckErr != nil || r.Stale || !r.Out.Result.OK {
		t.Fatalf("send = %+v", r)
	}
	if ws.says.Load() != 1 || local.says.Load() != 0 || pi.says.Load() != 0 {
		t.Fatalf("says: ws %d local %d pi %d", ws.says.Load(), local.says.Load(), pi.says.Load())
	}
}

func TestFleetDiscoveryAmbiguous(t *testing.T) {
	local := newFleetTestHost(t, "local", false, 0, 0)
	ws := newFleetTestHost(t, "ws", true, 0, 0)
	pi := newFleetTestHost(t, "pi", true, 0, 0)
	src := fleetTestSource(local, ws, pi)
	v, err := src.State()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(v.Ambiguous, ",") != "ws,pi" || v.Fleet.Alive || len(v.Cards) != 0 {
		t.Fatalf("State = %+v", v)
	}
	if got := stripANSI(fleetBadgeText(fleetBadgeSnapFor(v, nil))); got != "FLEET ?" {
		t.Fatalf("badge = %q", got)
	}
	if c, err := src.Client(); c != nil || err == nil {
		t.Fatalf("Client = %v, %v; ambiguous must choose none", c, err)
	}
	r := fleetSendVia(src)(fleetSayRequest{Text: "yes", CardID: "a", CardHash: "h-a", OptionKey: "yes"})
	if r.CheckErr == nil {
		t.Fatalf("send = %+v, want a refusal", r)
	}
	if ws.says.Load()+pi.says.Load()+local.says.Load() != 0 {
		t.Fatal("an ambiguous send reached a service")
	}
}

func TestFleetDiscoverySkipsRefusingRemotes(t *testing.T) {
	old := newFleetTestHost(t, "old", false, http.StatusNotFound, 0)
	bad := newFleetTestHost(t, "bad", false, http.StatusUnauthorized, 0)
	ws := newFleetTestHost(t, "ws", true, 0, 0)
	src := fleetTestSource(nil, old, bad, ws)
	inner := src.remotes
	// An entry without a host or a token is never asked.
	src.remotes = func() []ServerConfig {
		return append(inner(), ServerConfig{Name: "nohost", Token: "x", Port: 1},
			ServerConfig{Name: "notoken", Host: "127.0.0.1", Port: 1})
	}
	v, err := src.State()
	if err != nil || !v.Fleet.Alive || v.Host != "ws" {
		t.Fatalf("State = %+v, %v", v, err)
	}
	if old.states.Load() != 1 || bad.states.Load() != 1 {
		t.Fatalf("asked old %d bad %d times, want 1 each", old.states.Load(), bad.states.Load())
	}
	c, err := src.Client()
	if err != nil || c.base != ws.srv.URL {
		t.Fatalf("Client = %v, %v", c, err)
	}
}

func TestFleetDiscoveryNothingAnswers(t *testing.T) {
	old := newFleetTestHost(t, "old", false, http.StatusNotFound, 0)
	src := fleetTestSource(nil, old)
	v, err := src.State()
	if err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("State = %+v, %v; want the local error", v, err)
	}
	if got := fleetBadgeText(fleetBadgeSnapFor(v, err)); got != "" {
		t.Fatalf("badge = %q, want empty", got)
	}
	if c, err := src.Client(); c != nil || err == nil {
		t.Fatalf("Client = %v, %v", c, err)
	}
}

func TestFleetDiscoveryNoLiveFleet(t *testing.T) {
	// Local answered: its view (FLEET not running) is the answer.
	local := newFleetTestHost(t, "local", false, 0, 0)
	ws := newFleetTestHost(t, "ws", false, 0, 0)
	v, err := fleetTestSource(local, ws).State()
	if err != nil || v.Fleet.Alive || v.Host != "" || len(v.Cards) != 1 {
		t.Fatalf("local answered: %+v, %v", v, err)
	}
	// Local down and no remote ever chosen: not running, and no remote's old
	// cards.
	v, err = fleetTestSource(nil, ws).State()
	if err != nil || v.Fleet.Alive || len(v.Cards) != 0 {
		t.Fatalf("local down: %+v, %v", v, err)
	}
}

func TestFleetDiscoveryIsParallel(t *testing.T) {
	// Each state answer waits until both requests are in flight. Asked one
	// after the other, the first never sees the second and gives up.
	var inFlight atomic.Int32
	var gaveUp atomic.Bool
	both := make(chan struct{})
	gate := func() {
		if inFlight.Add(1) == 2 {
			close(both)
		}
		select {
		case <-both:
		case <-time.After(2 * time.Second):
			gaveUp.Store(true)
		}
	}
	a := newFleetTestHost(t, "a", false, 0, 0)
	b := newFleetTestHost(t, "b", true, 0, 0)
	a.gate.Store(&gate)
	b.gate.Store(&gate)
	v, err := fleetTestSource(nil, a, b).State()
	if gaveUp.Load() {
		t.Fatal("the two remotes were not asked at the same time")
	}
	if err != nil || v.Host != "b" {
		t.Fatalf("State = %+v, %v", v, err)
	}
}

func TestFleetHostNameSanitized(t *testing.T) {
	ws := newFleetTestHost(t, "ws", true, 0, 0)
	ws.cfg.Name = "w\x1b]0;pwn\x07s\x1b[2J" + strings.Repeat("x", 40)
	v, err := fleetTestSource(nil, ws).State()
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsRune(v.Host, 0x1b) || len([]rune(v.Host)) > 24 || !strings.HasPrefix(v.Host, "ws") {
		t.Fatalf("Host = %q", v.Host)
	}
}

func TestFleetBadgeTextRemote(t *testing.T) {
	c := fleetCardCounts{Approve: 1, Decide: 2}
	if got := stripANSI(fleetBadgeText(fleetBadgeSnap{Counts: c, OK: true})); got != "FLEET 1 approve · 2 decide" {
		t.Fatalf("local = %q", got)
	}
	if got := stripANSI(fleetBadgeText(fleetBadgeSnap{Counts: c, OK: true, Host: "ws"})); got != "FLEET@ws 1 approve · 2 decide" {
		t.Fatalf("remote = %q", got)
	}
	if got := stripANSI(fleetBadgeText(fleetBadgeSnap{Ambiguous: true})); got != "FLEET ?" {
		t.Fatalf("ambiguous = %q", got)
	}
	// The hub keeps the host it was given; the source sanitizes it.
	view := fleetBadgeAliveView(fleetCardView{ID: "a", Kind: "needs-you"})
	view.Host = fleetHostName("e\x1b[31mvil")
	got := fleetBadgeText(fleetBadgeSnapFor(view, nil))
	if strings.Contains(got, "\x1b[31m") || stripANSI(got) != "FLEET@evil 0 approve · 1 decide" {
		t.Fatalf("sanitized = %q", got)
	}
}

func TestFleetViewHeaderRemote(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	v := fleetTestView(fleetTestCard("a", "needs-you"))
	v.Host = "ws"
	st := &fleetViewState{}
	st.applyState(v, nil, now)
	out := stripANSI(renderFleetView(st, 100, 20, now))
	if first := strings.SplitN(out, "\n", 2)[0]; !strings.HasPrefix(first, "FLEET@ws ") {
		t.Fatalf("header = %q", first)
	}

	st = &fleetViewState{}
	st.applyState(fleetStateView{Ambiguous: []string{"ws", "pi"}}, nil, now)
	out = stripANSI(renderFleetView(st, 100, 20, now))
	if !strings.Contains(out, "Two FLEETs are live (ws, pi). Stop one.") {
		t.Fatalf("ambiguous view:\n%s", out)
	}
	if strings.Contains(out, "No cards wait") {
		t.Fatalf("ambiguous view shows the card list:\n%s", out)
	}
}

func TestFleetDiscoveryStickyRemote(t *testing.T) {
	local := newFleetTestHost(t, "local", false, 0, 0)
	a := newFleetTestHost(t, "a", true, 0, 0)
	b := newFleetTestHost(t, "b", false, 0, 0)
	src := fleetTestSource(local, a, b)
	if v, err := src.State(); err != nil || v.Host != "a" {
		t.Fatalf("first: %+v, %v", v, err)
	}

	// A fails: an error naming it, never the local view, and sends still go
	// to A (where they fail on their own).
	for _, fail := range []string{"500", "timeout"} {
		switch fail {
		case "500":
			a.status.Store(http.StatusInternalServerError)
		case "timeout":
			prev := fleetStateTimeout
			fleetStateTimeout = 100 * time.Millisecond
			defer func() { fleetStateTimeout = prev }()
			a.status.Store(0)
			a.delay.Store(int64(300 * time.Millisecond))
		}
		v, err := src.State()
		var down *fleetHostDownError
		if !errors.As(err, &down) || !strings.Contains(err.Error(), "a") || v.Host != "" || len(v.Cards) != 0 {
			t.Fatalf("%s: State = %+v, %v; want A not reachable", fail, v, err)
		}
		if c, err := src.Client(); err != nil || c.base != a.srv.URL {
			t.Fatalf("%s: Client = %v, %v; want A kept", fail, c, err)
		}
	}
	a.delay.Store(0)
	fleetStateTimeout = 5 * time.Second

	// A answers again with FLEET stopped: A's own view, not the local one.
	a.alive.Store(false)
	if v, err := src.State(); err != nil || v.Host != "a" || v.Fleet.Alive || v.Source != a.srv.URL {
		t.Fatalf("A stopped: %+v, %v", v, err)
	}
	// A recovers.
	a.alive.Store(true)
	if v, err := src.State(); err != nil || v.Host != "a" || !v.Fleet.Alive {
		t.Fatalf("A recovered: %+v, %v", v, err)
	}

	// Another remote live while A is down: the normal rules.
	a.status.Store(http.StatusInternalServerError)
	b.alive.Store(true)
	if v, err := src.State(); err != nil || v.Host != "b" {
		t.Fatalf("B live: %+v, %v", v, err)
	}
	// B is now the remembered host.
	b.status.Store(http.StatusBadGateway)
	if _, err := src.State(); err == nil || !strings.Contains(err.Error(), "b") {
		t.Fatalf("B down: %v", err)
	}

	// A live local FLEET wins and clears the memory.
	local.alive.Store(true)
	if v, err := src.State(); err != nil || v.Host != "" || !v.Fleet.Alive {
		t.Fatalf("local live: %+v, %v", v, err)
	}
	local.alive.Store(false)
	if v, err := src.State(); err != nil || v.Host != "" || v.Source != local.srv.URL {
		t.Fatalf("after local: %+v, %v; want the local view", v, err)
	}
}

func TestFleetDiscoveryStickyHostLeftConfig(t *testing.T) {
	local := newFleetTestHost(t, "local", false, 0, 0)
	a := newFleetTestHost(t, "a", true, 0, 0)
	src := fleetTestSource(local, a)
	if v, _ := src.State(); v.Host != "a" {
		t.Fatalf("first: %+v", v)
	}
	src.remotes = func() []ServerConfig { return nil }
	if v, err := src.State(); err != nil || v.Source != local.srv.URL {
		t.Fatalf("A removed from servers.yaml: %+v, %v; want the local view", v, err)
	}
}

func TestFleetSendAndDismissPinnedToHost(t *testing.T) {
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
	bStates := b.states.Load()

	r := fleetSendVia(src)(fleetSayRequest{Text: "yes", CardID: "a", CardHash: "h-a", OptionKey: "yes", Source: va.Source})
	if !r.Moved || r.Out.Result.OK {
		t.Fatalf("send = %+v, want Moved", r)
	}
	if _, err := fleetDismissVia(src)("a", va.Source); !errors.Is(err, errFleetMoved) {
		t.Fatalf("dismiss err = %v, want errFleetMoved", err)
	}
	if b.says.Load()+b.dismisss.Load()+a.says.Load()+a.dismisss.Load() != 0 || b.states.Load() != bStates {
		t.Fatalf("a pinned request reached a host: b says %d dismisses %d states +%d",
			b.says.Load(), b.dismisss.Load(), b.states.Load()-bStates)
	}

	// An empty source (no state applied) never sends either.
	if r := fleetSendVia(src)(fleetSayRequest{CardID: "a"}); !r.Moved {
		t.Fatalf("empty source: %+v", r)
	}

	now := time.Unix(1_800_000_000, 0)
	st := &fleetViewState{}
	st.applySend(fleetSendResult{CardID: "a", Moved: true}, now)
	if st.toastKind != fleetToastWarn || !strings.Contains(st.toast, "moved") {
		t.Fatalf("moved toast = %q %v", st.toast, st.toastKind)
	}
	if !st.applySend(fleetSendResult{CardID: "a", Moved: true}, now) {
		t.Fatal("a moved send must refetch")
	}
	if msg, kind := fleetDismissToast(fleetDismissResult{Err: errFleetMoved}); kind != fleetToastWarn || !strings.Contains(msg, "moved") {
		t.Fatalf("moved dismiss toast = %q %v", msg, kind)
	}
}

func TestFleetViewStateCarriesSource(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	v := fleetTestView(fleetTestCard("a", "needs-you"), fleetTestCard("b", "needs-you"))
	v.Answered["b"] = fleetAnswerWith(fleetAttemptUnconfirmed)
	v.Source = "http://host-a:8765"
	st := &fleetViewState{}
	st.applyState(v, nil, now)
	st.handleKey("1", false, now)
	if st.panel == nil {
		t.Fatal("no panel")
	}
	// A later state from another host does not move the open panel's pin.
	v2 := v
	v2.Source = "http://host-b:8765"
	st.applyState(v2, nil, now)
	if st.panel == nil {
		t.Fatal("the panel closed on an unchanged card")
	}
	if cmd := st.handleKey(KeyEnter, false, now.Add(time.Second)); cmd != fleetCmdSend || st.send.Source != v.Source {
		t.Fatalf("send = %v %+v", cmd, st.send)
	}
	st = &fleetViewState{}
	st.applyState(v, nil, now)
	st.sel = fleetItemIndex(st.items, "b")
	if cmd := st.handleKey("x", false, now); cmd != fleetCmdDismiss || st.dismissSource != v.Source {
		t.Fatalf("dismiss = %v %q", cmd, st.dismissSource)
	}
}
