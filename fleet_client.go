package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// The TUI's view of FLEET's routes, on this host's service or on the one
// remote host that runs FLEET. It talks to the same server the page does,
// with the bearer token and never a cookie, so every guard in fleet_say.go
// applies to a TUI answer as it does to a page one.

var (
	fleetStateTimeout = 5 * time.Second
	fleetSayTimeout   = 15 * time.Second
	// fleetLocalConfig names the service connectLocalFleet asks. A seam so
	// tests can point it at an httptest server.
	fleetLocalConfig = localServiceConfig
)

const (
	fleetStateBodyMax = 2 << 20
	fleetSayBodyMax   = 64 << 10
	fleetBodyTextMax  = 300
)

var errFleetUnauthorized = errors.New("service token rejected")

// fleetStatusError is a non-2xx answer: the service replied, so the
// connection itself is good. A 401 matches errFleetUnauthorized.
type fleetStatusError struct{ Status int }

func (e *fleetStatusError) Error() string { return fmt.Sprintf("HTTP %d", e.Status) }

func (e *fleetStatusError) Is(target error) bool {
	return target == errFleetUnauthorized && e.Status == http.StatusUnauthorized
}

// fleetErrorStatus is the HTTP status an error carried, 0 when none did.
func fleetErrorStatus(err error) int {
	var se *fleetStatusError
	if errors.As(err, &se) {
		return se.Status
	}
	return 0
}

// fleetSGR matches a complete SGR sequence, the one kind sanitizeTerminalText
// keeps (its body may also hold ':').
var fleetSGR = regexp.MustCompile("\x1b\\[[0-9;:]*m")

// fleetDisplayText makes text FLEET wrote safe to draw in the TUI. State
// text is written by an LLM from ticket titles strangers control, so an ESC,
// OSC or CSI sequence in it must never reach Andy's terminal.
// sanitizeTerminalText drops C0/C1/OSC/DCS and non-SGR CSI and expands tabs;
// the SGR sequences it keeps are removed too, so no ESC survives and display
// widths stay right. Newlines are kept.
func fleetDisplayText(s string) string {
	s = fleetSGR.ReplaceAllString(sanitizeTerminalText(s), "")
	return strings.ReplaceAll(s, "\x1b", "")
}

// fleetStateView is /fleet/state as the TUI reads it. It decodes as
// leniently as the page: a key of the wrong type is empty, and a card that
// is not an object is skipped.
type fleetStateView struct {
	Fleet       fleetSession
	Stale       bool
	StateFileAt string // null → ""
	StateError  string // null → ""
	Cards       []fleetCardView
	CardHashes  map[string]string
	Answered    map[string]fleetAnswer
	// Sessions is the server's working list, raw and never drawn: only what
	// the attach key needs. nil when the server sent null (its session list
	// failed) or no list at all, which allows no attach.
	Sessions []fleetWorkSession

	// Host is the servers.yaml name of the remote host this state came from,
	// "" for this host's service. Set by fleetLocal, never decoded, and
	// already passed through fleetHostName.
	Host string
	// Ambiguous names the hosts that each run a live FLEET when there is more
	// than one. The rest of the view is then empty, and nothing is sent.
	Ambiguous []string
	// Source is the base URL of the service this state came from. Set by
	// fleetLocal, never decoded, never drawn: a send or dismiss carries it,
	// and is refused when FLEET has moved to another host since.
	Source string
}

