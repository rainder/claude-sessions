package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// notifyEventKind distinguishes "the user is needed" from "no longer needed".
type notifyEventKind int

const (
	notifyAlert notifyEventKind = iota
	notifyClear
)

// notifyEvent is one thing worth telling a phone about.
type notifyEvent struct {
	Kind       notifyEventKind
	SessionID  string
	PID        int
	CWD        string
	Name       string
	WaitingFor string
	// Generation increments each time this session enters a distinct wait.
	// It rides in the push payload as part of event_id so a later phase can
	// refuse to act on a notification whose prompt has since been answered.
	Generation int
}

// waitPhase is where one session sits in the wait state machine.
type waitPhase int

const (
	phaseIdle    waitPhase = iota // not waiting
	phasePending                  // waiting, not yet long enough to alert
	phaseAlerted                  // alert sent for the current generation
)

// waitEntry is the tracker's per-session memory.
//
// It keeps identity as well as phase so a clear produced by absence — where
// there is no snapshot to read from — can still say which session ended.
type waitEntry struct {
	phase      waitPhase
	waitingFor string
	generation int
	pid        int
	cwd        string
	name       string
	// missed counts consecutive ticks in which the session was absent.
	missed int
}

// missedTicksBeforeClear is how many consecutive absences mean "gone".
//
// readSessionFile silently rejects a partially-written JSON file, so a single
// absence is indistinguishable from Claude Code being mid-write. Clearing on one
// would produce a spurious clear immediately followed by a re-alert.
const missedTicksBeforeClear = 2

// waitTracker turns a stream of session snapshots into alert/clear events.
//
// It is an explicit state machine rather than a diff of the previous tick,
// because a diff cannot express the debounce: by the second waiting tick the
// transition reads waiting -> waiting, so an edge test stops firing and the
// alert is dropped instead of delayed.
//
// The tracker is not safe for concurrent use; it is owned by the single ticker
// goroutine.
type waitTracker struct {
	entries   map[string]*waitEntry
	baselined bool
	// nextGen is a monotonic counter across the tracker's whole life, not a
	// per-entry one. An entry evicted after two missed ticks would otherwise
	// restart at 1, so a session that waits, disappears, and waits again would
	// reuse an event_id — and a stale notification for the first wait would pass
	// the freshness check on the second.
	nextGen int
}

func newWaitTracker() *waitTracker {
	return &waitTracker{entries: map[string]*waitEntry{}}
}

// Tick folds one snapshot in and returns the events it produced, sorted by
// session ID so output is deterministic.
//
// The first call establishes a silent baseline: sessions already waiting when
// the process starts move straight to alerted without notifying. A restart is a
// local action taken at the keyboard, and the app's session list is ground
// truth for anything that was already waiting.
func (t *waitTracker) Tick(sessions []Session) []notifyEvent {
	var events []notifyEvent
	seen := make(map[string]bool, len(sessions))

	for _, s := range sessions {
		if s.SessionID == "" {
			continue
		}
		if seen[s.SessionID] {
			// Two rows for one session in a single snapshot would advance the
			// machine twice and defeat the debounce. Shouldn't happen, but the
			// input is a directory listing, not a guarantee.
			continue
		}
		seen[s.SessionID] = true

		e, ok := t.entries[s.SessionID]
		if !ok {
			e = &waitEntry{}
			t.entries[s.SessionID] = e
		}
		// A resumed or migrated session keeps its SessionID but gets a new PID.
		// That is a different process and a different wait: start it over rather
		// than letting it inherit phaseAlerted and never notify again.
		if e.pid != 0 && e.pid != s.PID {
			e.phase = phaseIdle
			e.waitingFor = ""
		}
		e.missed = 0
		e.pid, e.cwd, e.name = s.PID, s.CWD, s.Name

		if !s.Waiting() {
			if e.phase == phaseAlerted {
				events = append(events, t.event(notifyClear, s, e))
			}
			e.phase = phaseIdle
			e.waitingFor = ""
			continue
		}

		if !t.baselined {
			// Already waiting at startup: adopt the state without notifying.
			e.phase = phaseAlerted
			e.waitingFor = s.WaitingFor
			e.generation = t.bumpGeneration()
			continue
		}

		switch e.phase {
		case phaseIdle:
			e.phase = phasePending
			e.waitingFor = s.WaitingFor
		case phasePending:
			e.phase = phaseAlerted
			e.waitingFor = s.WaitingFor
			e.generation = t.bumpGeneration()
			events = append(events, t.event(notifyAlert, s, e))
		case phaseAlerted:
			// A different prompt without an intervening non-waiting tick is a
			// new thing to notify about.
			if e.waitingFor != s.WaitingFor {
				e.waitingFor = s.WaitingFor
				e.generation = t.bumpGeneration()
				events = append(events, t.event(notifyAlert, s, e))
			}
		}
	}

	for id, e := range t.entries {
		if seen[id] {
			continue
		}
		e.missed++
		if e.missed < missedTicksBeforeClear {
			continue
		}
		if e.phase == phaseAlerted {
			events = append(events, notifyEvent{
				Kind:       notifyClear,
				SessionID:  id,
				PID:        e.pid,
				CWD:        e.cwd,
				Name:       e.name,
				WaitingFor: e.waitingFor,
				Generation: e.generation,
			})
		}
		delete(t.entries, id)
	}

	t.baselined = true
	sort.Slice(events, func(i, j int) bool { return events[i].SessionID < events[j].SessionID })
	return events
}

