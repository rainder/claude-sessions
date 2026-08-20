package main

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDetectTicketID(t *testing.T) {
	cases := []struct {
		name     string
		cwd      string
		sess     string
		worktree string
		want     string
	}{
		{"worktree basename match", "/Users/x/repo/.claude/worktrees/DR-860-fix-thing", "irrelevant", "", "DR-860"},
		{"session name fallback", "/Users/x/repo", "Fix DR-2213 login bug", "", "DR-2213"},
		{"inferred grok worktree when cwd is the main repo", "/Users/x/repo", "Ticket review", "DR-3141", "DR-3141"},
		{"cwd worktree takes precedence over inferred name", "/Users/x/repo/.claude/worktrees/DR-1", "mentions DR-999 too", "DR-3141", "DR-1"},
		{"worktree takes precedence over name", "/Users/x/repo/.claude/worktrees/DR-1", "mentions DR-999 too", "", "DR-1"},
		{"no match", "/Users/x/repo", "just a normal session", "", ""},
		{"rejects ADR prefix", "/Users/x/repo/.claude/worktrees/ADR-123", "ADR-123 design doc", "", ""},
		{"rejects trailing alnum suffix", "/Users/x/repo", "DR-123abc not a ticket", "", ""},
		{"accepts short and long digit runs", "/Users/x/repo", "DR-12 and DR-123456 both match, first wins", "", "DR-12"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := detectTicketID(c.cwd, c.sess, c.worktree); got != c.want {
				t.Errorf("detectTicketID(%q, %q, %q) = %q, want %q", c.cwd, c.sess, c.worktree, got, c.want)
			}
		})
	}
}

func TestFetchTicketSummarySuccess(t *testing.T) {
	prevCu, prevClaude := cuFetchFunc, claudeSummarizeFunc
	t.Cleanup(func() { cuFetchFunc, claudeSummarizeFunc = prevCu, prevClaude })

	cuFetchFunc = func(ctx context.Context, id string) ([]byte, error) {
		return []byte("raw ticket text"), nil
	}
	claudeSummarizeFunc = func(ctx context.Context, instruction string, input []byte) ([]byte, error) {
		if instruction != ticketSummaryInstruction {
			t.Errorf("instruction = %q", instruction)
		}
		if string(input) != "raw ticket text" {
			t.Errorf("input = %q", input)
		}
		return []byte("  short summary  \n"), nil
	}

	got, err := fetchTicketSummary(context.Background(), "DR-1")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got.Content != "short summary" {
		t.Errorf("Content = %q, want trimmed summary", got.Content)
	}
	if got.Label != "DR-1" {
		t.Errorf("Label = %q", got.Label)
	}
}

func TestFetchTicketSummaryCuFails(t *testing.T) {
	prevCu := cuFetchFunc
	t.Cleanup(func() { cuFetchFunc = prevCu })
	cuFetchFunc = func(ctx context.Context, id string) ([]byte, error) {
		return nil, errors.New("network down")
	}
	_, err := fetchTicketSummary(context.Background(), "DR-1")
	if err == nil {
		t.Fatal("want non-nil error when cu fetch fails")
	}
}

func TestFetchTicketSummaryClaudeFailsFallsBackToRaw(t *testing.T) {
	prevCu, prevClaude := cuFetchFunc, claudeSummarizeFunc
	t.Cleanup(func() { cuFetchFunc, claudeSummarizeFunc = prevCu, prevClaude })
	cuFetchFunc = func(ctx context.Context, id string) ([]byte, error) {
		return []byte("raw ticket text"), nil
	}
	claudeSummarizeFunc = func(ctx context.Context, instruction string, input []byte) ([]byte, error) {
		return nil, errors.New("rate limited")
	}
	got, err := fetchTicketSummary(context.Background(), "DR-1")
	if err != nil {
		t.Fatalf("err = %v, want nil (raw fallback is not an error)", err)
	}
	if !strings.Contains(got.Content, "raw ticket text") {
		t.Errorf("Content = %q, want it to contain the raw text", got.Content)
	}
	if !strings.Contains(got.Content, "summary unavailable") {
		t.Errorf("Content = %q, want a summary-unavailable note", got.Content)
	}
}

