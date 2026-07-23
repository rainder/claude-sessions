package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

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

// errPresetsUnavailable signals that a remote server's /presets response
// couldn't be used — either it predates the route (404) or its body isn't
// the expected JSON shape. Callers treat this as "unknown" and fall back to
// a local decision rather than a hard failure.
var errPresetsUnavailable = errors.New("presets endpoint unavailable")

// fetchRemotePresets retrieves the configured command preset NAMES from the
// named server's /presets endpoint. Names only — the server never exposes
// its command text to remote clients. Old servers without the route (404),
// or any response that isn't the expected JSON body, map to
// errPresetsUnavailable so callers can degrade gracefully instead of hard
// failing.
func fetchRemotePresets(host string) ([]string, error) {
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
		Presets []string `json:"presets"`
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
	c.prepareLineOutput()
	defer c.enterRaw()

	if !confirm(fmt.Sprintf("\nkill PID %d on %s? [y/N] ", pid, host)) {
		return
	}
	fmt.Print("\nsending remote kill... ")
	resp, err := remoteRequest(host, fmt.Sprintf("/sessions/%d/kill", pid), "POST", []byte(`{}`))
	if err != nil {
		fmt.Printf("failed: %v\n", err)
		pauseForKey(c.fd, c.oldState)
		return
	}
	var r actionResult
	_ = json.Unmarshal(resp, &r)
	if !r.OK {
		fmt.Printf("failed: %s\n", r.Error)
		pauseForKey(c.fd, c.oldState)
	}
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
	sshTarget := srv.EffectiveSSHTarget()

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
		c.prepareLineOutput()
		if !confirm(fmt.Sprintf("\nPID %d on %s is not in tmux. Migrate first? [y/N] ", pid, host)) {
			c.enterRaw()
			return
		}
		fmt.Print("\nmigrating... ")
		mresp, merr := remoteRequest(host, fmt.Sprintf("/sessions/%d/migrate", pid), "POST", []byte(`{}`))
		if merr != nil {
			fmt.Printf("failed: %v\n", merr)
			pauseForKey(c.fd, c.oldState)
			c.enterRaw()
			return
		}
		var r actionResult
		_ = json.Unmarshal(mresp, &r)
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

	// SSH into the host and attach to the tmux session.
	_ = c.runInteractive("ssh", "-t", sshTarget, "tmux", "attach", "-t", tname)
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
// when spawning on a remote host: the remote's own preset names fetched live
// over /presets, so the picker reflects what that host actually has
// configured rather than this one. The server never exposes command text, so
// Command mirrors Name — pickerCommandRows' dimmed second line then shows the
// name again instead of stale or wrong text. Falls back to this host's local
// presets when the remote is unreachable or predates the /presets route.
func remoteCommandPresetsForPicker(host string) ([]CommandPreset, error) {
	if names, err := fetchRemotePresets(host); err == nil && len(names) > 0 {
		presets := make([]CommandPreset, len(names))
		for i, name := range names {
			presets[i] = CommandPreset{Name: name, Command: name}
		}
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
		"cwd":     cwd,
		"command": preset.Name,
		"prompt":  prompt,
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
	sshTarget := srv.EffectiveSSHTarget()
	c.enterRaw()
	_ = c.runInteractive("ssh", "-t", sshTarget, "tmux", "attach", "-t", r.Tmux)
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
