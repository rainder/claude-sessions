package main

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"rsc.io/qr"
)

// pairingTTL and pairingMaxAttempts bound the one unauthenticated route on the
// server. A six-digit code is only safe because it is short-lived, single-use,
// attempt-capped, and exists solely while `claude-sessions pair` is running.
const (
	pairingTTL         = 5 * time.Minute
	pairingMaxAttempts = 10
)

// pairingCode is an in-flight pairing offer. Nil on a server with no `pair`
// running, which is the normal state.
type pairingCode struct {
	mu       sync.Mutex
	code     string
	expires  time.Time
	attempts int
	used     bool
	now      func() time.Time
}

func newPairingCode(code string, expires time.Time, now func() time.Time) *pairingCode {
	if now == nil {
		now = time.Now
	}
	return &pairingCode{code: code, expires: expires, now: now}
}

// Redeem reports whether the supplied code is the live one, consuming it on
// success. Every failure counts against the attempt cap, and exhausting it
// kills the code even for the correct value.
func (p *pairingCode) Redeem(code string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.used || p.attempts >= pairingMaxAttempts || p.now().After(p.expires) {
		return false
	}
	if subtle.ConstantTimeCompare([]byte(code), []byte(p.code)) != 1 {
		p.attempts++
		return false
	}
	p.used = true
	return true
}

// pairPayload is what the QR encodes. Deliberately not a URL and no scheme is
// registered: it is parsed only by the in-app scanner, so there is nothing for
// another app on the device to intercept.
func pairPayload(host string, port int, code, name string) string {
	return "cs-pair/1 " + host + " " + strconv.Itoa(port) + " " + code + " " + name
}

// qrQuietZone is the four-module margin the QR spec requires. Narrowing it to
// save terminal rows makes scanning unreliable against a busy background.
const qrQuietZone = 4

// renderQR draws a QR as unicode half-blocks, two rows per character cell.
func renderQR(w io.Writer, text string) error {
	code, err := qr.Encode(text, qr.M)
	if err != nil {
		return err
	}
	size := code.Size
	for y := -qrQuietZone; y < size+qrQuietZone; y += 2 {
		for x := -qrQuietZone; x < size+qrQuietZone; x++ {
			top := qrCellBlack(code, x, y)
			bottom := qrCellBlack(code, x, y+1)
			var cell string
			switch {
			case top && bottom:
				cell = "█"
			case top:
				cell = "▀"
			case bottom:
				cell = "▄"
			default:
				cell = " "
			}
			if _, err := io.WriteString(w, cell); err != nil {
				return err
			}
		}
		if _, err := io.WriteString(w, "\n"); err != nil {
			return err
		}
	}
	return nil
}

// qrCellBlack reports whether a cell is dark, treating the quiet zone as light.
func qrCellBlack(code *qr.Code, x, y int) bool {
	if x < 0 || y < 0 || x >= code.Size || y >= code.Size {
		return false
	}
	return code.Black(x, y)
}

// sixDigitCode returns a uniformly random six-digit string.
func sixDigitCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1000000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

// armPairing tells the running server to accept one pairing code.
// POST /pair/arm {"code":"483920"} — authenticated, so only something that can
// already read the token off local disk may arm a pairing.
//
// `pair` and `-s` are separate processes: the CLI generates the code and prints
// the QR, but the server is the one that must recognise it. This route is the
// handoff between them.
func (s *server) armPairing(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&body); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if len(body.Code) != 6 {
		http.Error(w, "code must be six digits", http.StatusBadRequest)
		return
	}
	s.pairingMu.Lock()
	s.pairing = newPairingCode(body.Code, time.Now().Add(pairingTTL), time.Now)
	s.pairingMu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// disarmPairing clears the in-flight offer. POST /pair/disarm, authenticated.
