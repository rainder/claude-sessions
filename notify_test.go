package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func waitingSession(id string, waitingFor string) Session {
	return Session{PID: 100, SessionID: id, CWD: "/Users/x/proj", Name: "proj", WaitingFor: waitingFor}
}

func idleSession(id string) Session {
	return Session{PID: 100, SessionID: id, CWD: "/Users/x/proj", Name: "proj", Status: "idle"}
}

// The first tick after start establishes a baseline silently: a server restart
// is a local action taken at the keyboard, and an alert burst there is noise.
func TestWaitTrackerStartupBaselineIsSilent(t *testing.T) {
	tr := newWaitTracker()
	if got := tr.Tick([]Session{waitingSession("a", "permission prompt")}); len(got) != 0 {
		t.Fatalf("first tick emitted %+v, want nothing", got)
	}
	if got := tr.Tick([]Session{waitingSession("a", "permission prompt")}); len(got) != 0 {
		t.Fatalf("second tick emitted %+v, want nothing for a baselined session", got)
	}
}

func TestWaitTrackerAlertsAfterTwoConsecutiveTicks(t *testing.T) {
	tr := newWaitTracker()
	tr.Tick(nil) // baseline

	if got := tr.Tick([]Session{waitingSession("a", "permission prompt")}); len(got) != 0 {
		t.Fatalf("one waiting tick emitted %+v, want nothing (debounce)", got)
	}
	got := tr.Tick([]Session{waitingSession("a", "permission prompt")})
	if len(got) != 1 || got[0].Kind != notifyAlert {
		t.Fatalf("second waiting tick = %+v, want one alert", got)
	}
	if got[0].SessionID != "a" || got[0].WaitingFor != "permission prompt" {
		t.Fatalf("alert = %+v, want session a on a permission prompt", got[0])
	}
	// A generation was assigned. Not its literal value: the counter is seeded
	// from the clock so that two processes cannot issue the same one, so the
	// only thing meaningful about a single generation is that it exists.
	if got[0].Generation <= 0 {
		t.Fatalf("Generation = %d, want a generation to have been assigned", got[0].Generation)
	}
	if again := tr.Tick([]Session{waitingSession("a", "permission prompt")}); len(again) != 0 {
		t.Fatalf("third tick re-alerted: %+v", again)
	}
}

// A prompt answered inside one tick must never notify.
func TestWaitTrackerShortWaitNeverAlerts(t *testing.T) {
	tr := newWaitTracker()
	tr.Tick(nil)
	tr.Tick([]Session{waitingSession("a", "permission prompt")})
	if got := tr.Tick([]Session{idleSession("a")}); len(got) != 0 {
		t.Fatalf("emitted %+v for a sub-tick wait, want nothing", got)
	}
}

func TestWaitTrackerClearsAfterAlert(t *testing.T) {
	tr := newWaitTracker()
	tr.Tick(nil)
	tr.Tick([]Session{waitingSession("a", "permission prompt")})
	tr.Tick([]Session{waitingSession("a", "permission prompt")})

	got := tr.Tick([]Session{idleSession("a")})
	if len(got) != 1 || got[0].Kind != notifyClear {
		t.Fatalf("= %+v, want one clear", got)
	}
}

// After a clear, the same session waiting again is a new event, not a
// permanently-silenced one.
func TestWaitTrackerAlertsAgainAfterClear(t *testing.T) {
	tr := newWaitTracker()
	tr.Tick(nil)
	tr.Tick([]Session{waitingSession("a", "permission prompt")})
	first := tr.Tick([]Session{waitingSession("a", "permission prompt")})
	tr.Tick([]Session{idleSession("a")})

	tr.Tick([]Session{waitingSession("a", "permission prompt")})
	got := tr.Tick([]Session{waitingSession("a", "permission prompt")})
	if len(got) != 1 || got[0].Kind != notifyAlert {
		t.Fatalf("= %+v, want a fresh alert", got)
	}
	// A *different* wait, so a stale notification for the first one cannot pass
	// as this one. Compared against the first rather than asserted literally:
	// the counter is seeded from the clock, so absolute values mean nothing.
	if got[0].Generation <= first[0].Generation {
		t.Fatalf("Generation = %d, want it past the first wait's %d",
			got[0].Generation, first[0].Generation)
	}
}

// Prompt A replaced by prompt B without an intervening non-waiting tick is a
// real sequence and must produce a fresh alert.
func TestWaitTrackerReAlertsWhenWaitingForChanges(t *testing.T) {
	tr := newWaitTracker()
	tr.Tick(nil)
	tr.Tick([]Session{waitingSession("a", "permission prompt")})
	first := tr.Tick([]Session{waitingSession("a", "permission prompt")})

	got := tr.Tick([]Session{waitingSession("a", "user input")})
	if len(got) != 1 || got[0].Kind != notifyAlert {
		t.Fatalf("= %+v, want one alert for the new prompt", got)
	}
	if got[0].WaitingFor != "user input" {
		t.Fatalf("WaitingFor = %q, want %q", got[0].WaitingFor, "user input")
	}
	if got[0].Generation <= first[0].Generation {
		t.Fatalf("Generation = %d, want it past the first prompt's %d",
			got[0].Generation, first[0].Generation)
	}
}

// readSessionFile silently rejects a half-written JSON file, so one missing
// observation is indistinguishable from a mid-write. Clearing on it would
// produce a spurious clear followed by a re-alert.
func TestWaitTrackerToleratesOneMissedTick(t *testing.T) {
	tr := newWaitTracker()
	tr.Tick(nil)
	tr.Tick([]Session{waitingSession("a", "permission prompt")})
	tr.Tick([]Session{waitingSession("a", "permission prompt")})

	if got := tr.Tick(nil); len(got) != 0 {
		t.Fatalf("one absent tick emitted %+v, want nothing", got)
	}
	got := tr.Tick(nil)
	if len(got) != 1 || got[0].Kind != notifyClear {
		t.Fatalf("= %+v, want one clear after two absent ticks", got)
	}
	if got[0].SessionID != "a" {
		t.Fatalf("clear = %+v, want it to identify session a", got[0])
	}
	if final := tr.Tick(nil); len(final) != 0 {
		t.Fatalf("kept emitting after the session was forgotten: %+v", final)
	}
}

