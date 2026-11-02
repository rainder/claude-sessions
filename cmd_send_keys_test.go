// cmd_send_keys_test.go
package main

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestParseSendKeysArgs(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want sendKeysArgs
	}{
		{"local pid", []string{"123", "hi"}, sendKeysArgs{target: "123", text: []string{"hi"}}},
		{"tmux name", []string{"FLEET", "hello", "world"}, sendKeysArgs{target: "FLEET", text: []string{"hello", "world"}}},
		{"server flag", []string{"--server", "ws", "123", "hi"}, sendKeysArgs{server: "ws", target: "123", text: []string{"hi"}}},
		{"server equals", []string{"--server=ws", "FLEET", "hi"}, sendKeysArgs{server: "ws", target: "FLEET", text: []string{"hi"}}},
		{"host:pid", []string{"ws:123", "hi"}, sendKeysArgs{server: "ws", target: "123", text: []string{"hi"}}},
		{"host:pid agrees with --server", []string{"--server", "ws", "ws:123", "hi"}, sendKeysArgs{server: "ws", target: "123", text: []string{"hi"}}},
		{"host with colon splits on last", []string{"a:b:42", "hi"}, sendKeysArgs{server: "a:b", target: "42", text: []string{"hi"}}},
		{"non-digit suffix not split", []string{"ws:abc", "hi"}, sendKeysArgs{target: "ws:abc", text: []string{"hi"}}},
		{"empty host not split", []string{":123", "hi"}, sendKeysArgs{target: ":123", text: []string{"hi"}}},
		{"trailing colon not split", []string{"ws:", "hi"}, sendKeysArgs{target: "ws:", text: []string{"hi"}}},
		{"flag-like text after target is text", []string{"FLEET", "-y", "--server", "x"}, sendKeysArgs{target: "FLEET", text: []string{"-y", "--server", "x"}}},
		{"stdin dash", []string{"FLEET", "-"}, sendKeysArgs{target: "FLEET", text: []string{"-"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseSendKeysArgs(c.args)
			if err != nil {
				t.Fatalf("parseSendKeysArgs(%q) error = %v", c.args, err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("parseSendKeysArgs(%q) = %+v, want %+v", c.args, got, c.want)
			}
		})
	}
}

func TestParseSendKeysArgsErrors(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no args", nil, "missing TARGET"},
		{"no text", []string{"FLEET"}, "missing TEXT"},
		{"only flags", []string{"--server", "ws"}, "missing TARGET"},
		{"empty target", []string{"", "hi"}, "missing TARGET"},
		{"server without value", []string{"--server"}, "--server needs a value"},
		{"server empty value", []string{"--server", "", "1", "hi"}, "--server needs a value"},
		{"server equals empty", []string{"--server=", "1", "hi"}, "--server needs a value"},
		{"unknown flag", []string{"-y", "FLEET", "hi"}, "unknown flag: -y"},
		{"host disagrees with --server", []string{"--server", "a", "b:123", "hi"}, "disagrees"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := parseSendKeysArgs(c.args)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("parseSendKeysArgs(%q) error = %v, want containing %q", c.args, err, c.want)
			}
		})
	}
}

func TestSendKeysTextJoinsWords(t *testing.T) {
	got, err := sendKeysText([]string{"hello", "big", "world"}, failingReader{})
	if err != nil || got != "hello big world" {
		t.Fatalf("sendKeysText = %q, %v; want %q", got, err, "hello big world")
	}
	// A "-" among other words is literal text, not stdin.
	got, err = sendKeysText([]string{"a", "-", "b"}, failingReader{})
	if err != nil || got != "a - b" {
		t.Fatalf("sendKeysText = %q, %v; want %q", got, err, "a - b")
	}
}

