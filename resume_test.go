package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeResumableTranscript writes lines (joined with '\n', trailing newline) to
// <home>/.claude/projects/<dir>/<sid>.jsonl and stamps its mtime.
func writeResumableTranscript(t *testing.T, home, dir, sid string, mtime time.Time, lines ...string) string {
	t.Helper()
	pdir := filepath.Join(home, ".claude", "projects", dir)
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(pdir, sid+".jsonl")
	var content string
	if len(lines) > 0 {
		content = strings.Join(lines, "\n") + "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	return path
}

// writeSessionFile writes a live-session JSON blob to
// <home>/.claude/sessions/<pid>.json, the source resumableNameMap reads for
// user-set names.
func writeSessionFile(t *testing.T, home, pid, content string) {
	t.Helper()
	dir := filepath.Join(home, ".claude", "sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, pid+".json"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCollectResumableFiltersAndSorts(t *testing.T) {
	home := t.TempDir()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)

	writeResumableTranscript(t, home, "proj", "aaaa1111", now.Add(-1*time.Hour),
		`{"type":"attachment","cwd":"/home/u/proj","gitBranch":"main"}`,
		`{"type":"user","message":{"role":"user","content":"fix the bug in parser"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"ok"}]}}`,
	)
	// Corrupt first line is skipped; cwd/branch/prompt come from later lines and
	// the array content keeps only the text block.
	writeResumableTranscript(t, home, "proj", "bbbb2222", now.Add(-2*time.Hour),
		`{this is not valid json`,
		`{"type":"attachment","cwd":"/home/u/other","gitBranch":"dev"}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"hello there"},{"type":"tool_result","content":"x"}]}}`,
	)
	// A caveat/command wrapper user entry is skipped in favor of the next one.
	writeResumableTranscript(t, home, "proj", "cccc3333", now.Add(-30*time.Minute),
		`{"type":"attachment","cwd":"/home/u/caveat"}`,
		`{"type":"user","message":{"role":"user","content":"<local-command-caveat>Caveat: blah</local-command-caveat>"}}`,
		`{"type":"user","message":{"role":"user","content":"real first prompt here"}}`,
	)
	// No cwd anywhere → excluded (can't spawn a resume without a working dir).
	writeResumableTranscript(t, home, "proj", "dddd4444", now.Add(-10*time.Minute),
		`{"type":"queue-operation"}`,
		`{"type":"user","message":{"role":"user","content":"no cwd anywhere"}}`,
	)
	// Zero-byte → excluded.
	writeResumableTranscript(t, home, "proj", "eeee5555", now.Add(-5*time.Minute))
	// Older than 30 days → excluded.
	writeResumableTranscript(t, home, "proj", "ffff6666", now.Add(-40*24*time.Hour),
		`{"type":"attachment","cwd":"/home/u/stale"}`,
		`{"type":"user","message":{"role":"user","content":"ancient"}}`,
	)
	// Currently live → excluded.
	writeResumableTranscript(t, home, "proj", "9999live", now.Add(-1*time.Minute),
		`{"type":"attachment","cwd":"/home/u/live"}`,
		`{"type":"user","message":{"role":"user","content":"running now"}}`,
	)
	// Scratch cwds (/tmp, /private) → excluded.
	writeResumableTranscript(t, home, "proj", "aaaatmp1", now.Add(-2*time.Minute),
		`{"type":"attachment","cwd":"/tmp/scratch"}`,
		`{"type":"user","message":{"role":"user","content":"temp work"}}`,
	)
	writeResumableTranscript(t, home, "proj", "aaaapriv", now.Add(-3*time.Minute),
		`{"type":"attachment","cwd":"/private/tmp/claude-501/x"}`,
		`{"type":"user","message":{"role":"user","content":"scratchpad work"}}`,
	)

	got := collectResumableFrom(home, map[string]bool{"9999live": true}, now)

	// Sorted mtime-desc: caveat (-30m), normal (-1h), corrupt (-2h).
	wantIDs := []string{"cccc3333", "aaaa1111", "bbbb2222"}
	if len(got) != len(wantIDs) {
		t.Fatalf("got %d sessions %v, want %d %v", len(got), ids(got), len(wantIDs), wantIDs)
	}
	for i, id := range wantIDs {
		if got[i].SessionID != id {
			t.Fatalf("order[%d] = %q, want %q (all: %v)", i, got[i].SessionID, id, ids(got))
		}
	}

	normal := got[1]
	if normal.CWD != "/home/u/proj" || normal.GitBranch != "main" {
		t.Fatalf("normal cwd/branch = %q/%q", normal.CWD, normal.GitBranch)
	}
	if normal.FirstPrompt != "fix the bug in parser" {
		t.Fatalf("normal prompt = %q", normal.FirstPrompt)
	}
	if normal.MessageCount != 3 {
		t.Fatalf("normal message count = %d, want 3", normal.MessageCount)
	}
	if !normal.ModifiedAt.Equal(now.Add(-1 * time.Hour)) {
		t.Fatalf("normal mtime = %v", normal.ModifiedAt)
	}

	if got[0].FirstPrompt != "real first prompt here" {
		t.Fatalf("caveat prompt = %q, want the non-caveat entry", got[0].FirstPrompt)
	}
	if got[2].FirstPrompt != "hello there" || got[2].GitBranch != "dev" {
		t.Fatalf("corrupt-file parse = %q / %q", got[2].FirstPrompt, got[2].GitBranch)
	}
}

func ids(ss []ResumableSession) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = s.SessionID
	}
	return out
}

