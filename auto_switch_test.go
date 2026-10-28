package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// autoSwitchHarness is a test server with its recorded switches and events,
// the seam's current accounts, the usage it reads and its clock.
type autoSwitchHarness struct {
	s        *server
	usage    *[]claudeUsageNumbers
	switches *[]fleetSwitchCall
	events   *[]fleetEventCall
	current  map[string]string
	now      *time.Time
	names    *[]string
}

// autoSwitchTestServer is fleetAccountServer with the auto-switch loop
// enabled at threshold 95 in a temp dir, a pinned clock, saved claude
// accounts home (live), work, spare, alpha and beta, and usage read from
// *usage.
func autoSwitchTestServer(t *testing.T) autoSwitchHarness {
	t.Helper()
	s, switches, events, current := fleetAccountServer(t, nil)
	now := time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)
	s.fleet.now = func() time.Time { return now }
	s.auto.now = func() time.Time { return now }
	names := []string{"alpha", "beta", "home", "spare", "work"}
	s.fleet.accountNames = func(p string) ([]string, error) {
		if p == accountToolClaude {
			return names, nil
		}
		return []string{"gx", "gy"}, nil
	}
	// No cached reading: every tick asks the (test) fresh source.
	s.auto.cachedUsage = func() []claudeUsageNumbers { return nil }
	var usage []claudeUsageNumbers
	s.fleet.claudeUsage = func() []claudeUsageNumbers { return usage }
	writeAutoSwitchConfig(t, s, `{"enabled": true}`)
	return autoSwitchHarness{s: s, usage: &usage, switches: switches, events: events, current: current, now: &now, names: &names}
}

