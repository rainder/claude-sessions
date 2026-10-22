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
	"time"
	"unicode/utf8"
)

// The TUI's view of the local service's FLEET routes. It talks to the same
// server the page does, with the bearer token and never a cookie, so every
// guard in fleet_say.go applies to a TUI answer as it does to a page one.

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
	v.sanitize()
	return v, nil
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
		c.UpdatedAt = fleetDisplayText(c.UpdatedAt)
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

// fleetLocal keeps one connection to the local service for the TUI and
// reconnects after the service went away.
type fleetLocal struct {
	mu      sync.Mutex
	c       *fleetClient
	connect func() (*fleetClient, fleetStateView, error) // nil → connectLocalFleet
}

func (l *fleetLocal) dial() (*fleetClient, fleetStateView, error) {
	if l.connect != nil {
		return l.connect()
	}
	return connectLocalFleet()
}

// State reads the state, connecting first when there is no client. It forgets
// the client only after an error that carried no HTTP status: an answer of any
// status proves the service is there.
func (l *fleetLocal) State() (fleetStateView, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
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

// Client returns the connected client, connecting when needed.
func (l *fleetLocal) Client() (*fleetClient, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.c == nil {
		c, _, err := l.dial()
		if err != nil {
			return nil, err
		}
		l.c = c
	}
	return l.c, nil
}
