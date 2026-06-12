package main

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// findTranscript locates the JSONL transcript Claude Code writes for a
// session: ~/.claude/projects/<dir>/<sessionId>.jsonl. The dir is keyed to
// the cwd the session *started* in (encoded lossily), so it can't be derived
// from the session's current cwd — that goes stale the moment the session
// enters a git worktree. Session IDs are UUIDs, so a glob across all project
// dirs is unambiguous; if several dirs hold the same sid, the newest wins.
// Returns "" if no transcript exists. Resolutions are cached per sid and
// re-resolved when the cached path disappears.
func findTranscript(home, sid string) string {
	if sid == "" {
		return ""
	}
	transcriptMu.Lock()
	cached, ok := transcriptCache[sid]
	transcriptMu.Unlock()
	if ok {
		if _, err := os.Stat(cached); err == nil {
			return cached
		}
	}

	matches, err := filepath.Glob(filepath.Join(home, ".claude", "projects", "*", sid+".jsonl"))
	if err != nil || len(matches) == 0 {
		return ""
	}
	best, bestTime := "", time.Time{}
	for _, m := range matches {
		st, err := os.Stat(m)
		if err != nil || st.IsDir() {
			continue
		}
		if best == "" || st.ModTime().After(bestTime) {
			best, bestTime = m, st.ModTime()
		}
	}
	if best != "" {
		transcriptMu.Lock()
		transcriptCache[sid] = best
		transcriptMu.Unlock()
	}
	return best
}

var (
	transcriptMu    sync.Mutex
	transcriptCache = map[string]string{}
)

// shortModel compresses a model id for the table: drops the "claude-" prefix
// and a trailing -YYYYMMDD date stamp, keeping any "[...]" capability suffix.
//
//	claude-haiku-4-5-20251001 → haiku-4-5
//	claude-fable-5[1m]        → fable-5[1m]
func shortModel(id string) string {
	suffix := ""
	if i := strings.IndexByte(id, '['); i >= 0 {
		id, suffix = id[:i], id[i:]
	}
	id = strings.TrimPrefix(id, "claude-")
	if i := strings.LastIndexByte(id, '-'); i >= 0 && len(id)-i-1 == 8 {
		if _, err := time.Parse("20060102", id[i+1:]); err == nil {
			id = id[:i]
		}
	}
	return id + suffix
}

// modelTailBytes bounds how much of the transcript tail we scan per read.
const modelTailBytes = 256 * 1024

// modelFromTranscript returns the model id of the last main-loop (non-
// sidechain) assistant entry in the transcript, scanning only the file's
// tail. Returns "" on any error or if no such entry exists.
func modelFromTranscript(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	seeked := false
	if st, err := f.Stat(); err == nil && st.Size() > modelTailBytes {
		if _, err := f.Seek(st.Size()-modelTailBytes, io.SeekStart); err == nil {
			seeked = true
		}
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024) // some entries are huge
	if seeked {
		scanner.Scan() // discard the partial first line
	}
	model := ""
	for scanner.Scan() {
		var e struct {
			Type        string `json:"type"`
			IsSidechain bool   `json:"isSidechain"`
			Message     struct {
				Model string `json:"model"`
			} `json:"message"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			continue
		}
		if e.Type == "assistant" && !e.IsSidechain && e.Message.Model != "" {
			model = e.Message.Model
		}
	}
	return model
}

// cachedModel wraps modelFromTranscript with an mtime+size cache so the
// steady-state cost per refresh tick is one stat per session.
var (
	modelCacheMu sync.Mutex
	modelCache   = map[string]modelCacheEntry{}
)

type modelCacheEntry struct {
	mtime time.Time
	size  int64
	model string
}

func cachedModel(path string) string {
	st, err := os.Stat(path)
	if err != nil {
		return ""
	}
	modelCacheMu.Lock()
	e, ok := modelCache[path]
	modelCacheMu.Unlock()
	if ok && e.mtime.Equal(st.ModTime()) && e.size == st.Size() {
		return e.model
	}
	model := modelFromTranscript(path)
	modelCacheMu.Lock()
	modelCache[path] = modelCacheEntry{mtime: st.ModTime(), size: st.Size(), model: model}
	modelCacheMu.Unlock()
	return model
}
