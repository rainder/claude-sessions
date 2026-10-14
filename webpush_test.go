package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func b64u(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(strings.Join(strings.Fields(s), ""))
	if err != nil {
		t.Fatalf("bad base64url %q: %v", s, err)
	}
	return b
}

// webpushDecryptForTest is the receiver side of RFC 8291 (one record).
func webpushDecryptForTest(uaPriv *ecdh.PrivateKey, authSecret, msg []byte) ([]byte, error) {
	if len(msg) < 21 {
		return nil, errors.New("short")
	}
	salt := msg[:16]
	rs := binary.BigEndian.Uint32(msg[16:20])
	idlen := int(msg[20])
	if len(msg) < 21+idlen {
		return nil, errors.New("short keyid")
	}
	asPublic := msg[21 : 21+idlen]
	ct := msg[21+idlen:]
	if uint32(len(ct)) > rs {
		return nil, errors.New("record larger than rs")
	}
	asKey, err := ecdh.P256().NewPublicKey(asPublic)
	if err != nil {
		return nil, err
	}
	secret, err := uaPriv.ECDH(asKey)
	if err != nil {
		return nil, err
	}
	cek, nonce, err := webpushKeys(secret, authSecret, uaPriv.PublicKey().Bytes(), asPublic, salt)
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
	plain, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, err
	}
	plain = bytes.TrimRight(plain, "\x00")
	if len(plain) == 0 || plain[len(plain)-1] != 0x02 {
		return nil, errors.New("no last-record delimiter")
	}
	return plain[:len(plain)-1], nil
}

