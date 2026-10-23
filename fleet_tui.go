package main

import (
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// The Fleet view on the terminal: drawing fleetViewState (fleet_tui_state.go),
// the background jobs that talk to the local service, and the modal loop the
// session list's F key runs. Every string drawn here is a fixed string, a
// number, or a field of the already sanitized fleetStateView.

// fleetJob runs one network call off the UI goroutine, following killJob's
// wake-pipe pattern (kill_async.go). If the pipe cannot be made the job still
// runs; the loop then sees the result on its next poll tick instead.
type fleetJob[T any] struct {
	mu     sync.Mutex
	done   bool
	result T
	wakeR  int
	wakeW  int
	closed bool
}

func startFleetJob[T any](run func() T) *fleetJob[T] {
	j := &fleetJob[T]{wakeR: -1, wakeW: -1}
	var fds [2]int
	if err := unix.Pipe(fds[:]); err == nil {
		syscall.CloseOnExec(fds[0])
		syscall.CloseOnExec(fds[1])
		_ = unix.SetNonblock(fds[0], true)
		_ = unix.SetNonblock(fds[1], true)
		j.wakeR, j.wakeW = fds[0], fds[1]
	}
	go func() {
		res := run()
		j.mu.Lock()
		j.done = true
		j.result = res
		// Under the lock close() takes, so the fd is never closed and reused
		// between the check and the write.
		if !j.closed && j.wakeW >= 0 {
			_, _ = unix.Write(j.wakeW, []byte{1})
		}
		j.mu.Unlock()
	}()
	return j
}

// snapshot reports the result and whether the job has finished.
func (j *fleetJob[T]) snapshot() (T, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.result, j.done
}

// wake is the read end for pollEvents; fd -1 when closed or without a pipe.
func (j *fleetJob[T]) wake() wakeFD {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed || j.wakeR < 0 {
		return wakeFD{fd: -1, kind: wakeFleet}
	}
	return wakeFD{fd: j.wakeR, kind: wakeFleet}
}

// close releases the pipe. Idempotent, and safe while the job still runs.
func (j *fleetJob[T]) close() {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return
	}
	j.closed = true
	if j.wakeR >= 0 {
		_ = unix.Close(j.wakeR)
	}
	if j.wakeW >= 0 {
		_ = unix.Close(j.wakeW)
	}
}

// reapFleetJobs splits jobs into those still running, in order, and the
// results of those that finished; each finished job is closed here.
func reapFleetJobs[T any](jobs []*fleetJob[T]) (remaining []*fleetJob[T], done []T) {
	remaining = jobs[:0]
	for _, j := range jobs {
		res, ok := j.snapshot()
		if !ok {
			remaining = append(remaining, j)
			continue
		}
		j.close()
		done = append(done, res)
	}
	return remaining, done
}

// fleetStateFetch is one state job's result, tagged with the order it started in.
type fleetStateFetch struct {
	seq  int
	view fleetStateView
	err  error
}

type fleetDismissResult struct {
	Res fleetSayResult
	Err error
}

// fleetLoop is the modal loop's job bookkeeping, apart from the terminal so
// the order of results can be tested.
//
// A state fetch holds fleetLocal's mutex across discovery: the local call and,
// when FLEET is not live here, the parallel remote fan-out, up to 10 s. A send
// and a dismiss hold it only to get the client (a discovery when none is
// remembered).
// So they run only inside jobs, never on the loop.
type fleetLoop struct {
	st       *fleetViewState
	state    func() (fleetStateView, error)
	send     func(fleetSayRequest) fleetSendResult
	dismiss  func(cardID, source string) (fleetSayResult, error)
	interval time.Duration
	nextPoll time.Time

	// seq numbers state fetches in the order they started. A fetch below
	// dropBelow started before a send or dismiss result landed: it may show
	// the card as it was before the answer was recorded, and applying it
	// after the send result would take nextAfter early and could let a
	// second answer through. A fetch at or below applied is older than one
	// already shown.
	seq       int
	dropBelow int
	applied   int

	states    []*fleetJob[fleetStateFetch]
	sends     []*fleetJob[fleetSendResult]
	dismisses []*fleetJob[fleetDismissResult]
}

