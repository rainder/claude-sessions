package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Device is one registered push target.
//
// No Live Activity tokens here, deliberately. Apple's protocol needs a
// push-to-start token, a separate per-activity update token issued only after
// an activity starts, and a different APNs topic. Modelling one third of that
// now would just be a field to delete later; the Live Activity work adds all
// three together.
type Device struct {
	Token       string `json:"token"`
	Platform    string `json:"platform,omitempty"`
	Environment string `json:"environment,omitempty"` // "production" or "sandbox"
	LastSeen    string `json:"last_seen,omitempty"`   // RFC3339, UTC
}

// deviceFile is the on-disk shape of devices.json.
type deviceFile struct {
	Devices []Device `json:"devices"`
}

// DeviceStore is the set of devices this host pushes to, at
// ~/.config/claude-sessions/devices.json (0600).
//
// It is mutex-guarded: HTTP handlers write it while the notification fan-out
// reads it. And unlike SessionStore, an unreadable file does NOT reset it to
// empty — silently forgetting every device disables notifications with no
// visible symptom, so a parse failure keeps whatever is in memory, refuses to
// overwrite the file, and complains on stderr.
type DeviceStore struct {
	mu       sync.Mutex
	devices  map[string]Device
	path     string
	readOnly bool // set when the existing file failed to parse
	now      func() time.Time
}

// maxRegisteredDevices caps the registry. A handful of phones is the real use;
// the limit exists so nothing can grow it without bound.
const maxRegisteredDevices = 32

// isAPNsDeviceToken reports whether a token looks like one APNs would issue:
// hex, and long enough to be real. Apple's are 64 characters today, but the
// upper bound is generous because that has changed before.
func isAPNsDeviceToken(token string) bool {
	if len(token) < 64 || len(token) > 200 {
		return false
	}
	for _, c := range token {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// LoadDeviceStore reads the registry from the standard config location.
func LoadDeviceStore() *DeviceStore {
	dir := ConfigDir()
	path := ""
	if dir != "" {
		path = filepath.Join(dir, "devices.json")
	}
	return loadDeviceStore(path, time.Now)
}

func loadDeviceStore(path string, now func() time.Time) *DeviceStore {
	s := &DeviceStore{devices: map[string]Device{}, path: path, now: now}
	if path == "" {
		return s
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s
	}
	if err != nil {
		s.readOnly = true
		fmt.Fprintf(os.Stderr, "claude-sessions: cannot read %s: %v — notifications will not persist\n", path, err)
		return s
	}
	var f deviceFile
	if err := json.Unmarshal(data, &f); err != nil {
		s.readOnly = true
		fmt.Fprintf(os.Stderr, "claude-sessions: %s is corrupt (%v) — leaving it untouched; re-pair to rebuild it\n", path, err)
		return s
	}
	for _, d := range f.Devices {
		if d.Token != "" {
			s.devices[d.Token] = d
		}
	}
	return s
}

// Upsert adds or replaces a device, stamping LastSeen.
func (s *DeviceStore) Upsert(d Device) {
	if d.Token == "" {
		return
	}
	s.mu.Lock()
	d.LastSeen = s.clockLocked().UTC().Format(time.RFC3339)
	s.devices[d.Token] = d
	s.mu.Unlock()
	s.saveAndReport()
}

// Remove drops a device. Called on APNs 410 Gone as well as explicit
// deregistration.
func (s *DeviceStore) Remove(token string) {
	s.mu.Lock()
	delete(s.devices, token)
	s.mu.Unlock()
	s.saveAndReport()
}

// List returns every device, sorted by token so callers and tests see a stable
// order.
func (s *DeviceStore) List() []Device {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Device, 0, len(s.devices))
	for _, d := range s.devices {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Token < out[j].Token })
	return out
}

// Has reports whether a token is already registered, so a re-registration is
// not rejected by the size cap.
func (s *DeviceStore) Has(token string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.devices[token]
	return ok
}

// Get returns one registered device. Separate from Has because a caller
// pushing to a single named device needs that device's environment — pushing
// a sandbox token at the production gateway is the exact failure a per-device
// test push exists to reveal, so the stored value must be the one used.
func (s *DeviceStore) Get(token string) (Device, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.devices[token]
	return d, ok
}

// clockLocked reads the injectable clock. Callers hold s.mu.
func (s *DeviceStore) clockLocked() time.Time {
	if s.now == nil {
		return time.Now()
	}
	return s.now()
}

// saveAndReport persists and complains on failure. The in-memory registry is
// still correct after a failed write, so this is not an error the caller can
// act on — but swallowing it entirely would mean a host that silently forgets
// every device on restart, with nothing in the log to explain it.
func (s *DeviceStore) saveAndReport() {
	if err := s.save(); err != nil {
		fmt.Fprintf(os.Stderr, "claude-sessions: cannot persist device registry: %v\n", err)
	}
}

// save writes atomically (temp file + rename in the same directory). It is a
// no-op when there is nowhere to write or when the existing file failed to
// parse.
//
// The lock is held across the whole write, not just the snapshot. Releasing it
// early would let two concurrent saves race on the rename and leave the file
// holding an older snapshot than memory — an in-memory-correct, on-disk-stale
// split that only shows up after a restart. Contention here is irrelevant:
// saves happen on device registration and 410 pruning, not per push.
func (s *DeviceStore) save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.path == "" || s.readOnly {
		return nil
	}
	f := deviceFile{Devices: make([]Device, 0, len(s.devices))}
	for _, d := range s.devices {
		f.Devices = append(f.Devices, d)
	}
	sort.Slice(f.Devices, func(i, j int) bool { return f.Devices[i].Token < f.Devices[j].Token })

	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "devices-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, s.path)
}
