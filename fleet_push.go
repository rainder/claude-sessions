package main

import (
	"crypto/ecdh"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Web Push for the FLEET page. The page subscribes through the browser's
// PushManager with the server's VAPID public key; the server keeps the
// subscriptions in push-subs.json and, when a card on state.json turns into a
// decision (needs-you or explicit-yes), sends one notification to every
// subscription. The payload holds the card's ticket and title only.

const (
	fleetPushMaxSubs     = 16
	fleetPushMaxEndpoint = 1024
	fleetPushPoll        = 5 * time.Second
	// fleetPushWindow: the first trigger opens a batch; at the first poll
	// this long after it, the whole batch goes as one notification.
	fleetPushWindow    = 10 * time.Second
	fleetPushTitleMax  = 120
	fleetPushTicketMax = 40
	fleetPushBatchTag  = "fleet-batch"
	// fleetPushCardTag stands in for a card id over fleetPushIDMax bytes.
	fleetPushCardTag = "fleet-card"
	fleetPushIDMax   = 128
	// fleetPushMaxFails: consecutive non-2xx answers (other than 404/410,
	// which remove at once) before a subscription is dropped.
	fleetPushMaxFails = 5
)

// fleetPushHosts are the push services a subscription endpoint may point at
// (exact host or a subdomain). Anything else is refused, so a crafted
// endpoint cannot make the server POST to an arbitrary host.
var fleetPushHosts = []string{
	"fcm.googleapis.com",
	"android.googleapis.com",
	"updates.push.services.mozilla.com",
	"push.services.mozilla.com",
	"web.push.apple.com",
	"notify.windows.com",
	"push.apple.com",
}

// pushSub is one stored browser subscription.
type pushSub struct {
	Endpoint  string    `json:"endpoint"`
	P256dh    string    `json:"p256dh"`
	Auth      string    `json:"auth"`
	CreatedAt time.Time `json:"createdAt"`
}

type fleetPushResult struct {
	OK    bool   `json:"ok,omitempty"`
	Code  string `json:"code,omitempty"`
	Error string `json:"error,omitempty"`
}

// fleetPushSubsPath is the production subscription file.
func fleetPushSubsPath() string {
	dir := ConfigDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "push-subs.json")
}

// validPushEndpoint: an absolute https URL with no userinfo and no explicit
// port, at most fleetPushMaxEndpoint bytes, on an allowlisted push host.
func validPushEndpoint(raw string) bool {
	if raw == "" || len(raw) > fleetPushMaxEndpoint {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Opaque != "" || u.Host == "" {
		return false
	}
	if u.Port() != "" || strings.HasSuffix(u.Host, ":") {
		return false
	}
	host := strings.ToLower(u.Hostname())
	for _, h := range fleetPushHosts {
		if host == h || strings.HasSuffix(host, "."+h) {
			return true
		}
	}
	return false
}

// validPushKeys: p256dh is a P-256 point (65 bytes, uncompressed) and auth is
// 16 bytes, both base64url with padding tolerated.
func validPushKeys(p256dh, auth string) bool {
	pub, err := decodePushKey(p256dh)
	if err != nil || len(pub) != 65 {
		return false
	}
	if _, err := ecdh.P256().NewPublicKey(pub); err != nil {
		return false
	}
	a, err := decodePushKey(auth)
	return err == nil && len(a) == 16
}

// loadPushSubsLocked loads the file once. A missing or bad file is no
// subscriptions: the page re-posts its subscription on every load.
// Caller holds pushMu.
func (s *server) loadPushSubsLocked() {
	f := &s.fleet
	if f.pushLoaded {
		return
	}
	f.pushLoaded = true
	f.pushSubs = nil
	if f.pushSubsPath == "" {
		return
	}
	data, err := os.ReadFile(f.pushSubsPath)
	if err != nil {
		if !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "fleet push: cannot read subscriptions: %v\n", err)
		}
		return
	}
	var subs []pushSub
	if err := json.Unmarshal(data, &subs); err != nil {
		fmt.Fprintf(os.Stderr, "fleet push: subscriptions file is not valid JSON; starting empty\n")
		return
	}
	for _, sub := range subs {
		if validPushEndpoint(sub.Endpoint) && validPushKeys(sub.P256dh, sub.Auth) {
			f.pushSubs = append(f.pushSubs, sub)
		}
	}
}

