package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// Persistent memo for the resume collector's two expensive per-file reads.
//
// collectResumableLimited's cheap pass is stat-only and already discards most
// of a busy host's transcripts; what is left costs a head scan (up to
// resumePromptsScanLines JSON unmarshals) plus a full-file line count, and both
// used to run again from scratch on every single picker open. Nothing about a
// transcript's head changes while the file does not, so the answer is cached on
// disk and a warm open re-reads no transcript contents at all.
//
// The key is (path, mtime, size), never the session id. Ids are not unique per
// file — a worktree move leaves two transcripts carrying one id — and the
// collector's rule is "emit the newest *content-valid* transcript per id", so an
// id-keyed entry would let one file's cached answer stand in for another's. A
// path key with the file's own mtime and size makes every edit, truncation or
// replacement a miss by construction: there is no invalidation logic to get
// wrong.
//
// What is cached is readResumableHead's answer, which is also what every
// *content* rejection is derived from (no cwd, a scratch cwd, an agent
// transcript), so a rejected file short-circuits on the next open exactly like
// an accepted one — the point of the cache, since rejections outnumber
// acceptances among the files pass 2 actually opens. A candidate skipped by the
// dedupe check (`emitted[sid]`) is a different thing: it never reaches the cache
// and never gets an entry, because it was never read either. Dedupe stays purely
// per-pass state, which is what keeps the fall-back-to-an-older-valid-copy rule
// (TestCollectResumableFallsBackToOlderValidTranscript) untouched by caching.

// resumableCacheVersion is the on-disk format version. A change to the entry
// shape bumps it, and the whole file is then ignored once and rewritten — the
// data is a memo of files that are all still on disk, so there is nothing worth
// migrating.
const resumableCacheVersion = 1

// resumableCachePath is the cache file, derived from the same home the
// collector globs under rather than os.UserHomeDir(). That is what keeps the
// collector's tests hermetic: each temp home carries its own cache, so a test
// is cold unless it deliberately runs a second pass.
func resumableCachePath(home string) string {
	return filepath.Join(home, ".claude", ".resumable-cache.json")
}

// resumableCacheEntry is one transcript's memoized head plus, when it was ever
// needed, its line count. Only a successful read is ever stored (see head's
// doc comment for why a failed open is never cached). Lines is -1 for "not
// computed": a file whose head was read but which was then rejected never had
// its lines counted, and a later pass that does accept it (its newer
// duplicate having gone away) must still be able to fill that in without
// invalidating the head beside it.
type resumableCacheEntry struct {
	MTimeUnixNano int64    `json:"mtime"`
	Size          int64    `json:"size"`
	CWD           string   `json:"cwd,omitempty"`
	GitBranch     string   `json:"git_branch,omitempty"`
	FirstPrompt   string   `json:"first_prompt,omitempty"`
	Prompts       []string `json:"prompts,omitempty"`
	Summary       string   `json:"summary,omitempty"`
	Sidechain     bool     `json:"sidechain,omitempty"`
	Entrypoint    string   `json:"entrypoint,omitempty"`
	Lines         int      `json:"lines"`
}

// current reports whether the entry still describes the file it was computed
// from. Any write to a transcript moves its mtime, and any truncation or rewrite
// its size; either one is a miss.
func (e resumableCacheEntry) current(mtime time.Time, size int64) bool {
	return e.MTimeUnixNano == mtime.UnixNano() && e.Size == size
}

func (e resumableCacheEntry) modTime() time.Time {
	return time.Unix(0, e.MTimeUnixNano)
}

// head rebuilds the resumableHead the entry was created from.
func (e resumableCacheEntry) head() resumableHead {
	return resumableHead{
		cwd:         e.CWD,
		gitBranch:   e.GitBranch,
		firstPrompt: e.FirstPrompt,
		prompts:     copyPrompts(e.Prompts),
		summary:     e.Summary,
		sidechain:   e.Sidechain,
		entrypoint:  e.Entrypoint,
	}
}

// copyPrompts keeps the cache and the rows it feeds from sharing a backing
// array. readResumableHead allocates a fresh slice per call, so before the cache
// existed every ResumableSession.Prompts was uniquely owned; an entry handing
// out its own slice would outlive the row through save and marshal whatever the
// row's owner did to it. Nothing mutates it today — this preserves the
// invariant rather than fixing a live bug, and three lines is cheaper than
// depending on that staying true.
func copyPrompts(p []string) []string {
	if len(p) == 0 {
		return nil
	}
	out := make([]string, len(p))
	copy(out, p)
	return out
}

// newResumableCacheEntry captures one successful head scan's answer for
// (mtime, size), with the line count still unknown.
func newResumableCacheEntry(head resumableHead, mtime time.Time, size int64) resumableCacheEntry {
	return resumableCacheEntry{
		MTimeUnixNano: mtime.UnixNano(),
		Size:          size,
		CWD:           head.cwd,
		GitBranch:     head.gitBranch,
		FirstPrompt:   head.firstPrompt,
		Prompts:       copyPrompts(head.prompts),
		Summary:       head.summary,
		Sidechain:     head.sidechain,
		Entrypoint:    head.entrypoint,
		Lines:         -1,
	}
}