// A session that reappears within the tolerance window was never gone.
func TestWaitTrackerMissedTickThenReappearsDoesNotClear(t *testing.T) {
	tr := newWaitTracker()
	tr.Tick(nil)
	tr.Tick([]Session{waitingSession("a", "permission prompt")})
	tr.Tick([]Session{waitingSession("a", "permission prompt")})

	tr.Tick(nil)
	if got := tr.Tick([]Session{waitingSession("a", "permission prompt")}); len(got) != 0 {
		t.Fatalf("emitted %+v when the session came back, want nothing", got)
	}
}

func TestWaitTrackerNeverAlertedSessionDisappearsQuietly(t *testing.T) {
	tr := newWaitTracker()
	tr.Tick(nil)
	tr.Tick([]Session{waitingSession("a", "permission prompt")})
	tr.Tick(nil)
	if got := tr.Tick(nil); len(got) != 0 {
		t.Fatalf("emitted %+v for a session that never alerted", got)
	}
}

// Two rows for one session in a single snapshot must not advance the machine
// twice and defeat the debounce.
func TestWaitTrackerIgnoresDuplicateSessionsInOneSnapshot(t *testing.T) {
	tr := newWaitTracker()
	tr.Tick(nil)
	dupe := []Session{
		waitingSession("a", "permission prompt"),
		waitingSession("a", "permission prompt"),
	}
	if got := tr.Tick(dupe); len(got) != 0 {
		t.Fatalf("emitted %+v from a duplicated row, want nothing", got)
	}
}

func TestWaitTrackerIgnoresBlankSessionIDs(t *testing.T) {
	tr := newWaitTracker()
	tr.Tick(nil)
	blank := []Session{{PID: 1, SessionID: "", WaitingFor: "permission prompt"}}
	tr.Tick(blank)
	if got := tr.Tick(blank); len(got) != 0 {
		t.Fatalf("emitted %+v for a session with no id", got)
	}
}

func TestWaitTrackerEventsAreSortedBySessionID(t *testing.T) {
	tr := newWaitTracker()
	tr.Tick(nil)
	batch := []Session{waitingSession("c", "p"), waitingSession("a", "p"), waitingSession("b", "p")}
	tr.Tick(batch)
	got := tr.Tick(batch)

	if len(got) != 3 {
		t.Fatalf("len(events) = %d, want 3", len(got))
	}
	for i, want := range []string{"a", "b", "c"} {
		if got[i].SessionID != want {
			t.Fatalf("events[%d].SessionID = %q, want %q", i, got[i].SessionID, want)
		}
		if got[i].Kind != notifyAlert {
			t.Fatalf("events[%d].Kind = %v, want an alert", i, got[i].Kind)
		}
	}
}

// A clear must still identify the session well enough to end a Live Activity,
// even when it was produced by absence rather than by an observed snapshot.
func TestWaitTrackerClearOnAbsenceCarriesIdentity(t *testing.T) {
	tr := newWaitTracker()
	tr.Tick(nil)
	tr.Tick([]Session{waitingSession("a", "permission prompt")})
	tr.Tick([]Session{waitingSession("a", "permission prompt")})
	tr.Tick(nil)
	got := tr.Tick(nil)

	if len(got) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(got))
	}
	if got[0].PID != 100 || got[0].CWD != "/Users/x/proj" || got[0].Name != "proj" {
		t.Fatalf("clear lost identity: %+v", got[0])
	}
}

// An evicted entry must not restart generation numbering: a stale notification
// for the first wait would otherwise pass the freshness check on the second.
func TestWaitTrackerGenerationNeverRepeats(t *testing.T) {
	tr := newWaitTracker()
	tr.Tick(nil)
	tr.Tick([]Session{waitingSession("a", "permission prompt")})
	first := tr.Tick([]Session{waitingSession("a", "permission prompt")})
	if len(first) != 1 {
		t.Fatalf("len(first) = %d, want 1", len(first))
	}

	// Two absences evict the entry entirely.
	tr.Tick(nil)
	tr.Tick(nil)

	tr.Tick([]Session{waitingSession("a", "permission prompt")})
	second := tr.Tick([]Session{waitingSession("a", "permission prompt")})
	if len(second) != 1 {
		t.Fatalf("len(second) = %d, want 1", len(second))
	}
	if second[0].Generation == first[0].Generation {
		t.Fatalf("generation %d reused after eviction", second[0].Generation)
	}
}

// A resumed or migrated session keeps its SessionID but gets a new PID. That is
// a new wait: it must re-debounce and then alert, not inherit the old alerted
// state and stay silent forever.
func TestWaitTrackerPIDChangeRestartsTheWait(t *testing.T) {
	tr := newWaitTracker()
	tr.Tick(nil)
	tr.Tick([]Session{waitingSession("a", "permission prompt")})
	tr.Tick([]Session{waitingSession("a", "permission prompt")})

	resumed := waitingSession("a", "permission prompt")
	resumed.PID = 999

	if got := tr.Tick([]Session{resumed}); len(got) != 0 {
		t.Fatalf("emitted %+v on the first tick of a new pid, want a re-debounce", got)
	}
	got := tr.Tick([]Session{resumed})
	if len(got) != 1 || got[0].Kind != notifyAlert {
		t.Fatalf("= %+v, want an alert for the resumed session", got)
	}
	if got[0].PID != 999 {
		t.Fatalf("PID = %d, want 999", got[0].PID)
	}
}

type fakeSender struct {
	mu   sync.Mutex
	sent []pushRequest
	err  error
}

func (f *fakeSender) Send(ctx context.Context, req pushRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, req)
	return f.err
}

