package main

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// InspectorSnapshot is an immutable, self-contained view of a session's live
// preview at one instant, handed from the hub goroutine to the render loop.
// Lines is already split and sanitized; Loading/Stale/Ended/Error describe why
// the content may not be fresh. Callers receive a deep copy of Lines so they can
// never race the hub's ownership of the underlying slice.
type InspectorSnapshot struct {
	TargetID string
	Session  Session
	Source   string
	Label    string
	Lines    []string
	Loading  bool
	Stale    bool
	Ended    bool
	Error    string
	Updated  time.Time
}

// inspectorViewState is the render loop's scroll/follow bookkeeping for the
// fullscreen inspector. It owns no terminal or hub resources — every method is a
// pure transform of offsets against the current snapshot, so it is trivially
// testable. top is the index of the first visible line; follow keeps the view
// pinned to the newest line as content arrives; newLines counts lines appended
// while paused so the UI can show a "N new" hint.
type inspectorViewState struct {
	targetID     string
	snapshot     InspectorSnapshot
	top          int
	viewportRows int
	follow       bool
	newLines     int
}

// newInspectorViewState starts in follow mode with no content.
func newInspectorViewState(targetID string) inspectorViewState {
	return inspectorViewState{targetID: targetID, follow: true}
}

// maxTop is the largest valid top offset: enough to show the last viewportRows
// lines and no further. It floors at zero when the content fits the viewport.
func (v *inspectorViewState) maxTop() int {
	m := len(v.snapshot.Lines) - v.viewportRows
	if m < 0 {
		m = 0
	}
	return m
}

// clampTop constrains top to [0, maxTop]. Every offset mutation routes through
// this so no method can leave top pointing past either end of the content.
func (v *inspectorViewState) clampTop() {
	if m := v.maxTop(); v.top > m {
		v.top = m
	}
	if v.top < 0 {
		v.top = 0
	}
}

// scroll moves the viewport by delta lines (negative = toward the top). Reaching
// the bottom re-engages follow mode and clears the new-line counter; leaving the
// bottom drops out of follow so incoming content no longer yanks the view.
func (v *inspectorViewState) scroll(delta int) {
	v.top += delta
	v.clampTop()
	v.follow = v.top >= v.maxTop()
	if v.follow {
		v.newLines = 0
	}
}

// page moves the viewport by delta whole viewports (negative = toward the top).
func (v *inspectorViewState) page(delta int) {
	v.scroll(delta * v.viewportRows)
}

// home jumps to the oldest line and stops following.
func (v *inspectorViewState) home() {
	v.top = 0
	v.follow = false
	v.clampTop()
}

// followBottom pins the view to the newest line, resuming follow mode and
// clearing the new-line counter.
func (v *inspectorViewState) followBottom() {
	v.follow = true
	v.newLines = 0
	v.top = v.maxTop()
}

// resize records a new viewport height and re-derives the offset: following
// stays pinned to the bottom, otherwise top is re-clamped into range.
func (v *inspectorViewState) resize(rows int) {
	v.viewportRows = rows
	if v.follow {
		v.top = v.maxTop()
		return
	}
	v.clampTop()
}

// applySnapshot adopts a fresh snapshot. In follow mode the view tracks the new
// bottom; while paused the previous top is preserved and any net-added lines are
// accumulated into newLines so the UI can flag unseen output. Snapshots for a
// different target are ignored.
func (v *inspectorViewState) applySnapshot(snap InspectorSnapshot) {
	if snap.TargetID != v.targetID {
		return
	}
	prevLines := len(v.snapshot.Lines)
	v.snapshot = snap
	if v.follow {
		v.top = v.maxTop()
		v.newLines = 0
		return
	}
	if delta := len(snap.Lines) - prevLines; delta > 0 {
		v.newLines += delta
	}
	v.clampTop()
}

// inspectorFetcher fetches one preview for a target. The default implementation
// dispatches local vs. remote; tests inject a deterministic stub.
type inspectorFetcher func(selectionTarget, PreviewLimits) (PreviewResult, error)

// defaultInspectorFetch dispatches by the target's host: an empty host is a
// local session (LoadPreview), otherwise the preview is fetched over HTTP from
// the named server (fetchRemotePreview). A target with no session (an empty-host
// placeholder row) has nothing to preview and reads as ended.
func defaultInspectorFetch(target selectionTarget, limits PreviewLimits) (PreviewResult, error) {
	if target.session == nil {
		return PreviewResult{}, errSessionEnded
	}
	if target.session.Host == "" {
		return LoadPreview(target.session.PID, limits)
	}
	return fetchRemotePreview(target.session.Host, target.session.PID, limits)
}

// InspectorHub polls one session's preview in a background goroutine and streams
// the newest snapshot into a mutex-guarded slot, mirroring RemoteHub. The
// goroutine never touches the terminal; instead every state change writes one
// byte to a wake pipe so the render loop can repaint immediately rather than
// waiting for its tick. The target, limits and fetcher are fixed at construction
// and read without the lock.
type InspectorHub struct {
	mu       sync.Mutex
	target   selectionTarget
	snapshot InspectorSnapshot
	limits   PreviewLimits
	fetch    inspectorFetcher
	kick     chan struct{}
	stop     chan struct{}
	wakeR    int  // read end: passed to unix.Select in the TUI loop
	wakeW    int  // write end: signaled after each snapshot update
	closed   bool // set under mu by Shutdown before the wake fds are closed
	once     sync.Once
}

