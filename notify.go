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
	//
	// It is *seeded* rather than started at zero, which closes the same hole
	// across process boundaries — see newWaitTrackerAt.
	nextGen int
}

func newWaitTracker() *waitTracker { return newWaitTrackerAt(time.Now()) }

// newWaitTrackerAt builds a tracker whose generations cannot collide with those
// of a tracker built at a different moment.
//
// Zero was the obvious starting value and it was wrong. host_id is persisted and
// session ids survive a restart, so a counter that restarts at zero re-issues
// "H:S:1" in the next process — and a clear for the *first* one, delayed across
// the restart, exact-matches the alert for the second and takes a live card off
// the phone. That is precisely the failure the app's exact-match rule exists to
// prevent, arriving through the back door.
//
// Seeding from the clock makes the two ranges disjoint without persisting
// anything. Nanoseconds rather than seconds so the argument is arithmetic rather
// than probabilistic: two processes started Δ apart begin Δ×10⁹ apart in
// generation space, and a tracker issues at most one generation per wait
// reaching its alerted phase, so it would have to sustain a billion new waits a
// second to catch up with a restart one second later. A seconds-resolution seed
// would only hold while a process consumed fewer generations than its uptime in
// seconds, which a machine with a screenful of sessions already waiting at
// startup breaks on its very first tick — the baseline adopts each of them with
// a generation of its own.
//
// The cost is nineteen digits instead of one in each event_id, against a 4 KB
// payload budget that buildClearPayload now bounds explicitly.
func newWaitTrackerAt(start time.Time) *waitTracker {
	return &waitTracker{entries: map[string]*waitEntry{}, nextGen: int(start.UnixNano())}
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
	// Alerts first, then clears — not the tracker's session-id order.
	//
	// Both kinds are dispatched one after another out of a single tick budget, so
	// whatever goes first can spend it. An alert is the product and a clear is
	// housekeeping: a lost alert is a prompt the user never hears about, and it
	// is never retried, because the tracker has already moved that session to
	// phaseAlerted. A lost clear is a card that stays a few seconds longer and is
	// then taken by the app's own reconciliation anyway.
	//
	// Two passes rather than a sort, so the tracker's deterministic ordering
	// within each kind is left exactly as it was.
	events := h.tracker.Tick(sessions)
	for _, e := range events {
		if e.Kind == notifyAlert {
			h.dispatch(ctx, e)
		}
	}
	for _, e := range events {
		if e.Kind != notifyAlert {
			h.dispatch(ctx, e)
		}
	}
}

// pushSpec is everything about one push that depends on the event rather than on
// the device it is going to.
//
// It exists so an alert and the clear that retracts it cannot drift: the
// collapse id is built once, before the two kinds diverge, which is what makes
// them byte-identical after apnsClient truncates them.
type pushSpec struct {
	CollapseID string
	PushType   string
	Priority   string
	Payload    []byte
}

