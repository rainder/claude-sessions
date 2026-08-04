package main

// Integration tests for GET /sessions/{pid}/attach against a REAL tmux server.
//
// Everything else about attach is tested through the startAttach seam, which is
// enough to pin the pump's own rules. It is not enough for the two claims this
// feature actually rests on — that a phone hanging up detaches without touching
// the session, and that a readonly connection cannot type into it — because
// both are claims about tmux's behaviour, not about our code's.
//
// Isolation, in full: every test here runs its own tmux server on a private
// socket under its own TMUX_TMPDIR, creates only sessions it named itself, and
// kills that server on the way out. $TMUX is unset for the duration so neither
// these commands nor the attach client the handler spawns can reach the
// developer's own tmux. Nothing here can touch a real Claude session.
//
// Skipped by `go test -short`, and skipped where tmux is not installed.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// tmuxFixture is a private tmux server with one session in it, plus an attach
// endpoint pointed at that session through the production code path — no
// startAttach seam, so `tmux attach-session` really runs on a real PTY.
type tmuxFixture struct {
	t       *testing.T
	name    string // tmux session name
	target  string // the Tmux location the session row reports, "name:0.0" by default
	srv     *httptest.Server
	handler *server // the server behind srv, so a test can shorten its timers
	pid     int     // pane pid, standing in for the Claude process
}

