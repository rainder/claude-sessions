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
	for _, mode := range []string{"updated", "status"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			SaveSortMode(mode)
			if got := LoadSortMode(); got != mode {
				t.Errorf("LoadSortMode() after SaveSortMode = %q, want %q", got, mode)
			}
		})
	}
}

func TestCommandPresetIndex(t *testing.T) {
	presets := []CommandPreset{{Name: "Claude"}, {Name: "Fable"}}
	if got := commandPresetIndex(presets, "Fable"); got != 1 {
		t.Fatalf("valid remembered index = %d, want 1", got)
	}
	if got := commandPresetIndex(presets, "removed"); got != 0 {
		t.Fatalf("stale remembered index = %d, want 0", got)
	}
	if got := commandPresetIndex(nil, "Fable"); got != 0 {
		t.Fatalf("empty preset index = %d, want 0", got)
	}
}

func TestCommandPresetNameRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	SaveCommandPresetName("Fable")
	presets := []CommandPreset{{Name: "Claude"}, {Name: "Fable"}}
	if got := LoadCommandPresetIndex(presets); got != 1 {
		t.Fatalf("loaded preset index = %d, want 1", got)
	}
}

func TestLoadSummaryBackendMissing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if got := LoadSummaryBackend(); got != "claude" {
		t.Errorf("LoadSummaryBackend() with no file = %q, want %q", got, "claude")
	}
}

func TestLoadSummaryBackendGarbage(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".config", "claude-sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "summary-backend"), []byte("nonsense\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := LoadSummaryBackend(); got != "claude" {
		t.Errorf("LoadSummaryBackend() with garbage value = %q, want %q", got, "claude")
	}
}

func TestSummaryBackendRoundTrip(t *testing.T) {
	for _, backend := range []string{"claude", "codex"} {
		t.Run(backend, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			SaveSummaryBackend(backend)
			if got := LoadSummaryBackend(); got != backend {
				t.Errorf("LoadSummaryBackend() after SaveSummaryBackend(%q) = %q, want %q", backend, got, backend)
			}
		})
	}
}
