package main

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestAccountSwitchResultToast(t *testing.T) {
	base := accountSwitchToast("box", "work", "andy@example.com", nil)
	cases := []struct {
		name     string
		warnings bool
		n        *fleetSwitchNotice
		suffix   string
		warn     bool
	}{
		{"no notifier", false, nil, base, false},
		{"told", false, &fleetSwitchNotice{Told: true}, " · FLEET told", false},
		{"told with warning", true, &fleetSwitchNotice{Told: true}, " · FLEET told", true},
		{"not told", false, &fleetSwitchNotice{Reason: "FLEET busy"}, " · FLEET not told (FLEET busy)", true},
		{"no FLEET", false, &fleetSwitchNotice{Reason: "no FLEET found"}, " · FLEET not told (no FLEET found)", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, warn := accountSwitchResultToast(base, c.warnings, c.n)
			if !strings.HasPrefix(got, base) || !strings.HasSuffix(got, c.suffix) || warn != c.warn {
				t.Fatalf("toast %q warn %v", got, warn)
			}
			if c.n == nil && got != base {
				t.Fatalf("no notifier must add nothing: %q", got)
			}
		})
	}
}

func TestFleetSwitchNoticeFor(t *testing.T) {
	cases := []struct {
		code   string
		err    error
		told   bool
		reason string
	}{
		{"sent", nil, true, ""},
		{"target_missing", nil, false, "no FLEET found"},
		{"target_ambiguous", nil, false, "two FLEETs live"},
		{"busy", nil, false, "FLEET busy"},
		{"dialog", nil, false, "FLEET shows a dialog"},
		{"not_submitted", nil, false, "typed, Enter not confirmed"},
		{"something_new", nil, false, "refused: something_new"},
		{"", errors.New("dial tcp: refused"), false, "request failed"},
		{"", &fleetStatusError{Status: http.StatusNotFound}, false, "FLEET server too old"},
		{"", &fleetStatusError{Status: http.StatusUnauthorized}, false, "token rejected"},
		{"", &fleetStatusError{Status: http.StatusBadRequest}, false, "request failed"},
	}
	for _, c := range cases {
		n := fleetSwitchNoticeFor(c.code, c.err)
		if n.Told != c.told || n.Reason != c.reason {
			t.Errorf("%q %v: %+v", c.code, c.err, n)
		}
	}
}

func TestAccountSwitchEventRequest(t *testing.T) {
	rows := []accountRow{
		{Name: "home", Tool: accountToolClaude, Active: true},
		{Name: "work", Tool: accountToolClaude},
		{Name: "gx", Tool: accountToolGrok, Active: true},
		{Name: "gy", Tool: accountToolGrok},
	}
	got := accountSwitchEventRequest("ws", rows[3], rows)
	if got != (fleetAccountEventRequest{Host: "ws", Tool: accountToolGrok, From: "gx", To: "gy"}) {
		t.Fatalf("remote %+v", got)
	}
	got = accountSwitchEventRequest("", rows[1], rows)
	if got.Host != shortHostname() || got.From != "home" || got.To != "work" || got.Tool != accountToolClaude {
		t.Fatalf("local %+v", got)
	}
	if got := accountSwitchEventRequest("ws", rows[1], rows[1:2]); got.From != "" {
		t.Fatalf("no active row must leave from empty: %+v", got)
	}
}

// fleetEventRig is a fake FLEET service: /fleet/state answers alive (or
// not), /fleet/account-event answers with eventStatus and event.
func fleetEventRig(t *testing.T, alive bool, eventStatus int, event string) (*fleetLocal, *atomic.Int32) {
	t.Helper()
	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/fleet/state":
			if alive {
				io.WriteString(w, `{"fleet":{"alive":true,"status":"idle"},"cards":[]}`)
			} else {
				io.WriteString(w, `{"fleet":{"alive":false},"cards":[]}`)
			}
		case "/fleet/account-event":
			posts.Add(1)
			w.WriteHeader(eventStatus)
			io.WriteString(w, `{"ok":true,"event":"`+event+`"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return &fleetLocal{connect: func() (*fleetClient, fleetStateView, error) {
		c := newFleetClient(srv.URL, "tok")
		v, err := c.GetState()
		return c, v, err
	}}, &posts
}

func TestFleetAccountEventVia(t *testing.T) {
	req := fleetAccountEventRequest{Host: "box", Tool: accountToolClaude, From: "home", To: "work"}
	cases := []struct {
		name   string
		alive  bool
		status int
		event  string
		want   fleetSwitchNotice
		posts  int32
	}{
		{"told", true, http.StatusOK, "sent", fleetSwitchNotice{Told: true}, 1},
		{"FLEET busy", true, http.StatusOK, "busy", fleetSwitchNotice{Reason: "FLEET busy"}, 1},
		{"old server", true, http.StatusNotFound, "", fleetSwitchNotice{Reason: "FLEET server too old"}, 1},
		{"not running", false, http.StatusOK, "sent", fleetSwitchNotice{Reason: "no FLEET found"}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src, posts := fleetEventRig(t, c.alive, c.status, c.event)
			if got := fleetAccountEventVia(src)(req); got != c.want || posts.Load() != c.posts {
				t.Fatalf("notice %+v posts %d", got, posts.Load())
			}
		})
	}
	t.Run("no service", func(t *testing.T) {
		src := &fleetLocal{connect: func() (*fleetClient, fleetStateView, error) {
			return nil, fleetStateView{}, errors.New("no service")
		}}
		if got := fleetAccountEventVia(src)(req); got != (fleetSwitchNotice{Reason: "no FLEET found"}) {
			t.Fatalf("notice %+v", got)
		}
	})
}
