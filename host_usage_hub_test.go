package main

import (
	"context"
	"sync"
	"testing"
	"time"
)

type fakeHostUsageCollector struct {
	mu        sync.Mutex
	samples   []HostUsage
	calls     int
	active    int
	maxActive int
	started   chan struct{}
	block     bool
}

func (f *fakeHostUsageCollector) Sample(ctx context.Context) HostUsage {
	f.mu.Lock()
	index := f.calls
	f.calls++
	f.active++
	f.maxActive = max(f.maxActive, f.active)
	block := f.block && index > 0
	started := f.started
	var sample HostUsage
	if len(f.samples) > 0 {
		sample = f.samples[min(index, len(f.samples)-1)]
	}
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.active--
		f.mu.Unlock()
	}()

	if block {
		select {
		case started <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return HostUsage{}
	}
	return sample
}

func (f *fakeHostUsageCollector) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeHostUsageCollector) maxConcurrent() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxActive
}

func TestHostUsageHubPublishesInitialAndKickedSamples(t *testing.T) {
	collector := &fakeHostUsageCollector{samples: []HostUsage{
		{CPUPercent: floatPtr(10), MemoryPercent: floatPtr(20)},
		{CPUPercent: floatPtr(30), MemoryPercent: floatPtr(40)},
	}}
	hub := newHostUsageHubWithCollector(collector, time.Hour)
	defer hub.Shutdown()
	assertFloatPtr(t, hub.Snapshot().CPUPercent, floatPtr(10))

	hub.Kick()
	waitFor(t, time.Second, func() bool {
		return hub.Snapshot().CPUPercent != nil && *hub.Snapshot().CPUPercent == 30
	})
	assertFloatPtr(t, hub.Snapshot().MemoryPercent, floatPtr(40))
}

func TestHostUsageHubPauseResume(t *testing.T) {
	collector := &fakeHostUsageCollector{samples: []HostUsage{{CPUPercent: floatPtr(1)}}}
	hub := newHostUsageHubWithCollector(collector, time.Hour)
	defer hub.Shutdown()

	hub.Pause()
	hub.Kick()
	time.Sleep(20 * time.Millisecond)
	if got := collector.callCount(); got != 1 {
		t.Fatalf("calls while paused = %d, want 1", got)
	}
	hub.Resume()
	waitFor(t, time.Second, func() bool { return collector.callCount() >= 2 })
}

func TestHostUsageHubConcurrentSnapshots(t *testing.T) {
	collector := &fakeHostUsageCollector{samples: []HostUsage{{CPUPercent: floatPtr(50), MemoryPercent: floatPtr(60)}}}
	hub := newHostUsageHubWithCollector(collector, time.Hour)
	defer hub.Shutdown()

	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				_ = hub.Snapshot()
			}
		}()
	}
	wg.Wait()
}

func TestHostUsageHubSerializesCollectorCalls(t *testing.T) {
	collector := &fakeHostUsageCollector{samples: []HostUsage{{CPUPercent: floatPtr(10)}}}
	hub := newHostUsageHubWithCollector(collector, time.Millisecond)
	defer hub.Shutdown()
	waitFor(t, time.Second, func() bool { return collector.callCount() >= 3 })
	if got := collector.maxConcurrent(); got != 1 {
		t.Fatalf("max concurrent samples = %d, want 1", got)
	}
}

func TestHostUsageHubRetriesUnavailableSample(t *testing.T) {
	collector := &fakeHostUsageCollector{samples: []HostUsage{
		{},
		{CPUPercent: floatPtr(25)},
	}}
	hub := newHostUsageHubWithCollector(collector, time.Hour)
	defer hub.Shutdown()
	if hub.Snapshot().CPUPercent != nil {
		t.Fatal("initial CPU should be unavailable")
	}
	hub.Kick()
	waitFor(t, time.Second, func() bool { return hub.Snapshot().CPUPercent != nil })
	assertFloatPtr(t, hub.Snapshot().CPUPercent, floatPtr(25))
}

func TestHostUsageHubShutdownCancelsSample(t *testing.T) {
	collector := &fakeHostUsageCollector{
		samples: []HostUsage{{CPUPercent: floatPtr(1)}},
		started: make(chan struct{}, 1),
		block:   true,
	}
	hub := newHostUsageHubWithCollector(collector, time.Hour)
	hub.Kick()
	select {
	case <-collector.started:
	case <-time.After(time.Second):
		t.Fatal("blocked sample did not start")
	}
	done := make(chan struct{})
	go func() {
		hub.Shutdown()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Shutdown did not cancel blocked sample")
	}
	// Idempotency must not panic or block.
	hub.Shutdown()
}

func waitFor(t *testing.T, timeout time.Duration, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition did not become true before timeout")
}
