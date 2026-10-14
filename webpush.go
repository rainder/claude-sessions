package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Web Push from the standard library: RFC 8291 message encryption over the
// RFC 8188 aes128gcm content coding, and RFC 8292 VAPID. One record per
// message, so a payload must fit one 4096-byte record.

const (
	webpushRecordSize = 4096
	// webpushHeaderSize: salt 16 + rs 4 + idlen 1 + a 65-byte key id.
	webpushHeaderSize = 16 + 4 + 1 + 65
	// webpushMaxPlain keeps the whole body (header, plaintext, the 0x02
	// delimiter and the 16-byte GCM tag) within 4096 bytes, the size push
	// services accept.
	webpushMaxPlain = webpushRecordSize - webpushHeaderSize - 1 - 16
	// webpushVAPIDTTL: Apple refuses a VAPID token that lives over 24 h.
	webpushVAPIDTTL = 12 * time.Hour
	// webpushSubject is the VAPID sub claim: a project URL, never an email.
	webpushSubject = "https://github.com/rainder/claude-sessions"
	webpushTimeout = 10 * time.Second
)

// webpushEncrypt encrypts plaintext for one subscription: uaPublic is the
// subscription's p256dh key (65 bytes, uncompressed), authSecret its 16-byte
// auth. A fresh sender key and salt make every message unique.
func webpushEncrypt(uaPublic, authSecret, plaintext []byte) ([]byte, error) {
	asPriv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	return webpushEncryptWith(asPriv, salt, uaPublic, authSecret, plaintext)
}

// webpushEncryptWith is webpushEncrypt with the sender key and salt given,
// so a test can reproduce RFC 8291 Appendix A byte for byte.
func webpushEncryptWith(asPriv *ecdh.PrivateKey, salt, uaPublic, authSecret, plaintext []byte) ([]byte, error) {
	if len(salt) != 16 {
		return nil, errors.New("webpush: salt must be 16 bytes")
	}
	if len(authSecret) != 16 {
		return nil, errors.New("webpush: auth secret must be 16 bytes")
	}
	if len(plaintext) > webpushMaxPlain {
		return nil, errors.New("webpush: payload too large")
	}
	uaKey, err := ecdh.P256().NewPublicKey(uaPublic)
	if err != nil {
		return nil, fmt.Errorf("webpush: bad receiver key: %w", err)
	}
	ecdhSecret, err := asPriv.ECDH(uaKey)
	if err != nil {
		return nil, err
	}
	asPublic := asPriv.PublicKey().Bytes()
	cek, nonce, err := webpushKeys(ecdhSecret, authSecret, uaKey.Bytes(), asPublic, salt)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	// RFC 8188 header: salt | rs (uint32) | idlen | keyid (sender public key).
	var out bytes.Buffer
	out.Write(salt)
	var rs [4]byte
	binary.BigEndian.PutUint32(rs[:], webpushRecordSize)
	out.Write(rs[:])
	out.WriteByte(byte(len(asPublic)))
	out.Write(asPublic)
	// The last (and only) record ends with the 0x02 padding delimiter.
	record := append(append(make([]byte, 0, len(plaintext)+1), plaintext...), 0x02)
	return gcm.Seal(out.Bytes(), nonce, record, nil), nil
}

// webpushKeys derives the content encryption key and nonce (RFC 8291 §3.3,
// §3.4 and RFC 8188 §2.2), shared by the sender and the test receiver.
func webpushKeys(ecdhSecret, authSecret, uaPublic, asPublic, salt []byte) (cek, nonce []byte, err error) {
	prkKey, err := hkdf.Extract(sha256.New, ecdhSecret, authSecret)
	if err != nil {
		return nil, nil, err
	}
	keyInfo := "WebPush: info\x00" + string(uaPublic) + string(asPublic)
	ikm, err := hkdf.Expand(sha256.New, prkKey, keyInfo, 32)
	if err != nil {
		return nil, nil, err
	}
	if cek, err = hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: aes128gcm\x00", 16); err != nil {
		return nil, nil, err
	}
	if nonce, err = hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: nonce\x00", 12); err != nil {
		return nil, nil, err
	}
	return cek, nonce, nil
}

// vapidKeyPath is where the server's VAPID private key lives, beside the
// server token and fleet-secret.
func vapidKeyPath() string {
	dir := ConfigDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "vapid-key")
}

