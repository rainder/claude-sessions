package main

import (
	"testing"
	"time"
)

func TestUpdatedPrefersUpdatedAt(t *testing.T) {
	s := Session{StartedAt: 1781093160434, UpdatedAt: 1781093170000}
	if got, want := s.Updated(), time.UnixMilli(1781093170000); !got.Equal(want) {
		t.Errorf("Updated() = %v, want %v", got, want)
	}
}

func TestUpdatedFallsBackToStartedAt(t *testing.T) {
	// Headless sessions (entrypoint "sdk-cli") never write updatedAt; their
	// age must come from startedAt, not the epoch.
	s := Session{StartedAt: 1781093160434}
	if got, want := s.Updated(), time.UnixMilli(1781093160434); !got.Equal(want) {
		t.Errorf("Updated() = %v, want %v", got, want)
	}
}

func TestHeadless(t *testing.T) {
	cases := []struct {
		entrypoint string
		want       bool
	}{
		{"cli", false},
		{"", false},
		{"sdk-cli", true},
		{"sdk-ts", true},
	}
	for _, c := range cases {
		s := Session{Entrypoint: c.entrypoint}
		if got := s.Headless(); got != c.want {
			t.Errorf("Headless() with entrypoint %q = %v, want %v", c.entrypoint, got, c.want)
		}
	}
}