// `pair` calls this on every exit path so a code never outlives the command
// that printed it.
func (s *server) disarmPairing(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	s.pairingMu.Lock()
	s.pairing = nil
	s.pairingMu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// activePairing reads the in-flight offer under the lock.
func (s *server) activePairing() *pairingCode {
	s.pairingMu.Lock()
	defer s.pairingMu.Unlock()
	return s.pairing
}

// pairExchange trades a live pairing code for this host's bearer token.
// POST /pair/exchange {"code":"483920"}
//
// This is the only unauthenticated route on the server. It answers nothing
// useful unless a `pair` command is running right now with a matching,
// unexpired, unused code.
func (s *server) pairExchange(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&body); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	// A fixed delay on every attempt, successful or not, keeps this route from
	// being a fast oracle for guessing the code.
	time.Sleep(pairingExchangeDelay)
	p := s.activePairing()
	if p == nil || !p.Redeem(body.Code) {
		http.Error(w, "no active pairing", http.StatusForbidden)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"token":   s.token,
		"host_id": LoadHostID(),
		"name":    s.host,
	})
}

// pairingExchangeDelay is a variable rather than a constant so tests do not pay
// it on every case.
var pairingExchangeDelay = time.Second

// cmdPair generates a pairing code, arms the running server with it, and prints
// the QR.
//
// It does not start a listener: `claude-sessions -s` must already be running on
// this host, and it is that process the phone talks to. The command stays in
// the foreground so the code cannot outlive it.
func cmdPair(args []string) int {
	port := defaultServerPort
	for i := 0; i < len(args); i++ {
		if args[i] == "--port" && i+1 < len(args) {
			if n, err := strconv.Atoi(args[i+1]); err == nil {
				port = n
			}
			i++
		}
	}
	// A QR pointing at 127.0.0.1 is unscannable from a phone. Failing loudly
	// beats printing something that looks like it worked.
	host := tailscaleIPv4()
	if host == "" {
		fmt.Fprintln(os.Stderr, "claude-sessions: no Tailscale IPv4 on this host — the phone has no way to reach it")
		fmt.Fprintln(os.Stderr, "start the server with --bind tailscale, then run pair again")
		return 1
	}
	code, err := sixDigitCode()
	if err != nil {
		fmt.Fprintln(os.Stderr, "claude-sessions:", err)
		return 1
	}
	if err := armLocalPairing(port, code); err != nil {
		fmt.Fprintf(os.Stderr, "claude-sessions: could not arm pairing (%v)\n", err)
		fmt.Fprintln(os.Stderr, "is `claude-sessions -s` running on this host?")
		return 1
	}
	// The code must not outlive this command: the guarantee is that it exists
	// only while `pair` is running. Disarm on every exit path, Ctrl-C included.
	defer func() { _ = disarmLocalPairing(port) }()
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)

	payload := pairPayload(host, port, code, shortHostname())

	fmt.Println()
	if err := renderQR(os.Stdout, payload); err != nil {
		fmt.Fprintln(os.Stderr, "claude-sessions: render QR:", err)
	}
	fmt.Printf("\n  host  %s:%d\n  code  %s\n\n", host, port, code)
	fmt.Println("Scan in the iOS app, or enter the host and code by hand.")
	fmt.Printf("Valid for %s, or until you press Ctrl-C. The bearer token is never in the QR.\n\n", pairingTTL)

	select {
	case <-sigs:
	case <-time.After(pairingTTL):
		fmt.Fprintln(os.Stderr, "pairing code expired")
	}
	return 0
}

// armLocalPairing posts the code to this host's own server over loopback. The
// token comes off local disk, which is the same trust level as running the
// binary at all.
func armLocalPairing(port int, code string) error {
	tok, err := loadOrCreateToken()
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]string{"code": code})
	if err != nil {
		return err
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/pair/arm", port)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("server returned %d", resp.StatusCode)
	}
	return nil
}

// disarmLocalPairing clears any in-flight pairing offer on the running server.
func disarmLocalPairing(port int) error {
	tok, err := loadOrCreateToken()
	if err != nil {
		return err
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/pair/disarm", port)
	req, err := http.NewRequest(http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}