// loadOrCreateVAPIDKey reads the VAPID private key (the P-256 scalar as
// unpadded base64url), creating it on first use. Errors never carry the key.
func loadOrCreateVAPIDKey(path string) (*ecdsa.PrivateKey, error) {
	if path == "" {
		return nil, errors.New("no home directory")
	}
	if data, err := os.ReadFile(path); err == nil {
		raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(strings.TrimSpace(string(data)), "="))
		if err != nil || len(raw) != 32 {
			return nil, fmt.Errorf("%s is not a VAPID key; delete it to regenerate", path)
		}
		key, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), raw)
		if err != nil {
			return nil, fmt.Errorf("%s is not a VAPID key; delete it to regenerate", path)
		}
		return key, nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	raw, err := key.Bytes()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := writeFileAtomic(path, []byte(base64.RawURLEncoding.EncodeToString(raw)+"\n"), 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

// vapidPublicKey is the key as the browser's applicationServerKey wants it:
// the uncompressed point, unpadded base64url.
func vapidPublicKey(key *ecdsa.PrivateKey) (string, error) {
	b, err := key.PublicKey.Bytes()
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// vapidJWT signs the RFC 8292 token for endpoint: ES256, aud = the
// endpoint's origin, exp = now + 12 h, sub = the project URL.
func vapidJWT(key *ecdsa.PrivateKey, endpoint string, now time.Time) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme == "" || u.Hostname() == "" {
		return "", errors.New("webpush: bad endpoint")
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"typ":"JWT","alg":"ES256"}`))
	claims, err := json.Marshal(struct {
		Aud string `json:"aud"`
		Exp int64  `json:"exp"`
		Sub string `json:"sub"`
	}{u.Scheme + "://" + u.Hostname(), now.Add(webpushVAPIDTTL).Unix(), webpushSubject})
	if err != nil {
		return "", err
	}
	signing := header + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		return "", err
	}
	// JWS ES256 wants raw r || s, each left-padded to 32 bytes, not ASN.1.
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// vapidAuthorization is the Authorization header value for endpoint.
func vapidAuthorization(key *ecdsa.PrivateKey, endpoint string, now time.Time) (string, error) {
	jwt, err := vapidJWT(key, endpoint, now)
	if err != nil {
		return "", err
	}
	pub, err := vapidPublicKey(key)
	if err != nil {
		return "", err
	}
	return "vapid t=" + jwt + ", k=" + pub, nil
}

// webpushClient never follows a redirect off the endpoint's own host: the
// host was checked against the push-service allowlist, a redirect target
// was not.
var webpushClient = &http.Client{
	Timeout: webpushTimeout,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 || req.URL.Scheme != "https" || req.URL.Host != via[0].URL.Host {
			return errors.New("webpush: redirect refused")
		}
		return nil
	},
}

// webpushSend encrypts payload for sub and POSTs it to sub's endpoint. It
// returns the push service's status code. An error is never the request URL
// (a *url.Error carries the whole endpoint): only its cause.
func webpushSend(client *http.Client, key *ecdsa.PrivateKey, sub pushSub, payload []byte, now time.Time) (int, error) {
	uaPublic, err := decodePushKey(sub.P256dh)
	if err != nil {
		return 0, errors.New("webpush: bad p256dh")
	}
	auth, err := decodePushKey(sub.Auth)
	if err != nil {
		return 0, errors.New("webpush: bad auth")
	}
	body, err := webpushEncrypt(uaPublic, auth, payload)
	if err != nil {
		return 0, err
	}
	authz, err := vapidAuthorization(key, sub.Endpoint, now)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequest(http.MethodPost, sub.Endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, errors.New("webpush: bad endpoint")
	}
	h := req.Header
	h.Set("Content-Encoding", "aes128gcm")
	h.Set("Content-Type", "application/octet-stream")
	h.Set("TTL", "86400")
	h.Set("Urgency", "high")
	h.Set("Authorization", authz)
	resp, err := client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			return 0, ue.Err
		}
		return 0, err
	}
	// Drain a little so the connection can be reused.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	resp.Body.Close()
	return resp.StatusCode, nil
}

// decodePushKey decodes a subscription key: base64url, padding tolerated.
func decodePushKey(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}