// NewInspectorHub starts a hub for target using the default local/remote
// fetcher, polling every interval. The first fetch is kicked immediately so the
// view fills in as soon as the reply lands.
func NewInspectorHub(target selectionTarget, interval time.Duration) (*InspectorHub, error) {
	return newInspectorHub(target, interval, defaultInspectorFetch)
}

// newInspectorHub is NewInspectorHub with an injectable fetcher for tests.
func newInspectorHub(target selectionTarget, interval time.Duration, fetch inspectorFetcher) (*InspectorHub, error) {
	var p [2]int
	if err := unix.Pipe(p[:]); err != nil {
		return nil, fmt.Errorf("inspector hub pipe: %w", err)
	}
	syscall.CloseOnExec(p[0])
	syscall.CloseOnExec(p[1])
	// Both ends non-blocking. Write: dropping a wake when the buffer is full is
	// fine — the next update signals again. Read: the TUI drains in a loop until
	// EAGAIN, so a blocking read end would hang on the second iteration.
	_ = unix.SetNonblock(p[0], true)
	_ = unix.SetNonblock(p[1], true)

	var sess Session
	if target.session != nil {
		sess = *target.session
	}
	h := &InspectorHub{
		target: target,
		snapshot: InspectorSnapshot{
			TargetID: target.id,
			Session:  sess,
			Loading:  true,
		},
		limits: DefaultPreviewLimits(),
		fetch:  fetch,
		kick:   make(chan struct{}, 1),
		stop:   make(chan struct{}),
		wakeR:  p[0],
		wakeW:  p[1],
	}
	go h.run(interval)
	h.Refresh()
	return h, nil
}

// WakeFD returns a file descriptor that becomes readable each time the snapshot
// changes. The caller drains it on read.
func (h *InspectorHub) WakeFD() int { return h.wakeR }

func (h *InspectorHub) run(interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-h.stop:
			return
		case <-t.C:
		case <-h.kick:
		}
		h.fetchOnce()
	}
}

// fetchOnce runs one fetch and folds the outcome into the snapshot under the
// error taxonomy, then wakes the render loop. Prior lines survive transient
// failures so the view never blanks on a hiccup.
func (h *InspectorHub) fetchOnce() {
	res, err := h.fetch(h.target, h.limits)

	h.mu.Lock()
	prev := h.snapshot
	now := time.Now()
	switch {
	case err == nil:
		h.snapshot = InspectorSnapshot{
			TargetID: prev.TargetID,
			Session:  prev.Session,
			Source:   res.Source,
			Label:    res.Label,
			Lines:    splitPreviewLines(res.Content),
			Updated:  now,
		}
	case errors.Is(err, errSessionEnded):
		// The session's pane and transcript are both gone: keep the last lines,
		// flag it ended, and stop showing the spinner. Ended is terminal, not a
		// transient hiccup, so clear any prior Stale/Error the snapshot carried.
		prev.Ended = true
		prev.Stale = false
		prev.Error = ""
		prev.Loading = false
		prev.Updated = now
		h.snapshot = prev
	case len(prev.Lines) > 0:
		// A transient failure with content already on screen: retain the lines
		// and mark them stale with a concise reason. Clear Ended in case a prior
		// "ended" verdict is now contradicted by the host answering again.
		prev.Stale = true
		prev.Ended = false
		prev.Loading = false
		prev.Error = shortErr(err)
		prev.Updated = now
		h.snapshot = prev
	default:
		// Never loaded successfully: surface the error in place of content, with
		// no stale/ended flags since there are no prior lines to qualify.
		prev.Stale = false
		prev.Ended = false
		prev.Loading = false
		prev.Error = shortErr(err)
		prev.Updated = now
		h.snapshot = prev
	}
	h.mu.Unlock()

	h.signalWake()
}

// Snapshot returns the most recent snapshot with a private copy of Lines so the
// caller can read or mutate it without racing the hub goroutine.
func (h *InspectorHub) Snapshot() InspectorSnapshot {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.snapshot
	if s.Lines != nil {
		lines := make([]string, len(s.Lines))
		copy(lines, s.Lines)
		s.Lines = lines
	}
	return s
}

// Refresh requests an immediate refetch. Non-blocking; coalesces when a kick is
// already pending.
func (h *InspectorHub) Refresh() {
	select {
	case h.kick <- struct{}{}:
	default:
	}
}

// signalWake nudges the render loop by writing one byte to the wake pipe. It
// takes h.mu and no-ops once Shutdown has flagged the hub closed, so a wake from
// an in-flight fetch that completes after Shutdown can never write to the closed
// (and possibly reused) descriptor. Unlike the process-lifetime RemoteHub, an
// InspectorHub is created and torn down per inspector open, so this teardown
// race is reachable.
func (h *InspectorHub) signalWake() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	_, _ = unix.Write(h.wakeW, []byte{1})
}

// Shutdown stops the background goroutine and closes the wake pipe. Safe to call
// more than once — the teardown runs exactly once via sync.Once. closed is set
// under h.mu before the fds are closed so signalWake (which checks it under the
// same lock) can never write to a descriptor Shutdown has already closed.
func (h *InspectorHub) Shutdown() {
	h.once.Do(func() {
		close(h.stop)
		h.mu.Lock()
		h.closed = true
		h.mu.Unlock()
		_ = unix.Close(h.wakeW)
		_ = unix.Close(h.wakeR)
	})
}

// splitPreviewLines splits preview content into display lines, dropping the
// single empty line produced by a trailing newline terminator so "ok\n" renders
// as one line rather than two. Genuinely blank interior lines are preserved.
func splitPreviewLines(content string) []string {
	content = strings.TrimSuffix(content, "\n")
	if content == "" {
		return []string{}
	}
	return strings.Split(content, "\n")
}
