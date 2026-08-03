// info_transcript.go
package main

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"strings"
)

// transcriptTurn is one user/assistant text turn. json tags are load-bearing:
// this type is also the wire shape for the /transcript-tail server response
// (Task 7) and its client decode (Task 8), not just a local value type.
type transcriptTurn struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

// infoTailBytes bounds how much of the transcript tail extractConversationTail
// scans, mirroring model.go's modelTailBytes/scanTranscript convention
// (seek-then-scan a fixed byte budget) rather than accumulating the whole
// file the way preview.go's formatTranscriptTail does.
const infoTailBytes = 256 * 1024

type transcriptLine struct {
	Type      string `json:"type"`
	RequestID string `json:"requestId"`
	Message   struct {
		ID      string          `json:"id"`
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// extractTurnText pulls plain text out of a message.content value, which is
// either a bare string or an array of content blocks. Only "text" blocks are
// kept — thinking/tool_use/tool_result are dropped. Returns ok=false if
// there's no usable text (e.g. a tool-only assistant turn).
func extractTurnText(contentRaw json.RawMessage) (string, bool) {
	var s string
	if err := json.Unmarshal(contentRaw, &s); err == nil {
		s = strings.TrimSpace(s)
		if s == "" {
			return "", false
		}
		return s, true
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(contentRaw, &blocks); err != nil {
		return "", false
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" {
			if t := strings.TrimSpace(b.Text); t != "" {
				parts = append(parts, t)
			}
		}
	}
	if len(parts) == 0 {
		return "", false
	}
	return strings.Join(parts, "\n"), true
}

// turnBuilder keeps only the most recent max turns, deduplicating assistant
// streaming re-emissions by dedupKey with last-wins semantics: a later
// re-emission of the same message.id+requestId overwrites the earlier one
// in place (same position), since it's typically the more complete text.
// This matches scanTranscript's "tracked independently (last wins)"
// convention (model.go:92-93) rather than cost.go's first-seen-wins, which
// is correct for accounting but wrong for picking the most complete text.
type turnBuilder struct {
	turns    []transcriptTurn
	idxByKey map[string]int
	max      int
}

func newTurnBuilder(max int) *turnBuilder {
	return &turnBuilder{idxByKey: make(map[string]int), max: max}
}

func (b *turnBuilder) add(role, text, dedupKey string) {
	if dedupKey != "" {
		if i, ok := b.idxByKey[dedupKey]; ok {
			b.turns[i].Text = text
			return
		}
	}
	b.turns = append(b.turns, transcriptTurn{Role: role, Text: text})
	if dedupKey != "" {
		b.idxByKey[dedupKey] = len(b.turns) - 1
	}
	if len(b.turns) > b.max {
		b.turns = b.turns[1:]
		for k, i := range b.idxByKey {
			if i == 0 {
				delete(b.idxByKey, k)
			} else {
				b.idxByKey[k] = i - 1
			}
		}
	}
}

// extractConversationTail returns the last n user/assistant text-only turns
// from the transcript at path. Seeks to the file's tail before scanning
// (see infoTailBytes) rather than accumulating the whole file.
func extractConversationTail(path string, n int) ([]transcriptTurn, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	seeked := false
	if st, err := f.Stat(); err == nil && st.Size() > infoTailBytes {
		if _, err := f.Seek(st.Size()-infoTailBytes, io.SeekStart); err == nil {
			seeked = true
		}
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	if seeked {
		scanner.Scan() // discard the partial first line
	}

	b := newTurnBuilder(n)
	for scanner.Scan() {
		var e transcriptLine
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			continue
		}
		if e.Type != "user" && e.Type != "assistant" {
			continue
		}
		text, ok := extractTurnText(e.Message.Content)
		if !ok {
			continue
		}
		dedupKey := ""
		if e.Type == "assistant" {
			dedupKey = e.Message.ID + "\x00" + e.RequestID
		}
		b.add(e.Type, text, dedupKey)
	}
	return b.turns, nil
}
