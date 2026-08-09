package main

import (
	"fmt"
	"io"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// maxNameCellWidth caps the NAME column so one long session name can't blow
// out the whole table's layout.
const maxNameCellWidth = 20

// ANSI escape sequences.
const (
	ansiReset      = "\033[0m"
	ansiBold       = "\033[1m"
	ansiDim        = "\033[2m"
	ansiInvert     = "\033[7m"
	ansiSelectedBG = "\033[48;5;8m"
	// ansiPreviewBar makes the inspector footer unmistakable (black on white):
	// a preview looks exactly like an attached session otherwise.
	ansiPreviewBar = "\033[30;47m"
)

// statusColor maps the raw `status` field to an ANSI SGR code.
var statusColor = map[string]string{
	"busy":    "1;31",
	"shell":   "1;36",
	"waiting": "1;33",
	"idle":    "2",
}

// statusGlyph is the single-char indicator used in the minimal view.
var statusGlyph = map[string]string{
	"busy":    "●",
	"shell":   "$",
	"waiting": "!",
	"idle":    "·",
}

func colorize(code, s string) string {
	if code == "" {
		return s
	}
	return "\033[" + code + "m" + s + ansiReset
}

func bold(s string) string { return ansiBold + s + ansiReset }
func dim(s string) string  { return ansiDim + s + ansiReset }

func highlightSelectedRow(row string, selected bool) string {
	if !selected {
		return row
	}
	row = strings.ReplaceAll(row, ansiReset, ansiReset+ansiSelectedBG)
	return ansiSelectedBG + row + ansiReset
}

func disabledRail(session Session, selected bool) string {
	if !session.Disabled {
		return "  "
	}
	if selected {
		return "\033[33m–\033[39m "
	}
	return colorize("33", "–") + " "
}

// noTmuxMarker flags a session that is not running inside tmux — worth
// surfacing in the compact views because it can't be reattached to once the
// terminal that spawned it closes. Blank for a tmux-backed session, since
// that's the unremarkable default; dim rather than alarm-colored, since this
// is a note, not a warning. plain drops the marker's own color entirely, for
// the same reason gv.badge has a badgePlain style: a self-contained dim+reset
// pair nested inside an outer dim() (headless rows) would reset the dim
// mid-row, and a selected row carries its distinction via background, not an
// embedded dim.
func noTmuxMarker(session Session, plain bool) string {
	if session.Tmux != "" {
		return "  "
	}
	if plain {
		return "∅ "
	}
	return dim("∅") + " "
}

func sessionRowPlain(session Session, selected bool) bool {
	return session.Headless() || (session.Disabled && !selected)
}

// groupSGR is the fixed per-group badge palette (SGR codes), 1..9.
var groupSGR = map[int]string{
	1: "36", 2: "35", 3: "33", 4: "32", 5: "34", 6: "31", 7: "96", 8: "95", 9: "97",
}

// groupView carries the client-side view state threaded through the render
// path: the sessionID→group map (for badges and the filter predicate), the
// active group filter (zero value = no filter), the free-text query (empty =
// no text filter), the hideDisabled toggle, and the per-frame first-column
// slot reservations. The group filter, the query, and hideDisabled all
// compose (AND) in filterSessionRows / filterRemoteResults.
// showBadge, showRail and showNoTmux each gate one 2-char indicator slot
// (group badge, disabled rail, not-in-tmux marker), and are set by
// BuildTableFrame once it knows which slots at least one visible session
// needs. A slot is present on every row of the frame (headers included) or on
// none. The zero value applies no filter and hides every slot — rendering
// exactly as before groups, the text filter, and conditional slots existed.
// showNoTmux is only ever set for the intermediate/minimal views — the full
// view already spells out tmux state in its own TMUX column, so it never
// reserves this slot.
type groupView struct {
	groups       map[string]int
	filter       groupFilter
	query        string
	hideDisabled bool
	showBadge    bool
	showRail     bool
	showNoTmux   bool
}

// groupOf returns the group assigned to s (1..9), or 0 for an ungrouped session
// or one with no stable SessionID.
func (gv groupView) groupOf(s Session) int {
	if s.SessionID == "" {
		return 0
	}
	return gv.groups[s.SessionID]
}

// passesGroupFilter reports whether a session survives the active group filter.
// filterNone admits everything. In filterOnly a session is visible iff its
// stored group's bit is set in mask (ungrouped sessions, incl. those with no
// SessionID, are hidden). In filterHide a session is visible iff it is
// ungrouped or its bit is not set (ungrouped sessions always survive).
func passesGroupFilter(s Session, groups map[string]int, filter groupFilter) bool {
	group := 0
	if s.SessionID != "" {
		group = groups[s.SessionID]
	}
	switch filter.mode {
	case filterOnly:
		return group != 0 && groupMaskHas(filter.mask, group)
	case filterHide:
		return group == 0 || !groupMaskHas(filter.mask, group)
	default:
		return true
	}
}

// matchesTextFilter reports whether session s (rendered under section host)
// satisfies the free-form text query. The query is split on spaces into tokens
// and every token must match (AND); a token matches when it is a
// case-insensitive substring of at least one searchable field — the display
// name, cwd, section host name, or tmux session name (the part before ":"). An
// empty (or all-whitespace) query matches everything. Pure so it is
// table-testable.
func matchesTextFilter(s Session, host, query string) bool {
	tokens := strings.Fields(query)
	if len(tokens) == 0 {
		return true
	}
	name, _ := s.DisplayName()
	tmuxName := s.Tmux
	if i := strings.IndexByte(tmuxName, ':'); i >= 0 {
		tmuxName = tmuxName[:i]
	}
	fields := []string{name, s.CWD, host, tmuxName}
	for i, f := range fields {
		fields[i] = strings.ToLower(f)
	}
	for _, tok := range tokens {
		tok = strings.ToLower(tok)
		found := false
		for _, f := range fields {
			if strings.Contains(f, tok) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// groupBadgeGlyph returns the circled digit for a group (U+2460 is ①), or "" for
// an ungrouped group.
func groupBadgeGlyph(group int) string {
	if group < 1 || group > 9 {
		return ""
	}
	return string(rune(0x2460 + group - 1))
}

// badge renders the 2-char badge slot for a row. It returns "" when the slot is
// not reserved (byte-identical to the pre-groups layout), two spaces for an
// ungrouped row when the slot is reserved, or the colored circled digit + space
// otherwise. style selects how the glyph is wrapped so it composes cleanly with
// the surrounding dim/selected treatment in decorateSessionRow.
type badgeStyle uint8

const (
	badgeColored badgeStyle = iota // stand-alone colored token (selected/bright rows)
	badgeDim                       // self-contained dim token (disabled non-selected rows)
	badgePlain                     // bare glyph, wrapped by an outer dim() (headless rows)
)

func (gv groupView) badge(s Session, style badgeStyle) string {
	if !gv.showBadge {
		return ""
	}
	group := gv.groupOf(s)
	glyph := groupBadgeGlyph(group)
	if glyph == "" {
		return "  "
	}
	token := glyph + " "
	switch style {
	case badgeDim:
		return dim(token)
	case badgePlain:
		return token
	default:
		return colorize(groupSGR[group], token)
	}
}

func decorateSessionRow(session Session, selected bool, body string, gv groupView) string {
	// The rail and no-tmux slots reserve their 2 cells only when this frame
	// reserves them, otherwise they collapse to nothing (byte-empty, never
	// spaces) so the row body sits flush. The badge already self-gates on
	// gv.showBadge inside gv.badge.
	rail := ""
	if gv.showRail {
		rail = disabledRail(session, selected)
	}
	noTmuxFor := func(plain bool) string {
		if !gv.showNoTmux {
			return ""
		}
		return noTmuxMarker(session, plain)
	}

	var row string
	switch {
	case selected:
		row = gv.badge(session, badgeColored) + rail + noTmuxFor(true) + body
	case session.Disabled:
		row = gv.badge(session, badgeDim) + rail + noTmuxFor(false) + dim(body)
	case session.Headless():
		row = dim(gv.badge(session, badgePlain) + rail + noTmuxFor(true) + body)
	default:
		row = gv.badge(session, badgeColored) + rail + noTmuxFor(false) + body
	}
	return highlightSelectedRow(row, selected)
}

// usageColor maps a rate-limit utilization percentage to an SGR code:
// default below 70%, yellow 70–89%, red at 90%+.
func usageColor(pct float64) string {
	switch {
	case pct >= 90:
		return "1;31"
	case pct >= 70:
		return "33"
	default:
		return ""
	}
}

// loadSeverity maps a load average to an SGR code the same way usageColor
// maps a rate-limit percentage: default below 0.7 of cores, yellow 0.7–0.99,
// red at 1.0+ (saturated). cores <= 0 means the host didn't report a core
// count (older remote server, unsupported OS), so severity is unknowable —
// the caller renders uncolored rather than guessing.
func loadSeverity(load float64, cores int) string {
	if cores <= 0 {
		return ""
	}
	switch ratio := load / float64(cores); {
	case ratio >= 1.0:
		return "1;31"
	case ratio >= 0.7:
		return "33"
	default:
		return ""
	}
}

// usageBar renders a width-cell block bar for pct (clamped to 0–100). The
// unfilled track is dimmed so it doesn't visually compete with the fill.
func usageBar(pct float64, width int) string {
	filled := int(pct*float64(width)/100 + 0.5)
	if filled < 0 {
		filled = 0
	}
	if filled > width {
		filled = width
	}
	bar := strings.Repeat("█", filled)
	if filled < width {
		bar += dim(strings.Repeat("░", width-filled))
	}
	return bar
}

// formatUntil → time left until t, largest unit only: "<1m", "42m", "2h", "3d".
// A zero or already-past t returns "" instead of "<1m" — a bucket's ResetsAt
// only reads that way once its reset has already happened, and since carried
// numbers stay on screen with no age bound (see liveCarryable/carryable), a
// carry stale by hours would otherwise show a permanent, misleading "resets
// in under a minute" beside its dim "stale" marker. Mirrors codexSegs' own
// "better a blank than a misleading '<1m' countdown" rule, extended from
// "unset" to "unset or elapsed".
func formatUntil(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Until(t)
	if d <= 0 {
		return ""
	}
	if d < time.Minute {
		return "<1m"
	}
	mins := int(d.Minutes())
	switch {
	case mins < 60:
		return fmt.Sprintf("%dm", mins)
	case mins < 24*60:
		return fmt.Sprintf("%dh", mins/60)
	default:
		return fmt.Sprintf("%dd", mins/(24*60))
	}
}

// moneyGrouped renders a minor-unit amount as whole major units with
// thousands separators: 2550¢ → "26", 112345¢ → "1,123". Cent precision is
// deliberately dropped — the header needs magnitude, not an invoice.
func moneyGrouped(minor float64, places int) string {
	scale := 1.0
	for i := 0; i < places; i++ {
		scale *= 10
	}
	n := int64(minor/scale + 0.5)
	digits := fmt.Sprintf("%d", n)
	var b strings.Builder
	if n < 0 {
		b.WriteByte('-')
		digits = digits[1:]
	}
	for i := 0; i < len(digits); i++ {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteByte(digits[i])
	}
	return b.String()
}

// usageBarMax is the widest a header bar gets; narrow terminals shrink bars
// down to usageBarMin before the line gets clipped.
const (
	usageBarMax = 10
	usageBarMin = 4
)

// writeUsage prints one Anthropic account rate-limit line under the title, or
// nothing when usage data isn't available (nil). All buckets share the line:
// "5h <bar> 42% 2h   wk <bar> 13% 3d   Fable <bar> 10% 5d   cr <bar> 5% $1,123".
// The trailing figure is the time remaining until that bucket resets (rate
// limits) or the amount of credits spent (extra usage). The model-scoped weekly
// segment (labeled with the model's display name, e.g. "Fable") only appears
// when the account has such a limit; the credits segment only appears when extra
// usage is enabled on the account. It builds the Anthropic segments (claudeSegs)
// and renders them at this line's own affordable bar width (lineBarW) — the
// standalone path; the header block instead shares one bar width across lines
// (see writeUsageHeader).
func writeUsage(w io.Writer, label string, u *UsageInfo, cols int) {
	if u == nil {
		return
	}
	segs := claudeSegs(u)
	renderUsageSegs(w, label, segs, lineBarW(label, segs, 0, cols))
}

// claudeSegs builds the Anthropic header segments for a non-nil snapshot: the 5h
// and weekly buckets, the model-scoped weekly segment when the account has one,
// and the credits segment when extra usage is enabled.
func claudeSegs(u *UsageInfo) []usageSeg {
	segs := []usageSeg{
		{label: "5h", trailer: formatUntil(u.FiveHour.ResetsAt), pct: u.FiveHour.Pct},
		{label: "wk", trailer: formatUntil(u.SevenDay.ResetsAt), pct: u.SevenDay.Pct},
	}
	if u.WeeklyScopedLabel != "" {
		segs = append(segs, usageSeg{
			label:   u.WeeklyScopedLabel,
			trailer: formatUntil(u.WeeklyScoped.ResetsAt),
			pct:     u.WeeklyScoped.Pct,
		})
	}
	if c := u.Credits; c.Enabled && c.Limit > 0 {
		sym := c.Currency
		if sym == "" || sym == "USD" {
			sym = "$"
		}
		segs = append(segs, usageSeg{
			label:   "cr",
			trailer: sym + moneyGrouped(c.Used, c.DecimalPlaces),
			pct:     c.Pct(),
		})
	}
	return segs
}

// usageSeg is one labeled bar segment on a header usage line: a short label, its
// utilization percentage, and a trailer (a reset countdown, or a credits
// figure). Provider-agnostic — writeUsage (Anthropic) and writeCodexUsage
// (Codex) each build their own segments and hand them to renderUsageSegs.
type usageSeg struct {
	label, trailer string
	pct            float64
	// trailerW is a minimum display width for the trailer column, so the segment
	// after it lines up across a header's lines when trailers differ in width
	// ("2h" vs "<1m"). 0 means just len(trailer); writeUsageHeader sets it per
	// column, standalone callers leave it zero (unpadded). Never widens a line's
	// last segment (renderUsageSegs skips the pad there — no trailing whitespace).
	trailerW int
}

// segTrailerWidth is the display width a segment's trailer content occupies (0
// when empty and unpadded). A non-last segment is padded to trailerW; the line's
// last segment is never padded, so it uses its own length regardless of trailerW.
func segTrailerWidth(s usageSeg, isLast bool) int {
	w := len(s.trailer)
	if !isLast && s.trailerW > w {
		w = s.trailerW
	}
	return w
}

// segTrailerText renders a segment's trailer portion: a separator space plus the
// dim trailer padded (outside the dim escape) to segTrailerWidth. An empty
// trailer whose column still has width — a Codex window with no reset time in a
// non-last, padded column — occupies that width in plain spaces so later columns
// stay aligned. Zero width (empty and unpadded, or an empty last segment) emits
// nothing.
func segTrailerText(s usageSeg, isLast bool) string {
	w := segTrailerWidth(s, isLast)
	if w == 0 {
		return ""
	}
	if s.trailer == "" {
		return strings.Repeat(" ", 1+w)
	}
	return " " + dim(s.trailer) + strings.Repeat(" ", w-len(s.trailer))
}

// lineBarW computes the bar width one header line can afford: the terminal width
// (cols) minus everything fixed — the padded label prefix, the segment labels,
// percentages, trailers (at their padded width, see segTrailerWidth), any suffix
// hung off the end (suffixW, 0 for a line with none), and inter-segment
// separators — split across the segments, clamped to usageBarMin..usageBarMax.
// cols <= 0 (unknown width, or no segments) yields usageBarMax. label is the
// already-padded label ("" for a bare line).
func lineBarW(label string, segs []usageSeg, suffixW, cols int) int {
	if cols <= 0 || len(segs) == 0 {
		return usageBarMax
	}
	fixed := usageLineFixedWidth(label, segs, suffixW)
	barW := usageBarMax
	if b := (cols - fixed) / len(segs); b < barW {
		barW = b
	}
	if barW < usageBarMin {
		barW = usageBarMin
	}
	return barW
}

// usageLineFixedWidth is everything on a header usage line except the bars
// themselves: the padded label prefix, each segment's label/percent/trailer,
// the inter-segment separators, and suffixW — the display width of anything the
// caller hangs off the end (the stale marker; 0 when there is none). Shared by
// lineBarW (sizing the bars) and writeUsageHeader's label-hiding check (does the
// line fit at all, even at usageBarMin, with the label kept).
//
// The suffix takes part in sizing even though it takes part in no alignment:
// leaving it out let a stale line size its bars to the full width and then lose
// the marker to the render loop's clip (cropTableFrame), which is the one thing
// the marker exists to prevent — carried-forward numbers reading as live.
func usageLineFixedWidth(label string, segs []usageSeg, suffixW int) int {
	prefixW := 0
	if label != "" {
		prefixW = len(label) + 1
	}
	fixed := prefixW + suffixW + 3*(len(segs)-1) // prefix + suffix + inter-segment separators
	for i, s := range segs {
		fixed += len(s.label) + 1 + 1 + len(fmt.Sprintf("%3.0f%%", s.pct))
		if tw := segTrailerWidth(s, i == len(segs)-1); tw > 0 {
			fixed += 1 + tw // separator + (possibly padded) trailer
		}
	}
	return fixed
}

// renderUsageSegs writes one header usage line: an optional dim label prefix
// followed by the segments, each "<label> <bar> <pct>% <trailer>", joined by
// three spaces, every bar barW cells wide. The caller sizes barW — lineBarW for
// a standalone line, or the shared minimum across a header block so every line's
// bars are the same width (see writeUsageHeader).
//
// label is an optional dim account prefix (email local-part, host name, or the
// "claude"/"codex" provider tag); "" renders the bare line. Trailing pad spaces
// (from padding every label to a common width) sit outside the dim escape so
// they don't leave a visible dim tail. Trailers are padded to their column width
// (segTrailerText) so later columns line up across lines; the last segment is
// never padded, so a line never ends in trailing whitespace. Empty segs writes
// nothing.
func renderUsageSegs(w io.Writer, label string, segs []usageSeg, barW int) {
	if line := usageSegsLine(label, segs, barW); line != "" {
		fmt.Fprintln(w, line)
	}
}

// usageSegsLine is renderUsageSegs' line, unwritten, so a caller can append to
// it. Empty segs yields "" (which renderUsageSegs turns back into writing
// nothing). writeUsageHeader uses this to hang the stale marker off the end of
// a line: the last segment is the one place nothing pads or aligns, so a suffix
// there can't shift a column on any other line. Exempt from alignment is not
// exempt from sizing — the caller still owes the suffix's width to
// usageLineFixedWidth, or the clip eats it.
func usageSegsLine(label string, segs []usageSeg, barW int) string {
	if len(segs) == 0 {
		return ""
	}
	parts := make([]string, len(segs))
	for i, s := range segs {
		part := fmt.Sprintf("%s %s %3.0f%%",
			s.label,
			colorize(usageColor(s.pct), usageBar(s.pct, barW)),
			s.pct)
		part += segTrailerText(s, i == len(segs)-1)
		parts[i] = part
	}
	return usageLinePrefix(label) + strings.Join(parts, "   ")
}

// usageLinePrefix builds a header usage line's dim label prefix: the label plus
// one separator space, with any padding spaces the caller appended left outside
// the dim escape so they don't leave a visible dim tail. An empty label yields
// an empty prefix, which is what keeps a bare line byte-identical to the
// pre-label layout.
func usageLinePrefix(label string) string {
	if label == "" {
		return ""
	}
	core := strings.TrimRight(label, " ")
	pad := len(label) - len(core)
	return dim(core) + " " + strings.Repeat(" ", pad)
}

// usageExpiredText is the placeholder a known account whose snapshot token no
// longer works renders instead of bars. It is classifyUsageErr's 401/403 tag,
// so the wording the user reads and the state the code branches on are one
// string.
const usageExpiredText = usageExpiredReason

// usageStaleText marks a line whose bars are carried forward from an earlier
// poll because the latest one failed. Short and dim on purpose: the numbers are
// still worth reading, and the marker only has to stop them passing as live.
const usageStaleText = "stale"

// renderUsagePlaceholder writes one header line for an account with no numbers
// to show: the same dim label prefix every other line gets, then dim text where
// the bars would be — "auth expired" for a dead credential, or the failure's
// reason ("rate limited", "unreachable", "bad snapshot", …) otherwise. Unlike
// a live account with no data, this line is never dropped — the account's
// existence is the point, and which of those two things happened is what tells
// the user whether to act.
func renderUsagePlaceholder(w io.Writer, label, text string) {
	fmt.Fprintln(w, usageLinePrefix(label)+dim(text))
}

// accountUsageLine is one resolved header account line: a usage snapshot with
// the dim label to prefix it with. dedupeAccounts produces these in display
// order; writeUsageHeader turns each into a writeUsage line.
type accountUsageLine struct {
	label string // email local-part, or host name for an unknown account
	email string // full account email ("" for an unknown account); disambiguates a label collision
	info  *UsageInfo
	// mine marks a line attributable to this machine's account: the local
	// entry itself, or a remote sharing the local email. Only such a line may
	// render bare (unlabeled) when it's the sole survivor — a lone foreign
	// remote must keep its label or it masquerades as the local account.
	// Always false for a known-account (pass 2) line: its numbers come from a
	// credential snapshot some host stashed, not from that account's live
	// session, so rendering it bare would let snapshot data pass as the local
	// account's current state — the same masquerade this flag exists to stop.
	mine bool
	// stale marks a line whose info was carried forward from an earlier poll:
	// a pass-2 snapshot account whose latest attempt failed transiently, or a
	// pass-1 live account whose latest pass was skipped outright by an armed
	// backoff. It still renders bars — old numbers beat no numbers — plus a dim
	// marker so they can't pass as live.
	stale bool
	// placeholder is the dim text a known account with no numbers renders
	// instead of bars: "auth expired" for a dead credential (the actionable
	// one — claude-switch into it and log in again), or whatever reason the
	// failed attempt classified as otherwise ("rate limited", "bad snapshot",
	// …). Empty means a normal segment line. Such
	// a line is deliberately never dropped, so the account's existence stays
	// visible either way.
	placeholder string
}

// knownAccountPlaceholder is the dim text a known account renders instead of
// bars, or "" when it has numbers to show. Order matters: an entry can carry
// both a Reason and carried-forward numbers (that is exactly what a stale entry
// is), and checking Info first is what keeps its bars.
func knownAccountPlaceholder(k KnownAccountUsage) string {
	switch {
	case k.Expired:
		return usageExpiredText
	case k.Info == nil && k.Reason != "":
		return k.Reason
	default:
		return ""
	}
}

// knownAccountRank scores a known-account line for dedup: lower is healthier.
// Used so a duplicate email across hosts keeps the best available reading
// instead of whichever host's entry was added first — see dedupeAccounts.
func knownAccountRank(l accountUsageLine) int {
	switch {
	case l.info != nil && !l.stale:
		return 0 // fresh numbers
	case l.info != nil && l.stale:
		return 1 // carried-forward numbers
	default:
		return 2 // no numbers at all (expired, or a failure with nothing to carry)
	}
}

// accountLocalPart is the label for a known account: the part before "@"
// (johndoe@example.com → "johndoe"), or the whole string when there's no "@".
func accountLocalPart(email string) string {
	if i := strings.IndexByte(email, '@'); i >= 0 {
		return email[:i]
	}
	return email
}

// dedupeAccounts resolves which account usage lines the header shows and in
// what order: local first, then remotes in config order. Entries whose Info is
// nil (never-fetched, or an older server that doesn't report usage) are
// dropped. Known accounts dedupe by lowercased email so several hosts on one
// account collapse to a single line — first occurrence wins, keeping local's
// snapshot when it shares the account. Unknown accounts ("" email: an identity
// read error, or a pre-propagation server) never dedupe — each keeps its own
// line keyed by host so two anonymous hosts don't merge. Each surviving line
// carries a dim label for the multi-account header: the email's local-part for
// a known account, the host name for an unknown one (local's fallback is
// "local", since the function has no name for the current machine). Two
// distinct accounts that happen to share a local-part (andy@trecs.aero,
// andy@avisoma.com) would otherwise render identical, indistinguishable
// labels, so a final pass promotes any colliding known-account labels to the
// full email. A live line whose numbers were carried forward (AccountUsage.Stale
// — newUsageFetcher re-serving through a throttle) keeps its bars and takes the
// same dim "stale" marker a snapshot line does; it is purely a marker, and
// changes neither which line wins an email collision nor what mine means.
//
// A second pass then appends the accounts no host is currently logged into: the
// credential snapshots claude-switch left behind (localKnown, then each
// remote's KnownAccounts, in config order). A snapshot entry is dropped when
// pass 1 already produced a line for its email — a live reading always beats
// any host's snapshot copy of the same account, which is what keeps account X
// live on host B from being shadowed by host A's stale snapshot of X. The
// survivors dedupe among themselves the same way (unknown-email entries key by
// snapshot name instead), and a failed one keeps its line rather than vanishing:
// an Expired entry as the dim "auth expired" placeholder, a transient one with
// nothing to carry as a dim placeholder naming its Reason ("rate limited",
// "bad snapshot", …), and a Stale one as its carried-forward bars plus the dim
// "stale" marker. Only the fourth shape is dropped — no Info, no Expired and no
// Reason, which describes every remote account now that GET /usage reports
// identity only and never fetches numbers on a remote caller's behalf (see
// addKnown). A remote host's own account bars therefore never appear in the
// header; only its heading label does (Usage.Account, populated for free).
func dedupeAccounts(local AccountUsage, localKnown []KnownAccountUsage, remotes []RemoteResult) []accountUsageLine {
	var lines []accountUsageLine
	seen := make(map[string]bool)
	add := func(account, host string, info *UsageInfo, stale, isLocal bool) {
		if info == nil {
			return
		}
		mine := isLocal || (account != "" && strings.EqualFold(account, local.Account))
		if account == "" {
			lines = append(lines, accountUsageLine{label: host, info: info, mine: mine, stale: stale})
			return
		}
		key := strings.ToLower(account)
		if seen[key] {
			return
		}
		seen[key] = true
		lines = append(lines, accountUsageLine{label: accountLocalPart(account), email: account, info: info, mine: mine, stale: stale})
	}
	add(local.Account, "local", local.Info, local.Stale, true)
	for _, r := range remotes {
		if r.Usage != nil {
			add(r.Usage.Account, r.Name, r.Usage.Info, r.Usage.Stale, false)
		}
	}
	// Pass 2: known (snapshot-derived) accounts. knownSeen maps each key (namespaced
	// so a snapshot name can never collide with an email key) to that key's line
	// index, so a later, healthier duplicate from another host can replace an
	// earlier host's worse reading of the same account instead of being dropped
	// silently — first-added-wins would otherwise let one host's chronic failure
	// permanently shadow another host's successful fetch of the identical account.
	knownSeen := make(map[string]int)
	addKnown := func(k KnownAccountUsage) {
		if !k.Expired && k.Info == nil && k.Reason == "" {
			// Nothing to show and no placeholder asked for. A remote host's
			// known accounts always look like this now — GET /usage reports
			// bare identity, never numbers — so the picker still keeps them
			// as switch targets (accountRowsFrom reads KnownAccounts
			// directly) without sprouting a header line saying nothing.
			return
		}
		key := "name\x00" + k.Name
		if k.Account != "" {
			if seen[strings.ToLower(k.Account)] {
				return // a live line already covers this account
			}
			key = "email\x00" + strings.ToLower(k.Account)
		}
		label := k.Name
		if k.Account != "" {
			label = accountLocalPart(k.Account)
		}
		candidate := accountUsageLine{
			label:       label,
			email:       k.Account,
			info:        k.Info,
			stale:       k.Stale,
			placeholder: knownAccountPlaceholder(k),
		}
		if idx, ok := knownSeen[key]; ok {
			if knownAccountRank(candidate) < knownAccountRank(lines[idx]) {
				lines[idx] = candidate
			}
			return
		}
		knownSeen[key] = len(lines)
		lines = append(lines, candidate)
	}
	for _, k := range localKnown {
		addKnown(k)
	}
	for _, r := range remotes {
		for _, k := range r.KnownAccounts {
			addKnown(k)
		}
	}
	labelCount := make(map[string]int)
	for _, l := range lines {
		labelCount[l.label]++
	}
	for i, l := range lines {
		if l.email != "" && labelCount[l.label] > 1 {
			lines[i].label = l.email
		}
	}
	return lines
}

// writeUsageHeader prints the header's account rate-limit line(s) for both
// providers — Anthropic lines first, then Codex — with their bars vertically
// aligned in two respects: every line's dim label is padded to one width shared
// across both blocks (rune-counted) so the first segment ("5h"/"wk"/…) starts in
// the same column, and every line's bars are the same width. A shorter Codex
// line (fewer/narrower segments) would otherwise afford wider bars than a Claude
// line the terminal has shrunk, so the shared width is the minimum any line can
// afford (clamped usageBarMin..usageBarMax); on a wide terminal that's
// usageBarMax for all.
//
// Anthropic labeling: a sole line attributable to this machine (mine) renders
// bare — byte-for-byte the pre-Codex layout — when there's no Codex block, but
// takes the dim "claude" tag once a Codex block shares the header, symmetric
// with "codex". A lone foreign remote keeps its account label so its limits
// can't masquerade as local; several lines each carry their account (local-part
// / host / full email on collision).
//
// Codex labeling: a sole mine line is the bare "codex" tag; every other line is
// "codex <account>". The same anti-masquerade carve-out applies — a lone foreign
// remote keeps its account so "codex" alone can't imply the local account.
//
// The pad spaces sit outside the dim escape (see renderUsageSegs); a bare ""
// label pads to nothing and stays byte-identical to the pre-Codex bare layout.
// Empty (no usage for either provider) writes nothing.
func writeUsageHeader(w io.Writer, accounts []accountUsageLine, codexAccounts []codexAccountLine, cols int) {
	type entry struct {
		label string
		segs  []usageSeg
		// placeholder lines ("auth expired", "rate limited", …) carry no
		// segments and render as dim text; they take part in label-width
		// alignment but in none of the bar/trailer sizing, which is all defined
		// over segments.
		placeholder string
		// suffix hangs off the end of a segment line (the stale marker), and
		// suffixW is its display width (the escapes in suffix are not width).
		// It takes part in no alignment — the last segment is never padded, so
		// nothing else moves because of it — but it does take part in sizing:
		// bars that fill the whole terminal would leave the marker to be clipped
		// away, and an unmarked stale line reads as a live one.
		suffix  string
		suffixW int
	}
	var entries []entry
	codexPresent := len(codexAccounts) > 0

	addClaude := func(label string, a accountUsageLine) {
		if a.placeholder != "" {
			entries = append(entries, entry{label: label, placeholder: a.placeholder})
			return
		}
		if a.info == nil {
			return
		}
		if segs := claudeSegs(a.info); len(segs) > 0 {
			suffix, suffixW := "", 0
			if a.stale {
				suffix, suffixW = " "+dim(usageStaleText), 1+len(usageStaleText)
			}
			entries = append(entries, entry{label: label, segs: segs, suffix: suffix, suffixW: suffixW})
		}
	}
	addCodex := func(label string, info *CodexUsageInfo) {
		if info == nil {
			return
		}
		if segs := codexSegs(info); len(segs) > 0 {
			entries = append(entries, entry{label: label, segs: segs})
		}
	}

	// Anthropic block. Only a live line may go bare — dedupeAccounts never marks
	// a snapshot-derived line mine, so an unlabeled line always means "this
	// machine's account, right now".
	if len(accounts) == 1 && accounts[0].mine {
		label := ""
		if codexPresent {
			label = "claude"
		}
		addClaude(label, accounts[0])
	} else {
		for _, a := range accounts {
			addClaude(a.label, a)
		}
	}

	// Codex block.
	if len(codexAccounts) == 1 && codexAccounts[0].mine {
		addCodex("codex", codexAccounts[0].info)
	} else {
		for _, a := range codexAccounts {
			addCodex("codex "+a.label, a.info)
		}
	}

	if len(entries) == 0 {
		return
	}

	// One label width across both blocks so the first segment lands in the same
	// column on every line.
	labelW := 0
	for _, e := range entries {
		if n := utf8.RuneCountInString(e.label); n > labelW {
			labelW = n
		}
	}
	// Pad the trailer of each segment column to the widest trailer any line has at
	// that column, so the segment after it stays aligned across lines when
	// trailers differ in width ("2h" vs "<1m"). Column = plain segment index; a
	// line's last segment is never padded (renderUsageSegs skips it), so this only
	// widens columns something actually follows. A single line has nothing to
	// align against, so skip it entirely (keeps the sole bare Claude line
	// byte-identical to the pre-Codex layout).
	if len(entries) > 1 {
		maxSegs := 0
		for _, e := range entries {
			if len(e.segs) > maxSegs {
				maxSegs = len(e.segs)
			}
		}
		for col := 0; col < maxSegs; col++ {
			tw := 0
			for _, e := range entries {
				if col < len(e.segs) {
					if n := len(e.segs[col].trailer); n > tw {
						tw = n
					}
				}
			}
			for _, e := range entries {
				if col < len(e.segs) {
					e.segs[col].trailerW = tw
				}
			}
		}
	}
	// Drop the label column when it's what's forcing bars below usageBarMin: a
	// line with a label whose fixed width (at the smallest bar) still overflows
	// cols is unreadable either way, so free the label's width for the
	// bars/percent/trailer instead of clipping. Checked against the padded labelW
	// (every entry pays for the widest label) since that's the width
	// writeUsageHeader actually renders.
	//
	// Placeholder lines (no segs) keep their label through this: they have no
	// bars to make room for, and a bare "auth expired" names no account — two
	// expired accounts would render as two identical, unattributable lines,
	// which defeats the point of keeping the line at all. So a shed leaves them
	// wider than the segmented lines by label+separator, deliberately: on a
	// terminal this narrow, knowing *whose* account expired is worth more than
	// the columns it costs.
	if cols > 0 && labelW > 0 {
		pad := strings.Repeat(" ", labelW)
		for _, e := range entries {
			if len(e.segs) == 0 {
				continue // placeholder line: no bars to make room for
			}
			if fixed := usageLineFixedWidth(pad, e.segs, e.suffixW); fixed+len(e.segs)*usageBarMin > cols {
				labelW = 0
				for i := range entries {
					if len(entries[i].segs) > 0 {
						entries[i].label = ""
					}
				}
				break
			}
		}
	}
	// Bars must be the same width on every line, so size them to the narrowest
	// line's affordable width (computed against the padded labels, the trailers
	// and any end-of-line suffix) and render all lines with it. cols <= 0 leaves
	// every line at usageBarMax.
	padded := make([]string, len(entries))
	barW := usageBarMax
	for i, e := range entries {
		// A placeholder that kept its label through a shed is wider than labelW
		// (which the shed zeroed), so the pad can go negative — it just gets none.
		pad := labelW - utf8.RuneCountInString(e.label)
		if pad < 0 {
			pad = 0
		}
		padded[i] = e.label + strings.Repeat(" ", pad)
		if b := lineBarW(padded[i], e.segs, e.suffixW, cols); b < barW {
			barW = b
		}
	}
	for i, e := range entries {
		if e.placeholder != "" {
			renderUsagePlaceholder(w, padded[i], e.placeholder)
			continue
		}
		// The suffix rides along only when there is a line to hang it off; a
		// segment-less entry can never emit a bare marker.
		if line := usageSegsLine(padded[i], e.segs, barW); line != "" {
			fmt.Fprintln(w, line+e.suffix)
		}
	}
}

// writeCodexUsage prints one Codex account usage line: a dim label prefix
// followed by one bar segment per rate-limit window the endpoint reported (5h /
// wk / mo …, see codexWindowLabel). Bars, colors, percent formatting, and the
// dim reset trailer match writeUsage. It renders at this line's own affordable
// bar width (lineBarW) — the standalone path; the header block instead shares
// one bar width across lines (see writeUsageHeader). Nil info or an account with
// no windows writes nothing.
func writeCodexUsage(w io.Writer, label string, info *CodexUsageInfo, cols int) {
	if info == nil {
		return
	}
	segs := codexSegs(info)
	if len(segs) == 0 {
		return
	}
	renderUsageSegs(w, label, segs, lineBarW(label, segs, 0, cols))
}

// codexSegs builds one segment per Codex rate-limit window; a window with no
// reset time gets an empty trailer, same as any other — formatUntil itself
// returns "" for a zero (or elapsed) ResetsAt now.
func codexSegs(info *CodexUsageInfo) []usageSeg {
	segs := make([]usageSeg, 0, len(info.Windows))
	for _, win := range info.Windows {
		segs = append(segs, usageSeg{label: win.Label, trailer: formatUntil(win.ResetsAt), pct: win.Pct})
	}
	return segs
}

// codexAccountLine is one resolved Codex header line, mirroring accountUsageLine
// for the Codex provider (see dedupeCodexAccounts).
type codexAccountLine struct {
	label string // email local-part, or host name for an unknown account
	email string // full account email ("" unknown); disambiguates a label collision
	info  *CodexUsageInfo
	// mine marks a line attributable to this machine's Codex account: the local
	// entry, or a remote sharing the local email (which merges into local
	// anyway). Only such a line may render as a bare "codex" tag when it's the
	// sole survivor — a lone foreign remote keeps its account label or its
	// limits masquerade as the local account's, mirroring accountUsageLine.mine.
	mine bool
}

// dedupeCodexAccounts resolves which Codex usage lines the header shows and in
// what order, mirroring dedupeAccounts exactly for the Codex provider: local
// first then remotes in config order; nil-Info entries dropped; known accounts
// deduped by lowercased email (first/local wins); unknown ("" email) accounts
// never merge and are keyed by host; colliding known-account local-parts
// promoted to the full email. The only differences from dedupeAccounts are the
// snapshot type and the remote source field (r.CodexUsage, not r.Usage).
func dedupeCodexAccounts(local CodexAccountUsage, remotes []RemoteResult) []codexAccountLine {
	var lines []codexAccountLine
	seen := make(map[string]bool)
	add := func(account, host string, info *CodexUsageInfo, isLocal bool) {
		if info == nil {
			return
		}
		mine := isLocal || (account != "" && strings.EqualFold(account, local.Account))
		if account == "" {
			lines = append(lines, codexAccountLine{label: host, info: info, mine: mine})
			return
		}
		key := strings.ToLower(account)
		if seen[key] {
			return
		}
		seen[key] = true
		lines = append(lines, codexAccountLine{label: accountLocalPart(account), email: account, info: info, mine: mine})
	}
	add(local.Account, "local", local.Info, true)
	for _, r := range remotes {
		if r.CodexUsage != nil {
			add(r.CodexUsage.Account, r.Name, r.CodexUsage.Info, false)
		}
	}
	labelCount := make(map[string]int)
	for _, l := range lines {
		labelCount[l.label]++
	}
	for i, l := range lines {
		if l.email != "" && labelCount[l.label] > 1 {
			lines[i].label = l.email
		}
	}
	return lines
}

// LocalUsage carries this machine's own account usage for each provider,
// threaded into the renderer so each provider's snapshot is deduped against its
// remotes' before the header draws. A nil LocalUsage, or a nil field, renders no
// bars for that provider — the Codex field is independent of the Anthropic one.
type LocalUsage struct {
	Claude *AccountUsage
	Codex  *CodexAccountUsage
	// KnownAccounts is this machine's own claude-switch credential snapshots'
	// usage — every account it knows about except the one currently logged in
	// (that one is Claude). Nil before the first known-accounts poll lands, or
	// on a machine with no snapshots.
	KnownAccounts []KnownAccountUsage
}

// formatTokens renders a context-token count compactly: 0 → "-", under 1k as
// plain digits, thousands as "124k" (rounded), millions as "1.2M".
func formatTokens(n int) string {
	switch {
	case n <= 0:
		return "-"
	case n < 1000:
		return fmt.Sprintf("%d", n)
	case n < 1_000_000:
		return fmt.Sprintf("%dk", (n+500)/1000)
	default:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	}
}

// contextWindow is the assumed model context limit used to color the CTX
// column. Flat 300k; per-model limits aren't tracked in the session file.
const contextWindow = 300_000

// ctxCell right-aligns the formatted token count in 5 columns and colors it
// by context utilization (usageColor thresholds: yellow ≥70%, red ≥90%).
// plain skips the color for rows dimmed as a whole (no embedded resets).
func ctxCell(ctxStr string, tokens int, plain bool) string {
	cell := fmt.Sprintf("%5s", ctxStr)
	if plain || tokens <= 0 {
		return cell
	}
	return colorize(usageColor(float64(tokens)/contextWindow*100), cell)
}

// dollars formats a single dollar amount: "$1.23" below $100, "$123" (cents
// dropped) at $100+ to keep the column narrow.
func dollars(c float64) string {
	if c < 100 {
		return fmt.Sprintf("$%.2f", c)
	}
	return fmt.Sprintf("$%.0f", c)
}

// formatCost renders a session's cost for the table: "—" when both parts are
// zero, otherwise the parent-transcript cost with a " (+$x.xx)" subagent
// suffix. The suffix is omitted when the subagent part rounds to under a cent.
func formatCost(main, subagents float64) string {
	if main+subagents <= 0 {
		return "—"
	}
	s := dollars(main)
	if subagents >= 0.005 {
		s += " (+" + dollars(subagents) + ")"
	}
	return s
}

// costCell right-aligns a formatted cost in a column of the given width. The
// "—" placeholder is a single display column despite being three bytes, so the
// padding is computed on rune count rather than byte length.
func costCell(cost string, width int) string {
	pad := width - utf8.RuneCountInString(cost)
	if pad < 0 {
		pad = 0
	}
	return strings.Repeat(" ", pad) + cost
}

// formatAge → "30s", "5m", "2h", "3d".
func formatAge(seconds float64) string {
	if seconds < 0 {
		seconds = 0
	}
	switch {
	case seconds < 60:
		return fmt.Sprintf("%ds", int(seconds))
	case seconds < 3600:
		return fmt.Sprintf("%dm", int(seconds/60))
	case seconds < 86400:
		return fmt.Sprintf("%dh", int(seconds/3600))
	default:
		return fmt.Sprintf("%dd", int(seconds/86400))
	}
}

// squashPath shortens each path component except the last to the first letter
// of each hyphen/underscore-separated word.
//
//	~/Developer/trecs-brain/src/dir → ~/D/tb/s/dir
func squashPath(p string) string {
	if p == "" || p == "/" {
		return p
	}
	parts := strings.Split(p, "/")
	if len(parts) <= 1 {
		return p
	}
	abbrev := func(s string) string {
		if s == "" || s == "~" {
			return s
		}
		bits := strings.FieldsFunc(s, func(r rune) bool { return r == '-' || r == '_' })
		if len(bits) == 0 {
			return string(s[0])
		}
		var b strings.Builder
		for _, x := range bits {
			if x != "" {
				b.WriteByte(x[0])
			}
		}
		return b.String()
	}
	head := make([]string, len(parts)-1)
	for i, x := range parts[:len(parts)-1] {
		head[i] = abbrev(x)
	}
	return strings.Join(append(head, parts[len(parts)-1]), "/")
}

// minDirW is the floor the DIR column shrinks to on a narrow terminal before
// marquee scrolling takes over; a squashed path below this is unreadable anyway.
const minDirW = 16

// marqueeCell renders s into a cell exactly width columns wide. When s fits it
// is left-aligned and space-padded (static). When it overflows, the window
// bounces: it holds at the start for marqueePause steps, slides right one rune
// per step until the tail is visible, holds again, then slides back — so both
// ends of the path get dwell time. Rune-safe and ANSI-free: the caller applies
// any color after slicing so a whole-row dim (or future highlight) survives.
func marqueeCell(s string, width, offset int) string {
	if width <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= width {
		return s + strings.Repeat(" ", width-len(r))
	}
	// d is the slide distance; one full cycle is hold(P) → out(d) → hold(P)
	// → back(d-1), so the period is 2P+2d-1 and pos walks 0..d..1.
	const marqueePause = 3
	d := len(r) - width
	period := 2*marqueePause + 2*d - 1
	t := ((offset % period) + period) % period
	var pos int
	switch {
	case t < marqueePause:
		pos = 0
	case t < marqueePause+d:
		pos = t - marqueePause + 1
	case t < 2*marqueePause+d:
		pos = d
	default:
		pos = 2*marqueePause + 2*d - 1 - t
	}
	return string(r[pos : pos+width])
}

// shrinkDirW reduces dirW so a row of total visible width lineW fits within
// cols, never dropping below minDirW. cols <= 0 (unknown terminal width) leaves
// dirW untouched. lineW must have been measured with the current dirW.
func shrinkDirW(dirW, lineW, cols int) int {
	if cols <= 0 {
		return dirW
	}
	if over := lineW - cols; over > 0 {
		// The floor never exceeds the starting width: a column already
		// narrower than minDirW must not be widened by the clamp.
		floor := min(minDirW, dirW)
		if dirW -= over; dirW < floor {
			dirW = floor
		}
	}
	return dirW
}

// displayCWD collapses a path's own collector $HOME prefix to "~". Each row
// carries the home of the host that produced it (Session.Home), so local and
// remote rows both collapse against the correct home; an empty home (rows from
// older servers that don't report it) is left absolute. The prefix match
// requires an exact home or a "home/" boundary so a sibling like
// /home/andy-other never collapses against /home/andy.
func displayCWD(cwd, home string) string {
	if home == "" {
		return cwd
	}
	if cwd == home {
		return "~"
	}
	if strings.HasPrefix(cwd, home+"/") {
		return "~" + strings.TrimPrefix(cwd, home)
	}
	return cwd
}

// repoDirName returns the short "<repo-name>" or "<repo-name>:<worktree-name>"
// form for a git-tracked cwd, and false when cwd isn't inside a git repo at
// all (GitRoot empty) — the caller then falls back to its own non-git
// display. A worktree checkout takes precedence over GitRoot: a worktree's
// own ".git" is a file (see isGitWorktree), so GitRoot would otherwise
// resolve to the worktree checkout itself rather than the main repo.
func repoDirName(cwd, gitRoot string) (string, bool) {
	if name := worktreeName(cwd); name != "" {
		return filepath.Base(worktreeRepoRoot(cwd)) + ":" + name, true
	}
	if gitRoot != "" {
		return filepath.Base(gitRoot), true
	}
	return "", false
}

// dirDisplay computes the full/intermediate view's DIR-column text for a
// session's cwd: the short repoDirName form for any git-tracked cwd, or a
// full squashed-home path when cwd isn't inside a git repo (GitRoot empty,
// e.g. scratch dirs or plain folders) — there, ancestry is the only way to
// tell dirs apart.
func dirDisplay(cwd, home, gitRoot string) string {
	if name, ok := repoDirName(cwd, gitRoot); ok {
		return name
	}
	return squashPath(displayCWD(cwd, home))
}

// section is one rendering block. host is the stable selection/action key
// ("" for local, configured alias for remote); name is the visible heading.
type section struct {
	name string
	host string
	// account is the email of the Anthropic account logged in on this host, ""
	// when unknown (identity unreadable, or an older server that doesn't report
	// usage). renderHostHeading appends it dimmed after LOAD; "" appends nothing
	// rather than a placeholder, matching how dedupeAccounts treats an unknown
	// account today.
	account   string
	hostUsage HostUsage
	rows      []Session
	error     string
	loading   bool
	stale     bool
}

// filterSessionRows returns the subset of rows passing the active view filter
// (the group filter AND the free-text query, composed), preserving order. Each
// row's own Host labels the section it renders under (empty for local), so the
// text filter can match the host name. With neither filter active, rows are
// returned unchanged (same backing array).
func filterSessionRows(rows []Session, gv groupView) []Session {
	if gv.filter.mode == filterNone && gv.query == "" && !gv.hideDisabled {
		return rows
	}
	out := make([]Session, 0, len(rows))
	for _, s := range rows {
		if passesGroupFilter(s, gv.groups, gv.filter) && matchesTextFilter(s, s.Host, gv.query) && (!gv.hideDisabled || !s.Disabled) {
			out = append(out, s)
		}
	}
	return out
}

// filterRemoteResults returns copies of the results with each host's Sessions
// filtered by the active view filter (group AND text). The RemoteResult metadata
// (Error, Loading, etc.) is preserved so empty-after-filter hosts still render
// their heading + the empty-host row. With neither filter active the input is
// returned unchanged.
func filterRemoteResults(remotes []RemoteResult, gv groupView) []RemoteResult {
	if gv.filter.mode == filterNone && gv.query == "" && !gv.hideDisabled {
		return remotes
	}
	out := make([]RemoteResult, len(remotes))
	for i, r := range remotes {
		r.Sessions = filterSessionRows(r.Sessions, gv)
		out[i] = r
	}
	return out
}

// buildSections turns the local host and every remote result into rendering
// blocks. localAccount is this machine's logged-in account email (see
// section.account); each remote's comes from the usage snapshot that host
// already reports, so the heading label costs no extra fetch and no protocol
// change.
func buildSections(local LocalHost, remotes []RemoteResult, gv groupView, localAccount string) []section {
	out := make([]section, 0, 1+len(remotes))
	out = append(out, section{
		name:      local.Name,
		host:      "",
		account:   localAccount,
		hostUsage: local.HostUsage,
		rows:      filterSessionRows(local.Sessions, gv),
	})
	for _, r := range remotes {
		account := ""
		if r.Usage != nil {
			account = r.Usage.Account
		}
		out = append(out, section{
			name:      r.Name,
			host:      r.Name,
			account:   account,
			hostUsage: r.HostUsage,
			rows:      filterSessionRows(r.Sessions, gv),
			error:     r.Error,
			loading:   r.Loading,
			stale:     r.Stale,
		})
	}
	return out
}

// sectionNameWidth returns the display width of the longest section name, so
// renderHostHeading can pad every host's name to the same column and keep
// CPU/MEM/LOAD aligned whether the host is called "pi" or
// "agent-workstation". Counted in runes, not bytes, so multi-byte names don't
// over-pad.
func sectionNameWidth(sections []section) int {
	width := 0
	for _, sec := range sections {
		if n := utf8.RuneCountInString(sec.name); n > width {
			width = n
		}
	}
	return width
}

// renderEmptyHostRow prints the selectable "(no sessions)" placeholder for a
// reachable local or remote host.
func renderEmptyHostRow(w *frameWriter, host, sel string) {
	selected := sel == emptyHostSelectionID(host)
	body := "(no sessions)"
	row := "  " + dim(body)
	if selected {
		row = highlightSelectedRow("  "+body, true)
	}
	w.record(emptyHostSelectionID(host), false)
	fmt.Fprintln(w, row)
}

// formatHostPercent renders a whole-host usage percentage. A nil pointer means
// the metric was unavailable and renders as "--"; otherwise the value is
// clamped to [0,100] and rounded half away from zero (math.Round, not Go's
// banker's %.0f) so 42.5 shows as "43%". Local values are already clamped by
// hostPercent, but remotely supplied values bypass it, so clamping here keeps a
// buggy server from rendering "250%" or "-0%".
func formatHostPercent(value *float64) string {
	if value == nil {
		return "--"
	}
	clamped := max(0, min(100, *value))
	return fmt.Sprintf("%.0f%%", math.Round(clamped))
}

// formatHostBar renders a whole-host usage percentage as a 5-cell block bar,
// colored the same way the rate-limit bars are (usageColor): default below
// 70%, yellow 70–89%, red 90%+. A nil pointer means the metric was
// unavailable and renders as a dimmed empty track, matching formatHostPercent's
// "--" fallback in spirit.
func formatHostBar(value *float64) string {
	if value == nil {
		return dim(strings.Repeat("░", hostUsageBarWidth))
	}
	clamped := max(0, min(100, *value))
	return colorize(usageColor(clamped), usageBar(clamped, hostUsageBarWidth))
}

// hostUsageBarWidth is the fixed width of the CPU/MEM bars in the host
// heading line.
const hostUsageBarWidth = 5

// loadToken formats one load-average value right-justified to a fixed width
// (so LOAD columns line up across hosts once combined with formatHostLoad's
// siblings) and wraps it in exactly one SGR code: the 1-minute figure
// (emphasize=true) bolds when otherwise uncolored, or bolds-in-place when
// loadSeverity already colored it (yellow -> bold yellow, red is already
// bold); the 5/15-minute figures dim when uncolored, or keep loadSeverity's
// plain-weight color untouched, so the eye lands on the actionable number
// first and reads the trend after. Padding happens on the plain numeral
// before the SGR wrap — wrapping first would let fmt's width count escape
// bytes and silently break the alignment this exists to fix.
func loadToken(v float64, cores int, emphasize bool) string {
	numeral := fmt.Sprintf("%5.1f", v)
	code := loadSeverity(v, cores)
	switch {
	case code == "33" && emphasize:
		code = "1;33"
	case code == "" && emphasize:
		code = "1"
	case code == "" && !emphasize:
		code = "2"
	}
	return colorize(code, numeral)
}

// formatHostLoad renders the 1/5/15-minute host load averages htop-style. The
// triple is atomic: a nil LoadAverage, any nil member, or any negative/NaN/Inf
// value (which remote JSON can carry past hostLoadAverage) renders the whole
// thing as uncolored "--" tokens at the same width as the colored path.
// Otherwise each value prints via loadToken — one decimal, never clamped,
// never sharing formatHostPercent's percentage formatting. cores is the
// host's reported CPU count (0 if unknown), used only for loadSeverity.
func formatHostLoad(load *LoadAverage, cores int) string {
	unavailableToken := fmt.Sprintf("%5s", "--")
	unavailable := strings.Join([]string{unavailableToken, unavailableToken, unavailableToken}, " ")
	if load == nil {
		return unavailable
	}
	values := [3]float64{}
	for i, v := range [...]*float64{load.OneMinute, load.FiveMinutes, load.FifteenMinutes} {
		if v == nil || *v < 0 || math.IsNaN(*v) || math.IsInf(*v, 0) {
			return unavailable
		}
		values[i] = *v
		if values[i] == 0 {
			values[i] = 0 // normalize IEEE negative zero to visible 0.0
		}
	}
	return loadToken(values[0], cores, true) + " " + loadToken(values[1], cores, false) + " " + loadToken(values[2], cores, false)
}

// renderHostHeading prints a section's host heading: the bold host name,
// padded to nameWidth (see sectionNameWidth), followed by its whole-host CPU,
// memory, and load-average usage, and finally the host's logged-in account
// email, dimmed, when known. Used for the local section and every remote
// section across all three views so the layout stays uniform. Padding is
// applied to the plain name before bolding — bolding first would let fmt's
// width count escape bytes and break the alignment. An unknown account appends
// nothing at all (no placeholder, no dash), so a heading without one stays
// byte-identical to the layout from before the label existed.
func renderHostHeading(w io.Writer, sec section, nameWidth int) {
	paddedName := fmt.Sprintf("%-*s", nameWidth, sec.name)
	account := ""
	if sec.account != "" {
		account = "  " + dim(sec.account)
	}
	fmt.Fprintf(w, "  %s  CPU %s  MEM %s  LOAD %s%s\n",
		bold(paddedName),
		formatHostBar(sec.hostUsage.CPUPercent),
		formatHostBar(sec.hostUsage.MemoryPercent),
		formatHostLoad(sec.hostUsage.Load, sec.hostUsage.NumCPU),
		account)
}

// plural renders a count with its word, pluralizing the word for counts other
// than 1: plural(1, "agent") → "1 agent", plural(2, "agent") → "2 agents".
func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// renderHeader prints the title line with live counts, the active view-filter
// indicators (the group badge then a dim "/query" when a text filter is active),
// the optional account usage bars (Anthropic lines then Codex lines, one line
// per distinct account — see dedupeAccounts / dedupeCodexAccounts), and the
// trailing blank line — shared by all three views.
// groupFilterIndicator renders the badge shown in the title while a filter is
// active. only mode shows "only ③" colored with the group's palette entry;
// hide mode shows a red "hide" label followed by the hidden groups' badges in
// ascending order, each in its own group color. Returns "" when no filter is
// active. Every branch ends in a reset, so renderHeader can re-assert bold.
func groupFilterIndicator(filter groupFilter) string {
	if filter.mode == filterNone || filter.mask == 0 {
		return ""
	}
	if filter.mode == filterOnly {
		for g := 1; g <= 9; g++ {
			if groupMaskHas(filter.mask, g) {
				return colorize(groupSGR[g], "only "+groupBadgeGlyph(g))
			}
		}
		return ""
	}
	// filterHide: red "hide" label + the hidden groups' badges, each in its color.
	var b strings.Builder
	b.WriteString(colorize("31", "hide"))
	b.WriteByte(' ')
	for g := 1; g <= 9; g++ {
		if groupMaskHas(filter.mask, g) {
			b.WriteString(colorize(groupSGR[g], groupBadgeGlyph(g)))
		}
	}
	return b.String()
}

func renderHeader(w io.Writer, sections []section, mode string, accounts []accountUsageLine, codexAccounts []codexAccountLine, cols int, filter groupFilter, query string) {
	live, busy := 0, 0
	for _, sec := range sections {
		for _, s := range sec.rows {
			live++
			// "busy" here means the main loop is occupied: working or in a shell.
			if s.Status == "busy" || s.Status == "shell" {
				busy++
			}
		}
	}
	// Two counts: live sessions (local + remote) and how many are occupied.
	// colorize ends with a full reset, so re-assert bold after the busy count
	// to keep the title bold.
	busyStr := colorize(statusColor["busy"], fmt.Sprintf("%d busy", busy)) + ansiBold
	// An active group filter shows a colored "only ③" / "hide ②③" indicator, and
	// an active text query appends a dim "/query" after it (each ends in a reset),
	// so re-assert bold after every segment to keep the trailing [mode] bright.
	filterStr := ""
	if ind := groupFilterIndicator(filter); ind != "" {
		filterStr = "  " + ind + ansiBold
	}
	if query != "" {
		filterStr += "  " + dim("/"+query) + ansiBold
	}
	fmt.Fprintf(w, "%sClaude sessions  %s  (%s, %s)%s  %s%s\n",
		ansiBold, time.Now().Format("15:04:05"),
		plural(live, "session"), busyStr,
		filterStr, ansiReset, dim("["+mode+"]"))
	writeUsageHeader(w, accounts, codexAccounts, cols)
	fmt.Fprintln(w)
}

// frameWriter is the sink BuildTableFrame renders into. It accumulates the
// frame text while tracking the current (zero-based) line index so a row writer
// can, immediately before printing a selectable row, record which line that row
// landed on. Every render write is newline-terminated, so line counts the
// newlines seen so far, which equals the index of the line about to be written.
type frameWriter struct {
	buf  strings.Builder
	line int
	rows []tableRow
}

func (w *frameWriter) Write(p []byte) (int, error) {
	for _, b := range p {
		if b == '\n' {
			w.line++
		}
	}
	return w.buf.Write(p)
}

// record notes that the row about to be printed occupies the current line and
// maps to targetID (matching the row's selectionTarget.id).
func (w *frameWriter) record(targetID string, openable bool) {
	w.rows = append(w.rows, tableRow{line: w.line, targetID: targetID, openable: openable})
}

// BuildTableFrame renders the live table into a tableFrame: the frame text
// split into lines, the line/target metadata for every selectable row, and the
// marquee-overflow flag. It is the structured sibling of RenderAll — the render
// loop uses the frame to crop a viewport and resolve mouse clicks, while
// RenderAll wraps it for callers that only want the text. Arguments mirror
// RenderAll (see its doc for cols/step/sortMode semantics).
func BuildTableFrame(viewMode string, local LocalHost, remotes []RemoteResult, sel string, localUsage *LocalUsage, cols, step int, sortMode string, gv groupView) tableFrame {
	// Resolve this machine's own account usage before the sections are built: the
	// local heading's account label comes from it, as does the local half of the
	// header's account dedupe.
	var localAU AccountUsage
	var localCodex CodexAccountUsage
	var localKnown []KnownAccountUsage
	if localUsage != nil {
		if localUsage.Claude != nil {
			localAU = *localUsage.Claude
		}
		if localUsage.Codex != nil {
			localCodex = *localUsage.Codex
		}
		localKnown = localUsage.KnownAccounts
	}
	sections := buildSections(local, remotes, gv, localAU.Account)
	// Reserve each first-column slot only when at least one visible (post-filter)
	// session needs it, so a frame with none of a given indicator stays
	// byte-identical to the layout from before that slot existed.
	badge, rail, noTmux := sectionSlotReservations(sections, gv)
	gv.showBadge, gv.showRail = badge, rail
	// The full view already has its own TMUX column, so the marker slot only
	// ever applies to the compact views.
	if viewMode == "2" || viewMode == "3" {
		gv.showNoTmux = noTmux
	}
	// Pair each provider's local snapshot with every remote's, dedupe by account,
	// and carry the resolved lines through the header so each distinct account
	// shows once. The two providers dedupe independently.
	accounts := dedupeAccounts(localAU, localKnown, remotes)
	codexAccounts := dedupeCodexAccounts(localCodex, remotes)
	w := &frameWriter{}
	var overflowing bool
	switch viewMode {
	case "2":
		overflowing = renderAllMinimal(w, sections, sel, accounts, codexAccounts, cols, step, sortMode, gv)
	case "3":
		overflowing = renderAllIntermediate(w, sections, sel, accounts, codexAccounts, cols, step, sortMode, gv)
	default:
		overflowing = renderAllFull(w, sections, sel, accounts, codexAccounts, cols, step, sortMode, gv)
	}
	return tableFrame{
		lines:       strings.Split(w.buf.String(), "\n"),
		rows:        w.rows,
		overflowing: overflowing,
	}
}

// sectionSlotReservations scans the already-filtered sections once and reports
// which of the first-column indicator slots the frame must reserve: badge
// (any grouped row), rail (any disabled row), noTmux (any row not running in
// tmux). A slot is reserved for every row of the frame or for none, so one
// visible row needing it settles the whole frame. Callers decide whether
// noTmux actually applies to the view being rendered (see BuildTableFrame) —
// this just reports whether any row would want it.
func sectionSlotReservations(sections []section, gv groupView) (badge, rail, noTmux bool) {
	for _, sec := range sections {
		for _, s := range sec.rows {
			if gv.groupOf(s) != 0 {
				badge = true
			}
			if s.Disabled {
				rail = true
			}
			if s.Tmux == "" {
				noTmux = true
			}
			if badge && rail && noTmux {
				return
			}
		}
	}
	return
}

// RenderAll writes the live table (or a one-shot snapshot) to w, with all
// rows sorted by cwd. Per-host remote sections appear after the local one,
// each separated by a hostname label and a blank line. localUsage is this
// machine's per-provider account rate-limit snapshot (nil when unknown); each
// provider is deduped against every remote's own usage and rendered as one bar
// line per distinct account below the title, sized to cols (cols <= 0 = unknown
// terminal width). step is the shared marquee clock (see marqueeCell);
// overflowing reports whether any visible DIR cell was scrolled, so the caller
// can drive animation ticks only when needed.
//
// It is a thin compatibility wrapper over BuildTableFrame: joining the frame
// lines with newlines reproduces the exact bytes the row writers emitted, so
// the `--once` path and existing callers/tests keep the same output and return.
func RenderAll(w io.Writer, viewMode string, local LocalHost, remotes []RemoteResult, sel string, localUsage *LocalUsage, cols, step int, sortMode string) (overflowing bool) {
	frame := BuildTableFrame(viewMode, local, remotes, sel, localUsage, cols, step, sortMode, groupView{})
	io.WriteString(w, strings.Join(frame.lines, "\n"))
	return frame.overflowing
}

// sortLabels returns the DIR, STATUS and AGE header labels, suffixing ▲/▼ on
// the column that carries the active sort: DIR for the dir mode (ascending),
// STATUS for the status mode, AGE for the time modes. In created modes the AGE
// column shows age since start (see ageBasis), so the arrow always sits on the
// column being sorted.
func sortLabels(sortMode string) (dirLabel, statusLabel, ageLabel string) {
	switch sortMode {
	case "status":
		return "DIR", "STATUS▲", "AGE"
	case "created", "updated":
		return "DIR", "STATUS", "AGE▼"
	case "created-asc", "updated-asc":
		return "DIR", "STATUS", "AGE▲"
	default: // dir
		return "DIR▲", "STATUS", "AGE"
	}
}

// minimalStatusLabel returns the minimal view's one-cell status header, adding
// the ▲ arrow when status is the active sort. The minimal view has no room for
// the word "STATUS", so it marks the single-glyph column with "S▲".
func minimalStatusLabel(sortMode string) string {
	if sortMode == "status" {
		return "S▲"
	}
	return "S"
}

// ageBasis is the timestamp the AGE column counts from: session start in the
// created sort modes, last update otherwise.
func ageBasis(s Session, sortMode string) time.Time {
	if sortMode == "created" || sortMode == "created-asc" {
		return time.UnixMilli(s.StartedAt)
	}
	return s.Updated()
}

// RenderFull renders local sessions only (used by `--once` when there are no
// remote servers configured, and by callers that want the local view alone).
func RenderFull(w io.Writer, sessions []Session, sel string) {
	RenderAll(w, "1", LocalHost{Name: shortHostname(), Sessions: sessions}, nil, sel, nil, 0, 0, "dir")
}

// RenderMinimal — same as RenderFull but for the compact view.
func RenderMinimal(w io.Writer, sessions []Session, sel string) {
	RenderAll(w, "2", LocalHost{Name: shortHostname(), Sessions: sessions}, nil, sel, nil, 0, 0, "dir")
}

// ============================================================================
// Full view
// ============================================================================

type drowFull struct {
	s         Session
	nameStr   string // resolved NAME label (name → tmux → worktree → "-")
	nameDim   bool   // true when nameStr is auto-derived, not user-set
	statusStr string
	cwdStr    string
	modelStr  string
	ctxStr    string
	costStr   string
	ageStr    string
	sidShort  string
}

func deriveFull(s Session, now time.Time, sortMode string) drowFull {
	sid := s.SessionID
	if len(sid) > 8 {
		sid = sid[:8]
	}
	name, nameDim := s.DisplayName()
	name = truncateRunes(name, maxNameCellWidth)
	return drowFull{
		s:         s,
		nameStr:   name,
		nameDim:   nameDim,
		statusStr: s.StatusDisplay(),
		cwdStr:    dirDisplay(s.CWD, s.Home, s.GitRoot),
		modelStr:  shortModel(s.Model),
		ctxStr:    formatTokens(s.ContextTokens),
		costStr:   formatCost(s.CostUSD, s.CostSubagentsUSD),
		ageStr:    formatAge(now.Sub(ageBasis(s, sortMode)).Seconds()),
		sidShort:  sid,
	}
}

// modelCell pads the model for its column, dimming the "-" placeholder unless
// plain is set (rows dimmed as a whole must not embed resets).
func modelCell(model string, width int, plain bool) string {
	s := model
	if s == "" {
		s = "-"
	}
	cell := fmt.Sprintf("%-*s", width, s)
	if model == "" && !plain {
		cell = dim(cell)
	}
	return cell
}

// rowIndent is the leading indent for the column-header and separator lines. It
// reserves 2 cells per active first-column slot (badge + rail) so the column
// labels stay aligned above the row bodies. With no slot active the indent is
// empty and the labels sit flush left, aligned with the row bodies.
func rowIndent(gv groupView) string {
	n := 0
	if gv.showBadge {
		n++
	}
	if gv.showRail {
		n++
	}
	if gv.showNoTmux {
		n++
	}
	return strings.Repeat("  ", n)
}

func renderAllFull(w *frameWriter, sections []section, sel string, accounts []accountUsageLine, codexAccounts []codexAccountLine, cols, step int, sortMode string, gv groupView) (overflowing bool) {
	now := time.Now()
	nameWidth := sectionNameWidth(sections)

	sectionRows := make([][]drowFull, len(sections))
	var all []drowFull
	for si, sec := range sections {
		sectionRows[si] = make([]drowFull, len(sec.rows))
		for i, s := range sec.rows {
			r := deriveFull(s, now, sortMode)
			sectionRows[si][i] = r
			all = append(all, r)
		}
	}

	dirLabel, statusLabel, ageLabel := sortLabels(sortMode)
	nameW, dirW, modelW, costW, statusW, tmuxW := len("NAME"), utf8.RuneCountInString(dirLabel), len("MODEL"), len("COST"), utf8.RuneCountInString(statusLabel), len("TMUX")
	pidW := len("PID")
	for _, r := range all {
		pidW = max(pidW, len(strconv.Itoa(r.s.PID)))
		nameW = max(nameW, len(r.nameStr))
		dirW = max(dirW, len(r.cwdStr))
		modelW = max(modelW, len(r.modelStr))
		costW = max(costW, len(r.costStr))
		statusW = max(statusW, len(r.statusStr))
		t := r.s.Tmux
		if t == "" {
			t = "-"
		}
		tmuxW = max(tmuxW, len(t))
	}

	renderHeader(w, sections, "full", accounts, codexAccounts, cols, gv.filter, gv.query)

	buildHdr := func() string {
		return fmt.Sprintf(
			rowIndent(gv)+"%*s  %-*s  %-*s  %-*s  %-*s  %*s  %5s  %-*s  %5s  %5s  %-8s  %s ",
			pidW, "PID", nameW, "NAME", dirW, dirLabel, modelW, "MODEL",
			statusW, statusLabel, costW, "COST",
			"CTX", tmuxW, "TMUX", "CPU%", ageLabel, "VER", "SID",
		)
	}
	hdr := buildHdr()
	if nd := shrinkDirW(dirW, visualLen(hdr), cols); nd != dirW {
		dirW = nd
		hdr = buildHdr()
	}
	fmt.Fprintln(w, hdr)
	fmt.Fprintln(w, strings.Repeat("-", visualLen(hdr)))

	rowFn := func(rows []drowFull) {
		for _, r := range rows {
			selected := r.s.ID() == sel
			plainCells := sessionRowPlain(r.s, selected)

			tmuxStr := r.s.Tmux
			if tmuxStr == "" {
				tmuxStr = "-"
			}
			tmuxCell := fmt.Sprintf("%-*s", tmuxW, tmuxStr)
			if r.s.Tmux == "" && !plainCells {
				tmuxCell = dim(tmuxCell)
			}
			statusCell := fmt.Sprintf("%-*s", statusW, r.statusStr)
			if !plainCells {
				statusCell = colorize(statusColor[r.s.Status], statusCell)
			}
			nameCell := fmt.Sprintf("%-*s", nameW, r.nameStr)
			if r.nameDim && !plainCells {
				nameCell = dim(nameCell)
			}
			if utf8.RuneCountInString(r.cwdStr) > dirW {
				overflowing = true
			}
			sidCell := r.sidShort
			if sidCell == "" {
				sidCell = "-"
				if !plainCells {
					sidCell = dim(sidCell)
				}
			}
			body := fmt.Sprintf("%*d  %s  %s  %s  %s  %s  %s  %s  %5s  %5s  %-8s  %s ",
				pidW, r.s.PID,
				nameCell,
				marqueeCell(r.cwdStr, dirW, step),
				modelCell(r.modelStr, modelW, plainCells),
				statusCell,
				costCell(r.costStr, costW),
				ctxCell(r.ctxStr, r.s.ContextTokens, plainCells),
				tmuxCell,
				r.s.CPU, r.ageStr, r.s.Version, sidCell,
			)
			row := decorateSessionRow(r.s, selected, body, gv)
			w.record(r.s.ID(), true)
			fmt.Fprintln(w, row)
		}
	}

	// Local first.
	renderHostHeading(w, sections[0], nameWidth)
	if len(sectionRows[0]) == 0 {
		renderEmptyHostRow(w, sections[0].host, sel)
	} else {
		rowFn(sectionRows[0])
	}
	// Remote sections.
	for i := 1; i < len(sections); i++ {
		fmt.Fprintln(w)
		renderHostHeading(w, sections[i], nameWidth)
		switch {
		case sections[i].loading && sections[i].error == "" && len(sectionRows[i]) == 0:
			fmt.Fprintln(w, "  "+dim("(loading...)"))
		case sections[i].stale && len(sectionRows[i]) > 0:
			fmt.Fprintf(w, "  %s\n", dim("[stale: "+sections[i].error+"]"))
			rowFn(sectionRows[i])
		case sections[i].error != "":
			fmt.Fprintf(w, "  %s\n", dim("[unreachable: "+sections[i].error+"]"))
		case len(sectionRows[i]) == 0:
			renderEmptyHostRow(w, sections[i].host, sel)
		default:
			rowFn(sectionRows[i])
		}
	}
	return overflowing
}

// ============================================================================
// Intermediate view — full's columns minus TMUX, VER, SID.
// ============================================================================

func renderAllIntermediate(w *frameWriter, sections []section, sel string, accounts []accountUsageLine, codexAccounts []codexAccountLine, cols, step int, sortMode string, gv groupView) (overflowing bool) {
	now := time.Now()
	nameWidth := sectionNameWidth(sections)

	sectionRows := make([][]drowFull, len(sections))
	var all []drowFull
	for si, sec := range sections {
		sectionRows[si] = make([]drowFull, len(sec.rows))
		for i, s := range sec.rows {
			r := deriveFull(s, now, sortMode)
			sectionRows[si][i] = r
			all = append(all, r)
		}
	}

	dirLabel, statusLabel, ageLabel := sortLabels(sortMode)
	nameW, dirW, modelW, costW, statusW := len("NAME"), utf8.RuneCountInString(dirLabel), len("MODEL"), len("COST"), utf8.RuneCountInString(statusLabel)
	for _, r := range all {
		nameW = max(nameW, len(r.nameStr))
		dirW = max(dirW, len(r.cwdStr))
		modelW = max(modelW, len(r.modelStr))
		costW = max(costW, len(r.costStr))
		statusW = max(statusW, len(r.statusStr))
	}

	renderHeader(w, sections, "intermediate", accounts, codexAccounts, cols, gv.filter, gv.query)

	buildHdr := func() string {
		return fmt.Sprintf(
			rowIndent(gv)+"%-*s  %-*s  %-*s  %-*s  %*s  %5s  %5s  %5s ",
			nameW, "NAME", dirW, dirLabel, statusW, statusLabel,
			modelW, "MODEL", costW, "COST",
			"CTX", "CPU%", ageLabel,
		)
	}
	hdr := buildHdr()
	if nd := shrinkDirW(dirW, visualLen(hdr), cols); nd != dirW {
		dirW = nd
		hdr = buildHdr()
	}
	fmt.Fprintln(w, hdr)
	fmt.Fprintln(w, strings.Repeat("-", visualLen(hdr)))

	rowFn := func(rows []drowFull) {
		for _, r := range rows {
			selected := r.s.ID() == sel
			plainCells := sessionRowPlain(r.s, selected)

			statusCell := fmt.Sprintf("%-*s", statusW, r.statusStr)
			if !plainCells {
				statusCell = colorize(statusColor[r.s.Status], statusCell)
			}
			nameCell := fmt.Sprintf("%-*s", nameW, r.nameStr)
			if r.nameDim && !plainCells {
				nameCell = dim(nameCell)
			}
			if utf8.RuneCountInString(r.cwdStr) > dirW {
				overflowing = true
			}
			body := fmt.Sprintf("%s  %s  %s  %s  %s  %s  %5s  %5s ",
				nameCell,
				marqueeCell(r.cwdStr, dirW, step),
				statusCell,
				modelCell(r.modelStr, modelW, plainCells),
				costCell(r.costStr, costW),
				ctxCell(r.ctxStr, r.s.ContextTokens, plainCells),
				r.s.CPU, r.ageStr,
			)
			row := decorateSessionRow(r.s, selected, body, gv)
			w.record(r.s.ID(), true)
			fmt.Fprintln(w, row)
		}
	}

	renderHostHeading(w, sections[0], nameWidth)
	if len(sectionRows[0]) == 0 {
		renderEmptyHostRow(w, sections[0].host, sel)
	} else {
		rowFn(sectionRows[0])
	}
	for i := 1; i < len(sections); i++ {
		fmt.Fprintln(w)
		renderHostHeading(w, sections[i], nameWidth)
		switch {
		case sections[i].loading && sections[i].error == "" && len(sectionRows[i]) == 0:
			fmt.Fprintln(w, "  "+dim("(loading...)"))
		case sections[i].stale && len(sectionRows[i]) > 0:
			fmt.Fprintf(w, "  %s\n", dim("[stale: "+sections[i].error+"]"))
			rowFn(sectionRows[i])
		case sections[i].error != "":
			fmt.Fprintf(w, "  %s\n", dim("[unreachable: "+sections[i].error+"]"))
		case len(sectionRows[i]) == 0:
			renderEmptyHostRow(w, sections[i].host, sel)
		default:
			rowFn(sectionRows[i])
		}
	}
	return overflowing
}

// ============================================================================
// Minimal view
// ============================================================================

type drowMinimal struct {
	s       Session
	dir     string // repo name (+ ":worktree") for a git cwd, else cwd basename
	display string // resolved NAME label (name → tmux → worktree → "-")
	nameDim bool   // true when display is auto-derived, not user-set
	ageStr  string
}

func deriveMinimal(s Session, now time.Time, sortMode string) drowMinimal {
	dir, ok := repoDirName(s.CWD, s.GitRoot)
	if !ok {
		cwd := displayCWD(s.CWD, s.Home)
		dir = filepath.Base(strings.TrimRight(cwd, "/"))
		if dir == "" {
			dir = cwd
		}
	}
	disp, dimName := s.DisplayName()
	disp = truncateRunes(disp, maxNameCellWidth)
	return drowMinimal{
		s:       s,
		dir:     dir,
		display: disp,
		nameDim: dimName,
		ageStr:  formatAge(now.Sub(ageBasis(s, sortMode)).Seconds()),
	}
}

func renderAllMinimal(w *frameWriter, sections []section, sel string, accounts []accountUsageLine, codexAccounts []codexAccountLine, cols, step int, sortMode string, gv groupView) (overflowing bool) {
	now := time.Now()
	nameWidth := sectionNameWidth(sections)

	sectionRows := make([][]drowMinimal, len(sections))
	var all []drowMinimal
	for si, sec := range sections {
		sectionRows[si] = make([]drowMinimal, len(sec.rows))
		for i, s := range sec.rows {
			r := deriveMinimal(s, now, sortMode)
			sectionRows[si][i] = r
			all = append(all, r)
		}
	}

	dirLabel, _, ageLabel := sortLabels(sortMode)
	statusLabel := minimalStatusLabel(sortMode)
	statusW := utf8.RuneCountInString(statusLabel)
	dirW, nameW := utf8.RuneCountInString(dirLabel), len("NAME")
	for _, r := range all {
		dirW = max(dirW, len(r.dir))
		nameW = max(nameW, len(r.display))
	}

	renderHeader(w, sections, "minimal", accounts, codexAccounts, cols, gv.filter, gv.query)

	buildHdr := func() string {
		return fmt.Sprintf(
			rowIndent(gv)+"%-*s  %-*s  %-*s  %5s ",
			dirW, dirLabel, nameW, "NAME", statusW, statusLabel, ageLabel,
		)
	}
	hdr := buildHdr()
	if nd := shrinkDirW(dirW, visualLen(hdr), cols); nd != dirW {
		dirW = nd
		hdr = buildHdr()
	}
	fmt.Fprintln(w, hdr)
	fmt.Fprintln(w, strings.Repeat("-", visualLen(hdr)))

	rowFn := func(rows []drowMinimal) {
		for _, r := range rows {
			selected := r.s.ID() == sel
			plainCells := sessionRowPlain(r.s, selected)

			glyph := statusGlyph[r.s.Status]
			if glyph == "" {
				glyph = "?"
			}
			statusCell := glyph + strings.Repeat(" ", statusW-1)
			if !plainCells {
				statusCell = colorize(statusColor[r.s.Status], statusCell)
			}
			nameCell := fmt.Sprintf("%-*s", nameW, r.display)
			if r.nameDim && !plainCells {
				nameCell = dim(nameCell)
			}
			if utf8.RuneCountInString(r.dir) > dirW {
				overflowing = true
			}
			body := fmt.Sprintf(
				"%s  %s  %s  %5s ",
				marqueeCell(r.dir, dirW, step),
				nameCell,
				statusCell,
				r.ageStr,
			)
			row := decorateSessionRow(r.s, selected, body, gv)
			w.record(r.s.ID(), true)
			fmt.Fprintln(w, row)
		}
	}

	renderHostHeading(w, sections[0], nameWidth)
	if len(sectionRows[0]) == 0 {
		renderEmptyHostRow(w, sections[0].host, sel)
	} else {
		rowFn(sectionRows[0])
	}
	for i := 1; i < len(sections); i++ {
		fmt.Fprintln(w)
		renderHostHeading(w, sections[i], nameWidth)
		switch {
		case sections[i].loading && sections[i].error == "" && len(sectionRows[i]) == 0:
			fmt.Fprintln(w, "  "+dim("(loading...)"))
		case sections[i].stale && len(sectionRows[i]) > 0:
			fmt.Fprintf(w, "  %s\n", dim("[stale: "+sections[i].error+"]"))
			rowFn(sectionRows[i])
		case sections[i].error != "":
			fmt.Fprintf(w, "  %s\n", dim("[unreachable: "+sections[i].error+"]"))
		case len(sectionRows[i]) == 0:
			renderEmptyHostRow(w, sections[i].host, sel)
		default:
			rowFn(sectionRows[i])
		}
	}
	return overflowing
}

// visualLen returns the display width of a string with ANSI escapes stripped.
func visualLen(s string) int {
	out := s
	for {
		i := strings.Index(out, "\033[")
		if i < 0 {
			return utf8.RuneCountInString(out)
		}
		j := strings.IndexByte(out[i:], 'm')
		if j < 0 {
			return utf8.RuneCountInString(out)
		}
		out = out[:i] + out[i+j+1:]
	}
}

// clipLine truncates s to at most width visible columns. ANSI escape
// sequences never count toward the width, and sequences past the cut point
// are still emitted so color state (e.g. the trailing reset on a dimmed or
// colorized row) survives truncation. Runes count as one column each.
func clipLine(s string, width int) string {
	var b strings.Builder
	vis := 0
	for i := 0; i < len(s); {
		if s[i] == '\033' && i+1 < len(s) && s[i+1] == '[' {
			j := strings.IndexByte(s[i:], 'm')
			if j < 0 {
				b.WriteString(s[i:])
				break
			}
			b.WriteString(s[i : i+j+1])
			i += j + 1
			continue
		}
		_, sz := utf8.DecodeRuneInString(s[i:])
		if vis < width {
			b.WriteString(s[i : i+sz])
			vis++
		}
		i += sz
	}
	return b.String()
}

// clipLines applies clipLine to every line of a rendered frame. width <= 0
// means "unknown terminal size" and leaves the frame untouched.
func clipLines(frame string, width int) string {
	if width <= 0 {
		return frame
	}
	lines := strings.Split(frame, "\n")
	for i, l := range lines {
		lines[i] = clipLine(l, width)
	}
	return strings.Join(lines, "\n")
}
