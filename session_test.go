package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDisplayName(t *testing.T) {
	const wtCWD = "/Users/andy/Developer/trecs-brain/.claude/worktrees/extraction-tables-unify"
	cases := []struct {
		name      string
		s         Session
		wantLabel string
		wantDim   bool
	}{
		{"user-set is bright", Session{Name: "my-task", NameSource: "user"}, "my-task", false},
		{"missing source treated as user-set", Session{Name: "my-task"}, "my-task", false},
		{"derived is dimmed", Session{Name: "trecs-brain-84", NameSource: "derived"}, "trecs-brain-84", true},
		{"worktree beats derived", Session{Name: "trecs-brain-84", NameSource: "derived", CWD: wtCWD}, "extraction-tables-unify", true},
		{"tmux never used as fallback", Session{Tmux: "trecs-brain-84:0.1"}, "-", true},
		{"derived beats tmux", Session{Name: "trecs-brain-84", NameSource: "derived", Tmux: "sess:0.0"}, "trecs-brain-84", true},
		{"worktree fallback dimmed", Session{CWD: wtCWD}, "extraction-tables-unify", true},
		{"worktree beats tmux", Session{Tmux: "sess:0.0", CWD: wtCWD}, "extraction-tables-unify", true},
		{"last resort dash", Session{CWD: "/tmp/plain"}, "-", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			label, dimmed := c.s.DisplayName()
			if label != c.wantLabel || dimmed != c.wantDim {
				t.Errorf("DisplayName() = (%q, %v), want (%q, %v)", label, dimmed, c.wantLabel, c.wantDim)
			}
		})
	}
}

func TestWorktreeName(t *testing.T) {
	cases := []struct {
		cwd, want string
	}{
		{"/Users/andy/Developer/trecs-brain/.claude/worktrees/extraction-tables-unify", "extraction-tables-unify"},
		{"/repo/.claude/worktrees/DR-860/sub/dir", "DR-860"},
		{"/repo/.claude/worktrees/", ""},
		{"/repo/not-a-worktree", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := worktreeName(c.cwd); got != c.want {
			t.Errorf("worktreeName(%q) = %q, want %q", c.cwd, got, c.want)
		}
	}
}

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

func TestSortSessions(t *testing.T) {
	// "/alpha" and "/Beta" differ only by case, so dir order is case-insensitive.
	// StartedAt / UpdatedAt are deliberately out of step so each mode reorders
	// the trio differently.
	fixture := []Session{
		{SessionID: "s1", CWD: "/Beta", StartedAt: 100, UpdatedAt: 150},
		{SessionID: "s2", CWD: "/alpha", StartedAt: 300, UpdatedAt: 250},
		{SessionID: "s3", CWD: "/alpha", StartedAt: 200, UpdatedAt: 350},
	}
	cases := []struct {
		mode string
		want []string
	}{
		{"dir", []string{"s2", "s3", "s1"}},         // cwd asc, StartedAt desc tiebreak
		{"created", []string{"s2", "s3", "s1"}},     // StartedAt desc
		{"created-asc", []string{"s1", "s3", "s2"}}, // StartedAt asc
		{"updated", []string{"s3", "s2", "s1"}},     // Updated() desc
		{"updated-asc", []string{"s1", "s2", "s3"}}, // Updated() asc
		{"bogus", []string{"s2", "s3", "s1"}},       // unknown => dir
	}
	for _, c := range cases {
		rows := append([]Session(nil), fixture...)
		SortSessions(rows, c.mode)
		got := make([]string, len(rows))
		for i, s := range rows {
			got[i] = s.SessionID
		}
		if !equalStrings(got, c.want) {
			t.Errorf("SortSessions(%q) = %v, want %v", c.mode, got, c.want)
		}
	}
}

func TestSortSessionsStatus(t *testing.T) {
	rows := []Session{
		{SessionID: "unknown", Status: "paused", UpdatedAt: 900},
		{SessionID: "busy-old", Status: "BUSY", UpdatedAt: 500},
		{SessionID: "idle-old", Status: "idle", UpdatedAt: 100},
		{SessionID: "waiting", Status: "busy", WaitingFor: "permission prompt", UpdatedAt: 50},
		{SessionID: "idle-new", Status: "IDLE", UpdatedAt: 300},
		{SessionID: "busy-new", Status: "busy", UpdatedAt: 700},
		{SessionID: "shell", Status: "shell", UpdatedAt: 200},
		{SessionID: "blank", UpdatedAt: 1000},
	}

	SortSessions(rows, "status")
	got := make([]string, len(rows))
	for i, s := range rows {
		got[i] = s.SessionID
	}
	want := []string{"waiting", "idle-new", "idle-old", "shell", "busy-new", "busy-old", "blank", "unknown"}
	if !equalStrings(got, want) {
		t.Fatalf("status order = %v, want %v", got, want)
	}
}

func TestSortSessionsStatusStable(t *testing.T) {
	rows := []Session{
		{SessionID: "a", Status: "idle", UpdatedAt: 100},
		{SessionID: "b", Status: "IDLE", UpdatedAt: 100},
		{SessionID: "c", Status: "idle", UpdatedAt: 100},
	}
	SortSessions(rows, "status")
	got := []string{rows[0].SessionID, rows[1].SessionID, rows[2].SessionID}
	if want := []string{"a", "b", "c"}; !equalStrings(got, want) {
		t.Fatalf("stable status order = %v, want %v", got, want)
	}
}

func TestSortSessionsStable(t *testing.T) {
	// Rows tied on the sort key must keep their input order.
	rows := []Session{
		{SessionID: "a", StartedAt: 100},
		{SessionID: "b", StartedAt: 100},
		{SessionID: "c", StartedAt: 100},
	}
	SortSessions(rows, "created")
	got := make([]string, len(rows))
	for i, s := range rows {
		got[i] = s.SessionID
	}
	if want := []string{"a", "b", "c"}; !equalStrings(got, want) {
		t.Errorf("SortSessions stability = %v, want %v", got, want)
	}
}

func TestCollectLocalSetsHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".claude", "sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	pid := os.Getpid()
	data, err := json.Marshal(Session{PID: pid, SessionID: "home-test", CWD: "/home/testuser/project", StartedAt: time.Now().UnixMilli()})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, strconv.Itoa(pid)+".json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	rows, err := CollectLocal()
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, r := range rows {
		if r.PID == pid {
			found = true
			if r.Home != home {
				t.Errorf("Home = %q, want %q", r.Home, home)
			}
		}
	}
	if !found {
		t.Fatalf("pid %d not found in CollectLocal() rows", pid)
	}
}