// savePushSubsLocked writes the list atomically. "" path → memory only.
// Caller holds pushMu.
func (s *server) savePushSubsLocked() error {
	f := &s.fleet
	if f.pushSubsPath == "" {
		return nil
	}
	subs := f.pushSubs
	if subs == nil {
		subs = []pushSub{}
	}
	data, err := json.MarshalIndent(subs, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(f.pushSubsPath, append(data, '\n'), 0o600)
}

// addPushSub stores sub, newest last. The same endpoint with the same keys is
// a no-op (the page re-posts on every load); new keys replace the entry and
// move it to the end. A 17th entry drops the oldest.
func (s *server) addPushSub(sub pushSub) error {
	f := &s.fleet
	f.pushMu.Lock()
	defer f.pushMu.Unlock()
	s.loadPushSubsLocked()
	out := make([]pushSub, 0, len(f.pushSubs)+1)
	for _, old := range f.pushSubs {
		if old.Endpoint == sub.Endpoint {
			if old.P256dh == sub.P256dh && old.Auth == sub.Auth {
				return nil
			}
			continue
		}
		out = append(out, old)
	}
	out = append(out, sub)
	if len(out) > fleetPushMaxSubs {
		out = out[len(out)-fleetPushMaxSubs:]
	}
	f.pushSubs = out
	// Counts belong to stored subscriptions only: drop replaced and evicted.
	live := make(map[pushSub]bool, len(out))
	for _, sub := range out {
		live[pushSubKey(sub)] = true
	}
	for k := range f.pushFails {
		if !live[k] {
			delete(f.pushFails, k)
		}
	}
	return s.savePushSubsLocked()
}

// removePushSubs drops every subscription gone reports true for.
func (s *server) removePushSubs(gone func(pushSub) bool) error {
	f := &s.fleet
	f.pushMu.Lock()
	defer f.pushMu.Unlock()
	s.loadPushSubsLocked()
	out := f.pushSubs[:0:0]
	for _, sub := range f.pushSubs {
		if !gone(sub) {
			out = append(out, sub)
		} else {
			delete(f.pushFails, pushSubKey(sub))
		}
	}
	if len(out) == len(f.pushSubs) {
		return nil
	}
	f.pushSubs = out
	return s.savePushSubsLocked()
}

// pushSubsSnapshot is a copy of the current list.
func (s *server) pushSubsSnapshot() []pushSub {
	f := &s.fleet
	f.pushMu.Lock()
	defer f.pushMu.Unlock()
	s.loadPushSubsLocked()
	return append([]pushSub(nil), f.pushSubs...)
}

// fleetPushGuard runs the shared checks: auth, same origin for a cookie POST,
// and a loaded key. It writes the refusal and returns false.
func (s *server) fleetPushGuard(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set("Cache-Control", "no-store")
	kind := s.fleetAuth(r)
	if kind == fleetAuthNone {
		writeJSON(w, http.StatusUnauthorized, fleetPushResult{Code: "unauthorized", Error: "Unauthorized"})
		return false
	}
	if r.Method == http.MethodPost && kind == fleetAuthCookie && !sameOrigin(r) {
		writeJSON(w, http.StatusForbidden, fleetPushResult{Code: "origin", Error: "origin mismatch"})
		return false
	}
	if s.fleet.vapid == nil {
		writeJSON(w, http.StatusServiceUnavailable, fleetPushResult{Code: "push_unavailable", Error: "push is not set up on this server"})
		return false
	}
	return true
}

// fleetPushKey is GET /fleet/push/key: the VAPID public key, nothing else.
func (s *server) fleetPushKey(w http.ResponseWriter, r *http.Request) {
	if !s.fleetPushGuard(w, r) {
		return
	}
	pub, err := vapidPublicKey(s.fleet.vapid)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, fleetPushResult{Code: "push_unavailable", Error: "push is not set up on this server"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"publicKey": pub})
}

// decodeStrictJSON decodes exactly one JSON value into dst (a pointer to a
// pointer, so a top-level null stays nil): unknown fields and trailing
// content are errors.
func decodeStrictJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("trailing content")
	}
	return nil
}

