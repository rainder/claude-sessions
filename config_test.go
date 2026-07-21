package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadSortModeMissing(t *testing.T) {
	// No config dir at all => default "dir".
	t.Setenv("HOME", t.TempDir())
	if got := LoadSortMode(); got != "dir" {
		t.Errorf("LoadSortMode() with no file = %q, want %q", got, "dir")
	}
}

func TestLoadSortModeGarbage(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".config", "claude-sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sort-mode"), []byte("nonsense\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := LoadSortMode(); got != "dir" {
		t.Errorf("LoadSortMode() with garbage value = %q, want %q", got, "dir")
	}
}

func TestLoadSortModeValid(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	SaveSortMode("updated")
	if got := LoadSortMode(); got != "updated" {
		t.Errorf("LoadSortMode() after SaveSortMode = %q, want %q", got, "updated")
	}
}