func TestFetchTicketSummaryRawFallbackTruncatesMultilineWithoutGutter(t *testing.T) {
	prevCu, prevClaude := cuFetchFunc, claudeSummarizeFunc
	t.Cleanup(func() { cuFetchFunc, claudeSummarizeFunc = prevCu, prevClaude })

	// Build multi-line raw text longer than ticketRawCap so the cap actually
	// bites, and so trunc's old "\n" -> "\n  │ " gutter rewrite (if it were
	// still in play) would be caught.
	var b strings.Builder
	for b.Len() <= ticketRawCap {
		b.WriteString("line of ticket text\n")
	}
	raw := b.String()

	cuFetchFunc = func(ctx context.Context, id string) ([]byte, error) {
		return []byte(raw), nil
	}
	claudeSummarizeFunc = func(ctx context.Context, instruction string, input []byte) ([]byte, error) {
		return nil, errors.New("rate limited")
	}

	got, err := fetchTicketSummary(context.Background(), "DR-1")
	if err != nil {
		t.Fatalf("err = %v, want nil (raw fallback is not an error)", err)
	}
	if strings.Contains(got.Content, "│") {
		t.Errorf("Content contains gutter marker │, want plain truncation: %q", got.Content)
	}
	// The kept raw prefix (before the dim "…(+N bytes)" suffix and the
	// "[summary unavailable...]" header) must not exceed ticketRawCap bytes.
	idx := strings.Index(got.Content, "\n\n")
	if idx < 0 {
		t.Fatalf("Content = %q, want a header/body separator", got.Content)
	}
	body := got.Content[idx+2:]
	cutIdx := strings.Index(body, "…(+")
	if cutIdx < 0 {
		t.Fatalf("Content body = %q, want a truncation marker", body)
	}
	// cutIdx includes the dim-marker's ANSI SGR escape (kept by
	// sanitizeTerminalText) plus the "  " before the ellipsis, so allow a
	// small fixed slack instead of an exact match.
	if cutIdx < ticketRawCap || cutIdx > ticketRawCap+16 {
		t.Errorf("truncated body length = %d, want ~%d (+small ANSI/space slack)", cutIdx, ticketRawCap)
	}
	if !strings.HasPrefix(body, raw[:ticketRawCap]) {
		t.Errorf("truncated body does not start with the expected raw prefix")
	}
}

func TestFetchTicketSummaryCachedAvoidsRefetch(t *testing.T) {
	prevCu, prevClaude := cuFetchFunc, claudeSummarizeFunc
	t.Cleanup(func() { cuFetchFunc, claudeSummarizeFunc = prevCu, prevClaude })
	prevCache := ticketCache
	t.Cleanup(func() { ticketCache = prevCache })
	var calls int
	cuFetchFunc = func(ctx context.Context, id string) ([]byte, error) {
		calls++
		return []byte("raw"), nil
	}
	claudeSummarizeFunc = func(ctx context.Context, instruction string, input []byte) ([]byte, error) {
		return []byte("summary"), nil
	}
	ticketCache = newSummaryCache(time.Hour, 15*time.Second, 20*time.Second, 64) // fresh cache, isolated from other tests
	fetchTicketSummaryCached(context.Background(), "DR-99")
	result, err := fetchTicketSummaryCached(context.Background(), "DR-99")
	if calls != 1 {
		t.Errorf("cu fetch called %d times, want 1", calls)
	}
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if result.Content != "summary" {
		t.Errorf("Content = %q, want %q", result.Content, "summary")
	}
}

// TestFetchTicketSummaryCachedDegradedUsesFailTTL proves a claude-leg
// failure (cu fetch succeeds, claude summarize fails) is cached under the
// short failTTL rather than the hour-long successTTL, so a rate-limited
// claude leg can be retried within seconds instead of being stuck showing
// the stale placeholder for an hour. It also proves the internal
// errTicketSummaryDegraded sentinel never leaks out of
// fetchTicketSummaryCached to its own caller.
func TestFetchTicketSummaryCachedDegradedUsesFailTTL(t *testing.T) {
	prevCu, prevClaude := cuFetchFunc, claudeSummarizeFunc
	t.Cleanup(func() { cuFetchFunc, claudeSummarizeFunc = prevCu, prevClaude })
	prevCache := ticketCache
	t.Cleanup(func() { ticketCache = prevCache })
	cuFetchFunc = func(ctx context.Context, id string) ([]byte, error) {
		return []byte("raw ticket text"), nil
	}
	claudeSummarizeFunc = func(ctx context.Context, instruction string, input []byte) ([]byte, error) {
		return nil, errors.New("rate limited")
	}
	const failTTL = 15 * time.Second
	ticketCache = newSummaryCache(time.Hour, failTTL, 20*time.Second, 64)

	result, err := fetchTicketSummaryCached(context.Background(), "DR-100")
	if err != nil {
		t.Fatalf("fetchTicketSummaryCached returned error %v, want nil (sentinel must not leak)", err)
	}
	if !strings.Contains(result.Content, "summary unavailable") {
		t.Errorf("Content = %q, want it to mention summary unavailable", result.Content)
	}

	entry, ok := ticketCache.entries["DR-100"]
	if !ok {
		t.Fatalf("expected a cache entry for DR-100")
	}
	wantExpires := time.Now().Add(failTTL)
	if diff := entry.expires.Sub(wantExpires); diff > 2*time.Second || diff < -2*time.Second {
		t.Errorf("entry.expires = %v, want close to %v (failTTL), got diff %v", entry.expires, wantExpires, diff)
	}
	if entry.expires.After(time.Now().Add(time.Hour - time.Minute)) {
		t.Errorf("entry.expires looks like successTTL (1h) was used instead of failTTL")
	}
}

