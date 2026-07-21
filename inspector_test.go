package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestInspectorApplySnapshotFollowsBottom(t *testing.T) {
	v := newInspectorViewState("42")
	v.viewportRows = 3
	v.applySnapshot(InspectorSnapshot{TargetID: "42", Lines: []string{"1", "2", "3", "4"}})
	if v.top != 1 || !v.follow {
		t.Fatalf("view = %#v", v)
	}
	v.applySnapshot(InspectorSnapshot{TargetID: "42", Lines: []string{"1", "2", "3", "4", "5"}})
	if v.top != 2 {
		t.Fatalf("top = %d, want 2", v.top)
	}
}

func TestInspectorPausedPreservesTopAndCountsNewLines(t *testing.T) {
	v := newInspectorViewState("42")
	v.viewportRows = 2
	v.applySnapshot(InspectorSnapshot{TargetID: "42", Lines: []string{"1", "2", "3"}})
	v.scroll(-1)
	v.applySnapshot(InspectorSnapshot{TargetID: "42", Lines: []string{"1", "2", "3", "4", "5"}})
	if v.top != 0 || v.follow || v.newLines != 2 {
		t.Fatalf("view = %#v", v)
	}
	v.followBottom()
	if !v.follow || v.newLines != 0 || v.top != 3 {
		t.Fatalf("followed view = %#v", v)
	}
}

func TestInspectorHubRetainsSnapshotOnRefreshError(t *testing.T) {
	calls := 0
	fetch := func(target selectionTarget, _ PreviewLimits) (PreviewResult, error) {
		calls++
		if calls == 1 {
			return PreviewResult{Source: "tmux", Content: "ok\n"}, nil
		}
		return PreviewResult{}, errors.New("offline")
	}
	h, err := newInspectorHub(sessionSelectionTarget(Session{PID: 42}), time.Hour, fetch)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Shutdown()
	waitForInspectorSnapshot(t, h, func(s InspectorSnapshot) bool { return len(s.Lines) == 1 })
	h.Refresh()
	got := waitForInspectorSnapshot(t, h, func(s InspectorSnapshot) bool { return s.Stale })
	if strings.Join(got.Lines, "\n") != "ok" || got.Error != "offline" {
		t.Fatalf("snapshot = %#v", got)
	}
}

// TestInspectorHubSnapshotsRetainDistinctTargetIDs proves a local pid 42 hub and
// a remote dev:42 hub keep their own TargetID (and hand the fetcher the matching
// target) rather than sharing a single mutable snapshot.
func TestInspectorHubSnapshotsRetainDistinctTargetIDs(t *testing.T) {
	fetch := func(target selectionTarget, _ PreviewLimits) (PreviewResult, error) {
		// Echo the target id back so we can prove each hub fetched its own.
		return PreviewResult{Source: "tmux", Content: target.id + "\n"}, nil
	}
	local, err := newInspectorHub(sessionSelectionTarget(Session{PID: 42}), time.Hour, fetch)
	if err != nil {
		t.Fatal(err)
	}
	defer local.Shutdown()
	remote, err := newInspectorHub(sessionSelectionTarget(Session{PID: 42, Host: "dev"}), time.Hour, fetch)
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Shutdown()

	ls := waitForInspectorSnapshot(t, local, func(s InspectorSnapshot) bool { return len(s.Lines) == 1 })
	rs := waitForInspectorSnapshot(t, remote, func(s InspectorSnapshot) bool { return len(s.Lines) == 1 })
	if ls.TargetID != "42" || strings.Join(ls.Lines, "\n") != "42" {
		t.Fatalf("local snapshot = %#v", ls)
	}
	if rs.TargetID != "dev:42" || strings.Join(rs.Lines, "\n") != "dev:42" {
		t.Fatalf("remote snapshot = %#v", rs)
	}
}

// waitForInspectorSnapshot polls Snapshot() until cond passes or 2s elapse.
func waitForInspectorSnapshot(t *testing.T, h *InspectorHub, cond func(InspectorSnapshot) bool) InspectorSnapshot {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s := h.Snapshot(); cond(s) {
			return s
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("snapshot condition not met; last = %#v", h.Snapshot())
	return InspectorSnapshot{}
}