// fleetPushSubscribe is POST /fleet/push/subscribe with the browser's
// PushSubscription.toJSON().
func (s *server) fleetPushSubscribe(w http.ResponseWriter, r *http.Request) {
	if !s.fleetPushGuard(w, r) {
		return
	}
	var body *struct {
		Endpoint       string   `json:"endpoint"`
		ExpirationTime *float64 `json:"expirationTime"`
		Keys           *struct {
			P256dh string `json:"p256dh"`
			Auth   string `json:"auth"`
		} `json:"keys"`
	}
	if err := decodeStrictJSON(w, r, &body); err != nil || body == nil {
		writeJSON(w, http.StatusBadRequest, fleetPushResult{Code: "bad_request", Error: "bad request body"})
		return
	}
	if !validPushEndpoint(body.Endpoint) {
		writeJSON(w, http.StatusBadRequest, fleetPushResult{Code: "bad_endpoint", Error: "endpoint is not a known push service"})
		return
	}
	if body.Keys == nil || !validPushKeys(body.Keys.P256dh, body.Keys.Auth) {
		writeJSON(w, http.StatusBadRequest, fleetPushResult{Code: "bad_keys", Error: "bad subscription keys"})
		return
	}
	sub := pushSub{Endpoint: body.Endpoint, P256dh: body.Keys.P256dh, Auth: body.Keys.Auth, CreatedAt: s.fleetNow().UTC()}
	if err := s.addPushSub(sub); err != nil {
		fmt.Fprintf(os.Stderr, "fleet push: cannot save subscriptions: %v\n", err)
		writeJSON(w, http.StatusInternalServerError, fleetPushResult{Code: "store_failed", Error: "cannot save the subscription"})
		return
	}
	writeJSON(w, http.StatusOK, fleetPushResult{OK: true})
}

// fleetPushUnsubscribe is POST /fleet/push/unsubscribe {"endpoint"}. An
// endpoint that is not stored is still ok.
func (s *server) fleetPushUnsubscribe(w http.ResponseWriter, r *http.Request) {
	if !s.fleetPushGuard(w, r) {
		return
	}
	var body *struct {
		Endpoint string `json:"endpoint"`
	}
	if err := decodeStrictJSON(w, r, &body); err != nil || body == nil || body.Endpoint == "" {
		writeJSON(w, http.StatusBadRequest, fleetPushResult{Code: "bad_request", Error: "bad request body"})
		return
	}
	if err := s.removePushSubs(func(sub pushSub) bool { return sub.Endpoint == body.Endpoint }); err != nil {
		fmt.Fprintf(os.Stderr, "fleet push: cannot save subscriptions: %v\n", err)
		writeJSON(w, http.StatusInternalServerError, fleetPushResult{Code: "store_failed", Error: "cannot save the subscription list"})
		return
	}
	writeJSON(w, http.StatusOK, fleetPushResult{OK: true})
}

// ---- trigger -------------------------------------------------------------

// fleetPushCard is what the trigger reads from a state.json card.
type fleetPushCard struct {
	ID     string
	Kind   string
	Ticket string
	Title  string
}

func fleetPushKind(kind string) bool { return kind == "needs-you" || kind == "explicit-yes" }

// fleetPushDiff returns the cards that became decisions since prev: a
// needs-you or explicit-yes card that prev did not hold, or held as another
// kind. Moving between the two decision kinds is not a new decision.
func fleetPushDiff(prev map[string]string, cards []fleetPushCard) []fleetPushCard {
	var out []fleetPushCard
	for _, c := range cards {
		if !fleetPushKind(c.Kind) {
			continue
		}
		if old, ok := prev[c.ID]; ok && fleetPushKind(old) {
			continue
		}
		out = append(out, c)
	}
	return out
}

// fleetPushLoop is the trigger's state. Only the loop goroutine (or a test,
// one tick at a time) touches it.
type fleetPushLoop struct {
	baselined bool
	prev      map[string]string        // card id → kind, from the last good parse
	cards     map[string]fleetPushCard // card id → latest card, for the flush text
	// pushed: ids already pushed (or queued) in this process. An id that
	// leaves state.json is pruned, so a card that goes away and comes back
	// later can push again; while it stays, it pushes once.
	pushed   map[string]bool
	queue    []string  // queued ids, trigger order
	windowAt time.Time // zero → no batch open
}

// fleetPushCards reads the cards through loadFleetState (same file, same size
// and validity rules). ok is false when the file is missing or bad.
func (s *server) fleetPushCards() ([]fleetPushCard, bool) {
	state, stateErr := s.loadFleetState()
	if stateErr != "" {
		return nil, false
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(state["cards"], &raw); err != nil {
		return nil, false
	}
	cards := make([]fleetPushCard, 0, len(raw))
	for _, r := range raw {
		var c struct {
			ID     any `json:"id"`
			Kind   any `json:"kind"`
			Ticket any `json:"ticket"`
			Title  any `json:"title"`
		}
		if json.Unmarshal(r, &c) != nil {
			continue
		}
		card := fleetPushCard{ID: fleetPushStr(c.ID), Kind: fleetPushStr(c.Kind), Ticket: fleetPushStr(c.Ticket), Title: fleetPushStr(c.Title)}
		if card.ID != "" {
			cards = append(cards, card)
		}
	}
	return cards, true
}

// fleetPushStr reads a string field the way the page's str() does: a string
// as is, a number as its text, anything else empty.
func fleetPushStr(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	}
	return ""
}