func TestCollectResumableDedupesBySessionID(t *testing.T) {
	home := t.TempDir()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)

	// Same session id under two project dirs (a worktree move): newest wins.
	writeResumableTranscript(t, home, "old", "dup12345", now.Add(-3*time.Hour),
		`{"type":"attachment","cwd":"/home/u/old"}`,
		`{"type":"user","message":{"role":"user","content":"old copy"}}`,
	)
	writeResumableTranscript(t, home, "new", "dup12345", now.Add(-1*time.Hour),
		`{"type":"attachment","cwd":"/home/u/new"}`,
		`{"type":"user","message":{"role":"user","content":"new copy"}}`,
	)

	got := collectResumableFrom(home, nil, now)
	if len(got) != 1 {
		t.Fatalf("got %d sessions, want 1 (deduped)", len(got))
	}
	if got[0].CWD != "/home/u/new" || got[0].FirstPrompt != "new copy" {
		t.Fatalf("dedupe kept %q / %q, want the newer transcript", got[0].CWD, got[0].FirstPrompt)
	}
}

func TestCollectResumableName(t *testing.T) {
	home := t.TempDir()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)

	// (a) A still-present session file with a user-set name wins over the
	// transcript summary.
	writeResumableTranscript(t, home, "proj", "namedses", now.Add(-1*time.Hour),
		`{"type":"summary","summary":"summary that should be overridden"}`,
		`{"type":"attachment","cwd":"/home/u/named"}`,
		`{"type":"user","message":{"role":"user","content":"hi"}}`,
	)
	writeSessionFile(t, home, "111",
		`{"pid":111,"sessionId":"namedses","cwd":"/home/u/named","name":"Refactor parser","nameSource":"user"}`)

	// (b) A derived nameSource is ignored (it just echoes the dir); the summary
	// is used instead.
	writeResumableTranscript(t, home, "proj", "derivses", now.Add(-2*time.Hour),
		`{"type":"summary","summary":"Investigate flaky test"}`,
		`{"type":"attachment","cwd":"/home/u/derv"}`,
		`{"type":"user","message":{"role":"user","content":"hi"}}`,
	)
	writeSessionFile(t, home, "222",
		`{"pid":222,"sessionId":"derivses","cwd":"/home/u/derv","name":"derv","nameSource":"derived"}`)

	// (c) No session file, summary present → the summary is the name.
	writeResumableTranscript(t, home, "proj", "summsess", now.Add(-3*time.Hour),
		`{"type":"summary","summary":"Wire up the resume picker"}`,
		`{"type":"attachment","cwd":"/home/u/summ"}`,
		`{"type":"user","message":{"role":"user","content":"hi"}}`,
	)

	// (d) Neither a session file nor a summary → empty name (rendered "-").
	writeResumableTranscript(t, home, "proj", "bareseso", now.Add(-4*time.Hour),
		`{"type":"attachment","cwd":"/home/u/bare"}`,
		`{"type":"user","message":{"role":"user","content":"hi"}}`,
	)

	names := map[string]string{}
	for _, s := range collectResumableFrom(home, nil, now) {
		names[s.SessionID] = s.Name
	}
	if names["namedses"] != "Refactor parser" {
		t.Errorf("user-set name = %q, want %q", names["namedses"], "Refactor parser")
	}
	if names["derivses"] != "Investigate flaky test" {
		t.Errorf("derived name should be ignored in favor of summary; got %q", names["derivses"])
	}
	if names["summsess"] != "Wire up the resume picker" {
		t.Errorf("summary name = %q", names["summsess"])
	}
	if names["bareseso"] != "" {
		t.Errorf("bare name = %q, want empty", names["bareseso"])
	}
}

