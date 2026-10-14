package main

import (
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fleetPushCall struct {
	endpoint string
	payload  fleetPushPayload
}

// fleetPushServer is fleetTestServer with a VAPID key, a subscription file
// in a temp dir, a settable clock and a recorded send. codes maps an
// endpoint to the status the fake push service answers (default 201).
func fleetPushServer(t *testing.T) (*server, *time.Time, *[]fleetPushCall, map[string]int) {
	t.Helper()
	s := fleetTestServer(t, nil)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s.fleet.vapid = key
	s.fleet.pushSubsPath = filepath.Join(t.TempDir(), "cfg", "push-subs.json")
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	s.fleet.now = func() time.Time { return now }
	var mu sync.Mutex
	var calls []fleetPushCall
	codes := map[string]int{}
	s.fleet.pushSend = func(sub pushSub, payload []byte) (int, error) {
		var p fleetPushPayload
		if err := json.Unmarshal(payload, &p); err != nil {
			t.Errorf("payload is not JSON: %s", payload)
		}
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, fleetPushCall{sub.Endpoint, p})
		if c, ok := codes[sub.Endpoint]; ok {
			return c, nil
		}
		return http.StatusCreated, nil
	}
	return s, &now, &calls, codes
}

func testPushKeys(t *testing.T) (p256dh, auth string) {
	t.Helper()
	k, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	a := make([]byte, 16)
	_, _ = rand.Read(a)
	return base64.RawURLEncoding.EncodeToString(k.PublicKey().Bytes()), base64.RawURLEncoding.EncodeToString(a)
}

func subscribeBody(t *testing.T, endpoint string) string {
	t.Helper()
	p, a := testPushKeys(t)
	b, _ := json.Marshal(map[string]any{"endpoint": endpoint, "expirationTime": nil, "keys": map[string]string{"p256dh": p, "auth": a}})
	return string(b)
}

func postFleetPush(s *server, method, path, body string, setup func(*http.Request)) (*httptest.ResponseRecorder, fleetPushResult) {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if setup == nil {
		r.Header.Set("Authorization", "Bearer tok")
	} else {
		setup(r)
	}
	w := httptest.NewRecorder()
	fleetMux(s).ServeHTTP(w, r)
	var out fleetPushResult
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w, out
}