// requests returns what was sent, sorted by device token. Arrival order is not
// meaningful now the fan-out is concurrent, and sorting here is the cheap fix —
// the alternative is serialising production code to keep an assertion happy.
func (f *fakeSender) requests() []pushRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]pushRequest(nil), f.sent...)
	sort.Slice(out, func(i, j int) bool { return out[i].DeviceToken < out[j].DeviceToken })
	return out
}

// inOrder returns what was sent, in the order it was sent.
//
// Only meaningful with a single device — the fan-out across devices is
// concurrent and its order is not — but "the clear followed the alert it
// retracts" is exactly a one-device question, and requests() sorts that answer
// away.
func (f *fakeSender) inOrder() []pushRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]pushRequest(nil), f.sent...)
}

// stallingSender never answers for the nominated tokens and answers every other
// one immediately.
//
// It models the real transport in the way that matters here: once the context
// is done, c.http.Do fails at once (apns.go:193-195), so a device dispatched
// behind an exhausted budget never reaches Apple at all.
type stallingSender struct {
	stalls map[string]bool

	mu       sync.Mutex
	accepted []string
}

func newStallingSender(tokens ...string) *stallingSender {
	s := &stallingSender{stalls: map[string]bool{}}
	for _, t := range tokens {
		s.stalls[t] = true
	}
	return s
}

func (s *stallingSender) Send(ctx context.Context, req pushRequest) error {
	// Block outside the lock. Holding it across the wait would serialise every
	// goroutine and make a correct concurrent fan-out look broken.
	if s.stalls[req.DeviceToken] {
		<-ctx.Done()
		return ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accepted = append(s.accepted, req.DeviceToken)
	return nil
}

// acceptedTokens returns the tokens that actually got a push, sorted.
func (s *stallingSender) acceptedTokens() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]string(nil), s.accepted...)
	sort.Strings(out)
	return out
}

// recordingStaller records every arrival and the budget it was handed, then
// blocks until that budget runs out. Nothing ever succeeds, so what it measures
// is purely what each device was given to work with.
type recordingStaller struct {
	mu           sync.Mutex
	arrived      []string
	expired      []string
	minRemaining time.Duration
	seen         bool
}

func (s *recordingStaller) Send(ctx context.Context, req pushRequest) error {
	remaining := time.Duration(0)
	if deadline, ok := ctx.Deadline(); ok {
		remaining = time.Until(deadline)
	}
	s.mu.Lock()
	s.arrived = append(s.arrived, req.DeviceToken)
	if ctx.Err() != nil {
		s.expired = append(s.expired, req.DeviceToken)
	}
	if !s.seen || remaining < s.minRemaining {
		s.seen, s.minRemaining = true, remaining
	}
	s.mu.Unlock()

	// Block outside the lock, or every goroutine serialises here.
	<-ctx.Done()
	return ctx.Err()
}

func (s *recordingStaller) result() (arrived, expired []string, minRemaining time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := append([]string(nil), s.arrived...)
	e := append([]string(nil), s.expired...)
	sort.Strings(a)
	sort.Strings(e)
	return a, e, s.minRemaining
}

// snapshotCollector replays a fixed sequence of snapshots, repeating the last.
func snapshotCollector(snapshots ...[]Session) func() ([]Session, error) {
	i := 0
	return func() ([]Session, error) {
		s := snapshots[min(i, len(snapshots)-1)]
		i++
		return s, nil
	}
}

