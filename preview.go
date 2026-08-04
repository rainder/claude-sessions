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
	"unicode/utf8"
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
// transcript fallback renders before the byte/line bounds are applied. It is
// the depth of the live tail only: a paged request (PreviewLimits.Offset)
// reaches as far back as it needs to.
const transcriptTailEntries = 8

// PreviewLimits bounds a rendered preview so a single request can never pull an
// unbounded amount of pane scrollback or transcript history.
type PreviewLimits struct {
	MaxLines int
	MaxBytes int
	// Offset pages backwards through history: how many lines to skip from the
	// newest end of the source before MaxLines/MaxBytes apply. Zero — the only
	// value anything sent before the "preview-range" capability — is the live
	// tail, byte for byte the request this made before paging existed.
	//
	// The unit is rendered lines for both sources (pane scrollback and
	// transcript tail), so one number means one thing whatever the source
	// header says.
	//
	// A client pages by adding the number of lines it actually RECEIVED, not
	// the number it asked for: a page can come back short because MaxBytes
	// trimmed it or history ran out, and only the received count keeps the
	// next window contiguous. Reading it as "offset += lines" leaves gaps.
	//
	// Paging needs MaxLines > 0 to size the window: the server never accepts a
	// request without it (lines is validated 1..2000), and a caller that builds
	// these by hand and sets an Offset without a MaxLines gets the live tail.
	//
	// The pane is a live thing and this is a plain offset, not a snapshot
	// cursor: a pane that keeps printing shifts its own history under a fixed
	// offset, so a page taken while output flows can repeat or skip lines. A
	// paused or finished session pages exactly.
	Offset int
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
	previewPaneGeometry  = tmuxPaneGeometry
	previewTmuxRun       = func(args ...string) ([]byte, error) {
		return exec.Command("tmux", args...).Output()
	}
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
	raw, err := formatTranscriptTail(path, transcriptTailEntries, limits)
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
// (unsanitized) scrollback capped at limits.MaxLines lines, ending
// limits.Offset lines above the newest line. Returns errNoTmuxPane when pid is
// not in tmux so the caller can fall back; any other error is a genuine capture
// failure.
func captureTmuxPreview(pid int, limits PreviewLimits) (label, content string, err error) {
	panes, _ := tmuxPaneMap()
	ppid, _ := ppidMap()
	pane, found := walkTmuxPane(pid, panes, ppid)
	if !found {
		return "", "", errNoTmuxPane
	}
	content, err = capturePaneContent(pane.Location, limits)
	if err != nil {
		return "", "", err
	}
	return "tmux pane " + pane.Location, content, nil
}

// capturePaneContent runs capture-pane for the window limits describes.
//
// Unpaged (Offset 0) it sends exactly the argv it always sent — no -E, no
// geometry lookup, one exec — because every existing caller is unpaged and none
// of them may pay for or notice paging. A page-back needs the pane's geometry
// first: tmux numbers the first VISIBLE line 0, so where the newest line sits
// (height-1) and how far back the history goes are both properties of the pane,
// not constants.
//
// A window entirely older than the history is an empty page, not an error: the
// caller keeps the tmux source and the client reads "no more history".
//
// The probe and the capture are two separate tmux invocations and tmux offers
// no way to make them one: capture-pane's -S/-E take plain numbers, so the
// range cannot be expressed in terms of a geometry the same command would
// resolve. The one geometry-free capture that would be atomic — -S
// -(Offset+MaxLines) with no -E, then dropping the newest Offset lines locally
// — asks tmux to hand over every line down to the newest on every page, so a
// deep offset would ship megabytes to trim them away again. Paying O(Offset) of
// transfer on every page to close a race that costs one page is the wrong
// trade, so the window stays open and is made harmless instead:
//
// If the pane's history changes in between (a clear-history, or a burst of
// output long enough to push lines out), the computed range names lines the
// pane no longer holds. tmux clamps such a range to what is left rather than
// failing — verified against tmux 3.7b: -S -310 -E -211 against a pane whose
// history had just been cleared returned exit 0 and a single row, the pane's
// top visible line. So the worst case is one already-visible row served as
// history for one request, never an error, never an unbounded read, and never
// sticky: the next page's own probe sees the new geometry and answers the empty
// page that is now the truth. TestCapturePaneContentSurvivesAClearedHistory
// pins both halves.
func capturePaneContent(location string, limits PreviewLimits) (string, error) {
	args := []string{"capture-pane", "-p", "-e"}
	if limits.Offset > 0 && limits.MaxLines > 0 {
		height, history, err := previewPaneGeometry(location)
		if err != nil {
			return "", err
		}
		start, end, ok := capturePaneRange(limits, height, history)
		if !ok {
			return "", nil
		}
		args = append(args, "-S", strconv.Itoa(start), "-E", strconv.Itoa(end))
	} else {
		args = append(args, "-S", "-"+strconv.Itoa(limits.MaxLines))
	}
	args = append(args, "-t", location)
	out, err := previewTmuxRun(args...)
	if err != nil {
		return "", fmt.Errorf("tmux capture-pane: %w", err)
	}
	return string(out), nil
}

// capturePaneRange converts an offset in lines-from-the-newest-line into tmux
// capture-pane -S/-E line numbers for a pane `height` rows tall with `history`
// lines of scrollback. Line 0 is the first visible row, so the newest line is
// height-1 and the history runs -1..-history.
//
// The unpaged capture (-S -MaxLines, no -E) returns MaxLines+height rows and
// limitPreview keeps the newest MaxLines of them, so page 0 effectively ends at
// height-1. Ending page n at height-1-Offset therefore makes consecutive pages
// contiguous: no gap the size of the pane, no repeated block.
//
// ok is false when the whole window is older than the history — an empty page.
func capturePaneRange(limits PreviewLimits, height, history int) (start, end int, ok bool) {
	end = height - 1 - limits.Offset
	if end < -history {
		return 0, 0, false
	}
	start = end - limits.MaxLines + 1
	if start < -history {
		start = -history
	}
	return start, end, true
}

// tmuxPaneGeometry returns a pane's visible height and the number of lines in
// its scrollback.
//
// The format is two numeric fields separated by a space and carries no free
// text, so it parses identically under any locale — the same discipline
// tmuxPaneMap documents. It is only ever run for a paged request.
func tmuxPaneGeometry(location string) (height, history int, err error) {
	out, err := previewTmuxRun("-u", "display-message", "-p", "-t", location,
		"-F", "#{pane_height} #{history_size}")
	if err != nil {
		return 0, 0, fmt.Errorf("tmux display-message: %w", err)
	}
	return parseTmuxPaneGeometry(string(out))
}

func parseTmuxPaneGeometry(out string) (height, history int, err error) {
	fields := strings.Fields(out)
	if len(fields) != 2 {
		return 0, 0, fmt.Errorf("tmux pane geometry: %q", strings.TrimSpace(out))
	}
	height, err = strconv.Atoi(fields[0])
	if err != nil {
		return 0, 0, fmt.Errorf("tmux pane height: %q", fields[0])
	}
	history, err = strconv.Atoi(fields[1])
	if err != nil {
		return 0, 0, fmt.Errorf("tmux history size: %q", fields[1])
	}
	return height, history, nil
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
		if c < 0x80 { // ASCII fast path
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
				b.WriteByte(c) // printable ASCII
			}
			i++
			continue
		}
		// Non-ASCII: decode a full rune so multi-byte characters stay intact
		// (a naive per-byte drop of 0x80–0x9f would shred continuation bytes of
		// legitimate runes such as "→" = e2 86 92). Invalid bytes — raw C1 like
		// 0x9b/0x9d and stray continuations — decode to RuneError with size 1
		// and are dropped. C1 controls (U+0080–U+009F, e.g. the UTF-8 pair
		// 0xc2 0x9b) are dropped as whole runes so they can never reach a
		// C1-honoring terminal as a CSI/OSC/DCS/PM/APC introducer.
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			i++
			continue
		}
		if r >= 0x80 && r <= 0x9f {
			i += size
			continue
		}
		b.WriteString(s[i : i+size])
		i += size
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
		numeric := true // every body byte so far is a digit, ':' or ';'
		for j < n {
			c := s[j]
			if c >= 0x40 && c <= 0x7e { // final byte
				// Keep only a complete SGR ("m") sequence whose body is purely
				// numeric parameters (0x30–0x3b: digits, ':', ';'). Private and
				// intermediate markers — '<','=','>','?' and 0x20–0x2f — are
				// excluded so sequences like "\x1b[>4;2m" (XTMODKEYS) or
				// "\x1b[?4m" are stripped rather than replayed to the terminal.
				if c == 'm' && numeric {
					b.WriteString(s[i : j+1])
				}
				return j + 1
			}
			if c < 0x20 || c > 0x3f { // not a param/intermediate byte — malformed
				return j // leave the offending byte for the main loop
			}
			if c < 0x30 || c > 0x3b { // private/intermediate marker — not a pure SGR
				numeric = false
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

// previewLines splits s the one way this server counts preview lines:
// everything through its "\n" is a line, and a trailing newline ends the last
// line rather than opening an empty one of its own.
//
// It exists so there is exactly one such rule. The number is a wire value —
// the server reports it as X-Claude-Sessions-Preview-Lines and a client adds it
// to its own offset for the next page — so a second count that disagreed by one
// would not look like a bug, it would look like a gap or a repeat in the
// history the user is scrolling through.
func previewLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.SplitAfter(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1] // drop empty tail from a trailing newline
	}
	return lines
}