func readPushSubsFile(t *testing.T, s *server) []pushSub {
	t.Helper()
	data, err := os.ReadFile(s.fleet.pushSubsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var subs []pushSub
	if err := json.Unmarshal(data, &subs); err != nil {
		t.Fatal(err)
	}
	return subs
}

func TestFleetPushKey(t *testing.T) {
	s, _, _, _ := fleetPushServer(t)
	w, _ := postFleetPush(s, "GET", "/fleet/push/key", "", nil)
	var got map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != 200 {
		t.Fatalf("key = %d %s", w.Code, w.Body)
	}
	want, _ := vapidPublicKey(s.fleet.vapid)
	if got["publicKey"] != want || len(got) != 1 {
		t.Fatalf("key body = %v", got)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Error("key must be no-store")
	}
	if w, out := postFleetPush(s, "GET", "/fleet/push/key", "", func(*http.Request) {}); w.Code != 401 || out.Code != "unauthorized" {
		t.Fatalf("no auth = %d %+v", w.Code, out)
	}
	s.fleet.vapid = nil
	if w, out := postFleetPush(s, "GET", "/fleet/push/key", "", nil); w.Code != 503 || out.Code != "push_unavailable" {
		t.Fatalf("no key = %d %+v", w.Code, out)
	}
}

func TestFleetPushSubscribeRefusals(t *testing.T) {
	p, a := testPushKeys(t)
	body := func(endpoint, p256dh, auth string) string {
		b, _ := json.Marshal(map[string]any{"endpoint": endpoint, "expirationTime": nil, "keys": map[string]string{"p256dh": p256dh, "auth": auth}})
		return string(b)
	}
	ok := "https://fcm.googleapis.com/fcm/send/abc"
	notOnCurve := base64.RawURLEncoding.EncodeToString(append([]byte{4}, make([]byte, 64)...))
	cases := []struct {
		name   string
		body   string
		setup  func(*http.Request)
		status int
		code   string
	}{
		{"no auth", body(ok, p, a), func(*http.Request) {}, 401, "unauthorized"},
		{"cookie without origin", body(ok, p, a), func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: "cs_fleet", Value: "sec"})
		}, 403, "origin"},
		{"not json", `nope`, nil, 400, "bad_request"},
		{"null", `null`, nil, 400, "bad_request"},
		{"unknown field", `{"endpoint":"` + ok + `","keys":{"p256dh":"` + p + `","auth":"` + a + `"},"x":1}`, nil, 400, "bad_request"},
		{"unknown key field", `{"endpoint":"` + ok + `","keys":{"p256dh":"` + p + `","auth":"` + a + `","x":"y"}}`, nil, 400, "bad_request"},
		{"trailing", body(ok, p, a) + " x", nil, 400, "bad_request"},
		{"too large", `{"endpoint":"` + strings.Repeat("a", 5000) + `"}`, nil, 400, "bad_request"},
		{"http endpoint", body("http://fcm.googleapis.com/fcm/send/abc", p, a), nil, 400, "bad_endpoint"},
		{"other host", body("https://evil.example.com/push", p, a), nil, 400, "bad_endpoint"},
		{"suffix trick", body("https://evilfcm.googleapis.com.example.com/x", p, a), nil, 400, "bad_endpoint"},
		{"lookalike suffix", body("https://notfcm.googleapis.com/x", p, a), nil, 400, "bad_endpoint"},
		{"userinfo", body("https://u:p@fcm.googleapis.com/x", p, a), nil, 400, "bad_endpoint"},
		{"odd port", body("https://fcm.googleapis.com:8443/x", p, a), nil, 400, "bad_endpoint"},
		{"explicit 443", body("https://fcm.googleapis.com:443/x", p, a), nil, 400, "bad_endpoint"},
		{"empty port", body("https://fcm.googleapis.com:/x", p, a), nil, 400, "bad_endpoint"},
		{"relative", body("/fleet/state", p, a), nil, 400, "bad_endpoint"},
		{"long endpoint", body(ok+strings.Repeat("a", 1100), p, a), nil, 400, "bad_endpoint"},
		{"no keys", `{"endpoint":"` + ok + `"}`, nil, 400, "bad_keys"},
		{"short p256dh", body(ok, "BAAA", a), nil, 400, "bad_keys"},
		{"off-curve p256dh", body(ok, notOnCurve, a), nil, 400, "bad_keys"},
		{"short auth", body(ok, p, "AAAA"), nil, 400, "bad_keys"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _, _ := fleetPushServer(t)
			w, out := postFleetPush(s, "POST", "/fleet/push/subscribe", tc.body, tc.setup)
			if w.Code != tc.status || out.Code != tc.code {
				t.Fatalf("got %d %+v, want %d %s", w.Code, out, tc.status, tc.code)
			}
			if subs := readPushSubsFile(t, s); len(subs) != 0 {
				t.Fatalf("a refusal stored %d subscriptions", len(subs))
			}
		})
	}
}