func TestFirstPromptText(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"string", `"hello world"`, "hello world"},
		{"whitespace collapsed", "\"  a\\n\\tb   c \"", "a b c"},
		{"caveat wrapper skipped", `"<local-command-caveat>x</local-command-caveat>"`, ""},
		{"array text only", `[{"type":"text","text":"pick"},{"type":"tool_result","content":"z"}]`, "pick"},
		{"array joins text blocks", `[{"type":"text","text":"a"},{"type":"text","text":"b"}]`, "a b"},
		{"empty", `""`, ""},
		{"unparseable", `12345`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := firstPromptText([]byte(c.raw)); got != c.want {
				t.Fatalf("firstPromptText(%s) = %q, want %q", c.raw, got, c.want)
			}
		})
	}
}

func TestFirstPromptTruncation(t *testing.T) {
	long := strings.Repeat("x", 80)
	got := cleanPrompt(long)
	if r := []rune(got); len(r) != resumePromptMax {
		t.Fatalf("truncated length = %d, want %d", len(r), resumePromptMax)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("truncated prompt %q missing ellipsis", got)
	}
}

func TestTruncateRunes(t *testing.T) {
	cases := []struct {
		s    string
		n    int
		want string
	}{
		{"hello", 10, "hello"},
		{"hello", 5, "hello"},
		{"hello", 3, "he…"},
		{"hello", 1, "…"},
		{"hello", 0, ""},
		{"héllo", 3, "hé…"}, // rune-safe, no split multibyte
	}
	for _, c := range cases {
		if got := truncateRunes(c.s, c.n); got != c.want {
			t.Errorf("truncateRunes(%q, %d) = %q, want %q", c.s, c.n, got, c.want)
		}
	}
}

func TestFormatAge(t *testing.T) {
	cases := []struct {
		seconds float64
		want    string
	}{
		{-5, "0s"},
		{30, "30s"},
		{90, "1m"},
		{3599, "59m"},
		{3600, "1h"},
		{7200, "2h"},
		{86400, "1d"},
		{3 * 86400, "3d"},
	}
	for _, c := range cases {
		if got := formatAge(c.seconds); got != c.want {
			t.Errorf("formatAge(%v) = %q, want %q", c.seconds, got, c.want)
		}
	}
}

func TestResumeRowsAlignmentAndFilter(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	sessions := []ResumableSession{
		{SessionID: "local001", CWD: "/home/u/proj", GitBranch: "main", Name: "Fix parser", FirstPrompt: "do a thing", MessageCount: 12, ModifiedAt: now.Add(-1 * time.Hour)},
		{SessionID: "remote01", CWD: "/srv/app", FirstPrompt: "", MessageCount: 3, ModifiedAt: now.Add(-2 * time.Hour), Host: "srv"},
	}
	// cols=0 (unknown width) → full layout, every column present.
	lines, header := resumeRows(sessions, "/home/u", "mac", 0, now)
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2", len(lines))
	}

	local := stripSGR(lines[0])
	remote := stripSGR(lines[1])

	if !strings.HasPrefix(local, "1h ") { // age padded to width 3
		t.Errorf("local age prefix = %q", local)
	}
	if !strings.Contains(local, "mac ") { // host padded to width of "HOST" (4)
		t.Errorf("local missing padded host: %q", local)
	}
	if !strings.Contains(local, "Fix parser") { // NAME column
		t.Errorf("local missing name: %q", local)
	}
	if !strings.Contains(local, "~/proj") {
		t.Errorf("local home not collapsed: %q", local)
	}
	if !strings.HasSuffix(local, "do a thing") {
		t.Errorf("local prompt not last column: %q", local)
	}

	if !strings.Contains(remote, "srv ") {
		t.Errorf("remote host label missing: %q", remote)
	}
	if !strings.Contains(remote, "/srv/app") {
		t.Errorf("remote raw path missing: %q", remote)
	}
	if !strings.HasSuffix(remote, "-") { // empty branch and prompt render as "-"
		t.Errorf("remote empty-prompt placeholder missing: %q", remote)
	}

	// Host column padded to the same width in both rows keeps columns aligned.
	if idxLocal, idxRemote := colStart(local, "mac"), colStart(remote, "srv"); idxLocal != idxRemote {
		t.Errorf("host column misaligned: local@%d remote@%d", idxLocal, idxRemote)
	}
	if !strings.Contains(header, "AGE") || !strings.Contains(header, "NAME") || !strings.Contains(header, "PROMPT") {
		t.Errorf("header missing columns: %q", header)
	}

	// Filter integration: case-insensitive substring over the visible row text.
	if _, idx := filterNewPickerLines(lines, "SRV"); len(idx) != 1 || idx[0] != 1 {
		t.Errorf("filter 'SRV' matched %v, want [1]", idx)
	}
	if _, idx := filterNewPickerLines(lines, "thing"); len(idx) != 1 || idx[0] != 0 {
		t.Errorf("filter 'thing' matched %v, want [0]", idx)
	}
}

// colStart returns the rune index where sub first appears in s, or -1.
func colStart(s, sub string) int {
	i := strings.Index(s, sub)
	if i < 0 {
		return -1
	}
	return len([]rune(s[:i]))
}

