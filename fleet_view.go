package main

import (
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// fleetSection is the TUI list section a decision card sits in.
type fleetSection int

const (
	fleetSecApprove fleetSection = iota // explicit-yes
	fleetSecDecide                      // needs-you
	fleetSecSent                        // latest attempt sent
)

// fleetItem is one decision card with its section and answer entry.
type fleetItem struct {
	Card    fleetCardView
	Section fleetSection
	Answer  *fleetAnswer // nil → no entry
}

// fleetItems lists the decision cards in state.json order, then stable-sorts
// them APPROVE, DECIDE, SENT. A card whose latest attempt was sent is SENT.
// Inside APPROVE and DECIDE, open cards sort by priority (1 first), then
// oldest firstSeenAt first; a card with no readable firstSeenAt goes after
// those with one. SENT keeps state.json order. The page sorts the same way.
func fleetItems(v fleetStateView) []fleetItem {
	var items []fleetItem
	for _, c := range v.Cards {
		if !fleetPushKind(c.Kind) {
			continue
		}
		it := fleetItem{Card: c, Section: fleetSecDecide}
		if c.Kind == "explicit-yes" {
			it.Section = fleetSecApprove
		}
		if c.ID != "" {
			if a, ok := v.Answered[c.ID]; ok {
				it.Answer = &a
				if a.latest().Status == fleetAttemptSent {
					it.Section = fleetSecSent
				}
			}
		}
		items = append(items, it)
	}
	seen := make([]time.Time, len(items))
	for i := range items {
		seen[i], _ = time.Parse(time.RFC3339, items[i].Card.FirstSeenAt)
	}
	idx := make([]int, len(items))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(x, y int) bool {
		a, b := items[idx[x]], items[idx[y]]
		if a.Section != b.Section {
			return a.Section < b.Section
		}
		if a.Section == fleetSecSent {
			return false
		}
		if pa, pb := a.Card.priority(), b.Card.priority(); pa != pb {
			return pa < pb
		}
		ta, tb := seen[idx[x]], seen[idx[y]]
		if ta.IsZero() != tb.IsZero() {
			return !ta.IsZero()
		}
		return ta.Before(tb)
	})
	out := make([]fleetItem, len(items))
	for i, k := range idx {
		out[i] = items[k]
	}
	return out
}

type fleetCardCounts struct{ Approve, Decide, Sent int }

// countFleetCards counts the answerable cards per section. Approve and Decide
// need an ID and either no entry or an unconfirmed latest attempt.
func countFleetCards(v fleetStateView) fleetCardCounts {
	var n fleetCardCounts
	for _, it := range fleetItems(v) {
		switch it.Section {
		case fleetSecSent:
			n.Sent++
			continue
		}
		if it.Card.ID == "" {
			continue
		}
		if it.Answer != nil && it.Answer.latest().Status != fleetAttemptUnconfirmed {
			continue
		}
		if it.Section == fleetSecApprove {
			n.Approve++
		} else {
			n.Decide++
		}
	}
	return n
}

// fleetOptionIndex maps the digit keys "1".."9" to option indexes 0..8 while
// the index is below n.
func fleetOptionIndex(key string, n int) (int, bool) {
	if len(key) != 1 || key[0] < '1' || key[0] > '9' {
		return 0, false
	}
	i := int(key[0] - '1')
	return i, i < n
}

// fleetSendText is the text typed into FLEET: the option's send, plus the
// comment with every whitespace run collapsed to one space.
func fleetSendText(send, comment string) string {
	c := strings.Join(strings.Fields(comment), " ")
	if c == "" {
		return send
	}
	return send + " Andy adds: " + c
}

// fleetOtherText is the text typed into FLEET for an Other answer: Andy's
// own words about card id, with every whitespace run (a pasted newline too)
// collapsed to one space. The page's Other button sends the same prefix, and
// FLEET's skill reads "Andy decides on <id>: <text>" as a free-text answer.
func fleetOtherText(id, text string) string {
	return "Andy decides on " + id + ": " + strings.Join(strings.Fields(text), " ")
}