func TestFleetPushSubscribeStoresDedupesAndCaps(t *testing.T) {
	s, now, _, _ := fleetPushServer(t)
	// Every allowlisted service, exact and as a subdomain.
	for _, ep := range []string{"https://web.push.apple.com/QG", "https://updates.push.services.mozilla.com/wpush/v2/x",
		"https://wns2-db5p.notify.windows.com/w/?token=x", "https://FCM.googleapis.com/fcm/send/y"} {
		if w, out := postFleetPush(s, "POST", "/fleet/push/subscribe", subscribeBody(t, ep), nil); w.Code != 200 || !out.OK {
			t.Fatalf("%s: %d %s", ep, w.Code, w.Body)
		}
	}
	// A cookie with the same origin is fine too.
	w, _ := postFleetPush(s, "POST", "/fleet/push/subscribe", subscribeBody(t, "https://fcm.googleapis.com/fcm/send/c"), func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: "cs_fleet", Value: "sec"})
		r.Header.Set("Origin", "http://example.com")
	})
	if w.Code != 200 {
		t.Fatalf("cookie same-origin = %d %s", w.Code, w.Body)
	}
	st, err := os.Stat(s.fleet.pushSubsPath)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("subs file mode %v, %v", st, err)
	}
	if got := len(readPushSubsFile(t, s)); got != 5 {
		t.Fatalf("stored %d, want 5", got)
	}
	// The same endpoint again (a page load re-post) is one entry.
	ep := "https://fcm.googleapis.com/fcm/send/dup"
	body := subscribeBody(t, ep)
	for range 3 {
		postFleetPush(s, "POST", "/fleet/push/subscribe", body, nil)
	}
	postFleetPush(s, "POST", "/fleet/push/subscribe", subscribeBody(t, ep), nil) // new keys replace
	n := 0
	for _, sub := range readPushSubsFile(t, s) {
		if sub.Endpoint == ep {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("endpoint stored %d times", n)
	}
	// A 17th distinct endpoint drops the oldest.
	for i := range 20 {
		*now = now.Add(time.Second)
		postFleetPush(s, "POST", "/fleet/push/subscribe", subscribeBody(t, "https://fcm.googleapis.com/fcm/send/n"+string(rune('a'+i))), nil)
	}
	subs := readPushSubsFile(t, s)
	if len(subs) != fleetPushMaxSubs {
		t.Fatalf("stored %d, want %d", len(subs), fleetPushMaxSubs)
	}
	if subs[len(subs)-1].Endpoint != "https://fcm.googleapis.com/fcm/send/nt" || subs[0].Endpoint != "https://fcm.googleapis.com/fcm/send/ne" {
		t.Fatalf("kept %s .. %s", subs[0].Endpoint, subs[len(subs)-1].Endpoint)
	}
	// Unsubscribe removes; an absent endpoint is still ok.
	for _, e := range []string{subs[0].Endpoint, "https://fcm.googleapis.com/fcm/send/never"} {
		if w, out := postFleetPush(s, "POST", "/fleet/push/unsubscribe", `{"endpoint":"`+e+`"}`, nil); w.Code != 200 || !out.OK {
			t.Fatalf("unsubscribe = %d %s", w.Code, w.Body)
		}
	}
	if got := len(readPushSubsFile(t, s)); got != fleetPushMaxSubs-1 {
		t.Fatalf("after unsubscribe %d", got)
	}
	for _, b := range []string{`{}`, `{"endpoint":"x","y":1}`, `null`} {
		if w, out := postFleetPush(s, "POST", "/fleet/push/unsubscribe", b, nil); w.Code != 400 || out.Code != "bad_request" {
			t.Fatalf("unsubscribe %s = %d %+v", b, w.Code, out)
		}
	}
	if w, out := postFleetPush(s, "POST", "/fleet/push/unsubscribe", `{"endpoint":"x"}`, func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: "cs_fleet", Value: "sec"})
		r.Header.Set("Origin", "https://evil.example")
	}); w.Code != 403 || out.Code != "origin" {
		t.Fatalf("cross-origin unsubscribe = %d %+v", w.Code, out)
	}
	// A restart reads the file back.
	s2 := fleetTestServer(t, nil)
	s2.fleet.pushSubsPath = s.fleet.pushSubsPath
	if got := len(s2.pushSubsSnapshot()); got != fleetPushMaxSubs-1 {
		t.Fatalf("reloaded %d", got)
	}
}

func TestFleetPushDiff(t *testing.T) {
	prev := map[string]string{"a": "working", "b": "needs-you", "c": "explicit-yes", "d": "waiting", "e": "done"}
	cards := []fleetPushCard{
		{ID: "a", Kind: "explicit-yes"}, // working → explicit-yes: push
		{ID: "b", Kind: "needs-you"},    // unchanged: no
		{ID: "c", Kind: "needs-you"},    // between decision kinds: no
		{ID: "d", Kind: "needs-you"},    // waiting → needs-you: push
		{ID: "e", Kind: "done"},
		{ID: "n1", Kind: "needs-you"}, // new: push
		{ID: "n2", Kind: "working"},   // new, not a decision: no
		{ID: "n3", Kind: "waiting"},
		{ID: "n4", Kind: "done"},
	}
	var got []string
	for _, c := range fleetPushDiff(prev, cards) {
		got = append(got, c.ID)
	}
	if strings.Join(got, ",") != "a,d,n1" {
		t.Fatalf("triggered %v", got)
	}
}