// decodeFleetStateView fails only when data is not a JSON object.
func decodeFleetStateView(data []byte) (fleetStateView, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil || m == nil {
		return fleetStateView{}, errors.New("state is not a JSON object")
	}
	var v fleetStateView
	_ = json.Unmarshal(m["fleet"], &v.Fleet)
	_ = json.Unmarshal(m["stale"], &v.Stale)
	_ = json.Unmarshal(m["stateFileAt"], &v.StateFileAt)
	_ = json.Unmarshal(m["stateError"], &v.StateError)
	var cards []json.RawMessage
	if json.Unmarshal(m["cards"], &cards) == nil {
		for _, raw := range cards {
			var c fleetCardView
			if json.Unmarshal(raw, &c) == nil {
				v.Cards = append(v.Cards, c)
			}
		}
	}
	var hashes map[string]json.RawMessage
	if json.Unmarshal(m["cardHashes"], &hashes) == nil {
		v.CardHashes = map[string]string{}
		for id, raw := range hashes {
			var h string
			if json.Unmarshal(raw, &h) == nil {
				v.CardHashes[id] = h
			}
		}
	}
	var answered map[string]json.RawMessage
	if json.Unmarshal(m["answered"], &answered) == nil {
		v.Answered = map[string]fleetAnswer{}
		for id, raw := range answered {
			var a fleetAnswer
			if json.Unmarshal(raw, &a) == nil {
				v.Answered[id] = a
			}
		}
	}
	v.Sessions = decodeFleetWorkSessions(m["sessions"])
	v.sanitize()
	return v, nil
}

// decodeFleetWorkSessions reads /fleet/state's "sessions" as leniently as
// the cards: a row that is not an object or has no string tmux is skipped,
// and a ticket that is not a string is dropped. Not an array (null, absent,
// anything else) is nil.
func decodeFleetWorkSessions(data json.RawMessage) []fleetWorkSession {
	var rows []json.RawMessage
	if json.Unmarshal(data, &rows) != nil || rows == nil {
		return nil
	}
	out := []fleetWorkSession{}
	for _, raw := range rows {
		var m map[string]json.RawMessage
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		var row fleetWorkSession
		if json.Unmarshal(m["tmux"], &row.Tmux) != nil || row.Tmux == "" {
			continue
		}
		var tickets []json.RawMessage
		if json.Unmarshal(m["tickets"], &tickets) == nil {
			for _, t := range tickets {
				var s string
				if json.Unmarshal(t, &s) == nil {
					row.Tickets = append(row.Tickets, s)
				}
			}
		}
		out = append(out, row)
	}
	return out
}

// sanitize runs fleetDisplayText over every string the TUI draws. Card ids,
// card hashes and an option's Send are identity or text for the server, and
// stay as received.
func (v *fleetStateView) sanitize() {
	f := &v.Fleet
	f.SessionID, f.Status, f.Tool = fleetDisplayText(f.SessionID), fleetDisplayText(f.Status), fleetDisplayText(f.Tool)
	f.Reason, f.Code = fleetDisplayText(f.Reason), fleetDisplayText(f.Code)
	f.PendingTmux = fleetDisplayText(f.PendingTmux)
	if f.Starting != nil {
		st := *f.Starting
		st.At, st.Mode = fleetDisplayText(st.At), fleetDisplayText(st.Mode)
		f.Starting = &st
	}
	v.StateFileAt = fleetDisplayText(v.StateFileAt)
	v.StateError = fleetDisplayText(v.StateError)
	for i := range v.Cards {
		c := &v.Cards[i]
		c.Status, c.Ticket, c.Title = fleetDisplayText(c.Status), fleetDisplayText(c.Ticket), fleetDisplayText(c.Title)
		c.Summary, c.Lean = fleetDisplayText(c.Summary), fleetDisplayText(c.Lean)
		c.WaitingOn, c.Session = fleetDisplayText(c.WaitingOn), fleetDisplayText(c.Session)
		c.UpdatedAt, c.FirstSeenAt = fleetDisplayText(c.UpdatedAt), fleetDisplayText(c.FirstSeenAt)
		for j := range c.Options {
			o := &c.Options[j]
			o.Key, o.Label, o.Line = fleetDisplayText(o.Key), fleetDisplayText(o.Label), fleetDisplayText(o.Line)
		}
	}
	for id, a := range v.Answered {
		attempts := make([]fleetAttempt, len(a.Attempts))
		for i, at := range a.Attempts {
			at.OptionKey, at.Label = fleetDisplayText(at.OptionKey), fleetDisplayText(at.Label)
			at.At, at.Status = fleetDisplayText(at.At), fleetDisplayText(at.Status)
			attempts[i] = at
		}
		a.Attempts = attempts
		v.Answered[id] = a
	}
}

