// info_transcript_test.go
package main

import "testing"

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