// fleetPushWriteState writes state.json with cards and a fresh mtime, so
// loadFleetState rereads it even when the size does not change.
func fleetPushWriteState(t *testing.T, s *server, step int, cards ...map[string]any) {
	t.Helper()
	if cards == nil {
		cards = []map[string]any{}
	}
	b, _ := json.Marshal(map[string]any{"version": 1, "writtenAt": "2026-10-14T12:00:00Z", "cards": cards})
	p := filepath.Join(s.fleet.dir, "state.json")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 1, 1, 0, 0, step, 0, time.UTC)
	if err := os.Chtimes(p, at, at); err != nil {
		t.Fatal(err)
	}
}

func card(id, kind, ticket, title string) map[string]any {
	return map[string]any{"id": id, "kind": kind, "ticket": ticket, "title": title,
		"summary": "secret summary", "options": []any{map[string]any{"key": "a", "label": "https://x"}}}
}

// subscribe adds n subscriptions through the handler.
func fleetPushSubscribeN(t *testing.T, s *server, eps ...string) {
	t.Helper()
	for _, ep := range eps {
		if w, _ := postFleetPush(s, "POST", "/fleet/push/subscribe", subscribeBody(t, ep), nil); w.Code != 200 {
			t.Fatalf("subscribe %s: %d %s", ep, w.Code, w.Body)
		}
	}
}

// advance moves the clock and runs one tick.
func advance(s *server, now *time.Time, d time.Duration) {
	*now = now.Add(d)
	s.fleetPushTick()
}

func TestFleetPushTriggerBaselineAndSingleCard(t *testing.T) {
	s, now, calls, _ := fleetPushServer(t)
	fleetPushSubscribeN(t, s, "https://fcm.googleapis.com/fcm/send/1", "https://web.push.apple.com/2")
	// Baseline: decisions already on the file at start never push.
	fleetPushWriteState(t, s, 1, card("old", "needs-you", "DR-1", "Old"), card("w", "working", "DR-2", "Work"), card("y", "explicit-yes", "", "Y"))
	s.fleetPushTick()
	advance(s, now, 30*time.Second)
	if len(*calls) != 0 {
		t.Fatalf("baseline pushed %d", len(*calls))
	}
	// working → needs-you, with a long title: one push after the window.
	long := strings.Repeat("é", 200)
	fleetPushWriteState(t, s, 2, card("old", "needs-you", "DR-1", "Old"), card("w", "needs-you", "DR-2", long), card("y", "needs-you", "", "Y"))
	advance(s, now, 5*time.Second) // seen: the window opens
	advance(s, now, 5*time.Second)
	if len(*calls) != 0 {
		t.Fatal("pushed before the window ended")
	}
	advance(s, now, 5*time.Second)
	if len(*calls) != 2 {
		t.Fatalf("want one push per subscription, got %d", len(*calls))
	}
	p := (*calls)[0].payload
	if p.Title != "DR-2" || p.Tag != "w" || p.CardID != "w" || len([]rune(p.Body)) != fleetPushTitleMax || !strings.HasSuffix(p.Body, "…") {
		t.Fatalf("payload = %+v", p)
	}
	// needs-you → needs-you, and explicit-yes → needs-you (card y): nothing.
	advance(s, now, 30*time.Second)
	if len(*calls) != 2 {
		t.Fatalf("an unchanged decision pushed again: %d", len(*calls))
	}
	// w goes back to working and returns to needs-you: still one push per id.
	fleetPushWriteState(t, s, 3, card("w", "working", "DR-2", "x"), card("y", "needs-you", "", "Y"))
	advance(s, now, 5*time.Second)
	fleetPushWriteState(t, s, 4, card("w", "needs-you", "DR-2", "x"), card("y", "needs-you", "", "Y"))
	advance(s, now, 15*time.Second)
	if len(*calls) != 2 {
		t.Fatalf("a card pushed twice: %d", len(*calls))
	}
	// A card that leaves the file and comes back may push again; no ticket
	// → title FLEET.
	fleetPushWriteState(t, s, 5, card("y", "needs-you", "", "Y"))
	advance(s, now, 5*time.Second)
	fleetPushWriteState(t, s, 6, card("y", "needs-you", "", "Y"), card("w", "explicit-yes", "", "Back"))
	advance(s, now, 5*time.Second)
	advance(s, now, 10*time.Second)
	if len(*calls) != 4 || (*calls)[2].payload.Title != "FLEET" || (*calls)[2].payload.Body != "Back" {
		t.Fatalf("return push = %+v", *calls)
	}
}

