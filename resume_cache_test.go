package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// countHeadScans installs a counting wrapper over readResumableHeadFn — the same
// seam TestCollectResumableStopsScanningAtTheLimit uses — and returns the
// counter. The cache sits in front of this seam, so the number it reports is
// transcript reads that actually happened: zero is the whole claim of a warm
// pass.
func countHeadScans(t *testing.T) *int {
	t.Helper()
	var scans int
	real := readResumableHeadFn
	readResumableHeadFn = func(path string) (resumableHead, bool) {
		scans++
		return real(path)
	}
	t.Cleanup(func() { readResumableHeadFn = real })
	return &scans
}

// writeCachedTranscript writes a plain, valid, acceptable transcript.
func writeCachedTranscript(t *testing.T, home, sid, cwd, prompt string, mtime time.Time) string {
	t.Helper()
	return writeResumableTranscript(t, home, "proj", sid, mtime,
		`{"type":"attachment","cwd":"`+cwd+`","gitBranch":"main"}`,
		`{"type":"user","message":{"role":"user","content":"`+prompt+`"}}`,
	)
}

func TestResumableCacheWarmPassReadsNoTranscripts(t *testing.T) {
	home := t.TempDir()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)

	for i := 0; i < 4; i++ {
		writeCachedTranscript(t, home, fmt.Sprintf("warm%04d", i), "/home/u/proj",
			fmt.Sprintf("prompt %d", i), now.Add(-time.Duration(i+1)*time.Minute))
	}

	scans := countHeadScans(t)
	cold := collectResumableFrom(home, nil, now)
	if len(cold) != 4 {
		t.Fatalf("cold pass collected %d sessions %v, want 4", len(cold), ids(cold))
	}
	if *scans != 4 {
		t.Fatalf("cold head scans = %d, want 4", *scans)
	}

	*scans = 0
	warm := collectResumableFrom(home, nil, now)
	if *scans != 0 {
		t.Fatalf("warm head scans = %d, want 0 (the cache is not being used)", *scans)
	}
	if len(warm) != len(cold) {
		t.Fatalf("warm pass collected %d sessions %v, want %d", len(warm), ids(warm), len(cold))
	}
	for i := range cold {
		if warm[i].SessionID != cold[i].SessionID || warm[i].CWD != cold[i].CWD ||
			warm[i].GitBranch != cold[i].GitBranch || warm[i].FirstPrompt != cold[i].FirstPrompt ||
			warm[i].MessageCount != cold[i].MessageCount || !warm[i].ModifiedAt.Equal(cold[i].ModifiedAt) {
			t.Fatalf("row %d differs between passes:\n cold %+v\n warm %+v", i, cold[i], warm[i])
		}
	}
}