func TestCollectLocalHidesTmpCWD(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".claude", "sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	pid := os.Getpid()
	data, err := json.Marshal(Session{PID: pid, SessionID: "tmp-test", CWD: "/tmp/scratch-123", StartedAt: time.Now().UnixMilli()})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, strconv.Itoa(pid)+".json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	rows, err := CollectLocal()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.PID == pid {
			t.Fatalf("pid %d with cwd /tmp/scratch-123 should be hidden, got row %+v", pid, r)
		}
	}
}

func TestIsScratchCWD(t *testing.T) {
	cases := []struct {
		cwd  string
		want bool
	}{
		{"/tmp", true},
		{"/tmp/foo", true},
		{"/tmpfoo", false},
		{"/private", true},
		{"/private/var/folders", true},
		{"/privateer", false},
		{"/home/andy/project", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isScratchCWD(c.cwd); got != c.want {
			t.Errorf("isScratchCWD(%q) = %v, want %v", c.cwd, got, c.want)
		}
	}
}

func TestSessionHomeJSONCompatibility(t *testing.T) {
	data, err := json.Marshal(Session{Home: "/home/andy"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"home":"/home/andy"`) {
		t.Fatalf("marshaled JSON missing home field: %s", data)
	}
	var old Session
	if err := json.Unmarshal([]byte(`{"pid":1,"cwd":"/home/andy/project"}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.Home != "" {
		t.Errorf("Home = %q, want empty", old.Home)
	}
}

func TestSessionTmuxAttachedJSONCompatibility(t *testing.T) {
	zero := 0
	positive := 3
	cases := []struct {
		name       string
		attached   *int
		wantJSON   string
		absentJSON bool
	}{
		{"unknown omitted", nil, "", true},
		{"detached retained", &zero, `"tmuxAttached":0`, false},
		{"positive retained", &positive, `"tmuxAttached":3`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(Session{Tmux: "dev:0.0", TmuxAttached: tc.attached})
			if err != nil {
				t.Fatal(err)
			}
			if tc.absentJSON {
				if strings.Contains(string(data), "tmuxAttached") {
					t.Fatalf("marshaled JSON unexpectedly contains tmuxAttached: %s", data)
				}
			} else if !strings.Contains(string(data), tc.wantJSON) {
				t.Fatalf("marshaled JSON = %s, want field %s", data, tc.wantJSON)
			}

			var roundTrip Session
			if err := json.Unmarshal(data, &roundTrip); err != nil {
				t.Fatal(err)
			}
			switch {
			case tc.attached == nil && roundTrip.TmuxAttached != nil:
				t.Fatalf("round-trip count = %v, want nil", roundTrip.TmuxAttached)
			case tc.attached != nil && roundTrip.TmuxAttached == nil:
				t.Fatalf("round-trip count = nil, want %d", *tc.attached)
			case tc.attached != nil && *roundTrip.TmuxAttached != *tc.attached:
				t.Fatalf("round-trip count = %d, want %d", *roundTrip.TmuxAttached, *tc.attached)
			}
		})
	}

	var legacy Session
	if err := json.Unmarshal([]byte(`{"pid":1,"tmux":"legacy:0.0"}`), &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.TmuxAttached != nil {
		t.Fatalf("legacy missing field decoded as %v, want nil", legacy.TmuxAttached)
	}

	var detached Session
	if err := json.Unmarshal([]byte(`{"pid":2,"tmux":"dev:0.0","tmuxAttached":0}`), &detached); err != nil {
		t.Fatal(err)
	}
	if detached.TmuxAttached == nil || *detached.TmuxAttached != 0 {
		t.Fatalf("detached count decoded as %v, want pointer to 0", detached.TmuxAttached)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestSessionDisabledJSONCompatibility(t *testing.T) {
	data, err := json.Marshal(Session{PID: 1, Disabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"disabled":true`) {
		t.Fatalf("marshaled JSON missing disabled field: %s", data)
	}

	data, err = json.Marshal(Session{PID: 1})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"disabled"`) {
		t.Fatalf("false disabled field must be omitted: %s", data)
	}

	var old Session
	if err := json.Unmarshal([]byte(`{"pid":1}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.Disabled {
		t.Fatal("missing disabled field decoded as true")
	}
}

func TestReadSessionFileIgnoresPersistedDisabled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json")
	data, err := json.Marshal(Session{
		PID:       1,
		SessionID: "persisted-disabled",
		Disabled:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	session, ok := readSessionFile(path)
	if !ok {
		t.Fatal("session file was not decoded")
	}
	if session.Disabled {
		t.Fatal("persisted disabled state became authoritative")
	}
}

func TestSortSessionsKeepsDisabledRowsLastInEveryMode(t *testing.T) {
	fixture := []Session{
		{SessionID: "disabled-busy", CWD: "/beta", Status: "busy", StartedAt: 200, UpdatedAt: 300, Disabled: true},
		{SessionID: "enabled-busy", CWD: "/beta", Status: "busy", StartedAt: 100, UpdatedAt: 400},
		{SessionID: "disabled-idle", CWD: "/alpha", Status: "idle", StartedAt: 400, UpdatedAt: 200, Disabled: true},
		{SessionID: "enabled-idle", CWD: "/alpha", Status: "idle", StartedAt: 300, UpdatedAt: 100},
	}
	cases := []struct {
		mode string
		want []string
	}{
		{"dir", []string{"enabled-idle", "enabled-busy", "disabled-idle", "disabled-busy"}},
		{"status", []string{"enabled-idle", "enabled-busy", "disabled-idle", "disabled-busy"}},
		{"created", []string{"enabled-idle", "enabled-busy", "disabled-idle", "disabled-busy"}},
		{"created-asc", []string{"enabled-busy", "enabled-idle", "disabled-busy", "disabled-idle"}},
		{"updated", []string{"enabled-busy", "enabled-idle", "disabled-busy", "disabled-idle"}},
		{"updated-asc", []string{"enabled-idle", "enabled-busy", "disabled-idle", "disabled-busy"}},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			rows := append([]Session(nil), fixture...)
			SortSessions(rows, tc.mode)
			got := make([]string, len(rows))
			for i := range rows {
				got[i] = rows[i].SessionID
			}
			if !equalStrings(got, tc.want) {
				t.Fatalf("SortSessions(%q) = %v, want %v", tc.mode, got, tc.want)
			}
		})
	}
}
