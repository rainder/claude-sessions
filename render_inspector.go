package main

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Inspector layout floor. Below either dimension the fullscreen preview cannot
// lay out its five fixed rows (title, metadata, separator, at least one body
// line, footer) or fit the footer's "Back  Refresh  Follow" strip (21 cols), so
// the renderer degrades to a single "terminal too small" notice plus a Back
// control instead of drawing a corrupted frame.
const (
	minInspectorRows = 5
	minInspectorCols = 24
)

// footerLabels are the inspector's clickable controls, laid left-to-right with a
// two-space gap. Their order fixes both the visible columns and the hit-region
// ordering returned to the render loop. It also fixes which label gets dropped
// first when width is tight: inspectorFooter's drop logic always trims from the
// end (footerLabels[:len(footerLabels)-1]), so whichever entry is added last is
// the least essential one and must be the last in this slice — a 5th label
// added anywhere but the end would get dropped in Compose's place.
var footerLabels = []struct {
	text   string
	action hitAction
}{
	{"Back", hitInspectorBack},
	{"Refresh", hitInspectorRefresh},
	{"Follow", hitInspectorFollow},
	{"Compose", hitInspectorCompose},
}

const footerGap = "  "

// footerLeftWidth returns the visible column width of the first n footer
// labels (in footerLabels order) plus their separating gaps — the same
// arithmetic inspectorFooter's render loop performs, factored out so it can
// be checked against cols before deciding which labels to draw.
func footerLeftWidth(n int) int {
	w := 0
	for i := 0; i < n && i < len(footerLabels); i++ {
		if i > 0 {
			w += len(footerGap)
		}
		w += len(footerLabels[i].text)
	}
	return w
}

// inspectorTicketSummaryMaxLines bounds the ticket block drawn above the body —
// heading included — so a long summary can never crowd out the session output
// the inspector exists to show.
const inspectorTicketSummaryMaxLines = 4

// RenderInspector writes the fullscreen session inspector to w as plain lines
// (no cursor movement or alternate-screen escapes — RunTUI owns positioning) and
// returns the zero-based clickable footer regions for the frame it drew.
//
// Layout, top to bottom: a title (name, PID, host), a width-responsive metadata
// line (model, status, context, cost, cpu — dropping cpu below 93 cols, cost
// below 80, context below 64, everything but status below 48), a separator, an optional ticket
// summary block (summaryLines plus its own trailing separator, drawn only when
// summaryLines is non-empty — so the chrome budget is 4 rows plus
// inspectorSummaryExtraRows(summaryLines), not a fixed 4), the content body from
// view.top, and a footer whose left carries the Back/Refresh/Follow/Compose
// controls and whose right shows the source and live-freshness status when
// space allows (dropping Compose first if width is too tight for both — see
// inspectorFooter). Below the min-size floor it emits a concise "terminal too
// small" notice with a Back control. Every emitted line is clipped to cols.
//
// summaryLines is re-fit to rows here even though the caller already fit it: the
// clamp is idempotent, and a renderer that can draw a frame taller than rows —
// dropping the footer while still reporting hit regions on the last row — is not
// something to leave to a caller's discipline.
func RenderInspector(w io.Writer, view inspectorViewState, summaryLines []string, cols, rows int) []hitRegion {
	if rows < minInspectorRows || cols < minInspectorCols {
		return renderInspectorTooSmall(w, cols, rows)
	}

	summaryLines = inspectorSummaryFit(summaryLines, rows)
	snap := view.snapshot

	// Row 0: title.
	fmt.Fprintln(w, clipLine(inspectorTitle(snap), cols))
	// Row 1: metadata strip.
	fmt.Fprintln(w, clipLine(inspectorMetadata(snap, cols), cols))
	// Row 2: separator.
	fmt.Fprintln(w, clipLine(dim(strings.Repeat("-", cols)), cols))

	// Optional ticket summary block, closed by a separator of its own.
	if len(summaryLines) > 0 {
		for _, line := range summaryLines {
			fmt.Fprintln(w, clipLine(line, cols))
		}
		fmt.Fprintln(w, clipLine(dim(strings.Repeat("-", cols)), cols))
	}

	// Remaining rows above the footer: content body.
	bodyRows := rows - 4 - inspectorSummaryExtraRows(summaryLines)
	if bodyRows < 0 {
		bodyRows = 0
	}
	inspectorBody(w, view, bodyRows, cols)

	// Row rows-1: footer — the compose input bar while composing, otherwise
	// the normal Back/Refresh/Follow/Compose controls.
	if view.composing {
		return inspectorComposeBar(w, view, cols)
	}
	return inspectorFooter(w, view, cols, rows-1)
}

