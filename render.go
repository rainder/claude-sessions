package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
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

// usageBar renders a 20-cell block bar for pct (clamped to 0–100).
func usageBar(pct float64) string {
	filled := int(pct/5 + 0.5)
	if filled < 0 {
		filled = 0
	}
	if filled > 20 {
		filled = 20
	}
	return strings.Repeat("█", filled) + strings.Repeat("░", 20-filled)
}

// writeUsage prints the two account rate-limit lines that sit under the
// title, or nothing when usage data isn't available (nil).
func writeUsage(w io.Writer, u *UsageInfo) {
	if u == nil {
		return
	}
	line := func(label string, b usageBucket, reset string) {
		fmt.Fprintf(w, "%s  %s  %3.0f%%  %s\n",
			label,
			colorize(usageColor(b.Pct), usageBar(b.Pct)),
			b.Pct,
			dim("resets "+reset))
	}
	line("5h", u.FiveHour, u.FiveHour.ResetsAt.Local().Format("15:04"))
	line("wk", u.SevenDay, u.SevenDay.ResetsAt.Local().Format("Mon 15:04"))
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
func renderHeader(w io.Writer, sections []section, mode string, usage *UsageInfo) {
	live, tmuxCount := 0, 0
	for _, sec := range sections {
		for _, s := range sec.rows {
			live++
			if s.Tmux != "" {
				tmuxCount++
			}
		}
	}
	hostsInfo := ""
	if len(sections) > 1 {
		ok := 0
		for _, s := range sections[1:] {
			if s.error == "" {
				ok++
			}
		}
		hostsInfo = fmt.Sprintf(", %d/%d hosts", ok, len(sections)-1)
	}
	fmt.Fprintf(w, "%sClaude sessions  %s  (%d live, %d in tmux%s)  %s%s\n",
		ansiBold, time.Now().Format("15:04:05"), live, tmuxCount, hostsInfo,
		ansiReset, dim("["+mode+"]"))
	writeUsage(w, usage)
	fmt.Fprintln(w)
}

// RenderAll writes the live table (or a one-shot snapshot) to w, with all
// rows sorted by cwd. Per-host remote sections appear after the local one,
// each separated by a hostname label and a blank line.
func RenderAll(w io.Writer, viewMode string, local []Session, remotes []RemoteResult, sel string, usage *UsageInfo) {
	sections := buildSections(local, remotes)
	switch viewMode {
	case "2":
		renderAllMinimal(w, sections, sel, usage)
	case "3":
		renderAllIntermediate(w, sections, sel, usage)
	default:
		renderAllFull(w, sections, sel, usage)
	}
}

// RenderFull renders local sessions only (used by `--once` when there are no
// remote servers configured, and by callers that want the local view alone).
func RenderFull(w io.Writer, sessions []Session, sel string) {
	RenderAll(w, "1", sessions, nil, sel, nil)
}

// RenderMinimal — same as RenderFull but for the compact view.
func RenderMinimal(w io.Writer, sessions []Session, sel string) {
	RenderAll(w, "2", sessions, nil, sel, nil)
}

// ============================================================================
// Full view
// ============================================================================

type drowFull struct {
	s         Session
	statusStr string
	cwdStr    string
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
		ageStr:    formatAge(now.Sub(s.Updated()).Seconds()),
		sidShort:  sid,
	}
}

func renderAllFull(w io.Writer, sections []section, sel string, usage *UsageInfo) {
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

	nameW, dirW, statusW, tmuxW := len("NAME"), len("DIR"), len("STATUS"), len("TMUX")
	for _, r := range all {
		nameW = max(nameW, len(r.s.Name))
		dirW = max(dirW, len(r.cwdStr))
		statusW = max(statusW, len(r.statusStr))
		t := r.s.Tmux
		if t == "" {
			t = "-"
		}
		tmuxW = max(tmuxW, len(t))
	}

	renderHeader(w, sections, "full", usage)

	hdr := fmt.Sprintf("  %7s  %-*s  %-*s  %-*s  %-*s  %5s  %5s  %-8s  %s",
		"PID", nameW, "NAME", dirW, "DIR", statusW, "STATUS", tmuxW, "TMUX",
		"CPU%", "AGE", "VER", "SID")
	fmt.Fprintln(w, hdr)
	fmt.Fprintln(w, strings.Repeat("-", visualLen(hdr)))

	rowFn := func(rows []drowFull) {
		for _, r := range rows {
			marker := "  "
			if r.s.ID() == sel {
				marker = "▶ "
			}
			tmuxCell := r.s.Tmux
			if tmuxCell == "" {
				tmuxCell = dim(fmt.Sprintf("%-*s", tmuxW, "-"))
			} else {
				tmuxCell = fmt.Sprintf("%-*s", tmuxW, tmuxCell)
			}
			statusCell := colorize(statusColor[r.s.Status], fmt.Sprintf("%-*s", statusW, r.statusStr))
			fmt.Fprintf(w, "%s%7d  %-*s  %-*s  %s  %s  %5s  %5s  %-8s  %s\n",
				marker, r.s.PID,
				nameW, r.s.Name,
				dirW, r.cwdStr,
				statusCell, tmuxCell,
				r.s.CPU, r.ageStr, r.s.Version, r.sidShort)
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

func renderAllIntermediate(w io.Writer, sections []section, sel string, usage *UsageInfo) {
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

	nameW, dirW, statusW := len("NAME"), len("DIR"), len("STATUS")
	for _, r := range all {
		nameW = max(nameW, len(r.s.Name))
		dirW = max(dirW, len(r.cwdStr))
		statusW = max(statusW, len(r.statusStr))
	}

	renderHeader(w, sections, "intermediate", usage)

	hdr := fmt.Sprintf("  %-*s  %-*s  %-*s  %5s  %5s",
		nameW, "NAME", dirW, "DIR", statusW, "STATUS", "CPU%", "AGE")
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
			statusCell := colorize(statusColor[r.s.Status], fmt.Sprintf("%-*s", statusW, r.statusStr))
			nameCell := fmt.Sprintf("%-*s", nameW, r.s.Name)
			if r.s.Name == "" {
				nameCell = dim(fmt.Sprintf("%-*s", nameW, "-"))
			}
			fmt.Fprintf(w, "%s%s  %-*s  %s  %5s  %5s\n",
				marker,
				nameCell,
				dirW, r.cwdStr,
				statusCell,
				r.s.CPU, r.ageStr)
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

func renderAllMinimal(w io.Writer, sections []section, sel string, usage *UsageInfo) {
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

	renderHeader(w, sections, "minimal", usage)

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
			glyph := statusGlyph[r.s.Status]
			if glyph == "" {
				glyph = "?"
			}
			statusCell := colorize(statusColor[r.s.Status], glyph)
			nameCell := fmt.Sprintf("%-*s", nameW, r.display)
			if r.s.Name == "" {
				nameCell = dim(nameCell)
			}
			fmt.Fprintf(w, "%s%-*s  %s  %s  %5s\n",
				marker, dirW, r.dir, nameCell, statusCell, r.ageStr)
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
