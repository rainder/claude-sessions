package main

import "testing"

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
