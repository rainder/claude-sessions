package main

import "testing"

func TestCycleSortMode(t *testing.T) {
	cases := []struct {
		mode  string
		delta int
		want  string
	}{
		{"dir", 1, "created"},
		{"updated-asc", 1, "dir"},  // wraps forward
		{"dir", -1, "updated-asc"}, // wraps backward
		{"created-asc", -1, "created"},
		{"bogus", 1, "created"}, // unknown = dir
		{"bogus", -1, "updated-asc"},
	}
	for _, c := range cases {
		if got := cycleSortMode(c.mode, c.delta); got != c.want {
			t.Errorf("cycleSortMode(%q, %d) = %q, want %q", c.mode, c.delta, got, c.want)
		}
	}
}
