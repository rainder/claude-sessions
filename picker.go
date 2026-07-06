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

// cwdPicker holds the picker rows plus a precomputed home prefix for short
// display.
type cwdPicker struct {
	entries []cwdEntry
	home    string
}

func (p *cwdPicker) shortName(cwd string) string {
	if p.home != "" && strings.HasPrefix(cwd, p.home) {
		return "~" + strings.TrimPrefix(cwd, p.home)
	}
	return cwd
}

// buildCwdPicker assembles the list of cwd suggestions, ordered as: the
// selected row's cwd first, then by frequency descending. Frequency is the
// total count of session files + transcript files pointing at that cwd.
func buildCwdPicker(selected *Session) cwdPicker {
	home, _ := os.UserHomeDir()
	p := cwdPicker{home: home}

	counts := map[string]int{}

	// Live + stale session JSONs (authoritative cwd values).
	if home != "" {
		matches, _ := filepath.Glob(filepath.Join(home, ".claude", "sessions", "*.json"))
		for _, path := range matches {
			s, ok := readSessionFile(path)
			if ok && s.CWD != "" {
				counts[s.CWD]++
			}
		}
	}

	// History from project transcript dirs. Pull cwd from the first JSONL
	// entry that has one — naive `-`→`/` decoding mangles hyphenated names.
	if home != "" {
		projects := filepath.Join(home, ".claude", "projects")
		ents, _ := os.ReadDir(projects)
		for _, e := range ents {
			if !e.IsDir() {
				continue
			}
			full := filepath.Join(projects, e.Name())
			jsonls, _ := filepath.Glob(filepath.Join(full, "*.jsonl"))
			if len(jsonls) == 0 {
				continue
			}
			cwd := extractCWDFromJSONL(jsonls[0])
			if cwd != "" && isDir(cwd) && counts[cwd] < len(jsonls) {
				counts[cwd] = len(jsonls)
			}
		}
	}

	seen := map[string]bool{}
	// Selected row's cwd at the top.
	if selected != nil && selected.CWD != "" && isDir(selected.CWD) {
		p.entries = append(p.entries, cwdEntry{
			cwd:       selected.CWD,
			count:     counts[selected.CWD],
			isDefault: true,
		})
		seen[selected.CWD] = true
	}

	// Rest sorted by count desc, then by path for stability.
	type kv struct {
		cwd string
		n   int
	}
	rest := make([]kv, 0, len(counts))
	for c, n := range counts {
		if seen[c] {
			continue
		}
		rest = append(rest, kv{c, n})
	}
	sort.Slice(rest, func(i, j int) bool {
		if rest[i].n != rest[j].n {
			return rest[i].n > rest[j].n
		}
		return rest[i].cwd < rest[j].cwd
	})
	for _, kv := range rest {
		p.entries = append(p.entries, cwdEntry{cwd: kv.cwd, count: kv.n})
	}

	// Always offer $PWD if not already present.
	if pwd, err := os.Getwd(); err == nil && !seen[pwd] && isDir(pwd) {
		p.entries = append(p.entries, cwdEntry{cwd: pwd})
	}

	if len(p.entries) > 15 {
		p.entries = p.entries[:15]
	}
	return p
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