func TestResumeRowsAutocompact(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	// All-local list: HOST is redundant (every row would repeat this host) and is
	// omitted regardless of width.
	sessions := []ResumableSession{
		{SessionID: "alpha001", CWD: "/home/u/alpha", GitBranch: "main", Name: "Alpha work", FirstPrompt: "do the alpha thing", MessageCount: 5, ModifiedAt: now.Add(-1 * time.Hour)},
		{SessionID: "beta0002", CWD: "/home/u/beta", GitBranch: "develop", Name: "Beta task", FirstPrompt: "beta prompt text here", MessageCount: 12, ModifiedAt: now.Add(-2 * time.Hour)},
	}

	cases := []struct {
		cols       int
		wantBranch bool
		wantMsg    bool
	}{
		{0, true, true},    // unknown width → full layout
		{70, true, true},   // wide → full layout
		{55, true, true},   // tight → only PROMPT shrinks, all columns stay
		{48, true, false},  // #MSG dropped
		{40, false, false}, // BRANCH dropped too
		{30, false, false}, // DIR/NAME shrunk toward floors, columns unchanged
	}
	for _, tc := range cases {
		lines, header := resumeRows(sessions, "/home/u", "mac", tc.cols, now)
		if len(lines) != 2 {
			t.Fatalf("cols=%d: got %d lines, want 2", tc.cols, len(lines))
		}
		// AGE, NAME, DIR always survive; HOST never appears for an all-local list.
		for _, must := range []string{"AGE", "NAME", "DIR"} {
			if !strings.Contains(header, must) {
				t.Errorf("cols=%d: header missing %q: %q", tc.cols, must, header)
			}
		}
		if strings.Contains(header, "HOST") {
			t.Errorf("cols=%d: HOST shown for all-local list: %q", tc.cols, header)
		}
		if got := strings.Contains(header, "BRANCH"); got != tc.wantBranch {
			t.Errorf("cols=%d: BRANCH present=%v, want %v (%q)", tc.cols, got, tc.wantBranch, header)
		}
		if got := strings.Contains(header, "#MSG"); got != tc.wantMsg {
			t.Errorf("cols=%d: #MSG present=%v, want %v (%q)", tc.cols, got, tc.wantMsg, header)
		}
		// Width invariant: neither the header nor any row exceeds cols.
		if tc.cols > 0 {
			if w := len([]rune(header)); w > tc.cols {
				t.Errorf("cols=%d: header width %d exceeds", tc.cols, w)
			}
			for _, ln := range lines {
				if w := len([]rune(stripSGR(ln))); w > tc.cols {
					t.Errorf("cols=%d: row width %d exceeds: %q", tc.cols, w, stripSGR(ln))
				}
			}
		}
		// Header matches rows: the DIR column's left edge is identical in the
		// header and the alpha row, proving equal widths for AGE/NAME to its left.
		row0 := stripSGR(lines[0])
		if hd, rd := colStart(header, "DIR"), colStart(row0, "~/alpha"); hd != rd {
			t.Errorf("cols=%d: DIR column misaligned header@%d row@%d (%q / %q)", tc.cols, hd, rd, header, row0)
		}
	}
}

func TestResumePickerStateFilterFirst(t *testing.T) {
	// Digits and letters are all literal filter text (no quick-select / quit
	// shortcut); Enter confirms, Esc cancels.
	state := resumePickerState{RowCount: 5}
	for _, k := range []string{"2", "d", "q"} {
		if confirm, cancel := state.handle(k); confirm || cancel {
			t.Fatalf("key %q: confirm=%v cancel=%v, want both false", k, confirm, cancel)
		}
	}
	if state.Filter != "2dq" {
		t.Fatalf("filter = %q, want %q", state.Filter, "2dq")
	}
	if state.Row != 0 {
		t.Fatalf("row = %d, want 0 after filter edits", state.Row)
	}
	if confirm, cancel := state.handle("\x7f"); confirm || cancel || state.Filter != "2d" {
		t.Fatalf("backspace: filter %q confirm=%v cancel=%v", state.Filter, confirm, cancel)
	}
	if confirm, _ := state.handle(KeyEnter); !confirm {
		t.Fatalf("Enter did not confirm")
	}
	if _, cancel := state.handle(KeyEsc); !cancel {
		t.Fatalf("Esc did not cancel")
	}
}

func TestResumePickerStateNavWraps(t *testing.T) {
	state := resumePickerState{RowCount: 3}
	state.handle(KeyUp)
	if state.Row != 2 {
		t.Fatalf("up from 0 = %d, want 2 (wrap)", state.Row)
	}
	state.handle(KeyDown)
	if state.Row != 0 {
		t.Fatalf("down from 2 = %d, want 0 (wrap)", state.Row)
	}
}