// inspectorTicketSummaryLines renders the ticket summary block shown above the
// inspector body: a bold heading plus the summary's own lines word-wrapped to
// cols, capped at inspectorTicketSummaryMaxLines with a dim "…" replacing the
// last kept line on overflow. Returns nil whenever there is nothing worth
// showing — still fetching, failed, or empty — because the inspector reserves no
// space for a summary it doesn't have: no placeholder, no spinner.
func inspectorTicketSummaryLines(ticketSummary overlayPreview, cols int) []string {
	if !ticketSummary.Loaded || ticketSummary.Err != nil {
		return nil
	}
	content := trimTrailingBlank(ticketSummary.Lines)
	if len(content) == 0 {
		return nil
	}

	heading := "ticket:"
	if ticketSummary.Label != "" {
		heading = "ticket: " + ticketSummary.Label + ":"
	}
	out := []string{bold(heading)}
	// Blank lines pass through unwrapped, matching infoDialogSectionLines.
	for _, line := range content {
		if line == "" {
			out = append(out, "")
			continue
		}
		out = append(out, wrapRunes(line, cols)...)
	}
	if len(out) > inspectorTicketSummaryMaxLines {
		out = out[:inspectorTicketSummaryMaxLines]
		out[inspectorTicketSummaryMaxLines-1] = dim("…")
	}
	return out
}

// inspectorSummaryExtraRows is how many rows the summary block costs on top of
// RenderInspector's fixed chrome: its own lines plus the separator that closes
// it, or nothing at all when there is no summary.
func inspectorSummaryExtraRows(summaryLines []string) int {
	if len(summaryLines) == 0 {
		return 0
	}
	return len(summaryLines) + 1
}

// inspectorSummaryFit shortens the summary block to what rows can actually hold,
// keeping at least one body row — a frame taller than the terminal loses its
// footer to the screen renderer's truncation while inspectorFooter still reports
// hit regions on the last row, so the bottom row would click as "Back" without
// saying so. A block that can't keep its heading plus one content line is
// dropped entirely rather than rendered as a bare heading.
//
// It returns a prefix and nothing else, which is what makes it idempotent: both
// RunTUI's render (which must size the viewport from the same block it draws)
// and RenderInspector itself apply it.
func inspectorSummaryFit(summaryLines []string, rows int) []string {
	// rows - 4 - (n+1) >= 1  =>  n <= rows - 6
	allowed := rows - 6
	if allowed > len(summaryLines) {
		allowed = len(summaryLines)
	}
	if allowed < 2 {
		return nil
	}
	return summaryLines[:allowed]
}

// renderInspectorTooSmall draws the degraded notice: the message on the first
// line and a clickable Back control on the second, each clipped to cols. The
// returned hit region points at the Back label.
func renderInspectorTooSmall(w io.Writer, cols, rows int) []hitRegion {
	fmt.Fprintln(w, clipLine("terminal too small", cols))
	label := "Back"
	fmt.Fprintln(w, clipLine(label, cols))
	x1 := len(label) - 1
	if x1 > cols-1 {
		x1 = cols - 1
	}
	return []hitRegion{{x0: 0, y0: 1, x1: x1, y1: 1, action: hitInspectorBack}}
}

// inspectorTitle formats the header line: display name (bold when user-set,
// dimmed when auto-derived — matching how render.go's list rows treat the
// same DisplayName flag), PID, and the host when the session came from a
// remote server.
func inspectorTitle(snap InspectorSnapshot) string {
	name, dimmed := snap.Session.DisplayName()
	label := bold(name)
	if dimmed {
		label = dim(name)
	}
	title := label + "  PID " + strconv.Itoa(snap.Session.PID)
	if snap.Session.Host != "" {
		title += "  " + dim(snap.Session.Host)
	}
	return title
}

