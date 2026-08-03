package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestSummaryCacheHitAvoidsRefetch(t *testing.T) {
	c := newSummaryCache(time.Hour, time.Minute, time.Second, 10)
	var calls int32
	fetch := func(ctx context.Context) (PreviewResult, error) {
		atomic.AddInt32(&calls, 1)
		return PreviewResult{Content: "result"}, nil
	}
	for i := 0; i < 3; i++ {
		got, err := c.getOrFetch(context.Background(), "k1", fetch)
		if err != nil || got.Content != "result" {
			t.Fatalf("call %d: got (%+v, %v)", i, got, err)
		}
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("fetch called %d times, want 1", n)
	}
}

func TestSummaryCacheDifferentKeysDontShare(t *testing.T) {
	c := newSummaryCache(time.Hour, time.Minute, time.Second, 10)
	var calls int32
	fetch := func(ctx context.Context) (PreviewResult, error) {
		atomic.AddInt32(&calls, 1)
		return PreviewResult{Content: "result"}, nil
	}
	c.getOrFetch(context.Background(), "k1", fetch)
	c.getOrFetch(context.Background(), "k2", fetch)
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Errorf("fetch called %d times, want 2", n)
	}
}

func TestSummaryCacheExpiredEntryRefetches(t *testing.T) {
	c := newSummaryCache(time.Millisecond, time.Millisecond, time.Second, 10)
	var calls int32
	fetch := func(ctx context.Context) (PreviewResult, error) {
		atomic.AddInt32(&calls, 1)
		return PreviewResult{Content: "result"}, nil
	}
	c.getOrFetch(context.Background(), "k1", fetch)
	time.Sleep(5 * time.Millisecond)
	c.getOrFetch(context.Background(), "k1", fetch)
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Errorf("fetch called %d times, want 2 after expiry", n)
	}
}

func TestSummaryCacheEvictsOverCapacity(t *testing.T) {
	c := newSummaryCache(time.Hour, time.Minute, time.Second, 2)
	fetch := func(ctx context.Context) (PreviewResult, error) {
		return PreviewResult{Content: "result"}, nil
	}
	// k1, k2, k3 are inserted in order, each with the same successTTL, so
	// their expires timestamps increase with insertion order. When k3's
	// insertion pushes the cache over capacity, prune evicts the
	// completed entry with the soonest expiry — k1, the oldest — leaving
	// the two most-recently-inserted keys, k2 and k3.
	c.getOrFetch(context.Background(), "k1", fetch)
	c.getOrFetch(context.Background(), "k2", fetch)
	c.getOrFetch(context.Background(), "k3", fetch)
	c.mu.Lock()
	n := len(c.entries)
	_, hasK1 := c.entries["k1"]
	_, hasK2 := c.entries["k2"]
	_, hasK3 := c.entries["k3"]
	c.mu.Unlock()
	if n != 2 {
		t.Fatalf("cache has %d entries, want exactly 2 (bounded)", n)
	}
	if hasK1 {
		t.Errorf("k1 (oldest, soonest to expire) should have been evicted, but survived")
	}
	if !hasK2 || !hasK3 {
		t.Errorf("k2 and k3 (most recently inserted) should have survived, got hasK2=%v hasK3=%v", hasK2, hasK3)
	}
}

func TestSummaryCachePruneNeverEvictsInFlightEntries(t *testing.T) {
	c := newSummaryCache(time.Hour, time.Minute, time.Second, 2)
	start := make(chan struct{})
	fetch := func(ctx context.Context) (PreviewResult, error) {
		<-start
		return PreviewResult{Content: "result"}, nil
	}
	done := make(chan struct{}, 4)
	keys := []string{"k1", "k2", "k3", "k4"}
	for _, k := range keys {
		k := k
		go func() {
			c.getOrFetch(context.Background(), k, fetch)
			done <- struct{}{}
		}()
	}
	time.Sleep(10 * time.Millisecond) // let all 4 reach getOrFetch and register in-flight
	c.mu.Lock()
	c.prune()
	n := len(c.entries)
	c.mu.Unlock()
	if n != 4 {
		t.Errorf("prune evicted in-flight entries: cache has %d entries, want 4 (max=%d should not apply while all in-flight)", n, c.max)
	}
	close(start)
	for range keys {
		<-done
	}
}

func TestSummaryCacheConcurrentCallersJoinOneFlight(t *testing.T) {
	c := newSummaryCache(time.Hour, time.Minute, time.Second, 10)
	var calls int32
	start := make(chan struct{})
	fetch := func(ctx context.Context) (PreviewResult, error) {
		atomic.AddInt32(&calls, 1)
		<-start
		return PreviewResult{Content: "result"}, nil
	}
	done := make(chan struct{}, 5)
	for i := 0; i < 5; i++ {
		go func() {
			c.getOrFetch(context.Background(), "k1", fetch)
			done <- struct{}{}
		}()
	}
	time.Sleep(10 * time.Millisecond) // let all 5 reach getOrFetch
	close(start)
	for i := 0; i < 5; i++ {
		<-done
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("fetch called %d times, want 1 (single-flight)", n)
	}
}
