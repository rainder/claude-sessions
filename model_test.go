package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestShortModel(t *testing.T) {
	cases := []struct{ in, want string }{
		{"claude-fable-5", "fable-5"},
		{"claude-opus-4-8", "opus-4-8"},
		{"claude-sonnet-4-6", "sonnet-4-6"},
		{"claude-haiku-4-5-20251001", "haiku-4-5"},
		{"claude-fable-5[1m]", "fable-5[1m]"},
		{"", ""},
		{"some-other-model", "some-other-model"},
	}
	for _, c := range cases {
		if got := shortModel(c.in); got != c.want {
			t.Errorf("shortModel(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// writeProjectTranscript creates home/.claude/projects/<dir>/<sid>.jsonl and
// returns its path.
func writeProjectTranscript(t *testing.T, home, dir, sid string) string {
	t.Helper()
	d := filepath.Join(home, ".claude", "projects", dir)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(d, sid+".jsonl")
	if err := os.WriteFile(p, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFindTranscript(t *testing.T) {
	home := t.TempDir()
	// The project dir is keyed to the *startup* cwd, which need not match the
	// session's current cwd (e.g. after entering a git worktree).
	want := writeProjectTranscript(t, home, "-Users-andy-Developer-foo", "sid-find")
	if got := findTranscript(home, "sid-find"); got != want {
		t.Errorf("findTranscript = %q, want %q", got, want)
	}
	if got := findTranscript(home, "sid-absent"); got != "" {
		t.Errorf("missing sid: got %q, want \"\"", got)
	}
	if got := findTranscript(home, ""); got != "" {
		t.Errorf("empty sid: got %q, want \"\"", got)
	}
}

func TestFindTranscriptPicksNewest(t *testing.T) {
	home := t.TempDir()
	old := writeProjectTranscript(t, home, "-proj-a", "sid-dup")
	newer := writeProjectTranscript(t, home, "-proj-b", "sid-dup")
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	if got := findTranscript(home, "sid-dup"); got != newer {
		t.Errorf("findTranscript = %q, want newest %q", got, newer)
	}
}

func TestFindTranscriptReResolvesWhenStale(t *testing.T) {
	home := t.TempDir()
	first := writeProjectTranscript(t, home, "-proj-a", "sid-stale")
	if got := findTranscript(home, "sid-stale"); got != first {
		t.Fatalf("initial resolve = %q, want %q", got, first)
	}
	// Transcript moves (e.g. project dir cleaned up); cached path goes stale.
	second := writeProjectTranscript(t, home, "-proj-b", "sid-stale")
	if err := os.Remove(first); err != nil {
		t.Fatal(err)
	}
	if got := findTranscript(home, "sid-stale"); got != second {
		t.Errorf("after move = %q, want %q", got, second)
	}
}

func writeTranscript(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "s.jsonl")
	var data []byte
	for _, l := range lines {
		data = append(data, l...)
		data = append(data, '\n')
	}
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestModelFromTranscript(t *testing.T) {
	p := writeTranscript(t,
		`{"type":"user","message":{"role":"user","content":"hi"}}`,
		`{"type":"assistant","isSidechain":true,"message":{"role":"assistant","model":"claude-haiku-4-5-20251001"}}`,
		`{"type":"assistant","message":{"role":"assistant","model":"claude-fable-5"}}`,
		`not json at all`,
		`{"type":"assistant","message":{"role":"assistant"}}`,
	)
	if got := modelFromTranscript(p); got != "claude-fable-5" {
		t.Errorf("modelFromTranscript = %q, want %q", got, "claude-fable-5")
	}
}

func TestModelFromTranscriptMissing(t *testing.T) {
	if got := modelFromTranscript(filepath.Join(t.TempDir(), "nope.jsonl")); got != "" {
		t.Errorf("missing transcript: got %q, want \"\"", got)
	}
}

func TestModelFromTranscriptNoAssistant(t *testing.T) {
	p := writeTranscript(t, `{"type":"user","message":{"role":"user","content":"hi"}}`)
	if got := modelFromTranscript(p); got != "" {
		t.Errorf("no assistant entries: got %q, want \"\"", got)
	}
}

func TestRenderModelColumn(t *testing.T) {
	s := Session{PID: 1, Name: "x", CWD: "/tmp", Status: "idle",
		Model: "claude-fable-5", UpdatedAt: time.Now().UnixMilli()}
	for _, view := range []string{"1", "3"} {
		var b strings.Builder
		RenderAll(&b, view, []Session{s}, nil, "", nil)
		out := b.String()
		if !strings.Contains(out, "MODEL") {
			t.Errorf("view %s: missing MODEL header:\n%s", view, out)
		}
		if !strings.Contains(out, "fable-5") {
			t.Errorf("view %s: missing shortened model value:\n%s", view, out)
		}
	}
}

func TestCachedModelInvalidation(t *testing.T) {
	p := writeTranscript(t,
		`{"type":"assistant","message":{"role":"assistant","model":"claude-fable-5"}}`,
	)
	if got := cachedModel(p); got != "claude-fable-5" {
		t.Fatalf("first read = %q, want claude-fable-5", got)
	}
	// Same mtime+size → cache hit even though content scan would still agree.
	if got := cachedModel(p); got != "claude-fable-5" {
		t.Fatalf("cached read = %q, want claude-fable-5", got)
	}
	// Append a model switch and bump mtime → cache must refresh.
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"type":"assistant","message":{"role":"assistant","model":"claude-opus-4-8"}}` + "\n")
	f.Close()
	later := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(p, later, later); err != nil {
		t.Fatal(err)
	}
	if got := cachedModel(p); got != "claude-opus-4-8" {
		t.Errorf("after append = %q, want claude-opus-4-8", got)
	}
}
