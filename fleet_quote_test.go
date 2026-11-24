package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestFleetHeadingsAndQuotes(t *testing.T) {
	s := "### 3. Onboarding (IAC-8) ###\n#### four\n####### seven\n#nospace\n" +
		"> Hi Dzhamal, two items.\n> 1. first item here\n>\n> > nested\nafter"
	want := []string{
		"3. Onboarding",
		"(IAC-8)",
		"four",
		"####### seven",
		"#nospace",
		"│ Hi Dzhamal, two",
		"│ items.",
		"│ 1. first item",
		"│    here",
		"│",
		"│ │ nested",
		"after",
	}
	if got := fleetSummaryLines(s, 18); !reflect.DeepEqual(got, want) {
		t.Errorf("got %q\nwant %q", got, want)
	}
	styled := fleetSummaryStyledLines("### Title `x`", 80)
	if len(styled) != 1 || !strings.Contains(styled[0], "\033[1;"+fleetSGRHeading+"m") ||
		!strings.Contains(styled[0], fleetSGRCode) || stripANSI(styled[0]) != "Title x" {
		t.Errorf("styled heading %q", styled)
	}
	if q := fleetSummaryStyledLines("> hi", 80); len(q) != 1 || stripANSI(q[0]) != "│ hi" || !strings.HasPrefix(q[0], ansiDim) {
		t.Errorf("styled quote %q", q)
	}
}

func TestFleetQuoteDepthCapped(t *testing.T) {
	deep := strings.Repeat(">", 10000) + " x"
	got := fleetSummaryLines(deep, 200)
	if len(got) == 0 || !strings.HasPrefix(got[0], strings.Repeat("│ ", fleetQuoteDepthMax)) {
		t.Fatalf("got %q", got[:min(len(got), 2)])
	}
	if strings.HasPrefix(got[0], strings.Repeat("│ ", fleetQuoteDepthMax+1)) {
		t.Errorf("nested past the cap")
	}
}
