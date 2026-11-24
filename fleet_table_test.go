package main

import (
	"encoding/json"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

func TestFleetTablePlain(t *testing.T) {
	s := "Verdicts:\n| Doc | Verdict |\n|---|---:|\n| a | ok |\n| b\\|c | **no** |\nafter"
	want := []string{
		"Verdicts:",
		"Doc │ Verdict",
		"────┼────────",
		"a   │      ok",
		"b|c │      no",
		"after",
	}
	if got := fleetSummaryLines(s, 80); !reflect.DeepEqual(got, want) {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestFleetTableWrapsAndRules(t *testing.T) {
	s := "| Document | Verdict |\n|---|---|\n| Processing register and retention schedule | Sign as baseline v1. |\n| x | y |"
	want := []string{
		"Document     │ Verdict",
		"─────────────┼─────────────",
		"Processing   │ Sign as",
		"register and │ baseline v1.",
		"retention    │",
		"schedule     │",
		"─────────────┼─────────────",
		"x            │ y",
	}
	if got := fleetSummaryLines(s, 27); !reflect.DeepEqual(got, want) {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestFleetTableNarrowFallsBackToList(t *testing.T) {
	s := "| Doc | Verdict | Owner |\n|---|---|---|\n| alpha | ready | me |"
	want := []string{"- alpha · Verdict:", "  ready · Owner: me"}
	if got := fleetSummaryLines(s, 20); !reflect.DeepEqual(got, want) {
		t.Errorf("got %q\nwant %q", got, want)
	}
	if got := fleetSummaryLines("| Alpha | Beta | Gamma |\n|---|---|---|", 16); !reflect.DeepEqual(got, []string{"- Alpha · Beta ·", "  Gamma"}) {
		t.Errorf("header-only: got %q", got)
	}
}

func TestFleetTableNotATable(t *testing.T) {
	for _, s := range []string{
		"| a | b |\nno separator",
		"| a | b |\n|--|--|",     // a rule needs three dashes
		"```\n| a |\n|---|\n```", // inside a fence
		"a | b\n|---|---|",       // header has no leading pipe
	} {
		got := fleetSummaryLines(s, 80)
		for _, l := range got {
			if strings.Contains(l, "│") || strings.Contains(l, "┼") {
				t.Errorf("%q rendered as a table: %q", s, got)
			}
		}
	}
}

func TestFleetTableStyledFits(t *testing.T) {
	s := "**Verdicts:**\n\n| Document | Verdict |\n|:-:|---|\n" +
		"| Processing register and [retention](https://example.com/r) schedule | Sign as baseline v1. Not yet an Article 30 record. DR-1234 |\n" +
		"| **The 4 DPIAs** | **Not ready.** They're scoping drafts with no risk rating. |\n" +
		"| ✓ 漢字 | `code cell` |\n\nafter"
	for _, w := range []int{0, 1, 3, 8, 12, 20, 27, 40, 60, 77, 120, 200} {
		plain := fleetSummaryLines(s, w)
		styled := fleetSummaryStyledLines(s, w)
		if len(plain) != len(styled) {
			t.Fatalf("w=%d: %d styled lines, %d plain", w, len(styled), len(plain))
		}
		for i, l := range styled {
			if p := stripANSI(l); p != plain[i] {
				t.Errorf("w=%d line %d: visible %q, plain %q", w, i, p, plain[i])
			}
			if fleetOpenStyle(l) {
				t.Errorf("w=%d line %q ends with an open style", w, l)
			}
			if w >= 8 && visibleWidth(l) > w {
				t.Errorf("w=%d line %q is %d wide", w, l, visibleWidth(l))
			}
		}
	}
	// An all-bold cell is bold, not heading-coloured.
	for _, l := range fleetSummaryStyledLines("| **x** |\n|---|", 80) {
		if strings.Contains(l, "\033[1;"+fleetSGRHeading+"m") {
			t.Errorf("cell drawn as heading: %q", l)
		}
	}
}

func TestFleetTableRaggedRows(t *testing.T) {
	// The header sets the columns, whatever the rule row or a body row holds.
	s := "| a | b |\n---\n| 1 |\n| 2 | 3 | 4 |"
	want := []string{"a │ b", "──┼──", "1 │", "2 │ 3"}
	if got := fleetSummaryLines(s, 80); !reflect.DeepEqual(got, want) {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

// TestFleetTableMatchesPage runs the page's TABLE_RULE and tableCells under
// node on the same rows as fleetTableSepRe and fleetTableCells, so the TUI and
// the page agree on what is a table and where its cells split. Skipped where
// node is not installed.
func TestFleetTableMatchesPage(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	page := string(fleetPageHTML)
	i := strings.Index(page, "  const TABLE_RULE = ")
	j := strings.Index(page, "  // richTable builds")
	if i < 0 || j < i {
		t.Fatal("fleet.html: TABLE_RULE .. richTable not found in order")
	}
	rows := []string{
		"|---|---|", "---", "|--|", "| :--- | ---: | :-: |", "a | b", "|-|-|",
		"| a | b |", "a|b|", "| a |", "| `x|y` | z |", "| a \\| b |", "| a \\|",
		"|", "||", "| |  |", "  | é | ✓ |  ", "| `open | b |", "| a | b | ",
	}
	js, _ := json.Marshal(rows)
	src := page[i:j] + `console.log(JSON.stringify(` + string(js) + `.map(r => [TABLE_RULE.test(r), tableCells(r)])));`
	out, err := exec.Command(node, "-e", src).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v: %s", err, out)
	}
	var got [][2]json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil || len(got) != len(rows) {
		t.Fatalf("node output %q: %v", out, err)
	}
	for k, r := range rows {
		var rule bool
		var cells []string
		if json.Unmarshal(got[k][0], &rule) != nil || json.Unmarshal(got[k][1], &cells) != nil {
			t.Fatalf("row %q: bad node result %s", r, got[k])
		}
		if g := fleetTableSepRe.MatchString(r); g != rule {
			t.Errorf("rule %q: TUI %v, page %v", r, g, rule)
		}
		if g := fleetTableCells(r); !reflect.DeepEqual(g, cells) {
			t.Errorf("cells %q: TUI %q, page %q", r, g, cells)
		}
	}
}
