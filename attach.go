package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/creack/pty"
)

// GET /sessions/{pid}/attach — a real terminal on the phone.
//
// Every session already lives in tmux, so a faithful remote terminal is cheap
// in concept: allocate a PTY, run `tmux attach-session` in it, and pump bytes
// between that PTY and a WebSocket. The phone renders them.
//
// The wire, in full:
//
//	binary frame, either direction  raw PTY bytes, nothing wrapped around them
//	text frame, client → server     JSON control, today only
//	                                {"resize":{"cols":C,"rows":R}}
//	text frame, server → client     unused, reserved
//	close                           normal = the attach client exited (tmux
//	                                detached, or the session died); going-away =
//	                                idle timeout; internal-error = we failed
//
// What this does not add: authority. The bearer token already grants POST
// /sessions/new with an arbitrary command, so it is already shell-equivalent —
// attach adds convenience, not reach. It still sits behind the same `authed`
// gate as everything else, and the audit-log debt recorded in the design spec
// gets one line deeper.
const (
	// attachMaxPerSession caps concurrent attaches to one session. Two, so a
	// phone that reconnects before the old socket has finished dying is never
	// locked out of its own session, and no more, because each attach holds a
	// PTY and a tmux client for as long as it lives.
	attachMaxPerSession = 2

	// attachMaxTotal caps concurrent attaches across every session on the host.
	// The per-session cap alone bounds nothing: one authenticated client can
	// attach to as many different sessions as this machine happens to be
	// running, and each of those costs a PTY, a tmux client and a pair of
	// goroutines. Thirty-two is far above any real use of a phone terminal and
	// far below anything that would hurt the host.
	attachMaxTotal = 32

	// attachIdleTimeout closes a connection nothing has been typed into for this
	// long. See attachActivity: it counts client→server frames only.
	attachIdleTimeout = 30 * time.Minute

	// attachWriteTimeout bounds one PTY→WebSocket frame write. A phone that
	// drives into a tunnel leaves the write blocked until TCP gives up, which is
	// minutes at best, and until then it holds a goroutine, a PTY and a live
	// tmux client. Reading from the PTY stays blocking-with-backpressure — that
	// is what keeps memory bounded — it is only the never-returning write that
	// needs a deadline.
	attachWriteTimeout = 30 * time.Second

	// attachReadLimit bounds one client→server frame. Input is keystrokes and
	// the occasional paste; nothing legitimate approaches this.
	attachReadLimit = 32 * 1024

	// attachExitGrace is how long the attach client gets to notice its PTY
	// closed and exit on its own before it is killed.
	attachExitGrace = 2 * time.Second

	// attachKillGrace bounds the wait after that kill. Killing a local child is
	// not a network call, so a couple of seconds is already generous, and the
	// wait has to be bounded by something: Kill can fail, and there is then
	// nothing left to wait for. See reapAttachProcess.
	attachKillGrace = 2 * time.Second
)

// Terminal geometry: the size a connection starts at when it says nothing, and
// the bounds any size must fall inside. A zero column count would make the
// tty unusable rather than small, and nothing real is a thousand columns wide.
const (
	attachDefaultCols = 80
	attachDefaultRows = 24
	attachMaxCols     = 1000
	attachMaxRows     = 1000
)

// codeNoPane: the session is not running inside tmux, so there is no pane to
// attach to and nothing to stream. A wire contract shared with every other
// endpoint that needs a pane — the string is what an iOS client matches on, so
// it is spelled here once and reused, never re-invented per endpoint.
const codeNoPane = "no_pane"

// codeAttachBusy: no attach slot is free — this session already has
// attachMaxPerSession terminals on it, or the host has attachMaxTotal. Distinct
// from the two staleness codes: the client's row is fine, and the right response
// is to close another terminal rather than to refresh.
const codeAttachBusy = "attach_busy"

// attachSize is one terminal geometry, in character cells.
type attachSize struct {
	Cols uint16
	Rows uint16
}

// attachClient is one running attach: a PTY master and the process on the other
// end of it. An interface so tests can drive the pump — resize, readonly,
// disconnect, idle — without a real tmux, the same seam pattern spawn and
// terminate already use for the other verbs.
//
// Read/Write move raw terminal bytes. Close detaches: it must leave the tmux
// session itself untouched, which is the entire point of attaching to tmux
// rather than to the process.
type attachClient interface {
	io.ReadWriteCloser
	Resize(size attachSize) error
}

// tmuxAttachClient is the production attachClient: `tmux attach-session`
// running on the far side of a PTY.
type tmuxAttachClient struct {
	pty  *os.File
	cmd  *exec.Cmd
	once sync.Once
}