// startState starts one state fetch.
func (l *fleetLoop) startState() {
	l.seq++
	seq, fetch := l.seq, l.state
	l.states = append(l.states, startFleetJob(func() fleetStateFetch {
		v, err := fetch()
		return fleetStateFetch{seq: seq, view: v, err: err}
	}))
}

// dropOlderStates makes every state fetch started so far stale. Their jobs are
// closed now; a goroutine still running simply finishes unheard.
func (l *fleetLoop) dropOlderStates() {
	l.dropBelow = l.seq + 1
	for _, j := range l.states {
		j.close()
	}
	l.states = l.states[:0]
}

// reap applies every finished job: send and dismiss results first, so a state
// read before them is never applied after them.
func (l *fleetLoop) reap(now time.Time) {
	var sent []fleetSendResult
	l.sends, sent = reapFleetJobs(l.sends)
	for _, r := range sent {
		l.dropOlderStates()
		if l.st.applySend(r, now) {
			l.startState()
		}
	}
	var dismissed []fleetDismissResult
	l.dismisses, dismissed = reapFleetJobs(l.dismisses)
	for _, r := range dismissed {
		l.st.dismissing = false
		l.dropOlderStates()
		msg, kind := fleetDismissToast(r)
		l.st.setToast(msg, kind, now)
		l.startState()
	}
	var fetched []fleetStateFetch
	l.states, fetched = reapFleetJobs(l.states)
	for _, f := range fetched {
		if f.seq < l.dropBelow || f.seq <= l.applied {
			continue
		}
		l.applied = f.seq
		l.st.applyState(f.view, f.err, now)
	}
}

// tick starts the periodic poll once nextPoll has passed and no fetch runs.
func (l *fleetLoop) tick(now time.Time) {
	if now.Before(l.nextPoll) || len(l.states) > 0 {
		return
	}
	l.startState()
	l.nextPoll = now.Add(l.interval)
}

// run carries out the command a key returned and reports whether the view
// closes. The loop takes st.send and st.dismissID and clears them, so a
// command never runs twice.
func (l *fleetLoop) run(cmd fleetCmd, now time.Time) (closeView bool) {
	switch cmd {
	case fleetCmdClose:
		return true
	case fleetCmdSend:
		req := l.st.send
		l.st.send = nil
		if req == nil {
			l.st.sending = false
			return false
		}
		send, r := l.send, *req
		l.sends = append(l.sends, startFleetJob(func() fleetSendResult { return send(r) }))
	case fleetCmdDismiss:
		id, source := l.st.dismissID, l.st.dismissSource
		l.st.dismissID, l.st.dismissSource = "", ""
		if id == "" {
			l.st.dismissing = false
			return false
		}
		dismiss := l.dismiss
		l.dismisses = append(l.dismisses, startFleetJob(func() fleetDismissResult {
			res, err := dismiss(id, source)
			return fleetDismissResult{Res: res, Err: err}
		}))
	}
	return false
}

// wakes is base plus every live job's wake pipe.
func (l *fleetLoop) wakes(base []wakeFD) []wakeFD {
	out := append([]wakeFD(nil), base...)
	for _, j := range l.states {
		out = append(out, j.wake())
	}
	for _, j := range l.sends {
		out = append(out, j.wake())
	}
	for _, j := range l.dismisses {
		out = append(out, j.wake())
	}
	return out
}

func (l *fleetLoop) closeAll() {
	for _, j := range l.states {
		j.close()
	}
	for _, j := range l.sends {
		j.close()
	}
	for _, j := range l.dismisses {
		j.close()
	}
}

// fleetSendVia is the send job body: get the chosen host's client, then
// fleetSendJob. A failed connect sent nothing. A client for another host than
// the state the answer was prepared against (req.Source) is refused as
// Moved: FLEET moved, and the card on the new host is another card.
func fleetSendVia(src *fleetLocal) func(fleetSayRequest) fleetSendResult {
	return func(req fleetSayRequest) fleetSendResult {
		c, err := src.Client()
		if err != nil {
			return fleetSendResult{CardID: req.CardID, CheckErr: err}
		}
		if c.base != req.Source {
			return fleetSendResult{CardID: req.CardID, Moved: true}
		}
		return fleetSendJob(c, req)
	}
}

