package main

import (
	"bytes"
	"encoding/json"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testPNG is a real 2×2 PNG, so the sniff sees what a browser would send.
func testPNG(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// postUpload sends body to /fleet/upload with the page's cookie and Origin;
// mod may change the request first.
func postUpload(t *testing.T, s *server, body []byte, mod func(*http.Request)) (int, fleetUploadResult) {
	t.Helper()
	r := httptest.NewRequest("POST", "http://ws.ts.net:8765/fleet/upload", bytes.NewReader(body))
	r.AddCookie(&http.Cookie{Name: "cs_fleet", Value: "sec"})
	r.Header.Set("Origin", "http://ws.ts.net:8765")
	r.Header.Set("Content-Type", "image/png")
	if mod != nil {
		mod(r)
	}
	w := httptest.NewRecorder()
	fleetMux(s).ServeHTTP(w, r)
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	var res fleetUploadResult
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	return w.Code, res
}

func uploadDirFiles(t *testing.T, s *server) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(s.fleet.dir, "uploads"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestFleetUploadNeedsAuth(t *testing.T) {
	s := fleetTestServer(t, nil)
	data := testPNG(t)
	code, res := postUpload(t, s, data, func(r *http.Request) { r.Header.Del("Cookie") })
	if code != http.StatusUnauthorized || res.OK || res.Code != "unauthorized" {
		t.Fatalf("no auth: %d %+v", code, res)
	}
	code, res = postUpload(t, s, data, func(r *http.Request) {
		r.Header.Del("Cookie")
		r.AddCookie(&http.Cookie{Name: "cs_fleet", Value: "wrong"})
	})
	if code != http.StatusUnauthorized || res.OK {
		t.Fatalf("wrong cookie: %d %+v", code, res)
	}
	// A cookie POST from another origin is a CSRF attempt.
	code, res = postUpload(t, s, data, func(r *http.Request) { r.Header.Set("Origin", "http://evil.example") })
	if code != http.StatusForbidden || res.Code != "origin" {
		t.Fatalf("cross origin: %d %+v", code, res)
	}
	code, res = postUpload(t, s, data, func(r *http.Request) { r.Header.Del("Origin") })
	if code != http.StatusForbidden || res.Code != "origin" {
		t.Fatalf("no origin: %d %+v", code, res)
	}
	if got := uploadDirFiles(t, s); len(got) != 0 {
		t.Fatalf("refused uploads stored %v", got)
	}
	// The bearer token needs no Origin.
	code, res = postUpload(t, s, data, func(r *http.Request) {
		r.Header.Del("Cookie")
		r.Header.Del("Origin")
		r.Header.Set("Authorization", "Bearer tok")
	})
	if code != http.StatusOK || !res.OK {
		t.Fatalf("bearer: %d %+v", code, res)
	}
}

func TestFleetUploadStoresUnderARandomName(t *testing.T) {
	s := fleetTestServer(t, nil)
	data := testPNG(t)
	// A client file name, a path in a header or the query reaches nothing.
	code, res := postUpload(t, s, data, func(r *http.Request) {
		r.Header.Set("Content-Disposition", `attachment; filename="../../evil.png"`)
		r.URL.RawQuery = "name=../../evil.png"
	})
	if code != http.StatusOK || !res.OK || res.Code != "" {
		t.Fatalf("%d %+v", code, res)
	}
	dir := filepath.Join(s.fleet.dir, "uploads")
	if !filepath.IsAbs(res.Path) || filepath.Dir(res.Path) != dir {
		t.Fatalf("path %q, want an absolute path in %q", res.Path, dir)
	}
	name := filepath.Base(res.Path)
	if !fleetUploadNameRe.MatchString(name) || !strings.HasSuffix(name, ".png") {
		t.Fatalf("name %q", name)
	}
	got, err := os.ReadFile(res.Path)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("stored bytes differ (%v)", err)
	}
	st, _ := os.Stat(res.Path)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("file mode %v, want 0600", st.Mode().Perm())
	}
	dst, _ := os.Stat(dir)
	if dst.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v, want 0700", dst.Mode().Perm())
	}
	_, res2 := postUpload(t, s, data, nil)
	if !res2.OK || res2.Path == res.Path {
		t.Fatalf("same bytes twice must get two names: %q %q", res.Path, res2.Path)
	}
	if files := uploadDirFiles(t, s); len(files) != 2 {
		t.Fatalf("uploads %v", files)
	}
	if _, err := os.Stat(filepath.Join(s.fleet.dir, "evil.png")); !os.IsNotExist(err) {
		t.Fatal("a client file name reached the disk")
	}
}

