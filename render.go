package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

// ANSI escape sequences.
const (
	ansiReset  = "\033[0m"
	ansiBold   = "\033[1m"
	ansiDim    = "\033[2m"
	ansiInvert = "\033[7m"
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

// moneyCompact renders a minor-unit amount rounded to whole major units,
// abbreviating thousands: 2550¢ → "26", 100000¢ → "1k", 155000¢ → "1.6k".
// Cent precision is deliberately dropped — the header needs magnitude, not
// an invoice.
func moneyCompact(minor float64, places int) string {
	scale := 1.0
	for i := 0; i < places; i++ {
		scale *= 10
	}
	v := minor / scale
	if v < 1000 {
		return fmt.Sprintf("%.0f", v)
	}
	k := v / 1000
	if r := fmt.Sprintf("%.1f", k); !strings.HasSuffix(r, ".0") {
		return r + "k"
	}
	return fmt.Sprintf("%.0fk", k)
}

// usageBarMax is the widest a header bar gets; narrow terminals shrink bars
// down to usageBarMin before the line gets clipped.
const (
	usageBarMax = 10
	usageBarMin = 4
)

// writeUsage prints the account rate-limit line that sits under the title,
// or nothing when usage data isn't available (nil). All buckets share one
// line: "5h <bar> 42% 2h   wk <bar> 13% 3d   cr <bar> 5% $50/1k". The
// trailing figure is the time remaining until that bucket resets (rate
// limits) or spent/limit credits (extra usage). The credits segment only
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
	if c := u.Credits; c.Enabled && c.Limit > 0 {
		sym := c.Currency
		if sym == "" || sym == "USD" {
			sym = "$"
		}
		segs = append(segs, seg{
			"cr",
			sym + moneyCompact(c.Used, c.DecimalPlaces) + "/" + moneyCompact(c.Limit, c.DecimalPlaces),
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

// displayCWD collapses the local $HOME prefix to "~". Remote paths are left
// alone since the remote host's $HOME differs from ours.
func displayCWD(cwd, home, host string) string {
	if host == "" && home != "" && strings.HasPrefix(cwd, home) {
		return "~" + strings.TrimPrefix(cwd, home)
	}
	return cwd
}

// section is one rendering block: the local sessions (label "") or one
// remote host's sessions (label = hostname).
type section struct {
	label   string
	rows    []Session
	error   string
	loading bool
}

func buildSections(local []Session, remotes []RemoteResult) []section {
	out := make([]section, 0, 1+len(remotes))
	out = append(out, section{rows: local})
	for _, r := range remotes {
		out = append(out, section{label: r.Name, rows: r.Sessions, error: r.Error, loading: r.Loading})
	}
	return out
}

// renderHeader prints the title line with live counts, the optional account
// usage bars, and the trailing blank line — shared by all three views.
func renderHeader(w io.Writer, sections []section, mode string, usage *UsageInfo, cols int) {
	live, tmuxCount, busy, shell := 0, 0, 0, 0
	for _, sec := range sections {
		for _, s := range sec.rows {
			live++
			if s.Tmux != "" {
				tmuxCount++
			}
			switch s.Status {
			case "busy":
				busy++
			case "shell":
				shell++
			}
		}
	}
	// colorize ends with a full reset, so re-assert bold after each count to
	// keep the rest of the title line bold.
	busyStr := colorize(statusColor["busy"], fmt.Sprintf("%d busy", busy)) + ansiBold
	shellStr := colorize(statusColor["shell"], fmt.Sprintf("%d shell", shell)) + ansiBold
	fmt.Fprintf(w, "%sClaude sessions  %s  (%d live, %d in tmux, %s, %s)  %s%s\n",
		ansiBold, time.Now().Format("15:04:05"), live, tmuxCount, busyStr, shellStr,
		ansiReset, dim("["+mode+"]"))
	writeUsage(w, usage, cols)
	fmt.Fprintln(w)
}

// RenderAll writes the live table (or a one-shot snapshot) to w, with all
// rows sorted by cwd. Per-host remote sections appear after the local one,
// each separated by a hostname label and a blank line. When usage is non-nil,
// account rate-limit bars are printed below the title, sized to cols
// (cols <= 0 = unknown terminal width).
func RenderAll(w io.Writer, viewMode string, local []Session, remotes []RemoteResult, sel string, usage *UsageInfo, cols int) {
	sections := buildSections(local, remotes)
	switch viewMode {
	case "2":
		renderAllMinimal(w, sections, sel, usage, cols)
	case "3":
		renderAllIntermediate(w, sections, sel, usage, cols)
	default:
		renderAllFull(w, sections, sel, usage, cols)
	}
}

// RenderFull renders local sessions only (used by `--once` when there are no
// remote servers configured, and by callers that want the local view alone).
func RenderFull(w io.Writer, sessions []Session, sel string) {
	RenderAll(w, "1", sessions, nil, sel, nil, 0)
}

// RenderMinimal — same as RenderFull but for the compact view.
func RenderMinimal(w io.Writer, sessions []Session, sel string) {
	RenderAll(w, "2", sessions, nil, sel, nil, 0)
}

// ============================================================================
// Full view
// ============================================================================

type drowFull struct {
	s         Session
	statusStr string
	cwdStr    string
	modelStr  string
	ageStr    string
	sidShort  string
}

func deriveFull(s Session, home string, now time.Time) drowFull {
	cwd := displayCWD(s.CWD, home, s.Host)
	sid := s.SessionID
	if len(sid) > 8 {
		sid = sid[:8]
	}
	return drowFull{
		s:         s,
		statusStr: s.StatusDisplay(),
		cwdStr:    squashPath(cwd),
		modelStr:  shortModel(s.Model),
		ageStr:    formatAge(now.Sub(s.Updated()).Seconds()),
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

func renderAllFull(w io.Writer, sections []section, sel string, usage *UsageInfo, cols int) {
	home, _ := os.UserHomeDir()
	now := time.Now()

	sectionRows := make([][]drowFull, len(sections))
	var all []drowFull
	for si, sec := range sections {
		sectionRows[si] = make([]drowFull, len(sec.rows))
		for i, s := range sec.rows {
			r := deriveFull(s, home, now)
			sectionRows[si][i] = r
			all = append(all, r)
		}
	}

	nameW, dirW, modelW, statusW, tmuxW := len("NAME"), len("DIR"), len("MODEL"), len("STATUS"), len("TMUX")
	for _, r := range all {
		nameW = max(nameW, len(r.s.Name))
		dirW = max(dirW, len(r.cwdStr))
		modelW = max(modelW, len(r.modelStr))
		statusW = max(statusW, len(r.statusStr))
		t := r.s.Tmux
		if t == "" {
			t = "-"
		}
		tmuxW = max(tmuxW, len(t))
	}

	renderHeader(w, sections, "full", usage, cols)

	hdr := fmt.Sprintf("  %7s  %-*s  %-*s  %-*s  %-*s  %-*s  %5s  %5s  %-8s  %s",
		"PID", nameW, "NAME", dirW, "DIR", modelW, "MODEL", statusW, "STATUS", tmuxW, "TMUX",
		"CPU%", "AGE", "VER", "SID")
	fmt.Fprintln(w, hdr)
	fmt.Fprintln(w, strings.Repeat("-", visualLen(hdr)))

	rowFn := func(rows []drowFull) {
		for _, r := range rows {
			marker := "  "
			if r.s.ID() == sel {
				marker = "▶ "
			}
			ghost := r.s.Headless()
			tmuxStr := r.s.Tmux
			if tmuxStr == "" {
				tmuxStr = "-"
			}
			tmuxCell := fmt.Sprintf("%-*s", tmuxW, tmuxStr)
			if r.s.Tmux == "" && !ghost {
				tmuxCell = dim(tmuxCell)
			}
			statusCell := fmt.Sprintf("%-*s", statusW, r.statusStr)
			if !ghost {
				statusCell = colorize(statusColor[r.s.Status], statusCell)
			}
			row := fmt.Sprintf("%7d  %-*s  %-*s  %s  %s  %s  %5s  %5s  %-8s  %s",
				r.s.PID,
				nameW, r.s.Name,
				dirW, r.cwdStr,
				modelCell(r.modelStr, modelW, ghost),
				statusCell, tmuxCell,
				r.s.CPU, r.ageStr, r.s.Version, r.sidShort)
			if ghost {
				row = dim(row)
			}
			fmt.Fprintf(w, "%s%s\n", marker, row)
		}
	}

	// Local first.
	rowFn(sectionRows[0])
	// Remote sections.
	for i := 1; i < len(sections); i++ {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "  %s\n", bold(sections[i].label))
		switch {
		case sections[i].loading && sections[i].error == "" && len(sectionRows[i]) == 0:
			fmt.Fprintln(w, "  "+dim("(loading...)"))
		case sections[i].error != "":
			fmt.Fprintf(w, "  %s\n", dim("[unreachable: "+sections[i].error+"]"))
		case len(sectionRows[i]) == 0:
			fmt.Fprintln(w, "  "+dim("(no sessions)"))
		default:
			rowFn(sectionRows[i])
		}
	}
}

// ============================================================================
// Intermediate view — full's columns minus TMUX, VER, SID.
// ============================================================================

func renderAllIntermediate(w io.Writer, sections []section, sel string, usage *UsageInfo, cols int) {
	home, _ := os.UserHomeDir()
	now := time.Now()

	sectionRows := make([][]drowFull, len(sections))
	var all []drowFull
	for si, sec := range sections {
		sectionRows[si] = make([]drowFull, len(sec.rows))
		for i, s := range sec.rows {
			r := deriveFull(s, home, now)
			sectionRows[si][i] = r
			all = append(all, r)
		}
	}

	nameW, dirW, modelW, statusW := len("NAME"), len("DIR"), len("MODEL"), len("STATUS")
	for _, r := range all {
		nameW = max(nameW, len(r.s.Name))
		dirW = max(dirW, len(r.cwdStr))
		modelW = max(modelW, len(r.modelStr))
		statusW = max(statusW, len(r.statusStr))
	}

	renderHeader(w, sections, "intermediate", usage, cols)

	hdr := fmt.Sprintf("  %-*s  %-*s  %-*s  %-*s  %5s  %5s",
		nameW, "NAME", dirW, "DIR", modelW, "MODEL", statusW, "STATUS", "CPU%", "AGE")
	fmt.Fprintln(w, hdr)
	fmt.Fprintln(w, strings.Repeat("-", visualLen(hdr)))

	rowFn := func(rows []drowFull) {
		for _, r := range rows {
			marker := "  "
			switch {
			case r.s.ID() == sel:
				marker = "▶ "
			case r.s.Tmux != "":
				marker = dim("· ")
			}
			ghost := r.s.Headless()
			statusCell := fmt.Sprintf("%-*s", statusW, r.statusStr)
			if !ghost {
				statusCell = colorize(statusColor[r.s.Status], statusCell)
			}
			nameStr := r.s.Name
			if nameStr == "" {
				nameStr = "-"
			}
			nameCell := fmt.Sprintf("%-*s", nameW, nameStr)
			if r.s.Name == "" && !ghost {
				nameCell = dim(nameCell)
			}
			row := fmt.Sprintf("%s  %-*s  %s  %s  %5s  %5s",
				nameCell,
				dirW, r.cwdStr,
				modelCell(r.modelStr, modelW, ghost),
				statusCell,
				r.s.CPU, r.ageStr)
			if ghost {
				row = dim(row)
			}
			fmt.Fprintf(w, "%s%s\n", marker, row)
		}
	}

	rowFn(sectionRows[0])
	for i := 1; i < len(sections); i++ {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "  %s\n", bold(sections[i].label))
		switch {
		case sections[i].loading && sections[i].error == "" && len(sectionRows[i]) == 0:
			fmt.Fprintln(w, "  "+dim("(loading...)"))
		case sections[i].error != "":
			fmt.Fprintf(w, "  %s\n", dim("[unreachable: "+sections[i].error+"]"))
		case len(sectionRows[i]) == 0:
			fmt.Fprintln(w, "  "+dim("(no sessions)"))
		default:
			rowFn(sectionRows[i])
		}
	}
}

// ============================================================================
// Minimal view
// ============================================================================

type drowMinimal struct {
	s       Session
	dir     string // cwd basename
	display string // name with tmux fallback
	ageStr  string
}

func deriveMinimal(s Session, home string, now time.Time) drowMinimal {
	cwd := displayCWD(s.CWD, home, s.Host)
	dir := filepath.Base(strings.TrimRight(cwd, "/"))
	if dir == "" {
		dir = cwd
	}
	disp := s.Name
	if disp == "" && s.Tmux != "" {
		disp = s.Tmux
	}
	return drowMinimal{
		s:       s,
		dir:     dir,
		display: disp,
		ageStr:  formatAge(now.Sub(s.Updated()).Seconds()),
	}
}

func renderAllMinimal(w io.Writer, sections []section, sel string, usage *UsageInfo, cols int) {
	home, _ := os.UserHomeDir()
	now := time.Now()

	sectionRows := make([][]drowMinimal, len(sections))
	var all []drowMinimal
	for si, sec := range sections {
		sectionRows[si] = make([]drowMinimal, len(sec.rows))
		for i, s := range sec.rows {
			r := deriveMinimal(s, home, now)
			sectionRows[si][i] = r
			all = append(all, r)
		}
	}

	dirW, nameW := len("DIR"), len("NAME")
	for _, r := range all {
		dirW = max(dirW, len(r.dir))
		nameW = max(nameW, len(r.display))
	}

	renderHeader(w, sections, "minimal", usage, cols)

	hdr := fmt.Sprintf("  %-*s  %-*s  S  %5s", dirW, "DIR", nameW, "NAME", "AGE")
	fmt.Fprintln(w, hdr)
	fmt.Fprintln(w, strings.Repeat("-", visualLen(hdr)))

	rowFn := func(rows []drowMinimal) {
		for _, r := range rows {
			marker := "  "
			switch {
			case r.s.ID() == sel:
				marker = "▶ "
			case r.s.Tmux != "":
				marker = dim("· ")
			}
			ghost := r.s.Headless()
			glyph := statusGlyph[r.s.Status]
			if glyph == "" {
				glyph = "?"
			}
			statusCell := glyph
			if !ghost {
				statusCell = colorize(statusColor[r.s.Status], glyph)
			}
			nameCell := fmt.Sprintf("%-*s", nameW, r.display)
			if r.s.Name == "" && !ghost {
				nameCell = dim(nameCell)
			}
			row := fmt.Sprintf("%-*s  %s  %s  %5s",
				dirW, r.dir, nameCell, statusCell, r.ageStr)
			if ghost {
				row = dim(row)
			}
			fmt.Fprintf(w, "%s%s\n", marker, row)
		}
	}

	rowFn(sectionRows[0])
	for i := 1; i < len(sections); i++ {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "  %s\n", bold(sections[i].label))
		switch {
		case sections[i].loading && sections[i].error == "" && len(sectionRows[i]) == 0:
			fmt.Fprintln(w, "  "+dim("(loading...)"))
		case sections[i].error != "":
			fmt.Fprintf(w, "  %s\n", dim("[unreachable: "+sections[i].error+"]"))
		case len(sectionRows[i]) == 0:
			fmt.Fprintln(w, "  "+dim("(no sessions)"))
		default:
			rowFn(sectionRows[i])
		}
	}
}

// visualLen returns the display width of a string with ANSI escapes stripped.
func visualLen(s string) int {
	out := s
	for {
		i := strings.Index(out, "\033[")
		if i < 0 {
			return len(out)
		}
		j := strings.IndexByte(out[i:], 'm')
		if j < 0 {
			return len(out)
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
