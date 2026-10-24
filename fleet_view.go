package main

import (
	"regexp"
	"sort"
	"strings"
	"time"
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

// fleetSummaryLines wraps a card summary to width for the terminal. Fenced
// blocks stay verbatim; elsewhere ** and backticks are removed and list
// items hang-indent under their marker.
func fleetSummaryLines(s string, width int) []string {
	var out []string
	fence := false
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			fence = !fence
			out = append(out, line)
			continue
		}
		if fence {
			out = append(out, line)
			continue
		}
		line = strings.NewReplacer("**", "", "`", "").Replace(line)
		if m := fleetListMarkerRe.FindString(line); m != "" {
			mw := len([]rune(m))
			wrapped := wrapRunes(line[len(m):], width-mw)
			pad := strings.Repeat(" ", mw)
			for i, w := range wrapped {
				if i == 0 {
					out = append(out, m+w)
				} else {
					out = append(out, pad+w)
				}
			}
			continue
		}
		if strings.TrimSpace(line) == "" {
			out = append(out, "")
			continue
		}
		out = append(out, wrapRunes(line, width)...)
	}
	return out
}
