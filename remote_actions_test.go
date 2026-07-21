package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
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