// fleetDismissVia is the dismiss job body, pinned to source like a send: a
// dismiss carries only a card id, so on another host it could clear an
// unrelated answer record.
func fleetDismissVia(src *fleetLocal) func(cardID, source string) (fleetSayResult, error) {
	return func(cardID, source string) (fleetSayResult, error) {
		c, err := src.Client()
		if err != nil {
			return fleetSayResult{}, err
		}
		if c.base != source {
			return fleetSayResult{}, errFleetMoved
		}
		return c.Dismiss(cardID)
	}
}

// fleetDismissToast maps a dismiss result to its toast.
func fleetDismissToast(r fleetDismissResult) (string, fleetToastKind) {
	switch {
	case r.Err != nil:
		if errors.Is(r.Err, errFleetUnauthorized) {
			return fleetToastTokenBad, fleetToastErr
		}
		if errors.Is(r.Err, errFleetMoved) {
			return errFleetMoved.Error(), fleetToastWarn
		}
		return fleetDisplayText(r.Err.Error()), fleetToastErr
	case r.Res.OK:
		return "dismissed", fleetToastInfo
	case r.Res.Code == "not_found":
		return "already gone", fleetToastInfo
	case r.Res.Error != "":
		return r.Res.Error, fleetToastErr
	case r.Res.Code != "":
		return r.Res.Code, fleetToastErr
	}
	return "dismiss failed", fleetToastErr
}

// fleetNoPipePoll caps the wait while a job without a wake pipe runs: only
// the loop's own passes can see it finish.
const fleetNoPipePoll = 250 * time.Millisecond

// timeout is the loop's wait. The next poll counts only while no state fetch
// is held: tick does not start a second one, so an overdue nextPoll would
// otherwise turn every select into 1 ms until the fetch lands. A held job's
// pipe wakes the loop; a job without a pipe gets fleetNoPipePoll.
func (l *fleetLoop) timeout(now time.Time) time.Duration {
	var pollAt time.Time
	if len(l.states) == 0 {
		pollAt = l.nextPoll
	}
	if l.pipeless() {
		if c := now.Add(fleetNoPipePoll); pollAt.IsZero() || c.Before(pollAt) {
			pollAt = c
		}
	}
	return fleetLoopTimeout(l.st, pollAt, now)
}

// pipeless reports whether a running job has no wake pipe.
func (l *fleetLoop) pipeless() bool {
	for _, w := range l.wakes(nil) {
		if w.fd < 0 {
			return true
		}
	}
	return false
}

// fleetLoopTimeout is how long the loop may wait for input: until pollAt
// (zero: none), the panel arming, or the toast running out, and at least
// 1 ms. With none of them it is 0, which pollEvents reads as "block until a
// key or a wake pipe".
func fleetLoopTimeout(st *fleetViewState, pollAt, now time.Time) time.Duration {
	var due time.Time
	earlier := func(t time.Time) {
		if due.IsZero() || t.Before(due) {
			due = t
		}
	}
	if !pollAt.IsZero() {
		earlier(pollAt)
	}
	if p := st.panel; p != nil {
		if arm := p.openedAt.Add(fleetArmDelay); now.Before(arm) {
			earlier(arm)
		}
	}
	if st.toast != "" && now.Before(st.toastUntil) {
		earlier(st.toastUntil)
	}
	if due.IsZero() {
		return 0
	}
	return max(due.Sub(now), time.Millisecond)
}

// runFleetView is the Fleet view's modal loop. It owns stdin until Esc (or q)
// closes it; network calls run in jobs that wake it through pipes, and the
// loop also checks them on every pass, so a job without a pipe still lands.
func runFleetView(src *fleetLocal, wakes []wakeFD) {
	fd := int(os.Stdin.Fd())
	dec := newInputDecoder()
	screen := newScreenRenderer(os.Stdout)
	l := &fleetLoop{
		st:       &fleetViewState{},
		state:    src.State,
		send:     fleetSendVia(src),
		dismiss:  fleetDismissVia(src),
		interval: fleetBadgeInterval,
	}
	defer l.closeAll()
	l.startState()
	l.nextPoll = time.Now().Add(l.interval)
	for {
		cols, rows, err := term.GetSize(fd)
		if err != nil {
			cols, rows = 0, 0
		}
		_ = screen.Draw(renderFleetView(l.st, cols, rows, time.Now()), cols, rows)
		events, _ := pollEvents(dec, l.timeout(time.Now()), l.wakes(wakes))
		now := time.Now()
		l.reap(now)
		l.tick(now)
		var keys []string
		for _, ev := range events {
			if ev.kind == eventKey {
				keys = append(keys, ev.key)
			}
		}
		for _, k := range keys {
			now := time.Now()
			if l.run(l.st.handleKey(k, len(keys) > 1, now), now) {
				return
			}
		}
	}
}

