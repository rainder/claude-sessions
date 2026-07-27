package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPathWithin(t *testing.T) {
	tests := []struct {
		name string
		p    string
		dir  string
		want bool
	}{
		{name: "direct child", p: "/var/tmp/go-build123/exe", dir: "/var/tmp", want: true},
		{name: "identical path", p: "/var/tmp", dir: "/var/tmp", want: true},
		{name: "outside", p: "/usr/local/bin/claude-sessions", dir: "/var/tmp", want: false},
		{name: "sibling with shared prefix is not within", p: "/var/tmpfoo/x", dir: "/var/tmp", want: false},
		{name: "parent is not within child", p: "/var", dir: "/var/tmp", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pathWithin(tt.p, tt.dir); got != tt.want {
				t.Errorf("pathWithin(%q, %q) = %v, want %v", tt.p, tt.dir, got, tt.want)
			}
		})
	}
}

// The temp-dir guard must survive /tmp being a symlink to /private/tmp on
// macOS: a raw string prefix check against os.TempDir() misses it.
func TestPathWithinResolvesSymlinkedTempDir(t *testing.T) {
	realDir := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(link)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", link, err)
	}
	exe := filepath.Join(resolved, "exe")
	if !pathWithin(exe, resolved) {
		t.Errorf("pathWithin(%q, %q) = false, want true", exe, resolved)
	}
}

func TestCapturePathFallsBackWhenUnset(t *testing.T) {
	t.Setenv("PATH", "")
	got := capturePath()
	if got == "" {
		t.Fatal("capturePath() = \"\", want a non-empty fallback")
	}
}

func TestCapturePathUsesEnvironment(t *testing.T) {
	t.Setenv("PATH", "/opt/homebrew/bin:/usr/bin")
	if got, want := capturePath(), "/opt/homebrew/bin:/usr/bin"; got != want {
		t.Errorf("capturePath() = %q, want %q", got, want)
	}
}