func writeAutoSwitchConfig(t *testing.T, s *server, body string) {
	t.Helper()
	if err := os.WriteFile(s.auto.configPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	// A rewrite inside one mtime tick with the same size would be missed;
	// tests always want it read.
	s.auto.mu.Lock()
	s.auto.cfgSeen = false
	s.auto.mu.Unlock()
}

func win(p float64) *usageWindowNumbers { return &usageWindowNumbers{Pct: p} }

func acct(name string, active bool, seven float64) claudeUsageNumbers {
	return claudeUsageNumbers{Name: name, Account: name + "@example.com", Active: active, SevenDay: win(seven), FiveHour: win(1)}
}

func (h autoSwitchHarness) state(t *testing.T) autoSwitchState {
	t.Helper()
	st, err := loadAutoSwitchState(h.s.auto.statePath)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func (h autoSwitchHarness) autoLog(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(h.s.auto.logPath)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(data)
}

func TestAutoSwitchTriggersAtThreshold(t *testing.T) {
	h := autoSwitchTestServer(t)
	spare := acct("spare", false, 30)
	spare.FiveHour = win(10)
	*h.usage = []claudeUsageNumbers{acct("home", true, 95), acct("work", false, 40), spare}
	h.s.autoSwitchTick()
	if len(*h.switches) != 1 || (*h.switches)[0] != (fleetSwitchCall{"claude", "spare"}) {
		t.Fatalf("switches %v", *h.switches)
	}
	want := "[claude-sessions event] account claude auto-switched from home to spare (7-day 95% -> 30%, threshold 95%); " + fleetSwitchNoteClaude
	if len(*h.events) != 1 || (*h.events)[0].text != want {
		t.Fatalf("events %v", *h.events)
	}
	lines := readFleetLogLines(t, h.s)
	if len(lines) != 1 || lines[0].Type != "auto-switch" || lines[0].Text != "claude account home -> spare (7-day 95% -> 30%, threshold 95%)" {
		t.Fatalf("log %+v", lines)
	}
	st := h.state(t)
	if st.Last == nil || !st.Last.OK || st.Last.From != "home" || st.Last.To != "spare" || st.Last.FromPct != 95 || st.Last.ToPct != 30 ||
		st.LastOK == nil || st.LastOK.At != "2026-10-28T10:00:00Z" {
		t.Fatalf("state %+v %+v", st.Last, st.LastOK)
	}
	if l := h.autoLog(t); !strings.Contains(l, "switched from=home to=spare") || strings.Contains(l, "@") {
		t.Fatalf("auto log %q", l)
	}
	if info, err := os.Stat(h.s.auto.statePath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("state file mode: %v %v", info, err)
	}
}

func TestAutoSwitchBelowThresholdDoesNothing(t *testing.T) {
	h := autoSwitchTestServer(t)
	*h.usage = []claudeUsageNumbers{acct("home", true, 94.9), acct("work", false, 10)}
	h.s.autoSwitchTick()
	if len(*h.switches) != 0 || len(*h.events) != 0 {
		t.Fatalf("%v %v", *h.switches, *h.events)
	}
}

func TestAutoSwitchActiveReadingMustBeFreshAndSaved(t *testing.T) {
	stale := acct("home", true, 99)
	stale.Stale = true
	expired := acct("home", true, 99)
	expired.Expired = true
	noSeven := acct("home", true, 99)
	noSeven.SevenDay = nil
	unsaved := acct("", true, 99)
	cases := map[string][]claudeUsageNumbers{
		"stale":     {stale, acct("work", false, 10)},
		"expired":   {expired, acct("work", false, 10)},
		"no 7-day":  {noSeven, acct("work", false, 10)},
		"unsaved":   {unsaved, acct("work", false, 10)},
		"no active": {acct("home", false, 99), acct("work", false, 10)},
		"none":      nil,
	}
	for name, usage := range cases {
		t.Run(name, func(t *testing.T) {
			h := autoSwitchTestServer(t)
			*h.usage = usage
			h.s.autoSwitchTick()
			if len(*h.switches) != 0 || len(*h.events) != 0 {
				t.Fatalf("%v %v", *h.switches, *h.events)
			}
			if st := h.state(t); st.Last != nil || st.NoTargetEpisode {
				t.Fatalf("state %+v", st)
			}
		})
	}
}

func TestAutoSwitchSkipsBadCandidates(t *testing.T) {
	h := autoSwitchTestServer(t)
	stale := acct("work", false, 1)
	stale.Stale = true
	expired := acct("spare", false, 1)
	expired.Expired = true
	fiveOver := acct("alpha", false, 1)
	fiveOver.FiveHour = win(95)
	unlisted := acct("ghost", false, 1)
	noSeven := acct("beta", false, 1)
	noSeven.SevenDay = nil
	*h.usage = []claudeUsageNumbers{acct("home", true, 97), stale, expired, fiveOver, unlisted, noSeven}
	h.s.autoSwitchTick()
	if len(*h.switches) != 0 {
		t.Fatalf("switched %v", *h.switches)
	}
	// Over the threshold on 7-day is no target either.
	*h.usage = []claudeUsageNumbers{acct("home", true, 97), acct("work", false, 95)}
	h.s.autoSwitchTick()
	if len(*h.switches) != 0 {
		t.Fatalf("switched %v", *h.switches)
	}
}

func TestAutoSwitchLowestSevenDayWinsTieByName(t *testing.T) {
	h := autoSwitchTestServer(t)
	noFive := acct("beta", false, 20)
	noFive.FiveHour = nil // no 5-hour reading counts as below
	*h.usage = []claudeUsageNumbers{acct("home", true, 99), acct("work", false, 20), noFive, acct("alpha", false, 20), acct("spare", false, 50)}
	h.s.autoSwitchTick()
	if len(*h.switches) != 1 || (*h.switches)[0].name != "alpha" {
		t.Fatalf("switches %v", *h.switches)
	}
	got, ok, _ := autoSwitchTarget([]claudeUsageNumbers{acct("home", true, 99), acct("work", false, 20), noFive}, "home", []string{"beta", "home", "work"}, 95)
	if !ok || got.Name != "beta" {
		t.Fatalf("target %+v %v", got, ok)
	}
}

func TestAutoSwitchCooldown(t *testing.T) {
	h := autoSwitchTestServer(t)
	*h.usage = []claudeUsageNumbers{acct("home", true, 96), acct("work", false, 10), acct("spare", false, 20)}
	h.s.autoSwitchTick()
	if len(*h.switches) != 1 {
		t.Fatalf("switches %v", *h.switches)
	}
	// work is live now and at the threshold too.
	*h.usage = []claudeUsageNumbers{acct("work", true, 96), acct("home", false, 96), acct("spare", false, 20)}
	*h.now = h.now.Add(29 * time.Minute)
	h.s.autoSwitchTick()
	if len(*h.switches) != 1 {
		t.Fatalf("switched inside the cooldown: %v", *h.switches)
	}
	*h.now = h.now.Add(2 * time.Minute)
	h.s.autoSwitchTick()
	if len(*h.switches) != 2 || (*h.switches)[1] != (fleetSwitchCall{"claude", "spare"}) {
		t.Fatalf("switches %v", *h.switches)
	}
}

func TestAutoSwitchFailureStartsCooldown(t *testing.T) {
	h := autoSwitchTestServer(t)
	calls := 0
	h.s.switchAcct = func(tool, name string) (string, []string, error) {
		calls++
		return "", nil, errors.New("keychain says no for someone@example.com")
	}
	*h.usage = []claudeUsageNumbers{acct("home", true, 96), acct("work", false, 10)}
	h.s.autoSwitchTick()
	*h.now = h.now.Add(10 * time.Minute)
	h.s.autoSwitchTick()
	want := "[claude-sessions event] account claude auto-switch from home to work failed (switch_failed); 7-day 96%, threshold 95%; next try in 30 min"
	if calls != 1 || len(*h.events) != 1 || (*h.events)[0].text != want {
		t.Fatalf("calls %d events %v", calls, *h.events)
	}
	st := h.state(t)
	if st.Last == nil || st.Last.OK || st.Last.Error != "switch_failed" || st.LastOK != nil {
		t.Fatalf("state %+v", st.Last)
	}
	if l := h.autoLog(t); !strings.Contains(l, "switch failed code=switch_failed") || strings.Contains(l, "@") {
		t.Fatalf("auto log %q", l)
	}
	if _, err := os.Stat(filepath.Join(h.s.fleet.dir, "log.jsonl")); !os.IsNotExist(err) {
		t.Fatal("a failed switch must not write log.jsonl")
	}
}

func TestAutoSwitchMovedAccountIsRefused(t *testing.T) {
	h := autoSwitchTestServer(t)
	h.current["claude"] = "work" // the numbers still say home is live
	*h.usage = []claudeUsageNumbers{acct("home", true, 96), acct("spare", false, 10)}
	h.s.autoSwitchTick()
	if len(*h.switches) != 0 || len(*h.events) != 1 || !strings.Contains((*h.events)[0].text, "auto-switch from home to spare failed (moved)") {
		t.Fatalf("%v %v", *h.switches, *h.events)
	}
	if st := h.state(t); st.Last == nil || st.Last.Error != "moved" {
		t.Fatalf("state %+v", st.Last)
	}
}

func TestAutoSwitchAlreadyLiveSendsNothing(t *testing.T) {
	h := autoSwitchTestServer(t)
	h.current["claude"] = "spare" // the target is live already
	*h.usage = []claudeUsageNumbers{acct("home", true, 96), acct("spare", false, 10)}
	h.s.autoSwitchTick()
	if len(*h.switches) != 0 || len(*h.events) != 0 {
		t.Fatalf("%v %v", *h.switches, *h.events)
	}
	if st := h.state(t); st.Last == nil || st.Last.Error != "already_live" {
		t.Fatalf("state %+v", st.Last)
	}
}

func TestAutoSwitchBusySkipsWithoutCooldown(t *testing.T) {
	h := autoSwitchTestServer(t)
	*h.usage = []claudeUsageNumbers{acct("home", true, 96), acct("work", false, 10)}
	h.s.fleet.switchMu.Lock()
	h.s.autoSwitchTick()
	h.s.fleet.switchMu.Unlock()
	if len(*h.switches) != 0 {
		t.Fatalf("switched while busy: %v", *h.switches)
	}
	if st := h.state(t); st.Last != nil {
		t.Fatalf("busy must not record an attempt: %+v", st.Last)
	}
	*h.now = h.now.Add(2 * time.Minute)
	h.s.autoSwitchTick()
	if len(*h.switches) != 1 {
		t.Fatalf("the next poll must switch: %v", *h.switches)
	}
}

func TestAutoSwitchNoTargetOncePerEpisode(t *testing.T) {
	h := autoSwitchTestServer(t)
	*h.names = []string{"home", "work"}
	over := []claudeUsageNumbers{acct("home", true, 96.4), acct("work", false, 97)}
	*h.usage = over
	h.s.autoSwitchTick()
	*h.now = h.now.Add(2 * time.Minute)
	h.s.autoSwitchTick()
	want := "[claude-sessions event] account claude at 96% 7-day, threshold 95%; no saved account below it to switch to"
	if len(*h.events) != 1 || (*h.events)[0].text != want {
		t.Fatalf("events %v", *h.events)
	}
	if n := strings.Count(h.autoLog(t), "no target pct=96"); n != 1 {
		t.Fatalf("no-target log lines %d", n)
	}
	if st := h.state(t); !st.NoTargetEpisode || st.LastNoTarget != "2026-10-28T10:00:00Z" {
		t.Fatalf("state %+v", st)
	}
	// Below the threshold ends the episode; the next one sends again.
	*h.usage = []claudeUsageNumbers{acct("home", true, 80)}
	h.s.autoSwitchTick()
	if st := h.state(t); st.NoTargetEpisode {
		t.Fatal("episode must reset below the threshold")
	}
	*h.usage = over
	h.s.autoSwitchTick()
	if len(*h.events) != 2 {
		t.Fatalf("events %v", *h.events)
	}
	// The log line waits 6 h.
	if n := strings.Count(h.autoLog(t), "no target pct="); n != 1 {
		t.Fatalf("no-target log lines %d", n)
	}
	*h.now = h.now.Add(6 * time.Hour)
	h.s.autoSwitchTick()
	if n := strings.Count(h.autoLog(t), "no target pct="); n != 2 {
		t.Fatalf("no-target log lines %d", n)
	}
	if len(*h.switches) != 0 {
		t.Fatalf("switched %v", *h.switches)
	}
}

func TestAutoSwitchNoTargetRetriesUndelivered(t *testing.T) {
	h := autoSwitchTestServer(t)
	*h.names = []string{"home"}
	codes := []string{"target_missing", "sent"}
	var texts []string
	h.s.fleet.deliverEvent = func(target, text string) string {
		texts = append(texts, text)
		c := codes[0]
		codes = codes[1:]
		return c
	}
	*h.usage = []claudeUsageNumbers{acct("home", true, 99)}
	h.s.autoSwitchTick()
	if st := h.state(t); st.NoTargetEpisode {
		t.Fatal("an undelivered line must not end the episode")
	}
	h.s.autoSwitchTick()
	h.s.autoSwitchTick()
	if len(texts) != 2 || !h.state(t).NoTargetEpisode {
		t.Fatalf("texts %v", texts)
	}
}

func TestAutoSwitchSuccessResetsEpisode(t *testing.T) {
	h := autoSwitchTestServer(t)
	*h.names = []string{"home", "work"}
	*h.usage = []claudeUsageNumbers{acct("home", true, 99), acct("work", false, 99)}
	h.s.autoSwitchTick()
	*h.usage = []claudeUsageNumbers{acct("home", true, 99), acct("work", false, 10)}
	h.s.autoSwitchTick()
	if len(*h.switches) != 1 || h.state(t).NoTargetEpisode {
		t.Fatalf("switches %v state %+v", *h.switches, h.state(t))
	}
}

func TestAutoSwitchDisabledReadsNothing(t *testing.T) {
	h := autoSwitchTestServer(t)
	h.s.fleet.claudeUsage = func() []claudeUsageNumbers { panic("read usage while off") }
	writeAutoSwitchConfig(t, h.s, `{"enabled": false}`)
	h.s.autoSwitchTick()
	_ = os.Remove(h.s.auto.configPath)
	h.s.auto.cfgSeen = false
	h.s.autoSwitchTick()
}

func TestAutoSwitchBadConfigKeepsLastGood(t *testing.T) {
	h := autoSwitchTestServer(t)
	writeAutoSwitchConfig(t, h.s, `{"enabled": true, "threshold": 90}`)
	h.s.auto.mu.Lock()
	h.s.auto.reloadConfigLocked()
	h.s.auto.mu.Unlock()
	if err := os.WriteFile(h.s.auto.configPath, []byte(`{"enabled": true, "threshhold": 80, "x": 1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	h.s.auto.mu.Lock()
	cfg := h.s.auto.reloadConfigLocked()
	h.s.auto.mu.Unlock()
	if !cfg.Enabled || cfg.Threshold != 90 {
		t.Fatalf("cfg %+v", cfg)
	}
	if !strings.Contains(h.autoLog(t), "config error") {
		t.Fatal("a bad config must be logged")
	}
}

func TestParseAutoSwitchConfig(t *testing.T) {
	c, err := parseAutoSwitchConfig([]byte(`{}`))
	if err != nil || c != (autoSwitchConfig{Threshold: 95, PollSeconds: 120}) {
		t.Fatalf("%+v %v", c, err)
	}
	c, err = parseAutoSwitchConfig([]byte(`{"enabled": true, "threshold": 92.5, "pollSeconds": 60}`))
	if err != nil || !c.Enabled || c.Threshold != 92.5 || c.PollSeconds != 60 {
		t.Fatalf("%+v %v", c, err)
	}
	for _, bad := range []string{
		`{"threshold": 49}`, `{"threshold": 101}`, `{"pollSeconds": 59}`, `{"pollSeconds": 3601}`,
		`{"pollSeconds": 90.5}`, `{"enabled": "yes"}`, `{"enabeld": true}`, `nope`, `{} {}`,
	} {
		if _, err := parseAutoSwitchConfig([]byte(bad)); err == nil {
			t.Errorf("%s must be refused", bad)
		}
	}
	for _, ok := range []string{`{"threshold": 50}`, `{"threshold": 100}`, `{"pollSeconds": 3600}`} {
		if _, err := parseAutoSwitchConfig([]byte(ok)); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
}

func TestAutoSwitchStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	d := &autoSwitchDeps{configPath: filepath.Join(dir, "c.json"), statePath: filepath.Join(dir, "state", "s.json"), logPath: filepath.Join(dir, "state", "l.log")}
	rec := &autoSwitchRecord{From: "a", To: "b", FromPct: 96, ToPct: 12, Threshold: 95, At: "2026-10-28T10:00:00Z", OK: true}
	d.update(func(st *autoSwitchState) {
		st.Last, st.LastOK, st.LastNoTarget, st.NoTargetEpisode = rec, rec, "2026-10-28T09:00:00Z", true
	})
	if info, err := os.Stat(filepath.Dir(d.statePath)); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("state dir: %v %v", info, err)
	}
	d2 := &autoSwitchDeps{statePath: d.statePath, logPath: d.logPath}
	d2.mu.Lock()
	d2.loadStateLocked()
	st := d2.state
	d2.mu.Unlock()
	if st.Last == nil || *st.Last != *rec || st.LastOK == nil || *st.LastOK != *rec || st.LastNoTarget != "2026-10-28T09:00:00Z" || !st.NoTargetEpisode {
		t.Fatalf("round trip %+v", st)
	}
	var buf bytes.Buffer
	printAutoSwitchStatus(&buf, "c.json", "ok", autoSwitchConfig{Enabled: true, Threshold: 95, PollSeconds: 120}, st, "")
	out := buf.String()
	for _, want := range []string{"state:       enabled", "threshold:   95% 7-day", "poll:        120s",
		"last switch: a -> b (7-day 96% -> 12%, threshold 95%) at 2026-10-28T10:00:00Z, ok", "no target:   2026-10-28T09:00:00Z"} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
}

func TestAutoSwitchSetEnabled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cfg", "auto-switch.json")
	c, note, err := autoSwitchSetEnabled(path, true)
	if err != nil || note != "" || !c.Enabled || c.Threshold != 95 || c.PollSeconds != 120 {
		t.Fatalf("%+v %q %v", c, note, err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v %v", info, err)
	}
	if err := os.WriteFile(path, []byte(`{"enabled": true, "threshold": 90, "pollSeconds": 300}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, _, err = autoSwitchSetEnabled(path, false)
	if err != nil || c.Enabled || c.Threshold != 90 || c.PollSeconds != 300 {
		t.Fatalf("%+v %v", c, err)
	}
	data, _ := os.ReadFile(path)
	if got, err := parseAutoSwitchConfig(data); err != nil || got != c {
		t.Fatalf("written %s: %v", data, err)
	}
	// A broken file: on refuses, off (the kill switch) still turns it off.
	if err := os.WriteFile(path, []byte(`{"enabled": true, "bogus": 1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := autoSwitchSetEnabled(path, true); err == nil {
		t.Fatal("on must refuse a broken file")
	}
	c, note, err = autoSwitchSetEnabled(path, false)
	if err != nil || c.Enabled || note == "" {
		t.Fatalf("%+v %q %v", c, note, err)
	}
}

func TestFleetStateCarriesAutoSwitch(t *testing.T) {
	h := autoSwitchTestServer(t)
	get := func() map[string]json.RawMessage {
		r := httptest.NewRequest("GET", "/fleet/state", nil)
		r.Header.Set("Authorization", "Bearer tok")
		w := httptest.NewRecorder()
		fleetMux(h.s).ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
		var out map[string]json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if got := string(get()["autoSwitch"]); got != `{"enabled":true,"threshold":95,"last":null}` {
		t.Fatalf("autoSwitch %s", got)
	}
	*h.usage = []claudeUsageNumbers{acct("home", true, 96), acct("work", false, 10)}
	h.s.autoSwitchTick()
	got := string(get()["autoSwitch"])
	if got != `{"enabled":true,"threshold":95,"last":{"from":"home","to":"work","fromPct":96,"toPct":10,"at":"2026-10-28T10:00:00Z"}}` {
		t.Fatalf("autoSwitch %s", got)
	}
	// A later failure keeps the last success on the page.
	h.s.switchAcct = func(string, string) (string, []string, error) { return "", nil, errors.New("no") }
	*h.usage = []claudeUsageNumbers{acct("work", true, 96), acct("home", false, 10)}
	*h.now = h.now.Add(time.Hour)
	h.s.autoSwitchTick()
	if again := string(get()["autoSwitch"]); again != got {
		t.Fatalf("autoSwitch %s", again)
	}
}

func fetchedAcct(name string, active bool, seven float64, at time.Time) claudeUsageNumbers {
	e := acct(name, active, seven)
	e.FetchedAt = &at
	e.Stale = true // the disk read's stale flag is not an input to the gate
	return e
}

func TestAutoSwitchNeedsFresh(t *testing.T) {
	now := time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)
	past := now.Add(-time.Minute)
	reset := fetchedAcct("home", true, 50, now.Add(-time.Minute))
	reset.SevenDay.ResetsAt = &past
	noSeven := fetchedAcct("home", true, 50, now)
	noSeven.SevenDay = nil
	noAt := acct("home", true, 50)
	cases := []struct {
		name   string
		cached []claudeUsageNumbers
		want   bool
	}{
		{"far below, recent", []claudeUsageNumbers{fetchedAcct("home", true, 84.9, now.Add(-29*time.Minute))}, false},
		{"far below, 30 min old", []claudeUsageNumbers{fetchedAcct("home", true, 50, now.Add(-30*time.Minute))}, true},
		{"near", []claudeUsageNumbers{fetchedAcct("home", true, 85, now)}, true},
		{"no fetch time", []claudeUsageNumbers{noAt}, true},
		{"no 7-day", []claudeUsageNumbers{noSeven}, true},
		{"past its reset", []claudeUsageNumbers{reset}, true},
		{"no active", []claudeUsageNumbers{fetchedAcct("work", false, 10, now)}, true},
		{"nothing", nil, true},
		{"unsaved live", []claudeUsageNumbers{fetchedAcct("", true, 99, now.Add(-time.Hour))}, false},
	}
	for _, c := range cases {
		if got := autoSwitchNeedsFresh(c.cached, 95, now); got != c.want {
			t.Errorf("%s: got %v", c.name, got)
		}
	}
}

func TestAutoSwitchFetchesOnlyWhenTheGateSays(t *testing.T) {
	h := autoSwitchTestServer(t)
	fresh := 0
	h.s.fleet.claudeUsage = func() []claudeUsageNumbers { fresh++; return *h.usage }
	cached := []claudeUsageNumbers{fetchedAcct("home", true, 60, *h.now), fetchedAcct("work", false, 10, *h.now)}
	h.s.auto.cachedUsage = func() []claudeUsageNumbers { return cached }
	// The fresh source would trigger; the cached one is far below and recent.
	*h.usage = []claudeUsageNumbers{acct("home", true, 99), acct("work", false, 10)}
	h.s.autoSwitchTick()
	if fresh != 0 || len(*h.switches) != 0 {
		t.Fatalf("fresh %d switches %v", fresh, *h.switches)
	}
	// Near the threshold: the fresh numbers decide.
	cached[0] = fetchedAcct("home", true, 90, *h.now)
	h.s.autoSwitchTick()
	if fresh != 1 || len(*h.switches) != 1 {
		t.Fatalf("fresh %d switches %v", fresh, *h.switches)
	}
	// Near on disk, but the fresh numbers say below: nothing happens.
	*h.now = h.now.Add(time.Hour)
	*h.usage = []claudeUsageNumbers{acct("work", true, 80), acct("home", false, 10)}
	cached = []claudeUsageNumbers{fetchedAcct("work", true, 96, *h.now)}
	h.s.autoSwitchTick()
	if fresh != 2 || len(*h.switches) != 1 {
		t.Fatalf("fresh %d switches %v", fresh, *h.switches)
	}
}

func TestAutoSwitchAccountListErrorSaysNothing(t *testing.T) {
	h := autoSwitchTestServer(t)
	h.s.fleet.accountNames = func(string) ([]string, error) { return []string{"home"}, errors.New("readdir failed") }
	*h.usage = []claudeUsageNumbers{acct("home", true, 99)}
	h.s.autoSwitchTick()
	if len(*h.events) != 0 || len(*h.switches) != 0 {
		t.Fatalf("%v %v", *h.events, *h.switches)
	}
	if st := h.state(t); st.NoTargetEpisode || st.NoTargetHedged || st.LastNoTarget != "" {
		t.Fatalf("state %+v", st)
	}
}

func TestAutoSwitchNoTargetHedgedWhenReadingsAreMissing(t *testing.T) {
	h := autoSwitchTestServer(t)
	*h.names = []string{"home", "spare", "work"}
	stale := acct("spare", false, 10)
	stale.Stale = true
	// work has no entry at all, spare a stale one.
	*h.usage = []claudeUsageNumbers{acct("home", true, 99), stale}
	h.s.autoSwitchTick()
	h.s.autoSwitchTick()
	want := "[claude-sessions event] account claude at 99% 7-day, threshold 95%; no saved account with a fresh reading below it (2 without a fresh reading)"
	if len(*h.events) != 1 || (*h.events)[0].text != want {
		t.Fatalf("events %v", *h.events)
	}
	if st := h.state(t); st.NoTargetEpisode || !st.NoTargetHedged {
		t.Fatalf("state %+v", st)
	}
	// Fresh readings, all over: the definitive line still goes.
	*h.usage = []claudeUsageNumbers{acct("home", true, 99), acct("spare", false, 97), acct("work", false, 98)}
	h.s.autoSwitchTick()
	if len(*h.events) != 2 || !strings.HasSuffix((*h.events)[1].text, "no saved account below it to switch to") {
		t.Fatalf("events %v", *h.events)
	}
	// Or a fresh reading below: it switches.
	*h.usage = []claudeUsageNumbers{acct("home", true, 99), acct("spare", false, 10)}
	h.s.autoSwitchTick()
	if len(*h.switches) != 1 || (*h.switches)[0].name != "spare" {
		t.Fatalf("switches %v", *h.switches)
	}
}

func TestAutoSwitchReenableStartsNewEpisode(t *testing.T) {
	h := autoSwitchTestServer(t)
	*h.names = []string{"home"}
	*h.usage = []claudeUsageNumbers{acct("home", true, 99)}
	h.s.autoSwitchTick()
	h.s.autoSwitchTick()
	if len(*h.events) != 1 {
		t.Fatalf("events %v", *h.events)
	}
	writeAutoSwitchConfig(t, h.s, `{"enabled": false}`)
	h.s.autoSwitchTick()
	writeAutoSwitchConfig(t, h.s, `{"enabled": true}`)
	h.s.autoSwitchTick()
	if len(*h.events) != 2 {
		t.Fatalf("re-enabling must report the episode again: %v", *h.events)
	}
	// A restart while on is not a re-enable.
	h.s.auto = autoSwitchDeps{configPath: h.s.auto.configPath, statePath: h.s.auto.statePath, logPath: h.s.auto.logPath,
		now: h.s.auto.now, cachedUsage: h.s.auto.cachedUsage}
	h.s.autoSwitchTick()
	if len(*h.events) != 2 {
		t.Fatalf("a restart must not repeat the line: %v", *h.events)
	}
}

func TestAutoSwitchOffDuringTickDoesNotSwitch(t *testing.T) {
	h := autoSwitchTestServer(t)
	h.s.fleet.claudeUsage = func() []claudeUsageNumbers {
		writeAutoSwitchConfig(t, h.s, `{"enabled": false}`)
		return []claudeUsageNumbers{acct("home", true, 99), acct("work", false, 10)}
	}
	h.s.autoSwitchTick()
	if len(*h.switches) != 0 {
		t.Fatalf("switched after off: %v", *h.switches)
	}
}

func TestAutoSwitchWeeklyResetPassedNeverTriggers(t *testing.T) {
	h := autoSwitchTestServer(t)
	home := acct("home", true, 99)
	at := *h.now
	home.SevenDay.ResetsAt = &at
	*h.usage = []claudeUsageNumbers{home, acct("work", false, 10)}
	h.s.autoSwitchTick()
	if len(*h.switches) != 0 || len(*h.events) != 0 {
		t.Fatalf("%v %v", *h.switches, *h.events)
	}
	later := at.Add(time.Second)
	home.SevenDay.ResetsAt = &later
	h.s.autoSwitchTick()
	if len(*h.switches) != 1 {
		t.Fatalf("switches %v", *h.switches)
	}
}

func TestAutoSwitchScopedWeeklyOverIsNoTarget(t *testing.T) {
	scoped := acct("work", false, 10)
	scoped.WeeklyScoped = &usageWindowNumbers{Label: "Opus", Pct: 95}
	under := acct("spare", false, 40)
	under.WeeklyScoped = &usageWindowNumbers{Label: "Opus", Pct: 94}
	got, ok, unfresh := autoSwitchTarget([]claudeUsageNumbers{acct("home", true, 99), scoped, under}, "home", []string{"home", "spare", "work"}, 95)
	if !ok || got.Name != "spare" || unfresh != 0 {
		t.Fatalf("%+v %v %d", got, ok, unfresh)
	}
}

func TestAutoSwitchCorruptStateStartsCooldown(t *testing.T) {
	h := autoSwitchTestServer(t)
	if err := os.MkdirAll(filepath.Dir(h.s.auto.statePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.s.auto.statePath, []byte(`{"last": {"fro`), 0o600); err != nil {
		t.Fatal(err)
	}
	*h.usage = []claudeUsageNumbers{acct("home", true, 99), acct("work", false, 10)}
	h.s.autoSwitchTick()
	if len(*h.switches) != 0 {
		t.Fatalf("switched on a torn state file: %v", *h.switches)
	}
	*h.now = h.now.Add(31 * time.Minute)
	h.s.autoSwitchTick()
	if len(*h.switches) != 1 {
		t.Fatalf("switches %v", *h.switches)
	}
}

func TestAutoSwitchManualPageSwitchStartsCooldown(t *testing.T) {
	h := autoSwitchTestServer(t)
	w, out := postFleetSwitch(h.s, `{"provider":"claude","name":"work"}`, nil)
	if w.Code != http.StatusOK || !out.OK {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if st := h.state(t); st.LastManualAt != "2026-10-28T10:00:00Z" {
		t.Fatalf("state %+v", st)
	}
	// Andy switched to work on purpose; it is over the threshold.
	*h.usage = []claudeUsageNumbers{acct("work", true, 99), acct("home", false, 10)}
	*h.now = h.now.Add(29 * time.Minute)
	h.s.autoSwitchTick()
	if len(*h.switches) != 1 {
		t.Fatalf("undid a manual switch inside the cooldown: %v", *h.switches)
	}
	*h.now = h.now.Add(2 * time.Minute)
	h.s.autoSwitchTick()
	if len(*h.switches) != 2 || (*h.switches)[1].name != "home" {
		t.Fatalf("switches %v", *h.switches)
	}
	// A grok switch does not touch the claude cooldown.
	h2 := autoSwitchTestServer(t)
	if w, _ := postFleetSwitch(h2.s, `{"provider":"grok","name":"gy"}`, nil); w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if st := h2.state(t); st.LastManualAt != "" {
		t.Fatalf("state %+v", st)
	}
}