// fleetPushTick is one poll: diff the cards, queue new decisions, and flush
// a batch whose window has passed.
func (s *server) fleetPushTick() {
	st := &s.fleet.push
	now := s.fleetNow()
	if cards, ok := s.fleetPushCards(); ok {
		kinds := make(map[string]string, len(cards))
		latest := make(map[string]fleetPushCard, len(cards))
		for _, c := range cards {
			kinds[c.ID], latest[c.ID] = c.Kind, c
		}
		if !st.baselined {
			st.baselined = true
			st.pushed = map[string]bool{}
		} else if len(s.pushSubsSnapshot()) > 0 {
			for _, c := range fleetPushDiff(st.prev, cards) {
				if st.pushed[c.ID] {
					continue
				}
				st.pushed[c.ID] = true
				st.queue = append(st.queue, c.ID)
				if st.windowAt.IsZero() {
					st.windowAt = now
				}
			}
		}
		for id := range st.pushed {
			if _, ok := kinds[id]; !ok {
				delete(st.pushed, id)
			}
		}
		st.prev, st.cards = kinds, latest
	}
	if !st.windowAt.IsZero() && now.Sub(st.windowAt) >= fleetPushWindow {
		s.fleetPushFlush()
	}
}

// fleetPushPayload is the notification JSON the worker shows.
type fleetPushPayload struct {
	Title  string `json:"title"`
	Body   string `json:"body"`
	Tag    string `json:"tag"`
	CardID string `json:"cardId,omitempty"`
}

func fleetPushText(s string, n int) string {
	return truncateRunes(strings.Join(strings.Fields(s), " "), n)
}

// fleetPushMessage builds the payload for the cards in one batch.
func fleetPushMessage(cards []fleetPushCard) fleetPushPayload {
	if len(cards) == 1 {
		c := cards[0]
		title := fleetPushText(c.Ticket, fleetPushTicketMax)
		if title == "" {
			title = "FLEET"
		}
		body := fleetPushText(c.Title, fleetPushTitleMax)
		if body == "" {
			body = "A decision needs you"
		}
		if len(c.ID) > fleetPushIDMax {
			return fleetPushPayload{Title: title, Body: body, Tag: fleetPushCardTag}
		}
		return fleetPushPayload{Title: title, Body: body, Tag: c.ID, CardID: c.ID}
	}
	return fleetPushPayload{Title: "FLEET", Body: fmt.Sprintf("%d decisions need you", len(cards)), Tag: fleetPushBatchTag}
}

