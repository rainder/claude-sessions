package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fleetSwitchCall struct{ provider, name string }
type fleetEventCall struct{ target, text string }

// fleetAccountServer is fleetTestServer plus account seams: fixed names and
// current accounts, recorded switches and event deliveries, and an idle
// config path in a temp dir (missing unless a test writes it).
func fleetAccountServer(t *testing.T, sessions []Session) (*server, *[]fleetSwitchCall, *[]fleetEventCall, map[string]string) {
	t.Helper()
	s := fleetTestServer(t, sessions)
	s.idle.configPath = filepath.Join(t.TempDir(), "idle-events.json")
	names := map[string][]string{"claude": {"work", "home"}, "grok": {"gx", "gy"}}
	current := map[string]string{"claude": "home", "grok": "gx"}
	s.fleet.accountNames = func(p string) ([]string, error) { return names[p], nil }
	s.fleet.accountCurrent = func(p string) string { return current[p] }
	s.fleet.accountLive = func(p string) string { return p + "-live@example.com" }
	s.fleet.claudeUsage = func() []claudeUsageNumbers { return nil }
	s.fleet.grokLiveUsage = func() []providerUsageNumbers { return nil }
	var switches []fleetSwitchCall
	s.switchAcct = func(tool, name string) (string, []string, error) {
		switches = append(switches, fleetSwitchCall{tool, name})
		current[tool] = name
		return name + "@example.com", []string{"a session still runs as old@example.com"}, nil
	}
	var events []fleetEventCall
	s.fleet.deliverEvent = func(target, text string) string {
		events = append(events, fleetEventCall{target, text})
		return "sent"
	}
	return s, &switches, &events, current
}

func postFleetSwitch(s *server, body string, setup func(*http.Request)) (*httptest.ResponseRecorder, fleetSwitchResult) {
	r := httptest.NewRequest("POST", "/fleet/account/switch", strings.NewReader(body))
	if setup == nil {
		r.Header.Set("Authorization", "Bearer tok")
	} else {
		setup(r)
	}
	w := httptest.NewRecorder()
	fleetMux(s).ServeHTTP(w, r)
	var out fleetSwitchResult
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w, out
}

func TestFleetAccountSwitchRefusals(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		setup  func(*http.Request)
		status int
		code   string
	}{
		{"no auth", `{"provider":"claude","name":"work"}`, func(*http.Request) {}, http.StatusUnauthorized, "unauthorized"},
		{"cookie without origin", `{"provider":"claude","name":"work"}`, func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: "cs_fleet", Value: "sec"})
		}, http.StatusForbidden, "origin"},
		{"not json", `nope`, nil, http.StatusBadRequest, "bad_request"},
		{"empty body", ``, nil, http.StatusBadRequest, "bad_request"},
		{"null", `null`, nil, http.StatusBadRequest, "bad_request"},
		{"unknown field", `{"provider":"claude","name":"work","tool":"claude"}`, nil, http.StatusBadRequest, "bad_request"},
		{"trailing", `{"provider":"claude","name":"work"} x`, nil, http.StatusBadRequest, "bad_request"},
		{"two values", `{"provider":"claude","name":"work"}{}`, nil, http.StatusBadRequest, "bad_request"},
		{"bad provider", `{"provider":"codex","name":"work"}`, nil, http.StatusBadRequest, "bad_provider"},
		{"unknown name", `{"provider":"claude","name":"nobody"}`, nil, http.StatusBadRequest, "unknown_account"},
		{"other provider's name", `{"provider":"claude","name":"gy"}`, nil, http.StatusBadRequest, "unknown_account"},
		{"case differs", `{"provider":"claude","name":"WORK"}`, nil, http.StatusBadRequest, "unknown_account"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, switches, events, _ := fleetAccountServer(t, nil)
			w, out := postFleetSwitch(s, c.body, c.setup)
			if w.Code != c.status || out.Code != c.code || out.OK {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("must be no-store")
			}
			if len(*switches) != 0 || len(*events) != 0 {
				t.Fatalf("must not switch or notify: %v %v", *switches, *events)
			}
		})
	}
}

