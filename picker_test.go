package main

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestHiddenCwd(t *testing.T) {
	cases := []struct {
		cwd  string
		want bool
	}{
		{"/private/tmp/claude-501/scratchpad", true},
		{"/private/var/folders/xy/T/tmp123", true},
		{"/private", true},
		{"/Users/andy/Developer/claude-sessions", false},
		{"/privateer/repo", false},
		{"", false},
	}
	for _, c := range cases {
		if got := hiddenCwd(c.cwd); got != c.want {
			t.Errorf("hiddenCwd(%q) = %v, want %v", c.cwd, got, c.want)
		}
	}
}

func TestCollectCwdSuggestionsFiltersAndRanks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	high := filepath.Join(home, "high")
	low := filepath.Join(home, "low")
	missing := filepath.Join(home, "missing")
	if err := os.MkdirAll(high, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(low, 0o755); err != nil {
		t.Fatal(err)
	}
	sessions := filepath.Join(home, ".claude", "sessions")
	if err := os.MkdirAll(sessions, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, cwd string, pid int) {
		t.Helper()
		data := fmt.Sprintf(`{"pid":%d,"cwd":%q}`, pid, cwd)
		if err := os.WriteFile(filepath.Join(sessions, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("1.json", high, 1)
	write("2.json", high, 2)
	write("3.json", low, 3)
	write("4.json", missing, 4)

	got := collectCwdSuggestions()
	want := []cwdSuggestion{{CWD: high, Count: 2}, {CWD: low, Count: 1}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("suggestions = %#v, want %#v", got, want)
	}
}

func TestCollapseHome(t *testing.T) {
	cases := []struct {
		path string
		home string
		want string
	}{
		{"/home/bob/repo", "/home/bob", "~/repo"},
		{"/home/bob", "/home/bob", "~"},
		{"/srv/data", "/home/bob", "/srv/data"},
		{"/home/bob/repo", "", "/home/bob/repo"},
		{"", "/home/bob", ""},
		{"/home/bobby/repo", "/home/bob", "~by/repo"},
	}
	for _, c := range cases {
		if got := collapseHome(c.path, c.home); got != c.want {
			t.Errorf("collapseHome(%q, %q) = %q, want %q", c.path, c.home, got, c.want)
		}
	}
}

func TestMergeRemoteCwdEntries(t *testing.T) {
	suggestions := []cwdSuggestion{{CWD: "/a", Count: 3}, {CWD: "/b", Count: 2}}
	got := mergeRemoteCwdEntries("/b", suggestions)
	want := []cwdEntry{{cwd: "/a", count: 3}, {cwd: "/b", count: 2}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merged entries = %#v, want %#v", got, want)
	}
}