func (c *tmuxAttachClient) Read(p []byte) (int, error)  { return c.pty.Read(p) }
func (c *tmuxAttachClient) Write(p []byte) (int, error) { return c.pty.Write(p) }

func (c *tmuxAttachClient) Resize(size attachSize) error {
	return pty.Setsize(c.pty, &pty.Winsize{Cols: size.Cols, Rows: size.Rows})
}

// Close detaches this one client and reaps it. Closing the PTY master hangs up
// the terminal, which a tmux client answers by detaching — the tmux server, the
// session and everything running in it are untouched, which is why a phone
// putting itself to sleep costs the user nothing. The kill is only for a client
// that ignores the hangup; without the Wait it would linger as a zombie.
func (c *tmuxAttachClient) Close() error {
	var err error
	c.once.Do(func() {
		err = c.pty.Close()
		reapAttachProcess(c.cmd.Wait,
			func() error { return c.cmd.Process.Kill() },
			attachExitGrace, attachKillGrace)
	})
	return err
}

// reapAttachProcess waits for the attach client to go, killing it if it will
// not, and gives up rather than waiting on a kill that failed.
//
// Every wait here is bounded, and that is the point. Close is what releases the
// caller's attach slot — a host-wide one, not just one of a session's two — so
// a wait that never returns does not merely leak a goroutine: it holds a slot
// for the life of the server, and enough of them close the endpoint to everyone.
// Kill can fail (a PID already reaped by something else, a process the kernel
// will not signal), and there is then nothing left to wait for, so waiting is
// the wrong thing to do.
//
// Giving up abandons the wait goroutine, deliberately: one leaked goroutine on
// a process that refuses to die is a far smaller thing than a handler that
// never returns.
func reapAttachProcess(wait, kill func() error, grace, killGrace time.Duration) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = wait()
	}()
	select {
	case <-done:
		return
	case <-time.After(grace):
	}
	if err := kill(); err != nil {
		fmt.Fprintf(os.Stderr, "claude-sessions: cannot kill the attach client (%v) — giving up on it\n", err)
	}
	select {
	case <-done:
	case <-time.After(killGrace):
		fmt.Fprintln(os.Stderr, "claude-sessions: the attach client did not exit after being killed — giving up on it")
	}
}

// startTmuxAttach runs `tmux attach-session -t <session>` inside a fresh PTY.
//
// -r is tmux's own read-only flag. It is a courtesy to the tmux client, not the
// enforcement point: the server drops input frames on a readonly connection
// itself (see pumpAttach), because a client that is compromised or simply buggy
// must not be able to promote itself by lying about the flag it asked for.
//
// Deliberately no -d: another client — the desktop, in a real session — stays
// attached rather than being kicked off by a phone.
func startTmuxAttach(target string, readonly bool, size attachSize) (attachClient, error) {
	// tmuxSessionName (migrate.go) reduces a "session:window.pane" location to
	// the session name attach-session wants, and errors rather than guessing on
	// malformed metadata. Attaching to the session rather than to the pane is
	// deliberate: it lands the phone wherever the session currently is, exactly
	// as a desktop attach does, instead of silently switching the user's active
	// window out from under them.
	name, err := tmuxSessionName(target)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command("tmux", tmuxAttachArgs(name, readonly)...)
	cmd.Env = attachEnv(os.Environ())
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: size.Cols, Rows: size.Rows})
	if err != nil {
		return nil, err
	}
	return &tmuxAttachClient{pty: f, cmd: cmd}, nil
}

// tmuxAttachArgs builds the argv for one attach.
//
// The target is written "=name", which is tmux's own syntax for "exact match
// only". Without it, tmux resolves a target session in three widening steps —
// exact, then prefix, then as an fnmatch pattern — and only the first of those
// is the thing we asked for.
//
// That matters because a session row's tmux location is not re-verified every
// time it is read: CollectLocal overwrites Tmux only when a live pane is found
// for the PID (session.go), so a value that came straight off the on-disk
// session JSON can name a tmux session that has since been renamed or
// recreated. Handed over loosely, such a name does not fail — it prefix-matches
// whatever live session happens to start with it, and the phone gets a full
// interactive terminal on somebody else's work. Exact or nothing: an attach
// that cannot resolve its own target must end, not land somewhere else.
//
// -r is tmux's own read-only flag; see the comment on startTmuxAttach for why
// it is a courtesy rather than the enforcement point.
func tmuxAttachArgs(name string, readonly bool) []string {
	args := []string{"-u", "attach-session", "-t", "=" + name}
	if readonly {
		args = append(args, "-r")
	}
	return args
}