type fleetClient struct{ base, token string } // base "http://host:port"

func newFleetClient(base, token string) *fleetClient {
	return &fleetClient{base: strings.TrimRight(base, "/"), token: token}
}

func (c *fleetClient) newRequest(ctx context.Context, method, path string, body []byte) (*http.Request, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// getState is one GET /fleet/state. status is the HTTP status when a
// response arrived, 0 when none did; a non-200 answer is a *fleetStatusError.
func (c *fleetClient) getState(ctx context.Context) (fleetStateView, int, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/fleet/state", nil)
	if err != nil {
		return fleetStateView{}, 0, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fleetStateView{}, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fleetStateView{}, resp.StatusCode, &fleetStatusError{Status: resp.StatusCode}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, fleetStateBodyMax))
	if err != nil {
		return fleetStateView{}, resp.StatusCode, err
	}
	v, err := decodeFleetStateView(data)
	return v, resp.StatusCode, err
}

func (c *fleetClient) GetState() (fleetStateView, error) {
	ctx, cancel := context.WithTimeout(context.Background(), fleetStateTimeout)
	defer cancel()
	v, _, err := c.getState(ctx)
	return v, err
}

type fleetSayRequest struct {
	// Source is the fleetStateView.Source the answer was prepared against.
	// It pins the send to that host and never goes on the wire.
	Source     string `json:"-"`
	Text       string `json:"text"`
	CardID     string `json:"cardId"`
	CardHash   string `json:"cardHash,omitempty"`
	OptionKey  string `json:"optionKey,omitempty"`
	Correction bool   `json:"correction,omitempty"`
}

type fleetSayOutcome struct {
	Status  int            // 0 when no response arrived
	Result  fleetSayResult // when the body was JSON
	Body    string         // trimmed body text, ≤300 bytes
	Unclear bool           // no response, timeout, or a 2xx without a boolean ok
}

// fleetBodyText trims a body and cuts it to fleetBodyTextMax bytes on a rune
// boundary.
func fleetBodyText(b []byte) string {
	s := strings.TrimSpace(fleetDisplayText(string(b)))
	if len(s) > fleetBodyTextMax {
		s = s[:fleetBodyTextMax]
		for len(s) > 0 && !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
	}
	return s
}

// Say posts one answer. A send that may have reached FLEET but whose result
// the TUI could not read is Unclear, so the caller never tells the user it
// did not go.
func (c *fleetClient) Say(req fleetSayRequest) fleetSayOutcome {
	body, err := json.Marshal(req)
	if err != nil {
		return fleetSayOutcome{Unclear: true}
	}
	ctx, cancel := context.WithTimeout(context.Background(), fleetSayTimeout)
	defer cancel()
	hr, err := c.newRequest(ctx, http.MethodPost, "/fleet/say", body)
	if err != nil {
		return fleetSayOutcome{Unclear: true}
	}
	resp, err := http.DefaultClient.Do(hr)
	if err != nil {
		return fleetSayOutcome{Unclear: true}
	}
	defer resp.Body.Close()
	out := fleetSayOutcome{Status: resp.StatusCode}
	data, err := io.ReadAll(io.LimitReader(resp.Body, fleetSayBodyMax))
	if err != nil {
		out.Unclear = true
		return out
	}
	out.Body = fleetBodyText(data)
	ok := fleetDecodeSayResult(data, &out.Result)
	if resp.StatusCode/100 == 2 && !ok {
		out.Unclear = true
	}
	return out
}

// fleetDecodeSayResult decodes a say answer and reports whether it was a JSON
// object with a boolean ok.
func fleetDecodeSayResult(data []byte, into *fleetSayResult) bool {
	var m map[string]json.RawMessage
	if json.Unmarshal(data, &m) != nil {
		return false
	}
	var ok bool
	if json.Unmarshal(m["ok"], &ok) != nil {
		return false
	}
	if json.Unmarshal(data, into) != nil {
		return false
	}
	into.Error, into.Code = fleetDisplayText(into.Error), fleetDisplayText(into.Code)
	return true
}

