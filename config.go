package main

import (
	"os"
	"path/filepath"
	"strings"
)

// ConfigDir is ~/.config/claude-sessions. Created lazily by writers.
func ConfigDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "claude-sessions")
}

// LoadViewMode reads the persisted view mode ("1" full, "2" minimal,
// "3" intermediate). Defaults to "1" on any error or unrecognized value.
func LoadViewMode() string {
	data, err := os.ReadFile(filepath.Join(ConfigDir(), "view-mode"))
	if err != nil {
		return "1"
	}
	v := strings.TrimSpace(string(data))
	if v == "1" || v == "2" || v == "3" {
		return v
	}
	return "1"
}

// SaveViewMode persists the view mode. Best-effort: errors are swallowed
// because a stale or unwritable config dir shouldn't break the live view.
func SaveViewMode(mode string) {
	dir := ConfigDir()
	if dir == "" {
		return
	}
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "view-mode"), []byte(mode+"\n"), 0o644)
}

// LoadSortMode reads the persisted sort mode ("dir", "created", "created-asc",
// "updated", "updated-asc"). Defaults to "dir" on any error or unrecognized value.
func LoadSortMode() string {
	data, err := os.ReadFile(filepath.Join(ConfigDir(), "sort-mode"))
	if err != nil {
		return "dir"
	}
	switch v := strings.TrimSpace(string(data)); v {
	case "dir", "created", "created-asc", "updated", "updated-asc":
		return v
	}
	return "dir"
}

// SaveSortMode persists the sort mode. Best-effort, like SaveViewMode.
func SaveSortMode(mode string) {
	dir := ConfigDir()
	if dir == "" {
		return
	}
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "sort-mode"), []byte(mode+"\n"), 0o644)
}