// attachEnv builds the environment for the attach client. Three fixes, each for
// a way the inherited environment is wrong for this child:
//
//   - TERM is forced. Whatever terminal the server itself was started from is
//     irrelevant — this PTY is read by the phone's emulator, not by the
//     server's terminal — and launchd starts the server with no TERM at all,
//     where tmux refuses to attach ("open terminal failed").
//   - LANG is forced unless what we inherited is already UTF-8, because launchd
//     starts the server with no locale. A tmux client in the C locale sanitizes
//     everything outside printable ASCII, which is the same trap commit b8c835d
//     documented for list-panes output. LC_ALL/LC_CTYPE are dropped when they
//     are not UTF-8 so they cannot quietly undo LANG.
//   - TMUX/TMUX_PANE are dropped. If they survive — a server, or a `go test`,
//     started from inside tmux — the client refuses with "sessions should be
//     nested with care" and attach never works at all.
func attachEnv(env []string) []string {
	const utf8Lang = "en_US.UTF-8"
	lang := utf8Lang
	out := make([]string, 0, len(env)+2)
	for _, kv := range env {
		key, val, _ := strings.Cut(kv, "=")
		switch key {
		case "TMUX", "TMUX_PANE", "TERM", "LANG":
			if key == "LANG" && isUTF8Locale(val) {
				lang = val
			}
			continue
		case "LC_ALL", "LC_CTYPE":
			if !isUTF8Locale(val) {
				continue
			}
		}
		out = append(out, kv)
	}
	return append(out, "TERM=xterm-256color", "LANG="+lang)
}

func isUTF8Locale(v string) bool {
	return strings.Contains(strings.ToUpper(v), "UTF-8") || strings.Contains(strings.ToUpper(v), "UTF8")
}

// attachLimiter caps concurrent attaches, per session id and across the host.
// Keyed by session id rather than PID: the id is the identity every other
// endpoint already guards on, and it survives a PID being recycled. The host
// total is counted alongside, because a cap that only ever counts one session
// at a time puts no ceiling on how many sessions a client may hold at once.
type attachLimiter struct {
	mu    sync.Mutex
	n     map[string]int
	total int
}

// acquire takes a slot for key, or reports that either ceiling is full. The
// returned release is idempotent, so a handler can defer it and still release
// early without double-counting — and a refused acquire counts against neither.
func (l *attachLimiter) acquire(key string, perSession, total int) (release func(), ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.n == nil {
		l.n = make(map[string]int)
	}
	if l.n[key] >= perSession || l.total >= total {
		return nil, false
	}
	l.n[key]++
	l.total++
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.total--
			if l.n[key] <= 1 {
				delete(l.n, key)
				return
			}
			l.n[key]--
		})
	}, true
}

// attachActivity is the last moment a connection's client said anything.
//
// A time.Time, compared with Sub, and not a nanosecond count. Go keeps a
// monotonic reading inside a time.Time and Sub uses it in preference to the wall
// clock, so an idle window measured this way is elapsed time and nothing else.
// Converting to UnixNano throws that reading away and leaves the comparison on
// the wall clock, where an NTP correction or a hand-set clock stepping backwards
// silently extends how long a connection may sit idle.
type attachActivity struct {
	mu sync.Mutex
	at time.Time
}

func (a *attachActivity) mark(t time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.at = t
}

// idleFor reports how long the connection has been quiet as of now.
func (a *attachActivity) idleFor(now time.Time) time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	return now.Sub(a.at)
}

// attachControl is a client→server text frame. Decoded tolerantly on purpose:
// an unknown key is a newer client talking to an older server and is ignored,
// never an error. Same rule the rest of the API follows in both directions.
type attachControl struct {
	Resize *struct {
		Cols int `json:"cols"`
		Rows int `json:"rows"`
	} `json:"resize"`
}

func (s *server) startAttachClient(target string, readonly bool, size attachSize) (attachClient, error) {
	if s.startAttach != nil {
		return s.startAttach(target, readonly, size)
	}
	return startTmuxAttach(target, readonly, size)
}

func (s *server) attachIdleTimeout() time.Duration {
	if s.attachIdle > 0 {
		return s.attachIdle
	}
	return attachIdleTimeout
}

