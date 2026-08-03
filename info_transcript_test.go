// info_transcript_test.go
package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestExtractConversationTailBasic(t *testing.T) {
	p := writeTranscript(t,
		`{"type":"user","message":{"role":"user","content":"first question"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"first answer"}]}}`,
		`{"type":"user","message":{"role":"user","content":"second question"}}`,
	)
	turns, err := extractConversationTail(p, 5)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	want := []transcriptTurn{
		{Role: "user", Text: "first question"},
		{Role: "assistant", Text: "first answer"},
		{Role: "user", Text: "second question"},
	}
	if len(turns) != len(want) {
		t.Fatalf("got %d turns, want %d: %+v", len(turns), len(want), turns)
	}
	for i := range want {
		if turns[i] != want[i] {
			t.Errorf("turn %d = %+v, want %+v", i, turns[i], want[i])
		}
	}
}

func TestExtractConversationTailStripsToolBlocks(t *testing.T) {
	p := writeTranscript(t,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"thinking","thinking":"pondering"},{"type":"tool_use","name":"Bash","input":{}},{"type":"text","text":"the real answer"}]}}`,
	)
	turns, err := extractConversationTail(p, 5)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(turns) != 1 || turns[0].Text != "the real answer" {
		t.Errorf("got %+v, want single turn with only the text block", turns)
	}
}

func TestExtractConversationTailKeepsOnlyLastN(t *testing.T) {
	var lines []string
	for i := 0; i < 8; i++ {
		lines = append(lines, `{"type":"user","message":{"role":"user","content":"msg"}}`)
	}
	p := writeTranscript(t, lines...)
	turns, err := extractConversationTail(p, 3)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(turns) != 3 {
		t.Errorf("got %d turns, want 3", len(turns))
	}
}

func TestExtractConversationTailDedupesLastWins(t *testing.T) {
	p := writeTranscript(t,
		`{"type":"assistant","requestId":"r1","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"partial"}]}}`,
		`{"type":"assistant","requestId":"r1","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"partial complete now"}]}}`,
	)
	turns, err := extractConversationTail(p, 5)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("got %d turns, want 1 (deduped)", len(turns))
	}
	if turns[0].Text != "partial complete now" {
		t.Errorf("Text = %q, want the later (more complete) re-emission to win", turns[0].Text)
	}
}

func TestExtractConversationTailSkipsNonTextEntries(t *testing.T) {
	p := writeTranscript(t,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Bash"}]}}`,
		`{"type":"user","message":{"role":"user","content":"real question"}}`,
	)
	turns, err := extractConversationTail(p, 5)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(turns) != 1 || turns[0].Text != "real question" {
		t.Errorf("got %+v, want only the real user turn", turns)
	}
}

func TestExtractConversationTailMissingFile(t *testing.T) {
	_, err := extractConversationTail("/nonexistent/path.jsonl", 5)
	if err == nil {
		t.Fatal("want error for missing file")
	}
}

