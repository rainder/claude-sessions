package main

import (
	"regexp"
	"sort"
	"strings"
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
	sort.SliceStable(items, func(i, j int) bool { return items[i].Section < items[j].Section })
	return items
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

// fleetEditComment applies one key to the comment box. The bool reports
// whether the key is an edit key (printable ASCII, backspace); a printable
// byte that would push the send text past sendKeysMaxLen is dropped.
func fleetEditComment(comment, key, send string) (string, bool) {
	switch {
	case key == "\x7f" || key == "\x08":
		if comment != "" {
			comment = comment[:len(comment)-1]
		}
		return comment, true
	case len(key) == 1 && key[0] >= 0x20 && key[0] <= 0x7e:
		if len(comment)+1 <= sendKeysMaxLen && len(fleetSendText(send, comment+key)) <= sendKeysMaxLen {
			comment += key
		}
		return comment, true
	}
	return comment, false
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
