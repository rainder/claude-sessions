package main

import (
	"sync"
	"sync/atomic"
)

// inspectorResizeSeq runs inspector pin/shrink/revert off the UI thread
// while keeping those jobs in FIFO order. A generation id lets a close skip
// a pin or shrink that has not started yet, so a quick right-then-left does
// not leave the window pinned. The TUI loop never waits on HTTP for back;
// attach and process-exit still call wait so revert lands first.
type inspectorResizeSeq struct {
	mu      sync.Mutex
	jobs    []func()
	running bool
	gen     atomic.Uint64
	wg      sync.WaitGroup
}

// begin invalidates in-flight pin/shrink work and returns the new id.
func (s *inspectorResizeSeq) begin() uint64 {
	return s.gen.Add(1)
}

// current reports whether id is still the latest begin() result.
func (s *inspectorResizeSeq) current(id uint64) bool {
	return s.gen.Load() == id
}

// run queues fn and returns at once. A single drain goroutine runs jobs
// in the order they were queued, so a revert queued after a pin always
// runs after that pin.
func (s *inspectorResizeSeq) run(fn func()) {
	s.wg.Add(1)
	s.mu.Lock()
	s.jobs = append(s.jobs, fn)
	start := !s.running
	if start {
		s.running = true
	}
	s.mu.Unlock()
	if start {
		go s.drain()
	}
}

func (s *inspectorResizeSeq) drain() {
	for {
		s.mu.Lock()
		if len(s.jobs) == 0 {
			s.running = false
			s.mu.Unlock()
			return
		}
		fn := s.jobs[0]
		s.jobs = s.jobs[1:]
		s.mu.Unlock()
		func() {
			defer s.wg.Done()
			fn()
		}()
	}
}

// wait blocks until every run() job has finished.
func (s *inspectorResizeSeq) wait() {
	s.wg.Wait()
}
