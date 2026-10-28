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

// fleetClarifyJobResult is what a clarify job hands back. Every job ends with
// a ClarifyGet, so View is the chat as it is after the ask, draft or discard.
// Code is the server's refusal of that POST; Err is a POST that got no usable
// answer (then no read was made) or a failed read. Posted: the server
// answered the POST (taken, or refused with Code), so an Err is the read's.
// Moved: FLEET is on another host than the chat was opened on, and nothing
// was sent.
type fleetClarifyJobResult struct {
	Kind   fleetCmd
	View   fleetClarifyView
	Code   string
	Err    error
	Posted bool
	Moved  bool
}

// fleetClarifyDone is a finished clarify job with the request it ran, so the
// result can be matched to the chat that is open now.
type fleetClarifyDone struct {
	req fleetClarifyRequest
	res fleetClarifyJobResult
}

// fleetClarifyPoll is how often the open chat is read while a fork runs.
const fleetClarifyPoll = 2 * time.Second

// fleetLoop is the modal loop's job bookkeeping, apart from the terminal so
// the order of results can be tested.
//
// A state fetch holds fleetLocal's mutex across discovery: the local call and,
// when FLEET is not live here, the parallel remote fan-out, up to 10 s. A send
// and a dismiss hold it only to get the client (a discovery when none is
// remembered).
// So they run only inside jobs, never on the loop.
type fleetLoop struct {
	st      *fleetViewState
	state   func() (fleetStateView, error)
	send    func(fleetSayRequest) fleetSendResult
	dismiss func(cardID, source string) (fleetSayResult, error)
	// attach hands the terminal to the attach and returns once Andy
	// detaches. nil: the a key does nothing.
	attach func(fleetAttachRequest) error
	// clarify runs one Clarify request of the given kind (fleetClarifyVia).
	clarify  func(fleetClarifyRequest, fleetCmd) fleetClarifyJobResult
	interval time.Duration
	nextPoll time.Time
	// nextClarify is when the open chat's next read is due; zero unless a
	// fork runs for it.
	nextClarify time.Time
	// nextFrame is when the ship screen's next animation frame is due; zero
	// while the ship screen is not showing, so no frame timer runs then.
	nextFrame time.Time

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
	// clarifies holds at most one job: a chat request is never sent beside
	// another, so a read cannot land after a newer ask's answer.
	clarifies []*fleetJob[fleetClarifyDone]
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
	var chats []fleetClarifyDone
	l.clarifies, chats = reapFleetJobs(l.clarifies)
	for _, d := range chats {
		l.applyClarifyJob(d, now)
	}
	// A chat opened while another card's job ran had its first read
	// skipped; it gets it once that job is gone.
	if c := l.st.clarify; c != nil && !c.loaded && c.err == "" && len(l.clarifies) == 0 {
		l.startClarify(fleetCmdClarifyGet)
	}
}

// applyClarifyJob takes one clarify job's result. A result for a chat that
// has since closed or moved to another card is dropped whole, its toast too.
func (l *fleetLoop) applyClarifyJob(d fleetClarifyDone, now time.Time) {
	st, r := l.st, d.res
	c := st.clarify
	if c == nil || c.item.Card.ID != d.req.CardID {
		return
	}
	// A question that was not taken goes back in the box, unless Andy has
	// started another.
	keep := func() {
		if r.Kind == fleetCmdClarifyAsk && c.input == "" {
			c.input = d.req.Text
		}
	}
	switch {
	case r.Moved:
		// Nothing was sent. The state names the new host, and applyState
		// would close the chat on it anyway.
		st.clarify = nil
		st.setToast(errFleetMoved.Error(), fleetToastWarn, now)
		l.startState()
		return
	case r.Err != nil && r.Kind != fleetCmdClarifyGet && !r.Posted:
		// The POST got no usable answer, so no read was made: the chat stays
		// as it was.
		keep()
		msg := "clarify failed"
		if errors.Is(r.Err, errFleetUnauthorized) {
			msg = fleetToastTokenBad
		}
		st.setToast(msg, fleetToastErr, now)
		return
	case r.Code != "":
		keep()
		st.applyClarifyRefusal(r.Code, now)
	}
	st.applyClarify(d.req.CardID, r.View, r.Err, now)
	if r.Posted && r.Err != nil {
		// The server took the request but the chat could not be read after
		// it: poll until a read lands, so the answer shows without a key.
		c.reread = true
	}
}

