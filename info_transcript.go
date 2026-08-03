// info_transcript.go
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
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
	IsMeta    bool   `json:"isMeta"`
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
		if e.IsMeta {
			continue
		}
		if e.Type != "user" && e.Type != "assistant" {
			continue
		}
		text, ok := extractTurnText(e.Message.Content)
		if !ok {
			continue
		}
		if e.Type == "user" && strings.HasPrefix(strings.TrimSpace(text), "<") {
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

const (
	conversationSummaryInstruction = "what's happening in this conversation right now? short version like i am 25"
	conversationTailTurns          = 5
	conversationTurnCap            = 1500           // chars kept per turn before piping
	conversationPromptCap          = 6000           // total chars fed to claude -p
	conversationCacheTTL           = 24 * time.Hour // safety-net upper bound; a content change already changes the cache key via mtime/size
	conversationCacheFailTTL       = 15 * time.Second
	conversationCacheFetchTimeout  = 20 * time.Second
	conversationCacheMax           = 256
)

var conversationCache = newSummaryCache(conversationCacheTTL, conversationCacheFailTTL, conversationCacheFetchTimeout, conversationCacheMax)

var errTranscriptNotFound = fmt.Errorf("transcript not found")

func formatTurnsForPrompt(turns []transcriptTurn) string {
	var b strings.Builder
	for _, t := range turns {
		fmt.Fprintf(&b, "%s: %s\n\n", t.Role, trunc(t.Text, conversationTurnCap))
	}
	s := b.String()
	if len(s) > conversationPromptCap {
		s = s[:conversationPromptCap]
	}
	return s
}

// summarizeTurns pipes turns into claude -p to produce a short summary.
// Shared by the local and remote conversation pipelines (Task 8) — neither
// cares how the turns were obtained.
func summarizeTurns(ctx context.Context, turns []transcriptTurn) (PreviewResult, error) {
	if len(turns) == 0 {
		return PreviewResult{Source: "conversation", Content: "(no conversation turns yet)"}, nil
	}
	input := formatTurnsForPrompt(turns)
	summary, err := claudeSummarizeFunc(ctx, conversationSummaryInstruction, []byte(input))
	if err != nil {
		return PreviewResult{}, fmt.Errorf("summarize conversation: %w", err)
	}
	// summary text is untrusted (it's derived from transcript content the
	// user or another process wrote) and flows straight to the terminal
	// viewer, so it gets the same sanitization as preview.go's other
	// pipelines (see sanitizeTerminalText's doc comment).
	return PreviewResult{Source: "conversation", Content: sanitizeTerminalText(strings.TrimSpace(string(summary)))}, nil
}

// conversationCacheKey identifies one revision of a session's transcript.
// host is "" for local sessions, the remote server name otherwise — without
// it, the same session id on two different hosts (unusual, but possible for
// a resumed transcript) would collide.
func conversationCacheKey(host, sessionID string, mtime time.Time, size int64) string {
	return host + "\x00" + sessionID + "\x00" + mtime.UTC().Format(time.RFC3339Nano) + "\x00" + fmt.Sprint(size)
}

// fetchConversationSummaryLocal reads the session's own transcript directly
// and summarizes it. The raw read (stat + extractConversationTail) happens
// on every call; only the expensive claude -p step is cached, keyed by the
// transcript's (mtime, size) so a new message invalidates the cache
// automatically.
func fetchConversationSummaryLocal(ctx context.Context, home, sessionID string) (PreviewResult, error) {
	path := findTranscript(home, sessionID)
	if path == "" {
		return PreviewResult{}, errTranscriptNotFound
	}
	st, err := os.Stat(path)
	if err != nil {
		return PreviewResult{}, fmt.Errorf("stat transcript: %w", err)
	}
	key := conversationCacheKey("", sessionID, st.ModTime(), st.Size())
	return conversationCache.getOrFetch(ctx, key, func(fetchCtx context.Context) (PreviewResult, error) {
		turns, err := extractConversationTail(path, conversationTailTurns)
		if err != nil {
			return PreviewResult{}, fmt.Errorf("read transcript: %w", err)
		}
		return summarizeTurns(fetchCtx, turns)
	})
}