// countPreviewLines reports how many lines s holds under previewLines' rule.
func countPreviewLines(s string) int { return len(previewLines(s)) }

// limitPreview retains the newest MaxLines lines and then trims to MaxBytes,
// dropping the oldest whole line at the byte boundary so the result never starts
// mid-line. A single line longer than MaxBytes is hard-cut to its tail.
func limitPreview(s string, limits PreviewLimits) string {
	if limits.MaxLines > 0 {
		lines := previewLines(s)
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
			for len(s) > 0 && s[0] >= 0x80 && s[0] < 0xc0 {
				s = s[1:] // don't begin on a UTF-8 continuation byte
			}
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

// transcriptEntry is one user/assistant line of a JSONL transcript, kept raw
// until it is rendered so paging can decide how deep to render.
type transcriptEntry struct {
	Type    string          `json:"type"`
	Message json.RawMessage `json:"message"`
}

// formatTranscriptTail reads the JSONL transcript and renders the last n
// user/assistant entries, or — when limits.Offset pages back — the window of
// rendered lines ending Offset lines above the newest one. It returns an error
// for read failures rather than embedding the message in the output.
func formatTranscriptTail(path string, n int, limits PreviewLimits) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open transcript: %w", err)
	}
	defer f.Close()

	var convo []transcriptEntry
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024) // some entries are huge
	for scanner.Scan() {
		var e transcriptEntry
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			continue
		}
		if e.Type != "user" && e.Type != "assistant" {
			continue
		}
		convo = append(convo, e)
	}
	if limits.Offset > 0 {
		return transcriptPage(convo, limits), nil
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

// transcriptPage renders the window of rendered lines that ends limits.Offset
// lines above the newest line. It renders entries newest-first and stops as soon
// as it holds Offset+MaxLines lines, so a page costs the same whether the
// transcript has fifty entries or five thousand.
//
// Paging past the first entry is an empty page — the same "no more history" a
// pane at the top of its scrollback gives, and never the
// "(no user/assistant entries)" notice, which describes the live tail rather
// than a stretch of history.
//
// What a deep offset costs, precisely, because it is the one thing here an
// unfriendly client controls: the walk stops at Offset+MaxLines lines or at the
// first entry, whichever comes first, so its ceiling is the transcript itself —
// which formatTranscriptTail has already read off disk and JSON-scanned line by
// line before this is called, so a deep page is a constant factor on work the
// request was always going to do, not a new order of it. The offset cannot
// outrun that: maxPreviewOffset caps it at the HTTP boundary, the only door
// paging comes through (the other LoadPreview callers — the inspector and the
// kill confirmation — never set one).
//
// Memory is bounded to the page instead, which is what the walk's ceiling
// cannot do: an entry whose lines all sit inside the newest Offset — the block
// dropTrailingLines discards below — is counted and then thrown away rather
// than carried, so building one page never holds the whole rendered transcript.
// Those entries are exactly the first ones walked (lines only grows as the walk
// goes older), so the discarded block is contiguous with the newest end and
// dropTrailingLines is left the remainder that is still present.
func transcriptPage(convo []transcriptEntry, limits PreviewLimits) string {
	want := -1 // MaxLines unset: no line ceiling, so render everything
	if limits.MaxLines > 0 {
		want = limits.Offset + limits.MaxLines
	}
	var rendered []string
	lines, dropped := 0, 0
	for i := len(convo) - 1; i >= 0 && (want < 0 || lines < want); i-- {
		var b strings.Builder
		renderEntry(&b, convo[i].Type, convo[i].Message)
		s := b.String()
		lines += countPreviewLines(s)
		if lines <= limits.Offset {
			dropped = lines // wholly inside the tail: the count is all we need
			continue
		}
		rendered = append(rendered, s)
	}
	var b strings.Builder
	for i := len(rendered) - 1; i >= 0; i-- { // back into transcript order
		b.WriteString(rendered[i])
	}
	return dropTrailingLines(b.String(), limits.Offset-dropped)
}

// dropTrailingLines removes the newest n lines from s, a line being everything
// through its "\n" (previewLines' notion, the same one limitPreview counts).
// Dropping as many lines as s has, or more, leaves "": paging past the start of
// history is an empty page, not the oldest line served over and over.
func dropTrailingLines(s string, n int) string {
	if n <= 0 || s == "" {
		return s
	}
	lines := previewLines(s)
	if n >= len(lines) {
		return ""
	}
	return strings.Join(lines[:len(lines)-n], "")
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

// tmuxLocForPID returns the full "session:win.pane" tmux location for the given
// pid by live discovery, or "" if the pid is not in a tmux pane.
func tmuxLocForPID(pid int) string {
	panes, _ := tmuxPaneMap()
	ppid, _ := ppidMap()
	pane, found := walkTmuxPane(pid, panes, ppid)
	if !found {
		return ""
	}
	return pane.Location
}

// tmuxSessionForPID returns the tmux session name (without :win.pane suffix)
// for the given pid, or "" if not in tmux.
func tmuxSessionForPID(pid int) string {
	loc := tmuxLocForPID(pid)
	if loc == "" {
		return ""
	}
	name, err := tmuxSessionName(loc)
	if err != nil {
		return ""
	}
	return name
}
