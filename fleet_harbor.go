package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// The harbor: what the Fleet view shows when FLEET is live and no card waits.
// Instead of one "No cards wait for you." line it draws FLEET's working list
// (/fleet/state's "sessions") as boats: a title, counters, one row per
// session with ↑↓ selection and the a attach key, and a calm wave on the last
// body row that moves one step each time a new state is applied.
//
// Every string drawn comes from the decoder's sanitized fields (Label, Name,
// Model, Status, …); a row's raw Tmux only ever reaches the attach request,
// through the same checks as a card's attach.

const (
	fleetHarborTitleText = "nothing needs you"
	fleetHarborWavePat   = ".  ~   .    ~  "
	fleetHarborSGRSea    = "36"   // the title's tildes
	fleetHarborSGRWave   = "2;36" // the wave
	fleetHarborSGRDim    = "2"
	fleetHarborNameMax   = 40 // a wide terminal does not stretch the name column further
	fleetHarborNameMin   = 12 // below this the model, then the status word, give way
	fleetHarborLabelMax  = 16
	fleetHarborModelMax  = 12

	fleetHelpHarbor       = "↑↓ session · Esc back"
	fleetHelpHarborAttach = "↑↓ session · a attach · Esc back"
)

// fleetHarborGroup is a working-list row's place in the harbor, in display
// order: waiting, at sea (busy, shell), in port (idle), then unknown.
type fleetHarborGroup int

const (
	fleetHarborWaiting fleetHarborGroup = iota
	fleetHarborAtSea
	fleetHarborInPort
	fleetHarborUnknown
)

func fleetHarborGroupOf(status string) fleetHarborGroup {
	switch status {
	case "waiting":
		return fleetHarborWaiting
	case "busy", "shell":
		return fleetHarborAtSea
	case "idle":
		return fleetHarborInPort
	}
	return fleetHarborUnknown
}

// fleetHarborGlyph is the row glyph for a status. ⛵ ⚓ ⏳ are two columns
// wide; the unknown "·" is one.
func fleetHarborGlyph(status string) string {
	switch fleetHarborGroupOf(status) {
	case fleetHarborWaiting:
		return "⏳"
	case fleetHarborAtSea:
		return "⛵"
	case fleetHarborInPort:
		return "⚓"
	}
	return "·"
}