// fleetEditComment applies one key to the comment box. The bool reports
// whether the key is an edit key (printable ASCII, backspace); a printable
// byte that would push the send text past sendKeysMaxLen is dropped.
func fleetEditComment(comment, key, send string) (string, bool) {
	return fleetEditText(comment, key, func(c string) bool {
		return len(c) <= sendKeysMaxLen && len(fleetSendText(send, c)) <= sendKeysMaxLen
	})
}

// fleetEditOther is fleetEditComment for the Other answer box of card id.
func fleetEditOther(text, key, id string) (string, bool) {
	return fleetEditText(text, key, func(t string) bool {
		return len(t) <= sendKeysMaxLen && len(fleetOtherText(id, t)) <= sendKeysMaxLen
	})
}

// fleetEditText applies one key to a one-line text box; fits says whether
// the longer text may still be sent.
func fleetEditText(text, key string, fits func(string) bool) (string, bool) {
	switch {
	case key == "\x7f" || key == "\x08":
		if text != "" {
			text = text[:len(text)-1]
		}
		return text, true
	case len(key) == 1 && key[0] >= 0x20 && key[0] <= 0x7e:
		if fits(text + key) {
			text += key
		}
		return text, true
	}
	return text, false
}

var fleetListMarkerRe = regexp.MustCompile(`^\s*([-*] |\d+\. )`)

// fleetLabelRe matches a labelled block line ("**Cause:** ..."); its
// wrapped lines indent by fleetLabelIndent so the label stands out.
var fleetLabelRe = regexp.MustCompile(`^\*\*[^*]{1,24}:\*\* `)

const fleetLabelIndent = "  "

// fleetCodeIndent marks a fenced code line in a card summary.
const fleetCodeIndent = "    "

// The summary's colours. A heading is a line that is only bold text.
const (
	fleetSGRHeading = "33" // bold comes from the ** span
	fleetSGRCode    = "36"
	fleetSGRURL     = "34"
	fleetSGRRef     = "35"
	fleetSGRLean    = "32"
	fleetSGRHold    = "31"
	fleetSGRReady   = "32"
	fleetSGRStatus  = "33"
)

var (
	fleetURLRe    = regexp.MustCompile(`https?://\S+`)
	fleetTicketRe = regexp.MustCompile(`\bDR-\d{3,5}\b`)
	fleetPRRe     = regexp.MustCompile(`(?:^|[^\w/#&])(#\d{3,5})\b`)
	fleetStatusRe = regexp.MustCompile(`^\s*(HOLD|READY|UPDATE|RESULT)\b`)
)

// fleetStyle is one summary rune's look: bold, dim and underline add up, fg
// is one SGR colour code.
type fleetStyle struct {
	bold, dim, under bool
	fg               string
}

func (s fleetStyle) sgr() string {
	var codes []string
	if s.bold {
		codes = append(codes, "1")
	}
	if s.dim {
		codes = append(codes, "2")
	}
	if s.under {
		codes = append(codes, "4")
	}
	if s.fg != "" {
		codes = append(codes, s.fg)
	}
	return strings.Join(codes, ";")
}

// fleetSummaryLines wraps a card summary to width for the terminal, as plain
// text. A fence line (```) is dropped and its block's lines indent by
// fleetCodeIndent, kept unchanged when they fit and wrapped under the same
// indent when not; elsewhere ** and backticks are removed and list items
// hang-indent under their marker.
func fleetSummaryLines(s string, width int) []string {
	return fleetSummaryRender(s, width, false)
}

// fleetSummaryStyledLines is fleetSummaryLines with colour: the same visible
// text, styled as fleetLineStyles says. Each styled run ends with a reset, so
// no style leaks into the next line, the indent or the panel border.
func fleetSummaryStyledLines(s string, width int) []string {
	return fleetSummaryRender(s, width, true)
}