func newTmuxFixture(t *testing.T) *tmuxFixture {
	t.Helper()
	if testing.Short() {
		t.Skip("-short: skipping the real-tmux integration test")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	// A private socket directory, and a short path on purpose: a unix socket
	// path is capped near 104 bytes and tmux appends "/tmux-<uid>/default".
	dir, err := os.MkdirTemp("", "cs-tmux")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMUX_TMPDIR", dir)
	// `go test` may itself be running inside tmux. $TMUX would then point both
	// these commands and the attach client at that server instead of this one —
	// and the attach client would refuse outright ("sessions should be nested
	// with care"). attachEnv drops it for the child; this drops it for us.
	if v, ok := os.LookupEnv("TMUX"); ok {
		os.Unsetenv("TMUX")
		t.Cleanup(func() { os.Setenv("TMUX", v) })
	}

	f := &tmuxFixture{t: t, name: "cs-attach-test"}
	f.target = f.name + ":0.0"
	t.Cleanup(func() {
		_ = exec.Command("tmux", "kill-server").Run()
		_ = os.RemoveAll(dir)
	})

	// An interactive shell: something that echoes what is typed at it, which is
	// how injected input becomes visible in the pane.
	f.mustTmux("new-session", "-d", "-s", f.name, "-x", "80", "-y", "24", "/bin/sh")
	f.pid, err = strconv.Atoi(f.mustTmux("list-panes", "-t", f.name, "-F", "#{pane_pid}"))
	if err != nil {
		t.Fatalf("pane pid: %v", err)
	}

	f.handler = &server{
		token: "secret",
		collect: func() ([]Session, error) {
			return []Session{{PID: f.pid, SessionID: "sess-live", Tmux: f.target}}, nil
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /sessions/{pid}/attach", f.handler.attach)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *tmuxFixture) tmux(args ...string) (string, error) {
	out, err := exec.Command("tmux", args...).CombinedOutput()
	return strings.TrimRight(string(out), "\n"), err
}

func (f *tmuxFixture) mustTmux(args ...string) string {
	f.t.Helper()
	out, err := f.tmux(args...)
	if err != nil {
		f.t.Fatalf("tmux %s: %v (%s)", strings.Join(args, " "), err, out)
	}
	return out
}

func (f *tmuxFixture) capture() string {
	f.t.Helper()
	return f.mustTmux("capture-pane", "-p", "-t", f.name)
}

func (f *tmuxFixture) clientCount() int { return f.clientCountOf(f.name) }

// clientCountOf counts the clients attached to one named session, and reports
// zero for a session that does not exist.
//
// Read off list-sessions and matched here, in Go, rather than asking tmux for
// one session by name: the tests below are about tmux's target matching, so a
// measurement that goes through that same matching could agree with a wrong
// answer. Nothing is passed to tmux as a target at all.
func (f *tmuxFixture) clientCountOf(name string) int {
	out, err := f.tmux("list-sessions", "-F", "#{session_name} #{session_attached}")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(out, "\n") {
		got, attached, ok := strings.Cut(line, " ")
		if !ok || got != name {
			continue
		}
		n, err := strconv.Atoi(attached)
		if err != nil {
			return 0
		}
		return n
	}
	return 0
}

func (f *tmuxFixture) sessionAlive() bool {
	_, err := f.tmux("has-session", "-t", f.name)
	return err == nil
}

// dial opens an attach WebSocket against the fixture's endpoint.
func (f *tmuxFixture) dial(query string) *websocket.Conn {
	f.t.Helper()
	conn := f.dialRaw(query)
	// tmux redraws the whole pane the moment a client attaches, so the first
	// frame both proves the socket carries real terminal bytes and marks the
	// point where the attach is fully established.
	readFrame(f.t, conn)
	return conn
}

// dialRaw opens the socket and reads nothing. For the attaches that are meant
// to fail: an attach client that exits immediately may close the connection
// before, or instead of, sending anything, and dial's "wait for the first
// frame" would turn that correct outcome into a test failure.
func (f *tmuxFixture) dialRaw(query string) *websocket.Conn {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	u := fmt.Sprintf("%s/sessions/%d/attach?session_id=sess-live&%s",
		strings.Replace(f.srv.URL, "http://", "ws://", 1), f.pid, query)
	conn, _, err := websocket.Dial(ctx, u, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": []string{"Bearer secret"}},
	})
	if err != nil {
		f.t.Fatalf("dial %s: %v", query, err)
	}
	f.t.Cleanup(func() { conn.CloseNow() })
	return conn
}

// TestAttachRealTmuxReadonlyCannotInjectInput is the security claim, proven
// rather than asserted.
//
// The order is what makes it a proof. The readonly connection is live (it has
// already delivered a frame) when its input is sent. A second, read-write
// connection then sends the same kind of input and the test waits until *that*
// marker appears in the pane — so the readonly bytes have had every opportunity
// a delivered byte would have had, through the same server, the same handler,
// and the same PTY plumbing, differing only in the readonly flag. Then the
// pane is checked for the readonly marker.
//
// Without the read-write half this test would pass just as happily against a
// broken harness that never typed anything anywhere.
func TestAttachRealTmuxReadonlyCannotInjectInput(t *testing.T) {
	f := newTmuxFixture(t)

	ro := f.dial("readonly=1")
	writeFrame(t, ro, websocket.MessageBinary, []byte("echo RO_MARKER\r"))

	rw := f.dial("")
	writeFrame(t, rw, websocket.MessageBinary, []byte("echo RW_MARKER\r"))

	waitUntil(t, "the read-write marker to reach the pane", func() bool {
		return strings.Contains(f.capture(), "RW_MARKER")
	})
	if pane := f.capture(); strings.Contains(pane, "RO_MARKER") {
		t.Fatalf("readonly input reached the session — pane:\n%s", pane)
	}
}

// TestAttachRealTmuxReadonlyHoldsWithoutTmuxsOwnReadOnlyFlag is the same proof
// with tmux's half of the defence deliberately taken away.
//
// The test above passes even with the server's input drop removed, because
// `tmux attach -r` refuses the keystrokes on its own — which is exactly why it
// cannot be the whole proof. Here the attach client is started read-write while
// the connection is readonly: a tmux client that would happily type anything it
// is given, standing in for the day -r is dropped by accident, renamed, or
// found not to cover some path. The server's own drop is then the only thing
// between a readonly WebSocket and a live shell, and this test fails if it goes.
func TestAttachRealTmuxReadonlyHoldsWithoutTmuxsOwnReadOnlyFlag(t *testing.T) {
	f := newTmuxFixture(t)
	// Production attach, real PTY, real tmux — with the readonly flag dropped
	// on the way to tmux and nowhere else. Set before anything dials.
	f.handler.startAttach = func(target string, readonly bool, size attachSize) (attachClient, error) {
		return startTmuxAttach(target, false, size)
	}

	ro := f.dial("readonly=1")
	writeFrame(t, ro, websocket.MessageBinary, []byte("echo RO_MARKER\r"))

	rw := f.dial("")
	writeFrame(t, rw, websocket.MessageBinary, []byte("echo RW_MARKER\r"))

	waitUntil(t, "the read-write marker to reach the pane", func() bool {
		return strings.Contains(f.capture(), "RW_MARKER")
	})
	if pane := f.capture(); strings.Contains(pane, "RO_MARKER") {
		t.Fatalf("readonly input reached the session once tmux stopped refusing it — pane:\n%s", pane)
	}
}

// TestAttachRealTmuxResizeResizesTheWindow proves the resize control frame
// reaches tmux, not merely the PTY: the ioctl on the master is what tmux reads
// as its client's size, and the window follows.
func TestAttachRealTmuxResizeResizesTheWindow(t *testing.T) {
	f := newTmuxFixture(t)
	conn := f.dial("")
	if got := f.mustTmux("display-message", "-p", "-t", f.name, "#{window_width}"); got != "80" {
		t.Fatalf("window_width = %s, want the 80 the session was created at", got)
	}

	writeFrame(t, conn, websocket.MessageText, []byte(`{"resize":{"cols":120,"rows":40}}`))
	// The client size is the resize verbatim: that is the PTY geometry tmux read
	// back off the master. The window is one row shorter because tmux spends it
	// on its own status line — which is why the client size, not the window
	// size, is what this asserts exactly.
	waitUntil(t, "tmux to follow the resize", func() bool {
		return f.mustTmux("list-clients", "-t", f.name, "-F", "#{client_width}x#{client_height}") == "120x40"
	})
	if got := f.mustTmux("display-message", "-p", "-t", f.name, "#{window_width}"); got != "120" {
		t.Fatalf("window_width = %s, want the resized 120", got)
	}

	// Not an assertion: tmux's window-size rule is a configuration, and what a
	// second, differently-sized client does to the first one's view is worth
	// knowing for the iOS side rather than pinning here.
	second := f.dial("cols=80&rows=24")
	defer second.CloseNow()
	time.Sleep(200 * time.Millisecond)
	t.Logf("with a second 80x24 client attached, window is %sx%s (tmux %s)",
		f.mustTmux("display-message", "-p", "-t", f.name, "#{window_width}"),
		f.mustTmux("display-message", "-p", "-t", f.name, "#{window_height}"),
		f.mustTmux("display-message", "-p", "#{version}"))
}

// TestAttachRealTmuxDisconnectLeavesTheSessionAlive is the whole point of
// attaching to tmux rather than to the process: the phone hanging up detaches
// one client and nothing else. The session, its pane process, and its scrollback
// all outlive the connection.
func TestAttachRealTmuxDisconnectLeavesTheSessionAlive(t *testing.T) {
	f := newTmuxFixture(t)
	conn := f.dial("")
	writeFrame(t, conn, websocket.MessageBinary, []byte("echo STILL_HERE\r"))
	waitUntil(t, "the typed marker to reach the pane", func() bool {
		return strings.Contains(f.capture(), "STILL_HERE")
	})
	if f.clientCount() != 1 {
		t.Fatalf("clients attached = %d, want the one we opened", f.clientCount())
	}

	if err := conn.Close(websocket.StatusNormalClosure, "leaving"); err != nil {
		t.Fatal(err)
	}

	// The attach client itself must go: that is the process the disconnect is
	// allowed to kill.
	waitUntil(t, "the attach client to detach", func() bool { return f.clientCount() == 0 })

	// And nothing else may have gone with it.
	if !f.sessionAlive() {
		t.Fatal("the tmux session died with the WebSocket")
	}
	if err := syscall.Kill(f.pid, 0); err != nil {
		t.Fatalf("pane process %d is gone: %v", f.pid, err)
	}
	if got := f.mustTmux("list-panes", "-t", f.name, "-F", "#{pane_pid}"); got != strconv.Itoa(f.pid) {
		t.Fatalf("pane pid = %s, want the unchanged %d", got, f.pid)
	}
	if pane := f.capture(); !strings.Contains(pane, "STILL_HERE") {
		t.Fatalf("scrollback lost the detach — pane:\n%s", pane)
	}
}

// TestAttachRealTmuxAttachesToTheExactSessionNotAPrefixSuperset: with two live
// sessions whose names share a prefix, the attach lands on the one the session
// row actually named.
//
// tmux resolves a target session by exact match, then by prefix, then as an
// fnmatch pattern. This case is the benign one — the exact match wins on its
// own — and it is here as the control for the test below it, which removes the
// exact match and leaves only the prefix.
func TestAttachRealTmuxAttachesToTheExactSessionNotAPrefixSuperset(t *testing.T) {
	f := newTmuxFixture(t)
	superset := f.name + "-superset"
	f.mustTmux("new-session", "-d", "-s", superset, "-x", "80", "-y", "24", "/bin/sh")

	f.dial("")
	if got := f.clientCountOf(f.name); got != 1 {
		t.Fatalf("clients on %s = %d, want the one we opened", f.name, got)
	}
	if got := f.clientCountOf(superset); got != 0 {
		t.Fatalf("clients on %s = %d, want none — the attach went to the wrong session", superset, got)
	}
}

// TestAttachRealTmuxRefusesAStaleNameThatOnlyPrefixMatches is the security
// claim, and the reason attach targets "=name" rather than "name".
//
// A session row's Tmux field is not re-verified on every collection: CollectLocal
// overwrites it only when a live pane is found for the PID (session.go), so a
// value read straight off the on-disk session JSON can name a tmux session that
// no longer exists. Handed to tmux loosely, such a name does not fail — it falls
// through to prefix matching and attaches to whatever live session happens to
// start with it, which is a full interactive terminal on somebody else's work.
//
// So: one live session named as a superset of the stale name, no session by the
// stale name itself, and the attach must reach nothing at all.
func TestAttachRealTmuxRefusesAStaleNameThatOnlyPrefixMatches(t *testing.T) {
	f := newTmuxFixture(t)
	// Live, and a strict superset of the stale name below. Nothing is ever
	// created under "cs-stale" itself, which is exactly the state a session
	// file left behind by a renamed or recreated tmux session describes.
	const stale = "cs-stale"
	const superset = stale + "-somebody-elses-work"
	f.mustTmux("new-session", "-d", "-s", superset, "-x", "80", "-y", "24", "/bin/sh")
	f.target = stale + ":0.0"

	conn := f.dialRaw("")
	// Drained in the background with an uncancellable context, on purpose. A
	// read that times out makes this library tear the TCP connection down, and
	// that teardown detaches the very client this test has to catch — measured
	// after it, a wrong attach leaves no trace and the test passes vacuously.
	closed := make(chan error, 1)
	go func() {
		var err error
		for err == nil {
			_, _, err = conn.Read(context.Background())
		}
		closed <- err
	}()

	// So the wrong session is watched while the socket is still live. Whichever
	// happens first decides the test: a client landing where nobody asked for
	// one, or the attach ending because tmux could not resolve the stale name.
	deadline := time.After(10 * time.Second)
	for {
		if got := f.clientCountOf(superset); got != 0 {
			t.Fatalf("the stale target prefix-matched onto %s (%d client(s)) — an attach reached a session nobody asked for", superset, got)
		}
		select {
		case err := <-closed:
			if got := f.clientCountOf(superset); got != 0 {
				t.Fatalf("the stale target prefix-matched onto %s (%d client(s)) — an attach reached a session nobody asked for", superset, got)
			}
			if websocket.CloseStatus(err) != websocket.StatusNormalClosure {
				t.Fatalf("close status = %v (err %v), want a clean normal closure: an unresolvable target must end the connection, not hang it", websocket.CloseStatus(err), err)
			}
			if !f.sessionAlive() {
				t.Fatal("the refused attach took the real session with it")
			}
			return
		case <-deadline:
			t.Fatal("the attach to a stale name neither ended nor landed anywhere — it is still holding a socket")
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// TestAttachRealTmuxIdleTimeoutDetachesButKeepsTheSession: the reaper is held to
// the same rule as a disconnect. It closes the socket and the attach client;
// the session is not its business.
func TestAttachRealTmuxIdleTimeoutDetachesButKeepsTheSession(t *testing.T) {
	f := newTmuxFixture(t)
	// Set before anything dials, so no handler is reading it yet.
	f.handler.attachIdle = 300 * time.Millisecond

	conn := f.dial("")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var err error
	for err == nil {
		_, _, err = conn.Read(ctx)
	}
	if websocket.CloseStatus(err) != websocket.StatusGoingAway {
		t.Fatalf("close status = %v (err %v), want going-away", websocket.CloseStatus(err), err)
	}
	waitUntil(t, "the idled-out attach client to detach", func() bool { return f.clientCount() == 0 })
	if !f.sessionAlive() {
		t.Fatal("the idle timeout killed the tmux session")
	}
}