// fleetWindow is the first body line to show so the selected card's lines
// [selStart, selEnd) stay on screen. A card taller than rows scrolls inside
// itself by scroll lines.
func fleetWindow(total, selStart, selEnd, rows, scroll int) int {
	if rows <= 0 {
		return 0
	}
	if h := selEnd - selStart; h > rows {
		return selStart + min(max(scroll, 0), h-rows)
	}
	if selEnd <= rows {
		return 0
	}
	return selEnd - rows
}

const (
	fleetSGRApprove = "1;31"
	fleetSGRDecide  = "1;33"
	fleetSGRAlive   = "32"
	fleetSGRAmber   = "33"
	fleetSGRRed     = "31"
	fleetIndent     = "   "
	fleetHelpRow    = "↑↓ card · 1-9 pick option · o other · c correct · x dismiss · PgUp/PgDn scroll · Esc back"
)

// fleetAge is the age of an RFC 3339 stamp, "" when it does not parse.
func fleetAge(stamp string, now time.Time) string {
	t, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		return ""
	}
	return formatAge(now.Sub(t).Seconds())
}

// fleetRowRight puts right at the right edge of a cols-wide row after left,
// cutting left with … to make room. Both are plain text.
func fleetRowRight(left, right string, cols int) string {
	if right == "" {
		return left
	}
	lw, rw := len([]rune(left)), len([]rune(right))
	if lw+1+rw > cols {
		left = truncateRunes(left, cols-1-rw)
		lw = len([]rune(left))
	}
	if pad := cols - lw - rw; pad >= 1 {
		return left + strings.Repeat(" ", pad) + right
	}
	return left
}

// renderFleetView draws the whole Fleet view for a cols×rows terminal. It
// clamps st.scroll to what the selected card can scroll, so PgUp after
// over-scrolling moves at once.
func renderFleetView(st *fleetViewState, cols, rows int, now time.Time) string {
	width := cols
	if width <= 0 {
		width = 80
	}
	sep := dim(strings.Repeat("─", width))

	top := []string{fleetHeaderLine(st, width, now)}
	if st.err == "" && st.loaded && st.view.StateError != "" {
		top = append(top, colorize(fleetSGRRed, st.view.StateError))
	}
	top = append(top, sep)
	bodyRows := -1
	if rows > 0 {
		bodyRows = max(rows-len(top)-2, 0)
	}

	var body []string
	switch {
	case st.err != "":
		for _, l := range wrapRunes(st.err, width) {
			body = append(body, colorize(fleetSGRRed, l))
		}
	case !st.loaded:
		body = []string{dim("loading…")}
	case len(st.view.Ambiguous) > 0:
		body = []string{colorize(fleetSGRAmber, fleetAmbiguousText(st.view.Ambiguous))}
	case st.panel != nil:
		body = fleetPanelLines(st, width, bodyRows, now)
	default:
		var selStart, selEnd int
		body, selStart, selEnd = fleetListLines(st, width, now)
		if bodyRows >= 0 {
			if h := selEnd - selStart; h > bodyRows {
				st.scroll = min(max(st.scroll, 0), h-bodyRows)
			} else {
				st.scroll = 0
			}
			start := fleetWindow(len(body), selStart, selEnd, bodyRows, st.scroll)
			body = body[min(start, len(body)):]
		}
	}
	if bodyRows >= 0 {
		if len(body) > bodyRows {
			body = body[:bodyRows]
		}
		for len(body) < bodyRows {
			body = append(body, "")
		}
	}

	lines := append(top, body...)
	lines = append(lines, sep, fleetBottomRow(st, now))
	for i, l := range lines {
		lines[i] = clipLine(l, width)
	}
	return strings.Join(lines, "\n")
}

