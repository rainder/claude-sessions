package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// sshMuxFlags turn on SSH connection multiplexing: the first attach to a host
// leaves a master connection behind (socket under ~/.ssh), and every attach
// within the next 60 seconds reuses it, skipping the handshake entirely.
// ControlPersist self-expires the master, so there is no cleanup path to
// manage; if the socket can't be created ssh degrades to a normal connection.
// %C hashes local host + remote host/port/user, keeping the path short and
// per-destination.
var sshMuxFlags = []string{
	"-o", "ControlMaster=auto",
	"-o", "ControlPath=~/.ssh/cs-mux-%C",
	"-o", "ControlPersist=60",
}

// runRemoteAttach runs the interactive `ssh -t <target> tmux attach -t <tname>`,
// relaying local-clipboard image pastes to srv for the lifetime of the attach.
// The relay goroutine (pasteRelayLoop) never reads stdin or writes the terminal
// — those belong to ssh — and is cancelled and joined the moment the attach
// returns.
func runRemoteAttach(c *actCtx, srv ServerConfig, tname string) error {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		pasteRelayLoop(ctx, srv, tname)
	}()
	args := append(append([]string{}, sshMuxFlags...),
		"-t", srv.EffectiveSSHTarget(), "tmux", "attach", "-t", tname)
	err := c.runInteractive("ssh", args...)
	cancel()
	wg.Wait()
	return err
}

// Remote action helpers — invoked from the TUI when the selected row is on a
// configured server. Mirror the local actions, but talk to the server's HTTP
// API and SSH for the interactive attach.

// serverRequestAttempt performs one HTTP request to a resolved server. Its
// responseReceived result is true as soon as http.Client.Do returns a response,
// including the unusual case where it also returns an error. Callers use that
// signal to decide whether a different endpoint may be attempted.
func serverRequestAttempt(
	ctx context.Context,
	srv ServerConfig,
	path, method string,
	body []byte,
) (data []byte, responseReceived bool, err error) {
	url := fmt.Sprintf("http://%s:%d%s", srv.Host, srv.Port, path)
	var bodyReader io.Reader
	if len(body) > 0 {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Authorization", "Bearer "+srv.Token)
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		if resp != nil {
			if resp.Body != nil {
				_ = resp.Body.Close()
			}
			return nil, true, err
		}
		return nil, false, err
	}
	defer resp.Body.Close()

	data, err = io.ReadAll(resp.Body)
	if err != nil {
		return data, true, err
	}
	if resp.StatusCode != http.StatusOK {
		return data, true, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return data, true, nil
}

// serverRequestWithTimeout performs an HTTP request to a resolved server with a
// single operation timeout. body is JSON if non-empty. Returns the response body
// or an error.
func serverRequestWithTimeout(srv ServerConfig, path, method string, body []byte, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	data, _, err := serverRequestAttempt(ctx, srv, path, method, body)
	return data, err
}

// remoteRequestWithTimeout performs an HTTP request to the named server with an
// explicit client timeout. body is JSON if non-empty. Returns the response body
// or an error.
func remoteRequestWithTimeout(name, path, method string, body []byte, timeout time.Duration) ([]byte, error) {
	srv, ok := LookupServer(name)
	if !ok {
		return nil, fmt.Errorf("unknown server: %s", name)
	}
	return serverRequestWithTimeout(srv, path, method, body, timeout)
}

// remoteRequest performs an HTTP request to the named server with the default
// 30s timeout. body is JSON if non-empty. Returns the response body or an error.
func remoteRequest(name, path, method string, body []byte) ([]byte, error) {
	return remoteRequestWithTimeout(name, path, method, body, 30*time.Second)
}

// fetchRemotePreview retrieves a bounded, sanitized preview from the named
// server, passing its limits as query params so the remote output matches the
// caller's ceiling. A 404 (session/transcript gone) maps to errSessionEnded;
// other non-200s surface the same concise HTTP error style as remoteRequest.
// The body is capped via io.LimitReader and rejected if it exceeds MaxBytes.
// The content is re-sanitized client-side (the server already sanitizes, but an
// old or compromised server could feed raw escapes, and clipLine passes escapes
// through) so nothing untrusted reaches the viewer's terminal.
func fetchRemotePreview(host string, pid int, limits PreviewLimits) (PreviewResult, error) {
	srv, ok := LookupServer(host)
	if !ok {
		return PreviewResult{}, fmt.Errorf("unknown server: %s", host)
	}
	url := fmt.Sprintf("http://%s:%d/sessions/%d/preview?lines=%d&bytes=%d",
		srv.Host, srv.Port, pid, limits.MaxLines, limits.MaxBytes)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return PreviewResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+srv.Token)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return PreviewResult{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return PreviewResult{}, errSessionEnded
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, int64(limits.MaxBytes)+1))
	if resp.StatusCode != http.StatusOK {
		return PreviewResult{}, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if len(data) > limits.MaxBytes {
		return PreviewResult{}, fmt.Errorf("preview exceeds %d bytes", limits.MaxBytes)
	}
	return PreviewResult{
		Source:  resp.Header.Get("X-Claude-Sessions-Preview-Source"),
		Label:   resp.Header.Get("X-Claude-Sessions-Preview-Label"),
		Content: sanitizeTerminalText(string(data)),
	}, nil
}

