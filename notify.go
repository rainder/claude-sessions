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

// tickBudgetTicks and sendBudgetTicks express both push deadlines as multiples
// of the tick interval, so the two can never drift apart.
//
// They used to be unrelated: a per-tick context of Interval*4 and a bare 10s
// per-device constant. The per-device deadline was therefore unreachable — the
// parent expired first, always — which made it look as though each device had
// its own budget when in truth they all shared one. sendBudgetTicks must stay
// below tickBudgetTicks for the per-device deadline to mean anything.
const (
	tickBudgetTicks = 4
	sendBudgetTicks = 2
)

// maxConcurrentSends bounds the fan-out. It is the registry cap, deliberately,
// and must not be lowered below it.
//
// A device that waits for a worker slot does NOT get its own budget:
// context.WithTimeout takes the earlier of the parent's deadline and the new
// one, so a device starting in a later round inherits whatever is left of the
// tick instead. Sized at 8 against a full registry that meant two rounds of
// eight and then nothing — the last sixteen devices never got a send attempt at
// all. That is the same starvation this file exists to prevent, relocated from
// device two to device seventeen.
//
// Covering the whole registry puts every device in the first round, which is
// what makes the per-device budget real. APNs is HTTP/2 to a single host and
// net/http multiplexes every send over the one connection apnsClient already
// holds; 32 concurrent streams is unremarkable for both.
const maxConcurrentSends = maxRegisteredDevices

// apnsPayloadMax is Apple's limit for a standard remote notification. A larger
// body is rejected with PayloadTooLarge, which would look exactly like a
// notification that never arrived.
const apnsPayloadMax = 4096

// alertFieldMax bounds the free-text fields that go into a payload. Session
// names and waitingFor strings are arbitrary, and two of them plus the JSON
// scaffolding must stay well inside apnsPayloadMax.
const alertFieldMax = 200

// truncateField shortens s to n characters, ending on an ellipsis so the cut is
// visible rather than looking like the real value.
func truncateField(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

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
	opts    notifyHubOptions
	tracker *waitTracker
	// collectFailing tracks whether the previous tick's collection failed, so a
	// persistent failure logs once rather than every interval.
	collectFailing bool
	// dispatchTurn rotates the starting point of the device fan-out. Touched
	// only from the ticker goroutine, via dispatch.
	dispatchTurn int
	stop         chan struct{}
	stopOnce     sync.Once
	done         chan struct{}
}

// interval is the tick period, defaulted for a hub that was handed none.
func (h *notifyHub) interval() time.Duration {
	if h.opts.Interval <= 0 {
		return notifyTickInterval
	}
	return h.opts.Interval
}

// tickBudget bounds the pushes one tick produces, so a wedged fan-out cannot
// outlive the tick that started it.
//
// It is not a bound on the whole tick. h.opts.Collect takes no context and
// nothing selects on ctx while it runs, so a slow collection is unbounded and
// spends this budget before the first push is even built.
func (h *notifyHub) tickBudget() time.Duration { return h.interval() * tickBudgetTicks }

// sendBudget bounds one push to one device. Half the tick budget, so it is a
// deadline that can actually fire, and so a hung connection is reclaimed rather
// than pinned until the tick's own backstop expires.
func (h *notifyHub) sendBudget() time.Duration { return h.interval() * sendBudgetTicks }

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
	//
	// The baseline rule means this tick emits no events and so never dispatches,
	// but it gets the same budget as every other tick regardless: relying on a
	// rule elsewhere in the file to keep an unbounded context harmless is a trap
	// for whoever changes that rule.
	h.tickWithBudget()
	t := time.NewTicker(h.opts.Interval)
	defer t.Stop()
	for {
		select {
		case <-h.stop:
			return
		case <-t.C:
			h.tickWithBudget()
		}
	}
}