// fleetHeaderLine is FLEET's status, the state file's age and the counts.
func fleetHeaderLine(st *fleetViewState, cols int, now time.Time) string {
	if st.err != "" || !st.loaded {
		return bold("FLEET")
	}
	v := st.view
	if len(v.Ambiguous) > 0 {
		return bold("FLEET") + " " + colorize(fleetSGRAmber, "?")
	}
	name := "FLEET"
	if v.Host != "" {
		name += "@" + v.Host // a fleetHostName
	}
	left := bold(name) + " "
	if v.Fleet.Alive {
		left += colorize(fleetSGRAlive, "●")
		if v.Fleet.Status != "" {
			left += " " + v.Fleet.Status
		}
	} else {
		left += colorize(fleetSGRRed, "○") + " not running"
		if v.Fleet.Code != "" {
			left += " (" + v.Fleet.Code + ")"
		}
	}
	stamp := ""
	if age := fleetAge(v.StateFileAt, now); age != "" {
		stamp = "state " + age + " ago"
	} else if v.Stale {
		stamp = "state stale"
	}
	if stamp != "" {
		if v.Stale {
			stamp = colorize(fleetSGRAmber, stamp)
		}
		left += "  " + stamp
	}
	n := countFleetCards(v)
	right := fmt.Sprintf("%d approve · %d decide · %d sent", n.Approve, n.Decide, n.Sent)
	if pad := cols - visualLen(left) - len([]rune(right)); pad >= 2 {
		return left + strings.Repeat(" ", pad) + right
	}
	return left
}

// fleetBottomRow is the active toast, "sending…" while a send runs, nothing
// while the panel (which has its own key line) is open, else the key help.
func fleetBottomRow(st *fleetViewState, now time.Time) string {
	if st.toast != "" && now.Before(st.toastUntil) {
		switch st.toastKind {
		case fleetToastWarn:
			return colorize(fleetSGRAmber, st.toast)
		case fleetToastErr:
			return colorize(fleetSGRRed, st.toast)
		}
		return colorize(fleetSGRAlive, st.toast)
	}
	if st.sending {
		return "sending…"
	}
	if st.panel != nil {
		return ""
	}
	return dim(fleetHelpRow)
}

var fleetSectionTitles = map[fleetSection]string{
	fleetSecApprove: colorize(fleetSGRApprove, "APPROVE"),
	fleetSecDecide:  colorize(fleetSGRDecide, "DECIDE"),
	fleetSecSent:    dim("SENT — WAITING FOR FLEET"),
}

// fleetListLines is the card list with the selected card expanded, and the
// selected card's line range.
func fleetListLines(st *fleetViewState, cols int, now time.Time) (lines []string, selStart, selEnd int) {
	if len(st.items) == 0 {
		return []string{dim("No cards wait for you.")}, 0, 1
	}
	sec := fleetSection(-1)
	for i, it := range st.items {
		if it.Section != sec {
			sec = it.Section
			lines = append(lines, fleetSectionTitles[sec])
		}
		selected := i == st.sel
		if selected {
			selStart = len(lines)
		}
		lines = append(lines, fleetCollapsedRow(it, selected, cols, now))
		if selected {
			lines = append(lines, fleetExpandedLines(st, it, cols)...)
			selEnd = len(lines)
		}
	}
	return lines, selStart, selEnd
}

// fleetCollapsedRow is one card's list row.
func fleetCollapsedRow(it fleetItem, selected bool, cols int, now time.Time) string {
	prefix := "  "
	if selected {
		prefix = "▸ "
	}
	head := prefix
	if it.Card.Ticket != "" {
		head += it.Card.Ticket + "  "
	}
	var row string
	if it.Section == fleetSecSent && it.Answer != nil {
		a := it.Answer.latest()
		row = head + "You chose: " + fleetAttemptLabel(a)
		if age := fleetAge(a.At, now); age != "" {
			row += "  (" + age + " ago)"
		}
		row = truncateRunes(row, cols)
	} else {
		row = fleetRowRight(head+it.Card.Title, fleetAge(it.Card.UpdatedAt, now), cols)
	}
	if selected {
		return bold(row)
	}
	if it.Section == fleetSecSent {
		return dim(row)
	}
	return row
}

func fleetAttemptLabel(a fleetAttempt) string {
	if a.Label != "" {
		return a.Label
	}
	return a.OptionKey
}

