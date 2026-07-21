package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// errSessionEnded signals that a session's live pane and transcript are both
// gone (the process exited or the files were removed). The server maps it to
// 404; fetchRemotePreview reconstructs it from a remote 404.
var errSessionEnded = errors.New("session ended")

// errNoTmuxPane is an internal sentinel returned by the tmux capture step when
// the pid is not attached to any tmux pane, telling LoadPreview to fall back to
// the transcript. It is never surfaced to callers.
var errNoTmuxPane = errors.New("no tmux pane for pid")

// transcriptTailEntries is how many trailing user/assistant entries the
// transcript fallback renders before the byte/line bounds are applied.
const transcriptTailEntries = 8

// PreviewLimits bounds a rendered preview so a single request can never pull an
// unbounded amount of pane scrollback or transcript history.
type PreviewLimits struct {
	MaxLines int
	MaxBytes int
}

// DefaultPreviewLimits is the standard bound: 2000 lines and 512 KiB. These are
// also the maximum accepted query values on the server side.
func DefaultPreviewLimits() PreviewLimits {
	return PreviewLimits{MaxLines: 2000, MaxBytes: 512 << 10}
}

// PreviewResult is a bounded, sanitized snapshot of a session's activity.
// Source is "tmux" or "transcript"; Label is a human-readable origin (the pane
// coordinates or the transcript path); Content is safe to write straight to a
// terminal.
type PreviewResult struct {
	Source  string
	Label   string
	Content string
}

// Injectable seams around external effects, replaced in tests via t.Cleanup.
var (
	previewTmuxCapture   = captureTmuxPreview
	previewSessionLookup = readSessionByPID
)

// LoadPreview builds a bounded, sanitized preview for a local pid. It prefers a
// live tmux pane and falls back to the JSONL transcript. Errors are returned
// rather than embedded in Content: errSessionEnded when nothing live remains,
// and the underlying error for tmux/transcript read failures.
func LoadPreview(pid int, limits PreviewLimits) (PreviewResult, error) {
	label, content, err := previewTmuxCapture(pid, limits)
	if err == nil {
		return PreviewResult{
			Source:  "tmux",
			Label:   label,
			Content: limitPreview(sanitizeTerminalText(content), limits),
		}, nil
	}
	if !errors.Is(err, errNoTmuxPane) {
		return PreviewResult{}, err
	}

	// No live pane — render the transcript tail instead.
	sess, ok := previewSessionLookup(pid)
	if !ok || sess.SessionID == "" {
		return PreviewResult{}, errSessionEnded
	}
	home, _ := os.UserHomeDir()
	path := findTranscript(home, sess.SessionID)
	if path == "" {
		return PreviewResult{}, errSessionEnded
	}
	raw, err := formatTranscriptTail(path, transcriptTailEntries)
	if err != nil {
		return PreviewResult{}, err
	}
	return PreviewResult{
		Source:  "transcript",
		Label:   path,
		Content: limitPreview(sanitizeTerminalText(raw), limits),
	}, nil
}

// captureTmuxPreview locates the tmux pane hosting pid and returns its raw
// (unsanitized) scrollback capped at limits.MaxLines lines. Returns
// errNoTmuxPane when pid is not in tmux so the caller can fall back; any other
// error is a genuine capture failure.
func captureTmuxPreview(pid int, limits PreviewLimits) (label, content string, err error) {
	panes, _ := tmuxPaneMap()
	ppid, _ := ppidMap()
	loc := walkTmuxPane(pid, panes, ppid)
	if loc == "" {
		return "", "", errNoTmuxPane
	}
	out, err := exec.Command("tmux", "capture-pane", "-p", "-e",
		"-S", "-"+strconv.Itoa(limits.MaxLines), "-t", loc).Output()
	if err != nil {
		return "", "", fmt.Errorf("tmux capture-pane: %w", err)
	}
	return "tmux pane " + loc, string(out), nil
}