// tickWithBudget runs one tick under the per-tick deadline. The deadline reins
// in the fan-out only — collection runs before anything consults ctx — so a
// wedged push cannot stop the hub sampling sessions, but a wedged collection
// still can.
func (h *notifyHub) tickWithBudget() {
	ctx, cancel := context.WithTimeout(context.Background(), h.tickBudget())
	defer cancel()
	h.tickOnce(ctx)
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
		// Log the first failure of a streak only. A broken home directory would
		// otherwise disable every notification silently, but logging every two
		// seconds would bury the machine in identical lines.
		if !h.collectFailing {
			h.collectFailing = true
			fmt.Fprintf(os.Stderr, "claude-sessions: cannot read sessions, notifications paused: %v\n", err)
		}
		return
	}
	if h.collectFailing {
		h.collectFailing = false
		fmt.Fprintln(os.Stderr, "claude-sessions: sessions readable again, notifications resumed")
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

// dispatch pushes one alert to every registered device, concurrently.
//
// Sends overlap, up to maxConcurrentSends at a time. That is the whole fix: a
// serial loop shares one tick's budget between every device, so a device that
// never answers spends the budget and every device behind it fails on an
// already-expired context — and because the tracker has already moved that
// session to phaseAlerted, nothing ever retries. Overlapping the sends means
// every device starts with its own full sendBudget, so no device's stall can
// deny another device its push.
//
// Concurrency is also what makes dispatch order stop mattering, which is the
// other half of the problem: DeviceStore.List sorts by token and tokens are
// stable, so a serial loop starved the same device every tick forever. The
// rotation below is belt and braces for that, since maxConcurrentSends now
// covers the whole registry and nobody queues in the first place.
//
// Known limit, deliberately left: events are dispatched one after another
// within a tick, so several sessions entering a wait in the same tick share one
// tick budget and the later events' fan-outs get less of it. Carrying undelivered
// pushes over to the next tick is a different design — a pending queue with its
// own duplicate-notification questions — and is not attempted here.
func (h *notifyHub) dispatch(ctx context.Context, e notifyEvent) {
	payload := buildAlertPayload(h.opts.HostName, h.opts.HostID, e)
	collapse := h.opts.HostID + ":" + e.SessionID

	// Snapshot the registry once, outside the fan-out: no lock is held across
	// any send, and a 410 arriving mid-dispatch removes a device without
	// disturbing the sends already in flight.
	devices := h.rotated(h.opts.Devices.List())

	slots := make(chan struct{}, maxConcurrentSends)
	var wg sync.WaitGroup
dispatchLoop:
	for _, d := range devices {
		// Check before the select. A select whose cases are both ready picks at
		// random, so a spent tick would otherwise still launch sends on an
		// already-expired context — work that cannot succeed and only logs.
		if ctx.Err() != nil {
			break
		}
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			// The tick is over. Stop rather than log a line per remaining device.
			break dispatchLoop
		}
		wg.Add(1)
		go func(d Device) {
			defer wg.Done()
			defer func() { <-slots }()
			h.send(ctx, d, collapse, payload)
		}(d)
	}
	wg.Wait()
}

// send pushes one payload to one device and acts on the outcome.
//
// What the deadline below actually is: the earlier of the tick's remaining
// budget and sendBudget. context.WithTimeout never extends a parent, so this
// cannot hand a device more time than the tick has left. It equals a full
// sendBudget only because maxConcurrentSends covers the whole registry, so
// every device starts in the first round with the tick budget barely touched.
// Shrink that pool and this stops being true for whoever starts in round two.
func (h *notifyHub) send(ctx context.Context, d Device, collapse string, payload []byte) {
	sendCtx, cancel := context.WithTimeout(ctx, h.sendBudget())
	defer cancel()
	err := h.opts.Sender.Send(sendCtx, pushRequest{
		DeviceToken: d.Token,
		Topic:       h.opts.BundleID,
		CollapseID:  collapse,
		PushType:    "alert",
		Priority:    "10",
		Environment: d.Environment,
		Payload:     payload,
	})
	// Each branch logs in a single Fprintf. Goroutines that build a line in two
	// calls interleave, and they do it in exactly the situation where the log is
	// the only thing you have.
	switch {
	case err == nil:
	case errors.Is(err, errDeviceGone):
		// Worth a line: a wrong production/sandbox environment reads as a
		// dead token, and a silently emptied registry looks identical to a
		// quiet week.
		fmt.Fprintf(os.Stderr, "claude-sessions: dropping device %s: %v\n", shortToken(d.Token), err)
		h.opts.Devices.Remove(d.Token)
	default:
		// Keep the device, and do not back off from it either.
		//
		// Pruning is out: a timeout is not evidence a token is dead. The usual
		// cause is this host's own network, and pruning on it would empty the
		// registry of every working device at once — precisely the failure
		// DeviceStore was written to avoid. Only Apple saying 410,
		// BadDeviceToken or Unregistered drops a device (apns.go:204-209).
		//
		// A per-device back-off is out too, and that is the less obvious call.
		// It would have earned its keep when the fan-out was serial, because a
		// known-slow device cost every other device its push. Concurrently it
		// costs one of maxConcurrentSends worker slots for at most sendBudget,
		// which is not worth a counter map that has to reset on first success
		// and be pruned whenever a token is removed, and which would suppress a
		// device that had recovered.
		fmt.Fprintf(os.Stderr, "claude-sessions: push to %s failed: %v\n", shortToken(d.Token), err)
	}
}

// rotated returns devices starting one place further along on each call, so a
// bounded fan-out cannot put the same device at the back of the queue forever.
//
// DeviceStore.List sorts by token and tokens never change, so without this the
// last device is last on every tick of every day. That sort is load-bearing —
// tests and the on-disk determinism of devices.json depend on it — so the
// rotation happens here rather than in List.
func (h *notifyHub) rotated(devices []Device) []Device {
	if len(devices) < 2 {
		return devices
	}
	off := h.dispatchTurn % len(devices)
	h.dispatchTurn++
	out := make([]Device, 0, len(devices))
	return append(append(out, devices[off:]...), devices[:off]...)
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
	label = truncateField(label, alertFieldMax)
	body = truncateField(body, alertFieldMax)
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
	if len(data) > apnsPayloadMax {
		// Belt and braces: the field caps above should make this unreachable,
		// but an oversized payload is rejected outright and is indistinguishable
		// from a lost notification, so degrade to something deliverable.
		return []byte(`{"aps":{"alert":{"title":"A session needs you","body":"Open to see which"},` +
			`"sound":"default","category":"SESSION_WAITING","interruption-level":"time-sensitive"}}`)
	}
	return data
}
