package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fleetTestServer(t *testing.T, sessions []Session) *server {
	t.Helper()
	return &server{
		token:   "tok",
		collect: func() ([]Session, error) { return sessions, nil },
		fleet: fleetDeps{dir: t.TempDir(), secret: "sec",
			sayLogPath: filepath.Join(t.TempDir(), "state", "fleet-say.log"),
			// Never the real ~/.config or ~/.claude: discovery writes
			// fleet-last.json and chat/start look up transcripts.
			lastPath: filepath.Join(t.TempDir(), "cfg", "fleet-last.json"),
			home:     t.TempDir(),
			// No test may shell out to tmux: every started session is gone.
			paneCommand: func(string) (string, bool) { return "", false },
			paneHistory: func(string) (string, error) { return "", errors.New("test: no tmux") }},
	}
}

func fleetMux(s *server) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /sessions", s.sessions)
	registerFleetRoutes(mux, s)
	return mux
}

func TestFleetAuthBearerAndCookie(t *testing.T) {
	s := fleetTestServer(t, nil)
	r := httptest.NewRequest("GET", "/fleet/state", nil)
	if s.fleetAuth(r) != fleetAuthNone {
		t.Fatal("no credentials must be none")
	}
	r.Header.Set("Authorization", "Bearer tok")
	if s.fleetAuth(r) != fleetAuthBearer {
		t.Fatal("bearer header")
	}
	r = httptest.NewRequest("GET", "/fleet/state", nil)
	r.AddCookie(&http.Cookie{Name: "cs_fleet", Value: "sec"})
	if s.fleetAuth(r) != fleetAuthCookie {
		t.Fatal("cookie")
	}
	r = httptest.NewRequest("GET", "/fleet/state", nil)
	r.AddCookie(&http.Cookie{Name: "cs_fleet", Value: "wrong"})
	if s.fleetAuth(r) != fleetAuthNone {
		t.Fatal("wrong cookie must be none")
	}
	// The server token is no longer a cookie value: an old cs_fleet=<token>
	// cookie must stop working.
	r = httptest.NewRequest("GET", "/fleet/state", nil)
	r.AddCookie(&http.Cookie{Name: "cs_fleet", Value: "tok"})
	if s.fleetAuth(r) != fleetAuthNone {
		t.Fatal("cookie holding the server token must be none")
	}
	// No secret loaded: cookie auth is off, even for an empty cookie.
	s.fleet.secret = ""
	r = httptest.NewRequest("GET", "/fleet/state", nil)
	r.AddCookie(&http.Cookie{Name: "cs_fleet", Value: ""})
	if s.fleetAuth(r) != fleetAuthNone {
		t.Fatal("empty secret must disable cookie auth")
	}
}

func TestFleetCookieRejectedOnSessions(t *testing.T) {
	s := fleetTestServer(t, nil)
	r := httptest.NewRequest("GET", "/sessions", nil)
	r.AddCookie(&http.Cookie{Name: "cs_fleet", Value: "sec"})
	w := httptest.NewRecorder()
	fleetMux(s).ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("GET /sessions with only the fleet cookie: %d, want 401", w.Code)
	}
}

func TestFleetPageNoAuthHopsOnce(t *testing.T) {
	s := fleetTestServer(t, nil)
	w := httptest.NewRecorder()
	fleetMux(s).ServeHTTP(w, httptest.NewRequest("GET", "/fleet", nil))
	if w.Code != http.StatusOK || len(w.Result().Cookies()) != 0 {
		t.Fatalf("no auth, no hop: %d cookies %v", w.Code, w.Result().Cookies())
	}
	if body := w.Body.String(); !strings.Contains(body, `location.replace("/fleet?hop=1")`) ||
		!strings.Contains(body, `url=/fleet?hop=1`) {
		t.Fatalf("hop page = %q", body)
	}
	h := w.Header()
	if h.Get("Referrer-Policy") != "no-referrer" || h.Get("Cache-Control") != "no-store" ||
		!strings.Contains(h.Get("Content-Type"), "text/html") {
		t.Fatalf("hop headers = %v", h)
	}
	w = httptest.NewRecorder()
	fleetMux(s).ServeHTTP(w, httptest.NewRequest("GET", "/fleet?hop=1", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("no auth after the hop: %d, want 401", w.Code)
	}
	r := httptest.NewRequest("GET", "/fleet?hop=1", nil)
	r.AddCookie(&http.Cookie{Name: "cs_fleet", Value: "sec"})
	w = httptest.NewRecorder()
	fleetMux(s).ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "/fleet/state") {
		t.Fatalf("cookie after the hop: %d", w.Code)
	}
}

