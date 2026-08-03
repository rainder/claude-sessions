package main

import (
	"strings"
	"time"
)

func infoDialogHeader(s Session) []string {
	lines := []string{
		bold(s.Name),
		dirDisplay(s.CWD, s.Home, s.GitRoot),
	}
	if s.Host != "" {
		lines = append(lines, "host: "+s.Host)
	}
	updated := s.StartedAt
	if s.UpdatedAt != 0 {
		updated = s.UpdatedAt
	}
	lines = append(lines, "updated: "+time.UnixMilli(updated).Format("2006-01-02 15:04"))
	return lines
}

const (
	infoDialogInnerWidth = 72 // default box width, clamped down on a narrow terminal
	infoDialogChrome     = 6  // top/bottom border + padding rows, mirrors resumePromptsChrome's role
)

// infoDialogSectionLines renders one section's current state: a loading
// placeholder, an error line, or its word-wrapped content. title is shown
// only when the section has something to say (loading/error/content) so an
// omitted (nil) section produces no output at the call site.
func infoDialogSectionLines(title string, sec *asyncSection, width int) []string {
	if sec == nil {
		return nil
	}
	snap := sec.snapshot()
	var out []string
	out = append(out, bold(title+":"))
	switch {
	case !snap.Loaded:
		out = append(out, dim("loading…"))
	case snap.Err != nil:
		out = append(out, dim("unavailable: "+snap.Err.Error()))
	case len(snap.Lines) == 0:
		out = append(out, dim("(empty)"))
	default:
		text := strings.Join(snap.Lines, "\n")
		for _, l := range wrapRunes(text, width) {
			out = append(out, l)
		}
	}
	return out
}

// renderInfoDialog draws the bordered info box: header, then an optional
// ticket section, then the conversation section, separated by a divider
// line. Modeled on renderResumePromptsOverlay's geometry (box glyphs,
// clamp-to-terminal-width, trailing "…" on overflow) but built fresh — the
// preview.go single-block renderer (previewBlock/renderConfirmOverlay)
// isn't built for multiple independently word-wrapped sections.
func renderInfoDialog(header []string, ticketSec, convoSec *asyncSection, cols, rows int) string {
	innerWidth := infoDialogInnerWidth
	if cols > 0 {
		max := cols - 4
		if max < 1 {
			max = 1
		}
		if innerWidth > max {
			innerWidth = max
		}
	}

	divider := strings.Repeat(confirmBoxH, innerWidth)
	var body []string
	body = append(body, header...)
	if ticketSec != nil {
		body = append(body, divider)
		body = append(body, infoDialogSectionLines("ticket", ticketSec, innerWidth)...)
	}
	body = append(body, divider)
	body = append(body, infoDialogSectionLines("conversation", convoSec, innerWidth)...)

	if rows > 0 {
		capacity := rows - infoDialogChrome
		if capacity < 1 {
			capacity = 1
		}
		if capacity < len(body) {
			body = body[:capacity]
			body[capacity-1] = dim("…")
		}
	}

	// pad mirrors renderConfirmOverlay's own row-padding idiom exactly
	// (confirm_overlay.go:140-142): visualLen skips ANSI escape sequences
	// when measuring width, so bold()/dim() content still right-pads
	// correctly instead of counting escape bytes as visible characters.
	pad := func(s string) string {
		s = clipLine(s, innerWidth)
		return confirmBoxV + " " + s + strings.Repeat(" ", innerWidth-visualLen(s)) + " " + confirmBoxV
	}

	var b strings.Builder
	b.WriteString(confirmBoxTL + strings.Repeat(confirmBoxH, innerWidth+2) + confirmBoxTR + "\n")
	for _, line := range body {
		b.WriteString(pad(line) + "\n")
	}
	b.WriteString(confirmBoxBL + strings.Repeat(confirmBoxH, innerWidth+2) + confirmBoxBR + "\n")
	return b.String()
}
