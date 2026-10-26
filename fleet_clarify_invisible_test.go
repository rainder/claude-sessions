package main

import (
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// A clarify fork is `claude -p --fork-session` of the FLEET session. While it
// runs, Claude Code writes it a session file (entrypoint sdk-cli) and a
// transcript that copies the parent's custom-title, so by name alone it reads
// as a second FLEET. These tests pin that the existing headless filters hide
// it everywhere a row or a resumable session could come from (checked live on
// claude 2.1.310).

// TestClarifyForkSessionFileIsNotARow: the fork's session file never becomes a
// row, so findFleet still sees exactly one FLEET.
func TestClarifyForkSessionFileIsNotARow(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	allPIDsAlive(t)

	cwd := filepath.Join(home, "work", "trecs-brain")
	started := strconv.FormatInt(time.Now().UnixMilli(), 10)
	writeSessionFile(t, home, "4101", `{"pid":4101,"sessionId":"11111111-1111-4111-8111-111111111111","cwd":"`+cwd+`",`+
		`"kind":"interactive","entrypoint":"cli","name":"FLEET","nameSource":"user","status":"idle","startedAt":`+started+`}`)
	writeSessionFile(t, home, "4102", `{"pid":4102,"sessionId":"22222222-2222-4222-8222-222222222222","cwd":"`+cwd+`",`+
		`"kind":"interactive","entrypoint":"sdk-cli","name":"FLEET","nameSource":"user","status":"busy","startedAt":`+started+`}`)

	rows, err := CollectLocal()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].PID != 4101 {
		t.Fatalf("CollectLocal = %+v, want only pid 4101", rows)
	}
	got, why := findFleet(rows)
	if why != "" || got.PID != 4101 {
		t.Fatalf("findFleet = pid %d, %q; want pid 4101 and no reason", got.PID, why)
	}

	lite, err := CollectLocalLite()
	if err != nil {
		t.Fatal(err)
	}
	if len(lite) != 1 || lite[0].PID != 4101 {
		t.Fatalf("CollectLocalLite = %+v, want only pid 4101", lite)
	}
}

// TestClarifyForkTranscriptNotResumable: the fork's transcript, custom-title
// line included, is never offered by the resume picker's collector.
func TestClarifyForkTranscriptNotResumable(t *testing.T) {
	home := t.TempDir()
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	const forkID = "33333333-3333-4333-8333-333333333333"

	writeResumableTranscript(t, home, "proj", forkID, now.Add(-time.Minute),
		`{"type":"custom-title","customTitle":"FLEET","sessionId":"`+forkID+`"}`,
		`{"type":"user","entrypoint":"sdk-cli","cwd":"/home/u/trecs-brain","sessionId":"`+forkID+`","message":{"role":"user","content":"which option?"}}`,
		`{"type":"assistant","entrypoint":"sdk-cli","cwd":"/home/u/trecs-brain","sessionId":"`+forkID+`","message":{"role":"assistant","content":[{"type":"text","text":"option A"}]}}`,
	)
	// A normal interactive transcript beside it is still listed, so the test
	// cannot pass by the collector returning nothing at all.
	writeResumableTranscript(t, home, "proj", "44444444-4444-4444-8444-444444444444", now.Add(-2*time.Minute),
		`{"type":"user","entrypoint":"cli","cwd":"/home/u/trecs-brain","message":{"role":"user","content":"hello"}}`,
	)

	got := collectResumableFrom(home, map[string]bool{}, now)
	if len(got) != 1 || got[0].SessionID != "44444444-4444-4444-8444-444444444444" {
		t.Fatalf("collectResumableFrom = %+v, want only the interactive transcript", got)
	}
}