// sanitizeTerminalText strips control sequences that could hijack the viewer's
// terminal, keeping only complete CSI SGR ("...m") color sequences and printable
// text. OSC (through BEL or ST), non-SGR CSI, and DCS/APC/PM (through ST) are
// removed, CR and disallowed C0 controls are dropped, tabs expand to four
// spaces, and newlines are preserved.
func sanitizeTerminalText(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		c := s[i]
		if c == 0x1b { // ESC — start of an escape sequence
			i = sanitizeEscape(s, i, &b)
			continue
		}
		switch {
		case c == '\n':
			b.WriteByte('\n')
		case c == '\t':
			b.WriteString("    ")
		case c == '\r':
			// drop carriage returns
		case c < 0x20 || c == 0x7f:
			// drop other C0 controls and DEL
		default:
			b.WriteByte(c) // printable ASCII or UTF-8 continuation byte
		}
		i++
	}
	return b.String()
}

// sanitizeEscape consumes the escape sequence beginning at s[i] (an ESC byte),
// writing it to b only if it is a complete CSI SGR sequence. Returns the index
// just past the consumed sequence.
func sanitizeEscape(s string, i int, b *strings.Builder) int {
	n := len(s)
	if i+1 >= n {
		return n // lone trailing ESC — drop
	}
	switch s[i+1] {
	case '[': // CSI
		j := i + 2
		for j < n {
			c := s[j]
			if c >= 0x40 && c <= 0x7e { // final byte
				if c == 'm' { // SGR — keep the complete sequence
					b.WriteString(s[i : j+1])
				}
				return j + 1
			}
			if c < 0x20 || c > 0x3f { // not a param/intermediate byte — malformed
				return j // leave the offending byte for the main loop
			}
			j++
		}
		return n // unterminated CSI — drop
	case ']': // OSC — strip through BEL or ST
		return skipToStringTerminator(s, i+2)
	case 'P', '_', '^': // DCS, APC, PM — strip through ST
		return skipToStringTerminator(s, i+2)
	default:
		return i + 2 // two-byte escape (charset select, etc.) — drop both
	}
}

// skipToStringTerminator returns the index just past the next string terminator
// (ST = ESC \) or BEL starting from j, or len(s) if none is found.
func skipToStringTerminator(s string, j int) int {
	n := len(s)
	for j < n {
		if s[j] == 0x07 { // BEL
			return j + 1
		}
		if s[j] == 0x1b && j+1 < n && s[j+1] == '\\' { // ST
			return j + 2
		}
		j++
	}
	return n
}

// limitPreview retains the newest MaxLines lines and then trims to MaxBytes,
// dropping the oldest whole line at the byte boundary so the result never starts
// mid-line. A single line longer than MaxBytes is hard-cut to its tail.
func limitPreview(s string, limits PreviewLimits) string {
	if limits.MaxLines > 0 {
		lines := strings.SplitAfter(s, "\n")
		if len(lines) > 0 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1] // drop empty tail from a trailing newline
		}
		if len(lines) > limits.MaxLines {
			lines = lines[len(lines)-limits.MaxLines:]
		}
		s = strings.Join(lines, "")
	}
	if limits.MaxBytes > 0 && len(s) > limits.MaxBytes {
		cut := len(s) - limits.MaxBytes
		// If cut lands mid-line, advance to the start of the next whole line so
		// the result never begins with a truncated line (or escape sequence).
		if cut > 0 && s[cut-1] != '\n' {
			if nl := strings.IndexByte(s[cut:], '\n'); nl >= 0 {
				cut += nl + 1
			}
		}
		if cut < len(s) {
			s = s[cut:]
		} else {
			s = s[len(s)-limits.MaxBytes:] // single line longer than MaxBytes
		}
	}
	return s
}

// PreviewContent returns a human-readable preview of the session's current
// activity, preserving the CLI/legacy-client format (a bold "source:" header
// followed by the bounded, sanitized content). On failure it returns the error
// text plus a newline.
func PreviewContent(pid int) string {
	res, err := LoadPreview(pid, DefaultPreviewLimits())
	if err != nil {
		return err.Error() + "\n"
	}
	var head string
	switch res.Source {
	case "transcript":
		head = bold("source: ") + "transcript tail  " + dim(res.Label)
	default:
		head = bold("source: ") + res.Label
	}
	return head + "\n\n" + res.Content
}

