package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"
)

func infoDialogHeader(s Session) []string {
	lines := []string{
		bold(s.Name),
		dirDisplay(s.CWD, s.Home, s.GitRoot),
	}
	if s.Host != "" {
		lines = append(lines, "host: "+s.Host)
	}
	lines = append(lines, "updated: "+s.Updated().Format("2006-01-02 15:04"))
	return lines
}

const (
	infoDialogInnerWidth = 72 // default box width, clamped down on a narrow terminal
	// infoDialogChrome accounts for renderInfoDialog's own fixed, non-body
	// output rows: the top border line and the bottom border line (2 total).
	// The extra headroom above that covers rounding slack when clamping to a
	// narrow terminal height.
	infoDialogChrome = 6
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
	heading := title + ":"
	if snap.Label != "" {
		heading = title + ": " + snap.Label + ":"
	}
	out = append(out, bold(heading))
	switch {
	case !snap.Loaded:
		out = append(out, dim("loading…"))
	case snap.Err != nil:
		out = append(out, dim("unavailable: "+snap.Err.Error()))
	case len(trimTrailingBlank(snap.Lines)) == 0:
		out = append(out, dim("(empty)"))
	default:
		for _, line := range snap.Lines {
			if line == "" {
				out = append(out, "")
				continue
			}
			out = append(out, wrapRunes(line, width)...)
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

// showInfoDialog opens the 'i'-hotkey info modal for s: a deterministic
// header, an optional ticket summary (only if a DR-XXXX id is detected),
// and a conversation summary. Modeled directly on resumePromptsOverlay
// (resume.go:954-972) — same self-contained renderer/decoder/loop shape,
// same close-key set.
func showInfoDialog(s Session, wakes []wakeFD) {
	header := infoDialogHeader(s)
	ticketID := detectTicketID(s.CWD, s.Name)

	var ticketSec *asyncSection
	if ticketID != "" {
		ticketSec = startAsyncSection("ticket", func(ctx context.Context) (PreviewResult, error) {
			return fetchTicketSummaryCached(ctx, ticketID)
		})
	}

	convoSec := startAsyncSection("conversation", func(ctx context.Context) (PreviewResult, error) {
		if s.Host == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return PreviewResult{}, fmt.Errorf("resolve home dir: %w", err)
			}
			return fetchConversationSummaryLocal(ctx, home, s.SessionID)
		}
		return fetchConversationSummaryRemote(ctx, s.Host, s.SessionID)
	})

	defer func() {
		ticketSec.close() // nil-safe when no ticket id was detected
		convoSec.close()
	}()

	renderer := newScreenRenderer(os.Stdout)
	decoder := newInputDecoder()
	fd := int(os.Stdin.Fd())

	for {
		cols, rows, err := term.GetSize(fd)
		if err != nil {
			cols, rows = 0, 0
		}
		_ = renderer.Draw(renderInfoDialog(header, ticketSec, convoSec, cols, rows), cols, rows)
		// modalWakesWithAll takes *asyncSection directly (post-Task-9-fix-round
		// signature) — nil-safe, so ticketSec being nil here needs no guard.
		modalW := modalWakesWithAll(wakes, ticketSec, convoSec)
		keys, _ := readModalEvents(decoder, modalW)
		for _, key := range keys {
			if resumePromptsClose(key) {
				return
			}
		}
	}
}

// actInfo is the action-handler wrapper showInfoDialog needs to fit this
// codebase's established convention (actKill/actAttach/actResume all take
// *actCtx and resolve the selected row via c.selected() — see actions.go).
// A no-op when nothing is selected, same as those.
func actInfo(c *actCtx) {
	s := c.selected()
	if s == nil {
		return
	}
	showInfoDialog(*s, c.modalWakes)
}
