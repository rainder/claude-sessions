package main

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// ticketIDRe matches a ClickUp custom id like DR-860. \b is an ASCII word
// boundary in Go's RE2 engine, so this rejects ADR-123 (no boundary between
// "A" and "D") and DR-123abc (no boundary between "3" and "a") — a bare
// `DR-\d+` would have matched both.
// Digit run is 1-6 (not 2-6 as the naive form might suggest) — a single-
// digit ticket like DR-1 is a real, if early, id and must still match.
var ticketIDRe = regexp.MustCompile(`\bDR-\d{1,6}\b`)

// detectTicketID looks for a ClickUp ticket id in the worktree directory
// basename first (the authoritative source — pickup-next-ticket names
// worktrees by ticket id), falling back to the session name. "" if neither
// matches.
func detectTicketID(cwd, name string) string {
	if wt := worktreeName(cwd); wt != "" {
		if id := ticketIDRe.FindString(wt); id != "" {
			return id
		}
	}
	return ticketIDRe.FindString(name)
}

const (
	ticketSummaryInstruction = "what is being fixed? short version like i am 25"
	ticketRawCap             = 4000 // chars of raw `cu fetch` text kept on a summarization failure
)

// fetchTicketSummary runs `cu fetch --with-comments <id>` piped into
// `claude -p` to produce a short plain-English summary. Three outcomes:
//   - cu fetch fails: returns a non-nil error (the caller's asyncSection
//     surfaces this as "unavailable" — there's no raw text to fall back to).
//   - cu succeeds but claude fails: returns the raw cu output (truncated)
//     with a "summary unavailable" note, and a nil error — the dialog never
//     blanks when only the LLM leg failed.
//   - both succeed: returns the trimmed summary.
func fetchTicketSummary(ctx context.Context, ticketID string) (PreviewResult, error) {
	raw, err := cuFetchFunc(ctx, ticketID)
	if err != nil {
		return PreviewResult{}, fmt.Errorf("cu fetch failed: %w", err)
	}
	summary, err := claudeSummarizeFunc(ctx, ticketSummaryInstruction, raw)
	if err != nil {
		content := fmt.Sprintf("[summary unavailable: %s]\n\n%s", err, trunc(string(raw), ticketRawCap))
		return PreviewResult{Source: "ticket", Label: ticketID, Content: content}, nil
	}
	return PreviewResult{Source: "ticket", Label: ticketID, Content: strings.TrimSpace(string(summary))}, nil
}