// fleetExpandedLines is the selected card's body under its row.
func fleetExpandedLines(st *fleetViewState, it fleetItem, cols int) []string {
	var out []string
	c := it.Card
	if strings.TrimSpace(c.Summary) != "" {
		for _, l := range fleetSummaryLines(c.Summary, cols-len(fleetIndent)) {
			out = append(out, fleetIndent+l)
		}
	}
	var meta []string
	if c.WaitingOn != "" {
		meta = append(meta, "Waiting on: "+c.WaitingOn)
	}
	if c.Session != "" {
		meta = append(meta, "session "+c.Session)
	}
	if len(meta) > 0 {
		out = append(out, fleetIndent+strings.Join(meta, " · "))
	}
	if a := it.Answer; a != nil {
		first := max(len(a.Attempts)-3, 0)
		for i := first; i < len(a.Attempts); i++ {
			word := "Correction: "
			if i == 0 {
				word = "You chose: "
			}
			out = append(out, fleetIndent+word+fleetAttemptLabel(a.Attempts[i]))
		}
		if a.latest().Status == fleetAttemptUnconfirmed {
			out = append(out, fleetIndent+colorize(fleetSGRAmber, fleetToastUnclear))
			if a.CardChanged {
				out = append(out, fleetIndent+colorize(fleetSGRAmber,
					"Card changed · delivery of your last answer is still unknown — check the chat"))
			}
			out = append(out, fleetIndent+dim("c send a correction · x dismiss"))
		}
	}
	if len(c.Options) > 0 {
		out = append(out, "")
		out = append(out, fleetOptionLines(st, it, cols)...)
	}
	return out
}

// fleetOptionLines is the numbered options, greyed when they cannot be
// answered here, with the lean and the reason for greying.
func fleetOptionLines(st *fleetViewState, it fleetItem, cols int) []string {
	c := it.Card
	note := ""
	locked := false
	switch {
	case c.ID == "":
		note = "answer in the chat"
	case !st.view.Fleet.Alive:
		note = "FLEET is not running — answers are off"
		if code := st.view.Fleet.Code; code != "" {
			note = "FLEET is not running (" + code + ") — answers are off"
		}
	case c.ID == st.justSent, c.ID == st.unclear, it.Answer != nil && st.unlocked != c.ID:
		locked = true
	}
	greyAll := note != "" || locked

	nW := len(strconv.Itoa(len(c.Options)))
	keyW, labelW := 0, 0
	for _, o := range c.Options {
		keyW = max(keyW, len([]rune(o.Key)))
		labelW = max(labelW, len([]rune(o.Label)))
	}
	keyW, labelW = min(keyW, 16), min(labelW, 40)
	lineIndent := strings.Repeat(" ", len(fleetIndent)+nW+2+keyW+2)

	var out []string
	leanFound := false
	for i, o := range c.Options {
		lean := c.Lean != "" && o.Key == c.Lean
		leanFound = leanFound || lean
		grey := greyAll || i >= 9
		suffix := ""
		switch {
		case i >= 9:
			suffix = "answer on the page"
		case lean:
			suffix = "★ FLEET recommends"
		}
		n := fmt.Sprintf("%*d", nW, i+1)
		var row string
		if grey {
			row = dim(fleetIndent + n + "  " + padRight(o.Key, keyW) + "  " + fleetPadIf(o.Label, labelW, suffix != "") + fleetSuffix(suffix))
		} else {
			if lean {
				suffix = colorize(fleetSGRDecide, suffix)
			}
			row = fleetIndent + bold(n) + "  " + dim(padRight(o.Key, keyW)) + "  " + fleetPadIf(o.Label, labelW, suffix != "") + fleetSuffix(suffix)
		}
		out = append(out, row)
		if o.Line != "" {
			for _, l := range wrapRunes(o.Line, cols-len(lineIndent)) {
				out = append(out, lineIndent+dim(l))
			}
		}
	}
	// Other, last, as on the page: Andy's own answer. The answer locks do not
	// grey it, since the card does not track it.
	n, k, label := fmt.Sprintf("%*s", nW, "o"), padRight("…", keyW), "Other — write your own answer"
	if note != "" {
		out = append(out, dim(fleetIndent+n+"  "+k+"  "+label))
	} else {
		out = append(out, fleetIndent+bold(n)+"  "+dim(k)+"  "+label)
	}
	if c.Lean != "" && !leanFound {
		out = append(out, fleetIndent+"Lean: "+c.Lean)
	}
	if note != "" {
		out = append(out, fleetIndent+dim(note))
	}
	return out
}