func fleetSummaryRender(s string, width int, styled bool) []string {
	var out []string
	fence, first := false, true
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			fence, first = !fence, false
			continue
		}
		if fence {
			code := fleetStyle{fg: fleetSGRCode}
			if !styled || strings.TrimSpace(line) == "" {
				code = fleetStyle{}
			}
			cw := width - len(fleetCodeIndent)
			if len([]rune(line)) <= cw || strings.TrimSpace(line) == "" {
				out = append(out, fleetCodeIndent+colorize(code.sgr(), line))
				continue
			}
			for _, w := range wrapRunes(line, cw) {
				out = append(out, fleetCodeIndent+colorize(code.sgr(), w))
			}
			continue
		}
		vis, sty := fleetLineStyles(line, first)
		if strings.TrimSpace(string(vis)) != "" {
			first = false
		}
		if !styled {
			sty = make([]fleetStyle, len(vis))
		}
		if m := fleetListMarkerRe.FindString(string(vis)); m != "" {
			mw := len([]rune(m))
			wrapped := fleetWrapStyled(vis[mw:], sty[mw:], width-mw)
			pad := strings.Repeat(" ", mw)
			for i, w := range wrapped {
				if i == 0 {
					out = append(out, fleetStyledText(vis[:mw], sty[:mw])+w)
				} else {
					out = append(out, pad+w)
				}
			}
			continue
		}
		if strings.TrimSpace(string(vis)) == "" {
			out = append(out, "")
			continue
		}
		if fleetLabelRe.MatchString(line) {
			for i, w := range fleetWrapStyled(vis, sty, width-len(fleetLabelIndent)) {
				if i > 0 {
					w = fleetLabelIndent + w
				}
				out = append(out, w)
			}
			continue
		}
		out = append(out, fleetWrapStyled(vis, sty, width)...)
	}
	return out
}

