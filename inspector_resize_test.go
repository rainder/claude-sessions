package main

import (
	"sync"
	"testing"
	"time"
)

func TestInspectorResizeSeqRunReturnsBeforeFnFinishes(t *testing.T) {
	var s inspectorResizeSeq
	returned := make(chan struct{})
	block := make(chan struct{})
	go func() {
		s.run(func() { <-block })
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("run blocked until fn finished")
	}
	close(block)
	s.wait()
}

func TestInspectorResizeSeqRunsJobsInOrder(t *testing.T) {
	var s inspectorResizeSeq
	var mu sync.Mutex
	var order []int
	release := make(chan struct{})
	s.run(func() {
		<-release
		mu.Lock()
		order = append(order, 1)
		mu.Unlock()
	})
	s.run(func() {
		mu.Lock()
		order = append(order, 2)
		mu.Unlock()
	})
	close(release)
	s.wait()
	if len(order) != 2 || order[0] != 1 || order[1] != 2 {
		t.Fatalf("order = %v, want [1 2]", order)
	}
}

func TestInspectorResizeSeqCloseSkipsStalePinThenReverts(t *testing.T) {
	var s inspectorResizeSeq
	release := make(chan struct{})
	var pin, revert bool
	gen := s.begin()
	s.run(func() {
		<-release
		if s.current(gen) {
			pin = true
		}
	})
	s.begin()
	s.run(func() { revert = true })
	close(release)
	s.wait()
	if pin {
		t.Fatal("stale pin ran after close began")
	}
	if !revert {
		t.Fatal("revert did not run")
	}
}

func TestInspectorResizeSeqRunDoesNotWaitForBusyWorker(t *testing.T) {
	var s inspectorResizeSeq
	block := make(chan struct{})
	s.run(func() { <-block })
	returned := make(chan struct{})
	go func() {
		s.run(func() {})
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("run blocked on a busy worker")
	}
	close(block)
	s.wait()
}

func TestInspectorResizeSeqWaitJoinsQueuedWork(t *testing.T) {
	var s inspectorResizeSeq
	done := false
	s.run(func() {
		time.Sleep(20 * time.Millisecond)
		done = true
	})
	s.wait()
	if !done {
		t.Fatal("wait returned before queued work finished")
	}
}