// TestResumableCacheServesFromDiskNotMemory proves the warm answers really come
// out of the cache file rather than being recomputed: between the two passes the
// transcript's *contents* are replaced with something that would parse to a
// rejection and a different line count, while its mtime and size — the whole key
// — are left exactly as they were. A collector that re-read the file would drop
// the row; one reading its memo returns the original head and count. That also
// covers countFileLines, which has no seam of its own.
func TestResumableCacheServesFromDiskNotMemory(t *testing.T) {
	home := t.TempDir()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	mtime := now.Add(-1 * time.Hour)

	path := writeCachedTranscript(t, home, "frozen01", "/home/u/orig", "original prompt", mtime)

	cold := collectResumableFrom(home, nil, now)
	if len(cold) != 1 || cold[0].CWD != "/home/u/orig" || cold[0].MessageCount != 2 {
		t.Fatalf("cold pass = %+v, want one row for /home/u/orig with 2 messages", cold)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Same byte count, no parseable line, a different number of newlines.
	if err := os.WriteFile(path, []byte(strings.Repeat("\n", int(info.Size()))), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}

	scans := countHeadScans(t)
	warm := collectResumableFrom(home, nil, now)
	if *scans != 0 {
		t.Fatalf("warm head scans = %d, want 0", *scans)
	}
	if len(warm) != 1 {
		t.Fatalf("warm pass collected %d sessions, want the cached row", len(warm))
	}
	if warm[0].CWD != "/home/u/orig" || warm[0].FirstPrompt != "original prompt" {
		t.Fatalf("warm row = %q / %q, want the cached head", warm[0].CWD, warm[0].FirstPrompt)
	}
	if warm[0].MessageCount != 2 {
		t.Fatalf("warm message count = %d, want the cached 2 (the line count was recomputed)", warm[0].MessageCount)
	}
}

// TestResumableCacheMissesOnMtimeOrSize pins both halves of the key. A file
// rewritten in place is a different file as far as the memo is concerned,
// whichever of the two the rewrite happened to move.
func TestResumableCacheMissesOnMtimeOrSize(t *testing.T) {
	t.Run("mtime", func(t *testing.T) {
		home := t.TempDir()
		now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
		mtime := now.Add(-1 * time.Hour)
		writeCachedTranscript(t, home, "mtimechg", "/home/u/aaaa", "prompt one", mtime)

		if got := collectResumableFrom(home, nil, now); len(got) != 1 || got[0].CWD != "/home/u/aaaa" {
			t.Fatalf("cold pass = %+v", got)
		}
		// Same length ("/home/u/bbbb" and "prompt two" match the originals), new mtime.
		writeCachedTranscript(t, home, "mtimechg", "/home/u/bbbb", "prompt two", mtime.Add(time.Minute))

		scans := countHeadScans(t)
		got := collectResumableFrom(home, nil, now)
		if *scans != 1 {
			t.Fatalf("head scans = %d, want 1 (a changed mtime must miss)", *scans)
		}
		if len(got) != 1 || got[0].CWD != "/home/u/bbbb" || got[0].FirstPrompt != "prompt two" {
			t.Fatalf("got %+v, want the rewritten transcript's contents", got)
		}
	})

	t.Run("size", func(t *testing.T) {
		home := t.TempDir()
		now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
		mtime := now.Add(-1 * time.Hour)
		writeCachedTranscript(t, home, "sizechng", "/home/u/aaaa", "prompt one", mtime)

		if got := collectResumableFrom(home, nil, now); len(got) != 1 {
			t.Fatalf("cold pass = %+v", got)
		}
		// Longer contents, mtime deliberately restored: only the size moved.
		writeCachedTranscript(t, home, "sizechng", "/home/u/aaaa-much-longer-path",
			"a considerably longer prompt than before", mtime)

		scans := countHeadScans(t)
		got := collectResumableFrom(home, nil, now)
		if *scans != 1 {
			t.Fatalf("head scans = %d, want 1 (a changed size must miss)", *scans)
		}
		if len(got) != 1 || got[0].CWD != "/home/u/aaaa-much-longer-path" {
			t.Fatalf("got %+v, want the rewritten transcript's contents", got)
		}
	})
}

// TestResumableCacheShortCircuitsRejections is the reason both outcomes are
// cached. Of the files pass 2 opens, most are rejected on their contents — no
// cwd, a scratch cwd, an agent transcript — and caching only the accepted rows
// would leave every open re-reading the larger half of the work.
func TestResumableCacheShortCircuitsRejections(t *testing.T) {
	home := t.TempDir()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)

	writeResumableTranscript(t, home, "proj", "nocwdaaa", now.Add(-1*time.Minute),
		`{"type":"queue-operation"}`,
		`{"type":"user","message":{"role":"user","content":"no cwd anywhere"}}`,
	)
	writeResumableTranscript(t, home, "proj", "scratchb", now.Add(-2*time.Minute),
		`{"type":"attachment","cwd":"/tmp/scratch"}`,
		`{"type":"user","message":{"role":"user","content":"temp work"}}`,
	)
	writeResumableTranscript(t, home, "proj", "sidechnc", now.Add(-3*time.Minute),
		`{"type":"user","isSidechain":true,"cwd":"/home/u/proj","message":{"role":"user","content":"subagent"}}`,
	)
	writeCachedTranscript(t, home, "goodrowd", "/home/u/proj", "keep me", now.Add(-4*time.Minute))

	scans := countHeadScans(t)
	cold := collectResumableFrom(home, nil, now)
	if len(cold) != 1 || cold[0].SessionID != "goodrowd" {
		t.Fatalf("cold pass = %v, want only goodrowd", ids(cold))
	}
	if *scans != 4 {
		t.Fatalf("cold head scans = %d, want 4 (every candidate is opened once)", *scans)
	}

	*scans = 0
	warm := collectResumableFrom(home, nil, now)
	if *scans != 0 {
		t.Fatalf("warm head scans = %d, want 0 — rejected files are being re-read", *scans)
	}
	if len(warm) != 1 || warm[0].SessionID != "goodrowd" {
		t.Fatalf("warm pass = %v, want only goodrowd", ids(warm))
	}
}

