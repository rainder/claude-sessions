package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

const (
	// fleetUploadMax caps one image from the page.
	fleetUploadMax = 10 << 20
	// fleetUploadMaxAge: an upload older than this is removed on the next
	// upload and at server start. A session reads the image soon after the
	// send, so a week is plenty.
	fleetUploadMaxAge = 7 * 24 * time.Hour
)

// fleetUploadExt maps the sniffed type onto the stored file's extension.
// Only these four pass: Claude Code and grok read them as images.
var fleetUploadExt = map[string]string{
	"image/png":  ".png",
	"image/jpeg": ".jpg",
	"image/webp": ".webp",
	"image/gif":  ".gif",
}

// fleetUploadNameRe is the only file name the server writes into the uploads
// folder, and the only one the prune removes.
var fleetUploadNameRe = regexp.MustCompile(`^[0-9a-f]{32}\.(png|jpg|webp|gif)$`)

type fleetUploadResult struct {
	OK bool `json:"ok"`
	// Path is the stored file's absolute path, which the page puts into the
	// message it sends: the session reads the image from there.
	Path  string `json:"path,omitempty"`
	Code  string `json:"code,omitempty"`
	Error string `json:"error,omitempty"`
}

// fleetUploadDir is ~/.claude/fleet/uploads (fleet.dir in tests). Every
// session the page types into is a local row, so this host is the host the
// session runs on.
func (s *server) fleetUploadDir() string {
	dir := s.fleetDir()
	if dir == "" {
		return ""
	}
	abs, err := filepath.Abs(filepath.Join(dir, "uploads"))
	if err != nil {
		return ""
	}
	return abs
}

// fleetUpload is POST /fleet/upload: the body is one image's bytes. The type
// is sniffed from the bytes (http.DetectContentType), never taken from a
// header, and the file name is random: nothing from the request reaches the
// path. The answer is {ok, path}; the page sends the path in its message.
func (s *server) fleetUpload(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	refuse := func(status int, code, msg string) {
		writeJSON(w, status, fleetUploadResult{Code: code, Error: msg})
	}
	kind := s.fleetAuth(r)
	if kind == fleetAuthNone {
		refuse(http.StatusUnauthorized, "unauthorized", "Unauthorized")
		return
	}
	if kind == fleetAuthCookie && !sameOrigin(r) {
		refuse(http.StatusForbidden, "origin", "origin mismatch")
		return
	}
	dir := s.fleetUploadDir()
	if dir == "" {
		refuse(http.StatusInternalServerError, "upload_failed", "The server cannot store the image.")
		return
	}
	s.pruneFleetUploads(dir)
	const tooLarge = "The image is larger than 10 MB."
	if r.ContentLength > fleetUploadMax {
		refuse(http.StatusRequestEntityTooLarge, "too_large", tooLarge)
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, fleetUploadMax))
	if err != nil {
		var big *http.MaxBytesError
		if errors.As(err, &big) {
			refuse(http.StatusRequestEntityTooLarge, "too_large", tooLarge)
			return
		}
		refuse(http.StatusBadRequest, "bad_request", "The server cannot read the image.")
		return
	}
	if len(data) == 0 {
		refuse(http.StatusBadRequest, "empty", "The image is empty.")
		return
	}
	ext, ok := fleetUploadExt[http.DetectContentType(data)]
	if !ok {
		refuse(http.StatusUnsupportedMediaType, "bad_type", "Only PNG, JPEG, WebP and GIF images can be sent.")
		return
	}
	path, err := storeFleetUpload(dir, ext, data)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fleet: upload store failed: %v\n", err)
		refuse(http.StatusInternalServerError, "upload_failed", "The server cannot store the image.")
		return
	}
	writeJSON(w, http.StatusOK, fleetUploadResult{OK: true, Path: path})
}

// storeFleetUpload writes data to dir under a random name with ext (dir
// 0700, file 0600, O_EXCL so it never replaces a file) and returns its path.
// A failed write removes the partial file.
func storeFleetUpload(dir, ext string, data []byte) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	path := filepath.Join(dir, hex.EncodeToString(b[:])+ext)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}

// pruneFleetUploads removes uploads older than fleetUploadMaxAge from dir:
// regular files with a name the server writes (fleetUploadNameRe) only, so
// nothing else that lands in the folder is touched. Errors are dropped.
func (s *server) pruneFleetUploads(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := s.fleetNow().Add(-fleetUploadMaxAge)
	for _, e := range entries {
		if !e.Type().IsRegular() || !fleetUploadNameRe.MatchString(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
}
