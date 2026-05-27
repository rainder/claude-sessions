package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// RemoteResult is the per-host outcome of a /sessions poll.
type RemoteResult struct {
	Name     string    // server name from config
	Sessions []Session // empty when Error != ""
	Error    string    // "" on success, short reason otherwise
}

// FetchRemote queries one server's /sessions endpoint. 2s timeout.
func FetchRemote(srv ServerConfig) RemoteResult {
	if srv.Host == "" || srv.Token == "" {
		return RemoteResult{Name: srv.Name, Error: "config missing host or token"}
	}
	url := fmt.Sprintf("http://%s:%d/sessions", srv.Host, srv.Port)
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+srv.Token)

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return RemoteResult{Name: srv.Name, Error: shortErr(err)}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return RemoteResult{Name: srv.Name, Error: fmt.Sprintf("HTTP %d", resp.StatusCode)}
	}
	var body struct {
		Sessions []Session `json:"sessions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return RemoteResult{Name: srv.Name, Error: "bad response: " + shortErr(err)}
	}
	// Tag every session with the host name so ID() and rendering know.
	for i := range body.Sessions {
		body.Sessions[i].Host = srv.Name
	}
	return RemoteResult{Name: srv.Name, Sessions: body.Sessions}
}

// FetchAllRemote polls all configured servers in parallel and returns the
// results in config order. Returns nil when no servers are configured.
func FetchAllRemote() []RemoteResult {
	cfgs, err := LoadServerConfigs()
	if err != nil || len(cfgs) == 0 {
		return nil
	}
	results := make([]RemoteResult, len(cfgs))
	var wg sync.WaitGroup
	for i, c := range cfgs {
		i, c := i, c
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = FetchRemote(c)
		}()
	}
	wg.Wait()
	return results
}

// shortErr trims long error strings (URLError wrappers can be verbose).
func shortErr(err error) string {
	s := err.Error()
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 60 {
		s = s[:60]
	}
	return s
}

// AllSessions flattens local + every remote section into one ordered slice,
// matching how the renderer lays things out. Used for nav and selection
// validation.
func AllSessions(local []Session, remotes []RemoteResult) []Session {
	out := make([]Session, 0, len(local))
	out = append(out, local...)
	for _, r := range remotes {
		out = append(out, r.Sessions...)
	}
	return out
}

// LookupServer finds a configured server by name.
func LookupServer(name string) (ServerConfig, bool) {
	cfgs, _ := LoadServerConfigs()
	for _, c := range cfgs {
		if c.Name == name {
			return c, true
		}
	}
	return ServerConfig{}, false
}
