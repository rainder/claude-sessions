package main

import (
	"context"
	"math"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// hostUsageSampleTimeout bounds one collector sample.
	hostUsageSampleTimeout = 2 * time.Second
	// hostUsageInterval is the default refresh cadence for long-running hubs.
	hostUsageInterval = 2 * time.Second
)

// LoadAverage holds the raw 1/5/15-minute load averages htop-style. Nil fields
// mean unavailable; pointers preserve a valid zero through JSON omitempty. The
// three fields move together — hostLoadAverage populates all or none.
type LoadAverage struct {
	OneMinute      *float64 `json:"oneMinute,omitempty"`
	FiveMinutes    *float64 `json:"fiveMinutes,omitempty"`
	FifteenMinutes *float64 `json:"fifteenMinutes,omitempty"`
}

// HostUsage is one whole-host resource snapshot. Nil fields mean unavailable;
// pointers preserve a valid zero value through JSON omitempty.
type HostUsage struct {
	CPUPercent    *float64     `json:"cpuPercent,omitempty"`
	MemoryPercent *float64     `json:"memoryPercent,omitempty"`
	Load          *LoadAverage `json:"loadAverage,omitempty"`
}

// LocalHost groups the current machine's identity, sessions, and resource
// snapshot for rendering. RemoteResult is the corresponding remote shape.
type LocalHost struct {
	Name      string
	Sessions  []Session
	HostUsage HostUsage
}

func hostPercent(v float64) *float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	v = max(0, min(100, v))
	return &v
}

// hostLoadAverage builds a LoadAverage from raw 1/5/15-minute samples. Load
// averages are reported htop-style: never clamped or core-normalized, so a valid
// zero is preserved and values above 100 pass through unchanged. Any negative,
// NaN, or infinite input makes the whole triple untrustworthy, so the function
// returns nil atomically rather than a partially valid LoadAverage.
func hostLoadAverage(one, five, fifteen float64) *LoadAverage {
	for _, v := range [...]float64{one, five, fifteen} {
		if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			return nil
		}
	}
	return &LoadAverage{
		OneMinute:      &one,
		FiveMinutes:    &five,
		FifteenMinutes: &fifteen,
	}
}

type hostUsageCollector interface {
	Sample(context.Context) HostUsage
}

type unavailableHostUsageCollector struct{}

func (unavailableHostUsageCollector) Sample(context.Context) HostUsage { return HostUsage{} }

func newHostUsageCollector() hostUsageCollector {
	switch runtime.GOOS {
	case "linux":
		return newLinuxHostUsageCollector()
	case "darwin":
		return newDarwinHostUsageCollector()
	default:
		return unavailableHostUsageCollector{}
	}
}

func CollectHostUsage() HostUsage {
	ctx, cancel := context.WithTimeout(context.Background(), hostUsageSampleTimeout)
	defer cancel()
	return newHostUsageCollector().Sample(ctx)
}

// HostUsageHub serializes one platform collector and caches its latest result.
type HostUsageHub struct {
	mu        sync.RWMutex
	current   HostUsage
	collector hostUsageCollector
	interval  time.Duration
	paused    atomic.Bool
	kick      chan struct{}
	ctx       context.Context
	cancel    context.CancelFunc
	done      chan struct{}
	shutdown  sync.Once
}

func NewHostUsageHub(interval time.Duration) *HostUsageHub {
	return newHostUsageHubWithCollector(newHostUsageCollector(), interval)
}

func newHostUsageHubWithCollector(collector hostUsageCollector, interval time.Duration) *HostUsageHub {
	if interval <= 0 {
		interval = hostUsageInterval
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &HostUsageHub{
		collector: collector,
		interval:  interval,
		kick:      make(chan struct{}, 1),
		ctx:       ctx,
		cancel:    cancel,
		done:      make(chan struct{}),
	}
	h.sample()
	go h.run()
	return h
}

func (h *HostUsageHub) sample() {
	ctx, cancel := context.WithTimeout(h.ctx, hostUsageSampleTimeout)
	defer cancel()
	usage := h.collector.Sample(ctx)
	h.mu.Lock()
	h.current = usage
	h.mu.Unlock()
}

func (h *HostUsageHub) run() {
	defer close(h.done)
	ticker := time.NewTicker(h.interval)
	defer ticker.Stop()
	for {
		select {
		case <-h.ctx.Done():
			return
		case <-ticker.C:
		case <-h.kick:
		}
		if h.paused.Load() {
			continue
		}
		h.sample()
	}
}

func (h *HostUsageHub) Snapshot() HostUsage {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.current
}

func (h *HostUsageHub) Pause() { h.paused.Store(true) }

func (h *HostUsageHub) Resume() {
	h.paused.Store(false)
	h.Kick()
}

func (h *HostUsageHub) Kick() {
	select {
	case h.kick <- struct{}{}:
	default:
	}
}

func (h *HostUsageHub) Shutdown() {
	h.shutdown.Do(func() {
		h.cancel()
		<-h.done
	})
}