// inspectorMetadata formats the model/status/context/cost/cpu strip, dropping
// fields as the terminal narrows: below 93 cols cpu goes, below 80 cost goes,
// below 64 context goes, below 48 only the status remains.
func inspectorMetadata(snap InspectorSnapshot, cols int) string {
	s := snap.Session
	model := shortModel(s.Model)
	if model == "" {
		model = "-"
	}
	status := colorize(statusColor[s.Status], s.StatusDisplay())
	ctx := "ctx " + formatTokens(s.ContextTokens)
	cost := formatCost(s.CostUSD, s.CostSubagentsUSD)
	cpu := "cpu " + s.CPU
	if s.CPU != "" && s.CPU != "-" {
		cpu += "%"
	}

	var parts []string
	switch {
	case cols < 48:
		parts = []string{status}
	case cols < 64:
		parts = []string{model, status}
	case cols < 80:
		parts = []string{model, status, ctx}
	case cols < 93:
		parts = []string{model, status, ctx, cost}
	default:
		parts = []string{model, status, ctx, cost, cpu}
	}
	return strings.Join(parts, dim("  ·  "))
}

// inspectorBody writes exactly bodyRows content lines. When the snapshot has
// lines it renders them from view.top, blank-filling any remainder; when it has
// none it shows a single state placeholder (error, ended, loading, or a neutral
// "no output") so the pane never reads as inexplicably empty.
func inspectorBody(w io.Writer, view inspectorViewState, bodyRows, cols int) {
	if bodyRows < 0 {
		bodyRows = 0
	}
	lines := view.snapshot.Lines
	for i := 0; i < bodyRows; i++ {
		var text string
		if len(lines) == 0 {
			// Placeholder only on the first body row; the rest stay blank.
			if i == 0 {
				text = inspectorEmptyBody(view.snapshot)
			}
		} else if idx := view.top + i; idx >= 0 && idx < len(lines) {
			text = lines[idx]
		}
		fmt.Fprintln(w, clipLine(text, cols))
	}
}

// inspectorEmptyBody is the single placeholder line shown when no content has
// loaded, describing why: an error message takes precedence, then the terminal
// "session ended" state, then a loading spinner, then a neutral fallback.
func inspectorEmptyBody(snap InspectorSnapshot) string {
	switch {
	case snap.Error != "":
		return dim("error: " + snap.Error)
	case snap.Ended:
		return dim("session ended")
	case snap.Loading:
		return dim("loading…")
	default:
		return dim("(no output)")
	}
}

// inspectorFooter writes the final row and returns the hit regions for its
// controls. The Back/Refresh/Follow/Compose labels sit on the left at fixed
// columns (each clickable, Follow included even while already following); the
// source and freshness status sit right-aligned when the remaining width
// admits them. The footer-right text carries actionable status — the
// "no tmux pane" hint, a "sent" confirmation, or live/stale/ended freshness —
// so when width is too tight for both, Compose (the newest and least
// essential label; its 'i' keybinding still works with the button hidden) is
// dropped first to make room, mirroring the width-based prioritization
// inspectorMetadata uses above. Dropping Compose is conditional on it actually
// helping: at some widths the right text won't fit even with the 3-label
// footer, and dropping Compose there would lose the button for nothing, so it
// only drops when doing so crosses the fit threshold. Back/Refresh/Follow is
// the floor either way, never dropped further.
func inspectorFooter(w io.Writer, view inspectorViewState, cols, footerY int) []hitRegion {
	right := inspectorFooterRight(view)
	rightVis := utf8.RuneCountInString(right)

	labels := footerLabels
	fullW := footerLeftWidth(len(footerLabels))
	shortW := footerLeftWidth(len(footerLabels) - 1)
	// rightVis is 0 only if inspectorFooterRight ever returned "", which it
	// currently never does (inspectorStatusText's default case always yields
	// "LIVE ↓"); cols < fullW is kept as a defensive floor for that case
	// anyway, so the label line itself is never clipped mid-word even if a
	// future change made the right side legitimately empty. The second clause
	// is the active path: only drop Compose when the full label set doesn't
	// fit the right text (cols-fullW-rightVis < 2) AND dropping to 3 labels
	// actually creates room (cols-shortW-rightVis >= 2) — otherwise the right
	// text won't fit either way and Compose stays, since losing it would gain
	// nothing.
	if cols < fullW || (rightVis > 0 && cols-fullW-rightVis < 2 && cols-shortW-rightVis >= 2) {
		labels = footerLabels[:len(footerLabels)-1]
	}

	var left strings.Builder
	var hits []hitRegion
	col := 0
	for i, l := range labels {
		if i > 0 {
			left.WriteString(footerGap)
			col += len(footerGap)
		}
		start := col
		left.WriteString(l.text)
		col += len(l.text)
		hits = append(hits, hitRegion{
			x0: start, y0: footerY, x1: col - 1, y1: footerY,
			action: l.action,
		})
	}
	leftVis := col

	line := left.String()
	if rightVis > 0 && cols-leftVis-rightVis >= 2 {
		pad := cols - leftVis - rightVis
		line += strings.Repeat(" ", pad) + right
	} else if cols > leftVis {
		// Pad so the white bar spans the full width even without a right side.
		line += strings.Repeat(" ", cols-leftVis)
	}
	fmt.Fprintln(w, ansiPreviewBar+clipLine(line, cols)+ansiReset)
	return hits
}