const transcriptTailMaxBytes = 256 * 1024

// sanitizeTranscriptTailErrExcerpt caps and single-lines a remote error body
// before it's embedded in an error string. fetchRemoteTranscriptTail's
// response body can be up to transcriptTailMaxBytes and is otherwise
// unsanitized; that error flows into the rendered info dialog via
// snap.Err.Error(), and embedded newlines/control characters there break
// the dialog's box-border rendering.
func sanitizeTranscriptTailErrExcerpt(s string) string {
	s = strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', '\t':
			return ' '
		}
		if r < 0x20 {
			return ' '
		}
		return r
	}, s)
	const maxLen = 200
	if len(s) > maxLen {
		s = s[:maxLen]
	}
	return s
}

// fetchRemoteTranscriptTail retrieves the raw last-n conversation turns from
// the named server, modeled directly on fetchRemotePreview. A 404 (no
// transcript for that session) maps to errTranscriptNotFound.
func fetchRemoteTranscriptTail(host, sessionID string, n int) ([]transcriptTurn, time.Time, int64, error) {
	srv, ok := LookupServer(host)
	if !ok {
		return nil, time.Time{}, 0, fmt.Errorf("unknown server: %s", host)
	}
	endpoint := fmt.Sprintf("http://%s:%d/transcript-tail?session_id=%s&n=%d",
		srv.Host, srv.Port, url.QueryEscape(sessionID), n)
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, time.Time{}, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+srv.Token)
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, time.Time{}, 0, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, transcriptTailMaxBytes+1))
	if err != nil {
		return nil, time.Time{}, 0, err
	}
	if resp.StatusCode == http.StatusNotFound {
		if strings.TrimSpace(string(data)) == "transcript not found" {
			return nil, time.Time{}, 0, errTranscriptNotFound
		}
		return nil, time.Time{}, 0, fmt.Errorf("HTTP %d: %s", resp.StatusCode, sanitizeTranscriptTailErrExcerpt(strings.TrimSpace(string(data))))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, time.Time{}, 0, fmt.Errorf("HTTP %d: %s", resp.StatusCode, sanitizeTranscriptTailErrExcerpt(strings.TrimSpace(string(data))))
	}
	if len(data) > transcriptTailMaxBytes {
		return nil, time.Time{}, 0, fmt.Errorf("transcript-tail response exceeds %d bytes", transcriptTailMaxBytes)
	}
	var body transcriptTailResponse
	if err := json.Unmarshal(data, &body); err != nil {
		return nil, time.Time{}, 0, fmt.Errorf("bad response: %w", err)
	}
	return body.Turns, body.ModifiedAt, body.Size, nil
}

