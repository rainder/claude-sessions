package main

import "testing"

func TestRuneCells(t *testing.T) {
	for _, c := range []struct {
		r    rune
		want int
	}{
		{'a', 1}, {'─', 1}, {'▸', 1}, {'✓', 1}, {'·', 1}, {'●', 1}, {'★', 1}, {'…', 1},
		{'⛵', 2}, {'⚓', 2}, {'⏳', 2}, {'界', 2}, {'한', 2}, {'Ａ', 2}, {'🚀', 2},
	} {
		if got := runeCells(c.r); got != c.want {
			t.Errorf("runeCells(%q) = %d, want %d", c.r, got, c.want)
		}
	}
}

func TestVisualLenCountsWideRunes(t *testing.T) {
	if got := visualLen(dim("⛵ at sea") + " ✓"); got != 11 {
		t.Fatalf("visualLen = %d, want 11", got)
	}
	if got := cellWidth("⚓⏳x"); got != 5 {
		t.Fatalf("cellWidth = %d, want 5", got)
	}
}

func TestClipLineWideRunes(t *testing.T) {
	for _, c := range []struct {
		in    string
		width int
		want  string
	}{
		{"⛵⛵⛵", 4, "⛵⛵"},
		{"⛵⛵⛵", 5, "⛵⛵"}, // a wide rune never straddles the edge
		{"a⛵b", 2, "a"},  // and nothing after it is drawn
		{"\x1b[2m⚓x\x1b[0m", 2, "\x1b[2m⚓\x1b[0m"},
	} {
		if got := clipLine(c.in, c.width); got != c.want {
			t.Errorf("clipLine(%q, %d) = %q, want %q", c.in, c.width, got, c.want)
		}
		if w := visualLen(clipLine(c.in, c.width)); w > c.width {
			t.Errorf("clipLine(%q, %d) is %d wide", c.in, c.width, w)
		}
	}
}

func TestTruncatePadCells(t *testing.T) {
	if got := truncateCells("⛵⛵⛵", 5); got != "⛵⛵…" {
		t.Fatalf("truncateCells = %q", got)
	}
	if got := truncateCells("⛵⛵⛵", 4); got != "⛵…" {
		t.Fatalf("truncateCells = %q", got)
	}
	if got := truncateCells("abc", 3); got != "abc" {
		t.Fatalf("truncateCells = %q", got)
	}
	if got := padCells("⚓", 4); got != "⚓  " {
		t.Fatalf("padCells = %q", got)
	}
	if got := padCellsLeft("⏳", 3); got != " ⏳" {
		t.Fatalf("padCellsLeft = %q", got)
	}
}
