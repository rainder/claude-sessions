// cmd_send_keys.go
package main

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

const sendKeysUsage = "usage: claude-sessions send-keys [--server SERVER] TARGET TEXT..."

// sendKeysArgs is the parsed command line of the send-keys subcommand.
type sendKeysArgs struct {
	server string   // "" = this host
	target string   // PID, tmux session name, session name or session id
	text   []string // the TEXT words; a lone "-" means read stdin
}

// parseSendKeysArgs reads `[--server SERVER] TARGET TEXT...`. Flags are read
// only before TARGET, so a text word such as "-y" is typed, never parsed. A
// HOST:PID target (Session.ID()'s remote form) is shorthand for --server HOST
// PID; it splits only when the part after the last colon is all digits and
// the host part is non-empty, so a plain tmux name is never cut. A --server
// that disagrees with the HOST: prefix is an error: neither can be picked
// without guessing which host the caller meant.
func parseSendKeysArgs(args []string) (sendKeysArgs, error) {
	var a sendKeysArgs
	i := 0
	for ; i < len(args) && strings.HasPrefix(args[i], "-"); i++ {
		switch arg := args[i]; {
		case arg == "--server":
			if i+1 >= len(args) || args[i+1] == "" {
				return a, fmt.Errorf("--server needs a value")
			}
			i++
			a.server = args[i]
		case strings.HasPrefix(arg, "--server="):
			a.server = strings.TrimPrefix(arg, "--server=")
			if a.server == "" {
				return a, fmt.Errorf("--server needs a value")
			}
		default:
			return a, fmt.Errorf("unknown flag: %s", arg)
		}
	}
	if i >= len(args) || args[i] == "" {
		return a, fmt.Errorf("missing TARGET")
	}
	a.target = args[i]
	if j := strings.LastIndex(a.target, ":"); j > 0 && isAllDigits(a.target[j+1:]) {
		host := a.target[:j]
		if a.server != "" && a.server != host {
			return a, fmt.Errorf("--server %s disagrees with target host %s", a.server, host)
		}
		a.server, a.target = host, a.target[j+1:]
	}
	a.text = args[i+1:]
	if len(a.text) == 0 {
		return a, fmt.Errorf("missing TEXT")
	}
	return a, nil
}

// isAllDigits reports whether s is non-empty and only ASCII digits.
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// sendKeysText builds the message: the TEXT words joined with single spaces,
// or stdin when the only word is "-". stdin is read up to two bytes past the
// limit, so an oversized message still fails validateSendKeysText's length
// check (rather than being cut silently) while a full-length line ending in
// "\r\n" still fits. One trailing "\n" or "\r\n" is stripped; any other
// newline stays and is refused as a control byte.
func sendKeysText(words []string, stdin io.Reader) (string, error) {
	if len(words) != 1 || words[0] != "-" {
		return strings.Join(words, " "), nil
	}
	b, err := io.ReadAll(io.LimitReader(stdin, int64(sendKeysMaxLen)+2))
	if err != nil {
		return "", fmt.Errorf("read stdin: %w", err)
	}
	s := string(b)
	if strings.HasSuffix(s, "\n") {
		s = strings.TrimSuffix(strings.TrimSuffix(s, "\n"), "\r")
	}
	return s, nil
}

// matchSendKeysTarget picks the one row target names. An all-digit target is
// a PID and matches only a PID, so a tmux session named "123" never shadows
// PID 123. Any other target matches a row's tmux session name, user-set Name
// or session id; a row that matches several ways counts once. No match and
// more than one match are both errors: typing into a guessed pane is worse
// than refusing.
func matchSendKeysTarget(sessions []Session, target string) (Session, error) {
	if target == "" {
		return Session{}, fmt.Errorf("empty target")
	}
	var matches []Session
	if isAllDigits(target) {
		pid, err := strconv.Atoi(target)
		if err == nil {
			for _, s := range sessions {
				if s.PID == pid {
					matches = append(matches, s)
				}
			}
		}
	} else {
		seen := map[int]bool{}
		for _, s := range sessions {
			hit := trimColon(s.Tmux) == target || s.Name == target || s.SessionID == target
			if hit && !seen[s.PID] {
				seen[s.PID] = true
				matches = append(matches, s)
			}
		}
	}
	switch len(matches) {
	case 0:
		return Session{}, fmt.Errorf("no live session matches %q", target)
	case 1:
		return matches[0], nil
	}
	pids := make([]string, len(matches))
	for i, s := range matches {
		pids[i] = fmt.Sprintf("PID %d", s.PID)
	}
	return Session{}, fmt.Errorf("%q is ambiguous: %s", target, strings.Join(pids, ", "))
}

// cmdSendKeys types TEXT plus Enter into one session's tmux pane, locally or
// on a servers.yaml host. It takes the TUI compose box's path
// (sendKeysToInspected): local is a fresh resolveLivePIDLocal then sendKeys,
// remote is POST /sessions/{pid}/send-keys with the matched row's session id
// as the precondition. The text is checked here with the server's own rule
// first, so a local send refuses exactly what a remote one would.
func cmdSendKeys(args []string) int {
	a, err := parseSendKeysArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "send-keys: %v\n%s\n", err, sendKeysUsage)
		return 2
	}
	text, err := sendKeysText(a.text, os.Stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "send-keys: %v\n", err)
		return 1
	}
	if err := validateSendKeysText(text); err != nil {
		fmt.Fprintf(os.Stderr, "send-keys: %v\n", err)
		return 2
	}

	var sessions []Session
	if a.server == "" {
		sessions, err = CollectLocal()
		if err != nil {
			fmt.Fprintf(os.Stderr, "send-keys: %v\n", err)
			return 1
		}
	} else {
		srv, ok := LookupServer(a.server)
		if !ok {
			cfgs, _ := LoadServerConfigs()
			names := make([]string, len(cfgs))
			for i, c := range cfgs {
				names[i] = c.Name
			}
			fmt.Fprintf(os.Stderr, "send-keys: unknown server %q (configured: %s)\n", a.server, strings.Join(names, ", "))
			return 2
		}
		r := FetchRemote(srv)
		if r.Error != "" {
			fmt.Fprintf(os.Stderr, "send-keys: %s: %s\n", a.server, r.Error)
			return 1
		}
		sessions = r.Sessions
	}

	match, err := matchSendKeysTarget(sessions, a.target)
	if err != nil {
		fmt.Fprintf(os.Stderr, "send-keys: %v\n", err)
		return 1
	}

	if a.server == "" {
		live, err := resolveLivePIDLocal(match.PID, match.SessionID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "send-keys: %v\n", err)
			return 1
		}
		if err := sendKeys(live, text); err != nil {
			fmt.Fprintf(os.Stderr, "send-keys: %v\n", err)
			return 1
		}
		fmt.Printf("sent to %s (PID %d)\n", trimColon(live.Tmux), live.PID)
		return 0
	}
	r, err := sendKeysRemote(a.server, match.PID, match.SessionID, text)
	if err != nil {
		fmt.Fprintf(os.Stderr, "send-keys: %s: %v\n", a.server, err)
		return 1
	}
	if !r.OK {
		msg := r.Error
		if r.Code != "" {
			msg += " (" + r.Code + ")"
		}
		fmt.Fprintf(os.Stderr, "send-keys: %s\n", msg)
		return 1
	}
	fmt.Printf("sent to %s: %s (PID %d)\n", a.server, trimColon(match.Tmux), match.PID)
	return 0
}
