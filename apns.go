package main

import (
	"bytes"
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
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"
)

// errDeviceGone means Apple has told us this token is dead. Callers drop the
// device from the registry rather than retrying.
var errDeviceGone = errors.New("apns: device token no longer valid")

// pushRequest is one notification to one device.
type pushRequest struct {
	DeviceToken string
	Topic       string
	CollapseID  string
	PushType    string // "alert" or "liveactivity"
	Priority    string // "10" for immediate delivery
	Environment string // per-device; empty falls back to the configured default
	Payload     []byte
}

// pushSender is the seam the notify hub talks to, so tests never touch the
// network.
type pushSender interface {
	Send(ctx context.Context, req pushRequest) error
}

// apnsTokenTTL is how long a provider JWT is reused. Apple rejects tokens older
// than an hour and throttles clients that mint a fresh one per request, so the
// window sits comfortably inside both bounds.
const apnsTokenTTL = 50 * time.Minute

// apnsCollapseIDMax is Apple's limit; a longer id is rejected outright.
const apnsCollapseIDMax = 64

// apnsClient signs ES256 provider tokens and posts to Apple over HTTP/2, which
// net/http negotiates automatically over TLS. Hand-rolled deliberately: a JWT
// dependency would be the largest thing in this module's graph, for eighty
// lines of code.
type apnsClient struct {
	cfg     APNsConfig
	key     *ecdsa.PrivateKey
	http    *http.Client
	baseURL string // set only in tests; empty means the real gateway

	mu      sync.Mutex
	token   string
	tokenAt time.Time
	now     func() time.Time
}

func newAPNsClient(cfg APNsConfig) (*apnsClient, error) {
	data, err := os.ReadFile(cfg.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("apns: read key: %w", err)
	}
	return newAPNsClientFromPEM(cfg, data)
}

func newAPNsClientFromPEM(cfg APNsConfig, pemBytes []byte) (*apnsClient, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("apns: key file is not PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("apns: parse key: %w", err)
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("apns: key is %T, want an ECDSA key", parsed)
	}
	// Signing below writes r and s into fixed 32-byte halves, which is only
	// correct for P-256. Apple's .p8 keys always are, but a wrong file would
	// otherwise panic inside FillBytes rather than failing here with a usable
	// message.
	if key.Curve != elliptic.P256() {
		return nil, fmt.Errorf("apns: key curve is %s, want P-256", key.Curve.Params().Name)
	}
	return &apnsClient{
		cfg:  cfg,
		key:  key,
		http: &http.Client{Timeout: 10 * time.Second},
		now:  time.Now,
	}, nil
}

// gateway resolves the APNs host for one device's environment, falling back to
// the host's configured default. One host must be able to serve a production
// TestFlight build and a sandbox debug build at once, which a single global
// setting cannot express.
func (c *apnsClient) gateway(environment string) string {
	if c.baseURL != "" {
		return c.baseURL
	}
	env := environment
	if env == "" {
		env = c.cfg.Environment
	}
	return "https://" + apnsHost(env)
}

// bearer returns a cached provider JWT, minting a new one when it ages out.
func (c *apnsClient) bearer() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if c.token != "" && now.Sub(c.tokenAt) < apnsTokenTTL {
		return c.token, nil
	}
	header, err := json.Marshal(struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}{"ES256", c.cfg.KeyID})
	if err != nil {
		return "", err
	}
	claims, err := json.Marshal(struct {
		Iss string `json:"iss"`
		Iat int64  `json:"iat"`
	}{c.cfg.TeamID, now.Unix()})
	if err != nil {
		return "", err
	}
	signing := base64.RawURLEncoding.EncodeToString(header) + "." +
		base64.RawURLEncoding.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, c.key, sum[:])
	if err != nil {
		return "", fmt.Errorf("apns: sign: %w", err)
	}
	// ES256 wants the raw 32-byte r and s concatenated, not the ASN.1 encoding
	// ecdsa.SignASN1 produces. Apple rejects the latter.
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])

	c.token = signing + "." + base64.RawURLEncoding.EncodeToString(sig)
	c.tokenAt = now
	return c.token, nil
}

// apnsError is Apple's JSON failure body.
type apnsError struct {
	Reason string `json:"reason"`
}

func (c *apnsClient) Send(ctx context.Context, req pushRequest) error {
	tok, err := c.bearer()
	if err != nil {
		return err
	}
	url := c.gateway(req.Environment) + "/3/device/" + req.DeviceToken
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(req.Payload))
	if err != nil {
		return err
	}
	httpReq.Header.Set("authorization", "bearer "+tok)
	httpReq.Header.Set("content-type", "application/json")
	if req.Topic != "" {
		httpReq.Header.Set("apns-topic", req.Topic)
	}
	if req.PushType != "" {
		httpReq.Header.Set("apns-push-type", req.PushType)
	}
	if req.Priority != "" {
		httpReq.Header.Set("apns-priority", req.Priority)
	}
	if req.CollapseID != "" {
		id := req.CollapseID
		if len(id) > apnsCollapseIDMax {
			id = id[:apnsCollapseIDMax]
		}
		httpReq.Header.Set("apns-collapse-id", id)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return fmt.Errorf("apns: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var apiErr apnsError
	_ = json.Unmarshal(body, &apiErr)
	// Only these mean "this token is dead". A 400 for any other reason is a
	// payload or configuration problem, and pruning on it would delete a working
	// registration over a mistake on our side.
	if resp.StatusCode == http.StatusGone || apiErr.Reason == "BadDeviceToken" || apiErr.Reason == "Unregistered" {
		return fmt.Errorf("%w (%s)", errDeviceGone, apiErr.Reason)
	}
	return fmt.Errorf("apns: %d %s", resp.StatusCode, apiErr.Reason)
}