func TestBuildAlertPayloadShape(t *testing.T) {
	raw := buildAlertPayload("myserver", "9f2c", notifyEvent{
		Kind: notifyAlert, SessionID: "abc-123", PID: 41234,
		CWD: "/Users/x/trecs-brain", Name: "trecs-brain",
		WaitingFor: "permission prompt", Generation: 3,
	})
	var got struct {
		APS struct {
			Alert struct {
				Title string `json:"title"`
				Body  string `json:"body"`
			} `json:"alert"`
			Category        string `json:"category"`
			InterruptionLvl string `json:"interruption-level"`
		} `json:"aps"`
		Host       string `json:"host"`
		HostID     string `json:"host_id"`
		PID        int    `json:"pid"`
		SessionID  string `json:"session_id"`
		EventID    string `json:"event_id"`
		WaitingFor string `json:"waiting_for"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal payload: %v — %s", err, raw)
	}
	if got.APS.Category != "SESSION_WAITING" {
		t.Fatalf("category = %q", got.APS.Category)
	}
	if got.APS.InterruptionLvl != "time-sensitive" {
		t.Fatalf("interruption-level = %q", got.APS.InterruptionLvl)
	}
	if !strings.Contains(got.APS.Alert.Title, "trecs-brain") {
		t.Fatalf("title = %q, want it to name the session", got.APS.Alert.Title)
	}
	if !strings.Contains(got.APS.Alert.Body, "permission prompt") {
		t.Fatalf("body = %q, want it to name the prompt", got.APS.Alert.Body)
	}
	if got.EventID != "9f2c:abc-123:3" {
		t.Fatalf("event_id = %q, want %q", got.EventID, "9f2c:abc-123:3")
	}
	if got.HostID != "9f2c" || got.Host != "myserver" || got.PID != 41234 {
		t.Fatalf("identity fields = %+v", got)
	}
}

// A session with no user-set name falls back to its directory, so the
// notification says something recognisable rather than "-".
func TestBuildAlertPayloadFallsBackToDirectoryName(t *testing.T) {
	raw := buildAlertPayload("h", "i", notifyEvent{
		SessionID: "s", CWD: "/Users/x/some-repo", Name: "-", WaitingFor: "user input",
	})
	if !strings.Contains(string(raw), "some-repo") {
		t.Fatalf("payload = %s, want the directory name", raw)
	}
}

func TestNotifyHubPushesAlertsToEveryDevice(t *testing.T) {
	devices := loadDeviceStore("", fixedClock(time.Now()))
	devices.Upsert(Device{Token: "dev-a", Environment: "production"})
	devices.Upsert(Device{Token: "dev-b", Environment: "sandbox"})
	sender := &fakeSender{}

	waiting := []Session{waitingSession("abc-123", "permission prompt")}
	h := newNotifyHub(notifyHubOptions{
		HostName: "myserver",
		HostID:   "9f2c",
		BundleID: "com.skerla.claude-sessions",
		Devices:  devices,
		Sender:   sender,
		Collect:  snapshotCollector(nil, waiting, waiting),
	})
	defer h.Shutdown()

	for n := 0; n < 3; n++ {
		h.tickOnce(context.Background())
	}

	got := sender.requests()
	if len(got) != 2 {
		t.Fatalf("sent %d pushes, want 2 (one per device): %+v", len(got), got)
	}
	if got[0].CollapseID != "9f2c:abc-123" {
		t.Fatalf("collapse id = %q, want %q", got[0].CollapseID, "9f2c:abc-123")
	}
	if got[0].Topic != "com.skerla.claude-sessions" {
		t.Fatalf("topic = %q", got[0].Topic)
	}
	if got[0].PushType != "alert" || got[0].Priority != "10" {
		t.Fatalf("push type/priority = %q/%q", got[0].PushType, got[0].Priority)
	}
	envs := map[string]bool{got[0].Environment: true, got[1].Environment: true}
	if !envs["production"] || !envs["sandbox"] {
		t.Fatalf("environments = %v, want each device's own", envs)
	}
}

// One device that never answers must not cost every device behind it its push.
//
// The tracker moves the session to phaseAlerted the moment the event is
// produced, and only a change of waitingFor produces another alert, so a push
// lost here is never retried: a starved device does not learn about that wait
// late, it never learns about it at all.
func TestNotifyHubStalledDeviceDoesNotStarveTheRest(t *testing.T) {
	// A serial fan-out can fit only tickBudget/sendBudget sends into one tick,
	// so this many stalled devices exhausts it and every device behind them
	// fails on an already-expired context. Concurrently they cost one budget
	// between them. Keep the total inside maxConcurrentSends so the assertion is
	// about starvation and not about queueing.
	stalled := tickBudgetTicks/sendBudgetTicks + 1
	healthy := []string{"zz-a", "zz-b", "zz-c"}

	devices := loadDeviceStore("", fixedClock(time.Now()))
	var stallTokens []string
	for i := 0; i < stalled; i++ {
		// DeviceStore.List sorts by token, so "aa-" lands ahead of "zz-": the
		// stallers go first, which is the worst case and the one that repeats
		// identically every tick because tokens never change.
		tok := fmt.Sprintf("aa-stall-%d", i)
		stallTokens = append(stallTokens, tok)
		devices.Upsert(Device{Token: tok, Environment: "sandbox"})
	}
	for _, tok := range healthy {
		devices.Upsert(Device{Token: tok, Environment: "sandbox"})
	}
	if total := stalled + len(healthy); total > maxConcurrentSends {
		t.Fatalf("test needs %d concurrent slots, pool has %d", total, maxConcurrentSends)
	}
	sender := newStallingSender(stallTokens...)

	waiting := []Session{waitingSession("abc-123", "permission prompt")}
	h := newNotifyHub(notifyHubOptions{
		HostName: "h", HostID: "i", BundleID: "b",
		Devices: devices, Sender: sender,
		Collect:  snapshotCollector(nil, waiting, waiting),
		Interval: 20 * time.Millisecond,
	})
	defer h.Shutdown()

	// Baseline, debounce, alert — under the same per-tick budget run() uses.
	for n := 0; n < 3; n++ {
		ctx, cancel := context.WithTimeout(context.Background(), h.tickBudget())
		h.tickOnce(ctx)
		cancel()
	}

	got := strings.Join(sender.acceptedTokens(), ",")
	if want := strings.Join(healthy, ","); got != want {
		t.Fatalf("devices that received a push = %q, want %q — stalled devices starved the rest", got, want)
	}
	// A timeout is not evidence a token is dead, so nothing is pruned.
	if n := len(devices.List()); n != stalled+len(healthy) {
		t.Fatalf("registry has %d devices, want %d — a stalled device must not be pruned", n, stalled+len(healthy))
	}
}

// The invariant has to hold at the registry cap, not just for a handful of
// phones. Every registered device stalls, and every one of them must still get
// a send attempt on a context that has not already run out.
//
// context.WithTimeout takes the EARLIER of the parent's deadline and the new
// one, so a device that queues behind a full worker pool inherits the remains
// of the tick budget rather than its own. The only thing that makes the
// per-device budget real is every device starting in the first round — which is
// why maxConcurrentSends covers the whole registry.
func TestNotifyHubGivesEveryDeviceInAFullRegistryALiveBudget(t *testing.T) {
	devices := loadDeviceStore("", fixedClock(time.Now()))
	for i := 0; i < maxRegisteredDevices; i++ {
		devices.Upsert(Device{Token: fmt.Sprintf("dev-%02d", i), Environment: "sandbox"})
	}
	sender := &recordingStaller{}

	waiting := []Session{waitingSession("abc-123", "permission prompt")}
	h := newNotifyHub(notifyHubOptions{
		HostName: "h", HostID: "i", BundleID: "b",
		Devices: devices, Sender: sender,
		Collect:  snapshotCollector(nil, waiting, waiting),
		Interval: 50 * time.Millisecond,
	})
	defer h.Shutdown()

	for n := 0; n < 3; n++ {
		ctx, cancel := context.WithTimeout(context.Background(), h.tickBudget())
		h.tickOnce(ctx)
		cancel()
	}

	arrived, expired, minRemaining := sender.result()
	if len(arrived) != maxRegisteredDevices {
		t.Fatalf("%d of %d devices got a send attempt — the tail starved waiting for a worker slot",
			len(arrived), maxRegisteredDevices)
	}
	if len(expired) != 0 {
		t.Fatalf("%d devices were handed an already-expired context: %v", len(expired), expired)
	}
	// Not the full budget to the nanosecond — 32 goroutines take a moment to
	// get going — but nowhere near the truncated remainder a second round gets.
	if floor := h.sendBudget() / 2; minRemaining < floor {
		t.Fatalf("thinnest budget handed to a device = %v, want at least %v of a %v budget",
			minRemaining, floor, h.sendBudget())
	}
}

// barrierSender answers only once every device has arrived, so it can complete
// at all only if the sends genuinely overlap. A serial fan-out wedges on the
// first device until the tick budget runs out.
type barrierSender struct {
	want    int
	release chan struct{}

	mu       sync.Mutex
	inflight int
	peak     int
	sent     []string
}

func newBarrierSender(want int) *barrierSender {
	return &barrierSender{want: want, release: make(chan struct{})}
}

func (s *barrierSender) Send(ctx context.Context, req pushRequest) error {
	s.mu.Lock()
	s.inflight++
	if s.inflight > s.peak {
		s.peak = s.inflight
	}
	if s.inflight == s.want {
		close(s.release)
	}
	s.mu.Unlock()

	// Wait outside the lock. Waiting inside it would serialise every goroutine
	// and guarantee the deadlock this test exists to rule out.
	select {
	case <-s.release:
	case <-ctx.Done():
		s.mu.Lock()
		s.inflight--
		s.mu.Unlock()
		return ctx.Err()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.inflight--
	s.sent = append(s.sent, req.DeviceToken)
	return nil
}

func (s *barrierSender) result() (peak int, sent int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.peak, len(s.sent)
}

// The fan-out must actually overlap. APNs is HTTP/2 to one host and net/http
// multiplexes the streams, so this is the shape the protocol wants — and it is
// what makes one device's stall stop mattering to the others.
func TestNotifyHubSendsToDevicesConcurrently(t *testing.T) {
	const n = 4
	devices := loadDeviceStore("", fixedClock(time.Now()))
	for i := 0; i < n; i++ {
		devices.Upsert(Device{Token: fmt.Sprintf("dev-%d", i), Environment: "sandbox"})
	}
	sender := newBarrierSender(n)

	waiting := []Session{waitingSession("abc-123", "permission prompt")}
	h := newNotifyHub(notifyHubOptions{
		HostName: "h", HostID: "i", BundleID: "b",
		Devices: devices, Sender: sender,
		Collect:  snapshotCollector(nil, waiting, waiting),
		Interval: 50 * time.Millisecond,
	})
	defer h.Shutdown()

	for k := 0; k < 3; k++ {
		ctx, cancel := context.WithTimeout(context.Background(), h.tickBudget())
		h.tickOnce(ctx)
		cancel()
	}

	peak, sent := sender.result()
	if sent != n {
		t.Fatalf("%d of %d devices completed — the sends did not overlap", sent, n)
	}
	if peak != n {
		t.Fatalf("peak concurrent sends = %d, want %d", peak, n)
	}
}

// DeviceStore.List sorts by token and tokens are stable, so a fan-out that
// always walks it in order puts the same device last on every tick of every
// day. The sort is load-bearing for devices.json determinism, so the rotation
// lives here instead.
func TestNotifyHubRotatesDispatchOrder(t *testing.T) {
	h := newNotifyHub(notifyHubOptions{
		Devices: loadDeviceStore("", fixedClock(time.Now())),
		Sender:  &fakeSender{},
		Collect: func() ([]Session, error) { return nil, nil },
	})
	defer h.Shutdown()

	sorted := []Device{{Token: "a"}, {Token: "b"}, {Token: "c"}}
	var leaders []string
	for n := 0; n < len(sorted); n++ {
		got := h.rotated(sorted)
		seen := map[string]bool{}
		for _, d := range got {
			seen[d.Token] = true
		}
		if len(got) != len(sorted) || len(seen) != len(sorted) {
			t.Fatalf("rotation %d = %+v, want the same three devices", n, got)
		}
		leaders = append(leaders, got[0].Token)
	}
	if got, want := strings.Join(leaders, ","), "a,b,c"; got != want {
		t.Fatalf("leading device across rotations = %q, want %q", got, want)
	}
	if sorted[0].Token != "a" {
		t.Fatalf("rotation mutated its input: %+v", sorted)
	}
}

// The per-device timeout used to be a bare 10s constant inside an 8s parent
// context, so it could never fire — the deadline meant to contain a stall was
// decorative, and that is the specific lie that hid the starvation. Both bounds
// now come from the interval, and the smaller one must stay smaller.
func TestNotifyHubSendBudgetIsReachableInsideTheTickBudget(t *testing.T) {
	for _, interval := range []time.Duration{0, 20 * time.Millisecond, notifyTickInterval, time.Minute} {
		h := newNotifyHub(notifyHubOptions{
			Interval: interval,
			Devices:  loadDeviceStore("", fixedClock(time.Now())),
			Sender:   &fakeSender{},
			Collect:  func() ([]Session, error) { return nil, nil },
		})
		if send, tick := h.sendBudget(), h.tickBudget(); send >= tick {
			t.Fatalf("interval %v: per-device budget %v is not reachable inside the tick budget %v", interval, send, tick)
		}
	}
}

func TestNotifyHubPrunesGoneDevices(t *testing.T) {
	devices := loadDeviceStore("", fixedClock(time.Now()))
	devices.Upsert(Device{Token: "dead"})
	sender := &fakeSender{err: errDeviceGone}

	waiting := []Session{waitingSession("abc-123", "permission prompt")}
	h := newNotifyHub(notifyHubOptions{
		HostName: "myserver", HostID: "9f2c", BundleID: "b",
		Devices: devices, Sender: sender,
		Collect: snapshotCollector(nil, waiting, waiting),
	})
	defer h.Shutdown()

	for n := 0; n < 3; n++ {
		h.tickOnce(context.Background())
	}

	if got := devices.List(); len(got) != 0 {
		t.Fatalf("devices = %+v, want the gone token pruned", got)
	}
}

// A send failure that is not errDeviceGone must keep the device: a transient
// network problem is not a reason to stop notifying a phone forever.
func TestNotifyHubKeepsDeviceOnTransientFailure(t *testing.T) {
	devices := loadDeviceStore("", fixedClock(time.Now()))
	devices.Upsert(Device{Token: "dev"})
	sender := &fakeSender{err: errors.New("connection reset")}

	waiting := []Session{waitingSession("abc-123", "permission prompt")}
	h := newNotifyHub(notifyHubOptions{
		HostName: "h", HostID: "i", BundleID: "b",
		Devices: devices, Sender: sender,
		Collect: snapshotCollector(nil, waiting, waiting),
	})
	defer h.Shutdown()

	for n := 0; n < 3; n++ {
		h.tickOnce(context.Background())
	}

	if got := devices.List(); len(got) != 1 {
		t.Fatalf("devices = %+v, want the device kept", got)
	}
}

// A collection failure must be skipped, not treated as "every session
// vanished" — that would clear every live alert.
func TestNotifyHubSurvivesCollectionErrors(t *testing.T) {
	devices := loadDeviceStore("", fixedClock(time.Now()))
	devices.Upsert(Device{Token: "dev"})
	sender := &fakeSender{}
	h := newNotifyHub(notifyHubOptions{
		HostName: "h", HostID: "i", BundleID: "b",
		Devices: devices, Sender: sender,
		Collect: func() ([]Session, error) { return nil, errors.New("disk on fire") },
	})
	defer h.Shutdown()

	h.tickOnce(context.Background())
	h.tickOnce(context.Background())

	if got := sender.requests(); len(got) != 0 {
		t.Fatalf("sent %+v despite collection failing", got)
	}
}

// Shutdown is deferred by cmdServer and called explicitly by tests; a
// select-then-close would panic when both happen.
func TestNotifyHubShutdownIsIdempotent(t *testing.T) {
	h := newNotifyHub(notifyHubOptions{
		Devices: loadDeviceStore("", fixedClock(time.Now())),
		Sender:  &fakeSender{},
		Collect: func() ([]Session, error) { return nil, nil },
	})
	h.Shutdown()
	h.Shutdown()
}

// Answering a prompt at the keyboard has to take the card off the phone, and the
// only thing that knows the prompt was answered is this hub.
//
// The clear must name the wait that was alerted, not a new one: the app removes
// exactly the delivered notification carrying that event_id, so a re-derived
// generation would match nothing and the card would sit there forever.
func TestNotifyHubPushesAClearWhenAWaitEnds(t *testing.T) {
	devices := loadDeviceStore("", fixedClock(time.Now()))
	devices.Upsert(Device{Token: "dev", Environment: "sandbox"})
	sender := &fakeSender{}

	waiting := []Session{waitingSession("abc-123", "permission prompt")}
	idle := []Session{idleSession("abc-123")}
	h := newNotifyHub(notifyHubOptions{
		HostName: "myserver", HostID: "9f2c", BundleID: "com.skerla.claude-sessions",
		Devices: devices, Sender: sender,
		Collect: snapshotCollector(nil, waiting, waiting, idle),
	})
	defer h.Shutdown()

	for n := 0; n < 4; n++ {
		h.tickOnce(context.Background())
	}

	got := sender.inOrder()
	if len(got) != 2 {
		t.Fatalf("sent %d pushes, want an alert then a clear: %+v", len(got), got)
	}
	alert, clear := got[0], got[1]

	if alert.PushType != "alert" || alert.Priority != "10" {
		t.Fatalf("alert push type/priority = %q/%q, want alert/10", alert.PushType, alert.Priority)
	}
	// APNs rejects a background push sent at priority 10 outright, which
	// produces a clear that silently never arrives.
	if clear.PushType != "background" || clear.Priority != "5" {
		t.Fatalf("clear push type/priority = %q/%q, want background/5", clear.PushType, clear.Priority)
	}
	if clear.Environment != "sandbox" {
		t.Fatalf("clear environment = %q, want the device's own", clear.Environment)
	}
	if clear.Topic != "com.skerla.claude-sessions" {
		t.Fatalf("clear topic = %q", clear.Topic)
	}
	// Identical, so a clear replaces an undelivered alert for that session in
	// APNs's queue rather than queueing behind it.
	if alert.CollapseID != clear.CollapseID {
		t.Fatalf("collapse ids differ: alert %q, clear %q", alert.CollapseID, clear.CollapseID)
	}
	if eventIDOf(t, alert) != eventIDOf(t, clear) {
		t.Fatalf("event ids differ: alert %q, clear %q", eventIDOf(t, alert), eventIDOf(t, clear))
	}
	// Named against the alert rather than a literal: generations are seeded from
	// the clock, and what has to hold is that the clear names the wait that was
	// alerted.
	if want := "9f2c:abc-123:"; !strings.HasPrefix(eventIDOf(t, clear), want) {
		t.Fatalf("clear event_id = %q, want it to name host and session", eventIDOf(t, clear))
	}
}

// The generation the clear carries is the one that was alerted, not the one the
// tracker happens to be on. A session re-alerted for a different prompt has two
// live generations, and clearing the first would leave the second card on screen
// with nothing left to remove it.
func TestNotifyHubClearCarriesTheGenerationThatWasAlerted(t *testing.T) {
	devices := loadDeviceStore("", fixedClock(time.Now()))
	devices.Upsert(Device{Token: "dev"})
	sender := &fakeSender{}

	first := []Session{waitingSession("abc-123", "permission prompt")}
	second := []Session{waitingSession("abc-123", "a different prompt")}
	idle := []Session{idleSession("abc-123")}
	h := newNotifyHub(notifyHubOptions{
		HostName: "h", HostID: "9f2c", BundleID: "b",
		Devices: devices, Sender: sender,
		Collect: snapshotCollector(nil, first, first, second, idle),
	})
	defer h.Shutdown()

	for n := 0; n < 5; n++ {
		h.tickOnce(context.Background())
	}

	got := sender.inOrder()
	if len(got) != 3 {
		t.Fatalf("sent %d pushes, want two alerts and a clear: %+v", len(got), got)
	}
	// The second wait's, not the first's. A session re-alerted for a different
	// prompt has two generations behind it, and clearing the older one would
	// leave the card that is actually on screen with nothing left to remove it.
	if eventIDOf(t, got[2]) != eventIDOf(t, got[1]) {
		t.Fatalf("clear event_id = %q, want the second alert's %q",
			eventIDOf(t, got[2]), eventIDOf(t, got[1]))
	}
	if eventIDOf(t, got[2]) == eventIDOf(t, got[0]) {
		t.Fatalf("clear event_id = %q, which is the *first* wait's", eventIDOf(t, got[2]))
	}
}

// A session that exits while it is waiting never stops waiting — it just stops
// being there. Its card has to go too.
func TestNotifyHubPushesAClearWhenASessionDisappears(t *testing.T) {
	devices := loadDeviceStore("", fixedClock(time.Now()))
	devices.Upsert(Device{Token: "dev"})
	sender := &fakeSender{}

	waiting := []Session{waitingSession("abc-123", "permission prompt")}
	h := newNotifyHub(notifyHubOptions{
		HostName: "h", HostID: "9f2c", BundleID: "b",
		Devices: devices, Sender: sender,
		// Two absent ticks: one absence is indistinguishable from a
		// half-written session file.
		Collect: snapshotCollector(nil, waiting, waiting, nil, nil),
	})
	defer h.Shutdown()

	for n := 0; n < 4; n++ {
		h.tickOnce(context.Background())
	}
	if got := sender.inOrder(); len(got) != 1 {
		t.Fatalf("one missed tick already cleared: %+v", got)
	}
	h.tickOnce(context.Background())

	got := sender.inOrder()
	if len(got) != 2 {
		t.Fatalf("sent %d pushes, want the alert and a clear: %+v", len(got), got)
	}
	if got[1].PushType != "background" || got[1].Priority != "5" {
		t.Fatalf("clear push type/priority = %q/%q", got[1].PushType, got[1].Priority)
	}
	if eventIDOf(t, got[1]) != eventIDOf(t, got[0]) {
		t.Fatalf("clear event_id = %q, want the alert's %q",
			eventIDOf(t, got[1]), eventIDOf(t, got[0]))
	}
}

// A background push carrying an alert is not a background push: iOS shows it,
// and the user gets a blank notification every time they answer a prompt.
func TestBuildClearPayloadShape(t *testing.T) {
	raw := buildClearPayload("9f2c", notifyEvent{
		Kind: notifyClear, SessionID: "abc-123", PID: 41234,
		CWD: "/Users/x/trecs-brain", Name: "trecs-brain",
		WaitingFor: "permission prompt", Generation: 6,
	})

	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal payload: %v — %s", err, raw)
	}
	aps, ok := got["aps"].(map[string]any)
	if !ok {
		t.Fatalf("no aps dictionary in %s", raw)
	}
	if aps["content-available"] != float64(1) {
		t.Fatalf("content-available = %v, want 1", aps["content-available"])
	}
	if len(aps) != 1 {
		t.Fatalf("aps = %v, want content-available and nothing else", aps)
	}
	if got["event_id"] != "9f2c:abc-123:6" {
		t.Fatalf("event_id = %v", got["event_id"])
	}
	if got["cleared"] != true {
		t.Fatalf("cleared = %v, want true", got["cleared"])
	}
	if got["host_id"] != "9f2c" || got["session_id"] != "abc-123" {
		t.Fatalf("identity fields = %v", got)
	}
	// The prompt text is what the alert said; repeating it in a push nobody
	// sees is payload spent on nothing.
	if _, ok := got["waiting_for"]; ok {
		t.Fatalf("clear carries waiting_for: %s", raw)
	}
}

// The alert and the clear for one wait are matched by the app on event_id and by
// APNs on collapse id. A realistic pair — a 32-hex host id and a UUID session id
// — is 69 characters, which is over the 64 APNs allows, so both are truncated
// today. That is fine only while they truncate to the same bytes.
func TestAlertAndClearShareACollapseIDEvenWhenTruncated(t *testing.T) {
	hostID := strings.Repeat("a", 32)
	sessionID := "6f1d2c3b-4a59-4e6f-8b0c-1d2e3f4a5b6c"

	devices := loadDeviceStore("", fixedClock(time.Now()))
	devices.Upsert(Device{Token: "dev"})
	sender := &fakeSender{}

	waiting := []Session{waitingSession(sessionID, "permission prompt")}
	idle := []Session{idleSession(sessionID)}
	h := newNotifyHub(notifyHubOptions{
		HostName: "h", HostID: hostID, BundleID: "b",
		Devices: devices, Sender: sender,
		Collect: snapshotCollector(nil, waiting, waiting, idle),
	})
	defer h.Shutdown()

	for n := 0; n < 4; n++ {
		h.tickOnce(context.Background())
	}

	got := sender.inOrder()
	if len(got) != 2 {
		t.Fatalf("sent %d pushes, want an alert and a clear: %+v", len(got), got)
	}
	// The premise of this test: without it, equal-and-short would pass and say
	// nothing about the case that actually happens.
	if len(got[0].CollapseID) <= apnsCollapseIDMax {
		t.Fatalf("collapse id is %d characters, so this test is not exercising truncation",
			len(got[0].CollapseID))
	}
	if got[0].CollapseID[:apnsCollapseIDMax] != got[1].CollapseID[:apnsCollapseIDMax] {
		t.Fatalf("truncated collapse ids differ: alert %q, clear %q",
			got[0].CollapseID[:apnsCollapseIDMax], got[1].CollapseID[:apnsCollapseIDMax])
	}
}

// A restart is the one thing that used to make an event_id repeat.
//
// nextGen started at its zero value in every process, while host_id is persisted
// and session ids survive, so "H:S:1" could genuinely be issued twice in one
// device's lifetime. A clear for the first one, delayed across the restart, then
// exact-matches the alert for the second and takes a live card off the screen —
// the single failure the exact-match rule exists to prevent.
func TestWaitTrackerGenerationsDoNotCollideAcrossARestart(t *testing.T) {
	start := time.Now()
	before := newWaitTrackerAt(start)
	// A restart one second later. That is the *smallest* gap a restart can have
	// and still be two processes; a real one is minutes.
	after := newWaitTrackerAt(start.Add(time.Second))

	issued := map[int]bool{}
	for i := 0; i < 1000; i++ {
		issued[before.bumpGeneration()] = true
	}
	for i := 0; i < 1000; i++ {
		if g := after.bumpGeneration(); issued[g] {
			t.Fatalf("generation %d was issued by both processes", g)
		}
	}
}

// And the seed must not break the property the counter already had within one
// process.
func TestWaitTrackerGenerationsAreStillMonotonicWithinAProcess(t *testing.T) {
	tr := newWaitTrackerAt(time.Now())
	first := tr.bumpGeneration()
	second := tr.bumpGeneration()
	if second <= first {
		t.Fatalf("generations went %d then %d, want strictly increasing", first, second)
	}
}

// clearStallingSender answers alerts at once and never answers a background
// push, which is how a clear spends a tick that an alert needed.
type clearStallingSender struct {
	mu       sync.Mutex
	accepted []string
}

func (s *clearStallingSender) Send(ctx context.Context, req pushRequest) error {
	if req.PushType == "background" {
		<-ctx.Done()
		return ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accepted = append(s.accepted, req.PushType+":"+req.CollapseID)
	return nil
}

func (s *clearStallingSender) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accepted = nil
}

func (s *clearStallingSender) got() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.accepted...)
}

// An alert is the product; a clear is housekeeping. They share one tick budget
// and are dispatched one after another, so ordering them by session id alone
// lets a clear that stalls spend the budget an alert in the same tick needed —
// and a lost alert is never retried, because the tracker has already moved that
// session to phaseAlerted.
//
// Session "a" sorts before "b", so by-id ordering puts the clear first.
func TestNotifyHubDispatchesAlertsBeforeClearsInOneTick(t *testing.T) {
	devices := loadDeviceStore("", fixedClock(time.Now()))
	devices.Upsert(Device{Token: "dev"})
	sender := &clearStallingSender{}

	aWaiting := []Session{waitingSession("a", "permission prompt")}
	bothWaiting := []Session{waitingSession("a", "permission prompt"), waitingSession("b", "permission prompt")}
	aDoneBWaiting := []Session{idleSession("a"), waitingSession("b", "permission prompt")}
	h := newNotifyHub(notifyHubOptions{
		HostName: "h", HostID: "9f2c", BundleID: "b",
		Devices: devices, Sender: sender,
		Collect: snapshotCollector(nil, aWaiting, aWaiting, bothWaiting, aDoneBWaiting),
	})
	defer h.Shutdown()

	for n := 0; n < 4; n++ {
		h.tickOnce(context.Background())
	}
	sender.reset()

	// A tick with almost nothing left, which is what a fan-out to a full
	// registry leaves behind. The stalled clear will consume all of it.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	h.tickOnce(ctx)

	got := sender.got()
	if len(got) != 1 || got[0] != "alert:9f2c:b" {
		t.Fatalf("accepted %v, want the alert for session b to have been sent", got)
	}
}

// A session id is arbitrary text read off disk, and event_id is built from it.
// An oversized payload is rejected by APNs outright, which is indistinguishable
// from a clear that never arrived.
func TestBuildClearPayloadStaysWithinAPNsLimit(t *testing.T) {
	raw := buildClearPayload("9f2c", notifyEvent{
		Kind: notifyClear, SessionID: strings.Repeat("s", 5000), Generation: 6,
	})
	if len(raw) > apnsPayloadMax {
		t.Fatalf("payload is %d bytes, over the %d APNs accepts", len(raw), apnsPayloadMax)
	}
	// Still a background push, so it cannot suddenly display something.
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal payload: %v — %s", err, raw)
	}
	aps, ok := got["aps"].(map[string]any)
	if !ok || aps["content-available"] != float64(1) || len(aps) != 1 {
		t.Fatalf("degraded payload is not a background push: %s", raw)
	}
}

// eventIDOf pulls event_id back out of a rendered payload, so a test asserts on
// what a device would actually receive rather than on what was passed in.
func eventIDOf(t *testing.T, req pushRequest) string {
	t.Helper()
	var body struct {
		EventID string `json:"event_id"`
	}
	if err := json.Unmarshal(req.Payload, &body); err != nil {
		t.Fatalf("unmarshal payload: %v — %s", err, req.Payload)
	}
	return body.EventID
}

// Session names and prompts are arbitrary text. An oversized payload is
// rejected by APNs outright, which is indistinguishable from a notification
// that never arrived.
func TestBuildAlertPayloadStaysWithinAPNsLimit(t *testing.T) {
	huge := strings.Repeat("A", 5000)
	raw := buildAlertPayload(huge, "9f2c", notifyEvent{
		SessionID: huge, CWD: huge, Name: huge, WaitingFor: huge, Generation: 1,
	})
	if len(raw) > apnsPayloadMax {
		t.Fatalf("payload is %d bytes, want <= %d", len(raw), apnsPayloadMax)
	}
	var probe map[string]any
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("degraded payload is not valid json: %v", err)
	}
	if _, ok := probe["aps"]; !ok {
		t.Fatalf("payload lost its aps dictionary: %s", raw)
	}
}

func TestTruncateField(t *testing.T) {
	if got := truncateField("short", 200); got != "short" {
		t.Fatalf("truncateField shortened a short value: %q", got)
	}
	got := truncateField(strings.Repeat("x", 500), 10)
	if len([]rune(got)) != 10 {
		t.Fatalf("truncateField returned %d runes, want 10", len([]rune(got)))
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("truncateField = %q, want a visible ellipsis", got)
	}
}
