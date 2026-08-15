package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseNewArgs(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    newArgs
		wantErr bool
	}{
		{
			name: "dir only",
			args: []string{"--dir", "/tmp/proj"},
			want: newArgs{dir: "/tmp/proj"},
		},
		{
			name: "cwd is a synonym for dir",
			args: []string{"--cwd", "/tmp/proj"},
			want: newArgs{dir: "/tmp/proj"},
		},
		{
			name: "full flag set plus prompt",
			args: []string{"--server", "agent-workstation", "--dir", "~/Developer/trecs-brain", "--command", "fable", "--name", "brain", "some", "initial", "prompt"},
			want: newArgs{dir: "~/Developer/trecs-brain", name: "brain", command: "fable", server: "agent-workstation", prompt: "some initial prompt"},
		},
		{
			name: "prompt before flags still joins",
			args: []string{"hello", "--dir", "/tmp/proj", "world"},
			want: newArgs{dir: "/tmp/proj", prompt: "hello world"},
		},
		{
			name: "group",
			args: []string{"--dir", "/tmp/proj", "--group", "3"},
			want: newArgs{dir: "/tmp/proj", group: 3},
		},
		{
			name: "cmd with flags and prompt after dashdash",
			args: []string{"--dir", "/tmp/proj", "--cmd", "grok", "-m", "grok-4.6", "--reasoning-effort", "high", "--", "DR-1234"},
			want: newArgs{dir: "/tmp/proj", cmd: "grok", cmdArgs: []string{"-m", "grok-4.6", "--reasoning-effort", "high"}, prompt: "DR-1234"},
		},
		{
			name: "cmd plus prompt tokens before cmd",
			args: []string{"hello", "--dir", "/tmp/proj", "--cmd", "claude", "--model=fable", "--", "DR-1"},
			want: newArgs{dir: "/tmp/proj", cmd: "claude", cmdArgs: []string{"--model=fable"}, prompt: "hello DR-1"},
		},
		{
			name:    "cmd and command together",
			args:    []string{"--dir", "/tmp", "--command", "Fable", "--cmd", "claude", "--"},
			wantErr: true,
		},
		{
			name:    "cmd without binary",
			args:    []string{"--dir", "/tmp", "--cmd"},
			wantErr: true,
		},
		{
			name:    "cmd without dashdash",
			args:    []string{"--dir", "/tmp", "--cmd", "grok", "-m", "grok-4.6"},
			wantErr: true,
		},
		{
			name:    "command after cmd",
			args:    []string{"--dir", "/tmp", "--cmd", "grok", "--", "--command", "Fable"},
			wantErr: false, // "--command" after "--" is prompt text
			want:    newArgs{dir: "/tmp", cmd: "grok", prompt: "--command Fable"},
		},
		{
			name:    "missing value for flag",
			args:    []string{"--dir"},
			wantErr: true,
		},
		{
			name:    "missing value for group",
			args:    []string{"--dir", "/tmp", "--group"},
			wantErr: true,
		},
		{
			// 0 is how the store spells "ungrouped"; a spawn asking for it is a
			// typo, not a request, so it is rejected with everything else out
			// of the 1-9 range.
			name:    "group zero",
			args:    []string{"--dir", "/tmp", "--group", "0"},
			wantErr: true,
		},
		{
			name:    "group above the range",
			args:    []string{"--dir", "/tmp", "--group", "10"},
			wantErr: true,
		},
		{
			name:    "negative group",
			args:    []string{"--dir", "/tmp", "--group", "-1"},
			wantErr: true,
		},
		{
			name:    "non-numeric group",
			args:    []string{"--dir", "/tmp", "--group", "abc"},
			wantErr: true,
		},
		{
			name:    "missing value for server",
			args:    []string{"--dir", "/tmp", "--server"},
			wantErr: true,
		},
		{
			name:    "unknown flag",
			args:    []string{"--dir", "/tmp", "--bogus"},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseNewArgs(tt.args)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseNewArgs(%v) error = %v, wantErr %v", tt.args, err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseNewArgs(%v) = %+v, want %+v", tt.args, got, tt.want)
			}
		})
	}
}