func TestFleetAccountSwitchCookieWithOrigin(t *testing.T) {
	s, switches, _, _ := fleetAccountServer(t, nil)
	w, out := postFleetSwitch(s, `{"provider":"claude","name":"work"}`, func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: "cs_fleet", Value: "sec"})
		r.Header.Set("Origin", "http://"+r.Host)
	})
	if w.Code != http.StatusOK || !out.OK || len(*switches) != 1 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestFleetAccountSwitchNoopWhenActive(t *testing.T) {
	s, switches, events, _ := fleetAccountServer(t, nil)
	w, out := postFleetSwitch(s, `{"provider":"claude","name":"home"}`, nil)
	if w.Code != http.StatusOK || !out.OK || !out.Noop {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if len(*switches) != 0 || len(*events) != 0 {
		t.Fatal("a no-op must not switch or notify")
	}
	if _, err := os.Stat(filepath.Join(s.fleet.dir, "log.jsonl")); !os.IsNotExist(err) {
		t.Fatal("a no-op must not log")
	}
}

func readFleetLogLines(t *testing.T, s *server) []fleetLogEntry {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(s.fleet.dir, "log.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var out []fleetLogEntry
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var e fleetLogEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

func TestFleetAccountSwitchSuccessUsesIdleTarget(t *testing.T) {
	// A FLEET row exists too: the config's target wins, even disabled.
	s, switches, events, _ := fleetAccountServer(t, []Session{{PID: 1, Name: "FLEET", Tmux: "FLEET-row:0.0"}})
	if err := os.WriteFile(s.idle.configPath, []byte(`{"enabled": false, "target": "FLEET-cfg"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	w, out := postFleetSwitch(s, `{"provider":"claude","name":"work"}`, nil)
	if w.Code != http.StatusOK || !out.OK || out.Noop {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if out.Provider != "claude" || out.Name != "work" || out.Previous != "home" || out.Event != "sent" || out.Warnings != 1 {
		t.Fatalf("%+v", out)
	}
	if strings.Contains(w.Body.String(), "@") {
		t.Fatalf("response carries an email: %s", w.Body.String())
	}
	if len(*switches) != 1 || (*switches)[0] != (fleetSwitchCall{"claude", "work"}) {
		t.Fatalf("switches %v", *switches)
	}
	if len(*events) != 1 || (*events)[0].target != "FLEET-cfg" {
		t.Fatalf("events %v", *events)
	}
	text := (*events)[0].text
	if !strings.HasPrefix(text, "[claude-sessions event] account claude switched from home to work; ") ||
		!strings.HasSuffix(text, fleetSwitchEventNote("claude")) {
		t.Fatalf("event text %q", text)
	}
	lines := readFleetLogLines(t, s)
	if len(lines) != 1 || lines[0].Type != "web-switch" || lines[0].Text != "claude account home -> work" {
		t.Fatalf("log %+v", lines)
	}
}

func TestFleetAccountSwitchFallsBackToFleetRow(t *testing.T) {
	s, _, events, current := fleetAccountServer(t, []Session{{PID: 1, Name: "FLEET", Tmux: "FLEET-row:0.0"}})
	current["grok"] = "" // live account is unsaved: previous is "-"
	w, out := postFleetSwitch(s, `{"provider":"grok","name":"gy"}`, nil)
	if w.Code != http.StatusOK || out.Event != "sent" || out.Previous != "" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	// No configured target: "" is FLEET, resolved by idleDeliver at send.
	if len(*events) != 1 || (*events)[0].target != "" {
		t.Fatalf("events %v", *events)
	}
	if !strings.HasPrefix((*events)[0].text, "[claude-sessions event] account grok switched from - to gy; ") {
		t.Fatalf("event text %q", (*events)[0].text)
	}
	if lines := readFleetLogLines(t, s); len(lines) != 1 || lines[0].Text != "grok account - -> gy" {
		t.Fatalf("log %+v", lines)
	}
}

func TestFleetAccountSwitchNoTarget(t *testing.T) {
	s, switches, _, _ := fleetAccountServer(t, nil)
	s.fleet.deliverEvent = nil // the real idleDeliver: no FLEET row, nothing typed
	typed := 0
	s.fleet.typeText = func(string, string) error { typed++; return nil }
	w, out := postFleetSwitch(s, `{"provider":"claude","name":"work"}`, nil)
	if w.Code != http.StatusOK || !out.OK || out.Event != "target_missing" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if len(*switches) != 1 || typed != 0 {
		t.Fatal("switch must still happen; nothing typed without a target")
	}
}

func TestFleetAccountSwitchErrorHidesDetail(t *testing.T) {
	s, _, events, _ := fleetAccountServer(t, nil)
	s.switchAcct = func(string, string) (string, []string, error) {
		return "", nil, errors.New("snapshot says other@example.com, live is me@example.com")
	}
	w, out := postFleetSwitch(s, `{"provider":"claude","name":"work"}`, nil)
	if w.Code != http.StatusInternalServerError || out.Code != "switch_failed" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "@") {
		t.Fatalf("error body carries an email: %s", w.Body.String())
	}
	if len(*events) != 0 {
		t.Fatal("a failed switch must not notify")
	}
	if _, err := os.Stat(filepath.Join(s.fleet.dir, "log.jsonl")); !os.IsNotExist(err) {
		t.Fatal("a failed switch must not log")
	}

	s.switchAcct = func(string, string) (string, []string, error) {
		return "", nil, errUnknownAccount
	}
	if w, out := postFleetSwitch(s, `{"provider":"claude","name":"work"}`, nil); w.Code != http.StatusBadRequest || out.Code != "unknown_account" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestFleetAccountSwitchBusy(t *testing.T) {
	s, switches, _, _ := fleetAccountServer(t, nil)
	s.fleet.switchMu.Lock()
	w, out := postFleetSwitch(s, `{"provider":"claude","name":"work"}`, nil)
	s.fleet.switchMu.Unlock()
	if w.Code != http.StatusConflict || out.Code != "busy" || len(*switches) != 0 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	// Released after a success, so the next one is not busy.
	if w, _ := postFleetSwitch(s, `{"provider":"claude","name":"work"}`, nil); w.Code != http.StatusOK {
		t.Fatalf("%d", w.Code)
	}
	if w, out := postFleetSwitch(s, `{"provider":"grok","name":"gy"}`, nil); w.Code != http.StatusOK || out.Noop {
		t.Fatalf("%d %+v", w.Code, out)
	}
}

func TestFleetAccountSwitchReleasesLockBeforeDelivery(t *testing.T) {
	s, _, _, _ := fleetAccountServer(t, []Session{{PID: 1, Name: "FLEET", Tmux: "F:0.0"}})
	s.fleet.deliverEvent = func(string, string) string {
		if !s.fleet.switchMu.TryLock() {
			t.Error("switch lock held during delivery")
			return "sent"
		}
		s.fleet.switchMu.Unlock()
		return "sent"
	}
	if w, _ := postFleetSwitch(s, `{"provider":"claude","name":"work"}`, nil); w.Code != http.StatusOK {
		t.Fatalf("%d", w.Code)
	}
}

func TestFleetSwitchEventTextSanitizesNames(t *testing.T) {
	got := fleetSwitchEventText("claude", "", `a"b [claude-sessions event] c`)
	if strings.Count(got, "claude-sessions event") != 1 || strings.Contains(got, `"`) {
		t.Fatalf("%q", got)
	}
	if !strings.Contains(got, "switched from - to ") {
		t.Fatalf("%q", got)
	}
}

func getFleetAccounts(t *testing.T, s *server) (fleetAccountsResponse, string) {
	t.Helper()
	r := httptest.NewRequest("GET", "/fleet/accounts", nil)
	r.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	fleetMux(s).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("must be no-store")
	}
	var out fleetAccountsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out, w.Body.String()
}

func TestFleetAccountsUnauthorized(t *testing.T) {
	s, _, _, _ := fleetAccountServer(t, nil)
	w := httptest.NewRecorder()
	fleetMux(s).ServeHTTP(w, httptest.NewRequest("GET", "/fleet/accounts", nil))
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), `"unauthorized"`) {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestFleetAccountsListing(t *testing.T) {
	s, _, _, _ := fleetAccountServer(t, nil)
	now := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	s.fleet.now = func() time.Time { return now }
	reset := now.Add(3 * time.Hour)
	s.fleet.accountNames = func(p string) ([]string, error) {
		if p == "claude" {
			// Unsorted, plus a file name this tool would never save.
			return []string{"work", "home", "me@example.com"}, nil
		}
		return []string{"gy", "gx", "gz"}, nil
	}
	s.fleet.claudeUsage = func() []claudeUsageNumbers {
		return []claudeUsageNumbers{
			{Name: "home", Account: "home@example.com", Active: true,
				FiveHour:     &usageWindowNumbers{Pct: 12, ResetsAt: &reset},
				SevenDay:     &usageWindowNumbers{Pct: 34},
				WeeklyScoped: &usageWindowNumbers{Label: "Fable", Pct: 91},
				Credits:      &usageCreditsNumbers{Enabled: true, Used: 1, Limit: 2}},
			{Name: "work", Account: "work@example.com", SevenDay: &usageWindowNumbers{Pct: 50}, Stale: true},
		}
	}
	s.fleet.accountLive = func(p string) string { return p + "-live@example.com" }
	s.fleet.grokLiveUsage = func() []providerUsageNumbers {
		return []providerUsageNumbers{{Account: "grok-live@example.com",
			Windows: []usageWindowNumbers{{Label: "wk", Pct: 19}}}}
	}
	calls := 0
	s.fleet.grokKnown = func() []KnownGrokAccountUsage {
		calls++
		return []KnownGrokAccountUsage{
			{Name: "gy", Account: "gy@example.com", FetchedAt: now.Add(-time.Minute),
				Info: &GrokUsageInfo{Windows: []grokWindow{{Label: "wk", Pct: 70}}}},
			{Name: "gz", Account: "gz@example.com", FetchedAt: now.Add(-time.Hour),
				Info: &GrokUsageInfo{Windows: []grokWindow{{Label: "mo", Pct: 5}}}},
		}
	}

	out, body := getFleetAccounts(t, s)
	if strings.Contains(body, "@") {
		t.Fatalf("body carries an email: %s", body)
	}
	if strings.Contains(body, "credits") || strings.Contains(body, "account\"") {
		t.Fatalf("body carries more than windows: %s", body)
	}
	c := out.Claude
	if c.Hint != fleetSwitchHintClaude || c.ActiveUnsaved || len(c.Accounts) != 2 {
		t.Fatalf("claude %+v", c)
	}
	home, work := c.Accounts[0], c.Accounts[1]
	if home.Name != "home" || !home.Active || home.FiveHour == nil || home.FiveHour.Pct != 12 || home.FiveHour.ResetsAt == nil ||
		home.SevenDay == nil || home.WeeklyScoped == nil || home.WeeklyScoped.Label != "Fable" || home.Stale {
		t.Fatalf("home %+v", home)
	}
	if work.Name != "work" || work.Active || work.FiveHour != nil || work.SevenDay == nil || work.SevenDay.Pct != 50 || !work.Stale {
		t.Fatalf("work %+v", work)
	}
	g := out.Grok
	if g.Hint != fleetSwitchHintGrok || len(g.Accounts) != 3 {
		t.Fatalf("grok %+v", g)
	}
	gx, gy, gz := g.Accounts[0], g.Accounts[1], g.Accounts[2]
	if gx.Name != "gx" || !gx.Active || len(gx.Windows) != 1 || gx.Windows[0].Pct != 19 {
		t.Fatalf("gx %+v", gx)
	}
	if gy.Name != "gy" || gy.Active || len(gy.Windows) != 1 || gy.Windows[0].Pct != 70 || gy.Stale {
		t.Fatalf("gy %+v", gy)
	}
	if gz.Name != "gz" || len(gz.Windows) != 1 || !gz.Stale {
		t.Fatalf("gz %+v", gz)
	}

	// The parked fetch is cached for one interval and dropped by a grok switch.
	getFleetAccounts(t, s)
	if calls != 1 {
		t.Fatalf("parked grok fetched %d times, want 1", calls)
	}
	if w, _ := postFleetSwitch(s, `{"provider":"grok","name":"gy"}`, nil); w.Code != http.StatusOK {
		t.Fatalf("%d", w.Code)
	}
	getFleetAccounts(t, s)
	if calls != 2 {
		t.Fatalf("parked grok fetched %d times after a switch, want 2", calls)
	}
	now = now.Add(usageRefreshInterval)
	getFleetAccounts(t, s)
	if calls != 3 {
		t.Fatalf("parked grok fetched %d times after the interval, want 3", calls)
	}
}

func TestFleetAccountsActiveUnsavedAndStaleLiveCache(t *testing.T) {
	s, _, _, current := fleetAccountServer(t, nil)
	current["claude"] = ""
	// The grok hub's cache still names the account live before a switch.
	s.fleet.grokLiveUsage = func() []providerUsageNumbers {
		return []providerUsageNumbers{{Account: "old@example.com", Windows: []usageWindowNumbers{{Label: "wk", Pct: 99}}}}
	}
	out, _ := getFleetAccounts(t, s)
	if !out.Claude.ActiveUnsaved {
		t.Fatal("claude live account with no snapshot must be activeUnsaved")
	}
	for _, a := range out.Claude.Accounts {
		if a.Active {
			t.Fatalf("no claude row may be active: %+v", a)
		}
	}
	if out.Grok.ActiveUnsaved || !out.Grok.Accounts[0].Active || out.Grok.Accounts[0].Windows != nil {
		t.Fatalf("grok %+v", out.Grok)
	}

	// No saved accounts: an empty list, never null.
	s.fleet.accountNames = func(string) ([]string, error) { return nil, nil }
	_, body := getFleetAccounts(t, s)
	if !strings.Contains(body, `"accounts":[]`) {
		t.Fatalf("%s", body)
	}
}

func TestFleetParkedGrokSwitchDuringFetchIsNotKept(t *testing.T) {
	s, _, _, _ := fleetAccountServer(t, nil)
	var once sync.Once
	calls := 0
	s.fleet.grokKnown = func() []KnownGrokAccountUsage {
		calls++
		once.Do(s.fleetInvalidateParkedGrok) // a switch lands mid-fetch
		return nil
	}
	s.fleetParkedGrok()
	s.fleetParkedGrok()
	if calls != 2 {
		t.Fatalf("fetched %d times, want 2", calls)
	}
}

// A live account whose snapshot name is email-shaped (not saved by this tool)
// must not leak through Previous, the log line, or the active marker.
func TestFleetAccountSwitchEmailShapedCurrentNeverLeaks(t *testing.T) {
	s, switches, _, current := fleetAccountServer(t, nil)
	current["claude"] = "me@x.com"
	w, out := postFleetSwitch(s, `{"provider":"claude","name":"work"}`, nil)
	if w.Code != http.StatusOK || !out.OK || len(*switches) != 1 {
		t.Fatalf("status %d body %s switches %v", w.Code, w.Body, *switches)
	}
	if strings.Contains(w.Body.String(), "@") {
		t.Fatalf("response leaks an email: %s", w.Body)
	}
	log, err := os.ReadFile(filepath.Join(s.fleet.dir, "log.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(log), "@") || !strings.Contains(string(log), `claude account - -\u003e work`) {
		t.Fatalf("log line: %s", log)
	}
}
