package main

import "regexp"

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
