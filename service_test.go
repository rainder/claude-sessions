package main

import (
	"os"
	"path/filepath"
	"strings"
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
		{name: "child directory name starting with dotdot is not an escape", p: "/var/tmp/..foo/exe", dir: "/var/tmp", want: true},
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
// macOS: a raw string prefix check against os.TempDir() misses it. The
// relationship that matters is asymmetric — an exe path under the *resolved*
// dir is NOT within the *unresolved* link, which is exactly why
// resolveBinPath has to resolve the temp dir before comparing.
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
	exeUnderResolved := filepath.Join(resolved, "exe")
	if pathWithin(exeUnderResolved, link) {
		t.Errorf("pathWithin(%q, %q) = true, want false (unresolved link path must not match a resolved exe path)", exeUnderResolved, link)
	}
	if !pathWithin(exeUnderResolved, resolved) {
		t.Errorf("pathWithin(%q, %q) = false, want true", exeUnderResolved, resolved)
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

// The `go test` binary itself runs from under a temp dir (os.TempDir(), or
// GOTMPDIR when set), so resolveBinPath must refuse it — that refusal is the
// entire point of the function. Skipped if GOTMPDIR points somewhere other
// than os.TempDir(), since that would move the test binary's build output out
// from under the directory this test can observe.
func TestResolveBinPathRefusesTempDir(t *testing.T) {
	if gotmp := os.Getenv("GOTMPDIR"); gotmp != "" {
		resolvedGotmp, err := filepath.EvalSymlinks(gotmp)
		if err != nil {
			t.Fatalf("EvalSymlinks(GOTMPDIR=%q): %v", gotmp, err)
		}
		resolvedTemp, err := filepath.EvalSymlinks(os.TempDir())
		if err != nil {
			t.Fatalf("EvalSymlinks(os.TempDir()): %v", err)
		}
		if resolvedGotmp != resolvedTemp {
			t.Skipf("GOTMPDIR=%q points outside os.TempDir(); premise doesn't hold here", gotmp)
		}
	}

	_, err := resolveBinPath()
	if err == nil {
		t.Fatal("resolveBinPath() = nil error, want an error (the go test binary runs from a temp dir)")
	}
	if !strings.Contains(err.Error(), "temporary") {
		t.Errorf("resolveBinPath() error = %q, want it to mention the temp directory", err.Error())
	}
}
