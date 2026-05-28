package main

import (
	"os"
	"strconv"
	"strings"
)

// Minimal YAML parser for our specific config shape:
//
//	servers:
//	  - name: foo
//	    host: bar
//	    port: 8765
//	    token: ...
//	    ssh_host: ...    (optional, defaults to host)
//	    ssh_user: ...    (optional)
//	    enable: false    (optional; default true — false hides the entry)
//
// No nested structures, no flow style, no multiline scalars, no anchors.
// PyYAML this isn't — but it's enough for the config format we ship.

// ServerConfig is one entry under `servers:`.
type ServerConfig struct {
	Name    string
	Host    string
	Port    int
	Token   string
	SSHHost string // optional, defaults to Host
	SSHUser string // optional, defaults to ssh's own default (usually $USER)
	Enable  bool   // optional, defaults to true; false drops the entry from LoadServerConfigs
}

// EffectiveSSHTarget returns "user@host" if SSHUser is set, else just "host".
// Used as the destination for `ssh -t <target> tmux attach -t <session>`.
// Without a user, ssh would default to the local username — which usually
// owns no tmux sessions on the remote, producing "no sessions".
func (s ServerConfig) EffectiveSSHTarget() string {
	host := s.SSHHost
	if host == "" {
		host = s.Host
	}
	if s.SSHUser != "" {
		return s.SSHUser + "@" + host
	}
	return host
}

// LoadServerConfigs reads ~/.config/claude-sessions/servers.yaml. Returns an
// empty slice (no error) when the file doesn't exist. Disabled entries
// (enable: false) are dropped here so callers never see them.
func LoadServerConfigs() ([]ServerConfig, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	path := home + "/.config/claude-sessions/servers.yaml"
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	all := parseServersYAML(string(data))
	enabled := all[:0]
	for _, s := range all {
		if s.Enable {
			enabled = append(enabled, s)
		}
	}
	return enabled, nil
}

func parseServersYAML(s string) []ServerConfig {
	var out []ServerConfig
	var current *ServerConfig
	inServers := false

	for _, raw := range strings.Split(s, "\n") {
		line := strings.TrimRight(raw, " \t\r")
		stripped := strings.TrimLeft(line, " \t")
		if stripped == "" || strings.HasPrefix(stripped, "#") {
			continue
		}
		indent := len(line) - len(stripped)

		// Top-level key — toggle in/out of the servers block.
		if indent == 0 && strings.Contains(line, ":") {
			key := strings.TrimSpace(strings.SplitN(line, ":", 2)[0])
			inServers = (key == "servers")
			continue
		}
		if !inServers {
			continue
		}

		if strings.HasPrefix(stripped, "- ") {
			if current != nil {
				out = append(out, *current)
			}
			current = &ServerConfig{Port: 8765, Enable: true}
			kv := strings.TrimSpace(stripped[2:])
			if k, v, ok := strings.Cut(kv, ":"); ok {
				setField(current, strings.TrimSpace(k), trimYAMLValue(v))
			}
			continue
		}
		if current != nil {
			if k, v, ok := strings.Cut(stripped, ":"); ok {
				setField(current, strings.TrimSpace(k), trimYAMLValue(v))
			}
		}
	}
	if current != nil {
		out = append(out, *current)
	}
	return out
}

func trimYAMLValue(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
	}
	return v
}

func setField(s *ServerConfig, key, val string) {
	switch key {
	case "name":
		s.Name = val
	case "host":
		s.Host = val
	case "port":
		if n, err := strconv.Atoi(val); err == nil {
			s.Port = n
		}
	case "token":
		s.Token = val
	case "ssh_host":
		s.SSHHost = val
	case "ssh_user":
		s.SSHUser = val
	case "enable":
		switch strings.ToLower(val) {
		case "false", "no", "off", "0":
			s.Enable = false
		case "true", "yes", "on", "1":
			s.Enable = true
		}
	}
}
