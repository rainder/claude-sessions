package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseAPNsYAML(t *testing.T) {
	in := `
# leading comment
key_file: ~/.config/claude-sessions/AuthKey_ABC123.p8
key_id: "ABC123DEFG"
team_id: XYZ9876543
bundle_id: com.avisoma.claude-sessions
environment: sandbox
`
	got := parseAPNsYAML(in)
	if got.KeyID != "ABC123DEFG" {
		t.Fatalf("KeyID = %q, want %q", got.KeyID, "ABC123DEFG")
	}
	if got.TeamID != "XYZ9876543" {
		t.Fatalf("TeamID = %q, want %q", got.TeamID, "XYZ9876543")
	}
	if got.BundleID != "com.avisoma.claude-sessions" {
		t.Fatalf("BundleID = %q, want %q", got.BundleID, "com.avisoma.claude-sessions")
	}
	if got.Environment != "sandbox" {
		t.Fatalf("Environment = %q, want %q", got.Environment, "sandbox")
	}
	if strings.HasPrefix(got.KeyFile, "~") {
		t.Fatalf("KeyFile = %q, want tilde expanded", got.KeyFile)
	}
}

// A trailing comment after a value is normal in a hand-written config and must
// not end up inside the value.
func TestParseAPNsYAMLStripsTrailingComment(t *testing.T) {
	got := parseAPNsYAML("environment: production   # or sandbox\nkey_id: A\n")
	if got.Environment != "production" {
		t.Fatalf("Environment = %q, want %q", got.Environment, "production")
	}
	if got.KeyID != "A" {
		t.Fatalf("KeyID = %q, want %q", got.KeyID, "A")
	}
}

func TestParseAPNsYAMLDefaultsEnvironment(t *testing.T) {
	got := parseAPNsYAML("key_id: A\nteam_id: B\nbundle_id: C\nkey_file: /k.p8\n")
	if got.Environment != "production" {
		t.Fatalf("Environment = %q, want %q", got.Environment, "production")
	}
}

// Indented lines are not top-level keys. yaml.go has no nested-structure
// support and this file deliberately has no nesting, so an indented key is a
// mistake and must be ignored rather than silently accepted.
func TestParseAPNsYAMLIgnoresIndentedKeys(t *testing.T) {
	got := parseAPNsYAML("apns:\n  key_id: NESTED\n")
	if got.KeyID != "" {
		t.Fatalf("KeyID = %q, want empty for an indented key", got.KeyID)
	}
}

func TestAPNsConfigValidate(t *testing.T) {
	full := APNsConfig{KeyFile: "/k.p8", KeyID: "A", TeamID: "B", BundleID: "C", Environment: "production"}
	tests := []struct {
		name    string
		mutate  func(*APNsConfig)
		wantErr string
	}{
		{"complete", func(*APNsConfig) {}, ""},
		{"missing key file", func(c *APNsConfig) { c.KeyFile = "" }, "key_file"},
		{"missing key id", func(c *APNsConfig) { c.KeyID = "" }, "key_id"},
		{"missing team id", func(c *APNsConfig) { c.TeamID = "" }, "team_id"},
		{"missing bundle id", func(c *APNsConfig) { c.BundleID = "" }, "bundle_id"},
		{"bad environment", func(c *APNsConfig) { c.Environment = "staging" }, "environment"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := full
			tt.mutate(&cfg)
			err := cfg.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() = %v, want error mentioning %q", err, tt.wantErr)
			}
		})
	}
}

func TestAPNsHost(t *testing.T) {
	tests := []struct{ env, want string }{
		{"production", "api.push.apple.com"},
		{"", "api.push.apple.com"},
		{"sandbox", "api.sandbox.push.apple.com"},
	}
	for _, tt := range tests {
		if got := apnsHost(tt.env); got != tt.want {
			t.Fatalf("apnsHost(%q) = %q, want %q", tt.env, got, tt.want)
		}
	}
}

// A missing apns.yaml is the normal state for a host that does not push. It
// must be an error the caller can log and continue past, never a panic.
func TestLoadAPNsConfigMissingFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if _, err := LoadAPNsConfig(); err == nil {
		t.Fatalf("LoadAPNsConfig() = nil error for a missing file, want one")
	}
}

func TestLoadAPNsConfigReadsAndValidates(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".config", "claude-sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := "key_file: /k.p8\nkey_id: A\nteam_id: B\nbundle_id: C\n"
	if err := os.WriteFile(filepath.Join(dir, "apns.yaml"), []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := LoadAPNsConfig()
	if err != nil {
		t.Fatalf("LoadAPNsConfig: %v", err)
	}
	if got.KeyID != "A" || got.Environment != "production" {
		t.Fatalf("config = %+v", got)
	}
}

func TestLoadAPNsConfigRejectsIncomplete(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".config", "claude-sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "apns.yaml"), []byte("key_id: A\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := LoadAPNsConfig(); err == nil {
		t.Fatalf("LoadAPNsConfig() = nil error for an incomplete config, want one")
	}
}

func TestLoadHostIDIsStable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	first := LoadHostID()
	if len(first) != 32 {
		t.Fatalf("host id = %q, want 32 hex chars", first)
	}
	if second := LoadHostID(); second != first {
		t.Fatalf("host id changed across calls: %q then %q", first, second)
	}
	path := filepath.Join(home, ".config", "claude-sessions", "host-id")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read host-id: %v", err)
	}
	if strings.TrimSpace(string(data)) != first {
		t.Fatalf("persisted host id = %q, want %q", strings.TrimSpace(string(data)), first)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("host-id mode = %o, want 600", perm)
	}
}

// A truncated or hand-mangled host-id file must be replaced, not returned.
func TestLoadHostIDReplacesGarbage(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".config", "claude-sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "host-id"), []byte("nope\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := LoadHostID()
	if len(got) != 32 {
		t.Fatalf("host id = %q, want a regenerated 32-char id", got)
	}
}