// fleetLineStyles removes ** and backticks from one summary line and styles
// each remaining rune. A ** or backtick opens bold or code only when a closing
// one follows on the line; an unpaired one is just removed. On top of that: a
// line of only bold text is a heading; URLs, DR-1234 and #1234 (outside a
// URL) and "(FLEET lean)" are coloured; on the summary's first line (first)
// a leading HOLD, READY, UPDATE or RESULT is its status; a list marker is
// dim. Inside code only the code colour shows.
func fleetLineStyles(line string, first bool) ([]rune, []fleetStyle) {
	var vis []rune
	var bold, code []bool
	b, c := false, false
	for i := 0; i < len(line); {
		switch {
		case strings.HasPrefix(line[i:], "**"):
			b = !b && strings.Contains(line[i+2:], "**")
			i += 2
		case line[i] == '`':
			c = !c && strings.Contains(line[i+1:], "`")
			i++
		default:
			r, sz := utf8.DecodeRuneInString(line[i:])
			vis = append(vis, r)
			bold, code = append(bold, b), append(code, c)
			i += sz
		}
	}
	sty := make([]fleetStyle, len(vis))
	for i := range sty {
		sty[i].bold = bold[i]
	}
	text := string(vis)
	// runeAt maps a byte offset in text, as regexp returns, to a rune index.
	runeAt := func(byteOff int) int { return utf8.RuneCountInString(text[:byteOff]) }
	paint := func(from, to int, f func(*fleetStyle)) {
		for i := runeAt(from); i < runeAt(to); i++ {
			f(&sty[i])
		}
	}

	marker := fleetListMarkerRe.FindStringIndex(text)
	heading := marker == nil && strings.TrimSpace(text) != ""
	for i, r := range vis {
		if !unicode.IsSpace(r) && !bold[i] {
			heading = false
		}
	}
	if heading {
		paint(0, len(text), func(s *fleetStyle) { s.fg = fleetSGRHeading })
	}
	if first {
		if m := fleetStatusRe.FindStringSubmatchIndex(text); m != nil {
			fg, strong := fleetSGRStatus, false
			switch text[m[2]:m[3]] {
			case "HOLD":
				fg, strong = fleetSGRHold, true
			case "READY":
				fg, strong = fleetSGRReady, true
			}
			paint(m[2], m[3], func(s *fleetStyle) { s.fg, s.bold = fg, s.bold || strong })
		}
	}
	urls := fleetURLRe.FindAllStringIndex(text, -1)
	for _, u := range urls {
		// Sentence punctuation after a URL, and a ")" that closes a bracket
		// the URL did not open, are not part of it.
		for u[1] > u[0] {
			last := text[u[1]-1]
			if !strings.ContainsRune(".,;:!?", rune(last)) &&
				(last != ')' || strings.Contains(text[u[0]:u[1]], "(")) {
				break
			}
			u[1]--
		}
	}
	inURL := func(from, to int) bool {
		for _, u := range urls {
			if from < u[1] && to > u[0] {
				return true
			}
		}
		return false
	}
	var refs [][]int
	for _, m := range fleetTicketRe.FindAllStringIndex(text, -1) {
		refs = append(refs, m)
	}
	for _, m := range fleetPRRe.FindAllStringSubmatchIndex(text, -1) {
		refs = append(refs, m[2:4])
	}
	for _, m := range refs {
		if !inURL(m[0], m[1]) {
			paint(m[0], m[1], func(s *fleetStyle) { s.fg = fleetSGRRef })
		}
	}
	for _, u := range urls {
		paint(u[0], u[1], func(s *fleetStyle) { s.fg, s.under = fleetSGRURL, true })
	}
	for off := 0; ; {
		i := strings.Index(text[off:], "(FLEET lean)")
		if i < 0 {
			break
		}
		from := off + i
		off = from + len("(FLEET lean)")
		if !inURL(from, off) {
			paint(from, off, func(s *fleetStyle) { s.fg, s.bold = fleetSGRLean, true })
		}
	}
	for i := range sty {
		if code[i] {
			sty[i].fg, sty[i].under = fleetSGRCode, false
		}
	}
	if marker != nil {
		paint(marker[0], marker[1], func(s *fleetStyle) {
			*s = fleetStyle{dim: true}
		})
	}
	return vis, sty
}

// fleetWrapStyled is wrapRunes over vis, with each output rune given the style
// of the visible rune it came from. A space wrapRunes puts between two words
// keeps the style only when both sides share it, so an underline or a colour
// never runs past its span's last rune.
func fleetWrapStyled(vis []rune, sty []fleetStyle, width int) []string {
	if width < 1 {
		return []string{fleetStyledText(vis, sty)}
	}
	wrapped := wrapRunes(string(vis), width)
	out := make([]string, 0, len(wrapped))
	src := 0
	next := func() int {
		for src < len(vis) && unicode.IsSpace(vis[src]) {
			src++
		}
		return src
	}
	for _, w := range wrapped {
		rs := []rune(w)
		ls := make([]fleetStyle, len(rs))
		for i, r := range rs {
			if unicode.IsSpace(r) {
				if i > 0 && next() < len(vis) && sty[src] == ls[i-1] {
					ls[i] = ls[i-1]
				}
				continue
			}
			if next() < len(vis) {
				ls[i] = sty[src]
				src++
			}
		}
		out = append(out, fleetStyledText(rs, ls))
	}
	return out
}

// fleetStyledText writes rs with one colorize call per run of equal styles.
func fleetStyledText(rs []rune, sty []fleetStyle) string {
	var b strings.Builder
	for i := 0; i < len(rs); {
		j := i + 1
		for j < len(rs) && sty[j] == sty[i] {
			j++
		}
		b.WriteString(colorize(sty[i].sgr(), string(rs[i:j])))
		i = j
	}
	return b.String()
}