// attachSizeFromQuery reads the optional ?cols=&rows= the client may open with,
// so the first frame the phone ever sees is already its own size instead of a
// standard 80x24 that resizes a moment later — which, with tmux sizing a window
// to its latest client, the desktop user would watch happen too.
//
// Absent is fine and common. Present but unusable is a 400: a client that
// computed a bogus geometry has a bug worth hearing about before a terminal
// opens on it.
func attachSizeFromQuery(q url.Values) (attachSize, error) {
	size := attachSize{Cols: attachDefaultCols, Rows: attachDefaultRows}
	if v := q.Get("cols"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > attachMaxCols {
			return attachSize{}, fmt.Errorf("bad cols value: %s", v)
		}
		size.Cols = uint16(n)
	}
	if v := q.Get("rows"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > attachMaxRows {
			return attachSize{}, fmt.Errorf("bad rows value: %s", v)
		}
		size.Rows = uint16(n)
	}
	return size, nil
}

// attach handles GET /sessions/{pid}/attach?session_id=...&readonly=1.
//
// Everything that can refuse, refuses before the upgrade: nothing is started
// and no socket is accepted until the PID resolves to the session the client
// named and that session has a pane.
//
// Those refusals answer with the action-contract envelope — {ok:false, error,
// code} — but not with the contract's HTTP 200. A 200 in reply to an upgrade
// request is a protocol lie, and the one client that matters here
// (URLSessionWebSocketTask) surfaces the status to its caller and the body to
// nobody. So the status carries the signal, and the body carries the precise
// code for curl, tests, and any client that can read it.
func (s *server) attach(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	pid, err := strconv.Atoi(r.PathValue("pid"))
	if err != nil {
		http.Error(w, "bad pid", http.StatusBadRequest)
		return
	}
	q := r.URL.Query()
	size, err := attachSizeFromQuery(q)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	readonly := q.Get("readonly") == "1"

	// Unlike kill/migrate, session_id is mandatory here, not "" = no
	// precondition. Those two predate the identity guard and keep the
	// unguarded mode for callers that do; attach has no such caller, and its
	// unguarded mode is a full interactive shell on whatever the PID
	// currently holds rather than a single mutation — the same reasoning
	// that made it mandatory on send-keys, sharper here because the blast
	// radius is bigger.
	sessionID := q.Get("session_id")
	if sessionID == "" {
		writeJSON(w, http.StatusBadRequest, actionResult{Error: "session_id is required"})
		return
	}

	// Same precondition as kill/migrate otherwise, and for the same reason: a
	// phone acts on a list it may have polled minutes ago, and a recycled
	// pane hands that PID to somebody else.
	target, _, refusal := s.resolveLivePID(pid, sessionID)
	if refusal != nil {
		writeJSON(w, http.StatusConflict, *refusal)
		return
	}
	// No pane, nothing to stream. Malformed tmux metadata refuses the same way
	// rather than getting its own code: from the client's side both mean "this
	// session has no terminal you can open", and there is nothing it would do
	// differently.
	if _, err := tmuxSessionName(target.Tmux); err != nil {
		writeJSON(w, http.StatusConflict, actionResult{
			Error: fmt.Sprintf("PID %d is not running inside tmux", pid),
			Code:  codeNoPane,
		})
		return
	}
	release, ok := s.attaches.acquire(target.SessionID, attachMaxPerSession, attachMaxTotal)
	if !ok {
		// One code for both ceilings, deliberately. The client's move is the
		// same either way — close a terminal it already has open, rather than
		// refresh — so telling it which counter refused would name a difference
		// it cannot act on, at the cost of a wire code that is permanent once
		// shipped.
		writeJSON(w, http.StatusTooManyRequests, actionResult{
			Error: "the maximum number of terminals are already attached",
			Code:  codeAttachBusy,
		})
		return
	}
	defer release()

	// Default options, which means the default origin check: a request carrying
	// an Origin from another host is rejected. Nothing legitimate here sends one
	// — a browser could not reach this endpoint anyway, having no way to put a
	// bearer token on a WebSocket handshake — so the strict default costs
	// nothing and closes the CSRF shape.
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{})
	if err != nil {
		// Accept has already written the failure; a non-WebSocket GET that got
		// this far (curl, a health probe) lands here.
		return
	}
	defer conn.CloseNow()

	// Started after the upgrade so a failure can be reported down the socket the
	// client is already holding, with a reason it can show, rather than as a
	// handshake error it can only guess at.
	client, err := s.startAttachClient(target.Tmux, readonly, size)
	if err != nil {
		_ = conn.Close(websocket.StatusInternalError, "attach failed")
		return
	}
	s.pumpAttach(conn, client, readonly)
}