// fetchRemoteCwdSuggestions retrieves the ranked cwd history from the named
// server's /cwd-suggestions endpoint, using a short 5s timeout so a slow or
// unreachable host doesn't stall the picker. It also returns the remote host's
// home directory (when reported) so the picker can collapse it to "~"; an older
// server that omits the field yields an empty home and raw paths.
func fetchRemoteCwdSuggestions(host string) (suggestions []cwdSuggestion, home string, err error) {
	data, err := remoteRequestWithTimeout(host, "/cwd-suggestions", http.MethodGet, nil, 5*time.Second)
	if err != nil {
		return nil, "", err
	}
	var response struct {
		Home        string          `json:"home"`
		Suggestions []cwdSuggestion `json:"suggestions"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return nil, "", err
	}
	return response.Suggestions, response.Home, nil
}

// fetchRemoteResumable retrieves the named server's resumable-session list from
// its /resumable endpoint, using a short 5s timeout so a slow or unreachable
// host doesn't stall the picker. Host is left blank here; the caller tags each
// row with the server name.
func fetchRemoteResumable(host string) ([]ResumableSession, error) {
	data, err := remoteRequestWithTimeout(host, "/resumable", http.MethodGet, nil, 5*time.Second)
	if err != nil {
		return nil, err
	}
	var response struct {
		Sessions []ResumableSession `json:"sessions"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return nil, err
	}
	return response.Sessions, nil
}

// errPresetsUnavailable signals that a remote server's /presets response
// couldn't be used — either it predates the route (404) or its body isn't
// the expected JSON shape. Callers treat this as "unknown" and fall back to
// a local decision rather than a hard failure.
var errPresetsUnavailable = errors.New("presets endpoint unavailable")