// TestWebpushRFC8291AppendixA reproduces RFC 8291 Section 5 / Appendix A
// byte for byte. The vectors were copied from the RFC text fetched from
// rfc-editor.org (rfc8291.txt).
func TestWebpushRFC8291AppendixA(t *testing.T) {
	plaintext := b64u(t, "V2hlbiBJIGdyb3cgdXAsIEkgd2FudCB0byBiZSBhIHdhdGVybWVsb24")
	if string(plaintext) != "When I grow up, I want to be a watermelon" {
		t.Fatalf("plaintext vector = %q", plaintext)
	}
	asPriv, err := ecdh.P256().NewPrivateKey(b64u(t, "yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw"))
	if err != nil {
		t.Fatal(err)
	}
	wantAS := b64u(t, "BP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIg Dll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A8")
	if !bytes.Equal(asPriv.PublicKey().Bytes(), wantAS) {
		t.Fatal("as_public does not match as_private")
	}
	uaPriv, err := ecdh.P256().NewPrivateKey(b64u(t, "q1dXpw3UpT5VOmu_cf_v6ih07Aems3njxI-JWgLcM94"))
	if err != nil {
		t.Fatal(err)
	}
	uaPublic := b64u(t, "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcx aOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4")
	if !bytes.Equal(uaPriv.PublicKey().Bytes(), uaPublic) {
		t.Fatal("ua_public does not match ua_private")
	}
	salt := b64u(t, "DGv6ra1nlYgDCS1FRnbzlw")
	auth := b64u(t, "BTBZMqHH6r4Tts7J_aSIgg")

	// Intermediate values: CEK and NONCE.
	secret, err := asPriv.ECDH(mustECDHPub(t, uaPublic))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(secret, b64u(t, "kyrL1jIIOHEzg3sM2ZWRHDRB62YACZhhSlknJ672kSs")) {
		t.Fatal("ecdh_secret differs from the RFC")
	}
	cek, nonce, err := webpushKeys(secret, auth, uaPublic, wantAS, salt)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(cek, b64u(t, "oIhVW04MRdy2XN9CiKLxTg")) {
		t.Fatalf("CEK = %s", base64.RawURLEncoding.EncodeToString(cek))
	}
	if !bytes.Equal(nonce, b64u(t, "4h_95klXJ5E_qnoN")) {
		t.Fatalf("NONCE = %s", base64.RawURLEncoding.EncodeToString(nonce))
	}

	got, err := webpushEncryptWith(asPriv, salt, uaPublic, auth, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	want := b64u(t, `DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27ml
		mlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A_yl95bQpu6cVPT
		pK4Mqgkf1CXztLVBSt2Ks3oZwbuwXPXLWyouBWLVWGNWQexSgSxsj_Qulcy4a-fN`)
	// Section 5's "Content-Length: 145" is off by one: the 86-byte header
	// plus 41 + 1 + 16 bytes of record is 144, and Appendix A's header and
	// ciphertext concatenate to exactly this body.
	appendix := append(b64u(t, `DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z 9KsN6nGRTbVYI_c7VJSPQTBtkgcy27ml
		mlMoZIIgDll6e3vCYLocInmYWAmS6Tlz AC8wEqKK6PBru3jl7A8`),
		b64u(t, `8pfeW0KbunFT06SuDKoJH9Ql87S1QUrd irN6GcG7sFz1y1sqLgVi1VhjVkHsUoEs bI_0LpXMuGvnzQ`)...)
	if len(want) != 144 || !bytes.Equal(want, appendix) {
		t.Fatalf("vector length = %d; Section 5 and Appendix A disagree", len(want))
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("message differs from RFC 8291:\n got %s\nwant %s",
			base64.RawURLEncoding.EncodeToString(got), base64.RawURLEncoding.EncodeToString(want))
	}
	back, err := webpushDecryptForTest(uaPriv, auth, got)
	if err != nil || !bytes.Equal(back, plaintext) {
		t.Fatalf("decrypt = %q, %v", back, err)
	}
}

func mustECDHPub(t *testing.T, b []byte) *ecdh.PublicKey {
	t.Helper()
	k, err := ecdh.P256().NewPublicKey(b)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestWebpushRoundTrip(t *testing.T) {
	uaPriv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	_, _ = rand.Read(auth)
	for _, msg := range []string{"", `{"title":"DR-1","body":"x"}`, strings.Repeat("ž", 1000)} {
		ct, err := webpushEncrypt(uaPriv.PublicKey().Bytes(), auth, []byte(msg))
		if err != nil {
			t.Fatal(err)
		}
		got, err := webpushDecryptForTest(uaPriv, auth, ct)
		if err != nil || string(got) != msg {
			t.Fatalf("round trip of %d bytes: %v", len(msg), err)
		}
		// Two encryptions of one message differ (fresh key and salt).
		ct2, _ := webpushEncrypt(uaPriv.PublicKey().Bytes(), auth, []byte(msg))
		if bytes.Equal(ct, ct2) {
			t.Fatal("two encryptions are identical")
		}
	}
	// The largest payload makes a body of exactly 4096 bytes.
	ct, err := webpushEncrypt(uaPriv.PublicKey().Bytes(), auth, make([]byte, webpushMaxPlain))
	if err != nil || len(ct) != 4096 {
		t.Fatalf("max payload body = %d bytes, %v", len(ct), err)
	}
	if got, err := webpushDecryptForTest(uaPriv, auth, ct); err != nil || len(got) != webpushMaxPlain {
		t.Fatalf("max payload round trip: %v", err)
	}
	if _, err := webpushEncrypt(uaPriv.PublicKey().Bytes(), auth, make([]byte, webpushMaxPlain+1)); err == nil {
		t.Fatal("a payload over one record must be refused")
	}
	if _, err := webpushEncrypt([]byte("short"), auth, []byte("x")); err == nil {
		t.Fatal("a bad receiver key must be refused")
	}
}

func TestVAPIDJWT(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_000_000, 0)
	jwt, err := vapidJWT(key, "https://web.push.apple.com/QGuQyavXutnMH:abc?x=1", now)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt has %d parts", len(parts))
	}
	var hdr map[string]string
	if err := json.Unmarshal(b64u(t, parts[0]), &hdr); err != nil || hdr["typ"] != "JWT" || hdr["alg"] != "ES256" || len(hdr) != 2 {
		t.Fatalf("header = %v (%v)", hdr, err)
	}
	var claims map[string]any
	if err := json.Unmarshal(b64u(t, parts[1]), &claims); err != nil {
		t.Fatal(err)
	}
	if claims["aud"] != "https://web.push.apple.com" {
		t.Errorf("aud = %v", claims["aud"])
	}
	if exp, _ := claims["exp"].(float64); int64(exp) != now.Add(12*time.Hour).Unix() {
		t.Errorf("exp = %v", claims["exp"])
	}
	if claims["sub"] != "https://github.com/rainder/claude-sessions" {
		t.Errorf("sub = %v", claims["sub"])
	}
	sig := b64u(t, parts[2])
	if len(sig) != 64 {
		t.Fatalf("signature is %d bytes, want raw r||s 64", len(sig))
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	r, s := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(&key.PublicKey, digest[:], r, s) {
		t.Fatal("signature does not verify with the public key")
	}
	// aud never carries a port.
	jwtPort, err := vapidJWT(key, "https://push.example.net:8443/x", now)
	if err != nil {
		t.Fatal(err)
	}
	var pc map[string]any
	_ = json.Unmarshal(b64u(t, strings.Split(jwtPort, ".")[1]), &pc)
	if pc["aud"] != "https://push.example.net" {
		t.Errorf("aud with a port = %v", pc["aud"])
	}
	authz, err := vapidAuthorization(key, "https://fcm.googleapis.com/fcm/send/x", now)
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := vapidPublicKey(key)
	if !strings.HasPrefix(authz, "vapid t=") || !strings.HasSuffix(authz, ", k="+pub) {
		t.Fatalf("authorization = %q", authz)
	}
	if len(b64u(t, pub)) != 65 || b64u(t, pub)[0] != 4 {
		t.Fatal("public key must be the 65-byte uncompressed point")
	}
}

func TestLoadOrCreateVAPIDKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cfg", "vapid-key")
	k1, err := loadOrCreateVAPIDKey(path)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode = %v, %v", st.Mode(), err)
	}
	if dst, err := os.Stat(filepath.Dir(path)); err != nil || dst.Mode().Perm() != 0o700 {
		t.Fatalf("key dir mode = %v, %v", dst.Mode(), err)
	}
	k2, err := loadOrCreateVAPIDKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if !k1.Equal(k2) {
		t.Fatal("a second load must return the same key")
	}
	if err := os.WriteFile(path, []byte("not a key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrCreateVAPIDKey(path); err == nil {
		t.Fatal("a bad key file must be an error, not a silent new key")
	} else if strings.Contains(err.Error(), "not a key") {
		t.Fatal("the error must not echo the file content")
	}
	if _, err := loadOrCreateVAPIDKey(""); err == nil {
		t.Fatal("no path must be an error")
	}
}

