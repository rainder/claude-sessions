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

// LoadSortMode reads the persisted sort mode ("dir", "status", "created",
// "created-asc", "updated", "updated-asc"). Defaults to "dir" on any error or
// unrecognized value.
func LoadSortMode() string {
	data, err := os.ReadFile(filepath.Join(ConfigDir(), "sort-mode"))
	if err != nil {
		return "dir"
	}
	switch v := strings.TrimSpace(string(data)); v {
	case "dir", "status", "created", "created-asc", "updated", "updated-asc":
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

// commandPresetIndex returns the index of the preset named remembered, or 0
// (the default first preset) if remembered is empty, stale, or presets is empty.
func commandPresetIndex(presets []CommandPreset, remembered string) int {
	for i, preset := range presets {
		if preset.Name == remembered {
			return i
		}
	}
	return 0
}

// LoadCommandPresetIndex reads the persisted command preset name and resolves
// it against presets. Defaults to 0 on any error or unrecognized value.
func LoadCommandPresetIndex(presets []CommandPreset) int {
	data, err := os.ReadFile(filepath.Join(ConfigDir(), "command-preset"))
	if err != nil {
		return 0
	}
	return commandPresetIndex(presets, strings.TrimSpace(string(data)))
}

// SaveCommandPresetName persists the remembered command preset name.
// Best-effort, like SaveViewMode.
func SaveCommandPresetName(name string) {
	dir := ConfigDir()
	if dir == "" {
		return
	}
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "command-preset"), []byte(name+"\n"), 0o644)
}