// waitForTicketPrefetchIdle blocks until id's prefetch goroutine (if any) has
// finished and removed itself from ticketPrefetchInFlight, or fails the test
// after 2s.
func waitForTicketPrefetchIdle(t *testing.T, id string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := ticketPrefetchInFlight.Load(id); !ok {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("prefetch for %q never completed", id)
}

func TestPrefetchTicketSummarySkipsWhenFresh(t *testing.T) {
	prevCache := ticketCache
	t.Cleanup(func() { ticketCache = prevCache })
	ticketCache = newSummaryCache(time.Hour, 15*time.Second, 20*time.Second, 64)

	prevCu, prevClaude := cuFetchFunc, claudeSummarizeFunc
	t.Cleanup(func() { cuFetchFunc, claudeSummarizeFunc = prevCu, prevClaude })
	var calls int32
	cuFetchFunc = func(ctx context.Context, id string) ([]byte, error) {
		atomic.AddInt32(&calls, 1)
		return []byte("raw"), nil
	}
	claudeSummarizeFunc = func(ctx context.Context, instruction string, input []byte) ([]byte, error) {
		return []byte("summary"), nil
	}

	// Pre-warm the cache directly, bypassing prefetchTicketSummary.
	fetchTicketSummaryCached(context.Background(), "DR-500")
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("setup: cu fetch called %d times, want 1", n)
	}

	prefetchTicketSummary("DR-500") // fresh() should short-circuit before spawning anything
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("cu fetch called %d times after prefetching an already-fresh id, want still 1", n)
	}
}

func TestPrefetchTicketSummaryDedupesConcurrentCalls(t *testing.T) {
	prevCache := ticketCache
	t.Cleanup(func() { ticketCache = prevCache })
	ticketCache = newSummaryCache(time.Hour, 15*time.Second, time.Second, 64)

	prevCu, prevClaude := cuFetchFunc, claudeSummarizeFunc
	t.Cleanup(func() { cuFetchFunc, claudeSummarizeFunc = prevCu, prevClaude })
	var calls int32
	start := make(chan struct{})
	cuFetchFunc = func(ctx context.Context, id string) ([]byte, error) {
		atomic.AddInt32(&calls, 1)
		<-start
		return []byte("raw"), nil
	}
	claudeSummarizeFunc = func(ctx context.Context, instruction string, input []byte) ([]byte, error) {
		return []byte("summary"), nil
	}

	prefetchTicketSummary("DR-501")
	prefetchTicketSummary("DR-501")
	prefetchTicketSummary("DR-501")
	time.Sleep(10 * time.Millisecond) // let the first goroutine reach cuFetchFunc and block
	close(start)
	waitForTicketPrefetchIdle(t, "DR-501")

	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("cu fetch called %d times, want 1 (dedup across concurrent prefetch calls for the same id)", n)
	}
}

func TestPrefetchTicketSummaryRespectsConcurrencyCap(t *testing.T) {
	prevCache := ticketCache
	t.Cleanup(func() { ticketCache = prevCache })
	ticketCache = newSummaryCache(time.Hour, 15*time.Second, time.Second, 64)

	prevCu, prevClaude := cuFetchFunc, claudeSummarizeFunc
	t.Cleanup(func() { cuFetchFunc, claudeSummarizeFunc = prevCu, prevClaude })

	start := make(chan struct{})
	var current, peak int32
	cuFetchFunc = func(ctx context.Context, id string) ([]byte, error) {
		n := atomic.AddInt32(&current, 1)
		for {
			p := atomic.LoadInt32(&peak)
			if n <= p || atomic.CompareAndSwapInt32(&peak, p, n) {
				break
			}
		}
		<-start
		atomic.AddInt32(&current, -1)
		return []byte("raw"), nil
	}
	claudeSummarizeFunc = func(ctx context.Context, instruction string, input []byte) ([]byte, error) {
		return []byte("summary"), nil
	}

	ids := []string{"DR-601", "DR-602", "DR-603", "DR-604", "DR-605"}
	for _, id := range ids {
		prefetchTicketSummary(id)
	}
	time.Sleep(30 * time.Millisecond) // let everything that can start, start
	close(start)
	for _, id := range ids {
		waitForTicketPrefetchIdle(t, id)
	}

	if peak > ticketPrefetchConcurrency {
		t.Errorf("peak concurrent prefetches = %d, want <= %d", peak, ticketPrefetchConcurrency)
	}
}
