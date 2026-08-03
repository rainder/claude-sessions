package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestSummaryCacheHitAvoidsRefetch(t *testing.T) {
	c := newSummaryCache(time.Hour, 10)
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
	c := newSummaryCache(time.Hour, 10)
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
	c := newSummaryCache(time.Millisecond, 10)
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
	c := newSummaryCache(time.Hour, 2)
	fetch := func(ctx context.Context) (PreviewResult, error) {
		return PreviewResult{Content: "result"}, nil
	}
	c.getOrFetch(context.Background(), "k1", fetch)
	c.getOrFetch(context.Background(), "k2", fetch)
	c.getOrFetch(context.Background(), "k3", fetch)
	c.mu.Lock()
	n := len(c.entries)
	c.mu.Unlock()
	if n > 2 {
		t.Errorf("cache has %d entries, want <= 2 (bounded)", n)
	}
}

func TestSummaryCacheConcurrentCallersJoinOneFlight(t *testing.T) {
	c := newSummaryCache(time.Hour, 10)
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
