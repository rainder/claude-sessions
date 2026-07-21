package main

import (
	"os"
	"path/filepath"
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

// CommandPreset is one entry under `commands:` — a named launch command
// offered when spawning a new session.
type CommandPreset struct {
	Name    string
	Command string
}

// appConfig is the fully parsed contents of servers.yaml.
type appConfig struct {
	Servers  []ServerConfig
	Commands []CommandPreset
}

// defaultCommandPresets is used when no (valid) commands: block is present.
func defaultCommandPresets() []CommandPreset {
	return []CommandPreset{{Name: "Claude", Command: "claude"}}
}

// findCommandPreset looks up a preset by exact name match.
func findCommandPreset(presets []CommandPreset, name string) (CommandPreset, bool) {
	for _, preset := range presets {
		if preset.Name == name {
			return preset, true
		}
	}
	return CommandPreset{}, false
}

// loadAppConfig reads ~/.config/claude-sessions/servers.yaml and parses both
// the servers: and commands: blocks. Missing file => default command presets,
// no servers.
func loadAppConfig() (appConfig, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return appConfig{}, err
	}
	data, err := os.ReadFile(filepath.Join(home, ".config", "claude-sessions", "servers.yaml"))
	if os.IsNotExist(err) {
		return appConfig{Commands: defaultCommandPresets()}, nil
	}
	if err != nil {
		return appConfig{}, err
	}
	return parseConfigYAML(string(data)), nil
}

// LoadCommandPresets returns the configured command presets, falling back to
// the default "Claude" preset when none are configured.
func LoadCommandPresets() ([]CommandPreset, error) {
	cfg, err := loadAppConfig()
	if err != nil {
		return nil, err
	}
	return cfg.Commands, nil
}

// LoadServerConfigs reads ~/.config/claude-sessions/servers.yaml. Returns an
// empty slice (no error) when the file doesn't exist. Disabled entries
// (enable: false) are dropped here so callers never see them.
func LoadServerConfigs() ([]ServerConfig, error) {
	cfg, err := loadAppConfig()
	if err != nil {
		return nil, err
	}
	enabled := cfg.Servers[:0]
	for _, s := range cfg.Servers {
		if s.Enable {
			enabled = append(enabled, s)
		}
	}
	return enabled, nil
}

// parseConfigYAML parses the top-level servers: and commands: blocks.
func parseConfigYAML(input string) appConfig {
	var cfg appConfig
	block := ""
	var server *ServerConfig
	var command *CommandPreset

	flushServer := func() {
		if server != nil {
			cfg.Servers = append(cfg.Servers, *server)
			server = nil
		}
	}
	flushCommand := func() {
		if command == nil {
			return
		}
		command.Name = strings.TrimSpace(command.Name)
		command.Command = strings.TrimSpace(command.Command)
		if command.Name != "" && command.Command != "" {
			if _, exists := findCommandPreset(cfg.Commands, command.Name); !exists {
				cfg.Commands = append(cfg.Commands, *command)
			}
		}
		command = nil
	}
	flush := func() {
		flushServer()
		flushCommand()
	}

	for _, raw := range strings.Split(input, "\n") {
		line := strings.TrimRight(raw, " \t\r")
		stripped := strings.TrimLeft(line, " \t")
		if stripped == "" || strings.HasPrefix(stripped, "#") {
			continue
		}
		indent := len(line) - len(stripped)
		if indent == 0 && strings.Contains(line, ":") {
			flush()
			block = strings.TrimSpace(strings.SplitN(line, ":", 2)[0])
			continue
		}
		if block != "servers" && block != "commands" {
			continue
		}
		if strings.HasPrefix(stripped, "- ") {
			flush()
			if block == "servers" {
				server = &ServerConfig{Port: 8765, Enable: true}
			} else {
				command = &CommandPreset{}
			}
			stripped = strings.TrimSpace(stripped[2:])
		}
		key, val, ok := strings.Cut(stripped, ":")
		if !ok {
			continue
		}
		key, val = strings.TrimSpace(key), trimYAMLValue(val)
		if block == "servers" && server != nil {
			setField(server, key, val)
		}
		if block == "commands" && command != nil {
			switch key {
			case "name":
				command.Name = val
			case "command":
				command.Command = val
			}
		}
	}
	flush()
	if len(cfg.Commands) == 0 {
		cfg.Commands = defaultCommandPresets()
	}
	return cfg
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
