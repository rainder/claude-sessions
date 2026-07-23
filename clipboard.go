package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"time"
)

// Laptop side of remote image paste. While the user is attached to a remote
// session (see runRemoteAttach), a goroutine runs pasteRelayLoop: it long-polls
// the server's /paste-wait endpoint and, whenever the remote pane requests a
// paste, reads THIS machine's clipboard image and pushes it back to /paste. It
// never touches stdin or the terminal (which belong to ssh) and logs nothing.

const (
	// pasteWaitClientTimeout sits just above the server's long-poll window so a
	// single GET spans one server cycle without tripping its own timeout first.
	pasteWaitClientTimeout = 30 * time.Second
	// pasteUploadTimeout bounds a clipboard push (a few MB over the LAN).
	pasteUploadTimeout = 15 * time.Second
	// pastePollBackoff throttles retries after a transport error so a down
	// server doesn't spin the loop.
	pastePollBackoff = 1 * time.Second
)

// pasteRelayLoop long-polls srv for paste requests in the given tmux session
// until ctx is cancelled, pushing the local clipboard image for each one. All
// errors are swallowed.
func pasteRelayLoop(ctx context.Context, srv ServerConfig, session string) {
	for {
		if ctx.Err() != nil {
			return
		}
		paneID, gotRequest, hardErr := pasteWaitPoll(ctx, srv, session)
		if ctx.Err() != nil {
			return
		}
		if gotRequest {
			relayClipboard(ctx, srv, paneID)
			continue
		}
		if hardErr {
			// Transport failure (server down, refused): back off before retrying.
			select {
			case <-ctx.Done():
				return
			case <-time.After(pastePollBackoff):
			}
		}
		// A clean 204 (server long-poll timed out with no request) loops at once.
	}
}

// pasteWaitPoll performs one GET /paste-wait scoped to the given tmux session.
// It returns the requested pane id on a 200, gotRequest=false with
// hardErr=false on a clean 204, and hardErr=true on any transport/protocol
// failure worth backing off from.
func pasteWaitPoll(ctx context.Context, srv ServerConfig, session string) (paneID string, gotRequest, hardErr bool) {
	reqCtx, cancel := context.WithTimeout(ctx, pasteWaitClientTimeout)
	defer cancel()

	q := url.Values{}
	if session != "" {
		q.Set("session", session)
	}
	u := fmt.Sprintf("http://%s:%d/paste-wait", srv.Host, srv.Port)
	if enc := q.Encode(); enc != "" {
		u += "?" + enc
	}
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, u, nil)
	if err != nil {
		return "", false, true
	}
	req.Header.Set("Authorization", "Bearer "+srv.Token)

	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return "", false, true
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNoContent {
		return "", false, false
	}
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return "", false, true
	}
	var body struct {
		PaneID string `json:"pane_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || body.PaneID == "" {
		return "", false, true
	}
	return body.PaneID, true, false
}

// relayClipboard reads the local clipboard image and POSTs it to /paste for
// paneID. An empty or unreadable clipboard becomes an empty=1 passthrough so the
// server sends the raw Ctrl+V instead. The temp path is reserved with
// os.CreateTemp (unpredictable name, 0600) to defeat symlink-follow on a
// predictable path — including on the darwin osascript branch, which opens the
// existing file rather than creating its own.
func relayClipboard(ctx context.Context, srv ServerConfig, paneID string) {
	f, err := os.CreateTemp(os.TempDir(), "claude-paste-*.png")
	if err != nil {
		postPaste(ctx, srv, paneID, nil, true)
		return
	}
	tmp := f.Name()
	_ = f.Close()
	defer os.Remove(tmp)

	if !readClipboardImage(tmp) {
		postPaste(ctx, srv, paneID, nil, true)
		return
	}
	data, err := os.ReadFile(tmp)
	if err != nil || len(data) == 0 {
		postPaste(ctx, srv, paneID, nil, true)
		return
	}
	postPaste(ctx, srv, paneID, data, false)
}

// postPaste sends the clipboard image (or empty=1 signal) to /paste?pane=<id>.
func postPaste(ctx context.Context, srv ServerConfig, paneID string, data []byte, empty bool) {
	reqCtx, cancel := context.WithTimeout(ctx, pasteUploadTimeout)
	defer cancel()

	q := url.Values{}
	q.Set("pane", paneID)
	if empty {
		q.Set("empty", "1")
	}
	u := fmt.Sprintf("http://%s:%d/paste?%s", srv.Host, srv.Port, q.Encode())

	var bodyReader io.Reader
	if !empty {
		bodyReader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, u, bodyReader)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+srv.Token)
	if !empty {
		req.Header.Set("Content-Type", "image/png")
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

// readClipboardImage writes the local clipboard's image to dst as PNG, returning
// true only when a non-empty image was captured. Best-effort per platform; any
// missing tool, tool error, or empty result yields false.
func readClipboardImage(dst string) bool {
	switch runtime.GOOS {
	case "darwin":
		// pngpaste is the fast path; osascript writing «class PNGf» is the
		// fallback when it isn't installed.
		if _, err := exec.LookPath("pngpaste"); err == nil {
			if err := exec.Command("pngpaste", dst).Run(); err == nil && fileNonEmpty(dst) {
				return true
			}
		}
		if runOSAScriptClipboard(dst) && fileNonEmpty(dst) {
			return true
		}
		return false
	case "linux":
		tools := []struct {
			name string
			args []string
		}{
			{"wl-paste", []string{"-t", "image/png"}},
			{"xclip", []string{"-selection", "clipboard", "-t", "image/png", "-o"}},
		}
		for _, tool := range tools {
			if _, err := exec.LookPath(tool.name); err != nil {
				continue
			}
			if captureToFile(dst, tool.name, tool.args...) && fileNonEmpty(dst) {
				return true
			}
		}
		return false
	default:
		return false
	}
}

// runOSAScriptClipboard pulls a PNG off the macOS clipboard into dst via
// AppleScript. Returns false if the clipboard holds no image (the get fails).
func runOSAScriptClipboard(dst string) bool {
	err := exec.Command("osascript",
		"-e", "set png to the clipboard as «class PNGf»",
		"-e", fmt.Sprintf("set f to open for access POSIX file %q with write permission", dst),
		"-e", "set eof f to 0",
		"-e", "write png to f",
		"-e", "close access f",
	).Run()
	return err == nil
}

// captureToFile runs name+args with stdout redirected to a freshly-truncated
// dst. Returns true only when the command exits zero.
func captureToFile(dst, name string, args ...string) bool {
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return false
	}
	defer f.Close()
	cmd := exec.Command(name, args...)
	cmd.Stdout = f
	return cmd.Run() == nil
}

// fileNonEmpty reports whether path exists and has a non-zero size.
func fileNonEmpty(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Size() > 0
}