// specFor renders one event into the push it becomes.
//
// A clear is a background push, and that is not cosmetic: **APNs rejects a
// background push sent at priority 10**, so a clear built with the alert's
// headers is a clear that silently never arrives. Nor may it carry an alert, a
// sound or an interruption-level — a background push carrying an alert is not a
// background push, it is a blank notification every time a prompt is answered.
//
// A clear still does not end a Live Activity. That needs the per-activity token
// model Apple requires, which is not built, and will still need it when it is.
// What a clear also is, and what this file used to miss, is "remove the card you
// already delivered" — which needs none of that machinery.
//
// **The collapse id is shared with the alert deliberately, and it turns out to do
// more than intended.** Measured on iOS 26.5 against a real device token: the
// phone was never woken for the background push at all — no app code ran — and
// the delivered card still disappeared, because APNs collapse-id replacement
// swaps the shown alert for a push that displays nothing. Rebuilding this with a
// distinct collapse id for the clear, and changing nothing else, leaves the card
// on screen. So on that runtime the removal is entirely the system's doing.
//
// That is the outcome we want, and it is also generation-blind, which the app's
// exact-match rule is not: the collapse id is host+session with no generation in
// it, so a clear for generation 5 that were delivered *after* generation 6's
// alert would take generation 6's card down with it. The server never emits them
// in that order — a clear precedes the next alert by at least two ticks — so it
// needs APNs to reorder across four seconds.
//
// That window is wider than tick spacing alone suggests, and deliberately so:
// the clear goes out at priority 5 and the alert at priority 10, which is an
// explicit invitation for APNs to hold the clear back while the alert goes
// straight through. Sending the clear at 10 is not the answer — APNs rejects a
// background push at that priority outright — so the reordering latitude is the
// price of the clear being a background push at all.
//
// Left as it is rather than quietly narrowed, because a distinct collapse id
// gives up both the queue replacement *and*, measured on 26.5, the only
// mechanism that actually removes the card. That is a trade to make
// deliberately, not as a side effect.
func (h *notifyHub) specFor(e notifyEvent) (pushSpec, bool) {
	spec := pushSpec{CollapseID: h.opts.HostID + ":" + e.SessionID}
	switch e.Kind {
	case notifyAlert:
		spec.PushType, spec.Priority = "alert", "10"
		spec.Payload = buildAlertPayload(h.opts.HostName, h.opts.HostID, e)
	case notifyClear:
		spec.PushType, spec.Priority = "background", "5"
		spec.Payload = buildClearPayload(h.opts.HostID, e)
	default:
		return pushSpec{}, false
	}
	return spec, true
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
	spec, ok := h.specFor(e)
	if !ok {
		return
	}

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
			h.send(ctx, d, spec)
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
func (h *notifyHub) send(ctx context.Context, d Device, spec pushSpec) {
	sendCtx, cancel := context.WithTimeout(ctx, h.sendBudget())
	defer cancel()
	err := h.opts.Sender.Send(sendCtx, pushRequest{
		DeviceToken: d.Token,
		Topic:       h.opts.BundleID,
		CollapseID:  spec.CollapseID,
		PushType:    spec.PushType,
		Priority:    spec.Priority,
		Environment: d.Environment,
		Payload:     spec.Payload,
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

// eventID names one specific wait: <host_id>:<session_id>:<generation>.
//
// One function rather than two format strings because the app matches a
// delivered notification to a clear by this exact string. An alert and the clear
// that retracts it must render identical bytes here, and two call sites that
// merely happen to agree are two call sites that can stop agreeing.
func eventID(hostID string, e notifyEvent) string {
	return fmt.Sprintf("%s:%s:%d", hostID, e.SessionID, e.Generation)
}

// buildClearPayload renders the APNs body for a wait that has ended.
//
// A background push: `content-available` and nothing else under `aps`. iOS wakes
// the app, which removes exactly the delivered notification carrying this
// event_id — not the session's other notifications, and not by prefix, so a
// stale clear for generation 5 arriving after generation 6's alert matches
// nothing and does no harm.
//
// The generation comes from the event and is never re-derived. Both paths that
// produce a clear already carry the generation that was alerted: the
// leaving-waiting path builds the event before resetting the entry, and the
// two-missed-ticks path copies e.generation explicitly. Deriving it here from
// the tracker's current state would name a wait that was never pushed.
//
// **Generation reuse across a restart**, reasoned through and deliberately left
// as is: newWaitTracker starts nextGen at zero while host_id is persisted and
// session ids survive, so "H:S:1" can genuinely be issued twice in one device's
// lifetime. That is benign because the id is host- and session-qualified, so a
// collision is confined to the same session on the same host, where "no longer
// waiting" means the same thing on both sides of the restart. The specific case
// worth naming: a session already waiting when the server restarts is adopted as
// phaseAlerted with a fresh generation and no push, so the pre-restart card
// stays on screen — and when the user answers, this clear carries a generation
// that was never pushed and removes that card. That is the outcome we want.
// Persisting nextGen is a change with its own consequences and is not made here.
//
// No waiting_for, no pid, no host name: nobody reads this payload except the
// code that matches event_id, and text in a push nobody sees is payload spent on
// nothing.
func buildClearPayload(hostID string, e notifyEvent) []byte {
	payload := map[string]any{
		"aps":        map[string]any{"content-available": 1},
		"host_id":    hostID,
		"session_id": e.SessionID,
		"event_id":   eventID(hostID, e),
		"cleared":    true,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		// Deliberately still a well-formed background push. A clear that cannot
		// name its event removes nothing, which is the same as not sending it —
		// but sending something malformed would be worse than either.
		return []byte(`{"aps":{"content-available":1}}`)
	}
	if len(data) > apnsPayloadMax {
		// The same belt and braces the alert path has, for the same reason: APNs
		// rejects an oversized body outright and that is indistinguishable from
		// a notification that never arrived. A session id is arbitrary text read
		// off disk and event_id is built from it, so this is reachable without
		// anything else going wrong.
		//
		// Logged, unlike the alert path's, because the degraded form of a clear
		// is inert: with no event_id there is nothing for the app to match, so
		// the card simply stays. Better a line saying why than a card nobody can
		// explain.
		fmt.Fprintf(os.Stderr,
			"claude-sessions: clear for session %s is too large to send (%d bytes); its notification will have to wait for the app to poll\n",
			truncateField(e.SessionID, 32), len(data))
		return []byte(`{"aps":{"content-available":1}}`)
	}
	return data
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
		"event_id":    eventID(hostID, e),
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