// startClarify starts the clarify job for a command: an ask takes
// st.clarifyReq, the others act on the open chat's card. While a job runs a
// new one is not started; an ask's text goes back in the box, so nothing
// typed is lost.
func (l *fleetLoop) startClarify(kind fleetCmd) {
	req := l.st.clarifyReq
	l.st.clarifyReq = nil
	c := l.st.clarify
	if c == nil || l.clarify == nil {
		return
	}
	if len(l.clarifies) > 0 {
		if req != nil && c.input == "" {
			c.input = req.Text
		}
		return
	}
	r := fleetClarifyRequest{Source: c.source, CardID: c.item.Card.ID}
	if kind == fleetCmdClarifyAsk {
		if req == nil {
			return
		}
		r = *req
	}
	run := l.clarify
	l.clarifies = append(l.clarifies, startFleetJob(func() fleetClarifyDone {
		return fleetClarifyDone{req: r, res: run(r, kind)}
	}))
}

// clarifyPollAt is when the open chat's next read is due: zero unless a fork
// runs for it or a read after a POST failed.
func (l *fleetLoop) clarifyPollAt(now time.Time) time.Time {
	if c := l.st.clarify; c == nil || !c.polling() {
		return time.Time{}
	}
	if l.nextClarify.IsZero() {
		return now.Add(fleetClarifyPoll)
	}
	return l.nextClarify
}

// tickClarify reads the open chat every fleetClarifyPoll while a fork runs
// for it or a read after a POST failed, and never otherwise.
func (l *fleetLoop) tickClarify(now time.Time) {
	if c := l.st.clarify; c == nil || !c.polling() {
		l.nextClarify = time.Time{}
		return
	}
	if l.nextClarify.IsZero() {
		l.nextClarify = now.Add(fleetClarifyPoll)
		return
	}
	if now.Before(l.nextClarify) || len(l.clarifies) > 0 {
		return
	}
	l.startClarify(fleetCmdClarifyGet)
	l.nextClarify = now.Add(fleetClarifyPoll)
}