// fleetPushFlush sends the open batch: cards still decisions at send time,
// one notification to every subscription. A card that stopped being a
// decision in the window is dropped and may push again later. 404 and 410
// mean the subscription is gone, so it is removed.
func (s *server) fleetPushFlush() {
	st := &s.fleet.push
	queue := st.queue
	st.queue, st.windowAt = nil, time.Time{}
	var cards []fleetPushCard
	for _, id := range queue {
		if c, ok := st.cards[id]; ok && fleetPushKind(c.Kind) {
			cards = append(cards, c)
		} else {
			delete(st.pushed, id)
		}
	}
	subs := s.pushSubsSnapshot()
	if len(cards) == 0 || len(subs) == 0 {
		return
	}
	payload, err := json.Marshal(fleetPushMessage(cards))
	if err != nil {
		return
	}
	send := s.fleet.pushSend
	if send == nil {
		key, now := s.fleet.vapid, s.fleetNow()
		if key == nil {
			return
		}
		send = func(sub pushSub, p []byte) (int, error) { return webpushSend(webpushClient, key, sub, p, now) }
	}
	what := "batch=" + strconv.Itoa(len(cards))
	if len(cards) == 1 {
		what = "card=" + fleetPushLogID(cards[0].ID)
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	// gone holds the exact subscriptions sent to (endpoint and keys): a
	// device that re-subscribed with new keys during the send is kept.
	gone := map[pushSub]bool{}
	for _, sub := range subs {
		wg.Add(1)
		go func(sub pushSub) {
			defer wg.Done()
			code, err := send(sub, payload)
			host := pushEndpointHost(sub.Endpoint)
			s.logFleetPush(what, host, code, err)
			switch {
			case err != nil:
				// A transport error may be this host's own network: it
				// does not count against the subscription.
				fmt.Fprintf(os.Stderr, "fleet push: send to %s failed: %v\n", host, err)
			case code == http.StatusNotFound || code == http.StatusGone:
				mu.Lock()
				gone[pushSubKey(sub)] = true
				mu.Unlock()
				fmt.Fprintf(os.Stderr, "fleet push: %s answered %d; subscription removed\n", host, code)
			case code == http.StatusOK || code == http.StatusCreated || code == http.StatusAccepted:
				s.pushFailure(sub, false)
			default:
				n := s.pushFailure(sub, true)
				if n >= fleetPushMaxFails {
					mu.Lock()
					gone[pushSubKey(sub)] = true
					mu.Unlock()
					fmt.Fprintf(os.Stderr, "fleet push: %s answered %d, %d failures in a row; subscription removed\n", host, code, n)
				} else {
					fmt.Fprintf(os.Stderr, "fleet push: %s answered %d\n", host, code)
				}
			}
		}(sub)
	}
	wg.Wait()
	if len(gone) > 0 {
		if err := s.removePushSubs(func(sub pushSub) bool { return gone[pushSubKey(sub)] }); err != nil {
			fmt.Fprintf(os.Stderr, "fleet push: cannot save subscriptions: %v\n", err)
		}
	}
}

// pushSubKey is a subscription's identity: endpoint and keys, not the time.
func pushSubKey(sub pushSub) pushSub {
	return pushSub{Endpoint: sub.Endpoint, P256dh: sub.P256dh, Auth: sub.Auth}
}

// pushFailure records one answer for sub: failed counts it, success resets
// it. It returns the consecutive failure count. In memory only, so a restart
// starts every count again.
func (s *server) pushFailure(sub pushSub, failed bool) int {
	f := &s.fleet
	f.pushMu.Lock()
	defer f.pushMu.Unlock()
	k := pushSubKey(sub)
	if !failed {
		delete(f.pushFails, k)
		return 0
	}
	if f.pushFails == nil {
		f.pushFails = map[pushSub]int{}
	}
	f.pushFails[k]++
	return f.pushFails[k]
}

// pushEndpointHost is the only part of an endpoint the log may show: the
// path is the subscription's capability.
// fleetPushLogDefault is fleet-push.log beside fleet-say.log.
func fleetPushLogDefault() string {
	if p := fleetSayLogDefault(); p != "" {
		return filepath.Join(filepath.Dir(p), "fleet-push.log")
	}
	return ""
}

// logFleetPush appends one line per send: time, the card id or batch size,
// the push host and the HTTP status ("error" for a transport failure), and
// nothing else — never the endpoint path, keys, payload or error text. Same
// writer and 1 MiB rotation as fleet-say.log: stderr reaches nobody here.
func (s *server) logFleetPush(what, host string, code int, err error) {
	if s.fleet.pushLogPath == "" {
		return
	}
	status := strconv.Itoa(code)
	if err != nil {
		status = "error"
	}
	appendFleetSayLog(s.fleet.pushLogPath, fmt.Sprintf("%s push %s host=%s status=%s\n",
		s.fleetNow().UTC().Format(time.RFC3339), what, host, status))
}

// fleetPushLogID keeps a card id log-safe: [A-Za-z0-9._:-] only, at most 64
// bytes, "-" when nothing is left.
func fleetPushLogID(id string) string {
	var b strings.Builder
	for i := 0; i < len(id) && b.Len() < 64; i++ {
		c := id[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == ':' || c == '-' {
			b.WriteByte(c)
		}
	}
	if b.Len() == 0 {
		return "-"
	}
	return b.String()
}

func pushEndpointHost(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return "-"
	}
	return u.Host
}

// runFleetPush polls state.json every fleetPushPoll for the life of the
// server. cmdServer starts it only when the VAPID key loaded.
func (s *server) runFleetPush() {
	for {
		func() {
			defer func() {
				if r := recover(); r != nil {
					fmt.Fprintf(os.Stderr, "fleet push: tick failed: %v\n", r)
				}
			}()
			s.fleetPushTick()
		}()
		time.Sleep(fleetPushPoll)
	}
}
