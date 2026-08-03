package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
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

func TestSendKeysRemoteSendsSessionIDAndText(t *testing.T) {
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

	r, err := sendKeysRemote("box", 42, "sess-1", "hello")
	if err != nil || !r.OK {
		t.Fatalf("sendKeysRemote = (%#v, %v)", r, err)
	}
	if string(gotBody) != `{"session_id":"sess-1","text":"hello"}` {
		t.Fatalf("body = %s, want session_id and text", gotBody)
	}
}

func TestSendKeysRemotePropagatesRefusal(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":false,"error":"PID 42 is a different session now","code":"session_mismatch"}`))
	}))
	defer backend.Close()

	u, _ := url.Parse(backend.URL)
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeServerYAML(t, home, "box", u.Hostname(), u.Port(), "secret")

	r, err := sendKeysRemote("box", 42, "sess-stale", "hello")
	if err != nil {
		t.Fatalf("sendKeysRemote err = %v", err)
	}
	if r.OK || r.Code != codeSessionMismatch {
		t.Fatalf("result = %+v, want refusal with codeSessionMismatch", r)
	}
}

func TestResizeRemoteSendsSessionIDColsRowsRevert(t *testing.T) {
	var gotPath string
	var gotBody []byte
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
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

	r, err := resizeRemote("box", 42, "sess-1", 120, 40, false)
	if err != nil || !r.OK {
		t.Fatalf("resizeRemote = (%#v, %v)", r, err)
	}
	if gotPath != "/sessions/42/resize" {
		t.Fatalf("path = %q, want /sessions/42/resize", gotPath)
	}
	if string(gotBody) != `{"cols":120,"revert":false,"rows":40,"session_id":"sess-1"}` {
		t.Fatalf("body = %s, want session_id/cols/rows/revert", gotBody)
	}
}

// The revert direction is the one whose failure is silent and permanent (a
// window left pinned to window-size=manual with nothing ever un-pinning it),
// and it was the direction with no client-side coverage at all: a typo in the
// route or a dropped field would have shipped green. cols/rows are 0 here
// because that is exactly what tui.go's revert call sites send.
func TestResizeRemoteRevertSendsRevertTrue(t *testing.T) {
	var gotPath string
	var gotBody []byte
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
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

	r, err := resizeRemote("box", 42, "sess-1", 0, 0, true)
	if err != nil || !r.OK {
		t.Fatalf("resizeRemote = (%#v, %v)", r, err)
	}
	if gotPath != "/sessions/42/resize" {
		t.Fatalf("path = %q, want /sessions/42/resize", gotPath)
	}
	if string(gotBody) != `{"cols":0,"revert":true,"rows":0,"session_id":"sess-1"}` {
		t.Fatalf("body = %s, want revert:true with session_id", gotBody)
	}
}

func TestResizeRemotePropagatesRefusal(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":false,"error":"PID 42 is a different session now","code":"session_mismatch"}`))
	}))
	defer backend.Close()

	u, _ := url.Parse(backend.URL)
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeServerYAML(t, home, "box", u.Hostname(), u.Port(), "secret")

	r, err := resizeRemote("box", 42, "sess-stale", 120, 40, false)
	if err != nil {
		t.Fatalf("resizeRemote err = %v", err)
	}
	if r.OK || r.Code != codeSessionMismatch {
		t.Fatalf("result = %+v, want refusal with codeSessionMismatch", r)
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

func TestMigrateRemoteOmitsSessionIDWhenUnknown(t *testing.T) {
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

	if _, err := migrateRemote("box", 42, ""); err != nil {
		t.Fatalf("migrateRemote err = %v", err)
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

// TestSwitchAccountRemote proves the client posts the name to /account/switch
// with the host's bearer token and reads back the new account email.
func TestSwitchAccountRemote(t *testing.T) {
	var gotPath, gotAuth, gotBody string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"account":"andy@trecs.aero"}`))
	}))
	defer backend.Close()

	u, _ := url.Parse(backend.URL)
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeServerYAML(t, home, "box", u.Hostname(), u.Port(), "secret")

	got, err := switchAccountRemote("box", "trecs")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if gotPath != "/account/switch" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotAuth != "Bearer secret" {
		t.Fatalf("auth = %q", gotAuth)
	}
	if gotBody != `{"name":"trecs"}` {
		t.Fatalf("body = %q", gotBody)
	}
	if !got.OK || got.Account != "andy@trecs.aero" {
		t.Fatalf("result = %+v", got)
	}
}

// TestSwitchAccountRemoteRefusal proves a 400 refusal is reported as the
// server's own explanation (which lists the names that host holds) rather than a
// bare "HTTP 400" transport error.
func TestSwitchAccountRemoteRefusal(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"ok":false,"code":"unknown_account","message":"no account snapshot for \"nope\" (known: avisoma)"}`))
	}))
	defer backend.Close()

	u, _ := url.Parse(backend.URL)
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeServerYAML(t, home, "box", u.Hostname(), u.Port(), "secret")

	got, err := switchAccountRemote("box", "nope")
	if err != nil {
		t.Fatalf("err = %v, want the refusal reported in the result", err)
	}
	if got.OK || got.Code != codeUnknownAccount || !strings.Contains(got.Message, "avisoma") {
		t.Fatalf("result = %+v, want the host's own message", got)
	}
}

func TestFetchRemoteTranscriptTail(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Query().Get("session_id") != "sid1" || r.URL.Query().Get("n") != "5" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(transcriptTailResponse{
			Turns:      []transcriptTurn{{Role: "user", Text: "hi"}},
			ModifiedAt: time.Unix(1000, 0).UTC(),
			Size:       42,
		})
	}))
	defer backend.Close()

	u, _ := url.Parse(backend.URL)
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeServerYAML(t, home, "test-host", u.Hostname(), u.Port(), "secret")

	turns, mtime, size, err := fetchRemoteTranscriptTail("test-host", "sid1", 5)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(turns) != 1 || turns[0].Text != "hi" {
		t.Errorf("Turns = %+v", turns)
	}
	if size != 42 {
		t.Errorf("Size = %d, want 42", size)
	}
	if !mtime.Equal(time.Unix(1000, 0).UTC()) {
		t.Errorf("ModifiedAt = %v", mtime)
	}
}

func TestFetchRemoteTranscriptTailUnknownServer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	_, _, _, err := fetchRemoteTranscriptTail("no-such-server", "sid1", 5)
	if err == nil {
		t.Fatal("want error for unknown server")
	}
}
