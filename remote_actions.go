package main

import (
	"bytes"
	"encoding/json"
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

// remoteRequestWithTimeout performs an HTTP request to the named server with an
// explicit client timeout. body is JSON if non-empty. Returns the response body
// or an error.
func remoteRequestWithTimeout(name, path, method string, body []byte, timeout time.Duration) ([]byte, error) {
	srv, ok := LookupServer(name)
	if !ok {
		return nil, fmt.Errorf("unknown server: %s", name)
	}
	url := fmt.Sprintf("http://%s:%d%s", srv.Host, srv.Port, path)
	var bodyReader io.Reader
	if len(body) > 0 {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+srv.Token)
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return data, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return data, nil
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
		Content: string(data),
	}, nil
}

// fetchRemoteCwdSuggestions retrieves the ranked cwd history from the named
// server's /cwd-suggestions endpoint, using a short 5s timeout so a slow or
// unreachable host doesn't stall the picker.
func fetchRemoteCwdSuggestions(host string) ([]cwdSuggestion, error) {
	data, err := remoteRequestWithTimeout(host, "/cwd-suggestions", http.MethodGet, nil, 5*time.Second)
	if err != nil {
		return nil, err
	}
	var response struct {
		Suggestions []cwdSuggestion `json:"suggestions"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return nil, err
	}
	return response.Suggestions, nil
}

// actKillRemote handles `k` on a remote-selected row.
func actKillRemote(c *actCtx) {
	s := c.selected()
	if s == nil {
		return
	}
	host, pid := s.Host, s.PID
	enterCooked(c.fd, c.oldState)
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
		enterCooked(c.fd, c.oldState)
		fmt.Printf("\nunknown server: %s\n", host)
		pauseForKey(c.fd, c.oldState)
		c.enterRaw()
		return
	}
	sshTarget := srv.EffectiveSSHTarget()

	// Fetch tmux info.
	resp, err := remoteRequest(host, fmt.Sprintf("/sessions/%d/tmux-info", pid), "GET", nil)
	if err != nil {
		enterCooked(c.fd, c.oldState)
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
		enterCooked(c.fd, c.oldState)
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
// no isDir/hiddenCwd filtering — the paths live on the remote host.
func remoteNewRows(defaultCWD string, suggestions []cwdSuggestion) (lines []string, start int, entries []cwdEntry) {
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
		lines = append(lines, fmt.Sprintf("%-50s%s", entry.cwd, freq))
	}
	lines = append(lines, "enter path manually…")
	return lines, start, entries
}

// actNewRemote prompts for a cwd and POSTs /sessions/new to the named remote
// server. A populated remote row supplies defaultCWD; an empty host does not.
func actNewRemote(c *actCtx, host, defaultCWD string) {
	presets, err := LoadCommandPresets()
	if err != nil {
		fmt.Printf("\nload commands: %v\n", err)
		pauseForKey(c.fd, c.oldState)
		c.enterRaw()
		return
	}
	presetStart := LoadCommandPresetIndex(presets)

	// Fetch the remote host's cwd history for the picker. A slow or unreachable
	// host must not block manual entry, so on error we fall back to no
	// suggestions and surface a note in the modal.
	suggestions, err := fetchRemoteCwdSuggestions(host)
	note := ""
	if err != nil {
		suggestions = nil
		note = "remote suggestions unavailable"
	}
	lines, start, entries := remoteNewRows(defaultCWD, suggestions)

	row, presetIndex, ok := pickNewSession("New session on "+host, lines, start, presets, presetStart, note)
	if !ok {
		return
	}
	preset := presets[presetIndex]
	SaveCommandPresetName(preset.Name)

	enterCooked(c.fd, c.oldState)
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
