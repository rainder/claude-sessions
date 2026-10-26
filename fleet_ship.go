package main

import (
	"fmt"
	"strings"
	"time"
)

// The ship screen: what the Fleet view shows while FLEET is live and no card
// needs Andy (no open card, no unconfirmed answer; SENT cards do not count).
// A small animated scene sits centered above a few text lines: "all clear,
// captain", FLEET's status with the session and done-today counts, the last
// done card, and how many answers wait for FLEET.
//
// The scene is a pure function of (frame, size): the modal loop only moves
// st.frame, every fleetShipFrame and only while this screen shows, so tests
// can draw any frame. Every string drawn is fixed text, a number, or a field
// the decoder already passed through fleetDisplayText.

const (
	fleetShipFrame = 250 * time.Millisecond // one animation frame (4 fps)

	fleetHelpShip     = "Esc back"
	fleetHelpShipSent = "Enter sent cards · Esc back"
	fleetShipTitle    = "all clear, captain"
	fleetShipTitleCut = 30 // a done card with no ticket shows this much of its title

	fleetShipBobHalf    = 3   // frames at rest, then as many raised: a 1.5 s bob
	fleetShipFlagEvery  = 2   // frames per flag state: |> then |~, 0.5 s each
	fleetShipGullEvery  = 100 // frames between gull crossings (25 s)
	fleetShipGullFrames = 24  // frames one crossing takes (6 s)
	fleetShipGullStart  = 40  // frame in each cycle the crossing starts (10 s in)

	fleetShipSGRTitle = "1;32"
	fleetShipSGRFlag  = "1;31"
	fleetShipSGRWater = "36"
	fleetShipSGRSea   = "2;36"
	fleetShipSGRGull  = "2"
)

// fleetSea is one scrolling strip of water: width columns from column x of
// the scene, showing pattern repeated and moved one column left every
// `every` frames. It repeats every len(pattern)*every frames.
type fleetSea struct {
	x, width int
	pattern  string
	every    int
	sgr      string
}

// period is how many frames the strip takes to come back to frame 0's look.
func (s fleetSea) period() int { return len([]rune(s.pattern)) * s.every }

// fleetShipArt is one size of the scene. ship is the hull and rig, top line
// first, its last line the hull; leading spaces place each line in the
// scene. A ship line is opaque from its first to its last non-space rune, so
// the hull's inside hides the water behind it. water runs along the hull's
// resting row (behind the hull), seas are the rows under it. sky adds a
// sky row for the gull and a reserved row the ship rises into when it bobs,
// both above the ship, so the scene's height never changes between frames.
type fleetShipArt struct {
	width int
	ship  []string
	flag  bool // ship[0] holds a "|>" flag that flutters
	water fleetSea
	seas  []fleetSea
	sky   bool
}

func (a fleetShipArt) height() int {
	h := len(a.ship) + len(a.seas)
	if a.sky {
		h += 2
	}
	return h
}

