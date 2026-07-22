package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// cwdEntry is one row in the new-session picker.
type cwdEntry struct {
	cwd       string
	count     int
	isDefault bool
}

// cwdSuggestion is a ranked cwd candidate collected from local session/transcript
// history. Serialized over the /cwd-suggestions endpoint for remote pickers.
type cwdSuggestion struct {
	CWD   string `json:"cwd"`
	Count int    `json:"count"`
}

// cwdPicker holds the picker rows plus a precomputed home prefix for short
// display.
type cwdPicker struct {
	entries []cwdEntry
	home    string
}

func (p *cwdPicker) shortName(cwd string) string {
	return collapseHome(cwd, p.home)
}

// collapseHome shortens path by replacing a leading home prefix with "~". A
// blank home (unknown, e.g. a remote host that didn't report one) leaves path
// untouched — never collapse everything onto a zero-value prefix. Shared by the
// local picker's shortName and the remote picker's display.
func collapseHome(path, home string) string {
	if home != "" && strings.HasPrefix(path, home) {
		return "~" + strings.TrimPrefix(path, home)
	}
	return path
}

// collectCwdSuggestions gathers ranked cwd candidates from this host's session
// JSONs and project transcript dirs. Frequency is the count of session files
// plus transcript files pointing at that cwd. Non-existent, hidden, and empty
// cwds are dropped; the list is sorted count-descending / path-ascending and
// capped at 15. Shared by the local picker and the /cwd-suggestions endpoint.
func collectCwdSuggestions() []cwdSuggestion {
	home, _ := os.UserHomeDir()
	counts := map[string]int{}
	if home == "" {
		return nil
	}

	// Live + stale session JSONs (authoritative cwd values).
	matches, _ := filepath.Glob(filepath.Join(home, ".claude", "sessions", "*.json"))
	for _, path := range matches {
		s, ok := readSessionFile(path)
		if ok && s.CWD != "" && !hiddenCwd(s.CWD) && isDir(s.CWD) {
			counts[s.CWD]++
		}
	}

	// History from project transcript dirs. Pull cwd from the first JSONL
	// entry that has one — naive `-`→`/` decoding mangles hyphenated names.
	projects := filepath.Join(home, ".claude", "projects")
	ents, _ := os.ReadDir(projects)
	for _, entry := range ents {
		if !entry.IsDir() {
			continue
		}
		jsonls, _ := filepath.Glob(filepath.Join(projects, entry.Name(), "*.jsonl"))
		if len(jsonls) == 0 {
			continue
		}
		cwd := extractCWDFromJSONL(jsonls[0])
		if cwd != "" && !hiddenCwd(cwd) && isDir(cwd) && counts[cwd] < len(jsonls) {
			counts[cwd] = len(jsonls)
		}
	}

	out := make([]cwdSuggestion, 0, len(counts))
	for cwd, count := range counts {
		out = append(out, cwdSuggestion{CWD: cwd, Count: count})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].CWD < out[j].CWD
	})
	if len(out) > 15 {
		out = out[:15]
	}
	return out
}

// buildCwdPicker assembles the list of cwd suggestions, ordered by frequency
// descending (from collectCwdSuggestions), with the local $PWD appended if
// not already present.
func buildCwdPicker(selected *Session) cwdPicker {
	home, _ := os.UserHomeDir()
	p := cwdPicker{home: home}

	suggestions := collectCwdSuggestions()

	seen := map[string]bool{}
	for _, sg := range suggestions {
		if seen[sg.CWD] {
			continue
		}
		p.entries = append(p.entries, cwdEntry{cwd: sg.CWD, count: sg.Count})
		seen[sg.CWD] = true
	}

	// Always offer $PWD if not already present.
	if pwd, err := os.Getwd(); err == nil && !seen[pwd] && !hiddenCwd(pwd) && isDir(pwd) {
		p.entries = append(p.entries, cwdEntry{cwd: pwd})
	}

	if len(p.entries) > 15 {
		p.entries = p.entries[:15]
	}
	return p
}

// mergeRemoteCwdEntries turns remote-collected cwd suggestions into picker
// rows, in their existing (frequency-ranked) order — no special-cased first
// row. Unlike buildCwdPicker it applies no isDir/hiddenCwd filtering — the
// paths live on the remote host, so local existence checks are meaningless.
func mergeRemoteCwdEntries(defaultCWD string, suggestions []cwdSuggestion) []cwdEntry {
	entries := make([]cwdEntry, 0, len(suggestions)+1)
	seen := map[string]bool{}
	for _, suggestion := range suggestions {
		if suggestion.CWD == "" || seen[suggestion.CWD] {
			continue
		}
		entries = append(entries, cwdEntry{cwd: suggestion.CWD, count: suggestion.Count})
		seen[suggestion.CWD] = true
	}
	return entries
}

// extractCWDFromJSONL reads up to the first 20 lines of a JSONL transcript and
// returns the cwd field of the first entry that has one. "" if not found.
func extractCWDFromJSONL(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for i := 0; scanner.Scan() && i < 20; i++ {
		var e struct {
			CWD string `json:"cwd"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &e); err == nil && e.CWD != "" {
			return e.CWD
		}
	}
	return ""
}

// hiddenCwd reports cwds that are never worth suggesting: everything under
// /private, which on macOS is where scratchpads and /tmp (a symlink to
// /private/tmp) live. The selected row's own cwd bypasses this — it's an
// explicit context, not a suggestion.
func hiddenCwd(cwd string) bool {
	return strings.HasPrefix(cwd, "/private/") || cwd == "/private"
}

// isDir returns true if path is a directory.
func isDir(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// expandTilde expands a leading ~ to $HOME.
func expandTilde(p string) string {
	if !strings.HasPrefix(p, "~") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}

// isInsideTmux returns true when $TMUX is set, meaning we're already in a
// tmux client (so attaches should switch-client instead of nesting).
func isInsideTmux() (bool, error) {
	return os.Getenv("TMUX") != "", nil
}
