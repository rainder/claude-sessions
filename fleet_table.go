package main

import (
	"regexp"
	"strings"
)

// fleetTableSepRe matches a markdown table's delimiter row (|---|:--:|),
// without the pipe check: fleetTableAt also wants one "|" so a bare "---"
// stays text.
var fleetTableSepRe = regexp.MustCompile(`^\s*\|?\s*:?-+:?\s*(\|\s*:?-+:?\s*)*\|?\s*$`)

// fleetTableMinCol is the narrowest a column shrinks to before a table falls
// back to one list item per row.
const fleetTableMinCol = 6

// fleetTableSep sits between two columns: one space each side of a bar.
const fleetTableSep = " │ "

// fleetTableAt reads a markdown table at the top of lines: a "|" header row,
// a delimiter row with the same cell count, then every following "|" row. It
// returns the header cells, each column's alignment ('l', 'r' or 'c'), the body
// rows padded or cut to the header's width, and how many lines it used; n is 0
// when lines does not start with a table.
func fleetTableAt(lines []string) (head []string, align []byte, rows [][]string, n int) {
	if len(lines) < 2 || !strings.HasPrefix(strings.TrimSpace(lines[0]), "|") ||
		!strings.Contains(lines[1], "|") || !fleetTableSepRe.MatchString(lines[1]) {
		return nil, nil, nil, 0
	}
	head = fleetTableCells(lines[0])
	seps := fleetTableCells(lines[1])
	if len(head) != len(seps) {
		return nil, nil, nil, 0
	}
	for _, c := range seps {
		switch {
		case strings.HasPrefix(c, ":") && strings.HasSuffix(c, ":"):
			align = append(align, 'c')
		case strings.HasSuffix(c, ":"):
			align = append(align, 'r')
		default:
			align = append(align, 'l')
		}
	}
	n = 2
	for ; n < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[n]), "|"); n++ {
		row := fleetTableCells(lines[n])
		for len(row) < len(head) {
			row = append(row, "")
		}
		rows = append(rows, row[:len(head)])
	}
	return head, align, rows, n
}

// fleetTableCells splits one table row on its unescaped pipes, drops the
// outer ones and trims each cell; "\|" is a literal pipe.
func fleetTableCells(line string) []string {
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(line, "|")
	if strings.HasSuffix(line, "|") && !strings.HasSuffix(line, "\\|") {
		line = line[:len(line)-1]
	}
	var cells []string
	var cur strings.Builder
	for i := 0; i < len(line); i++ {
		switch {
		case line[i] == '\\' && i+1 < len(line) && line[i+1] == '|':
			cur.WriteByte('|')
			i++
		case line[i] == '|':
			cells = append(cells, strings.TrimSpace(cur.String()))
			cur.Reset()
		default:
			cur.WriteByte(line[i])
		}
	}
	return append(cells, strings.TrimSpace(cur.String()))
}