// bumpGeneration hands out the next wait generation. Monotonic across the
// tracker, so an id is never reused after an entry is evicted.
func (t *waitTracker) bumpGeneration() int {
	t.nextGen++
	return t.nextGen
}

func (t *waitTracker) event(kind notifyEventKind, s Session, e *waitEntry) notifyEvent {
	return notifyEvent{
		Kind:       kind,
		SessionID:  s.SessionID,
		PID:        s.PID,
		CWD:        s.CWD,
		Name:       s.Name,
		WaitingFor: s.WaitingFor,
		Generation: e.generation,
	}
}

// notifyTickInterval is how often the hub samples sessions. Two ticks are
// required before an alert fires, so this also sets the notification floor:
// roughly four seconds from prompt to push.
const notifyTickInterval = 2 * time.Second

// notifyHubOptions is everything the hub needs. Collect and Sender are seams so
// tests drive it without touching disk or the network.
type notifyHubOptions struct {
	HostName string
	HostID   string
	BundleID string
	Devices  *DeviceStore
	Sender   pushSender
	Collect  func() ([]Session, error)
	Interval time.Duration
}

// notifyHub polls sessions and pushes alerts. It follows the repo's existing
// background-poller shape (see usage_hub.go) minus the wake pipe: nothing needs
// to kick it early.
type notifyHub struct {
	opts     notifyHubOptions
	tracker  *waitTracker
	stop     chan struct{}
	stopOnce sync.Once
	done     chan struct{}
}

func newNotifyHub(opts notifyHubOptions) *notifyHub {
	if opts.Interval == 0 {
		opts.Interval = notifyTickInterval
	}
	if opts.Collect == nil {
		opts.Collect = CollectLocalLite
	}
	return &notifyHub{
		opts:    opts,
		tracker: newWaitTracker(),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
}

// Start runs the poll loop until Shutdown. Tests call tickOnce directly instead.
func (h *notifyHub) Start() {
	go h.run()
}

func (h *notifyHub) run() {
	defer close(h.done)
	// Take the baseline immediately rather than waiting out the first tick.
	// Otherwise a session that starts waiting inside the startup window is
	// absorbed by the silent-baseline rule and never alerts at all.
	h.tickOnce(context.Background())
	t := time.NewTicker(h.opts.Interval)
	defer t.Stop()
	for {
		select {
		case <-h.stop:
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), h.opts.Interval*4)
			h.tickOnce(ctx)
			cancel()
		}
	}
}

// Shutdown stops the poll loop. Safe to call more than once — cmdServer defers
// it and tests call it explicitly, and a select-then-close would let two callers
// both reach the close and panic.
func (h *notifyHub) Shutdown() {
	h.stopOnce.Do(func() { close(h.stop) })
}

// tickOnce samples sessions, advances the state machine, and pushes whatever
// came out. A collection failure is skipped rather than treated as "every
// session vanished" — that would clear every live alert.
func (h *notifyHub) tickOnce(ctx context.Context) {
	sessions, err := h.opts.Collect()
	if err != nil {
		return
	}
	for _, e := range h.tracker.Tick(sessions) {
		if e.Kind != notifyAlert {
			// Clear events end a Live Activity, which needs the per-activity
			// token model Apple requires. Nothing to send until that lands.
			continue
		}
		h.dispatch(ctx, e)
	}
}

func (h *notifyHub) dispatch(ctx context.Context, e notifyEvent) {
	payload := buildAlertPayload(h.opts.HostName, h.opts.HostID, e)
	collapse := h.opts.HostID + ":" + e.SessionID
	for _, d := range h.opts.Devices.List() {
		err := h.opts.Sender.Send(ctx, pushRequest{
			DeviceToken: d.Token,
			Topic:       h.opts.BundleID,
			CollapseID:  collapse,
			PushType:    "alert",
			Priority:    "10",
			Environment: d.Environment,
			Payload:     payload,
		})
		switch {
		case err == nil:
		case errors.Is(err, errDeviceGone):
			h.opts.Devices.Remove(d.Token)
		default:
			// Transient: keep the device. A network blip is not a reason to stop
			// notifying a phone forever.
			fmt.Fprintf(os.Stderr, "claude-sessions: push to %s failed: %v\n", shortToken(d.Token), err)
		}
	}
}

// shortToken trims a device token for log lines — they are 64 hex characters
// and full ones make the log unreadable.
func shortToken(token string) string {
	if len(token) <= 8 {
		return token
	}
	return token[:8] + "…"
}

// buildAlertPayload renders the APNs body for one waiting session.
func buildAlertPayload(hostName, hostID string, e notifyEvent) []byte {
	label := e.Name
	if label == "" || label == "-" {
		label = filepath.Base(e.CWD)
	}
	body := e.WaitingFor
	if body == "" {
		body = "waiting"
	}
	payload := map[string]any{
		"aps": map[string]any{
			"alert": map[string]any{
				"title": label + " needs you",
				"body":  "waiting: " + body,
			},
			"sound":              "default",
			"category":           "SESSION_WAITING",
			"interruption-level": "time-sensitive",
		},
		"host":        hostName,
		"host_id":     hostID,
		"pid":         e.PID,
		"session_id":  e.SessionID,
		"event_id":    fmt.Sprintf("%s:%s:%d", hostID, e.SessionID, e.Generation),
		"waiting_for": e.WaitingFor,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return []byte(`{"aps":{"alert":"a session needs you"}}`)
	}
	return data
}