func TestFleetPushTriggerNonDecisionsAndNoSubs(t *testing.T) {
	s, now, calls, _ := fleetPushServer(t)
	fleetPushWriteState(t, s, 1)
	s.fleetPushTick()
	// New working / waiting / done cards never push.
	fleetPushWriteState(t, s, 2, card("a", "working", "", "a"), card("b", "waiting", "", "b"), card("c", "done", "", "c"))
	advance(s, now, 5*time.Second)
	// With no subscriptions a new decision only moves the baseline.
	fleetPushWriteState(t, s, 3, card("d", "needs-you", "", "d"))
	advance(s, now, 5*time.Second)
	advance(s, now, 15*time.Second)
	fleetPushSubscribeN(t, s, "https://fcm.googleapis.com/fcm/send/1")
	advance(s, now, 15*time.Second)
	if len(*calls) != 0 {
		t.Fatalf("pushed %+v", *calls)
	}
	// A torn file is skipped and keeps the baseline.
	if err := os.WriteFile(filepath.Join(s.fleet.dir, "state.json"), []byte(`{"cards": [`), 0o600); err != nil {
		t.Fatal(err)
	}
	advance(s, now, 5*time.Second)
	fleetPushWriteState(t, s, 9, card("d", "needs-you", "", "d"))
	advance(s, now, 15*time.Second)
	if len(*calls) != 0 {
		t.Fatalf("a torn file reset the baseline: %+v", *calls)
	}
}

func TestFleetPushTriggerBatchesAndDropsGone(t *testing.T) {
	s, now, calls, codes := fleetPushServer(t)
	eps := []string{"https://fcm.googleapis.com/fcm/send/ok", "https://fcm.googleapis.com/fcm/send/gone410",
		"https://fcm.googleapis.com/fcm/send/gone404", "https://fcm.googleapis.com/fcm/send/err500"}
	fleetPushSubscribeN(t, s, eps...)
	codes[eps[1]], codes[eps[2]], codes[eps[3]] = 410, 404, 500
	fleetPushWriteState(t, s, 1)
	s.fleetPushTick()
	fleetPushWriteState(t, s, 2, card("a", "needs-you", "DR-1", "A"))
	advance(s, now, 5*time.Second)
	fleetPushWriteState(t, s, 3, card("a", "needs-you", "DR-1", "A"), card("b", "explicit-yes", "DR-2", "B"), card("c", "needs-you", "DR-3", "C"))
	advance(s, now, 5*time.Second)
	if len(*calls) != 0 {
		t.Fatal("pushed before the window ended")
	}
	advance(s, now, 5*time.Second)
	if len(*calls) != len(eps) {
		t.Fatalf("want one push per subscription, got %d", len(*calls))
	}
	for _, c := range *calls {
		if c.payload != (fleetPushPayload{Title: "FLEET", Body: "3 decisions need you", Tag: "fleet-batch"}) {
			t.Fatalf("batch payload = %+v", c.payload)
		}
	}
	var left []string
	for _, sub := range readPushSubsFile(t, s) {
		left = append(left, sub.Endpoint)
	}
	if strings.Join(left, ",") != eps[0]+","+eps[3] {
		t.Fatalf("after 404/410 the file holds %v", left)
	}
}

