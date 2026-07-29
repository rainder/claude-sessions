package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"
)

// TestFetchRemotePreviewSanitizesBody proves the client re-sanitizes the body it
// receives: an old or compromised server that emits raw escape sequences (here an
// OSC title-set) must not have them reach the caller's terminal.
func TestFetchRemotePreviewSanitizesBody(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Claude-Sessions-Preview-Source", "tmux")
		w.Header().Set("X-Claude-Sessions-Preview-Label", "dev:0.0")
		_, _ = w.Write([]byte("\x1b]0;evil\x07hi"))
	}))
	defer backend.Close()

	u, _ := url.Parse(backend.URL)
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeServerYAML(t, home, "box", u.Hostname(), u.Port(), "secret")

	got, err := fetchRemotePreview("box", 42, DefaultPreviewLimits())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got.Content != "hi" {
		t.Fatalf("content = %q, want %q", got.Content, "hi")
	}
}

// TestFetchRemoteCwdSuggestionsParsesHome proves the client reads the remote
// host's home directory alongside the ranked suggestions so the picker can
// collapse it to "~".
func TestFetchRemoteCwdSuggestionsParsesHome(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"home":"/home/bob","suggestions":[{"cwd":"/home/bob/repo","count":3}]}`))
	}))
	defer backend.Close()

	u, _ := url.Parse(backend.URL)
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeServerYAML(t, home, "box", u.Hostname(), u.Port(), "secret")

	suggestions, remoteHome, err := fetchRemoteCwdSuggestions("box")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if remoteHome != "/home/bob" {
		t.Fatalf("home = %q, want %q", remoteHome, "/home/bob")
	}
	want := []cwdSuggestion{{CWD: "/home/bob/repo", Count: 3}}
	if !reflect.DeepEqual(suggestions, want) {
		t.Fatalf("suggestions = %#v, want %#v", suggestions, want)
	}
}

func TestPostDisableRemoteSendsResolvedIdentity(t *testing.T) {
	var gotBody []byte
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"session_id":"sess-1","disabled":true}`))
	}))
	defer backend.Close()

	u, _ := url.Parse(backend.URL)
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeServerYAML(t, home, "box", u.Hostname(), u.Port(), "secret")

	r, err := postDisableRemote("box", 42, "sess-1", true)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !r.OK || r.SessionID != "sess-1" || r.Disabled == nil || !*r.Disabled {
		t.Fatalf("response = %#v", r)
	}
	var sent struct {
		SessionID string `json:"session_id"`
		Disabled  bool   `json:"disabled"`
	}
	if err := json.Unmarshal(gotBody, &sent); err != nil {
		t.Fatalf("decode sent body: %v", err)
	}
	if sent.SessionID != "sess-1" || !sent.Disabled {
		t.Fatalf("sent body = %#v, want session_id=sess-1 disabled=true", sent)
	}
}

func TestPostDisableRemoteSurfacesRefusal(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":false,"error":"PID 42 is not a live Claude session","code":"not_live"}`))
	}))
	defer backend.Close()

	u, _ := url.Parse(backend.URL)
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeServerYAML(t, home, "box", u.Hostname(), u.Port(), "secret")

	r, err := postDisableRemote("box", 42, "sess-1", true)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if r.OK || r.Code != codeNotLive {
		t.Fatalf("response = %#v, want not_live refusal", r)
	}
}

func TestKillRemoteSendsSessionIDWhenKnown(t *testing.T) {
	var gotBody []byte
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()

	u, _ := url.Parse(backend.URL)
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeServerYAML(t, home, "box", u.Hostname(), u.Port(), "secret")

	r, err := killRemote("box", 42, "sess-1")
	if err != nil || !r.OK {
		t.Fatalf("killRemote = (%#v, %v)", r, err)
	}
	if string(gotBody) != `{"session_id":"sess-1"}` {
		t.Fatalf("body = %s, want session_id sent", gotBody)
	}
}

func TestKillRemoteOmitsSessionIDWhenUnknown(t *testing.T) {
	var gotBody []byte
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()

	u, _ := url.Parse(backend.URL)
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeServerYAML(t, home, "box", u.Hostname(), u.Port(), "secret")

	if _, err := killRemote("box", 42, ""); err != nil {
		t.Fatalf("killRemote err = %v", err)
	}
	if string(gotBody) != `{}` {
		t.Fatalf("body = %s, want bare {} when session id unknown", gotBody)
	}
}

func TestMigrateRemoteSendsSessionIDWhenKnown(t *testing.T) {
	var gotBody []byte
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"tmux":"cs-1"}`))
	}))
	defer backend.Close()

	u, _ := url.Parse(backend.URL)
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeServerYAML(t, home, "box", u.Hostname(), u.Port(), "secret")

	r, err := migrateRemote("box", 7, "sess-2")
	if err != nil || !r.OK || r.Tmux != "cs-1" {
		t.Fatalf("migrateRemote = (%#v, %v)", r, err)
	}
	if string(gotBody) != `{"session_id":"sess-2"}` {
		t.Fatalf("body = %s, want session_id sent", gotBody)
	}
}