// TestExtractConversationTailSeeksPastLeadingJunk writes a transcript larger
// than infoTailBytes and asserts the scan starts near the tail: text from
// the very first line (well before the seek point) must not survive, while
// the last few real turns must.
//
// This test has to actually discriminate between "seek then scan" and "scan
// everything, let FIFO eviction do the work" — those two implementations
// produce identical output if the only real (countable) turns in the file
// are the head marker plus a handful of tail turns and n is large enough
// that eviction never triggers. So the padding between the head marker and
// the tail markers is built entirely out of lines the turn-filter excludes
// regardless of seeking (type != "user"/"assistant"), and n is set well
// above the real turn count. If the seek logic were deleted entirely, the
// scan would still reach the head marker (a real, countable "user" turn)
// and it would show up in the result — only the seek prevents that.
func TestExtractConversationTailSeeksPastLeadingJunk(t *testing.T) {
	lines := []string{
		`{"type":"user","message":{"role":"user","content":"HEAD-MARKER-UNIQUE-TEXT first line"}}`,
	}
	// Filler uses type "system", which extractConversationTail's turn filter
	// excludes unconditionally (e.Type != "user" && e.Type != "assistant") —
	// so it never becomes a counted turn whether or not the seek runs, and
	// can't be mistaken for eviction doing the seek's job.
	fillerLine := `{"type":"system","message":{"role":"system","content":"` + strings.Repeat("x", 2000) + `"}}`
	for len(strings.Join(lines, "\n")) < infoTailBytes+64*1024 {
		lines = append(lines, fillerLine)
	}
	lines = append(lines,
		`{"type":"user","message":{"role":"user","content":"TAIL-MARKER-1"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"TAIL-MARKER-2"}]}}`,
		`{"type":"user","message":{"role":"user","content":"TAIL-MARKER-3"}}`,
	)
	p := writeTranscript(t, lines...)

	// n=10 is comfortably larger than the real turn count (4: the head
	// marker plus 3 tail turns), so FIFO eviction never engages either way —
	// only the seek can be responsible for the head marker's absence.
	turns, err := extractConversationTail(p, 10)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	for _, tu := range turns {
		if strings.Contains(tu.Text, "HEAD-MARKER-UNIQUE-TEXT") {
			t.Fatalf("head marker survived the seek: %+v", turns)
		}
	}
	for _, want := range []string{"TAIL-MARKER-1", "TAIL-MARKER-2", "TAIL-MARKER-3"} {
		var found bool
		for _, tu := range turns {
			if strings.Contains(tu.Text, want) {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected %s in result, got %+v", want, turns)
		}
	}
}

// TestExtractConversationTailEvictionAndDedupTogether combines more than n
// distinct turns (forcing eviction) with a repeated message.id+requestId
// (forcing dedup) in a single scan.
func TestExtractConversationTailEvictionAndDedupTogether(t *testing.T) {
	var lines []string
	for i := 0; i < 6; i++ {
		lines = append(lines, fmt.Sprintf(`{"type":"user","message":{"role":"user","content":"msg %d"}}`, i))
	}
	lines = append(lines,
		`{"type":"assistant","requestId":"r1","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"partial"}]}}`,
		`{"type":"user","message":{"role":"user","content":"interleaved"}}`,
		`{"type":"assistant","requestId":"r1","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"partial complete now"}]}}`,
	)
	p := writeTranscript(t, lines...)

	turns, err := extractConversationTail(p, 4)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(turns) != 4 {
		t.Fatalf("got %d turns, want 4 (window size after eviction+dedup)", len(turns))
	}
	var sawDeduped bool
	for _, tu := range turns {
		if tu.Role == "assistant" {
			if tu.Text != "partial complete now" {
				t.Errorf("assistant turn = %+v, want the deduped later re-emission", tu)
			}
			sawDeduped = true
		}
	}
	if !sawDeduped {
		t.Errorf("expected deduped assistant turn in result, got %+v", turns)
	}
	for _, tu := range turns {
		if tu.Text == "msg 0" || tu.Text == "msg 1" {
			t.Errorf("expected earliest turns evicted, found %+v in %+v", tu, turns)
		}
	}
}

// TestExtractConversationTailEmptyFile pins the empty-input contract: zero
// lines returns an empty (non-panicking) slice with no error.
func TestExtractConversationTailEmptyFile(t *testing.T) {
	p := writeTranscript(t)
	turns, err := extractConversationTail(p, 5)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(turns) != 0 {
		t.Errorf("got %d turns, want 0 for empty file", len(turns))
	}
}

// TestExtractConversationTailSkipsMetaAndWrapperText covers Finding 1: an
// isMeta:true entry and a user entry whose text starts with "<" (loop /
// skill-injection / command-wrapper noise) must both be excluded, while a
// real turn survives.
func TestExtractConversationTailSkipsMetaAndWrapperText(t *testing.T) {
	p := writeTranscript(t,
		`{"type":"user","isMeta":true,"message":{"role":"user","content":"re-injected loop prompt, run again"}}`,
		`{"type":"user","message":{"role":"user","content":"<command-message>some wrapper text</command-message>"}}`,
		`{"type":"user","message":{"role":"user","content":"a real question"}}`,
	)
	turns, err := extractConversationTail(p, 5)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(turns) != 1 || turns[0].Text != "a real question" {
		t.Errorf("got %+v, want only the real user turn", turns)
	}
}