// tick starts the periodic poll once nextPoll has passed and no fetch runs.
func (l *fleetLoop) tick(now time.Time) {
	l.tickClarify(now)
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
	case fleetCmdAttach:
		req := l.st.attach
		l.st.attach = nil
		if req == nil || l.attach == nil {
			return false
		}
		// Synchronous: the terminal belongs to tmux or ssh until Andy
		// detaches. The cards may have moved meanwhile, so fetch them now.
		err := l.attach(*req)
		l.st.applyAttach(err, time.Now())
		l.startState()
	case fleetCmdClarifyGet, fleetCmdClarifyAsk, fleetCmdClarifyDraft, fleetCmdClarifyDiscard:
		if (cmd == fleetCmdClarifyDraft || cmd == fleetCmdClarifyDiscard) && len(l.clarifies) > 0 {
			l.st.setToast("wait for the last request", fleetToastWarn, now)
			return false
		}
		l.startClarify(cmd)
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
	for _, j := range l.clarifies {
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
	for _, j := range l.clarifies {
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

// fleetClarifyVia is the clarify job body, pinned to the chat's source like a
// send: a client for another host gives Moved and sends nothing. After an
// accepted or refused ask, draft or discard it reads the chat, so the result
// always carries the chat as it is now.
func fleetClarifyVia(src *fleetLocal) func(fleetClarifyRequest, fleetCmd) fleetClarifyJobResult {
	return func(req fleetClarifyRequest, kind fleetCmd) fleetClarifyJobResult {
		r := fleetClarifyJobResult{Kind: kind}
		c, err := src.Client()
		if err != nil {
			r.Err = err
			return r
		}
		if c.base != req.Source {
			r.Moved = true
			return r
		}
		switch kind {
		case fleetCmdClarifyAsk:
			r.Code, r.Err = c.ClarifyAsk(req.CardID, req.Text)
		case fleetCmdClarifyDraft:
			r.Code, r.Err = c.ClarifyDraft(req.CardID, false)
		case fleetCmdClarifyDiscard:
			r.Code, r.Err = c.ClarifyDraft(req.CardID, true)
		}
		if r.Err != nil {
			return r
		}
		r.Posted = kind != fleetCmdClarifyGet
		r.View, r.Err = c.ClarifyGet(req.CardID)
		return r
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
// pipe wakes the loop; a job without a pipe gets fleetNoPipePoll. While the
// ship screen shows, the next animation frame (nextFrame) caps it too, and
// while a fork runs for the open chat, its next read (clarifyPollAt).
func (l *fleetLoop) timeout(now time.Time) time.Duration {
	var pollAt time.Time
	if len(l.states) == 0 {
		pollAt = l.nextPoll
	}
	// The chat's read counts the same way: while one is held its pipe wakes
	// the loop.
	if c := l.clarifyPollAt(now); !c.IsZero() && len(l.clarifies) == 0 && (pollAt.IsZero() || c.Before(pollAt)) {
		pollAt = c
	}
	if l.pipeless() {
		if c := now.Add(fleetNoPipePoll); pollAt.IsZero() || c.Before(pollAt) {
			pollAt = c
		}
	}
	if c := l.nextFrame; !c.IsZero() && (pollAt.IsZero() || c.Before(pollAt)) {
		pollAt = c
	}
	return fleetLoopTimeout(l.st, pollAt, now)
}

// animate keeps the ship screen's frame timer: while the ship shows it
// advances st.frame once nextFrame has passed and schedules the next one;
// otherwise it clears nextFrame, so timeout waits exactly as before. After a
// stall (an attach, a slow pass) it skips the missed frames instead of
// replaying them in a burst.
func (l *fleetLoop) animate(now time.Time) {
	if !l.st.shipOn() {
		l.nextFrame = time.Time{}
		return
	}
	if l.nextFrame.IsZero() {
		l.nextFrame = now.Add(fleetShipFrame)
		return
	}
	if now.Before(l.nextFrame) {
		return
	}
	l.st.frame++
	l.nextFrame = l.nextFrame.Add(fleetShipFrame)
	if !l.nextFrame.After(now) {
		l.nextFrame = now.Add(fleetShipFrame)
	}
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
// attach runs the terminal handoff for the a key (srv nil: this host); nil
// turns the key off.
func runFleetView(src *fleetLocal, wakes []wakeFD, attach func(srv *ServerConfig, tmux string) error) {
	fd := int(os.Stdin.Fd())
	dec := newInputDecoder()
	screen := newScreenRenderer(os.Stdout)
	l := &fleetLoop{
		st:       &fleetViewState{},
		state:    src.State,
		send:     fleetSendVia(src),
		dismiss:  fleetDismissVia(src),
		clarify:  fleetClarifyVia(src),
		interval: fleetBadgeInterval,
	}
	if attach != nil {
		l.attach = fleetAttachVia(src, attach)
	}
	defer l.closeAll()
	// The view reads the wheel and clicks and has no select mode, so it
	// captures the mouse even over the list's select mode (mouse_mode.go). An
	// attach or the raw text screen comes back with it on; leaving the view
	// gives the list its own mode back.
	defer mouseMode.capture(terminalOutput)()
	l.startState()
	l.nextPoll = time.Now().Add(l.interval)
	for {
		// Before the draw, so a frame that fell due is the one drawn, and
		// before timeout, so a ship screen that just appeared or went away
		// starts or stops its timer on this pass.
		l.animate(time.Now())
		cols, rows, err := term.GetSize(fd)
		if err != nil {
			cols, rows = 0, 0
		}
		_ = screen.Draw(renderFleetView(l.st, cols, rows, time.Now()), cols, rows)
		events, _ := pollEvents(dec, l.timeout(time.Now()), l.wakes(wakes))
		now := time.Now()
		l.reap(now)
		l.tick(now)
		inBatch := fleetKeyBatch(events)
		for _, ev := range events {
			now := time.Now()
			cmd := l.st.handleEvent(ev, inBatch, now)
			if cmd == fleetCmdRawText {
				if it := l.st.selected(); it != nil {
					runFleetRawView(fd, fleetRawLines(*it), wakes)
				}
				// The raw view drew over the screen, and keys read before it
				// belong to a view Andy has not seen since.
				screen.Invalidate()
				break
			}
			if l.run(cmd, now) {
				return
			}
			if cmd == fleetCmdAttach {
				// The attach drew over the screen; keys read before it
				// belong to a view Andy has not seen since.
				screen.Invalidate()
				break
			}
		}
	}
}

const (
	fleetSGRApprove = "1;31"
	fleetSGRDecide  = "1;33"
	fleetSGRAlive   = "32"
	fleetSGRAmber   = "33"
	fleetSGRRed     = "31"
	fleetIndent     = "   "
	fleetHelpRow    = "↑↓ card · 1-9 pick option · o other · t raw text · c correct · x dismiss · PgUp/PgDn scroll · Esc back"
	// fleetHelpRowAttach is the key help when the selected card resolves to
	// exactly one live session.
	fleetHelpRowAttach = "↑↓ card · 1-9 pick option · o other · a attach · t raw text · c correct · x dismiss · PgUp/PgDn scroll · Esc back"
	// fleetHelpClarifyHint goes after "o other" when k opens Clarify.
	fleetHelpClarifyHint = "k clarify"
	// fleetHelpLeanHint goes after "1-9 pick option" when r would send the
	// selected card's lean option.
	fleetHelpLeanHint = "r FLEET's pick"

	// The Clarify screen's help rows, without and with a draft.
	fleetHelpClarify      = "Enter ask · Ctrl-D draft for FLEET · Esc back"
	fleetHelpClarifyDraft = "Enter ask · s use draft as answer · x discard draft · Esc back"
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

// renderFleetView draws the whole Fleet view for a cols×rows terminal: the
// header, the card list, a rule, the details panel (or the confirm panel) and
// the bottom row. It moves st.listTop to keep the selected row in the list
// window and clamps st.scroll to what the panel can scroll, so PgUp after
// over-scrolling moves at once. With rows > 0 the frame is exactly rows lines.
func renderFleetView(st *fleetViewState, cols, rows int, now time.Time) string {
	width := cols
	if width <= 0 {
		width = 80
	}
	if st.clarify != nil {
		// The chat replaces the whole view; the mouse has no rows to hit.
		st.hit = fleetHitMap{}
		lines := fleetClarifyLines(st, width, rows, now)
		for i, l := range lines {
			lines[i] = clipLine(l, width)
		}
		return strings.Join(lines, "\n")
	}
	sep := dim(strings.Repeat("─", width))

	st.hit = fleetHitMap{}
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
	case st.shipOn():
		body = fleetShipBody(st, width, bodyRows, now)
	default:
		body = fleetStackedBody(st, width, bodyRows, now)
	}
	if bodyRows >= 0 {
		if len(body) > bodyRows {
			body = body[:bodyRows]
		}
		for len(body) < bodyRows {
			body = append(body, "")
		}
	}
	if st.hit.listItems != nil {
		// fleetStackedBody kept body-relative rows; the body starts under
		// top, and the panel is every body row after the list and its rule.
		st.hit.on = true
		st.hit.panelH = max(len(body)-st.hit.panelY, 0)
		st.hit.listY += len(top)
		st.hit.panelY += len(top)
	}

	lines := append(top, body...)
	lines = append(lines, sep, fleetBottomRow(st, now))
	if rows > 0 && len(lines) > rows {
		// A screen shorter than the header and footer: keep the bottom row,
		// which carries the toast.
		lines = append(lines[:rows-1], lines[len(lines)-1])
	}
	for i, l := range lines {
		lines[i] = clipLine(l, width)
	}
	return strings.Join(lines, "\n")
}

// fleetStackedLayout splits body rows between the card list, the rule under
// it and the panel. need is the list's line count (cards plus headings). The
// list gets need rows, at most 40% of the body rounded down but never fewer
// than 3, so its height (and the panel's first row) never depends on which
// card is selected. On a tiny screen the list gives way so the panel keeps
// minPanel rows: 1 for the details, 2 for the confirm panel (its comment and
// key lines); the details also keep 1 list row, the selected one. confirm
// changes the split only on such a screen. body < 0 (unknown size) shows all.
func fleetStackedLayout(body, need int, confirm bool) (listH int, rule bool, panelH int) {
	if body < 0 {
		return need, true, -1
	}
	minList, minPanel := 1, 1
	if confirm {
		minList, minPanel = 0, 2
	}
	listH = min(need, max(3, body*2/5))
	if listH+1+minPanel > body {
		listH = min(need, max(body-1-minPanel, minList))
	}
	listH = min(listH, body)
	rem := body - listH
	rule = listH > 0 && rem >= 2
	panelH = rem
	if rule {
		panelH--
	}
	return listH, rule, panelH
}

// fleetListWindow is the first list line to show in an h-row window over
// need lines so line sel stays visible. The window moves only when sel leaves
// it; moving up to a section's first card (headAbove) shows its heading too.
func fleetListWindow(top, sel, need, h int, headAbove bool) int {
	if h <= 0 || need <= h {
		return 0
	}
	switch {
	case sel < top:
		top = sel
		if headAbove && h >= 2 {
			top = sel - 1
		}
	case sel >= top+h:
		top = sel - h + 1
	}
	return min(max(top, 0), need-h)
}

// fleetPanelWindow cuts the panel's lines to h rows from scroll. When they do
// not fit, a dim "↑ more" takes the first row while scrolled and a dim
// "↓ more" the last while more lies below (a panel under 3 rows has no room
// for them). It returns the rows and the clamped scroll. h < 0 shows all.
func fleetPanelWindow(lines []string, h, scroll int) ([]string, int) {
	if h < 0 || len(lines) <= h {
		return lines, 0
	}
	if h < 3 {
		s := min(max(scroll, 0), len(lines)-h)
		return lines[s : s+h], s
	}
	s := min(max(scroll, 0), len(lines)-(h-1))
	var out []string
	avail := h
	if s > 0 {
		out = append(out, dim("↑ more"))
		avail--
	}
	end := s + avail
	below := end < len(lines)
	if below {
		end--
	}
	out = append(out, lines[s:end]...)
	if below {
		out = append(out, dim("↓ more"))
	}
	return out, s
}

// fleetStackedBody is the list window, the rule and the panel, in exactly
// bodyRows rows (bodyRows < 0: everything).
func fleetStackedBody(st *fleetViewState, cols, bodyRows int, now time.Time) []string {
	list, selRow, lineItems := fleetListLines(st, cols, now)
	listH, rule, panelH := fleetStackedLayout(bodyRows, len(list), st.panel != nil)
	headAbove := len(st.items) > 0 && (st.sel == 0 || st.items[st.sel-1].Section != st.items[st.sel].Section)
	st.listTop = fleetListWindow(st.listTop, selRow, len(list), listH, headAbove)
	st.panelRows = max(panelH, 0)

	out := append([]string(nil), list[st.listTop:st.listTop+listH]...)
	st.hit.listY, st.hit.listItems = 0, lineItems[st.listTop:st.listTop+listH]
	if rule {
		out = append(out, fleetListRule(cols, st.listTop, len(list)-st.listTop-listH))
	}
	// Body-relative; renderFleetView adds the rows above the body and sets
	// the panel's height from the body it drew (the rest of it).
	st.hit.panelY = len(out)
	st.hit.scrolls = st.panel == nil
	if panelH == 0 {
		return out
	}
	if st.panel != nil {
		return append(out, fleetPanelLines(st, cols, panelH, now)...)
	}
	var panel []string
	if it := st.selected(); it != nil {
		panel, st.scroll = fleetPanelWindow(fleetDetailLines(st, *it, cols, now), panelH, st.scroll)
	}
	return append(out, panel...)
}

// fleetListRule is the rule under the list. When list lines are scrolled off
// it says how many, above and below, at its right end.
func fleetListRule(cols, above, below int) string {
	var marks []string
	if above > 0 {
		marks = append(marks, "↑"+strconv.Itoa(above))
	}
	if below > 0 {
		marks = append(marks, "↓"+strconv.Itoa(below))
	}
	label := ""
	if len(marks) > 0 {
		label = " " + strings.Join(marks, " ") + " "
	}
	if n := len([]rune(label)); label == "" || n+4 > cols {
		return dim(strings.Repeat("─", cols))
	}
	return dim(strings.Repeat("─", cols-len([]rune(label))-2) + label + "──")
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
	if t := fleetToastLine(st, now); t != "" {
		return t
	}
	if st.sending {
		return "sending…"
	}
	if st.panel != nil {
		return ""
	}
	if st.shipOn() {
		// The sent line is the first to go on a small screen; the help
		// row still says Enter opens the sent cards.
		if len(st.items) > 0 {
			return dim(fleetHelpShipSent)
		}
		return dim(fleetHelpShip)
	}
	row := fleetHelpRow
	if st.attachable() {
		row = fleetHelpRowAttach
	}
	if st.leanPickable() {
		row = strings.Replace(row, " · 1-9 pick option · ", " · 1-9 pick option · "+fleetHelpLeanHint+" · ", 1)
	}
	if st.canClarify() {
		row = strings.Replace(row, " · o other · ", " · o other · "+fleetHelpClarifyHint+" · ", 1)
	}
	return dim(row)
}

// fleetToastLine is the active toast in its kind's colour, "" when none.
func fleetToastLine(st *fleetViewState, now time.Time) string {
	if st.toast == "" || !now.Before(st.toastUntil) {
		return ""
	}
	switch st.toastKind {
	case fleetToastWarn:
		return colorize(fleetSGRAmber, st.toast)
	case fleetToastErr:
		return colorize(fleetSGRRed, st.toast)
	}
	return colorize(fleetSGRAlive, st.toast)
}

var fleetSectionTitles = map[fleetSection]string{
	fleetSecApprove: colorize(fleetSGRApprove, "APPROVE"),
	fleetSecDecide:  colorize(fleetSGRDecide, "DECIDE"),
	fleetSecSent:    dim("SENT — WAITING FOR FLEET"),
}

// fleetListLines is one line per card under its section headings, the
// selected card's line index, and the item index each line shows (-1 for a
// heading or the empty-list line).
func fleetListLines(st *fleetViewState, cols int, now time.Time) (lines []string, selRow int, lineItems []int) {
	if len(st.items) == 0 {
		return []string{dim("No cards wait for you.")}, 0, []int{-1}
	}
	sec := fleetSection(-1)
	for i, it := range st.items {
		if it.Section != sec {
			sec = it.Section
			lines = append(lines, fleetSectionTitles[sec])
			lineItems = append(lineItems, -1)
		}
		if i == st.sel {
			selRow = len(lines)
		}
		chat := it.Card.ID != "" && st.view.Clarify[it.Card.ID].Turns > 0
		lines = append(lines, fleetCollapsedRow(it, i == st.sel, chat, cols, now))
		lineItems = append(lineItems, i)
	}
	return lines, selRow, lineItems
}

// fleetClarifyMark follows an open card's title on its row when the card has
// a Clarify chat.
const fleetClarifyMark = " ✎"

// fleetCollapsedRow is one card's list row. chat marks an open card that has
// a Clarify chat.
func fleetCollapsedRow(it fleetItem, selected, chat bool, cols int, now time.Time) string {
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
		// An open card shows its priority (P1..P4) at the right, before its age.
		right := "P" + strconv.Itoa(it.Card.priority())
		if age := fleetCardAge(it.Card, now); age != "" {
			right += " · " + age
		}
		if chat {
			return fleetMarkedRow(head+it.Card.Title, right, cols, selected)
		}
		row = fleetRowRight(head+it.Card.Title, right, cols)
	}
	if selected {
		return bold(row)
	}
	if it.Section == fleetSecSent {
		return dim(row)
	}
	return row
}

// fleetMarkedRow is fleetRowRight with a dim fleetClarifyMark right after
// left; the title is cut first, so the mark stays on a narrow row.
func fleetMarkedRow(left, right string, cols int, selected bool) string {
	style := func(s string) string { return s }
	if selected {
		style = bold
	}
	mw, rw := len([]rune(fleetClarifyMark)), len([]rune(right))
	if len([]rune(left))+mw+1+rw > cols {
		left = truncateRunes(left, cols-mw-1-rw)
	}
	pad := cols - len([]rune(left)) - mw - rw
	if pad < 1 {
		return style(truncateRunes(left, cols-mw)) + dim(fleetClarifyMark)
	}
	return style(left) + dim(fleetClarifyMark) + style(strings.Repeat(" ", pad)+right)
}

func fleetAttemptLabel(a fleetAttempt) string {
	if a.Label != "" {
		return a.Label
	}
	return a.OptionKey
}

// fleetCardAge is how long the card has been up: firstSeenAt, else updatedAt.
func fleetCardAge(c fleetCardView, now time.Time) string {
	if age := fleetAge(c.FirstSeenAt, now); age != "" {
		return age
	}
	return fleetAge(c.UpdatedAt, now)
}

// fleetDetailLines is the selected card in full for the details panel: its
// title, a line with section, ticket, priority and age, then its body.
func fleetDetailLines(st *fleetViewState, it fleetItem, cols int, now time.Time) []string {
	c := it.Card
	var out []string
	if strings.TrimSpace(c.Title) != "" {
		for _, l := range wrapRunes(c.Title, cols) {
			out = append(out, bold(l))
		}
	}
	meta := []string{fleetSectionTitles[it.Section]}
	if c.Ticket != "" {
		meta = append(meta, c.Ticket)
	}
	if it.Section != fleetSecSent {
		meta = append(meta, "P"+strconv.Itoa(c.priority()))
	}
	if age := fleetCardAge(c, now); age != "" {
		meta = append(meta, age+" old")
	}
	out = append(out, strings.Join(meta, dim(" · ")))
	return append(out, fleetExpandedLines(st, it, cols)...)
}

// fleetExpandedLines is the selected card's body: summary, meta, answers and
// options.
func fleetExpandedLines(st *fleetViewState, it fleetItem, cols int) []string {
	var out []string
	c := it.Card
	if strings.TrimSpace(c.Summary) != "" {
		for _, l := range fleetSummaryStyledLines(c.Summary, cols-len(fleetIndent)) {
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

const (
	fleetSGRYou        = "36"
	fleetSGRFork       = "35"
	fleetClarifyLabelW = 6 // the turn labels' column
)

// fleetClarifyLines is the Clarify screen (layout B) in exactly rows lines
// (rows <= 0: all of it): the card's head and options pinned on top, the chat
// bottom-anchored under it, a draft box, the input line and the help row. On
// a short screen the options line goes first, then the rules, then the head;
// the input line and the help row stay. It clamps c.scroll to what the chat
// can scroll and keeps st.panelRows at the chat's height for PgUp/PgDn.
// Every string drawn is fixed or from the already sanitized view.
func fleetClarifyLines(st *fleetViewState, cols, rows int, now time.Time) []string {
	c := st.clarify
	card := c.item.Card
	sep := dim(strings.Repeat("─", cols))
	top := []string{fleetClarifyHead(card, cols), dim(truncateCells("  "+fleetClarifyOptions(card), cols)), sep}
	help := fleetToastLine(st, now)
	switch {
	case help != "":
	case c.view.Draft != nil:
		help = dim(fleetHelpClarifyDraft)
	default:
		help = dim(fleetHelpClarify)
	}
	bottom := []string{sep, fleetClarifyInput(c.input, cols), help}

	h := -1
	if rows > 0 {
		shrink := []func(){
			func() { top = []string{top[0], top[2]} },
			func() { top = top[:1] },
			func() { bottom = bottom[1:] },
			func() { top = nil },
			func() { bottom = bottom[len(bottom)-1:] },
		}
		for i := 0; len(top)+len(bottom) > rows && i < len(shrink); i++ {
			shrink[i]()
		}
		h = max(rows-len(top)-len(bottom), 0)
	}

	box := fleetClarifyDraftBox(c.view.Draft, cols)
	turns := fleetClarifyTurnLines(c, cols, now)
	var body []string
	if h < 0 {
		body = append(turns, box...)
		c.scroll = 0
	} else {
		// The draft keeps its box, but leaves the chat 3 rows where it can.
		boxH := min(len(box), max(h-3, min(h, 3)))
		box = fleetClarifyCutBox(box, boxH)
		chatH := h - len(box)
		st.panelRows = chatH
		var win []string
		win, c.scroll = fleetClarifyWindow(turns, chatH, c.scroll)
		body = append(win, box...)
	}
	out := append(append(append([]string(nil), top...), body...), bottom...)
	if rows > 0 && len(out) > rows {
		out = out[len(out)-rows:]
	}
	return out
}

// fleetClarifyHead is "Clarify  <ticket>  <title>" with a dim
// "fork · read-only" at the right edge, the title cut to make room.
func fleetClarifyHead(card fleetCardView, cols int) string {
	const right = "fork · read-only"
	left := "Clarify  "
	if card.Ticket != "" {
		left += card.Ticket + "  "
	}
	left += strings.Join(strings.Fields(card.Title), " ")
	rw := cellWidth(right)
	if cellWidth(left)+2+rw > cols {
		left = truncateCells(left, cols-2-rw)
	}
	styled := left
	if rest, ok := strings.CutPrefix(left, "Clarify"); ok {
		styled = bold("Clarify") + rest
	}
	if pad := cols - cellWidth(left) - rw; left != "" && pad >= 2 {
		return styled + strings.Repeat(" ", pad) + dim(right)
	}
	return bold(truncateCells("Clarify", cols))
}

// fleetClarifyOptions is the card's options as "<key> <label>" joined with
// " · ", FLEET's lean marked.
func fleetClarifyOptions(card fleetCardView) string {
	var parts []string
	for _, o := range card.Options {
		p := o.Key + " " + o.Label
		if card.Lean != "" && o.Key == card.Lean {
			p += " (FLEET's pick)"
		}
		parts = append(parts, p)
	}
	return strings.Join(strings.Fields(strings.Join(parts, " · ")), " ")
}

// fleetClarifyInput is the input line; a text too long for it shows its end.
func fleetClarifyInput(input string, cols int) string {
	room := cols - 3 // "› " and the cursor
	if r := []rune(input); room >= 2 && len(r) > room {
		input = "…" + string(r[len(r)-room+1:])
	}
	return "› " + input + "▌"
}

// fleetClarifyTurnLines is the chat, oldest first: each turn wrapped to
// cols-6 under its 6-column label (you cyan, fleet magenta, an error turn
// yellow under "!", a note dim with no label), a blank line between turns,
// then "fleet thinking… Ns" while a fork runs, and a read error last.
func fleetClarifyTurnLines(c *fleetClarifyMode, cols int, now time.Time) []string {
	if !c.loaded && c.err == "" {
		return []string{dim("loading…")}
	}
	w := cols - fleetClarifyLabelW
	pad := strings.Repeat(" ", fleetClarifyLabelW)
	label := func(sgr, word string) string {
		return colorize(sgr, word) + strings.Repeat(" ", fleetClarifyLabelW-len(word))
	}
	var out []string
	if c.loaded && len(c.view.Turns) == 0 && c.view.Pending == nil {
		for _, l := range wrapRunes("Ask about this card. A read-only fork of FLEET answers; FLEET does not see this chat.", cols) {
			out = append(out, dim(l))
		}
	}
	for i, t := range c.view.Turns {
		if i > 0 {
			out = append(out, "")
		}
		head := pad
		var body []string
		switch t.Role {
		case clarifyRoleYou:
			head = label(fleetSGRYou, "you")
			body = fleetSummaryStyledLines(t.Text, w)
		case clarifyRoleFleet:
			head = label(fleetSGRFork, "fleet")
			body = fleetSummaryStyledLines(t.Text, w)
		case clarifyRoleError:
			head = label(fleetSGRAmber, "!")
			for _, l := range wrapRunes(t.Text, w) {
				body = append(body, colorize(fleetSGRAmber, l))
			}
		case clarifyRoleNote:
			for _, l := range wrapRunes(t.Text, w) {
				body = append(body, dim(l))
			}
		default:
			body = fleetSummaryStyledLines(t.Text, w)
		}
		for j, l := range body {
			if j > 0 {
				head = pad
			}
			out = append(out, head+l)
		}
	}
	if p := c.view.Pending; p != nil {
		if len(c.view.Turns) > 0 {
			out = append(out, "")
		}
		line := label(fleetSGRFork, "fleet") + dim("thinking…")
		if t, err := time.Parse(time.RFC3339, p.StartedAt); err == nil {
			line += dim(fmt.Sprintf(" %ds", max(int(now.Sub(t)/time.Second), 0)))
		}
		out = append(out, line)
	}
	if c.err != "" {
		out = append(out, colorize(fleetSGRRed, c.err))
	}
	return out
}

// fleetClarifyWindow cuts the chat to h rows, anchored at the bottom: scroll
// counts lines up from the newest. A chat that fits sits at the bottom with
// blank rows above it. Otherwise it is fleetPanelWindow's window, with its
// "↑ more" / "↓ more" rows. It returns the rows and the clamped scroll.
func fleetClarifyWindow(lines []string, h, scroll int) ([]string, int) {
	if h <= 0 {
		return nil, max(scroll, 0)
	}
	if len(lines) <= h {
		return append(make([]string, h-len(lines)), lines...), 0
	}
	// fleetPanelWindow's largest top: the last line just shows.
	last := len(lines) - h
	if h >= 3 {
		last = len(lines) - (h - 1)
	}
	win, top := fleetPanelWindow(lines, h, last-max(scroll, 0))
	return win, last - top
}

// fleetClarifyDraftBox is the draft as a yellow box titled "draft for FLEET",
// cols wide, its text wrapped inside. Draft whitespace collapses as it does
// when s puts the draft in the Other box. nil without a draft.
func fleetClarifyDraftBox(d *clarifyDraft, cols int) []string {
	if d == nil {
		return nil
	}
	const title = " draft for FLEET "
	inner := cols - 4
	if inner < 1 || cols < cellWidth(title)+3 {
		return []string{colorize(fleetSGRAmber, truncateCells("draft for FLEET", cols))}
	}
	out := []string{colorize(fleetSGRAmber, "┌─"+title+strings.Repeat("─", cols-3-cellWidth(title))+"┐")}
	for _, l := range wrapRunes(d.Text, inner) {
		if cellWidth(l) > inner {
			l = truncateCells(l, inner) // wide runes: wrapRunes counts runes
		}
		out = append(out, colorize(fleetSGRAmber, "│")+" "+l+strings.Repeat(" ", inner-cellWidth(l))+" "+colorize(fleetSGRAmber, "│"))
	}
	return append(out, colorize(fleetSGRAmber, "└"+strings.Repeat("─", cols-2)+"┘"))
}

// fleetClarifyCutBox cuts the draft box to h rows: its text ends in "…" and
// keeps both borders while 3 rows fit, else the title row (and the bottom
// border) is all that shows.
func fleetClarifyCutBox(box []string, h int) []string {
	switch {
	case len(box) <= h:
		return box
	case h <= 0:
		return nil
	case h == 1 || len(box) < 3:
		return box[:1]
	case h == 2:
		return []string{box[0], box[len(box)-1]}
	}
	text := append([]string(nil), box[1:h-1]...)
	// The last text row that shows becomes "…" inside the borders.
	cols := visualLen(box[0])
	text[len(text)-1] = colorize(fleetSGRAmber, "│") + " …" + strings.Repeat(" ", max(cols-5, 0)) + " " + colorize(fleetSGRAmber, "│")
	return append(append([]string{box[0]}, text...), box[len(box)-1])
}