// Dismiss asks the server to drop a card. An error means the service did not
// answer with a result; a refusal is Result.OK false with a Code.
func (c *fleetClient) Dismiss(cardID string) (fleetSayResult, error) {
	body, err := json.Marshal(struct {
		CardID string `json:"cardId"`
	}{cardID})
	if err != nil {
		return fleetSayResult{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), fleetStateTimeout)
	defer cancel()
	hr, err := c.newRequest(ctx, http.MethodPost, "/fleet/card/dismiss", body)
	if err != nil {
		return fleetSayResult{}, err
	}
	resp, err := http.DefaultClient.Do(hr)
	if err != nil {
		return fleetSayResult{}, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, fleetSayBodyMax))
	if err != nil {
		return fleetSayResult{}, err
	}
	var res fleetSayResult
	if fleetDecodeSayResult(data, &res) {
		return res, nil
	}
	if resp.StatusCode != http.StatusOK {
		return fleetSayResult{}, &fleetStatusError{Status: resp.StatusCode}
	}
	return fleetSayResult{}, errors.New("bad dismiss answer")
}

// connectLocalFleet finds this host's service (loopback, then Tailscale) and
// reads the first state from it.
func connectLocalFleet() (*fleetClient, fleetStateView, error) {
	cfg, err := fleetLocalConfig()
	if err != nil {
		return nil, fleetStateView{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), fleetStateTimeout)
	defer cancel()
	var view fleetStateView
	srv, _, err := tryLocalService(ctx, cfg, localTailscaleIPv4,
		func(ctx context.Context, srv ServerConfig) (int, error) {
			c := newFleetClient("http://"+hostPort(srv.Host, srv.Port), srv.Token)
			var status int
			var err error
			view, status, err = c.getState(ctx)
			return status, err
		})
	if err != nil {
		return nil, fleetStateView{}, err
	}
	return newFleetClient("http://"+hostPort(srv.Host, srv.Port), srv.Token), view, nil
}

// fleetHostNameMax caps a host name on the badge and the view header.
const fleetHostNameMax = 24

// fleetHostName is a servers.yaml name made safe to draw: user config, but
// every string this code puts on the terminal goes through fleetDisplayText.
// Whitespace collapses so a name never breaks the header line.
func fleetHostName(name string) string {
	return truncateRunes(strings.Join(strings.Fields(fleetDisplayText(name)), " "), fleetHostNameMax)
}

// fleetRemoteServers is the production remote list: servers.yaml without
// the entry that is this host. An unreadable file is no remotes.
func fleetRemoteServers() []ServerConfig {
	cfgs, err := LoadServerConfigs()
	if err != nil {
		return nil
	}
	return dropSelfServer(cfgs)
}

// fleetRemoteBase is the base URL a remote is asked at; it is also the
// remembered host's key.
func fleetRemoteBase(srv ServerConfig) string {
	return "http://" + hostPort(srv.Host, srv.Port)
}

// fleetRemoteName is a remote's display name: its config name, else its host.
func fleetRemoteName(srv ServerConfig) string {
	if strings.TrimSpace(srv.Name) == "" {
		return fleetHostName(srv.Host)
	}
	return fleetHostName(srv.Name)
}

// fleetRemoteAnswer is one remote host's 200 answer to /fleet/state.
type fleetRemoteAnswer struct {
	name string // fleetHostName of the config name
	srv  ServerConfig
	c    *fleetClient
	v    fleetStateView
}

// fleetAskRemotes asks every remote with a host and a token for its state,
// all at once, so the whole fan-out takes one fleetStateTimeout at most. Only
// 200 answers come back, in config order: any other status (404 from an older
// server, 401, 5xx) or a transport error means that host has no FLEET.
func fleetAskRemotes(cfgs []ServerConfig) []fleetRemoteAnswer {
	answers := make([]*fleetRemoteAnswer, len(cfgs))
	var wg sync.WaitGroup
	for i, srv := range cfgs {
		if srv.Host == "" || srv.Token == "" {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := newFleetClient(fleetRemoteBase(srv), srv.Token)
			v, err := c.GetState()
			if err != nil {
				return
			}
			answers[i] = &fleetRemoteAnswer{name: fleetRemoteName(srv), srv: srv, c: c, v: v}
		}()
	}
	wg.Wait()
	var out []fleetRemoteAnswer
	for _, a := range answers {
		if a != nil {
			out = append(out, *a)
		}
	}
	return out
}

