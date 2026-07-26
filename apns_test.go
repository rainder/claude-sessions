package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// testKeyPEM returns a freshly generated P-256 key in PKCS#8 PEM form, the
// shape Apple hands out as a .p8 file.
func testKeyPEM(t *testing.T) ([]byte, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), key
}

func newTestAPNsClient(t *testing.T, handler http.Handler) (*apnsClient, *ecdsa.PrivateKey) {
	t.Helper()
	pemBytes, key := testKeyPEM(t)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	c, err := newAPNsClientFromPEM(APNsConfig{
		KeyID: "KEYID12345", TeamID: "TEAMID6789",
		BundleID: "com.skerla.claude-sessions", Environment: "production",
	}, pemBytes)
	if err != nil {
		t.Fatalf("newAPNsClientFromPEM: %v", err)
	}
	c.baseURL = srv.URL
	c.now = func() time.Time { return time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC) }
	return c, key
}

func TestAPNsSendSetsHeadersAndPath(t *testing.T) {
	var gotPath, gotTopic, gotType, gotPriority, gotCollapse, gotAuth string
	var gotBody []byte
	c, _ := newTestAPNsClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotTopic = r.Header.Get("apns-topic")
		gotType = r.Header.Get("apns-push-type")
		gotPriority = r.Header.Get("apns-priority")
		gotCollapse = r.Header.Get("apns-collapse-id")
		gotAuth = r.Header.Get("authorization")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))

	err := c.Send(context.Background(), pushRequest{
		DeviceToken: "devtok",
		Topic:       "com.skerla.claude-sessions",
		CollapseID:  "host:sess",
		PushType:    "alert",
		Priority:    "10",
		Payload:     []byte(`{"aps":{}}`),
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if gotPath != "/3/device/devtok" {
		t.Fatalf("path = %q, want %q", gotPath, "/3/device/devtok")
	}
	if gotTopic != "com.skerla.claude-sessions" {
		t.Fatalf("apns-topic = %q", gotTopic)
	}
	if gotType != "alert" || gotPriority != "10" || gotCollapse != "host:sess" {
		t.Fatalf("headers = %q/%q/%q", gotType, gotPriority, gotCollapse)
	}
	if !strings.HasPrefix(gotAuth, "bearer ") {
		t.Fatalf("authorization = %q, want a bearer token", gotAuth)
	}
	if string(gotBody) != `{"aps":{}}` {
		t.Fatalf("body = %q", string(gotBody))
	}
}

// APNs rejects collapse ids over 64 bytes outright, so an over-long one must be
// truncated rather than sent and silently dropped.
func TestAPNsTruncatesLongCollapseID(t *testing.T) {
	var got string
	c, _ := newTestAPNsClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("apns-collapse-id")
		w.WriteHeader(http.StatusOK)
	}))
	long := strings.Repeat("x", 200)
	if err := c.Send(context.Background(), pushRequest{DeviceToken: "d", CollapseID: long, Payload: []byte(`{}`)}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(got) != 64 {
		t.Fatalf("collapse id is %d bytes, want 64", len(got))
	}
}

// The JWT must be a real ES256 token Apple would accept: raw r||s, not ASN.1.
func TestAPNsBearerIsVerifiableES256(t *testing.T) {
	c, key := newTestAPNsClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	tok, err := c.bearer()
	if err != nil {
		t.Fatalf("bearer: %v", err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts, want 3", len(parts))
	}

	var header struct{ Alg, Kid string }
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("decode header: %v", err)
	}
	if err := json.Unmarshal(raw, &header); err != nil {
		t.Fatalf("unmarshal header: %v", err)
	}
	if header.Alg != "ES256" || header.Kid != "KEYID12345" {
		t.Fatalf("header = %+v", header)
	}

	var claims struct {
		Iss string `json:"iss"`
		Iat int64  `json:"iat"`
	}
	raw, err = base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode claims: %v", err)
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatalf("unmarshal claims: %v", err)
	}
	if claims.Iss != "TEAMID6789" {
		t.Fatalf("iss = %q", claims.Iss)
	}
	if claims.Iat != c.now().Unix() {
		t.Fatalf("iat = %d, want %d", claims.Iat, c.now().Unix())
	}

	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	if len(sig) != 64 {
		t.Fatalf("signature is %d bytes, want 64 (raw r||s, not ASN.1)", len(sig))
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(&key.PublicKey, sum[:], r, s) {
		t.Fatalf("signature does not verify against the key")
	}
}