func TestFleetPageNeedsAuthAndSetsHeaders(t *testing.T) {
	s := fleetTestServer(t, nil)
	w := httptest.NewRecorder()
	fleetMux(s).ServeHTTP(w, httptest.NewRequest("GET", "/fleet?hop=1", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("no auth: %d", w.Code)
	}
	r := httptest.NewRequest("GET", "/fleet", nil)
	r.AddCookie(&http.Cookie{Name: "cs_fleet", Value: "sec"})
	w = httptest.NewRecorder()
	fleetMux(s).ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("page: %d %q", w.Code, w.Header().Get("Content-Type"))
	}
	if got := w.Header().Get("Referrer-Policy"); got != "same-origin" {
		t.Fatalf("Referrer-Policy = %q, want same-origin (no-referrer nulls Origin on POST)", got)
	}
	if !strings.Contains(w.Header().Get("Content-Security-Policy"), "default-src 'none'") {
		t.Fatalf("CSP = %q", w.Header().Get("Content-Security-Policy"))
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("page must be no-store")
	}
}

func TestFindFleet(t *testing.T) {
	one := []Session{{PID: 1, Name: "FLEET", SessionID: "a"}, {PID: 2, Name: "DR-5042 FLEET Ticket Pickup"}}
	if got, reason := findFleet(one); reason != "" || got.PID != 1 {
		t.Fatalf("one: %+v %q", got, reason)
	}
	if _, reason := findFleet(nil); reason != "not running" {
		t.Fatalf("zero: %q", reason)
	}
	two := []Session{{PID: 1, Name: "FLEET"}, {PID: 3, Name: "FLEET"}}
	if _, reason := findFleet(two); reason != "ambiguous" {
		t.Fatalf("two: %q", reason)
	}
	// A derived name, claude's or grok's, is not the user's FLEET, and does
	// not make the real one ambiguous.
	others := []Session{
		{PID: 4, Name: "FLEET", NameSource: "derived"},
		{PID: 5, Name: "FLEET", Tool: toolGrok, NameSource: "derived"},
		{PID: 1, Name: "FLEET", NameSource: "user"},
	}
	if got, reason := findFleet(others); reason != "" || got.PID != 1 {
		t.Fatalf("others: %+v %q", got, reason)
	}
	if _, reason := findFleet(others[:2]); reason != "not running" {
		t.Fatalf("only others: %q", reason)
	}
}

// FLEET may be a grok session renamed to FLEET (/rename sets a user-set
// name). One claude FLEET and one grok FLEET is ambiguous: nothing says which
// the user meant, so both refuse.
func TestFindFleetEitherTool(t *testing.T) {
	claude := Session{PID: 1, Name: "FLEET", NameSource: "user", SessionID: "c"}
	grok := Session{PID: 2, Name: "FLEET", NameSource: "user", Tool: toolGrok, SessionID: "g"}
	if got, reason := findFleet([]Session{claude}); reason != "" || got.PID != 1 {
		t.Fatalf("claude: %+v %q", got, reason)
	}
	if got, reason := findFleet([]Session{grok}); reason != "" || got.PID != 2 || got.Tool != toolGrok {
		t.Fatalf("grok: %+v %q", got, reason)
	}
	if _, reason := findFleet([]Session{claude, grok}); reason != "ambiguous" {
		t.Fatalf("claude+grok: %q, want ambiguous", reason)
	}
	derived := grok
	derived.NameSource = "derived"
	if _, reason := findFleet([]Session{derived}); reason != "not running" {
		t.Fatalf("derived grok: %q, want not running", reason)
	}
	// Another tool's row is never FLEET.
	other := Session{PID: 3, Name: "FLEET", NameSource: "user", Tool: "codex"}
	if _, reason := findFleet([]Session{other}); reason != "not running" {
		t.Fatalf("other tool: %q", reason)
	}
}