// fleetHostDownError is the remembered FLEET host not answering. The view
// shows it instead of falling back to another host's view: one failed poll
// must not close an open panel or swap the cards under Andy.
type fleetHostDownError struct{ host string }

func (e *fleetHostDownError) Error() string {
	return "FLEET host " + e.host + " is not reachable"
}

// errFleetMoved refuses a send or dismiss prepared against a state from
// another host than the one FLEET is on now: a dismiss carries only a card
// id, so it could clear another host's answer record.
var errFleetMoved = errors.New("FLEET moved to another host — refresh")

// fleetAmbiguousError is the refusal Client gives while more than one host
// runs a live FLEET: nothing says which one Andy means.
type fleetAmbiguousError struct{ hosts []string }

func (e *fleetAmbiguousError) Error() string {
	return fleetAmbiguousText(e.hosts)
}

// fleetAmbiguousText is the one-line warning for two or more live FLEETs.
func fleetAmbiguousText(hosts []string) string {
	list := strings.Join(hosts, ", ")
	if len(hosts) == 2 {
		return "Two FLEETs are live (" + list + "). Stop one."
	}
	return fmt.Sprintf("%d FLEETs are live (%s). Stop all but one.", len(hosts), list)
}

// fleetLocal finds FLEET for the TUI: this host's service first, then the
// remote hosts. It keeps one connection to the local service, reconnects
// after the service went away, and remembers the client of the host it chose
// for say and dismiss.
type fleetLocal struct {
	mu      sync.Mutex
	c       *fleetClient                                 // this host's service
	connect func() (*fleetClient, fleetStateView, error) // nil → connectLocalFleet
	// remotes lists the hosts to ask when FLEET is not live here. nil asks
	// none, so no test reaches the real servers.yaml; the TUI sets
	// fleetRemoteServers.
	remotes func() []ServerConfig

	chosen *fleetClient // the host say and dismiss use; nil when none or ambiguous
	// sticky is the last remote chosen for a live FLEET (keyed by its base
	// URL), stickyName its display name. While no FLEET is live anywhere it
	// stays the answer, so a failed poll does not swap in the local view. A
	// live local FLEET, or the host leaving servers.yaml, clears it.
	sticky     *fleetClient
	stickyName string
	stickySrv  ServerConfig

	// host mirrors chosen for the attach key, which runs on the view's loop
	// and so must not wait on mu (a state job holds it for up to 10 s). nil
	// when chosen is nil.
	host atomic.Pointer[fleetHostRef]
}

// fleetHostRef is the host chosen for say, dismiss and attach: its base URL
// and, for a remote, its servers.yaml entry (ssh target and paste relay).
type fleetHostRef struct {
	base string
	srv  *ServerConfig // nil: this host's service
}

// setChosenLocked sets chosen and its lock-free mirror. srv is nil for this
// host's service.
func (l *fleetLocal) setChosenLocked(c *fleetClient, srv *ServerConfig) {
	l.chosen = c
	if c == nil {
		l.host.Store(nil)
		return
	}
	ref := &fleetHostRef{base: c.base}
	if srv != nil {
		cp := *srv
		ref.srv = &cp
	}
	l.host.Store(ref)
}

// chosenHost is the host say and dismiss would use now, read without mu.
func (l *fleetLocal) chosenHost() (fleetHostRef, bool) {
	if p := l.host.Load(); p != nil {
		return *p, true
	}
	return fleetHostRef{}, false
}

func (l *fleetLocal) dial() (*fleetClient, fleetStateView, error) {
	if l.connect != nil {
		return l.connect()
	}
	return connectLocalFleet()
}

