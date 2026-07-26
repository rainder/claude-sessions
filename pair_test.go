package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func init() {
	// The exchange route sleeps a second per attempt to blunt guessing. Tests
	// assert the logic, not the delay.
	pairingExchangeDelay = 0
}

func TestPairingCodeRedeemsOnce(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	p := newPairingCode("483920", now.Add(5*time.Minute), func() time.Time { return now })

	if !p.Redeem("483920") {
		t.Fatalf("first redemption failed")
	}
	if p.Redeem("483920") {
		t.Fatalf("code redeemed twice")
	}
}

func TestPairingCodeRejectsWrongAndExpired(t *testing.T) {
	base := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	now := base
	p := newPairingCode("483920", base.Add(5*time.Minute), func() time.Time { return now })

	if p.Redeem("000000") {
		t.Fatalf("accepted the wrong code")
	}
	now = base.Add(6 * time.Minute)
	if p.Redeem("483920") {
		t.Fatalf("accepted an expired code")
	}
}

// Guessing must not be viable against a six-digit code on an unauthenticated
// route: the code dies after a fixed number of attempts.
func TestPairingCodeAttemptCap(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	p := newPairingCode("483920", now.Add(5*time.Minute), func() time.Time { return now })

	for i := 0; i < pairingMaxAttempts; i++ {
		if p.Redeem("000000") {
			t.Fatalf("accepted a wrong code at attempt %d", i)
		}
	}
	if p.Redeem("483920") {
		t.Fatalf("correct code still worked after the attempt cap")
	}
}

func TestPairPayloadShape(t *testing.T) {
	got := pairPayload("100.64.0.1", 8765, "483920", "myserver")
	want := "cs-pair/1 100.64.0.1 8765 483920 myserver"
	if got != want {
		t.Fatalf("pairPayload = %q, want %q", got, want)
	}
}

func TestSixDigitCode(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		got, err := sixDigitCode()
		if err != nil {
			t.Fatalf("sixDigitCode: %v", err)
		}
		if len(got) != 6 {
			t.Fatalf("code = %q, want six characters", got)
		}
		for _, c := range got {
			if c < '0' || c > '9' {
				t.Fatalf("code = %q, want digits only", got)
			}
		}
		seen[got] = true
	}
	if len(seen) < 20 {
		t.Fatalf("only %d distinct codes in 50 draws, want randomness", len(seen))
	}
}