// fetchRemotePresets retrieves the configured command presets (name + full
// command text) from the named server's /presets endpoint. Old servers
// without the route (404), or any response that isn't the expected JSON
// body, map to errPresetsUnavailable so callers can degrade gracefully
// instead of hard failing.
func fetchRemotePresets(host string) ([]CommandPreset, error) {
	srv, ok := LookupServer(host)
	if !ok {
		return nil, fmt.Errorf("unknown server: %s", host)
	}
	url := fmt.Sprintf("http://%s:%d/presets", srv.Host, srv.Port)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+srv.Token)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, errPresetsUnavailable
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var response struct {
		Presets []CommandPreset `json:"presets"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return nil, errPresetsUnavailable
	}
	return response.Presets, nil
}

// actKillRemote handles `k` on a remote-selected row.
func actKillRemote(c *actCtx) {
	s := c.selected()
	if s == nil {
		return
	}
	host, pid := s.Host, s.PID
	question := fmt.Sprintf("kill PID %d on %s?", pid, host)
	if s.NotIdle() {
		question = colorize(statusColor[s.Status], fmt.Sprintf("⚠ session is %s, not idle — killing will interrupt it", s.StatusDisplay())) + "\n" + question
	}
	pane := startRemotePreview(*s)
	confirmed := confirmOverlayPreview(question, pane, c.modalWakes, s.NotIdle())
	// Explicit call, not a defer: this frees the wake pipe before the remote
	// kill request and before the second (preview-less) worktree confirmOverlay
	// further down — the pane's raw fd is only safe while this loop is live.
	pane.close()
	if !confirmed {
		return
	}
	c.prepareLineOutput()
	defer c.enterRaw()

	fmt.Print("\nsending remote kill... ")
	r, err := killRemote(host, pid, s.SessionID)
	if err != nil {
		fmt.Printf("failed: %v\n", err)
		pauseForKey(c.fd, c.oldState)
		return
	}
	if !r.OK {
		fmt.Printf("failed: %s\n", r.Error)
		pauseForKey(c.fd, c.oldState)
		return
	}
	if r.Worktree == nil {
		return
	}
	// The server decided this kill emptied a worktree; ask, then have it remove.
	c.enterRaw() // confirmOverlay owns a fullscreen modal
	if !confirmOverlay(worktreeRemovalQuestion(r.Worktree.Path), c.modalWakes) {
		return
	}
	c.prepareLineOutput()
	fmt.Print("\nremoving worktree... ")
	if err := removeRemoteWorktree(host, r.Worktree.Path); err != nil {
		fmt.Printf("failed: %v\n", err)
		pauseForKey(c.fd, c.oldState)
	}
}

// removeRemoteWorktree asks host's server to remove a worktree checkout.
func removeRemoteWorktree(host, path string) error {
	body, err := json.Marshal(map[string]string{"path": path})
	if err != nil {
		return err
	}
	resp, err := remoteRequest(host, "/worktree/remove", "POST", body)
	if err != nil {
		return err
	}
	var r actionResult
	if err := json.Unmarshal(resp, &r); err != nil {
		return err
	}
	if !r.OK {
		return fmt.Errorf("%s", r.Error)
	}
	return nil
}

// postDisableRemote asks host's server to set pid's disabled state, and
// returns the state it actually applied — the server's own resolved
// SessionID and the value it wrote, never the caller's guess of what
// "disabled" now means. Mirrors removeRemoteWorktree's plain-function shape
// so the network+parse logic is unit-testable without a terminal.
func postDisableRemote(host string, pid int, sessionID string, disabled bool) (actionResult, error) {
	body, err := json.Marshal(map[string]any{"session_id": sessionID, "disabled": disabled})
	if err != nil {
		return actionResult{}, err
	}
	resp, err := remoteRequest(host, fmt.Sprintf("/sessions/%d/disable", pid), "POST", body)
	if err != nil {
		return actionResult{}, err
	}
	var r actionResult
	if err := json.Unmarshal(resp, &r); err != nil {
		return actionResult{}, err
	}
	return r, nil
}

// killRemote asks host's server to kill pid, sending sessionID as the
// precondition when known so the server's reattest (server.go:618) actually
// fires — an empty sessionID falls back to the bare {} body, matching
// sessionIDPrecondition's own "absent means no precondition" contract.
func killRemote(host string, pid int, sessionID string) (actionResult, error) {
	body := []byte(`{}`)
	if sessionID != "" {
		var err error
		body, err = json.Marshal(map[string]string{"session_id": sessionID})
		if err != nil {
			return actionResult{}, err
		}
	}
	resp, err := remoteRequest(host, fmt.Sprintf("/sessions/%d/kill", pid), "POST", body)
	if err != nil {
		return actionResult{}, err
	}
	var r actionResult
	if err := json.Unmarshal(resp, &r); err != nil {
		return actionResult{}, err
	}
	return r, nil
}

// sendKeysRemote asks host's server to send text as literal keystrokes plus
// Enter into pid's tmux pane. Unlike killRemote/migrateRemote, sessionID is
// never optional here — there is no legacy caller this must stay compatible
// with (see sendKeysBody, server.go), so there is no bare-{} fallback body.
func sendKeysRemote(host string, pid int, sessionID, text string) (actionResult, error) {
	body, err := json.Marshal(map[string]string{"session_id": sessionID, "text": text})
	if err != nil {
		return actionResult{}, err
	}
	resp, err := remoteRequest(host, fmt.Sprintf("/sessions/%d/send-keys", pid), "POST", body)
	if err != nil {
		return actionResult{}, err
	}
	var r actionResult
	if err := json.Unmarshal(resp, &r); err != nil {
		return actionResult{}, err
	}
	return r, nil
}

// migrateRemote asks host's server to migrate pid, same sessionID contract
// as killRemote.
func migrateRemote(host string, pid int, sessionID string) (actionResult, error) {
	body := []byte(`{}`)
	if sessionID != "" {
		var err error
		body, err = json.Marshal(map[string]string{"session_id": sessionID})
		if err != nil {
			return actionResult{}, err
		}
	}
	resp, err := remoteRequest(host, fmt.Sprintf("/sessions/%d/migrate", pid), "POST", body)
	if err != nil {
		return actionResult{}, err
	}
	var r actionResult
	if err := json.Unmarshal(resp, &r); err != nil {
		return actionResult{}, err
	}
	return r, nil
}

// switchAccountRemote asks host's server to make name its active Claude Code
// account. Same plain-function shape as killRemote/postDisableRemote, so the
// network+parse logic is unit-testable without a terminal.
//
// A refusal arrives as a non-200 (400 unknown_account / 500 switch_failed) whose
// body still carries the envelope, so the body is decoded before the transport
// error is considered: that is what lets the caller print the host's own "known:
// avisoma, trecs" message instead of a bare "HTTP 400".
func switchAccountRemote(host, name string) (accountSwitchResult, error) {
	body, err := json.Marshal(map[string]string{"name": name})
	if err != nil {
		return accountSwitchResult{}, err
	}
	resp, reqErr := remoteRequest(host, "/account/switch", "POST", body)
	var r accountSwitchResult
	decoded := json.Unmarshal(resp, &r) == nil
	if reqErr != nil {
		if decoded && r.Message != "" {
			return r, nil // a refusal the server explained; not a transport error
		}
		return accountSwitchResult{}, reqErr
	}
	if !decoded {
		return accountSwitchResult{}, fmt.Errorf("bad response from %s", host)
	}
	return r, nil
}

// actToggleDisabledRemote handles "-"/"+" on a remote-selected row. No
// confirmation dialog — unlike kill, disabling isn't destructive. Reports
// whether anything changed, matching actToggleDisabled's local-path contract.
func actToggleDisabledRemote(c *actCtx) bool {
	s := c.selected()
	if s == nil || s.SessionID == "" {
		return false
	}
	r, err := postDisableRemote(s.Host, s.PID, s.SessionID, !s.Disabled)
	if err != nil {
		showActionError(c, "disable", err)
		return false
	}
	if !r.OK {
		showActionError(c, "disable", fmt.Errorf("%s", r.Error))
		return false
	}
	return true
}

// actAttachRemote handles `a` on a remote-selected row. Gets the tmux session
// name (migrating first if needed), then `ssh -t host tmux attach -t name`.
func actAttachRemote(c *actCtx) {
	s := c.selected()
	if s == nil {
		return
	}
	host, pid := s.Host, s.PID
	srv, ok := LookupServer(host)
	if !ok {
		c.prepareLineOutput()
		fmt.Printf("\nunknown server: %s\n", host)
		pauseForKey(c.fd, c.oldState)
		c.enterRaw()
		return
	}

	// Fetch tmux info.
	resp, err := remoteRequest(host, fmt.Sprintf("/sessions/%d/tmux-info", pid), "GET", nil)
	if err != nil {
		c.prepareLineOutput()
		fmt.Printf("\ntmux-info failed: %v\n", err)
		pauseForKey(c.fd, c.oldState)
		c.enterRaw()
		return
	}
	var info struct {
		Tmux string `json:"tmux"`
	}
	_ = json.Unmarshal(resp, &info)

	tname := info.Tmux
	if tname == "" {
		// Not in tmux — offer migration.
		question := fmt.Sprintf("PID %d on %s is not in tmux. Migrate first?", pid, host)
		if s.NotIdle() {
			question = colorize(statusColor[s.Status], fmt.Sprintf("⚠ session is %s, not idle — migrating will interrupt it", s.StatusDisplay())) + "\n" + question
		}
		pane := startRemotePreview(*s)
		confirmed := confirmOverlayPreview(question, pane, c.modalWakes, s.NotIdle())
		pane.close()
		if !confirmed {
			return
		}
		c.prepareLineOutput()
		fmt.Print("\nmigrating... ")
		r, merr := migrateRemote(host, pid, s.SessionID)
		if merr != nil {
			fmt.Printf("failed: %v\n", merr)
			pauseForKey(c.fd, c.oldState)
			c.enterRaw()
			return
		}
		if !r.OK || r.Tmux == "" {
			fmt.Printf("failed: %s\n", r.Error)
			pauseForKey(c.fd, c.oldState)
			c.enterRaw()
			return
		}
		tname = r.Tmux
		fmt.Printf("ok → %s\n", tname)
		c.enterRaw()
	}

	// SSH into the host and attach to the tmux session, relaying clipboard
	// image pastes back to the server for the duration of the attach.
	_ = runRemoteAttach(c, srv, tname)
}

// remoteNewRows renders the picker rows for a remote new-session modal. It
// merges defaultCWD and the fetched suggestions into ordered entries, formats
// each as a fixed-width path plus dim frequency, and appends the manual-entry
// row. start is the index of the default row. Unlike the local picker it does
// no isDir/hiddenCwd filtering — the paths live on the remote host. home is the
// remote host's home directory (empty if unknown): it collapses only the
// DISPLAYED path to "~"; entries[i].cwd keeps the real absolute remote path for
// the POST body.
func remoteNewRows(defaultCWD string, suggestions []cwdSuggestion, home string) (lines []string, start int, entries []cwdEntry) {
	entries = mergeRemoteCwdEntries(defaultCWD, suggestions)
	lines = make([]string, 0, len(entries)+1)
	for i, entry := range entries {
		if entry.isDefault {
			start = i
		}
		freq := ""
		if entry.count > 0 {
			freq = "  " + dim(fmt.Sprintf("(%d)", entry.count))
		}
		lines = append(lines, fmt.Sprintf("%-50s%s", collapseHome(entry.cwd, home), freq))
	}
	lines = append(lines, "enter path manually…")
	return lines, start, entries
}

// remoteCommandPresetsForPicker returns the command preset choices to offer
// when spawning on a remote host: the remote's own presets (name + command
// text) fetched live over /presets, so the picker reflects what that host
// actually has configured rather than this one. Falls back to this host's
// local presets when the remote is unreachable or predates the /presets
// route.
func remoteCommandPresetsForPicker(host string) ([]CommandPreset, error) {
	if presets, err := fetchRemotePresets(host); err == nil && len(presets) > 0 {
		return presets, nil
	}
	return LoadCommandPresets()
}

// actNewRemote prompts for a cwd and POSTs /sessions/new to the named remote
// server. A populated remote row supplies defaultCWD; an empty host does not.
func actNewRemote(c *actCtx, host, defaultCWD string) {
	presets, err := remoteCommandPresetsForPicker(host)
	if err != nil {
		c.prepareLineOutput()
		fmt.Printf("\nload commands: %v\n", err)
		pauseForKey(c.fd, c.oldState)
		c.enterRaw()
		return
	}
	presetStart := LoadCommandPresetIndex(presets)

	// Fetch the remote host's cwd history for the picker. A slow or unreachable
	// host must not block manual entry, so on error we fall back to no
	// suggestions and surface a note in the modal.
	suggestions, home, err := fetchRemoteCwdSuggestions(host)
	note := ""
	if err != nil {
		suggestions = nil
		home = ""
		note = "remote suggestions unavailable"
	}
	lines, start, entries := remoteNewRows(defaultCWD, suggestions, home)

	row, presetIndex, prompt, ok := pickNewSession("New session on "+host, lines, start, presets, presetStart, note, c.modalWakes)
	if !ok {
		return
	}
	preset := presets[presetIndex]
	SaveCommandPresetName(preset.Name)

	c.prepareLineOutput()
	defer c.enterRaw()

	var cwd string
	if row < len(entries) {
		cwd = entries[row].cwd
	} else {
		// Manual entry. Do not locally expand or validate — the path lives on
		// the remote host; the server resolves and checks it.
		input := readLine("\ncwd path (q=cancel) > ")
		if input == "" || input == "q" || input == "Q" {
			return
		}
		cwd = input
	}

	fmt.Printf("\nspawning on %s in %s... ", host, cwd)
	body, _ := json.Marshal(map[string]string{
		"cwd":        cwd,
		"command":    preset.Name,
		"prompt":     prompt,
		"request_id": newSpawnRequestID(),
	})
	resp, err := remoteRequest(host, "/sessions/new", "POST", body)
	if err != nil {
		fmt.Printf("failed: %v\n", err)
		pauseForKey(c.fd, c.oldState)
		return
	}
	var r actionResult
	_ = json.Unmarshal(resp, &r)
	if !r.OK || r.Tmux == "" {
		fmt.Printf("failed: %s\n", r.Error)
		pauseForKey(c.fd, c.oldState)
		return
	}
	c.spawnedHost = host
	c.spawnedTmux = r.Tmux
	if prompt != "" {
		fmt.Printf("ok → %s (running in background)\n", r.Tmux)
		c.spawnedBackground = true
		return
	}
	fmt.Printf("ok → %s\n", r.Tmux)

	srv, _ := LookupServer(host)
	c.enterRaw()
	_ = runRemoteAttach(c, srv, r.Tmux)
}

// pidPart extracts the integer pid from a "host:pid" ID. Returns 0 if not a
// remote-style ID.
func pidPart(id string) int {
	i := strings.LastIndex(id, ":")
	if i < 0 {
		return 0
	}
	n, _ := strconv.Atoi(id[i+1:])
	return n
}