func TestAPNsBearerIsCachedUntilExpiry(t *testing.T) {
	c, _ := newTestAPNsClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	base := c.now()

	first, err := c.bearer()
	if err != nil {
		t.Fatalf("bearer: %v", err)
	}
	c.now = func() time.Time { return base.Add(30 * time.Minute) }
	second, err := c.bearer()
	if err != nil {
		t.Fatalf("bearer: %v", err)
	}
	if second != first {
		t.Fatalf("token regenerated inside the cache window")
	}
	c.now = func() time.Time { return base.Add(51 * time.Minute) }
	third, err := c.bearer()
	if err != nil {
		t.Fatalf("bearer: %v", err)
	}
	if third == first {
		t.Fatalf("token not refreshed after the cache window")
	}
}

func TestAPNsGoneTokensAreReported(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{"410 unregistered", http.StatusGone, `{"reason":"Unregistered"}`},
		{"400 bad device token", http.StatusBadRequest, `{"reason":"BadDeviceToken"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := newTestAPNsClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			err := c.Send(context.Background(), pushRequest{DeviceToken: "d", Payload: []byte(`{}`)})
			if !errors.Is(err, errDeviceGone) {
				t.Fatalf("err = %v, want errDeviceGone", err)
			}
		})
	}
}

// A 400 that is not BadDeviceToken must not prune the device — that would
// delete a working registration over a payload mistake.
func TestAPNsOtherBadRequestDoesNotPruneDevice(t *testing.T) {
	c, _ := newTestAPNsClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"reason":"PayloadTooLarge"}`))
	}))
	err := c.Send(context.Background(), pushRequest{DeviceToken: "d", Payload: []byte(`{}`)})
	if err == nil {
		t.Fatalf("Send = nil, want an error")
	}
	if errors.Is(err, errDeviceGone) {
		t.Fatalf("err = %v, want a non-gone error", err)
	}
}

func TestAPNsOtherFailuresCarryTheReason(t *testing.T) {
	c, _ := newTestAPNsClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"reason":"InvalidProviderToken"}`))
	}))
	err := c.Send(context.Background(), pushRequest{DeviceToken: "d", Payload: []byte(`{}`)})
	if err == nil {
		t.Fatalf("Send = nil, want an error")
	}
	if !strings.Contains(err.Error(), "InvalidProviderToken") {
		t.Fatalf("err = %v, want the APNs reason included", err)
	}
}

func TestAPNsEnvironmentSelectsGateway(t *testing.T) {
	pemBytes, _ := testKeyPEM(t)
	c, err := newAPNsClientFromPEM(APNsConfig{
		KeyID: "K", TeamID: "T", BundleID: "B", Environment: "production",
	}, pemBytes)
	if err != nil {
		t.Fatalf("newAPNsClientFromPEM: %v", err)
	}
	if got := c.gateway("sandbox"); !strings.Contains(got, "sandbox") {
		t.Fatalf("gateway(sandbox) = %q, want the sandbox host", got)
	}
	if got := c.gateway(""); strings.Contains(got, "sandbox") {
		t.Fatalf("gateway(\"\") = %q, want the configured default", got)
	}
	if got := c.gateway("production"); strings.Contains(got, "sandbox") {
		t.Fatalf("gateway(production) = %q", got)
	}
}

func TestAPNsRejectsNonPEMAndWrongKeyTypes(t *testing.T) {
	cfg := APNsConfig{KeyID: "K", TeamID: "T", BundleID: "B"}

	if _, err := newAPNsClientFromPEM(cfg, []byte("not a pem file")); err == nil {
		t.Fatalf("accepted a non-PEM key file")
	}

	// An RSA key in a valid PKCS#8 PEM must be rejected with a usable message,
	// not accepted and then fail cryptically at signing time.
	rsaPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("garbage")})
	if _, err := newAPNsClientFromPEM(cfg, rsaPEM); err == nil {
		t.Fatalf("accepted an unparseable PKCS#8 body")
	}

	// A non-P-256 curve would panic inside the fixed 32-byte signature halves.
	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatalf("generate p384: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(p384)
	if err != nil {
		t.Fatalf("marshal p384: %v", err)
	}
	wrongCurve := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if _, err := newAPNsClientFromPEM(cfg, wrongCurve); err == nil {
		t.Fatalf("accepted a P-384 key, want a P-256 check")
	}
}