// fleetTableRender draws a table in width columns: cells styled as
// fleetLineStyles says (header bold), columns padded to one width and joined
// by fleetTableSep, a rule under the header, and a cell too wide for its
// column wrapped inside it. Columns shrink widest-first to fit; when even
// fleetTableMinCol each will not fit, every body row becomes one list item
// instead (first cell, then "Header: cell" for the rest, joined by " · ").
func fleetTableRender(head []string, align []byte, rows [][]string, width int, styled bool) []string {
	type cell struct {
		vis []rune
		sty []fleetStyle
	}
	parse := func(s string, isHead bool) cell {
		vis, sty := fleetLineStyles(s, false)
		for i := range sty {
			if sty[i].fg == fleetSGRHeading { // an all-bold cell is not a heading
				sty[i].fg = ""
			}
		}
		if !styled {
			sty = make([]fleetStyle, len(vis))
		} else if isHead {
			for i := range sty {
				sty[i].bold = true
			}
		}
		return cell{vis, sty}
	}
	grid := make([][]cell, 0, len(rows)+1)
	for r, row := range append([][]string{head}, rows...) {
		cs := make([]cell, len(row))
		for j, s := range row {
			cs[j] = parse(s, r == 0)
		}
		grid = append(grid, cs)
	}
	cols := len(head)
	dimIf := func(s string) string {
		if styled {
			return dim(s)
		}
		return s
	}

	nat := make([]int, cols)
	for _, row := range grid {
		for j, c := range row {
			nat[j] = max(nat[j], cellWidth(string(c.vis)))
		}
	}
	avail := width - len([]rune(fleetTableSep))*(cols-1)
	floor := 0
	for _, w := range nat {
		floor += min(max(w, 1), fleetTableMinCol)
	}
	if floor > avail {
		return fleetTableAsList(grid[0], grid[1:], width, styled, func(c cell) ([]rune, []fleetStyle) { return c.vis, c.sty })
	}
	cw := make([]int, cols)
	total := 0
	for j, w := range nat {
		cw[j] = max(w, 1)
		total += cw[j]
	}
	for total > avail {
		wide := 0
		for j := range cw {
			if cw[j] > cw[wide] {
				wide = j
			}
		}
		cw[wide]--
		total--
	}

	var out []string
	tall := false // some body row wraps: rule between body rows too
	emit := func(row []cell) int {
		wrapped := make([][]string, cols)
		height := 1
		for j, c := range row {
			if len(c.vis) == 0 {
				wrapped[j] = []string{""}
				continue
			}
			wrapped[j] = fleetWrapStyled(c.vis, c.sty, cw[j])
			height = max(height, len(wrapped[j]))
		}
		for k := 0; k < height; k++ {
			var b strings.Builder
			for j := range cols {
				text := ""
				if k < len(wrapped[j]) {
					text = wrapped[j][k]
				}
				gap := max(cw[j]-visualLen(text), 0)
				left := 0
				switch align[j] {
				case 'r':
					left = gap
				case 'c':
					left = gap / 2
				}
				right := gap - left
				if j == cols-1 {
					right = 0
				}
				if j > 0 {
					b.WriteString(dimIf(fleetTableSep))
				}
				b.WriteString(strings.Repeat(" ", left) + text + strings.Repeat(" ", right))
			}
			out = append(out, strings.TrimRight(b.String(), " "))
		}
		return height
	}
	var parts []string
	for _, w := range cw {
		parts = append(parts, strings.Repeat("─", w))
	}
	rule := dimIf(strings.Join(parts, "─┼─"))
	emit(grid[0])
	out = append(out, rule)
	body := out
	for _, row := range grid[1:] {
		tall = emit(row) > 1 || tall
	}
	if !tall {
		return out
	}
	// Redraw the body with a rule between rows, so a wrapped row reads as one.
	out = body
	for i, row := range grid[1:] {
		if i > 0 {
			out = append(out, rule)
		}
		emit(row)
	}
	return out
}

// fleetTableAsList is fleetTableRender's narrow fallback: one "- " item per
// body row, hang-indented as a list item is. A table with no body rows lists
// its header cells once.
func fleetTableAsList[C any](head []C, rows [][]C, width int, styled bool, parts func(C) ([]rune, []fleetStyle)) []string {
	const marker = "- "
	var mark fleetStyle
	if styled {
		mark = fleetStyle{dim: true}
	}
	join := func(row []C, labels bool) ([]rune, []fleetStyle) {
		var vis []rune
		var sty []fleetStyle
		add := func(rs []rune, ss []fleetStyle) {
			vis, sty = append(vis, rs...), append(sty, ss...)
		}
		plain := func(s string, st fleetStyle) {
			rs := []rune(s)
			ss := make([]fleetStyle, len(rs))
			for i := range ss {
				ss[i] = st
			}
			add(rs, ss)
		}
		for j, c := range row {
			cv, cs := parts(c)
			if j > 0 {
				plain(" · ", mark)
				if labels {
					hv, hs := parts(head[j])
					if len(hv) > 0 {
						add(hv, hs)
						plain(": ", fleetStyle{})
					}
				}
			}
			add(cv, cs)
		}
		return vis, sty
	}
	item := func(vis []rune, sty []fleetStyle) []string {
		var out []string
		for i, w := range fleetWrapStyled(vis, sty, width-len(marker)) {
			if i == 0 {
				out = append(out, colorize(mark.sgr(), marker)+w)
			} else {
				out = append(out, strings.Repeat(" ", len(marker))+w)
			}
		}
		return out
	}
	if len(rows) == 0 {
		return item(join(head, false))
	}
	var out []string
	for _, row := range rows {
		out = append(out, item(join(row, true))...)
	}
	return out
}