// pumpAttach moves bytes until one side stops, then tears the other one down.
//
// Teardown order is the whole subtlety, so it is written once, at the bottom:
// cancel, close the attach client, and only then wait for the output pump. The
// pump reads the PTY and writes the socket, so returning while it is still
// running would have it reading a file we are closing and writing a connection
// we have already torn down.
func (s *server) pumpAttach(conn *websocket.Conn, client attachClient, readonly bool) {
	conn.SetReadLimit(attachReadLimit)

	// ctx bounds the goroutines started below. Rooted at Background, not at the
	// request: the handler outlives the request in every sense that matters
	// here, and hijacked-request context lifetime is a net/http detail this has
	// no reason to depend on.
	//
	// It is deliberately never the context handed to conn.Read. This library
	// answers a cancelled read context by tearing the TCP connection down where
	// it stands, so a server-side teardown would reach the phone as a dropped
	// connection rather than as a close code saying why. Teardown goes through
	// closeWith instead: it writes a real close frame, and the peer answering it
	// is what unblocks the read.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Whoever gets here first names the reason; later callers are no-ops, which
	// is also what makes the unconditional call at the end safe.
	var closeOnce sync.Once
	closeWith := func(status websocket.StatusCode, reason string) {
		closeOnce.Do(func() { _ = conn.Close(status, reason) })
	}

	var lastActivity attachActivity
	lastActivity.mark(time.Now())

	// PTY → WebSocket.
	outDone := make(chan struct{})
	go func() {
		defer close(outDone)
		buf := make([]byte, 32*1024)
		for {
			n, err := client.Read(buf)
			if n > 0 {
				wctx, wcancel := context.WithTimeout(ctx, attachWriteTimeout)
				werr := conn.Write(wctx, websocket.MessageBinary, buf[:n])
				wcancel()
				if werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	// The attach client exiting — a detach typed into tmux, or the session
	// dying — has to end the read loop below too, which is otherwise parked in
	// conn.Read until the phone says something.
	go func() {
		<-outDone
		closeWith(websocket.StatusNormalClosure, "detached")
	}()

	// Idle timeout. It counts client→server frames only, deliberately: tmux
	// redraws its status line every status-interval seconds whether anybody is
	// there or not, so an idle timer that PTY output could reset would never
	// fire on a real session. "Idle" here means nobody is touching this
	// terminal, which is the thing worth reclaiming a PTY over.
	idleTimeout := s.attachIdleTimeout()
	go func() {
		tick := time.NewTicker(idleTimeout / 4)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			// time.Now(), not the tick's own timestamp: both sides of the
			// comparison have to carry a monotonic reading for it to be one.
			case <-tick.C:
				if lastActivity.idleFor(time.Now()) >= idleTimeout {
					closeWith(websocket.StatusGoingAway, "idle timeout")
					return
				}
			}
		}
	}()

	// WebSocket → PTY. Background, not ctx: see closeWith above.
	for {
		typ, data, err := conn.Read(context.Background())
		if err != nil {
			break
		}
		lastActivity.mark(time.Now())
		switch typ {
		case websocket.MessageBinary:
			// The enforcement point. tmux's own -r is asked for politely on the
			// far side; this is what actually holds when the client is lying.
			if readonly {
				continue
			}
			if _, err := client.Write(data); err != nil {
				closeWith(websocket.StatusInternalError, "attach client gone")
			}
		case websocket.MessageText:
			// Allowed on readonly connections: a geometry is this client's own
			// window size, not input to the session.
			applyAttachControl(client, data)
		}
	}

	// The read loop is done, so the client is going away whatever the reason.
	// Order matters: stop the idle watcher, detach the attach client — which is
	// what makes the output pump's next read return — and only then wait for
	// that pump, because returning while it is alive would leave it reading a
	// PTY we are closing and writing a connection we have torn down.
	closeWith(websocket.StatusNormalClosure, "detached")
	cancel()
	_ = client.Close()
	<-outDone
}

// applyAttachControl applies one text-frame control message.
//
// A frame that is malformed, unknown, or out of range is dropped and the
// connection lives on. The alternative — tearing down a terminal somebody is
// working in because one control frame was wrong — is a worse failure than
// ignoring it.
func applyAttachControl(client attachClient, data []byte) {
	var ctrl attachControl
	if err := json.Unmarshal(data, &ctrl); err != nil {
		return
	}
	if ctrl.Resize == nil {
		return
	}
	cols, rows := ctrl.Resize.Cols, ctrl.Resize.Rows
	if cols < 1 || cols > attachMaxCols || rows < 1 || rows > attachMaxRows {
		return
	}
	_ = client.Resize(attachSize{Cols: uint16(cols), Rows: uint16(rows)})
}
