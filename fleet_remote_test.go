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
// answers /fleet/state with state (or status when non-zero) after delay, and
// /fleet/say with ok. It counts both.
type fleetTestHost struct {
	srv    *httptest.Server
	cfg    ServerConfig
	states atomic.Int32
	says   atomic.Int32
}

func newFleetTestHost(t *testing.T, name string, alive bool, status int, delay time.Duration) *fleetTestHost {
	t.Helper()
	h := &fleetTestHost{}
	state := `{"fleet":{"alive":` + strconv.FormatBool(alive) + `},` +
		`"cards":[{"id":"a","kind":"needs-you","options":[{"key":"yes","label":"Yes","send":"yes"}]}],` +
		`"cardHashes":{"a":"h-a"},"answered":{}}`
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok-"+name {
			http.Error(w, "bad", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/fleet/state":
			h.states.Add(1)
			time.Sleep(delay)
			if status != 0 {
				http.Error(w, "no", status)
				return
			}
			io.WriteString(w, state)
		case "/fleet/say":
			h.says.Add(1)
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
	if err != nil || !v.Fleet.Alive || v.Host != "ws" {
		t.Fatalf("State = %+v, %v", v, err)
	}
	r := fleetSendVia(src)(fleetSayRequest{Text: "yes", CardID: "a", CardHash: "h-a", OptionKey: "yes"})
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
	// Local down: the first remote that answered.
	v, err = fleetTestSource(nil, ws).State()
	if err != nil || v.Fleet.Alive || v.Host != "ws" {
		t.Fatalf("local down: %+v, %v", v, err)
	}
}

func TestFleetDiscoveryIsParallel(t *testing.T) {
	a := newFleetTestHost(t, "a", false, 0, 300*time.Millisecond)
	b := newFleetTestHost(t, "b", true, 0, 300*time.Millisecond)
	src := fleetTestSource(nil, a, b)
	start := time.Now()
	v, err := src.State()
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("discovery took %v; remotes must be asked in parallel", d)
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
