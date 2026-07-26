package main

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fixedClock lives at state_test.go:12 — same package, so it is reused here
// rather than redefined.

func TestDeviceStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	at := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)

	s := loadDeviceStore(path, fixedClock(at))
	s.Upsert(Device{Token: "bbb", Platform: "ios", Environment: "sandbox"})
	s.Upsert(Device{Token: "aaa", Platform: "ios", Environment: "production"})

	reloaded := loadDeviceStore(path, fixedClock(at))
	got := reloaded.List()
	if len(got) != 2 {
		t.Fatalf("len(devices) = %d, want 2", len(got))
	}
	if got[0].Token != "aaa" || got[1].Token != "bbb" {
		t.Fatalf("List() not sorted by token: %+v", got)
	}
	if got[0].Environment != "production" {
		t.Fatalf("Environment = %q, want %q", got[0].Environment, "production")
	}
	if got[0].LastSeen != at.Format(time.RFC3339) {
		t.Fatalf("LastSeen = %q, want %q", got[0].LastSeen, at.Format(time.RFC3339))
	}
}

func TestDeviceStoreUpsertReplacesAndRefreshesLastSeen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	first := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	s := loadDeviceStore(path, fixedClock(first))
	s.Upsert(Device{Token: "aaa", Environment: "sandbox"})

	later := first.Add(time.Hour)
	s.now = fixedClock(later)
	s.Upsert(Device{Token: "aaa", Environment: "production"})

	got := s.List()
	if len(got) != 1 {
		t.Fatalf("len(devices) = %d, want 1", len(got))
	}
	if got[0].Environment != "production" {
		t.Fatalf("Environment = %q, want %q", got[0].Environment, "production")
	}
	if got[0].LastSeen != later.Format(time.RFC3339) {
		t.Fatalf("LastSeen = %q, want refreshed %q", got[0].LastSeen, later.Format(time.RFC3339))
	}
}

func TestDeviceStoreIgnoresEmptyToken(t *testing.T) {
	s := loadDeviceStore("", fixedClock(time.Now()))
	s.Upsert(Device{Token: "", Environment: "production"})
	if got := s.List(); len(got) != 0 {
		t.Fatalf("devices = %+v, want an empty token ignored", got)
	}
}

func TestDeviceStoreRemove(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	s := loadDeviceStore(path, fixedClock(time.Now()))
	s.Upsert(Device{Token: "aaa"})
	s.Remove("aaa")
	if got := s.List(); len(got) != 0 {
		t.Fatalf("len(devices) = %d after Remove, want 0", len(got))
	}
	if got := loadDeviceStore(path, fixedClock(time.Now())).List(); len(got) != 0 {
		t.Fatalf("removal did not persist: %+v", got)
	}
}

// A registry file we cannot parse must not be silently replaced with an empty
// one — that would disable every phone with no signal.
func TestDeviceStoreCorruptFileIsNotOverwritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	const garbage = "{not json"
	if err := os.WriteFile(path, []byte(garbage), 0o600); err != nil {
		t.Fatalf("seed corrupt file: %v", err)
	}

	s := loadDeviceStore(path, fixedClock(time.Now()))
	s.Upsert(Device{Token: "aaa"})

	if got := s.List(); len(got) != 1 {
		t.Fatalf("in-memory state lost: %+v", got)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != garbage {
		t.Fatalf("corrupt file was overwritten with %q", string(data))
	}
}

func TestDeviceStoreFileMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	s := loadDeviceStore(path, fixedClock(time.Now()))
	s.Upsert(Device{Token: "aaa"})

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("mode = %o, want 600", perm)
	}
}

func TestDeviceStoreConcurrentAccess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	s := loadDeviceStore(path, fixedClock(time.Now()))

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s.Upsert(Device{Token: string(rune('a' + i%10))})
			_ = s.List()
		}(i)
	}
	wg.Wait()

	if got := s.List(); len(got) != 10 {
		t.Fatalf("len(devices) = %d, want 10", len(got))
	}
	// Disk must match memory. Releasing the lock before the rename would let two
	// saves invert and leave the file holding an older snapshot.
	if got := loadDeviceStore(path, fixedClock(time.Now())).List(); len(got) != 10 {
		t.Fatalf("persisted %d devices, want 10 — a save lost an update", len(got))
	}
}

func TestDeviceStoreEmptyPathDoesNotPanic(t *testing.T) {
	s := loadDeviceStore("", fixedClock(time.Now()))
	s.Upsert(Device{Token: "aaa"})
	if got := s.List(); len(got) != 1 {
		t.Fatalf("len(devices) = %d, want 1 held in memory", len(got))
	}
}

func TestDeviceStoreMissingFileStartsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	s := loadDeviceStore(path, fixedClock(time.Now()))
	if got := s.List(); len(got) != 0 {
		t.Fatalf("devices = %+v, want empty", got)
	}
	// And a missing file is not the corrupt case: writes must still persist.
	s.Upsert(Device{Token: "aaa"})
	if got := loadDeviceStore(path, fixedClock(time.Now())).List(); len(got) != 1 {
		t.Fatalf("write after a missing file did not persist: %+v", got)
	}
}