// cachedResumable is the on-disk envelope. Entries are keyed by absolute
// transcript path, so the key never has to be repeated inside the value.
type cachedResumable struct {
	Version int                            `json:"version"`
	Entries map[string]resumableCacheEntry `json:"entries"`
}

// resumableCache is one collector pass's view of that file: loaded at the start
// of the pass, written back at the end if anything changed. It is deliberately
// per-call state with no locking — the local TUI and the /resumable handler each
// run their own pass, and the file is a best-effort memo, not a source of truth
// (see save for what a lost update costs).
type resumableCache struct {
	entries map[string]resumableCacheEntry
	dirty   bool
}

// loadResumableCache reads the cache file, always returning a usable cache: an
// absent, unreadable, corrupt or version-mismatched file just means a cold pass
// that rewrites it.
func loadResumableCache(home string) *resumableCache {
	c := &resumableCache{entries: map[string]resumableCacheEntry{}}
	data, err := os.ReadFile(resumableCachePath(home))
	if err != nil {
		return c
	}
	var f cachedResumable
	if err := json.Unmarshal(data, &f); err != nil || f.Version != resumableCacheVersion {
		return c
	}
	for path, e := range f.Entries {
		if path == "" {
			continue
		}
		if e.Lines < -1 { // a hand-edited or truncated count means "unknown"
			e.Lines = -1
		}
		c.entries[path] = e
	}
	return c
}

// head answers readResumableHeadFn for path, reading the file only when no
// entry describes it at this exact (mtime, size). A failed read is deliberately
// NOT cached: os.Open can fail transiently (a permission fix-up, EIO, an
// interrupted read on a networked home) on a file whose mtime and size never
// change again once the session ends, so a stored OK=false would hide that
// session from the picker for up to resumableMaxAge with no way to self-heal —
// a strictly worse outcome than the one failed syscall a retry costs.
func (c *resumableCache) head(path string, mtime time.Time, size int64) (resumableHead, bool) {
	if e, ok := c.entries[path]; ok && e.current(mtime, size) {
		return e.head(), true
	}
	head, ok := readResumableHeadFn(path)
	if ok {
		c.entries[path] = newResumableCacheEntry(head, mtime, size)
		c.dirty = true
	}
	return head, ok
}

// lineCount answers countFileLines for path from the entry head() just created
// or reused, counting the file only the first time a pass actually needs the
// number. A count computed against an entry that has since moved out from under
// it (a concurrent write between the two calls) is returned but not stored: the
// entry it would attach to no longer describes the file it was measured from.
func (c *resumableCache) lineCount(path string, mtime time.Time, size int64) int {
	e, ok := c.entries[path]
	if ok && e.current(mtime, size) && e.Lines >= 0 {
		return e.Lines
	}
	n := countFileLines(path)
	if ok && e.current(mtime, size) {
		e.Lines = n
		c.entries[path] = e
		c.dirty = true
	}
	return n
}

// save prunes and persists the cache, best-effort. seen is every path this
// pass's glob returned that still stats — recorded *before* the cheap pass's
// filters, since a zero-byte or currently-live transcript is still a file whose
// memo must survive — so an entry outside it names a transcript that has been
// deleted or moved. That, plus dropping entries whose own mtime has fallen past
// the collector's resumableMaxAge cutoff, is the whole size bound: the cache can
// only ever hold transcripts inside the window that some pass actually read.
// (The prune set being "everything this glob returned" is only safe because one
// glob covers the whole corpus — narrowing it would evict the rest of the cache
// on every pass.)
//
// Nothing is written when the pass neither read a file nor pruned one, so a
// steady-state picker open leaves the file alone entirely. The write is
// temp-file-then-rename in the same directory (the pattern patchIdentityCache
// uses), so a reader never sees a half-written file and two racing passes cannot
// interleave into a corrupt one. No lock: the loser of a rename race simply
// loses its new entries, which costs one more scan of those files on some later
// pass.
func (c *resumableCache) save(home string, seen map[string]bool, cutoff time.Time) {
	pruned := false
	for path, e := range c.entries {
		if !seen[path] || e.modTime().Before(cutoff) {
			delete(c.entries, path)
			pruned = true
		}
	}
	if !c.dirty && !pruned {
		return
	}
	data, err := json.Marshal(cachedResumable{Version: resumableCacheVersion, Entries: c.entries})
	if err != nil {
		return
	}
	dir := filepath.Dir(resumableCachePath(home))
	tmp, err := os.CreateTemp(dir, ".resumable-cache-*.tmp")
	if err != nil {
		return
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return
	}
	if err := os.Rename(name, resumableCachePath(home)); err != nil {
		os.Remove(name)
	}
}
