package main

import (
	"reflect"
	"strings"
	"testing"
	"time"
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
	got = fleetSummaryLines("run:\n```\n**x**  y\ngh pr merge 3230 --merge\n```\nend", w)
	want := []string{"run:", "    **x**  y", "    gh pr merge 3230", "    --merge", "end"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("fence: %q", got)
	}
}

// fleetSummarySample is a card body with every markup the TUI styles.
const fleetSummarySample = "**READY (2026-10-25)** Head 13f786194, shared 0.522.0.\n" +
	"\n" +
	"**Issue**\n" +
	"- DR-5692: PR #3230 adds counters. See https://github.com/TRECS-SOFTWARE-LABS/trecs-brain/pull/3230 for `module-exchange` rows.\n" +
	"\n" +
	"**Fix**\n" +
	"1. Run:\n" +
	"```\n" +
	"gh pr merge 3230 --merge --delete-branch\n" +
	"```\n" +
	"\n" +
	"**Options**\n" +
	"- **done** — Merged. (FLEET lean) It is green."

// fleetSummaryLinesV1 is fleetSummaryLines before the styled variant, kept to
// prove the plain path did not change.
func fleetSummaryLinesV1(s string, width int) []string {
	var out []string
	fence := false
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			fence = !fence
			continue
		}
		if fence {
			cw := width - len(fleetCodeIndent)
			if len([]rune(line)) <= cw || strings.TrimSpace(line) == "" {
				out = append(out, fleetCodeIndent+line)
				continue
			}
			for _, w := range wrapRunes(line, cw) {
				out = append(out, fleetCodeIndent+w)
			}
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

var fleetSummaryCorpus = []string{
	fleetSummarySample,
	"**Bold** and `code`",
	"one\n\ntwo",
	"- alpha beta gamma delta epsilon",
	"12. alpha beta gamma delta",
	"run:\n```\n**x**  y\ngh pr merge 3230 --merge\n```\nend",
	"HOLD **unpaired and `open code",
	"  * nested  item with   gaps and ***stars***",
	"**a very long bold heading that wraps across lines**",
	"see https://example.com/a/b/c/d/e/f/g/h/i/j/k/l/m/n/o/p/q/r/s/t/u/v/w/x/y/z.",
	"tab\tseparated\u00a0nbsp and émoji ✓ DR-12345 #12",
	"```\nunterminated fence with a long line that must wrap somewhere",
	"",
	"- ",
}

func TestFleetSummaryPlainUnchanged(t *testing.T) {
	for _, s := range fleetSummaryCorpus {
		for _, w := range []int{0, 1, 3, 8, 20, 40, 77, 200} {
			want := fleetSummaryLinesV1(s, w)
			if got := fleetSummaryLines(s, w); !reflect.DeepEqual(got, want) {
				t.Errorf("plain w=%d %q:\n got %q\nwant %q", w, s, got, want)
			}
			styled := fleetSummaryStyledLines(s, w)
			if len(styled) != len(want) {
				t.Fatalf("styled w=%d %q: %d lines, want %d", w, s, len(styled), len(want))
			}
			for i, l := range styled {
				if p := stripANSI(l); p != want[i] {
					t.Errorf("styled w=%d %q line %d: visible %q, want %q", w, s, i, p, want[i])
				}
				if fleetOpenStyle(l) {
					t.Errorf("styled w=%d line %q ends with an open style", w, l)
				}
				if w >= 8 && visibleWidth(l) > w {
					t.Errorf("styled w=%d line %q is %d wide", w, l, visibleWidth(l))
				}
			}
		}
	}
}

// fleetOpenStyle reports whether line's last SGR is not a reset, so its style
// would leak past the line's end.
func fleetOpenStyle(line string) bool {
	i := strings.LastIndex(line, "\033[")
	return i >= 0 && !strings.HasPrefix(line[i:], ansiReset)
}

// fleetRunStyle is the SGR in force at the first byte of needle in line.
func fleetRunStyle(t *testing.T, line, needle string) string {
	t.Helper()
	i := strings.Index(line, needle)
	if i < 0 {
		t.Fatalf("%q not in %q", needle, line)
	}
	open := strings.LastIndex(line[:i], "\033[")
	if open < 0 {
		return ""
	}
	code := line[open+2 : open+strings.IndexByte(line[open:], 'm')]
	if code == "0" {
		return ""
	}
	return code
}

func TestFleetSummaryStyles(t *testing.T) {
	one := func(s string) string {
		t.Helper()
		l := fleetSummaryStyledLines(s, 80)
		if len(l) != 1 {
			t.Fatalf("%q: %d lines", s, len(l))
		}
		return l[0]
	}
	for _, c := range []struct{ in, needle, sgr string }{
		{"**Issue**", "Issue", "1;33"},
		{"**Decision (Andy)**", "(Andy)", "1;33"},
		{"plain **bold** text", "bold", "1"},
		{"plain **bold** text", " text", ""},
		{"run `make` now", "make", "36"},
		{"see https://h/x.", "https://h/x", "4;34"},
		{"see https://h/x.", ".", ""},
		{"(see https://h/x)", ")", ""},
		{"ticket DR-5692 done", "DR-5692", "35"},
		{"pr #3230 open", "#3230", "35"},
		{"pr #12 open", "#12", ""},
		{"https://h/pull/3230#1234 DR-1234", "DR-1234", "35"},
		{"go (FLEET lean) now", "(FLEET lean)", "1;32"},
		{"HOLD the merge", "HOLD", "1;31"},
		{"**READY (x)** go", "READY", "1;32"},
		{"**READY (x)** go", "(x)", "1"},
		{"UPDATE: x", "UPDATE", "33"},
		{"RESULT x", "RESULT", "33"},
		{"- item", "-", "2"},
		{"- item", "item", ""},
		{"12. item", "12.", "2"},
		{"`DR-1234 https://x.y`", "DR-1234", "36"},
	} {
		if got := fleetRunStyle(t, one(c.in), c.needle); got != c.sgr {
			t.Errorf("%q: %q has SGR %q, want %q", c.in, c.needle, got, c.sgr)
		}
	}
	// The URL contains a ref; it stays URL-blue.
	if got := fleetRunStyle(t, one("https://h/pull/3230#1234"), "3230"); got != "4;34" {
		t.Errorf("ref inside URL: %q", got)
	}
	// A status word counts only on the summary's first line.
	l := fleetSummaryStyledLines("first\nHOLD later", 80)
	if got := fleetRunStyle(t, l[1], "HOLD"); got != "" {
		t.Errorf("status on line 2: %q", got)
	}
	// A fenced code line is cyan; the indent is not styled.
	l = fleetSummaryStyledLines("```\nmake test\n```", 80)
	if l[0] != fleetCodeIndent+"\033[36mmake test"+ansiReset {
		t.Errorf("fence: %q", l[0])
	}
	// A bold span wrapped across lines keeps bold on each line.
	l = fleetSummaryStyledLines("x **alpha beta gamma delta** y", 12)
	if len(l) < 3 {
		t.Fatalf("wrap: %q", l)
	}
	for _, w := range []string{"alpha", "gamma"} {
		found := false
		for _, line := range l {
			if strings.Contains(line, w) {
				found = true
				if got := fleetRunStyle(t, line, w); got != "1" {
					t.Errorf("wrapped %q: SGR %q in %q", w, got, line)
				}
				if !strings.HasSuffix(line, ansiReset) {
					t.Errorf("wrapped line %q has no reset", line)
				}
			}
		}
		if !found {
			t.Errorf("%q missing from %q", w, l)
		}
	}
	// The underline stops at the URL's end, not on the space after it.
	if got := one("https://x.y z"); got != "\033[4;34mhttps://x.y"+ansiReset+" z" {
		t.Errorf("URL space: %q", got)
	}
}

// TestFleetSummaryFrame renders a card with the sample body in the TUI at
// several widths: no frame line is wider than the terminal, and none carries
// an open style at its end.
func TestFleetSummaryFrame(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	c := fleetTestCard("a", "needs-you")
	c.Summary = fleetSummarySample
	for _, cols := range []int{20, 33, 60, 120} {
		frame := renderFleetView(fleetTestState(fleetTestView(c), now), cols, 60, now)
		for _, l := range strings.Split(frame, "\n") {
			if visibleWidth(l) > cols {
				t.Errorf("cols=%d: line %q is %d wide", cols, l, visibleWidth(l))
			}
			if fleetOpenStyle(l) {
				t.Errorf("cols=%d: line %q ends with an open style", cols, l)
			}
		}
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