// inspectorComposeBar draws the send-keys compose row in place of the normal
// footer while view.composing is true: a "> text_" prompt on the same
// reverse-video bar the footer uses, styled after new_picker.go's
// renderPromptInput (new_picker.go:221-230). No hit regions — clicking during
// compose is out of scope; the box only responds to the keyboard.
//
// composeStatus is appended whenever non-empty, with no composeStatusUntil
// expiry check: handleInspectorEvent (tui.go) sets it to "sending…" while
// composing is still true, and on a failed send leaves composing true (by
// design, so the text survives for a retry) with the failure message in
// composeStatus. Neither of those is ever otherwise visible, since
// inspectorFooter/inspectorFooterRight — the only other place composeStatus
// renders, with the expiry check — is skipped entirely while composing. The
// status stays until either the user's next submit attempt overwrites it with
// a new "sending…", or cancels out of compose entirely: handleInspectorCompose
// (tui_state.go) clears composeStatus/composeStatusUntil on Esc/Ctrl-C, since a
// cancelled compose has nothing left to retry and a stale failure message must
// not linger past it.
func inspectorComposeBar(w io.Writer, view inspectorViewState, cols int) []hitRegion {
	line := "> " + view.composeText + dim("_")
	if view.composeStatus != "" {
		line += "  " + dim(view.composeStatus)
	}
	fmt.Fprintln(w, ansiPreviewBar+clipLine(line, cols)+ansiReset)
	return nil
}

// inspectorFooterRight is the source-plus-freshness-status text shown on the
// footer's right when it fits, e.g. "tmux · LIVE ↓". A recent compose-status
// result (composeStatus, while composeStatusUntil hasn't passed) takes
// priority over the normal freshness text — but this path is only ever
// reached while composing is false, so it can only show the two statuses set
// under that condition: "sent" (a successful submit clears composing before
// setting it) and "no tmux pane" (armCompose's hint when 'i' is pressed on a
// session with no pane to send into, tui_state.go). Every other status —
// "sending…" and a failure message, both set while composing stays true so
// the buffer survives for a retry — is rendered inside inspectorComposeBar
// instead, since RenderInspector skips this footer entirely while composing.
// Once composeStatusUntil passes, this falls back to the ordinary freshness
// status.
func inspectorFooterRight(view inspectorViewState) string {
	if view.composeStatus != "" && time.Now().Before(view.composeStatusUntil) {
		return view.composeStatus
	}
	status := inspectorStatusText(view)
	src := view.snapshot.Source
	switch {
	case src != "" && status != "":
		return src + " · " + status
	case status != "":
		return status
	default:
		return src
	}
}

// inspectorStatusText is the freshness indicator, in priority order: a dead
// session outranks stale content, which outranks a paused (non-following)
// viewport, which outranks the initial load, with a live tail as the default.
func inspectorStatusText(view inspectorViewState) string {
	snap := view.snapshot
	switch {
	case snap.Ended:
		return "SESSION ENDED"
	case snap.Stale:
		return "STALE"
	case !view.follow:
		return fmt.Sprintf("PAUSED · %d new", view.newLines)
	case snap.Loading:
		return "LOADING"
	default:
		return "LIVE ↓"
	}
}
