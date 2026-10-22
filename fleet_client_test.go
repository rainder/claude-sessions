package main

import (
	"bytes"
	"context"
	"encoding/json"
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

func TestDecodeFleetStateViewLenient(t *testing.T) {
	body := `{"stateFileAt":null,"stateError":null,"fleet":{"alive":true,"status":"idle"},` +
		`"cards":[5,{"id":"a","kind":"needs-you","options":"x"}],` +
		`"cardHashes":{"a":"h1","b":7},"answered":{"a":{"cardHash":"h1","attempts":[]},"b":3}}`
	v, err := decodeFleetStateView([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Cards) != 1 || v.Cards[0].ID != "a" || v.Cards[0].Options != nil {
		t.Errorf("cards = %+v, want one card a with no options", v.Cards)
	}
	if v.StateFileAt != "" || v.StateError != "" || !v.Fleet.Alive || v.Fleet.Status != "idle" {
		t.Errorf("view = %+v", v)
	}
	if len(v.CardHashes) != 1 || v.CardHashes["a"] != "h1" {
		t.Errorf("cardHashes = %v", v.CardHashes)
	}
	if len(v.Answered) != 1 || v.Answered["a"].CardHash != "h1" {
		t.Errorf("answered = %v", v.Answered)
	}
	if _, err := decodeFleetStateView([]byte(`[]`)); err == nil {
		t.Error("a body that is not an object must be an error")
	}
	if _, err := decodeFleetStateView([]byte(`null`)); err == nil {
		t.Error("null must be an error")
	}
}

func TestFleetClientGetState(t *testing.T) {
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/fleet/state" || r.Header.Get("Authorization") != "Bearer tok" || r.Header.Get("Cookie") != "" {
			t.Errorf("bad request: %s auth=%q cookie=%q", r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Cookie"))
		}
		if status != http.StatusOK {
			http.Error(w, "no", status)
			return
		}
		io.WriteString(w, `{"fleet":{"alive":true},"cards":[]}`)
	}))
	defer srv.Close()
	c := newFleetClient(srv.URL, "tok")
	v, err := c.GetState()
	if err != nil || !v.Fleet.Alive {
		t.Fatalf("GetState = %+v, %v", v, err)
	}
	status = http.StatusUnauthorized
	if _, err := c.GetState(); !errors.Is(err, errFleetUnauthorized) {
		t.Errorf("401 err = %v, want errFleetUnauthorized", err)
	}
	status = http.StatusInternalServerError
	if _, err := c.GetState(); err == nil || errors.Is(err, errFleetUnauthorized) {
		t.Errorf("500 err = %v, want a plain error", err)
	}
}

func TestFleetClientSayOutcomes(t *testing.T) {
	prev := fleetSayTimeout
	fleetSayTimeout = 50 * time.Millisecond
	defer func() { fleetSayTimeout = prev }()

	cases := []struct {
		name    string
		handler func(w http.ResponseWriter)
		check   func(t *testing.T, o fleetSayOutcome)
	}{
		{"queued", func(w http.ResponseWriter) { io.WriteString(w, `{"ok":true,"queued":true}`) },
			func(t *testing.T, o fleetSayOutcome) {
				if o.Status != 200 || !o.Result.OK || !o.Result.Queued || o.Unclear {
					t.Errorf("%+v", o)
				}
			}},
		{"already answered", func(w http.ResponseWriter) { io.WriteString(w, `{"ok":false,"code":"already_answered"}`) },
			func(t *testing.T, o fleetSayOutcome) {
				if o.Status != 200 || o.Result.OK || o.Result.Code != "already_answered" || o.Unclear {
					t.Errorf("%+v", o)
				}
			}},
		{"400 text", func(w http.ResponseWriter) { http.Error(w, "bad text", 400) },
			func(t *testing.T, o fleetSayOutcome) {
				if o.Status != 400 || o.Body != "bad text" || o.Unclear {
					t.Errorf("%+v", o)
				}
			}},
		{"400 json", func(w http.ResponseWriter) {
			w.WriteHeader(400)
			io.WriteString(w, `{"ok":false,"code":"reserved_tag","error":"no"}`)
		}, func(t *testing.T, o fleetSayOutcome) {
			if o.Status != 400 || o.Result.Code != "reserved_tag" || o.Unclear {
				t.Errorf("%+v", o)
			}
		}},
		{"200 not json", func(w http.ResponseWriter) { io.WriteString(w, "nope") },
			func(t *testing.T, o fleetSayOutcome) {
				if o.Status != 200 || !o.Unclear {
					t.Errorf("%+v", o)
				}
			}},
		{"200 no ok", func(w http.ResponseWriter) { io.WriteString(w, `{"x":1}`) },
			func(t *testing.T, o fleetSayOutcome) {
				if !o.Unclear {
					t.Errorf("%+v", o)
				}
			}},
		{"200 ok not boolean", func(w http.ResponseWriter) { io.WriteString(w, `{"ok":"yes"}`) },
			func(t *testing.T, o fleetSayOutcome) {
				if !o.Unclear {
					t.Errorf("%+v", o)
				}
			}},
		{"timeout", func(w http.ResponseWriter) { time.Sleep(300 * time.Millisecond) },
			func(t *testing.T, o fleetSayOutcome) {
				if o.Status != 0 || !o.Unclear {
					t.Errorf("%+v", o)
				}
			}},
		{"401", func(w http.ResponseWriter) { http.Error(w, "Unauthorized", 401) },
			func(t *testing.T, o fleetSayOutcome) {
				if o.Status != 401 || o.Unclear {
					t.Errorf("%+v", o)
				}
			}},
		{"long body cut", func(w http.ResponseWriter) { http.Error(w, strings.Repeat("é", 400), 400) },
			func(t *testing.T, o fleetSayOutcome) {
				if len(o.Body) > 300 || len(o.Body) < 298 {
					t.Errorf("body len = %d", len(o.Body))
				}
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/fleet/say" || r.Method != http.MethodPost ||
					r.Header.Get("Authorization") != "Bearer tok" || r.Header.Get("Cookie") != "" {
					t.Errorf("bad request %s %s", r.Method, r.URL.Path)
				}
				var raw map[string]any
				dec := json.NewDecoder(r.Body)
				if err := dec.Decode(&raw); err != nil {
					t.Errorf("body: %v", err)
				}
				for _, k := range []string{"text", "cardId", "cardHash", "optionKey", "correction"} {
					if _, ok := raw[k]; !ok {
						t.Errorf("missing key %q in %v", k, raw)
					}
				}
				if len(raw) != 5 {
					t.Errorf("keys = %v, want exactly five", raw)
				}
				tc.handler(w)
			}))
			defer srv.Close()
			o := newFleetClient(srv.URL, "tok").Say(fleetSayRequest{
				Text: "go", CardID: "a", CardHash: "h", OptionKey: "k", Correction: true})
			tc.check(t, o)
		})
	}
}