// TestListSessionsJSONCarriesGroup drives the real `list-sessions --json` in a
// temp home: a session file for this test process (so the pid is genuinely
// alive) plus a group for its session id in this host's shared flags store.
// The group is host-owned and never read from the session file, so this is the
// only way to prove the overlay reaches the JSON rather than just the table.
func TestListSessionsJSONCarriesGroup(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	const sessionID = "list-json-group-session"

	if err := os.MkdirAll(filepath.Join(home, ".claude", "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	sess := Session{
		PID:        os.Getpid(),
		SessionID:  sessionID,
		CWD:        home,
		Status:     "idle",
		Entrypoint: "cli",
		Group:      9, // must be ignored: the session file never sets a badge
	}
	data, err := json.Marshal(sess)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".claude", "sessions", fmt.Sprintf("%d.json", os.Getpid()))
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	store := newFlagsStore(filepath.Join(home, ".config", "claude-sessions", flagsFileName), time.Now, noResolver)
	if !store.SetGroup(sessionID, 4) {
		t.Fatal("could not record the group under the temp home")
	}

	var rc int
	out := captureStdout(t, func() { rc = cmdListSessions([]string{"--json"}) })
	if rc != 0 {
		t.Fatalf("list-sessions --json exit = %d, want 0", rc)
	}
	var hosts []struct {
		Sessions []Session `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(out), &hosts); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}
	if len(hosts) == 0 {
		t.Fatalf("no hosts in output:\n%s", out)
	}
	found := false
	for _, s := range hosts[0].Sessions {
		if s.SessionID != sessionID {
			continue
		}
		found = true
		if s.Group != 4 {
			t.Errorf("group = %d, want 4 (the store's, not the session file's)", s.Group)
		}
	}
	if !found {
		t.Fatalf("session %s missing from the local host's rows:\n%s", sessionID, out)
	}
}

// TestListSessionsLocalSkipsRemotes: --local is for scripted callers that only
// read this host's rows (the /spawn skill's parent-group lookup), where a
// configured-but-unreachable remote costs a timeout for output nobody reads.
// The remote here refuses instantly, so the non-local half of the comparison
// stays fast while still proving the host is normally there.
func TestListSessionsLocalSkipsRemotes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".config", "claude-sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A configured host with no token: FetchRemote refuses it before opening a
	// connection, so the non-local half of this comparison touches no network
	// and spends no timeout while still producing a real remote row. (A
	// loopback address would not do — dropSelfServer filters out any entry
	// pointing back at this machine.)
	yaml := "servers:\n  - name: nowhere\n    host: nowhere.example\n    port: 8765\n"
	if err := os.WriteFile(filepath.Join(home, ".config", "claude-sessions", "servers.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	hostsIn := func(args []string) []string {
		t.Helper()
		var rc int
		out := captureStdout(t, func() { rc = cmdListSessions(args) })
		if rc != 0 {
			t.Fatalf("list-sessions %v exit = %d", args, rc)
		}
		var hosts []struct {
			Hostname string `json:"hostname"`
		}
		if err := json.Unmarshal([]byte(out), &hosts); err != nil {
			t.Fatalf("output is not valid JSON: %v\n%s", err, out)
		}
		names := make([]string, len(hosts))
		for i, h := range hosts {
			names[i] = h.Hostname
		}
		return names
	}

	full := hostsIn([]string{"--json"})
	if len(full) != 2 || full[1] != "nowhere" {
		t.Fatalf("without --local, hosts = %v, want the local host plus nowhere", full)
	}
	local := hostsIn([]string{"--json", "--local"})
	if len(local) != 1 {
		t.Fatalf("with --local, hosts = %v, want only this host", local)
	}
}

func TestParseKillFlags(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		want    killFlags
		wantErr bool
	}{
		{name: "no flags", args: nil},
		{name: "yes", args: []string{"-y"}, want: killFlags{assumeYes: true}},
		{name: "remove worktree", args: []string{"--remove-worktree"}, want: killFlags{removeWorktree: true}},
		{
			name: "both",
			args: []string{"-y", "--remove-worktree"},
			want: killFlags{assumeYes: true, removeWorktree: true},
		},
		{
			name: "both reversed",
			args: []string{"--remove-worktree", "-y"},
			want: killFlags{assumeYes: true, removeWorktree: true},
		},
		{name: "unknown flag", args: []string{"--force"}, wantErr: true},
		{name: "typo is not silently ignored", args: []string{"-Y"}, wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseKillFlags(c.args)
			if (err != nil) != c.wantErr {
				t.Fatalf("parseKillFlags(%v) error = %v, wantErr %v", c.args, err, c.wantErr)
			}
			if err == nil && got != c.want {
				t.Fatalf("parseKillFlags(%v) = %+v, want %+v", c.args, got, c.want)
			}
		})
	}
}

func TestParseAccountArgs(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantName   string
		wantServer string
		wantForce  bool
		wantYes    bool
		wantErr    bool
	}{
		{name: "name only", args: []string{"avisoma"}, wantName: "avisoma"},
		{name: "name and server", args: []string{"avisoma", "--server", "box"}, wantName: "avisoma", wantServer: "box"},
		{name: "server before name", args: []string{"--server", "box", "avisoma"}, wantName: "avisoma", wantServer: "box"},
		{name: "server only", args: []string{"--server", "box"}, wantServer: "box"},
		{name: "no args", args: nil},
		{name: "server without a value", args: []string{"--server"}, wantErr: true},
		{name: "force", args: []string{"avisoma", "--force"}, wantName: "avisoma", wantForce: true},
		{name: "assume yes", args: []string{"avisoma", "-y"}, wantName: "avisoma", wantYes: true},
		{name: "unknown flag", args: []string{"avisoma", "--nope"}, wantErr: true},
		{name: "second positional", args: []string{"avisoma", "trecs"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseAccountArgs(tt.args)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("err = nil, want an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			want := accountArgs{name: tt.wantName, server: tt.wantServer, force: tt.wantForce, assumeYes: tt.wantYes}
			if got != want {
				t.Fatalf("= %+v, want %+v", got, want)
			}
		})
	}
}

func TestAccountSwitchedLine(t *testing.T) {
	if got := accountSwitchedLine("trecs", "andy@trecs.aero"); !strings.Contains(got, "trecs") || !strings.Contains(got, "andy@trecs.aero") {
		t.Fatalf("line = %q, want the name and the new email", got)
	}
}

func TestCmdSummary(t *testing.T) {
	t.Run("no args prints current backend without changing it", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		if code := cmdSummary(nil); code != 0 {
			t.Fatalf("cmdSummary(nil) = %d, want 0", code)
		}
		if got := LoadSummaryBackend(); got != "claude" {
			t.Errorf("LoadSummaryBackend() after a bare `summary` call = %q, want unchanged %q", got, "claude")
		}
	})

	t.Run("valid backend persists", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		if code := cmdSummary([]string{"codex"}); code != 0 {
			t.Fatalf("cmdSummary([codex]) = %d, want 0", code)
		}
		if got := LoadSummaryBackend(); got != "codex" {
			t.Errorf("LoadSummaryBackend() after `summary codex` = %q, want %q", got, "codex")
		}
	})

	t.Run("unknown backend is rejected and leaves config unchanged", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		if code := cmdSummary([]string{"bogus"}); code != 2 {
			t.Fatalf("cmdSummary([bogus]) = %d, want 2", code)
		}
		if got := LoadSummaryBackend(); got != "claude" {
			t.Errorf("LoadSummaryBackend() after a rejected `summary bogus` = %q, want unchanged %q", got, "claude")
		}
	})

	t.Run("too many args is rejected", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		if code := cmdSummary([]string{"claude", "extra"}); code != 2 {
			t.Fatalf("cmdSummary([claude extra]) = %d, want 2", code)
		}
	})
}

// TestCmdAccountRemove covers the CLI half of fix 4: an unknown name fails
// before touching anything, a non-live snapshot goes without ceremony, and the
// live account's snapshot is refused in a pipeline (nothing there can confirm)
// but removed with -y.
func TestCmdAccountRemove(t *testing.T) {
	t.Run("unknown name", func(t *testing.T) {
		f := newAccountFixture(t)
		f.snapshot("trecs", "tok-t", "andy@trecs.aero")
		f.setLive("tok-live")
		f.setIdentity("andy@avisoma.com")
		if got := cmdAccountRemove([]string{"nope"}); got != 1 {
			t.Fatalf("exit = %d, want 1", got)
		}
		if names, _ := snapshotAccountNames(); len(names) != 1 {
			t.Fatalf("names = %v, want the real snapshot untouched", names)
		}
	})

	t.Run("no name", func(t *testing.T) {
		if got := cmdAccountRemove(nil); got != 2 {
			t.Fatalf("exit = %d, want the usage exit", got)
		}
	})

	t.Run("--server is not a thing", func(t *testing.T) {
		if got := cmdAccountRemove([]string{"trecs", "--server", "box"}); got != 2 {
			t.Fatalf("exit = %d, want the usage exit", got)
		}
	})

	t.Run("a parked snapshot goes", func(t *testing.T) {
		f := newAccountFixture(t)
		f.snapshot("trecs", "tok-t", "andy@trecs.aero")
		f.setLive("tok-live")
		f.setIdentity("andy@avisoma.com")
		if got := cmdAccountRemove([]string{"trecs"}); got != 0 {
			t.Fatalf("exit = %d, want 0", got)
		}
		if names, _ := snapshotAccountNames(); len(names) != 0 {
			t.Fatalf("names = %v, want none left", names)
		}
	})

	t.Run("the live account needs -y when nothing can confirm", func(t *testing.T) {
		f := newAccountFixture(t)
		f.snapshot("avisoma", "tok-a", "andy@avisoma.com")
		f.setLive("tok-a")
		f.setIdentity("andy@avisoma.com")

		// `go test` never has a terminal on stdin, which is exactly the
		// pipeline case: refuse rather than prompt into the void.
		if got := cmdAccountRemove([]string{"avisoma"}); got != 1 {
			t.Fatalf("exit = %d, want a refusal", got)
		}
		if names, _ := snapshotAccountNames(); len(names) != 1 {
			t.Fatalf("names = %v, want the snapshot kept", names)
		}
		if got := cmdAccountRemove([]string{"avisoma", "-y"}); got != 0 {
			t.Fatalf("exit with -y = %d, want 0", got)
		}
		if names, _ := snapshotAccountNames(); len(names) != 0 {
			t.Fatalf("names = %v, want none left", names)
		}
		if got := f.live(); got != credBlob("tok-a") {
			t.Fatalf("live credential = %q, want removing a snapshot to leave the login alone", got)
		}
	})
}

// TestCmdAccountDispatchesRemove proves the subcommand is wired, and that a
// typo still lands on the usage message rather than silently doing nothing.
func TestCmdAccountDispatchesRemove(t *testing.T) {
	f := newAccountFixture(t)
	f.snapshot("trecs", "tok-t", "andy@trecs.aero")
	f.setLive("tok-live")
	f.setIdentity("andy@avisoma.com")

	if got := cmdAccount([]string{"remove", "trecs"}); got != 0 {
		t.Fatalf("exit = %d, want 0", got)
	}
	if names, _ := snapshotAccountNames(); len(names) != 0 {
		t.Fatalf("names = %v, want none left", names)
	}
	if got := cmdAccount([]string{"remve", "trecs"}); got != 2 {
		t.Fatalf("typo exit = %d, want the usage exit", got)
	}
}

// TestCmdAccountSaveFlagSpelling pins finding 5: `save` refuses -y. It never
// prompts, so there is no "yes" to assume — its override reassigns a snapshot to
// another account, which has to be spelled out rather than reached by the
// reflexive flag every other subcommand takes.
func TestCmdAccountSaveFlagSpelling(t *testing.T) {
	f := newAccountFixture(t)
	f.setLive("tok-avisoma")
	f.setIdentity("andy@avisoma.com")
	if err := saveAccountSnapshot("avisoma", false); err != nil {
		t.Fatal(err)
	}
	f.setLive("tok-trecs")
	f.setIdentity("andy@trecs.aero")

	if got := cmdAccountSave([]string{"avisoma", "-y"}); got != 2 {
		t.Fatalf("exit with -y = %d, want the usage exit", got)
	}
	if cred := f.snapshotCred("avisoma"); cred != credBlob("tok-avisoma") {
		t.Fatalf("snapshot = %q, want -y to have changed nothing", cred)
	}
	if got := cmdAccountSave([]string{"avisoma", "--force"}); got != 0 {
		t.Fatalf("exit with --force = %d, want 0", got)
	}
	if cred := f.snapshotCred("avisoma"); cred != credBlob("tok-trecs") {
		t.Fatalf("snapshot = %q, want --force to have reassigned it", cred)
	}
}