func TestArmPairingRequiresAuth(t *testing.T) {
	s := &server{token: "secret"}
	req := httptest.NewRequest(http.MethodPost, "/pair/arm", strings.NewReader(`{"code":"483920"}`))
	rec := httptest.NewRecorder()

	s.armPairing(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if s.activePairing() != nil {
		t.Fatalf("armed a pairing without auth")
	}
}

func TestArmPairingEnablesExchange(t *testing.T) {
	s := &server{token: "secret", host: "myserver"}

	arm := httptest.NewRequest(http.MethodPost, "/pair/arm", strings.NewReader(`{"code":"483920"}`))
	arm.Header.Set("Authorization", "Bearer secret")
	armRec := httptest.NewRecorder()
	s.armPairing(armRec, arm)
	if armRec.Code != http.StatusNoContent {
		t.Fatalf("arm status = %d, want 204", armRec.Code)
	}

	ex := httptest.NewRequest(http.MethodPost, "/pair/exchange", strings.NewReader(`{"code":"483920"}`))
	exRec := httptest.NewRecorder()
	s.pairExchange(exRec, ex)
	if exRec.Code != http.StatusOK {
		t.Fatalf("exchange status = %d, want 200", exRec.Code)
	}
}

func TestArmPairingRejectsMalformedCode(t *testing.T) {
	s := &server{token: "secret"}
	req := httptest.NewRequest(http.MethodPost, "/pair/arm", strings.NewReader(`{"code":"12"}`))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()

	s.armPairing(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if s.activePairing() != nil {
		t.Fatalf("armed a pairing from a malformed code")
	}
}

func TestPairExchangeReturnsTokenOnce(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	s := &server{
		token:   "the-real-token",
		host:    "myserver",
		pairing: newPairingCode("483920", now.Add(5*time.Minute), func() time.Time { return now }),
	}

	call := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/pair/exchange", strings.NewReader(`{"code":"483920"}`))
		rec := httptest.NewRecorder()
		s.pairExchange(rec, req)
		return rec
	}

	rec := call()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got struct {
		Token  string `json:"token"`
		HostID string `json:"host_id"`
		Name   string `json:"name"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Token != "the-real-token" {
		t.Fatalf("token = %q", got.Token)
	}
	if got.Name != "myserver" {
		t.Fatalf("name = %q", got.Name)
	}

	if second := call(); second.Code == http.StatusOK {
		t.Fatalf("second exchange succeeded, want the code consumed")
	}
}

// With no pairing armed, the route must give away nothing at all — it is the
// only unauthenticated endpoint on the server.
func TestPairExchangeWithoutAnActivePairing(t *testing.T) {
	s := &server{token: "the-real-token"}
	req := httptest.NewRequest(http.MethodPost, "/pair/exchange", strings.NewReader(`{"code":"483920"}`))
	rec := httptest.NewRecorder()

	s.pairExchange(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "the-real-token") {
		t.Fatalf("response leaked the token: %s", rec.Body.String())
	}
}

func TestDisarmPairingClearsTheOffer(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	s := &server{
		token:   "secret",
		pairing: newPairingCode("483920", now.Add(pairingTTL), func() time.Time { return now }),
	}
	req := httptest.NewRequest(http.MethodPost, "/pair/disarm", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()

	s.disarmPairing(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if s.activePairing() != nil {
		t.Fatalf("pairing survived disarm")
	}
}

func TestDisarmPairingRequiresAuth(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	s := &server{
		token:   "secret",
		pairing: newPairingCode("483920", now.Add(pairingTTL), func() time.Time { return now }),
	}
	req := httptest.NewRequest(http.MethodPost, "/pair/disarm", nil)
	rec := httptest.NewRecorder()

	s.disarmPairing(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if s.activePairing() == nil {
		t.Fatalf("unauthenticated disarm cleared the pairing")
	}
}

func TestRenderQRProducesOutput(t *testing.T) {
	var sb strings.Builder
	if err := renderQR(&sb, "cs-pair/1 100.64.0.1 8765 483920 myserver"); err != nil {
		t.Fatalf("renderQR: %v", err)
	}
	out := sb.String()
	if len(out) < 100 {
		t.Fatalf("QR output is %d bytes, want a real block", len(out))
	}
	rows := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(rows) < 10 {
		t.Fatalf("QR has %d rows, want a full symbol", len(rows))
	}
	// Every row must be the same width, or the symbol is malformed.
	width := len([]rune(rows[0]))
	for i, row := range rows {
		if got := len([]rune(row)); got != width {
			t.Fatalf("row %d is %d cells wide, want %d", i, got, width)
		}
	}
}

// Two overlapping `pair` commands must not disarm each other: the first one's
// exit would otherwise kill the second one's live code.
func TestDisarmPairingOnlyClearsAMatchingCode(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	s := &server{
		token:   "secret",
		pairing: newPairingCode("222222", now.Add(pairingTTL), func() time.Time { return now }),
	}

	stale := httptest.NewRequest(http.MethodPost, "/pair/disarm", strings.NewReader(`{"code":"111111"}`))
	stale.Header.Set("Authorization", "Bearer secret")
	staleRec := httptest.NewRecorder()
	s.disarmPairing(staleRec, stale)

	if s.activePairing() == nil {
		t.Fatalf("a stale disarm cleared a different command's live code")
	}

	own := httptest.NewRequest(http.MethodPost, "/pair/disarm", strings.NewReader(`{"code":"222222"}`))
	own.Header.Set("Authorization", "Bearer secret")
	ownRec := httptest.NewRecorder()
	s.disarmPairing(ownRec, own)

	if s.activePairing() != nil {
		t.Fatalf("a matching disarm left the code armed")
	}
}

// serverBaseURL must reject a host that is not answering, which is how cmdPair
// tells a tailscale-bound server from a loopback-bound one.
func TestServerBaseURLProbesForALiveServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	host, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("port: %v", err)
	}

	if got := serverBaseURL(host, p, "secret"); got == "" {
		t.Fatalf("serverBaseURL = %q, want the live server", got)
	}
	if got := serverBaseURL(host, p, "wrong"); got != "" {
		t.Fatalf("serverBaseURL = %q for a bad token, want empty", got)
	}
	if got := serverBaseURL(host, p+1, "secret"); got != "" {
		t.Fatalf("serverBaseURL = %q for a dead port, want empty", got)
	}
}