func TestFleetClientDismiss(t *testing.T) {
	reply := `{"ok":true}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/fleet/card/dismiss" || r.Method != http.MethodPost ||
			!bytes.Equal(b, []byte(`{"cardId":"x"}`)) || r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("bad request %s %s %q", r.Method, r.URL.Path, b)
		}
		io.WriteString(w, reply)
	}))
	defer srv.Close()
	c := newFleetClient(srv.URL, "tok")
	if res, err := c.Dismiss("x"); err != nil || !res.OK {
		t.Errorf("Dismiss = %+v, %v", res, err)
	}
	reply = `{"ok":false,"code":"not_found"}`
	if res, err := c.Dismiss("x"); err != nil || res.OK || res.Code != "not_found" {
		t.Errorf("Dismiss = %+v, %v", res, err)
	}
	reply = `garbage`
	if _, err := c.Dismiss("x"); err == nil {
		t.Error("a non-JSON answer must be an error")
	}
}

func TestFleetLocalReconnects(t *testing.T) {
	newSrv := func() *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"fleet":{"alive":true,"status":"idle"},"cards":[]}`)
		}))
	}
	a := newSrv()
	var calls atomic.Int32
	l := &fleetLocal{connect: func() (*fleetClient, fleetStateView, error) {
		calls.Add(1)
		c := newFleetClient(a.URL, "tok")
		v, err := c.GetState()
		return c, v, err
	}}
	if v, err := l.State(); err != nil || !v.Fleet.Alive || calls.Load() != 1 {
		t.Fatalf("first State = %+v, %v, connects %d", v, err, calls.Load())
	}
	if _, err := l.State(); err != nil || calls.Load() != 1 {
		t.Fatalf("second State err %v, connects %d (must reuse the client)", err, calls.Load())
	}
	a.Close()
	if _, err := l.State(); err == nil {
		t.Fatal("State against a closed server must fail")
	}
	if l.c != nil {
		t.Error("a transport error must forget the client")
	}
	b := newSrv()
	defer b.Close()
	a = b
	if _, err := l.State(); err != nil || calls.Load() != 2 {
		t.Fatalf("State after reconnect err %v, connects %d, want 2", err, calls.Load())
	}
	if c, err := l.Client(); err != nil || c == nil || calls.Load() != 2 {
		t.Errorf("Client = %v, %v, connects %d", c, err, calls.Load())
	}
}

func TestFleetLocalKeepsClientOnHTTPStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusInternalServerError)
	}))
	defer srv.Close()
	l := &fleetLocal{c: newFleetClient(srv.URL, "tok")}
	if _, err := l.State(); err == nil {
		t.Fatal("want an error")
	}
	if l.c == nil {
		t.Error("an HTTP status proves the service is there; keep the client")
	}
}

func TestConnectLocalFleetFallsBackToTailscale(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.Host, "127.0.0.1") {
			http.Error(w, "paste-only listener", http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer secret" {
			http.Error(w, "bad", http.StatusUnauthorized)
			return
		}
		io.WriteString(w, `{"fleet":{"alive":true},"cards":[]}`)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())

	prevCfg, prevTS := fleetLocalConfig, localTailscaleIPv4
	defer func() { fleetLocalConfig, localTailscaleIPv4 = prevCfg, prevTS }()
	fleetLocalConfig = func() (ServerConfig, error) {
		return ServerConfig{Host: "127.0.0.1", Port: port, Token: "secret"}, nil
	}
	localTailscaleIPv4 = func(context.Context) string { return "localhost" }

	c, v, err := connectLocalFleet()
	if err != nil {
		t.Fatal(err)
	}
	if want := "http://localhost:" + u.Port(); c.base != want {
		t.Errorf("base = %q, want %q", c.base, want)
	}
	if !v.Fleet.Alive {
		t.Errorf("state = %+v", v)
	}

	localTailscaleIPv4 = func(context.Context) string { return "" }
	if _, _, err := connectLocalFleet(); err == nil {
		t.Error("loopback 404 with no Tailscale address must fail")
	}
}