// formatTranscriptTail reads the JSONL transcript and renders the last n
// user/assistant entries. It returns an error for read failures rather than
// embedding the message in the output.
func formatTranscriptTail(path string, n int) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open transcript: %w", err)
	}
	defer f.Close()

	type entry struct {
		Type    string          `json:"type"`
		Message json.RawMessage `json:"message"`
	}
	var convo []entry
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024) // some entries are huge
	for scanner.Scan() {
		var e entry
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			continue
		}
		if e.Type != "user" && e.Type != "assistant" {
			continue
		}
		convo = append(convo, e)
	}
	if len(convo) == 0 {
		return "(no user/assistant entries in transcript)\n", nil
	}
	if len(convo) > n {
		convo = convo[len(convo)-n:]
	}

	var b strings.Builder
	for _, e := range convo {
		renderEntry(&b, e.Type, e.Message)
	}
	return b.String(), nil
}

func renderEntry(w *strings.Builder, typ string, msgRaw json.RawMessage) {
	var msg struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	_ = json.Unmarshal(msgRaw, &msg)
	role := msg.Role
	if role == "" {
		role = typ
	}

	color := "1"
	switch role {
	case "user":
		color = "1;34"
	case "assistant":
		color = "1;32"
	}
	fmt.Fprintf(w, "\033[%sm┌─ %s\033[0m\n", color, role)

	// Content can be a string or a list of blocks.
	var contentStr string
	if err := json.Unmarshal(msg.Content, &contentStr); err == nil {
		fmt.Fprintf(w, "  │ %s\n\n", trunc(contentStr, 600))
		return
	}
	var blocks []map[string]any
	if err := json.Unmarshal(msg.Content, &blocks); err != nil {
		fmt.Fprintln(w)
		return
	}
	for _, c := range blocks {
		switch c["type"] {
		case "text":
			if t, ok := c["text"].(string); ok {
				fmt.Fprintf(w, "  │ %s\n", trunc(t, 600))
			}
		case "thinking":
			if t, ok := c["thinking"].(string); ok {
				fmt.Fprintf(w, "  │ %s %s\n", dim("(thinking)"), trunc(t, 300))
			}
		case "tool_use":
			name, _ := c["name"].(string)
			inp, _ := json.Marshal(c["input"])
			fmt.Fprintf(w, "  │ \033[1;36m→ %s\033[0m %s\n", name, trunc(string(inp), 300))
		case "tool_result":
			fmt.Fprintf(w, "  │ %s %s\n", dim("← result:"),
				trunc(toolResultText(c["content"]), 400))
		}
	}
	fmt.Fprintln(w)
}

// toolResultText extracts the displayable text from a tool_result block,
// which can be a string or a list of blocks.
func toolResultText(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if list, ok := v.([]any); ok {
		var parts []string
		for _, c := range list {
			if m, ok := c.(map[string]any); ok && m["type"] == "text" {
				if t, ok := m["text"].(string); ok {
					parts = append(parts, t)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return fmt.Sprint(v)
}

// trunc cuts a string to n chars and indents continuation lines under "│ ".
func trunc(s string, n int) string {
	s = strings.ReplaceAll(s, "\r", "")
	if len(s) > n {
		s = s[:n] + dim(fmt.Sprintf("  …(+%d chars)", len(s)-n))
	}
	return strings.ReplaceAll(s, "\n", "\n  │ ")
}

// tmuxSessionForPID returns the tmux session name (without :win.pane suffix)
// for the given pid, or "" if not in tmux.
func tmuxSessionForPID(pid int) string {
	panes, _ := tmuxPaneMap()
	ppid, _ := ppidMap()
	loc := walkTmuxPane(pid, panes, ppid)
	if loc == "" {
		return ""
	}
	return strings.SplitN(loc, ":", 2)[0]
}
