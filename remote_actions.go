package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// Remote action helpers — invoked from the TUI when the selected row is on a
// configured server. Mirror the local actions, but talk to the server's HTTP
// API and SSH for the interactive attach.

// remoteRequest performs an HTTP request to the named server. body is JSON if
// non-empty. Returns the response body or an error.
func remoteRequest(name, path, method string, body []byte) ([]byte, error) {
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
	client := &http.Client{Timeout: 30 * time.Second}
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

// actKillRemote handles `k` on a remote-selected row.
func actKillRemote(c *actCtx) {
	s := c.selected()
	if s == nil {
		return
	}
	host, pid := s.Host, s.PID
	enterCooked(c.fd, c.oldState)
	defer enterRaw(c.fd)

	if !confirm(fmt.Sprintf("\nkill PID %d on %s? [y/N] ", pid, host)) {
		fmt.Println("\naborted")
		pauseForKey(c.fd, c.oldState)
		return
	}
	fmt.Print("\nsending remote kill... ")
	resp, err := remoteRequest(host, fmt.Sprintf("/sessions/%d/kill", pid), "POST", []byte(`{}`))
	if err != nil {
		fmt.Printf("failed: %v\n", err)
	} else {
		var r actionResult
		_ = json.Unmarshal(resp, &r)
		if r.OK {
			fmt.Println("ok")
		} else {
			fmt.Printf("failed: %s\n", r.Error)
		}
	}
	pauseForKey(c.fd, c.oldState)
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
		enterRaw(c.fd)
		return
	}
	sshTarget := srv.EffectiveSSHTarget()

	// Fetch tmux info.
	resp, err := remoteRequest(host, fmt.Sprintf("/sessions/%d/tmux-info", pid), "GET", nil)
	if err != nil {
		enterCooked(c.fd, c.oldState)
		fmt.Printf("\ntmux-info failed: %v\n", err)
		pauseForKey(c.fd, c.oldState)
		enterRaw(c.fd)
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
			fmt.Println("\naborted")
			pauseForKey(c.fd, c.oldState)
			enterRaw(c.fd)
			return
		}
		fmt.Print("\nmigrating... ")
		mresp, merr := remoteRequest(host, fmt.Sprintf("/sessions/%d/migrate", pid), "POST", []byte(`{}`))
		if merr != nil {
			fmt.Printf("failed: %v\n", merr)
			pauseForKey(c.fd, c.oldState)
			enterRaw(c.fd)
			return
		}
		var r actionResult
		_ = json.Unmarshal(mresp, &r)
		if !r.OK || r.Tmux == "" {
			fmt.Printf("failed: %s\n", r.Error)
			pauseForKey(c.fd, c.oldState)
			enterRaw(c.fd)
			return
		}
		tname = r.Tmux
		fmt.Printf("ok → %s\n", tname)
		enterRaw(c.fd)
	}

	// SSH into the host and attach to the tmux session.
	_ = runInteractive(c.fd, c.oldState, "ssh", "-t", sshTarget, "tmux", "attach", "-t", tname)
}

// actPreviewRemote shows the remote /preview output in a loop.
func actPreviewRemote(c *actCtx, interval time.Duration) {
	s := c.selected()
	if s == nil {
		return
	}
	host, pid := s.Host, s.PID

	render := func() {
		fmt.Print("\033[H\033[J")
		fmt.Printf("%sPreview: PID %d on %s%s  %s(q/p=back · r=refresh · auto-refresh %s)%s\n\n",
			ansiBold, pid, host, ansiReset, ansiDim, interval, ansiReset)
		resp, err := remoteRequest(host, fmt.Sprintf("/sessions/%d/preview", pid), "GET", nil)
		if err != nil {
			fmt.Printf("preview failed: %v\n", err)
			return
		}
		_, _ = os.Stdout.Write(resp)
	}
	render()
	nextTick := time.Now().Add(interval)
	for {
		timeout := time.Until(nextTick)
		if timeout <= 0 {
			render()
			nextTick = time.Now().Add(interval)
			continue
		}
		events := readEvents(timeout)
		if len(events) == 0 {
			render()
			nextTick = time.Now().Add(interval)
			continue
		}
		for _, k := range events {
			switch k {
			case "q", "Q", "p", "P", KeyEsc, "\x03":
				return
			case "r", "R":
				render()
			}
		}
	}
}

// actNewRemote prompts for a cwd (default = selected remote row's cwd) and
// POSTs /sessions/new to the remote server. Then SSH-attaches to the result.
func actNewRemote(c *actCtx) {
	s := c.selected()
	if s == nil {
		return
	}
	host := s.Host
	defaultCWD := s.CWD

	enterCooked(c.fd, c.oldState)
	defer enterRaw(c.fd)

	fmt.Printf("\n%sNew tmux+claude session on %s%s\n\n", ansiBold, host, ansiReset)
	if defaultCWD != "" {
		fmt.Printf("  default cwd: %s\n\n", defaultCWD)
	}
	input := readLine("cwd (Enter=default, q=cancel) > ")
	switch input {
	case "q", "Q":
		return
	case "":
		input = defaultCWD
	}
	if input == "" {
		fmt.Println("\nno cwd")
		pauseForKey(c.fd, c.oldState)
		return
	}

	fmt.Printf("\nspawning on %s in %s... ", host, input)
	body, _ := json.Marshal(map[string]string{"cwd": input})
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
	enterRaw(c.fd)
	_ = runInteractive(c.fd, c.oldState, "ssh", "-t", sshTarget, "tmux", "attach", "-t", r.Tmux)
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