// fleetHarborStamp is how long a row has been in its state: since, else (a
// busy row, for which the server sends no since) startedAt. Zero when
// neither parses.
func fleetHarborStamp(w fleetWorkSession) time.Time {
	for _, s := range []string{w.Since, w.StartedAt} {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// fleetHarborOrder is the working list in harbor order: by group, inside a
// group oldest stamp first (longest in that state first; a row with no
// stamp after those with one), ties by tmux name. nil stays nil.
func fleetHarborOrder(sessions []fleetWorkSession) []fleetWorkSession {
	if sessions == nil {
		return nil
	}
	out := append([]fleetWorkSession{}, sessions...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if ga, gb := fleetHarborGroupOf(a.Status), fleetHarborGroupOf(b.Status); ga != gb {
			return ga < gb
		}
		ta, tb := fleetHarborStamp(a), fleetHarborStamp(b)
		if ta.IsZero() != tb.IsZero() {
			return !ta.IsZero()
		}
		if !ta.Equal(tb) {
			return ta.Before(tb)
		}
		return a.Tmux < b.Tmux
	})
	return out
}

// fleetHarborCounts is the counters line's numbers.
type fleetHarborCounts struct{ AtSea, InPort, Waiting, DoneToday int }

// countFleetHarbor counts the rows per group (unknown counts nowhere) and
// the done cards that finished on or after local midnight.
func countFleetHarbor(v fleetStateView, now time.Time) fleetHarborCounts {
	var n fleetHarborCounts
	for _, w := range v.Sessions {
		switch fleetHarborGroupOf(w.Status) {
		case fleetHarborWaiting:
			n.Waiting++
		case fleetHarborAtSea:
			n.AtSea++
		case fleetHarborInPort:
			n.InPort++
		}
	}
	local := now.In(time.Local)
	y, m, d := local.Date()
	midnight := time.Date(y, m, d, 0, 0, 0, 0, time.Local)
	for _, t := range v.DoneAt {
		if !t.Before(midnight) {
			n.DoneToday++
		}
	}
	return n
}

// harborOn reports whether the view shows the harbor: a live FLEET with no
// card at all. Not running, ambiguous, host-down and error views keep their
// own bodies.
func (st *fleetViewState) harborOn() bool {
	return st.err == "" && st.loaded && len(st.view.Ambiguous) == 0 && st.view.Fleet.Alive && len(st.items) == 0
}

// harborSelected is the selected harbor row, nil when the harbor is off or
// has no rows.
func (st *fleetViewState) harborSelected() *fleetWorkSession {
	if !st.harborOn() || st.harborSel < 0 || st.harborSel >= len(st.harbor) {
		return nil
	}
	return &st.harbor[st.harborSel]
}

// harborAttachable reports whether a selected row can be attached to: its raw
// tmux name passes the attach's own checks (fleetAttachArgv refuses the
// rest). It is a working-list row by construction.
func (st *fleetViewState) harborAttachable() bool {
	w := st.harborSelected()
	return w != nil && st.panel == nil && fleetSessionNameRe.MatchString(w.Tmux) && !strings.Contains(w.Tmux, ".")
}

// syncHarbor takes a newly applied state's working list: harbor order, the
// selection kept on its tmux name (the same index first, so two rows in one
// tmux session keep theirs), else clamped where it was.
func (st *fleetViewState) syncHarbor() {
	selTmux := ""
	if st.harborSel >= 0 && st.harborSel < len(st.harbor) {
		selTmux = st.harbor[st.harborSel].Tmux
	}
	st.harbor = fleetHarborOrder(st.view.Sessions)
	if selTmux != "" && !(st.harborSel < len(st.harbor) && st.harbor[st.harborSel].Tmux == selTmux) {
		for i, w := range st.harbor {
			if w.Tmux == selTmux {
				st.harborSel = i
				break
			}
		}
	}
	st.harborSel = min(st.harborSel, len(st.harbor)-1)
	st.harborSel = max(st.harborSel, 0)
}

// handleHarborKey is handleKey while the harbor shows, for the keys it
// changes: ↑↓ move the row selection, and the keys that need a card do
// nothing. handled false leaves the key to handleKey (Esc, a, …).
func (st *fleetViewState) handleHarborKey(k string) (cmd fleetCmd, handled bool) {
	switch k {
	case KeyUp:
		if st.harborSel > 0 {
			st.harborSel--
		}
		return fleetCmdRender, true
	case KeyDown:
		if st.harborSel < len(st.harbor)-1 {
			st.harborSel++
		}
		return fleetCmdRender, true
	case "c", "x", "o", KeyPageUp, KeyPageDown:
		return fleetCmdNone, true
	}
	if _, ok := fleetOptionIndex(k, 9); ok {
		return fleetCmdNone, true
	}
	return fleetCmdNone, false
}

// fleetHarborLayout is the row columns for a cols-wide screen. A zero width
// means the column is dropped.
type fleetHarborLayout struct {
	label, name, model, status, age int
}

// fleetHarborColumns sizes the row columns: label, model, status word and age
// to their widest value (capped), the name to what is left (at most its
// widest value, capped at fleetHarborNameMax). Before a name is cut the
// model goes, then the status word; only then is the name cut to fit.
func fleetHarborColumns(rows []fleetWorkSession, cols int, now time.Time) fleetHarborLayout {
	var l fleetHarborLayout
	nameNat := 1
	for _, w := range rows {
		l.label = max(l.label, cellWidth(fleetHarborLabel(w)))
		l.model = max(l.model, cellWidth(fleetHarborModel(w)))
		l.status = max(l.status, cellWidth(fleetHarborStatus(w)))
		l.age = max(l.age, cellWidth(fleetHarborAge(w, now)))
		nameNat = max(nameNat, cellWidth(fleetHarborName(w)))
	}
	l.label, l.model = min(l.label, fleetHarborLabelMax), min(l.model, fleetHarborModelMax)
	nameNat = min(nameNat, fleetHarborNameMax)
	room := func() int { return cols - l.fixed() }
	if room() < nameNat {
		l.model = 0
	}
	if room() < nameNat {
		l.status = 0
	}
	l.name = min(nameNat, max(room(), 0))
	return l
}

// fleetHarborRowPrefix is " ▸ " (or three spaces), the glyph's two columns and
// a space.
const fleetHarborRowPrefix = 3 + 2 + 1

// fixed is the row width without the name column.
func (l fleetHarborLayout) fixed() int {
	n := fleetHarborRowPrefix + l.label + 2
	for _, w := range []int{l.model, l.status, l.age} {
		if w > 0 {
			n += 2 + w
		}
	}
	return n
}

func (l fleetHarborLayout) width() int { return l.fixed() + l.name }

func fleetHarborLabel(w fleetWorkSession) string {
	if w.Label == "" {
		return "-"
	}
	return w.Label
}

func fleetHarborName(w fleetWorkSession) string {
	if w.Name == "" {
		return "-"
	}
	return w.Name
}

// fleetHarborModel is the short model name; a grok row with no model shows
// its tool.
func fleetHarborModel(w fleetWorkSession) string {
	if m := shortModel(w.Model); m != "" {
		return m
	}
	if w.Tool == toolGrok {
		return w.Tool
	}
	return "-"
}

func fleetHarborStatus(w fleetWorkSession) string {
	if w.Status == "" {
		return "-"
	}
	return w.Status
}

func fleetHarborAge(w fleetWorkSession, now time.Time) string {
	t := fleetHarborStamp(w)
	if t.IsZero() {
		return ""
	}
	return formatAge(now.Sub(t).Seconds())
}

// fleetHarborRow is one session row. The selected row is bold as a whole;
// others dim a derived name and an idle status, and colour waiting amber.
func fleetHarborRow(w fleetWorkSession, l fleetHarborLayout, selected bool, now time.Time) string {
	type seg struct{ text, sgr string }
	prefix := "   "
	if selected {
		prefix = " ▸ "
	}
	segs := []seg{
		{prefix + padCells(fleetHarborGlyph(w.Status), 2) + " " + padCells(truncateCells(fleetHarborLabel(w), l.label), l.label) + "  ", ""},
	}
	if l.name > 0 {
		sgr := ""
		if w.NameSource == "derived" || w.Name == "" {
			sgr = fleetHarborSGRDim
		}
		segs = append(segs, seg{padCells(truncateCells(fleetHarborName(w), l.name), l.name), sgr})
	}
	if l.model > 0 {
		segs = append(segs, seg{"  ", ""}, seg{padCells(truncateCells(fleetHarborModel(w), l.model), l.model), ""})
	}
	if l.status > 0 {
		sgr := ""
		switch w.Status {
		case "waiting":
			sgr = fleetSGRAmber
		case "idle", "":
			sgr = fleetHarborSGRDim
		}
		segs = append(segs, seg{"  ", ""}, seg{padCells(fleetHarborStatus(w), l.status), sgr})
	}
	if l.age > 0 {
		segs = append(segs, seg{"  " + padCellsLeft(fleetHarborAge(w, now), l.age), ""})
	}
	var plain, styled strings.Builder
	for _, s := range segs {
		plain.WriteString(s.text)
		if s.sgr != "" && strings.TrimSpace(s.text) != "" {
			styled.WriteString(colorize(s.sgr, s.text))
		} else {
			styled.WriteString(s.text)
		}
	}
	if selected {
		return bold(plain.String())
	}
	return styled.String()
}

// fleetHarborTitle is the centered "~  nothing needs you  ~".
func fleetHarborTitle(cols int) string {
	full := "~  " + fleetHarborTitleText + "  ~"
	if cellWidth(full) > cols {
		return dim(truncateCells(fleetHarborTitleText, cols))
	}
	pad := strings.Repeat(" ", (cols-cellWidth(full))/2)
	tilde := colorize(fleetHarborSGRSea, "~")
	return pad + tilde + "  " + dim(fleetHarborTitleText) + "  " + tilde
}

// fleetHarborCounters is the centered counters line, in the widest form that
// fits: labelled with wide gaps, labelled with narrow gaps, then glyphs and
// numbers only (cut as a last resort).
func fleetHarborCounters(n fleetHarborCounts, cols int) string {
	type item struct {
		glyph, label string
		n            int
		sgr          string
	}
	items := []item{
		{"⛵", "at sea", n.AtSea, ""},
		{"⚓", "in port", n.InPort, ""},
		{"⏳", "waiting", n.Waiting, fleetSGRAmber},
		{"✓", "done today", n.DoneToday, fleetSGRAlive},
	}
	render := func(labels bool, gap string, styled bool) string {
		parts := make([]string, len(items))
		for i, it := range items {
			num := fmt.Sprint(it.n)
			if styled {
				num = bold(num)
				if it.sgr != "" && it.n > 0 {
					num = colorize("1;"+it.sgr, fmt.Sprint(it.n))
				}
			}
			if labels {
				parts[i] = it.glyph + " " + it.label + "  " + num
			} else {
				parts[i] = it.glyph + " " + num
			}
		}
		return strings.Join(parts, gap)
	}
	for _, f := range []struct {
		labels bool
		gap    string
	}{{true, "      "}, {true, "   "}, {false, "   "}, {false, "  "}} {
		if w := cellWidth(render(f.labels, f.gap, false)); w <= cols {
			return strings.Repeat(" ", (cols-w)/2) + render(f.labels, f.gap, true)
		}
	}
	return truncateCells(render(false, " ", false), cols)
}

// fleetHarborWave is the dim wave across cols columns, shifted by phase.
func fleetHarborWave(cols, phase int) string {
	pat := []rune(fleetHarborWavePat)
	if cols <= 0 {
		return ""
	}
	out := make([]rune, cols)
	for i := range out {
		out[i] = pat[((i+phase)%len(pat)+len(pat))%len(pat)]
	}
	return colorize(fleetHarborSGRWave, string(out))
}

// fleetHarborMarker is the dim right-aligned line under the rows: how many
// rows are scrolled off above and below, and "a attach" when the selected
// row can be attached to. "" when it has nothing to say.
func fleetHarborMarker(above, below int, attach bool, width int) string {
	var parts []string
	if above > 0 {
		parts = append(parts, fmt.Sprintf("⇡ %d more", above))
	}
	if below > 0 {
		parts = append(parts, fmt.Sprintf("⇣ %d more", below))
	}
	if attach {
		parts = append(parts, "a attach")
	}
	if len(parts) == 0 {
		return ""
	}
	text := truncateCells(strings.Join(parts, " · "), width)
	return dim(padCellsLeft(text, width))
}

// fleetHarborList is the rows area in at most h rows (h < 0: every row). The
// rows scroll a window that keeps the selection in view (st.harborTop);
// when they overflow, the last row of the area is the marker line, and a
// one-row area shows the selected row alone.
func fleetHarborList(st *fleetViewState, cols, h int, now time.Time) []string {
	if h == 0 {
		return nil
	}
	switch {
	case st.view.Sessions == nil:
		return []string{"   " + dim(truncateCells("session list unavailable", max(cols-3, 0)))}
	case len(st.harbor) == 0:
		return []string{"   " + dim(truncateCells("no working sessions", max(cols-3, 0)))}
	}
	l := fleetHarborColumns(st.harbor, cols, now)
	width := min(l.width(), cols)
	row := func(i int) string {
		return fleetHarborRow(st.harbor[i], l, i == st.harborSel, now)
	}
	attach := st.harborAttachable()
	n := len(st.harbor)
	var out []string
	switch {
	case h < 0 || n <= h:
		st.harborTop = 0
		for i := range n {
			out = append(out, row(i))
		}
		if m := fleetHarborMarker(0, 0, attach, width); m != "" && (h < 0 || h > n) {
			out = append(out, m)
		}
	case h == 1:
		st.harborTop = st.harborSel
		out = []string{row(st.harborSel)}
	default:
		rh := h - 1
		st.harborTop = fleetListWindow(st.harborTop, st.harborSel, n, rh, false)
		for i := st.harborTop; i < st.harborTop+rh; i++ {
			out = append(out, row(i))
		}
		out = append(out, fleetHarborMarker(st.harborTop, n-st.harborTop-rh, attach, width))
	}
	return out
}

// fleetHarborBody is the harbor in exactly bodyRows rows (bodyRows < 0:
// everything): the title, a gap, the counters, a gap, the rows area, and the
// wave on the last row. On a short screen the gaps go first, then the wave,
// then the counters, then the title, so the rows area keeps at least one row.
func fleetHarborBody(st *fleetViewState, cols, bodyRows int, now time.Time) []string {
	title := fleetHarborTitle(cols)
	counters := fleetHarborCounters(countFleetHarbor(st.view, now), cols)
	wave := fleetHarborWave(cols, st.wave)
	if bodyRows < 0 {
		out := []string{title, "", counters, ""}
		out = append(out, fleetHarborList(st, cols, -1, now)...)
		return append(out, wave)
	}
	show := struct{ title, gap1, counters, gap2, wave bool }{true, true, true, true, true}
	fixed := func() int {
		n := 0
		for _, b := range []bool{show.title, show.gap1, show.counters, show.gap2, show.wave} {
			if b {
				n++
			}
		}
		return n
	}
	for _, drop := range []*bool{&show.gap2, &show.gap1, &show.wave, &show.counters, &show.title} {
		if fixed()+1 <= bodyRows {
			break
		}
		*drop = false
	}
	listH := max(bodyRows-fixed(), 0)
	var out []string
	if show.title {
		out = append(out, title)
	}
	if show.gap1 {
		out = append(out, "")
	}
	if show.counters {
		out = append(out, counters)
	}
	if show.gap2 {
		out = append(out, "")
	}
	list := fleetHarborList(st, cols, listH, now)
	out = append(out, list...)
	for range listH - len(list) {
		out = append(out, "")
	}
	if show.wave {
		out = append(out, wave)
	}
	return out
}
