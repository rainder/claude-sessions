package main

import (
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
