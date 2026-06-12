package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// PreviewContent returns a human-readable preview of the session's current
// activity. For tmux-hosted sessions: pixel-perfect tmux capture-pane.
// For others: a filtered tail of the JSONL transcript (user/assistant entries
// only — system/hook noise is skipped).
func PreviewContent(pid int) string {
	panes, _ := tmuxPaneMap()
	ppid, _ := ppidMap()
	if loc := walkTmuxPane(pid, panes, ppid); loc != "" {
		out, err := exec.Command("tmux", "capture-pane", "-p", "-e", "-t", loc).Output()
		if err != nil {
			return fmt.Sprintf("tmux capture-pane failed: %v\n", err)
		}
		return bold("source: ") + "tmux pane " + loc + "\n\n" + string(out)
	}

	sess, ok := readSessionByPID(pid)
	if !ok || sess.SessionID == "" {
		return "session file missing sessionId\n"
	}
	home, _ := os.UserHomeDir()
	path := findTranscript(home, sess.SessionID)
	if path == "" {
		return fmt.Sprintf("no transcript found for session %s\n", sess.SessionID)
	}
	return bold("source: ") + "transcript tail  " + dim(path) + "\n\n" +
		formatTranscriptTail(path, 8)
}

// formatTranscriptTail reads the JSONL transcript and renders the last N
// user/assistant entries.
func formatTranscriptTail(path string, n int) string {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Sprintf("open transcript: %v\n", err)
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
		return "(no user/assistant entries in transcript)\n"
	}
	if len(convo) > n {
		convo = convo[len(convo)-n:]
	}

	var b strings.Builder
	for _, e := range convo {
		renderEntry(&b, e.Type, e.Message)
	}
	return b.String()
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