// TestResumableCacheFallbackRuleSurvivesCaching re-runs the dedupe fall-back
// case against a warm cache. A candidate skipped because a newer transcript
// already claimed its id is never read and never cached, so the "newest
// content-valid transcript per id" rule stays purely per-pass state.
func TestResumableCacheFallbackRuleSurvivesCaching(t *testing.T) {
	home := t.TempDir()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)

	writeResumableTranscript(t, home, "new", "dup99999", now.Add(-1*time.Hour),
		`{"type":"queue-operation"}`,
		`{"type":"user","message":{"role":"user","content":"newest copy, no cwd"}}`,
	)
	writeResumableTranscript(t, home, "old", "dup99999", now.Add(-3*time.Hour),
		`{"type":"attachment","cwd":"/home/u/old"}`,
		`{"type":"user","message":{"role":"user","content":"older but valid"}}`,
	)

	scans := countHeadScans(t)
	wantScans := map[string]int{"cold": 2, "warm": 0}
	for _, pass := range []string{"cold", "warm"} {
		*scans = 0
		got := collectResumableFrom(home, nil, now)
		if len(got) != 1 {
			t.Fatalf("%s pass: got %d sessions %v, want 1", pass, len(got), ids(got))
		}
		if got[0].CWD != "/home/u/old" || got[0].FirstPrompt != "older but valid" {
			t.Fatalf("%s pass: kept %q / %q, want the older valid transcript", pass, got[0].CWD, got[0].FirstPrompt)
		}
		// The newest copy's content rejection is cached exactly like an
		// acceptance would be (see resume_cache.go's doc comment on head), so
		// a warm pass reads neither file — proving this without a scan count
		// would let a silently-failed save (an unwritable ~/.claude, a
		// marshal error) leave both passes cold and the test still pass.
		if got, want := *scans, wantScans[pass]; got != want {
			t.Fatalf("%s pass: head scans = %d, want %d", pass, got, want)
		}
	}
}

// readResumableCacheFile decodes the persisted cache, failing the test if it is
// missing or unparseable.
func readResumableCacheFile(t *testing.T, home string) cachedResumable {
	t.Helper()
	data, err := os.ReadFile(resumableCachePath(home))
	if err != nil {
		t.Fatalf("reading cache file: %v", err)
	}
	var f cachedResumable
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("decoding cache file: %v", err)
	}
	if f.Version != resumableCacheVersion {
		t.Fatalf("cache version = %d, want %d", f.Version, resumableCacheVersion)
	}
	return f
}

func TestResumableCachePrunesDeletedAndAgedEntries(t *testing.T) {
	home := t.TempDir()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)

	gone := writeCachedTranscript(t, home, "deleted1", "/home/u/gone", "bye", now.Add(-1*time.Hour))
	kept := writeCachedTranscript(t, home, "kept0001", "/home/u/kept", "stay", now.Add(-2*time.Hour))

	if got := collectResumableFrom(home, nil, now); len(got) != 2 {
		t.Fatalf("cold pass collected %d sessions %v, want 2", len(got), ids(got))
	}
	if f := readResumableCacheFile(t, home); len(f.Entries) != 2 {
		t.Fatalf("cache holds %d entries, want 2", len(f.Entries))
	}

	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	collectResumableFrom(home, nil, now)
	f := readResumableCacheFile(t, home)
	if _, ok := f.Entries[gone]; ok {
		t.Fatalf("deleted transcript still cached: %v", f.Entries)
	}
	if _, ok := f.Entries[kept]; !ok {
		t.Fatalf("surviving transcript was evicted: %v", f.Entries)
	}

	// Every remaining transcript now sits past resumableMaxAge: the pass
	// collects nothing, and must still prune rather than keep the entries for as
	// long as the picker goes unused.
	collectResumableFrom(home, nil, now.Add(resumableMaxAge+time.Hour))
	if f := readResumableCacheFile(t, home); len(f.Entries) != 0 {
		t.Fatalf("aged-out entries survived: %v", f.Entries)
	}
}

// TestResumableCacheSteadyStateDoesNotRewrite pins the no-op rule: a pass that
// neither read a transcript nor pruned an entry leaves the file untouched, so
// two hosts' pollers are not renaming over each other for nothing.
func TestResumableCacheSteadyStateDoesNotRewrite(t *testing.T) {
	home := t.TempDir()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	writeCachedTranscript(t, home, "steady01", "/home/u/proj", "hello", now.Add(-1*time.Hour))

	collectResumableFrom(home, nil, now)
	before, err := os.Stat(resumableCachePath(home))
	if err != nil {
		t.Fatal(err)
	}
	// Backdate the cache file so any rewrite is unmistakable.
	old := now.Add(-99 * time.Hour)
	if err := os.Chtimes(resumableCachePath(home), old, old); err != nil {
		t.Fatal(err)
	}

	collectResumableFrom(home, nil, now)
	after, err := os.Stat(resumableCachePath(home))
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(old) {
		t.Fatalf("cache file was rewritten by a no-op pass (mtime %v → %v)", old, after.ModTime())
	}
	if after.Size() != before.Size() {
		t.Fatalf("cache file size changed on a no-op pass: %d → %d", before.Size(), after.Size())
	}
}