func TestWebpushSend(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	uaPriv, _ := ecdh.P256().GenerateKey(rand.Reader)
	auth := make([]byte, 16)
	_, _ = rand.Read(auth)
	var gotHdr http.Header
	var gotBody []byte
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "https://example.invalid/elsewhere", http.StatusTemporaryRedirect)
			return
		}
		gotHdr = r.Header.Clone()
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	client := &http.Client{
		Timeout:       webpushClient.Timeout,
		CheckRedirect: webpushClient.CheckRedirect,
		Transport:     &http.Transport{TLSClientConfig: &tls.Config{RootCAs: srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs}},
	}
	sub := pushSub{Endpoint: srv.URL + "/push/abc", P256dh: base64.RawURLEncoding.EncodeToString(uaPriv.PublicKey().Bytes()),
		Auth: base64.URLEncoding.EncodeToString(auth)} // padded: tolerated
	code, err := webpushSend(client, key, sub, []byte(`{"title":"t"}`), time.Now())
	if err != nil || code != http.StatusCreated {
		t.Fatalf("send = %d, %v", code, err)
	}
	for k, v := range map[string]string{"Content-Encoding": "aes128gcm", "Content-Type": "application/octet-stream", "Ttl": "86400", "Urgency": "high"} {
		if gotHdr.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, gotHdr.Get(k), v)
		}
	}
	if !strings.HasPrefix(gotHdr.Get("Authorization"), "vapid t=") {
		t.Errorf("Authorization = %q", gotHdr.Get("Authorization"))
	}
	plain, err := webpushDecryptForTest(uaPriv, auth, gotBody)
	if err != nil || string(plain) != `{"title":"t"}` {
		t.Fatalf("pushed body decrypts to %q, %v", plain, err)
	}

	sub.Endpoint = srv.URL + "/redirect"
	if _, err := webpushSend(client, key, sub, []byte("x"), time.Now()); err == nil {
		t.Fatal("a redirect to another host must fail")
	} else if strings.Contains(err.Error(), "/redirect") || strings.Contains(err.Error(), srv.URL) {
		t.Fatalf("the error must not carry the endpoint URL: %v", err)
	}
}