func TestFleetStatus(t *testing.T) {
	cases := map[string]Session{
		"waiting": {Status: "busy", WaitingFor: "permission prompt"},
		"busy":    {Status: "busy"},
		"idle":    {Status: "shell"},
	}
	for want, sess := range cases {
		if got := fleetStatus(sess); got != want {
			t.Errorf("%+v: %q want %q", sess, got, want)
		}
	}
	if got := fleetStatus(Session{Status: "idle"}); got != "idle" {
		t.Errorf("idle: %q", got)
	}
}

func TestFleetUsageAliasUsesFleetAuth(t *testing.T) {
	s := fleetTestServer(t, nil) // s.numbers nil → 503 once authed
	w := httptest.NewRecorder()
	fleetMux(s).ServeHTTP(w, httptest.NewRequest("GET", "/fleet/usage", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("no auth: %d", w.Code)
	}
	r := httptest.NewRequest("GET", "/fleet/usage", nil)
	r.AddCookie(&http.Cookie{Name: "cs_fleet", Value: "sec"})
	w = httptest.NewRecorder()
	fleetMux(s).ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("cookie auth reaches the usage body: %d, want 503 with nil source", w.Code)
	}
}

func writeFleetFile(t *testing.T, s *server, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(s.fleet.dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func getFleetJSON(t *testing.T, s *server, path string) map[string]any {
	t.Helper()
	r := httptest.NewRequest("GET", path, nil)
	r.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	fleetMux(s).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestFleetStateValidFreshAndAlive(t *testing.T) {
	now := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	s := fleetTestServer(t, []Session{{PID: 7, Name: "FLEET", SessionID: "sid", Status: "busy"}})
	s.fleet.now = func() time.Time { return now }
	writeFleetFile(t, s, "state.json", `{"version":1,"writtenAt":"2026-10-13T09:40:00Z","cards":[{"id":"a"}],"fleet":"spoof"}`)
	out := getFleetJSON(t, s, "/fleet/state")
	if out["stale"] != false || out["stateError"] != nil {
		t.Fatalf("stale/stateError: %v %v", out["stale"], out["stateError"])
	}
	fl := out["fleet"].(map[string]any)
	if fl["alive"] != true || fl["sessionId"] != "sid" || fl["status"] != "busy" {
		t.Fatalf("fleet block = %v (server keys must overwrite file keys)", fl)
	}
	if len(out["cards"].([]any)) != 1 {
		t.Fatal("cards pass through")
	}
}

func TestFleetStateStaleAfter40Minutes(t *testing.T) {
	s := fleetTestServer(t, nil)
	s.fleet.now = func() time.Time { return time.Date(2026, 10, 13, 10, 20, 1, 0, time.UTC) }
	writeFleetFile(t, s, "state.json", `{"version":1,"writtenAt":"2026-10-13T09:40:00Z","cards":[]}`)
	if out := getFleetJSON(t, s, "/fleet/state"); out["stale"] != true {
		t.Fatal("40m01s old must be stale")
	}
}

func TestFleetStateMissingWrittenAtIsStale(t *testing.T) {
	s := fleetTestServer(t, nil)
	writeFleetFile(t, s, "state.json", `{"version":1,"cards":[]}`)
	if out := getFleetJSON(t, s, "/fleet/state"); out["stale"] != true {
		t.Fatal("no writtenAt must be stale")
	}
}

func TestFleetStateMissingFile(t *testing.T) {
	s := fleetTestServer(t, nil)
	out := getFleetJSON(t, s, "/fleet/state")
	if out["stateError"] != "FLEET has not written state yet" || len(out["cards"].([]any)) != 0 {
		t.Fatalf("missing: %v", out)
	}
	if fl := out["fleet"].(map[string]any); fl["alive"] != false || fl["reason"] != "not running" {
		t.Fatalf("fleet = %v", fl)
	}
}

func TestFleetStateInvalidKeepsLastGood(t *testing.T) {
	s := fleetTestServer(t, nil)
	writeFleetFile(t, s, "state.json", `{"version":1,"writtenAt":"2026-10-13T09:40:00Z","cards":[{"id":"good"}]}`)
	getFleetJSON(t, s, "/fleet/state")
	// Different size so the (mtime,size) cache key changes even on a coarse clock.
	writeFleetFile(t, s, "state.json", `{"version":1,"cards":[{"id":"torn"`)
	out := getFleetJSON(t, s, "/fleet/state")
	cards := out["cards"].([]any)
	if len(cards) != 1 || cards[0].(map[string]any)["id"] != "good" {
		t.Fatalf("want last good copy, got %v", cards)
	}
	if e, _ := out["stateError"].(string); !strings.HasPrefix(e, "state.json is invalid") {
		t.Fatalf("stateError = %v", out["stateError"])
	}
}

func TestFleetStateInvalidWithNoGoodCopy(t *testing.T) {
	s := fleetTestServer(t, nil)
	writeFleetFile(t, s, "state.json", `[1,2]`)
	out := getFleetJSON(t, s, "/fleet/state")
	if len(out["cards"].([]any)) != 0 || out["stateError"] == nil {
		t.Fatalf("got %v", out)
	}
}

func TestFleetStateCardsNotArrayIsInvalid(t *testing.T) {
	s := fleetTestServer(t, nil)
	writeFleetFile(t, s, "state.json", `{"version":1,"cards":{}}`)
	if out := getFleetJSON(t, s, "/fleet/state"); out["stateError"] == nil {
		t.Fatal("cards must be an array")
	}
}

func TestFleetStateAmbiguous(t *testing.T) {
	s := fleetTestServer(t, []Session{{PID: 1, Name: "FLEET"}, {PID: 2, Name: "FLEET"}})
	out := getFleetJSON(t, s, "/fleet/state")
	if fl := out["fleet"].(map[string]any); fl["alive"] != false || fl["reason"] != "ambiguous" {
		t.Fatalf("fleet = %v", fl)
	}
}

func fleetLogLines(from, to int) string {
	var b strings.Builder
	for i := from; i < to; i++ {
		fmt.Fprintf(&b, `{"at":"2026-10-13T09:00:%02dZ","type":"merge","text":"m%d"}`+"\n", i, i)
	}
	return b.String()
}

func logTexts(out map[string]any) []string {
	var got []string
	for _, e := range out["entries"].([]any) {
		got = append(got, e.(map[string]any)["text"].(string))
	}
	return got
}

func TestFleetLogPagesNewestFirst(t *testing.T) {
	s := fleetTestServer(t, nil)
	body := fleetLogLines(0, 2) + "not json\n" + `{"text":"no at"}` + "\n" + fleetLogLines(2, 5) + `{"at":"partial`
	writeFleetFile(t, s, "log.jsonl", body)

	out := getFleetJSON(t, s, "/fleet/log?n=2")
	if got := logTexts(out); strings.Join(got, ",") != "m4,m3" {
		t.Fatalf("page 1 = %v", got)
	}
	next := int64(out["next"].(float64))
	out = getFleetJSON(t, s, fmt.Sprintf("/fleet/log?n=2&before=%d", next))
	if got := logTexts(out); strings.Join(got, ",") != "m2,m1" {
		t.Fatalf("page 2 = %v", got)
	}
	next = int64(out["next"].(float64))
	out = getFleetJSON(t, s, fmt.Sprintf("/fleet/log?n=2&before=%d", next))
	if got := logTexts(out); strings.Join(got, ",") != "m0" || out["next"] != nil {
		t.Fatalf("page 3 = %v next %v", got, out["next"])
	}
}

func TestFleetLogCursorSurvivesAppends(t *testing.T) {
	s := fleetTestServer(t, nil)
	writeFleetFile(t, s, "log.jsonl", fleetLogLines(0, 5))
	out := getFleetJSON(t, s, "/fleet/log?n=2")
	next := int64(out["next"].(float64))
	f, err := os.OpenFile(filepath.Join(s.fleet.dir, "log.jsonl"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(fleetLogLines(5, 40))
	f.Close()
	out = getFleetJSON(t, s, fmt.Sprintf("/fleet/log?n=2&before=%d", next))
	if got := logTexts(out); strings.Join(got, ",") != "m2,m1" {
		t.Fatalf("after appends page 2 = %v", got)
	}
}

func TestFleetLogSpansChunks(t *testing.T) {
	s := fleetTestServer(t, nil)
	pad := strings.Repeat("x", 300*1024) // one line bigger than a chunk
	body := fleetLogLines(0, 1) + `{"at":"2026-10-13T09:00:01Z","type":"note","text":"` + pad + `"}` + "\n" + fleetLogLines(2, 3)
	writeFleetFile(t, s, "log.jsonl", body)
	out := getFleetJSON(t, s, "/fleet/log?n=3")
	if got := out["entries"].([]any); len(got) != 3 {
		t.Fatalf("want 3 entries across chunks, got %d", len(got))
	}
}

func TestFleetLogBeforePastEndAfterTruncate(t *testing.T) {
	s := fleetTestServer(t, nil)
	body := fleetLogLines(0, 3)
	writeFleetFile(t, s, "log.jsonl", body)
	out := getFleetJSON(t, s, fmt.Sprintf("/fleet/log?n=2&before=%d", len(body)+100))
	if len(out["entries"].([]any)) != 0 || out["next"] != nil {
		t.Fatalf("cursor past a truncated log must return nothing: %v", out)
	}
}

func TestFleetLogMissingFile(t *testing.T) {
	s := fleetTestServer(t, nil)
	out := getFleetJSON(t, s, "/fleet/log")
	if len(out["entries"].([]any)) != 0 || out["next"] != nil {
		t.Fatalf("got %v", out)
	}
}

func TestFleetLogBadParams(t *testing.T) {
	s := fleetTestServer(t, nil)
	for _, q := range []string{"?n=0", "?n=201", "?before=-1", "?before=x"} {
		r := httptest.NewRequest("GET", "/fleet/log"+q, nil)
		r.Header.Set("Authorization", "Bearer tok")
		w := httptest.NewRecorder()
		fleetMux(s).ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", q, w.Code)
		}
	}
}

func TestAppendFleetLogCreatesDirAndAppends(t *testing.T) {
	s := fleetTestServer(t, nil)
	s.fleet.dir = filepath.Join(t.TempDir(), "fleet") // does not exist yet
	if err := s.appendFleetLog(fleetLogEntry{At: "2026-10-13T09:00:00Z", Type: "web-say", Text: "a\nb"}); err != nil {
		t.Fatal(err)
	}
	if err := s.appendFleetLog(fleetLogEntry{At: "2026-10-13T09:00:01Z", Type: "web-say", Text: "c", CardID: "x"}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(s.fleet.dir, "log.jsonl"))
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 || !strings.Contains(lines[1], `"cardId":"x"`) {
		t.Fatalf("log = %q", data)
	}
	if st, _ := os.Stat(filepath.Join(s.fleet.dir, "log.jsonl")); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode().Perm())
	}
}

func TestFleetStateCardHashesAndAnswered(t *testing.T) {
	now := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	s := fleetTestServer(t, nil)
	s.fleet.now = func() time.Time { return now }
	writeFleetFile(t, s, "state.json", `{"version":1,"writtenAt":"2026-10-13T09:59:00Z","cards":[
		{"id":"a","kind":"needs-you","title":"A?"},
		{"id":"b","kind":"explicit-yes","title":"B?"},
		{"id":"c","kind":"done","title":"C"},
		{"kind":"needs-you","title":"no id"}]}`)
	out := getFleetJSON(t, s, "/fleet/state")
	hashes, _ := out["cardHashes"].(map[string]any)
	if len(hashes) != 2 || hashes["a"] == nil || hashes["b"] == nil {
		t.Fatalf("cardHashes = %v", out["cardHashes"])
	}
	if ans, ok := out["answered"].(map[string]any); !ok || len(ans) != 0 {
		t.Fatalf("answered = %#v, want {}", out["answered"])
	}

	state, _ := s.loadFleetState()
	cards := fleetDecisionCards(state)
	s.fleet.answered.record("a", cards["a"].Hash,
		fleetAttempt{OptionKey: "yes", At: now.Format(time.RFC3339), Status: fleetAttemptSent}, cards, now)
	writeFleetFile(t, s, "state.json", `{"version":1,"cards":[{"id":"torn"`)
	out = getFleetJSON(t, s, "/fleet/state")
	if ans, _ := out["answered"].(map[string]any); ans["a"] == nil {
		t.Fatalf("a bad state.json must prune nothing: %v", out["answered"])
	}
	if out["stateError"] == nil {
		t.Fatal("stateError must be set")
	}

	writeFleetFile(t, s, "state.json", `{"version":1,"writtenAt":"2026-10-13T09:59:00Z","cards":[{"id":"b","kind":"explicit-yes","title":"B?"}]}`)
	out = getFleetJSON(t, s, "/fleet/state")
	if ans, ok := out["answered"].(map[string]any); !ok || len(ans) != 0 {
		t.Fatalf("answered = %#v, want {}", out["answered"])
	}
}
