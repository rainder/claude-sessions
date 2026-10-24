package main

import (
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func fleetViewFixture() fleetStateView {
	sent := fleetAnswer{CardHash: "h", Attempts: []fleetAttempt{{OptionKey: "a", Status: fleetAttemptSent}}}
	unconf := fleetAnswer{CardHash: "h", Attempts: []fleetAttempt{{OptionKey: "a", Status: fleetAttemptUnconfirmed}}}
	return fleetStateView{
		Cards: []fleetCardView{
			{ID: "a", Kind: "explicit-yes"},
			{ID: "b", Kind: "needs-you"},
			{ID: "c", Kind: "needs-you"},
			{ID: "d", Kind: "done"},
			{Kind: "needs-you", Title: "e"},
			{ID: "f", Kind: "explicit-yes"},
		},
		Answered: map[string]fleetAnswer{"b": unconf, "c": sent},
	}
}

func TestFleetItemsSections(t *testing.T) {
	items := fleetItems(fleetViewFixture())
	type row struct {
		id  string
		sec fleetSection
	}
	var got []row
	for _, it := range items {
		got = append(got, row{it.Card.ID, it.Section})
	}
	want := []row{
		{"a", fleetSecApprove}, {"f", fleetSecApprove},
		{"b", fleetSecDecide}, {"", fleetSecDecide},
		{"c", fleetSecSent},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if items[2].Answer == nil || items[0].Answer != nil {
		t.Fatalf("answer pointers wrong: %+v", items)
	}
}

func TestCountFleetCards(t *testing.T) {
	got := countFleetCards(fleetViewFixture())
	if got != (fleetCardCounts{Approve: 2, Decide: 1, Sent: 1}) {
		t.Fatalf("got %+v", got)
	}
}

func TestFleetOptionIndex(t *testing.T) {
	ok := []struct {
		k    string
		n    int
		want int
	}{{"1", 3, 0}, {"3", 3, 2}, {"9", 12, 8}}
	for _, c := range ok {
		if i, b := fleetOptionIndex(c.k, c.n); !b || i != c.want {
			t.Errorf("(%q,%d) = %d,%v", c.k, c.n, i, b)
		}
	}
	for _, c := range []struct {
		k string
		n int
	}{{"4", 3}, {"0", 3}, {"a", 3}, {"10", 12}, {KeyUp, 3}} {
		if _, b := fleetOptionIndex(c.k, c.n); b {
			t.Errorf("(%q,%d) should be false", c.k, c.n)
		}
	}
}

func TestFleetSendText(t *testing.T) {
	for _, c := range [][3]string{
		{"Merge now", "", "Merge now"},
		{"x", "  a \t b  ", "x Andy adds: a b"},
		{"x", "   ", "x"},
		{"x", "a\nb\r\n c", "x Andy adds: a b c"},
	} {
		if got := fleetSendText(c[0], c[1]); got != c[2] {
			t.Errorf("(%q,%q) = %q want %q", c[0], c[1], got, c[2])
		}
	}
}

func TestFleetEditComment(t *testing.T) {
	cases := []struct {
		comment, key, want string
		ok                 bool
	}{
		{"ab", "c", "abc", true},
		{"ab", "\x7f", "a", true},
		{"ab", "\x08", "a", true},
		{"", "\x7f", "", true},
		{"ab", "\x1b", "ab", false},
		{"ab", KeyUp, "ab", false},
		{"ab", "\xc3", "ab", false},
	}
	for _, c := range cases {
		got, ok := fleetEditComment(c.comment, c.key, "x")
		if got != c.want || ok != c.ok {
			t.Errorf("(%q,%q) = %q,%v want %q,%v", c.comment, c.key, got, ok, c.want, c.ok)
		}
	}
	send := strings.Repeat("x", 4083)
	if got, _ := fleetEditComment("", "a", send); got != "a" {
		t.Errorf("first byte refused: %q", got)
	}
	if got, _ := fleetEditComment("a", "b", send); got != "a" {
		t.Errorf("second byte accepted: %q", got)
	}
}

func TestFleetSummaryLines(t *testing.T) {
	const w = 20
	if got := fleetSummaryLines("**Bold** and `code`", w); !reflect.DeepEqual(got, []string{"Bold and code"}) {
		t.Errorf("markup: %q", got)
	}
	if got := fleetSummaryLines("one\n\ntwo", w); !reflect.DeepEqual(got, []string{"one", "", "two"}) {
		t.Errorf("blank: %q", got)
	}
	got := fleetSummaryLines("- alpha beta gamma delta epsilon", w)
	if len(got) < 2 || !strings.HasPrefix(got[0], "- ") {
		t.Fatalf("list: %q", got)
	}
	for i, l := range got {
		if utf8.RuneCountInString(l) > w {
			t.Errorf("line too long: %q", l)
		}
		if i > 0 && !strings.HasPrefix(l, "  ") {
			t.Errorf("continuation not indented: %q", l)
		}
	}
	got = fleetSummaryLines("12. alpha beta gamma delta", w)
	if len(got) < 2 || !strings.HasPrefix(got[0], "12. ") {
		t.Fatalf("numbered: %q", got)
	}
	for _, l := range got[1:] {
		if !strings.HasPrefix(l, "    ") || strings.HasPrefix(l, "     ") {
			t.Errorf("continuation indent != 4: %q", l)
		}
	}
	long := strings.Repeat("y", 30)
	got = fleetSummaryLines("```\n"+long+"\n**x**\n```", w)
	want := []string{"```", long, "**x**", "```"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("fence: %q", got)
	}
}

func TestFleetOtherText(t *testing.T) {
	for _, c := range [][3]string{
		{"dr-1", "merge it", "Andy decides on dr-1: merge it"},
		{"dr-1", "  a \t b  ", "Andy decides on dr-1: a b"},
		{"dr-1", "a\nb\r\n c", "Andy decides on dr-1: a b c"},
	} {
		if got := fleetOtherText(c[0], c[1]); got != c[2] {
			t.Errorf("(%q,%q) = %q want %q", c[0], c[1], got, c[2])
		}
	}
}

func TestFleetEditOther(t *testing.T) {
	if got, ok := fleetEditOther("ab", "c", "a"); got != "abc" || !ok {
		t.Errorf("printable: %q,%v", got, ok)
	}
	if got, ok := fleetEditOther("ab", "\x7f", "a"); got != "a" || !ok {
		t.Errorf("backspace: %q,%v", got, ok)
	}
	if got, ok := fleetEditOther("ab", KeyUp, "a"); got != "ab" || ok {
		t.Errorf("arrow: %q,%v", got, ok)
	}
	// The cap counts the "Andy decides on <id>: " prefix.
	id := strings.Repeat("x", sendKeysMaxLen-len("Andy decides on : ")-1)
	if got, _ := fleetEditOther("", "a", id); got != "a" {
		t.Errorf("first byte refused: %q", got)
	}
	if got, _ := fleetEditOther("a", "b", id); got != "a" {
		t.Errorf("second byte accepted: %q", got)
	}
}

func TestFleetItemsSortByPriorityThenAge(t *testing.T) {
	sent := fleetAnswer{CardHash: "h", Attempts: []fleetAttempt{{OptionKey: "a", Status: fleetAttemptSent}}}
	v := fleetStateView{
		Cards: []fleetCardView{
			{ID: "n-new", Kind: "needs-you", FirstSeenAt: "2026-10-24T12:00:00Z"},
			{ID: "y-low", Kind: "explicit-yes", Priority: 4, FirstSeenAt: "2026-10-24T08:00:00Z"},
			{ID: "n-none", Kind: "needs-you"},
			{ID: "n-urgent", Kind: "needs-you", Priority: 1, FirstSeenAt: "2026-10-24T13:00:00Z"},
			{ID: "s-2", Kind: "needs-you", Priority: 4},
			{ID: "n-old", Kind: "needs-you", FirstSeenAt: "2026-10-24T09:00:00Z"},
			{ID: "y-high", Kind: "explicit-yes", Priority: 2, FirstSeenAt: "2026-10-24T11:00:00Z"},
			{ID: "s-1", Kind: "explicit-yes", Priority: 1},
			{ID: "n-bad", Kind: "needs-you", FirstSeenAt: "yesterday"},
		},
		Answered: map[string]fleetAnswer{"s-1": sent, "s-2": sent},
	}
	var got []string
	for _, it := range fleetItems(v) {
		got = append(got, it.Card.ID)
	}
	want := []string{
		"y-high", "y-low", // APPROVE: priority 2 before 4
		"n-urgent", "n-old", "n-new", "n-none", "n-bad", // DECIDE: P1, then P3 oldest first, no time last in state order
		"s-2", "s-1", // SENT: state.json order
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
}