func fleetPadIf(s string, n int, pad bool) string {
	if pad {
		return padRight(s, n)
	}
	return s
}

func fleetSuffix(s string) string {
	if s == "" {
		return ""
	}
	return "  " + s
}

// fleetPanelLines is the confirm panel. When the send text would push the
// comment and key lines off a short screen, its preview is cut with "…".
func fleetPanelLines(st *fleetViewState, cols, bodyRows int, now time.Time) []string {
	p := st.panel
	c := p.item.Card
	title := "Send this to FLEET?"
	if c.Kind == "explicit-yes" && !p.other {
		title = "Approve this write?"
	}
	// The Other panel has no option: the box is the whole answer, and it
	// sends nothing while empty.
	var what, effectLine, sendText string
	commentLabel, keysTail := "Comment: ", " · Esc cancel · comment is optional"
	if p.other {
		what = "Other · your own answer"
		sendText = fleetOtherText(c.ID, p.comment)
		commentLabel, keysTail = "Answer: ", " · Esc cancel"
	} else {
		opt := c.Options[p.opt]
		what = fmt.Sprintf("%d %s · %s", p.opt+1, opt.Key, opt.Label)
		effectLine = opt.Line
		sendText = fleetSendText(opt.Send, p.comment)
	}
	if c.Ticket != "" {
		what = c.Ticket + " · " + what
	}
	head := []string{bold(title), "  " + what}
	if effectLine != "" {
		head = append(head, "effect: "+effectLine)
	}
	head = append(head, "", "FLEET receives:")

	// Send is kept raw for the server (fleet_client.go sanitize), so the
	// preview cleans it before it reaches the terminal.
	var preview []string
	for _, l := range wrapRunes(fleetDisplayText(sendText), cols-2) {
		preview = append(preview, "  "+l)
	}

	room := cols - len(commentLabel) - 1
	comment := p.comment
	if r := []rune(comment); room >= 2 && len(r) > room {
		comment = "…" + string(r[len(r)-room+1:])
	}
	keys := "Enter send"
	if arm := p.openedAt.Add(fleetArmDelay); now.Before(arm) {
		tenths := math.Ceil(float64(arm.Sub(now)) / float64(100*time.Millisecond))
		keys += fmt.Sprintf(" (ready in %.1f s)", tenths/10)
	}
	keys += keysTail
	tail := []string{"", commentLabel + comment + "▌", "", dim(keys)}

	if bodyRows < 0 {
		return append(append(head, preview...), tail...)
	}
	// On a short screen the comment and the key line must stay. The preview
	// is cut first; with no room for two of its lines the blank spacer lines
	// go, then the preview with its label, then the effect line, and as a
	// last resort the top of the panel.
	effect := effectLine != ""
	fixed := func(blanks bool) int {
		n := 3 // title, what, comment line
		if effect {
			n++
		}
		if blanks {
			n += 3
		}
		return n + 2 // "FLEET receives:" and the key line
	}
	blanks := true
	fit := bodyRows - fixed(true)
	if len(preview) > fit && fit < 2 {
		blanks = false
		fit = bodyRows - fixed(false)
	}
	switch {
	case len(preview) <= fit:
	case fit >= 2:
		preview = append(preview[:fit-1], "  …")
	case fit == 1:
		preview = preview[:1]
	default:
		preview = nil
	}
	sp := func() []string {
		if blanks {
			return []string{""}
		}
		return nil
	}
	out := []string{head[0], head[1]}
	if effect && (preview != nil || bodyRows-len(out)-2 > 0) {
		out = append(out, head[2])
	}
	if preview != nil {
		out = append(append(append(out, sp()...), "FLEET receives:"), preview...)
	}
	out = append(append(out, sp()...), tail[1])
	out = append(append(out, sp()...), tail[3])
	if len(out) > bodyRows {
		out = out[len(out)-bodyRows:]
	}
	return out
}
