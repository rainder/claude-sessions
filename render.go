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

// ANSI escape sequences.
const (
	ansiReset      = "\033[0m"
	ansiBold       = "\033[1m"
	ansiDim        = "\033[2m"
	ansiInvert     = "\033[7m"
	ansiSelectedBG = "\033[48;5;236m"
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

func tmuxViewerSymbol(s Session) (symbol, sgr string) {
	if s.Tmux == "" {
		return " ", ""
	}
	if s.TmuxAttached == nil || *s.TmuxAttached < 0 {
		return "·", "2"
	}
	attached := *s.TmuxAttached
	switch {
	case attached == 0:
		return "0", "2"
	case attached < 10:
		return strconv.Itoa(attached), "1;32"
	default:
		return "+", "1;32"
	}
}

func tmuxViewerPrefix(s Session, plain bool) string {
	symbol, sgr := tmuxViewerSymbol(s)
	prefix := symbol + " "
	if plain || sgr == "" {
		return prefix
	}
	return colorize(sgr, prefix)
}

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
		return "\033[33m│\033[39m "
	}
	return colorize("33", "│") + " "
}

func sessionRowPlain(session Session, selected bool) bool {
	return session.Headless() || (session.Disabled && !selected)
}

func decorateSessionRow(session Session, selected bool, body string) string {
	plain := sessionRowPlain(session, selected)
	viewer := tmuxViewerPrefix(session, plain)
	rail := disabledRail(session, selected)

	var row string
	switch {
	case selected:
		row = viewer + rail + body
	case session.Disabled:
		row = dim(viewer) + rail + dim(body)
	case session.Headless():
		row = dim(viewer + rail + body)
	default:
		row = viewer + rail + body
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
func formatUntil(t time.Time) string {
	d := time.Until(t)
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

// writeUsage prints the account rate-limit line that sits under the title,
// or nothing when usage data isn't available (nil). All buckets share one
// line: "5h <bar> 42% 2h   wk <bar> 13% 3d   Fable <bar> 10% 5d   cr <bar> 5% $1,123".
// The trailing figure is the time remaining until that bucket resets (rate
// limits) or the amount of credits spent (extra usage). The model-scoped
// weekly segment (labeled with the model's display name, e.g. "Fable") only
// appears when the account has such a limit; the credits segment only
// appears when extra usage is enabled on the account. Bars are sized to fit
// cols (usageBarMin..usageBarMax cells each); cols <= 0 means unknown width
// and gets the maximum.
func writeUsage(w io.Writer, u *UsageInfo, cols int) {
	if u == nil {
		return
	}
	type seg struct {
		label, trailer string
		pct            float64
	}
	segs := []seg{
		{"5h", formatUntil(u.FiveHour.ResetsAt), u.FiveHour.Pct},
		{"wk", formatUntil(u.SevenDay.ResetsAt), u.SevenDay.Pct},
	}
	if u.WeeklyScopedLabel != "" {
		segs = append(segs, seg{
			u.WeeklyScopedLabel,
			formatUntil(u.WeeklyScoped.ResetsAt),
			u.WeeklyScoped.Pct,
		})
	}
	if c := u.Credits; c.Enabled && c.Limit > 0 {
		sym := c.Currency
		if sym == "" || sym == "USD" {
			sym = "$"
		}
		segs = append(segs, seg{
			"cr",
			sym + moneyGrouped(c.Used, c.DecimalPlaces),
			c.Pct(),
		})
	}
	// Everything except the bars is fixed width; divide what's left of the
	// terminal between the bars.
	fixed := 3 * (len(segs) - 1) // inter-segment separators
	for _, s := range segs {
		fixed += len(s.label) + 1 + 1 + len(fmt.Sprintf("%.0f%%", s.pct)) + 1 + len(s.trailer)
	}
	barW := usageBarMax
	if cols > 0 {
		if b := (cols - fixed) / len(segs); b < barW {
			barW = b
		}
		if barW < usageBarMin {
			barW = usageBarMin
		}
	}
	parts := make([]string, len(segs))
	for i, s := range segs {
		parts[i] = fmt.Sprintf("%s %s %.0f%% %s",
			s.label,
			colorize(usageColor(s.pct), usageBar(s.pct, barW)),
			s.pct,
			dim(s.trailer))
	}
	fmt.Fprintln(w, strings.Join(parts, "   "))
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

// formatAgents renders a running-subagent count: blank at zero so idle rows
// stay quiet, plain digits otherwise.
func formatAgents(n int) string {
	if n <= 0 {
		return ""
	}
	return fmt.Sprintf("%d", n)
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

// section is one rendering block. host is the stable selection/action key
// ("" for local, configured alias for remote); name is the visible heading.
type section struct {
	name      string
	host      string
	hostUsage HostUsage
	rows      []Session
	error     string
	loading   bool
}

func buildSections(local LocalHost, remotes []RemoteResult) []section {
	out := make([]section, 0, 1+len(remotes))
	out = append(out, section{
		name:      local.Name,
		host:      "",
		hostUsage: local.HostUsage,
		rows:      local.Sessions,
	})
	for _, r := range remotes {
		out = append(out, section{
			name:      r.Name,
			host:      r.Name,
			hostUsage: r.HostUsage,
			rows:      r.Sessions,
			error:     r.Error,
			loading:   r.Loading,
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
// memory, and load-average usage. Used for the local section and every remote
// section across all three views so the layout stays uniform. Padding is
// applied to the plain name before bolding — bolding first would let fmt's
// width count escape bytes and break the alignment.
func renderHostHeading(w io.Writer, sec section, nameWidth int) {
	paddedName := fmt.Sprintf("%-*s", nameWidth, sec.name)
	fmt.Fprintf(w, "  %s  CPU %4s  MEM %4s  LOAD %s\n",
		bold(paddedName),
		formatHostPercent(sec.hostUsage.CPUPercent),
		formatHostPercent(sec.hostUsage.MemoryPercent),
		formatHostLoad(sec.hostUsage.Load, sec.hostUsage.NumCPU))
}

// plural renders a count with its word, pluralizing the word for counts other
// than 1: plural(1, "agent") → "1 agent", plural(2, "agent") → "2 agents".
func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// renderHeader prints the title line with live counts, the optional account
// usage bars, and the trailing blank line — shared by all three views.
func renderHeader(w io.Writer, sections []section, mode string, usage *UsageInfo, cols int) {
	live, busy, subs := 0, 0, 0
	for _, sec := range sections {
		for _, s := range sec.rows {
			live++
			subs += s.AgentsRunning
			// "busy" here means the main loop is occupied: working or in a shell.
			if s.Status == "busy" || s.Status == "shell" {
				busy++
			}
		}
	}
	// Three counts: total concurrent agent loops (each live session is one,
	// plus every running subagent incl. nested, across local and remote),
	// main loops only, and occupied main loops. colorize ends with a full
	// reset, so re-assert bold after the busy count to keep the title bold.
	busyStr := colorize(statusColor["busy"], fmt.Sprintf("%d busy", busy)) + ansiBold
	fmt.Fprintf(w, "%sClaude sessions  %s  (%s, %s, %s)  %s%s\n",
		ansiBold, time.Now().Format("15:04:05"),
		plural(live+subs, "agent"), plural(live, "session"), busyStr,
		ansiReset, dim("["+mode+"]"))
	writeUsage(w, usage, cols)
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
func BuildTableFrame(viewMode string, local LocalHost, remotes []RemoteResult, sel string, usage *UsageInfo, cols, step int, sortMode string) tableFrame {
	sections := buildSections(local, remotes)
	w := &frameWriter{}
	var overflowing bool
	switch viewMode {
	case "2":
		overflowing = renderAllMinimal(w, sections, sel, usage, cols, step, sortMode)
	case "3":
		overflowing = renderAllIntermediate(w, sections, sel, usage, cols, step, sortMode)
	default:
		overflowing = renderAllFull(w, sections, sel, usage, cols, step, sortMode)
	}
	return tableFrame{
		lines:       strings.Split(w.buf.String(), "\n"),
		rows:        w.rows,
		overflowing: overflowing,
	}
}

// RenderAll writes the live table (or a one-shot snapshot) to w, with all
// rows sorted by cwd. Per-host remote sections appear after the local one,
// each separated by a hostname label and a blank line. When usage is non-nil,
// account rate-limit bars are printed below the title, sized to cols
// (cols <= 0 = unknown terminal width). step is the shared marquee clock (see
// marqueeCell); overflowing reports whether any visible DIR cell was scrolled,
// so the caller can drive animation ticks only when needed.
//
// It is a thin compatibility wrapper over BuildTableFrame: joining the frame
// lines with newlines reproduces the exact bytes the row writers emitted, so
// the `--once` path and existing callers/tests keep the same output and return.
func RenderAll(w io.Writer, viewMode string, local LocalHost, remotes []RemoteResult, sel string, usage *UsageInfo, cols, step int, sortMode string) (overflowing bool) {
	frame := BuildTableFrame(viewMode, local, remotes, sel, usage, cols, step, sortMode)
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
	agentsStr string // running-subagent count, "" when zero
	ageStr    string
	sidShort  string
}

func deriveFull(s Session, now time.Time, sortMode string) drowFull {
	cwd := displayCWD(s.CWD, s.Home)
	sid := s.SessionID
	if len(sid) > 8 {
		sid = sid[:8]
	}
	name, nameDim := s.DisplayName()
	return drowFull{
		s:         s,
		nameStr:   name,
		nameDim:   nameDim,
		statusStr: s.StatusDisplay(),
		cwdStr:    squashPath(cwd),
		modelStr:  shortModel(s.Model),
		ctxStr:    formatTokens(s.ContextTokens),
		costStr:   formatCost(s.CostUSD, s.CostSubagentsUSD),
		agentsStr: formatAgents(s.AgentsRunning),
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

func renderAllFull(w *frameWriter, sections []section, sel string, usage *UsageInfo, cols, step int, sortMode string) (overflowing bool) {
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
	nameW, dirW, modelW, costW, agentsW, statusW, tmuxW := len("NAME"), utf8.RuneCountInString(dirLabel), len("MODEL"), len("COST"), len("AGENTS"), utf8.RuneCountInString(statusLabel), len("TMUX")
	for _, r := range all {
		nameW = max(nameW, len(r.nameStr))
		dirW = max(dirW, len(r.cwdStr))
		modelW = max(modelW, len(r.modelStr))
		costW = max(costW, len(r.costStr))
		agentsW = max(agentsW, len(r.agentsStr))
		statusW = max(statusW, len(r.statusStr))
		t := r.s.Tmux
		if t == "" {
			t = "-"
		}
		tmuxW = max(tmuxW, len(t))
	}

	renderHeader(w, sections, "full", usage, cols)

	buildHdr := func() string {
		return fmt.Sprintf(
			"    %7s  %-*s  %-*s  %-*s  %-*s  %*s  %*s  %5s  %-*s  %5s  %5s  %-8s  %s ",
			"PID", nameW, "NAME", dirW, dirLabel, modelW, "MODEL",
			statusW, statusLabel, costW, "COST", agentsW, "AGENTS",
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
			body := fmt.Sprintf("%7d  %s  %s  %s  %s  %s  %*s  %s  %s  %5s  %5s  %-8s  %s ",
				r.s.PID,
				nameCell,
				marqueeCell(r.cwdStr, dirW, step),
				modelCell(r.modelStr, modelW, plainCells),
				statusCell,
				costCell(r.costStr, costW),
				agentsW, r.agentsStr,
				ctxCell(r.ctxStr, r.s.ContextTokens, plainCells),
				tmuxCell,
				r.s.CPU, r.ageStr, r.s.Version, sidCell,
			)
			row := decorateSessionRow(r.s, selected, body)
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

func renderAllIntermediate(w *frameWriter, sections []section, sel string, usage *UsageInfo, cols, step int, sortMode string) (overflowing bool) {
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
	nameW, dirW, modelW, costW, agentsW, statusW := len("NAME"), utf8.RuneCountInString(dirLabel), len("MODEL"), len("COST"), len("AGENTS"), utf8.RuneCountInString(statusLabel)
	for _, r := range all {
		nameW = max(nameW, len(r.nameStr))
		dirW = max(dirW, len(r.cwdStr))
		modelW = max(modelW, len(r.modelStr))
		costW = max(costW, len(r.costStr))
		agentsW = max(agentsW, len(r.agentsStr))
		statusW = max(statusW, len(r.statusStr))
	}

	renderHeader(w, sections, "intermediate", usage, cols)

	buildHdr := func() string {
		return fmt.Sprintf(
			"    %-*s  %-*s  %-*s  %-*s  %*s  %*s  %5s  %5s  %5s ",
			nameW, "NAME", dirW, dirLabel, statusW, statusLabel,
			modelW, "MODEL", costW, "COST", agentsW, "AGENTS",
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
			body := fmt.Sprintf("%s  %s  %s  %s  %s  %*s  %s  %5s  %5s ",
				nameCell,
				marqueeCell(r.cwdStr, dirW, step),
				statusCell,
				modelCell(r.modelStr, modelW, plainCells),
				costCell(r.costStr, costW),
				agentsW, r.agentsStr,
				ctxCell(r.ctxStr, r.s.ContextTokens, plainCells),
				r.s.CPU, r.ageStr,
			)
			row := decorateSessionRow(r.s, selected, body)
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
	dir     string // cwd basename
	display string // resolved NAME label (name → tmux → worktree → "-")
	nameDim bool   // true when display is auto-derived, not user-set
	ageStr  string
}

func deriveMinimal(s Session, now time.Time, sortMode string) drowMinimal {
	cwd := displayCWD(s.CWD, s.Home)
	dir := filepath.Base(strings.TrimRight(cwd, "/"))
	if dir == "" {
		dir = cwd
	}
	disp, dimName := s.DisplayName()
	return drowMinimal{
		s:       s,
		dir:     dir,
		display: disp,
		nameDim: dimName,
		ageStr:  formatAge(now.Sub(ageBasis(s, sortMode)).Seconds()),
	}
}

func renderAllMinimal(w *frameWriter, sections []section, sel string, usage *UsageInfo, cols, step int, sortMode string) (overflowing bool) {
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

	renderHeader(w, sections, "minimal", usage, cols)

	buildHdr := func() string {
		return fmt.Sprintf(
			"    %-*s  %-*s  %-*s  %5s ",
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
			row := decorateSessionRow(r.s, selected, body)
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
