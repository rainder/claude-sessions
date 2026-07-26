package main

import "testing"

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