func TestResumableCacheIgnoresUnusableFile(t *testing.T) {
	home := t.TempDir()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	writeCachedTranscript(t, home, "corrupt1", "/home/u/proj", "hello", now.Add(-1*time.Hour))
	// One collector pass so ~/.claude exists to write into.
	collectResumableFrom(home, nil, now)

	for _, body := range []string{
		"not json at all",
		`{"version":9999,"entries":{}}`,
	} {
		if err := os.WriteFile(resumableCachePath(home), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		scans := countHeadScans(t)
		got := collectResumableFrom(home, nil, now)
		if *scans != 1 {
			t.Fatalf("%q: head scans = %d, want 1 (an unusable cache must read cold)", body, *scans)
		}
		if len(got) != 1 || got[0].CWD != "/home/u/proj" {
			t.Fatalf("%q: got %+v, want the transcript collected normally", body, got)
		}
		if f := readResumableCacheFile(t, home); len(f.Entries) != 1 {
			t.Fatalf("%q: cache was not rewritten: %v", body, f.Entries)
		}
	}
}

// TestResumableCacheColdVsWarmCorpus is the perf claim, on a corpus big enough
// for the cap to bite: a cold pass pays one head scan and one line count per
// candidate it walks, a warm pass pays none at all and only stats.
//
// The contract asserted is the scan counter, not the clock. Wall-clock is
// logged for the record but never failed on: it is the one number here that can
// go the wrong way for reasons unrelated to correctness (a loaded machine, a
// cold page cache, a badly placed GC), and "warm read zero transcripts" already
// says everything a timing comparison would, without a way to flake.
func TestResumableCacheColdVsWarmCorpus(t *testing.T) {
	home := t.TempDir()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	const corpus = 250

	// ~60 lines apiece, so a head scan and a line count both cost something
	// recognisable — the shape of a real transcript rather than a two-liner.
	body := make([]string, 0, 60)
	body = append(body,
		`{"type":"attachment","cwd":"/home/u/proj","gitBranch":"main"}`,
		`{"type":"user","message":{"role":"user","content":"first prompt"}}`,
	)
	for i := 0; i < 56; i++ {
		body = append(body, `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"`+
			strings.Repeat("filler ", 20)+`"}]}}`)
	}
	body = append(body,
		`{"type":"user","message":{"role":"user","content":"second prompt"}}`,
		`{"type":"user","message":{"role":"user","content":"third prompt"}}`,
	)
	for i := 0; i < corpus; i++ {
		writeResumableTranscript(t, home, "proj", fmt.Sprintf("corpus%03d", i),
			now.Add(-time.Duration(i+1)*time.Minute), body...)
	}

	scans := countHeadScans(t)
	start := time.Now()
	cold := collectResumableFrom(home, nil, now)
	coldDur := time.Since(start)
	coldScans := *scans
	if len(cold) != resumableMaxCount {
		t.Fatalf("cold pass collected %d sessions, want %d", len(cold), resumableMaxCount)
	}
	if coldScans != resumableMaxCount {
		t.Fatalf("cold head scans = %d, want %d", coldScans, resumableMaxCount)
	}

	*scans = 0
	start = time.Now()
	warm := collectResumableFrom(home, nil, now)
	warmDur := time.Since(start)
	if *scans != 0 {
		t.Fatalf("warm head scans = %d, want 0", *scans)
	}
	if len(warm) != len(cold) {
		t.Fatalf("warm pass collected %d sessions, want %d", len(warm), len(cold))
	}
	for i := range cold {
		if warm[i].SessionID != cold[i].SessionID || warm[i].CWD != cold[i].CWD ||
			warm[i].FirstPrompt != cold[i].FirstPrompt || warm[i].MessageCount != cold[i].MessageCount {
			t.Fatalf("row %d differs between passes:\n cold %+v\n warm %+v", i, cold[i], warm[i])
		}
	}

	t.Logf("corpus=%d cold=%v (%d head scans) warm=%v (0 head scans)",
		corpus, coldDur.Round(time.Millisecond), coldScans, warmDur.Round(time.Millisecond))
}
