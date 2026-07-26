package main

import (
	"context"
	"encoding/json"
	"errors"
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
	if got[0].Generation != 1 {
		t.Fatalf("Generation = %d, want 1", got[0].Generation)
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
	tr.Tick([]Session{waitingSession("a", "permission prompt")})
	tr.Tick([]Session{idleSession("a")})

	tr.Tick([]Session{waitingSession("a", "permission prompt")})
	got := tr.Tick([]Session{waitingSession("a", "permission prompt")})
	if len(got) != 1 || got[0].Kind != notifyAlert {
		t.Fatalf("= %+v, want a fresh alert", got)
	}
	if got[0].Generation != 2 {
		t.Fatalf("Generation = %d, want 2", got[0].Generation)
	}
}

// Prompt A replaced by prompt B without an intervening non-waiting tick is a
// real sequence and must produce a fresh alert.
func TestWaitTrackerReAlertsWhenWaitingForChanges(t *testing.T) {
	tr := newWaitTracker()
	tr.Tick(nil)
	tr.Tick([]Session{waitingSession("a", "permission prompt")})
	tr.Tick([]Session{waitingSession("a", "permission prompt")})

	got := tr.Tick([]Session{waitingSession("a", "user input")})
	if len(got) != 1 || got[0].Kind != notifyAlert {
		t.Fatalf("= %+v, want one alert for the new prompt", got)
	}
	if got[0].WaitingFor != "user input" {
		t.Fatalf("WaitingFor = %q, want %q", got[0].WaitingFor, "user input")
	}
	if got[0].Generation != 2 {
		t.Fatalf("Generation = %d, want 2", got[0].Generation)
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

func (f *fakeSender) requests() []pushRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]pushRequest(nil), f.sent...)
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
		BundleID: "com.avisoma.claude-sessions",
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
	if got[0].Topic != "com.avisoma.claude-sessions" {
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

// Clear events have no consumer until Live Activities land. The hub must not
// try to push them as alerts in the meantime.
func TestNotifyHubDoesNotPushClears(t *testing.T) {
	devices := loadDeviceStore("", fixedClock(time.Now()))
	devices.Upsert(Device{Token: "dev"})
	sender := &fakeSender{}

	waiting := []Session{waitingSession("abc-123", "permission prompt")}
	idle := []Session{idleSession("abc-123")}
	h := newNotifyHub(notifyHubOptions{
		HostName: "h", HostID: "i", BundleID: "b",
		Devices: devices, Sender: sender,
		Collect: snapshotCollector(nil, waiting, waiting, idle),
	})
	defer h.Shutdown()

	for n := 0; n < 4; n++ {
		h.tickOnce(context.Background())
	}

	if got := sender.requests(); len(got) != 1 {
		t.Fatalf("sent %d pushes, want only the alert: %+v", len(got), got)
	}
}