// localStateLocked reads the local service's state, connecting first when
// there is no client. It forgets the client only after an error that carried
// no HTTP status: an answer of any status proves the service is there.
func (l *fleetLocal) localStateLocked() (fleetStateView, error) {
	if l.c == nil {
		c, v, err := l.dial()
		if err != nil {
			return fleetStateView{}, err
		}
		l.c = c
		return v, nil
	}
	v, err := l.c.GetState()
	if err != nil && fleetErrorStatus(err) == 0 {
		l.c = nil
	}
	return v, err
}

// discoverLocked finds FLEET and sets chosen. A live local FLEET wins
// outright, no remote is asked, and the remembered remote is forgotten.
// Otherwise every remote is asked at once: exactly one live FLEET is used and
// remembered; two or more is an Ambiguous view and no client, because a guess
// could answer the wrong FLEET. With none live, a remembered remote is still
// the answer: its own view when it answered, else a fleetHostDownError with
// chosen kept on it. With no remote ever chosen, the local view when the
// service answered (it shows "not running"), else a not-running view with no
// cards when some remote answered, else the local error.
func (l *fleetLocal) discoverLocked() (fleetStateView, error) {
	lv, lerr := l.localStateLocked()
	if lerr == nil && lv.Fleet.Alive {
		l.setChosenLocked(l.c, nil)
		l.sticky, l.stickyName, l.stickySrv = nil, "", ServerConfig{}
		lv.Source = l.c.base
		return lv, nil
	}
	var cfgs []ServerConfig
	if l.remotes != nil {
		cfgs = l.remotes()
	}
	answers := fleetAskRemotes(cfgs)
	var live []fleetRemoteAnswer
	for _, a := range answers {
		if a.v.Fleet.Alive {
			live = append(live, a)
		}
	}
	pick := func(a fleetRemoteAnswer) (fleetStateView, error) {
		l.setChosenLocked(a.c, &a.srv)
		a.v.Host, a.v.Source = a.name, a.c.base
		return a.v, nil
	}
	if l.sticky != nil {
		known := false
		for _, srv := range cfgs {
			if srv.Host != "" && srv.Token != "" && fleetRemoteBase(srv) == l.sticky.base {
				known, l.stickyName, l.stickySrv = true, fleetRemoteName(srv), srv
			}
		}
		if !known {
			l.sticky, l.stickyName, l.stickySrv = nil, "", ServerConfig{}
		}
	}
	switch {
	case len(live) == 1:
		l.sticky, l.stickyName, l.stickySrv = live[0].c, live[0].name, live[0].srv
		return pick(live[0])
	case len(live) > 1:
		l.setChosenLocked(nil, nil)
		hosts := make([]string, len(live))
		for i, a := range live {
			hosts[i] = a.name
		}
		return fleetStateView{Ambiguous: hosts}, nil
	case l.sticky != nil:
		for _, a := range answers {
			if a.c.base == l.sticky.base {
				return pick(a)
			}
		}
		l.setChosenLocked(l.sticky, &l.stickySrv)
		return fleetStateView{}, &fleetHostDownError{host: l.stickyName}
	case lerr == nil:
		l.setChosenLocked(l.c, nil)
		lv.Source = l.c.base
		return lv, nil
	case len(answers) > 0:
		l.setChosenLocked(nil, nil)
		f := answers[0].v.Fleet
		f.Alive = false
		return fleetStateView{Fleet: f}, nil
	}
	l.setChosenLocked(nil, nil)
	return fleetStateView{}, lerr
}

// State runs discovery and returns the chosen host's state. The lock is held
// across the local call and the parallel fan-out: up to two timeouts.
func (l *fleetLocal) State() (fleetStateView, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.discoverLocked()
}

// Client returns the chosen host's client, running discovery when none is
// remembered. While FLEET is ambiguous it returns an error, so a send goes
// nowhere.
func (l *fleetLocal) Client() (*fleetClient, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.chosen == nil {
		v, err := l.discoverLocked()
		if err != nil {
			return nil, err
		}
		if len(v.Ambiguous) > 0 {
			return nil, &fleetAmbiguousError{hosts: v.Ambiguous}
		}
		if l.chosen == nil {
			return nil, errors.New("no FLEET service")
		}
	}
	return l.chosen, nil
}