func TestFleetPushPayloadHoldsOnlyTicketAndTitle(t *testing.T) {
	p := fleetPushMessage([]fleetPushCard{{ID: "x", Kind: "needs-you", Ticket: "DR-9  \n", Title: "Line\none"}})
	b, _ := json.Marshal(p)
	if string(b) != `{"title":"DR-9","body":"Line one","tag":"x","cardId":"x"}` {
		t.Fatalf("payload = %s", b)
	}
}

func TestFleetPushLongCardIDHasNoCardID(t *testing.T) {
	id := strings.Repeat("x", fleetPushIDMax+1)
	p := fleetPushMessage([]fleetPushCard{{ID: id, Kind: "needs-you", Ticket: "DR-1", Title: "T"}})
	if p.Tag != "fleet-card" || p.CardID != "" {
		t.Fatalf("long id payload = %+v", p)
	}
	ok := strings.Repeat("y", fleetPushIDMax)
	if p := fleetPushMessage([]fleetPushCard{{ID: ok, Kind: "needs-you"}}); p.Tag != ok || p.CardID != ok {
		t.Fatalf("128-byte id payload = %+v", p)
	}
}

// fleetPushQueue puts one decision card in the batch, as a tick would.
func fleetPushQueue(s *server, id string) {
	st := &s.fleet.push
	st.cards = map[string]fleetPushCard{id: {ID: id, Kind: "needs-you", Title: id}}
	st.queue = []string{id}
	st.windowAt = s.fleetNow()
}

func TestFleetPushDropsAfterFiveFailures(t *testing.T) {
	s, _, calls, codes := fleetPushServer(t)
	bad, flaky := "https://fcm.googleapis.com/fcm/send/bad", "https://fcm.googleapis.com/fcm/send/flaky"
	fleetPushSubscribeN(t, s, bad, flaky)
	codes[bad], codes[flaky] = 500, 401
	has := func(ep string) bool {
		for _, sub := range readPushSubsFile(t, s) {
			if sub.Endpoint == ep {
				return true
			}
		}
		return false
	}
	for i := 1; i <= 4; i++ {
		fleetPushQueue(s, "c")
		s.fleetPushFlush()
	}
	if !has(bad) || !has(flaky) {
		t.Fatal("four failures must not drop a subscription")
	}
	// A success resets the count.
	codes[flaky] = 201
	fleetPushQueue(s, "c")
	s.fleetPushFlush()
	codes[flaky] = 401
	if has(bad) {
		t.Fatal("five failures in a row must drop the subscription")
	}
	for i := 1; i <= 4; i++ {
		fleetPushQueue(s, "c")
		s.fleetPushFlush()
	}
	if !has(flaky) {
		t.Fatal("a success in between must reset the count")
	}
	fleetPushQueue(s, "c")
	s.fleetPushFlush()
	if has(flaky) {
		t.Fatal("five more failures must drop it")
	}
	if len(*calls) != 5+5+4+1 {
		t.Fatalf("sends = %d", len(*calls))
	}
	if len(s.fleet.pushFails) != 0 {
		t.Fatalf("dropped subscriptions keep counts: %v", s.fleet.pushFails)
	}
}

func TestFleetPushGoneKeepsResubscribedDevice(t *testing.T) {
	s, _, _, _ := fleetPushServer(t)
	ep := "https://fcm.googleapis.com/fcm/send/re"
	fleetPushSubscribeN(t, s, ep)
	p, a := testPushKeys(t)
	s.fleet.pushSend = func(sub pushSub, payload []byte) (int, error) {
		// The device re-subscribes with new keys while the old one is sent.
		if err := s.addPushSub(pushSub{Endpoint: ep, P256dh: p, Auth: a, CreatedAt: s.fleetNow()}); err != nil {
			t.Error(err)
		}
		return http.StatusGone, nil
	}
	fleetPushQueue(s, "c")
	s.fleetPushFlush()
	subs := readPushSubsFile(t, s)
	if len(subs) != 1 || subs[0].P256dh != p || subs[0].Auth != a {
		t.Fatalf("the re-subscribed device was removed: %+v", subs)
	}
}