// The three sizes, largest first.
var (
	fleetShipTall = fleetShipArt{
		width: 39,
		ship: []string{
			`              |    |    |`,
			`             )_)  )_)  )_)`,
			`            )___))___))___)\`,
			`           )____)____)_____)\\`,
			`         _____|____|____|____\\\__`,
			`         \                   /`,
		},
		water: fleetSea{x: 0, width: 39, pattern: "-", every: 1, sgr: fleetShipSGRWater},
		seas: []fleetSea{
			{x: 2, width: 27, pattern: "^^^^^ ^^^^^^^^^^^^^^^^^^^^^ ", every: 4, sgr: fleetShipSGRWater},
			{x: 4, width: 28, pattern: "^^^^      ^^^^     ^^^    ^^    ", every: 3, sgr: fleetShipSGRSea},
			{x: 9, width: 13, pattern: "^^^^      ^^^      ", every: 2, sgr: fleetShipSGRSea},
		},
		sky: true,
	}
	fleetShipSail = fleetShipArt{
		width: 21,
		ship: []string{
			`        |>`,
			`        |\`,
			`        | \`,
			`        |  \`,
			`      __|___\__`,
			`      \_______/`,
		},
		flag:  true,
		water: fleetSea{x: 3, width: 15, pattern: "~~~ ~~~~ ~~ ", every: 4, sgr: fleetShipSGRWater},
		seas: []fleetSea{
			{x: 0, width: 21, pattern: "~   ~~    ~   ~~   ", every: 2, sgr: fleetShipSGRSea},
		},
		sky: true,
	}
	fleetShipOne = fleetShipArt{
		width: 12,
		ship:  []string{`    _|\_`},
		water: fleetSea{x: 0, width: 12, pattern: "~~~ ~~~~ ~~~ ", every: 4, sgr: fleetShipSGRWater},
	}
	fleetShipArts = []fleetShipArt{fleetShipTall, fleetShipSail, fleetShipOne}
)

// fleetShipBob is how many rows the ship is raised in frame (0 or 1).
func fleetShipBob(frame int) int {
	return (fleetShipMod(frame, 2*fleetShipBobHalf)) / fleetShipBobHalf
}

// fleetShipGullX is the gull's column on a width-wide sky row in frame, and
// false while no gull is in the sky. It crosses left to right.
func fleetShipGullX(frame, width int) (int, bool) {
	p := fleetShipMod(frame, fleetShipGullEvery) - fleetShipGullStart
	if p < 0 || p >= fleetShipGullFrames || width <= 0 {
		return 0, false
	}
	return p * (width - 1) / (fleetShipGullFrames - 1), true
}

func fleetShipMod(a, n int) int { return (a%n + n) % n }

// fleetShipCell is one scene column: its rune and the SGR it is drawn in.
type fleetShipCell struct {
	r   rune
	sgr string
}

// fleetShipScene draws art in frame: exactly art.height() lines, each at
// most art.width columns (trailing blanks trimmed).
func fleetShipScene(a fleetShipArt, frame int) []string {
	grid := make([][]fleetShipCell, a.height())
	for i := range grid {
		grid[i] = make([]fleetShipCell, a.width)
		for j := range grid[i] {
			grid[i][j] = fleetShipCell{r: ' '}
		}
	}
	put := func(row, col int, r rune, sgr string) {
		if row >= 0 && row < len(grid) && col >= 0 && col < a.width {
			grid[row][col] = fleetShipCell{r, sgr}
		}
	}
	sea := func(row int, s fleetSea) {
		pat := []rune(s.pattern)
		if len(pat) == 0 || s.every <= 0 {
			return
		}
		shift := fleetShipMod(frame, s.period()) / s.every
		for i := range s.width {
			if r := pat[fleetShipMod(i+shift, len(pat))]; r != ' ' {
				put(row, s.x+i, r, s.sgr)
			}
		}
	}

	shipTop, bob := 0, 0
	if a.sky {
		shipTop, bob = 2, fleetShipBob(frame)
		if x, ok := fleetShipGullX(frame, a.width); ok {
			put(0, x, 'v', fleetShipSGRGull)
		}
	}
	hullRow := shipTop + len(a.ship) - 1
	if a.water.width > 0 {
		sea(hullRow, a.water)
	}
	for j, s := range a.seas {
		sea(hullRow+1+j, s)
	}
	for i, line := range a.ship {
		rs := []rune(line)
		first, last := -1, -1
		for c, r := range rs {
			if r != ' ' {
				if first < 0 {
					first = c
				}
				last = c
			}
		}
		for c := first; c >= 0 && c <= last; c++ {
			r, sgr := rs[c], ""
			if a.flag && i == 0 && r == '>' {
				sgr = fleetShipSGRFlag
				if fleetShipMod(frame, 2*fleetShipFlagEvery) >= fleetShipFlagEvery {
					r = '~'
				}
			}
			put(shipTop-bob+i, c, r, sgr)
		}
	}

	out := make([]string, len(grid))
	for i, row := range grid {
		var b strings.Builder
		for j := 0; j < len(row); {
			k := j
			for k < len(row) && row[k].sgr == row[j].sgr {
				k++
			}
			var run strings.Builder
			for _, c := range row[j:k] {
				run.WriteRune(c.r)
			}
			if row[j].sgr == "" || strings.TrimSpace(run.String()) == "" {
				b.WriteString(run.String())
			} else {
				b.WriteString(colorize(row[j].sgr, run.String()))
			}
			j = k
		}
		out[i] = strings.TrimRight(b.String(), " ")
	}
	return out
}

// fleetShipText is one text line under the scene: plain text, how it is
// styled, and its drop rank (higher drops first when rows run short).
type fleetShipText struct {
	plain string
	style func(string) string
	drop  int
}

// fleetShipTextLines is the text under the scene, top to bottom: the title,
// FLEET's status with the counts, the last done card (none without a done
// card), and the SENT line (none without a sent card). On a short body the
// last done line drops first, then the SENT line, then the status line.
func fleetShipTextLines(v fleetStateView, sent int, now time.Time) []fleetShipText {
	title := func(s string) string { return colorize(fleetShipSGRTitle, s) }
	out := []fleetShipText{{fleetShipTitle, title, 0}}

	status := "FLEET"
	if w := strings.Join(strings.Fields(v.Fleet.Status), " "); w != "" {
		status += " " + w
	}
	parts := []string{status}
	if v.Sessions != nil {
		parts = append(parts, fleetShipCount(len(v.Sessions), "session", "sessions"))
	}
	local := now.In(time.Local)
	y, m, d := local.Date()
	midnight := time.Date(y, m, d, 0, 0, 0, 0, time.Local)
	today := 0
	var last *fleetDoneCard
	for i := range v.Done {
		dc := &v.Done[i]
		if !dc.At.Before(midnight) {
			today++
		}
		if last == nil || !dc.At.Before(last.At) {
			last = dc
		}
	}
	parts = append(parts, fmt.Sprintf("%d done today", today))
	out = append(out, fleetShipText{strings.Join(parts, " · "), dim, 1})

	if last != nil {
		label := last.Ticket
		if label == "" {
			label = truncateCells(last.Title, fleetShipTitleCut)
		}
		age := formatAge(now.Sub(last.At).Seconds()) + " ago"
		line := "last done: " + label + " · " + age
		if label == "" {
			line = "last done " + age
		}
		out = append(out, fleetShipText{line, dim, 3})
	}
	if sent > 0 {
		out = append(out, fleetShipText{fmt.Sprintf("%d sent · waiting for FLEET · Enter shows them", sent), dim, 2})
	}
	return out
}

func fleetShipCount(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// fleetShipPick is the largest scene that fits cols columns and bodyRows
// rows together with n text lines and the blank line between; nil when none
// does. bodyRows < 0 (unknown size) checks the width alone.
func fleetShipPick(cols, bodyRows, n int) *fleetShipArt {
	for i := range fleetShipArts {
		a := &fleetShipArts[i]
		if cols >= a.width && (bodyRows < 0 || bodyRows >= a.height()+1+n) {
			return a
		}
	}
	return nil
}

// fleetShipBody is the ship screen in exactly bodyRows rows (bodyRows < 0:
// everything), centered both ways. The scene always reserves its bob row, so
// the text lines sit on the same rows in every frame.
func fleetShipBody(st *fleetViewState, cols, bodyRows int, now time.Time) []string {
	if bodyRows == 0 {
		return nil
	}
	text := fleetShipTextLines(st.view, len(st.items), now)
	art := fleetShipPick(cols, bodyRows, len(text))
	if art == nil && bodyRows >= 0 {
		for len(text) > bodyRows {
			worst := 0
			for i, t := range text {
				if t.drop > text[worst].drop {
					worst = i
				}
			}
			text = append(text[:worst], text[worst+1:]...)
		}
	}
	var block []string
	if art != nil {
		pad := strings.Repeat(" ", max(cols-art.width, 0)/2)
		for _, l := range fleetShipScene(*art, st.frame) {
			if l != "" {
				l = pad + l
			}
			block = append(block, l)
		}
		block = append(block, "")
	}
	for _, t := range text {
		plain := truncateCells(t.plain, cols)
		block = append(block, strings.Repeat(" ", max(cols-cellWidth(plain), 0)/2)+t.style(plain))
	}
	if bodyRows < 0 {
		return block
	}
	out := make([]string, 0, bodyRows)
	for range max(bodyRows-len(block), 0) / 2 {
		out = append(out, "")
	}
	out = append(out, block...)
	for len(out) < bodyRows {
		out = append(out, "")
	}
	return out[:bodyRows]
}

// fleetOnlySent reports whether every item is a SENT card (true for none):
// nothing in the list needs Andy, so the ship screen may show.
func fleetOnlySent(items []fleetItem) bool {
	for _, it := range items {
		if it.Section != fleetSecSent {
			return false
		}
	}
	return true
}

// shipOn reports whether the view shows the ship screen: a live FLEET with
// no card that needs Andy, and the SENT list not opened from it. Not
// running, ambiguous, host-down and error views keep their own bodies, and
// an open panel keeps the list it was opened from, and the Clarify chat
// stays over whatever is behind it.
func (st *fleetViewState) shipOn() bool {
	if st.err != "" || !st.loaded || len(st.view.Ambiguous) > 0 || !st.view.Fleet.Alive || st.panel != nil || st.clarify != nil {
		return false
	}
	return len(st.items) == 0 || (!st.listMode && fleetOnlySent(st.items))
}

// handleShipKey is handleKey while the ship screen shows. Enter opens the
// SENT cards as the normal list (Esc there comes back); every card key does
// nothing. handled false leaves the key to handleKey's own close (Esc, q,
// Ctrl-C).
func (st *fleetViewState) handleShipKey(k string) (cmd fleetCmd, handled bool) {
	switch k {
	case KeyEsc, "q", "Q", "\x03":
		return fleetCmdNone, false
	case KeyEnter:
		if len(st.items) == 0 {
			return fleetCmdNone, true
		}
		st.listMode = true
		return fleetCmdRender, true
	}
	return fleetCmdNone, true
}