func TestFleetUploadSniffsTheType(t *testing.T) {
	s := fleetTestServer(t, nil)
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	var jb, gb bytes.Buffer
	if err := jpeg.Encode(&jb, img, nil); err != nil {
		t.Fatal(err)
	}
	if err := gif.Encode(&gb, img, nil); err != nil {
		t.Fatal(err)
	}
	// A minimal RIFF/WEBP header is what the sniffer reads.
	webp := append([]byte("RIFF\x1a\x00\x00\x00WEBPVP8 "), make([]byte, 14)...)
	for _, c := range []struct {
		data []byte
		ext  string
	}{{jb.Bytes(), ".jpg"}, {gb.Bytes(), ".gif"}, {webp, ".webp"}} {
		// The header says PNG for all of them: the bytes decide.
		code, res := postUpload(t, s, c.data, nil)
		if code != http.StatusOK || !strings.HasSuffix(res.Path, c.ext) {
			t.Errorf("%s: %d %+v", c.ext, code, res)
		}
	}
	// Not an image, whatever the header claims.
	for _, bad := range [][]byte{
		[]byte("%PDF-1.7\n1 0 obj\n"),
		[]byte("<html><script>alert(1)</script></html>"),
		[]byte("just some text"),
		[]byte("<svg xmlns=\"http://www.w3.org/2000/svg\"></svg>"),
	} {
		code, res := postUpload(t, s, bad, nil)
		if code != http.StatusUnsupportedMediaType || res.OK || res.Code != "bad_type" {
			t.Errorf("%q: %d %+v", bad[:8], code, res)
		}
	}
	code, res := postUpload(t, s, nil, nil)
	if code != http.StatusBadRequest || res.Code != "empty" {
		t.Errorf("empty: %d %+v", code, res)
	}
	if files := uploadDirFiles(t, s); len(files) != 3 {
		t.Fatalf("only the three images may be stored: %v", files)
	}
}

func TestFleetUploadSizeCap(t *testing.T) {
	s := fleetTestServer(t, nil)
	big := append(testPNG(t), make([]byte, fleetUploadMax)...)
	code, res := postUpload(t, s, big, nil)
	if code != http.StatusRequestEntityTooLarge || res.Code != "too_large" {
		t.Fatalf("declared length: %d %+v", code, res)
	}
	// No Content-Length (a chunked body): the reader cap still stops it.
	code, res = postUpload(t, s, big, func(r *http.Request) { r.ContentLength = -1 })
	if code != http.StatusRequestEntityTooLarge || res.Code != "too_large" {
		t.Fatalf("chunked: %d %+v", code, res)
	}
	if files := uploadDirFiles(t, s); len(files) != 0 {
		t.Fatalf("an oversized upload was stored: %v", files)
	}
	exact := append(testPNG(t), make([]byte, fleetUploadMax-len(testPNG(t)))...)
	if code, res := postUpload(t, s, exact, nil); code != http.StatusOK || !res.OK {
		t.Fatalf("exactly 10 MB: %d %+v", code, res)
	}
}

func TestFleetUploadPrunesOldFiles(t *testing.T) {
	s := fleetTestServer(t, nil)
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	s.fleet.now = func() time.Time { return now }
	dir := filepath.Join(s.fleet.dir, "uploads")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(name string, age time.Duration) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
		return p
	}
	old := write(strings.Repeat("a", 32)+".png", 8*24*time.Hour)
	fresh := write(strings.Repeat("b", 32)+".jpg", 6*24*time.Hour)
	// Not a name the server writes: never removed, however old.
	foreign := write("notes.txt", 30*24*time.Hour)
	if code, res := postUpload(t, s, testPNG(t), nil); code != http.StatusOK || !res.OK {
		t.Fatalf("%d %+v", code, res)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("an upload older than 7 days must be removed")
	}
	for _, p := range []string{fresh, foreign} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s must stay: %v", filepath.Base(p), err)
		}
	}
	// The start-time prune is the same function.
	now = now.Add(2 * 24 * time.Hour)
	s.pruneFleetUploads(dir)
	if _, err := os.Stat(fresh); !os.IsNotExist(err) {
		t.Error("the prune must remove a file that aged past 7 days")
	}
}

// TestFleetUploadPathReachesTheSession is the page's flow: upload, then send
// the returned path with the text through the normal say routes. The path
// is typed into the pane as it is, so the session can read the image.
func TestFleetUploadPathReachesTheSession(t *testing.T) {
	rig := newSayRig(t, []Session{fleetRow()})
	_, up := postUpload(t, rig.s, testPNG(t), nil)
	if !up.OK {
		t.Fatalf("%+v", up)
	}
	body, _ := json.Marshal(map[string]string{"text": "what is wrong here?\nImage: " + up.Path})
	code, res := rig.post(t, string(body), nil)
	if code != 200 || !res.OK {
		t.Fatalf("%d %+v", code, res)
	}
	if len(rig.typed) != 1 || rig.typed[0] != "what is wrong here? Image: "+up.Path {
		t.Fatalf("typed %q", rig.typed)
	}

	srig, _ := sessionSayRig(t)
	_, up = postUpload(t, srig.s, testPNG(t), nil)
	body, _ = json.Marshal(map[string]string{"target": "spawn-dr-1-fix", "text": "Image: " + up.Path})
	if code, out := srig.postSession(t, string(body), nil); code != 200 || out["ok"] != true {
		t.Fatalf("session say: %d %v", code, out)
	}
	if len(srig.typed) != 1 || srig.typed[0] != "Image: "+up.Path {
		t.Fatalf("session typed %q", srig.typed)
	}
}
