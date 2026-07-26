package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// APNsConfig is ~/.config/claude-sessions/apns.yaml — the credentials this host
// uses to push directly to Apple. Flat top-level keys only: yaml.go's parser
// has no nested-structure support and this file deliberately does not warrant
// one.
//
// It lives apart from servers.yaml on purpose. servers.yaml is the *client's*
// host list; this is a server-side secret and the two must not share a file.
type APNsConfig struct {
	KeyFile     string
	KeyID       string
	TeamID      string
	BundleID    string
	Environment string // "production" (default) or "sandbox"
}

// Validate reports the first missing or nonsensical field. The server logs this
// once at startup and continues with notifications disabled — a bad config is
// never fatal.
func (c APNsConfig) Validate() error {
	switch {
	case c.KeyFile == "":
		return fmt.Errorf("apns.yaml: key_file is required")
	case c.KeyID == "":
		return fmt.Errorf("apns.yaml: key_id is required")
	case c.TeamID == "":
		return fmt.Errorf("apns.yaml: team_id is required")
	case c.BundleID == "":
		return fmt.Errorf("apns.yaml: bundle_id is required")
	}
	switch c.Environment {
	case "", "production", "sandbox":
		return nil
	}
	return fmt.Errorf("apns.yaml: environment must be production or sandbox, got %q", c.Environment)
}

// apnsHost maps an environment to Apple's gateway. Empty means production, so a
// device that registers without declaring one lands on the live gateway.
func apnsHost(environment string) string {
	if environment == "sandbox" {
		return "api.sandbox.push.apple.com"
	}
	return "api.push.apple.com"
}

// parseAPNsYAML reads flat `key: value` lines, ignoring blanks, comments, and
// any indented line (there are no nested structures in this file).
func parseAPNsYAML(input string) APNsConfig {
	var c APNsConfig
	for _, raw := range strings.Split(input, "\n") {
		line := strings.TrimRight(raw, " \t\r")
		if line == "" || strings.HasPrefix(strings.TrimLeft(line, " \t"), "#") {
			continue
		}
		if line != strings.TrimLeft(line, " \t") {
			continue // indented: not a top-level key
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		// Strip a trailing comment, so `production # or sandbox` yields
		// "production" rather than the whole tail — but only on unquoted values.
		// A quoted value may legitimately contain a '#', and truncating it would
		// silently produce a wrong credential that Validate cannot catch, since
		// it only checks for emptiness.
		if !strings.HasPrefix(val, `"`) && !strings.HasPrefix(val, "'") {
			if i := strings.Index(val, " #"); i >= 0 {
				val = val[:i]
			}
		}
		val = trimYAMLValue(val)
		switch key {
		case "key_file":
			c.KeyFile = expandTilde(val)
		case "key_id":
			c.KeyID = val
		case "team_id":
			c.TeamID = val
		case "bundle_id":
			c.BundleID = val
		case "environment":
			c.Environment = val
		}
	}
	if c.Environment == "" {
		c.Environment = "production"
	}
	return c
}

// LoadAPNsConfig reads and validates apns.yaml. Every error path here means
// "run without notifications", never "fail to start".
func LoadAPNsConfig() (APNsConfig, error) {
	dir := ConfigDir()
	if dir == "" {
		return APNsConfig{}, fmt.Errorf("apns: no config directory")
	}
	data, err := os.ReadFile(filepath.Join(dir, "apns.yaml"))
	if err != nil {
		return APNsConfig{}, fmt.Errorf("apns: %w", err)
	}
	cfg := parseAPNsYAML(string(data))
	if err := cfg.Validate(); err != nil {
		return APNsConfig{}, err
	}
	return cfg, nil
}

// hostIDLen is the hex length of a host id (16 random bytes).
const hostIDLen = 32

// LoadHostID returns this host's stable identifier, generating and persisting
// one on first use.
//
// Session IDs are not host-qualified anywhere server-side, so every identifier
// that can cross hosts — APNs collapse IDs, event IDs, and phase C's action
// targets — is keyed on host_id + session_id. Returns "" when there is no
// config directory to persist to; callers degrade rather than invent an
// unstable ID.
func LoadHostID() string {
	dir := ConfigDir()
	if dir == "" {
		return ""
	}
	path := filepath.Join(dir, "host-id")
	if id := readHostID(path); id != "" {
		return id
	}
	buf := make([]byte, hostIDLen/2)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	id := hex.EncodeToString(buf)
	// A host id that silently fails to persist is worse than useless: it looks
	// stable within one process and changes on every restart, so collapse IDs
	// and event IDs stop matching across restarts. Complain loudly rather than
	// letting it degrade quietly.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "claude-sessions: cannot persist host id (%v) — it will change on restart\n", err)
		return id
	}
	// O_EXCL, so two processes reaching first-use together do not each write a
	// different id and disagree about this host's identity. The loser reads the
	// winner's value instead of its own.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if os.IsExist(err) {
		// Either another process just won the race, or the file holds something
		// that is not an id. Prefer theirs; otherwise overwrite the garbage,
		// because leaving it would regenerate a different id on every call —
		// unstable in exactly the way this function exists to prevent.
		if existing := readHostID(path); existing != "" {
			return existing
		}
		f, err = os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "claude-sessions: cannot persist host id (%v) — it will change on restart\n", err)
		return id
	}
	if _, err := f.WriteString(id + "\n"); err != nil {
		fmt.Fprintf(os.Stderr, "claude-sessions: cannot persist host id (%v) — it will change on restart\n", err)
	}
	if err := f.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "claude-sessions: cannot persist host id (%v) — it will change on restart\n", err)
	}
	return id
}

// readHostID returns a persisted host id, or "" if the file is missing or holds
// something that is not a host id. Length alone is not enough: a truncated or
// hand-edited file can be the right size and still be garbage.
func readHostID(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	id := strings.TrimSpace(string(data))
	if len(id) != hostIDLen {
		return ""
	}
	if _, err := hex.DecodeString(id); err != nil {
		return ""
	}
	return id
}