func TestSendKeysTextStdin(t *testing.T) {
	cases := []struct{ in, want string }{
		{"hello\n", "hello"},
		{"hello\r\n", "hello"},
		{"hello", "hello"},
		{"hello\n\n", "hello\n"}, // only one newline stripped; the rest is refused later
		{"a\nb\n", "a\nb"},
		{"hello\r", "hello\r"}, // a bare CR is not a line ending here
		{"", ""},
	}
	for _, c := range cases {
		got, err := sendKeysText([]string{"-"}, strings.NewReader(c.in))
		if err != nil || got != c.want {
			t.Errorf("sendKeysText(stdin %q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
}

func TestSendKeysTextStdinLimit(t *testing.T) {
	// A full-length line with a CRLF ending still fits.
	full := strings.Repeat("x", sendKeysMaxLen)
	got, err := sendKeysText([]string{"-"}, strings.NewReader(full+"\r\n"))
	if err != nil || got != full {
		t.Fatalf("full-length line: len %d, err %v; want len %d", len(got), err, sendKeysMaxLen)
	}
	if err := validateSendKeysText(got); err != nil {
		t.Fatalf("full-length line refused: %v", err)
	}
	// Oversized input is read only past the limit and then refused, never cut
	// down to a valid message.
	got, err = sendKeysText([]string{"-"}, strings.NewReader(strings.Repeat("x", 10*sendKeysMaxLen)))
	if err != nil {
		t.Fatalf("oversized: err %v", err)
	}
	if len(got) <= sendKeysMaxLen || len(got) > sendKeysMaxLen+2 {
		t.Fatalf("oversized: read %d bytes, want just past %d", len(got), sendKeysMaxLen)
	}
	if err := validateSendKeysText(got); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized: validate = %v, want exceeds error", err)
	}
}

func TestSendKeysTextStdinReadError(t *testing.T) {
	if _, err := sendKeysText([]string{"-"}, failingReader{}); err == nil {
		t.Fatal("sendKeysText with a failing stdin returned no error")
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("boom") }

func TestValidateSendKeysText(t *testing.T) {
	ok := []string{"hi", "-R hello", "Enter", "C-c", "ünïcode ✓", strings.Repeat("x", sendKeysMaxLen)}
	for _, s := range ok {
		if err := validateSendKeysText(s); err != nil {
			t.Errorf("validateSendKeysText(%q) = %v, want nil", s, err)
		}
	}
	bad := map[string]string{
		"":                                    "must not be empty",
		strings.Repeat("x", sendKeysMaxLen+1): "exceeds",
		"a\nb":                                "control characters",
		"a\rb":                                "control characters",
		"a\x00b":                              "control characters",
		"a\x1b[2Jb":                           "control characters",
		"a\x7fb":                              "control characters",
		"tab\there":                           "control characters",
	}
	for s, want := range bad {
		if err := validateSendKeysText(s); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("validateSendKeysText(%q) = %v, want containing %q", s, err, want)
		}
	}
}

func TestMatchSendKeysTarget(t *testing.T) {
	sessions := []Session{
		{PID: 100, SessionID: "aaaa-1111", Name: "FLEET", Tmux: "fleet:0.0"},
		{PID: 200, SessionID: "bbbb-2222", Name: "", Tmux: "work:1.0"},
		{PID: 300, SessionID: "cccc-3333", Name: "CHAT", Tmux: ""},
		{PID: 400, SessionID: "dddd-4444", Name: "", Tmux: "123:0.0"},
	}
	cases := []struct {
		target string
		want   int
	}{
		{"100", 100},       // pid
		{"work", 200},      // tmux session name
		{"FLEET", 100},     // user-set Name
		{"fleet", 100},     // tmux name of the same row
		{"cccc-3333", 300}, // session id
		{"CHAT", 300},      // Name on a row with no tmux pane
		{"400", 400},       // pid, not the tmux name "123"
	}
	for _, c := range cases {
		got, err := matchSendKeysTarget(sessions, c.target)
		if err != nil {
			t.Errorf("matchSendKeysTarget(%q) error = %v", c.target, err)
			continue
		}
		if got.PID != c.want {
			t.Errorf("matchSendKeysTarget(%q) = PID %d, want %d", c.target, got.PID, c.want)
		}
	}
}

func TestMatchSendKeysTargetDigitsNeverMatchTmuxName(t *testing.T) {
	sessions := []Session{{PID: 400, SessionID: "d", Tmux: "123:0.0"}}
	_, err := matchSendKeysTarget(sessions, "123")
	if err == nil || !strings.Contains(err.Error(), `no live session matches "123"`) {
		t.Fatalf("matchSendKeysTarget(123) = %v, want no-match error", err)
	}
}

func TestMatchSendKeysTargetDedupesOneRow(t *testing.T) {
	// One row whose tmux name and Name are both "FLEET" is one match.
	sessions := []Session{
		{PID: 100, SessionID: "a", Name: "FLEET", Tmux: "FLEET:0.0"},
		{PID: 200, SessionID: "b", Tmux: "other:0.0"},
	}
	got, err := matchSendKeysTarget(sessions, "FLEET")
	if err != nil || got.PID != 100 {
		t.Fatalf("matchSendKeysTarget(FLEET) = PID %d, %v; want PID 100", got.PID, err)
	}
}

func TestMatchSendKeysTargetAmbiguous(t *testing.T) {
	sessions := []Session{
		{PID: 100, SessionID: "a", Name: "dev", Tmux: "x:0.0"},
		{PID: 200, SessionID: "b", Tmux: "dev:0.0"},
		{PID: 300, SessionID: "c", Tmux: "y:0.0"},
	}
	_, err := matchSendKeysTarget(sessions, "dev")
	if err == nil || err.Error() != `"dev" is ambiguous: PID 100, PID 200` {
		t.Fatalf("matchSendKeysTarget(dev) = %v, want ambiguity listing PID 100, PID 200", err)
	}
}

func TestMatchSendKeysTargetNoMatch(t *testing.T) {
	sessions := []Session{{PID: 100, SessionID: "a", Name: "FLEET", Tmux: "fleet:0.0"}}
	for _, target := range []string{"nope", "999", ""} {
		if _, err := matchSendKeysTarget(sessions, target); err == nil {
			t.Errorf("matchSendKeysTarget(%q) = nil error, want refusal", target)
		}
	}
	// A row without a Name or tmux pane is never matched by an empty field.
	if _, err := matchSendKeysTarget([]Session{{PID: 1}}, "x"); err == nil {
		t.Error("matchSendKeysTarget matched a row with empty fields")
	}
}
